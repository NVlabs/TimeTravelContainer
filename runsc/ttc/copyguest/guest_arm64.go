// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package copyguest

import _ "embed"

// linuxARM64 is a pure-Go Linux executable. The host passes its open file
// descriptor to the Sentry, so copy works even for distroless images.
//
//go:embed cmd/copy_helper_linux_arm64
var linuxARM64 []byte

// Binary returns the copy helper for the guest's ISA, the host's.
func Binary() []byte { return linuxARM64 }
