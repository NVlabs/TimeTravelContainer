# TTC userspace extent store

## Status

Concrete design and implementation plan. No storage-format or runtime changes
described here are implemented yet.

This proposal replaces TTC's eventual dependence on host `FICLONE`,
filesystem extent CoW, and whole-snapshot lifetime with a userspace-managed
page and extent store. It is intended to serve both:

* persistent rootfs and managed-volume regular-file contents; and
* durable, remotely readable, and demand-loaded container memory.

The existing inode, directory, branch-root, and metadata-journal model remains
useful. The principal filesystem change is replacing one opaque host file per
guest regular file with a logical page map. The principal memory change is
replacing one packed snapshot file per dehydrated branch with manifests and
shared page references.

## Decision summary

TTC should implement a logical database with these components, independently
of which physical storage engine is selected:

1. A small transactional metadata store containing immutable trees, branch
   roots, memory manifests, active loader leases, and backend placement records
   where applicable.
2. A persistent chunk backend storing 4 KiB pages and larger coalesced objects.
3. Stable `PageID` values that are independent of physical arena offsets.
4. Persistent CoW trees mapping logical file or memory ranges to runs of
   `PageID` values.
5. O(1) branch creation by sharing a tree root and advancing a branch data
   generation.
6. Concurrent or read-mostly lookup through immutable roots; a custom backend
   additionally uses atomically published page-directory entries.
7. Reclamation without requiring hole punching: either immutable-file rewrite
   and deletion in an embedded engine, or free-slot reuse in a custom arena.
8. If a custom arena is necessary, optional tail-fill compaction: move live
   tail pages into free lower slots, redirect their stable `PageID` entries,
   wait for old readers, and truncate the arena tail.
9. Optional hole punching only as an optimization. Correctness and bounded
   reuse must not depend on it.

The first prototype should use Pebble v2 behind a narrow backend interface.
Synthetic TTC-shaped qualification found it promising but also exposed
read/write tradeoffs and a potentially material per-database WAL-recycling
reserve. It is therefore a benchmark-qualified prototype candidate, not yet
the selected production storage format. A TTC-specific metadata checkpoint/WAL
plus data arenas remains the comparison and fallback. Large payloads should
not pass through a relational database's record and locking paths.

## Existing storage-engine survey

This problem overlaps embedded databases, but TTC adds several requirements
that ordinary key/value stores do not provide by themselves: a fork must copy
only a durable root, page identities must survive physical relocation, branch
roots must remain named after restart, and a runtime mapping must not pin an
embedded database transaction indefinitely.

The candidates investigated in August 2026 are:

| Candidate | Useful properties | Reason not to adopt as the complete design |
| --- | --- | --- |
| [Pebble v2](https://github.com/cockroachdb/pebble) | Maintained pure-Go engine; production use; concurrent reads and commit pipeline; synced batches, snapshots, WAL recovery, immutable files, compaction, and file deletion. Its v2 blob-file implementation can relocate live values while preserving logical blob handles. | TTC still needs its own immutable page maps, named roots, leases, and reachability. Value separation is exposed as an experimental policy and must be benchmarked separately from ordinary SST values. Values are read through Pebble rather than mapped directly into a guest. |
| [Dolt Noms Block Store](https://github.com/dolthub/dolt/tree/main/go/store/nbs) | The closest semantic match: a content-addressed DAG, root-hash commit, immutable table files, optimistic writers, mark-and-sweep GC, and remote-oriented storage. | The package has one store root, broad Dolt/cloud/archive dependencies, compression code that complicates TTC's static build, and whole-live-set GC. Extracting its local subset would effectively create another fork for TTC to maintain. Its design remains valuable prior art. |
| [Badger](https://github.com/dgraph-io/badger) | Mature pure-Go WiscKey engine with an LSM index, value log, concurrent ACID transactions, and segment rewriting. | Value-log reclamation is application-driven and can lag LSM version cleanup; prompt bounded shrink is difficult to guarantee. It also does not supply persistent branch roots. |
| [gosqlite.org/blobstore](https://pkg.go.dev/gosqlite.org/blobstore) | Fixed-size chunks, sparse objects, incremental reads/writes, reference-counted CoW clones, versions, and SQLite incremental vacuum. | It was first released only recently, SQLite permits one writer, every chunk is a SQL row, and clone copies every chunk mapping. A local 64 MiB/4 KiB-chunk clone-and-delete microbenchmark took about 1.56 seconds, so it cannot implement TTC's O(1) fork. |
| [libmdbx with mdbx-go](https://github.com/erigontech/mdbx-go) | Direct memory-mapped values, MVCC, CoW transactions, free-page reuse, configurable growth, and automatic tail truncation. | It is a C/cgo dependency, has one writer, and long-lived readers prevent retired-page reuse. Adopting it would compromise TTC's pure-Go static build and still would not provide durable branch roots. |
| [bbolt](https://github.com/etcd-io/bbolt) | Small, stable, pure-Go, mmap-backed transactional B+tree. | It has one writer and explicitly cannot truncate its database to return interior free pages to the host filesystem. |
| [go-storethehash](https://github.com/ipld/go-storethehash) | Append-oriented primary storage and an on-disk hash index aimed at block stores. | Its documented primary-store compaction/free-list work is incomplete, so TTC would inherit the unfinished part it is trying to avoid writing. |

### Prototype candidate: Pebble behind a TTC interface

Phase 0 may prototype Pebble v2 as the physical backend rather than import
its `sstable/blob` subpackage directly. The latter exposes Pebble-specific
object-storage and internal identifier types and is not an independent blob
database API.

TTC records remain explicit and versioned:

* `data/<PageID>` stores an immutable page or coalesced page run;
* `node/<NodeID>` stores an immutable page-map, inode-map, or directory node;
* `root/<DomainID>/<RootID>` names a committed root; and
* `state/...` stores allocator counters, leases, GC epochs, and format data.

A fork inserts one new root record pointing at the same immutable nodes. It
does not use a Pebble snapshot as the branch: Pebble snapshots are runtime read
views, whereas a TTC branch must be a small, durable named object that survives
daemon shutdown. A mutation allocates new data and nodes and atomically
publishes the new root in one synced batch. Unreachable immutable keys are
deleted after TTC's reachability and lease rules permit it; Pebble then
reclaims their physical space through compaction and deletion of obsolete
table or blob files.

The first synthetic qualification compared two Pebble modes:

1. ordinary 4 KiB values in SSTs, which uses only established Pebble paths;
2. v2 value separation with 4 KiB values in blob files, which may reduce data
   rewrite amplification but is still configured through Pebble's
   `Experimental.ValueSeparationPolicy`.

The results are recorded in
[`g3doc/benchmarks/ttc_pebble_qualification.md`](../benchmarks/ttc_pebble_qualification.md).
Ordinary SST values won the branch-shaped read-latency and steady-space tests;
experimental value separation reduced rewrite traffic and compaction time
under full overwrite churn. Neither result justifies shipping value separation
yet. Real TTC integration must still pass the crash, steady-state space,
write-amplification, page-fault, fork, deletion, and foreground-latency tests
below.

Keep the backend boundary small enough that the custom arena described in this
proposal can replace Pebble without changing branch metadata or guest-visible
semantics. Implement the custom allocator, WAL, relocation journal, and epoch
reclamation only if Pebble fails an explicit acceptance gate. This converts a
large storage-engine implementation into a measured fallback.

## Requirements

The store must:

* require only ordinary rootless regular-file operations: `openat`,
  `pread`/`pwrite`, `mmap`, `ftruncate`, `fdatasync`/`fsync`, and atomic rename;
* work without reflinks, sparse-file discovery, hole punching, OverlayFS,
  loop devices, FUSE, or special kernel modules;
* preserve 4 KiB correctness granularity even when metadata and I/O are
  coalesced into larger runs;
* fork a branch without scanning pages, inodes, or directory entries;
* copy only pages touched after a fork, rather than a complete regular file;
* share identical inherited pages across active and dehydrated branches;
* reclaim a stored page only after no persistent manifest, filesystem root,
  or active loader can read it;
* allow demand paging and large coalesced reads without a global read lock;
* preserve Linux `read`/`write`/`mmap` coherence and `fsync` semantics;
* recover to a structurally valid state after termination at every write and
  compaction boundary;
* bound metadata lookup cost independently of branch depth; and
* expose enough metrics to diagnose latency, contention, amplification, and
  fragmentation.

The first version need not deduplicate pages by content. CoW sharing follows
branch and snapshot relationships. Content hashing may be evaluated later,
but it must not delay writes or become a correctness dependency.

## Storage domains

There should not be one process-global database lock for the entire TTC data
root. The common implementation is reused by independent **storage domains**:

* one domain for a container family's memory;
* one domain for its private rootfs upper;
* one domain for each managed volume; and
* a cluster-stable domain after a repository is promoted for remote use.

Corruption, compaction, and writer contention are therefore contained within
one family or volume. A domain normally has one active writer Sentry. TTC
lifecycle operations serialize handoff of that mutation lease between the
Sentry and `ttcd`.

Remote branches may read immutable pages concurrently. Multi-node writers are
a later phase and receive disjoint allocator reservations and per-writer WAL
streams; they must not take one cross-node lock for every guest write.

## Logical and physical identity

The following identifiers are distinct:

```go
type DomainID [16]byte
type RootID uint64
type NodeID uint64
type PageID uint64
type ArenaShard uint8
type Slot uint64
```

`PageID` is stable until the page becomes unreachable and passes an epoch
grace period. It never encodes a host file offset. Pebble represents it in the
key of an immutable value. The fallback arena maps it to mutable physical
placement through a page-directory record:

```go
type PageLocation struct {
    Shard      ArenaShard
    Slot       Slot
    Generation uint32
    Flags      PageFlags
    Checksum   uint32
}
```

Directory entries are published as atomic pointers to immutable location
descriptors; the complete structure is not assumed to fit in one machine-word
atomic operation. Checksums are authoritative for sealed memory/snapshot pages
and immutable tree nodes. A private filesystem page that is writable through
`MAP_SHARED` is marked dirty and its checksum is refreshed when it is sealed
by fork, synchronization, or snapshot capture.

Logical trees and manifests contain `PageID`, not `Slot`. Moving data during
compaction therefore updates the page directory once; it does not rewrite
every branch, inode, memory manifest, or active loader that references the
page.

Page IDs are allocated monotonically within a domain epoch and are not reused
quickly. This prevents an old reader or replayed WAL record from observing an
unrelated page through an ABA collision.

## Fallback physical arenas

This section and **Tail-fill compaction** specify the custom backend retained
for use only if the Phase 0 Pebble qualification fails. They are not additional
layers to put beneath Pebble.

### Base layout

Each arena shard is an ordinary host file divided into 4 KiB slots:

```text
store-v3/<domain-id>/
  format
  superblock.0
  superblock.1
  checkpoint
  wal
  arena-00.data
  arena-01.data
  ...
  writer-lease
```

Four to sixteen shards should be benchmarked. Shards reduce allocator and I/O
contention while preserving long sequential runs. An allocation request for a
large logical range should reserve consecutive slots in one shard and publish
one run rather than thousands of unrelated records.

One shard is the simple monolithic-file configuration. Multiple arena files
remain one logical database and exist only to reduce lock contention and to
prevent one pinned high-offset page from blocking truncation of all other
storage. Phase 0 measurements, not the format API, choose the default.

The arena is not assumed to be sparse. A freed middle slot enters a reusable
free set. Reusing it avoids further file growth. Returning space to the host
uses tail compaction and `ftruncate`; hole punching is merely an optional
accelerator.

### Allocation granularity

Correctness and reference tracking use 4 KiB base pages. APIs accept ranges so
the implementation can:

* allocate consecutive runs;
* issue 64 KiB, 256 KiB, or 2 MiB `pread`/`pwrite` operations;
* store B+tree mappings as runs; and
* batch WAL, checksum, lease, and page-directory updates.

This avoids one lock acquisition and one metadata object per page on normal
sequential paths while retaining page-level CoW and reclamation.

### Free-space management

Each shard owns:

* a lock-free or lightly locked cache of recently freed slot runs;
* a sharded in-memory free-range tree for larger allocations;
* a high-water tail allocator; and
* a reserved emergency region for recovery and metadata progress under
  `ENOSPC`.

Small per-P allocator caches may serve single-page writes. Returning a range
to a cache must not make it physically reusable until all old readers have
left their read epoch.

Free-space state is reconstructible from the page directory and published
roots. Checkpoints accelerate startup but are not the sole source of truth.

## Tail-fill compaction

Moving tail pages into holes and truncating the old tail is the baseline way
to return disk space without hole punching.

For one batch, the compactor:

1. Selects free runs below a target high-water mark.
2. Selects the highest live, movable runs that fit those holes.
3. Pins their current page-directory generations.
4. Copies data from tail slots to lower free slots, coalescing adjacent pages
   into large I/O.
5. Verifies checksums and synchronizes the destination data as required.
6. Appends one relocation transaction containing
   `(PageID, old location, old generation, new location, new generation)` for
   every moved run.
7. Makes the transaction durable before publishing new directory entries.
8. Atomically publishes the new locations.
9. Waits for readers in the old storage epoch to finish.
10. Marks the old tail slots free and truncates every now-free suffix.

Pinning also changes the directory entry to a relocating state. A foreground
writer that resolves such an entry retries after relocation or explicitly
cancels the move before mutating the old slot. The compactor must never copy a
page concurrently with an in-place filesystem write.

A crash before the relocation commit leaves old locations authoritative. A
crash after it leaves new locations authoritative and old tail data as
collectible garbage. Recovery never has to guess which copy is valid.

Long-lived host mappings cannot silently follow a moved slot. Initially, the
online compactor skips pages pinned by active memory or filesystem mappings.
Later it may ask the relevant Sentry mappable to invalidate translations,
wait for the mapping epoch, and then move them. Fully dehydrated domains have
no runtime pins and are ideal compaction candidates.

Compaction is not placed on allocation or page-fault critical paths. It runs
when one of these policies requests it:

* an arena exceeds a configured high-water ratio;
* enough free space exists below live tail data to make shrinking worthwhile;
* the domain becomes fully dehydrated or otherwise idle;
* daemon shutdown explicitly requests compact dehydration; or
* an operator requests maintenance.

The policy compares bytes that can be truncated with bytes that must be moved.
A starting threshold should require at least two bytes of truncation benefit
per byte copied, except under disk pressure. Rate and I/O-depth limits prevent
compaction from interfering with active workloads.

Tail filling is efficient when holes and live tail runs can be coalesced. It is
poor for a nearly full arena containing many pinned tail pages. This is not a
correctness problem: middle holes remain reusable, so the arena stops growing
even if its high-water file size cannot immediately shrink.

## Persistent trees and branch roots

Immutable, bounded-fanout CoW B+trees map logical ranges to page runs. Separate
trees cover:

* virtual inode and directory metadata;
* each regular file's logical page map;
* a dehydrated memory file's logical page map;
* active loader unresolved ranges; and
* the page directory and allocator checkpoint.

Readers load an immutable root pointer once and traverse without taking the
writer mutex. Decoded upper tree levels and recent leaves are cached. Nodes
contain generation and checksum fields so torn or stale data fails closed.

Fork increments one root-node reference and publishes two new branch records.
It does not increment every page reference. When a writer first modifies a
shared tree path, it clones the bounded set of nodes on that path. New leaf
nodes acquire references to their page runs; retired nodes release theirs in
the background after an epoch grace period.

Reference counts are runtime acceleration, not unrecoverable truth. Recovery
can rebuild them by tracing published roots and durable loader leases. This
permits batched decrements and avoids synchronously walking a deleted branch.

## Data generations and mutation

Filesystem pages may be modified in place only while they are private to one
branch data generation and are not referenced by a memory snapshot or remote
lease. Every branch fork assigns distinct new generations to source and child
without walking files.

On a write to a page from an older generation:

1. lock the branch-local inode and affected logical range;
2. resolve the current page mapping;
3. allocate one new slot;
4. copy the old 4 KiB page, or synthesize zeroes for a hole;
5. apply the guest modification;
6. publish a new page-directory entry and file-map path transactionally; and
7. invalidate stale writable translations before making the page writable.

Untouched pages remain mapped to their old `PageID` values. A one-byte write
copies at most its 4 KiB page, subject to explicitly configured larger-page
policies measured later.

Before fork publishes shared filesystem roots, it revokes writable mappings
that could mutate old-generation pages directly. This is the page-level
equivalent of TTC's current whole-file CoW gate.

## Concurrency and lock hierarchy

The design avoids one repository mutex around ordinary I/O.

### Read path

The common read path is:

1. snapshot an immutable branch/file root;
2. traverse cached immutable nodes;
3. enter a storage read epoch;
4. resolve a `PageID` run through atomically published page-directory entries;
5. issue coalesced `pread` or map the selected arena range;
6. revalidate the location generation when necessary; and
7. leave the epoch.

Entering the epoch before resolving the physical location is mandatory. If a
reader resolved an old slot first, a concurrent compactor could otherwise see
no old-epoch reader, publish the move, truncate the tail, and only then have
the reader access the obsolete slot.

It takes no allocator, WAL, branch-wide, or compactor mutex.

### Write path

Independent writes should contend only on:

* the affected virtual inode/range lock;
* one allocator shard while reserving slots; and
* enqueueing a transaction to the WAL batcher.

The lock order is:

```text
family lifecycle
  -> filesystem/domain mutation lease
  -> virtual inodes in numeric order
  -> logical ranges in offset order
  -> tree path
  -> allocator shard
  -> WAL enqueue
```

The WAL writer is a multi-producer batching service, not a lock held while
copying guest bytes. Transactions reserve sequence numbers, prepare data and
nodes independently, then enqueue a compact commit record. `fsync` callers
wait for the containing durable batch; ordinary writes need not force a flush.

Metadata operations touching multiple inodes, such as rename and hard link,
use deterministic inode ordering. Compaction never takes inode locks while
holding the page-directory publication lock.

### Active loader path

Each memory file has its own sharded unresolved-range/bitmap state. Concurrent
faults reserve different ranges, coalesce adjacent storage reads, and install
pages independently. Completion acknowledgements are aggregated by bytes and
time before updating the durable lease. No family-global lock is taken per
page fault.

## Transactions and crash consistency

Every durable mutation follows data-before-reference ordering:

1. reserve new slots and IDs;
2. write page and immutable-node bytes;
3. append a checksummed WAL transaction describing new roots or directory
   locations;
4. make data durable before a synchronously durable reference when required;
5. commit and publish the new root/location atomically in memory; and
6. retire old nodes and slots only after their epoch and reference conditions
   are satisfied.

The WAL uses length-delimited, checksummed records and batch commit markers.
Recovery ignores an incomplete final batch. Periodic alternating superblocks
point to a complete checkpoint plus the WAL replay position.

Ordinary unsynchronized guest writes retain normal Linux crash semantics:
they are immediately visible to the running Sentry but are not promised after
a host crash. `fsync`, `fdatasync`, `msync(MS_SYNC)`, synchronous open flags,
dehydration completion, and durable remote publication wait for the necessary
data, nodes, WAL commit, and root checkpoint ordering.

The store reserves enough metadata space to abort or finish a transaction and
record free-space recovery after `ENOSPC`. A failure must never publish a root
that names unwritten data.

## Dehydrated memory

### Family-level manifests

A dehydrated branch stores small kernel metadata plus one logical memory
manifest per `MemoryFile`. Manifests map used logical offsets to shared
`PageID` runs. Pages belong to the storage domain, not to the branch; the
manifest merely references them.

Two dehydrated branches that still share a CoW page reference the same
`PageID`. A branch name and its UI parent are never part of page ownership.

### Rehydration

Rehydration atomically changes the durable consumer from a dehydrated manifest
to an active loader lease. Neither pages nor containing files move. The lease
initially references every unresolved page in the manifest.

As reads complete:

1. the page is installed into the target `MemoryFile`;
2. the in-memory unresolved bit is cleared;
3. cleared ranges are batched into a durable lease update; and
4. after that update commits, references used only by those ranges may be
   reclaimed.

If `ttcd` crashes while the Sentry continues, it reconstructs the live loader
state from the Sentry and reconciles it with the conservative durable lease.
Durable state may temporarily retain already-loaded pages but must never omit
an unresolved page.

### Re-dehydration without forced materialization

The current implementation drains all demand-loaded pages before dehydration.
The extent store should instead construct a new manifest from:

* still-unresolved ranges, which keep their existing `PageID` references;
* loaded pages known to remain identical to their origin, which may reuse
  those references; and
* dirty, newly allocated, or conservatively unknown pages, which are copied
  into new arena slots.

The first implementation may conservatively rewrite every loaded page while
reusing unresolved pages. A later write-protection/origin-tracking phase can
reuse clean loaded pages without weakening correctness.

### Sharing with active branches

Active gVisor `MemoryFile` pages already use page-granular CoW in RAM. The
storage exporter records stable bindings from captured immutable memory ranges
to `PageID` runs. A sibling dehydrating later can reuse a binding while the
captured range is still identical. When the final active reference disappears,
the arena copy remains reachable only through manifests and leases.

## Distributed fork

The shared extent domain replaces branch-specific packed handoff files:

1. The source captures immutable page ranges and kernel metadata.
2. An asynchronous exporter publishes those ranges into arena pages and a
   manifest.
3. The target may demand-read already published ranges while export continues,
   using the existing network protocol for unpublished ranges if configured.
4. Once the complete manifest is durable, the source process and node are no
   longer dependencies.
5. Target acknowledgements release active lease ranges; no source-side page
   push is required when the common store is reachable.

Cluster control messages carry domain, manifest, and epoch identities rather
than host paths. A target never trusts a peer-provided absolute path or slot.

Writers on different nodes initially allocate from disjoint shard/range leases
and publish independent roots. Compaction and page-directory checkpointing are
coordinator operations. Lease expiry must be fenced by a monotonically
increasing writer epoch so a delayed node cannot publish into reassigned
space.

## Filesystem regular-file integration

The existing virtual inode and directory-entry model remains. A regular inode
changes from:

```text
inode -> one opaque host object file
```

to:

```text
inode -> file data root -> logical page runs -> PageID runs
```

### Buffered system calls

`read`, `pread`, and copy sources resolve ranges and issue coalesced arena
reads. Holes return zeroes without storage I/O.

`write`, `pwrite`, append, and copy destinations privatize only touched pages.
Unaligned writes read/copy the two boundary pages as needed. Complete aligned
pages are written directly into newly allocated runs. Sequential writes batch
root and WAL updates.

Truncate down removes complete mappings and creates one private zero-tailed
boundary page if necessary. Truncate up adds logical holes. Hole-punch and
zero-range fallocate modes update mappings rather than requiring host
filesystem hole support.

### `mmap`

The current TTC disk-volume implementation maps one complete host object FD.
That shortcut must be replaced with a mappable that translates guest file
offsets to arena file offsets page by page or run by run.

For a writable `MAP_SHARED` fault:

1. resolve the branch's current inode and page mapping;
2. ensure the page belongs to the current branch data generation;
3. allocate and publish a private page when it is shared;
4. mark it dirty for synchronization accounting; and
5. return a translation of the arena shard and physical offset.

Subsequent CPU writes may mutate that private page through host page cache.
Before fork advances generations, TTC invalidates writable translations. A
later write fault in either branch passes through the CoW gate again.

`MAP_PRIVATE` continues to use process-memory CoW and must not update the file
map. `read`/`write` and shared mappings use the same arena bytes, preserving
page-cache coherence.

Mapped pages pin their current locations. Initial compaction skips them;
online relocation is introduced only after mapping invalidation and epoch
tests are complete.

### Durability

Arena mappings give ordinary guest writes the same broad behavior as ordinary
host page-cache writes: bytes are present in the backing arena immediately but
may not yet be durable on storage. File `fsync` synchronizes dirty arena ranges,
the file-map nodes, inode metadata, and the root/WAL commit needed to find them
after restart.

This preserves TTC's direct-persistence goal without keeping whole guest files
as host files.

## Garbage collection and accounting

The collector starts from:

* published filesystem branch roots;
* dehydrated memory manifests;
* durable active-loader leases;
* in-progress committed transactions;
* checkpoint/export roots; and
* explicitly pinned runtime mappings.

Tree-node reference counts make common reclamation incremental. A tracing
collector periodically verifies counts, recovers orphan allocations, and
rebuilds free sets. Branch deletion removes one root reference and queues
recursive node release; it does not synchronously walk the entire filesystem
or memory image.

Report at least:

* logical bytes per branch;
* unique physical live bytes per domain;
* shared referenced bytes;
* arena high-water bytes;
* reusable free bytes;
* reclaimable-but-epoch-pinned bytes;
* mapped/pinned bytes;
* fragmentation ratio; and
* compaction bytes read, written, and truncated.

Logical quotas are enforced at mapping publication. Physical quotas reserve
arena slots before a transaction begins.

## Security properties

Guest names never become host paths. Arena and metadata files are opened
relative to a donated domain directory. Guest-controlled values are bounded
logical inode numbers, offsets, and lengths validated before arithmetic.

The page directory is never guest-visible. A peer supplies authenticated
logical IDs; the receiving `ttcd` resolves them inside the expected domain and
epoch. Checksums detect stale locations, torn writes, and accidental
cross-page reads.

Compaction validates that the page ID, old generation, and old location still
match before publishing a move. Writer leases and epochs prevent stale Sentry
or cluster writers from modifying reassigned slots.

## Failure handling

The implementation must define and test:

* `ENOSPC` while reserving data, tree nodes, WAL, and compaction destinations;
* short/torn page and metadata writes;
* checksum failure on read;
* daemon death with a live Sentry and conservative loader lease;
* Sentry death after visible but unsynchronized writes;
* process death during fork-root publication;
* compactor death before copy, after copy, after WAL commit, after publication,
  during epoch wait, and after truncation;
* stale writer lease or cluster epoch;
* missing/corrupt checkpoint with recoverable WAL; and
* corrupt roots that reference missing pages or invalid slots.

Structural corruption fails closed for the affected domain. Recovery may
retain unreachable data, but it must not free reachable data or make one
branch observe another branch's private write.

## Implementation plan

### Phase 0: model and measurements

* Add a pure-Go reference model for roots, page IDs, locations, generations,
  leases, relocation, and crash recovery.
* Replay randomized fork/write/delete/load/relocate schedules against it.
* Instrument current TTC workloads to collect page-write sizes, mmap ratios,
  dirty pages at fork, dehydration reuse, and expected compaction benefit.
* Define a small `ChunkBackend` contract for immutable put/get/delete, synced
  batch publication, iteration, metrics, and explicit close/reopen.
* Implement a Pebble v2 spike using ordinary 4 KiB SST values.
* A/B it with Pebble v2 value separation, including forced garbage and blob
  rewrites, without making the experimental policy part of TTC's format.
* Benchmark a minimal custom arena only as the performance/control baseline,
  including 1, 4, 8, and 16 shards and representative B+tree node sizes.
* Measure binary size, build dependencies, startup/recovery time, steady-state
  disk amplification, disk convergence after deletion, random page latency,
  sequential throughput, write amplification, fsync batching, and p99 latency
  during compaction.

Exit criterion: invariants survive randomized crash points and one backend is
selected by recorded acceptance gates rather than architectural resemblance.

### Phase 1A: standalone logical engine on Pebble

* Implement immutable page-map nodes, branch roots, memory manifests, leases,
  data generations, and reachability above `ChunkBackend`.
* Commit new immutable records and the replacement named root in one synced
  Pebble batch.
* Add a background reclaimer that deletes unreachable records only after TTC
  leases and epochs permit it, then lets Pebble compact obsolete files.
* Repeat all crash injection, fork latency, churn, and disk-convergence tests
  with realistic family sizes.

Exit criterion: a standalone stress test runs billions of modeled operations,
reopens after injected crashes, always reproduces committed roots, and meets
the latency and amplification gates.

### Phase 1B: custom backend, only if Phase 1A fails

* Implement domain creation, dual superblocks, WAL, checkpoint, arena shards,
  stable page directory, free-range allocator, epochs, and checksums.
* Implement allocate/read/write/reference/release APIs over page runs.
* Implement root trees and O(1) root clone/delete.
* Implement offline tail-fill compaction and recovery from every relocation
  boundary.

Exit criterion: the same Phase 1A suite passes and the measured failure that
required replacing Pebble is eliminated without regressing the other gates.

### Phase 2: dehydration page store

* Export durable memory into extent manifests instead of packed files.
* Rehydrate through a backend-backed asynchronous reader and active lease.
* Batch page-consumption updates and reclaim pages after durable release.
* Re-dehydrate by reusing unresolved ranges without draining them first.
* Retain the existing packed format only behind a temporary development flag.

Exit criterion: repeated dehydrate/rehydrate cycles do not amplify memory or
disk usage when few pages change, and every branch can become process-free.

### Phase 3: distributed memory integration

* Publish shared-domain manifests to remote nodes.
* Combine partial chunk publication with the existing network reader where
  useful.
* Add writer fencing, disjoint allocator reservations, and remote lease
  recovery.
* Remove the shared-filesystem reflink assumption from the remote protocol.

Exit criterion: source deletion and source-node shutdown are safe after
manifest commit, and remote demand reads require no branch-specific snapshot
file.

### Phase 4: filesystem buffered I/O

* Add a new regular-file data-root field while retaining virtual inode and
  directory metadata.
* Implement read/write/truncate/fallocate/fsync against logical page maps.
* Preserve hard links, open-unlinked files, sparse files, and branch data
  generations.
* Keep the old host-object driver selectable for differential testing.

Exit criterion: syscall and application suites pass without mmap, and first
write cost is proportional to touched pages rather than guest file size.

### Phase 5: filesystem mappings and live fork

* Implement backing-range translations and pin tracking. With Pebble this may
  require a TTC page-cache file or memory object rather than exposing Pebble's
  internal table layout; with the fallback backend it uses arena ranges.
* Cover shared/private mappings, dirty synchronization, truncate races, and
  write-fault CoW.
* Integrate writable-translation invalidation into the atomic family fork.
* Add offline and then carefully synchronized online relocation of mapped
  pages.

Exit criterion: database-style mmap workloads remain coherent across fork,
and no writable translation can bypass page privatization.

### Phase 6: rootfs and volume cutover

* Move rootfs upper and managed volumes to the extent driver.
* Run old/new differential tests and crash tests against identical traces.
* Remove the daemon reflink probe, whole-file fallback, and
  `--allow-slow-volume-copy` after no caller uses host objects.
* Update user documentation only when the new format becomes the default.

This is a pre-release storage-format replacement, not a promise of automatic
migration from the development object format.

### Phase 7: optimization

* Tune run coalescing, cache sizes, WAL group commit, allocator shards, and
  compaction thresholds.
* Add clean-origin tracking to reuse loaded but unmodified dehydrated pages.
* Add optional compression or content dedup only if measurements justify the
  CPU and locking cost.
* Evaluate online compaction under mapped workloads and keep it disabled when
  it harms tail latency.

## Test plan

### Correctness and model tests

* Property-test fork isolation, generation changes, tree sharing, page
  reclamation, and PageID relocation.
* Exhaustively enumerate small pull/load/release/relocate races.
* Verify a page is never recycled while an old epoch reader or mapping can
  access it.
* Compare every optimized operation with a simple map-of-pages reference
  implementation.

### Crash and storage tests

* Inject termination after every data, WAL, root, checkpoint, relocation, and
  truncation step.
* Inject torn final records, stale superblocks, checksum failures, and disk
  full at every allocation class.
* Reopen after each failure and verify every committed branch byte-for-byte.
* Run on ext4, XFS, Btrfs, tmpfs, NFS, and a plain loopback filesystem while
  using only the baseline operations.

### Filesystem compatibility

* Exercise read/write variants, append, sparse files, truncate, all supported
  fallocate modes, hard links, rename exchange, unlink-open, fsync/syncfs, and
  shared/private mmap.
* Run filebench, fsx, SQLite, LMDB, Git, package managers, compiler/linker
  workloads, browser profiles, and the existing TTC image matrix.
* Fork during concurrent buffered I/O and mmap writes and compare source and
  child against Linux-visible expectations.

### Memory and lifecycle

* Fork, dehydrate, rehydrate, fork again, remove arbitrary branches, and repeat
  for thousands of generations.
* Verify process-free state, tmpfs, exec sessions, shared memory, and anonymous
  memory under partial demand load.
* Kill and restart `ttcd` while Sentries and remote loaders remain live.
* Verify disk use converges after branch deletion and compaction.

### Concurrency

* Race readers with relocation publication and epoch retirement.
* Race first writers in source and child branches.
* Race loader reads, proactive network delivery, lease batching, dehydration,
  removal, and compaction.
* Run Go race tests plus stress tests with forced scheduler yields at lock and
  publication boundaries.

## Performance qualification

Measure distributions, not only averages:

* live-fork source pause and total fork latency;
* first-write latency versus touched bytes and total file size;
* sequential and random read/write throughput;
* mmap fault and dirty-fault latency;
* page-loader demand latency and achievable queue depth;
* locks contended and time blocked per operation;
* WAL batch size, flush latency, and write amplification;
* tree-node and page-directory cache hit ratios;
* metadata bytes per GiB and per million mappings;
* dehydration bytes newly written versus reused;
* compaction bytes moved per byte truncated; and
* foreground p50/p95/p99 latency during compaction.

Initial targets are:

* no global mutex acquisition on cached reads;
* O(1) branch-root fork independent of file and page counts;
* one 4 KiB data copy for an isolated one-byte first write;
* sequential I/O coalesced to at least 256 KiB where the guest request stream
  permits it;
* metadata overhead below 1% of uniquely stored page bytes for large dense
  workloads;
* no unbounded backend growth under a steady allocate/delete workload; and
* configurable compaction that can be disabled with no correctness impact.

Absolute throughput and latency gates should be set only after Phase 0 records
the current host-object, Docker, and native baselines on the same machines.

## Acceptance criteria

The replacement is ready to become TTC's default only when:

* neither daemon startup nor any branch operation probes or requires reflink;
* a one-page modification of a multi-gigabyte inherited file copies no other
  file pages;
* dehydrated and active branches share storage without assigning page
  ownership to one branch;
* loaded memory pages are released incrementally from durable leases;
* deleted chunks are physically reclaimed and disk use converges using only
  ordinary file creation, writing, renaming, deletion, and truncation; for the
  fallback backend, free middle slots are reused immediately and tail-fill
  compaction safely reduces arena files using only copy and truncate;
* ordinary reads have no domain-global lock;
* filesystem mmap coherence and fork isolation pass stress testing;
* every injected crash recovers a valid committed root set; and
* rootfs, managed volumes, dehydration, and remote fork all pass their full
  compatibility and performance suites.
