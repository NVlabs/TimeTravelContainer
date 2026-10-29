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

//go:build darwin

// Package filter defines the gofer's host syscall confinement policy.
package filter

import (
	"gvisor.dev/gvisor/pkg/seccomp"
	"gvisor.dev/gvisor/pkg/seccomp/precompiledseccomp"
)

// Options are retained across hosts so the gofer lifecycle remains shared.
type Options struct {
	UDSOpenEnabled   bool
	UDSCreateEnabled bool
	ProfileEnabled   bool
	DirectFS         bool
	LisafsNeeded     bool
	CgoEnabled       bool
	VendorSyscalls   precompiledseccomp.VarSyscallNumbers
	ExtraRules       []seccomp.SyscallRules
}

// HostVendorSyscalls returns no vendor-specific syscalls: Darwin has no Linux
// seccomp filter to allow them in.
func HostVendorSyscalls() precompiledseccomp.VarSyscallNumbers {
	return nil
}

// Rules returns no Linux seccomp rules on Darwin.
func Rules(_ Options) seccomp.SyscallRules {
	return seccomp.NewSyscallRules()
}

// Install is a no-op because Darwin does not implement Linux seccomp. The
// macOS backend is admitted only in explicitly requested trusted mode and
// makes no host-security-boundary claim.
func Install(_ Options) error {
	return nil
}
