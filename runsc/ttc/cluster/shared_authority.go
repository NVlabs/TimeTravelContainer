// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// A cluster is anchored in its shared root, under cluster/:
//
//   - authority/record names the node that holds the cluster's authority, and
//     the epoch it holds it for.
//   - authority/epochs/N is the claim of epoch N, which one node creates
//     exclusively to take the authority.
//   - authority/N is the state of the authority of epoch N: a snapshot and
//     the log its holder appends to (see authorityLog). A node that takes
//     the authority makes epoch N+1's from epoch N's, so that nothing a node
//     leaves behind, however it leaves, can write the log of its successor.
//   - members/NODE is what the daemon of NODE says of itself, and a counter
//     it advances every heartbeat.
//
// Whether a daemon is alive is judged by whether its counter advances, on the
// reader's own clock, as sanlock does, and not by locks: file locks are
// node-local on Lustre with localflock, on BeeGFS by default, and on NFS
// mounted nolock. Records are read and written a block at a time with direct
// I/O, so that a reader on another host reads what the server has, not what
// its cache remembers.
//
// A daemon holds a lease while its heartbeats land: it holds it for
// LeaseHold after the last one did. A node takes the authority only once the
// holder's counter has stood still for AuthorityTakeover on the taker's
// clock, longer than the holder's lease, and the holder acknowledges a write
// of its log only while it holds its lease and no later epoch's record has
// replaced its own: it checks both after the write is synced. So whatever the
// holder acknowledged is in its log before its successor copies it.

const (
	// HeartbeatInterval is how often a daemon advances its counter.
	HeartbeatInterval = time.Second
	// LeaseHold is how long a daemon holds its lease after its last
	// heartbeat landed.
	LeaseHold = 7 * time.Second
	// AuthorityTakeover is how long an authority's counter must stand still
	// before another node takes the authority. With the default execution
	// lease, a node's Sentries keep running through a takeover: its lease
	// runs at least 12.5 s past its last renewal.
	AuthorityTakeover = 10 * time.Second
)

// Roles of the daemons of a cluster.
const (
	// RoleCompute is a daemon that runs containers.
	RoleCompute = "compute"
	// RoleControl is a control-only daemon, which a login node runs, and
	// which no other daemon ever connects to.
	RoleControl = "control"
)

// AuthorityRecord names the node that holds the authority of a cluster.
type AuthorityRecord struct {
	Epoch             uint64 `json:"epoch"`
	NodeID            string `json:"node_id"`
	Incarnation       string `json:"incarnation"`
	Role              string `json:"role"`
	ControlURL        string `json:"control_url,omitempty"`
	ControlServerName string `json:"control_server_name,omitempty"`
	// Previous is the epoch whose state this epoch's was made from, or 0 for
	// a cluster's first.
	Previous  uint64    `json:"previous,omitempty"`
	ClaimedAt time.Time `json:"claimed_at"`
}

// Holds reports whether record is the authority of the daemon of self.
func (record AuthorityRecord) Holds(self AuthorityRecord) bool {
	return record.NodeID == self.NodeID && record.Incarnation == self.Incarnation
}

// MemberRecord is what a daemon of a cluster says of itself.
type MemberRecord struct {
	NodeID            string `json:"node_id"`
	Incarnation       string `json:"incarnation"`
	Role              string `json:"role"`
	ControlURL        string `json:"control_url,omitempty"`
	ControlServerName string `json:"control_server_name,omitempty"`
	// Counter advances every heartbeat.
	Counter uint64 `json:"counter"`
	// Authority is the epoch whose authority the daemon holds, if any.
	Authority uint64 `json:"authority,omitempty"`
	// Released is the latest epoch whose authority the daemon gave up.
	Released uint64 `json:"released,omitempty"`
	// Stopped is set by the last heartbeat of a daemon that stops cleanly,
	// so that a reader knows it stopped without waiting for its counter to
	// stand still.
	Stopped bool `json:"stopped,omitempty"`
	// UpdatedAt is the daemon's own time of the heartbeat, for people.
	UpdatedAt time.Time `json:"updated_at"`
}

// SharedCluster is the part of a cluster in its shared root.
type SharedCluster struct {
	// Dir is the cluster directory of the shared root.
	Dir string
	// files, if set, keeps the files of the records open: see
	// KeepFilesOpen.
	files *sharedFiles
}

// KeepFilesOpen returns c, reading and writing the records of its members and
// of its authority through files it keeps open until CloseFiles. On a network
// filesystem, an open and a close cost a request each, where a read or a write
// of an open file costs one; a daemon reads and writes records every second,
// and its authority's holder reads the authority record for each batch.
func (c SharedCluster) KeepFilesOpen() SharedCluster {
	c.files = &sharedFiles{files: make(map[sharedFileKey]*sharedFile)}
	return c
}

// CloseFiles closes the files c keeps open; from then on, each read or write
// of a record opens its file for itself.
func (c SharedCluster) CloseFiles() {
	if c.files != nil {
		c.files.close()
	}
}

// readBlock reads the shared block of path into value.
func (c SharedCluster) readBlock(path string, value any) error {
	return c.files.use(path, unix.O_RDONLY, func(fd int) error { return readSharedBlockFrom(fd, path, value) })
}

// writeBlock writes value as the shared block of path.
func (c SharedCluster) writeBlock(path string, value any) error {
	return c.files.use(path, unix.O_WRONLY|unix.O_CREAT, func(fd int) error { return writeSharedBlockTo(fd, path, value) })
}

// NewSharedCluster returns the cluster anchored in sharedRoot.
func NewSharedCluster(sharedRoot string) SharedCluster {
	return SharedCluster{Dir: filepath.Join(sharedRoot, "cluster")}
}

func (c SharedCluster) authorityDir() string { return filepath.Join(c.Dir, "authority") }
func (c SharedCluster) recordPath() string   { return filepath.Join(c.authorityDir(), "record") }
func (c SharedCluster) claimsDir() string    { return filepath.Join(c.authorityDir(), "epochs") }
func (c SharedCluster) membersDir() string   { return filepath.Join(c.Dir, "members") }

func (c SharedCluster) claimPath(epoch uint64) string {
	return filepath.Join(c.claimsDir(), epochName(epoch))
}

func (c SharedCluster) memberPath(nodeID string) string {
	return filepath.Join(c.membersDir(), nodeID)
}

// EpochDir returns the directory of the state of epoch's authority.
func (c SharedCluster) EpochDir(epoch uint64) string {
	return filepath.Join(c.authorityDir(), epochName(epoch))
}

func epochName(epoch uint64) string {
	return fmt.Sprintf("%016d", epoch)
}

// ReadAuthority returns the authority record, and false if no node ever took
// the authority.
func (c SharedCluster) ReadAuthority() (AuthorityRecord, bool, error) {
	var record AuthorityRecord
	if err := c.readBlock(c.recordPath(), &record); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return AuthorityRecord{}, false, nil
		}
		return AuthorityRecord{}, false, fmt.Errorf("reading the cluster authority record: %w", err)
	}
	return record, true, nil
}

// WriteAuthority replaces the authority record.
func (c SharedCluster) WriteAuthority(record AuthorityRecord) error {
	if err := os.MkdirAll(c.authorityDir(), 0700); err != nil {
		return err
	}
	if err := c.writeBlock(c.recordPath(), record); err != nil {
		return fmt.Errorf("writing the cluster authority record: %w", err)
	}
	return nil
}

// HolderView keeps the authority record for a daemon that sends requests
// to the holder it names: reading the record for each request would cost
// the shared filesystem a request or more each time.
type HolderView struct {
	Cluster SharedCluster

	mu       sync.Mutex
	record   AuthorityRecord
	recorded bool
	// read is when the record was read, zero before it was.
	read time.Time
}

// Record returns the authority record, reading it again if the one the view
// has was read more than maxAge ago.
func (v *HolderView) Record(maxAge time.Duration) (AuthorityRecord, bool, error) {
	v.mu.Lock()
	if !v.read.IsZero() && time.Since(v.read) <= maxAge {
		defer v.mu.Unlock()
		return v.record, v.recorded, nil
	}
	v.mu.Unlock()
	record, recorded, err := v.Cluster.ReadAuthority()
	if err != nil {
		return AuthorityRecord{}, false, err
	}
	v.Set(record)
	return record, recorded, nil
}

// Set gives the view a record just read.
func (v *HolderView) Set(record AuthorityRecord) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.record, v.recorded, v.read = record, record.Epoch != 0, time.Now()
}

// LatestClaim returns the latest epoch claimed, or 0 if none was.
func (c SharedCluster) LatestClaim() (uint64, error) {
	entries, err := os.ReadDir(c.claimsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("listing the cluster authority's epochs: %w", err)
	}
	var latest uint64
	for _, entry := range entries {
		epoch, err := strconv.ParseUint(entry.Name(), 10, 64)
		if err == nil && epoch > latest {
			latest = epoch
		}
	}
	return latest, nil
}

// ClaimEpoch claims epoch for the node of record, exclusively: it reports
// false if another node claimed it first.
func (c SharedCluster) ClaimEpoch(record AuthorityRecord) (bool, error) {
	if record.Epoch == 0 {
		return false, errors.New("the first epoch of a cluster authority is 1")
	}
	if err := os.MkdirAll(c.claimsDir(), 0700); err != nil {
		return false, err
	}
	data, err := json.Marshal(record)
	if err != nil {
		return false, err
	}
	file, err := os.OpenFile(c.claimPath(record.Epoch), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if errors.Is(err, fs.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("claiming epoch %d of the cluster authority: %w", record.Epoch, err)
	}
	_, err = file.Write(append(data, '\n'))
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return true, fmt.Errorf("recording the claim of epoch %d: %w", record.Epoch, err)
	}
	return true, syncDir(c.claimsDir())
}

// ReadClaim returns the record of the claim of epoch.
func (c SharedCluster) ReadClaim(epoch uint64) (AuthorityRecord, error) {
	var record AuthorityRecord
	data, err := os.ReadFile(c.claimPath(epoch))
	if err != nil {
		return AuthorityRecord{}, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return AuthorityRecord{}, fmt.Errorf("decoding the claim of epoch %d: %w", epoch, err)
	}
	return record, nil
}

// AuthorityDecision is what a daemon of a cluster does about its authority.
type AuthorityDecision struct {
	// Hold is set if the daemon holds the authority of Record's epoch. Its
	// state is to be made from Record.Previous's, unless it is made.
	Hold bool
	// Record names the authority, the daemon's own if it holds it.
	Record AuthorityRecord
}

// DecideAuthority decides whether the daemon of self, whose heartbeat runs,
// holds the authority of the cluster or joins the node that does, observing
// its holder's liveness through liveness. It waits for as long as it takes
// to know, or until ctx is done.
//
//   - The first daemon of a cluster claims its first epoch, and holds its
//     authority.
//   - A daemon holds the authority its record names it for, as when it
//     restarts, unless another daemon has claimed a later epoch since.
//   - A daemon takes the authority, claiming the next epoch, from a holder
//     that has left it or ended, and from one whose counter stood still for
//     AuthorityTakeover.
//   - A compute daemon waits for a control daemon to give the authority up,
//     as it does once it sees a compute daemon run, and takes it from one
//     that has not after AuthorityTakeover.
//   - Otherwise it joins the holder; a control daemon never does.
func (c SharedCluster) DecideAuthority(ctx context.Context, self AuthorityRecord, liveness *Liveness) (AuthorityDecision, error) {
	for {
		record, recorded, err := c.ReadAuthority()
		if err != nil {
			return AuthorityDecision{}, err
		}
		latest, err := c.LatestClaim()
		if err != nil {
			return AuthorityDecision{}, err
		}
		takeFrom := func(previous uint64) (bool, error) {
			claim := self
			claim.Epoch, claim.Previous, claim.ClaimedAt = max(latest, previous)+1, previous, time.Now().UTC()
			won, err := c.ClaimEpoch(claim)
			if err != nil || !won {
				return false, err
			}
			return true, c.WriteAuthority(claim)
		}
		decided := func(claimed bool, err error) (AuthorityDecision, bool, error) {
			if err != nil || !claimed {
				return AuthorityDecision{}, false, err
			}
			record, _, err := c.ReadAuthority()
			return AuthorityDecision{Hold: true, Record: record}, true, err
		}
		var decision AuthorityDecision
		done := false
		switch {
		case !recorded && latest == 0:
			// A new cluster.
			decision, done, err = decided(takeFrom(0))
		case latest > record.Epoch:
			// A claim not yet recorded: by this daemon before it restarted,
			// or by another, which is making the epoch.
			claim, claimErr := c.ReadClaim(latest)
			if claimErr != nil && !errors.Is(claimErr, fs.ErrNotExist) {
				return AuthorityDecision{}, claimErr
			}
			if claimErr == nil && claim.Holds(self) {
				if err := c.WriteAuthority(claim); err != nil {
					return AuthorityDecision{}, err
				}
				return AuthorityDecision{Hold: true, Record: claim}, nil
			}
			if claimErr == nil && c.alive(claim, liveness) {
				break
			}
			decision, done, err = decided(takeFrom(record.Epoch))
		case record.Holds(self):
			return AuthorityDecision{Hold: true, Record: record}, nil
		case !c.alive(record, liveness):
			decision, done, err = decided(takeFrom(record.Epoch))
		case record.Role == RoleControl && self.Role == RoleCompute:
			// A control daemon holds the authority only while no compute
			// daemon runs, and gives it up once it sees one's counter
			// advance; nothing dials it. So this daemon waits for it to, and
			// takes the authority from one that has not after
			// AuthorityTakeover.
			if liveness.waitedFor(record) >= AuthorityTakeover {
				decision, done, err = decided(takeFrom(record.Epoch))
			}
		case self.Role == RoleControl && !c.running(record, liveness):
			// A control daemon holds the authority only while no other
			// daemon does, and waits to see whether the holder runs: until
			// its counter advances, or has stood still for
			// AuthorityTakeover.
		default:
			return AuthorityDecision{Record: record}, nil
		}
		if err != nil || done {
			return decision, err
		}
		select {
		case <-ctx.Done():
			return AuthorityDecision{}, ctx.Err()
		case <-time.After(HeartbeatInterval / 2):
		}
	}
}

// alive reports whether the daemon of record, whose authority it holds or
// claimed, may still hold it: its counter has not stood still for
// AuthorityTakeover, and it has neither given the authority up nor stopped.
func (c SharedCluster) alive(record AuthorityRecord, liveness *Liveness) bool {
	member, err := c.ReadMember(record.NodeID)
	if err != nil || member.Incarnation != record.Incarnation || member.Released >= record.Epoch || member.Stopped {
		return false
	}
	return liveness.StillFor(member) < AuthorityTakeover
}

// running reports whether the counter of the daemon of record, whose
// authority it holds, has been seen to advance.
func (c SharedCluster) running(record AuthorityRecord, liveness *Liveness) bool {
	member, err := c.ReadMember(record.NodeID)
	return err == nil && member.Incarnation == record.Incarnation && liveness.Advancing(member)
}

// AwaitRelease waits until the daemon that held the authority of previous
// can no longer write its log: until it gave the authority up, or ended, or
// its lease lapsed, which it has once its counter stood still for
// AuthorityTakeover.
func (c SharedCluster) AwaitRelease(ctx context.Context, previous AuthorityRecord, liveness *Liveness) error {
	for {
		member, err := c.ReadMember(previous.NodeID)
		if err != nil || member.Incarnation != previous.Incarnation || member.Released >= previous.Epoch ||
			member.Stopped || liveness.StillFor(member) >= AuthorityTakeover {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(HeartbeatInterval / 2):
		}
	}
}

// PruneEpochs removes the state and the claims of the epochs before keep,
// the epoch a holder made its own epoch's state from. The state of keep
// itself remains: its holder may still write it, if only what it will never
// acknowledge.
func (c SharedCluster) PruneEpochs(keep uint64) error {
	entries, err := os.ReadDir(c.claimsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		epoch, err := strconv.ParseUint(entry.Name(), 10, 64)
		if err != nil || epoch >= keep {
			continue
		}
		result = errors.Join(result, os.RemoveAll(filepath.Join(c.authorityDir(), epochName(epoch))))
		if err := os.Remove(c.claimPath(epoch)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

// ReadMembers returns the member records of the cluster, by node.
func (c SharedCluster) ReadMembers() (map[string]MemberRecord, error) {
	entries, err := os.ReadDir(c.membersDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("listing cluster members: %w", err)
	}
	members := make(map[string]MemberRecord, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		record, err := c.ReadMember(entry.Name())
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		members[record.NodeID] = record
	}
	return members, nil
}

// ReadMember returns the member record of nodeID.
func (c SharedCluster) ReadMember(nodeID string) (MemberRecord, error) {
	var record MemberRecord
	if err := c.readBlock(c.memberPath(nodeID), &record); err != nil {
		return MemberRecord{}, fmt.Errorf("reading the member record of %s: %w", nodeID, err)
	}
	return record, nil
}

// Heartbeat advances a daemon's counter in its member record every
// HeartbeatInterval, and keeps its lease.
type Heartbeat struct {
	cluster SharedCluster
	mu      sync.Mutex
	record  MemberRecord
	// landed is when the last heartbeat landed, by this host's monotonic
	// clock.
	landed time.Time
	stop   chan struct{}
	done   chan struct{}
}

// StartHeartbeat starts the heartbeat of the daemon of record, once its first
// one has landed.
func StartHeartbeat(cluster SharedCluster, record MemberRecord) (*Heartbeat, error) {
	if !transactionIDPattern.MatchString(record.NodeID) {
		return nil, fmt.Errorf("invalid cluster node ID %q", record.NodeID)
	}
	if err := os.MkdirAll(cluster.membersDir(), 0700); err != nil {
		return nil, err
	}
	// Continue the counter of the node's last daemon, so that one that
	// watched it sees it advance.
	if previous, err := cluster.ReadMember(record.NodeID); err == nil {
		record.Counter = previous.Counter
	}
	h := &Heartbeat{cluster: cluster, record: record, stop: make(chan struct{}), done: make(chan struct{})}
	if err := h.beat(); err != nil {
		return nil, err
	}
	go h.run()
	return h, nil
}

func (h *Heartbeat) run() {
	defer close(h.done)
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			_ = h.beat()
		}
	}
}

func (h *Heartbeat) beat() error {
	h.mu.Lock()
	h.record.Counter++
	h.record.UpdatedAt = time.Now().UTC()
	record := h.record
	h.mu.Unlock()
	started := time.Now()
	if err := h.cluster.writeBlock(h.cluster.memberPath(record.NodeID), record); err != nil {
		return err
	}
	h.mu.Lock()
	// The lease runs from when the write was sent: it may have landed as
	// soon as that.
	if started.After(h.landed) {
		h.landed = started
	}
	h.mu.Unlock()
	return nil
}

// Held reports whether the daemon holds its lease.
func (h *Heartbeat) Held() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return time.Since(h.landed) < LeaseHold
}

// Update changes the member record, from the next heartbeat on, or now if
// now is set.
func (h *Heartbeat) Update(change func(*MemberRecord), now bool) error {
	h.mu.Lock()
	change(&h.record)
	h.mu.Unlock()
	if now {
		return h.beat()
	}
	return nil
}

// Record returns the member record as the last heartbeat wrote it.
func (h *Heartbeat) Record() MemberRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.record
}

// Leave stops the heartbeat of a daemon that stops cleanly, and records that
// it stopped.
func (h *Heartbeat) Leave() error {
	h.Stop()
	h.mu.Lock()
	h.record.Stopped = true
	h.mu.Unlock()
	return h.beat()
}

// Stop stops the heartbeat. The lease lapses LeaseHold later.
func (h *Heartbeat) Stop() {
	select {
	case <-h.stop:
	default:
		close(h.stop)
	}
	<-h.done
}

// Liveness judges whether daemons are alive by whether their counters
// advance, on this host's clock.
type Liveness struct {
	mu   sync.Mutex
	seen map[string]livenessObservation
	// waiting has, by the control daemon's record it names, since when a
	// compute daemon has waited for a control daemon to give up the
	// authority; see DecideAuthority.
	waiting map[AuthorityRecord]time.Time
}

type livenessObservation struct {
	incarnation string
	counter     uint64
	since       time.Time
	// advanced is set once the counter changed while observed.
	advanced bool
}

// StillFor returns for how long, on this host's clock, the counter of the
// daemon of record has stood still, as far as this Liveness has seen.
func (l *Liveness) StillFor(record MemberRecord) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.observe(record)
	return now.Sub(l.seen[record.NodeID].since)
}

// Advancing reports whether this Liveness has seen the counter of the daemon
// of record advance, and not stand still since for AuthorityTakeover, nor the
// daemon stop: whether the daemon runs, where StillFor tells only for how
// long it has not.
func (l *Liveness) Advancing(record MemberRecord) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.observe(record)
	seen := l.seen[record.NodeID]
	return seen.advanced && now.Sub(seen.since) < AuthorityTakeover && !record.Stopped
}

// Stillness is what a Liveness saw of a daemon's counter: that it stood at
// Counter for Still, and whether it had been seen to advance before.
type Stillness struct {
	NodeID      string        `json:"node_id"`
	Incarnation string        `json:"incarnation"`
	Counter     uint64        `json:"counter"`
	Still       time.Duration `json:"still"`
	Advanced    bool          `json:"advanced,omitempty"`
}

// Stillness returns what this Liveness saw of the counter of the daemon of
// record, for another Liveness on this host to Assume.
func (l *Liveness) Stillness(record MemberRecord) Stillness {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.observe(record)
	seen := l.seen[record.NodeID]
	return Stillness{NodeID: record.NodeID, Incarnation: seen.incarnation, Counter: seen.counter, Still: now.Sub(seen.since), Advanced: seen.advanced}
}

// Assume takes what another Liveness on this host saw of a daemon's counter
// for what this one saw, if it saw nothing of it yet: the counter has stood
// still since at least s.Still ago.
func (l *Liveness) Assume(s Stillness) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = make(map[string]livenessObservation)
	}
	if _, ok := l.seen[s.NodeID]; ok {
		return
	}
	l.seen[s.NodeID] = livenessObservation{incarnation: s.Incarnation, counter: s.Counter, since: time.Now().Add(-s.Still), advanced: s.Advanced}
}

// observe records the counter of record, and returns when it did.
func (l *Liveness) observe(record MemberRecord) time.Time {
	if l.seen == nil {
		l.seen = make(map[string]livenessObservation)
	}
	now := time.Now()
	seen, ok := l.seen[record.NodeID]
	switch {
	case !ok || seen.incarnation != record.Incarnation:
		l.seen[record.NodeID] = livenessObservation{incarnation: record.Incarnation, counter: record.Counter, since: now}
	case seen.counter != record.Counter:
		l.seen[record.NodeID] = livenessObservation{incarnation: record.Incarnation, counter: record.Counter, since: now, advanced: true}
	}
	return now
}

// waitedFor returns for how long a compute daemon has waited for the control
// daemon of record to give up the authority, from now if it had not.
func (l *Liveness) waitedFor(record AuthorityRecord) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.waiting == nil {
		l.waiting = make(map[AuthorityRecord]time.Time)
	}
	since, ok := l.waiting[record]
	if !ok {
		since = time.Now()
		l.waiting[record] = since
	}
	return time.Since(since)
}

// heldReadRetry is how long AuthorityHolder.Held waits to read the authority
// record again after a read failed.
const heldReadRetry = 50 * time.Millisecond

// ErrAuthorityLost is the error of a write of the log of an authority that is
// no longer held.
var ErrAuthorityLost = errors.New("this node no longer holds the cluster authority")

// AuthorityHolder checks, for the node that holds the authority of an epoch,
// that it still does.
type AuthorityHolder struct {
	Cluster   SharedCluster
	Heartbeat *Heartbeat
	Record    AuthorityRecord
}

// Held returns nil if the node still holds its epoch's authority: it holds
// its lease, and no later epoch's record replaced its own. A record of an
// earlier epoch, which a node that claimed that epoch and then stalled wrote
// over it, it restores. A read of the record that fails proves nothing: it
// reads again, for as long as the lease holds. And the lease must still hold
// once a read returns: a read the server answered before a successor
// recorded its epoch may return long after, once the successor holds the
// authority.
func (h AuthorityHolder) Held() error {
	var current AuthorityRecord
	var ok bool
	for {
		if !h.Heartbeat.Held() {
			return fmt.Errorf("%w: its lease lapsed", ErrAuthorityLost)
		}
		var err error
		current, ok, err = h.Cluster.ReadAuthority()
		if !h.Heartbeat.Held() {
			return fmt.Errorf("%w: its lease lapsed", ErrAuthorityLost)
		}
		if err == nil {
			break
		}
		time.Sleep(heldReadRetry)
	}
	if ok && current.Epoch > h.Record.Epoch {
		return fmt.Errorf("%w: node %s took it for epoch %d", ErrAuthorityLost, current.NodeID, current.Epoch)
	}
	if ok && current.Epoch == h.Record.Epoch && (current.NodeID != h.Record.NodeID || current.Incarnation != h.Record.Incarnation) {
		return fmt.Errorf("%w: node %s recorded epoch %d as its own", ErrAuthorityLost, current.NodeID, current.Epoch)
	}
	if !ok || current.Epoch < h.Record.Epoch {
		if err := h.Cluster.WriteAuthority(h.Record); err != nil {
			return fmt.Errorf("%w: %v", ErrAuthorityLost, err)
		}
	}
	return nil
}

// StateSource returns the epoch whose state makes that of an epoch taken
// from previous: previous, or, if its claimant ended before it made
// previous's state, the epoch previous's would have been made from. No
// holder acknowledged anything in an epoch whose state was never made.
func (c SharedCluster) StateSource(previous uint64) (uint64, error) {
	for previous != 0 && !c.Prepared(previous) {
		claim, err := c.ReadClaim(previous)
		if err != nil {
			return 0, fmt.Errorf("finding the state of epoch %d: %w", previous, err)
		}
		if claim.Previous >= previous {
			return 0, fmt.Errorf("the claim of epoch %d takes it from epoch %d", previous, claim.Previous)
		}
		previous = claim.Previous
	}
	return previous, nil
}

// Prepared reports whether the state of epoch's authority was made.
func (c SharedCluster) Prepared(epoch uint64) bool {
	_, err := os.Stat(c.preparedPath(epoch))
	return err == nil
}

func (c SharedCluster) preparedPath(epoch uint64) string {
	return filepath.Join(c.EpochDir(epoch), "prepared")
}

// PrepareEpoch makes the state of the authority of epoch from the state of
// the authority of source, a previous epoch, or, with source 0, a new
// cluster's. It reads source's state and changes nothing of it: its holder
// may still be writing it, if only what it will never acknowledge.
func (c SharedCluster) PrepareEpoch(source, epoch uint64) (err error) {
	if c.Prepared(epoch) {
		return fmt.Errorf("the state of epoch %d was already made", epoch)
	}
	dir := c.EpochDir(epoch)
	// A claimant that stalled and lost its claim may have left part of this
	// directory; the claim is this node's now.
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dir)
		}
	}()
	sourceDir := ""
	if source != 0 {
		sourceDir = c.EpochDir(source)
	}
	if err := prepareAuthorityEpoch(dir, epoch, sourceDir, source); err != nil {
		return fmt.Errorf("making the state of epoch %d: %w", epoch, err)
	}
	return c.markPrepared(epoch)
}

// markPrepared records that the state of epoch was made: from then on its
// authority may write it, and it is never made again.
func (c SharedCluster) markPrepared(epoch uint64) error {
	if err := syncDir(c.EpochDir(epoch)); err != nil {
		return err
	}
	file, err := os.OpenFile(c.preparedPath(epoch), os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = file.Sync()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return syncDir(c.EpochDir(epoch))
}

func syncDir(dir string) error {
	file, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = file.Sync()
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// A shared block is a record, JSON-encoded, in the first sharedBlockSize
// bytes of its file: its length and its CRC-32C, four bytes each, then the
// JSON. The file is written in place, so that a reader never opens a file that
// was replaced; the checksum tells a torn write from a whole one.
const sharedBlockSize = 4096

var sharedBlockTable = crc32.MakeTable(crc32.Castagnoli)

// writeSharedBlock writes value as the block of path, with direct I/O where
// the filesystem allows it, and syncs it.
func writeSharedBlock(path string, value any) error {
	return (*sharedFiles)(nil).use(path, unix.O_WRONLY|unix.O_CREAT, func(fd int) error { return writeSharedBlockTo(fd, path, value) })
}

// writeSharedBlockTo is writeSharedBlock to the file of path open as fd.
func writeSharedBlockTo(fd int, path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > sharedBlockSize-8 {
		return fmt.Errorf("a record of %d bytes does not fit its %d-byte block", len(data), sharedBlockSize)
	}
	block, release, err := alignedBlock()
	if err != nil {
		return err
	}
	defer release()
	binary.LittleEndian.PutUint32(block[0:], uint32(len(data)))
	binary.LittleEndian.PutUint32(block[4:], crc32.Checksum(data, sharedBlockTable))
	copy(block[8:], data)
	if _, err := unix.Pwrite(fd, block, 0); err != nil {
		return &os.PathError{Op: "write", Path: path, Err: err}
	}
	if err := unix.Fsync(fd); err != nil {
		return &os.PathError{Op: "fsync", Path: path, Err: err}
	}
	return nil
}

// readSharedBlock reads the block of path into value. A torn block, which a
// write in progress leaves, it reads again.
func readSharedBlock(path string, value any) error {
	return (*sharedFiles)(nil).use(path, unix.O_RDONLY, func(fd int) error { return readSharedBlockFrom(fd, path, value) })
}

// readSharedBlockFrom is readSharedBlock from the file of path open as fd.
func readSharedBlockFrom(fd int, path string, value any) error {
	block, release, err := alignedBlock()
	if err != nil {
		return err
	}
	defer release()
	for attempt := 0; ; attempt++ {
		n, err := unix.Pread(fd, block, 0)
		if err != nil {
			return &os.PathError{Op: "read", Path: path, Err: err}
		}
		if n >= 8 {
			length := binary.LittleEndian.Uint32(block[0:])
			if length <= sharedBlockSize-8 && int(length)+8 <= n {
				data := block[8 : 8+length]
				if crc32.Checksum(data, sharedBlockTable) == binary.LittleEndian.Uint32(block[4:]) {
					return json.Unmarshal(data, value)
				}
			}
		}
		if attempt == 20 {
			return fmt.Errorf("%s holds no whole record", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// sharedFiles keeps the files of records open, each by its path and the
// flags it was opened with.
type sharedFiles struct {
	mu     sync.Mutex
	files  map[sharedFileKey]*sharedFile
	closed bool
}

type sharedFileKey struct {
	path  string
	flags int
}

type sharedFile struct {
	mu     sync.Mutex
	fd     int
	open   bool
	closed bool
}

// use calls do with the file of path open with flags: a file f keeps open, or,
// if f is nil or closed, one it opens for do and closes after it. A file that
// do failed with is closed, and opened again for the next.
func (f *sharedFiles) use(path string, flags int, do func(fd int) error) error {
	var file *sharedFile
	if f != nil {
		f.mu.Lock()
		if !f.closed {
			key := sharedFileKey{path: path, flags: flags}
			if file = f.files[key]; file == nil {
				file = &sharedFile{}
				f.files[key] = file
			}
		}
		f.mu.Unlock()
	}
	if file == nil {
		fd, err := openShared(path, flags)
		if err != nil {
			return err
		}
		defer unix.Close(fd)
		return do(fd)
	}
	file.mu.Lock()
	defer file.mu.Unlock()
	if !file.open {
		fd, err := openShared(path, flags)
		if err != nil {
			return err
		}
		file.fd, file.open = fd, true
	}
	err := do(file.fd)
	if err != nil || file.closed {
		_ = unix.Close(file.fd)
		file.open = false
	}
	return err
}

func (f *sharedFiles) close() {
	f.mu.Lock()
	files := f.files
	f.files, f.closed = nil, true
	f.mu.Unlock()
	for _, file := range files {
		file.mu.Lock()
		if file.open {
			_ = unix.Close(file.fd)
			file.open = false
		}
		file.closed = true
		file.mu.Unlock()
	}
}

// alignedBlock returns a block that direct I/O can use, and what releases it.
func alignedBlock() ([]byte, func(), error) {
	block, err := unix.Mmap(-1, 0, sharedBlockSize, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_ANON|unix.MAP_PRIVATE)
	if err != nil {
		return nil, nil, err
	}
	return block, func() { _ = unix.Munmap(block) }, nil
}
