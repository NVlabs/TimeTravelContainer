// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build arm64

package arch

import (
	"encoding/binary"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch/fpu"
	"gvisor.dev/gvisor/pkg/usermem"
)

// TestSignalFrameLayout checks that signal frames are Linux's: a struct
// rt_sigframe of sizeof(struct rt_sigframe), 16-byte aligned, siginfo first,
// below a frame record of the interrupted fp and lr, which the handler's
// frame pointer points to.
func TestSignalFrameLayout(t *testing.T) {
	const (
		stackTop = 0x10000
		// sizeof(struct rt_sigframe) on arm64: a 128-byte siginfo and a
		// 4560-byte ucontext, whose mcontext is at 176, with 4096 bytes of
		// __reserved.
		rtSigframeSize = 4688
		mcontextOffset = 128 + 176
	)
	c := &Context64{}
	c.fpState = fpu.NewState()
	for i := range c.Regs.Regs {
		c.Regs.Regs[i] = uint64(0x1000 + i)
	}
	c.Regs.Sp = stackTop - 8
	c.Regs.Pc = 0x4000
	io := &usermem.BytesIO{Bytes: make([]byte, stackTop)}
	st := &Stack{Arch: c, IO: io, Bottom: stackTop - 8}
	act := &linux.SigAction{Handler: 0x5000, Restorer: 0x6000, Flags: linux.SA_SIGINFO}
	info := &linux.SignalInfo{Signo: int32(linux.SIGUSR1)}
	var alt linux.SignalStack
	if err := c.SignalSetup(st, act, info, &alt, 0, cpuid.FeatureSet{}); err != nil {
		t.Fatalf("SignalSetup: %v", err)
	}

	sp := c.Regs.Sp
	if sp%16 != 0 {
		t.Errorf("sp %#x isn't 16-byte aligned", sp)
	}
	if got, want := c.Regs.Regs[1], sp; got != want {
		t.Errorf("siginfo at %#x, want %#x", got, want)
	}
	if got, want := c.Regs.Regs[2], sp+128; got != want {
		t.Errorf("ucontext at %#x, want %#x", got, want)
	}
	record := c.Regs.Regs[29]
	if record < sp+rtSigframeSize || record+16 > stackTop-8 {
		t.Errorf("frame record at %#x, want in [%#x, %#x)", record, sp+rtSigframeSize, stackTop-8-16)
	}
	if fp, lr := binary.LittleEndian.Uint64(io.Bytes[record:]), binary.LittleEndian.Uint64(io.Bytes[record+8:]); fp != 0x1000+29 || lr != 0x1000+30 {
		t.Errorf("frame record holds fp %#x lr %#x, want %#x %#x", fp, lr, 0x1000+29, 0x1000+30)
	}
	// The mcontext, as unwinders find it, holds the interrupted registers.
	mc := sp + mcontextOffset
	if x0 := binary.LittleEndian.Uint64(io.Bytes[mc+8:]); x0 != 0x1000 {
		t.Errorf("mcontext x0 = %#x, want %#x", x0, 0x1000)
	}
	if pc := binary.LittleEndian.Uint64(io.Bytes[mc+8+31*8+8:]); pc != 0x4000 {
		t.Errorf("mcontext pc = %#x, want %#x", pc, 0x4000)
	}
	if got := (&SignalContext64{}).SizeBytes(); got != 4384 {
		t.Errorf("sizeof(struct sigcontext) = %d, want 4384", got)
	}

	// sigreturn restores the interrupted registers.
	c.Regs.Regs[0] = 0
	c.Regs.Pc = 0
	st.Bottom = hostarch.Addr(c.Regs.Sp)
	if _, _, err := c.SignalRestore(st, true, cpuid.FeatureSet{}); err != nil {
		t.Fatalf("SignalRestore: %v", err)
	}
	if c.Regs.Regs[0] != 0x1000 || c.Regs.Pc != 0x4000 || c.Regs.Sp != stackTop-8 {
		t.Errorf("SignalRestore restored x0 %#x pc %#x sp %#x", c.Regs.Regs[0], c.Regs.Pc, c.Regs.Sp)
	}
}
