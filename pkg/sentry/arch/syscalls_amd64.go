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

//go:build amd64
// +build amd64

package arch

import "golang.org/x/sys/unix"

// RestartSyscallNr is the syscall number for restart_syscall(2).
const RestartSyscallNr = uintptr(219)

// SyscallSaveOrig save the value of the register which is clobbered in
// syscall handler(doSyscall()).
//
// Noop on x86.
func (c *Context64) SyscallSaveOrig() {
}

// PrepareSyscallEntry implements contextInterface.PrepareSyscallEntry.
func (c *Context64) PrepareSyscallEntry() {
	// Linux exposes -ENOSYS in RAX between syscall entry and invocation.
	errno := uintptr(unix.ENOSYS)
	c.SetReturn(-errno)
}

// SyscallNo returns the syscall number according to the 64-bit convention.
func (c *Context64) SyscallNo() uintptr {
	return uintptr(c.Regs.Orig_rax)
}

// SyscallArgs provides syscall arguments according to the 64-bit convention.
//
// Due to the way addresses are mapped for the sentry this binary *must* be
// built in 64-bit mode. So we can just assume the syscall numbers that come
// back match the expected host system call numbers.
func (c *Context64) SyscallArgs() SyscallArguments {
	return SyscallArguments{
		SyscallArgument{Value: uintptr(c.Regs.Rdi)},
		SyscallArgument{Value: uintptr(c.Regs.Rsi)},
		SyscallArgument{Value: uintptr(c.Regs.Rdx)},
		SyscallArgument{Value: uintptr(c.Regs.R10)},
		SyscallArgument{Value: uintptr(c.Regs.R8)},
		SyscallArgument{Value: uintptr(c.Regs.R9)},
	}
}

// RestartSyscall implements Context.RestartSyscall.
func (c *Context64) RestartSyscall() {
	c.Regs.Rip -= SyscallWidth
	c.Regs.Rax = c.Regs.Orig_rax
}

// RestartSyscallWithRestartBlock implements Context.RestartSyscallWithRestartBlock.
func (c *Context64) RestartSyscallWithRestartBlock() {
	c.Regs.Rip -= SyscallWidth
	c.Regs.Rax = uint64(RestartSyscallNr)
}

// ReenterRestartedSyscall implements Context.ReenterRestartedSyscall. The
// syscall instruction leaves RCX and R11 as its first execution did.
func (c *Context64) ReenterRestartedSyscall() {
	c.Regs.Rip += SyscallWidth
	c.Regs.Orig_rax = c.Regs.Rax
}
