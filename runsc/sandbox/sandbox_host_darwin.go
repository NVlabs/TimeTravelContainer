// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/runsc/specutils"
)

// sentryStackLimit is the stack size of the Sentry's host threads. cgo gives
// each new thread the stack size of the thread that creates it, all descended
// from the main thread's RLIMIT_STACK, 8 MiB by default. The HVF platform
// commits whatever part of a thread's stack it mirrors into the guest, so an
// 8 MiB stack cost 8 MiB. Go on Darwin without cgo uses 512 KiB.
const sentryStackLimit = 1 << 20

// stackLimitMu serializes the process-wide RLIMIT_STACK change around a
// Sentry start.
var stackLimitMu sync.Mutex

// startSandboxProcess starts the Sentry with its stack limit lowered to
// sentryStackLimit, which its main thread's stack, and so every thread's,
// takes at exec. The limit is restored once the process has started.
func startSandboxProcess(cmd *exec.Cmd, nss []specs.LinuxNamespace) error {
	stackLimitMu.Lock()
	defer stackLimitMu.Unlock()
	var original unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_STACK, &original); err != nil {
		log.Warningf("Reading RLIMIT_STACK before starting the Sentry: %v", err)
		return specutils.StartInNS(cmd, nss)
	}
	if original.Cur <= sentryStackLimit {
		return specutils.StartInNS(cmd, nss)
	}
	lowered := original
	lowered.Cur = sentryStackLimit
	if err := unix.Setrlimit(unix.RLIMIT_STACK, &lowered); err != nil {
		log.Warningf("Lowering RLIMIT_STACK before starting the Sentry: %v", err)
		return specutils.StartInNS(cmd, nss)
	}
	defer func() {
		if err := unix.Setrlimit(unix.RLIMIT_STACK, &original); err != nil {
			log.Warningf("Restoring RLIMIT_STACK after starting the Sentry: %v", err)
		}
	}()
	return specutils.StartInNS(cmd, nss)
}

func directIOFlag() int { return 0 }

// pathOnlyFlag opens a descriptor to exec its file, or to open files beneath
// it: macOS has no O_PATH, so it reads.
const pathOnlyFlag = unix.O_RDONLY

// setCloneIntoCgroup does nothing: macOS has no cgroups to start a process in.
func setCloneIntoCgroup(*unix.SysProcAttr, *os.File) {}

// pidfdOpen reports that macOS has no process descriptors; it has no ring of
// pinned files for runsc-fd-parking to hold either.
func pidfdOpen(int) (int, error) { return -1, unix.EOPNOTSUPP }

// openReadOnlyTree reports that macOS has no detached mounts to make
// read-only: no open_tree(2).
func openReadOnlyTree(string) (int, error) {
	return -1, unix.ENOSYS
}

// openRestoreDir returns an FD for the checkpoint directory dir, through which
// the files handed to the sandbox for a restore are opened. macOS has no
// detached read-only mount to protect them with; the sandbox runs as the
// daemon's user, whose files they are.
func openRestoreDir(dir string) (int, error) {
	return unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

// openCheckpointFile opens the checkpoint file name in dir, relative to dirFD,
// an FD for dir, through no symbolic link and no "..", so that the path can
// not lead out of dir: macOS has no openat2(2)'s RESOLVE_BENEATH.
func openCheckpointFile(dirFD int, dir, name string, flags int) (*os.File, error) {
	if slices.Contains(strings.Split(name, "/"), "..") {
		return nil, unix.EXDEV
	}
	fd, err := unix.Openat(dirFD, name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW_ANY, 0644)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir, name)), nil
}

func hostFDDirectory() string { return "/dev/fd" }

func hostSocketPair() ([2]int, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return fds, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	return fds, nil
}

func usableControlSocketPath(path string) (string, func(), error) {
	// sockaddr_un.sun_path is 104 bytes on Darwin, including the terminator.
	const darwinUnixPathMax = 104
	if len(path) >= darwinUnixPathMax {
		return "", nil, fmt.Errorf("control socket path is too long for macOS (%d >= %d): %q", len(path), darwinUnixPathMax, path)
	}
	return path, func() {}, nil
}

func unsupportedHostIsolation() error {
	return fmt.Errorf("Linux host credential and namespace setup is unavailable on native macOS: %w", unix.EOPNOTSUPP)
}

func disableSetgroupsForIDMapping(*exec.Cmd) error                { return unsupportedHostIsolation() }
func setCommandCredential(*exec.Cmd, uint32, uint32) error        { return unsupportedHostIsolation() }
func configureSandboxIDMapping(*exec.Cmd, uint32, int, int) error { return unsupportedHostIsolation() }
func addChrootAmbientCapabilities(*exec.Cmd, bool) error          { return unsupportedHostIsolation() }
func configureRootlessCommandCapabilities(*exec.Cmd) error        { return unsupportedHostIsolation() }
func setCommandParentDeathSignal(*exec.Cmd)                       {}
