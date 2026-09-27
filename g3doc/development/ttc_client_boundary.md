# TTC client/daemon boundary

`ttc` owns command grammar, help, completion, terminal forwarding, and the
versioned Unix-socket framing. It does not import daemon execution, image,
bundle, storage, or Sentry packages. A daemon's root, `TTC_ROOT` or on a node
of a cluster `TTC_LOCAL_ROOT`, identifies it: `ttc` uses the root only to find
the daemon's socket, `ttcd.sock` there, and to open the daemon's startup log,
`ttcd.log`. `ttcd` alone interprets the roots, owns runtimes and storage, and
contains internal copy and detached-auto-remove helpers.

`ttc daemon start` verifies and launches its exact-build sibling with the
socket of the root. `ttcd` validates the roots, including that the socket is
theirs, before creating runtime or storage state.

The official CLI includes its build ID on daemon requests. A mismatch rejects
every operation except the minimal `daemon stop` escape hatch. SDKs and MCP
clients instead negotiate API version and the `features` returned by `ping`.

## Size and dependency qualification

Measured on Linux/amd64 with the same repository Bazel configuration and
stripped static output:

| Build | `ttc` size |
| --- | ---: |
| Parent of the split (`ed227a4ff`) | 43,327,650 bytes (41.32 MiB) |
| Split client | 2,920,610 bytes (2.79 MiB) |

The split removes 93.26% of the client binary (14.84x smaller). A Bazel
`deps(//runsc/ttc:ttc)` audit found no dependency under `//pkg/sentry`,
`//runsc/boot`, `//runsc/sandbox`, `//runsc/ttc/cmd`,
`//runsc/ttc/bundle`, or `//runsc/ttc/images`. The client target depends only
on its local client package, TTC version metadata, the Go standard library,
and terminal support.

The end-to-end scripts stage matching `ttc` and `ttcd` siblings and export an
explicit `TTC_ROOT`, and `TTC_LOCAL_ROOT` for a node of a cluster, which both
the client and the daemon use.
