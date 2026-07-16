# TTOS end-to-end image matrix

`e2e_image_matrix.sh` is the authoritative manual smoke test for running
unmodified public Docker images through the complete TTOS path. It currently
covers:

- BusyBox process/procfs/sysfs startup;
- Alpine dynamic linking and package-tool startup;
- Python threading, JSON, and in-memory SQLite;
- nginx configuration loading;
- detached output and bounded multi-generation logs;
- `exec`, `fork --check`, live `fork`, `pause`, `resume`, `ps`, `top`, and the
  lineage tree;
- managed-volume copy-on-write isolation between a parent and child branch;
- graceful stop, branch removal, container removal, and volume removal.

The test requires network access for image pulls and an accessible containerd
socket. TTOS only uses containerd's content service; it extracts image layers
into its own rootless data directory instead of requesting a mounted snapshot.
The runtime user must also be able to create a user namespace with a one-entry
UID/GID map:

```shell
unshare -Ur /bin/true
```

If this probe fails, fix the host's unprivileged-user-namespace,
subordinate-ID, or LSM policy first. In particular, some Linux distributions
apply AppArmor restrictions independently of
`kernel.unprivileged_userns_clone`.

Build and invoke the matrix with:

```shell
bazel build //runsc:runsc //runsc/ttos:ttos \
  //runsc/ttos/test:e2e_image_matrix

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
