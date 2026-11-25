// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && !cgo

package seatbelt

import "errors"

// Apply fails: applying a profile takes cgo, as ttcd's Darwin build has, and
// pure-Go builds, such as tests of the packages that use this one, lack.
func Apply(string) error {
	return errors.New("applying a Seatbelt profile requires a cgo build")
}

// Confined reports false: telling takes cgo.
func Confined() bool {
	return false
}

// BundleCacheDir returns "": finding the directory takes cgo.
func BundleCacheDir() (string, error) {
	return "", nil
}
