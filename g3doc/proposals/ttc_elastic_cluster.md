# Elastic TimeTravelContainer clusters

Status: phases 1 through 4 are implemented with the limitations called out
below. Identity-preserving migration now uses live sparse memory transfer;
phase 5 physical-host and failure-injection qualification remains in progress.

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

Interrupted migration recovery uses three durable facts: the placement record
is the fenced transaction intent, the branch's persistent node ID says whether
ownership cutover happened, and the branch lifecycle says whether runtime
restoration is required. Before ownership cutover, recovery never touches an
operation while its exact source incarnation retains a live lease; after lease
loss, source-owned transactions restore and atomically roll back to the
source's new incarnation. Once ownership has moved, it is never rolled back:
the target may restore and commit immediately, even if the source remains
online after losing a response. Restoration is verified in the Sentry before a
stable placement is published.

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

Provisioning remains outside TTC. TTC publishes a durable JSON request in
`$TTC_DATA_ROOT/cluster/capacity-requests/<source-node>.json` when pressure or
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
Receivers should still deduplicate `request_id`. Native executable/HTTP
adapters and release events are future additions.

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

**Implemented.**

* Add lease-bearing node records and heartbeats in the shared repository.
* Add advertised control endpoints and dynamic peer client construction.
* Retain static peers as explicit overrides during transition.
* Include remote-owned persistent families in list and inspect JSON.
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
* Implement `ttc migrate CONTAINER@BRANCH --target NODE` without changing
  branch identity or parent.
* Transfer the filesystem writer lease atomically and reconnect named exec
  sessions after cutover.
* Recover and retry migration transactions after daemon loss. **Implemented.**

Exit: repeated migration between two nodes preserves process and filesystem
state, never runs both copies, and survives retries at every boundary.

### Phase 3: draining and capacity integration

**Implemented with a durable capacity-provider interface.** `ttc node ls` and
`ttc node drain` provide dynamic membership diagnostics, admission fencing,
active and cold branch evacuation, restart-safe retry, and dependency draining.
A durable per-node intent is written before cordoning. A restarted daemon
rejoins directly in `draining`, reconciles surviving Sentries, and resumes the
remaining ownership scan automatically. A drained daemon remains online for
diagnostics until `ttc daemon stop` marks it offline and consumes the completed
intent. Drain and rebalance publish a coalesced durable capacity request when
there is no feasible target. A daemon can deliver that request to either an
executable or an HTTP webhook. Attempts, failures, acceptance, retry timing,
and a stable idempotency key survive restart. TTC, rather than the provider,
removes the request after compatible usable capacity appears in membership.

* Add `ttc node drain`, admission control, dependency draining, and offline
  verification.
* Add executable and webhook adapters over the durable capacity outbox.
  **Implemented.**
* Resume incomplete drains after daemon restart. **Implemented.**

Exit: a node with mixed running, paused, and dehydrated real workloads can
leave without branch loss or repository garbage.

### Phase 4: placement planner

**Implemented as an initial bounded planner.** `ttc node rebalance` and the
opt-in `--cluster-auto-rebalance` loop use cgroup memory as the hard signal and
exact per-family resident-sharing hyperedges for move selection. The Sentry
coalesces physical 4 KiB page versions with identical branch membership into
one weighted edge; this preserves exact placement costs without returning one
JSON object per page. Responses are bounded at 4,096 distinct membership sets
and report truncation explicitly instead of silently planning from incomplete
data. Cgroup memory remains the hard admission signal.

* Export bounded resident-sharing summaries and node actual/reserved memory.
* Implement deterministic bipartition dry runs and scoring diagnostics.
* Add watermarks, hysteresis, cooldowns, and destination reservations.
  **Implemented.** Reservations are incarnation-fenced, atomically reject
  concurrent overcommit, expire after source loss, and remain charged through
  migration completion and the target's next fresh heartbeat.
* Add optional automatic rebalance and capacity requests.

Exit: correlated sibling workloads use materially less cluster memory than
random placement and do not oscillate under a stable workload.

### Phase 5: qualification

**In progress.** See [Elastic cluster qualification](../benchmarks/ttc_elastic_qualification.md)
for completed local, migration, and NFS cases and remaining physical-node/KVM
coverage.

Run unit, race, fuzz, and crash injection; multi-daemon loopback and local NFS
tests under delay, loss, and restart; systrap and KVM package-build,
browser/REPL, tmpfs, managed-filesystem, exec-session, and remote-fork
workloads; join/leave storms; thousands of branches; forced scheduler exits;
NFS stalls; and incomplete capacity requests.

Record source pause, cutover downtime, time to execution and independence, cut
bytes, transferred bytes, remote-fault percentiles, NFS operations, peak extra
RSS, and cleanup residue. A phase is not complete until its failure tests and
cleanup audits pass.
