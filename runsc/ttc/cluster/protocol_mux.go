// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

const raftWireMagic = "TTR1"

type replayConn struct {
	net.Conn
	reader io.Reader
}

func (c *replayConn) Read(dst []byte) (int, error) { return c.reader.Read(dst) }

type protocolListener struct {
	parent    *ProtocolMux
	conns     chan net.Conn
	done      chan struct{}
	closeOnce sync.Once
}

func (l *protocolListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	case <-l.parent.done:
		return nil, net.ErrClosed
	}
}

func (l *protocolListener) Close() error {
	l.closeOnce.Do(func() { close(l.done) })
	return nil
}

func (l *protocolListener) Addr() net.Addr { return l.parent.listener.Addr() }

// ProtocolMux separates TTC's HTTP control protocol and Raft protocol on one
// plaintext TCP listener. Raft connections start with raftWireMagic; all other
// connections are passed to net/http with their sniffed bytes replayed.
type ProtocolMux struct {
	listener net.Listener
	http     *protocolListener
	// raftMu guards raft, which RenewRaftListener replaces.
	raftMu    sync.Mutex
	raft      *protocolListener
	done      chan struct{}
	closeOnce sync.Once
}

// NewProtocolMux takes ownership of listener and begins accepting connections.
func NewProtocolMux(listener net.Listener) (*ProtocolMux, error) {
	if listener == nil {
		return nil, errors.New("protocol multiplexer requires a listener")
	}
	m := &ProtocolMux{listener: listener, done: make(chan struct{})}
	// Unbuffered delivery ensures that closing a logical listener cannot leave
	// accepted connections stranded in a queue. A classifier may wait briefly
	// until its protocol server starts accepting, but can always be released by
	// closing either the logical listener or the mux.
	m.http = &protocolListener{parent: m, conns: make(chan net.Conn), done: make(chan struct{})}
	m.raft = &protocolListener{parent: m, conns: make(chan net.Conn), done: make(chan struct{})}
	go m.serve()
	return m, nil
}

// HTTPListener returns the listener consumed by net/http.
func (m *ProtocolMux) HTTPListener() net.Listener { return m.http }

// RaftListener returns the listener consumed by Hashicorp Raft.
func (m *ProtocolMux) RaftListener() net.Listener {
	m.raftMu.Lock()
	defer m.raftMu.Unlock()
	return m.raft
}

// RenewRaftListener closes the listener consumed by Hashicorp Raft, if its
// Raft engine has not, and returns a new one for the engine that replaces it.
func (m *ProtocolMux) RenewRaftListener() net.Listener {
	m.raftMu.Lock()
	defer m.raftMu.Unlock()
	_ = m.raft.Close()
	m.raft = &protocolListener{parent: m, conns: make(chan net.Conn), done: make(chan struct{})}
	return m.raft
}

// Addr returns the shared physical listening address.
func (m *ProtocolMux) Addr() net.Addr { return m.listener.Addr() }

func (m *ProtocolMux) serve() {
	defer m.Close()
	for {
		conn, err := m.listener.Accept()
		if err != nil {
			return
		}
		go m.classify(conn)
	}
}

func (m *ProtocolMux) classify(conn net.Conn) {
	var prefix [len(raftWireMagic)]byte
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, prefix[:]); err != nil {
		conn.Close()
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	wrapped := &replayConn{Conn: conn, reader: io.MultiReader(bytes.NewReader(prefix[:]), conn)}
	destination := m.http
	if string(prefix[:]) == raftWireMagic {
		destination = m.RaftListener().(*protocolListener)
	}
	select {
	case destination.conns <- wrapped:
	case <-destination.done:
		conn.Close()
	case <-m.done:
		conn.Close()
	}
}

// Close stops the physical listener and both protocol listeners.
func (m *ProtocolMux) Close() error {
	var err error
	m.closeOnce.Do(func() {
		close(m.done)
		_ = m.http.Close()
		_ = m.RaftListener().Close()
		err = m.listener.Close()
	})
	return err
}
