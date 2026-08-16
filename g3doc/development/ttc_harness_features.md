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
| TTY observation channel | in progress | Named exec terminals expose consistent synchronous snapshots and streamed `S` frames with saved/forked monotonic counters, generations, process-group identity, and Sentry monotonic timestamps. |
| `ttc commit BRANCH IMAGE` | implemented | Commit snapshots the branch filesystem without stopping it, emits a runnable OCI image, supports `--label`/`--label-file`, and preserves and overrides OCI labels through `image inspect`. Complex regression verification is pending. |
| `ttc stats` | planned | Report per-branch private and CoW-shared memory and disk bytes, plus non-double-counted family totals, in human and stable JSON forms. |
| Named durable exec sessions | implemented | `ttc exec --session NAME -d` creates a reusable name; attach/logs/wait/kill work after client disconnect; retained output is bounded by the existing durable ring buffer. Complex regression verification is pending. |
| `ttc cp` from dehydrated branch | implemented | Read and write a dehydrated branch without changing its lifecycle state, including while peer branches are live. Complex regression verification is pending. |
| `ttc diff BRANCH` | implemented | Return changed paths and change kinds from TTC filesystem metadata without walking the guest tree. Complex regression verification is pending. |
| Batch local fork | implemented | `ttc fork SOURCE a b c` pauses and validates the source once and creates all siblings atomically. Complex regression verification is pending for this combined feature set. |
| Fork dehydrated branch | planned | Local and remote batch fork accept a durable dehydrated source without first rehydrating it; children preserve CoW memory/filesystem sharing and source state. |

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
