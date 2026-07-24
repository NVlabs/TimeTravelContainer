# TimeTravelContainer security, performance, and compatibility audit

This audit was refreshed on 2026-07-24 through configurable virtual-NIC
identity, line-oriented image progress, default guest SUID semantics, rootless
namespace preflight, orderly daemon shutdown, persistent writable-root restart,
and the private-memory and interactive-exec fixes. It evaluates the native OCI
image store, rootless execution, Docker-like CLI, in-memory live fork, and
TTC-specific changes to gVisor's security boundary. It is a manual
development-readiness review, not a security certification or a substitute for
continuous fuzzing and an independent adversarial audit.

## Executive assessment

TTC is suitable for controlled integration-test workloads whose resource graph can complete
`ttc fork` validation. It runs several nontrivial unmodified images and its
application-memory fork cost is effectively independent of resident byte count
in the measured range. It does not copy application RAM to disk or duplicate
all resident pages at fork time.

TTC is not a drop-in Docker daemon or a universally compatible Linux container
runtime. The biggest performance cost is full kernel-metadata serialization
while the source is paused.

Transparent relocation of systrap's platform-private stub arena is deliberately
deferred. It must not be introduced as a compact but discoverable mapping
without simultaneously replacing the global layout, immutable-filter, helper
lifecycle, and shared-state security assumptions. The complete future
implementation and security gates are recorded in the
[deferred systrap stub-reallocation design](timetravel_systrap_stub_reallocation.md).

The recommended readiness levels are:

| Use case | Assessment |
| --- | --- |
| Trusted integration tests with `--network none` or `ttc` | Ready for broader testing |
| Forking Python, shell, nginx configuration, or Redis-like local workloads | Demonstrated |
| General Docker CLI replacement | Partial |
| Parent external TCP continuity with child reset | Demonstrated; reconnect required in child |
| Replay of external nondeterministic inputs | Not a TTC guarantee |
| Untrusted production multi-tenancy | Known security findings; not recommended |

## Evidence

The following checks passed during this audit or the immediately preceding
native-store implementation verification:

* image, bundle, command, daemon, and live-fork focused Bazel tests;
* a hermetic Registry V2 pull, cache reuse, offline run, inspect, and deletion;
* the complete public-image matrix with BusyBox 1.36, Alpine 3.20, Python
  3.12-alpine, nginx 1.27-alpine, and Redis 7-alpine;
* exec, logs, attach, daemon restart, pause/resume, recursive fork, internal
  pipes and sockets, tmpfs, managed-volume CoW, and writable overlay CoW;
* a rootless in-process `ttc0` NIC, NAT-disabled isolation, TCP/UDP user-mode
  NAT, persistent parent-bound TCP publication, and child external-flow reset;
* the systrap `BenchmarkSandboxFamilyFork` benchmark; and
* real Python containers with 1 MiB, 128 MiB, and 256 MiB touched application
  allocations and multiple guest threads.

The July 24 security pass additionally reviewed TTC's daemon peer
authentication and state-directory permissions, OCI/image extraction and
virtual ownership, mount construction, SUID and capability setup, user-mode
NAT and published-port host access, Sentry seccomp additions, systrap syscall
patching, live-fork filesystem/FD/mapping/network validators, and the upstream
commits newer than TTC's merge base. Focused bundle, ownership, fsgofer, and
systrap usertrap tests passed. The review found the concrete issues below but
did not identify a TTC-specific path from a normal rootless guest to host code
execution or arbitrary unattached host-file access.

The original July 18 real-PTY run exposed a daemon transport EOF defect after
`exec -it` had already produced its expected output and exit status. The July 19
fix made daemon stdin pumping cancellable when the output side completes; the
real-PTY regression now requires `exec -it` to exit promptly. Focused tests also
cover preservation of application-modified private pages during live-fork
write-protection and custom virtual-NIC address/MAC propagation.

A loaded Chromium/VNC reproduction exposed a post-fork tmpfs lock inversion:
`pwrite()` from a mapping in one memory manager entered the file writer with
that manager read-locked, then branch CoW invalidation requested its write lock.
The affected task deadlocked, later tasks accumulated behind the same lock, and
a subsequent fork blocked in `Kernel.Pause()`. Shared-range syscall writes now
use a bounded bounce buffer so source-memory locks are released before mapping
invalidation. The focused regression checks the lock ordering and the real
validation forks Chromium after loading Google, Wikipedia, and Chromium.org,
with an authenticated VNC published-port connection active.

An independent, no-fork Chromium/VNC reproduction exposed a systrap syscall
patcher false positive. The patcher treated any trapped `syscall` whose
preceding seven-byte window began with `0xb8` as
`mov $sysno,%eax; syscall`, then rewrote the whole window. JIT-generated code
can contain that leading byte without containing the expected instruction
sequence, so Speedometer 3.1 corrupted a Chromium workload frame. Patching now
requires the complete opcode sequence and the immediate syscall number to
match the syscall that actually trapped. The fixed default fast path and the
`--systrap-disable-syscall-patching` control both render the workload normally;
the bundled Chromium 112 image still stops at 6/580 under both gVisor and
native Docker, which is a separate browser/benchmark compatibility issue.

## Security findings

### S1: non-root processes receive effective root-like default capabilities

This is a confirmed within-sandbox privilege escalation and policy bypass.
`BuildSpec` resolves the configured image user or `--user` override, but
`buildCapabilities` independently returns TTC's root-oriented Docker default
set. The generated OCI process then receives that set in its bounding,
permitted, and effective capability sets even when its UID is nonzero. The
existing bundle test demonstrates UID 123 with `CAP_DAC_OVERRIDE`,
`CAP_SETUID`, `CAP_SETGID`, and the other defaults effective.

For comparison, the cached Docker 29 installation was run with
`docker run --rm --user 123 busybox:1.36`; `/proc/self/status` reported zero
permitted and effective capabilities while retaining Docker's default bounding
set. TTC therefore differs materially from the CLI behavior it is emulating.

Sentry constructs credentials directly from those sets. Its `setuid(2)`
implementation permits an arbitrary UID transition when `CAP_SETUID` is
effective, so a process launched as UID 1000 can call `setuid(0)` and become
virtual root. No SUID executable is involved in this path: the process invokes
the syscall directly using its already-effective capability. Even without
changing UID, `CAP_DAC_OVERRIDE` bypasses ordinary guest file permissions.
`ttc exec --user ...` has the same problem: upstream runsc derives exec
capabilities from the container process spec before applying the requested
exec UID.

This does not grant a host UID or host capability; the escalation remains
inside the Sentry. It nevertheless violates TTC's stated within-sandbox
credential boundary and Docker-shaped `--user` expectations.

Required remediation:

1. Make initial-process capability construction UID-aware. A non-root process
   should not receive default permitted/effective capabilities merely because
   no capability flags were supplied.
2. Make `ttc exec --user` recompute the default capability sets for the
   requested UID rather than inheriting a root container's effective set.
3. Define and test the interaction of explicit `--cap-add`/`exec --cap`,
   bounding capabilities, ambient/inheritable sets, SUID, and
   `noNewPrivileges`.
4. Add runtime tests proving that a default non-root process and a non-root
   exec receive `EPERM` from `setuid(0)` while explicitly authorized
   configurations and UID 0 retain their intended behavior.

### Resolved S2: oversized numeric IDs could resolve to image names

The audit confirmed a within-sandbox user-policy bypass when an untrusted
image was combined with a supposedly numeric operator override. TTC resolves
OCI users with `moby/sys/user` v0.4.0. That implementation uses
`strconv.Atoi` to classify a user field as numeric; a decimal value larger
than the host `int` range produces an error and is then treated as a username.
A crafted image could place that exact digit string in `/etc/passwd` with UID
0. Consequently, `ttc run --user <oversized-decimal> ...` resolved to virtual
root instead of rejecting the out-of-range numeric UID. Explicit primary and
additional groups had analogous numeric-versus-name ambiguities.

An image's own `USER` field alone is not an additional security boundary
because the image already controls it. The relevant case is an operator or
higher-level policy using a numeric `--user` override to constrain an
untrusted image. This is the same vulnerability class as containerd's
[CVE-2026-46680](https://github.com/containerd/containerd/security/advisories/GHSA-fqw6-gf59-qr4w),
although TTC reaches it through its own user-resolution path rather than
containerd's CRI/OCI-spec code.

This is fixed. TTC now recognizes signed decimal syntax before consulting
image-controlled passwd/group files and requires numeric user, primary-group,
and additional-group IDs to be within the range supported by the resolver,
`0..2^31-1`. Overflow and negative values return a range error without name
lookup. Focused tests place the overflowing decimal string in both
`/etc/passwd` and `/etc/group` with ID 0 and verify rejection for all three
entry points; the maximum supported numeric UID/GID remains accepted.

### Resolved S2: missing upstream non-dumpable exiting-task fix

At audit time, TTC's upstream merge base was `3cc5535a1255` and the branch was
32 upstream commits behind `origin/master`. One intervening upstream commit,
`0f6b7d133f5b` ("Fix ptrace dumpability bypass check on exit"), fixes a
concrete access-check race. Before that fix, an exiting task clears its memory
manager, causing `canTraceStandard` to skip the task's non-dumpable check.
Another same-credential process can race `pidfd_getfd` and acquire a file
descriptor from the non-dumpable exiting task. Upstream added a focused
100-iteration regression for this case.

This was an inherited same-sandbox confidentiality/integrity issue, not a
demonstrated host escape. TTC has cherry-picked the exact upstream fix as
`eaef285a5`. The task's cached dumpability bit is normal saved state, so TTC's
state-stream live fork includes it without a separate unsaved resource.
Upstream's `PidfdGetfdNonDumpableExitingRace` regression passes under both
systrap directfs and shared-filesystem variants.

### Reviewed boundaries without a concrete escape finding

- `ttcd` accepts only Unix peers whose `SO_PEERCRED` UID equals the daemon's
  effective UID. Its socket parent must be owned by that UID with no
  group/other permissions, and the socket is mode `0600`. Same-UID control is
  the daemon's intended authorization boundary.
- Generated names and selectors reject path separators and traversal; private
  container, image, volume, journal, console, and event subdirectories/files
  use restrictive modes. No guest-controlled host-path traversal was found in
  these paths.
- Image layers use containerd v2.1.5's bounded archive extraction with
  `WithNoSameOwner`. The critical containerd image-unpack escape
  [CVE-2025-47290](https://github.com/containerd/containerd/security/advisories/GHSA-cm76-qm8v-3j95)
  affected only v2.1.0 and was fixed in v2.1.1. TTC's virtual ownership
  manifest paths are canonicalized and image roots are served read-only below
  an in-memory overlay.
- Writable host binds are rejected. Ordinary host binds are read-only shared
  external views, while managed CoW binds require an explicit immutable-source
  attestation. Directfs remains disabled in TTC's normal runsc arguments.
- SUID/SGID transitions occur in Sentry credentials and do not grant host
  credentials. They are intentional compatibility behavior; the effective
  default-capability defect above is separate and unintended.
- TTC NAT broadens the Sentry's host seccomp surface only for IPv4/IPv6
  TCP/UDP sockets. `--nat` intentionally allows outbound host networking and
  maps guest same-subnet aliases to host `127.0.0.x`; this is an explicit
  resource grant, not an implicit property of NAT-disabled `--network ttc`.
- Live fork fails closed for unrecognized file descriptions, mappings,
  filesystems, and network stacks. Host/directfs/FUSE resources, queued
  `SCM_RIGHTS`, AIO/io_uring and packet rings, host-bound Unix endpoints, and
  unvalidated network configuration are rejected. The reviewed opt-in
  validators recursively cover epoll and internal Unix endpoint graphs and
  reject host-backed eventfd resources.
- The systrap change tightens syscall-patch matching to an exact instruction
  sequence and does not add host syscalls or relax a seccomp rule.

Deliberate exclusions are the lack of isolation between branches in one
live-fork family, explicitly attached read-only host views and network
endpoints, and all defense-in-depth claims in
`--unsafe-no-host-isolation` mode. Resource-exhaustion limits, side channels,
and a complete re-audit of unchanged upstream gVisor were also outside this
focused pass.

## Performance measurements

The measurements below are retained from the July 18 run and were not repeated
for this documentation refresh. The host was an x86-64 Intel Xeon Platinum
8488C system. The host policy blocked unprivileged user namespaces, so
real-container measurements used `--unsafe-no-host-isolation`. These are
developer-build, single-host baseline numbers, not release guarantees.

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

Localhost and a small static image make these figures useful for comparing TTC
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

### Entropy behavior is intentionally nondeterministic

Guest `getrandom`, `/dev/random`, and `/dev/urandom` read the host-backed
`pkg/rand.Reader`. Their future outputs need not match across branches, just as
two post-fork Linux processes are not entitled to receive identical results
from later kernel entropy requests. This is not a live-fork compatibility
defect.

PRNG state that exists in application memory is cloned with that memory, so a
user-space generator that does not reseed may initially produce the same stream
in both branches. Applications that require independent streams remain
responsible for reseeding, as with ordinary process fork. TTC does not promise
replay of external nondeterministic inputs.

### C1: fork compatibility is deliberately narrower than run compatibility

TTC can run many resources that it refuses to fork. `ttc fork` is the authoritative runtime decision: it pauses the source,
validates before mutation, and resumes it on rejection.

Currently demonstrated as branch-local or safely cloned:

* processes, threads, PID namespaces, anonymous private mappings, and memfd;
* tmpfs, managed volumes, in-memory OverlayFS uppers, procfs, cgroupfs, devpts,
  restricted read-only sysfs, POSIX message queues, and namespace handles;
* SysV shared memory;
* anonymous pipes, eventfd, epoll graphs, timerfd, signalfd, inotify, pidfds,
  and Sentry-owned netlink sockets;
* entirely internal Unix socket graphs without queued `SCM_RIGHTS`; and
* an empty or canonical loopback-only netstack, including internal UDP/TCP
  queues and connections; and
* the TTC virtual NIC and an unmodified network configuration. Internal flows
  clone, while external child TCP flows reset and the source stays connected.

Rejected or externally shared:

* host-network mode, hostinet/plugin stacks, secondary network namespaces,
  modified iptables/conntrack, and nftables. TTC user-mode NAT and published
  TCP flows are handled by reset-on-child policy instead of rejection;
* imported host FDs, external pipes, host eventfd, host-bound Unix sockets,
  host-backed peers, and queued `SCM_RIGHTS`;
* directfs and writable gofer filesystems;
* AIO ring, io_uring ring, PACKET_MMAP, and KCOV mappings;
* host/FUSE/device mappings retained after their FD closes;
* TPU/GPU/device proxy state and private filesystem MemoryFiles; and
* any anonymous object or filesystem that has not explicitly opted in.

"Host FD" in this list means a file description whose behavior is owned by the
host kernel and has been imported into the guest graph: for example host stdio,
a host eventfd, socket, pipe endpoint, device, or a directfs file description.
It does **not** include an ordinary read-only host bind in the same category.
Read-only binds use a LISAFS gofer client with a stable resource identity;
`directfs=false` lets the child borrow that read-only client explicitly.

Read-only host binds are shared external views. `immutable-source` is only an
attestation; it does not stop an independent host writer. Managed bind CoW also
assumes its external lower remains immutable for stable snapshot semantics.

Directfs bypasses gofer RPCs by exposing host file descriptions more directly
to the Sentry. Duplicating such an FD normally shares the host open-file
description, offset, locks, and external mutations rather than creating a
branch-local clone, so TTC disables directfs in its normal runsc arguments.

Queued `SCM_RIGHTS` means an internal Unix socket has an unread ancillary
message carrying one or more FDs. Ordinary queued bytes clone correctly, but
the rights message adds graph edges to arbitrary file descriptions. Supporting
it requires cloning those edges with correct aliasing, cycles, reference counts,
and fork validators; this is feasible for fully Sentry-owned targets but is
not implemented yet.

AIO and io_uring expose memory-mapped submission/completion rings and may retain
pending operations, registered buffers, files, and worker state. Their mapped
control pages do not yet receive branch-local backing. A safe implementation is
possible, but it must first quiesce submissions and then clone or reject each
pending operation according to whether all of its resources are Sentry-owned.
External in-flight I/O cannot be duplicated transparently.

### C1: rootless operation depends on host user-namespace policy

TTC does not need subordinate ID maps, but normal isolation still needs the
host to allow a one-entry user namespace. Distribution LSM policy can deny this
even when the kernel sysctl appears enabled. The fallback
`--unsafe-no-host-isolation` is useful for trusted CI but must not silently
become a production default. It is therefore a daemon-start setting rather
than a normal per-command setting: one daemon serves all requests in one fixed
mode and reports that mode through `daemon status`. Clients do not send or
negotiate isolation mode, and normal commands require the user to start the
daemon explicitly.

CPU and memory limit compatibility additionally depends on delegated cgroup v2.
TTC fails closed when a requested hard limit cannot be enforced.

### C1: guest identity is virtual, but image metadata coverage is finite

Image layers are extracted as the invoking host user while TTC records their
OCI UID/GID values in a sidecar manifest. The LISAFS gofer substitutes those
values in guest-visible metadata, and writable overlay ownership remains Sentry
state. This is why arbitrary image users do not require subordinate host IDs.

TTC also enables Sentry SUID/SGID exec semantics by default for compatibility
with helpers such as `sudo`; OCI `noNewPrivileges` still prevents elevation.
This changes only emulated guest credentials. It does not grant host privilege
to the Sentry or gofer, and it does not fill the uncommon xattr, file-capability,
or device-node coverage gaps described below.

### C2: Docker compatibility is a CLI subset, not a Docker API

Implemented commands intentionally resemble Docker, but there is no Docker
Engine API, Compose API, swarm, named networks, a host bridge/CNI manager,
plugin system, or general device pass-through. TTC does provide its own
rootless `ttc` virtual-NIC mode, optional user-mode NAT, and persistent TCP
port publication, but this is not the Docker network API. Host mode cannot be
live-forked.

Restart recreates an exited main branch from persistent configuration, the
immutable image, and the last atomically exported writable-rootfs upper layer.
The main branch exports that upper layer after init exits, including shutdown
handler writes. A stopped branch does not retain its in-memory Kernel; child
branch filesystem changes are not promoted automatically. Managed-volume state
must be committed/imported or captured by the documented stop/exit preservation
path.

### C2: current gVisor networking and the TTC subset

Upstream runsc has four internal network selections; this TTC fork adds a
fifth, `ttc`:

* `none` creates netstack with canonical loopback only;
* `sandbox` imports interfaces and routes from a network namespace prepared by
  Docker, CNI, or another external network manager;
* `host` uses hostinet and redirects network operations to the host stack; and
* `plugin` delegates to a third-party stack; while
* `ttc` creates a branch-local in-process link between guest netstack and a
  user-mode gateway/proxy.

In `sandbox` mode, gVisor implements IP, TCP, UDP, ICMP, socket queues, and
firewall behavior in userspace. Docker or CNI remains responsible for creating
the veth/bridge topology, assigning addresses, NAT, and publishing ports. TTC
does not use that topology for `--network ttc`: it creates `ttc0` at
`10.0.2.15/24`, an in-process `10.0.2.2` gateway, optional IPv4 TCP/UDP host
proxying, and rootless host TCP listeners for `-p`. `--ip` may replace the
guest address and derives the gateway at `.2` in the selected `/24`;
`--mac-address` may replace the default guest MAC. Both identities persist
through restart and live fork.

Live fork accepts canonical loopback and an unmodified TTC virtual-NIC stack.
The child gets a new link and proxy realm. Internal TCP/UDP endpoints and queues
clone; established or connecting child TCP endpoints that terminate in NAT or
a published-port proxy are reset. The parent retains its proxy realm, existing
connections, and published listener. A child is not published automatically;
`ttc port publish CONTAINER@BRANCH ...` can assign an independent persistent
listener to any live child after the fork.

Host relay goroutines register channel-backed waiters while port-forwarded
traffic is active. The port-forwarding code marks these registrations explicitly
as runtime glue, so the live-fork snapshot filters them out without
unregistering them from the source queue. No listener type is globally assumed
to be transient: guest blocking operations also use channel waiters. Persistent
Sentry waiters (for example epoll interests) are cloned normally, while the
child's fresh proxy realm installs its own relay waiters. This permits a loaded
Chromium branch to fork without trying to serialize host Go channels and keeps
the source relay uninterrupted.

This gives useful fail-soft semantics without pretending that one host TCP flow
can be duplicated. It does not replay bytes arriving from outside during the
pause window or preserve a child's external TCP session. Explicit branch port
bindings provide stable public endpoints and are reconciled after daemon
restart. NAT is presently IPv4 TCP/UDP; publication is IPv4 TCP only. Modified
iptables/conntrack, nftables, host mode, secondary
network namespaces, external sandbox links, and plugin stacks still reject.

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

The public matrix verifies dynamic linking, image ownership through TTC's
virtual ownership manifest, whiteouts, hard links, and common entrypoints. It
does not yet establish broad compatibility for images containing device nodes,
privileged security xattrs, or executable file capabilities that the invoking
host user cannot materialize. Add purpose-built OCI fixtures for these cases and
define whether unsupported metadata is rejected or represented virtually.

### Resolved regression: terminal exec teardown

The July 18 audit observed a lingering `exec -it` transport after the guest had
exited. The July 19 daemon transport fix cancels the stdin pump when the output
side completes. The real-PTY suite retains a 60-second whole-command timeout as
a deadlock guard; reaching it is now a test failure, not accepted behavior.

## Recommended acceptance gates

Before calling TTC a broadly compatible beta:

1. Resolve the remaining effective-capability finding, including runtime
   exploit regressions for initial and exec non-root credentials. Retain the
   numeric-ID and upstream exiting-task regressions already added.
2. Establish a routine upstream merge and dependency-advisory cadence so
   published gVisor and image-library security fixes cannot remain unnoticed.
3. Add task/VMA/FD/queue/filesystem scaling benchmarks with regression budgets.
4. Shorten source downtime by separating metadata capture from child restore.
5. Shard image-store locking and replace foreground full-store GC.
6. Add multi-platform catalog keys and concurrent pull/materialization tests.
7. Add OCI fixtures for capabilities, xattrs, sparse files, device entries,
   zstd layers, foreign-layer policy, and private registry authentication.
8. Expand the live-fork resource matrix only through explicit fail-closed
   validators and branch-isolation tests.

Until those gates pass, end users should treat a failed `ttc fork` as an
authoritative compatibility result, review its resource diagnostics, and use
TTC only for trusted, self-contained test workloads. Non-root guest identity is
not currently a security boundary.
