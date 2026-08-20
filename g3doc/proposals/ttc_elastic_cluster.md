# Elastic TimeTravelContainer clusters

Status: implementation plan. Phase 1 is the first delivery target.

This document specifies an elastic TTC cluster in which compute nodes may join
and leave without restarting existing daemons. A client connected to any
`ttcd` sees one container and branch namespace; branch placement is an
implementation detail except in diagnostics. The scheduler keeps node memory
near configured targets, requests capacity through an operator-provided event
channel, and migrates branches without changing their identity or lineage.

The design builds on the implemented distributed-fork and sparse-dehydration
protocols described in [Distributed live fork](ttc_distributed_live_fork.md).
It does not introduce distributed shared writable memory: branches share page
versions while colocated and obtain independent resident copies after a
cross-node cut.

## Scope and assumptions

Elastic cluster mode assumes mutually trusted mTLS-authenticated `ttcd` nodes,
one shared TTC data root, a common architecture and base-page size, compatible
TTC protocol features, and reliable host networking. Images may be cached
locally by digest. Branch filesystems, managed volumes, dehydration metadata,
and immutable memory chunks live in the shared repository.

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
8. **Bounded metadata:** placement planning must not require a permanent
   object per 4 KiB page. Coarse summaries may guide scheduling without
   changing page-level correctness.

## Identities and records

Each daemon publishes a lease-bearing node record containing its node ID,
start incarnation, state (`joining`, `ready`, `pressured`, `draining`, or
`offline`), advertised control endpoint, capabilities, memory target, actual
and reserved memory, labels, and lease timestamps. `Incarnation` changes on
every daemon start and prevents an old daemon from renewing or deleting a
newer process's lease.

The first implementation stores node records atomically under
`cluster/nodes/` in the shared root and locks each node record while changing
it. This is sufficient for dynamic discovery and controlled NFS qualification.
Branch ownership later moves behind a linearizable catalog interface;
production multi-writer placement must not depend solely on cache-coherent NFS
reads.

A branch placement record identifies the family, branch, owner node and
incarnation, monotonically increasing ownership generation, lifecycle state,
filesystem lease, and active operation ID. Dehydrated and exited branches are
unplaced. Rehydration chooses a compatible node and acquires a new placement
generation. Running and paused branches have exactly one owner.

```text
joining -> ready -> pressured -> ready
                 \-> draining -> offline
joining -----------------------> offline
```

A draining node refuses new runs, forks, rehydrations, and incoming
migrations. Lease expiry marks a node unavailable but does not prove guest
execution stopped; ownership must pair lease loss with a local watchdog and
fencing action.

## Unified client view and routing

The shared data root already exposes persistent metadata to every node. Phase
1 includes remote-owned families in `ttc ls/ps --json` and adds `node_id` to
the stable family record. Human output hides placement by default; `inspect`
and explicit diagnostics expose it.

For mutating and streaming commands, the receiving daemon resolves branch
placement and either executes locally or proxies to the owner. Proxy requests
carry the original request ID, expected placement generation, terminal
metadata, and framed stdin/stdout. The owner rejects a stale generation. Named
exec sessions and retained output allow reconnection after migration.

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

Branches are vertices in a sharing hypergraph. A shared page version is a
hyperedge connecting every referencing branch; cutting it predicts duplicated
residency. Exact 4 KiB graphs are too large to rebuild continuously, so the
first planner uses stable 1 MiB chunk/version identities and splits unusually
sparse chunks only when measurements justify it.

The objective combines memory-target imbalance, shared-chunk cut bytes,
private bytes migrated, expected demand-fault traffic, destination-resident
chunk reuse, and fixed per-family Sentry overhead. An incremental deterministic
greedy/FM-style bipartitioner is implemented in Go first so TTC's static
binaries do not acquire a native solver dependency.

Pressure policy may migrate, dehydrate an inactive branch, request capacity,
or temporarily reject fan-out. TTH supplies policy weights and may mark
latency-sensitive branches as non-dehydratable.

## Capacity events

Provisioning remains outside TTC. A pluggable executable or HTTP event sink
receives an idempotent request such as:

```json
{
  "version": 1,
  "request_id": "capacity-...",
  "operation": "acquire",
  "nodes": 2,
  "memory_bytes_each": 68719476736,
  "architecture": "amd64",
  "repository_id": "cluster-a",
  "reason": "memory-pressure",
  "deadline": "2026-08-21T12:00:00Z"
}
```

Delivery is at least once and receivers deduplicate `request_id`. A release
event is emitted only after a node is offline with no ownership or dependency
records.

## Graceful drain

1. Atomically mark the node draining and reject incoming work.
2. Unplace dehydrated and exited branches.
3. Reserve destinations and migrate running and paused partitions.
4. Drain volatile page dependencies to shared chunks.
5. Transfer filesystem and network leases.
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

* Add lease-bearing node records and heartbeats in the shared repository.
* Add advertised control endpoints and dynamic peer client construction.
* Retain static peers as explicit overrides during transition.
* Include remote-owned persistent families in list and inspect JSON.
* Report membership, compatibility, lease state, and actual memory usage.

Exit: nodes can join after existing daemons start; every node sees the same
families; stale and duplicate node incarnations are detected.

### Phase 2: routed commands and manual migration

* Add daemon-to-daemon command proxying with placement-generation fencing.
* Implement `ttc migrate CONTAINER@BRANCH --target NODE` without changing
  branch identity or parent.
* Transfer the filesystem writer lease atomically and reconnect named exec
  sessions after cutover.

Exit: repeated migration between two nodes preserves process and filesystem
state, never runs both copies, and survives retries at every boundary.

### Phase 3: draining and capacity integration

* Add `ttc node drain`, admission control, dependency draining, and offline
  verification.
* Add executable and webhook capacity sinks with durable idempotency records.
* Resume incomplete drains after daemon restart.

Exit: a node with mixed running, paused, and dehydrated real workloads can
leave without branch loss or repository garbage.

### Phase 4: placement planner

* Export bounded chunk-sharing summaries and node actual/reserved memory.
* Implement deterministic bipartition dry runs and scoring diagnostics.
* Add watermarks, hysteresis, cooldowns, and destination reservations.
* Add optional automatic rebalance and capacity requests.

Exit: correlated sibling workloads use materially less cluster memory than
random placement and do not oscillate under a stable workload.

### Phase 5: qualification

Run unit, race, fuzz, and crash injection; multi-daemon loopback and local NFS
tests under delay, loss, and restart; systrap and KVM package-build,
browser/REPL, tmpfs, managed-filesystem, exec-session, and remote-fork
workloads; join/leave storms; thousands of branches; forced scheduler exits;
NFS stalls; and incomplete capacity requests.

Record source pause, cutover downtime, time to execution and independence, cut
bytes, transferred bytes, remote-fault percentiles, NFS operations, peak extra
RSS, and cleanup residue. A phase is not complete until its failure tests and
cleanup audits pass.
