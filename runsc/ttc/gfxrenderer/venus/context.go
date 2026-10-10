// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"fmt"
	"log"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/virgl"
)

// object is an object of a context, by the ID that the context's guest gave
// it.
type object struct {
	typ    int32
	id     uint64
	handle vn.Handle

	// dev is the device of a device's object, and of the device itself.
	dev *device

	// What objects of some types keep.
	inst  *instance
	pd    *physicalDevice
	queue *queue
	mem   *memory
	// pool is the pool of a command buffer or a descriptor set.
	pool *object
	// children are a pool's command buffers or descriptor sets.
	children map[uint64]*object
	// external is whether a buffer or image was created for external
	// memory that the renderer emulates (see dmabuf.go).
	external bool

	// What shadows need (see shadow_unsafe.go): an image's shadow, if it is
	// one; the shadow that an image view views; a framebuffer's attachments
	// that view shadows; what a render pass does with its attachments; and
	// the shadows that a command buffer wrote since it began.
	shadow        *shadowImage
	viewOf        *object
	fbShadows     []fbShadow
	attachmentOps []attachmentOp
	shadowWrites  []shadowWrite

	// shared, if not nil, counts the contexts whose objects share the
	// object's host handle: a fork's child shares its parent's immutable
	// objects, such as pipelines. The last one destroys it.
	shared *atomic.Int32

	// What a fork's child needs to rebuild the object: see
	// journal_unsafe.go.
	journalState
}

// releaseHost drops the object's reference on its host handle, and reports
// whether it was the last, which destroys the handle.
func (o *object) releaseHost() bool {
	return o.shared == nil || o.shared.Add(-1) == 0
}

// resource is a resource of a server: memory that the Sentry maps into the
// guest, and that the renderer may read and write, as rings and reply
// streams.
type resource struct {
	id   uint32
	size uint64
	// mapInfo is a VIRTGPU_MAP_CACHE_*.
	mapInfo uint32
	// mem is the resource's memory.
	mem *sharedMemory
	// fromCtx and fromMemory are, for a resource that exported a context's
	// memory, the context and the memory's ID, and seq is the export's in
	// journalSeq.
	fromCtx    *Context
	fromMemory uint64
	seq        uint64
	// virgl is a virgl context's classic resource's Gallium resource,
	// which holds a reference to mem.
	virgl *virgl.Resource
}

// release releases the server's references to the resource's memory.
func (r *resource) release() {
	if r.virgl != nil {
		r.virgl.DecRef()
	}
	r.mem.decRef()
}

// sharedMemory is shared memory that the renderer maps, and that the Sentry
// maps into the guest by its descriptor.
type sharedMemory struct {
	fd   int
	data []byte
	refs atomic.Int32
}

// newSharedMemory returns size bytes of shared memory, mapped, with a
// reference.
func newSharedMemory(size uint64) (*sharedMemory, error) {
	size = alignUp(size, uint64(unix.Getpagesize()))
	fd, err := vk.CreateSharedMemory(size)
	if err != nil {
		return nil, err
	}
	data, err := unix.Mmap(fd, 0, int(size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	m := &sharedMemory{fd: fd, data: data}
	m.refs.Store(1)
	return m, nil
}

func (m *sharedMemory) incRef() {
	m.refs.Add(1)
}

func (m *sharedMemory) decRef() {
	if m.refs.Add(-1) == 0 {
		unix.Munmap(m.data)
		unix.Close(m.fd)
	}
}

func alignUp(n, align uint64) uint64 {
	return (n + align - 1) &^ (align - 1)
}

// Context is a Venus context: what one guest process does with the GPU.
type Context struct {
	srv  *server
	id   uint32
	name string

	// fatal is whether the guest broke the protocol. The context executes
	// nothing more.
	fatal atomic.Bool

	mu sync.RWMutex
	// objects are the context's objects.
	objects map[uint64]*object
	// instance is the context's instance, the one that it may have.
	instance *instance
	// syncQueues are the queues bound to each ring's timeline.
	syncQueues [64]*queue

	resMu sync.Mutex
	// resources are the resources that the context may use.
	resources map[uint32]*resource

	// stream executes what the Sentry submits.
	stream *stream

	// crossDomain is the state of a cross-domain context, which executes
	// cross-domain commands rather than Venus's (see crossdomain_unsafe.go).
	crossDomain *crossDomain

	ringMu sync.Mutex
	rings  map[uint64]*ring
	// monitor marks monitored rings alive.
	monitor *ringMonitor

	// waitRing is the ring head that the context's stream waits for.
	waitRing struct {
		mu    sync.Mutex
		cond  *sync.Cond
		id    uint64
		seqno uint32
	}
}

func newContext(srv *server, id uint32, name string) *Context {
	c := &Context{
		srv:       srv,
		id:        id,
		name:      name,
		objects:   make(map[uint64]*object),
		resources: make(map[uint32]*resource),
		rings:     make(map[uint64]*ring),
	}
	c.waitRing.cond = sync.NewCond(&c.waitRing.mu)
	c.stream = newStream(c, nil)
	return c
}

// lib returns the Vulkan library, which every server of the process
// shares.
func (c *Context) lib() *vk.Library {
	return library
}

// logf logs a message about the context.
func (c *Context) logf(format string, args ...any) {
	log.Printf("venus: context %d (%s): %s", c.id, c.name, fmt.Sprintf(format, args...))
}

// setFatal marks the context failed: its guest broke the protocol, or the
// renderer failed it.
func (c *Context) setFatal() {
	if !c.fatal.Swap(true) {
		c.logf("fatal error")
	}
	// Wake the stream if it waits for a ring.
	c.waitRing.mu.Lock()
	c.waitRing.cond.Broadcast()
	c.waitRing.mu.Unlock()
}

// HasObject implements vn.ObjectTable.HasObject.
func (c *Context) HasObject(id uint64, typ int32) bool {
	c.mu.RLock()
	o := c.objects[id]
	c.mu.RUnlock()
	if o == nil || o.typ != typ {
		if o != nil {
			c.logf("object %d has type %d, not %d", id, o.typ, typ)
		} else {
			c.logf("no object %d of type %d", id, typ)
		}
		return false
	}
	return true
}

// HostHandle implements vn.ObjectTable.HostHandle.
func (c *Context) HostHandle(id uint64, typ int32) vn.Handle {
	c.mu.RLock()
	o := c.objects[id]
	c.mu.RUnlock()
	if o == nil {
		return 0
	}
	return o.handle
}

// lookup returns object id, or nil.
func (c *Context) lookup(id uint64) *object {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.objects[id]
}

// lookupType returns object id if it has type typ, or nil.
func (c *Context) lookupType(id uint64, typ int32) *object {
	o := c.lookup(id)
	if o == nil || o.typ != typ {
		return nil
	}
	return o
}

// validID reports whether id is free for a new object.
func (c *Context) validIDLocked(id uint64) bool {
	if id == 0 || c.objects[id] != nil {
		c.logf("invalid object ID %d", id)
		return false
	}
	return true
}

// addObject adds o, whose ID must be free; otherwise it fails the context.
func (c *Context) addObject(o *object) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.validIDLocked(o.id) {
		c.setFatal()
		return false
	}
	c.objects[o.id] = o
	if o.dev != nil && trackedByDevice(o.typ) {
		o.dev.objects[o.id] = o
	}
	return true
}

// removeObject removes o.
func (c *Context) removeObject(o *object) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.removeObjectLocked(o)
}

func (c *Context) removeObjectLocked(o *object) {
	if c.objects[o.id] == o {
		delete(c.objects, o.id)
	}
	if o.dev != nil {
		delete(o.dev.objects, o.id)
	}
	if o.pool != nil {
		delete(o.pool.children, o.id)
	}
}

// resource returns resource id of the context, or nil.
func (c *Context) resource(id uint32) *resource {
	c.resMu.Lock()
	defer c.resMu.Unlock()
	return c.resources[id]
}

// attach lets the context use r.
func (c *Context) attach(r *resource) {
	c.resMu.Lock()
	defer c.resMu.Unlock()
	if c.resources[r.id] == nil {
		r.mem.incRef()
		c.resources[r.id] = r
	}
}

// detach ends the context's use of resource id. A ring or reply stream in
// it fails the context, as virglrenderer's does.
func (c *Context) detach(id uint32) {
	c.resMu.Lock()
	r := c.resources[id]
	delete(c.resources, id)
	c.resMu.Unlock()
	if r == nil {
		return
	}
	if !c.stream.releaseResource(r) {
		c.setFatal()
	}
	c.ringMu.Lock()
	var stopped []*ring
	for id, rg := range c.rings {
		if rg.res == r || !rg.stream.releaseResource(r) {
			c.setFatal()
			delete(c.rings, id)
			stopped = append(stopped, rg)
		}
	}
	c.ringMu.Unlock()
	for _, rg := range stopped {
		rg.stop()
	}
	r.mem.decRef()
}

// submit executes a command stream that the Sentry submitted.
func (c *Context) submit(buf []byte) error {
	if c.fatal.Load() {
		return fmt.Errorf("the context failed")
	}
	if c.crossDomain != nil {
		return c.crossDomain.submit(c, buf)
	}
	if !c.stream.execute(buf) {
		return fmt.Errorf("executing a command stream failed")
	}
	return nil
}

// createFence signals fence once what was submitted to the context's ring
// ring before it is done.
func (c *Context) createFence(fenceID uint64, ring uint32) error {
	// Ring 0 is the CPU's: what was submitted before the fence was
	// executed when the fence came.
	if ring == 0 {
		c.srv.fenceSignaled(fenceID, c.id, true, 0)
		return nil
	}
	c.mu.RLock()
	var q *queue
	if ring < uint32(len(c.syncQueues)) {
		q = c.syncQueues[ring]
	}
	c.mu.RUnlock()
	if q == nil {
		return fmt.Errorf("no queue is bound to ring %d", ring)
	}
	return q.submitSync(c, ring, fenceID)
}

// destroy destroys the context and its objects.
func (c *Context) destroy() {
	c.ringMu.Lock()
	rings := c.rings
	c.rings = make(map[uint64]*ring)
	if c.monitor != nil {
		close(c.monitor.stop)
		c.monitor = nil
	}
	c.ringMu.Unlock()
	for _, rg := range rings {
		rg.stop()
	}
	// The context executes nothing more.
	c.fatal.Store(true)
	if c.instance != nil {
		c.stream.destroyInstance(c.instance)
		c.stream.dec.ResetTempPool()
	}
	c.stream.release()
	c.resMu.Lock()
	resources := c.resources
	c.resources = make(map[uint32]*resource)
	c.resMu.Unlock()
	for _, r := range resources {
		r.mem.decRef()
	}
}
