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

package hvf

import (
	"context"
	"fmt"

	"gvisor.dev/gvisor/pkg/sentry/platform/vmplatform/vmm"
)

// Backend is unavailable outside Apple Silicon Darwin.
type Backend struct{}

// GuestRAMIPAStart is retained in pure-Go build-host configurations so that
// platform registration code compiles. Runnable Darwin sentries use the cgo
// backend and the monitor's identical private-range boundary.
const GuestRAMIPAStart = uint64(0x10000000)

func NewBackend() *Backend { return &Backend{} }
func (*Backend) CreateVM(context.Context) (vmm.VM, error) {
	return nil, fmt.Errorf("Hypervisor.framework is supported only on Apple Silicon Darwin")
}
func (*Backend) HostMemory() vmm.HostMemory { return nil }
