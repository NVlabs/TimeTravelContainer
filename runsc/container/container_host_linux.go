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

package container

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/runsc/specutils"
)

func containerHostSocketPair() ([2]int, error) {
	return unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
}

func setGoferParentDeathSignal(cmd *exec.Cmd) { cmd.SysProcAttr.Pdeathsig = unix.SIGKILL }

func setGoferCommandCredential(cmd *exec.Cmd, uid, gid uint32) error {
	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid}
	return nil
}

func disableGoferSetgroupsForIDMapping(cmd *exec.Cmd) error {
	cmd.SysProcAttr.GidMappingsEnableSetgroups = false
	return nil
}

// enableGoferSetgroupsForHostUDS lets a gofer whose user namespace maps its
// groups call setgroups(2), where the namespace allows it: connecting to a host
// socket applies the application's supplementary groups. Allowing it also
// makes os/exec start the gofer with no supplementary groups.
func enableGoferSetgroupsForHostUDS(cmd *exec.Cmd) error {
	if len(cmd.SysProcAttr.GidMappings) > 0 && specutils.SetgroupsAllowed() {
		cmd.SysProcAttr.GidMappingsEnableSetgroups = true
	}
	return nil
}

func hostSetOOMScoreAdj(pid, scoreAdj int) error {
	f, err := os.OpenFile(fmt.Sprintf("/proc/%d/oom_score_adj", pid), os.O_WRONLY, 0644)
	if err != nil {
		if os.IsNotExist(err) {
			log.Warningf("Process (%d) not found setting oom_score_adj", pid)
			return nil
		}
		return err
	}
	defer f.Close()
	if _, err := f.WriteString(strconv.Itoa(scoreAdj)); err != nil {
		if errors.Is(err, unix.ESRCH) {
			log.Warningf("Process (%d) exited while setting oom_score_adj", pid)
			return nil
		}
		return fmt.Errorf("setting oom_score_adj to %d: %v", scoreAdj, err)
	}
	return nil
}
