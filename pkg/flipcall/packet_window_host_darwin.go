// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package flipcall

// Darwin's anonymous shared-file provider does not support Linux memfd seals.
// The FD is private to the trusted runsc process family in the initial macOS
// mode, so lifecycle code must not expose it to untrusted host processes.
func sealPacketWindowFile(_ int) error {
	return nil
}
