// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rendererclient

import (
	"unsafe"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// MapInternal implements memmap.File.MapInternal. The renderer may shrink the
// memory, so the Sentry's accesses may fault, which safecopy handles.
func (m *hostMemory) MapInternal(fr memmap.FileRange, at hostarch.AccessType) (safemem.BlockSeq, error) {
	if err := m.checkRange(fr); err != nil {
		return safemem.BlockSeq{}, err
	}
	return safemem.BlockSeqOf(safemem.BlockFromUnsafePointer(unsafe.Pointer(&m.mapping[fr.Start]), int(fr.Length()))), nil
}

// HostAddress implements memmap.HostMemoryFile.HostAddress.
func (m *hostMemory) HostAddress(fr memmap.FileRange) (uintptr, error) {
	if err := m.checkRange(fr); err != nil {
		return 0, err
	}
	return uintptr(unsafe.Pointer(&m.mapping[0])) + uintptr(fr.Start), nil
}
