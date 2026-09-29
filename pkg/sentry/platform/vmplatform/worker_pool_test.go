// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vmplatform

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
)

type fakeVM struct {
	started chan struct{}
}

func (*fakeVM) MapStage2(uint64, uintptr, uint64, Stage2Access) error { return nil }
func (*fakeVM) UnmapStage2(uint64, uint64) error                      { return nil }
func (f *fakeVM) CreateVCPU(int) (VCPU, error) {
	return &fakeVCPU{started: f.started, exit: make(chan struct{}, 1)}, nil
}
func (*fakeVM) Close() error { return nil }

type fakeVCPU struct {
	started chan struct{}
	exit    chan struct{}
	once    sync.Once
}

func (*fakeVCPU) RunMonitor(_ context.Context, encoded []byte) ([]byte, error) {
	command, err := monitorabi.UnmarshalCommand(encoded)
	if err != nil {
		return nil, err
	}
	return monitorabi.MarshalResponse(monitorabi.Response{Sequence: command.Sequence, Status: monitorabi.StatusOK}), nil
}

func (v *fakeVCPU) RequestExit() error {
	select {
	case v.exit <- struct{}{}:
	default:
	}
	return nil
}

func (*fakeVCPU) Close() error { return nil }

func TestWorkerPoolCancellationInterruptsWorker(t *testing.T) {
	vm := &fakeVM{started: make(chan struct{})}
	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatalf("NewWorkerPool: %v", err)
	}
	defer pool.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := pool.Do(ctx, func(ctx context.Context, vcpu VCPU) (any, error) {
			fake := vcpu.(*fakeVCPU)
			fake.once.Do(func() { close(fake.started) })
			<-fake.exit
			return nil, ctx.Err()
		})
		done <- err
	}()
	select {
	case <-vm.started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatalf("worker error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancellation did not interrupt worker")
	}
}

// startUninterruptibleJob runs a job that, like a monitor transaction,
// ignores interruption and finishes only when release is closed. It returns
// the result of Do.
func startUninterruptibleJob(t *testing.T, pool *WorkerPool, ctx context.Context, release <-chan struct{}) <-chan workerResult {
	t.Helper()
	started := make(chan struct{})
	done := make(chan workerResult, 1)
	go func() {
		value, err := pool.Do(ctx, func(context.Context, VCPU) (any, error) {
			close(started)
			<-release
			return "completed", nil
		})
		done <- workerResult{value: value, err: err}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not start the job")
	}
	return done
}

func checkDoWaitsForJob(t *testing.T, done <-chan workerResult, release chan<- struct{}) {
	t.Helper()
	select {
	case result := <-done:
		t.Fatalf("Do returned (%v, %v) while its job was still running", result.value, result.err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case result := <-done:
		if result.value != "completed" || result.err != nil {
			t.Fatalf("Do = (%v, %v), want the completed job's (completed, nil)", result.value, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Do did not return after its job completed")
	}
}

// TestWorkerPoolDoWaitsForJobWhenPoolStops checks that Do doesn't return
// while a job it handed to a worker is still running when the pool stops.
func TestWorkerPoolDoWaitsForJobWhenPoolStops(t *testing.T) {
	pool, err := NewWorkerPool(&fakeVM{started: make(chan struct{})}, 1)
	if err != nil {
		t.Fatalf("NewWorkerPool: %v", err)
	}
	release := make(chan struct{})
	done := startUninterruptibleJob(t, pool, context.Background(), release)
	closed := make(chan struct{})
	go func() {
		pool.Close()
		close(closed)
	}()
	checkDoWaitsForJob(t, done, release)
	<-closed
}

// TestWorkerPoolDoReportsJobCompletedAfterCancel checks that Do reports the
// outcome of a job that completed despite its context being canceled.
func TestWorkerPoolDoReportsJobCompletedAfterCancel(t *testing.T) {
	pool, err := NewWorkerPool(&fakeVM{started: make(chan struct{})}, 1)
	if err != nil {
		t.Fatalf("NewWorkerPool: %v", err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	done := startUninterruptibleJob(t, pool, ctx, release)
	cancel()
	checkDoWaitsForJob(t, done, release)
}

func TestMonitorRejectsWrongSequence(t *testing.T) {
	vm := &wrongSequenceVM{}
	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatalf("NewWorkerPool: %v", err)
	}
	defer pool.Close()
	err = NewMonitor(newWorkerPoolTestEngine(pool), monitorPoolExpander(vm)).Execute(context.Background(), monitorabi.Command{
		Opcode:         monitorabi.OpcodeActivate,
		AddressSpaceID: 1,
	})
	if err == nil {
		t.Fatal("monitor accepted wrong response sequence")
	}
}

type wrongSequenceVM struct{ fakeVM }

func (*wrongSequenceVM) CreateVCPU(int) (VCPU, error) { return wrongSequenceVCPU{}, nil }

type wrongSequenceVCPU struct{}

func (wrongSequenceVCPU) RunMonitor(_ context.Context, encoded []byte) ([]byte, error) {
	command, err := monitorabi.UnmarshalCommand(encoded)
	if err != nil {
		return nil, err
	}
	return monitorabi.MarshalResponse(monitorabi.Response{Sequence: command.Sequence + 1, Status: monitorabi.StatusOK}), nil
}
func (wrongSequenceVCPU) RequestExit() error { return nil }
func (wrongSequenceVCPU) Close() error       { return nil }

type expandingVM struct {
	mu        sync.Mutex
	commands  []monitorabi.Command
	prepared  bool
	completed bool
	accepted  bool
}

func (*expandingVM) MapStage2(uint64, uintptr, uint64, Stage2Access) error { return nil }
func (*expandingVM) UnmapStage2(uint64, uint64) error                      { return nil }
func (v *expandingVM) CreateVCPU(int) (VCPU, error)                        { return expandingVCPU{vm: v}, nil }
func (*expandingVM) Close() error                                          { return nil }

func (v *expandingVM) PrepareMonitorPoolExpansion(context.Context) (MonitorPoolExpansion, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.prepared {
		return MonitorPoolExpansion{}, fmt.Errorf("duplicate preparation")
	}
	v.prepared = true
	return MonitorPoolExpansion{GPA: 0x1200000, Length: 0x1000000}, nil
}

func (v *expandingVM) CompleteMonitorPoolExpansion(expansion MonitorPoolExpansion, accepted bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if expansion.GPA != 0x1200000 || expansion.Length != 0x1000000 {
		return fmt.Errorf("unexpected expansion %+v", expansion)
	}
	v.completed = true
	v.accepted = accepted
	return nil
}

type expandingVCPU struct{ vm *expandingVM }

func (v expandingVCPU) RunMonitor(_ context.Context, encoded []byte) ([]byte, error) {
	command, err := monitorabi.UnmarshalCommand(encoded)
	if err != nil {
		return nil, err
	}
	v.vm.mu.Lock()
	v.vm.commands = append(v.vm.commands, command)
	index := len(v.vm.commands)
	v.vm.mu.Unlock()
	status := monitorabi.StatusOK
	if index == 1 {
		status = monitorabi.StatusNoMemory
	}
	return monitorabi.MarshalResponse(monitorabi.Response{Sequence: command.Sequence, Status: status}), nil
}
func (expandingVCPU) RequestExit() error { return nil }
func (expandingVCPU) Close() error       { return nil }

func TestMonitorExpandsPoolAndRetries(t *testing.T) {
	vm := &expandingVM{}
	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatalf("NewWorkerPool: %v", err)
	}
	defer pool.Close()
	if err := NewMonitor(newWorkerPoolTestEngine(pool), monitorPoolExpander(vm)).Execute(context.Background(), monitorabi.Command{
		Opcode:         monitorabi.OpcodeCreateAddressSpace,
		AddressSpaceID: 1,
	}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if !vm.prepared || !vm.completed || !vm.accepted {
		t.Fatalf("pool lifecycle prepared=%t completed=%t accepted=%t", vm.prepared, vm.completed, vm.accepted)
	}
	if len(vm.commands) != 3 || vm.commands[0].Opcode != monitorabi.OpcodeCreateAddressSpace ||
		vm.commands[1].Opcode != monitorabi.OpcodeExpandPageTablePool ||
		vm.commands[2].Opcode != monitorabi.OpcodeCreateAddressSpace {
		t.Fatalf("monitor commands = %+v", vm.commands)
	}
}

// addressSpaceLimitVM rejects every monitor command the way the monitor
// rejects address-space creation when its table is full.
type addressSpaceLimitVM struct{ expandingVM }

func (v *addressSpaceLimitVM) CreateVCPU(int) (VCPU, error) { return addressSpaceLimitVCPU{vm: v}, nil }

type addressSpaceLimitVCPU struct{ vm *addressSpaceLimitVM }

func (v addressSpaceLimitVCPU) RunMonitor(_ context.Context, encoded []byte) ([]byte, error) {
	command, err := monitorabi.UnmarshalCommand(encoded)
	if err != nil {
		return nil, err
	}
	v.vm.mu.Lock()
	v.vm.commands = append(v.vm.commands, command)
	v.vm.mu.Unlock()
	return monitorabi.MarshalResponse(monitorabi.Response{Sequence: command.Sequence, Status: monitorabi.StatusLimit}), nil
}
func (addressSpaceLimitVCPU) RequestExit() error { return nil }
func (addressSpaceLimitVCPU) Close() error       { return nil }

// TestNewAddressSpaceLimitIsEAGAIN checks that a full monitor address-space
// table fails fork with EAGAIN instead of growing the page-table pool or
// returning an error that has no Linux errno.
func TestNewAddressSpaceLimitIsEAGAIN(t *testing.T) {
	vm := &addressSpaceLimitVM{}
	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatalf("NewWorkerPool: %v", err)
	}
	defer pool.Close()
	engine := newWorkerPoolTestEngine(pool)
	p := &Platform{engine: engine, monitor: NewMonitor(engine, monitorPoolExpander(vm)), spaces: make(map[uint64]*addressSpace)}
	if _, err := p.NewAddressSpace(); err != linuxerr.EAGAIN {
		t.Fatalf("NewAddressSpace() error = %v, want EAGAIN", err)
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.prepared || len(vm.commands) != 1 {
		t.Fatalf("pool expansion prepared=%t after commands %+v", vm.prepared, vm.commands)
	}
}
