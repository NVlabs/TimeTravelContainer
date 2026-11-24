// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin && arm64

// Package hvf registers the portable VM platform over Apple
// Hypervisor.framework and the ARM64 sentry state adapter.
package hvf

import (
	"runtime"

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
	// Keep bounded owner-affine residency headroom above the configured core
	// count. Application EL0 execution now releases its Go P just as KVM does,
	// so dormant HVF residencies do not starve the Sentry scheduler. The extra
	// IDs avoid destroy/create churn across syscall-heavy live branches; the FIFO
	// admission handoff remains the fallback at the architected 63-vCPU limit.
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

func (*constructor) PrecompiledSeccompInfo() []sentryplatform.SeccompInfo { return nil }

func init() {
	sentryplatform.Register("hvf", &constructor{})
}
