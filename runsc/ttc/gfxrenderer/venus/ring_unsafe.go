// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"math/bits"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// Ring status bits, VkRingStatusFlagBitsMESA.
const (
	ringStatusIdle  = 1
	ringStatusFatal = 2
	ringStatusAlive = 4
)

// maxRingBufferSize is the size of the largest ring buffer, as
// virglrenderer's VKR_RING_BUFFER_MAX_SIZE.
const maxRingBufferSize = 16 << 20

// ring is a ring of a context: a buffer of commands in shared memory, which
// the guest appends to and the ring executes, as virglrenderer's vkr_ring.c
// does.
type ring struct {
	id  uint64
	ctx *Context
	res *resource

	// head, tail, and status are in the resource. The guest advances the
	// tail as it appends commands; the ring advances the head as it
	// executes them.
	head, tail, status *uint32
	// buf is the ring's buffer, of a power-of-two size.
	buf []byte
	// cur is where the ring reads, free-running.
	cur uint32
	// extra is memory where vkWriteRingExtraMESA writes.
	extra []byte

	stream      *stream
	idleTimeout time.Duration
	monitor     bool
	// cmd holds the commands being executed.
	cmd []byte
	// base is the head at the start of cmd.
	base uint32

	// execMu is held while the ring executes commands, which a fork's
	// clone keeps it from doing.
	execMu sync.Mutex

	mu      sync.Mutex
	cond    *sync.Cond
	started bool
	// launched is whether the ring's goroutine started: a fork's clone
	// has rings that start only once a child adopts it.
	launched       bool
	pendingNotify  bool
	virtqueueSeqno uint64
	done           chan struct{}
	stopped        atomic.Bool
}

// region is a part of a resource.
type region struct {
	begin, end uint64
}

func newRegion(offset, size uint64) (region, bool) {
	end := offset + size
	return region{offset, end}, end >= offset
}

func (r region) within(o region) bool {
	return r.begin >= o.begin && r.end <= o.end
}

func (r region) disjoint(o region) bool {
	return r.end <= o.begin || o.end <= r.begin
}

// createRing creates a ring, as vkCreateRingMESA asks.
func (c *Context) createRing(s *stream, id uint64, info *vn.VkRingCreateInfoMESA) {
	r := c.resource(info.ResourceId)
	if r == nil {
		s.fail("vkCreateRingMESA: no resource %d", info.ResourceId)
		return
	}
	whole, ok := newRegion(info.Offset, info.Size)
	if !ok || !whole.within(region{0, r.size}) {
		s.fail("vkCreateRingMESA: the ring exceeds its resource")
		return
	}
	var regions [5]region
	sizes := [5]uint64{4, 4, 4, info.BufferSize, info.ExtraSize}
	offsets := [5]uint64{info.HeadOffset, info.TailOffset, info.StatusOffset, info.BufferOffset, info.ExtraOffset}
	for i := range regions {
		reg, ok := newRegion(info.Offset+offsets[i], sizes[i])
		if !ok || info.Offset+offsets[i] < info.Offset || !reg.within(whole) || reg.begin%4 != 0 {
			s.fail("vkCreateRingMESA: invalid layout")
			return
		}
		regions[i] = reg
	}
	for i := range regions {
		for j := i + 1; j < len(regions); j++ {
			if !regions[i].disjoint(regions[j]) {
				s.fail("vkCreateRingMESA: overlapping layout")
				return
			}
		}
	}
	size := info.BufferSize
	if size == 0 || size > maxRingBufferSize || size&(size-1) != 0 {
		s.fail("vkCreateRingMESA: invalid buffer size %d", size)
		return
	}
	c.ringMu.Lock()
	taken := c.rings[id] != nil
	c.ringMu.Unlock()
	if taken {
		s.fail("vkCreateRingMESA: ring %d exists", id)
		return
	}
	data := r.mem.data
	rg := &ring{
		id:          id,
		ctx:         c,
		res:         r,
		head:        (*uint32)(unsafe.Pointer(&data[regions[0].begin])),
		tail:        (*uint32)(unsafe.Pointer(&data[regions[1].begin])),
		status:      (*uint32)(unsafe.Pointer(&data[regions[2].begin])),
		buf:         data[regions[3].begin:regions[3].end],
		extra:       data[regions[4].begin:regions[4].end],
		idleTimeout: time.Duration(info.IdleTimeout),
		cmd:         make([]byte, size),
		done:        make(chan struct{}),
	}
	// The ring manages its head and status, which start at 0.
	if atomic.LoadUint32(rg.head) != 0 || atomic.LoadUint32(rg.status) != 0 {
		s.fail("vkCreateRingMESA: the head or status is not 0")
		return
	}
	rg.cond = sync.NewCond(&rg.mu)
	rg.stream = newStream(c, rg)
	rg.stream.afterCommand = func(off int) {
		head := rg.base + uint32(off)
		atomic.StoreUint32(rg.head, head)
		c.onRingSeqno(rg.id, head)
	}
	for p := info.PNext; p != nil; p = *(*unsafe.Pointer)(unsafe.Add(p, 8)) {
		switch *(*int32)(p) {
		case vn.VK_STRUCTURE_TYPE_RING_MONITOR_INFO_MESA:
			period := (*vn.VkRingMonitorInfoMESA)(p).MaxReportingPeriodMicroseconds
			if period == 0 {
				s.fail("vkCreateRingMESA: invalid reporting period")
				return
			}
			c.startRingMonitor(time.Duration(period) * time.Microsecond)
			rg.monitor = true
		case vn.VK_STRUCTURE_TYPE_RING_PRIORITY_INFO_MESA:
			// Go schedules the ring's goroutine as it schedules others.
		}
	}
	r.mem.incRef()
	c.ringMu.Lock()
	c.rings[id] = rg
	c.ringMu.Unlock()
	rg.start()
}

// start starts the ring's goroutine.
func (rg *ring) start() {
	rg.mu.Lock()
	rg.started = true
	rg.launched = true
	rg.mu.Unlock()
	go rg.run()
}

func (rg *ring) loadHead() uint32 {
	return atomic.LoadUint32(rg.head)
}

func (rg *ring) loadTail() uint32 {
	return atomic.LoadUint32(rg.tail)
}

func (rg *ring) setStatus(mask uint32) {
	atomic.OrUint32(rg.status, mask)
}

func (rg *ring) clearStatus(mask uint32) {
	atomic.AndUint32(rg.status, ^mask)
}

// read copies the next len(dst) bytes of the buffer to dst.
func (rg *ring) read(dst []byte) {
	size := uint32(len(rg.buf))
	off := rg.cur & (size - 1)
	n := copy(dst, rg.buf[off:])
	copy(dst[n:], rg.buf)
	rg.cur += uint32(len(dst))
}

// run executes the ring's commands until it stops.
func (rg *ring) run() {
	defer close(rg.done)
	defer rg.stream.release()

	lastSubmit := time.Now()
	relax := uint32(0)
	failed := false
	for rg.running() {
		wait := false
		if time.Since(lastSubmit) >= rg.idleTimeout {
			rg.mu.Lock()
			rg.pendingNotify = false
			rg.mu.Unlock()
			rg.setStatus(ringStatusIdle)
			wait = rg.cur == rg.loadTail()
			if !wait {
				rg.clearStatus(ringStatusIdle)
			}
		}
		if wait {
			rg.mu.Lock()
			for rg.started && !rg.pendingNotify {
				rg.cond.Wait()
			}
			started := rg.started
			rg.mu.Unlock()
			rg.clearStatus(ringStatusIdle)
			if !started {
				break
			}
			lastSubmit = time.Now()
			relax = 0
		}

		size := rg.loadTail() - rg.cur
		if size != 0 {
			if size > uint32(len(rg.buf)) {
				rg.ctx.logf("ring %d: %d bytes of commands exceed the ring", rg.id, size)
				failed = true
				break
			}
			rg.execMu.Lock()
			rg.base = rg.cur
			cmd := rg.cmd[:size]
			rg.read(cmd)
			ok := rg.stream.execute(cmd)
			rg.execMu.Unlock()
			if !ok {
				failed = true
				break
			}
			lastSubmit = time.Now()
			relax = 0
			continue
		}
		// A context that waits for this ring to reach a seqno that its tail
		// cannot reach waits forever.
		if seqno, ok := rg.ctx.waitedRingSeqno(rg.id); ok && !seqnoGE(rg.loadTail(), seqno) {
			rg.ctx.logf("ring %d: its tail %d cannot reach seqno %d", rg.id, rg.loadTail(), seqno)
			failed = true
			break
		}
		rg.relax(&relax)
	}
	if failed {
		rg.setStatus(ringStatusFatal)
		rg.ctx.setFatal()
	}
}

// relax waits a little for commands, as vkr_ring_relax does: it yields 16
// times, then sleeps longer and longer.
func (rg *ring) relax(iter *uint32) {
	const busyWaitOrder = 4
	*iter++
	if *iter < 1<<busyWaitOrder {
		runtime.Gosched()
		return
	}
	shift := bits.Len32(*iter) - busyWaitOrder - 1
	time.Sleep(10 * time.Microsecond << shift)
}

func (rg *ring) running() bool {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	return rg.started
}

// notify wakes the ring.
func (rg *ring) notify() {
	rg.mu.Lock()
	rg.pendingNotify = true
	rg.cond.Broadcast()
	rg.mu.Unlock()
}

// stop stops the ring and waits for it.
func (rg *ring) stop() {
	rg.mu.Lock()
	rg.started = false
	launched := rg.launched
	rg.cond.Broadcast()
	rg.mu.Unlock()
	if launched {
		<-rg.done
	}
	if !rg.stopped.Swap(true) {
		if !launched {
			// What the ring's goroutine releases as it ends.
			rg.stream.release()
		}
		rg.res.mem.decRef()
	}
	// Wake the context if it waits for the ring.
	rg.ctx.waitRing.mu.Lock()
	rg.ctx.waitRing.cond.Broadcast()
	rg.ctx.waitRing.mu.Unlock()
}

func (rg *ring) isStopped() bool {
	return rg.stopped.Load()
}

// writeExtra writes val at offset of the ring's extra memory.
func (rg *ring) writeExtra(offset uint64, val uint32) bool {
	if offset > uint64(len(rg.extra)) || uint64(len(rg.extra))-offset < 4 || offset%4 != 0 {
		return false
	}
	atomic.StoreUint32((*uint32)(unsafe.Pointer(&rg.extra[offset])), val)
	return true
}

// submitVirtqueueSeqno records that the context's stream reached seqno.
func (rg *ring) submitVirtqueueSeqno(seqno uint64) {
	rg.mu.Lock()
	rg.virtqueueSeqno = seqno
	rg.cond.Broadcast()
	rg.mu.Unlock()
}

// waitVirtqueueSeqno waits until the context's stream reaches seqno.
func (rg *ring) waitVirtqueueSeqno(seqno uint64) bool {
	rg.mu.Lock()
	defer rg.mu.Unlock()
	for rg.started && rg.virtqueueSeqno < seqno {
		rg.cond.Wait()
	}
	return rg.virtqueueSeqno >= seqno
}

// startRingMonitor starts reporting that the context's monitored rings are
// alive, at least every period.
func (c *Context) startRingMonitor(period time.Duration) {
	c.ringMu.Lock()
	defer c.ringMu.Unlock()
	if c.monitor != nil {
		if period < c.monitor.period {
			c.monitor.period = period
			c.monitor.reset <- struct{}{}
		}
		return
	}
	m := &ringMonitor{period: period, reset: make(chan struct{}, 1), stop: make(chan struct{})}
	c.monitor = m
	go m.run(c)
}

// ringMonitor marks a context's monitored rings alive, for their guest's
// watchdog.
type ringMonitor struct {
	period time.Duration
	reset  chan struct{}
	stop   chan struct{}
}

func (m *ringMonitor) run(c *Context) {
	for {
		c.ringMu.Lock()
		period := m.period
		for _, rg := range c.rings {
			if rg.monitor {
				rg.setStatus(ringStatusAlive)
			}
		}
		c.ringMu.Unlock()
		select {
		case <-m.stop:
			return
		case <-m.reset:
		case <-time.After(period):
		}
	}
}
