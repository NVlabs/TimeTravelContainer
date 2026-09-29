// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin || !arm64

package time

func platformInitialClockParameters(ClockID) (Parameters, bool) {
	return Parameters{}, false
}
