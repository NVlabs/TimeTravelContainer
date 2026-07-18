# TimeTravelOS performance and compatibility audit

This audit covers the TTOS implementation through commit `8368172` and was run
on 2026-07-18. It evaluates the native OCI image store, rootless execution, the
Docker-like CLI, and in-memory live fork. It is a development-readiness audit,
not a security certification.

## Executive assessment

TTOS is suitable for controlled integration-test workloads whose resource graph
passes `ttos fork --check`. It runs several nontrivial unmodified images and its
application-memory fork cost is effectively independent of resident byte count
in the measured range. It does not copy application RAM to disk or duplicate
all resident pages at fork time.

TTOS is not yet a deterministic time-travel system, a drop-in Docker daemon, or
a universally compatible Linux container runtime. The biggest performance cost
is full kernel-metadata serialization while the source is paused. The biggest
semantic gap against identical-observable branching is randomness: `getrandom`
and `/dev/urandom` still read a process-global host entropy source after fork.

The recommended readiness levels are:

| Use case | Assessment |
| --- | --- |
| Trusted integration tests with `--network none` | Ready for broader testing |
| Forking Python, shell, nginx configuration, or Redis-like local workloads | Demonstrated |
| General Docker CLI replacement | Partial |
| External networking at the fork boundary | Unsupported |
| Bit-for-bit deterministic replay after fork | Unsupported |
| Untrusted production multi-tenancy | Not audited or recommended |

## Evidence

The following checks passed during this audit or the immediately preceding
native-store implementation verification:

* image, bundle, command, daemon, and live-fork focused Bazel tests;
* a hermetic Registry V2 pull, cache reuse, offline run, inspect, and deletion;
* the complete public-image matrix with BusyBox 1.36, Alpine 3.20, Python
  3.12-alpine, nginx 1.27-alpine, and Redis 7-alpine;
* exec, logs, attach, daemon restart, pause/resume, recursive fork, internal
  pipes and sockets, tmpfs, managed-volume CoW, and writable overlay CoW;
* the systrap `BenchmarkSandboxFamilyFork` benchmark; and
* real Python containers with 1 MiB, 128 MiB, and 256 MiB touched application
  allocations and multiple guest threads.

The real-PTY suite completed its five quick TTY runs, parent and child attach,
and printed the expected `tty-exec-ok` result. The terminal exec connection then
remained open until the suite's 60-second watchdog, which is recorded below as
a compatibility defect.

## Performance measurements

The host was an x86-64 Intel Xeon Platinum 8488C system. The host policy blocked
unprivileged user namespaces, so real-container measurements used
`--unsafe-no-host-isolation`. These are developer-build, single-host baseline
numbers, not release guarantees.

### Live-fork latency

| Workload | Iterations | Internal fork | Source downtime | External CLI wall time |
| --- | ---: | ---: | ---: | ---: |
| Minimal in-process systrap fixture | 20 | 2.04 ms mean | 2.03 ms mean | n/a |
| Python, 4 guest threads, 1 MiB touched | 3 | 27.55 ms mean | 27.46 ms mean | not recorded |
| Python, 4 guest threads, 256 MiB touched | 3 | 29.12 ms mean | 28.99 ms mean | not recorded |
| Python, 17 guest threads, 128 MiB touched | 5 | 37.46 ms mean | 37.31 ms mean | about 80 ms |

For the minimal fixture, kernel metadata encode/decode accounted for 1.97 ms of
the 2.04 ms internal API time. In the 128 MiB workload, individual internal
forks ranged from 34.10 ms to 42.12 ms.

The 1 MiB versus 256 MiB comparison supports the intended O(metadata), not
O(resident bytes), fork behavior. It is not evidence of constant time with
respect to task count, VMA count, open files, queued bytes, or filesystem
metadata.

### Memory sharing

With one 128 MiB Python parent, adding five paused children changed the Sentry's
host accounting as follows:

| Metric | Parent only | Five children | Increase |
| --- | ---: | ---: | ---: |
| RSS | 58,864 KiB | 64,440 KiB | 5,576 KiB |
| PSS | 44,669 KiB | 45,795 KiB | 1,126 KiB |
| Anonymous RSS | 24,024 KiB | 29,456 KiB | 5,432 KiB |

This is consistent with shared application pages plus per-Kernel metadata. It
does not represent total system memory accounting for every memfd page, but it
rules out an eager five-times-128-MiB copy in this experiment.

### Native image path

Using the hermetic localhost registry fixture:

| Operation | Elapsed |
| --- | ---: |
| Cold pull, 789,280 bytes | 60 ms |
| Repeated pull with all blobs reused | 20 ms |
| First run, including rootfs materialization | 140 ms |
| Run with cached materialized root | 110 ms |

Localhost and a small static image make these figures useful for comparing TTOS
overhead, not public-registry performance.

## Performance findings

### P1: source downtime includes almost the complete clone

`SandboxFamily.Fork` pauses the source before `Kernel.LiveFork`, starts and
optionally resumes the child, and only then resumes the source. In the measured
real containers, source downtime was more than 99% of internal fork latency.

`Kernel.LiveFork` transitions pages to CoW but streams the complete savable
Kernel graph through `state.Save` and `state.Load`. Mutable pipe/socket queues,
task metadata, VFS metadata, and other objects are encoded and reconstructed.
Fork latency is therefore flat versus untouched resident RAM but grows with the
metadata graph.

Recommended work:

1. Split source capture from child restore so the source can resume after a
   self-contained metadata snapshot is captured.
2. Add a clone-specialized graph path that shares immutable metadata and copies
   only mutable objects, avoiding the general checkpoint codec where possible.
3. Benchmark independent sweeps of tasks, VMAs/PMAs, FDs, epoll edges, queued
   bytes, tmpfs extents, directory entries, and branch depth.

### P1: the native image store has one long-held exclusive lock

`openNativeStore` takes an exclusive `flock`. Pull keeps it across registry
resolution and downloads; rootfs materialization keeps it across decompression
and layer extraction. Image listing, lease changes, deletion, and GC use the
same lock. A slow registry or large layer can therefore block unrelated image
and family-preparation operations in the same data root.

Recommended work:

* fetch blobs outside the catalog lock using content-digest singleflight;
* use a short catalog transaction only to publish or retarget the image record;
* use per-rootfs-key construction locks and recheck the cache after acquiring
  the key lock; and
* keep family pin updates independent from unrelated registry traffic.

### P1: GC and catalog updates scale with the whole store

Each retag, image deletion, or family-pin release recursively marks descriptors
from every image and lease, walks every content blob, then deletes unreachable
content. Pull also walks content before and after transfer to calculate reuse.
The complete JSON catalog is rewritten, synced, renamed, and directory-synced
for each update.

This is robust for a small developer store but will add latency as images,
layers, aliases, and families accumulate. Maintain descriptor reachability or
reference counts incrementally, schedule nonessential GC outside foreground
commands, and compact or index the catalog before treating large stores as a
supported workload.

### P2: concurrent first use can extract the same image twice

The materialized-root cache is checked before extraction, but a second process
that missed the cache can wait for the global content lock and then extract
again without rechecking. The final rename detects the winner, so correctness is
preserved at the cost of redundant I/O and temporary disk use.

### P2: tmpfs and SysV SHM detach on first post-fork access

The current Mappable interface cannot safely install a writable translation
while invalidating every sibling translation in the same branch. Consequently,
shared tmpfs and SysV SHM pages conservatively detach on the first mapping fault,
including a read fault. Read-heavy workloads after fork can consume more memory
than write-fault-only CoW would require.

### P2: durable manager semantics add visible CLI latency

The external 128 MiB fork command took about 80 ms while internal timing averaged
37.5 ms. Process startup, Unix-socket transport, durable mutation bookkeeping,
metadata writes, and event recording account for the difference. The durability
is valuable, but batch/benchmark users would benefit from a long-lived client or
explicit multi-fork API that amortizes command setup and journal syncs.

## Compatibility findings

### C1: randomness is not branch-reproducible

Guest `getrandom`, `/dev/random`, and `/dev/urandom` read `pkg/rand.Reader`, a
process-global host-backed source. The source and child do not receive cloned
branch-local PRNG state, so their next random reads are neither equal nor
deterministically related.

This conflicts with a strict promise that every container-observable random seed
and stream is identical at the fork boundary. Before advertising deterministic
time travel, introduce a per-Kernel DRBG, serialize its state into the child,
and define whether the branches intentionally produce identical streams or
derive domain-separated streams after the first read.

### C1: fork compatibility is deliberately narrower than run compatibility

TTOS can run many resources that it refuses to fork. `fork --check` is the
authoritative runtime decision and fails before mutating the source.

Currently demonstrated as branch-local or safely cloned:

* processes, threads, PID namespaces, anonymous private mappings, and memfd;
* tmpfs, managed volumes, in-memory OverlayFS uppers, procfs, cgroupfs, devpts,
  restricted read-only sysfs, POSIX message queues, and namespace handles;
* SysV shared memory;
* anonymous pipes, eventfd, epoll graphs, timerfd, signalfd, and inotify;
* entirely internal Unix socket graphs without queued `SCM_RIGHTS`; and
* an empty or canonical loopback-only netstack, including internal UDP/TCP
  queues and connections.

Rejected or externally shared:

* host-network mode, external TCP/UDP links, hostinet/plugin stacks, secondary
  network namespaces, modified iptables/conntrack, and nftables;
* imported host FDs, external pipes, host eventfd, host-bound Unix sockets,
  host-backed peers, and queued `SCM_RIGHTS`;
* directfs and writable gofer filesystems;
* AIO ring, io_uring ring, PACKET_MMAP, and KCOV mappings;
* host/FUSE/device mappings retained after their FD closes;
* TPU/GPU/device proxy state and private filesystem MemoryFiles; and
* any anonymous object or filesystem that has not explicitly opted in.

Read-only host binds are shared external views. `immutable-source` is only an
attestation; it does not stop an independent host writer. Managed bind CoW also
assumes its external lower remains immutable for stable snapshot semantics.

### C1: rootless operation depends on host user-namespace policy

TTOS does not need subordinate ID maps, but normal isolation still needs the
host to allow a one-entry user namespace. Distribution LSM policy can deny this
even when the kernel sysctl appears enabled. The fallback
`--unsafe-no-host-isolation` is useful for trusted CI but must not silently
become a production default.

CPU and memory limit compatibility additionally depends on delegated cgroup v2.
TTOS fails closed when a requested hard limit cannot be enforced.

### C2: Docker compatibility is a CLI subset, not a Docker API

Implemented commands intentionally resemble Docker, but there is no Docker
Engine API, Compose API, swarm, bridge/NAT manager, port publishing, plugin
system, or general device pass-through. Supported TTOS networking is `none` or
`host`, and host mode cannot be live-forked.

Restart recreates an exited main branch from persistent configuration and the
immutable image. A stopped branch does not retain its in-memory Kernel or
uncommitted branch-local filesystem state. Managed-volume state must be
committed/imported or captured by the documented stop/exit preservation path.

### C2: one image reference stores one selected platform

Pull records are keyed by normalized image reference, not by
reference-plus-platform. Pulling the same tag for another platform retargets
that record and may collect the prior platform's blobs unless a family pin keeps
them reachable. Users who need simultaneous architectures should use distinct
digest references or separate data roots until the catalog key includes the
platform.

Docker schema 1 is explicitly rejected. OCI/Docker v2 Linux images, strict
platform selection, Docker config authentication, and credential helpers are
implemented. Image signatures, attestations, and content-trust policy are not.

### C2: uncommon layer metadata needs a larger rootless matrix

The public matrix verifies dynamic linking, image ownership through TTOS's
virtual ownership manifest, whiteouts, hard links, and common entrypoints. It
does not yet establish broad compatibility for images containing device nodes,
privileged security xattrs, or executable file capabilities that the invoking
host user cannot materialize. Add purpose-built OCI fixtures for these cases and
define whether unsupported metadata is rejected or represented virtually.

### C2: terminal exec teardown can linger

The real-PTY audit observed correct output and exit status from `exec -it`, but
the transport did not close until the 60-second test watchdog. This does not
affect noninteractive exec or the completed public-image matrix, but interactive
automation should treat it as an open defect.

## Recommended acceptance gates

Before calling TTOS a broadly compatible beta:

1. Implement and test branch-local randomness semantics.
2. Fix terminal exec EOF/teardown and make the real-PTY suite pass without a
   watchdog intervention.
3. Add task/VMA/FD/queue/filesystem scaling benchmarks with regression budgets.
4. Shorten source downtime by separating metadata capture from child restore.
5. Shard image-store locking and replace foreground full-store GC.
6. Add multi-platform catalog keys and concurrent pull/materialization tests.
7. Add OCI fixtures for capabilities, xattrs, sparse files, device entries,
   zstd layers, foreign-layer policy, and private registry authentication.
8. Expand the live-fork resource matrix only through explicit fail-closed
   validators and branch-isolation tests.

Until those gates pass, end users should run `ttos fork --check` immediately
before each fork, treat warnings as semantic differences rather than cosmetic
messages, and use TTOS primarily for trusted, self-contained test workloads.
