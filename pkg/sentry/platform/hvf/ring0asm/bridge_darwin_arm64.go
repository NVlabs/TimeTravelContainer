// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64

// Package ring0asm contains the Go-ABI side of the Darwin HVF continuation
// bridge. It is separate from package hvf so cgo does not pass Go assembly to
// Clang as platform assembly.
package ring0asm

import (
	"reflect"
	"unsafe"
)

func bluepillTrap()

// Bluepill transfers the current continuation to the requested guest vCPU.
// It returns at EL1 and does not return to the host until EnsureHost is called
// or the monitor redirects a host-dependent exception.
func Bluepill(bridge unsafe.Pointer, stateVA uint64)

// SwitchApplicationGuest enters the selected application when the calling
// Sentry continuation is already resident on its owner vCPU at EL1.
func SwitchApplicationGuest()

// RunMonitorGuest executes the monitor's returning command entry on the
// current resident EL1 vCPU. The caller must serialize monitor commands,
// exclude other application readers, and publish the guest-critical marker.
func RunMonitorGuest(monitorEntry, stateVA uint64)

// SyncGuestCode asks the native bridge to perform Darwin's writer-side cache
// maintenance for a GuestRAM HVA, then resumes this EL1 continuation in place.
// It returns zero on success and non-zero if the native bridge rejected the
// range without exporting the guest-critical continuation.
func SyncGuestCode(address uintptr, length uint64) uint64

// SyncGuestCodeRanges performs a sequence of cache-maintenance HVCs without
// returning through Go between ranges. ranges points to count consecutive
// {uintptr address, uint64 length} records. This must be used after publishing
// guest-critical state so the language runtime cannot host-gate between the
// final CurrentEL check and a later HVC.
func SyncGuestCodeRanges(ranges unsafe.Pointer, count uint64) uint64

// Yield emits the ARM64 processor-yield hint without entering the Go
// scheduler. It is used only for bounded guest-resident lock acquisition.
func Yield()

// SwitchApplicationGuestAddress returns the first instruction of the
// already-resident application-entry trampoline.
func SwitchApplicationGuestAddress() uintptr

// SwitchApplicationGuestTrapAddress returns the exact TPIDR_EL1 instruction
// in the already-resident application-entry trampoline. A translation
// mutation can redpill the continuation immediately before this instruction;
// the host SIGILL bridge converts that race to an interrupted application
// entry.
func SwitchApplicationGuestTrapAddress() uintptr

// EnsureHost returns a guest EL1 continuation to host EL0. It is a no-op when
// already running at host EL0.
func EnsureHost()

// Probe transfers the current continuation to EL1 and returns the observed
// exception level after deliberately returning to the host.
func Probe(bridge unsafe.Pointer, stateVA uint64) uint64

// ApplicationProbe transfers to EL1, enters an application through the
// monitor entry, and returns the exception level observed after the EL0
// exception restores the Sentry continuation.
func ApplicationProbe(bridge unsafe.Pointer, stateVA, monitorEntry uint64) uint64

// HostSVCGetpid executes Darwin's getpid SVC directly. It is used to qualify
// the current-EL SVC host-service gate independently of an external libSystem
// instruction fault.
func HostSVCGetpid() uint64

// BluepillTrapAddress returns the exact instruction address that raises the
// controlled host SIGILL.
func BluepillTrapAddress() uintptr

// ProbeAddress returns an address within Probe for identity mapping.
func ProbeAddress() uintptr

// ApplicationProbeAddress returns an address within ApplicationProbe for
// identity mapping.
func ApplicationProbeAddress() uintptr

// HostSVCGetpidAddress returns an address within HostSVCGetpid.
func HostSVCGetpidAddress() uintptr

// BluepillAddress returns an address within Bluepill for identity mapping.
func BluepillAddress() uintptr

// EnsureHostTrapAddress returns the exact HVC instruction address used by
// EnsureHost. At host EL0, the SIGILL bridge skips this instruction.
func EnsureHostTrapAddress() uintptr

// CurrentSP returns the current Go stack pointer.
func CurrentSP() uintptr

// ProbeStack writes the 512 bytes of stack below its caller's frame.
func ProbeStack()

// CurrentEL returns 0 at Darwin EL0 and 1 for a resident Sentry continuation
// at guest EL1. The EL0 MRS is handled as a controlled SIGILL before any vCPU
// is created or accessed.
func CurrentEL() uint64

// CurrentELTrapAddress returns the MRS instruction used by CurrentEL.
func CurrentELTrapAddress() uintptr

// guestCodeBodyCount is the number of entries guestCodeBodies stores.
const guestCodeBodyCount = 13

func guestCodeBodies(bodies *[guestCodeBodyCount]uintptr)

// GuestCode returns the entry of every function in this package that may
// execute at EL1, including the ABI wrappers through which Go calls the
// assembly. Several of them execute EL1-only instructions, so the Sentry
// mirror must contain all of them before any EL1 entry.
func GuestCode() []uintptr {
	var bodies [guestCodeBodyCount]uintptr
	guestCodeBodies(&bodies)
	code := bodies[:]
	for _, function := range []any{
		Bluepill, SwitchApplicationGuest, RunMonitorGuest, SyncGuestCode,
		SyncGuestCodeRanges, Yield, EnsureHost, Probe, ApplicationProbe,
		HostSVCGetpid, CurrentSP, CurrentEL,
	} {
		code = append(code, reflect.ValueOf(function).Pointer())
	}
	return code
}
