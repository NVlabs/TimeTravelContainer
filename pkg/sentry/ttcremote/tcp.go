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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/memmap"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state/stateio"
)

const (
	tcpProtocolVersion = 1
	tcpHeaderSize      = 44
	maxProtocolError   = 4096
)

var tcpMagic = [4]byte{'T', 'T', 'C', 'P'}

// SnapshotToken uniquely identifies a distributed memory snapshot. Tokens
// must be unpredictable when peers are not connected through a private link.
type SnapshotToken [16]byte

type tcpMessageType uint8

const (
	tcpPull tcpMessageType = iota + 1
	tcpData
	tcpAck
	tcpAckOK
	tcpError
)

type tcpHeader struct {
	type_     tcpMessageType
	token     SnapshotToken
	requestID uint32
	offset    uint64
	length    uint32
	checksum  uint32
}

func (h tcpHeader) marshal(dst []byte) {
	copy(dst[0:4], tcpMagic[:])
	dst[4] = tcpProtocolVersion
	dst[5] = byte(h.type_)
	dst[6], dst[7] = 0, 0
	copy(dst[8:24], h.token[:])
	binary.BigEndian.PutUint32(dst[24:28], h.requestID)
	binary.BigEndian.PutUint64(dst[28:36], h.offset)
	binary.BigEndian.PutUint32(dst[36:40], h.length)
	binary.BigEndian.PutUint32(dst[40:44], h.checksum)
}

func readTCPHeader(r io.Reader) (tcpHeader, error) {
	var data [tcpHeaderSize]byte
	if _, err := io.ReadFull(r, data[:]); err != nil {
		return tcpHeader{}, err
	}
	if string(data[0:4]) != string(tcpMagic[:]) {
		return tcpHeader{}, errors.New("invalid TTC page protocol magic")
	}
	if data[4] != tcpProtocolVersion {
		return tcpHeader{}, fmt.Errorf("unsupported TTC page protocol version %d", data[4])
	}
	if data[6] != 0 || data[7] != 0 {
		return tcpHeader{}, errors.New("nonzero TTC page protocol reserved bits")
	}
	var token SnapshotToken
	copy(token[:], data[8:24])
	return tcpHeader{
		type_:     tcpMessageType(data[5]),
		token:     token,
		requestID: binary.BigEndian.Uint32(data[24:28]),
		offset:    binary.BigEndian.Uint64(data[28:36]),
		length:    binary.BigEndian.Uint32(data[36:40]),
		checksum:  binary.BigEndian.Uint32(data[40:44]),
	}, nil
}

func writeTCPMessage(w io.Writer, header tcpHeader, payload []byte) error {
	var encoded [tcpHeaderSize]byte
	header.marshal(encoded[:])
	if err := writeFull(w, encoded[:]); err != nil {
		return err
	}
	if len(payload) != 0 {
		return writeFull(w, payload)
	}
	return nil
}

func writeFull(w io.Writer, data []byte) error {
	for len(data) != 0 {
		n, err := w.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
		data = data[n:]
	}
	return nil
}

// TCPPageServer serves a RemoteSnapshot over a bounded request/ACK protocol.
// It owns snapshot after construction.
type TCPPageServer struct {
	listener net.Listener
	token    SnapshotToken
	snapshot *pgalloc.RemoteSnapshot
	maxRead  uint64

	mu        sync.Mutex
	closed    bool
	conns     map[net.Conn]struct{}
	closeErr  error
	closeDone chan struct{}
	wg        sync.WaitGroup

	requests atomic.Uint64
	acks     atomic.Uint64
	bytes    atomic.Uint64
}

// TCPPageServerStats reports completed protocol operations.
type TCPPageServerStats struct {
	PullRequests uint64
	AckRequests  uint64
	BytesSent    uint64
}

// NewTCPPageServer constructs a server. Serve must be called to accept peers.
func NewTCPPageServer(listener net.Listener, token SnapshotToken, snapshot *pgalloc.RemoteSnapshot, maxRead uint64) (*TCPPageServer, error) {
	if listener == nil || snapshot == nil {
		return nil, errors.New("TCP page server requires a listener and snapshot")
	}
	if maxRead < hostarch.PageSize || !hostarch.IsPageAligned(maxRead) || maxRead > 2<<20 {
		return nil, fmt.Errorf("invalid maximum TCP page read size %d", maxRead)
	}
	return &TCPPageServer{
		listener:  listener,
		token:     token,
		snapshot:  snapshot,
		maxRead:   maxRead,
		conns:     make(map[net.Conn]struct{}),
		closeDone: make(chan struct{}),
	}, nil
}

// Addr returns the server's listening address.
func (s *TCPPageServer) Addr() net.Addr { return s.listener.Addr() }

// Stats returns server counters.
func (s *TCPPageServer) Stats() TCPPageServerStats {
	return TCPPageServerStats{
		PullRequests: s.requests.Load(),
		AckRequests:  s.acks.Load(),
		BytesSent:    s.bytes.Load(),
	}
}

// Serve accepts connections until Close is called or the listener fails.
func (s *TCPPageServer) Serve() error {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return nil
			}
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return nil
		}
		s.conns[conn] = struct{}{}
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serveConn(conn)
	}
}

func (s *TCPPageServer) serveConn(conn net.Conn) {
	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		s.wg.Done()
	}()
	for {
		header, err := readTCPHeader(conn)
		if err != nil {
			return
		}
		if header.token != s.token {
			s.writeError(conn, header, errors.New("snapshot token does not match"))
			return
		}
		switch header.type_ {
		case tcpPull:
			if header.checksum != 0 || header.length == 0 || uint64(header.length) > s.maxRead ||
				!hostarch.IsPageAligned(uint64(header.length)) || !hostarch.IsPageAligned(header.offset) {
				s.writeError(conn, header, errors.New("invalid pull range"))
				return
			}
			payload := make([]byte, header.length)
			n, err := s.snapshot.CopyStreamAt(payload, int64(header.offset))
			if err != nil || n != len(payload) {
				if err == nil {
					err = io.ErrUnexpectedEOF
				}
				s.writeError(conn, header, err)
				return
			}
			s.requests.Add(1)
			s.bytes.Add(uint64(len(payload)))
			if err := writeTCPMessage(conn, tcpHeader{
				type_:     tcpData,
				token:     s.token,
				requestID: header.requestID,
				offset:    header.offset,
				length:    header.length,
				checksum:  crc32.ChecksumIEEE(payload),
			}, payload); err != nil {
				return
			}

		case tcpAck:
			if header.checksum != 0 || header.length == 0 || !hostarch.IsPageAligned(uint64(header.length)) || !hostarch.IsPageAligned(header.offset) {
				s.writeError(conn, header, errors.New("invalid acknowledgement range"))
				return
			}
			if err := s.snapshot.AcknowledgeStreamRange(header.offset, uint64(header.length)); err != nil {
				s.writeError(conn, header, err)
				return
			}
			s.acks.Add(1)
			if err := writeTCPMessage(conn, tcpHeader{
				type_:     tcpAckOK,
				token:     s.token,
				requestID: header.requestID,
				offset:    header.offset,
				length:    header.length,
			}, nil); err != nil {
				return
			}

		default:
			s.writeError(conn, header, fmt.Errorf("unexpected message type %d", header.type_))
			return
		}
	}
}

func (s *TCPPageServer) writeError(conn net.Conn, request tcpHeader, err error) {
	payload := []byte(err.Error())
	if len(payload) > maxProtocolError {
		payload = payload[:maxProtocolError]
	}
	_ = writeTCPMessage(conn, tcpHeader{
		type_:     tcpError,
		token:     s.token,
		requestID: request.requestID,
		offset:    request.offset,
		length:    uint32(len(payload)),
		checksum:  crc32.ChecksumIEEE(payload),
	}, payload)
}

// Close stops the server, cancels the remaining snapshot, and waits for
// handlers. It is idempotent.
func (s *TCPPageServer) Close() error {
	s.mu.Lock()
	if s.closed {
		done := s.closeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		err := s.closeErr
		s.mu.Unlock()
		return err
	}
	s.closed = true
	err := s.listener.Close()
	for conn := range s.conns {
		conn.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	if snapshotErr := s.snapshot.Close(); err == nil {
		err = snapshotErr
	}
	s.mu.Lock()
	s.closeErr = err
	close(s.closeDone)
	s.mu.Unlock()
	return err
}

type tcpReadSubmission struct {
	id     int
	offset int64
	total  uint64
	dst    stateio.LocalClientRanges
}

// TCPRemoteReader implements stateio.AsyncReader over TCP. Each worker uses a
// separate ordered connection, allowing MaxParallel independent page pulls.
type TCPRemoteReader struct {
	stateio.NoRegisterClientFD

	address      string
	token        SnapshotToken
	maxReadBytes uint64
	maxRanges    int
	timeout      time.Duration

	ctx    context.Context
	cancel context.CancelFunc
	subs   chan tcpReadSubmission
	cmps   chan stateio.Completion
	wg     sync.WaitGroup
	once   sync.Once
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
}

// NewTCPRemoteReader constructs a TCP page reader. timeout applies to dialing
// and every request/response pair.
func NewTCPRemoteReader(address string, token SnapshotToken, maxReadBytes uint64, maxRanges, maxParallel int, timeout time.Duration) (*TCPRemoteReader, error) {
	if address == "" {
		return nil, errors.New("TCP remote reader address is empty")
	}
	if maxReadBytes < hostarch.PageSize || !hostarch.IsPageAligned(maxReadBytes) || maxReadBytes > 2<<20 {
		return nil, fmt.Errorf("invalid maximum TCP read size %d", maxReadBytes)
	}
	if maxRanges <= 0 || maxParallel <= 0 {
		return nil, fmt.Errorf("max ranges and parallelism must be positive")
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("TCP page timeout must be positive")
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &TCPRemoteReader{
		address:      address,
		token:        token,
		maxReadBytes: maxReadBytes,
		maxRanges:    maxRanges,
		timeout:      timeout,
		ctx:          ctx,
		cancel:       cancel,
		subs:         make(chan tcpReadSubmission, maxParallel),
		cmps:         make(chan stateio.Completion, maxParallel),
		conns:        make(map[net.Conn]struct{}),
	}
	r.wg.Add(maxParallel)
	for range maxParallel {
		go r.worker()
	}
	return r, nil
}

func (r *TCPRemoteReader) MaxReadBytes() uint64 { return r.maxReadBytes }
func (r *TCPRemoteReader) MaxRanges() int       { return r.maxRanges }
func (r *TCPRemoteReader) MaxParallel() int     { return cap(r.subs) }

func (r *TCPRemoteReader) AddRead(id int, off int64, _ stateio.DestinationFile, _ memmap.FileRange, dstMap []byte) {
	r.subs <- tcpReadSubmission{id: id, offset: off, total: uint64(len(dstMap)), dst: stateio.LocalClientMapping(dstMap)}
}

func (r *TCPRemoteReader) AddReadv(id int, off int64, total uint64, _ stateio.DestinationFile, _ []memmap.FileRange, dstMaps []unix.Iovec) {
	r.subs <- tcpReadSubmission{id: id, offset: off, total: total, dst: stateio.LocalClientMappings(dstMaps)}
}

func (r *TCPRemoteReader) Wait(completions []stateio.Completion, minCompletions int) ([]stateio.Completion, error) {
	return stateio.CompletionChanWait(r.cmps, completions, minCompletions)
}

func (r *TCPRemoteReader) Close() error {
	r.once.Do(func() {
		r.cancel()
		r.mu.Lock()
		for conn := range r.conns {
			conn.Close()
		}
		r.mu.Unlock()
	})
	r.wg.Wait()
	return nil
}

func (r *TCPRemoteReader) worker() {
	defer r.wg.Done()
	var conn net.Conn
	defer func() {
		if conn != nil {
			r.mu.Lock()
			delete(r.conns, conn)
			r.mu.Unlock()
			conn.Close()
		}
	}()
	for {
		select {
		case <-r.ctx.Done():
			return
		case sub := <-r.subs:
			if conn == nil {
				var err error
				dialer := net.Dialer{Timeout: r.timeout}
				conn, err = dialer.DialContext(r.ctx, "tcp", r.address)
				if err != nil {
					r.cmps <- stateio.Completion{ID: sub.id, Err: err}
					continue
				}
				r.mu.Lock()
				r.conns[conn] = struct{}{}
				r.mu.Unlock()
			}
			n, err := r.performRead(conn, sub)
			if err != nil {
				r.mu.Lock()
				delete(r.conns, conn)
				r.mu.Unlock()
				conn.Close()
				conn = nil
			}
			r.cmps <- stateio.Completion{ID: sub.id, N: n, Err: err}
		}
	}
}

func (r *TCPRemoteReader) performRead(conn net.Conn, sub tcpReadSubmission) (uint64, error) {
	if sub.offset < 0 || sub.total == 0 || sub.total > r.maxReadBytes || sub.total > uint64(^uint32(0)) ||
		!hostarch.IsPageAligned(uint64(sub.offset)) || !hostarch.IsPageAligned(sub.total) {
		return 0, fmt.Errorf("invalid TCP remote read: off=%d len=%d", sub.offset, sub.total)
	}
	deadline := time.Now().Add(r.timeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return 0, err
	}
	requestID := uint32(sub.id)
	pull := tcpHeader{
		type_:     tcpPull,
		token:     r.token,
		requestID: requestID,
		offset:    uint64(sub.offset),
		length:    uint32(sub.total),
	}
	if err := writeTCPMessage(conn, pull, nil); err != nil {
		return 0, err
	}
	response, payload, err := readTCPResponse(conn, r.token, requestID, r.maxReadBytes)
	if err != nil {
		return 0, err
	}
	if response.type_ == tcpError {
		return 0, fmt.Errorf("remote page server: %s", payload)
	}
	if response.type_ != tcpData || response.offset != uint64(sub.offset) || uint64(response.length) != sub.total {
		return 0, fmt.Errorf("mismatched TCP page response: type=%d off=%d len=%d", response.type_, response.offset, response.length)
	}

	var copied uint64
	sub.dst.Mappings(func(_ int, dst []byte) bool {
		n := copy(dst, payload[copied:])
		copied += uint64(n)
		return copied < sub.total
	})
	if copied != sub.total {
		return copied, fmt.Errorf("destination mappings contain %d bytes, want %d", copied, sub.total)
	}
	if err := writeTCPMessage(conn, tcpHeader{
		type_:     tcpAck,
		token:     r.token,
		requestID: requestID,
		offset:    uint64(sub.offset),
		length:    uint32(sub.total),
	}, nil); err != nil {
		return 0, err
	}
	ack, ackPayload, err := readTCPResponse(conn, r.token, requestID, r.maxReadBytes)
	if err != nil {
		return 0, err
	}
	if ack.type_ == tcpError {
		return 0, fmt.Errorf("remote page acknowledgement: %s", ackPayload)
	}
	if ack.type_ != tcpAckOK || ack.offset != uint64(sub.offset) || uint64(ack.length) != sub.total {
		return 0, errors.New("mismatched TCP page acknowledgement")
	}
	return copied, nil
}

func readTCPResponse(r io.Reader, token SnapshotToken, requestID uint32, maxPayload uint64) (tcpHeader, []byte, error) {
	header, err := readTCPHeader(r)
	if err != nil {
		return tcpHeader{}, nil, err
	}
	if header.token != token || header.requestID != requestID {
		return tcpHeader{}, nil, errors.New("TCP page response identity does not match request")
	}
	if uint64(header.length) > maxPayload {
		return tcpHeader{}, nil, fmt.Errorf("TCP page response length %d exceeds limit %d", header.length, maxPayload)
	}
	var payload []byte
	if header.type_ == tcpData || header.type_ == tcpError {
		payload = make([]byte, header.length)
		if _, err := io.ReadFull(r, payload); err != nil {
			return tcpHeader{}, nil, err
		}
		if crc32.ChecksumIEEE(payload) != header.checksum {
			return tcpHeader{}, nil, errors.New("TCP page response checksum mismatch")
		}
	} else if header.checksum != 0 {
		return tcpHeader{}, nil, errors.New("control response has a checksum")
	}
	return header, payload, nil
}
