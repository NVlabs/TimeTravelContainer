# TTC cluster-wide Raft control plane

## Status

This document defines the authoritative control-plane state machine for
distributed TimeTravelContainer. It replaces shared-filesystem JSON records and
advisory locks as the authority for membership, placement, distributed
transactions, publication, durable roots, and garbage-collection roots.

The cluster has exactly one Raft group. Families are keys in that state
machine, not independent consensus groups. Memory pages, filesystem objects,
chunks, and packs never enter the Raft log.

## Design rules

1. Ordinary guest memory and filesystem writes perform no Raft operation.
2. Raft grants authority at branch and storage-generation granularity.
3. Bulk data is immutable and content-addressed before Raft references it.
4. A node may execute a branch only while it holds the committed placement
   generation and a valid node execution lease.
5. A root becomes durable only after all referenced objects are readable and
   verified outside Raft.
6. Removing or superseding a generation revokes its publication authorization;
   a late worker cannot make it authoritative again.
7. A state-machine command is deterministic, bounded, generation-fenced, and
   idempotent under a caller-supplied operation ID.

## Why one group

Capacity reservations, node retirement, placement, and migration cross family
boundaries. A group per family would require another consensus group to make
those operations atomic and would make a cluster-wide view a collection of
potentially inconsistent reads. One group provides one revision and one total
order for all control decisions while keeping the data path out of consensus.

Hundreds or thousands of branches are not expected to overload the group: a
running branch does not append log entries for page faults, CoW writes, file
operations, or statistics samples. Entries occur for lifecycle, topology,
ownership, publication, and periodically coalesced membership changes.

## Replicated state

```text
ClusterState
  revision
  configuration epoch
  nodes[node ID]
    incarnation, state, endpoints, capabilities
    execution lease sequence/deadline
    memory target/current/reservations
  families[family ID]
    family generation, image identity
    branches[branch ID]
      lifecycle and lineage hint
      placement owner/incarnation/generation
      storage generation/status
      authoritative memory/filesystem roots
      volatile dependency summary
  transactions[operation ID]
    immutable fork/migration identity
    phase and revision
    source/target placement generations
    session digest and non-secret handoff root
  publications[family, branch, storage generation]
    authorization, capture fence, candidate roots, status
  gc epochs
    committed root-set revision and tombstone grace boundary
```

The state machine records dependency summaries and durable roots. It does not
record one item per page or file. Detailed manifests are immutable objects in
the bulk store and are reached through the committed root IDs.

## Commands

Every command contains a unique request ID. Commands that mutate an existing
object also contain its expected generation or revision. Reapplying the same
request returns the recorded result; using the request ID with different input
is rejected.

The initial command families are:

* `node-register`, `node-renew`, `node-transition`, `node-remove`;
* `capacity-reserve`, `capacity-complete`, `capacity-release`;
* `branch-create`, `branch-update`, `branch-remove`;
* `placement-begin-migration`, `placement-commit-migration`,
  `placement-abort-migration`, `placement-claim-unplaced`;
* `transaction-create`, `transaction-bind-session`, `transaction-advance`,
  `transaction-bind-handoff`, `transaction-activate`;
* `publication-begin`, `publication-capture-ready`,
  `publication-commit-roots`, `publication-fail`, `publication-cancel`;
* `branch-mark-corrupted` and `branch-remove-corrupted`;
* Raft membership changes for learner admission, voter promotion, demotion, and
  removal.

Time-based expiry is evaluated by the leader, which proposes an explicit
logical timestamp/lease sequence in the command. Followers never call their
wall clocks while applying a command. Nodes use conservative local deadlines
to self-fence; a lease is authority only when both the replicated grant and the
local deadline remain valid.

## Node admission and membership

The first node is explicitly bootstrapped. A joining daemon contacts any
configured seed over mutual TLS and is redirected to the leader. Admission
checks repository identity, exact TTC build, architecture, base-page size, and
required protocol features.

The node is first added as a non-voting learner. It receives the log or a
snapshot before admission. The first three members are promoted to voters;
additional elastic compute nodes remain learners, so a large worker fleet does
not enlarge the quorum. A node is not eligible for branch placement while it
is merely joining. Graceful voter retirement promotes a caught-up learner
before removing the old voter when that is required to retain the replica
target.

The shared repository contains a small immutable cluster identity. A fresh
local WAL must either create that identity as the explicit first bootstrap or
join the group named by it. The same shared root cannot silently host two Raft
groups, and a local WAL from another cluster is rejected.

Restarting a daemon creates a new random incarnation. The old incarnation's
commands and leases remain stale even when the human node ID is reused.

## Execution leases and fencing

Raft leadership is not itself permission for every node to keep running guest
code. The leader periodically commits a coalesced lease grant containing a
sequence and deadline for admitted node incarnations. A node may execute only
while its last locally observed committed grant is valid.

Before its conservative monotonic-local deadline, a node that cannot renew
closes client and peer ingress, durably records each Sentry PID plus host start
time, and sends `SIGSTOP` to every Sentry. This host-kernel fence also stops an
in-flight fork or resume that a branch-by-branch RPC could miss. If a Sentry
cannot be stopped before authority expires, TTC sends `SIGKILL` rather than run
outside its lease.

After communication returns, the node performs a linearizable read:

* after adopting every retained placement into its new incarnation, it may
  `SIGCONT` only PIDs whose host start time still matches the fence record;
* if authority moved, it destroys the stale local runtime;
* if the node was removed, it stays fenced and drains or exits as instructed.

No other node may recover an expired owner's branch until Raft has committed
the expiry/eviction after the old execution deadline. This pairs logical lease
expiry with a physical execution fence.

## Remote fork

1. Commit a transaction and target capacity reservation.
2. Pause the source, pin the exact page versions, and seal the filesystem
   generation.
3. Commit `source-captured` with the snapshot identities and dependency
   summary; resume the source.
4. Restore a hidden, paused target using authenticated peer readers.
5. Commit the target branch and placement plus transaction `published` in one
   command, then make the runtime visible and resume it if requested.
6. Publish immutable memory/filesystem roots in the background and commit the
   handoff.
7. After the target proves that no source-local-only object remains, commit
   `independent` and release the source retention.

The fork snapshot linearizes while the source is paused. Branch publication
linearizes at step 5. Source writes after step 2 create new versions and cannot
alter the retained snapshot.

## Migration

Migration preserves a branch identity and therefore has one ownership
linearization point:

```text
begin migration fence
pause source
restore target hidden and paused
commit placement source/gen N -> target/gen N+1
activate target
drain old source
```

The placement transition and transaction phase are one Raft command. Before
that command the source is authoritative and rollback is permitted. After it,
only the target can roll forward. A target never executes the migrated branch
before observing the committed placement generation.

## Publication and durable roots

A publisher first uploads immutable objects and verifies them. It then proposes
`publication-commit-roots` with its authorization generation, memory root,
filesystem root, and object-set digest. The state machine checks that the
publication is still authorized and atomically replaces the branch roots.

Raft never claims that bytes are durable before upload. Upload without the
final command produces harmless unreachable objects. A committed root without
its objects is a protocol violation detected by verification and marks the
branch corrupted rather than silently falling back to an older generation.

Branch deletion or name reuse commits a newer generation and cancels all older
publication authorizations in the same state-machine transition.

## Garbage collection

GC obtains a linearizable state snapshot at revision R and traces all branch,
transaction, publication, and reader-lease roots in that snapshot. It may
quarantine objects absent from the set, but final deletion waits for a later
Raft grace epoch and active-reader expiry. Newly committed roots after R either
refer only to newly uploaded immutable objects or preserve their publication
authorization as a trace root, so the collector cannot delete an object in the
upload-to-commit window.

## Failure semantics

* Loss of Raft quorum freezes authority-changing operations. Nodes self-fence
  when execution leases can no longer be renewed.
* Loss of shared bulk storage does not change placements. Running local
  branches may continue while publication and cold reads wait, as long as Raft
  leases remain available.
* Permanent loss of a node marks only branches whose newest authoritative state
  has no surviving location as `corrupted`. A corrupted branch supports inspect
  and removal, not start, fork, migration, commit, or copy.
* A branch already independent of the lost node is unaffected.
* A target with a source-local-only dependency is paused before the source can
  be declared lost, then becomes corrupted if no durable or replicated copy can
  satisfy the dependency.

## Rollout rule

There is no mixed-authority cluster mode. A Raft-enabled root carries a new
control-plane format marker. Cluster startup rejects the previous shared-JSON
format. Compatibility adapters may preserve Go call sites while the refactor is
in progress, but their mutations must be Raft proposals and their reads must be
linearizable Raft reads.

Standalone TTC does not need a multi-node Raft group. It uses the same
deterministic state-machine implementation with a local single-node authority,
keeping lifecycle semantics shared without adding a network dependency.
