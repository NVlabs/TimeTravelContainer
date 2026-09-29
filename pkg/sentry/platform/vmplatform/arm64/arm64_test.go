// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build arm64

package arm64

import (
	"bytes"
	"errors"
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/cpuid"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	sentryplatform "gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform"
)

func TestSingleStepPState(t *testing.T) {
	context := arch.New(arch.ARM64)
	if context.SingleStep() {
		t.Fatal("new ARM64 context is single-stepping")
	}
	context.SetSingleStep()
	if !context.SingleStep() || context.Regs.Pstate&arch.ARMTrapFlag == 0 {
		t.Fatalf("SetSingleStep left PSTATE=%#x", context.Regs.Pstate)
	}
	context.ClearSingleStep()
	if context.SingleStep() || context.Regs.Pstate&arch.ARMTrapFlag != 0 {
		t.Fatalf("ClearSingleStep left PSTATE=%#x", context.Regs.Pstate)
	}
}

func TestPtraceGeneralRegistersPreserveTLS(t *testing.T) {
	context := arch.New(arch.ARM64)
	const tls = uint64(0x123456789abc)
	context.Regs.TPIDR_EL0 = tls
	var registers bytes.Buffer
	if _, err := context.PtraceGetRegs(&registers); err != nil {
		t.Fatal(err)
	}
	if _, err := context.PtraceSetRegs(&registers); err != nil {
		t.Fatal(err)
	}
	if context.Regs.TPIDR_EL0 != tls {
		t.Fatalf("PTRACE_SETREGSET(NT_PRSTATUS) changed TLS from %#x to %#x", tls, context.Regs.TPIDR_EL0)
	}
}

func TestPtraceTLSRegset(t *testing.T) {
	context := arch.New(arch.ARM64)
	const (
		ntARMTLS = uintptr(0x401)
		tls      = uint64(0x123456789abc)
	)
	var encoded [8]byte
	hostarch.ByteOrder.PutUint64(encoded[:], tls)
	if n, err := context.PtraceSetRegSet(ntARMTLS, bytes.NewReader(encoded[:]), len(encoded), cpuid.FeatureSet{}); err != nil || n != len(encoded) {
		t.Fatalf("setting NT_ARM_TLS: n=%d err=%v", n, err)
	}
	var got bytes.Buffer
	if n, err := context.PtraceGetRegSet(ntARMTLS, &got, len(encoded), cpuid.FeatureSet{}); err != nil || n != len(encoded) {
		t.Fatalf("getting NT_ARM_TLS: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got.Bytes(), encoded[:]) {
		t.Fatalf("NT_ARM_TLS bytes=%x, want %x", got.Bytes(), encoded)
	}
}

func TestPtraceReportsNoHardwareDebugSlots(t *testing.T) {
	context := arch.New(arch.ARM64)
	for _, regset := range []uintptr{linux.NT_ARM_HW_BREAK, linux.NT_ARM_HW_WATCH} {
		var got bytes.Buffer
		n, err := context.PtraceGetRegSet(regset, &got, 8, cpuid.FeatureSet{})
		if err != nil || n != 8 {
			t.Fatalf("getting regset %#x: n=%d err=%v", regset, n, err)
		}
		if got.Len() != 8 || !bytes.Equal(got.Bytes(), make([]byte, 8)) {
			t.Fatalf("regset %#x capability=%x, want zero-slot header", regset, got.Bytes())
		}
	}
}

func TestDecodeDebugException(t *testing.T) {
	for _, test := range []struct {
		name   string
		ec     uint64
		signal linux.Signal
		code   int32
	}{
		{name: "hardware breakpoint", ec: 0x30, signal: linux.SIGTRAP, code: linux.TRAP_HWBKPT},
		{name: "software step", ec: 0x32, signal: linux.SIGTRAP, code: linux.TRAP_TRACE},
		{name: "watchpoint", ec: 0x34, signal: linux.SIGTRAP, code: linux.TRAP_HWBKPT},
		{name: "BRK", ec: 0x3c, signal: linux.SIGTRAP, code: linux.TRAP_BRKPT},
		{name: "PC alignment", ec: 0x22, signal: linux.SIGBUS, code: 1},
		{name: "undefined instruction", ec: 0, signal: linux.SIGILL, code: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			const pc = uint64(0x12345000)
			info, access, err := (Architecture{}).DecodeExit(vmplatform.Exit{
				Reason:   vmplatform.ExitException,
				Syndrome: test.ec << 26,
				Address:  pc,
			})
			if !errors.Is(err, sentryplatform.ErrContextSignal) {
				t.Fatalf("DecodeExit error=%v, want ErrContextSignal", err)
			}
			if access != hostarch.NoAccess {
				t.Fatalf("DecodeExit access=%+v, want none", access)
			}
			if info.Signo != int32(test.signal) || info.Code != test.code || info.Addr() != pc {
				t.Fatalf("DecodeExit info=%+v addr=%#x, want signal=%d code=%d addr=%#x", info, info.Addr(), test.signal, test.code, pc)
			}
		})
	}
}

// TestDecodeFaultSignalCode checks that unresolved stage-1 aborts carry the
// Linux si_code rather than 0, which applications read as SI_USER.
func TestDecodeFaultSignalCode(t *testing.T) {
	for _, test := range []struct {
		name string
		esr  uint64
		code int32
	}{
		{name: "level 0 translation", esr: 0x24<<26 | 0x04, code: 1},
		{name: "level 3 translation", esr: 0x24<<26 | 0x07, code: 1},
		{name: "instruction translation", esr: 0x20<<26 | 0x06, code: 1},
		{name: "access flag", esr: 0x24<<26 | 0x0b, code: 2},
		{name: "permission", esr: 0x24<<26 | 1<<6 | 0x0f, code: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			info, _, err := (Architecture{}).DecodeExit(vmplatform.Exit{
				Reason:   vmplatform.ExitFault,
				Syndrome: test.esr,
				Address:  0x10,
			})
			if !errors.Is(err, sentryplatform.ErrContextSignal) {
				t.Fatalf("DecodeExit error=%v, want ErrContextSignal", err)
			}
			if info.Signo != int32(linux.SIGSEGV) || info.Code != test.code || info.Addr() != 0x10 {
				t.Fatalf("DecodeExit info=%+v addr=%#x, want SIGSEGV code=%d addr=0x10", info, info.Addr(), test.code)
			}
		})
	}
}
