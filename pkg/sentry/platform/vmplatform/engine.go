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

// VMEngineFactory constructs the execution policy for a VM epoch. A fresh
// engine is constructed whenever the disposable VM epoch is rebuilt.
type VMEngineFactory func(VM, int) (VMEngine, error)
