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

package vmplatform

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

const (
	// GuestFrameSize is the canonical Linux page and stage-1 granule.
	GuestFrameSize uint64 = 4096
	// HostSlabSize is the stage-2 and host allocation unit on Apple Silicon.
	HostSlabSize uint64 = 16384
	// FramesPerSlab is the number of independent Linux pages in one host slab.
	FramesPerSlab = HostSlabSize / GuestFrameSize
)

// MappingRef identifies one stage-1 mapping of a logical frame.
type MappingRef struct {
	AddressSpaceID uint64
	VirtualAddress uint64
}

// FrameRevoker removes all listed stage-1 mappings and acknowledges the
// required TLB invalidation before returning.
type FrameRevoker interface {
	RevokeFrames(context.Context, map[pgalloc.FrameID][]MappingRef) error
}

// FrameRelocation describes a physical move of a stable logical frame. The
// frame ID and every stage-1 virtual address remain unchanged.
type FrameRelocation struct {
	OldGPA     uint64
	NewGPA     uint64
	References []MappingRef
}

// FrameRelocator atomically replaces stage-1 mappings for a set of physical
// frame moves and acknowledges all required TLB invalidations before return.
type FrameRelocator interface {
	RelocateFrames(context.Context, map[pgalloc.FrameID]FrameRelocation) error
}

// GuestRAMConfig configures a GuestRAM execution cache.
type GuestRAMConfig struct {
	VM        VM
	Host      HostMemory
	Revoker   FrameRevoker
	Relocator FrameRelocator
	IPAStart  uint64
	IPASize   uint64
	Rollover  func(*GuestRAM) error
}

// GuestRAMMetrics reports logical Linux usage separately from actual 16 KiB
// host residency and monotonic IPA consumption.
type GuestRAMMetrics struct {
	LogicalBytes      uint64
	HostResidentBytes uint64
	IPABytesConsumed  uint64
	LogicalLimitBytes uint64
	Epoch             uint64
}

type slabKey uint64

type guestFrame struct {
	id           pgalloc.FrameID
	offset       uint64
	backing      *guestBacking
	slot         uint8
	active       bool
	decommitting bool
	mappings     map[MappingRef]struct{}
	slab         *guestSlab
}

type guestSlab struct {
	key         slabKey
	hostAddress uintptr
	gpa         uint64
	releasing   bool
	frames      [FramesPerSlab]*guestFrame
}

type guestBacking struct {
	ram       *GuestRAM
	id        uint64
	size      uint64
	frames    map[uint64]*guestFrame
	destroyed bool
}

// GuestRAM implements pgalloc.BackingProvider over 16 KiB host slabs.
type GuestRAM struct {
	mu sync.Mutex
	// accessMu excludes physical frame relocation, zeroing, and retirement from
	// leased Sentry host mappings. Stage-1 mappings are synchronized
	// independently by the monitor.
	accessMu sync.RWMutex
	// cond wakes backing operations after a frame decommit or slab release.
	// It prevents a logical 4K frame from being reactivated while its old
	// stage-1 translations are still being revoked.
	cond *sync.Cond

	vm        VM
	host      HostMemory
	revoker   FrameRevoker
	relocator FrameRelocator
	rollover  func(*GuestRAM) error

	ipaStart uint64
	ipaSize  uint64
	nextGPA  uint64
	epoch    uint64
	layout   uint64

	nextBackingID uint64
	nextFrameID   pgalloc.FrameID
	nextSlabID    uint64
	backings      map[uint64]*guestBacking
	frames        map[pgalloc.FrameID]*guestFrame
	slabs         map[slabKey]*guestSlab
	// partialSlabs contains exactly the non-releasing slabs with at least one
	// free 4K frame. Full page-cache slabs are deliberately absent so that
	// choosing an allocation target does not become O(total resident memory).
	partialSlabs map[slabKey]*guestSlab
	// allocationSlab caches the densest non-full slab. Allocation increases
	// its occupancy, so it remains optimal until full. Decommit updates or
	// invalidates it while holding mu. This avoids scanning every live slab for
	// every 4K allocation in exec-heavy workloads with a large page cache.
	allocationSlab *guestSlab
	logicalBytes   uint64
	logicalLimit   uint64
}

type guestMappingLease struct {
	ram  *GuestRAM
	once sync.Once
}

func (l *guestMappingLease) Release() {
	if l == nil || l.ram == nil {
		return
	}
	l.once.Do(l.ram.accessMu.RUnlock)
}

// NewGuestRAM constructs an empty allocator.
func NewGuestRAM(config GuestRAMConfig) (*GuestRAM, error) {
	if config.VM == nil || config.Host == nil {
		return nil, fmt.Errorf("GuestRAM requires VM and host memory implementations")
	}
	if config.IPASize < HostSlabSize || config.IPAStart%HostSlabSize != 0 || config.IPASize%HostSlabSize != 0 {
		return nil, fmt.Errorf("invalid IPA range [%#x, %#x)", config.IPAStart, config.IPAStart+config.IPASize)
	}
	if config.IPAStart > ^uint64(0)-config.IPASize {
		return nil, fmt.Errorf("IPA range overflows")
	}
	r := &GuestRAM{
		vm:           config.VM,
		host:         config.Host,
		revoker:      config.Revoker,
		relocator:    config.Relocator,
		rollover:     config.Rollover,
		ipaStart:     config.IPAStart,
		ipaSize:      config.IPASize,
		nextGPA:      config.IPAStart,
		epoch:        1,
		backings:     make(map[uint64]*guestBacking),
		frames:       make(map[pgalloc.FrameID]*guestFrame),
		slabs:        make(map[slabKey]*guestSlab),
		partialSlabs: make(map[slabKey]*guestSlab),
	}
	r.cond = sync.NewCond(&r.mu)
	return r, nil
}

// NewBacking implements pgalloc.BackingProvider.
func (r *GuestRAM) NewBacking() (pgalloc.Backing, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextBackingID++
	b := &guestBacking{
		ram:    r,
		id:     r.nextBackingID,
		frames: make(map[uint64]*guestFrame),
	}
	r.backings[b.id] = b
	return b, nil
}

// SetRollover installs the epoch coordinator after platform construction.
func (r *GuestRAM) SetRollover(rollover func(*GuestRAM) error) {
	r.mu.Lock()
	r.rollover = rollover
	r.mu.Unlock()
}

// SetRevoker installs the monitor-backed stage-1 revoker after the common
// Platform and GuestRAM have been connected.
func (r *GuestRAM) SetRevoker(revoker FrameRevoker) {
	r.mu.Lock()
	r.revoker = revoker
	r.mu.Unlock()
}

// SetRelocator installs the monitor-backed stage-1 relocation coordinator.
func (r *GuestRAM) SetRelocator(relocator FrameRelocator) {
	r.mu.Lock()
	r.relocator = relocator
	r.mu.Unlock()
}

// SetLogicalLimit installs a hard limit over live logical 4 KiB frames. A
// zero limit means unlimited. Reducing the limit below current use is rejected
// so callers never observe a nominal limit that is not actually enforced.
func (r *GuestRAM) SetLogicalLimit(limit uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if limit != 0 && limit < r.logicalBytes {
		return fmt.Errorf("GuestRAM limit %#x is below current logical use %#x", limit, r.logicalBytes)
	}
	r.logicalLimit = limit
	return nil
}

func (b *guestBacking) Grow(oldSize, newSize uint64, _ bool) ([]uintptr, error) {
	b.ram.mu.Lock()
	defer b.ram.mu.Unlock()
	if b.destroyed || oldSize != b.size || newSize < oldSize || oldSize%pgalloc.BackingChunkSize != 0 || newSize%pgalloc.BackingChunkSize != 0 {
		return nil, fmt.Errorf("invalid GuestRAM backing growth from %#x to %#x", oldSize, newSize)
	}
	count := int((newSize - oldSize) / pgalloc.BackingChunkSize)
	b.size = newSize
	// MemoryFile chunks retain their logical size metadata but do not use a
	// stable offset-to-HVA mapping for relocatable GuestRAM. Physical slabs are
	// selected independently below.
	return make([]uintptr, count), nil
}

func (b *guestBacking) Commit(fr memmap.FileRange) error {
	_, err := b.ResolveFrames(fr, hostarch.Write)
	return err
}

func (b *guestBacking) QueryCommitment(fr memmap.FileRange, committed []byte) error {
	if err := validateFrameRange(fr); err != nil {
		return err
	}
	if uint64(len(committed)) != fr.Length()/GuestFrameSize {
		return fmt.Errorf("GuestRAM commitment bitmap has length %d for range %v", len(committed), fr)
	}
	b.ram.mu.Lock()
	defer b.ram.mu.Unlock()
	if b.destroyed || fr.End > b.size {
		return fmt.Errorf("invalid GuestRAM commitment query %v", fr)
	}
	for i, offset := 0, fr.Start; offset < fr.End; i, offset = i+1, offset+GuestFrameSize {
		committed[i] = 0
		if frame := b.frames[offset]; frame != nil && frame.active {
			committed[i] = 1
		}
	}
	return nil
}

func (b *guestBacking) ResolveFrames(fr memmap.FileRange, at hostarch.AccessType) ([]pgalloc.BackingFrame, error) {
	if err := validateFrameRange(fr); err != nil {
		return nil, err
	}
	for {
		b.ram.mu.Lock()
		if b.destroyed || fr.End > b.size {
			b.ram.mu.Unlock()
			return nil, fmt.Errorf("GuestRAM range %v exceeds backing size %#x", fr, b.size)
		}
		blocked := false
		for offset := fr.Start; offset < fr.End; offset += GuestFrameSize {
			if frame := b.frames[offset]; frame != nil {
				if frame.slab.releasing || frame.decommitting {
					b.ram.cond.Wait()
					blocked = true
					break
				}
			}
		}
		if blocked {
			b.ram.mu.Unlock()
			continue
		}
		var newFrames uint64
		for offset := fr.Start; offset < fr.End; offset += GuestFrameSize {
			if b.frames[offset] == nil {
				newFrames++
			}
		}
		freeSlots := b.ram.freeSlotsLocked(b)
		newSlabs := uint64(0)
		if newFrames > freeSlots {
			newSlabs = (newFrames - freeSlots + FramesPerSlab - 1) / FramesPerSlab
		}
		needsRollover := newSlabs != 0 && b.ram.wouldReachRolloverLocked(newSlabs)
		if needsRollover {
			rollover := b.ram.rollover
			epoch := b.ram.epoch
			b.ram.mu.Unlock()
			if rollover == nil {
				return nil, fmt.Errorf("GuestRAM IPA epoch %d reached 75%% rollover threshold", epoch)
			}
			if err := rollover(b.ram); err != nil {
				return nil, fmt.Errorf("rebuilding GuestRAM VM epoch: %w", err)
			}
			b.ram.mu.Lock()
			unchanged := b.ram.epoch == epoch
			b.ram.mu.Unlock()
			if unchanged {
				return nil, fmt.Errorf("GuestRAM rollover did not advance VM epoch %d", epoch)
			}
			continue
		}
		additional := newFrames * GuestFrameSize
		if limit := b.ram.logicalLimit; limit != 0 && (additional > limit || b.ram.logicalBytes > limit-additional) {
			b.ram.mu.Unlock()
			return nil, linuxerr.ENOMEM
		}

		frames := make([]pgalloc.BackingFrame, 0, fr.Length()/GuestFrameSize)
		for offset := fr.Start; offset < fr.End; offset += GuestFrameSize {
			frame := b.frames[offset]
			if frame == nil {
				var err error
				frame, err = b.ram.allocateFrameLocked(b, offset)
				if err != nil {
					b.ram.mu.Unlock()
					return nil, err
				}
			}
			if frame.slab.releasing {
				b.ram.mu.Unlock()
				return nil, fmt.Errorf("GuestRAM frame at offset %#x is being released", offset)
			}
			slot := uint64(frame.slot)
			frames = append(frames, pgalloc.BackingFrame{
				ID:          frame.id,
				FileOffset:  offset,
				GPA:         frame.slab.gpa + slot*GuestFrameSize,
				HostAddress: frame.slab.hostAddress + uintptr(slot*GuestFrameSize),
			})
		}
		b.ram.mu.Unlock()
		return frames, nil
	}
}

func (b *guestBacking) AcquireMapping(fr memmap.FileRange, at hostarch.AccessType) ([]pgalloc.BackingFrame, memmap.InternalMappingLease, error) {
	frames, lease, err := b.AcquireMappings([]pgalloc.BackingMappingRequest{{Range: fr, Access: at}})
	if err != nil {
		return nil, nil, err
	}
	return frames[0], lease, nil
}

// AcquireMappings resolves all requests under one accessMu read acquisition.
// Callers that copy between GuestRAM ranges must not recursively acquire this
// RWMutex: a pending Decommit writer would otherwise wait for the first lease
// while the second acquisition waits behind that writer.
func (b *guestBacking) AcquireMappings(requests []pgalloc.BackingMappingRequest) ([][]pgalloc.BackingFrame, memmap.InternalMappingLease, error) {
	if len(requests) == 0 {
		return nil, nil, nil
	}
	b.ram.accessMu.RLock()
	frames := make([][]pgalloc.BackingFrame, len(requests))
	for i, request := range requests {
		resolved, err := b.ResolveFrames(request.Range, request.Access)
		if err != nil {
			b.ram.accessMu.RUnlock()
			return nil, nil, err
		}
		frames[i] = resolved
	}
	return frames, &guestMappingLease{ram: b.ram}, nil
}

func (r *GuestRAM) wouldReachRolloverLocked(additionalSlabs uint64) bool {
	consumed := r.nextGPA - r.ipaStart
	threshold := r.ipaSize * 3 / 4
	if additionalSlabs > ^uint64(0)/HostSlabSize {
		return true
	}
	additional := additionalSlabs * HostSlabSize
	return additional > threshold || consumed >= threshold-additional
}

func slabOccupancy(slab *guestSlab) uint64 {
	var occupied uint64
	for _, frame := range slab.frames {
		if frame != nil {
			occupied++
		}
	}
	return occupied
}

func betterAllocationSlab(candidate, current *guestSlab) bool {
	if candidate == nil || candidate.releasing {
		return false
	}
	candidateOccupancy := slabOccupancy(candidate)
	if candidateOccupancy >= FramesPerSlab {
		return false
	}
	if current == nil || current.releasing {
		return true
	}
	currentOccupancy := slabOccupancy(current)
	return candidateOccupancy > currentOccupancy ||
		candidateOccupancy == currentOccupancy && candidate.key < current.key
}

func (r *GuestRAM) considerAllocationSlabLocked(candidate *guestSlab) {
	if candidate == nil {
		return
	}
	if candidate.releasing || slabOccupancy(candidate) >= FramesPerSlab {
		delete(r.partialSlabs, candidate.key)
		if r.allocationSlab == candidate {
			r.allocationSlab = nil
		}
		return
	}
	if r.partialSlabs == nil {
		r.partialSlabs = make(map[slabKey]*guestSlab)
	}
	r.partialSlabs[candidate.key] = candidate
	if betterAllocationSlab(candidate, r.allocationSlab) {
		r.allocationSlab = candidate
	}
}

func (r *GuestRAM) selectAllocationSlabLocked() *guestSlab {
	if r.allocationSlab != nil && !r.allocationSlab.releasing && slabOccupancy(r.allocationSlab) < FramesPerSlab {
		return r.allocationSlab
	}
	r.allocationSlab = nil
	for _, candidate := range r.partialSlabs {
		r.considerAllocationSlabLocked(candidate)
	}
	return r.allocationSlab
}

func (r *GuestRAM) freeSlotsLocked(_ *guestBacking) uint64 {
	var free uint64
	for _, slab := range r.partialSlabs {
		for _, frame := range slab.frames {
			if frame == nil {
				free++
			}
		}
	}
	return free
}

func (r *GuestRAM) allocateFrameLocked(backing *guestBacking, offset uint64) (*guestFrame, error) {
	slab := r.selectAllocationSlabLocked()
	if slab == nil {
		var err error
		slab, err = r.allocateSlabLocked()
		if err != nil {
			return nil, err
		}
	}
	var slot uint64
	for ; slot < FramesPerSlab; slot++ {
		if slab.frames[slot] == nil {
			break
		}
	}
	if slot == FramesPerSlab {
		return nil, fmt.Errorf("selected full GuestRAM slab %d", slab.key)
	}
	r.nextFrameID++
	frame := &guestFrame{
		id:       r.nextFrameID,
		offset:   offset,
		backing:  backing,
		slot:     uint8(slot),
		active:   true,
		mappings: make(map[MappingRef]struct{}),
		slab:     slab,
	}
	slab.frames[slot] = frame
	r.considerAllocationSlabLocked(slab)
	backing.frames[offset] = frame
	r.frames[frame.id] = frame
	r.logicalBytes += GuestFrameSize
	return frame, nil
}

func (r *GuestRAM) allocateSlabLocked() (*guestSlab, error) {
	hostAddress, err := r.host.Reserve(HostSlabSize)
	if err != nil {
		return nil, fmt.Errorf("reserving GuestRAM slab: %w", err)
	}
	if uint64(hostAddress)%HostSlabSize != 0 {
		_ = r.host.Release(hostAddress, HostSlabSize)
		return nil, fmt.Errorf("host reservation %#x is not %d-byte aligned", hostAddress, HostSlabSize)
	}
	if err := r.host.Commit(hostAddress, HostSlabSize); err != nil {
		_ = r.host.Release(hostAddress, HostSlabSize)
		return nil, fmt.Errorf("committing GuestRAM slab: %w", err)
	}
	gpa := r.nextGPA
	if err := r.vm.MapStage2(gpa, hostAddress, HostSlabSize, Stage2Read|Stage2Write|Stage2Execute); err != nil {
		_ = r.host.Decommit(hostAddress, HostSlabSize)
		_ = r.host.Release(hostAddress, HostSlabSize)
		return nil, fmt.Errorf("mapping GuestRAM stage-2 slab: %w", err)
	}
	r.nextGPA += HostSlabSize
	r.nextSlabID++
	slab := &guestSlab{
		key:         slabKey(r.nextSlabID),
		hostAddress: hostAddress,
		gpa:         gpa,
	}
	r.slabs[slab.key] = slab
	r.considerAllocationSlabLocked(slab)
	return slab, nil
}

func (b *guestBacking) Decommit(fr memmap.FileRange) error {
	if err := validateFrameRange(fr); err != nil {
		return err
	}
	// Decommit may zero a frame and release its containing host mapping. Drain
	// every operation-scoped Sentry mapping before changing either the bytes or
	// the lifetime of those host addresses.
	b.ram.accessMu.Lock()
	defer b.ram.accessMu.Unlock()
	b.ram.mu.Lock()
	for {
		if b.destroyed || fr.End > b.size {
			b.ram.mu.Unlock()
			return fmt.Errorf("invalid GuestRAM decommit %v", fr)
		}
		blocked := false
		for offset := fr.Start; offset < fr.End; offset += GuestFrameSize {
			if frame := b.frames[offset]; frame != nil {
				if frame.slab.releasing || frame.decommitting {
					b.ram.cond.Wait()
					blocked = true
					break
				}
			}
		}
		if !blocked {
			break
		}
	}
	touched := make(map[*guestSlab]struct{})
	var deactivated []*guestFrame
	refs := make(map[pgalloc.FrameID][]MappingRef)
	for offset := fr.Start; offset < fr.End; offset += GuestFrameSize {
		frame := b.frames[offset]
		if frame == nil {
			continue
		}
		if frame.active {
			frame.active = false
			frame.decommitting = true
			b.ram.logicalBytes -= GuestFrameSize
			deactivated = append(deactivated, frame)
			for ref := range frame.mappings {
				refs[frame.id] = append(refs[frame.id], ref)
			}
		}
		touched[frame.slab] = struct{}{}
	}
	b.ram.mu.Unlock()

	// A 4K frame may be reused while one of its 16K slab neighbors remains
	// live. Revoke and acknowledge every old stage-1 reference before zeroing
	// or reactivating that frame; waiting only for whole-slab reclamation leaves
	// a stale PTE/TLB entry pointing at repurposed data.
	if len(refs) != 0 {
		if b.ram.revoker == nil {
			b.ram.mu.Lock()
			for _, frame := range deactivated {
				frame.active = true
				frame.decommitting = false
				b.ram.logicalBytes += GuestFrameSize
			}
			b.ram.cond.Broadcast()
			b.ram.mu.Unlock()
			return fmt.Errorf("GuestRAM frames still have stage-1 mappings and no revoker")
		}
		if err := b.ram.revoker.RevokeFrames(context.Background(), refs); err != nil {
			b.ram.mu.Lock()
			for _, frame := range deactivated {
				frame.active = true
				frame.decommitting = false
				b.ram.logicalBytes += GuestFrameSize
			}
			b.ram.cond.Broadcast()
			b.ram.mu.Unlock()
			return err
		}
	}

	b.ram.mu.Lock()
	for _, frame := range deactivated {
		if err := b.ram.host.Zero(frame.slab.hostAddress+uintptr(frame.slot)*uintptr(GuestFrameSize), GuestFrameSize); err != nil {
			for _, rollback := range deactivated {
				rollback.active = true
				rollback.decommitting = false
				b.ram.logicalBytes += GuestFrameSize
			}
			b.ram.cond.Broadcast()
			b.ram.mu.Unlock()
			return err
		}
	}
	selectedTouched := false
	for _, frame := range deactivated {
		for _, ref := range refs[frame.id] {
			delete(frame.mappings, ref)
		}
		if frame.slab == b.ram.allocationSlab {
			selectedTouched = true
		}
		frame.slab.frames[frame.slot] = nil
		delete(frame.backing.frames, frame.offset)
		delete(b.ram.frames, frame.id)
	}
	var releases []slabRelease
	if selectedTouched {
		b.ram.allocationSlab = nil
	}
	for slab := range touched {
		b.ram.considerAllocationSlabLocked(slab)
	}
	for slab := range touched {
		if slabEmpty(slab) && !slab.releasing {
			slab.releasing = true
			b.ram.considerAllocationSlabLocked(slab)
			releases = append(releases, slabRelease{slab: slab, refs: slabRefs(slab)})
		}
	}
	for _, frame := range deactivated {
		frame.decommitting = false
	}
	b.ram.cond.Broadcast()
	b.ram.mu.Unlock()
	return b.ram.releaseSlabs(releases)
}

type slabRelease struct {
	slab *guestSlab
	refs map[pgalloc.FrameID][]MappingRef
}

func slabEmpty(slab *guestSlab) bool {
	for _, frame := range slab.frames {
		if frame != nil {
			return false
		}
	}
	return true
}

func slabRefs(slab *guestSlab) map[pgalloc.FrameID][]MappingRef {
	refs := make(map[pgalloc.FrameID][]MappingRef)
	for _, frame := range slab.frames {
		if frame == nil {
			continue
		}
		for ref := range frame.mappings {
			refs[frame.id] = append(refs[frame.id], ref)
		}
	}
	return refs
}

// releaseSlabs revokes and releases empty slabs, coalescing adjacent GPAs into
// a single stage-2 unmap. Host reservations remain independent because their
// virtual addresses need not be contiguous.
func (r *GuestRAM) releaseSlabs(releases []slabRelease) error {
	if len(releases) == 0 {
		return nil
	}

	refs := make(map[pgalloc.FrameID][]MappingRef)
	for _, release := range releases {
		for id, mappings := range release.refs {
			refs[id] = append(refs[id], mappings...)
		}
	}
	if len(refs) != 0 {
		if r.revoker == nil {
			r.mu.Lock()
			for _, release := range releases {
				release.slab.releasing = false
				r.considerAllocationSlabLocked(release.slab)
			}
			r.cond.Broadcast()
			r.mu.Unlock()
			return fmt.Errorf("GuestRAM slab still has stage-1 mappings and no revoker")
		}
		if err := r.revoker.RevokeFrames(context.Background(), refs); err != nil {
			r.mu.Lock()
			for _, release := range releases {
				release.slab.releasing = false
				r.considerAllocationSlabLocked(release.slab)
			}
			r.cond.Broadcast()
			r.mu.Unlock()
			return err
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	for _, release := range releases {
		slab := release.slab
		if r.slabs[slab.key] != slab || !slab.releasing || !slabEmpty(slab) {
			for _, rollback := range releases {
				rollback.slab.releasing = false
				r.considerAllocationSlabLocked(rollback.slab)
			}
			r.cond.Broadcast()
			return fmt.Errorf("GuestRAM slab changed during release")
		}
	}
	sort.Slice(releases, func(i, j int) bool {
		return releases[i].slab.gpa < releases[j].slab.gpa
	})

	clearReleasing := func(releases []slabRelease) {
		for _, release := range releases {
			release.slab.releasing = false
			r.considerAllocationSlabLocked(release.slab)
		}
	}
	remap := func(releases []slabRelease) error {
		var firstErr error
		for _, release := range releases {
			slab := release.slab
			if err := r.vm.MapStage2(slab.gpa, slab.hostAddress, HostSlabSize, Stage2Read|Stage2Write|Stage2Execute); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			slab.releasing = false
			r.considerAllocationSlabLocked(slab)
		}
		return firstErr
	}

	for groupStart := 0; groupStart < len(releases); {
		groupEnd := groupStart + 1
		for groupEnd < len(releases) && releases[groupEnd].slab.gpa == releases[groupEnd-1].slab.gpa+HostSlabSize {
			groupEnd++
		}
		group := releases[groupStart:groupEnd]
		if err := r.vm.UnmapStage2(group[0].slab.gpa, uint64(len(group))*HostSlabSize); err != nil {
			clearReleasing(releases[groupStart:])
			r.cond.Broadcast()
			return err
		}

		for i, release := range group {
			slab := release.slab
			if err := r.host.Decommit(slab.hostAddress, HostSlabSize); err != nil {
				remapErr := remap(group[i:])
				clearReleasing(releases[groupEnd:])
				r.cond.Broadcast()
				if remapErr != nil {
					return fmt.Errorf("decommitting GuestRAM slab: %v; restoring stage-2 mappings: %w", err, remapErr)
				}
				return err
			}
			if err := r.host.Release(slab.hostAddress, HostSlabSize); err != nil {
				commitErr := r.host.Commit(slab.hostAddress, HostSlabSize)
				var remapErr error
				if commitErr == nil {
					remapErr = remap(group[i:])
				} else {
					// The current reservation is still decommitted, so only
					// restore later slabs from the coalesced stage-2 range.
					remapErr = remap(group[i+1:])
				}
				clearReleasing(releases[groupEnd:])
				r.cond.Broadcast()
				if commitErr != nil {
					return fmt.Errorf("releasing GuestRAM slab host reservation: %v; recommitting host memory: %w", err, commitErr)
				}
				if remapErr != nil {
					return fmt.Errorf("releasing GuestRAM slab host reservation: %v; restoring stage-2 mappings: %w", err, remapErr)
				}
				return fmt.Errorf("releasing GuestRAM slab host reservation: %w", err)
			}
			delete(r.slabs, slab.key)
			delete(r.partialSlabs, slab.key)
			if r.allocationSlab == slab {
				r.allocationSlab = nil
			}
		}
		groupStart = groupEnd
	}
	r.cond.Broadcast()
	return nil
}

// AddMapping records a reverse mapping for compaction, reclamation, and epoch
// root reconstruction.
func (r *GuestRAM) AddMapping(frameID pgalloc.FrameID, ref MappingRef) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	frame := r.frames[frameID]
	if frame == nil || !frame.active {
		return fmt.Errorf("unknown or inactive GuestRAM frame %d", frameID)
	}
	frame.mappings[ref] = struct{}{}
	return nil
}

// RemoveMapping removes a reverse mapping.
func (r *GuestRAM) RemoveMapping(frameID pgalloc.FrameID, ref MappingRef) {
	r.mu.Lock()
	if frame := r.frames[frameID]; frame != nil {
		delete(frame.mappings, ref)
	}
	r.mu.Unlock()
}

// GuestRAMCompaction reports the effect of one stop-the-world compaction.
type GuestRAMCompaction struct {
	FramesMoved   uint64
	SlabsReleased uint64
	BytesCopied   uint64
}

type frameMove struct {
	frame       *guestFrame
	source      *guestSlab
	destination *guestSlab
	sourceSlot  uint8
	destSlot    uint8
}

// Compact packs live frames into the minimum number of slabs within each
// MemoryFile backing. The caller must have stopped application execution and
// discarded all unleased PMA host mappings. accessMu then drains every
// operation-scoped host mapping before any bytes or stage-1 PTEs move.
func (r *GuestRAM) Compact(ctx context.Context) (GuestRAMCompaction, error) {
	r.accessMu.Lock()
	defer r.accessMu.Unlock()

	r.mu.Lock()
	for _, slab := range r.slabs {
		if slab.releasing {
			r.mu.Unlock()
			return GuestRAMCompaction{}, fmt.Errorf("GuestRAM slab release is in progress")
		}
		for _, frame := range slab.frames {
			if frame != nil && frame.decommitting {
				r.mu.Unlock()
				return GuestRAMCompaction{}, fmt.Errorf("GuestRAM frame decommit is in progress")
			}
		}
	}

	var moves []frameMove
	slabs := make([]*guestSlab, 0, len(r.slabs))
	var live uint64
	for _, slab := range r.slabs {
		occupancy := slabOccupancy(slab)
		if occupancy == 0 {
			continue
		}
		live += occupancy
		slabs = append(slabs, slab)
	}
	keep := int((live + FramesPerSlab - 1) / FramesPerSlab)
	if keep == 0 || keep >= len(slabs) {
		r.mu.Unlock()
		return GuestRAMCompaction{}, nil
	}
	sort.Slice(slabs, func(i, j int) bool {
		oi, oj := slabOccupancy(slabs[i]), slabOccupancy(slabs[j])
		if oi != oj {
			return oi > oj
		}
		return slabs[i].key < slabs[j].key
	})
	destinations := slabs[:keep]
	destIndex, destSlot := 0, uint8(0)
	for _, source := range slabs[keep:] {
		for sourceSlot, frame := range source.frames {
			if frame == nil {
				continue
			}
			for destIndex < len(destinations) {
				dest := destinations[destIndex]
				for destSlot < uint8(FramesPerSlab) && dest.frames[destSlot] != nil {
					destSlot++
				}
				if destSlot < uint8(FramesPerSlab) {
					break
				}
				destIndex++
				destSlot = 0
			}
			if destIndex == len(destinations) {
				r.mu.Unlock()
				return GuestRAMCompaction{}, fmt.Errorf("GuestRAM compaction capacity accounting failed")
			}
			moves = append(moves, frameMove{
				frame:       frame,
				source:      source,
				destination: destinations[destIndex],
				sourceSlot:  uint8(sourceSlot),
				destSlot:    destSlot,
			})
			destSlot++
		}
	}

	if len(moves) == 0 {
		r.mu.Unlock()
		return GuestRAMCompaction{}, nil
	}
	relocations := make(map[pgalloc.FrameID]FrameRelocation, len(moves))
	for i := range moves {
		move := &moves[i]
		move.frame.decommitting = true
		references := make([]MappingRef, 0, len(move.frame.mappings))
		for ref := range move.frame.mappings {
			references = append(references, ref)
		}
		sort.Slice(references, func(i, j int) bool {
			if references[i].AddressSpaceID != references[j].AddressSpaceID {
				return references[i].AddressSpaceID < references[j].AddressSpaceID
			}
			return references[i].VirtualAddress < references[j].VirtualAddress
		})
		relocations[move.frame.id] = FrameRelocation{
			OldGPA:     move.source.gpa + uint64(move.sourceSlot)*GuestFrameSize,
			NewGPA:     move.destination.gpa + uint64(move.destSlot)*GuestFrameSize,
			References: references,
		}
	}
	rollback := func() {
		for i := range moves {
			moves[i].frame.decommitting = false
		}
		r.cond.Broadcast()
		r.mu.Unlock()
	}
	for _, move := range moves {
		dst := move.destination.hostAddress + uintptr(move.destSlot)*uintptr(GuestFrameSize)
		src := move.source.hostAddress + uintptr(move.sourceSlot)*uintptr(GuestFrameSize)
		if err := r.host.Copy(dst, src, GuestFrameSize); err != nil {
			rollback()
			return GuestRAMCompaction{}, fmt.Errorf("copying GuestRAM frame %d: %w", move.frame.id, err)
		}
	}
	if r.relocator == nil {
		for _, relocation := range relocations {
			if len(relocation.References) != 0 {
				rollback()
				return GuestRAMCompaction{}, fmt.Errorf("GuestRAM compaction has stage-1 mappings and no relocator")
			}
		}
	} else if err := r.relocator.RelocateFrames(ctx, relocations); err != nil {
		rollback()
		return GuestRAMCompaction{}, fmt.Errorf("relocating GuestRAM stage-1 mappings: %w", err)
	}

	sources := make(map[*guestSlab]struct{})
	for _, move := range moves {
		move.source.frames[move.sourceSlot] = nil
		move.destination.frames[move.destSlot] = move.frame
		move.frame.slab = move.destination
		move.frame.slot = move.destSlot
		move.frame.decommitting = false
		sources[move.source] = struct{}{}
	}
	r.layout++
	releases := make([]slabRelease, 0, len(sources))
	for source := range sources {
		if slabEmpty(source) {
			source.releasing = true
			releases = append(releases, slabRelease{slab: source})
		}
	}
	r.allocationSlab = nil
	r.partialSlabs = make(map[slabKey]*guestSlab)
	for _, slab := range r.slabs {
		r.considerAllocationSlabLocked(slab)
	}
	r.cond.Broadcast()
	r.mu.Unlock()

	result := GuestRAMCompaction{FramesMoved: uint64(len(moves)), BytesCopied: uint64(len(moves)) * GuestFrameSize}
	if err := r.releaseSlabs(releases); err != nil {
		return result, fmt.Errorf("releasing compacted GuestRAM slabs: %w", err)
	}
	result.SlabsReleased = uint64(len(releases))
	return result, nil
}

func (r *GuestRAM) layoutGeneration() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.layout
}

// RebindStage2 installs all live slabs into a fresh VM epoch. The platform
// coordinator stops workers and rebuilds monitor roots from reverse mappings
// around this call.
func (r *GuestRAM) RebindStage2(vm VM) error {
	plan, err := r.beginEpochRebind(vm)
	if err != nil {
		return err
	}
	return plan.commit()
}

type epochSlabBinding struct {
	slab *guestSlab
	gpa  uint64
}

// epochRebindPlan holds GuestRAM's mutex from construction through commit or
// abort. This lets the platform build every replacement stage-1 root from a
// stable frame-to-GPA snapshot before publishing the new execution epoch.
type epochRebindPlan struct {
	ram      *GuestRAM
	vm       VM
	bindings []epochSlabBinding
	frames   map[pgalloc.FrameID]uint64
	done     bool
}

func (r *GuestRAM) beginEpochRebind(vm VM) (*epochRebindPlan, error) {
	if vm == nil {
		return nil, fmt.Errorf("nil replacement VM")
	}
	r.mu.Lock()
	for {
		blocked := false
		for _, slab := range r.slabs {
			if slab.releasing {
				blocked = true
				break
			}
			for _, frame := range slab.frames {
				if frame != nil && frame.decommitting {
					blocked = true
					break
				}
			}
			if blocked {
				break
			}
		}
		if !blocked {
			break
		}
		r.cond.Wait()
	}
	keys := make([]slabKey, 0, len(r.slabs))
	for key := range r.slabs {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i] < keys[j]
	})
	if uint64(len(keys))*HostSlabSize >= r.ipaSize*3/4 {
		r.mu.Unlock()
		return nil, fmt.Errorf("live GuestRAM slabs do not fit below IPA rollover threshold")
	}
	plan := &epochRebindPlan{
		ram:      r,
		vm:       vm,
		bindings: make([]epochSlabBinding, 0, len(keys)),
		frames:   make(map[pgalloc.FrameID]uint64, len(keys)*int(FramesPerSlab)),
	}
	nextGPA := r.ipaStart
	for _, key := range keys {
		slab := r.slabs[key]
		if err := vm.MapStage2(nextGPA, slab.hostAddress, HostSlabSize, Stage2Read|Stage2Write|Stage2Execute); err != nil {
			for i := len(plan.bindings) - 1; i >= 0; i-- {
				_ = vm.UnmapStage2(plan.bindings[i].gpa, HostSlabSize)
			}
			r.mu.Unlock()
			return nil, fmt.Errorf("remapping GuestRAM slab in new epoch: %w", err)
		}
		plan.bindings = append(plan.bindings, epochSlabBinding{slab: slab, gpa: nextGPA})
		for i, frame := range slab.frames {
			if frame == nil {
				continue
			}
			plan.frames[frame.id] = nextGPA + uint64(i)*GuestFrameSize
		}
		nextGPA += HostSlabSize
	}
	return plan, nil
}

func (p *epochRebindPlan) frameGPA(id pgalloc.FrameID) (uint64, bool) {
	gpa, ok := p.frames[id]
	return gpa, ok
}

// +checklocksignore: beginEpochRebind transfers ram.mu ownership to this plan.
func (p *epochRebindPlan) commit() error {
	if p == nil || p.done {
		return fmt.Errorf("GuestRAM epoch plan is already completed")
	}
	r := p.ram
	for _, binding := range p.bindings {
		binding.slab.gpa = binding.gpa
	}
	r.vm = p.vm
	r.nextGPA = r.ipaStart + uint64(len(p.bindings))*HostSlabSize
	r.epoch++
	p.done = true
	r.mu.Unlock()
	return nil
}

// +checklocksignore: beginEpochRebind transfers ram.mu ownership to this plan.
func (p *epochRebindPlan) abort() error {
	if p == nil || p.done {
		return fmt.Errorf("GuestRAM epoch plan is already completed")
	}
	var firstErr error
	for i := len(p.bindings) - 1; i >= 0; i-- {
		if err := p.vm.UnmapStage2(p.bindings[i].gpa, HostSlabSize); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	p.done = true
	p.ram.mu.Unlock()
	return firstErr
}

// Metrics returns a consistent allocator snapshot.
func (r *GuestRAM) Metrics() GuestRAMMetrics {
	r.mu.Lock()
	defer r.mu.Unlock()
	return GuestRAMMetrics{
		LogicalBytes:      r.logicalBytes,
		HostResidentBytes: uint64(len(r.slabs)) * HostSlabSize,
		IPABytesConsumed:  r.nextGPA - r.ipaStart,
		LogicalLimitBytes: r.logicalLimit,
		Epoch:             r.epoch,
	}
}

func (b *guestBacking) TotalUsage() (uint64, error) {
	b.ram.mu.Lock()
	defer b.ram.mu.Unlock()
	return uint64(len(b.frames)) * GuestFrameSize, nil
}

func (*guestBacking) Sync() error { return nil }

// DropCache retains fr's contents. Host MemoryFiles drop only a page-cache
// copy of data their file keeps; GuestRAM frames are the data. Dehydration
// drops the cache of a parked capture and exports its pages afterwards, and
// the capture's references free the frames once the export is done.
func (*guestBacking) DropCache(memmap.FileRange) error { return nil }

func (*guestBacking) DataFD(memmap.FileRange) (int, error) {
	return -1, fmt.Errorf("GuestRAM has no host data file descriptor")
}

func (b *guestBacking) Destroy() error {
	// Destroy has the same host-address invalidation semantics as Decommit, but
	// for every frame in the backing.
	b.ram.accessMu.Lock()
	defer b.ram.accessMu.Unlock()
	b.ram.mu.Lock()
	if b.destroyed {
		b.ram.mu.Unlock()
		return nil
	}
	b.destroyed = true
	frames := make([]*guestFrame, 0, len(b.frames))
	refs := make(map[pgalloc.FrameID][]MappingRef)
	for _, frame := range b.frames {
		frame.active = false
		frame.decommitting = true
		b.ram.logicalBytes -= GuestFrameSize
		frames = append(frames, frame)
		for ref := range frame.mappings {
			refs[frame.id] = append(refs[frame.id], ref)
		}
	}
	b.ram.mu.Unlock()
	rollback := func() {
		b.ram.mu.Lock()
		b.destroyed = false
		for _, frame := range frames {
			frame.active = true
			frame.decommitting = false
			b.ram.logicalBytes += GuestFrameSize
		}
		b.ram.cond.Broadcast()
		b.ram.mu.Unlock()
	}
	if len(refs) != 0 {
		if b.ram.revoker == nil {
			rollback()
			return fmt.Errorf("destroying GuestRAM backing with stage-1 mappings and no revoker")
		}
		if err := b.ram.revoker.RevokeFrames(context.Background(), refs); err != nil {
			rollback()
			return err
		}
	}
	b.ram.mu.Lock()
	for _, frame := range frames {
		if err := b.ram.host.Zero(frame.slab.hostAddress+uintptr(frame.slot)*uintptr(GuestFrameSize), GuestFrameSize); err != nil {
			b.destroyed = false
			for _, rollbackFrame := range frames {
				rollbackFrame.active = true
				rollbackFrame.decommitting = false
				b.ram.logicalBytes += GuestFrameSize
			}
			b.ram.cond.Broadcast()
			b.ram.mu.Unlock()
			return err
		}
	}
	touched := make(map[*guestSlab]struct{})
	for _, frame := range frames {
		frame.slab.frames[frame.slot] = nil
		delete(b.frames, frame.offset)
		delete(b.ram.frames, frame.id)
		touched[frame.slab] = struct{}{}
	}
	delete(b.ram.backings, b.id)
	var releases []slabRelease
	for slab := range touched {
		if slabEmpty(slab) && !slab.releasing {
			slab.releasing = true
			releases = append(releases, slabRelease{slab: slab})
		}
	}
	b.ram.cond.Broadcast()
	b.ram.mu.Unlock()
	return b.ram.releaseSlabs(releases)
}

func validateFrameRange(fr memmap.FileRange) error {
	if !fr.WellFormed() || fr.Length() == 0 || fr.Start%GuestFrameSize != 0 || fr.End%GuestFrameSize != 0 {
		return fmt.Errorf("invalid GuestRAM frame range %v", fr)
	}
	return nil
}
