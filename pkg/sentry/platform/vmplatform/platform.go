// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vmplatform

import (
	stdcontext "context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/google/btree"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/log"
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
	// VMEngineFactory selects the VM host execution policy independently from
	// the VM memory and address-space implementation.
	VMEngineFactory VMEngineFactory
}

// Platform implements sentryplatform.Platform using an EL1 monitor and
// GuestRAM. It contains no host-hypervisor API calls.
type Platform struct {
	sentryplatform.NoCPUPreemptionDetection
	sentryplatform.NoCPUNumbers

	vm        VM
	arch      Architecture
	engine    VMEngine
	monitor   *Monitor
	ram       *GuestRAM
	staging   *stagingCache
	maxUser   hostarch.Addr
	name      string
	newVM     func(stdcontext.Context) (VM, error)
	newEngine VMEngineFactory
	vcpus     int

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

	// hostMemoryMu protects hostMemories, the parts of each
	// memmap.HostMemoryFile that guests may map, which the platform maps in
	// place until the file is released.
	hostMemoryMu sync.Mutex
	hostMemories map[memmap.HostMemoryFile]hostRange
}

// hostRange is a range of host addresses.
type hostRange struct {
	start, end uintptr
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
	engineFactory := config.VMEngineFactory
	if engineFactory == nil {
		_ = config.VM.Close()
		return nil, fmt.Errorf("vmplatform requires a VM engine factory")
	}
	engine, err := engineFactory(config.VM, config.VCPUs)
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
		_ = engine.Close()
		_ = config.VM.Close()
		return nil, err
	}
	staging, err := newStagingCache(ram)
	if err != nil {
		_ = engine.Close()
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
		vm:        config.VM,
		arch:      config.Architecture,
		engine:    engine,
		monitor:   NewMonitor(engine, monitorPoolExpander(config.VM)),
		ram:       ram,
		staging:   staging,
		maxUser:   maxUser,
		name:      name,
		newVM:     config.NewVM,
		newEngine: engineFactory,
		vcpus:     config.VCPUs,
		spaces:    make(map[uint64]*addressSpace),

		hostMemories: make(map[memmap.HostMemoryFile]hostRange),
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
	p.engine.InterruptApplications()
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
func (*Platform) MapUnit() uint64 {
	// Unlike KVM, the v1 VM platform builds 4K leaves and can mutate at most
	// monitorabi.MaxOperations pages per EL1 transaction. KVM's 16 MiB unit
	// turns a minor fault into thousands of PTE writes and many monitor entries.
	// Use exactly one monitor transaction so VM gofer readahead can amortize
	// executable faults without constructing an unbounded number of leaves.
	return monitorabi.MaxOperations * hostarch.PageSize
}
func (*Platform) MinUserAddress() hostarch.Addr   { return hostarch.PageSize }
func (p *Platform) MaxUserAddress() hostarch.Addr { return p.maxUser }

// ConcurrencyCount returns the application concurrency of the selected
// execution engine.
// +checklocksignore: lockEpoch conditionally returns with epochMu read-locked.
func (p *Platform) ConcurrencyCount() int {
	if err := p.lockEpoch(stdcontext.Background()); err != nil {
		return 0
	}
	count := p.engine.ConcurrencyCount()
	p.epochMu.RUnlock()
	return count
}
func (p *Platform) SeccompInfo() sentryplatform.SeccompInfo {
	return sentryplatform.StaticSeccompInfo{PlatformName: p.name}
}

// NewAddressSpace creates a monitor-owned application translation root.
// +checklocksignore: lockEpoch conditionally returns with epochMu read-locked.
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
		// fork, clone, and execve return this error to the application, so it
		// must carry a Linux errno.
		var statusErr *MonitorStatusError
		if errors.As(err, &statusErr) && statusErr.Response.Status == monitorabi.StatusLimit {
			// Linux reports exhausted process resources from fork as EAGAIN.
			log.Warningf("vmplatform: address-space limit reached: %v", err)
			return nil, linuxerr.EAGAIN
		}
		log.Warningf("vmplatform: creating address space: %v", err)
		return nil, linuxerr.ENOMEM
	}
	as := &addressSpace{platform: p, id: id, mappings: make(map[hostarch.Addr]addressMapping), mapped: newMappedSet()}
	p.mu.Lock()
	p.spaces[id] = as
	p.mu.Unlock()
	return as, nil
}

func (p *Platform) NewContext(context.Context) sentryplatform.Context {
	return &platformContext{platform: p, registers: p.arch.NewVCPUState()}
}

// FlushFile implements sentryplatform.FileCoherence.FlushFile.
func (p *Platform) FlushFile(file memmap.File, fr memmap.FileRange) error {
	return p.staging.flushFile(file, fr)
}

// UpdateFile implements sentryplatform.FileCoherence.UpdateFile.
func (p *Platform) UpdateFile(file memmap.File, offset uint64, data []byte) error {
	return p.staging.updateFile(file, offset, data)
}

var _ sentryplatform.FileCoherence = (*Platform)(nil)

// FlushForCheckpoint implements sentryplatform.CheckpointCoherence. The kernel
// is quiesced before this call; GuestRAM itself is already coherent.
// +checklocksignore: lockEpoch conditionally returns with epochMu read-locked.
func (p *Platform) FlushForCheckpoint(ctx context.Context) error {
	if err := p.lockEpoch(stdcontext.Background()); err != nil {
		return err
	}
	defer p.epochMu.RUnlock()
	if err := p.staging.flushAll(); err != nil {
		return err
	}
	if cache, ok := p.engine.(HostExecutionCacheEngine); ok {
		if err := cache.FlushHostExecutionCache(ctx); err != nil {
			return fmt.Errorf("flushing VM host execution cache: %w", err)
		}
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
	engineErr := p.engine.Close()
	p.epochMu.Lock()
	defer p.epochMu.Unlock()
	vmErr := p.vm.Close()
	if engineErr != nil {
		if vmErr != nil {
			return fmt.Errorf("closing execution engine: %v; closing VM: %w", engineErr, vmErr)
		}
		return fmt.Errorf("closing execution engine: %w", engineErr)
	}
	return vmErr
}

func (p *Platform) lockEpoch(ctx stdcontext.Context) error {
	return p.lockEpochBeforeWait(ctx, nil)
}

// lockEpochBeforeWait is lockEpoch, but calls beforeWait, if non-nil,
// whenever it is about to block on an epoch transition.
// +checklocksignore: returns with epochMu read-locked on success.
func (p *Platform) lockEpochBeforeWait(ctx stdcontext.Context, beforeWait func()) error {
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
		if beforeWait != nil {
			beforeWait()
		}
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
	oldEngine := p.engine
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
	if err := oldEngine.Close(); err != nil {
		return fmt.Errorf("closing old execution engine: %w", err)
	}
	p.epochMu.Lock()
	defer p.epochMu.Unlock()
	if err := oldVM.Close(); err != nil {
		return fmt.Errorf("closing old VM epoch: %w", err)
	}

	newVM, err := p.newVM(stdcontext.Background())
	if err != nil {
		return fmt.Errorf("creating replacement VM epoch: %w", err)
	}
	newEngine, err := p.newEngine(newVM, p.vcpus)
	if err != nil {
		_ = newVM.Close()
		return fmt.Errorf("creating replacement execution engine: %w", err)
	}
	newMonitor := NewMonitor(newEngine, monitorPoolExpander(newVM))
	plan, err := ram.beginEpochRebind(newVM)
	if err != nil {
		_ = newEngine.Close()
		_ = newVM.Close()
		return err
	}
	abort := true
	defer func() {
		if abort {
			_ = plan.abort()
			_ = newEngine.Close()
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
	p.engine = newEngine
	p.monitor = newMonitor
	p.mu.Unlock()
	return nil
}

type frameResolver interface {
	AcquireBackingFrames(memmap.FileRange, hostarch.AccessType) ([]pgalloc.BackingFrame, memmap.InternalMappingLease, error)
}

type addressMapping struct {
	frame  pgalloc.FrameID
	access hostarch.AccessType
	staged *stagedPage
}

type stage1MapRun struct {
	operation  monitorabi.Operation
	firstFrame int
	endFrame   int
}

// stage1MapRuns combines physically contiguous GuestRAM frames into one
// monitor operation. Page-cache fill allocates in slab order, so executable
// readahead normally becomes a single operation instead of one ABI entry per
// 4K page. The monitor still constructs 4K stage-1 leaves and performs the
// architected invalidation sequence for the complete range.
func stage1MapRuns(addr hostarch.Addr, frames []pgalloc.BackingFrame, access monitorabi.Access) []stage1MapRun {
	runs := make([]stage1MapRun, 0, len(frames))
	for i, frame := range frames {
		va := uint64(addr) + uint64(i)*hostarch.PageSize
		if len(runs) != 0 {
			last := &runs[len(runs)-1]
			if last.operation.VA+last.operation.Length == va &&
				last.operation.GPA+last.operation.Length == frame.GPA &&
				last.operation.Length < monitorabi.MaxOperations*hostarch.PageSize {
				last.operation.Length += hostarch.PageSize
				last.endFrame = i + 1
				continue
			}
		}
		runs = append(runs, stage1MapRun{
			operation: monitorabi.Operation{
				Type:   monitorabi.OperationMap,
				Access: access,
				VA:     va,
				GPA:    frame.GPA,
				Length: hostarch.PageSize,
			},
			firstFrame: i,
			endFrame:   i + 1,
		})
	}
	return runs
}

type addressSpace struct {
	sentryplatform.NoAddressSpaceIO

	platform *Platform
	id       uint64

	mu       sync.Mutex
	mappings map[hostarch.Addr]addressMapping
	// mapped holds the keys of mappings in order: unmapping a range, as each
	// of a process's mappings does before a live fork, must not scan every
	// page that the address space maps.
	mapped          *btree.BTreeG[hostarch.Addr]
	released        bool
	stickyWriteback error

	// forking is whether a fork establishes copy-on-write, between PreFork
	// and PostFork, and deferred the pages that Unmap unmaps meanwhile,
	// which PostFork unmaps in as few monitor transactions as they need,
	// rather than one or more for each of a process's writable mappings.
	forking  bool
	deferred []hostarch.Addr
}

func newMappedSet() *btree.BTreeG[hostarch.Addr] {
	return btree.NewG(32, func(a, b hostarch.Addr) bool { return a < b })
}

// mappedIn returns the VAs in [addr, end) that as maps, in order.
//
// Preconditions: as.mu is locked.
func (as *addressSpace) mappedIn(addr, end hostarch.Addr) []hostarch.Addr {
	var vas []hostarch.Addr
	if as.mapped == nil { // Released.
		return nil
	}
	as.mapped.AscendRange(addr, end, func(va hostarch.Addr) bool {
		vas = append(vas, va)
		return true
	})
	return vas
}

// +checklocksignore: lockEpoch conditionally returns with epochMu read-locked.
func (as *addressSpace) MapFile(addr hostarch.Addr, file memmap.File, fr memmap.FileRange, at hostarch.AccessType, _ bool) (retErr error) {
	if addr%hostarch.PageSize != 0 || fr.Start%hostarch.PageSize != 0 || fr.Length() == 0 || fr.End%hostarch.PageSize != 0 {
		return fmt.Errorf("invalid vmplatform mapping addr=%#x range=%v", addr, fr)
	}
	var frames []pgalloc.BackingFrame
	var frameLease memmap.InternalMappingLease
	var stagedPages []*stagedPage
	for {
		before := as.platform.ram.Metrics().Epoch
		layoutBefore := as.platform.ram.layoutGeneration()
		var err error
		if resolver, ok := file.(frameResolver); ok {
			frames, frameLease, err = resolver.AcquireBackingFrames(fr, at)
			stagedPages = nil
		} else if hostMemory, ok := file.(memmap.HostMemoryFile); ok {
			frames, err = as.platform.hostMemoryFrames(hostMemory, fr, at)
			frameLease = nil
			stagedPages = nil
		} else {
			frames, stagedPages, err = as.platform.staging.resolve(file, fr, at)
			frameLease = nil
		}
		if err != nil {
			return err
		}
		epoch := as.platform.ram.Metrics().Epoch
		if epoch != before {
			if frameLease != nil {
				frameLease.Release()
				frameLease = nil
			}
			if err := as.platform.staging.cancelReservations(stagedPages, at.Write); err != nil {
				return err
			}
			continue
		}
		if err := as.platform.lockEpoch(stdcontext.Background()); err != nil {
			if frameLease != nil {
				frameLease.Release()
				frameLease = nil
			}
			return errors.Join(err, as.platform.staging.cancelReservations(stagedPages, at.Write))
		}
		if as.platform.ram.Metrics().Epoch == epoch && as.platform.ram.layoutGeneration() == layoutBefore {
			break
		}
		as.platform.epochMu.RUnlock()
		if frameLease != nil {
			frameLease.Release()
			frameLease = nil
		}
		if err := as.platform.staging.cancelReservations(stagedPages, at.Write); err != nil {
			return err
		}
	}
	// Register reservation cleanup before epoch unlock so return processing
	// first releases as.mu and epochMu, then performs any potentially blocking
	// GuestRAM decommit.
	defer func() {
		retErr = errors.Join(retErr, as.platform.staging.cancelReservations(stagedPages, at.Write))
	}()
	var retiredMappings []addressMapping
	defer func() {
		for _, retired := range retiredMappings {
			if err := as.platform.staging.removeMapping(retired.staged, retired.access.Write); err != nil {
				as.mu.Lock()
				as.stickyWriteback = errors.Join(as.stickyWriteback, err)
				as.mu.Unlock()
			}
		}
	}()
	defer func() {
		if frameLease != nil {
			frameLease.Release()
		}
	}()
	defer as.platform.epochMu.RUnlock()
	if uint64(len(frames))*hostarch.PageSize != fr.Length() {
		return fmt.Errorf("GuestRAM returned %d frames for %d-byte mapping", len(frames), fr.Length())
	}
	runs := stage1MapRuns(addr, frames, monitorAccess(at))

	as.mu.Lock()
	defer as.mu.Unlock()
	if as.released {
		return fmt.Errorf("mapping released vmplatform address space")
	}
	for start := 0; start < len(runs); {
		end := start
		pages := uint64(0)
		for end < len(runs) {
			runPages := runs[end].operation.Length / hostarch.PageSize
			if pages != 0 && pages+runPages > monitorabi.MaxOperations {
				break
			}
			pages += runPages
			end++
		}
		operations := make([]monitorabi.Operation, end-start)
		for i := start; i < end; i++ {
			operations[i-start] = runs[i].operation
		}
		firstFrame := runs[start].firstFrame
		endFrame := runs[end-1].endFrame
		prepared := make([]int, 0, endFrame-firstFrame)
		for i := firstFrame; i < endFrame; i++ {
			va := addr + hostarch.Addr(uint64(i)*hostarch.PageSize)
			if old, ok := as.mappings[va]; ok && old.frame == frames[i].ID {
				continue
			}
			ref := MappingRef{AddressSpaceID: as.id, VirtualAddress: uint64(va)}
			if err := as.platform.ram.AddMapping(frames[i].ID, ref); err != nil {
				as.rollbackPreparedMappings(addr, frames, stagedPages, prepared, at.Write)
				return err
			}
			staged := stagedPageAt(stagedPages, i)
			if err := as.platform.staging.commitMapping(staged); err != nil {
				as.platform.ram.RemoveMapping(frames[i].ID, ref)
				as.rollbackPreparedMappings(addr, frames, stagedPages, prepared, at.Write)
				return err
			}
			prepared = append(prepared, i)
		}
		if err := as.platform.monitor.Execute(stdcontext.Background(), monitorabi.Command{
			Opcode:         monitorabi.OpcodeApply,
			AddressSpaceID: as.id,
			Operations:     operations,
		}); err != nil {
			as.rollbackPreparedMappings(addr, frames, stagedPages, prepared, at.Write)
			return err
		}
		for i := firstFrame; i < endFrame; i++ {
			va := addr + hostarch.Addr(uint64(i)*hostarch.PageSize)
			if old, ok := as.mappings[va]; ok {
				if old.frame == frames[i].ID {
					// The existing reverse/staging references already own this
					// frame. Leave the duplicate staging reservation for deferred
					// cancellation and only update the PTE permissions.
					if err := as.platform.staging.setMappingWritable(old.staged, old.access.Write, at.Write); err != nil {
						as.stickyWriteback = errors.Join(as.stickyWriteback, err)
					}
					old.access = at
					as.mappings[va] = old
					continue
				}
				as.platform.ram.RemoveMapping(old.frame, MappingRef{AddressSpaceID: as.id, VirtualAddress: uint64(va)})
				if old.staged != nil {
					retiredMappings = append(retiredMappings, old)
				}
			}
			staged := stagedPageAt(stagedPages, i)
			as.mappings[va] = addressMapping{frame: frames[i].ID, access: at, staged: staged}
			as.mapped.ReplaceOrInsert(va)
			if staged != nil {
				stagedPages[i] = nil
			}
		}
		start = end
	}
	return nil
}

// rollbackPreparedMappings removes ownership acquired before a monitor
// transaction that failed without publishing any of its PTE changes.
func (as *addressSpace) rollbackPreparedMappings(addr hostarch.Addr, frames []pgalloc.BackingFrame, stagedPages []*stagedPage, prepared []int, writable bool) {
	for i := len(prepared) - 1; i >= 0; i-- {
		frameIndex := prepared[i]
		va := addr + hostarch.Addr(uint64(frameIndex)*hostarch.PageSize)
		as.platform.ram.RemoveMapping(frames[frameIndex].ID, MappingRef{AddressSpaceID: as.id, VirtualAddress: uint64(va)})
		staged := stagedPageAt(stagedPages, frameIndex)
		if err := as.platform.staging.removeMapping(staged, writable); err != nil {
			as.stickyWriteback = errors.Join(as.stickyWriteback, err)
		}
		if staged != nil {
			// commitMapping consumed this reservation; prevent the deferred
			// cancellation path from dropping it a second time.
			stagedPages[frameIndex] = nil
		}
	}
}

// +checklocksignore: lockEpoch conditionally returns with epochMu read-locked.
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
	vas := as.mappedIn(addr, end)
	if as.forking {
		as.deferred = append(as.deferred, vas...)
		return
	}
	as.unmapLocked(vas)
}

// unmapLocked unmaps the pages at vas, which as maps, in order.
//
// Preconditions: as.mu is locked, and as.platform.epochMu read-locked.
func (as *addressSpace) unmapLocked(vas []hostarch.Addr) {
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
			if err := as.platform.staging.removeMapping(old.staged, old.access.Write); err != nil {
				as.stickyWriteback = errors.Join(as.stickyWriteback, err)
			}
			delete(as.mappings, va)
			as.mapped.Delete(va)
		}
	}
}

// +checklocksignore: lockEpoch conditionally returns with epochMu read-locked.
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
		if err := as.platform.staging.removeMapping(mapping.staged, mapping.access.Write); err != nil {
			as.stickyWriteback = errors.Join(as.stickyWriteback, err)
		}
	}
	as.mappings = nil
	as.mapped = nil
	as.released = true
	as.mu.Unlock()
	as.platform.mu.Lock()
	delete(as.platform.spaces, as.id)
	as.platform.mu.Unlock()
}

// PreFork and PostFork intentionally do not hold as.mu between them.
// MemoryManager fork establishes CoW by calling AddressSpace.Unmap between
// these hooks, and Unmap must acquire as.mu. Each address-space mutation is
// already serialized by as.mu, while the calling MM excludes task-driven
// mapping changes for the duration of the fork. Unmap defers its unmaps to
// PostFork, which unmaps them at once, before the fork returns: a page that
// another thread mapped meanwhile is unmapped too, which only costs a fault.
func (as *addressSpace) PreFork() {
	as.mu.Lock()
	as.forking = true
	as.mu.Unlock()
}

// PostFork implements sentryplatform.AddressSpace.PostFork.
//
// +checklocksignore: lockEpoch conditionally returns with epochMu read-locked.
func (as *addressSpace) PostFork() {
	as.mu.Lock()
	as.forking = false
	vas := as.deferred
	as.deferred = nil
	as.mu.Unlock()
	if len(vas) == 0 {
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
	// Ranges of separate Unmaps may overlap or come out of order; pages
	// that another thread unmapped or remapped meanwhile are as they are
	// now.
	sort.Slice(vas, func(i, j int) bool { return vas[i] < vas[j] })
	kept := vas[:0]
	for i, va := range vas {
		if i > 0 && va == vas[i-1] {
			continue
		}
		if _, ok := as.mappings[va]; ok {
			kept = append(kept, va)
		}
	}
	as.unmapLocked(kept)
}

// SyncRange implements sentryplatform.AddressSpaceCoherence.
func (as *addressSpace) SyncRange(_ context.Context, addr hostarch.Addr, length uint64) error {
	as.mu.Lock()
	defer as.mu.Unlock()
	pages := make([]*stagedPage, 0)
	end := addr + hostarch.Addr(length)
	if end < addr {
		end = ^hostarch.Addr(0)
	}
	for _, va := range as.mappedIn(addr, end) {
		if mapping := as.mappings[va]; mapping.staged != nil {
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
// +checklocksignore: lockEpoch conditionally returns with epochMu read-locked.
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
				if err := p.staging.removeMapping(mapping.staged, mapping.access.Write); err != nil {
					as.stickyWriteback = errors.Join(as.stickyWriteback, err)
				}
				delete(as.mappings, hostarch.Addr(va))
				as.mapped.Delete(hostarch.Addr(va))
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
// +checklocksignore: address-space locks are selected and ordered dynamically.
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

	mu         sync.Mutex
	runContext applicationRunContext
	runActive  bool
	// runOwner identifies the host thread running the active application run
	// for ApplicationInterruptEngine.
	runOwner    uint64
	interrupted bool
	released    bool
	resident    ApplicationResidencyEngine
	residentErr error
}

// beginApplicationResidency pins owner-affine VM state to the task's current
// host thread. The task goroutine remains resident while it handles a burst of
// Linux syscalls, avoiding a host hypervisor exit and vCPU reconstruction at
// every application entry.
func (c *platformContext) beginApplicationResidency(engine VMEngine) error {
	want, supported := engine.(ApplicationResidencyEngine)
	c.mu.Lock()
	current := c.resident
	residentErr := c.residentErr
	released := c.released
	c.mu.Unlock()
	if released {
		return fmt.Errorf("released vmplatform Context")
	}
	if current == want {
		return residentErr
	}
	if current != nil {
		if err := c.endApplicationResidency(); err != nil {
			return err
		}
	}
	if !supported {
		return residentErr
	}
	want.BeginApplicationResidency()
	c.mu.Lock()
	if c.released {
		c.mu.Unlock()
		if err := want.EndApplicationResidency(); err != nil {
			return errors.Join(fmt.Errorf("released vmplatform Context"), err)
		}
		return fmt.Errorf("released vmplatform Context")
	}
	c.resident = want
	residentErr = c.residentErr
	c.mu.Unlock()
	return residentErr
}

// endApplicationResidency must be called by the task goroutine that began the
// residency. platform.Context's preparation hooks satisfy that requirement.
func (c *platformContext) endApplicationResidency() error {
	c.mu.Lock()
	engine := c.resident
	c.resident = nil
	c.mu.Unlock()
	if engine == nil {
		return nil
	}
	err := engine.EndApplicationResidency()
	if err != nil {
		c.mu.Lock()
		c.residentErr = errors.Join(c.residentErr, err)
		c.mu.Unlock()
	}
	return err
}

// yieldApplicationResidency ends an owner-affine residency only when the
// selected backend reports actual contention. It must run on the task
// goroutine, immediately after application state has returned to host Sentry.
func (c *platformContext) yieldApplicationResidency() error {
	c.mu.Lock()
	engine := c.resident
	c.mu.Unlock()
	yielder, ok := engine.(ApplicationResidencyYieldEngine)
	if !ok || !yielder.ShouldYieldApplicationResidency() {
		return nil
	}
	return c.endApplicationResidency()
}

// completeApplicationExitResidency retains an owner-affine vCPU across Linux
// syscall handling and, for engines with bounded admission, local fault
// handling. Engines that cannot reserve progress capacity still end fault
// residency here because remote page acquisition does not necessarily block
// through Task.Block.
func (c *platformContext) completeApplicationExitResidency(reason ExitReason) error {
	if reason == ExitSyscall {
		return c.yieldApplicationResidency()
	}
	if reason == ExitFault {
		c.mu.Lock()
		engine := c.resident
		c.mu.Unlock()
		if retainer, ok := engine.(ApplicationFaultResidencyEngine); ok && retainer.ShouldRetainApplicationFaultResidency() {
			return c.yieldApplicationResidency()
		}
	}
	return c.endApplicationResidency()
}

// +checklocksignore: lockEpoch conditionally returns with epochMu read-locked.
func (c *platformContext) Switch(ctx context.Context, mm sentryplatform.MemoryManager, ac *arch.Context64, _ int32) (*linux.SignalInfo, hostarch.AccessType, error) {
	as, ok := mm.AddressSpace().(*addressSpace)
	if !ok || as.platform != c.platform {
		return nil, hostarch.NoAccess, fmt.Errorf("vmplatform Context used with incompatible address space")
	}
	// Closing the old engine during a transition must retire this context's
	// owner-affine vCPU, which only its owner thread can do. Do it here rather
	// than hold the vCPU while blocked on the transition, which would make the
	// transition time out and fail the platform.
	if err := c.platform.lockEpochBeforeWait(ctx, func() { _ = c.endApplicationResidency() }); err != nil {
		if errors.Is(err, stdcontext.Canceled) {
			return nil, hostarch.NoAccess, sentryplatform.ErrContextInterrupt
		}
		return nil, hostarch.NoAccess, err
	}
	defer c.platform.epochMu.RUnlock()
	engine := c.platform.engine
	if err := c.beginApplicationResidency(engine); err != nil {
		return nil, hostarch.NoAccess, err
	}
	c.mu.Lock()
	if c.released {
		c.mu.Unlock()
		return nil, hostarch.NoAccess, fmt.Errorf("released vmplatform Context")
	}
	if c.interrupted {
		c.interrupted = false
		c.mu.Unlock()
		// There is no application burst to amortize after consuming an interrupt.
		// In particular, restored tasks are commonly interrupted before their
		// first entry; retaining one vCPU per such task can exhaust the owner-
		// affine pool before any task becomes runnable.
		if err := c.endApplicationResidency(); err != nil {
			return nil, hostarch.NoAccess, err
		}
		// kernel.runApp compares this sentinel by identity, so it must not be
		// wrapped even when residency teardown succeeded.
		return nil, hostarch.NoAccess, sentryplatform.ErrContextInterrupt
	}
	c.runContext.reset(ctx)
	c.runActive = true
	if engine, ok := engine.(ApplicationInterruptEngine); ok {
		c.runOwner = engine.ApplicationOwner()
	}
	c.mu.Unlock()

	if err := c.platform.arch.LoadApplicationState(c.registers, ac); err != nil {
		c.finishApplicationRun()
		return nil, hostarch.NoAccess, errors.Join(err, c.endApplicationResidency())
	}
	exit, err := engine.RunApplication(&c.runContext, ApplicationState{AddressSpaceID: as.id, Registers: c.registers})
	wasInterrupted := c.finishApplicationRun()
	if err := c.platform.arch.SaveApplicationState(ac, c.registers, true); err != nil {
		return nil, hostarch.NoAccess, errors.Join(err, c.endApplicationResidency())
	}
	if err != nil {
		if wasInterrupted || errors.Is(err, stdcontext.Canceled) {
			return nil, hostarch.NoAccess, sentryplatform.ErrContextInterrupt
		}
		return nil, hostarch.NoAccess, errors.Join(err, c.endApplicationResidency())
	}
	if err := c.completeApplicationExitResidency(exit.Reason); err != nil {
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

func (c *platformContext) finishApplicationRun() bool {
	c.mu.Lock()
	wasInterrupted := c.interrupted
	c.interrupted = false
	c.runActive = false
	c.mu.Unlock()
	c.runContext.finish()
	return wasInterrupted
}

func (*platformContext) PullFullState(sentryplatform.AddressSpace, *arch.Context64) error { return nil }
func (*platformContext) FullStateChanged()                                                {}
func (c *platformContext) Interrupt() {
	c.mu.Lock()
	c.interrupted = true
	running := c.runActive
	owner := c.runOwner
	if running {
		c.runContext.cancel()
	}
	c.mu.Unlock()
	if !running {
		// Switch consumes interrupted before its next application entry.
		return
	}
	// Canceling the Go context alone cannot wake a running EL0 task. Request an
	// architected guest exit after publishing interrupted so Switch observes a
	// coherent register snapshot.
	if engine, ok := c.platform.engine.(ApplicationInterruptEngine); ok {
		engine.InterruptApplication(owner)
		return
	}
	c.platform.engine.InterruptApplications()
}
func (c *platformContext) Preempt() { c.Interrupt() }
func (c *platformContext) Release() {
	c.Interrupt()
	c.mu.Lock()
	c.released = true
	c.mu.Unlock()
	_ = c.endApplicationResidency()
}
func (c *platformContext) PrepareSleep()                { _ = c.endApplicationResidency() }
func (c *platformContext) PrepareUninterruptibleSleep() { _ = c.endApplicationResidency() }
func (c *platformContext) PrepareStop()                 { _ = c.endApplicationResidency() }
func (c *platformContext) PrepareExecve()               { _ = c.endApplicationResidency() }
func (c *platformContext) PrepareExit()                 { _ = c.endApplicationResidency() }
