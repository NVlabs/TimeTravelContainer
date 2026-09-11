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
	"strings"
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

const graphReferenceIntentVersion = 2

const (
	graphReferencePurposeBranch   = "branch"
	graphReferencePurposeTransfer = "transfer"
)

type graphReferenceIntent struct {
	Version     int                   `json:"version"`
	Transaction string                `json:"transaction"`
	Operation   string                `json:"operation"`
	Purpose     string                `json:"purpose"`
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
	// Version 1 predates transaction-owned migration transfer roots. Its only
	// possible meaning is a branch-owned reference, so upgrade that record in
	// memory without crossing the data-root format boundary.
	if intent.Version == 1 && intent.Purpose == "" {
		intent.Version = graphReferenceIntentVersion
		intent.Purpose = graphReferencePurposeBranch
	}
	if intent.Version != graphReferenceIntentVersion || intent.Transaction == "" || intent.Operation == "" ||
		intent.Container == "" || intent.Branch == "" || len(intent.Groups) == 0 ||
		(intent.Purpose != graphReferencePurposeBranch && intent.Purpose != graphReferencePurposeTransfer) ||
		(intent.State != "retaining" && intent.State != "active" && intent.State != "releasing") {
		return graphReferenceIntent{}, errors.New("invalid graph reference intent")
	}
	return intent, nil
}

func (b *runtimeBackend) newGraphReferenceIntent(transaction, operation, container, branch string, objects []string) (graphReferenceIntent, string, error) {
	return b.newGraphReferenceIntentWithPurpose(transaction, operation, container, branch, graphReferencePurposeBranch, objects)
}

func (b *runtimeBackend) newGraphReferenceIntentWithPurpose(transaction, operation, container, branch, purpose string, objects []string) (graphReferenceIntent, string, error) {
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
		Purpose: purpose, Container: container, Branch: branch, State: "retaining", CreatedAt: time.Now().UTC(),
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
		a.Purpose != b.Purpose || a.Container != b.Container || a.Branch != b.Branch || len(a.Groups) != len(b.Groups) {
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

// retainDistributedGraphTransfer holds the source branch's current immutable
// roots across a migration cutover. The visible branch record is replaced by
// the target before asynchronous graph handoff completes, so ordinary
// branch-owned reference reconciliation cannot safely cover this interval.
func (b *runtimeBackend) retainDistributedGraphTransfer(transaction, container, branch string, objects []string) error {
	if len(objects) == 0 {
		return nil
	}
	operation := graphTransferReferenceOperation(transaction)
	intent, path, err := b.newGraphReferenceIntentWithPurpose(transaction, operation, container, branch, graphReferencePurposeTransfer, objects)
	if err != nil {
		return err
	}
	return b.resumeGraphReferenceRetain(path, &intent)
}

func graphTransferReferenceOperation(transaction string) string {
	return "transfer-" + transaction
}

func (b *runtimeBackend) resumeGraphReferenceRetain(path string, intent *graphReferenceIntent) error {
	if intent == nil || intent.State == "releasing" {
		return errors.New("graph reference operation is already releasing")
	}
	return b.retryGraphReferenceMutation(func(ctx context.Context) error {
		if b.sharedReferences != nil {
			// Coordinator turnover rebuilds from these durable intent states.
			// Keep one provider attempt and its matching local transition on one
			// side of an inventory observation, but release the lock between
			// retries so this node may itself rebuild the next coordinator ledger.
			b.sharedReferences.claimMu.Lock()
			defer b.sharedReferences.claimMu.Unlock()
		}
		return advanceGraphReferenceRetain(path, intent, func(group graphReferenceGroup) error {
			return b.mutateGraphReferenceGroup(ctx, *intent, group, true)
		})
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
	if err := b.retryGraphReferenceMutation(func(ctx context.Context) error {
		if b.sharedReferences != nil {
			b.sharedReferences.claimMu.Lock()
			defer b.sharedReferences.claimMu.Unlock()
		}
		return advanceGraphReferenceRelease(path, &intent, func(group graphReferenceGroup) error {
			return b.mutateGraphReferenceGroup(ctx, intent, group, false)
		})
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

// retryGraphReferenceMutation hides the bounded unavailability window of the
// disposable shared-reference coordinator. Each attempt remains serialized
// with local inventory capture, but waiting occurs outside claimMu: otherwise
// a node elected as the successor could deadlock rebuilding its own inventory.
func (b *runtimeBackend) retryGraphReferenceMutation(run func(context.Context) error) error {
	if run == nil {
		return errors.New("invalid graph-reference mutation")
	}
	deadline := time.Now().Add(45 * time.Second)
	var lastErr error
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		err := run(ctx)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		if !isSharedReferenceCoordinatorTransition(err) || !time.Now().Before(deadline) {
			return lastErr
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// releaseDistributedGraphReferencesEventually preserves lifecycle
// availability across the short interval between one shared-reference
// coordinator releasing its lease and another rebuilding the ledger. The
// release intent is already durable and continues to reserve every root, so
// deferring only this coordination failure can leak temporarily but cannot
// reclaim live data. Other failures still stop the caller.
func (b *runtimeBackend) releaseDistributedGraphReferencesEventually(operation string) error {
	err := b.releaseDistributedGraphReferences(operation)
	if err == nil || !isSharedReferenceCoordinatorTransition(err) {
		return err
	}
	if b.stderr != nil {
		fmt.Fprintf(b.stderr, "ttc: deferred graph-reference release %q until shared coordinator takeover: %v\n", operation, err)
	}
	return nil
}

func isSharedReferenceCoordinatorTransition(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"shared reference coordinator has no active lease",
		"shared reference coordinator is unavailable",
		"does not hold an active shared reference lease",
		"shared reference lease is stale",
		"shared reference lease changed",
		"connection refused",
		"connection reset by peer",
		"no route to host",
		"lease has expired",
		"context deadline exceeded",
		"client.timeout exceeded",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func (b *runtimeBackend) pendingGraphReferenceReleases() (int, error) {
	dir := filepath.Join(b.activeDataRoot(), "graph-references")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	pending := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		intent, err := readGraphReferenceIntent(filepath.Join(dir, entry.Name()))
		if err != nil {
			return 0, err
		}
		if intent.State == "releasing" {
			pending++
		}
	}
	return pending, nil
}

// flushGraphReferenceReleases is a retirement fence. Unlike an ordinary
// remove, node retirement cannot leave retryable intents behind on scratch
// that is about to become disposable.
func (b *runtimeBackend) flushGraphReferenceReleases(timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := b.reconcileDistributedGraphReferences(false); err != nil {
			lastErr = err
		}
		pending, err := b.pendingGraphReferenceReleases()
		if err != nil {
			return err
		}
		if pending == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%d graph-reference releases remain pending: %w", pending, lastErr)
		}
		time.Sleep(100 * time.Millisecond)
	}
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
		branchPresent := false
		if info, loadErr := b.containerStore().LoadContainer(intent.Container); loadErr == nil {
			for _, branch := range info.Branches {
				if branch.ID == intent.Branch && transactionIDForBranch(info, branch) == intent.Transaction && graphReferenceOperationForBranch(info, branch) == intent.Operation {
					branchPresent = true
					break
				}
			}
		}
		if graphReferenceIntentShouldRelease(intent, tx, txErr, branchPresent) {
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

func graphReferenceIntentShouldRelease(intent graphReferenceIntent, tx *cluster.Transaction, txErr error, branchPresent bool) bool {
	if intent.State == "releasing" {
		return true
	}
	if intent.Purpose == graphReferencePurposeTransfer {
		// A transfer intent is transaction-owned, not branch-owned. Keep it
		// while the target can still depend on the source even though Raft
		// cutover has already replaced the visible branch metadata. A failed
		// post-cutover migration deliberately retains its source for recovery.
		return txErr == nil && tx != nil && (tx.State == cluster.TransactionIndependent || tx.State == cluster.TransactionCanceled)
	}
	terminal := txErr == nil && tx != nil && (tx.State == cluster.TransactionCanceled || tx.State == cluster.TransactionFailed)
	return terminal || !branchPresent
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
	if !volatile {
		lease, err := b.distributedSource.authority.SharedGCLease()
		if err != nil {
			return err
		}
		if lease.Generation == 0 || !time.Now().Before(lease.ExpiresAt) {
			return errors.New("shared reference coordinator has no active lease")
		}
		if lease.NodeID == b.nodeID && lease.Incarnation == b.distributedSource.incarnation {
			return b.sharedReferences.mutateRoots(ttcsnapshot.SnapshotID(group.Origin), intent.Operation, group.Roots, retain)
		}
		peer, err := b.distributedSource.peer(lease.NodeID, false)
		if err != nil {
			return err
		}
		if retain {
			return peer.RetainSnapshotRoots(ctx, group.Origin, intent.Transaction, intent.Operation, group.Roots)
		}
		return peer.ReleaseSnapshotRoots(ctx, group.Origin, intent.Transaction, intent.Operation, group.Roots)
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

// releaseSharedGraphMemoryRoots releases the canonical roots captured for the
// branch that originally produced a shared snapshot. Remote descendants use
// graphReferenceIntent instead; the producer branch has no such transaction
// and must address its baseline root names directly.
func (b *runtimeBackend) releaseSharedGraphMemoryRoots(roots, objects []string) error {
	if b == nil || b.distributedSource == nil || b.distributedSource.authority == nil || len(roots) == 0 {
		return nil
	}
	if len(roots) != len(objects) {
		return errors.New("shared memory root names and objects differ in length")
	}
	authority, err := b.distributedSource.authority.AuthorityState()
	if err != nil {
		return err
	}
	protected := make(map[uint64]bool)
	for _, branch := range authority.Branches {
		if !branch.Corrupted && branch.Roots.MemorySnapshot != 0 {
			protected[branch.Roots.MemorySnapshot] = true
		}
	}
	for _, publication := range authority.Publications {
		if (publication.State == cluster.PublicationPreparing || publication.State == cluster.PublicationReady) && publication.CandidateRoots.MemorySnapshot != 0 {
			protected[publication.CandidateRoots.MemorySnapshot] = true
		}
	}
	for index, name := range roots {
		id, err := ttcsnapshot.ParseObjectID(objects[index])
		if err != nil {
			return err
		}
		// Raft branch/publication roots are the canonical reachability source.
		// Never let a local cleanup race release one before the control-plane
		// transition that replaces or removes it. A skipped release is reclaimed
		// by the next coordinator rebuild; premature release could let pack
		// compaction discard still-authoritative bytes.
		if protected[uint64(id.Snapshot)] {
			continue
		}
		_, volatile, err := b.distributedSource.authority.SnapshotProvider(uint64(id.Snapshot))
		if err != nil {
			return err
		}
		if volatile {
			continue
		}
		lease, err := b.distributedSource.authority.SharedGCLease()
		if err != nil {
			return err
		}
		if lease.Generation == 0 || !time.Now().Before(lease.ExpiresAt) {
			return errors.New("shared reference coordinator has no active lease")
		}
		canonical := "root/" + name
		digest := sha256.Sum256([]byte(canonical + "\x00" + id.String()))
		operation := fmt.Sprintf("root-release-%x", digest[:12])
		if lease.NodeID == b.nodeID && lease.Incarnation == b.distributedSource.incarnation {
			err = b.sharedReferences.mutateReference(operation, canonical, id, false)
		} else {
			peer, peerErr := b.distributedSource.peer(lease.NodeID, false)
			if peerErr != nil {
				return peerErr
			}
			err = peer.ApplySnapshotReference(context.Background(), uint64(id.Snapshot), operation, canonical, id.String(), false)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func newGraphReferenceOperation() (string, error) {
	id, err := newDaemonRequestID()
	if err != nil {
		return "", err
	}
	return "branch-" + id, nil
}

func graphReferenceOperationForBranch(_ ttcbundle.ContainerInfo, branch ttcbundle.BranchInfo) string {
	return branch.DistributedGraphReferenceOperation
}
