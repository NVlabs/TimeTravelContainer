# TTOS end-to-end image matrix

`e2e_image_matrix.sh` is the authoritative manual smoke test for running
unmodified public Docker images through the complete TTOS path. It currently
covers:

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
  configuration, env-file/host-import environment overrides, merged labels,
  user mounts, sandbox PID, per-branch processes and timings, and explicit
  fork-eligibility reports;
- Docker-style `container` aliases, multi-container detached `start`, concurrent
  `wait` with argument-ordered exit codes, restart of an exited container, and
  an explicitly forced running-family restart where in-memory managed-volume
  contents are expected to be recreated;
- persisted `--network host` startup using an unmodified BusyBox HTTP server,
  Docker inspect projection, host-loopback reachability, and live-fork
  rejection for the externally shared network stack;
- BusyBox process/procfs/sysfs startup;
- Docker `--read-only` rootfs persistence through restart and live fork,
  including write rejection in both branches while custom tmpfs and managed
  volumes remain writable;
- image `STOPSIGNAL` inheritance plus Docker `--stop-signal` and
  `--stop-timeout` inspect persistence, graceful-stop delivery, and final exit-code reconciliation;
- Docker `--pids-limit` OCI and inspect persistence, Sentry-side process and
  thread enforcement without delegated host cgroups, and branch-local limit and
  task-charge inheritance across live fork;
- Docker-style custom tmpfs mode/size plus alternate private procfs and
  restricted sysfs mounts, including inspect projection and restart retention;
- Alpine dynamic linking and package-tool startup;
- Python threading, JSON, and in-memory SQLite;
- nginx configuration loading;
- Redis image ownership fidelity for a `999:1000` lower-layer directory while
  physical rootfs files remain caller-owned;
- immediate-exit console integrity with 256 KiB on each of stdout and stderr,
  verified in both foreground output and persistent logs;
- detached output and bounded multi-generation logs;
- `exec`, selective stdin/stdout/stderr `attach`, `fork --check`, live `fork`,
  `pause`, `resume`, `ps`, `top`, and the lineage tree, including
  branch/status/ancestor `ps` filters, templates, latest selection, and JSON
  creation timestamps;
- managed-volume and user tmpfs copy-on-write isolation between a parent and
  child branch;
- read-only host bind visibility and write rejection;
- graceful stop, branch removal, container removal, and volume removal.

The test requires network access for image pulls and an accessible containerd
socket. TTOS only uses containerd's content service; it extracts image layers
into its own rootless data directory instead of requesting a mounted snapshot.
By default, the runtime user must be able to create a user namespace with a
one-entry UID/GID map:

```shell
unshare -Ur /bin/true
```

If this probe fails, fix the host's unprivileged-user-namespace or LSM policy
first. A single-ID mapping does not require `newuidmap`, `newgidmap`, or
subordinate-ID ranges. Some Linux distributions apply AppArmor restrictions
independently of `kernel.unprivileged_userns_clone`.

For trusted test workloads on hosts that prohibit user namespaces, set
`TTOS_UNSAFE_NO_HOST_ISOLATION=1`. The matrix then passes
`--unsafe-no-host-isolation` to TTOS, running the Sentry and gofer as the
invoking user without host namespaces or chroots. Guest namespaces,
credentials, procfs, mounts, and filesystem CoW remain implemented by the
Sentry, but this mode removes host-side defense in depth and must not be used
for untrusted containers.

Build and invoke the matrix with:

```shell
bazel build //runsc:runsc //runsc/ttos:ttos \
  //runsc/ttos/test:e2e_image_matrix

TTOS="$PWD/bazel-bin/runsc/ttos/ttos_/ttos" \
RUNSC="$PWD/bazel-bin/runsc/runsc_/runsc" \
CONTAINERD_ADDRESS=/path/to/containerd.sock \
  "$PWD/runsc/ttos/test/e2e_image_matrix.sh"
```

No-userns example:

```shell
TTOS_UNSAFE_NO_HOST_ISOLATION=1 \
TTOS="$PWD/bazel-bin/runsc/ttos/ttos_/ttos" \
RUNSC="$PWD/bazel-bin/runsc/runsc_/runsc" \
CONTAINERD_ADDRESS=/path/to/containerd.sock \
  "$PWD/runsc/ttos/test/e2e_image_matrix.sh"
```
Use `TTOS_E2E_ROOT` to retain the generated TTOS state for inspection. Image
references and the containerd namespace can be overridden with
`BUSYBOX_IMAGE`, `ALPINE_IMAGE`, `PYTHON_IMAGE`, `NGINX_IMAGE`,
`REDIS_IMAGE`, and `TTOS_NAMESPACE`. By default, temporary TTOS state is
removed on success or failure; pulled content remains managed by the selected containerd namespace.

A passing result is stronger than the unit suite: it proves image pulling,
layer extraction, OCI generation, rootless runsc startup, nontrivial userspace,
console sidecars, and TimeTravel branch operations on the executing host.
