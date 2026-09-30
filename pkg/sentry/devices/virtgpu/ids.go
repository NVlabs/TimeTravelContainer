// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package virtgpu

import (
	"container/heap"
)

// idAllocator allocates the lowest free ID from 1, as Linux's IDAs and IDRs
// do for context IDs and GEM handles.
//
// +stateify savable
type idAllocator struct {
	// next is the lowest ID that was never allocated.
	next uint32
	// free is a min-heap of released IDs below next.
	free idHeap
	// max is the largest ID to allocate.
	max uint32
}

// alloc returns the lowest free ID, or false if every ID up to max is in use.
func (a *idAllocator) alloc() (uint32, bool) {
	if len(a.free) > 0 {
		return heap.Pop(&a.free).(uint32), true
	}
	if a.next == 0 {
		a.next = 1
	}
	if a.next > a.max {
		return 0, false
	}
	id := a.next
	a.next++
	return id, true
}

// release frees an ID that alloc returned.
func (a *idAllocator) release(id uint32) {
	if id+1 == a.next {
		a.next--
		return
	}
	heap.Push(&a.free, id)
}

// idHeap is a min-heap of IDs.
type idHeap []uint32

func (h idHeap) Len() int           { return len(h) }
func (h idHeap) Less(i, j int) bool { return h[i] < h[j] }
func (h idHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }

// Push implements heap.Interface.Push.
func (h *idHeap) Push(x any) { *h = append(*h, x.(uint32)) }

// Pop implements heap.Interface.Pop.
func (h *idHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
