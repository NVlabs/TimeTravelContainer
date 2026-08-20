# TimeTravelContainer live container fork

Status: implemented for the documented systrap and KVM scope; unsupported
external resources are rejected by design.

The status and implementation notes in this document are current. Imperative
language in the design and phase sections records the invariants used to build
and review the implementation; it is not an uncompleted roadmap. Distributed
fork and process-free dehydration are documented separately in
[ttc_distributed_live_fork.md](ttc_distributed_live_fork.md) and
[ttc_userspace_extent_store.md](ttc_userspace_extent_store.md).

Follow-on product, mount, volume, exec, attach/detach, image-management, and
Docker-like CLI work is tracked in
[timetravel_ttc_cli.md](timetravel_ttc_cli.md).

## Summary

TimeTravelContainer adds an in-memory operation that forks a paused gVisor sandbox
into two independently runnable branches. The operation clones Sentry kernel
metadata, preserves virtual Linux observables, shares existing physical pages,
and performs page-level copy-on-write (CoW) after either branch writes. It does
not create a checkpoint image or copy resident application memory.

The implemented design hosts all branches of a sandbox in one Sentry
process. Each branch has a distinct `kernel.Kernel` and virtual object graph,
but the family shares the daemon-selected `platform.Platform` and one or more
`pgalloc.MemoryFile`s. This avoids forking the Go runtime, permits reuse of
gVisor's existing private-memory CoW machinery, and optimizes for low fork
latency rather than branch failure isolation.

## Goals

1. Fork a sandbox without checkpoint files or work proportional to resident
   memory size, using rootless systrap by default or KVM when selected for the
   daemon.
2. Preserve the source branch's virtual PIDs, parentage, registers, signal
   state, UTS state, clocks, file descriptions, and internal object topology.
3. Preserve sharing inside a branch while preventing writes from crossing
   between branches.
4. Clone internal pipes, epoll graphs, Unix sockets, timers, and similar Sentry
   objects through the existing state object-graph machinery.
5. Use an immutable image lower and a persistent `ttcvol` upper whose metadata
   roots fork in O(1); shared regular files split on first write using reflinks
   or the explicitly enabled whole-file-copy fallback.
6. Reject unsupported resource creation in an eligible family. Retain a frozen
   pre-fork closure check as an invariant guard before mutation.
7. Make failure atomic from the guest's perspective: failure may leave the
   parent more conservatively write-protected, but must not change its data or
   observable kernel state.

## Original local-fork non-goals

These scoped the first local live-fork milestone. Persistence and cross-node
fork were subsequently implemented by the documents linked above.

* Running branches of one family in different Sentry processes on one host.
* Supporting ptrace or other gVisor platforms beyond systrap and KVM.
* Cloning host-network/hostinet or arbitrary external links, writable gofer
  mounts, host FDs, devices, GPU state, packet mmap rings, KCOV, AIO, or
  io_uring. TTC's own virtual NIC is supported by an explicit parent-retain,
  child-reset policy for external proxy flows.
* Security isolation between branches in the same Sentry process.
* Constant-time cloning of arbitrary kernel metadata. Pause time may scale with
  tasks, VMAs, FDs, pipes/socket queues, filesystem metadata, and allocated
  extents, but not with the number of resident data bytes.

## Existing mechanisms to reuse

### Quiescence and register capture

`kernel.Kernel.Pause()` already applies a `TaskSet` external stop, interrupts
tasks executing application code, and waits for task and asynchronous-I/O
goroutines. `Kernel.ReceiveTaskStates()` pulls full architectural and floating
point state from the platform. The live-fork path must use these mechanisms;
it must not deliver a guest-visible signal.

### Object graph cloning

`pkg/state` already serializes graphs with shared references and cycles. A
metadata-only live clone can stream `state.Save(parent)` directly into
`state.Load(child)` through an `io.Pipe`. The stream is in memory and contains
kernel metadata, pipe/socket buffers, and similar object data, but never
`MemoryFile` page contents.

`Kernel.SaveTo()` must not be called by live fork because it also scans and
saves every `MemoryFile`. Live fork needs a separate `Kernel.CloneMetadata()`
path that invokes the state engine directly and supplies clone-specific
contexts and completion hooks.

### Private application memory CoW

`mm.MemoryManager.Fork()` already marks private PMAs `needCOW`, removes writable
platform mappings, shares the old `MemoryFile` ranges, and copies a page into a
new range on the first write fault. Live fork should extract and reuse these
operations instead of implementing another private-page fault mechanism.

### Systrap execution

The child kernel receives new `MemoryManager` address spaces and platform
contexts when its state graph is loaded and tasks are started. Systrap already
interrupts execution through `platform.Context.Interrupt()` and maps PMA
`memmap.File` ranges into stub address spaces. No initial systrap signal or
assembly changes should be required.

## Architecture decision: an in-process sandbox family

A sandbox family owns the platform and shared page allocators. It contains one
or more independently runnable branches:

```text
SandboxFamily
  platform: systrap.Platform
  memory files: shared pgalloc.MemoryFile owners
  branches:
    Branch A -> Kernel A -> TaskSet/VFS/netstack A
    Branch B -> Kernel B -> TaskSet/VFS/netstack B
```

Virtual PIDs can be identical because they belong to distinct Sentry Kernels.
The branch ID exists only in the host control plane and is never exposed to the
guest.

This requires lifetime ownership to move above `kernel.Kernel`: currently
`Kernel.Release()` destroys its main `MemoryFile`, which is invalid when other
branches share it. `MemoryFile` needs an owner reference count, distinct from
its existing per-page reference counts. Creation starts with one owner;
creating a branch acquires an owner; releasing a branch drops it; final owner
release performs today's `Destroy()` operation.

The systrap and KVM platforms can be shared because they create independent
address spaces and contexts. Tests must prove that two started Kernels can
safely use the same `platform.Platform` concurrently.

## Core invariants

1. **Frozen source:** no source task or asynchronous operation can mutate the
   cloned graph between full-state capture and publication of the child.
2. **No writable cross-branch alias:** a physical page may be referenced by
   multiple branches only when all corresponding guest mappings are read-only,
   or when writes are intercepted by a branch-aware owner before they reach the
   page.
3. **Within-branch alias preservation:** mappings, FDs, and processes that
   shared one object before fork share one cloned object within each branch.
4. **Balanced backing references:** every `MemoryFile` range referenced by the
   child graph gains the same page references that its parent counterpart owns.
5. **Private destination:** breaking CoW changes only the faulting branch's PMA
   or branch-local shared object.
6. **Unpublished child:** child task goroutines cannot run before validation,
   reference acquisition, CoW preparation, and control-plane registration
   complete.
7. **No disk dependency:** metadata flows through memory and data pages remain
   in shared or newly allocated `MemoryFile` ranges.

## New top-level data structures

Names are provisional, but ownership boundaries are not.

```go
// kernel package; not guest-visible.
type BranchID uint64

// runsc/boot package.
type SandboxFamily struct {
    mu       sync.RWMutex
    platform platform.Platform
    branches map[kernel.BranchID]*Branch
    nextID   kernel.BranchID
}

type Branch struct {
    id       kernel.BranchID
    kernel   *kernel.Kernel
    runtime  *branchRuntime // exec/wait/control metadata currently held by Loader
    state    branchState    // creating, paused, running, destroying
}

// kernel package; passed through context during state.Load.
type LiveForkSession struct {
    ParentID   BranchID
    ChildID    BranchID
    Role       LiveForkRole
    MemoryFiles map[checkpoint.ResourceID]*pgalloc.MemoryFile
    Rejects    []LiveForkRejection
}
```

`LiveForkSession` is runtime-only. Clone-aware `afterLoad` hooks use it to
distinguish live clone from ordinary checkpoint restore and to acquire backing
references that ordinary restore obtains by loading a new `MemoryFile`.

## Required changes by subsystem

### `pkg/sentry/kernel`

Add:

* `Kernel.LiveFork(ctx, opts) (*Kernel, LiveForkStats, error)`.
* A metadata-only `cloneStateTo` helper built on `state.Save`/`state.Load`.
* `Kernel.StartPaused()` so restored tasks can create goroutines while an
  external stop is already active and timers remain frozen.
* A refactored runtime-state initializer shared by `LoadFrom` and live clone
  for condition variables, ticker channels, and other `state:"nosave"` fields.
* A graph/resource validation pass that runs before any child is created.
* Branch-aware completion and release hooks.

Do not add live fork to `Kernel.SaveTo`; disk checkpoint and live clone have
different lifetime and rollback semantics.

`TaskSet.stopCount` and `Task.stopCount` are intentionally not serialized.
The child must therefore receive an external stop before `StartPaused()` starts
its task goroutines.

Checkpoint-only package-global state requires audit. Registered loopback UDP,
established internal TCP endpoints, and the TTC virtual-NIC hooks pass
recursive-fork tests. The TTC gateway/proxy is runtime-only rather than
serialized: each child reconstructs a fresh realm and resets inherited external
TCP endpoints. All remaining package-global registries must still reject unless
they can be proven Kernel-local.

### `pkg/state`

Start without changing the wire format. Add a small `state.Clone` helper only
if it materially simplifies error propagation between a streaming encoder and
decoder. The required behavior is:

1. `io.Pipe` between parent encoder and child decoder.
2. Cancellation closes both ends with the first error.
3. No intermediate `[]byte` proportional to metadata size.
4. Existing shared-reference/cycle identity semantics remain unchanged.

Do not teach `pkg/state` about memory pages or branches.

### `pkg/sentry/pgalloc`

Add owner lifetime management to `MemoryFile`; retain existing page reference
counting and allocator locking.

Add debug-only/reference-audit support that can report ranges and reference
counts before and after live fork. Recursive fork and branch destruction must
be tested with this enabled.

The first implementation shares the same `MemoryFile` allocator between
branches. Newly copied pages are allocated at distinct offsets by the existing
thread-safe allocator.

### `pkg/sentry/mm`

Extract the private-PMA preparation from `MemoryManager.Fork()` into reusable
locked helpers:

* mark private PMAs `needCOW`;
* remove write permission from PMA effective/max permissions;
* unmap writable ranges from the parent's platform address space;
* retain the existing first-write allocation/copy path.

The parent is prepared before metadata serialization, so the child graph
inherits `needCOW`. Since the child's platform address spaces are new, it has no
old writable host mappings to invalidate.

During clone-mode `MemoryManager.afterLoad`, increment `MemoryFile` references
for every restored PMA that the child retains. Ordinary restore must keep its
current behavior because it loads allocator reference counts from the image.

For the first milestone, discard regenerable non-private PMAs from the clone
unless their backing owner explicitly supports live fork. This reduces the
reference inventory and forces safe retranslation after child resume.

### `pkg/sentry/vfs`

Add an optional extension separate from checkpoint save/restore:

```go
type FilesystemImplLiveForkExtension interface {
    ValidateLiveFork(ctx context.Context, report *LiveForkReport) error
    PrepareLiveForkParent(ctx context.Context, session *LiveForkSession) error
    CompleteLiveForkChild(ctx context.Context, session *LiveForkSession) error
    AbortLiveForkParent(ctx context.Context, session *LiveForkSession)
}
```

The exact interface may split validation from mutation, but all filesystems
must have an explicit disposition: clone, share immutable lower state, or
reject. Absence of an implementation is rejection, not implicit support.

### `pkg/sentry/fsimpl/tmpfs`

Tmpfs is the key to filesystem and shared-anonymous CoW. `regularFile.data`
currently maps file offsets directly to writable ranges in one `MemoryFile`.
State cloning would give two `regularFile` objects that still reference the
same pages, allowing cross-branch writes.

Add a side range set, provisionally `cowRanges`, marking logical file ranges
whose physical backing is shared with another branch. On live fork:

1. The child increments references for every range in its copied `data` set.
2. Parent and child mark all present data ranges as CoW.
3. Parent invalidates existing potentially writable mappings before resume.
4. Read translation of a CoW range returns no write permission.
5. `Translate(at.Write)`, `PWrite`, truncate tail-zeroing, hole punching, and
   other modifying paths call `breakCOWLocked` first.
6. `breakCOWLocked` allocates new ranges, copies old bytes, replaces only this
   `regularFile.data` mapping, drops this branch's old reference, clears its CoW
   range, and invalidates other mappings of the file in this branch.

The cloned object graph preserves hard-link and FD aliasing: every reference in
one branch points to that branch's `regularFile`; the other branch has a
distinct object. MAP_SHARED aliases within one branch therefore converge on
the same newly copied page.

This covers tmpfs regular files, memfd, POSIX shared memory, and anonymous
`MAP_SHARED`, all of which use tmpfs machinery. Add tests for each access path,
not only mmap faults.

Syscall writes need special lock ordering after a fork. `IOSequence.CopyInTo`
may invoke the tmpfs writer while holding the source `MemoryManager.activeMu`
for reading, but breaking file CoW invalidates mappings and may need that same
lock for writing. When a write overlaps a live-fork-shared tmpfs range, TTC
therefore stages source data through a bounded buffer before invalidation. The
ordinary non-CoW path remains direct. Regression coverage maps the destination
file in the source memory manager and verifies that `pwrite()` neither reenters
the lock nor loses data.

### `pkg/sentry/fsimpl/overlay` and gofer

The supported root layout is:

* an immutable, read-only lower;
* a tmpfs upper;
* overlay metadata cloned by the state graph.

The tmpfs changes provide upper-data CoW. Overlay tests must cover whiteouts,
opaque directories, copy-up, rename, hard links, open-unlinked files, and mmap
of upper files.

Initially accept a gofer lower only if it is configured read-only and treated
as immutable for the lifetime of every branch. Reuse of its live client/session
needs an explicit clone hook because gofer client and host-FD fields are not
serialized. Writable gofer mounts and mutable shared lowers are rejected.

### System V shared memory and other shared page owners

`kernel/shm.Shm` currently exposes one contiguous writable `MemoryFile` range.
It needs the same logical-page-to-physical-range CoW scheme as tmpfs, while
ensuring every mapping in one branch sees that branch's copied page.

Implement this after tmpfs. Prefer extracting a small reusable branch-page-map
helper once the tmpfs semantics are proven rather than designing a generic
abstraction first.

Audit and initially reject other `MemoryFile` owners, including packet mmap,
KCOV, AIO mappings, io_uring, and device-backed mappings. Each becomes
supported only after it has clone-mode reference acquisition and write-isolation
tests.

### Pipes, epoll, event objects, and Unix sockets

These are primarily Sentry Go object graphs. Once metadata clone works, shared
pointers and cycles should be duplicated correctly by `pkg/state`: all FDs in
the child that referenced one pipe or epoll instance still reference one child
object, while the parent retains its original object.

Support must still be gated by tests and validation. Host-backed Unix socket
endpoints are rejected; purely Sentry-internal endpoints can be enabled after
queue and peer-topology tests pass.

### Networking

The current implementation accepts:

* Sentry-owned netstack TCP/UDP endpoints and queues across guest-created
  network namespaces, interfaces, veth pairs, routes, and iptables/conntrack;
* purely internal Unix sockets, including queued `SCM_RIGHTS` when every
  referenced descriptor is itself forkable; and
* the TTC in-process virtual NIC with guest-managed configuration.

The TTC NIC has an explicit external-flow policy. The source keeps its
gateway/proxy, published listener, and host-facing connections. The child
reconstructs a fresh link and proxy realm, retains internal loopback/`ttc0`
queues, resets inherited TCP endpoints that terminate outside the branch, and
starts unpublished. Hostinet, plugin stacks, and arbitrary external links are
available only to families created with an explicit live-fork waiver. Nftables
mutation is rejected at creation time in eligible families until its state is
serializable.

The source proxy's explicitly marked port-forwarding waiter registrations are
host-runtime state rather than part of the Sentry object graph. Queue snapshots
omit only those registrations while retaining serializable Sentry listeners
such as epoll interests. Listener implementation alone is not sufficient: both
host relays and guest blocking operations can use channels. Snapshotting does
not unregister or mutate the source queue; the child creates new relay waiters
when its fresh proxy realm starts.

### `runsc/boot` and control plane

Add a `SandboxFamily`/branch manager above the current single `Loader.k` model.
The first control endpoint is conceptually:

```text
containerManager.LiveFork(sourceBranch, requestedChildBranch)
    -> childBranch, timings, rejection report
```

Existing RPCs default to the original branch until they acquire an explicit
branch selector. Then add branch-aware variants for processes, exec, signal,
wait, pause, resume, events, and destroy.

The external CLI should be a TimeTravelContainer command first, even if implemented in
the runsc tree:

```text
ttc fork <sandbox> --from <branch> --name <child>
ttc branches <sandbox>
ttc pause|resume|destroy <sandbox>:<branch>
```

Do not claim OCI runtime compatibility for multiple branches until state
directories and lifecycle semantics are designed. A later adapter can map a
branch to an OCI-visible container ID while routing all operations to the same
Sentry control socket.

## Resource-closure validation

Validation is mandatory and precedes parent CoW mutation. The report should
include a stable resource kind, guest-visible owner when available, and reason.

Current policy (the frozen walk remains fail-closed if an object somehow
bypassed its creation-time gate):

| Resource | Initial disposition |
|---|---|
| Private anonymous mappings | Clone with existing MM CoW |
| Internal pipe/eventfd/epoll graph | Clone after object-graph tests |
| Tmpfs/memfd/POSIX SHM | Page CoW implemented and exercised through syscall and mapped writes |
| In-memory OverlayFS upper on tmpfs | Clone, including metadata, hardlinks, whiteouts, and sparse size divergence |
| Read-only immutable gofer lower | Share with explicit hook |
| SysV SHM | Clone through a branch-local page map with first-fault CoW |
| pidfd | Clone and rebind its target to the corresponding branch-local task |
| Sentry netlink socket | Clone its protocol, queued datagrams, port allocation, and network-namespace state |
| Internal Unix socket | Clone, including queued SCM_RIGHTS whose referenced FDs are forkable; reject host peers |
| Sentry netstack socket | Clone across guest namespaces, NICs, routes, iptables/conntrack, and internal queues |
| TTC virtual-NIC socket | Clone internal state; retain source proxy; reset external child TCP |
| Host TCP/UDP/Unix socket outside the TTC proxy policy | Require a family-level live-fork waiver |
| Writable or mutable gofer mount | Do not admit to an eligible family |
| Host FD/device/external FUSE/GPU | Do not admit to an eligible family |
| In-sandbox `/dev/fuse` | Clone daemon, queues, filesystem state, and mmap cache |
| AIO/io_uring/KCOV/packet mmap | Fail creation in an eligible family |
| TTC-managed guest stdio | Clone |

The validator must inspect reachable kernel/VFS objects, not only the numeric
FD table. An unsupported object reachable through epoll, SCM_RIGHTS queues,
ioctl state, or a mapping is still a rejection.

## Live-fork transaction

The operation is serialized per source branch and follows this order:

1. Reserve a child branch ID in `creating` state.
2. Apply `Kernel.Pause()` to the source and wait for tasks/AIO to stop.
3. Call `ReceiveTaskStates()` to materialize complete registers and FP state.
4. Under `Kernel.extMu`, stop virtual time and suspend relevant notifications.
5. Run resource-closure validation. On rejection, resume the source unchanged.
6. Gather main and private `MemoryFile`s and acquire owner references for the
   prospective child.
7. Prepare parent private PMAs and supported shared-data owners for CoW. This
   may remove writable host mappings but does not copy data.
8. Allocate a child `Kernel` pre-seeded with the shared platform and memory-file
   context. It has no runnable task goroutines yet.
9. Stream parent kernel metadata through `state.Save` -> `io.Pipe` ->
   `state.Load(child)` using a clone-mode context.
10. Clone-mode hooks acquire every child page reference, reattach explicitly
    shared immutable external objects, initialize runtime-only fields, and
    reject any object that escaped validation.
11. Complete parent and child tmpfs/shared-object write protection.
12. After state load has created the child's `TaskSet`, apply an external stop
    to it and start child tasks with `StartPaused()`; the new goroutines enter
    the stop before executing guest code.
13. Register the fully constructed child in the branch manager.
14. Resume parent and child virtual time and tasks, then return success.

Failure handling:

* Before step 7: release reservations/references and resume the parent.
* After step 7 but before publication: destroy the partial child and resume the
  parent. Extra parent CoW markings are safe and may remain.
* After publication: the operation is committed; later errors are branch
  runtime failures, not fork rollback.

## Implementation status

As of 2026-07-19, the implementation spans the documented Phase 0-7 slices on
systrap. Kernel metadata streams through an in-memory pipe; branches share the
platform and eligible MemoryFile pages; visible PIDs and register state are
preserved; private anonymous memory, tmpfs, and SysV SHM detach copy-on-write;
and the disk-backed rootfs upper publishes a new `ttcvol` metadata-tree root
with reflink-on-first-write file data. Focused tests cover
bidirectional writes, application write faults, recursive branch trees, and
arbitrary release order, while real multithreaded Python measurements confirm
that resident application bytes are not eagerly copied.

`runsc` owns a transactional `SandboxFamily` with branch-local process,
signal, wait, exec, event, console, pause/resume, and destroy control. The
current gate accepts an immutable read-only gofer lower with directfs disabled,
a writable `ttcvol` OverlayFS upper, supported Sentry virtual filesystems and
managed mounts, canonical loopback, and the TTC virtual NIC. It validates
reachable FD/VMA/mount/network resources before page-ownership mutation and
continues to fail closed for host FDs, writable gofer state, unsupported special
mappings, host networking, devices, and unrecognized implementations.

The early Phase 0-2 measurements and phase-by-phase paragraphs below are
retained as implementation history. The current performance baseline and
compatibility boundary are summarized in
[timetravel_ttc_audit.md](timetravel_ttc_audit.md).

The Phase 3 object-graph slice is validated for anonymous pipes, pure Sentry
eventfds, epoll registration graphs, timerfds, signalfds, inotify watches,
pidfds, Sentry netlink sockets, and connected internal Unix socketpairs. The
real-loader fork test installs these
objects in the virtual PID 1 FD table before cloning. It verifies that queued
bytes, event counters, pending signals, inotify events, and timer expiration
counts start identical, aliases remain coherent inside each branch, consumption
and writes diverge, and readiness never crosses branches. A cloned epoll
interest resolves to the cloned pipe; deleting the interest in the child does
not mutate the parent interest. An armed cloned timerfd also expires in the
running child without making the paused parent readable. Signalfd target-task
pointers and inotify watch targets resolve inside the cloned graph. A connected
Unix socketpair preserves its peer topology while its receive queues become
branch-local. A cloned pidfd resolves to the corresponding task in its own
branch, and cloned netlink sockets are distinct branch-local objects.

These resources required no special clone implementation beyond their existing
state graph and post-load hooks. Timerfd testing exposed and fixed a kernel
lifecycle gap: `StartPaused` paused Timekeeper updates and timers, but the final
matching `Unpause` did not resume them. The kernel now resumes time before
releasing the final task stop.

An explicit FD-graph validator now runs before VFS save hooks and before the
parent MM is transitioned to CoW. It walks each distinct task FD table,
recursively follows epoll interests, and returns a stable PID/FD/resource/reason
report. Anonymous and socket objects fail closed unless their implementation
opts in. Pure eventfds are accepted while host-backed eventfds are rejected;
Unix transport validation rejects host-backed receivers, peers, and bound host
sockets. Queued `SCM_RIGHTS` is traversed recursively and accepted when every
carried description is forkable. A real-loader test checks direct and
epoll-reachable host eventfd rejection, then removes the resource and completes
a valid fork. The check is now an invariant guard: normal eligible TTC paths do
not import such host descriptions.

The first Phase 4 tmpfs slice is implemented for main-MemoryFile-backed regular
files, including memfd. Each tmpfs filesystem maintains a runtime registry that
also covers open-unlinked files. Live-fork preparation invalidates existing
translations and marks present logical pages as shared without copying them;
state load acquires one child MemoryFile reference per file-owned range.
Syscall writes and partial-page truncation allocate and copy only touched shared
pages. The real-loader test verifies initial memfd equality, parent `pwrite`
divergence, child mapped-write divergence, partial truncation, sparse allocation,
and parent page lifetime after child destruction.

The current mmap mechanism conservatively detaches a shared tmpfs page on its
first post-fork mapping fault, even when that fault requests only read access.
This avoids stale writable or sibling translations because `Mappable.Translate`
runs under MM fault locks and cannot synchronously invalidate every mapping in
the same branch. It preserves fork-time scaling and page-granular sharing, but
true write-fault-only tmpfs CoW requires an MM/Mappable handshake that performs
branch-wide invalidation before installing the writable translation. Private
tmpfs MemoryFiles and broader truncate/fallocate/mapping stress remain in
Phase 4.

The runsc gate accepts a writable root backed specifically by a TTC-managed
OverlayFS upper; TTC now supplies a persistent `ttcvol` upper, while generic
self-backed and anonymous-filestore uppers remain ineligible. The real-loader
fixture uses that writable root and verifies upper
file content and mode divergence, mapped upper writes, child unlink with a
still-open FD, branch-local hardlink topology, partial truncation and sparse
allocation, a parent-only new entry, and child-only copy-up, mutation, and
whiteout of an immutable lower file while an open FD remains usable. It then
recursively forks the modified child, checks that memfd and open-unlinked upper
contents start equal and diverge again, and destroys the grandchild first.

Recursive fork testing exposed gofer-backed executable and library PMAs that
were refaulted after the first child ran. `LiveFork` now invokes the same
`InvalidateUnsavable` pass as normal `Kernel.SaveTo`, after resource validation
but before VFS preparation. These VMAs remain intact and refault through the
cloned gofer graph, while unsupported `MmapCachedFile` PMAs never reach state
serialization.

The first Phase 5 slice replaces the single immutable MemoryFile range backing
each SysV SHM segment with an interval page map. Fork preparation invalidates
every mapping of each reachable IPC namespace and marks its present ranges
shared; the child takes one MemoryFile reference per range. The first post-fork
fault detaches only the required pages, using the same conservative first-access
rule as tmpfs. A real-loader test maps one segment twice and verifies alias
coherence within parent, child, and grandchild branches, isolation between those
branches, and recursive re-forking without copying untouched pages.

VMA resource closure runs immediately after FD-graph validation and before any
page-ownership mutation as an invariant check. Mappables provide an explicit
live-fork validator; eligible kernels reject creation of AIO rings, io_uring
SQ/CQ/SQE mappings, PACKET_MMAP rings, and KCOV before such mappings can enter
the graph. A
fallback also rejects mappings whose retained VFS identity belongs to host,
anonymous, dev, or devtmpfs filesystems, covering closed device FDs that
remain reachable only through a VMA. In-sandbox FUSE mappings opt in after
validating their `/dev/fuse` transport; external FUSE mappings reject. The
real-loader gate creates an AIO ring, checks rejection, destroys it, and then
forks the same parent successfully.

The first Phase 6 slice admitted only canonical loopback. The current slice
serializes TTC-only stack configuration, including Sentry-owned NICs, routes,
iptables/conntrack, and related network namespaces, while leaving ordinary
gVisor checkpoint behavior unchanged. The runtime-owned TTC proxy link is
reconstructed branch-locally before restoring endpoints. The real-loader gate
uses connected nonblocking UDP sockets and an established TCP stream: unread
datagrams and stream bytes clone into parent, child, and grandchild receive
queues, while later sends and readiness remain local to the originating branch.
Hostinet, plugin stacks, and external links require an explicit family waiver;
packet-mmap creation and nftables mutation fail in an eligible family.

## Implementation phases and gates

### Phase 0: metadata-clone feasibility spike

Changes:

* Add an internal `Kernel.CloneMetadataForTest` using `state.Save`/`state.Load`
  over `io.Pipe`.
* Refactor runtime-only initialization from `LoadFrom`.
* Add clone context plumbing and `StartPaused`.
* Construct two Kernels on one systrap platform with a shared `MemoryFile`.

Restrictions: no application pages, gofer, networking, private MemoryFiles, or
unsupported FDs.

Gate: both kernels start, expose identical PID/task topology, and can run and
terminate independently without double-destroying shared infrastructure.

### Phase 1: private anonymous memory

Changes:

* Add `MemoryFile` owner references.
* Extract MM CoW preparation from `MemoryManager.Fork`.
* Add clone-mode PMA reference acquisition.
* Discard unsupported non-private PMAs.

Gate: fork a multithreaded workload with at least 1 GiB of faulted anonymous
memory; parent and child see identical initial bytes; writes diverge; pause time
does not scale with copied bytes; repeated create/destroy has no ref leak.

### Phase 2: branch manager and minimal CLI

Changes:

* Add `SandboxFamily`, branch states, live-fork RPC, branch listing, pause,
  resume, and destroy.
* Reconstruct branch-local loader process/wait bookkeeping.
* Add failure injection after every transaction step.

Gate: a real systrap sandbox can be forked and both virtual PID 1 processes are
controllable independently.

### Phase 3: pure Sentry kernel objects

Enable and test pipes, eventfd, timerfd, signalfd, epoll, inotify, and internal
Unix sockets one class at a time. Add resource-closure traversal for queued FDs
and epoll graphs.

Gate: mutations and readiness notifications never cross branches, while aliases
inside a branch remain coherent.

### Phase 4: tmpfs and OverlayFS upper

Implement tmpfs page CoW for mmap and syscall write paths, then enable a
read-only lower plus tmpfs overlay upper.

Gate: file contents, metadata, directory topology, whiteouts, open FDs,
hardlinks, unlinked files, and mappings diverge correctly. Fork does not copy
tmpfs data pages.

### Phase 5: remaining shared memory

Implement SysV SHM and audit special mappings, AIO, io_uring, packet mmap, KCOV,
and device mappings. Unsupported classes remain explicit rejections.

Gate: shared mappings remain shared among processes in one branch and isolated
from the corresponding mappings in other branches.

### Phase 6: contained networking

Enable internal Unix sockets, loopback netstack connections, and the TTC
in-process virtual NIC through explicit validation and clone hooks. Host
networking and arbitrary external links remain rejected; TTC proxy flows use
the documented source-retain/child-reset policy.

Gate: connection state, packet queues, polling, and peer relationships clone
without packets or wakeups crossing branches, while child external flows reset
without interrupting the source.

### Phase 7: recursive fork and optimization

Enable any branch to fork again. Optimize metadata streaming, reference
acquisition, and range marking based on profiles. Consider persistent metadata
structures only after correctness and measurement.

`ParentID` records historical creation lineage, not a lifetime dependency.
Every successful child owns its complete Kernel and backing references, so an
ancestor may be destroyed while descendants continue to run and fork.

The Kernel transaction exposes a test-only failure injector after validation,
parent CoW preparation, owner acquisition, metadata decode, each child restore
phase, and final pause. Every injected failure must leave the source paused and
able to complete another fork; partially acquired MemoryFile owners and loaded
child graphs are released before the error returns.

The family transaction separately injects failures after placeholder
registration, source pause, Kernel clone, child task start, and final branch
publication. Each path removes the placeholder, terminates/releases any child,
restores the source's prior run state, and permits an immediate retry.

Successful fork results carry nanosecond-resolution timings for source pause,
total source downtime, validation, parent CoW/VFS preparation, metadata
streaming, child restore and task start, child resume, source resume, and total
API latency. The
`BenchmarkSandboxFamilyFork` benchmark reports each component independently so
optimization can follow profiles rather than conflating pause time with child
startup or teardown.

An initial profile showed metadata dominating a bare-sandbox fork because the
state wire codec performs many 1-10 byte operations and the prototype connected
it directly to a synchronous `io.Pipe`. Live fork now places a bounded 64 KiB
buffer at each side of the pipe; metadata remains streamed in memory without an
unbounded full-state buffer or filesystem I/O.

One development-host snapshot (Intel Xeon Platinum 8488C, systrap, five bare
forks) improved from about 9.25 ms/fork before buffering to 3.95 ms/fork after
buffering. The latter decomposed into about 3.85 ms metadata streaming, 44 us
child task start, 23 us child restore, 6.9 us parent preparation, and 3.94 ms
total source downtime. These numbers are diagnostic rather than an acceptance
threshold. A separate one-iteration anonymous-memory sanity sweep measured
about 617 us at 1 MiB resident and 305 us at 1 GiB resident; the noisy inversion
is not a throughput claim, but it confirms that fork latency did not grow with
the number of resident bytes because those pages were not copied.

The recursive-lifetime gate also builds and tears down twenty four-branch trees,
destroying ancestors while descendants remain live. After each round only the
default branch may remain, and aggregate `MemoryFile.TotalUsage` after the sweep
must exactly match a pre-sweep baseline taken after one warm-up fork, excluding
one-time lazy source initialization while still detecting per-branch growth.

Gate: a branch tree can be repeatedly forked and destroyed in arbitrary order
without page leaks, use-after-free, or cross-branch state changes.

## Test plan

### Correctness

* Virtual PID/TID/PPID/session/process-group equality across branches.
* Register, SIMD/FPU, TLS, signal mask/pending state, alternate stack, robust
  futex, rseq, and interrupted-syscall behavior.
* UTS, IPC, mount, user, and PID namespace state.
* Time and timer behavior at the fork boundary.
* `/proc` views, cwd/root, credentials, limits, and file offsets.
* Private anonymous and private file-backed mappings.
* Every supported internal resource and every explicit rejection case.
* Branch deletion while siblings still reference ancestor pages.
* Recursive forks and concurrent activity in sibling branches.

### Filesystem

* Read/write, mmap shared/private, truncate, fallocate, hole punch, and seals.
* Hard links, symlinks, rename, whiteouts, opaque directories, copy-up.
* Open-unlinked files and directories.
* Concurrent mappings of one tmpfs file by several processes in one branch.

### Failure injection

Inject failure after validation, owner acquisition, parent CoW preparation,
encoder start, partial decode, reference acquisition, child start, and branch
registration. The parent must always resume with its original data and object
topology.

### Performance

Measure separately:

* source pause time;
* metadata encode/decode time;
* child runtime initialization and task start;
* total API latency;
* first-write CoW latency and copied bytes.

Sweep resident memory while holding metadata constant, then independently sweep
tasks, VMAs, FDs, pipe/socket queued bytes, tmpfs extents, and file count. The
primary Phase 1 acceptance criterion is flat latency versus resident byte count,
not an absolute microsecond target.

## Known risks and early decision gates

1. **State engine side effects:** checkpoint hooks may assume the destination is
   a new Sentry process. Phase 0 must enumerate package-global and external
   state and add clone-specific behavior rather than silently sharing it.
2. **Backing reference completeness:** missing a single child `MemoryFile`
   reference can cause reuse or decommit while still mapped. Reference audits
   and destructive branch-lifetime tests are mandatory.
3. **Gofer reattachment:** cloning a lower filesystem without reopening host
   FDs requires explicit sharing of a read-only live client. If this is too
   invasive, the first real-container milestone should use an in-Sentry tmpfs
   root and defer overlay.
4. **Multiple Kernels per Sentry:** global fdnotifier, networking registries,
   accounting, watchdog, metrics, and control-plane assumptions require audit.
   Phase 0 is a stop/go gate for the in-process-family architecture.
5. **Metadata cost:** `pkg/state` copies mutable queues and filesystem metadata.
   This is acceptable initially, but large object graphs will not fork in
   constant time.
6. **Randomness semantics:** resolved policy: application-resident PRNG state is
   cloned as ordinary memory, while later `getrandom` and `/dev/{u,}random`
   reads remain fresh host-backed entropy requests and are not replayed. TTC
   does not promise identical future entropy streams across branches.

If the multiple-Kernel Phase 0 gate fails because of pervasive process-global
state, the fallback is a warm child Sentry process with sealed shared base
`memfd`s and branch-local allocators. That design is more isolated but requires
generation-aware `memmap.File` handling and is expected to have higher latency;
it should not be selected before the simpler architecture is tested.

## Historical first implementation slice

The first code change implemented only Phase 0 plus the minimum owner-lifetime
scaffolding needed to avoid double destruction. It intentionally omitted tmpfs
CoW, networking, and an OCI-visible command, answering the central
architectural question with executable evidence:

> Can two independently started gVisor Kernels, cloned through the state graph,
> safely share one systrap platform and one page allocator inside a Sentry?

That gate passed. The later phases described above subsequently added
private-memory and filesystem CoW, resource validation, networking, recursive
families, and the user-facing TTC lifecycle.
