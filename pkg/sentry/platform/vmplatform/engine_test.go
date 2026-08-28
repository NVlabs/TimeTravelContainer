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
	"sync/atomic"
	"testing"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
)

func TestStage1MapRunsCoalesceContiguousFrames(t *testing.T) {
	frames := []pgalloc.BackingFrame{
		{ID: 1, GPA: 0x10000},
		{ID: 2, GPA: 0x11000},
		{ID: 3, GPA: 0x30000},
		{ID: 4, GPA: 0x31000},
		{ID: 5, GPA: 0x32000},
	}
	runs := stage1MapRuns(hostarch.Addr(0x400000), frames, monitorabi.AccessRead|monitorabi.AccessExecute)
	if len(runs) != 2 {
		t.Fatalf("stage1MapRuns returned %d runs, want 2: %+v", len(runs), runs)
	}
	for i, want := range []struct {
		va, gpa, length uint64
		first, end      int
	}{
		{0x400000, 0x10000, 2 * hostarch.PageSize, 0, 2},
		{0x402000, 0x30000, 3 * hostarch.PageSize, 2, 5},
	} {
		got := runs[i]
		if got.operation.VA != want.va || got.operation.GPA != want.gpa || got.operation.Length != want.length || got.firstFrame != want.first || got.endFrame != want.end {
			t.Errorf("run %d = %+v, want VA=%#x GPA=%#x length=%#x frames=[%d,%d)", i, got, want.va, want.gpa, want.length, want.first, want.end)
		}
	}
}

func TestStage1MapRunsBoundMonitorMutations(t *testing.T) {
	frames := make([]pgalloc.BackingFrame, monitorabi.MaxOperations+1)
	for i := range frames {
		frames[i] = pgalloc.BackingFrame{ID: pgalloc.FrameID(i + 1), GPA: 0x10000 + uint64(i)*hostarch.PageSize}
	}
	runs := stage1MapRuns(hostarch.Addr(0x400000), frames, monitorabi.AccessRead)
	if len(runs) != 2 {
		t.Fatalf("stage1MapRuns returned %d runs, want 2", len(runs))
	}
	if got, want := runs[0].operation.Length, uint64(monitorabi.MaxOperations*hostarch.PageSize); got != want {
		t.Errorf("first run length = %#x, want %#x", got, want)
	}
	if got, want := runs[1].operation.Length, uint64(hostarch.PageSize); got != want {
		t.Errorf("second run length = %#x, want %#x", got, want)
	}
}

// workerPoolTestEngine adapts the fake VCPUs used by vmplatform unit tests.
// Production platforms must supply their own sentry-at-EL1 VMEngine.
type workerPoolTestEngine struct {
	pool *WorkerPool
}

func newWorkerPoolTestEngine(pool *WorkerPool) *workerPoolTestEngine {
	return &workerPoolTestEngine{pool: pool}
}

func (e *workerPoolTestEngine) RunApplication(ctx context.Context, state ApplicationState) (Exit, error) {
	return Exit{}, fmt.Errorf("workerPoolTestEngine does not execute applications")
}

func (e *workerPoolTestEngine) RunMonitor(ctx context.Context, encoded []byte) ([]byte, error) {
	value, err := e.pool.Do(ctx, func(ctx context.Context, vcpu VCPU) (any, error) {
		return vcpu.RunMonitor(ctx, encoded)
	})
	if err != nil {
		return nil, err
	}
	response, ok := value.([]byte)
	if !ok {
		return nil, fmt.Errorf("monitor returned response of type %T, want []byte", value)
	}
	return response, nil
}

func (*workerPoolTestEngine) InterruptApplications()  {}
func (e *workerPoolTestEngine) ConcurrencyCount() int { return len(e.pool.workers) }
func (e *workerPoolTestEngine) Close() error {
	e.pool.Close()
	return nil
}

var _ VMEngine = (*workerPoolTestEngine)(nil)

type interruptCountingEngine struct {
	interrupts atomic.Uint32
}

type residencyCountingEngine struct {
	interruptCountingEngine
	begins atomic.Uint32
	ends   atomic.Uint32
	endErr error
}

type yieldingResidencyEngine struct {
	residencyCountingEngine
	yield atomic.Bool
}

type faultRetainingResidencyEngine struct {
	yieldingResidencyEngine
	retain atomic.Bool
}

func (e *residencyCountingEngine) BeginApplicationResidency() { e.begins.Add(1) }
func (e *residencyCountingEngine) EndApplicationResidency() error {
	e.ends.Add(1)
	return e.endErr
}

func (e *yieldingResidencyEngine) ShouldYieldApplicationResidency() bool {
	return e.yield.Load()
}

func (e *faultRetainingResidencyEngine) ShouldRetainApplicationFaultResidency() bool {
	return e.retain.Load()
}

func (*interruptCountingEngine) RunApplication(context.Context, ApplicationState) (Exit, error) {
	return Exit{}, nil
}
func (*interruptCountingEngine) RunMonitor(context.Context, []byte) ([]byte, error) {
	return nil, nil
}
func (e *interruptCountingEngine) InterruptApplications() { e.interrupts.Add(1) }
func (*interruptCountingEngine) ConcurrencyCount() int    { return 1 }
func (*interruptCountingEngine) Close() error             { return nil }

func TestPlatformContextInterruptWakesVMEngine(t *testing.T) {
	engine := &interruptCountingEngine{}
	c := &platformContext{platform: &Platform{engine: engine}}
	c.Interrupt()
	if got := engine.interrupts.Load(); got != 1 {
		t.Fatalf("VM engine interrupts = %d, want 1", got)
	}
	if !c.interrupted {
		t.Fatal("platform context did not retain the interrupt")
	}
}

func TestPlatformContextApplicationResidencyBoundaries(t *testing.T) {
	engine := &residencyCountingEngine{}
	c := &platformContext{platform: &Platform{engine: engine}}
	if err := c.beginApplicationResidency(engine); err != nil {
		t.Fatal(err)
	}
	if err := c.beginApplicationResidency(engine); err != nil {
		t.Fatal(err)
	}
	if got := engine.begins.Load(); got != 1 {
		t.Fatalf("residency begins = %d, want 1", got)
	}

	c.PrepareSleep()
	if got := engine.ends.Load(); got != 1 {
		t.Fatalf("residency ends after PrepareSleep = %d, want 1", got)
	}
	if err := c.beginApplicationResidency(engine); err != nil {
		t.Fatal(err)
	}
	c.PrepareUninterruptibleSleep()
	if err := c.beginApplicationResidency(engine); err != nil {
		t.Fatal(err)
	}
	c.PrepareStop()
	if err := c.beginApplicationResidency(engine); err != nil {
		t.Fatal(err)
	}
	c.PrepareExecve()
	if err := c.beginApplicationResidency(engine); err != nil {
		t.Fatal(err)
	}
	c.PrepareExit()
	if got := engine.begins.Load(); got != 5 {
		t.Fatalf("residency begins = %d, want 5", got)
	}
	if got := engine.ends.Load(); got != 5 {
		t.Fatalf("residency ends = %d, want 5", got)
	}

	// Every boundary is idempotent when no application vCPU is resident.
	c.PrepareExit()
	if got := engine.ends.Load(); got != 5 {
		t.Fatalf("idempotent residency ends = %d, want 5", got)
	}
}

func TestPlatformContextApplicationResidencyYieldsOnlyOnBackendRequest(t *testing.T) {
	engine := &yieldingResidencyEngine{}
	c := &platformContext{platform: &Platform{engine: engine}}
	if err := c.beginApplicationResidency(engine); err != nil {
		t.Fatal(err)
	}
	if err := c.yieldApplicationResidency(); err != nil {
		t.Fatal(err)
	}
	if got := engine.ends.Load(); got != 0 {
		t.Fatalf("residency ended without backend request: %d", got)
	}
	engine.yield.Store(true)
	if err := c.yieldApplicationResidency(); err != nil {
		t.Fatal(err)
	}
	if got := engine.ends.Load(); got != 1 {
		t.Fatalf("residency ends after backend request = %d, want 1", got)
	}
	if c.resident != nil {
		t.Fatal("platform context retained yielded residency")
	}
}

func TestPlatformContextApplicationResidencyRetainedOnlyForSyscalls(t *testing.T) {
	for _, test := range []struct {
		name   string
		reason ExitReason
		ends   uint32
	}{
		{name: "syscall", reason: ExitSyscall, ends: 0},
		{name: "fault", reason: ExitFault, ends: 1},
		{name: "exception", reason: ExitException, ends: 1},
		{name: "interrupt", reason: ExitInterrupted, ends: 1},
		{name: "shutdown", reason: ExitShutdown, ends: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := &yieldingResidencyEngine{}
			c := &platformContext{platform: &Platform{engine: engine}}
			if err := c.beginApplicationResidency(engine); err != nil {
				t.Fatal(err)
			}
			if err := c.completeApplicationExitResidency(test.reason); err != nil {
				t.Fatal(err)
			}
			if got := engine.ends.Load(); got != test.ends {
				t.Fatalf("residency ends = %d, want %d", got, test.ends)
			}
		})
	}
}

func TestPlatformContextRetainsFaultResidencyOnlyWithBackendCapacity(t *testing.T) {
	engine := &faultRetainingResidencyEngine{}
	engine.retain.Store(true)
	c := &platformContext{platform: &Platform{engine: engine}}
	if err := c.beginApplicationResidency(engine); err != nil {
		t.Fatal(err)
	}
	if err := c.completeApplicationExitResidency(ExitFault); err != nil {
		t.Fatal(err)
	}
	if got := engine.ends.Load(); got != 0 {
		t.Fatalf("retained fault residency ended %d times, want 0", got)
	}
	engine.retain.Store(false)
	if err := c.completeApplicationExitResidency(ExitFault); err != nil {
		t.Fatal(err)
	}
	if got := engine.ends.Load(); got != 1 {
		t.Fatalf("capacity-limited fault residency ended %d times, want 1", got)
	}
}
