// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package dmaheap

import "testing"

// ioc encodes an ioctl request as Linux's include/uapi/asm-generic/ioctl.h
// does.
func ioc(dir, typ, nr, size uint32) uint32 {
	return dir<<30 | size<<16 | typ<<8 | nr
}

func TestUAPINumbers(t *testing.T) {
	const write, read = 1, 2
	for _, test := range []struct {
		name      string
		got, want uint32
	}{
		{"DMA_HEAP_IOCTL_ALLOC", ioctlHeapAlloc, ioc(read|write, 'H', 0, allocationDataSize)},
		{"DMA_BUF_IOCTL_SYNC", ioctlSync, ioc(write, 'b', 0, 8)},
		{"DMA_BUF_SET_NAME_A", ioctlSetNameA, ioc(write, 'b', 1, 4)},
		{"DMA_BUF_SET_NAME_B", ioctlSetNameB, ioc(write, 'b', 1, 8)},
	} {
		if test.got != test.want {
			t.Errorf("%s = %#x, want %#x", test.name, test.got, test.want)
		}
	}
}
