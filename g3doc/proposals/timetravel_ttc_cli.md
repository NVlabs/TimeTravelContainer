# TimeTravelContainer `ttc` product and CLI roadmap

Status: implemented for the documented rootless systrap scope. The original
tmpfs-plus-tar managed-volume prototype was removed before release and replaced
by the direct-to-disk `ttcvol` filesystem specified in
[ttc_disk_backed_volumes.md](ttc_disk_backed_volumes.md).

Implementation status: the roadmap is implemented. The `ttc` client is available at
`//runsc/ttc:ttc`. It controls existing TimeTravel-enabled runsc OCI
sandboxes through runsc's JSON command surface, pulls OCI/Docker images directly
into a native content store with Docker config and credential-helper authentication, generates
rootless image-backed OCI bundles, and implements the
run/create/start/wait/restart/inspect/list/remove lifecycle with
persistent metadata and immutable branch lineage/tombstones, plus synchronous, detached, and TTY exec into a selected
live branch, with branch-local managed-session list, wait, signal, log, and
reattach support.
Docker-style `ttc container` aliases cover the ordinary lifecycle surface;
`start` and `wait` accept multiple containers and preserve argument-order
output. Container inspect uses Docker-shaped JSON and Go templates while adding a
`TTC` section with the family tree, fork timings, and per-branch processes.
Live-fork failures return transactional resource diagnostics. Bash, Zsh, and
Fish completion scripts
resolve live families/branches, images, volumes, and exec sessions. Branch trees
show Sentry-reported PID 1 state and live CoW-sharing status alongside persisted
creation/deletion times and immutable lineage. Timestamped retained console
records support Docker-style log time filters and per-line timestamp output.
Effective Docker entrypoint, command, user, and image labels are retained in the
generated bundle for later inspection. Generated bundles include procfs,
restricted read-only sysfs, tmpfs-backed `/dev` and `/dev/shm`, and a private
devpts instance with `/dev/ptmx`. Docker-compatible `--shm-size` configures the
`/dev/shm` tmpfs and is retained across restart and live fork. Docker-compatible
`--tmpfs` and `--mount type=tmpfs` add arbitrary Sentry-owned tmpfs mounts with
mode, size, and access options; TTC extensions
`--mount type=proc|procfs|sysfs` add alternate Sentry virtual-filesystem
views. Existing host paths can be attached read-only with Docker-style `-v`
or `--mount` syntax. An ordinary writable host mount places a persistent,
branch-local `ttcvol` upper over the live read-only host lower; guest writes
never reach the host. The `:copy` extension imports the source into a detached
private repository during creation, using reflinks opportunistically and
falling back to streamed copies.
Named and anonymous disk-backed managed volumes can be attached through
Docker-compatible syntax, initialized from image contents, and fork their
immutable metadata root plus regular-file backing objects CoW with each live
branch. `ttc volume create|ls|inspect|rm` manages repositories. Advanced usage
accounting, import/export, and branch promotion remain roadmap items.
Containers default
to isolated `--network none`; Docker-style `--network/--net host` persists
hostinet configuration across start/restart and is rejected by live-fork
validation as an external resource. Rootless `--network ttc` adds a
Sentry-managed `ttc0` virtual NIC, optional persistent `--ip` and
`--mac-address`, optional IPv4 TCP/UDP user-mode NAT with
`--nat`, and persistent IPv4 TCP publication with `-p`/`--publish`. Live fork
keeps the source proxy and connections, gives the child a fresh proxy realm,
and resets inherited external child TCP flows instead of rejecting the fork.
`ttc port publish|ls|unpublish` manages independent persistent listeners for
the main branch or any existing live child.
An opt-in internal-console substrate now provides branch-local Sentry pipes for
non-TTY containers and Sentry devpts PTYs for terminal containers, with
nonblocking control RPCs, explicit stdin close, terminal resize, and saved
console state.
A persistent, single-consumer `runsc console-follow` sidecar drains each branch
into a Docker-multiplexed host log with frame-safe bounded rotation (defaulting
to two 16 MiB generations and configurable with Docker-style `--log-opt
max-size=...` and `max-file=...`); `ttc attach` and `ttc logs [-f] [--tail
all|N]` replay, tail, or follow rotations independently. This supports optional
stdin forwarding with Docker-style Ctrl-P, Ctrl-Q detach, foreground
`run`/`start`, detached output draining, and multiple output readers without
placing host FDs in the guest resource graph. `run` and `create` accept an explicit
`--log-driver local|json-file|none` policy. `local` and `json-file` both use
TTC versioned timestamped records (the latter is a Docker CLI compatibility
alias, not a promise of host JSON files). Docker-style `labels`, `labels-regex`,
`env`, and `env-regex` selectors resolve immutable attributes from the final
container configuration and `logs --details` prefixes them in sorted order;
`none` disables `ttc logs` while the
private attach transport remains available. A rootless per-user `ttcd` now
serves a versioned Unix-socket API, proxies command streams, and
reconciles persisted families plus missing console drainers after restart;
startup is explicit so its fixed isolation mode is never inferred from a
client request, and `--direct` remains an explicit recovery path.
Explicit commit/import, quiescent main-branch snapshots before stop, restart,
and sole-branch forced removal, plus a Sentry post-init-exit snapshot hook are
implemented. The Sentry retains the mount namespace through init teardown,
writes container-specific pending archives, and publishes readiness markers
only after successful serialization and sync; TTC validates and atomically
promotes completed archives.

This document records the product, runtime, filesystem, process-control, and
console work required to turn the TimeTravelContainer gVisor prototype into a
Docker-like local container manager. The underlying live-fork design remains in
[timetravel_live_fork.md](timetravel_live_fork.md).

## Product direction

`runsc` is intentionally a low-level OCI runtime. Upstream gVisor normally gets
a friendly interface by registering `runsc` as a Docker runtime (`runsc
install`, then `docker run --runtime=runsc ...`) or by installing
`containerd-shim-runsc-v1` as the `io.containerd.runsc.v1` runtime handler. In
those configurations Docker or containerd owns image pulling, content storage,
OCI bundle generation, snapshots, volumes, and ordinary lifecycle UX.

TimeTravelContainer needs a richer model than Docker's one container/one task view: a
container family owns a tree of simultaneously live branches, and every branch
has its own process state, lifecycle state, filesystem upper, internal resource
graph, and console sessions. The recommended architecture is therefore:

```text
ttc CLI
   |
   +-- ttcd local control service
         |
         +-- native OCI image store
         |     +-- registry/auth and Docker Hub pulls
         |     +-- verified content-addressed blobs and image metadata
         |     +-- transactional family pins and synchronous garbage collection
         |
         +-- TimeTravel-enabled runsc/shim
               +-- SandboxFamily and branch lifecycle RPCs
               +-- branch-local exec/wait/signal
               +-- branch-local console streaming
               +-- mount eligibility and live-fork validation
```

Docker Engine should remain an optional compatibility frontend, not the source
of truth for TimeTravel branches. A future Docker integration may expose only
the `main` branch and leave `ttc` responsible for branch operations. The
current `runsc install` path is retained for conventional main-only containers,
but Docker and `ttc` must not concurrently mutate the same sandbox: Docker has
no object or state representation for branch IDs, lineage, fork validation
outcomes,
branch-local filesystems, or different simultaneous branch lifecycle states.

## Object and state model

- [x] Define a stable 64-hex family ID and distinct user-facing container name.
- [x] Define branch selectors, initially `<container>@<branch>`, with `main` as
      the default branch.
- [x] Define immutable branch creation lineage. `ParentID` remains historical
      even if an ancestor is destroyed.
- [x] Persist branch tombstones and lineage metadata so `ttc ls --tree` can show
      descendants whose source branch was deleted.
- [x] Add an `exited` branch state and reconcile runtime state after virtual PID
      1 exits.
- [x] Distinguish family state, branch state, and exec-session state.
- [x] Define `stop` as process termination, not suspension; `pause`/`resume`
      retain memory and kernel state. The current `main` init remains the sandbox
      lifetime anchor, so its exit quiesces and exits all descendant branches.
- [x] Define initial `start` semantics: a created or exited base container
      starts from its persistent OCI bundle and immutable image root; destroyed
      in-memory branch state is not reconstructed.
- [x] Use typed namespaces: family names and IDs conflict globally with other
      families; branch IDs conflict within one family and immutable lineage; exec
      session IDs conflict within one branch. Image references and managed-volume
      names are independent namespaces, so they may equal a family, branch, or
      exec name without ambiguity because commands and selector delimiters carry
      the object type.
- [x] Store initial desired configuration and lifecycle metadata separately
      from transient Sentry state so created/stopped containers can be listed,
      inspected, restarted, and removed.
- [x] Move default persistent-state ownership behind `ttcd`; metadata files
      use synchronized temp-file replacement, and structural requests use a
      durable intent/result journal. Keep `--direct` as an explicit recovery
      path rather than a second background owner.

## `ttc` command surface

### Images

- [x] `ttc pull IMAGE` and Docker-compatible `ttc image pull IMAGE` with
      Docker Hub shorthand such as `ubuntu:24.04`.
- [x] Normalize unqualified names to `docker.io/library/...`.
- [x] Support OCI registries, tags, immutable digests, and multi-architecture
      manifest selection.
- [x] Pull directly into TTC's rootless native content store without Docker,
      containerd, a host snapshotter, or mount privileges.
- [x] Make `run` and `create` use Docker-compatible `--pull=missing` by
      default, reusing local tag or unambiguous image-ID records without
      registry access. Support explicit `--pull=always` refresh and
      offline-enforcing `--pull=never`, with requested-platform matching.
- [x] Integrate registry authentication and Docker-compatible credential
      helpers without storing plaintext credentials in the state database.
      Pulls discover `DOCKER_CONFIG` or `~/.docker/config.json`, support inline
      auth/token entries plus per-registry and global credential helpers, and
      resolve secrets only in the in-process registry callback.
- [x] Show line-oriented resolve, per-layer download/cache-reuse, extraction,
      completion, and measured content-store downloaded-versus-reused byte
      totals through both direct and daemon transports. Quiet mode remains
      digest-only.
- [x] `ttc images` with reference, digest, platform, materialized-root state, size, and
      creation time.
- [x] `ttc image inspect`, `ttc image rm`, and content garbage collection.
      `ttc image ls`, top-level `ttc images`, and `ttc rmi` aliases
      accept familiar Docker Hub names and unique image-ID prefixes. Listing
      supports Docker-style tables, `-q`, `--no-trunc`, and JSON while
      exposing the target platform and whether TTC has materialized its own
      immutable root.
- [x] Protect image removal with persistent-container usage checks. A forced
      removal may untag native records while retaining TTC roots required by
      existing containers. Removing the last alias of an unused digest
      synchronously collects unreachable native content and removes all
      platform-specific TTC roots for that digest.

### Container lifecycle

- [x] `ttc run [OPTIONS] IMAGE [COMMAND...]` (initial foreground/detached,
      non-TTY slice with procfs, `/dev` tmpfs, and restricted sysfs mounts).
- [x] Support `--name`, Docker-style `--env`/`-e` values and host imports,
      `--env-file` (including removal of inherited image variables),
      `--label`/`-l`, and `--label-file`, with direct flags overriding file and
      image values. Persist the effective environment and labels through
      create/start/restart/fork and expose them through inspect. Also support
      working directory, named or numeric user, entrypoint override, hostname,
      and rootless execution.
- [x] Support repeated Docker `--group-add`, `--cap-add`, and `--cap-drop` on
      run/create. Resolve group names from the image root, accept Docker capability
      spelling and `ALL` merge semantics, persist canonical HostConfig inspect
      values, and retain Sentry-enforced credentials across restart, exec, and
      live fork.
- [x] Keep single-ID user-namespace rootless execution as the default without
      requiring `newuidmap`, `newgidmap`, or subordinate-ID ranges. Provide an
      explicit `--unsafe-no-host-isolation` fallback for trusted workloads on
      hosts that prohibit user namespaces; this runs Sentry/gofer as the
      caller without host namespaces or chroots while retaining Sentry guest
      namespaces and credentials. Configure this once on `ttc daemon start`;
      the daemon owns one fixed isolation mode and all ordinary commands
      inherit it without sending or negotiating a mode. Keep the per-command
      flag only for explicit `--direct` recovery.
- [x] Support Docker-style `--network/--net none|host|ttc` on run/create,
      persist the selected mode across start/restart, and report it through
      inspect. `none` remains the fork-safe default. Host networking is
      ineligible for live fork because it exposes the external host stack.
- [x] Implement a rootless in-process TTC virtual NIC (`10.0.2.15/24`) and
      branch-private gateway/proxy, optional outbound IPv4 TCP/UDP `--nat`, and
      Docker-style IPv4 TCP `-p`/`--publish`. Persist bindings, reconcile
      listeners after daemon restart, retain the source listener across fork,
      reset inherited child external TCP flows, and leave child branches
      unpublished until explicitly assigned a listener with branch-aware
      `ttc port publish|ls|unpublish` commands.
- [x] Add persistent Docker-style `--ip` and `--mac-address` configuration for
      the TTC virtual NIC. Derive the branch-local `.2` gateway from the
      selected `/24`, validate unicast IPv4 and 48-bit MAC values, and retain
      the guest-visible identity across start, restart, and live fork.
- [x] Enable Sentry guest SUID/SGID exec semantics for TTC by default so common
      unmodified images using `sudo` or `su` work without changing the host
      identity of the Sentry/gofer. Retain OCI `noNewPrivileges` as the guest
      elevation override.
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
- [x] Support persistent isolated writable host views through
      `-v HOST:CONTAINER[:rw]` and `--mount type=bind,...`; use the host path
      only as a live read-only lower and store all guest changes in a private
      branch-local `ttcvol` repository.
- [x] Support detached imports through `-v HOST:CONTAINER:copy` and
      `--mount type=bind,...,copy`; opportunistically reflink during creation
      and always permit streamed-copy fallback for this explicit import mode.
- [x] Support Docker-compatible `--shm-size` byte units with Docker's 64 MiB
      default, persistent `HostConfig.ShmSize` inspect output, and retention
      across create/start/restart/live fork.
- [x] Support Docker-compatible `--tmpfs DEST[:OPTIONS]` and
      `--mount type=tmpfs,target=...` with read/write policy, octal mode, and
      byte-size limits. Support TTC extension mounts for alternate private
      procfs and restricted sysfs views through
      `--mount type=proc|procfs|sysfs,target=...`; persist all three through
      create/start/restart/fork and project them through inspect.
- [x] Support family-local named tmpfs volumes through `-v NAME:CONTAINER[:ro]`
      and `--mount type=volume,...`, including repeated aliases of one volume.
- [x] Support Docker `run/create --pids-limit`, persist it in OCI and
      Docker inspect metadata, and enforce it inside the Sentry for both processes
      and threads without requiring delegated host cgroups. Live forks inherit an
      independent limit and current task charge in every branch.
- [x] Support repeated Docker `run/create --ulimit NAME=SOFT[:HARD]`, including
      Docker resource names, infinity, duplicate last-value-wins behavior, OCI
      projection, and persistent inspect metadata. Sentry enforcement applies to
      the init process and exec sessions, and live forks inherit the limits.
- [x] Support Docker `run/create --memory`/`-m`, `--cpus`, `--cpu-shares`,
      `--cpu-period`, and `--cpu-quota`, including Docker-compatible validation,
      OCI and inspect persistence, and hard host enforcement in a collision-safe
      cgroup v2 child. After `runsc create` and before `start`, TTC proves the
      sandbox PID membership and verifies the actual `memory.max`, `cpu.max`, and
      `cpu.weight`; unavailable delegation or any mismatch fails closed and deletes
      the runtime container. Since all live branches currently inhabit one Sentry
      host process, CPU and memory limits account the whole sandbox family. Future
      Sentry hard accounting is required for independent per-branch limits.
- [x] Support foreground, `--detach`, and attached non-TTY `-i` modes.
- [x] Support `run/create -t` with a detachable Sentry-internal PTY, Docker-style
      combined `-it`/`-dit`, raw interactive attach, resize propagation, and
      branch-local terminal state.
- [x] Support Docker-style `exec -t`/`-it` with a transient Sentry-internal
      devpts PTY, raw host-terminal mode, resize propagation, stdin close, exact
      exit status, and execution in any running live branch.
- [x] Add branch-aware `ttc cp` with Docker directory and trailing `/.`
      semantics, `-a` ownership preservation, `-L` source-link following, and
      `-` tar streaming in both directions. Execute a donated TTC helper inside
      the selected Sentry VFS so images need no `tar` or `cp`, validate archive
      paths and symlink ancestors, and serialize copy-in against live fork.
      Running branches are supported initially; stopped-container VFS access
      remains future work.
- [x] `ttc create` as persistent image-root and OCI-bundle preparation.
- [x] `ttc start CONTAINER` for created/exited base containers, detached by
      default with optional `--attach`. Multiple detached containers start in
      argument order, and stale stopped runsc records are removed before OCI
      recreation while live duplicate IDs remain protected.
- [x] Docker-style `ttc container` aliases for run, create, start, attach,
      logs, exec, inspect, rm, ls/ps, top, pause/unpause, kill, stop, wait, and
      restart.
- [x] `ttc wait [--condition not-running] CONTAINER[@BRANCH]...` starts all
      waits concurrently, prints Docker exit codes in argument order, and
      retains branch-aware wait semantics.
- [x] `ttc restart CONTAINER...` for created, exited, running, or paused base
      containers, including TERM/timeout/KILL behavior. Restart rejects an
      individual branch because destroyed in-memory branches cannot be
      reconstructed. Live descendants require explicit `--force`; restart
      quiesces and snapshots main before teardown, then seeds the replacement
      sandbox from that snapshot.
- [x] `ttc stop CONTAINER[@BRANCH]` with TERM, timeout, then KILL semantics.
- [x] `ttc kill CONTAINER[@BRANCH] [SIGNAL]`.
- [x] Initial `ttc rm CONTAINER`: remove stopped/created family metadata,
      reject a live family unless `--force` is given, and require `branch rm`
      for an individual non-main branch.
- [x] Finalize removal policy: `branch rm` retains immutable lineage tombstones;
      unforced family removal rejects any live family; forced family removal tears
      down main and all descendants and deletes the container-local lineage record.
- [x] Docker-shaped `ttc inspect CONTAINER...` covering identity, image,
      process configuration, lifecycle state, sandbox PID, graph driver, and
      user mounts. Support Docker-style `--format/-f`, line-delimited
      `--format json`, and `--type container`.
- [x] Add a `TTC` inspect extension containing persistent metadata, the live
      family/branch tree, fork timing, and observational process lists for
      every running or paused branch. Ordinary inspect remains observational and
      does not pause branches or perform live-fork validation.
- [x] Initial `ttc logs CONTAINER[@BRANCH]` and `logs -f`, preserving separate
      stdout/stderr streams and backed by branch-local persistent console logs.
- [x] Add Docker-compatible `logs --tail all|N`, including global logical-line
      ordering across the retained and active log generations and `--tail 0 -f`.
- [x] Persist Docker-style `run/create --log-opt max-size=SIZE` and
      `max-file=COUNT` and apply them to every branch sidecar.
- [x] Add Docker-compatible `logs --since`, `--until`, and `--timestamps` over
      versioned timestamped records while continuing to decode legacy records.
      Accept `--details`; it emits no attributes until configurable log attributes exist.
- [x] Add explicit `local`, `json-file`, and `none` logging-driver policy,
      persisted configuration, Docker inspect projection, and `none` read rejection.
- [x] Add Docker-style label and environment selectors with persisted,
      deterministic attributes for `logs --details`.
- [x] Initial `ttc ps [-a|-q]` flat family/branch view and branch-qualified
      `ttc top [--json] CONTAINER[@BRANCH]` process listing.
- [x] Add Docker-style `ps --filter/-f` for name, ID, status, ancestor image,
      managed volume, and branch, with OR within one filter key and AND across
      keys. Add Go-template and line-delimited JSON formatting, `--latest/-l`,
      `--last/-n`, and `--no-trunc` image IDs.
- [x] Carry authoritative branch creation timestamps from the Sentry and merge
      persistent image, digest, volume, and container-creation metadata into
      live family rows. Recent selection includes non-running states, matching
      Docker's latest/last behavior.
- [x] Add durable `ttc events [--json] [-f|--follow]` with Docker-style
      `--since`/`--until`, type/action/subject filters, a versioned JSONL
      schema, daemon-restart persistence, and nonprinting disconnect heartbeats.
- [x] Add Docker `ps --size` writable/virtual size accounting. Running and
      paused writable branches read logical allocated bytes directly from the
      branch-local Sentry OverlayFS tmpfs upper without walking or serializing
      it; read-only roots and created branches report zero writable bytes, and
      exited branches report unavailable writable state because their in-memory
      upper has been released.
- [x] Add Docker-style process-selection formatting for `top`, including the
      common all/full selectors and explicit `-o` column lists backed only by
      authoritative Sentry process fields.

### TimeTravel lifecycle

- [x] `ttc fork SOURCE [DESTINATION]`, for example `ttc fork web@main
      web@test-a`.
- [x] Support `--paused`, defaulting according to a documented policy.
- [x] `ttc pause CONTAINER[@BRANCH]`.
- [x] `ttc resume CONTAINER[@BRANCH]` and Docker-compatible top-level and
      `ttc container unpause` aliases.
- [x] `ttc branch rm CONTAINER@BRANCH` or an unambiguous equivalent.
- [x] Initial `ttc ls --tree` compact Unicode lineage and lifecycle-state
      output, merging persistent base containers with authoritative live runsc
      branch state.
- [x] Extend tree output with PID 1 state, live CoW memory sharing, creation and
      deletion times, and the complete persisted lineage/tombstone model.
- [x] `ttc ls --json` with a stable machine-readable schema.
- [x] Mark deleted historical parents in tree output without reparenting their
      descendants.
- [x] Return structured PID/FD-or-mapping diagnostics from a rejected fork.
- [x] Make `ttc fork` the sole authoritative operation: pause the source,
      validate the frozen network/resource/mapping graph, resume on rejection,
      and never expose a racy advisory preflight command.
- [x] Add Bash, Zsh, and Fish completion for family, branch, image, volume,
      and session names.

### Exec

- [x] Initial `ttc exec [OPTIONS] CONTAINER[@BRANCH] COMMAND...` selecting a
      live branch through `runsc exec --branch`.
- [x] Support environment, cwd, and UID/GID overrides in the initial CLI.
- [x] Add supplementary groups, capabilities, and an OCI process JSON input to
      `ttc exec`; process JSON controls the complete process and terminal mode.
- [x] Support synchronous non-TTY commands with output streaming and
      exit-status forwarding; optionally import stdin with `-i`.
- [x] Support TTY-interactive `-t` commands through the internal console (`-i`
      without a TTY is supported for synchronous exec).
- [x] Support `--detach`, returning a stable branch-qualified execution ID.
- [x] Add a Sentry-owned branch-local exec registry with stable IDs, terminal
      metadata, retained exit status, and low-level list, wait, and signal RPCs.
- [x] Add `ttc exec ls`, `ttc exec wait`, and `ttc exec kill`, plus
      branch-qualified reattach and retained logs by execution ID.
- [x] Preserve virtual PIDs and exec IDs when a running exec session is forked.
- [x] Require a running branch for exec; do not implicitly resume paused
      branches.

### Attach and detach

- [x] Initial non-TTY `ttc attach CONTAINER[@BRANCH]` for the init console,
      with stdout/stderr streaming, optional `-i`, and exit-status forwarding.
- [x] `ttc attach --exec EXEC_ID CONTAINER[@BRANCH]` and
      `ttc logs --exec EXEC_ID CONTAINER[@BRANCH]` for managed exec sessions.
- [x] Support output-only and combined attachment by default/with `-i`, plus
      stdin-only, stdout-only, and stderr-only selection through `--no-stdin`,
      `--no-stdout`, and `--no-stderr`. Unselected output is still drained so
      console retention and wait behavior remain correct.
- [x] Support Docker-like detach keys for non-TTY stdin attachment, including
      Ctrl-P, Ctrl-Q by default and `ttc attach --detach-keys` overrides.
- [x] Support TTY detach keys, terminal resize/SIGWINCH propagation, and
      line-discipline signal delivery from an attached raw terminal. Daemon API
      v2 negotiates raw mode and window sizes with the client, queues early
      stdin without blocking control frames, and sets init/exec PTY dimensions
      before the guest process becomes runnable.
- [x] Initial noninteractive streaming suitable for pipelines.
- [x] Allow multiple independent stdout/stderr readers without consuming each
      other's output; one locked sidecar is the sole Sentry-pipe reader.
- [x] Initial single-stdin-owner enforcement with one advisory host lock per
      branch; detach, exit, or client death releases it without adding a host FD
      to forked guest state.
- [x] Move interactive attach stdin leases into `ttcd`, keyed by resolved
      container ID, branch, and optional exec session. A second owner is rejected;
      `attach -i --takeover` disconnects and waits for the prior client before
      proceeding. Keep the advisory file lock as a direct/recovery backstop.
- [x] Drain output while no client is attached so guest writers do not stop at
      the Sentry pipe capacity; retain output in a persistent host log.
- [x] Initial bounded retention: rotate only between complete multiplexed
      frames, keeping a 16 MiB current generation and one retained generation;
      readers follow inode replacement without duplicating frames.
- [x] Make retention size and generation count configurable, persist the
      settings per container, and report when a reader falls behind retention.
- [x] Define explicit output backpressure policy: the sole sidecar never drops
      live frames, so a stalled host log eventually backpressures guest writers;
      only complete historical generations are evicted by bounded retention.
      Persist cumulative encoded bytes and generation counts for init and managed
      exec consoles, expose them in `inspect` per branch/exec, and attach the
      relevant snapshot to daemon lifecycle, logs, inspect, attach, and exec events.
- [x] Bound daemon-side queued stdin to a 1 MiB credit window. API v2
      acknowledges bytes only after the guest pipe accepts them, so resize and
      other control frames remain responsive while a slow guest backpressures
      the client without unbounded manager memory.
- [x] Allow fork while output readers are attached: read RPCs serialize against
      fork, readers follow a host log, and a new child sidecar starts afterward.
- [x] Keep branches fork-eligible before, during, and after output attachment;
      host readers never enter the guest resource graph.
- [x] On fork, clone console queues and start a new branch-local drain while
      inheriting no host attachment clients.

## Branch-aware process control

The standard OCI `runsc exec` path retains `Loader.processes` only for
branch-unaware synchronous exec/wait and donated host-TTY compatibility. TTC
stable exec sessions select a branch Kernel and keep lifecycle, wait status, and
internal TTY/console ownership in that `sandboxBranch`, including for `main`.

- [x] Add branch ID to execute and wait-PID paths.
- [x] Add an optional branch ID to the general process-signal RPC while
      preserving empty-selector legacy behavior. Process, process-group, all-process,
      and cloned-internal-TTY foreground-group delivery are branch-aware; internal
      terminal resize/control RPCs also carry branch IDs.
- [x] Keep only legacy OCI init and synchronous exec bookkeeping in
      `Loader.processes`. Stable TTC exec sessions are owned exclusively by each
      `sandboxBranch`, including `main`, while Docker/OCI exec and wait remain
      compatible.
- [x] Reconstruct TTC-managed exec records after fork by resolving cloned
      virtual TIDs and container/execution IDs in the child Kernel; process groups
      and controlling terminals are cloned as Kernel state.
- [x] Select mount and PID namespaces from the target branch's container init.
- [x] Use the target branch Kernel in `control.Proc` and `control.ExecAsync`.
- [x] Share immutable OCI specs and rebuild seccomp programs from them while
      keeping TTC-managed exec lifecycle and process/TTY state branch-local.
- [x] Extend branch Kernel-lifetime leases to exec creation and wait, rejecting
      exec creation unless the selected branch is running.
- [x] Add staged failure injection after namespace selection, session reservation,
      internal-console creation, process creation, and session publication; rollback
      kills an unpublished process and removes provisional session/console state.
- [x] Add deterministic post-donated-FD-import failure injection and prove the
      provisional Sentry FD table is released before a successful live fork.
- [x] Donate a client-liveness pipe to managed exec waits; client loss cancels
      the Sentry wait and releases its branch lease without killing the exec
      process. Deterministic controller-level injection verifies cleanup.
- [x] Deterministically test the full exec operation lease against same-branch
      pause, resume, fork, and destroy, while proving sibling pause/resume remains
      independent.

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
- [x] Allocate and expose detached exec-session IDs in `ttc`.
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
- [x] Add bounded, cancellable pipe/PTY readiness waits for stdin, stdout, and
      stderr. Live fork cancels and joins active host waiters before serialization;
      resize is implemented for init and attached exec PTYs.
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
- [x] Make `ttcd` own, track, and reap console sidecars that it launches.
      Followers intentionally survive daemon exit under their advisory locks; a
      restarted daemon reconciles them, and console I/O uses readiness-driven
      transport instead of polling RPCs.
- [x] Clone unread non-TTY console data and pipe connectivity during live fork,
      including recursively forked branches.
- [x] Clone init- and exec-console PTY termios, foreground process groups,
      window sizes, queues, hangup state, and controlling-terminal identity.
- [x] Ensure post-fork non-TTY input and output cannot cross branches and that a
      descendant console survives deletion of its source branch.
- [x] Keep output draining after an ordinary attach client exits.
- [x] Test detach-key parsing and detection across stdin read boundaries.
- [x] Make an orderly `ttc daemon stop` resume paused branches and stop
      descendants before ancestors with normal stop policy. Writable rootfs and
      managed-volume state is already persistent, so daemon shutdown performs
      no serialization. Leave the daemon running if container shutdown fails,
      and preserve live sandboxes after an unexpected daemon crash for restart
      reconciliation, including console drains and port sidecars.
- [x] Propagate the invoking client's `TERM` for terminal runs and execs when no
      image/container or command value exists, with an `xterm` fallback.
- [x] Test sidecar SIGKILL and lock-based replacement with a newly published
      PID while the guest remains live and output draining resumes.
- [x] Test that client loss delivers already-acknowledged stdin before EOF,
      releases the receiver, and that a control-socket failure returns promptly.
- [x] Add `/dev/pts` and `/dev/ptmx` mount support required by interactive
      sessions.

## Mount validation framework

Live-fork validation walks both live mounts and otherwise referenced VFS
filesystems, including image-root gofer lowers hidden beneath OverlayFS. Each
filesystem implementation explicitly returns one of:

```text
Clone             mutable Sentry-owned state, copied using page CoW
ShareExternal     explicitly read-only external lower/resource with stable identity
Reject            unsupported, externally mutable, or host/device-backed
```

Unclassified filesystems reject by default. Gofer instances must carry a
stable resource ID, prove that their server connection is read-only, disable
directfs, and be mounted read-only when visible in the mount table. Restricted
sysfs also requires a read-only mount. `LiveFork` runs this closure validation
while the source is paused and before
shared-memory or address-space CoW preparation. Rejections carry structured
PID/FD resource issues, including mount path, filesystem type, source class,
and reason.

- [x] Add a VFS live-fork mount/filesystem eligibility interface returning the
      decision, stable identity, and rejection reason.
- [x] Collect all mount decisions before any parent CoW mutation.
- [x] Include mount path, filesystem type, source class, and reason in
      structured eligibility errors.
- [x] Replace the blanket `len(spec.Mounts) == 0` gate with an initial
      conservative allowlist that rejects every unclassified mount.
- [x] Replace the spec-level allowlist with closure validation over every
      reachable mounted filesystem and resource.
- [x] Verify recursive forks and arbitrary destruction orders for every allowed
      mount class.

### tmpfs, procfs, and sysfs

- [x] Admit main-`MemoryFile` tmpfs mounts, including `/dev/shm`, as the initial
      `Clone` class and verify file contents diverge CoW across branches.
- [x] Admit a private devpts mount, verify `/dev/ptmx` follows the standard
      symlink into it, and verify an open internal PTY is cloned rather than
      shared across branches.
- [x] Generate arbitrary user-requested tmpfs, procfs, and restricted sysfs
      mounts from Docker-style CLI flags; verify mode/size, inspect projection,
      restart persistence, fork validation, and parent/child tmpfs CoW with an
      unmodified Alpine image.
- [x] Test tmpfs contents, mappings, metadata, hardlinks, open-unlinked files,
      truncate, fallocate, and mount flags across branches. Standard OCI mount
      flags now use last-option-wins parsing; `nodev` is enforced and retained
      through recursive live forks.
- [x] Admit procfs after proving `/proc/self`, `/proc/PID`, `/proc/*/fd`,
      PID namespace inodes, mount topology, uptime, and `/proc/net` resolve
      through the selected branch Kernel. Network proof covers loopback device
      counters, SNMP counters, routes, and complete TCP, UDP, and Unix socket
      rows; live-fork loopback reconstruction preserves NIC counter values and
      TCP restore no longer double-counts established connections.
- [x] Verify parent/child `/proc/1/status` and `/proc/self/status` identity plus
      branch-divergent reads through an already-open `/proc/self/fd` handle.
- [x] Admit a read-only restricted Sentry-owned sysfs profile.
- [x] Keep host binds and TPU/NVIDIA device proxies rejected by the initial
      allowlist; other mirrored host sysfs paths remain rejected until
      separately proven branch-local or immutable.
- [x] Test nested and repeated procfs/sysfs mounts and bind-remount flags.
      The unmodified-image matrix now verifies alternate and nested mountpoints,
      read-only volume aliases, host-bind remounts, and recursive-fork retention;
      proc mount rendering also preserves `nodev` and the leading `rw`/`ro` option.

### Host paths

- [x] Admit a LISAFS/gofer bind only when the OCI mount and server connection
      are both read-only and directfs is disabled.
- [x] Borrow one live gofer client/root control FD per filesystem UniqueID.
- [x] Initial integration coverage reads the bind in parent, child, and
      grandchild branches, rejects writes, recursively forks it, and keeps the
      grandchild functional after deleting its source branch.
- [x] Test multiple simultaneous read-only binds, recursive fork, host mutation
      visibility, and source-branch deletion while a descendant remains live.
- [x] Test inherited open files and branch-local offsets, directory cursors, and
      read/execute mappings across recursive forks, including host mutation and
      deletion of the source branch while its descendant remains functional.
- [x] Test exhaustive descendant-first and ancestor-first destruction orders.
- [x] Document and expose read-only host binds as shared external views, not
      snapshots: TTC inspect metadata and fork-check warnings state that
      host-side mutations may become visible to every branch.
- [x] Add persistent container-private `ttcvol` uppers for writable host-mount
      syntax. Keep the host gofer read-only, preserve changes across
      stop/start, and fork the private repository with the branch.
- [x] Add `:copy` imports for directories and regular files, including
      opportunistic reflinks, unconditional streamed-copy fallback, hard links,
      symlinks, ownership, modes, and xattrs.
- [x] Include private host repositories in checkpoint/restore and support
      running, paused, and stopped `ttc cp` through mounted subtrees.

### Managed volumes

- [x] Replace the pre-release tmpfs/tar prototype with a direct-to-disk Sentry
      filesystem and remove its import/export/commit lifecycle.
- [x] Probe `FICLONE` at daemon startup and require explicit
      `--allow-slow-volume-copy` opt-in for whole-file first-write copying.
- [x] Implement persistent immutable namespace/inode trees, opaque data
      objects, stable virtual inodes, hard links, symlinks, xattrs, truncate,
      fallocate, and fsync.
- [x] Fork mounted volumes transactionally in O(number of volumes), retaining
      parent/child/grandchild isolation for namespace changes, regular writes,
      open file descriptions, and mappings.
- [x] Add `ttc volume create|ls|inspect|rm`, named and anonymous `-v`,
      `--mount type=volume`, image `VOLUME`, Docker copy-on-create,
      `volume-nocopy`, inspect projection, and `rm -v`.
- [x] Reject volume removal while any persisted container still attaches it.
- [x] Enforce a single writable sandbox-family attachment until coherent
      cross-Sentry invalidation is implemented; permit additional read-only
      attachments.
- [ ] Add sparse-aware slow copying, quotas/accounting, branch/object garbage
      collection, import/export, and explicit branch-head promotion.
- [ ] Add device/FIFO/socket nodes, ACLs, exhaustive crash fault injection,
      and reflink end-to-end qualification on supported host filesystems.

## Image and storage backend

- [x] Remove the containerd daemon and client dependency. Use the OCI registry,
      manifest, archive, and local content-store libraries in process while TTC
      owns its catalog, locking, lifecycle, and storage paths.
      Existing materialized families remain usable across this upgrade, while
      image names previously owned by containerd must be pulled once into the
      native store before they can seed new families.
- [x] Store digest-verified blobs in a private rootless CAS under `--data-root`,
      with atomic, versioned image metadata and target-guarded tag deletion.
- [x] Add deterministic native content pins for every prepared family. The
      image target is pinned transactionally after materialization, the pin ID is
      persisted for recovery and inspection, failed prepares roll it back, and family
      removal releases it synchronously only after runtime teardown.
- [x] Mark descriptors reachable from image records and family pins, and
      synchronously collect unreachable blobs after retag, image deletion, and
      family-pin release.
- [x] Use no host snapshotter for execution: TTC materializes a content-addressed,
      caller-owned immutable directory lower that is exposed through a read-only gofer.
      This avoids requiring a rootful or FUSE snapshotter in the live-fork path.
- [x] Cache content-addressed directory roots and construct the initial OCI
      spec from image config plus `ttc run` options.
- [x] Preserve distinct image UID/GID ownership in rootless materialization with
      a versioned sidecar manifest served only by the immutable root gofer
      connection. Physical files remain caller-owned; guest stat and Sentry
      permission checks receive the OCI-layer IDs without subordinate ranges.
      Materialized roots default to guest mode 0755 so non-root image users can
      traverse `/` regardless of private host cache-parent modes.
- [x] Construct the full mount manifest from the Sentry VFS closure and expose it
      in fork-check JSON and inspect fork-check results. Records include visible
      mounts and hidden referenced lowers with stable identity and
      clone/share-external/reject decisions.
- [x] Place a private disk-backed `ttcvol` OverlayFS upper over the immutable
      image lower by default. Apply mutations directly to the repository so
      stop/start has no rootfs save/restore step.
- [x] Track an image-target content pin for every family and release it only
      during family removal, after all live branches have been destroyed. TTC
      creates no host snapshot resources in the current directory-lower design.
- [x] Keep root and managed-volume namespace semantics in the Sentry while
      storing their immutable metadata trees and opaque regular-file objects in
      `ttcvol` repositories. Live fork publishes new branch roots in O(1), and
      first-write reflinks provide file then extent CoW without a host
      snapshotter.
- [x] Add a manual networked end-to-end matrix for unmodified BusyBox, Alpine,
      Python, nginx, and Redis images, covering rootless prerequisites, OCI startup,
      Docker-like lifecycle commands and inspect across created, running,
      forked, paused, and exited states; live fork; managed-volume CoW;
      read-only host binds; distinct Redis lower-layer ownership; exact quick-exit
      console integrity; configurable shared memory; supplementary groups and
      capabilities; Sentry-enforced PID limits; Docker ulimits across live fork;
      and the explicit no-userns fallback.
- [x] Add image pull and materialization tests using a hermetic local Registry V2
      server and repository-built static OCI guest, with full-content cache reuse
      and cached startup after the registry is stopped.

## `ttcd` and persistence

- [x] Initial direct-CLI persistent base-container metadata with created,
      running, and exited reconciliation against `runsc list`.
- [x] Initial per-branch console sidecars and logs survive the CLI process that
      launched them; advisory locks prevent duplicate pipe consumers and log
      readers diagnose premature sidecar exit.
- [x] Implement a local Unix-domain control service with streamed stdin,
      stdout, and stderr so commands and state ownership survive individual CLI
      processes. Background pulls and durable event streams run in daemon
      request workers.
- [x] Provide an explicitly started rootless per-user socket and private runtime
      directory, with peer-UID checks and a `0600` socket. Keep isolation mode
      exclusively in daemon startup configuration; clients never send it.
- [x] Persist family configuration, image references, rootfs/managed-volume
      repository identities, lineage, and tombstones; live process memory
      remains exclusively in the Sentry.
- [x] Reconcile persisted metadata with the native image catalog and live runsc
      sandboxes after daemon restart, and restart missing branch/exec console
      drainers. The current direct-runsc backend has no shim process.
- [x] Provide durable manager events for branch/container creation, pause,
      resume, exit, destroy, exec, attach, image operations, and fork outcomes.
      `ttc events` supports text/JSON output, time bounds,
      filters, and follow streaming across daemon restarts.
- [x] Serialize conflicting structural operations per data root and make
      daemon request delivery idempotent. Completed requests replay their exact
      status and output; interrupted pending requests fail closed with an
      indeterminate-outcome diagnostic instead of being blindly repeated.
- [x] Bound the completed-request journal to the newest 2,048 results per
      data root with durable compaction; never remove pending intents
      automatically.
- [x] Add daemon-mediated request listing and inspection plus explicit, audited
      `succeeded`, `failed`, or `abandoned` resolution for interrupted pending
      intents; resolution never re-executes the original mutation.
- [x] Add a versioned framed local API with explicit version rejection so the
      CLI and daemon may be upgraded independently.
- [x] Keep direct runsc invocation behind the daemon API and an explicit
      `--direct` recovery path while preserving the branch-control API boundary.

## Docker and containerd compatibility

- [x] Keep `runsc install`/Docker operation working for the `main` branch. The
      isolated Docker compatibility matrix covers create, start, inspect, logs,
      exec, pause, unpause, stop, and remove with the modified runtime.
- [x] Document that Docker cannot represent or control the TimeTravel branch
      tree and that `ttc` remains the source of truth for forks.
- [x] Preserve the standard OCI runtime command surface for external managers;
      TimeTravel control remains an additive `runsc timetravel` subcommand.
- [x] Prototype a containerd shim side-channel API for branch lifecycle without
      changing the standard task-v2 API. The versioned TTRPC service
      `gvisor.timetravel.v1.Branches` is registered on the existing shim socket
      and exposes `List`, `Fork`, `Pause`, `Resume`, and `Destroy`; its
      public Go client delegates to `runsc timetravel` while task-v2 is unchanged.
- [x] Keep the image target and its referenced content natively pinned while any
      branch in the family may reference the materialized immutable lower. No
      host snapshotter is used by the current execution backend.
- [x] Consider a future nerdctl/Docker-compatible flag layer only after the
      branch-native `ttc` model is stable. Decision: keep `ttc` as the
      Docker-compatible command surface rather than wrapping nerdctl; containerd
      adapters may use the additive shim service without owning the branch tree.

## Delivery sequence

### Milestone A: normal OCI bundles

- [x] Per-mount eligibility framework.
- [x] Initial tmpfs, procfs, and restricted read-only sysfs mounts.
- [x] devpts and `/dev/ptmx` mounts for the internal TTY console.
- [x] Initial immutable read-only gofer bind.
- [x] Multiple immutable read-only gofer binds and parent/child access coverage.
- [x] Managed tmpfs submount volumes.
- [x] Managed OverlayFS submount volumes.

Gate: an ordinary generated OCI bundle with `/proc`, `/sys`, `/dev`, `/dev/pts`,
read-only binds, and managed volumes forks without hand-editing its spec.

### Milestone B: branch-native exec

- [x] Branch-local process bookkeeping and branch-aware execute/wait/signal.
- [x] Synchronous non-TTY `ttc exec` with branch-aware execute/wait.
- [x] Eligibility recovery after an attached exec process exits and its imported
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
- [x] Daemon-owned framed stdin/stdout/stderr and terminal-control transport,
      including restart reconciliation.
- [x] Define daemon-owned stdin lease takeover and stale-client behavior:
      client EOF releases ownership; a competing owner is rejected; explicit
      `attach -i --takeover` cancels and joins the old request before granting the
      keyed container/branch/exec lease.
- [x] Detached exec creation with fork-aware per-session output drains.
- [x] Reattach, retained logs, and exec-session detach keys.
- [x] Internal devpts interactive consoles for init and attached exec, including
      host-size propagation and branch-local PTY signal semantics.
- [x] Non-TTY console state fork and branch-local I/O.
- [x] Init and attached exec-session PTY state fork and branch-local I/O.

Gate: a user can detach from a still-running interactive or noninteractive
process, fork its branch, and then attach independently to both branches.

### Milestone D: Docker-like `ttc`

- [x] Native daemon-free OCI image pull, verified CAS, family pins, GC, and
      managed image-root storage.
- [x] Full run/create/start/stop/rm/inspect/logs/exec/attach command
      surface, exercised through Docker-style aliases and the five-image matrix.
- [x] Initial fork/pause/resume and beautified branch tree.
- [x] Atomic pause-validate-fork transaction with automatic source resume on
      rejection.
- [x] Rich branch-tree diagnostics with PID 1 state, CoW state, immutable lineage
      and deletion tombstones, creation/deletion times, fork timings, and structured
      rejected-fork resource diagnostics.
- [x] Rootless daemon lifecycle, recovery, events, documentation, and shell
      completion.

Gate: a fresh user can pull a Docker Hub image and exercise the complete
TimeTravel lifecycle without invoking runsc, ctr, runc, or editing an OCI
bundle.

## Open semantic decisions

- [x] Define `start` for an exited main branch as OCI recreation from the
      immutable image, persistent configuration, and the existing main
      rootfs/managed-volume repository branches. Writes, including shutdown
      handler writes, update those repositories directly; stop/restart takes no
      TTC snapshot and performs no export/import.
- [x] Output attachment does not reject or migrate with fork: every branch owns
      one drain, while host readers remain attached only to the selected log.
- [x] Drain to a persistent host log instead of blocking guest output merely
      because no console client is attached; the initial policy keeps a 16 MiB
      current generation plus one retained generation.
- [x] Expose log size and generation-count policy in the user-facing CLI.
- [x] Support multiple simultaneous output readers.
- [x] Use one exclusive manager-level stdin owner per branch.
- [x] Daemon-reconciled stdin leases are process-local ownership records: daemon
      death closes clients, restart begins with no stale owner, and `--takeover`
      provides explicit replacement while a daemon remains alive.
- [x] Host-mutation semantics and warnings for shared read-only binds.
- [x] Do not share managed-volume contents across unrelated families. A name is a
      configuration identity; each family instantiates its own forkable tmpfs.
- [x] Do not retain stopped branch filesystem state without its Kernel and memory;
      branch-local writable state exists only for the live sandbox family.
- [x] Use the per-user `ttcd` API as the default deployment, keep direct runsc as
      an explicit recovery path, and defer a task-v2 shim extension until branch
      lifecycle integration is required by external managers.
