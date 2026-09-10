# TTC tiered-storage and data-path audit

The follow-up
[snapshot-graph storage refactor](../proposals/ttc_snapshot_graph_refactor.md)
is the authoritative replacement plan. This document remains the audit of the
pre-refactor implementation and the evidence for that plan.

Status: audit of commit `8f7a3a327` (2026-09-09). This document records
current behavior and design requirements. It does not prescribe the replacement
architecture yet.

Audit method: the review traced data placement, authority, lifecycle, transfer,
recovery, and garbage collection across the CLI/daemon, bundle and image stores,
Raft control plane, remote-fork protocol, TTC volume layer, sparse memory layer,
pack format, publication queue, SDK-visible state, console, and named-exec
storage. The existing targeted suites for `ttcchunk`, `ttcpack`, `ttcvolume`,
bundle, images, and publication storage all pass; the findings below are gaps in
the behavior and invariants those suites currently specify.

## Executive summary

TTC has most of the *pieces* of tiered storage, but not yet a consistently
demand-driven tiered data plane. Active writable filesystems and dehydration
captures are node-local; OCI blobs and immutable cold packs are shared; memory
and remote filesystem data can be fetched lazily in 1 MiB units. Those are good
foundations.

The current paths nevertheless have four critical gaps:

1. Starting a stopped branch while its publication is pending does not fence
   the old publication generation. A stale worker can later publish and unplace
   the now-running branch.
2. Starting a durable stopped branch on a node without its old local repository
   installs sparse object files but does not attach a shared-pack data source.
   Reads can therefore return zero-filled holes.
3. A new node must fully extract the OCI image before `run`, rehydrate, remote
   fork, or migration can execute. Remote fork also synchronously writes
   filesystem metadata to shared storage before target preparation.
4. Successful publication leaves complete local filesystem and memory copies
   indefinitely. There is no capacity-bounded, partially resident cache or
   eviction policy.

The most important conclusion is that TTC needs a single data-resolution
contract shared by images, writable filesystems, memory, and auxiliary branch
state. Today each subsystem has its own partial interpretation of `local`,
`remote`, and `shared`, so lifecycle operations cannot atomically fence all the
data sources on which a branch depends.

## Required invariant

“The local root is only a cache” should be interpreted operationally rather
than literally. While a branch is running, its newly written data is necessarily
authoritative on that node until another durable copy exists. A node also needs
a local Raft WAL and a publication queue. Every local object must therefore be
in exactly one of these classes:

| Class | May be the only copy? | May be evicted? | Required protection |
|---|---:|---:|---|
| Read cache | No | Yes, at any time when not open | Immutable object identity and structural bounds |
| Active writable authority | Yes, while leased | No | Raft owner/incarnation and branch mutation fence |
| Sealed publication staging | Yes, temporarily | No | Durable local intent and snapshot identity |
| Replicated control log | Yes on one member, replicated by Raft | No while member is active | Raft durability rules |

Anything else under `TTC_LOCAL_ROOT` is residue. Once a branch's durable roots
are committed, its local files must become explicitly evictable cache. Once a
node has drained, no unique active or staging authority may remain.

Shared storage should contain immutable content-addressed data, committed
manifests, and a small number of low-frequency roots. Guest reads and writes,
per-page updates, per-command event fsyncs, and whole-cluster scans must not be
normal shared-filesystem traffic.

Checksums must not substitute for protocol correctness. TTC assumes reliable
host storage rather than attempting to detect latent disk corruption. Immutable
snapshot UUIDs, exact lengths, atomic publication, and Raft-fenced pointer
updates should establish correctness. OCI-mandated digests and checks internal
to third-party protocols may remain, but TTC should not hash bulk data on its
write or read path merely to validate its own synchronization.

## Current placement inventory

| Data | Current placement | Current granularity and behavior |
|---|---|---|
| OCI manifests, configs, and compressed layers | Shared `TTC_ROOT/image-store` | Content-addressed blobs, plus one mutable JSON catalog and per-family catalog leases |
| Extracted image root | Local `TTC_LOCAL_ROOT/images` | Entire image tree is extracted before use and retained while the image exists globally |
| Container identity, branch array, OCI spec | Shared `TTC_ROOT/containers` | Mutable per-family JSON and bundle files; every node reads and writes them directly |
| Active rootfs upper, managed volumes, copied host mounts | Local repositories | Metadata journals and one opaque host file per regular-file generation |
| Live application memory | Anonymous memfd | Resident and page-CoW; correctly not disk-backed |
| Dehydrated memory control and chunks | Local container directory, then shared packs | 1 MiB logical chunks; publication is asynchronous after local capture |
| Remote live-fork memory | Source TCP, then shared loose chunk repository | Target faults on demand; background handoff writes content-addressed chunks directly to shared storage |
| Cold filesystem and memory generations | Shared `cold` packs | Immutable, indexed, checksummed; data can be read by range |
| Cold filesystem cache | Local sparse repository objects | 1 MiB ranges are filled on demand only when a live remote/shared reader is attached |
| Console logs and named-exec buffers | Local container directory | Not included in cold branch publication |
| Publication intents | Local publication queue | Durable generation-fenced JSON; one worker per daemon |
| Mutation replay records and events | Shared `TTC_ROOT` | Synchronously rewritten/appended for normal commands |
| Raft WAL | Local cluster directory | Correctly node-local and replicated through Raft |

## Findings

Severities describe risk to the requested elastic data plane, not release
severity for TTC as a whole.

### P0: correctness and architectural blockers

#### TS-001: stop followed by immediate start is not generation-fenced

`stopBranchLifecycle` destroys the runtime and queues a `stop` publication
(`runsc/ttc/cmd/cmd.go:4496-4532`). `queueBranchPublication` assigns a storage
generation and creates a hidden filesystem branch
(`runsc/ttc/cmd/cmd.go:5526-5685`). A subsequent `start` accepts the `exited`
branch and launches it without canceling or superseding that publication
(`runsc/ttc/cmd/cmd.go:6250-6337`). `RecordBranchStart` changes the storage state
to `local`, but does not advance `StorageGeneration`
(`runsc/ttc/bundle/store.go:3639-3659`).

The old worker can consequently pass its generation check, commit the stopped
snapshot, set storage to `shared`, and unplace the branch after new guest writes
have begun (`runsc/ttc/cmd/storage.go:540-571` and
`runsc/ttc/bundle/store.go:3407-3433`). Depending on Raft timing this either
fails late and retries forever or makes stale storage metadata authoritative
for a running branch.

Required invariant: before any guest instruction executes, activation must
atomically acquire a new execution/storage epoch and fence every older
publication. Publication completion must compare lifecycle epoch, storage
generation, and owner incarnation in one authoritative transition.

#### TS-002: a cold stopped branch can start with zero-filled file data

`ensureLocalColdFilesystem` materializes only repository metadata and creates
sparse local object files (`runsc/ttc/cmd/storage.go:844-941`). This is the right
shape for lazy startup. However, the ordinary `start` path launches immediately
after that import and never calls `prepareColdFilesystemRelays`
(`runsc/ttc/cmd/cmd.go:6298-6332`). Cold *rehydration* does attach those relays,
so the two paths have inconsistent semantics
(`runsc/ttc/cmd/cmd.go:11217-11330`).

An imported remote object is created at its logical length and marked pending
(`pkg/ttcvolume/snapshot.go:302-390`). If no `remoteObjectClient` is attached,
`ensureObjectRange` returns success without filling it and `ReadAt` reads the
sparse host file (`pkg/ttcvolume/remote.go:345-388` and
`pkg/ttcvolume/repository.go:1507-1541`). On a new node, this exposes zeros in
place of durable contents.

Required invariant: a local object with a pending marker must never be readable
without at least one fenced provider. Missing providers must fail closed, and
every launch path must install providers before the branch becomes runnable.

#### TS-003: branch auxiliary state is not part of durable branch publication

Console logs, retained output generations, named-exec buffers, PID/wait files,
and retention statistics live under the local active container directory
(`runsc/ttc/bundle/store.go:4574-4675`). `branchPublisher.Publish` publishes TTC
volume repositories and, for dehydration, memory control/chunks; it does not
publish console or named-exec output (`runsc/ttc/cmd/storage.go:112-233`). It
then unplaces the branch.

After node retirement or cache deletion, `ttc logs` and reattached named exec
sessions cannot recover their promised durable output. This also means that
“all writable branch state” is not represented by the committed roots.

Required invariant: the branch durable root must cover every persistent
branch-visible byte, or the API must explicitly classify a datum as ephemeral.
Named reattachable sessions and retained output are not ephemeral.

#### TS-004: publication is retryable but neither interruptible nor resumable

The queue invokes one `Publish(context.Background(), ...)` at a time
(`runsc/ttc/storage/queue.go:267-345`); `branchPublisher.Publish` ignores its
context. `Queue.Close` waits for the current call to finish. Cancellation fences
the result but cannot stop the I/O. A large pack interrupted by process death is
rewritten from byte zero on retry.

`ttcpack.WriteDirectory` creates the temporary pack directly on shared storage,
fsyncs it, then rereads the entire shared file to compute its name
(`pkg/ttcpack/pack.go:224-302`). Process death may leave `.pack-*.tmp`, which
pack GC ignores because it scans only `*.pack`. Thus a slow or unavailable
shared filesystem can delay daemon shutdown, cancellation, unrelated
publications, and node retirement.

Required invariant: transfer progress must be restartable at independently
committed extents; cancellation must stop scheduling new I/O promptly; local
sealed authority must remain valid until a committed manifest makes all extents
reachable elsewhere.

#### TS-005: remote fork is still synchronously dependent on shared storage

After the source is captured, `prepareDistributedFilesystem` exports complete
filesystem metadata and calls `ttcpack.WriteDirectory` in shared `TTC_ROOT`
before `Prepare` is sent to the target (`runsc/ttc/cmd/distributed.go:426-487`
and `:780-845`). The target then fully materializes that metadata pack before
restoring (`runsc/ttc/cmd/daemon.go:367-381`). It also calls
`RefreshContainerImageRoot`, which fully extracts the image if absent locally.

Consequently target time-to-execution grows with shared-filesystem latency,
image size/file count, and filesystem metadata size. Throttling NFS or Lustre
does affect the critical path, contrary to the intended protocol.

In batch remote fork, `prepareDistributedFilesystem` is called separately for
each child after the common capture (`runsc/ttc/cmd/distributed.go:1330-1400`).
Identical packs deduplicate only after each child has walked, serialized,
written, fsynced, and hashed its own copy.

Required invariant: target execution may depend initially on a transaction-
fenced source, but never on completion of shared publication. One sealed
metadata generation must be reusable by every sibling.

#### TS-006: remote handoff writes hot loose objects directly to shared storage

The background sparse handoff opens `TTC_ROOT/cluster/memory-chunks` and donates
that directory to the Sentry (`runsc/ttc/cmd/distributed.go:641-675`). The Sentry
publishes individual chunk, manifest, branch, transaction, and synchronization
files into the shared repository. This avoids delaying the initial fork, but
turns page consolidation into frequent shared-filesystem metadata operations
and makes every daemon's memory GC scan that shared loose-object repository.

This is particularly hostile to Lustre and large NFS clusters and contradicts
the rule that the shared tier contains immutable packs plus low-frequency
roots, not per-page updates.

Required invariant: remote-source handoff first produces sealed local chunks;
shared publication batches them into resumable immutable extents/packs. A
target may continue using peer sources while that happens.

### P1: major scalability and latency problems

#### TS-007: image use is all-or-nothing materialization

Every `Prepare` resolves the image and fully extracts all layers before creating
the container (`runsc/ttc/bundle/store.go:868-940`).
`RefreshContainerImageRoot` does the same for start, rehydrate, and distributed
target preparation (`runsc/ttc/bundle/store.go:2671-2725`). The extracted tree
is published only after completion, so there is no partial cache that another
attempt can reuse.

Even `ttc pull` calls `EnsureImageRoot`, making a registry/catalog operation
also fill the executing node's entire local rootfs cache
(`runsc/ttc/cmd/cmd.go:9090-9115`). A 20 GiB image therefore incurs 20 GiB-class
work before a one-file command can start.

#### TS-008: extracted image cache has no eviction and retains every global image

`PruneImageRoots` marks every image in the shared catalog as locally required,
not merely images used on this node (`runsc/ttc/bundle/store.go:4463-4525`). It
only removes roots after global image deletion or format replacement. There is
no size target, admission policy, LRU, open-reference pin, or partial-tree
accounting.

Thus local disk use approaches the total extracted size of every image ever
pulled cluster-wide, on every node that happened to pull/use it.

#### TS-009: the native image store globally serializes long operations

`openNativeStore` always takes an exclusive flock on the shared image-store
lock (`runsc/ttc/images/native_store.go:56-91`). `Pull` retains it across registry
resolution, download, metadata inspection, catalog update, and garbage
collection (`runsc/ttc/images/images.go:260-366`). `Load` retains it while
streaming an archive. Even list and resolve take the exclusive lock.

Creating or deleting a container also rewrites the shared image catalog to add
or remove a family lease. Lease removal immediately traverses every reachable
OCI graph, walks all blobs, and deletes garbage
(`runsc/ttc/images/images.go:1008-1076` and
`runsc/ttc/images/native_store.go:175-229`). One slow pull/save/GC can block all
image commands and container creation cluster-wide.

#### TS-010: image catalog mutations are O(catalog size)

Images and all family leases share one `catalog.json`. Every mutation marshals,
fsyncs, renames, and directory-fsyncs the entire catalog
(`runsc/ttc/images/native_store.go:137-173`). The lease count grows with the
number of families, even though an immutable image digest could be retained by
the authoritative branch/family catalog without one mutable image-store entry
per family.

#### TS-011: shared container metadata is a hot O(families + branches) path

Container records and OCI configs are stored in shared `TTC_ROOT/containers`.
Resolving a normal container name scans and decodes all containers because
directories are keyed by random ID (`runsc/ttc/bundle/store.go:2253-2295` and
`:2738-2795`). Branches are a slice inside one family JSON object. A state,
quarantine, dependency, parent, statistics, or storage update takes an NFS
flock and rewrites the complete family record
(`runsc/ttc/bundle/store.go:2313-2339`).

For thousands of families or tens of thousands of branches, routine commands
become O(total catalog) or O(family size), and hot metadata writes bypass the
Raft state machine that already owns cluster authority.

#### TS-012: command idempotency and event logging are synchronous shared-FS traffic

Most mutations write a pending request JSON and then a second completed JSON,
including buffered stdout/stderr, under shared `TTC_ROOT/daemon/requests`
(`runsc/ttc/cmd/idempotency.go:145-197`). Retention compaction scans the whole
directory after every completion. The coordinating lock is process-local, so
it does not itself serialize two daemons.

Every recorded event appends and fsyncs `TTC_ROOT/events/events.jsonl`; rotation
is protected only by a daemon-local mutex (`runsc/ttc/cmd/events.go:170-202`).
Multiple nodes can concurrently append or rotate the same files, risking lost
history as well as imposing shared latency on foreground commands.

#### TS-013: cold manifest chains grow without compaction

Each publication points to its previous manifest. Data objects are delta-packed,
but full namespace metadata is emitted each time. No implementation enforces
the planned chain-length or reclaimable-byte compaction thresholds.

Publication walks every ancestor and scans every pack index to compute inherited
object IDs (`runsc/ttc/cmd/storage.go:236-332`). Cold readers open and index the
whole chain (`runsc/ttc/cmd/storage.go:975-1011`). GC traces every reachable
manifest (`runsc/ttc/cmd/storage.go:1179-1268`). Cost therefore grows with
lifecycle history even if the current working set is small.

#### TS-014: filesystem snapshots are proportional to total metadata, not changes

`ExportRemoteSnapshot` and `ExportSnapshotDelta` resolve and serialize every
inode and directory entry. “Delta” applies only to regular-file objects; the
namespace snapshot remains complete (`pkg/ttcvolume/snapshot.go:52-118` and
`:121-225`). Every stop/dehydrate/remote fork of a million-file branch must walk,
sort, encode, and later decode its million-file namespace even if no path
changed.

Before packing, changed filesystem objects are also copied or reflinked into a
local staging tree, then read again by the pack writer. In full-copy mode this
temporarily duplicates every changed file and adds an avoidable full local I/O
pass.

#### TS-015: pack index construction is quadratic in paths

`ttcpack.OpenDirectory` first collects all paths in a pack, then loops over the
entire entry array again for each path (`pkg/ttcpack/pack.go:120-155`). With many
single-extent files this is O(N^2). Publication inheritance and lazy cold
startup both open these indexes, so large source trees can become unusable even
when little data is read.

`Directory.ReadAt` also allocates and reads a complete extent (up to 1 MiB) for
each requested subrange (`pkg/ttcpack/pack.go:160-189`). A 1 MiB cache/fetch unit
is intentional, but buffers should be pooled and the fetched extent should be
installed once in a node-local range cache rather than repeatedly allocated.

#### TS-016: one publication worker creates global head-of-line blocking per node

`Queue.process` handles all pending branches sequentially. One huge filesystem,
retrying NFS error, or stuck fsync delays unrelated stop/dehydrate durability.
Exponential retry is per intent, but there are no independent bounded workers,
per-branch ordering lanes, bandwidth scheduling, or priority for node drain.

#### TS-017: source/target dependency draining is synchronous and serial

Stopping, deleting, or dehydrating a source calls
`ensureDistributedSourceIndependent`, which drains matching targets one by one
and may wait two minutes for each (`runsc/ttc/cmd/daemon.go:1651-1728`).
Dehydrating a remote target additionally forces all outstanding memory and
filesystem bytes to become resident and polls for up to five minutes
(`runsc/ttc/cmd/cmd.go:10853-10930`).

This is safe but defeats the desired constant-time lifecycle transition. The
operation should be able to seal local authority and hand dependencies to a
durable provider without eagerly loading every byte.

#### TS-018: repeated migration forms transitive live-provider chains

Migrating `A -> B -> C` before the first handoff settles is possible in
principle: B serves C and faults missing data from A. However, provider state is
single-source and process-local (`pkg/ttcvolume/remote.go:213-254` and
`pkg/sentry/ttcchunk/remote.go:96-148`). The chain pins every predecessor,
amplifies a C fault into multiple network round trips, and has no direct fallback
to a shared copy that appears later.

Filesystem extent progress is held in `remoteObjectClient.fetched`; the durable
marker says only “some part of this object is remote.” After daemon/Sentry
recreation, already fetched ranges may be fetched again. The protocol is
generation-safe but not efficiently resumable.

#### TS-019: active filesystem writes are repository-serialized

Every `ttcvolume.WriteAt`, including writes to unrelated files and branches,
takes one process mutex plus an exclusive repository flock
(`pkg/ttcvolume/repository.go:650-664` and `:1461-1505`). First write after a
fork fetches an entire remote object, then reflinks or copies the entire host
file before modifying it (`pkg/ttcvolume/repository.go:1430-1480`).

This preserves file-level CoW but limits multicore/package-manager workloads
and makes one write to a huge inherited file a full-file operation. It is
independent of shared-tier design but will remain a bottleneck after lazy image
startup is fixed.

#### TS-020: maintenance is a cluster-wide scan repeated by every node

Every daemon, every five minutes, scans shared container tombstones, all local
repositories, the shared loose memory repository, all cold manifests, and all
cold packs (`runsc/ttc/cmd/daemon.go:3421-3475`). It holds that daemon's main
lifecycle mutation lock during the complete pass. In an N-node cluster this is
N redundant scans of the shared namespace, often beginning at similar times,
and each scan pauses local lifecycle commands.

Cold GC traces local pending intents but not Raft transaction descriptors or
active reader/source leases (`runsc/ttc/cmd/storage.go:1179-1268`). A pack used
only by an interrupted distributed prepare can age out even though the Raft
transaction remains recoverable in principle.

### P2: bounded leaks, portability, and incomplete abstractions

#### TS-021: local and shared temporary/control files have no complete sweeper

Publication queue `complete`, `canceled`, and `quarantine` records are never
aged out (`runsc/ttc/storage/queue.go:67-75`, `:128-160`, and `:315-345`). Crash
residue in `publication-staging`, `materialization-staging`, distributed
filesystem staging, and `.pack-*.tmp` is removed only if the exact operation is
retried, if at all. Long-running clusters with branch-name reuse will grow even
when all user-visible branches are removed.

#### TS-022: successful publication copies rather than moves the local generation

Cleanup removes the hidden capture branches, but retains the original stopped
filesystem branch, dehydration control directory, memory root, and cached
objects (`runsc/ttc/cmd/storage.go:419-476`). Reuse on the same node is useful,
but there is no transition from “authoritative local” to “evictable cache” and
no reclaimer. This is the direct reason `TTC_LOCAL_ROOT` cannot presently be
treated as a bounded working set.

#### TS-023: named volumes are node-local despite a unified cluster CLI

The volume manager stores its catalog in local `TTC_LOCAL_ROOT/volumes-v2`
(`pkg/ttcvolume/repository.go:86-108`). A named volume created on one node is
not a cluster object that an auto-placed `run` on another node can resolve.
Remote forks privately import branch metadata, but ordinary volume commands and
new placements have no shared identity/placement contract.

#### TS-024: read-only host mounts have no portable content identity

The canonical OCI spec records a host pathname. On another node, TTC rewrites
its own repository and image paths but not arbitrary read-only binds. A remote
target can therefore see missing or different bytes at the same path. Such a
mount must be explicitly node-pinned, declared shared with a verified identity,
or represented as a cacheable immutable snapshot; path equality is not data
identity.

#### TS-025: local cache accounting is descriptive, not controlling

TTC reports several memory/filesystem statistics, but no daemon-wide local
capacity target governs image roots, sparse extents, cold memory chunks,
console buffers, staging, or obsolete generations. There is no unified pin
count, lease, last-access signal, or eviction ordering across these classes.

## Scenario audit

| Scenario | Current result | Required result |
|---|---|---|
| Run an image on a node with an empty local root | Waits for full extraction | Start after required metadata/bootstrap files are available; fault/cache file ranges on demand |
| Remote fork under throttled shared FS | Waits for shared filesystem metadata pack and target image extraction | Execute from peer-fenced memory, image, and filesystem providers; publish in background |
| Batch remote fork | Rebuilds the same filesystem metadata pack per child | Seal once and share one immutable generation/provider set |
| Stop, then immediately restart while publishing | Old worker remains live and may unplace/publish stale state | Activation atomically supersedes the old generation; worker cancellation is prompt and idempotent |
| Start a stopped durable branch on another node | Sparse objects are created without a live provider | Provider installed before execution; no pending extent can read as zero |
| Rehydrate a durable branch on another node | Memory and writable data are lazy, but image is eager | Bootstrap metadata only; all bulk image/filesystem/memory data lazy |
| Migrate again before prior migration settles | Transitive A-to-B-to-C demand chain | Correct immediately, then flatten provider graph and consolidate without pinning A indefinitely |
| Dehydrate a still-dependent remote target | Eagerly drains all missing bytes before capture | Seal dependency identities; process-free state may continue to reference fenced peer/shared providers |
| Daemon dies during publication | Ready local intent retries from the beginning; some temp files remain | Resume completed extents, recover or cancel every staging object, never rescan/rewrite completed data |
| Node drains | Waits for serial dependencies/publications | Prioritized parallel consolidation; finishes only when no unique authority remains |
| Shared FS is unavailable | Foreground paths that touch catalogs/packs can block/fail | Active execution continues within lease policy; new durability visibly queues without corrupting authority |
| Successful cold publication | Full local generation remains indefinitely | Local copy becomes evictable, partially resident cache governed by node capacity |

## Positive foundations to retain

- Live resident application memory is anonymous and remains page-CoW; it is not
  bound to disk throughput.
- Active TTC volume writes occur on `TTC_LOCAL_ROOT`, not the shared filesystem.
- Filesystem and memory captures already have immutable identities and
  generation-fenced publication roots, although the identity and fencing roles
  should be separated in the replacement design.
- The local publication intent is fsynced before lifecycle commands return, and
  ready captures are retried after daemon restart.
- Remote memory and regular-file data paths already support 1 MiB demand fetch
  while serving smaller guest reads.
- Remote writes create target-local objects, and shared pack data is immutable.
- Raft now provides a suitable cluster-wide authority for placement,
  incarnation fencing, transactions, and durable roots.
- Container deletion uses namespace tombstones and isolates cleanup failures,
  which is the correct failure-containment shape.

## Systemic causes

The findings reduce to six design mismatches:

1. **Lifecycle epochs and storage generations are separate.** A branch can
   become active without atomically invalidating its old transfer.
2. **Resolvers are subsystem-specific.** Memory has demand providers,
   filesystem restore has them on only some paths, and images have none.
3. **Shared storage is used as both object store and coordination database.**
   Mutable JSON catalogs, locks, events, and loose chunks create hot metadata.
4. **Snapshots identify trees, but publication still walks trees.** Full
   metadata snapshots and history scans make cost depend on total state.
5. **Cache presence is confused with authority.** Local copies have no common
   pin/lease/eviction state, and successful publication never makes them
   reclaimable in practice.
6. **Retry is confused with resume.** Operations are idempotent at their final
   commit but redo large byte transfers after interruption.

## Acceptance tests for the future design

The systematic replacement should not be considered complete until these are
continuous tests rather than one-off demonstrations:

1. **Empty-node launch:** run a multi-gigabyte image on a node with an empty
   local root; time-to-first-instruction must be bounded by bootstrap metadata,
   not image size. Verify local cache contains only accessed ranges.
2. **Stop/start race:** repeatedly stop and immediately start while pausing the
   publisher at every boundary. No old worker may change the active branch's
   owner, lifecycle, generation, or visible bytes.
3. **Cold stopped start:** stop and make durable, delete the original local
   root, start elsewhere, and compare known contents of untouched, sparse,
   zero-filled, hardlink, xattr, and modified files before and after random
   reads/writes.
4. **Chained migration:** execute `A -> B -> C -> D` without waiting for
   independence. Kill or retire each obsolete source as soon as the protocol
   permits and continuously compare guest-visible memory and filesystem state.
5. **Interrupted transfers:** kill source daemon, target daemon, Raft leader,
   and shared-FS client during every extent/publish/manifest boundary. Resume
   without retransmitting already committed extents and end with zero residue.
6. **Throttled NFS and Lustre:** add latency, bandwidth limits, metadata delay,
   and transient errors independently. Initial remote execution must remain
   peer-bound rather than shared-FS-bound; durability status must remain
   accurate.
7. **Cache pressure:** set a small local target, run images and branches much
   larger than it, and verify eviction never removes active/staging authority
   or open extents. After access stops, usage must converge to the target.
8. **High fan-out:** fork hundreds of local and remote siblings from one seal.
   Shared bytes and metadata operations should scale with unique dirty data,
   not sibling count.
9. **Long history:** perform tens of thousands of stop/start/dehydrate/fork/
   remove/name-reuse cycles. Startup, lookup, publication, and GC latency must
   stay bounded by live state, and local/shared unreachable residue must be zero.
10. **Auxiliary state:** migrate/dehydrate branches with active named exec
    sessions and wrapped console logs; verify session identity, unread output,
    retention counters, and reattachment after source-local deletion.
11. **Shared outage:** keep active workloads running while shared storage is
    unavailable, queue sealed state locally, then recover and publish exactly
    once after service returns.
12. **Multi-node concurrency:** concurrently mutate different families, pull
    images, rotate events, compact packs, and collect garbage from many nodes.
    Raft roots and immutable data must be sufficient for correctness without
    shared advisory locks.

## Priority order for the design phase

The design should address groups of findings rather than patching individual
call sites:

1. Define the unified object/provider/authority state machine and make branch
   activation supersede publication atomically (TS-001, TS-002, TS-018).
2. Put images, writable objects, memory chunks, console/session data, and cold
   packs behind the same partial local-cache resolver (TS-003, TS-005-008).
3. Remove shared storage from remote-fork time-to-execution and from hot memory
   handoff (TS-005, TS-006).
4. Replace whole-tree/full-history work with dirty metadata generations and
   compact indexed roots (TS-013-015).
5. Move mutable cluster authority and event sequencing behind Raft or an
   explicitly sharded local service; leave only immutable bulk data on shared
   storage (TS-009-012, TS-020).
6. Add resumable parallel transfer scheduling and a capacity-driven local
   cache reclaimer (TS-004, TS-016, TS-021, TS-022, TS-025).
7. Remove repository-wide data-path serialization and define portable volume
   and host-mount placement semantics (TS-019, TS-023, TS-024).
