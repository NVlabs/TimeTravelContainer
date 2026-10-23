// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build (darwin || linux) && cgo

package virgl

import (
	"errors"
	"fmt"
	"sort"
)

// A live fork of a branch whose GPU is in use clones the branch's virgl
// contexts in the renderer, which serves the branch's family, for the fork's
// children, as it clones Venus's (see the venus package's clone_unsafe.go):
// nothing is serialized.
//
// A clone's sub-contexts are backend contexts of their own, of the parent's
// IDs, in which the clone creates the parent's objects again, of what the
// commands that created them carried, and binds and sets what the parent's
// state holds, of what the decoder keeps of it. Its shaders are translated
// again of their TGSI, of pipelines that the clone's draws compile.
// Its resources are copies, of their parameters and contents: those that
// the guest has are the clones that the server makes of its resources, and
// those that only the context holds, which the guest destroyed, copies of
// their own. A query that is active is begun again, and counts what the
// clone does from the fork on.

// Clone returns a copy of the context for a fork's child, whose resources
// are those that resource returns, with a reference, for the context's.
func (c *Context) Clone(resource func(*Resource) (*Resource, error)) (*Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.destroyed {
		return nil, errDestroyed
	}
	// The clone copies what the context wrote, once the GPU has written it.
	c.waitIdleLocked()
	cl := &cloner{resource: resource, clones: make(map[*Resource]*Resource)}
	defer cl.release()
	nc := &Context{
		id:            c.id,
		resources:     make(map[uint32]*Resource),
		subs:          make(map[uint32]*subContext),
		loggedCapture: c.loggedCapture,
	}
	if err := cl.cloneContext(nc, c); err != nil {
		nc.destroySubsLocked()
		return nil, err
	}
	return nc, nil
}

// waitIdleLocked waits until the GPU did what the context executed, and
// writes the results of the queries that the guest asked for.
//
// Preconditions: c.mu is locked.
func (c *Context) waitIdleLocked() {
	for _, sub := range c.subs {
		if f := sub.pipe.flush(); f != nil {
			f.wait()
		}
	}
	c.checkQueriesLocked()
}

// cloner clones a context.
type cloner struct {
	// resource returns a resource's clone, with a reference.
	resource func(*Resource) (*Resource, error)
	// clones are the clones of the resources, with a reference each of
	// the cloner's.
	clones map[*Resource]*Resource
}

func (cl *cloner) release() {
	for _, r := range cl.clones {
		r.DecRef()
	}
}

// res returns the clone of r, with a reference, or nil if r is nil.
func (cl *cloner) res(r *Resource) (*Resource, error) {
	if r == nil {
		return nil, nil
	}
	cr := cl.clones[r]
	if cr == nil {
		var err error
		if cr, err = cl.resource(r); err != nil {
			return nil, err
		}
		cl.clones[r] = cr
	}
	cr.incRef()
	return cr, nil
}

// cloneContext gives nc the resources and sub-contexts of c.
func (cl *cloner) cloneContext(nc, c *Context) error {
	for id, r := range c.resources {
		cr, err := cl.res(r)
		if err != nil {
			return err
		}
		nc.resources[id] = cr
	}
	ids := make([]uint32, 0, len(c.subs))
	for id := range c.subs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		nsub, err := nc.newSubContext()
		if err != nil {
			return err
		}
		nc.subs[id] = nsub
		// A sub-context's commands, which the clone executes again, are
		// executed in the current one.
		nc.sub = nsub
		if err := cl.cloneSub(nc, nsub, c.subs[id]); err != nil {
			return fmt.Errorf("sub-context %d: %w", id, err)
		}
	}
	for id, sub := range c.subs {
		if sub == c.sub {
			nc.sub = nc.subs[id]
		}
	}
	return nil
}

// cloneSub gives nsub, a new sub-context of nc, the objects and state of
// sub.
func (cl *cloner) cloneSub(nc *Context, nsub, sub *subContext) error {
	objs := make(map[*object]*object, len(sub.objects)+len(sub.zombies)+len(sub.defaults))
	for typ, o := range sub.defaults {
		if o != nil {
			objs[o] = nsub.defaults[typ]
		}
	}
	for h, o := range sub.objects {
		co, err := cl.object(nc, nsub, o)
		if err != nil {
			return fmt.Errorf("object %d of type %d: %w", h, o.typ, err)
		}
		nsub.objects[h] = co
		objs[o] = co
	}
	for o := range sub.zombies {
		co, err := cl.object(nc, nsub, o)
		if err != nil {
			return fmt.Errorf("a destroyed object of type %d: %w", o.typ, err)
		}
		nsub.zombies[co] = struct{}{}
		objs[o] = co
	}
	if err := cl.state(nc, nsub, sub, objs); err != nil {
		return err
	}
	// The destroyed objects that only the parent's derived state holds, as
	// a vertex shader whose variant a draw bound, the clone does not need.
	for o := range nsub.zombies {
		if o.binds == 0 {
			nc.freeObjectLocked(nsub, o)
		}
	}
	return nil
}

// object returns the clone of o, an object of a parent's sub-context, in
// sub, which the caller adds to sub's objects.
func (cl *cloner) object(nc *Context, sub *subContext, o *object) (*object, error) {
	pipe := sub.pipe
	co := &object{typ: o.typ, stage: o.stage, deleted: o.deleted, args: o.args}
	var err error
	if co.res, err = cl.res(o.res); err != nil {
		return nil, err
	}
	d := o.args
	switch o.typ {
	case objectBlend:
		co.obj = pipe.createBlend(d)
	case objectRasterizer:
		co.obj = pipe.createRasterizer(d)
	case objectDSA:
		co.obj = pipe.createDSA(d)
	case objectSamplerState:
		co.obj = pipe.createSamplerState(d)
	case objectVertexElements:
		// Draws create the backend's vertex elements of the strides bound.
		co.elements, co.sintMask, co.uintMask = o.elements, o.sintMask, o.uintMask
		co.csos = make(map[string]any)
	case objectSurface, objectMSAASurface:
		co.surf = o.surf
		co.surf.res = co.res.res
	case objectSamplerView:
		co.obj = pipe.createSamplerView(co.res.res, d[2]&0xffffff, d[2]>>24, d[3], d[4], d[5])
	case objectQuery:
		co.qtype, co.counted, co.sub = o.qtype, o.counted, sub
		co.generated, co.emitted, co.needed = o.generated, o.emitted, o.needed
		if !o.counted {
			co.obj = pipe.createQuery(o.qtype, d[1]>>16)
			if co.obj != nil && o.active {
				pipe.beginQuery(co.obj)
				co.active = true
			}
		}
	case objectStreamoutTarget:
		co.soOffset, co.soSize, co.soFilled, co.soVerts = o.soOffset, o.soSize, o.soFilled, o.soVerts
	case objectShader:
		co.numTokens, co.so, co.shared = o.numTokens, o.so, o.shared
		if o.text != nil {
			// A shader whose text the guest has yet to send all of.
			co.text = append([]byte(nil), o.text...)
			co.filled = o.filled
			break
		}
		if o.code == nil {
			nc.freeObjectLocked(sub, co)
			return nil, errors.New("a shader without its TGSI")
		}
		co.code = o.code.dup()
		if co.obj, err = pipe.createShader(o.stage, co.code, o.so, o.shared, 0, 0, false); err != nil {
			nc.freeObjectLocked(sub, co)
			return nil, fmt.Errorf("compiling a shader again: %w", err)
		}
		if o.stage == stageVertex {
			co.variants = map[variantKey]any{{}: co.obj}
		}
	}
	if co.obj == nil && co.text == nil && co.typ != objectSurface && co.typ != objectMSAASurface && !co.renderers() {
		nc.freeObjectLocked(sub, co)
		return nil, errors.New("the backend refused it")
	}
	return co, nil
}

// state binds and sets in nsub, the current sub-context of nc, what sub's
// state holds, whose objects' clones objs maps them to.
func (cl *cloner) state(nc *Context, nsub, sub *subContext, objs map[*object]*object) error {
	pipe := nsub.pipe
	for _, typ := range []uint32{objectBlend, objectRasterizer, objectDSA} {
		if o := objs[sub.cso[typ]]; o != nil {
			pipe.bindObject(typ, o.obj)
			nc.rebind(nsub, &nsub.cso[typ], o)
		}
	}
	for stage, o := range sub.shaders {
		o = objs[o]
		if o == nil {
			continue
		}
		// Draws bind the vertex shader's variant.
		if stage != stageVertex {
			pipe.bindShader(uint32(stage), o.obj)
		}
		nc.rebind(nsub, &nsub.shaders[stage], o)
	}
	for stage := range sub.samplers {
		for i, o := range sub.samplers[stage] {
			if o = objs[o]; o == nil {
				continue
			}
			pipe.bindSamplers(uint32(stage), uint32(i), []any{o.obj})
			nc.rebind(nsub, &nsub.samplers[stage][i], o)
		}
	}
	for stage := range sub.views {
		for i, o := range sub.views[stage] {
			if o = objs[o]; o == nil {
				continue
			}
			pipe.setSamplerViews(uint32(stage), uint32(i), []any{o.obj})
			nc.rebind(nsub, &nsub.views[stage][i], o)
		}
	}
	nsub.velems = objs[sub.velems]

	cbufs := make([]surface, sub.fbCbufs)
	var zsbuf surface
	for i, o := range sub.fb {
		o = objs[o]
		nc.rebind(nsub, &nsub.fb[i], o)
		switch {
		case o == nil:
		case i == 0:
			zsbuf = o.surf
		case i-1 < len(cbufs):
			cbufs[i-1] = o.surf
		}
	}
	if err := pipe.setFramebuffer(cbufs, zsbuf); err != nil {
		return fmt.Errorf("setting the framebuffer: %w", err)
	}
	nsub.fbCbufs, nsub.fbAttached = sub.fbCbufs, sub.fbAttached

	// What sets the state of no object or resource executes again, and
	// records itself again.
	var replays []command
	if d := sub.fbNoAttach; d != nil {
		replays = append(replays, command{cmdSetFramebufferNoAttach, d})
	}
	for i, d := range sub.viewports {
		if d != nil {
			replays = append(replays, command{cmdSetViewportState, append([]uint32{uint32(i)}, d...)})
		}
	}
	for i, d := range sub.scissors {
		if d != nil {
			replays = append(replays, command{cmdSetScissorState, append([]uint32{uint32(i)}, d...)})
		}
	}
	for stage, d := range sub.constants {
		if d != nil {
			replays = append(replays, command{cmdSetConstantBuffer, append([]uint32{uint32(stage), 0}, d...)})
		}
	}
	for cmd, d := range sub.scalars {
		replays = append(replays, command{cmd, d})
	}
	for _, r := range replays {
		if err := nc.execute(r.cmd, 0, r.payload); err != nil {
			return fmt.Errorf("command %d again: %w", r.cmd, err)
		}
	}

	for s, d := range sub.ranges {
		if err := cl.bindRange(nsub, s, sub.held[s], d); err != nil {
			return err
		}
	}
	for i := 0; i < sub.numVBs; i++ {
		vb := sub.vbs[i]
		r, err := cl.res(vb.res)
		if err != nil {
			return err
		}
		nsub.vbs[i] = vertexBuffer{res: r, stride: vb.stride, offset: vb.offset}
	}
	nsub.numVBs, nsub.vbsDirty = sub.numVBs, true
	ib, err := cl.res(sub.ib)
	if err != nil {
		return err
	}
	nsub.ib, nsub.ibSize, nsub.ibOffset = ib, sub.ibSize, sub.ibOffset

	for i, o := range sub.soTargets {
		nc.rebind(nsub, &nsub.soTargets[i], objs[o])
	}
	nsub.soCount = sub.soCount
	for o := range sub.countedQueries {
		nsub.countedQueries[objs[o]] = struct{}{}
	}
	if o := objs[sub.cond]; o != nil {
		pipe.renderCondition(o.obj, sub.condition[0]&1 != 0, sub.condition[1])
		nc.rebind(nsub, &nsub.cond, o)
		nsub.condition = sub.condition
	}
	return nil
}

// command is a command that a clone executes again.
type command struct {
	cmd     uint32
	payload []uint32
}

// bindRange binds the clone of r, the resource of the parent's slot s, to
// s of sub, of d, what the command that bound it carried but r.
func (cl *cloner) bindRange(sub *subContext, s slot, r *Resource, d []uint32) error {
	if r == nil {
		return nil
	}
	cr, err := cl.res(r)
	if err != nil {
		return err
	}
	// The slot holds a reference of its own.
	release(sub.hold(s, cr))
	cr.DecRef()
	sub.ranges[s] = append([]uint32(nil), d...)
	res := []backendResource{cr.res}
	switch s.kind {
	case slotConstantBuffer:
		err = sub.pipe.setUniformBuffer(s.stage, s.index, cr.res, d[0], d[1])
	case slotShaderBuffer:
		err = sub.pipe.setShaderBuffers(s.stage, s.index, res, d[:1], d[1:2])
	case slotAtomicBuffer:
		err = sub.pipe.setAtomicBuffers(s.index, res, d[:1], d[1:2])
	case slotShaderImage:
		image := [5]uint32{d[0], d[1], d[2], d[3]}
		err = sub.pipe.setShaderImages(s.stage, s.index, res, image[:])
	}
	if err != nil {
		return fmt.Errorf("binding resource %d to slot %+v: %w", r.id, s, err)
	}
	return nil
}
