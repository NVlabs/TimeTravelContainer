// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package copyguest

import _ "embed"

// linuxAMD64 is a pure-Go Linux executable. The host passes its open file
// descriptor to the Sentry, so copy works even for distroless images.
//
//go:embed cmd/copy_helper_linux_amd64
var linuxAMD64 []byte

// Binary returns the copy helper for the guest's ISA, the host's.
func Binary() []byte { return linuxAMD64 }
