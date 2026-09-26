# TTC elastic cluster qualification

This page records reproducible qualification evidence for elastic TTC. It is
not a general performance claim: loopback nodes share CPUs and the NFS server,
whereas production nodes do not.

## Implemented behaviors under test

- lease-bearing dynamic node join and discovery without static peer lists;
- a unified branch namespace and daemon-to-daemon command routing;
- least-loaded compatible routing for new container admission;
- fenced, identity-preserving migration of one branch while siblings remain on
  their original node;
- daemon-incarnation adoption after a clean restart;
- node cordon/drain, cold-branch transfer, and page-dependency draining;
- deterministic pressure placement and opt-in automatic rebalancing;
- reduced global fixed-terminal placement across several destinations;
- fenced, expiring target-memory reservations for pressure moves;
- coalesced durable capacity requests, executable/webhook delivery, persistent
  retry state, and membership-confirmed fulfillment;
- durable drain and per-branch migration recovery after daemon process loss;
- callback-safe routing of node-wide mutations; and
- remote live fork with 1 MiB bulk memory transfer through a shared NFS root.

## Local mixed-family migration

A BusyBox family was forked locally, its child was migrated to the second
daemon, and both branches were exercised through direct and remotely routed
clients. The child was then drained back to the first node. Its visible
`family@child` identity, parent display metadata, filesystem marker, process
counter, and running state survived. At no point did both daemons report a
runnable owner for the same branch.

Observed conservative cutovers were 354--483 ms on this host. A return
migration invoked through the non-owner daemon completed in 409 ms. These
figures include dehydration, ownership CAS, remote rehydration, and command
routing; they are not the future live sparse-migration target.

This test also found and fixed two elastic-specific recovery bugs:

1. startup reconciliation on a node with no local Sentry marked remotely owned
   siblings exited; reconciliation is now branch-owner scoped; and
2. a placement retained the previous daemon incarnation after restart;
   adoption now increments its fencing generation before another migration.

## Drain recovery after daemon loss

A five-branch BusyBox family was cordoned on `node-a` while no destination
existed. Drain failed safely, emitted one capacity request, and retained a
durable intent. The `ttcd` process was then killed with `SIGKILL`; its Sentry,
gofer, console followers, and guest counters continued running. After `node-b`
joined and the old three-second lease expired, restarting `node-a` with a new
incarnation admitted it directly as `draining`. The background reconciler
migrated all five branches without another client command.

Every branch was reported on `node-b`, an exec routed through the restarted
`node-a` observed its counter advancing, and the intent transitioned to
`complete`. A clean stop marked `node-a` offline and removed the completed
intent. Removing the family and stopping `node-b` left no test processes or
directory.

The same crash/rejoin sequence was repeated with the entire TTC data root on a
kernel NFSv3 mount (`hard`, TCP, 1 MiB reads/writes, synchronous export, no root
squashing). Atomic intent replacement, advisory locking, lease expiry,
incarnation fencing, automatic three-branch evacuation, routed execution, and
completed-intent consumption all passed. The NFS mount, export, daemons,
Sentries, followers, and temporary roots were removed afterward.

### Partial-cutover and repeated-crash recovery

A 13-branch BusyBox family ran a 128 MiB prefaulted memory workload while a
drain moved it between two dynamically discovered daemons. The source daemon
was killed after six ownership records had changed. One branch was caught
after its placement entered `migrating` and its durable dehydration completed,
but before the ownership compare-and-swap. On restart, the new source
incarnation restored that branch while it remained fenced, atomically rolled
the transaction back and adopted the placement, then resumed the drain.

The reverse drain was interrupted after ownership had moved to the target but
before the source received a successful response. After the old source lease
expired, the target daemon rehydrated and committed that exact operation. A
third kill after five additional committed branches verified that a durable
drain resumes from the remaining ownership set rather than replaying completed
moves. All 13 placements finished stable on one node and routed `exec true`
succeeded for every branch.

This test also exposed a distributed lifecycle-lock inversion: a client entry
daemon proxied `node stop --migrate-all` while holding its own mutation lock, and the source
called that daemon back to rehydrate a target. Node-wide mutations are now
routed to their source before any local mutation lock is taken. A final
13-branch reverse drain invoked through the destination completed in 25.932
seconds. Cleanup removed the family, both daemons, Sentries, gofers, console
followers, durable intents, and the test root.

### Plaintext cluster interface and two-node retirement

On 2026-09-08, a fresh two-node cluster was bootstrapped through the simplified
`--cluster-init` / `--cluster-join` interface with one shared root, independent
local roots, and `TTC_CLUSTER_TOKEN`. A join using the wrong token was rejected.
BusyBox was then placed explicitly on node B, forked locally through automatic
placement, and both live branches were evacuated by running `ttc node stop
node-b --migrate-all` through node A. Both branches retained a guest-written
file, node B became offline, its socket disappeared, and node A retained Raft
quorum and served both branches.

This run exposed and fixed three shutdown-path defects: plaintext Raft join
still required the old TLS address encoding; closing the local daemon listener
could race the final proxied response and return EOF; and a completed capacity
reservation incorrectly prevented the source voter from retiring. The test
was repeated from a fresh root after all three fixes. The surviving daemon was
then stopped with `ttc node stop node-a --stop-all`, and all temporary runtimes
and storage were removed.

### Single-port control/Raft multiplexing and member reuse

On 2026-09-08, the same qualification was repeated after HTTP control and Raft
were collapsed onto one TCP listener. Node A advertised and listened only on
port 17643 and node B only on port 17653; `node ls --json` reported each exact
port as both `control_url` and `raft_address`, and no adjacent listener existed.
A join with the wrong `TTC_CLUSTER_TOKEN` failed before admission.

A continuously updating BusyBox branch was migrated away while retiring the
Raft leader. Its counter continued advancing on the survivor, the retired
listener disappeared, and the retired node's local Raft database was removed.
That node then rejoined through the survivor on its original single port, took
the branch back during a second node retirement, and preserved the advancing
counter. The test exposed and fixed a lifecycle defect in which a cleanly
removed member retained obsolete local Raft state and could not later rejoin.
Both retirement directions and all test storage and processes were cleaned up.

## Pressure placement and cleanup

### Three-node fixed-terminal retirement

A five-branch family held a 128 MiB prefaulted anonymous mapping. Branch `b`
overwrote the first 32 MiB and branch `c` overwrote the next 32 MiB; `b2` and
`c2` were then forked from those differentiated states. `b` migrated to
`node-b`, `c` migrated to `node-c`, and `main`, `b2`, and `c2` remained on
`node-a`.

Draining `node-a` built one global fixed-terminal problem. It placed `b2` with
`b` on `node-b`, `c2` with `c` on `node-c`, and the remaining `main` branch on
`node-b`. All five routed exec checks passed after cutover. The 136 MiB
resident branch on `node-b` required only 27 stable version runs in its planner
response. Family removal, image removal, and all three daemon stops left no
test Sentry, socket, or temporary root.

The ordinary two-daemon capacity-provider smoke also passed with the new
retirement planner in 7.165 seconds. The same test completed in 11.684 seconds
with its data root on a loopback NFSv3 mount using hard TCP, 1 MiB reads/writes,
synchronous export, and no root squashing. NFS qualification exposed a capped
diagnostic writer that padded short provider errors to 4 KiB with NUL bytes;
the writer now copies only the bytes supplied and has a regression test.

A 64 MiB prefaulted process was forked into two fully shared branches on a
pressured source. The planner selected both siblings as one zero-cut group,
estimated 67,493,888 bytes freed/added, migrated them to the ready target, and
preserved routed execution for both names. After the last local branch moved,
the source Sentry and gofer were absent; stopping the now-empty source daemon,
removing both target branches, and stopping the target left no matching daemon,
Sentry, gofer, listener, or test directory.

That cleanup assertion found and fixed an owner-scope error where a running
remote sibling prevented deletion of an otherwise empty source Sentry.

The no-target path was also exercised at unit level: concurrent pressure/drain
attempts retain one durable request ID. Command and HTTP adapters preserve that
ID across failed delivery and acceptance; TTC waits for a fresh, compatible,
ready node with sufficient unreserved memory before consuming the request.

Target reservation concurrency was exercised against the shared node catalog:
two sources simultaneously requested the only remaining 2 GiB on an 8 GiB
target already using 6 GiB. Exactly one reservation succeeded. An exact retry
did not double-charge the target, heartbeat preserved the reservation, and its
TTL reclaimed it after simulated source loss. Planner execution now installs
this reservation before its first migration; completion marks it releasable
only by the target's next fresh heartbeat, closing the stale-heartbeat
overcommit window even when loopback nodes share one host cgroup.

A live two-daemon run prefaulted 256 MiB, forked it into three BusyBox
branches, and pressured the source. The exact-edge planner selected two
siblings as a zero-cut group, estimated 269,606,912 bytes freed and added, and
published an equal 269,606,912-byte reservation on `node-b` before moving the
first branch. The migration completed in 7.6 seconds and all three branch
records remained running on the target. Because both loopback nodes share one
host cgroup, source release can make aggregate RSS fall during target restore;
the completed-operation plus next-heartbeat rule was added and unit-tested to
cover that case. Both daemons, the family, Sentry, gofer, and temporary root
were removed after the run.

New-work admission was also exercised through a deliberately pressured entry
node. A detached BusyBox container was transparently created on the less-loaded
peer, its branch owner metadata named that peer, and `exec` through the entry
node succeeded. The test initially exposed a family-removal routing bug that
could delete shared metadata while leaving the remote Sentry alive.

After the fix, a two-branch family was split by identity-preserving migration.
Both the local and remote branch remained executable through node A. A single
`ttc rm -f FAMILY` issued to node A removed the owner-local branches, purged
the former source runtime, delegated final metadata removal to node B, and
left no container record, Sentry, gofer, or runtime directory. This test also
found and fixed two physical-versus-logical counting assumptions: migration
had retained a dehydrated source shell, and branch removal could ask runsc to
destroy the last physical branch merely because remote logical siblings still
existed.

The split family was then exercised through the full family lifecycle. One
command dehydrated both owners; each branch was independently rehydrated and
executed through node A; one family-wide stop left both branches `exited`; and
ordinary family removal reclaimed every record and process. The stop test
found and fixed the final-physical-branch case where a successful init exit
closed runsc's control socket before its wait reply arrived.

## Capacity-provider join

[`tools/ttc/elastic_capacity_smoke.py`](../../tools/ttc/elastic_capacity_smoke.py)
starts one dynamically discovered daemon and a real BusyBox counter workload,
forks the workload into two live branches, then drains the only node. With no
destination, TTC emits a durable capacity request. The executable provider
injects one scheduler outage, then accepts the retry. Both calls carry the same
request ID. Only after a compatible second daemon joins with enough unreserved
memory does TTC consume the request; the outstanding drain then resumes without
another client command.

After live sparse migration replaced the earlier dehydration-based cutover, the
local run completed in 7.268 seconds with two provider attempts and two
identity-preserving branch migrations. This wall time includes the injected
provider failure, retry backoff, dynamic daemon startup, both cutovers, and
source-dependency cleanup. Both counters continued through routed execution on
node B. The target's cold bootstrap retained the migrated visible branch name
instead of silently becoming `main`. Family removal, daemon shutdown, and the
smoke harness removed all Sentries, gofers, sockets, request state, and test
directories.

The same capacity-provider/drain smoke was then run with its shared data root
on a loopback kernel NFSv3 mount (`hard`, TCP, 1 MiB reads/writes, `sync`, no
root squashing) and separate node-local runtime directories. Both live sparse
migrations, placement-generation fencing, catalog snapshot locks, provider
retry, routed counters, and cleanup passed in 11.922 seconds. Removing the
family reclaimed every placement JSON record; only the bounded catalog and two
hash-shard lock files existed before the test mount and export were removed.

Putting the daemon sockets and runsc runtime roots on the NFS mount is an
invalid layout: a later sibling import can observe unsafe live-runtime
semantics rather than shared-repository semantics. TTC now detects NFS at
daemon startup and rejects that layout with instructions to keep only
`TTC_ROOT` shared and use a distinct node-local `TTC_LOCAL_ROOT`.

### Explicit-root and tiered-storage regression

After separating the root-unaware client from daemon storage configuration,
the capacity smoke was rerun with one shared `TTC_ROOT`, two distinct
node-local `TTC_LOCAL_ROOT` values, and independent `TTC_SOCKET` paths. A
two-branch BusyBox drain completed in 7.501 seconds, including one injected
capacity-provider failure and retry. A nontrivial Ubuntu 24.04 run created
1,000 files and a tar archive before migration, then verified every file and
wrote and synchronized 8 MiB independently in both branches; it completed in
10.044 seconds (0.261 seconds preparation, 0.082 and 0.169 seconds branch
verification).

The BusyBox case also passed with the shared root on a loopback NFSv3 mount in
9.01 seconds. A heavier NFS attempt exposed a race between eager dependency
drain and sparse target handoff: the eager drain could make the transaction
source-independent before the target registered a sparse descriptor, causing
the target to retry forever. The target now recognizes that fenced state as a
completed handoff. The heavy Ubuntu case passed locally after the fix; the
follow-up loopback-NFS run was not used as evidence because the local kernel
NFS server itself stalled during remount. Earlier Rocky Linux and repeated
crash/rejoin NFS results above continue to cover the full NFS workload path.

Later qualification replaced the host-kernel server with NFS-Ganesha and
covered both NFSv3 and NFSv4.2. An unshaped NFSv4.2 matrix forked four 128 MiB
siblings in 1.991 seconds and completed in 24.287 seconds; a random sparse
two-sibling variant forked in 1.702 seconds and completed in 14.672 seconds.
A 15±3 ms, 100 Mbit/s run completed in 101.280 seconds, including 20.850
seconds for remote fork, 15.973 seconds for dehydration, 13.555 seconds for
rehydration, and 62.562 seconds to make every remote dependency independent.

The final shaped regression used hard NFSv3 with 1 MiB reads/writes and
20±5 ms, 50 Mbit/s loopback shaping. The source kept 128 MiB resident in
`/dev/shm`, created a 64 MiB rootfs object and 2,000 small files, forked two
branches, and drained to a dynamically joined node. Each target then verified
the inherited files and independently wrote 32 MiB to rootfs and 32 MiB to
`/dev/shm`. It passed in 236.705 seconds; preparation took 17.336 seconds and
target verification took 2.370 and 2.948 seconds. The traffic shaper, family,
daemons, NFS test tree, and local roots were all removed afterward.

A controlled shared-storage outage then suspended NFS-Ganesha for eight
seconds during migration while node leases remained valid. Both branches
completed migration and verification in 20.805 seconds. This distinguished a
recoverable repository stall from source loss; a separate trial that exceeded
the deliberately short lease correctly fenced the expired source instead of
allowing two owners.

An earlier shaped run also caught a target-cleanup race: the background
reconciler could observe a prepared target before placement cutover, conclude
that its local runtime was unowned, and terminate it. Provisional distributed
targets are now protected by their fenced import transaction until cutover or
explicit cancellation. The full 128 MiB/4,000-file, 20±5 ms, 50 Mbit/s rerun
then passed in 596.508 seconds, including independent 64 MiB writes in both
target branches.

A follow-up cutover-hardening run completed locally in 7.306 seconds. It
verified that a prepared migration remains physically paused and hidden while
placement still names the source, then publishes console/exec sidecars and
resumes the target only after the ownership CAS. This run caught an attempted
pre-cutover sidecar lookup that the new generation fence correctly rejected;
moving sidecar activation behind the CAS removed the retry loop.

The smoke was also expanded from a counter into a nontrivial Rocky Linux 8.9
network and filesystem workload. The source ran `yum makecache` over TTC NAT,
then both branches were migrated and independently installed
`adwaita-icon-theme`, checked an installed path, and wrote and synchronized a
32 MiB file. The run passed in 52.395 seconds: source preparation took 24.434
seconds and post-migration verification took 9.775 seconds for `main` and
8.230 seconds for `child`. This test exposed and fixed two issues that the
counter did not reach: repeated logical chunks could be reordered by their new
physical offsets during a second sibling import, and the first restored TTC
network stack on a cold target retained an obsolete runtime link until it was
reattached after Sentry startup.

Run it with:

```shell
python3 tools/ttc/elastic_capacity_smoke.py --ttc /path/to/ttc
```

To keep runtime state local while qualifying a shared repository, use separate
paths explicitly:

```shell
python3 tools/ttc/elastic_capacity_smoke.py \
  --ttc /path/to/ttc \
  --work-dir /node-local/ttc-smoke \
  --data-dir /shared/nfs/ttc-smoke-data
```

## Exact CoW hypergraph placement

The live qualification worker in
[`tools/ttc/elastic_memory_worker.c`](../../tools/ttc/elastic_memory_worker.c)
prefaulted 128 MiB of anonymous memory before a three-way local fork. Branch
`a` then overwrote the first 16 MiB and branch `b` independently overwrote the
next 16 MiB. The Sentry exported three exact sharing edges:

| Branch membership | Resident bytes |
| --- | ---: |
| `a`, `b`, `main` | 102,354,944 |
| `a`, `main` | 16,777,216 |
| `b`, `main` | 16,777,216 |

Per-branch private/shared totals reconciled exactly with these edges. Under an
artificial 100 MiB pressure deficit, the planner correctly selected all three
branches: moving a strict subset would leave the large shared edge resident on
both nodes. It estimated 170,090,496 bytes freed and added with a zero-byte cut.
After conservative migration to the second daemon, routed execution succeeded
and the target Sentry reported the same three edges and byte totals, proving
that migration preserved the CoW topology. Removing the family and stopping
both daemons left no workload, Sentry, gofer, or test directory.

### High-fanout tmpfs pressure

The pressure regression was extended to eight running sibling branches. Each
branch independently dirtied a 48 MiB `/dev/shm` file, so accounting had to
include linked tmpfs pages that were no longer mapped by a task and deduplicate
them against mapped pages by their stable physical page-version identity. The
source reported at least 384 MiB resident, crossed its configured target, and
the scheduler moved a CoW-aware group across the two accepting peers. Every
branch and every independently written tmpfs payload remained readable after
migration.

The original exact representation expanded to roughly 2.6 MiB when sibling
writes interleaved allocator sequence numbers. TTC now sends one 256-version,
32-byte bitmap per allocator window. The same run kept the complete planner
payload below 512 KiB and completed with one migration. The 4,096-entry
sharing-edge cap remains only on the diagnostic summary; planner correctness
does not depend on that summary.

## NFS remote-fork matrix

The test data root was exported by the host kernel NFS server and mounted over
loopback as NFSv3 with hard TCP mounts, 1 MiB read/write sizes, synchronous
server export, and no root squashing. Two dynamically discovered daemons used
node-local runtime roots and the NFS-mounted shared data root. The benchmark
used BusyBox 1.36 and 1 MiB TTC memory transfers.

| Workload | Memory | Fan-out | Fork | Workload | Source-independent | Wall | Result |
| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| Sequential, two rounds | 128 MiB | 4 | 1,864 ms | 1,566 ms | 4,523 ms | 11,330 ms | pass |
| Random, 25% touched, three rounds | 128 MiB | 2 | 1,045 ms | 593 ms | 2,056 ms | 3,009 ms | pass |

All six targets passed their workload and routed-command checks. Every
transaction reached `independent`, all outstanding pages reached zero, and no
daemon, listener, export, mount, or benchmark directory was retained after
cleanup. Sequential targets each pulled 129 faults / 130 requests and about
134.8 MB; random targets each used 89 faults and about 134.6 MB.

A separate 64 MiB sequential run dehydrated and rehydrated the remote target
before executing it. It completed in 5.36 seconds overall (352 ms fork,
1.45 seconds dehydration, 327 ms rehydration), loaded 71.3 MB through 66
faults, reached `independent`, and passed direct and routed execution. The test
found a monitor race where intentional process-free dehydration was mistaken
for a crashed target; `dehydrating` is now published before teardown and a
durable dehydration image completes the remote dependency.

### Branch churn and deletion

A separate NFSv3 lifecycle test repeatedly reused one visible branch name for
50 local forks. Every branch created 128 files plus hardlinks and symlinks and
a 2 MiB regular file; every third iteration held an open file descriptor in a
named detached exec session; every fifth iteration dehydrated, waited for
shared durability, and copied the 2 MiB file directly from the process-free
branch. Every iteration then force-removed the branch and immediately reused
its name.

The first run exposed a stable-key cleanup bug: memory control images are keyed
by the internal filesystem-branch identity so renames do not move them, but
branch removal deleted only the legacy visible-name path. Name reuse therefore
collided with the retained dehydration image. Removal now deletes both the
stable key and legacy key after fencing publications and releasing the sparse
memory root. The same failure also showed that a sparse publication transaction
could remain rooted if a later control-image step failed; manifest reachability
is now committed before those steps and restored by the existing rollback.

The complete rerun passed. It left zero dehydration directories, zero memory
branch pointers, zero pending publications, zero exec followers, zero NFS
`.nfs*` files, and one rootfs branch. Rootfs metadata compacted to eight layers
after the run. A non-owning cluster node also attempted to collect another
node's intentionally unmaterialized private repository; maintenance now skips
only an absent repository path while continuing to report a present but
malformed repository.

Run the same matrix with:

```shell
python3 tools/ttc/distributed_benchmark/benchmark.py \
  --ttc /path/to/ttc --ttcd /path/to/ttcd \
  --dynamic-discovery \
  --matrix tools/ttc/distributed_benchmark/elastic-smoke-matrix.json \
  --work-dir /shared/nfs/ttc-benchmark --repetitions 1
```

## Remaining qualification

Lustre was not available on this qualification host: its
`6.17.0-1017-aws` kernel exposes neither Lustre nor LNet in `/proc/filesystems`,
has no corresponding kernel modules, and provides neither `mount.lustre` nor
`lctl`. NFS latency/bandwidth shaping exercises TTC's slow-shared-storage
behavior but is not presented as Lustre qualification. A real Lustre client
and server must be provisioned before that result can be recorded.

Physical multi-host loss/reordering, exhaustive node-loss injection at every
migration boundary, a real Lustre deployment, and long-running automatic
rebalance/anti-oscillation tests remain required before production use.
