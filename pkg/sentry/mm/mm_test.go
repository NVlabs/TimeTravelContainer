// Copyright 2018 The gVisor Authors.
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

package mm

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/usermem"
)

func testMemoryManagerWithMmapDirection(ctx context.Context, t testing.TB, mmapDirection arch.MmapDirection) *MemoryManager {
	p := platform.FromContext(ctx)
	mm, err := NewMemoryManager(p, pgalloc.MemoryFileFromContext(ctx))
	if err != nil {
		t.Fatalf("failed to create MemoryManager: %s", err)
	}
	mm.layout = arch.MmapLayout{
		MinAddr:          p.MinUserAddress(),
		MaxAddr:          p.MaxUserAddress(),
		BottomUpBase:     p.MinUserAddress(),
		TopDownBase:      p.MaxUserAddress(),
		DefaultDirection: mmapDirection,
	}
	return mm
}

func testMemoryManager(ctx context.Context, t testing.TB) *MemoryManager {
	return testMemoryManagerWithMmapDirection(ctx, t, arch.MmapBottomUp)
}

func (mm *MemoryManager) realUsageAS() uint64 {
	return uint64(mm.vmas.Span())
}

func cloneMemoryManagerMetadata(ctx context.Context, src *MemoryManager) (*MemoryManager, error) {
	const snapshotKey = 1
	snapshots := LiveForkPMASnapshots{
		snapshotKey: src.PrepareLiveForkSnapshot(snapshotKey),
	}
	defer src.ClearLiveForkPMASnapshot()
	src.mf.MarkSavable()

	var metadata bytes.Buffer
	if _, err := state.Save(ctx, &metadata, src); err != nil {
		return nil, fmt.Errorf("state.Save: %w", err)
	}
	loadCtx := context.WithValues(ctx, map[any]any{
		pgalloc.CtxLiveForkClone: true,
		CtxLiveForkPMASnapshots:  snapshots,
	})
	dst := &MemoryManager{}
	if _, err := state.Load(loadCtx, &metadata, dst); err != nil {
		return nil, fmt.Errorf("state.Load: %w", err)
	}
	return dst, nil
}

func newRemoteCloneMemoryFile(t *testing.T) *pgalloc.MemoryFile {
	t.Helper()
	file, err := os.CreateTemp("", "ttc-mm-remote-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		t.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(file, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
	})
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	return mf
}

func TestLiveForkRemoteMemoryClone(t *testing.T) {
	ctx := contexttest.Context(t)
	parent := testMemoryManager(ctx, t)
	defer parent.DecUsers(ctx)

	addr, err := parent.MMap(ctx, memmap.MMapOpts{
		Length:   4 * hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap: %v", err)
	}
	want := bytes.Repeat([]byte("remote-memory-"), 300)
	if n, err := parent.CopyOut(ctx, addr+hostarch.PageSize-100, want, usermem.IOOpts{}); err != nil || n != len(want) {
		t.Fatalf("parent.CopyOut: n=%d err=%v", n, err)
	}

	const snapshotKey = 1
	pmaSnapshots := LiveForkPMASnapshots{snapshotKey: parent.PrepareLiveForkSnapshot(snapshotKey)}
	defer parent.ClearLiveForkPMASnapshot()
	parent.mf.MarkSavable()
	var metadata bytes.Buffer
	if _, err := state.Save(ctx, &metadata, parent); err != nil {
		t.Fatalf("state.Save: %v", err)
	}

	targetMF := newRemoteCloneMemoryFile(t)
	defer targetMF.Destroy()
	regions, loads, err := pgalloc.CloneMemoryFileMetadataForRemote(ctx, parent.mf, targetMF)
	if err != nil {
		t.Fatalf("CloneMemoryFileMetadataForRemote: %v", err)
	}
	snapshot, err := pgalloc.NewRemoteSnapshot(regions)
	if err != nil {
		t.Fatalf("NewRemoteSnapshot: %v", err)
	}
	reader := pgalloc.NewLoopbackRemoteReader(snapshot, 64<<10, 16, 4)
	loaderDone := make(chan error, 1)
	pages, err := pgalloc.StartAsyncPagesFileLoad(reader, func(err error) { loaderDone <- err }, nil)
	if err != nil {
		t.Fatalf("StartAsyncPagesFileLoad: %v", err)
	}
	if err := targetMF.AttachRemotePageLoad(pages, loads, nil, nil); err != nil {
		t.Fatalf("AttachRemotePageLoad: %v", err)
	}
	pages.MemoryFilesDone()

	loadCtx := context.WithValues(ctx, map[any]any{
		pgalloc.CtxMemoryFile:                targetMF,
		pgalloc.CtxLiveForkClone:             true,
		pgalloc.CtxLiveForkRemoteMemoryClone: true,
		CtxLiveForkPMASnapshots:              pmaSnapshots,
	})
	child := &MemoryManager{}
	if _, err := state.Load(loadCtx, bytes.NewReader(metadata.Bytes()), child); err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	defer child.DecUsers(ctx)
	for pseg := child.pmas.FirstSegment(); pseg.Ok(); pseg = pseg.NextSegment() {
		if got, ok := pseg.ValuePtr().file.(*pgalloc.MemoryFile); ok && got != targetMF {
			t.Fatalf("child PMA retained source MemoryFile %p, want target %p", got, targetMF)
		}
	}

	got := make([]byte, len(want))
	if n, err := child.CopyIn(ctx, addr+hostarch.PageSize-100, got, usermem.IOOpts{}); err != nil || n != len(got) {
		t.Fatalf("child.CopyIn: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("remote-memory child observed incorrect fork-time contents")
	}
	if err := targetMF.AwaitLoadAll(); err != nil {
		t.Fatalf("AwaitLoadAll: %v", err)
	}
	if err := <-loaderDone; err != nil {
		t.Fatalf("remote loader: %v", err)
	}
	if stats := snapshot.Stats(); !stats.Closed || stats.PagesOutstanding != 0 {
		t.Fatalf("snapshot was not drained: %+v", stats)
	}

	childBytes := []byte("child-private")
	if n, err := child.CopyOut(ctx, addr, childBytes, usermem.IOOpts{}); err != nil || n != len(childBytes) {
		t.Fatalf("child.CopyOut: n=%d err=%v", n, err)
	}
	parentBytes := make([]byte, len(childBytes))
	if n, err := parent.CopyIn(ctx, addr, parentBytes, usermem.IOOpts{}); err != nil || n != len(parentBytes) {
		t.Fatalf("parent.CopyIn: n=%d err=%v", n, err)
	}
	if bytes.Equal(parentBytes, childBytes) {
		t.Fatal("remote child write changed source memory")
	}
}

func TestDistributedForkRemoteMemoryClone(t *testing.T) {
	ctx := contexttest.Context(t)
	parent := testMemoryManager(ctx, t)
	defer parent.DecUsers(ctx)
	addr, err := parent.MMap(ctx, memmap.MMapOpts{
		Length: 4 * hostarch.PageSize, Private: true,
		Perms: hostarch.ReadWrite, MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap: %v", err)
	}
	want := bytes.Repeat([]byte("distributed-memory-"), 200)
	if n, err := parent.CopyOut(ctx, addr+hostarch.PageSize-100, want, usermem.IOOpts{}); err != nil || n != len(want) {
		t.Fatalf("parent.CopyOut: n=%d err=%v", n, err)
	}
	parent.PrepareDistributedForkCOW()
	if parent.liveForkPMAKey != 0 {
		t.Fatalf("distributed PMA key is %d, want 0", parent.liveForkPMAKey)
	}
	savedPMAs := parent.savePmas()
	if parent.pmas.IsEmpty() || len(savedPMAs) == 0 {
		t.Fatal("distributed source has no serializable PMAs")
	}
	capture := pgalloc.NewMemoryRangeCapture()
	if err := parent.CaptureMemoryFileRanges(capture); err != nil {
		t.Fatalf("CaptureMemoryFileRanges: %v", err)
	}
	bindings, err := capture.Bindings()
	if err != nil {
		t.Fatalf("capture.Bindings: %v", err)
	}
	wantCapturedPages := 0
	for _, pma := range savedPMAs {
		wantCapturedPages += int(pma.End-pma.Start) / hostarch.PageSize
	}
	if len(bindings) != wantCapturedPages {
		t.Fatalf("captured %d PMA pages, want %d", len(bindings), wantCapturedPages)
	}
	parent.mf.MarkSavable()
	var metadata bytes.Buffer
	if _, err := state.Save(ctx, &metadata, parent); err != nil {
		t.Fatalf("state.Save: %v", err)
	}
	targetMF := newRemoteCloneMemoryFile(t)
	defer targetMF.Destroy()
	regions, loads, err := pgalloc.CloneMemoryFileMetadataForRemote(ctx, parent.mf, targetMF)
	if err != nil {
		t.Fatalf("CloneMemoryFileMetadataForRemote: %v", err)
	}
	snapshot, err := pgalloc.NewRemoteSnapshot(regions)
	if err != nil {
		t.Fatalf("NewRemoteSnapshot: %v", err)
	}
	reader := pgalloc.NewLoopbackRemoteReader(snapshot, 64<<10, 16, 4)
	pages, err := pgalloc.StartAsyncPagesFileLoad(reader, nil, nil)
	if err != nil {
		t.Fatalf("StartAsyncPagesFileLoad: %v", err)
	}
	if err := targetMF.AttachRemotePageLoad(pages, loads, nil, nil); err != nil {
		t.Fatalf("AttachRemotePageLoad: %v", err)
	}
	pages.MemoryFilesDone()
	loadCtx := context.WithValues(ctx, map[any]any{
		pgalloc.CtxMemoryFile:                targetMF,
		pgalloc.CtxLiveForkClone:             true,
		pgalloc.CtxLiveForkRemoteMemoryClone: true,
	})
	child := &MemoryManager{}
	if _, err := state.Load(loadCtx, bytes.NewReader(metadata.Bytes()), child); err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	defer child.DecUsers(ctx)
	if child.pmas.IsEmpty() {
		t.Fatal("distributed child restored no PMAs")
	}
	got := make([]byte, len(want))
	if n, err := child.CopyIn(ctx, addr+hostarch.PageSize-100, got, usermem.IOOpts{}); err != nil || n != len(got) {
		t.Fatalf("child.CopyIn: n=%d err=%v", n, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("distributed child observed incorrect fork-time contents")
	}
}

func TestLiveForkSourceCOWEmitsRemotePushHint(t *testing.T) {
	ctx := contexttest.Context(t)
	parent := testMemoryManager(ctx, t)
	defer parent.DecUsers(ctx)
	addr, err := parent.MMap(ctx, memmap.MMapOpts{
		Length: hostarch.PageSize, Private: true,
		Perms: hostarch.ReadWrite, MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parent.CopyOut(ctx, addr, []byte{1}, usermem.IOOpts{}); err != nil {
		t.Fatal(err)
	}
	parent.PrepareLiveForkSnapshot(1)
	defer parent.ClearLiveForkPMASnapshot()
	pseg := parent.pmas.FindSegment(addr)
	if !pseg.Ok() {
		t.Fatal("parent PMA missing")
	}
	fr := pseg.fileRange()
	snapshot, err := pgalloc.NewRemoteSnapshot([]pgalloc.RemoteSnapshotRegion{{ID: 1, File: parent.mf, Range: fr}})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if _, err := parent.CopyOut(ctx, addr, []byte{2}, usermem.IOOpts{}); err != nil {
		t.Fatal(err)
	}
	select {
	case hint := <-snapshot.PushHints():
		if hint.StreamOffset != 0 || hint.Length != hostarch.PageSize {
			t.Fatalf("push hint=%+v", hint)
		}
	default:
		t.Fatal("source COW emitted no remote push hint")
	}
}

func TestLiveForkPrivateAnonymousMemoryCOW(t *testing.T) {
	ctx := contexttest.Context(t)
	parent := testMemoryManager(ctx, t)
	defer parent.DecUsers(ctx)

	addr, err := parent.MMap(ctx, memmap.MMapOpts{
		Length:   2 * hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap: %v", err)
	}
	initial := []byte("parent-before-fork")
	if n, err := parent.CopyOut(ctx, addr, initial, usermem.IOOpts{}); err != nil || n != len(initial) {
		t.Fatalf("parent.CopyOut: n=%d err=%v", n, err)
	}

	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	defer child.DecUsers(ctx)
	parentBefore := parent.pmas.FindSegment(addr)
	childBefore := child.pmas.FindSegment(addr)
	if !parentBefore.Ok() || !childBefore.Ok() {
		t.Fatal("forked PMA missing")
	}
	parentID, parentVersion, ok := parent.mf.LogicalPageVersionAt(parentBefore.fileRange().Start)
	if !ok {
		t.Fatal("parent PMA has no logical page identity")
	}
	childID, childVersion, ok := child.mf.LogicalPageVersionAt(childBefore.fileRange().Start)
	if !ok || childID != parentID || childVersion != parentVersion {
		t.Fatalf("child did not initially share page version: parent=(%+v,%+v) child=(%+v,%+v,%v)", parentID, parentVersion, childID, childVersion, ok)
	}

	read := func(name string, memMgr *MemoryManager) string {
		buf := make([]byte, len(initial))
		if n, err := memMgr.CopyIn(ctx, addr, buf, usermem.IOOpts{}); err != nil || n != len(buf) {
			t.Fatalf("%s.CopyIn: n=%d err=%v", name, n, err)
		}
		return string(buf)
	}
	if got := read("child", child); got != string(initial) {
		t.Fatalf("child initial bytes: got %q, want %q", got, initial)
	}

	childBytes := []byte("child--after--fork")
	if n, err := child.CopyOut(ctx, addr, childBytes, usermem.IOOpts{}); err != nil || n != len(childBytes) {
		t.Fatalf("child.CopyOut: n=%d err=%v", n, err)
	}
	childAfter := child.pmas.FindSegment(addr)
	if !childAfter.Ok() {
		t.Fatal("child PMA missing after CoW")
	}
	childID, childVersion, ok = child.mf.LogicalPageVersionAt(childAfter.fileRange().Start)
	if !ok || childID != parentID || childVersion == parentVersion {
		t.Fatalf("child CoW identities: parent=(%+v,%+v) child=(%+v,%+v,%v)", parentID, parentVersion, childID, childVersion, ok)
	}
	if got := read("parent", parent); got != string(initial) {
		t.Errorf("child write changed parent: got %q, want %q", got, initial)
	}
	if got := read("child", child); got != string(childBytes) {
		t.Errorf("child write missing in child: got %q, want %q", got, childBytes)
	}

	parentBytes := []byte("parent-after-fork")
	if n, err := parent.CopyOut(ctx, addr, parentBytes, usermem.IOOpts{}); err != nil || n != len(parentBytes) {
		t.Fatalf("parent.CopyOut after fork: n=%d err=%v", n, err)
	}
	if got := read("child", child); got != string(childBytes) {
		t.Errorf("parent write changed child: got %q, want %q", got, childBytes)
	}
}

func TestLiveForkPrivateAnonymousMemoryApplicationWriteFault(t *testing.T) {
	ctx := contexttest.Context(t)
	parent := testMemoryManager(ctx, t)
	defer parent.DecUsers(ctx)

	addr, err := parent.MMap(ctx, memmap.MMapOpts{
		Length:   2 * hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap: %v", err)
	}
	if _, err := parent.CopyOut(ctx, addr, []byte{1}, usermem.IOOpts{}); err != nil {
		t.Fatalf("faulting first parent page: %v", err)
	}
	second := addr + hostarch.PageSize
	if _, err := parent.CopyOut(ctx, second, []byte{2}, usermem.IOOpts{}); err != nil {
		t.Fatalf("faulting second parent page: %v", err)
	}

	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("clone child: %v", err)
	}
	defer child.DecUsers(ctx)

	if err := parent.HandleUserFault(ctx, second, hostarch.Write, 0); err != nil {
		t.Errorf("parent HandleUserFault(Write): %v", err)
	}
	if err := child.HandleUserFault(ctx, second, hostarch.Write, 0); err != nil {
		t.Errorf("child HandleUserFault(Write): %v", err)
	}
	for name, memMgr := range map[string]*MemoryManager{"parent": parent, "child": child} {
		got := make([]byte, 1)
		if _, err := memMgr.CopyIn(ctx, addr, got, usermem.IOOpts{}); err != nil {
			t.Fatalf("%s CopyIn(first page): %v", name, err)
		}
		if got[0] != 1 {
			t.Errorf("%s first page after second-page write fault: got %d, want 1", name, got[0])
		}
	}
}

func TestLiveForkPrivateAnonymousMemoryRepeatedRelease(t *testing.T) {
	ctx := contexttest.Context(t)
	parent := testMemoryManager(ctx, t)
	defer parent.DecUsers(ctx)

	addr, err := parent.MMap(ctx, memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap: %v", err)
	}
	if _, err := parent.CopyOut(ctx, addr, []byte{1}, usermem.IOOpts{}); err != nil {
		t.Fatalf("faulting parent page: %v", err)
	}
	pseg := parent.pmas.FirstSegment()
	if !pseg.Ok() {
		t.Fatal("fault did not create a PMA")
	}
	fr := pseg.fileRange()
	if !parent.mf.HasUniqueRef(fr) {
		t.Fatal("parent PMA is not uniquely referenced before cloning")
	}

	for i := 0; i < 100; i++ {
		child, err := cloneMemoryManagerMetadata(ctx, parent)
		if err != nil {
			t.Fatalf("clone %d: %v", i, err)
		}
		if parent.mf.HasUniqueRef(fr) {
			t.Fatalf("clone %d did not acquire a child page reference", i)
		}
		child.DecUsers(ctx)
		if !parent.mf.HasUniqueRef(fr) {
			t.Fatalf("clone %d leaked a child page reference", i)
		}
	}
}

func TestLiveForkPrivateAnonymousMemoryBranchTreeReleaseOrder(t *testing.T) {
	ctx := contexttest.Context(t)
	parent := testMemoryManager(ctx, t)

	addr, err := parent.MMap(ctx, memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap: %v", err)
	}
	write := func(name string, memMgr *MemoryManager, value byte) {
		t.Helper()
		if n, err := memMgr.CopyOut(ctx, addr, []byte{value}, usermem.IOOpts{}); err != nil || n != 1 {
			t.Fatalf("%s.CopyOut: n=%d err=%v", name, n, err)
		}
	}
	read := func(name string, memMgr *MemoryManager, want byte) {
		t.Helper()
		buf := []byte{0}
		if n, err := memMgr.CopyIn(ctx, addr, buf, usermem.IOOpts{}); err != nil || n != 1 {
			t.Fatalf("%s.CopyIn: n=%d err=%v", name, n, err)
		}
		if buf[0] != want {
			t.Fatalf("%s byte: got %#x, want %#x", name, buf[0], want)
		}
	}

	write("parent", parent, 0x11)
	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("clone child: %v", err)
	}
	sibling, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("clone sibling: %v", err)
	}
	write("child", child, 0x22)
	grandchild, err := cloneMemoryManagerMetadata(ctx, child)
	if err != nil {
		t.Fatalf("clone grandchild: %v", err)
	}

	// Destroy ancestors before descendants. Surviving branches must retain both
	// their page references and their branch-local view of the bytes.
	parent.DecUsers(ctx)
	read("sibling after parent release", sibling, 0x11)
	read("child after parent release", child, 0x22)
	read("grandchild after parent release", grandchild, 0x22)
	child.DecUsers(ctx)
	read("sibling after child release", sibling, 0x11)
	read("grandchild after child release", grandchild, 0x22)
	write("grandchild", grandchild, 0x33)
	read("sibling after grandchild write", sibling, 0x11)
	read("grandchild after write", grandchild, 0x33)

	sibling.DecUsers(ctx)
	read("grandchild after sibling release", grandchild, 0x33)
	grandchild.DecUsers(ctx)
}

func BenchmarkLiveForkPrivateAnonymousMemory(b *testing.B) {
	for _, size := range []uint64{1 << 20, 1 << 30} {
		b.Run(fmt.Sprintf("resident_%d_MiB", size>>20), func(b *testing.B) {
			ctx := contexttest.Context(b)
			parent := testMemoryManager(ctx, b)
			defer parent.DecUsers(ctx)
			addr, err := parent.MMap(ctx, memmap.MMapOpts{
				Length:   size,
				Private:  true,
				Perms:    hostarch.ReadWrite,
				MaxPerms: hostarch.AnyAccess,
			})
			if err != nil {
				b.Fatalf("MMap: %v", err)
			}
			one := []byte{1}
			for off := uint64(0); off < size; off += hostarch.PageSize {
				if _, err := parent.CopyOut(ctx, addr+hostarch.Addr(off), one, usermem.IOOpts{}); err != nil {
					b.Fatalf("faulting page at %#x: %v", off, err)
				}
			}

			b.ReportMetric(float64(size), "resident-bytes")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				child, err := cloneMemoryManagerMetadata(ctx, parent)
				if err != nil {
					b.Fatalf("cloneMemoryManagerMetadata: %v", err)
				}
				b.StopTimer()
				child.DecUsers(ctx)
				b.StartTimer()
			}
		})
	}
}

func TestUsageASUpdates(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:  2 * hostarch.PageSize,
		Private: true,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	realUsage := mm.realUsageAS()
	if mm.usageAS != realUsage {
		t.Fatalf("usageAS believes %v bytes are mapped; %v bytes are actually mapped", mm.usageAS, realUsage)
	}

	mm.MUnmap(ctx, addr, hostarch.PageSize)
	realUsage = mm.realUsageAS()
	if mm.usageAS != realUsage {
		t.Fatalf("usageAS believes %v bytes are mapped; %v bytes are actually mapped", mm.usageAS, realUsage)
	}
}

func (mm *MemoryManager) realDataAS() uint64 {
	var sz uint64
	for seg := mm.vmas.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		vma := seg.ValuePtr()
		if vma.isPrivateDataLocked() {
			sz += uint64(seg.Range().Length())
		}
	}
	return sz
}

func TestDataASUpdates(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   3 * hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.Write,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	if mm.dataAS == 0 {
		t.Fatalf("dataAS is 0, wanted not 0")
	}
	realDataAS := mm.realDataAS()
	if mm.dataAS != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", mm.dataAS, realDataAS)
	}

	mm.MUnmap(ctx, addr, hostarch.PageSize)
	realDataAS = mm.realDataAS()
	if mm.dataAS != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", mm.dataAS, realDataAS)
	}

	mm.MProtect(addr+hostarch.PageSize, hostarch.PageSize, hostarch.Read, false)
	realDataAS = mm.realDataAS()
	if mm.dataAS != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", mm.dataAS, realDataAS)
	}

	mm.MRemap(ctx, addr+2*hostarch.PageSize, hostarch.PageSize, 2*hostarch.PageSize, MRemapOpts{
		Move: MRemapMayMove,
	})
	realDataAS = mm.realDataAS()
	if mm.dataAS != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", mm.dataAS, realDataAS)
	}
}

func TestBrkDataLimitUpdates(t *testing.T) {
	limitSet := limits.NewLimitSet()
	limitSet.Set(limits.Data, limits.Limit{}, true /* privileged */) // zero RLIMIT_DATA

	ctx := contexttest.WithLimitSet(contexttest.Context(t), limitSet)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	// Try to extend the brk by one page and expect doing so to fail.
	oldBrk, _ := mm.Brk(ctx, 0)
	if newBrk, _ := mm.Brk(ctx, oldBrk+hostarch.PageSize); newBrk != oldBrk {
		t.Errorf("brk() increased data segment above RLIMIT_DATA (old brk = %#x, new brk = %#x", oldBrk, newBrk)
	}
}

// TestIOAfterUnmap ensures that IO fails after unmap.
func TestIOAfterUnmap(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.Read,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}

	// IO works before munmap.
	b := make([]byte, 1)
	n, err := mm.CopyIn(ctx, addr, b, usermem.IOOpts{})
	if err != nil {
		t.Errorf("CopyIn got err %v want nil", err)
	}
	if n != 1 {
		t.Errorf("CopyIn got %d want 1", n)
	}

	err = mm.MUnmap(ctx, addr, hostarch.PageSize)
	if err != nil {
		t.Fatalf("MUnmap got err %v want nil", err)
	}

	n, err = mm.CopyIn(ctx, addr, b, usermem.IOOpts{})
	if !linuxerr.Equals(linuxerr.EFAULT, err) {
		t.Errorf("CopyIn got err %v want EFAULT", err)
	}
	if n != 0 {
		t.Errorf("CopyIn got %d want 0", n)
	}
}

// TestIOAfterMProtect tests IO interaction with mprotect permissions.
func TestIOAfterMProtect(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   hostarch.PageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}

	// Writing works before mprotect.
	b := make([]byte, 1)
	n, err := mm.CopyOut(ctx, addr, b, usermem.IOOpts{})
	if err != nil {
		t.Errorf("CopyOut got err %v want nil", err)
	}
	if n != 1 {
		t.Errorf("CopyOut got %d want 1", n)
	}

	err = mm.MProtect(addr, hostarch.PageSize, hostarch.Read, false)
	if err != nil {
		t.Errorf("MProtect got err %v want nil", err)
	}

	// Without IgnorePermissions, CopyOut should no longer succeed.
	n, err = mm.CopyOut(ctx, addr, b, usermem.IOOpts{})
	if !linuxerr.Equals(linuxerr.EFAULT, err) {
		t.Errorf("CopyOut got err %v want EFAULT", err)
	}
	if n != 0 {
		t.Errorf("CopyOut got %d want 0", n)
	}

	// With IgnorePermissions, CopyOut should succeed despite mprotect.
	n, err = mm.CopyOut(ctx, addr, b, usermem.IOOpts{
		IgnorePermissions: true,
	})
	if err != nil {
		t.Errorf("CopyOut got err %v want nil", err)
	}
	if n != 1 {
		t.Errorf("CopyOut got %d want 1", n)
	}
}

// TestAIOPrepareAfterDestroy tests that AIOContext should not be able to be
// prepared after destruction.
func TestAIOPrepareAfterDestroy(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	id, err := mm.NewAIOContext(ctx, 1)
	if err != nil {
		t.Fatalf("mm.NewAIOContext got err %v want nil", err)
	}
	aioCtx, ok := mm.LookupAIOContext(ctx, id)
	if !ok {
		t.Fatalf("AIOContext not found")
	}
	mm.DestroyAIOContext(ctx, id)

	// Prepare should fail because aioCtx should be destroyed.
	if err := aioCtx.Prepare(); !linuxerr.Equals(linuxerr.EINVAL, err) {
		t.Errorf("aioCtx.Prepare got err %v want nil", err)
	} else if err == nil {
		aioCtx.CancelPendingRequest()
	}
}

// TestAIOLookupAfterDestroy tests that AIOContext should not be able to be
// looked up after memory manager is destroyed.
func TestAIOLookupAfterDestroy(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)

	id, err := mm.NewAIOContext(ctx, 1)
	if err != nil {
		mm.DecUsers(ctx)
		t.Fatalf("mm.NewAIOContext got err %v want nil", err)
	}
	mm.DecUsers(ctx) // This destroys the AIOContext manager.

	if _, ok := mm.LookupAIOContext(ctx, id); ok {
		t.Errorf("AIOContext found even after AIOContext manager is destroyed")
	}
}

func TestGetAllocationDirection(t *testing.T) {
	testCases := []struct {
		name          string
		mmapDirection arch.MmapDirection
		ar            hostarch.AddrRange
		vma           *vma
		expected      pgalloc.Direction
	}{
		{
			"No last fault in vma with mmap direction BottomUp",
			arch.MmapBottomUp,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 0},
			pgalloc.BottomUp,
		},
		{
			"No last fault in vma with mmap direction TopDown",
			arch.MmapTopDown,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 0},
			pgalloc.TopDown,
		},
		{
			"Last fault in vma equals to addr range, with mmap direction BottomUp",
			arch.MmapBottomUp,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 123},
			pgalloc.BottomUp,
		},
		{
			"Last fault in vma equals to addr range, with mmap direction TopDown",
			arch.MmapTopDown,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 123},
			pgalloc.TopDown,
		},
		{
			"Last fault in vma greater than addr range",
			arch.MmapTopDown,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 456},
			pgalloc.TopDown,
		},
		{
			"Last fault in vma smaller than addr range",
			arch.MmapTopDown,
			hostarch.AddrRange{123, 456},
			&vma{lastFault: 100},
			pgalloc.BottomUp,
		},
	}
	for _, test := range testCases {
		t.Run(test.name, func(t *testing.T) {
			ctx := contexttest.Context(t)
			mm := testMemoryManagerWithMmapDirection(ctx, t, test.mmapDirection)
			actual := mm.getAllocationDirection(test.ar, test.vma)
			if actual != test.expected {
				t.Errorf("Unexpected allocation direction. Expected: %s, Actual: %s", test.expected, actual)
			}
		})
	}
}
