// Copyright 2018 The gVisor Authors.
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

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package platformtest provides a platform for tests of the Sentry that don't
// run application code, for hosts where no platform that can is available to
// an unprivileged test: on macOS, only a binary signed with the hypervisor
// entitlement can create a Hypervisor.framework VM.
package platformtest

import (
	"errors"
	"math"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/arch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/platform"
	"gvisor.dev/gvisor/pkg/sentry/platform/platformdesc"
)

// Name is the name of the platform in the platform registry.
const Name = "platformtest"

// errNoApplicationCode is the error of an attempt to run application code.
var errNoApplicationCode = errors.New("the test platform can't run application code")

// Platform is a platform.Platform whose address spaces map nothing and whose
// contexts can't run application code. The Sentry accesses application memory
// through its internal mappings.
type Platform struct {
	platform.MMapMinAddr
	platform.NoCPUPreemptionDetection
	platform.NoCPUNumbers
}

// New returns a Platform.
func New() *Platform {
	return &Platform{}
}

// SupportsAddressSpaceIO implements platform.Platform.SupportsAddressSpaceIO.
func (*Platform) SupportsAddressSpaceIO() bool {
	return false
}

// HaveGlobalMemoryBarrier implements platform.Platform.HaveGlobalMemoryBarrier.
func (*Platform) HaveGlobalMemoryBarrier() bool {
	return false
}

// GlobalMemoryBarrier implements platform.Platform.GlobalMemoryBarrier.
func (*Platform) GlobalMemoryBarrier() error {
	return errors.New("the test platform has no global memory barrier")
}

// MapUnit implements platform.Platform.MapUnit.
func (*Platform) MapUnit() uint64 {
	return 0
}

// MaxUserAddress implements platform.Platform.MaxUserAddress.
func (*Platform) MaxUserAddress() hostarch.Addr {
	return hostarch.Addr(1 << 47)
}

// NewAddressSpace implements platform.Platform.NewAddressSpace.
func (*Platform) NewAddressSpace() (platform.AddressSpace, error) {
	return &addressSpace{}, nil
}

// NewContext implements platform.Platform.NewContext.
func (*Platform) NewContext(context.Context) platform.Context {
	return &platformContext{}
}

// SeccompInfo implements platform.Platform.SeccompInfo.
func (*Platform) SeccompInfo() platform.SeccompInfo {
	return platform.StaticSeccompInfo{PlatformName: Name}
}

// ConcurrencyCount implements platform.Platform.ConcurrencyCount.
func (*Platform) ConcurrencyCount() int {
	return math.MaxInt
}

// addressSpace is a platform.AddressSpace that maps nothing. Each is an
// object of its own, as a platform's address spaces are, which the Kernel
// tells apart by identity.
type addressSpace struct {
	platform.NoAddressSpaceIO

	// _ makes addressSpace of nonzero size: pointers to distinct values of
	// zero size may be equal.
	_ byte
}

// MapFile implements platform.AddressSpace.MapFile.
func (*addressSpace) MapFile(hostarch.Addr, memmap.File, memmap.FileRange, hostarch.AccessType, bool) error {
	return nil
}

// Unmap implements platform.AddressSpace.Unmap.
func (*addressSpace) Unmap(hostarch.Addr, uint64) {}

// Release implements platform.AddressSpace.Release.
func (*addressSpace) Release() {}

// PreFork implements platform.AddressSpace.PreFork.
func (*addressSpace) PreFork() {}

// PostFork implements platform.AddressSpace.PostFork.
func (*addressSpace) PostFork() {}

// platformContext is a platform.Context that can't run application code.
type platformContext struct {
	platform.NoCPUNumbers
}

// Switch implements platform.Context.Switch.
func (*platformContext) Switch(context.Context, platform.MemoryManager, *arch.Context64, int32) (*linux.SignalInfo, hostarch.AccessType, error) {
	return nil, hostarch.NoAccess, errNoApplicationCode
}

// PullFullState implements platform.Context.PullFullState.
func (*platformContext) PullFullState(platform.AddressSpace, *arch.Context64) error {
	return nil
}

// FullStateChanged implements platform.Context.FullStateChanged.
func (*platformContext) FullStateChanged() {}

// Interrupt implements platform.Context.Interrupt.
func (*platformContext) Interrupt() {}

// Preempt implements platform.Context.Preempt.
func (*platformContext) Preempt() {}

// Release implements platform.Context.Release.
func (*platformContext) Release() {}

// PrepareSleep implements platform.Context.PrepareSleep.
func (*platformContext) PrepareSleep() {}

// PrepareUninterruptibleSleep implements
// platform.Context.PrepareUninterruptibleSleep.
func (*platformContext) PrepareUninterruptibleSleep() {}

// PrepareStop implements platform.Context.PrepareStop.
func (*platformContext) PrepareStop() {}

// PrepareExecve implements platform.Context.PrepareExecve.
func (*platformContext) PrepareExecve() {}

// PrepareExit implements platform.Context.PrepareExit.
func (*platformContext) PrepareExit() {}

type constructor struct{}

// New implements platform.Constructor.New.
func (*constructor) New(platform.Options) (platform.Platform, error) {
	return New(), nil
}

// PrecompiledSeccompInfo implements platform.Constructor.PrecompiledSeccompInfo.
func (*constructor) PrecompiledSeccompInfo() []platform.SeccompInfo {
	return nil
}

func init() {
	platform.Register(Name, &constructor{})
	platformdesc.Register(Name, platformdesc.Desc{})
}
