// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cluster

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

const (
	defaultAuthorityApplyTimeout = 15 * time.Second
	authoritySnapshotRetain      = 3
	maxAuthoritySnapshotBytes    = 512 << 20
)

// AuthorityRaftOptions configures one member of the single cluster-wide Raft
// group. LocalDir must be node-local; no Raft WAL or snapshot is stored under
// the shared bulk-data root.
type AuthorityRaftOptions struct {
	NodeID        string
	LocalDir      string
	ListenAddress string
	Listener      net.Listener
	AdvertiseHost string
	ServerName    string
	ServerTLS     *tls.Config
	ClientTLS     *tls.Config
	AuthToken     string
	Bootstrap     bool
	ApplyTimeout  time.Duration
}

// AuthorityNotLeaderError tells callers where to retry an authority request.
type AuthorityNotLeaderError struct {
	LeaderID         string
	Leader           string
	LeaderControlURL string
	LeaderServerName string
}

func (e *AuthorityNotLeaderError) Error() string {
	if e.Leader == "" {
		return "TTC authority has no elected Raft leader"
	}
	if e.LeaderID != "" {
		return fmt.Sprintf("TTC authority request must be sent to Raft leader %s at %q", e.LeaderID, e.Leader)
	}
	return fmt.Sprintf("TTC authority request must be sent to Raft leader %q", e.Leader)
}

// AuthorityFSM adapts AuthorityState to Hashicorp Raft's deterministic FSM.
type AuthorityFSM struct {
	mu    sync.RWMutex
	state *AuthorityState
}

func NewAuthorityFSM() *AuthorityFSM { return &AuthorityFSM{state: NewAuthorityState()} }

func (f *AuthorityFSM) Apply(log *raft.Log) interface{} {
	var command AuthorityCommand
	if err := json.Unmarshal(log.Data, &command); err != nil {
		return AuthorityResult{Error: fmt.Sprintf("decoding authority command: %v", err)}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state.Apply(command)
}

func (f *AuthorityFSM) Snapshot() (raft.FSMSnapshot, error) {
	f.mu.RLock()
	data, err := f.state.SnapshotBytes()
	f.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return authorityFSMSnapshot(data), nil
}

func (f *AuthorityFSM) Restore(reader io.ReadCloser) error {
	defer reader.Close()
	data, err := io.ReadAll(io.LimitReader(reader, maxAuthoritySnapshotBytes+1))
	if err != nil {
		return err
	}
	if len(data) > maxAuthoritySnapshotBytes {
		return fmt.Errorf("raft authority snapshot exceeds %d bytes", maxAuthoritySnapshotBytes)
	}
	state, err := RestoreAuthorityState(data)
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.state = state
	f.mu.Unlock()
	return nil
}

func (f *AuthorityFSM) copyState() (*AuthorityState, error) {
	f.mu.RLock()
	// Requests and RequestOrder are replicated replay protection, not cluster
	// state returned by a read. Including them made every catalog lookup copy
	// all historical command results, so steady-state latency grew with Raft
	// history even after transactions and placements were retired. Real Raft
	// snapshots still use Snapshot() above and retain the replay window.
	view := *f.state
	view.Requests = nil
	view.RequestOrder = nil
	data, err := view.SnapshotBytes()
	f.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	return RestoreAuthorityState(data)
}

func (f *AuthorityFSM) appliedTransaction(id string) (Transaction, bool) {
	f.mu.RLock()
	transaction, ok := f.state.Transactions[id]
	f.mu.RUnlock()
	return transaction, ok
}

func (f *AuthorityFSM) appliedNode(id string) (NodeRecord, bool) {
	f.mu.RLock()
	node, ok := f.state.Nodes[id]
	f.mu.RUnlock()
	return node, ok
}

type authorityFSMSnapshot []byte

func (s authorityFSMSnapshot) Persist(sink raft.SnapshotSink) error {
	if _, err := sink.Write(s); err != nil {
		_ = sink.Cancel()
		return err
	}
	return sink.Close()
}

func (authorityFSMSnapshot) Release() {}

type authorityRaftAddress string

func (a authorityRaftAddress) Network() string { return "tcp" }
func (a authorityRaftAddress) String() string  { return string(a) }

// raftAddress encodes the expected TLS identity with the dial address because
// raft.StreamLayer.Dial is intentionally given only a ServerAddress.
func raftAddress(serverName, host string) (string, error) {
	if strings.TrimSpace(serverName) != serverName || serverName == "" || strings.Contains(serverName, "@") {
		return "", fmt.Errorf("invalid Raft TLS server name %q", serverName)
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		return "", fmt.Errorf("invalid Raft host address %q: %w", host, err)
	}
	return serverName + "@" + host, nil
}

func parseRaftAddress(address string) (serverName, host string, err error) {
	separator := strings.IndexByte(address, '@')
	if separator <= 0 || separator == len(address)-1 {
		return "", "", fmt.Errorf("invalid Raft address %q", address)
	}
	serverName, host = address[:separator], address[separator+1:]
	if _, err := raftAddress(serverName, host); err != nil {
		return "", "", err
	}
	return serverName, host, nil
}

func validateRaftServerAddress(address string) error {
	if strings.Contains(address, "@") {
		_, _, err := parseRaftAddress(address)
		return err
	}
	if _, _, err := net.SplitHostPort(address); err != nil {
		return fmt.Errorf("invalid Raft address %q: %w", address, err)
	}
	return nil
}

type authorityStreamLayer struct {
	net.Listener
	advertise authorityRaftAddress
	clientTLS *tls.Config
	authToken string
}

func (l *authorityStreamLayer) Addr() net.Addr { return l.advertise }

func (l *authorityStreamLayer) Dial(address raft.ServerAddress, timeout time.Duration) (net.Conn, error) {
	host := string(address)
	serverName := ""
	var err error
	if l.clientTLS != nil {
		serverName, host, err = parseRaftAddress(host)
		if err != nil {
			return nil, err
		}
	}
	dialer := &net.Dialer{Timeout: timeout}
	connection, err := dialer.Dial("tcp", host)
	if err != nil {
		return nil, err
	}
	if l.clientTLS != nil {
		config := l.clientTLS.Clone()
		config.ServerName = serverName
		tlsConnection := tls.Client(connection, config)
		if err := tlsConnection.Handshake(); err != nil {
			connection.Close()
			return nil, err
		}
		connection = tlsConnection
	}
	if err := writeRaftToken(connection, l.authToken); err != nil {
		connection.Close()
		return nil, err
	}
	return connection, nil
}

func (l *authorityStreamLayer) Accept() (net.Conn, error) {
	connection, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if err := readRaftToken(connection, l.authToken); err != nil {
		connection.Close()
		return nil, err
	}
	return connection, nil
}

func writeRaftToken(w io.Writer, token string) error {
	if len(token) > 4096 {
		return errors.New("cluster authentication token is too long")
	}
	if _, err := io.WriteString(w, raftWireMagic); err != nil {
		return err
	}
	var size [2]byte
	binary.BigEndian.PutUint16(size[:], uint16(len(token)))
	if _, err := w.Write(size[:]); err != nil {
		return err
	}
	_, err := io.WriteString(w, token)
	return err
}

func readRaftToken(r io.Reader, want string) error {
	var magic [len(raftWireMagic)]byte
	if _, err := io.ReadFull(r, magic[:]); err != nil {
		return err
	}
	if string(magic[:]) != raftWireMagic {
		return errors.New("invalid TTC Raft protocol preface")
	}
	var size [2]byte
	if _, err := io.ReadFull(r, size[:]); err != nil {
		return err
	}
	n := int(binary.BigEndian.Uint16(size[:]))
	if n > 4096 {
		return errors.New("cluster authentication token is too long")
	}
	got := make([]byte, n)
	if _, err := io.ReadFull(r, got); err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(got, []byte(want)) != 1 {
		return errors.New("invalid cluster authentication token")
	}
	return nil
}

// AuthorityRaftNode owns the consensus engine and node-local durable log.
type AuthorityRaftNode struct {
	id          string
	raft        *raft.Raft
	fsm         *AuthorityFSM
	transport   *raft.NetworkTransport
	store       *raftboltdb.BoltStore
	listener    net.Listener
	timeout     time.Duration
	hadState    bool
	leaderMu    sync.RWMutex
	leaderSince time.Time
}

// OpenAuthorityRaftNode opens or creates one persistent Raft member.
func OpenAuthorityRaftNode(options AuthorityRaftOptions) (*AuthorityRaftNode, error) {
	if !transactionIDPattern.MatchString(options.NodeID) {
		return nil, fmt.Errorf("invalid Raft node ID %q", options.NodeID)
	}
	if options.LocalDir == "" || !filepath.IsAbs(options.LocalDir) {
		return nil, errors.New("raft local directory must be an absolute node-local path")
	}
	if (options.ListenAddress == "" && options.Listener == nil) || options.AdvertiseHost == "" {
		return nil, errors.New("raft listen and advertise addresses are required")
	}
	if (options.ServerTLS == nil) != (options.ClientTLS == nil) {
		return nil, errors.New("raft server and client TLS must be configured together")
	}
	if err := os.MkdirAll(options.LocalDir, 0700); err != nil {
		return nil, err
	}
	plainListener := options.Listener
	var err error
	if plainListener == nil {
		plainListener, err = net.Listen("tcp", options.ListenAddress)
		if err != nil {
			return nil, fmt.Errorf("listening for Raft: %w", err)
		}
	} else if options.ServerTLS != nil {
		return nil, errors.New("an externally multiplexed Raft listener cannot use TLS")
	}
	advertiseHost := options.AdvertiseHost
	if host, port, splitErr := net.SplitHostPort(advertiseHost); splitErr == nil && port == "0" {
		_, actualPort, actualErr := net.SplitHostPort(plainListener.Addr().String())
		if actualErr != nil {
			plainListener.Close()
			return nil, fmt.Errorf("resolving allocated Raft port: %w", actualErr)
		}
		advertiseHost = net.JoinHostPort(host, actualPort)
	}
	advertise := advertiseHost
	listener := net.Listener(plainListener)
	var clientTLS *tls.Config
	if options.ServerTLS != nil {
		advertise, err = raftAddress(options.ServerName, advertiseHost)
		if err != nil {
			plainListener.Close()
			return nil, err
		}
		listener = tls.NewListener(plainListener, options.ServerTLS.Clone())
		clientTLS = options.ClientTLS.Clone()
	}
	stream := &authorityStreamLayer{Listener: listener, advertise: authorityRaftAddress(advertise), clientTLS: clientTLS, authToken: options.AuthToken}
	transport := raft.NewNetworkTransportWithConfig(&raft.NetworkTransportConfig{
		Stream: stream, MaxPool: 4, MaxRPCsInFlight: 2, Timeout: 10 * time.Second,
	})
	store, err := raftboltdb.NewBoltStore(filepath.Join(options.LocalDir, "raft.db"))
	if err != nil {
		transport.Close()
		plainListener.Close()
		return nil, fmt.Errorf("opening Raft WAL: %w", err)
	}
	snapshots, err := raft.NewFileSnapshotStore(options.LocalDir, authoritySnapshotRetain, io.Discard)
	if err != nil {
		store.Close()
		transport.Close()
		plainListener.Close()
		return nil, fmt.Errorf("opening Raft snapshots: %w", err)
	}
	hasState, err := raft.HasExistingState(store, store, snapshots)
	if err != nil {
		store.Close()
		transport.Close()
		plainListener.Close()
		return nil, err
	}
	config := raft.DefaultConfig()
	config.LocalID = raft.ServerID(options.NodeID)
	config.LogOutput = io.Discard
	config.SnapshotThreshold = 8192
	config.SnapshotInterval = 2 * time.Minute
	// A TTC node whose execution lease expires is removed from the live voter
	// set, but the physical node is allowed to return later. Keep its Raft
	// engine alive long enough to apply the removal entry and a subsequent
	// AddNonvoter entry from the surviving leader. With ShutdownOnRemove set,
	// replay stops at the removal entry and a node with a stale local WAL can
	// never be re-admitted without deleting its only copy of the Raft log -- an
	// unsafe operation during a whole-cluster restart. An excluded engine has
	// no vote or proposal authority; TTC's replicated execution lease and
	// host-level guardian remain the data-plane fence.
	config.ShutdownOnRemove = false
	fsm := NewAuthorityFSM()
	engine, err := raft.NewRaft(config, fsm, store, store, snapshots, transport)
	if err != nil {
		store.Close()
		transport.Close()
		plainListener.Close()
		return nil, fmt.Errorf("starting Raft: %w", err)
	}
	node := &AuthorityRaftNode{id: options.NodeID, raft: engine, fsm: fsm, transport: transport, store: store, listener: plainListener, timeout: options.ApplyTimeout, hadState: hasState}
	go node.observeLeadership(engine.LeaderCh())
	if node.timeout == 0 {
		node.timeout = defaultAuthorityApplyTimeout
	}
	if options.Bootstrap {
		if !hasState {
			configuration := raft.Configuration{Servers: []raft.Server{{ID: raft.ServerID(options.NodeID), Address: raft.ServerAddress(advertise), Suffrage: raft.Voter}}}
			if err := engine.BootstrapCluster(configuration).Error(); err != nil {
				node.Close()
				return nil, fmt.Errorf("bootstrapping TTC Raft cluster: %w", err)
			}
		}
	}
	return node, nil
}

// HadExistingState reports whether this member opened a prior WAL/snapshot.
func (n *AuthorityRaftNode) HadExistingState() bool { return n != nil && n.hadState }

// Address returns the identity-bearing advertised Raft address.
func (n *AuthorityRaftNode) Address() string { return string(n.transport.LocalAddr()) }

func (n *AuthorityRaftNode) IsLeader() bool {
	return n != nil && n.raft != nil && n.raft.State() == raft.Leader
}

func (n *AuthorityRaftNode) observeLeadership(ch <-chan bool) {
	for leader := range ch {
		n.leaderMu.Lock()
		if leader {
			n.leaderSince = time.Now()
		} else {
			n.leaderSince = time.Time{}
		}
		n.leaderMu.Unlock()
	}
}

// LeadershipSafeFor reports whether every execution lease issued by a prior
// leader has had enough real time to self-fence. It uses this host's monotonic
// clock and is required before expiring or replacing another incarnation.
func (n *AuthorityRaftNode) LeadershipSafeFor(duration time.Duration) bool {
	if !n.IsLeader() {
		return false
	}
	n.leaderMu.RLock()
	since := n.leaderSince
	n.leaderMu.RUnlock()
	return !since.IsZero() && time.Since(since) >= duration
}

func (n *AuthorityRaftNode) currentTerm() (uint64, error) {
	if n == nil || n.raft == nil {
		return 0, errors.New("nil Raft authority")
	}
	term, err := strconv.ParseUint(n.raft.Stats()["term"], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("reading current Raft term: %w", err)
	}
	if term == 0 {
		return 0, errors.New("current Raft term is zero")
	}
	return term, nil
}

func (n *AuthorityRaftNode) Leader() string {
	if n == nil || n.raft == nil {
		return ""
	}
	return string(n.raft.Leader())
}

// LeaderID returns the Raft server identity associated with Leader.
func (n *AuthorityRaftNode) LeaderID() string {
	if n == nil || n.raft == nil {
		return ""
	}
	_, id := n.raft.LeaderWithID()
	return string(id)
}

// AppliedState returns the local applied state without claiming a linearizable
// read. It is suitable for diagnostics and locating the current leader, never
// for an ownership decision.
func (n *AuthorityRaftNode) AppliedState() (*AuthorityState, error) {
	if n == nil || n.fsm == nil {
		return nil, errors.New("nil Raft authority")
	}
	state, err := n.fsm.copyState()
	if err == nil {
		state.LeaderObservedAt = time.Now().UTC()
	}
	return state, err
}

// AppliedTransaction returns one transaction from the local applied FSM. It
// is intentionally non-linearizable and is suitable for high-rate background
// observation, not ownership or publication decisions.
func (n *AuthorityRaftNode) AppliedTransaction(id string) (Transaction, bool) {
	if n == nil || n.fsm == nil {
		return Transaction{}, false
	}
	return n.fsm.appliedTransaction(id)
}

// AppliedNode returns one node from the local applied FSM. Like
// AppliedTransaction, callers may use it for monitoring only.
func (n *AuthorityRaftNode) AppliedNode(id string) (NodeRecord, bool) {
	if n == nil || n.fsm == nil {
		return NodeRecord{}, false
	}
	return n.fsm.appliedNode(id)
}

// Apply appends one already timestamped deterministic command. Remote callers
// should send an untimestamped proposal to the leader-facing API instead.
func (n *AuthorityRaftNode) Apply(ctx context.Context, command AuthorityCommand) (AuthorityResult, error) {
	if n == nil {
		return AuthorityResult{}, errors.New("nil Raft authority")
	}
	if !n.IsLeader() {
		return AuthorityResult{}, &AuthorityNotLeaderError{LeaderID: n.LeaderID(), Leader: n.Leader()}
	}
	term, err := n.currentTerm()
	if err != nil {
		return AuthorityResult{}, err
	}
	if command.LeaderTerm != term {
		return AuthorityResult{}, errors.New("authority command was not issued in the current Raft term")
	}
	data, err := json.Marshal(command)
	if err != nil {
		return AuthorityResult{}, err
	}
	timeout := n.contextTimeout(ctx)
	if timeout <= 0 {
		return AuthorityResult{}, context.DeadlineExceeded
	}
	future := n.raft.Apply(data, timeout)
	if err := future.Error(); err != nil {
		return AuthorityResult{}, err
	}
	result, ok := future.Response().(AuthorityResult)
	if !ok {
		return AuthorityResult{}, fmt.Errorf("unexpected Raft FSM result %T", future.Response())
	}
	return result, nil
}

// Propose timestamps a mutation on the elected leader. Durations in command
// payloads are consequently interpreted against one clock before replication.
func (n *AuthorityRaftNode) Propose(ctx context.Context, requestID string, kind AuthorityCommandKind, payload any) (AuthorityResult, error) {
	if !n.IsLeader() {
		return AuthorityResult{}, &AuthorityNotLeaderError{LeaderID: n.LeaderID(), Leader: n.Leader()}
	}
	command, err := NewAuthorityCommand(requestID, time.Now().UTC(), kind, payload)
	if err != nil {
		return AuthorityResult{}, err
	}
	// Replacing or expiring an incarnation can authorize execution elsewhere.
	// After leader failover, wait at least the old grant duration so a node
	// using only its conservative monotonic-local deadline has self-fenced.
	if kind == AuthorityNodeRegister || kind == AuthorityNodeExpire {
		state, stateErr := n.AppliedState()
		if stateErr != nil {
			return AuthorityResult{}, stateErr
		}
		var nodeID, incarnation string
		var safety time.Duration
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
		if current, ok := state.Nodes[nodeID]; ok && current.Incarnation != incarnation || kind == AuthorityNodeExpire {
			if current, ok := state.Nodes[nodeID]; ok {
				safety = current.LeaseDuration
			}
			if safety > 0 && !n.LeadershipSafeFor(safety) {
				return AuthorityResult{}, errors.New("raft leader is waiting for prior-term execution leases to self-fence")
			}
		}
	}
	command.LeaderTerm, err = n.currentTerm()
	if err != nil {
		return AuthorityResult{}, err
	}
	return n.Apply(ctx, command)
}

func (n *AuthorityRaftNode) contextTimeout(ctx context.Context) time.Duration {
	timeout := n.timeout
	if deadline, ok := ctx.Deadline(); ok {
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
	}
	return timeout
}

// LinearizableState returns a snapshot after all preceding committed commands
// have been applied on the current leader.
func (n *AuthorityRaftNode) LinearizableState(ctx context.Context) (*AuthorityState, error) {
	if n == nil {
		return nil, errors.New("nil Raft authority")
	}
	if !n.IsLeader() {
		return nil, &AuthorityNotLeaderError{LeaderID: n.LeaderID(), Leader: n.Leader()}
	}
	timeout := n.contextTimeout(ctx)
	if err := n.raft.VerifyLeader().Error(); err != nil {
		return nil, err
	}
	if err := n.raft.Barrier(timeout).Error(); err != nil {
		return nil, err
	}
	return n.fsm.copyState()
}

// AddNonvoter admits a learner. Promotion is separate so a lagging joiner
// cannot immediately affect quorum.
func (n *AuthorityRaftNode) AddNonvoter(ctx context.Context, id, address string) error {
	if !n.IsLeader() {
		return &AuthorityNotLeaderError{LeaderID: n.LeaderID(), Leader: n.Leader()}
	}
	if !transactionIDPattern.MatchString(id) {
		return fmt.Errorf("invalid Raft member ID %q", id)
	}
	if err := validateRaftServerAddress(address); err != nil {
		return err
	}
	return n.raft.AddNonvoter(raft.ServerID(id), raft.ServerAddress(address), 0, n.contextTimeout(ctx)).Error()
}

// Promote promotes a caught-up learner to a voter.
func (n *AuthorityRaftNode) Promote(ctx context.Context, id, address string) error {
	if !n.IsLeader() {
		return &AuthorityNotLeaderError{LeaderID: n.LeaderID(), Leader: n.Leader()}
	}
	return n.raft.AddVoter(raft.ServerID(id), raft.ServerAddress(address), 0, n.contextTimeout(ctx)).Error()
}

// ReconcileVoters removes authoritatively fenced voters and promotes admitted,
// healthy learners until the bounded control-plane quorum reaches maxVoters.
// Joining itself never promotes: a process that crashes before committing its
// NodeReady record must remain unable to increase the quorum requirement.
func (n *AuthorityRaftNode) ReconcileVoters(ctx context.Context, maxVoters int) error {
	if !n.IsLeader() {
		return &AuthorityNotLeaderError{LeaderID: n.LeaderID(), Leader: n.Leader()}
	}
	if maxVoters <= 0 {
		maxVoters = 3
	}
	configuration, err := n.Configuration()
	if err != nil {
		return err
	}
	state, err := n.AppliedState()
	if err != nil {
		return err
	}
	// NodeOffline is a replicated incarnation fence: the node's execution
	// lease has expired (or it completed a clean drain), its placements and
	// provider authority have been revoked, and it may no longer contribute
	// useful state to quorum. Leaving such a server in the Raft configuration
	// can strand the last live voter when another member subsequently drains.
	// Remove one server at a time because each membership transition uses the
	// preceding configuration's quorum.
	for {
		removed := false
		for _, server := range configuration.Servers {
			record, ok := state.Nodes[string(server.ID)]
			if !ok || record.State != NodeOffline || string(server.ID) == n.id {
				continue
			}
			if err := n.RemoveMember(ctx, string(server.ID)); err != nil {
				return fmt.Errorf("removing fenced Raft member %q: %w", server.ID, err)
			}
			configuration, err = n.Configuration()
			if err != nil {
				return err
			}
			removed = true
			break
		}
		if !removed {
			break
		}
	}
	voters := 0
	var learners []raft.Server
	for _, server := range configuration.Servers {
		switch server.Suffrage {
		case raft.Voter:
			voters++
		case raft.Nonvoter:
			learners = append(learners, server)
		}
	}
	if voters >= maxVoters {
		return nil
	}
	sort.Slice(learners, func(i, j int) bool { return learners[i].ID < learners[j].ID })
	for _, learner := range learners {
		record, ok := state.Nodes[string(learner.ID)]
		if !ok || record.RaftAddress != string(learner.Address) || !record.LeaseActive(time.Now()) {
			continue
		}
		if record.State != NodeReady && record.State != NodePressured {
			continue
		}
		if err := n.Promote(ctx, string(learner.ID), string(learner.Address)); err != nil {
			return fmt.Errorf("promoting admitted learner %q: %w", learner.ID, err)
		}
		voters++
		if voters >= maxVoters {
			return nil
		}
	}
	return nil
}

// RemoveMember removes a node from quorum. Runtime drain/fencing must complete
// before calling this method.
func (n *AuthorityRaftNode) RemoveMember(ctx context.Context, id string) error {
	if !n.IsLeader() {
		return &AuthorityNotLeaderError{LeaderID: n.LeaderID(), Leader: n.Leader()}
	}
	return n.raft.RemoveServer(raft.ServerID(id), 0, n.contextTimeout(ctx)).Error()
}

// RetireMember maintains a compact three-voter control plane when possible,
// transfers leadership away from the retiring node, then removes it. The
// caller must first commit the node offline and prove that it owns no branch.
func (n *AuthorityRaftNode) RetireMember(ctx context.Context, id string) error {
	if !n.IsLeader() {
		return &AuthorityNotLeaderError{LeaderID: n.LeaderID(), Leader: n.Leader()}
	}
	configuration, err := n.Configuration()
	if err != nil {
		return err
	}
	var retiring *raft.Server
	var voters []raft.Server
	var learners []raft.Server
	for i := range configuration.Servers {
		server := configuration.Servers[i]
		if string(server.ID) == id {
			copy := server
			retiring = &copy
		}
		switch server.Suffrage {
		case raft.Voter:
			voters = append(voters, server)
		case raft.Nonvoter:
			learners = append(learners, server)
		}
	}
	if retiring == nil {
		return nil
	}
	if retiring.Suffrage == raft.Voter && len(voters) == 1 {
		return errors.New("refusing to retire the last Raft voter")
	}
	if retiring.Suffrage == raft.Voter && len(voters) <= 3 {
		state, _ := n.AppliedState()
		for _, learner := range learners {
			record, ok := state.Nodes[string(learner.ID)]
			if !ok || !record.LeaseActive(time.Now()) || (record.State != NodeReady && record.State != NodePressured) {
				continue
			}
			if err := n.Promote(ctx, string(learner.ID), string(learner.Address)); err != nil {
				return fmt.Errorf("promoting replacement voter: %w", err)
			}
			voters = append(voters, learner)
			break
		}
	}
	if id == n.id {
		for _, voter := range voters {
			if string(voter.ID) == id {
				continue
			}
			if err := n.raft.LeadershipTransferToServer(voter.ID, voter.Address).Error(); err != nil {
				return fmt.Errorf("transferring Raft leadership before retirement: %w", err)
			}
			// LeadershipTransferToServer waits for the handoff, but LeaderID's
			// observation may lag the completed future. Return the exact voter
			// selected above so the retiring daemon can ask that node to commit
			// RemoveServer without ever depending on the stale observation.
			return &AuthorityNotLeaderError{LeaderID: string(voter.ID), Leader: string(voter.Address)}
		}
		return errors.New("retiring Raft leader has no replacement voter")
	}
	return n.RemoveMember(ctx, id)
}

func (n *AuthorityRaftNode) Configuration() (raft.Configuration, error) {
	if n == nil {
		return raft.Configuration{}, errors.New("nil Raft authority")
	}
	future := n.raft.GetConfiguration()
	if err := future.Error(); err != nil {
		return raft.Configuration{}, err
	}
	return future.Configuration(), nil
}

// Close stops consensus and closes node-local durable state.
func (n *AuthorityRaftNode) Close() error {
	if n == nil {
		return nil
	}
	var result error
	if n.raft != nil {
		result = errors.Join(result, n.raft.Shutdown().Error())
	}
	if n.transport != nil {
		result = errors.Join(result, n.transport.Close())
	} else if n.listener != nil {
		result = errors.Join(result, n.listener.Close())
	}
	if n.store != nil {
		result = errors.Join(result, n.store.Close())
	}
	return result
}
