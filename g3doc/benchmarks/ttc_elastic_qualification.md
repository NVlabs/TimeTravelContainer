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
daemon proxied `node drain` while holding its own mutation lock, and the source
called that daemon back to rehydrate a target. Node-wide mutations are now
routed to their source before any local mutation lock is taken. A final
13-branch reverse drain invoked through the destination completed in 25.932
seconds. Cleanup removed the family, both daemons, Sentries, gofers, console
followers, durable intents, and the test root.

## Pressure placement and cleanup

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

Run it with:

```shell
python3 tools/ttc/elastic_capacity_smoke.py --ttc /path/to/ttc
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

Run the same matrix with:

```shell
python3 tools/ttc/distributed_benchmark/benchmark.py \
  --ttc /path/to/ttc --ttcd /path/to/ttcd \
  --dynamic-discovery \
  --matrix tools/ttc/distributed_benchmark/elastic-smoke-matrix.json \
  --work-dir /shared/nfs/ttc-benchmark --repetitions 1
```

## Remaining qualification

The configured `kvm-node` was unreachable (`No route to host`) during this
iteration, and the development host has no `/dev/kvm`; therefore the latest
elastic changes have not yet been rerun on the KVM platform. Physical
multi-host network-delay/loss, exhaustive node-loss injection at every
instruction-level migration boundary, and long-running automatic
rebalance/anti-oscillation tests also remain required before production use.
