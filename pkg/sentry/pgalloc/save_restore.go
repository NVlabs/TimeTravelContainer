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

package pgalloc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"runtime"
	"slices"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/bitmap"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/gohacks"
	"gvisor.dev/gvisor/pkg/goid"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/ringdeque"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/state/wire"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/syncevent"
	"gvisor.dev/gvisor/pkg/timing"
)

// MarkSavable marks f as savable.
func (f *MemoryFile) MarkSavable() {
	f.savable.Store(true)
}

// IsSavable returns true if f is savable.
func (f *MemoryFile) IsSavable() bool {
	return f.savable.Load()
}

// ResourceID returns the resource ID for f.
func (f *MemoryFile) ResourceID() checkpoint.ResourceID {
	return f.opts.ResourceID
}

// memoryFileSaved is the subset of MemoryFile that is stored in checkpoints.
//
// +stateify savable
type memoryFileSaved struct {
	unwasteSmall     *unwasteSet
	unwasteHuge      *unwasteSet
	unfreeSmall      *unfreeSet
	unfreeHuge       *unfreeSet
	logicalAllocator LogicalAllocatorID
	nextLogicalPage  uint64
	nextPageVersion  uint64
	logicalPages     *logicalPageSet
	subreleased      map[uint64]uint64
	memAcct          *memAcctSet
	chunks           []chunkInfo
}

// hugeSequenceBit marks, in the FirstSequence that a logicalPageInfo saves, a
// segment of 2 MiB pages. logicalPageInfo saves the fields it saved before 2
// MiB pages existed, so that images saved then still load, as base pages; no
// sequence reaches the bit.
const hugeSequenceBit = 1 << 63

func (i *logicalPageInfo) saveFirstSequence() uint64 {
	if i.Huge {
		return i.FirstSequence | hugeSequenceBit
	}
	return i.FirstSequence
}

func (i *logicalPageInfo) loadFirstSequence(_ context.Context, seq uint64) {
	i.FirstSequence, i.Huge = seq&^hugeSequenceBit, seq&hugeSequenceBit != 0
}

// SaveOpts provides options to MemoryFile.SaveTo().
type SaveOpts struct {
	// If PagesFile is not nil, then page contents will be written to PagesFile
	// rather than to w.
	PagesFile *AsyncPagesFileSave

	// If ExcludeCommittedZeroPages is true, SaveTo() will scan both committed
	// and possibly-committed pages to find zero pages, whose contents are
	// saved implicitly rather than explicitly to reduce checkpoint size. If
	// ExcludeCommittedZeroPages is false, SaveTo() will scan only
	// possibly-committed pages to find zero pages.
	//
	// Enabling ExcludeCommittedZeroPages will usually increase the time taken
	// by SaveTo() (due to the larger number of pages that must be scanned),
	// but may instead improve SaveTo() and LoadFrom() time, and checkpoint
	// size, if the application has many committed zero pages.
	ExcludeCommittedZeroPages bool

	// If ExitAfterSaving is true, the sentry exits soon after SaveTo()
	// returns, so SaveTo() may leave zero pages that are already committed on
	// the host in place rather than decommit them.
	ExitAfterSaving bool
}

// SaveTo writes f's state to the given stream.
func (f *MemoryFile) SaveTo(ctx context.Context, w io.Writer, opts *SaveOpts) error {
	if err := f.AwaitLoadAll(); err != nil {
		return fmt.Errorf("previous async page loading failed: %w", err)
	}

	// Wait for memory release.
	f.mu.Lock()
	defer f.mu.Unlock()
	for f.haveWaste {
		f.mu.Unlock()
		runtime.Gosched()
		f.mu.Lock()
	}
	// The saved state holds the pages of home chunks in the sets, as those of
	// other chunks.
	defer f.exportHomePagesLocked()()

	// Ensure that there are no pending evictions.
	if len(f.evictable) != 0 {
		panic(fmt.Sprintf("evictions still pending for %d users; call StartEvictions and WaitForEvictions before SaveTo", len(f.evictable)))
	}

	// Register this MemoryFile with async page saving if a pages file has been
	// provided.
	var amfs *asyncMemoryFileSave
	if opts.PagesFile != nil {
		var sf stateio.SourceFile
		if opts.PagesFile.aw.NeedRegisterSourceFD() && f.file != nil {
			fileSize := uint64(len(f.chunksLoad())) * chunkSize
			var err error
			sf, err = opts.PagesFile.aw.RegisterSourceFD(int32(f.file.Fd()), fileSize, f.getClientFileRangeSettings(fileSize))
			if err != nil {
				return fmt.Errorf("failed to register MemoryFile with pages file: %w", err)
			}
		}
		amfs = &asyncMemoryFileSave{
			f:  f,
			pf: opts.PagesFile,
			sf: sf,
		}
	}

	// We only want to explicitly include pages containing non-zero bytes in
	// the checkpoint, to reduce the size of the checkpoint and improve restore
	// performance. Scan pages for non-zero bytes and ensure that they are
	// marked known-committed.
	//
	// If async page saving is enabled, emit writes to the pages file during
	// scanning, which is feasible since the pages metadata file and pages file
	// are distinct. If async page saving is disabled, we can't do this since
	// pages would be written to the state file before the metadata needed to
	// determine where to load them to. (We could instead emit one memAcct
	// segment at a time, immediately before the contents of the pages it
	// represents. However, each call to state.Save() writes some overhead
	// (header and type information), which would bloat the state file and slow
	// down loading when the number of memAcct segments is large.)
	timeScanStart := gohacks.Nanotime()
	var (
		allocatedBytes          uint64
		alreadyCommittedBytes   uint64
		newCommittedBytes       uint64
		alreadyUncommittedBytes uint64
		newUncommittedBytes     uint64
		queuedPageBytes         uint64
	)
	asyncWritePages := func(fr memmap.FileRange) {}
	if amfs != nil {
		asyncWritePages = func(fr memmap.FileRange) {
			amount := fr.Length()
			queuedPageBytes += amount
			amfs.pf.mu.Lock()
			amfs.pf.unsaved.PushBack(apsRange{
				amfs:      amfs,
				FileRange: fr,
			})
			amfs.pf.saveOff += amount
			amfs.pf.mu.Unlock()
			amfs.pf.stStatus.Notify(apsSTPending)
		}
	}

	// Reading an uncommitted page to determine if it is zero-filled will cause
	// it to become committed. Thus, we need to decommit zero-filled pages that
	// were not previously known to be committed to avoid increasing memory
	// usage during scanning. Batch decommitment of transiently-committed pages
	// to reduce host syscalls.
	var (
		decommitWarnOnce  sync.Once
		decommitPendingFR memmap.FileRange
		decommitCount     uint64
	)
	decommitNow := func(fr memmap.FileRange) {
		decommitCount++
		if err := f.decommitFile(fr); err != nil {
			// This doesn't impact the correctness of saved memory, it just
			// means that we're incrementally more likely to OOM. Complain, but
			// don't abort saving.
			decommitWarnOnce.Do(func() {
				log.Warningf("Decommitting MemoryFile offsets %v while saving failed: %v", fr, err)
			})
		}
	}
	decommitAddPage := func(off uint64) {
		// Invariants:
		// (1) All of decommitPendingFR lies within a single huge page.
		// (2) decommitPendingFR.End is hugepage-aligned iff
		// decommitPendingFR.Length() == 0.
		end := off + uint64(hostarch.PageSize)
		if decommitPendingFR.End == off {
			// Merge with the existing range. By invariants, the page {off,
			// end} must be within the same huge page as the rest of
			// decommitPendingFR.
			decommitPendingFR.End = end
		} else {
			// Decommit the existing range and start a new one.
			if decommitPendingFR.Length() != 0 {
				decommitNow(decommitPendingFR)
			}
			decommitPendingFR = memmap.FileRange{off, end}
		}
		// Maintain invariants by decommitting if we've reached the end of the
		// containing huge page.
		if hostarch.IsHugePageAligned(end) {
			decommitNow(decommitPendingFR)
			decommitPendingFR = memmap.FileRange{}
		}
	}

	// Batch updates to f.memAcct to reduce set mutation overhead.
	//
	// Invariant: If updatePendingFR.Length() != 0, all of updatePendingFR
	// corresponds to a single segment in f.memAcct.
	var (
		updatePendingFR           memmap.FileRange
		updatePendingWasCommitted bool
		updatePendingNowCommitted bool
	)
	updateNow := func(maseg memAcctIterator, fr memmap.FileRange, wasCommitted, nowCommitted bool) memAcctIterator {
		amount := fr.Length()
		if amount == 0 {
			return maseg
		}
		if wasCommitted != nowCommitted {
			maseg = f.memAcct.Isolate(maseg, fr)
			ma := maseg.ValuePtr()
			if nowCommitted {
				ma.knownCommitted = true
				ma.commitSeq = 0
				f.knownCommittedBytes += amount
				if !f.opts.DisableMemoryAccounting {
					usage.MemoryAccounting.Inc(amount, ma.kind, ma.memCgID)
				}
			} else {
				ma.knownCommitted = false
				ma.commitSeq = f.commitSeq
				f.knownCommittedBytes -= amount
				if !f.opts.DisableMemoryAccounting {
					usage.MemoryAccounting.Dec(amount, ma.kind, ma.memCgID)
				}
			}
			maseg = f.memAcct.Unisolate(maseg)
		}
		if nowCommitted {
			asyncWritePages(fr)
		}
		return maseg
	}
	// Precondition: maseg.Range().IsSupersetOf(fr).
	// Postcondition: The returned memAcctIterator.Range().IsSupersetOf(fr).
	updateAddRange := func(maseg memAcctIterator, fr memmap.FileRange, wasCommitted, nowCommitted bool) memAcctIterator {
		if updatePendingFR.End == fr.Start && updatePendingWasCommitted == wasCommitted && updatePendingNowCommitted == nowCommitted {
			updatePendingFR.End = fr.End
			return maseg
		}
		if updatePendingFR.Length() != 0 {
			maseg = updateNow(maseg, updatePendingFR, updatePendingWasCommitted, updatePendingNowCommitted)
			if maseg.End() == fr.Start {
				maseg = maseg.NextSegment()
			}
		}
		updatePendingFR = fr
		updatePendingWasCommitted = wasCommitted
		updatePendingNowCommitted = nowCommitted
		return maseg
	}
	updateFlush := func(maseg memAcctIterator) memAcctIterator {
		if updatePendingFR.Length() != 0 {
			maseg = updateNow(maseg, updatePendingFR, updatePendingWasCommitted, updatePendingNowCommitted)
		}
		updatePendingFR = memmap.FileRange{}
		return maseg
	}

	zeroPage := make([]byte, hostarch.PageSize)
	// f.mu is unlocked below, allowing concurrent calls to f.UpdateUsage() to
	// observe pages that we transiently commit (for comparisons to zero) or
	// leave committed (if opts.ExcludeCommittedZeroPages is true). Set
	// f.isSaving to prevent this.
	if f.isSaving {
		panic(fmt.Sprintf("pgalloc.MemoryFile(%p).SaveTo() called concurrently", f))
	}
	f.isSaving = true
	defer func() { f.isSaving = false }()
	// An f.updateUsageLocked() that is still in progress skips merging the
	// segments it split in f.memAcct, since we hold an iterator into it
	// across f.mu unlocks below.
	f.memAcct.MergeAll()
	// Reset f.commitSeq and memAcctInfo.commitSeq to 0 to make more segments
	// in f.memAcct mergeable. This is safe because f.updateUsageLocked() is
	// excluded by f.isSaving being set to true.
	f.commitSeq = 0
	maseg := f.memAcct.FirstSegment()
	unscannedStart := uint64(0)
	for maseg.Ok() {
		ma := maseg.ValuePtr()
		if ma.wasteOrReleasing {
			// This shouldn't be possible since we waited for memory release
			// above, and f isn't mutated during saving.
			panic(fmt.Sprintf("found waste or releasing pages %v during pgalloc.MemoryFile.SaveTo()", maseg.Range()))
		}
		fr := maseg.Range()
		if fr.Start < unscannedStart {
			fr.Start = unscannedStart
		}
		unscannedStart = fr.End
		allocatedBytes += fr.Length()
		ma.commitSeq = 0
		wasCommitted := ma.knownCommitted
		if !opts.ExcludeCommittedZeroPages && wasCommitted {
			alreadyCommittedBytes += fr.Length()
			maseg = updateAddRange(maseg, fr, true /* wasCommitted */, true /* nowCommitted */)
			maseg = updateFlush(maseg)
			if maseg.End() == unscannedStart {
				maseg = maseg.NextSegment()
			}
			continue
		}
		scanPage := func(pg []byte, off uint64) {
			isZeroed := bytes.Equal(pg, zeroPage)
			if isZeroed {
				if !wasCommitted {
					alreadyUncommittedBytes += hostarch.PageSize
					// A platform backing is the coherent data plane for every live
					// TTC sibling. Accounting-uncommitted does not imply that its
					// resident frame is transient or unused by another branch, so a
					// single-branch checkpoint must not decommit it.
					if f.backing == nil {
						decommitAddPage(off)
					}
				} else {
					newUncommittedBytes += hostarch.PageSize
				}
			} else {
				if !wasCommitted {
					newCommittedBytes += hostarch.PageSize
				} else {
					alreadyCommittedBytes += hostarch.PageSize
				}
			}
			maseg = updateAddRange(maseg, memmap.FileRange{off, off + hostarch.PageSize}, wasCommitted, !isZeroed)
		}
		scanRange := func(scanRange memmap.FileRange) error {
			scanOffset := scanRange.Start
			return f.withMappingSlices(scanRange, hostarch.Read, func(bs []byte) error {
				for pgoff := 0; pgoff < len(bs); pgoff += hostarch.PageSize {
					off := scanOffset + uint64(pgoff)
					scanPage(bs[pgoff:pgoff+hostarch.PageSize], off)
				}
				scanOffset += uint64(len(bs))
				return nil
			})
		}
		if f.backing != nil && !wasCommitted {
			// Query physical commitment before mapping. ResolveFrames allocates
			// missing GuestRAM frames, so mapping the whole range would turn a
			// sparse zero allocation into resident memory just to omit it again.
			committed := make([]byte, fr.Length()/hostarch.PageSize)
			if err := f.backing.QueryCommitment(fr, committed); err != nil {
				return fmt.Errorf("querying platform commitment while saving %v: %w", fr, err)
			}
			for page := 0; page < len(committed); {
				off := fr.Start + uint64(page)*hostarch.PageSize
				if committed[page] == 0 {
					scanPage(zeroPage, off)
					page++
					continue
				}
				end := page + 1
				for end < len(committed) && committed[end] != 0 {
					end++
				}
				committedRange := memmap.FileRange{
					Start: off,
					End:   fr.Start + uint64(end)*hostarch.PageSize,
				}
				if err := scanRange(committedRange); err != nil {
					return fmt.Errorf("mapping committed platform pages for save: %w", err)
				}
				page = end
			}
		} else if err := scanRange(fr); err != nil {
			return fmt.Errorf("mapping MemoryFile pages for save: %w", err)
		}
		// Keep f.mu held across the scan. TTC live-fork kernels share one
		// MemoryFile, so quiescing the kernel being checkpointed does not stop
		// sibling branches from allocating or releasing ranges. Temporarily
		// dropping f.mu here allowed a sibling to invalidate maseg and corrupt
		// memAcct during SaveTo. UpdateUsage and allocator mutations may wait
		// for the checkpoint scan; this is a cold-path family-wide memory
		// rendezvous, while page contents remain copy-on-write coherent.
		// We need to flush batched updates to f.memAcct whenever potentially
		// reaching the end of a segment, in order to maintain the invariant
		// that updatePendingFR corresponds to a single segment.
		maseg = updateFlush(maseg)
		// maseg might already include offsets beyond unscannedStart if it was
		// merged with a successor segment.
		if maseg.End() == unscannedStart {
			maseg = maseg.NextSegment()
		}
	}
	if decommitPendingFR.Length() != 0 {
		decommitNow(decommitPendingFR)
		decommitPendingFR = memmap.FileRange{}
	}

	durScan := time.Duration(gohacks.Nanotime() - timeScanStart)
	log.Infof("MemoryFile(%p): save scanning took %s for %d allocated bytes: %d bytes of zero pages (%d bytes expected, decommitted in %d syscalls, + %d bytes new), %d bytes of non-zero pages (%d bytes expected + %d bytes new); ExcludeCommittedZeroPages=%v ExitAfterSaving=%v",
		f,
		durScan,
		allocatedBytes,
		alreadyUncommittedBytes+newUncommittedBytes,
		alreadyUncommittedBytes,
		decommitCount,
		newUncommittedBytes,
		alreadyCommittedBytes+newCommittedBytes,
		alreadyCommittedBytes,
		newCommittedBytes,
		opts.ExcludeCommittedZeroPages,
		opts.ExitAfterSaving)

	// Save metadata.
	timeMetadataStart := gohacks.Nanotime()
	if amfs != nil && queuedPageBytes != f.knownCommittedBytes {
		// MemoryFiles share a pages file, so a mismatch shifts the
		// PagesFileOffset used to restore every subsequent MemoryFile.
		return fmt.Errorf("async page save queued %d bytes, but metadata describes %d bytes", queuedPageBytes, f.knownCommittedBytes)
	}
	if _, err := state.Save(ctx, w, &memoryFileSaved{
		unwasteSmall:     &f.unwasteSmall,
		unwasteHuge:      &f.unwasteHuge,
		unfreeSmall:      &f.unfreeSmall,
		unfreeHuge:       &f.unfreeHuge,
		logicalAllocator: f.logicalAllocator,
		nextLogicalPage:  f.nextLogicalPage,
		nextPageVersion:  f.nextPageVersion,
		logicalPages:     &f.logicalPages,
		subreleased:      f.subreleased,
		memAcct:          &f.memAcct,
		chunks:           f.chunksLoad(),
	}); err != nil {
		return fmt.Errorf("failed to save metadata: %w", err)
	}
	log.Infof("MemoryFile(%p): saved metadata in %s", f, time.Duration(gohacks.Nanotime()-timeMetadataStart))

	if amfs == nil {
		// Save committed pages.
		ww := wire.Writer{Writer: w}
		timePagesStart := gohacks.Nanotime()
		bytesSaved := uint64(0)
		for maseg := f.memAcct.FirstSegment(); maseg.Ok(); maseg = maseg.NextSegment() {
			if !maseg.ValuePtr().knownCommitted {
				continue
			}
			maFR := maseg.Range()
			// Write a header to distinguish from objects.
			if err := state.WriteHeader(&ww, maFR.Length(), false); err != nil {
				return err
			}
			// Write out data.
			if err := f.withMappingSlices(maFR, hostarch.Read, func(s []byte) error {
				_, err := w.Write(s)
				return err
			}); err != nil {
				return err
			}
			bytesSaved += maFR.Length()
		}
		durPages := time.Duration(gohacks.Nanotime() - timePagesStart)
		log.Infof("MemoryFile(%p): saved pages in %s (%d bytes, %.3f MB/s)", f, durPages, bytesSaved, float64(bytesSaved)*1e-6/durPages.Seconds())
	}

	return nil
}

// AsyncPagesFileSave holds async page saving state for a single pages file.
type AsyncPagesFileSave struct {
	mu apfsMutex

	// saveOff is the offset in the pages file at which the next page to be
	// inserted into unsaved will be saved. saveOff is protected by mu.
	saveOff uint64

	// unsaved tracks pages that have not been saved. unsaved is protected by mu.
	unsaved ringdeque.Deque[apsRange]

	// stStatus communicates state from MemoryFile.SaveTo() and its callers to
	// the goroutine.
	stStatus syncevent.Waiter

	// Padding before fields used mostly by the async page saver goroutine:
	_ [hostarch.CacheLineSize]byte

	// bytesSaved is the number of write-completed bytes. bytesSaved is
	// protected by statsMu.
	bytesSaved uint64

	// When async page saving is enabled, MemoryFile.SaveTo() enqueues writes
	// while scanning for zero pages, and skips writing zero pages.
	// Consequently, if MemoryFile.SaveTo() finds a large number of zero pages
	// in close proximity, unsaved may become empty, causing writing to stall
	// and observed write throughput to drop. To differentiate this case from
	// write throughput being limited by I/O limitations, separately record and
	// report throughput for when the I/O queue is full (qavail == 0).
	//
	// bytesFull is the number of write-completed bytes while the I/O queue was
	// full. timeFullStart was the value of gohacks.Nanotime() when the I/O
	// queue last became full; if it is currently not full, timeFullStart is
	// MaxInt64. durFull is the duration elapsed while the I/O queue was full,
	// not counting time elapsed since timeFullStart. These fields are
	// protected by statsMu.
	bytesFull     uint64
	timeFullStart int64
	durFull       time.Duration

	// Following fields are exclusive to the async page saver goroutine.

	doneCallback func(error) // immutable

	// aw is the pages file. aw is immutable.
	aw stateio.AsyncWriter

	// maxWriteBytes is hostarch.PageRoundDown(ar.MaxWriteBytes()), cached to
	// avoid interface method calls and recomputation. maxWriteBytes is
	// immutable.
	maxWriteBytes uint64

	// qavail is unused capacity in aw.
	qavail int

	// Pages file writes are always contiguous, even when the corresponding
	// memmap.FileRanges and mappings are not. If curOp is not nil, it is the
	// current apsOp under construction, and curOpID is its index into ops.
	curOp   *apsOp
	curOpID uint32

	// opsBusy tracks which apsOps in ops are in use (correspond to
	// inflight operations or curOp).
	opsBusy bitmap.Bitmap

	// ops stores all apsOps.
	ops []apsOp

	// If err is not nil, it is the error that has terminated async page
	// saving. Note that unlike AsyncPagesFileLoad.err(),
	// AsyncPagesFileSave.err is never returned to application syscalls and
	// hence is not constrained to being an errno.
	err error
}

// Possible events in AsyncPagesFileSave.stStatus:
const (
	apsSTPending syncevent.Set = 1 << iota
	apsSTDone
)

// asyncMemoryFileSave holds state for async page saving from a single
// MemoryFile.
type asyncMemoryFileSave struct {
	// Immutable fields:
	f  *MemoryFile
	pf *AsyncPagesFileSave
	sf stateio.SourceFile
}

type apsRange struct {
	amfs *asyncMemoryFileSave
	memmap.FileRange
}

// apsOp tracks async page save state corresponding to a single AIO write
// operation.
type apsOp struct {
	// total is the number of bytes to be written by the operation.
	total uint64

	// amfs represents the MemoryFile being saved.
	amfs *asyncMemoryFileSave

	// frs are the MemoryFile ranges being saved.
	frs []memmap.FileRange

	// iovecs contains mappings of frs.
	iovecs []unix.Iovec

	// leases keep relocatable platform-backed mappings referenced by iovecs
	// stable until the asynchronous write completes.
	leases []memmap.InternalMappingLease
}

// StartAsyncPagesFileSave constructs asynchronous saving state for the pages
// file aw. It takes ownership of aw, even if it returns a non-nil error.
func StartAsyncPagesFileSave(aw stateio.AsyncWriter, doneCallback func(error)) (*AsyncPagesFileSave, error) {
	maxWriteBytes := hostarch.PageRoundDown(aw.MaxWriteBytes())
	if maxWriteBytes <= 0 {
		aw.Close()
		return nil, fmt.Errorf("stateio.AsyncWriter.MaxWriteBytes() (%d) must be at least one page)", aw.MaxWriteBytes())
	}
	// Cap maxParallel due to the uint32 range of bitmap.Bitmap.
	maxParallel := min(aw.MaxParallel(), 1<<30)
	apfs := &AsyncPagesFileSave{
		timeFullStart: math.MaxInt64,
		doneCallback:  doneCallback,
		aw:            aw,
		maxWriteBytes: maxWriteBytes,
		qavail:        maxParallel,
		opsBusy:       bitmap.New(uint32(maxParallel)),
		ops:           make([]apsOp, maxParallel),
	}
	// Mark ops in opsBusy that don't actually exist as permanently busy.
	for i, n := maxParallel, apfs.opsBusy.Size(); i < n; i++ {
		apfs.opsBusy.Add(uint32(i))
	}
	// Pre-allocate slices in ops.
	maxRanges := aw.MaxRanges()
	for i := range apfs.ops {
		op := &apfs.ops[i]
		op.frs = make([]memmap.FileRange, 0, maxRanges)
		op.iovecs = make([]unix.Iovec, 0, maxRanges)
		op.leases = make([]memmap.InternalMappingLease, 0, maxRanges)
	}
	apfs.stStatus.Init()
	go apfs.main()
	return apfs, nil
}

// MemoryFilesDone must be called after calling SaveTo() for all MemoryFiles
// saving to apfs. MemoryFilesDone may be called multiple times; subsequent
// calls have no effect.
func (apfs *AsyncPagesFileSave) MemoryFilesDone() {
	apfs.stStatus.Notify(apsSTDone)
}

// PagesFileOffset returns the offset into the pages file of the next page to
// be written.
func (apfs *AsyncPagesFileSave) PagesFileOffset() uint64 {
	return apfs.saveOff
}

func (apfs *AsyncPagesFileSave) canEnqueue() bool {
	return apfs.qavail > 0
}

// Preconditions: apfs.canEnqueue() == true.
func (apfs *AsyncPagesFileSave) enqueueCurOp() {
	if apfs.qavail <= 0 {
		panic("queue full")
	}
	op := apfs.curOp
	if op.total == 0 {
		panic("invalid write of 0 bytes")
	}
	if op.total > apfs.maxWriteBytes {
		panic(fmt.Sprintf("write of %d bytes exceeds per-write limit of %d bytes", op.total, apfs.maxWriteBytes))
	}

	apfs.qavail--
	apfs.curOp = nil
	if len(op.frs) == 1 && len(op.iovecs) == 1 {
		// Perform a non-vectorized write to save an indirection (and possible
		// userspace-to-kernelspace copy) in the AsyncWriter implementation.
		apfs.aw.AddWrite(int(apfs.curOpID), op.amfs.sf, op.frs[0], stateio.SliceFromIovec(op.iovecs[0]))
	} else {
		apfs.aw.AddWritev(int(apfs.curOpID), op.total, op.amfs.sf, op.frs, op.iovecs)
	}
}

// Preconditions:
// - apfs.canEnqueue() == true.
// - mfr.Length() > 0.
// - mfr must be page-aligned.
func (apfs *AsyncPagesFileSave) enqueueRange(mfr apsRange) (uint64, error) {
	for {
		if apfs.curOp == nil {
			id, err := apfs.opsBusy.FirstZero(0)
			if err != nil {
				panic(fmt.Sprintf("all ops busy with qavail=%d: %v", apfs.qavail, err))
			}
			apfs.opsBusy.Add(id)
			op := &apfs.ops[id]
			op.total = 0
			op.frs = op.frs[:0]
			op.iovecs = op.iovecs[:0]
			op.leases = op.leases[:0]
			apfs.curOp = op
			apfs.curOpID = id
		}
		n, err := apfs.combine(mfr)
		if err == errBackingBusy {
			// Submit the current op so that its leases are released when it
			// completes. combine only reports busy while writes are queued or
			// op holds leases, so the main loop then waits for queued writes,
			// and maps without leases once they have completed.
			if apfs.curOp.total != 0 {
				apfs.enqueueCurOp()
			} else {
				apfs.opsBusy.Remove(apfs.curOpID)
				apfs.curOp = nil
			}
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if n > 0 {
			return n, nil
		}
		// Flush the existing (conflicting) op and try again with a new one.
		apfs.enqueueCurOp()
		if !apfs.canEnqueue() {
			return 0, nil
		}
	}
}

// combine adds as much of the given save as possible to apfs.curOp and returns
// the number of bytes added.
//
// Preconditions:
// - mfr.Length() > 0.
// - mfr must be page-aligned.
func (apfs *AsyncPagesFileSave) combine(mfr apsRange) (uint64, error) {
	op := apfs.curOp
	if op.total != 0 {
		if op.amfs != mfr.amfs {
			// Differing MemoryFile.
			return 0, nil
		}
		if len(op.frs) == cap(op.frs) && op.frs[len(op.frs)-1].End != mfr.Start {
			// Non-contiguous in the MemoryFile, and we're out of space for
			// FileRanges.
			return 0, nil
		}
	}

	// Apply direct length limits.
	n := mfr.Length()
	if op.total+n >= apfs.maxWriteBytes {
		n = apfs.maxWriteBytes - op.total
	}
	if n == 0 {
		return 0, nil
	}
	mfr.End = mfr.Start + n

	// Collect iovecs, which may further limit length.
	n = 0
	mapInternal := mfr.amfs.f.MapInternalWithLease
	if apfs.qavail != len(apfs.ops) || len(op.leases) != 0 {
		// This goroutine holds leases until their writes complete, so it
		// must not wait: an exclusive backing operation such as a compaction
		// waits for the held leases, and both new leases and async page
		// loads wait behind it.
		mapInternal = mfr.amfs.f.tryMapInternalWithLease
	}
	blocks, mappingLease, err := mapInternal(mfr.FileRange, hostarch.Read)
	if err != nil {
		return 0, err
	}
	for !blocks.IsEmpty() {
		block := blocks.Head()
		if block.NeedSafecopy() {
			if mappingLease != nil {
				mappingLease.Release()
			}
			return 0, fmt.Errorf("async platform MemoryFile mapping unexpectedly requires safecopy")
		}
		bs := block.ToSlice()
		if len(op.iovecs) > 0 {
			if canMergeIovecAndSlice(op.iovecs[len(op.iovecs)-1], bs) {
				op.iovecs[len(op.iovecs)-1].Len += uint64(len(bs))
				n += uint64(len(bs))
				blocks = blocks.Tail()
				continue
			}
			if len(op.iovecs) == cap(op.iovecs) {
				break
			}
		}
		op.iovecs = append(op.iovecs, unix.Iovec{
			Base: &bs[0],
			Len:  uint64(len(bs)),
		})
		n += uint64(len(bs))
		blocks = blocks.Tail()
	}
	if n == 0 {
		if mappingLease != nil {
			mappingLease.Release()
		}
		return 0, nil
	}
	if mappingLease != nil {
		op.leases = append(op.leases, mappingLease)
	}
	mfr.End = mfr.Start + n

	// With the length decided, finish updating op.
	if op.total == 0 {
		op.amfs = mfr.amfs
	}
	op.total += n
	if len(op.frs) > 0 && op.frs[len(op.frs)-1].End == mfr.Start {
		op.frs[len(op.frs)-1].End = mfr.End
	} else {
		op.frs = append(op.frs, mfr.FileRange)
	}
	return n, nil
}

func releaseAPSOpLeases(op *apsOp) {
	for _, lease := range op.leases {
		lease.Release()
	}
	op.leases = op.leases[:0]
}

func (apfs *AsyncPagesFileSave) main() {
	defer func() {
		for i := range apfs.ops {
			releaseAPSOpLeases(&apfs.ops[i])
		}
		if apfs.err != nil {
			log.Warningf("Async page saving failed: %v", apfs.err)
		}
		if err := apfs.aw.Close(); err != nil {
			// Saving success is independent of err, so log it rather than
			// propagating it.
			log.Warningf("Async page saving: stateio.AsyncWriter.Close failed: %v", err)
		}
		if apfs.doneCallback != nil {
			apfs.doneCallback(apfs.err)
		}
	}()

	maxParallel := apfs.aw.MaxParallel()
	// Storage reused between main loop iterations:
	var completions []stateio.Completion

	// Don't start timing until we have pages to save.
	apfs.stStatus.Wait()
	timeStart := gohacks.Nanotime()
	if log.IsLogging(log.Debug) {
		log.Debugf("Async page saving started")
		progressTicker := time.NewTicker(5 * time.Second)
		progressStopC := make(chan struct{})
		defer func() { close(progressStopC) }()
		go func() {
			timeLast := timeStart
			bytesSavedLast := uint64(0)
			for {
				select {
				case <-progressStopC:
					progressTicker.Stop()
					return
				case <-progressTicker.C:
					// Take a snapshot of our progress.
					apfs.mu.Lock()
					bytesSaved := apfs.bytesSaved
					bytesFull := apfs.bytesFull
					timeFullStart := apfs.timeFullStart
					durFull := apfs.durFull
					apfs.mu.Unlock()
					now := gohacks.Nanotime()
					durTotal := time.Duration(now - timeStart)
					if timeFullStart < now {
						durFull += time.Duration(now - timeFullStart)
					}
					durDelta := time.Duration(now - timeLast)
					bytesSavedDelta := bytesSaved - bytesSavedLast
					bandwidthTotal := float64(bytesSaved) * 1e-6 / durTotal.Seconds()
					bandwidthFull := float64(bytesFull) * 1e-6 / durFull.Seconds()
					bandwidthSinceLast := float64(bytesSavedDelta) * 1e-6 / durDelta.Seconds()
					log.Infof("Async page saving in progress for %s (%d bytes, %.3f MB/s); queue full for %s (%d bytes, %.3f MB/s); since last update %s ago: %d bytes, %.3f MB/s",
						durTotal.Round(time.Millisecond),
						bytesSaved,
						bandwidthTotal,
						durFull.Round(time.Millisecond),
						bytesFull,
						bandwidthFull,
						durDelta.Round(time.Millisecond),
						bytesSavedDelta,
						bandwidthSinceLast)
					timeLast = now
					bytesSavedLast = bytesSaved
				}
			}
		}()
	}

	prevFull := false
	for {
		// Enqueue as many writes as possible.
		if !apfs.canEnqueue() {
			panic("main loop invariant failed")
		}
		apfs.mu.Lock()
		apfs.aw.Reserve(apfs.saveOff)
		for !apfs.unsaved.Empty() {
			mfr := apfs.unsaved.PeekFrontPtr()
			n, err := apfs.enqueueRange(*mfr)
			if err != nil {
				apfs.mu.Unlock()
				apfs.err = fmt.Errorf("mapping pages for asynchronous save: %w", err)
				return
			}
			if n == mfr.Length() {
				apfs.unsaved.RemoveFront()
			} else if n != 0 {
				mfr.Start += n
			} else {
				break
			}
			if !apfs.canEnqueue() {
				break
			}
		}
		full := apfs.qavail == 0
		if full && !prevFull {
			apfs.timeFullStart = gohacks.Nanotime()
		} else if !full && prevFull {
			apfs.durFull += time.Duration(gohacks.Nanotime() - apfs.timeFullStart)
			apfs.timeFullStart = math.MaxInt64
		}
		prevFull = full
		apfs.mu.Unlock()
		// Don't flush pending op unless it's the last one; this differs from
		// async page loading since writers are likely to be more sensitive to
		// write size than readers are to read size, and saving is less
		// latency-sensitive that loading.
		if apfs.curOp != nil && apfs.stStatus.Pending()&apsSTDone != 0 {
			apfs.enqueueCurOp()
		}

		if apfs.qavail == maxParallel {
			// We are out of work to do.
			ev := apfs.stStatus.Wait()
			if ev&apsSTPending != 0 {
				// We may have raced with MemoryFile.SaveTo() inserting into
				// apfs.unsaved.
				apfs.stStatus.Ack(apsSTPending)
				continue
			}
			if ev&apsSTDone != 0 {
				if apfs.curOp != nil {
					// Enqueue and complete the final write before finalizing.
					continue
				}
				if err := apfs.aw.Finalize(); err != nil {
					apfs.err = fmt.Errorf("stateio.AsyncWriter.Finalize failed: %w", err)
					return
				}
				// Successfully completed all saving for all MemoryFiles.
				durTotal := time.Duration(gohacks.Nanotime() - timeStart)
				apfs.mu.Lock()
				bytesTotal := apfs.bytesSaved
				bytesFull := apfs.bytesFull
				durFull := apfs.durFull
				apfs.mu.Unlock()
				bandwidthTotal := float64(bytesTotal) * 1e-6 / durTotal.Seconds()
				bandwidthFull := float64(bytesFull) * 1e-6 / durFull.Seconds()
				log.Infof("Async page saving completed in %s (%d bytes, %.3f MB/s); queue full for %s (%d bytes, %.3f MB/s)",
					durTotal.Round(time.Millisecond),
					bytesTotal,
					bandwidthTotal,
					durFull.Round(time.Millisecond),
					bytesFull,
					bandwidthFull)
				return
			}
			panic(fmt.Sprintf("unknown events in stStatus: %#x", ev))
		}

		// Wait for any number of writes to complete.
		var err error
		completions, err = apfs.aw.Wait(completions[:0], 1 /* minCompletions */)
		if err != nil {
			apfs.err = fmt.Errorf("stateio.AsyncWriter.Wait failed: %w", err)
			return
		}

		// Process completions.
		apfs.mu.Lock()
		for _, c := range completions {
			op := &apfs.ops[c.ID]
			releaseAPSOpLeases(op)
			apfs.opsBusy.Remove(uint32(c.ID))
			apfs.qavail++
			if c.Err != nil {
				apfs.mu.Unlock()
				apfs.err = fmt.Errorf("write for MemoryFile(%p) pages %v failed: %w", op.amfs.f, op.frs, c.Err)
				return
			}
			if c.N != op.total {
				apfs.mu.Unlock()
				apfs.err = fmt.Errorf("write for MemoryFile(%p) pages %v (total %d bytes) returned %d bytes", op.amfs.f, op.frs, op.total, c.N)
				return
			}
			apfs.bytesSaved += op.total
			if full {
				apfs.bytesFull += op.total
			}
		}
		apfs.mu.Unlock()
	}
}

// LoadOpts provides options to MemoryFile.LoadFrom().
type LoadOpts struct {
	// If PagesFile is not nil, then page contents will be read from PagesFile,
	// starting at offset PagesFileOffset, rather than from r. After LoadFrom
	// returns, PagesFileOffset will be updated to the offset of the first byte
	// in PagesFile after this MemoryFile's contents.
	//
	// Reading from PagesFile may continue after LoadFrom returns. If
	// DoneCallback is not nil, it will be called when reading for this
	// MemoryFile completes. DoneCallback will be called whether or not
	// LoadFrom returns a non-nil error.
	//
	// Invariant: PagesFileOffset must be page-aligned.
	PagesFile       *AsyncPagesFileLoad
	PagesFileOffset uint64
	DoneCallback    func(error)

	// Optional timeline for the restore process.
	// If async page loading is enabled, a forked timeline will be created, so
	// ownership of this timeline remains in the hands of the caller of
	// LoadFrom.
	Timeline *timing.Timeline
}

// LoadFrom loads MemoryFile state from the given stream.
func (f *MemoryFile) LoadFrom(ctx context.Context, r io.Reader, opts *LoadOpts) (err error) {
	mfTimeline := opts.Timeline.Fork(fmt.Sprintf("mf:%p", f)).Lease()
	defer mfTimeline.End()

	defer func() {
		if opts.DoneCallback != nil {
			opts.DoneCallback(err)
		}
	}()

	// Load metadata.
	timeMetadataStart := gohacks.Nanotime()
	var mfs memoryFileSaved
	if _, err := state.Load(ctx, r, &mfs); err != nil {
		return fmt.Errorf("failed to load metadata: %w", err)
	}
	f.unwasteSmall.MoveFrom(mfs.unwasteSmall)
	f.unwasteHuge.MoveFrom(mfs.unwasteHuge)
	f.unfreeSmall.MoveFrom(mfs.unfreeSmall)
	f.unfreeHuge.MoveFrom(mfs.unfreeHuge)
	if mfs.logicalAllocator != (LogicalAllocatorID{}) {
		f.logicalAllocator = mfs.logicalAllocator
		f.nextLogicalPage = mfs.nextLogicalPage
		f.nextPageVersion = mfs.nextPageVersion
	}
	if mfs.logicalPages != nil {
		f.logicalPages.MoveFrom(mfs.logicalPages)
	}
	f.ensureLogicalPagesLocked()
	f.subreleased = mfs.subreleased
	f.memAcct.MoveFrom(mfs.memAcct)
	chunks := mfs.chunks
	f.chunks.Store(&chunks)
	f.mu.Lock()
	f.loadHomesLocked()
	f.recountAccountedLocked()
	f.mu.Unlock()
	mfTimeline.Reached("metadata loaded")
	log.Infof("MemoryFile(%p): loaded metadata in %s", f, time.Duration(gohacks.Nanotime()-timeMetadataStart))

	fileSize := uint64(len(chunks)) * chunkSize
	// Obtain chunk mappings, then madvise them concurrently with loading data.
	var (
		madviseEnd  atomicbitops.Uint64
		madviseChan = make(chan struct{}, 1)
		madviseWG   sync.WaitGroup
	)
	if f.backing != nil {
		for i := range chunks {
			oldSize := uint64(i) * chunkSize
			mappings, err := f.backing.Grow(oldSize, oldSize+chunkSize, chunks[i].huge)
			if err != nil {
				return fmt.Errorf("growing platform MemoryFile backing for chunk %d: %w", i, err)
			}
			if len(mappings) != 1 {
				return fmt.Errorf("platform MemoryFile backing returned %d mappings for chunk %d, want 1", len(mappings), i)
			}
			chunks[i].mapping = mappings[0]
		}
		// Platform mappings don't require host madvise. Individual committed
		// ranges are materialized below before checkpoint data is copied in.
		madviseEnd.Store(fileSize)
	} else if len(chunks) != 0 {
		if err := f.file.Truncate(int64(fileSize)); err != nil {
			return fmt.Errorf("failed to truncate MemoryFile: %w", err)
		}
		m, err := f.mapChunksLocked(0, fileSize)
		if err != nil {
			return fmt.Errorf("failed to mmap MemoryFile: %w", err)
		}
		mfTimeline.Reached("mmaped chunks")
		for i := range chunks {
			chunk := &chunks[i]
			chunk.mapping = m
			m += chunkSize
		}
		madviseWG.Add(1)
		go func() {
			defer madviseWG.Done()
			for i := range chunks {
				chunk := &chunks[i]
				f.adviseChunk(chunk.mapping, chunk.huge, chunk.node)
				madviseEnd.Add(chunkSize)
				select {
				case madviseChan <- struct{}{}:
				default:
				}
			}
		}()
	}
	defer madviseWG.Wait()

	// Register this MemoryFile with async page loading if a pages file has
	// been provided.
	var amfl *asyncMemoryFileLoad
	if opts.PagesFile != nil {
		var df stateio.DestinationFile
		if opts.PagesFile.ar.NeedRegisterDestinationFD() {
			if f.backing != nil {
				return fmt.Errorf("pages reader requires a host destination FD for platform-backed MemoryFile")
			}
			var err error
			df, err = opts.PagesFile.ar.RegisterDestinationFD(int32(f.file.Fd()), fileSize, f.getClientFileRangeSettings(fileSize))
			if err != nil {
				return fmt.Errorf("failed to register MemoryFile with pages file: %w", err)
			}
		}
		amfl = &asyncMemoryFileLoad{
			f:            f,
			pf:           opts.PagesFile,
			df:           df,
			doneCallback: opts.DoneCallback,
			timeline:     mfTimeline.Transfer(),
		}
		amfl.pf.amflsMu.Lock()
		if err := amfl.pf.err(); err != nil {
			amfl.pf.amflsMu.Unlock()
			return err
		}
		amfl.pf.amfls.PushBack(amfl)
		amfl.pf.amflsMu.Unlock()
		f.asyncPageLoad.Store(amfl)
		opts.DoneCallback = nil
		defer func() {
			amfl.pf.amflsMu.Lock()
			defer amfl.pf.amflsMu.Unlock()
			amfl.pf.mu.Lock()
			defer amfl.pf.mu.Unlock()
			amfl.lfDone = true
			if amfl.unloaded.IsEmpty() {
				// The async page loader goroutine does this when it
				// transitions amfl.unloaded from non-empty to empty with
				// amfl.lfDone == true, but since amfl.unloaded is already
				// empty (async page loading for this MemoryFile finished
				// before we got here, possibly because the MemoryFile was
				// empty), we have to do so instead.
				amfl.minUnloaded.Store(math.MaxUint64)
				amfl.pf.amfls.Remove(amfl)
				amfl.f.asyncPageLoad.Store(nil)
				amfl.timeline.End()
				if amfl.doneCallback != nil {
					amfl.doneCallback(nil)
					amfl.doneCallback = nil
				}
			}
		}()
	}

	// Load committed pages and reconstruct memory accounting state.
	wr := wire.Reader{Reader: r}
	timePagesStart := gohacks.Nanotime()
	minUnloadedInit := false
	for maseg := f.memAcct.FirstSegment(); maseg.Ok(); maseg = maseg.NextSegment() {
		if !maseg.ValuePtr().knownCommitted {
			continue
		}
		maFR := maseg.Range()
		amount := maFR.Length()
		if f.backing != nil {
			if err := f.backing.Commit(maFR); err != nil {
				return fmt.Errorf("committing restored platform MemoryFile pages %v: %w", maFR, err)
			}
		}
		// Wait for all chunks spanned by this segment to be madvised.
		for madviseEnd.Load() < maFR.End {
			<-madviseChan
		}
		if amfl != nil {
			// Record where to read data.
			if !minUnloadedInit {
				minUnloadedInit = true
				amfl.minUnloaded.Store(maFR.Start)
			}
			amfl.pf.mu.Lock()
			amfl.unloaded.InsertRange(maFR, aplUnloadedInfo{
				off: opts.PagesFileOffset,
			})
			amfl.pf.mu.Unlock()
			opts.PagesFileOffset += amount
			amfl.pf.lfStatus.Notify(aplLFPending)
		} else {
			// Verify header.
			length, object, err := state.ReadHeader(&wr)
			if err != nil {
				return fmt.Errorf("failed to read header: %w", err)
			}
			if object {
				// Not expected.
				return fmt.Errorf("unexpected object")
			}
			if length != amount {
				// Size mismatch.
				return fmt.Errorf("mismatched segment: expected %d, got %d", amount, length)
			}
			// Read data.
			ioErr := f.withMappingSlicesNoWait(maFR, hostarch.Write, func(s []byte) error {
				_, err := io.ReadFull(r, s)
				return err
			})
			if ioErr != nil {
				return fmt.Errorf("failed to read pages: %w", ioErr)
			}
		}

		// Update accounting for restored pages. We need to do this here since
		// these segments are marked as "known committed", and will be skipped
		// over on accounting scans.
		f.knownCommittedBytes += amount
		if !f.opts.DisableMemoryAccounting {
			usage.MemoryAccounting.Inc(amount, maseg.ValuePtr().kind, maseg.ValuePtr().memCgID)
		}
	}
	durPages := time.Duration(gohacks.Nanotime() - timePagesStart)
	if amfl != nil {
		log.Infof("MemoryFile(%p): loaded page file offsets in %s; async loading %d bytes", f, durPages, f.knownCommittedBytes)
	} else {
		log.Infof("MemoryFile(%p): loaded pages in %s (%d bytes, %.3f MB/s)", f, durPages, f.knownCommittedBytes, float64(f.knownCommittedBytes)*1e-6/durPages.Seconds())
	}

	// Home chunks hold their pages' state in their tables from now on.
	f.mu.Lock()
	f.importHomePagesLocked()
	f.mu.Unlock()
	return nil
}

// AsyncPagesFileLoad holds async page loading state for a single pages file.
type AsyncPagesFileLoad struct {
	// owners keeps the asynchronous reader alive while either a Kernel or a
	// source-side RemoteSnapshot may still fault retained pages through it.
	// Page references alone are insufficient: an unloaded page remains
	// allocated after capture, but loses its contents if the last reader owner
	// cancels the load.
	owners atomic.Int64

	mu apflMutex

	// If errVal is not nil, it is an error that has terminated asynchronous
	// page loading. errVal can only be set by the async page loader goroutine,
	// and can only transition from nil to non-nil once, after which it is
	// immutable. errVal is stored with mu locked.
	errVal atomic.Value

	// errDetail retains the causal asynchronous-reader error for diagnostics.
	// errVal intentionally remains a Linux EIO for guest-facing fault paths,
	// which cannot safely expose arbitrary transport errors. Stats, however,
	// must preserve the cause so a failed remote migration can be diagnosed and
	// fenced instead of appearing as an unexplained "I/O error".
	errDetail atomic.Pointer[string]

	// demandOnly is true while unawaited pages must not be loaded. It may
	// transition from true to false exactly once when Drain is called.
	demandOnly atomic.Bool

	// durableBacking is true after the volatile reader has been replaced by
	// an immutable snapshot-graph reader. Multiple TTC branches can share one
	// sparse residency unit and therefore the same loader; this bit lets every
	// owner observe that the shared loader no longer depends on its original
	// source node.
	durableBacking atomic.Bool
	drainOnce      sync.Once

	// logicalChunkDemand makes every fault wait for all extant pages in the
	// same stable logical 1 MiB chunk. The source stream must place each
	// chunk's pages contiguously, and the reader must support one full chunk
	// and up to one destination range per base page in a single operation.
	logicalChunkDemand bool
	// rangeLoaded is called after a successful read has been copied into all
	// destination MemoryFile ranges and those ranges have left unloaded state.
	// It must not block the loader goroutine.
	rangeLoaded func(streamOffset, length uint64)

	// done is closed after the loader goroutine has stopped and ar has been
	// closed.
	done chan struct{}

	// priority contains possibly-unstarted ranges with at least one waiter.
	// priority is protected by mu.
	priority ringdeque.Deque[aplFileRange]

	// prefetch contains advisory ranges following awaited ranges. Unlike
	// priority, these ranges hold temporary references while I/O is in flight.
	// prefetch is protected by mu and bounded to avoid untrusted fault patterns
	// growing metadata without limit.
	prefetch ringdeque.Deque[aplFileRange]

	// prefetchAheadBytes and prefetchBehindBytes define the current advisory
	// window around a fault. They are mutable so a policy controller may switch
	// strategy without reconstructing the target.
	prefetchAheadBytes  atomic.Uint64
	prefetchBehindBytes atomic.Uint64

	// If throttled is true, a read of pages without waiters starts only when
	// no other read is in flight.
	throttled atomicbitops.Bool

	// lfStatus communicates MemoryFile.LoadFrom() state to the async page
	// loader goroutine.
	lfStatus syncevent.Waiter

	// Padding before state used mostly by the async page loader goroutine:
	_ [hostarch.CacheLineSize]byte

	// amfls tracks MemoryFiles that are currently loading from the pages file.
	// amfls is protected by amflsMu.
	amflsMu amflsMutex
	amfls   asyncMemoryFileLoadList

	// numWaiters is the current number of waiting waiters. numWaiters is
	// protected by mu.
	numWaiters int

	// totalWaiters is the number of waiters that have ever waited for pages
	// from this pages file. totalWaiters is protected by mu.
	totalWaiters     int
	faults           uint64
	sequentialFaults uint64
	lastFaultEnd     uint64
	haveLastFault    bool

	// timeStartWaiters was the value of gohacks.Nanotime() when numWaiters
	// most recently transitioned from 0 to 1. If numWaiters is 0,
	// timeStartWaiters is MaxInt64. timeStartWaiters is protected by mu.
	timeStartWaiters int64

	// nsWaitedOne is the duration for which at least one waiter was waiting
	// for a load. nsWaitedTotal is the duration for which waiters were waiting
	// for loads, summed across all waiters. bytesWaited is the number of bytes
	// for which at least one waiter waited. These fields are protected by mu.
	durWaitedOne   time.Duration
	durWaitedTotal time.Duration
	bytesWaited    uint64

	// bytesLoaded is the number of bytes that have been loaded so far.
	// bytesLoaded is protected by mu.
	bytesLoaded uint64

	// Following fields are exclusive to the async page loader goroutine.

	timeline     *timing.Timeline // immutable
	doneCallback func(error)      // immutable

	// ar is the pages file. ar is immutable.
	ar stateio.AsyncReader

	// maxReadBytes is hostarch.PageRoundDown(ar.MaxReadBytes()), cached to
	// avoid interface method calls and recomputation. maxReadBytes is
	// immutable.
	maxReadBytes               uint64
	maxRanges                  int
	maxParallel                int
	needsRegisterDestinationFD bool

	// readerSwitchMu serializes public reader handoffs. readerSwitchRequestMu
	// protects readerSwitchRequest, which is consumed only by main after all
	// reads submitted to the previous reader have completed.
	readerSwitchMu        sync.Mutex
	readerSwitchRequestMu sync.Mutex
	readerSwitchRequest   *asyncPagesReaderSwitch

	// qavail is unused capacity in ar.
	qavail int

	// The async page loader combines multiple loads with contiguous pages file
	// offsets (the common case) into a single read, even if their
	// corresponding memmap.FileRanges and mappings are discontiguous. If curOp
	// is not nil, it is the current aplOp under construction, and curOpID is
	// its index into ops.
	curOp   *aplOp
	curOpID uint32

	// busyBacking is the MemoryFile whose backing refused a mapping lease in
	// the current main loop iteration, or nil. Reads hold leases until they
	// complete, and an exclusive backing operation such as a compaction waits
	// for them, so the main loop must not wait for a lease itself.
	busyBacking *MemoryFile

	// opsBusy tracks which aplOps in ops are in use (correspond to
	// inflight operations or curOp).
	opsBusy bitmap.Bitmap

	// ops stores all aplOps.
	ops []aplOp
}

// Possible events in AsyncPagesFileLoad.lfStatus:
const (
	aplLFPending syncevent.Set = 1 << iota
	aplLFDone
	aplLFCancel
	aplLFSwitchReader
)

type asyncPagesReaderSwitch struct {
	reader stateio.AsyncReader
	done   chan error
}

// AsyncPagesReaderGeometry is the immutable queue shape required by a reader
// handed to AsyncPagesFileLoad.SwitchReader.
type AsyncPagesReaderGeometry struct {
	MaxReadBytes               uint64
	MaxRanges                  int
	MaxParallel                int
	NeedsRegisterDestinationFD bool
}

type aplFileRange struct {
	amfl *asyncMemoryFileLoad
	memmap.FileRange
}

func (apfl *AsyncPagesFileLoad) err() error {
	if v := apfl.errVal.Load(); v != nil {
		return v.(error)
	}
	return nil
}

// asyncMemoryFileLoad holds async page loading state for a single MemoryFile.
type asyncMemoryFileLoad struct {
	// Immutable fields:
	f             *MemoryFile
	pf            *AsyncPagesFileLoad
	df            stateio.DestinationFile
	doneCallback  func(error)
	timeline      *timing.Timeline
	sparse        bool
	logicalChunks []*logicalChunkLoad

	// minUnloaded is the MemoryFile offset of the first unloaded byte.
	minUnloaded atomicbitops.Uint64

	// unloaded tracks pages in the MemoryFile that have not been loaded.
	// unloaded is protected by pf.mu.
	unloaded aplUnloadedSet

	// lfDone is true if MemoryFile.LoadFrom() has finished inserting into
	// unloaded. lfDone is protected by pf.mu.
	lfDone bool

	// asyncMemoryFileLoadEntry links into pf.amfls. asyncMemoryFileLoadEntry
	// is protected by pf.amflsMu.
	asyncMemoryFileLoadEntry

	// Padding before state exclusive to the async page loader goroutine:
	_ [hostarch.CacheLineSize]byte

	// minUnstarted is the lowest offset that may map to a segment in unloaded
	// for which aplUnloadedInfo.started == false.
	minUnstarted uint64
}

// aplUnloadedInfo is the value type of asyncMemoryFileLoad.unloaded.
type aplUnloadedInfo struct {
	// off is the offset into the pages file at which the represented pages
	// begin.
	off uint64

	// started is true if a read has been enqueued for these pages.
	started bool

	// waiters queues goroutines waiting for these pages to be loaded.
	waiters []*aplWaiter

	// logicalChunk is non-nil when this range participates in an atomic
	// logical-chunk demand load.
	logicalChunk *logicalChunkLoad

	// unit, if not zero, is the sparse residency unit the range belongs to.
	// A sparse load makes every unit a demand touches resident at once, so
	// ranges of different units must not merge, however contiguous: merged,
	// the first fault of a restored branch whose units lay in stream order
	// loaded every unit of it.
	unit uint64

	// placed is true if the range is bound to a memory node already; see
	// MemoryFile.PlaceUnloaded.
	placed bool
}

type logicalChunkLoad struct {
	id       LogicalChunkID
	instance uint32
	ranges   []RemoteLoadRange
}

type aplWaiter struct {
	// wakeup is used by a caller of MemoryFile.awaitLoad() to block until all
	// pages in fr are loaded. wakeup is internally synchronized. fr is
	// immutable after initialization.
	wakeup syncevent.Waiter
	fr     memmap.FileRange

	// timeStart was the value of gohacks.Nanotime() when this waiter started
	// waiting. timeStart is immutable after initialization.
	timeStart int64

	// pending is the number of unloaded bytes that this waiter is waiting for.
	// pending is protected by aplShared.mu.
	pending uint64
}

var aplWaiterPool = sync.Pool{
	New: func() any {
		var w aplWaiter
		w.wakeup.Init()
		return &w
	},
}

// aplOp tracks async page load state corresponding to a single AIO read
// operation.
type aplOp struct {
	// total is the number of bytes to be read by the operation.
	total uint64

	// end is the pages file offset at which the read ends.
	end uint64

	// amfl represents the MemoryFile being loaded.
	amfl *asyncMemoryFileLoad

	// frs are the MemoryFile ranges being loaded.
	frs []memmap.FileRange

	// iovecs contains mappings of frs.
	iovecs []unix.Iovec

	// leases keep relocatable platform-backed mappings referenced by iovecs
	// stable until the asynchronous read completes.
	leases []memmap.InternalMappingLease

	// If tempRef is true, a temporary reference is held on pages in frs that
	// should be dropped after completion.
	tempRef bool

	// logicalChunk prevents one operation from straddling logical residency
	// units. nil is used by ordinary checkpoint and page-granular loaders.
	logicalChunk *logicalChunkLoad
}

func (op *aplOp) off() int64 {
	return int64(op.end - op.total)
}

// AsyncPagesFileLoadOpts controls asynchronous page loading.
type AsyncPagesFileLoadOpts struct {
	// DemandOnly prevents speculative background loading. Only ranges needed
	// by a current waiter are read until Drain is called.
	DemandOnly bool

	// DurableBacking declares that ar reads immutable durable storage rather
	// than a live source runtime. Most remote loaders become durable later via
	// SwitchReader, but graph-native cold restores start in this state. This is
	// lifecycle authority, not an I/O-success assertion: terminal reader errors
	// remain observable through Stats.
	DurableBacking bool

	// PrefetchBytes speculatively loads up to this many page-aligned bytes
	// immediately following a demanded range. It is effective only while
	// DemandOnly is true; Drain already loads every remaining page.
	PrefetchBytes uint64

	// PrefetchBehindBytes speculatively loads up to this many page-aligned
	// bytes immediately preceding a demanded range. Combining this with
	// PrefetchBytes implements a fixed window around a fault.
	PrefetchBehindBytes uint64

	// LogicalChunkDemand requires each demand read to install all extant pages
	// in the same stable logical 1 MiB chunk atomically.
	LogicalChunkDemand bool

	// RangeLoaded observes successfully installed pages-file ranges. It is
	// used by TTC sparse backing reclamation; failures must be handled by the
	// observer and do not invalidate resident memory.
	RangeLoaded func(streamOffset, length uint64)

	// If Throttled is true, reads of pages without waiters are throttled
	// until AsyncPagesFileLoad.Unthrottle() is called.
	Throttled bool
}

// AsyncPagesFileLoadStats is a point-in-time view of a remote or checkpoint
// page loader. It contains no transport credentials or process-local state.
type AsyncPagesFileLoadStats struct {
	Done                  bool          `json:"done"`
	DemandOnly            bool          `json:"demand_only"`
	BytesLoaded           uint64        `json:"bytes_loaded"`
	BytesOutstanding      uint64        `json:"bytes_outstanding"`
	BytesWaited           uint64        `json:"bytes_waited"`
	WaitDuration          time.Duration `json:"wait_duration"`
	AggregateWaitDuration time.Duration `json:"aggregate_wait_duration"`
	PrefetchBytes         uint64        `json:"prefetch_bytes"`
	PrefetchBehindBytes   uint64        `json:"prefetch_behind_bytes"`
	Waiters               int           `json:"waiters"`
	TotalWaiters          int           `json:"total_waiters"`
	Faults                uint64        `json:"faults"`
	SequentialFaults      uint64        `json:"sequential_faults"`
	Error                 string        `json:"error,omitempty"`
}

// Stats returns loader progress without waiting for outstanding I/O.
func (apfl *AsyncPagesFileLoad) Stats() AsyncPagesFileLoadStats {
	if apfl == nil {
		return AsyncPagesFileLoadStats{Done: true}
	}
	stats := AsyncPagesFileLoadStats{DemandOnly: apfl.demandOnly.Load()}
	select {
	case <-apfl.done:
		stats.Done = true
	default:
	}
	apfl.amflsMu.Lock()
	apfl.mu.Lock()
	stats.BytesLoaded = apfl.bytesLoaded
	stats.BytesWaited = apfl.bytesWaited
	stats.WaitDuration = apfl.durWaitedOne
	stats.AggregateWaitDuration = apfl.durWaitedTotal
	stats.PrefetchBytes = apfl.prefetchAheadBytes.Load()
	stats.PrefetchBehindBytes = apfl.prefetchBehindBytes.Load()
	stats.Waiters = apfl.numWaiters
	stats.TotalWaiters = apfl.totalWaiters
	stats.Faults = apfl.faults
	stats.SequentialFaults = apfl.sequentialFaults
	for amfl := apfl.amfls.Front(); amfl != nil; amfl = amfl.Next() {
		for seg := amfl.unloaded.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
			stats.BytesOutstanding += seg.Range().Length()
		}
	}
	apfl.mu.Unlock()
	apfl.amflsMu.Unlock()
	if err := apfl.err(); err != nil {
		if detail := apfl.errDetail.Load(); detail != nil {
			stats.Error = *detail
		} else {
			stats.Error = err.Error()
		}
	}
	return stats
}

// StartAsyncPagesFileLoad constructs asynchronous loading state for the pages
// file ar. It takes ownership of ar, even if it returns a non-nil error.
func StartAsyncPagesFileLoad(ar stateio.AsyncReader, doneCallback func(error), timeline *timing.Timeline, opts AsyncPagesFileLoadOpts) (*AsyncPagesFileLoad, error) {
	return StartAsyncPagesFileLoadWithOpts(ar, doneCallback, timeline, opts)
}

// StartAsyncPagesFileLoadWithOpts is StartAsyncPagesFileLoad.
func StartAsyncPagesFileLoadWithOpts(ar stateio.AsyncReader, doneCallback func(error), timeline *timing.Timeline, opts AsyncPagesFileLoadOpts) (*AsyncPagesFileLoad, error) {
	maxReadBytes := hostarch.PageRoundDown(ar.MaxReadBytes())
	if maxReadBytes <= 0 {
		ar.Close()
		return nil, fmt.Errorf("stateio.AsyncReader.MaxReadBytes() (%d) must be at least one page)", ar.MaxReadBytes())
	}
	if !hostarch.IsPageAligned(opts.PrefetchBytes) || !hostarch.IsPageAligned(opts.PrefetchBehindBytes) {
		ar.Close()
		return nil, fmt.Errorf("async page prefetch window behind=%d ahead=%d is not page-aligned", opts.PrefetchBehindBytes, opts.PrefetchBytes)
	}
	if opts.LogicalChunkDemand && (maxReadBytes < SparseChunkBytes || ar.MaxRanges() < logicalPagesPerChunk) {
		ar.Close()
		return nil, fmt.Errorf("logical chunk demand requires reader geometry of at least %d bytes and %d ranges, got %d and %d", SparseChunkBytes, logicalPagesPerChunk, maxReadBytes, ar.MaxRanges())
	}
	// Cap maxParallel due to the uint32 range of bitmap.Bitmap.
	maxParallel := min(ar.MaxParallel(), 1<<30)
	apfl := &AsyncPagesFileLoad{
		timeStartWaiters:           math.MaxInt64,
		timeline:                   timeline.Fork("async page loading"),
		doneCallback:               doneCallback,
		ar:                         ar,
		maxReadBytes:               maxReadBytes,
		maxRanges:                  ar.MaxRanges(),
		maxParallel:                maxParallel,
		needsRegisterDestinationFD: ar.NeedRegisterDestinationFD(),
		logicalChunkDemand:         opts.LogicalChunkDemand,
		rangeLoaded:                opts.RangeLoaded,
		throttled:                  atomicbitops.FromBool(opts.Throttled),
		qavail:                     maxParallel,
		opsBusy:                    bitmap.New(uint32(maxParallel)),
		ops:                        make([]aplOp, maxParallel),
		done:                       make(chan struct{}),
	}
	apfl.owners.Store(1)
	apfl.durableBacking.Store(opts.DurableBacking)
	apfl.prefetchAheadBytes.Store(opts.PrefetchBytes)
	apfl.prefetchBehindBytes.Store(opts.PrefetchBehindBytes)
	apfl.demandOnly.Store(opts.DemandOnly)
	// Mark ops in opsBusy that don't actually exist as permanently busy.
	for i, n := maxParallel, apfl.opsBusy.Size(); i < n; i++ {
		apfl.opsBusy.Add(uint32(i))
	}
	// Pre-allocate slices in ops.
	maxRanges := ar.MaxRanges()
	for i := range apfl.ops {
		op := &apfl.ops[i]
		op.frs = make([]memmap.FileRange, 0, maxRanges)
		op.iovecs = make([]unix.Iovec, 0, maxRanges)
		op.leases = make([]memmap.InternalMappingLease, 0, maxRanges)
	}
	apfl.lfStatus.Init()
	go apfl.main()
	return apfl, nil
}

// IncOwner retains apfl's asynchronous reader. The caller must already own a
// reference, which prevents resurrection after the final owner has canceled
// the load.
func (apfl *AsyncPagesFileLoad) IncOwner() {
	if !apfl.tryIncOwner() {
		panic("retaining released asynchronous page load")
	}
}

// Unthrottle allows apfl to read pages without waiters at full parallelism.
// It is idempotent.
func (apfl *AsyncPagesFileLoad) Unthrottle() {
	if apfl.throttled.Swap(false) {
		apfl.lfStatus.Notify(aplLFPending)
	}
}

// tryIncOwner retains apfl's asynchronous reader, unless its last owner has
// already released it.
func (apfl *AsyncPagesFileLoad) tryIncOwner() bool {
	for {
		owners := apfl.owners.Load()
		if owners <= 0 {
			return false
		}
		if apfl.owners.CompareAndSwap(owners, owners+1) {
			return true
		}
	}
}

// DecOwner releases one asynchronous-reader owner. The last release cancels
// any unresolved reads and waits until the reader has closed.
func (apfl *AsyncPagesFileLoad) DecOwner() error {
	owners := apfl.owners.Add(-1)
	if owners < 0 {
		panic("negative asynchronous page load owner count")
	}
	if owners != 0 {
		return nil
	}
	apfl.Cancel()
	return apfl.Wait()
}

// SwitchReader atomically replaces the backing page source without loading
// the remaining pages. Reads already submitted to the old source complete
// first; subsequent reads use replacement. This is used to hand a distributed
// target from a live source Sentry to an immutable shared-filesystem image.
// SwitchReader takes ownership of replacement even when it returns an error.
func (apfl *AsyncPagesFileLoad) SwitchReader(replacement stateio.AsyncReader) error {
	if replacement == nil {
		return errors.New("replacement async page reader is nil")
	}
	if hostarch.PageRoundDown(replacement.MaxReadBytes()) != apfl.maxReadBytes ||
		replacement.MaxRanges() != apfl.maxRanges || replacement.MaxParallel() != apfl.maxParallel ||
		replacement.NeedRegisterDestinationFD() != apfl.needsRegisterDestinationFD {
		_ = replacement.Close()
		return fmt.Errorf("replacement async page reader geometry (%d,%d,%d,%t) differs from active geometry (%d,%d,%d,%t)",
			hostarch.PageRoundDown(replacement.MaxReadBytes()), replacement.MaxRanges(), replacement.MaxParallel(), replacement.NeedRegisterDestinationFD(),
			apfl.maxReadBytes, apfl.maxRanges, apfl.maxParallel, apfl.needsRegisterDestinationFD)
	}
	apfl.readerSwitchMu.Lock()
	defer apfl.readerSwitchMu.Unlock()
	request := &asyncPagesReaderSwitch{reader: replacement, done: make(chan error, 1)}
	apfl.readerSwitchRequestMu.Lock()
	select {
	case <-apfl.done:
		apfl.readerSwitchRequestMu.Unlock()
		_ = replacement.Close()
		return errors.New("async page loader has already stopped")
	default:
	}
	apfl.readerSwitchRequest = request
	apfl.readerSwitchRequestMu.Unlock()
	apfl.lfStatus.Notify(aplLFSwitchReader)
	select {
	case err := <-request.done:
		if err == nil {
			apfl.durableBacking.Store(true)
		}
		return err
	case <-apfl.done:
		select {
		case err := <-request.done:
			if err == nil {
				apfl.durableBacking.Store(true)
			}
			return err
		default:
			return errors.New("async page loader stopped during reader handoff")
		}
	}
}

// HasDurableBacking reports whether unresolved pages are backed by an
// immutable graph rather than a live source runtime.
func (apfl *AsyncPagesFileLoad) HasDurableBacking() bool {
	return apfl != nil && apfl.durableBacking.Load()
}

// ReaderGeometry returns the queue shape required for a reader handoff.
func (apfl *AsyncPagesFileLoad) ReaderGeometry() AsyncPagesReaderGeometry {
	return AsyncPagesReaderGeometry{
		MaxReadBytes: apfl.maxReadBytes, MaxRanges: apfl.maxRanges,
		MaxParallel: apfl.maxParallel, NeedsRegisterDestinationFD: apfl.needsRegisterDestinationFD,
	}
}

// Drain enables background loading of every remaining page. It is idempotent.
func (apfl *AsyncPagesFileLoad) Drain() {
	if apfl.logicalChunkDemand {
		apfl.drainOnce.Do(func() {
			go func() {
				var loads []*asyncMemoryFileLoad
				apfl.amflsMu.Lock()
				for amfl := apfl.amfls.Front(); amfl != nil; amfl = amfl.Next() {
					loads = append(loads, amfl)
				}
				apfl.amflsMu.Unlock()
				for _, amfl := range loads {
					// awaitLoad expands this physical span to exact logical
					// chunks and queues all discontiguous members of each chunk.
					// Use the current unresolved span instead of a chunk's first
					// historical range: imports can reuse that leading range from
					// an already-resident CoW version.
					apfl.mu.Lock()
					first, last := amfl.unloaded.FirstSegment(), amfl.unloaded.LastSegment()
					if !first.Ok() {
						apfl.mu.Unlock()
						continue
					}
					unresolved := memmap.FileRange{Start: first.Start(), End: last.End()}
					apfl.mu.Unlock()
					if err := amfl.awaitLoad(unresolved); err != nil {
						break
					}
				}
				apfl.demandOnly.Store(false)
				apfl.lfStatus.Notify(aplLFPending)
			}()
		})
		return
	}
	if apfl.demandOnly.Swap(false) {
		apfl.lfStatus.Notify(aplLFPending)
	}
}

// SetPrefetchWindow changes the advisory fault window used by future demand
// loads. Existing in-flight reads are unaffected; queued advisory ranges are
// discarded so a policy switch takes effect promptly.
func (apfl *AsyncPagesFileLoad) SetPrefetchWindow(behind, ahead uint64) error {
	if !hostarch.IsPageAligned(behind) || !hostarch.IsPageAligned(ahead) {
		return fmt.Errorf("async page prefetch window behind=%d ahead=%d is not page-aligned", behind, ahead)
	}
	apfl.mu.Lock()
	apfl.prefetch.Clear()
	apfl.prefetchBehindBytes.Store(behind)
	apfl.prefetchAheadBytes.Store(ahead)
	apfl.mu.Unlock()
	apfl.lfStatus.Notify(aplLFPending)
	return nil
}

// Cancel stops page loading and closes the underlying reader. Any remaining
// waiters fail with context.Canceled. It is safe to call multiple times.
func (apfl *AsyncPagesFileLoad) Cancel() {
	apfl.lfStatus.Notify(aplLFCancel)
}

// Wait waits for the loader to stop and returns its terminal error.
func (apfl *AsyncPagesFileLoad) Wait() error {
	<-apfl.done
	return apfl.err()
}

// RemotePageInstallResult describes the outcome of an unsolicited page push.
type RemotePageInstallResult uint8

const (
	// RemotePageInstallBusy means that at least part of the range is still
	// pending, but the whole range cannot be installed atomically. A concurrent
	// pull may own it, or the request may span loaded and unloaded subranges.
	RemotePageInstallBusy RemotePageInstallResult = iota
	// RemotePageInstallInstalled means that data was installed atomically.
	RemotePageInstallInstalled
	// RemotePageInstallResolved means that no part of the range remains pending.
	// The target therefore no longer depends on the source copy.
	RemotePageInstallResolved
)

// TryInstallRemotePages installs an unsolicited pushed range if none of its
// pages has started a pull. It returns false without changing state when pull
// already owns the range or the stream range is already resolved. Callers that
// need to distinguish those cases should use TryInstallRemotePagesState.
func (apfl *AsyncPagesFileLoad) TryInstallRemotePages(streamOffset uint64, data []byte) (installed bool, err error) {
	result, err := apfl.TryInstallRemotePagesState(streamOffset, data)
	return result == RemotePageInstallInstalled, err
}

// TryInstallRemotePagesState is TryInstallRemotePages with a tri-state result.
// Resolved is reported only while holding the loader locks and after proving
// that no unloaded range overlaps the requested stream interval. This lets a
// push transport release source ownership without confusing an already-loaded
// range with a pull/push race.
func (apfl *AsyncPagesFileLoad) TryInstallRemotePagesState(streamOffset uint64, data []byte) (result RemotePageInstallResult, err error) {
	length := uint64(len(data))
	if length == 0 || !hostarch.IsPageAligned(streamOffset) || !hostarch.IsPageAligned(length) {
		return RemotePageInstallBusy, fmt.Errorf("pushed remote range must be non-empty and page-aligned: off=%d len=%d", streamOffset, length)
	}
	streamEnd := streamOffset + length
	if streamEnd < streamOffset {
		return RemotePageInstallBusy, fmt.Errorf("pushed remote range overflows: off=%d len=%d", streamOffset, length)
	}
	defer func() {
		if result == RemotePageInstallInstalled && apfl.rangeLoaded != nil {
			apfl.rangeLoaded(streamOffset, length)
		}
	}()
	var wakeups []*aplWaiter
	anyPending := false
	apfl.amflsMu.Lock()
	defer func() {
		apfl.amflsMu.Unlock()
		for _, waiter := range wakeups {
			waiter.wakeup.Notify(1)
		}
	}()
	if apfl.logicalChunkDemand {
		for amfl := apfl.amfls.Front(); amfl != nil; amfl = amfl.Next() {
			for _, chunk := range amfl.logicalChunks {
				if len(chunk.ranges) == 0 {
					continue
				}
				chunkStart := chunk.ranges[0].StreamOffset
				chunkEnd := chunkStart
				for _, load := range chunk.ranges {
					if load.StreamOffset != chunkEnd {
						return RemotePageInstallBusy, fmt.Errorf("logical chunk %+v has a non-contiguous stream", chunk.id)
					}
					chunkEnd += load.Target.Length()
				}
				if streamOffset != chunkStart || streamEnd != chunkEnd {
					continue
				}
				amfl.f.mu.Lock()
				apfl.mu.Lock()
				present := 0
				busy := false
				for _, load := range chunk.ranges {
					seg := amfl.unloaded.FindSegment(load.Target.Start)
					if !seg.Ok() || load.Target.End > seg.End() {
						continue
					}
					ul := seg.ValuePtr()
					if ul.off+(load.Target.Start-seg.Start()) != load.StreamOffset || ul.logicalChunk != chunk {
						continue
					}
					present++
					busy = busy || ul.started
				}
				if present == 0 {
					apfl.mu.Unlock()
					amfl.f.mu.Unlock()
					return RemotePageInstallResolved, nil
				}
				if present != len(chunk.ranges) || busy {
					apfl.mu.Unlock()
					amfl.f.mu.Unlock()
					return RemotePageInstallBusy, nil
				}
				for _, load := range chunk.ranges {
					seg := amfl.unloaded.FindSegment(load.Target.Start)
					seg = amfl.unloaded.SplitBefore(seg, load.Target.Start)
					seg = amfl.unloaded.SplitAfter(seg, load.Target.End)
					ul := seg.ValuePtr()
					dataStart := load.StreamOffset - streamOffset
					dataEnd := dataStart + load.Target.Length()
					copied := 0
					if err := amfl.f.withMappingSlicesNoWait(seg.Range(), hostarch.Write, func(dst []byte) error {
						copied += copy(dst, data[dataStart+uint64(copied):dataEnd])
						return nil
					}); err != nil {
						apfl.mu.Unlock()
						amfl.f.mu.Unlock()
						return RemotePageInstallBusy, fmt.Errorf("mapping pushed logical chunk: %w", err)
					}
					if uint64(copied) != load.Target.Length() {
						apfl.mu.Unlock()
						amfl.f.mu.Unlock()
						return RemotePageInstallBusy, fmt.Errorf("pushed logical chunk copied %d bytes, want %d", copied, load.Target.Length())
					}
					for _, waiter := range ul.waiters {
						waiter.pending -= load.Target.Length()
						if waiter.pending == 0 {
							wakeups = append(wakeups, waiter)
							now := gohacks.Nanotime()
							apfl.durWaitedTotal += time.Duration(now - waiter.timeStart)
							apfl.numWaiters--
							if apfl.numWaiters == 0 {
								apfl.durWaitedOne += time.Duration(now - apfl.timeStartWaiters)
								apfl.timeStartWaiters = math.MaxInt64
							}
						}
					}
					amfl.unloaded.Remove(seg)
					apfl.bytesLoaded += load.Target.Length()
				}
				if amfl.lfDone {
					if amfl.unloaded.IsEmpty() {
						amfl.minUnloaded.Store(math.MaxUint64)
						apfl.amfls.Remove(amfl)
						amfl.f.asyncPageLoad.Store(nil)
						amfl.timeline.End()
						if amfl.doneCallback != nil {
							amfl.doneCallback(nil)
							amfl.doneCallback = nil
						}
					} else {
						amfl.minUnloaded.Store(amfl.unloaded.FirstSegment().Start())
					}
				}
				apfl.mu.Unlock()
				amfl.f.mu.Unlock()
				return RemotePageInstallInstalled, nil
			}
		}
		// The range is no unit still to load. If no unit still to load
		// overlaps it, the target has loaded what it holds, or reused a
		// resident unit for it: it needs nothing more of it. Otherwise it is
		// not a unit of the target's, whose pulls load it, as when a target
		// without huge pages installs a 2 MiB page as base pages.
		for amfl := apfl.amfls.Front(); amfl != nil; amfl = amfl.Next() {
			amfl.f.mu.Lock()
			apfl.mu.Lock()
			pending := false
			for ulseg := amfl.unloaded.FirstSegment(); ulseg.Ok() && !pending; ulseg = ulseg.NextSegment() {
				ul := ulseg.ValuePtr()
				pending = streamOffset < ul.off+ulseg.Range().Length() && ul.off < streamEnd
			}
			apfl.mu.Unlock()
			amfl.f.mu.Unlock()
			if pending {
				return RemotePageInstallBusy, nil
			}
		}
		return RemotePageInstallResolved, nil
	}
	for amfl := apfl.amfls.Front(); amfl != nil; amfl = amfl.Next() {
		amfl.f.mu.Lock()
		apfl.mu.Lock()
		for ulseg := amfl.unloaded.FirstSegment(); ulseg.Ok(); ulseg = ulseg.NextSegment() {
			ul := ulseg.ValuePtr()
			unloadedEnd := ul.off + ulseg.Range().Length()
			if streamOffset < unloadedEnd && ul.off < streamEnd {
				anyPending = true
			}
			if streamOffset < ul.off || streamOffset-ul.off > ulseg.Range().Length() ||
				length > ulseg.Range().Length()-(streamOffset-ul.off) {
				continue
			}
			if ul.started {
				apfl.mu.Unlock()
				amfl.f.mu.Unlock()
				return RemotePageInstallBusy, nil
			}
			targetStart := ulseg.Start() + (streamOffset - ul.off)
			ulseg = amfl.unloaded.SplitBefore(ulseg, targetStart)
			ulseg = amfl.unloaded.SplitAfter(ulseg, targetStart+length)
			copied := 0
			if err := amfl.f.withMappingSlicesNoWait(ulseg.Range(), hostarch.Write, func(dst []byte) error {
				copied += copy(dst, data[copied:])
				return nil
			}); err != nil {
				apfl.mu.Unlock()
				amfl.f.mu.Unlock()
				return RemotePageInstallBusy, fmt.Errorf("mapping pushed remote range: %w", err)
			}
			if uint64(copied) != length {
				apfl.mu.Unlock()
				amfl.f.mu.Unlock()
				return RemotePageInstallBusy, fmt.Errorf("pushed remote range copied %d bytes, want %d", copied, length)
			}
			for _, waiter := range ul.waiters {
				waiter.pending -= length
				if waiter.pending == 0 {
					wakeups = append(wakeups, waiter)
					now := gohacks.Nanotime()
					apfl.durWaitedTotal += time.Duration(now - waiter.timeStart)
					apfl.numWaiters--
					if apfl.numWaiters == 0 {
						apfl.durWaitedOne += time.Duration(now - apfl.timeStartWaiters)
						apfl.timeStartWaiters = math.MaxInt64
					}
				}
			}
			amfl.unloaded.Remove(ulseg)
			apfl.bytesLoaded += length
			if amfl.lfDone {
				if amfl.unloaded.IsEmpty() {
					amfl.minUnloaded.Store(math.MaxUint64)
					apfl.amfls.Remove(amfl)
					if !amfl.sparse {
						amfl.f.asyncPageLoad.Store(nil)
					} else {
						amfl.f.removeSparseAsyncLoad(amfl)
						apfl.demandOnly.Store(false)
						apfl.lfStatus.Notify(aplLFPending)
					}
					amfl.timeline.End()
					if amfl.doneCallback != nil {
						amfl.doneCallback(nil)
						amfl.doneCallback = nil
					}
				} else {
					amfl.minUnloaded.Store(amfl.unloaded.FirstSegment().Start())
				}
			}
			apfl.mu.Unlock()
			amfl.f.mu.Unlock()
			return RemotePageInstallInstalled, nil
		}
		apfl.mu.Unlock()
		amfl.f.mu.Unlock()
	}
	if anyPending {
		return RemotePageInstallBusy, nil
	}
	return RemotePageInstallResolved, nil
}

// MemoryFilesDone must be called after calling LoadFrom() for all MemoryFiles
// loading from apfl. MemoryFilesDone may be called multiple times; subsequent
// calls have no effect.
func (apfl *AsyncPagesFileLoad) MemoryFilesDone() {
	apfl.lfStatus.Notify(aplLFDone)
}

// pendingPageLoads returns the loaders of the pages in fr that are still to be
// loaded.
func (f *MemoryFile) pendingPageLoads(fr memmap.FileRange) []*AsyncPagesFileLoad {
	var loads []*AsyncPagesFileLoad
	add := func(amfl *asyncMemoryFileLoad) {
		if amfl == nil || fr.End <= amfl.minUnloaded.Load() {
			return
		}
		amfl.pf.mu.Lock()
		pending := !amfl.unloaded.IsEmptyRange(fr)
		amfl.pf.mu.Unlock()
		if pending && !slices.Contains(loads, amfl.pf) {
			loads = append(loads, amfl.pf)
		}
	}
	add(f.asyncPageLoad.Load())
	if sparse := f.sparseAsyncLoads.Load(); sparse != nil {
		for _, amfl := range *sparse {
			add(amfl)
		}
	}
	return loads
}

// IsAsyncLoading returns true if async page loading is in progress or has
// failed permanently.
func (f *MemoryFile) IsAsyncLoading() bool {
	return f.asyncPageLoad.Load() != nil || f.sparseAsyncLoads.Load() != nil
}

// AsyncLoadingMapUnit returns the maximum address-space mapping unit that
// should be used around the page at off while demand-only page loading is
// active. Mapping a larger PMA range calls MapInternal for that entire range
// and accidentally turns one guest fault into an eager remote load. The unit
// follows the current runtime prefetch window, while the async reader remains
// free to split or coalesce network requests independently. A sparse load's
// is its residency unit's: a 2 MiB page's, mapped whole so that the host maps
// it with one huge page, or a logical chunk's.
func (f *MemoryFile) AsyncLoadingMapUnit(off uint64) uint64 {
	if loads := f.sparseAsyncLoads.Load(); loads != nil && len(*loads) != 0 {
		if _, ok := f.HugePageAt(off); ok {
			return hostarch.HugePageSize
		}
		return SparseChunkBytes
	}
	amfl := f.asyncPageLoad.Load()
	if amfl == nil || !amfl.pf.demandOnly.Load() {
		return 0
	}
	if amfl.pf.logicalChunkDemand {
		// Logical chunks may be physically discontiguous after CoW. Keep guest
		// mappings page-sized even though the load barrier is chunk-sized.
		return hostarch.PageSize
	}
	unit := hostarch.PageSize + amfl.pf.prefetchBehindBytes.Load() + amfl.pf.prefetchAheadBytes.Load()
	if unit < hostarch.PageSize || unit&(unit-1) != 0 {
		// mapAS alignment requires a power of two. Fall back to the correctness
		// unit for an unusual page-aligned but non-power-of-two policy.
		return hostarch.PageSize
	}
	return unit
}

// AwaitLoadAll blocks until async page loading has completed. If async page
// loading is not in progress, AwaitLoadAll returns immediately.
func (f *MemoryFile) AwaitLoadAll() error {
	if amfl := f.asyncPageLoad.Load(); amfl != nil {
		return amfl.awaitLoad(memmap.FileRange{0, hostarch.PageRoundDown(uint64(math.MaxUint64))})
	}
	if loads := f.sparseAsyncLoads.Load(); loads != nil {
		for _, amfl := range *loads {
			if err := amfl.awaitLoad(memmap.FileRange{0, hostarch.PageRoundDown(uint64(math.MaxUint64))}); err != nil {
				return err
			}
		}
	}
	return nil
}

// loaded returns true if data has been loaded for all pages in fr.
func (amfl *asyncMemoryFileLoad) loaded(fr memmap.FileRange) bool {
	if fr.End <= amfl.minUnloaded.Load() {
		return true
	}
	fr.Start = hostarch.PageRoundDown(fr.Start)
	fr.End = hostarch.MustPageRoundUp(fr.End)
	apfl := amfl.pf
	apfl.mu.Lock()
	defer apfl.mu.Unlock()
	return amfl.unloaded.IsEmptyRange(fr)
}

// awaitLoad blocks until data has been loaded for all pages in fr.
//
// Preconditions: At least one reference must be held on all unloaded pages in
// fr.
func (amfl *asyncMemoryFileLoad) awaitLoad(fr memmap.FileRange) error {
	// Lockless fast path:
	if fr.End <= amfl.minUnloaded.Load() {
		return nil
	}

	// fr might not be page-aligned; everything else involved in async page
	// loading requires page-aligned FileRanges.
	fr.Start = hostarch.PageRoundDown(fr.Start)
	fr.End = hostarch.MustPageRoundUp(fr.End)
	requested := fr

	apfl := amfl.pf
	apfl.mu.Lock()
	if err := apfl.err(); err != nil {
		if amfl.unloaded.IsEmptyRange(fr) {
			// fr is already loaded.
			apfl.mu.Unlock()
			return nil
		}
		// A previous error means that fr will never be loaded.
		apfl.mu.Unlock()
		return err
	}
	var logicalChunks []*logicalChunkLoad
	if apfl.logicalChunkDemand {
		// A logical slot can have several live CoW versions in one family
		// MemoryFile. Select the exact chunk instance associated with the
		// requested physical range instead of looking it up by logical ID.
		seen := make(map[*logicalChunkLoad]struct{})
		seg := amfl.unloaded.FindSegment(requested.Start)
		if !seg.Ok() {
			seg = amfl.unloaded.LowerBoundSegment(requested.Start)
		}
		for ; seg.Ok() && seg.Start() < requested.End; seg = seg.NextSegment() {
			chunk := seg.ValuePtr().logicalChunk
			if chunk == nil {
				continue
			}
			if _, ok := seen[chunk]; !ok {
				seen[chunk] = struct{}{}
				logicalChunks = append(logicalChunks, chunk)
			}
		}
	}
	var waitRanges []memmap.FileRange
	if len(logicalChunks) != 0 {
		for _, chunk := range logicalChunks {
			for _, load := range chunk.ranges {
				waitRanges = append(waitRanges, load.Target)
			}
		}
	} else if amfl.sparse {
		// Sparse chunk residency is transactional at 1 MiB granularity. Expand
		// an access to every complete unloaded chunk that it intersects before
		// splitting the unloaded set or publishing any waiter. This ensures
		// that no guest mapping can make only a subset of a chunk resident,
		// even when the caller asks MapInternal for a single 4 KiB page.
		first := true
		seg := amfl.unloaded.FindSegment(requested.Start)
		if !seg.Ok() {
			seg = amfl.unloaded.LowerBoundSegment(requested.Start)
		}
		for ; seg.Ok() && seg.Start() < requested.End; seg = seg.NextSegment() {
			if first || seg.Start() < fr.Start {
				fr.Start = seg.Start()
			}
			if first || seg.End() > fr.End {
				fr.End = seg.End()
			}
			first = false
		}
		if first {
			apfl.mu.Unlock()
			return nil
		}
		waitRanges = append(waitRanges, fr)
	} else {
		waitRanges = append(waitRanges, fr)
	}
	w := aplWaiterPool.Get().(*aplWaiter)
	defer aplWaiterPool.Put(w)
	w.fr = requested
	w.pending = 0
	for _, waitRange := range waitRanges {
		amfl.unloaded.MutateRange(waitRange, func(ulseg aplUnloadedIterator) bool {
			ul := ulseg.ValuePtr()
			ulFR := ulseg.Range()
			ullen := ulFR.Length()
			if len(ul.waiters) == 0 {
				apfl.bytesWaited += ullen
				if !ul.started {
					apfl.priority.PushBack(aplFileRange{amfl, ulFR})
					behind, ahead := apfl.prefetchBehindBytes.Load(), apfl.prefetchAheadBytes.Load()
					if !apfl.logicalChunkDemand && apfl.demandOnly.Load() && (behind != 0 || ahead != 0) && apfl.prefetch.Len() < 4096 {
						start := ulFR.Start - min(behind, ulFR.Start)
						end := ulFR.End + min(ahead, ^uint64(0)-ulFR.End)
						if start != ulFR.Start || end != ulFR.End {
							apfl.prefetch.PushBack(aplFileRange{amfl, memmap.FileRange{Start: start, End: end}})
						}
					}
				}
				if logAwaitedLoads {
					log.Infof("MemoryFile(%p): prioritize %v", amfl.f, ulFR)
				}
			}
			ul.waiters = append(ul.waiters, w)
			w.pending += ullen
			return true
		})
	}
	pending := w.pending != 0
	if pending {
		apfl.faults++
		if apfl.haveLastFault && requested.Start == apfl.lastFaultEnd {
			apfl.sequentialFaults++
		}
		apfl.lastFaultEnd = requested.End
		apfl.haveLastFault = true
		w.timeStart = gohacks.Nanotime()
		if apfl.numWaiters == 0 {
			apfl.timeStartWaiters = w.timeStart
		}
		apfl.numWaiters++
		apfl.totalWaiters++
	}
	apfl.mu.Unlock()
	if pending {
		apfl.lfStatus.Notify(aplLFPending)
		if logAwaitedLoads {
			log.Infof("MemoryFile(%p): awaitLoad goid %d start: %v (%d bytes)", amfl.f, goid.Get(), requested, requested.Length())
		}
		w.wakeup.WaitAndAckAll()
		if logAwaitedLoads {
			waitNS := gohacks.Nanotime() - w.timeStart
			log.Infof("MemoryFile(%p): awaitLoad goid %d waited %v: %v (%d bytes)", amfl.f, goid.Get(), time.Duration(waitNS), requested, requested.Length())
		}
	}
	return apfl.err()
}

func (apfl *AsyncPagesFileLoad) canEnqueue() bool {
	return apfl.qavail > 0
}

// Preconditions: apfl.canEnqueue() == true.
func (apfl *AsyncPagesFileLoad) enqueueCurOp() {
	if apfl.qavail <= 0 {
		panic("queue full")
	}
	op := apfl.curOp
	if op.total == 0 {
		panic("invalid read of 0 bytes")
	}
	if op.total > apfl.maxReadBytes {
		panic(fmt.Sprintf("read of %d bytes exceeds per-read limit of %d bytes", op.total, apfl.maxReadBytes))
	}

	apfl.qavail--
	apfl.curOp = nil
	if len(op.frs) == 1 && len(op.iovecs) == 1 {
		// Perform a non-vectorized read to save an indirection (and possible
		// userspace-to-kernelspace copy) in the AsyncReader implementation.
		apfl.ar.AddRead(int(apfl.curOpID), op.off(), op.amfl.df, op.frs[0], stateio.SliceFromIovec(op.iovecs[0]))
	} else {
		apfl.ar.AddReadv(int(apfl.curOpID), op.off(), op.total, op.amfl.df, op.frs, op.iovecs)
	}
	if logAwaitedLoads && !op.tempRef {
		log.Infof("MemoryFile(%p): awaited opid %d start, read %d bytes: %v", op.amfl.f, apfl.curOpID, op.total, op.frs)
	}
}

// Preconditions:
// - apfl.canEnqueue() == true.
// - fr.Length() > 0.
// - fr must be page-aligned.
func (apfl *AsyncPagesFileLoad) enqueueRange(amfl *asyncMemoryFileLoad, fr memmap.FileRange, off uint64, tempRef bool, logicalChunk *logicalChunkLoad) (uint64, error) {
	for {
		if apfl.curOp == nil {
			id, err := apfl.opsBusy.FirstZero(0)
			if err != nil {
				panic(fmt.Sprintf("all ops busy with qavail=%d: %v", apfl.qavail, err))
			}
			apfl.opsBusy.Add(id)
			op := &apfl.ops[id]
			op.total = 0
			op.frs = op.frs[:0]
			op.iovecs = op.iovecs[:0]
			op.leases = op.leases[:0]
			apfl.curOp = op
			apfl.curOpID = id
		}
		n, err := apfl.combine(amfl, fr, off, tempRef, logicalChunk)
		if err == errBackingBusy {
			// Submit the current op so that its leases are released when it
			// completes, and stop enqueueing until the main loop has waited.
			apfl.busyBacking = amfl.f
			if apfl.curOp.total != 0 {
				apfl.enqueueCurOp()
			} else {
				apfl.opsBusy.Remove(apfl.curOpID)
				apfl.curOp = nil
			}
			return 0, nil
		}
		if err != nil {
			return 0, err
		}
		if n > 0 {
			return n, nil
		}
		// Flush the existing (conflicting) op and try again with a new one.
		apfl.enqueueCurOp()
		if !apfl.canEnqueue() {
			return 0, nil
		}
	}
}

// combine adds as much of the given load as possible to g.curOp and returns
// the number of bytes added.
//
// Preconditions:
// - fr.Length() > 0.
// - fr must be page-aligned.
func (apfl *AsyncPagesFileLoad) combine(amfl *asyncMemoryFileLoad, fr memmap.FileRange, off uint64, tempRef bool, logicalChunk *logicalChunkLoad) (uint64, error) {
	op := apfl.curOp
	if op.total != 0 {
		if op.end != off {
			// Non-contiguous in the pages file.
			return 0, nil
		}
		if op.amfl != amfl {
			// Differing MemoryFile.
			return 0, nil
		}
		if len(op.frs) == cap(op.frs) && op.frs[len(op.frs)-1].End != fr.Start {
			// Non-contiguous in the MemoryFile, and we're out of space for
			// FileRanges.
			return 0, nil
		}
		if op.tempRef != tempRef {
			// Incompatible reference-counting semantics. We could handle this
			// by making tempRef per-FileRange, but it's very unlikely that an
			// awaited load (tempRef=false) will happen to be followed by an
			// unawaited load (tempRef=true) at the correct offset.
			return 0, nil
		}
		if op.logicalChunk != logicalChunk {
			return 0, nil
		}
	}

	// Apply direct length limits.
	n := fr.Length()
	if op.total+n >= apfl.maxReadBytes {
		n = apfl.maxReadBytes - op.total
	}
	if n == 0 {
		return 0, nil
	}
	fr.End = fr.Start + n

	// Collect iovecs, which may further limit length.
	n = 0
	// Don't wait for a lease: this goroutine may hold leases for queued reads,
	// and holds locks that lease holders may need. See busyBacking.
	blocks, mappingLease, err := amfl.f.tryMapInternalWithLeaseNoWait(fr, hostarch.Write)
	if err != nil {
		return 0, err
	}
	for !blocks.IsEmpty() {
		block := blocks.Head()
		if block.NeedSafecopy() {
			if mappingLease != nil {
				mappingLease.Release()
			}
			return 0, fmt.Errorf("async platform MemoryFile mapping unexpectedly requires safecopy")
		}
		bs := block.ToSlice()
		if len(op.iovecs) > 0 {
			if canMergeIovecAndSlice(op.iovecs[len(op.iovecs)-1], bs) {
				op.iovecs[len(op.iovecs)-1].Len += uint64(len(bs))
				n += uint64(len(bs))
				blocks = blocks.Tail()
				continue
			}
			if len(op.iovecs) == cap(op.iovecs) {
				break
			}
		}
		op.iovecs = append(op.iovecs, unix.Iovec{
			Base: &bs[0],
			Len:  uint64(len(bs)),
		})
		n += uint64(len(bs))
		blocks = blocks.Tail()
	}
	if n == 0 {
		if mappingLease != nil {
			mappingLease.Release()
		}
		return 0, nil
	}
	if mappingLease != nil {
		op.leases = append(op.leases, mappingLease)
	}
	fr.End = fr.Start + n

	// With the length decided, finish updating op.
	if op.total == 0 {
		op.end = off
		op.amfl = amfl
		op.logicalChunk = logicalChunk
	}
	op.end += n
	op.total += n
	op.tempRef = tempRef
	if len(op.frs) > 0 && op.frs[len(op.frs)-1].End == fr.Start {
		op.frs[len(op.frs)-1].End = fr.End
	} else {
		op.frs = append(op.frs, fr)
	}
	return n, nil
}

func releaseAPLOpLeases(op *aplOp) {
	for _, lease := range op.leases {
		lease.Release()
	}
	op.leases = op.leases[:0]
}

func (apfl *AsyncPagesFileLoad) main() {
	defer func() {
		// Close ar first since this synchronously stops inflight I/O.
		if err := apfl.ar.Close(); err != nil {
			// Completed reads are complete irrespective of err, so log err
			// rather than propagating it.
			log.Warningf("Async page loading: stateio.AsyncReader.Close failed: %v", err)
		}
		for i := range apfl.ops {
			releaseAPLOpLeases(&apfl.ops[i])
		}
		apfl.timeline.End()
		// Wake up any remaining waiters so that they can observe apfl.err().
		// Leave all segments in asyncMemoryFileLoad.unloaded so that new
		// callers of awaitLoad() will still observe the correct (permanently
		// unloaded) segments.
		apfl.amflsMu.Lock()
		apfl.mu.Lock()
		for amfl := apfl.amfls.Front(); amfl != nil; amfl = amfl.Next() {
			for ulseg := amfl.unloaded.FirstSegment(); ulseg.Ok(); ulseg = ulseg.NextSegment() {
				ul := ulseg.ValuePtr()
				ullen := ulseg.Range().Length()
				for _, w := range ul.waiters {
					w.pending -= ullen
					if w.pending == 0 {
						w.wakeup.Notify(1)
					}
				}
				ul.started = false
				ul.waiters = nil
			}
			if amfl.doneCallback != nil {
				amfl.doneCallback(apfl.err())
				amfl.doneCallback = nil
			}
		}
		apfl.mu.Unlock()
		apfl.amflsMu.Unlock()
		if apfl.doneCallback != nil {
			apfl.doneCallback(apfl.err())
		}
		apfl.readerSwitchRequestMu.Lock()
		if request := apfl.readerSwitchRequest; request != nil {
			apfl.readerSwitchRequest = nil
			_ = request.reader.Close()
			request.done <- errors.New("async page loader stopped before reader handoff")
		}
		apfl.readerSwitchRequestMu.Unlock()
		close(apfl.done)
	}()

	maxParallel := apfl.maxParallel
	// Storage reused between main loop iterations:
	var completions []stateio.Completion
	var wakeups []*aplWaiter
	var decRefs []aplFileRange
	type completedRange struct {
		offset uint64
		length uint64
	}
	var loadedCallbacks []completedRange

	dropDelayedDecRefs := func() {
		if len(decRefs) != 0 {
			for _, fr := range decRefs {
				fr.amfl.f.DecRef(fr.FileRange)
			}
			decRefs = decRefs[:0]
		}
	}
	defer dropDelayedDecRefs()

	// discardEmptyLoads removes destinations whose remaining pages became
	// waste before they were demanded. DecRef cancels those ranges while
	// holding MemoryFile.mu, so final list removal is deferred to this goroutine
	// to preserve the amflsMu -> MemoryFile.mu -> mu lock order.
	discardEmptyLoads := func() bool {
		var callbacks []func(error)
		apfl.amflsMu.Lock()
		for amfl := apfl.amfls.Front(); amfl != nil; {
			next := amfl.Next()
			amfl.f.mu.Lock()
			apfl.mu.Lock()
			empty := amfl.lfDone && amfl.unloaded.IsEmpty()
			if empty {
				amfl.minUnloaded.Store(math.MaxUint64)
				apfl.amfls.Remove(amfl)
			}
			apfl.mu.Unlock()
			if empty {
				if !amfl.sparse {
					amfl.f.asyncPageLoad.Store(nil)
				} else {
					amfl.f.removeSparseAsyncLoad(amfl)
				}
				amfl.timeline.End()
				if amfl.doneCallback != nil {
					callbacks = append(callbacks, amfl.doneCallback)
					amfl.doneCallback = nil
				}
			}
			amfl.f.mu.Unlock()
			amfl = next
		}
		empty := apfl.amfls.Empty()
		apfl.amflsMu.Unlock()
		for _, callback := range callbacks {
			callback(nil)
		}
		return empty
	}

	// Don't start timing until we have pages to load.
	apfl.lfStatus.Wait()
	timeStart := gohacks.Nanotime()
	apfl.timeline.Reached("async page loading started")
	if log.IsLogging(log.Debug) {
		log.Debugf("Async page loading started")
		progressTicker := time.NewTicker(5 * time.Second)
		progressStopC := make(chan struct{})
		defer func() { close(progressStopC) }()
		go func() {
			timeLast := timeStart
			bytesLoadedLast := uint64(0)
			for {
				select {
				case <-progressStopC:
					progressTicker.Stop()
					return
				case <-progressTicker.C:
					// Take a snapshot of our progress.
					apfl.mu.Lock()
					totalWaiters := apfl.totalWaiters
					timeStartWaiters := apfl.timeStartWaiters
					durWaitedOne := apfl.durWaitedOne
					durWaitedTotal := apfl.durWaitedTotal
					bytesWaited := apfl.bytesWaited
					bytesLoaded := apfl.bytesLoaded
					apfl.mu.Unlock()
					now := gohacks.Nanotime()
					durTotal := time.Duration(now - timeStart)
					// apfl can have at least one waiter for a very long time
					// due to new waiters enqueueing before old ones are
					// served; avoid apparent jumps in durWaitedOne.
					if timeStartWaiters < now {
						durWaitedOne += time.Duration(now - timeStartWaiters)
					}
					durDelta := time.Duration(now - timeLast)
					bytesLoadedDelta := bytesLoaded - bytesLoadedLast
					bandwidthSinceLast := float64(bytesLoadedDelta) * 1e-6 / durDelta.Seconds()
					apfl.timeline.Reached(fmt.Sprintf("%.3f MB/s", bandwidthSinceLast))
					log.Infof("Async page loading in progress for %s (%d bytes, %.3f MB/s); since last update %s ago: %d bytes, %.3f MB/s; %d waiters waited %v~%v for %d bytes", durTotal.Round(time.Millisecond), bytesLoaded, float64(bytesLoaded)*1e-6/durTotal.Seconds(), durDelta.Round(time.Millisecond), bytesLoadedDelta, bandwidthSinceLast, totalWaiters, durWaitedOne.Round(time.Millisecond), durWaitedTotal.Round(time.Millisecond), bytesWaited)
					timeLast = now
					bytesLoadedLast = bytesLoaded
				}
			}
		}()
	}

	memoryFilesDone := false
	readerSwitchPending := false
	for {
		ev := apfl.lfStatus.Pending()
		if ev&aplLFCancel != 0 {
			apfl.mu.Lock()
			apfl.errVal.Store(context.Canceled)
			apfl.mu.Unlock()
			return
		}
		if ev&aplLFDone != 0 {
			memoryFilesDone = true
			apfl.lfStatus.Ack(aplLFDone)
		}
		if ev&aplLFSwitchReader != 0 {
			readerSwitchPending = true
		}
		if readerSwitchPending && apfl.qavail == maxParallel && apfl.curOp == nil {
			apfl.readerSwitchRequestMu.Lock()
			request := apfl.readerSwitchRequest
			apfl.readerSwitchRequest = nil
			apfl.readerSwitchRequestMu.Unlock()
			apfl.lfStatus.Ack(aplLFSwitchReader)
			readerSwitchPending = false
			if request != nil {
				old := apfl.ar
				apfl.ar = request.reader
				if err := old.Close(); err != nil {
					log.Warningf("Async page loading: closing replaced reader failed: %v", err)
				}
				request.done <- nil
			}
			continue
		}
		noLoads := discardEmptyLoads()
		// Enqueue as many reads as possible.
		if !apfl.canEnqueue() {
			panic("main loop invariant failed")
		}
		// Prioritize reading pages with waiters.
		apfl.mu.Lock()
		for !readerSwitchPending && apfl.busyBacking == nil && apfl.canEnqueue() && !apfl.priority.Empty() {
			fr := apfl.priority.PopFront()
			// All pages in apfl.priority have non-zero waiters and were split
			// around fr by fr.amfl.awaitLoad(), and fr.amfl.unloaded never
			// merges segments with waiters. Thus, we don't need to split
			// around fr again, and fr.Intersect(ulseg.Range()) ==
			// ulseg.Range().
			ulseg := fr.amfl.unloaded.LowerBoundSegment(fr.Start)
			for ulseg.Ok() && ulseg.Start() < fr.End {
				ul := ulseg.ValuePtr()
				ulFR := ulseg.Range()
				if ul.started {
					fr.Start = ulFR.End
					ulseg = ulseg.NextSegment()
					continue
				}
				// Awaited pages are guaranteed to have a reference held (by
				// fr.amfl.awaitLoad() precondition), so they can't become
				// waste (which would allow them to be racily released or
				// recycled).
				n, err := apfl.enqueueRange(fr.amfl, ulFR, ul.off, false /* tempRef */, ul.logicalChunk)
				if err != nil {
					apfl.errVal.Store(fmt.Errorf("mapping pages for asynchronous load: %w", err))
					apfl.mu.Unlock()
					return
				}
				if n == 0 {
					// Try again in the next iteration of the main loop, when
					// we have space in the queue again.
					apfl.priority.PushFront(fr)
					break
				}
				ulFR.End = ulFR.Start + n
				ulseg = fr.amfl.unloaded.SplitAfter(ulseg, ulFR.End)
				ulseg.ValuePtr().started = true
				fr.Start = ulFR.End
				if fr.Length() > 0 {
					// Cycle the rest of fr to the end of apl.priority. This
					// prevents large awaited reads from starving other
					// waiters.
					apfl.priority.PushBack(fr)
					break
				}
				ulseg = ulseg.NextSegment()
			}
		}
		apfl.mu.Unlock()
		// Use spare queue slots for forward prefetches created by faults. The
		// lock order matches background loading: amflsMu -> MemoryFile.mu ->
		// apfl.mu. Awaited reads above remain strictly higher priority.
		if !readerSwitchPending && apfl.busyBacking == nil && apfl.canEnqueue() && apfl.demandOnly.Load() &&
			(apfl.prefetchBehindBytes.Load() != 0 || apfl.prefetchAheadBytes.Load() != 0) {
			apfl.amflsMu.Lock()
			for apfl.busyBacking == nil && apfl.canEnqueue() {
				apfl.mu.Lock()
				if apfl.prefetch.Empty() {
					apfl.mu.Unlock()
					break
				}
				prefetch := apfl.prefetch.PopFront()
				apfl.mu.Unlock()
				amfl := prefetch.amfl
				amfl.f.mu.Lock()
				apfl.mu.Lock()
				ulseg := amfl.unloaded.FindSegment(prefetch.Start)
				if !ulseg.Ok() {
					ulseg = amfl.unloaded.LowerBoundSegment(prefetch.Start)
				}
				for ulseg.Ok() && ulseg.Start() < prefetch.End && apfl.canEnqueue() {
					ul := ulseg.ValuePtr()
					ulFR := ulseg.Range()
					if ul.started || len(ul.waiters) != 0 {
						ulseg = ulseg.NextSegment()
						continue
					}
					loadFR := ulFR.Intersect(prefetch.FileRange)
					ulseg = amfl.unloaded.SplitBefore(ulseg, loadFR.Start)
					ul = ulseg.ValuePtr()
					n, err := apfl.enqueueRange(amfl, loadFR, ul.off, true /* tempRef */, ul.logicalChunk)
					if err != nil {
						apfl.errVal.Store(fmt.Errorf("mapping pages for asynchronous prefetch: %w", err))
						apfl.mu.Unlock()
						amfl.f.mu.Unlock()
						apfl.amflsMu.Unlock()
						return
					}
					if n == 0 {
						break
					}
					loadFR.End = loadFR.Start + n
					ulseg = amfl.unloaded.SplitAfter(ulseg, loadFR.End)
					ulseg.ValuePtr().started = true
					amfl.f.incRefLocked(loadFR)
					ulseg = ulseg.NextSegment()
				}
				apfl.mu.Unlock()
				amfl.f.mu.Unlock()
			}
			apfl.amflsMu.Unlock()
		}
		// Fill remaining queue with reads for pages with no waiters.
		// While throttled, a read of pages without waiters starts only when no
		// other read is in flight.
		throttled := apfl.throttled.Load()
		if !readerSwitchPending && apfl.busyBacking == nil && apfl.canEnqueue() && !apfl.demandOnly.Load() && (!throttled || (apfl.qavail == maxParallel && apfl.curOp == nil)) {
			apfl.amflsMu.Lock()
			// Unawaited loads from earlier MemoryFiles are prioritized over
			// unawaited loads from later MemoryFiles. Callers of
			// MemoryFile.LoadFrom() (in //pkg/sentry/kernel) always load the
			// MemoryFile containing application memory before other
			// MemoryFiles (which most often store disk state); thus, this
			// property ensures that application memory is prioritized over
			// disk state. This is reasonable since memory latency is
			// significantly lower than disk latency, so applications are
			// likely to be more sensitive to elevated memory latency due to
			// awaited loads vs. elevated disk latency.
		amflsLoop:
			for amfl := apfl.amfls.Front(); amfl != nil; amfl = amfl.Next() {
				amfl.f.mu.Lock()
				apfl.mu.Lock()
				ulseg := amfl.unloaded.LowerBoundSegment(amfl.minUnstarted)
				for ulseg.Ok() {
					ul := ulseg.ValuePtr()
					ulFR := ulseg.Range()
					if ul.started {
						amfl.minUnstarted = ulFR.End
						ulseg = ulseg.NextSegment()
						continue
					}
					// We need to take page references during reading to
					// prevent pages from becoming waste due to concurrent
					// dropping of the last reference.
					n, err := apfl.enqueueRange(amfl, ulFR, ul.off, true /* tempRef */, ul.logicalChunk)
					if err != nil {
						apfl.errVal.Store(fmt.Errorf("mapping pages for asynchronous background load: %w", err))
						apfl.mu.Unlock()
						amfl.f.mu.Unlock()
						apfl.amflsMu.Unlock()
						return
					}
					if n == 0 {
						apfl.mu.Unlock()
						amfl.f.mu.Unlock()
						break amflsLoop
					}
					partial := n < ulFR.Length()
					ulFR.End = ulFR.Start + n
					ulseg = amfl.unloaded.SplitAfter(ulseg, ulFR.End)
					ulseg.ValuePtr().started = true
					amfl.minUnstarted = ulFR.End
					amfl.f.incRefLocked(ulFR)
					if (throttled && partial) || !apfl.canEnqueue() {
						apfl.mu.Unlock()
						amfl.f.mu.Unlock()
						break amflsLoop
					}
					ulseg = ulseg.NextSegment()
				}
				apfl.mu.Unlock()
				amfl.f.mu.Unlock()
			}
			apfl.amflsMu.Unlock()
		}
		// Flush pending op.
		if !readerSwitchPending && apfl.curOp != nil {
			apfl.enqueueCurOp()
		}

		if f := apfl.busyBacking; f != nil {
			apfl.busyBacking = nil
			if apfl.qavail == maxParallel {
				// No reads are queued, so this goroutine holds no leases.
				// Wait for the exclusive backing operation without holding
				// any locks, then try again. Otherwise, wait below for queued
				// reads to complete and release their leases.
				f.awaitBackingIdle()
				continue
			}
		}

		if apfl.qavail == maxParallel {
			if readerSwitchPending {
				continue
			}
			// We are out of work to do.
			if memoryFilesDone && (noLoads || !apfl.demandOnly.Load()) {
				// Successfully completed all loading for all MemoryFiles.
				durTotal := time.Duration(gohacks.Nanotime() - timeStart)
				apfl.mu.Lock()
				log.Infof("Async page loading completed in %s (%d bytes, %.3f MB/s); %d waiters waited %v~%v for %d bytes", durTotal.Round(time.Millisecond), apfl.bytesLoaded, float64(apfl.bytesLoaded)*1e-6/durTotal.Seconds(), apfl.totalWaiters, apfl.durWaitedOne.Round(time.Millisecond), apfl.durWaitedTotal.Round(time.Millisecond), apfl.bytesWaited)
				apfl.mu.Unlock()
				return
			}
			ev := apfl.lfStatus.Wait()
			if ev&aplLFCancel != 0 {
				continue
			}
			if ev&aplLFPending != 0 {
				// We may have raced with MemoryFile.LoadFrom() inserting into
				// asyncMemoryFileLoad.unloaded.
				apfl.lfStatus.Ack(aplLFPending)
				continue
			}
			if ev&aplLFDone != 0 {
				continue
			}
			if ev&aplLFSwitchReader != 0 {
				continue
			}
			panic(fmt.Sprintf("unknown events in lfStatus: %#x", ev))
		}

		// Wait for any number of reads to complete.
		var err error
		completions, err = apfl.ar.Wait(completions[:0], 1 /* minCompletions */)
		if err != nil {
			log.Warningf("Async page loading failed: stateio.AsyncReader.Wait failed: %v", err)
			apfl.mu.Lock()
			detail := fmt.Sprintf("waiting for remote page read: %v", err)
			apfl.errDetail.Store(&detail)
			apfl.errVal.Store(linuxerr.EIO)
			apfl.mu.Unlock()
			return
		}

		// Process completions.
		apfl.amflsMu.Lock()
		apfl.mu.Lock()
		for _, c := range completions {
			op := &apfl.ops[c.ID]
			releaseAPLOpLeases(op)
			apfl.opsBusy.Remove(uint32(c.ID))
			apfl.qavail++
			if op.tempRef {
				// Delay f.DecRef(fr) until after dropping locks. This is
				// required to avoid lock recursion via dropping the last
				// reference => asyncMemoryFileLoad.cancelWasteLoad() =>
				// apfl.mu.Lock().
				for _, fr := range op.frs {
					decRefs = append(decRefs, aplFileRange{op.amfl, fr})
				}
			}
			if c.N != op.total {
				log.Warningf("Async page loading failed: read for MemoryFile(%p) pages %v (total %d bytes) returned %d bytes, error: %v", op.amfl.f, op.frs, op.total, c.N, c.Err)
				detail := fmt.Sprintf("remote page read at offset %d returned %d of %d bytes: %v", op.off(), c.N, op.total, c.Err)
				apfl.errDetail.Store(&detail)
				apfl.errVal.Store(linuxerr.EIO)
				apfl.mu.Unlock()
				apfl.amflsMu.Unlock()
				return
			}
			apfl.bytesLoaded += op.total
			if apfl.rangeLoaded != nil {
				loadedCallbacks = append(loadedCallbacks, completedRange{offset: uint64(op.off()), length: op.total})
			}
			amfl := op.amfl
			haveWaiters := false
			now := int64(0)
			for _, fr := range op.frs {
				// All pages in fr have been started and were split around fr
				// when they were started (above), and fr.amfl.unloaded never
				// merges started segments. Thus, we don't need to split around
				// fr again, and fr.Intersect(ulseg.Range()) == ulseg.Range().
				for ulseg := amfl.unloaded.FindSegment(fr.Start); ulseg.Ok() && ulseg.Start() < fr.End; ulseg = amfl.unloaded.Remove(ulseg).NextSegment() {
					ul := ulseg.ValuePtr()
					ullen := ulseg.Range().Length()
					if !ul.started {
						panic(fmt.Sprintf("MemoryFile(%p): completion of %v includes pages %v that were never started", amfl.f, fr, ulseg.Range()))
					}
					for _, w := range ul.waiters {
						haveWaiters = true
						w.pending -= ullen
						if w.pending == 0 {
							wakeups = append(wakeups, w)
							if now == 0 {
								now = gohacks.Nanotime()
							}
							// This definition of "wait time" skips the time
							// taken for w to wake up (bad), but avoids having
							// to lock apfl.mu again in apfl.awaitLoad()
							// (good).
							apfl.durWaitedTotal += time.Duration(now - w.timeStart)
							apfl.numWaiters--
							if apfl.numWaiters == 0 {
								apfl.durWaitedOne += time.Duration(now - apfl.timeStartWaiters)
								apfl.timeStartWaiters = math.MaxInt64
							}
						}
					}
				}
			}
			if logAwaitedLoads && haveWaiters {
				log.Infof("MemoryFile(%p): awaited opid %d complete, read %d bytes: %v", op.amfl.f, c.ID, op.total, op.frs)
			}
			// Keep amfl.minUnloaded up to date. We can only determine this
			// accurately if insertions into amfl.unloaded are complete.
			if amfl.lfDone {
				if amfl.unloaded.IsEmpty() {
					amfl.minUnloaded.Store(math.MaxUint64)
					apfl.amfls.Remove(amfl)
					if !amfl.sparse {
						amfl.f.asyncPageLoad.Store(nil)
					} else {
						amfl.f.removeSparseAsyncLoad(amfl)
						apfl.demandOnly.Store(false)
						apfl.lfStatus.Notify(aplLFPending)
					}
					amfl.timeline.End()
					if amfl.doneCallback != nil {
						amfl.doneCallback(nil)
						amfl.doneCallback = nil
					}
				} else {
					amfl.minUnloaded.Store(amfl.unloaded.FirstSegment().Start())
				}
			}
		}
		apfl.mu.Unlock()
		apfl.amflsMu.Unlock()
		for _, loaded := range loadedCallbacks {
			apfl.rangeLoaded(loaded.offset, loaded.length)
		}
		loadedCallbacks = loadedCallbacks[:0]
		for _, w := range wakeups {
			w.wakeup.Notify(1)
		}
		wakeups = wakeups[:0]
		dropDelayedDecRefs()
	}
}

// cancelUnusedLoads forgets the pages of frs that amfl has yet to load and no
// user of its file holds: nobody will await them. Pages being read stay until
// their read completes. amfl finishes if nothing is left for it to load.
//
// Preconditions: frs are page-aligned, and their pages hold only the
// reference of a sparse unit being released.
func (amfl *asyncMemoryFileLoad) cancelUnusedLoads(frs []memmap.FileRange) {
	apfl := amfl.pf
	apfl.amflsMu.Lock()
	defer apfl.amflsMu.Unlock()
	apfl.mu.Lock()
	defer apfl.mu.Unlock()
	removed := false
	for _, fr := range frs {
		ulseg := amfl.unloaded.LowerBoundSegment(fr.Start)
		for ulseg.Ok() && ulseg.Start() < fr.End {
			ulseg = amfl.unloaded.Isolate(ulseg, fr)
			if ul := ulseg.ValuePtr(); ul.started || len(ul.waiters) != 0 {
				ulseg = ulseg.NextSegment()
				continue
			}
			ulseg = amfl.unloaded.Remove(ulseg).NextSegment()
			removed = true
		}
	}
	if !removed || !amfl.lfDone {
		return
	}
	if !amfl.unloaded.IsEmpty() {
		amfl.minUnloaded.Store(amfl.unloaded.FirstSegment().Start())
		return
	}
	// As the loader finishes a sparse load it completes.
	amfl.minUnloaded.Store(math.MaxUint64)
	apfl.amfls.Remove(amfl)
	if !amfl.sparse {
		amfl.f.asyncPageLoad.Store(nil)
	} else {
		amfl.f.removeSparseAsyncLoad(amfl)
		apfl.demandOnly.Store(false)
		apfl.lfStatus.Notify(aplLFPending)
	}
	amfl.timeline.End()
	if amfl.doneCallback != nil {
		amfl.doneCallback(nil)
		amfl.doneCallback = nil
	}
}

// Preconditions:
// - All pages in fr must be becoming waste pages.
// - fr must be page-aligned.
func (amfl *asyncMemoryFileLoad) cancelWasteLoad(fr memmap.FileRange) {
	// Lockless fast path:
	if fr.End <= amfl.minUnloaded.Load() {
		return
	}

	amfl.pf.mu.Lock()
	amfl.unloaded.RemoveRangeWith(fr, func(ulseg aplUnloadedIterator) {
		ul := ulseg.ValuePtr()
		if ul.started {
			// This shouldn't be possible since page references are held while
			// reading (see MemoryFile.asyncPageLoadMain()).
			panic(fmt.Sprintf("pages %v becoming waste during inflight read from async loading", ulseg.Range()))
		}
		if n := len(ul.waiters); n != 0 {
			// This shouldn't be possible since the waiters should hold page
			// references.
			panic(fmt.Sprintf("pages %v becoming waste with %d async load waiters", ulseg.Range(), n))
		}
	})
	if amfl.unloaded.IsEmpty() {
		amfl.minUnloaded.Store(math.MaxUint64)
	} else {
		amfl.minUnloaded.Store(amfl.unloaded.FirstSegment().Start())
	}
	amfl.pf.mu.Unlock()
	// Wake the loader so it can detach an empty destination and close its
	// reader when no other destination has outstanding pages.
	amfl.pf.lfStatus.Notify(aplLFPending)
}

type aplUnloadedSetFunctions struct{}

func (aplUnloadedSetFunctions) MinKey() uint64 {
	return 0
}

func (aplUnloadedSetFunctions) MaxKey() uint64 {
	return math.MaxUint64
}

func (aplUnloadedSetFunctions) ClearValue(ul *aplUnloadedInfo) {
	ul.waiters = nil
}

func (aplUnloadedSetFunctions) Merge(fr1 memmap.FileRange, ul1 aplUnloadedInfo, fr2 memmap.FileRange, ul2 aplUnloadedInfo) (aplUnloadedInfo, bool) {
	if ul1.off+fr1.Length() != ul2.off || ul1.logicalChunk != ul2.logicalChunk || ul1.unit != ul2.unit || ul1.placed != ul2.placed {
		return aplUnloadedInfo{}, false
	}
	if ul1.started || ul2.started || len(ul1.waiters) != 0 || len(ul2.waiters) != 0 {
		// Merging would be counterproductive, since we expect that these
		// segments will shortly be removed (separately) based on AIO
		// completions, which would just necessitate splitting again.
		return aplUnloadedInfo{}, false
	}
	return ul1, true
}

func (aplUnloadedSetFunctions) Split(fr memmap.FileRange, ul aplUnloadedInfo, splitAt uint64) (aplUnloadedInfo, aplUnloadedInfo) {
	ul2 := aplUnloadedInfo{
		off:          ul.off + (splitAt - fr.Start),
		started:      ul.started,
		logicalChunk: ul.logicalChunk,
		unit:         ul.unit,
		placed:       ul.placed,
		// Setting cap(ul2.waiters) == len(ul2.waiters) makes ul2
		// "copy-on-append", saving an allocation if ul2 is never appended-to.
		// This is safe since existing elements in ul.waiters will never be
		// mutated.
		waiters: ul.waiters[:len(ul.waiters):len(ul.waiters)],
	}
	return ul, ul2
}

func (f *MemoryFile) getClientFileRangeSettings(fileSize uint64) []stateio.ClientFileRangeSetting {
	if !f.opts.AdviseHugepage && !f.opts.AdviseNoHugepage {
		return nil
	}
	var cfrs []stateio.ClientFileRangeSetting
	f.forEachChunk(memmap.FileRange{0, fileSize}, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		if chunk.huge {
			if f.opts.AdviseHugepage {
				cfrs = append(cfrs, stateio.ClientFileRangeSetting{
					FileRange: chunkFR,
					Property:  stateio.PropertyHugepage,
				})
			}
		} else {
			if f.opts.AdviseNoHugepage {
				cfrs = append(cfrs, stateio.ClientFileRangeSetting{
					FileRange: chunkFR,
					Property:  stateio.PropertyNoHugepage,
				})
			}
		}
		return true
	})
	return cfrs
}
