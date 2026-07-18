# TimeTravelOS user guide

TimeTravelOS (TTOS) is an experimental, rootless container manager built on a
modified gVisor `runsc`. In addition to Docker-like image and container
commands, TTOS can pause, resume, and live-fork a running container into
multiple branches inside one Sentry process.

TTOS is currently intended for trusted development and integration-test
workloads. It is not yet a general Docker replacement or a production security
boundary. Read the [performance and compatibility audit](../proposals/timetravel_ttos_audit.md)
before depending on live-fork behavior.

## What is installed

TTOS consists of three binaries:

* `runsc`, containing the TimeTravelOS Sentry and live-fork implementation;
* `ttos`, the user-facing command-line client; and
* `ttosd`, the per-user manager service.

`ttos` starts the service automatically when needed. Docker, containerd, CRIU,
KVM, and a containerd snapshotter are not required. TTOS talks directly to OCI
registries and stores image content under its own data root.

## Host requirements

TTOS builds and runs on Linux x86-64 and ARM64. Build with the Bazel version in
the repository's `.bazelversion` file (currently 8.3.1).

For a direct Debian or Ubuntu Bazel build, install the native and cross-build
dependencies used by gVisor's canonical build image. In particular, `runsc`
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
`ttos daemon start` performs an equivalent disposable user/mount-namespace
probe automatically and refuses to start in rootless mode when the probe fails,
so an apparently healthy daemon cannot defer this error until container launch.
Directly launching `ttosd` performs the same preflight.

For trusted local testing only, start the daemon with
`--unsafe-no-host-isolation` to bypass the host user-namespace and chroot
setup:

```shell
ttos daemon start --unsafe-no-host-isolation
ttos run --rm busybox:1.36 echo hello
```

Isolation mode is fixed for the lifetime of that daemon. All ordinary commands
inherit it, so do not repeat the flag on `run`, `exec`, or other operations.
`ttos daemon status` reports `isolation_mode` and
`unsafe_no_host_isolation`. Stop and explicitly restart the daemon to change
modes. Ordinary commands do not autostart the service; if it is absent, they
tell you to run `ttos daemon start`.

Unsafe mode retains Sentry-level guest isolation and branch semantics, but
removes host-side defense in depth. Do not use it for untrusted images. For a
one-off recovery command that bypasses the daemon, the equivalent opt-in is:

```shell
ttos --direct --unsafe-no-host-isolation run --rm busybox:1.36 echo hello
```

## Compile

From the gVisor repository root:

```shell
bazel build //runsc:runsc //runsc/ttos:ttos //runsc/ttosd:ttosd
```

The binaries are written to:

```text
bazel-bin/runsc/runsc_/runsc
bazel-bin/runsc/ttos/ttos_/ttos
bazel-bin/runsc/ttosd/ttosd_/ttosd
```

You can use them directly without installation:

```shell
TTOS="$PWD/bazel-bin/runsc/ttos/ttos_/ttos"
RUNSC="$PWD/bazel-bin/runsc/runsc_/runsc"

"$TTOS" --runsc "$RUNSC" --help
```

When `runsc` is not installed beside `ttos`, pass `--runsc` or set
`TTOS_RUNSC`.

## Optional per-user installation

Keeping all three binaries together lets `ttos` find `runsc` and `ttosd`
automatically:

```shell
mkdir -p "$HOME/.local/libexec/ttos" "$HOME/.local/bin"
install -m 0755 bazel-bin/runsc/runsc_/runsc \
  "$HOME/.local/libexec/ttos/runsc"
install -m 0755 bazel-bin/runsc/ttos/ttos_/ttos \
  "$HOME/.local/libexec/ttos/ttos"
install -m 0755 bazel-bin/runsc/ttosd/ttosd_/ttosd \
  "$HOME/.local/libexec/ttos/ttosd"
ln -sfn "$HOME/.local/libexec/ttos/ttos" "$HOME/.local/bin/ttos"
```

Add `$HOME/.local/bin` to `PATH` if necessary, then verify the installation:

```shell
ttos --version
ttos daemon start
ttos daemon status
ttos daemon stop
```

No privileged installation step is required. A system-wide installation may
place the same three files together under `/usr/local/libexec/ttos` and link
`ttos` into `/usr/local/bin`.

## State and runtime paths

Image content, materialized immutable roots, container metadata, logs, and
managed-volume snapshots default to:

```text
$TTOS_DATA_ROOT
$XDG_DATA_HOME/ttos
$HOME/.local/share/ttos
```

The first defined location wins. The native OCI content store is the
`image-store` subdirectory of that data root.

The default `runsc` root is `$XDG_RUNTIME_DIR/runsc`. Without
`XDG_RUNTIME_DIR`, the inherited gVisor default is `/var/run/runsc`, which is
usually not writable by an unprivileged user. In a non-systemd or minimal shell,
pass explicit per-user paths before the subcommand:

```shell
RUNTIME_BASE="${XDG_RUNTIME_DIR:-/tmp/ttos-$UID}"

ttos \
  --root "$RUNTIME_BASE/runsc" \
  --daemon-socket "$RUNTIME_BASE/ttosd.sock" \
  run --rm busybox:1.36 echo hello
```

Global options must appear before the command. Use a dedicated data root for
each independent TTOS installation or test run:

```shell
export TTOS_DATA_ROOT="$HOME/.local/share/ttos"
export TTOS_RUNSC="$HOME/.local/libexec/ttos/runsc"
```

Do not point two incompatible TTOS versions at the same live runtime root.

## Images

Pull, list, inspect, and remove images with familiar commands:

```shell
ttos pull alpine:3.20
ttos images
ttos image inspect alpine:3.20
ttos rmi alpine:3.20
```

Short names use Docker's familiar-name rules, so `alpine:3.20` resolves to
`docker.io/library/alpine:3.20`. TTOS reads authentication from
`$DOCKER_CONFIG/config.json` or `$HOME/.docker/config.json`, including Docker
credential helpers.

Pull policy is selected per `run` or `create`:

```shell
ttos run --pull missing IMAGE       # default: reuse, otherwise pull
ttos run --pull always IMAGE        # refresh the registry reference
ttos run --pull never IMAGE         # require a local image record
```

`ttos pull` contacts the registry even when all blobs are already cached.
`--pull missing` is the fastest offline path after the first pull.

## Run and inspect containers

Run a foreground one-shot command:

```shell
ttos run --rm alpine:3.20 uname -a
```

Start a persistent detached container:

```shell
ttos run -dit --name web alpine:3.20 sh
ttos ps
ttos inspect web
ttos logs web
```

Container IDs are 64 hexadecimal characters. A unique ID prefix or the assigned
name may be used in later commands. The main branch is selected when no branch
suffix is present:

```text
web       means web@main
web@test  selects branch test in family web
```

Use `ttos ls --tree` for the branch lineage and `ttos top web@test` for the
processes in one branch.

## Live fork, pause, and resume

Always check eligibility when a workload may hold external resources:

```shell
ttos fork --check web
ttos fork --check --json web
```

Create a running child branch:

```shell
ttos fork web web@test-a
```

Or create it paused so no child instruction executes before inspection:

```shell
ttos fork --paused web web@test-b
ttos resume web@test-b
```

The source is paused while its kernel metadata is cloned, then returned to its
previous state. Application memory, tmpfs files, managed volumes, and writable
overlay state begin shared and detach copy-on-write. Virtual PIDs and threads
are reproduced in the child; both branches may therefore observe PID 1 and the
same initial process tree.

Manage branches independently:

```shell
ttos pause web@test-a
ttos resume web@test-a
ttos kill --signal TERM web@test-a
ttos wait web@test-a
ttos branch rm web@test-a
```

`ttos rm web` removes the whole family and requires all live branches to be
stopped unless `--force` is used.

## Exec, attach, detach, and logs

Run a command in a selected branch:

```shell
ttos exec web@test-b cat /etc/os-release
ttos exec -it web@test-b sh
ttos exec -d web@test-b long-running-command
ttos exec ls web@test-b
```

For `-t`, TTOS propagates the invoking client's `TERM` value into a new
container or exec process when the image/container and command do not already
define `TERM`; an unset client value falls back to `xterm`. This lets terminal
applications such as `tmux` select the correct terminfo entry while preserving
an explicitly configured container terminal type.

Attach to the branch's init console:

```shell
ttos attach -i web@test-b
```

The default detach sequence is Ctrl-P followed by Ctrl-Q. Init-console and
managed-console attachments use TTOS's internal pipes or PTYs and do not place
host FDs in the guest resource graph, so merely attaching does not make the
branch ineligible. A synchronous exec created with directly imported host stdio
is different: that exec must exit before the branch can fork. Output continues
into persistent logs:

```shell
ttos logs --follow --tail 100 web@test-b
```

## Filesystems and volumes

The ordinary container root is writable by default. While the container is
running it is a Sentry-owned memory overlay, so live forks share it copy-on-write.
When the main container exits, TTOS exports only that overlay's upper layer and
uses it as the seed for the next `ttos start`; normal rootfs changes and shutdown
handler writes therefore survive stop/start. A read-only root (`--read-only`)
has no writable upper, and child-branch changes are not promoted to the main
container's restart seed.

Plain tmpfs mounts remain ephemeral across stop/start. Use a managed volume for
tmpfs-backed data that must be forkable and restart-persistent.

Arbitrary Sentry-owned tmpfs, alternate procfs, and restricted read-only sysfs
mounts are supported:

```shell
ttos run -d --name work \
  --tmpfs /scratch:size=64m,mode=1777 \
  --mount type=proc,target=/alt-proc,readonly \
  --mount type=sysfs,target=/alt-sys,readonly \
  alpine:3.20 sleep 600
```

Managed named volumes fork with the family:

```shell
ttos volume create --opt size=256m state
ttos run -d --name db --volume state:/var/lib/app alpine:3.20 sleep 600
ttos fork db db@test
ttos volume inspect --usage db@test state
ttos volume commit db@test state
```

Volume names are configuration identities. Separate container families get
separate in-memory instances. `volume commit` explicitly promotes one branch's
contents to the persistent seed used by a later restart or new family.

A host bind must be read-only:

```shell
ttos run --rm \
  --mount type=bind,source=/host/input,target=/input,readonly \
  alpine:3.20 cat /input/file
```

Such a bind is a shared external view, not a snapshot; later host changes may
become visible in every branch. `immutable-source` records the user's promise
that the host path will not change, but TTOS cannot enforce it. A branch-local
memory overlay can be placed above an asserted immutable lower:

```shell
ttos run -d --name edit \
  --mount type=bind,source=/host/base,target=/work,cow,immutable-source \
  alpine:3.20 sleep 600
```

## Networking and resource limits

TTOS exposes three network modes:

```shell
--network none       # loopback-only netstack; forkable and the default
--network ttos       # loopback plus rootless in-process virtual NIC; forkable
--network host       # hostinet; host-facing and not live-forkable
```

`none` creates a userspace netstack with only canonical loopback. Processes in
one branch can use TCP and UDP over `127.0.0.1` and `::1`. At fork, TTOS creates
a new netstack and loopback endpoint, clones protocol/socket state, and
preserves internal connections and unread data independently in both branches.

`ttos` adds a rootless, Sentry-managed IPv4 interface without TAP, veth, a host
network namespace, CNI, or elevated network capabilities:

```text
ttos0       10.0.2.15/24
gateway     10.0.2.2
```

The interface is useful by itself for a stable container-visible NIC. Add
`--nat` to permit outbound IPv4 TCP and UDP through the Sentry's user-mode
proxy:

```shell
ttos run --rm --network ttos --nat alpine:3.20 \
  wget -qO- https://example.com/
```

Without `--nat`, non-container destinations are unreachable. TTOS rewrites a
host loopback DNS resolver such as `127.0.0.53` to its corresponding
`10.0.2.53` host alias, so common systemd-resolved configurations work through
the proxy. NAT is currently IPv4-only and implements TCP plus short-lived UDP
mappings; ICMP forwarding is not provided.

Publish a container TCP port with Docker-style `-p`/`--publish`:

```shell
ttos run -d --name web --network ttos \
  -p 127.0.0.1:8080:80 nginx:1.27-alpine
curl http://127.0.0.1:8080/
```

Accepted forms are `HOST_PORT:CONTAINER_PORT` and
`HOST_IP:HOST_PORT:CONTAINER_PORT[/tcp]`. Host addresses and published ports are
currently IPv4/TCP only. The binding is persistent configuration: `ttosd`
reconciles its rootless user-mode listener after restart. It is also shown in
Docker-compatible `.HostConfig.PortBindings` inspect output.

Live-fork policy deliberately distinguishes internal from external flows:

* the source branch's established and connecting NAT TCP flows continue;
* the source branch retains its published-port listener and accepted flows;
* the child receives a fresh virtual link and proxy realm;
* every inherited child TCP flow to the host, internet, or a published-port
  proxy is reset, while internal loopback and `ttos0` connections remain; and
* no host port is automatically published for a child branch.

Thus a fork is not rejected merely because an external TCP flow is open. Child
applications observe ordinary connection-reset behavior and may reconnect
outbound. Publishing a child branch separately is not implemented yet. UDP NAT
mappings are runtime-only and are re-created on later child datagrams.

Modified iptables/conntrack state, nftables, secondary network namespaces,
hostinet, and plugin stacks still fail `ttos fork --check`. `host` mode remains
useful for conventional execution but is not live-forkable because its socket
state belongs to the host kernel.

Upstream runsc's `sandbox` mode imports a network namespace prepared by Docker,
CNI, or another manager. That manager owns the veth/bridge topology, address
assignment, NAT, and port rules. TTOS's `ttos` mode is a separate in-process
design and does not require that host networking control plane. TTOS still does
not implement the Docker Engine network API, named networks, or inter-family
virtual switching.

PID and Sentry-enforced limits work without host cgroups where possible.
Hard CPU and memory limits require a writable delegated cgroup-v2 subtree; TTOS
fails before start when the requested enforcement is unavailable.

## Daemon operation and recovery

Normal commands use the versioned per-user `ttosd` API. Start it once before
issuing ordinary commands:

```shell
ttos daemon start
ttos daemon status
ttos daemon stop
```

One daemon uses one isolation mode across every data root it serves. Start it
without an option for normal rootless host isolation, or use
`ttos daemon start --unsafe-no-host-isolation` for trusted workloads on a host
that cannot create an unprivileged user namespace. An explicit start request
does not reconfigure an already-running daemon; stop it first if you want to
change modes. `daemon status` returns the active mode in JSON.

`--direct` bypasses `ttosd` for recovery and debugging. Do not routinely mix
direct mutations with daemon-mediated commands against the same data root.
Durable mutation records can be inspected with `ttos daemon requests` after an
interrupted operation.

`ttos daemon stop` is an orderly manager shutdown: it resumes paused branches,
stops descendants before their source branches using each container's normal
stop policy, persists managed volumes and writable rootfs state, and then exits
the daemon. If a stop fails, the daemon remains available and reports the
failure. In contrast, an unexpected `ttosd` crash does not terminate Sentry
sandboxes; after restarting the daemon, TTOS reconciles those live containers
and their console/port sidecars.

## Shell completion

Generate completion for Bash, Zsh, or Fish:

```shell
ttos completion bash >"$HOME/.local/share/bash-completion/completions/ttos"
ttos completion zsh >"${fpath[1]}/_ttos"
ttos completion fish >"$HOME/.config/fish/completions/ttos.fish"
```

Create the target directory first if it does not exist.

## Upgrade from the old containerd-backed build

Existing family metadata and materialized root filesystems remain usable.
Image records and blobs formerly owned by containerd are not imported; pull
each required image once into the native TTOS data root. Restart a manually
managed `ttosd` after upgrading, then start it explicitly before issuing normal
commands.

## Further verification

The public-image matrix exercises BusyBox, Alpine, Python, nginx, Redis, image
reuse, exec, logs, mounts, volumes, pause/resume, recursive fork, and daemon
restart:

```shell
bazel build //runsc:runsc //runsc/ttos:ttos //runsc/ttosd:ttosd \
  //runsc/ttos/test:e2e_image_matrix

TTOS="$PWD/bazel-bin/runsc/ttos/ttos_/ttos" \
RUNSC="$PWD/bazel-bin/runsc/runsc_/runsc" \
  "$PWD/runsc/ttos/test/e2e_image_matrix.sh"
```

See the [test guide](../../runsc/ttos/test/README.md) for rootless and trusted
no-user-namespace variants.
