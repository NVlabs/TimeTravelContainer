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

//go:build !darwin || !arm64 || !cgo

// Package hvf adapts Apple Hypervisor.framework to vmplatform. Outside Apple
// Silicon Darwin with cgo, it only reports that the platform is unavailable.
package hvf

import (
	"context"
	"fmt"

	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

// Backend is unavailable outside Apple Silicon Darwin.
type Backend struct{}

// SentryMirrorIPAStart and GuestRAMIPAStart retain the VM aperture contract in
// pure-Go analysis builds. Runnable Darwin sentries use the cgo definitions.
const SentryMirrorIPAStart = uint64(0x10000000)

const GuestRAMIPAStart = uint64(1 << 39)

// MaxApplicationVCPUs retains the owner-affine vCPU aperture contract in
// pure-Go analysis builds.
const MaxApplicationVCPUs = 63

func NewBackend() *Backend { return &Backend{} }

// NewVMEngine retains the engine-factory surface for analysis and pure-Go
// build configurations. A runnable Darwin binary must enable cgo so the real
// Hypervisor.framework implementation replaces this explicit failure.
func NewVMEngine(vmm.VM, int) (vmplatform.VMEngine, error) {
	return nil, fmt.Errorf("the HVF ring0 engine requires cgo on Apple Silicon Darwin")
}

func (*Backend) CreateVM(context.Context) (vmm.VM, error) {
	return nil, fmt.Errorf("the Hypervisor.framework backend is supported only on Apple Silicon Darwin")
}
func (*Backend) HostMemory() vmm.HostMemory { return nil }
