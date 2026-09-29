// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build amd64

package arch

import (
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
)

func TestApplySignalFault(t *testing.T) {
	const addr = hostarch.Addr(0x12345000)
	tests := []struct {
		name string
		at   hostarch.AccessType
		code int32
		want uint64
	}{
		{name: "read", at: hostarch.Read, want: linux.X86_PF_USER},
		{name: "write protection", at: hostarch.Write, code: 2, want: linux.X86_PF_USER | linux.X86_PF_WRITE | linux.X86_PF_PROT},
		{name: "execute", at: hostarch.Execute, want: linux.X86_PF_USER | linux.X86_PF_INSTR},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := &Context64{}
			ctx.SetSignalFault(addr, test.at)
			info := linux.SignalInfo{Signo: int32(linux.SIGSEGV), Code: test.code}
			info.SetAddr(uint64(addr))
			var sc SignalContext64
			ctx.applySignalFault(&info, &sc)
			if sc.Trapno != x86TrapPageFault || sc.Err != test.want {
				t.Fatalf("got trap=%d err=%#x, want trap=%d err=%#x", sc.Trapno, sc.Err, x86TrapPageFault, test.want)
			}
			var consumed SignalContext64
			ctx.applySignalFault(&info, &consumed)
			if consumed.Trapno != 0 || consumed.Err != 0 {
				t.Fatalf("fault metadata was not consumed: %+v", consumed)
			}
		})
	}
}

func TestPtraceGetRegsDoesNotSynthesizeTLSSelectors(t *testing.T) {
	ctx := &Context64{}
	ctx.Regs.Fs = 0
	ctx.Regs.Gs = 0
	ctx.Regs.Fs_base = 0x1234
	ctx.Regs.Gs_base = 0x5678
	regs := ctx.ptraceGetRegs()
	if regs.Fs != 0 || regs.Gs != 0 {
		t.Fatalf("ptrace synthesized TLS selectors: FS=%#x GS=%#x", regs.Fs, regs.Gs)
	}
}
