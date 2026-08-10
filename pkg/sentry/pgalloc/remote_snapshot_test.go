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
	stdcontext "context"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
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

func newEmptyRemoteTestMemoryFile(t *testing.T) *MemoryFile {
	t.Helper()
	file, err := os.CreateTemp("", "ttc-remote-target-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
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
	return mf
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

func TestRemoteSnapshotSiblingsShareSourceReferences(t *testing.T) {
	mf, fr := newRemoteTestMemoryFile(t, 2)
	snapshots, err := NewRemoteSnapshots([]RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}}, 2)
	if err != nil {
		t.Fatal(err)
	}
	firstPage := memmap.FileRange{0, hostarch.PageSize}
	absoluteFirstPage := memmap.FileRange{fr.Start, fr.Start + hostarch.PageSize}
	if err := snapshots[0].ReleaseRegion(1, firstPage); err != nil {
		t.Fatal(err)
	}
	if mf.HasUniqueRef(absoluteFirstPage) {
		t.Fatal("first sibling prematurely released the shared source page")
	}
	if err := snapshots[1].ReleaseRegion(1, firstPage); err != nil {
		t.Fatal(err)
	}
	if !mf.HasUniqueRef(absoluteFirstPage) {
		t.Fatal("last sibling acknowledgement did not release the shared page")
	}
	if err := snapshots[0].Close(); err != nil {
		t.Fatal(err)
	}
	if mf.HasUniqueRef(memmap.FileRange{fr.Start + hostarch.PageSize, fr.End}) {
		t.Fatal("closing one sibling prematurely released another sibling's page")
	}
	if err := snapshots[1].Close(); err != nil {
		t.Fatal(err)
	}
	if !mf.HasUniqueRef(fr) {
		t.Fatal("closing every sibling did not restore source uniqueness")
	}
}

func TestSyncAndDropSnapshotCacheSharedDehydratedPages(t *testing.T) {
	mf := newEmptyRemoteTestMemoryFile(t)
	fr, err := mf.Allocate(2*hostarch.PageSize, AllocOpts{
		Kind: usage.Anonymous,
		Mode: AllocateAndWritePopulate,
	})
	if err != nil {
		mf.Destroy()
		t.Fatal(err)
	}
	want := bytes.Repeat([]byte("dehydrated"), int(fr.Length())/len("dehydrated")+1)[:fr.Length()]
	fillRemoteTestRange(t, mf, fr, want)
	first, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		mf.DecRef(fr)
		mf.Destroy()
		t.Fatal(err)
	}
	second, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: mf, Range: fr}})
	if err != nil {
		first.Close()
		mf.DecRef(fr)
		mf.Destroy()
		t.Fatal(err)
	}
	// Model the last live Kernel releasing a page that remains shared by two
	// dehydrated branches. Collective eviction must account for both snapshot
	// references and leave their bytes reloadable from disk.
	mf.DecRef(fr)
	if err := SyncAndDropSnapshotCache([]*RemoteSnapshot{first, second}); err != nil {
		t.Fatalf("SyncAndDropSnapshotCache: %v", err)
	}
	got := make([]byte, len(want))
	if n, err := first.CopyStreamAt(got, 0); err != nil || n != len(got) {
		t.Fatalf("CopyStreamAt after eviction: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("reloaded dehydrated bytes differ")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	mf.Destroy()
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
	if got := target.AsyncLoadingMapUnit(); got != 0 {
		t.Fatalf("background-load map unit=%d, want default cap", got)
	}

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

func TestDemandOnlyRemotePageLoad(t *testing.T) {
	source, sourceFR := newRemoteTestMemoryFile(t, 8)
	target, targetFR := newRemoteTestMemoryFile(t, 8)
	want := make([]byte, sourceFR.Length())
	for i := range want {
		want[i] = byte(i*13 + 11)
	}
	fillRemoteTestRange(t, source, sourceFR, want)

	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: source, Range: sourceFR}})
	if err != nil {
		t.Fatal(err)
	}
	reader := NewLoopbackRemoteReader(snapshot, hostarch.PageSize, 1, 2)
	pages, err := StartAsyncPagesFileLoadWithOpts(reader, nil, nil, AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AttachRemotePageLoad(pages, []RemoteLoadRange{{Target: targetFR, StreamOffset: 0}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	if got := target.AsyncLoadingMapUnit(); got != hostarch.PageSize {
		t.Fatalf("demand-only map unit=%d, want one page", got)
	}

	// MemoryFilesDone must not turn a demand-only load into an eager drain.
	time.Sleep(25 * time.Millisecond)
	if stats := snapshot.Stats(); stats.PagesOutstanding != 8 {
		t.Fatalf("demand-only loader fetched untouched pages: %+v", stats)
	}
	if stats := pages.Stats(); stats.Done || !stats.DemandOnly || stats.BytesOutstanding != sourceFR.Length() || stats.BytesLoaded != 0 {
		t.Fatalf("initial demand-only loader stats: %+v", stats)
	}

	page := uint64(3)
	pageFR := memmap.FileRange{targetFR.Start + page*hostarch.PageSize, targetFR.Start + (page+1)*hostarch.PageSize}
	loaded, err := target.MapInternal(pageFR, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, hostarch.PageSize)
	if n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), loaded); err != nil || n != hostarch.PageSize {
		t.Fatalf("copy demanded page: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want[page*hostarch.PageSize:(page+1)*hostarch.PageSize]) {
		t.Fatal("demand-loaded page differs from source")
	}
	if stats := snapshot.Stats(); stats.PagesOutstanding != 7 || stats.PagesReleased != 1 {
		t.Fatalf("demand load did not release exactly one page: %+v", stats)
	}
	if stats := pages.Stats(); stats.Done || stats.BytesOutstanding != sourceFR.Length()-hostarch.PageSize || stats.BytesLoaded != hostarch.PageSize {
		t.Fatalf("post-demand loader stats: %+v", stats)
	}

	pages.Drain()
	if err := pages.Wait(); err != nil {
		t.Fatal(err)
	}
	if stats := snapshot.Stats(); !stats.Closed || stats.PagesOutstanding != 0 {
		t.Fatalf("drained snapshot: %+v", stats)
	}
	if stats := pages.Stats(); !stats.Done || stats.BytesOutstanding != 0 || stats.BytesLoaded != sourceFR.Length() || stats.Error != "" {
		t.Fatalf("drained loader stats: %+v", stats)
	}
	if !source.HasUniqueRef(sourceFR) {
		t.Fatal("drain did not release all source references")
	}
}

func TestRemoteSnapshotOutstandingStreamRanges(t *testing.T) {
	source, sourceFR := newRemoteTestMemoryFile(t, 6)
	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: source, Range: sourceFR}})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if err := snapshot.AcknowledgeStreamRange(hostarch.PageSize, hostarch.PageSize); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.AcknowledgeStreamRange(3*hostarch.PageSize, hostarch.PageSize); err != nil {
		t.Fatal(err)
	}
	ranges, err := snapshot.OutstandingStreamRanges(2 * hostarch.PageSize)
	if err != nil {
		t.Fatal(err)
	}
	want := []RemotePushHint{
		{StreamOffset: 0, Length: hostarch.PageSize},
		{StreamOffset: 2 * hostarch.PageSize, Length: hostarch.PageSize},
		{StreamOffset: 4 * hostarch.PageSize, Length: 2 * hostarch.PageSize},
	}
	if len(ranges) != len(want) {
		t.Fatalf("outstanding ranges=%+v, want %+v", ranges, want)
	}
	for i := range want {
		if ranges[i] != want[i] {
			t.Fatalf("outstanding range %d=%+v, want %+v", i, ranges[i], want[i])
		}
	}
}

func TestDemandOnlyRemotePageLoadForwardPrefetch(t *testing.T) {
	source, sourceFR := newRemoteTestMemoryFile(t, 8)
	target, targetFR := newRemoteTestMemoryFile(t, 8)
	want := make([]byte, sourceFR.Length())
	for i := range want {
		want[i] = byte(i*17 + 3)
	}
	fillRemoteTestRange(t, source, sourceFR, want)

	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: source, Range: sourceFR}})
	if err != nil {
		t.Fatal(err)
	}
	reader := NewLoopbackRemoteReader(snapshot, 4*hostarch.PageSize, 1, 2)
	pages, err := StartAsyncPagesFileLoadWithOpts(reader, nil, nil, AsyncPagesFileLoadOpts{
		DemandOnly: true, PrefetchBytes: 3 * hostarch.PageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AttachRemotePageLoad(pages, []RemoteLoadRange{{Target: targetFR, StreamOffset: 0}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	if got := target.AsyncLoadingMapUnit(); got != 4*hostarch.PageSize {
		t.Fatalf("forward-prefetch map unit=%d, want four pages", got)
	}

	page := uint64(2)
	pageFR := memmap.FileRange{targetFR.Start + page*hostarch.PageSize, targetFR.Start + (page+1)*hostarch.PageSize}
	if _, err := target.MapInternal(pageFR, hostarch.Read); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for snapshot.Stats().PagesReleased != 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stats := snapshot.Stats(); stats.PagesReleased != 4 || stats.PagesOutstanding != 4 {
		t.Fatalf("forward prefetch loaded wrong range: %+v", stats)
	}
	prefetchedFR := memmap.FileRange{pageFR.End, pageFR.End + 3*hostarch.PageSize}
	mapped, err := target.MapInternal(prefetchedFR, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, prefetchedFR.Length())
	if n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), mapped); err != nil || n != prefetchedFR.Length() {
		t.Fatalf("copy prefetched pages: n=%d err=%v", n, err)
	}
	wantStart := (page + 1) * hostarch.PageSize
	if !bytes.Equal(got, want[wantStart:wantStart+uint64(len(got))]) {
		t.Fatal("prefetched pages differ from source")
	}
	// Accessing the already-prefetched pages must not request more data.
	time.Sleep(25 * time.Millisecond)
	if stats := snapshot.Stats(); stats.PagesReleased != 4 {
		t.Fatalf("prefetched access caused another pull: %+v", stats)
	}
	pages.Cancel()
	if err := pages.Wait(); err == nil {
		t.Fatal("canceled loader unexpectedly completed without cancellation")
	}
}

func TestDemandOnlyRemotePageLoadFixedWindowAndPolicySwitch(t *testing.T) {
	source, sourceFR := newRemoteTestMemoryFile(t, 8)
	target, targetFR := newRemoteTestMemoryFile(t, 8)
	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: source, Range: sourceFR}})
	if err != nil {
		t.Fatal(err)
	}
	reader := NewLoopbackRemoteReader(snapshot, 4*hostarch.PageSize, 1, 3)
	pages, err := StartAsyncPagesFileLoadWithOpts(reader, nil, nil, AsyncPagesFileLoadOpts{
		DemandOnly: true, PrefetchBehindBytes: hostarch.PageSize, PrefetchBytes: 2 * hostarch.PageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AttachRemotePageLoad(pages, []RemoteLoadRange{{Target: targetFR, StreamOffset: 0}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	if got := target.AsyncLoadingMapUnit(); got != 4*hostarch.PageSize {
		t.Fatalf("fixed-window map unit=%d, want four pages", got)
	}
	fault := func(page uint64) {
		t.Helper()
		fr := memmap.FileRange{targetFR.Start + page*hostarch.PageSize, targetFR.Start + (page+1)*hostarch.PageSize}
		if _, err := target.MapInternal(fr, hostarch.Read); err != nil {
			t.Fatal(err)
		}
	}
	fault(3)
	deadline := time.Now().Add(5 * time.Second)
	for snapshot.Stats().PagesReleased != 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stats := snapshot.Stats(); stats.PagesReleased != 4 {
		t.Fatalf("fixed prefetch window did not load four pages: %+v", stats)
	}
	if err := pages.SetPrefetchWindow(0, 0); err != nil {
		t.Fatal(err)
	}
	if got := target.AsyncLoadingMapUnit(); got != hostarch.PageSize {
		t.Fatalf("disabled-prefetch map unit=%d, want one page", got)
	}
	fault(7)
	if stats := snapshot.Stats(); stats.PagesReleased != 5 {
		t.Fatalf("disabled prefetch loaded more than demanded page: %+v", stats)
	}
	pages.Cancel()
	_ = pages.Wait()
}

func TestCancelDemandOnlyRemotePageLoad(t *testing.T) {
	source, sourceFR := newRemoteTestMemoryFile(t, 4)
	target, targetFR := newRemoteTestMemoryFile(t, 4)
	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: source, Range: sourceFR}})
	if err != nil {
		t.Fatal(err)
	}
	reader := NewLoopbackRemoteReader(snapshot, hostarch.PageSize, 1, 1)
	pages, err := StartAsyncPagesFileLoadWithOpts(reader, nil, nil, AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AttachRemotePageLoad(pages, []RemoteLoadRange{{Target: targetFR, StreamOffset: 0}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	pages.Cancel()
	if err := pages.Wait(); err != stdcontext.Canceled {
		t.Fatalf("Wait after Cancel: got %v, want context.Canceled", err)
	}
	if stats := snapshot.Stats(); !stats.Closed || stats.PagesOutstanding != 0 {
		t.Fatalf("canceled snapshot: %+v", stats)
	}
	if !source.HasUniqueRef(sourceFR) {
		t.Fatal("cancel did not release source references")
	}
}

func TestTryInstallRemotePagesDistinguishesResolvedAndMixedRanges(t *testing.T) {
	source, sourceFR := newRemoteTestMemoryFile(t, 2)
	target, targetFR := newRemoteTestMemoryFile(t, 2)
	want := bytes.Repeat([]byte{0x4a}, int(sourceFR.Length()))
	fillRemoteTestRange(t, source, sourceFR, want)
	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: source, Range: sourceFR}})
	if err != nil {
		t.Fatal(err)
	}
	reader := NewLoopbackRemoteReader(snapshot, hostarch.PageSize, 1, 1)
	pages, err := StartAsyncPagesFileLoadWithOpts(reader, nil, nil, AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		pages.Cancel()
		_ = pages.Wait()
	}()
	if err := target.AttachRemotePageLoad(pages, []RemoteLoadRange{{Target: targetFR, StreamOffset: 0}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	loaded, err := target.MapInternal(memmap.FileRange{targetFR.Start, targetFR.Start + hostarch.PageSize}, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, hostarch.PageSize)
	if n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), loaded); err != nil || n != hostarch.PageSize {
		t.Fatalf("faulting first page: n=%d err=%v", n, err)
	}
	if result, err := pages.TryInstallRemotePagesState(0, want); err != nil || result != RemotePageInstallBusy {
		t.Fatalf("mixed range result=%v err=%v, want busy", result, err)
	}
	if result, err := pages.TryInstallRemotePagesState(0, want[:hostarch.PageSize]); err != nil || result != RemotePageInstallResolved {
		t.Fatalf("loaded range result=%v err=%v, want resolved", result, err)
	}
	if result, err := pages.TryInstallRemotePagesState(hostarch.PageSize, want[hostarch.PageSize:]); err != nil || result != RemotePageInstallInstalled {
		t.Fatalf("pending range result=%v err=%v, want installed", result, err)
	}
}

func TestLoopbackProactivePushWinsBeforePull(t *testing.T) {
	source, sourceFR := newRemoteTestMemoryFile(t, 2)
	target, targetFR := newRemoteTestMemoryFile(t, 2)
	want := bytes.Repeat([]byte{0x6d}, int(sourceFR.Length()))
	fillRemoteTestRange(t, source, sourceFR, want)
	snapshot, err := NewRemoteSnapshot([]RemoteSnapshotRegion{{ID: 1, File: source, Range: sourceFR}})
	if err != nil {
		t.Fatal(err)
	}
	reader := NewLoopbackRemoteReader(snapshot, hostarch.PageSize, 1, 1)
	pages, err := StartAsyncPagesFileLoadWithOpts(reader, nil, nil, AsyncPagesFileLoadOpts{DemandOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AttachRemotePageLoad(pages, []RemoteLoadRange{{Target: targetFR, StreamOffset: 0}}, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	done := StartLoopbackRemotePush(snapshot, pages, hostarch.PageSize)
	source.NotifyRemoteCOW(memmap.FileRange{sourceFR.Start, sourceFR.Start + hostarch.PageSize})
	deadline := time.Now().Add(5 * time.Second)
	for snapshot.Stats().PagesReleased != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stats := snapshot.Stats(); stats.PagesReleased != 1 || stats.PagesOutstanding != 1 {
		t.Fatalf("push did not install and acknowledge one page: %+v", stats)
	}
	loaded, err := target.MapInternal(memmap.FileRange{targetFR.Start, targetFR.Start + hostarch.PageSize}, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, hostarch.PageSize)
	if n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), loaded); err != nil || n != hostarch.PageSize {
		t.Fatalf("copy pushed page: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want[:hostarch.PageSize]) {
		t.Fatal("proactively pushed bytes differ")
	}
	pages.Cancel()
	_ = pages.Wait()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("push pump did not stop with snapshot")
	}
}

func TestCloneMemoryFileMetadataForRemote(t *testing.T) {
	source, sourceFR := newRemoteTestMemoryFile(t, 4)
	want := make([]byte, sourceFR.Length())
	for i := range want {
		want[i] = byte(i*17 + 3)
	}
	fillRemoteTestRange(t, source, sourceFR, want)
	target := newEmptyRemoteTestMemoryFile(t)
	targetHasRefs := false
	t.Cleanup(func() {
		if targetHasRefs {
			target.DecRef(sourceFR)
		}
		target.Destroy()
	})

	regions, loads, err := CloneMemoryFileMetadataForRemote(context.Background(), source, target)
	if err != nil {
		t.Fatal(err)
	}
	targetHasRefs = true
	if len(regions) != 1 || regions[0].Range != sourceFR {
		t.Fatalf("snapshot regions: %+v, want one region %v", regions, sourceFR)
	}
	if len(loads) != 1 || loads[0].Target != sourceFR || loads[0].StreamOffset != 0 {
		t.Fatalf("target load ranges: %+v", loads)
	}
	if !target.HasUniqueRef(sourceFR) {
		t.Fatal("remote allocator metadata did not preserve source refcounts")
	}

	snapshot, err := NewRemoteSnapshot(regions)
	if err != nil {
		t.Fatal(err)
	}
	reader := NewLoopbackRemoteReader(snapshot, 2*hostarch.PageSize, 4, 2)
	loaderDone := make(chan error, 1)
	pages, err := StartAsyncPagesFileLoad(reader, func(err error) { loaderDone <- err }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.AttachRemotePageLoad(pages, loads, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	loaded, err := target.MapInternal(sourceFR, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if n, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(got)), loaded); err != nil || n != sourceFR.Length() {
		t.Fatalf("copy from metadata clone: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("metadata-cloned MemoryFile loaded incorrect contents")
	}
	select {
	case err := <-loaderDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote metadata clone load timed out")
	}
	if !source.HasUniqueRef(sourceFR) {
		t.Fatal("source did not reclaim snapshot references after clone load")
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
