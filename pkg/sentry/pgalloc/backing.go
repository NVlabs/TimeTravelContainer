// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

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

// BackingProvider creates storage for one MemoryFile. A VM platform normally
// implements this with a view onto its shared GuestRAM allocator.
type BackingProvider interface {
	NewBacking() (Backing, error)
}

// Backing is platform-owned MemoryFile storage. Grow returns one mapping slot
// per newly added pgalloc chunk; relocatable implementations return zero slots
// and expose host addresses only through AcquireMapping. ResolveFrames is for
// platform page-table construction, not Sentry host access.
type Backing interface {
	Grow(oldSize, newSize uint64, huge bool) ([]uintptr, error)
	Commit(fr memmap.FileRange) error
	Decommit(fr memmap.FileRange) error
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

// ResolveBackingFrames returns the stable logical frames backing fr. It is
// used by VM AddressSpaces instead of translating host file descriptors or
// host physical addresses.
func (f *MemoryFile) ResolveBackingFrames(fr memmap.FileRange, at hostarch.AccessType) ([]BackingFrame, error) {
	if f.backing == nil {
		return nil, &NoPlatformBackingError{}
	}
	return f.backing.ResolveFrames(fr, at)
}

// NoPlatformBackingError indicates that a MemoryFile uses the ordinary host
// file backing and therefore has no VM frame identities.
type NoPlatformBackingError struct{}

func (*NoPlatformBackingError) Error() string {
	return "MemoryFile has no platform backing"
}
