# Distributed live fork with remote copy-on-read memory

Status: design and implementation plan; not yet implemented.

Implementation progress:

* Phase 0 protocol model, trace replay, invariant tests, randomized schedules,
  policy validation, and analytical pull simulator: implemented.
* Phase 1 `MemoryFile` snapshot ownership, page-granular acknowledgement,
  sender leases, loopback `AsyncReader`, and asynchronous target-page loading:
  implemented and tested. Metadata-only allocator cloning and MemoryManager PMA
  rebinding to a distinct target `MemoryFile` are also implemented; full Kernel
  object-graph orchestration is pending.
* Phases 2 through 6: pending.

## Summary

This proposal extends TimeTravelContainer (TTC) so that a branch running on
node A can be forked into a new branch on node B without first writing or
transferring the complete memory image. The source briefly pauses to capture
kernel metadata and establish copy-on-write (CoW) protection. Node B then
loads memory on demand directly from node A. The source retains each fork-time
page until node B confirms that it has installed that page.

Correctness remains page-granular, normally 4 KiB, but network transfers are
configurable ranges such as 4 KiB, 64 KiB, or 2 MiB. Pull, prefetch, push, and
adaptive policies are deliberately configurable so that they can be evaluated
under controlled network conditions and real workloads before TTC chooses
production defaults.

The initial implementation assumes:

* mutually trusted `ttcd` instances;
* a common image store and TTC filesystem repository visible on all nodes,
  initially through a shared filesystem;
* a reliable, high-speed IP network between nodes; and
* one remote child per distributed-fork operation.

RDMA, multiple remote consumers of one memory snapshot, source-independent
failover, and distributed filesystem CoW are later extensions. The memory
protocol must not depend on NFS and must not route page contents through it.

## Goals

1. Keep the source pause independent of resident-memory size.
2. Allow the target to execute before its complete memory image is present.
3. Preserve the exact fork-time contents of every guest page.
4. Release the source's snapshot reference page by page after the target
   durably installs the corresponding data in its Sentry `MemoryFile`.
5. Never wait for network I/O in the source application's write-fault path.
6. Support configurable transfer sizes, prefetchers, push policies, flow
   control, and an optional adaptive policy engine.
7. Make all pull/push races idempotent and testable under reordering,
   duplication, cancellation, and peer failure.
8. Measure both branch startup and interference with the running source.

## Non-goals of the first implementation

* Transparent continuation after node A fails while node B still depends on
  source pages.
* Sharing a writable guest page concurrently between nodes.
* Coherent external devices, host FDs, or external TCP connections. Existing
  distributed-fork resource policies still apply.
* Selecting one hard-coded transfer policy before workload measurements.
* Treating the shared filesystem as a coherent shared-memory transport.

## Correctness and transfer granularity

The unit of ownership and acknowledgement is a guest base page. The transfer
unit is an independently configurable, aligned range containing one or more
pages. A 2 MiB response therefore carries 512 individually accountable 4 KiB
pages; its acknowledgement contains a page bitmap or a run-length encoded set
and does not imply that unrelated pages were installed.

This separation permits large network operations without making reclamation
or correctness depend on large-page alignment. It also allows a partially
successful range transfer to be retried without retaining pages that the
target already owns.

## Core invariants

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

## Proposed architecture

### Control plane

The source `ttcd` coordinates a distributed-fork transaction with the target
`ttcd`:

1. Reserve a target container identity and transaction UUID.
2. Validate the source and target image/filesystem identities.
3. Pause and validate the source using the existing atomic fork path.
4. Create a `RemoteSnapshot` and stream the metadata clone to node B.
5. Install CoW protection and immutable page references before resuming A.
6. Construct B with a `RemotePageProvider` and publish it as paused or running.
7. Retire the transaction after every required page is acknowledged, or abort
   it and release all references if B is destroyed.

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

Configuration belongs to `ttcd` defaults, with safe per-fork overrides exposed
through the TTC API and SDKs. Initial knobs should include:

* transfer range: `4KiB`, `16KiB`, `64KiB`, `256KiB`, `1MiB`, or `2MiB`;
* prefetch: `none`, `sequential`, `fixed-window`, or `adaptive`;
* prefetch window and maximum outstanding bytes;
* push: `demand-only`, `dirty-orphan`, `memory-pressure`, or `eager-drain`;
* push/pull scheduling priority and fairness;
* request concurrency and transport connection count;
* ACK aggregation interval and maximum ACK batch;
* optional bandwidth cap; and
* adaptive-policy enablement, sampling interval, and hysteresis.

Configuration must be reported in fork statistics so every benchmark is
reproducible. Defaults are not finalized by this proposal.

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

1. Start at 64 KiB with sequential prefetch and dirty-orphan push.
2. Grow the range after sustained high prefetch usefulness and available
   bandwidth.
3. Shrink it after sustained low usefulness or high fault head-of-line delay.
4. Increase push aggressiveness when retained bytes grow faster than pages are
   acknowledged.
5. Switch to eager drain when the estimated remaining-demand transfer cost is
   close to the remaining snapshot size, or when source shutdown is requested.

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
* Stream kernel metadata separately from memory page data.
* Add configurable range sizes, request concurrency, ACK batching, and
  sequential/fixed-window prefetch.
* Expose metrics and a debug transaction dump.

Exit criteria: B starts and runs before full transfer; induced duplication,
reordering, corruption, disconnects, and reconnects produce either correct
execution or a cleanly failed target, never silent corruption.

### Phase 3: source-write notification and proactive push

* Add a post-CoW notification that identifies old snapshot pages after MM locks
  are released.
* Implement sharded bounded queues, range coalescing, dirty-orphan push, and
  batched installation acknowledgements.
* Release old source pages individually and validate the unique-page write
  fast path without eager PMA splitting or TLB work on ACK.
* Add memory-pressure and explicit eager-drain policies.

Exit criteria: the source write path contains no network wait; stress tests show
no deadlock under queue saturation; acknowledged obsolete pages are reclaimed;
and source application throughput regression stays within the performance
budget selected below.

### Phase 4: cross-node `ttcd` orchestration

* Add peer configuration, capability negotiation, image/rootfs identity checks,
  transaction lifecycle, authorization, quotas, and observability.
* Add `ttc fork --target` and equivalent SDK operations.
* Keep filesystem publication transactional on the common repository and make
  target cleanup idempotent.
* Add drain-before-source-stop and explicit dependent-target status.

Exit criteria: repeatable end-to-end forks work across two hosts, daemon restart
does not publish partial target containers, and cleanup leaves neither memory
references nor filesystem transaction debris.

### Phase 5: policy experimentation and adaptive selection

* Implement all policy knobs and record them with metrics.
* Run the network and workload matrix below.
* Select workload-independent conservative defaults.
* Implement the adaptive controller and compare it with the best fixed policy
  for every workload/network cell.

Exit criteria: adaptive mode avoids catastrophic choices, remains within 10%
of the best fixed policy on representative cells where one policy dominates,
and documents cases where manual configuration remains preferable. This is an
experimental target, not a correctness requirement.

### Phase 6: multiple children and advanced transports

* Generalize page need state to multiple remote consumers or a delegated page
  server.
* Evaluate RDMA reads/writes, io_uring, compression, deduplication, and target
  page eviction back to a snapshot owner.
* Add replication if target survival after source failure becomes a product
  requirement.

This phase starts only after the single-target protocol is stable.

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
