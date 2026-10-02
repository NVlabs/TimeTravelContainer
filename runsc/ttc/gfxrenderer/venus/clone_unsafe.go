// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// A live fork of a branch whose GPU is in use clones the branch's contexts
// and resources in the renderer, which serves the branch's family: nothing
// is serialized, and nothing is compiled.
//
// The child's contexts share the parent's VkInstance and VkDevices, and, by
// reference, its immutable objects: shader modules, pipelines, layouts,
// samplers, render passes, and descriptor update templates. They create
// their mutable objects anew, with the parent's IDs, by executing the
// commands that created them again (see journal_unsafe.go): memory, whose
// contents they copy, buffers and images, which they bind and whose
// contents they copy, views, framebuffers, pools, descriptor sets, whose
// descriptors they write, command buffers, which they record again, and
// fences, semaphores, and events, whose states they take. Resources are
// copies of the parent's shared memory, or the child's memory's, where the
// parent's memory exported them; rings resume where the parent's were. The
// child creates objects, binds memory, and exports memory in the order in
// which its parent did, in which what each step needs exists.
//
// The guest is stopped while the renderer clones its state, which waits for
// the GPU to be idle, and for the rings to execute what the guest wrote.

// cloneState is a clone of a branch's contexts and resources, which a fork's
// children adopt.
type cloneState struct {
	contexts  map[uint32]*Context
	resources map[uint32]*resource
}

// release destroys a clone that no child adopted.
func (st *cloneState) release() {
	for _, c := range st.contexts {
		c.destroy()
	}
	for _, r := range st.resources {
		r.mem.decRef()
	}
}

// ringDrainTimeout bounds how long a clone waits for rings to execute what
// the stopped guest wrote.
const ringDrainTimeout = 10 * time.Second

// quiesce waits until the contexts' rings executed what was written to
// them, and keeps them from executing more until resume, which it returns.
func quiesce(contexts map[uint32]*Context) (func(), error) {
	var held []*ring
	resume := func() {
		for _, rg := range held {
			rg.execMu.Unlock()
		}
	}
	deadline := time.Now().Add(ringDrainTimeout)
	for _, c := range contexts {
		c.ringMu.Lock()
		rings := make([]*ring, 0, len(c.rings))
		for _, rg := range c.rings {
			rings = append(rings, rg)
		}
		c.ringMu.Unlock()
		for _, rg := range rings {
			for rg.loadHead() != rg.loadTail() && !rg.isStopped() && !c.fatal.Load() {
				if time.Now().After(deadline) {
					resume()
					return nil, fmt.Errorf("ring %d of context %d did not drain", rg.id, c.id)
				}
				rg.notify()
				time.Sleep(100 * time.Microsecond)
			}
			rg.execMu.Lock()
			held = append(held, rg)
		}
	}
	return resume, nil
}

// cloneContexts clones contexts and the resources that they use, which must
// be quiescent.
func cloneContexts(contexts map[uint32]*Context, resources map[uint32]*resource) (*cloneState, error) {
	st := &cloneState{
		contexts:  make(map[uint32]*Context),
		resources: make(map[uint32]*resource),
	}
	cl := &cloner{
		st:      st,
		parents: contexts,
		shm:     make(map[*sharedMemory]*sharedMemory),
		devices: make(map[*device]*device),
		copiers: make(map[*hostDevice]*copyBatch),
	}
	defer cl.release()

	// Every device that the contexts use is idle: the clone copies what
	// it wrote.
	for _, c := range contexts {
		cl.waitIdle(c)
	}

	ids := make([]uint32, 0, len(contexts))
	for id := range contexts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		c := contexts[id]
		cc := newContext(nil, id, c.name)
		st.contexts[id] = cc
		if c.crossDomain != nil {
			cc.crossDomain = c.crossDomain.clone()
		}
		cl.cloneDevices(c, cc)
	}

	// Resources of shared memory of their own are copies. The rest, which
	// the contexts' memory exported, are the child's memory's, which the
	// steps below export again.
	var steps []cloneStep
	for _, r := range resources {
		if r.fromCtx != nil && contexts[r.fromCtx.id] == r.fromCtx {
			steps = append(steps, cloneStep{seq: r.seq, res: r})
			continue
		}
		mem, err := cl.copyShared(r.mem)
		if err != nil {
			st.release()
			return nil, err
		}
		cl.addResource(&resource{id: r.id, size: r.size, mapInfo: r.mapInfo, mem: mem})
	}

	// The contexts' mutable objects, the binding of memory to their
	// buffers and images, and the export of their memory as resources, in
	// the order that the contexts did them, in which what each needs
	// exists: the image of a dedicated allocation, the memory that a
	// buffer is bound to, or the resource that memory imports.
	for _, id := range ids {
		c := contexts[id]
		for _, o := range objectsBySeq(c, func(o *object) bool { return o.create != nil }) {
			steps = append(steps, cloneStep{seq: o.seq, ctx: c, obj: o})
			if o.bound {
				steps = append(steps, cloneStep{seq: o.bindSeq, ctx: c, obj: o, bind: true})
			}
		}
	}
	sort.Slice(steps, func(i, j int) bool { return steps[i].seq < steps[j].seq })
	for _, step := range steps {
		switch {
		case step.res != nil:
			if err := cl.cloneExport(step.res); err != nil {
				st.release()
				return nil, err
			}
		case step.bind:
			cl.cloneBinding(step.ctx, step.obj)
		default:
			cl.cloneObject(step.ctx, step.obj)
		}
	}

	for _, id := range ids {
		c, cc := contexts[id], st.contexts[id]
		cl.clonePools(c, cc)
		cl.cloneStreams(c, cc)
	}
	if err := cl.flush(); err != nil {
		st.release()
		return nil, err
	}
	return st, nil
}

// cloneStep is what a context did that a fork's child does again.
type cloneStep struct {
	seq uint64
	// obj is an object that context ctx created, or bound to memory if bind
	// is set.
	ctx  *Context
	obj  *object
	bind bool
	// res is a resource that memory of a context exported.
	res *resource
}

// cloner clones contexts.
type cloner struct {
	st *cloneState
	// parents are the contexts that the cloner clones.
	parents map[uint32]*Context
	// shm maps the parent's shared memory to the child's copies.
	shm map[*sharedMemory]*sharedMemory
	// devices maps the parent's devices to the child's.
	devices map[*device]*device
	// copiers batch the GPU copies of each VkDevice.
	copiers map[*hostDevice]*copyBatch
	// replays executes the commands that the child executes again, in the
	// context that it replays into.
	replays map[*Context]*stream
}

func (cl *cloner) release() {
	for _, s := range cl.replays {
		s.release()
	}
}

// replay executes commands of the parent's, b, in the child's context cc,
// and reports whether they all succeeded. A command that fails fails only
// what it would have created or recorded: the commands that follow it
// execute, unless the command could not be decoded.
func (cl *cloner) replay(cc *Context, b []byte) bool {
	s := cl.stream(cc)
	s.dec.Reset(b)
	ok := true
	for s.dec.Remaining() > 0 {
		start := s.dec.Offset()
		s.cmdEnd = start
		s.dispatch()
		if !s.dec.Fatal() && !cc.fatal.Load() {
			continue
		}
		ok = false
		cc.fatal.Store(false)
		if s.cmdEnd == start {
			// The command was not decoded: where the next starts is
			// unknown.
			break
		}
		s.dec.ClearFatal()
	}
	s.dec.ResetTempPool()
	return ok
}

// stream returns the stream that replays commands in cc, and makes the
// cloner's own calls.
func (cl *cloner) stream(cc *Context) *stream {
	if cl.replays == nil {
		cl.replays = make(map[*Context]*stream)
	}
	s := cl.replays[cc]
	if s == nil {
		s = newStream(cc, nil)
		s.replaying = true
		cl.replays[cc] = s
	}
	return s
}

// waitIdle waits until the devices of context c are idle.
func (cl *cloner) waitIdle(c *Context) {
	if c.instance == nil {
		return
	}
	var pool vn.Pool
	defer pool.Release()
	for _, pd := range c.instance.physicalDevices {
		if pd == nil {
			continue
		}
		for dev := range pd.devices {
			a := (*vn.Args_vkDeviceWaitIdle)(pool.Alloc(unsafe.Sizeof(vn.Args_vkDeviceWaitIdle{})))
			a.Device = dev.handle
			dev.host.lockQueues()
			vk.Call(vn.VK_COMMAND_TYPE_vkDeviceWaitIdle_EXT, dev.table, unsafe.Pointer(a))
			dev.host.unlockQueues()
		}
	}
}

// copyShared returns a copy of shared memory, with a reference.
func (cl *cloner) copyShared(m *sharedMemory) (*sharedMemory, error) {
	if c, ok := cl.shm[m]; ok {
		c.incRef()
		return c, nil
	}
	c, err := newSharedMemory(uint64(len(m.data)))
	if err != nil {
		return nil, err
	}
	copy(c.data, m.data)
	cl.shm[m] = c
	return c, nil
}

// addResource makes r, whose reference it takes, a resource of the clone,
// which the child's contexts use as their parents use the parent's.
func (cl *cloner) addResource(r *resource) {
	cl.st.resources[r.id] = r
	for id, c := range cl.parents {
		c.resMu.Lock()
		used := c.resources[r.id] != nil
		c.resMu.Unlock()
		if used {
			cl.st.contexts[id].attach(r)
		}
	}
}

// cloneExport makes the child's resource of r, which memory of a parent
// exported, the memory of the child's.
func (cl *cloner) cloneExport(r *resource) error {
	cc := cl.st.contexts[r.fromCtx.id]
	co := cc.lookupType(r.fromMemory, vn.VK_OBJECT_TYPE_DEVICE_MEMORY)
	if co == nil || co.mem == nil || co.mem.shm == nil || co.mem.exported {
		// The child lost the memory: its resource is a copy.
		mem, err := cl.copyShared(r.mem)
		if err != nil {
			return err
		}
		cl.addResource(&resource{id: r.id, size: r.size, mapInfo: r.mapInfo, mem: mem})
		return nil
	}
	co.mem.exported = true
	co.mem.shm.incRef()
	cl.addResource(&resource{id: r.id, size: r.size, mapInfo: r.mapInfo, mem: co.mem.shm, fromCtx: cc, fromMemory: r.fromMemory, seq: r.seq})
	return nil
}

// cloneDevices gives the child's context cc its parent's instance, physical
// devices, devices, and queues, which share the parent's host objects, and
// the parent's immutable objects, which it shares.
func (cl *cloner) cloneDevices(c, cc *Context) {
	inst := c.instance
	if inst == nil {
		return
	}
	inst.refs.Add(1)
	ci := &instance{
		handle:     inst.handle,
		table:      inst.table,
		pdTable:    inst.pdTable,
		apiVersion: inst.apiVersion,
		refs:       inst.refs,
		enumerated: inst.enumerated,
		handles:    append([]vn.Handle(nil), inst.handles...),
	}
	ci.obj = &object{typ: vn.VK_OBJECT_TYPE_INSTANCE, id: inst.obj.id, handle: inst.handle, inst: ci}
	cc.addObject(ci.obj)
	cc.instance = ci
	ci.physicalDevices = make([]*physicalDevice, len(inst.physicalDevices))
	s := cl.stream(cc)
	for i, pd := range inst.physicalDevices {
		if pd == nil {
			continue
		}
		cpd := *pd
		cpd.inst = ci
		cpd.devices = make(map[*device]struct{})
		cpd.obj = &object{typ: vn.VK_OBJECT_TYPE_PHYSICAL_DEVICE, id: pd.obj.id, handle: pd.handle, pd: &cpd}
		cc.addObject(cpd.obj)
		ci.physicalDevices[i] = &cpd
		for dev := range pd.devices {
			dev.host.refs.Add(1)
			cdev := &device{
				pd:      &cpd,
				handle:  dev.handle,
				table:   dev.table,
				metal:   dev.metal,
				objects: make(map[uint64]*object),
				host:    dev.host,
			}
			for _, q := range dev.queues {
				cq := s.cloneQueue(cdev, q)
				cdev.queues = append(cdev.queues, cq)
				if q.obj != nil {
					cq.obj = &object{typ: vn.VK_OBJECT_TYPE_QUEUE, id: q.obj.id, handle: q.handle, dev: cdev, queue: cq}
					cc.addObject(cq.obj)
					if q.ringIdx != 0 {
						cq.ringIdx = q.ringIdx
						cc.syncQueues[q.ringIdx] = cq
					}
				}
			}
			cdev.obj = &object{typ: vn.VK_OBJECT_TYPE_DEVICE, id: dev.obj.id, handle: dev.handle, dev: cdev}
			cc.addObject(cdev.obj)
			cpd.devices[cdev] = struct{}{}
			cl.devices[dev] = cdev
		}
	}

	// The immutable objects.
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, o := range c.objects {
		if o.shared == nil {
			continue
		}
		o.shared.Add(1)
		co := &object{typ: o.typ, id: o.id, handle: o.handle, dev: cl.devices[o.dev], shared: o.shared}
		co.seq = o.seq
		co.layoutBindings = o.layoutBindings
		cc.addObject(co)
	}
}

// cloneQueue returns the child's queue of the parent's q, on the same
// VkQueue, and starts its goroutine.
func (s *stream) cloneQueue(dev *device, q *queue) *queue {
	cq := &queue{
		dev:    dev,
		host:   q.host,
		handle: q.handle,
		flags:  q.flags,
		family: q.family,
		index:  q.index,
		done:   make(chan struct{}),
	}
	cq.syncCond = sync.NewCond(&cq.syncMu)
	go cq.run()
	return cq
}

// objectsBySeq returns the objects of c that pick selects, in the order of
// their creation.
func objectsBySeq(c *Context, pick func(*object) bool) []*object {
	c.mu.RLock()
	var objs []*object
	for _, o := range c.objects {
		if pick(o) {
			objs = append(objs, o)
		}
	}
	c.mu.RUnlock()
	sort.Slice(objs, func(i, j int) bool { return objs[i].seq < objs[j].seq })
	return objs
}

// importsResource reports whether memory o imports a resource.
func importsResource(o *object) bool {
	return o.mem != nil && o.mem.imported
}

// cloneObject creates the child's object of the parent's mutable object o,
// of context c, and gives it o's contents or state.
func (cl *cloner) cloneObject(c *Context, o *object) {
	cc := cl.st.contexts[c.id]
	if !cl.replay(cc, o.create) {
		c.logf("a fork's child lost object %d of type %d", o.id, o.typ)
		return
	}
	co := cc.lookupType(o.id, o.typ)
	if co == nil {
		return
	}
	co.seq = o.seq
	co.image = o.image
	co.timeline = o.timeline
	switch o.typ {
	case vn.VK_OBJECT_TYPE_DEVICE_MEMORY:
		switch {
		case importsResource(o):
			// The memory is the child's resource's, a copy already.
		case o.mem.shm != nil && co.mem.shm != nil:
			copy(co.mem.shm.data, o.mem.shm.data)
		default:
			cl.batch(o.dev).copyMemory(o, co)
		}
	case vn.VK_OBJECT_TYPE_FENCE, vn.VK_OBJECT_TYPE_SEMAPHORE, vn.VK_OBJECT_TYPE_EVENT:
		cl.stream(cc).cloneSyncState(o, co)
	}
}

// cloneBinding binds the child's buffer or image of the parent's o, of
// context c, to memory as o is, and copies an image's contents.
func (cl *cloner) cloneBinding(c *Context, o *object) {
	cc := cl.st.contexts[c.id]
	co := cc.lookupType(o.id, o.typ)
	if co == nil || !cl.stream(cc).bindMemory(o, co) {
		return
	}
	co.bindSeq = o.bindSeq
	if o.typ == vn.VK_OBJECT_TYPE_IMAGE && o.image != nil && o.image.flags&imageCreateSparse == 0 {
		cl.batch(o.dev).copyImage(o, co)
	}
}

// clonePools allocates the child's descriptor sets and command buffers of
// the pools of context c, writes the sets' descriptors, and records again
// what the command buffers recorded.
func (cl *cloner) clonePools(c, cc *Context) {
	s := cl.stream(cc)
	for _, pool := range objectsBySeq(c, func(o *object) bool { return o.children != nil }) {
		cpool := cc.lookupType(pool.id, pool.typ)
		if cpool == nil {
			continue
		}
		c.mu.RLock()
		children := make([]*object, 0, len(pool.children))
		for _, o := range pool.children {
			children = append(children, o)
		}
		c.mu.RUnlock()
		sort.Slice(children, func(i, j int) bool { return children[i].seq < children[j].seq })
		for _, o := range children {
			switch o.typ {
			case vn.VK_OBJECT_TYPE_DESCRIPTOR_SET:
				s.cloneDescriptorSet(cpool, o)
			case vn.VK_OBJECT_TYPE_COMMAND_BUFFER:
				s.cloneCommandBuffer(cpool, o)
			}
		}
	}
	// Record again what was recorded: secondary command buffers before the
	// primary ones that execute them.
	recorded := objectsBySeq(c, func(o *object) bool {
		return o.typ == vn.VK_OBJECT_TYPE_COMMAND_BUFFER && o.recording != nil
	})
	sort.SliceStable(recorded, func(i, j int) bool { return recorded[i].level > recorded[j].level })
	for _, o := range recorded {
		if !cl.replay(cc, o.recording) {
			c.logf("a fork's child could not record command buffer %d again", o.id)
		}
	}
}

// bindMemory binds the child's buffer or image co to memory as the
// parent's o is, and reports whether it did.
func (s *stream) bindMemory(o, co *object) bool {
	mem := s.ctx.lookupType(o.boundMemory, vn.VK_OBJECT_TYPE_DEVICE_MEMORY)
	if mem == nil {
		return false
	}
	typ := int32(vn.VK_COMMAND_TYPE_vkBindBufferMemory_EXT)
	if o.typ == vn.VK_OBJECT_TYPE_IMAGE {
		typ = vn.VK_COMMAND_TYPE_vkBindImageMemory_EXT
	}
	// vkBindBufferMemory and vkBindImageMemory take the same arguments.
	a := newArgs[vn.Args_vkBindBufferMemory](s)
	a.Device = co.dev.handle
	a.Buffer = co.handle
	a.Memory = mem.handle
	a.MemoryOffset = o.boundOffset
	vk.Call(typ, co.dev.table, unsafe.Pointer(a))
	s.dec.ResetTempPool()
	if a.Ret != vn.VK_SUCCESS {
		return false
	}
	co.bound, co.boundMemory, co.boundOffset = true, o.boundMemory, o.boundOffset
	return true
}

// cloneSyncState gives the child's fence, semaphore, or event co the state
// of the parent's o.
func (s *stream) cloneSyncState(o, co *object) {
	defer s.dec.ResetTempPool()
	dev := o.dev
	switch o.typ {
	case vn.VK_OBJECT_TYPE_FENCE:
		get := newArgs[vn.Args_vkGetFenceStatus](s)
		get.Device, get.Fence = dev.handle, o.handle
		vk.Call(vn.VK_COMMAND_TYPE_vkGetFenceStatus_EXT, dev.table, unsafe.Pointer(get))
		cget := newArgs[vn.Args_vkGetFenceStatus](s)
		cget.Device, cget.Fence = co.dev.handle, co.handle
		vk.Call(vn.VK_COMMAND_TYPE_vkGetFenceStatus_EXT, co.dev.table, unsafe.Pointer(cget))
		switch {
		case get.Ret == vn.VK_SUCCESS && cget.Ret == vn.VK_NOT_READY:
			// Signal the child's fence with an empty submission.
			q := co.dev.queues[0]
			a := newArgs[vn.Args_vkQueueSubmit](s)
			a.Queue, a.Fence = q.handle, co.handle
			q.host.mu.Lock()
			vk.Call(vn.VK_COMMAND_TYPE_vkQueueSubmit_EXT, co.dev.table, unsafe.Pointer(a))
			q.host.mu.Unlock()
		case get.Ret == vn.VK_NOT_READY && cget.Ret == vn.VK_SUCCESS:
			fences := alloc[vn.Handle](s, 1)
			*fences = co.handle
			a := newArgs[vn.Args_vkResetFences](s)
			a.Device, a.FenceCount, a.PFences = co.dev.handle, 1, unsafe.Pointer(fences)
			vk.Call(vn.VK_COMMAND_TYPE_vkResetFences_EXT, co.dev.table, unsafe.Pointer(a))
		}
	case vn.VK_OBJECT_TYPE_SEMAPHORE:
		if !o.timeline {
			// A binary semaphore's state cannot be read: the child's is
			// unsignaled.
			return
		}
		get := newArgs[vn.Args_vkGetSemaphoreCounterValue](s)
		value := alloc[uint64](s, 1)
		get.Device, get.Semaphore, get.PValue = dev.handle, o.handle, unsafe.Pointer(value)
		vk.Call(vn.VK_COMMAND_TYPE_vkGetSemaphoreCounterValue_EXT, dev.table, unsafe.Pointer(get))
		if get.Ret != vn.VK_SUCCESS || *value == 0 {
			return
		}
		info := newArgs[vn.VkSemaphoreSignalInfo](s)
		info.SType = vn.VK_STRUCTURE_TYPE_SEMAPHORE_SIGNAL_INFO
		info.Semaphore, info.Value = co.handle, *value
		a := newArgs[vn.Args_vkSignalSemaphore](s)
		a.Device, a.PSignalInfo = co.dev.handle, unsafe.Pointer(info)
		vk.Call(vn.VK_COMMAND_TYPE_vkSignalSemaphore_EXT, co.dev.table, unsafe.Pointer(a))
	case vn.VK_OBJECT_TYPE_EVENT:
		get := newArgs[vn.Args_vkGetEventStatus](s)
		get.Device, get.Event = dev.handle, o.handle
		vk.Call(vn.VK_COMMAND_TYPE_vkGetEventStatus_EXT, dev.table, unsafe.Pointer(get))
		typ := int32(vn.VK_COMMAND_TYPE_vkResetEvent_EXT)
		if get.Ret == vn.VK_EVENT_SET {
			typ = vn.VK_COMMAND_TYPE_vkSetEvent_EXT
		}
		a := newArgs[vn.Args_vkSetEvent](s)
		a.Device, a.Event = co.dev.handle, co.handle
		vk.Call(typ, co.dev.table, unsafe.Pointer(a))
	}
}

// cloneDescriptorSet allocates the child's descriptor set of the parent's
// o, from the child's pool cpool, and writes its descriptors as the
// parent's are.
func (s *stream) cloneDescriptorSet(cpool *object, o *object) {
	defer s.dec.ResetTempPool()
	st := o.set
	if st == nil || st.layout == nil {
		return
	}
	c := s.ctx
	layout := c.lookupType(st.layout.id, vn.VK_OBJECT_TYPE_DESCRIPTOR_SET_LAYOUT)
	if layout == nil {
		return
	}
	dev := cpool.dev
	layouts := alloc[vn.Handle](s, 1)
	*layouts = layout.handle
	info := newArgs[vn.VkDescriptorSetAllocateInfo](s)
	info.SType = vn.VK_STRUCTURE_TYPE_DESCRIPTOR_SET_ALLOCATE_INFO
	info.DescriptorPool = cpool.handle
	info.DescriptorSetCount = 1
	info.PSetLayouts = unsafe.Pointer(layouts)
	if st.hasVariableCount {
		counts := alloc[uint32](s, 1)
		*counts = st.variableCount
		v := newArgs[vn.VkDescriptorSetVariableDescriptorCountAllocateInfo](s)
		v.SType = vn.VK_STRUCTURE_TYPE_DESCRIPTOR_SET_VARIABLE_DESCRIPTOR_COUNT_ALLOCATE_INFO
		v.DescriptorSetCount = 1
		v.PDescriptorCounts = unsafe.Pointer(counts)
		info.PNext = unsafe.Pointer(v)
	}
	out := alloc[vn.Handle](s, 1)
	a := newArgs[vn.Args_vkAllocateDescriptorSets](s)
	a.Device, a.PAllocateInfo, a.PDescriptorSets = dev.handle, unsafe.Pointer(info), unsafe.Pointer(out)
	vk.Call(vn.VK_COMMAND_TYPE_vkAllocateDescriptorSets_EXT, dev.table, unsafe.Pointer(a))
	if a.Ret != vn.VK_SUCCESS {
		c.logf("a fork's child could not allocate descriptor set %d: %d", o.id, a.Ret)
		return
	}
	cs := &descriptorSetState{
		layout:           layout,
		hasVariableCount: st.hasVariableCount,
		variableCount:    st.variableCount,
		descriptors:      make(map[descriptorKey]descriptor, len(st.descriptors)),
	}
	for k, d := range st.descriptors {
		cs.descriptors[k] = d
	}
	if st.inline != nil {
		cs.inline = make(map[uint32][]byte, len(st.inline))
		for b, data := range st.inline {
			cs.inline[b] = append([]byte(nil), data...)
		}
	}
	co := &object{typ: vn.VK_OBJECT_TYPE_DESCRIPTOR_SET, id: o.id, handle: *out, dev: dev, pool: cpool}
	co.seq = o.seq
	co.set = cs
	if !c.addObject(co) {
		return
	}
	c.mu.Lock()
	cpool.children[o.id] = co
	c.mu.Unlock()
	s.writeDescriptors(co)
}

// writeDescriptors writes the descriptors of the child's set co, which its
// state holds by the guest's IDs.
func (s *stream) writeDescriptors(co *object) {
	st := co.set
	c := s.ctx
	keys := st.sortedKeys()
	n := len(keys) + len(st.inline)
	if n == 0 {
		return
	}
	writes := unsafe.Slice(alloc[vn.VkWriteDescriptorSet](s, n), n)
	host := func(id uint64) vn.Handle {
		if o := c.lookup(id); o != nil {
			return o.handle
		}
		return 0
	}
	i := 0
	for _, k := range keys {
		d := st.descriptors[k]
		w := &writes[i]
		w.SType = vn.VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET
		w.DstSet = co.handle
		w.DstBinding = k.binding
		w.DstArrayElement = k.element
		w.DescriptorCount = 1
		w.DescriptorType = d.typ
		switch d.typ {
		case descriptorSampler, descriptorCombinedImageSampler, descriptorSampledImage,
			descriptorStorageImage, descriptorInputAttachment:
			info := newArgs[vn.VkDescriptorImageInfo](s)
			info.Sampler, info.ImageView, info.ImageLayout = host(d.sampler), host(d.imageView), d.imageLayout
			w.PImageInfo = unsafe.Pointer(info)
		case descriptorUniformTexelBuffer, descriptorStorageTexelBuffer:
			view := alloc[vn.Handle](s, 1)
			*view = host(d.texelView)
			w.PTexelBufferView = unsafe.Pointer(view)
		case descriptorUniformBuffer, descriptorStorageBuffer, descriptorUniformBufferDynamic,
			descriptorStorageBufferDynamic:
			info := newArgs[vn.VkDescriptorBufferInfo](s)
			info.Buffer, info.Offset, info.Range = host(d.buffer), d.offset, d.rng
			w.PBufferInfo = unsafe.Pointer(info)
		case descriptorAccelerationKHR:
			accel := alloc[vn.Handle](s, 1)
			*accel = host(d.accel)
			as := newArgs[vn.VkWriteDescriptorSetAccelerationStructureKHR](s)
			as.SType = vn.VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET_ACCELERATION_STRUCTURE_KHR
			as.AccelerationStructureCount = 1
			as.PAccelerationStructures = unsafe.Pointer(accel)
			w.PNext = unsafe.Pointer(as)
		}
		i++
	}
	for b, data := range st.inline {
		if len(data) == 0 {
			continue
		}
		buf := unsafe.Slice(alloc[byte](s, len(data)), len(data))
		copy(buf, data)
		block := newArgs[vn.VkWriteDescriptorSetInlineUniformBlock](s)
		block.SType = vn.VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET_INLINE_UNIFORM_BLOCK
		block.DataSize = uint32(len(data))
		block.PData = unsafe.Pointer(&buf[0])
		w := &writes[i]
		w.SType = vn.VK_STRUCTURE_TYPE_WRITE_DESCRIPTOR_SET
		w.PNext = unsafe.Pointer(block)
		w.DstSet = co.handle
		w.DstBinding = b
		w.DescriptorCount = uint32(len(data))
		w.DescriptorType = descriptorInlineUniformBlock
		i++
	}
	a := newArgs[vn.Args_vkUpdateDescriptorSets](s)
	a.Device = co.dev.handle
	a.DescriptorWriteCount = uint32(i)
	a.PDescriptorWrites = unsafe.Pointer(&writes[0])
	vk.Call(vn.VK_COMMAND_TYPE_vkUpdateDescriptorSets_EXT, co.dev.table, unsafe.Pointer(a))
}

// cloneCommandBuffer allocates the child's command buffer of the parent's
// o, from the child's pool cpool. clonePools records it again.
func (s *stream) cloneCommandBuffer(cpool *object, o *object) {
	defer s.dec.ResetTempPool()
	c := s.ctx
	dev := cpool.dev
	info := newArgs[vn.VkCommandBufferAllocateInfo](s)
	info.SType = vn.VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO
	info.CommandPool = cpool.handle
	info.Level = o.level
	info.CommandBufferCount = 1
	out := alloc[vn.Handle](s, 1)
	a := newArgs[vn.Args_vkAllocateCommandBuffers](s)
	a.Device, a.PAllocateInfo, a.PCommandBuffers = dev.handle, unsafe.Pointer(info), unsafe.Pointer(out)
	vk.Call(vn.VK_COMMAND_TYPE_vkAllocateCommandBuffers_EXT, dev.table, unsafe.Pointer(a))
	if a.Ret != vn.VK_SUCCESS {
		c.logf("a fork's child could not allocate command buffer %d: %d", o.id, a.Ret)
		return
	}
	co := &object{typ: vn.VK_OBJECT_TYPE_COMMAND_BUFFER, id: o.id, handle: *out, dev: dev, pool: cpool}
	co.seq = o.seq
	co.level = o.level
	if !c.addObject(co) {
		return
	}
	c.mu.Lock()
	cpool.children[o.id] = co
	c.mu.Unlock()
}

// cloneStreams gives the child's context cc the parent's rings and reply
// streams, in the child's resources. The child's rings start once a child
// adopts the clone.
func (cl *cloner) cloneStreams(c, cc *Context) {
	cloneReply(c.stream, cc.stream, cc)
	c.ringMu.Lock()
	defer c.ringMu.Unlock()
	for id, rg := range c.rings {
		r := cc.resources[rg.res.id]
		if r == nil {
			c.logf("a fork's child lost ring %d", id)
			continue
		}
		base := uintptr(unsafe.Pointer(&rg.res.mem.data[0]))
		at := func(p unsafe.Pointer) unsafe.Pointer {
			return unsafe.Pointer(&r.mem.data[uintptr(p)-base])
		}
		bufOff := uintptr(unsafe.Pointer(&rg.buf[0])) - base
		crg := &ring{
			id:             id,
			ctx:            cc,
			res:            r,
			head:           (*uint32)(at(unsafe.Pointer(rg.head))),
			tail:           (*uint32)(at(unsafe.Pointer(rg.tail))),
			status:         (*uint32)(at(unsafe.Pointer(rg.status))),
			buf:            r.mem.data[bufOff : bufOff+uintptr(len(rg.buf))],
			cur:            rg.cur,
			idleTimeout:    rg.idleTimeout,
			monitor:        rg.monitor,
			cmd:            make([]byte, len(rg.buf)),
			virtqueueSeqno: rg.virtqueueSeqno,
			done:           make(chan struct{}),
		}
		if len(rg.extra) != 0 {
			off := uintptr(unsafe.Pointer(&rg.extra[0])) - base
			crg.extra = r.mem.data[off : off+uintptr(len(rg.extra))]
		}
		crg.cond = sync.NewCond(&crg.mu)
		crg.stream = newStream(cc, crg)
		crg.stream.afterCommand = func(off int) {
			head := crg.base + uint32(off)
			atomic.StoreUint32(crg.head, head)
			cc.onRingSeqno(crg.id, head)
		}
		cloneReply(rg.stream, crg.stream, cc)
		r.mem.incRef()
		cc.rings[id] = crg
	}
}

// cloneReply gives the child's stream cs the reply stream of the parent's s,
// in the child's resources of cc.
func cloneReply(s, cs *stream, cc *Context) {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	if s.reply == nil {
		return
	}
	r := cc.resources[s.reply.id]
	if r == nil {
		return
	}
	base := uintptr(unsafe.Pointer(&s.reply.mem.data[0]))
	start := uintptr(unsafe.Pointer(unsafe.SliceData(s.enc.Stream()))) - base
	if cs.setReplyStream(r, uint64(start), uint64(len(s.enc.Stream()))) {
		cs.seekReplyStream(uint64(s.enc.Offset()))
	}
}

// adopt makes the clone's contexts and resources the server's, and starts
// the contexts' rings.
func (srv *server) adopt(st *cloneState) {
	srv.mu.Lock()
	for id, r := range st.resources {
		if old := srv.resources[id]; old != nil {
			old.mem.decRef()
		}
		srv.resources[id] = r
	}
	for id, c := range st.contexts {
		c.srv = srv
		srv.contexts[id] = c
	}
	srv.mu.Unlock()
	for _, c := range st.contexts {
		monitor := false
		c.ringMu.Lock()
		for _, rg := range c.rings {
			rg.start()
			monitor = monitor || rg.monitor
		}
		c.ringMu.Unlock()
		if monitor {
			c.startRingMonitor(defaultMonitorPeriod)
		}
	}
}

// defaultMonitorPeriod is the reporting period of a cloned context's ring
// monitor.
const defaultMonitorPeriod = time.Second

// copyBatch batches the GPU copies of a fork's memory and images on a
// VkDevice.
type copyBatch struct {
	dev      *device
	regions  []func(s *stream, cmd vn.Handle)
	cleanups []func(s *stream)
}

// batch returns the batch of copies on dev's VkDevice.
func (cl *cloner) batch(dev *device) *copyBatch {
	b := cl.copiers[dev.host]
	if b == nil {
		b = &copyBatch{dev: dev}
		cl.copiers[dev.host] = b
	}
	return b
}

// Usages of buffers, VkBufferUsageFlagBits.
const bufferUsageTransfers = 0x1 | 0x2

// copyMemory copies memory o of the parent to the child's co with the GPU,
// through buffers that alias both.
func (b *copyBatch) copyMemory(o, co *object) {
	b.regions = append(b.regions, func(s *stream, cmd vn.Handle) {
		src := s.aliasBuffer(o.dev, o.handle, o.mem.size, o.mem.typeIndex)
		if src == 0 {
			return
		}
		dst := s.aliasBuffer(co.dev, co.handle, o.mem.size, o.mem.typeIndex)
		if dst == 0 {
			s.destroyBuffer(o.dev, src)
			return
		}
		region := newArgs[vn.VkBufferCopy](s)
		region.Size = o.mem.size
		a := newArgs[vn.Args_vkCmdCopyBuffer](s)
		a.CommandBuffer, a.SrcBuffer, a.DstBuffer, a.RegionCount, a.PRegions = cmd, src, dst, 1, unsafe.Pointer(region)
		vk.Call(vn.VK_COMMAND_TYPE_vkCmdCopyBuffer_EXT, o.dev.table, unsafe.Pointer(a))
		b.cleanups = append(b.cleanups, func(s *stream) {
			s.destroyBuffer(o.dev, src)
			s.destroyBuffer(co.dev, dst)
		})
	})
}

// aliasBuffer returns a buffer of size bytes bound to the start of memory
// mem, of memory type typeIndex, or 0 if the memory cannot have one.
func (s *stream) aliasBuffer(dev *device, mem vn.Handle, size uint64, typeIndex uint32) vn.Handle {
	info := newArgs[vn.VkBufferCreateInfo](s)
	info.SType = vn.VK_STRUCTURE_TYPE_BUFFER_CREATE_INFO
	info.Size = size
	info.Usage = bufferUsageTransfers
	out := alloc[vn.Handle](s, 1)
	a := newArgs[vn.Args_vkCreateBuffer](s)
	a.Device, a.PCreateInfo, a.PBuffer = dev.handle, unsafe.Pointer(info), unsafe.Pointer(out)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateBuffer_EXT, dev.table, unsafe.Pointer(a))
	if a.Ret != vn.VK_SUCCESS {
		return 0
	}
	req := newArgs[vn.VkMemoryRequirements](s)
	r := newArgs[vn.Args_vkGetBufferMemoryRequirements](s)
	r.Device, r.Buffer, r.PMemoryRequirements = dev.handle, *out, unsafe.Pointer(req)
	vk.Call(vn.VK_COMMAND_TYPE_vkGetBufferMemoryRequirements_EXT, dev.table, unsafe.Pointer(r))
	if req.MemoryTypeBits&(1<<typeIndex) == 0 || req.Size > size {
		s.destroyBuffer(dev, *out)
		return 0
	}
	bind := newArgs[vn.Args_vkBindBufferMemory](s)
	bind.Device, bind.Buffer, bind.Memory = dev.handle, *out, mem
	vk.Call(vn.VK_COMMAND_TYPE_vkBindBufferMemory_EXT, dev.table, unsafe.Pointer(bind))
	if bind.Ret != vn.VK_SUCCESS {
		s.destroyBuffer(dev, *out)
		return 0
	}
	return *out
}

func (s *stream) destroyBuffer(dev *device, buf vn.Handle) {
	a := newArgs[vn.Args_vkDestroyBuffer](s)
	a.Device, a.Buffer = dev.handle, buf
	vk.Call(vn.VK_COMMAND_TYPE_vkDestroyBuffer_EXT, dev.table, unsafe.Pointer(a))
}

// Image aspects and formats that copies of images need.
const (
	aspectColor   = 0x1
	aspectDepth   = 0x2
	aspectStencil = 0x4
	// imageLayoutGeneral is VK_IMAGE_LAYOUT_GENERAL. MoltenVK ignores
	// layouts, as Metal has none, so that copies need no transitions.
	imageLayoutGeneral = 1
)

// formatAspects returns the aspects of a format, or 0 for a format whose
// copies the renderer does not make, such as a multi-planar one.
func formatAspects(format uint32) uint32 {
	switch format {
	case 124, 126: // D16_UNORM, D32_SFLOAT
		return aspectDepth
	case 125: // X8_D24_UNORM_PACK32
		return aspectDepth
	case 127: // S8_UINT
		return aspectStencil
	case 128, 129, 130: // D16_UNORM_S8_UINT, D24_UNORM_S8_UINT, D32_SFLOAT_S8_UINT
		return aspectDepth | aspectStencil
	}
	if format >= 1000156000 && format <= 1000156033 {
		// YCbCr formats, some multi-planar.
		return 0
	}
	return aspectColor
}

// copyImage copies the contents of image o of the parent to the child's co
// with the GPU, every level and layer.
func (b *copyBatch) copyImage(o, co *object) {
	info := o.image
	if info.tiling != imageTilingOptimal {
		// A linear image is in its memory, which the child copied.
		return
	}
	aspects := formatAspects(info.format)
	if aspects == 0 {
		return
	}
	b.regions = append(b.regions, func(s *stream, cmd vn.Handle) {
		n := int(info.mipLevels)
		regions := unsafe.Slice(alloc[vn.VkImageCopy](s, n), n)
		for level := range regions {
			sub := vn.VkImageSubresourceLayers{AspectMask: aspects, MipLevel: uint32(level), LayerCount: info.arrayLayers}
			regions[level] = vn.VkImageCopy{
				SrcSubresource: sub,
				DstSubresource: sub,
				Extent: vn.VkExtent3D{
					Width:  max(1, info.extent.Width>>level),
					Height: max(1, info.extent.Height>>level),
					Depth:  max(1, info.extent.Depth>>level),
				},
			}
		}
		a := newArgs[vn.Args_vkCmdCopyImage](s)
		a.CommandBuffer = cmd
		a.SrcImage, a.SrcImageLayout = o.handle, imageLayoutGeneral
		a.DstImage, a.DstImageLayout = co.handle, imageLayoutGeneral
		a.RegionCount, a.PRegions = uint32(n), unsafe.Pointer(&regions[0])
		vk.Call(vn.VK_COMMAND_TYPE_vkCmdCopyImage_EXT, o.dev.table, unsafe.Pointer(a))
	})
}

// flush executes the batched copies, and waits for them.
func (cl *cloner) flush() error {
	for _, b := range cl.copiers {
		if len(b.regions) == 0 {
			continue
		}
		s := &stream{}
		s.dec = vn.NewDecoder(&s.pool, nil)
		err := b.execute(s)
		s.pool.Release()
		if err != nil {
			return err
		}
	}
	return nil
}

// execute records the batch's copies in a command buffer, submits it, and
// waits for it.
func (b *copyBatch) execute(s *stream) error {
	dev := b.dev
	t := dev.table
	q := dev.queues[0]

	poolInfo := newArgs[vn.VkCommandPoolCreateInfo](s)
	poolInfo.SType = vn.VK_STRUCTURE_TYPE_COMMAND_POOL_CREATE_INFO
	poolInfo.QueueFamilyIndex = q.family
	cmdPool := alloc[vn.Handle](s, 1)
	cp := newArgs[vn.Args_vkCreateCommandPool](s)
	cp.Device, cp.PCreateInfo, cp.PCommandPool = dev.handle, unsafe.Pointer(poolInfo), unsafe.Pointer(cmdPool)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateCommandPool_EXT, t, unsafe.Pointer(cp))
	if cp.Ret != vn.VK_SUCCESS {
		return fmt.Errorf("creating a command pool for a fork's copies: %d", cp.Ret)
	}
	defer func() {
		d := newArgs[vn.Args_vkDestroyCommandPool](s)
		d.Device, d.CommandPool = dev.handle, *cmdPool
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyCommandPool_EXT, t, unsafe.Pointer(d))
	}()

	allocInfo := newArgs[vn.VkCommandBufferAllocateInfo](s)
	allocInfo.SType = vn.VK_STRUCTURE_TYPE_COMMAND_BUFFER_ALLOCATE_INFO
	allocInfo.CommandPool = *cmdPool
	allocInfo.CommandBufferCount = 1
	cmd := alloc[vn.Handle](s, 1)
	ac := newArgs[vn.Args_vkAllocateCommandBuffers](s)
	ac.Device, ac.PAllocateInfo, ac.PCommandBuffers = dev.handle, unsafe.Pointer(allocInfo), unsafe.Pointer(cmd)
	vk.Call(vn.VK_COMMAND_TYPE_vkAllocateCommandBuffers_EXT, t, unsafe.Pointer(ac))
	if ac.Ret != vn.VK_SUCCESS {
		return fmt.Errorf("allocating a command buffer for a fork's copies: %d", ac.Ret)
	}

	beginInfo := newArgs[vn.VkCommandBufferBeginInfo](s)
	beginInfo.SType = vn.VK_STRUCTURE_TYPE_COMMAND_BUFFER_BEGIN_INFO
	beginInfo.Flags = 0x1 // VK_COMMAND_BUFFER_USAGE_ONE_TIME_SUBMIT_BIT
	bc := newArgs[vn.Args_vkBeginCommandBuffer](s)
	bc.CommandBuffer, bc.PBeginInfo = *cmd, unsafe.Pointer(beginInfo)
	vk.Call(vn.VK_COMMAND_TYPE_vkBeginCommandBuffer_EXT, t, unsafe.Pointer(bc))
	for _, region := range b.regions {
		region(s, *cmd)
	}
	ec := newArgs[vn.Args_vkEndCommandBuffer](s)
	ec.CommandBuffer = *cmd
	vk.Call(vn.VK_COMMAND_TYPE_vkEndCommandBuffer_EXT, t, unsafe.Pointer(ec))

	fenceInfo := newArgs[vn.VkFenceCreateInfo](s)
	fenceInfo.SType = vn.VK_STRUCTURE_TYPE_FENCE_CREATE_INFO
	fence := alloc[vn.Handle](s, 1)
	cf := newArgs[vn.Args_vkCreateFence](s)
	cf.Device, cf.PCreateInfo, cf.PFence = dev.handle, unsafe.Pointer(fenceInfo), unsafe.Pointer(fence)
	vk.Call(vn.VK_COMMAND_TYPE_vkCreateFence_EXT, t, unsafe.Pointer(cf))
	if cf.Ret != vn.VK_SUCCESS {
		return fmt.Errorf("creating a fence for a fork's copies: %d", cf.Ret)
	}
	defer func() {
		d := newArgs[vn.Args_vkDestroyFence](s)
		d.Device, d.Fence = dev.handle, *fence
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyFence_EXT, t, unsafe.Pointer(d))
	}()
	submit := newArgs[vn.VkSubmitInfo](s)
	submit.SType = vn.VK_STRUCTURE_TYPE_SUBMIT_INFO
	submit.CommandBufferCount = 1
	submit.PCommandBuffers = unsafe.Pointer(cmd)
	qs := newArgs[vn.Args_vkQueueSubmit](s)
	qs.Queue, qs.SubmitCount, qs.PSubmits, qs.Fence = q.handle, 1, unsafe.Pointer(submit), *fence
	q.host.mu.Lock()
	vk.Call(vn.VK_COMMAND_TYPE_vkQueueSubmit_EXT, t, unsafe.Pointer(qs))
	q.host.mu.Unlock()
	if qs.Ret != vn.VK_SUCCESS {
		return fmt.Errorf("submitting a fork's copies: %d", qs.Ret)
	}
	wait := newArgs[vn.Args_vkWaitForFences](s)
	wait.Device, wait.FenceCount, wait.PFences, wait.WaitAll, wait.Timeout = dev.handle, 1, unsafe.Pointer(fence), 1, ^uint64(0)
	vk.Call(vn.VK_COMMAND_TYPE_vkWaitForFences_EXT, t, unsafe.Pointer(wait))
	for _, cleanup := range b.cleanups {
		cleanup(s)
	}
	if wait.Ret != vn.VK_SUCCESS {
		return fmt.Errorf("waiting for a fork's copies: %d", wait.Ret)
	}
	return nil
}
