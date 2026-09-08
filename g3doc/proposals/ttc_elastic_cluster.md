# Elastic TimeTravelContainer clusters

Status: the cluster authority has moved to one persistent cluster-wide Raft
group. Identity-preserving migration uses live sparse memory transfer, and
placement uses a cluster-global hypergraph with event-local movable vertices
and fixed node terminals. Stressed physical-host and failure-injection
qualification remains in progress.

This document specifies an elastic TTC cluster in which compute nodes may join
and leave without restarting existing daemons. A client connected to any
`ttcd` sees one container and branch namespace; branch placement is an
implementation detail except in diagnostics. The scheduler keeps node memory
near configured targets, requests capacity through an operator-provided event
channel, and migrates branches without changing their identity or lineage.

The design builds on the implemented distributed-fork and sparse-dehydration
protocols described in [Distributed live fork](ttc_distributed_live_fork.md)
and the authority and failure rules in
[Cluster-wide Raft control plane](ttc_raft_control_plane.md).
It does not introduce distributed shared writable memory: branches share page
versions while colocated and obtain independent resident copies after a
cross-node cut.

## Scope and assumptions

Elastic cluster mode assumes an OpenMPI-style trusted private network between
`ttcd` nodes, optionally protected by one shared `TTC_CLUSTER_TOKEN`, one
shared `TTC_ROOT`, a common architecture and base-page size, exact daemon build
compatibility, and reliable host networking. Every node also configures
a distinct, node-local `TTC_LOCAL_ROOT`. Active filesystems, runtime state,
mutable metadata, staging data, and caches stay local; immutable image content,
committed cold manifests and packs live in the shared repository. The Raft WAL,
snapshots, active catalog view, and all hot journals are node-local; the shared
filesystem is not the coordination database. The daemon socket is selected independently with `TTC_SOCKET` (or
the normal per-user runtime default).

Unexpected node loss is distinct from graceful elasticity. A clean drain is
lossless. A branch that still depends exclusively on volatile pages from a
failed source is recoverable only if those pages were already made durable or
replicated.

## Cluster invariants

1. **One runnable owner:** one placement generation owns a branch. A stale
   generation must not execute guest instructions or mutate its filesystem.
2. **Identity-preserving migration:** migration keeps the family ID, visible
   branch name, filesystem identity, creation time, parent display metadata,
   labels, and exec-session identities.
3. **Paused cutover:** a destination is not runnable until an atomic ownership
   compare-and-swap succeeds. The source is no longer runnable afterward.
4. **Exclusive filesystem writer:** an ownership lease fences rootfs and
   managed-volume writes independently of memory-page dependencies.
5. **Publish before route:** clients are routed to a destination only after its
   runtime is complete and paused at the cutover point.
6. **Dependency before departure:** a draining node may exit only after every
   remote target is source-independent and every durable publication is done.
7. **Idempotent operations:** join, heartbeat, reserve, cutover, drain,
   capacity request, retry, cancellation, and cleanup use stable operation IDs.
8. **Compressed metadata:** placement planning must not require a permanent
   object per 4 KiB page. Resident page-version sequences are interval
   compressed in control responses and swept into exact weighted edges only
   for the current event window.

## Identities and records

Each daemon publishes a lease-bearing node record containing its node ID,
start incarnation, state (`joining`, `ready`, `pressured`, `draining`, or
`offline`), advertised control endpoint, capabilities, memory target, actual
and reserved memory, labels, and lease timestamps. `Incarnation` changes on
every daemon start and prevents an old daemon from renewing or deleting a
newer process's lease.

One cluster-wide Raft state machine owns node records, branch placement,
capacity reservations, fork/migration transactions, publication generations,
corruption decisions, and committed memory/filesystem root IDs. There is no
per-family consensus group and no shared-filesystem locking authority. The
first three admitted members vote; later elastic workers are learners unless a
membership rebalance promotes one. Bulk pages, chunks, extents, and packs never
enter the Raft log.

A branch placement record identifies the family, branch, owner node and
incarnation, monotonically increasing ownership generation, lifecycle state,
filesystem lease, and active operation ID. Dehydrated and exited branches are
unplaced. Rehydration chooses a compatible node and acquires a new placement
generation. Running and paused branches have exactly one owner.

Interrupted migration recovery uses the transaction and placement from one
Raft revision. Before cutover, the source placement remains authoritative and
the hidden target cannot execute. Cutover validates the published paused target
and changes both the transaction's ownership marker and placement generation
in one log entry. Once ownership has moved, it is never rolled back: only the
target can activate and roll forward. Shared container metadata is a
materialized data-plane cache, not an ownership fact.

```text
joining -> ready -> pressured -> ready
                 \-> draining -> offline
joining -----------------------> offline
```

A draining node refuses new runs, forks, rehydrations, and incoming
migrations. A node that cannot renew uses a conservative monotonic-local
deadline, closes ingress, durably records the fence, and sends `SIGSTOP` to
each Sentry before its lease may expire. Restart adopts placements through
linearizable Raft CASes before sending `SIGCONT`; a failed stop falls back to
`SIGKILL`. Thus logical lease loss is paired with a host-kernel execution fence.

## Unified client view and routing

The shared data root already exposes persistent metadata to every node. Phase
1 includes remote-owned families in `ttc ls/ps --json` and adds `node_id` to
the stable family record. Human output hides placement by default; `inspect`
and explicit diagnostics expose it.

For mutating and streaming commands, the receiving daemon resolves branch
placement and either executes locally or proxies to the owner. Proxy requests
carry the original request ID, exact owner incarnation and placement generation,
terminal metadata, and framed stdin/stdout. The owner revalidates every claim
after taking its local lifecycle lock. A command observed during migration, or
one delayed past cutover, is rejected for retry instead of reaching a stale
Sentry. Named exec sessions and retained output allow reconnection after
migration.

Family-wide `stop`, `dehydrate`, and `rm` are coordinator operations. The entry
daemon expands them into idempotent owner-local mutations before taking a
local lifecycle lock. Removal tears down non-final branches and residual
physical runtime shells before the final owner deletes shared family metadata.

## Identity-preserving migration

Migration reuses portable Kernel capture, sparse page loading, distributed
transaction recovery, and target-local networking, but it is not a fork:

```text
running or paused on A
    -> quiescing on A
    -> prepared-paused on B
    -> ownership CAS A/g -> B/g+1
    -> source-fenced, target-running (or target-paused)
    -> dependency-draining
    -> independent
```

The destination first imports a temporary runtime identity. At cutover, the
catalog transfers the visible branch and filesystem writer lease together.
Target preparation and transaction publication leave the imported Kernel
physically paused and absent from ordinary branch metadata. Only after the
placement CAS succeeds does a separate idempotent activation publish the
target owner, reconnect console and exec sidecars, recreate published ports,
and resume a branch whose requested lifecycle was running. Dependency draining
cannot advance before this activation marker.
The source retains only immutable snapshot/page-server ownership needed by B;
it may not resume or write the branch. Once B uses only shared chunks or local
pages, the source snapshot is released.

External TCP connections are reset on the destination, matching distributed
fork policy. Cluster-transparent published ports require a separate ingress
router. The first migration phase rejects branches with published ports unless
the operator explicitly accepts interruption and rebinding.

## Memory pressure and placement

Node pressure uses actual process or cgroup RSS as the authoritative signal.
Per-branch TTC statistics explain which move is useful but do not include all
Sentry heap and unmapped-cache memory. Nodes report both actual use and
branch/family accounting. Targets use configurable high and low watermarks,
incoming reservations, cooldowns, and hysteresis to prevent oscillation.

Branches are vertices in a sharing hypergraph. A shared 4 KiB page version is
a hyperedge connecting every referencing branch; cutting it predicts
duplicated residency. Sentries export stable page-version runs rather than one
record per page. The scheduler interval-sweeps runs from every live node and
coalesces intervals with identical membership into weighted hyperedges.

Each scheduling event creates a reduced global problem. Branches on the
pressured or retiring node are movable. Every branch elsewhere remains fixed
and is collapsed to one terminal per incident node. Fixed-only edges are
dropped. This preserves the global CoW objective without creating variables
for hundreds of unrelated branches. The measured cgroup load on each node is
the baseline; moving a vertex applies exact marginal private-byte and edge
occupancy deltas.

The native Go solver first repairs the hard pressure or evacuation constraint,
considering both contracted vertices and bounded whole-hyperedge groups. It
then runs deterministic FM-style refinement. Its objective combines
connectivity bytes and private migration bytes; target capacities and incoming
reservations remain hard constraints. Incremental occupancy and gain state
means a proposed move touches only incident edges. One solve can select
several destinations and TTC reserves all of them before the first migration.

An optional shared policy at
`$TTC_ROOT/cluster/placement-policy.json` adds exact-selector allowed-node
sets, hard co-location groups, and weighted soft affinities. Policy branches
outside the movable window become fixed node terminals. The current schema is:

```json
{
  "version": 1,
  "allowed_nodes": {"agent@latency-sensitive": ["node-a", "node-b"]},
  "hard_co_location": [
    {"id": "service-pair", "branches": ["app@api", "app@database"]}
  ],
  "soft_affinity": [
    {
      "id": "reuse-warm-state",
      "branches": ["agent@candidate", "agent@warm-peer"],
      "nodes": ["node-c"],
      "weight_bytes": 268435456
    }
  ]
}
```

The document is strict JSON, dynamically reloaded for every placement event,
and limited to 1 MiB. A hard group already split across multiple fixed nodes
is diagnosed rather than silently weakened.

Pressure policy may migrate, dehydrate an inactive branch, request capacity,
or temporarily reject fan-out. TTH supplies policy weights and may mark
latency-sensitive branches as non-dehydratable.

## Capacity events

Provisioning remains outside TTC. TTC publishes a durable JSON request in
`$TTC_ROOT/cluster/capacity-requests/<source-node>.json` when pressure or
a drain has no feasible destination. An external provisioner watches this
shared outbox and removes the file only after durably accepting it. Manual and
automatic retries coalesce while the file exists, and removal permits a new
request ID. A request looks like:

```json
{
  "version": 1,
  "request_id": "capacity-...",
  "operation": "acquire",
  "nodes": 1,
  "memory_bytes_each": 68719476736,
  "architecture": "amd64",
  "repository_id": "cluster-a",
  "source_node": "node-a",
  "reason": "memory-pressure",
  "deadline": "2026-08-21T12:00:00Z"
}
```

Atomic publication makes the outbox safe to scan after either side restarts.
Receivers should still deduplicate `request_id`. Native executable and HTTP
webhook adapters are implemented; a separate provider-specific capacity
release adapter remains future work.

## Graceful drain

1. Atomically mark the node draining and reject incoming work.
2. Build one fixed-terminal evacuation window for all active source branches.
3. Reserve every selected destination and migrate running and paused
   partitions; then transfer zero-resident cold branch ownership.
4. Drain volatile page dependencies to shared chunks.
5. Transfer filesystem and network leases and publish any remaining sealed
   filesystem generations.
6. Verify no branch, transaction, chunk lease, port listener, or session proxy
   remains owned by the node.
7. Mark offline, stop `ttcd`, and emit a capacity release event.

When capacity is insufficient, drain requests nodes before pausing a source. A
deadline may choose dehydration over live migration but may never silently
discard a branch.

## Implementation phases

### Phase 0: specification and model

Record invariants, formats, operation state machines, and failure rules. Add
deterministic ownership/migration models covering stale owners, retry,
cancellation, and node loss.

Exit: bounded model and fuzz schedules never produce two runnable owners or an
unreferenced live filesystem writer.

### Phase 1: dynamic discovery and global view

**Implemented.**

* Add lease-bearing node records and renewals in the cluster-wide Raft group.
* Add advertised control endpoints and dynamic peer client construction.
* Retain static peers as explicit overrides during transition.
* Include remote-owned persistent families in list and inspect JSON.
  Cataloged branch owners come from one locked catalog snapshot, so a
  half-published target cannot leak into the unified view.
* Report membership, compatibility, lease state, and actual memory usage.
* Route new container admission to the compatible ready node with the lowest
  normalized current-plus-reserved memory load. **Implemented.** The selected
  node is pinned in the proxied request so concurrent heartbeat changes cannot
  create a routing loop.

Exit: nodes can join after existing daemons start; every node sees the same
families; stale and duplicate node incarnations are detected.

### Phase 2: routed commands and manual migration

**Implemented with live sparse cutover.** `ttc migrate` pauses the selected
branch once, restores a paused target with the same visible identity, and lets
the target demand-page from the source immediately after publication. A
background 1 MiB sparse-chunk handoff removes the live-source dependency before
the source runtime is reclaimed. Siblings remain live on their existing nodes.
Daemons reconcile interrupted source- and target-owned transactions after lease
expiry, including the boundaries immediately before and after ownership CAS.

* Add daemon-to-daemon command proxying with placement-generation fencing.
* Implement `ttc migrate --node NODE CONTAINER@BRANCH` without changing
  branch identity or parent.
* Transfer the filesystem writer lease atomically and reconnect named exec
  sessions after cutover.
* Recover and retry migration transactions after daemon loss. **Implemented.**

Exit: repeated migration between two nodes preserves process and filesystem
state, never runs both copies, and survives retries at every boundary.

### Phase 3: draining and capacity integration

**Implemented with a durable capacity-provider interface.** `ttc node ls` and
`ttc node stop --migrate-all` provide dynamic membership diagnostics, admission fencing,
active and cold branch evacuation, restart-safe retry, and dependency draining.
A durable per-node intent is written before cordoning. A restarted daemon
rejoins directly in `draining`, reconciles surviving Sentries, and resumes the
remaining ownership scan automatically. A completed node stop marks the daemon
offline, retires its Raft membership, and exits automatically. Stop and
rebalance publish a coalesced durable capacity request when
there is no feasible target. A daemon can deliver that request to either an
executable or an HTTP webhook. Attempts, failures, acceptance, retry timing,
and a stable idempotency key survive restart. TTC, rather than the provider,
removes the request after compatible usable capacity appears in membership.

* Add `ttc node stop`, admission control, dependency draining, and offline
  verification.
* Add executable and webhook adapters over the durable capacity outbox.
  **Implemented.**
* Resume incomplete drains after daemon restart. **Implemented.**

Exit: a node with mixed running, paused, and dehydrated real workloads can
leave without branch loss or repository garbage.

### Phase 4: placement planner

**Implemented as a reduced global fixed-terminal planner.** `ttc node
rebalance`, `ttc node stop --migrate-all`, and the opt-in automatic-rebalance loop use
cgroup memory as the hard signal and stable resident page-version identity for
move selection. Only source branches are variables; all other live nodes are
queried and represented as fixed terminals even when they are not legal
destinations. Cgroup memory remains the hard admission signal.

* Export bounded resident-sharing summaries and node actual/reserved memory.
* Implement deterministic multi-destination repair and FM refinement.
* Add watermarks, hysteresis, cooldowns, and destination reservations.
  **Implemented.** Reservations are incarnation-fenced, atomically reject
  concurrent overcommit, expire after source loss, and remain charged through
  migration completion and the target's next fresh heartbeat.
* Add optional automatic rebalance, capacity requests, and cluster-shared
  hard/soft affinity policy.

Exit: correlated sibling workloads use materially less cluster memory than
random placement and do not oscillate under a stable workload.

### Phase 5: qualification

**In progress.** See [Elastic cluster qualification](../benchmarks/ttc_elastic_qualification.md)
for completed local, migration, capacity-provider, real Rocky Linux package,
and NFS cases, plus the remaining physical-node/KVM coverage.

Run unit, race, fuzz, and crash injection; multi-daemon loopback and local NFS
tests under delay, loss, and restart; systrap and KVM package-build,
browser/REPL, tmpfs, managed-filesystem, exec-session, and remote-fork
workloads; join/leave storms; thousands of branches; forced scheduler exits;
NFS stalls; and incomplete capacity requests.

Record source pause, cutover downtime, time to execution and independence, cut
bytes, transferred bytes, remote-fault percentiles, NFS operations, peak extra
RSS, and cleanup residue. A phase is not complete until its failure tests and
cleanup audits pass.
