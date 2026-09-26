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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

const sharedPublicationBridgeIntentVersion = 1

const (
	sharedPublicationBridgeTransaction = "transaction"
	sharedPublicationBridgePublication = "publication"
	sharedPublicationBridgeOrigin      = "origin"
)

// sharedPublicationBridgeIntent is the durable crash fence for the short
// provider-to-shared interval in which an immutable origin exists in the
// shared pack store but its eventual consumer has not installed replacement
// roots yet. It is snapshot-granular: page and file identities remain outside
// the control plane.
type sharedPublicationBridgeIntent struct {
	Version       int       `json:"version"`
	Snapshot      uint64    `json:"snapshot"`
	Kind          string    `json:"kind"`
	Transaction   string    `json:"transaction,omitempty"`
	Family        string    `json:"family,omitempty"`
	Branch        string    `json:"branch,omitempty"`
	PublicationID string    `json:"publication_id,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

func (i sharedPublicationBridgeIntent) validate() error {
	if i.Version != sharedPublicationBridgeIntentVersion || i.Snapshot == 0 {
		return errors.New("invalid shared publication bridge intent")
	}
	switch i.Kind {
	case sharedPublicationBridgeTransaction:
		if i.Transaction == "" || i.Family != "" || i.Branch != "" || i.PublicationID != "" {
			return errors.New("invalid transaction publication bridge intent")
		}
	case sharedPublicationBridgePublication:
		if i.Transaction != "" || i.Family == "" || i.Branch == "" || i.PublicationID == "" {
			return errors.New("invalid branch publication bridge intent")
		}
	case sharedPublicationBridgeOrigin:
		if i.Transaction != "" || i.Family != "" || i.Branch != "" || i.PublicationID != "" {
			return errors.New("invalid origin publication bridge intent")
		}
	default:
		return fmt.Errorf("unknown shared publication bridge intent kind %q", i.Kind)
	}
	return nil
}

func sharedPublicationBridgeIntentPath(sharedRoot string, snapshot ttcsnapshot.SnapshotID) string {
	return filepath.Join(sharedRoot, "cluster", "reference-bridges", fmt.Sprintf("%016x.json", uint64(snapshot)))
}

func persistSharedPublicationBridgeIntent(sharedRoot string, intent sharedPublicationBridgeIntent) error {
	return persistSharedPublicationBridgeIntentWithAuthority(sharedRoot, intent, nil)
}

// persistSharedPublicationBridgeIntentWithAuthority durably transfers bridge
// ownership from a completed operation to its next operation on the same
// immutable snapshot. The bridge's ledger roots are snapshot/object-derived,
// so they remain valid across this ownership handoff. Raft terminality is the
// fence that prevents a previous publisher from resuming after adoption.
func persistSharedPublicationBridgeIntentWithAuthority(sharedRoot string, intent sharedPublicationBridgeIntent, state *cluster.AuthorityState) error {
	if sharedRoot == "" {
		return errors.New("shared root is unavailable for publication bridge")
	}
	if err := intent.validate(); err != nil {
		return err
	}
	path := sharedPublicationBridgeIntentPath(sharedRoot, ttcsnapshot.SnapshotID(intent.Snapshot))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// One bounded directory lock avoids leaking a lock inode for every snapshot
	// ever created. Bridge ownership changes are control-plane operations, so
	// serializing them does not enter the guest data path.
	lock, err := os.OpenFile(filepath.Join(filepath.Dir(path), ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if data, err := os.ReadFile(path); err == nil {
		var current sharedPublicationBridgeIntent
		if err := json.Unmarshal(data, &current); err != nil {
			return fmt.Errorf("decoding existing publication bridge intent: %w", err)
		}
		if err := current.validate(); err != nil {
			return err
		}
		if current.Snapshot != intent.Snapshot || current.Kind != intent.Kind || current.Transaction != intent.Transaction ||
			current.Family != intent.Family || current.Branch != intent.Branch || current.PublicationID != intent.PublicationID {
			// An origin handoff has no separate Raft transaction whose
			// terminality can fence a concurrent attempt, so only its exact retry
			// may reuse the intent. Transaction and branch-publication owners are
			// safely replaceable only after Raft says they are terminal.
			if state == nil || current.Kind == sharedPublicationBridgeOrigin || publicationBridgeIntentNeeded(state, current) {
				return fmt.Errorf("snapshot %d publication bridge is owned by a different operation", intent.Snapshot)
			}
			return writeDurableJSON(path, intent)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeDurableJSON(path, intent)
}

func removeSharedPublicationBridgeIntent(sharedRoot string, snapshot ttcsnapshot.SnapshotID) error {
	path := sharedPublicationBridgeIntentPath(sharedRoot, snapshot)
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	return errors.Join(syncErr, directory.Close())
}

func loadSharedPublicationBridgeIntents(sharedRoot string) ([]sharedPublicationBridgeIntent, error) {
	directory := filepath.Join(sharedRoot, "cluster", "reference-bridges")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	intents := make([]sharedPublicationBridgeIntent, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		encoded := strings.TrimSuffix(entry.Name(), ".json")
		snapshot, parseErr := strconv.ParseUint(encoded, 16, 64)
		if parseErr != nil || snapshot == 0 || fmt.Sprintf("%016x", snapshot) != encoded {
			// Unknown files are never interpreted as TTC state.
			continue
		}
		data, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var intent sharedPublicationBridgeIntent
		if err := json.Unmarshal(data, &intent); err != nil {
			return nil, fmt.Errorf("decoding publication bridge intent %q: %w", entry.Name(), err)
		}
		if err := intent.validate(); err != nil {
			return nil, fmt.Errorf("validating publication bridge intent %q: %w", entry.Name(), err)
		}
		if intent.Snapshot != snapshot {
			return nil, fmt.Errorf("publication bridge intent %q names snapshot %d", entry.Name(), intent.Snapshot)
		}
		intents = append(intents, intent)
	}
	sort.Slice(intents, func(i, j int) bool { return intents[i].Snapshot < intents[j].Snapshot })
	return intents, nil
}

func publicationBridgeIntentNeeded(state *cluster.AuthorityState, intent sharedPublicationBridgeIntent) bool {
	if state == nil {
		return true
	}
	if _, volatile := state.VolatileOrigins[intent.Snapshot]; volatile {
		return false
	}
	if state.AbandonedOrigins[intent.Snapshot] {
		return false
	}
	switch intent.Kind {
	case sharedPublicationBridgeTransaction:
		tx, ok := state.Transactions[intent.Transaction]
		return ok && !tx.Terminal()
	case sharedPublicationBridgePublication:
		for _, publication := range state.Publications {
			if publication.FamilyID == intent.Family && publication.BranchID == intent.Branch && publication.PublicationID == intent.PublicationID {
				return publication.State == cluster.PublicationPreparing || publication.State == cluster.PublicationReady
			}
		}
		return false
	case sharedPublicationBridgeOrigin:
		// Origin handoff is performed only after a branch/transfer reference has
		// already been retained. A rebuild obtains that replacement from node
		// inventories, so the temporary bridge must not be resurrected.
		return false
	default:
		return true
	}
}

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
	// compactionPending is protected by mu. Shared-pack compaction is
	// needed only after logical collection removes objects, plus one recovery
	// reconciliation after a new coordinator activates. Remembering that fact
	// avoids cloning every potentially huge manifest on every maintenance tick.
	compactionPending bool
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
	retired := make(map[uint64]struct{})
	if s.authority != nil {
		state, err := s.authority.AuthorityState()
		if err != nil {
			return nil, fmt.Errorf("reading retired snapshot origins: %w", err)
		}
		for origin := range state.RetiredOrigins {
			retired[origin] = struct{}{}
		}
		for origin := range state.AbandonedOrigins {
			retired[origin] = struct{}{}
		}
	}
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
		claims = append(claims, snapshotReferenceClaims(intent, retired)...)
	}
	sort.Slice(claims, func(i, j int) bool {
		if claims[i].Origin != claims[j].Origin {
			return claims[i].Origin < claims[j].Origin
		}
		return claims[i].Operation < claims[j].Operation
	})
	return &cluster.SnapshotReferenceInventory{NodeID: s.nodeID, Incarnation: s.incarnation, Claims: claims}, nil
}

func snapshotReferenceClaims(intent graphReferenceIntent, retired map[uint64]struct{}) []cluster.SnapshotReferenceClaim {
	claims := make([]cluster.SnapshotReferenceClaim, 0, len(intent.Groups))
	for _, group := range intent.Groups {
		if group.Released {
			continue
		}
		// A release reaches the exact reference ledger before the intent is
		// marked complete. If the daemon dies between those two durable
		// writes, collection may legitimately retire the now-unreferenced
		// origin. Reporting the stale local intent as a live claim would then
		// make every future ledger rebuild reject the retired origin, while
		// the release cannot finish without an active ledger. Retirement is
		// irreversible proof that this release was already applied.
		if _, done := retired[group.Origin]; done {
			continue
		}
		claims = append(claims, cluster.SnapshotReferenceClaim{
			Origin: group.Origin, Operation: intent.Operation,
			Roots: append([]cluster.SnapshotRootBinding(nil), group.Roots...),
		})
	}
	return claims
}

// activeSharedClaimReferences collects the exact shared-origin roots claimed
// by every execution incarnation that has not been fenced. A node-local intent
// is written before its retain RPC and remains present until its release RPC is
// durably acknowledged, so the union is a conservative, crash-safe inventory.
//
// This inventory is deliberately queried from live nodes instead of inferred
// from Raft branch records. A branch can own roots during target preparation or
// an asynchronous handoff before its final visible record is committed.
func (s *sharedReferenceService) activeSharedClaimReferences(state *cluster.AuthorityState, deadline time.Time) (map[ttcsnapshot.SnapshotID]map[string]ttcsnapshot.ObjectID, error) {
	if s == nil || s.peers == nil || state == nil {
		return nil, errors.New("shared reference inventory is unavailable")
	}
	desired := make(map[ttcsnapshot.SnapshotID]map[string]ttcsnapshot.ObjectID)
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
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for _, node := range nodes {
		if !node.LeaseActive(observed) {
			return nil, fmt.Errorf("node %s/%s has not been fenced and cannot provide a current reference inventory", node.NodeID, node.Incarnation)
		}
		var inventory *cluster.SnapshotReferenceInventory
		var err error
		if node.NodeID == s.nodeID && node.Incarnation == s.incarnation {
			inventory, err = s.SnapshotReferenceInventory(ctx, s.nodeID)
		} else {
			peer, peerErr := s.peers.peer(node.NodeID, false)
			if peerErr != nil {
				return nil, peerErr
			}
			inventory, err = peer.SnapshotReferenceInventory(ctx)
		}
		if err != nil {
			return nil, fmt.Errorf("reading snapshot reference claims from node %s: %w", node.NodeID, err)
		}
		if inventory == nil || inventory.NodeID != node.NodeID || inventory.Incarnation != node.Incarnation {
			return nil, fmt.Errorf("snapshot reference inventory identity is %v/%v, want %s/%s", inventoryNodeID(inventory), inventoryIncarnation(inventory), node.NodeID, node.Incarnation)
		}
		for _, claim := range inventory.Claims {
			if state.AbandonedOrigins[claim.Origin] {
				continue
			}
			if _, volatile := state.VolatileOrigins[claim.Origin]; volatile {
				continue
			}
			origin := ttcsnapshot.SnapshotID(claim.Origin)
			if _, retired := state.RetiredOrigins[claim.Origin]; retired {
				return nil, fmt.Errorf("node %s claims retired snapshot origin %d", node.NodeID, claim.Origin)
			}
			roots, err := graphReferenceRoots(origin, claim.Operation, claim.Roots)
			if err != nil {
				return nil, fmt.Errorf("validating node %s reference claim: %w", node.NodeID, err)
			}
			if desired[origin] == nil {
				desired[origin] = make(map[string]ttcsnapshot.ObjectID)
			}
			for name, object := range roots {
				if previous, exists := desired[origin][name]; exists && previous != object {
					return nil, fmt.Errorf("reference claim %q selects both %s and %s", name, previous, object)
				}
				desired[origin][name] = object
			}
		}
	}
	return desired, nil
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
			if state.AbandonedOrigins[uint64(outbound.Child.Snapshot)] {
				if outbound.Retain {
					return delivered, fmt.Errorf("cannot retain abandoned snapshot origin %d", outbound.Child.Snapshot)
				}
				delivered++
				offset++
				continue
			}
			provider, volatile := state.VolatileOrigins[uint64(outbound.Child.Snapshot)]
			end := offset + 1
			if !volatile {
				err = s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
					mutations := make([]ttcsnapshot.SnapshotReferenceMutation, 0, len(pending)-offset)
					for end = offset; end < len(pending); end++ {
						candidate := pending[end]
						if state.AbandonedOrigins[uint64(candidate.Child.Snapshot)] {
							break
						}
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

// providerBaselineReferenceName scopes provider-local roots and pins to the
// immutable origin whose handoff journal mutates them. Repository branch names
// are intentionally stable across generations, so two simultaneously live
// origins may otherwise select different objects with the same root name.
// Structural incoming-edge bridges remain canonical: they identify the parent
// object and slot globally and are replaced by that exact edge when the parent
// origin is imported.
func providerBaselineReferenceName(origin ttcsnapshot.SnapshotID, name string) string {
	if strings.HasPrefix(name, "root/bridge/edge/") ||
		(!strings.HasPrefix(name, "root/") && !strings.HasPrefix(name, "pin/")) {
		return name
	}
	digest := sha256.Sum256([]byte(name))
	return fmt.Sprintf("root/provider/%016x/%x", uint64(origin), digest)
}

func providerBaselineReferences(origin ttcsnapshot.SnapshotID, references map[string]ttcsnapshot.ObjectID) map[string]ttcsnapshot.ObjectID {
	result := make(map[string]ttcsnapshot.ObjectID, len(references))
	for name, object := range references {
		result[providerBaselineReferenceName(origin, name)] = object
	}
	return result
}

// incomingEdgeParent decodes the canonical owner-crossing edge reference
// emitted by the snapshot graph. A shared manifest is immutable, so it may
// retain historical bridge names after the parent origin is permanently
// abandoned or retired; coordinator rebuild must classify those names rather
// than treating every manifest entry as currently live.
func incomingEdgeParent(name string) (ttcsnapshot.ObjectID, bool, error) {
	effective := strings.TrimPrefix(name, "root/bridge/")
	if !strings.HasPrefix(effective, "edge/") {
		return ttcsnapshot.ObjectID{}, false, nil
	}
	parts := strings.Split(effective, "/")
	if len(parts) != 3 {
		return ttcsnapshot.ObjectID{}, true, fmt.Errorf("malformed incoming edge reference %q", name)
	}
	parent, err := ttcsnapshot.ParseObjectID(parts[1])
	if err != nil {
		return ttcsnapshot.ObjectID{}, true, fmt.Errorf("parsing incoming edge reference %q: %w", name, err)
	}
	if _, err := strconv.ParseUint(parts[2], 16, 16); err != nil {
		return ttcsnapshot.ObjectID{}, true, fmt.Errorf("parsing incoming edge slot in %q: %w", name, err)
	}
	return parent, true, nil
}

func manifestReferenceIsLive(state *cluster.AuthorityState, name string) (bool, error) {
	parent, edge, err := incomingEdgeParent(name)
	if err != nil || !edge {
		return !edge, err
	}
	if state == nil || uint64(parent.Snapshot) >= state.NextSnapshotID {
		return false, fmt.Errorf("incoming edge reference %q names unallocated parent origin %d", name, parent.Snapshot)
	}
	if state.AbandonedOrigins[uint64(parent.Snapshot)] {
		return false, nil
	}
	if _, retired := state.RetiredOrigins[uint64(parent.Snapshot)]; retired {
		return false, nil
	}
	return true, nil
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
	coldMemoryReferences := make(map[ttcsnapshot.SnapshotID]map[string]ttcsnapshot.ObjectID)
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
			if state.AbandonedOrigins[uint64(root.Snapshot)] {
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
		if !branch.Corrupted && branch.Roots.MemorySnapshot != 0 && !memoryVolatile && !state.AbandonedOrigins[branch.Roots.MemorySnapshot] {
			origin := ttcsnapshot.SnapshotID(branch.Roots.MemorySnapshot)
			manifest, err := s.packs.Manifest(origin)
			if err != nil {
				return fail(fmt.Errorf("reading branch memory roots: %w", err))
			}
			if coldMemoryReferences[origin] == nil {
				coldMemoryReferences[origin] = make(map[string]ttcsnapshot.ObjectID)
			}
			identity := coldBranchReferenceIdentity(branch.FamilyID, branch.BranchID, branch.PublicationID)
			for name, object := range manifest.References {
				if strings.HasPrefix(name, "root/") {
					coldMemoryReferences[origin][coldMemoryReferenceName(origin, identity, name, object)] = object
				}
			}
		}
		if !branch.Corrupted {
			identity := coldBranchReferenceIdentity(branch.FamilyID, branch.BranchID, branch.PublicationID)
			if err := addFilesystemRoots(identity, branch.Roots.FilesystemRoot); err != nil {
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
		if publication.CandidateRoots.MemorySnapshot != 0 && !memoryVolatile && !state.AbandonedOrigins[publication.CandidateRoots.MemorySnapshot] {
			origin := ttcsnapshot.SnapshotID(publication.CandidateRoots.MemorySnapshot)
			manifest, err := s.packs.Manifest(origin)
			if err != nil {
				return fail(fmt.Errorf("reading publication memory roots: %w", err))
			}
			if coldMemoryReferences[origin] == nil {
				coldMemoryReferences[origin] = make(map[string]ttcsnapshot.ObjectID)
			}
			identity := coldBranchReferenceIdentity(publication.FamilyID, publication.BranchID, publication.PublicationID)
			for name, object := range manifest.References {
				if strings.HasPrefix(name, "root/") {
					coldMemoryReferences[origin][coldMemoryReferenceName(origin, identity, name, object)] = object
				}
			}
		}
		if publication.CandidateRoots.FilesystemRoot != "" {
			identity := coldBranchReferenceIdentity(publication.FamilyID, publication.BranchID, publication.PublicationID)
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

	// Reconstruct the temporary roots that cover a provider-to-shared handoff.
	// The intent is written before the first coordinator mutation, so a new
	// lease holder can close every crash prefix without relying on the previous
	// coordinator's local operation receipts.
	publicationBridgeReferencesByOrigin := make(map[ttcsnapshot.SnapshotID]map[string]ttcsnapshot.ObjectID)
	bridgeIntents, err := loadSharedPublicationBridgeIntents(s.sharedRoot)
	if err != nil {
		return fail(fmt.Errorf("reading publication bridge intents: %w", err))
	}
	for _, intent := range bridgeIntents {
		if !publicationBridgeIntentNeeded(state, intent) {
			continue
		}
		origin := ttcsnapshot.SnapshotID(intent.Snapshot)
		manifest, err := s.packs.Manifest(origin)
		if err != nil {
			return fail(fmt.Errorf("reading publication bridge origin %d: %w", origin, err))
		}
		roots := make(map[string]ttcsnapshot.ObjectID)
		for _, bridge := range publicationBridgeReferences(origin, manifest.References) {
			roots[bridge.Name] = bridge.Object
		}
		publicationBridgeReferencesByOrigin[origin] = roots
	}

	// Query every unfenced execution incarnation. If one cannot prove its
	// claims, GC remains unavailable, but normal reads and execution continue.
	// Claims must be part of ImportOrigin's initial baseline: importing an
	// unreferenced object first would correctly make it reclaiming, after which
	// a later retain is forbidden from resurrecting it.
	claimReferences, err := s.activeSharedClaimReferences(state, lease.ExpiresAt)
	if err != nil {
		return fail(err)
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
		if state.AbandonedOrigins[uint64(manifest.Snapshot)] {
			busy, discardErr := s.packs.DiscardHandoff(manifest.Snapshot)
			if discardErr != nil {
				return fail(fmt.Errorf("discarding abandoned snapshot origin %d: %w", manifest.Snapshot, discardErr))
			}
			if busy {
				return fail(fmt.Errorf("discarding abandoned snapshot origin %d: pack is busy", manifest.Snapshot))
			}
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
		// Retain every provider baseline long enough to import all origins.
		// Parent origins may sort after their children, and ImportOrigin correctly
		// forbids resurrecting a zero-count object. Namespacing ordinary roots
		// avoids collisions between repositories; incoming-edge bridge names stay
		// canonical so a subsequently imported parent can replace them in place.
		references := make(map[string]ttcsnapshot.ObjectID, len(manifest.References))
		for name, object := range manifest.References {
			live, err := manifestReferenceIsLive(state, name)
			if err != nil {
				return fail(fmt.Errorf("validating snapshot %d manifest reference: %w", manifest.Snapshot, err))
			}
			if !live {
				continue
			}
			if strings.HasPrefix(name, "root/bridge/edge/") {
				references[name] = object
			} else {
				references[providerBaselineReferenceName(manifest.Snapshot, name)] = object
			}
		}
		for name, object := range coldMemoryReferences[manifest.Snapshot] {
			references[name] = object
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
		for name, object := range publicationBridgeReferencesByOrigin[manifest.Snapshot] {
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
		if !strings.HasPrefix(name, "root/provider/") {
			continue
		}
		operation := fmt.Sprintf("rebuild/%016x/release/%s", lease.Generation, name)
		if err := ledger.ApplyReference(operation, name, object, false); err != nil {
			return fail(fmt.Errorf("releasing provider baseline reference %q: %w", name, err))
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
	for origin := range coldMemoryReferences {
		if _, ok := imported[origin]; !ok {
			return fail(fmt.Errorf("cold memory references shared origin %d without a committed manifest", origin))
		}
	}
	for origin := range publicationBridgeReferencesByOrigin {
		if _, ok := imported[origin]; !ok {
			return fail(fmt.Errorf("publication bridge references shared origin %d without a committed manifest", origin))
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
	s.compactionPending = true
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
	s.compactionPending = false
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
	if err := fn(s.ledger); err != nil {
		return err
	}
	// The semantic WAL records crash-safe transitions, not retained history.
	// Long migration chains can perform millions of short-lived edge updates
	// while converging back to a tiny live graph. Bound restart time and local
	// scratch growth by checkpointing the resulting state once the WAL reaches
	// the same scale as other snapshot-graph journals.
	if s.ledger.JournalBytes() >= sharedReferenceLedgerCheckpointBytes {
		return s.ledger.Checkpoint()
	}
	return nil
}

const sharedReferenceLedgerCheckpointBytes = 16 << 20

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
		origin := ttcsnapshot.SnapshotID(snapshotID)
		if err := ledger.ImportOrigin(origin, objects, providerBaselineReferences(origin, manifest.References)); err != nil {
			return fmt.Errorf("importing origin into reference ledger: %w", err)
		}
		return nil
	})
}

// ApplySharedSnapshotReference records an idempotent publication bridge at
// the shared-GC coordinator. Only the current volatile provider may create a
// bridge. After authority moves to shared storage, any authenticated member
// may release the exact bridge: the immutable name/object binding prevents it
// from releasing another consumer's reference.
func (s *sharedReferenceService) ApplySharedSnapshotReference(_ context.Context, peer string, snapshotID uint64, operation, name, objectText string, retain bool) error {
	object, err := ttcsnapshot.ParseObjectID(objectText)
	if err != nil {
		return err
	}
	if uint64(object.Snapshot) != snapshotID {
		return fmt.Errorf("shared reference object %s does not belong to snapshot %d", object, snapshotID)
	}
	wantPrefix := fmt.Sprintf("root/publication-bridge/%016x/", snapshotID)
	if !strings.HasPrefix(name, wantPrefix) {
		return fmt.Errorf("invalid publication bridge name %q for snapshot %d", name, snapshotID)
	}
	if retain {
		if err := s.authorizeProvider(peer, snapshotID); err != nil {
			return err
		}
	} else {
		_, volatile, err := s.authority.SnapshotProvider(snapshotID)
		if err != nil {
			return err
		}
		if volatile {
			if err := s.authorizeProvider(peer, snapshotID); err != nil {
				return err
			}
		} else if err := s.reconcileAuthoritativeColdReferences(); err != nil {
			// Cold branch roots selected by Raft must reach the ledger before
			// their temporary publication bridge is released.
			return fmt.Errorf("reconciling replacement roots: %w", err)
		}
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
			Sequence: input.Sequence, Operation: input.Operation,
			Name:   providerBaselineReferenceName(ttcsnapshot.SnapshotID(snapshotID), input.Name),
			Object: object, Retain: input.Retain,
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
		return applySharedRootMutation(ledger, names, roots, action, retain)
	})
}

// applySharedRootMutation preserves release idempotency across coordinator
// rebuilds. The reference ledger keeps operation receipts while one
// coordinator is active, but a newly elected coordinator reconstructs the
// current reference set rather than every historical receipt. A crash after a
// release commit and before the branch intent advances therefore legitimately
// retries against an absent exact name. Retains remain strict, and a release
// that finds the name bound to another immutable object is still rejected.
func applySharedRootMutation(ledger *ttcsnapshot.ReferenceLedger, names []string, roots map[string]ttcsnapshot.ObjectID, action string, retain bool) error {
	if ledger == nil {
		return errors.New("shared reference ledger is unavailable")
	}
	for _, name := range names {
		id := roots[name]
		if !retain {
			current, exists := ledger.References()[name]
			if !exists {
				continue
			}
			if current != id {
				return fmt.Errorf("reference %q selects %s, not %s", name, current, id)
			}
		}
		idempotency := fmt.Sprintf("shared-%s/%s", action, name)
		if err := ledger.ApplyReference(idempotency, name, id, retain); err != nil {
			// Concurrent batches are serialized inside the ledger, but the
			// observation above is intentionally lock-free from this layer. If an
			// identical release won that race, its absent exact name is success.
			if !retain {
				if _, exists := ledger.References()[name]; !exists {
					continue
				}
			}
			return err
		}
	}
	return nil
}

func (s *sharedReferenceService) mutateReference(operation, name string, object ttcsnapshot.ObjectID, retain bool) error {
	return s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		return applySharedReferenceMutation(ledger, operation, name, object, retain)
	})
}

// applySharedReferenceMutation applies a single reference mutation while
// preserving release idempotency across coordinator rebuilds. A rebuilt
// shared ledger contains the authoritative reference set, but intentionally
// does not retain every historical operation receipt. Consequently, replaying
// a completed release against an absent exact name is success. A conflicting
// live binding remains an error and retains are always strict.
func applySharedReferenceMutation(ledger *ttcsnapshot.ReferenceLedger, operation, name string, object ttcsnapshot.ObjectID, retain bool) error {
	if ledger == nil {
		return errors.New("shared reference ledger is unavailable")
	}
	if !retain {
		current, exists := ledger.References()[name]
		if !exists {
			return nil
		}
		if current != object {
			return fmt.Errorf("reference %q selects %s, not %s", name, current, object)
		}
	}
	return ledger.ApplyReference(operation, name, object, retain)
}

func isAuthoritativeColdReference(name string) bool {
	return strings.HasPrefix(name, "root/cold-memory/") || strings.HasPrefix(name, "root/cold/")
}

func isProviderBaselineReference(name string) bool {
	if isAuthoritativeColdReference(name) || strings.HasPrefix(name, "root/bridge/edge/") || strings.HasPrefix(name, "root/publication-bridge/") {
		return false
	}
	return strings.HasPrefix(name, "root/") || strings.HasPrefix(name, "pin/")
}

func providerBaselineCanBeReleased(state *cluster.AuthorityState, name string, object ttcsnapshot.ObjectID) bool {
	if state == nil || !isProviderBaselineReference(name) {
		return false
	}
	_, volatile := state.VolatileOrigins[uint64(object.Snapshot)]
	return !volatile && !state.AbandonedOrigins[uint64(object.Snapshot)]
}

// coldBranchReferenceIdentity makes a durable branch root activation unique.
// A branch may stop, restart, and stop again without changing an immutable
// filesystem root or memory snapshot. PublicationID distinguishes those
// lifetimes while remaining identical on both sides of the Ready->Committed
// Raft transition, so replacement is count-neutral and an old retain receipt
// can never suppress a later activation.
func coldBranchReferenceIdentity(family, branch, publicationID string) string {
	return "branch\x00" + family + "\x00" + branch + "\x00" + publicationID
}

func coldMemoryReferenceName(origin ttcsnapshot.SnapshotID, identity, source string, object ttcsnapshot.ObjectID) string {
	digest := sha256.Sum256([]byte(identity + "\x00" + source + "\x00" + object.String()))
	return fmt.Sprintf("root/cold-memory/%016x/%x", uint64(origin), digest)
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
	memory := make(map[ttcsnapshot.SnapshotID]map[string]struct{})
	addMemory := func(snapshot uint64, identity string) {
		if snapshot == 0 {
			return
		}
		if _, volatile := state.VolatileOrigins[snapshot]; !volatile && !state.AbandonedOrigins[snapshot] {
			origin := ttcsnapshot.SnapshotID(snapshot)
			if memory[origin] == nil {
				memory[origin] = make(map[string]struct{})
			}
			memory[origin][identity] = struct{}{}
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
			if state.AbandonedOrigins[uint64(root.Snapshot)] {
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
		identity := coldBranchReferenceIdentity(branch.FamilyID, branch.BranchID, branch.PublicationID)
		addMemory(branch.Roots.MemorySnapshot, identity)
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
		identity := coldBranchReferenceIdentity(publication.FamilyID, publication.BranchID, publication.PublicationID)
		addMemory(publication.CandidateRoots.MemorySnapshot, identity)
		if err := addFilesystem(identity, publication.CandidateRoots.FilesystemRoot); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
		}
	}
	for snapshot, identities := range memory {
		manifest, err := s.packs.Manifest(snapshot)
		if err != nil {
			return nil, fmt.Errorf("reading authoritative memory origin %d: %w", snapshot, err)
		}
		for identity := range identities {
			for name, object := range manifest.References {
				if !strings.HasPrefix(name, "root/") {
					continue
				}
				desired[coldMemoryReferenceName(snapshot, identity, name, object)] = object
			}
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
		if _, structural, err := incomingEdgeParent(name); err != nil {
			return err
		} else if structural {
			live, err := manifestReferenceIsLive(state, name)
			if err != nil {
				return err
			}
			if !live {
				// An incoming-edge bridge stands in for a parent that was not
				// present when this child origin was imported. If that parent is
				// later abandoned or permanently retired, no future import can
				// replace the bridge with a structural edge. Release it in the
				// live coordinator as well as filtering it during rebuild; waiting
				// for coordinator turnover otherwise pins the child pack forever.
				release = append(release, name)
			}
			continue
		}
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
		mutations := make([]ttcsnapshot.SnapshotReferenceMutation, 0, len(retain)+len(release))
		for _, name := range retain {
			object := desired[name]
			digest := sha256.Sum256([]byte("retain\x00" + name + "\x00" + object.String()))
			mutations = append(mutations, ttcsnapshot.SnapshotReferenceMutation{
				Operation: fmt.Sprintf("authority-root-retain/%x", digest),
				Name:      name, Object: object, Retain: true,
			})
		}
		for _, name := range release {
			object := current[name]
			digest := sha256.Sum256([]byte("release\x00" + name + "\x00" + object.String()))
			mutations = append(mutations, ttcsnapshot.SnapshotReferenceMutation{
				Operation: fmt.Sprintf("authority-root-release/%x", digest),
				Name:      name, Object: object,
			})
		}
		return applySharedReferenceBatches(ledger, mutations)
	})
}

// reconcileInactiveGraphReferences removes active-consumer roots whose owning
// daemon incarnation has disappeared. Ordinary branch removal releases these
// roots directly. A hard node loss cannot do so, and keeping the same shared-GC
// coordinator used to leave the dead node's remote/... claims pinned until an
// unrelated coordinator election rebuilt the ledger.
//
// Only releases are synthesized here. A newly written intent appears in its
// node inventory before its retain RPC, so installing missing retains from one
// sampled inventory could race a concurrent release and resurrect a dead root.
// The normal retain path remains responsible for additions. Conversely, an
// absent remote/... name cannot be reused by another lifetime: operation names
// are branch/transaction-unique, making release-only reconciliation monotonic.
func (s *sharedReferenceService) reconcileInactiveGraphReferences(lease cluster.SharedGCLease) error {
	state, err := s.authority.AuthorityState()
	if err != nil {
		return err
	}
	desiredByOrigin, err := s.activeSharedClaimReferences(state, lease.ExpiresAt)
	if err != nil {
		return err
	}
	desired := make(map[string]ttcsnapshot.ObjectID)
	for _, roots := range desiredByOrigin {
		for name, object := range roots {
			desired[name] = object
		}
	}
	var current map[string]ttcsnapshot.ObjectID
	if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		current = ledger.References()
		return nil
	}); err != nil {
		return err
	}
	mutations, err := inactiveGraphReferenceMutations(state, current, desired)
	if err != nil {
		return err
	}
	return s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		return applySharedReferenceBatches(ledger, mutations)
	})
}

func inactiveGraphReferenceMutations(state *cluster.AuthorityState, current, desired map[string]ttcsnapshot.ObjectID) ([]ttcsnapshot.SnapshotReferenceMutation, error) {
	if state == nil {
		return nil, errors.New("shared authority state is unavailable")
	}
	names := make([]string, 0)
	for name, object := range current {
		if !strings.HasPrefix(name, "remote/") {
			continue
		}
		if wanted, ok := desired[name]; ok {
			if wanted != object {
				return nil, fmt.Errorf("active reference %q changed from %s to %s", name, object, wanted)
			}
			continue
		}
		// Provider authority only moves node -> shared. Restricting cleanup to
		// origins already shared in the inventory's linearizable Raft view keeps
		// it disjoint from an in-progress provider handoff.
		if _, volatile := state.VolatileOrigins[uint64(object.Snapshot)]; volatile {
			continue
		}
		if state.AbandonedOrigins[uint64(object.Snapshot)] {
			continue
		}
		if _, retired := state.RetiredOrigins[uint64(object.Snapshot)]; retired {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	mutations := make([]ttcsnapshot.SnapshotReferenceMutation, 0, len(names))
	for _, name := range names {
		object := current[name]
		digest := sha256.Sum256([]byte("inactive-release\x00" + name + "\x00" + object.String()))
		mutations = append(mutations, ttcsnapshot.SnapshotReferenceMutation{
			Operation: fmt.Sprintf("inactive-claim-release/%x", digest),
			Name:      name, Object: object,
		})
	}
	return mutations, nil
}

// applySharedReferenceBatches bounds both WAL record size and the number of
// full ledger-state clones/fsyncs. Reconciliation can release one structural
// edge per guest page; committing each edge independently made snapshot
// retirement quadratic and turned a modest container into minutes of busy
// shutdown work. 1024 records remain comfortably below the ledger's 8 MiB
// record limit even with maximum-length names.
func applySharedReferenceBatches(ledger *ttcsnapshot.ReferenceLedger, mutations []ttcsnapshot.SnapshotReferenceMutation) error {
	const maxBatch = 1024
	for len(mutations) != 0 {
		count := min(len(mutations), maxBatch)
		if err := ledger.ApplyReferences(mutations[:count]); err != nil {
			return err
		}
		mutations = mutations[count:]
	}
	return nil
}

// reconcilePublicationBridgeIntents rolls forward bridge cleanup after a
// publisher crash or lost response. Nonterminal operations keep their bridge;
// committed publications, terminal handoffs, and generic origin transfers
// already have replacement roots in Raft or node inventories.
func (s *sharedReferenceService) reconcilePublicationBridgeIntents() error {
	state, err := s.authority.AuthorityState()
	if err != nil {
		return err
	}
	intents, err := loadSharedPublicationBridgeIntents(s.sharedRoot)
	if err != nil {
		return err
	}
	// Install Raft-selected cold roots before removing any publication bridge.
	if err := s.reconcileAuthoritativeColdReferences(); err != nil {
		return err
	}
	for _, intent := range intents {
		if state.AbandonedOrigins[intent.Snapshot] {
			origin := ttcsnapshot.SnapshotID(intent.Snapshot)
			busy, discardErr := s.packs.DiscardHandoff(origin)
			if discardErr != nil {
				return fmt.Errorf("discarding abandoned publication bridge %d: %w", origin, discardErr)
			}
			if busy {
				return fmt.Errorf("discarding abandoned publication bridge %d: pack is busy", origin)
			}
			if err := removeSharedPublicationBridgeIntent(s.sharedRoot, origin); err != nil {
				return err
			}
			continue
		}
		if _, volatile := state.VolatileOrigins[intent.Snapshot]; volatile || publicationBridgeIntentNeeded(state, intent) {
			continue
		}
		origin := ttcsnapshot.SnapshotID(intent.Snapshot)
		manifest, err := s.packs.Manifest(origin)
		if errors.Is(err, os.ErrNotExist) {
			if err := removeSharedPublicationBridgeIntent(s.sharedRoot, origin); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		for _, bridge := range publicationBridgeReferences(origin, manifest.References) {
			if err := s.mutateReference(bridge.ReleaseOperation, bridge.Name, bridge.Object, false); err != nil {
				return err
			}
		}
		if err := removeSharedPublicationBridgeIntent(s.sharedRoot, origin); err != nil {
			return err
		}
	}
	return nil
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
	if err := s.reconcilePublicationBridgeIntents(); err != nil {
		return 0, err
	}
	if err := s.reconcileAuthoritativeColdReferences(); err != nil {
		return 0, err
	}
	if err := s.reconcileInactiveGraphReferences(lease); err != nil {
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
	state, err := s.authority.AuthorityState()
	if err != nil {
		return collected, err
	}
	if collected != 0 {
		s.mu.Lock()
		s.compactionPending = true
		s.mu.Unlock()
	}
	s.mu.RLock()
	compactionPending := s.compactionPending
	s.mu.RUnlock()
	if compactionPending {
		// Rewrite at most one partially-live origin per maintenance pass. Logical
		// collection is already durable in the ledger; compaction only changes its
		// reader-fenced physical representation and therefore may be retried after
		// interruption without affecting authority. A new coordinator performs one
		// recovery scan; steady-state scans happen only after collection actually
		// changes the physical live set.
		var origins []ttcsnapshot.SnapshotID
		if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
			origins = ledger.Origins()
			return nil
		}); err != nil {
			return collected, err
		}
		retryCompaction := false
		for _, origin := range origins {
			if err := ctx.Err(); err != nil {
				return collected, err
			}
			encoded := uint64(origin)
			if encoded >= state.NextSnapshotID || state.AbandonedOrigins[encoded] {
				continue
			}
			if _, volatile := state.VolatileOrigins[encoded]; volatile {
				continue
			}
			if _, retired := state.RetiredOrigins[encoded]; retired {
				continue
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
			if !ready {
				retryCompaction = true
				continue
			}
			if len(objects) == 0 {
				continue
			}
			if _, busy, err := s.packs.Reconcile(origin); err != nil {
				return collected, err
			} else if busy {
				retryCompaction = true
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
			} else if busy {
				retryCompaction = true
				continue
			}
			// Only one physical rewrite runs per pass. Revisit the remaining origins
			// after the next lease tick.
			retryCompaction = true
			break
		}
		s.mu.Lock()
		s.compactionPending = retryCompaction
		s.mu.Unlock()
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
		if uint64(snapshot) >= state.NextSnapshotID {
			// The origin was imported concurrently after the bulk authority
			// read. Refresh before classifying it; an absent entry only means
			// shared authority for IDs that had already been allocated.
			state, err = s.authority.AuthorityState()
			if err != nil {
				return collected, err
			}
			if uint64(snapshot) >= state.NextSnapshotID {
				continue
			}
		}
		if state.AbandonedOrigins[uint64(snapshot)] {
			// A provider-to-shared handoff imports the immutable graph before
			// changing its routing authority. Cancellation may permanently
			// abandon the still-volatile origin in that interval. The publisher
			// removes the speculative pack, but an already-active coordinator
			// must also forget its now-empty imported ledger origin. A rebuilt
			// coordinator naturally omits abandoned origins.
			if err := s.completeAbandonment(snapshot); err != nil {
				return collected, err
			}
			continue
		}
		if _, retired := state.RetiredOrigins[uint64(snapshot)]; retired {
			// Resume a crash after the irreversible Raft fence. In particular,
			// the ledger and physical pack may independently have completed
			// their idempotent cleanup steps.
			if err := s.completeRetirement(snapshot); err != nil {
				return collected, err
			}
			continue
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
		if _, wasVolatile := state.VolatileOrigins[uint64(snapshot)]; wasVolatile {
			// SnapshotProvider observed a provider transition after the bulk
			// state read above. Resolve that one transition again: it committed
			// either publication (ordinary shared retirement below) or permanent
			// abandonment. Once shared, an origin cannot become abandoned.
			latest, err := s.authority.AuthorityState()
			if err != nil {
				return collected, err
			}
			if latest.AbandonedOrigins[uint64(snapshot)] {
				if err := s.completeAbandonment(snapshot); err != nil {
					return collected, err
				}
				continue
			}
			if _, retired := latest.RetiredOrigins[uint64(snapshot)]; retired {
				if err := s.completeRetirement(snapshot); err != nil {
					return collected, err
				}
				continue
			}
			if _, stillVolatile := latest.VolatileOrigins[uint64(snapshot)]; stillVolatile {
				// A stale follower read must never authorize shared-pack mutation.
				continue
			}
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
		collected, err := s.collectAndRetire(ctx, lease, 64<<10)
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
	// Raft has already made the origin unreachable to new readers. Forget the
	// logical origin durably before deleting its bytes. If the daemon crashes
	// between these steps, RetiredOrigins is the permanent recovery work list;
	// deleting bytes first instead leaves a live ledger origin with no pack.
	if err := s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		return ledger.RetireOrigin(snapshot)
	}); err != nil {
		return err
	}
	busy, err := s.packs.CompleteRetirement(snapshot)
	if err != nil {
		return err
	}
	if busy {
		return fmt.Errorf("shared snapshot %d still has an active reader lease", snapshot)
	}
	return nil
}

// completeAbandonment removes the speculative data-plane state left when a
// provider-to-shared handoff is canceled after coordinator import but before
// the Raft authority switch. Raft's abandonment is already permanent, so no
// shared reader may newly acquire this origin and a missing handoff pack is an
// expected idempotent prefix rather than evidence of lost shared data.
func (s *sharedReferenceService) completeAbandonment(snapshot ttcsnapshot.SnapshotID) error {
	return s.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
		return discardAbandonedSnapshot(s.packs, ledger, snapshot)
	})
}

func discardAbandonedSnapshot(packs *ttcsnapshot.SharedPackStore, ledger *ttcsnapshot.ReferenceLedger, snapshot ttcsnapshot.SnapshotID) error {
	if packs == nil || ledger == nil {
		return errors.New("abandoned snapshot cleanup is unavailable")
	}
	busy, err := packs.DiscardHandoff(snapshot)
	if err != nil {
		return err
	}
	if busy {
		return fmt.Errorf("abandoned snapshot %d still has an active handoff reader lease", snapshot)
	}
	return ledger.RetireOrigin(snapshot)
}

var _ cluster.SharedSnapshotReferenceProvider = (*sharedReferenceService)(nil)
var _ cluster.SnapshotReferenceInventoryProvider = (*sharedReferenceService)(nil)
