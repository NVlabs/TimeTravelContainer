// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !((darwin || linux) && arm64) && !(linux && amd64)

package copyguest

// Binary returns nil for a host without a copy helper of its own, whose TTC
// executable copies in the guest. Other native-host/guest-ISA pairs will add
// an embedded helper alongside their platform implementation.
func Binary() []byte { return nil }
