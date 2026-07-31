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

package ttcremote

import (
	"bytes"
	"reflect"
	"testing"

	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

func TestSessionMetadataRoundTripAndIntegrity(t *testing.T) {
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
