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

//go:build darwin

package sandboxsetup

import (
	"fmt"
	"os"
	"os/exec"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/flag"
	"gvisor.dev/gvisor/runsc/specutils"
	"gvisor.dev/gvisor/runsc/starttime"
)

func execSelf(args []string) error {
	if err := unix.Exec(specutils.ExePath, args, starttime.AppendEnviron(os.Environ())); err != nil {
		return fmt.Errorf("error executing %s: %w", specutils.ExePath, err)
	}
	panic("unreachable")
}

func SetCapsAndCallSelf(args []string, caps *specs.LinuxCapabilities) error { return execSelf(args) }
func CallSelfAsNobody(args []string) error                                  { return execSelf(args) }

func PrepareArgs(subCmdName string, fSet *flag.FlagSet, override map[string]string) []string {
	var args []string
	for _, arg := range os.Args {
		args = append(args, arg)
		if arg == subCmdName {
			break
		}
	}
	fSet.Visit(func(gf *flag.Flag) {
		if ov, ok := override[gf.Name]; ok {
			args = append(args, fmt.Sprintf("--%s=%s", gf.Name, ov))
			delete(override, gf.Name)
			return
		}
		args = append(args, fmt.Sprintf("--%s=%s", gf.Name, gf.Value))
	})
	for of, ov := range override {
		args = append(args, fmt.Sprintf("--%s=%s", of, ov))
	}
	return append(args, fSet.Args()...)
}

func SyncUsernsForRootless(fd int, uid, gid uint32) {
	if fd >= 0 {
		_ = WaitForFD(fd, "userns sync FD")
	}
}

func ExecProcUmounter(*os.File) (*exec.Cmd, *os.File) { return nil, nil }
func UmountProc(syncFD int)                           {}
func UmountProcInProcess()                            {}
func UmountProcFile(syncFile *os.File) {
	if syncFile != nil {
		_ = syncFile.Close()
	}
}
