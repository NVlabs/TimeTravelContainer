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
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

var graphCaptureMagic = [16]byte{'T', 'T', 'C', 'G', 'R', 'A', 'P', 'H', 'C', 'A', 'P', 'T', 'U', 'R', 'E', 2}

const (
	maxGraphCaptureManifestBytes = 256 << 20
	maxGraphCaptureObjectBytes   = 2 << 20
)

// WriteGraphCapture writes an ephemeral, bounded transport stream containing
// the branch-reachable memory manifest and immutable chunks. The stream is a
// daemon/Sentry transaction channel, not a persistent storage format.
func WriteGraphCapture(w io.Writer, snapshot *pgalloc.RemoteSnapshot, bindings []pgalloc.CapturedPageBinding) error {
	if w == nil || snapshot == nil {
		return errors.New("graph memory capture requires a writer and snapshot")
	}
	groups, stored, err := planGraphCapture(bindings)
	if err != nil {
		return err
	}
	if err := writeGraphCaptureHeader(w, stored, len(groups)); err != nil {
		return err
	}
	zeroPage := make([]byte, hostarch.PageSize)
	for _, group := range groups {
		chunk := Chunk{Logical: group.logical, Pages: make([]Page, 0, len(group.bindings))}
		for _, binding := range group.bindings {
			data := make([]byte, hostarch.PageSize)
			n, err := snapshot.CopyStreamAt(data, int64(binding.StreamOffset))
			if err != nil || n != len(data) {
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				return fmt.Errorf("reading captured page at stream offset %#x: %w", binding.StreamOffset, err)
			}
			page := Page{Slot: binding.Slot, Version: binding.Version}
			if bytes.Equal(data, zeroPage) {
				page.Zero = true
			} else {
				page.Data = data
			}
			chunk.Pages = append(chunk.Pages, page)
		}
		encoded, _, err := Encode(chunk)
		if err != nil {
			return err
		}
		if err := writeGraphCaptureObject(w, group.id, encoded); err != nil {
			return err
		}
	}
	return nil
}

func writeGraphCapture(w io.Writer, objects []Object, stored []StoredBinding) error {
	if err := writeGraphCaptureHeader(w, stored, len(objects)); err != nil {
		return err
	}
	objects = append([]Object(nil), objects...)
	sort.Slice(objects, func(i, j int) bool { return bytes.Compare(objects[i].ID[:], objects[j].ID[:]) < 0 })
	for _, object := range objects {
		if err := writeGraphCaptureObject(w, object.ID, object.Data); err != nil {
			return err
		}
	}
	return nil
}

func writeGraphCaptureHeader(w io.Writer, stored []StoredBinding, objectCount int) error {
	manifest, err := CompactBindings(stored)
	if err != nil {
		return err
	}
	manifestData, _, err := EncodeManifest(manifest)
	if err != nil {
		return err
	}
	if len(manifestData) > maxGraphCaptureManifestBytes {
		return fmt.Errorf("graph memory manifest has %d bytes, limit is %d", len(manifestData), maxGraphCaptureManifestBytes)
	}
	if err := writeGraphCaptureBytes(w, graphCaptureMagic[:]); err != nil {
		return err
	}
	if err := writeGraphCaptureUint64(w, uint64(len(manifestData))); err != nil {
		return err
	}
	if err := writeGraphCaptureUint64(w, uint64(objectCount)); err != nil {
		return err
	}
	if err := writeGraphCaptureBytes(w, manifestData); err != nil {
		return err
	}
	return nil
}

func writeGraphCaptureObject(w io.Writer, id ID, data []byte) error {
	if len(data) == 0 || len(data) > maxGraphCaptureObjectBytes {
		return fmt.Errorf("graph memory object %s has invalid size %d", id, len(data))
	}
	if err := writeGraphCaptureBytes(w, id[:]); err != nil {
		return err
	}
	if err := writeGraphCaptureUint64(w, uint64(len(data))); err != nil {
		return err
	}
	return writeGraphCaptureBytes(w, data)
}

type graphCaptureGroup struct {
	id       ID
	logical  pgalloc.LogicalChunkID
	bindings []pgalloc.CapturedPageBinding
	used     [32]byte
}

func graphCaptureObjectID(sequence uint64) ID {
	var id ID
	copy(id[:16], []byte("TTCGRAPHSTREAM02"))
	binary.BigEndian.PutUint64(id[24:], sequence)
	return id
}

// planGraphCapture assigns transport-local object IDs using metadata only.
// It performs no page reads, which lets the manifest precede a bounded stream
// of one-chunk payloads.
func planGraphCapture(bindings []pgalloc.CapturedPageBinding) ([]graphCaptureGroup, []StoredBinding, error) {
	ordered := append([]pgalloc.CapturedPageBinding(nil), bindings...)
	sort.Slice(ordered, func(i, j int) bool {
		left, right := &ordered[i], &ordered[j]
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
	groupsByLogical := make(map[pgalloc.LogicalChunkID][]int)
	groups := make([]graphCaptureGroup, 0)
	stored := make([]StoredBinding, 0, len(ordered))
	seenVersions := make(map[struct {
		page    pgalloc.LogicalPageID
		version pgalloc.PageVersionID
	}]struct{}, len(ordered))
	for _, binding := range ordered {
		chunk, slot := binding.Page.Chunk()
		if binding.Chunk != chunk || binding.Slot != slot || binding.References == 0 || binding.Version.Allocator == (pgalloc.LogicalAllocatorID{}) {
			return nil, nil, fmt.Errorf("invalid captured page binding: %+v", binding)
		}
		versionKey := struct {
			page    pgalloc.LogicalPageID
			version pgalloc.PageVersionID
		}{binding.Page, binding.Version}
		if _, exists := seenVersions[versionKey]; exists {
			return nil, nil, fmt.Errorf("captured page %+v repeats version %+v", binding.Page, binding.Version)
		}
		seenVersions[versionKey] = struct{}{}
		groupIndex := -1
		for _, candidate := range groupsByLogical[binding.Chunk] {
			if !hasBit(&groups[candidate].used, binding.Slot) {
				groupIndex = candidate
				break
			}
		}
		if groupIndex < 0 {
			groupIndex = len(groups)
			id := graphCaptureObjectID(uint64(groupIndex + 1))
			groups = append(groups, graphCaptureGroup{id: id, logical: binding.Chunk})
			groupsByLogical[binding.Chunk] = append(groupsByLogical[binding.Chunk], groupIndex)
		}
		group := &groups[groupIndex]
		setBit(&group.used, binding.Slot)
		group.bindings = append(group.bindings, binding)
		stored = append(stored, StoredBinding{Binding: binding, Object: group.id})
	}
	return groups, stored, nil
}

func writeGraphCaptureUint64(w io.Writer, value uint64) error {
	var data [8]byte
	binary.LittleEndian.PutUint64(data[:], value)
	return writeGraphCaptureBytes(w, data[:])
}

func writeGraphCaptureBytes(w io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(data) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func readGraphCaptureUint64(r io.Reader) (uint64, error) {
	var data [8]byte
	if _, err := io.ReadFull(r, data[:]); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(data[:]), nil
}

// ImportGraphCapture consumes a direct Sentry capture stream and atomically
// installs its graph roots and bulk objects. Persistent compatibility chunk
// repositories are never involved.
func ImportGraphCapture(r io.Reader, graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, origin ttcsnapshot.SnapshotID, rootPrefix string) (GraphImportResult, error) {
	if r == nil {
		return GraphImportResult{}, errors.New("graph memory capture reader is nil")
	}
	var magic [len(graphCaptureMagic)]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return GraphImportResult{}, fmt.Errorf("reading graph memory capture header: %w", err)
	}
	if magic != graphCaptureMagic {
		return GraphImportResult{}, errors.New("invalid graph memory capture magic")
	}
	manifestLength, err := readGraphCaptureUint64(r)
	if err != nil {
		return GraphImportResult{}, fmt.Errorf("reading graph memory manifest length: %w", err)
	}
	objectCount, err := readGraphCaptureUint64(r)
	if err != nil {
		return GraphImportResult{}, fmt.Errorf("reading graph memory object count: %w", err)
	}
	if manifestLength == 0 || manifestLength > maxGraphCaptureManifestBytes {
		return GraphImportResult{}, fmt.Errorf("graph memory manifest length %d is invalid", manifestLength)
	}
	manifestData := make([]byte, int(manifestLength))
	if _, err := io.ReadFull(r, manifestData); err != nil {
		return GraphImportResult{}, fmt.Errorf("reading graph memory manifest: %w", err)
	}
	manifest, err := DecodeManifest(manifestData, nil)
	if err != nil {
		return GraphImportResult{}, fmt.Errorf("decoding graph memory manifest: %w", err)
	}
	wanted := make(map[ID]struct{})
	for _, run := range manifest.Runs {
		wanted[run.Object] = struct{}{}
	}
	if objectCount != uint64(len(wanted)) {
		return GraphImportResult{}, fmt.Errorf("graph memory capture has %d objects, manifest references %d", objectCount, len(wanted))
	}
	objectIndex := uint64(0)
	readObject := func(expected ID) (Chunk, error) {
		if objectIndex >= objectCount {
			return Chunk{}, fmt.Errorf("graph memory object %s is missing", expected)
		}
		var id ID
		if _, err := io.ReadFull(r, id[:]); err != nil {
			return Chunk{}, fmt.Errorf("reading graph memory object %d identity: %w", objectIndex, err)
		}
		if id != expected {
			return Chunk{}, fmt.Errorf("graph memory object %d is %s, want %s", objectIndex, id, expected)
		}
		length, err := readGraphCaptureUint64(r)
		if err != nil {
			return Chunk{}, fmt.Errorf("reading graph memory object %d length: %w", objectIndex, err)
		}
		if length == 0 || length > maxGraphCaptureObjectBytes {
			return Chunk{}, fmt.Errorf("graph memory object %s length %d is invalid", id, length)
		}
		encoded := make([]byte, int(length))
		if _, err := io.ReadFull(r, encoded); err != nil {
			return Chunk{}, fmt.Errorf("reading graph memory object %s: %w", id, err)
		}
		chunk, err := Decode(encoded, nil)
		if err != nil {
			return Chunk{}, fmt.Errorf("decoding graph memory object %s: %w", id, err)
		}
		objectIndex++
		if objectIndex == objectCount {
			var trailing [1]byte
			if n, err := r.Read(trailing[:]); n != 0 || err == nil {
				return Chunk{}, errors.New("graph memory capture has trailing data")
			} else if !errors.Is(err, io.EOF) {
				return Chunk{}, fmt.Errorf("finishing graph memory capture: %w", err)
			}
		}
		return chunk, nil
	}
	if objectCount == 0 {
		var trailing [1]byte
		if n, err := r.Read(trailing[:]); n != 0 || err == nil {
			return GraphImportResult{}, errors.New("empty graph memory capture has trailing data")
		} else if !errors.Is(err, io.EOF) {
			return GraphImportResult{}, fmt.Errorf("finishing empty graph memory capture: %w", err)
		}
	}
	return importSnapshotGraph(graph, data, origin, manifest, rootPrefix, readObject)
}
