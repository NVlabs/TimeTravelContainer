# TimeTravelOS live container fork

Status: design draft for the first implementation.

## Summary

TimeTravelOS adds an in-memory operation that forks a paused gVisor sandbox
into two independently runnable branches. The operation clones Sentry kernel
metadata, preserves virtual Linux observables, shares existing physical pages,
and performs page-level copy-on-write (CoW) after either branch writes. It does
not create a checkpoint image or copy resident application memory.

The first implementation will host all branches of a sandbox in one Sentry
process. Each branch has a distinct `kernel.Kernel` and virtual object graph,
but the family shares the systrap `platform.Platform` and one or more
`pgalloc.MemoryFile`s. This avoids forking the Go runtime, permits reuse of
gVisor's existing private-memory CoW machinery, and optimizes for low fork
latency rather than branch failure isolation.

## Goals

1. Fork a systrap sandbox without KVM, root privilege, checkpoint files, or
   work proportional to resident memory size.
2. Preserve the source branch's virtual PIDs, parentage, registers, signal
   state, UTS state, clocks, file descriptions, and internal object topology.
3. Preserve sharing inside a branch while preventing writes from crossing
   between branches.
4. Clone internal pipes, epoll graphs, Unix sockets, timers, and similar Sentry
   objects through the existing state object-graph machinery.
5. Use an OverlayFS-style root with an immutable lower and a tmpfs upper whose
   file data becomes page-level CoW at live fork.
6. Reject a fork before mutation if any reachable resource is not supported or
   escapes the sandbox closure.
7. Make failure atomic from the guest's perspective: failure may leave the
   parent more conservatively write-protected, but must not change its data or
   observable kernel state.

## Non-goals for the first implementation

* Running different branches in different Sentry processes or hosts.
* Supporting KVM, ptrace, or other gVisor platforms before systrap.
* Migrating or persisting branches.
* Cloning host networking, writable gofer mounts, host FDs, devices, GPU state,
  packet mmap rings, KCOV, AIO, or io_uring.
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

The systrap platform can be shared because it creates independent address
spaces and contexts and already uses global stub/process pools. The first
implementation must add tests proving that two started Kernels can safely use
the same `platform.Platform` concurrently.

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

Checkpoint-only package-global state requires audit. In particular, dangling
netstack endpoints are currently gathered globally during state save. The
network phase must make this per-Kernel or reject non-empty network state.

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

Phase one rejects all network sockets. Later phases may allow:

* loopback-only netstack TCP/UDP where all endpoints and queued packets are
  contained in the cloned Kernel;
* purely internal Unix sockets.

Hostinet, plugin stacks, link endpoints connected to the host, port forwarding,
and externally routed connections remain rejection cases until an explicit
policy and implementation exist. Network namespaces and netstack package-global
registries need an ownership audit before enabling this phase.

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

The external CLI should be a TimeTravelOS command first, even if implemented in
the runsc tree:

```text
ttos fork <sandbox> --from <branch> --name <child>
ttos branches <sandbox>
ttos pause|resume|destroy <sandbox>:<branch>
```

Do not claim OCI runtime compatibility for multiple branches until state
directories and lifecycle semantics are designed. A later adapter can map a
branch to an OCI-visible container ID while routing all operations to the same
Sentry control socket.

## Resource-closure validation

Validation is mandatory and precedes parent CoW mutation. The report should
include a stable resource kind, guest-visible owner when available, and reason.

Initial policy:

| Resource | Initial disposition |
|---|---|
| Private anonymous mappings | Clone with existing MM CoW |
| Internal pipe/eventfd/epoll graph | Clone after object-graph tests |
| Tmpfs/memfd/POSIX SHM | Clone after tmpfs page CoW |
| Overlay upper on tmpfs | Clone after tmpfs and overlay tests |
| Read-only immutable gofer lower | Share with explicit hook |
| SysV SHM | Reject until branch page map is implemented |
| Internal Unix socket | Clone; reject host peers and queued SCM_RIGHTS |
| Loopback netstack socket | Reject until networking phase |
| Host TCP/UDP/Unix socket | Reject |
| Writable or mutable gofer mount | Reject |
| Host FD/device/FUSE/GPU | Reject |
| AIO/io_uring/KCOV/packet mmap | Reject |
| Guest stdio connected outside | Reject in strict mode |

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

As of 2026-07-16, Phase 0 passes on the systrap platform. The prototype streams
Kernel metadata directly through an in-memory pipe, shares one platform and main
MemoryFile, preserves the visible init PID, starts both kernels from an external
stop, gives each branch a private VVAR page, and releases both kernels
independently. The focused `pgalloc` owner-lifetime and systrap two-Kernel tests
pass.

Phase 1 scaffolding is also implemented: private PMAs are write-protected using
the existing MM fork COW transition, clone-mode state load acquires child PMA
range references, and a focused anonymous-memory test verifies bidirectional
write divergence. A one-iteration benchmark measured about 0.72 ms at 1 MiB and 0.55 ms at
1 GiB of resident anonymous memory, confirming that clone time did not scale with
resident bytes in this setup. A 100-cycle clone/release test also verifies that
the parent PMA regains a unique page reference after every child release. A real
multithreaded sandbox workload remains before the complete Phase 1 gate.

The restricted Phase 2 gate is implemented. `runsc` now owns a `SandboxFamily`
with explicit creating, paused, running, destroying, and failed states; exposes
fork/list/pause/resume/destroy plus branch-local process, signal, and wait RPCs;
and provides matching `runsc timetravel` commands. Fork publication is
transactional, source run state is restored on failure, branch trees can fork
recursively, each child has its own watchdog, and loader teardown destroys
leaves before ancestors. A real loader test forks a running systrap sandbox,
observes virtual PID 1 in both kernels, kills and waits for only the child, and
verifies that the parent remains alive.

That gate currently applies only to `NetworkNone`, a read-only gofer root with
no additional OCI mounts, directfs disabled, and no host-facing process FDs at
the fork boundary. The contained netstack is cloned and restored as an
independent object. The child gofer filesystem borrows the immutable root
connection and root FDID while owning descendant FDIDs; children are therefore
destroyed before the default branch. Branch-local exec, events, foreground TTY
signaling, writable roots, additional mounts, and general network resource
closure remain later work.

The Phase 3 object-graph slice is validated for anonymous pipes, pure Sentry
eventfds, epoll registration graphs, timerfds, signalfds, inotify watches, and
connected internal Unix socketpairs. The real-loader fork test installs these
objects in the virtual PID 1 FD table before cloning. It verifies that queued
bytes, event counters, pending signals, inotify events, and timer expiration
counts start identical, aliases remain coherent inside each branch, consumption
and writes diverge, and readiness never crosses branches. A cloned epoll
interest resolves to the cloned pipe; deleting the interest in the child does
not mutate the parent interest. An armed cloned timerfd also expires in the
running child without making the paused parent readable. Signalfd target-task
pointers and inotify watch targets resolve inside the cloned graph. A connected
Unix socketpair preserves its peer topology while its receive queues become
branch-local.

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
Unix transport validation rejects host-backed receivers, peers, bound host
sockets, and queued SCM_RIGHTS. A real-loader test checks direct and
epoll-reachable host eventfd rejection, then removes the resource and completes
a valid fork. Mapping, ioctl-held, and filesystem-specific closure still need
the same explicit treatment in later phases.

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

Refactor global netstack save state as needed, then enable internal Unix sockets
and loopback-only netstack connections. External links remain rejected.

Gate: connection state, packet queues, polling, and peer relationships clone
without packets or wakeups crossing branches.

### Phase 7: recursive fork and optimization

Enable any branch to fork again. Optimize metadata streaming, reference
acquisition, and range marking based on profiles. Consider persistent metadata
structures only after correctness and measurement.

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
6. **Randomness semantics:** guest-visible PRNG and entropy sources require an
   explicit policy. If TimeTravelOS promises identical post-fork random streams,
   host-backed entropy calls must be virtualized per branch rather than simply
   forwarded after resume.

If the multiple-Kernel Phase 0 gate fails because of pervasive process-global
state, the fallback is a warm child Sentry process with sealed shared base
`memfd`s and branch-local allocators. That design is more isolated but requires
generation-aware `memmap.File` handling and is expected to have higher latency;
it should not be selected before the simpler architecture is tested.

## First implementation slice

The first code change after this design should implement only Phase 0 plus the
minimum owner-lifetime scaffolding needed to avoid double destruction. It
should not yet add tmpfs CoW, networking, or an OCI-visible command. The result
will answer the central architectural question with executable evidence:

> Can two independently started gVisor Kernels, cloned through the state graph,
> safely share one systrap platform and one page allocator inside a Sentry?

Only after that test passes should Phase 1 modify private-memory CoW paths.
