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

//go:build darwin && arm64

// Package ring0asm contains the Go-ABI side of the Darwin HVF continuation
// bridge. It is separate from package hvf so cgo does not pass Go assembly to
// Clang as platform assembly.
package ring0asm

import "unsafe"

func bluepillTrap()

// Bluepill transfers the current continuation to the requested guest vCPU.
// It returns at EL1 and does not return to the host until EnsureHost is called
// or the monitor redirects a host-dependent exception.
func Bluepill(bridge unsafe.Pointer, stateVA uint64)

// SwitchApplicationGuest enters the selected application when the calling
// Sentry continuation is already resident on its owner vCPU at EL1.
func SwitchApplicationGuest()

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

// CurrentEL returns 0 at Darwin EL0 and 1 for a resident Sentry continuation
// at guest EL1. The EL0 MRS is handled as a controlled SIGILL before any vCPU
// is created or accessed.
func CurrentEL() uint64

// CurrentELTrapAddress returns the MRS instruction used by CurrentEL.
func CurrentELTrapAddress() uintptr
