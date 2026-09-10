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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

const graphReferenceIntentVersion = 1

type graphReferenceIntent struct {
	Version     int                   `json:"version"`
	Transaction string                `json:"transaction"`
	Operation   string                `json:"operation"`
	Container   string                `json:"container"`
	Branch      string                `json:"branch"`
	State       string                `json:"state"`
	Groups      []graphReferenceGroup `json:"groups"`
	CreatedAt   time.Time             `json:"created_at"`
}

type graphReferenceGroup struct {
	Origin   uint64                        `json:"origin"`
	Provider string                        `json:"provider,omitempty"`
	Roots    []cluster.SnapshotRootBinding `json:"roots"`
	Retained bool                          `json:"retained,omitempty"`
	Released bool                          `json:"released,omitempty"`
}

func graphReferenceIntentPath(localRoot, operation string) (string, error) {
	if localRoot == "" || operation == "" || filepath.Base(operation) != operation || strings.ContainsAny(operation, `/\\`) {
		return "", errors.New("invalid graph reference intent identity")
	}
	return filepath.Join(localRoot, "graph-references", operation+".json"), nil
}

func readGraphReferenceIntent(path string) (graphReferenceIntent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return graphReferenceIntent{}, err
	}
	var intent graphReferenceIntent
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&intent); err != nil {
		return graphReferenceIntent{}, err
	}
	if intent.Version != graphReferenceIntentVersion || intent.Transaction == "" || intent.Operation == "" ||
		intent.Container == "" || intent.Branch == "" || len(intent.Groups) == 0 ||
		(intent.State != "retaining" && intent.State != "active" && intent.State != "releasing") {
		return graphReferenceIntent{}, errors.New("invalid graph reference intent")
	}
	return intent, nil
}

func (b *runtimeBackend) newGraphReferenceIntent(transaction, operation, container, branch string, objects []string) (graphReferenceIntent, string, error) {
	path, err := graphReferenceIntentPath(b.activeDataRoot(), operation)
	if err != nil {
		return graphReferenceIntent{}, "", err
	}
	groups, err := groupGraphRootBindings(objects)
	if err != nil {
		return graphReferenceIntent{}, "", err
	}
	origins := make([]uint64, 0, len(groups))
	for origin := range groups {
		origins = append(origins, uint64(origin))
	}
	sort.Slice(origins, func(i, j int) bool { return origins[i] < origins[j] })
	intent := graphReferenceIntent{
		Version: graphReferenceIntentVersion, Transaction: transaction, Operation: operation,
		Container: container, Branch: branch, State: "retaining", CreatedAt: time.Now().UTC(),
	}
	for _, encodedOrigin := range origins {
		origin := ttcsnapshot.SnapshotID(encodedOrigin)
		provider, volatile, err := b.distributedSource.authority.SnapshotProvider(uint64(origin))
		if err != nil {
			return graphReferenceIntent{}, "", err
		}
		group := graphReferenceGroup{Origin: uint64(origin), Roots: groups[origin]}
		if volatile {
			group.Provider = provider.NodeID
		}
		intent.Groups = append(intent.Groups, group)
	}
	if current, err := readGraphReferenceIntent(path); err == nil {
		// Exact retries resume at the first uncommitted provider boundary.
		if !graphReferenceIntentEqualIdentity(current, intent) {
			return graphReferenceIntent{}, "", errors.New("graph reference operation is bound to different roots")
		}
		// Provider is a captured routing fence rather than caller identity. Keep
		// the durable value if authority concurrently changed node -> shared.
		return current, path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return graphReferenceIntent{}, "", err
	}
	if err := writeDurableJSON(path, &intent); err != nil {
		return graphReferenceIntent{}, "", err
	}
	return intent, path, nil
}

func graphReferenceIntentEqualIdentity(a, b graphReferenceIntent) bool {
	if a.Version != b.Version || a.Transaction != b.Transaction || a.Operation != b.Operation ||
		a.Container != b.Container || a.Branch != b.Branch || len(a.Groups) != len(b.Groups) {
		return false
	}
	for i := range a.Groups {
		x, y := a.Groups[i], b.Groups[i]
		if x.Origin != y.Origin || len(x.Roots) != len(y.Roots) {
			return false
		}
		for j := range x.Roots {
			if x.Roots[j] != y.Roots[j] {
				return false
			}
		}
	}
	return true
}

// retainDistributedGraphReferences durably records the complete intent before
// contacting any provider. Each successful provider boundary is then fenced
// locally. A crash may repeat one RPC, which is safe because provider root
// names are operation-stable and RetainRoots is idempotent.
func (b *runtimeBackend) retainDistributedGraphReferences(transaction, operation, container, branch string, objects []string) error {
	if b == nil || b.distributedSource == nil || b.distributedSource.authority == nil || b.snapshotGraph == nil {
		return errors.New("distributed graph reference authority is unavailable")
	}
	intent, path, err := b.newGraphReferenceIntent(transaction, operation, container, branch, objects)
	if err != nil {
		return err
	}
	return b.resumeGraphReferenceRetain(path, &intent)
}

func (b *runtimeBackend) resumeGraphReferenceRetain(path string, intent *graphReferenceIntent) error {
	if intent == nil || intent.State == "releasing" {
		return errors.New("graph reference operation is already releasing")
	}
	return advanceGraphReferenceRetain(path, intent, func(group graphReferenceGroup) error {
		return b.mutateGraphReferenceGroup(context.Background(), *intent, group, true)
	})
}

func advanceGraphReferenceRetain(path string, intent *graphReferenceIntent, mutate func(graphReferenceGroup) error) error {
	if intent == nil || mutate == nil || intent.State == "releasing" {
		return errors.New("invalid retaining graph reference intent")
	}
	for index := range intent.Groups {
		group := &intent.Groups[index]
		if group.Retained {
			continue
		}
		if err := mutate(*group); err != nil {
			return err
		}
		group.Retained = true
		if err := writeDurableJSON(path, intent); err != nil {
			return err
		}
	}
	intent.State = "active"
	return writeDurableJSON(path, intent)
}

func (b *runtimeBackend) releaseDistributedGraphReferences(operation string) error {
	if operation == "" || b == nil || b.distributedSource == nil || b.distributedSource.authority == nil || b.snapshotGraph == nil {
		return nil
	}
	path, err := graphReferenceIntentPath(b.activeDataRoot(), operation)
	if err != nil {
		return err
	}
	intent, err := readGraphReferenceIntent(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	intent.State = "releasing"
	if err := writeDurableJSON(path, &intent); err != nil {
		return err
	}
	if err := advanceGraphReferenceRelease(path, &intent, func(group graphReferenceGroup) error {
		return b.mutateGraphReferenceGroup(context.Background(), intent, group, false)
	}); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

// reconcileDistributedGraphReferences closes every crash prefix before the
// node may resume execution. Retains are completed first; terminal or missing
// branches are released. Release failures remain durable and retryable, while
// an incomplete retain is fatal because executing without it could let an
// origin collect pages still needed by this node.
func (b *runtimeBackend) reconcileDistributedGraphReferences(strictRetain bool) error {
	if b == nil || b.distributedSource == nil {
		return nil
	}
	dir := filepath.Join(b.activeDataRoot(), "graph-references")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var retryable error
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		intent, err := readGraphReferenceIntent(path)
		if err != nil {
			if strictRetain {
				return fmt.Errorf("reading graph reference intent %q: %w", entry.Name(), err)
			}
			retryable = errors.Join(retryable, fmt.Errorf("reading graph reference intent %q: %w", entry.Name(), err))
			continue
		}
		tx, txErr := b.distributedSource.transactions.Load(intent.Transaction)
		terminal := txErr == nil && (tx.State == cluster.TransactionCanceled || tx.State == cluster.TransactionFailed)
		branchPresent := false
		if info, loadErr := b.containerStore().LoadContainer(intent.Container); loadErr == nil {
			for _, branch := range info.Branches {
				if branch.ID == intent.Branch && transactionIDForBranch(info, branch) == intent.Transaction && graphReferenceOperationForBranch(info, branch) == intent.Operation {
					branchPresent = true
					break
				}
			}
		}
		if intent.State == "releasing" || terminal || !branchPresent {
			if err := b.releaseDistributedGraphReferences(intent.Operation); err != nil {
				retryable = errors.Join(retryable, fmt.Errorf("resuming graph reference release %q: %w", intent.Operation, err))
			}
			continue
		}
		if txErr != nil {
			failure := fmt.Errorf("loading graph reference transaction %q: %w", intent.Transaction, txErr)
			if strictRetain && intent.State == "retaining" {
				return failure
			}
			retryable = errors.Join(retryable, failure)
			continue
		}
		if intent.State == "retaining" {
			if err := b.resumeGraphReferenceRetain(path, &intent); err != nil {
				failure := fmt.Errorf("resuming graph reference retain %q: %w", intent.Operation, err)
				if strictRetain {
					return failure
				}
				retryable = errors.Join(retryable, failure)
			}
		}
	}
	return retryable
}

func advanceGraphReferenceRelease(path string, intent *graphReferenceIntent, mutate func(graphReferenceGroup) error) error {
	if intent == nil || mutate == nil || intent.State != "releasing" {
		return errors.New("invalid releasing graph reference intent")
	}
	for index := range intent.Groups {
		group := &intent.Groups[index]
		if !group.Retained || group.Released {
			continue
		}
		if err := mutate(*group); err != nil {
			return err
		}
		group.Released = true
		if err := writeDurableJSON(path, intent); err != nil {
			return err
		}
	}
	return nil
}

func (b *runtimeBackend) mutateGraphReferenceGroup(ctx context.Context, intent graphReferenceIntent, group graphReferenceGroup, retain bool) error {
	current, volatile, err := b.distributedSource.authority.SnapshotProvider(group.Origin)
	if err != nil {
		return err
	}
	// Once an origin is fully published, shared storage—not its former node—
	// owns lifetime. Durable branch manifests retain shared roots; no mutable
	// provider root is needed for this operation.
	if !volatile {
		return nil
	}
	if group.Provider == "" || current.NodeID != group.Provider {
		return fmt.Errorf("snapshot origin %d provider changed from %q to %q without shared publication", group.Origin, group.Provider, current.NodeID)
	}
	if group.Provider == b.nodeID {
		roots, err := graphReferenceRoots(ttcsnapshot.SnapshotID(group.Origin), intent.Operation, group.Roots)
		if err != nil {
			return err
		}
		if retain {
			return b.snapshotGraph.RetainRoots(roots)
		}
		return b.snapshotGraph.ReleaseRetainedRoots(roots)
	}
	peer, err := b.distributedSource.peer(group.Provider, false)
	if err != nil {
		return err
	}
	if retain {
		return peer.RetainSnapshotRoots(ctx, group.Origin, intent.Transaction, intent.Operation, group.Roots)
	}
	return peer.ReleaseSnapshotRoots(ctx, group.Origin, intent.Transaction, intent.Operation, group.Roots)
}

func newGraphReferenceOperation() (string, error) {
	id, err := newDaemonRequestID()
	if err != nil {
		return "", err
	}
	return "branch-" + id, nil
}

func graphReferenceOperationForBranch(info ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) string {
	if branch.DistributedGraphReferenceOperation != "" {
		return branch.DistributedGraphReferenceOperation
	}
	// Distributed targets created before branch-scoped descendants use the
	// transaction as their stable operation identity. New descendants always
	// receive an explicit independent identity.
	return transactionIDForBranch(info, branch)
}
