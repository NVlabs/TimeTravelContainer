// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && cgo

#include "textflag.h"

TEXT ·addrOfHandleMemcpyFault(SB), NOSPLIT, $0-8
	MOVD $handleMemcpyFault(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·addrOfHandleMemclrFault(SB), NOSPLIT, $0-8
	MOVD $handleMemclrFault(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·addrOfHandleSwapUint32Fault(SB), NOSPLIT, $0-8
	MOVD $handleSwapUint32Fault(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·addrOfHandleSwapUint64Fault(SB), NOSPLIT, $0-8
	MOVD $handleSwapUint64Fault(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·addrOfHandleCompareAndSwapUint32Fault(SB), NOSPLIT, $0-8
	MOVD $handleCompareAndSwapUint32Fault(SB), R0
	MOVD R0, ret+0(FP)
	RET

TEXT ·addrOfHandleLoadUint32Fault(SB), NOSPLIT, $0-8
	MOVD $handleLoadUint32Fault(SB), R0
	MOVD R0, ret+0(FP)
	RET
