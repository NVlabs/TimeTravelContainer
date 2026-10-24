# TimeTravelContainer Current Limitations

This page tracks limitations that users should consider when deciding whether
TTC fits a workload. Live-fork eligibility is fixed when a family is created.
The default is eligible: TTC rejects unsupported resource-creation operations
instead of allowing later workload activity to make `ttc fork` unpredictable.
Features whose state is intentionally outside the Sentry require the explicit
`--waive-live-fork` run/create option; such a family is systematically
non-forkable for its entire lifetime.

## Isolation and portability

- TTC is intended for trusted development, testing, agent, and research
  workloads. It has not yet received the security review needed to serve as a
  production boundary for hostile multi-tenant workloads.
- Active branches in one container family share a Sentry runtime process. A
  runtime failure can therefore affect active peers in that family, and loses
  the dehydrations that a standalone daemon keeps in that process until they
  are made durable (`storage_state` `resident`). Fully dehydrated families are
  process-free.
- `--unsafe-no-host-isolation` is only for trusted workloads on hosts that
  prohibit unprivileged user namespaces or, on macOS, where `ttcd` cannot
  apply its Seatbelt sandbox, as inside another sandbox.
- Containers use the host CPU architecture. TTC does not provide
  cross-architecture CPU emulation.
- On macOS, TTC needs Apple Silicon and macOS 15.0 or newer, and runs on the
  `hvf` platform. `--network=host`, hard memory and CPU limits, and
  `--nvproxy` are unavailable there.

## Fork and lifecycle behavior

- Fork latency depends on the number of processes, mappings, and kernel
  resources; it is not universally constant time.
- A fork still rejects an invalid request, including a missing, exited, or
  transitional source, an invalid/existing destination branch,
  or a family created with `--waive-live-fork`. Resource exhaustion, damaged
  persistent state, and internal/runtime failures can also make an individual
  operation fail; these are operational errors, not workload-dependent
  eligibility decisions.
- A dehydrated source is forkable without first being rehydrated. Its children
  start from the durable sparse-memory and filesystem roots; the source remains
  dehydrated.
- `--network=host` is intentionally outside TTC's branchable resource graph and
  therefore requires `--waive-live-fork`. Unsupported host FD, device,
  externally served FUSE, io_uring/AIO, packet-ring, KCOV, writable-sysfs, and
  nftables creation paths fail at creation in an eligible family rather than
  poisoning it later. FUSE filesystems served through the guest's `/dev/fuse`
  are contained in the Sentry and are forkable.
- A `--gfx` or `--nvproxy` family is eligible only with `--gfx-fork` or
  `--nvproxy-fork`, which choose whether a fork of a GPU in use gives the child
  a lost device (`device-lost`) or a copy of the GPU's state (`snapshot`);
  otherwise it needs `--waive-live-fork`. `--nvproxy-fork=snapshot` copies
  only CUDA's state, and so allows only the `compute` and `utility` driver
  capabilities.
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
  also requires a live-fork-eligible family because it begins with a private
  live fork.

## Storage and networking

- Read-only host mounts remain live shared views. A writable host mount, as
  Docker makes one, writes the host path itself, so its family must waive live
  forks; `:lazy_copy` uses the host path as a live read-only lower layer with a
  branch-private upper instead, and `:copy` gives a view detached from later
  host changes.
- A managed volume cannot be attached writable to unrelated container
  families at the same time.
- Efficient persistent file CoW requires filesystem reflinks. Without them,
  TTC automatically copies a complete shared file on its first write.
- Virtual networking and published ports are currently IPv4-only, and
  published ports currently support TCP.
- Linux 3.17 through 4.14.76 is supported with host GSO disabled
  automatically. Large-payload network performance can be substantially lower
  than on Linux 4.14.77 or newer.

## Ecosystem coverage

- TTC provides a Docker-style CLI, not the Docker Engine API. Docker Compose,
  Swarm, named Docker networks, and general device passthrough are not
  currently implemented. GPUs are available as a virtual Vulkan GPU (`--gfx`)
  and, on Linux hosts with NVIDIA GPUs, as those GPUs for CUDA (`--nvproxy`).
- Image signature, attestation, and content-trust policy are not yet
  implemented.
- Cross-node fork assumes a trusted private network, a common shared TTC data
  root, and reachable peer-transfer ports. The nodes authenticate one another
  with a token in the shared root, and each transfer with a token of its own,
  but TTC does not currently add TLS: the network must be trusted against
  eavesdropping. Use a checkpoint for offline transfer or unrelated TTC
  installations.
- Elastic migration uses live sparse handoff. A newly running target may
  temporarily depend on transaction-fenced memory chunks or whole files served
  by the source. Graceful drain resolves these dependencies; unexpected source
  loss before they are cached or published is reported as unrecoverable
  volatile-state loss.
  Automatic rebalancing uses exact resident-page identities represented as
  fixed-size bitmaps. The 4,096-edge bound applies only to the optional
  human-readable sharing-edge diagnostic; truncating that diagnostic does not
  truncate planner input or reject a placement plan.
  Capacity acquisition uses a durable shared-filesystem JSON outbox with
  executable and HTTP webhook adapters. An unsuccessful drain or rebalance
  remains safely retryable after a requested node joins.

For the detailed resource matrix, measurements, and audit evidence, see the
[security, performance, and compatibility audit](../proposals/timetravel_ttc_audit.md).
