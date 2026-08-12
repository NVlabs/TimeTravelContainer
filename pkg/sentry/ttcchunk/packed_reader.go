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
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

type packedPage struct {
	object ID
	slot   uint8
}

// PackedManifestReader exposes a sparse manifest using the packed stream
// offsets from a distributed snapshot. It permits an existing target
// MemoryFile to retain its original offsets while its demand loader switches
// from a live TCP source to immutable shared chunks.
type PackedManifestReader struct {
	repository *MemoryRepository
	pages      map[uint64]packedPage
	mu         sync.Mutex
	closed     bool
}

// NewPackedManifestReader verifies manifest and constructs its packed-stream
// index. It takes ownership of repository on success and failure.
func NewPackedManifestReader(repository *MemoryRepository, id ManifestID, loads []pgalloc.RemoteLoadRange) (*PackedManifestReader, error) {
	if repository == nil {
		return nil, errors.New("packed manifest reader repository is nil")
	}
	fail := func(err error) (*PackedManifestReader, error) {
		_ = repository.Close()
		return nil, err
	}
	manifest, err := repository.LoadManifest(id)
	if err != nil {
		return fail(fmt.Errorf("loading packed memory manifest: %w", err))
	}
	ordered := append([]pgalloc.RemoteLoadRange(nil), loads...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Target.Start < ordered[j].Target.Start })
	var previousEnd uint64
	var totalPages uint64
	for i, load := range ordered {
		if !load.Target.WellFormed() || load.Target.Length() == 0 || !hostarch.IsPageAligned(load.Target.Start) ||
			!hostarch.IsPageAligned(load.Target.End) || !hostarch.IsPageAligned(load.StreamOffset) {
			return fail(fmt.Errorf("invalid packed load range %d: %+v", i, load))
		}
		if i != 0 && load.Target.Start < previousEnd {
			return fail(fmt.Errorf("overlapping packed load ranges at %#x", load.Target.Start))
		}
		previousEnd = load.Target.End
		totalPages += load.Target.Length() / hostarch.PageSize
	}
	findStreamOffset := func(old uint64) (uint64, bool) {
		i := sort.Search(len(ordered), func(i int) bool { return ordered[i].Target.End > old })
		if i == len(ordered) || old < ordered[i].Target.Start {
			return 0, false
		}
		return ordered[i].StreamOffset + old - ordered[i].Target.Start, true
	}
	pages := make(map[uint64]packedPage, totalPages)
	for _, run := range manifest.Runs {
		for i := uint16(0); i < run.Pages; i++ {
			old := run.OldOffset + uint64(i)*hostarch.PageSize
			stream, ok := findStreamOffset(old)
			if !ok {
				return fail(fmt.Errorf("manifest offset %#x has no packed load range", old))
			}
			if _, exists := pages[stream]; exists {
				return fail(fmt.Errorf("packed stream repeats page at %#x", stream))
			}
			pages[stream] = packedPage{object: run.Object, slot: run.FirstSlot + uint8(i)}
		}
	}
	if uint64(len(pages)) != totalPages {
		return fail(fmt.Errorf("packed manifest maps %d pages, load ranges require %d", len(pages), totalPages))
	}
	return &PackedManifestReader{repository: repository, pages: pages}, nil
}

// ReadAt implements io.ReaderAt. Reads may be smaller than a chunk; immutable
// chunks touched by one call are decoded once and discarded afterward.
func (r *PackedManifestReader) ReadAt(dst []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative packed manifest offset")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, errors.New("packed manifest reader is closed")
	}
	r.mu.Unlock()
	chunks := make(map[ID]Chunk)
	written := 0
	for written < len(dst) {
		absolute := uint64(offset) + uint64(written)
		pageOffset := absolute &^ uint64(hostarch.PageSize-1)
		page, ok := r.pages[pageOffset]
		if !ok {
			if written == 0 {
				return 0, io.EOF
			}
			return written, io.EOF
		}
		chunk, ok := chunks[page.object]
		if !ok {
			var err error
			chunk, err = r.repository.chunks.Get(page.object)
			if err != nil {
				return written, err
			}
			chunks[page.object] = chunk
		}
		var stored *Page
		for i := range chunk.Pages {
			if chunk.Pages[i].Slot == page.slot {
				stored = &chunk.Pages[i]
				break
			}
		}
		if stored == nil {
			return written, fmt.Errorf("memory object %s omits slot %d", page.object, page.slot)
		}
		within := absolute - pageOffset
		n := min(len(dst)-written, int(hostarch.PageSize-within))
		if stored.Zero {
			clear(dst[written : written+n])
		} else {
			copy(dst[written:written+n], stored.Data[within:within+uint64(n)])
		}
		written += n
	}
	return written, nil
}

// Close releases the donated repository descriptor.
func (r *PackedManifestReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	return r.repository.Close()
}
