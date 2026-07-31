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
	"bytes"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

func newRemoteTestMemoryFile(t *testing.T, pages uint64) (*MemoryFile, memmap.FileRange) {
	t.Helper()
	file, err := os.CreateTemp("", "ttc-remote-memory-")
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	if err := os.Remove(name); err != nil {
		file.Close()
		t.Fatal(err)
	}
	mf, err := NewMemoryFile(file, MemoryFileOpts{
		DelayedEviction:         DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	fr, err := mf.Allocate(pages*hostarch.PageSize, AllocOpts{
		Kind: usage.Anonymous,
		Mode: AllocateAndWritePopulate,
	})
	if err != nil {
		mf.Destroy()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		mf.DecRef(fr)
		mf.Destroy()
	})
	return mf, fr
}

func fillRemoteTestRange(t *testing.T, mf *MemoryFile, fr memmap.FileRange, data []byte) {
	t.Helper()
	if uint64(len(data)) != fr.Length() {
		t.Fatalf("data length %d, range length %d", len(data), fr.Length())
	}
	dst, err := mf.MapInternal(fr, hostarch.Write)
	if err != nil {
		t.Fatal(err)
	}
	src := safemem.BlockSeqOf(safemem.BlockFromSafeSlice(data))
	if n, err := safemem.CopySeq(dst, src); err != nil || n != fr.Length() {
		t.Fatalf("copy to MemoryFile: n=%d err=%v", n, err)
	}
}

func TestRemoteSnapshotPageRelease(t *testing.T) {
	mf, fr := newRemoteTestMemoryFile(t, 4)
	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 10, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}

	firstPage := memmap.FileRange{0, hostarch.PageSize}
	if mf.HasUniqueRef(memmap.FileRange{fr.Start, fr.Start + hostarch.PageSize}) {
		t.Fatal("source page remained unique after snapshot creation")
	}
	lease, err := snapshot.AcquireRegion(10, firstPage)
	if err != nil {
		t.Fatal(err)
	}
	if err := snapshot.ReleaseRegion(10, firstPage); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.ReleaseRegion(10, firstPage); err != nil {
		t.Fatalf("duplicate release: %v", err)
	}
	if mf.HasUniqueRef(memmap.FileRange{fr.Start, fr.Start + hostarch.PageSize}) {
		t.Fatal("active sender lease did not pin the released page")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if !mf.HasUniqueRef(memmap.FileRange{fr.Start, fr.Start + hostarch.PageSize}) {
		t.Fatal("acknowledged page did not become source-unique")
	}
	stats := snapshot.Stats()
	if stats.PagesTotal != 4 || stats.PagesReleased != 1 || stats.PagesOutstanding != 3 || stats.ActiveLeases != 0 {
		t.Fatalf("snapshot stats: %+v", stats)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatalf("duplicate close: %v", err)
	}
	if !mf.HasUniqueRef(fr) {
		t.Fatal("canceling remaining snapshot pages did not restore uniqueness")
	}
}

func TestLoopbackRemoteReader(t *testing.T) {
	mf, fr := newRemoteTestMemoryFile(t, 4)
	want := make([]byte, fr.Length())
	for i := range want {
		want[i] = byte((i / int(hostarch.PageSize)) + 1)
	}
	fillRemoteTestRange(t, mf, fr, want)
	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	reader := NewLoopbackRemoteReader(snapshot, 2*hostarch.PageSize, 2, 2)
	dst := make([]byte, len(want))
	reader.AddRead(0, 0, nil, memmap.FileRange{}, dst[:2*hostarch.PageSize])
	reader.AddRead(1, int64(2*hostarch.PageSize), nil, memmap.FileRange{}, dst[2*hostarch.PageSize:])
	completions, err := reader.Wait(nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, completion := range completions {
		if completion.Err != nil || completion.N != 2*hostarch.PageSize {
			t.Fatalf("completion %+v", completion)
		}
	}
	if !bytes.Equal(dst, want) {
		t.Fatal("loopback reader copied incorrect page contents")
	}
	stats := snapshot.Stats()
	if stats.PagesOutstanding != 0 || stats.PagesReleased != 4 {
		t.Fatalf("snapshot not fully acknowledged: %+v", stats)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !mf.HasUniqueRef(fr) {
		t.Fatal("source pages are not unique after loopback copy")
	}
}

func TestLoopbackRemoteReaderReadv(t *testing.T) {
	mf, fr := newRemoteTestMemoryFile(t, 2)
	want := bytes.Repeat([]byte{0x5a}, int(fr.Length()))
	fillRemoteTestRange(t, mf, fr, want)
	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	reader := NewLoopbackRemoteReader(snapshot, fr.Length(), 2, 1)
	first := make([]byte, hostarch.PageSize)
	second := make([]byte, hostarch.PageSize)
	iovecs := []unix.Iovec{
		{Base: &first[0], Len: uint64(len(first))},
		{Base: &second[0], Len: uint64(len(second))},
	}
	reader.AddReadv(0, 0, fr.Length(), nil, nil, iovecs)
	completions, err := reader.Wait(nil, 1)
	if err != nil || len(completions) != 1 || completions[0].Err != nil {
		t.Fatalf("Wait: completions=%+v err=%v", completions, err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(first, second...), want) {
		t.Fatal("loopback readv copied incorrect page contents")
	}
}

func TestAttachRemotePageLoad(t *testing.T) {
	source, sourceFR := newRemoteTestMemoryFile(t, 4)
	target, targetFR := newRemoteTestMemoryFile(t, 4)
	want := make([]byte, sourceFR.Length())
	for i := range want {
		want[i] = byte(i*31 + 7)
	}
	fillRemoteTestRange(t, source, sourceFR, want)

	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: source, Range: sourceFR}})
	if err != nil {
		t.Fatal(err)
	}
	reader := NewLoopbackRemoteReader(snapshot, 2*hostarch.PageSize, 4, 2)
	loaderDone := make(chan error, 1)
	pages, err := StartAsyncPagesFileLoad(reader, func(err error) {
		loaderDone <- err
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	memoryFileDone := make(chan error, 1)
	if err := target.AttachRemotePageLoad(pages, []RemoteLoadRange{{Target: targetFR, StreamOffset: 0}}, func(err error) {
		memoryFileDone <- err
	}, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()

	if !target.IsAsyncLoading() {
		t.Fatal("target did not enter asynchronous loading state")
	}
	loaded, err := target.MapInternal(targetFR, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), loaded); err != nil || n != targetFR.Length() {
		t.Fatalf("copy from target: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("asynchronously loaded target data differs from source snapshot")
	}
	select {
	case err := <-memoryFileDone:
		if err != nil {
			t.Fatalf("MemoryFile load callback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("MemoryFile load callback timed out")
	}
	select {
	case err := <-loaderDone:
		if err != nil {
			t.Fatalf("pages loader callback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pages loader callback timed out")
	}
	if target.IsAsyncLoading() {
		t.Fatal("target remained in asynchronous loading state")
	}
	if stats := snapshot.Stats(); !stats.Closed || stats.PagesOutstanding != 0 {
		t.Fatalf("source snapshot after load: %+v", stats)
	}
	if !source.HasUniqueRef(sourceFR) {
		t.Fatal("source pages did not become unique after target installation")
	}
}

func TestRemoteSnapshotCloseWaitsForLease(t *testing.T) {
	mf, fr := newRemoteTestMemoryFile(t, 1)
	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := snapshot.AcquireRegion(1, memmap.FileRange{0, hostarch.PageSize})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = snapshot.Close()
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !snapshot.Stats().Closed && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !snapshot.Stats().Closed {
		t.Fatal("Close did not start")
	}
	select {
	case <-done:
		t.Fatal("Close returned while a sender lease remained active")
	default:
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after sender lease closed")
	}
}

func TestRemoteSnapshotAcquireReleaseRace(t *testing.T) {
	mf, fr := newRemoteTestMemoryFile(t, 1)
	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	relative := memmap.FileRange{0, hostarch.PageSize}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if lease, err := snapshot.AcquireRegion(1, relative); err == nil {
				lease.Close()
			}
		}()
	}
	close(start)
	if err := snapshot.ReleaseRegion(1, relative); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if !mf.HasUniqueRef(fr) {
		t.Fatal("source page did not become unique after acquire/release race")
	}
}

func TestNewRemoteSnapshotRejectsOverlap(t *testing.T) {
	mf, fr := newRemoteTestMemoryFile(t, 2)
	_, err := NewRemoteSnapshot([]RemoteSnapshotRegion{
		{ID: 1, File: mf, Range: fr},
		{ID: 2, File: mf, Range: memmap.FileRange{fr.Start + hostarch.PageSize, fr.End}},
	})
	if err == nil {
		t.Fatal("NewRemoteSnapshot accepted overlapping regions")
	}
}
