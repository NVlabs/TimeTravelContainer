# TimeTravelContainer user guide

TimeTravelContainer (TTC) is a rootless container runtime built on a modified
gVisor userspace kernel. In addition to Docker-style image and container
commands, TTC can pause, resume, dehydrate, rehydrate, and live-fork a running
container into independent branches. Active branches in one family share a
Sentry process on each node.

TTC is currently intended for trusted development and integration-test
workloads. It is not yet a general Docker replacement or a production security
boundary. Non-root initial and exec processes use Docker-shaped capability
defaults, and SUID/SGID elevation remains confined to the Sentry's virtual
credentials. Read the
[security, performance, and compatibility audit](../proposals/timetravel_ttc_audit.md)
before depending on live-fork or credential-isolation behavior.

## What is installed

TTC consists of two binaries:

* `ttc`, the user-facing command-line client; and
* `ttcd`, the per-user manager service, userspace container runtime, Sentry,
  and live-fork implementation.

Start `ttcd` explicitly with `ttc daemon start` before ordinary commands.
Docker, containerd, CRIU, KVM, and a containerd snapshotter are not required.
TTC talks directly to OCI registries and stores image content under its own
data root.

On Linux, the default `systrap` runtime needs no hardware virtualization. On a
host that exposes a usable `/dev/kvm`, `ttc daemon start --platform=kvm`
selects gVisor's KVM runtime for all containers owned by that daemon. On macOS,
the only runtime is `hvf`, which runs containers with Hypervisor.framework
without booting a Linux VM.

## Host requirements

TTC builds and runs on Linux x86-64 and ARM64, where it needs Linux 3.17 or
newer (4.14.77 or newer for full network performance), and on Apple silicon
Macs with macOS 15.0 or newer. Build with the Bazel version in the
repository's `.bazelversion` file (currently 8.3.1).

For a direct Debian or Ubuntu Bazel build, install the native and cross-build
dependencies used by gVisor's canonical build image. In particular, `ttcd`
requires Clang and the libbpf headers, while the generated VDSO and systrap
objects require both amd64 and arm64 compiler components:

```shell
sudo apt-get update
sudo apt-get install \
  build-essential crossbuild-essential-amd64 crossbuild-essential-arm64 \
  clang libbpf-dev libc6-dev-i386 pkg-config libffi-dev libssl-dev \
  python3 patch diffutils zip unzip
```

`libc6-dev-i386` is needed on x86-64 hosts and may be omitted on ARM64. See
`images/default/Dockerfile` for the canonical complete package list. A missing
`bpf/bpf_endian.h` means `libbpf-dev` is absent; a missing `cc1plus` during an
aarch64 VDSO build means the ARM64 cross-C++ package is incomplete. A macOS
build uses Xcode's compilers and SDK; Bazel fetches everything else, MoltenVK
among them.

The normal rootless mode requires unprivileged user namespaces. Check the host
before starting if desired:

```shell
unshare -Ur /bin/true
```

A one-entry UID/GID map is sufficient. `newuidmap`, `newgidmap`, and subordinate
UID/GID ranges are not required. If the command fails, enable unprivileged user
namespaces and check the host's AppArmor or other LSM policy.
`ttc daemon start` performs an equivalent disposable user/mount-namespace
probe automatically and refuses to start in rootless mode when the probe fails,
so an apparently healthy daemon cannot defer this error until container launch.
Directly launching `ttcd` performs the same preflight.

The daemon also probes the node-local volume object store under `TTC_ROOT`
(standalone) or `TTC_LOCAL_ROOT` (cluster mode) with `FICLONE`, or with APFS
`clonefile` on macOS. Working reflinks are used automatically. When they are
unavailable, TTC transparently copies a complete shared file on its first
write after a fork. `ttc daemon status` reports the selected
`volume_copy_mode`, data root, backing device, and filesystem type.

For trusted local testing only, or inside another sandbox that forbids user
namespaces, start the daemon with `--unsafe-no-host-isolation` to bypass the
host user-namespace and chroot setup:

```shell
ttc daemon start --unsafe-no-host-isolation
ttc run --rm busybox:1.36 echo hello
```

macOS has no user namespaces. There `ttcd` instead confines itself, and every
process it starts, with the macOS sandbox, Seatbelt: they keep all file and
network access, but can open no devices, USB ones among them, and reach only the
system services every process uses. The GPU renderers of `--gfx` containers,
which need the GPU, confine themselves to a narrower profile of their own
instead. `--unsafe-no-host-isolation` starts the daemon unconfined, as it must
inside another Seatbelt sandbox, where `ttcd` cannot apply its own.

Isolation mode is fixed for the lifetime of that daemon. All ordinary commands
inherit it, so do not repeat the flag on `run`, `exec`, or other operations.
`ttc daemon status` reports `isolation_mode`, which is `rootless`, `seatbelt`
on macOS, or `unsafe-no-host-isolation`, and `unsafe_no_host_isolation`. It
also reports `runtime_platform`, which defaults to `systrap`, and is `hvf` on
macOS. To use KVM on Linux instead:

```shell
test -r /dev/kvm && test -w /dev/kvm
ttc daemon start --platform=kvm
```

The KVM device is checked during daemon startup. Platform selection is
daemon-wide; `ttc run` and `ttc create` always use the daemon's native CPU
architecture and runtime platform. TTC does not emulate foreign CPU
architectures. Stop and explicitly restart the daemon to change isolation or
runtime-platform modes. Ordinary commands do not autostart the service; if it
is absent, they tell you to run `ttc daemon start`.

Unsafe mode retains Sentry-level guest isolation and branch semantics, but
removes host-side defense in depth. Do not use it for untrusted images.

The default `--cpu-profile=portable` uses an x86-64-v2-class instruction set on
amd64, suitable for moving workloads between compatible AMD and Intel nodes.
It hides AVX, AVX-512, and guest XSAVE. Systrap still uses the host's extended
register save format internally. For vector-heavy workloads, initialize the
root or cluster with `--cpu-profile=host`, or select feature names with
`--cpu-profile=explicit --cpu-features=...`. The profile is fixed at
initialization. Joining nodes must support every selected feature. Profiles
exposing XSAVE also require identical native XCR0 and XSAVE layouts: libraries
may cache their buffer size, and these instructions execute directly on the
CPU. AVX-512 register state is preserved by fork and hydration on compatible
hosts; AMX is not exposed. A systrap cluster also requires host CPUID faulting
(`ARCH_SET_CPUID`) on every member. Without it, an application can observe the
physical CPU instead of the immutable profile; TTC rejects that node at daemon
startup rather than allowing a later cross-CPU migration to corrupt userspace
XSAVE buffers. This restriction applies to distributed systrap execution, not
standalone TTC.

A cluster of identical CPUs may run without CPUID faulting if it is created
with `--cpu-profile=host --experimental-no-cpuid-faulting`: for example, one
cluster per CPU generation on hosts whose kernels predate AMD's CPUID faulting
(Linux 6.17), or whose CPUs lack it. The cluster records its first node's own
CPUID feature registers, and every node, with or without CPUID faulting, must
have exactly these, apart from the hypervisor bit and the speculation controls
that microcode reports. A node whose CPUID differs is refused at startup, with
the registers that differ. Applications on a node without CPUID faulting read
the host's CPUID rather than the profile, and may see and use FSGSBASE, which
systrap's syscall patching cannot coexist with; every node of such a cluster
therefore runs systrap with syscall patching disabled, which makes system calls
slower: a patchable system call took 4.1 µs instead of 1.1 µs on an AMD EPYC
7003 (Milan) host. Such a cluster runs systrap nodes only. The option is
chosen when the cluster is created: nodes that join may repeat it or leave it
out, and TTC builds that predate it refuse such a cluster.

## CPU topology and memory placement

`ttc daemon start --cpu-topology single|host` chooses the CPU topology that
containers see. It is independent of `--cpu-profile`, and fixed for the
lifetime of the daemon, or of a cluster, whose first daemon records it.

`single`, the default, shows one socket and one memory node, whose CPUs are
those the daemon may run on, numbered from 0. TTC promises nothing about where
a container's threads and memory are: it records their CPU affinities
(`sched_setaffinity`) and memory policies (`set_mempolicy`, `mbind`) but does
not apply them, and the host schedules the threads on any of the daemon's CPUs.
On a host whose CPUs span more than one NUMA node, `ttc daemon start` without
`--cpu-topology` warns:

```text
ttcd: warning: this daemon has access to multiple numa nodes, but --cpu-topology is not set explicitly. by default, numa and guest affinity are ignored. use --cpu-topology host or bind the daemon to a specific numa node with numactl or taskset.
```

To keep a `single` daemon's containers, and their memory, on one node, start
it bound to that node; its sandboxes inherit the binding, and see only its
CPUs:

```shell
numactl --cpunodebind=0 --membind=0 ttc daemon start
```

`host` shows the host's own CPUs, cores, packages and memory nodes, by their
host numbers, in `/proc/cpuinfo`, `/sys/devices/system/cpu`,
`/sys/devices/system/node` and, where the host has CPUID faulting, CPUID. A
thread may run on the CPUs the daemon may run on, and an affinity confines it
strictly to its CPUs: TTC never moves it elsewhere, even when they are busy and
others idle. `getcpu` and `sched_getcpu` report the host CPU and node it runs
on, and `/proc/stat` and `/proc/loadavg` report the host's CPU times and load,
so that an application can choose where to run its threads by what other work
leaves idle. Memory goes where Linux would put it: on the node of the CPU whose
thread first touches it, unless `set_mempolicy` or `mbind` place it otherwise
(`MPOL_PREFERRED`, `MPOL_BIND`, and `MPOL_INTERLEAVE` by huge page). A page
copied on write after a fork goes to the node of the thread that writes it,
while pages still shared stay where they are. Pages keep their nodes when a
branch is checkpointed, dehydrated, rehydrated or forked on its node. A branch
forked to another node pulls its pages as its threads touch them, and a page
goes to the node of the thread whose touch pulls it in, with the rest of its
2 MiB page or megabyte pulled with it; pages pulled before any thread touches
them go where the host puts them. `get_mempolicy` with
`MPOL_F_NODE|MPOL_F_ADDR` and `move_pages` report the node a page is on; TTC
does not migrate pages, so `MPOL_MF_MOVE` and `move_pages` move none. The host
topology needs the systrap platform.

In a cluster, the first daemon's choice holds for every node, and a node that
asks for the other is refused. Under `host`, every node must have the same
CPUs, cores, packages and nodes, by the same numbers, which in practice means
the same machine model, so that a branch forked or moved to another node finds
its affinities and memory policies meaning what they meant; a node that differs
is refused at startup. TTC builds that predate `--cpu-topology` refuse a
cluster created with `host`. A branch keeps the topology it was created with:
one saved under `single` and restored by a daemon showing `host` keeps
`single`, and one saved under `host` and restored by a daemon showing `single`,
or on a host numbered differently, keeps its CPU numbers, which then place
nothing.

Containers' memory is backed by huge pages where the host allows them: each
sandbox keeps its memory in a file on a private tmpfs mounted with
`huge=within_size`, so a host whose `shmem_enabled` is `never` still gives them
2 MiB pages. Anonymous memory is allocated in 2 MiB pages, and TTC treats
each as one page. A branch and its fork share it until one of them writes it,
and the write copies all 2 MiB into a new 2 MiB page, so the memory stays in
huge pages at every fork; a branch that holds a page alone writes it in place.
As in Linux, a 2 MiB page that is unmapped or discarded in part splits into 4
KiB pages; one that `mprotect` or `mremap` maps in pieces stays whole; and a
write to one that a branch maps only in part copies only the 4 KiB it writes.
Stacks, files and shared memory use 4 KiB pages, and a write copies only the
page it writes. A branch forked to another node, or rehydrated, pulls a 2 MiB
page whole and maps it with a huge page. `ttc stats` shows how much of the
containers' memory the host maps with huge pages.

## Compile

The repository's Docker-based build wrapper can copy both binaries in one
command:

```shell
mkdir -p bin
make copy TARGETS="runsc/ttc runsc/ttcd" DESTINATION="$PWD/bin"
```

This build path requires Docker only while compiling; Docker is not needed to
run TTC. It builds Linux binaries, also on a Mac. To build directly on the host
with Bazel, as macOS binaries require, from the gVisor repository root run:

```shell
bazel build //runsc/ttc:ttc //runsc/ttcd:ttcd
```

The binaries are written to:

```text
bazel-bin/runsc/ttc/ttc_/ttc
bazel-bin/runsc/ttcd/ttcd_/ttcd
```

You can use them directly without installation:

```shell
TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc"
"$TTC" --help
```

On macOS, `ttcd` starts only once signed with the Hypervisor entitlement, which
Bazel's output lacks: install and sign it as below before `ttc daemon start`.

## Optional per-user installation

Keep `ttc` and `ttcd` together so `daemon start` can locate and build-check its
matching service/runtime:

```shell
mkdir -p "$HOME/.local/bin"
install -m 0755 bazel-bin/runsc/ttc/ttc_/ttc \
  "$HOME/.local/bin/ttc"
install -m 0755 bazel-bin/runsc/ttcd/ttcd_/ttcd \
  "$HOME/.local/bin/ttcd"
```

On macOS, then sign the installed `ttcd` with the Hypervisor entitlement, which
it checks as it starts:

```shell
codesign --force --sign - --entitlements runsc/hvf.entitlements \
  "$HOME/.local/bin/ttcd"
```

`tools/ttc/install_macos.sh DIR` builds both binaries, installs them in `DIR`,
and signs `ttcd`.

Instead of building them, you can take both binaries from a daily
[release](https://gitlab-master.nvidia.com/avr/TimeTravelContainer/-/releases),
whose `ttc-linux-amd64.zip`, `ttc-linux-arm64.zip`, and `ttc-darwin-arm64.zip`
each hold a `ttc` and `ttcd` to unpack side by side. The macOS `ttcd` is
already signed, and its zip's `hvf.entitlements` signs it again if need be. On
macOS, run `xattr -d com.apple.quarantine` on a zip that a browser downloaded
before unpacking it.

Add `$HOME/.local/bin` to `PATH` if necessary, then verify the installation:

```shell
export TTC_ROOT="$HOME/.local/share/ttc"
ttc --version
ttc daemon start
ttc daemon status
ttc daemon stop
```

No privileged installation step is required. A system-wide installation may
place both files together under `/usr/local/bin`.

## State and runtime paths

`ttcd` refuses to start until `TTC_ROOT` names an explicit absolute path. In
standalone mode it contains everything of the daemon's: the image store,
container catalog, writable filesystems, and durable cold state, and also its
socket `ttcd.sock`, its log `ttcd.log`, the sockets of its sandboxes in `run/`,
its temporary files in `tmp/`, and runsc's state of its sandboxes in
`runtime/runsc/`. The root identifies the daemon, and `ttc` finds the daemon
through it:

```shell
export TTC_ROOT="$HOME/.local/share/ttc"
ttc daemon start
```

TTC keeps nothing of its own outside the root. What `run/`, `tmp/`, and
`runtime/runsc/` hold lives only as long as the processes it serves: a starting
daemon removes what outlived them, and all of it after the host restarted.

The root must be on a local filesystem: `ttcd` refuses to start on NFS,
Lustre, or another network filesystem, which does not keep the root's sockets,
locks, and process state to one host, unless `ttc daemon start
--allow-network-filesystem-root` insists. Unix socket paths are short, so the
root's path may be at most 76 bytes long on macOS and 80 on Linux, and that of
a `--control-only` gateway (see below) 13 bytes less.

Existing roots created before this format are deliberately not migrated; use
a fresh directory. `TTC_DATA_ROOT`, `TTC_RUNTIME_DIR`, `--data-root`, runtime
`--root` forwarding, and direct/embedded-daemon operation are unsupported.

In cluster mode, every node uses the same shared `TTC_ROOT` and its own fast,
node-local scratch directory:

```shell
export TTC_ROOT=/shared/ttc
export TTC_LOCAL_ROOT=/local/scratch/ttc-node-a
ttc daemon start --node node-a \
  --listen 0.0.0.0:7443 --advertise node-a.example:7443
```

`TTC_LOCAL_ROOT` is required on cluster nodes, must differ from `TTC_ROOT`, and
is exclusively locked to one daemon. Put it on a disk of the node itself, such
as `/tmp` or a local SSD, not in a home directory on NFS: a node prepares there
the root filesystem of each image it runs, and on NFS the first fork of an
image to a node spent 7 s doing so, against 0.3 s on a local disk; a node
begins as soon as it accepts the fork, while the source captures the branch.
It is the daemon's root, as `TTC_ROOT` is
a standalone daemon's: the daemon's socket, log, sockets of its sandboxes,
temporary files, and runtime state are there, and it must be on a local
filesystem likewise. Active filesystems, console/exec buffers, caches, and
publication staging remain local too. Sealed generations publish
asynchronously as immutable indexed packs; after a clean node stop, remaining
local files are disposable caches or staging residue.

The client locates the daemon by its root: `$TTC_LOCAL_ROOT/ttcd.sock` if
`TTC_LOCAL_ROOT` is set, as on a node of a cluster, and otherwise
`$TTC_ROOT/ttcd.sock`. `--daemon-socket` takes precedence for one invocation,
except for `daemon start`. The client uses nothing else of either root but
`ttcd.log`, which `daemon start` opens for the daemon. Help, shell completion,
and `ttc --version` are client-local and work without a daemon; the daemon
validates each command's arguments. Ordinary CLI operations require the client
and daemon build IDs to match exactly; `daemon stop` remains available as the
version-independent recovery operation. Programs use the same client; see
[Driving TTC from programs](#driving-ttc-from-programs).

## Images

Pull, load, save, list, inspect, and remove images with Docker-compatible
commands:

```shell
ttc pull alpine:3.20
ttc load -i images.tar
ttc save -o alpine.tar alpine:3.20
ttc images
ttc image inspect alpine:3.20
ttc rmi alpine:3.20
```

Short names use Docker's familiar-name rules, so `alpine:3.20` resolves to
`docker.io/library/alpine:3.20`. TTC reads authentication from
`$DOCKER_CONFIG/config.json` or `$HOME/.docker/config.json`, including Docker
credential helpers.

Pull policy is selected per `run` or `create`:

```shell
ttc run --pull missing IMAGE       # default: reuse, otherwise pull
ttc run --pull always IMAGE        # refresh the registry reference
ttc run --pull never IMAGE         # require a local image record
```

`ttc pull` contacts the registry even when all blobs are already cached and
materializes the immutable image root, leaving the image ready to run.
In a terminal, `ttc pull` maintains one live status row per layer while
downloading, reusing, and extracting content.
Redirected output remains line-oriented and contains no terminal control
sequences. `ttc pull -q` suppresses progress and prints only the digest.
`--pull missing` is the fastest offline path after the first pull.

`ttc load` reads a Docker-save or OCI archive from standard input by default;
`-i/--input` names a file on the client's host. It restores every tag in the
archive that matches the selected `--platform`. `ttc save` writes a Docker/OCI
archive to standard output by default; `-o/--output` names a file on the
client's host. Both commands also work as `ttc image load` and `ttc image
save`:

```shell
docker save tools:latest | ttc load
ttc save tools:latest | gzip > tools.tar.gz
gzip -dc tools.tar.gz | ttc load
```

## Run and inspect containers

Run a foreground one-shot command:

```shell
ttc run --rm alpine:3.20 uname -a
```

Start a persistent detached container:

```shell
ttc run -dit --name web alpine:3.20 sh
ttc ps
ttc inspect web
ttc logs web@main
```

Container IDs are 64 hexadecimal characters. A unique ID prefix or the assigned
name may be used as the container part of a selector. Branch-targeting commands
always require the branch suffix:

```text
web@main  selects branch main in family web
web@test  selects branch test in family web
```

Use `ttc ls --tree` for human-readable branch lineage and `ttc ls --json` for
the stable machine-readable form. Each non-root branch carries `parent`. Use
`ttc top web@test` for the processes in one branch.

## Live fork, pause, and resume

Live-fork eligibility is immutable for a container family. A normal `ttc run`
or `ttc create` produces an eligible family, and TTC rejects unsupported
resource creation rather than letting later workload activity revoke that
promise. Features intentionally outside TTC's branchable graph must be chosen
with `--waive-live-fork`; fork then rejects that family systematically. A fork
can still fail for an invalid source/destination, resource exhaustion, damaged
state, or an internal/runtime error.

Fork atomically pauses the source and checks internal invariants before
mutation. If an operational check fails, TTC removes the tentative child,
resumes a previously running source, writes the reason to stderr, and exits
nonzero. Create a running child branch directly:

```shell
ttc fork web@main test-a
```

Create several siblings with one source pause and validation pass:

```shell
ttc fork web@main test-a test-b test-c
```

Or create it paused so no child instruction executes before inspection:

```shell
ttc fork --paused web@main test-b
ttc resume web@test-b
```

The source is paused while its kernel metadata is cloned, then returned to its
previous state. Application memory, tmpfs files, and writable overlay state
begin shared and detach copy-on-write. Virtual PIDs and threads are reproduced
in the child; both branches may therefore observe PID 1 and the same initial
process tree.

Manage branches independently:

```shell
ttc pause web@test-a
ttc resume web@test-a
ttc quarantine web@test-a
ttc unquarantine web@test-a
ttc kill --signal TERM web@test-a
ttc wait web@test-a
ttc rm web@test-a
```

`rm` accepts `web@main`; `main` is an ordinary branch name and its removal does
not remove surviving peers. Use `-f` to remove a running or paused branch.

`ttc rm web` removes the whole family and requires all live branches to be
exited unless `--force` is used.

Quarantine is an independent per-branch flag, not a lifecycle state. It can be
changed while a branch is running, paused, dehydrated, or exited. A quarantined
branch retains its virtual NIC, internal sockets, and guest network
configuration, but TTC disconnects its runtime-managed external edge and does
not expose its published ports. The flag survives local and remote forks,
migration, dehydration, rehydration, stop/start, and checkpoint/restore.
Unquarantining recreates the external edge and published-port listeners;
external connections that could not remain valid while disconnected are reset.

Starting or restarting an exited branch does not require its peers to stop.
TTC starts only the selected branch in a short-lived paused bootstrap, imports
that never-executed branch into the family's existing Sentry, and then resumes
it. Running and paused peers remain in place throughout; TTC does not write or
reload their memory, so their size does not determine start latency.

A dehydrated branch is also a valid fork source. TTC clones its durable sparse
memory manifest and filesystem roots, starts the children, and leaves the
source process-free:

```shell
ttc fork web@archived candidate-a candidate-b
```

## Distributed live fork

Configured TTC nodes can place a live child on another host without first
copying all container memory or extracting its filesystem, and without
creating a checkpoint. The target starts from sparse memory and filesystem
manifests. It can temporarily fetch missing 1 MiB ranges from the transaction-
fenced live source while immutable copies are published to shared storage;
target writes always create target-local data. Once every required range is
local or durable, the target no longer depends on the source process.

The nodes must use the same shared TTC data root, run the exact same TTC build
and architecture, and be able to reach each other's cluster addresses. TTC's
cluster transport is plaintext because the cluster assumes an OpenMPI-style
trusted private network. The nodes authenticate one another with a token that
the cluster's first daemon makes in the shared root, readable only by the
root's owner: whoever can read the shared root administers the cluster. A
request without the token, from another host or from a container, is refused.

The shared root anchors the cluster, and every node starts alike, in any
order and at any time, as the jobs of a batch system do:

```shell
export TTC_ROOT=/shared/ttc
export TTC_LOCAL_ROOT=/local/scratch/ttc-node-a
ttc daemon start --node node-a \
  --listen 0.0.0.0:7443 --advertise node-a.example:7443 \
  --memory-target 64GiB
```

The cluster's authority is one node at a time, which keeps the authority's
state as a log in the shared root; the other nodes keep no copy, and send it
their requests. The first node to start takes the authority, and every later
one registers with the node that holds it, which the shared root names. Each daemon advances a
counter in the shared root every second; when the authority's has stood still
for 10 seconds, as when its node fails or its daemon stalls, another node takes
the authority over, from the log in the shared root, and the others follow
it. Branches keep running meanwhile. A node that starts with no local root,
as a new batch job does, takes over a cluster whose nodes are all gone. A
daemon that stops gives the authority up at once. The authority depends on the
shared filesystem: while it cannot write there, it holds the authority for 7
seconds at most, and then gives it up. A cluster whose authority an earlier
TTC kept in a Raft log is refused; start a new cluster.

A login node that should control the cluster but never execute containers can
run an outbound-only gateway:

```shell
export TTC_ROOT=/shared/ttc
export TTC_LOCAL_ROOT=/local/scratch/ttc-login
ttc daemon start --control-only
```

The gateway keeps its own files in `TTC_LOCAL_ROOT`, as any daemon of a cluster
does. It opens only its Unix socket there; it does not open or advertise a TCP
port, provide page data, or accept branch placement. It finds the compute nodes
in the shared root, so the login host needs outbound connectivity to them, but
compute nodes never initiate connections to it. Commands such as `run`, `exec`,
`fork`, `migrate`, `ps`, and `node ls` are tunneled to a compute node whose
counter advances and then routed by the normal cluster placement authority. The
shared `TTC_ROOT` marker is read to prevent accidentally controlling a different
repository, but the gateway does not write container data there. A failed stream
is never replayed ambiguously; the next command goes to another compute node.

While no compute node runs, the gateway starts a control holder, a daemon of
the login node that listens only on loopback: it takes the cluster's
authority, and the gateway relays to it. It answers what needs no compute
node, such as `ps`, `inspect`, `node ls`, and `rm` of dehydrated or exited
branches, and a command that needs one, such as `run` or `rehydrate`, fails
until a compute node starts. A compute node that starts takes the authority
back within a few seconds, and the holder stops. `ttc daemon stop` stops the
gateway, and its holder with it; use `ttc node stop` to retire a compute node.
Run one gateway per cluster.

HTTP control requests use the single TCP port named by `--listen` and
`--advertise`; page-transfer listeners are ephemeral per operation.
Cluster-wide paging, lease, transaction, and automatic-rebalance policy is
created once, by the cluster's first node, and adopted by later ones; a node
started with a different policy has the authority adopt it when it holds the
authority, but then fails to start while any node holds a live execution
lease. Node names, addresses, local memory targets, and capacity-provider hooks
remain node-local settings.

Node names default to their hostnames. Admission checks connectivity in both
directions between the candidate and every current member before it
registers. Use `--advertise` when the hostname does not resolve to an address that
the other nodes can reach. Firewalls must also allow the ephemeral page-data
connections used during remote fork and migration.

Only the authority's holder writes its log, so a fleet of any size commits as
fast as one node writes the shared filesystem, and the loss of any other node
stops nothing. Membership, leases, placements, distributed transactions,
publication generations, and durable root IDs are the authority's state. Memory pages, filesystem
extents, and immutable packs remain on the data plane and ordinary guest reads
and writes perform no consensus operation.

From node A, create one child on node B:

```shell
ttc fork --node node-b web@main remote-web
```

Create several siblings from one source pause and one metadata capture by
listing all destinations in a single command:

```shell
ttc fork --node node-b web@main candidate-a candidate-b candidate-c
```

Using one destination family lets compatible branches share one Sentry and
resident page cache on node B. Each child still has an independent filesystem
and process state. A child becomes source-independent when every volatile
memory and filesystem dependency is cached locally or promoted to shared
storage; it need not fault every range into RAM. `ttc inspect` and `ttc stats`
report the source node and remaining dependent bytes. Source shutdown drains
or promotes all dependencies before allowing the source daemon to exit.

Distributed live fork currently targets one remote node per command. Use a
durable checkpoint for offline transfer or for TTC installations that do not
share a repository and trust configuration.

### Elastic placement and node removal

A client attached to any cluster daemon can list membership and memory load:

```shell
ttc node ls
```

New containers are admitted on the compatible ready node with the lowest
normalized memory load, including outstanding placement reservations. A branch
that `start` or `rehydrate` places goes instead to a node where its family
already runs, if one accepts it: only there does it share memory with its
relatives again. `run`, `start`, `restore`, `rehydrate`, and `fork` accept an
optional `--node`; without it, TTC chooses a compatible ready node. Commands
naming an existing branch are routed to its current owner.

```shell
ttc run --node node-b -d --name build ubuntu:26.04 sleep infinity
ttc rehydrate --node node-c build@main
```

Commands naming `CONTAINER@BRANCH` are routed to the recorded owner. Move a
branch without changing its family, branch name, filesystem identity, or
process state with:

```shell
ttc migrate --node node-b web@candidate-a
```

Family-wide `ttc stop FAMILY`, `ttc dehydrate FAMILY`, and `ttc rm FAMILY`
also work when the family's branches are spread across nodes. TTC coordinates
the owner-local operations and prints one result for the original family.

Migration uses a live sparse cutover. It pauses and captures the selected
branch once, prepares a paused target that can demand-page memory and
materialize whole files on demand, atomically transfers fenced ownership, and
only then lets the target run. The source cannot execute after ownership
changes, while its sealed snapshot remains available until all target
dependencies are local or shared-durable. External connections are interrupted
according to the same policy as remote fork.

To remove a compute node, stop it from any TTC client. Exactly one policy is
chosen when it still owns active branches:

```shell
ttc node stop --migrate-all node-a
ttc node stop --dehydrate-all node-b
ttc node stop --stop-all node-c
```

Node stop first changes the node to `draining`, which rejects new runs, forks,
rehydrations, and incoming migrations. `--migrate-all` partitions and moves
running and paused branches to scheduler-selected ready nodes.
`--dehydrate-all` durably captures them as unplaced cold branches;
`--stop-all` terminates them and publishes their filesystems. With no policy,
the operation rejects while active branches remain. After dependencies and
publications are durable, `ttcd` stops automatically and retires its cluster
membership. If a step fails, the node stays cordoned and the same command can
be retried; the durable stop intent resumes after a daemon restart.

`ttc daemon stop --migrate-all` applies the same scheduler-selected evacuation
to the daemon of `TTC_LOCAL_ROOT`. It is rejected outside distributed mode.

If a daemon disappears during an individual migration, TTC reconciles the
fenced transaction automatically. A branch still owned by the source waits for
that exact source incarnation's lease to expire, then is restored and retried
there. Once ownership has moved it is never rolled back; the target restores
and commits the branch, including when only the destination response was lost.
Commands may be sent through any ready daemon, including the destination of a
drain.

`ttc node rebalance node-a` performs a smaller pressure-relief move using the
configured memory targets and branch CoW accounting. Starting `ttcd` with
`--cluster-auto-rebalance` runs this operation automatically when the node
crosses its target, with a 10% low-water mark and cooldown to avoid oscillation.

A node's memory, which its target bounds and `ttc node ls` shows, is what its
daemon and runtimes hold, on every platform: the daemon's resident memory, the
runtimes' own, and their guests' memory, each page once. A rebalance counts a
family's runtime as freed when the last of the family's branches leaves the
node, and as spent again on a node that runs none of them.

If a drain or pressure rebalance has no feasible destination, TTC writes one
durable request per source node to:

```text
$TTC_ROOT/cluster/capacity-requests/<node-id>.json
```

Configure exactly one delivery adapter on every daemon:

```shell
# Execute an absolute path directly, without a shell. The JSON request is stdin.
ttc daemon start ... --cluster-capacity-command /opt/ttc/bin/request-node

# Or POST the same JSON to a controller. HTTP 2xx acknowledges acceptance.
ttc daemon start ... --cluster-capacity-webhook https://scheduler.example/v1/ttc-capacity
```

The executable receives `TTC_CAPACITY_REQUEST_ID` and
`TTC_CAPACITY_SOURCE_NODE` in its environment. The webhook receives
`Content-Type: application/json` and an `Idempotency-Key` header. Provisioners
must treat the request ID idempotently: TTC retries failures and redelivers an
accepted request after five minutes if no matching capacity joins. Attempts,
acceptance, retry time, and the most recent error are recorded in the JSON and
survive daemon restart.

The provisioner must not remove or rewrite the outbox file. It starts a `ttcd`
with the requested architecture, repository, and usable memory. The new daemon
joins through normal dynamic membership; TTC removes the request only after a
compatible ready node publishes sufficient unreserved capacity. An outstanding
drain or automatic rebalance then resumes. Concurrent pressure events retain
one request ID per source node, preventing duplicate provisioning.

## Dehydrate and rehydrate

Dehydration hibernates a branch inside its existing family. It is useful when
the branch must remain available in the fork tree but should stop consuming
private application memory and active guest-kernel resources:

```shell
ttc dehydrate web@test-a
ttc ls --tree
ttc rehydrate web@test-a
```

A running branch is paused atomically before capture; a paused branch remains
paused. Rehydrating a branch returns it to the state it had before dehydration:
formerly running branches run, while formerly paused branches remain paused.
Its persistent root filesystem, volumes, ancestry, and copy-on-write sharing
with siblings are unchanged.

TTC tracks page versions across the entire family. Memory that must survive
without a resident owner is stored in immutable files of up to 1 MiB in the
TTC data root. A 2 MiB page is stored whole, its two halves each in a file of
its own, under its one identity and version, however little of it changed; 4
KiB pages are packed together. If an active peer still shares a page, TTC can
keep that page resident; when the peer first writes, TTC preserves the old
version for the dehydrated branch. Rehydration loads complete 1 MiB chunks on
demand, and a 2 MiB page whole, and installs their pages with their existing
CoW identities. Images that builds before 2 MiB pages wrote are read as 4 KiB
pages; those builds cannot read images that hold 2 MiB pages.

A standalone daemon defers that durable write while another branch of the
family is live on the node: the dehydrated branch's capture stays in the
family's Sentry, and JSON inspection reports its `storage_state` as
`resident`. TTC makes the capture durable when the family's last live branch
on the node is dehydrated, stopped, or removed, or exits; before the branch is
forked; at `ttc wait --condition durable`; and when the daemon stops with
`--dehydrate-all`. Until then, losing the runtime loses the capture as it
loses a running branch: the branch becomes exited, and starting it boots it
from its filesystem. A daemon of a cluster makes every dehydration durable at
once.

For a distributed child that still has a live source dependency, dehydration
first makes each required chunk durable in the shared repository. It then
publishes the branch manifest and releases the local runtime. Rehydration can
therefore proceed on any compatible node without contacting the original
source process.

Dehydration and checkpointing solve different problems:

| | Dehydration | Checkpoint |
|---|---|---|
| Result | The same branch remains in its family tree | A standalone archive file |
| Restore identity | Rehydrates the original branch | Creates a new unrelated family |
| Filesystem relationship | Remains in the same family and keeps CoW sharing | Imports private writable repositories |
| Portability | Internal to the same TTC data root and compatible build | Intended for explicit transfer, subject to image/build compatibility |
| Immutable image | Referenced from the existing family | Locked by exact recorded digest |

Dehydration persists task, kernel, internal-resource, tmpfs, and private-memory
state rather than merely writing anonymous pages. It releases the guest task
and kernel graph and every runtime process that exists only for the dehydrated
family. When all branches in a family are dehydrated, no Sentry or filesystem
transport process remains. A checkpoint is still the appropriate
representation when one portable file or an unrelated restored family is
required.

## Durable checkpoint and restore

`ttc fork` is the fast, in-memory operation for creating another branch in the
same live family. Use `ttc checkpoint` when the state must survive removal of
the source container or must be exported as one standalone file:

```shell
ttc checkpoint web@test-a web-test-a.ttc
ttc restore --name recovered web-test-a.ttc
```

Checkpoint accepts either a running or paused branch. TTC briefly creates a
private, paused in-memory fork, resumes the source, and serializes the private
snapshot. The potentially slow file write therefore does not keep a running
source paused. The private branch is removed once its state is saved and
does not appear in the family tree. Consequently, the selected family must be
live-fork eligible. If the private fork fails operationally, TTC leaves the
source unchanged and returns the same diagnostic used by `ttc fork`.

The checkpoint contains the selected branch's process tree, threads, memory,
supported kernel and internal-network state, writable root filesystem, managed
volumes, container configuration, and snapshots of its read-only host binds.
It does not contain the common immutable OCI image. The manifest records the
exact image digest and platform, and restore refuses to substitute different
image content. That digest must already be present in the restoring TTC
daemon's image store.

Restore creates a new container ID, name, writable repositories, virtual
network identity, and one `main` branch. It has no fork ancestry or mutable
storage relationship with the source family. A checkpoint made from a running
branch restores running; one made from a paused branch restores paused:

```shell
ttc restore --name paused-copy paused.ttc
ttc resume paused-copy@main
```

Published host ports are not recreated automatically. Add the desired
forwardings to the restored container with `ttc port publish`. Connections to
external hosts are disconnected in the snapshot, as they are for the child of
a live fork; internal sockets and pipes are retained when supported.

A checkpoint holds the branch's own memory, only the pages it uses however
much its family holds, as a dehydration of the branch; restore imports it and
rehydrates it as `main`. Checkpoint files are intended to be restored by a
compatible TTC build. They hold TTC's dehydration state, which is not a stable
interchange format between arbitrary TTC or upstream gVisor versions, and a
checkpoint made before TTC saved branches this way can be restored only by the
build that made it. Compression defaults to fast DEFLATE and may be disabled:

```shell
ttc checkpoint --compression none web@main raw-checkpoint.ttc
```

## Exec, attach, detach, and logs

Run a command in a selected branch:

```shell
ttc exec web@test-b cat /etc/os-release
ttc exec -it web@test-b sh
ttc exec -d web@test-b long-running-command
ttc exec web@test-b ls
```

Give a detached exec a durable name when a harness must reconnect after its
client process exits:

```shell
ttc exec --session build1 -dit web@test-b sh
ttc exec ls web@test-b
ttc attach -i --exec build1 web@test-b
ttc logs --exec build1 --tail 200 web@test-b
ttc exec wait web@test-b:build1
ttc exec kill --signal TERM web@test-b:build1
```

The session name, exit status, and bounded output log survive client
disconnects, live fork, and dehydration. TTC supplies the supervision; the
image does not need `tmux`, `screen`, or another process supervisor.

For REPL automation, inspect one named PTY synchronously or stream complete
JSON observations with heartbeat delivery:

```shell
ttc tty stat web@test-b --session build1 --json
ttc tty watch web@test-b --session build1 --json
```

The observation includes input-consumption and queue counters, blocked readers
and readiness waiters, termios and foreground-process-group generations,
output progress, flush detection, and the Sentry monotonic clock. TTC reports
observations only; the harness decides what constitutes a ready prompt. Watch
streams are driven by changes inside the Sentry, with burst coalescing and
heartbeats; they do not poll the sandbox or launch a helper runtime process.

For `-t`, TTC propagates the invoking client's `TERM` value into a new
container or exec process when the image/container and command do not already
define `TERM`; an unset client value falls back to `xterm`. This lets terminal
applications such as `tmux` select the correct terminfo entry while preserving
an explicitly configured container terminal type.

TTC enables guest SUID/SGID exec semantics by default for compatibility with
images that use helpers such as `sudo` or `su`. UID/GID elevation changes only
the Sentry's emulated guest credentials; it never changes the host UID of the
Sentry or gofer. An OCI process with `noNewPrivileges` still suppresses this
elevation.

Attach to the branch's init console:

```shell
ttc attach -i web@test-b
```

The default detach sequence is Ctrl-P followed by Ctrl-Q. Init-console,
managed-console, and CLI exec sessions use TTC-managed pipes or PTYs and do not
change the family's immutable live-fork eligibility. Output continues into
persistent logs:

```shell
ttc logs --follow --tail 100 web@test-b
```

## Copy files between the host and a branch

`ttc cp` uses Docker-compatible endpoint syntax. Exactly one operand must be a
container selector:

```shell
ttc cp ./fixtures web@main:/tmp/fixtures
ttc cp ./fixtures/. web@test-b:/tmp/fixtures
ttc cp web@test-b:/tmp/report.json ./report.json
ttc cp -a web@test-b:/var/lib/app ./saved-state
ttc cp -L web@test-b:/tmp/latest ./resolved-latest
```

The selector is `CONTAINER@BRANCH:PATH`; the branch component is mandatory.
A relative container path is interpreted from `/`. As with Docker, copying a
directory to an existing directory nests its basename, while copying it to a
nonexistent path creates exactly that path. A trailing `/.` copies only the
directory contents. Destination parents must already exist.

By default, copied entries use the destination side's user and group while
retaining modes and timestamps. `-a`/`--archive` preserves numeric source
UID/GID values as well. `-L`/`--follow-link` dereferences a symbolic link used
as the source operand. Regular files, directories, symbolic and hard links,
FIFOs/device nodes where permitted, and extended attributes are carried in the
archive stream.

Use `-` to stream an unmodified tar archive:

```shell
ttc cp web@test-b:/tmp/results - >results.tar
mkdir -p host-results
ttc cp web@test-b:/tmp/results/. host-results

ttc exec web@test-a mkdir -p /tmp/restored
ttc cp - web@test-a:/tmp/restored <results.tar
```

The import form requires an existing destination directory because the input
archive has no separate source operand. TTC runs its own donated copy helper in
the selected branch, so neither `tar` nor `cp` needs to exist in the image and
the binary stream is not routed through a guest shell.

Copy supports running, paused, dehydrated, and exited branches. For a paused branch,
TTC runs only its trusted copy helper while the branch's existing tasks remain
paused.
For a dehydrated branch, TTC mounts its persistent filesystem directly and
does not rehydrate or change the branch state.
For an exited branch, as for a dehydrated one, TTC launches a short-lived
helper Sentry whose process is the donated TTC copy helper, mounts the
immutable image lower plus the selected persistent `ttcvol` rootfs/volume
branch, and exits when the copy completes. It neither launches the image
entrypoint nor restores process memory. Other branches of the family may
remain live meanwhile. In a cluster, a dehydrated or exited branch whose state
has been published to the shared root can only be copied from; rehydrate or
start it to copy into it. A copy process temporarily owns host stream FDs;
`ttcd` therefore serializes copy operations with lifecycle mutations such as
fork, pause, stop, and branch removal.

## Commit, diff, and resource accounting

Commit a branch filesystem as a runnable local OCI image. Docker-style
`--change` options update supported image configuration fields. Existing OCI
labels round-trip; a `LABEL` change adds or replaces harness metadata such as a
bundled prompt:

```shell
ttc commit --change 'LABEL timetravel.usage=code-review' \
  web@test-b local/web-review:prepared
ttc image inspect local/web-review:prepared
```

As with `docker commit`, TTC pauses a running branch while capturing it unless
`--pause=false` is supplied. `-a/--author` records an author and `-m/--message`
records the history message. Repeat `-c/--change` to apply `CMD`, `ENTRYPOINT`,
`ENV`, `EXPOSE`, `LABEL`, `USER`, `VOLUME`, or `WORKDIR`. TTC requires the
destination repository argument so the committed image remains addressable in
its local image store.

`ttc diff` reads TTC's CoW metadata rather than walking the guest tree. Its
JSON form reports changed paths and change kinds relative to the branch's
lower layer:

```shell
ttc diff web@test-b
ttc diff --json web@test-b
```

`ttc stats` reports the selected family in one snapshot. For each branch it
separates private and CoW-shared resident memory, durable dehydration backing,
and persistent filesystem bytes, and reports non-double-counted family totals:

```shell
ttc stats web@test-b
ttc stats --json web@test-b
```

Resident memory counts mapped application pages in the Sentry; it deliberately
does not estimate the Sentry's Go heap or unmapped cache. It is counted from the
ranges of the Sentry's memory file that address spaces and in-memory files hold,
not page by page, so that it takes milliseconds however much memory the family
holds. Filesystem sharing is reported at TTC's immutable file-object boundary,
while durable memory backing is reported from still-present sparse chunk
objects.

JSON inspection reports storage separately from lifecycle. `storage_state` is
`local`, `resident`, `publishing`, `shared`, `materializing`, or `error`;
publication errors include retry details. This lets an orchestrator use a
branch as soon as local capture finishes while independently waiting for
shared durability:

```shell
ttc wait --condition durable web@test-b
```

## Filesystems and volumes

The ordinary container root is writable by default. Its immutable image lower
is covered by a family-owned, disk-backed `ttcvol` upper managed by the Sentry.
Every namespace, metadata, and regular-file mutation updates that repository
directly. Acknowledged data writes use normal host page-cache semantics, and
guest `fsync`/`fdatasync` propagate to backing files. TTC performs no rootfs
export on stop and no rootfs import on start.

Live fork seals the current metadata journal and publishes fresh source and
child layers without walking the filesystem. Both branches initially reference
the same opaque regular-file objects; the first write in either branch
reflinks the affected file, after which the backing filesystem performs
block/extent CoW. A read-only root
(`--read-only`) has no writable upper. Restart recreates only the selected
branch's process and kernel state; every branch retains its own filesystem
state.

Plain tmpfs mounts remain ephemeral across stop/start.

Arbitrary Sentry-owned tmpfs, alternate procfs, and restricted read-only sysfs
mounts are supported:

```shell
ttc run -d --name work \
  --tmpfs /scratch:size=64m,mode=1777 \
  --mount type=proc,target=/alt-proc,readonly \
  --mount type=sysfs,target=/alt-sys,readonly \
  alpine:3.20 sleep 600
```

Managed volumes are persistent disk-backed `ttcvol` filesystems:

```shell
ttc volume create database
ttc run -d --name db -v database:/var/lib/db alpine:3.20 sleep 600
ttc volume ls
ttc volume inspect database
```

`-v /work` creates an anonymous volume. The long form is
`--mount type=volume,source=database,target=/var/lib/db,readonly`; omit
`source` for an anonymous volume. A volume that a container's mount creates is
initialized from the image directory at its mount destination;
`ttc volume create` makes an empty one. Add `volume-nocopy` (or `nocopy` in
`-v` syntax) to start empty. Image `VOLUME` declarations create
anonymous volumes automatically when no explicit mount overrides them.

Writes go directly to opaque regular files beneath the TTC data root. On live
fork, TTC creates only two small metadata layers and branch pointers. The
first write to one shared regular file reflinks that file, after which the host
filesystem performs extent CoW. Stop/start reopens the same repository; there
is no tar export/import. `ttc rm -v` also removes anonymous volumes, while
named volumes remain until `ttc volume rm`.

The daemon uses `FICLONE` (on macOS, APFS `clonefile`) when available and
otherwise automatically selects whole-file copying on the first write after
each fork. See the
[snapshot-graph storage design](../proposals/ttc_snapshot_graph_refactor.md).

One named volume may have one writable container family at a time. TTC rejects
a second writable attachment because separate Sentry namespace caches are not
yet coherent; additional read-only attachments are allowed.

In a cluster, a named volume belongs to one node, as a Docker Swarm node's
local volumes do: it lives in that node's `TTC_LOCAL_ROOT`, and a branch uses
the volume of its name on the node it runs on, created on first use. The same
name on another node is another, separate volume, so branches placed on
different nodes do not see each other's writes to it. `ttc volume create`,
`inspect` and `rm` act on the node that runs the command, and `inspect` names
it; `ttc volume ls` lists the volumes of every node, with a `NODE` column.
Keep data that branches on several nodes must share on shared storage, and
bind it read-only.

A host path binds as Docker binds it: `-v HOST:PATH` alone, or with `:rw`, and
`--mount type=bind,...` without a mode, bind the host path itself, writable.
Its other modes are explicit: `:ro` (`readonly` in `--mount`), `:lazy_copy`
(`lazy_copy`), and `:copy` (`copy`).

A writable bind of the host path itself shares it with the host: the
container's writes reach the host path at once, and the host's changes show in
the container. Live forks could not give each branch a view of its own, so the
family must waive them:

```shell
ttc run -d --name build --waive-live-fork \
  -v "$PWD":/src \
  alpine:3.20 sleep 600
```

Without `--waive-live-fork`, `ttc run` and `ttc create` refuse such a bind,
and name the forkable modes. Such a family stops, starts, dehydrates and
rehydrates with its bind; as any family that waives live forks, it cannot be
forked or checkpointed. In rootless mode the invoking host user is the guest's
root, as a user namespace maps it; with `--unsafe-no-host-isolation` the gofer
shows that user's files as root's too, and creates the guest root's files as
that user, so that guest root writes such a bind in either.

Use `:ro` for a live read-only view of a host path:

```shell
ttc run --rm \
  --mount type=bind,source=/host/input,target=/input,readonly \
  alpine:3.20 cat /input/file
```

Such a bind is a shared external view, not a snapshot; later host changes may
become visible in every branch. Its source may be under an autofs map, as
`/home` is on many batch farms: ttcd looks each source up before it starts a
sandbox, so that the map mounts it, which a sandbox, in a mount namespace of
its own, cannot have done.

A guest may hold more of a bind's files open than the host lets TTC keep
descriptors for. Near that limit, TTC closes descriptors that nothing is using
and opens them again by path when they are next used. A file that something
else on the host removes, renames or replaces while its descriptor is closed
is then gone for the guest, which gets `ESTALE` for it, as an NFS client
would.

Use `:lazy_copy` (`cow` and `managed-cow` are its other names) for a writable
view that live forks keep: a persistent, branch-local writable layer over the
live host path.

```shell
ttc run -d --name edit \
  -v /host/base:/work:lazy_copy \
  alpine:3.20 sleep 600
```

The host path is always read-only from the guest's perspective. A first write
to a lower file copies that complete file into TTC-managed storage; later
writes and live forks use reflink-backed copy-on-write there. Host changes to
paths that have not been copied up may still become visible. Container
stop/start preserves the private layer, and each live-fork branch receives its
own persistent view.

Use `:copy` for a detached import:

```shell
ttc run -d --name edit \
  -v /host/base:/work:copy \
  alpine:3.20 sleep 600
```

TTC copies the source during container creation, opportunistically using
reflinks when the source and TTC data root permit it and otherwise streaming
the file contents. The source can then change or disappear without affecting
the container. Both files and directories are supported.

## Networking and resource limits

TTC exposes three network modes; macOS provides only `none` and `ttc`:

```shell
--network none       # loopback-only netstack; forkable and the default
--network ttc       # loopback plus rootless in-process virtual NIC; forkable
--network=host --waive-live-fork
                     # hostinet; explicitly and permanently non-forkable
```

`none` creates a userspace netstack with only canonical loopback. Processes in
one branch can use TCP and UDP over `127.0.0.1` and `::1`. At fork, TTC creates
a new netstack and loopback endpoint, clones protocol/socket state, and
preserves internal connections and unread data independently in both branches.

`ttc` adds a rootless, Sentry-managed IPv4 interface without TAP, veth, a host
network namespace, CNI, or elevated network capabilities. It is `eth0`, as a
Docker container's interface is, unless `--interface-name` names it otherwise:

```text
eth0       10.0.2.15/24           MAC 02:42:0a:00:02:0f
gateway     10.0.2.2
```

Set a stable guest-visible address, MAC, and interface name at create/run time
when a test needs a particular network identity:

```shell
ttc run -d --name appliance --network ttc --nat \
  --ip 192.0.2.44 --mac-address 02:aa:bb:cc:dd:ee --interface-name net0 \
  alpine:3.20 sleep 600
```

These values are persistent container configuration and survive start, restart,
and live fork. The address must be unicast IPv4; TTC uses a `/24` and reserves
the `.2` address in that subnet for its gateway, in addition to the network and
broadcast addresses. The MAC must be a unicast 48-bit address. The interface
name follows Linux's rules: 1 to 15 bytes, without `/`, `:`, or white space,
and not `lo`. `--ip`, `--mac-address`, and `--interface-name` are valid only
with `--network ttc`. If no custom value is given, the defaults above apply.
Containers created before TTC recorded the interface's name keep `ttc0`, the
name they were created with.

The interface is useful by itself for a stable container-visible NIC. Add
`--nat` to permit outbound IPv4 TCP and UDP through the Sentry's user-mode
proxy:

```shell
ttc run --rm --network ttc --nat alpine:3.20 \
  wget -qO- https://example.com/
```

Without `--nat`, non-container destinations are unreachable. TTC rewrites a
host loopback DNS resolver such as `127.0.0.53` to its corresponding
`10.0.2.53` host alias, so common systemd-resolved configurations work through
the proxy. NAT is currently IPv4-only and implements TCP plus short-lived UDP
mappings; ICMP forwarding is not provided.

With `--nat`, the container's subnet aliases the host's loopback: the gateway,
`10.0.2.2`, is the host's `127.0.0.1`, and `10.0.2.x` is `127.0.0.x`. A
container can therefore connect to every service that listens on the host's
loopback addresses, as any local process can. Run untrusted containers with
`--nat` only on hosts whose loopback services authenticate their clients.

Publish a container TCP port with Docker-style `-p`/`--publish`:

```shell
ttc run -d --name web --network ttc \
  -p 127.0.0.1:8080:80 nginx:1.27-alpine
curl http://127.0.0.1:8080/
```

Accepted forms are `HOST_PORT:CONTAINER_PORT` and
`HOST_IP:HOST_PORT:CONTAINER_PORT[/tcp]`. Host addresses and published ports are
currently IPv4/TCP only. A standalone daemon records the binding with the
container and recreates its listener whenever the branch runs again, including
after a daemon restart. In a cluster, a published listener belongs to the
daemon receiving the command and lasts until it is unpublished or that daemon
stops. It routes new connections to the branch's current owner, so the same
host address continues working after migration. Existing connections can be
interrupted during migration. This does not create an all-node guest network
or DNS service.

Add or remove a listener on any existing live branch without restarting it:

```shell
ttc port publish web@main 127.0.0.1:8081:80
ttc port publish web@child 127.0.0.1:8082:80
ttc port ls web@main
ttc port ls --json web@child
ttc port unpublish web@child 127.0.0.1:8082:80
```

`publish`, `unpublish`, and `port ls` all require a full branch selector.
In a cluster, run `ttc port ls --json` against the publishing daemon to
inspect its local listeners, and publish them again after that daemon
restarts. Publishing the same branch on another node creates an independent
ingress listener there.

Live-fork policy deliberately distinguishes internal from external flows:

* the source branch's established and connecting NAT TCP flows continue;
* the source branch retains its published-port listener and accepted flows;
* the child receives a fresh virtual link and proxy realm;
* every inherited child TCP flow to the host, internet, or a published-port
  proxy is reset, while internal loopback and `eth0` connections remain; and
* no host port is automatically published for a child branch.

Thus a fork is not rejected merely because an external TCP flow is open. Child
applications observe ordinary connection-reset behavior and may reconnect
outbound. A child is not published automatically, but `ttc port publish` can
assign it an independent host endpoint after the fork. UDP NAT mappings are
runtime-only and are re-created on later child datagrams.

Guest-created network namespaces, veth pairs, routes, iptables rules, conntrack
state, and internal sockets are part of the Sentry-owned graph and are cloned.
Eligible families currently reject nftables mutation because that state is not
yet serializable. Hostinet and other external/plugin stacks are not
live-forkable because their socket and link state belongs outside the Sentry;
TTC exposes host mode only when creation explicitly includes
`--waive-live-fork`.

Upstream runsc's `sandbox` mode imports a network namespace prepared by Docker,
CNI, or another manager. That manager owns the veth/bridge topology, address
assignment, NAT, and port rules. TTC's `ttc` mode is a separate in-process
design and does not require that host networking control plane. TTC still does
not implement the Docker Engine network API, named networks, or inter-family
virtual switching.

PID and Sentry-enforced limits work without host cgroups where possible.
Hard CPU and memory limits require a writable delegated cgroup-v2 subtree,
which macOS does not have; TTC fails before start when the requested
enforcement is unavailable.

## Daemon operation and recovery

Normal commands use the versioned per-user `ttcd` API. Start it once before
issuing ordinary commands:

```shell
ttc daemon start
ttc daemon status
ttc daemon stop
```

One daemon uses one runtime platform and one isolation mode for all of its
containers. On Linux, start it without an option for systrap and normal
rootless host isolation, use `ttc daemon start --platform=kvm` when `/dev/kvm`
is available, or use
`ttc daemon start --unsafe-no-host-isolation` for trusted workloads on a host
that cannot create an unprivileged user namespace. On macOS, the platform is
`hvf`, and the daemon confines itself with Seatbelt unless started with
`--unsafe-no-host-isolation`. An explicit start request
does not reconfigure an already-running daemon; stop it first if you want to
change modes. `daemon status` returns the active platform and isolation mode in
JSON.

Durable mutation records can be inspected with `ttc daemon requests` after an
interrupted operation.

`ttc daemon stop` refuses to exit while any branch is running, paused,
restarting, dehydrating, or rehydrating. Choose the desired policy explicitly:

```shell
ttc daemon stop --dehydrate-all  # hibernate every live branch, then exit
ttc daemon stop --stop-all       # stop every live branch, then exit
```

`--dehydrate-all` waits for all captures, including any required remote-page
drain, before stopping the daemon. If any transition fails, the daemon remains
available and reports the failure. Dehydrated families remain visible after a
later daemon start and may be rehydrated individually. In contrast, an
unexpected `ttcd` crash does not terminate live Sentry sandboxes; after
restarting the daemon, TTC reconciles those containers and their console/port
sidecars.

### Sharing a daemon with a group

A daemon serves only the user it runs as, its owner. To share it with the
members of a Unix group, such as a project's, start it with
`--unsafe-access-host-group GROUP`, where GROUP is a group name or ID that
the owner belongs to:

```shell
ttc daemon start --unsafe-access-host-group myproject
```

The daemon's root, `TTC_ROOT` or a cluster node's `TTC_LOCAL_ROOT`, becomes
searchable by the group (mode 0710) and its socket usable by it (0660);
nothing else in the root is. A member sets `TTC_ROOT` (and on a cluster node,
`TTC_LOCAL_ROOT`) as the owner does, or passes `--daemon-socket`, and uses
the daemon like the owner. Every container is the group's: a member can run,
exec into, fork, migrate, stop, and remove any of them. A cluster's shared
`TTC_ROOT` stays private: start each node and the gateway that members use
with the option.

The option is unsafe because a member's containers run as the owner. The
daemon limits what a member can make it do as the owner: it opens no host
path for a member, so a member cannot bind mount a host path, name an
environment, label, or process file that `ttcd` would read, or have `ttcd`
read or write a `cp`, checkpoint, restore, or image archive path. `ttc` reads
a member's environment and label files and copies a member's files itself,
with the member's own access. Only the owner can stop the daemon, stop or
forget a cluster node, resolve an interrupted request, and restore a
checkpoint, as a checkpoint's container may bind mount host paths. The daemon records
who asked for each request in `ttc daemon requests`, as the requester of its
events, and as `created_by` on each container. Run a shared daemon as a
project account rather than a person's where you can.

The option applies to the run it is given to: a daemon started without it
makes its root private again. Members of the group must be able to search
every directory above the root, about which the daemon warns.

## Driving TTC from programs

Programs, scripts, and coding agents drive TTC through the same `ttc` client
as people do: they run it as a subprocess, with `TTC_ROOT`, or a node's
`TTC_LOCAL_ROOT`, in its environment, and use the `ttc` of the daemon's
release. The client is thin, so each operation costs a process start of a few
milliseconds.

Read JSON rather than tables. `ls --json`, `ps --format json`, `inspect`,
`stats --json`, `diff --json`, `top --json`, `images --json`,
`volume ls --json`, `port ls --json`, `node ls --json`, `exec ls --json`,
`tty stat --json`, `daemon status`, and `daemon requests --json` print it, and
`events --json --follow` and `tty watch --json` stream one JSON object per
line. Later releases may add fields to these objects: ignore the ones you do
not know.

`ttc` exits with its command's status, and `ttc exec` with that of the process
it ran; diagnostics go to standard error. `exec -i` gives the process standard
input, `cp` reads or writes a tar stream for a `-` path, `load` reads an
archive from standard input, and `save` writes one to standard output. A
terminal session needs no terminal of the caller's: start it named and
detached, as `ttc exec -dt --session repl agent@main bash`, then observe it with
`ttc tty stat --json` or `tty watch`, and read its output with
`ttc logs --exec repl agent@main`.

A caller that loses a mutating command's answer, as when it crashes, can run
the command again under the same request ID, 32 hexadecimal digits: `ttcd`
runs a command of an ID once, and answers it again with its first result. If
the daemon was interrupted while it ran, `ttcd` refuses the ID instead;
`ttc daemon requests --json` and `ttc daemon request inspect ID` show what is
known of it.

```shell
ttc --request-id 0123456789abcdef0123456789abcdef fork agent@main candidate
```

For example, in Python:

```python
import json
import os
import subprocess

ROOT = os.path.expanduser("~/.local/share/ttc")


def ttc(*args, check=True):
    result = subprocess.run(["ttc", *args], capture_output=True, text=True,
                            env={**os.environ, "TTC_ROOT": ROOT})
    if check and result.returncode != 0:
        raise RuntimeError(f"ttc {' '.join(args)}: {result.stderr.strip()}")
    return result


ttc("run", "-d", "--name", "agent", "ubuntu:26.04", "sleep", "infinity")
ttc("fork", "agent@main", "candidate")
test = ttc("exec", "agent@candidate", "sh", "-c", "exit 3", check=False)
print("exit status", test.returncode)
print(json.loads(ttc("stats", "--json", "agent@candidate").stdout))
ttc("rm", "-f", "agent")
```

## Shell completion

Generate completion for Bash, Zsh, or Fish:

```shell
ttc completion bash >"$HOME/.local/share/bash-completion/completions/ttc"
ttc completion zsh >"${fpath[1]}/_ttc"
ttc completion fish >"$HOME/.config/fish/completions/ttc.fish"
```

Create the target directory first if it does not exist.

## Further verification

The public-image matrix exercises BusyBox, Alpine, Python, nginx, Redis, image
reuse, exec, logs, mounts, volumes, pause/resume, recursive fork, and daemon
restart:

```shell
bazel build //runsc/ttc:ttc //runsc/ttcd:ttcd \
  //runsc/ttc/test:e2e_image_matrix

TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
TTCD="$PWD/bazel-bin/runsc/ttcd/ttcd_/ttcd" \
  "$PWD/runsc/ttc/test/e2e_image_matrix.sh"
```

See the [test guide](../../runsc/ttc/test/README.md) for rootless and trusted
no-user-namespace variants.
