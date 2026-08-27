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

func (v *fakeVCPU) Run(ctx context.Context, _ any) (Exit, error) {
	v.once.Do(func() { close(v.started) })
	select {
	case <-v.exit:
		return Exit{Reason: ExitInterrupted}, nil
	case <-ctx.Done():
		return Exit{}, ctx.Err()
	}
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

func TestMonitorInterruptsApplicationWorker(t *testing.T) {
	vm := &fakeVM{started: make(chan struct{})}
	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatalf("NewWorkerPool: %v", err)
	}
	defer pool.Close()

	applicationDone := make(chan Exit, 1)
	go func() {
		exit, _ := pool.RunApplication(context.Background(), nil)
		applicationDone <- exit
	}()
	select {
	case <-vm.started:
	case <-time.After(5 * time.Second):
		t.Fatal("application vCPU did not start")
	}

	monitor := NewMonitor(pool)
	if err := monitor.Execute(context.Background(), monitorabi.Command{
		Opcode:         monitorabi.OpcodeCreateAddressSpace,
		AddressSpaceID: 1,
	}); err != nil {
		t.Fatalf("monitor.Execute: %v", err)
	}
	select {
	case exit := <-applicationDone:
		if exit.Reason != ExitInterrupted {
			t.Fatalf("application exit reason = %v, want interrupted", exit.Reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("monitor did not interrupt application worker")
	}
}

type synchronousExitRaceVM struct {
	started chan struct{}
}

func (*synchronousExitRaceVM) MapStage2(uint64, uintptr, uint64, Stage2Access) error {
	return nil
}
func (*synchronousExitRaceVM) UnmapStage2(uint64, uint64) error { return nil }
func (v *synchronousExitRaceVM) CreateVCPU(int) (VCPU, error) {
	return &synchronousExitRaceVCPU{started: v.started, exit: make(chan struct{})}, nil
}
func (*synchronousExitRaceVM) Close() error { return nil }

type synchronousExitRaceVCPU struct {
	started chan struct{}
	exit    chan struct{}
	once    sync.Once
}

func (v *synchronousExitRaceVCPU) Run(context.Context, any) (Exit, error) {
	close(v.started)
	<-v.exit
	return Exit{Reason: ExitSyscall}, nil
}
func (*synchronousExitRaceVCPU) RunMonitor(context.Context, []byte) ([]byte, error) {
	return nil, nil
}
func (v *synchronousExitRaceVCPU) RequestExit() error {
	v.once.Do(func() { close(v.exit) })
	return nil
}
func (*synchronousExitRaceVCPU) Close() error { return nil }

func TestSynchronousApplicationExitWinsContextCancellation(t *testing.T) {
	vm := &synchronousExitRaceVM{started: make(chan struct{})}
	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan workerResult, 1)
	go func() {
		exit, err := pool.RunApplication(ctx, nil)
		done <- workerResult{value: exit, err: err}
	}()
	<-vm.started
	cancel()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatalf("RunApplication returned cancellation instead of synchronous exit: %v", result.err)
		}
		exit := result.value.(Exit)
		if exit.Reason != ExitSyscall {
			t.Fatalf("exit reason = %d, want syscall", exit.Reason)
		}
	case <-time.After(time.Second):
		t.Fatal("RunApplication did not resolve cancellation/exit race")
	}
}

func TestMonitorRejectsWrongSequence(t *testing.T) {
	vm := &wrongSequenceVM{}
	pool, err := NewWorkerPool(vm, 1)
	if err != nil {
		t.Fatalf("NewWorkerPool: %v", err)
	}
	defer pool.Close()
	err = NewMonitor(pool).Execute(context.Background(), monitorabi.Command{
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

func (wrongSequenceVCPU) Run(context.Context, any) (Exit, error) { return Exit{}, nil }
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

func (expandingVCPU) Run(context.Context, any) (Exit, error) { return Exit{}, nil }
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
	if err := NewMonitor(pool).Execute(context.Background(), monitorabi.Command{
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
