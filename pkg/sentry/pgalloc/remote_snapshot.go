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
	"bytes"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sort"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/state"
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
	// mu is shared by sibling snapshots captured from one instant. Sharing the
	// lock lets the last consumer of a page release one common source reference
	// without a per-page consumer counter or cross-snapshot lock ordering.
	mu *sync.Mutex

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
	pushHints     chan RemotePushHint
	pushEnqueued  uint64
	pushDropped   uint64
	shared        *remoteSnapshotShared
}

// remoteSnapshotShared owns the single set of MemoryFile references retained
// for one capture instant. Each target has its own RemoteSnapshot release
// bitmap and transport state, but source pages are released only after every
// still-live target has acknowledged or canceled them.
type remoteSnapshotShared struct {
	views  map[*RemoteSnapshot]struct{}
	owners []*MemoryFile
	active sync.WaitGroup
}

// RemotePushHint identifies fork-time bytes that became especially valuable
// to transfer because the source just broke COW on the corresponding range.
// Hints are advisory and may be coalesced, reordered, or dropped.
type RemotePushHint struct {
	StreamOffset uint64
	Length       uint64
}

// SyncAndDropUniqueCache persists and evicts pages whose only remaining
// MemoryFile reference is held by s. Pages shared with another live branch are
// intentionally retained.
func (s *RemoteSnapshot) SyncAndDropUniqueCache() error {
	if s == nil {
		return errors.New("remote snapshot is nil")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("remote snapshot is closed")
	}
	byFile := make(map[*MemoryFile][]memmap.FileRange)
	for _, region := range s.regions {
		byFile[region.File] = append(byFile[region.File], region.Range)
	}
	s.mu.Unlock()
	var result error
	for file, ranges := range byFile {
		if err := file.SyncAndDropUniqueCache(ranges); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

// SyncAndDropSnapshotCache persists and evicts pages whose references are
// owned entirely by the given stable snapshots. This includes pages shared by
// multiple dehydrated branches, while retaining pages referenced by any live
// Kernel or unrelated remote transfer.
func SyncAndDropSnapshotCache(snapshots []*RemoteSnapshot) error {
	type event struct {
		offset uint64
		delta  int64
	}
	byFile := make(map[*MemoryFile][]event)
	seenShared := make(map[*remoteSnapshotShared]struct{})
	for _, snapshot := range snapshots {
		if snapshot == nil {
			return errors.New("remote snapshot is nil")
		}
		snapshot.mu.Lock()
		if snapshot.closed {
			snapshot.mu.Unlock()
			return errors.New("remote snapshot is closed")
		}
		if snapshot.pagesReleased != 0 || len(snapshot.shared.views) != 1 {
			snapshot.mu.Unlock()
			return errors.New("cache eviction requires an unreleased single-view snapshot")
		}
		if _, ok := seenShared[snapshot.shared]; !ok {
			seenShared[snapshot.shared] = struct{}{}
			for _, region := range snapshot.regions {
				byFile[region.File] = append(byFile[region.File],
					event{offset: region.Range.Start, delta: 1},
					event{offset: region.Range.End, delta: -1})
			}
		}
		snapshot.mu.Unlock()
	}
	var result error
	for file, events := range byFile {
		sort.Slice(events, func(i, j int) bool { return events[i].offset < events[j].offset })
		var candidates []cacheDropCandidate
		var refs int64
		for i := 0; i < len(events); {
			offset := events[i].offset
			if i != 0 && events[i-1].offset < offset && refs > 0 {
				candidates = append(candidates, cacheDropCandidate{
					range_:       memmap.FileRange{Start: events[i-1].offset, End: offset},
					expectedRefs: uint64(refs),
				})
			}
			for i < len(events) && events[i].offset == offset {
				refs += events[i].delta
				i++
			}
		}
		if refs != 0 {
			return errors.New("unbalanced snapshot cache ranges")
		}
		if err := file.syncAndDropCache(candidates); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}

// StartLoopbackRemotePush starts a non-blocking proactive-push pump for local
// distributed-fork testing. Pull owns any range that has already started;
// otherwise the pump installs bytes and only then acknowledges the source.
// The returned channel closes when snapshot is closed.
func StartLoopbackRemotePush(snapshot *RemoteSnapshot, pages *AsyncPagesFileLoad, maxChunk uint64) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for hint := range snapshot.PushHints() {
			for off, remaining := hint.StreamOffset, hint.Length; remaining != 0; {
				length := min(remaining, maxChunk)
				length = hostarch.PageRoundDown(length)
				if length == 0 {
					length = hostarch.PageSize
				}
				data := make([]byte, length)
				if n, err := snapshot.CopyStreamAt(data, int64(off)); err == nil && uint64(n) == length {
					if installed, err := pages.TryInstallRemotePages(off, data); err == nil && installed {
						_ = snapshot.AcknowledgeStreamRange(off, length)
					}
				}
				off += length
				remaining -= length
			}
		}
	}()
	return done
}

// RemoteSnapshotStats is a point-in-time snapshot of page ownership.
type RemoteSnapshotStats struct {
	PagesTotal       uint64
	PagesReleased    uint64
	PagesOutstanding uint64
	ActiveLeases     uint64
	Closed           bool
	PushEnqueued     uint64
	PushDropped      uint64
}

// NewRemoteSnapshot creates a snapshot and acquires one page reference for
// every region. Region order defines the packed stream exposed by
// NewLoopbackRemoteReader. It must not be called while the source may change
// its PMA ownership.
func NewRemoteSnapshot(regions []RemoteSnapshotRegion) (*RemoteSnapshot, error) {
	snapshots, err := NewRemoteSnapshots(regions, 1)
	if err != nil {
		return nil, err
	}
	return snapshots[0], nil
}

// NewRemoteSnapshots creates count independently readable views of one
// fork-time snapshot. It acquires one source page reference, rather than one
// reference per destination. This makes sibling fan-out add only release
// bitmaps and transport state to the source pause path.
func NewRemoteSnapshots(regions []RemoteSnapshotRegion, count int) ([]*RemoteSnapshot, error) {
	if len(regions) == 0 {
		return nil, errors.New("remote snapshots require at least one region")
	}
	if count <= 0 {
		return nil, fmt.Errorf("remote snapshot count must be positive, got %d", count)
	}
	var streamBytes, pagesTotal uint64
	regionByID := make(map[RemoteRegionID]int, len(regions))
	ownerSet := make(map[*MemoryFile]struct{})
	for i, region := range regions {
		if region.File == nil {
			return nil, fmt.Errorf("region %d has a nil MemoryFile", region.ID)
		}
		if !region.Range.WellFormed() || region.Range.Length() == 0 ||
			!hostarch.IsPageAligned(region.Range.Start) || !hostarch.IsPageAligned(region.Range.End) {
			return nil, fmt.Errorf("region %d has invalid range %v", region.ID, region.Range)
		}
		if _, ok := regionByID[region.ID]; ok {
			return nil, fmt.Errorf("duplicate remote region ID %d", region.ID)
		}
		if streamBytes > ^uint64(0)-region.Range.Length() {
			return nil, errors.New("remote snapshot stream size overflows uint64")
		}
		regionByID[region.ID] = i
		streamBytes += region.Range.Length()
		pagesTotal += region.Range.Length() / hostarch.PageSize
		ownerSet[region.File] = struct{}{}
	}
	for i := range regions {
		for j := 0; j < i; j++ {
			left, right := &regions[i], &regions[j]
			if left.File == right.File && left.Range.Start < right.Range.End && right.Range.Start < left.Range.End {
				return nil, fmt.Errorf("remote regions %d and %d overlap in one MemoryFile: %v and %v", left.ID, right.ID, left.Range, right.Range)
			}
		}
	}
	shared := &remoteSnapshotShared{views: make(map[*RemoteSnapshot]struct{}, count)}
	mu := &sync.Mutex{}
	snapshots := make([]*RemoteSnapshot, count)
	for snapshotIndex := range snapshots {
		s := &RemoteSnapshot{
			mu:          mu,
			regionByID:  regionByID,
			regions:     make([]remoteSnapshotRegion, len(regions)),
			streamBytes: streamBytes,
			closeDone:   make(chan struct{}),
			pagesTotal:  pagesTotal,
			pushHints:   make(chan RemotePushHint, 1024),
			shared:      shared,
		}
		var streamStart uint64
		for i, region := range regions {
			pages := region.Range.Length() / hostarch.PageSize
			s.regions[i] = remoteSnapshotRegion{
				RemoteSnapshotRegion: region,
				streamStart:          streamStart,
				released:             make([]uint64, (pages+63)/64),
			}
			streamStart += region.Range.Length()
		}
		snapshots[snapshotIndex] = s
		shared.views[s] = struct{}{}
	}

	// Keep allocator mappings alive independently from the source Kernel.
	for file := range ownerSet {
		file.IncOwner()
		shared.owners = append(shared.owners, file)
	}
	for i := range regions {
		region := &regions[i]
		region.File.IncRef(region.Range, region.MemCgID)
	}
	for _, s := range snapshots {
		s.owners = shared.owners
		for _, file := range shared.owners {
			file.registerRemoteSnapshot(s)
		}
	}
	return snapshots, nil
}

// ExportMemoryFileMetadataForRemote captures allocator metadata and the packed
// page-stream layout without reading page contents. The returned bytes contain
// no process-local pointers and may be sent to another Sentry process.
func ExportMemoryFileMetadataForRemote(ctx context.Context, source *MemoryFile) ([]byte, []RemoteSnapshotRegion, []RemoteLoadRange, error) {
	if source == nil {
		return nil, nil, nil, errors.New("source MemoryFile is nil")
	}
	var metadata bytes.Buffer
	var used []memmap.FileRange
	source.mu.Lock()
	for source.haveWaste {
		source.mu.Unlock()
		runtime.Gosched()
		source.mu.Lock()
	}
	if len(source.evictable) != 0 {
		source.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("source MemoryFile has %d pending evictable users", len(source.evictable))
	}
	collectUsed := func(set *unfreeSet) {
		for seg := set.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
			if seg.ValuePtr().refs != 0 {
				used = append(used, seg.Range())
			}
		}
	}
	collectUsed(&source.unfreeSmall)
	collectUsed(&source.unfreeHuge)
	if _, err := state.Save(ctx, &metadata, &memoryFileSaved{
		unwasteSmall: &source.unwasteSmall,
		unwasteHuge:  &source.unwasteHuge,
		unfreeSmall:  &source.unfreeSmall,
		unfreeHuge:   &source.unfreeHuge,
		subreleased:  source.subreleased,
		memAcct:      &source.memAcct,
		chunks:       source.chunksLoad(),
	}); err != nil {
		source.mu.Unlock()
		return nil, nil, nil, fmt.Errorf("saving remote MemoryFile metadata: %w", err)
	}
	source.mu.Unlock()

	sort.Slice(used, func(i, j int) bool { return used[i].Start < used[j].Start })
	merged := used[:0]
	for _, fr := range used {
		if len(merged) == 0 || merged[len(merged)-1].End < fr.Start {
			merged = append(merged, fr)
			continue
		}
		last := &merged[len(merged)-1]
		if fr.Start < last.End {
			return nil, nil, nil, fmt.Errorf("source MemoryFile used ranges overlap: %v and %v", *last, fr)
		}
		last.End = fr.End
	}
	used = merged
	regions := make([]RemoteSnapshotRegion, len(used))
	loads := make([]RemoteLoadRange, len(used))
	var streamOffset uint64
	for i, fr := range used {
		regions[i] = RemoteSnapshotRegion{ID: RemoteRegionID(i + 1), File: source, Range: fr}
		loads[i] = RemoteLoadRange{Target: fr, StreamOffset: streamOffset}
		streamOffset += fr.Length()
	}
	return metadata.Bytes(), regions, loads, nil
}

// ImportMemoryFileMetadataForRemote reconstructs allocator metadata exported
// by ExportMemoryFileMetadataForRemote in a newly created empty MemoryFile.
// Page contents remain absent until a remote page loader is attached.
func ImportMemoryFileMetadataForRemote(ctx context.Context, target *MemoryFile, metadata []byte) error {
	if target == nil {
		return errors.New("target MemoryFile is nil")
	}
	target.mu.Lock()
	targetEmpty := len(target.chunksLoad()) == 0
	target.mu.Unlock()
	if !targetEmpty {
		return errors.New("remote target MemoryFile is not empty")
	}

	var saved memoryFileSaved
	if _, err := state.Load(ctx, bytes.NewReader(metadata), &saved); err != nil {
		return fmt.Errorf("loading remote MemoryFile metadata: %w", err)
	}
	chunks := saved.chunks
	fileSize := uint64(len(chunks)) * chunkSize
	if err := target.file.Truncate(int64(fileSize)); err != nil {
		return fmt.Errorf("sizing remote target MemoryFile: %w", err)
	}
	if len(chunks) != 0 {
		mapping, _, errno := unix.Syscall6(
			unix.SYS_MMAP,
			0,
			uintptr(fileSize),
			unix.PROT_READ|unix.PROT_WRITE,
			unix.MAP_SHARED,
			target.file.Fd(),
			0)
		if errno != 0 {
			return fmt.Errorf("mapping remote target MemoryFile: %w", errno)
		}
		for i := range chunks {
			chunks[i].mapping = mapping + uintptr(i)*chunkSize
			target.madviseChunkMapping(chunks[i].mapping, chunkSize, chunks[i].huge)
		}
	}

	var knownCommitted uint64
	for seg := saved.memAcct.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		if seg.ValuePtr().knownCommitted {
			knownCommitted += seg.Range().Length()
		}
	}
	target.mu.Lock()
	target.unwasteSmall.MoveFrom(saved.unwasteSmall)
	target.unwasteHuge.MoveFrom(saved.unwasteHuge)
	target.unfreeSmall.MoveFrom(saved.unfreeSmall)
	target.unfreeHuge.MoveFrom(saved.unfreeHuge)
	target.subreleased = saved.subreleased
	target.memAcct.MoveFrom(saved.memAcct)
	target.knownCommittedBytes = knownCommitted
	target.chunks.Store(&chunks)
	target.mu.Unlock()
	if !target.opts.DisableMemoryAccounting {
		for seg := target.memAcct.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
			if seg.ValuePtr().knownCommitted {
				usage.MemoryAccounting.Inc(seg.Range().Length(), seg.ValuePtr().kind, seg.ValuePtr().memCgID)
			}
		}
	}

	return nil
}

// CloneMemoryFileMetadataForRemote is the same-process composition of Export
// and Import. target must be newly created and empty, and source users must be
// externally stopped while its reference graph is captured.
func CloneMemoryFileMetadataForRemote(ctx context.Context, source, target *MemoryFile) ([]RemoteSnapshotRegion, []RemoteLoadRange, error) {
	if source == target {
		return nil, nil, errors.New("remote MemoryFile clone requires distinct source and target")
	}
	metadata, regions, loads, err := ExportMemoryFileMetadataForRemote(ctx, source)
	if err != nil {
		return nil, nil, err
	}
	if err := ImportMemoryFileMetadataForRemote(ctx, target, metadata); err != nil {
		return nil, nil, err
	}
	return regions, loads, nil
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
		PushEnqueued:     s.pushEnqueued,
		PushDropped:      s.pushDropped,
	}
}

// OutstandingStreamRanges returns packed-stream ranges that still require the
// target. Each range is at most maxRange bytes. It is a point-in-time advisory
// snapshot: concurrent pulls may acknowledge ranges immediately afterward.
func (s *RemoteSnapshot) OutstandingStreamRanges(maxRange uint64) ([]RemotePushHint, error) {
	if maxRange < hostarch.PageSize || !hostarch.IsPageAligned(maxRange) {
		return nil, fmt.Errorf("invalid outstanding remote range limit %d", maxRange)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil
	}
	maxPages := maxRange / hostarch.PageSize
	var ranges []RemotePushHint
	for i := range s.regions {
		region := &s.regions[i]
		pages := region.Range.Length() / hostarch.PageSize
		for page := uint64(0); page < pages; {
			if region.released[page/64]&(uint64(1)<<(page%64)) != 0 {
				page++
				continue
			}
			start := page
			for page < pages && page-start < maxPages && region.released[page/64]&(uint64(1)<<(page%64)) == 0 {
				page++
			}
			ranges = append(ranges, RemotePushHint{
				StreamOffset: region.streamStart + start*hostarch.PageSize,
				Length:       (page - start) * hostarch.PageSize,
			})
		}
	}
	return ranges, nil
}

// PushHints returns a bounded stream of advisory proactive-push candidates.
func (s *RemoteSnapshot) PushHints() <-chan RemotePushHint { return s.pushHints }

func (s *RemoteSnapshot) notifySourceCOW(file *MemoryFile, fr memmap.FileRange) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	for i := range s.regions {
		region := &s.regions[i]
		if region.File != file || fr.Start >= region.Range.End || region.Range.Start >= fr.End {
			continue
		}
		intersection := fr.Intersect(region.Range)
		relative := memmap.FileRange{intersection.Start - region.Range.Start, intersection.End - region.Range.Start}
		if region.anyReleased(relative) {
			// Released subranges do not need another transfer. Splitting around
			// them is left to the consumer's idempotent ACK bitmap.
			continue
		}
		hint := RemotePushHint{
			StreamOffset: region.streamStart + relative.Start,
			Length:       relative.Length(),
		}
		select {
		case s.pushHints <- hint:
			s.pushEnqueued++
		default:
			s.pushDropped++
		}
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
		l.snapshot.shared.active.Done()
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
	s.shared.active.Add(1)
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
	pending := s.sharedReleasesLocked(index, released)
	s.mu.Unlock()

	for _, release := range pending {
		release.file.DecRef(release.range_)
	}
	return nil
}

type remoteSnapshotRelease struct {
	file   *MemoryFile
	range_ memmap.FileRange
}

// sharedReleasesLocked returns source ranges for which this update satisfied
// the last sibling. All sibling release bitmaps are protected by their common
// mutex, so no per-page destination counter is needed.
func (s *RemoteSnapshot) sharedReleasesLocked(regionIndex int, newlyReleased []memmap.FileRange) []remoteSnapshotRelease {
	region := &s.regions[regionIndex]
	var releases []remoteSnapshotRelease
	for _, relative := range newlyReleased {
		start := relative.Start / hostarch.PageSize
		end := relative.End / hostarch.PageSize
		runStart := ^uint64(0)
		for page := start; page < end; page++ {
			allReleased := true
			for view := range s.shared.views {
				other := &view.regions[regionIndex]
				if other.released[page/64]&(uint64(1)<<(page%64)) == 0 {
					allReleased = false
					break
				}
			}
			if allReleased {
				if runStart == ^uint64(0) {
					runStart = page
				}
				continue
			}
			if runStart != ^uint64(0) {
				releases = append(releases, remoteSnapshotRelease{
					file: region.File,
					range_: memmap.FileRange{Start: region.Range.Start + runStart*hostarch.PageSize,
						End: region.Range.Start + page*hostarch.PageSize},
				})
				runStart = ^uint64(0)
			}
		}
		if runStart != ^uint64(0) {
			releases = append(releases, remoteSnapshotRelease{
				file: region.File,
				range_: memmap.FileRange{Start: region.Range.Start + runStart*hostarch.PageSize,
					End: region.Range.Start + end*hostarch.PageSize},
			})
		}
	}
	return releases
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
	close(s.pushHints)
	var pending []remoteSnapshotRelease
	for i := range s.regions {
		region := &s.regions[i]
		all := memmap.FileRange{0, region.Range.Length()}
		newlyReleased := region.markReleased(all)
		for _, relativeFR := range newlyReleased {
			s.pagesReleased += relativeFR.Length() / hostarch.PageSize
		}
		pending = append(pending, s.sharedReleasesLocked(i, newlyReleased)...)
	}
	delete(s.shared.views, s)
	lastView := len(s.shared.views) == 0
	s.mu.Unlock()
	for _, file := range s.owners {
		file.unregisterRemoteSnapshot(s)
	}

	for _, release := range pending {
		release.file.DecRef(release.range_)
	}
	s.active.Wait()
	if lastView {
		s.shared.active.Wait()
		for _, file := range s.owners {
			file.Destroy()
		}
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
	written, err := r.snapshot.CopyStreamAt(dst, off)
	if err != nil {
		return written, err
	}
	if err := r.snapshot.AcknowledgeStreamRange(uint64(off), uint64(len(dst))); err != nil {
		return written, err
	}
	return written, nil
}

// CopyStreamAt copies an aligned packed-stream range while holding temporary
// sender references. It does not release the snapshot references; the caller
// must call AcknowledgeStreamRange only after the target installs the bytes.
func (s *RemoteSnapshot) CopyStreamAt(dst []byte, off int64) (int, error) {
	if len(dst) == 0 {
		return 0, nil
	}
	if off < 0 || !hostarch.IsPageAligned(uint64(off)) || !hostarch.IsPageAligned(uint64(len(dst))) {
		return 0, fmt.Errorf("remote read must be page-aligned: off=%d len=%d", off, len(dst))
	}
	streamOff := uint64(off)
	if streamOff >= s.streamBytes || uint64(len(dst)) > s.streamBytes-streamOff {
		return 0, io.EOF
	}

	written := 0
	for written < len(dst) {
		regionIndex := sort.Search(len(s.regions), func(i int) bool {
			region := &s.regions[i]
			return region.streamStart+region.Range.Length() > streamOff
		})
		if regionIndex == len(s.regions) {
			return written, io.ErrUnexpectedEOF
		}
		region := &s.regions[regionIndex]
		relativeStart := streamOff - region.streamStart
		length := min(uint64(len(dst)-written), region.Range.Length()-relativeStart)
		relative := memmap.FileRange{relativeStart, relativeStart + length}
		lease, err := s.AcquireRegion(region.ID, relative)
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
		lease.Close()
		written += int(copied)
		if copyErr != nil {
			return written, copyErr
		}
		streamOff += length
	}
	return written, nil
}

// AcknowledgeStreamRange releases source snapshot references for an aligned
// packed-stream range after the target has installed it. Duplicate and
// overlapping acknowledgements are idempotent.
func (s *RemoteSnapshot) AcknowledgeStreamRange(streamOff, length uint64) error {
	if length == 0 || !hostarch.IsPageAligned(streamOff) || !hostarch.IsPageAligned(length) ||
		streamOff >= s.streamBytes || length > s.streamBytes-streamOff {
		return fmt.Errorf("invalid remote acknowledgement: off=%#x len=%#x stream=%#x", streamOff, length, s.streamBytes)
	}
	remaining := length
	for remaining != 0 {
		regionIndex := sort.Search(len(s.regions), func(i int) bool {
			region := &s.regions[i]
			return region.streamStart+region.Range.Length() > streamOff
		})
		if regionIndex == len(s.regions) {
			return io.ErrUnexpectedEOF
		}
		region := &s.regions[regionIndex]
		relativeStart := streamOff - region.streamStart
		part := min(remaining, region.Range.Length()-relativeStart)
		if err := s.ReleaseRegion(region.ID, memmap.FileRange{relativeStart, relativeStart + part}); err != nil {
			return err
		}
		streamOff += part
		remaining -= part
	}
	return nil
}
