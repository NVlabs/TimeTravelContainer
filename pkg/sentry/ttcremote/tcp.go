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
	"os"
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
	tcpHello
	tcpHelloOK
	tcpMetadataPull
	tcpMetadataData
	tcpAckBatch
	tcpAckBatchOK
)

const tcpAckRangeSize = 16

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
	conns     map[io.ReadWriteCloser]struct{}
	closeErr  error
	closeDone chan struct{}
	stop      chan struct{}
	wg        sync.WaitGroup

	requests      atomic.Uint64
	acks          atomic.Uint64
	ackBatches    atomic.Uint64
	bytes         atomic.Uint64
	metadata      []byte
	metadataBytes atomic.Uint64
	pushOpts      *TCPPushSenderOpts
	pushBytes     atomic.Uint64
	pushRetries   atomic.Uint64
	pushSenders   map[*TCPPushSender]struct{}
}

// TCPPageServerStats reports completed protocol operations.
type TCPPageServerStats struct {
	PullRequests      uint64
	AckRequests       uint64
	AckBatches        uint64
	BytesSent         uint64
	MetadataBytesSent uint64
	PushBytesSent     uint64
	PushRetries       uint64
}

// NewTCPPageServer constructs a server. Serve must be called to accept peers.
func NewTCPPageServer(listener net.Listener, token SnapshotToken, snapshot *pgalloc.RemoteSnapshot, maxRead uint64) (*TCPPageServer, error) {
	if listener == nil {
		return nil, errors.New("TCP page server requires a listener")
	}
	server, err := NewTCPPageSession(token, snapshot, maxRead)
	if err != nil {
		return nil, err
	}
	server.listener = listener
	return server, nil
}

// NewTCPPageSession constructs a page protocol session whose connected TCP
// streams are supplied by ServeConn. This permits a less-privileged Sentry to
// serve memory while a host-side ttcd owns the listening socket.
func NewTCPPageSession(token SnapshotToken, snapshot *pgalloc.RemoteSnapshot, maxRead uint64) (*TCPPageServer, error) {
	if snapshot == nil {
		return nil, errors.New("TCP page session requires a snapshot")
	}
	if maxRead < hostarch.PageSize || !hostarch.IsPageAligned(maxRead) || maxRead > 2<<20 {
		return nil, fmt.Errorf("invalid maximum TCP page read size %d", maxRead)
	}
	return &TCPPageServer{
		token:       token,
		snapshot:    snapshot,
		maxRead:     maxRead,
		conns:       make(map[io.ReadWriteCloser]struct{}),
		closeDone:   make(chan struct{}),
		stop:        make(chan struct{}),
		pushSenders: make(map[*TCPPushSender]struct{}),
	}, nil
}

// Addr returns the server's listening address.
func (s *TCPPageServer) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// Stats returns server counters.
func (s *TCPPageServer) Stats() TCPPageServerStats {
	stats := TCPPageServerStats{
		PullRequests:      s.requests.Load(),
		AckRequests:       s.acks.Load(),
		AckBatches:        s.ackBatches.Load(),
		BytesSent:         s.bytes.Load(),
		MetadataBytesSent: s.metadataBytes.Load(),
		PushBytesSent:     s.pushBytes.Load(),
		PushRetries:       s.pushRetries.Load(),
	}
	s.mu.Lock()
	for sender := range s.pushSenders {
		active := sender.Stats()
		stats.PushBytesSent += active.BytesPushed
		stats.PushRetries += active.Retries
	}
	s.mu.Unlock()
	return stats
}

// SnapshotStats returns the current page-ownership state. It intentionally
// excludes the session token so callers can expose it through diagnostics.
func (s *TCPPageServer) SnapshotStats() pgalloc.RemoteSnapshotStats {
	return s.snapshot.Stats()
}

// StreamBytes returns the size of the immutable packed memory stream.
func (s *TCPPageServer) StreamBytes() uint64 { return s.snapshot.StreamBytes() }

// ExportPackedSnapshot persists the immutable page stream in transport order.
func (s *TCPPageServer) ExportPackedSnapshot(dst *os.File, allowCopy bool) error {
	return s.snapshot.ExportPackedSnapshot(dst, allowCopy)
}

// SetMetadata configures the immutable Kernel metadata stream advertised by
// the session. It must be called before Serve.
func (s *TCPPageServer) SetMetadata(metadata []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) != 0 || s.closed {
		return errors.New("cannot change TCP page metadata after serving starts")
	}
	if uint64(len(metadata)) > uint64(^uint32(0)) {
		return errors.New("TCP page metadata exceeds 4 GiB protocol limit")
	}
	s.metadata = append([]byte(nil), metadata...)
	return nil
}

// ConfigurePush enables target-registered reverse push lanes. It must be
// called before any stream is served.
func (s *TCPPageServer) ConfigurePush(opts TCPPushSenderOpts) error {
	if err := validateTCPPushSenderOpts(&opts); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.conns) != 0 || s.closed {
		return errors.New("cannot configure TCP push after serving starts")
	}
	s.pushOpts = &opts
	return nil
}

// Serve accepts connections until Close is called or the listener fails.
func (s *TCPPageServer) Serve() error {
	if s.listener == nil {
		return errors.New("TCP page session has no listener")
	}
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
		go func() { _ = s.ServeStream(conn) }()
	}
}

// ServeConn handles one already-connected page protocol stream. It blocks
// until the peer disconnects or Close cancels the session.
func (s *TCPPageServer) ServeConn(conn net.Conn) error {
	return s.ServeStream(conn)
}

// ServeStream is ServeConn without requiring host socket introspection.
func (s *TCPPageServer) ServeStream(conn io.ReadWriteCloser) error {
	if conn == nil {
		return errors.New("nil TCP page connection")
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		conn.Close()
		return net.ErrClosed
	}
	s.conns[conn] = struct{}{}
	s.wg.Add(1)
	s.mu.Unlock()
	s.serveConn(conn)
	return nil
}

func (s *TCPPageServer) serveConn(conn io.ReadWriteCloser) {
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
		case tcpHello:
			if header.offset != 0 || header.length != 0 || header.checksum != 0 {
				s.writeError(conn, header, errors.New("invalid session hello"))
				return
			}
			if err := writeTCPMessage(conn, tcpHeader{
				type_: tcpHelloOK, token: s.token, requestID: header.requestID,
				length: uint32(len(s.metadata)),
			}, nil); err != nil {
				return
			}

		case tcpMetadataPull:
			if header.checksum != 0 || header.length == 0 || uint64(header.length) > s.maxRead ||
				header.offset > uint64(len(s.metadata)) || uint64(header.length) > uint64(len(s.metadata))-header.offset {
				s.writeError(conn, header, errors.New("invalid metadata range"))
				return
			}
			payload := s.metadata[header.offset : header.offset+uint64(header.length)]
			s.metadataBytes.Add(uint64(len(payload)))
			if err := writeTCPMessage(conn, tcpHeader{
				type_: tcpMetadataData, token: s.token, requestID: header.requestID,
				offset: header.offset, length: header.length, checksum: crc32.ChecksumIEEE(payload),
			}, payload); err != nil {
				return
			}

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
			if header.checksum != 0 || header.length == 0 || uint64(header.length) > s.maxRead ||
				!hostarch.IsPageAligned(uint64(header.length)) || !hostarch.IsPageAligned(header.offset) {
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

		case tcpAckBatch:
			if header.offset != 0 || header.length == 0 || uint64(header.length) > s.maxRead ||
				int(header.length)%tcpAckRangeSize != 0 {
				s.writeError(conn, header, errors.New("invalid acknowledgement batch"))
				return
			}
			payload := make([]byte, header.length)
			if _, err := io.ReadFull(conn, payload); err != nil || crc32.ChecksumIEEE(payload) != header.checksum {
				return
			}
			entries := len(payload) / tcpAckRangeSize
			for i := 0; i < entries; i++ {
				entry := payload[i*tcpAckRangeSize : (i+1)*tcpAckRangeSize]
				offset := binary.BigEndian.Uint64(entry[4:12])
				length := uint64(binary.BigEndian.Uint32(entry[12:16]))
				if length == 0 || length > s.maxRead || !hostarch.IsPageAligned(offset) || !hostarch.IsPageAligned(length) {
					s.writeError(conn, header, errors.New("invalid range in acknowledgement batch"))
					return
				}
				if err := s.snapshot.AcknowledgeStreamRange(offset, length); err != nil {
					s.writeError(conn, header, err)
					return
				}
			}
			s.acks.Add(uint64(entries))
			s.ackBatches.Add(1)
			if err := writeTCPMessage(conn, tcpHeader{
				type_: tcpAckBatchOK, token: s.token, requestID: header.requestID,
				offset: uint64(entries),
			}, nil); err != nil {
				return
			}

		case tcpPushRegister:
			if header.offset != 0 || header.length != 0 || header.checksum != 0 || s.pushOpts == nil {
				s.writeError(conn, header, errors.New("reverse push is not enabled"))
				return
			}
			if err := writeTCPMessage(conn, tcpHeader{
				type_: tcpPushRegisterOK, token: s.token, requestID: header.requestID,
			}, nil); err != nil {
				return
			}
			netConn, ok := conn.(net.Conn)
			if !ok {
				s.writeError(conn, header, errors.New("reverse push stream does not support deadlines"))
				return
			}
			sender := &TCPPushSender{done: make(chan struct{})}
			s.mu.Lock()
			s.pushSenders[sender] = struct{}{}
			s.mu.Unlock()
			sender.run("", s.token, s.snapshot, *s.pushOpts, netConn, false, s.stop)
			stats := sender.Stats()
			s.mu.Lock()
			delete(s.pushSenders, sender)
			s.mu.Unlock()
			s.pushBytes.Add(stats.BytesPushed)
			s.pushRetries.Add(stats.Retries)
			return

		default:
			s.writeError(conn, header, fmt.Errorf("unexpected message type %d", header.type_))
			return
		}
	}
}

// FetchTCPMetadata performs the session handshake and downloads the bounded,
// checksummed Kernel metadata stream separately from memory pages.
func FetchTCPMetadata(ctx context.Context, address string, token SnapshotToken, maxChunk, maxMetadata uint64, timeout time.Duration) ([]byte, error) {
	if maxChunk == 0 || maxChunk > 2<<20 || maxMetadata == 0 || timeout <= 0 {
		return nil, errors.New("invalid TCP metadata fetch limits")
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	return FetchTCPMetadataConn(ctx, conn, token, maxChunk, maxMetadata, timeout)
}

// FetchTCPMetadataConn fetches metadata over a host-connected donated stream.
func FetchTCPMetadataConn(ctx context.Context, conn net.Conn, token SnapshotToken, maxChunk, maxMetadata uint64, timeout time.Duration) ([]byte, error) {
	if conn == nil || maxChunk == 0 || maxChunk > 2<<20 || maxMetadata == 0 || timeout <= 0 {
		return nil, errors.New("invalid TCP metadata connection or fetch limits")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	if err := writeTCPMessage(conn, tcpHeader{type_: tcpHello, token: token, requestID: 1}, nil); err != nil {
		return nil, err
	}
	hello, payload, err := readTCPResponse(conn, token, 1, maxMetadata)
	if err != nil {
		return nil, err
	}
	if hello.type_ == tcpError {
		return nil, fmt.Errorf("remote session hello: %s", payload)
	}
	if hello.type_ != tcpHelloOK || uint64(hello.length) > maxMetadata {
		return nil, fmt.Errorf("invalid remote metadata size %d", hello.length)
	}
	metadata := make([]byte, hello.length)
	for offset, requestID := uint64(0), uint32(2); offset < uint64(len(metadata)); requestID++ {
		length := min(maxChunk, uint64(len(metadata))-offset)
		if err := writeTCPMessage(conn, tcpHeader{
			type_: tcpMetadataPull, token: token, requestID: requestID,
			offset: offset, length: uint32(length),
		}, nil); err != nil {
			return nil, err
		}
		response, chunk, err := readTCPResponse(conn, token, requestID, maxChunk)
		if err != nil {
			return nil, err
		}
		if response.type_ != tcpMetadataData || response.offset != offset || uint64(response.length) != length {
			return nil, errors.New("mismatched TCP metadata response")
		}
		copy(metadata[offset:], chunk)
		offset += length
	}
	return metadata, nil
}

func (s *TCPPageServer) writeError(conn io.Writer, request tcpHeader, err error) {
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
	close(s.stop)
	var err error
	if s.listener != nil {
		err = s.listener.Close()
	}
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

type tcpAckSubmission struct {
	requestID uint32
	offset    uint64
	length    uint64
	done      chan error
}

// TCPRemoteReader implements stateio.AsyncReader over TCP. Each worker uses a
// separate ordered connection, allowing MaxParallel independent page pulls.
type TCPRemoteReader struct {
	stateio.NoRegisterClientFD

	address       string
	token         SnapshotToken
	maxReadBytes  uint64
	maxRanges     int
	timeout       time.Duration
	retryLimit    int
	retryBackoff  time.Duration
	ackBatchPages int
	ackInterval   time.Duration
	maxAckEntries int

	ctx        context.Context
	cancel     context.CancelFunc
	subs       chan tcpReadSubmission
	cmps       chan stateio.Completion
	ackSubs    chan tcpAckSubmission
	wg         sync.WaitGroup
	once       sync.Once
	mu         sync.Mutex
	conns      map[net.Conn]struct{}
	spareConns chan net.Conn

	submitted      atomic.Uint64
	completed      atomic.Uint64
	failed         atomic.Uint64
	bytesInstalled atomic.Uint64
	retries        atomic.Uint64
	ackOnlyRetries atomic.Uint64
}

// TCPRemoteReaderOpts configures remote page reads.
type TCPRemoteReaderOpts struct {
	MaxReadBytes  uint64
	MaxRanges     int
	MaxParallel   int
	Timeout       time.Duration
	RetryLimit    int
	RetryBackoff  time.Duration
	AckBatchPages int
	AckInterval   time.Duration
}

// TCPRemoteReaderStats reports target-side demand paging activity.
type TCPRemoteReaderStats struct {
	SubmittedReads uint64
	CompletedReads uint64
	FailedReads    uint64
	BytesInstalled uint64
	Retries        uint64
	AckOnlyRetries uint64
}

// TCPRemoteReaderDebugState is a token-free transaction snapshot suitable for
// logs, diagnostics, and experiment result capture.
type TCPRemoteReaderDebugState struct {
	Address       string
	MaxReadBytes  uint64
	MaxRanges     int
	MaxParallel   int
	Timeout       time.Duration
	RetryLimit    int
	RetryBackoff  time.Duration
	AckBatchPages int
	AckInterval   time.Duration
	Stats         TCPRemoteReaderStats
}

// NewTCPRemoteReader constructs a TCP page reader. timeout applies to dialing
// and every request/response pair.
func NewTCPRemoteReader(address string, token SnapshotToken, maxReadBytes uint64, maxRanges, maxParallel int, timeout time.Duration) (*TCPRemoteReader, error) {
	return NewTCPRemoteReaderWithOpts(address, token, TCPRemoteReaderOpts{
		MaxReadBytes:  maxReadBytes,
		MaxRanges:     maxRanges,
		MaxParallel:   maxParallel,
		Timeout:       timeout,
		RetryLimit:    3,
		RetryBackoff:  10 * time.Millisecond,
		AckBatchPages: 1,
	})
}

// NewTCPRemoteReaderWithOpts constructs a configurable TCP page reader.
func NewTCPRemoteReaderWithOpts(address string, token SnapshotToken, opts TCPRemoteReaderOpts) (*TCPRemoteReader, error) {
	return newTCPRemoteReader(address, token, opts, nil)
}

// NewTCPRemoteReaderWithConns uses host-connected streams donated to an
// isolated target. No host network syscall is required by the reader.
func NewTCPRemoteReaderWithConns(address string, token SnapshotToken, opts TCPRemoteReaderOpts, conns []net.Conn) (*TCPRemoteReader, error) {
	if len(conns) == 0 {
		return nil, errors.New("TCP remote reader connection pool is empty")
	}
	return newTCPRemoteReader(address, token, opts, conns)
}

func newTCPRemoteReader(address string, token SnapshotToken, opts TCPRemoteReaderOpts, donated []net.Conn) (*TCPRemoteReader, error) {
	if address == "" {
		return nil, errors.New("TCP remote reader address is empty")
	}
	if opts.MaxReadBytes < hostarch.PageSize || !hostarch.IsPageAligned(opts.MaxReadBytes) || opts.MaxReadBytes > 2<<20 {
		return nil, fmt.Errorf("invalid maximum TCP read size %d", opts.MaxReadBytes)
	}
	if opts.MaxRanges <= 0 || opts.MaxParallel <= 0 {
		return nil, fmt.Errorf("max ranges and parallelism must be positive")
	}
	if opts.Timeout <= 0 {
		return nil, fmt.Errorf("TCP page timeout must be positive")
	}
	if opts.RetryLimit < 0 || opts.RetryBackoff < 0 {
		return nil, errors.New("TCP retry limit and backoff must be non-negative")
	}
	if opts.AckBatchPages == 0 {
		opts.AckBatchPages = 1
	}
	if opts.AckBatchPages < 0 || opts.AckInterval < 0 {
		return nil, errors.New("TCP acknowledgement batch size and interval must be non-negative")
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &TCPRemoteReader{
		address:       address,
		token:         token,
		maxReadBytes:  opts.MaxReadBytes,
		maxRanges:     opts.MaxRanges,
		timeout:       opts.Timeout,
		retryLimit:    opts.RetryLimit,
		retryBackoff:  opts.RetryBackoff,
		ackBatchPages: opts.AckBatchPages,
		ackInterval:   opts.AckInterval,
		maxAckEntries: max(1, int(opts.MaxReadBytes/tcpAckRangeSize)),
		ctx:           ctx,
		cancel:        cancel,
		subs:          make(chan tcpReadSubmission, opts.MaxParallel),
		cmps:          make(chan stateio.Completion, opts.MaxParallel),
		conns:         make(map[net.Conn]struct{}),
	}
	if len(donated) != 0 {
		r.spareConns = make(chan net.Conn, len(donated))
		for _, conn := range donated {
			if conn == nil {
				continue
			}
			r.spareConns <- conn
		}
	}
	if opts.AckBatchPages > 1 {
		r.ackSubs = make(chan tcpAckSubmission, opts.MaxParallel)
		r.wg.Add(1)
		go r.ackWorker()
	}
	r.wg.Add(opts.MaxParallel)
	for range opts.MaxParallel {
		go r.worker()
	}
	return r, nil
}

func (r *TCPRemoteReader) MaxReadBytes() uint64 { return r.maxReadBytes }
func (r *TCPRemoteReader) MaxRanges() int       { return r.maxRanges }
func (r *TCPRemoteReader) MaxParallel() int     { return cap(r.subs) }

// Stats returns target-side paging counters.
func (r *TCPRemoteReader) Stats() TCPRemoteReaderStats {
	return TCPRemoteReaderStats{
		SubmittedReads: r.submitted.Load(), CompletedReads: r.completed.Load(),
		FailedReads: r.failed.Load(), BytesInstalled: r.bytesInstalled.Load(),
		Retries: r.retries.Load(), AckOnlyRetries: r.ackOnlyRetries.Load(),
	}
}

// DebugState returns reader configuration and current counters without the
// session token.
func (r *TCPRemoteReader) DebugState() TCPRemoteReaderDebugState {
	return TCPRemoteReaderDebugState{
		Address: r.address, MaxReadBytes: r.maxReadBytes, MaxRanges: r.maxRanges,
		MaxParallel: cap(r.subs), Timeout: r.timeout, RetryLimit: r.retryLimit,
		RetryBackoff: r.retryBackoff, Stats: r.Stats(),
		AckBatchPages: r.ackBatchPages, AckInterval: r.ackInterval,
	}
}

func (r *TCPRemoteReader) AddRead(id int, off int64, _ stateio.DestinationFile, _ memmap.FileRange, dstMap []byte) {
	r.submitted.Add(1)
	r.subs <- tcpReadSubmission{id: id, offset: off, total: uint64(len(dstMap)), dst: stateio.LocalClientMapping(dstMap)}
}

func (r *TCPRemoteReader) AddReadv(id int, off int64, total uint64, _ stateio.DestinationFile, _ []memmap.FileRange, dstMaps []unix.Iovec) {
	r.submitted.Add(1)
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
		if r.spareConns != nil {
			for {
				select {
				case conn := <-r.spareConns:
					conn.Close()
				default:
					return
				}
			}
		}
	})
	r.wg.Wait()
	return nil
}

func (r *TCPRemoteReader) connect() (net.Conn, error) {
	if r.spareConns != nil {
		select {
		case conn := <-r.spareConns:
			return conn, nil
		default:
			return nil, errors.New("donated TCP connection pool is exhausted")
		}
	}
	dialer := net.Dialer{Timeout: r.timeout}
	return dialer.DialContext(r.ctx, "tcp", r.address)
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
			n, err := r.performReadWithRetry(&conn, sub, r.ackSubs == nil)
			if err == nil && r.ackSubs != nil {
				err = r.batchAcknowledge(sub)
				if err != nil {
					n = 0
				}
			}
			if err != nil {
				r.failed.Add(1)
			} else {
				r.completed.Add(1)
			}
			if conn != nil && err != nil {
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

func (r *TCPRemoteReader) performReadWithRetry(conn *net.Conn, sub tcpReadSubmission, acknowledge bool) (uint64, error) {
	if sub.offset < 0 || sub.total == 0 || sub.total > r.maxReadBytes || sub.total > uint64(^uint32(0)) ||
		!hostarch.IsPageAligned(uint64(sub.offset)) || !hostarch.IsPageAligned(sub.total) {
		return 0, fmt.Errorf("invalid TCP remote read: off=%d len=%d", sub.offset, sub.total)
	}
	installed := false
	var lastErr error
	for attempt := 0; attempt <= r.retryLimit; attempt++ {
		if attempt != 0 {
			r.retries.Add(1)
			if installed {
				r.ackOnlyRetries.Add(1)
			}
		}
		if *conn == nil {
			var err error
			*conn, err = r.connect()
			if err != nil {
				lastErr = err
			} else {
				r.mu.Lock()
				r.conns[*conn] = struct{}{}
				r.mu.Unlock()
			}
		}
		if *conn != nil {
			var retryable bool
			installed, retryable, lastErr = r.performReadAttempt(*conn, sub, installed, acknowledge)
			if lastErr == nil {
				return sub.total, nil
			}
			r.mu.Lock()
			delete(r.conns, *conn)
			r.mu.Unlock()
			(*conn).Close()
			*conn = nil
			if !retryable {
				return 0, lastErr
			}
		}
		if attempt != r.retryLimit && r.retryBackoff != 0 {
			timer := time.NewTimer(r.retryBackoff)
			select {
			case <-r.ctx.Done():
				timer.Stop()
				return 0, r.ctx.Err()
			case <-timer.C:
			}
		}
	}
	return 0, fmt.Errorf("TCP remote read failed after %d attempts: %w", r.retryLimit+1, lastErr)
}

// performReadAttempt performs either pull+install+ACK or, if installed is
// true, an ACK-only recovery. Retrying ACK rather than PULL is essential: the
// server may have released the source page before its ACKOK was lost.
func (r *TCPRemoteReader) performReadAttempt(conn net.Conn, sub tcpReadSubmission, installed, acknowledge bool) (bool, bool, error) {
	deadline := time.Now().Add(r.timeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return installed, true, err
	}
	defer conn.SetDeadline(time.Time{})
	requestID := uint32(sub.id)
	if !installed {
		pull := tcpHeader{
			type_:     tcpPull,
			token:     r.token,
			requestID: requestID,
			offset:    uint64(sub.offset),
			length:    uint32(sub.total),
		}
		if err := writeTCPMessage(conn, pull, nil); err != nil {
			return false, true, err
		}
		response, payload, err := readTCPResponse(conn, r.token, requestID, r.maxReadBytes)
		if err != nil {
			return false, true, err
		}
		if response.type_ == tcpError {
			return false, false, fmt.Errorf("remote page server: %s", payload)
		}
		if response.type_ != tcpData || response.offset != uint64(sub.offset) || uint64(response.length) != sub.total {
			return false, false, fmt.Errorf("mismatched TCP page response: type=%d off=%d len=%d", response.type_, response.offset, response.length)
		}

		var copied uint64
		sub.dst.Mappings(func(_ int, dst []byte) bool {
			n := copy(dst, payload[copied:])
			copied += uint64(n)
			return copied < sub.total
		})
		if copied != sub.total {
			return false, false, fmt.Errorf("destination mappings contain %d bytes, want %d", copied, sub.total)
		}
		installed = true
		r.bytesInstalled.Add(sub.total)
	}
	if !acknowledge {
		return installed, false, nil
	}
	if err := writeTCPMessage(conn, tcpHeader{
		type_:     tcpAck,
		token:     r.token,
		requestID: requestID,
		offset:    uint64(sub.offset),
		length:    uint32(sub.total),
	}, nil); err != nil {
		return installed, true, err
	}
	ack, ackPayload, err := readTCPResponse(conn, r.token, requestID, r.maxReadBytes)
	if err != nil {
		return installed, true, err
	}
	if ack.type_ == tcpError {
		return installed, false, fmt.Errorf("remote page acknowledgement: %s", ackPayload)
	}
	if ack.type_ != tcpAckOK || ack.offset != uint64(sub.offset) || uint64(ack.length) != sub.total {
		return installed, false, errors.New("mismatched TCP page acknowledgement")
	}
	return installed, false, nil
}

func (r *TCPRemoteReader) batchAcknowledge(sub tcpReadSubmission) error {
	ack := tcpAckSubmission{
		requestID: uint32(sub.id), offset: uint64(sub.offset), length: sub.total, done: make(chan error, 1),
	}
	select {
	case r.ackSubs <- ack:
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
	select {
	case err := <-ack.done:
		return err
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

func (r *TCPRemoteReader) ackWorker() {
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
	batchID := uint32(1)
	for {
		var first tcpAckSubmission
		select {
		case <-r.ctx.Done():
			return
		case first = <-r.ackSubs:
		}
		batch := []tcpAckSubmission{first}
		pages := int((first.length + hostarch.PageSize - 1) / hostarch.PageSize)
		var timer *time.Timer
		var timerC <-chan time.Time
		if r.ackInterval != 0 {
			timer = time.NewTimer(r.ackInterval)
			timerC = timer.C
		}
		canceled := false
	collect:
		for pages < r.ackBatchPages && len(batch) < r.maxAckEntries {
			if timerC == nil {
				select {
				case ack := <-r.ackSubs:
					batch = append(batch, ack)
					pages += int((ack.length + hostarch.PageSize - 1) / hostarch.PageSize)
				default:
					break collect
				}
				continue
			}
			select {
			case ack := <-r.ackSubs:
				batch = append(batch, ack)
				pages += int((ack.length + hostarch.PageSize - 1) / hostarch.PageSize)
			case <-timerC:
				break collect
			case <-r.ctx.Done():
				canceled = true
				break collect
			}
		}
		if timer != nil && !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		var err error
		if canceled {
			err = r.ctx.Err()
		} else {
			err = r.sendAckBatchWithRetry(&conn, batchID, batch)
			batchID++
		}
		for _, ack := range batch {
			ack.done <- err
		}
		if canceled {
			return
		}
	}
}

func (r *TCPRemoteReader) sendAckBatchWithRetry(conn *net.Conn, batchID uint32, batch []tcpAckSubmission) error {
	payload := make([]byte, len(batch)*tcpAckRangeSize)
	for i, ack := range batch {
		entry := payload[i*tcpAckRangeSize : (i+1)*tcpAckRangeSize]
		binary.BigEndian.PutUint32(entry[0:4], ack.requestID)
		binary.BigEndian.PutUint64(entry[4:12], ack.offset)
		binary.BigEndian.PutUint32(entry[12:16], uint32(ack.length))
	}
	header := tcpHeader{
		type_: tcpAckBatch, token: r.token, requestID: batchID,
		length: uint32(len(payload)), checksum: crc32.ChecksumIEEE(payload),
	}
	var lastErr error
	for attempt := 0; attempt <= r.retryLimit; attempt++ {
		if attempt != 0 {
			r.retries.Add(1)
			r.ackOnlyRetries.Add(1)
		}
		if *conn == nil {
			var err error
			*conn, err = r.connect()
			if err != nil {
				lastErr = err
			} else {
				r.mu.Lock()
				r.conns[*conn] = struct{}{}
				r.mu.Unlock()
			}
		}
		if *conn != nil {
			if err := (*conn).SetDeadline(time.Now().Add(r.timeout)); err != nil {
				lastErr = err
			} else if err := writeTCPMessage(*conn, header, payload); err == nil {
				response, responsePayload, err := readTCPResponse(*conn, r.token, batchID, r.maxReadBytes)
				_ = (*conn).SetDeadline(time.Time{})
				if err == nil {
					if response.type_ == tcpError {
						return fmt.Errorf("remote page acknowledgement batch: %s", responsePayload)
					}
					if response.type_ == tcpAckBatchOK && response.offset == uint64(len(batch)) && response.length == 0 {
						return nil
					}
					return errors.New("mismatched TCP page acknowledgement batch response")
				}
				lastErr = err
			} else {
				_ = (*conn).SetDeadline(time.Time{})
				lastErr = err
			}
			r.mu.Lock()
			delete(r.conns, *conn)
			r.mu.Unlock()
			(*conn).Close()
			*conn = nil
		}
		if attempt != r.retryLimit && r.retryBackoff != 0 {
			timer := time.NewTimer(r.retryBackoff)
			select {
			case <-r.ctx.Done():
				timer.Stop()
				return r.ctx.Err()
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("TCP acknowledgement batch failed after %d attempts: %w", r.retryLimit+1, lastErr)
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
	if header.type_ == tcpData || header.type_ == tcpMetadataData || header.type_ == tcpError {
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
