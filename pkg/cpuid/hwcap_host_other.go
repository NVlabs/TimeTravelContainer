// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && (!darwin || !arm64)
// +build !linux
// +build !darwin !arm64

package cpuid

// initHWCap leaves the feature set empty on hosts without a supported native
// capability discovery mechanism.
func initHWCap() {}
