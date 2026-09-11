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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"gvisor.dev/gvisor/pkg/sentry/ttcchunk"
	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

// openDistributedGraphRestore turns an immutable graph manifest into the
// descriptors consumed by the Sentry. The local object directory is always
// donated. A transaction-fenced resolver stream resolves cache misses without
// making the Sentry aware of provider placement or shared storage.
func (b *runtimeBackend) openDistributedGraphRestore(manifest *os.File, transaction string, token ttcsnapshot.ObjectRelayToken) (*graphMemoryRestore, error) {
	if b == nil || manifest == nil || b.snapshotData == nil {
		return nil, errors.New("distributed graph restore is unavailable")
	}
	encoded, err := io.ReadAll(manifest)
	if err != nil {
		return nil, err
	}
	if _, err := manifest.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	decoded, err := ttcchunk.DecodeManifest(encoded, nil)
	if err != nil {
		return nil, fmt.Errorf("decoding distributed graph manifest: %w", err)
	}
	chunks, err := ttcchunk.GraphChunkObjects(decoded)
	if err != nil {
		return nil, err
	}
	objects, err := os.Open(filepath.Join(b.activeDataRoot(), "objects"))
	if err != nil {
		return nil, err
	}
	restore := &graphMemoryRestore{manifest: manifest, objects: objects}
	available := make(map[ttcsnapshot.ObjectID]ttcsnapshot.Object)
	for _, id := range chunks {
		available[id] = ttcsnapshot.Object{ID: id, State: ttcsnapshot.ObjectLive, BulkLength: ttcsnapshot.MemoryChunkBytes}
	}
	if b.distributedSource == nil || b.distributedSource.authority == nil || b.snapshotShared == nil || b.snapshotGraph == nil {
		restore.close()
		return nil, errors.New("distributed graph restore has missing objects and no resolver")
	}
	resolver, err := ttcsnapshot.NewResolver(b.snapshotGraph, b.snapshotData,
		raftOriginLocator{authority: b.distributedSource.authority},
		clusterPeerSources{runtime: b.distributedSource, transaction: transaction}, b.snapshotShared)
	if err != nil {
		restore.close()
		return nil, err
	}
	restore.token = token
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		restore.close()
		return nil, err
	}
	server := os.NewFile(uintptr(fds[0]), "ttc-distributed-graph-server")
	restore.remote = os.NewFile(uintptr(fds[1]), "ttc-distributed-graph-client")
	allowed := make(map[ttcsnapshot.ObjectID]uint64, len(available))
	for id := range available {
		allowed[id] = ttcsnapshot.MemoryChunkBytes
	}
	go func() {
		err := ttcsnapshot.ServeObjectRelay(server, restore.token, graphRestoreObjectSource{resolver: resolver, objects: available}, allowed)
		b.reportColdRelayError("distributed graph memory", err)
	}()
	return restore, nil
}

type graphObjectProvider struct {
	graph        *ttcsnapshot.Store
	data         *ttcsnapshot.LocalDataStore
	authority    *cluster.RaftCatalog
	transactions cluster.TransactionCatalog
	nodeID       string
	incarnation  string
	shared       *sharedReferenceService
}

const graphSnapshotCatalogVersion = 1

type graphSnapshotCatalog struct {
	Version  int                    `json:"version"`
	Snapshot ttcsnapshot.SnapshotID `json:"snapshot"`
	Objects  []ttcsnapshot.Object   `json:"objects"`
}

type raftOriginLocator struct{ authority *cluster.RaftCatalog }

func (locator raftOriginLocator) Provider(_ context.Context, snapshot ttcsnapshot.SnapshotID) (string, bool, error) {
	provider, found, err := locator.authority.SnapshotProvider(uint64(snapshot))
	return provider.NodeID, found, err
}

type clusterObjectSource struct {
	client      *cluster.Client
	transaction string
}

func (source clusterObjectSource) Fetch(ctx context.Context, id ttcsnapshot.ObjectID) (io.ReadCloser, uint64, error) {
	return source.client.FetchObject(ctx, id.String(), source.transaction)
}

type clusterPeerSources struct {
	runtime     *distributedSourceRuntime
	transaction string
}

func (sources clusterPeerSources) Peer(_ context.Context, node string) (ttcsnapshot.ObjectSource, error) {
	client, err := sources.runtime.peer(node, false)
	if err != nil {
		return nil, err
	}
	return clusterObjectSource{client: client, transaction: sources.transaction}, nil
}

func (p *graphObjectProvider) OpenObject(_ context.Context, peer, objectText, transactionID string) (io.ReadCloser, uint64, error) {
	if p == nil || p.graph == nil || p.data == nil || p.authority == nil {
		return nil, 0, errors.New("snapshot graph provider is unavailable")
	}
	id, err := ttcsnapshot.ParseObjectID(objectText)
	if err != nil {
		return nil, 0, err
	}
	if err := p.authorizeSnapshot(peer, id.Snapshot, transactionID, false); err != nil {
		return nil, 0, err
	}
	object, ok := p.graph.Object(id)
	if !ok || object.State != ttcsnapshot.ObjectLive {
		return nil, 0, os.ErrNotExist
	}
	if object.BulkLength > math.MaxInt64 {
		return nil, 0, errors.New("snapshot object exceeds provider length limit")
	}
	file, err := p.data.Open(id, int64(object.BulkLength))
	if err != nil {
		return nil, 0, err
	}
	return file, object.BulkLength, nil
}

func (p *graphObjectProvider) authorizeSnapshot(peer string, snapshot ttcsnapshot.SnapshotID, transactionID string, allowTerminal bool) error {
	provider, found, err := p.authority.SnapshotProvider(uint64(snapshot))
	if err != nil {
		return err
	}
	if !found || provider.NodeID != p.nodeID {
		return fmt.Errorf("snapshot origin %d is not owned by node %q", snapshot, p.nodeID)
	}
	node, err := p.authority.Lookup(p.nodeID)
	if err != nil {
		return err
	}
	if node.Incarnation != p.incarnation || !node.LeaseActive(time.Now()) ||
		(node.State != cluster.NodeReady && node.State != cluster.NodePressured && node.State != cluster.NodeDraining) {
		return errors.New("snapshot provider incarnation is not currently authoritative")
	}
	if transactionID != "" {
		if p.transactions == nil {
			return errors.New("snapshot transaction catalog is unavailable")
		}
		transaction, err := p.transactions.Load(transactionID)
		if err != nil {
			// Branches may outlive the bounded Raft transaction history that
			// originally authorized their retain. A release is still fenced by its
			// unguessable operation name and exact object IDs, and can only remove
			// that previously-created reference. Requiring the retired transaction
			// here would make an old but live branch permanently unreclaimable.
			if allowTerminal && errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		// An immutable root may have been inherited through any number of local
		// and remote forks. Its provider is therefore not necessarily the source
		// node of the transaction that is currently fetching or retaining it.
		// Raft's snapshot-origin mapping above proves that this node is the unique
		// provider; the transaction must prove only that the authenticated caller
		// is its target. Exact object IDs then bound the requested authority.
		if transaction.Identity.TargetNodeID != peer {
			return errors.New("snapshot transaction does not authorize this requester")
		}
		if !allowTerminal && (transaction.State == cluster.TransactionCanceled || transaction.State == cluster.TransactionFailed) {
			return fmt.Errorf("snapshot transaction is %s", transaction.State)
		}
	}
	return nil
}

// OpenSnapshot serves graph metadata separately from bulk bytes. The catalog
// is immutable after Seal and does not transfer the provider's refcounts.
func (p *graphObjectProvider) OpenSnapshot(_ context.Context, peer string, snapshotID uint64, transactionID string) (io.ReadCloser, uint64, error) {
	if p == nil || p.graph == nil || p.authority == nil || snapshotID == 0 {
		return nil, 0, errors.New("snapshot graph provider is unavailable")
	}
	snapshot := ttcsnapshot.SnapshotID(snapshotID)
	if err := p.authorizeSnapshot(peer, snapshot, transactionID, false); err != nil {
		return nil, 0, err
	}
	record, ok := p.graph.Snapshot(snapshot)
	if !ok || record.State != ttcsnapshot.SnapshotSealedVolatile {
		return nil, 0, fmt.Errorf("snapshot origin %d is not sealed for peer reads", snapshot)
	}
	objects, err := p.graph.SnapshotObjects(snapshot)
	if err != nil {
		return nil, 0, err
	}
	encoded, err := json.Marshal(graphSnapshotCatalog{Version: graphSnapshotCatalogVersion, Snapshot: snapshot, Objects: objects})
	if err != nil {
		return nil, 0, err
	}
	if len(encoded) == 0 || len(encoded) > 512<<20 {
		return nil, 0, errors.New("snapshot metadata catalog exceeds transport limit")
	}
	return io.NopCloser(bytes.NewReader(encoded)), uint64(len(encoded)), nil
}

// OpenSnapshotObject serves one immutable metadata record without enumerating
// its origin. Tree edges in the returned record are the capabilities needed
// to discover the next bounded level; bulk bytes remain on OpenObject.
func (p *graphObjectProvider) OpenSnapshotObject(_ context.Context, peer string, snapshotID uint64, objectText, transactionID string) (io.ReadCloser, uint64, error) {
	if p == nil || p.graph == nil || p.authority == nil || snapshotID == 0 {
		return nil, 0, errors.New("snapshot graph provider is unavailable")
	}
	snapshot := ttcsnapshot.SnapshotID(snapshotID)
	id, err := ttcsnapshot.ParseObjectID(objectText)
	if err != nil {
		return nil, 0, err
	}
	if id.Snapshot != snapshot {
		return nil, 0, fmt.Errorf("snapshot %d cannot serve object from origin %d", snapshot, id.Snapshot)
	}
	if err := p.authorizeSnapshot(peer, snapshot, transactionID, false); err != nil {
		return nil, 0, err
	}
	record, ok := p.graph.Snapshot(snapshot)
	if !ok || record.State != ttcsnapshot.SnapshotSealedVolatile {
		return nil, 0, fmt.Errorf("snapshot origin %d is not sealed for peer reads", snapshot)
	}
	object, ok := p.graph.Object(id)
	if !ok || object.State != ttcsnapshot.ObjectLive {
		return nil, 0, os.ErrNotExist
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, 0, err
	}
	if len(encoded) == 0 || len(encoded) > 8<<20 {
		return nil, 0, errors.New("snapshot metadata object exceeds transport limit")
	}
	return io.NopCloser(bytes.NewReader(encoded)), uint64(len(encoded)), nil
}

func graphReferenceRoots(snapshot ttcsnapshot.SnapshotID, operation string, bindings []cluster.SnapshotRootBinding) (map[string]ttcsnapshot.ObjectID, error) {
	if snapshot == 0 || operation == "" || len(bindings) == 0 || len(bindings) > 4096 {
		return nil, errors.New("invalid snapshot reference operation")
	}
	result := make(map[string]ttcsnapshot.ObjectID, len(bindings))
	seenSlots := make(map[uint32]struct{}, len(bindings))
	for _, binding := range bindings {
		if _, exists := seenSlots[binding.Slot]; exists {
			return nil, fmt.Errorf("snapshot reference operation repeats slot %d", binding.Slot)
		}
		seenSlots[binding.Slot] = struct{}{}
		id, err := ttcsnapshot.ParseObjectID(binding.Object)
		if err != nil {
			return nil, err
		}
		if id.Snapshot != snapshot {
			return nil, fmt.Errorf("root object %s does not belong to snapshot origin %d", id, snapshot)
		}
		name := fmt.Sprintf("remote/%d/%s/%08x", snapshot, operation, binding.Slot)
		result[name] = id
	}
	return result, nil
}

// groupGraphRootBindings partitions a branch's bounded logical roots by their
// immutable origin. Fork remains O(number of memory resources), not O(pages):
// every provider receives only the radix roots it owns and accounts all
// descendants through the graph's lazy edge references.
func groupGraphRootBindings(objects []string) (map[ttcsnapshot.SnapshotID][]cluster.SnapshotRootBinding, error) {
	if len(objects) == 0 || len(objects) > 4096 {
		return nil, errors.New("invalid graph root binding count")
	}
	groups := make(map[ttcsnapshot.SnapshotID][]cluster.SnapshotRootBinding)
	for slot, encoded := range objects {
		id, err := ttcsnapshot.ParseObjectID(encoded)
		if err != nil {
			return nil, fmt.Errorf("parsing graph root slot %d: %w", slot, err)
		}
		if id.Snapshot == 0 || id.Kind != ttcsnapshot.ObjectKindMemoryNode {
			return nil, fmt.Errorf("graph root slot %d selects non-memory-root object %s", slot, id)
		}
		groups[id.Snapshot] = append(groups[id.Snapshot], cluster.SnapshotRootBinding{
			Slot: uint32(slot), Object: id.String(),
		})
	}
	return groups, nil
}

// RetainSnapshotRoots activates one transaction-fenced root batch in the
// volatile origin's authoritative graph. Store.RetainRoots supplies the
// durable idempotency boundary; neither descendant edges nor counts enter
// Raft or this request.
func (p *graphObjectProvider) RetainSnapshotRoots(ctx context.Context, peer string, snapshotID uint64, transaction, operation string, bindings []cluster.SnapshotRootBinding) error {
	snapshot := ttcsnapshot.SnapshotID(snapshotID)
	_, volatile, err := p.authority.SnapshotProvider(snapshotID)
	if err != nil {
		return err
	}
	if !volatile {
		if err := p.authorizeReferenceTransaction(peer, transaction, false); err != nil {
			return err
		}
		return p.shared.mutateRoots(snapshot, operation, bindings, true)
	}
	if err := p.authorizeSnapshot(peer, snapshot, transaction, false); err != nil {
		return err
	}
	roots, err := graphReferenceRoots(snapshot, operation, bindings)
	if err != nil {
		return err
	}
	return p.graph.RetainRoots(roots)
}

// ReleaseSnapshotRoots deactivates the exact retained batch. Expected object
// IDs fence stale or malformed releases from deleting another operation's
// roots, and completed retries are no-ops.
func (p *graphObjectProvider) ReleaseSnapshotRoots(ctx context.Context, peer string, snapshotID uint64, transaction, operation string, bindings []cluster.SnapshotRootBinding) error {
	snapshot := ttcsnapshot.SnapshotID(snapshotID)
	_, volatile, err := p.authority.SnapshotProvider(snapshotID)
	if err != nil {
		return err
	}
	if !volatile {
		if err := p.authorizeReferenceTransaction(peer, transaction, true); err != nil {
			return err
		}
		return p.shared.mutateRoots(snapshot, operation, bindings, false)
	}
	if err := p.authorizeSnapshot(peer, snapshot, transaction, true); err != nil {
		return err
	}
	roots, err := graphReferenceRoots(snapshot, operation, bindings)
	if err != nil {
		return err
	}
	return p.graph.ReleaseRetainedRoots(roots)
}

// ApplySnapshotReference installs or removes one cross-owner bridge while the
// child origin remains volatile on this provider. The bridge is represented as
// a graph root and is folded back into an internal edge if both origins later
// move to the shared ledger.
func (p *graphObjectProvider) ApplySnapshotReference(ctx context.Context, peer string, snapshotID uint64, operation, name, objectText string, retain bool) error {
	if operation == "" || name == "" {
		return errors.New("invalid owner reference mutation")
	}
	snapshot := ttcsnapshot.SnapshotID(snapshotID)
	_, volatile, err := p.authority.SnapshotProvider(snapshotID)
	if err != nil {
		return err
	}
	object, err := ttcsnapshot.ParseObjectID(objectText)
	if err != nil {
		return err
	}
	if !volatile {
		if object.Snapshot != snapshot {
			return fmt.Errorf("owner reference object %s does not belong to snapshot %d", object, snapshot)
		}
		return p.shared.mutateReference(operation, name, object, retain)
	}
	if err := p.authorizeSnapshot(peer, snapshot, "", true); err != nil {
		return err
	}
	if object.Snapshot != snapshot {
		return fmt.Errorf("owner reference object %s does not belong to snapshot %d", object, snapshot)
	}
	bridge := map[string]ttcsnapshot.ObjectID{"bridge/" + name: object}
	if retain {
		return p.graph.RetainRoots(bridge)
	}
	return p.graph.ReleaseRetainedRoots(bridge)
}

func (p *graphObjectProvider) authorizeReferenceTransaction(peer, transactionID string, allowTerminal bool) error {
	if transactionID == "" {
		return nil
	}
	if p.transactions == nil {
		return errors.New("snapshot transaction catalog is unavailable")
	}
	transaction, err := p.transactions.Load(transactionID)
	if err != nil {
		if allowTerminal && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if transaction.Identity.TargetNodeID != peer {
		return errors.New("snapshot transaction does not authorize this requester")
	}
	if !allowTerminal && (transaction.State == cluster.TransactionCanceled || transaction.State == cluster.TransactionFailed) {
		return fmt.Errorf("snapshot transaction is %s", transaction.State)
	}
	return nil
}

var _ cluster.ImmutableObjectProvider = (*graphObjectProvider)(nil)
var _ cluster.ImmutableSnapshotProvider = (*graphObjectProvider)(nil)
var _ cluster.SnapshotReferenceProvider = (*graphObjectProvider)(nil)

func decodeGraphSnapshotObjects(reader io.Reader, length uint64, snapshot ttcsnapshot.SnapshotID) ([]ttcsnapshot.Object, error) {
	if reader == nil || length == 0 || length > 512<<20 {
		return nil, errors.New("invalid immutable snapshot catalog length")
	}
	var wire graphSnapshotCatalog
	decoder := json.NewDecoder(io.LimitReader(reader, int64(length)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return nil, fmt.Errorf("decoding immutable snapshot catalog: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("immutable snapshot catalog contains trailing JSON")
	}
	if wire.Version != graphSnapshotCatalogVersion || wire.Snapshot != snapshot {
		return nil, fmt.Errorf("immutable snapshot catalog identifies version %d snapshot %d, want version %d snapshot %d", wire.Version, wire.Snapshot, graphSnapshotCatalogVersion, snapshot)
	}
	for _, object := range wire.Objects {
		if object.ID.Snapshot != snapshot {
			return nil, fmt.Errorf("snapshot %d catalog contains foreign object %s", snapshot, object.ID)
		}
	}
	if _, err := ttcsnapshot.NewImmutableCatalog(wire.Objects); err != nil {
		return nil, err
	}
	return wire.Objects, nil
}

func decodeGraphSnapshotObject(reader io.Reader, length uint64, expected ttcsnapshot.ObjectID) (ttcsnapshot.Object, error) {
	if reader == nil || length == 0 || length > 8<<20 {
		return ttcsnapshot.Object{}, errors.New("invalid immutable snapshot object length")
	}
	var object ttcsnapshot.Object
	decoder := json.NewDecoder(io.LimitReader(reader, int64(length)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&object); err != nil {
		return ttcsnapshot.Object{}, fmt.Errorf("decoding immutable snapshot object: %w", err)
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return ttcsnapshot.Object{}, errors.New("immutable snapshot object contains trailing JSON")
	}
	if object.ID != expected {
		return ttcsnapshot.Object{}, fmt.Errorf("immutable snapshot object identifies %s, want %s", object.ID, expected)
	}
	// NewImmutableCatalog supplies the package's complete structural validator
	// without exporting mutable graph internals.
	if _, err := ttcsnapshot.NewImmutableCatalog([]ttcsnapshot.Object{object}); err != nil {
		return ttcsnapshot.Object{}, err
	}
	return object, nil
}

// immutableSnapshotCatalog resolves metadata by origin authority. Unlike the
// bulk resolver it never installs metadata into the local authoritative graph.
func (b *runtimeBackend) immutableSnapshotCatalog(ctx context.Context, roots []ttcsnapshot.ObjectID, transaction string) (*ttcsnapshot.ImmutableCatalog, error) {
	if b == nil || len(roots) == 0 {
		return nil, errors.New("invalid immutable snapshot catalog request")
	}
	return ttcsnapshot.ResolveImmutableCatalog(roots, func(origin ttcsnapshot.SnapshotID) ([]ttcsnapshot.Object, error) {
		return b.immutableSnapshotOriginObjects(ctx, origin, transaction)
	})
}

// lazyImmutableSnapshotCatalog resolves one metadata object at a time from its
// immutable origin. It is the filesystem lookup path; memory reconstruction
// continues using immutableSnapshotCatalog because building a process image
// necessarily enumerates the complete radix page map.
func (b *runtimeBackend) lazyImmutableSnapshotCatalog(ctx context.Context, roots []ttcsnapshot.ObjectID, transaction string) (*ttcsnapshot.LazyImmutableCatalog, error) {
	if b == nil || len(roots) == 0 {
		return nil, errors.New("invalid lazy immutable snapshot catalog request")
	}
	for _, root := range roots {
		if root.IsZero() {
			return nil, errors.New("lazy immutable snapshot catalog contains an empty root")
		}
	}
	return ttcsnapshot.NewLazyImmutableCatalog(func(id ttcsnapshot.ObjectID) (ttcsnapshot.Object, error) {
		if b.snapshotGraph != nil {
			if record, ok := b.snapshotGraph.Snapshot(id.Snapshot); ok && record.State != ttcsnapshot.SnapshotWorking {
				object, ok := b.snapshotGraph.Object(id)
				if !ok {
					return ttcsnapshot.Object{}, os.ErrNotExist
				}
				return object, nil
			}
		}
		if b.distributedSource == nil || b.distributedSource.authority == nil {
			return ttcsnapshot.Object{}, os.ErrNotExist
		}
		provider, volatile, err := b.distributedSource.authority.SnapshotProvider(uint64(id.Snapshot))
		if err != nil {
			return ttcsnapshot.Object{}, err
		}
		if !volatile {
			if b.snapshotShared == nil {
				return ttcsnapshot.Object{}, errors.New("shared snapshot repository is unavailable")
			}
			manifest, err := b.snapshotShared.Manifest(id.Snapshot)
			if err != nil {
				return ttcsnapshot.Object{}, err
			}
			for _, packed := range manifest.Objects {
				if packed.Object.ID == id {
					return packed.Object, nil
				}
			}
			return ttcsnapshot.Object{}, os.ErrNotExist
		}
		if provider.NodeID == b.nodeID {
			return ttcsnapshot.Object{}, fmt.Errorf("local node owns snapshot %d but has no sealed graph metadata", id.Snapshot)
		}
		peer, err := b.distributedSource.peer(provider.NodeID, false)
		if err != nil {
			return ttcsnapshot.Object{}, err
		}
		reader, length, err := peer.FetchSnapshotObject(ctx, uint64(id.Snapshot), id.String(), transaction)
		if err != nil {
			return ttcsnapshot.Object{}, err
		}
		defer reader.Close()
		return decodeGraphSnapshotObject(reader, length, id)
	})
}

func (b *runtimeBackend) immutableSnapshotOriginObjects(ctx context.Context, snapshot ttcsnapshot.SnapshotID, transaction string) ([]ttcsnapshot.Object, error) {
	if b.snapshotGraph != nil {
		if record, ok := b.snapshotGraph.Snapshot(snapshot); ok && record.State != ttcsnapshot.SnapshotWorking {
			return b.snapshotGraph.SnapshotObjects(snapshot)
		}
	}
	if b.distributedSource == nil || b.distributedSource.authority == nil {
		return nil, os.ErrNotExist
	}
	provider, volatile, err := b.distributedSource.authority.SnapshotProvider(uint64(snapshot))
	if err != nil {
		return nil, err
	}
	if !volatile {
		if b.snapshotShared == nil {
			return nil, errors.New("shared snapshot repository is unavailable")
		}
		manifest, err := b.snapshotShared.Manifest(snapshot)
		if err != nil {
			return nil, err
		}
		objects := make([]ttcsnapshot.Object, len(manifest.Objects))
		for index := range manifest.Objects {
			objects[index] = manifest.Objects[index].Object
		}
		return objects, nil
	}
	if provider.NodeID == b.nodeID {
		return nil, fmt.Errorf("local node owns snapshot %d but has no sealed graph metadata", snapshot)
	}
	peer, err := b.distributedSource.peer(provider.NodeID, false)
	if err != nil {
		return nil, err
	}
	reader, length, err := peer.FetchSnapshot(ctx, uint64(snapshot), transaction)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return decodeGraphSnapshotObjects(reader, length, snapshot)
}
