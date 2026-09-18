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
	"fmt"
	"strings"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

// SpecialMappable implements memmap.MappingIdentity and memmap.Mappable with
// semantics similar to Linux's mm/mmap.c:_install_special_mapping(), except
// that SpecialMappable takes ownership of the memory that it represents
// (_install_special_mapping() does not.)
//
// +stateify savable
type SpecialMappable struct {
	SpecialMappableRefs

	mf *pgalloc.MemoryFile `state:"nosave"`
	// fr is the common single-run representation. frs is used only when a
	// sparse restore places adjacent logical pages in discontiguous MemoryFile
	// ranges. The ranges in frs are ordered by logical mappable offset.
	fr   memmap.FileRange
	frs  []memmap.FileRange
	name string
}

// NewSpecialMappable returns a SpecialMappable that owns fr, which represents
// offsets in mfp.MemoryFile() that contain the SpecialMappable's data. The
// SpecialMappable will use the given name in /proc/[pid]/maps.
//
// Preconditions: fr.Length() != 0.
func NewSpecialMappable(name string, mf *pgalloc.MemoryFile, fr memmap.FileRange) *SpecialMappable {
	m := SpecialMappable{mf: mf, fr: fr, name: name}
	m.InitRefs()
	return &m
}

// DecRef implements refs.RefCounter.DecRef.
func (m *SpecialMappable) DecRef(ctx context.Context) {
	m.SpecialMappableRefs.DecRef(func() {
		for _, fr := range m.fileRanges() {
			m.mf.DecRef(fr)
		}
	})
}

// MappedName implements memmap.MappingIdentity.MappedName.
func (m *SpecialMappable) MappedName(ctx context.Context) string {
	return m.name
}

// DeviceID implements memmap.MappingIdentity.DeviceID.
func (m *SpecialMappable) DeviceID() uint64 {
	return 0
}

// InodeID implements memmap.MappingIdentity.InodeID.
func (m *SpecialMappable) InodeID() uint64 {
	return 0
}

// Msync implements memmap.MappingIdentity.Msync.
func (m *SpecialMappable) Msync(ctx context.Context, mr memmap.MappableRange) error {
	// Linux: vm_file is NULL, causing msync to skip it entirely.
	return nil
}

// AddMapping implements memmap.Mappable.AddMapping.
func (*SpecialMappable) AddMapping(context.Context, memmap.MappingSpace, hostarch.AddrRange, uint64, bool) error {
	return nil
}

// RemoveMapping implements memmap.Mappable.RemoveMapping.
func (*SpecialMappable) RemoveMapping(context.Context, memmap.MappingSpace, hostarch.AddrRange, uint64, bool) {
}

// CopyMapping implements memmap.Mappable.CopyMapping.
func (*SpecialMappable) CopyMapping(context.Context, memmap.MappingSpace, hostarch.AddrRange, hostarch.AddrRange, uint64, bool) error {
	return nil
}

// Translate implements memmap.Mappable.Translate.
func (m *SpecialMappable) Translate(ctx context.Context, required, optional memmap.MappableRange, at hostarch.AccessType) ([]memmap.Translation, error) {
	var err error
	length := m.Length()
	if required.End > length {
		err = &memmap.BusError{linuxerr.EFAULT}
	}
	optional = optional.Intersect(memmap.MappableRange{0, length})
	if optional.Length() == 0 {
		return nil, err
	}
	translations := make([]memmap.Translation, 0, len(m.fileRanges()))
	var logical uint64
	for _, fr := range m.fileRanges() {
		logicalRange := memmap.MappableRange{Start: logical, End: logical + fr.Length()}
		if source := optional.Intersect(logicalRange); source.Length() != 0 {
			translations = append(translations, memmap.Translation{
				Source: source,
				File:   m.mf,
				Offset: fr.Start + source.Start - logical,
				Perms:  hostarch.AnyAccess,
			})
		}
		logical = logicalRange.End
	}
	return translations, err
}

// InvalidateUnsavable implements memmap.Mappable.InvalidateUnsavable.
func (m *SpecialMappable) InvalidateUnsavable(ctx context.Context) error {
	// Since data is stored in pgalloc.MemoryFile, the contents of which are
	// preserved across save/restore, we don't need to do anything.
	return nil
}

// ValidateLiveForkMapping implements memmap.LiveForkMappingValidator.
func (m *SpecialMappable) ValidateLiveForkMapping() error {
	if strings.HasPrefix(m.name, "[kcov:") {
		return fmt.Errorf("KCOV mappings do not yet have branch-local backing")
	}
	return nil
}

// RebindMemoryFileRange transfers m to a replacement MemoryFile range and
// releases the range previously owned by m. The replacement range must carry
// one reference for m.
//
// Preconditions: m is not concurrently accessible.
func (m *SpecialMappable) RebindMemoryFileRange(mf *pgalloc.MemoryFile, fr memmap.FileRange) {
	oldMF, oldRanges := m.mf, m.fileRanges()
	m.mf, m.fr, m.frs = mf, fr, nil
	for _, oldFR := range oldRanges {
		oldMF.DecRef(oldFR)
	}
}

// InvalidateMappingsForMappable drops all cached PMAs and host address-space
// mappings derived from m. The VMAs remain intact, so their next access calls
// m.Translate again. This is required when an otherwise immutable Mappable is
// rebound to different backing while tasks are stopped, as happens when a
// restored live-fork child replaces its host-specific VVAR page.
//
// Preconditions: m is not concurrently rebound.
func (mm *MemoryManager) InvalidateMappingsForMappable(m memmap.Mappable) {
	mm.mappingMu.RLock()
	defer mm.mappingMu.RUnlock()
	mm.activeMu.Lock()
	defer mm.activeMu.Unlock()
	for vseg := mm.vmas.FirstSegment(); vseg.Ok(); vseg = vseg.NextSegment() {
		if vseg.ValuePtr().mappable == m {
			mm.invalidateLocked(vseg.Range(), true, true)
		}
	}
}

// FileRange returns the offsets into m.mf that stores the SpecialMappable's
// contents.
func (m *SpecialMappable) FileRange() memmap.FileRange {
	if len(m.frs) != 0 {
		panic("SpecialMappable has discontiguous MemoryFile ranges")
	}
	return m.fr
}

// MemoryFileRanges implements memmap.MemoryFileRangeProvider.
func (m *SpecialMappable) MemoryFileRanges() []memmap.MemoryFileRange {
	ranges := m.fileRanges()
	result := make([]memmap.MemoryFileRange, 0, len(ranges))
	for _, fr := range ranges {
		result = append(result, memmap.MemoryFileRange{File: m.mf, Range: fr})
	}
	return result
}

// Length returns the length of the SpecialMappable.
func (m *SpecialMappable) Length() uint64 {
	var length uint64
	for _, fr := range m.fileRanges() {
		length += fr.Length()
	}
	return length
}

func (m *SpecialMappable) fileRanges() []memmap.FileRange {
	if len(m.frs) != 0 {
		return m.frs
	}
	return []memmap.FileRange{m.fr}
}
