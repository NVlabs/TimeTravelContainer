# Distributed live fork with remote copy-on-read memory

> **Planned storage replacement:**
> [TTC userspace extent store](ttc_userspace_extent_store.md) describes the
> family-level immutable 1 MiB chunk repository intended to replace
> branch-specific packed snapshots and shared-filesystem reflink requirements.
> Pebble is not part of that plan. This document continues to describe the
> currently implemented transport and packed-snapshot design where explicitly
> labelled.

Status: design and implementation plan; implementation in progress.

Implementation progress:

* Phase 0 protocol model, trace replay, invariant tests, randomized schedules,
  policy validation, and analytical pull simulator: implemented.
* Phase 1 `MemoryFile` snapshot ownership, page-granular acknowledgement,
  sender leases, loopback `AsyncReader`, asynchronous target-page loading,
  metadata-only allocator cloning, and whole-Kernel remote-memory cloning:
  implemented and tested.
* Phase 2 bounded TCP pull, checksummed metadata envelopes, multi-process
  whole-Kernel reconstruction, demand paging, prefetch, batched ACKs, retries,
  metrics, and target-local network reconstruction: implemented and tested.
* Phase 3 post-CoW push hints, reverse TCP push, idempotent pull/push races,
  coalescing, and demand/dirty/high-water/eager push policies: implemented and
  tested.
* Phase 4 mutually authenticated daemon coordination, transactional target
  publication, host-owned connection donation, end-to-end remote container
  restore, durable dependency state, restart recovery, and drain-before-source
  stop: implemented and tested in a two-daemon same-host cluster.
* Phase 5 policy controls, durable metrics, real-container benchmark harness,
  PMA-aware demand granularity, and adaptive locality selection: implemented
  and tested.
* Phase 6 coordinated sibling capture, shared source-page ownership,
  per-child transactions, parallel target reconstruction, and advanced
  transport evaluation: implemented and tested.
* Phase 7 durable shared-memory handoff, demand-only target materialization,
  source-independent filesystem repository lifetime, and dehydration
  integration: implemented and tested.

## Planned 1 MiB shared-chunk data plane

All cooperating `ttcd` instances use the same shared TTC data root. Durable
memory is represented by immutable, content-addressed files containing up to
1 MiB, or 256 stable logical base-page slots. Immutable manifests map logical
chunk coordinates and page-version vectors to content IDs; branch records
point to manifests without encoding branch ancestry. Memory CoW and correctness
remain 4 KiB, while one node loads and evicts all valid slots of a version
together.

For an already-dehydrated branch, remote fork is metadata-only: the new branch
references the same manifest, the destination acquires a loader lease, and it
reads 1 MiB chunks directly from the shared filesystem on demand. Loading a
chunk installs every valid page. Consumption releases that manifest's
reference, but physical unlink waits until no manifest or loader lease can
still read the chunk.

Dehydration writes and synchronizes all chunks, publishes the manifest last,
and only then releases the Sentry and its memory. After publication, any node
can rehydrate the branch even if the source process and source node have exited.

The preferred placement is one active Sentry for each compatible global memory
lineage on a node, with all local branches imported into it. That permits one
resident chunk installation to serve local sibling branches and preserves
ordinary 4 KiB CoW between them. The current remote CLI instead creates each
destination container family as its own sandbox, so separately named remote
siblings on the same node use separate Sentries and share durable chunk files,
but not resident pages. Cross-Sentry resident-page sharing is not required for
correctness and remains a separate optimization if lineage consolidation is
not possible.

A live remote fork must not wait for the complete memory image to be written.
It therefore uses the existing page-granular TCP dependency as a bridge:

1. Node A pauses, establishes the fork-time CoW snapshot, publishes target
   metadata, and resumes the source.
2. Node B may start immediately and pull missing pages from A using the current
   race-tested transport.
3. In parallel, A or B groups preserved snapshot pages by stable 1 MiB logical
   chunk coordinate,
   hashes them, and publishes them atomically to the shared repository.
4. Immutable progress generations redirect completed coordinates to shared
   chunks. A target lookup chooses the newest committed generation; an
   in-flight TCP read and a newly published identical chunk are resolved
   idempotently.
5. After a complete durable manifest is committed and every target has switched
   from the live dependency, A releases the remaining page-server references.

This combines remote fork and dehydration without making snapshot construction
part of the source pause. The shared filesystem becomes the durable ownership
tier; TCP is temporary for live forks and unnecessary for forks of already
dehydrated branches.

The first format fixes the chunk load size at 1 MiB. It deliberately accepts up
to 256-page read/residency amplification to reduce shared-filesystem inode and
metadata pressure. Qualification must measure useful-page ratio, fault tail
latency, shared-filesystem operations, and source-release time before the format
is made stable. Pebble and remote SSTs are out of scope. The normative chunk
coordinates, residency/origin/promotion enums, local and cluster reference
classes, `L0`--`L8` transition points, source-write promotion, GC fencing, and
operation timing rules are specified in
[TTC userspace extent store](ttc_userspace_extent_store.md#chunk-coordinates-and-alignment).

## Current packed-snapshot baseline

TimeTravelContainer (TTC) can fork a branch running on node A into a new branch
on node B without transferring the complete memory image before the target
starts. The source briefly pauses to capture kernel metadata and establish
copy-on-write (CoW) protection. It then publishes an immutable packed memory
snapshot in the cluster's shared TTC data root, normally by reflinking the
used ranges of its persistent memory backing. Node B reads pages from this
snapshot on demand and installs them into its own persistent backing.

The durable shared snapshot is the default data plane. After it has been
published and the target metadata is committed, node A releases its in-process
snapshot references immediately; source writes, source deletion, and source
node shutdown require no page-level coordination with B. TTC deletes the
shared snapshot after B has materialized every required page or after the
target is removed. The original TCP pull/reverse-push protocol remains an
explicit experimental policy for clusters without shared durable memory
backing and for transport research.

Correctness remains page-granular, normally 4 KiB, but network transfers are
configurable ranges such as 4 KiB, 64 KiB, or 2 MiB. Pull, prefetch, push, and
adaptive policies are deliberately configurable so that they can be evaluated
under controlled network conditions and real workloads before TTC chooses
production defaults.

The default implementation assumes:

* mutually trusted `ttcd` instances;
* a common image store, TTC filesystem repository, and memory-snapshot
  directory visible on all nodes through a shared filesystem;
* reflink support in that shared filesystem for fast memory handoff, unless
  the operator explicitly accepts a full-copy fallback; and
* a reliable IP network for the mutually authenticated control plane; and
* one target node per distributed-fork operation, with one or more sibling
  children on that node.

Cross-target-node fan-out and transparent target failover remain later
extensions. A published target is already independent of the source process
and source node: its memory handoff and promoted filesystem repositories have
durable shared lifetimes.

## Goals

1. Keep the source pause independent of resident-memory size.
2. Allow the target to execute before its complete memory image is present.
3. Preserve the exact fork-time contents of every guest page.
4. Release all source-process snapshot references after the immutable shared
   snapshot is durably published; reclaim the shared snapshot after the target
   has installed all required pages.
5. Never wait for network I/O in the source application's write-fault path.
6. Support configurable transfer sizes, prefetchers, push policies, flow
   control, and an optional adaptive policy engine.
7. Make all pull/push races idempotent and testable under reordering,
   duplication, cancellation, and peer failure.
8. Measure both branch startup and interference with the running source.

## Non-goals of the first implementation

* Transparent continuation if node A fails before publishing the shared
  snapshot and target transaction.
* Sharing a writable guest page concurrently between nodes.
* Coherent external devices, host FDs, or external TCP connections. Existing
  distributed-fork resource policies still apply.
* Selecting one hard-coded transfer policy before workload measurements.
* Coherent writable memory shared between nodes. The shared file is an
  immutable handoff object, not distributed shared memory.

## Correctness and transfer granularity

For the default shared-snapshot path, the source exports the exact packed page
stream described by the signed allocator metadata. The target may read and
install that stream in configurable aligned ranges, but its present/missing
accounting remains page-granular. Publication uses a temporary file, sync, and
atomic rename; a transaction never advertises a partially written snapshot.
The target's persistent `application.mem` is distinct from the immutable
handoff file, so target writes cannot alter the snapshot.

For the optional TCP path, the unit of ownership and acknowledgement is a
guest base page. The transfer unit is an independently configurable, aligned
range containing one or more pages. A 2 MiB response therefore carries 512
individually accountable 4 KiB pages; its acknowledgement contains a page
bitmap or a run-length encoded set and does not imply that unrelated pages
were installed.

This separation permits large network operations without making reclamation
or correctness depend on large-page alignment. It also allows a partially
successful range transfer to be retried without retaining pages that the
target already owns.

## Default shared-snapshot invariants

1. **Publish before release:** node A retains immutable page ownership until
   the complete packed snapshot has been synced and atomically published.
2. **Immutable handoff:** no source or target write can mutate published
   snapshot contents.
3. **Install before reclaim:** TTC removes the handoff only after every target
   load range is local, or after the target transaction is removed.
4. **Durable repository lifetime:** writable filesystem repositories are
   promoted outside source-container metadata before target reservation and
   are garbage-collected only after no container spec references them.
5. **Dehydration drains dependencies:** dehydration materializes any remaining
   snapshot-backed pages before releasing the target Kernel graph. Rehydration
   retains the target's actual repository branch identity, even though a
   distributed target is presented as visible branch `main`.
6. **Idempotent recovery:** repeated reserve, publish, dependency polling,
   cancellation, and cleanup cannot publish two targets or delete a referenced
   snapshot/repository.

## Optional TCP protocol invariants

1. **Immutable snapshot data:** while a target still needs a fork-time page,
   the source holds a reference to immutable old data. Source writes use local
   CoW and never mutate that data.
2. **Install-before-ack:** node B acknowledges a page only after its bytes and
   page metadata are installed and visible to target faults.
3. **Release-after-ack:** node A drops the snapshot reference only after
   processing a valid acknowledgement for the current snapshot epoch.
4. **No network under memory locks:** page requests, responses, pushes, and
   acknowledgements are queued only after releasing MM, PMA, and `MemoryFile`
   locks.
5. **Idempotence:** duplicate or reordered messages cannot install different
   contents, decrement a source reference twice, or publish a page from another
   snapshot.
6. **One winner at the target:** a pull response and proactive push racing for
   the same page may both arrive, but exactly one installation transitions the
   page from missing to present.
7. **Failure retains ownership:** timeout, disconnect, or ambiguous delivery
   never causes the source to release an unacknowledged page.
8. **Unpublished construction:** node B does not expose the branch until its
   kernel metadata, remote-memory provider, and minimum bootstrap page set are
   ready.

## Architecture

### Control plane

The source `ttcd` coordinates a distributed-fork transaction with the target
`ttcd`. In the default shared-snapshot path it:

1. Reserve a target container identity and transaction UUID.
2. Validate the source and target image/filesystem identities.
3. Pause and validate the source using the existing atomic fork path.
4. Creates a `RemoteSnapshot`, resumes A with CoW protection in place, and
   exports its packed page stream into a temporary shared file.
5. Syncs and atomically publishes the shared snapshot, then sends B the small
   metadata envelope and relative snapshot path over mutual TLS.
6. Constructs B with an asynchronous reader backed by the shared snapshot and
   publishes it as paused or running.
7. Marks the transaction source-independent and releases A's `RemoteSnapshot`.
8. Deletes the shared snapshot once B is fully local, or when B is removed.

Before reservation, source-private ttcvol repositories are atomically renamed
into a cluster-stable family namespace. Open Sentry directory descriptors stay
valid across the rename, while updated OCI metadata lets future starts and the
target donate the stable paths. Reference-scanning garbage collection removes
the promoted repositories only after the final source/target container is
gone.

The optional TCP policy replaces steps 4-8 with the page protocol described
below. It retains source page references until target acknowledgements and may
use reverse push.

The transaction carries a random snapshot UUID plus a monotonically allocated
epoch. Every data-plane message includes the source sandbox identity,
snapshot UUID, memory-file identity, page offset, and length.

### Source-side data structures

Names are provisional:

```go
type RemoteSnapshot struct {
    id       SnapshotID
    epoch    uint64
    files    map[MemoryFileID]*RemotePageSet
    peer     PeerID
    policy   TransferPolicy
}

type RemotePageSet struct {
    // Dense bitmaps or range sets for the common states. Per-request objects
    // exist only while a transfer is in flight.
    required Bitmap
    inflight Bitmap
    acked    Bitmap
    orphaned Bitmap // Source branch has copied away from the old page.
}
```

The existing `MemoryFile` page reference is authoritative for lifetime. The
bitmaps describe remote protocol state; they do not replace local reference
counting. Large PMAs must not be eagerly split into per-page segments. On an
acknowledgement, the snapshot reference is dropped for the acknowledged page.
If A later writes that page, the normal fault path discovers that it is unique
and clears CoW without copying.

### Target-side data structures

```go
type RemotePageProvider struct {
    snapshot SnapshotID
    files    map[MemoryFileID]*TargetPageSet
    policy   TransferPolicy
}

type TargetPageSet struct {
    present    Bitmap
    installing Bitmap
    waiters    map[PageIndex]*pageWaiters
}
```

The provider should reuse the asynchronous page-loading integration currently
used by checkpoint restore rather than introduce a second kind of blocking
PMA. A target fault asks the provider for a range, coalesces with an existing
request when possible, and waits only for its required page. Prefetched
neighbors are installed through the same path.

### Data plane

Begin with multiplexed TCP using bounded binary messages and checksums. Keep
transport behind an interface so that io_uring, multiple TCP streams, or RDMA
can be evaluated later without changing page ownership semantics.

Required messages are:

* `PullRequest`: missing pages plus a preferred transfer range;
* `PageData`: snapshot identity, range, per-page presence bitmap, bytes, and
  checksum;
* `PushOffer` or unsolicited `PageData`: source-selected orphaned pages;
* `Ack`: pages installed at B;
* `Cancel`: target no longer needs pages or the transaction is aborting;
* `Drain`: request eager completion and source independence; and
* `Error`: permanent protocol or snapshot failure.

Requests and acknowledgements use bitmaps or ranges. They must not require one
packet or RPC per page. Flow control limits outstanding bytes separately from
outstanding request count.

## Page state machine and races

Avoid encoding all combinations into one large enum. Source ownership,
transfer attempts, and target installation are related but independently
tracked facts.

For one page, the normal sequence is:

```text
A: required + snapshot ref
       |
       | pull response or proactive push
       v
B: installing
       |
       | data verified and installed
       v
B: present ---- ACK ----> A: acked + snapshot ref released once
```

When A writes first, the existing local CoW path gives A a new page. After its
memory locks are released, it marks the immutable old page `orphaned` and
queues it for optional push. This notification must not perform socket I/O,
wait for queue space, or allocate unbounded memory in the fault path. A
preallocated, sharded queue records page ranges; overflow merely falls back to
target pull or a background bitmap scan.

The protocol must explicitly test these races:

| Race | Required result |
|---|---|
| Pull request vs. A write | Response always contains fork-time bytes; A continues with its new private page. |
| Pull response vs. proactive push | B installs either identical copy once and acknowledges once; the duplicate is discarded. |
| ACK vs. another A write | Snapshot reference is released once; A sees either shared CoW or unique-page fast path. |
| Retry vs. delayed original response | Both carry the same epoch and content; only one target transition wins. |
| Cancel vs. response/ACK | Transaction accounting chooses a terminal outcome without leaks or double release. |
| B destruction vs. active faults | Waiters fail, B stops, and A releases the cancelled snapshot only after no sender can use it. |
| A shutdown vs. incomplete B | A either completes a configured drain or B is stopped with an explicit dependency error. |
| Snapshot ID reuse/stale packet | Message is rejected without touching current page state. |

Use a small locked or atomic transition helper as the only writer of protocol
bitmaps and counters. Network callbacks, page faults, cancellation, and
reclaim workers call this helper rather than open-coding transitions.

## Configurable policies

Configuration belongs to `ttcd` defaults. Range and prefetch controls apply to
target reads from either the shared snapshot or TCP; push controls apply only
when the legacy TCP data plane is explicitly selected. Knobs include:

* transfer range: `4KiB`, `16KiB`, `64KiB`, `256KiB`, `1MiB`, or `2MiB`;
* prefetch: `none`, `sequential`, `fixed-window`, or `adaptive`;
* prefetch window and maximum outstanding bytes;
* push: `demand-only`, `dirty-orphan`, `memory-pressure`, or `eager-drain`;
* push/pull scheduling priority and fairness;
* request concurrency and transport connection count;
* ACK aggregation interval and maximum ACK batch;
* optional bandwidth cap; and
* adaptive-policy enablement, sampling interval, and hysteresis.

Configuration is reported in fork statistics so every benchmark is
reproducible. The selected default is a durable shared snapshot with demand-only
source behavior and adaptive target prefetch beginning at 64 KiB, bounded by
the negotiated 2 MiB maximum, with four parallel reads. No proactive page push
is needed: publication transfers ownership from the source process to the
durable snapshot. ACK aggregation, outstanding network bytes, and non-demand
push policies remain configurable for explicit TCP experiments.

### Adaptive strategy

The adaptive controller observes:

* page-fault rate and p50/p95/p99 remote-fault latency;
* ratio of useful demanded pages to prefetched pages;
* sequential-run length and address-space locality;
* source dirty/orphan rate;
* retained snapshot bytes and their growth rate;
* measured RTT, delivered bandwidth, queue depth, and retransmissions; and
* time since the target last touched a region.

It may enlarge or shrink transfer ranges, change the prefetch window, enable
pressure-driven push, or enter eager drain. Changes use hysteresis and minimum
residence times so oscillation cannot amplify load. Policy changes never alter
page correctness state.

The first adaptive algorithm should be intentionally simple and explainable:

1. Start at 64 KiB with sequential prefetch. The shared-snapshot default needs
   no push; an explicit TCP experiment may independently enable dirty-orphan
   push.
2. Accumulate exact fault-adjacency samples across 50 ms observations. After a
   strongly sequential run, grow directly to the negotiated transport limit;
   this avoids a slow doubling ladder on short RTT-bound workloads.
3. When at most one quarter of a sampled run is adjacent, halve the window
   with hysteresis until loading is page-granular.
4. Keep correctness and network transfer units separate: the address-space
   mapper limits demand-only mappings to the active prefetch window, while the
   transport may split or coalesce that range independently.
5. Enter eager drain when source shutdown is requested. Pressure-driven
   switching and latency/usefulness inputs remain later controller extensions.

## Expected code boundaries

The implementation should follow the existing ownership boundaries:

* `pkg/sentry/pgalloc`: remote-snapshot page references, per-file page sets,
  asynchronous remote loading, installation, acknowledgement-driven release,
  and reference-audit hooks. Extend `StartAsyncPagesFileLoad` and the
  `stateio.AsyncReader` integration where practical instead of creating a
  parallel paging system.
* `pkg/sentry/mm`: expose a post-CoW notification containing the immutable old
  `MemoryFile` range. The callback runs or queues work only after PMA/MM locks
  are released. Preserve the existing `HasUniqueRef` fast path.
* `pkg/sentry/kernel`: distributed-fork validation context, metadata-clone
  lifecycle, paused target construction, cancellation, and metrics. It must
  not contain transport policy.
* `runsc/boot`: source/target sandbox coordination and attachment of the remote
  page provider to a newly loaded Kernel.
* `runsc/ttc`: daemon peer configuration, transaction RPCs, persistence of
  control-plane state, CLI/API options, status, and cleanup.
* `sdk`: expose the final distributed-fork and statistics API only after the
  daemon contract is versioned; SDKs must not reproduce protocol policy.
* a new internal transport package: bounded message codec, peer sessions,
  flow control, retry, and pluggable TCP/RDMA transport. It must know page
  identifiers but not guest virtual addresses or PMAs.

Before implementation, confirm whether the hidden local branch used by TTC's
current fork path can directly own the `RemoteSnapshot`, or whether its PMA
references should be transferred into a smaller snapshot-only object. The
choice must preserve the current pause complexity: no fork-time scan or byte
copy proportional to resident memory merely to populate protocol metadata.

## Development phases

### Phase 0: protocol model and measurement harness

* Implement the page transition system as a standalone Go model.
* Add deterministic event injection for pulls, writes, pushes, ACKs, retries,
  cancellation, and peer failure.
* Add property tests for the invariants and a trace format for replaying a
  failing schedule.
* Build a synthetic benchmark that models 4 KiB pages and configurable range
  requests without integrating Sentry.

Exit criteria: exhaustive bounded schedules and fuzzing find no double release,
wrong-version install, missing wakeup, or retained reference after a completed
transaction.

### Phase 1: local asynchronous remote-page provider

* `RemoteSnapshot` reference ownership and metadata-only `MemoryFile` cloning
  are implemented in `pgalloc`.
* The asynchronous restore/page-loader path now supports demand-only loading,
  explicit drain, cancellation, and both loopback and TCP readers.
* A full `Kernel.LiveFork` can now use a distinct metadata-cloned target
  `MemoryFile`; target page access is forced through the asynchronous loopback
  transport, and Kernel release cancels the remaining remote dependency.
* Implement pull and batched ACK only; use demand-only policy.

Exit criteria: recursive memory tests, process-heavy containers, and randomized
read/write workloads remain byte-exact; all snapshot references reach zero
after a full drain and branch destruction.

### Phase 2: network pull between two Sentries

* The bounded TCP pull/data/ACK transport, checksums, bounded reconnect retry,
  and ACK-only recovery are implemented. It releases source references only
  after destination installation and an acknowledged ACK. Complete the
  transaction handshake, flow control, and target dependency handling.
* A token-authenticated session hello and bounded, checksummed metadata stream
  now transfer Kernel metadata separately from memory page data.
* MemoryFile allocator metadata now has explicit pointer-free export/import
  APIs. A versioned, allocation-bounded, SHA-256-protected session envelope
  carries Kernel metadata, allocator metadata, page-stream mappings, and branch
  identity between independent Sentry processes.
* A subprocess target test reconstructs its own MemoryFile from the envelope
  and fetches exactly one demanded 4 KiB page over TCP while leaving the rest
  remote.
* Portable whole-Kernel capture/load APIs now serialize ordinary PMAs, rebuild
  them against a separate target allocator, attach TCP demand paging, complete
  VFS restore, and start the reconstructed Kernel without process-local PMA
  snapshot pointers.
* A whole-Kernel subprocess target now fetches the portable envelope from the
  source process, creates its own platform context and MemoryFile, restores and
  starts the Kernel, drains remote pages, and tears down cleanly.
* Portable capture now validates and serializes contained netstack state. The
  target load API requires ttcd to recreate target-local virtual-link/NAT
  plumbing, then restores netstack and resets inherited external connections.
* Configurable TCP range sizes and request concurrency are implemented. The
  demand loader now also supports a bounded, page-aligned forward sequential
  or two-sided fixed prefetch window: waiter-owned reads keep priority while
  spare asynchronous slots fetch adjacent pages in multi-page requests. The
  active window can be changed at runtime without reconstructing the target,
  providing the switch point for an adaptive controller. Automated policy
  selection remains to be added.
* Installed pull ranges may be acknowledged individually or through a bounded
  coordinator that batches ranges by page count and timer. Batch confirmation
  gates target completion, and loss of `ACK_BATCH_OK` retries the same
  idempotent ranges. Normal, response-loss, and race-detector tests cover this
  path.
* Reader, page-server, push-sender, and push-receiver counters are exposed,
  including installed bytes, failures, retries, ACK-only recovery, skipped
  pushes, and pending push receipts. Token-free reader configuration/state can
  be captured with experiment results. Daemon-wide aggregation and a complete
  multi-component transaction dump remain to be added.

Exit criteria: B starts and runs before full transfer; induced duplication,
reordering, corruption, disconnects, and reconnects produce either correct
execution or a cleanly failed target, never silent corruption.

### Phase 3: source-write notification and proactive push

* A non-blocking post-CoW notification now identifies old snapshot ranges only
  after MM locks are released and feeds a bounded per-snapshot push-hint queue.
  A loopback push pump installs not-yet-pulled ranges and ACKs only after
  installation; ranges whose pull has started remain pull-owned. Connect this
  same state transition to network transport-side push workers. A reverse TCP
  push channel now implements `PUSH_OK` (installed, source may release) and
  `PUSH_SKIP` (pull owns the range, source must retain it).
* An adversarial transport test now holds a pull after it claims a page, races
  a reverse-channel push for the same page, and verifies that `PUSH_SKIP`
  retains the source reference until the pull installs and acknowledges it
  exactly once.
* Proactive pushes now use stable transaction identities, bounded reconnect
  retries, and an idempotent commit handshake. The target retains a receipt
  until commit, so loss of `PUSH_OK` replays success instead of misclassifying
  an already installed page as pull-owned. Fault-injection and race-detector
  tests cover this response-loss path.
* The sender now drains bounded batches of queued COW hints, sorts them, and
  coalesces adjacent or overlapping ranges before applying the configured
  network chunk size. Sharded queues and batched installation
  acknowledgements remain to be added.
* `RemoteSnapshot` can enumerate bounded runs of pages that remain
  unacknowledged. The push transport now implements demand-only,
  dirty-orphan, retained-memory high-water, and explicit eager-drain policies.
  Tests cover policy behavior without source writes, per-page reclamation, and
  restoration of the source allocator's unique-reference fast path after a
  complete drain.

Exit criteria: the source write path contains no network wait; stress tests show
no deadlock under queue saturation; acknowledged obsolete pages are reclaimed;
and source application throughput regression stays within the performance
budget selected below.

### Phase 4: cross-node `ttcd` orchestration

* A versioned capability contract now validates architecture, base page size,
  maximum transfer size, shared-repository identity, and required feature
  intersection before capture. Fork identities bind the request to image and
  rootfs SHA-256 digests.
* An explicit transaction state machine now keeps target reservation,
  source capture, restore, paused readiness, publication, draining, and
  independence separate. A private per-daemon store atomically persists each
  revision, rejects stale writers, reloads after restart, and permits deletion
  only after a terminal state.
* A TLS 1.3 control plane now requires CA-verified client certificates and
  binds the certificate common name to the claimed TTC node ID. Its bounded
  JSON API supports idempotent hidden-target reservation, reconnect-safe
  status, and revisioned cancellation; per-target active transaction quotas
  are enforced before reservation. `ttcd` and `ttc daemon start` now accept
  the cluster listen/node/repository/certificate/key/CA/quota configuration,
  start the peer listener only with a complete configuration, report its
  capabilities through daemon status, and shut it down with the local daemon.
* `ttc fork --target` publishes a restored target through the ordinary TTC API;
  the language SDKs use the same versioned daemon command contract.
* Filesystem publication is transactional on the common repository and target
  cleanup is idempotent.
* Target loader progress is reported in bytes. The target daemon durably marks
  a transaction independent or failed, both nodes resume monitors after daemon
  restart, `ttc inspect` includes the transaction, and source daemon shutdown
  requests and waits for eager drain before stopping source containers.
* Host-side `ttcd` owns page listeners and outgoing connections, then donates
  connected streams into isolated Sentries. This keeps host networking outside
  the guest kernel while retaining direct source-to-target page transfer.

Exit criteria: repeatable end-to-end forks work across two hosts, daemon restart
does not publish partial target containers, and cleanup leaves neither memory
references nor filesystem transaction debris.

### Phase 5: policy experimentation and adaptive selection

* All initial policy knobs are exposed through `ttcd` and persisted with each
  transaction. Final pull, push, ACK, byte, page-release, and independence-time
  counters are committed atomically with the independent transition.
* `tools/ttc/distributed_benchmark` runs two real daemons with independent
  runtime roots, short-lived mutual-TLS identities, a shared repository, an
  unmodified OCI image, and an in-container page workload. It supports
  `tc netem` delay, rate, and loss, emits raw JSONL, and verifies both source
  and target after every fork.
* Demand-only address-space mapping now follows the current prefetch window
  rather than mapping a hard-coded 32 MiB PMA. This makes a 4 KiB policy truly
  page-granular and prevents sparse workloads from silently loading whole PMAs.
* The adaptive controller uses accumulated page-fault adjacency, grows to the
  negotiated 2 MiB limit for a sequential run, and halves to page-sized demand
  loading for random runs.

Corrected representative measurements used a 16 MiB prefaulted working set,
four parallel requests, 1 Gbit/s shaping, and 5 ms one-way delay (about 10 ms
request/response RTT):

| Workload and policy | Workload time | Page bytes | Pull requests |
|---|---:|---:|---:|
| Sequential, fixed 64 KiB | 5656 ms | 17.21 MB | 525 |
| Sequential, fixed 1 MiB | 792 ms | 18.24 MB | 41 |
| Sequential, adaptive | **695 ms** | 18.52 MB | 35 |
| Sparse random, fixed 4 KiB | 4583 ms | 2.35 MB | 574 |
| Sparse random, fixed 64 KiB | **3284 ms** | 15.68 MB | 287 |
| Sparse random, adaptive | 4279 ms | 2.96 MB | 219 |

Adaptive sequential loading finished about 12% faster than the best fixed
sequential window tested. On sparse random access, adaptive stayed within 7%
of conservative fixed 4 KiB latency while transferring only 26% more bytes;
fixed 64 KiB was faster only by spending about 6.7 times as much bandwidth.
These results select adaptive mode as the workload-independent default. A fixed
window remains useful when bandwidth is cheap and minimizing sparse-fault
latency is the only objective.

Exit criteria: adaptive mode avoids catastrophic choices, remains within 10%
of the best fixed policy on representative cells where one policy dominates,
and documents cases where manual configuration remains preferable. This is an
experimental target, not a correctness requirement.

### Phase 6: multiple children and advanced transports

`ttc fork --target NODE SOURCE CHILD...` now creates all requested siblings
from one atomic source pause and one kernel-metadata capture. The allocator
adds one physical snapshot reference per source page, not one per child. Each
child has its own release bitmap, leases, metrics, page session, filesystem
branch, and durable transaction. A source page becomes reclaimable only after
every live child has acknowledged or cancelled it. This preserves independent
failure and cleanup while avoiding reference-count and metadata work that
scales with sibling count.

Target reservations are still quota-serialized, but preparation and
publication use per-transaction locks and run concurrently. Failure before
publication rolls back every sibling; failure after publication remains an
independent durable transaction outcome. Unit tests cover staggered ACKs,
cancel/ACK races, sibling close, shared page lifetime, batch rollback, and
control-plane lock independence. A real four-child test verifies all guest
workloads, source and target liveness, independent terminal state, and exact
release of every source snapshot page.

On an unshaped same-host two-daemon setup with an 8 MiB prefaulted BusyBox
workload, three runs produced these median wall times:

| Capture | Fork command | All children independent |
|---|---:|---:|
| One child | 262 ms | 532 ms |
| Four siblings, one batch capture | **668 ms** | **1,985 ms** |
| Four siblings, four sequential captures | 1,063 ms | 2,397 ms |

Batch capture reduced four-child fork-command latency by 37% and time to full
independence by 17%. Every transaction in all runs released
`pages_released == pages_total`; all source and target health probes passed.
These are development-machine measurements, not general performance claims.

The current bounded multi-stream TCP transport remains the default after the
advanced transport evaluation:

* **RDMA** can reduce CPU use and tail latency on specialized clusters, but
  memory registration/pinning, hardware and module requirements, and a more
  complex revocation protocol conflict with TTC's portable rootless baseline.
  The page-provider boundary remains suitable for a future negotiated RDMA
  backend.
* **io_uring** can reduce host syscall and scheduling overhead, but it does not
  remove remote-fault RTT. With donated streams and concurrent range requests,
  it is an incremental transport optimization rather than an architectural
  prerequisite.
* **Compression** can help on bandwidth-limited links but adds latency and CPU
  cost, especially for already-compressed or high-entropy pages. It should be
  negotiated per session and selected from measured compressibility.
* **Content deduplication** requires hashing every candidate page and provides
  little benefit when branches already share one immutable source snapshot.
  It is better suited to optional background replication.
* **Target eviction and source-independent replication** change ownership and
  failure semantics. They remain future protocol extensions rather than being
  hidden behind the current ACK contract.

Cross-node siblings therefore share source memory efficiently today, while
specialized transports and failover remain additive extensions with explicit
negotiation and tests.

### Phase 7: durable shared handoff and source independence

The default paging policy is now `demand-only`. When both nodes advertise the
`shared-memory-snapshot` capability, the source exports the immutable packed
page stream into `cluster/memory-snapshots/<transaction>.mem`. Reflink range
cloning keeps this proportional to allocator metadata rather than copied RAM
on a capable shared filesystem; the daemon's explicit slow-copy mode permits
a buffered fallback for development filesystems.

The target reserves a persistent `application.mem`, restores the Kernel from
the small metadata envelope, and uses the shared file as its asynchronous page
reader. Target faults and adaptive prefetch populate the persistent backing.
The source transaction becomes independent as soon as target publication
succeeds, so source-side page services and dirty-page push workers are absent.
Dependency monitoring continues on B and removes the handoff file after the
last page is materialized. Dehydration forces that materialization and then
composes normally with rehydration.

Writable rootfs and private host-mount repositories are promoted into
`cluster/repositories/<source-family>/` before reservation. Source and target
specs reference the same stable repository while their ttcvol branches remain
independent. Removing the source therefore cannot invalidate B; removing the
last referencing target garbage-collects the promoted repository.

Validation includes unit tests for packed stream order, policy negotiation,
path validation, repository promotion/idempotence/reference lifetime, and the
distributed target's filesystem-branch identity across dehydration. A real
two-daemon test verifies target writes, source deletion, process and filesystem
continuity, dehydration/rehydration, daemon restart, shared-snapshot cleanup,
and final repository garbage collection. The TCP pull/push implementation and
simulator remain covered as an optional policy rather than the default.

## Test plan

### Unit and model tests

* Table-test every legal and illegal page transition.
* Property-test reference conservation:

  ```text
  source snapshot refs = required pages not terminally ACKed or cancelled
  ```

* Verify install-before-ack and exactly-once release with duplicate messages.
* Test range operations with unaligned ends, sparse bitmaps, partial success,
  zero pages, huge offsets, and integer overflow.
* Run fuzzers over message decoding and page-event schedules.
* Run transition and transport tests with Go's race detector and lock-order
  checks.

### Deterministic race tests

Add barriers at every state transition and enumerate the important orderings
from the race table. Each test records page contents, source/target states,
reference counts, outstanding waiters, and emitted messages. Repeat with one
page, adjacent pages in one transfer, and pages sharing one large PMA.

The CI suite should execute at least 10,000 seeded randomized schedules per
protocol change. Any failure prints a seed and replayable event trace.

### Fault-injection tests

Inject:

* delayed, reordered, duplicated, truncated, and checksum-corrupted data;
* ACK loss and retry after installation;
* connection loss during headers, page data, and ACK batches;
* source or target daemon termination at each transaction phase;
* target cancellation with active faults and pushes;
* bounded-queue exhaustion and artificial allocation failure; and
* stale messages from an earlier snapshot epoch.

There must be no silent data corruption, use-after-free, double decrement,
deadlock, leaked waiter, or published half-constructed container.

### Memory correctness workloads

* Fill every page with `(snapshot ID, virtual address, generation)` and verify
  it after randomized A/B reads and writes.
* Exercise anonymous memory, file-backed private mappings, shared mappings,
  forked guest processes, sparse address spaces, huge VMAs, executable pages,
  stacks, and memory under reclaim.
* Compare full target memory hashes after drain with an eager-copy reference.
* Run nested and recursive TTC fork tests after distributed support stabilizes.

### Network emulation matrix

Run source and target in separate Docker network namespaces and use `tc netem`
plus token-bucket shaping. The harness records its exact commands and effective
link measurements.

Representative dimensions are:

* bandwidth: 100 Mbit/s, 1, 10, 25, and 100 Gbit/s;
* round-trip latency: 0.1, 1, 10, and 50 ms;
* loss: 0%, 0.01%, and 0.1%, with optional reordering;
* transfer size: 4, 16, 64, and 256 KiB, 1 MiB, and 2 MiB;
* concurrency: 1, 4, 16, 64, and 256 outstanding ranges;
* prefetch policy: none, sequential, fixed-window, and adaptive; and
* push policy: demand-only, dirty-orphan, pressure-driven, and eager drain.

A complete Cartesian product is unnecessarily large. Use a staged experiment:

1. sweep chunk size and concurrency with synthetic sequential and random reads;
2. keep the Pareto-optimal combinations and sweep prefetch/push policies;
3. run retained combinations against real workloads;
4. validate adaptive mode against the best fixed results; and
5. repeat finalists at least 20 times with randomized run order.

### Workloads

Synthetic workloads:

* sequential scan;
* uniform and Zipf random 4 KiB reads;
* pointer chasing with one dependent access at a time;
* sequential and random source dirtying;
* disjoint and overlapping A/B working sets; and
* configurable sparse target working sets from 1% to 100% of RSS.

Real workloads:

* interactive shell and package/build environment;
* Chromium with warmed pages and scripted browsing;
* language server plus source tree and incremental compilation;
* PostgreSQL or Redis with an initialized dataset;
* Python/Node agent tool-use environment;
* parallel test runner; and
* a long-running service whose source latency is measured while children
  start and drain.

### Baselines and metrics

Compare against:

* local TTC live fork;
* eager cross-node memory copy;
* TTC checkpoint/restore transfer when available; and
* relevant forkd and AgentENV measurements on the same nodes.

Record:

* source pause time and target time-to-first-instruction;
* time-to-ready for the workload and time-to-full-drain;
* remote-fault latency distribution;
* source application p50/p95/p99 latency and throughput;
* transferred, useful-prefetched, duplicate, retried, and compressed bytes;
* source retained snapshot bytes and target resident bytes over time;
* CoW faults, copies avoided after ACK, and pages reclaimed;
* CPU time, context switches, allocator pressure, and network utilization; and
* failures, retries, policy transitions, and transition reasons.

Results must distinguish initial responsiveness from eventual total cost. A
policy that starts B quickly but severely stalls A or transfers unused memory
is not automatically better.

## Initial performance budgets

These are gates for regressions, not claims about final product performance:

* Distributed-fork source pause should remain independent of RSS and add no
  more than 10% to the equivalent local TTC metadata-fork pause at the same
  object count.
* With the transport disconnected but queues available, the extra source
  first-write CPU cost should remain below 5% relative to local TTC CoW in a
  page-dirtying microbenchmark.
* No source application thread may block on remote ACK or transport queue
  capacity.
* After a successful drain, retained remote-snapshot references and bytes on A
  must be zero.
* After target cancellation, all transaction resources must return to baseline
  within a bounded cleanup interval.

Budgets should be revised from measured data rather than weakened to hide a
regression.

## Observability and reproducibility

Each transaction exposes:

* snapshot and peer IDs;
* negotiated protocol and transport versions;
* full fixed-policy configuration and adaptive transitions;
* pages in required, in-flight, installed, orphaned, and acknowledged states;
* bytes retained, requested, pushed, duplicated, retried, and reclaimed;
* fault and ACK latency histograms; and
* dependency and drain status.

Benchmark output includes TTC commit, kernel version, CPU topology, NIC/link
settings, image digest, workload revision, random seed, and network-emulation
configuration. This is required before comparing policy combinations.

## Implementation order and review boundaries

Keep reviews small and preserve a working local TTC at every stage:

1. state model, invariant tests, and trace replayer;
2. remote snapshot page-reference API;
3. loopback provider and asynchronous fault integration;
4. TCP transport and protocol decoder;
5. range pull, ACK batching, and prefetch;
6. post-CoW orphan notification and push;
7. cross-daemon transaction orchestration;
8. policy experiment harness;
9. adaptive controller; and
10. multi-consumer or advanced transports.

The race model and tests land before the Sentry write-path hook. Protocol
changes require corresponding model transitions, invariant tests, and an
upgrade/compatibility decision.
