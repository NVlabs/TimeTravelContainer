# TTC end-to-end image matrix

`e2e_image_matrix.sh` is the authoritative manual smoke test for running
unmodified public Docker images through the complete TTC path. It currently
covers:

- explicit rootless `ttcd` startup over a versioned per-user Unix API,
  full command streaming through the daemon, status/stop controls, and live
  sandbox plus console-drainer reconciliation after a daemon restart;
- durable versioned manager events across daemon restart, including text/JSON
  inventory, type/action filters, time bounds, and live follow of new branch
  lifecycle activity;
- Docker-compatible run/create pull policies: explicit refresh with
  `--pull=always`, cached startup with `--pull=never`, missing-image rejection,
  and default local reuse after the initial matrix pull;
- Docker-style image inventory, quiet IDs, JSON output, and materialized-root
  inspection;
- Docker-compatible 64-hex container IDs from `create` and detached `run`,
  distinct persisted names, 12-character `ps` IDs, and lifecycle lookup by
  exact name, full ID, and unambiguous ID prefix;
- Docker-compatible foreground and detached `run --rm`, plus persisted
  `create --rm` cleanup after a later detached start;
- Docker-shaped container inspect JSON and Go templates across created,
  running, forked, paused, and exited states, including effective image
  configuration, compacted branch lineage and reusable branch names,
  env-file/host-import environment overrides, merged labels,
  user mounts, sandbox PID, per-branch processes and timings, and transactional
  fork-rejection diagnostics;
- Docker-style `container` aliases, multi-container detached `start`, concurrent
  `wait` with argument-ordered exit codes, restart of an exited container, and
  an explicitly forced running-family restart; writable-rootfs state, including
  shutdown-handler writes, is updated directly in the family-owned `ttcvol`
  repository and reopened without an exit export/start restore;
- persisted `--network host` startup using an unmodified BusyBox HTTP server,
  Docker inspect projection, host-loopback reachability, and live-fork
  rejection for the externally shared network stack;
- rootless `--network ttc` startup with the `ttc0` virtual NIC, NAT-disabled
  isolation, optional IPv4 TCP/UDP user-mode NAT, Docker-style persistent TCP
  publication, branch-aware live publication and removal, daemon-restart
  listener reconciliation, listener cleanup on branch deletion, parent listener
  continuity across live fork, and child external TCP reset/fresh-proxy semantics;
- Docker `--add-host`, `--dns`, `--dns-search`, and `--dns-option`/`--dns-opt`
  persistence through immutable `/etc/hosts`, `/etc/resolv.conf`, and
  `/etc/hostname` files, including Docker inspect projection, read-only-rootfs
  startup, restart retention, live-fork inheritance, and hiding of internal
  implementation binds from user mount inspection; literal IPv4 and IPv6
  addresses are supported, while `host-gateway` is rejected until TTC gains a
  bridge gateway;
- BusyBox process/procfs/sysfs startup;
- Docker `--group-add` plus `--cap-add`/`--cap-drop` normalization, inspect
  persistence, init and exec enforcement, restart retention, and live-fork
  inheritance;
- Docker `--shm-size` parsing, OCI tmpfs sizing, inspect persistence, restart
  retention, and identical capacity in both sides of a live fork;
- Docker `--read-only` rootfs persistence through restart and live fork,
  including write rejection in both branches while custom tmpfs remains
  writable;
- image `STOPSIGNAL` inheritance plus Docker `--stop-signal` and
  `--stop-timeout` inspect persistence, graceful-stop delivery, and final exit-code reconciliation;
- Docker `--pids-limit` OCI and inspect persistence, Sentry-side process and
  thread enforcement without delegated host cgroups, and branch-local limit and
  task-charge inheritance across live fork;
- Docker `--memory`/`-m`, `--cpus`, `--cpu-shares`, `--cpu-period`, and
  `--cpu-quota` parsing, OCI projection, persistence, inspect projection, and
  synthetic cgroup-v2 verification are covered by the focused Go suites. A live
  enforcement matrix requires a delegated cgroup-v2 subtree and is intentionally
  not requested by this portable image matrix; TTC fails closed before start when
  delegation is unavailable;
- repeated Docker `--ulimit NAME=SOFT[:HARD]`, including inspect persistence,
  enforcement in the image process, and inheritance by exec sessions in both
  sides of a live fork;
- Docker-style custom tmpfs mode/size plus alternate private procfs and
  restricted sysfs mounts, including inspect projection and restart retention;
- disk-backed named and anonymous volumes, image `VOLUME` initialization,
  direct persistence across stop/start and daemon restart, hard links,
  symlinks, fallocate, open-FD writes, parent/child/grandchild CoW, overlapping
  first writers in two and four branches, and same-path namespace divergence;
- branch-aware `ttc cp` in both directions without guest utilities, including
  Docker directory and trailing `/.` semantics, binary tar streaming through
  the daemon, hard links, symlinks, modes, parent/child divergence, and
  management-only copying while ordinary tasks in the target branch remain
  paused;
- Alpine dynamic linking and package-tool startup;
- Python threading, JSON, and in-memory SQLite;
- nginx configuration loading;
- Redis startup with its image-declared `/data` automatically backed by an
  anonymous persistent TTC volume;
- immediate-exit console integrity with 256 KiB on each of stdout and stderr,
  verified in both foreground output and persistent logs;
- detached output and bounded multi-generation logs;
- persisted `local`, `json-file`, and `none` log-driver policy, Docker inspect
  projection, rejection of log reads for `none`, and label/environment attributes
  in `logs --details`;
- Docker-compatible `logs --since`, `--until`, `--timestamps`, and `--details`
  over versioned timestamped records, including filtering and legacy-record decoding;
- `exec`, selective stdin/stdout/stderr `attach`, transactional `fork`
  behavior, live `fork`, `pause`,
  `resume`/`unpause`, `ps`, `top`, and the lineage tree, including
  branch/status/ancestor `ps` filters, templates, latest selection, JSON
  creation timestamps, and branch-local writable/virtual `ps --size` accounting;
- live fork while a daemon-mediated attached exec is active, and detached
  exec-session ID/PID cloning
  through parent, child, and grandchild with branch-local signal/wait status;
- user tmpfs copy-on-write isolation between a parent and child branch;
- tmpfs live-fork `pwrite()` from a mapping in the same memory manager, covering
  the source-read-lock versus CoW-invalidation-write-lock ordering that loaded
  Chromium exercises;
- exact validation of systrap's `mov $sysno,%eax; syscall` fast-path pattern,
  preventing false-positive rewriting of unrelated Chromium/V8 JIT code;
- simultaneous read-only host binds, write rejection, shared-view fork
  warnings, and host-side mutation visibility in parent and child;
- persistent isolated writable host views, protection of the host lower,
  visibility of uncopied live-lower changes, stop/start persistence, exited
  `ttc cp`, checkpoint/restore, inspect output, and parent/child CoW divergence;
- detached `:copy` imports for directories and individual files, source
  deletion independence, fork divergence, inspect output, and
  checkpoint/restore;
- graceful stop, branch removal, and container removal.

The dedicated disk-volume suite additionally covers the ordinary rootfs upper:
direct disk objects while running, absence of tar snapshots, O(1) branch
creation and divergence, exited main/child `ttc cp`, copy into a
created-but-never-started container, and stop/start reopening without explicit
save or restore.

Focused Go tests additionally cover custom TTC NIC IPv4/MAC validation and
runtime propagation, line-oriented image pull/extraction progress, default
guest SUID enablement in TTC's runsc arguments, daemon namespace preflight,
private-anonymous-memory write-fault regression cases, branch-local pidfd target
rebinding, and cloning of Sentry-owned netlink sockets.

The test requires network access for image pulls. TTC resolves OCI registries
directly and stores verified manifests, configs, and layers in its native
rootless content store under the explicitly configured `TTC_ROOT`; no Docker or containerd daemon is
required. By default, the runtime user must be able to create a user namespace
with a one-entry UID/GID map:

```shell
unshare -Ur /bin/true
```

If this probe fails, fix the host's unprivileged-user-namespace or LSM policy
first. A single-ID mapping does not require `newuidmap`, `newgidmap`, or
subordinate-ID ranges. Some Linux distributions apply AppArmor restrictions
independently of `kernel.unprivileged_userns_clone`.
Rootless `ttc daemon start` and direct `ttcd` startup run the equivalent
probe and fail immediately with an isolation-mode diagnostic when it is denied.

For trusted test workloads on hosts that prohibit user namespaces, set
`TTC_UNSAFE_NO_HOST_ISOLATION=1`. The harness then starts its `ttcd` once
with `--unsafe-no-host-isolation`; all subsequent commands inherit that fixed
daemon mode. This runs the Sentry and gofer as the invoking user without host
namespaces or chroots. Guest namespaces,
credentials, procfs, mounts, and filesystem CoW remain implemented by the
Sentry, but this mode removes host-side defense in depth and must not be used
for untrusted containers.

Build and invoke the matrix with:

```shell
bazel build //runsc:runsc //runsc/ttc:ttc //runsc/ttcd:ttcd \
  //runsc/ttc/test:e2e_image_matrix

TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/bazel-bin/runsc/ttcd/ttcd_/ttcd" \
  "$PWD/runsc/ttc/test/e2e_image_matrix.sh"
```

No-userns example:

```shell
TTC_UNSAFE_NO_HOST_ISOLATION=1 \
TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/bazel-bin/runsc/ttcd/ttcd_/ttcd" \
  "$PWD/runsc/ttc/test/e2e_image_matrix.sh"
```
Use `TTC_E2E_ROOT` to retain the generated TTC state for inspection. Image
references can be overridden with `BUSYBOX_IMAGE`, `ALPINE_IMAGE`,
`PYTHON_IMAGE`, `NGINX_IMAGE`, and `REDIS_IMAGE`. By default, temporary TTC
state, including the native image content, is removed on success or failure.

A passing result is stronger than the unit suite: it proves image pulling,
layer extraction, OCI generation, rootless runsc startup, versioned daemon
transport and restart recovery, nontrivial userspace, console sidecars, and
TimeTravel branch operations on the executing host.

## Branch-aware copy integration

`e2e_cp.sh` is the focused host-to-container, container-to-host, and tar-stream
test for `ttc cp`. It copies into main, forks, writes distinct files into both
branches, exports the child, and imports the child's binary tar stream into the
parent. It also checks directory `/.` behavior, symlinks, hard links, and modes.

```shell
bazel build //runsc:runsc //runsc/ttc:ttc //runsc/ttcd:ttcd \
  //runsc/ttc/test:e2e_cp

TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/bazel-bin/runsc/ttcd/ttcd_/ttcd" \
  "$PWD/runsc/ttc/test/e2e_cp.sh"
```

Use `TTC_UNSAFE_NO_HOST_ISOLATION=1` only on a trusted host that cannot create
unprivileged user namespaces. Set `TTC_ALLOW_SLOW_VOLUME_COPY=1` when the
test's data root is on a filesystem without `FICLONE`.

## Terminal and live-fork integration

`e2e_terminal.sh` is the authoritative real-PTY test for `run -it`, detached
terminal containers, the `ttcd` terminal transport, attach/detach keys,
branch-local terminal state after live fork, pause/resume, persistent logs,
client `TERM` propagation, terminal ioctls, and `exec -it`. It repeats
quick-exit terminal runs to cover the pre-start console
follower handshake, verifies that their output is never lost, and requires
`exec -it` to close promptly after the guest process exits. The 60-second
`timeout` is a test-wide deadlock guard, not an accepted teardown delay.

The `runsc/boot:boot_test` live-fork coverage also installs the same explicitly
transient channel waiter used by active TTC port forwarding. It verifies that
the source registration survives snapshotting, the child omits that host-runtime
waiter, and ordinary Sentry listeners are not classified by implementation
type.

```shell
bazel build //runsc:runsc //runsc/ttc:ttc //runsc/ttcd:ttcd \
  //runsc/ttc/test:e2e_terminal

TTC_UNSAFE_NO_HOST_ISOLATION=1 \
TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/bazel-bin/runsc/ttcd/ttcd_/ttcd" \
  "$PWD/runsc/ttc/test/e2e_terminal.sh"
```

Omit `TTC_UNSAFE_NO_HOST_ISOLATION=1` on a host where `unshare -Ur /bin/true`
succeeds. The test also requires util-linux `script` and `timeout`. Set
`TTC_E2E_ROOT` to retain terminal transcripts, console logs, and runtime state.

## Docker main-container compatibility

`e2e_docker_main.sh` verifies that the TimeTravel-enabled `runsc` retains its
standard OCI runtime surface. It starts an isolated privileged Docker-in-Docker
daemon, uses `runsc install` to register `runsc-ttc`, and exercises Docker
create, start, inspect, logs, exec, pause, unpause, stop, and remove against an
unmodified image. It never changes the host daemon configuration.

```shell
bazel build //runsc:runsc //runsc/ttc/test:e2e_docker_main

RUNSC="$PWD/bazel-bin/runsc/ttcd/ttcd_/ttcd" \
  "$PWD/runsc/ttc/test/e2e_docker_main.sh"
```

The test requires Docker access, network access for image pulls, and permission
to launch a privileged container. Override `DIND_IMAGE`, `TEST_IMAGE`, or
`TEST_NAME` when needed.

Docker sees one ordinary OCI container and can manage only its `main` process
tree. It cannot represent TTC branch IDs, lineage, branch-local filesystems,
fork validation outcomes, or simultaneous branch lifecycle states. Do not use Docker
and `ttc` concurrently to mutate the same sandbox. Use Docker compatibility
for conventional main-only containers; use `ttc` as the sole source of truth
for every container family that is live-forked.

## Hermetic local-registry integration

`e2e_local_registry.sh` builds an OCI image around a static repository guest,
serves it from a tiny localhost Registry V2 fixture, and verifies first-pull
download accounting, second-pull content reuse, and `--pull=never` startup after
the registry is stopped. It makes no public network request.

```shell
bazel build //runsc:runsc //runsc/ttc:ttc \
  //runsc/ttc/test/registry_fixture:registry_fixture \
  //runsc/ttc/test/registry_fixture/guest:guest \
  //runsc/ttc/test:e2e_local_registry

TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/bazel-bin/runsc/ttcd/ttcd_/ttcd" \
REGISTRY_FIXTURE="$PWD/bazel-bin/runsc/ttc/test/registry_fixture/registry_fixture_/registry_fixture" \
FIXTURE_BINARY="$PWD/bazel-bin/runsc/ttc/test/registry_fixture/guest/guest_/guest" \
  "$PWD/runsc/ttc/test/e2e_local_registry.sh"
```

The same rootless prerequisite and trusted-workload
`TTC_UNSAFE_NO_HOST_ISOLATION=1` fallback as the image matrix apply.
