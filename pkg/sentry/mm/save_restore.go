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
	goContext "context"
	"fmt"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform"
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
func (mm *MemoryManager) InvalidateUnsavable(ctx context.Context) error {
	mm.mappingMu.RLock()
	defer mm.mappingMu.RUnlock()
	for vseg := mm.vmas.FirstSegment(); vseg.Ok(); vseg = vseg.NextSegment() {
		if vma := vseg.ValuePtr(); vma.mappable != nil {
			if err := vma.mappable.InvalidateUnsavable(ctx); err != nil {
				return err
			}
		}
	}
	return nil
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
	if pgalloc.LiveForkCloneNeedsPageRefs(ctx) {
		memCgID := pgalloc.MemoryCgroupIDFromContext(ctx)
		for pseg := mm.pmas.FirstSegment(); pseg.Ok(); pseg = pseg.NextSegment() {
			pseg.ValuePtr().file.IncRef(pseg.fileRange(), memCgID)
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
// generic wire encoder. Durable checkpoints retain the normal representation.
func (mm *MemoryManager) savePmas() pmaSet {
	if mm.liveForkPMAKey != 0 {
		return pmaSet{}
	}
	return mm.pmas
}

// loadPmas is invoked by stateify.
func (mm *MemoryManager) loadPmas(ctx goContext.Context, saved pmaSet) {
	if mm.liveForkPMAKey == 0 {
		mm.pmas = saved
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
	if pgalloc.LiveForkCloneNeedsPageRefs(ctx) {
		m.mf.IncRef(m.fr, 0)
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
