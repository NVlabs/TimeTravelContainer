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

var (
	graphCaptureMagic = [16]byte{'T', 'T', 'C', 'G', 'R', 'A', 'P', 'H', 'C', 'A', 'P', 'T', 'U', 'R', 'E', 2}
	// graphCaptureFilteredMagic begins a capture whose reader answers the
	// manifest, on the same stream, with the slots of each object its graph
	// lacks. Only those pages follow.
	graphCaptureFilteredMagic = [16]byte{'T', 'T', 'C', 'G', 'R', 'A', 'P', 'H', 'C', 'A', 'P', 'T', 'U', 'R', 'E', 3}
)

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
	if err := writeGraphCaptureHeader(w, graphCaptureMagic, stored, len(groups)); err != nil {
		return err
	}
	for index := range groups {
		if err := writeGraphCaptureGroup(w, snapshot, &groups[index], nil); err != nil {
			return err
		}
	}
	return nil
}

// WriteFilteredGraphCapture writes a capture as WriteGraphCapture does, except
// that after the manifest the daemon answers, on the same stream, with the
// slots of each object its graph lacks, and only those pages are read and
// sent. The pages a fork still shares with its dehydrated source cost nothing.
func WriteFilteredGraphCapture(stream io.ReadWriter, snapshot *pgalloc.RemoteSnapshot, bindings []pgalloc.CapturedPageBinding) error {
	if stream == nil || snapshot == nil {
		return errors.New("graph memory capture requires a stream and snapshot")
	}
	groups, stored, err := planGraphCapture(bindings)
	if err != nil {
		return err
	}
	if err := writeGraphCaptureHeader(stream, graphCaptureFilteredMagic, stored, len(groups)); err != nil {
		return err
	}
	answer := make([]byte, len(groups)*slotBitmapBytes)
	if _, err := io.ReadFull(stream, answer); err != nil {
		return fmt.Errorf("reading which memory pages the daemon lacks: %w", err)
	}
	for index := range groups {
		group := &groups[index]
		var need [slotBitmapBytes]byte
		copy(need[:], answer[index*slotBitmapBytes:])
		if need == ([slotBitmapBytes]byte{}) {
			continue
		}
		for slot := 0; slot < PagesPerChunk; slot++ {
			if hasBit(&need, uint8(slot)) && !hasBit(&group.used, uint8(slot)) {
				return fmt.Errorf("the daemon asked for slot %d of memory object %s, which the capture does not hold", slot, group.id)
			}
		}
		if err := writeGraphCaptureGroup(stream, snapshot, group, &need); err != nil {
			return err
		}
	}
	return nil
}

var zeroGraphCapturePage [hostarch.PageSize]byte

// writeGraphCaptureGroup reads the pages of group, or only those in need if
// it is set, and writes them as one object.
func writeGraphCaptureGroup(w io.Writer, snapshot *pgalloc.RemoteSnapshot, group *graphCaptureGroup, need *[slotBitmapBytes]byte) error {
	bindings := group.bindings
	if need != nil {
		bindings = make([]pgalloc.CapturedPageBinding, 0, countBits(need))
		for _, binding := range group.bindings {
			if hasBit(need, binding.Slot) {
				bindings = append(bindings, binding)
			}
		}
	}
	buffer := make([]byte, len(bindings)*hostarch.PageSize)
	// Pages the capture stream holds one after another are read with one copy.
	for begin := 0; begin < len(bindings); {
		end := begin + 1
		for end < len(bindings) && bindings[end].StreamOffset == bindings[end-1].StreamOffset+hostarch.PageSize {
			end++
		}
		data := buffer[begin*hostarch.PageSize : end*hostarch.PageSize]
		n, err := snapshot.CopyStreamAt(data, int64(bindings[begin].StreamOffset))
		if err != nil || n != len(data) {
			if err == nil {
				err = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("reading captured pages at stream offset %#x: %w", bindings[begin].StreamOffset, err)
		}
		begin = end
	}
	chunk := Chunk{Logical: group.logical, Pages: make([]Page, 0, len(bindings))}
	for index, binding := range bindings {
		data := buffer[index*hostarch.PageSize : (index+1)*hostarch.PageSize : (index+1)*hostarch.PageSize]
		page := Page{Slot: binding.Slot, Version: binding.Version}
		if bytes.Equal(data, zeroGraphCapturePage[:]) {
			page.Zero = true
		} else {
			page.Data = data
		}
		chunk.Pages = append(chunk.Pages, page)
	}
	encoded, err := Encode(chunk)
	if err != nil {
		return err
	}
	return writeGraphCaptureObject(w, group.id, encoded)
}

func writeGraphCaptureHeader(w io.Writer, magic [16]byte, stored []StoredBinding, objectCount int) error {
	manifest, err := CompactBindings(stored)
	if err != nil {
		return err
	}
	manifestData, err := EncodeManifest(manifest)
	if err != nil {
		return err
	}
	if len(manifestData) > maxGraphCaptureManifestBytes {
		return fmt.Errorf("graph memory manifest has %d bytes, limit is %d", len(manifestData), maxGraphCaptureManifestBytes)
	}
	if err := writeGraphCaptureBytes(w, magic[:]); err != nil {
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
		stored = append(stored, capturedBinding(binding, group.id))
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
	return ImportGraphCaptureWithReusableStates(r, graph, data, origin, rootPrefix, nil)
}

// ImportGraphCaptureWithReusableStates imports a capture while limiting
// stable-page reuse to the specified snapshot authority states. Passing nil
// retains the standalone behavior of reusing every live immutable page.
//
// A filtered capture, from WriteFilteredGraphCapture, needs r to be a two-way
// stream: the import writes to it which pages the graph lacks.
func ImportGraphCaptureWithReusableStates(r io.Reader, graph *ttcsnapshot.Store, data *ttcsnapshot.LocalDataStore, origin ttcsnapshot.SnapshotID, rootPrefix string, reusableStates []ttcsnapshot.SnapshotState) (GraphImportResult, error) {
	if r == nil {
		return GraphImportResult{}, errors.New("graph memory capture reader is nil")
	}
	var magic [len(graphCaptureMagic)]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return GraphImportResult{}, fmt.Errorf("reading graph memory capture header: %w", err)
	}
	var answer io.Writer
	switch magic {
	case graphCaptureMagic:
	case graphCaptureFilteredMagic:
		writer, ok := r.(io.Writer)
		if !ok {
			return GraphImportResult{}, errors.New("a filtered graph memory capture needs a two-way stream")
		}
		answer = writer
	default:
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
	manifest, err := DecodeManifest(manifestData)
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
	// carried is every object the stream holds, in the order it holds them:
	// all of them, unless the Sentry is told which it need not send.
	carried := make([]ID, 0, len(wanted))
	for id := range wanted {
		carried = append(carried, id)
	}
	sort.Slice(carried, func(i, j int) bool { return bytes.Compare(carried[i][:], carried[j][:]) < 0 })
	var source captureSource
	if answer != nil {
		source.need = func(objects []ID, slots [][slotBitmapBytes]byte) error {
			data := make([]byte, 0, len(objects)*slotBitmapBytes)
			carried = carried[:0]
			for index, id := range objects {
				data = append(data, slots[index][:]...)
				if slots[index] != ([slotBitmapBytes]byte{}) {
					carried = append(carried, id)
				}
			}
			if err := writeGraphCaptureBytes(answer, data); err != nil {
				return fmt.Errorf("telling the Sentry which memory pages the graph lacks: %w", err)
			}
			return nil
		}
	}
	next := 0
	// Each object is read into buffer, and its chunk refers to it until the
	// next is read.
	var buffer []byte
	source.chunk = func(expected ID) (Chunk, error) {
		for next < len(carried) {
			id := carried[next]
			next++
			chunk, err := readGraphCaptureObject(r, id, id == expected, &buffer)
			if err != nil {
				return Chunk{}, err
			}
			if id == expected {
				return chunk, nil
			}
		}
		return Chunk{}, fmt.Errorf("graph memory object %s is missing", expected)
	}
	source.finish = func() error {
		for ; next < len(carried); next++ {
			if _, err := readGraphCaptureObject(r, carried[next], false, nil); err != nil {
				return err
			}
		}
		var trailing [1]byte
		if n, err := r.Read(trailing[:]); n != 0 || err == nil {
			return errors.New("graph memory capture has trailing data")
		} else if !errors.Is(err, io.EOF) {
			return fmt.Errorf("finishing graph memory capture: %w", err)
		}
		return nil
	}
	return importCapture(graph, data, origin, manifest, rootPrefix, source, reusableStates)
}

// readGraphCaptureObject reads the next object of a capture stream, which
// must be expected, and decodes it if decode is set. The object is read into
// *buffer, grown as needed, and the chunk's pages refer to it.
func readGraphCaptureObject(r io.Reader, expected ID, decode bool, buffer *[]byte) (Chunk, error) {
	var id ID
	if _, err := io.ReadFull(r, id[:]); err != nil {
		return Chunk{}, fmt.Errorf("reading graph memory object %s identity: %w", expected, err)
	}
	if id != expected {
		return Chunk{}, fmt.Errorf("graph memory object is %s, want %s", id, expected)
	}
	length, err := readGraphCaptureUint64(r)
	if err != nil {
		return Chunk{}, fmt.Errorf("reading graph memory object %s length: %w", id, err)
	}
	if length == 0 || length > maxGraphCaptureObjectBytes {
		return Chunk{}, fmt.Errorf("graph memory object %s length %d is invalid", id, length)
	}
	if !decode {
		if _, err := io.CopyN(io.Discard, r, int64(length)); err != nil {
			return Chunk{}, fmt.Errorf("reading graph memory object %s: %w", id, err)
		}
		return Chunk{}, nil
	}
	if uint64(cap(*buffer)) < length {
		*buffer = make([]byte, maxGraphCaptureObjectBytes)
	}
	encoded := (*buffer)[:length]
	if _, err := io.ReadFull(r, encoded); err != nil {
		return Chunk{}, fmt.Errorf("reading graph memory object %s: %w", id, err)
	}
	chunk, err := decodeInPlace(encoded)
	if err != nil {
		return Chunk{}, fmt.Errorf("decoding graph memory object %s: %w", id, err)
	}
	return chunk, nil
}
