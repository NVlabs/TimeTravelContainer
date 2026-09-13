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
	collectionMu sync.Mutex
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
		batchLimit := limit - delivered
		if batchLimit > 512 {
			batchLimit = 512
		}
		var pending []ttcsnapshot.OutboundReference
		if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
			pending = ledger.PendingOutboundBatch(batchLimit)
			return nil
		}); err != nil {
			return delivered, err
		}
		if len(pending) == 0 {
			return delivered, nil
		}
		state, err := s.authority.AuthorityState()
		if err != nil {
			return delivered, err
		}
		for offset := 0; offset < len(pending); {
			outbound := pending[offset]
			provider, volatile := state.VolatileOrigins[uint64(outbound.Child.Snapshot)]
			end := offset + 1
			if !volatile {
				err = s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
					mutations := make([]ttcsnapshot.SnapshotReferenceMutation, 0, len(pending)-offset)
					for end = offset; end < len(pending); end++ {
						candidate := pending[end]
						if _, candidateVolatile := state.VolatileOrigins[uint64(candidate.Child.Snapshot)]; candidateVolatile {
							break
						}
						if candidate.Retain && ledger.OwnsEdge(candidate.Parent, outboundSlot(candidate.Name), candidate.Child) {
							continue
						}
						mutations = append(mutations, ttcsnapshot.SnapshotReferenceMutation{
							Operation: candidate.Operation, Name: candidate.Name,
							Object: candidate.Child, Retain: candidate.Retain,
						})
					}
					return ledger.ApplyReferences(mutations)
				})
			} else if provider.NodeID == s.nodeID {
				roots := make(map[string]ttcsnapshot.ObjectID)
				for end = offset; end < len(pending); end++ {
					candidate := pending[end]
					candidateProvider, candidateVolatile := state.VolatileOrigins[uint64(candidate.Child.Snapshot)]
					if !candidateVolatile || candidateProvider.NodeID != s.nodeID || candidate.Retain != outbound.Retain {
						break
					}
					roots["bridge/"+candidate.Name] = candidate.Child
				}
				if outbound.Retain {
					err = s.graph.RetainRoots(roots)
				} else {
					err = s.graph.ReleaseRetainedRoots(roots)
				}
			} else {
				if s.peers == nil {
					return delivered, errors.New("cluster peer routing is unavailable")
				}
				peer, peerErr := s.peers.peer(provider.NodeID, false)
				if peerErr != nil {
					return delivered, peerErr
				}
				// The wire API remains element-addressed because a provider may
				// change at an origin boundary. Delivery is idempotent, while the
				// source acknowledgement below still commits the whole prefix once.
				for end = offset; end < len(pending); end++ {
					candidate := pending[end]
					candidateProvider, candidateVolatile := state.VolatileOrigins[uint64(candidate.Child.Snapshot)]
					if !candidateVolatile || candidateProvider.NodeID != provider.NodeID {
						break
					}
					wireOperation := fmt.Sprintf("edge-%016x-%016x", uint64(candidate.Parent.Snapshot), candidate.Sequence)
					if err = peer.ApplySnapshotReference(ctx, uint64(candidate.Child.Snapshot), wireOperation, candidate.Name, candidate.Child.String(), candidate.Retain); err != nil {
						break
					}
				}
			}
			if err != nil {
				return delivered, err
			}
			offset = end
		}
		last := pending[len(pending)-1].Sequence
		if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
			return ledger.AcknowledgeOutboundThrough(last)
		}); err != nil {
			return delivered, err
		}
		delivered += len(pending)
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
			if _, volatile := state.VolatileOrigins[uint64(root.Snapshot)]; volatile {
				continue
			}
			if coldFilesystemReferences[root.Snapshot] == nil {
				coldFilesystemReferences[root.Snapshot] = make(map[string]ttcsnapshot.ObjectID)
			}
			// Include the immutable root in the name. Replacing a branch's cold
			// generation must retain the new graph before releasing the old one;
			// a stable branch-only name would require an unsafe in-place reference
			// replacement in the ledger.
			nameHash := sha256.Sum256([]byte(identity + "\x00" + filesystem.Root.Source + "\x00" + root.String()))
			coldFilesystemReferences[root.Snapshot][fmt.Sprintf("root/cold/%x", nameHash)] = root
		}
		return nil
	}
	for _, branch := range state.Branches {
		_, memoryVolatile := state.VolatileOrigins[branch.Roots.MemorySnapshot]
		if !branch.Corrupted && branch.Roots.MemorySnapshot != 0 && !memoryVolatile {
			coldOrigins[ttcsnapshot.SnapshotID(branch.Roots.MemorySnapshot)] = struct{}{}
		}
		if !branch.Corrupted {
			if err := addFilesystemRoots("branch\x00"+branch.FamilyID+"\x00"+branch.BranchID, branch.Roots.FilesystemRoot); err != nil {
				return fail(fmt.Errorf("reading branch filesystem roots: %w", err))
			}
		}
	}
	// Complete() transfers each origin's reference authority before it commits
	// the branch roots. A coordinator rebuild in that interval must retain the
	// publication's already-shared candidate roots; otherwise it can discard a
	// graph after provider handoff but immediately before the final branch CAS.
	// The candidate uses the final branch reference names, so commit changes no
	// counts. Origins that are still volatile remain protected by the provider.
	for _, publication := range state.Publications {
		if publication.State != cluster.PublicationReady || (publication.CandidateRoots.MemorySnapshot == 0 && publication.CandidateRoots.FilesystemRoot == "") {
			continue
		}
		_, memoryVolatile := state.VolatileOrigins[publication.CandidateRoots.MemorySnapshot]
		if publication.CandidateRoots.MemorySnapshot != 0 && !memoryVolatile {
			coldOrigins[ttcsnapshot.SnapshotID(publication.CandidateRoots.MemorySnapshot)] = struct{}{}
		}
		if publication.CandidateRoots.FilesystemRoot != "" {
			identity := "branch\x00" + publication.FamilyID + "\x00" + publication.BranchID
			if err := addFilesystemRoots(identity, publication.CandidateRoots.FilesystemRoot); err != nil {
				// Ready is committed before the publisher creates the cold
				// manifest. Until the first origin handoff, the provider remains
				// authoritative, so a missing candidate manifest is expected and
				// must not prevent coordinator activation.
				if !errors.Is(err, os.ErrNotExist) {
					return fail(fmt.Errorf("reading publication filesystem roots: %w", err))
				}
			}
		}
	}

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
		// Publication writes immutable pack bytes before authority handoff.
		// While Raft still names a volatile provider, that provider's ledger is
		// authoritative and a coordinator must not pre-import a speculative pack:
		// doing so with no local roots would make its objects reclaiming and cause
		// the later exact handoff to look like an idempotent import.
		if _, volatile := state.VolatileOrigins[uint64(manifest.Snapshot)]; volatile {
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
	if s.ledger == nil || s.lease.Generation == 0 {
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
		return fmt.Errorf("authorizing provider: %w", err)
	}
	manifest, err := s.packs.Manifest(ttcsnapshot.SnapshotID(snapshotID))
	if err != nil {
		return fmt.Errorf("reading shared pack manifest: %w", err)
	}
	objects := make([]ttcsnapshot.Object, len(manifest.Objects))
	for index, packed := range manifest.Objects {
		objects[index] = packed.Object
	}
	return s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		if err := ledger.ImportOrigin(ttcsnapshot.SnapshotID(snapshotID), objects, manifest.References); err != nil {
			return fmt.Errorf("importing origin into reference ledger: %w", err)
		}
		return nil
	})
}

// ApplySharedSnapshotReference records an idempotent publication bridge at
// the shared-GC coordinator. Only the current volatile provider may create or
// remove such a bridge, and the referenced object must belong to the origin
// being published.
func (s *sharedReferenceService) ApplySharedSnapshotReference(_ context.Context, peer string, snapshotID uint64, operation, name, objectText string, retain bool) error {
	if err := s.authorizeProvider(peer, snapshotID); err != nil {
		return err
	}
	object, err := ttcsnapshot.ParseObjectID(objectText)
	if err != nil {
		return err
	}
	if uint64(object.Snapshot) != snapshotID {
		return fmt.Errorf("shared reference object %s does not belong to snapshot %d", object, snapshotID)
	}
	return s.mutateReference(operation, name, object, retain)
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

func isAuthoritativeColdReference(name string) bool {
	return strings.HasPrefix(name, "root/cold-memory/") || strings.HasPrefix(name, "root/cold/")
}

func isProviderBaselineReference(name string) bool {
	if isAuthoritativeColdReference(name) || strings.HasPrefix(name, "root/bridge/edge/") {
		return false
	}
	return strings.HasPrefix(name, "root/") || strings.HasPrefix(name, "pin/")
}

func providerBaselineCanBeReleased(state *cluster.AuthorityState, name string, object ttcsnapshot.ObjectID) bool {
	if state == nil || !isProviderBaselineReference(name) {
		return false
	}
	_, volatile := state.VolatileOrigins[uint64(object.Snapshot)]
	return !volatile
}

// authoritativeColdReferences derives the small mutable root set selected by
// Raft. The immutable object graph and its per-edge counts remain outside
// consensus; only these branch-level roots change when a durable branch is
// published, replaced, or removed.
func (s *sharedReferenceService) authoritativeColdReferences(state *cluster.AuthorityState) (map[string]ttcsnapshot.ObjectID, error) {
	if s == nil || state == nil || s.packs == nil {
		return nil, errors.New("shared authority roots are unavailable")
	}
	desired := make(map[string]ttcsnapshot.ObjectID)
	memory := make(map[ttcsnapshot.SnapshotID]struct{})
	addMemory := func(snapshot uint64) {
		if snapshot == 0 {
			return
		}
		if _, volatile := state.VolatileOrigins[snapshot]; !volatile {
			memory[ttcsnapshot.SnapshotID(snapshot)] = struct{}{}
		}
	}
	addFilesystem := func(identity, relative string) error {
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
			if _, volatile := state.VolatileOrigins[uint64(root.Snapshot)]; volatile {
				continue
			}
			nameHash := sha256.Sum256([]byte(identity + "\x00" + filesystem.Root.Source + "\x00" + root.String()))
			name := fmt.Sprintf("root/cold/%x", nameHash)
			if previous, ok := desired[name]; ok && previous != root {
				return fmt.Errorf("authority root %q selects both %s and %s", name, previous, root)
			}
			desired[name] = root
		}
		return nil
	}
	for _, branch := range state.Branches {
		if branch.Corrupted {
			continue
		}
		addMemory(branch.Roots.MemorySnapshot)
		identity := "branch\x00" + branch.FamilyID + "\x00" + branch.BranchID
		if err := addFilesystem(identity, branch.Roots.FilesystemRoot); err != nil {
			return nil, err
		}
	}
	// Candidate roots bridge the committed provider-to-shared switch and the
	// final branch-root CAS. A candidate and its final branch root have the same
	// immutable-object-qualified reference name, making that transition
	// count-neutral while still permitting generation replacement.
	for _, publication := range state.Publications {
		if publication.State != cluster.PublicationReady {
			continue
		}
		addMemory(publication.CandidateRoots.MemorySnapshot)
		identity := "branch\x00" + publication.FamilyID + "\x00" + publication.BranchID
		if err := addFilesystem(identity, publication.CandidateRoots.FilesystemRoot); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	for snapshot := range memory {
		manifest, err := s.packs.Manifest(snapshot)
		if err != nil {
			return nil, fmt.Errorf("reading authoritative memory origin %d: %w", snapshot, err)
		}
		for name, object := range manifest.References {
			if !strings.HasPrefix(name, "root/") {
				continue
			}
			digest := sha256.Sum256([]byte(name))
			desired[fmt.Sprintf("root/cold-memory/%016x/%x", uint64(snapshot), digest)] = object
		}
	}
	return desired, nil
}

// reconcileAuthoritativeColdReferences keeps coordinator-local reference
// counts synchronized with the current Raft root set without rebuilding or
// walking immutable objects. Retains precede releases so replacing one branch
// root can never expose a transient zero-count graph.
func (s *sharedReferenceService) reconcileAuthoritativeColdReferences() error {
	state, err := s.authority.AuthorityState()
	if err != nil {
		return err
	}
	desired, err := s.authoritativeColdReferences(state)
	if err != nil {
		return err
	}
	var current map[string]ttcsnapshot.ObjectID
	if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		current = ledger.References()
		return nil
	}); err != nil {
		return err
	}
	retain := make([]string, 0)
	release := make([]string, 0)
	for name, object := range desired {
		if previous, ok := current[name]; ok {
			if previous != object {
				return fmt.Errorf("authoritative reference %q changed from %s to %s", name, previous, object)
			}
			continue
		}
		retain = append(retain, name)
	}
	for name, object := range current {
		if isAuthoritativeColdReference(name) {
			if _, ok := desired[name]; !ok {
				release = append(release, name)
			}
			continue
		}
		// PublishHandoff imports the provider's exact root/pin baseline so its
		// counts remain correct until Raft moves the origin to shared storage.
		// Once that move commits, branch durability is represented by desired
		// cold roots above and live remote consumers by their durable remote/
		// claims. Keeping the provider baseline would pin an otherwise-dead
		// origin until coordinator turnover. Retains are applied before these
		// releases below, so the replacement is atomic with respect to GC.
		if providerBaselineCanBeReleased(state, name, object) {
			release = append(release, name)
		}
	}
	sort.Strings(retain)
	sort.Strings(release)
	return s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		for _, name := range retain {
			object := desired[name]
			digest := sha256.Sum256([]byte("retain\x00" + name + "\x00" + object.String()))
			if err := ledger.ApplyReference(fmt.Sprintf("authority-root-retain/%x", digest), name, object, true); err != nil {
				return err
			}
		}
		for _, name := range release {
			object := current[name]
			digest := sha256.Sum256([]byte("release\x00" + name + "\x00" + object.String()))
			if err := ledger.ApplyReference(fmt.Sprintf("authority-root-release/%x", digest), name, object, false); err != nil {
				return err
			}
		}
		return nil
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
	// The periodic maintenance loop and an explicit node shutdown may both run
	// under this daemon's current lease. Reference-ledger calls are individually
	// serialized, but collection also has a physical phase between calls: one
	// pass may retire an origin while another has already selected that origin
	// for compaction. Serialize the complete logical/physical transaction so a
	// selected origin cannot disappear underneath a sibling pass.
	s.collectionMu.Lock()
	defer s.collectionMu.Unlock()
	if err := s.reconcileAuthoritativeColdReferences(); err != nil {
		return 0, err
	}
	// A freshly rebuilt ledger represents cross-origin parent edges as durable
	// outbound retains. Deliver the complete finite retain frontier before
	// examining zero-count objects. Collecting first can mark a child reclaiming
	// and then reject its legitimate parent edge as an attempted resurrection.
	// Mutations are serialized by collectionMu, so reaching an empty outbox is a
	// stable barrier for this pass.
	for {
		delivered, err := s.drainOutbox(ctx, limit*4)
		if err != nil {
			return 0, err
		}
		if delivered == 0 {
			break
		}
	}
	collected := 0
	if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		var err error
		collected, err = ledger.Collect(limit)
		return err
	}); err != nil {
		return 0, err
	}
	// Collection creates cross-origin releases. These may be bounded here: a
	// subsequent pass cannot reclaim their children until its leading outbox
	// barrier has delivered the remainder.
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
		var ready bool
		if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
			var ok bool
			objects, references, ok, ready = ledger.OriginCompactionManifest(origin)
			if !ok {
				return os.ErrNotExist
			}
			return nil
		}); err != nil {
			return collected, err
		}
		if !ready || len(objects) == 0 {
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

// drainBeforeShutdown finishes every immediately reachable shared-graph
// collection batch when this daemon is the elected coordinator. Node stop is
// the last guaranteed maintenance opportunity: relying on the periodic pass
// leaves dead snapshot packs behind when a family removal is followed
// immediately by cluster shutdown.
func (s *sharedReferenceService) drainBeforeShutdown(ctx context.Context, requireOwnership bool) error {
	if s == nil || s.authority == nil {
		return nil
	}
	const duration = 15 * time.Second
	var lease cluster.SharedGCLease
	for {
		var err error
		lease, err = s.authority.SharedGCLease()
		if err != nil {
			return err
		}
		now := time.Now()
		if lease.Generation == 0 || !now.Before(lease.ExpiresAt) {
			lease, err = s.authority.AcquireSharedGCLease(s.nodeID, s.incarnation, duration)
			if err == nil {
				break
			}
			if !requireOwnership {
				return nil
			}
		} else if lease.NodeID == s.nodeID && lease.Incarnation == s.incarnation {
			break
		} else if !requireOwnership {
			// A different live coordinator owns shared mutation. Its ordinary
			// maintenance remains responsible; this node must not contend with it.
			return nil
		}
		// A final cluster node has no successor that can rebuild the disposable
		// ledger. Wait out a concurrently retiring coordinator's bounded lease,
		// then acquire a fresh generation and leave shared packs residue-free.
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	if err := s.activate(lease); err != nil {
		return err
	}
	stopKeeper := keepSharedGCLeaseAlive(s.authority, lease, duration)
	defer func() { _, _ = stopKeeper() }()
	// One zero pass can follow a pass that only retired/compacted an origin.
	// Require two stable passes so releases emitted by that boundary are also
	// observed, while referenced live origins naturally make immediate exit.
	zeroPasses := 0
	for zeroPasses < 2 {
		if err := ctx.Err(); err != nil {
			return err
		}
		collected, err := s.collectAndRetire(ctx, lease, 4096)
		if err != nil {
			return err
		}
		if collected == 0 {
			zeroPasses++
		} else {
			zeroPasses = 0
		}
	}
	return nil
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
