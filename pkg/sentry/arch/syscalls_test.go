// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package arch

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestPrepareSyscallEntryPtraceRegisters(t *testing.T) {
	ctx := New(Host)
	const firstArg = uintptr(0x12345678)
	ctx.SetReturn(firstArg)
	ctx.SyscallSaveOrig()
	ctx.PrepareSyscallEntry()

	switch Host {
	case ARM64:
		if got := ctx.Return(); got != firstArg {
			t.Fatalf("syscall-entry X0 = %#x, want first argument %#x", got, firstArg)
		}

		// PTRACE_SETREGSET can change X0 while the tracee is stopped. The
		// syscall path saves it again after the stop before collecting args.
		const changedArg = uintptr(0x87654321)
		ctx.SetReturn(changedArg)
		ctx.SyscallSaveOrig()
		if got := ctx.SyscallArgs()[0].Value; got != changedArg {
			t.Fatalf("first syscall argument after ptrace edit = %#x, want %#x", got, changedArg)
		}
	case AMD64:
		if got, want := ctx.Return(), ^uintptr(unix.ENOSYS-1); got != want {
			t.Fatalf("syscall-entry RAX = %#x, want %#x", got, want)
		}
	default:
		t.Fatalf("unexpected host architecture %v", Host)
	}
}
