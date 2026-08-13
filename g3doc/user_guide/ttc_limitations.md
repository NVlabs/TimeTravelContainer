# TimeTravelContainer Current Limitations

This page tracks limitations that users should consider when deciding whether
TTC fits a workload. The authoritative compatibility check for a live branch
is `ttc fork`: TTC pauses and validates the source atomically, then either
creates the child or resumes the unchanged source and returns a diagnostic.

## Isolation and portability

- TTC is intended for trusted development, testing, agent, and research
  workloads. It has not yet received the security review needed to serve as a
  production boundary for hostile multi-tenant workloads.
- Active branches in one container family share a Sentry runtime process. A
  runtime failure can therefore affect active peers in that family. Fully
  dehydrated families are process-free.
- `--unsafe-no-host-isolation` is only for trusted workloads on hosts that
  prohibit unprivileged user namespaces.
- Containers use the host CPU architecture. TTC does not provide
  cross-architecture CPU emulation.

## Fork and lifecycle behavior

- Fork latency depends on the number of processes, mappings, and kernel
  resources; it is not universally constant time.
- Resources connected directly to the host or another container may not be
  safely duplicable. TTC rejects a fork when it cannot preserve the requested
  semantics.
- Internal virtual-network connections are copied. The source keeps external
  connections, while the child starts with external connections disconnected
  and no inherited published host ports.
- Stopping a branch preserves its filesystem, configuration, logs, and
  identity but discards its live process and memory state. Use dehydration
  when that state must survive without consuming resident memory.
- Dehydration data is managed by TTC and is not a standalone portable archive.
  Use `ttc checkpoint` for an explicitly exportable file.
- Checkpoint restore currently needs a compatible TTC build and the exact
  recorded image digest in the destination image store. Creating a checkpoint
  also requires the selected branch to be eligible for live fork.

## Storage and networking

- Read-only host mounts remain live shared views. A normal writable host mount
  uses the host path as a live read-only lower layer with a branch-private
  upper; use `:copy` for a view detached from later host changes.
- A managed volume cannot be attached writable to unrelated container
  families at the same time.
- Efficient persistent file CoW requires filesystem reflinks. The
  `--allow-slow-volume-copy` fallback can copy a complete shared file on its
  first write.
- Virtual networking and published ports are currently IPv4-only, and
  published ports currently support TCP.
- Linux 3.17 through 4.14.76 is supported with host GSO disabled
  automatically. Large-payload network performance can be substantially lower
  than on Linux 4.14.77 or newer.

## Ecosystem coverage

- TTC provides a Docker-style CLI, not the Docker Engine API. Docker Compose,
  Swarm, named Docker networks, and general device or accelerator passthrough
  are not currently implemented.
- Image signature, attestation, and content-trust policy are not yet
  implemented.
- Cross-node fork requires mutually authenticated TTC peers, common image
  content, a shared TTC data root, and a reachable page-transfer network. Use
  a checkpoint for offline transfer or unrelated TTC installations.

For the detailed resource matrix, measurements, and audit evidence, see the
[security, performance, and compatibility audit](../proposals/timetravel_ttc_audit.md).
