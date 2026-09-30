// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package gfxproto is the protocol between the Sentry's virtual GPU (see
// pkg/sentry/devices/virtgpu) and its renderer, a host process that renders
// what the GPU's contexts submit.
//
// The two exchange messages over a connected Unix stream socket. A message is
// a header, a payload of fixed-size fields, and a trailer of bytes: a
// context's name, or a command buffer. A message may carry file descriptors,
// such as the shared memory of a resource.
//
// The renderer handles the Sentry's messages in order. It answers the
// requests that return something, and signals fences, as they complete.
package gfxproto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/unet"
)

// Version is the protocol's version. A renderer says which it speaks in its
// Hello.
const Version = 1

// MsgType is the type of a message.
type MsgType uint32

// Messages from the Sentry to the renderer.
const (
	MsgCreateContext MsgType = iota + 1
	MsgDestroyContext
	MsgCreateResource
	MsgCreateBlob
	MsgMapResource
	MsgAttachResource
	MsgDetachResource
	MsgDestroyResource
	MsgSubmitCommand
	MsgTransfer
	MsgCreateFence
	MsgSnapshot
	MsgRestore
)

// Messages from the renderer to the Sentry.
const (
	MsgHello MsgType = iota + 100
	MsgReply
	MsgFenceSignaled
)

// Messages from the Sentry to the renderers' broker, which starts renderers
// for it. The broker answers with Reply.
const (
	MsgStartRenderer MsgType = iota + 200
)

// Messages from ttcd to the renderers' launcher, which starts renderers for
// a daemon whose sandbox would bar them from the GPU. The launcher answers
// with Reply.
const (
	MsgLaunchRenderer MsgType = iota + 300
	MsgLaunchProbe
)

// String implements fmt.Stringer.String.
func (t MsgType) String() string {
	switch t {
	case MsgCreateContext:
		return "CreateContext"
	case MsgDestroyContext:
		return "DestroyContext"
	case MsgCreateResource:
		return "CreateResource"
	case MsgCreateBlob:
		return "CreateBlob"
	case MsgMapResource:
		return "MapResource"
	case MsgAttachResource:
		return "AttachResource"
	case MsgDetachResource:
		return "DetachResource"
	case MsgDestroyResource:
		return "DestroyResource"
	case MsgSubmitCommand:
		return "SubmitCommand"
	case MsgTransfer:
		return "Transfer"
	case MsgCreateFence:
		return "CreateFence"
	case MsgSnapshot:
		return "Snapshot"
	case MsgRestore:
		return "Restore"
	case MsgHello:
		return "Hello"
	case MsgReply:
		return "Reply"
	case MsgFenceSignaled:
		return "FenceSignaled"
	case MsgStartRenderer:
		return "StartRenderer"
	case MsgLaunchRenderer:
		return "LaunchRenderer"
	case MsgLaunchProbe:
		return "LaunchProbe"
	default:
		return fmt.Sprintf("MsgType(%d)", uint32(t))
	}
}

// The payloads of messages. Each is followed by the message's trailer, if it
// has one.
type (
	// CreateContext creates a context. Its trailer is the context's name.
	// The renderer replies.
	CreateContext struct {
		CtxID    uint32
		CapsetID uint32
	}

	// DestroyContext destroys a context.
	DestroyContext struct {
		CtxID uint32
	}

	// CreateResource creates a classic resource, whose backing the renderer
	// allocates.
	CreateResource struct {
		CtxID     uint32
		ResID     uint32
		Target    uint32
		Format    uint32
		Bind      uint32
		Width     uint32
		Height    uint32
		Depth     uint32
		ArraySize uint32
		LastLevel uint32
		NrSamples uint32
		Flags     uint32
		Size      uint64
	}

	// CreateBlob creates a blob resource.
	CreateBlob struct {
		CtxID  uint32
		ResID  uint32
		Mem    uint32
		Flags  uint32
		BlobID uint64
		Size   uint64
	}

	// MapResource asks for the memory of a resource. The renderer replies
	// with a file descriptor of shared memory, whose first Reply.Size bytes
	// are the resource's.
	MapResource struct {
		ResID uint32
	}

	// AttachResource lets a context use a resource.
	AttachResource struct {
		CtxID uint32
		ResID uint32
	}

	// DetachResource ends a context's use of a resource.
	DetachResource struct {
		CtxID uint32
		ResID uint32
	}

	// DestroyResource destroys a resource.
	DestroyResource struct {
		ResID uint32
	}

	// SubmitCommand submits a command buffer, its trailer, to a context.
	SubmitCommand struct {
		CtxID uint32
	}

	// Transfer copies between a classic resource's backing and the renderer's
	// copy of it.
	Transfer struct {
		CtxID       uint32
		ResID       uint32
		ToHost      uint32
		X, Y, Z     uint32
		W, H, D     uint32
		Level       uint32
		Offset      uint32
		Stride      uint32
		LayerStride uint32
	}

	// CreateFence asks the renderer to signal a fence once what was
	// submitted to its timeline before it is done: ring Ring of context
	// CtxID if HasRing is 1, or the global timeline.
	CreateFence struct {
		FenceID uint64
		CtxID   uint32
		HasRing uint32
		Ring    uint32
	}

	// Hello is the renderer's first message: the protocol version it speaks,
	// and whether it started. Its trailer is an error message, if it did not.
	Hello struct {
		Version uint32
		Errno   uint32
	}

	// Reply answers a request, whose sequence number it has. Errno is 0 or
	// the request's error.
	Reply struct {
		Errno   uint32
		MapInfo uint32
		Size    uint64
	}

	// FenceSignaled reports a signaled fence, which signals every fence
	// created before it on its timeline too.
	FenceSignaled struct {
		FenceID uint64
		CtxID   uint32
		HasRing uint32
		Ring    uint32
	}

	// StartRenderer asks the broker to start a renderer, with Flags,
	// RendererFlag*. The broker replies with the descriptor of a connection
	// to it, a Unix stream socket, on which the renderer sends Hello.
	StartRenderer struct {
		Flags uint32
	}

	// LaunchRenderer asks the launcher to start a renderer, with Flags,
	// RendererFlag*, whose connection to its Sentry is the message's
	// descriptor. The reply's trailer says why it failed, if it did.
	LaunchRenderer struct {
		Flags uint32
	}

	// Snapshot asks the renderer for a snapshot of its state. The reply's
	// descriptor is shared memory that holds the snapshot, of the reply's
	// Size bytes.
	Snapshot struct {
		Reserved uint32
	}

	// Restore restores a renderer, which has no contexts or resources, from
	// a snapshot, of Size bytes in the message's descriptor. The renderer
	// replies.
	Restore struct {
		Size uint64
	}

	// LaunchProbe asks the launcher for the capability sets of a renderer
	// with Flags, RendererFlag*. The reply's trailer is what the renderer's
	// probe printed, or why it failed.
	LaunchProbe struct {
		Flags uint32
	}
)

// RendererFlagSnapshots is a flag of StartRenderer and LaunchRenderer: the
// renderer takes snapshots and restores them, which costs it memory from its
// start, since it records how to rebuild its Vulkan objects.
const RendererFlagSnapshots = 1 << 0

const (
	// headerSize is the size of a message's header: its type, the length of
	// its payload and trailer, and its sequence number.
	headerSize = 16

	// MaxMessageSize is the size of the largest message.
	MaxMessageSize = 64 << 20

	// maxFDs is the most file descriptors that one message carries.
	maxFDs = 4
)

// Message is a received message.
type Message struct {
	Type MsgType
	// Seq is the sequence number of a request, or of the request that a
	// reply answers.
	Seq uint64
	// Payload is the message's payload and trailer.
	Payload []byte
	// FDs are the file descriptors that the message carried. The receiver
	// owns them.
	FDs []int
}

// Decode decodes the message's payload into p, a pointer to one of the
// payload types, and returns the trailer.
func (m *Message) Decode(p any) ([]byte, error) {
	n, err := binary.Decode(m.Payload, binary.LittleEndian, p)
	if err != nil {
		return nil, fmt.Errorf("decoding %v: %w", m.Type, err)
	}
	return m.Payload[n:], nil
}

// CloseFDs closes the message's file descriptors.
func (m *Message) CloseFDs() {
	for _, fd := range m.FDs {
		unix.Close(fd)
	}
	m.FDs = nil
}

// Conn is an end of a connection between the Sentry and a renderer. Its
// methods may be called concurrently, but only one goroutine receives.
type Conn struct {
	sock *unet.Socket

	writeMu sync.Mutex
}

// socketBufferSize is the size of a connection's socket buffers. macOS's
// default is a few KiB, which a command buffer outgrows.
const socketBufferSize = 1 << 20

// NewConn returns a Conn of the connected Unix stream socket fd, which it
// takes.
func NewConn(fd int) (*Conn, error) {
	// Best effort: a smaller buffer is only slower.
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, socketBufferSize)
	_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, socketBufferSize)
	sock, err := unet.NewSocket(fd)
	if err != nil {
		return nil, err
	}
	return &Conn{sock: sock}, nil
}

// Close closes the connection.
func (c *Conn) Close() error {
	return c.sock.Close()
}

// Send sends a message whose payload is p, one of the payload types, followed
// by trailer, with fds.
func (c *Conn) Send(typ MsgType, seq uint64, p any, trailer []byte, fds ...int) error {
	if len(fds) > maxFDs {
		return fmt.Errorf("sending %d file descriptors, more than %d", len(fds), maxFDs)
	}
	buf := make([]byte, headerSize, headerSize+binary.Size(p))
	buf, err := binary.Append(buf, binary.LittleEndian, p)
	if err != nil {
		return fmt.Errorf("encoding %v: %w", typ, err)
	}
	length := len(buf) - headerSize + len(trailer)
	if length > MaxMessageSize {
		return fmt.Errorf("%v of %d bytes is too long", typ, length)
	}
	binary.LittleEndian.PutUint32(buf[0:], uint32(typ))
	binary.LittleEndian.PutUint32(buf[4:], uint32(length))
	binary.LittleEndian.PutUint64(buf[8:], seq)

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	bufs := [][]byte{buf, trailer}
	w := c.sock.Writer(true /* blocking */)
	if len(fds) != 0 {
		w.ControlMessage.PackFDs(fds...)
	}
	for total := len(buf) + len(trailer); total > 0; {
		n, err := w.WriteVec(bufs)
		if err != nil {
			return err
		}
		// The descriptors go with the first bytes.
		w.ControlMessage = nil
		total -= n
		bufs = consume(bufs, n)
	}
	return nil
}

// consume drops the first n bytes of bufs.
func consume(bufs [][]byte, n int) [][]byte {
	for n > 0 && len(bufs) > 0 {
		if n < len(bufs[0]) {
			bufs[0] = bufs[0][n:]
			return bufs
		}
		n -= len(bufs[0])
		bufs = bufs[1:]
	}
	for len(bufs) > 0 && len(bufs[0]) == 0 {
		bufs = bufs[1:]
	}
	return bufs
}

// ErrClosed is returned by Recv once the peer closed the connection.
var ErrClosed = errors.New("the connection is closed")

// Recv receives a message.
func (c *Conn) Recv() (*Message, error) {
	var hdr [headerSize]byte
	var fds []int
	closeFDs := func() {
		for _, fd := range fds {
			unix.Close(fd)
		}
	}
	r := c.sock.Reader(true /* blocking */)
	r.EnableFDs(maxFDs)
	for got := 0; got < headerSize; {
		n, err := r.ReadVec([][]byte{hdr[got:]})
		if got == 0 && n > 0 {
			// The descriptors come with the first bytes.
			received, fdErr := r.ExtractFDs()
			fds = append(fds, received...)
			r.EnableFDs(0)
			if fdErr != nil {
				closeFDs()
				return nil, fdErr
			}
		}
		if n == 0 && (err == nil || errors.Is(err, io.EOF)) {
			closeFDs()
			if got == 0 {
				return nil, ErrClosed
			}
			return nil, fmt.Errorf("the connection closed within a message header")
		}
		if err != nil {
			closeFDs()
			return nil, err
		}
		got += n
	}
	length := binary.LittleEndian.Uint32(hdr[4:])
	if length > MaxMessageSize {
		closeFDs()
		return nil, fmt.Errorf("a message of %d bytes is too long", length)
	}
	m := &Message{
		Type:    MsgType(binary.LittleEndian.Uint32(hdr[0:])),
		Seq:     binary.LittleEndian.Uint64(hdr[8:]),
		Payload: make([]byte, length),
		FDs:     fds,
	}
	for got := 0; got < len(m.Payload); {
		n, err := r.ReadVec([][]byte{m.Payload[got:]})
		if n == 0 && (err == nil || errors.Is(err, io.EOF)) {
			m.CloseFDs()
			return nil, fmt.Errorf("the connection closed within a %v", m.Type)
		}
		if err != nil {
			m.CloseFDs()
			return nil, err
		}
		got += n
	}
	return m, nil
}
