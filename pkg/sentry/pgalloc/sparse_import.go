// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"slices"
	"sort"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// InstallRemoteBindings installs or reuses live-transport chunks identified
// by stable logical page versions. Unlike durable object IDs, these keys do
// not claim to hash page contents; their domain prefix keeps the two identity
// schemes distinct. The returned ranges read directly from the capture's
// packed TCP stream.
//
// A 2 MiB page is a unit of its own, installed in an aligned 2 MiB huge page
// as one page (see logicalPageInfo.Huge), and read in one piece; a file
// without huge pages installs its base pages instead.
func (f *MemoryFile) InstallRemoteBindings(bindings []CapturedPageBinding, kind usage.MemoryKind, memCgID uint32) (*SparseChunkInstallation, *SparseRelocation, []RemoteLoadRange, error) {
	type chunkInstance struct {
		id       LogicalChunkID
		instance uint32
	}
	if !f.HugepagesEnabled() {
		bindings = BasePageBindings(bindings)
	}
	orderedBindings := append([]CapturedPageBinding(nil), bindings...)
	sort.Slice(orderedBindings, func(i, j int) bool { return orderedBindings[i].StreamOffset < orderedBindings[j].StreamOffset })
	byChunk := make(map[chunkInstance][]CapturedPageBinding)
	current := make(map[LogicalChunkID]chunkInstance)
	used := make(map[chunkInstance][4]uint64)
	var chunks []chunkInstance
	// As CapturedMemoryLayout.ChunkOrdered numbers them.
	hugeInstance := uint32(1 << 31)
	for _, binding := range orderedBindings {
		chunk, slot := binding.Page.Chunk()
		if binding.ResourceID.Ok() || binding.Chunk != chunk || binding.Slot != slot || binding.References == 0 ||
			(binding.Huge && !ValidHugePageBinding(&binding)) {
			return nil, nil, nil, fmt.Errorf("invalid live sparse binding %+v", binding)
		}
		if binding.Huge {
			key := chunkInstance{id: chunk, instance: hugeInstance}
			hugeInstance++
			chunks = append(chunks, key)
			byChunk[key] = []CapturedPageBinding{binding}
			continue
		}
		key, ok := current[chunk]
		if !ok {
			key = chunkInstance{id: chunk}
			current[chunk] = key
			chunks = append(chunks, key)
		}
		bitmap := used[key]
		if bitmap[slot/64]&(uint64(1)<<(slot%64)) != 0 {
			key.instance++
			current[chunk] = key
			chunks = append(chunks, key)
			bitmap = used[key]
		}
		bitmap[slot/64] |= uint64(1) << (slot % 64)
		used[key] = bitmap
		byChunk[key] = append(byChunk[key], binding)
	}
	installs := make([]SparseChunkInstall, 0, len(chunks))
	objectBindings := make(map[SparseChunkObjectID][]CapturedPageBinding, len(chunks))
	for _, key := range chunks {
		chunk := key.id
		pages := byChunk[key]
		if binding := pages[0]; binding.Huge {
			hash := sha256.New()
			hash.Write([]byte("ttc-live-2mib-page-version-v1\x00"))
			var encoded [8]byte
			hash.Write(binding.Page.Allocator[:])
			binary.BigEndian.PutUint64(encoded[:], binding.Page.Sequence)
			hash.Write(encoded[:])
			hash.Write(binding.Version.Allocator[:])
			binary.BigEndian.PutUint64(encoded[:], binding.Version.Sequence)
			hash.Write(encoded[:])
			install := SparseChunkInstall{Kind: kind, MemCgID: memCgID, Huge: true, Runs: []SparseChunkRun{{
				Position: 0, Pages: uint16(pagesPerHugePage()), Page: binding.Page, Version: binding.Version,
			}}}
			copy(install.Object[:], hash.Sum(nil))
			installs = append(installs, install)
			objectBindings[install.Object] = pages
			continue
		}
		sort.Slice(pages, func(i, j int) bool { return pages[i].Slot < pages[j].Slot })
		hash := sha256.New()
		hash.Write([]byte("ttc-live-page-versions-v1\x00"))
		hash.Write(chunk.Allocator[:])
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], chunk.Sequence)
		hash.Write(encoded[:])
		install := SparseChunkInstall{Kind: kind, MemCgID: memCgID}
		for i, binding := range pages {
			if i != 0 && pages[i-1].Slot == binding.Slot {
				return nil, nil, nil, fmt.Errorf("live sparse chunk repeats slot %d", binding.Slot)
			}
			hash.Write([]byte{binding.Slot})
			hash.Write(binding.Page.Allocator[:])
			binary.BigEndian.PutUint64(encoded[:], binding.Page.Sequence)
			hash.Write(encoded[:])
			hash.Write(binding.Version.Allocator[:])
			binary.BigEndian.PutUint64(encoded[:], binding.Version.Sequence)
			hash.Write(encoded[:])
			install.Runs = append(install.Runs, SparseChunkRun{Position: binding.Slot, Pages: 1, Page: binding.Page, Version: binding.Version})
		}
		copy(install.Object[:], hash.Sum(nil))
		installs = append(installs, install)
		objectBindings[install.Object] = pages
	}
	transaction, err := f.InstallSparseChunks(installs)
	if err != nil {
		return nil, nil, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Abort()
		}
	}()
	relocation := NewSparseRelocation()
	newRanges := transaction.NewRanges()
	var loads []RemoteLoadRange
	for i, install := range installs {
		key := chunks[i]
		chunkLoad := &logicalChunkLoad{id: key.id, instance: key.instance}
		resident, ok := f.SparseChunkRange(install.Object)
		if !ok {
			return nil, nil, nil, fmt.Errorf("installed live chunk %x has no resident range", install.Object)
		}
		_, isNew := newRanges[install.Object]
		for _, binding := range objectBindings[install.Object] {
			// A 2 MiB page's slot is 0.
			target := resident.Start + uint64(binding.Slot)*hostarch.PageSize
			if err := relocation.AddRange(binding.ResourceID, memmap.FileRange{Start: binding.OldOffset, End: binding.OldOffset + binding.Length()}, target); err != nil {
				return nil, nil, nil, err
			}
			if isNew {
				load := RemoteLoadRange{Target: memmap.FileRange{Start: target, End: target + binding.Length()}, StreamOffset: binding.StreamOffset, logicalChunk: chunkLoad}
				loads = append(loads, load)
				chunkLoad.ranges = append(chunkLoad.ranges, load)
			}
		}
	}
	loads = orderAndCoalesceRemoteLoads(loads)
	committed = true
	return transaction, relocation, loads, nil
}

func orderAndCoalesceRemoteLoads(loads []RemoteLoadRange) []RemoteLoadRange {
	// The async reader consumes one contiguous packed-stream interval and may
	// scatter it into discontiguous destination ranges. Existing resident
	// chunks make destination allocation order unrelated to stream order, so
	// sorting by Target would split or interleave atomic logical chunks.
	sort.Slice(loads, func(i, j int) bool { return loads[i].StreamOffset < loads[j].StreamOffset })
	coalesced := loads[:0]
	for _, load := range loads {
		if len(coalesced) != 0 {
			last := &coalesced[len(coalesced)-1]
			if last.logicalChunk == load.logicalChunk && last.Target.End == load.Target.Start && last.StreamOffset+last.Target.Length() == load.StreamOffset {
				last.Target.End = load.Target.End
				continue
			}
		}
		coalesced = append(coalesced, load)
	}
	return coalesced
}

const SparseChunkBytes = logicalPagesPerChunk * hostarch.PageSize

// PagesPerSparseChunk returns the number of base pages in one stable sparse
// residency unit.
func PagesPerSparseChunk() int { return logicalPagesPerChunk }

// SparseChunkObjectID is the content address of one immutable TTC memory
// chunk. pgalloc treats it as opaque to avoid depending on the chunk codec.
type SparseChunkObjectID [32]byte

// SparseChunkRun describes Pages valid pages of a sparse chunk at
// consecutive positions, whose page and version identities are consecutive
// too. Position is the first page's index in the chunk object, where its
// bytes are. A chunk may hold pages of several logical chunks, packed
// together.
type SparseChunkRun struct {
	Position uint8
	Pages    uint16
	Page     LogicalPageID
	Version  PageVersionID
}

// valid reports whether run's pages are in a chunk and have identities.
func (run SparseChunkRun) valid() bool {
	last := uint64(run.Pages) - 1
	return run.Pages != 0 && uint16(run.Position)+run.Pages <= logicalPagesPerChunk &&
		run.Page.Allocator != (LogicalAllocatorID{}) && run.Version.Allocator != (LogicalAllocatorID{}) &&
		run.Page.Sequence <= math.MaxUint64-last && run.Version.Sequence <= math.MaxUint64-last
}

// aligns reports whether a page at a position of both run and next, which
// starts no earlier, would have the same identities in each.
func (run SparseChunkRun) aligns(next SparseChunkRun) bool {
	delta := uint64(next.Position - run.Position)
	return run.Page.Allocator == next.Page.Allocator && run.Version.Allocator == next.Version.Allocator &&
		run.Page.Sequence <= math.MaxUint64-delta && run.Page.Sequence+delta == next.Page.Sequence &&
		run.Version.Sequence <= math.MaxUint64-delta && run.Version.Sequence+delta == next.Version.Sequence
}

// normalizeSparseChunkRuns orders runs by position, lists a page given more
// than once with one identity once, and merges runs whose identities continue
// each other's. It fails if a page is given two identities. It reuses runs.
func normalizeSparseChunkRuns(runs []SparseChunkRun) ([]SparseChunkRun, error) {
	sort.Slice(runs, func(i, j int) bool { return runs[i].Position < runs[j].Position })
	normalized := runs[:0]
	for _, run := range runs {
		if len(normalized) != 0 {
			last := &normalized[len(normalized)-1]
			end := uint16(last.Position) + last.Pages
			aligned := uint16(run.Position) <= end && last.aligns(run)
			if uint16(run.Position) < end && !aligned {
				return nil, fmt.Errorf("page %d has two identities", run.Position)
			}
			if aligned {
				last.Pages = max(end, uint16(run.Position)+run.Pages) - uint16(last.Position)
				continue
			}
		}
		normalized = append(normalized, run)
	}
	return normalized, nil
}

// SparseChunkInstall describes one immutable 1 MiB residency unit, or one 2
// MiB page.
type SparseChunkInstall struct {
	Object  SparseChunkObjectID
	Runs    []SparseChunkRun
	Kind    usage.MemoryKind
	MemCgID uint32
	// Huge is true if the unit is one 2 MiB page, which Runs' one run names
	// from position 0: it is installed in an aligned 2 MiB huge page, as a 2
	// MiB page (see logicalPageInfo.Huge).
	Huge bool
	// Parts, if any, are the objects whose bytes make up the unit, which
	// ReclaimedSparseChunks reports for it: the halves of a 2 MiB page that
	// an image holds apart.
	Parts []SparseChunkObjectID
}

// validHugeSparseChunkRuns reports whether runs name one 2 MiB page whole, as
// a huge SparseChunkInstall's must.
func validHugeSparseChunkRuns(runs []SparseChunkRun) bool {
	if len(runs) != 1 {
		return false
	}
	run := runs[0]
	return run.Position == 0 && uint64(run.Pages) == pagesPerHugePage() &&
		run.Page.Allocator != (LogicalAllocatorID{}) && run.Version.Allocator != (LogicalAllocatorID{}) &&
		run.Page.Sequence%pagesPerHugePage() == 0 && run.Version.Sequence%pagesPerHugePage() == 0 &&
		run.Page.Sequence <= math.MaxUint64-pagesPerHugePage() && run.Version.Sequence <= math.MaxUint64-pagesPerHugePage()
}

type sparseResidentChunk struct {
	Range memmap.FileRange
	// Runs are the unit's pages its installations used, as
	// normalizeSparseChunkRuns leaves them.
	Runs []SparseChunkRun
	// Parts are SparseChunkInstall.Parts.
	Parts []SparseChunkObjectID
}

// SparseChunkInstallation is a transactional set of new residency units.
// Restored owners acquire their ordinary page references through state load;
// the installation itself owns only each chunk's cache reference.
type SparseChunkInstallation struct {
	mf        *MemoryFile
	newRanges map[SparseChunkObjectID]memmap.FileRange
	// objects are the units the installation installs or reuses, which
	// ReclaimUnusedSparseChunks leaves until it is committed or aborted.
	objects []SparseChunkObjectID
	// loads are the loaders of the pages still to be loaded in the units the
	// installation reuses, each with an owner reference of the
	// installation's: the owners it restores read those pages too, and a
	// loader stops, leaving them unloaded, once its last owner goes.
	// TakePageLoads hands them over; Commit and Abort release those not
	// taken.
	loads []*AsyncPagesFileLoad
	done  bool
}

type sparsePendingReference struct {
	mf      *MemoryFile
	range_  memmap.FileRange
	memCgID uint32
}

// SparseReferenceTransaction stages state-graph references while a shared
// family MemoryFile is being restored. Chunk cache references keep all pages
// allocated until Commit, so a failed state load can discard the transaction
// without modifying allocator counts.
type SparseReferenceTransaction struct {
	references []sparsePendingReference
	// allocations already carry their restored owner's reference. They are
	// retained on commit and released on abort, unlike references which are
	// acquired only at commit time.
	allocations []sparsePendingReference
	done        bool
}

// NewSparseReferenceTransaction creates an empty restore transaction.
func NewSparseReferenceTransaction() *SparseReferenceTransaction {
	return &SparseReferenceTransaction{}
}

// Add stages one reference. It panics on invalid post-relocation ranges in the
// same manner as MemoryFile.IncRef.
func (t *SparseReferenceTransaction) Add(mf *MemoryFile, fr memmap.FileRange, memCgID uint32) {
	if t == nil || t.done || mf == nil || !fr.WellFormed() || fr.Length() == 0 ||
		!hostarch.IsPageAligned(fr.Start) || !hostarch.IsPageAligned(fr.End) {
		panic(fmt.Sprintf("invalid sparse restore reference: mf=%p range=%v", mf, fr))
	}
	t.references = append(t.references, sparsePendingReference{mf: mf, range_: fr, memCgID: memCgID})
}

// AddAllocation adopts an already-allocated range for the restored state
// graph. The allocation's initial reference becomes the object's reference on
// Commit and is released on Abort.
func (t *SparseReferenceTransaction) AddAllocation(mf *MemoryFile, fr memmap.FileRange, memCgID uint32) {
	if t == nil || t.done || mf == nil || !fr.WellFormed() || fr.Length() == 0 ||
		!hostarch.IsPageAligned(fr.Start) || !hostarch.IsPageAligned(fr.End) {
		panic(fmt.Sprintf("invalid sparse restore allocation: mf=%p range=%v", mf, fr))
	}
	t.allocations = append(t.allocations, sparsePendingReference{mf: mf, range_: fr, memCgID: memCgID})
}

// CheckHeldBy returns an error unless s holds every page t stages a reference
// to. A restore in the Sentry that made s takes the pages its state refers to
// where they are, and s, which holds the pages the capture found the state to
// reach, kept those from being freed and reused since: a page outside s may
// have been.
func (t *SparseReferenceTransaction) CheckHeldBy(s *RemoteSnapshot) error {
	if t == nil || t.done {
		return fmt.Errorf("sparse restore reference transaction is unavailable")
	}
	if len(t.references) == 0 {
		return nil
	}
	if s == nil {
		return fmt.Errorf("restored state refers to MemoryFile range %v, but no snapshot holds pages", t.references[0].range_)
	}
	held, err := s.heldRanges()
	if err != nil {
		return err
	}
	for _, reference := range t.references {
		ranges := held[reference.mf]
		i := sort.Search(len(ranges), func(i int) bool { return ranges[i].End > reference.range_.Start })
		if i == len(ranges) || ranges[i].Start > reference.range_.Start || ranges[i].End < reference.range_.End {
			return fmt.Errorf("restored state refers to MemoryFile range %v, which the capture does not hold", reference.range_)
		}
	}
	return nil
}

// Commit installs every staged reference.
func (t *SparseReferenceTransaction) Commit() error {
	if t == nil || t.done {
		return fmt.Errorf("sparse restore reference transaction is unavailable")
	}
	t.done = true
	for _, reference := range t.references {
		reference.mf.IncRef(reference.range_, reference.memCgID)
	}
	t.references = nil
	t.allocations = nil
	return nil
}

// Abort discards staged references.
func (t *SparseReferenceTransaction) Abort() {
	if t == nil || t.done {
		return
	}
	t.done = true
	t.references = nil
	for _, allocation := range t.allocations {
		allocation.mf.DecRef(allocation.range_)
	}
	t.allocations = nil
}

// NewRanges returns a detached map of chunks that require page loading.
func (i *SparseChunkInstallation) NewRanges() map[SparseChunkObjectID]memmap.FileRange {
	result := make(map[SparseChunkObjectID]memmap.FileRange, len(i.newRanges))
	for object, fr := range i.newRanges {
		result[object] = fr
	}
	return result
}

// TakePageLoads hands the caller the owner references the installation holds
// on the loaders of its reused units' pages; see SparseChunkInstallation.loads.
func (i *SparseChunkInstallation) TakePageLoads() []*AsyncPagesFileLoad {
	loads := i.loads
	i.loads = nil
	return loads
}

// releasePageLoads releases the loaders the installation still owns.
func (i *SparseChunkInstallation) releasePageLoads() {
	for _, load := range i.loads {
		_ = load.DecOwner()
	}
	i.loads = nil
}

// Commit retains newly created residency units in the family cache.
func (i *SparseChunkInstallation) Commit() error {
	if i == nil || i.done {
		return fmt.Errorf("sparse chunk installation is unavailable")
	}
	i.done = true
	i.finish()
	i.releasePageLoads()
	return nil
}

// finish lets ReclaimUnusedSparseChunks consider the installation's units.
func (i *SparseChunkInstallation) finish() {
	i.mf.mu.Lock()
	defer i.mf.mu.Unlock()
	for _, object := range i.objects {
		if i.mf.sparseInstalling[object] <= 1 {
			delete(i.mf.sparseInstalling, object)
		} else {
			i.mf.sparseInstalling[object]--
		}
	}
}

// Abort releases all references acquired by this installation.
func (i *SparseChunkInstallation) Abort() error {
	if i == nil || i.done {
		return nil
	}
	i.done = true
	i.finish()
	i.releasePageLoads()
	for object, fr := range i.newRanges {
		i.mf.mu.Lock()
		i.mf.deleteSparseChunkLocked(object)
		i.mf.mu.Unlock()
		i.mf.DecRef(fr)
	}
	return nil
}

// SparseRelocation maps serialized page offsets to family-MemoryFile offsets.
type SparseRelocation struct {
	// resources map each serialized MemoryFile's relocated ranges to the
	// offsets they relocate to.
	resources map[checkpoint.ResourceID]*sparseRelocationSet
}

// NewSparseRelocation creates an empty relocation table.
func NewSparseRelocation() *SparseRelocation {
	return &SparseRelocation{resources: make(map[checkpoint.ResourceID]*sparseRelocationSet)}
}

// Add records one page relocation. Repeated identical mappings are accepted.
func (r *SparseRelocation) Add(resourceID checkpoint.ResourceID, oldOffset, newOffset uint64) error {
	return r.AddRange(resourceID, memmap.FileRange{Start: oldOffset, End: oldOffset + hostarch.PageSize}, newOffset)
}

// AddRange records that old's pages relocate to consecutive pages from
// newStart. Pages it already records must relocate as they do here.
func (r *SparseRelocation) AddRange(resourceID checkpoint.ResourceID, old memmap.FileRange, newStart uint64) error {
	if r == nil || !old.WellFormed() || old.Length() == 0 || !hostarch.IsPageAligned(old.Start) || !hostarch.IsPageAligned(old.End) ||
		!hostarch.IsPageAligned(newStart) || newStart > math.MaxUint64-old.Length() {
		return fmt.Errorf("invalid sparse page relocation %s:%v -> %#x", resourceID, old, newStart)
	}
	set := r.resources[resourceID]
	if set == nil {
		set = &sparseRelocationSet{}
		r.resources[resourceID] = set
	}
	cursor := old.Start
	seg, gap := set.Find(cursor)
	for cursor < old.End {
		want := newStart + (cursor - old.Start)
		if !seg.Ok() {
			// Insert merges the new segment only with neighbors that
			// relocate consistently with it: the pages they add need no
			// check.
			seg = set.Insert(gap, memmap.FileRange{Start: cursor, End: min(gap.End(), old.End)}, want)
		} else if previous := seg.Value() + (cursor - seg.Start()); previous != want {
			return fmt.Errorf("conflicting sparse page relocation %s:%#x -> %#x and %#x", resourceID, cursor, previous, want)
		}
		cursor = seg.End()
		seg, gap = seg.NextNonEmpty()
	}
	return nil
}

// Page returns the relocated page offset.
func (r *SparseRelocation) Page(resourceID checkpoint.ResourceID, oldOffset uint64) (uint64, bool) {
	if r == nil || r.resources[resourceID] == nil {
		return 0, false
	}
	seg := r.resources[resourceID].FindSegment(oldOffset)
	if !seg.Ok() {
		return 0, false
	}
	return seg.Value() + (oldOffset - seg.Start()), true
}

// Range relocates a contiguous FileRange. It fails closed if any page is
// absent or if the destination pages are not contiguous; callers with
// splittable range sets should use Runs instead.
func (r *SparseRelocation) Range(resourceID checkpoint.ResourceID, old memmap.FileRange) (memmap.FileRange, error) {
	runs, err := r.Runs(resourceID, old)
	if err != nil {
		return memmap.FileRange{}, err
	}
	if len(runs) != 1 {
		return memmap.FileRange{}, fmt.Errorf("sparse range %s:%v relocates to %d discontiguous runs", resourceID, old, len(runs))
	}
	return runs[0], nil
}

// Runs relocates old and splits whenever destination offsets are not adjacent.
func (r *SparseRelocation) Runs(resourceID checkpoint.ResourceID, old memmap.FileRange) ([]memmap.FileRange, error) {
	if !old.WellFormed() || old.Length() == 0 || !hostarch.IsPageAligned(old.Start) || !hostarch.IsPageAligned(old.End) {
		return nil, fmt.Errorf("invalid sparse source range %s:%v", resourceID, old)
	}
	var set *sparseRelocationSet
	if r != nil {
		set = r.resources[resourceID]
	}
	if set == nil {
		return nil, fmt.Errorf("sparse relocation is missing %s:%#x", resourceID, old.Start)
	}
	var runs []memmap.FileRange
	cursor := old.Start
	for seg := set.FindSegment(cursor); cursor < old.End; seg = seg.NextSegment() {
		if !seg.Ok() || seg.Start() > cursor {
			return nil, fmt.Errorf("sparse relocation is missing %s:%#x", resourceID, cursor)
		}
		end := min(seg.End(), old.End)
		target := seg.Value() + (cursor - seg.Start())
		if len(runs) == 0 || runs[len(runs)-1].End != target {
			runs = append(runs, memmap.FileRange{Start: target, End: target + (end - cursor)})
		} else {
			runs[len(runs)-1].End += end - cursor
		}
		cursor = end
	}
	return runs, nil
}

type sparseRelocationSetFunctions struct{}

func (sparseRelocationSetFunctions) MinKey() uint64 {
	return 0
}

func (sparseRelocationSetFunctions) MaxKey() uint64 {
	return math.MaxUint64
}

func (sparseRelocationSetFunctions) ClearValue(*uint64) {
}

func (sparseRelocationSetFunctions) Merge(r1 memmap.FileRange, target1 uint64, _ memmap.FileRange, target2 uint64) (uint64, bool) {
	return target1, target1+r1.Length() == target2
}

func (sparseRelocationSetFunctions) Split(r memmap.FileRange, target uint64, split uint64) (uint64, uint64) {
	return target, target + (split - r.Start)
}

// InstallSparseChunks installs or reuses immutable resident chunks. It adds a
// cache reference for each newly installed chunk; restored branch owners stage
// their own references separately through SparseReferenceTransaction. Newly
// installed chunks are zero-filled until their transactional loader supplies
// bytes. The returned ranges identify only new chunks and should be registered
// with a loader before restored state can access them.
func (f *MemoryFile) InstallSparseChunks(installs []SparseChunkInstall) (*SparseChunkInstallation, error) {
	newRanges := make(map[SparseChunkObjectID]memmap.FileRange)
	installation := &SparseChunkInstallation{mf: f, newRanges: newRanges}
	rollback := func() {
		_ = installation.Abort()
	}
	seen := make(map[SparseChunkObjectID]struct{}, len(installs))
	for _, install := range installs {
		if install.Object == (SparseChunkObjectID{}) {
			rollback()
			return nil, fmt.Errorf("invalid sparse chunk object")
		}
		if _, ok := seen[install.Object]; ok {
			rollback()
			return nil, fmt.Errorf("sparse chunk object %x is repeated", install.Object)
		}
		seen[install.Object] = struct{}{}
		runs := install.Runs
		if install.Huge {
			if !validHugeSparseChunkRuns(runs) {
				rollback()
				return nil, fmt.Errorf("invalid 2 MiB sparse page %+v", runs)
			}
		} else {
			for _, run := range install.Runs {
				if !run.valid() {
					rollback()
					return nil, fmt.Errorf("invalid sparse chunk run %+v", run)
				}
			}
			var err error
			runs, err = normalizeSparseChunkRuns(append([]SparseChunkRun(nil), install.Runs...))
			if err != nil {
				rollback()
				return nil, fmt.Errorf("sparse chunk object %x: %w", install.Object, err)
			}
			if len(runs) == 0 {
				rollback()
				return nil, fmt.Errorf("sparse chunk object %x has no pages", install.Object)
			}
		}
		installation.loads = append(installation.loads, f.adoptResidentSparseChunk(install.Object)...)

		f.mu.Lock()
		resident, exists := f.sparseChunks[install.Object]
		if exists && install.Huge {
			// A 2 MiB page is resident whole.
			f.markSparseInstallingLocked(installation, install.Object)
			f.mu.Unlock()
		} else if exists {
			f.markSparseInstallingLocked(installation, install.Object)
			// One immutable chunk can back branches that each use a different
			// subset of its pages. Its whole 1 MiB is read into the resident
			// range whichever branch installed it first, so a page only a
			// later branch uses needs just its identity recorded. Pages both
			// use must name the same page version.
			merged, err := normalizeSparseChunkRuns(append(append([]SparseChunkRun(nil), resident.Runs...), runs...))
			if err != nil {
				f.mu.Unlock()
				rollback()
				return nil, fmt.Errorf("resident sparse chunk %x: %w", install.Object, err)
			}
			var held [logicalPagesPerChunk / 64]uint64
			for _, run := range resident.Runs {
				for position := uint16(run.Position); position < uint16(run.Position)+run.Pages; position++ {
					held[position/64] |= 1 << (position % 64)
				}
			}
			for _, run := range runs {
				end := uint16(run.Position) + run.Pages
				for position := uint16(run.Position); position < end; {
					if held[position/64]&(1<<(position%64)) != 0 {
						position++
						continue
					}
					next := position + 1
					for next < end && held[next/64]&(1<<(next%64)) == 0 {
						next++
					}
					f.setSparseIdentitiesLocked(resident.Range.Start, run, position, next)
					position = next
				}
			}
			resident.Runs = merged
			f.sparseChunks[install.Object] = resident
			f.mu.Unlock()
		} else {
			f.mu.Unlock()
			length, opts := uint64(SparseChunkBytes), AllocOpts{Kind: install.Kind, MemCgID: install.MemCgID}
			if install.Huge {
				length, opts.Huge, opts.LogicalHugePages = hostarch.HugePageSize, true, true
			}
			// What an address space restores is in address order, as it was
			// when it was saved, where the unit's pages are at their slots.
			fr, err := f.allocateUnit(runs, length, opts)
			if err != nil {
				rollback()
				return nil, fmt.Errorf("allocating sparse resident chunk: %w", err)
			}
			resident = sparseResidentChunk{Range: fr, Runs: runs, Parts: slices.Clone(install.Parts)}
			f.mu.Lock()
			if f.sparseChunks == nil {
				f.sparseChunks = make(map[SparseChunkObjectID]sparseResidentChunk)
			}
			if _, raced := f.sparseChunks[install.Object]; raced {
				f.mu.Unlock()
				f.DecRef(fr)
				rollback()
				return nil, fmt.Errorf("concurrent sparse chunk installation is unsupported")
			}
			if install.Huge && hostarch.IsHugePageAligned(fr.Start) {
				// Its identities are a 2 MiB page's, as its allocation's were.
				f.setLogicalPagesLocked(fr, logicalPageInfo{
					Allocator: runs[0].Page.Allocator, FirstSequence: runs[0].Page.Sequence,
					VersionAllocator: runs[0].Version.Allocator, FirstVersionSequence: runs[0].Version.Sequence,
					Huge: true,
				})
			} else {
				for _, run := range runs {
					f.setSparseIdentitiesLocked(fr.Start, run, uint16(run.Position), uint16(run.Position)+run.Pages)
				}
			}
			f.sparseChunks[install.Object] = resident
			delete(f.reclaimedSparseChunks, install.Object)
			for _, part := range resident.Parts {
				if f.sparseParts == nil {
					f.sparseParts = make(map[SparseChunkObjectID]int)
				}
				f.sparseParts[part]++
				delete(f.reclaimedSparseChunks, part)
			}
			f.markSparseInstallingLocked(installation, install.Object)
			f.mu.Unlock()
			newRanges[install.Object] = fr
		}
	}
	return installation, nil
}

// setSparseIdentitiesLocked gives the pages of the unit at base from position
// start to end the identities run, which holds them, gives them.
//
// Preconditions: f.mu is locked.
func (f *MemoryFile) setSparseIdentitiesLocked(base uint64, run SparseChunkRun, start, end uint16) {
	delta := uint64(start - uint16(run.Position))
	fr := memmap.FileRange{Start: base + uint64(start)*hostarch.PageSize, End: base + uint64(end)*hostarch.PageSize}
	f.setLogicalPagesLocked(fr, logicalPageInfo{
		Allocator: run.Page.Allocator, FirstSequence: run.Page.Sequence + delta,
		VersionAllocator: run.Version.Allocator, FirstVersionSequence: run.Version.Sequence + delta,
	})
}

// adoptResidentSparseChunk returns, each with an owner reference of the
// caller's, the loaders of the pages of object's resident unit that are still
// to be loaded. A unit that a loader which has stopped left unloaded can never
// be read: it is forgotten instead, to be installed afresh, and its range
// keeps its cache reference while the file lives, as the stopped loader may
// still hold some of its pages.
func (f *MemoryFile) adoptResidentSparseChunk(object SparseChunkObjectID) []*AsyncPagesFileLoad {
	f.mu.Lock()
	resident, ok := f.sparseChunks[object]
	f.mu.Unlock()
	if !ok {
		return nil
	}
	var loads []*AsyncPagesFileLoad
	stranded := false
	for _, load := range f.pendingPageLoads(resident.Range) {
		if load.err() != nil || !load.tryIncOwner() {
			stranded = true
			break
		}
		loads = append(loads, load)
	}
	if !stranded {
		return loads
	}
	for _, load := range loads {
		_ = load.DecOwner()
	}
	f.mu.Lock()
	if current, ok := f.sparseChunks[object]; ok && current.Range == resident.Range {
		f.deleteSparseChunkLocked(object)
		log.Warningf("Resident sparse chunk %x waits on a stopped page loader; installing it again", object)
	}
	f.mu.Unlock()
	return nil
}

// SparseChunkRange returns the resident range for object.
func (f *MemoryFile) SparseChunkRange(object SparseChunkObjectID) (memmap.FileRange, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	chunk, ok := f.sparseChunks[object]
	return chunk.Range, ok
}

// ReclaimedSparseChunks returns the sparse chunks ReclaimUnusedSparseChunks
// has released since they were last installed, in no order: every page of
// them was since freed or rewritten by all of the file's users.
func (f *MemoryFile) ReclaimedSparseChunks() []SparseChunkObjectID {
	f.mu.Lock()
	defer f.mu.Unlock()
	objects := make([]SparseChunkObjectID, 0, len(f.reclaimedSparseChunks))
	for object := range f.reclaimedSparseChunks {
		objects = append(objects, object)
	}
	return objects
}

// markSparseInstallingLocked records that installation installs or reuses
// object.
//
// Preconditions: f.mu is locked.
func (f *MemoryFile) markSparseInstallingLocked(installation *SparseChunkInstallation, object SparseChunkObjectID) {
	if f.sparseInstalling == nil {
		f.sparseInstalling = make(map[SparseChunkObjectID]int)
	}
	f.sparseInstalling[object]++
	installation.objects = append(installation.objects, object)
}

// sparseChunkUsedLocked reports whether a resident unit is object, or is made
// of it.
//
// Preconditions: f.mu is locked.
func (f *MemoryFile) sparseChunkUsedLocked(object SparseChunkObjectID) bool {
	_, ok := f.sparseChunks[object]
	return ok || f.sparseParts[object] != 0
}

// deleteSparseChunkLocked forgets object's resident unit, if any.
//
// Preconditions: f.mu is locked.
func (f *MemoryFile) deleteSparseChunkLocked(object SparseChunkObjectID) {
	for _, part := range f.sparseChunks[object].Parts {
		if f.sparseParts[part] <= 1 {
			delete(f.sparseParts, part)
		} else {
			f.sparseParts[part]--
		}
	}
	delete(f.sparseChunks, object)
}

// ReclaimUnusedSparseChunks releases residency units whose cache reference is
// their only remaining reference. It returns the number of units released.
// A unit an installation still commits stays, as the owners it restores are
// yet to take their references. A loader may hold a released unit's pages as
// not yet loaded, the loader of a rehydration that loads only what is
// touched: no user will await them, and the loader forgets them, so that it
// can finish.
func (f *MemoryFile) ReclaimUnusedSparseChunks() int {
	var release []memmap.FileRange
	f.mu.Lock()
	for object, resident := range f.sparseChunks {
		if f.sparseInstalling[object] != 0 {
			continue
		}
		// A page being loaded or awaited has a reference of its loader or
		// its waiter besides the cache's.
		if f.hasUniqueRefLocked(resident.Range) {
			f.deleteSparseChunkLocked(object)
			if f.reclaimedSparseChunks == nil {
				f.reclaimedSparseChunks = make(map[SparseChunkObjectID]struct{})
			}
			// What another unit is made of is in use still.
			if !f.sparseChunkUsedLocked(object) {
				f.reclaimedSparseChunks[object] = struct{}{}
			}
			for _, part := range resident.Parts {
				if !f.sparseChunkUsedLocked(part) {
					f.reclaimedSparseChunks[part] = struct{}{}
				}
			}
			release = append(release, resident.Range)
		}
	}
	f.mu.Unlock()
	if len(release) == 0 {
		return 0
	}
	// Before the pages are released, and may be reused.
	if loads := f.sparseAsyncLoads.Load(); loads != nil {
		for _, amfl := range *loads {
			amfl.cancelUnusedLoads(release)
		}
	}
	for _, fr := range release {
		f.DecRef(fr)
	}
	return len(release)
}
