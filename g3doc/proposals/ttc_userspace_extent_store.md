# TTC userspace extent store

## Status

Design exploration. No storage-format or runtime changes described here are
implemented yet. The unified mutable-filesystem store is postponed. The active
first scope is a simple immutable 1 MiB-file backend for dehydrated and remotely
forked memory. Pebble is not part of the implementation plan.

This proposal evaluates replacing TTC's eventual dependence on host `FICLONE`,
filesystem extent CoW, and whole-snapshot lifetime with a userspace-managed
page and extent store for:

* persistent rootfs and managed-volume regular-file contents; and
* durable, remotely readable, and demand-loaded container memory.

The existing inode, directory, branch-root, and metadata-journal model remains
useful. However, efficiently implementing mutable filesystem semantics over a
general blob database has enough lookup, partial-write, reclamation, and crash
consistency costs that TTC will not attempt it in the next implementation
phase. Rootfs and volume storage retain their current design while benchmarks
and profiles identify a concrete need for replacement.

## Decision summary

The following is the deferred unified design, independently of which physical
storage engine is selected:

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

The first prototype is deliberately narrower: store each immutable 1 MiB
dehydrated-memory transfer unit in an ordinary file in the cluster's shared TTC
data root. Each chunk contains up to 256 consecutive 4 KiB pages from the
packed memory stream. Put at most 1,024 chunks, or 1 GiB of dense payload, in
each shard directory; retain directory descriptors; and use
`openat`/`unlinkat` on the demand path.

A successful load reads the complete chunk, populates up to 256 independently
CoW 4 KiB memory pages, and releases that manifest's reference. The chunk is
unlinked only after no dehydrated manifest or active loader lease references
it. Chunk files are immutable and addressable by stable IDs, so another `ttcd`
on the shared filesystem can load them without a source-process dependency.
The existing packed-file backend remains only as a compatibility path while
the chunk format is integrated. It is not the target design even on filesystems
that support reflinks and hole punching.

Pebble experiments remain historical qualification only; no Pebble code or
dependency is added to TTC now. Pebble is also not approved for rootfs or
managed-volume contents. In particular, representing a mutable 64 KiB extent
as one Pebble value retained only 0.39% of raw-file unsynced random 4 KiB write
throughput in the synthetic benchmark. A TTC-specific metadata checkpoint/WAL
plus data arenas remains a future comparison if the filesystem store is
revisited.

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

### Selected memory hydration design

Phase 0 implements only loose 1 MiB chunk files. Pebble remains benchmarked
prior art and is not included as an A/B runtime backend in this phase.

Snapshot contents are written as immutable chunks before an immutable manifest
is published. Interrupted unpublished construction can be removed as a unit.
Large snapshots use bounded shard directories rather than one unbounded
directory. Reads and unlinks operate relative to retained directory
descriptors, and directory synchronization batches consumption records.

The first implementation chunks every referenced range from every
`MemoryFile` captured by the existing dehydration/remote-fork traversal,
including application anonymous memory and private resource `MemoryFile`s.
Kernel object graphs remain in the existing metadata stream. Reconstructible
clean caches may be omitted only by a later explicit optimization that proves
the backing object and identity are durable; the initial conversion must not
silently narrow the current packed snapshot's coverage.

“Referenced” is branch-specific, not equivalent to every allocated page in a
family-wide `MemoryFile`. The existing
`ExportMemoryFileMetadataForRemote` baseline walks allocator occupancy, so it
also exports pages reachable only from sibling Kernels. That is not an
authoritative source for sparse manifests. During state capture, every saved
PMA and kernel object that owns a `MemoryFile` range registers a binding:

```go
type CapturedPageBinding struct {
	ResourceID checkpoint.ResourceID
	OldOffset uint64
	Page      LogicalPageID
	Version   PageVersionID
	Chunk     ChunkID
	Slot      uint8
}
```

The immutable binding-map root is captured with the Kernel; background code
may then group its referenced versions into chunk files. Restore first merges
page versions into the node-local canonical store, builds an old-offset to
target-offset relocation map, and applies that map to every saved reference.
Allocator free-space/refcount metadata is reconstructed for the imported
branch rather than copied wholesale from the source Sentry.

One branch can legitimately retain two versions of the same logical page, for
example when aliases diverge through private CoW. Consequently a chunk file is
a transport/storage bundle for one page-version vector, not the sole value of
a logical coordinate. A branch binding names the exact `(Page, Version,
Chunk, Slot)` it requires; several chunk objects may cover the same logical
coordinate. Node-local canonicalization still collapses equal page versions.

One chunk contains as many as 256 stable logical 4 KiB page slots, not
necessarily one contiguous guest virtual-address interval. Its manifest entry
records the logical coordinate, valid-slot bitmap, immutable content version,
and checksum, plus scatter/gather mappings back to `MemoryFile` ranges. Loading
reads the whole chunk and atomically installs every valid page. Thus 1 MiB is
the persistence, shared-filesystem, network-fetch, and residency unit; memory
CoW and correctness remain 4 KiB.

The 1 MiB choice deliberately trades as much as 256-page read and residency
amplification on a sparse access for 16 times fewer host files and metadata
operations than the measured 64 KiB layout. A 16 GiB dense snapshot uses
16,384 chunk files rather than 262,144. This tradeoff must be validated with
real lazy-rehydration and shared-filesystem workloads before the format is
declared stable.

The shared repository uses a family-level namespace; chunks never belong to a
particular branch:

```text
memory-v2/<family-id>/
  chunks/<shard>/<chunk-id>.chunk
  manifests/<manifest-id>.manifest
  branches/<branch-id>
  leases/<node-id>/<loader-id>
  transactions/<transaction-id>/
```

`branches/<branch-id>` is a small atomic pointer to an immutable manifest, not
a parent/child lineage record. Manifests are the authoritative chunk roots.
Loader leases protect chunks that a host may still read while a branch pointer
is being consumed or replaced. Family GC traces all manifests and live or
conservatively recovered leases; it does not rely on a chunk being owned by one
branch or on an in-memory reference count surviving daemon failure.

Rehydration atomically replaces the dehydrated branch pointer with a loader
lease containing the manifest ID and a remaining-chunk bitmap. After all pages
in a chunk are installed, the loader records that clear and synchronizes lease
updates in bounded batches; until the clear is durable, GC conservatively keeps
the chunk. A crash before the clear merely retains garbage and never loses
required data.
Redehydration reuses every still-unconsumed chunk and writes new chunks for the
resident state, then publishes a new complete immutable manifest. This keeps
consumption and reuse at the same 1 MiB boundary without assigning chunks to a
branch.

`chunk-id` is SHA-256 over a format-domain separator, valid payload length, and
payload bytes; the first digest bytes select the shard. The digest provides
idempotent multi-host publication, integrity checking, and opportunistic
deduplication without a mutable chunk index. Hashing cost and contention remain
part of the qualification benchmark.

Publication order is data before references:

1. Write each chunk to a transaction-local temporary name on the shared
   filesystem, verify its length and checksum, and synchronize it.
2. Atomically rename it into the immutable chunk namespace and synchronize the
   shard directory. An existing identical ID is verified and reused; a
   collision with different contents fails closed.
3. Write and synchronize the immutable manifest only after every referenced
   chunk is visible.
4. Atomically publish or replace the branch pointer and synchronize its
   directory.
5. Mark the transaction complete. Recovery removes unpublished temporary data
   and retains any chunk reachable from a manifest or lease.

The required shared-filesystem contract is ordinary immutable-file behavior,
atomic same-filesystem rename, durable file and directory synchronization, and
coherent visibility after publication. TTC does not require reflink, hole
punching, host OverlayFS, or a userspace database. NFS and other candidate
filesystems must pass publication, cache-coherence, node-crash, and concurrent
GC tests before being advertised as supported.

### Chunk coordinates and alignment

Capture-specific packed-stream offsets are not stable chunk identities. They
remain useful wire offsets in the current implementation, but inserting a new
used page before a later capture can shift subsequent offsets and destroy chunk
reuse. The replacement format adds stable logical allocation identities:

```go
type LogicalChunkID [16]byte

type LogicalPageID struct {
	Chunk LogicalChunkID
	Slot  uint8 // 0..255
}

type PageVersionID [16]byte

type ChunkCoordinate struct {
	MemoryLineageID [16]byte
	Chunk           LogicalChunkID
}
```

`MemoryLineageID` identifies a logical `MemoryFile` lineage across local forks,
remote metadata clones, dehydration, and rehydration. `LogicalChunkID` names a
stable group of 256 logical page slots. The immutable shared file is a
**version** of that coordinate; its content-addressed `ChunkID` changes when
any slot's bytes change. `PageVersionID` distinguishes CoW incarnations of one
logical page. It is preserved by fork and changes when a write must split a
shared or snapshot-pinned page; a mutable page is sealed before publication.

New pages receive monotonically allocated slots from a process-local open
logical chunk; nodes use disjoint ID namespaces, so this requires no cross-node
allocator. Local branches in one Sentry may share the small allocator lock and
continue filling the same open chunk. Publication captures an immutable
`(valid-slot bitmap, page-version vector)` without sealing the logical
coordinate: later allocations may use previously invalid slots but create a
different immutable content version. An already published version never
changes, and logical page IDs are not reused within a family lifetime. A 4 KiB
CoW write likewise creates a new content version for the same `LogicalPageID`;
it does not change the logical chunk coordinate.

Consequently, growing a stack downward by one page does not insert a page into
an address-sorted stream and does not renumber anything. The new stack page
gets the next process-local open slot, or starts one new chunk if the previous
one is full or no open chunk exists. It does not consume a whole 1 MiB object:
the next 255 page allocations can fill the remaining slots. Only versions that
include the new logical page differ. Its guest virtual address and growth
direction are irrelevant.

Each manifest maps logical page IDs to the guest/kernel objects and current
`MemoryFile` offsets that use them. A shared chunk object carries a 256-bit
valid-slot bitmap, a 256-bit zero-slot bitmap, and one page-aligned payload slot
for each valid nonzero logical page. The first format is uncompressed and
records the base page size, which is 4 KiB for its initial supported format. It
hashes the format domain, page size, both bitmaps, and payload bytes in slot
order; restore rejects a page-size or architecture mismatch.
The all-resident/all-non-resident invariant applies to all slots valid in that
chunk version; invalid slots require no backing page. The manifest mapping
supplies the scatter/gather destinations needed to install those pages.

Adding stable logical page IDs is new implementation work. The current remote
format instead assigns packed stream offsets at capture time. The old format
can be read during development, but it cannot be used as the persistent
identity scheme for the shared-chunk design.

The same fixed 256-slot, at-most-1-MiB unit is used for shared chunk files and
live network fetches. One RPC may pipeline requests and responses for several
chunks, but one chunk version is the indivisible request, validation,
installation, acknowledgement, and reclamation unit.

For two remote forks taken from one evolving source, the manifests share every
unchanged `(ChunkCoordinate, ChunkID)` pair. A 4 KiB write preserves the
coordinate but gives the affected chunk contents a new `ChunkID`; the older
remote child retains the old version and the newer child names the new one. A
new anonymous page occupies a previously invalid logical slot or a new logical
chunk, so only that chunk version and the mapping metadata differ. Changes such
as `mprotect`, unmapping an alias, or mapping another alias to an existing page
can change branch mapping metadata without changing any immutable chunk bytes.
If a modified chunk later has exactly the same valid bitmap and bytes as an
existing version, content addressing permits reuse of that version.

### Normative local state

State is represented with orthogonal enums rather than one product enum with
dozens of combinations:

```go
type ChunkResidency uint8

const (
	ChunkNonResident ChunkResidency = iota
	ChunkLoading
	ChunkResident
	ChunkEvicting
)

type ChunkOrigin uint8

const (
	OriginNone ChunkOrigin = iota       // Resident and no reusable backing.
	OriginLive                           // Fork-time pages on another process/node.
	OriginShared                         // Immutable shared-filesystem chunk.
	OriginDual                           // Both origins contain identical bytes.
)

type ChunkPromotion uint8

const (
	PromotionIdle ChunkPromotion = iota
	PromotionQueued
	PromotionCapturing
	PromotionWriting
	PromotionDataDurable
	PromotionDescriptorDurable
	PromotionRetiringLive
	PromotionComplete
	PromotionRetryable
)
```

Each branch/node chunk slot contains at least:

```go
type BranchChunkSlot struct {
	Residency       ChunkResidency
	Origin          ChunkOrigin
	Live            *LiveChunkRef
	SharedChunkID   ChunkID
	Generation      uint64
	ValidPages      uint16 // 1..256
	DirtyPages      [4]uint64
	LoadTransaction LoadID
}
```

`DirtyPages` is meaningful only while resident and is relative to
`SharedChunkID`, if any. `ChunkResident` means all `ValidPages` are installed;
individual writes may then CoW and dirty any subset at 4 KiB granularity.
`ChunkNonResident` means none is installed. `ChunkLoading` and `ChunkEvicting`
are private transactional states: guest execution cannot observe a partially
loaded or partially removed chunk. The branch mutation lock prevents fork from
racing an eviction transition. Fork does not wait for network/disk loading: it
atomically adds the child binding to the same immutable load transaction.
Dehydration must settle loading before it can release the runtime.

Here `resident` means installed in TTC's node-local `MemoryFile` and usable
without consulting a live/shared origin. It does not promise that the host
kernel has the bytes physically present in DRAM; ordinary host swapping remains
below TTC and does not change this state machine.

The permitted steady-state combinations are:

| Residency | Origin | Meaning |
| --- | --- | --- |
| non-resident | live | Fetch the complete fork-time chunk from a source snapshot. |
| non-resident | shared | Fetch the complete immutable file from the shared root. |
| non-resident | dual | Either source is valid; one successful installation wins. |
| resident | none | Current pages have no reusable complete shared backing. |
| resident | shared | `DirtyPages == 0`; the shared chunk exactly matches memory. |
| resident | live/dual | Temporary during remote reconstruction or handoff. |

No operation may leave one valid page resident and another non-resident in the
same branch/node chunk slot.

### Node-local sharing of a chunk version

Residency belongs to one immutable chunk **version on a node**, not separately
to every branch. Branch views bind their logical chunk coordinates to canonical
node-local instances:

```go
type NodeChunkKey struct {
	Coordinate ChunkCoordinate
	Version    ChunkID
}

type NodeChunkInstance struct {
	Key       NodeChunkKey
	Residency ChunkResidency
	Pages     [256]PageVersionRef
	Bindings  uint32
	Load      LoadID
}

type BranchChunkBinding struct {
	Coordinate ChunkCoordinate
	Base       *NodeChunkInstance
	Current    [256]PageVersionRef
	DirtyPages [4]uint64
}
```

These types are schematic; durable manifests need not contain Go pointers.
The normative rules are:

1. A node canonicalizes `(coordinate, version)` before starting a load. All
   local branch bindings that still name that exact version join the same load.
2. `loading -> resident` installs all valid pages in the canonical instance in
   one transaction. Every joined binding becomes usable at the same `L4`.
3. A local fork shares the binding's current 4 KiB page-version references. It
   does not create another 1 MiB allocation or loader.
4. A write replaces only the writer's `Current[slot]` through ordinary 4 KiB
   CoW and sets its dirty bit. Siblings continue to reference the canonical
   page version. The immutable base instance is never modified.
5. An instance can be evicted only when no resident binding or in-flight I/O
   needs its pages. One branch becoming dirty does not evict clean base pages
   still used by another branch.
6. Different chunk versions are canonicalized again by per-slot
   `(LogicalPageID, PageVersionID)`. If only one page changed, installing the
   newer chunk aliases the other 255 resident page references rather than
   allocating another 1 MiB. The chunk transaction remains all-or-nothing even
   when most slots are satisfied from the local canonical-page table.

For example, after `A.1 -> B.2` and a local `B.2 -> B.3`, both B branches bind
to the same target `MemoryFile` ranges and node chunk instance. If either
branch faults first, one network/NFS read installs the complete chunk for both.
A later write by B.2 CoWs only the affected 4 KiB page; B.3 keeps the fork-time
page.

The current TTC implementation already supplies the core mechanism for this
specific same-Sentry case: `MemoryManager.Fork` shares its `MemoryFile`, the
remote async loader is attached once to that `MemoryFile`, and private PMAs are
made CoW by local fork. Current demand loading can still install subranges, so
the canonical `NodeChunkInstance` and atomic 1 MiB residency transaction are
new work. Branches restored into separate Sentry processes do not currently
share resident host pages merely because they reference the same NFS chunk;
they each need a load unless a later cross-Sentry shared-page cache is added.

The distributed implementation MUST maintain one active Sentry per
`(global family ID, node ID)` and import every compatible branch of that family
on the node into it. Local fork, peer rehydration, and repeated remote forks to
the same node therefore use the same `MemoryFile` and canonical chunk/page
tables. The first branch creates the node-local family runtime; later branches
are imported through its control server without spawning another Sentry.
Incompatible platform or family configuration is rejected rather than silently
creating a second runtime. Unrelated global families retain separate Sentries.
The current remote CLI's new-target-family behavior must be replaced as part of
this work; a cross-Sentry page cache is not part of the first implementation.

### Forking a branch with an upstream live origin

A remote branch may be forked again before it has faulted all pages or before
its source snapshot has been promoted to shared storage. This does not require
materializing the branch first:

* every live origin has a cluster-stable `OriginLeaseID` naming an immutable
  chunk version and its provider;
* a non-resident clean binding delegates that lease to the new consumer;
* a resident binding is complete by invariant, so the current node can retain
  and serve its exact fork-time page versions; and
* a binding in `loading` is still identical to its immutable origin until its
  atomic install. Fork joins or delegates the origin transaction without
  exposing the staging pages.

The target daemon registers the descendant as a consumer with the original
provider when reachable, avoiding an A-to-B-to-C data relay. Delegation and
consumer registration are idempotent and occur before the new branch is
published. By default, if the provider cannot be contacted and no shared
version exists, the fork fails without publishing the child. An explicit slow
policy may first materialize the required chunks and retry; it is never an
implicit latency surprise. A published branch whose last provider is later
lost before durable promotion is stopped and marked failed when it next cannot
resolve the dependency; TTC never substitutes zeroes or exposes partial chunk
installation.

### Normative 4 KiB page-version state

A page version may simultaneously belong to local branches, fork snapshots,
and active I/O, so one exclusive owner enum would be misleading. Its state is
derived from three process-local reference classes:

```go
type PageVersionRefs struct {
	BranchRefs   uint32 // PMAs/mappings for live local branches.
	SnapshotRefs uint32 // Fork-time snapshots for remote consumers.
	IORefs       uint32 // Network responses and chunk publishers in flight.
}
```

| Derived page state | Predicate | Meaning |
| --- | --- | --- |
| live unique | `BranchRefs == 1 && SnapshotRefs == 0` | A write may use the ordinary unique-page fast path. |
| live shared | `BranchRefs > 1 && SnapshotRefs == 0` | A local branch write performs 4 KiB CoW. |
| live and snapshot-pinned | `BranchRefs > 0 && SnapshotRefs > 0` | A source write creates a new page version; the old version remains exact for remote consumers. |
| snapshot-only | `BranchRefs == 0 && SnapshotRefs > 0` | No local branch uses the old version; promotion or remote acknowledgement can release it. |
| I/O-only | `BranchRefs == 0 && SnapshotRefs == 0 && IORefs > 0` | A transfer already acquired it; final release waits for that operation. |
| reclaimable | all three are zero | The `MemoryFile` may free the 4 KiB version. |

On a source write, replacement of the branch's page reference and increment of
the new page's reference are the 4 KiB CoW linearization point. The old version
may become snapshot-only, but the containing `BranchChunkSlot` remains fully
resident. After memory locks are released, the dirty bit is set for the source
branch's chunk and the old snapshot chunk is queued for promotion. No single
page is independently persisted or acknowledged: the local reference stays
page-granular, while all cross-process actions group the 256 version references
under the chunk coordinate.

### Normative remote-consumer and object state

A live fork snapshot groups the process-local 4 KiB page-version references by
stable logical chunk coordinate:

```go
type RemoteConsumerState uint8

const (
	ConsumerNeedsLive RemoteConsumerState = iota
	ConsumerNeedsEither
	ConsumerNeedsShared
	ConsumerLoadingLive
	ConsumerLoadingShared
	ConsumerResident
	ConsumerCancelled
)

type LiveSnapshotChunk struct {
	SnapshotID SnapshotID
	Coordinate ChunkCoordinate
	Pages      [256]PageVersionRef
	Consumers  map[ConsumerID]RemoteConsumerState
	Promotion  ChunkPromotion
}
```

Every `PageVersionRef` pins the exact fork-time 4 KiB version, even if the
source branch later writes that page. The array is an accounting group, not a
1 MiB physical CoW object.

Shared objects use the following lifecycle:

```go
type SharedObjectState uint8

const (
	ObjectAbsent SharedObjectState = iota
	ObjectStaging
	ObjectPublished
	ObjectRetiring
	ObjectTrash
	ObjectDeleted
	ObjectCorrupt
)
```

`ObjectPublished` begins only after chunk file sync, atomic rename, and shard
directory sync. `ObjectRetiring` is an internal GC decision while holding the
family repository mutation lease. The deletion linearization point is atomic
rename from `chunks/` to a transaction-specific `trash/` name followed by
directory sync. No new manifest may begin referencing the object after that
point. Final unlink from `trash/` is asynchronous. A correctly referenced or
leased object never reaches `ObjectRetiring`; TTC does not rely on NFS
`.nfs*` placeholders to make an incorrect deletion safe.

### Reference classes

TTC maintains two separate reference domains.

**Process-local references** protect live host memory:

1. branch PMA and mapping references to current 4 KiB pages;
2. `PageVersionRef` references retained by each fork snapshot;
3. network read leases acquired while copying a complete chunk response;
4. publisher leases acquired while hashing and writing a complete chunk; and
5. loader/install references held until an all-or-nothing installation commits.

These references are managed under Sentry locks and disappear with the Sentry.
A Sentry may exit only after every externally required page version has a
durable shared replacement or its dependent branch has been cancelled.

**Cluster references** protect shared chunk files across processes and nodes:

1. immutable dehydrated-branch manifests;
2. immutable live-handoff generations mapping promoted coordinates to chunks;
3. active loader leases and their remaining-chunk bitmaps;
4. publication transactions not yet incorporated into a manifest; and
5. explicit soft cache pins, which are never required for correctness.

The first four are GC roots. They are traced instead of summarized by a mutable
numeric reference count, since a daemon crash could lose a count update. All
root creation, root removal, and GC retirement for one family are serialized by
a fenced family repository mutation lease. Failure may retain unreachable
objects, but cannot remove a reachable object.

The initial shared-filesystem implementation realizes this lease with one
cross-host advisory lock file per family plus a monotonically increasing epoch
in family metadata. Chunk data construction does not hold the lock. Publishing
or removing branch roots, creating or finally removing loader leases, publishing
handoff generations, and the complete GC trace-and-retire step do. A holder
rechecks its epoch immediately before every metadata rename. Loader bitmap
clears use atomic replacement outside the family lock because they only remove
roots; a concurrent GC may observe the older conservative bitmap. Distributed
mode starts only after two-daemon tests prove that the configured shared
filesystem provides cross-host lock exclusion, lock release on process/node
loss, coherent rename visibility, and durable directory synchronization.

### Reference and transition points

The following durable or locked events are the protocol's reference points:

| Point | Event | What becomes legal afterward |
| --- | --- | --- |
| `L0` | Fork snapshot and its page references are published under Sentry locks. | Source execution may resume and write through 4 KiB CoW. |
| `L1` | Target branch and live dependency are durably accepted. | The remote fork command may report success and B may fault pages. |
| `L2` | Chunk file is synced, renamed into `chunks/`, and its directory is synced. | A later manifest may name the chunk; live refs still cannot be released. |
| `L3` | Manifest or handoff generation naming the chunk is synced and published. | Consumers may switch from live to shared; the origin is `dual`. |
| `L4` | A target atomically installs all valid pages and changes `loading` to `resident`. | Guest waiters may resume; the target may acknowledge the complete chunk. |
| `L5` | Target acknowledgement or durable shared-origin switch is recorded. | That consumer no longer needs this live snapshot chunk. |
| `L6` | Every consumer passed `L5` and all live I/O leases drained. | Source `PageVersionRef`s for the chunk may be released. |
| `L7` | Loader-lease clears are durably synchronized. | GC no longer treats that loader as needing the installed chunks. |
| `L8` | GC, under the fenced mutation lease, renames an unreachable chunk to `trash/` and syncs directories. | New roots cannot acquire it; asynchronous final unlink is safe. |

Publishing bytes at `L2` is not sufficient to release live memory. Release is
delayed until both discoverability (`L3`) and every consumer's transition
(`L5`) are durable.

The local state transitions themselves have these linearization points:

| Transition | Linearization point | Failure rollback |
| --- | --- | --- |
| non-resident -> loading | CAS or chunk-map lock records a fresh `LoadID`. | The elected loader restores non-resident with the same origin and increments generation. |
| loading -> resident | All pages and accounting are prepared, then one chunk-map update publishes them at `L4`. | Prepared pages are released without becoming visible. |
| resident clean -> resident dirty | The 4 KiB page replacement and corresponding dirty-bit update occur under the branch chunk/PMA locks. | The write has not occurred if replacement fails. |
| live -> dual | A handoff generation containing `ChunkID` is published at `L3`. | Keep live-only and retry; an unreferenced published chunk is later collected. |
| dual -> shared | The consumer's durable origin switch or resident acknowledgement reaches `L5`. | Keep both origins; never discard live first. |
| resident -> evicting | A frozen branch records an eviction transaction after loaders settle. | Restore resident and keep the old runtime state. |
| evicting -> non-resident/shared | The new complete manifest is published and the branch/runtime pointer is atomically replaced. | Keep resident until publication succeeds. |
| published -> retiring/trash | GC holds the family mutation lease, traces no roots, renames at `L8`, and syncs. | Before `L8`, restore published; after `L8`, only recovery/final unlink may act. |

### Source writes and adjacent-page promotion

When a local branch writes a 4 KiB page retained by a remote snapshot, ordinary
CoW preserves the old `PageVersionRef`. The write-fault path performs no file or
network I/O. After locks are released it queues the corresponding
`(SnapshotID, ChunkCoordinate)` if at least one consumer still needs live data
and no identical shared chunk has reached `L3`.

The background publisher writes all valid pages in that 1 MiB snapshot chunk:

1. Change `idle/retryable` to `queued`, coalescing duplicate dirty-page hints.
2. Acquire publisher leases for all fork-time page versions and change to
   `capturing`. Current source pages are not substituted for snapshot pages.
3. If every consumer became resident meanwhile, cancel and release the leases.
4. Otherwise hash and write the complete chunk, then pass `L2` and `L3`.
5. Notify consumers that their origin is `dual`; network requests already in
   flight remain valid because both sources contain identical bytes.
6. After each consumer reaches `L5`, pass `L6` and mark promotion complete.

Thus adjacent pages are written too, but asynchronously and only from the same
fork-time snapshot. A dirty hint on any of the 256 pages promotes the complete
chunk. Multiple remote children from the same capture share the promotion;
children from different capture instants use different snapshot chunks and may
produce different content IDs.

`PromotionQueued` means eligible, not that the write must start immediately.
The scheduler prioritizes a target demand for that chunk, source shutdown or
dehydration, high snapshot-only memory, and repeated dirty hints. Under low
pressure it may delay speculative publication long enough for the target to
fetch and acknowledge the chunk directly, avoiding the disk write altogether.
Once promotion enters `capturing`, however, its payload and network-visible
unit are always the complete 1 MiB chunk; TTC does not create adjacent-page
subobjects.

### Operation timing rules

| Operation | Required transition and completion point |
| --- | --- |
| Local fork | Freeze the branch; share resident 4 KiB page refs and non-resident origin descriptors; join immutable `loading` transactions; serialize against `evicting`; publish the child's chunk-map root; then resume. No chunks are loaded or written. |
| Live remote fork | Freeze; share or delegate clean non-resident origins, join immutable loads, and retain fork-time refs for resident versions at `L0`; durably publish the target dependency at `L1`, then resume. Chunk publication is background work and is never on the source-pause path. |
| Network fault | Canonicalize the node chunk version, atomically elect one loader, request exactly that logical chunk, acquire source network leases, transfer and validate all bytes, install all pages for every joined local binding at `L4`, and acknowledge the chunk at `L5`; interrupted transfers install nothing. |
| Source page write | CoW only the written 4 KiB page, then enqueue whole-chunk promotion after releasing memory locks. The application never waits for `L2` or `L3`. |
| Source memory pressure | Queue promotion for live-only chunks in priority order. Reclaim a chunk's old page versions only at `L6`. |
| Dehydrate running/paused branch | Freeze and settle loaders. Reuse non-resident shared chunks; materialize live-only origins; write complete current contents for resident chunks. Publish a complete manifest at `L3`, replace runtime state with that manifest, then release branch pages. Outbound remote snapshots are promoted separately because they may contain older bytes. |
| Stop/exited branch | Discard the branch's own current memory, but first promote every outbound remote snapshot through `L3` and move its consumers through `L5`, or explicitly cancel those consumers. Stop completes only when the Sentry can release those references at `L6`. |
| Rehydrate | Before consuming the branch manifest, publish a loader lease covering all chunks. Restore with every slot non-resident/shared. Each fault commits a whole chunk at `L4`; batch lease clears reach `L7`. |
| Redehydrate partially loaded branch | Reuse all non-resident shared chunks. Hash/write complete chunks for every resident slot, publish a new manifest, then remove the old loader lease. No half-old/half-new chunk merge exists. |
| Fork dehydrated branch locally or remotely | Under the family mutation lease, publish another branch pointer to the same immutable manifest. The operation is metadata-only; the destination creates its own loader lease before consuming it. |
| Remote fork of partially rehydrated branch | Reference clean resident/shared and non-resident/shared chunks directly. Create live snapshot chunks at `L0` only for resident chunks whose current contents lack an exact shared backing. |
| Remove branch | Remove its manifest pointer or loader lease under the family mutation lease. Release process-local page refs after guest execution stops. GC may begin `L8` only after all other roots are traced. |
| Unlink chunk | Only GC performs it, only for an unreferenced published object, and only at `L8`. Loading, rehydration, branch removal, and source shutdown merely remove their own roots; they never directly unlink shared data. |

### Keeping useful shared chunks after rehydration

After loading, the resident branch may retain `SharedChunkID` with a zero dirty
bitmap as a clean-backing root. This consumes no additional space beyond the
already published chunk and allows a later local or remote fork to reference it
without consulting the process. On the first write to any page in the chunk,
the branch marks that page dirty. Existing remote consumers keep their own
manifest roots to the old chunk; the writing branch may drop its clean-backing
root when policy no longer values it.

This clean-backing root is an optimization, not a source of page correctness:
the resident pages remain authoritative for the running branch. TTC may expose
a retention policy such as `while-clean`, `cache`, or `eager-reclaim`, but it
must represent the retained object as a real cluster root until it is released.

A remote fork racing the first dirty write is serialized by the branch chunk
lock. If the fork wins, it publishes the target manifest reference to the clean
chunk before the source can drop its clean-backing root; the later write does
not affect the target. If the write wins, the chunk is dirty and the fork takes
fork-time `PageVersionRef`s at `L0`, using the live-to-shared promotion path.

The benchmark and its limitations are recorded in
[`g3doc/benchmarks/ttc_pebble_qualification.md`](../benchmarks/ttc_pebble_qualification.md).
Real integration must still test guest-fault latency, state restoration,
shared pages across dehydrated branches, loader leases, daemon crashes, and a
crash between backend construction and TTC metadata publication.

### Deferred general page and extent store

If TTC later revisits a unified mutable store, its records remain explicit and
versioned:

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

The earlier general-store synthetic qualification compared two Pebble modes:

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
for a future unified mutable store if simpler backends fail its qualification.
They are not additional layers beneath the loose-file or Pebble hydration
backends.

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

### Immediate memory-hydration prototype

* Define a versioned immutable manifest and 1 MiB loose-chunk format. Do not
  add Pebble to TTC's runtime dependency graph.
* Add stable lineage, logical-chunk, and logical-page IDs to `MemoryFile`
  allocation and preserve them across local fork, remote metadata cloning,
  dehydration, and rehydration. Do not use capture-specific packed offsets as
  persistent identities.
* Assign each Sentry incarnation a collision-resistant allocator domain and
  allocate page/chunk IDs as `(allocator domain, monotonic sequence)`. Preserve
  imported IDs, never reuse a logical page ID, and start a fresh allocator
  domain after process-free rehydration so no synchronous per-allocation
  cluster coordination is required.
* Maintain logical-page and sealed-version mappings incrementally in a
  persistent run-based map keyed by `MemoryFile` page index. Fork takes a
  refcounted root in O(1) and manifest encoding walks that root after the source
  resumes; no page/range scan is allowed in the source-pause interval. Reusing
  a freed `MemoryFile` offset always allocates a new logical page ID.
* Audit every mutation path that can change `MemoryFile` bytes, including PMA
  write faults, kernel `MapInternal` writes, zeroing, truncation, tmpfs/shared
  memory, and writes through aliases. Each path must split a shared/sealed page
  version before modification or prove that the page is unique and mutable.
* Add a state-capture range registrar to PMAs and every serialized kernel owner
  of a `MemoryFile` range (special mappings, AIO, shared memory, VDSO, packet
  rings, tmpfs/private files, and later supported devices). Reject capture if a
  saved reference has no binding; never fall back silently to exporting all
  family allocator occupancy.
* Replace the remote target's new-family sandbox creation with lookup-or-create
  of the one `(global family, target node)` Sentry. Import later remote branches
  into that Sentry and reject incompatible configuration.
* Replace `ImportMemoryFileMetadataForRemote`'s empty-file/exact-offset clone
  for subsequent imports with a merge plan. It resolves each source
  `(LogicalPageID, PageVersionID)` to an existing canonical target range or a
  newly allocated target range, then translates every restored `MemoryFile`
  reference through that plan. Source backing offsets are never treated as
  globally unique and may not overwrite an existing target allocation.
* Implement shared-root chunk publication with temporary files, file sync,
  atomic rename, shard-directory sync, and manifest-last publication.
* Implement `openat`/`unlinkat`, batched directory synchronization, bounded
  descriptors, full-chunk loading, and checksums.
* Replace the current subrange installation path with a per-runtime canonical
  chunk table and transactional 1 MiB loading: all valid slots move together
  from non-resident to loading to resident, concurrent faults join one load,
  and no guest thread can observe a partially installed chunk.
* Keep family-level chunk references outside branch ancestry so shared chunks
  survive until the last dehydrated manifest releases them.
* Publish active-loader leases in the shared root. GC may unlink a chunk only
  when immutable manifests and unexpired/recovered leases make it unreachable.
* Keep 4 KiB resident and CoW accounting above the 1 MiB backend; loading one
  chunk installs all of its valid pages without merging their CoW identities.
* Canonicalize sealed page versions independently of chunk versions. A
  one-page difference between two resident chunk versions must consume one new
  4 KiB page, not another 1 MiB of resident memory.
* Implement idempotent live-origin delegation so an untouched non-resident
  chunk can survive remote-fork chains without first loading on every
  intermediate node. Never publish a descendant until every origin is durably
  shared or registered with an available provider.
* Exercise local and cross-host demand loading, concurrent prefetch, partial
  rehydration, redehydration, crash recovery, source-node loss after
  publication, and snapshot removal.
* Test `A.1 -> B.2 -> B.3` with B.2/B.3 forked both before and after their
  inherited chunks load: either sibling's first fault performs one node-local
  full-chunk load, both see identical bytes, and a 4 KiB write by either sibling
  remains invisible to the other. Repeat with live and shared-file origins and
  with fault/fork/write races.
* Test two direct `A.1 -> B.2` and `A.1 -> B.3` remote forks around source
  writes and mapping changes. Assert one Sentry on B, exact reuse of unchanged
  chunk and page-version identities, one new resident page for a one-page CoW,
  stable mappings for downward stack growth, and no branch publication before
  origin registration.
* Model and test `A -> B -> C` while chunks are non-resident, loading,
  resident-clean, and resident-dirty; inject provider loss and every ordering
  of delegation, promotion, fault, cancellation, and acknowledgement.
* Randomize allocation, free, backing-offset reuse, aliases, `mmap`/`munmap`,
  `mprotect`, stack growth, tmpfs/shared-memory writes, truncate, fork, and
  removal. Compare every branch byte-for-byte with a simple page model and
  assert that no retired logical page ID reappears.
* Import two independently diverged source allocator layouts into one nonempty
  target `MemoryFile`, deliberately collide their source offsets, and verify
  that restore translation preserves aliases, shares equal page versions, and
  isolates different versions. Audit the state graph so no raw source
  `MemoryFile` offset bypasses translation.
* Measure source pause against resident size and fragmentation. Adding memory
  pages or allocator ranges may increase background manifest work, but must not
  make the root-snapshot portion of fork linear in either count.
* Benchmark 1 MiB chunks against the historical 64 KiB loose-file and packed
  baselines on local ext4/XFS and the intended shared filesystem. Record inode
  use, metadata operations, network bytes, useful-page ratio, page-fault tail
  latency, and full dehydrate/rehydrate time.

Exit criterion: lifecycle and crash tests pass, disk usage falls with consumed
loose chunks, another host can rehydrate after the source process and node have
exited, and real guest fault and dehydration times meet recorded gates.

The following phases apply only if TTC later resumes the deferred unified
mutable-filesystem store.

### Deferred Phase 0: model and measurements

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

### Deferred Phase 1A: standalone logical engine on Pebble

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

### Deferred Phase 1B: custom backend, only if Phase 1A fails

* Implement domain creation, dual superblocks, WAL, checkpoint, arena shards,
  stable page directory, free-range allocator, epochs, and checksums.
* Implement allocate/read/write/reference/release APIs over page runs.
* Implement root trees and O(1) root clone/delete.
* Implement offline tail-fill compaction and recovery from every relocation
  boundary.

Exit criterion: the same Phase 1A suite passes and the measured failure that
required replacing Pebble is eliminated without regressing the other gates.

### Deferred Phase 2: shared dehydration page store

* Export durable memory into extent manifests instead of packed files.
* Rehydrate through a backend-backed asynchronous reader and active lease.
* Batch page-consumption updates and reclaim pages after durable release.
* Re-dehydrate by reusing unresolved ranges without draining them first.
* Retain the existing packed format only behind a temporary development flag.

Exit criterion: repeated dehydrate/rehydrate cycles do not amplify memory or
disk usage when few pages change, and every branch can become process-free.

### Deferred Phase 3: distributed shared-store integration

* Publish shared-domain manifests to remote nodes.
* Combine partial chunk publication with the existing network reader where
  useful.
* Add writer fencing, disjoint allocator reservations, and remote lease
  recovery.
* Remove the shared-filesystem reflink assumption from the remote protocol.

Exit criterion: source deletion and source-node shutdown are safe after
manifest commit, and remote demand reads require no branch-specific snapshot
file.

### Deferred Phase 4: filesystem buffered I/O

* Add a new regular-file data-root field while retaining virtual inode and
  directory metadata.
* Implement read/write/truncate/fallocate/fsync against logical page maps.
* Preserve hard links, open-unlinked files, sparse files, and branch data
  generations.
* Keep the old host-object driver selectable for differential testing.

Exit criterion: syscall and application suites pass without mmap, and first
write cost is proportional to touched pages rather than guest file size.

### Deferred Phase 5: filesystem mappings and live fork

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

### Deferred Phase 6: rootfs and volume cutover

* Move rootfs upper and managed volumes to the extent driver.
* Run old/new differential tests and crash tests against identical traces.
* Remove the daemon reflink probe, whole-file fallback, and
  `--allow-slow-volume-copy` after no caller uses host objects.
* Update user documentation only when the new format becomes the default.

This is a pre-release storage-format replacement, not a promise of automatic
migration from the development object format.

### Deferred Phase 7: optimization

* Tune run coalescing, cache sizes, WAL group commit, allocator shards, and
  compaction thresholds.
* Add clean-origin tracking to reuse loaded but unmodified dehydrated pages.
* Add optional compression or content dedup only if measurements justify the
  CPU and locking cost.
* Evaluate online compaction under mapped workloads and keep it disabled when
  it harms tail latency.

## Test plan

### 1 MiB shared-chunk qualification

* Compare fixed 1 MiB chunks with historical 64 KiB chunks and the current
  packed snapshot using dense sequential, sparse random, pointer-chasing,
  browser, build, database, and agent workloads.
* Measure dehydrate throughput, first-fault p50/p95/p99, useful 4 KiB pages per
  loaded chunk, bytes read per demanded page, peak rehydration RSS, inode and
  directory counts, metadata operations, and time until disk space is returned.
* Repeat with 1, 4, 16, and 64 loader workers and multiple branches sharing the
  same chunks.
* Run the matrix on local ext4/XFS and the production shared filesystem, both
  same-host and cross-host, with cold and warm client caches.
* Verify atomic publication and visibility while killing the writer before and
  after file sync, rename, directory sync, manifest publication, and branch
  pointer replacement.
* Race two hosts publishing the same digest, consuming the last two references,
  renewing or expiring loader leases, running GC, and redehydrating a partially
  loaded branch.
* Disconnect and restart nodes, expire only fenced leases, and prove that GC
  either conservatively leaks or deletes an unreachable chunk, never a
  reachable one.

Acceptance requires bounded metadata growth, no stale-cache reads, no required
reflink or hole punching, cross-host recovery after source loss, and explicit
useful-page and tail-latency limits derived from real workloads.

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
