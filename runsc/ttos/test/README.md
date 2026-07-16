# TTOS end-to-end image matrix

`e2e_image_matrix.sh` is the authoritative manual smoke test for running
unmodified public Docker images through the complete TTOS path. It currently
covers:

- BusyBox process/procfs/sysfs startup;
- Alpine dynamic linking and package-tool startup;
- Python threading, JSON, and in-memory SQLite;
- nginx configuration loading;
- immediate-exit console integrity with 256 KiB on each of stdout and stderr,
  verified in both foreground output and persistent logs;
- detached output and bounded multi-generation logs;
- `exec`, `fork --check`, live `fork`, `pause`, `resume`, `ps`, `top`, and the
  lineage tree;
- managed-volume copy-on-write isolation between a parent and child branch;
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
`BUSYBOX_IMAGE`, `ALPINE_IMAGE`, `PYTHON_IMAGE`, `NGINX_IMAGE`, and
`TTOS_NAMESPACE`. By default, temporary TTOS state is removed on success or
failure; pulled content remains managed by the selected containerd namespace.

A passing result is stronger than the unit suite: it proves image pulling,
layer extraction, OCI generation, rootless runsc startup, nontrivial userspace,
console sidecars, and TimeTravel branch operations on the executing host.
