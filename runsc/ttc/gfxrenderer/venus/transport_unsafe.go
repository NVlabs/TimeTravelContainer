// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// The commands of VK_MESA_venus_protocol that carry the protocol: reply
// streams, nested command streams, and rings, as virglrenderer's
// vkr_transport.c executes them.
func registerTransport() {
	handle(vn.VK_COMMAND_TYPE_vkSetReplyCommandStreamMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		desc := (*vn.VkCommandStreamDescriptionMESA)((*vn.Args_vkSetReplyCommandStreamMESA)(args).PStream)
		r := s.ctx.resource(desc.ResourceId)
		if r == nil {
			s.fail("vkSetReplyCommandStreamMESA: no resource %d", desc.ResourceId)
			return
		}
		if !s.setReplyStream(r, desc.Offset, desc.Size) {
			s.fail("vkSetReplyCommandStreamMESA: invalid stream")
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkSeekReplyCommandStreamMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if !s.seekReplyStream((*vn.Args_vkSeekReplyCommandStreamMESA)(args).Position) {
			s.fail("vkSeekReplyCommandStreamMESA: invalid position")
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkExecuteCommandStreamsMESA_EXT, executeCommandStreams)
	handle(vn.VK_COMMAND_TYPE_vkCreateRingMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if s.ring != nil {
			s.fail("vkCreateRingMESA on a ring")
			return
		}
		a := (*vn.Args_vkCreateRingMESA)(args)
		s.ctx.createRing(s, a.Ring, (*vn.VkRingCreateInfoMESA)(a.PCreateInfo))
	})
	handle(vn.VK_COMMAND_TYPE_vkDestroyRingMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if s.ring != nil {
			s.fail("vkDestroyRingMESA on a ring")
			return
		}
		id := (*vn.Args_vkDestroyRingMESA)(args).Ring
		s.ctx.ringMu.Lock()
		rg := s.ctx.rings[id]
		delete(s.ctx.rings, id)
		s.ctx.ringMu.Unlock()
		if rg == nil {
			s.fail("vkDestroyRingMESA: no ring %d", id)
			return
		}
		rg.stop()
	})
	handle(vn.VK_COMMAND_TYPE_vkNotifyRingMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		rg := s.contextRing(cmd, (*vn.Args_vkNotifyRingMESA)(args).Ring)
		if rg != nil {
			rg.notify()
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkWriteRingExtraMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkWriteRingExtraMESA)(args)
		rg := s.contextRing(cmd, a.Ring)
		if rg != nil && !rg.writeExtra(a.Offset, a.Value) {
			s.fail("vkWriteRingExtraMESA: invalid offset %d", a.Offset)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkSubmitVirtqueueSeqnoMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkSubmitVirtqueueSeqnoMESA)(args)
		if rg := s.contextRing(cmd, a.Ring); rg != nil {
			rg.submitVirtqueueSeqno(a.Seqno)
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkWaitVirtqueueSeqnoMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		if s.ring == nil {
			s.fail("vkWaitVirtqueueSeqnoMESA off a ring")
			return
		}
		if !s.ring.waitVirtqueueSeqno((*vn.Args_vkWaitVirtqueueSeqnoMESA)(args).Seqno) {
			s.fail("vkWaitVirtqueueSeqnoMESA: the ring stopped")
		}
	})
	handle(vn.VK_COMMAND_TYPE_vkWaitRingSeqnoMESA_EXT, func(s *stream, cmd *vn.Command, args unsafe.Pointer) {
		a := (*vn.Args_vkWaitRingSeqnoMESA)(args)
		rg := s.contextRing(cmd, a.Ring)
		if rg == nil {
			return
		}
		// Wake the ring, to find an invalid wait.
		rg.notify()
		if !s.ctx.waitRingSeqno(rg, uint32(a.Seqno)) {
			s.fail("vkWaitRingSeqnoMESA failed")
		}
	})
}

// contextRing returns ring id, for a command that only the context's stream
// may execute.
func (s *stream) contextRing(cmd *vn.Command, id uint64) *ring {
	if s.ring != nil {
		s.fail("%s on a ring", cmd.Name)
		return nil
	}
	s.ctx.ringMu.Lock()
	rg := s.ctx.rings[id]
	s.ctx.ringMu.Unlock()
	if rg == nil {
		s.fail("%s: no ring %d", cmd.Name, id)
	}
	return rg
}

// executeCommandStreams executes vkExecuteCommandStreamsMESA: command
// streams in resources, which the guest records once and submits again.
func executeCommandStreams(s *stream, cmd *vn.Command, args unsafe.Pointer) {
	a := (*vn.Args_vkExecuteCommandStreamsMESA)(args)
	if a.StreamCount == 0 {
		s.fail("vkExecuteCommandStreamsMESA: no streams")
		return
	}
	if s.inNested {
		s.fail("vkExecuteCommandStreamsMESA: nested")
		return
	}
	streams := unsafe.Slice((*vn.VkCommandStreamDescriptionMESA)(a.PStreams), a.StreamCount)
	var positions []uint64
	if a.PReplyPositions != nil {
		positions = unsafe.Slice((*uint64)(a.PReplyPositions), a.StreamCount)
	}
	saved := s.dec.Save()
	s.inNested = true
	defer func() {
		s.inNested = false
		s.dec.Restore(saved)
	}()
	for i := range streams {
		desc := streams[i]
		if positions != nil && !s.seekReplyStream(positions[i]) {
			s.fail("vkExecuteCommandStreamsMESA: invalid reply position")
			return
		}
		if desc.Size == 0 {
			continue
		}
		r := s.ctx.resource(desc.ResourceId)
		if r == nil {
			s.fail("vkExecuteCommandStreamsMESA: no resource %d", desc.ResourceId)
			return
		}
		if !s.executeResource(r, desc.Offset, desc.Size) {
			return
		}
	}
}

// waitRingSeqno waits until ring rg's head reaches seqno, or the context
// fails.
func (c *Context) waitRingSeqno(rg *ring, seqno uint32) bool {
	c.waitRing.mu.Lock()
	defer c.waitRing.mu.Unlock()
	c.waitRing.id = rg.id
	c.waitRing.seqno = seqno
	for !c.fatal.Load() && !rg.isStopped() && !seqnoGE(rg.loadHead(), seqno) {
		c.waitRing.cond.Wait()
	}
	c.waitRing.id = 0
	return !c.fatal.Load() && seqnoGE(rg.loadHead(), seqno)
}

// onRingSeqno wakes the context's stream if it waits for ring id to reach
// seqno.
func (c *Context) onRingSeqno(id uint64, seqno uint32) {
	c.waitRing.mu.Lock()
	if c.waitRing.id == id && seqnoGE(seqno, c.waitRing.seqno) {
		c.waitRing.cond.Broadcast()
	}
	c.waitRing.mu.Unlock()
}

// waitedRingSeqno returns the seqno that the context's stream waits for ring
// id to reach, if it waits for that ring.
func (c *Context) waitedRingSeqno(id uint64) (uint32, bool) {
	c.waitRing.mu.Lock()
	defer c.waitRing.mu.Unlock()
	if c.waitRing.id == id {
		return c.waitRing.seqno, true
	}
	return 0, false
}

// seqnoGE reports whether seqno a is at or after b, with wraparound.
func seqnoGE(a, b uint32) bool {
	return int32(a-b) >= 0
}
