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

// Package pgalloc contains the page allocator subsystem, which provides
// allocatable memory that may be mapped into application address spaces.
//
// Lock order:
//
//	AsyncPagesFileLoad.amflsMu
//	  MemoryFile.mu
//	    AsyncPagesFileLoad.mu
package pgalloc

import (
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/atomicbitops"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/rand"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/hostmm"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sync"
)

const pagesPerHugePage = hostarch.HugePageSize / hostarch.PageSize

// MemoryFile is a memmap.File whose pages may be allocated to arbitrary
// users.
type MemoryFile struct {
	memmap.DefaultMemoryType
	memmap.NoBufferedIOFallback

	// MemoryFile owns a single backing file. Each page in the backing file is
	// considered "committed" or "uncommitted". A page is committed if the host
	// kernel is spending resources to store its contents and uncommitted
	// otherwise. This definition includes pages that the host kernel has
	// swapped. This is intentional; it means that committed pages can only
	// become uncommitted as a result of MemoryFile's actions, such that page
	// commitment does not change even if host kernel swapping behavior changes.
	//
	// Each page in the MemoryFile is in one of the following logical states,
	// protected by mu:
	//
	// - Void: Pages beyond the backing file's current size cannot store data.
	// Void pages are uncommitted. Extending the file's size transitions pages
	// between the old and new sizes from void to free.
	//
	// - Free: Free pages are immediately allocatable. Free pages are
	// uncommitted, and implicitly zeroed. Free pages become used when they are
	// allocated.
	//
	// - Used: Used pages have been allocated and currently have a non-zero
	// reference count. Used pages may transition from uncommitted to committed
	// outside of MemoryFile's control, but can only transition from committed
	// to uncommitted via MemoryFile.Decommit(). The content of used pages is
	// unknown. Used pages become waste when their reference count becomes
	// zero.
	//
	// - Waste: Waste pages have no users, but cannot be immediately
	// reallocated since their commitment state and content is unknown. Waste
	// pages may be uncommitted or committed, but cannot transition between the
	// two. MemoryFile's releaser goroutine transitions pages from waste to
	// releasing. Allocations that may return committed pages can transition
	// pages from waste to used (referred to as "recycling").
	//
	// - Releasing: Releasing pages are waste pages that the releaser goroutine
	// has removed from waste-tracking, making them ineligible for recycling.
	// The releaser decommits releasing pages without holding mu, then
	// transitions them back to free or sub-released with mu locked.
	//
	// - Sub-release: Sub-released pages are released small pages within a
	// huge-page-backed allocation where the containing huge page as a whole
	// has not yet been released, which can arise because references are still
	// counted at page granularity within huge-page-backed ranges. Sub-released
	// pages cannot be used for allocations until release of the whole
	// containing huge page causes it to transition it to free. We assume that
	// sub-released pages are uncommitted; this isn't necessarily true (see
	// discussion of khugepaged elsewhere in this file), but the assumption is
	// consistent with legacy behavior.

	mu memoryFileMutex

	// remoteCOWMu protects remoteCOWSnapshots. It is independent of mu so COW
	// notifications can be emitted only after MemoryManager locks are dropped.
	remoteCOWMu        sync.RWMutex
	remoteCOWSnapshots map[*RemoteSnapshot]struct{}

	// unwasteSmall and unwasteHuge track waste ranges backed by small/huge pages
	// respectively. Both sets are "inverted"; segments exist for all ranges that
	// are *not* waste, allowing use of segment.Set gap-tracking to efficiently
	// find ranges for both release and recycling allocations.
	//
	// unwasteSmall and unwasteHuge are protected by mu.
	unwasteSmall unwasteSet
	unwasteHuge  unwasteSet

	// haveWaste is true if there may be at least one waste page in the
	// MemoryFile.
	//
	// haveWaste is protected by mu.
	haveWaste bool

	// releaseCond is signaled (with mu locked) when haveWaste or destroyed
	// transitions from false to true.
	releaseCond sync.Cond

	// unfreeSmall and unfreeHuge track information for non-free ranges backed
	// by small/huge pages respectively. Each unfreeSet also contains segments
	// representing chunks that are backed by a different page size. Gaps in
	// the sets therefore represent free ranges backed by small/huge pages,
	// allowing use of segment.Set gap-tracking to efficiently find free ranges
	// for allocation.
	//
	// unfreeSmall and unfreeHuge are protected by mu.
	unfreeSmall unfreeSet
	unfreeHuge  unfreeSet

	// logicalPages maps used backing-file offsets to stable logical page IDs.
	// Unlike backing-file offsets, IDs are never reused. Contiguous IDs are
	// stored as runs, so ordinary multi-page allocations add one segment rather
	// than one metadata object per page.
	//
	// logicalAllocator, nextLogicalPage, nextPageVersion, and logicalPages are
	// protected by mu.
	logicalAllocator LogicalAllocatorID
	nextLogicalPage  uint64
	nextPageVersion  uint64
	logicalPages     logicalPageSet

	// sparseChunks maps immutable TTC chunk objects to their resident 1 MiB
	// backing ranges. Each entry owns one reference on every page in Range;
	// branch object graphs own additional references on valid slots. The map is
	// runtime-local and protected by mu.
	sparseChunks map[SparseChunkObjectID]sparseResidentChunk `state:"nosave"`

	// reclaimedSparseChunks has the sparse chunks ReclaimUnusedSparseChunks
	// released, since they were last installed: no user of the file held any
	// of their pages. It is protected by mu.
	reclaimedSparseChunks map[SparseChunkObjectID]struct{} `state:"nosave"`

	// sparseInstalling counts, by unit, the installations that install or
	// reuse it and are not yet committed or aborted. It is protected by mu.
	sparseInstalling map[SparseChunkObjectID]int `state:"nosave"`

	// subreleased maps hugepage-aligned file offsets to the number of
	// sub-released small pages within the hugepage beginning at that offset.
	// subreleased is protected by mu.
	subreleased map[uint64]uint64

	// These fields are used for memory accounting.
	//
	// Memory accounting is based on identifying the set of committed pages.
	// Since we do not have direct access to application page tables (on most
	// platforms), tracking application accesses to uncommitted pages to detect
	// commitment would introduce additional page faults, which would be
	// prohibitively expensive. Instead, we query the host kernel to determine
	// which pages are committed.
	//
	// memAcct tracks memory accounting state, including commitment status, for
	// each page. Non-empty gaps in memAcct represent pages known to be
	// uncommitted (void, free, and sub-released pages).
	//
	// knownCommittedBytes is the number of bytes in the file known to be
	// committed, i.e. the span of all segments in memAcct for which
	// knownCommitted is true.
	//
	// commitSeq is a sequence counter used to detect races between scans for
	// committed pages and concurrent decommitment.
	//
	// nextCommitScan is the next time at which UpdateUsage(nil) may scan the
	// backing file for commitment information.
	//
	// ongoingCommitScan is true while UpdateUsage(nil) is in progress. Writing
	// to it requires f.mu to be locked, but reading it does not.
	//
	// isSaving is true during f.SaveTo() to prevent concurrent calls to
	// f.UpdateUsage() from marking pages as committed.
	//
	// All of these fields are protected by mu.
	memAcct             memAcctSet
	knownCommittedBytes uint64
	commitSeq           uint64
	nextCommitScan      time.Time
	ongoingCommitScan   atomic.Bool
	isSaving            bool

	// evictable maps EvictableMemoryUsers to eviction state.
	//
	// evictable is protected by mu.
	evictable map[EvictableMemoryUser]*evictableMemoryUserInfo

	// evictionWG counts the number of goroutines currently performing evictions.
	evictionWG sync.WaitGroup

	// opts holds options passed to NewMemoryFile. opts is immutable.
	opts MemoryFileOpts

	// savable is true if this MemoryFile will be saved via SaveTo() during
	// the kernel's SaveTo operation. savable is protected by mu.
	savable bool

	// destroyed is set by Destroy to instruct the releaser goroutine to
	// release all MemoryFile resources and exit. destroyed is protected by mu.
	destroyed bool

	// owners is the number of objects that may call Destroy. This is distinct
	// from references on pages allocated from this MemoryFile: live-forked
	// kernels share the allocator itself while independently owning its pages.
	// owners is protected by mu.
	owners uint64

	// stopNotifyPressure stops memory cgroup pressure level
	// notifications used to drive eviction. stopNotifyPressure is
	// immutable.
	stopNotifyPressure func()

	// If asyncPageLoad is non-nil, it tracks the state of in-progress or
	// failed async page loading.
	asyncPageLoad atomic.Pointer[asyncMemoryFileLoad]

	// sparseAsyncLoads contains independent immutable-chunk loaders attached
	// by successive sparse branch imports. The pointed-to slice is immutable.
	// sparseLoadMu serializes copy-on-write updates to the slice.
	sparseLoadMu     sync.Mutex
	sparseAsyncLoads atomic.Pointer[[]*asyncMemoryFileLoad]

	// file is the backing file. The file pointer is immutable.
	file *os.File

	// chunks holds metadata for each usable chunk in the backing file.
	//
	// chunks is at the end of MemoryFile in hopes of placing it on a relatively
	// quiet cache line, since MapInternal() is by far the hottest path through
	// pgalloc.
	//
	// chunks is protected by mu. chunks slices are immutable.
	chunks atomic.Pointer[[]chunkInfo]
}

func (f *MemoryFile) removeSparseAsyncLoad(remove *asyncMemoryFileLoad) {
	f.sparseLoadMu.Lock()
	defer f.sparseLoadMu.Unlock()
	old := f.sparseAsyncLoads.Load()
	if old == nil {
		return
	}
	next := make([]*asyncMemoryFileLoad, 0, len(*old))
	for _, load := range *old {
		if load != remove {
			next = append(next, load)
		}
	}
	if len(next) == 0 {
		f.sparseAsyncLoads.Store(nil)
	} else {
		f.sparseAsyncLoads.Store(&next)
	}
}

// NotifyRemoteCOW reports that a source mapping has stopped owning the given
// fork-time range after a copy-on-write break. Notification is non-blocking;
// transports consume it as a proactive-push hint.
func (f *MemoryFile) NotifyRemoteCOW(fr memmap.FileRange) {
	f.remoteCOWMu.RLock()
	snapshots := make([]*RemoteSnapshot, 0, len(f.remoteCOWSnapshots))
	for snapshot := range f.remoteCOWSnapshots {
		snapshots = append(snapshots, snapshot)
	}
	f.remoteCOWMu.RUnlock()
	for _, snapshot := range snapshots {
		snapshot.notifySourceCOW(f, fr)
	}
}

func (f *MemoryFile) registerRemoteSnapshot(snapshot *RemoteSnapshot) {
	f.remoteCOWMu.Lock()
	if f.remoteCOWSnapshots == nil {
		f.remoteCOWSnapshots = make(map[*RemoteSnapshot]struct{})
	}
	f.remoteCOWSnapshots[snapshot] = struct{}{}
	f.remoteCOWMu.Unlock()
}

func (f *MemoryFile) unregisterRemoteSnapshot(snapshot *RemoteSnapshot) {
	f.remoteCOWMu.Lock()
	delete(f.remoteCOWSnapshots, snapshot)
	f.remoteCOWMu.Unlock()
}

const (
	chunkShift = 30
	chunkSize  = 1 << chunkShift // 1 GB
	chunkMask  = chunkSize - 1
	maxChunks  = math.MaxInt64 / chunkSize // because file size is int64
)

// chunkInfo is the value type of MemoryFile.chunks.
//
// +stateify savable
type chunkInfo struct {
	// mapping is the start address of a mapping of the chunk.
	//
	// mapping is immutable.
	mapping uintptr `state:"nosave"`

	// huge is true if this chunk is expected to be hugepage-backed and false if
	// this chunk is expected to be smallpage-backed.
	//
	// huge is immutable.
	huge bool
}

func (f *MemoryFile) chunksLoad() []chunkInfo {
	return *f.chunks.Load()
}

// assignLogicalPagesLocked assigns fresh, never-reused identities to fr.
//
// Preconditions: f.mu must be locked and fr must contain newly allocated pages.
func (f *MemoryFile) assignLogicalPagesLocked(fr memmap.FileRange) {
	pages := fr.Length() / hostarch.PageSize
	if pages == 0 || pages > math.MaxUint64-f.nextLogicalPage || pages > math.MaxUint64-f.nextPageVersion {
		panic(fmt.Sprintf("logical page sequence exhausted for allocation %v", fr))
	}
	f.logicalPages.InsertRange(fr, logicalPageInfo{
		Allocator:            f.logicalAllocator,
		FirstSequence:        f.nextLogicalPage,
		VersionAllocator:     f.logicalAllocator,
		FirstVersionSequence: f.nextPageVersion,
	})
	f.nextLogicalPage += pages
	f.nextPageVersion += pages
}

// ensureLogicalPagesLocked synthesizes identities when loading a checkpoint
// written before logical-page metadata existed. New checkpoints always take
// the fast path because logicalPages is already populated.
//
// Preconditions: f is not concurrently accessible, or f.mu is locked.
func (f *MemoryFile) ensureLogicalPagesLocked() {
	if f.logicalPages.FirstSegment().Ok() {
		return
	}
	assignUsed := func(set *unfreeSet) {
		for seg := set.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
			if seg.ValuePtr().refs != 0 {
				f.assignLogicalPagesLocked(seg.Range())
			}
		}
	}
	assignUsed(&f.unfreeSmall)
	assignUsed(&f.unfreeHuge)
}

// LogicalPageIDAt returns the stable identity of the allocated page beginning
// at offset. It returns false for an unaligned or currently unallocated offset.
func (f *MemoryFile) LogicalPageIDAt(offset uint64) (LogicalPageID, bool) {
	id, _, ok := f.LogicalPageVersionAt(offset)
	return id, ok
}

// LogicalPageVersionAt returns the stable page and content-version identities
// for the allocated page beginning at offset.
func (f *MemoryFile) LogicalPageVersionAt(offset uint64) (LogicalPageID, PageVersionID, bool) {
	if !hostarch.IsPageAligned(offset) {
		return LogicalPageID{}, PageVersionID{}, false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	seg := f.logicalPages.FindSegment(offset)
	if !seg.Ok() {
		return LogicalPageID{}, PageVersionID{}, false
	}
	info := seg.ValuePtr()
	pageDelta := (offset - seg.Start()) / hostarch.PageSize
	return LogicalPageID{
			Allocator: info.Allocator,
			Sequence:  info.FirstSequence + pageDelta,
		}, PageVersionID{
			Allocator: info.VersionAllocator,
			Sequence:  info.FirstVersionSequence + pageDelta,
		}, true
}

// LogicalPageRanges returns the stable identity runs intersecting fr. The
// returned slice is detached from f and remains valid after this call.
func (f *MemoryFile) LogicalPageRanges(fr memmap.FileRange) []LogicalPageRange {
	if !fr.WellFormed() || !hostarch.IsPageAligned(fr.Start) || !hostarch.IsPageAligned(fr.End) {
		panic(fmt.Sprintf("invalid logical page range: %v", fr))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logicalPageRangesLocked(fr)
}

// AdoptCOWIdentities replaces the fresh logical identities on targetFR with
// the logical page identities from sourceFR and fresh content-version IDs.
// This makes a physical CoW copy another version of the same logical pages.
// sourceFR and targetFR must have equal, non-zero, page-aligned lengths.
//
// The caller must retain references on sourceFR and targetFR for this call.
func (f *MemoryFile) AdoptCOWIdentities(source *MemoryFile, sourceFR, targetFR memmap.FileRange) error {
	if source == nil || !sourceFR.WellFormed() || !targetFR.WellFormed() ||
		sourceFR.Length() == 0 || sourceFR.Length() != targetFR.Length() ||
		!hostarch.IsPageAligned(sourceFR.Start) || !hostarch.IsPageAligned(sourceFR.End) ||
		!hostarch.IsPageAligned(targetFR.Start) || !hostarch.IsPageAligned(targetFR.End) {
		return fmt.Errorf("invalid CoW identity ranges: source=%v target=%v", sourceFR, targetFR)
	}
	sourceRuns := source.LogicalPageRanges(sourceFR)
	cursor := sourceFR.Start
	for _, run := range sourceRuns {
		if run.Range.Start != cursor {
			return fmt.Errorf("source CoW identity range %v has a gap at %#x", sourceFR, cursor)
		}
		cursor = run.Range.End
	}
	if cursor != sourceFR.End {
		return fmt.Errorf("source CoW identity range %v ends at %#x", sourceFR, cursor)
	}

	pages := targetFR.Length() / hostarch.PageSize
	f.mu.Lock()
	defer f.mu.Unlock()
	targetRuns := f.logicalPageRangesLocked(targetFR)
	cursor = targetFR.Start
	for _, run := range targetRuns {
		if run.Range.Start != cursor {
			return fmt.Errorf("target CoW identity range %v has a gap at %#x", targetFR, cursor)
		}
		cursor = run.Range.End
	}
	if cursor != targetFR.End {
		return fmt.Errorf("target CoW identity range %v ends at %#x", targetFR, cursor)
	}
	if pages > math.MaxUint64-f.nextPageVersion {
		return fmt.Errorf("page version sequence exhausted for CoW range %v", targetFR)
	}
	f.logicalPages.RemoveFullRange(targetFR)
	nextVersion := f.nextPageVersion
	for _, run := range sourceRuns {
		destStart := targetFR.Start + (run.Range.Start - sourceFR.Start)
		dest := memmap.FileRange{Start: destStart, End: destStart + run.Range.Length()}
		f.logicalPages.InsertRange(dest, logicalPageInfo{
			Allocator:            run.First.Allocator,
			FirstSequence:        run.First.Sequence,
			VersionAllocator:     f.logicalAllocator,
			FirstVersionSequence: nextVersion,
		})
		nextVersion += run.Range.Length() / hostarch.PageSize
	}
	f.nextPageVersion += pages
	return nil
}

// logicalPageRangesLocked is LogicalPageRanges without locking.
//
// Preconditions: f.mu must be locked.
func (f *MemoryFile) logicalPageRangesLocked(fr memmap.FileRange) []LogicalPageRange {
	var ranges []LogicalPageRange
	f.logicalPages.VisitRange(fr, func(seg logicalPageIterator) bool {
		intersection := seg.Range().Intersect(fr)
		info := seg.ValuePtr()
		pageDelta := (intersection.Start - seg.Start()) / hostarch.PageSize
		ranges = append(ranges, LogicalPageRange{
			Range: intersection,
			First: LogicalPageID{
				Allocator: info.Allocator,
				Sequence:  info.FirstSequence + pageDelta,
			},
			FirstVersion: PageVersionID{
				Allocator: info.VersionAllocator,
				Sequence:  info.FirstVersionSequence + pageDelta,
			},
		})
		return true
	})
	return ranges
}

// forEachChunk invokes fn on a sequence of chunks that collectively span all
// bytes in fr. In each call, chunkFR is the subset of fr that falls within
// chunk. If any call to f returns false, forEachChunk stops iteration and
// returns.
func (f *MemoryFile) forEachChunk(fr memmap.FileRange, fn func(chunk *chunkInfo, chunkFR memmap.FileRange) bool) {
	chunks := f.chunksLoad()
	chunkStart := fr.Start &^ chunkMask
	i := int(fr.Start / chunkSize)
	for chunkStart < fr.End {
		chunkEnd := chunkStart + chunkSize
		if !fn(&chunks[i], fr.Intersect(memmap.FileRange{chunkStart, chunkEnd})) {
			return
		}
		chunkStart = chunkEnd
		i++
	}
}

// unwasteInfo is the value type of MemoryFile.unwasteSmall/Huge.
//
// +stateify savable
type unwasteInfo struct{}

// unfreeInfo is the value type of MemoryFile.unfreeSmall/Huge.
//
// +stateify savable
type unfreeInfo struct {
	// refs is the per-page reference count. refs is non-zero for used pages,
	// and zero for void, waste, releasing, and sub-released pages, as well as
	// pages backed by a different page size.
	refs uint64
}

// LogicalAllocatorID identifies one collision-resistant logical-page allocator
// incarnation. Imported IDs retain their original allocator ID.
//
// +stateify savable
type LogicalAllocatorID [16]byte

// LogicalPageID is the stable identity of one allocated base page. It is
// independent of the page's current MemoryFile offset.
//
// +stateify savable
type LogicalPageID struct {
	Allocator LogicalAllocatorID
	Sequence  uint64
}

// PageVersionID identifies one immutable content incarnation of a logical
// page. A CoW split preserves LogicalPageID and assigns a new PageVersionID.
//
// +stateify savable
type PageVersionID struct {
	Allocator LogicalAllocatorID
	Sequence  uint64
}

// LogicalChunkID identifies the group containing a logical page. TTC's first
// sparse-memory format uses 256 base-page slots per chunk.
//
// +stateify savable
type LogicalChunkID struct {
	Allocator LogicalAllocatorID
	Sequence  uint64
}

const logicalPagesPerChunk = 256

// Chunk returns the stable logical chunk and slot containing id.
func (id LogicalPageID) Chunk() (LogicalChunkID, uint8) {
	return LogicalChunkID{
		Allocator: id.Allocator,
		Sequence:  id.Sequence / logicalPagesPerChunk,
	}, uint8(id.Sequence % logicalPagesPerChunk)
}

// logicalPageInfo is the value type of MemoryFile.logicalPages. FirstSequence
// is the sequence for the first page in the segment's range.
//
// +stateify savable
type logicalPageInfo struct {
	Allocator            LogicalAllocatorID
	FirstSequence        uint64
	VersionAllocator     LogicalAllocatorID
	FirstVersionSequence uint64
}

// LogicalPageRange describes a contiguous run of backing pages with contiguous
// stable identities.
type LogicalPageRange struct {
	Range        memmap.FileRange
	First        LogicalPageID
	FirstVersion PageVersionID
}

// memAcctInfo is the value type of MemoryFile.memAcct.
//
// +stateify savable
type memAcctInfo struct {
	// kind is the memory accounting type. kind is allocation-dependent for
	// used pages, and usage.System for void, waste, releasing, and
	// sub-released pages.
	kind usage.MemoryKind

	// memCgID is the memory cgroup ID to which represented pages are accounted.
	memCgID uint32

	// knownCommitted is true if represented pages are definitely committed.
	// (If knownCommitted is false, represented pages may or may not be
	// committed; pages that are definitely not committed are represented by
	// gaps in MemoryFile.memAcct.)
	knownCommitted bool

	// If true, represented pages are waste or releasing pages.
	wasteOrReleasing bool

	// If knownCommitted is false, commitSeq was the value of
	// MemoryFile.commitSeq when knownCommitted last transitioned to false.
	// Otherwise, commitSeq is 0.
	commitSeq uint64
}

// An EvictableMemoryUser represents a user of MemoryFile-allocated memory that
// may be asked to deallocate that memory in the presence of memory pressure.
type EvictableMemoryUser interface {
	// Evict requests that the EvictableMemoryUser deallocate memory used by
	// er, which was registered as evictable by a previous call to
	// MemoryFile.MarkEvictable.
	//
	// Evict is not required to deallocate memory. In particular, since pgalloc
	// must call Evict without holding locks to avoid circular lock ordering,
	// it is possible that the passed range has already been marked as
	// unevictable by a racing call to MemoryFile.MarkUnevictable.
	// Implementations of EvictableMemoryUser must detect such races and handle
	// them by making Evict have no effect on unevictable ranges.
	//
	// After a call to Evict, the MemoryFile will consider the evicted range
	// unevictable (i.e. it will not call Evict on the same range again) until
	// informed otherwise by a subsequent call to MarkEvictable.
	Evict(ctx context.Context, er EvictableRange)
}

// An EvictableRange represents a range of uint64 offsets in an
// EvictableMemoryUser.
//
// In practice, most EvictableMemoryUsers will probably be implementations of
// memmap.Mappable, and EvictableRange therefore corresponds to
// memmap.MappableRange. However, this package cannot depend on the memmap
// package, since doing so would create a circular dependency.
//
// type EvictableRange <generated using go_generics>

// evictableMemoryUserInfo is the value type of MemoryFile.evictable.
type evictableMemoryUserInfo struct {
	// ranges tracks all evictable ranges for the given user.
	ranges evictableRangeSet

	// If evicting is true, there is a goroutine currently evicting all
	// evictable ranges for this user.
	evicting bool
}

// MemoryFileOpts provides options to NewMemoryFile.
type MemoryFileOpts struct {
	// DelayedEviction controls the extent to which the MemoryFile may delay
	// eviction of evictable allocations.
	DelayedEviction DelayedEvictionType

	// If UseHostMemcgPressure is true, use host memory cgroup pressure level
	// notifications to determine when eviction is necessary. This option has
	// no effect unless DelayedEviction is DelayedEvictionEnabled.
	UseHostMemcgPressure bool

	// DecommitOnDestroy indicates whether the entire host file should be
	// decommitted on destruction. This is appropriate for host filesystem based
	// files that need to be explicitly cleaned up to release disk space.
	DecommitOnDestroy bool

	// If DisableIMAWorkAround is true, NewMemoryFile will not call
	// IMAWorkAroundForMemFile().
	DisableIMAWorkAround bool

	// DiskBackedFile indicates that the MemoryFile is backed by a file on disk.
	DiskBackedFile bool

	// ResourceID is used to reassociate the MemoryFile with its replacement
	// during restore.
	ResourceID checkpoint.ResourceID

	// If ExpectHugepages is true, MemoryFile will expect that the host will
	// attempt to back AllocOpts.Huge == true allocations with huge pages. If
	// ExpectHugepages is false, MemoryFile will expect that the host will back
	// all allocations with small pages.
	ExpectHugepages bool

	// If AdviseHugepage is true, MemoryFile will request that the host back
	// AllocOpts.Huge == true allocations with huge pages using MADV_HUGEPAGE.
	AdviseHugepage bool

	// If AdviseNoHugepage is true, MemoryFile will request that the host back
	// AllocOpts.Huge == false allocations with small pages using
	// MADV_NOHUGEPAGE.
	AdviseNoHugepage bool

	// If DisableMemoryAccounting is true, memory usage observed by the
	// MemoryFile will not be reported in usage.MemoryAccounting.
	DisableMemoryAccounting bool
}

// DelayedEvictionType is the type of MemoryFileOpts.DelayedEviction.
type DelayedEvictionType uint8

const (
	// DelayedEvictionDefault has unspecified behavior.
	DelayedEvictionDefault DelayedEvictionType = iota

	// DelayedEvictionDisabled requires that evictable allocations are evicted
	// as soon as possible.
	DelayedEvictionDisabled

	// DelayedEvictionEnabled requests that the MemoryFile delay eviction of
	// evictable allocations until doing so is considered necessary to avoid
	// performance degradation due to host memory pressure, or OOM kills.
	//
	// As of this writing, the behavior of DelayedEvictionEnabled depends on
	// whether or not MemoryFileOpts.UseHostMemcgPressure is enabled:
	//
	//	- If UseHostMemcgPressure is true, evictions are delayed until memory
	//	pressure is indicated.
	//
	//	- Otherwise, evictions are only delayed until the releaser goroutine is
	//	out of work (pages to release).
	DelayedEvictionEnabled

	// DelayedEvictionManual requires that evictable allocations are only
	// evicted when MemoryFile.StartEvictions() is called. This is extremely
	// dangerous outside of tests.
	DelayedEvictionManual
)

// NewMemoryFile creates a MemoryFile backed by the given file. If
// NewMemoryFile succeeds, ownership of file is transferred to the returned
// MemoryFile.
func NewMemoryFile(file *os.File, opts MemoryFileOpts) (*MemoryFile, error) {
	switch opts.DelayedEviction {
	case DelayedEvictionDefault:
		opts.DelayedEviction = DelayedEvictionEnabled
	case DelayedEvictionDisabled, DelayedEvictionManual:
		opts.UseHostMemcgPressure = false
	case DelayedEvictionEnabled:
		// ok
	default:
		return nil, fmt.Errorf("invalid MemoryFileOpts.DelayedEviction: %v", opts.DelayedEviction)
	}

	// Truncate the file to 0 bytes first to ensure that it's empty.
	if err := file.Truncate(0); err != nil {
		return nil, err
	}
	f := &MemoryFile{
		opts: opts,
		file: file,
	}
	f.initFields()

	if f.opts.DelayedEviction == DelayedEvictionEnabled && f.opts.UseHostMemcgPressure {
		stop, err := hostmm.NotifyCurrentMemcgPressureCallback(func() {
			f.mu.Lock()
			startedAny := f.startEvictionsLocked()
			f.mu.Unlock()
			if startedAny {
				log.Debugf("pgalloc.MemoryFile performing evictions due to memcg pressure")
			}
		}, "low")
		if err != nil {
			return nil, fmt.Errorf("failed to configure memcg pressure level notifications: %v", err)
		}
		f.stopNotifyPressure = stop
	}

	go f.releaserMain() // S/R-SAFE: f.mu

	if !opts.DisableIMAWorkAround {
		IMAWorkAroundForMemFile(file.Fd())
	}
	return f, nil
}

func (f *MemoryFile) initFields() {
	f.owners = 1
	// Initially, all pages are void.
	fullFR := memmap.FileRange{0, math.MaxUint64}
	f.unwasteSmall.InsertRange(fullFR, unwasteInfo{})
	f.unwasteHuge.InsertRange(fullFR, unwasteInfo{})
	f.releaseCond.L = &f.mu
	f.unfreeSmall.InsertRange(fullFR, unfreeInfo{})
	f.unfreeHuge.InsertRange(fullFR, unfreeInfo{})
	f.subreleased = make(map[uint64]uint64)
	f.evictable = make(map[EvictableMemoryUser]*evictableMemoryUserInfo)
	f.sparseChunks = make(map[SparseChunkObjectID]sparseResidentChunk)
	if _, err := rand.Read(f.logicalAllocator[:]); err != nil {
		panic(fmt.Sprintf("generating MemoryFile logical allocator ID: %v", err))
	}
	f.nextLogicalPage = 0
	f.nextPageVersion = 0
	chunks := []chunkInfo(nil)
	f.chunks.Store(&chunks)
}

// IMAWorkAroundForMemFile works around IMA by immediately creating a temporary
// PROT_EXEC mapping, while the backing file is still small. IMA will ignore
// any future mappings.
//
// The Linux kernel contains an optional feature called "Integrity
// Measurement Architecture" (IMA). If IMA is enabled, it will checksum
// binaries the first time they are mapped PROT_EXEC. This is bad news for
// executable pages mapped from our backing file, which can grow to
// terabytes in (sparse) size. If IMA attempts to checksum a file that
// large, it will allocate all of the sparse pages and quickly exhaust all
// memory.
func IMAWorkAroundForMemFile(fd uintptr) {
	m, _, errno := unix.Syscall6(
		unix.SYS_MMAP,
		0,
		hostarch.PageSize,
		unix.PROT_EXEC,
		unix.MAP_SHARED,
		fd,
		0)
	if errno != 0 {
		// This isn't fatal (IMA may not even be in use). Log the error, but
		// don't return it.
		log.Warningf("Failed to pre-map MemoryFile PROT_EXEC: %v", errno)
	} else {
		if _, _, errno := unix.Syscall(
			unix.SYS_MUNMAP,
			m,
			hostarch.PageSize,
			0); errno != 0 {
			panic(fmt.Sprintf("failed to unmap PROT_EXEC MemoryFile mapping: %v", errno))
		}
	}
}

// IncOwner adds an owner of f. Each successful call must be paired with a
// call to Destroy.
//
// IncOwner must not be called after the last owner has called Destroy.
func (f *MemoryFile) IncOwner() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyed || f.owners == 0 {
		panic("IncOwner called on destroyed MemoryFile")
	}
	f.owners++
}

// Destroy releases one ownership reference to f. Resources used by f are
// released after the last owner calls Destroy.
//
// Preconditions: When the last owner calls Destroy, all pages allocated by f
// have been freed.
//
// Postconditions: The caller may not use f after calling Destroy.
func (f *MemoryFile) Destroy() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.owners == 0 {
		panic("Destroy called without a MemoryFile owner")
	}
	f.owners--
	if f.owners != 0 {
		return
	}
	f.destroyed = true
	f.releaseCond.Signal()
}

// Preconditions: f.mu must be locked.
func (f *MemoryFile) releaserDestroyLocked() {
	if !f.destroyed {
		panic("destroyed is no longer set")
	}

	if f.opts.DecommitOnDestroy {
		if chunks := f.chunksLoad(); len(chunks) != 0 {
			if err := f.decommitFile(memmap.FileRange{0, uint64(len(chunks)) * chunkSize}); err != nil {
				panic(fmt.Sprintf("failed to decommit entire memory file during destruction: %v", err))
			}
		}
	}

	f.file.Close()
	// Ensure that any attempts to use f.file.Fd() fail instead of getting a fd
	// that has possibly been reassigned.
	f.file = nil
	chunks := f.chunksLoad()
	for i := range chunks {
		chunk := &chunks[i]
		_, _, errno := unix.Syscall(unix.SYS_MUNMAP, chunk.mapping, chunkSize, 0)
		if errno != 0 {
			log.Warningf("Failed to unmap mapping %#x for MemoryFile chunk %d: %v", chunk.mapping, i, errno)
		}
		chunk.mapping = 0
	}
}

// AllocOpts are options used in MemoryFile.Allocate.
type AllocOpts struct {
	// Kind is the allocation's memory accounting type.
	Kind usage.MemoryKind

	// MemCgID is the memory cgroup ID and the zero value indicates that
	// the memory will not be accounted to any cgroup.
	MemCgID uint32

	// Mode controls the commitment status of returned pages.
	Mode AllocationMode

	// If Huge is true, the allocation should be hugepage-backed if possible.
	Huge bool

	// Dir indicates the direction in which offsets are allocated.
	Dir Direction

	// If ReaderFunc is provided, the allocated memory is filled by calling it
	// repeatedly until either length bytes are read or a non-nil error is
	// returned. It returns the allocated memory, truncated down to the nearest
	// page. If this is shorter than length bytes due to an error returned by
	// ReaderFunc, it returns the partially filled fr and error.
	ReaderFunc safemem.ReaderFunc
}

// Direction is the type of AllocOpts.Dir.
type Direction uint8

const (
	// BottomUp allocates offsets in increasing offsets.
	BottomUp Direction = iota
	// TopDown allocates offsets in decreasing offsets.
	TopDown
)

// String implements fmt.Stringer.
func (d Direction) String() string {
	switch d {
	case BottomUp:
		return "up"
	case TopDown:
		return "down"
	}
	panic(fmt.Sprintf("invalid direction: %d", d))
}

// AllocationMode is the type of AllocOpts.Mode.
type AllocationMode int

const (
	// AllocateUncommitted indicates that MemoryFile.Allocate() must return
	// uncommitted pages.
	AllocateUncommitted AllocationMode = iota

	// AllocateCallerIndirectCommit indicates that the caller of
	// MemoryFile.Allocate() intends to commit all allocated pages, without
	// using our page tables. Thus, Allocate() may return committed or
	// uncommitted pages.
	AllocateCallerIndirectCommit

	// AllocateAndCommit indicates that MemoryFile.Allocate() must return
	// committed pages.
	AllocateAndCommit

	// AllocateAndWritePopulate indicates that the caller of
	// MemoryFile.Allocate() intends to commit all allocated pages, using our
	// page tables. Thus, Allocate() may return committed or uncommitted pages,
	// and should pre-populate page table entries permitting writing for
	// mappings of those pages returned by MapInternal().
	AllocateAndWritePopulate
)

// allocState holds the state of a call to MemoryFile.Allocate().
type allocState struct {
	length     uint64
	opts       AllocOpts
	willCommit bool // either us or our caller
	recycled   bool
	huge       bool
}

// Allocate returns a range of initially-zeroed pages of the given length, with
// a single reference on each page held by the caller. When the last reference
// on an allocated page is released, ownership of the page is returned to the
// MemoryFile, allowing it to be returned by a future call to Allocate.
//
// Preconditions:
//   - length > 0.
//   - length must be page-aligned.
//   - If opts.Hugepage == true, length must be hugepage-aligned.
func (f *MemoryFile) Allocate(length uint64, opts AllocOpts) (memmap.FileRange, error) {
	if length == 0 || !hostarch.IsPageAligned(length) || (opts.Huge && !hostarch.IsHugePageAligned(length)) {
		panic(fmt.Sprintf("invalid allocation length: %#x", length))
	}

	alloc := allocState{
		length:     length,
		opts:       opts,
		willCommit: opts.Mode != AllocateUncommitted,
		huge:       opts.Huge && f.opts.ExpectHugepages,
	}

	fr, err := f.findAllocatableAndMarkUsed(&alloc)
	if err != nil {
		return fr, err
	}

	var dsts safemem.BlockSeq
	if alloc.willCommit {
		needHugeTouch := false
		if alloc.recycled {
			// We will need writable page table entries in our address space to
			// zero these pages.
			alloc.opts.Mode = AllocateAndWritePopulate
		} else if alloc.opts.Mode != AllocateAndWritePopulate && ((alloc.huge && f.opts.AdviseHugepage) || (!alloc.huge && f.opts.AdviseNoHugepage)) {
			// If Mode is AllocateCallerIndirectCommit and we do nothing, the
			// first access to the allocation may be by the application,
			// through a platform.AddressSpace, which may not have
			// MADV_HUGEPAGE (=> vma flag VM_HUGEPAGE) set. Consequently,
			// shmem_fault() => shmem_get_folio_gfp() will commit a small page.
			//
			// If Mode is AllocateAndCommit and we do nothing, the first access
			// to the allocation is via fallocate(2), which has the same
			// problem: shmem_fallocate() => shmem_get_folio() =>
			// shmem_get_folio_gfp(vma=NULL).
			//
			// khugepaged may eventually collapse the containing
			// hugepage-aligned region into a huge page when it scans our
			// mapping (khugepaged_scan_mm_slot() => khugepaged_scan_file()),
			// but this depends on khugepaged_max_ptes_none, and in addition to
			// the latency and overhead of doing so, this will incur another
			// round of page faults.
			//
			// If write-populating through our mappings succeeds, then it will
			// avoid this problem. Otherwise, we need to touch each huge page
			// through our mappings.
			//
			// An analogous problem applies if MADV_NOHUGEPAGE is required
			// rather than MADV_HUGEPAGE; MADV_NOHUGEPAGE is only enabled if
			// the file defaults to huge pages, so populating or touching
			// through our mappings is needed to ensure that the allocation is
			// small-page-backed. In this case, we only need to force
			// commitment of one small page per huge page to prevent future
			// page faults within the huge page from faulting a huge page,
			// though there's nothing we can do about khugepaged.
			alloc.opts.Mode = AllocateAndWritePopulate
			needHugeTouch = true
		}

		switch alloc.opts.Mode {
		case AllocateUncommitted, AllocateCallerIndirectCommit:
			// Nothing for us to do.
		case AllocateAndCommit:
			if err := f.commitFile(fr); err != nil {
				f.DecRef(fr)
				return memmap.FileRange{}, err
			}
		case AllocateAndWritePopulate:
			dsts, err = f.MapInternal(fr, hostarch.Write)
			if err != nil {
				f.DecRef(fr)
				return memmap.FileRange{}, err
			}
			if canPopulate() {
				rem := dsts
				for tryPopulate(rem.Head()) {
					rem = rem.Tail()
					if rem.IsEmpty() {
						needHugeTouch = false
						break
					}
				}
			}
			if alloc.recycled {
				// The contents of recycled waste pages are initially unknown, so we
				// need to zero them.
				f.manuallyZero(fr)
			} else if needHugeTouch {
				// We only need to touch a single byte in each huge page.
				f.forEachMappingSlice(fr, func(bs []byte) {
					for i := 0; i < len(bs); i += hostarch.HugePageSize {
						bs[i] = 0
					}
				})
			}
		default:
			panic(fmt.Sprintf("unknown AllocOpts.Mode %d", alloc.opts.Mode))
		}
	}
	if alloc.opts.ReaderFunc != nil {
		if dsts.IsEmpty() {
			dsts, err = f.MapInternal(fr, hostarch.Write)
			if err != nil {
				f.DecRef(fr)
				return memmap.FileRange{}, err
			}
		}
		n, err := safemem.ReadFullToBlocks(alloc.opts.ReaderFunc, dsts)
		un := uint64(hostarch.Addr(n).RoundDown())
		if un < length {
			// Free unused memory and update fr to contain only the memory that is
			// still allocated.
			f.DecRef(memmap.FileRange{fr.Start + un, fr.End})
			fr.End = fr.Start + un
		}
		if err != nil {
			return fr, err
		}
	}

	return fr, nil
}

func (f *MemoryFile) findAllocatableAndMarkUsed(alloc *allocState) (fr memmap.FileRange, err error) {
	unwaste := &f.unwasteSmall
	unfree := &f.unfreeSmall
	if alloc.huge {
		unwaste = &f.unwasteHuge
		unfree = &f.unfreeHuge
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if alloc.willCommit {
		// Try to recycle waste pages, since this avoids the overhead of
		// decommitting and then committing them again.
		var uwgap unwasteGapIterator
		if alloc.opts.Dir == BottomUp {
			uwgap = unwaste.FirstLargeEnoughGap(alloc.length)
		} else {
			uwgap = unwaste.LastLargeEnoughGap(alloc.length)
		}
		if uwgap.Ok() {
			alloc.recycled = true
			if alloc.opts.Dir == BottomUp {
				fr = memmap.FileRange{
					Start: uwgap.Start(),
					End:   uwgap.Start() + alloc.length,
				}
			} else {
				fr = memmap.FileRange{
					Start: uwgap.End() - alloc.length,
					End:   uwgap.End(),
				}
			}
			unwaste.Insert(uwgap, fr, unwasteInfo{})
			// Update reference count for these pages from 0 to 1.
			unfree.MutateFullRange(fr, func(ufseg unfreeIterator) bool {
				uf := ufseg.ValuePtr()
				if uf.refs != 0 {
					panic(fmt.Sprintf("waste pages %v have unexpected refcount %d during recycling of %v\n%s", ufseg.Range(), uf.refs, fr, f.stringLocked()))
				}
				uf.refs = 1
				return true
			})
			// These pages should all be unknown-commitment or known-committed;
			// mark them unknown-commitment, for consistency with non-recycling
			// allocations (below).
			f.memAcct.MutateFullRange(fr, func(maseg memAcctIterator) bool {
				ma := maseg.ValuePtr()
				malen := maseg.Range().Length()
				if ma.knownCommitted {
					if ma.kind != usage.System {
						panic(fmt.Sprintf("waste pages %v have unexpected kind %v\n%s", maseg.Range(), ma.kind, f.stringLocked()))
					}
					ma.knownCommitted = false
					ma.commitSeq = f.commitSeq
					f.knownCommittedBytes -= malen
					if !f.opts.DisableMemoryAccounting {
						usage.MemoryAccounting.Dec(malen, usage.System, ma.memCgID)
					}
				}
				ma.kind = alloc.opts.Kind
				ma.memCgID = alloc.opts.MemCgID
				ma.wasteOrReleasing = false
				return true
			})
			f.assignLogicalPagesLocked(fr)
			return
		}
	}

	// No suitable waste pages or we can't use them.
retryFree:
	// Try to allocate free pages from existing chunks.
	var ufgap unfreeGapIterator
	if alloc.opts.Dir == BottomUp {
		ufgap = unfree.FirstLargeEnoughGap(alloc.length)
	} else {
		ufgap = unfree.LastLargeEnoughGap(alloc.length)
	}
	if !ufgap.Ok() {
		// Extend the file to create more chunks.
		err = f.extendChunksLocked(alloc)
		if err != nil {
			return
		}
		// Retry the allocation using new chunks.
		goto retryFree
	}
	if alloc.opts.Dir == BottomUp {
		fr = memmap.FileRange{
			Start: ufgap.Start(),
			End:   ufgap.Start() + alloc.length,
		}
	} else {
		fr = memmap.FileRange{
			Start: ufgap.End() - alloc.length,
			End:   ufgap.End(),
		}
	}
	unfree.Insert(ufgap, fr, unfreeInfo{refs: 1})
	// These pages should all be known-decommitted; mark them
	// unknown-commitment, since they can be concurrently committed by the
	// allocation's users at any time until deallocation.
	//
	// If alloc.willCommit is true, we expect these pages to become committed
	// in the near future; mark them unknown-commitment anyway, since marking
	// them committed prematurely makes them more likely to be saved even if
	// zeroed, unless SaveOpts.ExcludeCommittedZeroPages is enabled.
	f.memAcct.InsertRange(fr, memAcctInfo{
		kind:           alloc.opts.Kind,
		memCgID:        alloc.opts.MemCgID,
		knownCommitted: false,
		commitSeq:      f.commitSeq,
	})
	f.assignLogicalPagesLocked(fr)
	return
}

// Preconditions: f.mu must be locked.
func (f *MemoryFile) extendChunksLocked(alloc *allocState) error {
	unfree := &f.unfreeSmall
	if alloc.huge {
		unfree = &f.unfreeHuge
	}

	oldChunks := f.chunksLoad()
	oldNrChunks := uint64(len(oldChunks))
	oldFileSize := oldNrChunks * chunkSize

	// Determine how many chunks we need to satisfy alloc.
	tail := uint64(0)
	if oldNrChunks != 0 {
		if lastChunk := oldChunks[oldNrChunks-1]; lastChunk.huge == alloc.huge {
			// We can use free pages at the end of the current last chunk.
			if ufgap := unfree.FindGap(oldFileSize - 1); ufgap.Ok() {
				tail = ufgap.Range().Length()
			}
		}
	}
	incNrChunks := (alloc.length + chunkMask - tail) / chunkSize
	incFileSize := incNrChunks * chunkSize
	newNrChunks := oldNrChunks + incNrChunks
	if newNrChunks > maxChunks || newNrChunks < oldNrChunks /* overflow */ {
		return linuxerr.ENOMEM
	}
	newFileSize := newNrChunks * chunkSize

	// Extend the backing file and obtain mappings for the new chunks. If the
	// backing file is memory-backed, and THP is enabled, Linux will align our
	// mapping to a hugepage boundary; see
	// mm/shmem.c:shmem_get_unmapped_area().
	//
	// In tests, f.file may be nil.
	var mapStart uintptr
	if f.file != nil {
		if err := f.file.Truncate(int64(newFileSize)); err != nil {
			return err
		}
		m, _, errno := unix.Syscall6(
			unix.SYS_MMAP,
			0,
			uintptr(incFileSize),
			unix.PROT_READ|unix.PROT_WRITE,
			unix.MAP_SHARED,
			f.file.Fd(),
			uintptr(oldFileSize))
		if errno != 0 {
			return errno
		}
		mapStart = m
		f.madviseChunkMapping(mapStart, uintptr(incFileSize), alloc.huge)
	}

	// Update chunk state.
	newChunks := make([]chunkInfo, newNrChunks)
	copy(newChunks, oldChunks)
	m := mapStart
	for i := oldNrChunks; i < newNrChunks; i++ {
		newChunks[i].huge = alloc.huge
		if f.file != nil {
			newChunks[i].mapping = m
			m += chunkSize
		}
	}
	f.chunks.Store(&newChunks)

	// Mark void pages free.
	unfree.RemoveFullRange(memmap.FileRange{
		Start: oldNrChunks * chunkSize,
		End:   newNrChunks * chunkSize,
	})

	return nil
}

func (f *MemoryFile) madviseChunkMapping(addr, len uintptr, huge bool) {
	if huge {
		if f.opts.AdviseHugepage {
			_, _, errno := unix.Syscall(unix.SYS_MADVISE, addr, len, unix.MADV_HUGEPAGE)
			if errno != 0 {
				// Log this failure but continue.
				log.Warningf("madvise(%#x, %d, MADV_HUGEPAGE) failed: %s", addr, len, errno)
			}
		}
	} else {
		if f.opts.AdviseNoHugepage {
			_, _, errno := unix.Syscall(unix.SYS_MADVISE, addr, len, unix.MADV_NOHUGEPAGE)
			if errno != 0 {
				// Log this failure but continue.
				log.Warningf("madvise(%#x, %d, MADV_NOHUGEPAGE) failed: %s", addr, len, errno)
			}
		}
	}
}

var mlockDisabled atomicbitops.Uint32
var madvPopulateWriteDisabled atomicbitops.Uint32

func canPopulate() bool {
	return mlockDisabled.Load() == 0 || madvPopulateWriteDisabled.Load() == 0
}

func tryPopulateMadv(b safemem.Block) bool {
	if madvPopulateWriteDisabled.Load() != 0 {
		return false
	}
	// Only call madvise(MADV_POPULATE_WRITE) if >=2 pages are being populated.
	// 1 syscall overhead >= 1 page fault overhead. This is because syscalls are
	// susceptible to additional overheads like seccomp-bpf filters and auditing.
	if b.Len() <= hostarch.PageSize {
		return true
	}
	_, _, errno := unix.Syscall(unix.SYS_MADVISE, b.Addr(), uintptr(b.Len()), unix.MADV_POPULATE_WRITE)
	if errno != 0 {
		if errno == unix.EINVAL {
			// EINVAL is expected if MADV_POPULATE_WRITE is not supported (Linux <5.14).
			log.Infof("Disabling pgalloc.MemoryFile.AllocateAndFill pre-population: madvise failed: %s", errno)
		} else {
			log.Warningf("Disabling pgalloc.MemoryFile.AllocateAndFill pre-population: madvise failed: %s", errno)
		}
		madvPopulateWriteDisabled.Store(1)
		return false
	}
	return true
}

func tryPopulateMlock(b safemem.Block) bool {
	if mlockDisabled.Load() != 0 {
		return false
	}
	// Call mlock to populate pages, then munlock to cancel the mlock (but keep
	// the pages populated). Only do so for hugepage-aligned address ranges to
	// ensure that splitting the VMA in mlock doesn't split any existing
	// hugepages. This assumes that two host syscalls, plus the MM overhead of
	// mlock + munlock, is faster on average than trapping for
	// HugePageSize/PageSize small page faults.
	start, ok := hostarch.Addr(b.Addr()).HugeRoundUp()
	if !ok {
		return true
	}
	end := hostarch.Addr(b.Addr() + uintptr(b.Len())).HugeRoundDown()
	if start >= end {
		return true
	}
	_, _, errno := unix.Syscall(unix.SYS_MLOCK, uintptr(start), uintptr(end-start), 0)
	unix.RawSyscall(unix.SYS_MUNLOCK, uintptr(start), uintptr(end-start), 0)
	if errno != 0 {
		if errno == unix.ENOMEM || errno == unix.EPERM {
			// These errors are expected from hitting non-zero RLIMIT_MEMLOCK, or
			// hitting zero RLIMIT_MEMLOCK without CAP_IPC_LOCK, respectively.
			log.Infof("Disabling pgalloc.MemoryFile.AllocateAndFill pre-population: mlock failed: %s", errno)
		} else {
			log.Warningf("Disabling pgalloc.MemoryFile.AllocateAndFill pre-population: mlock failed: %s", errno)
		}
		mlockDisabled.Store(1)
		return false
	}
	return true
}

func tryPopulate(b safemem.Block) bool {
	// There are two approaches for populating writable pages:
	// 1. madvise(MADV_POPULATE_WRITE). It has the desired effect: "Populate
	//    (prefault) page tables writable, faulting in all pages in the range
	//    just as if manually writing to each each page".
	// 2. Call mlock to populate pages, then munlock to cancel the mlock (but
	//    keep the pages populated).
	//
	// Prefer the madvise(MADV_POPULATE_WRITE) approach because:
	// - Only requires 1 syscall, as opposed to 2 syscalls with mlock approach.
	// - It is faster because it doesn't have to modify vmas like mlock does.
	// - It works for disk-backed memory mappings too. The mlock approach doesn't
	//   work for disk-backed filesystems (e.g. ext4). This is because
	//   mlock(2) => mm/gup.c:__mm_populate() emulates a read fault on writable
	//   MAP_SHARED mappings. For memory-backed (shmem) files,
	//   mm/mmap.c:vma_set_page_prot() => vma_wants_writenotify() is false, so
	//   the page table entries populated by a read fault are writable. For
	//   disk-backed files, vma_set_page_prot() => vma_wants_writenotify() is
	//   true, so the page table entries populated by a read fault are read-only.
	if tryPopulateMadv(b) {
		return true
	}
	return tryPopulateMlock(b)
}

// Decommit uncommits the given pages, causing them to become zeroed.
//
// Preconditions:
//   - fr.Start and fr.End must be page-aligned.
//   - fr.Length() > 0.
//   - At least one reference must be held on all pages in fr.
func (f *MemoryFile) Decommit(fr memmap.FileRange) {
	if !fr.WellFormed() || fr.Length() == 0 || fr.Start%hostarch.PageSize != 0 || fr.End%hostarch.PageSize != 0 {
		panic(fmt.Sprintf("invalid range: %v", fr))
	}

	f.decommitOrManuallyZero(fr)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.memAcct.MutateFullRange(fr, func(maseg memAcctIterator) bool {
		ma := maseg.ValuePtr()
		if ma.knownCommitted {
			ma.knownCommitted = false
			malen := maseg.Range().Length()
			f.knownCommittedBytes -= malen
			if !f.opts.DisableMemoryAccounting {
				usage.MemoryAccounting.Dec(malen, ma.kind, ma.memCgID)
			}
		}
		// Update commitSeq to invalidate any observations made by
		// concurrent calls to f.updateUsageLocked().
		ma.commitSeq = f.commitSeq
		return true
	})
}

func (f *MemoryFile) commitFile(fr memmap.FileRange) error {
	// "The default operation (i.e., mode is zero) of fallocate() allocates the
	// disk space within the range specified by offset and len." - fallocate(2)
	return unix.Fallocate(
		int(f.file.Fd()),
		0, // mode
		int64(fr.Start),
		int64(fr.Length()))
}

func (f *MemoryFile) decommitFile(fr memmap.FileRange) error {
	// "After a successful call, subsequent reads from this range will
	// return zeroes. The FALLOC_FL_PUNCH_HOLE flag must be ORed with
	// FALLOC_FL_KEEP_SIZE in mode ..." - fallocate(2)
	return unix.Fallocate(
		int(f.file.Fd()),
		unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE,
		int64(fr.Start),
		int64(fr.Length()))
}

func (f *MemoryFile) manuallyZero(fr memmap.FileRange) {
	f.forEachMappingSlice(fr, func(bs []byte) {
		clear(bs)
	})
}

func (f *MemoryFile) decommitOrManuallyZero(fr memmap.FileRange) {
	if err := f.decommitFile(fr); err != nil {
		log.Warningf("Failed to decommit %v: %v", fr, err)
		// Zero the pages manually. This won't reduce memory usage, but at
		// least ensures that the pages will be zeroed when reallocated.
		f.manuallyZero(fr)
	}
}

// HasUniqueRef returns true if all pages in the given range have exactly one
// reference. A return value of false is inherently racy, but if the caller
// holds a reference on the given range and is preventing other goroutines from
// copying it, then a return value of true is not racy.
//
// Preconditions: At least one reference must be held on all pages in fr.
func (f *MemoryFile) HasUniqueRef(fr memmap.FileRange) bool {
	hasUniqueRef := true
	f.mu.Lock()
	defer f.mu.Unlock()
	f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		unfree := &f.unfreeSmall
		if chunk.huge {
			unfree = &f.unfreeHuge
		}
		unfree.VisitFullRange(fr, func(ufseg unfreeIterator) bool {
			if ufseg.ValuePtr().refs != 1 {
				hasUniqueRef = false
				return false
			}
			return true
		})
		return hasUniqueRef
	})
	return hasUniqueRef
}

// IncRef implements memmap.File.IncRef.
func (f *MemoryFile) IncRef(fr memmap.FileRange, memCgID uint32) {
	if !fr.WellFormed() || fr.Length() == 0 || !hostarch.IsPageAligned(fr.Start) || !hostarch.IsPageAligned(fr.End) {
		panic(fmt.Sprintf("invalid range: %v", fr))
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	f.incRefLocked(fr)
}

// Preconditions: f.mu must be locked.
func (f *MemoryFile) incRefLocked(fr memmap.FileRange) {
	f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		unfree := &f.unfreeSmall
		if chunk.huge {
			unfree = &f.unfreeHuge
		}
		unfree.MutateFullRange(chunkFR, func(ufseg unfreeIterator) bool {
			uf := ufseg.ValuePtr()
			if uf.refs <= 0 {
				panic(fmt.Sprintf("IncRef(%v) called with %d references on pages %v", fr, uf.refs, ufseg.Range()))
			}
			uf.refs++
			return true
		})
		return true
	})
}

// DecRef implements memmap.File.DecRef.
func (f *MemoryFile) DecRef(fr memmap.FileRange) {
	if !fr.WellFormed() || fr.Length() == 0 || !hostarch.IsPageAligned(fr.Start) || !hostarch.IsPageAligned(fr.End) {
		panic(fmt.Sprintf("invalid range: %v", fr))
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	haveWaste := false
	f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		unwaste := &f.unwasteSmall
		unfree := &f.unfreeSmall
		if chunk.huge {
			unwaste = &f.unwasteHuge
			unfree = &f.unfreeHuge
		}
		unfree.MutateFullRange(chunkFR, func(ufseg unfreeIterator) bool {
			uf := ufseg.ValuePtr()
			if uf.refs <= 0 {
				panic(fmt.Sprintf("DecRef(%v) called with %d references on pages %v", fr, uf.refs, ufseg.Range()))
			}
			uf.refs--
			if uf.refs == 0 {
				// Mark these pages as waste.
				wasteFR := ufseg.Range()
				f.logicalPages.RemoveRange(wasteFR)
				unwaste.RemoveFullRange(wasteFR)
				haveWaste = true
				// Reclassify waste memory as System until it's recycled or
				// released.
				f.memAcct.MutateFullRange(wasteFR, func(maseg memAcctIterator) bool {
					ma := maseg.ValuePtr()
					if !f.opts.DisableMemoryAccounting && ma.knownCommitted {
						usage.MemoryAccounting.Move(maseg.Range().Length(), usage.System, ma.kind, ma.memCgID)
					}
					ma.kind = usage.System
					ma.wasteOrReleasing = true
					return true
				})
				// Cancel any pending async load on waste pages.
				if apl := f.asyncPageLoad.Load(); apl != nil {
					apl.cancelWasteLoad(wasteFR)
				}
			}
			return true
		})
		return true
	})

	// Wake the releaser if we marked any pages as waste. Leave this until just
	// before unlocking f.mu.
	if haveWaste && !f.haveWaste {
		f.haveWaste = true
		f.releaseCond.Signal()
	}
}

// releaserMain implements the releaser goroutine.
func (f *MemoryFile) releaserMain() {
	f.mu.Lock()
MainLoop:
	for {
		for {
			if f.destroyed {
				f.releaserDestroyLocked()
				f.mu.Unlock()
				// This must be called without holding f.mu to avoid circular lock
				// ordering.
				if f.stopNotifyPressure != nil {
					f.stopNotifyPressure()
				}
				return
			}
			if f.haveWaste {
				break
			}
			if f.opts.DelayedEviction == DelayedEvictionEnabled && !f.opts.UseHostMemcgPressure {
				// No work to do. Evict any pending evictable allocations to
				// get more waste pages before going to sleep.
				f.startEvictionsLocked()
			}
			f.releaseCond.Wait() // releases f.mu while waiting
		}
		// Huge pages are relatively rare and expensive due to fragmentation
		// and the cost of compaction. Fragmentation is expected to increase
		// over time. Most allocations are done upwards, with the main
		// exception being thread stacks. So we expect lower offsets to weakly
		// correlate with older allocations, which are more likely to actually
		// be hugepage-backed. Thus, release from unwasteSmall before
		// unwasteHuge, and higher offsets before lower ones.
		for i, unwaste := range []*unwasteSet{&f.unwasteSmall, &f.unwasteHuge} {
			if uwgap := unwaste.LastLargeEnoughGap(1); uwgap.Ok() {
				fr := uwgap.Range()
				// Linux serializes fallocate()s on shmem files, so limit the amount we
				// release at once to avoid starving Decommit().
				const maxReleasingBytes = 128 << 20 // 128 MB
				if fr.Length() > maxReleasingBytes {
					fr.Start = fr.End - maxReleasingBytes
				}
				unwaste.Insert(uwgap, fr, unwasteInfo{})
				f.releaseLocked(fr, i == 1)
				continue MainLoop
			}
		}
		f.haveWaste = false
	}
}

// Preconditions: f.mu must be locked; it may be unlocked and reacquired.
func (f *MemoryFile) releaseLocked(fr memmap.FileRange, huge bool) {
	defer func() {
		maseg := f.memAcct.LowerBoundSegmentSplitBefore(fr.Start)
		for maseg.Ok() && maseg.Start() < fr.End {
			maseg = f.memAcct.SplitAfter(maseg, fr.End)
			ma := maseg.ValuePtr()
			if ma.kind != usage.System {
				panic(fmt.Sprintf("waste pages %v have unexpected kind %v\n%s", maseg.Range(), ma.kind, f.stringLocked()))
			}
			if ma.knownCommitted {
				malen := maseg.Range().Length()
				f.knownCommittedBytes -= malen
				if !f.opts.DisableMemoryAccounting {
					usage.MemoryAccounting.Dec(malen, ma.kind, ma.memCgID)
				}
			}
			maseg = f.memAcct.Remove(maseg).NextSegment()
		}
	}()

	if !huge {
		// Decommit the range being released, then mark the released range as
		// freed.
		f.mu.Unlock()
		f.decommitOrManuallyZero(fr)
		f.mu.Lock()
		f.unfreeSmall.RemoveFullRange(fr)
		return
	}

	// Handle huge pages and sub-release.

	firstHugeStart := hostarch.HugePageRoundDown(fr.Start)
	lastHugeStart := hostarch.HugePageRoundDown(fr.End - 1)
	firstHugeEnd := firstHugeStart + hostarch.HugePageSize
	lastHugeEnd := lastHugeStart + hostarch.HugePageSize
	if firstHugeStart == lastHugeStart {
		// All of fr falls within a single huge page.
		oldSubrel := f.subreleased[firstHugeStart]
		incSubrel := fr.Length() / hostarch.PageSize
		newSubrel := oldSubrel + incSubrel
		if newSubrel == pagesPerHugePage {
			// Free this huge page.
			//
			// When a small page within a hugepage-backed allocation is
			// individually deallocated (becomes waste), we decommit it to
			// reduce memory usage (and for consistency with legacy behavior).
			// This requires the host to split the containing huge page, if one
			// exists. khugepaged may later re-assemble the containing huge
			// page, implicitly re-committing previously-decommitted small
			// pages as a result.
			//
			// Thus: When a huge page is freed, ensure that the whole huge page
			// is decommitted rather than just the final small page(s), to
			// ensure that we leave behind an uncommitted hugepage-sized range
			// with no re-committed small pages.
			if oldSubrel != 0 {
				delete(f.subreleased, firstHugeStart)
			}
			hugeFR := memmap.FileRange{firstHugeStart, firstHugeEnd}
			f.mu.Unlock()
			f.decommitOrManuallyZero(hugeFR)
			f.mu.Lock()
			f.unfreeHuge.RemoveFullRange(hugeFR)
		} else {
			f.subreleased[firstHugeStart] = newSubrel
			f.mu.Unlock()
			f.decommitOrManuallyZero(fr)
			f.mu.Lock()
		}
		return
	}

	// fr spans at least two huge pages. Resolve sub-release in the first and
	// last huge pages; any huge pages in between are decommitted/freed in
	// full.
	var (
		decommitFR memmap.FileRange
		freeFR     memmap.FileRange
	)
	if fr.Start == firstHugeStart {
		decommitFR.Start = firstHugeStart
		freeFR.Start = firstHugeStart
	} else {
		oldSubrel := f.subreleased[firstHugeStart]
		incSubrel := (firstHugeEnd - fr.Start) / hostarch.PageSize
		newSubrel := oldSubrel + incSubrel
		if newSubrel == pagesPerHugePage {
			if oldSubrel != 0 {
				delete(f.subreleased, firstHugeStart)
			}
			decommitFR.Start = firstHugeStart
			freeFR.Start = firstHugeStart
		} else {
			decommitFR.Start = fr.Start
			freeFR.Start = firstHugeEnd
		}
	}
	if fr.End == lastHugeEnd {
		decommitFR.End = lastHugeEnd
		freeFR.End = lastHugeEnd
	} else {
		oldSubrel := f.subreleased[lastHugeStart]
		incSubrel := (fr.End - lastHugeStart) / hostarch.PageSize
		newSubrel := oldSubrel + incSubrel
		if newSubrel == pagesPerHugePage {
			if oldSubrel != 0 {
				delete(f.subreleased, lastHugeStart)
			}
			decommitFR.End = lastHugeEnd
			freeFR.End = lastHugeEnd
		} else {
			decommitFR.End = fr.End
			freeFR.End = lastHugeStart
		}
	}
	f.mu.Unlock()
	f.decommitOrManuallyZero(decommitFR)
	f.mu.Lock()
	if freeFR.Length() != 0 {
		f.unfreeHuge.RemoveFullRange(freeFR)
	}
}

// MapInternal implements memmap.File.MapInternal.
func (f *MemoryFile) MapInternal(fr memmap.FileRange, at hostarch.AccessType) (safemem.BlockSeq, error) {
	if !fr.WellFormed() || fr.Length() == 0 {
		panic(fmt.Sprintf("invalid range: %v", fr))
	}
	if at.Execute {
		return safemem.BlockSeq{}, linuxerr.EACCES
	}

	if amfl := f.asyncPageLoad.Load(); amfl != nil {
		if err := amfl.awaitLoad(fr); err != nil {
			return safemem.BlockSeq{}, err
		}
	}
	if loads := f.sparseAsyncLoads.Load(); loads != nil {
		for _, amfl := range *loads {
			if err := amfl.awaitLoad(fr); err != nil {
				return safemem.BlockSeq{}, err
			}
		}
	}

	chunks := ((fr.End + chunkMask) / chunkSize) - (fr.Start / chunkSize)
	if chunks == 1 {
		// Avoid an unnecessary slice allocation.
		var seq safemem.BlockSeq
		f.forEachMappingSlice(fr, func(bs []byte) {
			seq = safemem.BlockSeqOf(safemem.BlockFromSafeSlice(bs))
		})
		return seq, nil
	}
	blocks := make([]safemem.Block, 0, chunks)
	f.forEachMappingSlice(fr, func(bs []byte) {
		blocks = append(blocks, safemem.BlockFromSafeSlice(bs))
	})
	return safemem.BlockSeqFromSlice(blocks), nil
}

// forEachMappingSlice invokes fn on a sequence of byte slices that
// collectively map all bytes in fr.
func (f *MemoryFile) forEachMappingSlice(fr memmap.FileRange, fn func([]byte)) {
	f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
		fn(chunk.sliceAt(chunkFR))
		return true
	})
}

// MarkEvictable allows f to request memory deallocation by calling
// user.Evict(er) in the future.
//
// Redundantly marking an already-evictable range as evictable has no effect.
func (f *MemoryFile) MarkEvictable(user EvictableMemoryUser, er EvictableRange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.evictable[user]
	if !ok {
		info = &evictableMemoryUserInfo{}
		f.evictable[user] = info
	}
	gap := info.ranges.LowerBoundGap(er.Start)
	for gap.Ok() && gap.Start() < er.End {
		gapER := gap.Range().Intersect(er)
		if gapER.Length() == 0 {
			gap = gap.NextGap()
			continue
		}
		gap = info.ranges.Insert(gap, gapER, evictableRangeSetValue{}).NextGap()
	}
	if !info.evicting {
		switch f.opts.DelayedEviction {
		case DelayedEvictionDisabled:
			// Kick off eviction immediately.
			f.startEvictionGoroutineLocked(user, info)
		case DelayedEvictionEnabled:
			if !f.opts.UseHostMemcgPressure {
				// Ensure that the releaser goroutine is running, so that it
				// can start eviction when necessary.
				f.releaseCond.Signal()
			}
		}
	}
}

// MarkUnevictable informs f that user no longer considers er to be evictable,
// so the MemoryFile should no longer call user.Evict(er). Note that, per
// EvictableMemoryUser.Evict's documentation, user.Evict(er) may still be
// called even after MarkUnevictable returns due to race conditions, and
// implementations of EvictableMemoryUser must handle this possibility.
//
// Redundantly marking an already-unevictable range as unevictable has no
// effect.
func (f *MemoryFile) MarkUnevictable(user EvictableMemoryUser, er EvictableRange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.evictable[user]
	if !ok {
		return
	}
	info.ranges.RemoveRange(er)
	// We can only remove info if there's no eviction goroutine running on its
	// behalf.
	if !info.evicting && info.ranges.IsEmpty() {
		delete(f.evictable, user)
	}
}

// MarkAllUnevictable informs f that user no longer considers any offsets to be
// evictable. It otherwise has the same semantics as MarkUnevictable.
func (f *MemoryFile) MarkAllUnevictable(user EvictableMemoryUser) {
	f.mu.Lock()
	defer f.mu.Unlock()
	info, ok := f.evictable[user]
	if !ok {
		return
	}
	info.ranges.RemoveAll()
	// We can only remove info if there's no eviction goroutine running on its
	// behalf.
	if !info.evicting {
		delete(f.evictable, user)
	}
}

// ShouldCacheEvictable returns true if f is meaningfully delaying evictions of
// evictable memory, such that it may be advantageous to cache data in
// evictable memory. The value returned by ShouldCacheEvictable may change
// between calls.
func (f *MemoryFile) ShouldCacheEvictable() bool {
	return f.opts.DelayedEviction == DelayedEvictionManual || f.opts.UseHostMemcgPressure
}

// commitScanIntervalRatio is the minimum ratio of the interval between
// consecutive scans for committed pages to the duration of the preceding
// scan, limiting scanning to 1/commitScanIntervalRatio of a CPU. It trades
// scanning cost against how far usage.MemoryAccounting may lag: scans cost
// tens of milliseconds for tens of GB of memory, but seconds once a sandbox
// commits hundreds of GB. If the previous scan took 2 seconds, the next scan
// will be delayed by at least 16 seconds.
const commitScanIntervalRatio = 8

// UpdateUsage attempts to bring the memory usage statistics in
// usage.MemoryAccounting up to date. If memCgIDs is nil, all the pages
// will be scanned. Else only the pages which belong to the memory cgroup ids
// in memCgIDs will be scanned and the memory usage will be updated.
//
// Scanning is throttled, so statistics may lag page commitment by an
// unbounded amount; only callers that tolerate stale statistics may use
// UpdateUsage.
func (f *MemoryFile) UpdateUsage(memCgIDs map[uint32]struct{}) error {
	if memCgIDs == nil && f.ongoingCommitScan.Load() {
		return nil
	}

	// If we already know of every committed page, skip scanning.
	currentUsage, err := f.TotalUsage()
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if currentUsage == f.knownCommittedBytes {
		return nil
	}

	if f.isSaving {
		log.Debugf("pgalloc.MemoryFile.UpdateUsage() inhibited during MemoryFile save")
		return nil
	}

	// Linux updates usage values at CONFIG_HZ, so scanning more frequently is
	// never useful. Scanning is also far more expensive for us than it is for
	// Linux: pages are committed by the host kernel without our involvement,
	// so each scan must ask the host about every page not already known to be
	// committed, making it O(uncommitted pages). So scanning is throttled
	// proportional to the previous scan duration to avoid excessive CPU usage.
	startTime := time.Now()
	if memCgIDs == nil {
		if startTime.Before(f.nextCommitScan) || f.ongoingCommitScan.Load() {
			return nil
		}
		f.ongoingCommitScan.Store(true)
	}
	err = f.updateUsageLocked(memCgIDs)
	if memCgIDs == nil {
		f.ongoingCommitScan.Store(false)
	}
	scanTime := time.Since(startTime)
	if memCgIDs == nil {
		f.nextCommitScan = startTime.Add(max(linux.ClockTick, scanTime*commitScanIntervalRatio))
	}

	if _, ok := err.(updateUsageDuringSaveErr); ok {
		log.Debugf("pgalloc.MemoryFile.UpdateUsage() inhibited during MemoryFile save")
		return nil
	}
	if log.IsLogging(log.Debug) {
		log.Debugf("UpdateUsage: took %v, currentUsage=%d knownCommittedBytes=%d",
			scanTime, currentUsage, f.knownCommittedBytes)
	}
	return err
}

// updateUsageLocked attempts to detect commitment of previously-uncommitted
// pages, and updates memory accounting to reflect newly-committed pages.
//
// Precondition: f.mu must be held; it may be unlocked and reacquired.
// +checklocks:f.mu
func (f *MemoryFile) updateUsageLocked(memCgIDs map[uint32]struct{}) error {
	// Track if anything changed to elide the merge.
	changedAny := false
	defer func() {
		if changedAny {
			f.memAcct.MergeAll()
		}
	}()

	// Reused mincore buffer.
	var buf []byte

	maseg := f.memAcct.FirstSegment()
	unscannedStart := uint64(0)
	for maseg.Ok() {
		ma := maseg.ValuePtr()
		if ma.wasteOrReleasing {
			// Skip scanning of waste and releasing pages. This isn't
			// necessarily correct, since !knownCommitted may have become
			// committed after the last call to updateUsageLocked(), then
			// transitioned from used to waste. However, this is consistent
			// with legacy behavior.
			maseg = maseg.NextSegment()
			continue
		}
		if ma.knownCommitted {
			// Known-committed pages remain known-committed until they are
			// decommitted.
			maseg = maseg.NextSegment()
			continue
		}

		// Scan the pages of the given memCgID only. This will avoid scanning
		// the whole memory file when the memory usage is required only for a
		// specific cgroup. The total memory usage of all cgroups can be
		// obtained when memCgIDs is nil.
		if memCgIDs != nil {
			if _, ok := memCgIDs[ma.memCgID]; !ok {
				maseg = maseg.NextSegment()
				continue
			}
		}

		fr := maseg.Range()
		if fr.Start < unscannedStart {
			fr.Start = unscannedStart
		}
		var checkErr error
		f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
			s := chunk.sliceAt(chunkFR)

			// Ensure that we have sufficient buffer for the call (one byte per
			// page). The length of s must be page-aligned.
			bufLen := len(s) / hostarch.PageSize
			if len(buf) < bufLen {
				buf = make([]byte, bufLen)
			}

			// Query for new pages in core.
			// NOTE(b/165896008): mincore might take a really long time. So
			// unlock f.mu while mincore runs.
			lastCommitSeq := f.commitSeq
			f.commitSeq++
			f.mu.Unlock() // +checklocksforce
			err := mincore(s, buf)
			f.mu.Lock()
			if err != nil {
				checkErr = err
				return false
			}

			// Reconcile internal state with buf. Since we temporarily dropped
			// f.mu, f.isSaving and f.memAcct may have changed, and maseg/ma
			// are no longer valid. If wasCommitted is false, then we are
			// marking ranges that are now committed; otherwise, we are marking
			// ranges that are now uncommitted.
			if f.isSaving {
				checkErr = updateUsageDuringSaveErr{}
				return false
			}
			maseg = f.memAcct.LowerBoundSegment(chunkFR.Start)
			for i := 0; i < bufLen; {
				if buf[i]&0x1 == 0 {
					i++
					continue
				}
				// Scan to the end of this newly-committed range.
				j := i + 1
				for ; j < bufLen; j++ {
					if buf[j]&0x1 == 0 {
						break
					}
				}
				commitFR := memmap.FileRange{
					Start: chunkFR.Start + uint64(i*hostarch.PageSize),
					End:   chunkFR.Start + uint64(j*hostarch.PageSize),
				}
				// Advance maseg to commitFR.Start.
				for maseg.Ok() && maseg.End() <= commitFR.Start {
					maseg = maseg.NextSegment()
				}
				// Update pages overlapping commitFR, but don't mark ranges as
				// committed if they might have raced with decommit.
				for maseg.Ok() && maseg.Start() < commitFR.End {
					if ma := maseg.ValuePtr(); !ma.wasteOrReleasing && !ma.knownCommitted && ma.commitSeq <= lastCommitSeq {
						maseg = f.memAcct.Isolate(maseg, commitFR)
						ma := maseg.ValuePtr()
						amount := maseg.Range().Length()
						ma.knownCommitted = true
						ma.commitSeq = 0
						f.knownCommittedBytes += amount
						if !f.opts.DisableMemoryAccounting {
							usage.MemoryAccounting.Inc(amount, ma.kind, ma.memCgID)
						}
						changedAny = true
					}
					maseg = maseg.NextSegment()
				}
				// Continue scanning for changed pages.
				i = j + 1
			}

			// Don't continue to the next chunk, since while f.mu was unlocked
			// its memory accounting state could have changed completely.
			// Instead, continue the outer loop with the first segment after
			// chunkFR.End.
			maseg = f.memAcct.LowerBoundSegment(chunkFR.End)
			unscannedStart = chunkFR.End
			return false
		})
		if checkErr != nil {
			return checkErr
		}
	}

	return nil
}

type updateUsageDuringSaveErr struct{}

// Error implements error.Error.
func (updateUsageDuringSaveErr) Error() string {
	return "pgalloc.MemoryFile.UpdateUsage() called during MemoryFile save"
}

// TotalUsage returns an aggregate usage for all memory statistics except
// Mapped (which is external to MemoryFile). This is generally much cheaper
// than UpdateUsage, but will not provide a fine-grained breakdown.
func (f *MemoryFile) TotalUsage() (uint64, error) {
	// Stat the underlying file to discover the underlying usage. stat(2)
	// always reports the allocated block count in units of 512 bytes. This
	// includes pages in the page cache and swapped pages.
	var stat unix.Stat_t
	if err := unix.Fstat(int(f.file.Fd()), &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Blocks * 512), nil
}

// TotalSize returns the current size of the backing file in bytes, which is an
// upper bound on the amount of memory that can currently be allocated from the
// MemoryFile. The value returned by TotalSize is permitted to change.
func (f *MemoryFile) TotalSize() uint64 {
	return uint64(len(f.chunksLoad())) * chunkSize
}

// File returns the backing file.
func (f *MemoryFile) File() *os.File {
	return f.file
}

// DataFD implements memmap.File.DataFD.
func (f *MemoryFile) DataFD(fr memmap.FileRange) (int, error) {
	if amfl := f.asyncPageLoad.Load(); amfl != nil {
		if err := amfl.awaitLoad(fr); err != nil {
			return -1, err
		}
	}
	if loads := f.sparseAsyncLoads.Load(); loads != nil {
		for _, amfl := range *loads {
			if err := amfl.awaitLoad(fr); err != nil {
				return -1, err
			}
		}
	}
	return f.FD(), nil
}

// FD implements memmap.File.FD.
func (f *MemoryFile) FD() int {
	return int(f.file.Fd())
}

// IsDiskBacked returns true if f is backed by a file on disk.
func (f *MemoryFile) IsDiskBacked() bool {
	return f.opts.DiskBackedFile
}

type cacheDropCandidate struct {
	range_       memmap.FileRange
	expectedRefs uint64
}

// SyncAndDropUniqueCache writes dirty mappings to the backing file, then asks
// the host to evict only pages in candidates that currently have one
// MemoryFile reference. TTC dehydration holds that last reference through a
// snapshot reservation, so these pages are private to the dehydrated branch.
// Shared CoW pages are deliberately left mapped for live siblings.
func (f *MemoryFile) SyncAndDropUniqueCache(candidates []memmap.FileRange) error {
	owned := make([]cacheDropCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		owned = append(owned, cacheDropCandidate{range_: candidate, expectedRefs: 1})
	}
	return f.syncAndDropCache(owned)
}

// syncAndDropCache evicts candidate subranges only when their complete
// MemoryFile reference count is accounted for by dehydration reservations.
func (f *MemoryFile) syncAndDropCache(candidates []cacheDropCandidate) error {
	if f.file == nil {
		return fmt.Errorf("MemoryFile has no backing file")
	}
	if err := f.file.Sync(); err != nil {
		return fmt.Errorf("syncing MemoryFile: %w", err)
	}
	var unique []memmap.FileRange
	f.mu.Lock()
	for _, candidate := range candidates {
		f.forEachChunk(candidate.range_, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
			unfree := &f.unfreeSmall
			if chunk.huge {
				unfree = &f.unfreeHuge
			}
			unfree.VisitFullRange(chunkFR, func(seg unfreeIterator) bool {
				if seg.ValuePtr().refs != candidate.expectedRefs {
					return true
				}
				fr := seg.Range().Intersect(chunkFR)
				if n := len(unique); n != 0 && unique[n-1].End == fr.Start {
					unique[n-1].End = fr.End
				} else {
					unique = append(unique, fr)
				}
				return true
			})
			return true
		})
	}
	f.mu.Unlock()
	for _, fr := range unique {
		var dropErr error
		f.forEachChunk(fr, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
			if chunk.mapping == 0 {
				return true
			}
			chunkStart := chunkFR.Start &^ chunkMask
			addr := chunk.mapping + uintptr(chunkFR.Start-chunkStart)
			_, _, errno := unix.Syscall(unix.SYS_MADVISE, addr, uintptr(chunkFR.Length()), unix.MADV_DONTNEED)
			if errno != 0 {
				dropErr = errno
				return false
			}
			return true
		})
		if dropErr != nil {
			return fmt.Errorf("dropping MemoryFile mapping cache for %v: %w", fr, dropErr)
		}
	}
	return nil
}

// HugepagesEnabled returns true if the MemoryFile expects to back allocations
// for which AllocOpts.Huge == true with huge pages.
func (f *MemoryFile) HugepagesEnabled() bool {
	return f.opts.ExpectHugepages
}

// String implements fmt.Stringer.String.
func (f *MemoryFile) String() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stringLocked()
}

// Preconditions: f.mu must be locked.
func (f *MemoryFile) stringLocked() string {
	var b strings.Builder
	fmt.Fprintf(&b, "unwasteSmall:\n%s", &f.unwasteSmall)
	if f.opts.ExpectHugepages {
		fmt.Fprintf(&b, "unwasteHuge:\n%s", &f.unwasteHuge)
	}
	fmt.Fprintf(&b, "unfreeSmall:\n%s", &f.unfreeSmall)
	if f.opts.ExpectHugepages {
		fmt.Fprintf(&b, "unfreeHuge:\n%s", &f.unfreeHuge)
		fmt.Fprintf(&b, "subreleased:\n")
		for off, pgs := range f.subreleased {
			fmt.Fprintf(&b, "- %#x: %d\n", off, pgs)
		}
	}
	fmt.Fprintf(&b, "memAcct:\n%s", &f.memAcct)
	return b.String()
}

// StartEvictions requests that f evict all evictable allocations. It does not
// wait for eviction to complete; for this, see MemoryFile.WaitForEvictions.
func (f *MemoryFile) StartEvictions() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.startEvictionsLocked()
}

// Preconditions: f.mu must be locked.
func (f *MemoryFile) startEvictionsLocked() bool {
	startedAny := false
	for user, info := range f.evictable {
		// Don't start multiple goroutines to evict the same user's
		// allocations.
		if !info.evicting {
			f.startEvictionGoroutineLocked(user, info)
			startedAny = true
		}
	}
	return startedAny
}

// Preconditions:
//   - info == f.evictable[user].
//   - !info.evicting.
//   - f.mu must be locked.
func (f *MemoryFile) startEvictionGoroutineLocked(user EvictableMemoryUser, info *evictableMemoryUserInfo) {
	info.evicting = true
	f.evictionWG.Add(1)
	go func() { // S/R-SAFE: f.evictionWG
		defer f.evictionWG.Done()
		for {
			f.mu.Lock()
			info, ok := f.evictable[user]
			if !ok {
				// This shouldn't happen: only this goroutine is permitted
				// to delete this entry.
				f.mu.Unlock()
				panic(fmt.Sprintf("evictableMemoryUserInfo for EvictableMemoryUser %v deleted while eviction goroutine running", user))
			}
			if info.ranges.IsEmpty() {
				delete(f.evictable, user)
				f.mu.Unlock()
				return
			}
			// Evict from the end of info.ranges, under the assumption that
			// if ranges in user start being used again (and are
			// consequently marked unevictable), such uses are more likely
			// to start from the beginning of user.
			seg := info.ranges.LastSegment()
			er := seg.Range()
			info.ranges.Remove(seg)
			// user.Evict() must be called without holding f.mu to avoid
			// circular lock ordering.
			f.mu.Unlock()
			user.Evict(context.Background(), er)
		}
	}()
}

// WaitForEvictions blocks until f is no longer evicting any evictable
// allocations.
func (f *MemoryFile) WaitForEvictions() {
	f.evictionWG.Wait()
}

type unwasteSetFunctions struct{}

func (unwasteSetFunctions) MinKey() uint64 {
	return 0
}

func (unwasteSetFunctions) MaxKey() uint64 {
	return math.MaxUint64
}

func (unwasteSetFunctions) ClearValue(val *unwasteInfo) {
}

func (unwasteSetFunctions) Merge(_ memmap.FileRange, val1 unwasteInfo, _ memmap.FileRange, val2 unwasteInfo) (unwasteInfo, bool) {
	return val1, val1 == val2
}

func (unwasteSetFunctions) Split(_ memmap.FileRange, val unwasteInfo, _ uint64) (unwasteInfo, unwasteInfo) {
	return val, val
}

type unfreeSetFunctions struct{}

func (unfreeSetFunctions) MinKey() uint64 {
	return 0
}

func (unfreeSetFunctions) MaxKey() uint64 {
	return math.MaxUint64
}

func (unfreeSetFunctions) ClearValue(val *unfreeInfo) {
}

func (unfreeSetFunctions) Merge(_ memmap.FileRange, val1 unfreeInfo, _ memmap.FileRange, val2 unfreeInfo) (unfreeInfo, bool) {
	return val1, val1 == val2
}

func (unfreeSetFunctions) Split(_ memmap.FileRange, val unfreeInfo, _ uint64) (unfreeInfo, unfreeInfo) {
	return val, val
}

type logicalPageSetFunctions struct{}

func (logicalPageSetFunctions) MinKey() uint64 {
	return 0
}

func (logicalPageSetFunctions) MaxKey() uint64 {
	return math.MaxUint64
}

func (logicalPageSetFunctions) ClearValue(val *logicalPageInfo) {
	*val = logicalPageInfo{}
}

func (logicalPageSetFunctions) Merge(r1 memmap.FileRange, val1 logicalPageInfo, r2 memmap.FileRange, val2 logicalPageInfo) (logicalPageInfo, bool) {
	if val1.Allocator != val2.Allocator || val1.VersionAllocator != val2.VersionAllocator {
		return logicalPageInfo{}, false
	}
	pages := r1.Length() / hostarch.PageSize
	wantPage := val1.FirstSequence + pages
	wantVersion := val1.FirstVersionSequence + pages
	return val1, wantPage == val2.FirstSequence && wantVersion == val2.FirstVersionSequence && r1.End == r2.Start
}

func (logicalPageSetFunctions) Split(r memmap.FileRange, val logicalPageInfo, splitAt uint64) (logicalPageInfo, logicalPageInfo) {
	right := val
	pages := (splitAt - r.Start) / hostarch.PageSize
	right.FirstSequence += pages
	right.FirstVersionSequence += pages
	return val, right
}

type memAcctSetFunctions struct{}

func (memAcctSetFunctions) MinKey() uint64 {
	return 0
}

func (memAcctSetFunctions) MaxKey() uint64 {
	return math.MaxUint64
}

func (memAcctSetFunctions) ClearValue(val *memAcctInfo) {
}

func (memAcctSetFunctions) Merge(_ memmap.FileRange, val1 memAcctInfo, _ memmap.FileRange, val2 memAcctInfo) (memAcctInfo, bool) {
	return val1, val1 == val2
}

func (memAcctSetFunctions) Split(_ memmap.FileRange, val memAcctInfo, _ uint64) (memAcctInfo, memAcctInfo) {
	return val, val
}

// evictableRangeSetValue is the value type of evictableRangeSet.
type evictableRangeSetValue struct{}

type evictableRangeSetFunctions struct{}

func (evictableRangeSetFunctions) MinKey() uint64 {
	return 0
}

func (evictableRangeSetFunctions) MaxKey() uint64 {
	return math.MaxUint64
}

func (evictableRangeSetFunctions) ClearValue(val *evictableRangeSetValue) {
}

func (evictableRangeSetFunctions) Merge(_ EvictableRange, _ evictableRangeSetValue, _ EvictableRange, _ evictableRangeSetValue) (evictableRangeSetValue, bool) {
	return evictableRangeSetValue{}, true
}

func (evictableRangeSetFunctions) Split(_ EvictableRange, _ evictableRangeSetValue, _ uint64) (evictableRangeSetValue, evictableRangeSetValue) {
	return evictableRangeSetValue{}, evictableRangeSetValue{}
}
