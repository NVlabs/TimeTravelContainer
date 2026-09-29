// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package sandboxsetup

import specs "github.com/opencontainers/runtime-spec/specs-go"

func ApplyCapsAllThreads(caps *specs.LinuxCapabilities) error { return nil }
