// Copyright 2023 The gVisor Authors.
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

//go:build amd64
// +build amd64

package linux

// detectTaskSize discovers the exact contiguous address-space ceiling. Native
// amd64 Linux normally exposes one of the 4-level or 5-level paging limits,
// but a nested systrap Sentry intentionally reserves a high stub band and
// therefore exposes a non-standard, page-aligned ceiling to its children.
func detectTaskSize(pageSize uintptr) uintptr {
	const (
		maximum = uintptr(0xfffffffffff000)
		minimum = uintptr(0xfffff000)
	)
	if taskSizeUsable(maximum, pageSize) {
		return maximum
	}
	if !taskSizeUsable(minimum, pageSize) {
		panic("address space is smaller than the minimum supported task size")
	}

	// Binary-search page numbers. low is usable and high is not.
	low := minimum / pageSize
	high := maximum / pageSize
	for low+1 < high {
		mid := low + (high-low)/2
		if taskSizeUsable(mid*pageSize, pageSize) {
			low = mid
		} else {
			high = mid
		}
	}
	return low * pageSize
}

// Page fault error codes
const (
	X86_PF_PROT = 1 << iota
	X86_PF_WRITE
	X86_PF_USER
	X86_PF_RSVD
	X86_PF_INSTR
)
