# TimeTravelContainer

TimeTravelContainer (TTC) is an experimental, lightweight, rootless Linux container
manager with one unusual primitive: it can pause, resume, and **live-fork an
entire running container** into independent branches.

A live fork happens in memory. TTC does not write a checkpoint image, copy all
container RAM, invoke CRIU, or require KVM. Guest processes, threads, virtual
kernel state, supported IPC objects, writable filesystems, and internal network
state are cloned together. Existing data pages begin shared and detach
copy-on-write as either branch modifies them.

> [!WARNING]
> TTC is currently intended for trusted development, integration testing,
> state-space exploration, and similar workloads. It is not yet a production
> security boundary or a drop-in replacement for every Docker workload.
> Every `ttc fork` atomically pauses and validates its source. On rejection,
> TTC resumes the source and returns a diagnostic with a nonzero status.

TimeTravelContainer is forked from [Google's gVisor](https://github.com/google/gvisor).
It builds on gVisor's Sentry userspace kernel, systrap platform, netstack, VFS,
and OCI-compatible `runsc` runtime. Google and the gVisor project are not
responsible for or affiliated with the TimeTravelContainer-specific modifications.

## Security principles

Upstream gVisor is designed to be a strong production sandbox for potentially
hostile workloads. Its
[security model](https://gvisor.dev/docs/architecture_guide/security/) minimizes
direct access to the host kernel and combines the Sentry boundary with host
namespaces, chroots, seccomp filters, and separate gofer processes. Its
[vulnerability policy](https://gvisor.dev/security/) consequently focuses on
crossing the sandbox boundary.

TTC inherits that architecture but currently has a narrower deployment goal:
trusted development and integration-test workloads rather than untrusted
multi-tenant production. TTC intentionally does not promise security or failure
isolation between live-fork branches in one family because they share a Sentry
process. Its explicit `--unsafe-no-host-isolation` daemon mode also removes the
normal host user-namespace and chroot layers and therefore trusts every image
and process run by that daemon.

Those deliberate limitations are not permission to introduce new escape paths.
TTC follows these principles:

- In normal rootless mode, guest code must not escape the gVisor boundary,
  access host resources that were not explicitly attached, or interfere with
  unrelated container families through a TTC modification.
- TTC must not create an unintended guest privilege-escalation path. Linux
  credential transitions explicitly requested by the image or configuration,
  including SUID/SGID semantics within the virtual kernel, are not themselves
  vulnerabilities and must not grant corresponding host credentials.
- Fork, restore, mount, image, and resource-import paths fail closed whenever
  TTC cannot prove that an operation preserves its declared isolation and
  ownership rules.
- Security-relevant relaxations must be explicit, narrowly scoped, documented,
  and testable. TTC must not silently fall back from normal isolation to an
  unsafe mode.
- Upstream gVisor hardening must remain intact unless TTC replaces it with a
  complete design that has equivalent safety invariants and focused security
  tests. Performance or compatibility alone does not justify a partial
  weakening.
- A vulnerability introduced by TTC remains a bug even if current users are
  advised to run trusted workloads. The trusted-workload recommendation
  reflects the project's audit maturity, not an acceptance of sandbox escapes.

The intentionally shared trust boundary between branches and the explicitly
unsafe daemon mode are excluded from this guarantee as documented. Everything
else should be treated conservatively until TTC has undergone a dedicated
security review. Read-only host binds, `--network host`, published ports, and
`--network ttc --nat` are explicit grants of host-facing access. In particular,
TTC NAT deliberately maps the guest gateway and same-subnet host aliases to
host loopback services; use `--network ttc` without `--nat` when that access is
not wanted.

The concrete credential-policy defects found by the current audit have been
fixed. A non-root initial or exec process retains the configured capability
bounding set but does not inherit root's effective or permitted capabilities;
this prevents a direct `setuid(0)` or `CAP_DAC_OVERRIDE` bypass. Root-owned
SUID/SGID programs can still elevate inside the Sentry because their required
capabilities remain bounded unless explicitly dropped. Numeric-ID parsing and
the upstream exiting-task race are also covered by focused regressions. These
fixes strengthen the virtual credential boundary but do not change TTC's
trusted-workload status or constitute a security certification; see the
[security, performance, and compatibility audit](g3doc/proposals/timetravel_ttc_audit.md)
for scope and exclusions.

## Why TimeTravelContainer?

An ordinary container runtime can start, stop, pause, or checkpoint a
container. TTC additionally treats a live container as a branching state tree:

```text
demo@main
├── demo@test-a
│   └── demo@test-a-2
└── demo@test-b
```

Every branch has its own virtual process tree, memory view, writable filesystem,
supported IPC graph, timers, console state, and contained network stack.
Virtual observables such as PIDs remain the same at the fork boundary because
each branch owns a separate Sentry kernel object graph.

This is useful when many tests share an expensive setup phase:

1. Boot an unmodified OCI/Docker image.
2. Initialize the application and its data.
3. Live-fork the initialized container.
4. Run a different test or mutation in each branch.
5. Discard branches without rebuilding the initial state.

Fork cost scales primarily with kernel metadata—tasks, mappings, file
descriptions, queues, and filesystem objects—not with the number of resident
application bytes. See the
[security, performance, and compatibility audit](g3doc/proposals/timetravel_ttc_audit.md)
for measured results and current limits.

## Features

### Live container branching

- Pause and resume an individual live branch.
- Fork any eligible branch recursively into a running or initially paused child.
- Preserve guest PIDs, threads, registers, credentials, clocks, signals,
  namespaces, file-offset topology, and supported kernel resources.
- Share anonymous memory, tmpfs, SysV shared memory, and writable OverlayFS
  data page-by-page until a branch writes.
- Clone internal pipes, eventfd, epoll graphs, timerfd, signalfd, inotify,
  pidfds, netlink sockets, internal Unix sockets, loopback TCP/UDP, and other
  explicitly validated Sentry-owned objects.
- Validate the complete reachable resource graph after pausing and before any
  fork mutation; on failure, remove the tentative child and resume the source.
- Display immutable ancestry, deleted-parent tombstones, branch state, PID 1
  state, and CoW status with `ttc ls --tree`.

All branches in one family currently live inside one Sentry process. This is
what makes the operation fast, but branches are not security- or
failure-isolated from one another.

### Docker-like container workflow

The `ttc` CLI provides familiar commands without requiring a Docker or
containerd daemon:

- images: `pull`, `images`, `image inspect`, `rmi`;
- lifecycle: `run`, `create`, `start`, `stop`, `restart`, `wait`,
  `kill`, and `rm`;
- observation: `inspect`, `ps`, `top`, `logs`, and `events`;
- interaction: `exec` and `attach`;
- TimeTravel: `fork`, `pause`, `resume`, `branch rm`, and `ls --tree`;
- storage: managed `volume` create, inspect, import, export, commit, and remove;
- completion: Bash, Zsh, and Fish scripts.

Run/create supports common Docker-shaped options for environment variables,
labels, entrypoints, working directories, users and groups, capabilities,
ulimits, stop policy, read-only roots, tmpfs, volumes, binds, logging, DNS,
host entries, network mode, published ports, and resource limits.

### Native rootless image management

TTC talks directly to OCI and Docker v2 registries and keeps a private,
content-addressed image store under its data root. It supports:

- Docker Hub shorthand such as `alpine:3.20`;
- tags, digests, and multi-architecture manifest selection;
- Docker configuration authentication and credential helpers;
- `missing`, `always`, and `never` pull policies;
- human-readable resolve, download, reuse, and extraction progress;
- digest-only quiet pulls;
- immutable materialized roots without a host snapshotter or privileged mount.

Physical image files remain owned by the invoking host user. TTC records their
OCI UID/GID values in a sidecar ownership manifest and makes the gofer expose
those values to the guest. Arbitrary image users therefore do not require
`newuidmap`, `newgidmap`, or subordinate UID/GID ranges.

Guest SUID/SGID execution is enabled by default for compatibility with images
that use `sudo` or `su`. It changes only Sentry-emulated credentials and
never grants host root; OCI `noNewPrivileges` still suppresses elevation.

### Forkable and persistent storage

- The image root is an immutable lower layer.
- The running container gets a Sentry-memory OverlayFS upper by default.
- Live-forked branches share upper-layer pages CoW.
- Main-branch rootfs changes, including shutdown-handler writes, are exported
  on exit and seed the next `ttc start`.
- Arbitrary Sentry-owned tmpfs, alternate procfs, restricted read-only sysfs,
  `/dev`, `/dev/shm`, and private devpts mounts are supported.
- Read-only host binds are allowed as shared external views.
- A TTC-specific `cow,immutable-source` bind places a branch-local memory
  upper over a user-asserted immutable host lower.
- Directfs is disabled so host open-file descriptions do not silently become
  shared branch state.

The former tmpfs-plus-tar managed-volume driver has been removed because it did
not provide normal durable-volume semantics. Named, anonymous, and unoverridden
image-declared volumes are temporarily rejected. The replacement is a
disk-backed Sentry filesystem with lazy metadata CoW and host reflink file CoW;
see the [disk-backed volume design](g3doc/proposals/ttc_disk_backed_volumes.md).

### Rootless virtual networking

`--network ttc` creates a Sentry-managed Ethernet interface and gateway
entirely in userspace:

```text
interface   ttc0
address     10.0.2.15/24
gateway     10.0.2.2
MAC         02:42:0a:00:02:0f
```

It requires no TAP device, veth pair, bridge, CNI, iptables, network namespace
manager, or `CAP_NET_ADMIN`.

- `--nat` enables rootless user-mode IPv4 TCP/UDP outbound access, including
  deliberate access to host-loopback aliases in the guest NIC's `/24`.
- `-p`/`--publish` creates persistent rootless IPv4 TCP host listeners.
- `--ip` and `--mac-address` customize the persistent guest NIC identity.
- The parent retains established external connections and published listeners
  across a fork.
- The child receives a fresh proxy realm; inherited external TCP flows are
  reset, while internal loopback and `ttc0` state is cloned.
- A child branch is not automatically assigned a published host port.
- `ttc port publish`, `port ls`, and `port unpublish` manage persistent TCP
  listeners for `main` or any existing live fork without restarting it.

```shell
ttc port publish web@child 127.0.0.1:8081:80
ttc port ls web
ttc port unpublish web@child 127.0.0.1:8081:80
```

The default `--network none` provides a forkable loopback-only netstack.
`--network host` is available for conventional execution but is not
live-forkable.

### Interactive processes, logs, and recovery

- Foreground, detached, stdin-attached, and PTY-backed `-it` containers.
- Branch-aware synchronous, detached, and interactive exec sessions.
- Detach and later reattach without placing host console FDs in the guest graph.
- Client `TERM` propagation, terminal resize, job-control signals, and
  branch-local PTY state.
- Persistent, timestamped, bounded logs with follow, tail, time filters, and
  Docker-style detail selection.
- A durable per-user `ttcd` service with peer-UID checks, serialized
  mutations, idempotent request results, events, and crash reconciliation.
- Orderly daemon shutdown that stops managed branches and persists rootfs and
  volume state.
- A `--direct` recovery mode when bypassing the daemon is necessary.

## Architecture

TTC consists of three binaries:

```text
ttc
  Docker-like CLI and TimeTravel branch commands
    │
    ▼
ttcd
  per-user lifecycle owner, image store, logs, events, ports, recovery
    │
    ▼
runsc
  OCI runtime
    ├── Sentry userspace Linux kernel
    │     ├── SandboxFamily
    │     ├── one Kernel object graph per branch
    │     ├── shared page allocators with branch-aware CoW
    │     └── userspace netstack and TTC proxy realm
    └── gofer/LISAFS immutable host-file access
```

A live fork pauses the source, validates its reachable resource graph, prepares
supported page owners for CoW, streams Sentry metadata through an in-memory
`state.Save`/`state.Load` pipe, reconstructs runtime-only child resources,
then resumes the requested branches. It does not invoke host `fork()` on the
Go runtime and does not create a disk checkpoint.

The detailed design is in
[TimeTravelContainer live container fork](g3doc/proposals/timetravel_live_fork.md).

## Requirements and rootless modes

TTC currently targets Linux x86-64 and ARM64 and uses gVisor's systrap
platform. It does not require KVM, CRIU, Docker, containerd, or root privileges
at runtime.

The default mode uses an unprivileged host user namespace with a one-entry
UID/GID map:

```sh
unshare -Ur /bin/true
```

No subordinate IDs or `uidmap` helpers are needed. `ttc daemon start`
performs an equivalent disposable user/mount-namespace preflight and fails
immediately if the kernel or an LSM such as AppArmor rejects it.

For trusted workloads on hosts that prohibit unprivileged user namespaces:

```sh
ttc daemon start --unsafe-no-host-isolation
```

This fallback runs the Sentry and gofer as the invoking user without creating
their usual host isolation namespaces or chroots. Guest namespaces,
credentials, mounts, networking, and CoW still exist inside the Sentry, and the
gofer still installs seccomp filters, but host defense in depth is removed.
Anything accessible to the invoking host user is in the potential compromise
scope. Do not use this mode for untrusted images.

The daemon owns one isolation mode for its entire lifetime. Clients inherit the
mode and do not send or negotiate it. Stop the daemon before changing modes.

Hard CPU and memory limits additionally require a writable delegated cgroup v2
subtree. TTC fails closed when a requested hard limit cannot be verified.
Sentry task/PID limits do not require host cgroup delegation.

## Build

### Docker-based build wrapper

Docker is needed only to run this build path, not to operate the resulting
TTC binaries:

```sh
mkdir -p bin
make copy TARGETS="runsc runsc/ttc runsc/ttcd" DESTINATION="$PWD/bin"
```

The output directory contains:

```text
bin/runsc
bin/ttc
bin/ttcd
```

### Direct Bazel build

Install the native dependencies described in
[`images/default/Dockerfile`](images/default/Dockerfile), use the Bazel version
from [`.bazelversion`](.bazelversion), then run:

```sh
bazel build //runsc:runsc //runsc/ttc:ttc //runsc/ttcd:ttcd
```

Outputs are written to:

```text
bazel-bin/runsc/runsc_/runsc
bazel-bin/runsc/ttc/ttc_/ttc
bazel-bin/runsc/ttcd/ttcd_/ttcd
```

No privileged installation is required. Keep the three binaries together so
`ttc` can discover `runsc` and `ttcd`, or configure `TTC_RUNSC`.
The [complete user guide](g3doc/user_guide/timetravelcontainer.md) includes a per-user
installation layout and build dependency list.

## Quick start

The commands below use the Docker-built binaries from `./bin`.

Start the per-user service explicitly:

```sh
./bin/ttc daemon start
./bin/ttc daemon status
```

Pull and run an unmodified image:

```sh
./bin/ttc pull alpine:3.20
./bin/ttc run --rm alpine:3.20 uname -a
```

Start an interactive container in the background:

```sh
./bin/ttc run -dit --name demo alpine:3.20 sh
./bin/ttc exec -it demo sh
```

Fork it:

```sh
./bin/ttc fork demo demo@test-a
./bin/ttc fork --paused demo demo@test-b
./bin/ttc ls --tree
./bin/ttc resume demo@test-b
```

Each selector is `CONTAINER@BRANCH`; omitting the suffix selects `main`.
After the fork, interact with branches independently:

```sh
./bin/ttc exec demo@test-a sh -c 'echo child-a >/tmp/result'
./bin/ttc exec demo@test-b sh -c 'echo child-b >/tmp/result'
./bin/ttc exec demo@test-a cat /tmp/result
./bin/ttc exec demo@test-b cat /tmp/result
./bin/ttc pause demo@test-a
./bin/ttc resume demo@test-a
```

Attach to a branch's init console and detach with Ctrl-P, Ctrl-Q:

```sh
./bin/ttc attach -i demo@test-b
```

Stop and remove the family:

```sh
./bin/ttc stop demo
./bin/ttc rm --force demo
```

### Rootless network example

```sh
./bin/ttc run -d --name web \
  --network ttc --nat \
  --ip 10.23.4.15 --mac-address 02:aa:bb:cc:dd:ee \
  -p 127.0.0.1:8080:80 \
  nginx:1.27-alpine
curl http://127.0.0.1:8080/
```

When finished, stop every managed container and the daemon cleanly:

```sh
./bin/ttc daemon stop
```

See [the user guide](g3doc/user_guide/timetravelcontainer.md) for image authentication,
mount syntax, resource limits, logging, events, shell completion, daemon
recovery, and the full command surface.

## Current limitations

- Live fork is implemented only for systrap.
- A family shares one Sentry process and is not a security boundary between
  branches.
- Fork time is O(kernel metadata), not universally constant time.
- TTC is not a Docker Engine API, Compose implementation, swarm manager,
  general device runtime, or named-network implementation.
- Host networking, imported host FDs, writable gofer filesystems, external Unix
  peers, queued `SCM_RIGHTS`, AIO/io_uring rings, packet mmap, KCOV, devices,
  GPU/TPU proxy state, and unrecognized resources are rejected by live-fork
  validation.
- TTC user-mode NAT is currently IPv4 TCP/UDP; published ports are IPv4 TCP.
- A stopped child branch cannot be recreated from memory. Start/restart rebuilds
  the main branch from persistent configuration, its immutable image, the last
  exported main rootfs upper.
- Managed volumes are unavailable until the disk-backed reflink CoW driver is
  implemented.
- Read-only host binds remain shared external views rather than snapshots.
- Uncommon OCI metadata such as device nodes, privileged security xattrs, and
  executable file capabilities needs broader rootless virtualization coverage.
- One image reference currently records one selected platform.
- Image signatures, attestations, and content-trust policy are not implemented.
- Live branches cannot be migrated to another Sentry or host, and TTC does not
  persist them as checkpoint images.
- The unsafe no-host-isolation mode is only appropriate for trusted workloads.

The authoritative runtime decision is `ttc fork` itself: it pauses the source,
validates the frozen resource graph, and either creates the child or resumes the
source and fails without leaving a child. The full
supported/rejected resource matrix and open performance work are documented in
the [audit](g3doc/proposals/timetravel_ttc_audit.md).

## Testing

Run the focused TTC command, networking, memory, and tmpfs tests through the
Docker build wrapper:

```sh
make test TARGETS="//runsc/ttc/cmd:cmd_test //runsc/boot:boot_test \
  //pkg/sentry/mm:mm_test //pkg/sentry/fsimpl/tmpfs:tmpfs_test"
```

The public-image end-to-end matrix exercises BusyBox, Alpine, Python, nginx,
Redis, image lifecycle, exec, logs, filesystems, volumes, pause/resume,
recursive fork, daemon recovery, and rootless networking:

```sh
bazel build //runsc:runsc //runsc/ttc:ttc //runsc/ttcd:ttcd \
  //runsc/ttc/test:e2e_image_matrix

TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
RUNSC="$PWD/bazel-bin/runsc/runsc_/runsc" \
  "$PWD/runsc/ttc/test/e2e_image_matrix.sh"
```

See the [TTC test guide](runsc/ttc/test/README.md) for the real-PTY suite,
hermetic registry fixture, Docker-main compatibility test, retained test state,
and the trusted no-user-namespace variant.

## Documentation

- [End-user guide](g3doc/user_guide/timetravelcontainer.md)
- [Performance and compatibility audit](g3doc/proposals/timetravel_ttc_audit.md)
- [Live-fork design](g3doc/proposals/timetravel_live_fork.md)
- [Deferred transparent systrap stub-reallocation design](g3doc/proposals/timetravel_systrap_stub_reallocation.md)
- [Product and CLI roadmap](g3doc/proposals/timetravel_ttc_cli.md)
- [End-to-end test guide](runsc/ttc/test/README.md)
- [Upstream gVisor documentation](https://gvisor.dev/docs/)

## Relationship to gVisor

TimeTravelContainer would not exist without gVisor. The fork retains gVisor's
userspace Linux kernel, OCI runtime, filesystem and networking implementations,
build system, tests, documentation, and copyright notices, then adds the
TimeTravelContainer live-fork kernel paths and local container-management product.

Changes intended specifically for TTC should be developed and reported against
the [TimeTravelContainer repository](https://github.com/gzz2000/TimeTravelContainer).
General gVisor usage, architecture, and upstream contribution information is
available from the [gVisor project](https://github.com/google/gvisor) and
[gvisor.dev](https://gvisor.dev/).

This repository is licensed under the [Apache License 2.0](LICENSE).
