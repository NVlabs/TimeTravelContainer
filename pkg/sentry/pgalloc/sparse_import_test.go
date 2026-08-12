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

//go:build !pagesize_64k

package pgalloc

import (
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

func sparseTestInstall(objectByte byte) SparseChunkInstall {
	var object SparseChunkObjectID
	object[0] = objectByte
	var allocator LogicalAllocatorID
	allocator[0] = 1
	logical := LogicalChunkID{Allocator: allocator, Sequence: 7}
	return SparseChunkInstall{
		Object: object, Logical: logical, Kind: usage.Anonymous,
		Pages: []SparseChunkPage{
			{Slot: 3, Page: LogicalPageID{Allocator: allocator, Sequence: 7*logicalPagesPerChunk + 3}, Version: PageVersionID{Allocator: allocator, Sequence: 30}, References: 2},
			{Slot: 4, Page: LogicalPageID{Allocator: allocator, Sequence: 7*logicalPagesPerChunk + 4}, Version: PageVersionID{Allocator: allocator, Sequence: 31}, References: 1},
		},
	}
}

func TestInstallSparseChunksReusesResidentObject(t *testing.T) {
	mf := newEmptyRemoteTestMemoryFile(t)
	defer mf.Destroy()
	install := sparseTestInstall(1)
	firstInstall, err := mf.InstallSparseChunks([]SparseChunkInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if err := firstInstall.Commit(); err != nil {
		t.Fatal(err)
	}
	first := firstInstall.NewRanges()[install.Object]
	if first.Length() != SparseChunkBytes {
		t.Fatalf("resident chunk range is %v", first)
	}
	if got, ok := mf.SparseChunkRange(install.Object); !ok || got != first {
		t.Fatalf("SparseChunkRange=(%v,%t), want %v", got, ok, first)
	}
	for _, page := range install.Pages {
		offset := first.Start + uint64(page.Slot)*hostarch.PageSize
		gotPage, gotVersion, ok := mf.LogicalPageVersionAt(offset)
		if !ok || gotPage != page.Page || gotVersion != page.Version {
			t.Fatalf("slot %d identity=(%+v,%+v,%t), want (%+v,%+v,true)", page.Slot, gotPage, gotVersion, ok, page.Page, page.Version)
		}
	}
	reusedInstall, err := mf.InstallSparseChunks([]SparseChunkInstall{install})
	if err != nil {
		t.Fatal(err)
	}
	if err := reusedInstall.Commit(); err != nil {
		t.Fatal(err)
	}
	if len(reusedInstall.NewRanges()) != 0 {
		t.Fatalf("reused import allocated ranges: %v", reusedInstall.NewRanges())
	}
	if got, _ := mf.SparseChunkRange(install.Object); got != first {
		t.Fatalf("reused object moved from %v to %v", first, got)
	}
}

func TestSparseRelocationSplitsDiscontiguousDestination(t *testing.T) {
	resource := checkpoint.ResourceID{Path: "/tmpfs"}
	relocation := NewSparseRelocation()
	if err := relocation.Add(resource, 0, 10*hostarch.PageSize); err != nil {
		t.Fatal(err)
	}
	if err := relocation.Add(resource, hostarch.PageSize, 20*hostarch.PageSize); err != nil {
		t.Fatal(err)
	}
	source := memmap.FileRange{Start: 0, End: 2 * hostarch.PageSize}
	runs, err := relocation.Runs(resource, source)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("Runs=%v, want two ranges", runs)
	}
	if _, err := relocation.Range(resource, source); err == nil {
		t.Fatal("Range accepted a discontiguous relocation")
	}
}

func TestSparseReferenceTransactionCommitAndAbort(t *testing.T) {
	mf := newEmptyRemoteTestMemoryFile(t)
	defer mf.Destroy()
	installation, err := mf.InstallSparseChunks([]SparseChunkInstall{sparseTestInstall(1)})
	if err != nil {
		t.Fatal(err)
	}
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}
	chunk := installation.NewRanges()[sparseTestInstall(1).Object]
	page := memmap.FileRange{Start: chunk.Start + 3*hostarch.PageSize, End: chunk.Start + 4*hostarch.PageSize}
	if !mf.HasUniqueRef(page) {
		t.Fatal("new sparse chunk does not have exactly its cache reference")
	}

	aborted := NewSparseReferenceTransaction()
	aborted.Add(mf, page, 0)
	aborted.Abort()
	if !mf.HasUniqueRef(page) {
		t.Fatal("aborted reference transaction changed allocator references")
	}

	committed := NewSparseReferenceTransaction()
	committed.Add(mf, page, 0)
	if err := committed.Commit(); err != nil {
		t.Fatal(err)
	}
	if mf.HasUniqueRef(page) {
		t.Fatal("committed reference transaction did not add the graph reference")
	}
	mf.DecRef(page)
	if !mf.HasUniqueRef(page) {
		t.Fatal("released graph reference did not leave the cache reference")
	}
}
