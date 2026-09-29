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
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/moby/sys/capability"
	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/runsc/specutils"
)

// startSandboxProcess starts the Sentry in its namespaces.
func startSandboxProcess(cmd *exec.Cmd, nss []specs.LinuxNamespace) error {
	return specutils.StartInNS(cmd, nss)
}

func directIOFlag() int { return unix.O_DIRECT }

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
