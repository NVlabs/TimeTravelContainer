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
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
)

var graphCaptureMagic = [16]byte{'T', 'T', 'C', 'G', 'R', 'A', 'P', 'H', 'C', 'A', 'P', 'T', 'U', 'R', 'E', 1}

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
	objects, stored, err := ReadSnapshot(snapshot, bindings)
	if err != nil {
		return err
	}
	return writeGraphCapture(w, objects, stored)
}

func writeGraphCapture(w io.Writer, objects []Object, stored []StoredBinding) error {
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
	if err := writeGraphCaptureUint64(w, uint64(len(objects))); err != nil {
		return err
	}
	if err := writeGraphCaptureBytes(w, manifestData); err != nil {
		return err
	}
	for _, object := range objects {
		if len(object.Data) == 0 || len(object.Data) > maxGraphCaptureObjectBytes {
			return fmt.Errorf("graph memory object %s has invalid size %d", object.ID, len(object.Data))
		}
		if err := writeGraphCaptureBytes(w, object.ID[:]); err != nil {
			return err
		}
		if err := writeGraphCaptureUint64(w, uint64(len(object.Data))); err != nil {
			return err
		}
		if err := writeGraphCaptureBytes(w, object.Data); err != nil {
			return err
		}
	}
	return nil
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
	chunks := make(map[ID]Chunk, len(wanted))
	for i := uint64(0); i < objectCount; i++ {
		var id ID
		if _, err := io.ReadFull(r, id[:]); err != nil {
			return GraphImportResult{}, fmt.Errorf("reading graph memory object %d identity: %w", i, err)
		}
		if _, ok := wanted[id]; !ok {
			return GraphImportResult{}, fmt.Errorf("graph memory stream contains unreferenced object %s", id)
		}
		if _, exists := chunks[id]; exists {
			return GraphImportResult{}, fmt.Errorf("graph memory stream repeats object %s", id)
		}
		length, err := readGraphCaptureUint64(r)
		if err != nil {
			return GraphImportResult{}, fmt.Errorf("reading graph memory object %d length: %w", i, err)
		}
		if length == 0 || length > maxGraphCaptureObjectBytes {
			return GraphImportResult{}, fmt.Errorf("graph memory object %s length %d is invalid", id, length)
		}
		encoded := make([]byte, int(length))
		if _, err := io.ReadFull(r, encoded); err != nil {
			return GraphImportResult{}, fmt.Errorf("reading graph memory object %s: %w", id, err)
		}
		chunk, err := Decode(encoded, &id)
		if err != nil {
			return GraphImportResult{}, fmt.Errorf("decoding graph memory object %s: %w", id, err)
		}
		chunks[id] = chunk
	}
	var trailing [1]byte
	if n, err := r.Read(trailing[:]); n != 0 || err == nil {
		return GraphImportResult{}, errors.New("graph memory capture has trailing data")
	} else if !errors.Is(err, io.EOF) {
		return GraphImportResult{}, fmt.Errorf("finishing graph memory capture: %w", err)
	}
	return importSnapshotGraph(graph, data, origin, manifest, rootPrefix, func(id ID) (Chunk, error) {
		chunk, ok := chunks[id]
		if !ok {
			return Chunk{}, fmt.Errorf("graph memory object %s is missing", id)
		}
		return chunk, nil
	})
}
