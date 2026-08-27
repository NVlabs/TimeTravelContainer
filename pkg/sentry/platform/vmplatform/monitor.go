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

	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/monitorabi"
)

// Monitor serializes all v1 page-table transactions and executes them through
// the selected VM execution engine.
type Monitor struct {
	executor MonitorExecutor
	expander MonitorPoolExpander

	mu       sync.Mutex
	sequence uint64
}

// NewMonitor constructs a backend-independent monitor client. executor owns
// how a monitor command reaches EL1; expander owns only the host stage-2 lease
// required when the monitor's page-table pool grows.
func NewMonitor(executor MonitorExecutor, expander MonitorPoolExpander) *Monitor {
	return &Monitor{executor: executor, expander: expander}
}

// Execute applies a transaction and returns only after the monitor's
// synchronous TLB acknowledgement.
func (m *Monitor) Execute(ctx context.Context, command monitorabi.Command) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Once admitted, a monitor mutation is non-cancelable: callers must never
	// observe an error while the EL1 side may already have changed page tables.
	executionCtx := context.WithoutCancel(ctx)
	for attempts := 0; ; attempts++ {
		response, err := m.executeLocked(executionCtx, command)
		if err != nil {
			return err
		}
		if response.Status == monitorabi.StatusOK {
			return nil
		}
		if response.Status != monitorabi.StatusNoMemory || attempts >= 64 {
			return fmt.Errorf("monitor transaction %d failed with status %d detail %d", response.Sequence, response.Status, response.Detail)
		}
		if err := m.expandPoolLocked(executionCtx); err != nil {
			return fmt.Errorf("expanding monitor page-table pool: %w", err)
		}
	}
}

func (m *Monitor) executeLocked(ctx context.Context, command monitorabi.Command) (monitorabi.Response, error) {
	m.sequence++
	if m.sequence == 0 {
		m.sequence++
	}
	command.Sequence = m.sequence
	encoded, err := monitorabi.MarshalCommand(command)
	if err != nil {
		return monitorabi.Response{}, err
	}
	encodedResponse, err := m.executor.RunMonitor(ctx, encoded)
	if err != nil {
		return monitorabi.Response{}, err
	}
	response, err := monitorabi.UnmarshalResponse(encodedResponse)
	if err != nil {
		return monitorabi.Response{}, err
	}
	if response.Sequence != command.Sequence {
		return monitorabi.Response{}, fmt.Errorf("monitor acknowledged sequence %d, want %d", response.Sequence, command.Sequence)
	}
	return response, nil
}

func (m *Monitor) expandPoolLocked(ctx context.Context) error {
	expander := m.expander
	if expander == nil {
		return fmt.Errorf("VM backend does not support monitor pool expansion")
	}
	expansion, err := expander.PrepareMonitorPoolExpansion(ctx)
	if err != nil {
		return err
	}
	accepted := false
	defer func() {
		if !accepted {
			_ = expander.CompleteMonitorPoolExpansion(expansion, false)
		}
	}()
	response, err := m.executeLocked(ctx, monitorabi.Command{
		Opcode:     monitorabi.OpcodeExpandPageTablePool,
		PoolGPA:    expansion.GPA,
		PoolLength: expansion.Length,
	})
	if err != nil {
		return err
	}
	if response.Status != monitorabi.StatusOK {
		return fmt.Errorf("monitor pool transaction %d failed with status %d detail %d", response.Sequence, response.Status, response.Detail)
	}
	if err := expander.CompleteMonitorPoolExpansion(expansion, true); err != nil {
		return err
	}
	accepted = true
	return nil
}
