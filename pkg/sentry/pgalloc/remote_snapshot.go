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
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/timing"
)

// RemoteRegionID identifies a MemoryFile range within one RemoteSnapshot.
// IDs are stable for the lifetime of the snapshot and are carried by the
// distributed paging protocol.
type RemoteRegionID uint32

// RemoteSnapshotRegion describes an immutable fork-time MemoryFile range.
// Range must be non-empty and base-page-aligned.
type RemoteSnapshotRegion struct {
	ID      RemoteRegionID
	File    *MemoryFile
	Range   memmap.FileRange
	MemCgID uint32
}

type remoteSnapshotRegion struct {
	RemoteSnapshotRegion
	streamStart uint64
	released    []uint64
}

// RemoteSnapshot pins fork-time pages until they are installed by a remote
// target or the target is canceled. It also owns every referenced MemoryFile,
// allowing a target to finish pulling after the source Kernel is destroyed.
//
// RemoteSnapshot is safe for concurrent acquisition, acknowledgement, reads,
// and Close. Close prevents new acquisitions, releases unacknowledged pages,
// waits for active readers, and drops MemoryFile ownership.
type RemoteSnapshot struct {
	mu sync.Mutex

	regions      []remoteSnapshotRegion
	regionByID   map[RemoteRegionID]int
	streamBytes  uint64
	owners       []*MemoryFile
	closed       bool
	closeDone    chan struct{}
	active       sync.WaitGroup
	activeLeases uint64

	pagesTotal    uint64
	pagesReleased uint64
}

// RemoteSnapshotStats is a point-in-time snapshot of page ownership.
type RemoteSnapshotStats struct {
	PagesTotal       uint64
	PagesReleased    uint64
	PagesOutstanding uint64
	ActiveLeases     uint64
	Closed           bool
}

// NewRemoteSnapshot creates a snapshot and acquires one page reference for
// every region. Region order defines the packed stream exposed by
// NewLoopbackRemoteReader. It must not be called while the source may change
// its PMA ownership.
func NewRemoteSnapshot(regions []RemoteSnapshotRegion) (*RemoteSnapshot, error) {
	if len(regions) == 0 {
		return nil, errors.New("remote snapshot requires at least one region")
	}
	s := &RemoteSnapshot{
		regionByID: make(map[RemoteRegionID]int, len(regions)),
		regions:    make([]remoteSnapshotRegion, len(regions)),
		closeDone:  make(chan struct{}),
	}
	ownerSet := make(map[*MemoryFile]struct{})
	for i, region := range regions {
		if region.File == nil {
			return nil, fmt.Errorf("region %d has a nil MemoryFile", region.ID)
		}
		if !region.Range.WellFormed() || region.Range.Length() == 0 ||
			!hostarch.IsPageAligned(region.Range.Start) || !hostarch.IsPageAligned(region.Range.End) {
			return nil, fmt.Errorf("region %d has invalid range %v", region.ID, region.Range)
		}
		if _, ok := s.regionByID[region.ID]; ok {
			return nil, fmt.Errorf("duplicate remote region ID %d", region.ID)
		}
		if s.streamBytes > ^uint64(0)-region.Range.Length() {
			return nil, errors.New("remote snapshot stream size overflows uint64")
		}
		pages := region.Range.Length() / hostarch.PageSize
		s.regionByID[region.ID] = i
		s.regions[i] = remoteSnapshotRegion{
			RemoteSnapshotRegion: region,
			streamStart:          s.streamBytes,
			released:             make([]uint64, (pages+63)/64),
		}
		s.streamBytes += region.Range.Length()
		s.pagesTotal += pages
		ownerSet[region.File] = struct{}{}
	}
	for i := range s.regions {
		for j := 0; j < i; j++ {
			left, right := &s.regions[i], &s.regions[j]
			if left.File == right.File && left.Range.Start < right.Range.End && right.Range.Start < left.Range.End {
				return nil, fmt.Errorf("remote regions %d and %d overlap in one MemoryFile: %v and %v", left.ID, right.ID, left.Range, right.Range)
			}
		}
	}

	// Keep allocator mappings alive independently from the source Kernel.
	for file := range ownerSet {
		file.IncOwner()
		s.owners = append(s.owners, file)
	}
	for i := range s.regions {
		region := &s.regions[i]
		region.File.IncRef(region.Range, region.MemCgID)
	}
	return s, nil
}

// StreamBytes returns the size of the packed page stream.
func (s *RemoteSnapshot) StreamBytes() uint64 {
	return s.streamBytes
}

// Stats returns current ownership statistics.
func (s *RemoteSnapshot) Stats() RemoteSnapshotStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RemoteSnapshotStats{
		PagesTotal:       s.pagesTotal,
		PagesReleased:    s.pagesReleased,
		PagesOutstanding: s.pagesTotal - s.pagesReleased,
		ActiveLeases:     s.activeLeases,
		Closed:           s.closed,
	}
}

// RemotePageLease pins a page range while a sender copies its bytes. Close is
// idempotent.
type RemotePageLease struct {
	snapshot *RemoteSnapshot
	file     *MemoryFile
	range_   memmap.FileRange
	once     sync.Once
}

// File returns the lease's MemoryFile.
func (l *RemotePageLease) File() *MemoryFile { return l.file }

// Range returns the absolute MemoryFile range pinned by the lease.
func (l *RemotePageLease) Range() memmap.FileRange { return l.range_ }

// Close releases the temporary sender reference.
func (l *RemotePageLease) Close() error {
	l.once.Do(func() {
		l.file.DecRef(l.range_)
		l.snapshot.mu.Lock()
		l.snapshot.activeLeases--
		l.snapshot.mu.Unlock()
		l.snapshot.active.Done()
	})
	return nil
}

// AcquireRegion acquires a temporary sender reference for a relative range in
// region. It fails if any page has already been acknowledged or canceled.
func (s *RemoteSnapshot) AcquireRegion(id RemoteRegionID, relative memmap.FileRange) (*RemotePageLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, errors.New("remote snapshot is closed")
	}
	index, ok := s.regionByID[id]
	if !ok {
		return nil, fmt.Errorf("unknown remote region %d", id)
	}
	region := &s.regions[index]
	if !relative.WellFormed() || relative.Length() == 0 ||
		!hostarch.IsPageAligned(relative.Start) || !hostarch.IsPageAligned(relative.End) ||
		relative.End > region.Range.Length() {
		return nil, fmt.Errorf("invalid relative range %v for region %d of length %#x", relative, id, region.Range.Length())
	}
	if region.anyReleased(relative) {
		return nil, fmt.Errorf("region %d range %v has already been released", id, relative)
	}
	abs := memmap.FileRange{Start: region.Range.Start + relative.Start, End: region.Range.Start + relative.End}
	region.File.IncRef(abs, region.MemCgID)
	s.active.Add(1)
	s.activeLeases++
	return &RemotePageLease{snapshot: s, file: region.File, range_: abs}, nil
}

// ReleaseRegion acknowledges installation of relative pages by the target.
// Duplicate and overlapping acknowledgements are idempotent.
func (s *RemoteSnapshot) ReleaseRegion(id RemoteRegionID, relative memmap.FileRange) error {
	s.mu.Lock()
	index, ok := s.regionByID[id]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("unknown remote region %d", id)
	}
	region := &s.regions[index]
	if !relative.WellFormed() || relative.Length() == 0 ||
		!hostarch.IsPageAligned(relative.Start) || !hostarch.IsPageAligned(relative.End) ||
		relative.End > region.Range.Length() {
		s.mu.Unlock()
		return fmt.Errorf("invalid relative range %v for region %d of length %#x", relative, id, region.Range.Length())
	}
	released := region.markReleased(relative)
	for _, fr := range released {
		s.pagesReleased += fr.Length() / hostarch.PageSize
	}
	s.mu.Unlock()

	for _, relativeFR := range released {
		region.File.DecRef(memmap.FileRange{
			Start: region.Range.Start + relativeFR.Start,
			End:   region.Range.Start + relativeFR.End,
		})
	}
	return nil
}

// Close cancels all pages not yet acknowledged and releases snapshot
// ownership. It is idempotent. It may wait for active page copies.
func (s *RemoteSnapshot) Close() error {
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		return nil
	}
	s.closed = true
	type pendingRelease struct {
		file   *MemoryFile
		range_ memmap.FileRange
	}
	var pending []pendingRelease
	for i := range s.regions {
		region := &s.regions[i]
		all := memmap.FileRange{0, region.Range.Length()}
		for _, relativeFR := range region.markReleased(all) {
			s.pagesReleased += relativeFR.Length() / hostarch.PageSize
			pending = append(pending, pendingRelease{
				file: region.File,
				range_: memmap.FileRange{
					Start: region.Range.Start + relativeFR.Start,
					End:   region.Range.Start + relativeFR.End,
				},
			})
		}
	}
	s.mu.Unlock()

	for _, release := range pending {
		release.file.DecRef(release.range_)
	}
	s.active.Wait()
	for _, file := range s.owners {
		file.Destroy()
	}
	close(s.closeDone)
	return nil
}

func (r *remoteSnapshotRegion) anyReleased(relative memmap.FileRange) bool {
	start := relative.Start / hostarch.PageSize
	end := relative.End / hostarch.PageSize
	for page := start; page < end; page++ {
		if r.released[page/64]&(uint64(1)<<(page%64)) != 0 {
			return true
		}
	}
	return false
}

// markReleased marks relative pages and returns newly released relative runs.
func (r *remoteSnapshotRegion) markReleased(relative memmap.FileRange) []memmap.FileRange {
	start := relative.Start / hostarch.PageSize
	end := relative.End / hostarch.PageSize
	var ranges []memmap.FileRange
	runStart := ^uint64(0)
	for page := start; page < end; page++ {
		word := &r.released[page/64]
		mask := uint64(1) << (page % 64)
		if *word&mask == 0 {
			*word |= mask
			if runStart == ^uint64(0) {
				runStart = page
			}
			continue
		}
		if runStart != ^uint64(0) {
			ranges = append(ranges, memmap.FileRange{runStart * hostarch.PageSize, page * hostarch.PageSize})
			runStart = ^uint64(0)
		}
	}
	if runStart != ^uint64(0) {
		ranges = append(ranges, memmap.FileRange{runStart * hostarch.PageSize, end * hostarch.PageSize})
	}
	return ranges
}

// NewLoopbackRemoteReader returns an AsyncReader that copies pages directly
// from source MemoryFiles. It takes ownership of snapshot. Completion of a
// read acknowledges the copied pages before reporting the read complete.
func NewLoopbackRemoteReader(snapshot *RemoteSnapshot, maxReadBytes uint64, maxRanges, maxParallel int) stateio.AsyncReader {
	return stateio.NewIOReader(&remoteSnapshotReaderAt{snapshot: snapshot}, maxReadBytes, maxRanges, maxParallel)
}

type remoteSnapshotReaderAt struct {
	snapshot *RemoteSnapshot
}

// RemoteLoadRange maps a target MemoryFile range to an offset in the packed
// remote snapshot stream.
type RemoteLoadRange struct {
	Target       memmap.FileRange
	StreamOffset uint64
}

// AttachRemotePageLoad marks existing target pages as unloaded and attaches
// them to pages. Access through MapInternal or DataFD blocks until the relevant
// remote ranges have been installed. The caller must call
// AsyncPagesFileLoad.MemoryFilesDone after attaching every destination
// MemoryFile.
//
// The target ranges must already be allocated with at least one reference.
func (f *MemoryFile) AttachRemotePageLoad(pages *AsyncPagesFileLoad, ranges []RemoteLoadRange, doneCallback func(error), timeline *timing.Timeline) error {
	if pages == nil {
		return errors.New("remote page loader is nil")
	}
	if len(ranges) == 0 {
		return errors.New("remote page loader requires at least one target range")
	}
	for i, loadRange := range ranges {
		if !loadRange.Target.WellFormed() || loadRange.Target.Length() == 0 ||
			!hostarch.IsPageAligned(loadRange.Target.Start) || !hostarch.IsPageAligned(loadRange.Target.End) ||
			!hostarch.IsPageAligned(loadRange.StreamOffset) {
			return fmt.Errorf("remote load range %d is invalid: target=%v stream offset=%#x", i, loadRange.Target, loadRange.StreamOffset)
		}
		for j := 0; j < i; j++ {
			other := ranges[j].Target
			if loadRange.Target.Start < other.End && other.Start < loadRange.Target.End {
				return fmt.Errorf("remote target ranges %v and %v overlap", loadRange.Target, other)
			}
		}
	}

	fileSize := uint64(len(f.chunksLoad())) * chunkSize
	var destination stateio.DestinationFile
	if pages.ar.NeedRegisterDestinationFD() {
		var err error
		destination, err = pages.ar.RegisterDestinationFD(int32(f.file.Fd()), fileSize, f.getClientFileRangeSettings(fileSize))
		if err != nil {
			return fmt.Errorf("registering remote destination MemoryFile: %w", err)
		}
	}
	amfl := &asyncMemoryFileLoad{
		f:            f,
		pf:           pages,
		df:           destination,
		doneCallback: doneCallback,
		timeline:     timeline.Fork(fmt.Sprintf("remote-mf:%p", f)),
	}

	pages.amflsMu.Lock()
	if err := pages.err(); err != nil {
		pages.amflsMu.Unlock()
		amfl.timeline.End()
		return err
	}
	if !f.asyncPageLoad.CompareAndSwap(nil, amfl) {
		pages.amflsMu.Unlock()
		amfl.timeline.End()
		return errors.New("MemoryFile already has an asynchronous page load")
	}
	pages.amfls.PushBack(amfl)
	pages.amflsMu.Unlock()

	minUnloaded := uint64(^uint64(0))
	pages.mu.Lock()
	for _, loadRange := range ranges {
		amfl.unloaded.InsertRange(loadRange.Target, aplUnloadedInfo{off: loadRange.StreamOffset})
		if loadRange.Target.Start < minUnloaded {
			minUnloaded = loadRange.Target.Start
		}
	}
	amfl.minUnloaded.Store(minUnloaded)
	amfl.lfDone = true
	pages.mu.Unlock()
	pages.lfStatus.Notify(aplLFPending)
	return nil
}

func (r *remoteSnapshotReaderAt) Close() error {
	return r.snapshot.Close()
}

func (r *remoteSnapshotReaderAt) ReadAt(dst []byte, off int64) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if off < 0 || !hostarch.IsPageAligned(uint64(off)) || !hostarch.IsPageAligned(uint64(len(dst))) {
		return 0, fmt.Errorf("remote read must be page-aligned: off=%d len=%d", off, len(dst))
	}
	streamOff := uint64(off)
	if streamOff >= r.snapshot.streamBytes || uint64(len(dst)) > r.snapshot.streamBytes-streamOff {
		return 0, io.EOF
	}

	written := 0
	for written < len(dst) {
		regionIndex := sort.Search(len(r.snapshot.regions), func(i int) bool {
			region := &r.snapshot.regions[i]
			return region.streamStart+region.Range.Length() > streamOff
		})
		if regionIndex == len(r.snapshot.regions) {
			return written, io.ErrUnexpectedEOF
		}
		region := &r.snapshot.regions[regionIndex]
		relativeStart := streamOff - region.streamStart
		length := min(uint64(len(dst)-written), region.Range.Length()-relativeStart)
		relative := memmap.FileRange{relativeStart, relativeStart + length}
		lease, err := r.snapshot.AcquireRegion(region.ID, relative)
		if err != nil {
			return written, err
		}
		src, err := lease.File().MapInternal(lease.Range(), hostarch.Read)
		if err != nil {
			lease.Close()
			return written, err
		}
		dstSeq := safemem.BlockSeqOf(safemem.BlockFromSafeSlice(dst[written : written+int(length)]))
		copied, copyErr := safemem.CopySeq(dstSeq, src)
		if copyErr == nil && copied != length {
			copyErr = io.ErrUnexpectedEOF
		}
		if copyErr == nil {
			copyErr = r.snapshot.ReleaseRegion(region.ID, relative)
		}
		lease.Close()
		written += int(copied)
		if copyErr != nil {
			return written, copyErr
		}
		streamOff += length
	}
	return written, nil
}
