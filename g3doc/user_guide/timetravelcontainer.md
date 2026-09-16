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

The default `systrap` runtime needs no hardware virtualization. On a host that
exposes a usable `/dev/kvm`, `ttc daemon start --platform=kvm` selects gVisor's
KVM runtime for all containers owned by that daemon.

## Host requirements

TTC builds and runs on Linux x86-64 and ARM64. Build with the Bazel version in
the repository's `.bazelversion` file (currently 8.3.1).

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
aarch64 VDSO build means the ARM64 cross-C++ package is incomplete.

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
(standalone) or `TTC_LOCAL_ROOT` (cluster mode) with `FICLONE`. Working
reflinks are used automatically. When they are unavailable, TTC transparently
copies a complete shared file on its first write after a fork. `ttc daemon
status` reports the selected `volume_copy_mode`, data root, backing device, and
filesystem type.

For trusted local testing only, start the daemon with
`--unsafe-no-host-isolation` to bypass the host user-namespace and chroot
setup:

```shell
ttc daemon start --unsafe-no-host-isolation
ttc run --rm busybox:1.36 echo hello
```

Isolation mode is fixed for the lifetime of that daemon. All ordinary commands
inherit it, so do not repeat the flag on `run`, `exec`, or other operations.
`ttc daemon status` reports `isolation_mode` and
`unsafe_no_host_isolation`. It also reports `runtime_platform`, which defaults
to `systrap`. To use KVM instead:

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

## Compile

The repository's Docker-based build wrapper can copy both binaries in one
command:

```shell
mkdir -p bin
make copy TARGETS="runsc/ttc runsc/ttcd" DESTINATION="$PWD/bin"
```

This build path requires Docker only while compiling; Docker is not needed to
run TTC. To build directly on the host with Bazel, from the gVisor repository
root run:

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
standalone mode it contains the image store, container catalog, active runtime
state, writable filesystems, and durable cold state:

```shell
export TTC_ROOT="$HOME/.local/share/ttc"
ttc daemon start
```

Existing roots created before this format are deliberately not migrated; use
a fresh directory. `TTC_DATA_ROOT`, `TTC_RUNTIME_DIR`, `--data-root`, runtime
`--root` forwarding, and direct/embedded-daemon operation are unsupported.

In cluster mode, every node uses the same shared `TTC_ROOT` and its own fast,
node-local scratch directory:

```shell
export TTC_ROOT=/shared/ttc
export TTC_LOCAL_ROOT=/local/scratch/ttc-node-a
ttc daemon start --cluster-init --node node-a \
  --listen 0.0.0.0:7443 --advertise node-a.example:7443
```

`TTC_LOCAL_ROOT` is required in cluster mode, must differ from `TTC_ROOT`, and
is exclusively locked to one daemon. Active filesystems, runtime state,
console/exec buffers, caches, and publication staging remain local. Sealed
generations publish asynchronously as immutable indexed packs; after a clean
node stop, remaining local files are disposable caches or staging residue.

The client locates the daemon through `TTC_SOCKET`, then
`$XDG_RUNTIME_DIR/ttc/ttcd.sock`, then `/tmp/ttc-$UID/ttcd.sock`.
`--daemon-socket` takes precedence for one invocation. The client never needs
access to either storage root. Help, shell completion, argument validation, and
`ttc --version` are client-local and work without a daemon. Ordinary CLI
operations require the client and daemon build IDs to match exactly; `daemon
stop` remains available as the version-independent recovery operation. SDK and
MCP clients negotiate the public API and features instead of asserting a CLI
build ID.

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
`-i/--input` names a daemon-host file. It restores every tag in the archive
that matches the selected `--platform`. `ttc save` writes a Docker/OCI archive
to standard output by default; `-o/--output` names a daemon-host file. Both
commands also work as `ttc image load` and `ttc image save`:

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
trusted private network. Set the same `TTC_CLUSTER_TOKEN` on every node when a
shared bearer secret is useful; omit it only when every process that can reach
the cluster network is trusted as a cluster administrator.

Cluster authority is one Raft group. Its WAL and snapshots live in each node's
local root, never on the shared filesystem. Bootstrap node A with:

```shell
export TTC_ROOT=/shared/ttc
export TTC_LOCAL_ROOT=/local/scratch/ttc-node-a
export TTC_CLUSTER_TOKEN='replace-with-a-cluster-secret' # optional
ttc daemon start --cluster-init --node node-a \
  --listen 0.0.0.0:7443 --advertise node-a.example:7443 \
  --memory-target 64GiB
```

Start node B by naming any live seed:

```shell
export TTC_ROOT=/shared/ttc
export TTC_LOCAL_ROOT=/local/scratch/ttc-node-b
export TTC_CLUSTER_TOKEN='replace-with-a-cluster-secret' # same optional value
ttc daemon start --cluster-join node-a.example:7443 --node node-b \
  --listen 0.0.0.0:7443 --advertise node-b.example:7443 \
  --memory-target 64GiB
```

Only one seed is needed. Membership and leader discovery come from Raft after
admission. HTTP control requests and Raft streams share the single TCP port
named by `--listen` and `--advertise`; page-transfer listeners are ephemeral
per operation. Cluster-wide paging, lease,
transaction, and automatic-rebalance policy is created once by the bootstrap
node and adopted by joiners. Node names, addresses, local memory targets, and
capacity-provider hooks remain node-local settings.

Node names default to their hostnames. Admission checks connectivity in both
directions between the candidate and every current member before joining
Raft. Use `--advertise` when the hostname does not resolve to an address that
the other nodes can reach. Firewalls must also allow the ephemeral page-data
connections used during remote fork and migration.

The first three admitted members are voting replicas; further elastic compute
nodes remain non-voting replicas. Thus a large worker fleet does not enlarge
the commit quorum. Membership, leases, placements, distributed transactions,
publication generations, and durable root IDs are Raft state. Memory pages,
filesystem extents, and immutable packs remain on the data plane and ordinary
guest reads and writes perform no consensus operation.

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
normalized memory load, including outstanding placement reservations. `run`,
`start`, `restore`, `rehydrate`, and `fork` accept an optional `--node`; without
it, TTC chooses a compatible ready node. Commands naming an existing branch
are routed to its current owner.

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
to the daemon reached by `TTC_SOCKET`. It is rejected outside distributed mode.

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
without a resident owner is stored in immutable, content-addressed files of up
to 1 MiB in the TTC data root. Resident sharing and write-time CoW remain
4 KiB-granular. If an active peer still shares a page, TTC can keep that page
resident; when the peer first writes, TTC preserves the old version for the
dehydrated branch. Rehydration loads complete 1 MiB chunks on demand and
installs their constituent pages with their existing CoW identities.

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
source paused. The private branch is removed after the archive is complete and
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

Checkpoint files are intended to be restored by a compatible TTC build. They
wrap gVisor's versioned state data and are not a stable interchange format
between arbitrary TTC or upstream gVisor versions. Compression defaults to
fast DEFLATE and may be disabled:

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
For an exited branch, TTC launches a short-lived helper Sentry whose process is
the donated TTC copy helper, mounts the immutable image lower plus the selected
persistent `ttcvol` rootfs/volume branch, and exits when the copy completes.
It neither launches the image entrypoint nor restores process memory. TTC
rejects offline access to one exited child while another branch in the same
family remains live, because two Sentries would otherwise cache one repository
concurrently. A copy process temporarily owns host stream FDs; `ttcd` therefore
serializes copy operations with lifecycle mutations such as fork, pause, stop,
and branch removal.

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
does not estimate the Sentry's Go heap or unmapped cache. Filesystem sharing is
reported at TTC's immutable file-object boundary, while durable memory backing
is reported from still-present sparse chunk objects.

JSON inspection reports storage separately from lifecycle. `storage_state` is
`local`, `publishing`, `shared`, `materializing`, or `error`; publication
errors include retry details. This lets an orchestrator use a branch as soon
as local capture finishes while independently waiting for shared durability:

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
`source` for an anonymous volume. A newly created volume is initialized from
the image directory at its mount destination. Add `volume-nocopy` (or
`nocopy` in `-v` syntax) to start empty. Image `VOLUME` declarations create
anonymous volumes automatically when no explicit mount overrides them.

Writes go directly to opaque regular files beneath the TTC data root. On live
fork, TTC creates only two small metadata layers and branch pointers. The
first write to one shared regular file reflinks that file, after which the host
filesystem performs extent CoW. Stop/start reopens the same repository; there
is no tar export/import. `ttc rm -v` also removes anonymous volumes, while
named volumes remain until `ttc volume rm`.

The daemon uses `FICLONE` when available and otherwise automatically selects
whole-file copying on the first write after each fork. See the
[disk-backed volume design](../proposals/ttc_disk_backed_volumes.md).

One named volume may have one writable container family at a time. TTC rejects
a second writable attachment because separate Sentry namespace caches are not
yet coherent; additional read-only attachments are allowed.

Use `:ro` for a live read-only view of a host path:

```shell
ttc run --rm \
  --mount type=bind,source=/host/input,target=/input,readonly \
  alpine:3.20 cat /input/file
```

Such a bind is a shared external view, not a snapshot; later host changes may
become visible in every branch.

An ordinary writable bind creates a persistent, branch-local writable layer
over the live host path:

```shell
ttc run -d --name edit \
  -v /host/base:/work \
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

TTC exposes three network modes:

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
network namespace, CNI, or elevated network capabilities:

```text
ttc0       10.0.2.15/24           MAC 02:42:0a:00:02:0f
gateway     10.0.2.2
```

Set a stable guest-visible address and MAC at create/run time when a test needs
a particular network identity:

```shell
ttc run -d --name appliance --network ttc --nat \
  --ip 192.0.2.44 --mac-address 02:aa:bb:cc:dd:ee \
  alpine:3.20 sleep 600
```

Both values are persistent container configuration and survive start, restart,
and live fork. The address must be unicast IPv4; TTC uses a `/24` and reserves
the `.2` address in that subnet for its gateway, in addition to the network and
broadcast addresses. The MAC must be a unicast 48-bit address. `--ip` and
`--mac-address` are valid only with `--network ttc`. If no custom value is
given, the defaults above apply.

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

Publish a container TCP port with Docker-style `-p`/`--publish`:

```shell
ttc run -d --name web --network ttc \
  -p 127.0.0.1:8080:80 nginx:1.27-alpine
curl http://127.0.0.1:8080/
```

Accepted forms are `HOST_PORT:CONTAINER_PORT` and
`HOST_IP:HOST_PORT:CONTAINER_PORT[/tcp]`. Host addresses and published ports are
currently IPv4/TCP only. A published listener belongs to the daemon receiving
the command and lasts until it is unpublished or that daemon stops. In a
cluster, it routes new connections to the branch's current owner, so the same
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
Run `ttc port ls --json` against the publishing daemon to inspect its local
listeners. Publish them again after a daemon restart. Publishing the same
branch on another node creates an independent ingress listener there.

Live-fork policy deliberately distinguishes internal from external flows:

* the source branch's established and connecting NAT TCP flows continue;
* the source branch retains its published-port listener and accepted flows;
* the child receives a fresh virtual link and proxy realm;
* every inherited child TCP flow to the host, internet, or a published-port
  proxy is reset, while internal loopback and `ttc0` connections remain; and
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
Hard CPU and memory limits require a writable delegated cgroup-v2 subtree; TTC
fails before start when the requested enforcement is unavailable.

## Daemon operation and recovery

Normal commands use the versioned per-user `ttcd` API. Start it once before
issuing ordinary commands:

```shell
ttc daemon start
ttc daemon status
ttc daemon stop
```

One daemon uses one runtime platform and one isolation mode across every data
root it serves. Start it without an option for systrap and normal rootless host
isolation, use `ttc daemon start --platform=kvm` when `/dev/kvm` is available,
or use
`ttc daemon start --unsafe-no-host-isolation` for trusted workloads on a host
that cannot create an unprivileged user namespace. An explicit start request
does not reconfigure an already-running daemon; stop it first if you want to
change modes. `daemon status` returns the active platform and isolation mode in
JSON.

Durable mutation records can be inspected with `ttc daemon requests` after an
interrupted operation.

`ttc daemon stop` refuses to exit while any branch is running, paused,
dehydrating, or rehydrating. Choose the desired policy explicitly:

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
RUNSC="$PWD/bazel-bin/runsc/ttcd/ttcd_/ttcd" \
  "$PWD/runsc/ttc/test/e2e_image_matrix.sh"
```

See the [test guide](../../runsc/ttc/test/README.md) for rootless and trusted
no-user-namespace variants.
