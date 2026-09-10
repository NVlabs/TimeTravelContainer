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
	"context"
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

func (p *graphObjectProvider) OpenObject(_ context.Context, peer, objectText, transactionID string) (io.ReadCloser, uint64, error) {
	if p == nil || p.graph == nil || p.data == nil || p.authority == nil {
		return nil, 0, errors.New("snapshot graph provider is unavailable")
	}
	id, err := ttcsnapshot.ParseObjectID(objectText)
	if err != nil {
		return nil, 0, err
	}
	provider, found, err := p.authority.SnapshotProvider(uint64(id.Snapshot))
	if err != nil {
		return nil, 0, err
	}
	if !found || provider.NodeID != p.nodeID {
		return nil, 0, fmt.Errorf("snapshot origin %d is not owned by node %q", id.Snapshot, p.nodeID)
	}
	node, err := p.authority.Lookup(p.nodeID)
	if err != nil {
		return nil, 0, err
	}
	if node.Incarnation != p.incarnation || !node.LeaseActive(time.Now()) ||
		(node.State != cluster.NodeReady && node.State != cluster.NodePressured && node.State != cluster.NodeDraining) {
		return nil, 0, errors.New("snapshot provider incarnation is not currently authoritative")
	}
	if transactionID != "" {
		if p.transactions == nil {
			return nil, 0, errors.New("snapshot transaction catalog is unavailable")
		}
		transaction, err := p.transactions.Load(transactionID)
		if err != nil {
			return nil, 0, err
		}
		if transaction.Identity.SourceNodeID != p.nodeID || transaction.Identity.TargetNodeID != peer {
			return nil, 0, errors.New("snapshot transaction does not authorize this provider/requester pair")
		}
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
