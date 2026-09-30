// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package rendererclient connects the Sentry's virtual GPU to a renderer
// process, which speaks pkg/gfxproto.
package rendererclient

import (
	"fmt"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/gfxproto"
	"gvisor.dev/gvisor/pkg/log"
	"gvisor.dev/gvisor/pkg/sentry/devices/virtgpu"
)

// Client is a virtgpu.Renderer that a renderer process implements.
type Client struct {
	conn   *gfxproto.Conn
	events virtgpu.RendererEvents

	// mu protects the fields below.
	mu sync.Mutex
	// nextSeq is the sequence number of the next request.
	nextSeq uint64
	// pending are the requests that wait for replies.
	pending map[uint64]chan *gfxproto.Message
	// sizes are the sizes of the resources that the client created.
	sizes map[uint32]uint64
	// err is why the connection ended, once it did.
	err error
	// closed is whether Close was called.
	closed bool
}

var _ virtgpu.Renderer = (*Client)(nil)

// New returns a Client of the renderer connected to the Unix stream socket
// fd, which it takes, once the renderer says that it started.
func New(fd int, events virtgpu.RendererEvents) (*Client, error) {
	conn, err := gfxproto.NewConn(fd)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	m, err := conn.Recv()
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("waiting for the renderer to start: %w", err)
	}
	m.CloseFDs()
	var hello gfxproto.Hello
	trailer, err := m.Decode(&hello)
	switch {
	case err != nil || m.Type != gfxproto.MsgHello:
		err = fmt.Errorf("the renderer's first message is %v, not Hello: %v", m.Type, err)
	case hello.Version != gfxproto.Version:
		err = fmt.Errorf("the renderer speaks version %d of the protocol, not %d", hello.Version, gfxproto.Version)
	case hello.Errno != 0:
		err = fmt.Errorf("the renderer did not start: %s: %w", trailer, unix.Errno(hello.Errno))
	}
	if err != nil {
		conn.Close()
		return nil, err
	}
	c := &Client{
		conn:    conn,
		events:  events,
		nextSeq: 1,
		pending: make(map[uint64]chan *gfxproto.Message),
		sizes:   make(map[uint32]uint64),
	}
	go c.receive()
	return c, nil
}

// receive handles the renderer's messages until the connection ends.
func (c *Client) receive() {
	for {
		m, err := c.conn.Recv()
		if err != nil {
			c.lose(err)
			return
		}
		switch m.Type {
		case gfxproto.MsgFenceSignaled:
			var fs gfxproto.FenceSignaled
			if _, err := m.Decode(&fs); err != nil {
				c.lose(err)
				return
			}
			c.events.FenceSignaled(virtgpu.Fence{
				ID:      fs.FenceID,
				CtxID:   fs.CtxID,
				HasRing: fs.HasRing != 0,
				Ring:    fs.Ring,
			})
		case gfxproto.MsgReply:
			c.mu.Lock()
			ch, ok := c.pending[m.Seq]
			delete(c.pending, m.Seq)
			c.mu.Unlock()
			if !ok {
				m.CloseFDs()
				c.lose(fmt.Errorf("a reply to no request, %d", m.Seq))
				return
			}
			ch <- m
		default:
			m.CloseFDs()
			c.lose(fmt.Errorf("an unexpected %v", m.Type))
			return
		}
	}
}

// lose ends the connection, and reports its loss unless Close ended it.
func (c *Client) lose(err error) {
	c.mu.Lock()
	if c.err != nil {
		c.mu.Unlock()
		return
	}
	c.err = err
	closed := c.closed
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()
	c.conn.Close()
	for _, ch := range pending {
		close(ch)
	}
	if !closed {
		c.events.RendererLost(err)
	}
}

// send sends a message that has no reply.
func (c *Client) send(typ gfxproto.MsgType, p any, trailer []byte) error {
	if err := c.conn.Send(typ, 0, p, trailer); err != nil {
		c.lose(err)
		return err
	}
	return nil
}

// call sends a request, and returns its reply.
func (c *Client) call(typ gfxproto.MsgType, p any, trailer []byte) (*gfxproto.Message, *gfxproto.Reply, error) {
	ch := make(chan *gfxproto.Message, 1)
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return nil, nil, err
	}
	seq := c.nextSeq
	c.nextSeq++
	c.pending[seq] = ch
	c.mu.Unlock()
	if err := c.conn.Send(typ, seq, p, trailer); err != nil {
		c.lose(err)
		return nil, nil, err
	}
	m, ok := <-ch
	if !ok {
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		return nil, nil, fmt.Errorf("the renderer is lost: %w", err)
	}
	var reply gfxproto.Reply
	if _, err := m.Decode(&reply); err != nil {
		m.CloseFDs()
		c.lose(err)
		return nil, nil, err
	}
	if reply.Errno != 0 {
		m.CloseFDs()
		return nil, nil, unix.Errno(reply.Errno)
	}
	return m, &reply, nil
}

// CreateContext implements virtgpu.Renderer.CreateContext.
func (c *Client) CreateContext(ctxID, capsetID uint32, name string) error {
	m, _, err := c.call(gfxproto.MsgCreateContext, &gfxproto.CreateContext{CtxID: ctxID, CapsetID: capsetID}, []byte(name))
	if err != nil {
		return err
	}
	m.CloseFDs()
	return nil
}

// DestroyContext implements virtgpu.Renderer.DestroyContext.
func (c *Client) DestroyContext(ctxID uint32) {
	c.send(gfxproto.MsgDestroyContext, &gfxproto.DestroyContext{CtxID: ctxID}, nil)
}

// CreateResource implements virtgpu.Renderer.CreateResource.
func (c *Client) CreateResource(ctxID, resID uint32, args virtgpu.ResourceArgs) error {
	c.setSize(resID, args.Size)
	return c.send(gfxproto.MsgCreateResource, &gfxproto.CreateResource{
		CtxID:     ctxID,
		ResID:     resID,
		Target:    args.Target,
		Format:    args.Format,
		Bind:      args.Bind,
		Width:     args.Width,
		Height:    args.Height,
		Depth:     args.Depth,
		ArraySize: args.ArraySize,
		LastLevel: args.LastLevel,
		NrSamples: args.NrSamples,
		Flags:     args.Flags,
		Size:      args.Size,
	}, nil)
}

// CreateBlob implements virtgpu.Renderer.CreateBlob.
func (c *Client) CreateBlob(ctxID, resID uint32, args virtgpu.BlobArgs) error {
	c.setSize(resID, args.Size)
	return c.send(gfxproto.MsgCreateBlob, &gfxproto.CreateBlob{
		CtxID:  ctxID,
		ResID:  resID,
		Mem:    args.Mem,
		Flags:  args.Flags,
		BlobID: args.ID,
		Size:   args.Size,
	}, nil)
}

func (c *Client) setSize(resID uint32, size uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sizes[resID] = size
}

// MapResource implements virtgpu.Renderer.MapResource.
func (c *Client) MapResource(resID uint32) (virtgpu.ResourceMemory, error) {
	c.mu.Lock()
	size, ok := c.sizes[resID]
	c.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("no resource %d", resID)
	}
	m, reply, err := c.call(gfxproto.MsgMapResource, &gfxproto.MapResource{ResID: resID}, nil)
	if err != nil {
		return nil, err
	}
	if len(m.FDs) != 1 {
		m.CloseFDs()
		return nil, fmt.Errorf("the memory of resource %d came with %d file descriptors", resID, len(m.FDs))
	}
	fd := m.FDs[0]
	m.FDs = nil
	if reply.Size < size {
		unix.Close(fd)
		return nil, fmt.Errorf("the memory of resource %d is %d bytes, less than its %d", resID, reply.Size, size)
	}
	mem, err := newHostMemory(fd, reply.Size)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	return mem, nil
}

// AttachResource implements virtgpu.Renderer.AttachResource.
func (c *Client) AttachResource(ctxID, resID uint32) {
	c.send(gfxproto.MsgAttachResource, &gfxproto.AttachResource{CtxID: ctxID, ResID: resID}, nil)
}

// DetachResource implements virtgpu.Renderer.DetachResource.
func (c *Client) DetachResource(ctxID, resID uint32) {
	c.send(gfxproto.MsgDetachResource, &gfxproto.DetachResource{CtxID: ctxID, ResID: resID}, nil)
}

// DestroyResource implements virtgpu.Renderer.DestroyResource.
func (c *Client) DestroyResource(resID uint32) {
	c.mu.Lock()
	delete(c.sizes, resID)
	c.mu.Unlock()
	c.send(gfxproto.MsgDestroyResource, &gfxproto.DestroyResource{ResID: resID}, nil)
}

// SubmitCommand implements virtgpu.Renderer.SubmitCommand.
func (c *Client) SubmitCommand(ctxID uint32, cmd []byte) error {
	return c.send(gfxproto.MsgSubmitCommand, &gfxproto.SubmitCommand{CtxID: ctxID}, cmd)
}

// Transfer implements virtgpu.Renderer.Transfer.
func (c *Client) Transfer(ctxID, resID uint32, args virtgpu.TransferArgs) error {
	toHost := uint32(0)
	if args.ToHost {
		toHost = 1
	}
	return c.send(gfxproto.MsgTransfer, &gfxproto.Transfer{
		CtxID:       ctxID,
		ResID:       resID,
		ToHost:      toHost,
		X:           args.X,
		Y:           args.Y,
		Z:           args.Z,
		W:           args.W,
		H:           args.H,
		D:           args.D,
		Level:       args.Level,
		Offset:      args.Offset,
		Stride:      args.Stride,
		LayerStride: args.LayerStride,
	}, nil)
}

// CreateFence implements virtgpu.Renderer.CreateFence.
func (c *Client) CreateFence(f virtgpu.Fence) error {
	hasRing := uint32(0)
	if f.HasRing {
		hasRing = 1
	}
	return c.send(gfxproto.MsgCreateFence, &gfxproto.CreateFence{
		FenceID: f.ID,
		CtxID:   f.CtxID,
		HasRing: hasRing,
		Ring:    f.Ring,
	}, nil)
}

// Close implements virtgpu.Renderer.Close.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	if err := c.conn.Close(); err != nil {
		log.Debugf("virtgpu: closing the renderer connection: %v", err)
	}
}
