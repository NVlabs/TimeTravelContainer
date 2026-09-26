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

// Package ttcchunk implements TTC's bounded memory capture transport and the
// adapters between Sentry memory and the authoritative snapshot graph.
package ttcchunk

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

const (
	// PagesPerChunk is the number of independently-CoW base pages loaded and
	// evicted as one sparse-memory unit.
	PagesPerChunk = 256
	formatVersion = 1
	headerBytes   = 16 + 8 + 16 + 8 + 32 + 32
	entryBytes    = 1 + 16 + 8
)

var formatMagic = [16]byte{'T', 'T', 'C', 'M', 'E', 'M', 'C', 'H', 'U', 'N', 'K', 0, 0, 0, 0, 1}

// ID is an opaque, transaction-local memory object identity. Persistent graph
// objects use ttcsnapshot.ObjectID instead.
type ID [32]byte

// String returns the lowercase hexadecimal transport identity.
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// Page is one valid logical slot. Data must contain exactly one base page.
// Zero pages may omit Data and set Zero.
type Page struct {
	Slot    uint8
	Version pgalloc.PageVersionID
	Zero    bool
	Data    []byte
}

// Chunk is one immutable version of a stable logical 256-page coordinate.
type Chunk struct {
	Logical pgalloc.LogicalChunkID
	Pages   []Page
}

// slotBitmapBytes is the size of a bitmap with a bit for each slot of a chunk.
const slotBitmapBytes = PagesPerChunk / 8

func setBit(bitmap *[slotBitmapBytes]byte, slot uint8) { bitmap[slot/8] |= byte(1 << (slot % 8)) }
func hasBit(bitmap *[slotBitmapBytes]byte, slot uint8) bool {
	return bitmap[slot/8]&(byte(1)<<(slot%8)) != 0
}

// Encode validates and encodes a chunk for an ephemeral capture stream.
func Encode(chunk Chunk) ([]byte, error) {
	if chunk.Logical.Allocator == (pgalloc.LogicalAllocatorID{}) {
		return nil, errors.New("memory chunk has zero logical allocator ID")
	}
	if len(chunk.Pages) == 0 || len(chunk.Pages) > PagesPerChunk {
		return nil, fmt.Errorf("memory chunk has %d pages", len(chunk.Pages))
	}
	pages := make([]*Page, PagesPerChunk)
	var valid, zero [32]byte
	for i := range chunk.Pages {
		page := &chunk.Pages[i]
		if pages[page.Slot] != nil {
			return nil, fmt.Errorf("memory chunk repeats slot %d", page.Slot)
		}
		if page.Version.Allocator == (pgalloc.LogicalAllocatorID{}) {
			return nil, fmt.Errorf("memory chunk slot %d has zero version allocator ID", page.Slot)
		}
		if page.Zero {
			if len(page.Data) != 0 {
				return nil, fmt.Errorf("zero memory chunk slot %d has payload", page.Slot)
			}
			setBit(&zero, page.Slot)
		} else if len(page.Data) != hostarch.PageSize {
			return nil, fmt.Errorf("memory chunk slot %d has %d payload bytes, want %d", page.Slot, len(page.Data), hostarch.PageSize)
		}
		setBit(&valid, page.Slot)
		pages[page.Slot] = page
	}

	payloadPages := len(chunk.Pages)
	for _, page := range chunk.Pages {
		if page.Zero {
			payloadPages--
		}
	}
	encoded := make([]byte, 0, headerBytes+len(chunk.Pages)*entryBytes+payloadPages*hostarch.PageSize)
	encoded = append(encoded, formatMagic[:]...)
	var number [8]byte
	binary.LittleEndian.PutUint32(number[0:4], formatVersion)
	binary.LittleEndian.PutUint32(number[4:8], uint32(hostarch.PageSize))
	encoded = append(encoded, number[:]...)
	encoded = append(encoded, chunk.Logical.Allocator[:]...)
	binary.LittleEndian.PutUint64(number[:], chunk.Logical.Sequence)
	encoded = append(encoded, number[:]...)
	encoded = append(encoded, valid[:]...)
	encoded = append(encoded, zero[:]...)
	for slot, page := range pages {
		if page == nil {
			continue
		}
		encoded = append(encoded, byte(slot))
		encoded = append(encoded, page.Version.Allocator[:]...)
		binary.LittleEndian.PutUint64(number[:], page.Version.Sequence)
		encoded = append(encoded, number[:]...)
	}
	for _, page := range pages {
		if page != nil && !page.Zero {
			encoded = append(encoded, page.Data...)
		}
	}
	return encoded, nil
}

// Decode validates capture-stream chunk data.
func Decode(data []byte) (Chunk, error) {
	chunk, err := decodeInPlace(data)
	if err != nil {
		return Chunk{}, err
	}
	for i := range chunk.Pages {
		if chunk.Pages[i].Data != nil {
			chunk.Pages[i].Data = append([]byte(nil), chunk.Pages[i].Data...)
		}
	}
	return chunk, nil
}

// decodeInPlace is Decode, except that the pages' data are slices of data.
func decodeInPlace(data []byte) (Chunk, error) {
	if len(data) < headerBytes || !bytes.Equal(data[:16], formatMagic[:]) {
		return Chunk{}, errors.New("invalid TTC memory chunk header")
	}
	if version := binary.LittleEndian.Uint32(data[16:20]); version != formatVersion {
		return Chunk{}, fmt.Errorf("unsupported TTC memory chunk version %d", version)
	}
	if pageSize := binary.LittleEndian.Uint32(data[20:24]); pageSize != uint32(hostarch.PageSize) {
		return Chunk{}, fmt.Errorf("memory chunk page size is %d, want %d", pageSize, hostarch.PageSize)
	}
	var result Chunk
	copy(result.Logical.Allocator[:], data[24:40])
	result.Logical.Sequence = binary.LittleEndian.Uint64(data[40:48])
	if result.Logical.Allocator == (pgalloc.LogicalAllocatorID{}) {
		return Chunk{}, errors.New("memory chunk has zero logical allocator ID")
	}
	var valid, zero [32]byte
	copy(valid[:], data[48:80])
	copy(zero[:], data[80:112])
	for i := range zero {
		if zero[i]&^valid[i] != 0 {
			return Chunk{}, errors.New("memory chunk marks an invalid slot as zero")
		}
	}
	validPages := 0
	zeroPages := 0
	for slot := 0; slot < PagesPerChunk; slot++ {
		if hasBit(&valid, uint8(slot)) {
			validPages++
		}
		if hasBit(&zero, uint8(slot)) {
			zeroPages++
		}
	}
	if validPages == 0 {
		return Chunk{}, errors.New("memory chunk has no valid pages")
	}
	metadataEnd := headerBytes + validPages*entryBytes
	payloadEnd := metadataEnd + (validPages-zeroPages)*hostarch.PageSize
	if payloadEnd != len(data) {
		return Chunk{}, fmt.Errorf("memory chunk has %d bytes, expected %d", len(data), payloadEnd)
	}
	result.Pages = make([]Page, 0, validPages)
	offset := headerBytes
	payloadOffset := metadataEnd
	for slot := 0; slot < PagesPerChunk; slot++ {
		if !hasBit(&valid, uint8(slot)) {
			continue
		}
		if gotSlot := data[offset]; gotSlot != byte(slot) {
			return Chunk{}, fmt.Errorf("memory chunk entry slot is %d, want %d", gotSlot, slot)
		}
		page := Page{Slot: uint8(slot), Zero: hasBit(&zero, uint8(slot))}
		copy(page.Version.Allocator[:], data[offset+1:offset+17])
		page.Version.Sequence = binary.LittleEndian.Uint64(data[offset+17 : offset+25])
		if page.Version.Allocator == (pgalloc.LogicalAllocatorID{}) {
			return Chunk{}, fmt.Errorf("memory chunk slot %d has zero version allocator ID", slot)
		}
		offset += entryBytes
		if !page.Zero {
			page.Data = data[payloadOffset : payloadOffset+hostarch.PageSize : payloadOffset+hostarch.PageSize]
			payloadOffset += hostarch.PageSize
		}
		result.Pages = append(result.Pages, page)
	}
	return result, nil
}

// Read decodes one bounded chunk stream.
func Read(reader io.Reader) (Chunk, error) {
	const maxEncodedBytes = headerBytes + PagesPerChunk*entryBytes + PagesPerChunk*hostarch.PageSize
	data, err := io.ReadAll(io.LimitReader(reader, maxEncodedBytes+1))
	if err != nil {
		return Chunk{}, err
	}
	if len(data) > maxEncodedBytes {
		return Chunk{}, errors.New("TTC memory chunk exceeds maximum encoded size")
	}
	return Decode(data)
}
