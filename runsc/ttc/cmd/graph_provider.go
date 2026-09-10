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
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

type graphObjectProvider struct {
	graph        *ttcsnapshot.Store
	data         *ttcsnapshot.LocalDataStore
	authority    *cluster.RaftCatalog
	transactions cluster.TransactionCatalog
	nodeID       string
	incarnation  string
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
	if err := p.authorizeSnapshot(peer, id.Snapshot, transactionID); err != nil {
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

func (p *graphObjectProvider) authorizeSnapshot(peer string, snapshot ttcsnapshot.SnapshotID, transactionID string) error {
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
			return err
		}
		if transaction.Identity.SourceNodeID != p.nodeID || transaction.Identity.TargetNodeID != peer {
			return errors.New("snapshot transaction does not authorize this provider/requester pair")
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
	if err := p.authorizeSnapshot(peer, snapshot, transactionID); err != nil {
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

var _ cluster.ImmutableObjectProvider = (*graphObjectProvider)(nil)
var _ cluster.ImmutableSnapshotProvider = (*graphObjectProvider)(nil)

func decodeGraphSnapshotCatalog(reader io.Reader, length uint64, snapshot ttcsnapshot.SnapshotID) (*ttcsnapshot.ImmutableCatalog, error) {
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
	return ttcsnapshot.NewImmutableCatalog(wire.Objects)
}

// immutableSnapshotCatalog resolves metadata by origin authority. Unlike the
// bulk resolver it never installs metadata into the local authoritative graph.
func (b *runtimeBackend) immutableSnapshotCatalog(ctx context.Context, snapshot ttcsnapshot.SnapshotID, transaction string) (*ttcsnapshot.ImmutableCatalog, error) {
	if b == nil || snapshot == 0 {
		return nil, errors.New("invalid immutable snapshot catalog request")
	}
	if b.snapshotGraph != nil {
		if record, ok := b.snapshotGraph.Snapshot(snapshot); ok && record.State != ttcsnapshot.SnapshotWorking {
			objects, err := b.snapshotGraph.SnapshotObjects(snapshot)
			if err != nil {
				return nil, err
			}
			return ttcsnapshot.NewImmutableCatalog(objects)
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
		return ttcsnapshot.NewImmutableCatalog(objects)
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
	return decodeGraphSnapshotCatalog(reader, length, snapshot)
}
