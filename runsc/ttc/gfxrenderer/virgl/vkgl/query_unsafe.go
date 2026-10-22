// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package vkgl

import (
	"errors"
	"fmt"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// The kinds of queries that contexts answer.
const (
	// QueryOcclusion counts the samples that pass the depth and stencil
	// tests between its beginning and its end.
	QueryOcclusion = iota
	// QueryTimestamp is the GPU's time at its end, in nanoseconds.
	QueryTimestamp
	// QueryTimeElapsed is the time between its beginning and its end, in
	// nanoseconds.
	QueryTimeElapsed
	// QueryFinished is whether the GPU finished the commands before its
	// end: its result is ready once it did.
	QueryFinished
)

// Query is a query of a context's. The context answers it with Vulkan
// queries of its own, whose results it adds up as the GPU finishes their
// batches: an occlusion query counts in the occlusion queries that the
// context begins for its active occlusion queries, one at a time, as
// Vulkan has no two of a type active in a command buffer, and none active
// across command buffers.
type Query struct {
	Kind int
	// gen counts the query's beginnings: what the Vulkan queries of
	// earlier ones count is not its.
	gen uint64
	// active is whether an occlusion query counts, and ended whether the
	// query ended since it began.
	active, ended bool
	// pending is the number of the query's Vulkan queries whose results
	// the context has yet to add up.
	pending int
	// count is the sum of an occlusion query's results, and begin and end
	// the timestamps of a query's beginning and end.
	count, begin, end uint64
	// lost is whether the GPU executed none of a batch of the query's.
	lost bool
}

// NewQuery returns a query of kind, one of Query*.
func (d *Device) NewQuery(kind int) (*Query, error) {
	switch kind {
	case QueryOcclusion:
	case QueryTimestamp, QueryTimeElapsed, QueryFinished:
		if d.timestampBits == 0 {
			return nil, errors.New("a query of time of a queue of no timestamps")
		}
	default:
		return nil, fmt.Errorf("a query of kind %d", kind)
	}
	return &Query{Kind: kind}, nil
}

// queryPoolSize is the number of the Vulkan queries of a pool of a
// context's.
const queryPoolSize = 64

// queryPools are a context's Vulkan queries of a type, in pools of
// queryPoolSize, which a slot numbers, and the slots that are free.
type queryPools struct {
	typ   uint32
	pools []vn.Handle
	free  []uint32
}

// at returns the pool, and its query, of slot.
func (p *queryPools) at(slot uint32) (vn.Handle, uint32) {
	return p.pools[slot/queryPoolSize], slot % queryPoolSize
}

// Uses of a Vulkan query's result, of queryUse's.
const (
	useCount = iota
	useBegin
	useEnd
)

// queryUse is a use of a Vulkan query's result: as field, a use*, of q's
// beginning gen.
type queryUse struct {
	q     *Query
	gen   uint64
	field int
}

// vkQuery is a Vulkan query of a batch's, of a slot of pools, and the uses
// of its result.
type vkQuery struct {
	pools *queryPools
	slot  uint32
	uses  []queryUse
}

// allocQuery returns a free slot of p, of a new pool if p has none.
func (c *Context) allocQuery(p *queryPools) (uint32, error) {
	if len(p.free) == 0 {
		d := c.d
		pool, err := func() (vn.Handle, error) {
			d.mu.Lock()
			defer d.mu.Unlock()
			a := d.a
			defer a.reset()
			info := get[vn.VkQueryPoolCreateInfo](a)
			info.SType = vn.VK_STRUCTURE_TYPE_QUERY_POOL_CREATE_INFO
			info.QueryType, info.QueryCount = p.typ, queryPoolSize
			h := get[vn.Handle](a)
			cq := get[vn.Args_vkCreateQueryPool](a)
			cq.Device, cq.PCreateInfo, cq.PQueryPool = d.dev, unsafe.Pointer(info), unsafe.Pointer(h)
			vk.Call(vn.VK_COMMAND_TYPE_vkCreateQueryPool_EXT, d.table, unsafe.Pointer(cq))
			return *h, check("vkCreateQueryPool", cq.Ret)
		}()
		if err != nil {
			return 0, err
		}
		base := uint32(len(p.pools)) * queryPoolSize
		p.pools = append(p.pools, pool)
		for i := uint32(queryPoolSize); i > 0; i-- {
			p.free = append(p.free, base+i-1)
		}
	}
	slot := p.free[len(p.free)-1]
	p.free = p.free[:len(p.free)-1]
	return slot, nil
}

// destroyQueryPools destroys the pools of p, of no batch's queries.
func (c *Context) destroyQueryPools(p *queryPools) {
	for _, pool := range p.pools {
		dq := get[vn.Args_vkDestroyQueryPool](c.a)
		dq.Device, dq.QueryPool = c.d.dev, pool
		vk.Call(vn.VK_COMMAND_TYPE_vkDestroyQueryPool_EXT, c.d.table, unsafe.Pointer(dq))
	}
	p.pools, p.free = nil, nil
}

// resetQuery resets a Vulkan query, of no rendering instance.
func (c *Commands) resetQuery(pool vn.Handle, index uint32) {
	rq := get[vn.Args_vkCmdResetQueryPool](c.a)
	rq.QueryPool, rq.FirstQuery, rq.QueryCount = pool, index, 1
	c.call(vn.VK_COMMAND_TYPE_vkCmdResetQueryPool_EXT, unsafe.Pointer(rq))
}

// openSegment begins an occlusion query of the batch, of cmds of no
// rendering instance, that counts for the active occlusion queries, if
// there are any and it has none.
func (c *Context) openSegment(cmds *Commands) {
	if len(c.active) == 0 || c.segment != nil {
		return
	}
	slot, err := c.allocQuery(&c.occlusion)
	if err != nil {
		for _, q := range c.active {
			q.lost = true
		}
		return
	}
	pool, index := c.occlusion.at(slot)
	cmds.resetQuery(pool, index)
	bq := get[vn.Args_vkCmdBeginQuery](c.a)
	bq.QueryPool, bq.Query = pool, index
	if c.d.features10.OcclusionQueryPrecise != 0 {
		bq.Flags = 0x1 // VK_QUERY_CONTROL_PRECISE_BIT
	}
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdBeginQuery_EXT, unsafe.Pointer(bq))
	vq := &vkQuery{pools: &c.occlusion, slot: slot}
	for _, q := range c.active {
		vq.uses = append(vq.uses, queryUse{q: q, gen: q.gen, field: useCount})
		q.pending++
	}
	c.segment = vq
}

// closeSegment ends the batch's occlusion query, if it has one, of cmds of
// no rendering instance.
func (c *Context) closeSegment(cmds *Commands, b *batch) {
	vq := c.segment
	if vq == nil {
		return
	}
	pool, index := vq.pools.at(vq.slot)
	eq := get[vn.Args_vkCmdEndQuery](c.a)
	eq.QueryPool, eq.Query = pool, index
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdEndQuery_EXT, unsafe.Pointer(eq))
	b.queries = append(b.queries, vq)
	c.segment = nil
}

// timestamp writes the GPU's time, once it finished the batch's earlier
// commands, of cmds of no rendering instance, as field of q.
func (c *Context) timestamp(cmds *Commands, q *Query, field int) {
	slot, err := c.allocQuery(&c.timestamps)
	if err != nil {
		q.lost = true
		return
	}
	pool, index := c.timestamps.at(slot)
	cmds.resetQuery(pool, index)
	wt := get[vn.Args_vkCmdWriteTimestamp](c.a)
	wt.PipelineStage, wt.QueryPool, wt.Query = StageBottomOfPipe, pool, index
	cmds.call(vn.VK_COMMAND_TYPE_vkCmdWriteTimestamp_EXT, unsafe.Pointer(wt))
	c.batch.queries = append(c.batch.queries, &vkQuery{pools: &c.timestamps, slot: slot,
		uses: []queryUse{{q: q, gen: q.gen, field: field}}})
	q.pending++
}

// restart has q's results of earlier beginnings no longer count.
func (q *Query) restart() {
	q.gen++
	q.ended, q.pending, q.lost = false, 0, false
	q.count, q.begin, q.end = 0, 0, 0
}

// deactivate has an occlusion query no longer count.
func (c *Context) deactivate(q *Query) {
	for i, aq := range c.active {
		if aq == q {
			c.active = append(c.active[:i], c.active[i+1:]...)
			break
		}
	}
	q.active = false
}

// BeginQuery begins q, which counts, or times, the context's commands from
// then on.
func (c *Context) BeginQuery(q *Query) error {
	defer c.a.reset()
	cmds, err := c.outsideRendering()
	if err != nil {
		return err
	}
	switch q.Kind {
	case QueryOcclusion:
		c.closeSegment(cmds, c.batch)
		c.deactivate(q)
		q.restart()
		q.active = true
		c.active = append(c.active, q)
		c.openSegment(cmds)
	case QueryTimeElapsed:
		q.restart()
		c.timestamp(cmds, q, useBegin)
	}
	return nil
}

// EndQuery ends q: its result is of the context's commands until then.
func (c *Context) EndQuery(q *Query) error {
	defer c.a.reset()
	cmds, err := c.outsideRendering()
	if err != nil {
		return err
	}
	switch q.Kind {
	case QueryOcclusion:
		if q.active {
			c.closeSegment(cmds, c.batch)
			c.deactivate(q)
			c.openSegment(cmds)
		}
	case QueryTimestamp, QueryFinished:
		// Which no beginning restarts.
		q.restart()
		c.timestamp(cmds, q, useEnd)
	case QueryTimeElapsed:
		c.timestamp(cmds, q, useEnd)
	}
	q.ended = true
	return nil
}

// DestroyQuery lets q go: what the context counts is no longer its.
func (c *Context) DestroyQuery(q *Query) {
	c.deactivate(q)
	q.restart()
}

// settle adds up the results of the Vulkan queries of b, which the GPU is
// done with, or never executed, and frees them.
func (c *Context) settle(b *batch) {
	a := c.a
	for _, vq := range b.queries {
		var value uint64
		lost := b.dropped
		if !lost {
			pool, index := vq.pools.at(vq.slot)
			gr := get[vn.Args_vkGetQueryPoolResults](a)
			gr.Device, gr.QueryPool, gr.FirstQuery, gr.QueryCount = c.d.dev, pool, index, 1
			gr.DataSize, gr.PData, gr.Stride = 8, unsafe.Pointer(&value), 8
			gr.Flags = 0x1 | 0x2 // VK_QUERY_RESULT_64_BIT, VK_QUERY_RESULT_WAIT_BIT
			vk.Call(vn.VK_COMMAND_TYPE_vkGetQueryPoolResults_EXT, c.d.table, unsafe.Pointer(gr))
			lost = gr.Ret != vn.VK_SUCCESS
		}
		for _, u := range vq.uses {
			q := u.q
			if u.gen != q.gen {
				continue
			}
			q.pending--
			q.lost = q.lost || lost
			switch u.field {
			case useCount:
				q.count += value
			case useBegin:
				q.begin = value
			case useEnd:
				q.end = value
			}
		}
		vq.pools.free = append(vq.pools.free, vq.slot)
	}
	b.queries = nil
}

// QueryResult returns q's result, and whether it has it: once the GPU
// finished the commands until q's end, which, if wait, it waits for.
func (c *Context) QueryResult(q *Query, wait bool) (uint64, bool) {
	if !q.ended {
		return 0, false
	}
	c.retire()
	if q.pending != 0 {
		if !wait {
			return 0, false
		}
		c.Flush()
		for q.pending != 0 && len(c.inflight) != 0 {
			c.waitOldest()
		}
	}
	if q.lost || q.pending != 0 {
		// Of a lost device's batches: of nothing counted.
		return 0, true
	}
	switch q.Kind {
	case QueryTimestamp:
		return c.d.nanoseconds(q.end), true
	case QueryTimeElapsed:
		return c.d.nanoseconds(q.end - q.begin), true
	case QueryFinished:
		return 1, true
	}
	return q.count, true
}

// nanoseconds returns the nanoseconds of ticks of the queue's timestamps,
// of their valid bits.
func (d *Device) nanoseconds(ticks uint64) uint64 {
	if d.timestampBits < 64 {
		ticks &= 1<<d.timestampBits - 1
	}
	if period := d.props.Limits.TimestampPeriod; period != 1 {
		return uint64(float64(ticks) * float64(period))
	}
	return ticks
}
