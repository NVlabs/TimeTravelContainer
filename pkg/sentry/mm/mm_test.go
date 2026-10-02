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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mm

import (
	"bytes"
	"fmt"
	"os"
	"testing"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
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
	snapshot, err := src.PrepareLiveForkSnapshot(snapshotKey)
	defer src.ClearLiveForkPMASnapshot()
	if err != nil {
		return nil, err
	}
	snapshots := LiveForkPMASnapshots{snapshotKey: snapshot}
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

func TestCaptureMemoryFileRangesIncludesUnfaultedAliasedMappableOwnerOnce(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)
	fr, err := mm.mf.Allocate(2*hostarch.PageSize, pgalloc.AllocOpts{})
	if err != nil {
		t.Fatal(err)
	}
	mappable := NewSpecialMappable("[capture-test]", mm.mf, fr)
	defer mappable.DecRef(ctx)
	for i := 0; i < 2; i++ {
		if _, err := mm.MMap(ctx, memmap.MMapOpts{
			Length:          fr.Length(),
			MappingIdentity: mappable,
			Mappable:        mappable,
			Perms:           hostarch.Read,
			MaxPerms:        hostarch.Read,
		}); err != nil {
			t.Fatalf("MMap alias %d: %v", i, err)
		}
	}
	if !mm.pmas.IsEmpty() {
		t.Fatal("unfaulted special mappings unexpectedly have PMAs")
	}
	capture := pgalloc.NewMemoryRangeCapture()
	if err := mm.CaptureMemoryFileRanges(capture); err != nil {
		t.Fatal(err)
	}
	bindings, err := capture.Bindings()
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 2 {
		t.Fatalf("captured %d pages, want 2", len(bindings))
	}
	for _, binding := range bindings {
		if binding.References != 1 {
			t.Fatalf("aliased Mappable page has %d captured references, want 1", binding.References)
		}
	}
}

func TestInvalidateMappingsForReboundSpecialMappable(t *testing.T) {
	ctx := contexttest.Context(t)
	memMgr := testMemoryManager(ctx, t)
	defer memMgr.DecUsers(ctx)

	writePage := func(value byte) memmap.FileRange {
		fr, err := memMgr.mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{Mode: pgalloc.AllocateAndCommit})
		if err != nil {
			t.Fatal(err)
		}
		blocks, err := memMgr.mf.MapInternal(fr, hostarch.ReadWrite)
		if err != nil {
			memMgr.mf.DecRef(fr)
			t.Fatal(err)
		}
		if _, err := safemem.CopySeq(blocks, safemem.BlockSeqOf(safemem.BlockFromSafeSlice([]byte{value}))); err != nil {
			memMgr.mf.DecRef(fr)
			t.Fatal(err)
		}
		return fr
	}
	oldRange := writePage(1)
	newRange := writePage(2)
	mappable := NewSpecialMappable("[rebind-test]", memMgr.mf, oldRange)
	defer mappable.DecRef(ctx)
	addr, err := memMgr.MMap(ctx, memmap.MMapOpts{
		Length: hostarch.PageSize, MappingIdentity: mappable, Mappable: mappable,
		Perms: hostarch.Read, MaxPerms: hostarch.Read,
	})
	if err != nil {
		t.Fatal(err)
	}
	read := func() byte {
		var got [1]byte
		if n, err := memMgr.CopyIn(ctx, addr, got[:], usermem.IOOpts{}); err != nil || n != 1 {
			t.Fatalf("CopyIn: n=%d err=%v", n, err)
		}
		return got[0]
	}
	if got := read(); got != 1 {
		t.Fatalf("initial byte=%d, want 1", got)
	}
	mappable.RebindMemoryFileRange(memMgr.mf, newRange)
	memMgr.InvalidateMappingsForMappable(mappable)
	if got := read(); got != 2 {
		t.Fatalf("byte after rebind=%d, want 2", got)
	}
}

func TestSpecialMappableSparseRestoreLinearizesDiscontiguousRanges(t *testing.T) {
	ctx := contexttest.Context(t)
	memMgr := testMemoryManager(ctx, t)
	defer memMgr.DecUsers(ctx)

	allocatePage := func(value byte) memmap.FileRange {
		fr, err := memMgr.mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{Mode: pgalloc.AllocateAndCommit})
		if err != nil {
			t.Fatal(err)
		}
		blocks, err := memMgr.mf.MapInternal(fr, hostarch.ReadWrite)
		if err != nil {
			memMgr.mf.DecRef(fr)
			t.Fatal(err)
		}
		data := bytes.Repeat([]byte{value}, hostarch.PageSize)
		if _, err := safemem.CopySeq(blocks, safemem.BlockSeqOf(safemem.BlockFromSafeSlice(data))); err != nil {
			memMgr.mf.DecRef(fr)
			t.Fatal(err)
		}
		return fr
	}
	source, err := memMgr.mf.Allocate(2*hostarch.PageSize, pgalloc.AllocOpts{})
	if err != nil {
		t.Fatal(err)
	}
	first := allocatePage(0x11)
	gap := allocatePage(0x99)
	defer memMgr.mf.DecRef(gap)
	second := allocatePage(0x22)
	relocation := pgalloc.NewSparseRelocation()
	if err := relocation.Add(memMgr.mf.ResourceID(), source.Start, first.Start); err != nil {
		t.Fatal(err)
	}
	if err := relocation.Add(memMgr.mf.ResourceID(), source.Start+hostarch.PageSize, second.Start); err != nil {
		t.Fatal(err)
	}
	mappable := NewSpecialMappable("[sparse-test]", memMgr.mf, source)
	references := pgalloc.NewSparseReferenceTransaction()
	loadCtx := context.WithValues(ctx, map[any]any{
		pgalloc.CtxMemoryFile:        memMgr.mf,
		pgalloc.CtxSparseRelocation:  relocation,
		pgalloc.CtxSparseRestoreRefs: references,
		pgalloc.CtxLiveForkClone:     true,
	})
	mappable.afterLoad(loadCtx)
	if err := references.Commit(); err != nil {
		t.Fatal(err)
	}
	// In a real sparse restore the serialized source offsets do not own ranges
	// in the target MemoryFile. This test allocated them only to obtain valid
	// logical page identities.
	memMgr.mf.DecRef(source)
	memMgr.mf.DecRef(first)
	memMgr.mf.DecRef(second)
	defer mappable.DecRef(ctx)

	translations, err := mappable.Translate(ctx,
		memmap.MappableRange{End: 2 * hostarch.PageSize},
		memmap.MappableRange{End: 2 * hostarch.PageSize}, hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	if len(translations) != 1 || translations[0].Offset == first.Start || translations[0].Offset == second.Start {
		t.Fatalf("translations=%+v, want one newly allocated contiguous range", translations)
	}
	if mappable.Length() != 2*hostarch.PageSize {
		t.Fatalf("length=%d, want %d", mappable.Length(), 2*hostarch.PageSize)
	}
	contents, err := memMgr.mf.MapInternal(mappable.FileRange(), hostarch.Read)
	if err != nil {
		t.Fatal(err)
	}
	var got [2]byte
	for i, offset := range []uint64{0, hostarch.PageSize} {
		got[i] = contents.DropFirst64(offset).Head().ToSlice()[0]
	}
	if got != [2]byte{0x11, 0x22} {
		t.Fatalf("linearized page prefixes=%#x, want 0x11,0x22", got)
	}
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
	pmaSnapshot, err := parent.PrepareLiveForkSnapshot(snapshotKey)
	defer parent.ClearLiveForkPMASnapshot()
	if err != nil {
		t.Fatal(err)
	}
	pmaSnapshots := LiveForkPMASnapshots{snapshotKey: pmaSnapshot}
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
	// A distributed source resumes before its target necessarily faults any
	// pages. Verify that a subsequent source write cannot change the fork-time
	// bytes served to the demand-paged child.
	mutated := bytes.Repeat([]byte("source-after-fork---"), 200)
	if n, err := parent.CopyOut(ctx, addr+hostarch.PageSize-100, mutated, usermem.IOOpts{}); err != nil || n != len(mutated) {
		t.Fatalf("parent.CopyOut after snapshot: n=%d err=%v", n, err)
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
	_, err = parent.PrepareLiveForkSnapshot(1)
	defer parent.ClearLiveForkPMASnapshot()
	if err != nil {
		t.Fatal(err)
	}
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

// populatedPrivateMapping maps length bytes of private anonymous memory in
// memMgr and touches every page, so that all of it is resident.
func populatedPrivateMapping(ctx context.Context, t testing.TB, memMgr *MemoryManager, length uint64) hostarch.Addr {
	t.Helper()
	addr, err := memMgr.MMap(ctx, memmap.MMapOpts{
		Length:   length,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap: %v", err)
	}
	for page := addr; page < addr+hostarch.Addr(length); page += hostarch.PageSize {
		if _, err := memMgr.CopyOut(ctx, page, []byte{1}, usermem.IOOpts{}); err != nil {
			t.Fatalf("faulting page %#x: %v", page, err)
		}
	}
	return addr
}

// sharesPage reports whether a and b hold the same version of the page at
// addr, which is to say that neither has copied it since they forked.
func sharesPage(t testing.TB, a, b *MemoryManager, addr hostarch.Addr) bool {
	t.Helper()
	version := func(memMgr *MemoryManager) pgalloc.PageVersionID {
		pseg := memMgr.pmas.FindSegment(addr)
		if !pseg.Ok() {
			t.Fatalf("no pma at %#x", addr)
		}
		_, v, ok := memMgr.mf.LogicalPageVersionAt(pseg.fileRange().Start + uint64(addr-pseg.Start()))
		if !ok {
			t.Fatalf("no page identity at %#x", addr)
		}
		return v
	}
	return version(a) == version(b)
}

// A write after a live fork copies the page written and nothing around it.
//
// The copy used to be widened to the surrounding huge page, so one written
// byte duplicated 2 MiB and gave all 512 pages new content versions: the
// branches diverged 512 times faster than either wrote, and every later
// capture or transfer saw 512 changed pages where one had changed.
func TestLiveForkCopyOnWriteCopiesOnlyTheWrittenPage(t *testing.T) {
	ctx := contexttest.Context(t)
	parent := testMemoryManager(ctx, t)
	defer parent.DecUsers(ctx)
	// Two huge pages of address space guarantee one aligned huge page inside.
	addr := populatedPrivateMapping(ctx, t, parent, 2*hostarch.HugePageSize)
	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	defer child.DecUsers(ctx)

	target := addr.HugeRoundDown() + hostarch.HugePageSize + 7*hostarch.PageSize
	if err := child.HandleUserFault(ctx, target, hostarch.Write, 0); err != nil {
		t.Fatalf("child write fault: %v", err)
	}
	if got := child.pmas.FindSegment(target).Range().Length(); got != hostarch.PageSize {
		t.Fatalf("a one-page write fault copied %d bytes, want one page", got)
	}
	if sharesPage(t, child, parent, target) {
		t.Fatal("the written page still shares the parent's version")
	}
	for _, neighbor := range []hostarch.Addr{target - hostarch.PageSize, target + hostarch.PageSize} {
		if !sharesPage(t, child, parent, neighbor) {
			t.Fatalf("the unwritten page at %#x was copied with the written one", neighbor)
		}
	}
}

// A write fault that continues a sequential run copies ahead; any other
// copies only its page.
//
// Page-by-page copying costs a platform round trip per page, which made a
// front-to-back rewrite after a fork thirteen times slower. A run of write
// faults that each begin where the previous one ended asks for twice as much
// each time, so a sweep reaches huge-page copies quickly while scattered
// writes stay exact.
func TestLiveForkCopyOnWriteWidensOnlyForSequentialRuns(t *testing.T) {
	ctx := contexttest.Context(t)
	parent := testMemoryManager(ctx, t)
	defer parent.DecUsers(ctx)
	addr := populatedPrivateMapping(ctx, t, parent, 2*hostarch.HugePageSize)
	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	defer child.DecUsers(ctx)
	base := addr.HugeRoundDown() + hostarch.HugePageSize
	page := func(n int) hostarch.Addr { return base + hostarch.Addr(n)*hostarch.PageSize }

	// A sequential run: pages 0 and 1 are written; the second copy doubles.
	for _, n := range []int{0, 1} {
		if err := child.HandleUserFault(ctx, page(n), hostarch.Write, 0); err != nil {
			t.Fatalf("write fault on page %d: %v", n, err)
		}
	}
	if sharesPage(t, child, parent, page(2)) {
		t.Fatal("a write continuing a run did not copy ahead")
	}
	if !sharesPage(t, child, parent, page(3)) {
		t.Fatal("the second copy of a run copied more than twice the first")
	}
	// A write elsewhere breaks the run and copies one page.
	if err := child.HandleUserFault(ctx, page(100), hostarch.Write, 0); err != nil {
		t.Fatalf("write fault on page 100: %v", err)
	}
	if !sharesPage(t, child, parent, page(101)) || !sharesPage(t, child, parent, page(99)) {
		t.Fatal("a write outside the run copied its neighbours")
	}
}

// A sequential run copies ahead across the pmas that earlier page-sized copies
// split memory into, rather than faulting once per piece, and copies them into
// one allocation, rather than one per piece.
//
// Scattered writes in one fork generation leave a pma per copied page, and
// the next fork shares all of them again. Copying ahead only within the pma
// that faulted made a sweep over such memory take six times as long as it had
// with huge-page copies, and copying each piece separately still took half as
// long again.
func TestLiveForkCopyOnWriteRunCrossesSplitPMAs(t *testing.T) {
	ctx := contexttest.Context(t)
	parent := testMemoryManager(ctx, t)
	defer parent.DecUsers(ctx)
	addr := populatedPrivateMapping(ctx, t, parent, 2*hostarch.HugePageSize)
	base := addr.HugeRoundDown() + hostarch.HugePageSize
	page := func(n int) hostarch.Addr { return base + hostarch.Addr(n)*hostarch.PageSize }

	// One fork generation writes every fourth page, splitting the pmas.
	earlier, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	for n := 2; n < 64; n += 4 {
		if err := parent.HandleUserFault(ctx, page(n), hostarch.Write, 0); err != nil {
			t.Fatalf("write fault on page %d: %v", n, err)
		}
	}
	earlier.DecUsers(ctx)
	if got := parent.pmas.FindSegment(page(2)).Range().Length(); got != hostarch.PageSize {
		t.Fatalf("setup: the page written after the first fork is in a %d-byte pma, want its own", got)
	}

	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	defer child.DecUsers(ctx)
	// A run of faults at 0, 1, 3 and 7 asks for 1, 2, 4 and 8 pages; the last
	// spans pmas split at pages 10, 11, 14 and 15.
	for _, n := range []int{0, 1, 3, 7} {
		if err := child.HandleUserFault(ctx, page(n), hostarch.Write, 0); err != nil {
			t.Fatalf("write fault on page %d: %v", n, err)
		}
	}
	for n := 0; n < 15; n++ {
		if sharesPage(t, child, parent, page(n)) {
			t.Fatalf("page %d, inside the run, was not copied", n)
		}
	}
	if !sharesPage(t, child, parent, page(15)) {
		t.Fatal("the run copied past what it asked for")
	}
	// And into one allocation, which leaves the pieces joined again.
	run := hostarch.AddrRange{Start: page(7), End: page(15)}
	if got := child.pmas.FindSegment(page(7)).Range(); !got.IsSupersetOf(run) {
		t.Fatalf("the copy of %v is in a pma covering only %v", run, got)
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

// BenchmarkLiveForkWriteFault measures the Sentry's side of copy-on-write
// write faults after a live fork — pma lookup, copy, identity adoption, pma
// and AddressSpace bookkeeping — per page written, for a front-to-back sweep, for
// writes scattered one per huge page, and for a sweep over memory that an
// earlier fork generation's scattered writes split into small pmas.
func BenchmarkLiveForkWriteFault(b *testing.B) {
	sequential := func(i int) int { return i }
	for _, pattern := range []struct {
		name  string
		order func(i int) int
		// split, if not zero, is the stride of pages written in an earlier
		// fork generation, each of which then has a pma of its own.
		split int
	}{
		{name: "sequential", order: sequential},
		// One page per huge page, then the next page of each, and so on.
		{name: "scattered", order: func(i int) int { return (i%32)*512 + i/32 }},
		{name: "sequential-over-split", order: sequential, split: 16},
	} {
		b.Run(pattern.name, func(b *testing.B) {
			ctx := contexttest.Context(b)
			parent := testMemoryManager(ctx, b)
			defer parent.DecUsers(ctx)
			const length = 64 << 20
			const pages = length / hostarch.PageSize
			addr := populatedPrivateMapping(ctx, b, parent, length)
			if pattern.split != 0 {
				earlier, err := cloneMemoryManagerMetadata(ctx, parent)
				if err != nil {
					b.Fatalf("cloneMemoryManagerMetadata: %v", err)
				}
				for n := pattern.split / 2; n < pages; n += pattern.split {
					if err := parent.HandleUserFault(ctx, addr+hostarch.Addr(n)*hostarch.PageSize, hostarch.Write, 0); err != nil {
						b.Fatalf("write fault: %v", err)
					}
				}
				earlier.DecUsers(ctx)
			}
			faults := 0
			b.ResetTimer()
			for done := 0; done < b.N; {
				b.StopTimer()
				child, err := cloneMemoryManagerMetadata(ctx, parent)
				if err != nil {
					b.Fatalf("cloneMemoryManagerMetadata: %v", err)
				}
				b.StartTimer()
				for i := 0; i < pages && done < b.N; i++ {
					// Only a page that is still shared faults; one copied
					// ahead is already writable.
					page := addr + hostarch.Addr(pattern.order(i))*hostarch.PageSize
					if pseg := child.pmas.FindSegment(page); !pseg.Ok() || pseg.ValuePtr().needCOW {
						if err := child.HandleUserFault(ctx, page, hostarch.Write, 0); err != nil {
							b.Fatalf("write fault: %v", err)
						}
						faults++
					}
					done++
				}
				b.StopTimer()
				child.DecUsers(ctx)
				b.StartTimer()
			}
			b.ReportMetric(float64(faults)/float64(b.N), "faults/page")
		})
	}
}

// liveForkTestFile is a File whose pages the test says are immutable, or not.
type liveForkTestFile struct {
	memmap.DefaultMemoryType
	memmap.NoBufferedIOFallback
	shared bool
	refs   uint64
}

func (f *liveForkTestFile) IncRef(fr memmap.FileRange, memCgID uint32) {
	f.refs += fr.Length() / hostarch.PageSize
}

func (f *liveForkTestFile) DecRef(fr memmap.FileRange) {
	f.refs -= fr.Length() / hostarch.PageSize
}

func (f *liveForkTestFile) MapInternal(fr memmap.FileRange, at hostarch.AccessType) (safemem.BlockSeq, error) {
	return safemem.BlockSeq{}, linuxerr.EFAULT
}

func (f *liveForkTestFile) DataFD(fr memmap.FileRange) (int, error) {
	return -1, linuxerr.EINVAL
}

func (f *liveForkTestFile) FD() int {
	return -1
}

func (f *liveForkTestFile) LiveForkShared() bool {
	return f.shared
}

// TestLiveForkChildSharesImmutableFilePMAs checks that a live fork's child
// takes its parent's translations of a file's immutable pages, with
// references of its own, and refuses translations of mutable ones.
func TestLiveForkChildSharesImmutableFilePMAs(t *testing.T) {
	ctx := contexttest.Context(t)
	for _, shared := range []bool{true, false} {
		file := &liveForkTestFile{shared: shared}
		child := &MemoryManager{}
		ar := hostarch.AddrRange{Start: 0x10000, End: 0x10000 + 2*hostarch.PageSize}
		child.pmas.Insert(child.pmas.FirstGap(), ar, pma{
			file:           file,
			translatePerms: hostarch.ReadExecute,
			effectivePerms: hostarch.Read,
			maxPerms:       hostarch.AnyAccess,
		})
		loadCtx := context.WithValue(ctx, pgalloc.CtxLiveForkClone, true)
		panicked := func() (panicked bool) {
			defer func() {
				panicked = recover() != nil
			}()
			child.afterLoad(loadCtx)
			return false
		}()
		if shared && (panicked || file.refs != 2) {
			t.Errorf("the child of a translation of immutable pages: panicked %t, took %d page references, want 2", panicked, file.refs)
		}
		if !shared && !panicked {
			t.Errorf("the child took a translation of mutable pages")
		}
	}
}
