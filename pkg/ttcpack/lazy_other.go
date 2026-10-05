// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package ttcpack

// startWriteback does nothing where writeback cannot be started apart from a
// sync: each file of a fetch is synced in turn.
func startWriteback(int) {}
