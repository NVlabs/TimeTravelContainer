// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pgalloc

import (
	"bytes"
	"cmp"
	"fmt"
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
}

// CapturedMemoryLayout is the exact branch-reachable byte layout pinned for a
// remote fork or dehydration. Regions contain each physical source page once;
// Bindings retain the number of serialized owners of that page.
type CapturedMemoryLayout struct {
	Regions  []RemoteSnapshotRegion
	Bindings []CapturedPageBinding
}

// ChunkOrdered returns an equivalent layout whose packed stream keeps every
// logical 1 MiB chunk contiguous. Physical MemoryFile offsets are unrelated to
// logical page allocation order after CoW, so the ordinary address-ordered
// layout cannot provide this property. Live remote readers require it to load
// and publish a chunk atomically.
func (l CapturedMemoryLayout) ChunkOrdered() (CapturedMemoryLayout, error) {
	// Sort indexes, and move each 100-byte binding once.
	order := make([]int32, len(l.Bindings))
	for i := range order {
		order[i] = int32(i)
	}
	slices.SortFunc(order, func(a, b int32) int {
		left, right := &l.Bindings[a], &l.Bindings[b]
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
		binding.StreamOffset = uint64(i) * hostarch.PageSize
		page := memmap.FileRange{Start: binding.OldOffset, End: binding.OldOffset + hostarch.PageSize}
		if len(ordered.Regions) != 0 {
			last := &ordered.Regions[len(ordered.Regions)-1]
			if last.File == file && last.Range.End == page.Start {
				last.Range.End = page.End
				continue
			}
		}
		ordered.Regions = append(ordered.Regions, RemoteSnapshotRegion{
			ID: RemoteRegionID(len(ordered.Regions) + 1), File: file, Range: page,
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

// Bindings resolves all recorded ranges to page identities. It fails if any
// byte in a recorded range is no longer backed by an allocated logical page;
// callers must keep the source Kernel paused until a snapshot pins the ranges.
func (c *MemoryRangeCapture) Bindings() ([]CapturedPageBinding, error) {
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
		for _, referenced := range referencedCapturedRanges(c.ranges[mf]) {
			capturedRange := referenced.range_
			cursor := capturedRange.Start
			for _, run := range mf.LogicalPageRanges(capturedRange) {
				if run.Range.Start != cursor {
					return nil, fmt.Errorf("captured MemoryFile range %v has no logical page at offset %#x", capturedRange, cursor)
				}
				for off := run.Range.Start; off < run.Range.End; off += hostarch.PageSize {
					for index < len(ranges) && ranges[index].range_.End <= off {
						index++
					}
					if index == len(ranges) || ranges[index].range_.Start > off {
						return nil, fmt.Errorf("captured MemoryFile offset %#x has no packed stream range", off)
					}
					streamOffset := ranges[index].start + off - ranges[index].range_.Start
					pageDelta := (off - run.Range.Start) / hostarch.PageSize
					pageID := LogicalPageID{Allocator: run.First.Allocator, Sequence: run.First.Sequence + pageDelta}
					versionID := PageVersionID{Allocator: run.FirstVersion.Allocator, Sequence: run.FirstVersion.Sequence + pageDelta}
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
// this capture. It does not acquire references; pass Regions to
// NewRemoteSnapshot or NewRemoteSnapshots while the source remains paused.
func (c *MemoryRangeCapture) SnapshotLayout() (CapturedMemoryLayout, error) {
	bindings, err := c.Bindings()
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
