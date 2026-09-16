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

package cmd

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	sentryremote "gvisor.dev/gvisor/pkg/sentry/ttcremote"
	ttcpolicy "gvisor.dev/gvisor/pkg/ttcremote"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	"gvisor.dev/gvisor/runsc/boot"
	"gvisor.dev/gvisor/runsc/specutils"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

// distributedBatchControlConcurrency bounds target-side control requests from
// one sibling batch. Target runtime mutation is serialized per family anyway;
// allowing every sibling to occupy an HTTP handler only queues Raft state
// transitions behind slow Sentry work and can starve the target node's
// execution-lease renewal. A small window retains network/Raft overlap without
// making fan-out width part of the lease-safety envelope.
const distributedBatchControlConcurrency = 8

// distributedHandoffConcurrency keeps optional post-publication graph
// consolidation from competing unboundedly with foreground Sentry commands.
// A fan-out can publish thousands of children at once, but those children can
// continue faulting through their live relays while this bounded background
// queue makes them source-independent.
const distributedHandoffConcurrency = 2

func remoteExecSessions(sessions []boot.ExecSessionInfo) []cluster.RemoteExecSessionDescriptor {
	out := make([]cluster.RemoteExecSessionDescriptor, 0, len(sessions))
	for _, session := range sessions {
		if session.State != boot.ExecSessionRunning {
			continue
		}
		var status *uint32
		if session.ExitStatus != nil {
			value := *session.ExitStatus
			status = &value
		}
		out = append(out, cluster.RemoteExecSessionDescriptor{
			ID: session.ID, PID: int32(session.PID), ConsoleID: session.ConsoleID,
			Terminal: session.Terminal, State: string(session.State), CreatedAt: session.CreatedAt,
			ExitStatus: status,
		})
	}
	return out
}

func bootExecSessions(sessions []cluster.RemoteExecSessionDescriptor, containerID string) []boot.ExecSessionInfo {
	out := make([]boot.ExecSessionInfo, len(sessions))
	for i, session := range sessions {
		var status *uint32
		if session.ExitStatus != nil {
			value := *session.ExitStatus
			status = &value
		}
		out[i] = boot.ExecSessionInfo{
			ID: session.ID, ContainerID: containerID, PID: kernel.ThreadID(session.PID),
			ConsoleID: session.ConsoleID, Terminal: session.Terminal,
			State: boot.ExecSessionState(session.State), CreatedAt: session.CreatedAt,
			ExitStatus: status,
		}
	}
	return out
}

func (b *runtimeBackend) portableDistributedInternalMounts(container string) ([]cluster.InternalMountDescriptor, error) {
	mounts, err := b.containerStore().PortableInternalMounts(container)
	if err != nil {
		return nil, err
	}
	result := make([]cluster.InternalMountDescriptor, len(mounts))
	for i, mount := range mounts {
		result[i] = cluster.InternalMountDescriptor{
			Destination: mount.Destination, Contents: append([]byte(nil), mount.Contents...), Mode: uint32(mount.Mode.Perm()),
		}
	}
	return result, nil
}

func (b *runtimeBackend) portableDistributedFamily(info ttcbundle.ContainerInfo) (cluster.RemoteFamilyDescriptor, error) {
	spec, err := b.containerStore().LoadSpec(info.ID)
	if err != nil {
		return cluster.RemoteFamilyDescriptor{}, fmt.Errorf("loading distributed family specification: %w", err)
	}
	containerJSON, err := json.Marshal(info)
	if err != nil {
		return cluster.RemoteFamilyDescriptor{}, fmt.Errorf("encoding distributed family metadata: %w", err)
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return cluster.RemoteFamilyDescriptor{}, fmt.Errorf("encoding distributed family specification: %w", err)
	}
	return cluster.RemoteFamilyDescriptor{Container: containerJSON, Spec: specJSON}, nil
}

func runBoundedBatch(count, limit int, fn func(int)) {
	if count <= 0 {
		return
	}
	if limit <= 0 || limit > count {
		limit = count
	}
	work := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < limit; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range work {
				fn(index)
			}
		}()
	}
	for index := 0; index < count; index++ {
		work <- index
	}
	close(work)
	workers.Wait()
}

// newConfigMemFD returns a seekable in-memory file positioned at the start of
// payload. A pipe cannot be filled before commandWithFiles starts its reader:
// sufficiently wide sibling batches exceed the host pipe capacity and would
// otherwise deadlock the daemon while it still owns the family mutation lock.
func newConfigMemFD(name string, payload []byte) (*os.File, error) {
	fd, err := memutil.CreateMemFD(name, unix.MFD_CLOEXEC)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// publishRemoteTarget retries the idempotent publication boundary while a
// restored target remains physically paused. A Raft leadership change renews
// node execution leases in the new term asynchronously; without this retry a
// perfectly healthy target can be exposed to the caller as a dynamic fork
// denial during that short interval. Status distinguishes a lost successful
// response from a target that is still safe to retry.
func publishRemoteTarget(ctx context.Context, peer *cluster.Client, transactionID string, revision uint64, paused bool) (*cluster.Transaction, error) {
	var lastErr error
	for {
		tx, err := peer.Publish(ctx, transactionID, revision, paused)
		if err == nil {
			return tx, nil
		}
		lastErr = err
		status, statusErr := peer.Status(ctx, transactionID)
		if statusErr == nil {
			switch status.State {
			case cluster.TransactionPublished, cluster.TransactionDraining, cluster.TransactionIndependent:
				return status, nil
			case cluster.TransactionTargetPaused:
				revision = status.Revision
			case cluster.TransactionCanceled, cluster.TransactionFailed:
				return nil, fmt.Errorf("remote target became %s while publishing: %s", status.State, status.Failure)
			default:
				return nil, err
			}
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("publishing remote target: %w (last attempt: %v)", ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
}

// activateRemoteMigration rolls a committed placement forward even if target
// dependency processing advances the transaction between cutover and the
// activation RPC. Published, draining, and independent all preserve the same
// target execution authority; only their revision changes. Fetching the
// current revision and retrying is therefore required for progress, while a
// canceled or failed transaction remains terminal.
func activateRemoteMigration(ctx context.Context, peer *cluster.Client, transactionID string, revision uint64) (*cluster.Transaction, error) {
	var lastErr error
	for {
		tx, err := peer.Activate(ctx, transactionID, revision)
		if err == nil {
			return tx, nil
		}
		lastErr = err
		status, statusErr := peer.Status(ctx, transactionID)
		if statusErr == nil {
			if status.Activated {
				return status, nil
			}
			activatable := status.State == cluster.TransactionPublished || status.State == cluster.TransactionDraining || status.State == cluster.TransactionIndependent
			if !status.Identity.Migration || !status.OwnershipCommitted || !activatable {
				if status.State == cluster.TransactionCanceled || status.State == cluster.TransactionFailed {
					return nil, fmt.Errorf("migration target became %s while activating: %s", status.State, status.Failure)
				}
				return nil, err
			}
			if status.Revision == revision {
				// The target rejected activation for a runtime reason, not an
				// optimistic-revision race. Repeating cannot make progress.
				return nil, err
			}
			revision = status.Revision
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, fmt.Errorf("activating committed migration target: %w (last attempt: %v)", ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
}

type distributedSourceRuntime struct {
	capabilities  cluster.Capabilities
	incarnation   string
	peers         map[string]*cluster.Client
	directory     cluster.NodeCatalog
	placements    cluster.PlacementCatalog
	transactions  cluster.TransactionCatalog
	authority     *cluster.RaftCatalog
	authToken     string
	peerMu        sync.Mutex
	dynamicPeers  map[string]dynamicClusterPeer
	handoffOnce   sync.Once
	handoffSlots  chan struct{}
	handoffGate   sync.RWMutex
	collectNeeded atomic.Bool
	collectActive atomic.Bool
	monitorOnce   sync.Once
	monitorSlots  chan struct{}
	monitorCount  atomic.Int64
	sourceLocks   [64]sync.Mutex
	pageListen    string
	pageAdvertise string
}

func (r *distributedSourceRuntime) lockSourceFamily(family string) func() {
	if r == nil {
		return func() {}
	}
	// Fixed stripes keep cleanup bookkeeping bounded even when a long-lived
	// cluster creates and removes arbitrarily many family names.
	hash := uint64(14695981039346656037)
	for i := range len(family) {
		hash ^= uint64(family[i])
		hash *= 1099511628211
	}
	lock := &r.sourceLocks[hash%uint64(len(r.sourceLocks))]
	lock.Lock()
	return lock.Unlock
}

const distributedSourceMonitorConcurrency = 2

func (r *distributedSourceRuntime) beginSourceMonitor() func() {
	if r == nil {
		return func() {}
	}
	r.monitorCount.Add(1)
	return func() { r.monitorCount.Add(-1) }
}

func (r *distributedSourceRuntime) sourceMonitorInterval() time.Duration {
	if r == nil {
		return 250 * time.Millisecond
	}
	watchers := r.monitorCount.Load()
	if watchers < 1 {
		watchers = 1
	}
	interval := time.Duration(watchers) * time.Second / 16
	if interval < 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	if interval > 30*time.Second {
		interval = 30 * time.Second
	}
	return interval
}

func (r *distributedSourceRuntime) trySourcePoll() (func(), bool) {
	if r == nil {
		return func() {}, true
	}
	r.monitorOnce.Do(func() { r.monitorSlots = make(chan struct{}, distributedSourceMonitorConcurrency) })
	select {
	case r.monitorSlots <- struct{}{}:
		return func() { <-r.monitorSlots }, true
	default:
		return nil, false
	}
}

func (r *distributedSourceRuntime) scheduleHandoff(fn func()) {
	if fn == nil {
		return
	}
	if r == nil {
		go fn()
		return
	}
	r.handoffOnce.Do(func() {
		r.handoffSlots = make(chan struct{}, distributedHandoffConcurrency)
	})
	go func() {
		r.handoffSlots <- struct{}{}
		defer func() { <-r.handoffSlots }()
		fn()
	}()
}

func (r *distributedSourceRuntime) beginBackgroundSnapshotWork() func() {
	if r == nil {
		return func() {}
	}
	r.handoffGate.RLock()
	return r.handoffGate.RUnlock
}

// beginForegroundCapture prevents queued background graph handoffs from
// starving a user-requested live capture. sync.RWMutex gives a waiting writer
// priority over new readers, so the capture waits for at most the currently
// active bounded handoffs rather than the entire queue.
func (r *distributedSourceRuntime) beginForegroundCapture() func() {
	if r == nil {
		return func() {}
	}
	r.handoffGate.Lock()
	return r.handoffGate.Unlock
}

// scheduleColdCollection coalesces terminal handoff cleanup onto the same
// bounded background queue. Collection traces the cluster's immutable roots;
// running it once per sibling makes an N-way fan-out perform N near-identical
// global scans and can delay foreground forks for minutes. The small loop
// closes the race where another terminal handoff arrives during collection.
func (r *distributedSourceRuntime) scheduleColdCollection(fn func()) {
	if fn == nil {
		return
	}
	if r == nil {
		go fn()
		return
	}
	r.collectNeeded.Store(true)
	if !r.collectActive.CompareAndSwap(false, true) {
		return
	}
	r.scheduleHandoff(func() {
		for {
			r.collectNeeded.Store(false)
			fn()
			r.collectActive.Store(false)
			if !r.collectNeeded.Load() || !r.collectActive.CompareAndSwap(false, true) {
				return
			}
		}
	})
}

type dynamicClusterPeer struct {
	incarnation string
	controlURL  string
	serverName  string
	client      *cluster.Client
}

// renewDistributedExecutionAuthority synchronously acquires an execution
// lease in the current Raft term. A leader change intentionally invalidates
// grants from the prior term; user-triggered fork and migration operations
// must not race the periodic heartbeat and fail dynamically during that
// otherwise healthy takeover window.
func (b *runtimeBackend) renewDistributedExecutionAuthority() (*cluster.NodeRecord, error) {
	if b == nil || b.distributedSource == nil || b.distributedSource.directory == nil || b.nodeID == "" {
		return nil, errors.New("distributed execution authority is unavailable")
	}
	record, err := b.distributedSource.directory.Lookup(b.nodeID)
	if err != nil {
		return nil, err
	}
	lease := record.LeaseDuration
	if lease <= 0 {
		lease = record.LeaseExpiresAt.Sub(record.UpdatedAt)
	}
	if lease <= 0 {
		return nil, errors.New("distributed node has no configured execution lease")
	}
	current := record.MemoryCurrentBytes
	if measured, measureErr := currentNodeMemoryBytes(); measureErr == nil {
		current = measured
	}
	renewed, err := b.distributedSource.directory.Heartbeat(record.NodeID, record.Incarnation, current, time.Now(), lease)
	if err != nil {
		return nil, err
	}
	if !renewed.LeaseActive(time.Now()) {
		return nil, fmt.Errorf("node %q did not acquire active execution authority", renewed.NodeID)
	}
	return renewed, nil
}

func (b *runtimeBackend) placementCatalog() (cluster.PlacementCatalog, error) {
	if b != nil && b.distributedSource != nil && b.distributedSource.placements != nil {
		return b.distributedSource.placements, nil
	}
	return nil, errors.New("raft placement authority is unavailable")
}

func (b *runtimeBackend) transactionCatalog() (cluster.TransactionCatalog, error) {
	if b != nil && b.distributedSource != nil && b.distributedSource.transactions != nil {
		return b.distributedSource.transactions, nil
	}
	return nil, errors.New("raft transaction authority is unavailable")
}

func (r *distributedSourceRuntime) peer(nodeID string, forPlacement bool) (*cluster.Client, error) {
	if r == nil {
		return nil, errors.New("distributed source runtime is unavailable")
	}
	if nodeID == r.capabilities.NodeID {
		return nil, fmt.Errorf("cluster target node %q is the local node", nodeID)
	}
	if peer := r.peers[nodeID]; peer != nil {
		return peer, nil
	}
	if r.directory == nil {
		return nil, fmt.Errorf("cluster target node %q is not configured", nodeID)
	}
	record, err := r.directory.Lookup(nodeID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("cluster target node %q is not registered", nodeID)
		}
		return nil, fmt.Errorf("discovering cluster target node %q: %w", nodeID, err)
	}
	now := time.Now()
	if !record.LeaseActive(now) {
		return nil, fmt.Errorf("cluster target node %q is %s or its lease has expired", nodeID, record.State)
	}
	if forPlacement && !record.AcceptsPlacement(now) {
		return nil, fmt.Errorf("cluster target node %q is not accepting placement (state=%s)", nodeID, record.State)
	}
	if record.Capabilities.RepositoryID != r.capabilities.RepositoryID {
		return nil, fmt.Errorf("cluster target node %q uses repository %q, want %q", nodeID, record.Capabilities.RepositoryID, r.capabilities.RepositoryID)
	}
	if record.Capabilities.BuildID != r.capabilities.BuildID {
		return nil, fmt.Errorf("cluster target node %q uses TTC build %q, want exact build %q", nodeID, record.Capabilities.BuildID, r.capabilities.BuildID)
	}
	return r.peerForRecord(*record)
}

// peerForRecord constructs a control-plane client from an already-authorized
// Raft membership record. In particular, Raft leader redirection must use a
// locally applied record: resolving it through directory.Lookup would recurse
// into the same linearizable read that is being redirected.
func (r *distributedSourceRuntime) peerForRecord(record cluster.NodeRecord) (*cluster.Client, error) {
	nodeID := record.NodeID
	r.peerMu.Lock()
	defer r.peerMu.Unlock()
	if cached, ok := r.dynamicPeers[nodeID]; ok && cached.incarnation == record.Incarnation &&
		cached.controlURL == record.ControlURL && cached.serverName == record.ControlServerName {
		return cached.client, nil
	}
	client, err := cluster.NewPlainClient(record.ControlURL, r.capabilities.NodeID, r.authToken, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("configuring discovered cluster peer %q: %w", nodeID, err)
	}
	r.dynamicPeers[nodeID] = dynamicClusterPeer{
		incarnation: record.Incarnation, controlURL: record.ControlURL,
		serverName: record.ControlServerName, client: client,
	}
	return client, nil
}

type distributedForkBackend interface {
	forkRemote(source selector, targetNode, targetContainer string, paused bool) (selector, error)
	forkRemoteMany(source selector, targetNode string, targetContainers []string, paused bool) ([]selector, error)
}

// distributedForkPlacementBackend selects where a fork should execute. An
// empty requested node gives the scheduler authority to choose. Keeping a
// fork local while the source node accepts work preserves resident CoW
// sharing; a pressured or draining source is placed on the least-loaded
// compatible node instead.
type distributedForkPlacementBackend interface {
	selectForkNode(requested string) (node string, remote bool, err error)
}

func (b *runtimeBackend) selectForkNode(requested string) (string, bool, error) {
	if b.distributedSource == nil || b.distributedSource.directory == nil || b.nodeID == "" {
		if requested == "" {
			return "", false, nil
		}
		return "", false, errors.New("distributed node placement is not configured")
	}
	now := time.Now()
	if requested != "" {
		record, err := b.distributedSource.directory.Lookup(requested)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return "", false, fmt.Errorf("cluster node %q is not registered", requested)
			}
			return "", false, fmt.Errorf("looking up cluster node %q: %w", requested, err)
		}
		if !record.AcceptsPlacement(now) {
			return "", false, fmt.Errorf("cluster node %q is not accepting placement (state=%s)", requested, record.State)
		}
		if record.NodeID != b.nodeID {
			if _, err := cluster.Negotiate(b.distributedSource.capabilities, record.Capabilities); err != nil {
				return "", false, fmt.Errorf("cluster node %q is incompatible: %w", requested, err)
			}
		}
		return record.NodeID, record.NodeID != b.nodeID, nil
	}

	local, err := b.distributedSource.directory.Lookup(b.nodeID)
	if err == nil && local.AcceptsPlacement(now) {
		return local.NodeID, false, nil
	}
	records, listErr := b.distributedSource.directory.List()
	if listErr != nil {
		return "", false, fmt.Errorf("listing cluster nodes for fork placement: %w", listErr)
	}
	selected, selectErr := selectAdmissionNode(records, b.distributedSource.capabilities, now)
	if selectErr != nil {
		if err != nil {
			return "", false, fmt.Errorf("local node lookup failed (%v) and automatic fork placement failed: %w", err, selectErr)
		}
		return "", false, fmt.Errorf("automatic fork placement: %w", selectErr)
	}
	return selected.NodeID, selected.NodeID != b.nodeID, nil
}

type distributedPageRelay struct {
	listener             net.Listener
	backend              *runtimeBackend
	source               string
	session              string
	close                sync.Once
	mu                   sync.Mutex
	closed               bool
	commands             map[*exec.Cmd]struct{}
	connections          map[*net.TCPConn]struct{}
	filesystemConfigured bool
	directFilesystem     []ttcvolume.RemoteObjectSource
	directToken          ttcvolume.RemoteToken
	directCleanup        func()
	directWG             sync.WaitGroup
	consoleToken         [16]byte
	consoleLogs          map[string][]string
}

func newDistributedPageRelay(listener net.Listener, backend *runtimeBackend, source, session string) *distributedPageRelay {
	relay := &distributedPageRelay{
		listener: listener, backend: backend, source: source, session: session,
		commands: make(map[*exec.Cmd]struct{}), connections: make(map[*net.TCPConn]struct{}),
	}
	go relay.serve()
	return relay
}

func (r *distributedPageRelay) serve() {
	for {
		conn, err := r.listener.Accept()
		if err != nil {
			return
		}
		tcp, ok := conn.(*net.TCPConn)
		if !ok {
			conn.Close()
			continue
		}
		r.mu.Lock()
		if r.closed {
			r.mu.Unlock()
			tcp.Close()
			continue
		}
		r.connections[tcp] = struct{}{}
		r.mu.Unlock()
		// Filesystem sources are donated before memory streams, but remain idle
		// until a guest actually reads an object. Classifying here synchronously
		// would stop Accept at that first idle stream and deadlock the metadata
		// connection behind it.
		go r.serveConnection(tcp)
	}
}

func (r *distributedPageRelay) serveConnection(tcp *net.TCPConn) {
	defer func() {
		r.mu.Lock()
		delete(r.connections, tcp)
		r.mu.Unlock()
	}()
	magic, err := peekDistributedMagic(tcp)
	if err != nil {
		tcp.Close()
		return
	}
	if magic == [4]byte{'T', 'T', 'C', 'F'} {
		r.mu.Lock()
		configured := r.filesystemConfigured
		closed := r.closed
		direct := len(r.directFilesystem) != 0
		if closed || !configured {
			r.mu.Unlock()
			tcp.Close()
			return
		}
		if direct {
			sources := append([]ttcvolume.RemoteObjectSource(nil), r.directFilesystem...)
			token := r.directToken
			r.directWG.Add(1)
			r.mu.Unlock()
			r.serveDirectFilesystem(tcp, token, sources)
			return
		}
		r.mu.Unlock()
		file, err := tcp.File()
		tcp.Close()
		if err != nil {
			return
		}
		r.serveFilesystemFile(file)
		return
	}
	if magic == [4]byte{'T', 'T', 'C', 'L'} {
		r.serveConsoleLog(tcp)
		return
	}
	file, err := tcp.File()
	tcp.Close()
	if err != nil {
		return
	}
	r.serveFile(file)
}

// ConfigureConsoleLogs exposes stable, stopped-follower log generations over
// the same authenticated point-to-point listener as memory and filesystem
// paging. The paths never cross the wire and are valid only for this relay's
// lifetime.
func (r *distributedPageRelay) ConfigureConsoleLogs(token [16]byte, logs map[string][]string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || len(r.consoleLogs) != 0 {
		return errors.New("distributed console relay is closed or already configured")
	}
	r.consoleToken = token
	r.consoleLogs = make(map[string][]string, len(logs))
	for id, paths := range logs {
		r.consoleLogs[id] = append([]string(nil), paths...)
	}
	return nil
}

func (r *distributedPageRelay) serveConsoleLog(conn *net.TCPConn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	magic := make([]byte, 4)
	if _, err := io.ReadFull(reader, magic); err != nil || string(magic) != "TTCL" {
		return
	}
	var token [16]byte
	if _, err := io.ReadFull(reader, token[:]); err != nil {
		return
	}
	var idLength uint16
	if err := binary.Read(reader, binary.BigEndian, &idLength); err != nil || idLength == 0 || idLength > 4096 {
		return
	}
	idBytes := make([]byte, idLength)
	if _, err := io.ReadFull(reader, idBytes); err != nil {
		return
	}
	r.mu.Lock()
	paths := append([]string(nil), r.consoleLogs[string(idBytes)]...)
	authorized := token == r.consoleToken && len(paths) != 0 && !r.closed
	r.mu.Unlock()
	if !authorized {
		return
	}
	var size uint64
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || info.Size() < 0 {
			return
		}
		if uint64(info.Size()) > math.MaxUint64-size {
			return
		}
		size += uint64(info.Size())
	}
	if err := binary.Write(conn, binary.BigEndian, size); err != nil {
		return
	}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return
		}
		_, copyErr := io.Copy(conn, file)
		closeErr := file.Close()
		if copyErr != nil || closeErr != nil {
			return
		}
	}
}

func fetchRemoteConsoleLog(ctx context.Context, address string, token [16]byte, execID string, expected uint64, destination string) (retErr error) {
	if execID == "" || len(execID) > 4096 || expected == 0 || expected > math.MaxInt64 {
		return errors.New("invalid remote console log descriptor")
	}
	conn, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var request bytes.Buffer
	request.WriteString("TTCL")
	request.Write(token[:])
	if err := binary.Write(&request, binary.BigEndian, uint16(len(execID))); err != nil {
		return err
	}
	request.WriteString(execID)
	if _, err := conn.Write(request.Bytes()); err != nil {
		return err
	}
	var size uint64
	if err := binary.Read(conn, binary.BigEndian, &size); err != nil {
		return err
	}
	if size != expected {
		return fmt.Errorf("remote console log size is %d, want %d", size, expected)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
		return err
	}
	temporary := destination + ".remote-partial"
	if info, err := os.Stat(destination); err == nil && uint64(info.Size()) == expected {
		return nil
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	_ = os.Remove(temporary)
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		_ = file.Close()
		if retErr != nil {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := io.CopyN(file, conn, int64(size)); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		return err
	}
	return nil
}

func (b *runtimeBackend) captureRemoteExecConsoleLogs(container, branch string, sessions []boot.ExecSessionInfo) ([]cluster.RemoteConsoleLogDescriptor, map[string][]string, error) {
	_, maxFiles, err := b.consoleRetention(container)
	if err != nil {
		return nil, nil, err
	}
	store := b.containerStore()
	descriptors := make([]cluster.RemoteConsoleLogDescriptor, 0, len(sessions))
	files := make(map[string][]string)
	for _, session := range sessions {
		if session.State != boot.ExecSessionRunning {
			continue
		}
		paths, err := store.ExecConsolePaths(container, branch, session.ID)
		if err != nil {
			return nil, nil, err
		}
		generations, err := listConsoleLogGenerations(paths.Log, maxFiles)
		if err != nil {
			return nil, nil, err
		}
		var size uint64
		generationPaths := make([]string, 0, len(generations))
		for _, generation := range generations {
			if generation.info.Size() < 0 {
				return nil, nil, fmt.Errorf("exec console log %q has a negative size", session.ID)
			}
			if uint64(generation.info.Size()) > math.MaxUint64-size {
				return nil, nil, fmt.Errorf("exec console log %q is too large", session.ID)
			}
			size += uint64(generation.info.Size())
			generationPaths = append(generationPaths, generation.path)
		}
		if size == 0 {
			continue
		}
		descriptors = append(descriptors, cluster.RemoteConsoleLogDescriptor{ExecID: session.ID, Size: size})
		files[session.ID] = generationPaths
	}
	return descriptors, files, nil
}

func (b *runtimeBackend) installRemoteExecConsoleLogs(ctx context.Context, address string, token [16]byte, container, branch string, logs []cluster.RemoteConsoleLogDescriptor) error {
	store := b.containerStore()
	for _, log := range logs {
		paths, err := store.ExecConsolePaths(container, branch, log.ExecID)
		if err != nil {
			return err
		}
		if err := fetchRemoteConsoleLog(ctx, address, token, log.ExecID, log.Size, paths.Log); err != nil {
			return fmt.Errorf("fetching retained output for exec session %q: %w", log.ExecID, err)
		}
	}
	return nil
}

func peekDistributedMagic(tcp *net.TCPConn) ([4]byte, error) {
	var magic [4]byte
	raw, err := tcp.SyscallConn()
	if err != nil {
		return magic, err
	}
	var socketErr error
	err = raw.Read(func(fd uintptr) bool {
		n, _, recvErr := unix.Recvfrom(int(fd), magic[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
		if errors.Is(recvErr, unix.EAGAIN) || errors.Is(recvErr, unix.EWOULDBLOCK) {
			return false
		}
		if recvErr != nil {
			socketErr = recvErr
			return true
		}
		if n != len(magic) {
			socketErr = io.ErrUnexpectedEOF
		}
		return true
	})
	return magic, errors.Join(err, socketErr)
}

func (r *distributedPageRelay) ConfigureFilesystem() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.filesystemConfigured {
		return errors.New("distributed filesystem relay is closed or already configured")
	}
	r.filesystemConfigured = true
	return nil
}

// ConfigureDirectFilesystem switches an already configured filesystem relay
// from Sentry donation to daemon-side immutable repository serving. This is
// used by process-free dehydrated branches: there is no source Sentry, while
// the manager-side graph repository is backed lazily by durable shared packs.
// It must be installed before the target receives the relay address.
func (r *distributedPageRelay) ConfigureDirectFilesystem(token ttcvolume.RemoteToken, sources []ttcvolume.RemoteObjectSource, cleanup func()) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.filesystemConfigured || len(r.directFilesystem) != 0 || len(sources) == 0 {
		return errors.New("distributed direct filesystem relay is unavailable or already configured")
	}
	r.directToken = token
	r.directFilesystem = sources
	r.directCleanup = cleanup
	return nil
}

func (r *distributedPageRelay) serveDirectFilesystem(conn *net.TCPConn, token ttcvolume.RemoteToken, sources []ttcvolume.RemoteObjectSource) {
	defer r.directWG.Done()
	if err := ttcvolume.ServeRemoteObjects(conn, token, sources); err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
		fmt.Fprintf(os.Stderr, "ttcd: distributed direct filesystem relay failed: %v\n", err)
	}
}

func (r *distributedPageRelay) serveFile(file *os.File) {
	r.serveRunscFile(file, "distributed-serve")
}

func (r *distributedPageRelay) serveFilesystemFile(file *os.File) {
	r.serveRunscFile(file, "distributed-filesystem-serve")
}

func (r *distributedPageRelay) serveRunscFile(file *os.File, action string) {
	defer file.Close()
	// A donated filesystem stream is deliberately lazy: the target may not
	// fault its first deferred object until minutes or hours after migration.
	// Do not put an inactivity timeout on this socket. Relay.Close tracks and
	// kills the serving command, so explicit transaction completion remains the
	// lifetime fence without turning an idle, authoritative source into a
	// broken pipe.
	command := runscCommand(r.backend.runsc,
		"--root", r.backend.root, "timetravel", "--distributed-connection-fd=3",
		r.source, action, r.session)
	command.ExtraFiles = []*os.File{file}
	command.Stdin = strings.NewReader("")
	command.Stdout = io.Discard
	var commandError bytes.Buffer
	command.Stderr = &commandError
	command.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	if err := command.Start(); err != nil {
		r.mu.Unlock()
		fmt.Fprintf(os.Stderr, "ttcd: distributed %s relay could not start: %v\n", action, err)
		return
	}
	r.commands[command] = struct{}{}
	r.mu.Unlock()
	waitErr := command.Wait()
	if waitErr != nil {
		fmt.Fprintf(os.Stderr, "ttcd: distributed %s relay failed: %v: %s\n", action, waitErr, strings.TrimSpace(commandError.String()))
	}
	r.mu.Lock()
	delete(r.commands, command)
	r.mu.Unlock()
}

func (r *distributedPageRelay) Close() {
	if r != nil {
		r.close.Do(func() {
			_ = r.listener.Close()
			r.mu.Lock()
			r.closed = true
			commands := make([]*exec.Cmd, 0, len(r.commands))
			for command := range r.commands {
				commands = append(commands, command)
			}
			connections := make([]*net.TCPConn, 0, len(r.connections))
			for connection := range r.connections {
				connections = append(connections, connection)
			}
			r.mu.Unlock()
			for _, connection := range connections {
				_ = connection.Close()
			}
			for _, command := range commands {
				if command.Process != nil {
					_ = command.Process.Kill()
				}
			}
			r.directWG.Wait()
			r.mu.Lock()
			cleanup := r.directCleanup
			r.directCleanup = nil
			r.directFilesystem = nil
			r.mu.Unlock()
			if cleanup != nil {
				cleanup()
			}
		})
	}
}

func resolveDistributedAdvertiseAddress(configured, bound string) (string, error) {
	host, port, err := net.SplitHostPort(configured)
	if err != nil || host == "" {
		return "", fmt.Errorf("invalid cluster page advertise address %q", configured)
	}
	if port == "0" {
		_, port, err = net.SplitHostPort(bound)
		if err != nil {
			return "", err
		}
	}
	return net.JoinHostPort(host, port), nil
}

func newDistributedTransactionID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "fork-" + hex.EncodeToString(random[:]), nil
}

// prepareDistributedFilesystem describes the already-forked immutable volume
// roots and configures the source relay. The descriptor is bounded by the
// number of mounted repositories and no shared-storage write is required on
// the target time-to-execution path.
func (b *runtimeBackend) prepareDistributedFilesystem(info ttcbundle.ContainerInfo, filesystemBranch string, relay *distributedPageRelay) ([]cluster.FilesystemRootDescriptor, uint64, error) {
	if filesystemBranch == "" {
		return nil, 0, errors.New("distributed capture omitted its filesystem generation")
	}
	spec, err := b.containerStore().LoadSpec(info.ID)
	if err != nil {
		return nil, 0, err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return nil, 0, err
	}
	if len(configs) == 0 {
		return nil, 0, nil
	}
	roots := make([]cluster.FilesystemRootDescriptor, 0, len(configs))
	var dependentBytes uint64
	seenRepositories := make(map[string]struct{})
	for _, config := range configs {
		path := filepath.Clean(config.Path)
		if _, seen := seenRepositories[path]; seen {
			continue
		}
		seenRepositories[path] = struct{}{}
		repository, err := ttcvolume.OpenRepository(path, ttcvolume.CopyMode(config.CopyMode))
		if err != nil {
			return nil, 0, err
		}
		descriptor, describeErr := repository.DescribeRemoteSnapshot(filesystemBranch)
		closeErr := repository.Close()
		if describeErr != nil || closeErr != nil {
			return nil, 0, errors.Join(describeErr, closeErr)
		}
		snapshot := descriptor.Snapshot
		restoreID := config.RestoreID
		if restoreID == "" {
			restoreID = snapshot.RepositoryID
		}
		if snapshot.LogicalBytes > ^uint64(0)-dependentBytes {
			return nil, 0, errors.New("distributed filesystem dependency byte count overflow")
		}
		dependentBytes += snapshot.LogicalBytes
		roots = append(roots, cluster.FilesystemRootDescriptor{
			Source: config.Source, RepositoryID: snapshot.RepositoryID,
			RestoreID: restoreID,
			BranchID:  snapshot.BranchID, Root: descriptor.Root.String(),
			NextInode: snapshot.NextInode, LogicalBytes: snapshot.LogicalBytes,
		})
	}
	if err := relay.ConfigureFilesystem(); err != nil {
		return nil, 0, err
	}
	return roots, dependentBytes, nil
}

// materializeDistributedFilesystemRoots installs the bounded graph-root
// descriptors sent directly by the source. It performs no shared-storage I/O;
// metadata below each root and whole-file bytes remain demand-fetched through
// the transaction-fenced Sentry relay.
func (b *runtimeBackend) materializeDistributedFilesystemRoots(info ttcbundle.ContainerInfo, filesystemBranch string, roots []cluster.FilesystemRootDescriptor, peer bool) (_ int, retErr error) {
	if len(roots) == 0 {
		return 0, nil
	}
	allocateOrigins, finishOrigins := b.newFilesystemOriginScope()
	originsCommitted := false
	type importedFilesystem struct {
		path     string
		copyMode ttcvolume.CopyMode
		created  bool
	}
	var imported []importedFilesystem
	defer func() {
		if !originsCommitted {
			for i := len(imported) - 1; i >= 0; i-- {
				entry := imported[i]
				if entry.created {
					retErr = errors.Join(retErr, os.RemoveAll(entry.path))
					continue
				}
				repository, err := ttcvolume.OpenRepository(entry.path, entry.copyMode)
				if err == nil {
					err = repository.RemoveBranch(filesystemBranch)
					err = errors.Join(err, repository.Close())
				}
				retErr = errors.Join(retErr, err)
			}
			retErr = errors.Join(retErr, finishOrigins(false))
		}
	}()
	bySource := make(map[string]cluster.FilesystemRootDescriptor, len(roots))
	for _, root := range roots {
		if _, exists := bySource[root.Source]; exists {
			return 0, fmt.Errorf("distributed filesystem repeats source %q", root.Source)
		}
		bySource[root.Source] = root
	}
	store := b.containerStore()
	spec, err := store.LoadSpec(info.ID)
	if err != nil {
		return 0, err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return 0, err
	}
	manager, err := ttcvolume.OpenManager(b.activeDataRoot())
	if err != nil {
		return 0, err
	}
	seenPaths := make(map[string]struct{})
	installed := 0
	for index := range configs {
		config := &configs[index]
		path := filepath.Clean(config.Path)
		if _, seen := seenPaths[path]; seen {
			continue
		}
		seenPaths[path] = struct{}{}
		root, ok := bySource[config.Source]
		if !ok {
			return 0, fmt.Errorf("distributed filesystem omits repository %q", config.Source)
		}
		var origins []ttcsnapshot.SnapshotID
		if allocateOrigins != nil {
			origins, err = allocateOrigins(2)
			if err != nil {
				return 0, fmt.Errorf("allocating target filesystem origins for %q: %w", config.Source, err)
			}
		}
		descriptor, err := ttcvolume.NewRemoteSnapshotDescriptor(root.RepositoryID, root.BranchID, root.Root, root.NextInode, root.LogicalBytes)
		if err != nil {
			return 0, fmt.Errorf("decoding repository %q graph root: %w", config.Source, err)
		}
		if peer {
			var repository *ttcvolume.Repository
			created := false
			if _, statErr := os.Stat(filepath.Join(config.Path, "volume.json")); statErr == nil {
				repository, err = ttcvolume.OpenRepository(config.Path, ttcvolume.CopyMode(config.CopyMode))
				if err == nil {
					if len(origins) == 0 {
						err = repository.ImportRemoteBranchDescriptor(descriptor, filesystemBranch)
					} else {
						err = repository.ImportRemoteBranchDescriptorWithOrigins(descriptor, filesystemBranch, origins[0], origins[1])
					}
				}
			} else if errors.Is(statErr, os.ErrNotExist) {
				created = true
				if len(origins) == 0 {
					repository, err = manager.ImportPrivateRemoteDescriptor(config.Path, "peer-"+fmt.Sprint(index), descriptor, filesystemBranch)
				} else {
					repository, err = manager.ImportPrivateRemoteDescriptorWithOrigins(config.Path, "peer-"+fmt.Sprint(index), descriptor, filesystemBranch, origins[0], origins[1])
				}
			} else {
				err = statErr
			}
			if err != nil {
				return 0, fmt.Errorf("importing peer repository %q metadata: %w", config.Source, err)
			}
			if _, err := repository.Branch(filesystemBranch); err != nil {
				_ = repository.Close()
				return 0, fmt.Errorf("verifying imported peer repository %q branch %q: %w", config.Source, filesystemBranch, err)
			}
			config.Branch = filesystemBranch
			config.CopyMode = string(manager.Capability().Mode)
			config.RestoreID = root.RestoreID
			config.Private = true
			if err := repository.Close(); err != nil {
				return 0, err
			}
			imported = append(imported, importedFilesystem{path: config.Path, copyMode: ttcvolume.CopyMode(config.CopyMode), created: created})
			installed++
			continue
		}
		pathKey := sha256.Sum256([]byte(path))
		destination := filepath.Join(b.activeDataRoot(), "containers", info.ID, "repositories", "remote", hex.EncodeToString(pathKey[:]))
		var repository *ttcvolume.Repository
		created := false
		if _, statErr := os.Stat(destination); statErr == nil {
			repository, err = ttcvolume.OpenRepository(destination, manager.Capability().Mode)
			if err == nil {
				if len(origins) == 0 {
					err = repository.ImportRemoteBranchDescriptor(descriptor, filesystemBranch)
				} else {
					err = repository.ImportRemoteBranchDescriptorWithOrigins(descriptor, filesystemBranch, origins[0], origins[1])
				}
			}
		} else if errors.Is(statErr, os.ErrNotExist) {
			created = true
			if len(origins) == 0 {
				repository, err = manager.ImportPrivateRemoteDescriptor(destination, "remote-"+fmt.Sprint(index), descriptor, filesystemBranch)
			} else {
				repository, err = manager.ImportPrivateRemoteDescriptorWithOrigins(destination, "remote-"+fmt.Sprint(index), descriptor, filesystemBranch, origins[0], origins[1])
			}
		} else {
			err = statErr
		}
		if err != nil {
			return 0, fmt.Errorf("importing remote repository %q metadata: %w", config.Source, err)
		}
		config.Path = destination
		config.Branch = filesystemBranch
		config.CopyMode = string(manager.Capability().Mode)
		config.RestoreID = root.RestoreID
		config.Private = true
		if err := repository.Close(); err != nil {
			return 0, err
		}
		imported = append(imported, importedFilesystem{path: destination, copyMode: manager.Capability().Mode, created: created})
		installed++
	}
	if installed != len(roots) {
		return 0, fmt.Errorf("distributed filesystem installed %d roots, descriptor contains %d", installed, len(roots))
	}
	if err := store.WriteActiveRepositoryMap(info.ID, configs); err != nil {
		return 0, err
	}
	if err := finishOrigins(true); err != nil {
		return 0, err
	}
	originsCommitted = true
	return installed, nil
}

// exportDistributedGraph publishes the fork-time memory retained by session
// into a graph-native snapshot origin. The Sentry writes a bounded capture
// stream directly to the daemon; no compatibility repository is created.
func (b *runtimeBackend) exportDistributedGraph(containerID, transactionID string) (_ cluster.GraphHandoffDescriptor, rootNames, rootObjects []string, retErr error) {
	if b.snapshotGraph == nil || b.snapshotData == nil || b.snapshotShared == nil || b.distributedSource == nil || b.distributedSource.authority == nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, errors.New("distributed graph publication is unavailable")
	}
	origin, sealState, err := b.allocateGraphSnapshotOrigin()
	if err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	shared := false
	manifestRelative := filepath.Join("cluster", "memory-manifests", transactionID+".manifest")
	manifestPath := filepath.Join(b.dataRoot, manifestRelative)
	defer func() {
		if retErr == nil {
			return
		}
		_ = os.Remove(manifestPath)
		if shared {
			_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
		} else {
			_ = b.releaseGraphMemoryRoots(rootNames)
		}
		if err := b.retireFailedGraphSnapshot(origin); err != nil && b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: retiring failed live handoff snapshot %d: %v\n", origin, err)
		}
	}()

	reader, writer, err := os.Pipe()
	if err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	type importResult struct {
		capture ttcchunk.GraphImportResult
		err     error
	}
	imported := make(chan importResult, 1)
	prefix := fmt.Sprintf("remote/%d/%s", origin, transactionID)
	go func() {
		capture, importErr := ttcchunk.ImportGraphCapture(reader, b.snapshotGraph, b.snapshotData, origin, prefix)
		_ = reader.Close()
		imported <- importResult{capture: capture, err: importErr}
	}()
	// Only the Sentry export competes with a foreground live capture. Shared
	// publication and reference-ledger work may take arbitrarily long on a slow
	// shared filesystem and must never hold the foreground-priority gate.
	finishBackgroundWork := b.distributedSource.beginBackgroundSnapshotWork()
	_, exportErr := b.commandWithFiles([]*os.File{writer}, "timetravel", "--dehydration-graph-stream-fd=3", containerID, "distributed-export", transactionID)
	_ = writer.Close()
	finishBackgroundWork()
	outcome := <-imported
	if exportErr != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("streaming live graph memory: %w", exportErr)
	}
	if outcome.err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("importing live graph memory: %w", outcome.err)
	}
	for _, root := range outcome.capture.Roots {
		rootNames = append(rootNames, root.Name)
		rootObjects = append(rootObjects, root.Root.String())
	}
	if len(rootObjects) == 0 {
		return cluster.GraphHandoffDescriptor{}, nil, nil, errors.New("live graph capture returned no roots")
	}
	if err := b.snapshotGraph.Seal(origin, sealState); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("sealing live graph snapshot %d: %w", origin, err)
	}
	adapted, err := ttcchunk.BuildGraphManifest(b.snapshotGraph, outcome.capture.Roots)
	if err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	encoded, err := ttcchunk.EncodeManifest(adapted.Manifest)
	if err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	if err := writeDurableDistributedFile(manifestPath, encoded); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("publishing live graph manifest: %w", err)
	}
	if err := b.snapshotShared.PublishHandoff(context.Background(), b.snapshotGraph, origin, b.snapshotData); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("publishing live graph snapshot %d: %w", origin, err)
	}
	// Serialize only the short final ledger/authority switch against foreground
	// captures. The potentially slow shared-pack write above remains outside the
	// gate. Once a foreground writer owns the gate, it may add references to
	// this origin; the later freeze will include them in its baseline.
	finishFinalHandoff := b.distributedSource.beginBackgroundSnapshotWork()
	defer finishFinalHandoff()
	ledgerFrozen := false
	defer func() {
		if retErr != nil && ledgerFrozen {
			_ = b.snapshotGraph.UnfreezeSnapshotLedger(origin)
		}
	}()
	if _, _, err := b.snapshotGraph.FreezeSnapshotLedger(origin); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	ledgerFrozen = true
	changes, _, frozen, err := b.snapshotGraph.SnapshotLedgerHandoffChanges(origin, 0)
	if err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	if !frozen {
		return cluster.GraphHandoffDescriptor{}, nil, nil, errors.New("live graph reference handoff did not freeze")
	}
	publisher := &branchPublisher{
		nodeID: b.nodeID, incarnation: b.distributedSource.incarnation,
		authority: b.distributedSource.authority, graphShared: b.snapshotShared,
		sharedRefs: b.sharedReferences, peers: b.distributedSource,
	}
	if err := publisher.publishSharedReferenceLedger(origin, changes); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("publishing live graph reference ledger: %w", err)
	}
	if err := b.distributedSource.authority.PublishSnapshotOrigin(b.nodeID, b.distributedSource.incarnation, uint64(origin)); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("publishing live graph authority: %w", err)
	}
	shared = true
	if err := b.snapshotGraph.MarkShared(origin); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	if err := b.snapshotGraph.UnfreezeSnapshotLedger(origin); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	ledgerFrozen = false
	if err := releasePublishedProviderGraphRoots(b.snapshotGraph, b.snapshotData, origin); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	return cluster.GraphHandoffDescriptor{
		GraphManifest: manifestRelative, GraphSnapshot: uint64(origin),
		GraphRootObjects: strings.Join(rootObjects, ","),
	}, rootNames, rootObjects, nil
}

func distributedRootfsDigest(info ttcbundle.ContainerInfo, sourceBranch, repositoryID string) (string, error) {
	identity := struct {
		ContainerID  string                    `json:"container_id"`
		ImageDigest  string                    `json:"image_digest"`
		RootfsUpper  string                    `json:"rootfs_upper"`
		Volumes      []ttcbundle.ManagedVolume `json:"volumes"`
		SourceBranch string                    `json:"source_branch"`
		RepositoryID string                    `json:"repository_id"`
	}{info.ID, info.Digest, info.RootfsUpperID, info.Volumes, sourceBranch, repositoryID}
	encoded, err := json.Marshal(identity)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

const targetPlacementTemporarilyUnavailable = "transaction target is not accepting placement"

// reserveRemoteTarget hides the short authority gap after a Raft election.
// A ready target cannot accept a new transaction until it has renewed its
// execution lease in the new leader term, even though the request reaching
// that target proves its control plane is alive. Retry the same transaction
// identity until that renewal commits. Draining and offline are policy states,
// so they remain immediate systematic rejections.
func reserveRemoteTarget(ctx context.Context, targetNode string, lookup func(string) (*cluster.NodeRecord, error), reserve func(context.Context) (*cluster.Transaction, error)) (*cluster.Transaction, error) {
	if reserve == nil {
		return nil, errors.New("remote target reservation is unavailable")
	}
	delay := 50 * time.Millisecond
	for {
		tx, err := reserve(ctx)
		if err == nil || !strings.Contains(err.Error(), targetPlacementTemporarilyUnavailable) || lookup == nil {
			return tx, err
		}
		record, lookupErr := lookup(targetNode)
		if lookupErr == nil && record != nil && record.State != cluster.NodeJoining && record.State != cluster.NodeReady && record.State != cluster.NodePressured {
			return nil, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, errors.Join(err, ctx.Err())
		case <-timer.C:
		}
		if delay < 500*time.Millisecond {
			delay *= 2
		}
	}
}

func (b *runtimeBackend) forkRemote(source selector, targetNode, targetDestination string, paused bool) (selector, error) {
	if b.distributedSource == nil {
		return selector{}, errors.New("ttcd is not configured for distributed forks")
	}
	peer, err := b.distributedSource.peer(targetNode, true)
	if err != nil {
		return selector{}, err
	}
	target, err := parseSelector(targetDestination)
	if err != nil {
		return selector{}, fmt.Errorf("invalid distributed destination %q: %w", targetDestination, err)
	}
	resolved, sourceInfo, err := b.resolveSelector(source)
	if err != nil {
		return selector{}, err
	}
	source = resolved
	logicalSourceContainer := sourceInfo.DistributedFamilyID
	if logicalSourceContainer == "" {
		logicalSourceContainer = sourceInfo.ID
	}
	if !sourceInfo.LiveForkEligible {
		return selector{}, fmt.Errorf("container family %q is not live fork eligible: %s", sourceInfo.Name, sourceInfo.LiveForkWaiver)
	}
	sourceBranch, err := persistentBranchInfo(sourceInfo, source.branch)
	if err != nil {
		return selector{}, err
	}
	if _, err := b.renewDistributedExecutionAuthority(); err != nil {
		return selector{}, fmt.Errorf("renewing source execution authority: %w", err)
	}
	if state := normalizeLifecycleState(sourceBranch.State); state == "dehydrated" || state == "dehydrating" {
		return b.forkRemoteDehydrated(source, sourceInfo, peer, targetNode, target, paused, nil)
	}
	finishForegroundCapture := b.distributedSource.beginForegroundCapture()
	foregroundCaptureActive := true
	defer func() {
		if foregroundCaptureActive {
			finishForegroundCapture()
		}
	}()
	transactionID, err := newDistributedTransactionID()
	if err != nil {
		return selector{}, fmt.Errorf("generating distributed transaction ID: %w", err)
	}
	rootfsDigest, err := distributedRootfsDigest(sourceInfo, source.branch, b.distributedSource.capabilities.RepositoryID)
	if err != nil {
		return selector{}, err
	}
	identity := cluster.ForkIdentity{
		TransactionID: transactionID, SourceNodeID: b.distributedSource.capabilities.NodeID,
		TargetNodeID: targetNode, SourceContainer: logicalSourceContainer, SourceBranch: source.branch,
		TargetContainer: logicalSourceContainer, TargetBranch: transactionID,
		TargetVisibleBranch: target.branch,
		ImageDigest:         sourceInfo.Digest, RootfsDigest: rootfsDigest,
		RepositoryID: b.distributedSource.capabilities.RepositoryID,
	}
	family, err := b.portableDistributedFamily(sourceInfo)
	if err != nil {
		return selector{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tx, err := reserveRemoteTarget(ctx, targetNode, b.distributedSource.directory.Lookup, func(ctx context.Context) (*cluster.Transaction, error) {
		return peer.Reserve(ctx, cluster.ReserveRequest{
			SourceCapabilities: b.distributedSource.capabilities,
			Identity:           identity,
			Family:             family,
		})
	})
	if err != nil {
		return selector{}, fmt.Errorf("reserving target node %s: %w", targetNode, err)
	}
	var relay *distributedPageRelay
	captured := false
	committed := false
	defer func() {
		if committed {
			return
		}
		relay.Close()
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		status, statusErr := peer.Status(cleanupCtx, transactionID)
		if statusErr == nil {
			_, _ = peer.Cancel(cleanupCtx, transactionID, status.Revision)
		} else if tx != nil {
			_, _ = peer.Cancel(cleanupCtx, transactionID, tx.Revision)
		}
		if captured {
			_, _ = b.command("timetravel", source.container, "distributed-cancel", transactionID)
		}
	}()
	listener, err := net.Listen("tcp", b.distributedSource.pageListen)
	if err != nil {
		return selector{}, fmt.Errorf("opening distributed page listener: %w", err)
	}
	advertise, err := resolveDistributedAdvertiseAddress(b.distributedSource.pageAdvertise, listener.Addr().String())
	if err != nil {
		listener.Close()
		return selector{}, err
	}
	relayBackend := *b
	relayBackend.stdin = strings.NewReader("")
	relayBackend.stdout = io.Discard
	relayBackend.stderr = io.Discard
	relay = newDistributedPageRelay(listener, &relayBackend, source.container, transactionID)

	dataPlaneMax := tx.Negotiated.PagingPolicy.TransferBytes
	if tx.Negotiated.PagingPolicy.Prefetch == ttcpolicy.PrefetchAdaptive {
		// TransferBytes is the adaptive controller's initial request/window
		// size. Negotiate the full protocol limit with the transport so runtime
		// window growth can also coalesce I/O into larger network requests.
		dataPlaneMax = tx.Negotiated.MaxTransferSize
	}
	args := []string{
		"timetravel", "--source", source.branch,
		"--distributed-session", transactionID,
		"--distributed-family", logicalSourceContainer,
		"--distributed-external-listener",
		"--distributed-advertise", advertise,
		"--distributed-max-transfer", fmt.Sprint(dataPlaneMax),
		"--distributed-push-policy", string(tx.Negotiated.PagingPolicy.Push),
		"--distributed-push-high-water", fmt.Sprint(tx.Negotiated.PagingPolicy.MaxOutstandingBytes),
	}
	args = append(args, source.container, "distributed-capture", transactionID)
	filesystemOrigins, finishFilesystemOrigins, err := b.allocateFilesystemForkOrigins(sourceInfo.ID, 1)
	if err != nil {
		return selector{}, err
	}
	var output []byte
	if len(filesystemOrigins) == 0 {
		output, err = b.command(args...)
	} else {
		originsReader, pipeErr := filesystemOriginAssignmentsPipe(filesystemOrigins)
		if pipeErr != nil {
			return selector{}, errors.Join(pipeErr, finishFilesystemOrigins(false))
		}
		args = append([]string{"timetravel", "--filesystem-origins-fd=3"}, args[1:]...)
		output, err = b.commandWithFiles([]*os.File{originsReader}, args...)
		closeErr := originsReader.Close()
		if err == nil {
			err = closeErr
		}
	}
	finishForegroundCapture()
	foregroundCaptureActive = false
	if err != nil {
		return selector{}, errors.Join(fmt.Errorf("capturing source: %w", err), finishFilesystemOrigins(false))
	}
	if err := finishFilesystemOrigins(true); err != nil {
		return selector{}, err
	}
	captured = true
	var session boot.DistributedForkSessionInfo
	if err := json.Unmarshal(output, &session); err != nil {
		return selector{}, fmt.Errorf("decoding source capture: %w", err)
	}
	if session.SessionID != transactionID || session.MetadataBytes == 0 || session.StreamBytes == 0 {
		return selector{}, fmt.Errorf("source returned an incomplete distributed session")
	}
	filesystemRoots, filesystemBytes, err := b.prepareDistributedFilesystem(sourceInfo, session.FilesystemBranch, relay)
	if err != nil {
		return selector{}, fmt.Errorf("sealing distributed filesystem generation: %w", err)
	}
	internalMounts, err := b.portableDistributedInternalMounts(sourceInfo.ID)
	if err != nil {
		return selector{}, fmt.Errorf("capturing portable internal mounts: %w", err)
	}
	descriptor := cluster.RemoteSessionDescriptor{
		Address: session.Address, SnapshotToken: hex.EncodeToString(session.Token[:]),
		MetadataBytes: session.MetadataBytes, StreamBytes: session.StreamBytes,
		MaxTransferBytes: dataPlaneMax,
		FilesystemRoots:  filesystemRoots, FilesystemBytes: filesystemBytes, InternalMounts: internalMounts,
		ExecSessions: remoteExecSessions(session.ExecSessions),
	}
	tx, err = peer.Prepare(ctx, transactionID, tx.Revision, descriptor)
	if err != nil {
		return selector{}, fmt.Errorf("restoring target node %s: %w", targetNode, err)
	}
	tx, err = publishRemoteTarget(ctx, peer, transactionID, tx.Revision, paused)
	if err != nil {
		return selector{}, fmt.Errorf("publishing target node %s: %w", targetNode, err)
	}
	committed = true
	monitorBackend := *b
	monitorBackend.stdin = strings.NewReader("")
	monitorBackend.stdout = io.Discard
	monitorBackend.stderr = io.Discard
	if tx.Negotiated.PagingPolicy.Push == ttcpolicy.PushDemandOnly {
		b.distributedSource.scheduleHandoff(func() {
			monitorBackend.handoffDistributedFork(peer, source.container, transactionID, tx.Revision, relay)
		})
	} else {
		go monitorBackend.monitorDistributedFork(peer, source.container, transactionID, tx.Revision, relay)
	}
	return target, nil
}

// forkRemoteDehydrated restores a process-free branch directly from its
// durable kernel metadata and sparse memory graph. No source Sentry is
// created, and the target is independent as soon as publication completes.
func (b *runtimeBackend) forkRemoteDehydrated(source selector, sourceInfo ttcbundle.ContainerInfo, peer *cluster.Client, targetNode string, target selector, paused bool, committedTransaction *string) (result selector, retErr error) {
	sourceBranch, branchErr := persistentBranchInfo(sourceInfo, source.branch)
	if branchErr != nil {
		return selector{}, branchErr
	}
	if err := b.ensureLocalColdBranch(sourceInfo, sourceBranch); err != nil {
		return selector{}, fmt.Errorf("materializing dehydrated remote-fork source: %w", err)
	}
	transactionID, err := newDistributedTransactionID()
	if err != nil {
		return selector{}, fmt.Errorf("generating distributed transaction ID: %w", err)
	}
	rootfsDigest, err := distributedRootfsDigest(sourceInfo, source.branch, b.distributedSource.capabilities.RepositoryID)
	if err != nil {
		return selector{}, err
	}
	identity := cluster.ForkIdentity{
		TransactionID: transactionID, SourceNodeID: b.distributedSource.capabilities.NodeID,
		TargetNodeID: targetNode, SourceContainer: source.container, SourceBranch: source.branch,
		TargetContainer: target.container, TargetBranch: transactionID, TargetVisibleBranch: target.branch,
		ImageDigest: sourceInfo.Digest, RootfsDigest: rootfsDigest,
		RepositoryID: b.distributedSource.capabilities.RepositoryID,
	}
	family, err := b.portableDistributedFamily(sourceInfo)
	if err != nil {
		return selector{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tx, err := reserveRemoteTarget(ctx, targetNode, b.distributedSource.directory.Lookup, func(ctx context.Context) (*cluster.Transaction, error) {
		return peer.Reserve(ctx, cluster.ReserveRequest{SourceCapabilities: b.distributedSource.capabilities, Identity: identity, Family: family})
	})
	if err != nil {
		return selector{}, fmt.Errorf("reserving target node %s: %w", targetNode, err)
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		status, statusErr := peer.Status(cleanupCtx, transactionID)
		if statusErr == nil {
			_, _ = peer.Cancel(cleanupCtx, transactionID, status.Revision)
		} else {
			_, _ = peer.Cancel(cleanupCtx, transactionID, tx.Revision)
		}
	}()

	imageDir := existingMemoryBranchDir(b.activeDataRoot(), sourceInfo.ID, "dehydrated", sourceBranch)
	controlData, err := os.ReadFile(filepath.Join(imageDir, "control.json"))
	if err != nil {
		return selector{}, fmt.Errorf("reading dehydrated source control state: %w", err)
	}
	var capture boot.BranchDehydrateInfo
	if err := json.Unmarshal(controlData, &capture); err != nil {
		return selector{}, fmt.Errorf("decoding dehydrated source control state: %w", err)
	}
	if capture.FilesystemBranch == "" || capture.SnapshotID == 0 || len(capture.MemoryRoots) == 0 {
		return selector{}, errors.New("dehydrated source has no graph-native memory roots")
	}
	encodedMetadata, err := os.ReadFile(filepath.Join(imageDir, "metadata.state"))
	if err != nil {
		return selector{}, fmt.Errorf("reading dehydrated source kernel metadata: %w", err)
	}
	metadataRelative := filepath.Join("cluster", "memory-metadata", transactionID+".state")
	metadataPath := filepath.Join(b.dataRoot, metadataRelative)
	if err := writeDurableDistributedFile(metadataPath, encodedMetadata); err != nil {
		return selector{}, fmt.Errorf("publishing distributed metadata: %w", err)
	}
	defer os.Remove(metadataPath)
	graphRestore, err := b.openGraphMemoryRestore(capture)
	if err != nil {
		return selector{}, fmt.Errorf("opening dehydrated graph memory: %w", err)
	}
	graphManifest, err := io.ReadAll(graphRestore.manifest)
	graphRestore.close()
	if err != nil {
		return selector{}, fmt.Errorf("reading dehydrated graph manifest: %w", err)
	}
	decodedGraphManifest, err := ttcchunk.DecodeManifest(graphManifest)
	if err != nil {
		return selector{}, fmt.Errorf("decoding dehydrated graph manifest: %w", err)
	}
	graphManifestRelative := filepath.Join("cluster", "memory-manifests", transactionID+".manifest")
	graphManifestPath := filepath.Join(b.dataRoot, graphManifestRelative)
	if err := writeDurableDistributedFile(graphManifestPath, graphManifest); err != nil {
		return selector{}, fmt.Errorf("publishing distributed graph manifest: %w", err)
	}
	defer os.Remove(graphManifestPath)

	childFilesystemBranch := ttcvolume.ScopedBranchID(sourceInfo.ID, transactionID)

	filesystemForks, finishFilesystemOrigins, err := b.forkDehydratedDistributedFilesystems(sourceInfo.ID, capture.FilesystemBranch, childFilesystemBranch)
	if err != nil {
		return selector{}, err
	}
	filesystemCommitted := false
	committedFilesystemForks := 0
	defer func() {
		if filesystemCommitted {
			if !committed {
				_ = b.removeDistributedFilesystemBranch(sourceInfo.ID, childFilesystemBranch)
			}
			return
		}
		for i := len(filesystemForks) - 1; i >= committedFilesystemForks; i-- {
			_ = filesystemForks[i].abort()
		}
		if committedFilesystemForks != 0 {
			_ = b.removeDistributedFilesystemBranch(sourceInfo.ID, childFilesystemBranch)
		}
		_ = finishFilesystemOrigins(false)
	}()
	for _, fork := range filesystemForks {
		if err := fork.commit(); err != nil {
			return selector{}, fmt.Errorf("committing distributed filesystem fork: %w", err)
		}
		committedFilesystemForks++
	}
	if err := finishFilesystemOrigins(true); err != nil {
		return selector{}, fmt.Errorf("committing distributed filesystem origins: %w", err)
	}
	filesystemCommitted = true
	listener, err := net.Listen("tcp", b.distributedSource.pageListen)
	if err != nil {
		return selector{}, fmt.Errorf("opening dehydrated filesystem range listener: %w", err)
	}
	advertise, err := resolveDistributedAdvertiseAddress(b.distributedSource.pageAdvertise, listener.Addr().String())
	if err != nil {
		listener.Close()
		return selector{}, err
	}
	relayBackend := *b
	relayBackend.stdin = strings.NewReader("")
	relayBackend.stdout = io.Discard
	relayBackend.stderr = io.Discard
	relay := newDistributedPageRelay(listener, &relayBackend, sourceInfo.ID, transactionID)
	relayOwned := true
	defer func() {
		if relayOwned {
			relay.Close()
		}
	}()
	var filesystemToken ttcvolume.RemoteToken
	if _, err := rand.Read(filesystemToken[:]); err != nil {
		return selector{}, err
	}
	filesystemRoots, filesystemBytes, err := b.prepareDistributedFilesystem(sourceInfo, childFilesystemBranch, relay)
	if err != nil {
		return selector{}, fmt.Errorf("preparing dehydrated filesystem objects: %w", err)
	}
	internalMounts, err := b.portableDistributedInternalMounts(sourceInfo.ID)
	if err != nil {
		return selector{}, fmt.Errorf("capturing portable internal mounts: %w", err)
	}
	directSources, directCleanup, err := b.prepareColdDirectFilesystemSources(sourceInfo, sourceBranch, childFilesystemBranch)
	if err != nil {
		return selector{}, fmt.Errorf("opening durable filesystem sources: %w", err)
	}
	if err := relay.ConfigureDirectFilesystem(filesystemToken, directSources, directCleanup); err != nil {
		directCleanup()
		return selector{}, err
	}
	var streamBytes uint64
	for _, run := range decodedGraphManifest.Runs {
		streamBytes += uint64(run.Pages) * 4096
	}
	rootObjects := make([]string, len(capture.MemoryRoots))
	for index, root := range capture.MemoryRoots {
		rootObjects[index] = root.Root
	}
	descriptor := cluster.RemoteSessionDescriptor{
		Address: advertise, SnapshotToken: hex.EncodeToString(filesystemToken[:]),
		MetadataBytes: uint64(len(encodedMetadata)), StreamBytes: streamBytes,
		MaxTransferBytes: tx.Negotiated.PagingPolicy.TransferBytes,
		SharedMetadata:   metadataRelative, GraphManifest: graphManifestRelative,
		GraphSnapshot: capture.SnapshotID, GraphRootObjects: rootObjects,
		DurableDehydration: true, FilesystemRoots: filesystemRoots, FilesystemBytes: filesystemBytes, InternalMounts: internalMounts,
		ExecSessions: remoteExecSessions(capture.ExecSessions),
	}
	tx, err = peer.Prepare(ctx, transactionID, tx.Revision, descriptor)
	if err != nil {
		return selector{}, fmt.Errorf("restoring dehydrated target node %s: %w", targetNode, err)
	}
	tx, err = publishRemoteTarget(ctx, peer, transactionID, tx.Revision, paused)
	if err != nil {
		status, statusErr := peer.Status(ctx, transactionID)
		if statusErr != nil || (status.State != cluster.TransactionPublished && status.State != cluster.TransactionIndependent) {
			return selector{}, fmt.Errorf("publishing dehydrated target node %s: %w", targetNode, err)
		}
		tx = status
	}
	committed = true
	relayOwned = false
	go b.completeDehydratedFilesystemTransfer(peer, transactionID, tx.Revision, sourceInfo.ID, childFilesystemBranch, relay)
	if committedTransaction != nil {
		*committedTransaction = transactionID
	}
	return target, nil
}

func (b *runtimeBackend) completeDehydratedFilesystemTransfer(peer *cluster.Client, transactionID string, revision uint64, containerID, filesystemBranch string, relay *distributedPageRelay) {
	defer relay.Close()
	defer b.removeDistributedFilesystemBranch(containerID, filesystemBranch)
	lastDiagnostic := ""
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		tx, err := peer.Drain(ctx, transactionID, revision)
		cancel()
		if err == nil && tx != nil {
			revision = tx.Revision
			ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
			_, err = peer.MarkIndependentWithMetrics(ctx, transactionID, revision, cluster.TransferMetrics{})
			cancel()
			if err == nil {
				return
			}
		}
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		status, statusErr := peer.Status(ctx, transactionID)
		cancel()
		if statusErr == nil {
			revision = status.Revision
			switch status.State {
			case cluster.TransactionIndependent, cluster.TransactionCanceled, cluster.TransactionFailed:
				return
			}
		}
		diagnostic := errors.Join(err, statusErr)
		if text := fmt.Sprint(diagnostic); diagnostic != nil && text != lastDiagnostic {
			lastDiagnostic = text
			if b.stderr != nil {
				fmt.Fprintf(b.stderr, "ttcd: warning: distributed filesystem independence for %s is retrying: %v\n", transactionID, diagnostic)
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (b *runtimeBackend) removeDistributedFilesystemBranch(containerID, branch string) error {
	spec, err := b.containerStore().LoadSpec(containerID)
	if err != nil {
		return err
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(configs))
	var result error
	for _, config := range configs {
		if _, ok := seen[config.Path]; ok {
			continue
		}
		seen[config.Path] = struct{}{}
		repository, openErr := ttcvolume.OpenRepository(config.Path, ttcvolume.CopyMode(config.CopyMode))
		if openErr != nil {
			result = errors.Join(result, openErr)
			continue
		}
		removeErr := repository.RemoveBranch(branch)
		closeErr := repository.Close()
		if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			result = errors.Join(result, removeErr)
		}
		result = errors.Join(result, closeErr)
	}
	return result
}

func writeDurableDistributedFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".metadata-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

type distributedFilesystemFork struct {
	tx         *ttcvolume.ForkTransaction
	repository *ttcvolume.Repository
}

func (f *distributedFilesystemFork) commit() error {
	err := f.tx.Commit()
	_ = f.repository.Close()
	return err
}

func (f *distributedFilesystemFork) abort() error {
	err := f.tx.Abort()
	_ = f.repository.Close()
	return err
}

func (b *runtimeBackend) forkDehydratedDistributedFilesystems(containerID, sourceBranch, childBranch string) ([]*distributedFilesystemFork, func(bool) error, error) {
	assignments, finishOrigins, err := b.allocateFilesystemForkOrigins(containerID, 1)
	if err != nil {
		return nil, func(bool) error { return nil }, err
	}
	originsByRepository := make(map[string][]uint64, len(assignments))
	for _, assignment := range assignments {
		originsByRepository[assignment.ResourceID.Path] = assignment.Origins
	}
	spec, err := b.containerStore().LoadSpec(containerID)
	if err != nil {
		return nil, finishOrigins, errors.Join(err, finishOrigins(false))
	}
	configs, err := specutils.TTCVolumeRepositories(spec)
	if err != nil {
		return nil, finishOrigins, errors.Join(err, finishOrigins(false))
	}
	transactions := make([]*distributedFilesystemFork, 0, len(configs))
	seen := make(map[string]bool)
	for _, config := range configs {
		if seen[config.Path] {
			continue
		}
		seen[config.Path] = true
		dir, err := os.Open(config.Path)
		if err != nil {
			return transactions, finishOrigins, errors.Join(err, abortFilesystemForks(transactions), finishOrigins(false))
		}
		repository, err := ttcvolume.OpenRepositoryFD(dir, ttcvolume.CopyMode(config.CopyMode))
		_ = dir.Close()
		if err != nil {
			return transactions, finishOrigins, errors.Join(err, abortFilesystemForks(transactions), finishOrigins(false))
		}
		var tx *ttcvolume.ForkTransaction
		if origins := originsByRepository[repository.Info().ID]; len(origins) != 0 {
			tx, err = repository.BeginForkWithOrigins(repositoryPhysicalBranch(config, sourceBranch), childBranch,
				ttcsnapshot.SnapshotID(origins[0]), ttcsnapshot.SnapshotID(origins[1]))
		} else {
			tx, err = repository.BeginFork(repositoryPhysicalBranch(config, sourceBranch), childBranch)
		}
		if err != nil {
			repository.Close()
			return transactions, finishOrigins, errors.Join(err, abortFilesystemForks(transactions), finishOrigins(false))
		}
		// ForkTransaction retains its repository; close after Commit/Abort is not
		// currently exposed, so keep the donated descriptor for transaction life.
		transactions = append(transactions, &distributedFilesystemFork{tx: tx, repository: repository})
	}
	return transactions, finishOrigins, nil
}

func abortFilesystemForks(transactions []*distributedFilesystemFork) error {
	var result error
	for i := len(transactions) - 1; i >= 0; i-- {
		result = errors.Join(result, transactions[i].abort())
	}
	return result
}

// forkRemoteMany reserves multiple targets before pausing the source, then
// captures all sibling memory/filesystem branches in one Sentry operation.
// Each child retains an independent transaction and page session after the
// shared capture.
func (b *runtimeBackend) forkRemoteMany(source selector, targetNode string, targetContainers []string, paused bool) ([]selector, error) {
	if len(targetContainers) == 0 {
		return nil, errors.New("distributed fork requires at least one destination")
	}
	if len(targetContainers) == 1 {
		target, err := b.forkRemote(source, targetNode, targetContainers[0], paused)
		if err != nil {
			return nil, err
		}
		return []selector{target}, nil
	}
	if b.distributedSource == nil {
		return nil, errors.New("ttcd is not configured for distributed forks")
	}
	peer, err := b.distributedSource.peer(targetNode, true)
	if err != nil {
		return nil, err
	}
	destinations := make([]selector, len(targetContainers))
	seenTargets := make(map[string]struct{}, len(targetContainers))
	for i, value := range targetContainers {
		target, err := parseSelector(value)
		if err != nil {
			return nil, fmt.Errorf("invalid distributed destination %q: %w", value, err)
		}
		if _, ok := seenTargets[target.String()]; ok {
			return nil, fmt.Errorf("distributed fork repeats target %q", target)
		}
		seenTargets[target.String()] = struct{}{}
		destinations[i] = target
	}
	resolved, sourceInfo, err := b.resolveSelector(source)
	if err != nil {
		return nil, err
	}
	source = resolved
	if !sourceInfo.LiveForkEligible {
		return nil, fmt.Errorf("container family %q is not live fork eligible: %s", sourceInfo.Name, sourceInfo.LiveForkWaiver)
	}
	sourceBranch, err := persistentBranchInfo(sourceInfo, source.branch)
	if err != nil {
		return nil, err
	}
	if _, err := b.renewDistributedExecutionAuthority(); err != nil {
		return nil, fmt.Errorf("renewing source execution authority: %w", err)
	}
	if state := normalizeLifecycleState(sourceBranch.State); state == "dehydrated" || state == "dehydrating" {
		results := make([]selector, 0, len(targetContainers))
		transactions := make([]string, 0, len(targetContainers))
		for _, target := range destinations {
			var transactionID string
			result, err := b.forkRemoteDehydrated(source, sourceInfo, peer, targetNode, target, paused, &transactionID)
			if err == nil {
				results = append(results, result)
				transactions = append(transactions, transactionID)
				continue
			}
			// Cold targets are already source-independent when the helper
			// returns. Remove earlier siblings so the batch remains all-or-none
			// from the caller's perspective.
			var rollbackErr error
			for i := len(transactions) - 1; i >= 0; i-- {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
				status, statusErr := peer.Status(cleanupCtx, transactions[i])
				if statusErr == nil {
					_, cancelErr := peer.Cancel(cleanupCtx, transactions[i], status.Revision)
					// Cancel of an independent transaction removes the runtime
					// before the terminal-state transition is rejected.
					if cancelErr != nil {
						if _, verifyErr := b.containerStore().LoadContainer(results[i].container); verifyErr == nil {
							rollbackErr = errors.Join(rollbackErr, cancelErr)
						}
					}
				} else {
					rollbackErr = errors.Join(rollbackErr, statusErr)
				}
				cleanupCancel()
			}
			return nil, errors.Join(fmt.Errorf("forking dehydrated target %s on node %s: %w", target, targetNode, err), rollbackErr)
		}
		return results, nil
	}
	finishForegroundCapture := b.distributedSource.beginForegroundCapture()
	foregroundCaptureActive := true
	defer func() {
		if foregroundCaptureActive {
			finishForegroundCapture()
		}
	}()
	rootfsDigest, err := distributedRootfsDigest(sourceInfo, source.branch, b.distributedSource.capabilities.RepositoryID)
	if err != nil {
		return nil, err
	}
	type childState struct {
		target       selector
		transaction  string
		tx           *cluster.Transaction
		relay        *distributedPageRelay
		advertise    string
		dataPlaneMax uint64
		captured     bool
	}
	children := make([]childState, len(targetContainers))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		for i := range children {
			child := &children[i]
			child.relay.Close()
			if child.transaction == "" {
				continue
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
			if status, statusErr := peer.Status(cleanupCtx, child.transaction); statusErr == nil {
				_, _ = peer.Cancel(cleanupCtx, child.transaction, status.Revision)
			} else if child.tx != nil {
				_, _ = peer.Cancel(cleanupCtx, child.transaction, child.tx.Revision)
			}
			cleanupCancel()
			if child.captured {
				_, _ = b.command("timetravel", source.container, "distributed-cancel", child.transaction)
			}
		}
	}()

	var commonPolicy *ttcpolicy.Policy
	var commonDataPlaneMax uint64
	family, err := b.portableDistributedFamily(sourceInfo)
	if err != nil {
		return nil, err
	}
	for i, target := range destinations {
		transactionID, err := newDistributedTransactionID()
		if err != nil {
			return nil, fmt.Errorf("generating distributed transaction ID: %w", err)
		}
		identity := cluster.ForkIdentity{
			TransactionID: transactionID, SourceNodeID: b.distributedSource.capabilities.NodeID,
			TargetNodeID: targetNode, SourceContainer: source.container, SourceBranch: source.branch,
			TargetContainer: target.container, TargetBranch: transactionID, TargetVisibleBranch: target.branch,
			ImageDigest: sourceInfo.Digest, RootfsDigest: rootfsDigest,
			RepositoryID: b.distributedSource.capabilities.RepositoryID,
		}
		tx, err := reserveRemoteTarget(ctx, targetNode, b.distributedSource.directory.Lookup, func(ctx context.Context) (*cluster.Transaction, error) {
			return peer.Reserve(ctx, cluster.ReserveRequest{SourceCapabilities: b.distributedSource.capabilities, Identity: identity, Family: family})
		})
		if err != nil {
			return nil, fmt.Errorf("reserving target %s on node %s: %w", target, targetNode, err)
		}
		dataPlaneMax := tx.Negotiated.PagingPolicy.TransferBytes
		if tx.Negotiated.PagingPolicy.Prefetch == ttcpolicy.PrefetchAdaptive {
			dataPlaneMax = tx.Negotiated.MaxTransferSize
		}
		if commonPolicy == nil {
			policy := tx.Negotiated.PagingPolicy
			commonPolicy = &policy
			commonDataPlaneMax = dataPlaneMax
		} else if *commonPolicy != tx.Negotiated.PagingPolicy || commonDataPlaneMax != dataPlaneMax {
			return nil, fmt.Errorf("target %s negotiated a different paging policy within one sibling batch", target)
		}
		listener, err := net.Listen("tcp", b.distributedSource.pageListen)
		if err != nil {
			return nil, fmt.Errorf("opening distributed page listener for %s: %w", target, err)
		}
		advertise, err := resolveDistributedAdvertiseAddress(b.distributedSource.pageAdvertise, listener.Addr().String())
		if err != nil {
			listener.Close()
			return nil, err
		}
		relayBackend := *b
		relayBackend.stdin = strings.NewReader("")
		relayBackend.stdout = io.Discard
		relayBackend.stderr = io.Discard
		children[i] = childState{
			target: target, transaction: transactionID, tx: tx,
			relay:     newDistributedPageRelay(listener, &relayBackend, source.container, transactionID),
			advertise: advertise, dataPlaneMax: dataPlaneMax,
		}
	}
	captureArgs := boot.DistributedForkCaptureManyArgs{
		Source: source.branch, FamilyID: source.container,
		MaxTransferBytes: commonDataPlaneMax, PushPolicy: sentryremote.TCPPushPolicy(commonPolicy.Push),
		PushHighWater: commonPolicy.MaxOutstandingBytes,
		Children:      make([]boot.DistributedForkCaptureChildArgs, len(children)),
	}
	filesystemOrigins, finishFilesystemOrigins, err := b.allocateFilesystemForkOrigins(sourceInfo.ID, len(children))
	if err != nil {
		return nil, err
	}
	filesystemOriginsCommitted := false
	defer func() {
		if !filesystemOriginsCommitted {
			_ = finishFilesystemOrigins(false)
		}
	}()
	captureArgs.FilesystemOrigins = filesystemOrigins
	for i := range children {
		captureArgs.Children[i] = boot.DistributedForkCaptureChildArgs{
			SessionID: children[i].transaction, TargetBranchID: children[i].transaction,
			AdvertiseAddress: children[i].advertise,
		}
	}
	encodedConfig, err := json.Marshal(&captureArgs)
	if err != nil {
		return nil, fmt.Errorf("encoding distributed batch config: %w", err)
	}
	configReader, err := newConfigMemFD("ttc-distributed-batch-config", encodedConfig)
	if err != nil {
		return nil, fmt.Errorf("creating distributed batch config: %w", err)
	}
	output, err := b.commandWithFiles([]*os.File{configReader}, "timetravel", "--distributed-config-fd=3",
		source.container, "distributed-capture-many", children[0].transaction)
	finishForegroundCapture()
	foregroundCaptureActive = false
	configReader.Close()
	if err != nil {
		return nil, fmt.Errorf("capturing %d distributed siblings: %w", len(children), err)
	}
	if err := finishFilesystemOrigins(true); err != nil {
		return nil, err
	}
	filesystemOriginsCommitted = true
	var sessions []boot.DistributedForkSessionInfo
	if err := json.Unmarshal(output, &sessions); err != nil {
		return nil, fmt.Errorf("decoding distributed sibling capture: %w", err)
	}
	if len(sessions) != len(children) {
		return nil, fmt.Errorf("source returned %d sessions for %d siblings", len(sessions), len(children))
	}
	for i := range children {
		children[i].captured = true
	}
	sessionByID := make(map[string]boot.DistributedForkSessionInfo, len(sessions))
	for _, session := range sessions {
		sessionByID[session.SessionID] = session
	}
	descriptors := make([]cluster.RemoteSessionDescriptor, len(children))
	internalMounts, err := b.portableDistributedInternalMounts(sourceInfo.ID)
	if err != nil {
		return nil, fmt.Errorf("capturing portable internal mounts: %w", err)
	}
	for i := range children {
		child := &children[i]
		session, ok := sessionByID[child.transaction]
		if !ok || session.MetadataBytes == 0 || session.StreamBytes == 0 || session.Address != child.advertise {
			return nil, fmt.Errorf("source returned an incomplete session for target %s", child.target)
		}
		filesystemRoots, filesystemBytes, err := b.prepareDistributedFilesystem(sourceInfo, session.FilesystemBranch, child.relay)
		if err != nil {
			return nil, fmt.Errorf("sealing filesystem generation for target %s: %w", child.target, err)
		}
		descriptors[i] = cluster.RemoteSessionDescriptor{
			Address: session.Address, SnapshotToken: hex.EncodeToString(session.Token[:]),
			MetadataBytes: session.MetadataBytes, StreamBytes: session.StreamBytes,
			MaxTransferBytes: child.dataPlaneMax,
			FilesystemRoots:  filesystemRoots, FilesystemBytes: filesystemBytes, InternalMounts: internalMounts,
			ExecSessions: remoteExecSessions(session.ExecSessions),
		}
	}
	prepareErrors := make([]error, len(children))
	runBoundedBatch(len(children), distributedBatchControlConcurrency, func(i int) {
		child := &children[i]
		child.tx, prepareErrors[i] = peer.Prepare(ctx, child.transaction, child.tx.Revision, descriptors[i])
	})
	for i, prepareErr := range prepareErrors {
		if prepareErr != nil {
			return nil, fmt.Errorf("restoring target %s on node %s: %w", children[i].target, targetNode, prepareErr)
		}
	}
	publishErrors := make([]error, len(children))
	runBoundedBatch(len(children), distributedBatchControlConcurrency, func(i int) {
		child := &children[i]
		child.tx, publishErrors[i] = publishRemoteTarget(ctx, peer, child.transaction, child.tx.Revision, paused)
	})
	for i, publishErr := range publishErrors {
		if publishErr != nil {
			return nil, fmt.Errorf("publishing target %s on node %s: %w", children[i].target, targetNode, publishErr)
		}
	}
	// Every target is now externally visible and owns its durable snapshot.
	// Later independence bookkeeping failures must not cancel or delete them.
	cleanup = false
	results := make([]selector, len(children))
	for i := range children {
		child := &children[i]
		monitorBackend := *b
		monitorBackend.stdin = strings.NewReader("")
		monitorBackend.stdout = io.Discard
		monitorBackend.stderr = io.Discard
		if commonPolicy.Push == ttcpolicy.PushDemandOnly {
			b.distributedSource.scheduleHandoff(func() {
				monitorBackend.handoffDistributedFork(peer, source.container, child.transaction, child.tx.Revision, child.relay)
			})
		} else {
			go monitorBackend.monitorDistributedFork(peer, source.container, child.transaction, child.tx.Revision, child.relay)
		}
		results[i] = child.target
	}
	return results, nil
}

// handoffDistributedFork publishes fork-time pages after the target is visible,
// then atomically redirects all remaining faults from TCP to shared chunks.
// Failure before the target acknowledges the switch falls back to ordinary
// live paging; it never invalidates an already-published target.
func (b *runtimeBackend) handoffDistributedFork(peer *cluster.Client, sourceContainer, transactionID string, revision uint64, relay *distributedPageRelay) {
	// A successful handoff makes its transaction-scoped pack and intent
	// unreachable. Collection must follow the terminal transition here rather
	// than relying on the daemon's periodic maintenance loop: an elastic source
	// may stop immediately after its last dependency becomes independent.
	defer b.distributedSource.scheduleColdCollection(b.collectTerminalColdStorage)
	handoff, rootNames, rootObjects, err := b.exportDistributedGraph(sourceContainer, transactionID)
	if err != nil {
		// Ordinary demand paging may remain live for the lifetime of the target
		// when graph publication is unavailable. It must not occupy one of the
		// bounded graph-export workers and prevent independent siblings from
		// being consolidated.
		go b.monitorDistributedFork(peer, sourceContainer, transactionID, revision, relay)
		return
	}
	var tx *cluster.Transaction
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		tx, err = peer.Handoff(ctx, transactionID, revision, handoff)
		if err != nil {
			if current, statusErr := peer.Status(ctx, transactionID); statusErr == nil {
				tx = current
				revision = current.Revision
				switch current.State {
				case cluster.TransactionDraining, cluster.TransactionIndependent:
					if current.GraphHandoff != nil && *current.GraphHandoff == handoff {
						err = nil
					} else if current.State == cluster.TransactionDraining && current.GraphHandoff == nil {
						// A concurrent node drain may eagerly materialize the target
						// before this optional graph handoff is bound. Eager drain is
						// already a valid route to independence; do not retry handoff
						// forever merely because the faster strategy lost the race.
						err = nil
					} else if current.State == cluster.TransactionIndependent {
						// The target faulted every page from TCP before handoff.
						cancel()
						_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
						_ = os.Remove(filepath.Join(b.dataRoot, handoff.GraphManifest))
						_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
						relay.Close()
						return
					}
				case cluster.TransactionCanceled, cluster.TransactionFailed:
					cancel()
					_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
					_ = os.Remove(filepath.Join(b.dataRoot, handoff.GraphManifest))
					_, _ = b.command("timetravel", sourceContainer, "distributed-cancel", transactionID)
					relay.Close()
					return
				}
			}
		}
		cancel()
		if err == nil && tx != nil {
			break
		}
		// Once publication succeeds, never delete its root merely because an
		// RPC response or reader switch failed. The target may have completed
		// the switch, and both the durable descriptor and Sentry operation are
		// idempotent. Retain live paging and retry until target acknowledgement.
		time.Sleep(500 * time.Millisecond)
	}
	if tx.State == cluster.TransactionIndependent {
		_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
		relay.Close()
		return
	}
	if tx.GraphHandoff == nil {
		// Eager drain won the race, so this source-side sparse publication was
		// never made authoritative and can be reclaimed immediately.
		_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
		_ = os.Remove(filepath.Join(b.dataRoot, handoff.GraphManifest))
	}
	if err := b.completeSharedDistributedFork(peer, sourceContainer, transactionID, tx.Revision, relay); err != nil {
		// Retrying status/independence is safe; the target now has no source
		// reads, while the source retains its snapshot until completion.
		go b.monitorHandoffCompletion(peer, sourceContainer, transactionID, tx.Revision, relay, rootNames, rootObjects, handoff.GraphManifest)
		return
	}
	_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
	_ = os.Remove(filepath.Join(b.dataRoot, handoff.GraphManifest))
}

func (b *runtimeBackend) monitorHandoffCompletion(peer *cluster.Client, sourceContainer, transactionID string, revision uint64, relay *distributedPageRelay, rootNames, rootObjects []string, manifest string) {
	defer relay.Close()
	// This monitor owns the exported root after the synchronous completion path
	// hands off to it. All of its return paths are terminal, so release both the
	// authoritative reference and the transient restore manifest exactly once.
	// Previously the lost-response path completed the Sentry session but leaked
	// these two publication artifacts indefinitely.
	defer func() {
		_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
		if manifest != "" {
			_ = os.Remove(filepath.Join(b.dataRoot, manifest))
		}
		b.collectTerminalColdStorage()
	}()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		status, err := peer.Status(ctx, transactionID)
		cancel()
		if err == nil {
			revision = status.Revision
			switch status.State {
			case cluster.TransactionIndependent:
				_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
				return
			case cluster.TransactionCanceled, cluster.TransactionFailed:
				_, _ = b.command("timetravel", sourceContainer, "distributed-cancel", transactionID)
				return
			}
		}
		if err := b.completeSharedDistributedFork(peer, sourceContainer, transactionID, revision, relay); err == nil {
			return
		}
	}
}

func (b *runtimeBackend) completeSharedDistributedFork(peer *cluster.Client, sourceContainer, transactionID string, revision uint64, relay *distributedPageRelay) error {
	output, err := b.command("timetravel", sourceContainer, "distributed-status", transactionID)
	if err != nil {
		return fmt.Errorf("reading shared-snapshot session status: %w", err)
	}
	var status boot.DistributedForkSessionStatus
	if err := json.Unmarshal(output, &status); err != nil {
		return fmt.Errorf("decoding shared-snapshot session status: %w", err)
	}
	metrics := cluster.TransferMetrics{
		PullRequests: status.Transport.PullRequests, AckRequests: status.Transport.AckRequests,
		AckBatches: status.Transport.AckBatches, PullBytes: status.Transport.BytesSent,
		MetadataBytes: status.Transport.MetadataBytesSent, PushBytes: status.Transport.PushBytesSent,
		PushRetries: status.Transport.PushRetries, PagesTotal: status.Snapshot.PagesTotal,
		// The durable snapshot replaces every live source-page reference at this
		// transition. CompleteDistributedForkSession releases those references
		// immediately after the control plane records independence.
		PagesReleased: status.Snapshot.PagesTotal, PushHints: status.Snapshot.PushEnqueued,
		PushHintsDropped: status.Snapshot.PushDropped,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	_, err = peer.MarkIndependentWithMetrics(ctx, transactionID, revision, metrics)
	cancel()
	if err != nil {
		return fmt.Errorf("recording shared-snapshot independence: %w", err)
	}
	if _, err := b.command("timetravel", sourceContainer, "distributed-complete", transactionID); err != nil {
		return fmt.Errorf("releasing shared-snapshot source references: %w", err)
	}
	relay.Close()
	return nil
}

func (b *runtimeBackend) monitorDistributedFork(peer *cluster.Client, sourceContainer, transactionID string, revision uint64, relay *distributedPageRelay) {
	defer relay.Close()
	finishMonitor := b.distributedSource.beginSourceMonitor()
	defer finishMonitor()
	for {
		timer := time.NewTimer(b.distributedSource.sourceMonitorInterval())
		<-timer.C
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		peerStatus, statusErr := peer.Status(ctx, transactionID)
		cancel()
		if statusErr == nil {
			revision = peerStatus.Revision
			switch peerStatus.State {
			case cluster.TransactionIndependent:
				_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
				return
			case cluster.TransactionCanceled, cluster.TransactionFailed:
				_, _ = b.command("timetravel", sourceContainer, "distributed-cancel", transactionID)
				return
			}
		}
		releasePoll, admitted := b.distributedSource.trySourcePoll()
		if !admitted {
			continue
		}
		output, err := b.command("timetravel", sourceContainer, "distributed-status", transactionID)
		releasePoll()
		if err != nil {
			return
		}
		var status boot.DistributedForkSessionStatus
		if err := json.Unmarshal(output, &status); err != nil {
			return
		}
		if status.Snapshot.PagesOutstanding != 0 {
			continue
		}
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		draining, drainErr := peer.Drain(ctx, transactionID, revision)
		cancel()
		if drainErr != nil || draining == nil {
			continue
		}
		revision = draining.Revision
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		metrics := cluster.TransferMetrics{
			PullRequests:     status.Transport.PullRequests,
			AckRequests:      status.Transport.AckRequests,
			AckBatches:       status.Transport.AckBatches,
			PullBytes:        status.Transport.BytesSent,
			MetadataBytes:    status.Transport.MetadataBytesSent,
			PushBytes:        status.Transport.PushBytesSent,
			PushRetries:      status.Transport.PushRetries,
			PagesTotal:       status.Snapshot.PagesTotal,
			PagesReleased:    status.Snapshot.PagesReleased,
			PushHints:        status.Snapshot.PushEnqueued,
			PushHintsDropped: status.Snapshot.PushDropped,
		}
		tx, err := peer.MarkIndependentWithMetrics(ctx, transactionID, revision, metrics)
		if err != nil {
			if current, statusErr := peer.Status(ctx, transactionID); statusErr == nil && current.State == cluster.TransactionIndependent {
				tx = current
				err = nil
			}
		}
		cancel()
		if err != nil || tx == nil {
			continue
		}
		_, _ = b.command("timetravel", sourceContainer, "distributed-complete", transactionID)
		return
	}
}

func resumeDistributedForkMonitors(targetRuntime *daemonTargetRuntime, backend *runtimeBackend, runtime *daemonClusterRuntime) {
	if targetRuntime == nil || backend == nil || runtime == nil || runtime.controlPlane == nil || runtime.controlPlane.Store == nil {
		return
	}
	transactions, err := runtime.controlPlane.Store.List()
	if err != nil {
		return
	}
	localNode := runtime.controlPlane.Capabilities.NodeID
	for i := range transactions {
		tx := transactions[i]
		// Independent is normally terminal, but older or interrupted activation
		// recording can leave an authorized target independent yet unactivated.
		// Roll that safe paused state forward before applying the terminal fast
		// path.
		if tx.Terminal() && !(tx.State == cluster.TransactionIndependent && !tx.Activated) {
			continue
		}
		if tx.Identity.TargetNodeID == localNode && (tx.State == cluster.TransactionPublished || tx.State == cluster.TransactionDraining || tx.State == cluster.TransactionIndependent) {
			if info, err := backend.containerStore().LoadDistributedForkTarget(tx.Identity.TransactionID); err == nil {
				if !tx.Activated {
					if placements, openErr := backend.placementCatalog(); openErr == nil {
						placementBranch := tx.Identity.VisibleTargetBranch()
						if tx.Identity.Migration {
							placementBranch = tx.Identity.SourceBranch
						}
						if placement, lookupErr := placements.Lookup(tx.Identity.SourceContainer, placementBranch); lookupErr == nil &&
							placement.OwnerNode == localNode && placement.OwnerIncarnation == targetRuntime.incarnation &&
							(!tx.Identity.Migration || (placement.LastOperationID == tx.Identity.TransactionID && placement.State != cluster.PlacementMigrating)) {
							txCopy := tx
							go recoverTargetActivation(targetRuntime, runtime.controlPlane, &txCopy)
						}
					}
				}
				if tx.State == cluster.TransactionPublished && tx.GraphHandoff != nil {
					txCopy := tx
					go recoverTargetGraphHandoff(targetRuntime, runtime.controlPlane, &txCopy)
				}
				targetRuntime.monitorDependency(tx.Identity.TransactionID, info.ID)
			}
		}
		if tx.Identity.SourceNodeID != localNode || !tx.SourceNeeded ||
			(tx.State != cluster.TransactionPublished && tx.State != cluster.TransactionDraining) {
			continue
		}
		peer, err := runtime.source.peer(tx.Identity.TargetNodeID, false)
		if err != nil {
			continue
		}
		if tx.Identity.Migration && tx.State == cluster.TransactionPublished && tx.OwnershipCommitted && !tx.Activated {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			activated, activateErr := activateRemoteMigration(ctx, peer, tx.Identity.TransactionID, tx.Revision)
			cancel()
			if activateErr != nil || activated == nil || !activated.Activated {
				continue
			}
			tx = *activated
		}
		monitorBackend := *backend
		monitorBackend.stdin = strings.NewReader("")
		monitorBackend.stdout = io.Discard
		monitorBackend.stderr = io.Discard
		if tx.Identity.Migration {
			if tx.State == cluster.TransactionDraining {
				manifest := ""
				if tx.GraphHandoff != nil {
					manifest = tx.GraphHandoff.GraphManifest
				}
				go monitorBackend.monitorHandoffCompletion(peer, tx.Identity.SourceContainer,
					tx.Identity.TransactionID, tx.Revision, nil, nil, nil, manifest)
			} else {
				runtime.source.scheduleHandoff(func() {
					monitorBackend.handoffDistributedFork(peer, tx.Identity.SourceContainer,
						tx.Identity.TransactionID, tx.Revision, nil)
				})
			}
		} else if tx.Negotiated.PagingPolicy.Push == ttcpolicy.PushDemandOnly {
			if tx.State == cluster.TransactionDraining {
				manifest := ""
				if tx.GraphHandoff != nil {
					manifest = tx.GraphHandoff.GraphManifest
				}
				go monitorBackend.monitorHandoffCompletion(peer, tx.Identity.SourceContainer,
					tx.Identity.TransactionID, tx.Revision, nil, nil, nil, manifest)
			} else {
				runtime.source.scheduleHandoff(func() {
					monitorBackend.handoffDistributedFork(peer, tx.Identity.SourceContainer,
						tx.Identity.TransactionID, tx.Revision, nil)
				})
			}
		} else {
			go monitorBackend.monitorDistributedFork(peer, tx.Identity.SourceContainer,
				tx.Identity.TransactionID, tx.Revision, nil)
		}
		if tx.Identity.Migration {
			go monitorBackend.monitorSparseMigrationCleanup(peer, tx.Identity.SourceContainer,
				tx.Identity.SourceBranch, tx.Identity.TransactionID, nil)
		}
	}
}

func recoverTargetActivation(targetRuntime *daemonTargetRuntime, control *cluster.ControlPlane, tx *cluster.Transaction) {
	if targetRuntime == nil || control == nil || control.Store == nil || tx == nil || tx.Activated {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err := targetRuntime.Activate(ctx, tx)
	cancel()
	if err != nil {
		return
	}
	_, _ = control.Store.MarkActivated(tx.Identity.TransactionID, tx.Revision, control.Now())
}

func recoverTargetGraphHandoff(targetRuntime *daemonTargetRuntime, control *cluster.ControlPlane, tx *cluster.Transaction) {
	if targetRuntime == nil || control == nil || control.Store == nil || tx == nil || tx.GraphHandoff == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	err := targetRuntime.Handoff(ctx, tx, *tx.GraphHandoff)
	cancel()
	if err != nil {
		return
	}
	_, _ = control.Store.Advance(tx.Identity.TransactionID, tx.Revision, cluster.TransactionDraining, "", control.Now())
}
