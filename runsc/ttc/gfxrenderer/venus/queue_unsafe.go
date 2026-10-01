// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// queue is a queue of a device. Each that the guest binds to a ring's
// timeline signals that timeline's fences, as virglrenderer's queues do: a
// fence follows an empty submission with a VkFence, which the queue's
// goroutine waits for.
type queue struct {
	// obj is the queue's object, once the guest gets the queue.
	obj    *object
	dev    *device
	handle vn.Handle
	flags  uint32
	family uint32
	index  uint32
	// ringIdx is the ring whose timeline the queue's fences are on.
	ringIdx uint32

	// mu serializes the use of the queue, which Vulkan requires.
	mu sync.Mutex
	// pool holds the arguments of the renderer's own submissions, under mu.
	pool vn.Pool

	syncMu   sync.Mutex
	syncCond *sync.Cond
	// syncs are the submitted fences, in order.
	syncs []*queueSync
	// free are fences to reuse.
	free     []vn.Handle
	stopping bool
	done     chan struct{}
}

// queueSync is a fence of the Sentry's that a VkFence signals.
type queueSync struct {
	fence      vn.Handle
	deviceLost bool
	ctx        *Context
	ring       uint32
	fenceID    uint64
}

// newQueue gets a queue of dev, and starts its goroutine.
func (s *stream) newQueue(dev *device, flags, family, index uint32) *queue {
	q := &queue{dev: dev, flags: flags, family: family, index: index, done: make(chan struct{})}
	q.syncCond = sync.NewCond(&q.syncMu)
	out := alloc[vn.Handle](s, 1)
	if flags != 0 {
		info := newArgs[vn.VkDeviceQueueInfo2](s)
		info.SType = vn.VK_STRUCTURE_TYPE_DEVICE_QUEUE_INFO_2
		info.Flags = flags
		info.QueueFamilyIndex = family
		info.QueueIndex = index
		a := newArgs[vn.Args_vkGetDeviceQueue2](s)
		a.Device = dev.handle
		a.PQueueInfo = unsafe.Pointer(info)
		a.PQueue = unsafe.Pointer(out)
		vk.Call(vn.VK_COMMAND_TYPE_vkGetDeviceQueue2_EXT, dev.table, unsafe.Pointer(a))
	} else {
		// Some drivers forbid vkGetDeviceQueue2 without flags, as the spec
		// did before 1.1.130.
		a := newArgs[vn.Args_vkGetDeviceQueue](s)
		a.Device = dev.handle
		a.QueueFamilyIndex = family
		a.QueueIndex = index
		a.PQueue = unsafe.Pointer(out)
		vk.Call(vn.VK_COMMAND_TYPE_vkGetDeviceQueue_EXT, dev.table, unsafe.Pointer(a))
	}
	q.handle = *out
	go q.run()
	return q
}

// submitSync submits a fence of the Sentry's, which the queue signals once
// what was submitted to it is done.
func (q *queue) submitSync(c *Context, ring uint32, fenceID uint64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	defer q.pool.Reset()
	t := q.dev.table
	fence := q.takeFence()
	if fence == 0 {
		info := (*vn.VkFenceCreateInfo)(q.pool.Alloc(unsafe.Sizeof(vn.VkFenceCreateInfo{})))
		info.SType = vn.VK_STRUCTURE_TYPE_FENCE_CREATE_INFO
		a := (*vn.Args_vkCreateFence)(q.pool.Alloc(unsafe.Sizeof(vn.Args_vkCreateFence{})))
		out := (*vn.Handle)(q.pool.Alloc(8))
		a.Device = q.dev.handle
		a.PCreateInfo = unsafe.Pointer(info)
		a.PFence = unsafe.Pointer(out)
		vk.Call(vn.VK_COMMAND_TYPE_vkCreateFence_EXT, t, unsafe.Pointer(a))
		if a.Ret != vn.VK_SUCCESS {
			return fmt.Errorf("creating a fence: %d", a.Ret)
		}
		fence = *out
	} else {
		handles := (*vn.Handle)(q.pool.Alloc(8))
		*handles = fence
		a := (*vn.Args_vkResetFences)(q.pool.Alloc(unsafe.Sizeof(vn.Args_vkResetFences{})))
		a.Device = q.dev.handle
		a.FenceCount = 1
		a.PFences = unsafe.Pointer(handles)
		vk.Call(vn.VK_COMMAND_TYPE_vkResetFences_EXT, t, unsafe.Pointer(a))
	}
	a := (*vn.Args_vkQueueSubmit)(q.pool.Alloc(unsafe.Sizeof(vn.Args_vkQueueSubmit{})))
	a.Queue = q.handle
	a.Fence = fence
	vk.Call(vn.VK_COMMAND_TYPE_vkQueueSubmit_EXT, t, unsafe.Pointer(a))
	qs := &queueSync{fence: fence, ctx: c, ring: ring, fenceID: fenceID}
	switch {
	case a.Ret == vn.VK_ERROR_DEVICE_LOST:
		qs.deviceLost = true
	case a.Ret != vn.VK_SUCCESS:
		q.putFence(fence)
		return fmt.Errorf("submitting a fence: %d", a.Ret)
	}
	q.syncMu.Lock()
	q.syncs = append(q.syncs, qs)
	q.syncCond.Broadcast()
	q.syncMu.Unlock()
	return nil
}

func (q *queue) takeFence() vn.Handle {
	q.syncMu.Lock()
	defer q.syncMu.Unlock()
	if n := len(q.free); n != 0 {
		f := q.free[n-1]
		q.free = q.free[:n-1]
		return f
	}
	return 0
}

func (q *queue) putFence(f vn.Handle) {
	q.syncMu.Lock()
	q.free = append(q.free, f)
	q.syncMu.Unlock()
}

// run signals the queue's fences, in order, as they signal.
func (q *queue) run() {
	defer close(q.done)
	var pool vn.Pool
	defer pool.Release()
	a := (*vn.Args_vkWaitForFences)(pool.Alloc(unsafe.Sizeof(vn.Args_vkWaitForFences{})))
	fences := (*vn.Handle)(pool.Alloc(8))
	for {
		q.syncMu.Lock()
		for len(q.syncs) == 0 && !q.stopping {
			q.syncCond.Wait()
		}
		if q.stopping {
			q.syncMu.Unlock()
			return
		}
		qs := q.syncs[0]
		q.syncMu.Unlock()

		result := int32(vn.VK_ERROR_DEVICE_LOST)
		if !qs.deviceLost {
			*fences = qs.fence
			a.Device = q.dev.handle
			a.FenceCount = 1
			a.PFences = unsafe.Pointer(fences)
			a.WaitAll = 1
			a.Timeout = uint64(3 * time.Second)
			vk.Call(vn.VK_COMMAND_TYPE_vkWaitForFences_EXT, q.dev.table, unsafe.Pointer(a))
			result = a.Ret
		}
		if result == vn.VK_TIMEOUT {
			continue
		}
		q.syncMu.Lock()
		q.syncs = q.syncs[1:]
		q.free = append(q.free, qs.fence)
		q.syncMu.Unlock()
		qs.ctx.srv.fenceSignaled(qs.fenceID, qs.ctx.id, true, qs.ring)
	}
}

// stop stops the queue's goroutine, once its device is idle, and signals
// the fences left.
func (q *queue) stop() {
	q.syncMu.Lock()
	q.stopping = true
	q.syncCond.Broadcast()
	q.syncMu.Unlock()
	<-q.done
	q.syncMu.Lock()
	syncs := q.syncs
	q.syncs = nil
	for _, qs := range syncs {
		q.free = append(q.free, qs.fence)
	}
	q.syncMu.Unlock()
	for _, qs := range syncs {
		qs.ctx.srv.fenceSignaled(qs.fenceID, qs.ctx.id, true, qs.ring)
	}
}

// releaseFences destroys the queue's fences.
func (q *queue) releaseFences(s *stream) {
	q.syncMu.Lock()
	free := q.free
	q.free = nil
	q.syncMu.Unlock()
	for _, f := range free {
		a := newArgs[vn.Args_vkDestroyFence](s)
		a.Device = q.dev.handle
		a.Fence = f
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyFence_EXT, q.dev.table, unsafe.Pointer(a))
	}
	q.pool.Release()
}

func registerQueue() {
	// The guest's driver resolves sync_fd payloads itself: the renderer has
	// no external fences or semaphores to export.
	handle(vn.VK_COMMAND_TYPE_vkResetFenceResourceMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		s.fail("vkResetFenceResourceMESA: the renderer has no sync_fd fences")
	})
	handle(vn.VK_COMMAND_TYPE_vkWaitSemaphoreResourceMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		s.fail("vkWaitSemaphoreResourceMESA: the renderer has no sync_fd semaphores")
	})
	handle(vn.VK_COMMAND_TYPE_vkImportSemaphoreResourceMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		s.fail("vkImportSemaphoreResourceMESA: the renderer has no sync_fd semaphores")
	})
}
