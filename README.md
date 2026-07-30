# TimeTravelContainer

<img src="assets/ttc-logo.png" alt="TimeTravelContainer logo" width="180">

TimeTravelContainer (TTC) is a lightweight, rootless Linux container manager
that can **fork an entire running container** into independent branches.

Each branch receives its own processes, memory view, writable filesystem,
internal networking, and supported kernel resources. Memory and files remain
shared through copy-on-write until either branch modifies them, so a fork does
not copy all container RAM or create a checkpoint image.

TTC is forked from [Google's gVisor](https://github.com/google/gvisor) and
builds on its userspace Linux runtime.

Compared with microVM-based solutions such as
[forkd](https://github.com/deeplethe/forkd) and
[AgentENV](https://github.com/kvcache-ai/AgentENV), TTC does not require
hardware virtualization support such as KVM, root privileges, or special
kernel modules. It supports Linux 3.17+ and can run nested inside Docker
containers and other virtual machines. TTC is distributed as just two static
Linux binaries, `ttc` and `ttcd`, with no Docker or containerd daemon,
system-wide installation, or CRIU setup required.

On Linux 3.17 through 4.14.76, `ttcd` automatically disables host GSO at
startup and warns that large-payload network performance will be reduced.

```text
demo@main
├── demo@test-a
│   └── demo@test-a-2
└── demo@test-b
```

Install the latest static release:

```sh
curl -fsSL https://raw.githubusercontent.com/gzz2000/TimeTravelOS/master/install.sh | sh
```

## Why TimeTravelContainer?

Containers are usually treated as disposable machines: when a workload needs
another possible future, it starts another container and repeats everything
that led to the present. TTC makes the present itself forkable.

1. Start an unmodified Docker or OCI image.
2. Initialize its applications, tools, services, and data once.
3. Fork the whole running environment at the decision point.
4. Explore a different action, test, or mutation in each branch.
5. Keep the useful outcomes and discard the rest.

This model is particularly useful for agentic AI. A coding agent may spend
minutes cloning a repository, installing dependencies, indexing source,
starting databases, and building a project before it can attempt a change.
With TTC, that warmed environment can become the root of a branch tree:

- **Parallel exploration:** give coding agents, planners, or tree-search nodes
  the same live starting state, then explore different implementations or
  action sequences in independent branches.
- **Speculation and rollback:** preserve warm milestones and fork before risky
  tool calls, upgrades, or migrations; keep promising outcomes and discard the
  rest without replaying the task from the beginning.
- **Evaluation and training:** run candidate agents or policies from an
  identical prepared state. Branching training rollouts from a shared prefix
  reduces environment variance, enables direct counterfactual comparison, and
  can support more stable credit assignment.

The same primitive accelerates integration testing, state-space exploration,
fault injection, fuzzing, interactive debugging, and any workflow that needs
many independent variations of one live starting state.

At the instant of a fork, guest-visible details such as process IDs, threads,
open-file offsets, clocks, signals, and internal connections are preserved in
both branches. Fork cost is driven mainly by the number of processes, mappings,
and kernel objects rather than the amount of resident application memory.

## Features

### Live container branching

- Pause and resume a running branch.
- Fork a branch into a running or initially paused child.
- Fork children again to build a tree of experiments.
- Save one live or paused branch to a checkpoint file and restore it later as
  an independent container.
- Preserve process trees, threads, memory, credentials, timers, signals,
  internal pipes, internal sockets, and supported IPC resources.
- Share memory page-by-page until one branch writes.
- Share disk-backed files through filesystem copy-on-write.
- Show ancestry and branch state with `ttc ls --tree`.
- Validate a fork while the source is paused. If a resource cannot be copied
  safely, TTC resumes the source and reports the reason without creating a
  partial child.

### Familiar container workflow

The `ttc` command provides a Docker-like interface:

- Images: `pull`, `images`, `image inspect`, and `rmi`
- Lifecycle: `run`, `create`, `start`, `stop`, `restart`, `wait`, `kill`,
  and `rm`
- Interaction: `exec`, `attach`, `logs`, and `cp`
- Inspection: `inspect`, `ps`, `top`, and `events`
- Branching: `fork`, `pause`, `resume`, `checkpoint`, `restore`, `branch rm`,
  and `ls --tree`
- Storage: `volume create`, `volume ls`, `volume inspect`, and `volume rm`
- Shell completion for Bash, Zsh, and Fish

Common Docker-style run options are supported, including environment
variables, labels, entrypoints, working directories, users, capabilities,
ulimits, read-only roots, tmpfs, volumes, host binds, DNS settings, network
mode, published ports, and CPU or memory limits.

TTC is self-contained: it pulls images directly from OCI and Docker registries
and does not require a Docker or containerd daemon.

### Rootless images and containers

- Pull and fully prepare images using familiar names such as `alpine:3.20`.
- Select image tags, digests, and multi-architecture variants.
- Use Docker configuration files and credential helpers for authenticated
  registries.
- See human-readable download, reuse, and extraction progress.
- Run images containing arbitrary users without subordinate UID/GID ranges or
  `newuidmap`/`newgidmap`.
- Use image workflows that rely on `sudo`, `su`, or SUID/SGID programs; their
  credentials exist only inside TTC's virtual Linux environment and do not
  grant host root.

### Persistent copy-on-write storage

Every writable container family has a persistent disk-backed layer over its
immutable image. A fork gives the child its own view of that layer without
eagerly copying every file. On the first write to a shared file, TTC creates a
reflink and lets the host filesystem copy only the modified extents.

TTC also supports:

- Managed persistent volumes
- Container-local tmpfs mounts
- `/proc`, restricted read-only `/sys`, `/dev`, `/dev/shm`, and private
  terminal mounts
- Read-only views of host files and directories
- Persistent, branch-local writable views over host paths
- Detached copies of host files and directories
- File copying to or from running, paused, and fully stopped containers

```sh
ttc volume create cache
ttc run -d --name app -v cache:/var/cache/app \
  alpine:3.20 sleep 600
ttc volume inspect cache
```

Host paths use familiar Docker-style syntax with TTC isolation semantics:

```sh
# A live, read-only view of the host path.
ttc run --rm -v /host/input:/input:ro alpine:3.20 cat /input/file

# A persistent private writable view. Guest writes never modify /host/work.
ttc run -d --name work -v /host/work:/work alpine:3.20 sleep 600

# Copy once, then run without any dependency on /host/template.
ttc run -d --name imported -v /host/template:/work:copy \
  alpine:3.20 sleep 600
```

For an ordinary writable bind, the host path remains a live read-only lower
layer: host changes to files that the container has not copied up may become
visible. Use `:copy` when the container must see a fixed, fully private initial
copy. In both writable forms, stop/start preserves changes and live fork gives
each branch an independent copy-on-write view.

### Rootless virtual networking

`--network ttc` gives the container a virtual Ethernet interface managed
entirely in userspace. It does not require a TAP device, host bridge, CNI,
iptables, or `CAP_NET_ADMIN`.

- `--nat` enables outbound IPv4 TCP and UDP access.
- `-p` and `--publish` expose container TCP ports on the host.
- `--ip` and `--mac-address` customize the virtual interface.
- `ttc port publish`, `port ls`, and `port unpublish` update forwarding for an
  existing branch without restarting it.
- Internal loopback and virtual-network connections are copied during a fork.
- The source keeps its external connections. The child starts with those
  external connections disconnected and receives no published host ports until
  ports are assigned to it.

The default network mode is an isolated, loopback-only network.
`--network host` is available for ordinary container execution but cannot be
live-forked.

### Interactive use and recovery

- Foreground, detached, stdin-attached, and `-it` containers
- Interactive and detached `exec` sessions
- Detach and later reattach with terminal resize and job-control support
- Client `TERM` propagation
- Persistent timestamped logs
- A per-user `ttcd` service that owns containers, ports, logs, and events
- Crash reconciliation and idempotent management requests
- A `--direct` recovery mode for bypassing the daemon when necessary

## Security and intended use

TTC is currently intended for trusted development, integration testing,
state-space exploration, and similar workloads. It has not yet undergone the
security review required to serve as a production boundary for hostile or
multi-tenant workloads.

Normal rootless mode retains gVisor's userspace-kernel boundary and host
isolation layers. TTC changes must not create new host escapes or unintended
privilege-escalation paths, and security-relevant relaxations must remain
explicit and fail closed.

Branches in the same container family share one runtime process. They are
independent experiment states, but they are not security or failure boundaries
from one another.

`--unsafe-no-host-isolation` is an explicit compatibility mode for trusted
workloads on machines that prohibit unprivileged user namespaces. It removes
important host defense-in-depth and should never be used for an untrusted
image.

See the
[security, performance, and compatibility audit](g3doc/proposals/timetravel_ttc_audit.md)
for the detailed threat model and current audit results.

## Requirements

TTC currently targets Linux on x86-64 and ARM64.

### User namespaces

Normal rootless mode requires an unprivileged user namespace:

```sh
unshare -Ur /bin/true
```

No subordinate IDs or `uidmap` helpers are required. `ttc daemon start`
performs its own preflight and reports a diagnostic if the host kernel or an
LSM policy prevents rootless operation.

If user namespaces cannot be enabled and every workload is trusted:

```sh
ttc daemon start --unsafe-no-host-isolation
```

The daemon uses one isolation mode for its entire lifetime. Stop it before
changing modes.

### Reflink storage

By default, the TTC data root must reside on a filesystem with working
`FICLONE` support, such as a suitably configured XFS or Btrfs filesystem.
Daemon status reports the selected storage mode:

```sh
ttc daemon status
```

For functional testing on a filesystem without reflinks, explicitly allow
whole-file copying:

```sh
ttc daemon start --allow-slow-volume-copy
```

This fallback preserves behavior but can make the first write to a shared
large file substantially slower.

### Resource limits

Hard CPU and memory limits require a writable delegated cgroup v2 subtree.
TTC reports an error rather than silently ignoring an unavailable hard limit.
Container process-count limits do not require host cgroup delegation.

## Build and install

### Docker-based build

Docker is needed only for this build method. It is not needed to run TTC.

From the repository root:

```sh
mkdir -p bin
make copy TARGETS="runsc/ttc runsc/ttcd" \
  DESTINATION="$PWD/bin"
```

This produces:

```text
bin/ttc
bin/ttcd
```

Keep the two binaries together. `ttcd` contains both the TTC control service
and the userspace container runtime. You can run them directly from `bin`, add
that directory to `PATH`, or install both in a per-user directory:

```sh
mkdir -p "$HOME/.local/bin"
install -m 0755 bin/ttc bin/ttcd "$HOME/.local/bin/"
```

### Direct Bazel build

Install the native dependencies listed in
[`images/default/Dockerfile`](images/default/Dockerfile), use the Bazel version
from [`.bazelversion`](.bazelversion), and run:

```sh
bazel build //runsc/ttc:ttc //runsc/ttcd:ttcd
```

The binaries are written beneath `bazel-bin/runsc/`.

No privileged system-wide installation is required. See the [complete user
guide](g3doc/user_guide/timetravelcontainer.md) for native build dependencies
and installation layouts.

## Quick start

The following examples assume `ttc` and `ttcd` are available in `PATH`.
Prefix commands with `./bin/` if you are running directly from a build.

Start the per-user daemon:

```sh
ttc daemon start
ttc daemon status
```

Pull and run an image:

```sh
ttc pull alpine:3.20
ttc run --rm alpine:3.20 uname -a
```

Create an interactive background container:

```sh
ttc run -dit --name demo alpine:3.20 sh
ttc exec -it demo sh
```

Fork the running container:

```sh
ttc fork demo demo@test-a
ttc fork --paused demo demo@test-b
ttc ls --tree
ttc resume demo@test-b
```

A branch selector has the form `CONTAINER@BRANCH`. Omitting `@BRANCH` selects
`main`.

Run different work in each branch:

```sh
ttc exec demo@test-a sh -c 'echo child-a >/tmp/result'
ttc exec demo@test-b sh -c 'echo child-b >/tmp/result'

ttc exec demo@test-a cat /tmp/result
ttc exec demo@test-b cat /tmp/result
```

The two branches now have different memory, process, and filesystem states
while retaining the same initial container history.

Save a branch to a durable checkpoint and restore it as a new, unrelated
container:

```sh
ttc checkpoint demo@test-a demo-test-a.ttc
ttc restore --name recovered demo-test-a.ttc
```

Pause and resume a branch:

```sh
ttc pause demo@test-a
ttc resume demo@test-a
```

Copy files between the host and a running, paused, or fully stopped branch:

```sh
ttc cp ./test-input demo@test-a:/tmp/input
ttc cp demo@test-a:/tmp/result ./result-a
```

Attach to a branch's init console and detach with Ctrl-P, Ctrl-Q:

```sh
ttc attach -i demo@test-b
```

Stop and remove the family:

```sh
ttc stop demo
ttc rm --force demo
```

Stop the daemon when it is no longer needed:

```sh
ttc daemon stop
```

## SDKs and agent integration

Applications can connect directly to `ttcd` without invoking the `ttc` CLI.
The `ttcx` bindings expose TTC commands as structured results with separate
stdout, stderr, and exit status:

- [Python SDK](sdk/python/README.md)
- [TypeScript SDK](sdk/typescript/README.md)
- [Rust SDK](sdk/rust/README.md)
- [MCP server for coding agents and MCP clients](sdk/mcp/README.md)

All bindings include lifecycle conveniences such as run, exec, fork, pause,
resume, stop, and remove, plus a raw command method covering the complete
non-interactive TTC interface. See the [local API protocol](sdk/PROTOCOL.md)
when implementing another language binding.

## Networking example

Start nginx with outbound access, a chosen virtual address, and a published
host port:

```sh
ttc run -d --name web \
  --network ttc --nat \
  --ip 10.23.4.15 \
  --mac-address 02:aa:bb:cc:dd:ee \
  -p 127.0.0.1:8080:80 \
  nginx:1.27-alpine

curl http://127.0.0.1:8080/
```

Publish a different host port for a fork:

```sh
ttc fork web web@test
ttc port publish web@test 127.0.0.1:8081:80
ttc port ls web
```

## How live fork works

TTC groups a container and all of its descendants into a container family.
When `ttc fork` is requested, TTC:

1. Pauses the selected source branch.
2. Checks whether its current resources can be copied safely.
3. Creates a child view of its process, memory, filesystem, and internal
   network state.
4. Configures copy-on-write sharing.
5. Resumes the source and, unless requested otherwise, the child.

Application memory is shared at page granularity. Writable disk files are
shared through reflinks and become physically distinct as filesystem blocks
are modified. The operation does not use a virtual machine or write a
checkpoint containing all container memory.

For implementation details, see the
[live-fork design](g3doc/proposals/timetravel_live_fork.md).

Durable checkpointing is a separate, explicitly requested operation. It
captures one selected branch's process, memory, filesystem, and supported
runtime state in a file. The immutable OCI image is identified by its exact
digest instead of being copied into the checkpoint. Restoring creates a new
container family with no ancestry or mutable storage shared with the source.

## Current limitations

- TTC is not yet a production security boundary for hostile workloads.
- Branches in one family share a runtime process, so a runtime failure can
  affect the whole family.
- Fork duration depends on the number of processes, mappings, and resources.
  It is not universally constant time.
- Some resources connected directly to the host or another container cannot be
  duplicated safely. TTC may reject the fork and report the resource that
  blocked it.
- The source keeps established external network connections during a fork.
  The child starts with external connections disconnected and without
  published host ports.
- Virtual networking and published ports currently support IPv4; published
  ports currently support TCP.
- A stopped child branch retains its filesystem for `ttc cp`, but its former
  live process and memory state cannot be started again. Restarting a container
  starts its main branch from its persistent configuration and filesystem.
- Read-only host binds remain shared live views during a fork. Ordinary
  writable host mounts also retain a live read-only host lower, while each
  branch has an independent persistent upper. A `:copy` mount is fully detached
  from its source after creation. Durable checkpoints capture host-backed
  content into the checkpoint.
- A managed volume cannot be attached writable to unrelated container families
  at the same time.
- TTC does not yet implement the Docker Engine API, Compose, Swarm, general
  device or accelerator passthrough, or Docker named networks.
- One image reference currently records one selected platform. Image signature,
  attestation, and content-trust policy are not yet implemented.
- Live branches require a durable checkpoint to move to another runtime
  process or host.
- Checkpoint restore requires the exact recorded image digest to be available
  in the destination TTC image store and currently requires a compatible TTC
  build.
- Creating a durable checkpoint requires the selected branch to be eligible
  for live fork, because TTC first makes a short-lived private branch before
  writing the archive.
- `--unsafe-no-host-isolation` is suitable only for trusted workloads.

The authoritative compatibility check is the `ttc fork` operation itself. It
pauses and validates the source atomically, then either creates the child or
resumes the unchanged source and returns a nonzero status.

The complete resource matrix and performance measurements are available in
the [audit](g3doc/proposals/timetravel_ttc_audit.md).

## Documentation

- [End-user guide](g3doc/user_guide/timetravelcontainer.md)
- [Performance and compatibility audit](g3doc/proposals/timetravel_ttc_audit.md)
- [Live-fork design](g3doc/proposals/timetravel_live_fork.md)
- [Disk-backed storage design](g3doc/proposals/ttc_disk_backed_volumes.md)
- [Product and CLI roadmap](g3doc/proposals/timetravel_ttc_cli.md)
- [End-to-end test guide](runsc/ttc/test/README.md)
- [Upstream gVisor documentation](https://gvisor.dev/docs/)

## Development and testing

Run the focused TTC tests through the Docker build wrapper:

```sh
make test TARGETS="//runsc/ttc/cmd:cmd_test //runsc/boot:boot_test \
  //pkg/sentry/mm:mm_test //pkg/sentry/fsimpl/tmpfs:tmpfs_test \
  //pkg/ttcvolume:ttcvolume_test"
```

The public-image end-to-end matrix covers BusyBox, Alpine, Python, nginx,
Redis, image lifecycle, exec, logs, filesystems, volumes, pause/resume,
recursive fork, daemon recovery, and rootless networking. See the
[TTC test guide](runsc/ttc/test/README.md) for commands and additional suites.

## License

This repository is licensed under the [Apache License 2.0](LICENSE).

## Relationship to gVisor

TimeTravelContainer would not exist without gVisor. TTC retains gVisor's
userspace Linux kernel, OCI runtime, filesystem and networking implementations,
build system, tests, documentation, and copyright notices, and extends them
with live container branching and the TTC container-management interface.

Changes specific to TTC should be developed and reported against the
[TimeTravelContainer repository](https://github.com/gzz2000/TimeTravelContainer).
General gVisor usage, architecture, security reporting, and upstream
contribution information is available from the
[gVisor project](https://github.com/google/gvisor) and
[gvisor.dev](https://gvisor.dev/).

Google and the gVisor project are not responsible for or affiliated with the
TimeTravelContainer-specific modifications.
