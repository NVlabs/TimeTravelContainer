// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultAuthorityApplyTimeout = 15 * time.Second
	// maxAuthorityBatch bounds the commands one sync of the log carries.
	maxAuthorityBatch = 256
)

// AuthorityOptions configures the holder of an epoch's authority.
type AuthorityOptions struct {
	NodeID string
	// Dir is the epoch's directory in the shared root, whose state
	// SharedCluster.PrepareEpoch made.
	Dir   string
	Epoch uint64
	// Held returns nil while this node holds the epoch's authority; see
	// AuthorityHolder.
	Held         func() error
	ApplyTimeout time.Duration
}

// AuthorityNotHolderError tells a caller which node to send an authority
// request to.
type AuthorityNotHolderError struct {
	HolderID          string
	ControlURL        string
	ControlServerName string
}

func (e *AuthorityNotHolderError) Error() string {
	if e.HolderID == "" {
		return "the TTC cluster authority has no holder"
	}
	return fmt.Sprintf("TTC authority requests go to its holder, node %s", e.HolderID)
}

// AuthorityFSM is the authority's state, as the commands applied so far made
// it.
type AuthorityFSM struct {
	mu    sync.RWMutex
	state *AuthorityState
}

// NewAuthorityFSM returns the state of a new cluster.
func NewAuthorityFSM() *AuthorityFSM { return &AuthorityFSM{state: NewAuthorityState()} }

func decodeAuthorityCommand(data []byte) (AuthorityCommand, error) {
	var command AuthorityCommand
	if err := json.Unmarshal(data, &command); err != nil {
		return AuthorityCommand{}, fmt.Errorf("decoding authority command: %w", err)
	}
	return command, nil
}

func (f *AuthorityFSM) apply(command AuthorityCommand) AuthorityResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.Apply(command)
}

// snapshot encodes the whole state, replay window included.
func (f *AuthorityFSM) snapshot() ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.state.SnapshotBytes()
}

func (f *AuthorityFSM) copyState() (*AuthorityState, error) {
	return f.query(WholeAuthority)
}

// query returns a copy of the records of the state that query names.
func (f *AuthorityFSM) query(query AuthorityQuery) (*AuthorityState, error) {
	data, err := f.encode(query)
	if err != nil {
		return nil, err
	}
	return decodeAuthorityView(data)
}

// encode encodes the records of the state that query names. Requests and
// RequestOrder are logged replay protection, not cluster state returned
// by a read, and no read has them: they would make every read copy all
// historical command results. Snapshots of the log keep them.
func (f *AuthorityFSM) encode(query AuthorityQuery) ([]byte, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.state.view(query).SnapshotBytes()
}

func (f *AuthorityFSM) revision() uint64 {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.state.Revision
}

func (f *AuthorityFSM) appliedTransaction(id string) (Transaction, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	transaction, ok := f.state.Transactions[id]
	return transaction, ok
}

func (f *AuthorityFSM) appliedNode(id string) (NodeRecord, bool) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	node, ok := f.state.Nodes[id]
	return node, ok
}

// AuthorityNode is a daemon's part in its cluster's authority: the authority
// itself while the daemon holds it, and otherwise the way to the node that
// does.
type AuthorityNode struct {
	id      string
	timeout time.Duration
	current atomic.Pointer[authorityEngine]
	// holder names the node that holds the authority, for a node that does
	// not: see AuthorityNotHolderError.
	holder func() (AuthorityRecord, bool, error)
}

// NewAuthorityNode returns the part in the authority of the node id, which
// holds none yet; holder names the node that does.
func NewAuthorityNode(id string, applyTimeout time.Duration, holder func() (AuthorityRecord, bool, error)) *AuthorityNode {
	if applyTimeout == 0 {
		applyTimeout = defaultAuthorityApplyTimeout
	}
	return &AuthorityNode{id: id, timeout: applyTimeout, holder: holder}
}

// Hold makes the node the holder of an epoch's authority, in place of any it
// held before.
func (n *AuthorityNode) Hold(options AuthorityOptions) error {
	if options.NodeID != n.id {
		return fmt.Errorf("node %s cannot hold the authority as node %s", n.id, options.NodeID)
	}
	if options.Held == nil || options.Dir == "" || options.Epoch == 0 {
		return errors.New("an epoch's authority needs its directory and its holder's fence")
	}
	if previous := n.current.Swap(nil); previous != nil {
		previous.close()
	}
	engine, err := openAuthorityEngine(options)
	if err != nil {
		return err
	}
	n.current.Store(engine)
	return nil
}

// Holds reports whether the node holds the authority.
func (n *AuthorityNode) Holds() bool {
	if n == nil {
		return false
	}
	engine := n.current.Load()
	return engine != nil && engine.lostErr() == nil
}

// Epoch returns the epoch whose authority the node holds, or 0.
func (n *AuthorityNode) Epoch() uint64 {
	if engine := n.current.Load(); engine != nil {
		return engine.epoch
	}
	return 0
}

// notHolder returns the error of a request to a node that does not hold the
// authority.
func (n *AuthorityNode) notHolder() error {
	result := &AuthorityNotHolderError{}
	if n.holder != nil {
		if record, ok, err := n.holder(); err == nil && ok && record.NodeID != n.id {
			result.HolderID, result.ControlURL, result.ControlServerName = record.NodeID, record.ControlURL, record.ControlServerName
		}
	}
	return result
}

// engine returns the engine of the authority the node holds, or the error
// that says where the authority is. A node whose hold ended, however it
// ended, sends requests on as a node that never held it does: a deposed
// holder's daemon stops through requests to its successor.
func (n *AuthorityNode) engine() (*authorityEngine, error) {
	if n == nil {
		return nil, errors.New("nil cluster authority")
	}
	if engine := n.holding(); engine != nil {
		return engine, nil
	}
	return nil, n.notHolder()
}

// holding returns the engine of the authority the node holds, or nil.
func (n *AuthorityNode) holding() *authorityEngine {
	engine := n.current.Load()
	if engine == nil || engine.lostErr() != nil {
		return nil
	}
	return engine
}

// HeldFor reports whether this node has held the authority for
// duration: every execution lease a previous holder granted has had that
// long, on this host's monotonic clock, to self-fence. It is required before
// expiring or replacing another incarnation.
func (n *AuthorityNode) HeldFor(duration time.Duration) bool {
	engine, err := n.engine()
	return err == nil && time.Since(engine.since) >= duration
}

// AppliedState returns the state the holder applied. Only the holder has
// one: a node that does not reads the holder's.
func (n *AuthorityNode) AppliedState() (*AuthorityState, error) {
	return n.AppliedQuery(WholeAuthority)
}

// AppliedRevision returns the revision of the state the holder applied, or
// 0 on a node that holds no authority.
func (n *AuthorityNode) AppliedRevision() uint64 {
	if engine := n.holding(); engine != nil {
		return engine.fsm.revision()
	}
	return 0
}

// AppliedQuery is AppliedState for the records query names.
func (n *AuthorityNode) AppliedQuery(query AuthorityQuery) (*AuthorityState, error) {
	engine := n.holding()
	if engine == nil {
		return nil, n.notHolder()
	}
	state, err := engine.fsm.query(query)
	if err == nil {
		state.HolderObservedAt = time.Now().UTC()
	}
	return state, err
}

// AppliedTransaction returns one transaction of the holder's state, for
// observation, not for ownership or publication decisions.
func (n *AuthorityNode) AppliedTransaction(id string) (Transaction, bool) {
	if engine := n.holding(); engine != nil {
		return engine.fsm.appliedTransaction(id)
	}
	return Transaction{}, false
}

// AppliedNode returns one node of the holder's state; see AppliedTransaction.
func (n *AuthorityNode) AppliedNode(id string) (NodeRecord, bool) {
	if engine := n.holding(); engine != nil {
		return engine.fsm.appliedNode(id)
	}
	return NodeRecord{}, false
}

// Apply appends one timestamped deterministic command to the log and applies
// it. Other nodes send an untimestamped proposal to the holder instead.
func (n *AuthorityNode) Apply(ctx context.Context, command AuthorityCommand) (AuthorityResult, error) {
	engine, err := n.engine()
	if err != nil {
		return AuthorityResult{}, err
	}
	if command.Epoch != engine.epoch {
		return AuthorityResult{}, errors.New("authority command was not issued in the current epoch")
	}
	data, err := json.Marshal(command)
	if err != nil {
		return AuthorityResult{}, err
	}
	timeout := n.contextTimeout(ctx)
	if timeout <= 0 {
		return AuthorityResult{}, context.DeadlineExceeded
	}
	result, err := engine.submit(ctx, timeout, command, data)
	if errors.Is(err, ErrAuthorityLost) || errors.Is(err, ErrAuthorityClosed) {
		// The log may hold the command, and its successor's state then: the
		// caller retries it there by its request ID.
		return AuthorityResult{}, n.notHolder()
	}
	return result, err
}

// Propose timestamps a mutation on the holder, so that durations in command
// payloads are read against one clock.
func (n *AuthorityNode) Propose(ctx context.Context, requestID string, kind AuthorityCommandKind, payload any) (AuthorityResult, error) {
	engine, err := n.engine()
	if err != nil {
		return AuthorityResult{}, err
	}
	command, err := NewAuthorityCommand(requestID, time.Now().UTC(), kind, payload)
	if err != nil {
		return AuthorityResult{}, err
	}
	// Replacing or expiring an incarnation can authorize execution elsewhere.
	// After a takeover, wait at least the old grant's duration so that a node
	// using only its conservative monotonic-local deadline has self-fenced;
	// an incarnation whose record grants no execution has nothing to wait
	// out.
	if kind == AuthorityNodeRegister || kind == AuthorityNodeExpire {
		var nodeID, incarnation string
		if kind == AuthorityNodeRegister {
			decoded, decodeErr := decodeAuthorityPayload[AuthorityNodeRegisterPayload](command)
			if decodeErr != nil {
				return AuthorityResult{}, decodeErr
			}
			nodeID, incarnation = decoded.Record.NodeID, decoded.Record.Incarnation
		} else {
			decoded, decodeErr := decodeAuthorityPayload[AuthorityNodeExpirePayload](command)
			if decodeErr != nil {
				return AuthorityResult{}, decodeErr
			}
			nodeID, incarnation = decoded.NodeID, decoded.Incarnation
		}
		current, ok := engine.fsm.appliedNode(nodeID)
		if ok && current.Incarnation != incarnation || kind == AuthorityNodeExpire {
			if ok && current.mayExecute() && !n.HeldFor(current.LeaseDuration) {
				return AuthorityResult{}, errors.New("the authority's holder is waiting for prior execution leases to self-fence")
			}
		}
	}
	command.Epoch = engine.epoch
	return n.Apply(ctx, command)
}

func (n *AuthorityNode) contextTimeout(ctx context.Context) time.Duration {
	timeout := n.timeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	return timeout
}

// LinearizableState returns the state after every command acknowledged so
// far.
func (n *AuthorityNode) LinearizableState(ctx context.Context) (*AuthorityState, error) {
	return n.LinearizableQuery(ctx, WholeAuthority)
}

// LinearizableQuery is LinearizableState for the records query names.
func (n *AuthorityNode) LinearizableQuery(ctx context.Context, query AuthorityQuery) (*AuthorityState, error) {
	data, err := n.EncodeLinearizable(ctx, query)
	if err != nil {
		return nil, err
	}
	return decodeAuthorityView(data)
}

// EncodeLinearizable is LinearizableQuery, encoded. The holder applies each
// command as it acknowledges it, so its state is the cluster's while it
// holds the authority: a read asks only whether it still does, and writes
// nothing.
func (n *AuthorityNode) EncodeLinearizable(ctx context.Context, query AuthorityQuery) ([]byte, error) {
	engine, err := n.engine()
	if err != nil {
		return nil, err
	}
	if err := engine.fence.wait(engine.held); err != nil {
		engine.lose(err)
		return nil, n.notHolder()
	}
	if engine.lostErr() != nil {
		// The node gave the authority up while the check ran.
		return nil, n.notHolder()
	}
	return engine.fsm.encode(query)
}

// Relinquish ends the node's hold of the authority, if it holds it, without
// waiting for requests in flight: from then on it answers as a node that never
// held it does. A holder relinquishes the authority before it records that it
// gave it up, so that no read it serves follows that record.
func (n *AuthorityNode) Relinquish() {
	if n == nil {
		return
	}
	if engine := n.current.Load(); engine != nil {
		engine.lose(ErrAuthorityClosed)
	}
}

// Close stops the authority the node holds, if any.
func (n *AuthorityNode) Close() error {
	if n == nil {
		return nil
	}
	if engine := n.current.Swap(nil); engine != nil {
		return engine.close()
	}
	return nil
}

// authorityEngine is the authority of an epoch its node holds: the state
// and the log, whose appends one goroutine makes, a batch at a time.
type authorityEngine struct {
	epoch uint64
	fsm   *AuthorityFSM
	log   *authorityLog
	held  func() error
	since time.Time
	// fence has the checks that the node still holds the authority.
	fence heldFence

	requests chan *authorityRequest
	done     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once

	// compacting is set while a snapshot is written in the background, which
	// background waits for.
	compacting atomic.Bool
	background sync.WaitGroup

	mu   sync.Mutex
	lost error
}

// authorityRequest is one command waiting for its batch.
type authorityRequest struct {
	command AuthorityCommand
	data    []byte
	reply   chan authorityReply
}

type authorityReply struct {
	result AuthorityResult
	err    error
}

func openAuthorityEngine(options AuthorityOptions) (*authorityEngine, error) {
	log, state, err := openAuthorityLog(options.Dir, options.Epoch)
	if err != nil {
		return nil, err
	}
	engine := &authorityEngine{
		epoch: options.Epoch, fsm: &AuthorityFSM{state: state}, log: log,
		held: options.Held, since: time.Now(),
		requests: make(chan *authorityRequest, maxAuthorityBatch),
		done:     make(chan struct{}), stopped: make(chan struct{}),
	}
	go engine.run()
	return engine, nil
}

// ErrAuthorityClosed is the error of a request to an authority its node has
// stopped holding.
var ErrAuthorityClosed = errors.New("this node stopped holding the cluster authority")

func (e *authorityEngine) lostErr() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lost
}

// lose records that the node no longer holds the authority, for err.
func (e *authorityEngine) lose(err error) {
	e.mu.Lock()
	if e.lost == nil {
		e.lost = err
	}
	e.mu.Unlock()
}

func (e *authorityEngine) submit(ctx context.Context, timeout time.Duration, command AuthorityCommand, data []byte) (AuthorityResult, error) {
	request := &authorityRequest{command: command, data: data, reply: make(chan authorityReply, 1)}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case e.requests <- request:
	case <-e.stopped:
		return AuthorityResult{}, ErrAuthorityClosed
	case <-ctx.Done():
		return AuthorityResult{}, ctx.Err()
	case <-timer.C:
		return AuthorityResult{}, context.DeadlineExceeded
	}
	// A command the log took may be applied even if its caller stops
	// waiting: callers retry by request ID.
	select {
	case reply := <-request.reply:
		return reply.result, reply.err
	case <-e.stopped:
		select {
		case reply := <-request.reply:
			return reply.result, reply.err
		default:
			return AuthorityResult{}, ErrAuthorityClosed
		}
	case <-ctx.Done():
		return AuthorityResult{}, ctx.Err()
	case <-timer.C:
		return AuthorityResult{}, context.DeadlineExceeded
	}
}

func (e *authorityEngine) run() {
	defer close(e.stopped)
	for {
		var batch []*authorityRequest
		select {
		case request := <-e.requests:
			batch = append(batch, request)
		case <-e.done:
			for batch := e.drain(nil); len(batch) != 0; batch = e.drain(nil) {
				e.fail(batch, ErrAuthorityClosed)
			}
			return
		}
		batch = e.drain(batch)
		e.commit(batch)
	}
}

// drain adds to batch the requests waiting, up to maxAuthorityBatch.
func (e *authorityEngine) drain(batch []*authorityRequest) []*authorityRequest {
	for len(batch) < maxAuthorityBatch {
		select {
		case request := <-e.requests:
			batch = append(batch, request)
		default:
			return batch
		}
	}
	return batch
}

func (e *authorityEngine) fail(batch []*authorityRequest, err error) {
	for _, request := range batch {
		request.reply <- authorityReply{err: err}
	}
}

// commit appends batch to the log, syncs it, and, if the node still holds
// the authority, applies and acknowledges it.
func (e *authorityEngine) commit(batch []*authorityRequest) {
	if err := e.lostErr(); err != nil {
		e.fail(batch, err)
		return
	}
	commands := make([][]byte, len(batch))
	for i, request := range batch {
		commands[i] = request.data
	}
	if _, err := e.log.append(commands); err != nil {
		// What the log holds now is unknown: stop holding the authority.
		err = fmt.Errorf("%w: writing its log: %v", ErrAuthorityLost, err)
		e.lose(err)
		e.fail(batch, err)
		return
	}
	// Acknowledge only what was written while this node held the authority;
	// see SharedCluster.
	if err := e.fence.wait(e.held); err != nil {
		e.lose(err)
		e.fail(batch, err)
		return
	}
	for _, request := range batch {
		request.reply <- authorityReply{result: e.fsm.apply(request.command)}
	}
	if e.log.full() && !e.compacting.Load() {
		e.compact()
	}
}

// compact begins a segment, and has the state as of the last command before
// it written as the snapshot in the background, which then removes the
// segments before it. Encoding the state holds up commands, as it did Raft's;
// writing it does not. A compaction that fails leaves the log whole, to be
// compacted once the next segment fills.
func (e *authorityEngine) compact() {
	snapshot, err := e.fsm.snapshot()
	if err != nil {
		return
	}
	index := e.log.next - 1
	if err := e.log.startSegment(); err != nil {
		return
	}
	e.compacting.Store(true)
	e.background.Add(1)
	go func() {
		defer e.background.Done()
		defer e.compacting.Store(false)
		_ = compactAuthorityLog(e.log.dir, e.epoch, index, snapshot)
	}()
}

func (e *authorityEngine) close() error {
	// Refuse new requests first: run answers those already queued.
	e.lose(ErrAuthorityClosed)
	e.stopOnce.Do(func() { close(e.done) })
	<-e.stopped
	e.background.Wait()
	return e.log.close()
}

// heldFence batches the checks that the node still holds the authority. A
// caller waits for a check that began after it called, and one check serves
// every caller that came while the one before it ran: reads and batches wait
// for one read of the authority record between them, not one each.
type heldFence struct {
	mu      sync.Mutex
	running bool
	// next is the check that begins once the running one ends.
	next *heldCheck
}

type heldCheck struct {
	done chan struct{}
	err  error
}

func (f *heldFence) wait(held func() error) error {
	f.mu.Lock()
	check := f.next
	if check == nil {
		check = &heldCheck{done: make(chan struct{})}
		f.next = check
	}
	if !f.running {
		f.running, f.next = true, nil
		go f.run(check, held)
	}
	f.mu.Unlock()
	<-check.done
	return check.err
}

// run makes check, and then each next one, until none waits.
func (f *heldFence) run(check *heldCheck, held func() error) {
	for check != nil {
		check.err = held()
		close(check.done)
		f.mu.Lock()
		check, f.next = f.next, nil
		f.running = check != nil
		f.mu.Unlock()
	}
}
