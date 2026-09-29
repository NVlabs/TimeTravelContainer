// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package abi

// Host is the ABI implemented by the Sentry. On native macOS the execution
// host is Darwin, but the userspace kernel and traced applications still use
// the Linux ABI.
const Host = Linux
