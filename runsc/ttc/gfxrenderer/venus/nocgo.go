// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !cgo

// Package venus is TTC's renderer for Mesa's Venus Vulkan driver. It needs
// cgo, to call Vulkan.
package venus

import "errors"

var errNoCgo = errors.New("the renderer was built without cgo, which it needs to call Vulkan")

// CapsetVenus is the ID of Venus's capability set.
const CapsetVenus = 4

// CapsetCrossDomain is the ID of the cross-domain capability set.
const CapsetCrossDomain = 5

// CrossDomainCapset returns the cross-domain capability set. Without cgo,
// there is none.
func CrossDomainCapset() []byte {
	return nil
}

// Serve renders for the Sentry connected to fd. Without cgo, it cannot.
func Serve(fd int) error {
	return errNoCgo
}

// Probe returns Venus's capability set. Without cgo, it cannot.
func Probe() ([]byte, error) {
	return nil, errNoCgo
}

// PendingClones returns the number of clones that forks' children have yet
// to adopt. Without cgo, there are none.
func PendingClones() int {
	return 0
}
