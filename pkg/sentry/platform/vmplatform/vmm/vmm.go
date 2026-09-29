// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package vmm defines the dependency-light host hypervisor boundary.
package vmm

import "context"

const (
	LogicalPageSize uint64 = 4096
	HostSlabSize    uint64 = 16384
)

type Stage2Access uint8

const (
	Stage2Read Stage2Access = 1 << iota
	Stage2Write
	Stage2Execute
)

type Backend interface {
	CreateVM(context.Context) (VM, error)
}

type VM interface {
	MapStage2(gpa uint64, hostAddress uintptr, length uint64, access Stage2Access) error
	UnmapStage2(gpa, length uint64) error
	CreateVCPU(id int) (VCPU, error)
	Close() error
}

// MonitorPoolExpansion describes one permanently leased stage-2 range that
// may be admitted to the monitor's private page-table allocator. It is an
// execution cache and is never checkpointed.
type MonitorPoolExpansion struct {
	GPA    uint64
	Length uint64
}

// MonitorPoolExpander is an optional VM capability used when the monitor's
// page-table allocator is exhausted. Prepare must make the stage-2 range
// accessible but not release it; Complete commits ownership to the monitor or
// rolls the preparation back after the synchronous monitor acknowledgement.
type MonitorPoolExpander interface {
	PrepareMonitorPoolExpansion(context.Context) (MonitorPoolExpansion, error)
	CompleteMonitorPoolExpansion(MonitorPoolExpansion, bool) error
}

type VCPU interface {
	RunMonitor(context.Context, []byte) ([]byte, error)
	RequestExit() error
	Close() error
}

// ApplicationState is the architecture-neutral envelope passed to a
// vmplatform VMEngine. Registers must be one of the architecture state types
// supported by the backend. Neither field is checkpointed as platform state;
// the sentry's arch.Context64 remains authoritative.
type ApplicationState struct {
	AddressSpaceID uint64
	Registers      any
}

// ARM64State is the portable register transport between the common platform
// adapter and an ARM64 VMM backend. FP contains fpsr, fpcr, then Q0-Q31, which
// matches bytes [8:528] of Linux's fpsimd_context representation.
type ARM64State struct {
	X      [31]uint64
	SP     uint64
	PC     uint64
	PState uint64
	TPIDR  uint64
	FP     [520]byte
}

type ExitReason uint8

const (
	ExitUnknown ExitReason = iota
	ExitSyscall
	ExitFault
	// ExitException is a synchronous architectural exception that is not a
	// stage-1 memory abort, such as an AArch64 breakpoint, software step, or
	// undefined instruction. Architecture.DecodeExit converts its syndrome to
	// the Linux signal ABI.
	ExitException
	ExitInterrupted
	ExitShutdown
)

type Exit struct {
	Reason   ExitReason
	Syndrome uint64
	Address  uint64
	Access   Stage2Access
}

// HostMemory controls physical commitment behind stable virtual-address
// reservations at the host allocation granule.
type HostMemory interface {
	Reserve(length uint64) (uintptr, error)
	Commit(address uintptr, length uint64) error
	Decommit(address uintptr, length uint64) error
	Zero(address uintptr, length uint64) error
	Copy(dst, src uintptr, length uint64) error
	Release(address uintptr, length uint64) error
}

// HostRange is a range of host virtual addresses.
type HostRange struct {
	Address uintptr
	Length  uint64
}

// HostRangeReleaser is implemented by HostMemory that can decommit and release
// many reservations together much more cheaply than one at a time.
type HostRangeReleaser interface {
	// ReleaseRanges decommits and releases every range. If it returns an
	// error, it has released none of them.
	ReleaseRanges(ranges []HostRange) error
}
