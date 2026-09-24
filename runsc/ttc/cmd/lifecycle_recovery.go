// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
)

// reconcileInterruptedStandaloneLifecycles repairs the two process-free
// dehydration commit windows that can survive an abrupt ttcd exit:
//
//  1. The Sentry captured the branch, but ttcd did not finish importing and
//     recording its graph roots. Rehydrate the still-owned capture and roll
//     persistent state back to the exact running/paused state remembered by
//     the Sentry.
//  2. The durable graph capture was recorded, but ttcd did not remove the now
//     process-free runtime shell. Commit the durable lifecycle and delete the
//     shell.
//
// It returns false without changing an actively transitioning Sentry so the
// daemon retries reconciliation on a later request. Clustered dehydration has
// additional Raft/publication fences and is reconciled by its data plane.
func (b *runtimeBackend) reconcileInterruptedStandaloneLifecycles() (bool, error) {
	if b == nil || b.dataRoot == "" || b.distributedSource != nil {
		return true, nil
	}
	store := b.containerStore()
	containers, err := store.ListContainersForGC()
	if err != nil {
		return false, err
	}
	if len(containers) == 0 {
		return true, nil
	}
	runtimes, err := b.listRuntimeFamilies()
	if err != nil {
		return false, err
	}
	runtimeByID := make(map[string]Family, len(runtimes))
	for _, family := range runtimes {
		runtimeByID[family.ID] = family
	}
	complete := true
	for _, container := range containers {
		// A long operation's dehydration is in progress, not interrupted.
		if _, busy := b.operations.lookup(container.ID); busy {
			complete = false
			continue
		}
		runtime, runtimePresent := runtimeByID[container.ID]
		for _, branch := range container.Branches {
			if normalizeLifecycleState(branch.State) != "dehydrating" {
				continue
			}
			if !runtimePresent || runtimeFamilyUnavailable(runtime) {
				// No live in-Sentry capture remains. A fully recorded standalone
				// graph image is independently recoverable; otherwise the abrupt
				// runtime loss destroyed the only authoritative process state.
				state := "exited"
				if b.standaloneDehydrationCaptureComplete(container.ID, branch) {
					state = "dehydrated"
				} else if err := b.removeInterruptedDehydrationImages(container.ID, branch); err != nil {
					return false, fmt.Errorf("removing incomplete dehydration for %s@%s: %w", container.ID, branch.ID, err)
				}
				if err := store.UpdateBranchState(container.ID, branch.ID, state); err != nil {
					return false, fmt.Errorf("reconciling lost dehydration runtime for %s@%s: %w", container.ID, branch.ID, err)
				}
				continue
			}
			runtimeState := ""
			for _, current := range runtime.Branches {
				if current.ID == branch.ID {
					runtimeState = normalizeLifecycleState(current.State)
					break
				}
			}
			switch runtimeState {
			case "dehydrating", "rehydrating", "creating", "restarting":
				complete = false
				continue
			case "dehydrated":
				if b.standaloneDehydrationCaptureComplete(container.ID, branch) {
					if err := store.UpdateBranchState(container.ID, branch.ID, "dehydrated"); err != nil {
						return false, fmt.Errorf("committing recovered dehydration for %s@%s: %w", container.ID, branch.ID, err)
					}
					continue
				}
				if _, err := b.command("timetravel", container.ID, "rehydrate", branch.ID); err != nil {
					return false, fmt.Errorf("rolling back interrupted dehydration for %s@%s: %w", container.ID, branch.ID, err)
				}
				state, err := b.runtimeBranchState(selector{container: container.ID, branch: branch.ID})
				if err != nil {
					return false, fmt.Errorf("reading rolled-back dehydration state for %s@%s: %w", container.ID, branch.ID, err)
				}
				runtimeState = state
			case "running", "paused":
				// The Sentry already rolled back a failed capture before ttcd
				// exited; only its persistent transition marker remains.
			case "":
				complete = false
				continue
			default:
				return false, fmt.Errorf("interrupted dehydration for %s@%s has runtime state %q", container.ID, branch.ID, runtimeState)
			}
			if runtimeState != "running" && runtimeState != "paused" {
				return false, fmt.Errorf("rolled-back dehydration for %s@%s reached state %q", container.ID, branch.ID, runtimeState)
			}
			if err := b.removeInterruptedDehydrationImages(container.ID, branch); err != nil {
				return false, fmt.Errorf("removing interrupted dehydration image for %s@%s: %w", container.ID, branch.ID, err)
			}
			if err := store.UpdateBranchState(container.ID, branch.ID, runtimeState); err != nil {
				return false, fmt.Errorf("rolling back dehydration state for %s@%s: %w", container.ID, branch.ID, err)
			}
		}
	}

	// A crash after the durable lifecycle commit but before runsc delete leaves
	// a stopped shell that may still retain the complete in-Sentry capture. The
	// persistent record is authoritative at this point, so release every such
	// shell; this is idempotent across repeated daemon restarts.
	containers, err = store.ListContainersForGC()
	if err != nil {
		return false, err
	}
	for _, container := range containers {
		if _, busy := b.operations.lookup(container.ID); busy {
			complete = false
			continue
		}
		if err := b.deleteRuntimeIfProcessFree(container); err != nil {
			return false, err
		}
	}
	return complete, nil
}

func (b *runtimeBackend) runtimeBranchState(target selector) (string, error) {
	output, err := b.command("timetravel", target.container, "branches")
	if err != nil {
		return "", err
	}
	var branches []runscBranchInfo
	if err := json.Unmarshal(output, &branches); err != nil {
		return "", fmt.Errorf("decoding runtime branches: %w", err)
	}
	for _, branch := range branches {
		if branch.ID == target.branch {
			return normalizeLifecycleState(branch.State), nil
		}
	}
	return "", fmt.Errorf("branch %q does not exist", target.branch)
}

func (b *runtimeBackend) standaloneDehydrationCaptureComplete(containerID string, branch ttcbundle.BranchInfo) bool {
	if branch.SnapshotID == 0 || len(branch.MemoryRoots) == 0 || len(branch.MemoryRootObjects) == 0 ||
		branch.ControlRoot == "" || branch.ControlRootObject == "" {
		return false
	}
	dir := existingMemoryBranchDir(b.activeDataRoot(), containerID, "dehydrated", branch)
	for _, name := range []string{"control.json", "metadata.state"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}

func (b *runtimeBackend) removeInterruptedDehydrationImages(containerID string, branch ttcbundle.BranchInfo) error {
	root := filepath.Join(b.activeDataRoot(), "containers", containerID, "memory", "dehydrated")
	key := ttcbundle.MemoryBranchKey(branch)
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != key && !strings.HasPrefix(entry.Name(), "."+key+"-") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	err = dir.Sync()
	err = errors.Join(err, dir.Close())
	return err
}
