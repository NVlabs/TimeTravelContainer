# Deferred design: transparent systrap stub reallocation

Status: deferred; design record only.

This work is intentionally not implemented yet. It must be implemented and
reviewed as one security-complete change rather than landed as a sequence of
partially protected runtime modes. In particular, TTC must not expose a
compact, discoverable systrap arena before the hardening and migration
invariants in this document are satisfied.

## Motivation

Systrap executes guest application code natively in host stub processes. A
signal handler, alternate signal stacks, context records, queues, and a small
set of helper instructions share those processes with the guest. The Sentry
uses these mappings to exchange register and event state with stub threads
without paying the cost of `ptrace` or another kernel-mediated message for
every transition.

The current implementation reserves the upper 1/128 of the detected host task
address space. It places the stub at a randomized address in that band and
reports the bottom of the band as `Platform.MaxUserAddress()`. The size of the
excluded band is based on normal Linux address randomization rather than on the
amount of memory the stub actually needs. The active layout is approximately:

* executable initialization and signal-handler code;
* read-only seccomp programs and configuration;
* one guard page;
* 32 KiB per helper thread on systems with 4 KiB pages;
* a lock-free context queue and spinning-thread queue; and
* 4,096 context slots of 4 KiB each, currently 16 MiB in total.

The large excluded band has three purposes in upstream systrap:

1. It prevents ordinary guest mappings from colliding with platform-private
   code and data.
2. It conceals the actual stub address from native guest code. Seccomp and
   Syscall User Dispatch distinguish trusted helper execution partly by
   instruction address, so address secrecy is a defense-in-depth property.
3. It gives the guest a stable maximum address across checkpoint and restore
   even when the actual stub is placed at a different randomized address.

This representation interferes with nested TTC. An inner Sentry observes the
outer Sentry's reduced maximum address and may select a smaller architectural
task-size tier. On amd64, one nesting level can fall from the 56-bit tier to
the 47-bit tier, and another level has no smaller supported tier.

The long-term goal is to retain systrap's shared-memory fast path while making
the platform-private range behave like a small relocatable implementation
detail rather than an architectural address-space ceiling.

## Required outcome

For legitimate applications, the final implementation should have these
properties:

* The detected architectural task size remains stable across nesting levels.
* Non-fixed mappings automatically avoid platform-private ranges.
* A fixed mapping that conflicts with a systrap range causes a safe relocation
  and is then retried.
* Relocation does not copy guest data pages.
* The stub does not appear in guest `/proc` mappings or other guest memory
  accounting.
* A failed relocation leaves the old execution environment fully usable and
  leaves the requested guest mapping uncommitted.
* Knowing or influencing a stub address is not sufficient to execute an
  unintended host syscall, corrupt the Sentry, or escape the sandbox.
* Live fork, pause/resume, exec, stop/start, checkpoint/restore, and nested TTC
  either preserve these properties or fail before mutation with a useful
  diagnostic.

"Transparent" does not require satisfying an adversarial request that consumes
the complete user address space. If TTC cannot retain the minimum address
entropy, guard space, or resources required to construct a safe replacement,
Linux-compatible `ENOMEM` is preferable to weakening the isolation boundary.

## Non-goals

* Removing all platform-private mappings. The handler, signal frames, shared
  state, and synchronization words must remain addressable by native stub code.
* Replacing the fast path with `ptrace`, pipes, or seccomp user notification for
  every guest transition.
* Allowing an application mapping to overwrite a live stub arena.
* Moving an arena with `mremap` while any thread can execute in it.
* Treating address randomization alone as a complete security boundary.

## Security model and mandatory invariants

The design must assume a guest can:

* execute arbitrary machine instructions and create many threads;
* issue arbitrary mapping, signal, and process-control syscalls;
* choose fixed addresses and intentionally collide with platform ranges;
* catch and repeat guest-visible memory faults;
* race relocation against faults, syscalls, clone, exec, and exit;
* inspect all guest-visible procfs and memory-management results; and
* know the source code and infer or discover a candidate stub address.

The completed implementation must maintain all of the following invariants:

1. **No guest overwrite:** no guest VMA or platform mapping may overlap a stub
   generation in a native helper that can execute that guest address space.
   Retiring helpers never receive mappings committed after the generation
   switch.
2. **No mixed generation:** a helper thread's code, signal action, alternate
   stack, seccomp policy, Syscall User Dispatch range, message, queue, and
   context table all belong to the same immutable generation.
3. **No stale execution:** a generation cannot be retired while a thread may
   execute its code, return through one of its signal frames, or reference one
   of its shared records.
4. **Immutable filter transition:** installed seccomp filters are never
   weakened or rewritten. Threads using a new permitted code range are newly
   created with a policy compiled for that generation.
5. **Untrusted shared state:** every value written by a stub remains treated as
   attacker-controlled and is bounds-, generation-, state-, and ownership-
   checked before the Sentry uses it.
6. **Exact syscall authority:** seccomp rules use exact code ranges and strict
   argument rules. Merely choosing an instruction pointer above a lower bound
   must not grant helper authority.
7. **W^X and sealing:** executable pages are never writable after publication;
   rule/configuration pages become read-only; guard pages remain inaccessible;
   and `mseal` is applied where available after all generation-specific
   patching.
8. **Failure atomicity:** before the commit point, the old generation is the
   sole active generation. After the commit point, all resumed contexts use
   the new generation. Any pre-commit failure discards the candidate.
9. **Guest mapping atomicity:** the mapping that triggered relocation is not
   inserted into the logical guest MM until the platform reservation has moved
   successfully.
10. **Fork isolation:** platform-private pages and synchronization state are
    not application memory and never become guest CoW pages shared between
    live-fork branches.
11. **Bounded resource use:** repeated collision requests cannot leak
    generations, helper processes, mappings, memory-file ranges, or file
    descriptors.
12. **No security downgrade:** lack of a safe destination, clean launcher, or
    protection required by the completed security review causes a controlled
    failure, not a fallback to a discoverable and insufficiently protected
    arena.

Before implementation is enabled, perform a dedicated audit of every host
syscall available to a sysmsg thread. Assume an attacker can jump to any byte
of known stub code with chosen registers. Direct syscalls that are not safe
under that assumption must move to a dedicated helper that never executes
guest instructions, or gain sufficient seccomp argument restrictions and
entry validation.

## Why the current arena cannot be moved in place

The current layout is initialized once and represented by package globals such
as `stubStart`, `stubSysmsgStart`, `stubSysmsgStack`,
`stubContextQueueRegion`, and `stubContextRegion`. The address is embedded or
cached in:

* installed seccomp BPF programs;
* each thread's Syscall User Dispatch permitted interval;
* signal dispositions and alternate signal stacks;
* shared `sysmsg` handler and stack pointers;
* signal-handler globals containing queue and context addresses;
* subprocess unmap and lifecycle logic; and
* checkpoint/restore platform parameters.

Threads may also be executing in the handler, polling a queue, sleeping on a
futex, or holding a signal frame on an old stack. Seccomp filters cannot be
removed or broadened after installation, and sealed mappings may be impossible
to unmap after fork inheritance. Consequently, relocation must replace the
native execution helpers; it cannot mutate a live generation in place.

The amd64 syscall-patching path is less restrictive than it first appears.
Patched application sites jump to the guest usertrap table, which reaches the
handler through GS and the `sysmsg.Syshandler` pointer. Once all contexts are
quiescent, existing patches can be rebound to new per-thread messages without
rewriting every patched application instruction. This property must be covered
by regression tests rather than assumed.

## Target architecture

### Per-address-space immutable layouts

Replace package-global addresses with an immutable layout object. Names are
provisional:

```go
type StubGenerationID uint64

type StubLayout struct {
    ID StubGenerationID

    Arena            hostarch.AddrRange
    Executable       hostarch.AddrRange
    ReadOnly         hostarch.AddrRange
    GuardRanges      []hostarch.AddrRange
    SysmsgStacks     hostarch.AddrRange
    ContextQueue     hostarch.AddrRange
    SpinningQueue    hostarch.AddrRange
    Contexts         hostarch.AddrRange

    // Sentry-local mappings and backing ranges are deliberately distinct
    // from addresses used by the native helper.
    Backing           []memmap.FileRange
    SentryMappings    []hostarch.AddrRange

    refs atomic.Uint64
}

type StubGeneration struct {
    layout  *StubLayout
    helpers map[uint32]*sysmsgThread
    state   generationState // building, ready, active, retiring, dead
}
```

One platform address space owns one active generation and may temporarily own
one candidate or retiring generation. Global caches may share immutable code
bytes, but not absolute addresses, filters, queues, stacks, or mutable control
state.

### Guest MM representation

Keep the architectural maximum address independent from platform reservations.
Add an internal, non-VMA reservation set to `mm.MemoryManager` or an equivalent
platform/MM contract:

```go
type PlatformReservation struct {
    Generation StubGenerationID
    Range      hostarch.AddrRange
}
```

Requirements:

* automatic placement skips reservations;
* fixed placement detects a reservation before modifying guest VMAs;
* reservations are absent from guest procfs and VMA accounting;
* changing a reservation is transactional with the conflicting guest mapping;
* faults and platform `MapFile` calls reject stale generations; and
* mappings inherited from an enclosing TTC layer that cannot be used at the
  native host level cause candidate placement to retry rather than corrupting
  the inner MM.

The interface should not make the generic MM import systrap. A platform
reservation/relocation interface should expose capabilities and transactions
without platform-specific types.

### Clean helper launcher

Do not create replacement helpers by forking a process that contains old
`mseal`ed arenas. Introduce a small launcher before any stub generation is
mapped. The launcher remains free of guest mappings and generation arenas and
creates new helpers on authenticated requests from its Sentry.

The launcher must:

* use a private, lifetime-bound control channel;
* accept only validated layout descriptors and required file descriptors;
* set `PR_SET_PDEATHSIG`;
* close unrelated descriptors;
* map the candidate arena with `MAP_FIXED_NOREPLACE`;
* apply W^X protections, signal setup, Syscall User Dispatch, and the exact
  generation seccomp program in a defined order;
* report readiness only after the helper is fully sealed and stopped at a safe
  handoff point; and
* run under a minimal seccomp policy of its own.

If a persistent launcher proves incompatible with rootless or nested
operation, an exec-based clean bootstrap may be evaluated, but it must not
reintroduce a `/proc/self/exe`, filesystem-permission, or user-namespace
dependency. The persistent clean launcher is the preferred design.

### Candidate construction

Build a candidate off to the side while the old generation remains active:

1. Snapshot the logical guest VMA gaps and all platform reservations.
2. Choose a randomized, page-aligned interval large enough for the compact
   arena, guards, growth allowance, and required entropy.
3. Reserve it tentatively so concurrent guest mapping operations cannot claim
   it.
4. Construct generation-specific code, read-only configuration, and seccomp
   programs in private backing memory.
5. Map Sentry views at unrelated Sentry-selected addresses.
6. Ask the clean launcher to create stopped helper processes and threads.
7. Verify every mapping, protection, seal, filter, signal action, queue, and
   generation identifier.
8. Publish the candidate as `ready`, but do not schedule guest execution on it.

Candidate placement must use bounded retries. A host or ancestor TTC collision
is a normal retry condition. Exhaustion returns `ENOMEM` without changing the
active generation.

Context records and per-thread stacks should be allocated lazily or in bounded
chunks so the private hole reflects actual concurrency rather than the maximum
4,096 contexts where practical. Growth must use already reserved space or
trigger a normal relocation; it must never expand over a guest VMA.

## Relocation transaction

Relocation is a cold path. It is allowed to scale with threads and VMAs, but not
with resident guest bytes.

### Triggering operations

At minimum, collision detection must cover:

* `mmap(MAP_FIXED)` and `mmap(MAP_FIXED_NOREPLACE)`;
* `mremap(MREMAP_FIXED)` and in-place VMA expansion;
* fixed-address `shmat`;
* `brk` growth;
* automatic and explicit stack growth;
* executable image and interpreter placement during `exec`;
* restore of a saved VMA layout; and
* any internal forced mapping path that can enter ordinary user addresses.

Non-fixed mapping hints do not require relocation if another Linux-compatible
address can be selected.

### Prepare phase

1. Serialize relocation for the affected platform address space.
2. Mark the conflicting mapping transaction pending without changing the
   visible guest VMA graph.
3. Prevent new contexts, helper threads, exec transitions, live forks, and
   address-space destruction from entering the affected generation.
4. Pause every task sharing the guest memory manager using Sentry/platform
   stops, not guest-visible signals.
5. Interrupt polling and futex waits, drain the context queue, and wait until
   every context is owned by the Sentry.
6. Capture registers, FPU state, signal/fault metadata, TLS state, and usertrap
   bindings.
7. Prove no helper is executing stub code and no live return path depends on an
   old signal frame.
8. Construct and validate the candidate generation.
9. Recreate the native address space by mapping the existing guest
   `memmap.File` ranges into candidate helpers. Do not copy page contents.
10. Bind captured contexts to candidate messages without making them runnable.

Any failure in this phase destroys the candidate, releases its tentative
reservation, unblocks the old generation, and leaves the guest mapping
uncommitted.

### Commit phase

Under the MM mapping lock and the platform generation lock:

1. Revalidate that the guest VMA graph and triggering request still match the
   prepare snapshot.
2. Atomically replace the old active reservation with the candidate
   reservation, commit the requested guest mapping into the released range,
   and change the address space's active generation pointer exactly once.
3. Advance a generation epoch used by faults, mappings, queues, and contexts to
   reject stale operations.

The old helpers remain stopped and never receive the newly committed guest
mapping. Their former address range may be retained in retirement metadata,
but it is no longer an active MM reservation after the atomic generation
switch.

There must be no fallible operation after the active-generation pointer and
guest VMA transaction become visible. If that cannot be guaranteed, retain
enough journal state to complete forward; do not attempt an unsafe partial
rollback.

### Resume and retirement phase

1. Resume contexts only on candidate helpers.
2. Keep the old reservation until every old helper is stopped and all
   generation references, pending events, injected syscalls, and signal-frame
   references are gone.
3. Kill and reap old helpers.
4. Unmap Sentry-local mutable views and release all old backing ranges and
   descriptors.
5. Release the old generation's retirement metadata and range record.
6. Record duration, reason, retries, old/new ranges, and resource counts in
   host diagnostics without exposing addresses through guest interfaces.

An old sealed code mapping retained solely by a launcher or Sentry must not be
inherited by future helpers and must have a bounded lifetime. Tests must detect
address-space and descriptor leakage after repeated relocation.

## Concurrency and lifecycle integration

### Faults and mappings

Every platform fault, map, unmap, and context operation carries or validates a
generation epoch. Operations begun before quiescence either complete against
the old generation before prepare proceeds or restart against the active
generation afterward.

Lock ordering must be documented before implementation. Relocation must not
wait for a task or helper while holding an MM lock that the task needs to
finish a fault. The prepare/commit split exists partly to keep process creation
and mapping work outside the guest MM write lock.

### Clone, exec, and exit

* Guest `clone` cannot publish a new task into a relocating address space.
* `exec` either completes before prepare or restarts after relocation.
* Task exit participates in quiescence and cannot free a context record still
  referenced by either generation.
* Address-space destruction cancels an uncommitted candidate and waits for
  retirement without resurrecting the old generation.

### Live fork

Live fork and relocation are mutually exclusive mutation transactions for an
affected branch.

* A fork waits for relocation to finish or fails before source mutation.
* Platform generations, queues, stacks, and helper processes are not serialized
  as guest kernel state.
* A child branch receives its own active generation or a separately referenced
  immutable layout with branch-local helpers and mutable state.
* Parent and child reservations may use different addresses.
* No helper thread, futex word, queue entry, pending platform signal, or
  retirement reference crosses branch ownership.

Recursive fork tests must cover relocation before and after each fork and a
fixed mapping that forces only one branch to relocate.

### Pause, stop/start, and checkpoint/restore

Pause must wait for an in-progress relocation commit and then report a fully
active generation. Stop/start and restore may choose a fresh arena, but the
guest architectural maximum and saved VMA layout remain unchanged.

Checkpoint data records no host stub address. Restore reserves a safe candidate
that does not overlap saved VMAs before starting any task. If no safe candidate
exists, restore fails before guest execution rather than changing the saved
layout.

### Nested TTC

Every nesting level must retain the same architectural task-size tier.
Candidate placement must tolerate host `MAP_FIXED_NOREPLACE` failures caused by
ancestor-private mappings and retry another guest-free interval. Tests must
cover enough nesting levels to prove task-size stability and the absence of a
geometric address-space reduction.

## Security hardening required before rollout

The implementation is not releasable until all of these audits are complete:

1. Enumerate every syscall allowed by the sysmsg-thread, syscall-thread,
   launcher, and initialization filters, including architecture-specific
   rules.
2. Replace lower-bound-only instruction-pointer checks with exact executable
   intervals.
3. Analyze arbitrary entry at every syscall instruction in the executable
   stub with attacker-controlled registers.
4. Move dangerous host operations to a helper thread/process that never runs
   guest instructions.
5. Validate every shared structure field before using it as an ID, offset,
   length, address, state transition, or Sentry pointer.
6. Add generation IDs and ownership checks to shared queues, messages, context
   slots, and interrupt paths.
7. Confirm W^X, read-only rules/configuration, guard pages, and `mseal` on every
   candidate before readiness.
8. Confirm guest memory APIs cannot read, write, execute, dump, pin, remap, or
   account platform-private mappings through an emulated kernel path.
9. Test direct native read, write, execute, and syscall attempts against known
   and guessed arena addresses.
10. Review timing and fault-oracle exposure. Address secrecy may remain
    defense in depth, but correctness must not depend solely on it.
11. Fuzz candidate descriptors and launcher messages as hostile inputs even
    though their channel is private.
12. Run the existing gVisor security and syscall suites in addition to TTC
    tests.

No command-line switch may enable relocation while skipping these checks.
There must be no "unsafe relocation" counterpart to
`--unsafe-no-host-isolation`.

## Failure handling and observability

Return errors at the original guest operation using Linux-compatible errno:

* `ENOMEM` when no safe arena or required resource can be reserved;
* `EEXIST` for `MAP_FIXED_NOREPLACE` only when a real guest VMA still conflicts
  after any successful relocation;
* the operation-specific errno for unrelated validation failures.

Internal failures should identify the phase and generation in host logs without
printing secrets into guest-visible output. Export counters and latency for:

* relocation attempts, successes, rollbacks, and forward-completed commits;
* placement retries and ancestor collisions;
* quiescence, candidate construction, remapping, commit, and retirement time;
* helpers, VMAs, contexts, and backing ranges migrated; and
* live generation and resource counts.

A daemon or Sentry crash during prepare needs no recovery beyond normal sandbox
recovery because the old generation remains authoritative. A crash after the
commit point must never resume the old generation; durable restart/checkpoint
logic reconstructs a fresh generation from guest kernel state.

## Test and validation gates

### Unit tests

* reservation lookup, allocation skipping, overlap, replacement, and rollback;
* exact errno behavior for every mapping operation;
* layout calculation, alignment, guards, overflow, and bounded placement retry;
* generation state-machine transitions and illegal transition rejection;
* stale epoch rejection in queues, faults, mappings, and interrupts;
* seccomp program generation with exact instruction intervals;
* shared-record bounds and ownership validation; and
* reference and descriptor balance across success and every injected failure.

### Concurrency and fault-injection tests

Inject failure after every prepare and commit step. Race relocation against:

* thousands of concurrent syscalls and page faults;
* signal delivery and `rt_sigreturn`;
* helper sleep/wakeup and fast-path polling;
* thread clone/exit and process exec;
* `mmap`, `munmap`, `mremap`, `brk`, and stack growth;
* pause/resume, stop, branch removal, and daemon shutdown; and
* live fork in parent and child branches.

Run with the race detector where possible and with forced generation-ID reuse
attempts to expose ABA bugs.

### Compatibility tests

* fixed mappings across the old and new arena locations preserve data;
* sparse allocators, JVMs, Chromium, sanitizers, and large-address probes;
* `/proc/maps`, `/proc/smaps`, `mincore`, memory limits, and core-dump behavior;
* usertrap syscall patches before and after relocation;
* multi-thread signal stacks and FPU/vector state;
* checkpoint/restore to a different arena address;
* repeated relocation without RSS, VMA, FD, or helper growth; and
* amd64 47/56-bit and supported arm64 VA-width configurations.

### Nesting tests

At minimum:

* Docker -> TTC;
* TTC -> TTC;
* TTC -> TTC -> TTC;
* repeated deeper nesting until a declared non-address-space resource limit;
* relocation at each level; and
* parent and child live forks with different private arena addresses.

All levels must report the same architectural task-size tier.

### Security tests

* malicious fixed-map pressure intended to force a predictable arena;
* direct probing and jumping to every exported or syscall-containing stub
  offset;
* forged queue entries, context IDs, state transitions, stack pointers, and
  signal frames;
* attempts to invoke host syscalls from arbitrary stub entry points;
* attempts to overmap or change protection on active and retiring arenas;
* launcher protocol fuzzing and descriptor substitution;
* filter and sealing verification from host-side inspection; and
* sandbox-escape regression tests under normal rootless and
  `--unsafe-no-host-isolation` daemon modes.

Security review is a release gate, not follow-up work.

### Performance gates

Measure:

* normal systrap switch and syscall performance before and after the refactor;
* relocation pause time by tasks, VMAs, mappings, and nesting depth;
* proof that relocation does not scale with resident guest bytes;
* first relocation and repeated relocation RSS/PSS;
* live-fork latency before and after per-generation layout changes; and
* Chromium/Speedometer and syscall-heavy workloads.

The normal fast path should remain within benchmark noise or an explicitly
reviewed small regression. Relocation itself may be substantially slower
because it is rare and compatibility-driven.

## Implementation sequence for the future one-run change

When this becomes a priority, implement it on a dedicated branch as one
coherent effort:

1. Add the threat-model tests and exact seccomp/shared-state audits first.
2. Introduce immutable `StubLayout`/generation objects without changing guest
   address semantics.
3. Add the clean launcher and create ordinary initial generations through it.
4. Remove package-global address assumptions and make all hot paths consume
   their owning generation.
5. Add MM platform reservations with unchanged architectural task size.
6. Add candidate construction, quiescence, helper replacement, and retirement.
7. Add transactional collision handling for every mapping operation.
8. Integrate live fork, pause/resume, exec, stop/start, restore, and nesting.
9. Complete security, fault-injection, compatibility, and performance gates.
10. Only then remove the old upper-band `MaxUserAddress` reduction and enable
    transparent relocation by default.

Steps 2-9 may exist as intermediate commits on the dedicated development
branch for review, but no intermediate runtime configuration should be merged
or documented as supported. The mainline-visible change should preserve the
old protected behavior until the complete replacement passes all gates.

## Acceptance criteria

This deferred item is complete only when:

* all supported architectures retain their host architectural task-size tier
  through the tested nesting depth;
* conflicting legitimate fixed mappings succeed after relocation;
* adversarial collision pressure cannot force an unsafe or predictably weak
  layout;
* no address knowledge or arbitrary stub entry yields additional host syscall
  authority;
* all lifecycle and live-fork tests pass with failure injection;
* repeated relocation has no resource leak;
* checkpoint/restore and recursive fork preserve guest memory and observables;
* normal systrap performance remains acceptable; and
* a focused security review finds no sandbox escape introduced by the change.
