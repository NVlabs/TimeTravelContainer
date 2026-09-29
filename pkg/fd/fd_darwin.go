// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package fd

// Darwin has no O_LARGEFILE; all files use 64-bit offsets.
const openExtraFlags = 0
