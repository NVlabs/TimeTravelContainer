// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build cgo

package venus

import (
	"sync"
	"unsafe"

	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vk"
	"gvisor.dev/gvisor/runsc/ttc/gfxrenderer/venus/vn"
)

// commandGenerateReply is VK_COMMAND_GENERATE_REPLY_BIT_EXT: the guest waits
// for the command's reply.
const commandGenerateReply = 1

// stream executes command streams: what the Sentry submits to a context, or
// what the guest writes to a ring. Each has its own reply stream.
type stream struct {
	ctx  *Context
	ring *ring

	pool vn.Pool
	dec  *vn.Decoder
	r    *vn.Replacer

	// afterCommand, if set, is called after each command, with the offset
	// in the stream that follows it.
	afterCommand func(off int)

	encMu sync.Mutex
	enc   vn.Encoder
	// reply is the resource of the reply stream.
	reply *resource

	// nested is the resource whose stream vkExecuteCommandStreamsMESA
	// executes.
	nestedMu sync.Mutex
	nested   *resource
	inNested bool
}

func newStream(c *Context, rg *ring) *stream {
	s := &stream{ctx: c, ring: rg}
	s.dec = vn.NewDecoder(&s.pool, c)
	s.r = vn.NewReplacer(c)
	return s
}

// execute executes the commands of buf. It reports whether they executed
// without failing the context.
func (s *stream) execute(buf []byte) bool {
	s.dec.Reset(buf)
	ok := s.executeAll()
	s.dec.ResetTempPool()
	return ok
}

// executeAll executes the commands left in the decoder's stream.
func (s *stream) executeAll() bool {
	for s.dec.Remaining() > 0 {
		s.dispatch()
		if s.dec.Fatal() || s.ctx.fatal.Load() {
			s.ctx.setFatal()
			return false
		}
		if s.afterCommand != nil && !s.inNested {
			s.afterCommand(s.dec.Offset())
		}
	}
	return true
}

// dispatch executes the next command of the stream, as vn_dispatch_command
// does.
func (s *stream) dispatch() {
	off := s.dec.Offset()
	typ, flags := s.dec.DecodeCommandHeader()
	if s.dec.Fatal() {
		s.ctx.logf("a command header at offset %d of %d is truncated", off, off+s.dec.Remaining())
		return
	}
	cmd := vn.LookupCommand(typ)
	var h handler
	if cmd != nil {
		h = handlers[typ]
	}
	if h == nil {
		name := "unknown"
		if cmd != nil {
			name = cmd.Name
		}
		s.ctx.logf("unsupported command %d (%s)", typ, name)
		s.dec.SetFatal()
		return
	}
	args := cmd.DecodeArgs(s.dec)
	if s.dec.Fatal() {
		s.ctx.logf("decoding %s failed", cmd.Name)
		return
	}
	h(s, cmd, args)
	if s.dec.Fatal() || s.ctx.fatal.Load() {
		s.ctx.logf("%s failed", cmd.Name)
		return
	}
	if flags&commandGenerateReply != 0 {
		s.encodeReply(cmd, args)
	} else if cmd.CanLoseDevice() {
		if r, ok := cmd.Result(args); ok && r == vn.VK_ERROR_DEVICE_LOST {
			s.ctx.logf("%s lost the device", cmd.Name)
			s.ctx.setFatal()
		}
	}
	s.dec.ResetTempPool()
}

// fail fails the context, for what its guest did.
func (s *stream) fail(format string, args ...any) {
	s.ctx.logf(format, args...)
	s.ctx.setFatal()
	s.dec.SetFatal()
}

// encodeReply encodes a command's reply in the reply stream.
func (s *stream) encodeReply(cmd *vn.Command, args unsafe.Pointer) {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	if !s.enc.HasStream() {
		s.ctx.logf("%s wants a reply, without a reply stream", cmd.Name)
		s.ctx.setFatal()
		return
	}
	cmd.EncodeReply(&s.enc, args)
	if s.enc.Fatal() {
		s.enc.ClearFatal()
		s.ctx.logf("the reply of %s overflows the reply stream", cmd.Name)
		s.ctx.setFatal()
	}
}

// setReplyStream makes the stream reply to size bytes at offset of r.
func (s *stream) setReplyStream(r *resource, offset, size uint64) bool {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	if size > r.size || offset > r.size-size {
		s.ctx.logf("reply stream of %d bytes at %d exceeds resource %d", size, offset, r.id)
		return false
	}
	r.mem.incRef()
	if s.reply != nil {
		s.reply.mem.decRef()
	}
	s.reply = r
	s.enc.SetStream(r.mem.data[offset : offset+size])
	return true
}

// seekReplyStream moves the reply stream to pos.
func (s *stream) seekReplyStream(pos uint64) bool {
	s.encMu.Lock()
	defer s.encMu.Unlock()
	if !s.enc.Seek(pos) {
		s.ctx.logf("seeking the reply stream to %d failed", pos)
		return false
	}
	return true
}

// releaseResource stops the stream's use of r, which is going away. It
// reports false if the stream is executing r's commands.
func (s *stream) releaseResource(r *resource) bool {
	s.encMu.Lock()
	if s.reply == r {
		s.enc.SetStream(nil)
		s.reply = nil
		r.mem.decRef()
	}
	s.encMu.Unlock()
	s.nestedMu.Lock()
	defer s.nestedMu.Unlock()
	return s.nested != r
}

// release releases the stream's memory.
func (s *stream) release() {
	s.encMu.Lock()
	if s.reply != nil {
		s.reply.mem.decRef()
		s.reply = nil
	}
	s.enc.SetStream(nil)
	s.encMu.Unlock()
	s.pool.Release()
}

// executeResource executes the commands of size bytes at offset of resource
// r, in the middle of another command's execution.
func (s *stream) executeResource(r *resource, offset, size uint64) bool {
	if size > r.size || offset > r.size-size {
		s.ctx.logf("command stream of %d bytes at %d exceeds resource %d", size, offset, r.id)
		return false
	}
	s.nestedMu.Lock()
	s.nested = r
	s.nestedMu.Unlock()
	r.mem.incRef()
	s.dec.SetStream(r.mem.data[offset : offset+size])
	ok := s.executeAll()
	s.nestedMu.Lock()
	s.nested = nil
	s.nestedMu.Unlock()
	r.mem.decRef()
	return ok
}

// call calls the command with its arguments, whose handles it replaces,
// through the table of the object that its first argument is.
func (s *stream) call(cmd *vn.Command, args unsafe.Pointer) bool {
	t, q, ok := s.table(cmd, args)
	if !ok {
		s.fail("%s: no table", cmd.Name)
		return false
	}
	cmd.ReplaceHandles(s.r, args)
	var called bool
	if q != nil {
		// Vulkan requires that the use of a queue be externally
		// synchronized, and the context's stream and rings may use one at
		// once.
		q.mu.Lock()
		called = vk.Call(cmd.Type, t, args)
		q.mu.Unlock()
	} else {
		called = vk.Call(cmd.Type, t, args)
	}
	if !called {
		// The guest used a command of an extension that its device did not
		// enable, or that the host lacks.
		s.fail("%s: the host's Vulkan has no such command", cmd.Name)
	}
	return called
}

// table returns the table through which to call a command, and the queue
// that the command uses, if any.
func (s *stream) table(cmd *vn.Command, args unsafe.Pointer) (vk.Table, *queue, bool) {
	switch cmd.Level() {
	case vn.LevelGlobal:
		return s.ctx.srv.lib.Global(), nil, true
	case vn.LevelInstance:
		if o := s.ctx.lookupType(firstID(args), vn.VK_OBJECT_TYPE_INSTANCE); o != nil {
			return o.inst.table, nil, true
		}
	case vn.LevelPhysicalDevice:
		if o := s.ctx.lookupType(firstID(args), vn.VK_OBJECT_TYPE_PHYSICAL_DEVICE); o != nil {
			return o.pd.inst.pdTable, nil, true
		}
	case vn.LevelDevice:
		if o := s.ctx.lookup(firstID(args)); o != nil && o.dev != nil {
			return o.dev.table, o.queue, true
		}
	}
	return vk.Table{}, nil, false
}

// firstID returns the object ID of a command's first argument.
func firstID(args unsafe.Pointer) uint64 {
	return *(*uint64)(args)
}
