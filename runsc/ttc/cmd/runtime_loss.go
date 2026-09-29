// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

// consoleFollowerExitWait bounds how long finding a runtime gone waits for its
// branches' console followers to finish. A follower whose Sentry is gone fails
// its next call to it at once; one whose branch exited just before the Sentry
// did may still be recording the branch's exit status.
const consoleFollowerExitWait = 2 * time.Second

// recordLostRuntime records this node's branches of a family whose runtime is
// gone as process-free: each that needed the runtime as exited, the way it
// ended.
func (b *runtimeBackend) recordLostRuntime(container ttcbundle.ContainerInfo) error {
	// How each branch ended, found before deleting the runtime takes its
	// cgroup.
	loss := b.observeRuntimeLoss(container)
	// Once the Sentry is known to be gone, no console follower or gofer may
	// retain files from the persistent container directory. Reap the sidecars
	// before recording process-free state so later removal is also safe on
	// NFS, where unlinking an open file creates .nfs files.
	_ = b.stopContainerConsoleDrains(container.ID)
	_, _ = b.command("delete", "--force", container.ID)
	if err := b.containerStore().ReconcileNodeRuntimeUnavailable(container.ID, b.nodeID, loss); err != nil {
		return err
	}
	b.releaseLostBranchImages(container)
	return nil
}

// releaseLostBranchImages releases the images the family's live branches were
// rehydrated from, which each kept for its next dehydration: that dehydration
// will not come, as the branch lost its memory with its runtime.
func (b *runtimeBackend) releaseLostBranchImages(container ttcbundle.ContainerInfo) {
	if b.nodeID != "" {
		return
	}
	store := b.containerStore()
	for _, branch := range container.Branches {
		switch branch.State {
		case "running", "paused":
		default:
			continue
		}
		if len(branch.MemoryRoots) == 0 {
			continue
		}
		cleared, err := store.ClearBranchMemoryGraphIf(container.ID, branch.ID, branch.SnapshotID, branch.MemoryRoots, branch.MemoryRootObjects, branch.DistributedGraphReferenceOperation)
		if err == nil && cleared {
			err = b.releaseBranchGraphRoots(branch)
		}
		if err != nil && b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: releasing the image of lost branch %s@%s: %v\n", container.Name, branch.ID, err)
		}
	}
}

// lostRuntimeWaitStatus is the wait status of a branch whose runtime died
// while it was waited for: that of its init, if it exited first, and otherwise
// that of a process killed with SIGKILL, as `docker wait` reports a container
// killed outright. It records the lost runtime, as listing it would.
func (b *runtimeBackend) lostRuntimeWaitStatus(target selector) (uint32, bool) {
	unlock := func() {}
	if !b.lifecycleLockHeld {
		unlock = b.holdFamilyLifecycle(b.familyID(target.container))
	}
	defer unlock()
	if _, busy := b.operations.lookup(target.container); busy {
		return 0, false
	}
	info, err := b.containerStore().LoadContainer(target.container)
	if err != nil {
		return 0, false
	}
	if persistentFamilyRequiredRuntime(info, b.nodeID) {
		// Gone as inventory judges it: runsc's record of a runtime can outlive
		// its Sentry.
		runtimes, err := b.listRuntimeFamiliesOf(info.ID)
		if err != nil {
			return 0, false
		}
		for _, runtime := range runtimes {
			if runtime.ID == info.ID && !runtimeFamilyUnavailable(runtime) {
				return 0, false
			}
		}
		if err := b.recordLostRuntime(info); err != nil {
			return 0, false
		}
		if info, err = b.containerStore().LoadContainer(target.container); err != nil {
			return 0, false
		}
	}
	branch, err := persistentBranchInfo(info, target.branch)
	if err != nil || normalizeLifecycleState(branch.State) != "exited" || branch.ExitStatus == nil {
		return 0, false
	}
	return uint32(*branch.ExitStatus) << 8, true
}

// observeRuntimeLoss finds out what became of this node's branches of a family
// whose runtime is gone. A branch whose init exited first has the wait status
// its console follower recorded, the durable proof of a normal exit; any other
// was killed with the runtime. It must run before the runtime is deleted,
// which removes the runtime's cgroup and the count of out-of-memory kills in
// it.
func (b *runtimeBackend) observeRuntimeLoss(container ttcbundle.ContainerInfo) ttcbundle.RuntimeLoss {
	loss := ttcbundle.RuntimeLoss{ExitStatuses: make(map[string]int)}
	store := b.containerStore()
	for _, branch := range container.Branches {
		if b.nodeID != "" && container.BranchNodeID(branch.ID) != b.nodeID {
			continue
		}
		if !persistentBranchRequiredRuntime(container, branch.ID) && !ttcbundle.ResidentDehydration(branch) {
			continue
		}
		paths, err := store.BranchConsolePaths(container.ID, branch.ID)
		if err != nil {
			continue
		}
		waitForConsoleFollowerExit(paths.Lock, consoleFollowerExitWait)
		if status, found, err := readConsoleWaitStatus(paths.WaitStatus); err == nil && found {
			loss.ExitStatuses[branch.ID] = waitStatusExitCode(status)
		}
	}
	loss.OOMKilled = runtimeCgroupOOMKilled(container.BundleDir)
	return loss
}

// waitForConsoleFollowerExit waits, for up to timeout, for the console
// follower that holds lock to exit.
func waitForConsoleFollowerExit(lock string, timeout time.Duration) {
	file, err := os.OpenFile(lock, os.O_RDWR, 0)
	if err != nil {
		return
	}
	defer file.Close()
	if waitForPortLock(file, timeout) == nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	}
}

// runtimeCgroupOOMKilled reports whether the out-of-memory killer killed a
// process in the cgroup of the runtime whose bundle is bundleDir: its Sentry,
// which holds all of the family's memory. Only a family with hard resource
// limits has a cgroup of its own; a host that runs out of memory kills without
// saying for whom.
func runtimeCgroupOOMKilled(bundleDir string) bool {
	data, err := os.ReadFile(filepath.Join(bundleDir, "config.json"))
	if err != nil {
		return false
	}
	var spec specs.Spec
	if json.Unmarshal(data, &spec) != nil || spec.Linux == nil || spec.Linux.CgroupsPath == "" {
		return false
	}
	dir := filepath.Join("/sys/fs/cgroup", spec.Linux.CgroupsPath)
	if !filepath.IsAbs(spec.Linux.CgroupsPath) {
		parent, err := delegatedCgroupParent("/proc/self/cgroup", "/sys/fs/cgroup")
		if err != nil {
			return false
		}
		dir = filepath.Join(parent, spec.Linux.CgroupsPath)
	}
	events, err := os.ReadFile(filepath.Join(dir, "memory.events"))
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(events), "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == "oom_kill" {
			count, err := strconv.ParseUint(fields[1], 10, 64)
			return err == nil && count > 0
		}
	}
	return false
}
