# TimeTravelOS `ttos` product and CLI roadmap

Status: TODO roadmap following the live-fork prototype.

Implementation status: an initial lightweight `ttos` client is available at
`//runsc/ttos:ttos`. It controls existing TimeTravel-enabled runsc OCI
sandboxes through runsc's JSON command surface and pulls OCI/Docker images
through containerd. Image-backed run/create lifecycle, `ttosd`, exec, attach,
mounts, and managed volumes remain TODO.

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
- [ ] Decide `start` semantics for an exited branch. The first release may
      support `start` only for created/stopped base containers and require a new
      branch or run for a destroyed in-memory state.
- [ ] Specify behavior when a family or branch name conflicts with an image,
      volume, or exec-session name.
- [ ] Store desired configuration separately from transient Sentry state so
      stopped containers can be inspected and removed.

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

- [ ] `ttos run [OPTIONS] IMAGE [COMMAND...]`.
- [ ] Support `--name`, environment variables, working directory, user,
      entrypoint override, resource limits, hostname, and rootless execution.
- [ ] Support foreground, `--detach`, and interactive `-i`/`-t` modes.
- [ ] `ttos create`, even if initially implemented as `run` without `start`.
- [ ] `ttos start CONTAINER` for created/stopped base containers.
- [x] `ttos stop CONTAINER[@BRANCH]` with TERM, timeout, then KILL semantics.
- [x] `ttos kill CONTAINER[@BRANCH] [SIGNAL]`.
- [ ] `ttos rm CONTAINER` and explicit policy for live descendant branches.
- [ ] `ttos inspect CONTAINER[@BRANCH]` exposing family, branch, process,
      resource-eligibility, timing, and mount information.
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
- [ ] `ttos ls --tree` with a compact Unicode tree, current state, PID 1 state,
      parent, fork latency, source downtime, memory sharing, and creation time.
- [x] `ttos ls --json` with a stable machine-readable schema.
- [x] Mark deleted historical parents in tree output without reparenting their
      descendants.
- [ ] Display structured fork-ineligibility issues before attempting a fork.
- [ ] Add `ttos fork --check SOURCE` for a read-only eligibility report.
- [ ] Add shell completion for family, branch, image, volume, and session names.

### Exec

- [ ] `ttos exec [OPTIONS] CONTAINER[@BRANCH] COMMAND...`.
- [ ] Support environment, cwd, UID/GID, supplementary groups, capabilities,
      and an OCI process JSON input.
- [ ] Support synchronous noninteractive commands with exit-status forwarding.
- [ ] Support interactive `-i`/`-t` commands.
- [ ] Support `--detach`, returning a stable branch-qualified execution ID.
- [ ] Add `ttos exec ls`, `ttos exec wait`, `ttos exec kill`, and reattach by
      execution ID.
- [ ] Preserve virtual PIDs and exec IDs when a running exec session is forked.
- [ ] Require a running branch for exec in the first release; do not implicitly
      resume paused branches.

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

- [ ] Add branch ID to execute, wait-PID, process signal, and terminal-control
      RPCs.
- [ ] Move process bookkeeping from `Loader.processes` into each
      `sandboxBranch`.
- [ ] Reconstruct branch process records after fork by resolving cloned virtual
      TIDs, container IDs, process groups, and execution IDs.
- [ ] Select mount and PID namespaces from the target branch's container init.
- [ ] Use the target branch Kernel in `control.Proc` and `control.ExecAsync`.
- [ ] Share immutable OCI specs and seccomp configuration while keeping mutable
      process/TTY state branch-local.
- [ ] Extend `beginControl` lifetime leases to exec creation and wait.
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
- [ ] Add `/dev/pts` and `/dev/ptmx` mount support required by interactive
      sessions.

## Mount eligibility framework

The current loader admits filesystem sharing with one sandbox-wide boolean and
rejects every additional OCI mount. Replace this with an explicit decision for
each reachable filesystem and mount:

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
- [ ] Replace `len(spec.Mounts) == 0` with closure validation over every mount.
- [ ] Verify recursive forks and arbitrary destruction orders for every allowed
      mount class.

### tmpfs, procfs, and sysfs

- [ ] Admit main-`MemoryFile` tmpfs mounts as `Clone`.
- [ ] Test tmpfs contents, mappings, metadata, hardlinks, open-unlinked files,
      truncate, fallocate, and mount flags across branches.
- [ ] Admit procfs only after proving `/proc/self`, `/proc/PID`, `/proc/*/fd`,
      namespaces, mounts, clocks, and `/proc/net` resolve through the child
      Kernel rather than the source Kernel.
- [ ] Admit a restricted Sentry-owned sysfs profile.
- [ ] Keep host PCI, VFIO, accelerator, TPU, device proxy, and other mirrored
      host sysfs paths rejected until separately proven branch-local or
      immutable.
- [ ] Test nested and repeated procfs/sysfs mounts and bind-remount flags.

### Read-only host paths

- [ ] Admit a LISAFS/gofer bind only when the OCI mount and server connection
      are both read-only and directfs is disabled.
- [ ] Borrow one live gofer client/root control FD per filesystem UniqueID.
- [ ] Test multiple simultaneous read-only binds, open files, directory cursors,
      executable mappings, recursive fork, and descendant-first or
      ancestor-first destruction.
- [ ] Document read-only host binds as shared external views, not snapshots:
      host-side mutations may become visible to every branch.
- [ ] Add an opt-in immutable-source assertion and diagnostics, without claiming
      the runtime can prevent an independent host writer.

### Managed volumes

- [ ] `ttos volume create NAME` for a forkable managed volume.
- [ ] Support pure tmpfs managed volumes.
- [ ] Support immutable image/host lower plus main-MemoryFile tmpfs OverlayFS
      upper.
- [ ] Generalize the already-supported root OverlayFS live-fork tests to
      arbitrary submounts.
- [ ] Fork every managed volume upper together with its branch.
- [ ] Preserve volume aliases and sharing among processes inside one branch
      without sharing writes across branches.
- [ ] Reject writable host binds without a managed CoW upper.
- [ ] Define `ttos run --volume NAME:/path`, anonymous volumes, read-only
      attachment, ownership, quotas, inspect, list, and removal.
- [ ] Define whether a volume belongs to one family or may be attached to
      multiple unrelated families; default to one family for deterministic
      fork semantics.
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
- [ ] Construct OCI specs and mount manifests from image config plus `ttos run`
      options.
- [ ] Place an in-memory OverlayFS upper over the immutable image lower by
      default.
- [ ] Track containerd content/snapshot leases for every live family and release
      them only after the last branch is destroyed.
- [ ] Keep branch-local mutable filesystem state owned by the Sentry, not by a
      host snapshotter that cannot participate in live fork.
- [ ] Add image pull/unpack tests using a local registry and no network-dependent
      test fixtures.

## `ttosd` and persistence

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
- [ ] tmpfs, procfs, restricted sysfs, and devpts mounts.
- [ ] Multiple immutable read-only gofer binds.
- [ ] Managed tmpfs/OverlayFS submount volumes.

Gate: an ordinary generated OCI bundle with `/proc`, `/sys`, `/dev`, `/dev/pts`,
read-only binds, and managed volumes forks without hand-editing its spec.

### Milestone B: branch-native exec

- [ ] Branch-local process bookkeeping and branch-aware execute/wait/signal.
- [ ] Synchronous non-TTY `ttos exec`.
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

- [ ] containerd-backed image pull/unpack and managed storage.
- [ ] Full run/create/start/stop/rm/inspect/logs/exec/attach UX.
- [ ] Fork/pause/resume/check and beautified branch tree.
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

