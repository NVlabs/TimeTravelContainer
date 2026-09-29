// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64 && !cgo

package safecopy

// initializePlatform is a build-host fallback for pure-Go code generators.
// Runnable Darwin sentries are built with cgo and install the Mach exception
// handler in safecopy_darwin.go.
func initializePlatform() error {
	return nil
}
