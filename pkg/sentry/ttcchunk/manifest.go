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
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

const (
	manifestVersion        = 1
	maxManifestStringBytes = 1 << 20
	maxManifestRuns        = 1 << 32
)

var manifestMagic = [16]byte{'T', 'T', 'C', 'M', 'E', 'M', 'M', 'A', 'N', 'I', 'F', 'E', 'S', 'T', 0, 1}

// ManifestID is the content address of an encoded memory manifest.
type ManifestID [sha256.Size]byte

// String returns the lowercase hexadecimal manifest content address.
func (id ManifestID) String() string { return hex.EncodeToString(id[:]) }

// ParseManifestID parses a manifest content address.
func ParseManifestID(value string) (ManifestID, error) {
	var id ManifestID
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != len(id) {
		return id, fmt.Errorf("invalid TTC memory manifest ID %q", value)
	}
	copy(id[:], decoded)
	return id, nil
}

// ManifestRun maps a contiguous source MemoryFile range and logical identity
// run to slots in one immutable object.
type ManifestRun struct {
	ResourceID   checkpoint.ResourceID
	OldOffset    uint64
	References   uint64
	FirstPage    pgalloc.LogicalPageID
	FirstVersion pgalloc.PageVersionID
	Logical      pgalloc.LogicalChunkID
	FirstSlot    uint8
	Pages        uint16
	Object       ID
}

// Manifest is an immutable branch memory root.
type Manifest struct {
	Runs []ManifestRun
}

func bindingLess(left, right *StoredBinding) bool {
	if left.Binding.ResourceID.ContainerName != right.Binding.ResourceID.ContainerName {
		return left.Binding.ResourceID.ContainerName < right.Binding.ResourceID.ContainerName
	}
	if left.Binding.ResourceID.Path != right.Binding.ResourceID.Path {
		return left.Binding.ResourceID.Path < right.Binding.ResourceID.Path
	}
	if left.Binding.OldOffset != right.Binding.OldOffset {
		return left.Binding.OldOffset < right.Binding.OldOffset
	}
	if cmp := compareAllocator(left.Binding.Page.Allocator, right.Binding.Page.Allocator); cmp != 0 {
		return cmp < 0
	}
	if left.Binding.Page.Sequence != right.Binding.Page.Sequence {
		return left.Binding.Page.Sequence < right.Binding.Page.Sequence
	}
	if cmp := compareAllocator(left.Binding.Version.Allocator, right.Binding.Version.Allocator); cmp != 0 {
		return cmp < 0
	}
	return left.Binding.Version.Sequence < right.Binding.Version.Sequence
}

func validateStoredBinding(stored StoredBinding) error {
	binding := &stored.Binding
	chunk, slot := binding.Page.Chunk()
	if binding.Chunk != chunk || binding.Slot != slot || binding.References == 0 || stored.Object == (ID{}) || !hostarch.IsPageAligned(binding.OldOffset) {
		return fmt.Errorf("invalid stored page binding: %+v", stored)
	}
	if binding.Version.Allocator == (pgalloc.LogicalAllocatorID{}) {
		return fmt.Errorf("stored page %+v has zero version allocator", binding.Page)
	}
	return nil
}

func runCanAppend(run *ManifestRun, stored *StoredBinding) bool {
	if run.Pages == PagesPerChunk || run.ResourceID != stored.Binding.ResourceID || run.References != stored.Binding.References || run.Object != stored.Object || run.Logical != stored.Binding.Chunk {
		return false
	}
	delta := uint64(run.Pages)
	return stored.Binding.OldOffset == run.OldOffset+delta*hostarch.PageSize &&
		stored.Binding.Page.Allocator == run.FirstPage.Allocator && stored.Binding.Page.Sequence == run.FirstPage.Sequence+delta &&
		stored.Binding.Version.Allocator == run.FirstVersion.Allocator && stored.Binding.Version.Sequence == run.FirstVersion.Sequence+delta &&
		uint16(stored.Binding.Slot) == uint16(run.FirstSlot)+run.Pages
}

// CompactBindings builds a deterministic run-compressed manifest.
func CompactBindings(bindings []StoredBinding) (Manifest, error) {
	ordered := append([]StoredBinding(nil), bindings...)
	for _, binding := range ordered {
		if err := validateStoredBinding(binding); err != nil {
			return Manifest{}, err
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return bindingLess(&ordered[i], &ordered[j]) })
	manifest := Manifest{Runs: make([]ManifestRun, 0, len(ordered))}
	for i := range ordered {
		stored := &ordered[i]
		if len(manifest.Runs) != 0 && runCanAppend(&manifest.Runs[len(manifest.Runs)-1], stored) {
			manifest.Runs[len(manifest.Runs)-1].Pages++
			continue
		}
		manifest.Runs = append(manifest.Runs, ManifestRun{
			ResourceID:   stored.Binding.ResourceID,
			OldOffset:    stored.Binding.OldOffset,
			References:   stored.Binding.References,
			FirstPage:    stored.Binding.Page,
			FirstVersion: stored.Binding.Version,
			Logical:      stored.Binding.Chunk,
			FirstSlot:    stored.Binding.Slot,
			Pages:        1,
			Object:       stored.Object,
		})
	}
	return manifest, nil
}

func appendUint32(dst []byte, value uint32) []byte {
	var encoded [4]byte
	binary.LittleEndian.PutUint32(encoded[:], value)
	return append(dst, encoded[:]...)
}

func appendUint64(dst []byte, value uint64) []byte {
	var encoded [8]byte
	binary.LittleEndian.PutUint64(encoded[:], value)
	return append(dst, encoded[:]...)
}

func appendString(dst []byte, value string) ([]byte, error) {
	if len(value) > maxManifestStringBytes {
		return nil, fmt.Errorf("manifest string has %d bytes", len(value))
	}
	dst = appendUint32(dst, uint32(len(value)))
	return append(dst, value...), nil
}

// EncodeManifest validates and deterministically encodes manifest.
func EncodeManifest(manifest Manifest) ([]byte, ManifestID, error) {
	if uint64(len(manifest.Runs)) > maxManifestRuns {
		return nil, ManifestID{}, fmt.Errorf("memory manifest has %d runs", len(manifest.Runs))
	}
	data := append([]byte(nil), manifestMagic[:]...)
	data = appendUint32(data, manifestVersion)
	data = appendUint32(data, uint32(hostarch.PageSize))
	data = appendUint64(data, uint64(len(manifest.Runs)))
	for i, run := range manifest.Runs {
		if run.Pages == 0 || uint16(run.FirstSlot)+run.Pages > PagesPerChunk || run.References == 0 || run.Object == (ID{}) || !hostarch.IsPageAligned(run.OldOffset) {
			return nil, ManifestID{}, fmt.Errorf("invalid memory manifest run %d: %+v", i, run)
		}
		chunk, slot := run.FirstPage.Chunk()
		if chunk != run.Logical || slot != run.FirstSlot || run.FirstVersion.Allocator == (pgalloc.LogicalAllocatorID{}) {
			return nil, ManifestID{}, fmt.Errorf("inconsistent memory manifest run %d: %+v", i, run)
		}
		var err error
		data, err = appendString(data, run.ResourceID.ContainerName)
		if err != nil {
			return nil, ManifestID{}, err
		}
		data, err = appendString(data, run.ResourceID.Path)
		if err != nil {
			return nil, ManifestID{}, err
		}
		data = appendUint64(data, run.OldOffset)
		data = appendUint64(data, run.References)
		data = append(data, run.FirstPage.Allocator[:]...)
		data = appendUint64(data, run.FirstPage.Sequence)
		data = append(data, run.FirstVersion.Allocator[:]...)
		data = appendUint64(data, run.FirstVersion.Sequence)
		data = append(data, run.Logical.Allocator[:]...)
		data = appendUint64(data, run.Logical.Sequence)
		data = append(data, run.FirstSlot)
		data = appendUint32(data, uint32(run.Pages))
		data = append(data, run.Object[:]...)
	}
	id := sha256.Sum256(data)
	return data, id, nil
}

type manifestReader struct {
	data []byte
	off  int
}

func (r *manifestReader) take(length int) ([]byte, error) {
	if length < 0 || length > len(r.data)-r.off {
		return nil, io.ErrUnexpectedEOF
	}
	value := r.data[r.off : r.off+length]
	r.off += length
	return value, nil
}

func (r *manifestReader) uint32() (uint32, error) {
	data, err := r.take(4)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(data), nil
}

func (r *manifestReader) uint64() (uint64, error) {
	data, err := r.take(8)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint64(data), nil
}

func (r *manifestReader) string() (string, error) {
	length, err := r.uint32()
	if err != nil {
		return "", err
	}
	if length > maxManifestStringBytes {
		return "", fmt.Errorf("manifest string has %d bytes", length)
	}
	data, err := r.take(int(length))
	return string(data), err
}

// DecodeManifest verifies and decodes one manifest.
func DecodeManifest(data []byte, expected *ManifestID) (Manifest, error) {
	if expected != nil && ManifestID(sha256.Sum256(data)) != *expected {
		return Manifest{}, errors.New("memory manifest content address mismatch")
	}
	r := manifestReader{data: data}
	magic, err := r.take(len(manifestMagic))
	if err != nil || !bytes.Equal(magic, manifestMagic[:]) {
		return Manifest{}, errors.New("invalid TTC memory manifest header")
	}
	version, err := r.uint32()
	if err != nil || version != manifestVersion {
		return Manifest{}, fmt.Errorf("unsupported TTC memory manifest version %d", version)
	}
	pageSize, err := r.uint32()
	if err != nil || pageSize != uint32(hostarch.PageSize) {
		return Manifest{}, fmt.Errorf("memory manifest page size is %d, want %d", pageSize, hostarch.PageSize)
	}
	runCount, err := r.uint64()
	if err != nil || runCount > maxManifestRuns || runCount > uint64(len(data)) {
		return Manifest{}, errors.New("invalid memory manifest run count")
	}
	manifest := Manifest{Runs: make([]ManifestRun, int(runCount))}
	for i := range manifest.Runs {
		run := &manifest.Runs[i]
		if run.ResourceID.ContainerName, err = r.string(); err != nil {
			return Manifest{}, err
		}
		if run.ResourceID.Path, err = r.string(); err != nil {
			return Manifest{}, err
		}
		if run.OldOffset, err = r.uint64(); err != nil {
			return Manifest{}, err
		}
		if run.References, err = r.uint64(); err != nil {
			return Manifest{}, err
		}
		value, err := r.take(16)
		if err != nil {
			return Manifest{}, err
		}
		copy(run.FirstPage.Allocator[:], value)
		if run.FirstPage.Sequence, err = r.uint64(); err != nil {
			return Manifest{}, err
		}
		value, err = r.take(16)
		if err != nil {
			return Manifest{}, err
		}
		copy(run.FirstVersion.Allocator[:], value)
		if run.FirstVersion.Sequence, err = r.uint64(); err != nil {
			return Manifest{}, err
		}
		value, err = r.take(16)
		if err != nil {
			return Manifest{}, err
		}
		copy(run.Logical.Allocator[:], value)
		if run.Logical.Sequence, err = r.uint64(); err != nil {
			return Manifest{}, err
		}
		value, err = r.take(1)
		if err != nil {
			return Manifest{}, err
		}
		run.FirstSlot = value[0]
		pages, err := r.uint32()
		if err != nil || pages > PagesPerChunk {
			return Manifest{}, errors.New("invalid memory manifest run length")
		}
		run.Pages = uint16(pages)
		value, err = r.take(len(run.Object))
		if err != nil {
			return Manifest{}, err
		}
		copy(run.Object[:], value)
	}
	if r.off != len(data) {
		return Manifest{}, errors.New("memory manifest has trailing bytes")
	}
	// Reuse the encoder's complete structural validation.
	if _, _, err := EncodeManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// Expand reconstructs page-granular stored bindings for restore.
func (m Manifest) Expand() []StoredBinding {
	var bindings []StoredBinding
	for _, run := range m.Runs {
		for page := uint16(0); page < run.Pages; page++ {
			delta := uint64(page)
			slot := run.FirstSlot + uint8(page)
			bindings = append(bindings, StoredBinding{
				Binding: pgalloc.CapturedPageBinding{
					ResourceID: run.ResourceID,
					OldOffset:  run.OldOffset + delta*hostarch.PageSize,
					References: run.References,
					Page: pgalloc.LogicalPageID{
						Allocator: run.FirstPage.Allocator,
						Sequence:  run.FirstPage.Sequence + delta,
					},
					Version: pgalloc.PageVersionID{
						Allocator: run.FirstVersion.Allocator,
						Sequence:  run.FirstVersion.Sequence + delta,
					},
					Chunk: run.Logical,
					Slot:  slot,
				},
				Object: run.Object,
			})
		}
	}
	return bindings
}
