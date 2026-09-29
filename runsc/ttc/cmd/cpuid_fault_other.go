// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !amd64

package cmd

func hostSupportsCPUIDFaulting() bool {
	return true
}
