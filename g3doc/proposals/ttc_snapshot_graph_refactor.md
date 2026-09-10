# TTC snapshot-graph storage refactor

## Status and scope

This document is the implementation plan for the next TTC data-root format. It
consolidates the recent standalone-storage, distributed authority, remote
paging, dehydration, reference-counting, and garbage-collection decisions into
one design.

The refactor is intentionally incompatible with existing TTC data roots. It
introduces standalone root format 2 and clustered root format 5. TTC will not
migrate, partially import, or modify an older root. An operator must select an
empty root and explicitly move data through OCI image save/load or another
user-visible export mechanism when that is useful.

This plan supersedes the data-layout and ownership portions of:

* `ttc_userspace_extent_store.md`;
* `ttc_disk_backed_volumes.md`;
* `ttc_distributed_live_fork.md`; and
* `ttc_elastic_cluster.md`.

Those documents remain historical descriptions until the final documentation
phase removes or rewrites them. The cluster-wide Raft control-plane principle
remains: one Raft group governs the cluster, and bulk data never enters its
log.

## Implementation ledger

This section is the authoritative progress tracker. A phase is marked complete
only after its gate has passed; landing foundations for a later phase does not
waive that phase's remaining migration or qualification work.

| Phase | Status | Landed milestones | Remaining gate |
| --- | --- | --- | --- |
| 0: specification and boundary | in progress | Plan committed in `15705aec7`; strict root recognition and the destructive format boundary landed in `5de1eb4a3`. | Record the complete pre/post performance and NFS/Lustre qualification matrix. |
| 1: version/API/config cleanup | complete | Standalone format 2 and clustered format 5 are enforced; writable repositories moved behind repository format 3 in `90b336ae9`; reflink is optional with automatic whole-file fallback. Marked roots no longer synthesize omitted branch/filesystem/storage/eligibility metadata or search visible-name memory layouts. The client does not inspect either current or legacy root variables, including while launching its sibling daemon. The executable grammar, completion, SDKs, MCP, and protocol document agree on API 6; removed root/runtime options occur only in rejection tests and explanatory documentation. The stripped static client is 2.8 MiB and depends only on `//runsc/ttc/client`, versus the recorded 41.32 MiB monolith. | Gate passed. |
| 2: common graph | complete | Persistent IDs, bounded persistent radix/B-trees, WAL/checkpoints, explicit roots/pins, exact hierarchical references, cascading GC, physical-path-free inspection, and graph-native 1 MiB memory records are present. Root transactions are reopened at every possible journal prefix; generation switching is tested on both sides of the atomic head publication and fails closed on incomplete published state. Bulk installation validates exact physical lengths and reconciliation removes unpublished temporaries and uncommitted complete objects. A 1,000-step mutation/fork/checkpoint/reopen model independently recomputes every edge/root/pin count after each operation and drains all unreachable objects; it found and fixed duplicate-child undercounting. A second randomized graph-backed filesystem model exercises forks, whole-file CoW writes, hardlinks, open-unlink/reap, rename/name reuse, branch deletion/GC, and repository reopen, supplementing the focused path-copy, mmap-memory, xattr, whiteout, and crash tests. | Gate passed. |
| 3: standalone filesystem/images | in progress | Directory import (`2e3ec8092`), managed/rootfs repository graph conversion (`8da8fc220`), ancestry-independent direct lookup with authoritative diff roots (`edf392cb9`), and Sentry-safe descriptor-relative graph/object mutation (`2146eaff3`) have landed. A real commit/save/remove/load/recreate sequence preserved image labels, rootfs changes, and whiteout deletions. Selected whole-file CoW, hardlink/orphan, interrupted publication, and 131-cycle fork/delete/GC tests pass directly on NFSv4.2 with no `.nfs*` residue. TTC no longer overlays its persistent rootfs `/tmp` with anonymous tmpfs, so offline branch operations see it; `/dev/shm` and explicit tmpfs mounts remain memory-backed. | Remove remaining legacy filesystem data paths and qualify copied mounts, FUSE, broader OCI images, and package/source-build workloads. |
| 4: standalone memory/lifecycle | in progress | Dehydration writes cold state into 1 MiB graph objects with independent 4 KiB page identities and versions (`061371505`); direct graph reads (`a72e461e4`) and standalone graph-root rehydration (`1c570928f`) are active; live and dehydrated forks clone only radix-root references. Standalone control roots and persistent cold data are graph-only, including peer rehydration and dehydrated fork, and standalone no longer runs the clustered publication/cache layer (`0790edf2c`). Direct daemon/Sentry graph capture (`8d464de75`) removes the transient compatibility repository. Branch pointer replacement, crash-root reconciliation, empty-origin collection, and bounded WAL compaction landed in `6df3827e7`; capture payload buffering is bounded to one logical chunk in `a04462c29`. A real lifecycle run preserved a 24 MiB `/dev/shm` object and disk-backed `/tmp` across dehydrate/rehydrate and dehydrated fork, verified branch divergence, stopped paused and dehydrated branches without resuming them, and reclaimed every bulk object. Large final-release WALs now compact synchronously at the existing 8 MiB bound rather than waiting five minutes for maintenance. | Extend the lifecycle matrix to daemon-crash boundaries, named exec/FUSE/socket state, high branch counts, and resident/private accounting. |
| 5: distributed provider protocol | in progress | Raft snapshot allocation (`8ed3d29fa`), exact local adoption of externally allocated origins (`439ab99a8`), provider fencing (`3409e38d2`), transport (`819c907b0`), publication fencing (`ca619de37`), and resolver initialization (`de71aeefa`) are foundations. Immutable snapshot metadata now streams from its Raft-authorized provider or shared pack into a read-only catalog and reconstructs memory without claiming reference authority. Cluster dehydration allocates its origin in Raft, captures directly into the graph, publishes graph packs before switching provider authority, and rehydrates on another node without a legacy memory repository. Repeated A-to-B-to-A capture restored monotonically advancing guest state from snapshots 1 and 2; stale branch-name control caches are fenced by embedded snapshot identity. Sentry faults now fetch only missing catalog-authorized 1 MiB objects through a fenced relay and atomically install them in the node-local cache. A cold-cache two-node test began execution after fetching 2 of 12 objects and fetched the remaining working set only when it was read. Branch records preserve immutable root object IDs separately from branch-local root names, so a shared snapshot rehydrated on B can fork locally there without importing reference authority into B's cache graph; the child subsequently dehydrated as snapshot 2 and rehydrated on A with advancing guest state. Remote fork from a process-free graph branch now transfers only metadata and a graph manifest eagerly; the target faults immutable objects through its transaction-fenced resolver. Both a new target Sentry and import beside an existing sibling are covered, including repeated B-to-A round trips, filesystem isolation, and cleanup of transient handoff files. Volatile origins now have a durable target-side retain/release intent and a provider-side idempotent root batch (`fbb260e27`). Local children receive independent lifetime identities even when they share the original distributed transaction. | Convert live remote fork/migration, close provider-to-shared ledger handoff, and pass the full interruption/retry matrix. |
| 6: shared images/cache | in progress | Cluster image creation now emits an immutable indexed uncompressed shared pack. A cold node creates only a metadata skeleton; its gofer reports pack metadata and atomically populates whole files before exposing descriptors, while a background pass completes the cache. Per-file completion is crash-retryable and serialized across gofers without checksumming guest reads. Unused node-local image roots are evicted, and unreferenced shared manifests/packs are collected after a publication grace. Manifest publication is create-if-absent and ordered after pack durability. Pack creation hashes bytes as they are written instead of rereading the shared file. | Pass large-image shaped-NFS/Lustre cold-start gates and add cache-size/eviction accounting. |
| 7: peer data/publication | not started | Immutable object transport is available as a primitive. | Peer filesystem/memory range service, asynchronous publication, dependency accounting, and repeated migration gates. |
| 8: distributed references/GC | not started | None. | Origin-owned ledgers, cross-owner outboxes, authority handoff, shared GC/compaction, and crash gates. |
| 9: elastic integration | not started | Existing scheduler/Raft groundwork predates this storage refactor. | Rebase placement, drain, quarantine, fencing, and corruption closure on graph authority. |
| 10: removal/qualification/docs | not started | None. | Delete legacy implementations, run the full non-KVM matrix, update all docs/SDKs/MCP, and install final static binaries. |

### Current milestone

Graph roots are now the sole standalone dehydration source, and the Sentry
streams a bounded capture directly to the daemon that owns the graph. No
compatibility chunk repository is created. Standalone TTC also creates no cold
packs, publication queue, or `application.mem`; resident memory remains
anonymous and 4 KiB CoW, while cold data uses exact 1 MiB graph objects. The
active milestone exercises failure boundaries across the complete standalone
lifecycle matrix. Repeated dehydration/rehydration and final family removal
have been verified to leave an empty graph and zero memory objects after
reconciliation, with the WAL compacted to its header.
The capture stream derives its manifest without reading page bytes and then
transfers one logical chunk at a time, so peak payload buffering does not grow
with resident memory.
In clustered restore, immutable graph metadata is resolved before execution,
but bulk memory is not. The Sentry uses its donated local object directory and
a per-restore token-fenced stream; a missing 1 MiB chunk is resolved from its
Raft-selected origin or shared pack, installed once, and reused locally.
The same path now serves remote forks from dehydrated branches. In a two-node
cold-cache test, boot fetched 2 additional objects, reading a 12 MiB guest file
raised the cache by 13 more objects, and the guest counter advanced across the
fork. A second cold fork imported beside the first branch in the same Sentry;
their filesystem writes remained isolated. Dehydrating the imported branch and
forking it back to the first node preserved execution state and left no staged
metadata or graph manifests in the shared transaction directories.
The volatile-origin provider protocol also exposes bounded, transaction-fenced
root retain/release operations. Each batch is journaled atomically at the
provider, identifies roots by immutable object ID and stable logical slot, and
is idempotent across lost responses. Descendant edges and object counts remain
entirely in the provider's graph ledger rather than entering HTTP or Raft.
Distributed graph sessions now carry the selected snapshot ID and ordered
immutable root-object IDs as control metadata. Targets persist fresh logical
root names derived from their filesystem-branch identity for both new-family
and existing-family imports, so later fork, dehydration, removal, and ledger
operations no longer lose the restored branch's graph identity.
In a fresh two-node test, a shared snapshot-1 branch imported on node B kept
its root object in `ls --json`, forked a running local sibling from that root,
and then dehydrated into snapshot 2 while the sibling continued to reference
snapshot 1. This verifies identity continuity across import, local fork, and a
new capture origin.
Target-side provider references now close their own crash prefixes before a
cluster daemon accepts commands. A two-node qualification remotely forked a
dehydrated graph branch from A to B, locally forked it again on B, removed the
original B branch, killed and restarted B's daemon, and executed the surviving
child before releasing its last intent. The test also exposed and fixed two
independent faults: manager-side publication had retained a descriptor-only
bulk store (`53d9f0062`), and Raft proposal retry identity incorrectly included
leader-generated timestamps (`3b32e255d`). Split-family deletion now removes a
node-private repository when its last physical branch disappears instead of
asking the repository to represent zero roots (`23c28ba85`).
Once shared publication commits, provider-root retirement makes the former
node's immutable object set reclaimable without changing snapshot identity or
target references (`e3a126b20`). Cluster images also have their first
shared-storage-native representation: a complete local extraction is packed
once, its manifest is installed only after pack durability, and other nodes
reconstruct caches from indexed range reads without applying OCI layers again.
Restrictive modes are restored after materialization, fixing read-only files
that span multiple 1 MiB pack extents.
Cold nodes now stop before that full reconstruction: they create directory and
link metadata plus empty regular-file placeholders, then let the rootfs gofer
populate a complete immutable file before donating any descriptor. A durable
per-file completion record follows the file fsync, so interrupted writes are
rewritten and concurrent gofers serialize on the same local lock. A real
three-node cold-cache BusyBox run reached guest execution in 330 ms on a local
shared root, including command verification. Lazy-pack, root, and completion
state descriptors now survive the rootless gofer's pivot and re-exec without
reopening host paths. A forced target-daemon restart after deliberately
truncating a partially materialized file reconstructed it from the shared pack
and resumed the branch without losing Raft quorum. An earlier NFSv4.2 run also
reached guest execution and subsequently exercised cluster self-fencing and
Raft rejoin when the host NFS export was externally restarted; that outage is
not counted as large-image performance evidence.

## Goals

1. Make a local fork or filesystem snapshot O(1) in resident pages and total
   files. Work may depend on the fixed tree height and the number of top-level
   address spaces, filesystems, or runtime roots.
2. Make page and file lookup independent of branch ancestry depth.
3. Preserve page-granular memory CoW across local fork, remote fork,
   dehydration, rehydration, and repeated migration.
4. Use whole regular files as the filesystem data-object and CoW boundary.
   Do not implement sparse local files or a userspace block store in this
   iteration.
5. Make every immutable object's lifetime exact: a zero reference count at its
   authoritative owner is sufficient to reclaim it.
6. Keep ordinary guest memory writes, filesystem writes, reads, and page faults
   off Raft and, for clustered TTC, off the shared filesystem hot path.
7. Make standalone TTC genuinely standalone: no Raft, no cache tier, no lazy
   image materialization, and no distinction between local and authoritative
   storage.
8. In clustered TTC, use node-local storage only for active authority,
   publication staging, replicated control state, and disposable complete-
   object caches. Shared storage contains durable immutable snapshots and
   images.
9. Make all asynchronous work restartable, cancelable, idempotent, and unable
   to resurrect a removed branch or supersede newer execution.
10. Remove correctness dependencies on host reflinks, hole punching, sparse
    files, host overlayfs, and runtime bulk-data checksums.

## Non-goals

* Migrating an old TTC data root in place.
* Detecting latent disk or memory corruption. TTC assumes reliable storage.
* File-block-level CoW. A first inherited-file write may copy the whole file.
* Recording page, file, tree-node, or reference-count entries in Raft.
* A single-node Raft mode for standalone TTC.
* Making node-local image or snapshot caches authoritative after shared
  publication.
* Compressing hot snapshot data. OCI import/export may retain OCI compression.

## Correctness invariants

These are implementation requirements, not aspirations.

### Identity and immutability

* Snapshot IDs are monotonically allocated unsigned 64-bit integers. They are
  never reused, including after branch deletion or failed transactions.
* Distributed IDs are allocated by the cluster Raft state machine. Standalone
  IDs are allocated by a local transactional catalog.
* A node's physical identity remains a UUID. A daemon incarnation fences a
  particular execution of that node identity.
* An immutable object ID is `(origin snapshot ID, object ordinal)`. The object
  type is part of the typed reference, not inferred from a pathname.
* An object created in a branch's working origin may change only through that
  branch's exclusive active authority. Sealing freezes the entire origin in
  O(1); every later write uses a fresh working origin and CoW object.
* Sealed or published object bytes and metadata never change in place.
  Compaction changes physical representation and indexes, not logical object
  IDs.
* A branch name, its UI parent hint, and its current placement are not object
  identities. Branch names may be reused after removal.

### Authority

* At every instant, an object has exactly one authoritative reference ledger
  and exactly one authority class: standalone-local, volatile node, or shared.
* In distributed TTC, Raft's snapshot provider directory contains only origins
  not yet wholly durable in shared storage:

  ```text
  volatile_origins[snapshot_id] = physical_node_uuid
  ```

* Presence means that node is the authoritative provider for objects created
  under the snapshot origin. Absence means the origin is in committed shared
  storage. Authority may transition only `node -> shared`; it is never handed
  from one node directly to another.
* The provider directory says nothing about inherited objects. Their IDs keep
  their older origin snapshot IDs and route directly to that older provider.
* Removing the provider entry is legal only after the shared manifest is
  durable and the shared reference ledger has accepted ownership.
* A referenced origin with neither a live provider entry nor a committed
  shared manifest is corruption, not a cache miss.

### References and reclamation

* Every branch root, transaction root, runtime/open-file pin, tree edge, and
  in-flight graph operation is represented by exactly one reference.
* Every parent-to-child edge contributes one reference to the child. The edge
  counts once regardless of how many roots reference the parent.
* Adding a branch that shares an existing tree increments only the tree root.
  It does not traverse or increment descendants.
* Only sealed immutable objects are garbage-collected. Unsealed working
  objects are discarded only by their exclusive branch transaction or are
  first sealed before they become shareable.
* `refcount == 0` at the unique authoritative ledger is sufficient to reclaim
  an object. Safety must not depend on a special filename, provider epoch,
  cache pin, grace-period guess, or a later reachability scan.
* Open-unlinked files, executable mappings, FUSE-held objects, and runtime
  kernel objects are ordinary explicit roots until their last use ends.
* Cache entries do not contribute authoritative references. A node may evict a
  complete cached object whenever no local operation has it open.

### Publication and execution

* A snapshot is immutable. Publication changes only where that snapshot can be
  resolved; it never changes branch lifecycle or execution placement.
* Starting or rehydrating a branch while one of its snapshots is publishing is
  allowed. It reads from the already sealed local snapshot or its provider.
* A late publisher cannot stop, unplace, overwrite, or otherwise mutate a live
  branch.
* Remote fork and migration may begin target execution while immutable objects
  are still served by origin nodes. Source scratch is retained until every
  origin it owns has transitioned to shared or become unreachable.
* A draining node may exit only when it owns no branch execution, no volatile
  snapshot origin, no unacknowledged cross-owner edge operation, and no unique
  publication staging.

### Data integrity

* TTC uses structural validation: magic/version, typed IDs, exact lengths,
  bounded offsets/counts, complete-write markers, atomic rename, fsync order,
  and Raft/local transaction fencing.
* TTC does not hash memory chunks, files, trees, or packs on normal reads and
  writes. Checksums required by OCI or internal to dependencies such as Raft
  WAL implementations remain.
* A temporary or incomplete physical object is never authoritative merely
  because a file with the expected name exists.

## Common logical model

Standalone and distributed TTC share this model even though their physical
layouts differ.

### Snapshot and active epochs

Each active branch owns a preallocated `working_snapshot_id`. New pages and
files use that ID as their origin and remain exclusively mutable by that branch.
Metadata tree nodes are immutable from creation. Sealing the branch freezes the
entire working origin in O(1) and records its logical roots; it does not visit or
relabel each object. If the branch continues, it receives a fresh working
snapshot ID before guest execution resumes, and a later write to an object from
the sealed origin performs CoW.

Fork is therefore:

1. pause and quiesce the source branch;
2. seal its working snapshot and logical roots;
3. add one root reference for each new branch;
4. assign fresh working IDs to the source and destinations;
5. resume the requested branches.

It does not walk memory pages or directory trees. Stop and dehydrate also seal
the current working snapshot immediately. This gives every asynchronous
publication operation a stable identity without tying publication to the
mutable branch record.

### State enums

The logical model uses separate state machines rather than overloading branch
lifecycle:

```text
branch lifecycle:
  creating -> running <-> paused
  running|paused -> dehydrating -> dehydrated
  running|paused|dehydrated -> exited
  any healthy state -> corrupted -> removed

snapshot authority (distributed):
  working -> sealed-volatile -> shared -> reclaiming -> dead
                    \-> reclaiming -> dead

snapshot authority (standalone):
  working -> sealed-local -> reclaiming -> dead

publication job:
  queued -> writing -> manifest-ready -> committing -> complete
              |              |              |
              +------------> error <--------+
              \------------> canceled

edge operation:
  prepared -> activating -> active -> deactivating -> complete

local cache entry:
  absent -> fetching -> complete -> evicting -> absent

shared pack:
  staging -> committed -> retiring -> deleted
```

The publication job is orthogonal bookkeeping on a `sealed-volatile` origin;
it is not a branch lifecycle. A standalone seal transitions directly to
authoritative `sealed-local`, which replaces the distributed
`sealed-volatile/shared` distinction. A working origin is never peer-readable
or shared-publishable.

Every transition names one transaction ID and an expected current state.
Retries return the original result. States cannot move backward: restart and
rehydrate create new working origins rather than reopening a sealed origin.

### Memory tree

Memory mappings retain normal gVisor VMA metadata. Each mapped content range
refers through a fixed-height radix tree to immutable page IDs:

```text
address-space root
  -> radix nodes
     -> leaf runs
        -> PageID(snapshot_id, ordinal)
```

The semantic sharing and write-CoW unit is one guest base page, normally
4 KiB. Serialization and network transfer group aligned runs into records of
at most 1 MiB (up to 256 4-KiB pages). A transfer record may contain absent
slots for unmapped pages, but it is an ordinary compact record, not a sparse
host file.

A node installs a fetched record as one operation: all represented pages
become resident before waiters are released. The pages then retain independent
4-KiB CoW identities. Loading a 1-MiB record does not make 1 MiB the CoW unit.

Chunk coordinates are aligned in the address space, not relative to the first
mapped page:

```text
chunk_index = virtual_address / 1 MiB
```

A downward-growing stack that maps one new page changes the existing aligned
chunk when possible and introduces a new chunk only when it crosses a 1-MiB
boundary. Mapping identity and protection changes remain in VMA metadata.

### Filesystem tree

Each forkable filesystem has an immutable filesystem root containing:

* a bounded-fanout tree of inode metadata;
* a bounded-fanout directory-entry tree per directory;
* typed whiteout, xattr, device, symlink, and mount metadata; and
* whole-file `FileObjectID` references for regular-file contents.

Directory trees are keyed by guest name bytes. Inode trees are keyed by stable
guest inode ID. Hardlinks make the metadata an immutable DAG: multiple
directory entries may reference one inode object. Very large directories are
split into bounded B-tree nodes, so mutation and reference propagation never
depend on total directory size.

The first content-changing write to an inherited or sealed regular file creates
a new whole-file object in the branch's working origin. Later writes by that
same exclusive branch may update that working object in place. If source and
destination are on one local filesystem and `FICLONE` succeeds, TTC uses it;
otherwise TTC copies the file. `O_TRUNC`, unlink, rename-over, and complete
replacement avoid copying bytes that the operation discards. Guest reads
retain arbitrary byte granularity.

Rootfs uppers, managed volumes, `:copy` host mounts, and other forkable writable
mounts use this same graph. Read-only host mounts remain external read-only
resources. Tmpfs and `/dev/shm` memory are captured through their existing
memory/kernel-object paths.

### Persistent tree mutation

Mutation path-copies only the fixed-height tree path. A new parent node contains
direct child IDs, so lookup never follows snapshot-parent chains. To keep the
foreground mutation bounded, reference activation uses a retained-source
protocol:

1. add a durable operation reference to the new node and a retention reference
   to the old node;
2. publish the new immutable node with its bounded child list;
3. asynchronously and idempotently activate each new parent-to-child edge;
4. after all child activations are durable, mark the new node active and drop
   the old-node retention reference and operation reference.

The old node protects the same descendants until activation completes. The
operation reference prevents the new node from being reclaimed if its branch
root disappears concurrently. A crash may temporarily retain too much data,
but replay cannot undercount. There is no lookup indirection to compress and
no fork-depth penalty.

Edge operations are keyed by `(parent object ID, child slot, operation kind)`.
Duplicate activation or deactivation is a no-op. An immutable node cannot
change the child at a slot, making the key stable.

The authoritative ledger atomically changes an object with count one to
`reclaiming` when applying its final decrement. New non-idempotent increments
to a reclaiming or dead object are rejected; TTC cannot resurrect a zero-count
object. The reclaimer then deactivates its bounded outgoing edges and deletes
the physical object. A crash at any point replays those same edge-operation
keys.

### Root references

The reference engine recognizes these root types:

* current branch memory and filesystem roots;
* sealed snapshot manifests retained by a branch;
* fork/migration/publication transactions before commit or abort;
* active task address spaces and runtime kernel objects;
* open, executable, mapped, or FUSE-pinned filesystem objects;
* pending edge-activation/deactivation operations; and
* explicit administrative pins, if added later.

UI lineage is deliberately not a root. Removing a parent branch reparents its
children's display hints but does not alter data sharing.

## Preserved process, interface, and control-plane boundaries

This refactor changes storage and internal APIs, but keeps these previously
agreed boundaries:

* `ttc` remains a thin root-unaware client. CLI grammar, help, completion, and
  version output are local to the client. Only `ttcd` reads `TTC_ROOT` or
  `TTC_LOCAL_ROOT`.
* `TTC_SOCKET` selects the client/daemon Unix socket, with
  `--daemon-socket` taking precedence. Official client requests require an
  exact daemon build match, except the stable minimal `daemon stop` request.
  SDKs and MCP negotiate API features rather than build identity.
* Cluster nodes use one multiplexed TCP listener for Raft and TTC peer/control
  traffic. The trust model is an isolated high-performance private network,
  optionally authenticated by `TTC_CLUSTER_TOKEN`; this phase does not add TLS.
* `run`, `start`, `restore`, `rehydrate`, and `fork` accept optional `--node`.
  Omission delegates placement to the daemon. `--target` does not return.
* `ttc node stop NODE` performs node retirement and accepts exactly one of
  `--migrate-all`, `--dehydrate-all`, or `--stop-all`. Distributed
  `ttc daemon stop` supports the same policies for its local node.
* Quarantine is an independent branch flag that may be set or cleared at any
  time. Placement and migration preserve it; it is not a separate fork mode or
  lineage type.
* Every branch selector remains `CONTAINER@BRANCH`. `fork` takes one source
  selector followed by one or more destination branch names in the same
  family. Branch-level `rm`, `stop`, `pause`, `dehydrate`, and related commands
  remain primary; explicitly family-wide variants expand into branch
  transactions.
* `exited` remains the sole terminated lifecycle name. `main` has no storage or
  lifecycle privilege over another branch and may be removed independently.

## Standalone storage profile

Standalone TTC has one explicit `TTC_ROOT`. Everything in it is authoritative;
there is no `TTC_LOCAL_ROOT`, Raft, provider directory, shared pack, cache
eviction, publication queue, or source dependency.

### Physical layout

The exact filenames are private implementation details, but the format starts
with this division:

```text
TTC_ROOT/
  .ttc-root-format
  catalog/
    state
    journal/
  images/<image-id>/
    image.json
    rootfs/...
  objects/
    memory/...
    files/...
    metadata/...
  snapshots/...
  families/...
  runtime/...
  temporary/...
```

Images are completely extracted once during `pull` or `load`. A new branch can
use that extracted tree without copying it. There is no lazy image pack reader
in standalone mode.

Whole-file objects are ordinary private host files. Metadata and references use
append-only journal segments plus compact checkpoints. Memory dehydration uses
compact records up to 1 MiB rather than one file per page. Live resident memory
remains anonymous and is never permanently file-backed.

### Local transactions and recovery

The catalog serializes only ID allocation, root changes, and lifecycle
transactions. Guest data operations do not acquire a catalog-wide lock.

Every mutation follows:

1. allocate IDs and write immutable objects to temporary names;
2. fsync required object data and atomically install the objects;
3. append and fsync one bounded catalog transaction containing root and
   reference changes;
4. publish the checkpoint opportunistically;
5. reclaim obsolete objects asynchronously after their durable count reaches
   zero.

Recovery replays complete journal records, removes incomplete temporary
objects, resumes pending bounded edge operations, and rebuilds the reference
ledger from roots and immutable edges if the ledger is missing. Unknown files
are quarantined and reported; they do not make unrelated families unusable.

### Standalone lifecycle costs

* local fork: O(number of logical roots), excluding pause/quiescence work;
* filesystem seal/stop: O(number of dirty top-level roots), not total files;
* first inherited-file write: O(file size) without reflink;
* dehydration: O(branch-private resident state that must become durable);
* rehydration: metadata restore plus demand reads of 1-MiB memory records;
* branch removal: O(number of roots) synchronously, cascading GC
  asynchronously.

## Distributed storage profile

Clustered TTC has a common shared `TTC_ROOT` and one distinct
`TTC_LOCAL_ROOT` per node. It retains one cluster-wide Raft group for small
control-plane state. The logical snapshot graph is the same as standalone TTC;
only authority, routing, physical packing, and caching differ.

### Raft state

Raft contains:

* cluster configuration, membership, node incarnations, and leases;
* branch identity, lifecycle, placement, and current logical roots;
* globally monotonic snapshot-ID allocation;
* fork, migration, removal, and node-retirement transactions;
* `volatile_origins[snapshot_id] -> node UUID`;
* the shared-GC coordinator lease and shared-pack retirement records; and
* bounded idempotency results.

Raft does not contain snapshot parentage, tree nodes, pages, files, per-object
locations, reference counts, cache entries, or transfer progress. A branch
parent is only a UI lineage hint associated with the branch record.

### Resolver

All image, memory, filesystem, and auxiliary-state reads use one resolver:

1. use a complete local active object or cache entry;
2. inspect the object's origin snapshot ID;
3. if `volatile_origins` contains the ID, fetch the complete object or memory
   transfer record from that exact node;
4. otherwise locate it through the shared snapshot manifest and pack index;
5. atomically install the complete local cache entry, then satisfy readers.

Only one fetch per object runs on a node; concurrent readers join it. A partial
temporary file is never readable or advertised. Target writes always create
new target-local objects under the target's working snapshot ID.

Because every inherited object keeps its origin ID, this lookup is independent
of how often a branch has forked or migrated. Repeated migration does not form
an A-to-B-to-C proxy chain.

### Images

OCI blobs remain import/export inputs, but `pull` and `load` preprocess each
image into an uncompressed, indexed, random-access shared image pack. The pack
contains path/inode metadata and each regular file as one contiguous record.

A node does not extract the entire image before starting a container. It starts
background sequential extraction into a node-local complete-file cache. If a
guest requests a file ahead of that progress, the resolver reads that one
whole-file record from the shared pack and installs it. The two paths share a
per-file state machine:

```text
absent -> fetching -> complete
                  \-> absent + retryable error
```

Node-local image cache is disposable. Raft tracks family-to-image references,
not cache entries. Removing an unreferenced image removes its shared pack and
notifies nodes to evict matching cache opportunistically.

### Snapshot packs

Shared snapshots use immutable, uncompressed, indexed packs. A pack may contain
several origin sections to avoid a large number of small shared files. The
logical name of a section is `(snapshot ID, kind, segment ordinal)`.

```text
pack header
  magic, format, pack ID, creation transaction
data records
  typed object ID, logical length, stored length, payload
index pages
  ordered object ID -> record offset and exact length
footer
  index offset/length, record counts, completion marker
```

Records are structurally aligned for efficient range reads. Regular-file bytes
are stored as one whole-file record. Memory records contain at most 1 MiB and
an explicit page-slot bitmap. Tree and inode records are compact metadata.
There are no per-record data checksums.

Publication writes a temporary pack, fsyncs it, atomically renames it, writes
and fsyncs an immutable snapshot manifest, transfers reference-ledger
authority, then commits the Raft provider-map removal. The manifest is the
shared lookup root; the existence of a pack alone grants no authority.

### Provider service

A volatile provider serves only immutable objects whose origin IDs it owns.
Every request carries the snapshot ID, typed object ID, expected exact length,
requesting transaction, and current node/daemon fencing information. The
provider validates its current Raft lease and provider ownership before opening
data. It never serves a mutable host path.

A remote fork can return once the target has installed all control metadata and
can fault missing data through this service. Publication runs concurrently.
Snapshots backing remote branches receive the highest background publication
priority, followed by node drain, explicit durable waits, stopped/dehydrated
state, compaction, and cache prefetch.

### Reference-ledger ownership

There is not one replicated reference database on every node.

* While `volatile_origins[S] = A`, node A owns the ledger and reclamation for
  immutable objects created under S, including objects already staged in an
  uncommitted shared pack.
* After S becomes shared, exactly one Raft-leased shared-GC coordinator owns
  reference accounting and reclamation for its shared objects. This coordinator
  need not be the Raft leader.
* Every node independently evicts its disposable cache entries.

An edge whose parent and child have different owners uses a durable,
idempotent `ActivateEdge` or `DeactivateEdge` message. The sender retains the
old protecting reference and records the message in a local outbox until the
child owner durably acknowledges it. Duplicate delivery is harmless. Loss of a
deactivation delays reclamation; it cannot cause premature deletion.

The shared coordinator keeps its hot ledger and WAL on its local root. Counts
are derivable from committed Raft roots, immutable graphs, and all live volatile
providers. If the coordinator or its local database is lost, shared GC stops;
a replacement rebuilds the ledger before receiving a new GC lease. If any
provider required by the rebuild is unavailable, reclamation remains stopped.
Reads and execution do not wait for this rebuild.

### Node-to-shared handoff

For one snapshot origin S:

1. A freezes a ledger sequence boundary and produces packs and a manifest from
   immutable objects reachable in its owned origin.
2. Concurrent graph-reference messages after that boundary remain in A's
   ordered handoff log.
3. The shared coordinator imports the baseline and replays the catch-up log.
4. A briefly blocks new reference mutations for S, sends the final sequence,
   and waits for an exact acknowledgement.
5. Raft atomically removes `volatile_origins[S]` using the expected owner and
   handoff transaction.
6. Old messages are rejected or forwarded idempotently to the shared
   coordinator. A's physical objects become cache or are deleted.

Provider authority never changes to another node. Retirement completes this
handoff for every origin still owned by the retiring node.

### Shared reclamation and compaction

The shared coordinator cascades zero-count objects through bounded child lists.
It marks dead pack records in its ledger. A wholly dead pack is retired through
one Raft-fenced pack-retirement record and then deleted. A partially live pack
is retained until a background compaction threshold is met.

Compaction copies live records into new packs, publishes replacement physical
indexes/manifests, commits the representation switch, and retires old packs.
It does not change snapshot or object IDs. The initial thresholds are 32 packs
in a lookup set or at least 50% reclaimable bytes, subject to benchmarks.

Unknown shared files are quarantined and reported. Uncommitted temporary packs
are removed only after transaction recovery proves that no active publisher can
complete them.

## Operation protocols

### Local fork

1. Validate the family-level lifetime eligibility chosen at container creation.
2. Pause the source and quiesce branch-owned kernel state.
3. Seal the working snapshot and add destination root references.
4. Construct all requested siblings from the same seal in one pause window.
5. Assign each running branch a fresh working snapshot ID.
6. Resume source and destinations as requested.

No memory page, file, or descendant tree count is traversed.

### Remote fork

1. Raft reserves the target node and allocates operation/snapshot IDs.
2. Source pause seals the memory, filesystem, kernel, console, and named-exec
   roots; Raft records the origin provider and target placement transaction.
3. The source resumes immediately after immutable roots and provider retention
   are established.
4. Target restores metadata into its existing family Sentry, remaining paused
   until Raft grants execution.
5. Target executes and fetches missing whole files and 1-MiB memory records on
   demand from the origin providers.
6. Background publication and cache filling remove volatile dependencies.

All remote branches of one family on a node share one Sentry and one resident
page graph. If two local branches reference one remote PageID, the first fetch
installs a shared resident page object for both; either branch's write then CoW
copies only its 4-KiB page.

### Live migration

Migration uses the same seal and resolver as remote fork, followed by a Raft
placement cutover. The source remains paused until target execution authority
commits. The migrated branch receives a new target-local working snapshot ID;
its immutable inherited roots continue routing directly to their origin IDs.

A second migration need not wait for the first snapshot publication. It seals
the current target state as a new origin while retaining references to all
older origins. Each origin provider independently transitions to shared.

### Stop and restart

Stop seals a filesystem/runtime snapshot, terminates the runtime, records the
branch as exited, and begins publication in cluster mode. Restart may begin
immediately from the sealed local/provider roots. It receives a new working
snapshot ID; the older immutable publication neither observes nor controls the
new execution.

### Dehydrate and rehydrate

Dehydrate pauses the branch, seals all roots, writes process-free kernel state
and necessary memory records to authoritative local staging, destroys the
runtime, and records `dehydrated`. Cluster publication follows asynchronously.

Rehydrate restores metadata and demand-loads memory records. A record remains
available while any branch or remote origin needs it; loading it for one branch
does not delete shared authority or break CoW with another branch. Standalone
storage may reclaim a zero-count record immediately through its ledger.

### Removal and name reuse

Removal first revokes execution, then drops branch and transaction root
references. It does not recursively delete files during the user request.
Reference cascades and pack reclamation run asynchronously. A later branch may
reuse the human name because its internal branch ID, snapshot IDs, and object
IDs differ.

A publication already in progress may finish writing unreachable immutable
objects, but it cannot add a removed root. Those objects reach zero and are
collected. This is bounded temporary residue, not resurrection.

### Node drain and failure

Graceful drain:

1. stop new placement and snapshot allocation on the node;
2. migrate or stop/dehydrate branches according to the requested policy;
3. publish every volatile origin owned by the node, prioritizing remote
   dependents;
4. finish all cross-owner edge outbox messages and reference-ledger handoffs;
5. relinquish execution leases and Raft membership as required;
6. verify that the local root contains only caches and reclaimable temporary
   residue; and
7. stop the daemon.

If a node disappears before a volatile origin becomes shared, Raft fences the
node after lease expiry. Branches whose required objects existed only at that
origin become `corrupted` and support only removal. Branches that reference
only shared or surviving origins remain usable.

A shared-filesystem outage does not create corruption claims. Cluster control
and shared publication freeze according to the lease protocol; nodes must not
remove provider mappings or shared data until shared authority is available
again.

## Linearization points and foreground work

| Operation | Linearization point | Must finish before success | May continue asynchronously |
|---|---|---|---|
| Standalone local fork | Local catalog commits sealed roots and destination root references | Pause/quiesce, root-reference transaction, runtime branch construction | Descendant edge activation, GC |
| Distributed local fork | Raft commits destination branch identities/roots after local seal | Pause/quiesce, local root retention, bounded Raft command | Publication, edge activation, GC |
| Remote fork | Raft commits target placement and sealed roots | Source seal/retention, target control restore, execution grant | Page/file transfer, shared publication, cache fill |
| Migration | Raft CAS transfers placement after source pause and target preparation | Seal, target preparation, execution fencing/cutover | Transfer, publication, source-cache eviction |
| Stop | Branch lifecycle transaction records exited and sealed roots | Quiesce, seal, terminate runtime, root commit | Shared publication, GC |
| Dehydrate | Branch transaction records process-free dehydrated roots | Pause, complete local capture, destroy runtime, root commit | Shared publication, GC |
| Rehydrate/start | Execution authority commits for a fresh working origin | Restore enough control state to remain paused, placement/epoch grant | Demand paging, cache fill, older snapshot publication |
| Remove | Execution is revoked and branch root references are durably dropped | Stop/fence runtime, root transaction | Reference cascade, pack compaction/deletion |
| Publish origin | Raft removes its expected provider entry | Pack/manifest fsync, ledger handoff, provider-map CAS | Provider cache eviction, pack consolidation |
| Drain node | Raft records the node offline after all owned authority is gone | Branch disposition, every origin handoff, edge outbox flush, lease release | Disposable cache deletion only |

No foreground success depends on recursive graph traversal. Dehydration is the
intentional exception to constant data volume: freeing memory necessarily
writes the branch-private resident state that lacks another authoritative
copy.

## Why the protocol is correct

The implementation must maintain these proof obligations in reviews and model
tests.

### A fork observes one instant

All mutable task, kernel, memory-origin, and filesystem-origin state is
quiesced while the source is paused. Sealing freezes the working origins before
either branch obtains a fresh writable origin. Therefore every object in the
fork roots denotes pre-resume bytes, and every later source or destination
write creates branch-local state.

### Resolution returns one immutable version

Tree lookup produces a typed object ID, not a snapshot ancestor. That ID has
one origin. A present provider-map entry names the only volatile authority; an
absent entry is committed only after the shared manifest is durable. Local
caches install complete objects atomically and do not become providers. Thus a
reader selects one immutable version and cannot mix bytes from providers or
partially filled cache files.

### Reclamation cannot delete a live object

Every way to reach an immutable object is either an explicit root or a counted
parent edge. A path-copied parent does not become unprotected until its new
edges are durably activated, while the retained old parent protects the same
children in the interim. The last decrement atomically prevents resurrection
before cascading. Consequently a zero count at the unique owner proves there
is no path from a live root; deleting the object is safe.

### Publication cannot race execution

A publisher operates on an immutable snapshot ID and expected provider entry,
not on a branch's mutable lifecycle/storage generation. Restart, rehydrate, or
another migration uses a new working snapshot ID. The publisher's only Raft
mutation is the exact origin's `node -> shared` authority transition, so it
cannot overwrite newer roots, revoke placement, or change lifecycle.

### Provider handoff has no gap or double authority

The node stays authoritative while packs, manifests, and the shared ledger are
prepared. The final Raft CAS removes the node mapping only after shared
authority acknowledges the exact final reference sequence. Before that CAS all
readers choose the node; after it all new readers choose shared storage. The
bytes are immutable, so in-flight reads crossing the CAS remain valid, while
only the current ledger owner may reclaim.

### Execution remains single-owner

Branch placement remains protected by Raft generation/CAS and node execution
leases. Migration pauses the old executor before cutover and the target cannot
resume before its placement commits. Provider authority does not grant
execution authority. A stale node self-fences before its lease expires.

### Failures become either replay or explicit corruption

Every multi-step mutation has a durable transaction ID and idempotent steps.
After restart TTC completes or rolls back from the last committed boundary.
When a physical node is permanently lost, only origins for which it remained
the unique provider are unavailable; Raft computes the exact dependent branch
closure and marks it corrupted. Disk corruption is outside the model and is
not disguised as protocol recovery.

## Root-format break and startup rules

### New markers

The new root marker retains `.ttc-root-format` and adds explicit schema and
role fields. Proposed versions are:

```text
standalone TTC_ROOT: version 2, role standalone-authority
cluster TTC_ROOT:    version 5, role cluster-shared
TTC_LOCAL_ROOT:      version 5, role cluster-local
schema: snapshot-graph-v1
```

The shared marker also identifies the cluster/repository. The local marker
identifies its cluster, physical node UUID, and local-root ownership epoch.

### Validation order

Daemon startup must perform these checks before opening the daemon socket,
starting workers, joining Raft, or creating runtime processes:

1. require an absolute nonempty `TTC_ROOT`;
2. in cluster mode require a distinct absolute `TTC_LOCAL_ROOT`;
3. resolve symlink and mount aliases and reject shared/local aliasing;
4. inspect existing root markers without modifying their directories;
5. reject standalone version 1, clustered version 4, unknown versions, wrong
   roles, and mismatched cluster/node identities with a fresh-root diagnostic;
6. reject every nonempty unmarked directory except an otherwise empty
   filesystem root containing only `lost+found`;
7. only then create missing roots, atomically install new markers, acquire
   exclusive local-root ownership, and initialize storage.

An example diagnostic is:

```text
ttcd: TTC_ROOT uses incompatible TTC root format 1; this build requires
snapshot-graph format 2 and does not migrate existing roots; select an empty
TTC_ROOT
```

Startup never upgrades a marker in place. A concurrent initializer must either
publish the identical requested marker or lose creation and validate the
winner.

### Removed configuration

Remove `--allow-slow-volume-copy` from daemon grammar, completion, SDK/MCP
schema, tests, and documentation. Each local object repository probes reflink
support and attempts it opportunistically. Unsupported or cross-filesystem
copies transparently use a whole-file copy. Inspection may report the selected
copy mechanism, but startup never fails or warns solely because reflinks are
unavailable.

## Multi-phase implementation

Every phase ends with a reviewable commit and a green phase gate. New-format
code must not silently fall back to an old subsystem.

| Phase | Outcome |
|---:|---|
| 0 | Freeze invariants and capture old-format correctness/performance baselines |
| 1 | Enforce the incompatible root/API boundary and simplify configuration |
| 2 | Build the common ID, persistent-tree, journal, and reference core |
| 3 | Move standalone images and writable filesystems to the new graph |
| 4 | Move standalone memory, dehydration, and lifecycle to the new graph |
| 5 | Add Raft origin routing and the unified distributed resolver |
| 6 | Add shared random-access images and complete-file node caching |
| 7 | Add peer demand paging and asynchronous shared snapshot publication |
| 8 | Add cross-owner references, authority handoff, shared GC, and compaction |
| 9 | Integrate elastic placement, pressure migration, drain, and failure policy |
| 10 | Remove legacy paths and complete stressed release qualification |

### Phase 0: specification, baselines, and destructive boundary

Deliverables:

* land this plan and a machine-readable invariant checklist;
* capture current fork, stop/start, dehydration, image start, package-manager,
  NFS/Lustre, and storage-residue baselines;
* inventory every reader/writer of old container JSON, `ttcvolume`, `ttcchunk`,
  `ttcpack`, publication queues, and shared event/idempotency files;
* define API/storage feature names and reserve the new root versions; and
* add a build tag or test-only switch that proves no new-format test opens old
  storage code.

Gate: existing tests pass and benchmark inputs/results are recorded before any
data-path replacement.

### Phase 1: root version, API break, and configuration cleanup

Deliverables:

* implement standalone format 2 and clustered format 5 markers and validation
  order;
* reject old/nonempty unmarked roots without mutation;
* add root role, repository, node, and schema checks;
* remove `--allow-slow-volume-copy` and add automatic reflink fallback;
* bump daemon API/storage feature versions and update all SDKs and MCP schemas;
* introduce new JSON fields for internal branch ID, snapshot roots, provider
  dependency bytes, storage authority, and publication progress; and
* preserve client-local help/version and build-mismatch daemon-stop behavior.

Gate: a table-driven startup suite covers nonexistent, empty, old standalone,
old clustered, future, malformed, aliased, wrong-role, NFS-shared, duplicate
local-owner, and concurrent initialization cases. Old roots remain byte-for-
byte unchanged after rejection.

### Phase 2: common IDs, trees, and reference engine

Deliverables:

* monotonic snapshot and per-snapshot object allocators;
* typed PageID, FileObjectID, inode/tree-node ID, and logical root types;
* persistent memory radix tree and filesystem inode/directory B-trees;
* local journal/checkpoint interface with idempotent transactions;
* hierarchical edge activation/deactivation and explicit root pins;
* zero-count cascading GC and rebuild from roots/edges; and
* graph inspection/debug tooling without exposing physical host paths.

Gate: property tests generate long random sequences of fork, path-copy,
hardlink, open-unlink, mmap, pin, remove, and name reuse. After every injected
crash and replay, computed reachability equals positive-count objects, no live
object reaches zero, and all unreachable objects eventually do.

### Phase 3: standalone filesystem and image conversion

Deliverables:

* new fully extracted standalone image store;
* rootfs, managed volumes, copied mounts, and persistent writable mounts on the
  common whole-file graph;
* path-copy metadata updates and whole-file first-write CoW;
* reflink-then-copy materialization with truncate/delete fast paths;
* metadata-based `ttc diff`, `ttc cp`, commit/save/load, xattrs, hardlinks,
  whiteouts, device metadata, and FUSE pins; and
* removal of the old trie/repository path from new-format standalone roots.

Gate: package installation and source-build workloads must have no operation
whose complexity grows with fork-history depth. OCI commit/save/load preserves
deletions and labels. NFS-backed standalone deletion leaves no `.nfs`-specific
logic and no open stale object after forced branch removal.

### Phase 4: standalone memory and lifecycle conversion

Deliverables:

* anonymous resident PageID-backed memory and page-granular CoW;
* fixed-height memory roots shared by batch local fork;
* compact 1-MiB dehydration records with atomic all-slot installation;
* process-free dehydrate/rehydrate preserving shared PageIDs;
* stop/restart/fork-dehydrated and `cp`-dehydrated on immutable roots;
* complete kernel, tmpfs, socket, timer, console, named-exec, and FUSE root
  enumeration; and
* removal of legacy `application.mem`, per-page loose chunks, and old memory
  consumption sidecars from new-format roots.

Gate: hundreds of branches repeatedly fork, write disjoint and overlapping
pages, dehydrate in different orders, rehydrate, stop, restart, remove, and
reuse names. Private/shared accounting matches actual PageIDs, resident memory
returns to baseline after removal, and daemon restart loses no dehydrated or
exited state.

### Phase 5: distributed provider directory and snapshot protocol

Deliverables:

* Raft snapshot-ID allocation and volatile-origin provider map;
* provider leases/incarnation fencing and corruption closure on provider loss;
* branch transactions expressed in immutable snapshot roots rather than
  storage generations;
* unified resolver interfaces for memory, files, image files, and auxiliary
  state;
* peer whole-object/file and 1-MiB memory-record RPCs on the multiplexed cluster
  port; and
* same-family target branches installed into one Sentry per node.

Gate: local and two-node model tests prove there is never more than one
execution owner, absence from the provider map implies a committed shared
manifest, and repeated A-to-B-to-C migration routes each object directly to its
origin rather than through a proxy chain.

### Phase 6: shared images and complete-file node cache

Deliverables:

* one-off OCI-to-uncompressed-indexed-pack conversion during pull/load;
* shared image catalog governed by Raft image/family references;
* background sequential extraction plus on-demand whole-file extraction;
* atomic complete-file cache installation, request coalescing, capacity
  eviction, and cache cleanup after image removal; and
* image start paths that require metadata/index only, not complete extraction.

Gate: with an empty local cache and a throttled shared filesystem, a container
whose entrypoint files are small begins without waiting for total image size.
Random first access returns exact whole-file contents, concurrent requests
perform one fetch, and cache deletion never affects a running/open file.

### Phase 7: peer data plane and asynchronous shared publication

Deliverables:

* remote fork/migration time-to-execution independent of shared-pack creation;
* peer-served immutable memory records and whole filesystem objects;
* snapshot pack writer, paged index, manifest, and structural reader;
* prioritized resumable publication with local durable intent;
* stop/dehydrate publication separated from branch lifecycle;
* rehydrate/restart/fork during publication; and
* accurate dependent-memory/file byte statistics and durable wait conditions.

Gate: throttle or pause NFS/Lustre during remote fork and verify the target
executes using peer data. Resume shared storage, reach durability, delete source
scratch after drain, and verify every target. Repeat migration before earlier
publication completes.

### Phase 8: distributed reference ownership and shared GC

Deliverables:

* origin-owned ledgers and durable cross-owner edge outboxes;
* shared-GC coordinator election/lease and rebuild protocol;
* node-to-shared baseline/catch-up/final-barrier handoff;
* zero-count cascade, dead-record tracking, whole-pack retirement, and partial
  pack compaction;
* cache eviction independent of authoritative GC; and
* admin inspection for roots, counts, pending edges, packs, cache, and residue.

Gate: crash source, receiver, Raft leader, GC coordinator, and shared mount at
every handoff boundary. Repeated messages do not double-count. A lost
coordinator stops deletion until rebuild. After recovery and quiescence, exact
graph traversal finds neither missing live objects nor unreachable retained
objects.

### Phase 9: elastic lifecycle integration

Deliverables:

* placement and pressure migration over snapshot roots;
* drain ordering that publishes all owned origins and flushes edge outboxes;
* new-node placement with no eager image/snapshot materialization;
* correct quarantine flag handling through fork and migration;
* shared-filesystem outage fencing and node-loss corruption propagation; and
* scheduler costs based on private/shared PageIDs, filesystem objects, cache
  state, and volatile dependency bytes.

Gate: run hundreds of branches across multiple nodes, trigger memory pressure,
join/retire nodes, interrupt migrations, kill daemons, and restore the shared
filesystem. A retired local root is safely deletable; unaffected branches keep
running; lost volatile authority produces only the exact corrupted dependency
closure.

### Phase 10: removal, qualification, and documentation

Deliverables:

* delete old root parsers, volume trie, sparse-file readers, legacy publication
  paths, content-digest TTC chunk naming, and compatibility projections;
* consolidate all lifecycle commands on the snapshot protocol;
* update README, user guide, limitations, development docs, all SDK READMEs,
  MCP tools, CLI completion, and examples;
* replace superseded proposals with short historical pointers to this design;
* publish benchmark and failure-injection results; and
* build and install final static `ttc` and `ttcd` binaries.

Gate: full systrap, NFS, Lustre, nested Docker/TTC, package-manager, browser,
filebench, database, FUSE/AppImage, TTY/REPL, named-session, fork eligibility,
deletion, and remote/elastic suites pass. KVM is not required for this
refactor's qualification. Static client dependency and size audits remain
green.

## Test strategy

### Deterministic model tests

Use a small in-memory reference model as the oracle for:

* immutable tree path-copy and root sharing;
* edge activation interruption at every child;
* local and cross-owner reference messages;
* provider `node -> shared` transition;
* branch deletion and immediate name reuse;
* publication followed by restart/rehydrate/second migration; and
* pack compaction without ID changes.

Random tests should use thousands of operations and minimize failing traces.
Assertions compare logical roots, bytes, references, authority, and reclaimable
sets after every step.

### Crash matrix

Inject failures before and after every fsync, rename, journal append, Raft
proposal/commit, provider response, edge acknowledgement, manifest publish,
handoff barrier, and pack-retirement step. Test both process crash and loss of
the entire local root. Recovery must choose one documented state; it may leak
temporarily but may never expose partial data or reclaim a live object.

### Filesystem matrix

Run standalone and distributed data roots on:

* local ext4 without reflink;
* local XFS or btrfs with reflink;
* NFS with silly-rename behavior;
* Lustre; and
* throttled/high-latency shared mounts.

Reflink and non-reflink results must be semantically identical. Tests monitor
host open descriptors and prove forced removal closes guest-derived object
descriptors before zero-count reclaim; there is no `.nfs*` exception in GC.

### Performance gates

Record distributions, not only averages, for:

* source pause and total local/batch/remote fork latency;
* target time-to-first-instruction with cold caches;
* memory page-fault and 1-MiB fetch latency;
* hot and first-access file read/write latency by file size;
* directory lookup/mutation at high entry counts;
* package installation and source builds;
* dehydration capture throughput and rehydration working-set latency;
* shared publication bandwidth and operations per GiB;
* refcount backlog, GC throughput, compaction write amplification;
* cache hit rate and eviction latency; and
* final disk/RSS residue after long fork/delete churn.

Required asymptotic checks deliberately grow resident memory, file count,
directory width, fork depth, and branch count independently. Fork latency may
not grow with resident memory or recursively with files. Lookup latency may not
grow with fork or migration depth.

## Observability and user-visible state

`inspect`, `stats`, events, and SDK/MCP types should expose stable logical
information, not internal paths:

* branch lifecycle and placement node;
* working and sealed snapshot IDs;
* logical memory/filesystem roots;
* storage authority: local node, publishing, or shared;
* provider dependencies and dependent memory/file bytes by node;
* publication and durable-wait progress/errors;
* private, same-node-shared, remotely backed, shared-durable, and cached bytes;
* reference/GC backlog and cache pressure at node level; and
* corruption reason when unique volatile authority is lost.

Human CLI output may summarize these fields. JSON schemas are versioned and all
four SDKs plus MCP are updated in the same phase as their daemon API change.
No output exposes an assumption that a snapshot belongs permanently to one
branch or that `main` is structurally special.

## Completion criteria

The refactor is complete only when:

1. old roots are refused and never modified;
2. every new-format lifecycle operation uses the immutable snapshot graph;
3. standalone TTC contains no Raft/cache/shared-pack dependency;
4. clustered lookup is origin-directed and ancestry-depth-independent;
5. local fork/seal is O(1) in pages and recursive file count;
6. whole-file CoW works with and without reflink automatically;
7. every live immutable object has a positive authoritative count and every
   zero-count object is eventually reclaimed;
8. publication cannot mutate a newer branch lifecycle or execution;
9. a drained node's local root is disposable;
10. stressed standalone, NFS, Lustre, remote, and elastic qualification passes;
    and
11. legacy storage implementations and stale documentation are removed rather
    than left as hidden fallbacks.
