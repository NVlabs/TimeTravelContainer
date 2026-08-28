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

// Package hvf registers the portable VM platform over Apple
// Hypervisor.framework and the ARM64 sentry state adapter.
package hvf

import (
	"fmt"
	"runtime"

	"gvisor.dev/gvisor/pkg/fd"
	"gvisor.dev/gvisor/pkg/hostarch"
	sentryplatform "gvisor.dev/gvisor/pkg/sentry/platform"
	hvfbackend "gvisor.dev/gvisor/pkg/sentry/platform/hvf"
	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform"
	vmarm64 "gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/arm64"
)

const (
	hvfIPASize        = uint64(1 << 40)
	hvfVCPUOvercommit = 3
)

type constructor struct{}

func hvfApplicationVCPUs(applicationCores int) int {
	vcpus := hvfbackend.MaxApplicationVCPUs
	if applicationCores <= hvfbackend.MaxApplicationVCPUs/hvfVCPUOvercommit {
		vcpus = applicationCores * hvfVCPUOvercommit
	}
	return vcpus
}

func (*constructor) New(opts sentryplatform.Options) (sentryplatform.Platform, error) {
	backend := hvfbackend.NewBackend()
	applicationCores := opts.ApplicationCores
	if applicationCores <= 0 {
		applicationCores = runtime.NumCPU()
	}
	// Match KVM's application-vCPU admission headroom. HVF vCPUs are
	// owner-thread affine and therefore cannot be stolen by a waiting Sentry
	// task; extra lazily-created IDs keep syscall-heavy tasks from monopolizing
	// every execution slot and delaying exec or fork control work.
	vcpus := hvfApplicationVCPUs(applicationCores)
	config := vmplatform.Config{
		NewVM:           backend.CreateVM,
		HostMemory:      backend.HostMemory(),
		Architecture:    vmarm64.Architecture{},
		VCPUs:           vcpus,
		IPAStart:        hvfbackend.GuestRAMIPAStart,
		IPASize:         hvfIPASize - hvfbackend.GuestRAMIPAStart,
		MaxUser:         hostarch.Addr(1 << 48),
		PlatformName:    "hvf",
		VMEngineFactory: hvfbackend.NewVMEngine,
	}
	return vmplatform.New(config)
}

func (*constructor) OpenDevice(devicePath string) (*fd.FD, error) {
	if devicePath != "" {
		return nil, fmt.Errorf("HVF does not use a platform device path")
	}
	return nil, nil
}

func (*constructor) Requirements() sentryplatform.Requirements { return sentryplatform.Requirements{} }

func (*constructor) PrecompiledSeccompInfo() []sentryplatform.SeccompInfo { return nil }

func init() {
	sentryplatform.Register("hvf", &constructor{})
}
