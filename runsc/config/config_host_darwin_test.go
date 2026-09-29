// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package config

func init() {
	// Generic config tests need a valid host configuration as their baseline.
	// Production binaries retain the explicit opt-in default (false).
	hostDefaultUnsafeNoHostIsolation = true
}
