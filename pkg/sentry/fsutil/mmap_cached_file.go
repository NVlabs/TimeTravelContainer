// Copyright 2018 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package fsutil

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/fdbudget"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sync"
)

// MmapCachedFile implements MmapFile. It differs from MmapPreciseFile in the
// following notable ways:
//   - MmapCachedFile tracks referenced pages as host page cache usage in
//     sentry memory accounting. (This is the "cache" referred to by the name of
//     MmapCachedFile.) The AddMapping and RemoveMapping methods manipulate
//     reference counts without memory accounting.
//   - MmapCachedFile creates sentry mappings of referenced pages at aligned
//     units called "chunks", to expedite mapping reference counting and lookup.
//
// SetFD must be called on zero-value MmapCachedFiles before first use.
//
// +stateify savable
type MmapCachedFile struct {
	memmap.DefaultMemoryType

	// NoBufferedIOFallback is unused, as MmapCachedFile has a buffered I/O
	// fallback. It remains so that saved images keep this type's fields.
	NoBufferedIOFallback memmap.NoBufferedIOFallback

	// fd is the file descriptor.
	fd atomicbitops.Int32

	refsMu refsMutex `state:"nosave"`

	// +checklocks:refsMu
	mappableReleased bool

	// refs maps chunk start offsets to the sum of reference counts for all
	// pages in that chunk.
	// +checklocks:refsMu
	refs map[uint64]int64

	// +checklocks:refsMu
	memAcct FrameRefSet

	mapsMu mapsMutex `state:"nosave"`

	// mappings maps chunk start offsets to mappings of those chunks.
	// +checklocks:mapsMu
	mappings map[uint64]mapping `state:"nosave"`

	// fdMu excludes closing an idle descriptor (CloseIdleFD) while a caller
	// maps it: MapInternal, and address spaces (PinDataFD). It precedes
	// mapsMu.
	fdMu sync.RWMutex `state:"nosave"`

	// reopen, if set, opens the file again, so that the Sentry's budget of
	// host file descriptors may close f's descriptor while it is idle (see
	// SetReopen). evicted is set while it has. Both are protected by fdMu;
	// mayEvict is set once reopen is.
	reopen   func() (int, error) `state:"nosave"`
	evicted  bool                `state:"nosave"`
	mayEvict atomicbitops.Bool   `state:"nosave"`

	// fdEntry registers f's descriptor with the budget while reopen is set.
	fdEntry fdbudget.Entry `state:"nosave"`
}

// chunkShift returns the binary log of the chunk size, which is the huge page
// size. It is not a constant since the huge page size is only known at runtime
// on some architectures.
//
// TODO: In addition to compounding the repeated remapping problem
// described in MmapCachedFile.DecRef, this small chunk size may contribute
// to host VMA exhaustion. Consider increasing chunkShift. (But note that
// larger values of chunkShift increase the speed with which applications
// can consume sentry's address space, since only one byte per chunk needs
// to be touched to cause the whole chunk to be mapped.)
func chunkShift() uint {
	return hostarch.HugePageShift
}

// chunkSize returns the size of a chunk.
func chunkSize() uint64 {
	return 1 << chunkShift()
}

// chunkMask returns chunkSize() - 1.
func chunkMask() uint64 {
	return chunkSize() - 1
}

func pagesInChunk(fr memmap.FileRange, chunkStart uint64) int64 {
	return int64(fr.Intersect(memmap.FileRange{Start: chunkStart, End: chunkStart + chunkSize()}).Length() >> hostarch.PageShift)
}

// cachedChunks counts the Sentry's mappings of chunks of MmapCachedFiles.
var cachedChunks atomicbitops.Int64

var chunkBudget struct {
	bounded atomicbitops.Bool
	once    sync.Once
	n       int64
}

// BoundChunkMappings makes MmapCachedFiles keep at most chunkMappingBudget
// chunks mapped in the Sentry. The Sentry calls it on platforms whose address
// spaces map files themselves; one whose address spaces map the Sentry's
// mappings of files (KVM) needs every chunk that its guests map.
func BoundChunkMappings() {
	chunkBudget.bounded.Store(true)
}

// chunkMappingBudget returns how many chunks MmapCachedFiles may keep mapped
// in the Sentry, if BoundChunkMappings was called: a quarter of the mappings
// that the host allows a process, as application memory may take another
// quarter (see pgalloc's homes). Past it, MapInternal declines to map more
// chunks (memmap.BufferedIOFallbackErr), and the Sentry reads and writes what
// it would have mapped with buffered I/O: slower, but its guests do not fail
// for the host's limit.
func chunkMappingBudget() int64 {
	if !chunkBudget.bounded.Load() {
		return math.MaxInt64
	}
	chunkBudget.once.Do(func() {
		chunkBudget.n = int64(hostmm.MaxMapCount() / 4)
	})
	return chunkBudget.n
}

// +stateify savable
type mapping struct {
	addr     uintptr
	writable bool
}

// SetFD implements MmapFile.SetFD.
//
// It is legal to call SetFD on a MmapCachedFile more than once; however,
// after the first call to SetFD that passes a non-negative fd, all calls to
// SetFD must pass the same fd.
func (f *MmapCachedFile) SetFD(fd int) {
	f.fd.Store(int32(fd))
}

// ReplaceFD changes the backing file descriptor and discards every cached
// internal mapping, which will be recreated lazily. Ownership of fd transfers
// to f on success. This is used by branchable filesystems after invalidating
// all application translations and publishing a copy-on-write backing object.
//
// The caller must ensure that MappableRelease is not concurrent and that no
// application translation can access the old internal mappings.
func (f *MmapCachedFile) ReplaceFD(fd int) error {
	if fd < 0 {
		return unix.EBADF
	}
	f.resetFD(fd)
	return nil
}

// ResetFD discards every cached internal mapping and closes the backing file
// descriptor without releasing the MmapCachedFile itself. A later ReplaceFD
// may install a new backing descriptor. Branchable filesystems use this while
// publishing a copy-on-write object transition so an unreachable old object
// cannot remain pinned by a dormant mmap cache.
//
// The caller must ensure the same conditions as ReplaceFD and must prevent a
// concurrent MapInternal until a new descriptor has been installed.
func (f *MmapCachedFile) ResetFD() {
	f.resetFD(-1)
}

func (f *MmapCachedFile) resetFD(fd int) {
	f.fdMu.Lock()
	defer f.fdMu.Unlock()
	f.mapsMu.Lock()
	for chunkStart, m := range f.mappings {
		f.unmapAndRemove(chunkStart, m)
	}
	f.mapsMu.Unlock()
	oldFD := int(f.fd.Swap(int32(fd)))
	if oldFD >= 0 {
		unix.Close(oldFD)
	}
	// A new descriptor is of another object, which reopen does not open.
	f.dropReopenLocked()
}

func (f *MmapCachedFile) close() {
	f.fdMu.Lock()
	defer f.fdMu.Unlock()
	if fd := f.fd.Load(); fd >= 0 {
		unix.Close(int(fd))
		f.fd.Store(-1)
	}
	f.dropReopenLocked()
}

// SetReopen makes f's descriptor one that the Sentry's budget of host file
// descriptors may close while it is idle, to stay within the host's limit on
// them: reopen opens the file again when f next needs a descriptor, to map a
// chunk that it has not mapped (MapInternal) or to map the file into an
// address space (PinDataFD). Mappings of the file outlast its descriptor.
//
// Preconditions: SetFD has been called with a valid descriptor.
func (f *MmapCachedFile) SetReopen(reopen func() (int, error)) {
	f.fdMu.Lock()
	defer f.fdMu.Unlock()
	// The descriptor is registered while it is open and reopen is set.
	registered := f.reopen != nil && !f.evicted
	f.reopen = reopen
	f.mayEvict.Store(true)
	if fd := f.fd.Load(); fd >= 0 && !registered {
		fdbudget.Opened(&f.fdEntry, f, int(fd))
	}
}

// dropReopenLocked forgets reopen, and f's descriptor, which is closed or
// replaced, with the budget.
//
// +checklocks:f.fdMu
func (f *MmapCachedFile) dropReopenLocked() {
	if f.reopen != nil {
		fdbudget.Closed(&f.fdEntry)
	}
	f.reopen = nil
	f.evicted = false
}

// CloseIdleFD implements fdbudget.Holder.CloseIdleFD.
func (f *MmapCachedFile) CloseIdleFD() bool {
	if !f.fdMu.TryLock() {
		return false
	}
	defer f.fdMu.Unlock()
	if fd := f.fd.Load(); fd >= 0 && f.reopen != nil {
		unix.Close(int(fd))
		f.fd.Store(-1)
		f.evicted = true
	}
	return true
}

// PinDataFD implements memmap.DataFDPinner.PinDataFD.
func (f *MmapCachedFile) PinDataFD(fr memmap.FileRange) (int, error) {
	return f.pinFD()
}

// UnpinDataFD implements memmap.DataFDPinner.UnpinDataFD.
func (f *MmapCachedFile) UnpinDataFD() {
	f.fdMu.RUnlock()
}

// pinFD returns f's descriptor, opening it again if the budget closed it,
// which stays open until f.fdMu.RUnlock.
func (f *MmapCachedFile) pinFD() (int, error) {
	for {
		f.fdMu.RLock()
		fd := f.fd.Load()
		if fd >= 0 {
			if f.reopen != nil {
				f.fdEntry.Used()
			}
			return int(fd), nil
		}
		evicted := f.evicted
		f.fdMu.RUnlock()
		if !evicted {
			return -1, unix.EBADF
		}
		if err := f.reopenFD(); err != nil {
			return -1, err
		}
	}
}

// reopenFD opens f's descriptor again, after the budget closed it.
func (f *MmapCachedFile) reopenFD() error {
	f.fdMu.Lock()
	defer f.fdMu.Unlock()
	if !f.evicted {
		return nil
	}
	fd, err := f.reopen()
	if err != nil && fdbudget.Retry(err) {
		fd, err = f.reopen()
	}
	if err != nil {
		log.Warningf("fsutil.MmapCachedFile: opening a file again: %v", err)
		return err
	}
	f.fd.Store(int32(fd))
	f.evicted = false
	fdbudget.Opened(&f.fdEntry, f, fd)
	return nil
}

// MappableRelease implements MmapFile.MappableRelease.
func (f *MmapCachedFile) MappableRelease() {
	f.refsMu.Lock()
	defer f.refsMu.Unlock()
	if f.mappableReleased {
		return
	}
	f.mappableReleased = true
	if len(f.refs) == 0 {
		f.close()
	}
}

// IncRef implements memmap.File.IncRef.
func (f *MmapCachedFile) IncRef(fr memmap.FileRange, memCgID uint32) {
	f.refsMu.Lock()
	defer f.refsMu.Unlock()
	f.incRefUnaccounted(fr)
	f.memAcct.IncRefAndAccount(fr, memCgID)
}

// AddMapping is called by implementations of memmap.Mappable.AddMapping to
// increment the reference count on the pages mapped by ar, starting at the
// given file offset, without affecting memory accounting. This is not needed
// for correctness, but ensures that mappings of those pages will be maintained
// if all references obtained using IncRef are dropped. The reference must be
// released using RemoveMapping when no longer needed.
//
// AddMapping may be called on pages without an existing reference as long as
// f.MappableRelease() has not been called.
//
// Preconditions: As for memmap.Mappable.AddMapping.
func (f *MmapCachedFile) AddMapping(ar hostarch.AddrRange, offset uint64) {
	f.refsMu.Lock()
	defer f.refsMu.Unlock()
	f.incRefUnaccounted(memmap.FileRange{offset, offset + uint64(ar.Length())})
}

// Preconditions:
//   - fr.Length() != 0.
//   - fr.Start and fr.End must be page-aligned.
//
// +checklocks:f.refsMu
func (f *MmapCachedFile) incRefUnaccounted(fr memmap.FileRange) {
	if f.refs == nil {
		f.refs = make(map[uint64]int64)
	}
	chunkStart := fr.Start &^ chunkMask()
	for {
		refs := f.refs[chunkStart]
		pgs := pagesInChunk(fr, chunkStart)
		if refs+pgs < refs {
			// Would overflow.
			panic(fmt.Sprintf("fsutil.MmapCachedFile.IncRef(%v): adding %d page references to chunk %#x, which has %d page references", fr, pgs, chunkStart, refs))
		}
		f.refs[chunkStart] = refs + pgs
		chunkStart += chunkSize()
		if chunkStart >= fr.End || chunkStart == 0 {
			break
		}
	}
}

// DecRef implements memmap.File.DecRef.
func (f *MmapCachedFile) DecRef(fr memmap.FileRange) {
	f.refsMu.Lock()
	defer f.refsMu.Unlock()
	f.decRefUnaccounted(fr)
	f.memAcct.DecRefAndAccount(fr)
}

// RemoveMapping is called by implementations of memmap.Mappable.RemoveMapping
// to release page references acquired by AddMapping.
//
// Preconditions: As for memmap.Mappable.RemoveMapping.
func (f *MmapCachedFile) RemoveMapping(ar hostarch.AddrRange, offset uint64) {
	f.refsMu.Lock()
	defer f.refsMu.Unlock()
	f.decRefUnaccounted(memmap.FileRange{offset, offset + uint64(ar.Length())})
}

// Preconditions:
//   - fr.Length() != 0.
//   - fr.Start and fr.End must be page-aligned.
//
// +checklocks:f.refsMu
func (f *MmapCachedFile) decRefUnaccounted(fr memmap.FileRange) {
	chunkStart := fr.Start &^ chunkMask()
	for {
		refs := f.refs[chunkStart]
		pgs := pagesInChunk(fr, chunkStart)
		switch {
		case refs > pgs:
			f.refs[chunkStart] = refs - pgs
		case refs == pgs:
			delete(f.refs, chunkStart)
			// TODO: Unmapping here generally means that mappings of host FDs
			// will be unmapped when there are no application VMAs mapping f,
			// e.g. between process executions, which may be expensive for some
			// workloads. Consider retaining mappings until f.close() instead.
			// This would also let us drop Add/RemoveMapping.
			f.mapsMu.Lock()
			if m, ok := f.mappings[chunkStart]; ok {
				f.unmapAndRemove(chunkStart, m)
			}
			f.mapsMu.Unlock()
		case refs < pgs:
			panic(fmt.Sprintf("fsutil.MmapCachedFile.DecRef(%v): removing %d page references from chunk %#x, which has %d page references", fr, pgs, chunkStart, refs))
		}
		chunkStart += chunkSize()
		if chunkStart >= fr.End || chunkStart == 0 {
			break
		}
	}
	if f.mappableReleased && len(f.refs) == 0 {
		f.close()
	}
}

// Preconditions:
//   - f.mappings[chunkStart] == m.
//
// +checklocks:f.mapsMu
func (f *MmapCachedFile) unmapAndRemove(chunkStart uint64, m mapping) {
	if _, _, errno := unix.Syscall(unix.SYS_MUNMAP, m.addr, uintptr(chunkSize()), 0); errno != 0 {
		// This leaks address space and is unexpected, but is otherwise
		// harmless, so complain but don't panic.
		log.Warningf("fsutil.MmapCachedFile: failed to unmap mapping %#x for chunk %#x: %v", m.addr, chunkStart, errno)
	}
	delete(f.mappings, chunkStart)
	cachedChunks.Add(-1)
}

// MapInternal implements memmap.File.MapInternal.
func (f *MmapCachedFile) MapInternal(fr memmap.FileRange, at hostarch.AccessType) (safemem.BlockSeq, error) {
	// A descriptor that the budget may close is pinned to map chunks;
	// chunks mapped already need none, and stay mapped while the caller
	// holds references to their pages.
	if f.mayEvict.Load() && !f.chunksMapped(fr, at.Write) {
		if _, err := f.pinFD(); err != nil {
			return safemem.BlockSeq{}, err
		}
		defer f.fdMu.RUnlock()
	}
	chunks := ((fr.End + chunkMask()) >> chunkShift()) - (fr.Start >> chunkShift())
	f.mapsMu.Lock()
	defer f.mapsMu.Unlock()
	if chunks == 1 {
		// Avoid an unnecessary slice allocation.
		var seq safemem.BlockSeq
		err := f.forEachMappingBlockLocked(fr, at.Write, func(b safemem.Block) {
			seq = safemem.BlockSeqOf(b)
		})
		return seq, err
	}
	blocks := make([]safemem.Block, 0, chunks)
	err := f.forEachMappingBlockLocked(fr, at.Write, func(b safemem.Block) {
		blocks = append(blocks, b)
	})
	return safemem.BlockSeqFromSlice(blocks), err
}

// chunksMapped returns whether every chunk of fr is mapped, for writing if
// write is set.
func (f *MmapCachedFile) chunksMapped(fr memmap.FileRange, write bool) bool {
	f.mapsMu.Lock()
	defer f.mapsMu.Unlock()
	for chunkStart := fr.Start &^ chunkMask(); chunkStart < fr.End; chunkStart += chunkSize() {
		m, ok := f.mappings[chunkStart]
		if !ok || write && !m.writable {
			return false
		}
		if chunkStart+chunkSize() == 0 {
			break
		}
	}
	return true
}

// +checklocks:f.mapsMu
func (f *MmapCachedFile) forEachMappingBlockLocked(fr memmap.FileRange, write bool, fn func(safemem.Block)) error {
	fd := f.fd.Load()
	prot := unix.PROT_READ
	if write {
		prot |= unix.PROT_WRITE
	}
	chunkStart := fr.Start &^ chunkMask()
	if f.mappings == nil {
		f.mappings = make(map[uint64]mapping)
	}
	for {
		m, ok := f.mappings[chunkStart]
		if (!ok || write && !m.writable) && fd < 0 {
			return unix.EBADF
		}
		if !ok && cachedChunks.Load() >= chunkMappingBudget() {
			return memmap.BufferedIOFallbackErr{}
		}
		if !ok {
			addr, _, errno := unix.Syscall6(
				unix.SYS_MMAP,
				0,
				uintptr(chunkSize()),
				uintptr(prot),
				unix.MAP_SHARED,
				uintptr(fd),
				uintptr(chunkStart))
			if errno != 0 {
				return errno
			}
			cachedChunks.Add(1)
			m = mapping{addr, write}
			f.mappings[chunkStart] = m
		} else if write && !m.writable {
			addr, _, errno := unix.Syscall6(
				unix.SYS_MMAP,
				m.addr,
				uintptr(chunkSize()),
				uintptr(prot),
				unix.MAP_SHARED|unix.MAP_FIXED,
				uintptr(fd),
				uintptr(chunkStart))
			if errno != 0 {
				return errno
			}
			m = mapping{addr, write}
			f.mappings[chunkStart] = m
		}
		var startOff uint64
		if chunkStart < fr.Start {
			startOff = fr.Start - chunkStart
		}
		endOff := chunkSize()
		if chunkStart+chunkSize() > fr.End {
			endOff = fr.End - chunkStart
		}
		fn(unsafeBlockFromMapping(m.addr, int(chunkSize())).TakeFirst64(endOff).DropFirst64(startOff))
		chunkStart += chunkSize()
		if chunkStart >= fr.End || chunkStart == 0 {
			break
		}
	}
	return nil
}

// RegenerateMappings must be called when the file description mapped by f
// changes, to replace existing mappings of the previous file description.
//
// Preconditions:
//   - f.MappableRelease() is not called concurrently with, or before,
//     f.RegenerateMappings().
func (f *MmapCachedFile) RegenerateMappings() error {
	fd := f.fd.Load()
	if fd < 0 {
		// No previous call to f.MapInternal() can have succeeded.
		return nil
	}

	f.mapsMu.Lock()
	defer f.mapsMu.Unlock()

	for chunkStart, m := range f.mappings {
		prot := unix.PROT_READ
		if m.writable {
			prot |= unix.PROT_WRITE
		}
		_, _, errno := unix.Syscall6(
			unix.SYS_MMAP,
			m.addr,
			uintptr(chunkSize()),
			uintptr(prot),
			unix.MAP_SHARED|unix.MAP_FIXED,
			uintptr(fd),
			uintptr(chunkStart))
		if errno != 0 {
			return errno
		}
	}
	return nil
}

// DataFD implements memmap.File.DataFD. A descriptor that the budget may
// close (see SetReopen) is valid only until it does: callers that map it
// pin it instead (see memmap.PinDataFD).
func (f *MmapCachedFile) DataFD(fr memmap.FileRange) (int, error) {
	fd, err := f.pinFD()
	if err != nil {
		return -1, err
	}
	f.fdMu.RUnlock()
	return fd, nil
}

// BufferReadAt implements memmap.File.BufferReadAt, for chunks that
// MapInternal does not map (see chunkMappingBudget). It reads what a mapping
// of the file would: zeroes past the file's end in the page that holds it,
// and a fault past that page.
func (f *MmapCachedFile) BufferReadAt(off uint64, dst []byte) (uint64, error) {
	fd, err := f.pinFD()
	if err != nil {
		return 0, err
	}
	defer f.fdMu.RUnlock()
	done := uint64(0)
	for done < uint64(len(dst)) {
		n, err := unix.Pread(fd, dst[done:], int64(off+done))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return done, err
		}
		if n == 0 {
			end := min(uint64(len(dst)), pageRoundUp(off+done)-off)
			clear(dst[done:end])
			if done = end; done < uint64(len(dst)) {
				return done, unix.EFAULT
			}
			break
		}
		done += uint64(n)
	}
	return done, nil
}

// BufferWriteAt implements memmap.File.BufferWriteAt, for chunks that
// MapInternal does not map (see chunkMappingBudget). It writes what a mapping
// of the file would: nothing past the file's end, which it does not extend,
// and a fault past the page that holds it.
func (f *MmapCachedFile) BufferWriteAt(off uint64, src []byte) (uint64, error) {
	fd, err := f.pinFD()
	if err != nil {
		return 0, err
	}
	defer f.fdMu.RUnlock()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return 0, err
	}
	size := uint64(stat.Size)
	length := uint64(len(src))
	if mapped := pageRoundUp(size); off+length > mapped {
		length = mapped - min(off, mapped)
		err = unix.EFAULT
	}
	written := min(length, size-min(off, size))
	for done := uint64(0); done < written; {
		n, werr := unix.Pwrite(fd, src[done:written], int64(off+done))
		if werr == unix.EINTR {
			continue
		}
		if werr != nil {
			return done, werr
		}
		done += uint64(n)
	}
	return length, err
}

// pageRoundUp returns off rounded up to a page boundary.
func pageRoundUp(off uint64) uint64 {
	return (off + hostarch.PageSize - 1) &^ (hostarch.PageSize - 1)
}

// FD implements memmap.File.FD.
func (f *MmapCachedFile) FD() int {
	return int(f.fd.Load())
}
