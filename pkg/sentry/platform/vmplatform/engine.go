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
)

// MonitorExecutor delivers serialized page-table transactions to the EL1
// monitor. It is deliberately narrower than VMEngine so monitor construction
// and tests do not depend on application-continuation policy.
type MonitorExecutor interface {
	RunMonitor(context.Context, []byte) ([]byte, error)
}

// VMEngine owns the host-specific execution policy for one VM epoch.
//
// This is an internal boundary for platforms implemented using vmplatform,
// currently Darwin HVF and potentially future Windows WHPX. It is not the
// selectable Sentry-backend boundary: platform.Platform remains that boundary,
// and Linux KVM and systrap retain their independent implementations.
type VMEngine interface {
	MonitorExecutor
	RunApplication(context.Context, ApplicationState) (Exit, error)
	InterruptApplications()
	ConcurrencyCount() int
	Close() error
}

// ApplicationInterruptEngine is optionally implemented by engines whose
// application runs are owned by host threads. ApplicationOwner identifies the
// calling thread. InterruptApplication stops only the run owned by that
// thread, if any, and returns once the run has acknowledged or left the
// guest. Signals and preemption target one task; interrupting every run
// instead makes each running task end its residency.
type ApplicationInterruptEngine interface {
	VMEngine
	ApplicationOwner() uint64
	InterruptApplication(owner uint64)
}

// HostExecutionCacheEngine is optionally implemented by engines that execute
// Sentry code through a disposable mirror of host virtual memory. A quiesced
// state capture must discard mutable aliases: host allocators may recycle the
// backing pages while the capture allocates and decodes a second object graph.
// Engines that execute Sentry exclusively on the host do not need this hook.
type HostExecutionCacheEngine interface {
	VMEngine
	FlushHostExecutionCache(context.Context) error
}

// ApplicationResidencyEngine is implemented by VM engines whose application
// vCPU is owned by the host thread running the Sentry task. vmplatform keeps a
// residency across consecutive application entries and the intervening Linux
// syscall handling. Residency preserves the owner/vCPU association; it does not
// require the syscall handler itself to remain inside the VM. It ends immediately
// before the task blocks or changes execution identity.
//
// This is intentionally optional. Engines without owner-affine vCPUs retain
// the ordinary per-Switch lifecycle.
type ApplicationResidencyEngine interface {
	VMEngine
	BeginApplicationResidency()
	EndApplicationResidency() error
}

// ApplicationResidencyYieldEngine optionally asks vmplatform to end the
// current owner-thread residency after an application exit. Engines use this
// to provide fair vCPU handoff under contention without adding work to their
// uncontended syscall path. The decision and quantum remain backend policy;
// vmplatform only performs the owner-thread lifecycle operation.
type ApplicationResidencyYieldEngine interface {
	ApplicationResidencyEngine
	ShouldYieldApplicationResidency() bool
}

// ApplicationFaultResidencyEngine is optionally implemented by owner-affine
// engines that can retain a vCPU while Sentry resolves an application fault.
// Implementations must bound retained fault residencies so slow or remote
// fault handling cannot exhaust the execution pool.
type ApplicationFaultResidencyEngine interface {
	ApplicationResidencyEngine
	ShouldRetainApplicationFaultResidency() bool
}

// VMEngineFactory constructs the execution policy for a VM epoch. A fresh
// engine is constructed whenever the disposable VM epoch is rebuilt.
type VMEngineFactory func(VM, int) (VMEngine, error)
