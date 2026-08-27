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

package hostmm

// HostMemBarrier reports no host membarrier capability on Darwin. VM
// platforms synchronize guest translations through the EL1 monitor instead.
type HostMemBarrier struct{}

func (HostMemBarrier) HaveGlobalMemoryBarrier() bool  { return false }
func (HostMemBarrier) HaveProcessMemoryBarrier() bool { return false }
func (HostMemBarrier) GlobalMemoryBarrier() error {
	panic("hostmm: GlobalMemoryBarrier is unavailable on Darwin")
}
func (HostMemBarrier) ProcessMemoryBarrier() error {
	panic("hostmm: ProcessMemoryBarrier is unavailable on Darwin")
}

func Probe(bool) <-chan HostMemBarrier {
	ch := make(chan HostMemBarrier, 1)
	ch <- HostMemBarrier{}
	close(ch)
	return ch
}
