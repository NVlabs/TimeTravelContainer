// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// FrameID is a stable logical 4 KiB frame identity. It is independent of GPA,
// host virtual address, VM epoch, and checkpoint stream offset.
type FrameID uint64

// BackingFrame describes the current execution-cache location of a logical
// frame. GPA and HostAddress are disposable and must never be checkpointed.
type BackingFrame struct {
	ID          FrameID
	FileOffset  uint64
	GPA         uint64
	HostAddress uintptr
}

// BackingMappingRequest describes one logical MemoryFile range in an atomic
// host-mapping lease acquisition.
type BackingMappingRequest struct {
	Range  memmap.FileRange
	Access hostarch.AccessType
}

// BackingProvider creates storage for one MemoryFile. A VM platform normally
// implements this with a view onto its shared GuestRAM allocator.
type BackingProvider interface {
	NewBacking() (Backing, error)
}

// Backing is platform-owned MemoryFile storage. Grow returns one mapping slot
// per newly added pgalloc chunk; relocatable implementations return zero slots
// and expose host addresses only through AcquireMapping. ResolveFrames is for
// backing management and inspection; stage-1 page-table construction must use
// MemoryFile.AcquireBackingFrames so physical lifetime is pinned.
type Backing interface {
	Grow(oldSize, newSize uint64, huge bool) ([]uintptr, error)
	Commit(fr memmap.FileRange) error
	Decommit(fr memmap.FileRange) error
	// QueryCommitment stores the commitment state of each logical page in fr
	// in committed, using one byte per hostarch.PageSize page. The low bit is
	// set for committed pages and clear for uncommitted pages. Implementations
	// must overwrite every byte in committed.
	QueryCommitment(fr memmap.FileRange, committed []byte) error
	TotalUsage() (uint64, error)
	Sync() error
	DropCache(fr memmap.FileRange) error
	DataFD(fr memmap.FileRange) (int, error)
	ResolveFrames(fr memmap.FileRange, at hostarch.AccessType) ([]BackingFrame, error)
	// AcquireMapping resolves fr while preventing physical frame relocation.
	// The returned lease must be released after all host accesses through the
	// frame addresses have completed.
	AcquireMapping(fr memmap.FileRange, at hostarch.AccessType) ([]BackingFrame, memmap.InternalMappingLease, error)
	Destroy() error
}

// backingTryMapper is implemented by backings whose AcquireMapping can wait
// for an exclusive operation, such as a compaction, that itself waits for every
// outstanding lease. A caller that already holds a lease must not wait there.
type backingTryMapper interface {
	// TryAcquireMapping is AcquireMapping, but reports acquired == false
	// instead of waiting.
	TryAcquireMapping(fr memmap.FileRange, at hostarch.AccessType) (frames []BackingFrame, lease memmap.InternalMappingLease, acquired bool, err error)

	// AwaitMappingIdle waits for the exclusive operations that
	// TryAcquireMapping could have observed to finish. The caller must not
	// hold a lease.
	AwaitMappingIdle()
}

type backingBatchMapper interface {
	AcquireMappings([]BackingMappingRequest) ([][]BackingFrame, memmap.InternalMappingLease, error)
}

// ResolveBackingFrames returns the stable logical frames backing fr without a
// lifetime lease. It is intended for backing management and inspection. VM
// AddressSpaces must use AcquireBackingFrames.
func (f *MemoryFile) ResolveBackingFrames(fr memmap.FileRange, at hostarch.AccessType) ([]BackingFrame, error) {
	if f.backing == nil {
		return nil, &NoPlatformBackingError{}
	}
	// VM AddressSpaces resolve GuestRAM frames directly instead of obtaining a
	// host mapping through MapInternal. They must nevertheless observe the same
	// remote-restore interlock: resolving an unloaded sparse page would allocate
	// and map a zero frame before the asynchronous loader installs its contents.
	if err := f.awaitBackingLoads(fr); err != nil {
		return nil, err
	}
	return f.backing.ResolveFrames(fr, at)
}

// AcquireBackingFrames returns the stable logical frames backing fr and pins
// their physical storage until the returned lease is released. VM address
// spaces must retain this lease until the monitor has acknowledged the
// stage-1 transaction and every new reverse mapping has been recorded. This
// closes the lifetime window in which Decommit could otherwise retire a frame
// after resolution but before its new PTE became visible to the reverse map.
func (f *MemoryFile) AcquireBackingFrames(fr memmap.FileRange, at hostarch.AccessType) ([]BackingFrame, memmap.InternalMappingLease, error) {
	if f.backing == nil {
		return nil, nil, &NoPlatformBackingError{}
	}
	if err := f.awaitBackingLoads(fr); err != nil {
		return nil, nil, err
	}
	return f.backing.AcquireMapping(fr, at)
}

// NoPlatformBackingError indicates that a MemoryFile uses the ordinary host
// file backing and therefore has no VM frame identities.
type NoPlatformBackingError struct{}

func (*NoPlatformBackingError) Error() string {
	return "MemoryFile has no platform backing"
}
