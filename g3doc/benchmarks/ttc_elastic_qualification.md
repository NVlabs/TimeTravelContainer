# TTC elastic cluster qualification

This page records reproducible qualification evidence for elastic TTC. It is
not a general performance claim: loopback nodes share CPUs and the NFS server,
whereas production nodes do not.

## Implemented behaviors under test

- lease-bearing dynamic node join and discovery without static peer lists;
- a unified branch namespace and daemon-to-daemon command routing;
- fenced, identity-preserving migration of one branch while siblings remain on
  their original node;
- daemon-incarnation adoption after a clean restart;
- node cordon/drain, cold-branch transfer, and page-dependency draining;
- deterministic pressure placement and opt-in automatic rebalancing; and
- coalesced durable capacity requests when no placement is feasible; and
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
attempts retain one durable request ID, and removing the accepted outbox entry
allows the next pressure episode to publish a fresh ID.

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
multi-host network-delay/loss, node-loss injection during every migration
boundary, an external provisioner's end-to-end response to the durable
capacity outbox, and long-running automatic
rebalance/anti-oscillation tests also remain required before production use.
