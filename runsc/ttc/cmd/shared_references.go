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
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
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
// through the daemon holding Raft's shared-GC lease. The hot ledger lives only
// on TTC_LOCAL_ROOT and is rebuilt for every new lease generation from shared
// immutable manifests plus each live node's durable bounded root claims.
type sharedReferenceService struct {
	mu           sync.RWMutex
	activationMu sync.Mutex
	claimMu      sync.Mutex
	authority    *cluster.RaftCatalog
	packs        *ttcsnapshot.SharedPackStore
	directory    string
	localRoot    string
	sharedRoot   string
	nodeID       string
	incarnation  string
	graph        *ttcsnapshot.Store
	peers        *distributedSourceRuntime
	lease        cluster.SharedGCLease
	ledger       *ttcsnapshot.ReferenceLedger
}

func newSharedReferenceService(sharedRoot, localRoot, nodeID, incarnation string, authority *cluster.RaftCatalog, packs *ttcsnapshot.SharedPackStore, graph *ttcsnapshot.Store, peers *distributedSourceRuntime) *sharedReferenceService {
	return &sharedReferenceService{
		authority: authority, packs: packs, nodeID: nodeID, incarnation: incarnation,
		directory: filepath.Join(localRoot, "snapshot-graph", "shared-reference-ledger"), localRoot: localRoot, sharedRoot: sharedRoot,
		graph: graph, peers: peers,
	}
}

// SnapshotReferenceInventory returns the locally durable claim set. The
// intent itself reserves every not-yet-released group: it is written before
// provider contact, so recovery remains safe across a lost retain response or
// coordinator turnover in the middle of a multi-root operation.
func (s *sharedReferenceService) SnapshotReferenceInventory(_ context.Context, _ string) (*cluster.SnapshotReferenceInventory, error) {
	if s == nil || s.localRoot == "" {
		return nil, errors.New("snapshot reference inventory is unavailable")
	}
	s.claimMu.Lock()
	defer s.claimMu.Unlock()
	dir := filepath.Join(s.localRoot, "graph-references")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return &cluster.SnapshotReferenceInventory{NodeID: s.nodeID, Incarnation: s.incarnation}, nil
	}
	if err != nil {
		return nil, err
	}
	var claims []cluster.SnapshotReferenceClaim
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		intent, err := readGraphReferenceIntent(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading reference claim %q: %w", entry.Name(), err)
		}
		for _, group := range intent.Groups {
			if group.Released {
				continue
			}
			claims = append(claims, cluster.SnapshotReferenceClaim{
				Origin: group.Origin, Operation: intent.Operation,
				Roots: append([]cluster.SnapshotRootBinding(nil), group.Roots...),
			})
		}
	}
	sort.Slice(claims, func(i, j int) bool {
		if claims[i].Origin != claims[j].Origin {
			return claims[i].Origin < claims[j].Origin
		}
		return claims[i].Operation < claims[j].Operation
	})
	return &cluster.SnapshotReferenceInventory{NodeID: s.nodeID, Incarnation: s.incarnation, Claims: claims}, nil
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

func (s *sharedReferenceService) rebuildLedger(lease cluster.SharedGCLease) (*ttcsnapshot.ReferenceLedger, error) {
	if s == nil || s.authority == nil || s.packs == nil || s.peers == nil {
		return nil, errors.New("shared reference rebuild is unavailable")
	}
	state, err := s.authority.AuthorityState()
	if err != nil {
		return nil, err
	}
	if state.SharedGC.NodeID != lease.NodeID || state.SharedGC.Incarnation != lease.Incarnation || state.SharedGC.Generation != lease.Generation || !time.Now().Before(state.SharedGC.ExpiresAt) {
		return nil, errors.New("shared reference lease changed before rebuild")
	}
	generationDir := filepath.Join(s.directory, fmt.Sprintf("generation-%016x", lease.Generation))
	if err := os.RemoveAll(generationDir); err != nil {
		return nil, err
	}
	ledger, err := ttcsnapshot.OpenReferenceLedger(generationDir)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*ttcsnapshot.ReferenceLedger, error) {
		_ = ledger.Close()
		_ = os.RemoveAll(generationDir)
		return nil, err
	}

	// A cold branch's original canonical roots remain live exactly while Raft
	// names its immutable memory snapshot. Cross-origin tree edges are rebuilt
	// from immutable objects, never copied reference counters.
	coldOrigins := make(map[ttcsnapshot.SnapshotID]struct{})
	coldFilesystemReferences := make(map[ttcsnapshot.SnapshotID]map[string]ttcsnapshot.ObjectID)
	addFilesystemRoots := func(identity, relative string) error {
		if relative == "" {
			return nil
		}
		path := filepath.Join(s.sharedRoot, relative)
		if err := validateColdManifestPath(s.sharedRoot, path); err != nil {
			return err
		}
		manifest, err := readColdBranchManifest(path)
		if err != nil {
			return err
		}
		for _, filesystem := range manifest.Filesystems {
			root, err := ttcsnapshot.ParseObjectID(filesystem.Root.Root)
			if err != nil {
				return err
			}
			if coldFilesystemReferences[root.Snapshot] == nil {
				coldFilesystemReferences[root.Snapshot] = make(map[string]ttcsnapshot.ObjectID)
			}
			nameHash := sha256.Sum256([]byte(identity + "\x00" + filesystem.Root.Source))
			coldFilesystemReferences[root.Snapshot][fmt.Sprintf("root/cold/%x", nameHash)] = root
		}
		return nil
	}
	for _, branch := range state.Branches {
		if !branch.Corrupted && branch.Roots.MemorySnapshot != 0 {
			coldOrigins[ttcsnapshot.SnapshotID(branch.Roots.MemorySnapshot)] = struct{}{}
		}
		if !branch.Corrupted {
			if err := addFilesystemRoots("branch\x00"+branch.FamilyID+"\x00"+branch.BranchID, branch.Roots.FilesystemRoot); err != nil {
				return fail(fmt.Errorf("reading branch filesystem roots: %w", err))
			}
		}
	}
	// Preparing and capture-ready publications are still volatile origins.
	// Their source daemon's inventory retains the local graph roots until the
	// same Raft command that commits the branch's durable roots routes those
	// origins to shared storage. CandidateRoots intentionally names the future
	// manifest before it exists, so treating it as an already-cold root both
	// races publication and incorrectly splits one reference authority between
	// the provider and this shared ledger.

	// Query every unfenced execution incarnation. If one cannot prove its
	// claims, GC remains unavailable, but normal reads and execution continue.
	// Claims must be part of ImportOrigin's initial baseline: importing an
	// unreferenced object first would correctly make it reclaiming, after which
	// a later retain is forbidden from resurrecting it.
	claimReferences := make(map[ttcsnapshot.SnapshotID]map[string]ttcsnapshot.ObjectID)
	nodes := make([]cluster.NodeRecord, 0, len(state.Nodes))
	for _, node := range state.Nodes {
		if node.State != cluster.NodeOffline {
			nodes = append(nodes, node)
		}
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].NodeID < nodes[j].NodeID })
	observed := state.LeaderObservedAt
	if observed.IsZero() {
		observed = time.Now()
	}
	ctx, cancel := context.WithDeadline(context.Background(), lease.ExpiresAt)
	defer cancel()
	for _, node := range nodes {
		if !node.LeaseActive(observed) {
			return fail(fmt.Errorf("node %s/%s has not been fenced and cannot provide a current reference inventory", node.NodeID, node.Incarnation))
		}
		var inventory *cluster.SnapshotReferenceInventory
		if node.NodeID == s.nodeID && node.Incarnation == s.incarnation {
			inventory, err = s.SnapshotReferenceInventory(ctx, s.nodeID)
		} else {
			peer, peerErr := s.peers.peer(node.NodeID, false)
			if peerErr != nil {
				return fail(peerErr)
			}
			inventory, err = peer.SnapshotReferenceInventory(ctx)
		}
		if err != nil {
			return fail(fmt.Errorf("reading snapshot reference claims from node %s: %w", node.NodeID, err))
		}
		if inventory == nil || inventory.NodeID != node.NodeID || inventory.Incarnation != node.Incarnation {
			return fail(fmt.Errorf("snapshot reference inventory identity is %v/%v, want %s/%s", inventoryNodeID(inventory), inventoryIncarnation(inventory), node.NodeID, node.Incarnation))
		}
		for _, claim := range inventory.Claims {
			if _, volatile := state.VolatileOrigins[claim.Origin]; volatile {
				continue
			}
			origin := ttcsnapshot.SnapshotID(claim.Origin)
			if _, retired := state.RetiredOrigins[claim.Origin]; retired {
				return fail(fmt.Errorf("node %s claims retired snapshot origin %d", node.NodeID, claim.Origin))
			}
			roots, err := graphReferenceRoots(origin, claim.Operation, claim.Roots)
			if err != nil {
				return fail(fmt.Errorf("validating node %s reference claim: %w", node.NodeID, err))
			}
			if claimReferences[origin] == nil {
				claimReferences[origin] = make(map[string]ttcsnapshot.ObjectID)
			}
			for name, object := range roots {
				if previous, exists := claimReferences[origin][name]; exists && previous != object {
					return fail(fmt.Errorf("reference claim %q selects both %s and %s", name, previous, object))
				}
				claimReferences[origin][name] = object
			}
		}
	}

	manifests, err := s.packs.Manifests()
	if err != nil {
		return fail(err)
	}
	imported := make(map[ttcsnapshot.SnapshotID]struct{}, len(manifests))
	for _, manifest := range manifests {
		if _, retired := state.RetiredOrigins[uint64(manifest.Snapshot)]; retired {
			continue
		}
		objects := make([]ttcsnapshot.Object, len(manifest.Objects))
		for index, packed := range manifest.Objects {
			objects[index] = packed.Object
		}
		// Bootstrap every provider baseline long enough to import all origins.
		// Parent origins may sort after their children, and ImportOrigin correctly
		// forbids resurrecting a zero-count object. Namespacing ordinary roots
		// avoids collisions between repositories; incoming-edge bridge names stay
		// canonical so a subsequently imported parent can replace them in place.
		references := make(map[string]ttcsnapshot.ObjectID, len(manifest.References))
		for name, object := range manifest.References {
			if strings.HasPrefix(name, "root/bridge/edge/") {
				references[name] = object
			} else {
				digest := sha256.Sum256([]byte(name))
				references[fmt.Sprintf("bootstrap/%016x/%x", uint64(manifest.Snapshot), digest)] = object
			}
		}
		if _, live := coldOrigins[manifest.Snapshot]; live {
			for name, object := range manifest.References {
				if strings.HasPrefix(name, "root/") {
					digest := sha256.Sum256([]byte(name))
					references[fmt.Sprintf("root/cold-memory/%016x/%x", uint64(manifest.Snapshot), digest)] = object
				}
			}
		}
		for name, object := range claimReferences[manifest.Snapshot] {
			if previous, exists := references[name]; exists && previous != object {
				return fail(fmt.Errorf("reference %q selects both %s and %s", name, previous, object))
			}
			references[name] = object
		}
		for name, object := range coldFilesystemReferences[manifest.Snapshot] {
			if previous, exists := references[name]; exists && previous != object {
				return fail(fmt.Errorf("reference %q selects both %s and %s", name, previous, object))
			}
			references[name] = object
		}
		if err := ledger.ImportOrigin(manifest.Snapshot, objects, references); err != nil {
			return fail(fmt.Errorf("importing shared origin %d during rebuild: %w", manifest.Snapshot, err))
		}
		imported[manifest.Snapshot] = struct{}{}
	}
	// All structural cross-origin edges and authoritative roots are now
	// present. Remove the temporary provider baselines; only Raft-selected cold
	// roots and incarnation-bound active claims remain.
	inspection := ledger.Inspect()
	for name, object := range inspection.References {
		if !strings.HasPrefix(name, "bootstrap/") {
			continue
		}
		operation := fmt.Sprintf("rebuild/%016x/release/%s", lease.Generation, name)
		if err := ledger.ApplyReference(operation, name, object, false); err != nil {
			return fail(fmt.Errorf("releasing bootstrap reference %q: %w", name, err))
		}
	}
	for origin := range claimReferences {
		if _, ok := imported[origin]; !ok {
			return fail(fmt.Errorf("active references select shared origin %d without a committed manifest", origin))
		}
	}
	for origin := range coldFilesystemReferences {
		if _, ok := imported[origin]; !ok {
			return fail(fmt.Errorf("cold filesystem references shared origin %d without a committed manifest", origin))
		}
	}
	if err := ledger.Checkpoint(); err != nil {
		return fail(err)
	}
	return ledger, nil
}

func inventoryNodeID(inventory *cluster.SnapshotReferenceInventory) string {
	if inventory == nil {
		return "<nil>"
	}
	return inventory.NodeID
}

func inventoryIncarnation(inventory *cluster.SnapshotReferenceInventory) string {
	if inventory == nil {
		return "<nil>"
	}
	return inventory.Incarnation
}

func (s *sharedReferenceService) activate(lease cluster.SharedGCLease) error {
	if s == nil || lease.Generation == 0 || lease.NodeID != s.nodeID || lease.Incarnation != s.incarnation || !time.Now().Before(lease.ExpiresAt) {
		return errors.New("invalid shared reference coordinator lease")
	}
	s.activationMu.Lock()
	defer s.activationMu.Unlock()
	s.mu.Lock()
	if s.ledger != nil && s.lease.Generation == lease.Generation {
		s.lease = lease
		s.mu.Unlock()
		return nil
	}
	if s.ledger != nil {
		_ = s.ledger.Close()
		s.ledger = nil
		s.lease = cluster.SharedGCLease{}
	}
	s.mu.Unlock()
	ledger, err := s.rebuildLedger(lease)
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
	s.mu.Lock()
	s.ledger, s.lease = ledger, current
	s.mu.Unlock()
	if entries, err := os.ReadDir(s.directory); err == nil {
		keep := fmt.Sprintf("generation-%016x", current.Generation)
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), "generation-") && entry.Name() != keep {
				_ = os.RemoveAll(filepath.Join(s.directory, entry.Name()))
			}
		}
	}
	return nil
}

func (s *sharedReferenceService) deactivate() error {
	if s == nil {
		return nil
	}
	s.activationMu.Lock()
	defer s.activationMu.Unlock()
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

// collectAndRetire performs the shared origin retirement protocol in its only
// safe order: collect exact zero-count objects, deliver every resulting
// cross-origin release, prove that no physical reader remains, fence the empty
// origin permanently in Raft, remove its physical packs, and finally forget
// its ledger state. Each boundary is durable and idempotent, so a successor
// lease holder rolls an interrupted attempt forward rather than guessing from
// directory contents.
func (s *sharedReferenceService) collectAndRetire(ctx context.Context, lease cluster.SharedGCLease, limit int) (int, error) {
	if limit <= 0 {
		return 0, nil
	}
	collected := 0
	if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		var err error
		collected, err = ledger.Collect(limit)
		return err
	}); err != nil {
		return 0, err
	}
	if _, err := s.drainOutbox(ctx, limit*4); err != nil {
		return collected, err
	}
	// Rewrite at most one partially-live origin per maintenance pass. Logical
	// collection is already durable in the ledger; compaction only changes its
	// reader-fenced physical representation and therefore may be retried after
	// interruption without affecting authority.
	var origins []ttcsnapshot.SnapshotID
	if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		origins = ledger.Inspect().Origins
		return nil
	}); err != nil {
		return collected, err
	}
	for _, origin := range origins {
		if err := ctx.Err(); err != nil {
			return collected, err
		}
		var objects []ttcsnapshot.Object
		var references map[string]ttcsnapshot.ObjectID
		if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
			var ok bool
			objects, references, ok = ledger.OriginManifest(origin)
			if !ok {
				return os.ErrNotExist
			}
			return nil
		}); err != nil {
			return collected, err
		}
		if len(objects) == 0 {
			continue
		}
		if _, busy, err := s.packs.Reconcile(origin); err != nil {
			return collected, err
		} else if busy {
			continue
		}
		manifest, err := s.packs.Manifest(origin)
		if err != nil {
			return collected, err
		}
		if !sharedPackNeedsCompaction(manifest, objects) {
			continue
		}
		if _, busy, err := s.packs.Compact(origin, objects, references); err != nil {
			return collected, err
		} else if !busy {
			break
		}
	}
	var candidates []ttcsnapshot.SnapshotID
	if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		candidates = ledger.EmptyOrigins()
		return nil
	}); err != nil {
		return collected, err
	}
	for _, snapshot := range candidates {
		if err := ctx.Err(); err != nil {
			return collected, err
		}
		_, volatile, err := s.authority.SnapshotProvider(uint64(snapshot))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return collected, err
		}
		if volatile {
			// Import precedes the provider-to-shared authority switch. An empty
			// handoff baseline cannot be retired until that switch commits.
			continue
		}
		ready, err := s.packs.RetirementReady(snapshot)
		if err != nil {
			return collected, err
		}
		if !ready {
			// A range reader is still using this origin. Do not make the
			// irreversible Raft transition until the physical data plane agrees
			// with the ledger's zero-reference result.
			continue
		}
		if _, err := s.authority.RetireSharedSnapshot(lease, uint64(snapshot)); err != nil {
			return collected, err
		}
		if err := s.completeRetirement(snapshot); err != nil {
			return collected, err
		}
	}
	return collected, nil
}

func sharedPackNeedsCompaction(manifest *ttcsnapshot.SnapshotManifest, live []ttcsnapshot.Object) bool {
	if manifest == nil || len(live) == 0 {
		return false
	}
	if len(manifest.Packs) > 32 {
		return true
	}
	liveIDs := make(map[ttcsnapshot.ObjectID]struct{}, len(live))
	for _, object := range live {
		liveIDs[object.ID] = struct{}{}
	}
	// Account for both aligned bulk extents and the manifest records that hold
	// metadata-only objects. This keeps metadata-heavy filesystem origins from
	// growing forever even though their B-tree nodes occupy no pack extents.
	var total, retained uint64
	for _, packed := range manifest.Objects {
		weight := uint64(64 + len(packed.Object.Data) + 32*len(packed.Object.Children) + 32*len(packed.Extents))
		for _, extent := range packed.Extents {
			weight += uint64((extent.Length + 4095) &^ 4095)
		}
		total += weight
		if _, ok := liveIDs[packed.Object.ID]; ok {
			retained += weight
		}
	}
	return total != 0 && retained <= total/2
}

func (s *sharedReferenceService) completeRetirement(snapshot ttcsnapshot.SnapshotID) error {
	busy, err := s.packs.CompleteRetirement(snapshot)
	if err != nil {
		return err
	}
	if busy {
		return fmt.Errorf("shared snapshot %d still has an active reader lease", snapshot)
	}
	return s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		return ledger.RetireOrigin(snapshot)
	})
}

var _ cluster.SharedSnapshotReferenceProvider = (*sharedReferenceService)(nil)
var _ cluster.SnapshotReferenceInventoryProvider = (*sharedReferenceService)(nil)
