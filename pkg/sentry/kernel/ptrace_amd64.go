// Copyright 2019 The gVisor Authors.
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

//go:build amd64
// +build amd64

package kernel

import (
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/usermem"
)

// ptracePrepareGuestGS retires any platform mechanism that privately owns GS
// before ptrace changes the tracee's guest-visible GS base. This is required
// for nested systrap: the inner Sentry installs GS on its syscall threads with
// PTRACE_SETREGSET, so the outer Sentry never observes an ARCH_SET_GS syscall
// from those threads.
func (t *Task) ptracePrepareGuestGS(target *Task, data hostarch.Addr, length int) error {
	regsSize := (*linux.PtraceRegs)(nil).SizeBytes()
	if length < regsSize {
		return nil // The subsequent register setter reports the short input.
	}
	buf := make([]byte, regsSize)
	if _, err := t.CopyInBytes(data, buf); err != nil {
		return err
	}
	var regs linux.PtraceRegs
	regs.UnmarshalUnsafe(buf)
	if uint64(target.Arch().GSBase()) == regs.Gs_base {
		return nil
	}
	return platform.PrepareGuestGSBase(target, target.Kernel().Platform, target.MemoryManager())
}

// ptraceArch implements arch-specific ptrace commands.
func (t *Task) ptraceArch(target *Task, req int64, addr, data hostarch.Addr) error {
	switch req {
	case linux.PTRACE_PEEKUSR: // aka PTRACE_PEEKUSER
		n, err := target.Arch().PtracePeekUser(uintptr(addr))
		if err != nil {
			return err
		}
		_, err = n.CopyOut(t, data)
		return err

	case linux.PTRACE_POKEUSR: // aka PTRACE_POKEUSER
		return target.Arch().PtracePokeUser(uintptr(addr), uintptr(data))

	case linux.PTRACE_GETREGS:
		// "Copy the tracee's general-purpose ... registers ... to the address
		// data in the tracer. ... (addr is ignored.) Note that SPARC systems
		// have the meaning of data and addr reversed ..."
		_, err := target.Arch().PtraceGetRegs(&usermem.IOReadWriter{
			Ctx:  t,
			IO:   t.MemoryManager(),
			Addr: data,
		})
		return err

	case linux.PTRACE_GETFPREGS:
		s := target.Arch().FloatingPointData()
		_, err := target.Arch().FloatingPointData().PtraceGetFPRegs(&usermem.IOReadWriter{
			Ctx:  t,
			IO:   t.MemoryManager(),
			Addr: data,
		}, len(*s))
		return err

	case linux.PTRACE_SETREGS:
		if err := t.ptracePrepareGuestGS(target, data, (*linux.PtraceRegs)(nil).SizeBytes()); err != nil {
			return err
		}
		_, err := target.Arch().PtraceSetRegs(&usermem.IOReadWriter{
			Ctx:  t,
			IO:   t.MemoryManager(),
			Addr: data,
		})
		if err == nil {
			target.p.FullStateChanged()
		}
		return err

	case linux.PTRACE_SETFPREGS:
		s := target.Arch().FloatingPointData()
		_, err := s.PtraceSetFPRegs(&usermem.IOReadWriter{
			Ctx:  t,
			IO:   t.MemoryManager(),
			Addr: data,
		}, len(*s))
		if err == nil {
			target.p.FullStateChanged()
		}
		return err

	default:
		return linuxerr.EIO
	}
}
