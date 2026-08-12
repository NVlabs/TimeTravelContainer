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
	"fmt"
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
}

// NewMemoryRangeCapture creates an empty branch-scoped memory capture.
func NewMemoryRangeCapture() *MemoryRangeCapture {
	return &MemoryRangeCapture{ranges: make(map[*MemoryFile][]memmap.FileRange)}
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
	for _, mf := range c.files {
		for _, referenced := range referencedCapturedRanges(c.ranges[mf]) {
			capturedRange := referenced.range_
			cursor := capturedRange.Start
			for _, run := range mf.LogicalPageRanges(capturedRange) {
				if run.Range.Start != cursor {
					return nil, fmt.Errorf("captured MemoryFile range %v has no logical page at offset %#x", capturedRange, cursor)
				}
				for off := run.Range.Start; off < run.Range.End; off += hostarch.PageSize {
					var streamOffset uint64
					foundStreamRange := false
					for _, streamRange := range streamRanges[mf] {
						if streamRange.range_.Start <= off && off < streamRange.range_.End {
							streamOffset = streamRange.start + off - streamRange.range_.Start
							foundStreamRange = true
							break
						}
					}
					if !foundStreamRange {
						return nil, fmt.Errorf("captured MemoryFile offset %#x has no packed stream range", off)
					}
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
