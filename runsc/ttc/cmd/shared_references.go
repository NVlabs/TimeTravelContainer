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

package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

// sharedReferenceService serializes exact shared-origin reference mutations
// through the daemon holding Raft's shared-GC lease. The ledger itself is on
// TTC_ROOT and exclusively locked, so lease turnover cannot create concurrent
// writers even if one daemon notices expiry late.
type sharedReferenceService struct {
	mu          sync.RWMutex
	authority   *cluster.RaftCatalog
	packs       *ttcsnapshot.SharedPackStore
	directory   string
	nodeID      string
	incarnation string
	graph       *ttcsnapshot.Store
	peers       *distributedSourceRuntime
	lease       cluster.SharedGCLease
	ledger      *ttcsnapshot.ReferenceLedger
}

func newSharedReferenceService(sharedRoot, nodeID, incarnation string, authority *cluster.RaftCatalog, packs *ttcsnapshot.SharedPackStore, graph *ttcsnapshot.Store, peers *distributedSourceRuntime) *sharedReferenceService {
	return &sharedReferenceService{
		authority: authority, packs: packs, nodeID: nodeID, incarnation: incarnation,
		directory: filepath.Join(sharedRoot, "snapshot-graph", "shared-reference-ledger"),
		graph:     graph, peers: peers,
	}
}

func (s *sharedReferenceService) drainOutbox(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	delivered := 0
	for delivered < limit {
		var outbound *ttcsnapshot.OutboundReference
		if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
			pending := ledger.PendingOutbound()
			if len(pending) != 0 {
				item := pending[0]
				outbound = &item
			}
			return nil
		}); err != nil {
			return delivered, err
		}
		if outbound == nil {
			return delivered, nil
		}
		provider, volatile, err := s.authority.SnapshotProvider(uint64(outbound.Child.Snapshot))
		if err != nil {
			return delivered, err
		}
		if !volatile {
			err = s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
				if outbound.Retain && ledger.OwnsEdge(outbound.Parent, outboundSlot(outbound.Name), outbound.Child) {
					return nil
				}
				return ledger.ApplyReference(outbound.Operation, outbound.Name, outbound.Child, outbound.Retain)
			})
		} else if provider.NodeID == s.nodeID {
			bridge := map[string]ttcsnapshot.ObjectID{"bridge/" + outbound.Name: outbound.Child}
			if outbound.Retain {
				err = s.graph.RetainRoots(bridge)
			} else {
				err = s.graph.ReleaseRetainedRoots(bridge)
			}
		} else {
			if s.peers == nil {
				return delivered, errors.New("cluster peer routing is unavailable")
			}
			peer, peerErr := s.peers.peer(provider.NodeID, false)
			if peerErr != nil {
				return delivered, peerErr
			}
			wireOperation := fmt.Sprintf("edge-%016x-%016x", uint64(outbound.Parent.Snapshot), outbound.Sequence)
			err = peer.ApplySnapshotReference(ctx, uint64(outbound.Child.Snapshot), wireOperation, outbound.Name, outbound.Child.String(), outbound.Retain)
		}
		if err != nil {
			return delivered, err
		}
		if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
			return ledger.AcknowledgeOutbound(outbound.Sequence)
		}); err != nil {
			return delivered, err
		}
		delivered++
	}
	return delivered, nil
}

func outboundSlot(name string) uint16 {
	index := strings.LastIndexByte(name, '/')
	if index < 0 {
		return 0
	}
	value, err := strconv.ParseUint(name[index+1:], 16, 16)
	if err != nil {
		return 0
	}
	return uint16(value)
}

func (s *sharedReferenceService) activate(lease cluster.SharedGCLease) error {
	if s == nil || lease.Generation == 0 || lease.NodeID != s.nodeID || lease.Incarnation != s.incarnation || !time.Now().Before(lease.ExpiresAt) {
		return errors.New("invalid shared reference coordinator lease")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ledger != nil && s.lease.Generation == lease.Generation {
		s.lease = lease
		return nil
	}
	if s.ledger != nil {
		_ = s.ledger.Close()
		s.ledger = nil
		s.lease = cluster.SharedGCLease{}
	}
	ledger, err := ttcsnapshot.OpenReferenceLedger(s.directory)
	if err != nil {
		return err
	}
	current, err := s.authority.SharedGCLease()
	if err != nil || current.NodeID != lease.NodeID || current.Incarnation != lease.Incarnation || current.Generation != lease.Generation || !time.Now().Before(current.ExpiresAt) {
		ledger.Close()
		if err != nil {
			return err
		}
		return errors.New("shared reference coordinator lease changed while opening ledger")
	}
	s.ledger, s.lease = ledger, current
	return nil
}

func (s *sharedReferenceService) deactivate() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.ledger != nil {
		err = s.ledger.Close()
	}
	s.ledger = nil
	s.lease = cluster.SharedGCLease{}
	return err
}

func (s *sharedReferenceService) withLedger(fn func(*ttcsnapshot.ReferenceLedger) error) error {
	if s == nil || fn == nil {
		return errors.New("shared reference coordinator is unavailable")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.ledger == nil || s.lease.Generation == 0 || !time.Now().Before(s.lease.ExpiresAt) {
		return errors.New("this daemon does not hold an active shared reference lease")
	}
	current, err := s.authority.SharedGCLease()
	if err != nil {
		return err
	}
	if current.NodeID != s.lease.NodeID || current.Incarnation != s.lease.Incarnation || current.Generation != s.lease.Generation || !time.Now().Before(current.ExpiresAt) {
		return errors.New("shared reference lease is stale")
	}
	return fn(s.ledger)
}

func (s *sharedReferenceService) authorizeProvider(peer string, snapshot uint64) error {
	provider, volatile, err := s.authority.SnapshotProvider(snapshot)
	if err != nil {
		return err
	}
	if !volatile || provider.NodeID != peer {
		return fmt.Errorf("node %q is not the volatile provider for snapshot %d", peer, snapshot)
	}
	return nil
}

func (s *sharedReferenceService) ImportSharedSnapshot(_ context.Context, peer string, snapshotID uint64) error {
	if err := s.authorizeProvider(peer, snapshotID); err != nil {
		return err
	}
	manifest, err := s.packs.Manifest(ttcsnapshot.SnapshotID(snapshotID))
	if err != nil {
		return err
	}
	objects := make([]ttcsnapshot.Object, len(manifest.Objects))
	for index, packed := range manifest.Objects {
		objects[index] = packed.Object
	}
	return s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		return ledger.ImportOrigin(ttcsnapshot.SnapshotID(snapshotID), objects, manifest.References)
	})
}

func (s *sharedReferenceService) ApplySharedSnapshotHandoff(_ context.Context, peer string, snapshotID, from uint64, wire []cluster.SharedReferenceMutation) error {
	if err := s.authorizeProvider(peer, snapshotID); err != nil {
		return err
	}
	mutations := make([]ttcsnapshot.SnapshotReferenceMutation, len(wire))
	for index, input := range wire {
		object, err := ttcsnapshot.ParseObjectID(input.Object)
		if err != nil {
			return err
		}
		mutations[index] = ttcsnapshot.SnapshotReferenceMutation{
			Sequence: input.Sequence, Operation: input.Operation, Name: input.Name, Object: object, Retain: input.Retain,
		}
	}
	return s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		return ledger.ApplyHandoff(ttcsnapshot.SnapshotID(snapshotID), from, mutations)
	})
}

func (s *sharedReferenceService) FlushSharedSnapshot(ctx context.Context, peer string, snapshotID uint64) error {
	if err := s.authorizeProvider(peer, snapshotID); err != nil {
		return err
	}
	for {
		pendingOrigin := false
		if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
			for _, outbound := range ledger.PendingOutbound() {
				if uint64(outbound.Parent.Snapshot) == snapshotID {
					pendingOrigin = true
					break
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if !pendingOrigin {
			return nil
		}
		delivered, err := s.drainOutbox(ctx, 4096)
		if err != nil {
			return err
		}
		if delivered == 0 {
			return fmt.Errorf("snapshot %d outgoing references made no progress", snapshotID)
		}
	}
}

// mutateRoots applies ordinary branch lifetime references after Raft routes an
// origin to shared storage. A partial response is safe: each stable slot has
// an independent idempotency identity and the caller retries the group.
func (s *sharedReferenceService) mutateRoots(snapshot ttcsnapshot.SnapshotID, operation string, bindings []cluster.SnapshotRootBinding, retain bool) error {
	roots, err := graphReferenceRoots(snapshot, operation, bindings)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(roots))
	for name := range roots {
		names = append(names, name)
	}
	sort.Strings(names)
	action := "release"
	if retain {
		action = "retain"
	}
	return s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		for _, name := range names {
			id := roots[name]
			idempotency := fmt.Sprintf("shared-%s/%s", action, name)
			if err := ledger.ApplyReference(idempotency, name, id, retain); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *sharedReferenceService) mutateReference(operation, name string, object ttcsnapshot.ObjectID, retain bool) error {
	return s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		return ledger.ApplyReference(operation, name, object, retain)
	})
}

var _ cluster.SharedSnapshotReferenceProvider = (*sharedReferenceService)(nil)
