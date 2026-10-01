// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package vn

import (
	"unsafe"
)

// Level is the procedure table through which Vulkan dispatches a command.
type Level int

// Levels.
const (
	// LevelNone is a command of Venus's, which the renderer executes.
	LevelNone Level = iota
	// LevelGlobal is a command without an instance.
	LevelGlobal
	// LevelInstance is a command of an instance.
	LevelInstance
	// LevelPhysicalDevice is a command of a physical device.
	LevelPhysicalDevice
	// LevelDevice is a command of a device, or of its queues and command
	// buffers.
	LevelDevice
)

// Flags of commands.
const (
	// cmdDispatchable is a command whose first argument is a dispatchable
	// handle, which must not be null.
	cmdDispatchable = 1 << iota
	// cmdCanDeviceLost is a command that can lose its device.
	cmdCanDeviceLost
	// cmdResult is a command that returns a VkResult.
	cmdResult
	// cmdCreates is a command that returns objects, whose IDs the guest
	// chooses.
	cmdCreates
	// cmdDestroys is a command that destroys or frees objects.
	cmdDestroys
)

// Command is a command of the protocol.
type Command struct {
	// Name is the command's name, such as vkCreateBuffer.
	Name string
	// Type is the command's VkCommandTypeEXT.
	Type int32
	// ArgsSize is the size of the command's arguments, Args_<Name>.
	ArgsSize uintptr

	level   Level
	flags   uint32
	ret     uintptr
	decode  func(*Decoder, unsafe.Pointer)
	replace func(*Replacer, unsafe.Pointer)
	encode  func(*Encoder, unsafe.Pointer)
}

// LookupCommand returns the command of type typ, or nil if the protocol has
// none.
func LookupCommand(typ int32) *Command {
	if typ < 0 || typ >= CommandTableSize {
		return nil
	}
	return commandTable[typ]
}

// Level returns the procedure table through which Vulkan dispatches the
// command.
func (c *Command) Level() Level {
	return c.level
}

// DecodeArgs decodes the command's arguments from d, into memory of d's
// pool, which it returns. It marks d fatal if they are malformed.
func (c *Command) DecodeArgs(d *Decoder) unsafe.Pointer {
	args := d.AllocTemp(c.ArgsSize)
	if args == nil {
		return nil
	}
	c.decode(d, args)
	// A dispatchable handle is the object that executes the command.
	if c.flags&cmdDispatchable != 0 && *(*uint64)(args) == 0 {
		d.SetFatal()
	}
	return args
}

// ReplaceHandles replaces the object IDs that the arguments args hold with
// host handles, but for the objects that the command creates or returns.
func (c *Command) ReplaceHandles(r *Replacer, args unsafe.Pointer) {
	if c.replace != nil {
		c.replace(r, args)
	}
}

// EncodeReply encodes the command's reply.
func (c *Command) EncodeReply(e *Encoder, args unsafe.Pointer) {
	c.encode(e, args)
}

// Result returns the VkResult that the command returned, if it returns one.
func (c *Command) Result(args unsafe.Pointer) (int32, bool) {
	if c.flags&cmdResult == 0 {
		return 0, false
	}
	return *(*int32)(unsafe.Add(args, c.ret)), true
}

// SetResult sets the command's VkResult, if it returns one.
func (c *Command) SetResult(args unsafe.Pointer, result int32) {
	if c.flags&cmdResult != 0 {
		*(*int32)(unsafe.Add(args, c.ret)) = result
	}
}

// ManagesObjects reports whether the command creates, returns, destroys, or
// frees objects, which the renderer must track.
func (c *Command) ManagesObjects() bool {
	return c.flags&(cmdCreates|cmdDestroys) != 0
}

// CanLoseDevice reports whether the command can return
// VK_ERROR_DEVICE_LOST.
func (c *Command) CanLoseDevice() bool {
	return c.flags&cmdCanDeviceLost != 0
}

// Helpers of the generated code.

// The offsets of sType and pNext in a struct with them.
const (
	sTypeOffset = 0
	pNextOffset = 8
)

func getSType(p unsafe.Pointer) int32 {
	return *(*int32)(unsafe.Add(p, sTypeOffset))
}

func setSType(p unsafe.Pointer, stype int32) {
	*(*int32)(unsafe.Add(p, sTypeOffset)) = stype
}

func getPNext(p unsafe.Pointer) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Add(p, pNextOffset))
}

func setPNext(p, next unsafe.Pointer) {
	*(*unsafe.Pointer)(unsafe.Add(p, pNextOffset)) = next
}

func decode_int32_value(d *Decoder) int32 {
	var b [4]byte
	d.read(4, unsafe.Pointer(&b), 4)
	return int32(le.Uint32(b[:]))
}

func decode_uint32_value(d *Decoder) uint32 {
	var b [4]byte
	d.read(4, unsafe.Pointer(&b), 4)
	return le.Uint32(b[:])
}

func encode_int32_value(e *Encoder, v int32) {
	encode_uint32_value(e, uint32(v))
}

func encode_uint32_value(e *Encoder, v uint32) {
	if 4 > len(e.buf)-e.off {
		e.fatal = true
		return
	}
	le.PutUint32(e.buf[e.off:], v)
	e.off += 4
}

// SetFatal marks the reply as failed.
func (e *Encoder) SetFatal() {
	e.fatal = true
}

func decode_handle(d *Decoder, val unsafe.Pointer) {
	decode_uint64_t(d, val)
}

func decode_handle_lookup(d *Decoder, val unsafe.Pointer, typ int32) {
	decode_uint64_t(d, val)
	d.lookupObject(*(*uint64)(val), typ)
}

func load_uint32_t(p unsafe.Pointer) uint64 {
	if p == nil {
		return 0
	}
	return uint64(*(*uint32)(p))
}

func load_size_t(p unsafe.Pointer) uint64 {
	if p == nil {
		return 0
	}
	return *(*uint64)(p)
}

func load_uint64_t(p unsafe.Pointer) uint64 {
	return load_size_t(p)
}
