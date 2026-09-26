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
	"container/heap"
	"context"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
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

	// inPartial and partialOccupancy record the slab's membership in
	// GuestRAM.partialSlabs and the occupancy that GuestRAM.partialFree and
	// GuestRAM.partialByOccupancy account for. They are protected by
	// GuestRAM.mu.
	inPartial        bool
	partialOccupancy uint64
}

// slabKeyHeap is a min-heap of slab keys.
type slabKeyHeap []slabKey

func (h slabKeyHeap) Len() int           { return len(h) }
func (h slabKeyHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h slabKeyHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *slabKeyHeap) Push(x any)        { *h = append(*h, x.(slabKey)) }
func (h *slabKeyHeap) Pop() any {
	old := *h
	key := old[len(old)-1]
	*h = old[:len(old)-1]
	return key
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
	// accessMu excludes physical frame relocation and backing destruction from
	// leased Sentry host mappings, which hold it for reading. Stage-1 mappings
	// are synchronized independently by the monitor.
	accessMu sync.RWMutex
	// Decommit frees frames without excluding leases, which would make every
	// new lease wait behind it; and since a lease may be held while taking
	// another, it could not wait for the leases it holds. Instead, each lease
	// also registers in leaseCounts[leaseEpoch&1] when it resolves its frames,
	// and a Decommit, after making its frames inactive so that new leases
	// cannot resolve them, advances leaseEpoch and waits for the leases
	// registered under the previous epoch to be released before zeroing or
	// releasing their memory. drainMu serializes those waits.
	leaseEpoch  atomic.Uint64
	leaseCounts [2]atomic.Int64
	drainMu     sync.Mutex
	// drainsWaiting counts Decommits waiting for leases, for tests.
	drainsWaiting atomic.Int32
	// compacting is set while Compact relocates frames; Decommit waits for it
	// to clear. It is protected by mu.
	compacting bool
	// cond wakes backing operations after a frame decommit or slab release.
	// It prevents a logical 4K frame from being reactivated while its old
	// stage-1 translations are still being revoked.
	cond *sync.Cond

	vm        VM
	host      HostMemory
	revoker   FrameRevoker
	relocator FrameRelocator
	rollover  func(*GuestRAM) error
	// rolloverPending is set while a background rollover runs. It is
	// protected by mu.
	rolloverPending bool

	ipaStart uint64
	ipaSize  uint64
	nextGPA  uint64
	epoch    uint64
	layout   uint64

	// Slabs are carved from chunks of host memory that are reserved,
	// committed, and mapped at stage 2 together, since each of those
	// operations is costly and independent of its length. chunkHost and
	// chunkGPA locate the next unused slab of the current chunk, and
	// chunkSlabs counts its unused slabs. A slab is released individually.
	chunkHost  uintptr
	chunkGPA   uint64
	chunkSlabs uint64

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
	// partialFree counts the free frames in partialSlabs, so sizing an
	// allocation does not scan them.
	partialFree uint64
	// partialByOccupancy indexes partialSlabs by occupancy, each as a min-heap
	// of slab keys, so choosing the densest slab does not scan them either.
	// Memory churn leaves many partial slabs, and both scans ran under mu on
	// every allocation. Entries are invalidated lazily; partialIndexEntries
	// counts them to bound the stale ones.
	partialByOccupancy  [FramesPerSlab]slabKeyHeap
	partialIndexEntries int
	// allocationSlab caches the densest non-full slab. Allocation increases
	// its occupancy, so it remains optimal until full. Decommit updates or
	// invalidates it while holding mu. This avoids scanning every live slab for
	// every 4K allocation in exec-heavy workloads with a large page cache.
	allocationSlab *guestSlab
	logicalBytes   uint64
	logicalLimit   uint64
}

type guestMappingLease struct {
	ram *GuestRAM
	// epoch is the parity of the lease epoch that the lease registered in.
	epoch uint64
	once  sync.Once
}

func (l *guestMappingLease) Release() {
	if l == nil || l.ram == nil {
		return
	}
	l.once.Do(func() {
		l.ram.leaseCounts[l.epoch].Add(-1)
		l.ram.accessMu.RUnlock() // +checklocksforce: the lease holds it.
	})
}

// registerLeaseLocked registers a lease in the current lease epoch and returns
// the epoch's parity. The caller must have resolved every frame that the lease
// covers in the same critical section.
//
// Preconditions: r.mu is locked.
func (r *GuestRAM) registerLeaseLocked() uint64 {
	for {
		parity := r.leaseEpoch.Load() & 1
		r.leaseCounts[parity].Add(1)
		// A registration in an epoch that drainLeases has already ended would
		// not be waited for; retry in the new one.
		if r.leaseEpoch.Load()&1 == parity {
			return parity
		}
		r.leaseCounts[parity].Add(-1)
	}
}

// drainLeases waits until every lease registered before the call has been
// released. It never delays new leases.
func (r *GuestRAM) drainLeases() {
	r.drainMu.Lock()
	defer r.drainMu.Unlock()
	// Earlier drains waited for the other parity's leases, so every lease
	// registered before this call is in this parity's count.
	parity := (r.leaseEpoch.Add(1) - 1) & 1
	if r.leaseCounts[parity].Load() == 0 {
		return
	}
	r.drainsWaiting.Add(1)
	defer r.drainsWaiting.Add(-1)
	for delay := time.Microsecond; r.leaseCounts[parity].Load() != 0; delay = min(2*delay, time.Millisecond) {
		time.Sleep(delay)
	}
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
	b.ram.mu.Lock()
	defer b.ram.mu.Unlock()
	for {
		frames, waited, err := b.resolveFramesLocked(fr, at)
		if !waited {
			return frames, err
		}
	}
}

// resolveFramesLocked is ResolveFrames with b.ram.mu held. If a frame in fr is
// being decommitted or released, it waits on b.ram.cond, which releases
// b.ram.mu, and returns waited == true without resolving fr.
//
// Preconditions: fr is valid.
func (b *guestBacking) resolveFramesLocked(fr memmap.FileRange, at hostarch.AccessType) (frames []pgalloc.BackingFrame, waited bool, err error) {
	if b.destroyed || fr.End > b.size {
		return nil, false, fmt.Errorf("GuestRAM range %v exceeds backing size %#x", fr, b.size)
	}
	for offset := fr.Start; offset < fr.End; offset += GuestFrameSize {
		if frame := b.frames[offset]; frame != nil {
			if frame.slab.releasing || frame.decommitting {
				b.ram.cond.Wait()
				return nil, true, nil
			}
		}
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
	newSlabs = b.ram.newIPASlabsLocked(newSlabs)
	if newSlabs != 0 && b.ram.wouldReachRolloverLocked(newSlabs) {
		epoch := b.ram.epoch
		if b.ram.rollover == nil {
			return nil, false, fmt.Errorf("GuestRAM IPA epoch %d reached 75%% rollover threshold", epoch)
		}
		// Roll over in the background. Callers hold GuestRAM leases and
		// staging or address-space locks, while the rebuild waits for every
		// epoch reader, and those readers can wait for the same locks, so
		// rolling over inline could deadlock. Allocation continues from the
		// IPA above the threshold in the meantime.
		b.ram.startRolloverLocked()
		if b.ram.wouldExhaustIPALocked(newSlabs) {
			return nil, false, fmt.Errorf("GuestRAM IPA epoch %d is exhausted while its rollover is pending", epoch)
		}
	}
	additional := newFrames * GuestFrameSize
	if limit := b.ram.logicalLimit; limit != 0 && (additional > limit || b.ram.logicalBytes > limit-additional) {
		return nil, false, linuxerr.ENOMEM
	}

	frames = make([]pgalloc.BackingFrame, 0, fr.Length()/GuestFrameSize)
	for offset := fr.Start; offset < fr.End; offset += GuestFrameSize {
		frame := b.frames[offset]
		if frame == nil {
			var err error
			frame, err = b.ram.allocateFrameLocked(b, offset)
			if err != nil {
				return nil, false, err
			}
		}
		if frame.slab.releasing {
			return nil, false, fmt.Errorf("GuestRAM frame at offset %#x is being released", offset)
		}
		slot := uint64(frame.slot)
		frames = append(frames, pgalloc.BackingFrame{
			ID:          frame.id,
			FileOffset:  offset,
			GPA:         frame.slab.gpa + slot*GuestFrameSize,
			HostAddress: frame.slab.hostAddress + uintptr(slot*GuestFrameSize),
		})
	}
	return frames, false, nil
}

// TryAcquireMapping is AcquireMapping, but reports acquired == false instead
// of waiting behind an exclusive operation such as Compact or Destroy.
func (b *guestBacking) TryAcquireMapping(fr memmap.FileRange, at hostarch.AccessType) ([]pgalloc.BackingFrame, memmap.InternalMappingLease, bool, error) {
	if err := validateFrameRange(fr); err != nil {
		return nil, nil, true, err
	}
	if !b.ram.accessMu.TryRLock() {
		return nil, nil, false, nil
	}
	frames, epoch, err := b.resolveLeased([]pgalloc.BackingMappingRequest{{Range: fr, Access: at}})
	if err != nil {
		b.ram.accessMu.RUnlock() // +checklocksforce: TryRLock succeeded.
		return nil, nil, true, err
	}
	return frames[0], &guestMappingLease{ram: b.ram, epoch: epoch}, true, nil
}

// AwaitMappingIdle waits for pending exclusive operations such as Destroy by
// acquiring a lease, which queues behind them, and releasing it.
func (b *guestBacking) AwaitMappingIdle() {
	b.ram.accessMu.RLock()
	lease := guestMappingLease{ram: b.ram}
	lease.Release()
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
	for _, request := range requests {
		if err := validateFrameRange(request.Range); err != nil {
			return nil, nil, err
		}
	}
	b.ram.accessMu.RLock()
	frames, epoch, err := b.resolveLeased(requests)
	if err != nil {
		b.ram.accessMu.RUnlock()
		return nil, nil, err
	}
	return frames, &guestMappingLease{ram: b.ram, epoch: epoch}, nil
}

// resolveLeased resolves requests and registers a lease covering them in one
// critical section, so that a Decommit of any of their frames waits for the
// lease. While it waits for a frame being decommitted, it holds no
// registration that the Decommit would wait for.
//
// Preconditions: The requests' ranges are valid.
func (b *guestBacking) resolveLeased(requests []pgalloc.BackingMappingRequest) ([][]pgalloc.BackingFrame, uint64, error) {
	b.ram.mu.Lock()
	defer b.ram.mu.Unlock()
retry:
	for {
		frames := make([][]pgalloc.BackingFrame, len(requests))
		for i, request := range requests {
			resolved, waited, err := b.resolveFramesLocked(request.Range, request.Access)
			if err != nil {
				return nil, 0, err
			}
			if waited {
				continue retry
			}
			frames[i] = resolved
		}
		return frames, b.ram.registerLeaseLocked(), nil
	}
}

// startRolloverLocked runs the epoch rollover on a new goroutine unless one
// is already pending.
//
// Preconditions: r.mu is locked and r.rollover is set.
func (r *GuestRAM) startRolloverLocked() {
	if r.rolloverPending {
		return
	}
	r.rolloverPending = true
	rollover := r.rollover
	go func() {
		err := rollover(r)
		r.mu.Lock()
		r.rolloverPending = false
		r.mu.Unlock()
		if err != nil {
			log.Warningf("GuestRAM epoch rollover failed: %v", err)
		}
	}()
}

// wouldExhaustIPALocked reports whether additionalSlabs more slabs do not fit
// in the IPA range at all.
// maxSlabChunkSlabs bounds the slabs that GuestRAM reserves and maps at once.
const maxSlabChunkSlabs = 64

// slabChunkSlabsLocked returns the number of slabs to reserve and map at once:
// maxSlabChunkSlabs, unless that is a significant part of the IPA range.
func (r *GuestRAM) slabChunkSlabsLocked() uint64 {
	return max(1, min(maxSlabChunkSlabs, r.ipaSize/HostSlabSize/1024))
}

// newIPASlabsLocked returns the slabs of IPA that allocating slabs more slabs
// consumes beyond the current chunk.
func (r *GuestRAM) newIPASlabsLocked(slabs uint64) uint64 {
	return slabs - min(slabs, r.chunkSlabs)
}

func (r *GuestRAM) wouldExhaustIPALocked(additionalSlabs uint64) bool {
	remaining := r.ipaSize - (r.nextGPA - r.ipaStart)
	return additionalSlabs > remaining/HostSlabSize
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
	occupancy := slabOccupancy(candidate)
	if candidate.releasing || occupancy >= FramesPerSlab {
		r.removePartialSlabLocked(candidate)
		if r.allocationSlab == candidate {
			r.allocationSlab = nil
		}
		return
	}
	if r.partialSlabs == nil {
		r.partialSlabs = make(map[slabKey]*guestSlab)
	}
	if !candidate.inPartial {
		r.partialSlabs[candidate.key] = candidate
		candidate.inPartial = true
		candidate.partialOccupancy = occupancy
		r.partialFree += FramesPerSlab - occupancy
		r.indexPartialSlabLocked(candidate)
	} else if candidate.partialOccupancy != occupancy {
		r.partialFree += FramesPerSlab - occupancy
		r.partialFree -= FramesPerSlab - candidate.partialOccupancy
		candidate.partialOccupancy = occupancy
		r.indexPartialSlabLocked(candidate)
	}
	if betterAllocationSlab(candidate, r.allocationSlab) {
		r.allocationSlab = candidate
	}
}

// removePartialSlabLocked removes slab from partialSlabs and its accounting.
func (r *GuestRAM) removePartialSlabLocked(slab *guestSlab) {
	if !slab.inPartial {
		return
	}
	delete(r.partialSlabs, slab.key)
	r.partialFree -= FramesPerSlab - slab.partialOccupancy
	slab.inPartial = false
}

// resetPartialSlabsLocked empties partialSlabs and its accounting; callers
// then consider every live slab again.
func (r *GuestRAM) resetPartialSlabsLocked() {
	for _, slab := range r.partialSlabs {
		slab.inPartial = false
	}
	r.partialSlabs = make(map[slabKey]*guestSlab)
	r.partialFree = 0
	r.partialByOccupancy = [FramesPerSlab]slabKeyHeap{}
	r.partialIndexEntries = 0
}

// indexPartialSlabLocked records slab under its current partial occupancy.
// Entries under its previous occupancy become stale.
func (r *GuestRAM) indexPartialSlabLocked(slab *guestSlab) {
	heap.Push(&r.partialByOccupancy[slab.partialOccupancy], slab.key)
	r.partialIndexEntries++
	if r.partialIndexEntries > 4*len(r.partialSlabs)+64 {
		for occupancy := range r.partialByOccupancy {
			r.partialByOccupancy[occupancy] = r.partialByOccupancy[occupancy][:0]
		}
		for _, partial := range r.partialSlabs {
			h := &r.partialByOccupancy[partial.partialOccupancy]
			*h = append(*h, partial.key)
		}
		for occupancy := range r.partialByOccupancy {
			heap.Init(&r.partialByOccupancy[occupancy])
		}
		r.partialIndexEntries = len(r.partialSlabs)
	}
}

// selectAllocationSlabLocked returns the densest partial slab, preferring
// the lowest key among equally dense slabs, as betterAllocationSlab does.
func (r *GuestRAM) selectAllocationSlabLocked() *guestSlab {
	if r.allocationSlab != nil && !r.allocationSlab.releasing && slabOccupancy(r.allocationSlab) < FramesPerSlab {
		return r.allocationSlab
	}
	r.allocationSlab = nil
	for occupancy := FramesPerSlab; occupancy > 0; occupancy-- {
		h := &r.partialByOccupancy[occupancy-1]
		for h.Len() != 0 {
			slab := r.partialSlabs[(*h)[0]]
			if slab != nil && slab.inPartial && !slab.releasing && slab.partialOccupancy == occupancy-1 {
				r.allocationSlab = slab
				return slab
			}
			heap.Pop(h)
			r.partialIndexEntries--
		}
	}
	return nil
}

func (r *GuestRAM) freeSlotsLocked(_ *guestBacking) uint64 {
	return r.partialFree
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
	if r.chunkSlabs == 0 {
		if err := r.allocateChunkLocked(); err != nil {
			return nil, err
		}
	}
	r.nextSlabID++
	slab := &guestSlab{
		key:         slabKey(r.nextSlabID),
		hostAddress: r.chunkHost,
		gpa:         r.chunkGPA,
	}
	r.chunkHost += uintptr(HostSlabSize)
	r.chunkGPA += HostSlabSize
	r.chunkSlabs--
	r.slabs[slab.key] = slab
	r.considerAllocationSlabLocked(slab)
	return slab, nil
}

// allocateChunkLocked reserves, commits, and maps the next chunk of slabs.
//
// Preconditions: r.chunkSlabs == 0.
func (r *GuestRAM) allocateChunkLocked() error {
	slabs := min(r.slabChunkSlabsLocked(), (r.ipaSize-(r.nextGPA-r.ipaStart))/HostSlabSize)
	if slabs == 0 {
		return fmt.Errorf("GuestRAM IPA epoch %d is exhausted", r.epoch)
	}
	length := slabs * HostSlabSize
	hostAddress, err := r.host.Reserve(length)
	if err != nil {
		return fmt.Errorf("reserving GuestRAM slabs: %w", err)
	}
	if uint64(hostAddress)%HostSlabSize != 0 {
		_ = r.host.Release(hostAddress, length)
		return fmt.Errorf("host reservation %#x is not %d-byte aligned", hostAddress, HostSlabSize)
	}
	if err := r.host.Commit(hostAddress, length); err != nil {
		_ = r.host.Release(hostAddress, length)
		return fmt.Errorf("committing GuestRAM slabs: %w", err)
	}
	gpa := r.nextGPA
	if err := r.vm.MapStage2(gpa, hostAddress, length, Stage2Read|Stage2Write|Stage2Execute); err != nil {
		_ = r.host.Decommit(hostAddress, length)
		_ = r.host.Release(hostAddress, length)
		return fmt.Errorf("mapping GuestRAM stage-2 slabs: %w", err)
	}
	r.nextGPA += length
	r.chunkHost = hostAddress
	r.chunkGPA = gpa
	r.chunkSlabs = slabs
	return nil
}

// releaseChunkLocked releases the unused slabs of the current chunk, whose
// stage-2 mappings the caller has already discarded.
func (r *GuestRAM) releaseChunkLocked() {
	if r.chunkSlabs == 0 {
		return
	}
	length := r.chunkSlabs * HostSlabSize
	if err := r.host.Decommit(r.chunkHost, length); err != nil {
		log.Warningf("Decommitting unused GuestRAM slabs: %v", err)
	} else if err := r.host.Release(r.chunkHost, length); err != nil {
		log.Warningf("Releasing unused GuestRAM slabs: %v", err)
	}
	r.chunkHost = 0
	r.chunkGPA = 0
	r.chunkSlabs = 0
}

func (b *guestBacking) Decommit(fr memmap.FileRange) error {
	if err := validateFrameRange(fr); err != nil {
		return err
	}
	// Decommit may zero a frame and release its containing host mapping. It
	// drains the operation-scoped Sentry mappings that could have resolved the
	// frame before changing either the bytes or the lifetime of those host
	// addresses; see GuestRAM.leaseEpoch.
	b.ram.mu.Lock()
	for {
		if b.destroyed || fr.End > b.size {
			b.ram.mu.Unlock()
			return fmt.Errorf("invalid GuestRAM decommit %v", fr)
		}
		if b.ram.compacting {
			b.ram.cond.Wait()
			continue
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
	// New leases can't resolve the frames being decommitted. Wait for those
	// that could have.
	b.ram.drainLeases()

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

// decommitInProgressLocked returns true if a frame of backing, or of any
// backing if backing is nil, is being decommitted, or its slab released.
//
// Preconditions: r.mu is locked.
func (r *GuestRAM) decommitInProgressLocked(backing *guestBacking) bool {
	for _, slab := range r.slabs {
		for _, frame := range slab.frames {
			if frame == nil || (backing != nil && frame.backing != backing) {
				continue
			}
			if frame.decommitting || slab.releasing {
				return true
			}
		}
		if backing == nil && slab.releasing {
			return true
		}
	}
	return false
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

	if releaser, ok := r.host.(HostRangeReleaser); ok {
		return r.releaseSlabRangesLocked(releases, releaser, clearReleasing, remap)
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
			r.removePartialSlabLocked(slab)
			if r.allocationSlab == slab {
				r.allocationSlab = nil
			}
		}
		groupStart = groupEnd
	}
	r.cond.Broadcast()
	return nil
}

// releaseSlabRangesLocked is releaseSlabs for host memory that releases many
// reservations together: releasing slabs one at a time retires each one's
// Sentry aliases separately, which on HVF blocks every application entry and
// changes the Sentry's translations per 16 KiB.
//
// Preconditions: r.mu is locked, and releases are sorted by GPA.
func (r *GuestRAM) releaseSlabRangesLocked(releases []slabRelease, releaser HostRangeReleaser, clearReleasing func([]slabRelease), remap func([]slabRelease) error) error {
	for groupStart := 0; groupStart < len(releases); {
		groupEnd := groupStart + 1
		for groupEnd < len(releases) && releases[groupEnd].slab.gpa == releases[groupEnd-1].slab.gpa+HostSlabSize {
			groupEnd++
		}
		group := releases[groupStart:groupEnd]
		if err := r.vm.UnmapStage2(group[0].slab.gpa, uint64(len(group))*HostSlabSize); err != nil {
			remapErr := remap(releases[:groupStart])
			clearReleasing(releases[groupStart:])
			r.cond.Broadcast()
			if remapErr != nil {
				return fmt.Errorf("unmapping GuestRAM slabs: %v; restoring stage-2 mappings: %w", err, remapErr)
			}
			return err
		}
		groupStart = groupEnd
	}
	ranges := make([]HostRange, 0, len(releases))
	for _, release := range releases {
		ranges = append(ranges, HostRange{Address: release.slab.hostAddress, Length: HostSlabSize})
	}
	if err := releaser.ReleaseRanges(ranges); err != nil {
		remapErr := remap(releases)
		r.cond.Broadcast()
		if remapErr != nil {
			return fmt.Errorf("releasing GuestRAM slabs: %v; restoring stage-2 mappings: %w", err, remapErr)
		}
		return fmt.Errorf("releasing GuestRAM slabs: %w", err)
	}
	for _, release := range releases {
		slab := release.slab
		delete(r.slabs, slab.key)
		r.removePartialSlabLocked(slab)
		if r.allocationSlab == slab {
			r.allocationSlab = nil
		}
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

	// Decommit doesn't exclude leases, so it can run concurrently; wait for
	// those in progress and hold off new ones while frames move. They need no
	// leases to finish.
	r.mu.Lock()
	r.compacting = true
	for r.decommitInProgressLocked(nil) {
		r.cond.Wait()
	}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		r.compacting = false
		r.cond.Broadcast()
		r.mu.Unlock()
	}()
	return r.compactExclusive(ctx)
}

// compactExclusive implements Compact.
//
// Preconditions: r.accessMu is locked for writing, and r.compacting is set.
func (r *GuestRAM) compactExclusive(ctx context.Context) (GuestRAMCompaction, error) {
	r.mu.Lock()

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
	r.resetPartialSlabsLocked()
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
	// The current chunk's unused slabs were mapped only in the old VM.
	r.releaseChunkLocked()
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
	// Decommit doesn't exclude leases, so it can be in progress; let it
	// finish with this backing's frames first.
	for b.ram.decommitInProgressLocked(b) {
		b.ram.cond.Wait()
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
		// Frames freed here become available, and releasing slabs leave the
		// partial set.
		b.ram.considerAllocationSlabLocked(slab)
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
