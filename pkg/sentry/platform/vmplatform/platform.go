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
	stdcontext "context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	sentryplatform "gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
)

// Config constructs a portable VM platform over one host backend VM.
type Config struct {
	VM           VM
	NewVM        func(stdcontext.Context) (VM, error)
	HostMemory   HostMemory
	Architecture Architecture
	VCPUs        int
	IPAStart     uint64
	IPASize      uint64
	MaxUser      hostarch.Addr
	PlatformName string
	Rollover     func(*GuestRAM) error
}

// Platform implements sentryplatform.Platform using an EL1 monitor and
// GuestRAM. It contains no host-hypervisor API calls.
type Platform struct {
	sentryplatform.NoCPUPreemptionDetection
	sentryplatform.NoCPUNumbers

	vm      VM
	arch    Architecture
	pool    *WorkerPool
	monitor *Monitor
	ram     *GuestRAM
	staging *stagingCache
	maxUser hostarch.Addr
	name    string
	newVM   func(stdcontext.Context) (VM, error)
	vcpus   int

	// epochMu excludes every stage-1 operation and application vCPU while an
	// HVF VM epoch is replaced. rebuildMu serializes rebuild and close.
	epochMu   sync.RWMutex
	rebuildMu sync.Mutex

	mu             sync.Mutex
	nextAddressID  uint64
	spaces         map[uint64]*addressSpace
	transitioning  bool
	transitionDone chan struct{}
	epochErr       error
	closed         bool
}

// New constructs the common VM platform.
func New(config Config) (*Platform, error) {
	if config.Architecture == nil {
		return nil, fmt.Errorf("vmplatform requires an architecture implementation")
	}
	if config.VM == nil {
		if config.NewVM == nil {
			return nil, fmt.Errorf("vmplatform requires a VM or VM factory")
		}
		var err error
		config.VM, err = config.NewVM(stdcontext.Background())
		if err != nil {
			return nil, err
		}
	}
	pool, err := NewWorkerPool(config.VM, config.VCPUs)
	if err != nil {
		_ = config.VM.Close()
		return nil, err
	}
	ram, err := NewGuestRAM(GuestRAMConfig{
		VM:       config.VM,
		Host:     config.HostMemory,
		IPAStart: config.IPAStart,
		IPASize:  config.IPASize,
		Rollover: config.Rollover,
	})
	if err != nil {
		pool.Close()
		_ = config.VM.Close()
		return nil, err
	}
	staging, err := newStagingCache(ram)
	if err != nil {
		pool.Close()
		_ = config.VM.Close()
		return nil, err
	}
	maxUser := config.MaxUser
	if maxUser == 0 {
		maxUser = hostarch.Addr(1 << 48)
	}
	name := config.PlatformName
	if name == "" {
		name = "vmplatform"
	}
	p := &Platform{
		vm:      config.VM,
		arch:    config.Architecture,
		pool:    pool,
		monitor: NewMonitor(pool),
		ram:     ram,
		staging: staging,
		maxUser: maxUser,
		name:    name,
		newVM:   config.NewVM,
		vcpus:   config.VCPUs,
		spaces:  make(map[uint64]*addressSpace),
	}
	ram.SetRevoker(p)
	ram.SetRelocator(p)
	if config.Rollover == nil && config.NewVM != nil {
		ram.SetRollover(p.rebuildEpoch)
	}
	return p, nil
}

// MemoryFileBackingProvider exposes GuestRAM to runsc without changing the
// base platform.Platform interface.
func (p *Platform) MemoryFileBackingProvider() pgalloc.BackingProvider { return p.ram }

// GuestRAM returns the platform allocator for metrics and TTC compaction.
func (p *Platform) GuestRAM() *GuestRAM { return p.ram }

// SetMemoryLimit implements sentryplatform.MemoryLimiter.
func (p *Platform) SetMemoryLimit(limit uint64) error {
	return p.ram.SetLogicalLimit(limit)
}

// PlatformMemoryUsage implements sentryplatform.MemoryAccounting.
func (p *Platform) PlatformMemoryUsage() sentryplatform.MemoryUsage {
	metrics := p.ram.Metrics()
	return sentryplatform.MemoryUsage{
		LogicalBytes:      metrics.LogicalBytes,
		HostResidentBytes: metrics.HostResidentBytes,
		IPABytesConsumed:  metrics.IPABytesConsumed,
		LogicalLimitBytes: metrics.LogicalLimitBytes,
		VMEpoch:           metrics.Epoch,
	}
}

// CompactMemory implements sentryplatform.MemoryCompactor. Callers must stop
// every Kernel sharing p and discard their PMA internal-mapping caches first.
func (p *Platform) CompactMemory(ctx context.Context) (sentryplatform.MemoryCompaction, error) {
	p.rebuildMu.Lock()
	defer p.rebuildMu.Unlock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return sentryplatform.MemoryCompaction{}, fmt.Errorf("vmplatform is closed")
	}
	if p.transitioning {
		p.mu.Unlock()
		return sentryplatform.MemoryCompaction{}, fmt.Errorf("vmplatform VM epoch is transitioning")
	}
	p.mu.Unlock()
	p.pool.InterruptApplications()
	p.epochMu.Lock()
	defer p.epochMu.Unlock()
	result, err := p.ram.Compact(ctx)
	return sentryplatform.MemoryCompaction{
		FramesMoved:   result.FramesMoved,
		SlabsReleased: result.SlabsReleased,
		BytesCopied:   result.BytesCopied,
	}, err
}

func (*Platform) SupportsAddressSpaceIO() bool  { return false }
func (*Platform) HaveGlobalMemoryBarrier() bool { return false }
func (*Platform) GlobalMemoryBarrier() error {
	panic("vmplatform does not implement a global memory barrier")
}
func (*Platform) MapUnit() uint64                 { return 16 << 20 }
func (*Platform) MinUserAddress() hostarch.Addr   { return hostarch.PageSize }
func (p *Platform) MaxUserAddress() hostarch.Addr { return p.maxUser }
func (p *Platform) ConcurrencyCount() int {
	if err := p.lockEpoch(stdcontext.Background()); err != nil {
		return 0
	}
	count := len(p.pool.workers)
	p.epochMu.RUnlock()
	return count
}
func (p *Platform) SeccompInfo() sentryplatform.SeccompInfo {
	return sentryplatform.StaticSeccompInfo{PlatformName: p.name}
}

func (p *Platform) NewAddressSpace() (sentryplatform.AddressSpace, error) {
	if err := p.lockEpoch(stdcontext.Background()); err != nil {
		return nil, err
	}
	defer p.epochMu.RUnlock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, fmt.Errorf("vmplatform is closed")
	}
	p.nextAddressID++
	id := p.nextAddressID
	p.mu.Unlock()
	if err := p.monitor.Execute(stdcontext.Background(), monitorabi.Command{
		Opcode:         monitorabi.OpcodeCreateAddressSpace,
		AddressSpaceID: id,
	}); err != nil {
		return nil, err
	}
	as := &addressSpace{platform: p, id: id, mappings: make(map[hostarch.Addr]addressMapping)}
	p.mu.Lock()
	p.spaces[id] = as
	p.mu.Unlock()
	return as, nil
}

func (p *Platform) NewContext(context.Context) sentryplatform.Context {
	return &platformContext{platform: p, registers: p.arch.NewVCPUState()}
}

// FlushForCheckpoint implements sentryplatform.CheckpointCoherence. The kernel
// is quiesced before this call; GuestRAM itself is already coherent.
func (p *Platform) FlushForCheckpoint(context.Context) error {
	if err := p.lockEpoch(stdcontext.Background()); err != nil {
		return err
	}
	defer p.epochMu.RUnlock()
	if err := p.staging.flushAll(); err != nil {
		return err
	}
	p.mu.Lock()
	spaces := make([]*addressSpace, 0, len(p.spaces))
	for _, as := range p.spaces {
		spaces = append(spaces, as)
	}
	p.mu.Unlock()
	for _, as := range spaces {
		as.mu.Lock()
		err := as.stickyWriteback
		as.mu.Unlock()
		if err != nil {
			return err
		}
	}
	return nil
}

// PrepareRestore implements sentryplatform.RestoreReuser. VM platform state
// is an execution cache: after the bootstrap Kernel releases its address
// spaces, the same process-scoped VM can receive the restored Kernel's roots
// and GuestRAM-backed MemoryFile without serializing any hypervisor state.
func (p *Platform) PrepareRestore() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return fmt.Errorf("vmplatform is closed")
	}
	if len(p.spaces) != 0 {
		return fmt.Errorf("vmplatform still has %d bootstrap address spaces", len(p.spaces))
	}
	return nil
}

// Close releases workers and the disposable VM execution cache.
func (p *Platform) Close() error {
	p.rebuildMu.Lock()
	defer p.rebuildMu.Unlock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.closed = true
	p.mu.Unlock()
	p.pool.Close()
	p.epochMu.Lock()
	defer p.epochMu.Unlock()
	return p.vm.Close()
}

func (p *Platform) lockEpoch(ctx stdcontext.Context) error {
	for {
		p.epochMu.RLock()
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			p.epochMu.RUnlock()
			return fmt.Errorf("vmplatform is closed")
		}
		if p.epochErr != nil {
			err := p.epochErr
			p.mu.Unlock()
			p.epochMu.RUnlock()
			return fmt.Errorf("vmplatform execution epoch failed: %w", err)
		}
		if !p.transitioning {
			p.mu.Unlock()
			return nil
		}
		done := p.transitionDone
		p.mu.Unlock()
		p.epochMu.RUnlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (p *Platform) rebuildEpoch(ram *GuestRAM) (retErr error) {
	p.rebuildMu.Lock()
	defer p.rebuildMu.Unlock()
	if ram != p.ram || p.newVM == nil {
		return fmt.Errorf("vmplatform has no VM epoch factory")
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return fmt.Errorf("vmplatform is closed")
	}
	if p.transitioning {
		done := p.transitionDone
		p.mu.Unlock()
		<-done
		p.mu.Lock()
		err := p.epochErr
		p.mu.Unlock()
		return err
	}
	p.transitioning = true
	p.transitionDone = make(chan struct{})
	done := p.transitionDone
	oldPool := p.pool
	oldVM := p.vm
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		if retErr != nil {
			p.epochErr = retErr
		}
		p.transitioning = false
		close(done)
		p.mu.Unlock()
	}()

	// Closing the worker pool interrupts every running application and waits
	// for any non-cancelable monitor transaction to acknowledge. New users see
	// transitioning=true and wait before taking epochMu.
	oldPool.Close()
	p.epochMu.Lock()
	defer p.epochMu.Unlock()
	if err := oldVM.Close(); err != nil {
		return fmt.Errorf("closing old VM epoch: %w", err)
	}

	newVM, err := p.newVM(stdcontext.Background())
	if err != nil {
		return fmt.Errorf("creating replacement VM epoch: %w", err)
	}
	newPool, err := NewWorkerPool(newVM, p.vcpus)
	if err != nil {
		_ = newVM.Close()
		return fmt.Errorf("creating replacement vCPU pool: %w", err)
	}
	newMonitor := NewMonitor(newPool)
	plan, err := ram.beginEpochRebind(newVM)
	if err != nil {
		newPool.Close()
		_ = newVM.Close()
		return err
	}
	abort := true
	defer func() {
		if abort {
			_ = plan.abort()
			newPool.Close()
			_ = newVM.Close()
		}
	}()

	p.mu.Lock()
	spaces := make([]*addressSpace, 0, len(p.spaces))
	for _, as := range p.spaces {
		spaces = append(spaces, as)
	}
	p.mu.Unlock()
	sort.Slice(spaces, func(i, j int) bool { return spaces[i].id < spaces[j].id })
	for _, as := range spaces {
		as.mu.Lock()
		if as.released {
			as.mu.Unlock()
			continue
		}
		if err := newMonitor.Execute(stdcontext.Background(), monitorabi.Command{
			Opcode:         monitorabi.OpcodeCreateAddressSpace,
			AddressSpaceID: as.id,
		}); err != nil {
			as.mu.Unlock()
			return fmt.Errorf("recreating address space %d: %w", as.id, err)
		}
		vas := make([]hostarch.Addr, 0, len(as.mappings))
		for va := range as.mappings {
			vas = append(vas, va)
		}
		sort.Slice(vas, func(i, j int) bool { return vas[i] < vas[j] })
		operations := make([]monitorabi.Operation, 0, monitorabi.MaxOperations)
		for _, va := range vas {
			mapping := as.mappings[va]
			gpa, ok := plan.frameGPA(mapping.frame)
			if !ok {
				as.mu.Unlock()
				return fmt.Errorf("address space %d references missing frame %d", as.id, mapping.frame)
			}
			operations = append(operations, monitorabi.Operation{
				Type:   monitorabi.OperationMap,
				Access: monitorAccess(mapping.access),
				VA:     uint64(va),
				GPA:    gpa,
				Length: hostarch.PageSize,
			})
			if len(operations) == monitorabi.MaxOperations {
				if err := newMonitor.Execute(stdcontext.Background(), monitorabi.Command{Opcode: monitorabi.OpcodeApply, AddressSpaceID: as.id, Operations: operations}); err != nil {
					as.mu.Unlock()
					return fmt.Errorf("rebuilding address space %d: %w", as.id, err)
				}
				operations = operations[:0]
			}
		}
		if len(operations) != 0 {
			if err := newMonitor.Execute(stdcontext.Background(), monitorabi.Command{Opcode: monitorabi.OpcodeApply, AddressSpaceID: as.id, Operations: operations}); err != nil {
				as.mu.Unlock()
				return fmt.Errorf("rebuilding address space %d: %w", as.id, err)
			}
		}
		as.mu.Unlock()
	}
	if err := plan.commit(); err != nil {
		return err
	}
	abort = false
	p.mu.Lock()
	p.vm = newVM
	p.pool = newPool
	p.monitor = newMonitor
	p.mu.Unlock()
	return nil
}

type frameResolver interface {
	ResolveBackingFrames(memmap.FileRange, hostarch.AccessType) ([]pgalloc.BackingFrame, error)
}

type addressMapping struct {
	frame  pgalloc.FrameID
	access hostarch.AccessType
	staged *stagedPage
}

type addressSpace struct {
	sentryplatform.NoAddressSpaceIO

	platform *Platform
	id       uint64

	mu              sync.Mutex
	mappings        map[hostarch.Addr]addressMapping
	released        bool
	stickyWriteback error
}

func (as *addressSpace) MapFile(addr hostarch.Addr, file memmap.File, fr memmap.FileRange, at hostarch.AccessType, _ bool) error {
	if addr%hostarch.PageSize != 0 || fr.Start%hostarch.PageSize != 0 || fr.Length() == 0 || fr.End%hostarch.PageSize != 0 {
		return fmt.Errorf("invalid vmplatform mapping addr=%#x range=%v", addr, fr)
	}
	var frames []pgalloc.BackingFrame
	var stagedPages []*stagedPage
	for {
		before := as.platform.ram.Metrics().Epoch
		layoutBefore := as.platform.ram.layoutGeneration()
		var err error
		if resolver, ok := file.(frameResolver); ok {
			frames, err = resolver.ResolveBackingFrames(fr, at)
			stagedPages = nil
		} else {
			frames, stagedPages, err = as.platform.staging.resolve(file, fr, at)
		}
		if err != nil {
			return err
		}
		epoch := as.platform.ram.Metrics().Epoch
		if epoch != before {
			continue
		}
		if err := as.platform.lockEpoch(stdcontext.Background()); err != nil {
			return err
		}
		if as.platform.ram.Metrics().Epoch == epoch && as.platform.ram.layoutGeneration() == layoutBefore {
			break
		}
		as.platform.epochMu.RUnlock()
	}
	defer as.platform.epochMu.RUnlock()
	if uint64(len(frames))*hostarch.PageSize != fr.Length() {
		return fmt.Errorf("GuestRAM returned %d frames for %d-byte mapping", len(frames), fr.Length())
	}
	access := monitorAccess(at)
	operations := make([]monitorabi.Operation, len(frames))
	for i, frame := range frames {
		operations[i] = monitorabi.Operation{
			Type:   monitorabi.OperationMap,
			Access: access,
			VA:     uint64(addr) + uint64(i)*hostarch.PageSize,
			GPA:    frame.GPA,
			Length: hostarch.PageSize,
		}
	}

	as.mu.Lock()
	defer as.mu.Unlock()
	if as.released {
		return fmt.Errorf("mapping released vmplatform address space")
	}
	for start := 0; start < len(operations); start += monitorabi.MaxOperations {
		end := min(start+monitorabi.MaxOperations, len(operations))
		if err := as.platform.monitor.Execute(stdcontext.Background(), monitorabi.Command{
			Opcode:         monitorabi.OpcodeApply,
			AddressSpaceID: as.id,
			Operations:     operations[start:end],
		}); err != nil {
			return err
		}
		for i := start; i < end; i++ {
			va := addr + hostarch.Addr(uint64(i)*hostarch.PageSize)
			if old, ok := as.mappings[va]; ok {
				if old.frame == frames[i].ID {
					as.mappings[va] = addressMapping{frame: frames[i].ID, access: at, staged: stagedPageAt(stagedPages, i)}
					continue
				}
				as.platform.ram.RemoveMapping(old.frame, MappingRef{AddressSpaceID: as.id, VirtualAddress: uint64(va)})
				if err := as.platform.staging.removeMapping(old.staged); err != nil {
					as.stickyWriteback = errors.Join(as.stickyWriteback, err)
				}
			}
			ref := MappingRef{AddressSpaceID: as.id, VirtualAddress: uint64(va)}
			if err := as.platform.ram.AddMapping(frames[i].ID, ref); err != nil {
				as.stickyWriteback = errors.Join(as.stickyWriteback, err)
				return err
			}
			staged := stagedPageAt(stagedPages, i)
			as.platform.staging.addMapping(staged)
			as.mappings[va] = addressMapping{frame: frames[i].ID, access: at, staged: staged}
		}
	}
	return nil
}

func (as *addressSpace) Unmap(addr hostarch.Addr, length uint64) {
	if length == 0 {
		return
	}
	if err := as.platform.lockEpoch(stdcontext.Background()); err != nil {
		as.mu.Lock()
		as.stickyWriteback = errors.Join(as.stickyWriteback, err)
		as.mu.Unlock()
		return
	}
	defer as.platform.epochMu.RUnlock()
	as.mu.Lock()
	defer as.mu.Unlock()
	if as.released {
		return
	}

	// MemoryManager intentionally extrapolates invalidations to neighboring
	// PMAs so platforms can reclaim intermediate page tables. Such a range can
	// span almost the entire user address space despite containing only a few
	// mapped pages. Walking the requested range a page at a time would therefore
	// turn a sparse unmap into millions of monitor transactions. The host-side
	// mapping index is authoritative for installed stage-1 leaves, so issue
	// mutations only for leaves that are actually present.
	end := addr + hostarch.Addr(length)
	if end < addr {
		end = ^hostarch.Addr(0)
	}
	vas := make([]hostarch.Addr, 0)
	for va := range as.mappings {
		if va >= addr && va < end {
			vas = append(vas, va)
		}
	}
	sort.Slice(vas, func(i, j int) bool { return vas[i] < vas[j] })
	for start := 0; start < len(vas); start += monitorabi.MaxOperations {
		batch := vas[start:min(start+monitorabi.MaxOperations, len(vas))]
		operations := make([]monitorabi.Operation, 0, len(batch))
		for i := 0; i < len(batch); {
			runStart := batch[i]
			i++
			for i < len(batch) && batch[i] == batch[i-1]+hostarch.PageSize {
				i++
			}
			operations = append(operations, monitorabi.Operation{
				Type:   monitorabi.OperationUnmap,
				VA:     uint64(runStart),
				Length: uint64(batch[i-1]-runStart) + hostarch.PageSize,
			})
		}
		if err := as.platform.monitor.Execute(stdcontext.Background(), monitorabi.Command{
			Opcode:         monitorabi.OpcodeApply,
			AddressSpaceID: as.id,
			Operations:     operations,
		}); err != nil {
			as.stickyWriteback = errors.Join(as.stickyWriteback, err)
			return
		}
		// Keep the host mapping index and reverse maps in step with each
		// acknowledged monitor batch, including if a later batch fails.
		for _, va := range batch {
			old := as.mappings[va]
			as.platform.ram.RemoveMapping(old.frame, MappingRef{AddressSpaceID: as.id, VirtualAddress: uint64(va)})
			if err := as.platform.staging.removeMapping(old.staged); err != nil {
				as.stickyWriteback = errors.Join(as.stickyWriteback, err)
			}
			delete(as.mappings, va)
		}
	}
}

func (as *addressSpace) Release() {
	if err := as.platform.lockEpoch(stdcontext.Background()); err != nil {
		as.mu.Lock()
		as.stickyWriteback = errors.Join(as.stickyWriteback, err)
		as.mu.Unlock()
		return
	}
	defer as.platform.epochMu.RUnlock()
	as.mu.Lock()
	if as.released {
		as.mu.Unlock()
		return
	}
	err := as.platform.monitor.Execute(stdcontext.Background(), monitorabi.Command{
		Opcode:         monitorabi.OpcodeDestroyAddressSpace,
		AddressSpaceID: as.id,
	})
	if err != nil {
		as.stickyWriteback = errors.Join(as.stickyWriteback, err)
		as.mu.Unlock()
		return
	}
	for va, mapping := range as.mappings {
		as.platform.ram.RemoveMapping(mapping.frame, MappingRef{AddressSpaceID: as.id, VirtualAddress: uint64(va)})
		if err := as.platform.staging.removeMapping(mapping.staged); err != nil {
			as.stickyWriteback = errors.Join(as.stickyWriteback, err)
		}
	}
	as.mappings = nil
	as.released = true
	as.mu.Unlock()
	as.platform.mu.Lock()
	delete(as.platform.spaces, as.id)
	as.platform.mu.Unlock()
}

// PreFork and PostFork intentionally do not hold as.mu. MemoryManager fork
// establishes CoW by calling AddressSpace.Unmap between these hooks, and
// Unmap must acquire as.mu to issue and record the monitor transaction. Each
// address-space mutation is already serialized by as.mu, while the calling MM
// excludes task-driven mapping changes for the duration of the fork.
func (*addressSpace) PreFork()  {}
func (*addressSpace) PostFork() {}

// SyncRange implements sentryplatform.AddressSpaceCoherence.
func (as *addressSpace) SyncRange(_ context.Context, addr hostarch.Addr, length uint64) error {
	as.mu.Lock()
	defer as.mu.Unlock()
	pages := make([]*stagedPage, 0)
	end := addr + hostarch.Addr(length)
	if end < addr {
		end = ^hostarch.Addr(0)
	}
	for va, mapping := range as.mappings {
		if va >= addr && va < end && mapping.staged != nil {
			pages = append(pages, mapping.staged)
		}
	}
	err := errors.Join(as.stickyWriteback, as.platform.staging.flushPages(pages))
	as.stickyWriteback = nil
	return err
}

func monitorAccess(at hostarch.AccessType) monitorabi.Access {
	var access monitorabi.Access
	if at.Read {
		access |= monitorabi.AccessRead
	}
	if at.Write {
		access |= monitorabi.AccessWrite
	}
	if at.Execute {
		access |= monitorabi.AccessExecute
	}
	return access
}

// RevokeFrames implements FrameRevoker. It is a last-line correctness path;
// ordinary MM teardown removes reverse mappings before pgalloc decommit.
func (p *Platform) RevokeFrames(_ stdcontext.Context, refs map[pgalloc.FrameID][]MappingRef) error {
	if err := p.lockEpoch(stdcontext.Background()); err != nil {
		return err
	}
	defer p.epochMu.RUnlock()
	byAddressSpace := make(map[uint64]map[uint64]pgalloc.FrameID)
	for frameID, frameRefs := range refs {
		for _, ref := range frameRefs {
			vas := byAddressSpace[ref.AddressSpaceID]
			if vas == nil {
				vas = make(map[uint64]pgalloc.FrameID)
				byAddressSpace[ref.AddressSpaceID] = vas
			}
			vas[ref.VirtualAddress] = frameID
		}
	}
	for id, vas := range byAddressSpace {
		p.mu.Lock()
		as := p.spaces[id]
		p.mu.Unlock()
		if as == nil {
			continue
		}
		as.mu.Lock()
		operations := make([]monitorabi.Operation, 0, len(vas))
		for va, frameID := range vas {
			mapping, ok := as.mappings[hostarch.Addr(va)]
			if ok && mapping.frame == frameID {
				operations = append(operations, monitorabi.Operation{Type: monitorabi.OperationUnmap, VA: va, Length: hostarch.PageSize})
			}
		}
		for start := 0; start < len(operations); start += monitorabi.MaxOperations {
			end := min(start+monitorabi.MaxOperations, len(operations))
			if err := p.monitor.Execute(stdcontext.Background(), monitorabi.Command{Opcode: monitorabi.OpcodeApply, AddressSpaceID: id, Operations: operations[start:end]}); err != nil {
				as.mu.Unlock()
				return err
			}
		}
		for va, frameID := range vas {
			mapping, ok := as.mappings[hostarch.Addr(va)]
			if ok && mapping.frame == frameID {
				if err := p.staging.removeMapping(mapping.staged); err != nil {
					as.stickyWriteback = errors.Join(as.stickyWriteback, err)
				}
				delete(as.mappings, hostarch.Addr(va))
			}
		}
		as.mu.Unlock()
	}
	return nil
}

type relocationOperation struct {
	addressSpaceID uint64
	frame          pgalloc.FrameID
	va             uint64
	oldGPA         uint64
	newGPA         uint64
	access         hostarch.AccessType
}

// RelocateFrames implements FrameRelocator. GuestRAM calls this only while
// CompactMemory holds epochMu for writing, so no AddressSpace mutation or
// application execution can race with these break-before-make replacements.
func (p *Platform) RelocateFrames(ctx stdcontext.Context, relocations map[pgalloc.FrameID]FrameRelocation) error {
	byAddressSpace := make(map[uint64][]relocationOperation)
	for frame, relocation := range relocations {
		for _, ref := range relocation.References {
			byAddressSpace[ref.AddressSpaceID] = append(byAddressSpace[ref.AddressSpaceID], relocationOperation{
				addressSpaceID: ref.AddressSpaceID,
				frame:          frame,
				va:             ref.VirtualAddress,
				oldGPA:         relocation.OldGPA,
				newGPA:         relocation.NewGPA,
			})
		}
	}
	ids := make([]uint64, 0, len(byAddressSpace))
	for id := range byAddressSpace {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	spaces := make(map[uint64]*addressSpace, len(ids))
	p.mu.Lock()
	for _, id := range ids {
		spaces[id] = p.spaces[id]
	}
	p.mu.Unlock()
	for _, id := range ids {
		as := spaces[id]
		if as == nil {
			return fmt.Errorf("relocation references missing address space %d", id)
		}
		as.mu.Lock()
	}
	defer func() {
		for i := len(ids) - 1; i >= 0; i-- {
			spaces[ids[i]].mu.Unlock()
		}
	}()
	for _, id := range ids {
		ops := byAddressSpace[id]
		sort.Slice(ops, func(i, j int) bool { return ops[i].va < ops[j].va })
		for i := range ops {
			mapping, ok := spaces[id].mappings[hostarch.Addr(ops[i].va)]
			if !ok || mapping.frame != ops[i].frame {
				return fmt.Errorf("address space %d VA %#x no longer maps frame %d", id, ops[i].va, ops[i].frame)
			}
			ops[i].access = mapping.access
		}
		byAddressSpace[id] = ops
	}

	var applied []relocationOperation
	apply := func(id uint64, ops []relocationOperation, useNew bool) error {
		for start := 0; start < len(ops); start += monitorabi.MaxOperations {
			end := min(start+monitorabi.MaxOperations, len(ops))
			monitorOps := make([]monitorabi.Operation, 0, end-start)
			for _, op := range ops[start:end] {
				gpa := op.oldGPA
				if useNew {
					gpa = op.newGPA
				}
				monitorOps = append(monitorOps, monitorabi.Operation{
					Type: monitorabi.OperationMap, Access: monitorAccess(op.access),
					VA: op.va, GPA: gpa, Length: hostarch.PageSize,
				})
			}
			if err := p.monitor.Execute(ctx, monitorabi.Command{Opcode: monitorabi.OpcodeApply, AddressSpaceID: id, Operations: monitorOps}); err != nil {
				return err
			}
			if useNew {
				applied = append(applied, ops[start:end]...)
			}
		}
		return nil
	}
	for _, id := range ids {
		if err := apply(id, byAddressSpace[id], true); err != nil {
			var rollbackErr error
			for end := len(applied); end > 0; {
				start := end - 1
				id := applied[start].addressSpaceID
				for start > 0 && applied[start-1].addressSpaceID == id {
					start--
				}
				rollbackErr = errors.Join(rollbackErr, apply(id, applied[start:end], false))
				end = start
			}
			return errors.Join(err, rollbackErr)
		}
	}
	return nil
}

func stagedPageAt(pages []*stagedPage, i int) *stagedPage {
	if pages == nil {
		return nil
	}
	return pages[i]
}

type platformContext struct {
	sentryplatform.NoCPUNumbers

	platform  *Platform
	registers any

	mu          sync.Mutex
	cancel      stdcontext.CancelFunc
	interrupted bool
	released    bool
}

func (c *platformContext) Switch(ctx context.Context, mm sentryplatform.MemoryManager, ac *arch.Context64, _ int32) (*linux.SignalInfo, hostarch.AccessType, error) {
	as, ok := mm.AddressSpace().(*addressSpace)
	if !ok || as.platform != c.platform {
		return nil, hostarch.NoAccess, fmt.Errorf("vmplatform Context used with incompatible address space")
	}
	if err := c.platform.lockEpoch(ctx); err != nil {
		if errors.Is(err, stdcontext.Canceled) {
			return nil, hostarch.NoAccess, sentryplatform.ErrContextInterrupt
		}
		return nil, hostarch.NoAccess, err
	}
	defer c.platform.epochMu.RUnlock()
	c.mu.Lock()
	if c.released {
		c.mu.Unlock()
		return nil, hostarch.NoAccess, fmt.Errorf("released vmplatform Context")
	}
	if c.interrupted {
		c.interrupted = false
		c.mu.Unlock()
		return nil, hostarch.NoAccess, sentryplatform.ErrContextInterrupt
	}
	runCtx, cancel := stdcontext.WithCancel(ctx)
	c.cancel = cancel
	c.mu.Unlock()

	if err := c.platform.arch.LoadApplicationState(c.registers, ac); err != nil {
		cancel()
		return nil, hostarch.NoAccess, err
	}
	exit, err := c.platform.pool.RunApplication(runCtx, ApplicationState{AddressSpaceID: as.id, Registers: c.registers})
	c.mu.Lock()
	wasInterrupted := c.interrupted
	c.interrupted = false
	c.cancel = nil
	c.mu.Unlock()
	cancel()
	if err := c.platform.arch.SaveApplicationState(ac, c.registers, true); err != nil {
		return nil, hostarch.NoAccess, err
	}
	if err != nil {
		if wasInterrupted || errors.Is(err, stdcontext.Canceled) {
			return nil, hostarch.NoAccess, sentryplatform.ErrContextInterrupt
		}
		return nil, hostarch.NoAccess, err
	}
	// Synchronous exceptions win races with Context.Interrupt. The monitor has
	// already advanced PC past SVC (or captured an exact fault PC), so dropping
	// this exit would skip syscall emulation and leave X0 holding its argument.
	if exit.Reason == ExitSyscall {
		return nil, hostarch.NoAccess, nil
	}
	if exit.Reason == ExitInterrupted {
		return nil, hostarch.NoAccess, sentryplatform.ErrContextInterrupt
	}
	return c.platform.arch.DecodeExit(exit)
}

func (*platformContext) PullFullState(sentryplatform.AddressSpace, *arch.Context64) error { return nil }
func (*platformContext) FullStateChanged()                                                {}
func (c *platformContext) Interrupt() {
	c.mu.Lock()
	c.interrupted = true
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
}
func (c *platformContext) Preempt() { c.Interrupt() }
func (c *platformContext) Release() {
	c.Interrupt()
	c.mu.Lock()
	c.released = true
	c.mu.Unlock()
}
func (*platformContext) PrepareSleep()                {}
func (*platformContext) PrepareUninterruptibleSleep() {}
func (*platformContext) PrepareStop()                 {}
func (*platformContext) PrepareExecve()               {}
func (*platformContext) PrepareExit()                 {}
