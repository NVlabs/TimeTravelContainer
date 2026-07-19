# TimeTravelOS

TimeTravelOS (TTOS) is an experimental, lightweight, rootless Linux container
manager with one unusual primitive: it can pause, resume, and **live-fork an
entire running container** into independent branches.

A live fork happens in memory. TTOS does not write a checkpoint image, copy all
container RAM, invoke CRIU, or require KVM. Guest processes, threads, virtual
kernel state, supported IPC objects, writable filesystems, and internal network
state are cloned together. Existing data pages begin shared and detach
copy-on-write as either branch modifies them.

> [!WARNING]
> TTOS is currently intended for trusted development, integration testing,
> state-space exploration, and similar workloads. It is not yet a production
> security boundary or a drop-in replacement for every Docker workload. Always
> run `ttos fork --check` before relying on a fork.

TimeTravelOS is forked from [Google's gVisor](https://github.com/google/gvisor).
It builds on gVisor's Sentry userspace kernel, systrap platform, netstack, VFS,
and OCI-compatible `runsc` runtime. Google and the gVisor project are not
responsible for or affiliated with the TimeTravelOS-specific modifications.

## Why TimeTravelOS?

An ordinary container runtime can start, stop, pause, or checkpoint a
container. TTOS additionally treats a live container as a branching state tree:

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
[performance and compatibility audit](g3doc/proposals/timetravel_ttos_audit.md)
for measured results and current limits.

## Features

### Live container branching

- Pause and resume an individual live branch.
- Fork any eligible branch recursively into a running or initially paused child.
- Preserve guest PIDs, threads, registers, credentials, clocks, signals,
  namespaces, file-offset topology, and supported kernel resources.
- Share anonymous memory, tmpfs, SysV shared memory, managed volumes, and
  writable OverlayFS data page-by-page until a branch writes.
- Clone internal pipes, eventfd, epoll graphs, timerfd, signalfd, inotify,
  internal Unix sockets, loopback TCP/UDP, and other explicitly validated
  Sentry-owned objects.
- Validate the complete reachable resource graph before mutation with
  `ttos fork --check`.
- Display immutable ancestry, deleted-parent tombstones, branch state, PID 1
  state, and CoW status with `ttos ls --tree`.

All branches in one family currently live inside one Sentry process. This is
what makes the operation fast, but branches are not security- or
failure-isolated from one another.

### Docker-like container workflow

The `ttos` CLI provides familiar commands without requiring a Docker or
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

TTOS talks directly to OCI and Docker v2 registries and keeps a private,
content-addressed image store under its data root. It supports:

- Docker Hub shorthand such as `alpine:3.20`;
- tags, digests, and multi-architecture manifest selection;
- Docker configuration authentication and credential helpers;
- `missing`, `always`, and `never` pull policies;
- human-readable resolve, download, reuse, and extraction progress;
- digest-only quiet pulls;
- immutable materialized roots without a host snapshotter or privileged mount.

Physical image files remain owned by the invoking host user. TTOS records their
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
  on exit and seed the next `ttos start`.
- Managed named volumes fork CoW and can be committed, imported, exported, and
  restored across lifecycle operations.
- Arbitrary Sentry-owned tmpfs, alternate procfs, restricted read-only sysfs,
  `/dev`, `/dev/shm`, and private devpts mounts are supported.
- Read-only host binds are allowed as shared external views.
- A TTOS-specific `cow,immutable-source` bind places a branch-local memory
  upper over a user-asserted immutable host lower.
- Directfs is disabled so host open-file descriptions do not silently become
  shared branch state.

Child-branch filesystem changes are not automatically promoted into the main
branch's restart seed. Commit the relevant managed volume explicitly when that
is the desired result.

### Rootless virtual networking

`--network ttos` creates a Sentry-managed Ethernet interface and gateway
entirely in userspace:

```text
interface   ttos0
address     10.0.2.15/24
gateway     10.0.2.2
MAC         02:42:0a:00:02:0f
```

It requires no TAP device, veth pair, bridge, CNI, iptables, network namespace
manager, or `CAP_NET_ADMIN`.

- `--nat` enables rootless user-mode IPv4 TCP/UDP outbound access.
- `-p`/`--publish` creates persistent rootless IPv4 TCP host listeners.
- `--ip` and `--mac-address` customize the persistent guest NIC identity.
- The parent retains established external connections and published listeners
  across a fork.
- The child receives a fresh proxy realm; inherited external TCP flows are
  reset, while internal loopback and `ttos0` state is cloned.
- A child branch is not automatically assigned a published host port.

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
- A durable per-user `ttosd` service with peer-UID checks, serialized
  mutations, idempotent request results, events, and crash reconciliation.
- Orderly daemon shutdown that stops managed branches and persists rootfs and
  volume state.
- A `--direct` recovery mode when bypassing the daemon is necessary.

## Architecture

TTOS consists of three binaries:

```text
ttos
  Docker-like CLI and TimeTravel branch commands
    │
    ▼
ttosd
  per-user lifecycle owner, image store, logs, events, ports, recovery
    │
    ▼
runsc
  OCI runtime
    ├── Sentry userspace Linux kernel
    │     ├── SandboxFamily
    │     ├── one Kernel object graph per branch
    │     ├── shared page allocators with branch-aware CoW
    │     └── userspace netstack and TTOS proxy realm
    └── gofer/LISAFS immutable host-file access
```

A live fork pauses the source, validates its reachable resource graph, prepares
supported page owners for CoW, streams Sentry metadata through an in-memory
`state.Save`/`state.Load` pipe, reconstructs runtime-only child resources,
then resumes the requested branches. It does not invoke host `fork()` on the
Go runtime and does not create a disk checkpoint.

The detailed design is in
[TimeTravelOS live container fork](g3doc/proposals/timetravel_live_fork.md).

## Requirements and rootless modes

TTOS currently targets Linux x86-64 and ARM64 and uses gVisor's systrap
platform. It does not require KVM, CRIU, Docker, containerd, or root privileges
at runtime.

The default mode uses an unprivileged host user namespace with a one-entry
UID/GID map:

```sh
unshare -Ur /bin/true
```

No subordinate IDs or `uidmap` helpers are needed. `ttos daemon start`
performs an equivalent disposable user/mount-namespace preflight and fails
immediately if the kernel or an LSM such as AppArmor rejects it.

For trusted workloads on hosts that prohibit unprivileged user namespaces:

```sh
ttos daemon start --unsafe-no-host-isolation
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
subtree. TTOS fails closed when a requested hard limit cannot be verified.
Sentry task/PID limits do not require host cgroup delegation.

## Build

### Docker-based build wrapper

Docker is needed only to run this build path, not to operate the resulting
TTOS binaries:

```sh
mkdir -p bin
make copy TARGETS="runsc runsc/ttos runsc/ttosd" DESTINATION="$PWD/bin"
```

The output directory contains:

```text
bin/runsc
bin/ttos
bin/ttosd
```

### Direct Bazel build

Install the native dependencies described in
[`images/default/Dockerfile`](images/default/Dockerfile), use the Bazel version
from [`.bazelversion`](.bazelversion), then run:

```sh
bazel build //runsc:runsc //runsc/ttos:ttos //runsc/ttosd:ttosd
```

Outputs are written to:

```text
bazel-bin/runsc/runsc_/runsc
bazel-bin/runsc/ttos/ttos_/ttos
bazel-bin/runsc/ttosd/ttosd_/ttosd
```

No privileged installation is required. Keep the three binaries together so
`ttos` can discover `runsc` and `ttosd`, or configure `TTOS_RUNSC`.
The [complete user guide](g3doc/user_guide/timetravelos.md) includes a per-user
installation layout and build dependency list.

## Quick start

The commands below use the Docker-built binaries from `./bin`.

Start the per-user service explicitly:

```sh
./bin/ttos daemon start
./bin/ttos daemon status
```

Pull and run an unmodified image:

```sh
./bin/ttos pull alpine:3.20
./bin/ttos run --rm alpine:3.20 uname -a
```

Start an interactive container in the background:

```sh
./bin/ttos run -dit --name demo alpine:3.20 sh
./bin/ttos exec -it demo sh
```

Check and fork it:

```sh
./bin/ttos fork --check demo
./bin/ttos fork demo demo@test-a
./bin/ttos fork --paused demo demo@test-b
./bin/ttos ls --tree
./bin/ttos resume demo@test-b
```

Each selector is `CONTAINER@BRANCH`; omitting the suffix selects `main`.
After the fork, interact with branches independently:

```sh
./bin/ttos exec demo@test-a sh -c 'echo child-a >/tmp/result'
./bin/ttos exec demo@test-b sh -c 'echo child-b >/tmp/result'
./bin/ttos exec demo@test-a cat /tmp/result
./bin/ttos exec demo@test-b cat /tmp/result
./bin/ttos pause demo@test-a
./bin/ttos resume demo@test-a
```

Attach to a branch's init console and detach with Ctrl-P, Ctrl-Q:

```sh
./bin/ttos attach -i demo@test-b
```

Stop and remove the family:

```sh
./bin/ttos stop demo
./bin/ttos rm --force demo
```

### Rootfs and managed-volume example

```sh
./bin/ttos volume create --opt size=256m state
./bin/ttos run -d --name db \
  --volume state:/var/lib/app \
  alpine:3.20 sleep 600
./bin/ttos fork db db@test
./bin/ttos volume inspect --usage db@test state
./bin/ttos volume commit db@test state
```

### Rootless network example

```sh
./bin/ttos run -d --name web \
  --network ttos --nat \
  --ip 10.23.4.15 --mac-address 02:aa:bb:cc:dd:ee \
  -p 127.0.0.1:8080:80 \
  nginx:1.27-alpine
curl http://127.0.0.1:8080/
```

When finished, stop every managed container and the daemon cleanly:

```sh
./bin/ttos daemon stop
```

See [the user guide](g3doc/user_guide/timetravelos.md) for image authentication,
mount syntax, resource limits, logging, events, shell completion, daemon
recovery, and the full command surface.

## Current limitations

- Live fork is implemented only for systrap.
- A family shares one Sentry process and is not a security boundary between
  branches.
- Fork time is O(kernel metadata), not universally constant time.
- TTOS is not a Docker Engine API, Compose implementation, swarm manager,
  general device runtime, or named-network implementation.
- Host networking, imported host FDs, writable gofer filesystems, external Unix
  peers, queued `SCM_RIGHTS`, AIO/io_uring rings, packet mmap, KCOV, devices,
  GPU/TPU proxy state, and unrecognized resources are rejected by live-fork
  validation.
- TTOS user-mode NAT is currently IPv4 TCP/UDP; published ports are IPv4 TCP.
- A stopped child branch cannot be recreated from memory. Start/restart rebuilds
  the main branch from persistent configuration, its immutable image, the last
  exported main rootfs upper, and persistent volume seeds.
- Read-only host binds remain shared external views rather than snapshots.
- Uncommon OCI metadata such as device nodes, privileged security xattrs, and
  executable file capabilities needs broader rootless virtualization coverage.
- One image reference currently records one selected platform.
- Image signatures, attestations, and content-trust policy are not implemented.
- Live branches cannot be migrated to another Sentry or host, and TTOS does not
  persist them as checkpoint images.
- The unsafe no-host-isolation mode is only appropriate for trusted workloads.

The authoritative runtime decision is always `ttos fork --check`. The full
supported/rejected resource matrix and open performance work are documented in
the [audit](g3doc/proposals/timetravel_ttos_audit.md).

## Testing

Run the focused TTOS command, networking, memory, and tmpfs tests through the
Docker build wrapper:

```sh
make test TARGETS="//runsc/ttos/cmd:cmd_test //runsc/boot:boot_test \
  //pkg/sentry/mm:mm_test //pkg/sentry/fsimpl/tmpfs:tmpfs_test"
```

The public-image end-to-end matrix exercises BusyBox, Alpine, Python, nginx,
Redis, image lifecycle, exec, logs, filesystems, volumes, pause/resume,
recursive fork, daemon recovery, and rootless networking:

```sh
bazel build //runsc:runsc //runsc/ttos:ttos //runsc/ttosd:ttosd \
  //runsc/ttos/test:e2e_image_matrix

TTOS="$PWD/bazel-bin/runsc/ttos/ttos_/ttos" \
RUNSC="$PWD/bazel-bin/runsc/runsc_/runsc" \
  "$PWD/runsc/ttos/test/e2e_image_matrix.sh"
```

See the [TTOS test guide](runsc/ttos/test/README.md) for the real-PTY suite,
hermetic registry fixture, Docker-main compatibility test, retained test state,
and the trusted no-user-namespace variant.

## Documentation

- [End-user guide](g3doc/user_guide/timetravelos.md)
- [Performance and compatibility audit](g3doc/proposals/timetravel_ttos_audit.md)
- [Live-fork design](g3doc/proposals/timetravel_live_fork.md)
- [Product and CLI roadmap](g3doc/proposals/timetravel_ttos_cli.md)
- [End-to-end test guide](runsc/ttos/test/README.md)
- [Upstream gVisor documentation](https://gvisor.dev/docs/)

## Relationship to gVisor

TimeTravelOS would not exist without gVisor. The fork retains gVisor's
userspace Linux kernel, OCI runtime, filesystem and networking implementations,
build system, tests, documentation, and copyright notices, then adds the
TimeTravelOS live-fork kernel paths and local container-management product.

Changes intended specifically for TTOS should be developed and reported against
the [TimeTravelOS repository](https://github.com/gzz2000/TimeTravelOS).
General gVisor usage, architecture, and upstream contribution information is
available from the [gVisor project](https://github.com/google/gvisor) and
[gvisor.dev](https://gvisor.dev/).

This repository is licensed under the [Apache License 2.0](LICENSE).
