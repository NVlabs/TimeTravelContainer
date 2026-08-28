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
