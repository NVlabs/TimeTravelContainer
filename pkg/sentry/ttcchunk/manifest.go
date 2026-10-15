// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcchunk

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

const (
	// A manifest of version 1 has every page at its slot in its object; one
	// of version 2 says where each run's pages are; one of version 3 says
	// too which runs are halves of 2 MiB pages, which builds that predate
	// them refuse.
	manifestVersionSlotted = 1
	manifestVersionPacked  = 2
	manifestVersion        = 3
	maxManifestStringBytes = 1 << 20
	maxManifestRuns        = 1 << 32
)

// manifestRunHuge marks, in a run's flags, a half of a 2 MiB page.
const manifestRunHuge = 1

var manifestMagic = [16]byte{'T', 'T', 'C', 'M', 'E', 'M', 'M', 'A', 'N', 'I', 'F', 'E', 'S', 'T', 0, 1}

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
	// ObjectPage is the page of Object that holds the run's first page; the
	// others follow it. A capture object holds each page at its slot, and a
	// graph chunk may hold the pages of several logical chunks packed.
	ObjectPage uint8
	// Huge is true if the run is half of a 2 MiB page (see
	// pgalloc.LogicalPageRange.Huge): its 256 base pages, at their slots of
	// an Object of their own, from the page's first or 257th, at the offset
	// of the 2 MiB page or 1 MiB past it. The run of its other half follows
	// or precedes it.
	Huge bool
}

// Manifest is an immutable branch memory root.
type Manifest struct {
	Runs []ManifestRun
}

// StoredBinding extends a branch binding with the transaction-local object
// carrying its page bytes in the capture stream.
type StoredBinding struct {
	Binding pgalloc.CapturedPageBinding
	Object  ID
	// ObjectPage is the page of Object that holds the binding's page.
	ObjectPage uint8
	// Huge is true if the binding's page is a base page of a 2 MiB page,
	// which Binding binds a base page of: see ManifestRun.Huge.
	Huge bool
}

// capturedBinding is binding, carried by object of a capture, which holds each
// page at its slot.
func capturedBinding(binding pgalloc.CapturedPageBinding, object ID) StoredBinding {
	return StoredBinding{Binding: binding, Object: object, ObjectPage: binding.Slot}
}

func compareAllocator(a, b pgalloc.LogicalAllocatorID) int { return bytes.Compare(a[:], b[:]) }

func compareBindings(left, right *StoredBinding) int {
	if c := cmp.Compare(left.Binding.ResourceID.ContainerName, right.Binding.ResourceID.ContainerName); c != 0 {
		return c
	}
	if c := cmp.Compare(left.Binding.ResourceID.Path, right.Binding.ResourceID.Path); c != 0 {
		return c
	}
	if c := cmp.Compare(left.Binding.OldOffset, right.Binding.OldOffset); c != 0 {
		return c
	}
	if c := compareAllocator(left.Binding.Page.Allocator, right.Binding.Page.Allocator); c != 0 {
		return c
	}
	if c := cmp.Compare(left.Binding.Page.Sequence, right.Binding.Page.Sequence); c != 0 {
		return c
	}
	if c := compareAllocator(left.Binding.Version.Allocator, right.Binding.Version.Allocator); c != 0 {
		return c
	}
	return cmp.Compare(left.Binding.Version.Sequence, right.Binding.Version.Sequence)
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
	if run.Pages == PagesPerChunk || run.ResourceID != stored.Binding.ResourceID || run.References != stored.Binding.References || run.Object != stored.Object || run.Logical != stored.Binding.Chunk || run.Huge != stored.Huge {
		return false
	}
	delta := uint64(run.Pages)
	return stored.Binding.OldOffset == run.OldOffset+delta*hostarch.PageSize &&
		stored.Binding.Page.Allocator == run.FirstPage.Allocator && stored.Binding.Page.Sequence == run.FirstPage.Sequence+delta &&
		stored.Binding.Version.Allocator == run.FirstVersion.Allocator && stored.Binding.Version.Sequence == run.FirstVersion.Sequence+delta &&
		uint16(stored.Binding.Slot) == uint16(run.FirstSlot)+run.Pages &&
		uint16(stored.ObjectPage) == uint16(run.ObjectPage)+run.Pages
}

// CompactBindings builds a deterministic run-compressed manifest.
func CompactBindings(bindings []StoredBinding) (Manifest, error) {
	return compactBindings(len(bindings), func(i int) StoredBinding { return bindings[i] })
}

// compactBindings is CompactBindings of count bindings, the ith of which
// binding returns.
func compactBindings(count int, binding func(int) StoredBinding) (Manifest, error) {
	// The runs are counted first, so that the manifest's are allocated at
	// once: a manifest of hundreds of thousands grew by a quarter at a time.
	// A capture lists its bindings in order already, and they are counted
	// as they are checked.
	runs := 0
	var run ManifestRun
	countRun := func(stored *StoredBinding) {
		if runs != 0 && runCanAppend(&run, stored) {
			run.Pages++
			return
		}
		run = newManifestRun(stored)
		runs++
	}
	sorted := true
	var previous StoredBinding
	for i := 0; i < count; i++ {
		stored := binding(i)
		if err := validateStoredBinding(stored); err != nil {
			return Manifest{}, err
		}
		sorted = sorted && (i == 0 || compareBindings(&previous, &stored) <= 0)
		if sorted {
			countRun(&stored)
		}
		previous = stored
	}
	var order []int
	at := func(position int) StoredBinding {
		if order != nil {
			return binding(order[position])
		}
		return binding(position)
	}
	if !sorted {
		order = make([]int, count)
		for i := range order {
			order[i] = i
		}
		slices.SortFunc(order, func(a, b int) int {
			left, right := binding(a), binding(b)
			return compareBindings(&left, &right)
		})
		runs = 0
		for position := 0; position < count; position++ {
			stored := at(position)
			countRun(&stored)
		}
	}
	manifest := Manifest{Runs: make([]ManifestRun, 0, runs)}
	for position := 0; position < count; position++ {
		stored := at(position)
		if last := len(manifest.Runs) - 1; last >= 0 && runCanAppend(&manifest.Runs[last], &stored) {
			manifest.Runs[last].Pages++
			continue
		}
		manifest.Runs = append(manifest.Runs, newManifestRun(&stored))
	}
	return manifest, nil
}

// newManifestRun is the run of one page, stored.
func newManifestRun(stored *StoredBinding) ManifestRun {
	return ManifestRun{
		ResourceID:   stored.Binding.ResourceID,
		OldOffset:    stored.Binding.OldOffset,
		References:   stored.Binding.References,
		FirstPage:    stored.Binding.Page,
		FirstVersion: stored.Binding.Version,
		Logical:      stored.Binding.Chunk,
		FirstSlot:    stored.Binding.Slot,
		Pages:        1,
		Object:       stored.Object,
		ObjectPage:   stored.ObjectPage,
		Huge:         stored.Huge,
	}
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
func EncodeManifest(manifest Manifest) ([]byte, error) {
	if uint64(len(manifest.Runs)) > maxManifestRuns {
		return nil, fmt.Errorf("memory manifest has %d runs", len(manifest.Runs))
	}
	// A manifest is written in the oldest version that says what it holds:
	// one whose pages are all at their slots as version 1, which daemons
	// predating packed chunks read too, and one with no 2 MiB page as
	// version 2.
	version := uint32(manifestVersionSlotted)
	for _, run := range manifest.Runs {
		if run.Huge {
			version = manifestVersion
			break
		}
		if run.ObjectPage != run.FirstSlot {
			version = manifestVersionPacked
		}
	}
	if err := validateHugeRuns(manifest.Runs); err != nil {
		return nil, err
	}
	data := append([]byte(nil), manifestMagic[:]...)
	data = appendUint32(data, version)
	data = appendUint32(data, uint32(hostarch.PageSize))
	data = appendUint64(data, uint64(len(manifest.Runs)))
	for i, run := range manifest.Runs {
		if run.Pages == 0 || uint16(run.FirstSlot)+run.Pages > PagesPerChunk || uint16(run.ObjectPage)+run.Pages > PagesPerChunk || run.References == 0 || run.Object == (ID{}) || !hostarch.IsPageAligned(run.OldOffset) {
			return nil, fmt.Errorf("invalid memory manifest run %d: %+v", i, run)
		}
		chunk, slot := run.FirstPage.Chunk()
		if chunk != run.Logical || slot != run.FirstSlot || run.FirstVersion.Allocator == (pgalloc.LogicalAllocatorID{}) {
			return nil, fmt.Errorf("inconsistent memory manifest run %d: %+v", i, run)
		}
		var err error
		data, err = appendString(data, run.ResourceID.ContainerName)
		if err != nil {
			return nil, err
		}
		data, err = appendString(data, run.ResourceID.Path)
		if err != nil {
			return nil, err
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
		if version >= manifestVersionPacked {
			data = append(data, run.ObjectPage)
		}
		if version >= manifestVersion {
			var flags byte
			if run.Huge {
				flags |= manifestRunHuge
			}
			data = append(data, flags)
		}
	}
	return data, nil
}

// validateHugeRuns checks that each run of runs that is half of a 2 MiB page
// is one, beside the run of its other half.
func validateHugeRuns(runs []ManifestRun) error {
	for i := range runs {
		run := &runs[i]
		if !run.Huge {
			continue
		}
		half := run.FirstPage.Sequence / PagesPerChunk % 2
		if run.FirstSlot != 0 || run.Pages != PagesPerChunk || run.ObjectPage != 0 || run.FirstVersion.Sequence%PagesPerChunk != 0 ||
			run.FirstVersion.Sequence/PagesPerChunk%2 != half || run.OldOffset%SparseHalfBytes != 0 || run.OldOffset/SparseHalfBytes%2 != half {
			return fmt.Errorf("memory manifest run %d is no half of a 2 MiB page: %+v", i, *run)
		}
		if half == 1 {
			continue
		}
		if i+1 == len(runs) {
			return fmt.Errorf("memory manifest run %d, half of a 2 MiB page, has no other half", i)
		}
		upper := &runs[i+1]
		if !upper.Huge || upper.ResourceID != run.ResourceID || upper.References != run.References || upper.Object == run.Object ||
			upper.OldOffset != run.OldOffset+SparseHalfBytes || upper.FirstPage.Allocator != run.FirstPage.Allocator ||
			upper.FirstPage.Sequence != run.FirstPage.Sequence+PagesPerChunk || upper.FirstVersion.Allocator != run.FirstVersion.Allocator ||
			upper.FirstVersion.Sequence != run.FirstVersion.Sequence+PagesPerChunk {
			return fmt.Errorf("memory manifest runs %d and %d are not the halves of a 2 MiB page", i, i+1)
		}
	}
	for i := range runs {
		if run := &runs[i]; run.Huge && run.FirstPage.Sequence/PagesPerChunk%2 == 1 && (i == 0 || !runs[i-1].Huge || runs[i-1].FirstPage.Sequence+PagesPerChunk != run.FirstPage.Sequence || runs[i-1].Object == run.Object) {
			return fmt.Errorf("memory manifest run %d, half of a 2 MiB page, has no other half", i)
		}
	}
	return nil
}

// SparseHalfBytes is the size of a half of a 2 MiB page, a manifest run's.
const SparseHalfBytes = PagesPerChunk * hostarch.PageSize

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
func DecodeManifest(data []byte) (Manifest, error) {
	r := manifestReader{data: data}
	magic, err := r.take(len(manifestMagic))
	if err != nil || !bytes.Equal(magic, manifestMagic[:]) {
		return Manifest{}, errors.New("invalid TTC memory manifest header")
	}
	version, err := r.uint32()
	if err != nil || version < manifestVersionSlotted || version > manifestVersion {
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
		run.ObjectPage = run.FirstSlot
		if version >= manifestVersionPacked {
			value, err = r.take(1)
			if err != nil {
				return Manifest{}, err
			}
			run.ObjectPage = value[0]
		}
		if version >= manifestVersion {
			value, err = r.take(1)
			if err != nil {
				return Manifest{}, err
			}
			if value[0]&^manifestRunHuge != 0 {
				return Manifest{}, fmt.Errorf("memory manifest run %d has unknown flags %#x", i, value[0])
			}
			run.Huge = value[0]&manifestRunHuge != 0
		}
	}
	if r.off != len(data) {
		return Manifest{}, errors.New("memory manifest has trailing bytes")
	}
	// Reuse the encoder's complete structural validation.
	if _, err := EncodeManifest(manifest); err != nil {
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
				Object:     run.Object,
				ObjectPage: run.ObjectPage + uint8(page),
				Huge:       run.Huge,
			})
		}
	}
	return bindings
}
