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

//go:build darwin

package fsutil

import "golang.org/x/sys/unix"

func mmapFileAtExact(_ int, _, _ uint64, _ int) (uintptr, unix.Errno) {
	// Darwin has MAP_FIXED, which destructively replaces mappings, but no
	// MAP_FIXED_NOREPLACE. Exact-offset host mappings are a Linux platform
	// optimization and are not used by the HVF/GuestRAM data plane.
	return 0, unix.EOPNOTSUPP
}
