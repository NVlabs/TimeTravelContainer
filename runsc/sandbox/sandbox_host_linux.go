// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/moby/sys/capability"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/runsc/specutils"
)

// startSandboxProcess starts the Sentry in its namespaces.
func startSandboxProcess(cmd *exec.Cmd, nss []specs.LinuxNamespace) error {
	return specutils.StartInNS(cmd, nss)
}

func directIOFlag() int { return unix.O_DIRECT }

// pathOnlyFlag opens a descriptor that only names its file: to exec it, or to
// open files beneath it.
const pathOnlyFlag = unix.O_PATH

// setCloneIntoCgroup has the process that attr starts begin in the cgroup
// that fd opens, if any.
func setCloneIntoCgroup(attr *unix.SysProcAttr, fd *os.File) {
	if fd != nil {
		attr.UseCgroupFD = true
		attr.CgroupFD = int(fd.Fd())
	}
}

func pidfdOpen(pid int) (int, error) { return unix.PidfdOpen(pid, 0) }

func hostFDDirectory() string { return "/proc/self/fd" }

func hostSocketPair() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}

func usableControlSocketPath(path string) (string, func(), error) {
	if len(path) < linux.UnixPathMax {
		return path, func() {}, nil
	}
	fd, err := unix.Open(path, unix.O_PATH, 0)
	if err != nil {
		return "", nil, fmt.Errorf("opening long control socket path %q: %w", path, err)
	}
	return filepath.Join("/proc/self/fd", strconv.Itoa(fd)), func() { unix.Close(fd) }, nil
}

func disableSetgroupsForIDMapping(cmd *exec.Cmd) error {
	cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	return nil
}

func setCommandCredential(cmd *exec.Cmd, uid, gid uint32) error {
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid}
	return nil
}

func configureSandboxIDMapping(cmd *exec.Cmd, sandboxID uint32, hostUID, hostGID int) error {
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: sandboxID, Gid: sandboxID}
	cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: int(sandboxID), HostID: hostUID, Size: 1}}
	cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: int(sandboxID), HostID: hostGID, Size: 1}}
	return nil
}

func addChrootAmbientCapabilities(cmd *exec.Cmd, ptrace bool) error {
	cmd.SysProcAttr.AmbientCaps = append(cmd.SysProcAttr.AmbientCaps,
		uintptr(capability.CAP_SYS_ADMIN),
		uintptr(capability.CAP_SYS_CHROOT),
		uintptr(capability.CAP_SETPCAP),
	)
	if ptrace {
		cmd.SysProcAttr.AmbientCaps = append(cmd.SysProcAttr.AmbientCaps, uintptr(capability.CAP_SYS_PTRACE))
	}
	return nil
}

func setCommandParentDeathSignal(cmd *exec.Cmd) { cmd.SysProcAttr.Pdeathsig = unix.SIGKILL }

func configureRootlessCommandCapabilities(cmd *exec.Cmd) error {
	cmd.SysProcAttr.AmbientCaps = []uintptr{
		unix.CAP_CHOWN,
		unix.CAP_DAC_OVERRIDE,
		unix.CAP_DAC_READ_SEARCH,
		unix.CAP_FOWNER,
		unix.CAP_FSETID,
		unix.CAP_SYS_CHROOT,
		unix.CAP_SETUID,
		unix.CAP_SETGID,
		unix.CAP_SYS_ADMIN,
		unix.CAP_SETPCAP,
	}
	return nil
}

// openReadOnlyTree returns an FD for a detached, read-only copy of the mount
// of dir, rooted at dir. Files opened through it refer to a read-only mount,
// so they can not be opened for writing again, by anyone, through any path.
// The copy stays alive as long as any file opened through it does.
func openReadOnlyTree(dir string) (int, error) {
	treeFD, err := unix.OpenTree(unix.AT_FDCWD, dir, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return -1, err
	}
	attr := unix.MountAttr{Attr_set: unix.MOUNT_ATTR_RDONLY}
	if err := unix.MountSetattr(treeFD, "", unix.AT_EMPTY_PATH, &attr); err != nil {
		unix.Close(treeFD)
		return -1, err
	}
	return treeFD, nil
}

// openRestoreDir returns an FD for the checkpoint directory dir through which
// the files handed to the sandbox for a restore are opened. The sandbox only
// reads them and must not be able to modify them, which file permissions do
// not guarantee since it may run as root with CAP_DAC_OVERRIDE. So this is a
// detached, read-only copy of dir's mount (see openReadOnlyTree) when the
// kernel and runsc's privileges allow it, and dir itself otherwise.
func openRestoreDir(dir string) (int, error) {
	dirFD, err := openReadOnlyTree(dir)
	if err == nil {
		return dirFD, nil
	}
	log.Warningf("Checkpoint files in %q are not protected from modification by the sandbox: %v", dir, err)
	return unix.Open(dir, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
}

// openCheckpointFile opens the checkpoint file name in dir, relative to dirFD,
// an FD for dir. The path can not lead out of dir, so the file is on dirFD's
// mount.
func openCheckpointFile(dirFD int, dir, name string, flags int) (*os.File, error) {
	how := unix.OpenHow{
		Flags:   uint64(flags | unix.O_CLOEXEC),
		Resolve: unix.RESOLVE_BENEATH,
	}
	if flags&unix.O_CREAT != 0 {
		how.Mode = 0644
	}
	fd, err := unix.Openat2(dirFD, name, &how)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), filepath.Join(dir, name)), nil
}
