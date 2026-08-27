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
)

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
