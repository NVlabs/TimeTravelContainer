# TimeTravelOS `ttos` product and CLI roadmap

Status: TODO roadmap following the live-fork prototype.

Implementation status: an initial lightweight `ttos` client is available at
`//runsc/ttos:ttos`. It controls existing TimeTravel-enabled runsc OCI
sandboxes through runsc's JSON command surface, pulls OCI/Docker images through
containerd, generates rootless image-backed OCI bundles, and implements the
initial run/create/start/wait/restart/inspect/list/remove lifecycle with
persistent metadata, plus synchronous, detached, and TTY exec into a selected
live branch, with branch-local managed-session list, wait, signal, log, and
reattach support.
Docker-style `ttos container` aliases cover the ordinary lifecycle surface;
`start` and `wait` accept multiple containers and preserve argument-order
output. Container inspect uses Docker-shaped JSON and Go templates while adding
a `TTOS` section with the family tree, fork timings, per-branch processes, and
optional live-fork eligibility reports. Effective Docker entrypoint, command,
user, and image labels are retained in the generated bundle for later
inspection. Generated bundles include procfs, restricted read-only sysfs,
tmpfs-backed `/dev` and `/dev/shm`, and a private devpts instance with
`/dev/ptmx`. Docker-compatible `--tmpfs` and `--mount type=tmpfs` add arbitrary
Sentry-owned tmpfs mounts with mode, size, and access options; TTOS extensions
`--mount type=proc|procfs|sysfs` add alternate Sentry virtual-filesystem
views. Existing host paths can be attached read-only with Docker-style `-v`
or `--mount` syntax. Named managed tmpfs volumes can be attached at one or
more paths, and their state forks CoW with each live branch. Containers default
to isolated `--network none`; Docker-style `--network/--net host` persists
hostinet configuration across start/restart and is rejected by live-fork
validation as an external resource. An opt-in internal-console substrate now provides branch-local Sentry pipes for
non-TTY containers and Sentry devpts PTYs for terminal containers, with
nonblocking control RPCs, explicit stdin close, terminal resize, and saved
console state.
A persistent, single-consumer `runsc console-follow` sidecar drains each branch
into a Docker-multiplexed host log with frame-safe bounded rotation (defaulting
to two 16 MiB generations and configurable with Docker-style `--log-opt
max-size=...` and `max-file=...`); `ttos attach` and `ttos logs [-f] [--tail
all|N]` replay, tail, or follow rotations independently. This supports optional
stdin forwarding with Docker-style Ctrl-P, Ctrl-Q detach, foreground
`run`/`start`, detached output draining, and multiple output readers without
placing host FDs in the guest resource graph. `ttosd`, daemon-owned stdin
ownership, persistent managed-volume backing, and managed OverlayFS volumes
remain TODO.

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
- [x] Default rootless pulls to the containerd content store without requesting
      snapshotter unpack; retain explicit `--unpack` for environments where the
      selected snapshotter's mount requirements are available.
- [x] Make `run` and `create` use Docker-compatible `--pull=missing` by
      default, reusing local tag or unambiguous image-ID records without
      registry access. Support explicit `--pull=always` refresh and
      offline-enforcing `--pull=never`, with requested-platform matching.
- [ ] Integrate registry authentication and Docker-compatible credential
      helpers without storing plaintext credentials in the state database.
- [ ] Show pull/unpack progress and content reuse.
- [x] `ttos images` with reference, digest, platform, unpacked state, size, and
      creation time.
- [x] `ttos image inspect`, `ttos image rm`, and content garbage collection.
      `ttos image ls`, top-level `ttos images`, and `ttos rmi` aliases
      accept familiar Docker Hub names and unique image-ID prefixes. Listing
      supports Docker-style tables, `-q`, `--no-trunc`, and JSON while
      exposing the target platform and whether TTOS has materialized its own
      immutable root.
- [x] Protect image removal with persistent-container usage checks. A forced
      removal may untag containerd records while retaining TTOS roots required
      by existing containers. Removing the last alias of an unused digest
      synchronously requests containerd content collection and removes all
      platform-specific TTOS roots for that digest.

### Container lifecycle

- [x] `ttos run [OPTIONS] IMAGE [COMMAND...]` (initial foreground/detached,
      non-TTY slice with procfs, `/dev` tmpfs, and restricted sysfs mounts).
- [x] Support `--name`, Docker-style `--env`/`-e` values and host imports,
      `--env-file` (including removal of inherited image variables),
      `--label`/`-l`, and `--label-file`, with direct flags overriding file and
      image values. Persist the effective environment and labels through
      create/start/restart/fork and expose them through inspect. Also support
      working directory, named or numeric user, entrypoint override, hostname,
      and rootless execution.
- [x] Keep single-ID user-namespace rootless execution as the default without
      requiring `newuidmap`, `newgidmap`, or subordinate-ID ranges. Provide an
      explicit `--unsafe-no-host-isolation` fallback for trusted workloads on
      hosts that prohibit user namespaces; this runs Sentry/gofer as the
      caller without host namespaces or chroots while retaining Sentry guest
      namespaces and credentials.
- [x] Support Docker-style `--network/--net none|host` on run/create, persist
      the selected mode across start/restart, and report it through inspect.
      `none` remains the fork-safe default. Host networking deliberately has no
      port-publishing layer and is ineligible for live fork because it exposes
      the external host network stack.
- [x] Support Docker `run/create --read-only`, persist it in the OCI root and
      Docker inspect view, and retain it across start/restart/live fork while
      writable tmpfs and managed-volume submounts remain usable and fork CoW.
- [x] Support Docker `run/create --stop-signal` and `--stop-timeout`, inherit
      image `STOPSIGNAL` when no override is supplied, persist both values in
      container metadata and Docker inspect, and honor them in `stop` and
      `restart` unless a command-level timeout override is supplied.
      Successful main-branch waits also reconcile the final exit state, code,
      and finish time into persistent inspect metadata.
- [x] Support existing host paths through `-v HOST:CONTAINER:ro` and
      `--mount type=bind,...,readonly`.
- [x] Support Docker-compatible `--tmpfs DEST[:OPTIONS]` and
      `--mount type=tmpfs,target=...` with read/write policy, octal mode, and
      byte-size limits. Support TTOS extension mounts for alternate private
      procfs and restricted sysfs views through
      `--mount type=proc|procfs|sysfs,target=...`; persist all three through
      create/start/restart/fork and project them through inspect.
- [x] Support family-local named tmpfs volumes through `-v NAME:CONTAINER[:ro]`
      and `--mount type=volume,...`, including repeated aliases of one volume.
- [ ] Add CPU, memory, process-count, and other resource-limit flags.
- [x] Support foreground, `--detach`, and attached non-TTY `-i` modes.
- [x] Support `run/create -t` with a detachable Sentry-internal PTY, Docker-style
      combined `-it`/`-dit`, raw interactive attach, resize propagation, and
      branch-local terminal state.
- [x] Support Docker-style `exec -t`/`-it` with a transient Sentry-internal
      devpts PTY, raw host-terminal mode, resize propagation, stdin close, exact
      exit status, and execution in any running live branch.
- [x] `ttos create` as persistent image-root and OCI-bundle preparation.
- [x] `ttos start CONTAINER` for created/exited base containers, detached by
      default with optional `--attach`. Multiple detached containers start in
      argument order, and stale stopped runsc records are removed before OCI
      recreation while live duplicate IDs remain protected.
- [x] Docker-style `ttos container` aliases for run, create, start, attach,
      logs, exec, inspect, rm, ls/ps, top, pause/unpause, kill, stop, wait, and
      restart.
- [x] `ttos wait [--condition not-running] CONTAINER[@BRANCH]...` starts all
      waits concurrently, prints Docker exit codes in argument order, and
      retains branch-aware wait semantics.
- [x] `ttos restart CONTAINER...` for created, exited, running, or paused base
      containers, including TERM/timeout/KILL behavior. Restart rejects an
      individual branch because destroyed in-memory branches cannot be
      reconstructed. Live descendants or in-memory managed volumes require
      explicit `--force`; persistent volume restart parity remains blocked on
      persistent managed-volume backing.
- [x] `ttos stop CONTAINER[@BRANCH]` with TERM, timeout, then KILL semantics.
- [x] `ttos kill CONTAINER[@BRANCH] [SIGNAL]`.
- [x] Initial `ttos rm CONTAINER`: remove stopped/created family metadata,
      reject a live family unless `--force` is given, and require `branch rm`
      for an individual non-main branch.
- [ ] Finalize forced-removal and tombstone policy for live descendant
      branches once persistent lineage is implemented.
- [x] Docker-shaped `ttos inspect CONTAINER...` covering identity, image,
      process configuration, lifecycle state, sandbox PID, graph driver, and
      user mounts. Support Docker-style `--format/-f`, line-delimited
      `--format json`, and `--type container`.
- [x] Add a `TTOS` inspect extension containing persistent metadata, the live
      family/branch tree, fork timing, and observational process lists for
      every running or paused branch. `--fork-check` explicitly adds a
      per-branch resource-eligibility report without making ordinary inspect
      mutate pause state.
- [x] Initial `ttos logs CONTAINER[@BRANCH]` and `logs -f`, preserving separate
      stdout/stderr streams and backed by branch-local persistent console logs.
- [x] Add Docker-compatible `logs --tail all|N`, including global logical-line
      ordering across the retained and active log generations and `--tail 0 -f`.
- [x] Persist Docker-style `run/create --log-opt max-size=SIZE` and
      `max-file=COUNT` and apply them to every branch sidecar.
- [ ] Add Docker-compatible `--since`, `--until`, timestamps, details,
      and explicit logging-driver policy.
- [x] Initial `ttos ps [-a|-q]` flat family/branch view and branch-qualified
      `ttos top [--json] CONTAINER[@BRANCH]` process listing.
- [x] Add Docker-style `ps --filter/-f` for name, ID, status, ancestor image,
      managed volume, and branch, with OR within one filter key and AND across
      keys. Add Go-template and line-delimited JSON formatting, `--latest/-l`,
      `--last/-n`, and `--no-trunc` image IDs.
- [x] Carry authoritative branch creation timestamps from the Sentry and merge
      persistent image, digest, volume, and container-creation metadata into
      live family rows. Recent selection includes non-running states, matching
      Docker's latest/last behavior.
- [ ] Add writable/virtual size accounting and pass-through process-selection
      options for `top`.

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
- [x] Display structured PID/FD-or-mapping fork-ineligibility issues before
      attempting a fork.
- [x] Add `ttos fork --check [--json] SOURCE` as a read-only eligibility
      report with script-friendly success/failure status.
      The report is an advisory snapshot of the current pre-mutation network,
      resource, and mapping graph. A subsequent fork can still lose a race or
      fail a later VFS/state-serialization check.
- [ ] Add shell completion for family, branch, image, volume, and session names.

### Exec

- [x] Initial `ttos exec [OPTIONS] CONTAINER[@BRANCH] COMMAND...` selecting a
      live branch through `runsc exec --branch`.
- [x] Support environment, cwd, and UID/GID overrides in the initial CLI.
- [x] Add supplementary groups, capabilities, and an OCI process JSON input to
      `ttos exec`; process JSON controls the complete process and terminal mode.
- [x] Support synchronous non-TTY commands with output streaming and
      exit-status forwarding; optionally import stdin with `-i`.
- [x] Support TTY-interactive `-t` commands through the internal console (`-i`
      without a TTY is supported for synchronous exec).
- [x] Support `--detach`, returning a stable branch-qualified execution ID.
- [x] Add a Sentry-owned branch-local exec registry with stable IDs, terminal
      metadata, retained exit status, and low-level list, wait, and signal RPCs.
- [x] Add `ttos exec ls`, `ttos exec wait`, and `ttos exec kill`, plus
      branch-qualified reattach and retained logs by execution ID.
- [x] Preserve virtual PIDs and exec IDs when a running exec session is forked.
- [x] Require a running branch for exec; do not implicitly resume paused
      branches.

### Attach and detach

- [x] Initial non-TTY `ttos attach CONTAINER[@BRANCH]` for the init console,
      with stdout/stderr streaming, optional `-i`, and exit-status forwarding.
- [x] `ttos attach --exec EXEC_ID CONTAINER[@BRANCH]` and
      `ttos logs --exec EXEC_ID CONTAINER[@BRANCH]` for managed exec sessions.
- [x] Support output-only and combined attachment by default/with `-i`, plus
      stdin-only, stdout-only, and stderr-only selection through `--no-stdin`,
      `--no-stdout`, and `--no-stderr`. Unselected output is still drained so
      console retention and wait behavior remain correct.
- [x] Support Docker-like detach keys for non-TTY stdin attachment, including
      Ctrl-P, Ctrl-Q by default and `ttos attach --detach-keys` overrides.
- [x] Support TTY detach keys, terminal resize/SIGWINCH propagation, and
      line-discipline signal delivery from an attached raw terminal.
- [x] Initial noninteractive streaming suitable for pipelines.
- [x] Allow multiple independent stdout/stderr readers without consuming each
      other's output; one locked sidecar is the sole Sentry-pipe reader.
- [x] Initial single-stdin-owner enforcement with one advisory host lock per
      branch; detach, exit, or client death releases it without adding a host FD
      to forked guest state.
- [ ] Move stdin leases into `ttosd` and define explicit takeover behavior.
- [x] Drain output while no client is attached so guest writers do not stop at
      the Sentry pipe capacity; retain output in a persistent host log.
- [x] Initial bounded retention: rotate only between complete multiplexed
      frames, keeping a 16 MiB current generation and one retained generation;
      readers follow inode replacement without duplicating frames.
- [x] Make retention size and generation count configurable, persist the
      settings per container, and report when a reader falls behind retention.
- [ ] Define explicit backpressure policy and surface cumulative truncation
      counters through inspect/events.
- [x] Allow fork while output readers are attached: read RPCs serialize against
      fork, readers follow a host log, and a new child sidecar starts afterward.
- [x] Keep branches fork-eligible before, during, and after output attachment;
      host readers never enter the guest resource graph.
- [x] On fork, clone console queues and start a new branch-local drain while
      inheriting no host attachment clients.

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

Persistent logs are host-side observations only. They are neither part of live
branch state nor read during fork; process, kernel, console-queue, and filesystem
state still forks entirely in memory.

- [x] Define a saved branch-local non-TTY console object with a stable session
      ID and stdin/stdout/stderr queues.
- [x] Add terminal metadata to the Sentry-owned managed exec registry.
- [x] Allocate and expose detached exec-session IDs in `ttos`.
- [x] For non-TTY processes, install internal Sentry pipes or equivalent queue
      endpoints at guest FDs 0, 1, and 2.
- [x] For init interactive processes, allocate a Sentry devpts PTY and install its
      slave as guest stdio.
- [x] Allocate a distinct Sentry devpts PTY for each attached interactive exec
      process and install one shared slave description at guest FDs 0, 1, and 2.
- [x] Keep pipe peers in the branch-local Kernel console registry, not
      in a host `FileDescription`.
- [x] Add branch-serialized nonblocking console read/write RPCs.
- [x] Add branch-serialized stdin close and verify that the guest observes EOF.
- [ ] Add readiness waiting and cancellation. Resize is implemented for init and
      attached exec PTYs.
- [x] Add a long-lived low-level multiplexed `console-follow` stream so one
      process drains both outputs without spawning one `runsc` per read.
- [x] Start one advisory-locked sidecar per branch, persist its stream, diagnose
      premature failure, rotate at complete-frame boundaries with bounded
      two-generation retention, and let attach/log readers follow without guest
      FDs.
- [x] Register the main console follower before OCI start, atomically persist
      terminal wait status, retain exited branch status for repeated waits, and
      preserve exact stdout/stderr plus one EOF per stream for immediate exits.
      The image matrix stress-checks 256 KiB per stream in both foreground and
      persistent-log paths.
- [ ] Move sidecar ownership into `ttosd`, add restart reconciliation, and
      replace the sidecar's polling RPC loop with readiness-driven transport.
- [x] Clone unread non-TTY console data and pipe connectivity during live fork,
      including recursively forked branches.
- [x] Clone init- and exec-console PTY termios, foreground process groups,
      window sizes, queues, hangup state, and controlling-terminal identity.
- [x] Ensure post-fork non-TTY input and output cannot cross branches and that a
      descendant console survives deletion of its source branch.
- [x] Keep output draining after an ordinary attach client exits.
- [x] Test detach-key parsing and detection across stdin read boundaries.
- [ ] Test client crash during stdin, sidecar crash/restart, daemon restart, and
      control-socket failure.
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
- [x] Generate arbitrary user-requested tmpfs, procfs, and restricted sysfs
      mounts from Docker-style CLI flags; verify mode/size, inspect projection,
      restart persistence, fork eligibility, and parent/child tmpfs CoW with an
      unmodified Alpine image.
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
- [x] Use target-guarded synchronous containerd image deletion APIs for image
      removal and newly unreachable content collection.
- [ ] Add containerd leases for materialization and live-family content
      retention.
- [ ] Select a rootless-compatible snapshotter; prefer an immutable directory
      lower that can be exposed to runsc through a read-only gofer.
- [x] Cache content-addressed directory roots and construct the initial OCI
      spec from image config plus `ttos run` options.
- [x] Preserve distinct image UID/GID ownership in rootless materialization with
      a versioned sidecar manifest served only by the immutable root gofer
      connection. Physical files remain caller-owned; guest stat and Sentry
      permission checks receive the OCI-layer IDs without subordinate ranges.
      Materialized roots default to guest mode 0755 so non-root image users can
      traverse `/` regardless of private host cache-parent modes.
- [ ] Construct the full mount manifest once allowed mount classes are
      implemented.
- [x] Place an in-memory OverlayFS upper over the immutable image lower by
      default.
- [ ] Track containerd content/snapshot leases for every live family and release
      them only after the last branch is destroyed.
- [ ] Keep branch-local mutable filesystem state owned by the Sentry, not by a
      host snapshotter that cannot participate in live fork.
- [x] Add a manual networked end-to-end matrix for unmodified BusyBox, Alpine,
      Python, nginx, and Redis images, covering rootless prerequisites, OCI startup,
      Docker-like lifecycle commands and inspect across created, running,
      forked, paused, and exited states; live fork; managed-volume CoW;
      read-only host binds; distinct Redis lower-layer ownership; exact quick-exit
      console integrity; and the explicit no-userns fallback.
- [ ] Add image pull/unpack tests using a local registry and no network-dependent
      test fixtures.

## `ttosd` and persistence

- [x] Initial direct-CLI persistent base-container metadata with created,
      running, and exited reconciliation against `runsc list`.
- [x] Initial per-branch console sidecars and logs survive the CLI process that
      launched them; advisory locks prevent duplicate pipe consumers and log
      readers diagnose premature sidecar exit.
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

- [x] Internal non-TTY console sessions and nonblocking branch RPC substrate.
- [x] Initial direct-CLI non-TTY host-side streaming and attach command.
- [x] Persistent single-consumer branch sidecars, multi-reader log replay/follow,
      and detached output draining without fork-inhibiting host FDs.
- [x] Persistent single-consumer exec-session sidecars, stable IDs and PIDs
      across fork, and independent retained exit status per branch.
- [x] Frame-safe bounded log rotation and rotation-aware replay/follow.
- [x] Persistent configurable log size/generation retention.
- [ ] Daemon-owned stdin leases and daemon-owned transport/reconciliation.
- [x] Detached exec creation with fork-aware per-session output drains.
- [x] Reattach, retained logs, and exec-session detach keys.
- [x] Internal devpts interactive consoles for init and attached exec, including
      host-size propagation and branch-local PTY signal semantics.
- [x] Non-TTY console state fork and branch-local I/O.
- [x] Init and attached exec-session PTY state fork and branch-local I/O.

Gate: a user can detach from a still-running interactive or noninteractive
process, fork its branch, and then attach independently to both branches.

### Milestone D: Docker-like `ttos`

- [x] Initial containerd-backed image pull/unpack and managed image-root storage.
- [ ] Full run/create/start/stop/rm/inspect/logs/exec/attach UX.
- [x] Initial fork/pause/resume and beautified branch tree.
- [x] Read-only fork eligibility check.
- [ ] Richer branch-tree diagnostics.
- [ ] Rootless daemon lifecycle, recovery, events, documentation, and shell
      completion.

Gate: a fresh user can pull a Docker Hub image and exercise the complete
TimeTravel lifecycle without invoking runsc, ctr, runc, or editing an OCI
bundle.

## Open semantic decisions

- [ ] Exact `start` behavior for an exited in-memory branch.
- [x] Output attachment does not reject or migrate with fork: every branch owns
      one drain, while host readers remain attached only to the selected log.
- [x] Drain to a persistent host log instead of blocking guest output merely
      because no console client is attached; the initial policy keeps a 16 MiB
      current generation plus one retained generation.
- [x] Expose log size and generation-count policy in the user-facing CLI.
- [x] Support multiple simultaneous output readers.
- [x] Use one exclusive manager-level stdin owner per branch.
- [ ] Define daemon-reconciled stdin lease takeover behavior.
- [ ] Host-mutation semantics and warnings for shared read-only binds.
- [ ] Managed-volume sharing across unrelated families.
- [ ] Whether stopped branch filesystem state may be retained without retaining
      its Kernel and memory.
- [ ] Initial direct-runsc versus extended-containerd-shim deployment model.
