# TimeTravelOS `ttos` product and CLI roadmap

Status: TODO roadmap following the live-fork prototype.

Implementation status: an initial lightweight `ttos` client is available at
`//runsc/ttos:ttos`. It controls existing TimeTravel-enabled runsc OCI
sandboxes through runsc's JSON command surface, pulls OCI/Docker images through
containerd, generates rootless image-backed OCI bundles, and implements the
initial run/create/start/inspect/list/remove lifecycle with persistent metadata,
plus synchronous non-TTY exec into a selected live branch. Generated bundles
include procfs, restricted read-only sysfs, tmpfs-backed `/dev` and `/dev/shm`,
and a private devpts instance with `/dev/ptmx`. Existing host paths can be
attached read-only with Docker-style `-v` or `--mount` syntax. Named managed
tmpfs volumes can be attached at one or more paths, and their state forks CoW
with each live branch. `ttosd`, detached and TTY exec, attach, persistent
managed-volume backing, and managed OverlayFS volumes remain TODO.

This document records the product, runtime, filesystem, process-control, and
console work required to turn the TimeTravelOS gVisor prototype into a
Docker-like local container manager. The underlying live-fork design remains in
[timetravel_live_fork.md](timetravel_live_fork.md).

## Product direction

`runsc` is intentionally a low-level OCI runtime. Upstream gVisor normally gets
a friendly interface by registering `runsc` as a Docker runtime (`runsc
install`, then `docker run --runtime=runsc ...`) or by installing
`containerd-shim-runsc-v1` as the `io.containerd.runsc.v1` runtime handler. In
those configurations Docker or containerd owns image pulling, content storage,
OCI bundle generation, snapshots, volumes, and ordinary lifecycle UX.

TimeTravelOS needs a richer model than Docker's one container/one task view: a
container family owns a tree of simultaneously live branches, and every branch
has its own process state, lifecycle state, filesystem upper, internal resource
graph, and console sessions. The recommended architecture is therefore:

```text
ttos CLI
   |
   +-- ttosd local control service
         |
         +-- containerd client
         |     +-- registry/auth and Docker Hub pulls
         |     +-- OCI content store and image metadata
         |     +-- snapshotter/unpacked immutable image roots
         |
         +-- TimeTravel-enabled runsc/shim
               +-- SandboxFamily and branch lifecycle RPCs
               +-- branch-local exec/wait/signal
               +-- branch-local console streaming
               +-- mount eligibility and live-fork validation
```

Docker Engine should remain an optional compatibility frontend, not the source
of truth for TimeTravel branches. A future Docker integration may expose only
the `main` branch and leave `ttos` responsible for branch operations.

## Object and state model

- [ ] Define a stable family ID and user-facing container name.
- [x] Define branch selectors, initially `<container>@<branch>`, with `main` as
      the default branch.
- [ ] Define immutable branch creation lineage. `ParentID` remains historical
      even if an ancestor is destroyed.
- [ ] Persist branch tombstones or lineage metadata so `ttos ls --tree` can show
      descendants whose source branch was deleted.
- [ ] Add an `exited` branch state and reconcile runtime state after virtual PID
      1 exits.
- [ ] Distinguish family state, branch state, and exec-session state.
- [ ] Define `stop` as process termination, not suspension; `pause`/`resume`
      retain memory and kernel state.
- [x] Define initial `start` semantics: a created or exited base container
      starts from its persistent OCI bundle and immutable image root; destroyed
      in-memory branch state is not reconstructed.
- [ ] Specify behavior when a family or branch name conflicts with an image,
      volume, or exec-session name.
- [x] Store initial desired configuration and lifecycle metadata separately
      from transient Sentry state so created/stopped containers can be listed,
      inspected, restarted, and removed.
- [ ] Move persistent state behind `ttosd`, make updates transactional, and
      persist complete branch lineage rather than only the base container.

## `ttos` command surface

### Images

- [x] `ttos pull IMAGE` with Docker Hub shorthand such as `ubuntu:24.04`.
- [x] Normalize unqualified names to `docker.io/library/...`.
- [x] Support OCI registries, tags, immutable digests, and multi-architecture
      manifest selection.
- [ ] Integrate registry authentication and Docker-compatible credential
      helpers without storing plaintext credentials in the state database.
- [ ] Show pull/unpack progress and content reuse.
- [ ] `ttos images` with reference, digest, platform, unpacked state, size, and
      creation time.
- [ ] `ttos image inspect`, `ttos image rm`, and content garbage collection.

### Container lifecycle

- [x] `ttos run [OPTIONS] IMAGE [COMMAND...]` (initial foreground/detached,
      non-TTY slice with procfs, `/dev` tmpfs, and restricted sysfs mounts).
- [x] Support `--name`, environment variables, working directory, named or
      numeric user, entrypoint override, hostname, and rootless execution.
- [x] Support existing host paths through `-v HOST:CONTAINER:ro` and
      `--mount type=bind,...,readonly`.
- [x] Support family-local named tmpfs volumes through `-v NAME:CONTAINER[:ro]`
      and `--mount type=volume,...`, including repeated aliases of one volume.
- [ ] Add CPU, memory, process-count, and other resource-limit flags.
- [x] Support foreground, `--detach`, and attached non-TTY `-i` modes.
- [ ] Support `-t` after the internal detachable console is implemented.
- [x] `ttos create` as persistent image-root and OCI-bundle preparation.
- [x] `ttos start CONTAINER` for created/exited base containers, detached by
      default with optional `--attach`.
- [x] `ttos stop CONTAINER[@BRANCH]` with TERM, timeout, then KILL semantics.
- [x] `ttos kill CONTAINER[@BRANCH] [SIGNAL]`.
- [x] Initial `ttos rm CONTAINER`: remove stopped/created family metadata,
      reject a live family unless `--force` is given, and require `branch rm`
      for an individual non-main branch.
- [ ] Finalize forced-removal and tombstone policy for live descendant
      branches once persistent lineage is implemented.
- [x] Initial `ttos inspect CONTAINER` exposing persistent image/bundle data and
      the live family/branch tree when present.
- [ ] Extend inspect with branch process, resource-eligibility, timing, and
      mount information.
- [ ] `ttos logs CONTAINER[@BRANCH]` backed by branch-local console output.
- [ ] `ttos ps` for host-side container families and `ttos top` for processes
      inside a selected branch.

### TimeTravel lifecycle

- [x] `ttos fork SOURCE [DESTINATION]`, for example `ttos fork web@main
      web@test-a`.
- [x] Support `--paused`, defaulting according to a documented policy.
- [x] `ttos pause CONTAINER[@BRANCH]`.
- [x] `ttos resume CONTAINER[@BRANCH]`.
- [x] `ttos branch rm CONTAINER@BRANCH` or an unambiguous equivalent.
- [x] Initial `ttos ls --tree` compact Unicode lineage and lifecycle-state
      output, merging persistent base containers with authoritative live runsc
      branch state.
- [ ] Extend tree output with PID 1 state, memory sharing, creation time, and
      the complete persisted lineage/tombstone model.
- [x] `ttos ls --json` with a stable machine-readable schema.
- [x] Mark deleted historical parents in tree output without reparenting their
      descendants.
- [ ] Display structured fork-ineligibility issues before attempting a fork.
- [ ] Add `ttos fork --check SOURCE` for a read-only eligibility report.
- [ ] Add shell completion for family, branch, image, volume, and session names.

### Exec

- [x] Initial `ttos exec [OPTIONS] CONTAINER[@BRANCH] COMMAND...` selecting a
      live branch through `runsc exec --branch`.
- [x] Support environment, cwd, and UID/GID overrides in the initial CLI.
- [ ] Add supplementary groups, capabilities, and an OCI process JSON input to
      `ttos exec`.
- [x] Support synchronous non-TTY commands with output streaming and
      exit-status forwarding; optionally import stdin with `-i`.
- [ ] Support TTY-interactive `-t` commands through the internal console (`-i`
      without a TTY is supported for synchronous exec).
- [ ] Support `--detach`, returning a stable branch-qualified execution ID.
- [ ] Add `ttos exec ls`, `ttos exec wait`, `ttos exec kill`, and reattach by
      execution ID.
- [ ] Preserve virtual PIDs and exec IDs when a running exec session is forked.
- [x] Require a running branch for exec; do not implicitly resume paused
      branches.

### Attach and detach

- [ ] `ttos attach CONTAINER[@BRANCH]` for the init console.
- [ ] `ttos attach CONTAINER[@BRANCH] --exec EXEC_ID` for an exec session.
- [ ] Support stdin-only, stdout/stderr-only, and combined attachment modes.
- [ ] Support Docker-like detach keys, terminal resize, signal forwarding, and
      exit-status reporting.
- [ ] Support noninteractive streaming suitable for pipelines.
- [ ] Define multiple-reader and single-stdin-owner semantics.
- [ ] Define bounded output retention, backpressure, and log truncation while
      no client is attached.
- [ ] Reject fork while a transient host attachment lease is active in the
      first release.
- [ ] Make the branch immediately fork-eligible after every host attachment is
      detached, even if the guest process and internal console stay alive.
- [ ] On fork, clone console queues and terminal state but inherit no host
      attachment clients.

## Branch-aware process control

The existing `runsc exec` path reaches `control.ExecAsync`, which can operate on
a selected `kernel.Kernel`. The current loader wrapper nevertheless hardcodes
`Loader.k`, default-branch namespaces, and a single process/TTY map.

- [x] Add branch ID to execute and wait-PID paths.
- [ ] Add branch ID to general process-signal and terminal-control RPCs (the
      TimeTravel-specific signal RPC is already branch-aware).
- [ ] Move process bookkeeping from `Loader.processes` into each
      `sandboxBranch`.
- [ ] Reconstruct branch process records after fork by resolving cloned virtual
      TIDs, container IDs, process groups, and execution IDs.
- [x] Select mount and PID namespaces from the target branch's container init.
- [x] Use the target branch Kernel in `control.Proc` and `control.ExecAsync`.
- [ ] Share immutable OCI specs and seccomp configuration while keeping mutable
      process/TTY state branch-local.
- [x] Extend branch Kernel-lifetime leases to exec creation and wait, rejecting
      exec creation unless the selected branch is running.
- [ ] Add failure injection around FD import, namespace selection, process
      creation, publication, client disconnect, and wait cleanup.
- [ ] Test exec concurrently with pause, resume, fork, destroy, and sibling
      activity.

## Internal console architecture

Ordinary runsc stdio is host-backed and remains in the guest FD graph after the
CLI detaches. Closing only the host CLI is therefore insufficient for live-fork
eligibility. Guest processes must instead retain Sentry-internal endpoints;
host clients connect transiently through the control plane.

- [ ] Define a branch-local `ConsoleSession` object with stable session ID,
      stdin queue, stdout/stderr queues, terminal metadata, and attachment
      leases.
- [ ] For non-TTY processes, install internal Sentry pipes or equivalent queue
      endpoints at guest FDs 0, 1, and 2.
- [ ] For interactive processes, allocate a Sentry devpts PTY and install its
      slave as guest stdio.
- [ ] Keep the PTY master or pipe peer in the branch-local console service, not
      in a host `FileDescription`.
- [ ] Add pollable console read/write/resize/close RPCs and cancellation.
- [ ] Implement host-side stream pumping in `ttos`/`ttosd` without adding host
      FDs to the guest resource graph.
- [ ] Clone unread console data, termios, foreground process group, window size,
      and hangup state during live fork.
- [ ] Ensure post-fork input and output cannot cross branches.
- [ ] Test detach caused by normal command, detach keys, client crash, daemon
      restart, and network/socket failure.
- [x] Add `/dev/pts` and `/dev/ptmx` mount support required by interactive
      sessions.

## Mount eligibility framework

The initial loader now has a narrow OCI-spec allowlist for procfs, main-
`MemoryFile` tmpfs, and read-only Sentry sysfs. Replace this transitional gate
with an explicit decision for each reachable filesystem and mount:

```text
Clone             mutable Sentry-owned state, copied using page CoW
ShareImmutable    explicitly immutable external lower/resource
Reject            unsupported, externally mutable, or host/device-backed
```

- [ ] Add a VFS live-fork mount/filesystem eligibility interface returning the
      decision, stable identity, and rejection reason.
- [ ] Collect all mount decisions before any parent CoW mutation.
- [ ] Include mount path, filesystem type, source class, and reason in
      structured eligibility errors.
- [x] Replace the blanket `len(spec.Mounts) == 0` gate with an initial
      conservative allowlist that rejects every unclassified mount.
- [ ] Replace the spec-level allowlist with closure validation over every
      reachable mounted filesystem and resource.
- [ ] Verify recursive forks and arbitrary destruction orders for every allowed
      mount class.

### tmpfs, procfs, and sysfs

- [x] Admit main-`MemoryFile` tmpfs mounts, including `/dev/shm`, as the initial
      `Clone` class and verify file contents diverge CoW across branches.
- [x] Admit a private devpts mount, verify `/dev/ptmx` follows the standard
      symlink into it, and verify an open internal PTY is cloned rather than
      shared across branches.
- [ ] Test tmpfs contents, mappings, metadata, hardlinks, open-unlinked files,
      truncate, fallocate, and mount flags across branches.
- [ ] Admit procfs only after proving `/proc/self`, `/proc/PID`, `/proc/*/fd`,
      namespaces, mounts, clocks, and `/proc/net` resolve through the child
      Kernel rather than the source Kernel.
- [x] Initial procfs fork and parent/child `/proc/1/status` access coverage;
      exhaustive dynamic-file validation remains above.
- [x] Admit a read-only restricted Sentry-owned sysfs profile.
- [x] Keep host binds and TPU/NVIDIA device proxies rejected by the initial
      allowlist; other mirrored host sysfs paths remain rejected until
      separately proven branch-local or immutable.
- [ ] Test nested and repeated procfs/sysfs mounts and bind-remount flags.

### Read-only host paths

- [x] Admit a LISAFS/gofer bind only when the OCI mount and server connection
      are both read-only and directfs is disabled.
- [x] Borrow one live gofer client/root control FD per filesystem UniqueID.
- [x] Initial integration coverage reads the bind in parent, child, and
      grandchild branches, rejects writes, recursively forks it, and keeps the
      grandchild functional after deleting its source branch.
- [ ] Test multiple simultaneous read-only binds, open files, directory cursors,
      executable mappings, recursive fork, and descendant-first or
      ancestor-first destruction.
- [ ] Document read-only host binds as shared external views, not snapshots:
      host-side mutations may become visible to every branch.
- [ ] Add an opt-in immutable-source assertion and diagnostics, without claiming
      the runtime can prevent an independent host writer.

### Managed volumes

- [x] `ttos volume create NAME` for a forkable managed volume.
- [x] Support pure tmpfs managed volumes.
- [ ] Support immutable image/host lower plus main-MemoryFile tmpfs OverlayFS
      upper.
- [ ] Generalize the already-supported root OverlayFS live-fork tests to
      arbitrary submounts.
- [x] Fork every managed tmpfs volume together with its branch and verify
      parent/child/grandchild write isolation.
- [x] Preserve volume aliases and sharing among processes inside one branch
      without sharing writes across branches.
- [x] Reject writable host binds without a managed CoW upper.
- [x] Initial named-volume attachment, read-only aliases, metadata inspection,
      listing, in-use removal rejection, and removal.
- [ ] Add anonymous volumes, ownership initialization, quotas, and richer
      inspection.
- [x] Initial family-local semantics: attaching a name instantiates one tmpfs
      per sandbox family; the same name in unrelated families does not share
      data.
- [ ] Retain managed-volume contents across sandbox-family teardown. The first
      slice persists metadata only; contents remain entirely in Sentry memory.
- [ ] Add export/commit of a selected branch's managed volume without making
      disk checkpointing part of live fork.

## Image and storage backend

- [x] Use containerd's client APIs for registry pulls, OCI content, manifest
      resolution, and unpacking rather than implementing a Docker registry
      client from scratch.
- [ ] Use containerd leases and APIs for image removal and content garbage
      collection.
- [ ] Select a rootless-compatible snapshotter; prefer an immutable directory
      lower that can be exposed to runsc through a read-only gofer.
- [x] Cache content-addressed directory roots and construct the initial OCI
      spec from image config plus `ttos run` options.
- [ ] Preserve distinct image UID/GID ownership in rootless materialization,
      for example with an EROFS lower or an explicit ownership metadata scheme.
- [ ] Construct the full mount manifest once allowed mount classes are
      implemented.
- [ ] Place an in-memory OverlayFS upper over the immutable image lower by
      default.
- [ ] Track containerd content/snapshot leases for every live family and release
      them only after the last branch is destroyed.
- [ ] Keep branch-local mutable filesystem state owned by the Sentry, not by a
      host snapshotter that cannot participate in live fork.
- [ ] Add image pull/unpack tests using a local registry and no network-dependent
      test fixtures.

## `ttosd` and persistence

- [x] Initial direct-CLI persistent base-container metadata with created,
      running, and exited reconciliation against `runsc list`.
- [ ] Implement a local Unix-domain control service so console sessions,
      background pulls, event streams, and state survive individual CLI
      processes.
- [ ] Provide a rootless per-user socket and state directory by default.
- [ ] Persist family configuration, image references, managed-volume metadata,
      lineage, and tombstones; do not persist live memory as part of ordinary
      operation.
- [ ] Reconcile persisted metadata with containerd, runsc sandboxes, and shim
      processes after daemon restart.
- [ ] Provide events for branch creation, pause, resume, exit, destroy, exec,
      attach, and eligibility changes.
- [ ] Serialize conflicting lifecycle operations and make commands idempotent.
- [ ] Add a versioned local API so the CLI and daemon may be upgraded
      independently.
- [x] Use direct runsc invocation for the first lightweight client; later extend
      `containerd-shim-runsc-v1`, keeping the API boundary compatible with moving
      branch control into the shim later.

## Docker and containerd compatibility

- [ ] Keep `runsc install`/Docker operation working for the `main` branch where
      possible.
- [ ] Document that Docker cannot represent or control the TimeTravel branch
      tree and that `ttos` remains the source of truth for forks.
- [ ] Preserve the standard OCI runtime command surface for external managers.
- [ ] Prototype a containerd shim extension or side-channel API for branch
      lifecycle without changing the standard task-v2 API.
- [ ] Ensure containerd does not garbage-collect image snapshots while any
      TimeTravel branch still references their immutable lower.
- [ ] Consider a future nerdctl/Docker-compatible flag layer only after the
      branch-native `ttos` model is stable.

## Delivery sequence

### Milestone A: normal OCI bundles

- [ ] Per-mount eligibility framework.
- [x] Initial tmpfs, procfs, and restricted read-only sysfs mounts.
- [x] devpts and `/dev/ptmx` mounts for the internal TTY console.
- [x] Initial immutable read-only gofer bind.
- [ ] Multiple immutable read-only gofer binds and exhaustive access coverage.
- [x] Managed tmpfs submount volumes.
- [ ] Managed OverlayFS submount volumes.

Gate: an ordinary generated OCI bundle with `/proc`, `/sys`, `/dev`, `/dev/pts`,
read-only binds, and managed volumes forks without hand-editing its spec.

### Milestone B: branch-native exec

- [ ] Branch-local process bookkeeping and branch-aware execute/wait/signal.
- [x] Synchronous non-TTY `ttos exec` with branch-aware execute/wait.
- [ ] Eligibility recovery after an attached exec process exits and its imported
      host FDs are released.

Gate: commands can be launched and observed independently in parent, child, and
grandchild branches without PID or wait-status crossover.

### Milestone C: detachable console

- [ ] Internal non-TTY console sessions and streaming RPC.
- [ ] Detached exec and reattach.
- [ ] Internal devpts interactive console, resize, signals, and detach keys.
- [ ] Console state fork and branch-local I/O.

Gate: a user can detach from a still-running interactive or noninteractive
process, fork its branch, and then attach independently to both branches.

### Milestone D: Docker-like `ttos`

- [x] Initial containerd-backed image pull/unpack and managed image-root storage.
- [ ] Full run/create/start/stop/rm/inspect/logs/exec/attach UX.
- [x] Initial fork/pause/resume and beautified branch tree.
- [ ] Read-only fork eligibility check and richer branch-tree diagnostics.
- [ ] Rootless daemon lifecycle, recovery, events, documentation, and shell
      completion.

Gate: a fresh user can pull a Docker Hub image and exercise the complete
TimeTravel lifecycle without invoking runsc, ctr, runc, or editing an OCI
bundle.

## Open semantic decisions

- [ ] Exact `start` behavior for an exited in-memory branch.
- [ ] Whether an active host attachment always rejects fork or can be routed to
      one selected result branch.
- [ ] Output buffering versus blocking when no console client is attached.
- [ ] Multiple simultaneous attach clients and stdin ownership.
- [ ] Host-mutation semantics and warnings for shared read-only binds.
- [ ] Managed-volume sharing across unrelated families.
- [ ] Whether stopped branch filesystem state may be retained without retaining
      its Kernel and memory.
- [ ] Initial direct-runsc versus extended-containerd-shim deployment model.
