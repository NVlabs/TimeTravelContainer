// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
type HostRange = vmm.HostRange
type HostRangeReleaser = vmm.HostRangeReleaser
type SharedStage2Mapper = vmm.SharedStage2Mapper
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
