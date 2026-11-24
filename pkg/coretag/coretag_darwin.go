// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

// Package coretag implements Linux core scheduling tags.
package coretag

import "errors"

var errUnavailable = errors.New("Linux core scheduling tags are unavailable on Darwin")

// Enable reports that core scheduling cannot be enabled on Darwin.
func Enable() (uint64, error) {
	return 0, errUnavailable
}

// Get reports that core scheduling cannot be queried on Darwin.
func Get() (uint64, error) {
	return 0, errUnavailable
}
