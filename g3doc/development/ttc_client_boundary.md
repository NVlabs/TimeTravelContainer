# TTC client/daemon boundary

`ttc` owns command grammar, help, completion, terminal forwarding, and the
versioned Unix-socket framing. It does not import daemon execution, image,
bundle, storage, or Sentry packages. `ttcd` alone interprets `TTC_ROOT` and
`TTC_LOCAL_ROOT`, owns runtimes and storage, and contains internal copy and
detached-auto-remove helpers.

This boundary also applies to `ttc daemon start`: the client verifies and
launches its exact-build sibling but does not inspect current or legacy root
environment variables. `ttcd` validates them before creating runtime or
storage state.

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

The end-to-end scripts stage matching `ttc` and `ttcd` siblings, export an
explicit `TTC_ROOT`, and give the client only a daemon socket. This catches an
accidental reintroduction of root or runtime forwarding.
