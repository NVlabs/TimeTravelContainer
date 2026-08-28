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

//go:build darwin && arm64

#include "textflag.h"

// bluepillTrap is entered with the C bridge pointer in R8 and the expected
// monitor state VA in R9. MRS traps as SIGILL at host EL0. The signal bridge
// resumes at the same instruction in guest EL1. A Go continuation may migrate
// after its call-scoped OS-thread lock is released, so verify TPIDR_EL1 on
// every entry. A wrong guest vCPU redpills and retries from host; a host MRS
// raises the controlled SIGILL and bluepills into the requested bridge.
	TEXT ·bluepillTrap(SB),NOSPLIT|NOFRAME,$0
begin:
			WORD $0xd538d08a // mrs x10, tpidr_el1
		CMP R10, R9
		BEQ right_vcpu
		HVC $0x73
		B begin
right_vcpu:
		RET

TEXT ·Bluepill(SB),NOSPLIT,$0-16
	MOVD bridge+0(FP), R8
	MOVD stateVA+8(FP), R9
	MOVD $0, R11
	CALL ·bluepillTrap(SB)
	RET

TEXT ·SwitchApplicationGuest(SB),NOSPLIT|NOFRAME,$0
	MOVD RSP, R2
	WORD $0xd538d080 // mrs x0, tpidr_el1
	MOVD R2, 0x3348(R0)
	ADD $0x4000, R0, R2
	MOVD R2, RSP
	MOVD 0x3340(R0), R1
	MOVD R30, R20
	CALL (R1)
	MOVD R20, R30
	RET

TEXT ·SwitchApplicationGuestAddress(SB),NOSPLIT,$0-8
	MOVD $·SwitchApplicationGuest(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·SwitchApplicationGuestTrapAddress(SB),NOSPLIT,$0-8
	MOVD $·SwitchApplicationGuest(SB), R0
	ADD $4, R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·EnsureHost(SB),NOSPLIT|NOFRAME,$0
	HVC $0x73
	RET

// Probe transfers this exact continuation to guest EL1, observes the current
// exception level there, and deliberately returns it to host EL0.
TEXT ·Probe(SB),NOSPLIT,$0-24
	MOVD bridge+0(FP), R8
	MOVD stateVA+8(FP), R9
	MOVD $0, R11
	CALL ·bluepillTrap(SB)
	CBNZ R11, probe_failed
	WORD $0xd5384240 // mrs x0, CurrentEL
	LSR $2, R0
	HVC $0x73
	MOVD R0, ret+16(FP)
	RET
probe_failed:
	MOVD $0, R0
	MOVD R0, ret+16(FP)
	RET

TEXT ·ApplicationProbe(SB),NOSPLIT,$0-32
	MOVD bridge+0(FP), R8
	MOVD stateVA+8(FP), R9
	MOVD $0, R11
	CALL ·bluepillTrap(SB)
	CBNZ R11, application_probe_failed
	MOVD monitorEntry+16(FP), R1
	MOVD RSP, R2
	WORD $0xd538d080 // mrs x0, tpidr_el1
	MOVD R2, 0x3348(R0)
	ADD $0x4000, R0, R2
	MOVD R2, RSP
	// R9 still carries stateVA. The Go argument frame is on the original stack
	// and must not be addressed after switching SP.
	MOVD R9, R0
	CALL (R1)
	WORD $0xd5384240 // mrs x0, CurrentEL
	LSR $2, R0
	HVC $0x73
	MOVD R0, ret+24(FP)
	RET
application_probe_failed:
	MOVD $0, R0
	MOVD R0, ret+24(FP)
	RET

TEXT ·HostSVCGetpid(SB),NOSPLIT|NOFRAME,$0-8
	MOVD $20, R16 // SYS_getpid
	WORD $0xd4001001 // svc #0x80
	MOVD R0, ret+0(FP)
	RET

TEXT ·BluepillTrapAddress(SB),NOSPLIT,$0-8
	MOVD $·bluepillTrap(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·ProbeAddress(SB),NOSPLIT,$0-8
	MOVD $·Probe(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·ApplicationProbeAddress(SB),NOSPLIT,$0-8
	MOVD $·ApplicationProbe(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·HostSVCGetpidAddress(SB),NOSPLIT,$0-8
	MOVD $·HostSVCGetpid(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·BluepillAddress(SB),NOSPLIT,$0-8
	MOVD $·Bluepill(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·EnsureHostTrapAddress(SB),NOSPLIT,$0-8
	MOVD $·EnsureHost(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·CurrentSP(SB),NOSPLIT|NOFRAME,$0-8
	MOVD RSP, R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·CurrentEL(SB),NOSPLIT|NOFRAME,$0-8
current_el_trap:
	WORD $0xd5384240 // mrs x0, CurrentEL
	LSR $2, R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·CurrentELTrapAddress(SB),NOSPLIT,$0-8
	MOVD $·CurrentEL(SB), R0
	MOVD R0, ret+0(FP)
	RET
