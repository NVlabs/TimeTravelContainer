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
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

type Stage2Access = vmm.Stage2Access

const (
	Stage2Read    = vmm.Stage2Read
	Stage2Write   = vmm.Stage2Write
	Stage2Execute = vmm.Stage2Execute
)

type Backend = vmm.Backend
type VM = vmm.VM
type VCPU = vmm.VCPU
type ExitReason = vmm.ExitReason
type Exit = vmm.Exit
type HostMemory = vmm.HostMemory
type MonitorPoolExpansion = vmm.MonitorPoolExpansion
type MonitorPoolExpander = vmm.MonitorPoolExpander

func monitorPoolExpander(vm VM) MonitorPoolExpander {
	expander, _ := vm.(MonitorPoolExpander)
	return expander
}

const (
	ExitUnknown     = vmm.ExitUnknown
	ExitSyscall     = vmm.ExitSyscall
	ExitFault       = vmm.ExitFault
	ExitException   = vmm.ExitException
	ExitInterrupted = vmm.ExitInterrupted
	ExitShutdown    = vmm.ExitShutdown
)

type ApplicationState = vmm.ApplicationState

type Architecture interface {
	NewVCPUState() any
	LoadApplicationState(dst any, src *arch.Context64) error
	SaveApplicationState(dst *arch.Context64, src any, full bool) error
	DecodeExit(Exit) (*linux.SignalInfo, hostarch.AccessType, error)
}
