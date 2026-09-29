// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin || !arm64

package copyguest

// Binary returns nil when the host TTC executable already has the Linux guest
// executable format. Other native-host/guest-ISA pairs will add an embedded
// helper alongside their platform implementation.
func Binary() []byte { return nil }
