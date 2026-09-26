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

//go:build darwin && arm64 && cgo

package safecopy

import (
	"fmt"

	"gvisor.dev/gvisor/pkg/safecopy/machexc"
)

func initializePlatform() error {
	ranges := []struct {
		begin   uintptr
		end     uintptr
		handler uintptr
	}{
		{memcpyBegin, memcpyEnd, addrOfHandleMemcpyFault()},
		{memclrBegin, memclrEnd, addrOfHandleMemclrFault()},
		{swapUint32Begin, swapUint32End, addrOfHandleSwapUint32Fault()},
		{swapUint64Begin, swapUint64End, addrOfHandleSwapUint64Fault()},
		{compareAndSwapUint32Begin, compareAndSwapUint32End, addrOfHandleCompareAndSwapUint32Fault()},
		{loadUint32Begin, loadUint32End, addrOfHandleLoadUint32Fault()},
	}
	for _, faultRange := range ranges {
		if err := machexc.AddFaultRange(faultRange.begin, faultRange.end, faultRange.handler); err != nil {
			return fmt.Errorf("registering fault range [%#x, %#x): %w", faultRange.begin, faultRange.end, err)
		}
	}
	if err := machexc.Install(); err != nil {
		return fmt.Errorf("installing Mach exception handler: %w", err)
	}
	return nil
}

func addrOfHandleMemcpyFault() uintptr
func addrOfHandleMemclrFault() uintptr
func addrOfHandleSwapUint32Fault() uintptr
func addrOfHandleSwapUint64Fault() uintptr
func addrOfHandleCompareAndSwapUint32Fault() uintptr
func addrOfHandleLoadUint32Fault() uintptr
