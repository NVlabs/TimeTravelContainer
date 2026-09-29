// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ttcremote

import (
	"bytes"
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
