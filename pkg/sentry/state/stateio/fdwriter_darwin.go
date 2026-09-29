// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package stateio

import "gvisor.dev/gvisor/pkg/aio"

func newFDWriterQueue(_ int32, maxParallel int) (aio.Queue, bool) {
	return aio.NewSerialGoQueue(maxParallel), false
}
