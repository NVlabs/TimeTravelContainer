# TimeTravelContainer user guide

TimeTravelContainer (TTC) is an experimental, rootless container manager built on a
modified gVisor `runsc`. In addition to Docker-like image and container
commands, TTC can pause, resume, and live-fork a running container into
multiple branches inside one Sentry process.

TTC is currently intended for trusted development and integration-test
workloads. It is not yet a general Docker replacement or a production security
boundary. Non-root initial and exec processes use Docker-shaped capability
defaults, and SUID/SGID elevation remains confined to the Sentry's virtual
credentials. Read the
[security, performance, and compatibility audit](../proposals/timetravel_ttc_audit.md)
before depending on live-fork or credential-isolation behavior.

## What is installed

TTC consists of three binaries:

* `runsc`, containing the TimeTravelContainer Sentry and live-fork implementation;
* `ttc`, the user-facing command-line client; and
* `ttcd`, the per-user manager service.

Start `ttcd` explicitly with `ttc daemon start` before ordinary commands.
Docker, containerd, CRIU, KVM, and a containerd snapshotter are not required.
TTC talks directly to OCI registries and stores image content under its own
data root.

## Host requirements

TTC builds and runs on Linux x86-64 and ARM64. Build with the Bazel version in
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
`ttc daemon start` performs an equivalent disposable user/mount-namespace
probe automatically and refuses to start in rootless mode when the probe fails,
so an apparently healthy daemon cannot defer this error until container launch.
Directly launching `ttcd` performs the same preflight.

The daemon also probes the actual volume object store under `--data-root` with
`FICLONE`. It fails startup if the backing filesystem lacks working reflinks.
To explicitly trade first-write performance for compatibility, use:

```shell
ttc daemon start --allow-slow-volume-copy
```

This fallback copies a complete shared file on its first write after a fork.
It is never selected silently. `ttc daemon status` reports the fixed
`volume_copy_mode`, data root, backing device, and filesystem type.

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
`unsafe_no_host_isolation`. Stop and explicitly restart the daemon to change
isolation or volume-copy modes. Ordinary commands do not autostart the service; if it is absent, they
tell you to run `ttc daemon start`.

Unsafe mode retains Sentry-level guest isolation and branch semantics, but
removes host-side defense in depth. Do not use it for untrusted images. For a
one-off recovery command that bypasses the daemon, the equivalent opt-in is:

```shell
ttc --direct --unsafe-no-host-isolation run --rm busybox:1.36 echo hello
```

## Compile

The repository's Docker-based build wrapper can copy all three binaries in one
command:

```shell
mkdir -p bin
make copy TARGETS="runsc runsc/ttc runsc/ttcd" DESTINATION="$PWD/bin"
```

This build path requires Docker only while compiling; Docker is not needed to
run TTC. To build directly on the host with Bazel, from the gVisor repository
root run:

```shell
bazel build //runsc:runsc //runsc/ttc:ttc //runsc/ttcd:ttcd
```

The binaries are written to:

```text
bazel-bin/runsc/runsc_/runsc
bazel-bin/runsc/ttc/ttc_/ttc
bazel-bin/runsc/ttcd/ttcd_/ttcd
```

You can use them directly without installation:

```shell
TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc"
RUNSC="$PWD/bazel-bin/runsc/runsc_/runsc"

"$TTC" --runsc "$RUNSC" --help
```

When `runsc` is not installed beside `ttc`, pass `--runsc` or set
`TTC_RUNSC`.

## Optional per-user installation

Keeping all three binaries together lets `ttc` find `runsc` and `ttcd`
automatically:

```shell
mkdir -p "$HOME/.local/libexec/ttc" "$HOME/.local/bin"
install -m 0755 bazel-bin/runsc/runsc_/runsc \
  "$HOME/.local/libexec/ttc/runsc"
install -m 0755 bazel-bin/runsc/ttc/ttc_/ttc \
  "$HOME/.local/libexec/ttc/ttc"
install -m 0755 bazel-bin/runsc/ttcd/ttcd_/ttcd \
  "$HOME/.local/libexec/ttc/ttcd"
ln -sfn "$HOME/.local/libexec/ttc/ttc" "$HOME/.local/bin/ttc"
```

Add `$HOME/.local/bin` to `PATH` if necessary, then verify the installation:

```shell
ttc --version
ttc daemon start
ttc daemon status
ttc daemon stop
```

No privileged installation step is required. A system-wide installation may
place the same three files together under `/usr/local/libexec/ttc` and link
`ttc` into `/usr/local/bin`.

## State and runtime paths

Image content, materialized immutable roots, container metadata, and logs
default to:

```text
$TTC_DATA_ROOT
$XDG_DATA_HOME/ttc
$HOME/.local/share/ttc
```

The first defined location wins. The native OCI content store is the
`image-store` subdirectory of that data root.

The default `runsc` root is `$XDG_RUNTIME_DIR/runsc`. Without
`XDG_RUNTIME_DIR`, TTC uses `/tmp/ttc-$UID/runsc`, alongside its per-user
daemon socket. Both locations are writable only by the invoking user. Explicit
paths remain available for independent installations or test runs:

```shell
RUNTIME_BASE="${XDG_RUNTIME_DIR:-/tmp/ttc-$UID}"

ttc \
  --root "$RUNTIME_BASE/runsc" \
  --daemon-socket "$RUNTIME_BASE/ttcd.sock" \
  run --rm busybox:1.36 echo hello
```

Global options must appear before the command. Use a dedicated data root for
each independent TTC installation or test run:

```shell
export TTC_DATA_ROOT="$HOME/.local/share/ttc"
export TTC_RUNSC="$HOME/.local/libexec/ttc/runsc"
```

Do not point two incompatible TTC versions at the same live runtime root.

## Images

Pull, list, inspect, and remove images with familiar commands:

```shell
ttc pull alpine:3.20
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

`ttc pull` contacts the registry even when all blobs are already cached.
Normal pulls and implicit image preparation print line-oriented resolve,
download, cache-reuse, and extraction progress that survives daemon stream
forwarding. `ttc pull -q` suppresses progress and prints only the digest.
`--pull missing` is the fastest offline path after the first pull.

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
ttc logs web
```

Container IDs are 64 hexadecimal characters. A unique ID prefix or the assigned
name may be used in later commands. The main branch is selected when no branch
suffix is present:

```text
web       means web@main
web@test  selects branch test in family web
```

Use `ttc ls --tree` for the branch lineage and `ttc top web@test` for the
processes in one branch.

## Live fork, pause, and resume

Fork atomically pauses the source and validates its frozen resource graph. If
validation fails, TTC removes the tentative child, resumes a previously running
source, writes the reason to stderr, and exits nonzero. Create a running child
branch directly:

```shell
ttc fork web web@test-a
```

Or create it paused so no child instruction executes before inspection:

```shell
ttc fork --paused web web@test-b
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
ttc kill --signal TERM web@test-a
ttc wait web@test-a
ttc branch rm web@test-a
```

`ttc rm web` removes the whole family and requires all live branches to be
stopped unless `--force` is used.

## Exec, attach, detach, and logs

Run a command in a selected branch:

```shell
ttc exec web@test-b cat /etc/os-release
ttc exec -it web@test-b sh
ttc exec -d web@test-b long-running-command
ttc exec ls web@test-b
```

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

The default detach sequence is Ctrl-P followed by Ctrl-Q. Init-console and
managed-console attachments use TTC's internal pipes or PTYs and do not place
host FDs in the guest resource graph, so merely attaching does not make the
branch ineligible. A synchronous exec created with directly imported host stdio
is different: that exec must exit before the branch can fork. Output continues
into persistent logs:

```shell
ttc logs --follow --tail 100 web@test-b
```

## Copy files between the host and a branch

`ttc cp` uses Docker-compatible endpoint syntax. Exactly one operand must be a
container selector:

```shell
ttc cp ./fixtures web:/tmp/fixtures
ttc cp ./fixtures/. web@test-b:/tmp/fixtures
ttc cp web@test-b:/tmp/report.json ./report.json
ttc cp -a web@test-b:/var/lib/app ./saved-state
ttc cp -L web@test-b:/tmp/latest ./resolved-latest
```

The selector is `CONTAINER[@BRANCH]:PATH`; omitting `@BRANCH` selects `main`.
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

Copy supports running, paused, and fully stopped branches. For a paused branch,
TTC runs only its trusted copy helper while the branch's existing tasks remain
stopped.
For a stopped family, TTC launches a short-lived helper Sentry whose process is
the donated TTC copy helper, mounts the immutable image lower plus the selected
persistent `ttcvol` rootfs/volume branch, and exits when the copy completes.
It neither launches the image entrypoint nor restores process memory. TTC
rejects offline access to one stopped child while another branch in the same
family remains live, because two Sentries would otherwise cache one repository
concurrently. A copy process temporarily owns host stream FDs; `ttcd` therefore
serializes copy operations with lifecycle mutations such as fork, pause, stop,
and branch removal.

## Filesystems and volumes

The ordinary container root is writable by default. Its immutable image lower
is covered by a family-owned, disk-backed `ttcvol` upper managed by the Sentry.
Every namespace, metadata, and regular-file mutation updates that repository
directly. Acknowledged data writes use normal host page-cache semantics, and
guest `fsync`/`fdatasync` propagate to backing files. TTC performs no rootfs
export on stop and no rootfs import on start.

Live fork publishes a new immutable metadata-tree root without walking the
filesystem. Both branches initially reference the same opaque regular-file
objects; the first write in either branch reflinks the affected file, after
which the backing filesystem performs block/extent CoW. A read-only root
(`--read-only`) has no writable upper. Restart recreates process/kernel state
only for `main`, while each stopped child retains its filesystem branch for
offline `ttc cp`.

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
fork, TTC duplicates only an immutable metadata-tree root. The first write to
one shared regular file reflinks that file, after which the host filesystem
performs extent CoW. Stop/start reopens the same repository; there is no tar
export/import. `ttc rm -v` also removes anonymous volumes, while named volumes
remain until `ttc volume rm`.

The daemon requires `FICLONE` by default. On a host without reflinks, starting
it with `--allow-slow-volume-copy` explicitly selects whole-file copying on
the first write after each fork. See the
[disk-backed volume design](../proposals/ttc_disk_backed_volumes.md).

One named volume may have one writable container family at a time. TTC rejects
a second writable attachment because separate Sentry namespace caches are not
yet coherent; additional read-only attachments are allowed.

A host bind must be read-only:

```shell
ttc run --rm \
  --mount type=bind,source=/host/input,target=/input,readonly \
  alpine:3.20 cat /input/file
```

Such a bind is a shared external view, not a snapshot; later host changes may
become visible in every branch. `immutable-source` records the user's promise
that the host path will not change, but TTC cannot enforce it. A branch-local
memory overlay can be placed above an asserted immutable lower:

```shell
ttc run -d --name edit \
  --mount type=bind,source=/host/base,target=/work,cow,immutable-source \
  alpine:3.20 sleep 600
```

## Networking and resource limits

TTC exposes three network modes:

```shell
--network none       # loopback-only netstack; forkable and the default
--network ttc       # loopback plus rootless in-process virtual NIC; forkable
--network host       # hostinet; host-facing and not live-forkable
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
currently IPv4/TCP only. The binding is persistent configuration: `ttcd`
reconciles its rootless user-mode listener after restart. It is also shown in
Docker-compatible `.HostConfig.PortBindings` inspect output.

Add or remove a listener on any existing live branch without restarting it:

```shell
ttc port publish web@main 127.0.0.1:8081:80
ttc port publish web@child 127.0.0.1:8082:80
ttc port ls web
ttc port ls --json web@child
ttc port unpublish web@child 127.0.0.1:8082:80
```

Omitting `@BRANCH` from `publish` or `unpublish` selects `main`. For `port ls`,
an unqualified container lists every branch, while a qualified selector filters
one branch. Dynamically published listeners are persistent and reconciled after
`ttcd` restarts. Docker-compatible `.HostConfig.PortBindings` continues to
show only `main`; the complete branch-aware list is available through
`ttc port ls --json` and `.TTC.container.published_ports` in `ttc inspect`.

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

Modified iptables/conntrack state, nftables, secondary network namespaces,
hostinet, and plugin stacks still cause `ttc fork` to fail during its atomic
validation. `host` mode remains
useful for conventional execution but is not live-forkable because its socket
state belongs to the host kernel.

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

One daemon uses one isolation mode across every data root it serves. Start it
without an option for normal rootless host isolation, or use
`ttc daemon start --unsafe-no-host-isolation` for trusted workloads on a host
that cannot create an unprivileged user namespace. An explicit start request
does not reconfigure an already-running daemon; stop it first if you want to
change modes. `daemon status` returns the active mode in JSON.

`--direct` bypasses `ttcd` for recovery and debugging. Do not routinely mix
direct mutations with daemon-mediated commands against the same data root.
Durable mutation records can be inspected with `ttc daemon requests` after an
interrupted operation.

`ttc daemon stop` is an orderly manager shutdown: it resumes paused branches,
stops descendants before their source branches using each container's normal
stop policy, and then exits the daemon. Rootfs and managed-volume writes were
already applied directly to their repositories; shutdown performs no TTC
save/export step. If a stop fails, the daemon remains available and reports
the failure. In contrast, an unexpected `ttcd` crash does not terminate Sentry
sandboxes; after restarting the daemon, TTC reconciles those live containers
and their console/port sidecars.

## Shell completion

Generate completion for Bash, Zsh, or Fish:

```shell
ttc completion bash >"$HOME/.local/share/bash-completion/completions/ttc"
ttc completion zsh >"${fpath[1]}/_ttc"
ttc completion fish >"$HOME/.config/fish/completions/ttc.fish"
```

Create the target directory first if it does not exist.

## Upgrade from the old containerd-backed build

Existing family metadata and materialized root filesystems remain usable.
Image records and blobs formerly owned by containerd are not imported; pull
each required image once into the native TTC data root. Restart a manually
managed `ttcd` after upgrading, then start it explicitly before issuing normal
commands.

## Further verification

The public-image matrix exercises BusyBox, Alpine, Python, nginx, Redis, image
reuse, exec, logs, mounts, volumes, pause/resume, recursive fork, and daemon
restart:

```shell
bazel build //runsc:runsc //runsc/ttc:ttc //runsc/ttcd:ttcd \
  //runsc/ttc/test:e2e_image_matrix

TTC="$PWD/bazel-bin/runsc/ttc/ttc_/ttc" \
RUNSC="$PWD/bazel-bin/runsc/runsc_/runsc" \
  "$PWD/runsc/ttc/test/e2e_image_matrix.sh"
```

See the [test guide](../../runsc/ttc/test/README.md) for rootless and trusted
no-user-namespace variants.
