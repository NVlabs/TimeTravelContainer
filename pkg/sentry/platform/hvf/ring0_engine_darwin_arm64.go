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

//go:build darwin && arm64 && cgo

package hvf

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/hosttid"
	"gvisor.dev/gvisor/pkg/sentry/platform/hvf/ring0asm"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

const (
	ring0SentryAddressSpaceID   = ^uint64(0) // Sentry mirror and qualification only.
	ring0HostVALimit            = uint64(1 << 48)
	ring0HostMirrorGPAStart     = SentryMirrorIPAStart
	ring0HostWorkingSetRadius   = uint64(256 << 10)
	ring0MonitorVCPUID          = 63
	stateApplicationFP          = 0x3000
	stateRing0Entry             = 0x3340
	stateTLBFlushRequested      = 0x2a68
	stateVTimerOffset           = 0x3360
	stateClockGettimePC         = 0x3368
	stateMachAbsoluteTimePC     = 0x3370
	stateTimebaseNumer          = 0x3378
	stateTimebaseDenom          = 0x3380
	stateRealtimeOffsetNS       = 0x3388
	stateGuestCritical          = 0x3390
	applicationResidencyQuantum = time.Millisecond
)

// MaxApplicationVCPUs is the number of HVF vCPU IDs available to application
// continuations. The final architected ID is reserved for the EL1 monitor.
const MaxApplicationVCPUs = ring0MonitorVCPUID

type ring0ApplicationCPU struct {
	vcpu              *hvfVCPU
	bridge            *ring0Bridge
	tid               uint64
	uses              atomic.Int64
	retiring          atomic.Bool
	applicationActive atomic.Bool
	entryActive       atomic.Bool
	tlbGeneration     uint64
	residentSince     time.Time
}

// tryPin prevents bridge reclamation while a caller uses cpu. A negative
// reference count permanently marks the bridge retired; this closes the race
// between a lock-free owner-thread lookup and removal from the residency map.
func (cpu *ring0ApplicationCPU) tryPin() bool {
	for {
		uses := cpu.uses.Load()
		if uses < 0 {
			return false
		}
		if cpu.uses.CompareAndSwap(uses, uses+1) {
			return true
		}
	}
}

// ring0ApplicationWaiter is intentionally non-zero-sized so pointer identity
// is stable when entries are removed from the FIFO.
type ring0ApplicationWaiter struct {
	token byte
}

type ring0Stage2Identity struct {
	objectID     uint64
	objectOffset uint64
	access       vmm.Stage2Access
	mainImage    bool
}

type ring0Stage2Lease struct {
	hostAddress uintptr
	length      uint64
	shadow      bool
	tracked     bool
	refs        int
}

type ring0Stage2Slab struct {
	ring0Stage2Identity
	lease     *ring0Stage2Lease
	gpa       uint64
	canonical bool
}

// ring0QuiescedMonitorExecutor bypasses the public mutation rendezvous after
// its caller has already established the required exclusion. Keeping this
// executor separate prevents Monitor's pool-expansion path from recursively
// entering the translation-mutation rendezvous.
type ring0QuiescedMonitorExecutor struct {
	engine *ring0Engine
}

func (x ring0QuiescedMonitorExecutor) RunMonitor(ctx context.Context, encoded []byte) ([]byte, error) {
	return x.engine.runMonitorCommand(ctx, encoded)
}

// ring0Engine keeps hypervisor mechanics below vmplatform.VMEngine. GuestRAM,
// application roots, staging, checkpoint policy, and Linux MM remain in the
// portable layer; this type owns only the EL1 dispatcher and vCPU residency.
type ring0Engine struct {
	vm               *hvfVM
	monitorPool      *vmplatform.WorkerPool
	monitor          *vmplatform.Monitor
	concurrency      int
	monitorMu        sync.Mutex
	timeFastpath     ring0TimeFastpath
	realtimeOffsetNS atomic.Int64
	clockStop        chan struct{}
	clockDone        chan struct{}

	// entryMutationMu serializes translation mutations. entryBlocked and each
	// vCPU's entryActive implement a lock-free read-side rendezvous: ordinary
	// syscalls touch only owner-local state instead of bouncing sync.RWMutex's
	// global reader counter between physical CPUs.
	entryMutationMu sync.Mutex
	entryBlocked    atomic.Bool
	mirrorMu        sync.Mutex
	stage2Slabs     map[uintptr]ring0Stage2Slab
	stage1Access    map[uint64]monitorabi.Access
	applicationGPAs map[uint64]map[uint64]uint64
	nextMirrorGPA   uint64
	faultMu         sync.Mutex
	pendingMirror   map[uintptr]struct{}
	sentryTTBR0     uint64
	// mirrorInitialized is protected by mirrorMu. Once the initial Sentry
	// working set is installed, refreshHostMirror can ignore host-service gates
	// that only sampled slabs which are already present. This is essential for
	// keeping ordinary host-dependent Sentry operations out of the Mach VM-map
	// reconciliation path.
	mirrorInitialized bool

	cpuMu      sync.Mutex
	cpusByID   [ring0MonitorVCPUID]*ring0ApplicationCPU
	cpusByTID  sync.Map // map[uint64]*ring0ApplicationCPU; owner hot path.
	cpuIDs     []bool
	cpuWaiters []*ring0ApplicationWaiter
	closed     atomic.Bool
	closeMu    sync.Mutex
	closeErr   error

	// mmuGeneration changes after every acknowledged monitor transaction.
	// Application vCPUs locally invalidate stage-1 translations on their first
	// subsequent entry, avoiding dependence on HVF cross-vCPU TLBI delivery.
	mmuGeneration atomic.Uint64

	// applicationWaiters is non-zero only when every owner-affine application
	// vCPU is leased. Incumbents consult it after returning to host Sentry so
	// the uncontended syscall path retains its long-lived residency.
	applicationWaiters atomic.Int64

	// applicationCPUs counts owner-affine vCPU residencies, including tasks
	// that are between application entries in host Sentry. It is diagnostic
	// state only; applicationWaiters drives admission handoff so a fully leased
	// but uncontended pool retains its syscall-fast residencies.
	applicationCPUs atomic.Int64
}

// NewVMEngine constructs the Darwin Sentry-at-EL1 dispatcher for the
// vmplatform/hvf registration package. Application exceptions resume the
// calling Sentry continuation inside the VM; host-dependent work redpills that
// continuation through the bounded host-service gate.
func NewVMEngine(vmInterface vmm.VM, vcpus int) (vmplatform.VMEngine, error) {
	vm, ok := vmInterface.(*hvfVM)
	if !ok {
		return nil, fmt.Errorf("HVF ring0 engine requires *hvfVM, got %T", vmInterface)
	}
	if vcpus <= 0 {
		return nil, fmt.Errorf("invalid HVF ring0 vCPU count %d", vcpus)
	}
	if vcpus > ring0MonitorVCPUID {
		vcpus = ring0MonitorVCPUID
	}
	if err := installRing0SignalBridge(); err != nil {
		return nil, err
	}
	if err := installRing0RuntimeVMMemoryHook(); err != nil {
		return nil, err
	}
	timeFastpath, err := calibrateRing0TimeFastpath(vm.monitor.vtimerOffset)
	if err != nil {
		return nil, err
	}
	monitorPool, err := vmplatform.NewWorkerPoolWithIDBase(vm, 1, ring0MonitorVCPUID)
	if err != nil {
		return nil, fmt.Errorf("creating HVF ring0 monitor vCPU: %w", err)
	}
	e := &ring0Engine{
		vm:              vm,
		monitorPool:     monitorPool,
		concurrency:     vcpus,
		timeFastpath:    timeFastpath,
		stage2Slabs:     make(map[uintptr]ring0Stage2Slab),
		stage1Access:    make(map[uint64]monitorabi.Access),
		applicationGPAs: make(map[uint64]map[uint64]uint64),
		nextMirrorGPA:   ring0HostMirrorGPAStart,
		pendingMirror:   make(map[uintptr]struct{}),
		cpuIDs:          make([]bool, vcpus),
		clockStop:       make(chan struct{}),
		clockDone:       make(chan struct{}),
	}
	e.realtimeOffsetNS.Store(timeFastpath.realtimeOffsetNS)
	var expander vmm.MonitorPoolExpander
	if candidate, ok := vmInterface.(vmm.MonitorPoolExpander); ok {
		expander = candidate
	}
	e.monitor = vmplatform.NewMonitor(ring0QuiescedMonitorExecutor{engine: e}, expander)
	if err := e.monitor.Execute(context.Background(), monitorabi.Command{
		Opcode:         monitorabi.OpcodeCreateAddressSpace,
		AddressSpaceID: ring0SentryAddressSpaceID,
		Flags:          monitorabi.AddressSpaceFlagSentry,
	}); err != nil {
		monitorPool.Close()
		return nil, fmt.Errorf("creating monitor-owned Sentry address space: %w", err)
	}
	if err := e.refreshHostMirror(context.Background()); err != nil {
		cleanupErr := e.releaseHostMirror()
		monitorPool.Close()
		return nil, errors.Join(err, cleanupErr)
	}
	for offset := uint64(0); offset < monitorStatesLength; offset += vmm.HostSlabSize {
		address := vm.monitor.statesHost + uintptr(offset)
		slab, ok := e.stage2Slabs[address]
		if !ok || !slab.canonical || slab.gpa != monitorStatesGPA+offset {
			cleanupErr := e.releaseHostMirror()
			monitorPool.Close()
			return nil, errors.Join(fmt.Errorf("Sentry monitor state slab %#x does not reuse canonical GPA %#x: slab=%+v present=%t", address, monitorStatesGPA+offset, slab, ok), cleanupErr)
		}
	}
	e.sentryTTBR0 = vm.monitorAddressSpaceTTBR(ring0SentryAddressSpaceID)
	if e.sentryTTBR0 == 0 {
		cleanupErr := e.releaseHostMirror()
		monitorPool.Close()
		return nil, errors.Join(fmt.Errorf("monitor did not publish the Sentry TTBR0"), cleanupErr)
	}
	if err := vm.hostState.register(e); err != nil {
		cleanupErr := e.releaseHostMirror()
		monitorPool.Close()
		return nil, errors.Join(err, cleanupErr)
	}
	go e.runRuntimeClockUpdater()
	return e, nil
}

func (e *ring0Engine) RunMonitor(ctx context.Context, encoded []byte) ([]byte, error) {
	mutatesTranslations := monitorCommandMutatesTranslations(encoded)
	if mutatesTranslations {
		if response, ran, err := e.tryRunResidentMonitor(ctx, encoded); ran {
			return response, err
		}
	}
	// Monitor work normally runs on a dedicated host worker. A retained,
	// owner-affine application vCPU may execute it directly to avoid another
	// host-thread handoff. A Sentry continuation may call this while handling a
	// Linux syscall at EL1, so return that continuation to Darwin before either
	// path runs. At host EL0 EnsureHost is a signal-bridge no-op.
	ring0asm.EnsureHost()
	// Mapping mutation and application execution share page tables but execute
	// on distinct HVF vCPUs. Hypervisor.framework does not provide a reliable
	// cross-vCPU TLB rendezvous on every supported macOS 15 machine. Kick every
	// active reader before publishing a translation mutation. Application vCPUs
	// additionally perform one local generation flush after the transaction.
	if !mutatesTranslations {
		return e.runMonitorCommand(ctx, encoded)
	}
	if err := e.lockApplicationEntries(ctx); err != nil {
		return nil, err
	}
	defer e.unlockApplicationEntries()
	return e.runMonitorCommand(ctx, encoded)
}

// tryRunResidentMonitor executes an uncontended page-table transaction on the
// current Sentry-at-EL1 vCPU. Independent address spaces need neither a
// redpill nor a second hv_vcpu_run: entryBlocked prevents a new application
// reader from entering, and the owner table proves that no other existing vCPU
// is currently executing the root being changed. CPUs retaining dormant TLB
// entries observe mmuGeneration and flush locally before their next entry.
//
// Executable mutations issue a lightweight native cache-maintenance HVC before
// entering the monitor. Any lock contention is delegated to the host path so a
// resident continuation never parks while holding a guest-only critical
// section.
func (e *ring0Engine) tryRunResidentMonitor(ctx context.Context, encoded []byte) ([]byte, bool, error) {
	if len(encoded) == 0 || len(encoded) > monitorCommandMax || ring0asm.CurrentEL() == 0 {
		return nil, false, nil
	}
	command, err := monitorabi.UnmarshalCommandView(encoded)
	if err != nil || (command.Opcode != monitorabi.OpcodeApply &&
		command.Opcode != monitorabi.OpcodeDestroyAddressSpace) {
		return nil, false, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err
	}
	lockedEntries := false
	// Monitor transactions are short and globally serialized. A bounded
	// processor-yield loop lets independent guest-resident writers hand the
	// lock directly to one another without redpilling merely because their
	// calls overlapped by a few microseconds. Longer contention still falls back
	// to the host path, where waiting and cancellation may use the Go scheduler.
	for spin := 0; spin < 4096; spin++ {
		if e.entryMutationMu.TryLock() {
			lockedEntries = true
			break
		}
		ring0asm.Yield()
	}
	if !lockedEntries {
		return nil, false, nil
	}
	e.entryBlocked.Store(true)
	defer func() {
		e.entryBlocked.Store(false)
		e.entryMutationMu.Unlock()
	}()
	if !e.monitorMu.TryLock() {
		return nil, false, nil
	}
	defer e.monitorMu.Unlock()

	value, ok := e.cpusByTID.Load(hosttid.Current())
	if !ok {
		return nil, false, nil
	}
	cpu := value.(*ring0ApplicationCPU)
	if !cpu.tryPin() {
		return nil, false, nil
	}
	defer e.releaseApplicationCPU(cpu)
	if e.closed.Load() || cpu.retiring.Load() || cpu.entryActive.Load() ||
		!cpu.bridge.inGuest() || cpu.bridge.destroyed() {
		return nil, false, nil
	}
	if conflicting, inspected := e.residentMonitorConflicts(command.AddressSpaceID, cpu); !inspected || conflicting {
		return nil, false, nil
	}
	state := unsafe.Slice((*byte)(unsafe.Pointer(cpu.vcpu.stateHost)), vmm.HostSlabSize)
	binary.LittleEndian.PutUint32(state[stateCommandLength:], uint32(len(encoded)))
	copy(state[stateCommand:stateCommand+len(encoded)], encoded)
	clear(state[stateResponse : stateResponse+32])

	needsCacheSync := e.residentCommandNeedsCacheSync(command)
	vmLocked := false
	if needsCacheSync {
		if !e.vm.mu.TryLock() {
			return nil, false, nil
		}
		vmLocked = true
	}
	if vmLocked && e.vm.closed {
		e.vm.mu.Unlock()
		return nil, true, fmt.Errorf("HVF VM is closed")
	}
	if err := e.validateResidentExecutableMappings(command); err != nil {
		if vmLocked {
			e.vm.mu.Unlock()
		}
		return nil, true, err
	}
	critical := (*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateGuestCritical))
	atomic.StoreUint64(critical, 1)
	// A cancellation may have safely exported this Go continuation while it
	// acquired the ordinary locks above. Once critical is published, a second
	// CurrentEL check closes that race: later cancellations resume the vCPU
	// until the guest-only cache and monitor calls have returned.
	if ring0asm.CurrentEL() == 0 {
		atomic.StoreUint64(critical, 0)
		if vmLocked {
			e.vm.mu.Unlock()
		}
		return nil, false, nil
	}

	if needsCacheSync {
		e.syncResidentExecutableMappings(command)
	}
	ring0asm.RunMonitorGuest(
		monitorCodeVA+cpu.vcpu.monitor.commandCallOffset,
		cpu.vcpu.stateVA,
	)
	if vmLocked {
		e.vm.mu.Unlock()
	}
	atomic.StoreUint64(critical, 0)
	// A cancellation that arrived inside the critical section remains
	// published. Complete its requested host transfer now that every PC and
	// stack address again has a valid Darwin representation.
	if cpu.bridge.externalInterruptRequested() || cpu.bridge.destroyRequested() {
		ring0asm.EnsureHost()
	}
	if err := cpu.bridge.error("HVF resident EL1 monitor command failed"); err != nil {
		return nil, true, err
	}
	response := make([]byte, 32)
	copy(response, state[stateResponse:stateResponse+32])
	e.recordMonitorCommandView(command, response, true)
	return response, true, nil
}

// residentMonitorConflicts reports whether another application vCPU is
// actively executing the address space that a direct monitor command will
// mutate. The caller has already published entryBlocked, so an inactive vCPU
// cannot become a new reader until after mmuGeneration is advanced. TryLock is
// deliberate: direct EL1 execution must fall back instead of parking on a Go
// mutex while its continuation is guest-resident.
func (e *ring0Engine) residentMonitorConflicts(addressSpaceID uint64, current *ring0ApplicationCPU) (conflicting, inspected bool) {
	if !e.cpuMu.TryLock() {
		return false, false
	}
	defer e.cpuMu.Unlock()
	for _, cpu := range e.cpusByID {
		if cpu == nil || cpu == current || !cpu.entryActive.Load() {
			continue
		}
		stateAddressSpace := (*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateAddressSpaceID))
		if atomic.LoadUint64(stateAddressSpace) == addressSpaceID {
			return true, true
		}
	}
	return false, true
}

// lockApplicationEntries serializes translation writers and prevents a
// host-resident reader from crossing the application admission boundary.
// The monitor's native run call performs the short exact-context pause for
// readers already executing at EL0 or EL1; no Go code runs during that pause.
// The returned state owns entryMutationMu and must be released with
// unlockApplicationEntries.
func (e *ring0Engine) lockApplicationEntries(ctx context.Context) error {
	for !e.entryMutationMu.TryLock() {
		if err := ctx.Err(); err != nil {
			return err
		}
		time.Sleep(50 * time.Microsecond)
	}
	e.entryBlocked.Store(true)
	return nil
}

func (e *ring0Engine) unlockApplicationEntries() {
	e.entryBlocked.Store(false)
	e.entryMutationMu.Unlock()
}

// pinCurrentIdleApplicationCPU returns the reusable application vCPU owned by
// the calling host thread. A retained fault residency leaves this vCPU at host
// EL0, so it can execute the monitor command directly without a worker handoff
// or another owner qualification. The next application entry reloads all
// Linux registers from the shared state page.
func (e *ring0Engine) pinCurrentIdleApplicationCPU() *ring0ApplicationCPU {
	value, ok := e.cpusByTID.Load(hosttid.Current())
	if !ok {
		return nil
	}
	cpu := value.(*ring0ApplicationCPU)
	if !cpu.tryPin() {
		return nil
	}
	if e.closed.Load() || cpu.retiring.Load() || cpu.applicationActive.Load() || cpu.entryActive.Load() || cpu.bridge.inGuest() || cpu.bridge.destroyed() {
		e.releaseApplicationCPU(cpu)
		return nil
	}
	return cpu
}

// runMonitorCommand executes one encoded command. Its caller must either hold
// the translation-mutation rendezvous or be constructing an engine that has
// no application vCPUs yet.
func (e *ring0Engine) runMonitorCommand(ctx context.Context, encoded []byte) ([]byte, error) {
	e.monitorMu.Lock()
	defer e.monitorMu.Unlock()
	if len(encoded) == 0 || len(encoded) > monitorCommandMax {
		return nil, fmt.Errorf("invalid EL1 monitor command length %d", len(encoded))
	}
	command, commandErr := monitorabi.UnmarshalCommand(encoded)
	if commandErr == nil {
		if err := e.synchronizeExecutableMappings(command); err != nil {
			return nil, err
		}
	}
	var response []byte
	if cpu := e.pinCurrentIdleApplicationCPU(); cpu != nil {
		var err error
		response, err = cpu.vcpu.RunMonitor(ctx, encoded)
		e.releaseApplicationCPU(cpu)
		if err != nil {
			return nil, err
		}
	} else {
		value, err := e.monitorPool.Do(ctx, func(ctx context.Context, vcpu vmm.VCPU) (any, error) {
			return vcpu.RunMonitor(ctx, encoded)
		})
		if err != nil {
			return nil, err
		}
		var ok bool
		response, ok = value.([]byte)
		if !ok {
			return nil, fmt.Errorf("HVF ring0 monitor returned %T, want []byte", value)
		}
	}
	e.recordMonitorCommand(command, response, commandErr == nil, monitorCommandMutatesTranslations(encoded))
	return response, nil
}

func (e *ring0Engine) recordMonitorCommand(command monitorabi.Command, response []byte, validCommand, mutatesTranslations bool) {
	if validCommand {
		if decoded, err := monitorabi.UnmarshalResponse(response); err == nil && decoded.Status == monitorabi.StatusOK {
			e.updateApplicationGPARecords(command)
		}
	}
	if mutatesTranslations {
		e.mmuGeneration.Add(1)
	}
}

func (e *ring0Engine) recordMonitorCommandView(command monitorabi.CommandView, response []byte, mutatesTranslations bool) {
	if decoded, err := monitorabi.UnmarshalResponse(response); err == nil && decoded.Status == monitorabi.StatusOK {
		e.updateApplicationGPARecordsView(command)
	}
	if mutatesTranslations {
		e.mmuGeneration.Add(1)
	}
}

type ring0ExecutableGuestRange struct {
	va     uint64
	gpa    uint64
	length uint64
}

// executableGuestRanges resolves every range that will newly gain execute
// access. Protect operations use acknowledged mappings plus earlier operations
// in the same ordered batch.
func (e *ring0Engine) executableGuestRanges(command monitorabi.Command) []ring0ExecutableGuestRange {
	if command.Opcode != monitorabi.OpcodeApply || command.Flags&monitorabi.AddressSpaceFlagSentry != 0 {
		return nil
	}
	space := e.applicationGPAs[command.AddressSpaceID]
	type pendingGPA struct {
		gpa    uint64
		mapped bool
	}
	pending := make(map[uint64]pendingGPA)
	resolve := func(va uint64) (uint64, bool) {
		if replacement, ok := pending[va]; ok {
			return replacement.gpa, replacement.mapped
		}
		gpa, ok := space[va]
		return gpa, ok
	}
	var executable []ring0ExecutableGuestRange
	for _, operation := range command.Operations {
		switch operation.Type {
		case monitorabi.OperationMap:
			if operation.Access&monitorabi.AccessExecute != 0 && operation.GPA >= GuestRAMIPAStart {
				executable = append(executable, ring0ExecutableGuestRange{
					va: operation.VA, gpa: operation.GPA, length: operation.Length,
				})
			}
			for offset := uint64(0); offset < operation.Length; offset += vmm.LogicalPageSize {
				pending[operation.VA+offset] = pendingGPA{gpa: operation.GPA + offset, mapped: true}
			}
		case monitorabi.OperationUnmap:
			for offset := uint64(0); offset < operation.Length; offset += vmm.LogicalPageSize {
				pending[operation.VA+offset] = pendingGPA{}
			}
		case monitorabi.OperationProtect:
			if operation.Access&monitorabi.AccessExecute == 0 {
				continue
			}
			for offset := uint64(0); offset < operation.Length; offset += vmm.LogicalPageSize {
				gpa, ok := resolve(operation.VA + offset)
				if !ok || gpa < GuestRAMIPAStart {
					continue
				}
				executable = append(executable, ring0ExecutableGuestRange{
					va: operation.VA + offset, gpa: gpa, length: vmm.LogicalPageSize,
				})
			}
		}
	}
	return executable
}

// synchronizeExecutableMappings completes the Darwin writer-side cache
// protocol before the host-run monitor publishes an executable stage-1 leaf.
func (e *ring0Engine) synchronizeExecutableMappings(command monitorabi.Command) error {
	for _, executable := range e.executableGuestRanges(command) {
		if err := e.vm.invalidateGuestCode(executable.gpa, executable.length); err != nil {
			return fmt.Errorf("synchronizing executable guest mapping at VA %#x: %w", executable.va, err)
		}
	}
	return nil
}

// residentCommandNeedsCacheSync reports whether a direct command may grant
// execute access to GuestRAM. It intentionally does no allocation: heap growth
// while this continuation is resident could require a Darwin service and turn
// a direct command back into the host path.
func (e *ring0Engine) residentCommandNeedsCacheSync(command monitorabi.CommandView) bool {
	if command.Opcode != monitorabi.OpcodeApply || command.Flags&monitorabi.AddressSpaceFlagSentry != 0 {
		return false
	}
	for index := 0; index < command.OperationCount; index++ {
		operation := command.Operation(index)
		if operation.Access&monitorabi.AccessExecute == 0 {
			continue
		}
		if operation.Type == monitorabi.OperationProtect ||
			(operation.Type == monitorabi.OperationMap && operation.GPA >= GuestRAMIPAStart) {
			return true
		}
	}
	return false
}

// residentExecutableGPA resolves va using operations preceding before in the
// current ordered batch, then the last acknowledged host mapping record.
func (e *ring0Engine) residentExecutableGPA(command monitorabi.CommandView, before int, va uint64) (uint64, bool) {
	for index := before - 1; index >= 0; index-- {
		operation := command.Operation(index)
		if va < operation.VA || va-operation.VA >= operation.Length {
			continue
		}
		switch operation.Type {
		case monitorabi.OperationMap:
			return operation.GPA + va - operation.VA, true
		case monitorabi.OperationUnmap:
			return 0, false
		}
	}
	gpa, ok := e.applicationGPAs[command.AddressSpaceID][va]
	return gpa, ok
}

// visitResidentExecutableMappings walks executable GuestRAM ranges without
// allocating. Returning false stops the walk.
func (e *ring0Engine) visitResidentExecutableMappings(command monitorabi.CommandView, visit func(gpa, length uint64) bool) {
	if command.Opcode != monitorabi.OpcodeApply || command.Flags&monitorabi.AddressSpaceFlagSentry != 0 {
		return
	}
	for index := 0; index < command.OperationCount; index++ {
		operation := command.Operation(index)
		if operation.Access&monitorabi.AccessExecute == 0 {
			continue
		}
		switch operation.Type {
		case monitorabi.OperationMap:
			if operation.GPA >= GuestRAMIPAStart && !visit(operation.GPA, operation.Length) {
				return
			}
		case monitorabi.OperationProtect:
			for offset := uint64(0); offset < operation.Length; offset += vmm.LogicalPageSize {
				gpa, ok := e.residentExecutableGPA(command, index, operation.VA+offset)
				if ok && gpa >= GuestRAMIPAStart && !visit(gpa, vmm.LogicalPageSize) {
					return
				}
			}
		}
	}
}

// validateResidentExecutableMappings verifies every stage-2 lookup before the
// guest-critical section. The caller holds e.vm.mu, so the subsequent HVC walk
// observes the same stable host mappings and cannot need an error path.
func (e *ring0Engine) validateResidentExecutableMappings(command monitorabi.CommandView) error {
	var missing uint64
	e.visitResidentExecutableMappings(command, func(gpa, length uint64) bool {
		for current, end := gpa, gpa+length; current < end; current = (current &^ (vmm.HostSlabSize - 1)) + vmm.HostSlabSize {
			if _, ok := e.vm.stage2[current&^(vmm.HostSlabSize-1)]; !ok {
				missing = current
				return false
			}
		}
		return true
	})
	if missing != 0 {
		return fmt.Errorf("executable guest GPA %#x has no tracked stage-2 slab", missing)
	}
	return nil
}

type ring0ExecutableCacheBatch struct {
	address uintptr
	length  uint64
}

func (b *ring0ExecutableCacheBatch) flush() {
	if b.length != 0 {
		ring0asm.SyncGuestCode(b.address, b.length)
		b.address = 0
		b.length = 0
	}
}

// syncResidentExecutableMappings performs the writer-side Darwin cache
// protocol through the native bridge. The caller holds e.vm.mu and has already
// validated every lookup, published guest-critical state, and rechecked EL1.
func (e *ring0Engine) syncResidentExecutableMappings(command monitorabi.CommandView) {
	var batch ring0ExecutableCacheBatch
	e.visitResidentExecutableMappings(command, func(gpa, length uint64) bool {
		for current, end := gpa, gpa+length; current < end; {
			slabGPA := current &^ (vmm.HostSlabSize - 1)
			slab := e.vm.stage2[slabGPA]
			offset := current - slabGPA
			chunk := vmm.HostSlabSize - offset
			if remaining := end - current; chunk > remaining {
				chunk = remaining
			}
			hostAddress := slab.host + uintptr(offset)
			if batch.length != 0 && batch.address+uintptr(batch.length) != hostAddress {
				batch.flush()
			}
			if batch.length == 0 {
				batch.address = hostAddress
			}
			batch.length += chunk
			current += chunk
		}
		return true
	})
	batch.flush()
}

func (e *ring0Engine) updateApplicationGPARecords(command monitorabi.Command) {
	switch command.Opcode {
	case monitorabi.OpcodeCreateAddressSpace:
		if command.Flags&monitorabi.AddressSpaceFlagSentry == 0 {
			e.applicationGPAs[command.AddressSpaceID] = make(map[uint64]uint64)
		}
	case monitorabi.OpcodeDestroyAddressSpace:
		delete(e.applicationGPAs, command.AddressSpaceID)
	case monitorabi.OpcodeApply:
		space := e.applicationGPAs[command.AddressSpaceID]
		if space == nil {
			return
		}
		for _, operation := range command.Operations {
			switch operation.Type {
			case monitorabi.OperationMap:
				for offset := uint64(0); offset < operation.Length; offset += vmm.LogicalPageSize {
					space[operation.VA+offset] = operation.GPA + offset
				}
			case monitorabi.OperationUnmap:
				for offset := uint64(0); offset < operation.Length; offset += vmm.LogicalPageSize {
					delete(space, operation.VA+offset)
				}
			}
		}
	}
}

func (e *ring0Engine) updateApplicationGPARecordsView(command monitorabi.CommandView) {
	switch command.Opcode {
	case monitorabi.OpcodeDestroyAddressSpace:
		delete(e.applicationGPAs, command.AddressSpaceID)
	case monitorabi.OpcodeApply:
		space := e.applicationGPAs[command.AddressSpaceID]
		if space == nil {
			return
		}
		for index := 0; index < command.OperationCount; index++ {
			operation := command.Operation(index)
			switch operation.Type {
			case monitorabi.OperationMap:
				for offset := uint64(0); offset < operation.Length; offset += vmm.LogicalPageSize {
					space[operation.VA+offset] = operation.GPA + offset
				}
			case monitorabi.OperationUnmap:
				for offset := uint64(0); offset < operation.Length; offset += vmm.LogicalPageSize {
					delete(space, operation.VA+offset)
				}
			}
		}
	}
}

func monitorCommandMutatesTranslations(encoded []byte) bool {
	// The versioned ABI stores its little-endian opcode at byte 10. Treat a
	// malformed short command conservatively; the monitor will reject it after
	// the application rendezvous.
	if len(encoded) < 12 {
		return true
	}
	switch monitorabi.Opcode(binary.LittleEndian.Uint16(encoded[10:12])) {
	case monitorabi.OpcodeCreateAddressSpace,
		monitorabi.OpcodeDestroyAddressSpace,
		monitorabi.OpcodeApply:
		return true
	default:
		return false
	}
}

// kickApplicationEntries requests an architected exit from every application
// reader that published entryActive. cancelRequested also covers the narrow
// interval after a task published entryActive but before its first
// hv_vcpu_run; RequestExit alone is not guaranteed to remain pending across
// that interval on all HVF versions.
func (e *ring0Engine) kickApplicationEntries() error {
	cpus := e.pinApplicationCPUs()
	defer e.unpinApplicationCPUs(cpus)
	for _, cpu := range cpus {
		if cpu.bridge.destroyed() {
			continue
		}
		if cpu.entryActive.Load() {
			cancel := (*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateCancelRequested))
			atomic.StoreUint64(cancel, 1)
		}
		// An inactive application residency may still be executing its Sentry
		// syscall continuation at EL1. Kick every guest-resident bridge; the owner
		// loop exports an EL1 continuation directly and converts EL0 cancellation
		// into the monitor's architected application interrupt.
		if !cpu.bridge.inGuest() {
			continue
		}
		cpu.bridge.requestExternalInterrupt()
		if err := cpu.vcpu.RequestExit(); err != nil && !cpu.bridge.destroyed() {
			return fmt.Errorf("interrupting HVF ring0 vCPU %d for application admission: %w", cpu.vcpu.id, err)
		}
	}
	return nil
}

func (e *ring0Engine) findApplicationCPUByTIDLocked(tid uint64) *ring0ApplicationCPU {
	for _, cpu := range e.cpusByID {
		if cpu != nil && cpu.tid == tid {
			return cpu
		}
	}
	return nil
}

func (e *ring0Engine) applicationCPUCountLocked() int {
	count := 0
	for _, cpu := range e.cpusByID {
		if cpu != nil {
			count++
		}
	}
	return count
}

// BeginApplicationResidency pins the Sentry task goroutine to one Darwin host
// thread. Hypervisor.framework vCPUs are owner-thread affine, so this lock is
// retained across the application's syscall exit and the corresponding Sentry
// syscall handling instead of reconstructing a vCPU on every entry.
func (*ring0Engine) BeginApplicationResidency() {
	runtime.LockOSThread()
}

// EndApplicationResidency returns the current Sentry continuation to host EL0,
// retires its owner-affine vCPU, and releases the host thread. vmplatform calls
// this immediately before a Linux task blocks, stops, execs, or exits.
func (e *ring0Engine) EndApplicationResidency() (retErr error) {
	defer runtime.UnlockOSThread()
	retErr = e.retireCurrentApplicationCPU()
	return retErr
}

// retireCurrentApplicationCPU destroys the vCPU owned by the calling host
// thread without changing its Go thread-lock count. Epoch rollover uses this
// when it is initiated synchronously by the resident task itself; the context
// transfers its thread lock to the replacement engine on its next Switch.
func (e *ring0Engine) retireCurrentApplicationCPU() (retErr error) {
	atGuestEL1 := ring0asm.CurrentEL() != 0
	tid := hosttid.Current()
	e.cpuMu.Lock()
	cpu := e.findApplicationCPUByTIDLocked(tid)
	if cpu == nil {
		e.cpuMu.Unlock()
		return nil
	}
	if !cpu.tryPin() {
		e.cpuMu.Unlock()
		return nil
	}
	cpu.applicationActive.Store(false)
	cpu.retiring.Store(true)
	cpu.bridge.requestDestroy()
	e.cpuMu.Unlock()
	if atGuestEL1 {
		// The redpill HVC exports this exact Go continuation through the owner
		// signal bridge. destroy_requested makes the bridge destroy the
		// thread-affine HVF vCPU before resuming it at Darwin EL0.
		ring0asm.EnsureHost()
	}

	// Host-returned and never-entered bridges are retired directly by their
	// owner thread. A guest-resident bridge should already be destroyed by the
	// redpill path above.
	if err := cpu.bridge.error(fmt.Sprintf("ending residency for HVF ring0 vCPU %d", cpu.vcpu.id)); err != nil {
		retErr = err
	} else if !cpu.bridge.destroyed() {
		if cpu.bridge.inGuest() {
			retErr = fmt.Errorf("HVF ring0 vCPU %d remained in guest after owner-thread redpill", cpu.vcpu.id)
		} else if err := cpu.vcpu.closeStaleOwner(); err != nil {
			retErr = fmt.Errorf("ending owner-affine HVF ring0 vCPU %d residency: %w", cpu.vcpu.id, err)
		} else {
			cpu.bridge.markDestroyed()
		}
	}
	e.releaseApplicationCPU(cpu)
	return retErr
}

func (e *ring0Engine) RunApplication(ctx context.Context, application vmm.ApplicationState) (retExit vmm.Exit, retErr error) {
	cpu, _, err := e.getApplicationCPU(ctx)
	if err != nil {
		return vmm.Exit{}, err
	}
	return e.runApplicationCPU(ctx, application, cpu, true)
}

func (e *ring0Engine) runApplicationCPU(ctx context.Context, application vmm.ApplicationState, cpu *ring0ApplicationCPU, release bool) (retExit vmm.Exit, retErr error) {
	defer func() {
		if retErr == nil || errors.Is(retErr, context.Canceled) {
			return
		}
		// Sentry boot reserves fd 3 for --panic-log-fd. Platform failures are
		// normally converted into a guest SIGKILL, so without this diagnostic the
		// client sees only status 137 or control-socket EOF. write(2) is safe here
		// and deliberately avoids creating an os.File that could close the fd.
		message := []byte("ttc hvf application error: " + retErr.Error() + "\n")
		_, _ = syscall.Write(3, message)
	}()
	registers, ok := application.Registers.(*vmm.ARM64State)
	if !ok {
		return vmm.Exit{}, fmt.Errorf("HVF ring0 ARM64 state has type %T, want *vmm.ARM64State", application.Registers)
	}
	if !validApplicationPState(registers.PState) {
		return vmm.Exit{}, fmt.Errorf("invalid ARM64 application PSTATE %#x before ring0 EL0 entry", registers.PState)
	}
	state := unsafe.Slice((*byte)(unsafe.Pointer(cpu.vcpu.stateHost)), vmm.HostSlabSize)
	cleanupCPU := func() {
		cpu.applicationActive.Store(false)
		atomic.StoreUint64((*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateCancelRequested)), 0)
		cpu.bridge.clearExternalInterrupt()
		if release {
			e.releaseApplicationCPU(cpu)
		}
	}
	defer cleanupCPU()

retryApplication:
	atGuestEL1 := ring0asm.CurrentEL() != 0
	if err := ctx.Err(); err != nil {
		return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
	}
	if !atGuestEL1 {
		// A host-service gate publishes its faulting VA in the permanent state
		// page. Demand-map that slab together with the small continuation working
		// set needed to return through ordinary Go code at EL1.
		e.learnHostGateFault(cpu)
		e.requestHostMirror(uintptr(unsafe.Pointer(e)))
		e.requestHostMirror(uintptr(unsafe.Pointer(cpu)))
		e.requestHostMirror(uintptr(unsafe.Pointer(cpu.vcpu)))
		e.requestHostMirror(uintptr(unsafe.Pointer(cpu.bridge)))
		e.requestHostMirror(uintptr(unsafe.Pointer(cpu.bridge.pointer)))
		e.requestHostMirror(cpu.vcpu.stateHost)
		e.requestHostMirror(uintptr(unsafe.Pointer(registers)))
		stackSlab := ring0asm.CurrentSP() &^ uintptr(vmm.HostSlabSize-1)
		e.requestHostMirror(stackSlab)
		if stackSlab >= uintptr(vmm.HostSlabSize) {
			e.requestHostMirror(stackSlab - uintptr(vmm.HostSlabSize))
		}
		if stackSlab <= ^uintptr(0)-uintptr(vmm.HostSlabSize) {
			e.requestHostMirror(stackSlab + uintptr(vmm.HostSlabSize))
		}
		g, m, p, mcache, g0, g0StackLo, g0StackHi, gsignal, gsignalStackLo, gsignalStackHi := hosttid.RuntimePointers()
		e.requestHostMirror(g)
		e.requestHostMirror(m)
		e.requestHostMirror(p)
		e.requestHostMirror(mcache)
		e.requestHostMirror(g0)
		e.requestHostMirror(g0StackLo)
		if g0StackHi != 0 {
			e.requestHostMirror(g0StackHi - 1)
		}
		e.requestHostMirror(gsignal)
		e.requestHostMirror(gsignalStackLo)
		if gsignalStackHi != 0 {
			e.requestHostMirror(gsignalStackHi - 1)
		}
		e.requestHostMirror(hosttid.RuntimeTLSBase())
		if err := e.extendHostMirror(ctx); err != nil {
			return vmm.Exit{}, errors.Join(err, e.retireUnenteredApplicationCPU(cpu))
		}
	}
	binary.LittleEndian.PutUint64(state[stateAddressSpaceID:], application.AddressSpaceID)
	for i, value := range registers.X {
		binary.LittleEndian.PutUint64(state[stateX0+i*8:], value)
	}
	binary.LittleEndian.PutUint64(state[stateSP:], registers.SP)
	binary.LittleEndian.PutUint64(state[statePC:], registers.PC)
	binary.LittleEndian.PutUint64(state[statePState:], registers.PState&^0xf)
	binary.LittleEndian.PutUint64(state[stateTPIDR:], registers.TPIDR)
	binary.LittleEndian.PutUint64(state[stateExitKind:], 0)
	binary.LittleEndian.PutUint64(state[stateRing0Entry:],
		monitorCodeVA+cpu.vcpu.monitor.ring0ApplicationOffset)
	copy(state[stateApplicationFP:stateApplicationFP+len(registers.FP)], registers.FP[:])
	if err := ctx.Err(); err != nil {
		return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
	}
	if err := e.beginApplicationEntry(ctx, cpu); err != nil {
		return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
	}
	if e.closed.Load() {
		cpu.entryActive.Store(false)
		if err := e.retireUnenteredApplicationCPU(cpu); err != nil {
			return vmm.Exit{}, err
		}
		return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
	}
	// Sample the translation generation only after publishing entryActive and
	// observing entryBlocked clear. A monitor mutation that starts before this
	// point must finish first; one that starts after it must cancel this entry
	// before changing page tables. Sampling earlier leaves a window in which a
	// mutation can complete after the sample but before guest admission, letting
	// this vCPU enter an application with stale local translations.
	if generation := e.mmuGeneration.Load(); cpu.tlbGeneration != generation {
		atomic.StoreUint64((*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateTLBFlushRequested)), 1)
		cpu.tlbGeneration = generation
	}
	if atGuestEL1 && ring0asm.CurrentEL() == 0 {
		// A concurrent Sentry-root refresh may redpill this continuation after
		// the entry sample but before it publishes entryActive. Re-run host-side mirror
		// preparation on the same pinned owner vCPU; the outer invocation retains
		// the sole reference and performs the eventual release.
		cpu.entryActive.Store(false)
		goto retryApplication
	}
	// Keep this G and its P active while the application runs. Unlike KVM's
	// SwitchToUser, an application exception restores this continuation directly
	// at EL1, so ordinary Sentry Go code executes before this function returns to
	// Darwin. Marking the G as _Gsyscall here would release its P and mcache for
	// use by another M while the EL1 continuation still allocates from them.
	// Long-running EL0 execution is made preemptible by the bridge's vCPU
	// cancellation path; it must not be modeled as a host syscall interval.
	if atGuestEL1 {
		ring0asm.SwitchApplicationGuest()
	} else {
		// The first entry for a retained residency uses a controlled signal so
		// Darwin supplies a complete architectural context. Synthesizing a
		// partial context for a signal-free entry is not equivalent to KVM's
		// bluepill and can lose Go runtime register state. The signal cost is
		// paid once per residency; subsequent syscall entries stay in EL1.
		ring0asm.Bluepill(unsafe.Pointer(cpu.bridge.pointer), cpu.vcpu.stateVA)
		ring0asm.SwitchApplicationGuest()
	}
	cpu.entryActive.Store(false)
	if err := cpu.bridge.error("HVF Sentry-at-EL1 application bridge failed"); err != nil {
		postRunErr := cpu.bridge.probeOwner()
		retireErr := e.retireUnenteredApplicationCPU(cpu)
		return vmm.Exit{}, errors.Join(err, postRunErr, retireErr)
	}
	for i := range registers.X {
		registers.X[i] = binary.LittleEndian.Uint64(state[stateX0+i*8:])
	}
	registers.SP = binary.LittleEndian.Uint64(state[stateSP:])
	registers.PC = binary.LittleEndian.Uint64(state[statePC:])
	registers.PState = binary.LittleEndian.Uint64(state[statePState:])
	registers.TPIDR = binary.LittleEndian.Uint64(state[stateTPIDR:])
	copy(registers.FP[:], state[stateApplicationFP:stateApplicationFP+len(registers.FP)])
	exitKind := binary.LittleEndian.Uint64(state[stateExitKind:])
	syndrome := binary.LittleEndian.Uint64(state[stateESR:])
	address := binary.LittleEndian.Uint64(state[stateFAR:])
	switch exitKind {
	case 1:
		return vmm.Exit{Reason: vmm.ExitSyscall, Syndrome: syndrome}, nil
	case 2:
		return vmm.Exit{Reason: vmm.ExitFault, Syndrome: syndrome, Address: address, Access: stage1FaultAccess(syndrome)}, nil
	case 3:
		return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
	case 5:
		// A Darwin VM mutation can redpill the Sentry after CurrentEL was
		// sampled but before SwitchApplicationGuest reaches the monitor. The
		// SIGILL bridge records an internal retry in that case, but no Linux
		// execution occurred and no caller requested cancellation. Retry on the
		// same pinned owner vCPU so this internal MMU-notifier rendezvous is not
		// exposed as a spurious application interrupt.
		if err := ctx.Err(); err != nil {
			return vmm.Exit{}, err
		}
		goto retryApplication
	case 6:
		// A native MMU rendezvous interrupted EL0 so this retry performs the
		// vCPU's local generation flush before re-entry. The monitor encoded it
		// separately from an architected Sentry cancellation, so it never leaks
		// to Linux syscall handling.
		if err := ctx.Err(); err != nil {
			return vmm.Exit{}, err
		}
		goto retryApplication
	case 4:
		return vmm.Exit{Reason: vmm.ExitException, Syndrome: syndrome, Address: registers.PC}, nil
	default:
		if atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateCancelRequested))) != 0 {
			return vmm.Exit{Reason: vmm.ExitInterrupted}, nil
		}
		if err := ctx.Err(); err != nil {
			return vmm.Exit{}, err
		}
		return vmm.Exit{}, errors.New("EL1 monitor returned no ring0 application exit state")
	}
}

// stage1FaultAccess decodes the access represented by an ARM64 stage-1 abort.
// Cache-maintenance instructions set both ESR_ELx.WnR and ESR_ELx.CM even
// though their translation fault must be resolved as a read. Treating WnR in
// isolation causes legitimate JIT cache invalidation on an RX page to be
// rejected as a write protection fault.
func stage1FaultAccess(syndrome uint64) vmm.Stage2Access {
	accessType := hostarch.ESRAccessType(syndrome)
	var access vmm.Stage2Access
	if accessType.Read {
		access |= vmm.Stage2Read
	}
	if accessType.Write {
		access |= vmm.Stage2Write
	}
	if accessType.Execute {
		access |= vmm.Stage2Execute
	}
	return access
}

func (e *ring0Engine) beginApplicationEntry(ctx context.Context, cpu *ring0ApplicationCPU) error {
	for {
		if !e.entryBlocked.Load() {
			cpu.entryActive.Store(true)
			if !e.entryBlocked.Load() {
				return nil
			}
			cpu.entryActive.Store(false)
		}
		// A Sentry continuation cannot wait on Darwin synchronization while it is
		// resident at EL1. Redpill first; the late CurrentEL check in the caller
		// will then repeat host-mirror preparation before entering the application.
		if ring0asm.CurrentEL() != 0 {
			ring0asm.EnsureHost()
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		time.Sleep(50 * time.Microsecond)
	}
}

func (e *ring0Engine) requestHostMirror(address uintptr) {
	// RuntimePointers intentionally reports zero for runtime structures that are
	// absent on the current host thread. Slab zero is not a Sentry mapping; if it
	// enters pendingMirror it can never be satisfied and forces a complete Mach
	// VM-map reconciliation after every later host gate.
	if address == 0 || uint64(address) >= ring0HostVALimit {
		return
	}
	e.faultMu.Lock()
	e.pendingMirror[address&^uintptr(vmm.HostSlabSize-1)] = struct{}{}
	e.faultMu.Unlock()
}

// hostSlabCanMirror returns true only when the HVA has a lifetime protocol
// strong enough for a persistent HVF stage-2 lease. File-backed mappings used
// by safecopy are external-pager VM objects whose VA may be unmapped and reused
// independently of the patched Go allocator; those accesses must redpill and
// complete at host EL0. The patched runtime and the native bridge allocator use
// distinct application-specific Darwin VM tags, while VM-owned slabs have a
// canonical GPA and an explicit retirement notifier. Go pthread stacks are
// suspended and rebound by minit/unminit runtime callbacks. Other untagged
// anonymous mappings remain host-only.
func (e *ring0Engine) hostSlabCanMirror(region ring0HostRegion, address uintptr) bool {
	if region.mainImage {
		return true
	}
	if _, _, ok := e.vm.canonicalGPAForHost(address); ok {
		return true
	}
	const (
		vmMemoryTTCRuntime = uint32(240)
		vmMemoryTTCBridge  = uint32(241)
		vmMemoryTTCVM      = uint32(242)
		vmMemoryStack      = uint32(30)
	)
	return !region.externalPager &&
		(region.userTag == vmMemoryTTCRuntime || region.userTag == vmMemoryTTCBridge ||
			region.userTag == vmMemoryTTCVM || region.userTag == vmMemoryStack)
}

// retireUnenteredApplicationCPU destroys a vCPU that was created by the
// current owner thread but never crossed the bluepill boundary. Such a vCPU
// cannot use the signal handler's normal owner-affine retirement path.
func (e *ring0Engine) retireUnenteredApplicationCPU(cpu *ring0ApplicationCPU) error {
	if cpu.bridge.inGuest() || cpu.bridge.destroyed() {
		return nil
	}
	cpu.retiring.Store(true)
	if err := cpu.vcpu.Close(); err != nil {
		return fmt.Errorf("retiring unentered owner-affine HVF ring0 vCPU %d: %w", cpu.vcpu.id, err)
	}
	cpu.bridge.markDestroyed()
	return nil
}

func (e *ring0Engine) getApplicationCPU(ctx context.Context) (*ring0ApplicationCPU, bool, error) {
	tid := hosttid.Current()
	var waiter *ring0ApplicationWaiter
	var waitStarted, nextWaitDiagnostic, lastWaiterKick time.Time
	defer func() {
		if waiter != nil {
			e.removeApplicationWaiter(waiter)
		}
	}()

retry:
	// A residency keeps its goroutine pinned to this host thread. Resolve the
	// already-qualified vCPU without taking cpuMu; the atomic reference pairs
	// with reapApplicationCPULocked's 0 -> -1 retirement transition.
	if cached, ok := e.cpusByTID.Load(tid); ok {
		cpu := cached.(*ring0ApplicationCPU)
		if cpu.tryPin() {
			if !e.closed.Load() && !cpu.bridge.destroyed() {
				cpu.applicationActive.Store(true)
				return cpu, false, nil
			}
			e.releaseApplicationCPU(cpu)
		}
	}
	if ring0asm.CurrentEL() != 0 {
		return nil, false, fmt.Errorf("guest EL1 continuation on host thread %d has no reusable HVF vCPU", tid)
	}
	e.cpuMu.Lock()
	for {
		stale := e.findApplicationCPUByTIDLocked(tid)
		if stale == nil || !stale.bridge.destroyed() {
			break
		}
		if stale.uses.Load() == 0 {
			e.reapApplicationCPULocked(stale)
			break
		}
		// Close and translation rendezvous pin bridges while interrupting them
		// without cpuMu. Wait for that bounded reference before recycling the ID.
		e.cpuMu.Unlock()
		runtime.Gosched()
		e.cpuMu.Lock()
	}
	e.reapDestroyedCPUsLocked()
	if e.closed.Load() {
		e.cpuMu.Unlock()
		return nil, false, fmt.Errorf("HVF ring0 engine is closed")
	}
	if cpu := e.findApplicationCPUByTIDLocked(tid); cpu != nil {
		if cpu.bridge.destroyed() {
			if cpu.uses.Load() != 0 {
				e.cpuMu.Unlock()
				runtime.Gosched()
				goto retry
			}
			e.reapApplicationCPULocked(cpu)
			e.cpuMu.Unlock()
			goto retry
		}
		if !cpu.tryPin() {
			e.cpuMu.Unlock()
			goto retry
		}
		cpu.applicationActive.Store(true)
		if waiter != nil {
			e.removeApplicationWaiterLocked(waiter)
			waiter = nil
		}
		e.cpuMu.Unlock()
		return cpu, false, nil
	}
	id := -1
	for candidate, used := range e.cpuIDs {
		if !used {
			id = candidate
			break
		}
	}
	// Once admission is contended, new and former incumbents join the FIFO even
	// if an ID is momentarily free. This prevents the task that just yielded its
	// residency from barging ahead of the waiter that caused the yield.
	enqueued := false
	if waiter == nil && (id < 0 || len(e.cpuWaiters) != 0) {
		waiter = e.enqueueApplicationWaiterLocked()
		enqueued = true
		waitStarted = time.Now()
		nextWaitDiagnostic = waitStarted.Add(2 * time.Second)
	}
	if id < 0 || waiter != nil && e.cpuWaiters[0] != waiter {
		e.cpuMu.Unlock()
		// Admission pressure must preempt a CPU-bound incumbent as well as a
		// syscall-heavy one. Without this kick, a task that never exits EL0 can
		// retain the final owner-affine vCPU indefinitely while every later exec
		// waits in the FIFO. kickApplicationEntries also leaves cancellation
		// pending for resident tasks currently between application entries.
		// A runtime VM mutation may have temporarily removed an incumbent from
		// guest admission when the first kick was attempted. Reissue at a bounded
		// rate until the architected application exit is acknowledged; a single
		// RequestExit is not persistent across an inactive hv_vcpu_run interval.
		if enqueued || lastWaiterKick.IsZero() || time.Since(lastWaiterKick) >= time.Millisecond {
			if err := e.kickApplicationEntries(); err != nil {
				return nil, false, err
			}
			lastWaiterKick = time.Now()
		}
		if !nextWaitDiagnostic.IsZero() && !time.Now().Before(nextWaitDiagnostic) {
			e.writeApplicationWaitDiagnostic(tid, waitStarted)
			nextWaitDiagnostic = time.Now().Add(5 * time.Second)
		}
		// Every vCPU belongs to another pinned task residency. The caller remains
		// thread-affine while it waits for an incumbent to hand off a slot. A
		// bounded poll also handles cancellation and asynchronous bridge teardown.
		select {
		case <-ctx.Done():
			return nil, false, ctx.Err()
		case <-time.After(100 * time.Microsecond):
		}
		goto retry
	}
	if waiter != nil {
		e.removeApplicationWaiterLocked(waiter)
		waiter = nil
	}
	e.cpuIDs[id] = true
	e.cpuMu.Unlock()
	vcpuInterface, err := e.vm.CreateVCPU(id)
	if err != nil {
		e.cpuMu.Lock()
		e.cpuIDs[id] = false
		e.cpuMu.Unlock()
		return nil, false, err
	}
	vcpu := vcpuInterface.(*hvfVCPU)
	bridge, err := vcpu.newRing0Bridge(e.sentryTTBR0, false)
	if err != nil {
		_ = vcpu.Close()
		e.cpuMu.Lock()
		e.cpuIDs[id] = false
		e.cpuMu.Unlock()
		return nil, false, err
	}
	e.installRuntimeClockFastpath(vcpu)
	cpu := &ring0ApplicationCPU{vcpu: vcpu, bridge: bridge, tid: tid, residentSince: time.Now()}
	cpu.uses.Store(1)
	cpu.applicationActive.Store(true)
	atomic.StoreUint64((*uint64)(unsafe.Add(unsafe.Pointer(vcpu.stateHost), stateCancelRequested)), 0)
	e.cpuMu.Lock()
	if e.closed.Load() {
		e.cpuIDs[id] = false
		e.cpuMu.Unlock()
		bridge.close()
		_ = vcpu.Close()
		return nil, false, fmt.Errorf("HVF ring0 engine closed while creating vCPU")
	}
	e.cpusByID[id] = cpu
	e.cpusByTID.Store(tid, cpu)
	e.applicationCPUs.Add(1)
	e.cpuMu.Unlock()
	return cpu, false, nil
}

func (e *ring0Engine) installRuntimeClockFastpath(vcpu *hvfVCPU) {
	state := unsafe.Slice((*byte)(unsafe.Pointer(vcpu.stateHost)), vmm.HostSlabSize)
	binary.LittleEndian.PutUint64(state[stateClockGettimePC:], e.timeFastpath.clockGettimePC)
	binary.LittleEndian.PutUint64(state[stateMachAbsoluteTimePC:], e.timeFastpath.machAbsolutePC)
	binary.LittleEndian.PutUint64(state[stateTimebaseNumer:], e.timeFastpath.numer)
	binary.LittleEndian.PutUint64(state[stateTimebaseDenom:], e.timeFastpath.denom)
	binary.LittleEndian.PutUint64(state[stateVTimerOffset:], e.timeFastpath.vtimerOffset)
	atomic.StoreUint64((*uint64)(unsafe.Add(unsafe.Pointer(vcpu.stateHost), stateRealtimeOffsetNS)), uint64(e.realtimeOffsetNS.Load()))
}

func (e *ring0Engine) runRuntimeClockUpdater() {
	defer close(e.clockDone)
	// Linux updates its VDSO time data continuously. Refreshing the single
	// realtime offset here gives the EL1 Darwin-runtime service the same clock
	// step behavior without putting a host exit back in the syscall hot path.
	refresh := time.NewTicker(250 * time.Millisecond)
	defer refresh.Stop()
	for {
		select {
		case <-e.clockStop:
			return
		case <-refresh.C:
			calibration, err := calibrateRing0TimeFastpath(e.timeFastpath.vtimerOffset)
			if err != nil || calibration.clockGettimePC != e.timeFastpath.clockGettimePC ||
				calibration.machAbsolutePC != e.timeFastpath.machAbsolutePC ||
				calibration.numer != e.timeFastpath.numer || calibration.denom != e.timeFastpath.denom {
				continue
			}
			e.realtimeOffsetNS.Store(calibration.realtimeOffsetNS)
			cpus := e.pinApplicationCPUs()
			for _, cpu := range cpus {
				atomic.StoreUint64((*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateRealtimeOffsetNS)), uint64(calibration.realtimeOffsetNS))
			}
			e.unpinApplicationCPUs(cpus)
		}
	}
}

// writeApplicationWaitDiagnostic records backend-owned admission state only
// after a pathological multi-second wait. It deliberately avoids guest or
// Sentry policy data and writes to the existing panic-log descriptor so a
// lifecycle timeout has actionable evidence even when control RPCs are also
// waiting behind the affected task.
func (e *ring0Engine) writeApplicationWaitDiagnostic(tid uint64, started time.Time) {
	e.cpuMu.Lock()
	var state strings.Builder
	fmt.Fprintf(&state, "ttc hvf application wait: tid=%d elapsed=%s concurrency=%d residencies=%d waiters=%d ids=", tid, time.Since(started).Round(time.Millisecond), e.concurrency, e.applicationCPUs.Load(), len(e.cpuWaiters))
	for id, used := range e.cpuIDs {
		if used {
			fmt.Fprintf(&state, "%d,", id)
		}
	}
	for id, cpu := range e.cpusByID {
		if cpu == nil {
			continue
		}
		cancel := atomic.LoadUint64((*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateCancelRequested)))
		fmt.Fprintf(&state, " cpu%d={tid:%d uses:%d active:%t guest:%t cancel:%d destroyed:%t age:%s}", id, cpu.tid, cpu.uses.Load(), cpu.applicationActive.Load(), cpu.bridge.inGuest(), cancel, cpu.bridge.destroyed(), time.Since(cpu.residentSince).Round(time.Millisecond))
	}
	state.WriteByte('\n')
	e.cpuMu.Unlock()
	_, _ = syscall.Write(3, []byte(state.String()))
}

func (e *ring0Engine) enqueueApplicationWaiterLocked() *ring0ApplicationWaiter {
	waiter := &ring0ApplicationWaiter{}
	e.cpuWaiters = append(e.cpuWaiters, waiter)
	e.applicationWaiters.Add(1)
	return waiter
}

func (e *ring0Engine) removeApplicationWaiter(waiter *ring0ApplicationWaiter) {
	e.cpuMu.Lock()
	e.removeApplicationWaiterLocked(waiter)
	e.cpuMu.Unlock()
}

func (e *ring0Engine) removeApplicationWaiterLocked(waiter *ring0ApplicationWaiter) {
	for index, candidate := range e.cpuWaiters {
		if candidate != waiter {
			continue
		}
		copy(e.cpuWaiters[index:], e.cpuWaiters[index+1:])
		e.cpuWaiters[len(e.cpuWaiters)-1] = nil
		e.cpuWaiters = e.cpuWaiters[:len(e.cpuWaiters)-1]
		e.applicationWaiters.Add(-1)
		return
	}
}

// ShouldYieldApplicationResidency provides bounded vCPU handoff only when an
// application is actually waiting for an owner-affine slot. A short residency
// quantum amortizes vCPU creation across syscall bursts while preventing a
// busy task or live-fork clone from starving later execs indefinitely.
func (e *ring0Engine) ShouldYieldApplicationResidency() bool {
	if e.applicationWaiters.Load() == 0 {
		return false
	}
	tid := hosttid.Current()
	e.cpuMu.Lock()
	cpu := e.findApplicationCPUByTIDLocked(tid)
	shouldYield := cpu != nil && time.Since(cpu.residentSince) >= applicationResidencyQuantum
	e.cpuMu.Unlock()
	return shouldYield
}

// ShouldRetainApplicationFaultResidency keeps the owner vCPU available for
// the common local page-cache fault, avoiding a destroy/create cycle before
// the next EL0 entry. At most half of the application pool may be retained by
// fault handlers, leaving the other half available if those handlers block on
// gofer or remote-page work without passing through Task.Block.
func (e *ring0Engine) ShouldRetainApplicationFaultResidency() bool {
	if e.concurrency < 2 {
		return false
	}
	reserveThreshold := int64(e.concurrency / 2)
	return e.applicationCPUs.Load() <= reserveThreshold
}

func (e *ring0Engine) releaseApplicationCPU(cpu *ring0ApplicationCPU) {
	uses := cpu.uses.Add(-1)
	if uses < 0 {
		panic(fmt.Sprintf("negative HVF ring0 vCPU %d reference count %d", cpu.vcpu.id, uses))
	}
	if uses != 0 || !cpu.retiring.Load() {
		return
	}
	e.cpuMu.Lock()
	if id := cpu.vcpu.id; id >= 0 && id < len(e.cpusByID) && e.cpusByID[id] == cpu {
		e.reapApplicationCPULocked(cpu)
	}
	e.cpuMu.Unlock()
}

// pinApplicationCPUs returns a stable snapshot of every application bridge.
// A bridge is freed by reapDestroyedCPUsLocked only when uses reaches zero,
// so callers may invoke RequestExit after dropping cpuMu without racing a
// concurrent application return that retires its owner-affine vCPU.
func (e *ring0Engine) pinApplicationCPUs() []*ring0ApplicationCPU {
	e.cpuMu.Lock()
	defer e.cpuMu.Unlock()
	cpus := make([]*ring0ApplicationCPU, 0, e.concurrency)
	for _, cpu := range e.cpusByID {
		if cpu == nil {
			continue
		}
		if cpu.tryPin() {
			cpus = append(cpus, cpu)
		}
	}
	return cpus
}

func (e *ring0Engine) unpinApplicationCPUs(cpus []*ring0ApplicationCPU) {
	for _, cpu := range cpus {
		e.releaseApplicationCPU(cpu)
	}
}

// reapDestroyedCPUsLocked retires bridges only after their owning signal
// handler has destroyed the underlying HVF vCPU. This makes the small fixed
// vCPU-ID/state-page set recyclable across Go's changing host-thread set
// without ever invoking an owner-affine HVF operation from the wrong thread.
func (e *ring0Engine) reapDestroyedCPUsLocked() {
	for _, cpu := range e.cpusByID {
		if cpu == nil {
			continue
		}
		e.reapApplicationCPULocked(cpu)
	}
}

func (e *ring0Engine) reapApplicationCPULocked(cpu *ring0ApplicationCPU) {
	id := cpu.vcpu.id
	if id < 0 || id >= len(e.cpusByID) || e.cpusByID[id] != cpu ||
		!cpu.retiring.Load() || !cpu.bridge.destroyed() || !cpu.uses.CompareAndSwap(0, -1) {
		return
	}
	e.learnHostGateFault(cpu)
	e.cpusByTID.Delete(cpu.tid)
	e.cpuIDs[id] = false
	e.cpusByID[id] = nil
	e.applicationCPUs.Add(-1)
	cpu.bridge.close()
}

// learnHostGateFault classifies an EL1 data abort that entered the host-service
// gate and queues the missing Darwin slab for the next bounded mirror refresh.
// The faulting continuation already resumed at host EL0, so reconciliation can
// use Mach and Hypervisor.framework without nesting a host operation in EL1.
func (e *ring0Engine) learnHostGateFault(cpu *ring0ApplicationCPU) {
	state := unsafe.Slice((*byte)(unsafe.Pointer(cpu.vcpu.stateHost)), vmm.HostSlabSize)
	esr := binary.LittleEndian.Uint64(state[stateHostGateESR:])
	far := binary.LittleEndian.Uint64(state[stateHostGateFAR:])
	clear(state[stateHostGateESR : stateHostGateFAR+8])
	if esr == 0 {
		return
	}
	ec := esr >> 26 & 0x3f
	if ec != 0x25 || far >= ring0HostVALimit {
		return
	}
	e.requestHostMirror(uintptr(far))
}

func (*ring0Engine) clearHostGateFault(cpu *ring0ApplicationCPU) {
	state := unsafe.Slice((*byte)(unsafe.Pointer(cpu.vcpu.stateHost)), vmm.HostSlabSize)
	clear(state[stateHostGateESR : stateHostGateFAR+8])
}

// extendHostMirror services the small set of Sentry VAs sampled after a host
// gate or before first EL1 residency. Unlike refreshHostMirror, it performs a
// Mach lookup only for regions containing missing slabs and never reconciles
// unrelated runtime memory. This keeps a demand fault proportional to the
// touched working set instead of the process's complete VM map.
func (e *ring0Engine) extendHostMirror(ctx context.Context) error {
	e.mirrorMu.Lock()
	defer e.mirrorMu.Unlock()

	pendingSlabs := make(map[uintptr]struct{})
	e.faultMu.Lock()
	for address := range e.pendingMirror {
		if _, mapped := e.stage2Slabs[address]; mapped {
			delete(e.pendingMirror, address)
			continue
		}
		pendingSlabs[address] = struct{}{}
	}
	e.faultMu.Unlock()
	if len(pendingSlabs) == 0 {
		return nil
	}

	if err := e.lockApplicationEntries(ctx); err != nil {
		return fmt.Errorf("excluding application entries for Sentry working-set extension: %w", err)
	}
	defer e.unlockApplicationEntries()
	desiredSlabs := make(map[uintptr]ring0Stage2Identity)
	desiredAccess := make(map[uint64]monitorabi.Access)
	seenRegions := make(map[uintptr]ring0HostRegion)
	addSlab := func(region ring0HostRegion, address uintptr, access monitorabi.Access, stage2Access vmm.Stage2Access) {
		if !e.hostSlabCanMirror(region, address) {
			return
		}
		offset := uint64(address - region.address)
		desiredSlabs[address] = ring0Stage2Identity{
			objectID:     region.objectID,
			objectOffset: region.objectOffset + offset,
			access:       stage2Access,
			mainImage:    region.mainImage,
		}
		for va := uint64(address); va < uint64(address)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
			desiredAccess[va] = access
		}
	}
	for address := range pendingSlabs {
		region, ok, err := ring0HostRegionAt(address)
		if err != nil {
			return err
		}
		if !ok {
			// Sampled runtime pointers can become unreachable before this owner
			// acquires mirrorMu. They are hints, not lifetime leases.
			continue
		}
		seenRegions[region.address] = region
	}
	for _, region := range seenRegions {
		access := monitorabi.AccessRead
		stage2Access := vmm.Stage2Access(0)
		if region.protection&ring0VMRead != 0 {
			stage2Access |= vmm.Stage2Read
		}
		if region.protection&ring0VMWrite != 0 {
			access |= monitorabi.AccessWrite
			stage2Access |= vmm.Stage2Write
		}
		if region.protection&ring0VMExecute != 0 && region.mainImage {
			access |= monitorabi.AccessExecute
			stage2Access |= vmm.Stage2Execute
		}
		if stage2Access == 0 {
			continue
		}
		regionEnd := region.address + uintptr(region.length)
		if region.protection&ring0VMWrite != 0 || region.mainImage {
			// Go heap and stack spans grow throughout an existing Mach region.
			// Importing a touched writable region as one run avoids a demand-fault
			// storm across Sentry objects.
			for address := region.address; address < regionEnd; address += uintptr(vmm.HostSlabSize) {
				addSlab(region, address, access, stage2Access)
			}
			continue
		}
		for requested := range pendingSlabs {
			if requested < region.address || requested >= regionEnd {
				continue
			}
			windowStart := requested
			if uint64(requested-region.address) > ring0HostWorkingSetRadius {
				windowStart -= uintptr(ring0HostWorkingSetRadius)
			} else {
				windowStart = region.address
			}
			windowEnd := requested + uintptr(vmm.HostSlabSize) + uintptr(ring0HostWorkingSetRadius)
			if windowEnd < requested || windowEnd > regionEnd {
				windowEnd = regionEnd
			}
			for address := windowStart; address < windowEnd; address += uintptr(vmm.HostSlabSize) {
				addSlab(region, address, access, stage2Access)
			}
		}
	}

	newSlabs := make([]uintptr, 0, len(desiredSlabs))
	var accessChanged []uintptr
	for address, desired := range desiredSlabs {
		old, mapped := e.stage2Slabs[address]
		if !mapped {
			newSlabs = append(newSlabs, address)
		} else if old.access != desired.access {
			accessChanged = append(accessChanged, address)
		}
	}
	if len(accessChanged) != 0 {
		pages := make(map[uint64]struct{}, len(accessChanged)*int(vmm.HostSlabSize/vmm.LogicalPageSize))
		for _, address := range accessChanged {
			for va := uint64(address); va < uint64(address)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
				pages[va] = struct{}{}
			}
		}
		if err := e.applyHostStage1(ctx, hostStage1UnmapOperations(pages)); err != nil {
			return err
		}
		if err := e.unmapHostStage2Slabs(accessChanged); err != nil {
			return err
		}
		newSlabs = append(newSlabs, accessChanged...)
	}
	skipped, err := e.mapHostStage2Slabs(newSlabs, desiredSlabs)
	if err != nil {
		return err
	}
	dropSkippedHostSlabs(skipped, desiredSlabs, desiredAccess)

	desiredGPA := make(map[uint64]uint64, len(desiredAccess))
	currentSubset := make(map[uint64]monitorabi.Access, len(desiredAccess))
	for va := range desiredAccess {
		slabAddress := uintptr(va &^ (vmm.HostSlabSize - 1))
		slab, ok := e.stage2Slabs[slabAddress]
		if !ok {
			return fmt.Errorf("Sentry host VA %#x has no stage-2 slab", va)
		}
		desiredGPA[va] = slab.gpa + va - uint64(slabAddress)
		if old, mapped := e.stage1Access[va]; mapped {
			currentSubset[va] = old
		}
	}
	if err := e.applyHostStage1(ctx, hostStage1ReconcileOperations(currentSubset, desiredAccess, desiredGPA)); err != nil {
		return err
	}

	e.faultMu.Lock()
	for address := range pendingSlabs {
		delete(e.pendingMirror, address)
	}
	e.faultMu.Unlock()
	return nil
}

func (e *ring0Engine) refreshHostMirror(ctx context.Context) error {
	e.mirrorMu.Lock()
	defer e.mirrorMu.Unlock()
	bootstrap := !e.mirrorInitialized
	if !bootstrap {
		// RunApplication samples a small fixed set of continuation pointers after
		// every genuine host gate. Most of those slabs are process-lifetime
		// mappings. Do not walk thousands of Mach VM regions and rendezvous every
		// vCPU merely to rediscover that the requested slabs are already live.
		needsReconcile := false
		e.faultMu.Lock()
		for address := range e.pendingMirror {
			if _, mapped := e.stage2Slabs[address]; mapped {
				delete(e.pendingMirror, address)
				continue
			}
			needsReconcile = true
		}
		e.faultMu.Unlock()
		if !needsReconcile {
			return nil
		}
	}
	if err := e.lockApplicationEntries(ctx); err != nil {
		return fmt.Errorf("excluding application entries for host-memory reconciliation: %w", err)
	}
	defer e.unlockApplicationEntries()
	regions, err := snapshotRing0HostRegions(ring0HostVALimit)
	if err != nil {
		return err
	}
	desiredSlabs := make(map[uintptr]ring0Stage2Identity)
	desiredAccess := make(map[uint64]monitorabi.Access)
	retainedSlabs := make([]uintptr, 0, len(e.stage2Slabs))
	for address := range e.stage2Slabs {
		retainedSlabs = append(retainedSlabs, address)
	}
	sort.Slice(retainedSlabs, func(i, j int) bool { return retainedSlabs[i] < retainedSlabs[j] })
	workingSetSlabs := make(map[uintptr]struct{})
	addWorkingSetSlab := func(address uintptr) {
		address &^= uintptr(vmm.HostSlabSize - 1)
		if uint64(address) >= ring0HostVALimit {
			return
		}
		if _, mapped := e.stage2Slabs[address]; !mapped {
			workingSetSlabs[address] = struct{}{}
		}
	}
	// The guest-only tail of SwitchApplication contains nested calls after the
	// bluepill. Go may move SP across a 16K boundary while building those call
	// frames, so include both adjacent slabs around the current stack sample.
	stackSlab := ring0asm.CurrentSP() &^ uintptr(vmm.HostSlabSize-1)
	addWorkingSetSlab(stackSlab)
	if stackSlab >= uintptr(vmm.HostSlabSize) {
		addWorkingSetSlab(stackSlab - uintptr(vmm.HostSlabSize))
	}
	if stackSlab <= ^uintptr(0)-uintptr(vmm.HostSlabSize) {
		addWorkingSetSlab(stackSlab + uintptr(vmm.HostSlabSize))
	}
	pendingSlabs := make(map[uintptr]struct{})
	e.faultMu.Lock()
	for address := range e.pendingMirror {
		pendingSlabs[address] = struct{}{}
		addWorkingSetSlab(address)
	}
	e.faultMu.Unlock()
	workingSetAddresses := make([]uintptr, 0, len(workingSetSlabs))
	for address := range workingSetSlabs {
		workingSetAddresses = append(workingSetAddresses, address)
	}
	sort.Slice(workingSetAddresses, func(i, j int) bool { return workingSetAddresses[i] < workingSetAddresses[j] })
	forEachInRegion := func(addresses []uintptr, start, end uintptr, fn func(uintptr)) {
		index := sort.Search(len(addresses), func(i int) bool { return addresses[i] >= start })
		for index < len(addresses) && addresses[index] < end {
			fn(addresses[index])
			index++
		}
	}
	for _, region := range regions {
		access := monitorabi.AccessRead
		if region.protection&ring0VMWrite != 0 {
			access |= monitorabi.AccessWrite
		}
		if region.protection&ring0VMExecute != 0 && region.mainImage {
			access |= monitorabi.AccessExecute
		}
		stage2Access := vmm.Stage2Access(0)
		if region.protection&ring0VMRead != 0 {
			stage2Access |= vmm.Stage2Read
		}
		if region.protection&ring0VMWrite != 0 {
			stage2Access |= vmm.Stage2Write
		}
		if region.protection&ring0VMExecute != 0 && region.mainImage {
			stage2Access |= vmm.Stage2Execute
		}
		if stage2Access == 0 {
			continue
		}
		addSlab := func(address uintptr) {
			if !e.hostSlabCanMirror(region, address) {
				return
			}
			offset := uint64(address - region.address)
			desiredSlabs[address] = ring0Stage2Identity{
				objectID:     region.objectID,
				objectOffset: region.objectOffset + offset,
				access:       stage2Access,
				mainImage:    region.mainImage,
			}
			for va := uint64(address); va < uint64(address)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
				desiredAccess[va] = access
			}
		}
		if region.mainImage {
			for offset := uint64(0); offset < region.length; offset += vmm.HostSlabSize {
				addSlab(region.address + uintptr(offset))
			}
			continue
		}
		regionEnd := region.address + uintptr(region.length)
		workingSetIndex := sort.Search(len(workingSetAddresses), func(i int) bool {
			return workingSetAddresses[i] >= region.address
		})
		workingSetRegion := workingSetIndex < len(workingSetAddresses) && workingSetAddresses[workingSetIndex] < regionEnd
		bootstrapOwned := region.userTag == 240 || region.userTag == 242
		if region.protection&ring0VMWrite != 0 &&
			(workingSetRegion || bootstrap && bootstrapOwned) {
			// At bootstrap, import each complete explicitly owned region so a
			// pre-existing Go or VMM object cannot spuriously redpill based on its
			// placement. Demand mapping retains the narrower working-set rule for
			// tagged mappings created after bootstrap.
			for offset := uint64(0); offset < region.length; offset += vmm.HostSlabSize {
				addSlab(region.address + uintptr(offset))
			}
			continue
		}
		forEachInRegion(retainedSlabs, region.address, regionEnd, addSlab)
		forEachInRegion(workingSetAddresses, region.address, regionEnd, func(address uintptr) {
			windowStart := address
			if uint64(address-region.address) > ring0HostWorkingSetRadius {
				windowStart = address - uintptr(ring0HostWorkingSetRadius)
			} else {
				windowStart = region.address
			}
			windowEnd := address + uintptr(vmm.HostSlabSize) + uintptr(ring0HostWorkingSetRadius)
			if windowEnd < address || windowEnd > regionEnd {
				windowEnd = regionEnd
			}
			for slab := windowStart; slab < windowEnd; slab += uintptr(vmm.HostSlabSize) {
				addSlab(slab)
			}
		})
	}

	// Completely new stage-2 slabs can be installed before their stage-1
	// leaves. Replaced slabs must first lose all stage-1 references so HVF
	// never observes a live leaf while its backing VM object changes.
	var newSlabs, replacedOrRemoved []uintptr
	for address, desired := range desiredSlabs {
		old, mapped := e.stage2Slabs[address]
		if !mapped {
			newSlabs = append(newSlabs, address)
			continue
		}
		// A zero Mach object ID cannot prove that an address still names the same
		// allocation after Darwin reused it. Replace these conservatively so stage
		// 2 never retains an HVF lease to stale host memory.
		if desired.objectID == 0 || old.ring0Stage2Identity != desired {
			replacedOrRemoved = append(replacedOrRemoved, address)
		}
	}
	for address := range e.stage2Slabs {
		if _, desired := desiredSlabs[address]; !desired {
			replacedOrRemoved = append(replacedOrRemoved, address)
		}
	}
	skipped, err := e.mapHostStage2Slabs(newSlabs, desiredSlabs)
	if err != nil {
		return err
	}
	dropSkippedHostSlabs(skipped, desiredSlabs, desiredAccess)
	if len(replacedOrRemoved) != 0 {
		pages := make(map[uint64]struct{})
		for _, address := range replacedOrRemoved {
			for va := uint64(address); va < uint64(address)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
				if _, mapped := e.stage1Access[va]; mapped {
					pages[va] = struct{}{}
				}
			}
		}
		if err := e.applyHostStage1(ctx, hostStage1UnmapOperations(pages)); err != nil {
			return err
		}
		if err := e.unmapHostStage2Slabs(replacedOrRemoved); err != nil {
			return err
		}
		var replacements []uintptr
		for _, address := range replacedOrRemoved {
			if _, desired := desiredSlabs[address]; desired {
				replacements = append(replacements, address)
			}
		}
		skipped, err := e.mapHostStage2Slabs(replacements, desiredSlabs)
		if err != nil {
			return err
		}
		dropSkippedHostSlabs(skipped, desiredSlabs, desiredAccess)
	}
	desiredGPA := make(map[uint64]uint64, len(desiredAccess))
	for va := range desiredAccess {
		slabAddress := uintptr(va &^ (vmm.HostSlabSize - 1))
		slab, ok := e.stage2Slabs[slabAddress]
		if !ok {
			return fmt.Errorf("Sentry host VA %#x has no stage-2 slab", va)
		}
		desiredGPA[va] = slab.gpa + va - uint64(slabAddress)
	}
	if err := e.applyHostStage1(ctx, hostStage1ReconcileOperations(e.stage1Access, desiredAccess, desiredGPA)); err != nil {
		return err
	}
	// Retire a demand sample only after its slab is live. A Mach snapshot race
	// may skip an immutable slab; preserving the sample lets the next host-gate
	// retry it.
	e.faultMu.Lock()
	for address := range pendingSlabs {
		if _, mapped := desiredSlabs[address]; mapped {
			delete(e.pendingMirror, address)
		}
	}
	e.faultMu.Unlock()
	e.mirrorInitialized = true
	return nil
}

func dropSkippedHostSlabs(addresses []uintptr, desiredSlabs map[uintptr]ring0Stage2Identity, desiredAccess map[uint64]monitorabi.Access) {
	for _, address := range addresses {
		delete(desiredSlabs, address)
		for va := uint64(address); va < uint64(address)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
			delete(desiredAccess, va)
		}
	}
}

func prepareRing0Stage2Lease(source uintptr, length uint64, shadow bool) (*ring0Stage2Lease, error) {
	hostAddress := source
	if shadow {
		var err error
		hostAddress, err = (hostMemory{}).Reserve(length)
		if err == nil {
			err = copyRing0HostMemory(hostAddress, source, length)
		}
		if err != nil {
			if hostAddress != 0 {
				_ = (hostMemory{}).Release(hostAddress, length)
			}
			return nil, err
		}
	}
	return &ring0Stage2Lease{
		hostAddress: hostAddress,
		length:      length,
		shadow:      shadow,
		tracked:     !shadow,
	}, nil
}

func releaseRing0Stage2Lease(lease *ring0Stage2Lease) error {
	if lease == nil {
		return nil
	}
	if !lease.shadow {
		return nil
	}
	return (hostMemory{}).Release(lease.hostAddress, lease.length)
}

func ring0HostMirrorNeedsShadow(identity ring0Stage2Identity) bool {
	return identity.mainImage && identity.access&vmm.Stage2Write == 0
}

func (e *ring0Engine) mapHostStage2Slabs(addresses []uintptr, desired map[uintptr]ring0Stage2Identity) ([]uintptr, error) {
	sort.Slice(addresses, func(i, j int) bool { return addresses[i] < addresses[j] })
	var skipped []uintptr
	for start := 0; start < len(addresses); {
		identity := desired[addresses[start]]
		// A host mapping's protection describes how Sentry accesses it, not
		// whether its contents are immutable. In particular, safecopy commonly
		// exposes GuestRAM through read-only host mappings while the application
		// continues to modify the underlying frames. Shadowing those mappings
		// gives EL1 a stale snapshot and corrupts copy-in. Only immutable pages in
		// the Sentry's own Mach-O image may use a private compatibility shadow;
		// every other data mapping must remain a coherent direct alias.
		shadow := ring0HostMirrorNeedsShadow(identity)
		if gpa, canonicalAccess, ok := e.vm.canonicalGPAForHost(addresses[start]); !shadow && ok && identity.access&^canonicalAccess == 0 {
			e.stage2Slabs[addresses[start]] = ring0Stage2Slab{
				ring0Stage2Identity: identity,
				gpa:                 gpa,
				canonical:           true,
			}
			start++
			continue
		}
		end := start + 1
		// Private copies avoid Mach's executable-file mapping restrictions for
		// read-only image pages. Writable source mappings may be coalesced only
		// while their Mach object identity remains contiguous.
		for !shadow && end < len(addresses) &&
			addresses[end] == addresses[end-1]+uintptr(vmm.HostSlabSize) &&
			desired[addresses[end]].access == identity.access &&
			desired[addresses[end]].access&vmm.Stage2Write != 0 &&
			desired[addresses[end]].objectID == desired[addresses[end-1]].objectID &&
			desired[addresses[end]].objectOffset == desired[addresses[end-1]].objectOffset+vmm.HostSlabSize {
			end++
		}
		address := addresses[start]
		var (
			lease  *ring0Stage2Lease
			mapErr error
		)
		for {
			length := uint64(end-start) * vmm.HostSlabSize
			if e.nextMirrorGPA > GuestRAMIPAStart-length {
				return skipped, fmt.Errorf("Sentry host mirror exhausted GPA aperture [%#x, %#x)", uint64(ring0HostMirrorGPAStart), uint64(GuestRAMIPAStart))
			}
			lease, mapErr = prepareRing0Stage2Lease(address, length, shadow)
			if mapErr == nil {
				if lease.tracked {
					mapErr = e.vm.mapRing0MirrorStage2Source(uintptr(e.nextMirrorGPA), lease.hostAddress, length, identity.access)
				} else {
					mapErr = e.vm.mapRing0Stage2Source(uintptr(e.nextMirrorGPA), lease.hostAddress, length, identity.access)
				}
			}
			if mapErr == nil || end-start == 1 {
				break
			}
			if lease != nil {
				if err := releaseRing0Stage2Lease(lease); err != nil {
					return skipped, fmt.Errorf("releasing failed Sentry stage-2 alias [%#x, %#x) after mapping error %v: %w", address, address+uintptr(length), mapErr, err)
				}
				lease = nil
			}
			// A VM region may change after snapshotRing0HostRegions. Retry the
			// first slab independently so one changed neighbor does not discard
			// an otherwise stable mapping.
			end = start + 1
		}
		length := uint64(end-start) * vmm.HostSlabSize
		gpa := e.nextMirrorGPA
		if mapErr != nil {
			if lease != nil {
				if err := releaseRing0Stage2Lease(lease); err != nil {
					return skipped, fmt.Errorf("releasing unmapped Sentry stage-2 alias [%#x, %#x) after mapping error %v: %w", address, address+uintptr(length), mapErr, err)
				}
			}
			if identity.access&vmm.Stage2Execute != 0 {
				return skipped, fmt.Errorf("mapping executable Sentry stage-2 identity range [%#x, %#x) gpa=%#x access=%#x object=%#x/%#x shadow=%t: %w",
					address, address+uintptr(length), gpa, identity.access,
					identity.objectID, identity.objectOffset, shadow, mapErr)
			}
			// A non-executable mapping may race a host VM change. Leaving it
			// absent is safe: an access redpills through the host gate.
			skipped = append(skipped, addresses[start:end]...)
			start = end
			continue
		}
		e.nextMirrorGPA += length
		lease.refs = end - start
		for index, mapped := range addresses[start:end] {
			e.stage2Slabs[mapped] = ring0Stage2Slab{
				ring0Stage2Identity: desired[mapped], lease: lease,
				gpa: gpa + uint64(index)*vmm.HostSlabSize,
			}
		}
		start = end
	}
	return skipped, nil
}

func (e *ring0Engine) unmapHostStage2Slabs(addresses []uintptr) error {
	sort.Slice(addresses, func(i, j int) bool { return addresses[i] < addresses[j] })
	for start := 0; start < len(addresses); {
		if slab := e.stage2Slabs[addresses[start]]; slab.canonical {
			delete(e.stage2Slabs, addresses[start])
			start++
			continue
		}
		end := start + 1
		for end < len(addresses) &&
			addresses[end] == addresses[end-1]+uintptr(vmm.HostSlabSize) &&
			!e.stage2Slabs[addresses[end]].canonical &&
			e.stage2Slabs[addresses[end]].gpa == e.stage2Slabs[addresses[end-1]].gpa+vmm.HostSlabSize &&
			e.stage2Slabs[addresses[end]].lease.tracked == e.stage2Slabs[addresses[end-1]].lease.tracked {
			end++
		}
		address := addresses[start]
		length := uint64(end-start) * vmm.HostSlabSize
		gpa := e.stage2Slabs[address].gpa
		lease := e.stage2Slabs[address].lease
		var unmapErr error
		if lease.tracked {
			unmapErr = e.vm.unmapRing0MirrorStage2GPA(gpa, length)
		} else {
			unmapErr = e.vm.unmapRing0Stage2GPA(gpa, length)
		}
		if unmapErr != nil {
			return fmt.Errorf("unmapping Sentry stage-2 range gpa=[%#x, %#x) host=[%#x, %#x): %w", gpa, gpa+length, address, address+uintptr(length), unmapErr)
		}
		var released []*ring0Stage2Lease
		for _, unmapped := range addresses[start:end] {
			lease := e.stage2Slabs[unmapped].lease
			delete(e.stage2Slabs, unmapped)
			if lease != nil {
				lease.refs--
				if lease.refs == 0 {
					released = append(released, lease)
				}
			}
		}
		for _, lease := range released {
			if err := releaseRing0Stage2Lease(lease); err != nil {
				return fmt.Errorf("releasing Sentry stage-2 lease [%#x, %#x): %w", lease.hostAddress, lease.hostAddress+uintptr(lease.length), err)
			}
		}
		start = end
	}
	return nil
}

func (e *ring0Engine) releaseHostMirror() error {
	addresses := make([]uintptr, 0, len(e.stage2Slabs))
	for address := range e.stage2Slabs {
		addresses = append(addresses, address)
	}
	return e.unmapHostStage2Slabs(addresses)
}

// retireHostMirrorRange removes every Sentry stage-1 and secondary stage-2
// alias of a VMM-owned host range before its backing is decommitted or its HVA
// can be reused. The Go runtime VM hook cannot observe GuestRAM's explicit
// host allocator, so this is the corresponding owner-driven MMU notifier.
func (e *ring0Engine) retireHostMirrorRange(ctx context.Context, address uintptr, length uint64) error {
	if length == 0 || address%uintptr(vmm.HostSlabSize) != 0 ||
		length%vmm.HostSlabSize != 0 || address+uintptr(length) < address {
		return fmt.Errorf("invalid Sentry mirror retirement [%#x, %#x)", address, address+uintptr(length))
	}
	ring0asm.EnsureHost()
	e.mirrorMu.Lock()
	defer e.mirrorMu.Unlock()
	if err := e.lockApplicationEntries(ctx); err != nil {
		return err
	}
	defer e.unlockApplicationEntries()
	end := address + uintptr(length)
	var addresses []uintptr
	pages := make(map[uint64]struct{})
	// stage2Slabs is keyed by HVA slab base. Retirement ranges are aligned, so
	// walk only the requested range instead of scanning the entire Sentry
	// mirror. GuestRAM commonly retires one 16 KiB slab at a time; an O(total
	// mirrored heap) scan here otherwise dominates process-exit churn.
	for slabAddress := address; slabAddress < end; slabAddress += uintptr(vmm.HostSlabSize) {
		if _, mapped := e.stage2Slabs[slabAddress]; !mapped {
			continue
		}
		addresses = append(addresses, slabAddress)
		for va := uint64(slabAddress); va < uint64(slabAddress)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
			if _, mapped := e.stage1Access[va]; mapped {
				pages[va] = struct{}{}
			}
		}
	}
	if len(addresses) == 0 {
		return nil
	}
	if err := e.applyHostStage1(ctx, hostStage1UnmapOperations(pages)); err != nil {
		return fmt.Errorf("retiring Sentry stage-1 aliases for host range [%#x, %#x): %w", address, end, err)
	}
	if err := e.unmapHostStage2Slabs(addresses); err != nil {
		return fmt.Errorf("retiring Sentry stage-2 aliases for host range [%#x, %#x): %w", address, end, err)
	}
	e.faultMu.Lock()
	for pending := range e.pendingMirror {
		if pending >= address && pending < end {
			delete(e.pendingMirror, pending)
		}
	}
	e.faultMu.Unlock()
	return nil
}

// FlushHostExecutionCache discards every mutable or allocator-owned Sentry
// alias while application continuations are quiesced. hv_vm_map leases the
// pages that currently back a host range, whereas Darwin's allocator can later
// recycle the same virtual region without preserving that lease's identity.
// State capture is a natural cache boundary: it allocates and decodes large
// object graphs and is therefore especially likely to recycle heap spans.
func (e *ring0Engine) FlushHostExecutionCache(ctx context.Context) error {
	ring0asm.EnsureHost()
	e.mirrorMu.Lock()
	defer e.mirrorMu.Unlock()
	if err := e.lockApplicationEntries(ctx); err != nil {
		return err
	}
	defer e.unlockApplicationEntries()
	var addresses []uintptr
	pages := make(map[uint64]struct{})
	for address, slab := range e.stage2Slabs {
		if slab.mainImage {
			continue
		}
		addresses = append(addresses, address)
		for va := uint64(address); va < uint64(address)+vmm.HostSlabSize; va += vmm.LogicalPageSize {
			if _, mapped := e.stage1Access[va]; mapped {
				pages[va] = struct{}{}
			}
		}
	}
	if err := e.applyHostStage1(ctx, hostStage1UnmapOperations(pages)); err != nil {
		return err
	}
	if err := e.unmapHostStage2Slabs(addresses); err != nil {
		return err
	}
	e.faultMu.Lock()
	for address := range e.pendingMirror {
		if _, mapped := e.stage2Slabs[address]; !mapped {
			delete(e.pendingMirror, address)
		}
	}
	e.faultMu.Unlock()
	return nil
}

func hostStage1UnmapOperations(pages map[uint64]struct{}) []monitorabi.Operation {
	addresses := make([]uint64, 0, len(pages))
	for address := range pages {
		addresses = append(addresses, address)
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i] < addresses[j] })
	operations := make([]monitorabi.Operation, 0, len(addresses))
	for start := 0; start < len(addresses); {
		end := start + 1
		for end < len(addresses) && addresses[end] == addresses[end-1]+vmm.LogicalPageSize {
			end++
		}
		operations = append(operations, monitorabi.Operation{
			Type: monitorabi.OperationUnmap, VA: addresses[start],
			Length: uint64(end-start) * vmm.LogicalPageSize,
		})
		start = end
	}
	return operations
}

func hostStage1ReconcileOperations(current, desired map[uint64]monitorabi.Access, desiredGPA map[uint64]uint64) []monitorabi.Operation {
	addresses := make([]uint64, 0, len(current)+len(desired))
	seen := make(map[uint64]struct{}, len(current)+len(desired))
	for address := range current {
		seen[address] = struct{}{}
		addresses = append(addresses, address)
	}
	for address := range desired {
		if _, ok := seen[address]; !ok {
			addresses = append(addresses, address)
		}
	}
	sort.Slice(addresses, func(i, j int) bool { return addresses[i] < addresses[j] })
	operations := make([]monitorabi.Operation, 0)
	for index := 0; index < len(addresses); {
		address := addresses[index]
		old, mapped := current[address]
		want, wanted := desired[address]
		var opType monitorabi.OperationType
		switch {
		case !wanted:
			opType = monitorabi.OperationUnmap
		case !mapped:
			opType = monitorabi.OperationMap
		case old != want:
			opType = monitorabi.OperationProtect
		default:
			index++
			continue
		}
		end := index + 1
		for end < len(addresses) && addresses[end] == addresses[end-1]+vmm.LogicalPageSize {
			next := addresses[end]
			nextOld, nextMapped := current[next]
			nextWant, nextWanted := desired[next]
			if opType == monitorabi.OperationUnmap && nextWanted ||
				opType == monitorabi.OperationMap && (nextMapped || !nextWanted || nextWant != want || desiredGPA[next] != desiredGPA[address]+uint64(end-index)*vmm.LogicalPageSize) ||
				opType == monitorabi.OperationProtect && (!nextMapped || !nextWanted || nextOld == nextWant || nextWant != want) {
				break
			}
			end++
		}
		operation := monitorabi.Operation{Type: opType, VA: address, Length: uint64(end-index) * vmm.LogicalPageSize}
		if opType != monitorabi.OperationUnmap {
			operation.Access = want
		}
		if opType == monitorabi.OperationMap {
			operation.GPA = desiredGPA[address]
		}
		operations = append(operations, operation)
		index = end
	}
	return operations
}

func (e *ring0Engine) applyHostStage1(ctx context.Context, operations []monitorabi.Operation) error {
	operations = splitHostStage1Operations(operations)
	for start := 0; start < len(operations); {
		end := start
		pages := 0
		for end < len(operations) && end-start < monitorabi.MaxOperations {
			operationPages := int(operations[end].Length / vmm.LogicalPageSize)
			if end != start && pages+operationPages > monitorabi.MaxOperations {
				break
			}
			pages += operationPages
			end++
		}
		batch := operations[start:end]
		if err := e.monitor.Execute(ctx, monitorabi.Command{
			Opcode: monitorabi.OpcodeApply, AddressSpaceID: ring0SentryAddressSpaceID,
			Operations: batch,
		}); err != nil {
			return fmt.Errorf("reconciling Sentry stage-1 identity mappings: %w", err)
		}
		for _, operation := range batch {
			for va := operation.VA; va < operation.VA+operation.Length; va += vmm.LogicalPageSize {
				if operation.Type == monitorabi.OperationUnmap {
					delete(e.stage1Access, va)
				} else {
					e.stage1Access[va] = operation.Access
				}
			}
		}
		start = end
	}
	return nil
}

func splitHostStage1Operations(operations []monitorabi.Operation) []monitorabi.Operation {
	const maxLength = uint64(monitorabi.MaxOperations) * vmm.LogicalPageSize
	result := make([]monitorabi.Operation, 0, len(operations))
	for _, operation := range operations {
		for offset := uint64(0); offset < operation.Length; offset += maxLength {
			part := operation
			part.VA += offset
			if part.Type == monitorabi.OperationMap {
				part.GPA += offset
			}
			part.Length = min(maxLength, operation.Length-offset)
			result = append(result, part)
		}
	}
	return result
}

func (e *ring0Engine) InterruptApplications() {
	cpus := e.pinApplicationCPUs()
	defer e.unpinApplicationCPUs(cpus)
	lastRequest := make(map[*ring0ApplicationCPU]time.Time, len(cpus))
	for _, cpu := range cpus {
		cancel := (*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateCancelRequested))
		if cpu.applicationActive.Load() && !cpu.bridge.destroyed() {
			atomic.StoreUint64(cancel, 1)
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		pending := false
		for _, cpu := range cpus {
			if !cpu.applicationActive.Load() || cpu.bridge.destroyed() {
				continue
			}
			exitKind := (*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateExitKind))
			kind := atomic.LoadUint64(exitKind)
			if kind == 3 {
				continue
			}
			pending = true
			// An internal MMU retry also passes through the monitor finish path,
			// which clears cancelRequested. Republish the external request until
			// TTC_EXIT_INTERRUPTED is visible or RunApplication returns for another
			// reason and drops applicationActive.
			cancel := (*uint64)(unsafe.Add(unsafe.Pointer(cpu.vcpu.stateHost), stateCancelRequested))
			atomic.StoreUint64(cancel, 1)
			if kind != 0 {
				continue
			}
			// A runtime mapping mutation temporarily withdraws the vCPU from
			// guest admission. Keep the application pending until it is admitted
			// again; RequestExit issued while hv_vcpu_run is inactive is not a
			// persistent virtual interrupt on macOS 15.
			if !cpu.bridge.inGuest() {
				continue
			}
			// After cancellation, give hv_vcpu_run enough time to deliver the
			// injected level IRQ. Repeatedly canceling the next run at millisecond
			// cadence can win before the monitor vector on macOS 15 and starve the
			// very acknowledgement this loop is waiting for.
			if last := lastRequest[cpu]; last.IsZero() || time.Since(last) >= 10*time.Millisecond {
				cpu.bridge.requestExternalInterrupt()
				if err := cpu.vcpu.RequestExit(); err != nil {
					return
				}
				lastRequest[cpu] = time.Now()
			}
		}
		if !pending {
			return
		}
		if time.Now().After(deadline) {
			return
		}
		runtime.Gosched()
	}
}

func (e *ring0Engine) ConcurrencyCount() int { return e.concurrency }

func (e *ring0Engine) Close() (retErr error) {
	e.closeMu.Lock()
	defer e.closeMu.Unlock()
	e.cpuMu.Lock()
	if e.closed.Load() {
		retErr = e.closeErr
		e.cpuMu.Unlock()
		return retErr
	}
	e.cpuMu.Unlock()
	// Wait for any host allocator notification already using this engine, then
	// prevent new GuestRAM releases from entering while the monitor is torn
	// down. Platform epoch transitions exclude allocator users at this point.
	e.vm.hostState.unregister(e)
	e.cpuMu.Lock()
	e.closed.Store(true)
	e.cpuMu.Unlock()
	close(e.clockStop)
	<-e.clockDone
	// GuestRAM rollover is commonly initiated by the resident Sentry task while
	// servicing a syscall. If a prior host gate has already returned that task to
	// Darwin, hv_vcpus_exit cannot retire its idle owner-affine vCPU. The caller
	// is the owner thread, so destroy that bridge directly before waiting for the
	// remaining running continuations.
	if err := e.retireCurrentApplicationCPU(); err != nil {
		return err
	}
	defer func() {
		e.monitorPool.Close()
		e.closeErr = retErr
	}()

	// A ring0 application vCPU remains inside hv_vcpu_run while its Sentry
	// continuation executes at EL1. Requesting an exit returns that continuation
	// through the owner thread's signal handler, which destroys the vCPU there.
	// Wait for the release acknowledgement before destroying the VM epoch.
	deadline := time.Now().Add(5 * time.Second)
	lastExitRequest := make(map[*ring0ApplicationCPU]time.Time)
	for {
		e.cpuMu.Lock()
		e.reapDestroyedCPUsLocked()
		remainingCount := e.applicationCPUCountLocked()
		e.cpuMu.Unlock()
		if remainingCount == 0 {
			break
		}
		remaining := e.pinApplicationCPUs()
		var requestErr error
		var waitDetail string
		for _, cpu := range remaining {
			if cpu.bridge.destroyed() {
				continue
			}
			cpu.retiring.Store(true)
			cpu.bridge.requestDestroy()
			if err := cpu.bridge.error(fmt.Sprintf("destroying owner-affine HVF ring0 vCPU %d after bridge failure", cpu.vcpu.id)); err != nil {
				requestErr = err
				break
			}
			state := unsafe.Slice((*byte)(unsafe.Pointer(cpu.vcpu.stateHost)), vmm.HostSlabSize)
			applicationExited := binary.LittleEndian.Uint64(state[stateExitKind:]) != 0
			waitDetail = fmt.Sprintf("vCPU=%d in_guest=%t destroyed=%t cancellations=%d cancel_pc=%#x cancel_pstate=%#x application_exit=%d",
				cpu.vcpu.id, cpu.bridge.inGuest(), cpu.bridge.destroyed(),
				cpu.bridge.cancelCount(), cpu.bridge.lastCancelPC(),
				cpu.bridge.lastCancelPState(),
				binary.LittleEndian.Uint64(state[stateExitKind:]))
			lastRequest := lastExitRequest[cpu]
			// Avoid repeatedly winning hv_vcpu_run's cancellation race before
			// the injected EL0 IRQ can be delivered. Once the monitor has
			// published an application exit, request the second, EL1 redpill
			// promptly; otherwise retry only if the first request caught monitor
			// entry rather than the application.
			if !lastRequest.IsZero() && !applicationExited && time.Since(lastRequest) < 10*time.Millisecond {
				continue
			}
			if err := cpu.vcpu.RequestExit(); err != nil {
				requestErr = fmt.Errorf("requesting owner-affine HVF ring0 vCPU %d exit: %w", cpu.vcpu.id, err)
				break
			}
			lastExitRequest[cpu] = time.Now()
		}
		e.unpinApplicationCPUs(remaining)
		if requestErr != nil {
			return requestErr
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %d owner-affine HVF ring0 vCPUs to retire: %s", remainingCount, waitDetail)
		}
		time.Sleep(time.Millisecond)
	}
	// No new RunApplication can pass its closed check, and every prior reader
	// has now returned through its owner thread. Exclude any reconciliation
	// that began before closed was published while tearing down the mirror.
	if err := e.lockApplicationEntries(context.Background()); err != nil {
		return err
	}
	defer e.unlockApplicationEntries()
	if err := e.releaseHostMirror(); err != nil {
		return err
	}
	return nil
}

var _ vmplatform.VMEngine = (*ring0Engine)(nil)
var _ vmplatform.HostExecutionCacheEngine = (*ring0Engine)(nil)
