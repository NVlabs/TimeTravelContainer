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

#include "textflag.h"

#define M_OFFSET      {{ Offset (Import "runtime") "g.m" }}
#define PROCID_OFFSET {{ Offset (Import "runtime") "m.procid" }}
#define P_OFFSET      {{ Offset (Import "runtime") "m.p" }}
#define MCACHE_OFFSET {{ Offset (Import "runtime") "p.mcache" }}
#define G0_OFFSET     {{ Offset (Import "runtime") "m.g0" }}
#define GSIGNAL_OFFSET {{ Offset (Import "runtime") "m.gsignal" }}
#define STACK_OFFSET  {{ Offset (Import "runtime") "g.stack" }}

TEXT ·Current(SB),NOSPLIT|NOFRAME,$0-8
	// procid is in getg().m.procid.
	MOVQ TLS, AX
	MOVQ 0(AX)(TLS*1), AX
	MOVQ M_OFFSET(AX), AX // gp.m
	MOVQ PROCID_OFFSET(AX), AX // mp.procid
	MOVQ AX, ret+0(FP)
	RET

TEXT ·RuntimePointers(SB),NOSPLIT|NOFRAME,$0-80
	MOVQ TLS, AX
	MOVQ 0(AX)(TLS*1), AX
	MOVQ AX, gp+0(FP)
	MOVQ M_OFFSET(AX), CX
	MOVQ CX, mp+8(FP)
	MOVQ P_OFFSET(CX), DX
	MOVQ DX, pp+16(FP)
	MOVQ MCACHE_OFFSET(DX), BX
	MOVQ BX, mcachep+24(FP)
	MOVQ G0_OFFSET(CX), SI
	MOVQ SI, g0p+32(FP)
	MOVQ STACK_OFFSET(SI), DI
	MOVQ DI, g0StackLo+40(FP)
	MOVQ (STACK_OFFSET+8)(SI), DI
	MOVQ DI, g0StackHi+48(FP)
	MOVQ GSIGNAL_OFFSET(CX), SI
	MOVQ SI, gsignalp+56(FP)
	TESTQ SI, SI
	JZ no_gsignal
	MOVQ STACK_OFFSET(SI), DI
	MOVQ DI, gsignalStackLo+64(FP)
	MOVQ (STACK_OFFSET+8)(SI), DI
	MOVQ DI, gsignalStackHi+72(FP)
	RET
no_gsignal:
	MOVQ $0, gsignalStackLo+64(FP)
	MOVQ $0, gsignalStackHi+72(FP)
	RET

TEXT ·RuntimeTLSBase(SB),NOSPLIT|NOFRAME,$0-8
	MOVQ TLS, AX
	MOVQ AX, ret+0(FP)
	RET
