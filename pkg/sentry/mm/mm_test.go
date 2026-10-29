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

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
	"gvisor.dev/gvisor/pkg/sentry/limits"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/state"
	"gvisor.dev/gvisor/pkg/usermem"
)

// Typed versions of hostarch.PageSize, which is not a constant on all
// architectures.
var (
	pageSize     = uint64(hostarch.PageSize)
	pageSizeAddr = hostarch.Addr(hostarch.PageSize)
	pageSizeInt  = int(hostarch.PageSize)
)

func testMemoryManagerWithMmapDirection(ctx context.Context, t testing.TB, mmapDirection arch.MmapDirection) *MemoryManager {
	p := platform.FromContext(ctx)
	mm, err := NewMemoryManager(p, pgalloc.MemoryFileFromContext(ctx))
	if err != nil {
		t.Fatalf("failed to create MemoryManager: %s", err)
	}
	minAddr, ok := p.MinUserAddress().RoundUp()
	if !ok {
		t.Fatalf("MinUserAddress %#x overflows when rounded up", p.MinUserAddress())
	}
	maxAddr := p.MaxUserAddress().RoundDown()
	mm.layout = arch.MmapLayout{
		MinAddr:          minAddr,
		MaxAddr:          maxAddr,
		BottomUpBase:     minAddr,
		TopDownBase:      maxAddr,
		DefaultDirection: mmapDirection,
	}
	return mm
}

func testMemoryManager(ctx context.Context, t testing.TB) *MemoryManager {
	return testMemoryManagerWithMmapDirection(ctx, t, arch.MmapBottomUp)
}

// +checklocksexclude:mm.mappingMu
func (mm *MemoryManager) realUsageAS() uint64 {
	mm.mappingMu.RLock()
	defer mm.mappingMu.RUnlock()
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
	return newRemoteCloneMemoryFileWithOpts(t, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
	})
}

func newRemoteCloneMemoryFileWithOpts(t *testing.T, opts pgalloc.MemoryFileOpts) *pgalloc.MemoryFile {
	t.Helper()
	file, err := os.CreateTemp("", "ttc-mm-remote-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file.Name()); err != nil {
		file.Close()
		t.Fatal(err)
	}
	mf, err := pgalloc.NewMemoryFile(file, opts)
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

// countingMappable counts the calls of its InvalidateUnsavable.
type countingMappable struct {
	*SpecialMappable
	invalidated int
}

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable.
func (m *countingMappable) InvalidateUnsavable(ctx context.Context) error {
	m.invalidated++
	return m.SpecialMappable.InvalidateUnsavable(ctx)
}

func TestUnsavableMappablesInvalidatesEachMappableOnce(t *testing.T) {
	ctx := contexttest.Context(t)
	first := testMemoryManager(ctx, t)
	defer first.DecUsers(ctx)
	second := testMemoryManager(ctx, t)
	defer second.DecUsers(ctx)
	mappable := func(name string) *countingMappable {
		fr, err := first.mf.Allocate(hostarch.PageSize, pgalloc.AllocOpts{})
		if err != nil {
			t.Fatal(err)
		}
		return &countingMappable{SpecialMappable: NewSpecialMappable(name, first.mf, fr)}
	}
	shared, own := mappable("[shared]"), mappable("[own]")
	defer shared.DecRef(ctx)
	defer own.DecRef(ctx)
	mapIn := func(memMgr *MemoryManager, m *countingMappable, times int) {
		for i := 0; i < times; i++ {
			if _, err := memMgr.MMap(ctx, memmap.MMapOpts{
				Length: hostarch.PageSize, MappingIdentity: m.SpecialMappable, Mappable: m,
				Perms: hostarch.Read, MaxPerms: hostarch.Read,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The shared mappable is mapped three times in one and twice in the
	// other, as a binary is by each process that runs it.
	mapIn(first, shared, 3)
	mapIn(second, shared, 2)
	mapIn(second, own, 1)
	refs := shared.ReadRefs()
	mappables := make(UnsavableMappables)
	first.AddUnsavableMappables(mappables)
	second.AddUnsavableMappables(mappables)
	if err := mappables.Invalidate(ctx); err != nil {
		t.Fatal(err)
	}
	if shared.invalidated != 1 || own.invalidated != 1 {
		t.Errorf("invalidated the shared mappable %d times and the other %d, want once each", shared.invalidated, own.invalidated)
	}
	if got := shared.ReadRefs(); got != refs {
		t.Errorf("the shared mappable has %d references after the invalidation, against %d before", got, refs)
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

// hugePageParent returns a MemoryManager, in a MemoryFile whose huge memory is
// 2 MiB pages, that maps a 2 MiB page at the returned address, page n of which
// holds n.
func hugePageParent(t *testing.T) (ctx context.Context, parent *MemoryManager, base hostarch.Addr) {
	t.Helper()
	ctx = contexttest.Context(t)
	mf := newRemoteCloneMemoryFileWithOpts(t, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
		ExpectHugepages:         true,
	})
	t.Cleanup(mf.Destroy)
	ctx = context.WithValues(ctx, map[any]any{pgalloc.CtxMemoryFile: mf})
	parent = testMemoryManager(ctx, t)
	t.Cleanup(func() { parent.DecUsers(ctx) })
	// Two huge pages of address space hold one aligned huge page.
	addr := populatedPrivateMapping(ctx, t, parent, 2*hostarch.HugePageSize)
	base = addr.HugeRoundDown() + hostarch.HugePageSize
	for n := 0; n < hugePagePages; n++ {
		if _, err := parent.CopyOut(ctx, base+hostarch.Addr(n)*hostarch.PageSize, []byte{byte(n), byte(n >> 8)}, usermem.IOOpts{}); err != nil {
			t.Fatalf("writing page %d: %v", n, err)
		}
	}
	if _, ok := parent.mf.HugePageAt(fileOffsetAt(t, parent, base)); !ok {
		t.Fatal("setup: the parent's huge memory is not a 2 MiB page")
	}
	return ctx, parent, base
}

// hugePageFork returns hugePageParent's parent and its live fork, which share
// the 2 MiB page at the returned address.
func hugePageFork(t *testing.T) (ctx context.Context, parent, child *MemoryManager, base hostarch.Addr) {
	t.Helper()
	ctx, parent, base = hugePageParent(t)
	child, err := cloneMemoryManagerMetadata(ctx, parent)
	if err != nil {
		t.Fatalf("cloneMemoryManagerMetadata: %v", err)
	}
	t.Cleanup(func() { child.DecUsers(ctx) })
	return ctx, parent, child, base
}

const hugePagePages = hostarch.HugePageSize / hostarch.PageSize

// fileOffsetAt returns the offset of the page that memMgr maps at addr.
func fileOffsetAt(t testing.TB, memMgr *MemoryManager, addr hostarch.Addr) uint64 {
	t.Helper()
	pseg := memMgr.pmas.FindSegment(addr)
	if !pseg.Ok() {
		t.Fatalf("no pma at %#x", addr)
	}
	return pseg.fileRange().Start + uint64(addr-pseg.Start())
}

// pageVersion returns the identity and version of the page memMgr maps at
// addr.
func pageVersion(t testing.TB, memMgr *MemoryManager, addr hostarch.Addr) (pgalloc.LogicalPageID, pgalloc.PageVersionID) {
	t.Helper()
	id, version, ok := memMgr.mf.LogicalPageVersionAt(fileOffsetAt(t, memMgr, addr))
	if !ok {
		t.Fatalf("no page identity at %#x", addr)
	}
	return id, version
}

// checkHugePageContents checks that page n of the 2 MiB that memMgr maps at
// base holds n.
func checkHugePageContents(ctx context.Context, t testing.TB, memMgr *MemoryManager, base hostarch.Addr) {
	t.Helper()
	for n := 0; n < hugePagePages; n++ {
		buf := make([]byte, 2)
		if _, err := memMgr.CopyIn(ctx, base+hostarch.Addr(n)*hostarch.PageSize, buf, usermem.IOOpts{}); err != nil || buf[0] != byte(n) || buf[1] != byte(n>>8) {
			t.Fatalf("page %d holds %v (err %v), want %v", n, buf, err, []byte{byte(n), byte(n >> 8)})
		}
	}
}

// A write to a 2 MiB page that a fork shares copies it whole into a new 2 MiB
// page, which keeps its identity and takes one new version; the original
// stays one page. The page it then holds alone, its other holder takes over
// whole, writing it in place under one new version.
func TestLiveForkCopiesA2MiBPageWhole(t *testing.T) {
	ctx, parent, child, base := hugePageFork(t)
	block := hostarch.AddrRange{Start: base, End: base + hostarch.HugePageSize}
	parentOffset := fileOffsetAt(t, parent, base)
	parentID, parentVersion := pageVersion(t, parent, base)

	target := base + 7*hostarch.PageSize
	if err := child.HandleUserFault(ctx, target, hostarch.Write, 0); err != nil {
		t.Fatalf("child write fault: %v", err)
	}
	pseg := child.pmas.FindSegment(target)
	if pseg.Range() != block || !pseg.ValuePtr().huge || !hostarch.IsHugePageAligned(pseg.fileRange().Start) {
		t.Fatalf("the child's copy is pma %v (huge %t) at %#x, want one huge pma %v at an aligned offset", pseg.Range(), pseg.ValuePtr().huge, pseg.fileRange().Start, block)
	}
	if hp, ok := child.mf.HugePageAt(pseg.fileRange().Start); !ok || hp != pseg.fileRange() {
		t.Fatalf("the child's copy %v is not one 2 MiB page (%v, %t)", pseg.fileRange(), hp, ok)
	}
	for _, n := range []hostarch.Addr{0, 7, hugePagePages - 1} {
		addr := base + n*hostarch.PageSize
		id, version := pageVersion(t, child, addr)
		wantID, _ := pageVersion(t, parent, addr)
		if id != wantID || sharesPage(t, child, parent, addr) {
			t.Fatalf("the child's page %d is %+v at %+v; want the parent's identity %+v at a new version", n, id, version, wantID)
		}
	}
	checkHugePageContents(ctx, t, child, base)
	if _, ok := parent.mf.HugePageAt(parentOffset); !ok {
		t.Fatal("the parent's page stopped being one 2 MiB page")
	}
	if id, version := pageVersion(t, parent, base); id != parentID || version != parentVersion {
		t.Fatal("the child's copy changed the parent's page")
	}

	// The parent now holds its page alone.
	if err := parent.HandleUserFault(ctx, base+3*hostarch.PageSize, hostarch.Write, 0); err != nil {
		t.Fatalf("parent write fault: %v", err)
	}
	if got := fileOffsetAt(t, parent, base); got != parentOffset {
		t.Fatalf("the parent copied a 2 MiB page it held alone, to %#x from %#x", got, parentOffset)
	}
	if pseg := parent.pmas.FindSegment(base); pseg.ValuePtr().needCOW || !pseg.Range().IsSupersetOf(block) {
		t.Fatalf("the parent's page is in pma %v (needCOW %t), want an owned pma covering %v", pseg.Range(), pseg.ValuePtr().needCOW, block)
	}
	if _, ok := parent.mf.HugePageAt(parentOffset); !ok {
		t.Fatal("taking over a whole 2 MiB page split it")
	}
	if id, version := pageVersion(t, parent, base); id != parentID || version == parentVersion {
		t.Fatalf("the parent's owned page is %+v at %+v, want %+v at a new version", id, version, parentID)
	}
	checkHugePageContents(ctx, t, parent, base)
}

// noRoomAddressSpace refuses mappings, as a host that has no room for another
// (vm.max_map_count) does, until it unmaps some.
type noRoomAddressSpace struct {
	platform.AddressSpace
	full    bool
	refused int
}

// MapFile implements platform.AddressSpace.MapFile.
func (as *noRoomAddressSpace) MapFile(addr hostarch.Addr, f memmap.File, fr memmap.FileRange, at hostarch.AccessType, precommit bool) error {
	if as.full {
		as.refused++
		return fmt.Errorf("stub syscall failed with %w", unix.ENOMEM)
	}
	return as.AddressSpace.MapFile(addr, f, fr, at, precommit)
}

// Unmap implements platform.AddressSpace.Unmap.
func (as *noRoomAddressSpace) Unmap(addr hostarch.Addr, length uint64) {
	as.full = false
	as.AddressSpace.Unmap(addr, length)
}

// A fault on a host that has no room for another mapping, as small, scattered
// pmas leave a host whose vm.max_map_count is low, unmaps the address space,
// which faults then map again, rather than failing.
func TestFaultMapsWhenTheHostRunsOutOfMappings(t *testing.T) {
	ctx := contexttest.Context(t)
	memMgr := testMemoryManager(ctx, t)
	defer memMgr.DecUsers(ctx)
	addr := populatedPrivateMapping(ctx, t, memMgr, hostarch.PageSize)
	as := &noRoomAddressSpace{AddressSpace: memMgr.as, full: true}
	memMgr.as = as
	defer func() { memMgr.as = as.AddressSpace }()
	if err := memMgr.HandleUserFault(ctx, addr, hostarch.Read, 0); err != nil {
		t.Fatalf("a fault with no room for a mapping failed: %v", err)
	}
	if as.refused != 1 || as.full {
		t.Fatalf("the host refused %d mappings and has room %t; want one refusal, then room", as.refused, !as.full)
	}
}

// A remote fork's target installs a 2 MiB page whole, in an aligned 2 MiB
// page that it maps with one huge pma and reads in one piece of the stream,
// and a write to it there copies it whole.
func TestRelocatedDistributedForkKeeps2MiBPages(t *testing.T) {
	ctx, parent, base := hugePageParent(t)
	block := hostarch.AddrRange{Start: base, End: base + hostarch.HugePageSize}
	parent.PrepareDistributedForkCOW()
	capture := pgalloc.NewMemoryRangeCapture()
	if err := parent.CaptureMemoryFileRanges(capture); err != nil {
		t.Fatalf("CaptureMemoryFileRanges: %v", err)
	}
	layout, err := capture.HugePageSnapshotLayout()
	if err != nil {
		t.Fatal(err)
	}
	if layout, err = layout.ChunkOrdered(); err != nil {
		t.Fatal(err)
	}
	bound := false
	for _, binding := range layout.Bindings {
		bound = bound || (binding.Huge && binding.OldOffset == fileOffsetAt(t, parent, base))
	}
	if !bound {
		t.Fatal("the capture does not bind the 2 MiB page whole")
	}
	parent.mf.MarkSavable()
	var metadata bytes.Buffer
	if _, err := state.Save(ctx, &metadata, parent); err != nil {
		t.Fatalf("state.Save: %v", err)
	}
	snapshot, err := pgalloc.NewRemoteSnapshot(layout.Regions)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	targetMF := newRemoteCloneMemoryFileWithOpts(t, pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableIMAWorkAround:    true,
		DisableMemoryAccounting: true,
		ExpectHugepages:         true,
	})
	defer targetMF.Destroy()
	installation, relocation, loads, err := targetMF.InstallRemoteBindings(layout.Bindings, usage.Anonymous, 0)
	if err != nil {
		t.Fatal(err)
	}
	reader := pgalloc.NewLoopbackRemoteReader(snapshot, hostarch.HugePageSize, hugePagePages, 4)
	pages, err := pgalloc.StartAsyncPagesFileLoadWithOpts(reader, nil, nil, pgalloc.AsyncPagesFileLoadOpts{DemandOnly: true, LogicalChunkDemand: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		pages.Cancel()
		_ = pages.Wait()
	}()
	if err := targetMF.AttachSparseChunkPageLoad(pages, loads, nil, nil); err != nil {
		t.Fatal(err)
	}
	pages.MemoryFilesDone()
	references := pgalloc.NewSparseReferenceTransaction()
	loadCtx := context.WithValues(ctx, map[any]any{
		pgalloc.CtxMemoryFile:        targetMF,
		pgalloc.CtxLiveForkClone:     true,
		pgalloc.CtxSparseRelocation:  relocation,
		pgalloc.CtxSparseRestoreRefs: references,
	})
	child := &MemoryManager{}
	if _, err := state.Load(loadCtx, bytes.NewReader(metadata.Bytes()), child); err != nil {
		t.Fatalf("state.Load: %v", err)
	}
	if err := references.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := installation.Commit(); err != nil {
		t.Fatal(err)
	}
	defer child.DecUsers(ctx)

	pseg := child.pmas.FindSegment(base)
	if !pseg.Ok() || !pseg.Range().IsSupersetOf(block) || !pseg.ValuePtr().huge || !targetMF.IsHugePages(pseg.fileRangeOf(block)) {
		t.Fatalf("the target maps the 2 MiB page with pma %v (huge %t) of %v, want one huge pma of a 2 MiB page", pseg.Range(), pseg.ValuePtr().huge, pseg.fileRangeOf(block))
	}
	checkHugePageContents(ctx, t, child, base)
	if err := child.HandleUserFault(ctx, base+7*hostarch.PageSize, hostarch.Write, 0); err != nil {
		t.Fatalf("write fault: %v", err)
	}
	if pseg := child.pmas.FindSegment(base); pseg.Range() != block || pseg.ValuePtr().needCOW || !targetMF.IsHugePages(pseg.fileRange()) {
		t.Fatalf("after a write, the target maps the page with pma %v (needCOW %t) of %v, want a whole 2 MiB page", pseg.Range(), pseg.ValuePtr().needCOW, pseg.fileRange())
	}
	checkHugePageContents(ctx, t, child, base)
}

// A 2 MiB page that mprotect has split between pmas is still one page: a
// write to either part copies it whole, and the copy keeps each part's
// permissions.
func TestLiveForkCopiesA2MiBPageMappedInPiecesWhole(t *testing.T) {
	ctx, parent, child, base := hugePageFork(t)
	half := base + hostarch.HugePageSize/2
	if err := child.MProtect(half, hostarch.HugePageSize/2, hostarch.Read, false); err != nil {
		t.Fatalf("MProtect: %v", err)
	}
	if err := child.HandleUserFault(ctx, base, hostarch.Write, 0); err != nil {
		t.Fatalf("child write fault: %v", err)
	}
	first, second := child.pmas.FindSegment(base), child.pmas.FindSegment(half)
	if first.Range() != (hostarch.AddrRange{Start: base, End: half}) || second.Range() != (hostarch.AddrRange{Start: half, End: base + hostarch.HugePageSize}) {
		t.Fatalf("the child maps its copy with pmas %v and %v, want one for each half", first.Range(), second.Range())
	}
	if first.ValuePtr().needCOW || second.ValuePtr().needCOW || second.fileRange().Start != first.fileRange().End {
		t.Fatalf("the halves map %v and %v (needCOW %t, %t), want one copy", first.fileRange(), second.fileRange(), first.ValuePtr().needCOW, second.ValuePtr().needCOW)
	}
	if !first.ValuePtr().effectivePerms.Write || second.ValuePtr().effectivePerms.Write || second.ValuePtr().maxPerms.Write != first.ValuePtr().maxPerms.Write {
		t.Fatalf("the halves have permissions %v and %v, want the vmas' %v and %v", first.ValuePtr().effectivePerms, second.ValuePtr().effectivePerms, hostarch.ReadWrite, hostarch.Read)
	}
	if hp, ok := child.mf.HugePageAt(first.fileRange().Start); !ok || hp.Start != first.fileRange().Start {
		t.Fatal("the child's copy is not one 2 MiB page")
	}
	if sharesPage(t, child, parent, half) {
		t.Fatal("the read-only half still shares the parent's version")
	}
	checkHugePageContents(ctx, t, child, base)
	checkHugePageContents(ctx, t, parent, base)
}

// A write to a 2 MiB page that the address space maps only in part copies
// only the page written, as Linux copies a base page of a huge page that a
// process maps in part; the original stays one page for those that hold all
// of it, and splits once part of it is freed.
func TestLiveForkCopiesBasePagesOfA2MiBPageMappedInPart(t *testing.T) {
	ctx, parent, child, base := hugePageFork(t)
	parentOffset := fileOffsetAt(t, parent, base)
	half := base + hostarch.HugePageSize/2
	if err := child.MUnmap(ctx, half, hostarch.HugePageSize/2); err != nil {
		t.Fatalf("MUnmap: %v", err)
	}
	if _, ok := parent.mf.HugePageAt(parentOffset); !ok {
		t.Fatal("a 2 MiB page that the parent still holds whole split")
	}
	if err := child.HandleUserFault(ctx, base, hostarch.Write, 0); err != nil {
		t.Fatalf("child write fault: %v", err)
	}
	if got := child.pmas.FindSegment(base).Range().Length(); got != hostarch.PageSize {
		t.Fatalf("a write to a 2 MiB page mapped in part copied %d bytes, want one page", got)
	}
	if !sharesPage(t, child, parent, base+hostarch.PageSize) {
		t.Fatal("the page next to the written one was copied")
	}
	if _, ok := parent.mf.HugePageAt(parentOffset); !ok {
		t.Fatal("copying a base page of a 2 MiB page split it")
	}
	// The parent unmaps the page that only it holds now: the 2 MiB page is
	// freed in part.
	if err := parent.MUnmap(ctx, base, hostarch.PageSize); err != nil {
		t.Fatalf("MUnmap: %v", err)
	}
	if _, ok := parent.mf.HugePageAt(parentOffset + hostarch.PageSize); ok {
		t.Fatal("a 2 MiB page freed in part is still one page")
	}
	if !sharesPage(t, child, parent, base+hostarch.PageSize) {
		t.Fatal("splitting a 2 MiB page changed the versions of its pages")
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
		Length:  2 * pageSize,
		Private: true,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	realUsage := mm.realUsageAS()
	if got := mm.VirtualMemorySize(); got != realUsage {
		t.Fatalf("usageAS believes %v bytes are mapped; %v bytes are actually mapped", got, realUsage)
	}

	mm.MUnmap(ctx, addr, pageSize)
	realUsage = mm.realUsageAS()
	if got := mm.VirtualMemorySize(); got != realUsage {
		t.Fatalf("usageAS believes %v bytes are mapped; %v bytes are actually mapped", got, realUsage)
	}
}

// +checklocksexclude:mm.mappingMu
func (mm *MemoryManager) realDataAS() uint64 {
	mm.mappingMu.RLock()
	defer mm.mappingMu.RUnlock()
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
		Length:   3 * pageSize,
		Private:  true,
		Perms:    hostarch.Write,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	if mm.VirtualDataSize() == 0 {
		t.Fatalf("dataAS is 0, wanted not 0")
	}
	realDataAS := mm.realDataAS()
	if got := mm.VirtualDataSize(); got != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", got, realDataAS)
	}

	mm.MUnmap(ctx, addr, pageSize)
	realDataAS = mm.realDataAS()
	if got := mm.VirtualDataSize(); got != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", got, realDataAS)
	}

	mm.MProtect(addr+pageSizeAddr, pageSize, hostarch.Read, false)
	realDataAS = mm.realDataAS()
	if got := mm.VirtualDataSize(); got != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", got, realDataAS)
	}

	mm.MRemap(ctx, addr+2*pageSizeAddr, pageSize, 2*pageSize, MRemapOpts{
		Move: MRemapMayMove,
	})
	realDataAS = mm.realDataAS()
	if got := mm.VirtualDataSize(); got != realDataAS {
		t.Fatalf("dataAS believes %v bytes are mapped; %v bytes are actually mapped", got, realDataAS)
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
	if newBrk, _ := mm.Brk(ctx, oldBrk+pageSizeAddr); newBrk != oldBrk {
		t.Errorf("brk() increased data segment above RLIMIT_DATA (old brk = %#x, new brk = %#x", oldBrk, newBrk)
	}
}

// TestIOAfterUnmap ensures that IO fails after unmap.
func TestIOAfterUnmap(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   pageSize,
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

	err = mm.MUnmap(ctx, addr, pageSize)
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
		Length:   pageSize,
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

	err = mm.MProtect(addr, pageSize, hostarch.Read, false)
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

// readPinnedRange returns the current contents of the pinned range pr.
func readPinnedRange(t *testing.T, pr PinnedRange) []byte {
	t.Helper()
	ims, err := pr.File.MapInternal(pr.FileRange(), hostarch.Read)
	if err != nil {
		t.Fatalf("MapInternal got err %v want nil", err)
	}
	buf := make([]byte, pr.Source.Length())
	if _, err := safemem.CopySeq(safemem.BlockSeqOf(safemem.BlockFromSafeSlice(buf)), ims); err != nil {
		t.Fatalf("CopySeq got err %v want nil", err)
	}
	return buf
}

// TestPinnedPagesCopiedOnFork tests that Fork copies pinned pages to the
// child immediately instead of making them copy-on-write, so that the
// parent's mappings never diverge from pages registered for DMA.
func TestPinnedPagesCopiedOnFork(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	// Map 3 pages, but pin only the middle page, so that Fork must handle
	// both the pinned page and the copy-on-write pages surrounding it.
	const npages = 3
	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   npages * pageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	preFork := bytes.Repeat([]byte{'A'}, npages*pageSizeInt)
	if _, err := mm.CopyOut(ctx, addr, preFork, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut got err %v want nil", err)
	}

	pinAR := hostarch.AddrRange{
		Start: addr + pageSizeAddr,
		End:   addr + 2*pageSizeAddr,
	}
	prs, err := mm.Pin(ctx, pinAR, hostarch.ReadWrite, false /* ignorePermissions */)
	if err != nil {
		t.Fatalf("Pin got err %v want nil", err)
	}
	defer Unpin(prs)

	// Fork twice so that both the first fork of a pinned pma, and the fork
	// of the resulting parent pma state, are tested.
	for i := 0; i < 2; i++ {
		mm2, err := mm.Fork(ctx)
		if err != nil {
			t.Fatalf("Fork got err %v want nil", err)
		}
		defer mm2.DecUsers(ctx)

		// The parent's writes to the pinned page must be visible through the
		// pinned range, i.e. the parent must not have been moved to a new
		// copy of the page by copy-on-write.
		postFork := bytes.Repeat([]byte{'B' + byte(i)}, npages*pageSizeInt)
		if _, err := mm.CopyOut(ctx, addr, postFork, usermem.IOOpts{}); err != nil {
			t.Fatalf("CopyOut got err %v want nil", err)
		}
		if got, want := readPinnedRange(t, prs[0]), postFork[:hostarch.PageSize]; !bytes.Equal(got, want) {
			t.Errorf("pinned page contains %q..., want %q...; parent's mapping of the pinned page diverged", got[:4], want[:4])
		}

		// The child must see the pre-fork contents of all pages.
		childBuf := make([]byte, npages*hostarch.PageSize)
		if _, err := mm2.CopyIn(ctx, addr, childBuf, usermem.IOOpts{}); err != nil {
			t.Fatalf("CopyIn got err %v want nil", err)
		}
		if !bytes.Equal(childBuf, preFork) {
			t.Errorf("child read %q..., want %q...", childBuf[:4], preFork[:4])
		}

		// The child's writes must not be visible through the pinned range.
		if _, err := mm2.CopyOut(ctx, addr, bytes.Repeat([]byte{'z'}, npages*pageSizeInt), usermem.IOOpts{}); err != nil {
			t.Fatalf("CopyOut got err %v want nil", err)
		}
		if got, want := readPinnedRange(t, prs[0]), postFork[:hostarch.PageSize]; !bytes.Equal(got, want) {
			t.Errorf("pinned page contains %q..., want %q...; child's writes are visible through the pinned range", got[:4], want[:4])
		}

		preFork = postFork
	}
}

// TestPinUnsharesCopyOnWritePages tests that pinning copy-on-write pages
// breaks copy-on-write, so that the pinned pages are those mapped by the
// pinning process, even if the pin does not require write access.
func TestPinUnsharesCopyOnWritePages(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   pageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	preFork := bytes.Repeat([]byte{'A'}, pageSizeInt)
	if _, err := mm.CopyOut(ctx, addr, preFork, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut got err %v want nil", err)
	}

	// Make the page copy-on-write by forking, then pin it for reading only.
	mm2, err := mm.Fork(ctx)
	if err != nil {
		t.Fatalf("Fork got err %v want nil", err)
	}
	defer mm2.DecUsers(ctx)
	ar := hostarch.AddrRange{
		Start: addr,
		End:   addr + pageSizeAddr,
	}
	prs, err := mm.Pin(ctx, ar, hostarch.Read, false /* ignorePermissions */)
	if err != nil {
		t.Fatalf("Pin got err %v want nil", err)
	}
	defer Unpin(prs)

	// The parent's writes must be visible through the pinned range, and must
	// not be visible to the child.
	postFork := bytes.Repeat([]byte{'B'}, pageSizeInt)
	if _, err := mm.CopyOut(ctx, addr, postFork, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut got err %v want nil", err)
	}
	if got := readPinnedRange(t, prs[0]); !bytes.Equal(got, postFork) {
		t.Errorf("pinned page contains %q..., want %q...; pin did not unshare the copy-on-write page", got[:4], postFork[:4])
	}
	childBuf := make([]byte, hostarch.PageSize)
	if _, err := mm2.CopyIn(ctx, addr, childBuf, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyIn got err %v want nil", err)
	}
	if !bytes.Equal(childBuf, preFork) {
		t.Errorf("child read %q..., want %q...", childBuf[:4], preFork[:4])
	}
}

// TestUnpinnedPagesCopyOnWriteAfterFork tests that pages that were pinned but
// have been unpinned are once again made copy-on-write by Fork.
func TestUnpinnedPagesCopyOnWriteAfterFork(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   pageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	if _, err := mm.CopyOut(ctx, addr, bytes.Repeat([]byte{'A'}, pageSizeInt), usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut got err %v want nil", err)
	}

	ar := hostarch.AddrRange{
		Start: addr,
		End:   addr + pageSizeAddr,
	}
	prs, err := mm.Pin(ctx, ar, hostarch.ReadWrite, false /* ignorePermissions */)
	if err != nil {
		t.Fatalf("Pin got err %v want nil", err)
	}
	Unpin(prs)

	mm2, err := mm.Fork(ctx)
	if err != nil {
		t.Fatalf("Fork got err %v want nil", err)
	}
	defer mm2.DecUsers(ctx)

	// Since no pages remain pinned, all pages should be copy-on-write, not
	// copied for the child.
	mm.activeMu.RLock()
	pseg := mm.pmas.FindSegment(addr)
	if !pseg.Ok() {
		mm.activeMu.RUnlock()
		t.Fatalf("no pma for addr %#x", addr)
	}
	needCOW := pseg.ValuePtr().needCOW
	mm.activeMu.RUnlock()
	if !needCOW {
		t.Errorf("pma is not copy-on-write after fork of unpinned page")
	}
}

func TestMRemapMappableOffsetOverflow(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	mf := pgalloc.MemoryFileFromContext(ctx)
	fr, err := mf.Allocate(3*pageSize, pgalloc.AllocOpts{})
	if err != nil {
		t.Fatalf("failed to allocate memory file range: %v", err)
	}
	mappable := NewSpecialMappable("test_mappable", mf, fr)
	defer mappable.DecRef(ctx)

	// Map a page at a high offset (2^64 - 2*PageSize).
	// This mapping itself is valid because offset + length (2^64 - PageSize) does not overflow.
	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:          pageSize,
		MappingIdentity: mappable,
		Mappable:        mappable,
		Offset:          ^uint64(0) - 2*pageSize + 1,
		Private:         true,
		MaxPerms:        hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}

	// 1. A shrink should succeed, even if offset+newSize overflows.
	// We remap with oldSize = 3*PageSize, newSize = 2*PageSize.
	if _, err := mm.MRemap(ctx, addr, 3*pageSize, 2*pageSize, MRemapOpts{}); err != nil {
		t.Errorf("MRemap shrink got err %v want nil", err)
	}

	// 2. A grow should fail if offset+newSize overflows.
	// We remap with oldSize = PageSize, newSize = 3*PageSize.
	if _, err := mm.MRemap(ctx, addr, pageSize, 3*pageSize, MRemapOpts{}); !linuxerr.Equals(linuxerr.EINVAL, err) {
		t.Errorf("MRemap grow got err %v want EINVAL", err)
	}
}

// pmaAt returns the offset into the backing file for the page at addr that is in mm,
// and whether the pma mapping is COW.
func pmaAt(t *testing.T, mm *MemoryManager, addr hostarch.Addr) (uint64, bool) {
	t.Helper()
	mm.activeMu.RLock()
	defer mm.activeMu.RUnlock()
	pseg := mm.pmas.FindSegment(addr)
	if !pseg.Ok() {
		t.Fatalf("no pma for addr %#x", addr)
	}
	fr := pseg.fileRangeOf(hostarch.AddrRange{
		Start: addr,
		End:   addr + pageSizeAddr,
	})
	return fr.Start, pseg.ValuePtr().needCOW
}

// TestForkSkipsDontForkMappings tests that Fork does not give the child
// MADV_DONTFORK mappings or the pages in them, and does not mark those pages
// as COW in the parent.
func TestForkSkipsDontForkMappings(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	// Populate both pages before applying MADV_DONTFORK to only the second, so
	// that Fork must split a pma so it can skip the MADV_DONTFORK mapping.
	const npages = 2
	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   npages * pageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	preFork := bytes.Repeat([]byte{'A'}, npages*pageSizeInt)
	if _, err := mm.CopyOut(ctx, addr, preFork, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut got err %v want nil", err)
	}
	dontForkAddr := addr + pageSizeAddr
	if err := mm.SetDontFork(dontForkAddr, pageSize, true); err != nil {
		t.Fatalf("SetDontFork got err %v want nil", err)
	}

	mm2, err := mm.Fork(ctx)
	if err != nil {
		t.Fatalf("Fork got err %v want nil", err)
	}
	defer mm2.DecUsers(ctx)

	// The child must see the pre-fork contents of the first page, and must not
	// have the second page mapped.
	buf := make([]byte, hostarch.PageSize)
	if _, err := mm2.CopyIn(ctx, addr, buf, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyIn got err %v want nil", err)
	}
	if !bytes.Equal(buf, preFork[:hostarch.PageSize]) {
		t.Errorf("child read %q..., want %q...", buf[:4], preFork[:4])
	}
	if _, err := mm2.CopyIn(ctx, dontForkAddr, buf, usermem.IOOpts{}); !linuxerr.Equals(linuxerr.EFAULT, err) {
		t.Errorf("CopyIn of MADV_DONTFORK page got err %v want EFAULT", err)
	}

	if _, needCOW := pmaAt(t, mm, dontForkAddr); needCOW {
		t.Errorf("parent's MADV_DONTFORK pma is copy-on-write after fork")
	}
}

// testMemoryManagerWithExhaustibleMemory returns a MemoryManager whose memory
// file is backed by a new memfd, and a function that exhausts the memory file
// by sealing the memfd against growth and allocating all of its free pages,
// causing later allocations from it to fail. Since the memory file releases
// freed pages asynchronously, pages freed before calling exhaust may become
// allocatable after it returns, so callers must not free memory from the
// memory file before calling exhaust.
func testMemoryManagerWithExhaustibleMemory(ctx context.Context, t *testing.T) (*MemoryManager, func()) {
	const name = "mm-test-memory"
	memfd, err := memutil.CreateMemFD(name, unix.MFD_ALLOW_SEALING)
	if err != nil {
		t.Fatalf("CreateMemFD got err %v want nil", err)
	}
	memfile := os.NewFile(uintptr(memfd), name)
	mf, err := pgalloc.NewMemoryFile(memfile, pgalloc.MemoryFileOpts{
		DisableMemoryAccounting: true,
	})
	if err != nil {
		memfile.Close()
		t.Fatalf("NewMemoryFile got err %v want nil", err)
	}
	mm := testMemoryManager(ctx, t)
	mm.mf = mf // mm has no mappings yet.

	exhaust := func() {
		t.Helper()
		if _, err := unix.FcntlInt(uintptr(memfd), unix.F_ADD_SEALS, unix.F_SEAL_GROW); err != nil {
			t.Fatalf("fcntl(F_ADD_SEALS, F_SEAL_GROW) got err %v want nil", err)
		}
		var frs []memmap.FileRange
		t.Cleanup(func() {
			for _, fr := range frs {
				mf.DecRef(fr)
			}
		})
		// Since mf can no longer grow, this terminates once all of its free
		// pages are allocated. The allocated pages are never committed.
		for length := uint64(1 << 30); length >= pageSize; length /= 2 {
			for {
				fr, err := mf.Allocate(length, pgalloc.AllocOpts{})
				if err != nil {
					break
				}
				frs = append(frs, fr)
			}
		}
	}
	return mm, exhaust
}

// TestForkCopyFailure tests that if Fork fails to copy pages for the child, it
// releases the references that it took for the child, leaving the parent's
// memory unchanged and usable.
func TestForkCopyFailure(t *testing.T) {
	for _, test := range []struct {
		name string
		// eagerForkCopy is memmap.MMapOpts.EagerForkCopy for the second page.
		eagerForkCopy bool
		// If pin is true, the second page is pinned.
		pin bool
	}{
		{
			name:          "EagerForkCopy",
			eagerForkCopy: true,
		},
		{
			name: "Pinned",
			pin:  true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := contexttest.Context(t)
			// Used to exhaust memory later.
			mm, exhaustMemory := testMemoryManagerWithExhaustibleMemory(ctx, t)
			defer mm.DecUsers(ctx)

			// Map a page that Fork marks COW, followed by a page that
			// Fork must copy, so that Fork fails after taking a reference on
			// the first page for the child. Map the pages separately, rather
			// than replacing part of a larger mapping, since MMap populates
			// small anonymous mappings, and replacing part of one would free
			// memory before exhaustMemory.
			const npages = 2
			opts := memmap.MMapOpts{
				Length:   pageSize,
				Private:  true,
				Perms:    hostarch.ReadWrite,
				MaxPerms: hostarch.AnyAccess,
			}
			addr, err := mm.MMap(ctx, opts)
			if err != nil {
				t.Fatalf("MMap got err %v want nil", err)
			}
			copyAR := hostarch.AddrRange{
				Start: addr + pageSizeAddr,
				End:   addr + npages*pageSizeAddr,
			}
			opts.Addr = copyAR.Start
			opts.Fixed = true
			opts.EagerForkCopy = test.eagerForkCopy
			if _, err := mm.MMap(ctx, opts); err != nil {
				t.Fatalf("MMap got err %v want nil", err)
			}
			if test.pin {
				prs, err := mm.Pin(ctx, copyAR, hostarch.ReadWrite, false /* ignorePermissions */)
				if err != nil {
					t.Fatalf("Pin got err %v want nil", err)
				}
				t.Cleanup(func() { Unpin(prs) })
			}
			preFork := bytes.Repeat([]byte{'A'}, npages*pageSizeInt)
			if _, err := mm.CopyOut(ctx, addr, preFork, usermem.IOOpts{}); err != nil {
				t.Fatalf("CopyOut got err %v want nil", err)
			}

			// Also map a MappingIdentity, on which Fork takes a reference for
			// the child.
			mf := pgalloc.MemoryFileFromContext(ctx)
			fr, err := mf.Allocate(pageSize, pgalloc.AllocOpts{})
			if err != nil {
				t.Fatalf("Allocate got err %v want nil", err)
			}
			mappable := NewSpecialMappable("test_mappable", mf, fr)
			defer mappable.DecRef(ctx)
			if _, err := mm.MMap(ctx, memmap.MMapOpts{
				Length:          pageSize,
				MappingIdentity: mappable,
				Mappable:        mappable,
				Private:         true,
				MaxPerms:        hostarch.AnyAccess,
			}); err != nil {
				t.Fatalf("MMap got err %v want nil", err)
			}
			wantRefs := mappable.ReadRefs()

			// Prevent Fork from allocating memory to copy pages for the child.
			exhaustMemory()
			if mm2, err := mm.Fork(ctx); err == nil {
				mm2.DecUsers(ctx)
				t.Fatalf("Fork got err nil want non-nil")
			}

			if got := mappable.ReadRefs(); got != wantRefs {
				t.Errorf("MappingIdentity has %d references after failed fork, want %d", got, wantRefs)
			}
			off, _ := pmaAt(t, mm, addr)
			if !mm.mf.HasUniqueRef(memmap.FileRange{Start: off, End: off + pageSize}) {
				t.Errorf("parent's copy-on-write page is still shared after failed fork")
			}

			// The parent's memory must be unchanged, and since the parent no
			// longer shares its pages with the child, writing to them must not
			// require allocating memory.
			buf := make([]byte, npages*hostarch.PageSize)
			if _, err := mm.CopyIn(ctx, addr, buf, usermem.IOOpts{}); err != nil {
				t.Fatalf("CopyIn got err %v want nil", err)
			}
			if !bytes.Equal(buf, preFork) {
				t.Errorf("parent read %q..., want %q...", buf[:4], preFork[:4])
			}
			if _, err := mm.CopyOut(ctx, addr, buf, usermem.IOOpts{}); err != nil {
				t.Errorf("CopyOut got err %v want nil", err)
			}
		})
	}
}

// TestEagerForkCopy tests that Fork gives the child a copy of pages in
// EagerForkCopy mappings immediately, so that later writes by the parent do
// not break COW, replacing the pages mapped by the parent.
func TestEagerForkCopy(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	// Mirror the usertrap table: read/execute for the application, but
	// written by the sentry with IgnorePermissions.
	const npages = 2
	addr, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:        npages * pageSize,
		Private:       true,
		Perms:         hostarch.ReadExecute,
		MaxPerms:      hostarch.AnyAccess,
		EagerForkCopy: true,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	ignorePerms := usermem.IOOpts{IgnorePermissions: true}
	preFork := bytes.Repeat([]byte{'A'}, npages*pageSizeInt)
	if _, err := mm.CopyOut(ctx, addr, preFork, ignorePerms); err != nil {
		t.Fatalf("CopyOut got err %v want nil", err)
	}

	offBefore, _ := pmaAt(t, mm, addr)

	// Fork twice to check that the parent's pma remains eligible for eager
	// copying after the first fork.
	for i := 0; i < 2; i++ {
		mm2, err := mm.Fork(ctx)
		if err != nil {
			t.Fatalf("Fork got err %v want nil", err)
		}
		defer mm2.DecUsers(ctx)

		if _, needCOW := pmaAt(t, mm, addr); needCOW {
			t.Errorf("parent pma is copy-on-write after fork")
		}
		childOff, childNeedCOW := pmaAt(t, mm2, addr)
		if childNeedCOW {
			t.Errorf("child pma is copy-on-write after fork")
		}
		if childOff == offBefore {
			t.Errorf("child pma shares memory with the parent at offset %#x", childOff)
		}

		// The parent's writes must not break COW.
		postFork := bytes.Repeat([]byte{'B' + byte(i)}, npages*pageSizeInt)
		if _, err := mm.CopyOut(ctx, addr, postFork, ignorePerms); err != nil {
			t.Fatalf("CopyOut got err %v want nil", err)
		}
		if off, _ := pmaAt(t, mm, addr); off != offBefore {
			t.Errorf("parent pma moved from offset %#x to %#x after write", offBefore, off)
		}

		// The child's view must be unchanged.
		childBuf := make([]byte, npages*hostarch.PageSize)
		if _, err := mm2.CopyIn(ctx, addr, childBuf, usermem.IOOpts{}); err != nil {
			t.Fatalf("CopyIn got err %v want nil", err)
		}
		if !bytes.Equal(childBuf, preFork) {
			t.Errorf("child read %q..., want %q...", childBuf[:4], preFork[:4])
		}
		preFork = postFork
	}
}

// TestEagerForkCopyAdjacentMappings tests that Fork only copies pages in
// EagerForkCopy mappings for the child, and still makes pages in adjacent
// mappings marked COW.
func TestEagerForkCopyAdjacentMappings(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	// Map an EagerForkCopy mapping between two mappings with otherwise
	// identical options, so that only EagerForkCopy distinguishes them.
	const npages = 4
	opts := memmap.MMapOpts{
		Length:   npages * pageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	}
	addr, err := mm.MMap(ctx, opts)
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	eagerAR := hostarch.AddrRange{
		Start: addr + pageSizeAddr,
		End:   addr + (npages-1)*pageSizeAddr,
	}
	opts.Length = uint64(eagerAR.Length())
	opts.Addr = eagerAR.Start
	opts.Fixed = true
	opts.Unmap = true
	opts.EagerForkCopy = true
	if _, err := mm.MMap(ctx, opts); err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	preFork := bytes.Repeat([]byte{'A'}, npages*pageSizeInt)
	if _, err := mm.CopyOut(ctx, addr, preFork, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut got err %v want nil", err)
	}

	mm2, err := mm.Fork(ctx)
	if err != nil {
		t.Fatalf("Fork got err %v want nil", err)
	}
	defer mm2.DecUsers(ctx)

	// Only the parent's pages outside of the EagerForkCopy mapping should
	// have been made copy-on-write.
	for i := 0; i < npages; i++ {
		pageAddr := addr + hostarch.Addr(i)*pageSizeAddr
		wantCOW := !eagerAR.Contains(pageAddr)
		if _, needCOW := pmaAt(t, mm, pageAddr); needCOW != wantCOW {
			t.Errorf("parent pma for page %d: got needCOW %t want %t", i, needCOW, wantCOW)
		}
	}

	// The child must see the pre-fork contents of all pages, and not the
	// parent's post-fork writes.
	postFork := bytes.Repeat([]byte{'B'}, npages*pageSizeInt)
	if _, err := mm.CopyOut(ctx, addr, postFork, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyOut got err %v want nil", err)
	}
	childBuf := make([]byte, npages*hostarch.PageSize)
	if _, err := mm2.CopyIn(ctx, addr, childBuf, usermem.IOOpts{}); err != nil {
		t.Fatalf("CopyIn got err %v want nil", err)
	}
	if !bytes.Equal(childBuf, preFork) {
		t.Errorf("child read %q..., want %q...", childBuf[:4], preFork[:4])
	}
}

// wantError reports a test error if err is not want. op describes the
// operation that returned err.
func wantError(t *testing.T, want *errors.Error, op string, err error) {
	t.Helper()
	if !linuxerr.Equals(want, err) {
		t.Errorf("%s got err %v want %v", op, err, want)
	}
}

// TestSealedMapping tests that application operations that would
// modify a sealed vma fail with EPERM, without modifying other vmas in the
// same range.
func TestSealedMapping(t *testing.T) {
	ctx := contexttest.Context(t)
	mm := testMemoryManager(ctx, t)
	defer mm.DecUsers(ctx)

	// Map three unsealed pages, then replace the middle page with a sealed
	// mapping.
	base, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   3 * pageSize,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	if err != nil {
		t.Fatalf("MMap got err %v want nil", err)
	}
	sealedAddr := base + pageSizeAddr
	if _, err := mm.MMap(ctx, memmap.MMapOpts{
		Length:   pageSize,
		Addr:     sealedAddr,
		Fixed:    true,
		Unmap:    true,
		Private:  true,
		Perms:    hostarch.ReadExecute,
		MaxPerms: hostarch.AnyAccess,
		Sealed:   true,
	}); err != nil {
		t.Fatalf("MMap(Sealed) got err %v want nil", err)
	}
	all := 3 * pageSize

	wantError(t, linuxerr.EPERM, "MUnmap(sealed)", mm.MUnmap(ctx, sealedAddr, pageSize))
	wantError(t, linuxerr.EPERM, "MUnmap(all)", mm.MUnmap(ctx, base, all))
	wantError(t, linuxerr.EPERM, "MProtect(sealed)", mm.MProtect(sealedAddr, pageSize, hostarch.ReadWrite, false))
	wantError(t, linuxerr.EPERM, "MProtect(all)", mm.MProtect(base, all, hostarch.Read, false))
	wantError(t, linuxerr.EPERM, "Decommit(sealed)", mm.Decommit(sealedAddr, pageSize))
	wantError(t, linuxerr.EPERM, "SetDontFork(all)", mm.SetDontFork(base, all, true))
	_, err = mm.MRemap(ctx, sealedAddr, pageSize, 2*pageSize, MRemapOpts{Move: MRemapMayMove})
	wantError(t, linuxerr.EPERM, "MRemap(grow sealed)", err)
	_, err = mm.MRemap(ctx, sealedAddr, 0, pageSize, MRemapOpts{Move: MRemapMayMove})
	wantError(t, linuxerr.EPERM, "MRemap(copy sealed)", err)
	_, err = mm.MRemap(ctx, base, pageSize, pageSize, MRemapOpts{Move: MRemapMustMove, NewAddr: sealedAddr})
	wantError(t, linuxerr.EPERM, "MRemap(move onto sealed)", err)
	_, err = mm.MMap(ctx, memmap.MMapOpts{
		Length:   pageSize,
		Addr:     sealedAddr,
		Fixed:    true,
		Unmap:    true,
		Private:  true,
		Perms:    hostarch.ReadWrite,
		MaxPerms: hostarch.AnyAccess,
	})
	wantError(t, linuxerr.EPERM, "MMap(MAP_FIXED over sealed)", err)

	// None of the above should have modified the unsealed neighbors.
	b := []byte{'x'}
	for _, addr := range []hostarch.Addr{base, base + 2*pageSizeAddr} {
		if _, err := mm.CopyOut(ctx, addr, b, usermem.IOOpts{}); err != nil {
			t.Errorf("CopyOut(%#x) got err %v want nil", addr, err)
		}
	}
	mm.mappingMu.RLock()
	dontfork := mm.vmas.FindSegment(base).ValuePtr().dontfork
	mm.mappingMu.RUnlock()
	if dontfork {
		t.Errorf("unsealed vma became MADV_DONTFORK after failed SetDontFork")
	}

	// madvise operations are refused on sealed vmas, even ones that
	// would be no-ops.
	wantError(t, linuxerr.EPERM, "SetDontFork(sealed, false)", mm.SetDontFork(sealedAddr, pageSize, false))
	wantError(t, linuxerr.EPERM, "SetVMAAnonName(sealed)", mm.SetVMAAnonName(sealedAddr, pageSize, "x", false))

	// Operations that don't touch the sealed vma still work.
	if err := mm.MUnmap(ctx, base, pageSize); err != nil {
		t.Errorf("MUnmap(unsealed neighbor) got err %v want nil", err)
	}

	// Seals are inherited across fork.
	mm2, err := mm.Fork(ctx)
	if err != nil {
		t.Fatalf("Fork got err %v want nil", err)
	}
	defer mm2.DecUsers(ctx)
	wantError(t, linuxerr.EPERM, "child MUnmap(sealed)", mm2.MUnmap(ctx, sealedAddr, pageSize))
}
