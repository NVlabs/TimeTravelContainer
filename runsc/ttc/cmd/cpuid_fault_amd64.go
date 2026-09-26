// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0

//go:build amd64

package cmd

import (
	"runtime"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
)

// hostSupportsCPUIDFaulting reports whether systrap can enforce a virtual
// CPUID contract. ARCH_SET_CPUID is per-thread, so probe and restore it on one
// locked thread. No Go code between the two raw syscalls executes CPUID.
func hostSupportsCPUIDFaulting() bool {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	_, _, disableErrno := unix.RawSyscall(unix.SYS_ARCH_PRCTL, linux.ARCH_SET_CPUID, 0, 0)
	if disableErrno != 0 {
		return false
	}
	_, _, enableErrno := unix.RawSyscall(unix.SYS_ARCH_PRCTL, linux.ARCH_SET_CPUID, 1, 0)
	return enableErrno == 0
}
