// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"bytes"
	"cmp"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
)

// CapturedMemoryRange identifies one branch-reachable MemoryFile range.
type CapturedMemoryRange struct {
	MemoryFile *MemoryFile
	Range      memmap.FileRange
}

// CapturedPageBinding binds one offset in serialized Kernel state to a stable
// logical page and immutable content version. OldOffset is an offset in the
// source MemoryFile; restore may relocate it in the destination MemoryFile.
type CapturedPageBinding struct {
	ResourceID checkpoint.ResourceID
	OldOffset  uint64
	// StreamOffset is the byte offset of this page in SnapshotLayout's packed
	// stream. It is transport-local and is not part of logical identity.
	StreamOffset uint64
	References   uint64
	Page         LogicalPageID
	Version      PageVersionID
	Chunk        LogicalChunkID
	Slot         uint8
	// Huge is true if the binding binds a 2 MiB page whole (see
	// logicalPageInfo.Huge): OldOffset, Page, Version, Chunk and Slot are
	// those of its first base page, which the others follow, and the packed
	// stream holds its 2 MiB from StreamOffset.
	Huge bool
}

// ValidHugePageBinding reports whether b binds a 2 MiB page as one can be:
// at an aligned offset, with identities aligned as a 2 MiB page's are (see
// logicalPageInfo.Huge).
func ValidHugePageBinding(b *CapturedPageBinding) bool {
	return b.Huge && hostarch.IsHugePageAligned(b.OldOffset) && hostarch.IsPageAligned(b.StreamOffset) &&
		b.Page.Sequence%pagesPerHugePage() == 0 && b.Version.Sequence%pagesPerHugePage() == 0 &&
		b.Page.Sequence <= math.MaxUint64-pagesPerHugePage() && b.Version.Sequence <= math.MaxUint64-pagesPerHugePage()
}

// Length returns the number of bytes that b binds.
func (b *CapturedPageBinding) Length() uint64 {
	if b.Huge {
		return hostarch.HugePageSize
	}
	return hostarch.PageSize
}

// BasePageBindings returns bindings with the binding of each 2 MiB page
// replaced by those of its base pages, for a consumer that takes base pages
// only. It returns bindings itself if they bind no 2 MiB page.
func BasePageBindings(bindings []CapturedPageBinding) []CapturedPageBinding {
	huge := 0
	for i := range bindings {
		if bindings[i].Huge {
			huge++
		}
	}
	if huge == 0 {
		return bindings
	}
	result := make([]CapturedPageBinding, 0, len(bindings)+huge*int(pagesPerHugePage()-1))
	for _, binding := range bindings {
		if !binding.Huge {
			result = append(result, binding)
			continue
		}
		binding.Huge = false
		for k := uint64(0); k < pagesPerHugePage(); k++ {
			page := binding
			page.OldOffset += k * hostarch.PageSize
			page.StreamOffset += k * hostarch.PageSize
			page.Page.Sequence += k
			page.Version.Sequence += k
			page.Chunk, page.Slot = page.Page.Chunk()
			result = append(result, page)
		}
	}
	return result
}

// CapturedMemoryLayout is the exact branch-reachable byte layout pinned for a
// remote fork or dehydration. Regions contain each physical source page once;
// Bindings retain the number of serialized owners of that page.
type CapturedMemoryLayout struct {
	Regions  []RemoteSnapshotRegion
	Bindings []CapturedPageBinding
}

// ChunkOrdered returns an equivalent layout whose packed stream keeps every
// residency unit contiguous: each 2 MiB page, and the base pages of each
// logical 1 MiB chunk that InstallRemoteBindings installs together. Physical
// MemoryFile offsets are unrelated to logical page allocation order after
// CoW, so the ordinary address-ordered layout cannot provide this property.
// Live remote readers require it to load and publish a unit atomically, and
// its regions name their units, so that a snapshot of them pushes whole units
// (see RemoteSnapshot.AtomicPushRanges).
func (l CapturedMemoryLayout) ChunkOrdered() (CapturedMemoryLayout, error) {
	// Sort indexes, and move each 100-byte binding once.
	order := make([]int32, len(l.Bindings))
	for i := range order {
		order[i] = int32(i)
	}
	slices.SortFunc(order, func(a, b int32) int {
		left, right := &l.Bindings[a], &l.Bindings[b]
		// 2 MiB pages come first, so that they leave the base pages of
		// each logical chunk together.
		if left.Huge != right.Huge {
			if left.Huge {
				return -1
			}
			return 1
		}
		if c := bytes.Compare(left.Chunk.Allocator[:], right.Chunk.Allocator[:]); c != 0 {
			return c
		}
		if c := cmp.Compare(left.Chunk.Sequence, right.Chunk.Sequence); c != 0 {
			return c
		}
		if c := cmp.Compare(left.Slot, right.Slot); c != 0 {
			return c
		}
		if c := bytes.Compare(left.Version.Allocator[:], right.Version.Allocator[:]); c != 0 {
			return c
		}
		if c := cmp.Compare(left.Version.Sequence, right.Version.Sequence); c != 0 {
			return c
		}
		if left.ResourceID != right.ResourceID {
			return cmp.Compare(left.ResourceID.String(), right.ResourceID.String())
		}
		return cmp.Compare(left.OldOffset, right.OldOffset)
	})
	bindings := make([]CapturedPageBinding, len(order))
	for i, index := range order {
		bindings[i] = l.Bindings[index]
	}
	files := make(map[checkpoint.ResourceID]*MemoryFile)
	for _, region := range l.Regions {
		resource := region.File.ResourceID()
		if previous := files[resource]; previous != nil && previous != region.File {
			return CapturedMemoryLayout{}, fmt.Errorf("captured resource %s refers to multiple MemoryFiles", resource)
		}
		files[resource] = region.File
	}
	ordered := CapturedMemoryLayout{Bindings: bindings}
	var previousPage LogicalPageID
	var previousVersion PageVersionID
	var streamOffset uint64
	// The residency units, as InstallRemoteBindings forms them: a 2 MiB
	// page alone; base pages by logical chunk, in a further instance of the
	// chunk from a page whose slot the current one holds.
	type unit struct {
		chunk    LogicalChunkID
		instance uint32
	}
	instances := make(map[LogicalChunkID]uint32)
	slots := make(map[unit][logicalPagesPerChunk / 64]uint64)
	hugeInstance := uint32(1 << 31)
	for i := range ordered.Bindings {
		binding := &ordered.Bindings[i]
		file := files[binding.ResourceID]
		if file == nil {
			return CapturedMemoryLayout{}, fmt.Errorf("captured binding %s:%#x has no MemoryFile", binding.ResourceID, binding.OldOffset)
		}
		// A branch may legitimately contain multiple content versions of one
		// logical page. For example, two MAP_PRIVATE aliases initially share a
		// page and then diverge through CoW. Chunk assembly deliberately places
		// those versions in separate immutable objects. Only an identical
		// page-version pair is ambiguous here: it would cause the same immutable
		// version to have two physical stream sources.
		if i != 0 && binding.Page == previousPage && binding.Version == previousVersion {
			return CapturedMemoryLayout{}, fmt.Errorf("captured logical page %+v repeats version %+v", binding.Page, binding.Version)
		}
		previousPage = binding.Page
		previousVersion = binding.Version
		key := unit{chunk: binding.Chunk}
		if binding.Huge {
			key.instance = hugeInstance
			hugeInstance++
		} else {
			key.instance = instances[binding.Chunk]
			if used := slots[key]; used[binding.Slot/64]&(uint64(1)<<(binding.Slot%64)) != 0 {
				key.instance++
				instances[binding.Chunk] = key.instance
			}
			used := slots[key]
			used[binding.Slot/64] |= uint64(1) << (binding.Slot % 64)
			slots[key] = used
		}
		binding.StreamOffset = streamOffset
		streamOffset += binding.Length()
		page := memmap.FileRange{Start: binding.OldOffset, End: binding.OldOffset + binding.Length()}
		if len(ordered.Regions) != 0 {
			last := &ordered.Regions[len(ordered.Regions)-1]
			if last.File == file && last.Range.End == page.Start && last.LogicalChunk == key.chunk && last.LogicalChunkInstance == key.instance {
				last.Range.End = page.End
				continue
			}
		}
		ordered.Regions = append(ordered.Regions, RemoteSnapshotRegion{
			ID: RemoteRegionID(len(ordered.Regions) + 1), File: file, Range: page,
			LogicalChunked: true, LogicalChunk: key.chunk, LogicalChunkInstance: key.instance,
		})
	}
	return ordered, nil
}

type capturedReferencedRange struct {
	range_     memmap.FileRange
	references uint64
}

// MemoryRangeCapture accumulates the exact MemoryFile ranges reachable from a
// single Kernel state graph. It is intentionally independent of MemoryFile's
// allocator-wide reference counts, which may include sibling branches.
//
// A capture is built while its Kernel is paused and is not goroutine-safe.
type MemoryRangeCapture struct {
	files  []*MemoryFile
	ranges map[*MemoryFile][]memmap.FileRange
	owners map[any]map[capturedOwnerRange]struct{}
}

type capturedOwnerRange struct {
	file       *MemoryFile
	start, end uint64
}

// NewMemoryRangeCapture creates an empty branch-scoped memory capture.
func NewMemoryRangeCapture() *MemoryRangeCapture {
	return &MemoryRangeCapture{
		ranges: make(map[*MemoryFile][]memmap.FileRange),
		owners: make(map[any]map[capturedOwnerRange]struct{}),
	}
}

// Record includes fr in the state graph's references to mf. Overlapping and
// adjacent ranges are coalesced when Ranges or Bindings is called.
func (c *MemoryRangeCapture) Record(mf *MemoryFile, fr memmap.FileRange) error {
	if c == nil || mf == nil {
		return fmt.Errorf("memory range capture and MemoryFile must be non-nil")
	}
	if !fr.WellFormed() || fr.Length() == 0 || !hostarch.IsPageAligned(fr.Start) || !hostarch.IsPageAligned(fr.End) {
		return fmt.Errorf("invalid captured MemoryFile range %v", fr)
	}
	if _, ok := c.ranges[mf]; !ok {
		c.files = append(c.files, mf)
	}
	c.ranges[mf] = append(c.ranges[mf], fr)
	return nil
}

// RecordOwned records a range directly owned by one serialized object. The
// same object may be reached through several graph paths (for example a socket
// FD and its mmap VMA); duplicate owner/range pairs count only once. Distinct
// owners of the same pages continue to contribute distinct references.
func (c *MemoryRangeCapture) RecordOwned(owner any, mf *MemoryFile, fr memmap.FileRange) error {
	if owner == nil || !reflect.TypeOf(owner).Comparable() {
		return fmt.Errorf("memory range owner %T is nil or not comparable", owner)
	}
	key := capturedOwnerRange{file: mf, start: fr.Start, end: fr.End}
	ranges := c.owners[owner]
	if ranges == nil {
		ranges = make(map[capturedOwnerRange]struct{})
		c.owners[owner] = ranges
	}
	if _, ok := ranges[key]; ok {
		return nil
	}
	if err := c.Record(mf, fr); err != nil {
		return err
	}
	ranges[key] = struct{}{}
	return nil
}

func mergeCapturedRanges(ranges []memmap.FileRange) []memmap.FileRange {
	sorted := append([]memmap.FileRange(nil), ranges...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start < sorted[j].Start
		}
		return sorted[i].End < sorted[j].End
	})
	merged := make([]memmap.FileRange, 0, len(sorted))
	for _, fr := range sorted {
		if len(merged) == 0 || merged[len(merged)-1].End < fr.Start {
			merged = append(merged, fr)
			continue
		}
		if fr.End > merged[len(merged)-1].End {
			merged[len(merged)-1].End = fr.End
		}
	}
	return merged
}

func referencedCapturedRanges(ranges []memmap.FileRange) []capturedReferencedRange {
	type event struct {
		offset uint64
		delta  int64
	}
	events := make([]event, 0, 2*len(ranges))
	for _, fr := range ranges {
		events = append(events, event{offset: fr.Start, delta: 1}, event{offset: fr.End, delta: -1})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].offset < events[j].offset })
	var result []capturedReferencedRange
	var references int64
	var previous uint64
	for i := 0; i < len(events); {
		offset := events[i].offset
		if references > 0 && previous < offset {
			result = append(result, capturedReferencedRange{
				range_:     memmap.FileRange{Start: previous, End: offset},
				references: uint64(references),
			})
		}
		for i < len(events) && events[i].offset == offset {
			references += events[i].delta
			i++
		}
		if references < 0 {
			panic("negative captured MemoryFile reference count")
		}
		previous = offset
	}
	if references != 0 {
		panic("unbalanced captured MemoryFile reference count")
	}
	return result
}

// Ranges returns a detached, coalesced snapshot of all recorded ranges.
func (c *MemoryRangeCapture) Ranges() []CapturedMemoryRange {
	if c == nil {
		return nil
	}
	var result []CapturedMemoryRange
	for _, mf := range c.files {
		for _, fr := range mergeCapturedRanges(c.ranges[mf]) {
			result = append(result, CapturedMemoryRange{MemoryFile: mf, Range: fr})
		}
	}
	return result
}

// Bindings resolves all recorded ranges to page identities, a binding for each
// base page. It fails if any byte in a recorded range is no longer backed by
// an allocated logical page; callers must keep the source Kernel paused until
// a snapshot pins the ranges.
func (c *MemoryRangeCapture) Bindings() ([]CapturedPageBinding, error) {
	return c.bindings(false)
}

// bindings is Bindings, but with one binding for each 2 MiB page that the
// recorded ranges hold whole, if huge, whose References is the most that any
// of its base pages has.
func (c *MemoryRangeCapture) bindings(huge bool) ([]CapturedPageBinding, error) {
	var result []CapturedPageBinding
	streamRanges := make(map[*MemoryFile][]struct {
		range_ memmap.FileRange
		start  uint64
	})
	var nextStreamOffset uint64
	for _, captured := range c.Ranges() {
		streamRanges[captured.MemoryFile] = append(streamRanges[captured.MemoryFile], struct {
			range_ memmap.FileRange
			start  uint64
		}{captured.Range, nextStreamOffset})
		nextStreamOffset += captured.Range.Length()
	}
	pages := uint64(0)
	for _, mf := range c.files {
		for _, referenced := range referencedCapturedRanges(c.ranges[mf]) {
			pages += referenced.range_.Length() / hostarch.PageSize
		}
	}
	result = make([]CapturedPageBinding, 0, pages)
	for _, mf := range c.files {
		// The stream ranges of mf are sorted and disjoint, and its referenced
		// ranges are visited in ascending order, so the stream range of each
		// page is at or after the previous page's.
		ranges := streamRanges[mf]
		index := 0
		// The 2 MiB pages that the recorded ranges hold whole, in order, and
		// the index in result of the binding of each that has one yet.
		var hugePages []memmap.FileRange
		if huge {
			for _, captured := range ranges {
				for _, run := range mf.LogicalPageRanges(captured.range_) {
					if !run.Huge {
						continue
					}
					for start := hostarch.MustHugePageRoundUp(run.Range.Start); start < hostarch.HugePageRoundDown(run.Range.End); start += hostarch.HugePageSize {
						hugePages = append(hugePages, memmap.FileRange{Start: start, End: start + hostarch.HugePageSize})
					}
				}
			}
		}
		hugeBindings := make(map[uint64]int)
		hugeIndex := 0
		for _, referenced := range referencedCapturedRanges(c.ranges[mf]) {
			capturedRange := referenced.range_
			cursor := capturedRange.Start
			for _, run := range mf.LogicalPageRanges(capturedRange) {
				if run.Range.Start != cursor {
					return nil, fmt.Errorf("captured MemoryFile range %v has no logical page at offset %#x", capturedRange, cursor)
				}
				for off := run.Range.Start; off < run.Range.End; {
					for index < len(ranges) && ranges[index].range_.End <= off {
						index++
					}
					if index == len(ranges) || ranges[index].range_.Start > off {
						return nil, fmt.Errorf("captured MemoryFile offset %#x has no packed stream range", off)
					}
					for hugeIndex < len(hugePages) && hugePages[hugeIndex].End <= off {
						hugeIndex++
					}
					pageDelta := (off - run.Range.Start) / hostarch.PageSize
					pageID := LogicalPageID{Allocator: run.First.Allocator, Sequence: run.First.Sequence + pageDelta}
					versionID := PageVersionID{Allocator: run.FirstVersion.Allocator, Sequence: run.FirstVersion.Sequence + pageDelta}
					if hugeIndex < len(hugePages) && hugePages[hugeIndex].Start <= off {
						// The 2 MiB page is bound once, by its first base
						// page, however its references cover it.
						page := hugePages[hugeIndex]
						if i, ok := hugeBindings[page.Start]; ok {
							result[i].References = max(result[i].References, referenced.references)
						} else {
							first := (off - page.Start) / hostarch.PageSize
							pageID.Sequence -= first
							versionID.Sequence -= first
							chunkID, slot := pageID.Chunk()
							hugeBindings[page.Start] = len(result)
							result = append(result, CapturedPageBinding{
								ResourceID:   mf.ResourceID(),
								OldOffset:    page.Start,
								StreamOffset: ranges[index].start + page.Start - ranges[index].range_.Start,
								References:   referenced.references,
								Page:         pageID,
								Version:      versionID,
								Chunk:        chunkID,
								Slot:         slot,
								Huge:         true,
							})
						}
						off = min(page.End, run.Range.End)
						continue
					}
					streamOffset := ranges[index].start + off - ranges[index].range_.Start
					chunkID, slot := pageID.Chunk()
					result = append(result, CapturedPageBinding{
						ResourceID:   mf.ResourceID(),
						OldOffset:    off,
						StreamOffset: streamOffset,
						References:   referenced.references,
						Page:         pageID,
						Version:      versionID,
						Chunk:        chunkID,
						Slot:         slot,
					})
					off += hostarch.PageSize
				}
				cursor = run.Range.End
			}
			if cursor != capturedRange.End {
				return nil, fmt.Errorf("captured MemoryFile range %v has no logical page at offset %#x", capturedRange, cursor)
			}
		}
	}
	return result, nil
}

// SnapshotLayout returns the regions and page bindings needed to pin exactly
// this capture, a binding for each base page. It does not acquire references;
// pass Regions to NewRemoteSnapshot or NewRemoteSnapshots while the source
// remains paused.
func (c *MemoryRangeCapture) SnapshotLayout() (CapturedMemoryLayout, error) {
	return c.snapshotLayout(false)
}

// HugePageSnapshotLayout is SnapshotLayout, but with one binding for each 2
// MiB page that the capture holds whole, for a consumer that installs 2 MiB
// pages whole.
func (c *MemoryRangeCapture) HugePageSnapshotLayout() (CapturedMemoryLayout, error) {
	return c.snapshotLayout(true)
}

func (c *MemoryRangeCapture) snapshotLayout(huge bool) (CapturedMemoryLayout, error) {
	bindings, err := c.bindings(huge)
	if err != nil {
		return CapturedMemoryLayout{}, err
	}
	ranges := c.Ranges()
	regions := make([]RemoteSnapshotRegion, len(ranges))
	for i, captured := range ranges {
		regions[i] = RemoteSnapshotRegion{
			ID:    RemoteRegionID(i + 1),
			File:  captured.MemoryFile,
			Range: captured.Range,
		}
	}
	return CapturedMemoryLayout{Regions: regions, Bindings: bindings}, nil
}
