// Copyright 2026 The gVisor Authors.
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

package boot

import (
	"crypto/sha256"
	"encoding/json"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/bpf"
	"gvisor.dev/gvisor/pkg/seccomp"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/runsc/config"
)

func allowOnlySpec(syscall string) *specs.Spec {
	return &specs.Spec{Linux: &specs.Linux{Seccomp: &specs.LinuxSeccomp{
		DefaultAction: specs.ActErrno,
		Syscalls: []specs.LinuxSyscall{{
			Names:  []string{syscall},
			Action: specs.ActAllow,
		}},
	}}}
}

// linuxSysno returns the number of the Linux syscall name on the host's
// architecture, the guests': the host's own numbers are another system's on
// macOS.
func linuxSysno(t *testing.T, name string) uintptr {
	t.Helper()
	table, ok := kernel.LookupSyscallTable(abi.Linux, arch.Host)
	if !ok {
		t.Fatalf("no Linux syscall table of %v", arch.Host)
	}
	sysno, err := table.LookupNo(name)
	if err != nil {
		t.Fatalf("LookupNo(%q): %v", name, err)
	}
	return sysno
}

func runFilter(t *testing.T, p *bpf.Program, sysno uintptr) uint32 {
	t.Helper()
	d := linux.SeccompData{Nr: int32(sysno), Arch: seccomp.LINUX_AUDIT_ARCH}
	ret, err := bpf.Exec[bpf.NativeEndian](*p, seccomp.DataAsBPFInput(&d, make([]byte, d.SizeBytes())))
	if err != nil {
		t.Fatalf("bpf.Exec: %v", err)
	}
	return ret
}

func TestOCISeccompProgramCache(t *testing.T) {
	conf := &config.Config{OCISeccomp: true}
	const (
		allow = uint32(linux.SECCOMP_RET_ALLOW)
		eperm = uint32(linux.SECCOMP_RET_ERRNO) | uint32(unix.EPERM)
	)

	getpid1, err := buildOCISeccompProgram(conf, allowOnlySpec("getpid"))
	if err != nil {
		t.Fatalf("building getpid profile: %v", err)
	}
	getpid2, err := buildOCISeccompProgram(conf, allowOnlySpec("getpid"))
	if err != nil {
		t.Fatalf("building getpid profile again: %v", err)
	}
	if getpid1 != getpid2 {
		t.Errorf("same profile built twice: got programs %p and %p, want one cached program", getpid1, getpid2)
	}

	getppid, err := buildOCISeccompProgram(conf, allowOnlySpec("getppid"))
	if err != nil {
		t.Fatalf("building getppid profile: %v", err)
	}
	for _, tc := range []struct {
		name  string
		p     *bpf.Program
		sysno uintptr
		want  uint32
	}{
		{"getpid profile, getpid", getpid2, linuxSysno(t, "getpid"), allow},
		{"getpid profile, getppid", getpid2, linuxSysno(t, "getppid"), eperm},
		{"getppid profile, getpid", getppid, linuxSysno(t, "getpid"), eperm},
		{"getppid profile, getppid", getppid, linuxSysno(t, "getppid"), allow},
	} {
		if got := runFilter(t, tc.p, tc.sysno); got != tc.want {
			t.Errorf("%s: got %#x, want %#x", tc.name, got, tc.want)
		}
	}
}

func TestOCISeccompProgramCacheSkipsErrors(t *testing.T) {
	conf := &config.Config{OCISeccomp: true}
	bad := allowOnlySpec("getpid")
	bad.Linux.Seccomp.DefaultAction = "SCMP_ACT_BOGUS"
	for i := 0; i < 2; i++ {
		if p, err := buildOCISeccompProgram(conf, bad); err == nil {
			t.Errorf("call %d with invalid profile: got program %p, want error", i, p)
		}
	}
	profile, _ := json.Marshal(bad.Linux.Seccomp)
	if _, ok := ociSeccompCache.programs[sha256.Sum256(profile)]; ok {
		t.Errorf("invalid profile was cached")
	}
}
