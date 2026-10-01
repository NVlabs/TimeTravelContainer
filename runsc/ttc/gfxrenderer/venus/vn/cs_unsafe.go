// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package vn is the renderer side of the Venus protocol, which Mesa's Venus
// Vulkan driver speaks: it decodes the commands of a command stream into the
// C structs that Vulkan takes, replaces their object IDs with host handles,
// dispatches them, and encodes their replies.
//
// Most of the package is generated from venus-protocol's model of the
// protocol by tools/venusgen. The decoded structs live in a Pool of memory
// outside Go's heap, so that they pass to Vulkan as they are.
package vn

import (
	"encoding/binary"
	"math"
	"unsafe"
)

// Handle is a Vulkan handle, dispatchable or not: an object ID as decoded,
// and a host handle once replaced.
type Handle uint64

// ObjectTable is the objects of a context, by the IDs that the guest gave
// them.
type ObjectTable interface {
	// HasObject reports whether the object id exists and has type typ, a
	// VkObjectType.
	HasObject(id uint64, typ int32) bool
	// HostHandle returns the host handle of the object id of type typ.
	HostHandle(id uint64, typ int32) Handle
}

// Decoder decodes a command stream.
type Decoder struct {
	buf   []byte
	off   int
	fatal bool
	pool  *Pool
	table ObjectTable
}

// NewDecoder returns a decoder that allocates decoded data from pool and
// looks up objects in table.
func NewDecoder(pool *Pool, table ObjectTable) *Decoder {
	return &Decoder{pool: pool, table: table}
}

// Reset makes the decoder decode buf, from its start.
func (d *Decoder) Reset(buf []byte) {
	d.buf = buf
	d.off = 0
	d.fatal = false
}

// Remaining returns the number of bytes left to decode.
func (d *Decoder) Remaining() int {
	return len(d.buf) - d.off
}

// Offset returns the offset of the next byte to decode.
func (d *Decoder) Offset() int {
	return d.off
}

// SetFatal marks the stream as malformed; decoding stops at the end of the
// current command.
func (d *Decoder) SetFatal() {
	d.fatal = true
}

// Fatal reports whether the stream is malformed.
func (d *Decoder) Fatal() bool {
	return d.fatal
}

// read consumes size bytes, of which it copies the first valSize to val.
// size is a multiple of 4.
func (d *Decoder) read(size uint64, val unsafe.Pointer, valSize uint64) {
	if size > uint64(len(d.buf)-d.off) {
		d.fatal = true
		if valSize != 0 {
			clear(unsafe.Slice((*byte)(val), valSize))
		}
		return
	}
	if valSize != 0 {
		copy(unsafe.Slice((*byte)(val), valSize), d.buf[d.off:])
	}
	d.off += int(size)
}

// peek copies valSize bytes to val without consuming them.
func (d *Decoder) peek(size uint64, val unsafe.Pointer, valSize uint64) {
	if size > uint64(len(d.buf)-d.off) {
		d.fatal = true
		clear(unsafe.Slice((*byte)(val), valSize))
		return
	}
	copy(unsafe.Slice((*byte)(val), valSize), d.buf[d.off:])
}

// AllocTemp allocates size bytes, zeroed, until ResetTempPool.
func (d *Decoder) AllocTemp(size uintptr) unsafe.Pointer {
	p := d.pool.alloc(size)
	if p == nil {
		d.fatal = true
	}
	return p
}

// AllocTempArray allocates count elements of size bytes each.
func (d *Decoder) AllocTempArray(size uintptr, count uint64) unsafe.Pointer {
	if count != 0 && uint64(size) > math.MaxInt32/count {
		d.fatal = true
		return nil
	}
	return d.AllocTemp(size * uintptr(count))
}

// ResetTempPool frees what AllocTemp allocated.
func (d *Decoder) ResetTempPool() {
	d.pool.reset()
}

// lookupObject checks that object id exists with type typ, or is 0.
func (d *Decoder) lookupObject(id uint64, typ int32) {
	if id != 0 && !d.table.HasObject(id, typ) {
		d.fatal = true
	}
}

// DecoderState is where a decoder is in its stream, which a command that
// executes other streams saves.
type DecoderState struct {
	buf  []byte
	off  int
	mark poolMark
}

// Save returns the decoder's state, and keeps what its pool holds until
// Restore.
func (d *Decoder) Save() DecoderState {
	s := DecoderState{buf: d.buf, off: d.off}
	s.mark = d.pool.setMark()
	return s
}

// Restore restores a state that Save returned.
func (d *Decoder) Restore(s DecoderState) {
	d.buf = s.buf
	d.off = s.off
	d.pool.restoreMark(s.mark)
}

// SetStream makes the decoder decode buf, from its start, keeping its
// fatal state.
func (d *Decoder) SetStream(buf []byte) {
	d.buf = buf
	d.off = 0
}

// Encoder encodes replies into a reply stream.
type Encoder struct {
	buf   []byte
	off   int
	fatal bool
}

// SetStream makes the encoder write to buf, from its start, or to nothing
// if buf is nil.
func (e *Encoder) SetStream(buf []byte) {
	e.buf = buf
	e.off = 0
}

// Seek moves the encoder to offset pos of its stream. It reports whether
// the stream has the offset.
func (e *Encoder) Seek(pos uint64) bool {
	if e.buf == nil || pos > uint64(len(e.buf)) {
		return false
	}
	e.off = int(pos)
	return true
}

// HasStream reports whether the encoder has a stream.
func (e *Encoder) HasStream() bool {
	return e.buf != nil
}

// ClearFatal clears the encoder's failure.
func (e *Encoder) ClearFatal() {
	e.fatal = false
}

// Offset returns the offset of the next byte to write.
func (e *Encoder) Offset() int {
	return e.off
}

// Fatal reports whether a reply did not fit.
func (e *Encoder) Fatal() bool {
	return e.fatal
}

// write writes size bytes: valSize bytes of val, then zeros.
func (e *Encoder) write(size uint64, val unsafe.Pointer, valSize uint64) {
	if size > uint64(len(e.buf)-e.off) {
		e.fatal = true
		return
	}
	dst := e.buf[e.off : e.off+int(size)]
	n := 0
	if valSize != 0 {
		n = copy(dst, unsafe.Slice((*byte)(val), valSize))
	}
	clear(dst[n:])
	e.off += int(size)
}

// Replacer replaces object IDs with host handles.
type Replacer struct {
	table ObjectTable
}

// NewReplacer returns a Replacer of the objects of table.
func NewReplacer(table ObjectTable) *Replacer {
	return &Replacer{table: table}
}

func (r *Replacer) hostHandle(val unsafe.Pointer, typ int32) {
	id := *(*uint64)(val)
	if id != 0 {
		*(*Handle)(val) = r.table.HostHandle(id, typ)
	}
}

// align4 rounds n up to a multiple of 4.
func align4(n uint64) uint64 {
	return (n + 3) &^ 3
}

// elem returns the address of element i of the array at p, whose elements
// are size bytes.
func elem(p unsafe.Pointer, i uint64, size uintptr) unsafe.Pointer {
	return unsafe.Add(p, uintptr(i)*size)
}

// loadPointer returns the pointer stored at p.
func loadPointer(p unsafe.Pointer) unsafe.Pointer {
	return *(*unsafe.Pointer)(p)
}

// storePointer stores v at p.
func storePointer(p, v unsafe.Pointer) {
	*(*unsafe.Pointer)(p) = v
}

// Scalars. Each is padded to 4 bytes.

func decode_uint8_t(d *Decoder, val unsafe.Pointer)  { d.read(4, val, 1) }
func decode_uint16_t(d *Decoder, val unsafe.Pointer) { d.read(4, val, 2) }
func decode_uint32_t(d *Decoder, val unsafe.Pointer) { d.read(4, val, 4) }
func decode_int32_t(d *Decoder, val unsafe.Pointer)  { d.read(4, val, 4) }
func decode_float(d *Decoder, val unsafe.Pointer)    { d.read(4, val, 4) }
func decode_uint64_t(d *Decoder, val unsafe.Pointer) { d.read(8, val, 8) }
func decode_int64_t(d *Decoder, val unsafe.Pointer)  { d.read(8, val, 8) }
func decode_double(d *Decoder, val unsafe.Pointer)   { d.read(8, val, 8) }
func decode_size_t(d *Decoder, val unsafe.Pointer)   { d.read(8, val, 8) }

func encode_uint8_t(e *Encoder, val unsafe.Pointer)  { e.write(4, val, 1) }
func encode_uint16_t(e *Encoder, val unsafe.Pointer) { e.write(4, val, 2) }
func encode_uint32_t(e *Encoder, val unsafe.Pointer) { e.write(4, val, 4) }
func encode_int32_t(e *Encoder, val unsafe.Pointer)  { e.write(4, val, 4) }
func encode_float(e *Encoder, val unsafe.Pointer)    { e.write(4, val, 4) }
func encode_uint64_t(e *Encoder, val unsafe.Pointer) { e.write(8, val, 8) }
func encode_int64_t(e *Encoder, val unsafe.Pointer)  { e.write(8, val, 8) }
func encode_double(e *Encoder, val unsafe.Pointer)   { e.write(8, val, 8) }
func encode_size_t(e *Encoder, val unsafe.Pointer)   { e.write(8, val, 8) }

// arrayBytes returns count*size, or false if it overflows.
func arrayBytes(count uint64, size uint64) (uint64, bool) {
	if count != 0 && size > math.MaxInt32/count {
		return 0, false
	}
	return count * size, true
}

func (d *Decoder) readArray(val unsafe.Pointer, count uint64, size uint64) {
	n, ok := arrayBytes(count, size)
	if !ok {
		d.fatal = true
		return
	}
	d.read(align4(n), val, n)
}

func (e *Encoder) writeArray(val unsafe.Pointer, count uint64, size uint64) {
	n, ok := arrayBytes(count, size)
	if !ok {
		e.fatal = true
		return
	}
	e.write(align4(n), val, n)
}

func decode_uint8_t_array(d *Decoder, val unsafe.Pointer, count uint64)  { d.readArray(val, count, 1) }
func decode_uint16_t_array(d *Decoder, val unsafe.Pointer, count uint64) { d.readArray(val, count, 2) }
func decode_uint32_t_array(d *Decoder, val unsafe.Pointer, count uint64) { d.readArray(val, count, 4) }
func decode_int32_t_array(d *Decoder, val unsafe.Pointer, count uint64)  { d.readArray(val, count, 4) }
func decode_float_array(d *Decoder, val unsafe.Pointer, count uint64)    { d.readArray(val, count, 4) }
func decode_uint64_t_array(d *Decoder, val unsafe.Pointer, count uint64) { d.readArray(val, count, 8) }
func decode_int64_t_array(d *Decoder, val unsafe.Pointer, count uint64)  { d.readArray(val, count, 8) }
func decode_double_array(d *Decoder, val unsafe.Pointer, count uint64)   { d.readArray(val, count, 8) }
func decode_size_t_array(d *Decoder, val unsafe.Pointer, count uint64)   { d.readArray(val, count, 8) }

func encode_uint8_t_array(e *Encoder, val unsafe.Pointer, count uint64)  { e.writeArray(val, count, 1) }
func encode_uint16_t_array(e *Encoder, val unsafe.Pointer, count uint64) { e.writeArray(val, count, 2) }
func encode_uint32_t_array(e *Encoder, val unsafe.Pointer, count uint64) { e.writeArray(val, count, 4) }
func encode_int32_t_array(e *Encoder, val unsafe.Pointer, count uint64)  { e.writeArray(val, count, 4) }
func encode_float_array(e *Encoder, val unsafe.Pointer, count uint64)    { e.writeArray(val, count, 4) }
func encode_uint64_t_array(e *Encoder, val unsafe.Pointer, count uint64) { e.writeArray(val, count, 8) }
func encode_int64_t_array(e *Encoder, val unsafe.Pointer, count uint64)  { e.writeArray(val, count, 8) }
func encode_double_array(e *Encoder, val unsafe.Pointer, count uint64)   { e.writeArray(val, count, 8) }
func encode_size_t_array(e *Encoder, val unsafe.Pointer, count uint64)   { e.writeArray(val, count, 8) }

// Blobs, and strings, which are blobs that include their terminating NUL.

func decode_blob_array(d *Decoder, val unsafe.Pointer, size uint64) { d.read(align4(size), val, size) }
func encode_blob_array(e *Encoder, val unsafe.Pointer, size uint64) { e.write(align4(size), val, size) }

func decode_char_array(d *Decoder, val unsafe.Pointer, size uint64) {
	decode_blob_array(d, val, size)
	if size == 0 {
		d.fatal = true
		return
	}
	*(*byte)(unsafe.Add(val, size-1)) = 0
}

func encode_char_array(e *Encoder, val unsafe.Pointer, size uint64) {
	encode_blob_array(e, val, size)
}

// cstrlen returns the length of the C string at p.
func cstrlen(p unsafe.Pointer) uint64 {
	n := uint64(0)
	for *(*byte)(unsafe.Add(p, n)) != 0 {
		n++
	}
	return n
}

// Array sizes and pointers. A pointer is an array of 0 or 1 elements.

func decode_array_size(d *Decoder, expected uint64) uint64 {
	var size uint64
	decode_uint64_t(d, unsafe.Pointer(&size))
	if size != expected {
		d.fatal = true
		size = 0
	}
	return size
}

func decode_array_size_unchecked(d *Decoder) uint64 {
	var size uint64
	decode_uint64_t(d, unsafe.Pointer(&size))
	return size
}

func peek_array_size(d *Decoder) uint64 {
	var size uint64
	d.peek(8, unsafe.Pointer(&size), 8)
	return size
}

func decode_simple_pointer(d *Decoder) bool {
	return decode_array_size_unchecked(d) != 0
}

func encode_array_size(e *Encoder, size uint64) {
	encode_uint64_t(e, unsafe.Pointer(&size))
}

func encode_simple_pointer(e *Encoder, val unsafe.Pointer) bool {
	if val == nil {
		encode_array_size(e, 0)
		return false
	}
	encode_array_size(e, 1)
	return true
}

// le is the byte order of the protocol, and of the hosts it runs on.
var le = binary.LittleEndian

// DecodeCommandHeader decodes a command's type and flags.
func (d *Decoder) DecodeCommandHeader() (typ int32, flags uint32) {
	decode_int32_t(d, unsafe.Pointer(&typ))
	decode_uint32_t(d, unsafe.Pointer(&flags))
	return typ, flags
}

// EncodeCommandType encodes a reply's command type.
func (e *Encoder) EncodeCommandType(typ int32) {
	encode_int32_t(e, unsafe.Pointer(&typ))
}
