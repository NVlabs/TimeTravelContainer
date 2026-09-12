# TTC cluster-wide Raft control plane

> **Current data plane:** The
> [snapshot-graph storage refactor](ttc_snapshot_graph_refactor.md) preserves
> this single cluster-wide Raft group and is authoritative for snapshot IDs,
> immutable graph objects, publication, routing, and reference ownership.

## Status

This document defines the implemented authoritative control-plane state
machine for distributed TimeTravelContainer. It replaces shared-filesystem
JSON records and advisory locks as the authority for membership, placement,
distributed transactions, publication, and durable roots. Node-local and
shared materializations are data-plane records only; they cannot grant
execution or storage authority.

The cluster has exactly one Raft group. Families are keys in that state
machine, not independent consensus groups. Memory pages, filesystem objects,
chunks, and packs never enter the Raft log.

## Design rules

1. Ordinary guest memory and filesystem writes perform no Raft operation.
2. Raft grants authority at branch, placement-generation, and snapshot-root
   granularity.
3. Bulk data is immutable and structurally identified before Raft references
   its bounded logical roots.
4. A node may execute a branch only while it holds the committed placement
   generation and a valid node execution lease.
5. A root becomes durable only after all referenced objects are structurally
   complete and readable outside Raft.
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
configured seed over plaintext TCP on the trusted cluster network and is
redirected to the leader. An optional `TTC_CLUSTER_TOKEN` authenticates both
HTTP control requests and Raft streams; without it, reachability implies
cluster-administrator trust. Admission
checks repository identity, exact TTC build, architecture, base-page size, and
required protocol features.

Each daemon exposes one plaintext cluster TCP socket. Connections beginning
with TTC's reserved Raft preface are handed to the authenticated Raft stream
layer; every other connection is handed to the HTTP server with the inspected
bytes replayed. Consequently a node has one control address for admission,
commands, consensus, and replication. The preface is only protocol framing,
not an authentication mechanism; `TTC_CLUSTER_TOKEN`, when configured, is
validated independently by both HTTP and Raft.

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

The bootstrap node also creates one immutable cluster-policy record. Joining
nodes adopt its paging, lease, transaction-limit, and automatic-rebalance
settings instead of repeating them. Node IDs, advertised addresses, local
memory targets, and capacity-provider hooks remain per-node configuration.

Restarting a daemon creates a new random incarnation. The old incarnation's
commands and leases remain stale even when the human node ID is reused.

## Execution leases and fencing

Raft leadership is not itself permission for every node to keep running guest
code. Each daemon proposes its own renewal every one third of the lease period.
The committed node record contains the exact incarnation, leader term, lease
sequence, and deadline. A node may execute only while its last locally observed
committed grant and its more conservative monotonic-local deadline are valid.

After a renewal commits, but before the daemon extends its host guardian
deadline, it performs a linearizable placement read while holding the local
lifecycle lock. It removes every physical branch no longer assigned to its
incarnation. If selective removal fails, the daemon immediately host-fences all
of its Sentries and does not notify the guardian. Thus a renewal cannot extend
execution of a placement that the daemon has already observed as revoked.

Placement transitions that could otherwise create two executors do not rely on
heartbeat observation: migration pauses the source before its atomic ownership
CAS, branch removal tears down the owner before removing the placement, and
node expiry is proposed only after the old lease deadline. The periodic
projection is the recovery/backstop path for committed corruption and
interrupted cleanup.

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

A stopped Sentry may contain several branch Kernels. If restart finds a mixture
of authorized and revoked branches, TTC does not resume the process to run a
branch-level cleanup RPC. It marks the otherwise-authorized co-residents
corrupted and kills the entire Sentry. This is deliberately conservative: it
turns an availability loss into explicit state loss instead of briefly running
a revoked guest.

No other node may recover an expired owner's branch until Raft has committed
the expiry/eviction after the old execution deadline. This pairs logical lease
expiry with a physical execution fence.

## Remote fork

1. Commit a transaction and target capacity reservation.
2. Pause the source, pin the exact page versions, and seal the filesystem
   generation.
3. Commit `source-captured` with the snapshot identities and dependency
   summary; resume the source.
4. Restore a hidden target and keep every guest task paused.
5. Commit the target branch, its first placement generation, and transaction
   `published` in one Raft command.
6. If a running target was requested, resume it only after step 5 and commit
   the idempotent `activated` acknowledgement. A crash between these actions
   leaves an authorized but paused runtime that recovery may roll forward.
7. Publish immutable memory roots and materialize or durably hand off every
   incomplete filesystem object in the background.
8. After the target proves that no source-local-only page or object remains,
   commit `independent`, clear the volatile dependency edge, and release the
   source retention.

The fork snapshot linearizes while the source is paused. Branch authority
linearizes at step 5, and execution begins no earlier than step 6. Source writes
after step 2 create new versions and cannot alter the retained snapshot.

Filesystem object IDs are immutable content identities. A target repository
keeps a persistent marker for every object whose 1 MiB extents are not all
local. The marker, not the existence or apparent size of a sparse host file,
decides whether a network reader is required. A newer branch may replace a
stale reader for the same object ID because both readers are required to return
identical bytes. The marker is removed only after every extent is installed;
only then may the transaction become source-independent.

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

## Cold materialization

A durable, unplaced branch is claimed with one compare-and-swap:

```text
unplaced/gen N
    -> materializing(owner=node, incarnation=I, gen N+1)
```

Only that incarnation may construct the local filesystem and Sentry. The guest
does not execute while the placement is `materializing`. A successful restore
commits `running` or `paused`; a failed restore releases the placement back to
`unplaced`. Expiry of the materializer also returns the placement to
`unplaced`, rather than corrupting the branch, because its committed cold roots
remain authoritative and unchanged.

This claim serializes concurrent `start`, `rehydrate`, and remote placement
without putting range reads or page faults through Raft.

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

The intended distributed collector obtains a linearizable state snapshot at
revision R and traces all branch, transaction, publication, and reader-lease
roots in that snapshot. It may quarantine objects absent from the set, but
final deletion must wait for a later committed grace boundary and active-reader
expiry. Newly committed roots after R either refer only to newly uploaded
immutable objects or preserve their publication authorization as a trace root,
so the collector cannot delete an object in the upload-to-commit window.

The current state machine already generation-fences branch removal,
publication, and name reuse. Replicated reader leases and the final global
grace-boundary command are not implemented yet; current repository-local GC
must therefore remain conservative. They are data-plane completion work, not
execution-authority prerequisites.

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

Shared storage and Raft deliberately have different failure domains. If shared
storage fails while Raft still has quorum, a fully local running branch may
continue; cold materialization, publication, or a fault requiring a shared-only
extent waits or fails visibly. If Raft quorum is lost, no node can renew its
execution lease, so all execution is physically fenced even if shared data is
still readable.

## Why the protocol is safe

The safety argument assumes crash-stop, non-Byzantine daemons; durable Raft
WALs on a majority; authenticated node identities; a host kernel that honors
`SIGSTOP`/`SIGKILL`; collision-resistant content IDs and checksums; and the
documented durability and atomic-rename behavior from shared storage. Raft
does not protect against a malicious daemon, a compromised host, or Byzantine
storage.

The state machine and data plane maintain these invariants:

1. **Single placement authority.** A branch generation has at most one owner
   node and incarnation. Placement creation or transfer is one deterministic
   Raft transition guarded by the previous generation.
2. **Execution requires two grants.** Physical execution requires both the
   exact committed placement and an unexpired execution lease for that same
   node incarnation. Losing either grant causes host-level fencing.
3. **Authority precedes execution.** A newly restored remote target remains
   paused until its placement transition commits. Recovery may repeat
   activation, but cannot activate an unplaced target.
4. **Snapshot identity is immutable.** Fork pins memory versions and seals a
   filesystem generation while the source is paused. Later source writes use
   CoW versions and cannot mutate the child's snapshot.
5. **Dependency precedes source release.** A source is retained while the
   target references any source-local-only page or filesystem object. The edge
   is cleared only after persistent target markers prove all required bytes
   local or a durable immutable root proves them shared.
6. **Data precedes authority.** A durable root may be committed only after its
   immutable objects are uploaded and verified. A crash before the root commit
   leaves garbage, not a partially authoritative snapshot.
7. **Stale work cannot publish.** Every lifecycle, placement, and publication
   mutation carries the expected revision/generation and operation ID. A late
   worker from an old incarnation or deleted generation fails its compare-and-
   swap instead of resurrecting state.
8. **Unknown loss is explicit.** When TTC cannot prove that every required
   authoritative byte survives, it stops the affected dependency closure and
   marks it `corrupted`; it never silently resumes from an older root.

These invariants give the important failure proofs:

* During a fork, the source is the only executor before target placement
  commits. The target is paused at the commit point, and starts afterward, so
  there is no interval in which an unauthorized target executes.
* During migration, the paused source owns generation N before the ownership
  CAS; the paused target owns N+1 after it. A crash on either side of that one
  log entry has a unique rollback or roll-forward direction.
* During a partition, a minority cannot commit lease renewals. Its conservative
  local deadline expires no later than its committed authority and the host
  guardian stops the entire Sentry. A majority cannot reassign an expired
  placement before the old deadline, preventing overlapping executors.
* During retries, identical operation IDs return the recorded result and stale
  revisions are rejected. Crashes between placement commit and activation
  leave a paused authorized target, which is safe and recoverable.
* During source loss, the committed dependency graph identifies the exact
  transitive branches that still require source-local state. Independent
  branches remain runnable; incomplete branches are fenced and corrupted.

The protocol is not intended to make all observations linearizable. Statistics,
progress events, cache contents, shared-JSON compatibility records, and normal
guest reads/writes may lag or proceed locally. Placement, leases, lifecycle
authority, transaction phase, publication authorization, and committed roots
are the strongly ordered part.

## Cost and scalability

Normal guest instructions, memory faults, 4 KiB CoW, 1 MiB transfers, file
operations, and cache hits perform zero Raft proposals. A fork or migration
uses a small constant number of proposals independent of memory size and file
count. Lease renewals are coalesced per node. Raft snapshots bound replay and
WAL growth, so hundreds of branches enlarge state snapshots but do not put
their data traffic through consensus.

The remaining distributed performance bottleneck is not Raft: some
compatibility metadata and filesystem setup paths still issue many small
operations against the shared filesystem. Those paths must move to local
caches and immutable packs without weakening the authority rules above.

## Known safety and availability limits

* A majority outage stops all execution after lease expiry; this sacrifices
  availability to prevent split-brain.
* Raft cannot recover volatile bytes whose only authoritative copy disappeared
  with a failed source node. Such dependent branches become `corrupted`.
* Several branch Kernels may share one Sentry host process. If restart cannot
  selectively clean a revoked Kernel without running it, TTC kills the Sentry
  and conservatively corrupts otherwise-authorized co-residents.
* Replicated reader leases and the final globally fenced shared-pack collector
  remain incomplete, so current GC is intentionally conservative.

## Rollout rule

There is no mixed-authority cluster mode. A Raft-enabled root carries a new
control-plane format marker. Cluster startup rejects the previous shared-JSON
format. Compatibility adapters may preserve Go call sites while the refactor is
in progress, but their mutations must be Raft proposals and their reads must be
linearizable Raft reads.

Standalone TTC does not need a multi-node Raft group. It uses the same
deterministic state-machine implementation with a local single-node authority,
keeping lifecycle semantics shared without adding a network dependency.
