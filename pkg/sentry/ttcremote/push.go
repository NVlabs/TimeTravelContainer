// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ttcremote

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

const (
	tcpPush tcpMessageType = 32 + iota
	tcpPushOK
	tcpPushSkip
)

// TCPPushReceiver accepts proactive pages into a target async loader.
type TCPPushReceiver struct {
	listener net.Listener
	token    SnapshotToken
	pages    *pgalloc.AsyncPagesFileLoad
	maxPush  uint64

	mu        sync.Mutex
	closed    bool
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
	installed atomic.Uint64
	skipped   atomic.Uint64
}

// NewTCPPushReceiver constructs a target-side push receiver.
func NewTCPPushReceiver(listener net.Listener, token SnapshotToken, pages *pgalloc.AsyncPagesFileLoad, maxPush uint64) (*TCPPushReceiver, error) {
	if listener == nil || pages == nil || maxPush < hostarch.PageSize || !hostarch.IsPageAligned(maxPush) || maxPush > 2<<20 {
		return nil, errors.New("invalid TCP push receiver configuration")
	}
	return &TCPPushReceiver{listener: listener, token: token, pages: pages, maxPush: maxPush, conns: make(map[net.Conn]struct{})}, nil
}

// Addr returns the reverse-channel listen address.
func (r *TCPPushReceiver) Addr() net.Addr { return r.listener.Addr() }

// Serve accepts push connections until Close.
func (r *TCPPushReceiver) Serve() error {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			r.mu.Lock()
			closed := r.closed
			r.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			conn.Close()
			return nil
		}
		r.conns[conn] = struct{}{}
		r.wg.Add(1)
		r.mu.Unlock()
		go r.serveConn(conn)
	}
}

func (r *TCPPushReceiver) serveConn(conn net.Conn) {
	defer func() {
		conn.Close()
		r.mu.Lock()
		delete(r.conns, conn)
		r.mu.Unlock()
		r.wg.Done()
	}()
	for {
		header, err := readTCPHeader(conn)
		if err != nil {
			return
		}
		if header.token != r.token || header.type_ != tcpPush || header.length == 0 || uint64(header.length) > r.maxPush ||
			!hostarch.IsPageAligned(header.offset) || !hostarch.IsPageAligned(uint64(header.length)) {
			return
		}
		payload := make([]byte, header.length)
		if _, err := io.ReadFull(conn, payload); err != nil || crc32.ChecksumIEEE(payload) != header.checksum {
			return
		}
		installed, err := r.pages.TryInstallRemotePages(header.offset, payload)
		if err != nil {
			return
		}
		responseType := tcpPushSkip
		if installed {
			responseType = tcpPushOK
			r.installed.Add(uint64(header.length))
		} else {
			r.skipped.Add(uint64(header.length))
		}
		if err := writeTCPMessage(conn, tcpHeader{
			type_: responseType, token: r.token, requestID: header.requestID,
			offset: header.offset, length: header.length,
		}, nil); err != nil {
			return
		}
	}
}

// Close stops accepting pushes and waits for handlers.
func (r *TCPPushReceiver) Close() error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	err := r.listener.Close()
	for conn := range r.conns {
		conn.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
	return err
}

// TCPPushReceiverStats reports accepted and pull-owned bytes.
func (r *TCPPushReceiver) Stats() (installed, skipped uint64) {
	return r.installed.Load(), r.skipped.Load()
}

// TCPPushSender consumes source COW hints and sends them to the target.
type TCPPushSender struct {
	done  chan struct{}
	err   atomic.Value
	bytes atomic.Uint64
}

// StartTCPPushSender starts a source-side reverse-channel worker.
func StartTCPPushSender(address string, token SnapshotToken, snapshot *pgalloc.RemoteSnapshot, maxPush uint64, timeout time.Duration) (*TCPPushSender, error) {
	if address == "" || snapshot == nil || maxPush < hostarch.PageSize || !hostarch.IsPageAligned(maxPush) || timeout <= 0 {
		return nil, errors.New("invalid TCP push sender configuration")
	}
	s := &TCPPushSender{done: make(chan struct{})}
	go s.run(address, token, snapshot, maxPush, timeout)
	return s, nil
}

func (s *TCPPushSender) run(address string, token SnapshotToken, snapshot *pgalloc.RemoteSnapshot, maxPush uint64, timeout time.Duration) {
	defer close(s.done)
	var conn net.Conn
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	requestID := uint32(1)
	for hint := range snapshot.PushHints() {
		for off, remaining := hint.StreamOffset, hint.Length; remaining != 0; requestID++ {
			length := min(remaining, maxPush)
			payload := make([]byte, length)
			if n, err := snapshot.CopyStreamAt(payload, int64(off)); err != nil || uint64(n) != length {
				break
			}
			if conn == nil {
				var err error
				conn, err = net.DialTimeout("tcp", address, timeout)
				if err != nil {
					s.err.Store(err)
					return
				}
			}
			_ = conn.SetDeadline(time.Now().Add(timeout))
			header := tcpHeader{type_: tcpPush, token: token, requestID: requestID, offset: off, length: uint32(length), checksum: crc32.ChecksumIEEE(payload)}
			if err := writeTCPMessage(conn, header, payload); err != nil {
				s.err.Store(err)
				return
			}
			response, _, err := readTCPResponse(conn, token, requestID, maxPush)
			if err != nil {
				s.err.Store(err)
				return
			}
			if response.offset != off || uint64(response.length) != length {
				s.err.Store(fmt.Errorf("mismatched TCP push response"))
				return
			}
			switch response.type_ {
			case tcpPushOK:
				if err := snapshot.AcknowledgeStreamRange(off, length); err != nil {
					s.err.Store(err)
					return
				}
				s.bytes.Add(length)
			case tcpPushSkip:
			default:
				s.err.Store(fmt.Errorf("unexpected TCP push response %d", response.type_))
				return
			}
			off += length
			remaining -= length
		}
	}
}

// Wait waits for snapshot closure to stop the sender.
func (s *TCPPushSender) Wait() error {
	<-s.done
	if value := s.err.Load(); value != nil {
		return value.(error)
	}
	return nil
}

// BytesPushed returns successfully installed and acknowledged bytes.
func (s *TCPPushSender) BytesPushed() uint64 { return s.bytes.Load() }
