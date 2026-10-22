// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package rendererclient

import (
	"fmt"
	"sync"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/gfxproto"
	"gvisor.dev/gvisor/pkg/sentry/devices/virtgpu"
)

// Broker asks the renderers' broker, ttcd, which the Sentry cannot be since
// it starts no processes, for renderers.
type Broker struct {
	// mu serializes requests: each waits for its own reply.
	mu   sync.Mutex
	conn *gfxproto.Conn
}

// NewBroker returns a Broker that asks over the connected Unix stream socket
// fd, which it takes.
func NewBroker(fd int) (*Broker, error) {
	conn, err := gfxproto.NewConn(fd)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &Broker{conn: conn}, nil
}

// NewRenderer implements virtgpu.NewRendererFunc: it starts a renderer, and
// returns a Client of it.
func (b *Broker) NewRenderer(events virtgpu.RendererEvents) (virtgpu.Renderer, error) {
	fd, err := b.start()
	if err != nil {
		return nil, err
	}
	return New(fd, events)
}

// start asks the broker for a renderer, and returns the descriptor of a
// connection to it.
func (b *Broker) start() (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.conn.Send(gfxproto.MsgStartRenderer, 1, &gfxproto.StartRenderer{}, nil); err != nil {
		return -1, fmt.Errorf("asking for a renderer: %w", err)
	}
	m, err := b.conn.Recv()
	if err != nil {
		return -1, fmt.Errorf("waiting for a renderer: %w", err)
	}
	var reply gfxproto.Reply
	if _, err := m.Decode(&reply); err != nil || m.Type != gfxproto.MsgReply {
		m.CloseFDs()
		return -1, fmt.Errorf("the broker answered with %v: %v", m.Type, err)
	}
	if reply.Errno != 0 {
		m.CloseFDs()
		return -1, fmt.Errorf("the broker did not start a renderer: %w", unix.Errno(reply.Errno))
	}
	if len(m.FDs) != 1 {
		m.CloseFDs()
		return -1, fmt.Errorf("the broker answered with %d descriptors", len(m.FDs))
	}
	// The broker keeps its end of the connection open until it hears that
	// the Sentry received it: see gfxproto.RendererReceived.
	if err := b.conn.Send(gfxproto.MsgRendererReceived, 0, &gfxproto.RendererReceived{}, nil); err != nil {
		m.CloseFDs()
		return -1, fmt.Errorf("telling the broker that the renderer's connection arrived: %w", err)
	}
	return m.FDs[0], nil
}

// Close closes the connection to the broker.
func (b *Broker) Close() error {
	return b.conn.Close()
}
