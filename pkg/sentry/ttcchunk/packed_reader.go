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
	"os"
	"sort"
	"sync"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

// GraphPackedManifestReader exposes graph-native 1 MiB memory objects through
// the dense byte offsets of an already-running remote page load. It allows a
// live target to change its backing source without moving resident pages.
type GraphPackedManifestReader struct {
	data    *ttcsnapshot.LocalDataStore
	remote  *ttcsnapshot.ObjectRelayClient
	pages   map[uint64]graphPackedPage
	ownData bool
	fetcher objectFetcher
	mu      sync.Mutex
	closed  bool
}

type graphPackedPage struct {
	object ttcsnapshot.ObjectID
	slot   uint8
}

// NewGraphPackedManifestReader constructs a graph-backed reader in the packed
// stream coordinate system described by loads. remote may be nil when every
// graph object is already local. The reader adopts remote and, when ownData is
// true, data.
func NewGraphPackedManifestReader(data *ttcsnapshot.LocalDataStore, remote *ttcsnapshot.ObjectRelayClient, manifest Manifest, loads []pgalloc.RemoteLoadRange, ownData bool) (*GraphPackedManifestReader, error) {
	fail := func(err error) (*GraphPackedManifestReader, error) {
		if remote != nil {
			err = errors.Join(err, remote.Close())
		}
		if ownData && data != nil {
			err = errors.Join(err, data.Close())
		}
		return nil, err
	}
	if data == nil {
		return fail(errors.New("graph packed manifest reader data store is nil"))
	}
	objects, err := GraphChunkObjects(manifest)
	if err != nil {
		return fail(err)
	}
	ordered := append([]pgalloc.RemoteLoadRange(nil), loads...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Target.Start < ordered[j].Target.Start })
	var previousEnd, totalPages uint64
	for i, load := range ordered {
		if !load.Target.WellFormed() || load.Target.Length() == 0 || !hostarch.IsPageAligned(load.Target.Start) ||
			!hostarch.IsPageAligned(load.Target.End) || !hostarch.IsPageAligned(load.StreamOffset) {
			return fail(fmt.Errorf("invalid graph packed load range %d: %+v", i, load))
		}
		if i != 0 && load.Target.Start < previousEnd {
			return fail(fmt.Errorf("overlapping graph packed load ranges at %#x", load.Target.Start))
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
	pages := make(map[uint64]graphPackedPage, totalPages)
	for _, run := range manifest.Runs {
		object, ok := objects[run.Object]
		if !ok {
			return fail(fmt.Errorf("graph manifest object %s has no graph identity", run.Object))
		}
		for i := uint16(0); i < run.Pages; i++ {
			old := run.OldOffset + uint64(i)*hostarch.PageSize
			stream, ok := findStreamOffset(old)
			if !ok {
				return fail(fmt.Errorf("graph manifest offset %#x has no packed load range", old))
			}
			if _, exists := pages[stream]; exists {
				return fail(fmt.Errorf("graph packed stream repeats page at %#x", stream))
			}
			pages[stream] = graphPackedPage{object: object, slot: run.FirstSlot + uint8(i)}
		}
	}
	if uint64(len(pages)) != totalPages {
		return fail(fmt.Errorf("graph manifest maps %d pages, load ranges require %d", len(pages), totalPages))
	}
	return &GraphPackedManifestReader{data: data, remote: remote, pages: pages, ownData: ownData, fetcher: objectFetcher{data: data, remote: remote}}, nil
}

func (r *GraphPackedManifestReader) openObject(id ttcsnapshot.ObjectID) (*os.File, error) {
	return r.fetcher.open(id, int64(pgalloc.SparseChunkBytes))
}

// ReadAt implements io.ReaderAt without decoding a compatibility chunk.
func (r *GraphPackedManifestReader) ReadAt(dst []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative graph packed manifest offset")
	}
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return 0, errors.New("graph packed manifest reader is closed")
	}
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
		within := absolute - pageOffset
		n := min(len(dst)-written, int(hostarch.PageSize-within))
		file, err := r.openObject(page.object)
		if err != nil {
			return written, err
		}
		readOffset := int64(page.slot)*hostarch.PageSize + int64(within)
		read, readErr := file.ReadAt(dst[written:written+n], readOffset)
		closeErr := file.Close()
		written += read
		if readErr != nil || closeErr != nil {
			return written, errors.Join(readErr, closeErr)
		}
	}
	return written, nil
}

// Close releases the relay and donated local data store.
func (r *GraphPackedManifestReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	var remoteErr, dataErr error
	if r.remote != nil {
		remoteErr = r.remote.Close()
	}
	if r.ownData {
		dataErr = r.data.Close()
	}
	return errors.Join(remoteErr, dataErr)
}
