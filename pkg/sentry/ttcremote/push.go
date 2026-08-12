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
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/hostarch"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
)

const (
	tcpPushRegister tcpMessageType = 32 + iota
	tcpPushRegisterOK
	tcpPush
	tcpPushOK
	tcpPushSkip
	tcpPushCommit
	tcpPushCommitOK
	tcpPushResolved
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
	install  func(uint64, []byte) (pgalloc.RemotePageInstallResult, error)
	maxPush  uint64

	mu        sync.Mutex
	closed    bool
	conns     map[io.ReadWriteCloser]struct{}
	receipts  map[pushReceiptKey]uint32
	wg        sync.WaitGroup
	installed atomic.Uint64
	skipped   atomic.Uint64
}

// NewTCPPushReceiver constructs a target-side push receiver.
func NewTCPPushReceiver(listener net.Listener, token SnapshotToken, pages *pgalloc.AsyncPagesFileLoad, maxPush uint64) (*TCPPushReceiver, error) {
	if listener == nil || pages == nil {
		return nil, errors.New("invalid TCP push receiver configuration")
	}
	return newTCPPushReceiver(listener, token, pages.TryInstallRemotePagesState, maxPush)
}

// NewTCPPushSession constructs a receiver whose connected stream is supplied
// by ServeStream. install atomically arbitrates each push against concurrent
// pull completion.
func NewTCPPushSession(token SnapshotToken, install func(uint64, []byte) (bool, error), maxPush uint64) (*TCPPushReceiver, error) {
	if install == nil {
		return nil, errors.New("invalid TCP push receiver configuration")
	}
	return newTCPPushReceiver(nil, token, func(offset uint64, data []byte) (pgalloc.RemotePageInstallResult, error) {
		installed, err := install(offset, data)
		if installed {
			return pgalloc.RemotePageInstallInstalled, err
		}
		return pgalloc.RemotePageInstallBusy, err
	}, maxPush)
}

// NewTCPPushStateSession constructs a receiver that preserves the distinction
// between a pull-owned range and one the target has already resolved. Runtime
// restore should use this form so the source can release pages materialized by
// restore itself without weakening pull/push arbitration.
func NewTCPPushStateSession(token SnapshotToken, install func(uint64, []byte) (pgalloc.RemotePageInstallResult, error), maxPush uint64) (*TCPPushReceiver, error) {
	return newTCPPushReceiver(nil, token, install, maxPush)
}

func newTCPPushReceiver(listener net.Listener, token SnapshotToken, install func(uint64, []byte) (pgalloc.RemotePageInstallResult, error), maxPush uint64) (*TCPPushReceiver, error) {
	if install == nil || maxPush < hostarch.PageSize || !hostarch.IsPageAligned(maxPush) || maxPush > 2<<20 {
		return nil, errors.New("invalid TCP push receiver configuration")
	}
	return &TCPPushReceiver{
		listener: listener,
		token:    token,
		install:  install,
		maxPush:  maxPush,
		conns:    make(map[io.ReadWriteCloser]struct{}),
		receipts: make(map[pushReceiptKey]uint32),
	}, nil
}

// Addr returns the reverse-channel listen address.
func (r *TCPPushReceiver) Addr() net.Addr {
	if r.listener == nil {
		return nil
	}
	return r.listener.Addr()
}

// Serve accepts push connections until Close.
func (r *TCPPushReceiver) Serve() error {
	if r.listener == nil {
		return errors.New("TCP push session has no listener")
	}
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

// ServeStream registers a reverse push lane on an already-connected page
// stream, then receives pushes until the source closes it.
func (r *TCPPushReceiver) ServeStream(conn io.ReadWriteCloser) error {
	if conn == nil {
		return errors.New("nil TCP push connection")
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		conn.Close()
		return net.ErrClosed
	}
	r.conns[conn] = struct{}{}
	r.wg.Add(1)
	r.mu.Unlock()
	if err := writeTCPMessage(conn, tcpHeader{type_: tcpPushRegister, token: r.token, requestID: 1}, nil); err != nil {
		r.finishConn(conn)
		return err
	}
	response, err := readTCPHeader(conn)
	if err != nil || response.type_ != tcpPushRegisterOK || response.token != r.token || response.requestID != 1 || response.offset != 0 || response.length != 0 || response.checksum != 0 {
		r.finishConn(conn)
		if err != nil {
			return err
		}
		return errors.New("invalid TCP push registration response")
	}
	r.serveConn(conn)
	return nil
}

func (r *TCPPushReceiver) finishConn(conn io.ReadWriteCloser) {
	conn.Close()
	r.mu.Lock()
	delete(r.conns, conn)
	r.mu.Unlock()
	r.wg.Done()
}

func (r *TCPPushReceiver) serveConn(conn io.ReadWriteCloser) {
	defer func() {
		r.finishConn(conn)
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
		result, err := r.install(header.offset, payload)
		if err != nil {
			return
		}
		responseType := tcpPushSkip
		switch result {
		case pgalloc.RemotePageInstallInstalled:
			responseType = tcpPushOK
			r.mu.Lock()
			r.receipts[key] = header.checksum
			r.mu.Unlock()
			r.installed.Add(uint64(header.length))
		case pgalloc.RemotePageInstallResolved:
			responseType = tcpPushResolved
		default:
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
	var err error
	if r.listener != nil {
		err = r.listener.Close()
	}
	for conn := range r.conns {
		conn.Close()
	}
	r.mu.Unlock()
	r.wg.Wait()
	return err
}

// Stats reports accepted and pull-owned bytes.
func (r *TCPPushReceiver) Stats() (installed, skipped uint64) {
	return r.installed.Load(), r.skipped.Load()
}

// TCPPushReceiverDebugState reports push results and uncommitted retry
// receipts. A nonzero receipt count is expected only while transactions are
// in flight or after a source disconnects between PUSH_OK and commit.
type TCPPushReceiverDebugState struct {
	BytesInstalled  uint64
	BytesSkipped    uint64
	PendingReceipts int
}

// DebugState returns current target-side push state.
func (r *TCPPushReceiver) DebugState() TCPPushReceiverDebugState {
	r.mu.Lock()
	receipts := len(r.receipts)
	r.mu.Unlock()
	return TCPPushReceiverDebugState{
		BytesInstalled: r.installed.Load(), BytesSkipped: r.skipped.Load(), PendingReceipts: receipts,
	}
}

// TCPPushSender consumes source COW hints and sends them to the target.
type TCPPushSender struct {
	done    chan struct{}
	err     atomic.Value
	bytes   atomic.Uint64
	retries atomic.Uint64
	hints   atomic.Uint64
	chunks  atomic.Uint64
	scans   atomic.Uint64
}

// TCPPushPolicy selects which source pages are proactively transferred.
type TCPPushPolicy string

const (
	// TCPPushDemandOnly disables proactive transfer.
	TCPPushDemandOnly TCPPushPolicy = "demand-only"
	// TCPPushDirtyOrphan transfers fork-time pages when the source breaks COW.
	TCPPushDirtyOrphan TCPPushPolicy = "dirty-orphan"
	// TCPPushMemoryPressure adds periodic draining above a retained-byte limit.
	TCPPushMemoryPressure TCPPushPolicy = "memory-pressure"
	// TCPPushEagerDrain immediately transfers every outstanding page.
	TCPPushEagerDrain TCPPushPolicy = "eager-drain"
)

// TCPPushSenderOpts configures proactive push transport and retry behavior.
type TCPPushSenderOpts struct {
	MaxPush      uint64
	Timeout      time.Duration
	RetryLimit   int
	RetryBackoff time.Duration
	Policy       TCPPushPolicy
	// AtomicLogicalChunks expands every proactive candidate to the complete
	// extant 1 MiB logical chunk used by the target residency protocol.
	AtomicLogicalChunks bool
	// RetainedHighWater triggers a full outstanding-range scan in
	// memory-pressure mode.
	RetainedHighWater uint64
	ScanInterval      time.Duration
}

// StartTCPPushSender starts a source-side reverse-channel worker.
func StartTCPPushSender(address string, token SnapshotToken, snapshot *pgalloc.RemoteSnapshot, maxPush uint64, timeout time.Duration) (*TCPPushSender, error) {
	return StartTCPPushSenderWithOpts(address, token, snapshot, TCPPushSenderOpts{
		MaxPush: maxPush, Timeout: timeout, RetryLimit: 3, RetryBackoff: 10 * time.Millisecond,
		Policy: TCPPushDirtyOrphan,
	})
}

// StartTCPPushSenderWithOpts starts a configurable source-side reverse-channel
// worker. Every chunk keeps the same transaction identity across retries.
func StartTCPPushSenderWithOpts(address string, token SnapshotToken, snapshot *pgalloc.RemoteSnapshot, opts TCPPushSenderOpts) (*TCPPushSender, error) {
	if opts.Policy == "" {
		opts.Policy = TCPPushDirtyOrphan
	}
	if address == "" || snapshot == nil {
		return nil, errors.New("invalid TCP push sender configuration")
	}
	if err := validateTCPPushSenderOpts(&opts); err != nil {
		return nil, err
	}
	s := &TCPPushSender{done: make(chan struct{})}
	go s.run(address, token, snapshot, opts, nil, true, nil)
	return s, nil
}

func validateTCPPushSenderOpts(opts *TCPPushSenderOpts) error {
	if opts.MaxPush < hostarch.PageSize || !hostarch.IsPageAligned(opts.MaxPush) ||
		opts.MaxPush > 2<<20 || opts.Timeout <= 0 || opts.RetryLimit < 0 || opts.RetryBackoff < 0 {
		return errors.New("invalid TCP push sender configuration")
	}
	switch opts.Policy {
	case TCPPushDemandOnly, TCPPushDirtyOrphan:
	case TCPPushEagerDrain:
		if opts.ScanInterval <= 0 {
			opts.ScanInterval = 10 * time.Millisecond
		}
	case TCPPushMemoryPressure:
		if opts.RetainedHighWater < hostarch.PageSize {
			return errors.New("memory-pressure push requires a retained high-water mark of at least one page")
		}
		if opts.ScanInterval <= 0 {
			opts.ScanInterval = 10 * time.Millisecond
		}
	default:
		return fmt.Errorf("unknown TCP push policy %q", opts.Policy)
	}
	return nil
}

func (s *TCPPushSender) run(address string, token SnapshotToken, snapshot *pgalloc.RemoteSnapshot, opts TCPPushSenderOpts, initial net.Conn, allowDial bool, stop <-chan struct{}) {
	defer close(s.done)
	conn := initial
	defer func() {
		if conn != nil {
			conn.Close()
		}
	}()
	requestID := uint32(1)
	hints := snapshot.PushHints()
	if opts.Policy == TCPPushDemandOnly {
		for range hints {
			s.hints.Add(1)
		}
		return
	}
	if opts.Policy == TCPPushEagerDrain {
		outstanding, err := snapshot.OutstandingStreamRanges(opts.MaxPush)
		if err != nil {
			s.err.Store(err)
			return
		}
		s.scans.Add(1)
		if err := s.sendRanges(&conn, address, token, snapshot, opts, outstanding, &requestID, allowDial); err != nil {
			s.err.Store(err)
			return
		}
		if snapshot.Stats().PagesOutstanding == 0 {
			return
		}
	}
	var ticker *time.Ticker
	var tickerC <-chan time.Time
	if opts.Policy == TCPPushMemoryPressure || opts.Policy == TCPPushEagerDrain {
		ticker = time.NewTicker(opts.ScanInterval)
		defer ticker.Stop()
		tickerC = ticker.C
	}
	for {
		select {
		case <-stop:
			return
		case hint, ok := <-hints:
			if !ok {
				return
			}
			merged, consumed, closed := collectCoalescedPushHints(hint, hints)
			s.hints.Add(uint64(consumed))
			if err := s.sendRanges(&conn, address, token, snapshot, opts, merged, &requestID, allowDial); err != nil {
				s.err.Store(err)
				return
			}
			if closed {
				return
			}
		case <-tickerC:
			stats := snapshot.Stats()
			if opts.Policy == TCPPushEagerDrain && stats.PagesOutstanding == 0 {
				return
			}
			if opts.Policy == TCPPushMemoryPressure && stats.PagesOutstanding*hostarch.PageSize < opts.RetainedHighWater {
				continue
			}
			outstanding, err := snapshot.OutstandingStreamRanges(opts.MaxPush)
			if err != nil {
				s.err.Store(err)
				return
			}
			s.scans.Add(1)
			if err := s.sendRanges(&conn, address, token, snapshot, opts, outstanding, &requestID, allowDial); err != nil {
				s.err.Store(err)
				return
			}
		}
	}
}

func (s *TCPPushSender) sendRanges(conn *net.Conn, address string, token SnapshotToken, snapshot *pgalloc.RemoteSnapshot, opts TCPPushSenderOpts, ranges []pgalloc.RemotePushHint, requestID *uint32, allowDial bool) error {
	var err error
	if opts.AtomicLogicalChunks {
		ranges, err = snapshot.AtomicPushRanges(ranges, opts.MaxPush)
		if err != nil {
			return err
		}
	}
	for _, candidate := range ranges {
		for off, remaining := candidate.StreamOffset, candidate.Length; remaining != 0; {
			length := min(remaining, opts.MaxPush)
			payload := make([]byte, length)
			if n, err := snapshot.CopyStreamAt(payload, int64(off)); err != nil || uint64(n) != length {
				break
			}
			id := *requestID
			*requestID = *requestID + 1
			header := tcpHeader{type_: tcpPush, token: token, requestID: id, offset: off, length: uint32(length), checksum: crc32.ChecksumIEEE(payload)}
			response, err := s.pushWithRetry(conn, address, token, header, payload, opts, allowDial)
			if err != nil {
				return err
			}
			if response.offset != off || uint64(response.length) != length {
				return fmt.Errorf("mismatched TCP push response")
			}
			switch response.type_ {
			case tcpPushOK:
				commit := header
				commit.type_ = tcpPushCommit
				commit.checksum = 0
				response, err = s.pushWithRetry(conn, address, token, commit, nil, opts, allowDial)
				if err != nil {
					return err
				}
				if response.type_ != tcpPushCommitOK {
					return fmt.Errorf("unexpected TCP push commit response %d", response.type_)
				}
				if err := snapshot.AcknowledgeStreamRange(off, length); err != nil {
					return err
				}
				s.bytes.Add(length)
				s.chunks.Add(1)
			case tcpPushSkip:
				// A large push may straddle a range already materialized by
				// restore and a range owned by a concurrent pull. Refine it to
				// page granularity so resolved pages can be acknowledged while
				// genuinely busy pages remain conservatively pinned.
				if length > hostarch.PageSize && !opts.AtomicLogicalChunks {
					left := hostarch.PageRoundDown(length / 2)
					if left == 0 {
						left = hostarch.PageSize
					}
					parts := []pgalloc.RemotePushHint{
						{StreamOffset: off, Length: left},
						{StreamOffset: off + left, Length: length - left},
					}
					if err := s.sendRanges(conn, address, token, snapshot, opts, parts, requestID, allowDial); err != nil {
						return err
					}
				}
			case tcpPushResolved:
				if err := snapshot.AcknowledgeStreamRange(off, length); err != nil {
					return err
				}
			default:
				return fmt.Errorf("unexpected TCP push response %d", response.type_)
			}
			off += length
			remaining -= length
		}
	}
	return nil
}

func collectCoalescedPushHints(first pgalloc.RemotePushHint, hints <-chan pgalloc.RemotePushHint) ([]pgalloc.RemotePushHint, int, bool) {
	batch := []pgalloc.RemotePushHint{first}
	closed := false
collect:
	for len(batch) < 256 {
		select {
		case hint, ok := <-hints:
			if !ok {
				closed = true
				break collect
			}
			batch = append(batch, hint)
		default:
			break collect
		}
	}
	consumed := len(batch)
	sort.Slice(batch, func(i, j int) bool { return batch[i].StreamOffset < batch[j].StreamOffset })
	merged := batch[:0]
	for _, candidate := range batch {
		if len(merged) == 0 {
			merged = append(merged, candidate)
			continue
		}
		last := &merged[len(merged)-1]
		if candidate.StreamOffset <= last.StreamOffset+last.Length {
			end := max(last.StreamOffset+last.Length, candidate.StreamOffset+candidate.Length)
			last.Length = end - last.StreamOffset
			continue
		}
		merged = append(merged, candidate)
	}
	return merged, consumed, closed
}

func (s *TCPPushSender) pushWithRetry(conn *net.Conn, address string, token SnapshotToken, header tcpHeader, payload []byte, opts TCPPushSenderOpts, allowDial bool) (tcpHeader, error) {
	var lastErr error
	for attempt := 0; attempt <= opts.RetryLimit; attempt++ {
		if *conn == nil && allowDial {
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
		if !allowDial {
			break
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

// TCPPushSenderStats reports source-side proactive push activity.
type TCPPushSenderStats struct {
	BytesPushed uint64
	Retries     uint64
	Hints       uint64
	Chunks      uint64
	Scans       uint64
}

// Stats returns source-side proactive push counters.
func (s *TCPPushSender) Stats() TCPPushSenderStats {
	return TCPPushSenderStats{
		BytesPushed: s.bytes.Load(), Retries: s.retries.Load(), Hints: s.hints.Load(), Chunks: s.chunks.Load(), Scans: s.scans.Load(),
	}
}
