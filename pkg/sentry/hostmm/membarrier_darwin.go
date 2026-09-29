// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

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
