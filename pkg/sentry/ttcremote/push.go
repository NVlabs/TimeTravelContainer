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
	tcpPushCommit
	tcpPushCommitOK
)

type pushReceiptKey struct {
	requestID uint32
	offset    uint64
	length    uint32
}

// TCPPushReceiver accepts proactive pages into a target async loader.
type TCPPushReceiver struct {
	listener net.Listener
	token    SnapshotToken
	pages    *pgalloc.AsyncPagesFileLoad
	maxPush  uint64

	mu        sync.Mutex
	closed    bool
	conns     map[net.Conn]struct{}
	receipts  map[pushReceiptKey]uint32
	wg        sync.WaitGroup
	installed atomic.Uint64
	skipped   atomic.Uint64
}

// NewTCPPushReceiver constructs a target-side push receiver.
func NewTCPPushReceiver(listener net.Listener, token SnapshotToken, pages *pgalloc.AsyncPagesFileLoad, maxPush uint64) (*TCPPushReceiver, error) {
	if listener == nil || pages == nil || maxPush < hostarch.PageSize || !hostarch.IsPageAligned(maxPush) || maxPush > 2<<20 {
		return nil, errors.New("invalid TCP push receiver configuration")
	}
	return &TCPPushReceiver{
		listener: listener,
		token:    token,
		pages:    pages,
		maxPush:  maxPush,
		conns:    make(map[net.Conn]struct{}),
		receipts: make(map[pushReceiptKey]uint32),
	}, nil
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
		if header.token != r.token {
			return
		}
		if header.type_ == tcpPushCommit {
			if header.length == 0 || uint64(header.length) > r.maxPush || header.checksum != 0 ||
				!hostarch.IsPageAligned(header.offset) || !hostarch.IsPageAligned(uint64(header.length)) {
				return
			}
			key := pushReceiptKey{header.requestID, header.offset, header.length}
			r.mu.Lock()
			delete(r.receipts, key)
			r.mu.Unlock()
			if err := writeTCPMessage(conn, tcpHeader{
				type_: tcpPushCommitOK, token: r.token, requestID: header.requestID,
				offset: header.offset, length: header.length,
			}, nil); err != nil {
				return
			}
			continue
		}
		if header.type_ != tcpPush || header.length == 0 || uint64(header.length) > r.maxPush ||
			!hostarch.IsPageAligned(header.offset) || !hostarch.IsPageAligned(uint64(header.length)) {
			return
		}
		payload := make([]byte, header.length)
		if _, err := io.ReadFull(conn, payload); err != nil || crc32.ChecksumIEEE(payload) != header.checksum {
			return
		}
		key := pushReceiptKey{header.requestID, header.offset, header.length}
		r.mu.Lock()
		checksum, duplicate := r.receipts[key]
		r.mu.Unlock()
		if duplicate {
			if checksum != header.checksum {
				return
			}
			if err := writeTCPMessage(conn, tcpHeader{
				type_: tcpPushOK, token: r.token, requestID: header.requestID,
				offset: header.offset, length: header.length,
			}, nil); err != nil {
				return
			}
			continue
		}
		installed, err := r.pages.TryInstallRemotePages(header.offset, payload)
		if err != nil {
			return
		}
		responseType := tcpPushSkip
		if installed {
			responseType = tcpPushOK
			r.mu.Lock()
			r.receipts[key] = header.checksum
			r.mu.Unlock()
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
	done    chan struct{}
	err     atomic.Value
	bytes   atomic.Uint64
	retries atomic.Uint64
}

// TCPPushSenderOpts configures proactive push transport and retry behavior.
type TCPPushSenderOpts struct {
	MaxPush      uint64
	Timeout      time.Duration
	RetryLimit   int
	RetryBackoff time.Duration
}

// StartTCPPushSender starts a source-side reverse-channel worker.
func StartTCPPushSender(address string, token SnapshotToken, snapshot *pgalloc.RemoteSnapshot, maxPush uint64, timeout time.Duration) (*TCPPushSender, error) {
	return StartTCPPushSenderWithOpts(address, token, snapshot, TCPPushSenderOpts{
		MaxPush: maxPush, Timeout: timeout, RetryLimit: 3, RetryBackoff: 10 * time.Millisecond,
	})
}

// StartTCPPushSenderWithOpts starts a configurable source-side reverse-channel
// worker. Every chunk keeps the same transaction identity across retries.
func StartTCPPushSenderWithOpts(address string, token SnapshotToken, snapshot *pgalloc.RemoteSnapshot, opts TCPPushSenderOpts) (*TCPPushSender, error) {
	if address == "" || snapshot == nil || opts.MaxPush < hostarch.PageSize || !hostarch.IsPageAligned(opts.MaxPush) ||
		opts.MaxPush > 2<<20 || opts.Timeout <= 0 || opts.RetryLimit < 0 || opts.RetryBackoff < 0 {
		return nil, errors.New("invalid TCP push sender configuration")
	}
	s := &TCPPushSender{done: make(chan struct{})}
	go s.run(address, token, snapshot, opts)
	return s, nil
}

func (s *TCPPushSender) run(address string, token SnapshotToken, snapshot *pgalloc.RemoteSnapshot, opts TCPPushSenderOpts) {
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
			length := min(remaining, opts.MaxPush)
			payload := make([]byte, length)
			if n, err := snapshot.CopyStreamAt(payload, int64(off)); err != nil || uint64(n) != length {
				break
			}
			header := tcpHeader{type_: tcpPush, token: token, requestID: requestID, offset: off, length: uint32(length), checksum: crc32.ChecksumIEEE(payload)}
			response, err := s.pushWithRetry(&conn, address, token, header, payload, opts)
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
				commit := header
				commit.type_ = tcpPushCommit
				commit.checksum = 0
				response, err = s.pushWithRetry(&conn, address, token, commit, nil, opts)
				if err != nil {
					s.err.Store(err)
					return
				}
				if response.type_ != tcpPushCommitOK {
					s.err.Store(fmt.Errorf("unexpected TCP push commit response %d", response.type_))
					return
				}
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

func (s *TCPPushSender) pushWithRetry(conn *net.Conn, address string, token SnapshotToken, header tcpHeader, payload []byte, opts TCPPushSenderOpts) (tcpHeader, error) {
	var lastErr error
	for attempt := 0; attempt <= opts.RetryLimit; attempt++ {
		if *conn == nil {
			var err error
			*conn, err = net.DialTimeout("tcp", address, opts.Timeout)
			if err != nil {
				lastErr = err
			}
		}
		if *conn != nil {
			_ = (*conn).SetDeadline(time.Now().Add(opts.Timeout))
			if err := writeTCPMessage(*conn, header, payload); err == nil {
				response, _, err := readTCPResponse(*conn, token, header.requestID, opts.MaxPush)
				if err == nil {
					return response, nil
				}
				lastErr = err
			} else {
				lastErr = err
			}
			(*conn).Close()
			*conn = nil
		}
		if attempt != opts.RetryLimit {
			s.retries.Add(1)
			if opts.RetryBackoff != 0 {
				time.Sleep(opts.RetryBackoff)
			}
		}
	}
	return tcpHeader{}, fmt.Errorf("TCP push failed after %d attempts: %w", opts.RetryLimit+1, lastErr)
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

// Retries returns the number of repeated push or commit attempts.
func (s *TCPPushSender) Retries() uint64 { return s.retries.Load() }
