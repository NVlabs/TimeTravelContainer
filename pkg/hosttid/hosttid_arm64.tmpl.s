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

//go:build arm64

#include "textflag.h"

#define M_OFFSET      {{ Offset (Import "runtime") "g.m" }}
#define PROCID_OFFSET {{ Offset (Import "runtime") "m.procid" }}
#define P_OFFSET      {{ Offset (Import "runtime") "m.p" }}
#define MCACHE_OFFSET {{ Offset (Import "runtime") "p.mcache" }}

TEXT ·Current(SB),NOSPLIT,$0-8
	// procid is in getg().m.procid.
	MOVD g, R0      // g
	MOVD M_OFFSET(R0), R0 // gp.m
	MOVD PROCID_OFFSET(R0), R0 // mp.procid
	MOVD R0, ret+0(FP)
	RET

TEXT ·RuntimePointers(SB),NOSPLIT|NOFRAME,$0-32
	MOVD g, R0
	MOVD R0, gp+0(FP)
	MOVD M_OFFSET(R0), R1
	MOVD R1, mp+8(FP)
	MOVD P_OFFSET(R1), R2
	MOVD R2, pp+16(FP)
	MOVD MCACHE_OFFSET(R2), R3
	MOVD R3, mcachep+24(FP)
	RET

TEXT ·RuntimeTLSBase(SB),NOSPLIT|NOFRAME,$0-8
#ifdef GOOS_darwin
	// Darwin uses the read-only EL0 thread pointer for runtime TLS.
	WORD $0xd53bd060 // MRS TPIDRRO_EL0, R0
#else
	WORD $0xd53bd040 // MRS TPIDR_EL0, R0
#endif
	MOVD R0, ret+0(FP)
	RET
