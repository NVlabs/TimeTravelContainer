// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package virtgpu

import (
	"fmt"
	"time"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/waiter"
)

// waitTimeout is how long DRM_IOCTL_VIRTGPU_WAIT waits, as in Linux.
const waitTimeout = 15 * time.Second

// timelineKey identifies a timeline: a context's ring, or the device's global
// timeline.
//
// +stateify savable
type timelineKey struct {
	global bool
	ctxID  uint32
	ring   uint32
}

// timelineOf returns the key of f's timeline.
func timelineOf(f Fence) timelineKey {
	if !f.HasRing {
		return timelineKey{global: true}
	}
	return timelineKey{ctxID: f.CtxID, ring: f.Ring}
}

// String returns the timeline's name, as sync_file_info shows it.
func (k timelineKey) String() string {
	if k.global {
		return "controlq"
	}
	return fmt.Sprintf("context %d ring %d", k.ctxID, k.ring)
}

// timeline is the fences of a timeline that are not yet signaled.
//
// +stateify savable
type timeline struct {
	// pending is in the order that the fences were created, which is also
	// the order of their IDs.
	pending []*fence
}

// fenceObserver is notified when a fence that it observes is signaled.
type fenceObserver interface {
	// fenceSignaledLocked is called with device.mu locked. If notify is
	// false, the fence was signaled by a load, which has no waiters to wake.
	fenceSignaledLocked(f *fence, notify bool)
}

// fence is a dma-buf fence: a point on a timeline, which the renderer
// signals, or a set of fences, which is signaled once they all are.
//
// +stateify savable
type fence struct {
	// id is the fence's ID, or 0 for a set of fences or a fence that is
	// signaled from the start.
	id       uint64
	timeline timelineKey

	// The following fields are protected by device.mu.

	signaled bool
	// status is 0, or a negative errno if the fence was signaled with an
	// error.
	status int32
	// timestamp is when the fence was signaled, in nanoseconds of the
	// monotonic clock.
	timestamp int64
	// queue is notified when the fence is signaled.
	queue waiter.Queue
	// observers are notified when the fence is signaled.
	observers []fenceObserver

	// members are the fences of a set, and unsignaled is how many of them
	// are not yet signaled.
	members    []*fence
	unsignaled int
}

// newSignaledFence returns a fence that is already signaled.
func newSignaledFence() *fence {
	return &fence{
		timeline: timelineKey{global: true},
		signaled: true,
	}
}

// observeLocked makes o observe f, unless f is signaled.
//
// Preconditions: device.mu is locked.
func (f *fence) observeLocked(o fenceObserver) {
	if !f.signaled {
		f.observers = append(f.observers, o)
	}
}

// unobserveLocked undoes observeLocked.
//
// Preconditions: device.mu is locked.
func (f *fence) unobserveLocked(o fenceObserver) {
	for i, other := range f.observers {
		if other == o {
			f.observers = append(f.observers[:i], f.observers[i+1:]...)
			return
		}
	}
}

// signalLocked signals f with the given status, and wakes its waiters if
// notify is true. Waiters' callbacks run with device.mu locked, which they
// may: waiter.Queue's callbacks do not block or call back into the file.
//
// Preconditions: device.mu is locked. f is not signaled.
func (f *fence) signalLocked(status int32, timestamp int64, notify bool) {
	f.signaled = true
	f.status = status
	f.timestamp = timestamp
	observers := f.observers
	f.observers = nil
	if notify {
		f.queue.Notify(waiter.ReadableEvents | waiter.WritableEvents)
	}
	for _, o := range observers {
		o.fenceSignaledLocked(f, notify)
	}
}

// fenceSignaledLocked implements fenceObserver.fenceSignaledLocked for a set.
func (f *fence) fenceSignaledLocked(member *fence, notify bool) {
	if f.status == 0 && member.status < 0 {
		f.status = member.status
	}
	f.unsignaled--
	if f.unsignaled == 0 {
		f.signalLocked(f.status, member.timestamp, notify)
	}
}

// statusLocked returns the fence's status as dma_fence_get_status does: 0 if
// it is not signaled, 1 if it is, or a negative errno if it failed.
//
// Preconditions: device.mu is locked.
func (f *fence) statusLocked() int32 {
	switch {
	case !f.signaled:
		return 0
	case f.status < 0:
		return f.status
	default:
		return 1
	}
}

// leaves returns the fences that f is made of: f itself, or the members of a
// set.
func (f *fence) leaves() []*fence {
	if f.members == nil {
		return []*fence{f}
	}
	return f.members
}

// mergeFencesLocked returns a fence that is signaled once all of fences are.
// Like Linux's dma_fence_unwrap_merge, it keeps only the latest fence of each
// timeline, and fences that are already signaled only if they failed.
//
// Preconditions: device.mu is locked.
func mergeFencesLocked(fences ...*fence) *fence {
	latest := make(map[timelineKey]*fence)
	var order []timelineKey
	var failed []*fence
	for _, f := range fences {
		for _, leaf := range f.leaves() {
			if leaf.signaled {
				if leaf.status < 0 {
					failed = append(failed, leaf)
				}
				continue
			}
			if leaf.id == 0 {
				// A pending leaf always has an ID; only a set has none.
				panic("virtgpu: a pending fence without an ID")
			}
			prev, ok := latest[leaf.timeline]
			if !ok {
				order = append(order, leaf.timeline)
			}
			if !ok || leaf.id > prev.id {
				latest[leaf.timeline] = leaf
			}
		}
	}
	members := failed
	for _, key := range order {
		members = append(members, latest[key])
	}
	switch len(members) {
	case 0:
		return newSignaledFence()
	case 1:
		return members[0]
	}
	set := &fence{
		timeline: timelineKey{global: true},
		members:  members,
	}
	for _, m := range members {
		if m.signaled {
			if set.status == 0 {
				set.status = m.status
			}
			continue
		}
		set.unsignaled++
		m.observeLocked(set)
	}
	if set.unsignaled == 0 {
		set.signaled = true
	}
	return set
}

// waitFence blocks until f is signaled, as dma_fence_wait() does. It returns
// ERESTARTSYS if ctx is interrupted, and ETIMEDOUT if the timeout, if any,
// expires.
func (d *device) waitFence(ctx context.Context, f *fence, haveTimeout bool, timeout time.Duration) error {
	e, ch := waiter.NewChannelEntry(waiter.ReadableEvents)
	d.mu.Lock()
	if f.signaled {
		d.mu.Unlock()
		return nil
	}
	f.queue.EventRegister(&e)
	d.mu.Unlock()
	defer f.queue.EventUnregister(&e)
	for {
		var err error
		timeout, err = ctx.BlockWithTimeout(ch, haveTimeout, timeout)
		d.mu.Lock()
		signaled := f.signaled
		d.mu.Unlock()
		if signaled {
			return nil
		}
		if err != nil {
			return linuxerr.ConvertIntr(err, linuxerr.ERESTARTSYS)
		}
	}
}
