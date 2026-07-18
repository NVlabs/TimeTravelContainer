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
before starting:

```shell
unshare -Ur /bin/true
```

A one-entry UID/GID map is sufficient. `newuidmap`, `newgidmap`, and subordinate
UID/GID ranges are not required. If the command fails, enable unprivileged user
namespaces and check the host's AppArmor or other LSM policy.

For trusted local testing only, `--unsafe-no-host-isolation` bypasses the host
user-namespace and chroot setup:

```shell
ttos --unsafe-no-host-isolation run --rm busybox:1.36 echo hello
```

This mode retains Sentry-level guest isolation and branch semantics, but removes
host-side defense in depth. Do not use it for untrusted images.

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

Attach to the branch's init console:

```shell
ttos attach -i web@test-b
```

The default detach sequence is Ctrl-P followed by Ctrl-Q. Detaching releases
the host stdin attachment; this is important because an active host attachment
can make a fork ineligible. Output continues into persistent logs:

```shell
ttos logs --follow --tail 100 web@test-b
```

## Filesystems and volumes

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

The supported network modes are:

```shell
--network none       # forkable contained stack; default
--network host       # host-facing and therefore not live-forkable
```

Bridge networking, NAT, published ports, CNI, and Docker's network API are not
implemented by `ttos`. A contained empty or loopback-only netstack can be
forked; external links and connections are rejected by `fork --check`.

PID and Sentry-enforced limits work without host cgroups where possible.
Hard CPU and memory limits require a writable delegated cgroup-v2 subtree; TTOS
fails before start when the requested enforcement is unavailable.

## Daemon operation and recovery

Normal commands use the versioned per-user `ttosd` API. Service management is
rarely necessary:

```shell
ttos daemon status
ttos daemon stop
ttos daemon start
```

`--direct` bypasses `ttosd` for recovery and debugging. Do not routinely mix
direct mutations with daemon-mediated commands against the same data root.
Durable mutation records can be inspected with `ttos daemon requests` after an
interrupted operation.

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
managed `ttosd` after upgrading. CLI-managed daemon autostart handles the
protocol change automatically.

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
