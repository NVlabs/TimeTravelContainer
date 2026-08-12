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

package ttcchunk

import (
	"bytes"
	"fmt"
	"sort"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// SourcePage pairs a branch binding with its immutable fork-time bytes.
type SourcePage struct {
	Binding pgalloc.CapturedPageBinding
	Zero    bool
	Data    []byte
}

// Object is one encoded immutable chunk ready for publication.
type Object struct {
	ID      ID
	Logical pgalloc.LogicalChunkID
	Data    []byte
}

// StoredBinding extends a branch binding with the immutable object containing
// its page version.
type StoredBinding struct {
	Binding pgalloc.CapturedPageBinding
	Object  ID
}

type assemblyPage struct {
	sourceIndex int
	page        Page
}

type assemblyGroup struct {
	logical pgalloc.LogicalChunkID
	pages   []assemblyPage
	used    [32]byte
}

func compareAllocator(a, b pgalloc.LogicalAllocatorID) int { return bytes.Compare(a[:], b[:]) }

// Assemble deterministically packs source pages into immutable chunk objects.
// Different versions of the same logical slot are placed in different objects.
// This is necessary when aliases in one branch have diverged through CoW.
func Assemble(source []SourcePage) ([]Object, []StoredBinding, error) {
	if len(source) == 0 {
		return nil, nil, nil
	}
	ordered := append([]SourcePage(nil), source...)
	sort.Slice(ordered, func(i, j int) bool {
		left, right := &ordered[i].Binding, &ordered[j].Binding
		if cmp := compareAllocator(left.Chunk.Allocator, right.Chunk.Allocator); cmp != 0 {
			return cmp < 0
		}
		if left.Chunk.Sequence != right.Chunk.Sequence {
			return left.Chunk.Sequence < right.Chunk.Sequence
		}
		if left.Slot != right.Slot {
			return left.Slot < right.Slot
		}
		if cmp := compareAllocator(left.Version.Allocator, right.Version.Allocator); cmp != 0 {
			return cmp < 0
		}
		if left.Version.Sequence != right.Version.Sequence {
			return left.Version.Sequence < right.Version.Sequence
		}
		return left.OldOffset < right.OldOffset
	})
	groupsByLogical := make(map[pgalloc.LogicalChunkID][]*assemblyGroup)
	var groupOrder []*assemblyGroup
	seenVersions := make(map[struct {
		page    pgalloc.LogicalPageID
		version pgalloc.PageVersionID
	}]struct{}, len(ordered))
	for sourceIndex := range ordered {
		sourcePage := &ordered[sourceIndex]
		binding := &sourcePage.Binding
		wantChunk, wantSlot := binding.Page.Chunk()
		if binding.Chunk != wantChunk || binding.Slot != wantSlot {
			return nil, nil, fmt.Errorf("captured page %+v has inconsistent chunk coordinate (%+v,%d)", binding.Page, binding.Chunk, binding.Slot)
		}
		if binding.References == 0 {
			return nil, nil, fmt.Errorf("captured page %+v has no references", binding.Page)
		}
		versionKey := struct {
			page    pgalloc.LogicalPageID
			version pgalloc.PageVersionID
		}{binding.Page, binding.Version}
		if _, ok := seenVersions[versionKey]; ok {
			return nil, nil, fmt.Errorf("captured page %+v repeats version %+v", binding.Page, binding.Version)
		}
		seenVersions[versionKey] = struct{}{}
		if sourcePage.Zero {
			if len(sourcePage.Data) != 0 {
				return nil, nil, fmt.Errorf("captured zero page %+v has payload", binding.Page)
			}
		} else if len(sourcePage.Data) != hostarch.PageSize {
			return nil, nil, fmt.Errorf("captured page %+v has %d bytes, want %d", binding.Page, len(sourcePage.Data), hostarch.PageSize)
		}
		var target *assemblyGroup
		for _, candidate := range groupsByLogical[binding.Chunk] {
			if !hasBit(&candidate.used, binding.Slot) {
				target = candidate
				break
			}
		}
		if target == nil {
			target = &assemblyGroup{logical: binding.Chunk}
			groupsByLogical[binding.Chunk] = append(groupsByLogical[binding.Chunk], target)
			groupOrder = append(groupOrder, target)
		}
		setBit(&target.used, binding.Slot)
		target.pages = append(target.pages, assemblyPage{
			sourceIndex: sourceIndex,
			page:        Page{Slot: binding.Slot, Version: binding.Version, Zero: sourcePage.Zero, Data: sourcePage.Data},
		})
	}

	objects := make([]Object, 0, len(groupOrder))
	stored := make([]StoredBinding, len(ordered))
	for _, group := range groupOrder {
		pages := make([]Page, len(group.pages))
		for i := range group.pages {
			pages[i] = group.pages[i].page
		}
		data, id, err := Encode(Chunk{Logical: group.logical, Pages: pages})
		if err != nil {
			return nil, nil, err
		}
		objects = append(objects, Object{ID: id, Logical: group.logical, Data: data})
		for _, page := range group.pages {
			stored[page.sourceIndex] = StoredBinding{Binding: ordered[page.sourceIndex].Binding, Object: id}
		}
	}
	return objects, stored, nil
}
