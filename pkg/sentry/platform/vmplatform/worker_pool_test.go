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
