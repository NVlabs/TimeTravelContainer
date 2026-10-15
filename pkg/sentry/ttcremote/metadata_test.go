// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcremote

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"reflect"
	"testing"

	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

func TestSessionMetadataRoundTripAndIntegrity(t *testing.T) {
	var allocator pgalloc.LogicalAllocatorID
	allocator[0] = 1
	var versionAllocator pgalloc.LogicalAllocatorID
	versionAllocator[0] = 2
	page := pgalloc.LogicalPageID{Allocator: allocator, Sequence: 7}
	chunk, slot := page.Chunk()
	hugePage := pgalloc.LogicalPageID{Allocator: allocator, Sequence: 512}
	hugeChunk, hugeSlot := hugePage.Chunk()
	want := &SessionMetadata{
		FamilyID:          "family-1",
		SourceBranchID:    "main",
		TargetBranchID:    "worker-b",
		KernelMetadata:    bytes.Repeat([]byte{0x31}, 70<<10),
		AllocatorMetadata: bytes.Repeat([]byte{0xa7}, 19<<10),
		LoadRanges: []pgalloc.RemoteLoadRange{
			{Target: memmap.FileRange{Start: 0x1000, End: 0x9000}, StreamOffset: 0},
			{Target: memmap.FileRange{Start: 0x20000, End: 0x24000}, StreamOffset: 0x8000},
		},
		MemoryBindings: []pgalloc.CapturedPageBinding{{
			OldOffset: 0x2000, StreamOffset: 0x1000, References: 2, Page: page,
			Version: pgalloc.PageVersionID{Allocator: versionAllocator, Sequence: 9}, Chunk: chunk, Slot: slot,
		}, {
			OldOffset: 0x400000, StreamOffset: 0x2000, References: 1, Page: hugePage,
			Version: pgalloc.PageVersionID{Allocator: versionAllocator, Sequence: 1024}, Chunk: hugeChunk, Slot: hugeSlot, Huge: true,
		}},
	}
	encoded, err := want.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	limits := SessionMetadataLimits{MaxTotalBytes: 1 << 20, MaxKernelBytes: 128 << 10, MaxAllocatorBytes: 64 << 10, MaxLoadRanges: 10}
	got, err := UnmarshalSessionMetadata(encoded, limits)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip mismatch: got %+v want %+v", got, want)
	}
	encoded[len(encoded)/2] ^= 0x80
	if _, err := UnmarshalSessionMetadata(encoded, limits); err == nil {
		t.Fatal("corrupt metadata passed integrity validation")
	}
}

func TestSessionMetadataDecodeLimits(t *testing.T) {
	metadata := &SessionMetadata{KernelMetadata: make([]byte, 4096), AllocatorMetadata: make([]byte, 4096)}
	encoded, err := metadata.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnmarshalSessionMetadata(encoded, SessionMetadataLimits{
		MaxTotalBytes: uint64(len(encoded)), MaxKernelBytes: 1024,
		MaxAllocatorBytes: 4096, MaxLoadRanges: 1,
	}); err == nil {
		t.Fatal("oversized Kernel metadata passed decode limits")
	}
}

// A family of more than 4 GiB has more than 1<<20 pages, each bound in its
// envelope: limits derived from the envelope's size admit it, and still
// refuse counts it has no room for.
func TestSessionMetadataLimitsForLargeEnvelopes(t *testing.T) {
	const ranges = 1<<20 + 1
	want := &SessionMetadata{FamilyID: "family-1", SourceBranchID: "main", TargetBranchID: "large"}
	want.LoadRanges = make([]pgalloc.RemoteLoadRange, ranges)
	for i := range want.LoadRanges {
		start := uint64(i) * 2 * 4096
		want.LoadRanges[i] = pgalloc.RemoteLoadRange{Target: memmap.FileRange{Start: start, End: start + 4096}, StreamOffset: uint64(i) * 4096}
	}
	encoded, err := want.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	got, err := UnmarshalSessionMetadata(encoded, SessionMetadataLimitsFor(uint64(len(encoded))))
	if err != nil {
		t.Fatalf("decoding %d load ranges: %v", ranges, err)
	}
	if len(got.LoadRanges) != ranges || got.LoadRanges[ranges-1] != want.LoadRanges[ranges-1] {
		t.Fatalf("decoded %d load ranges, last %+v; want %d, last %+v", len(got.LoadRanges), got.LoadRanges[len(got.LoadRanges)-1], ranges, want.LoadRanges[ranges-1])
	}
	binary.BigEndian.PutUint32(encoded[28:32], uint32(len(encoded)/sessionLoadRangeBytes+1))
	if _, err := UnmarshalSessionMetadata(encoded, SessionMetadataLimitsFor(uint64(len(encoded)))); err == nil {
		t.Fatal("an envelope claiming more bindings than it has room for was decoded")
	}
}

// A binding of a 2 MiB page must name one: at an aligned offset, with
// identities aligned as a 2 MiB page's are. Flags it does not know are
// refused.
func TestSessionMetadataRefusesInvalidHugeBindings(t *testing.T) {
	var allocator pgalloc.LogicalAllocatorID
	allocator[0] = 1
	valid := pgalloc.CapturedPageBinding{
		OldOffset: 2 << 20, References: 1, Huge: true,
		Page:    pgalloc.LogicalPageID{Allocator: allocator, Sequence: 512},
		Version: pgalloc.PageVersionID{Allocator: allocator, Sequence: 1024},
	}
	limits := SessionMetadataLimits{MaxTotalBytes: 1 << 20, MaxKernelBytes: 1 << 10, MaxAllocatorBytes: 1 << 10, MaxLoadRanges: 10}
	for _, test := range []struct {
		name   string
		mutate func(*pgalloc.CapturedPageBinding)
		ok     bool
	}{
		{"valid", func(*pgalloc.CapturedPageBinding) {}, true},
		{"unaligned offset", func(b *pgalloc.CapturedPageBinding) { b.OldOffset += 4096 }, false},
		{"unaligned page", func(b *pgalloc.CapturedPageBinding) { b.Page.Sequence++ }, false},
		{"unaligned version", func(b *pgalloc.CapturedPageBinding) { b.Version.Sequence += 256 }, false},
	} {
		binding := valid
		test.mutate(&binding)
		metadata := &SessionMetadata{MemoryBindings: []pgalloc.CapturedPageBinding{binding}}
		encoded, err := metadata.MarshalBinary()
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if _, err := UnmarshalSessionMetadata(encoded, limits); (err == nil) != test.ok {
			t.Errorf("%s: decoding returned %v, want ok %t", test.name, err, test.ok)
		}
	}
	metadata := &SessionMetadata{MemoryBindings: []pgalloc.CapturedPageBinding{valid}}
	encoded, err := metadata.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	// The flags are the last 8 bytes of the binding, before the digest.
	flags := len(encoded) - sessionMetadataDigest - 8
	encoded[flags] |= 0x80
	digest := sha256.Sum256(encoded[:len(encoded)-sessionMetadataDigest])
	copy(encoded[len(encoded)-sessionMetadataDigest:], digest[:])
	if _, err := UnmarshalSessionMetadata(encoded, limits); err == nil {
		t.Error("a binding with unknown flags was decoded")
	}
}
