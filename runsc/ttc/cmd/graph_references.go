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
	"time"

	"gvisor.dev/gvisor/pkg/ttcsnapshot"
	"gvisor.dev/gvisor/pkg/ttcvolume"
	ttcbundle "gvisor.dev/gvisor/runsc/ttc/bundle"
	"gvisor.dev/gvisor/runsc/ttc/cluster"
)

const graphReferenceIntentVersion = 2

const (
	graphReferencePurposeBranch   = "branch"
	graphReferencePurposeTransfer = "transfer"
	// graphReferencePurposeProvider is a node's own claim on an origin it
	// handed off, which keeps what its graph still references of the origin;
	// see recordProviderReferences.
	graphReferencePurposeProvider = "provider"
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
	// Repository names, for a provider claim, the filesystem repository whose
	// graph the claim keeps references of; empty, the node's memory graph.
	Repository string `json:"repository,omitempty"`
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
	valid := intent.Version == graphReferenceIntentVersion && intent.Transaction != "" && intent.Operation != "" &&
		len(intent.Groups) != 0 && (intent.State == "retaining" || intent.State == "active" || intent.State == "releasing")
	switch intent.Purpose {
	case graphReferencePurposeBranch, graphReferencePurposeTransfer:
		valid = valid && intent.Container != "" && intent.Branch != "" && intent.Repository == ""
	case graphReferencePurposeProvider:
		// The node, not a branch, owns a provider claim.
		valid = valid && intent.Container == "" && intent.Branch == "" && len(intent.Groups) == 1
	default:
		valid = false
	}
	if !valid {
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
			return graphReferenceIntent{}, "", fmt.Errorf("resolving operation %q origin %d before retain: %w", operation, origin, err)
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
		// the durable value if authority concurrently changed node -> shared,
		// and the branch name recorded before any rename likewise.
		return current, path, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return graphReferenceIntent{}, "", err
	}
	if err := writeDurableJSON(path, &intent); err != nil {
		return graphReferenceIntent{}, "", err
	}
	return intent, path, nil
}

// graphReferenceIntentEqualIdentity compares the identities of two intents.
// The branch's name is not part of one: a rename changes it, as
// containerHasGraphReferenceIntent allows, and a retry after the rename
// resumes the intent recorded under the old name.
func graphReferenceIntentEqualIdentity(a, b graphReferenceIntent) bool {
	if a.Version != b.Version || a.Transaction != b.Transaction || a.Operation != b.Operation ||
		a.Purpose != b.Purpose || a.Container != b.Container || len(a.Groups) != len(b.Groups) {
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

// graphHandoffReferenceOperation separates a target's initial inherited graph
// from a later sparse-handoff graph. Both may legitimately exist during one
// transaction, and each immutable snapshot must have a distinct idempotency
// identity until the branch record atomically switches between them.
func graphHandoffReferenceOperation(transaction string, snapshot uint64) string {
	return fmt.Sprintf("handoff-%s-%016x", transaction, snapshot)
}

// graphHandoffReferenceIntent reports whether intent is the one a graph
// handoff of its transaction retains, as graphHandoffReferenceOperation names
// it.
func graphHandoffReferenceIntent(intent graphReferenceIntent) bool {
	return intent.Purpose == graphReferencePurposeBranch && strings.HasPrefix(intent.Operation, "handoff-"+intent.Transaction+"-")
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
			if err := b.mutateGraphReferenceGroup(ctx, *intent, group, true); err != nil {
				return fmt.Errorf("retaining operation %q origin %d from provider %q (%d roots): %w", intent.Operation, group.Origin, group.Provider, len(group.Roots), err)
			}
			return nil
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
			if err := b.mutateGraphReferenceGroup(ctx, intent, group, false); err != nil {
				return fmt.Errorf("releasing operation %q origin %d from provider %q (%d roots): %w", intent.Operation, group.Origin, group.Provider, len(group.Roots), err)
			}
			return nil
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

// retryGraphReferenceMutation hides bounded reference-authority transitions:
// coordinator turnover and the provider-to-shared handoff freeze. Each
// attempt remains serialized with local inventory capture, but waiting occurs
// outside claimMu: otherwise a node elected as the successor could deadlock
// rebuilding its own inventory.
func (b *runtimeBackend) retryGraphReferenceMutation(run func(context.Context) error) error {
	return retryGraphReferenceMutation(run)
}

func retryGraphReferenceMutation(run func(context.Context) error) error {
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
		"references are frozen for authority handoff",
		"objects are frozen for authority handoff",
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
		if err := b.reconcileProviderClaims(); err != nil {
			lastErr = errors.Join(lastErr, err)
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
		if intent.Purpose == graphReferencePurposeProvider {
			// See reconcileProviderClaims.
			continue
		}
		tx, txErr := b.distributedSource.transactions.Load(intent.Transaction)
		branchPresent := false
		if info, loadErr := b.containerStore().LoadContainer(intent.Container); loadErr == nil {
			branchPresent = containerHasGraphReferenceIntent(info, intent)
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

// containerHasGraphReferenceIntent matches the immutable branch ownership
// identity, not its user-visible name. A rename deliberately changes only the
// latter, and the old name may immediately be reused by a different branch.
// Treating intent.Branch as identity made the reconciler release a live
// branch's roots after rename and allowed GC to retire memory still needed by
// that branch.
func containerHasGraphReferenceIntent(info ttcbundle.ContainerInfo, intent graphReferenceIntent) bool {
	for _, branch := range info.Branches {
		if transactionIDForBranch(info, branch) == intent.Transaction && graphReferenceOperationForBranch(info, branch) == intent.Operation {
			return true
		}
	}
	return false
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
	if txErr != nil {
		// A catalog or quorum failure does not prove that the authorizing
		// transaction disappeared. Only an authoritative not-found result and a
		// missing branch make this intent unreachable.
		return errors.Is(txErr, os.ErrNotExist) && !branchPresent
	}
	if tx == nil {
		return !branchPresent
	}
	if tx.State == cluster.TransactionCanceled || tx.State == cluster.TransactionFailed {
		return true
	}
	if branchPresent {
		return false
	}
	// Prepare retains roots before it records their ownership in the hidden
	// target branch. The transaction remains target-restoring throughout that
	// crash-safe handoff. Releasing a temporarily ownerless intent in this
	// window lets GC retire live roots before UpdateDistributedForkMemoryGraph
	// can publish them. Once Prepare returns, TargetPaused (and every later
	// state) proves that the branch record should already own the intent, so a
	// missing record is a genuine interrupted removal.
	//
	// A graph handoff's intent is owned the same way, later: the handoff
	// retains the roots it hands the target while the transaction is
	// published, and records them in the branch before the transaction drains.
	// Released in between, the branch went on paging from an origin GC then
	// retired.
	switch tx.State {
	case cluster.TransactionPreparing,
		cluster.TransactionTargetReserved,
		cluster.TransactionSourceCaptured,
		cluster.TransactionTargetRestoring:
		return false
	case cluster.TransactionPublished:
		return !graphHandoffReferenceIntent(intent)
	default:
		return true
	}
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
	transaction := graphReferenceAuthorizationTransaction(intent)
	current, volatile, err := b.distributedSource.authority.SnapshotProvider(group.Origin)
	if err != nil {
		if !retain && errors.Is(err, os.ErrNotExist) {
			state, stateErr := b.distributedSource.authority.AuthorityStateOf(cluster.AuthorityQuery{
				RetiredOrigins: cluster.Only(group.Origin), AbandonedOrigins: cluster.Only(group.Origin),
			})
			if stateErr != nil {
				return stateErr
			}
			if _, retired := state.RetiredOrigins[group.Origin]; retired {
				// Reference-ledger release precedes marking the local intent
				// complete. A crash after the release may allow the zero-count
				// origin to retire first; retirement is an irreversible,
				// generation-fenced acknowledgement of that release.
				return nil
			}
			if state.AbandonedOrigins[group.Origin] {
				// An unpublished origin can have no surviving reference. A
				// release retry after its abandonment is therefore complete.
				return nil
			}
		}
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
			return peer.RetainSnapshotRoots(ctx, group.Origin, transaction, intent.Operation, group.Roots)
		}
		return peer.ReleaseSnapshotRoots(ctx, group.Origin, transaction, intent.Operation, group.Roots)
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
		return peer.RetainSnapshotRoots(ctx, group.Origin, transaction, intent.Operation, group.Roots)
	}
	return peer.ReleaseSnapshotRoots(ctx, group.Origin, transaction, intent.Operation, group.Roots)
}

// graphReferenceAuthorizationTransaction distinguishes the transaction-owned
// reference installed while importing a remote branch from descendant branch
// references created later. A local fork inherits immutable roots and may
// outlive or move away from the original transaction's target node; requiring
// that old transaction to authorize every descendant makes behavior depend on
// which node currently holds the disposable shared-reference coordinator.
// Descendant operations have fresh, unguessable operation identities and can
// mutate only their exact retained root names, so cluster membership and the
// immutable object bindings are their authority. Transfer intents remain
// transaction-fenced even though their operation carries a prefix. A provider
// claim has no transaction; its unguessable operation is its authority too.
func graphReferenceAuthorizationTransaction(intent graphReferenceIntent) string {
	if intent.Purpose == graphReferencePurposeProvider {
		return ""
	}
	if intent.Purpose == graphReferencePurposeBranch && intent.Operation != intent.Transaction && !graphHandoffReferenceIntent(intent) {
		return ""
	}
	return intent.Transaction
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
	authority, err := b.distributedSource.authority.AuthorityStateOf(cluster.AuthorityQuery{
		Branches: cluster.All[string](), Publications: cluster.All[string](),
	})
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
	releasableOrigins := make(map[ttcsnapshot.SnapshotID]struct{})
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
		_, volatile, terminal, err := snapshotOriginAuthority(b.distributedSource.authority, id.Snapshot)
		if terminal {
			// Raft tombstones are permanent. Shared GC has either already
			// consumed this reference, or the unpublished origin never had a
			// shared-ledger reference. This is the idempotent completion case
			// when removing a branch marked corrupted after provider loss.
			continue
		}
		if err != nil {
			return err
		}
		if volatile {
			continue
		}
		// An abandoned provider origin and a committed shared origin both lack a
		// volatile route. Only the latter has an immutable shared manifest and a
		// corresponding shared-ledger baseline. Removal may be retrying after it
		// already reclaimed the former, so do not invent a shared release.
		if b.snapshotShared != nil {
			if err := b.snapshotShared.CheckManifest(id.Snapshot); errors.Is(err, os.ErrNotExist) {
				continue
			} else if err != nil {
				return err
			}
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
		// The bridge belongs to the shared pack, not merely to the logical
		// snapshot ID. Remember it only after proving that the pack exists and
		// releasing one of its canonical roots succeeded. Volatile and already
		// retired/abandoned origins have no shared bridge to release. Recording
		// them here used to turn an otherwise idempotent branch removal into a
		// missing-manifest failure.
		releasableOrigins[id.Snapshot] = struct{}{}
	}
	publisher := &branchPublisher{
		sharedRoot: b.dataRoot, nodeID: b.nodeID, incarnation: b.distributedSource.incarnation,
		authority: b.distributedSource.authority, graphShared: b.snapshotShared,
		sharedRefs: b.sharedReferences, peers: b.distributedSource,
	}
	for origin := range releasableOrigins {
		if err := publisher.releaseSharedPublicationBridge(origin); err != nil {
			return fmt.Errorf("releasing snapshot origin %d publication bridge: %w", origin, err)
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

// providerOwnReference reports whether a reference that a graph holds of an
// origin it handed off is the graph's own: a claim that a consumer holds here
// is the consumer's to keep, and the shared ledger counts a bridge edge.
func providerOwnReference(name string) bool {
	return !strings.HasPrefix(name, "root/remote/") && !strings.HasPrefix(name, "root/bridge/")
}

// providerReferencesNeeded reports whether a graph still references an origin
// it handed off, given what OriginReferences returned of it.
func providerReferencesNeeded(references map[string]ttcsnapshot.ObjectID, edges bool) bool {
	if edges {
		return true
	}
	for name := range references {
		if providerOwnReference(name) {
			return true
		}
	}
	return false
}

// providerReferencePrefix begins the operation of every provider claim on
// origin for the graph of repository, or the memory graph's when it is empty.
func providerReferencePrefix(origin ttcsnapshot.SnapshotID, repository string) string {
	graph := "memory"
	if repository != "" {
		digest := sha256.Sum256([]byte(repository))
		graph = fmt.Sprintf("%x", digest[:8])
	}
	return fmt.Sprintf("provider-%016x-%s-", uint64(origin), graph)
}

// recordProviderReferences records this node's claim on origin, which it is
// handing off, for what its graph still references of the origin: the memory
// graph's, or the filesystem repository's at repository. references are the
// graph's references of the origin, which the handoff has frozen.
//
// The handoff moves the counting of the origin's references to the shared
// ledger, which counts the origin's published branches by their roots in Raft
// and other nodes' branches by their claims. This node's other live branches
// it would count by nothing: the ledger would collect a fork base they still
// share, and their next publication, whose objects have edges into it, would
// fail to import. Whatever the graph references of the origin later, it
// reaches from these roots and pins, so the claim keeps them until the graph
// references nothing of the origin; see reconcileProviderReferences.
// retainProviderReferences retains the claim before the handoff commits.
//
// It returns the claim's path, or "" when the graph has no roots or pins of
// the origin. A retried handoff finds the claim it recorded.
func (p *branchPublisher) recordProviderReferences(origin ttcsnapshot.SnapshotID, repository string, references map[string]ttcsnapshot.ObjectID) (string, error) {
	if p == nil || p.sharedRefs == nil || p.sharedRefs.localRoot == "" || origin == 0 {
		return "", errors.New("provider reference claims are unavailable")
	}
	objects := make(map[string]struct{})
	for name, object := range references {
		if providerOwnReference(name) && object.Snapshot == origin {
			objects[object.String()] = struct{}{}
		}
	}
	if len(objects) == 0 {
		return "", nil
	}
	sorted := make([]string, 0, len(objects))
	for object := range objects {
		sorted = append(sorted, object)
	}
	sort.Strings(sorted)
	var groups []graphReferenceGroup
	for slot, object := range sorted {
		// A group is one retain request, which carries at most 4096 roots.
		if slot%4096 == 0 {
			groups = append(groups, graphReferenceGroup{Origin: uint64(origin), Provider: p.nodeID})
		}
		group := &groups[len(groups)-1]
		group.Roots = append(group.Roots, cluster.SnapshotRootBinding{Slot: uint32(slot), Object: object})
	}
	prefix := providerReferencePrefix(origin, repository)
	directory := filepath.Join(p.sharedRefs.localRoot, "graph-references")
	entries, err := os.ReadDir(directory)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), prefix) || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		existing, err := readGraphReferenceIntent(path)
		if err != nil {
			return "", err
		}
		if existing.State != "releasing" && existing.Repository == repository && sameGraphReferenceRoots(existing.Groups, groups) {
			return path, nil
		}
	}
	id, err := newDaemonRequestID()
	if err != nil {
		return "", err
	}
	operation := prefix + id
	path, err := graphReferenceIntentPath(p.sharedRefs.localRoot, operation)
	if err != nil {
		return "", err
	}
	intent := graphReferenceIntent{
		Version: graphReferenceIntentVersion, Transaction: operation, Operation: operation,
		Purpose: graphReferencePurposeProvider, State: "retaining", Groups: groups,
		CreatedAt: time.Now().UTC(), Repository: repository,
	}
	if err := writeDurableJSON(path, &intent); err != nil {
		return "", err
	}
	return path, nil
}

// keepProviderReferences records and retains this node's claim on origin,
// which it is handing off, for what a graph still references of it: the
// filesystem repository's at repository, or the memory graph's when it is
// empty. references are the graph's references of origin, which the handoff
// has frozen. It runs after the shared ledger imported origin and before the
// handoff commits.
//
// Other nodes' claims on origin, which they retained here while this node
// provided it, move to the shared ledger with it: they release them there
// once it is shared.
func (p *branchPublisher) keepProviderReferences(origin ttcsnapshot.SnapshotID, repository string, references map[string]ttcsnapshot.ObjectID) error {
	path, err := p.recordProviderReferences(origin, repository, references)
	if err != nil {
		return fmt.Errorf("recording this node's claim on snapshot origin %d: %w", origin, err)
	}
	if err := p.retainProviderReferences(path); err != nil {
		return fmt.Errorf("retaining this node's claim on snapshot origin %d: %w", origin, err)
	}
	consumers := make(map[string]ttcsnapshot.ObjectID)
	for name, object := range references {
		if root := strings.TrimPrefix(name, "root/"); root != name && consumerClaimRoot(root, origin) {
			consumers[root] = object
		}
	}
	if len(consumers) == 0 {
		return nil
	}
	if err := retryGraphReferenceMutation(func(ctx context.Context) error {
		return p.retainAtSharedLedger(ctx, origin, consumers)
	}); err != nil {
		return fmt.Errorf("moving other nodes' claims on snapshot origin %d to the shared ledger: %w", origin, err)
	}
	return nil
}

// consumerClaimRoot reports whether root, a root of a provider's graph, is
// another node's claim on origin, as graphReferenceRoots names one.
func consumerClaimRoot(root string, origin ttcsnapshot.SnapshotID) bool {
	parts := strings.Split(root, "/")
	if len(parts) != 4 || parts[0] != "remote" || parts[1] != fmt.Sprint(uint64(origin)) || parts[2] == "" || len(parts[3]) != 8 {
		return false
	}
	_, err := strconv.ParseUint(parts[3], 16, 32)
	return err == nil
}

// sameGraphReferenceRoots reports whether two claims' groups bind the same
// roots of the same origins.
func sameGraphReferenceRoots(a, b []graphReferenceGroup) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Origin != b[i].Origin || len(a[i].Roots) != len(b[i].Roots) {
			return false
		}
		for j := range a[i].Roots {
			if a[i].Roots[j] != b[i].Roots[j] {
				return false
			}
		}
	}
	return true
}

// retainProviderReferences retains, at the shared ledger, the claim at path
// that recordProviderReferences recorded, if any. The ledger has imported the
// origin with this node's references to it, which the coordinator releases
// once the handoff commits: so this must precede the commit, while this node
// still provides the origin, which authorizes it.
func (p *branchPublisher) retainProviderReferences(path string) error {
	if path == "" {
		return nil
	}
	intent, err := readGraphReferenceIntent(path)
	if err != nil {
		return err
	}
	if intent.State == "active" {
		return nil
	}
	return retryGraphReferenceMutation(func(ctx context.Context) error {
		// As resumeGraphReferenceRetain does, keep each attempt and its local
		// transition on one side of an inventory observation.
		p.sharedRefs.claimMu.Lock()
		defer p.sharedRefs.claimMu.Unlock()
		return advanceGraphReferenceRetain(path, &intent, func(group graphReferenceGroup) error {
			return p.retainProviderGroup(ctx, intent.Operation, group)
		})
	})
}

func (p *branchPublisher) retainProviderGroup(ctx context.Context, operation string, group graphReferenceGroup) error {
	origin := ttcsnapshot.SnapshotID(group.Origin)
	roots, err := graphReferenceRoots(origin, operation, group.Roots)
	if err != nil {
		return err
	}
	return p.retainAtSharedLedger(ctx, origin, roots)
}

// retainAtSharedLedger retains roots of origin at the shared ledger, which has
// imported the origin, while this node still provides the origin: the
// coordinator takes them as it takes a publication bridge from the provider.
func (p *branchPublisher) retainAtSharedLedger(ctx context.Context, origin ttcsnapshot.SnapshotID, roots map[string]ttcsnapshot.ObjectID) error {
	if p.authority == nil {
		return errors.New("shared reference authority is unavailable")
	}
	lease, err := p.authority.SharedGCLease()
	if err != nil {
		return err
	}
	if lease.Generation == 0 || !time.Now().Before(lease.ExpiresAt) {
		return errors.New("shared reference coordinator has no active lease")
	}
	names := make([]string, 0, len(roots))
	for name := range roots {
		names = append(names, name)
	}
	sort.Strings(names)
	if lease.NodeID == p.nodeID && lease.Incarnation == p.incarnation {
		return p.sharedRefs.withLedger(func(ledger *ttcsnapshot.ReferenceLedger) error {
			return applySharedRootMutation(ledger, names, roots, "retain", true)
		})
	}
	if p.peers == nil {
		return errors.New("cluster peer routing is unavailable")
	}
	peer, err := p.peers.peer(lease.NodeID, false)
	if err != nil {
		return err
	}
	for _, name := range names {
		object := roots[name]
		digest := sha256.Sum256([]byte(name + "\x00" + object.String()))
		if err := peer.ApplySharedSnapshotReference(ctx, uint64(origin), fmt.Sprintf("provider-retain-%x", digest[:12]), name, object.String(), true); err != nil {
			return err
		}
	}
	return nil
}

// reconcileProviderClaims reconciles every provider claim of this node; see
// reconcileProviderReferences. The handoff that records a claim retains it
// before it commits, so execution waits for none, and it needs no lock of the
// node's: releasing a claim waits for the shared-reference coordinator,
// which a node's start may be what brings back, and during a failover for a
// while.
func (b *runtimeBackend) reconcileProviderClaims() error {
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
	var result error
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), "provider-") || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		intent, err := readGraphReferenceIntent(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err == nil && intent.Purpose == graphReferencePurposeProvider {
			err = b.reconcileProviderReferences(path, intent)
		}
		if err != nil {
			result = errors.Join(result, fmt.Errorf("reconciling provider claim %q: %w", entry.Name(), err))
		}
	}
	return result
}

// providerCheck is when a provider claim's graph is to be checked next, and
// the wait after that check if it finds the claim still needed.
type providerCheck struct {
	next     time.Time
	interval time.Duration
	// busy counts the checks in a row that found the claim's repository in
	// use by another handle, and so did not read it.
	busy int
}

// providerCheckBusyLimit is the checks in a row that may find a claim's
// repository in use before one reads it anyway.
const providerCheckBusyLimit = 5

// reconcileProviderReferences releases the provider claim at path once its
// graph references nothing of the claim's origin, checking a claim that is
// still needed ever less often, up to every two minutes: checking a
// repository's reads its whole graph. It forgets a claim whose handoff was
// canceled.
func (b *runtimeBackend) reconcileProviderReferences(path string, intent graphReferenceIntent) error {
	if intent.State == "releasing" {
		return b.releaseDistributedGraphReferences(intent.Operation)
	}
	origin := ttcsnapshot.SnapshotID(intent.Groups[0].Origin)
	_, volatile, terminal, err := snapshotOriginAuthority(b.distributedSource.authority, origin)
	if err != nil {
		return err
	}
	if terminal {
		b.distributedSource.providerChecks.Delete(intent.Operation)
		return b.releaseDistributedGraphReferences(intent.Operation)
	}
	if volatile {
		if _, _, frozen, err := b.providerGraphReferences(intent.Repository, origin, true); err != nil || frozen {
			if errors.Is(err, ttcvolume.ErrRepositoryBusy) {
				// Checked again, at the next reconciliation.
				return nil
			}
			return err
		}
		// The handoff was canceled before it committed, and the origin's
		// references are this node's alone again. What the claim retained at
		// the shared ledger, which no inventory reports any more, the
		// coordinator releases once the origin is shared.
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if intent.State == "retaining" {
		return b.resumeGraphReferenceRetain(path, &intent)
	}
	now := time.Now()
	check := providerCheck{}
	if value, ok := b.distributedSource.providerChecks.Load(intent.Operation); ok {
		check = value.(providerCheck)
		if now.Before(check.next) {
			return nil
		}
	}
	// A repository another handle has open is read once it is idle, or once
	// it has been found in use too often.
	references, edges, _, err := b.providerGraphReferences(intent.Repository, origin, check.busy < providerCheckBusyLimit)
	if errors.Is(err, ttcvolume.ErrRepositoryBusy) {
		check.busy++
		check.interval = min(max(2*check.interval, 5*time.Second), 2*time.Minute)
		check.next = now.Add(check.interval)
		b.distributedSource.providerChecks.Store(intent.Operation, check)
		return nil
	}
	if err != nil {
		return err
	}
	check.busy = 0
	if providerReferencesNeeded(references, edges) {
		check.interval = min(max(2*check.interval, 5*time.Second), 2*time.Minute)
		check.next = now.Add(check.interval)
		b.distributedSource.providerChecks.Store(intent.Operation, check)
		return nil
	}
	b.distributedSource.providerChecks.Delete(intent.Operation)
	return b.releaseDistributedGraphReferences(intent.Operation)
}

// providerGraphReferences returns what the graph of the repository at
// repository, or the memory graph when it is empty, still references of
// origin, as OriginReferences returns it, and whether origin's handoff is
// frozen there. A repository that is gone references nothing. With ifIdle,
// a repository another handle has open is not read: it reports
// ttcvolume.ErrRepositoryBusy.
func (b *runtimeBackend) providerGraphReferences(repository string, origin ttcsnapshot.SnapshotID, ifIdle bool) (map[string]ttcsnapshot.ObjectID, bool, bool, error) {
	if repository == "" {
		if b.snapshotGraph == nil {
			return nil, false, false, nil
		}
		references, edges := b.snapshotGraph.OriginReferences(origin)
		return references, edges, b.snapshotGraph.SnapshotLedgerFrozen(origin), nil
	}
	read := ttcvolume.ReadOriginReferences
	if ifIdle {
		read = ttcvolume.ReadOriginReferencesIfIdle
	}
	references, edges, frozen, err := read(repository, origin)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, false, nil
	}
	return references, edges, frozen, err
}
