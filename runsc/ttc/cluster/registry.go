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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const maxNodeRecordBytes = 1 << 20

// NodeState is one elastic-cluster membership state.
type NodeState string

const (
	NodeJoining   NodeState = "joining"
	NodeReady     NodeState = "ready"
	NodePressured NodeState = "pressured"
	NodeDraining  NodeState = "draining"
	NodeOffline   NodeState = "offline"
)

var nodeTransitions = map[NodeState]map[NodeState]bool{
	NodeJoining:   {NodeReady: true, NodeDraining: true, NodeOffline: true},
	NodeReady:     {NodePressured: true, NodeDraining: true, NodeOffline: true},
	NodePressured: {NodeReady: true, NodeDraining: true, NodeOffline: true},
	NodeDraining:  {NodeOffline: true},
}

// NodeRecord is the shared discovery record for one ttcd incarnation.
// Placement ownership uses a separate, more strongly fenced catalog.
type NodeRecord struct {
	NodeID                string                         `json:"node_id"`
	Incarnation           string                         `json:"incarnation"`
	State                 NodeState                      `json:"state"`
	ControlURL            string                         `json:"control_url"`
	ControlServerName     string                         `json:"control_server_name"`
	RaftAddress           string                         `json:"raft_address,omitempty"`
	Capabilities          Capabilities                   `json:"capabilities"`
	MemoryTargetBytes     uint64                         `json:"memory_target_bytes,omitempty"`
	MemoryCurrentBytes    uint64                         `json:"memory_current_bytes,omitempty"`
	IncomingReservedBytes uint64                         `json:"incoming_reserved_bytes,omitempty"`
	IncomingReservations  map[string]IncomingReservation `json:"incoming_reservations,omitempty"`
	Labels                map[string]string              `json:"labels,omitempty"`
	Revision              uint64                         `json:"revision"`
	StartedAt             time.Time                      `json:"started_at"`
	UpdatedAt             time.Time                      `json:"updated_at"`
	LeaseExpiresAt        time.Time                      `json:"lease_expires_at"`
	LeaseDuration         time.Duration                  `json:"lease_duration,omitempty"`
	// LeaseTerm proves that this incarnation renewed with the current Raft
	// leader. A prior-term wall-clock deadline alone never authorizes work.
	LeaseTerm uint64 `json:"lease_term,omitempty"`
}

// IncomingReservation fences capacity promised to one placement operation.
// It expires automatically so a failed source cannot leak target capacity.
type IncomingReservation struct {
	SourceNode       string    `json:"source_node"`
	Bytes            uint64    `json:"bytes"`
	AccountedAtBytes uint64    `json:"accounted_at_bytes"`
	CompletedAt      time.Time `json:"completed_at,omitempty"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// LeaseActive reports whether the record may be used for discovery at now.
func (r NodeRecord) LeaseActive(now time.Time) bool {
	return r.State != NodeOffline && now.Before(r.LeaseExpiresAt)
}

// AcceptsPlacement reports whether a scheduler may reserve new work here.
func (r NodeRecord) AcceptsPlacement(now time.Time) bool {
	return r.LeaseActive(now) && r.State == NodeReady
}

// NewNodeIncarnation returns an unpredictable identifier that fences daemon
// restarts which reuse the same human-facing node ID.
func NewNodeIncarnation() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generating node incarnation: %w", err)
	}
	return hex.EncodeToString(value[:]), nil
}

// Validate rejects malformed or unbounded shared-directory input.
func (r NodeRecord) Validate() error {
	if !transactionIDPattern.MatchString(r.NodeID) {
		return fmt.Errorf("invalid node ID %q", r.NodeID)
	}
	if !transactionIDPattern.MatchString(r.Incarnation) {
		return fmt.Errorf("invalid node incarnation %q", r.Incarnation)
	}
	if _, ok := nodeTransitions[r.State]; !ok && r.State != NodeOffline {
		return fmt.Errorf("invalid node state %q", r.State)
	}
	if err := r.Capabilities.Validate(); err != nil {
		return fmt.Errorf("node capabilities: %w", err)
	}
	if r.Capabilities.NodeID != r.NodeID {
		return fmt.Errorf("capability node %q does not match record node %q", r.Capabilities.NodeID, r.NodeID)
	}
	parsed, err := url.Parse(r.ControlURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("invalid node control URL %q", r.ControlURL)
	}
	if parsed.Scheme == "https" && (strings.TrimSpace(r.ControlServerName) == "" || strings.TrimSpace(r.ControlServerName) != r.ControlServerName) {
		return fmt.Errorf("invalid control TLS server name %q", r.ControlServerName)
	}
	if r.RaftAddress != "" {
		if strings.Contains(r.RaftAddress, "@") {
			if _, _, err := parseRaftAddress(r.RaftAddress); err != nil {
				return fmt.Errorf("invalid node Raft address: %w", err)
			}
		} else if _, _, err := net.SplitHostPort(r.RaftAddress); err != nil {
			return fmt.Errorf("invalid node Raft address: %w", err)
		}
	}
	if r.Revision == 0 || r.LeaseDuration <= 0 || r.StartedAt.IsZero() || r.UpdatedAt.Before(r.StartedAt) || r.LeaseExpiresAt.Before(r.UpdatedAt) {
		return errors.New("invalid node revision or lease timestamps")
	}
	if len(r.Labels) > 64 {
		return fmt.Errorf("node has %d labels, limit is 64", len(r.Labels))
	}
	for key, value := range r.Labels {
		if key == "" || len(key) > 128 || len(value) > 1024 || strings.TrimSpace(key) != key {
			return fmt.Errorf("invalid node label %q", key)
		}
	}
	if len(r.IncomingReservations) > 1024 {
		return fmt.Errorf("node has %d incoming reservations, limit is 1024", len(r.IncomingReservations))
	}
	var reserved uint64
	for operation, reservation := range r.IncomingReservations {
		if !transactionIDPattern.MatchString(operation) || !transactionIDPattern.MatchString(reservation.SourceNode) || reservation.Bytes == 0 || reservation.AccountedAtBytes == 0 || reservation.ExpiresAt.IsZero() || (!reservation.CompletedAt.IsZero() && reservation.CompletedAt.After(reservation.ExpiresAt)) {
			return fmt.Errorf("invalid incoming reservation %q", operation)
		}
		if reserved > math.MaxUint64-reservation.Bytes {
			return errors.New("incoming reservation byte total overflows")
		}
		reserved += reservation.Bytes
	}
	if len(r.IncomingReservations) != 0 && reserved != r.IncomingReservedBytes {
		return fmt.Errorf("incoming reservation total is %d, recorded total is %d", reserved, r.IncomingReservedBytes)
	}
	return nil
}

// NodeDirectory stores dynamic discovery records in a shared TTC root.
// Per-node advisory locks serialize competing incarnations on NFS.
type NodeDirectory struct {
	dir string
}

// NodeCatalog is the linearizable membership and capacity authority consumed
// by elastic placement. NodeDirectory is retained only for standalone tests
// and old-format rejection; clustered TTC uses the Raft implementation.
type NodeCatalog interface {
	Register(NodeRecord, time.Time, time.Duration) (*NodeRecord, error)
	Heartbeat(nodeID, incarnation string, current uint64, now time.Time, lease time.Duration) (*NodeRecord, error)
	ReserveIncoming(nodeID, incarnation, operation, sourceNode string, bytes uint64, now time.Time, ttl time.Duration) (*NodeRecord, error)
	ReleaseIncoming(nodeID, incarnation, operation string, now time.Time) (*NodeRecord, error)
	CompleteIncoming(nodeID, incarnation, operation string, now time.Time) (*NodeRecord, error)
	Transition(nodeID, incarnation string, next NodeState, now time.Time, lease time.Duration) (*NodeRecord, error)
	Lookup(nodeID string) (*NodeRecord, error)
	List() ([]NodeRecord, error)
}

// NodeExpiryCatalog commits the consequences of one exact expired execution
// lease. It is separate from NodeCatalog because the legacy filesystem
// directory is not a distributed fencing authority.
type NodeExpiryCatalog interface {
	ExpireNode(nodeID, incarnation string, revision uint64) (*NodeRecord, error)
}

var _ NodeCatalog = (*NodeDirectory)(nil)

// OpenNodeDirectory opens or creates a private shared discovery directory.
func OpenNodeDirectory(dir string) (*NodeDirectory, error) {
	if dir == "" {
		return nil, errors.New("cluster node directory is empty")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("creating cluster node directory: %w", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("cluster node directory %q is not private", dir)
	}
	return &NodeDirectory{dir: dir}, nil
}

func (d *NodeDirectory) recordPath(nodeID string) (string, error) {
	if !transactionIDPattern.MatchString(nodeID) {
		return "", fmt.Errorf("invalid node ID %q", nodeID)
	}
	return filepath.Join(d.dir, nodeID+".json"), nil
}

func (d *NodeDirectory) withLock(nodeID string, fn func() error) error {
	if _, err := d.recordPath(nodeID); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(d.dir, nodeID+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("locking node %q: %w", nodeID, err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func validateLease(now time.Time, lease time.Duration) error {
	if now.IsZero() || lease < time.Second || lease > 10*time.Minute {
		return fmt.Errorf("invalid node lease %s", lease)
	}
	return nil
}

// Register claims a node ID for one daemon incarnation. An unexpired different
// incarnation is never overwritten.
func (d *NodeDirectory) Register(record NodeRecord, now time.Time, lease time.Duration) (*NodeRecord, error) {
	if err := validateLease(now, lease); err != nil {
		return nil, err
	}
	record.State = NodeJoining
	record.Revision = 1
	record.StartedAt = now
	record.UpdatedAt = now
	record.LeaseExpiresAt = now.Add(lease)
	record.LeaseDuration = lease
	if err := record.Validate(); err != nil {
		return nil, err
	}
	var result NodeRecord
	err := d.withLock(record.NodeID, func() error {
		current, err := d.load(record.NodeID)
		switch {
		case err == nil && current.Incarnation != record.Incarnation && current.LeaseActive(now):
			return fmt.Errorf("node %q is owned by live incarnation %q until %s", record.NodeID, current.Incarnation, current.LeaseExpiresAt.Format(time.RFC3339Nano))
		case err == nil:
			record.Revision = current.Revision + 1
			if current.Incarnation == record.Incarnation {
				record.StartedAt = current.StartedAt
				record.State = current.State
				record.IncomingReservedBytes = current.IncomingReservedBytes
				record.IncomingReservations = current.IncomingReservations
			}
		case !errors.Is(err, os.ErrNotExist):
			return err
		}
		if err := d.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Heartbeat renews one live incarnation and updates its load report. Expired
// incarnations must register again instead of silently resurrecting.
func (d *NodeDirectory) Heartbeat(nodeID, incarnation string, current uint64, now time.Time, lease time.Duration) (*NodeRecord, error) {
	if err := validateLease(now, lease); err != nil {
		return nil, err
	}
	var result NodeRecord
	err := d.withLock(nodeID, func() error {
		record, err := d.load(nodeID)
		if err != nil {
			return err
		}
		if record.Incarnation != incarnation {
			return fmt.Errorf("node %q incarnation changed from %q to %q", nodeID, incarnation, record.Incarnation)
		}
		if !record.LeaseActive(now) {
			return fmt.Errorf("node %q lease expired at %s", nodeID, record.LeaseExpiresAt.Format(time.RFC3339Nano))
		}
		record.MemoryCurrentBytes = current
		pruneIncomingReservations(&record, now)
		record.Revision++
		record.UpdatedAt = now
		record.LeaseExpiresAt = now.Add(lease)
		record.LeaseDuration = lease
		if err := d.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func pruneIncomingReservations(record *NodeRecord, now time.Time) {
	if len(record.IncomingReservations) == 0 {
		record.IncomingReservations = nil
		record.IncomingReservedBytes = 0
		return
	}
	var total uint64
	for operation, reservation := range record.IncomingReservations {
		if !now.Before(reservation.ExpiresAt) || record.MemoryCurrentBytes >= reservation.AccountedAtBytes || (!reservation.CompletedAt.IsZero() && now.After(reservation.CompletedAt)) {
			delete(record.IncomingReservations, operation)
			continue
		}
		total += reservation.Bytes
	}
	if len(record.IncomingReservations) == 0 {
		record.IncomingReservations = nil
	}
	record.IncomingReservedBytes = total
}

// ReserveIncoming atomically promises target memory to one placement
// operation. Concurrent sources serialize on the target record, and retries
// with the same operation are idempotent.
func (d *NodeDirectory) ReserveIncoming(nodeID, incarnation, operation, sourceNode string, bytes uint64, now time.Time, ttl time.Duration) (*NodeRecord, error) {
	if !transactionIDPattern.MatchString(operation) || !transactionIDPattern.MatchString(sourceNode) || bytes == 0 {
		return nil, errors.New("invalid incoming reservation identity or size")
	}
	if now.IsZero() || ttl < time.Second || ttl > 10*time.Minute {
		return nil, fmt.Errorf("invalid incoming reservation TTL %s", ttl)
	}
	var result NodeRecord
	err := d.withLock(nodeID, func() error {
		record, err := d.load(nodeID)
		if err != nil {
			return err
		}
		if record.Incarnation != incarnation || !record.AcceptsPlacement(now) {
			return fmt.Errorf("node %q incarnation is unavailable for reservation", nodeID)
		}
		pruneIncomingReservations(&record, now)
		if existing, ok := record.IncomingReservations[operation]; ok {
			if existing.SourceNode != sourceNode || existing.Bytes != bytes {
				return fmt.Errorf("incoming reservation %q was reused with different parameters", operation)
			}
			result = record
			return nil
		}
		if len(record.IncomingReservations) >= 1024 {
			return errors.New("target has too many incoming reservations")
		}
		if record.IncomingReservedBytes > math.MaxUint64-bytes || record.MemoryCurrentBytes > math.MaxUint64-record.IncomingReservedBytes-bytes {
			return errors.New("incoming reservation byte total overflows")
		}
		projected := record.MemoryCurrentBytes + record.IncomingReservedBytes + bytes
		if record.MemoryTargetBytes != 0 && projected > record.MemoryTargetBytes {
			return fmt.Errorf("reservation would raise node %q to %d bytes above its %d-byte target", nodeID, projected, record.MemoryTargetBytes)
		}
		if record.IncomingReservations == nil {
			record.IncomingReservations = make(map[string]IncomingReservation)
		}
		record.IncomingReservations[operation] = IncomingReservation{
			SourceNode: sourceNode, Bytes: bytes, AccountedAtBytes: projected, ExpiresAt: now.Add(ttl),
		}
		record.IncomingReservedBytes += bytes
		record.Revision++
		record.UpdatedAt = now
		if err := d.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ReleaseIncoming releases an operation's target reservation. Missing
// reservations are an idempotent success.
func (d *NodeDirectory) ReleaseIncoming(nodeID, incarnation, operation string, now time.Time) (*NodeRecord, error) {
	if !transactionIDPattern.MatchString(operation) || now.IsZero() {
		return nil, errors.New("invalid incoming reservation release")
	}
	var result NodeRecord
	err := d.withLock(nodeID, func() error {
		record, err := d.load(nodeID)
		if err != nil {
			return err
		}
		if record.Incarnation != incarnation {
			return fmt.Errorf("node %q incarnation changed before reservation release", nodeID)
		}
		pruneIncomingReservations(&record, now)
		if _, ok := record.IncomingReservations[operation]; !ok {
			result = record
			return nil
		}
		delete(record.IncomingReservations, operation)
		pruneIncomingReservations(&record, now)
		record.Revision++
		record.UpdatedAt = now
		if err := d.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// CompleteIncoming records that migration mutations have finished. The
// reservation remains charged until a subsequent heartbeat publishes fresh
// target memory, avoiding a stale-load admission window even when actual RSS
// grows by less than the estimate due to cache reuse.
func (d *NodeDirectory) CompleteIncoming(nodeID, incarnation, operation string, now time.Time) (*NodeRecord, error) {
	if !transactionIDPattern.MatchString(operation) || now.IsZero() {
		return nil, errors.New("invalid incoming reservation completion")
	}
	var result NodeRecord
	err := d.withLock(nodeID, func() error {
		record, err := d.load(nodeID)
		if err != nil {
			return err
		}
		if record.Incarnation != incarnation {
			return fmt.Errorf("node %q incarnation changed before reservation completion", nodeID)
		}
		pruneIncomingReservations(&record, now)
		reservation, ok := record.IncomingReservations[operation]
		if !ok {
			result = record
			return nil
		}
		if reservation.CompletedAt.IsZero() {
			reservation.CompletedAt = now
			record.IncomingReservations[operation] = reservation
			record.Revision++
			record.UpdatedAt = now
			if err := d.store(record); err != nil {
				return err
			}
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Transition changes one node's admission state with incarnation fencing.
func (d *NodeDirectory) Transition(nodeID, incarnation string, next NodeState, now time.Time, lease time.Duration) (*NodeRecord, error) {
	if err := validateLease(now, lease); err != nil {
		return nil, err
	}
	var result NodeRecord
	err := d.withLock(nodeID, func() error {
		record, err := d.load(nodeID)
		if err != nil {
			return err
		}
		if record.Incarnation != incarnation {
			return fmt.Errorf("node %q incarnation changed from %q to %q", nodeID, incarnation, record.Incarnation)
		}
		if next != record.State && !nodeTransitions[record.State][next] {
			return fmt.Errorf("illegal node transition %s -> %s", record.State, next)
		}
		record.State = next
		record.Revision++
		record.UpdatedAt = now
		if next == NodeOffline {
			record.LeaseExpiresAt = now
		} else {
			record.LeaseExpiresAt = now.Add(lease)
			record.LeaseDuration = lease
		}
		if err := d.store(record); err != nil {
			return err
		}
		result = record
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// Lookup returns one record, including an expired or offline record.
func (d *NodeDirectory) Lookup(nodeID string) (*NodeRecord, error) {
	record, err := d.load(nodeID)
	if err != nil {
		return nil, err
	}
	return &record, nil
}

// List returns every valid published node record in stable node-ID order.
func (d *NodeDirectory) List() ([]NodeRecord, error) {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return nil, err
	}
	records := make([]NodeRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		nodeID := strings.TrimSuffix(entry.Name(), ".json")
		if !transactionIDPattern.MatchString(nodeID) {
			return nil, fmt.Errorf("invalid node record name %q", entry.Name())
		}
		record, err := d.load(nodeID)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].NodeID < records[j].NodeID })
	return records, nil
}

func (d *NodeDirectory) load(nodeID string) (NodeRecord, error) {
	path, err := d.recordPath(nodeID)
	if err != nil {
		return NodeRecord{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return NodeRecord{}, err
	}
	defer file.Close()
	limited := io.LimitReader(file, maxNodeRecordBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return NodeRecord{}, err
	}
	if len(data) > maxNodeRecordBytes {
		return NodeRecord{}, fmt.Errorf("node record %q exceeds size limit", nodeID)
	}
	var record NodeRecord
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return NodeRecord{}, fmt.Errorf("decoding node record %q: %w", nodeID, err)
	}
	if err := record.Validate(); err != nil {
		return NodeRecord{}, fmt.Errorf("validating node record %q: %w", nodeID, err)
	}
	if record.NodeID != nodeID {
		return NodeRecord{}, fmt.Errorf("node record %q contains identity %q", nodeID, record.NodeID)
	}
	return record, nil
}

func (d *NodeDirectory) store(record NodeRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	path, err := d.recordPath(record.NodeID)
	if err != nil {
		return err
	}
	data, err := json.Marshal(&record)
	if err != nil {
		return err
	}
	if len(data) > maxNodeRecordBytes {
		return errors.New("node record exceeds size limit")
	}
	temp, err := os.CreateTemp(d.dir, ".node-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return syncDirectory(d.dir)
}
