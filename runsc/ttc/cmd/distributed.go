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
	"maps"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/abi/linux"
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
	fd, err := memutil.CreateMemFD(name, linux.MFD_CLOEXEC)
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
	handoffCount  atomic.Int64
	handoffGate   sync.RWMutex
	collectNeeded atomic.Bool
	collectActive atomic.Bool
	monitorOnce   sync.Once
	monitorSlots  chan struct{}
	monitorCount  atomic.Int64
	sourceLocks   [64]sync.Mutex
	originLocks   [64]sync.Mutex
	// building has the origins being built: a capture's memory origin,
	// which owns no object until the capture's first objects commit, and
	// the filesystem origins a bundle has yet to install. Either may be
	// built without the lifecycle lock, so the cleanup of empty and
	// orphaned origins leaves them alone. allocating is held, for reading,
	// from an origin's allocation until it is building, so that cleanup can
	// wait for each origin allocated before it looked to be marked.
	buildingMu sync.Mutex
	building   map[ttcsnapshot.SnapshotID]struct{}
	allocating sync.RWMutex
	// reclamationMu serializes terminal physical-source reclamation across
	// families. Each reclamation performs repository-wide origin inventory and
	// provider reconciliation, so per-family locks alone are insufficient: one
	// family deleting its private repository can otherwise race another
	// family's global inventory opening that same repository.
	reclamationMu sync.Mutex
	pageListen    string
	pageAdvertise string
	// exports has, by transaction, a function that stops the transaction's
	// live graph export: one that no target will read.
	exports sync.Map
	// rollingForward has the memory origins whose frozen handoffs a
	// background roll-forward is completing.
	rollingForward sync.Map
	// syncedOwners has, by family and branch, the placement generation and
	// owner that placement reconciliation last brought the family's record in
	// line with.
	syncedOwners sync.Map
	// providerChecks has, by operation, the providerCheck of each provider
	// claim that its last check found still needed.
	providerChecks sync.Map
	// parked has, by container ID, a count of the times this daemon parked
	// runtime memory backings of the container: see noteParked.
	parkedMu sync.Mutex
	parked   map[string]uint64
	// parkedKnown is set once parked also has the containers whose backings
	// an earlier daemon parked.
	parkedKnown bool
}

// noteParked records that this daemon parked a runtime memory backing of the
// container id, for reconcileConsumedMemoryBacking to release.
func (r *distributedSourceRuntime) noteParked(id string) {
	r.parkedMu.Lock()
	defer r.parkedMu.Unlock()
	if r.parked == nil {
		r.parked = make(map[string]uint64)
	}
	r.parked[id]++
}

// parkedContainers returns the containers with backings to release, with
// their counts, and whether it knows them all.
func (r *distributedSourceRuntime) parkedContainers() (map[string]uint64, bool) {
	r.parkedMu.Lock()
	defer r.parkedMu.Unlock()
	return maps.Clone(r.parked), r.parkedKnown
}

// releasedParked forgets the containers found with no backings to release,
// unless they were parked again since they were counted, and records that
// parked knows every container if all were read.
func (r *distributedSourceRuntime) releasedParked(released map[string]uint64, all bool) {
	r.parkedMu.Lock()
	defer r.parkedMu.Unlock()
	for id, count := range released {
		if r.parked[id] == count {
			delete(r.parked, id)
		}
	}
	if all {
		r.parkedKnown = true
	}
}

type syncedPlacementOwner struct {
	generation uint64
	owner      string
}

// ownerSynced reports whether the family record's owner of the branch whose
// placement is record, under key, was brought in line with it.
func (r *distributedSourceRuntime) ownerSynced(key string, record cluster.PlacementRecord) bool {
	synced, ok := r.syncedOwners.Load(key)
	return ok && synced.(syncedPlacementOwner) == syncedPlacementOwner{generation: record.Generation, owner: record.OwnerNode}
}

func (r *distributedSourceRuntime) markOwnerSynced(key string, record cluster.PlacementRecord) {
	r.syncedOwners.Store(key, syncedPlacementOwner{generation: record.Generation, owner: record.OwnerNode})
}

// forgetRemovedOwners forgets the branches that have no placement.
func (r *distributedSourceRuntime) forgetRemovedOwners(placements map[string]cluster.PlacementRecord) {
	r.syncedOwners.Range(func(key, _ any) bool {
		if _, ok := placements[key.(string)]; !ok {
			r.syncedOwners.Delete(key)
		}
		return true
	})
}

// errExportAbandoned is the error of a live graph export stopped because
// no target will read it.
var errExportAbandoned = errors.New("live graph export was abandoned: its target was removed")

// abortExport stops the live graph export of transactionID, if one runs.
func (r *distributedSourceRuntime) abortExport(transactionID string) {
	if r == nil {
		return
	}
	if stop, ok := r.exports.Load(transactionID); ok {
		stop.(func())()
	}
}

func (r *distributedSourceRuntime) lockReclamation() func() {
	if r == nil {
		return func() {}
	}
	r.reclamationMu.Lock()
	return r.reclamationMu.Unlock
}

// buildOrigin marks origin as one a capture builds, until the returned
// function is called.
func (r *distributedSourceRuntime) buildOrigin(origin ttcsnapshot.SnapshotID) func() {
	if r == nil {
		return func() {}
	}
	r.buildingMu.Lock()
	defer r.buildingMu.Unlock()
	if r.building == nil {
		r.building = make(map[ttcsnapshot.SnapshotID]struct{})
	}
	r.building[origin] = struct{}{}
	var once sync.Once
	return func() {
		once.Do(func() {
			r.buildingMu.Lock()
			defer r.buildingMu.Unlock()
			delete(r.building, origin)
		})
	}
}

// buildingOrigin reports whether a capture builds origin.
func (r *distributedSourceRuntime) buildingOrigin(origin ttcsnapshot.SnapshotID) bool {
	if r == nil {
		return false
	}
	r.buildingMu.Lock()
	defer r.buildingMu.Unlock()
	_, ok := r.building[origin]
	return ok
}

// awaitOriginAllocations waits until each origin allocated so far is marked
// building, if a capture builds it.
func (r *distributedSourceRuntime) awaitOriginAllocations() {
	if r == nil {
		return
	}
	r.allocating.Lock()
	r.allocating.Unlock()
}

func (r *distributedSourceRuntime) lockSnapshotOrigin(origin ttcsnapshot.SnapshotID) func() {
	if r == nil || origin == 0 {
		return func() {}
	}
	lock := &r.originLocks[uint64(origin)%uint64(len(r.originLocks))]
	lock.Lock()
	return lock.Unlock
}

// tryLockSnapshotOrigin is lockSnapshotOrigin for a caller that holds the
// lifecycle lock, which must not wait for an origin's lock: a publication
// holds its origin's lock while it waits for the handoff gate, and a capture
// holds the gate while it waits for the lifecycle lock.
func (r *distributedSourceRuntime) tryLockSnapshotOrigin(origin ttcsnapshot.SnapshotID) (func(), bool) {
	if r == nil || origin == 0 {
		return func() {}, true
	}
	lock := &r.originLocks[uint64(origin)%uint64(len(r.originLocks))]
	if !lock.TryLock() {
		return nil, false
	}
	return lock.Unlock, true
}

func (r *distributedSourceRuntime) lockSourceFamily(family string) func() {
	if r == nil {
		return func() {}
	}
	lock := r.sourceFamilyLock(family)
	lock.Lock()
	return lock.Unlock
}

// tryLockSourceFamily is lockSourceFamily for a caller that must not wait for
// the lock while it holds another.
func (r *distributedSourceRuntime) tryLockSourceFamily(family string) (func(), bool) {
	if r == nil {
		return func() {}, true
	}
	lock := r.sourceFamilyLock(family)
	if !lock.TryLock() {
		return nil, false
	}
	return lock.Unlock, true
}

func (r *distributedSourceRuntime) sourceFamilyLock(family string) *sync.Mutex {
	// Fixed stripes keep cleanup bookkeeping bounded even when a long-lived
	// cluster creates and removes arbitrarily many family names.
	hash := uint64(14695981039346656037)
	for i := range len(family) {
		hash ^= uint64(family[i])
		hash *= 1099511628211
	}
	return &r.sourceLocks[hash%uint64(len(r.sourceLocks))]
}

const distributedSourceMonitorConcurrency = 2

func (r *distributedSourceRuntime) scheduleSourceMonitor(fn func()) {
	if fn == nil {
		return
	}
	if r == nil {
		go fn()
		return
	}
	r.monitorCount.Add(1)
	go func() {
		defer r.monitorCount.Add(-1)
		fn()
	}()
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
	r.scheduleHandoffAfter(0, fn)
}

// scheduleHandoffAfter is scheduleHandoff, delay later. Meanwhile the handoff
// counts as pending, but holds no worker.
func (r *distributedSourceRuntime) scheduleHandoffAfter(delay time.Duration, fn func()) {
	if fn == nil {
		return
	}
	if r == nil {
		go func() {
			time.Sleep(delay)
			fn()
		}()
		return
	}
	r.handoffOnce.Do(func() {
		r.handoffSlots = make(chan struct{}, distributedHandoffConcurrency)
	})
	r.handoffCount.Add(1)
	go func() {
		defer r.handoffCount.Add(-1)
		time.Sleep(delay)
		r.handoffSlots <- struct{}{}
		defer func() { <-r.handoffSlots }()
		fn()
	}()
}

// waitForBackgroundTasks is the graceful-retirement barrier for work that may
// still own transaction-scoped graph roots or a source page relay. Scheduling
// increments the counters before starting a goroutine, so observing a stable
// zero cannot miss a goroutine that has been created but has not run yet.
func (r *distributedSourceRuntime) waitForBackgroundTasks(ctx context.Context) error {
	if r == nil {
		return nil
	}
	stable := 0
	for {
		if r.handoffCount.Load() == 0 && r.monitorCount.Load() == 0 && !r.collectActive.Load() {
			stable++
			if stable == 2 {
				return nil
			}
		} else {
			stable = 0
		}
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("waiting for distributed source cleanup (handoffs=%d monitors=%d collection=%t): %w", r.handoffCount.Load(), r.monitorCount.Load(), r.collectActive.Load(), ctx.Err())
		case <-timer.C:
		}
	}
}

func (r *distributedSourceRuntime) beginBackgroundSnapshotWork() func() {
	if r == nil {
		return func() {}
	}
	r.handoffGate.RLock()
	return r.handoffGate.RUnlock
}

// beginBackgroundSnapshotWorkUnless is beginBackgroundSnapshotWork for work
// that must stop waiting for the gate once abandon reports true; it then
// returns false.
func (r *distributedSourceRuntime) beginBackgroundSnapshotWorkUnless(abandon func() bool) (func(), bool) {
	if r == nil {
		return func() {}, true
	}
	for !r.handoffGate.TryRLock() {
		if abandon() {
			return nil, false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return r.handoffGate.RUnlock, true
}

// sourceTransactionTerminalAction resolves handoff liveness from local Raft
// authority when the target peer is unavailable. Family removal may retire a
// transaction before a background handoff next contacts the target; retrying
// that dead endpoint forever would pin both source memory and daemon shutdown.
func (r *distributedSourceRuntime) sourceTransactionTerminalAction(transactionID string) (string, bool) {
	if r == nil || r.transactions == nil || transactionID == "" {
		return "", false
	}
	tx, err := r.transactions.Load(transactionID)
	return terminalSourceAction(tx, err)
}

func terminalSourceAction(tx *cluster.Transaction, err error) (string, bool) {
	if errors.Is(err, os.ErrNotExist) {
		return "distributed-cancel", true
	}
	if err != nil || tx == nil {
		return "", false
	}
	switch tx.State {
	case cluster.TransactionIndependent:
		return "distributed-complete", true
	case cluster.TransactionCanceled, cluster.TransactionFailed:
		return distributedSourceTerminalCommand(tx), true
	default:
		return "", false
	}
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

// tryBeginForegroundCapture is beginForegroundCapture, if the gate is free.
func (r *distributedSourceRuntime) tryBeginForegroundCapture() (func(), bool) {
	if r == nil {
		return func() {}, true
	}
	if !r.handoffGate.TryLock() {
		return nil, false
	}
	return r.handoffGate.Unlock, true
}

// beginForegroundCapture takes the snapshot handoff gate for a capture of a
// branch of info's family, as distributedSourceRuntime.beginForegroundCapture
// does. A command that holds the gate may wait for the lifecycle lock, as a
// dehydration that streamed its memory without the lock waits to take it
// again; so a command that finds the gate taken waits for it without the
// lock, as the long operation op.
func (b *runtimeBackend) beginForegroundCapture(info ttcbundle.ContainerInfo, op longOperation) (func(), error) {
	if finish, ok := b.distributedSource.tryBeginForegroundCapture(); ok {
		return finish, nil
	}
	var finish func()
	if err := b.withoutLifecycleLock(info, op, func() error {
		finish = b.distributedSource.beginForegroundCapture()
		return nil
	}); err != nil {
		if finish != nil {
			finish()
		}
		return nil, err
	}
	return finish, nil
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
	// The node's memory is what its last heartbeat measured, a few seconds
	// ago at most: this renewal is for the lease.
	renewed, err := b.distributedSource.directory.Heartbeat(record.NodeID, record.Incarnation, record.MemoryCurrentBytes, time.Now(), lease)
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
	consoleLogs          map[string][]remoteConsoleLogExtent
	artifactToken        [16]byte
	artifacts            map[string][]byte
}

// remoteConsoleLogExtent pins one console-log generation at the exact length
// observed during capture. Keeping the file open makes the handoff independent
// of subsequent log rotation or unlink, while the length fence prevents a live
// session from extending the supposedly immutable captured prefix.
type remoteConsoleLogExtent struct {
	file *os.File
	size int64
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
	if magic == [4]byte{'T', 'T', 'C', 'A'} {
		r.serveArtifact(tcp)
		return
	}
	file, err := tcp.File()
	tcp.Close()
	if err != nil {
		return
	}
	r.serveFile(file)
}

// ConfigureArtifacts exposes immutable transaction metadata over the peer
// relay. These small control artifacts must travel through the same fenced
// channel as lazy memory and filesystem data: publishing a temporary NFS file
// and immediately naming it to another client is not coherently visible on all
// supported shared filesystems.
func (r *distributedPageRelay) ConfigureArtifacts(token [16]byte, artifacts map[string][]byte) error {
	if len(artifacts) == 0 {
		return errors.New("distributed artifact set is empty")
	}
	owned := make(map[string][]byte, len(artifacts))
	for name, payload := range artifacts {
		if name == "" || len(name) > 4096 || strings.ContainsAny(name, "/\\") || len(payload) == 0 {
			return fmt.Errorf("invalid distributed artifact %q", name)
		}
		owned[name] = payload
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || len(r.artifacts) != 0 {
		return errors.New("distributed artifact relay is closed or already configured")
	}
	r.artifactToken = token
	r.artifacts = owned
	return nil
}

func (r *distributedPageRelay) serveArtifact(conn *net.TCPConn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	magic := make([]byte, 4)
	if _, err := io.ReadFull(reader, magic); err != nil || string(magic) != "TTCA" {
		return
	}
	var token [16]byte
	if _, err := io.ReadFull(reader, token[:]); err != nil {
		return
	}
	var nameLength uint16
	if err := binary.Read(reader, binary.BigEndian, &nameLength); err != nil || nameLength == 0 || nameLength > 4096 {
		return
	}
	name := make([]byte, nameLength)
	if _, err := io.ReadFull(reader, name); err != nil {
		return
	}
	r.mu.Lock()
	payload, exists := r.artifacts[string(name)]
	authorized := token == r.artifactToken && exists && !r.closed
	r.mu.Unlock()
	if !authorized {
		_, _ = conn.Write([]byte{0})
		return
	}
	if _, err := conn.Write([]byte{1}); err != nil {
		return
	}
	if err := binary.Write(conn, binary.BigEndian, uint64(len(payload))); err != nil {
		return
	}
	_, _ = conn.Write(payload)
}

// fetchRemoteArtifact copies one immutable peer artifact into a seekable
// anonymous file. expected is part of the transaction descriptor and fences
// truncation, substitution, and unbounded peer responses without introducing
// content hashing into the trusted-cluster data path.
func fetchRemoteArtifact(ctx context.Context, address string, token [16]byte, name string, expected uint64) (*os.File, error) {
	if name == "" || len(name) > 4096 || strings.ContainsAny(name, "/\\") || expected == 0 || expected > math.MaxInt64 {
		return nil, errors.New("invalid remote artifact descriptor")
	}
	conn, err := (&net.Dialer{Timeout: 30 * time.Second}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	var request bytes.Buffer
	request.WriteString("TTCA")
	request.Write(token[:])
	if err := binary.Write(&request, binary.BigEndian, uint16(len(name))); err != nil {
		return nil, err
	}
	request.WriteString(name)
	if _, err := conn.Write(request.Bytes()); err != nil {
		return nil, err
	}
	status := []byte{0}
	if _, err := io.ReadFull(conn, status); err != nil {
		return nil, err
	}
	if status[0] != 1 {
		return nil, fmt.Errorf("remote artifact %q is unavailable", name)
	}
	var size uint64
	if err := binary.Read(conn, binary.BigEndian, &size); err != nil {
		return nil, err
	}
	if size != expected {
		return nil, fmt.Errorf("remote artifact %q size is %d, want %d", name, size, expected)
	}
	file, err := newConfigMemFD("ttc-distributed-"+name, nil)
	if err != nil {
		return nil, err
	}
	if _, err := io.CopyN(file, conn, int64(size)); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// ConfigureConsoleLogs exposes capture-fenced log generations over the same
// authenticated point-to-point listener as memory and filesystem paging.
// Ownership of every open extent transfers to the relay on success.
func (r *distributedPageRelay) ConfigureConsoleLogs(token [16]byte, logs map[string][]remoteConsoleLogExtent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || len(r.consoleLogs) != 0 {
		return errors.New("distributed console relay is closed or already configured")
	}
	r.consoleToken = token
	r.consoleLogs = make(map[string][]remoteConsoleLogExtent, len(logs))
	for id, extents := range logs {
		r.consoleLogs[id] = append([]remoteConsoleLogExtent(nil), extents...)
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
	extents := append([]remoteConsoleLogExtent(nil), r.consoleLogs[string(idBytes)]...)
	authorized := token == r.consoleToken && len(extents) != 0 && !r.closed
	r.mu.Unlock()
	if !authorized {
		return
	}
	var size uint64
	for _, extent := range extents {
		if extent.file == nil || extent.size < 0 {
			return
		}
		if uint64(extent.size) > math.MaxUint64-size {
			return
		}
		size += uint64(extent.size)
	}
	if err := binary.Write(conn, binary.BigEndian, size); err != nil {
		return
	}
	for _, extent := range extents {
		if _, err := io.Copy(conn, io.NewSectionReader(extent.file, 0, extent.size)); err != nil {
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
	if _, err := os.Stat(destination); err != nil && !os.IsNotExist(err) {
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
	if err := reconcileRemoteConsoleLog(destination, temporary); err != nil {
		return err
	}
	return nil
}

// reconcileRemoteConsoleLog installs one immutable-at-capture remote output
// stream without discarding output retained from an earlier residence on this
// node. A migration can return to a node before its previous local cache has
// been reclaimed. Byte length alone cannot identify that cache: fixed-size
// terminal records frequently give two disjoint histories the same size.
func reconcileRemoteConsoleLog(destination, remote string) error {
	if _, err := os.Stat(destination); err != nil {
		if os.IsNotExist(err) {
			return os.Rename(remote, destination)
		}
		return err
	}
	destinationPrefixesRemote, err := fileContentPrefix(destination, remote)
	if err != nil {
		return err
	}
	if destinationPrefixesRemote {
		return os.Rename(remote, destination)
	}
	remotePrefixesDestination, err := fileContentPrefix(remote, destination)
	if err != nil {
		return err
	}
	if remotePrefixesDestination {
		return os.Remove(remote)
	}
	// The target cache is from the branch's previous residence; the currently
	// authoritative source segment happened later. Preserve that ordering.
	merged := remote + ".merged"
	_ = os.Remove(merged)
	out, err := os.OpenFile(merged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	copyFile := func(path string) error {
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(out, in)
		return errors.Join(copyErr, in.Close())
	}
	if err := copyFile(destination); err != nil {
		out.Close()
		_ = os.Remove(merged)
		return err
	}
	if err := copyFile(remote); err != nil {
		out.Close()
		_ = os.Remove(merged)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		_ = os.Remove(merged)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(merged)
		return err
	}
	if err := os.Rename(merged, destination); err != nil {
		_ = os.Remove(merged)
		return err
	}
	return os.Remove(remote)
}

func fileContentPrefix(prefixPath, completePath string) (bool, error) {
	prefixInfo, err := os.Stat(prefixPath)
	if err != nil {
		return false, err
	}
	completeInfo, err := os.Stat(completePath)
	if err != nil {
		return false, err
	}
	if prefixInfo.Size() > completeInfo.Size() {
		return false, nil
	}
	prefix, err := os.Open(prefixPath)
	if err != nil {
		return false, err
	}
	defer prefix.Close()
	complete, err := os.Open(completePath)
	if err != nil {
		return false, err
	}
	defer complete.Close()
	left, right := make([]byte, 64<<10), make([]byte, 64<<10)
	remaining := prefixInfo.Size()
	for remaining != 0 {
		length := int64(len(left))
		if length > remaining {
			length = remaining
		}
		if _, err := io.ReadFull(prefix, left[:length]); err != nil {
			return false, err
		}
		if _, err := io.ReadFull(complete, right[:length]); err != nil {
			return false, err
		}
		if !bytes.Equal(left[:length], right[:length]) {
			return false, nil
		}
		remaining -= length
	}
	return true, nil
}

func closeRemoteConsoleLogExtents(logs map[string][]remoteConsoleLogExtent) error {
	var result error
	for _, extents := range logs {
		for _, extent := range extents {
			if extent.file != nil {
				result = errors.Join(result, extent.file.Close())
			}
		}
	}
	return result
}

func (b *runtimeBackend) captureRemoteExecConsoleLogs(container, branch string, sessions []boot.ExecSessionInfo) ([]cluster.RemoteConsoleLogDescriptor, map[string][]remoteConsoleLogExtent, error) {
	_, maxFiles, err := b.consoleRetention(container)
	if err != nil {
		return nil, nil, err
	}
	store := b.containerStore()
	descriptors := make([]cluster.RemoteConsoleLogDescriptor, 0, len(sessions))
	files := make(map[string][]remoteConsoleLogExtent)
	fail := func(err error) ([]cluster.RemoteConsoleLogDescriptor, map[string][]remoteConsoleLogExtent, error) {
		return nil, nil, errors.Join(err, closeRemoteConsoleLogExtents(files))
	}
	for _, session := range sessions {
		paths, err := store.ExecConsolePaths(container, branch, session.ID)
		if err != nil {
			return fail(err)
		}
		generations, err := listConsoleLogGenerations(paths.Log, maxFiles)
		if err != nil {
			return fail(err)
		}
		var size uint64
		extents := make([]remoteConsoleLogExtent, 0, len(generations))
		for _, generation := range generations {
			file, err := os.Open(generation.path)
			if err != nil {
				return fail(err)
			}
			info, err := file.Stat()
			if err != nil {
				file.Close()
				return fail(err)
			}
			if info.Size() < 0 {
				file.Close()
				return fail(fmt.Errorf("exec console log %q has a negative size", session.ID))
			}
			if uint64(info.Size()) > math.MaxUint64-size {
				file.Close()
				return fail(fmt.Errorf("exec console log %q is too large", session.ID))
			}
			size += uint64(info.Size())
			extents = append(extents, remoteConsoleLogExtent{file: file, size: info.Size()})
			files[session.ID] = extents
		}
		if size == 0 {
			_ = closeRemoteConsoleLogExtents(map[string][]remoteConsoleLogExtent{session.ID: extents})
			delete(files, session.ID)
			continue
		}
		descriptors = append(descriptors, cluster.RemoteConsoleLogDescriptor{ExecID: session.ID, Size: size})
		files[session.ID] = extents
	}
	return descriptors, files, nil
}

// configureRemoteExecConsoleLogs captures the host-side portion of managed
// exec state and attaches it to the same fenced relay as the Sentry snapshot.
// Every remote fork/migration path must call this before preparing its target.
func (b *runtimeBackend) configureRemoteExecConsoleLogs(relay *distributedPageRelay, token [16]byte, container, branch string, sessions []boot.ExecSessionInfo) ([]cluster.RemoteConsoleLogDescriptor, error) {
	descriptors, files, err := b.captureRemoteExecConsoleLogs(container, branch, sessions)
	if err != nil {
		return nil, err
	}
	if len(descriptors) == 0 {
		return nil, closeRemoteConsoleLogExtents(files)
	}
	if err := relay.ConfigureConsoleLogs(token, files); err != nil {
		return nil, errors.Join(err, closeRemoteConsoleLogExtents(files))
	}
	return descriptors, nil
}

// applyColdFilesystemRestoreIDs preserves the repository identities encoded in
// a process-free kernel snapshot. A cold branch may be materialized into a new
// node-local repository before it is forked remotely; that cache repository
// provides the graph objects, but the saved filesystem still names the
// repository that was active at dehydration time.
func applyColdFilesystemRestoreIDs(roots []cluster.FilesystemRootDescriptor, filesystems []coldFilesystemSnapshot) error {
	bySource := make(map[string]string, len(filesystems))
	for _, filesystem := range filesystems {
		if filesystem.Root.Source == "" || filesystem.Root.RestoreID == "" {
			return errors.New("cold filesystem snapshot omits source or restore identity")
		}
		if _, ok := bySource[filesystem.Root.Source]; ok {
			return fmt.Errorf("cold filesystem snapshot repeats source %q", filesystem.Root.Source)
		}
		bySource[filesystem.Root.Source] = filesystem.Root.RestoreID
	}
	for i := range roots {
		restoreID, ok := bySource[roots[i].Source]
		if !ok {
			return fmt.Errorf("cold filesystem snapshot omits repository %q", roots[i].Source)
		}
		roots[i].RestoreID = restoreID
		delete(bySource, roots[i].Source)
	}
	if len(bySource) != 0 {
		return fmt.Errorf("cold filesystem snapshot has %d unmatched repositories", len(bySource))
	}
	return nil
}

// applyDehydratedFilesystemRestoreIDs binds a materialized cold cache back to
// the repository identities serialized in its kernel snapshot. A freshly
// dehydrated branch that is still publishing has no shared manifest yet and
// still uses the exact repositories from which it was captured; in that case
// prepareDistributedFilesystem has already recorded the correct identities.
func applyDehydratedFilesystemRestoreIDs(roots []cluster.FilesystemRootDescriptor, manifestPath string) error {
	if manifestPath == "" {
		return nil
	}
	manifest, err := readColdBranchManifest(manifestPath)
	if err != nil {
		return err
	}
	return applyColdFilesystemRestoreIDs(roots, manifest.Filesystems)
}

// installRemoteExecSessionStates restores the host-side completion markers for
// retained exec sessions. The Sentry owns the authoritative session metadata,
// but attach/log readers use these files to avoid starting a console follower
// for a process that had already exited before migration.
func (b *runtimeBackend) installRemoteExecSessionStates(container, branch string, sessions []cluster.RemoteExecSessionDescriptor) error {
	store := b.containerStore()
	for _, session := range sessions {
		if session.State != string(boot.ExecSessionExited) {
			continue
		}
		if session.ExitStatus == nil {
			return fmt.Errorf("exited exec session %q has no wait status", session.ID)
		}
		paths, err := store.ExecConsolePaths(container, branch, session.ID)
		if err != nil {
			return err
		}
		if err := installRetainedConsoleWaitStatus(paths, *session.ExitStatus); err != nil {
			return fmt.Errorf("restoring wait status for exec session %q: %w", session.ID, err)
		}
	}
	return nil
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
	command := runscCommand(r.backend.runsc, append(r.backend.runtimeControlArgs(),
		"timetravel", "--distributed-connection-fd=3",
		r.source, action, r.session)...)
	command.ExtraFiles = []*os.File{file}
	command.Stdin = strings.NewReader("")
	command.Stdout = io.Discard
	var commandError bytes.Buffer
	command.Stderr = &commandError
	setTTCCommandParentDeathSignal(command)
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
			r.artifacts = nil
			consoleLogs := r.consoleLogs
			r.consoleLogs = nil
			r.mu.Unlock()
			if cleanup != nil {
				cleanup()
			}
			_ = closeRemoteConsoleLogExtents(consoleLogs)
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
func (b *runtimeBackend) prepareDistributedFilesystem(info ttcbundle.ContainerInfo, filesystemBranch, preserveDiffBaseBranch string, relay *distributedPageRelay) ([]cluster.FilesystemRootDescriptor, uint64, error) {
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
		// A migration uses an internal fork only to freeze the transfer view; it
		// does not create a new user-visible branch or parent edge. Preserve the
		// visible source's existing diff base instead of treating the migration
		// instant as a new parent. Ordinary remote forks leave this empty and use
		// the captured child's fork-point base.
		if describeErr == nil && preserveDiffBaseBranch != "" {
			var sourceDescriptor ttcvolume.RemoteSnapshotDescriptor
			sourceDescriptor, describeErr = repository.DescribeRemoteSnapshot(preserveDiffBaseBranch)
			if describeErr == nil {
				descriptor.BaseRoot = sourceDescriptor.BaseRoot
			}
		}
		closeErr := repository.Close()
		if describeErr != nil || closeErr != nil {
			return nil, 0, errors.Join(describeErr, closeErr)
		}
		snapshot := descriptor.Snapshot
		if snapshot.LogicalBytes > ^uint64(0)-dependentBytes {
			return nil, 0, errors.New("distributed filesystem dependency byte count overflow")
		}
		dependentBytes += snapshot.LogicalBytes
		roots = append(roots, cluster.FilesystemRootDescriptor{
			Source: config.Source, RepositoryID: snapshot.RepositoryID,
			// A live capture serializes the identity of the repository object that
			// is currently mounted in the Sentry. Use that exact physical identity
			// for the target restore. config.RestoreID may describe any earlier
			// import hop and is therefore neither authoritative nor sufficient
			// after repeated migration or cold rehydration.
			RestoreID: snapshot.RepositoryID,
			BranchID:  snapshot.BranchID, Root: descriptor.Root.String(), BaseRoot: descriptor.BaseRootString(),
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
		descriptor, err := ttcvolume.NewRemoteSnapshotDescriptor(root.RepositoryID, root.BranchID, root.Root, root.BaseRoot, root.NextInode, root.LogicalBytes)
		if err != nil {
			return 0, fmt.Errorf("decoding repository %q graph root: %w", config.Source, err)
		}
		if peer {
			// An imported peer must join the repository already mounted by its
			// physical runtime. If this is the first materialization for the source,
			// use the same container-private deterministic path as a new target.
			// Never create a private replica at filesystems/volumes/<logical-id>:
			// that directory is the managed-volume catalog and requires its on-disk
			// repository identity to equal the directory name.
			repositoryPath := config.Path
			if _, statErr := os.Stat(filepath.Join(repositoryPath, "volume.json")); errors.Is(statErr, os.ErrNotExist) {
				repositoryPath = distributedRepositoryCachePath(b.activeDataRoot(), info.ID, config.Source)
			} else if statErr != nil {
				return 0, statErr
			}
			repository, created, err := importRemoteRepositoryBranch(manager, repositoryPath, ttcvolume.CopyMode(config.CopyMode), "peer-"+fmt.Sprint(index), descriptor, filesystemBranch, origins)
			if err != nil {
				return 0, fmt.Errorf("importing peer repository %q metadata: %w", config.Source, err)
			}
			if _, err := repository.Branch(filesystemBranch); err != nil {
				_ = repository.Close()
				return 0, fmt.Errorf("verifying imported peer repository %q branch %q: %w", config.Source, filesystemBranch, err)
			}
			config.Path = repositoryPath
			config.Branch = filesystemBranch
			config.CopyMode = string(manager.Capability().Mode)
			config.RestoreID = root.RestoreID
			config.Private = true
			if err := repository.Close(); err != nil {
				return 0, err
			}
			imported = append(imported, importedFilesystem{path: repositoryPath, copyMode: ttcvolume.CopyMode(config.CopyMode), created: created})
			installed++
			continue
		}
		destination := distributedRepositoryCachePath(b.activeDataRoot(), info.ID, config.Source)
		repository, created, err := importRemoteRepositoryBranch(manager, destination, manager.Capability().Mode, "remote-"+fmt.Sprint(index), descriptor, filesystemBranch, origins)
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

// importRemoteRepositoryBranch imports descriptor as branch of the repository
// at path, or, if there is none, of a new private one named name, with the
// Raft-allocated origins if there are any. An import that would share a
// snapshot frozen for an authority handoff, as the branch's own is until the
// provider that published it completes the handoff, is refused and changes
// nothing: it waits the handoff out.
func importRemoteRepositoryBranch(manager *ttcvolume.Manager, path string, mode ttcvolume.CopyMode, name string, descriptor ttcvolume.RemoteSnapshotDescriptor, branch string, origins []ttcsnapshot.SnapshotID) (repository *ttcvolume.Repository, created bool, err error) {
	err = throughAuthorityHandoffs(func() error {
		if _, statErr := os.Stat(filepath.Join(path, "volume.json")); errors.Is(statErr, os.ErrNotExist) {
			created = true
			var err error
			if len(origins) == 0 {
				repository, err = manager.ImportPrivateRemoteDescriptor(path, name, descriptor, branch)
			} else {
				repository, err = manager.ImportPrivateRemoteDescriptorWithOrigins(path, name, descriptor, branch, origins[0], origins[1])
			}
			return err
		} else if statErr != nil {
			return statErr
		}
		created = false
		opened, err := ttcvolume.OpenRepository(path, mode)
		if err != nil {
			return err
		}
		if len(origins) == 0 {
			err = opened.ImportRemoteBranchDescriptor(descriptor, branch)
		} else {
			err = opened.ImportRemoteBranchDescriptorWithOrigins(descriptor, branch, origins[0], origins[1])
		}
		if err != nil {
			return errors.Join(err, opened.Close())
		}
		repository = opened
		return nil
	})
	return repository, created, err
}

func distributedRepositoryCachePath(activeRoot, containerID, source string) string {
	key := sha256.Sum256([]byte(source))
	return filepath.Join(activeRoot, "containers", containerID, "repositories", "remote", hex.EncodeToString(key[:]))
}

// exportDistributedGraph publishes the fork-time memory retained by session
// into a graph-native snapshot origin. The Sentry writes a bounded capture
// stream directly to the daemon; no compatibility repository is created.
func (b *runtimeBackend) exportDistributedGraph(containerID, transactionID string) (_ cluster.GraphHandoffDescriptor, rootNames, rootObjects []string, retErr error) {
	if b.snapshotGraph == nil || b.snapshotData == nil || b.snapshotShared == nil || b.distributedSource == nil || b.distributedSource.authority == nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, errors.New("distributed graph publication is unavailable")
	}
	origin, sealState, built, err := b.allocateGraphSnapshotOrigin()
	if err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	defer built()
	shared, ledgerFrozen := false, false
	manifestRelative := filepath.Join("cluster", "memory-manifests", transactionID+".manifest")
	manifestPath := filepath.Join(b.dataRoot, manifestRelative)
	defer func() {
		if retErr == nil {
			return
		}
		_ = os.Remove(manifestPath)
		if shared {
			_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
		} else if !ledgerFrozen {
			_ = removeSharedPublicationBridgeIntent(b.dataRoot, origin)
			_ = b.releaseGraphMemoryRoots(rootNames)
		}
		if ledgerFrozen {
			// The handoff cannot be canceled once frozen; complete it, which
			// releases the capture's roots, rather than discard the capture.
			b.graphOriginPublisher().rollForwardFrozenOrigin(origin)
			return
		}
		if err := b.discardFailedGraphSnapshot(origin); err != nil && b.stderr != nil {
			fmt.Fprintf(b.stderr, "ttc: warning: discarding failed live handoff snapshot %d: %v\n", origin, err)
		}
	}()

	reader, writer, err := graphCaptureStream()
	if err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
	}
	// Closing the daemon's end fails the Sentry's writes, which ends the
	// export and frees the source's session for its removal.
	var aborted atomic.Bool
	b.distributedSource.exports.Store(transactionID, func() {
		aborted.Store(true)
		_ = reader.Close()
	})
	defer b.distributedSource.exports.Delete(transactionID)
	type importResult struct {
		capture ttcchunk.GraphImportResult
		err     error
	}
	imported := make(chan importResult, 1)
	prefix := fmt.Sprintf("remote/%d/%s", origin, transactionID)
	go func() {
		capture, importErr := b.importGraphCapture(reader, origin, prefix)
		_ = reader.Close()
		imported <- importResult{capture: capture, err: importErr}
	}()
	// Only the Sentry export competes with a foreground live capture. Shared
	// publication and reference-ledger work may take arbitrarily long on a slow
	// shared filesystem and must never hold the foreground-priority gate.
	finishBackgroundWork := b.distributedSource.beginBackgroundSnapshotWork()
	_, exportErr := b.commandWithFiles([]*os.File{writer}, "timetravel", "--dehydration-graph-stream-fd=3", "--dehydration-graph-stream-filtered", containerID, "distributed-export", transactionID)
	_ = writer.Close()
	finishBackgroundWork()
	outcome := <-imported
	// The Sentry command can fail after it has finished writing a valid stream
	// (for example, if its source branch is retired concurrently). Record the
	// imported roots before considering the command status so rollback can
	// release everything the importer committed.
	for _, root := range outcome.capture.Roots {
		rootNames = append(rootNames, root.Name)
		rootObjects = append(rootObjects, root.Root.String())
	}
	if aborted.Load() {
		return cluster.GraphHandoffDescriptor{}, nil, nil, errExportAbandoned
	}
	if outcome.err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("importing live graph memory: %w", outcome.err)
	}
	if len(rootObjects) == 0 {
		return cluster.GraphHandoffDescriptor{}, nil, nil, errors.New("live graph capture returned no roots")
	}
	// A complete imported capture is immutable even when the control command
	// reports an error after closing its stream. Seal it before considering the
	// command status. Rollback below can still reclaim an unreferenced capture,
	// while pages already referenced by a concurrent completed capture remain a
	// valid volatile origin that the dependent export can publish.
	if err := b.snapshotGraph.Seal(origin, sealState); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("sealing live graph snapshot %d: %w", origin, err)
	}
	if exportErr != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("streaming live graph memory: %w", exportErr)
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
	if err := b.publishGraphManifestDependencies(adapted.Manifest, origin); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("publishing live graph dependencies: %w", err)
	}
	if err := b.snapshotShared.PublishHandoff(context.Background(), b.snapshotGraph, origin, b.snapshotData); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("publishing live graph snapshot %d: %w", origin, err)
	}
	// Hold the origin through the final switch, as publishVolatileGraphOrigin
	// does: another publication of it, a dependency's or a roll-forward's,
	// then waits for this one rather than running beside it.
	unlockOrigin := b.distributedSource.lockSnapshotOrigin(origin)
	defer unlockOrigin()
	// Serialize only the short final ledger/authority switch against foreground
	// captures. The potentially slow shared-pack write above remains outside the
	// gate. Once a foreground writer owns the gate, it may add references to
	// this origin; the later freeze will include them in its baseline.
	finishFinalHandoff := b.distributedSource.beginBackgroundSnapshotWork()
	defer finishFinalHandoff()
	if err := b.snapshotGraph.FreezeSnapshotLedgerHandoff(origin); err != nil {
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
		sharedRoot: b.dataRoot,
		nodeID:     b.nodeID, incarnation: b.distributedSource.incarnation,
		authority: b.distributedSource.authority, graphShared: b.snapshotShared,
		sharedRefs: b.sharedReferences, peers: b.distributedSource,
	}
	bridgeIntent := sharedPublicationBridgeIntent{Kind: sharedPublicationBridgeTransaction, Transaction: transactionID}
	if err := b.retryGraphReferenceMutation(func(context.Context) error {
		// Publication is immutable and every ledger operation is idempotent, so
		// replaying the complete handoff is safe across coordinator election and
		// lease turnover. A daemon restart commonly resumes source consolidation
		// before the new shared-reference coordinator has acquired its lease.
		return publisher.publishSharedReferenceLedger(origin, changes, bridgeIntent)
	}); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, fmt.Errorf("publishing live graph reference ledger: %w", err)
	}
	references, _ := b.snapshotGraph.OriginReferences(origin)
	if err := publisher.keepProviderReferences(origin, "", references); err != nil {
		return cluster.GraphHandoffDescriptor{}, nil, nil, err
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

// publishGraphManifestDependencies makes every immutable chunk named by a
// flattened restore manifest independently readable before its new root is
// advertised. An interrupted export may leave a later capture rooted in a new
// snapshot while retaining chunk identities from the earlier sealed origin.
// Publishing only the new root would create a durable-looking handoff whose
// first access to an old chunk fails after the provider exits.
func (b *runtimeBackend) publishGraphManifestDependencies(manifest ttcchunk.Manifest, root ttcsnapshot.SnapshotID) error {
	chunks, err := ttcchunk.GraphChunkObjects(manifest)
	if err != nil {
		return err
	}
	origins := make(map[ttcsnapshot.SnapshotID]struct{})
	for _, object := range chunks {
		if object.Snapshot != root {
			origins[object.Snapshot] = struct{}{}
		}
	}
	ordered := make([]ttcsnapshot.SnapshotID, 0, len(origins))
	for origin := range origins {
		ordered = append(ordered, origin)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, origin := range ordered {
		provider, volatile, err := b.distributedSource.authority.SnapshotProvider(uint64(origin))
		if err != nil {
			return fmt.Errorf("resolving chunk origin %d: %w", origin, err)
		}
		if !volatile {
			if err := b.snapshotShared.CheckManifest(origin); err != nil {
				return fmt.Errorf("shared chunk origin %d has no pack: %w", origin, err)
			}
			continue
		}
		if provider.NodeID != b.nodeID {
			return fmt.Errorf("chunk origin %d remains volatile on node %q", origin, provider.NodeID)
		}
		if err := b.publishVolatileGraphOrigin(origin); err != nil {
			return fmt.Errorf("publishing chunk origin %d: %w", origin, err)
		}
	}
	return nil
}

// publishMigratedMemoryOrigins makes immutable backing named by a migrating
// branch independent of this node. A live stream drain copies all bytes into
// the target's MemoryFile, but mapped PMAs intentionally retain immutable file
// identity so a later fork/dehydration can preserve CoW edges. Those objects
// must therefore move to shared authority before their provider node retires.
func (b *runtimeBackend) publishMigratedMemoryOrigins(branch ttcbundle.BranchInfo) error {
	if b == nil || b.distributedSource == nil || b.distributedSource.authority == nil {
		return nil
	}
	// Use the graph's current bindings rather than the branch record's captured
	// object list. Source cleanup is intentionally restartable: a prior attempt
	// may have committed root release and then failed while removing filesystem
	// artifacts. In that state the durable branch record still carries its old
	// diagnostic object IDs, but there is no longer a provider root to publish.
	// Treating those IDs as live made every retry fail on the origin tombstone.
	origins := make(map[ttcsnapshot.SnapshotID]struct{})
	for _, root := range branchGraphRootNames(branch) {
		id, ok := b.snapshotGraph.Root(root)
		if !ok {
			continue
		}
		provider, volatile, err := b.distributedSource.authority.SnapshotProvider(uint64(id.Snapshot))
		if err != nil {
			return fmt.Errorf("resolving snapshot origin %d: %w", id.Snapshot, err)
		}
		if volatile && provider.NodeID == b.nodeID {
			origins[id.Snapshot] = struct{}{}
		}
	}
	ordered := make([]ttcsnapshot.SnapshotID, 0, len(origins))
	for origin := range origins {
		ordered = append(ordered, origin)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	for _, origin := range ordered {
		if err := b.publishVolatileGraphOrigin(origin); err != nil {
			return err
		}
	}
	return nil
}

// publishVolatileGraphOrigin performs the provider-to-shared authority handoff
// for an existing immutable origin. Bulk pack creation happens before the
// short ledger freeze; Raft changes routing only after the shared coordinator
// has imported the exact provider baseline and catch-up suffix.
func (b *runtimeBackend) publishVolatileGraphOrigin(origin ttcsnapshot.SnapshotID) error {
	return b.graphOriginPublisher().publishVolatileGraphOrigin(context.Background(), origin)
}

// graphOriginPublisher returns the publisher of this node's memory origins.
func (b *runtimeBackend) graphOriginPublisher() *branchPublisher {
	return &branchPublisher{
		sharedRoot: b.dataRoot,
		nodeID:     b.nodeID, incarnation: b.distributedSource.incarnation,
		authority: b.distributedSource.authority, graphShared: b.snapshotShared,
		graph: b.snapshotGraph, graphData: b.snapshotData,
		sharedRefs: b.sharedReferences, peers: b.distributedSource,
	}
}

// resumeFrozenOriginPublications rolls forward, in the background, the
// handoffs of this node's memory and filesystem origins that a crash left
// frozen.
func resumeFrozenOriginPublications(b *runtimeBackend) {
	if b == nil || b.snapshotGraph == nil || b.distributedSource == nil || b.distributedSource.authority == nil {
		return
	}
	publisher := b.graphOriginPublisher()
	for _, origin := range b.snapshotGraph.FrozenSnapshotLedgers() {
		publisher.rollForwardFrozenOrigin(origin)
	}
	// Finding the filesystem origins opens every repository.
	go func() {
		origins, err := b.containerStore().FrozenFilesystemOrigins()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ttcd: finding frozen filesystem origin handoffs: %v\n", err)
			return
		}
		for _, origin := range origins {
			b.rollForwardFrozenFilesystemOrigin(origin)
		}
	}()
}

// publishVolatileFilesystemOrigin transfers one immutable filesystem origin
// from this node's repository graph to shared authority. This is the
// filesystem counterpart of publishVolatileGraphOrigin and is intentionally
// origin-scoped: graceful node retirement must cover immutable origins kept
// alive by remote descendants even when no current local branch names them.
func (b *runtimeBackend) publishVolatileFilesystemOrigin(origin ttcsnapshot.SnapshotID) error {
	return b.publishVolatileFilesystemOriginContext(context.Background(), origin, make(map[ttcsnapshot.SnapshotID]struct{}))
}

func (b *runtimeBackend) publishVolatileFilesystemOriginContext(ctx context.Context, origin ttcsnapshot.SnapshotID, visiting map[ttcsnapshot.SnapshotID]struct{}) error {
	if origin == 0 || b.snapshotShared == nil || b.distributedSource == nil ||
		b.distributedSource.authority == nil || b.sharedReferences == nil {
		return errors.New("distributed filesystem publication is unavailable")
	}
	if _, cycle := visiting[origin]; cycle {
		return fmt.Errorf("filesystem snapshot origin dependency cycle through %d", origin)
	}
	provider, volatile, err := b.distributedSource.authority.SnapshotProvider(uint64(origin))
	if err != nil {
		return err
	}
	if !volatile {
		if err := b.snapshotShared.CheckManifest(origin); err != nil {
			return fmt.Errorf("filesystem origin %d has neither volatile nor shared authority: %w", origin, err)
		}
		return nil
	}
	if provider.NodeID != b.nodeID {
		return fmt.Errorf("filesystem origin %d belongs to node %q, not %q", origin, provider.NodeID, b.nodeID)
	}

	// Shared-ledger import installs cross-origin child edges immediately. Make
	// every referenced origin authoritative in shared storage first, regardless
	// of numeric snapshot ID; filesystem path-copy may legitimately create an
	// older-origin metadata object that selects data from a newer origin.
	var dependencies []ttcsnapshot.SnapshotID
	found, err := b.containerStore().WithFilesystemSnapshotOrigin(origin, func(repository *ttcvolume.Repository) error {
		var dependencyErr error
		dependencies, dependencyErr = repository.OriginDependencies(origin)
		return dependencyErr
	})
	if err != nil {
		return fmt.Errorf("inventorying filesystem origin %d dependencies: %w", origin, err)
	}
	if !found {
		return fmt.Errorf("filesystem origin %d is absent from node-local repositories", origin)
	}
	visiting[origin] = struct{}{}
	defer delete(visiting, origin)
	for _, dependency := range dependencies {
		dependencyProvider, dependencyVolatile, err := b.distributedSource.authority.SnapshotProvider(uint64(dependency))
		if err != nil {
			return fmt.Errorf("resolving filesystem origin %d dependency %d: %w", origin, dependency, err)
		}
		if !dependencyVolatile {
			if err := b.snapshotShared.CheckManifest(dependency); err != nil {
				return fmt.Errorf("filesystem dependency %d has shared authority without a manifest: %w", dependency, err)
			}
			continue
		}
		if dependencyProvider.NodeID == b.nodeID {
			if err := b.publishVolatileFilesystemOriginContext(ctx, dependency, visiting); err != nil {
				return fmt.Errorf("publishing backing filesystem origin %d for origin %d: %w", dependency, origin, err)
			}
			continue
		}
		peer, err := b.distributedSource.peer(dependencyProvider.NodeID, false)
		if err != nil {
			return fmt.Errorf("routing filesystem dependency %d to provider %q: %w", dependency, dependencyProvider.NodeID, err)
		}
		if err := peer.PublishSnapshot(ctx, uint64(dependency)); err != nil {
			return fmt.Errorf("requesting filesystem dependency %d from node %q: %w", dependency, dependencyProvider.NodeID, err)
		}
	}
	// Dependency publication can take long enough for an idempotent concurrent
	// publisher to finish this origin too.
	provider, volatile, err = b.distributedSource.authority.SnapshotProvider(uint64(origin))
	if err != nil {
		return err
	}
	if !volatile {
		err := b.snapshotShared.CheckManifest(origin)
		return err
	}
	if provider.NodeID != b.nodeID {
		return fmt.Errorf("filesystem origin %d provider changed to node %q", origin, provider.NodeID)
	}
	// Pack construction for dependencies is intentionally outside the origin
	// critical section. Serialize the final provider ledger/authority handoff
	// with cancellation and orphan reconciliation, then revalidate authority:
	// another idempotent publisher may have completed while dependencies were
	// being made durable.
	unlockOrigin := b.distributedSource.lockSnapshotOrigin(origin)
	defer unlockOrigin()
	provider, volatile, err = b.distributedSource.authority.SnapshotProvider(uint64(origin))
	if err != nil {
		return err
	}
	if !volatile {
		err := b.snapshotShared.CheckManifest(origin)
		return err
	}
	if provider.NodeID != b.nodeID {
		return fmt.Errorf("filesystem origin %d provider changed to node %q", origin, provider.NodeID)
	}

	publisher := &branchPublisher{
		sharedRoot: b.dataRoot,
		nodeID:     b.nodeID, incarnation: b.distributedSource.incarnation,
		authority: b.distributedSource.authority, graphShared: b.snapshotShared,
		sharedRefs: b.sharedReferences, peers: b.distributedSource,
	}
	bridgeIntent := sharedPublicationBridgeIntent{Kind: sharedPublicationBridgeOrigin}
	found, err = b.containerStore().WithFilesystemSnapshotOrigin(origin, func(repository *ttcvolume.Repository) error {
		empty, err := repository.OriginEmpty(origin)
		if err != nil {
			return err
		}
		if empty {
			busy, err := b.snapshotShared.DiscardHandoff(origin)
			if err != nil {
				return err
			}
			if busy {
				return fmt.Errorf("filesystem origin %d handoff pack is busy", origin)
			}
			return b.distributedSource.authority.AbandonSnapshotOrigin(b.nodeID, b.distributedSource.incarnation, uint64(origin))
		}
		if err := repository.PublishOriginHandoff(context.Background(), b.snapshotShared, origin); err != nil {
			return fmt.Errorf("publishing filesystem origin %d: %w", origin, err)
		}
		changes, err := repository.FreezeOriginHandoff(origin)
		if err != nil {
			return fmt.Errorf("freezing filesystem origin %d reference handoff: %w", origin, err)
		}
		if err := publisher.publishSharedReferenceLedger(origin, changes, bridgeIntent); err != nil {
			return fmt.Errorf("transferring filesystem origin %d reference authority: %w", origin, err)
		}
		references, _ := repository.OriginReferences(origin)
		if err := publisher.keepProviderReferences(origin, repository.Path(), references); err != nil {
			return err
		}
		if err := b.distributedSource.authority.PublishSnapshotOrigin(b.nodeID, b.distributedSource.incarnation, uint64(origin)); err != nil {
			return fmt.Errorf("publishing filesystem origin %d authority: %w", origin, err)
		}
		if err := repository.CompleteOriginHandoff(origin); err != nil {
			return fmt.Errorf("completing filesystem origin %d handoff: %w", origin, err)
		}
		if err := publisher.releaseSharedPublicationBridge(origin); err != nil {
			return fmt.Errorf("releasing filesystem origin %d publication bridge: %w", origin, err)
		}
		return nil
	})
	if err != nil {
		// A handoff that failed after it froze cannot be canceled, and its
		// repository refuses writes meanwhile: complete it.
		b.rollForwardFrozenFilesystemOrigin(origin)
		return err
	}
	if !found {
		return fmt.Errorf("filesystem origin %d is absent from node-local repositories", origin)
	}
	return nil
}

// resumeFrozenFilesystemOrigin rolls forward the handoff of origin, a
// filesystem origin of this node's whose repository a failed or interrupted
// handoff left frozen, as resumeFrozenGraphOrigin does a memory origin's. A
// frozen repository refuses every metadata transaction, so its branches'
// writes wait for this. The handoff's pack exists, so the roll-forward starts
// at the freeze: PublishOriginHandoff would first sync metadata, which the
// freeze refuses.
func (b *runtimeBackend) resumeFrozenFilesystemOrigin(origin ttcsnapshot.SnapshotID) error {
	if origin == 0 || b.snapshotShared == nil || b.distributedSource == nil ||
		b.distributedSource.authority == nil || b.sharedReferences == nil {
		return errors.New("distributed filesystem publication is unavailable")
	}
	unlockOrigin := b.distributedSource.lockSnapshotOrigin(origin)
	defer unlockOrigin()
	// Only the exact retry of the operation that recorded a bridge may reuse
	// it. Without one recorded yet, the handoff proceeds as the origin's own.
	bridgeIntent, found, err := loadSharedPublicationBridgeIntent(b.dataRoot, origin)
	if err != nil {
		return err
	}
	if !found {
		bridgeIntent = sharedPublicationBridgeIntent{Kind: sharedPublicationBridgeOrigin}
	} else if bridgeIntent.Kind == sharedPublicationBridgePublication {
		return errFrozenOriginOwned
	}
	provider, volatile, terminal, err := snapshotOriginAuthority(b.distributedSource.authority, origin)
	if err != nil {
		return err
	}
	publisher := &branchPublisher{
		sharedRoot: b.dataRoot,
		nodeID:     b.nodeID, incarnation: b.distributedSource.incarnation,
		authority: b.distributedSource.authority, graphShared: b.snapshotShared,
		sharedRefs: b.sharedReferences, peers: b.distributedSource,
	}
	_, err = b.containerStore().WithFilesystemSnapshotOrigin(origin, func(repository *ttcvolume.Repository) error {
		if !repository.OriginHandoffFrozen(origin) {
			return nil
		}
		if terminal {
			// The origin was abandoned or retired: no handoff of it
			// completes, and no shared ledger counts its references any more.
			return repository.CancelOriginHandoff(origin)
		}
		if volatile && provider.NodeID != b.nodeID {
			return fmt.Errorf("filesystem origin %d is frozen here but provided by node %q", origin, provider.NodeID)
		}
		if err := b.snapshotShared.CheckManifest(origin); err != nil {
			return fmt.Errorf("reading frozen filesystem origin %d manifest: %w", origin, err)
		}
		if volatile {
			changes, err := repository.FreezeOriginHandoff(origin)
			if err != nil {
				return fmt.Errorf("reading filesystem origin %d reference handoff: %w", origin, err)
			}
			if err := publisher.publishSharedReferenceLedger(origin, changes, bridgeIntent); err != nil {
				return fmt.Errorf("transferring filesystem origin %d reference authority: %w", origin, err)
			}
			references, _ := repository.OriginReferences(origin)
			if err := publisher.keepProviderReferences(origin, repository.Path(), references); err != nil {
				return err
			}
			if err := b.distributedSource.authority.PublishSnapshotOrigin(b.nodeID, b.distributedSource.incarnation, uint64(origin)); err != nil {
				return fmt.Errorf("publishing filesystem origin %d authority: %w", origin, err)
			}
		}
		if err := repository.CompleteOriginHandoff(origin); err != nil {
			return fmt.Errorf("completing filesystem origin %d handoff: %w", origin, err)
		}
		// A graph handoff's bridge lasts until its transaction ends, when the
		// shared coordinator releases it. The origin's own ends here.
		if bridgeIntent.Kind == sharedPublicationBridgeOrigin {
			if err := publisher.releaseSharedPublicationBridge(origin); err != nil {
				return fmt.Errorf("releasing filesystem origin %d publication bridge: %w", origin, err)
			}
		}
		return nil
	})
	return err
}

// rollForwardFrozenFilesystemOrigin completes, in the background, the
// handoff of origin, a filesystem origin whose repository a failed or
// interrupted handoff left frozen, retrying until it completes, as
// rollForwardFrozenOrigin does a memory origin's.
func (b *runtimeBackend) rollForwardFrozenFilesystemOrigin(origin ttcsnapshot.SnapshotID) {
	source := b.distributedSource
	if source == nil || origin == 0 {
		return
	}
	if _, running := source.rollingForward.LoadOrStore(origin, struct{}{}); running {
		return
	}
	go func() {
		defer source.rollingForward.Delete(origin)
		for delay := time.Second; ; delay = min(2*delay, 30*time.Second) {
			err := b.resumeFrozenFilesystemOrigin(origin)
			if err == nil || errors.Is(err, errFrozenOriginOwned) {
				return
			}
			fmt.Fprintf(os.Stderr, "ttcd: completing the frozen handoff of filesystem origin %d: %v\n", origin, err)
			time.Sleep(delay)
		}
	}()
}

type snapshotProviderLookup interface {
	SnapshotProvider(uint64) (cluster.SnapshotOriginProvider, bool, error)
}

func snapshotOriginAuthority(authority snapshotProviderLookup, origin ttcsnapshot.SnapshotID) (cluster.SnapshotOriginProvider, bool, bool, error) {
	if authority == nil || origin == 0 {
		return cluster.SnapshotOriginProvider{}, false, false, errors.New("snapshot origin authority is unavailable")
	}
	provider, volatile, err := authority.SnapshotProvider(uint64(origin))
	if errors.Is(err, os.ErrNotExist) {
		return cluster.SnapshotOriginProvider{}, false, true, nil
	}
	return provider, volatile, false, err
}

// publishRetirementFilesystemOrigin publishes a filesystem origin discovered
// in node-local repository metadata unless Raft has already permanently
// retired or abandoned that origin. A corrupted branch can leave an immutable
// local graph generation behind after provider-loss reconciliation has
// tombstoned its authority. Such residue must not prevent the node that still
// holds the disposable scratch copy from retiring.
//
// Do not generally translate os.ErrNotExist from publication into success:
// the same error can report a missing live bulk object or shared manifest. We
// only accept it after SnapshotProvider itself proves the origin terminal,
// including a race in which authority becomes terminal during publication.
func (b *runtimeBackend) publishRetirementFilesystemOrigin(origin ttcsnapshot.SnapshotID) error {
	if b == nil || b.distributedSource == nil || b.distributedSource.authority == nil {
		return errors.New("distributed filesystem publication is unavailable")
	}
	_, _, terminal, err := snapshotOriginAuthority(b.distributedSource.authority, origin)
	if err != nil {
		return fmt.Errorf("resolving retiring filesystem origin %d: %w", origin, err)
	}
	if terminal {
		return nil
	}
	err = b.publishVolatileFilesystemOrigin(origin)
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, _, terminal, lookupErr := snapshotOriginAuthority(b.distributedSource.authority, origin)
	if lookupErr != nil {
		return errors.Join(err, fmt.Errorf("rechecking retiring filesystem origin %d authority: %w", origin, lookupErr))
	}
	if terminal {
		return nil
	}
	return err
}

// publishRemainingVolatileOrigins is the graceful-retirement authority
// barrier. Branch migration and dependency draining remove the common roots,
// but remote descendants may legitimately keep older immutable origins alive.
// A node must publish or abandon every such origin before its process and
// scratch storage become disposable.
func (b *runtimeBackend) publishRemainingVolatileOrigins() error {
	if b == nil || b.distributedSource == nil || b.distributedSource.authority == nil || b.nodeID == "" {
		return nil
	}
	// drainNode has fenced and evacuated every runtime. Seal the repositories'
	// final working generations before consulting the coarse Raft provider
	// directory; publishing a raw working origin is invalid, while creating a
	// replacement working generation would leave new authority on the node that
	// is trying to retire.
	preparedFilesystem, err := b.containerStore().PrepareFilesystemRetirementOrigins()
	if err != nil {
		return fmt.Errorf("preparing retiring filesystem generations: %w", err)
	}
	for _, origin := range preparedFilesystem {
		if err := b.publishRetirementFilesystemOrigin(origin); err != nil {
			return fmt.Errorf("publishing prepared filesystem origin %d: %w", origin, err)
		}
	}
	if _, err := b.reconcileVolatileSnapshotOrigins(); err != nil {
		return err
	}
	filesystem, err := b.containerStore().FilesystemSnapshotOrigins()
	if err != nil {
		return fmt.Errorf("inventorying filesystem origins before retirement: %w", err)
	}
	state, err := b.distributedSource.authority.AuthorityState()
	if err != nil {
		return err
	}
	var origins []ttcsnapshot.SnapshotID
	for encoded, provider := range state.VolatileOrigins {
		if provider.NodeID == b.nodeID {
			origins = append(origins, ttcsnapshot.SnapshotID(encoded))
		}
	}
	sort.Slice(origins, func(i, j int) bool { return origins[i] < origins[j] })
	settled := false
	for _, origin := range origins {
		if b.snapshotGraph != nil {
			if _, ok := b.snapshotGraph.Snapshot(origin); ok {
				if err := b.publishVolatileGraphOrigin(origin); err != nil {
					return fmt.Errorf("publishing remaining memory origin %d: %w", origin, err)
				}
				continue
			}
		}
		if _, ok := filesystem[origin]; ok {
			if err := b.publishVolatileFilesystemOrigin(origin); err != nil {
				return fmt.Errorf("publishing remaining filesystem origin %d: %w", origin, err)
			}
			continue
		}
		// Reconciliation inventories the same local stores while the daemon
		// mutation lock is held, but leaves an origin whose lock, or lock
		// stripe, another holds to a later pass: settle it now, waiting for
		// the lock. The last check below is what decides.
		if !settled {
			b.distributedSource.awaitOriginAllocations()
			settled = true
		}
		if err := b.settleStaleOrigin(b.nodeID, uint64(origin), true); err != nil {
			return fmt.Errorf("settling remaining origin %d: %w", origin, err)
		}
	}
	state, err = b.distributedSource.authority.AuthorityState()
	if err != nil {
		return err
	}
	for encoded, provider := range state.VolatileOrigins {
		if provider.NodeID == b.nodeID {
			return fmt.Errorf("snapshot origin %d still names retiring node %q", encoded, b.nodeID)
		}
	}
	return nil
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
	logicalSource := source
	resolved, sourceInfo, err := b.resolveSelector(source)
	if err != nil {
		return selector{}, err
	}
	source = resolved
	logicalSourceContainer := sourceInfo.DistributedFamilyID
	if logicalSourceContainer == "" {
		logicalSourceContainer = sourceInfo.ID
	}
	logicalSource.container = logicalSourceContainer
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
		return b.forkRemoteDehydrated(source, logicalSource, sourceInfo, peer, targetNode, target, paused, nil)
	}
	finishForegroundCapture, err := b.beginForegroundCapture(sourceInfo, longOperation{branch: source.branch, state: "forking"})
	if err != nil {
		return selector{}, err
	}
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
		TargetNodeID: targetNode, SourceContainer: logicalSourceContainer, SourceBranch: logicalSource.branch,
		TargetContainer: logicalSourceContainer, TargetBranch: transactionID,
		TargetVisibleBranch: target.branch,
		ImageDigest:         sourceInfo.Digest, RootfsDigest: rootfsDigest,
		RepositoryID: b.distributedSource.capabilities.RepositoryID,
	}
	family, err := b.portableDistributedFamily(sourceInfo)
	if err != nil {
		return selector{}, err
	}
	// Reserving the target, capturing the branch and restoring it there take
	// seconds, and more for more memory; other commands on this node go on
	// meanwhile, except on its family.
	resumeLifecycle, err := b.releaseLifecycleLock(sourceInfo, longOperation{branch: source.branch, state: "forking"})
	if err != nil {
		return selector{}, err
	}
	defer resumeLifecycle()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	tx, err := reserveRemoteTarget(ctx, targetNode, b.distributedSource.directory.Lookup, func(ctx context.Context) (*cluster.Transaction, error) {
		return peer.Reserve(ctx, cluster.ReserveRequest{
			SourceCapabilities:      b.distributedSource.capabilities,
			Identity:                identity,
			Family:                  family,
			SourcePhysicalContainer: sourceInfo.ID,
			SourcePhysicalBranch:    source.branch,
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
	filesystemRoots, filesystemBytes, err := b.prepareDistributedFilesystem(sourceInfo, session.FilesystemBranch, "", relay)
	if err != nil {
		return selector{}, fmt.Errorf("sealing distributed filesystem generation: %w", err)
	}
	internalMounts, err := b.portableDistributedInternalMounts(sourceInfo.ID)
	if err != nil {
		return selector{}, fmt.Errorf("capturing portable internal mounts: %w", err)
	}
	execConsoleLogs, err := b.configureRemoteExecConsoleLogs(relay, session.Token, sourceInfo.ID, source.branch, session.ExecSessions)
	if err != nil {
		return selector{}, fmt.Errorf("capturing retained exec output: %w", err)
	}
	descriptor := cluster.RemoteSessionDescriptor{
		Address: session.Address, SnapshotToken: hex.EncodeToString(session.Token[:]),
		MetadataBytes: session.MetadataBytes, StreamBytes: session.StreamBytes,
		MaxTransferBytes: dataPlaneMax,
		FilesystemRoots:  filesystemRoots, FilesystemBytes: filesystemBytes, InternalMounts: internalMounts,
		ExecSessions: remoteExecSessions(session.ExecSessions), ExecConsoleLogs: execConsoleLogs,
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
	monitorBackend := b.forBackground()
	if tx.Negotiated.PagingPolicy.Push == ttcpolicy.PushDemandOnly {
		b.distributedSource.scheduleHandoff(func() {
			monitorBackend.handoffDistributedFork(peer, source.container, transactionID, tx.Revision, relay)
		})
	} else {
		b.distributedSource.scheduleSourceMonitor(func() {
			monitorBackend.monitorDistributedFork(peer, source.container, transactionID, tx.Revision, relay)
		})
	}
	return target, nil
}

// forkRemoteDehydrated restores a process-free branch directly from its
// durable kernel metadata and sparse memory graph. No source Sentry is
// created, and the target is independent as soon as publication completes.
func (b *runtimeBackend) forkRemoteDehydrated(source, logicalSource selector, sourceInfo ttcbundle.ContainerInfo, peer *cluster.Client, targetNode string, target selector, paused bool, committedTransaction *string) (result selector, retErr error) {
	sourceBranch, branchErr := persistentBranchInfo(sourceInfo, source.branch)
	if branchErr != nil {
		return selector{}, branchErr
	}
	// A cold fork adds references to immutable memory and filesystem origins.
	// Serialize those additions with the short provider-to-shared authority
	// switch, just like a live capture. Slow pack construction happens outside
	// the background side of this gate, so this does not make a fork wait for
	// the complete publication; it waits only if the final handoff is already
	// in progress. Without the gate, FreezeSnapshotLedger can reject a perfectly
	// valid fork in the narrow interval after publication freezes the provider
	// ledger and before shared authority becomes visible.
	finishForegroundCapture, err := b.beginForegroundCapture(sourceInfo, longOperation{branch: source.branch, state: "forking"})
	if err != nil {
		return selector{}, err
	}
	defer finishForegroundCapture()
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
		TargetNodeID: targetNode, SourceContainer: logicalSource.container, SourceBranch: logicalSource.branch,
		TargetContainer: logicalSource.container, TargetBranch: transactionID, TargetVisibleBranch: target.branch,
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
		return peer.Reserve(ctx, cluster.ReserveRequest{
			SourceCapabilities: b.distributedSource.capabilities, Identity: identity, Family: family,
			SourcePhysicalContainer: sourceInfo.ID, SourcePhysicalBranch: source.branch,
		})
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
	const (
		metadataArtifact = "kernel-metadata"
		graphArtifact    = "memory-graph"
	)
	if err := relay.ConfigureArtifacts([16]byte(filesystemToken), map[string][]byte{
		metadataArtifact: encodedMetadata,
		graphArtifact:    graphManifest,
	}); err != nil {
		return selector{}, fmt.Errorf("configuring dehydrated artifact relay: %w", err)
	}
	filesystemRoots, filesystemBytes, err := b.prepareDistributedFilesystem(sourceInfo, childFilesystemBranch, "", relay)
	if err != nil {
		return selector{}, fmt.Errorf("preparing dehydrated filesystem objects: %w", err)
	}
	if err := applyDehydratedFilesystemRestoreIDs(filesystemRoots, sourceBranch.StorageManifest); err != nil {
		return selector{}, fmt.Errorf("reading dehydrated filesystem identities: %w", err)
	}
	internalMounts, err := b.portableDistributedInternalMounts(sourceInfo.ID)
	if err != nil {
		return selector{}, fmt.Errorf("capturing portable internal mounts: %w", err)
	}
	var directSources []ttcvolume.RemoteObjectSource
	var directCleanup func()
	if sourceBranch.StorageManifest == "" {
		directSources, directCleanup, err = b.prepareLocalDirectFilesystemSources(sourceInfo, childFilesystemBranch)
	} else {
		directSources, directCleanup, err = b.prepareColdDirectFilesystemSources(sourceInfo, sourceBranch, childFilesystemBranch)
	}
	if err != nil {
		return selector{}, fmt.Errorf("opening durable filesystem sources: %w", err)
	}
	if err := relay.ConfigureDirectFilesystem(filesystemToken, directSources, directCleanup); err != nil {
		directCleanup()
		return selector{}, err
	}
	execConsoleLogs, err := b.configureRemoteExecConsoleLogs(relay, filesystemToken, sourceInfo.ID, sourceBranch.ID, capture.ExecSessions)
	if err != nil {
		return selector{}, fmt.Errorf("capturing retained exec output: %w", err)
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
		MetadataArtifact: metadataArtifact, GraphArtifact: graphArtifact, GraphArtifactBytes: uint64(len(graphManifest)),
		GraphSnapshot: capture.SnapshotID, GraphRootObjects: rootObjects,
		DurableDehydration: true, FilesystemRoots: filesystemRoots, FilesystemBytes: filesystemBytes, InternalMounts: internalMounts,
		ExecSessions: remoteExecSessions(capture.ExecSessions), ExecConsoleLogs: execConsoleLogs,
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
	logicalSource := source
	resolved, sourceInfo, err := b.resolveSelector(source)
	if err != nil {
		return nil, err
	}
	source = resolved
	logicalSource.container = logicalFamilyID(sourceInfo)
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
			result, err := b.forkRemoteDehydrated(source, logicalSource, sourceInfo, peer, targetNode, target, paused, &transactionID)
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
	finishForegroundCapture, err := b.beginForegroundCapture(sourceInfo, longOperation{branch: source.branch, state: "forking"})
	if err != nil {
		return nil, err
	}
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
		identity     cluster.ForkIdentity
		tx           *cluster.Transaction
		relay        *distributedPageRelay
		advertise    string
		dataPlaneMax uint64
		captured     bool
	}
	children := make([]childState, len(targetContainers))
	// Reserving the targets, capturing the branch and restoring it there take
	// seconds, and more for more memory; other commands on this node go on
	// meanwhile, except on its family.
	resumeLifecycle, err := b.releaseLifecycleLock(sourceInfo, longOperation{branch: source.branch, state: "forking"})
	if err != nil {
		return nil, err
	}
	defer resumeLifecycle()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		// Imported siblings share the first child's physical Sentry. Remove
		// imports before their loader branch so rollback never deletes a runtime
		// that still contains a later sibling's pending metadata.
		for i := len(children) - 1; i >= 0; i-- {
			child := &children[i]
			if child.relay != nil {
				child.relay.Close()
			}
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

	var commonPolicy ttcpolicy.Policy
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
			TargetNodeID: targetNode, SourceContainer: logicalSource.container, SourceBranch: logicalSource.branch,
			TargetContainer: logicalSource.container, TargetBranch: transactionID, TargetVisibleBranch: target.branch,
			ImageDigest: sourceInfo.Digest, RootfsDigest: rootfsDigest,
			RepositoryID: b.distributedSource.capabilities.RepositoryID,
		}
		var tx *cluster.Transaction
		if i == 0 {
			tx, err = reserveRemoteTarget(ctx, targetNode, b.distributedSource.directory.Lookup, func(ctx context.Context) (*cluster.Transaction, error) {
				return peer.Reserve(ctx, cluster.ReserveRequest{
					SourceCapabilities: b.distributedSource.capabilities, Identity: identity, Family: family,
					SourcePhysicalContainer: sourceInfo.ID, SourcePhysicalBranch: source.branch,
				})
			})
			if err != nil {
				return nil, fmt.Errorf("reserving target %s on node %s: %w", target, targetNode, err)
			}
			commonPolicy = tx.Negotiated.PagingPolicy
			commonDataPlaneMax = commonPolicy.TransferBytes
			if commonPolicy.Prefetch == ttcpolicy.PrefetchAdaptive {
				commonDataPlaneMax = tx.Negotiated.MaxTransferSize
			}
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
			target: target, transaction: transactionID, identity: identity, tx: tx,
			relay:     newDistributedPageRelay(listener, &relayBackend, source.container, transactionID),
			advertise: advertise, dataPlaneMax: commonDataPlaneMax,
		}
	}
	captureArgs := boot.DistributedForkCaptureManyArgs{
		// Distributed metadata is a cluster-visible snapshot identity, not the
		// identity of the Sentry process that happens to capture it. In
		// particular, source.container changes after migration while
		// logicalSource.container remains stable. Singular fork and migration
		// use the logical identity too; keeping the batch path consistent lets a
		// migrated branch fan out without weakening restore's identity check.
		Source: source.branch, FamilyID: logicalSource.container,
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
		filesystemRoots, filesystemBytes, err := b.prepareDistributedFilesystem(sourceInfo, session.FilesystemBranch, "", child.relay)
		if err != nil {
			return nil, fmt.Errorf("sealing filesystem generation for target %s: %w", child.target, err)
		}
		execConsoleLogs, err := b.configureRemoteExecConsoleLogs(child.relay, session.Token, sourceInfo.ID, source.branch, session.ExecSessions)
		if err != nil {
			return nil, fmt.Errorf("capturing retained exec output for target %s: %w", child.target, err)
		}
		descriptors[i] = cluster.RemoteSessionDescriptor{
			Address: session.Address, SnapshotToken: hex.EncodeToString(session.Token[:]),
			MetadataBytes: session.MetadataBytes, StreamBytes: session.StreamBytes,
			MaxTransferBytes: child.dataPlaneMax,
			FilesystemRoots:  filesystemRoots, FilesystemBytes: filesystemBytes, InternalMounts: internalMounts,
			ExecSessions: remoteExecSessions(session.ExecSessions), ExecConsoleLogs: execConsoleLogs,
		}
	}
	prepareChild := func(i int) error {
		child := &children[i]
		var err error
		child.tx, err = peer.Prepare(ctx, child.transaction, child.tx.Revision, descriptors[i])
		return err
	}
	publishChild := func(i int) error {
		child := &children[i]
		var err error
		child.tx, err = publishRemoteTarget(ctx, peer, child.transaction, child.tx.Revision, paused)
		return err
	}
	// Establish one authoritative physical Sentry before reserving the other
	// siblings. Their reservations then resolve through the first placement and
	// become branch imports into that Sentry. Reserving the whole batch up front
	// would create one hidden physical runtime per transaction because no
	// placement exists yet.
	if err := prepareChild(0); err != nil {
		return nil, fmt.Errorf("restoring target %s on node %s: %w", children[0].target, targetNode, err)
	}
	if err := publishChild(0); err != nil {
		return nil, fmt.Errorf("publishing target %s on node %s: %w", children[0].target, targetNode, err)
	}
	for i := 1; i < len(children); i++ {
		child := &children[i]
		child.tx, err = reserveRemoteTarget(ctx, targetNode, b.distributedSource.directory.Lookup, func(ctx context.Context) (*cluster.Transaction, error) {
			return peer.Reserve(ctx, cluster.ReserveRequest{
				SourceCapabilities: b.distributedSource.capabilities, Identity: child.identity, Family: family,
				SourcePhysicalContainer: sourceInfo.ID, SourcePhysicalBranch: source.branch,
			})
		})
		if err != nil {
			return nil, fmt.Errorf("reserving target %s on node %s: %w", child.target, targetNode, err)
		}
		dataPlaneMax := child.tx.Negotiated.PagingPolicy.TransferBytes
		if child.tx.Negotiated.PagingPolicy.Prefetch == ttcpolicy.PrefetchAdaptive {
			dataPlaneMax = child.tx.Negotiated.MaxTransferSize
		}
		if child.tx.Negotiated.PagingPolicy != commonPolicy || dataPlaneMax != commonDataPlaneMax {
			return nil, fmt.Errorf("target %s negotiated a different paging policy within one sibling batch", child.target)
		}
	}
	prepareErrors := make([]error, len(children)-1)
	runBoundedBatch(len(children)-1, distributedBatchControlConcurrency, func(index int) {
		prepareErrors[index] = prepareChild(index + 1)
	})
	for index, prepareErr := range prepareErrors {
		if prepareErr != nil {
			i := index + 1
			return nil, fmt.Errorf("restoring target %s on node %s: %w", children[i].target, targetNode, prepareErr)
		}
	}
	publishErrors := make([]error, len(children)-1)
	runBoundedBatch(len(children)-1, distributedBatchControlConcurrency, func(index int) {
		publishErrors[index] = publishChild(index + 1)
	})
	for index, publishErr := range publishErrors {
		if publishErr != nil {
			i := index + 1
			return nil, fmt.Errorf("publishing target %s on node %s: %w", children[i].target, targetNode, publishErr)
		}
	}
	// Every target is now externally visible and owns its durable snapshot.
	// Later independence bookkeeping failures must not cancel or delete them.
	cleanup = false
	results := make([]selector, len(children))
	for i := range children {
		child := &children[i]
		monitorBackend := b.forBackground()
		if commonPolicy.Push == ttcpolicy.PushDemandOnly {
			b.distributedSource.scheduleHandoff(func() {
				monitorBackend.handoffDistributedFork(peer, source.container, child.transaction, child.tx.Revision, child.relay)
			})
		} else {
			b.distributedSource.scheduleSourceMonitor(func() {
				monitorBackend.monitorDistributedFork(peer, source.container, child.transaction, child.tx.Revision, child.relay)
			})
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
	b.handoffDistributedForkUntil(peer, sourceContainer, transactionID, revision, relay, time.Now().Add(forkHandoffWait))
}

// distributedHandoffFreezeRetry is how long a graph handoff whose export met a
// frozen snapshot ledger waits before it exports again.
const distributedHandoffFreezeRetry = time.Second

// handoffDistributedForkUntil is handoffDistributedFork, which waits out, until
// deadline, the freezes of the snapshots its export references.
func (b *runtimeBackend) handoffDistributedForkUntil(peer *cluster.Client, sourceContainer, transactionID string, revision uint64, relay *distributedPageRelay, deadline time.Time) {
	// A successful handoff makes its transaction-scoped pack and intent
	// unreachable. Collection must follow the terminal transition here rather
	// than relying on the daemon's periodic maintenance loop: an elastic source
	// may stop immediately after its last dependency becomes independent.
	defer b.distributedSource.scheduleColdCollection(b.collectTerminalColdStorage)
	handoff, rootNames, rootObjects, err := b.exportDistributedGraph(sourceContainer, transactionID)
	if err != nil && frozenForAuthorityHandoff(err) && time.Now().Before(deadline) {
		// The export references a snapshot whose ledger is frozen while its
		// provider hands it to the shared store, which ends when that
		// publication does. Export again then, as a fork waits out the same
		// freeze, rather than leave the target on live paging for good.
		if _, terminal := b.distributedSource.sourceTransactionTerminalAction(transactionID); !terminal {
			b.distributedSource.scheduleHandoffAfter(distributedHandoffFreezeRetry, func() {
				b.handoffDistributedForkUntil(peer, sourceContainer, transactionID, revision, relay, deadline)
			})
			return
		}
	}
	if err != nil {
		// Graph publication is an optimization, but failure leaves the target
		// dependent on a live source indefinitely. Background monitors do not own
		// a client response stream, so report the fallback in the daemon log
		// instead of silently discarding the only useful diagnosis.
		if !errors.Is(err, errExportAbandoned) {
			fmt.Fprintf(os.Stderr, "ttcd: distributed graph handoff %s fell back to live paging: %v\n", transactionID, err)
		}
		// Ordinary demand paging may remain live for the lifetime of the target
		// when graph publication is unavailable. It must not occupy one of the
		// bounded graph-export workers and prevent independent siblings from
		// being consolidated.
		b.distributedSource.scheduleSourceMonitor(func() {
			b.monitorDistributedFork(peer, sourceContainer, transactionID, revision, relay)
		})
		return
	}
	var releaseOnce sync.Once
	releaseExport := func() {
		releaseOnce.Do(func() {
			_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
		})
	}
	finishTerminal := func(action string) {
		_, _ = b.command("timetravel", sourceContainer, action, transactionID)
		relay.Close()
		releaseExport()
	}
	var tx *cluster.Transaction
	var lastHandoffError string
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
						finishTerminal("distributed-complete")
						return
					}
				case cluster.TransactionCanceled, cluster.TransactionFailed:
					cancel()
					finishTerminal(distributedSourceTerminalCommand(current))
					return
				}
			}
		}
		cancel()
		if err == nil && tx != nil {
			break
		}
		if err != nil && err.Error() != lastHandoffError {
			lastHandoffError = err.Error()
			fmt.Fprintf(os.Stderr, "ttcd: distributed graph handoff %s is waiting for target acknowledgement: %v\n", transactionID, err)
		}
		if action, terminal := b.distributedSource.sourceTransactionTerminalAction(transactionID); terminal {
			finishTerminal(action)
			return
		}
		// Once publication succeeds, never delete its root merely because an
		// RPC response or reader switch failed. The target may have completed
		// the switch, and both the durable descriptor and Sentry operation are
		// idempotent. Retain live paging and retry until target acknowledgement.
		time.Sleep(500 * time.Millisecond)
	}
	if tx.State == cluster.TransactionIndependent {
		finishTerminal("distributed-complete")
		return
	}
	if tx.GraphHandoff == nil {
		// Eager drain won the race, so this source-side sparse publication was
		// never made authoritative and can be reclaimed immediately.
		releaseExport()
	}
	if err := b.completeSharedDistributedFork(peer, sourceContainer, transactionID, tx.Revision, relay); err != nil {
		// Retrying status/independence is safe; the target now has no source
		// reads, while the source retains its snapshot until completion.
		b.distributedSource.scheduleSourceMonitor(func() {
			b.monitorHandoffCompletion(peer, sourceContainer, transactionID, tx.Revision, relay, rootNames, rootObjects)
		})
		return
	}
	releaseExport()
}

func (b *runtimeBackend) monitorHandoffCompletion(peer *cluster.Client, sourceContainer, transactionID string, revision uint64, relay *distributedPageRelay, rootNames, rootObjects []string) {
	defer relay.Close()
	// This monitor owns the exported root after the synchronous completion path
	// hands off to it. All of its return paths are terminal, so release the
	// source's authoritative reference exactly once. The restore manifest is
	// owned by the bound Raft transaction: an independent target still needs it
	// to recreate its detached shared-pack reader after a daemon/helper restart.
	defer func() {
		_ = b.releaseSharedGraphMemoryRoots(rootNames, rootObjects)
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
				_, _ = b.command("timetravel", sourceContainer, distributedSourceTerminalCommand(status), transactionID)
				return
			}
		}
		if action, terminal := b.distributedSource.sourceTransactionTerminalAction(transactionID); terminal {
			_, _ = b.command("timetravel", sourceContainer, action, transactionID)
			return
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
				_, _ = b.command("timetravel", sourceContainer, distributedSourceTerminalCommand(peerStatus), transactionID)
				return
			}
		}
		if action, terminal := b.distributedSource.sourceTransactionTerminalAction(transactionID); terminal {
			_, _ = b.command("timetravel", sourceContainer, action, transactionID)
			return
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
		// Close releases every still-retained page and consequently reports zero
		// outstanding bytes. That is cancellation, not proof that the target
		// installed the bytes. A source Sentry can close a session while an older
		// migration is being retired; treating that local cleanup as a successful
		// ACK fence makes a later migration cascade independence through the whole
		// chain and strands the newest target on a dead TCP reader.
		if status.Snapshot.Closed {
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
			if info, err := backend.containerStore().LoadDistributedForkTarget(tx.Identity.TargetContainer, tx.Identity.TransactionID); err == nil {
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
				if tx.GraphHandoff != nil && (tx.State == cluster.TransactionPublished || tx.State == cluster.TransactionDraining || tx.State == cluster.TransactionIndependent) {
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
		monitorBackend := backend.forBackground()
		if tx.Identity.Migration {
			if tx.State == cluster.TransactionDraining {
				runtime.source.scheduleSourceMonitor(func() {
					monitorBackend.monitorHandoffCompletion(peer, tx.Identity.SourceContainer,
						tx.Identity.TransactionID, tx.Revision, nil, nil, nil)
				})
			} else {
				runtime.source.scheduleHandoff(func() {
					monitorBackend.handoffDistributedFork(peer, tx.Identity.SourceContainer,
						tx.Identity.TransactionID, tx.Revision, nil)
				})
			}
		} else if tx.Negotiated.PagingPolicy.Push == ttcpolicy.PushDemandOnly {
			if tx.State == cluster.TransactionDraining {
				runtime.source.scheduleSourceMonitor(func() {
					monitorBackend.monitorHandoffCompletion(peer, tx.Identity.SourceContainer,
						tx.Identity.TransactionID, tx.Revision, nil, nil, nil)
				})
			} else {
				runtime.source.scheduleHandoff(func() {
					monitorBackend.handoffDistributedFork(peer, tx.Identity.SourceContainer,
						tx.Identity.TransactionID, tx.Revision, nil)
				})
			}
		} else {
			runtime.source.scheduleSourceMonitor(func() {
				monitorBackend.monitorDistributedFork(peer, tx.Identity.SourceContainer,
					tx.Identity.TransactionID, tx.Revision, nil)
			})
		}
		if tx.Identity.Migration {
			runtime.source.scheduleSourceMonitor(func() {
				monitorBackend.monitorSparseMigrationCleanup(peer, tx.Identity.SourceContainer,
					tx.Identity.SourceBranch, tx.Identity.TransactionID, nil)
			})
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

// distributedSourceTerminalCommand chooses how a source releases a terminal
// remote session. Once an activated migration has crossed the placement CAS,
// its target owns the forked filesystem branch even if the target process dies
// before draining memory. Completing releases page reservations while retaining
// that filesystem; canceling would delete the branch referenced by the now-
// exited target and make an ordinary later `ttc start` impossible.
func distributedSourceTerminalCommand(tx *cluster.Transaction) string {
	if tx != nil && tx.Identity.Migration && tx.Activated {
		return "distributed-complete"
	}
	return "distributed-cancel"
}
