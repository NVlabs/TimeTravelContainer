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

package pgalloc

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/checkpoint"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/usage"
)

// InstallRemoteBindings installs or reuses live-transport chunks identified
// by stable logical page versions. Unlike durable object IDs, these keys do
// not claim to hash page contents; their domain prefix keeps the two identity
// schemes distinct. The returned ranges read directly from the capture's
// packed TCP stream.
func (f *MemoryFile) InstallRemoteBindings(bindings []CapturedPageBinding, kind usage.MemoryKind, memCgID uint32) (*SparseChunkInstallation, *SparseRelocation, []RemoteLoadRange, error) {
	type chunkInstance struct {
		id       LogicalChunkID
		instance uint32
	}
	orderedBindings := append([]CapturedPageBinding(nil), bindings...)
	sort.Slice(orderedBindings, func(i, j int) bool { return orderedBindings[i].StreamOffset < orderedBindings[j].StreamOffset })
	byChunk := make(map[chunkInstance][]CapturedPageBinding)
	current := make(map[LogicalChunkID]chunkInstance)
	used := make(map[chunkInstance][4]uint64)
	var chunks []chunkInstance
	for _, binding := range orderedBindings {
		chunk, slot := binding.Page.Chunk()
		if binding.ResourceID.Ok() || binding.Chunk != chunk || binding.Slot != slot || binding.References == 0 {
			return nil, nil, nil, fmt.Errorf("invalid live sparse binding %+v", binding)
		}
		key, ok := current[chunk]
		if !ok {
			key = chunkInstance{id: chunk}
			current[chunk] = key
			chunks = append(chunks, key)
		}
		bitmap := used[key]
		if bitmap[slot/64]&(uint64(1)<<(slot%64)) != 0 {
			key.instance++
			current[chunk] = key
			chunks = append(chunks, key)
			bitmap = used[key]
		}
		bitmap[slot/64] |= uint64(1) << (slot % 64)
		used[key] = bitmap
		byChunk[key] = append(byChunk[key], binding)
	}
	installs := make([]SparseChunkInstall, 0, len(chunks))
	objectBindings := make(map[SparseChunkObjectID][]CapturedPageBinding, len(chunks))
	for _, key := range chunks {
		chunk := key.id
		pages := byChunk[key]
		sort.Slice(pages, func(i, j int) bool { return pages[i].Slot < pages[j].Slot })
		hash := sha256.New()
		hash.Write([]byte("ttc-live-page-versions-v1\x00"))
		hash.Write(chunk.Allocator[:])
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], chunk.Sequence)
		hash.Write(encoded[:])
		install := SparseChunkInstall{Logical: chunk, Kind: kind, MemCgID: memCgID}
		for i, binding := range pages {
			if i != 0 && pages[i-1].Slot == binding.Slot {
				return nil, nil, nil, fmt.Errorf("live sparse chunk repeats slot %d", binding.Slot)
			}
			hash.Write([]byte{binding.Slot})
			hash.Write(binding.Page.Allocator[:])
			binary.BigEndian.PutUint64(encoded[:], binding.Page.Sequence)
			hash.Write(encoded[:])
			hash.Write(binding.Version.Allocator[:])
			binary.BigEndian.PutUint64(encoded[:], binding.Version.Sequence)
			hash.Write(encoded[:])
			install.Pages = append(install.Pages, SparseChunkPage{Slot: binding.Slot, Page: binding.Page, Version: binding.Version, References: binding.References})
		}
		copy(install.Object[:], hash.Sum(nil))
		installs = append(installs, install)
		objectBindings[install.Object] = pages
	}
	transaction, err := f.InstallSparseChunks(installs)
	if err != nil {
		return nil, nil, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = transaction.Abort()
		}
	}()
	relocation := NewSparseRelocation()
	newRanges := transaction.NewRanges()
	var loads []RemoteLoadRange
	for i, install := range installs {
		key := chunks[i]
		chunkLoad := &logicalChunkLoad{id: key.id, instance: key.instance}
		resident, ok := f.SparseChunkRange(install.Object)
		if !ok {
			return nil, nil, nil, fmt.Errorf("installed live chunk %x has no resident range", install.Object)
		}
		_, isNew := newRanges[install.Object]
		for _, binding := range objectBindings[install.Object] {
			target := resident.Start + uint64(binding.Slot)*hostarch.PageSize
			if err := relocation.Add(binding.ResourceID, binding.OldOffset, target); err != nil {
				return nil, nil, nil, err
			}
			if isNew {
				load := RemoteLoadRange{Target: memmap.FileRange{Start: target, End: target + hostarch.PageSize}, StreamOffset: binding.StreamOffset, logicalChunk: chunkLoad}
				loads = append(loads, load)
				chunkLoad.ranges = append(chunkLoad.ranges, load)
			}
		}
	}
	loads = orderAndCoalesceRemoteLoads(loads)
	committed = true
	return transaction, relocation, loads, nil
}

func orderAndCoalesceRemoteLoads(loads []RemoteLoadRange) []RemoteLoadRange {
	// The async reader consumes one contiguous packed-stream interval and may
	// scatter it into discontiguous destination ranges. Existing resident
	// chunks make destination allocation order unrelated to stream order, so
	// sorting by Target would split or interleave atomic logical chunks.
	sort.Slice(loads, func(i, j int) bool { return loads[i].StreamOffset < loads[j].StreamOffset })
	coalesced := loads[:0]
	for _, load := range loads {
		if len(coalesced) != 0 {
			last := &coalesced[len(coalesced)-1]
			if last.logicalChunk == load.logicalChunk && last.Target.End == load.Target.Start && last.StreamOffset+last.Target.Length() == load.StreamOffset {
				last.Target.End = load.Target.End
				continue
			}
		}
		coalesced = append(coalesced, load)
	}
	return coalesced
}

const SparseChunkBytes = logicalPagesPerChunk * hostarch.PageSize

// PagesPerSparseChunk returns the number of base pages in one stable sparse
// residency unit.
func PagesPerSparseChunk() int { return logicalPagesPerChunk }

// SparseChunkObjectID is the content address of one immutable TTC memory
// chunk. pgalloc treats it as opaque to avoid depending on the chunk codec.
type SparseChunkObjectID [32]byte

// SparseChunkPage describes one valid page in a sparse chunk and the number
// of references the restored branch object graph will own.
type SparseChunkPage struct {
	Slot       uint8
	Page       LogicalPageID
	Version    PageVersionID
	References uint64
}

// SparseChunkInstall describes one immutable 1 MiB residency unit.
type SparseChunkInstall struct {
	Object  SparseChunkObjectID
	Logical LogicalChunkID
	Pages   []SparseChunkPage
	Kind    usage.MemoryKind
	MemCgID uint32
}

type sparseResidentChunk struct {
	Range   memmap.FileRange
	Logical LogicalChunkID
	Pages   map[uint8]SparseChunkPage
}

// SparseChunkInstallation is a transactional set of new residency units.
// Restored owners acquire their ordinary page references through state load;
// the installation itself owns only each chunk's cache reference.
type SparseChunkInstallation struct {
	mf        *MemoryFile
	newRanges map[SparseChunkObjectID]memmap.FileRange
	done      bool
}

type sparsePendingReference struct {
	mf      *MemoryFile
	range_  memmap.FileRange
	memCgID uint32
}

// SparseReferenceTransaction stages state-graph references while a shared
// family MemoryFile is being restored. Chunk cache references keep all pages
// allocated until Commit, so a failed state load can discard the transaction
// without modifying allocator counts.
type SparseReferenceTransaction struct {
	references []sparsePendingReference
	// allocations already carry their restored owner's reference. They are
	// retained on commit and released on abort, unlike references which are
	// acquired only at commit time.
	allocations []sparsePendingReference
	done        bool
}

// NewSparseReferenceTransaction creates an empty restore transaction.
func NewSparseReferenceTransaction() *SparseReferenceTransaction {
	return &SparseReferenceTransaction{}
}

// Add stages one reference. It panics on invalid post-relocation ranges in the
// same manner as MemoryFile.IncRef.
func (t *SparseReferenceTransaction) Add(mf *MemoryFile, fr memmap.FileRange, memCgID uint32) {
	if t == nil || t.done || mf == nil || !fr.WellFormed() || fr.Length() == 0 ||
		!hostarch.IsPageAligned(fr.Start) || !hostarch.IsPageAligned(fr.End) {
		panic(fmt.Sprintf("invalid sparse restore reference: mf=%p range=%v", mf, fr))
	}
	t.references = append(t.references, sparsePendingReference{mf: mf, range_: fr, memCgID: memCgID})
}

// AddAllocation adopts an already-allocated range for the restored state
// graph. The allocation's initial reference becomes the object's reference on
// Commit and is released on Abort.
func (t *SparseReferenceTransaction) AddAllocation(mf *MemoryFile, fr memmap.FileRange, memCgID uint32) {
	if t == nil || t.done || mf == nil || !fr.WellFormed() || fr.Length() == 0 ||
		!hostarch.IsPageAligned(fr.Start) || !hostarch.IsPageAligned(fr.End) {
		panic(fmt.Sprintf("invalid sparse restore allocation: mf=%p range=%v", mf, fr))
	}
	t.allocations = append(t.allocations, sparsePendingReference{mf: mf, range_: fr, memCgID: memCgID})
}

// Commit installs every staged reference.
func (t *SparseReferenceTransaction) Commit() error {
	if t == nil || t.done {
		return fmt.Errorf("sparse restore reference transaction is unavailable")
	}
	t.done = true
	for _, reference := range t.references {
		reference.mf.IncRef(reference.range_, reference.memCgID)
	}
	t.references = nil
	t.allocations = nil
	return nil
}

// Abort discards staged references.
func (t *SparseReferenceTransaction) Abort() {
	if t == nil || t.done {
		return
	}
	t.done = true
	t.references = nil
	for _, allocation := range t.allocations {
		allocation.mf.DecRef(allocation.range_)
	}
	t.allocations = nil
}

// NewRanges returns a detached map of chunks that require page loading.
func (i *SparseChunkInstallation) NewRanges() map[SparseChunkObjectID]memmap.FileRange {
	result := make(map[SparseChunkObjectID]memmap.FileRange, len(i.newRanges))
	for object, fr := range i.newRanges {
		result[object] = fr
	}
	return result
}

// Commit retains newly created residency units in the family cache.
func (i *SparseChunkInstallation) Commit() error {
	if i == nil || i.done {
		return fmt.Errorf("sparse chunk installation is unavailable")
	}
	i.done = true
	return nil
}

// Abort releases all references acquired by this installation.
func (i *SparseChunkInstallation) Abort() error {
	if i == nil || i.done {
		return nil
	}
	i.done = true
	for object, fr := range i.newRanges {
		i.mf.mu.Lock()
		delete(i.mf.sparseChunks, object)
		i.mf.mu.Unlock()
		i.mf.DecRef(fr)
	}
	return nil
}

// SparseOffset identifies a page offset in a serialized MemoryFile.
type SparseOffset struct {
	ResourceID checkpoint.ResourceID
	OldOffset  uint64
}

// SparseRelocation maps serialized page offsets to family-MemoryFile offsets.
type SparseRelocation struct {
	pages map[SparseOffset]uint64
}

// NewSparseRelocation creates an empty relocation table.
func NewSparseRelocation() *SparseRelocation {
	return &SparseRelocation{pages: make(map[SparseOffset]uint64)}
}

// Add records one page relocation. Repeated identical mappings are accepted.
func (r *SparseRelocation) Add(resourceID checkpoint.ResourceID, oldOffset, newOffset uint64) error {
	if r == nil || !hostarch.IsPageAligned(oldOffset) || !hostarch.IsPageAligned(newOffset) {
		return fmt.Errorf("invalid sparse page relocation %s:%#x -> %#x", resourceID, oldOffset, newOffset)
	}
	key := SparseOffset{ResourceID: resourceID, OldOffset: oldOffset}
	if previous, ok := r.pages[key]; ok && previous != newOffset {
		return fmt.Errorf("conflicting sparse page relocation %s:%#x -> %#x and %#x", resourceID, oldOffset, previous, newOffset)
	}
	r.pages[key] = newOffset
	return nil
}

// Page returns the relocated page offset.
func (r *SparseRelocation) Page(resourceID checkpoint.ResourceID, oldOffset uint64) (uint64, bool) {
	if r == nil {
		return 0, false
	}
	value, ok := r.pages[SparseOffset{ResourceID: resourceID, OldOffset: oldOffset}]
	return value, ok
}

// Range relocates a contiguous FileRange. It fails closed if any page is
// absent or if the destination pages are not contiguous; callers with
// splittable range sets should use Runs instead.
func (r *SparseRelocation) Range(resourceID checkpoint.ResourceID, old memmap.FileRange) (memmap.FileRange, error) {
	runs, err := r.Runs(resourceID, old)
	if err != nil {
		return memmap.FileRange{}, err
	}
	if len(runs) != 1 {
		return memmap.FileRange{}, fmt.Errorf("sparse range %s:%v relocates to %d discontiguous runs", resourceID, old, len(runs))
	}
	return runs[0], nil
}

// Runs relocates old and splits whenever destination offsets are not adjacent.
func (r *SparseRelocation) Runs(resourceID checkpoint.ResourceID, old memmap.FileRange) ([]memmap.FileRange, error) {
	if !old.WellFormed() || old.Length() == 0 || !hostarch.IsPageAligned(old.Start) || !hostarch.IsPageAligned(old.End) {
		return nil, fmt.Errorf("invalid sparse source range %s:%v", resourceID, old)
	}
	var runs []memmap.FileRange
	for offset := old.Start; offset < old.End; offset += hostarch.PageSize {
		target, ok := r.Page(resourceID, offset)
		if !ok {
			return nil, fmt.Errorf("sparse relocation is missing %s:%#x", resourceID, offset)
		}
		if len(runs) == 0 || runs[len(runs)-1].End != target {
			runs = append(runs, memmap.FileRange{Start: target, End: target + hostarch.PageSize})
		} else {
			runs[len(runs)-1].End += hostarch.PageSize
		}
	}
	return runs, nil
}

// InstallSparseChunks installs or reuses immutable resident chunks. It adds a
// cache reference for each newly installed chunk; restored branch owners stage
// their own references separately through SparseReferenceTransaction. Newly
// installed chunks are zero-filled until their transactional loader supplies
// bytes. The returned ranges identify only new chunks and should be registered
// with a loader before restored state can access them.
func (f *MemoryFile) InstallSparseChunks(installs []SparseChunkInstall) (*SparseChunkInstallation, error) {
	ordered := append([]SparseChunkInstall(nil), installs...)
	newRanges := make(map[SparseChunkObjectID]memmap.FileRange)
	installation := &SparseChunkInstallation{mf: f, newRanges: newRanges}
	rollback := func() {
		_ = installation.Abort()
	}
	seen := make(map[SparseChunkObjectID]struct{}, len(ordered))
	for _, install := range ordered {
		if install.Object == (SparseChunkObjectID{}) || install.Logical.Allocator == (LogicalAllocatorID{}) {
			rollback()
			return nil, fmt.Errorf("invalid sparse chunk object or logical coordinate")
		}
		if _, ok := seen[install.Object]; ok {
			rollback()
			return nil, fmt.Errorf("sparse chunk object %x is repeated", install.Object)
		}
		seen[install.Object] = struct{}{}
		pages := make(map[uint8]SparseChunkPage, len(install.Pages))
		for _, page := range install.Pages {
			chunk, slot := page.Page.Chunk()
			if chunk != install.Logical || slot != page.Slot || page.References == 0 || page.Version.Allocator == (LogicalAllocatorID{}) {
				rollback()
				return nil, fmt.Errorf("invalid sparse chunk page %+v for %+v", page, install.Logical)
			}
			if _, ok := pages[page.Slot]; ok {
				rollback()
				return nil, fmt.Errorf("sparse chunk repeats slot %d", page.Slot)
			}
			pages[page.Slot] = page
		}
		if len(pages) == 0 {
			rollback()
			return nil, fmt.Errorf("sparse chunk object %x has no pages", install.Object)
		}

		f.mu.Lock()
		resident, exists := f.sparseChunks[install.Object]
		f.mu.Unlock()
		if exists {
			if resident.Logical != install.Logical || len(resident.Pages) != len(pages) {
				rollback()
				return nil, fmt.Errorf("resident sparse chunk %x metadata mismatch", install.Object)
			}
			for slot, page := range pages {
				old, ok := resident.Pages[slot]
				if !ok || old.Page != page.Page || old.Version != page.Version {
					rollback()
					return nil, fmt.Errorf("resident sparse chunk %x slot %d identity mismatch", install.Object, slot)
				}
			}
		} else {
			fr, err := f.Allocate(SparseChunkBytes, AllocOpts{Kind: install.Kind, MemCgID: install.MemCgID})
			if err != nil {
				rollback()
				return nil, fmt.Errorf("allocating sparse resident chunk: %w", err)
			}
			resident = sparseResidentChunk{Range: fr, Logical: install.Logical, Pages: pages}
			f.mu.Lock()
			if f.sparseChunks == nil {
				f.sparseChunks = make(map[SparseChunkObjectID]sparseResidentChunk)
			}
			if _, raced := f.sparseChunks[install.Object]; raced {
				f.mu.Unlock()
				f.DecRef(fr)
				rollback()
				return nil, fmt.Errorf("concurrent sparse chunk installation is unsupported")
			}
			for slot, page := range pages {
				pageFR := memmap.FileRange{Start: fr.Start + uint64(slot)*hostarch.PageSize, End: fr.Start + (uint64(slot)+1)*hostarch.PageSize}
				f.logicalPages.RemoveFullRange(pageFR)
				f.logicalPages.InsertRange(pageFR, logicalPageInfo{
					Allocator: page.Page.Allocator, FirstSequence: page.Page.Sequence,
					VersionAllocator: page.Version.Allocator, FirstVersionSequence: page.Version.Sequence,
				})
			}
			f.sparseChunks[install.Object] = resident
			f.mu.Unlock()
			newRanges[install.Object] = fr
		}
	}
	return installation, nil
}

// SparseChunkRange returns the resident range for object.
func (f *MemoryFile) SparseChunkRange(object SparseChunkObjectID) (memmap.FileRange, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	chunk, ok := f.sparseChunks[object]
	return chunk.Range, ok
}

// ReclaimUnusedSparseChunks releases residency units whose cache reference is
// their only remaining reference. It returns the number of units released.
// Reclamation is skipped while any sparse loader is attached: a loader may
// still be filling a range on behalf of a branch that is concurrently exiting.
func (f *MemoryFile) ReclaimUnusedSparseChunks() int {
	f.sparseLoadMu.Lock()
	defer f.sparseLoadMu.Unlock()
	if loads := f.sparseAsyncLoads.Load(); loads != nil && len(*loads) != 0 {
		return 0
	}

	var release []memmap.FileRange
	f.mu.Lock()
	for object, resident := range f.sparseChunks {
		unique := true
		f.forEachChunk(resident.Range, func(chunk *chunkInfo, chunkFR memmap.FileRange) bool {
			unfree := &f.unfreeSmall
			if chunk.huge {
				unfree = &f.unfreeHuge
			}
			unfree.VisitFullRange(chunkFR, func(ufseg unfreeIterator) bool {
				if ufseg.ValuePtr().refs != 1 {
					unique = false
					return false
				}
				return true
			})
			return unique
		})
		if unique {
			delete(f.sparseChunks, object)
			release = append(release, resident.Range)
		}
	}
	f.mu.Unlock()
	for _, fr := range release {
		f.DecRef(fr)
	}
	return len(release)
}
