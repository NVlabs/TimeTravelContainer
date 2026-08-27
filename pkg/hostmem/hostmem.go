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

// Package hostmem describes the host's virtual-memory allocation geometry.
//
// hostarch.PageSize is the page size of the Linux ABI implemented by the
// sentry. It must not be used to align host mmap, munmap, or VM operations on
// hosts whose allocation granule differs from Linux's page size (notably
// Apple Silicon Darwin, where it is 16 KiB).
package hostmem

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

// AllocationGranule returns the alignment required by host virtual-memory
// allocation and deallocation operations.
func AllocationGranule() uint64 {
	return uint64(unix.Getpagesize())
}

// IsAllocationAligned reports whether value is aligned to the host allocation
// granule.
func IsAllocationAligned(value uint64) bool {
	return value%AllocationGranule() == 0
}

// AllocationRoundDown rounds value down to the host allocation granule.
func AllocationRoundDown(value uint64) uint64 {
	granule := AllocationGranule()
	return value &^ (granule - 1)
}

// AllocationRoundUp rounds value up to the host allocation granule. It
// returns an error if rounding would overflow uint64.
func AllocationRoundUp(value uint64) (uint64, error) {
	granule := AllocationGranule()
	if granule == 0 || granule&(granule-1) != 0 {
		return 0, fmt.Errorf("host allocation granule %d is not a power of two", granule)
	}
	if value > math.MaxUint64-(granule-1) {
		return 0, fmt.Errorf("rounding %#x to host allocation granule %d overflows", value, granule)
	}
	return (value + granule - 1) &^ (granule - 1), nil
}

// ValidateAllocationRange verifies the alignment requirements common to host
// VM map and unmap operations.
func ValidateAllocationRange(addr, length uint64) error {
	if length == 0 {
		return fmt.Errorf("host allocation range has zero length")
	}
	if !IsAllocationAligned(addr) {
		return fmt.Errorf("host address %#x is not aligned to allocation granule %d", addr, AllocationGranule())
	}
	if !IsAllocationAligned(length) {
		return fmt.Errorf("host length %#x is not aligned to allocation granule %d", length, AllocationGranule())
	}
	if addr > math.MaxUint64-length {
		return fmt.Errorf("host allocation range [%#x, %#x) overflows", addr, length)
	}
	return nil
}
