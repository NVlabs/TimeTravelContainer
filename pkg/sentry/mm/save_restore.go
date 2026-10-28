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
	goContext "context"
	"fmt"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/safemem"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/usage"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
)

type liveForkContextID int

const (
	// CtxLiveForkPMASnapshots maps live-fork state keys to in-memory PMA
	// metadata. It is present only while reconstructing in-process clones.
	CtxLiveForkPMASnapshots liveForkContextID = iota
)

// LiveForkMappingIssue describes one VMA that cannot be cloned safely.
type LiveForkMappingIssue struct {
	AddrRange hostarch.AddrRange
	Resource  string
	Reason    string
}

// ValidateLiveForkMappings rejects special and host/device-backed Mappables
// before live-fork preparation mutates page ownership.
func (mm *MemoryManager) ValidateLiveForkMappings() []LiveForkMappingIssue {
	mm.mappingMu.RLock()
	defer mm.mappingMu.RUnlock()
	var issues []LiveForkMappingIssue
	for vseg := mm.vmas.FirstSegment(); vseg.Ok(); vseg = vseg.NextSegment() {
		vma := vseg.ValuePtr()
		if vma.mappable == nil {
			continue
		}
		if validator, ok := vma.mappable.(memmap.LiveForkMappingValidator); ok {
			if err := validator.ValidateLiveForkMapping(); err != nil {
				issues = append(issues, LiveForkMappingIssue{vseg.Range(), fmt.Sprintf("%T", vma.mappable), err.Error()})
			}
			continue
		}
		if fd, ok := vma.id.(*vfs.FileDescription); ok {
			fsType := fd.Mount().Filesystem().FilesystemType().Name()
			switch fsType {
			case "none", "host", "fuse", "dev", "devtmpfs":
				issues = append(issues, LiveForkMappingIssue{
					vseg.Range(),
					fmt.Sprintf("%T", vma.mappable),
					fmt.Sprintf("mapping identity is backed by unsupported filesystem %q", fsType),
				})
			}
		}
	}
	return issues
}

// InvalidateUnsavable invokes memmap.Mappable.InvalidateUnsavable on all
// Mappables mapped by mm.
//
// +checklocksexclude:mm.mappingMu
func (mm *MemoryManager) InvalidateUnsavable(ctx context.Context) error {
	mappables := make(UnsavableMappables)
	mm.AddUnsavableMappables(mappables)
	return mappables.Invalidate(ctx)
}

// UnsavableMappables is a set of Mappables whose unsavable translations are
// to be invalidated, each with a reference on an identity that keeps it.
type UnsavableMappables map[memmap.Mappable]memmap.MappingIdentity

// AddUnsavableMappables adds the Mappables mm maps to mappables. A Mappable's
// InvalidateUnsavable invalidates its translations in every MemoryManager,
// so a file that every process maps, such as a shell's binary, is
// invalidated once, not once for each of its mappings in each process: a
// live fork of a branch was quadratic in its processes.
func (mm *MemoryManager) AddUnsavableMappables(mappables UnsavableMappables) {
	mm.mappingMu.RLock()
	defer mm.mappingMu.RUnlock()
	for vseg := mm.vmas.FirstSegment(); vseg.Ok(); vseg = vseg.NextSegment() {
		vma := vseg.ValuePtr()
		if vma.mappable == nil {
			continue
		}
		if _, ok := mappables[vma.mappable]; ok {
			continue
		}
		if vma.id != nil {
			vma.id.IncRef()
		}
		mappables[vma.mappable] = vma.id
	}
}

// Invalidate invokes memmap.Mappable.InvalidateUnsavable on each of
// mappables, and drops the references mappables holds.
func (mappables UnsavableMappables) Invalidate(ctx context.Context) error {
	var err error
	for mappable, id := range mappables {
		if err == nil {
			err = mappable.InvalidateUnsavable(ctx)
		}
		if id != nil {
			id.DecRef(ctx)
		}
		delete(mappables, mappable)
	}
	return err
}

// afterLoad is invoked by stateify.
func (mm *MemoryManager) afterLoad(ctx goContext.Context) {
	if p := ctx.Value(platform.CtxPlatform); p != nil {
		mm.p = p.(platform.Platform)
	}
	if mm.p == nil {
		panic("MemoryManager restore context has no platform")
	}
	mm.mf = pgalloc.MemoryFileFromContext(ctx)
	// A restored or forked address space reserves homes of its own; the pages
	// it restores stay where they are, until a fold.
	mm.initHomes()
	mm.fold.foldable = mm.homeOwner != 0
	// A save keeps no fork groups: an address space restored from one stays
	// of group 0, any (see FoldGroup). One that a live fork made took its
	// source's in loadPmas.
	if pgalloc.SparseRelocationFromContext(ctx) != nil {
		for pseg := mm.pmas.FirstSegment(); pseg.Ok(); {
			mf, ok := pseg.ValuePtr().file.(*pgalloc.MemoryFile)
			if !ok {
				panic(fmt.Sprintf("sparse PMA restore has unsupported file %T", pseg.ValuePtr().file))
			}
			runs, err := pgalloc.SparseRelocationFromContext(ctx).Runs(mf.ResourceID(), pseg.fileRange())
			if err != nil {
				panic(fmt.Sprintf("relocating sparse PMA %v: %v", pseg.Range(), err))
			}
			// A PMA segment is allowed to split at restore time: each child
			// retains identical mapping semantics but names one contiguous run
			// in the family MemoryFile. This is required when adjacent source
			// pages reuse independently resident sparse chunks. A run of 2 MiB
			// pages that the address space maps at an aligned address is huge
			// memory, and any other run is not, wherever the source's was.
			current := pseg
			for i, relocated := range runs {
				if i+1 < len(runs) {
					split := current.Start() + hostarch.Addr(relocated.Length())
					left, right := mm.pmas.Split(current, split)
					left.ValuePtr().off = relocated.Start
					left.ValuePtr().huge = left.Range().IsHugePageAligned() && mf.IsHugePages(relocated)
					current = right
				} else {
					current.ValuePtr().off = relocated.Start
					current.ValuePtr().huge = current.Range().IsHugePageAligned() && mf.IsHugePages(relocated)
				}
			}
			pseg = current.NextSegment()
		}
	}
	if pgalloc.LiveForkCloneNeedsPageRefs(ctx) {
		memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
		for pseg := mm.pmas.FirstSegment(); pseg.Ok(); pseg = pseg.NextSegment() {
			switch f := pseg.ValuePtr().file.(type) {
			case *pgalloc.MemoryFile:
				pgalloc.AcquireRestoreRef(ctx, f, pseg.fileRange(), memCgID)
			case memmap.LiveForkSharedFile:
				// The parent's translation of pages that cannot change,
				// which the child shares.
				if !f.LiveForkShared() {
					panic(fmt.Sprintf("restored PMA %v maps mutable pages of %T", pseg.Range(), f))
				}
				f.IncRef(pseg.fileRange(), memCgID)
			default:
				panic(fmt.Sprintf("restored PMA has unsupported file %T", pseg.ValuePtr().file))
			}
		}
	}
	mm.haveASIO = mm.p.SupportsAddressSpaceIO()
	if mm.users.Load() != 0 {
		as, err := mm.p.NewAddressSpace()
		if err != nil {
			panic(fmt.Sprintf("failed to create AddressSpace after restore: %v", err))
		}
		mm.as = as
	}
}

// savePmas is invoked by stateify. Live fork keeps PMA metadata in an
// in-memory snapshot rather than reflecting every resident range through the
// generic wire encoder. Durable checkpoints retain the normal representation,
// but for PMAs of devices' memory, which what loads them does not map (see
// memmap.LiveForkUnsharedFile).
func (mm *MemoryManager) savePmas() []pmaFlatSegment {
	if mm.liveForkPMAKey != 0 {
		return nil
	}
	saved := mm.pmas.ExportSlice()
	kept := saved[:0]
	for _, seg := range saved {
		if _, ok := seg.Value.file.(memmap.LiveForkUnsharedFile); !ok {
			kept = append(kept, seg)
		}
	}
	// State serialization preserves slice capacity. Prevent unused capacity
	// from being encoded as zero-valued PMA segments.
	return kept[:len(kept):len(kept)]
}

// CaptureMemoryFileRanges records the MemoryFile ranges directly referenced
// by mm's serialized PMAs. The Kernel must be paused while this runs.
func (mm *MemoryManager) CaptureMemoryFileRanges(capture *pgalloc.MemoryRangeCapture) error {
	if capture == nil {
		return fmt.Errorf("nil memory range capture")
	}
	// Mappables may own backing independently from PMAs. Record each serialized
	// object once even if it is aliased by multiple VMAs.
	mm.mappingMu.RLock()
	for vseg := mm.vmas.FirstSegment(); vseg.Ok(); vseg = vseg.NextSegment() {
		mappable := vseg.ValuePtr().mappable
		if mappable == nil {
			continue
		}
		provider, ok := mappable.(memmap.MemoryFileRangeProvider)
		if !ok {
			continue
		}
		for _, owned := range provider.MemoryFileRanges() {
			mf, ok := owned.File.(*pgalloc.MemoryFile)
			if !ok {
				mm.mappingMu.RUnlock()
				return fmt.Errorf("serialized Mappable %T owns unsupported memory file %T", mappable, owned.File)
			}
			if err := capture.RecordOwned(mappable, mf, owned.Range); err != nil {
				mm.mappingMu.RUnlock()
				return err
			}
		}
	}
	mm.mappingMu.RUnlock()

	mm.activeMu.RLock()
	defer mm.activeMu.RUnlock()
	for pseg := mm.pmas.FirstSegment(); pseg.Ok(); pseg = pseg.NextSegment() {
		if _, ok := pseg.ValuePtr().file.(memmap.LiveForkUnsharedFile); ok {
			// savePmas leaves it out.
			continue
		}
		mf, ok := pseg.ValuePtr().file.(*pgalloc.MemoryFile)
		if !ok {
			return fmt.Errorf("serializable PMA %v is backed by unsupported file %T", pseg.Range(), pseg.ValuePtr().file)
		}
		if err := capture.Record(mf, pseg.fileRange()); err != nil {
			return err
		}
	}
	return nil
}

// loadPmas is invoked by stateify.
func (mm *MemoryManager) loadPmas(ctx goContext.Context, saved []pmaFlatSegment) {
	if mm.liveForkPMAKey == 0 {
		if err := mm.pmas.ImportSlice(saved); err != nil {
			panic(err)
		}
		return
	}
	snapshots, ok := ctx.Value(CtxLiveForkPMASnapshots).(LiveForkPMASnapshots)
	if !ok {
		panic("live-fork PMA snapshots were not provided")
	}
	snapshot := snapshots[mm.liveForkPMAKey]
	if snapshot == nil {
		panic(fmt.Sprintf("live-fork PMA snapshot %d was not provided", mm.liveForkPMAKey))
	}
	// mm shares its source's pages: they are of one fork group.
	mm.foldGroup = snapshot.group
	pgap := mm.pmas.FirstGap()
	for _, entry := range snapshot.entries {
		pma := entry.pma
		if pgalloc.IsLiveForkRemoteMemoryClone(ctx) {
			if _, ok := pma.file.(*pgalloc.MemoryFile); ok {
				pma.file = pgalloc.MemoryFileFromContext(ctx)
			}
		}
		pgap = mm.pmas.Insert(pgap, entry.ar, pma).NextGap()
	}
	mm.liveForkPMAKey = 0
}

// afterLoad is invoked by stateify.
func (m *SpecialMappable) afterLoad(ctx goContext.Context) {
	m.mf = pgalloc.MemoryFileFromContext(ctx)
	allocated := false
	if relocation := pgalloc.SparseRelocationFromContext(ctx); relocation != nil {
		runs, err := relocation.Runs(m.mf.ResourceID(), m.fr)
		if err != nil {
			panic(fmt.Sprintf("relocating SpecialMappable %q: %v", m.name, err))
		}
		if len(runs) == 1 {
			m.fr = runs[0]
		} else {
			// SpecialMappable's stable state schema represents a single range.
			// Sparse chunk reuse may place adjacent logical pages in multiple
			// physical runs, so materialize only this small special mapping into
			// a contiguous target-owned range. This preserves wire compatibility
			// with already-running Sentries while ordinary application memory
			// remains lazy and page-granular CoW.
			contiguous, err := m.mf.Allocate(m.fr.Length(), pgalloc.AllocOpts{
				Kind: usage.System,
				Mode: pgalloc.AllocateAndCommit,
			})
			if err != nil {
				panic(fmt.Sprintf("allocating relocated SpecialMappable %q: %v", m.name, err))
			}
			transaction, ok := ctx.Value(pgalloc.CtxSparseRestoreRefs).(*pgalloc.SparseReferenceTransaction)
			if !ok || transaction == nil {
				m.mf.DecRef(contiguous)
				panic(fmt.Sprintf("relocating discontiguous SpecialMappable %q without a restore transaction", m.name))
			}
			transaction.AddAllocation(m.mf, contiguous, 0)
			destination, err := m.mf.MapInternal(contiguous, hostarch.Write)
			if err != nil {
				panic(fmt.Sprintf("mapping relocated SpecialMappable %q destination: %v", m.name, err))
			}
			var copied uint64
			for _, run := range runs {
				source, err := m.mf.MapInternal(run, hostarch.Read)
				if err != nil {
					panic(fmt.Sprintf("mapping relocated SpecialMappable %q source: %v", m.name, err))
				}
				n, err := safemem.CopySeq(destination.DropFirst64(copied), source)
				copied += n
				if err != nil || n != run.Length() {
					panic(fmt.Sprintf("copying relocated SpecialMappable %q: copied %d of %d bytes: %v", m.name, n, run.Length(), err))
				}
			}
			m.fr = contiguous
			allocated = true
		}
	}
	if pgalloc.LiveForkCloneNeedsPageRefs(ctx) && !allocated {
		pgalloc.AcquireRestoreRef(ctx, m.mf, m.fr, 0)
	}
}

const (
	vmaRealPermsRead = 1 << iota
	vmaRealPermsWrite
	vmaRealPermsExecute
	vmaEffectivePermsRead
	vmaEffectivePermsWrite
	vmaEffectivePermsExecute
	vmaMaxPermsRead
	vmaMaxPermsWrite
	vmaMaxPermsExecute
	vmaPrivate
	vmaGrowsDown
	vmaIsStack
)

func (v *vma) saveRealPerms() int {
	var b int
	if v.realPerms.Read {
		b |= vmaRealPermsRead
	}
	if v.realPerms.Write {
		b |= vmaRealPermsWrite
	}
	if v.realPerms.Execute {
		b |= vmaRealPermsExecute
	}
	if v.effectivePerms.Read {
		b |= vmaEffectivePermsRead
	}
	if v.effectivePerms.Write {
		b |= vmaEffectivePermsWrite
	}
	if v.effectivePerms.Execute {
		b |= vmaEffectivePermsExecute
	}
	if v.maxPerms.Read {
		b |= vmaMaxPermsRead
	}
	if v.maxPerms.Write {
		b |= vmaMaxPermsWrite
	}
	if v.maxPerms.Execute {
		b |= vmaMaxPermsExecute
	}
	if v.private {
		b |= vmaPrivate
	}
	if v.growsDown {
		b |= vmaGrowsDown
	}
	if v.isStack {
		b |= vmaIsStack
	}
	return b
}

func (v *vma) loadRealPerms(_ goContext.Context, b int) {
	if b&vmaRealPermsRead > 0 {
		v.realPerms.Read = true
	}
	if b&vmaRealPermsWrite > 0 {
		v.realPerms.Write = true
	}
	if b&vmaRealPermsExecute > 0 {
		v.realPerms.Execute = true
	}
	if b&vmaEffectivePermsRead > 0 {
		v.effectivePerms.Read = true
	}
	if b&vmaEffectivePermsWrite > 0 {
		v.effectivePerms.Write = true
	}
	if b&vmaEffectivePermsExecute > 0 {
		v.effectivePerms.Execute = true
	}
	if b&vmaMaxPermsRead > 0 {
		v.maxPerms.Read = true
	}
	if b&vmaMaxPermsWrite > 0 {
		v.maxPerms.Write = true
	}
	if b&vmaMaxPermsExecute > 0 {
		v.maxPerms.Execute = true
	}
	if b&vmaPrivate > 0 {
		v.private = true
	}
	if b&vmaGrowsDown > 0 {
		v.growsDown = true
	}
	if b&vmaIsStack > 0 {
		v.isStack = true
	}
}

func (p *pma) saveFile() checkpoint.ResourceID {
	mf, ok := p.file.(*pgalloc.MemoryFile)
	if !ok {
		// InvalidateUnsavable should have caused all such pmas to be
		// invalidated.
		panic(fmt.Sprintf("Can't save pma with non-MemoryFile of type %T", p.file))
	}
	if !mf.IsSavable() {
		panic(fmt.Sprintf("Can't save pma because its MemoryFile is not savable: %v", mf))
	}
	return mf.ResourceID()
}

func (p *pma) loadFile(ctx goContext.Context, resourceID checkpoint.ResourceID) {
	if !resourceID.Ok() {
		p.file = pgalloc.MemoryFileFromContext(ctx)
		return
	}
	mfmap := pgalloc.MemoryFileMapFromContext(ctx)
	mf, ok := mfmap[resourceID]
	if !ok {
		panic(fmt.Sprintf("can't restore pma because its MemoryFile's resource ID %q was not found in CtxMemoryFileMap", resourceID))
	}
	p.file = mf
}
