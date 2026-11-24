# TTC end-to-end image matrix

`e2e_image_matrix.sh` is the authoritative manual smoke test for running
unmodified public Docker images through the complete TTC path. It currently
covers:

- explicit rootless `ttcd` startup over a versioned per-user Unix API,
  full command streaming through the daemon, status/stop controls, live
  sandbox plus console-drainer reconciliation after a daemon restart, and
  replacement of a killed console follower;
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
  rejection for the externally shared network stack; on macOS, which supports
  only `--network=ttc` and `--network=none`, its explicit refusal instead;
- rootless `--network ttc` startup with the `eth0` virtual NIC, NAT-disabled
  isolation, optional IPv4 TCP/UDP user-mode NAT, Docker-style persistent TCP
  publication, branch-aware live publication and removal, daemon-restart
  listener reconciliation, listener cleanup on branch deletion, parent listener
  continuity across live fork, and child external TCP reset/fresh-proxy semantics;
- a Sentry-managed secondary network namespace, made by `unshare -n` in the
  guest, whose loopback service runs on in both branches of a live fork;
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
- Docker `--memory`/`-m`, `--memory-swap` (twice the memory unless given; `-1`
  for unlimited swap), `--cpus`, `--cpu-shares`, `--cpu-period`, and
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
- a disk-backed named volume whose data outlives the container that wrote it;
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
  over versioned timestamped records, including filtering and malformed-record rejection;
- `exec`, selective stdin/stdout/stderr `attach`, transactional `fork`
  behavior, live `fork`, `pause`,
  `resume`/`unpause`, `ps`, `top`, and the lineage tree, including
  branch/status/ancestor `ps` filters, templates, latest selection, JSON
  creation timestamps, and branch-local writable/virtual `ps --size` accounting;
- live fork while a daemon-mediated attached exec is active, and detached
  exec-session ID/PID cloning
  through parent, child, and grandchild with branch-local signal/wait status;
- user tmpfs copy-on-write isolation between a parent and child branch;
- simultaneous read-only host binds, write rejection, shared-view fork
  warnings, and host-side mutation visibility in parent and child;
- persistent isolated writable host views, protection of the host lower,
  inspect output, and parent/child/grandchild CoW divergence;
- graceful stop, branch removal, and container removal.

`e2e_managed_volumes.sh`, the dedicated disk-volume suite, covers the ordinary
rootfs upper: direct disk objects while running, absence of tar snapshots, O(1)
branch creation and divergence, a process running an executable of the upper
across dehydration, exited main/child `ttc cp`, copy into a
created-but-never-started container, and stop/start reopening without explicit
save or restore. For managed volumes, it covers seeding a new volume from the
image and `nocopy`, direct persistence across stop/start and daemon restart,
hard links, symlinks, fallocate, open-FD writes, parent/child/grandchild CoW,
overlapping first writers in two and four branches, and same-path namespace
divergence. It also covers persistent isolated writable host views, with
visibility of uncopied live-lower changes, stop/start persistence, exited
`ttc cp`, checkpoint/restore, and parent/child CoW divergence, and detached
`:copy` imports for directories and individual files, with source deletion
independence, fork divergence, inspect output, and checkpoint/restore. It
takes the image matrix's `TTC`, `TTCD`, `TTC_E2E_ROOT`,
`TTC_UNSAFE_NO_HOST_ISOLATION`, and `BUSYBOX_IMAGE`.

`e2e_fuse_lifecycle.sh` installs `fuse-overlayfs` inside an Ubuntu guest and
keeps its `/dev/fuse` server and writable mount active across live fork,
branch-local writes, dehydration, and rehydration. It starts its own daemon,
from `TTC` and `TTCD`, in a fresh root, or in `TTC_FUSE_TEST_ROOT`, which it
keeps, and needs the network to install the package:

```shell
TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/$(bazel cquery --output=files //runsc/ttcd:ttcd 2>/dev/null)" \
  "$PWD/runsc/ttc/test/e2e_fuse_lifecycle.sh"
```

`e2e_gfx.sh` covers the virtual GPU, `--gfx`, in a Debian guest:

*   the refusal of `--gfx` without fork semantics;
*   the render node and its sysfs entries, and `vulkaninfo`, which must find
    the GPU once although Debian's Mesa brings a Venus driver of its own;
*   repeated offscreen compute and render runs, whose every pixel
    `gfx/vkoffscreen.c` checks;
*   OpenGL ES on EGL's surfaceless platform, which must render by Zink on the
    GPU, every pixel checked (`gfx/gloffscreen.c`). On macOS, where Zink has
    only OpenGL ES 2.0, the test opts in with `MESA_LOADER_DRIVER_OVERRIDE`.
    It must render the same by Mesa's virgl driver, which the renderer
    serves with vkgl;
*   a live fork with `--gfx-fork=device-lost` while the guest renders. In the
    child, the process that was rendering must die of `SIGBUS`, not hang, and
    a new one must render. The source must render on;
*   two branches rendering at once, with their family's renderer, and no
    renderer left once nothing uses the GPU;
*   live forks of a family with `--gfx-fork=snapshot` while the guest
    renders with Vulkan and OpenGL ES: a child, two children at once, and a
    child's child. Every branch's render loops must finish, every frame
    checked, the children's on clones of their parents' GPU state.

It needs Mesa's Venus Vulkan driver for the guest (see
`runsc/ttc/gfxrenderer/README.md`), and the network. On macOS ttcd is the
renderer, with MoltenVK linked in, so the host needs nothing installed; on
Linux ttcd carries the renderer, which loads the host's Vulkan loader and
driver. `TTC_GFX_RENDERER` names a renderer to run instead. The daemon runs
with host isolation unless `TTC_UNSAFE_NO_HOST_ISOLATION=1`; on macOS the
test then checks that the renderers confine themselves. `gfx/webgl.html`,
which no script loads, checks a browser's WebGL and WebGPU in a `--gfx`
container by hand.

```shell
bazel build //runsc/ttc:ttc //runsc/ttcd:ttcd

TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/$(bazel cquery --output=files //runsc/ttcd:ttcd 2>/dev/null)" \
TTC_GFX_GUEST_DRIVER=/path/to/dir/of/libvulkan_virtio.so \
  "$PWD/runsc/ttc/test/e2e_gfx.sh"
```

`e2e_redroid.sh` boots Android with the virtual GPU: redroid's image, of the
host's architecture, whose init is the container's, in a family with
`--gfx-fork=snapshot`. By default it boots redroid's own host mode, by flags
alone: Android's Mesa renders with its virgl driver, which the renderer serves
with vkgl. With
`REDROID_GPU_MODE=venus`, Android renders with ANGLE on Mesa's Venus Vulkan
driver for Android, with `runsc/ttc/gfxrenderer/guest/redroid/gpu_config.sh`. It
checks that SurfaceFlinger composes on the GPU, by the mode's driver and the
renderer's backend, that Android's network comes up on `eth0` and validates on
the internet, that Settings, and the app of `APK` once it installs, render on
the GPU, and that the screen records to video, as scrcpy needs. Then it forks
Android live, and checks that both branches run on, with the same framework, and
that Settings renders in the child. The screens and the recording are left in
`out/` of `TTC_E2E_ROOT`, when that is set. Venus mode needs, besides what
`e2e_gfx.sh` needs, the driver that
`runsc/ttc/gfxrenderer/guest/build_mesa_venus_android.sh` builds (with
`ARCH=x86_64` for x86-64 hosts). Where the host's network blocks Android's DNS
server, 8.8.8.8, `REDROID_DNS` names another.

```shell
TTC=... TTCD=... APK=/path/to/app.apk \
  "$PWD/runsc/ttc/test/e2e_redroid.sh"

REDROID_GPU_MODE=venus TTC=... TTCD=... TTC_GFX_GUEST_DRIVER=... \
ANDROID_GFX_DRIVER=/path/to/vulkan.virtio.so \
  "$PWD/runsc/ttc/test/e2e_redroid.sh"
```

`e2e_nvproxy.sh` covers the host's NVIDIA GPUs, `--nvproxy`, in a Debian
guest:

*   the refusal of `--nvproxy` without fork semantics;
*   the driver's device nodes, `nvidia-smi`, and a CUDA program,
    `nvproxy/cudaloop.cu`, which counts on the GPU and checks the count;
*   a live fork with `--nvproxy-fork=device-lost` while the guest counts. In
    the child, the process that was counting must end, not hang or count
    on, and a new one must count. The source must count on;
*   live forks of a family with `--nvproxy-fork=snapshot` while the guest
    counts: a child, two children at once, and a child's child. Every
    branch's count must finish, checked at every step, the children's from
    their parents' count at the fork.

It needs a host with NVIDIA GPUs, and `cudaloop` built for them. ttcd
carries `ttc-cuda-checkpoint`; `TTC_CUDA_CHECKPOINT` names one to give
containers instead. A host whose driver nvproxy does not support, such as
a pre-release one, names in `TTC_NVPROXY_DRIVER_VERSION` a driver whose
interface it has. On KVM, where TTC refuses `--nvproxy` for now, it checks
the refusal.

`e2e_gfx.sh`, `e2e_redroid.sh` and `e2e_nvproxy.sh` run their daemons on the
platform that `TTC_PLATFORM` names, such as `kvm`, and check that they do.

```shell
bazel build //runsc/ttc:ttc //runsc/ttcd:ttcd
nvcc -O2 -o /tmp/cudaloop runsc/ttc/test/nvproxy/cudaloop.cu

TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/$(bazel cquery --output=files //runsc/ttcd:ttcd 2>/dev/null)" \
CUDALOOP=/tmp/cudaloop \
  "$PWD/runsc/ttc/test/e2e_nvproxy.sh"
```

`e2e_fanout_lifecycle.sh` creates 32 siblings from one source pause, writes a
different 4 KiB page in every branch, and dehydrates/rehydrates alternating
branches before verifying their private contents. Set
`TTC_FANOUT_TEST_BRANCHES` to increase the fanout. Final snapshot-graph
qualification runs this test with 256 siblings and requires zero remaining
bulk objects after its cleanup. It starts no daemon: it requires an already
running one, whose root `TTC_ROOT` names, and a TTC client of that daemon's
build selected through `TTC`.

`e2e_distributed_fanout.sh` splits a configurable high-fanout family across
three cluster nodes, dirties one private page in every remote branch, retires
one owning node with `--migrate-all`, and verifies every value through unified
command routing. Set `TTC_DISTRIBUTED_FANOUT_BRANCHES` to change the fanout,
64 by default. It then stops the cluster, checks that the shared root keeps
the authority's log, and restarts a node from it, which must find no family
left and release every shared snapshot pack.

`e2e_image_roundtrip.sh` commits running, paused, and exited branches to OCI
images, saves and reloads those images through Docker-compatible archives, and
verifies labels, file changes, and whiteout deletions after recreation. It uses
a fresh explicit root and cleans it on completion, unless `TTC_E2E_ROOT` names
one to keep:

```shell
TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/$(bazel cquery --output=files //runsc/ttcd:ttcd 2>/dev/null)" \
  "$PWD/runsc/ttc/test/e2e_image_roundtrip.sh"
```

`e2e_cli_commands.sh` composes the CLI paths that the larger suites do not
repeat: completion, version, and help without a daemon; image and volume
inventory; named exec sessions with `ttc tty stat` and `ttc tty watch`; `cp`
and `diff`; rename, quarantine, pause, and resume; checkpoint and restore into
a new family; dehydration, cold copy and fork, and rehydration; stop of a
paused branch, start, and restart; `top`, `stats`, `logs`, and `ps`; durable
request history through `ttc daemon requests` and `ttc daemon request`;
events; kill and wait; and `rmi`. It starts its own daemon from `TTC` and
`TTCD`, always with `--unsafe-no-host-isolation`, in a fresh root, or in
`TTC_CLI_TEST_ROOT`, which it keeps, and needs `jq`.

`e2e_distributed_multihop.sh` starts a fresh two-node cluster, remotely forks
A-to-B, migrates the child B-to-A, and remotely forks it A-to-B again. It
verifies a continuously running process, exact 32 MiB memory contents, sparse
whole-file materialization, and branch-local filesystem divergence after both
handoffs. `TTC_DISTRIBUTED_TEST_PORT_A` and
`TTC_DISTRIBUTED_TEST_PORT_B` select its host listener ports.

`e2e_distributed_source_loss.sh` starts a three-node cluster, retains a large
demand-paged memory dependency on node A, then destroys node A, which holds the
cluster's authority, before the target becomes independent. The surviving
nodes must take the authority over, stop and permanently mark only the
dependent branch `corrupted`; an unrelated branch on node C must
remain executable. The test also verifies that removal is still available for
the corrupted branch and reaps every process in the failed node's private test
root.

`e2e_control_only.sh` starts two compute nodes and an outbound-only login-node
gateway. Every pull, run, exec, fork, migration, inventory, and port command is
issued through the gateway. The test verifies that the gateway never enters
placement inventory, transaction-private branches remain hidden, and a host
listener transparently follows its branch between remote compute owners.

`e2e_distributed_workload.sh` runs `workload_fixture.py`, a page-checked
Python heap and a synchronous SQLite WAL writer, in a named exec session on a
two-node cluster. It forks the workload remotely, migrates both branches
across, then dehydrates one, copies from it, diffs it, forks it cold, and
rehydrates it, checking in every branch that the workload advances and its
database stays intact. It also checks resource accounting, and that removal
leaves nothing behind. `TTC_DISTRIBUTED_WORKLOAD_MEMORY_MIB` sizes the heap,
128 MiB by default.

`e2e_distributed_active_fs.sh` migrates a Python guest that keeps writing an
SQLite WAL database while the target still materializes its filesystem
lazily, and checks the database's integrity and the image's files while
background publication and drain run. It removes its root, even one that
`TTC_DISTRIBUTED_ACTIVE_FS_ROOT` names, unless
`TTC_DISTRIBUTED_ACTIVE_FS_KEEP_ROOT` is set, and passes its daemons
`TTC_DISTRIBUTED_ACTIVE_FS_DAEMON_ARGS`, `--unsafe-no-host-isolation` by
default.

`e2e_cluster_restart.sh` crashes a whole two-node cluster: it forks a running
branch from node A to node B, SIGKILLs both daemons before the child is
independent, and restarts them together, as a batch system would, twice. The
branches must keep their memory, the child's unfaulted tail through a
shared-memory relay that outlives the daemons; a branch name must stay unique
across the cluster; and a branch must survive the removal of its parent. It
starts its nodes with `ttc daemon start`, so it takes only `TTC`, beside which
must be the `ttcd` of its build, signed on macOS as described below.

`e2e_elastic_lifecycle.sh` joins three nodes and covers a foreground run whose
exited branch becomes durable, unplaced, and copyable from another node; a
remote fork whose memory matches its source's; quarantine kept across
migration; the retirement of node A with `--migrate-all` to nodes that the
scheduler picks, with memory and volume contents intact; and reclamation of
every private fork and migration container once the family is removed.

`e2e_elastic_pressure.sh` gives node A a small memory target,
`TTC_ELASTIC_PRESSURE_SOURCE_TARGET` (512 MiB by default), dirties
`TTC_ELASTIC_PRESSURE_BRANCH_BYTES` (64 MiB by default) in each of eight
branches there, which must make node A pressured, and has `ttc node rebalance`
move branches to the two other nodes, with their data intact. Once the
families are removed, no memory chunk or shared snapshot pack may remain.

`e2e_elastic_drain_recovery.sh` starts the retirement of node A with
`--migrate-all`, SIGKILLs its daemon once the durable drain intent is visible,
and restarts it: the new daemon must adopt its branches, finish the drain, and
stop by itself, leaving every branch running, with its data, on node B or C.

`e2e_lifecycle_state_matrix.sh` composes branch states with daemon retirement
policies across three nodes: stop of paused and dehydrated branches and their
start on another node, restart of paused and dehydrated branches, the refusal
of `ttc daemon stop` without a policy while branches run, retirement with
`--dehydrate-all` and later rehydration elsewhere, and
`ttc daemon stop --migrate-all`.

`e2e_shared_gc_turnover.sh` kills the shared-GC coordinator of a three-node
cluster and at once forks a dehydrated branch to another node: the fork must
wait out the coordinator's fenced lease and the rebuild of its reference
ledger, within 45 seconds, rather than fail. It then forks the child's cold
state back, and checks that removal leaves no shared snapshot pack, cold
manifest, or container. `TTC_SHARED_GC_TEST_CLUSTER_LEASE` sets the lease,
30 seconds by default.

`e2e_publication_restart.sh` races a fresh start, and then a rehydration,
against the asynchronous cold publication of a branch that wrote a 96 MiB
file, on one clustered daemon: each must fence the publication first, and no
late publication may unplace or overwrite the newer runtime.

The cluster tests (`e2e_distributed_*.sh`, `e2e_control_only.sh`,
`e2e_cluster_restart.sh`, `e2e_elastic_*.sh`, `e2e_lifecycle_state_matrix.sh`,
`e2e_shared_gc_turnover.sh`, and `e2e_publication_restart.sh`) run their nodes
on loopback ports, in one fresh root that holds their shared `TTC_ROOT` and
each node's `TTC_LOCAL_ROOT`, and with `--unsafe-no-host-isolation`: run them
only on a trusted host. They take `TTC` and `TTCD` (`e2e_cluster_restart.sh`
only `TTC`), most of them `TTC_E2E_COMMAND_TIMEOUT` for each command, and
variables, set at the top of each script, for their image, ports, and root;
most keep a root that a variable names, such as `TTC_DISTRIBUTED_FANOUT_ROOT`.
They need `timeout`, most of them `jq` and `setsid` too, `e2e_control_only.sh`
`curl`, and `e2e_distributed_workload.sh` GNU's `timeout`, for `--kill-after`.
macOS has neither `setsid` nor `timeout`, so there they need substitutes on
`PATH`. They run `TTCD` as given: on macOS, give them a copy signed as the
other tests sign theirs
(`codesign --force --sign - --entitlements runsc/hvf.entitlements ttcd`), and
a short root, such as `/tmp/tdf`, as their default roots under macOS's
`TMPDIR` are too long for the nodes' sockets. Their cleanup finds leftover
runtimes through Linux's `/proc` and session IDs; on macOS, kill any that
remain before removing a root.

Two guest programs serve stress runs by hand; no script runs them.
`sqlite_fork_stress.py` keeps an SQLite connection writing across forks, in
WAL or rollback-journal mode and optionally through mmap, and checks the
database every 100 writes. `//runsc/ttc/test/workloads:stress`, a static Linux
binary, exercises syscalls, memory, files, `SCM_RIGHTS`, processes, the
network, signals, and synchronization for its `--duration`, and prints its
counters as JSON.

Focused Go tests additionally cover custom TTC NIC IPv4/MAC validation and
runtime propagation, line-oriented image pull/extraction progress, default
guest SUID enablement in TTC's runsc arguments, daemon namespace preflight,
private-anonymous-memory write-fault regression cases, branch-local pidfd target
rebinding, cloning of Sentry-owned netlink sockets, a tmpfs live-fork
`pwrite()` from a mapping in the same memory manager, covering the
source-read-lock versus CoW-invalidation-write-lock ordering that loaded
Chromium exercises, and exact validation of systrap's
`mov $sysno,%eax; syscall` fast-path pattern, preventing false-positive
rewriting of unrelated Chromium/V8 JIT code.

The test requires network access for image pulls, and `curl` and `python3` on
the host for its HTTP probes and NAT fixture. TTC resolves OCI registries
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

macOS has no user namespaces, and needs neither probe nor opt-in: without
`TTC_UNSAFE_NO_HOST_ISOLATION=1`, the harness's `ttcd` confines itself and its
Sentries with the macOS sandbox, Seatbelt, as `ttcd` on macOS does by default.

The tests that start one daemon copy `TTC` and `TTCD` into their root, and on
macOS sign the copy of `ttcd` ad hoc with the Hypervisor entitlement of
`runsc/hvf.entitlements`, which they find relative to their own path: run them
from the checkout. `TTC` and `TTCD` must come from one build, as `ttc` refuses
a `ttcd` of another. A daemon keeps all its files, its socket among them, in
its root, `data/` in a test's root, which the paths of its sockets limit to 76
bytes on macOS and 80 on Linux.

Build and invoke the matrix with:

```shell
bazel build //runsc/ttc:ttc //runsc/ttcd:ttcd \
  //runsc/ttc/test:e2e_image_matrix

TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/$(bazel cquery --output=files //runsc/ttcd:ttcd 2>/dev/null)" \
  "$PWD/runsc/ttc/test/e2e_image_matrix.sh"
```

No-userns example:

```shell
TTC_UNSAFE_NO_HOST_ISOLATION=1 \
TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/$(bazel cquery --output=files //runsc/ttcd:ttcd 2>/dev/null)" \
  "$PWD/runsc/ttc/test/e2e_image_matrix.sh"
```
Use `TTC_E2E_ROOT` to retain the generated TTC state for inspection. Image
references can be overridden with `BUSYBOX_IMAGE`, `ALPINE_IMAGE`,
`PYTHON_IMAGE`, `NGINX_IMAGE`, and `REDIS_IMAGE`, and its 15-second readiness
deadline and 2-second HTTP probe timeout with `TTC_E2E_DEADLINE_SECONDS` and
`TTC_E2E_GUEST_CURL_TIMEOUT`. By default, temporary TTC state, including the
native image content, is removed on success or failure.

A passing result is stronger than the unit suite: it proves image pulling,
layer extraction, OCI generation, rootless runsc startup, versioned daemon
transport and restart recovery, nontrivial userspace, console sidecars, and
TimeTravel branch operations on the executing host.

## Branch-aware copy integration

`e2e_cp.sh` is the focused host-to-container, container-to-host, and tar-stream
test for `ttc cp`. It copies into main, forks, writes distinct files into both
branches, exports the child, and imports the child's binary tar stream into the
parent. It also checks directory `/.` behavior, symlinks, hard links, and
modes, without guest utilities, and copying in both directions while the
ordinary tasks of the target branch stay paused.

```shell
bazel build //runsc/ttc:ttc //runsc/ttcd:ttcd \
  //runsc/ttc/test:e2e_cp

TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/$(bazel cquery --output=files //runsc/ttcd:ttcd 2>/dev/null)" \
  "$PWD/runsc/ttc/test/e2e_cp.sh"
```

Use `TTC_UNSAFE_NO_HOST_ISOLATION=1` only on a trusted host that cannot create
unprivileged user namespaces. Filesystems without `FICLONE` are supported
automatically through whole-file copying.

## Terminal and live-fork integration

`e2e_terminal.sh` is the authoritative real-PTY test for `run -it`, detached
terminal containers, the `ttcd` terminal transport, attach/detach keys,
branch-local terminal state after live fork, pause/resume, persistent logs,
client `TERM` propagation, terminal ioctls, and `exec -it`. It repeats
quick-exit terminal runs to cover the pre-start console
follower handshake, verifies that their output is never lost, and requires
`exec -it` to close promptly after the guest process exits. The 60-second
per-operation timeout is a deadlock guard, not an accepted teardown delay. The
image matrix uses the Linux release requirements on every host: 15 seconds for
lifecycle and service readiness, and 2 seconds for individual HTTP probes.
macOS has no correctness-only timeout exception.

The `runsc/boot:boot_test` live-fork coverage also installs the same explicitly
transient channel waiter used by active TTC port forwarding. It verifies that
the source registration survives snapshotting, the child omits that host-runtime
waiter, and ordinary Sentry listeners are not classified by implementation
type.

```shell
bazel build //runsc/ttc:ttc //runsc/ttcd:ttcd \
  //runsc/ttc/test:e2e_terminal

TTC_UNSAFE_NO_HOST_ISOLATION=1 \
TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/$(bazel cquery --output=files //runsc/ttcd:ttcd 2>/dev/null)" \
  "$PWD/runsc/ttc/test/e2e_terminal.sh"
```

Omit `TTC_UNSAFE_NO_HOST_ISOLATION=1` on macOS, and on a Linux host where
`unshare -Ur /bin/true` succeeds. The test uses util-linux `script` and
`timeout` on Linux; on macOS it uses BSD `script` and Perl as the timeout
fallback. Set `TTC_E2E_ROOT` to retain terminal transcripts, console logs, and
runtime state.

## Docker main-container compatibility

`e2e_docker_main.sh` verifies that the TimeTravel-enabled `runsc` retains its
standard OCI runtime surface. It starts an isolated privileged Docker-in-Docker
daemon, uses `runsc install` to register `runsc-ttc`, and exercises Docker
create, start, inspect, logs, exec, pause, unpause, stop, and remove against an
unmodified image. It never changes the host daemon configuration.

```shell
bazel build //runsc/ttcd:ttcd //runsc/ttc/test:e2e_docker_main

RUNSC="$PWD/$(bazel cquery --output=files //runsc/ttcd:ttcd 2>/dev/null)" \
  "$PWD/runsc/ttc/test/e2e_docker_main.sh"
```

`RUNSC` is a `ttcd`, which embeds runsc: the test mounts it as `runsc` in the
Docker-in-Docker container, a name that selects that entry point, so it must
be a Linux build. The test requires Docker access, network access for image
pulls, and permission to launch a privileged container. Override `DIND_IMAGE`,
`TEST_IMAGE`, or `TEST_NAME` when needed.

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
bazel build //runsc/ttc:ttc //runsc/ttcd:ttcd \
  //runsc/ttc/test/registry_fixture:registry_fixture \
  //runsc/ttc/test/registry_fixture/guest:guest_fixture \
  //runsc/ttc/test:e2e_local_registry

TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/$(bazel cquery --output=files //runsc/ttcd:ttcd 2>/dev/null)" \
REGISTRY_FIXTURE="$PWD/bazel-bin/runsc/ttc/test/registry_fixture/registry_fixture_/registry_fixture" \
FIXTURE_BINARY="$PWD/bazel-bin/runsc/ttc/test/registry_fixture/guest/guest_fixture_/guest_fixture" \
  "$PWD/runsc/ttc/test/e2e_local_registry.sh"
```

The same rootless prerequisite and trusted-workload
`TTC_UNSAFE_NO_HOST_ISOLATION=1` fallback as the image matrix apply. The guest
runs `FIXTURE_BINARY`, so on macOS it must be a Linux build of
`guest_fixture`, not the host's; `FIXTURE_GUEST_ARCH` sets the architecture
that the image records, the host's by default.
