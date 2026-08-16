# TTC harness feature implementation

This document tracks the TTC capabilities requested by TimeTravelHarness. It
is an implementation checklist, not an end-user compatibility promise. Update
the status and test evidence in the same change that advances an item.

Status values are **planned**, **in progress**, **implemented**, and
**verified**.

## Standing regression rule

Changes to live fork, dehydration, persistent filesystems, exec sessions,
terminal state, or distributed operation are not complete after unit tests
alone. Before marking them verified, rerun the complex stress workload under:

- local systrap;
- KVM;
- an NFS-backed TTC data root;
- cooperating source and target daemons using remote fork; and
- fork, pause/resume, dehydration/rehydration, daemon restart, and forced
  process-death combinations relevant to the change.

The workload should include threads and child processes, anonymous and shared
memory, memory mappings, internal TCP and Unix sockets, pipes, epoll,
`SCM_RIGHTS`, active exec sessions, and a non-trivial filesystem tree.

## Progress

| Capability | Status | Acceptance criteria |
| --- | --- | --- |
| TTY observation channel | verified | `ttc tty stat TARGET --session NAME --json` exposes saved/forked queue counters, exact blocked readers, readiness waiters, termios/foreground/flush generations, process-group identity, and the Sentry monotonic clock. `ttc tty watch` delivers complete JSON `S` frames, coalesces changes at 10 ms, and emits heartbeats. Each new internal console receives a durable `session_epoch`; live fork and dehydration preserve it. Verified through a local PTY fork/dehydrate/rehydrate sequence and SDK protocol tests. |
| `ttc commit BRANCH IMAGE` | verified | Commit snapshots the branch filesystem without stopping it, emits a runnable OCI image, supports `--label`/`--label-file`, and preserves and overrides OCI labels through `image inspect`. Verified on an NFS-backed data root, including `timetravel.usage` label round-trip. |
| `ttc stats` | verified | Reports per-branch private and CoW-shared resident mapped memory, durable sparse-memory backing, and persistent file-object bytes, plus non-double-counted family totals, in human and stable JSON forms. It deliberately excludes the Sentry Go heap and unmapped cache rather than presenting estimates as branch ownership. Verified under systrap, KVM, NFS, live remote fork, and cold remote/local fork. |
| Named durable exec sessions | verified | `ttc exec --session NAME -d` creates a reusable name; attach/logs/wait/kill work after client disconnect; retained output is bounded by the existing durable ring buffer. Verified on systrap, KVM, NFS, and while a resource-stress process was live. |
| `ttc cp` from dehydrated branch | verified | Read and write a dehydrated branch without changing its lifecycle state, including while peer branches are live. Verified on an NFS-backed data root across daemon restart. |
| `ttc diff BRANCH` | verified | Return changed paths and change kinds from TTC filesystem metadata without walking the guest tree. Verified after divergent writes in an NFS-backed branch family. |
| Batch local fork | verified | `ttc fork SOURCE a b c` pauses and validates the source once and creates all siblings atomically. Verified under systrap, KVM, NFS, and after a remote target was locally fanned out. |
| Fork dehydrated branch | verified | Local and remote batch fork accept a durable dehydrated source without first rehydrating it; children preserve CoW memory/filesystem sharing and source state. Verified under systrap and KVM, from an NFS-resident source, and in a cooperating two-daemon remote-fork topology. |

## TTY observation contract

The authoritative field definitions and harness rationale currently live in
`harness/docs/ttc-tty-observations.md`. TTC deliberately exposes observations,
not prompt matching or a command-completion policy. The implementation must
preserve counters and generations through live fork and dehydration. If an
implementation ever resets them, it must instead expose a changed
`session_epoch` so clients cannot mistake a reset for a stall.

## API compatibility

Every machine-oriented command added here must have a stable JSON form and be
available through the daemon protocol rather than requiring the SDKs to spawn
`ttc`. Python, TypeScript, Rust, and MCP bindings must be updated before the
feature is marked verified.

## Findings from complex regression runs

- 2026-08-16: the full matrix exercised local systrap, KVM, an NFS-backed data
  root, and mutually authenticated source/target daemons. The workload included
  six threads, a child process, anonymous and shared mappings, loopback TCP,
  Unix datagrams, queued real `SCM_RIGHTS`, a pipe, epoll, 128 files, and an
  active named exec session. It crossed batch fork, pause/resume, dehydration,
  cold fork, rehydration, daemon restart, and cleanup. Python, TypeScript, Rust,
  and MCP protocol tests also passed.
- Sparse peer restore was missing `flock(LOCK_SH)` from the Sentry seccomp
  filter. Its first chunk read killed the family. The filter and regression test
  now cover the shared repository-reader lock.
- Restored peers initially allocated independent application memory files,
  turning durable shared pages into private resident copies. Peer restore now
  reuses the family's application `MemoryFile` with explicit owner references;
  `ttc stats` confirms shared resident bytes after cold fan-out.
- containerd's fs-verity capability probe leaked its test descriptor. On NFS,
  the resulting `.nfs` placeholder could delay daemon startup by minutes. TTC's
  pinned containerd dependency now closes the descriptor; a fresh NFS startup
  completed in 57 ms in the regression environment.
- Sparse chunk readers now hold renewable manifest leases until all donated
  readers close. Lease mutation uses descriptor-relative, seccomp-compatible
  operations. Garbage collection performs a short generic retry for transient
  `ENOTEMPTY` or `EBUSY`, but does not recognize or conceal `.nfs*` files.
- Branch destruction now records the irreversible exited state immediately
  after destroying the runtime and before reclaiming persistent artifacts. A
  later NFS or disk cleanup error therefore remains retryable instead of
  leaving metadata that falsely claims a deleted dehydration image is usable.

## Live-fork failure audit

TTC deliberately keeps systematic rejection for invalid lifecycle requests:
the source must exist and be running, paused, or dehydrated; destination names
must be valid and unused; and the family must not have been created with
`--waive-live-fork`. Host networking is permitted only with that explicit
waiver and is therefore systematically non-forkable. Guest-created network
namespaces and their Sentry-managed netstacks are part of the branch and are
eligible. External TCP state is reset in the children rather than denying the
fork.

For an otherwise eligible running or paused source, the remaining
workload-dependent validation failures are narrow and fail closed:

- an unclassified filesystem, a writable external/shared filesystem, or a
  filesystem implementation that explicitly rejects live fork;
- a host- or FUSE-backed descriptor, an anonymous/socket descriptor without a
  live-fork implementation, a host-bound Unix socket, or another Unix endpoint
  implementation outside the Sentry-owned transport graph;
- mapped legacy AIO rings, io_uring rings, `PACKET_MMAP` rings, or KCOV memory;
  these mappings do not yet have branch-local backing; and
- a network stack without TTC containment, branch-owned endpoint, save, and
  child-reset hooks.

Ordinary pipes, epoll graphs, pidfds, inotify, internal TCP and Unix sockets,
and queued Sentry file descriptions passed through `SCM_RIGHTS` are validated
recursively and are forkable. TTC prevents most explicitly unsupported
host/device resources from entering a fork-eligible family at creation or
syscall time, so the validation list is intended as an invariant guard rather
than a normal harness decision point.

After validation succeeds, allocation failure, ENOSPC or filesystem I/O error,
NFS/server failure, remote transport failure, runtime process death, or a TTC
serialization defect may still fail the command. These are operational errors,
not changes in branch eligibility. The stress rule above exists specifically
to keep reducing that second category.
