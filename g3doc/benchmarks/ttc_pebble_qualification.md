# Pebble qualification for TTC storage

## Status

This is a synthetic qualification benchmark, not an adoption decision. It was
run on 12 August 2026 against Pebble v2.1.5. The benchmark models TTC's proposed
immutable page records, persistent CoW page-map nodes, named branch roots,
durable commits, branch deletion, and crash recovery. It does not exercise the
gVisor `MemoryFile`, guest page faults, VFS coherence, remote reads, or the
eventual TTC reachability collector.

The harness is in `tools/ttc/storage_benchmark`. It intentionally lives in a
small standalone Go module so that changing the candidate storage engine does
not add it to the TTC runtime dependency graph.

## Test machine

* Linux 6.17.0-1017-aws, x86-64
* 4 vCPUs, Intel Xeon Platinum 8488C
* 15 GiB RAM
* ext4 on local NVMe (`discard`, `commit=30`)
* Go 1.26.5
* Pebble v2.1.5 with a 128 MiB block cache

Numbers from this machine are useful for comparing modes, not as portable TTC
performance promises. In particular, synced-root latency depends heavily on
the host filesystem and storage device.

## Branch-shaped workload

The test builds 131,072 immutable 4 KiB pages (512 MiB) under a three-level,
fanout-64 immutable radix tree. It creates four branches, writes 4,096 random
pages in each branch, and publishes a new root after every 128 writes with a
synced batch. Concurrent readers continue to verify the unchanged base branch.
Every logical page lookup performs three node lookups and one 4 KiB data
lookup. The test then reopens the database, deletes all branch-private records,
forces compaction, and verifies final reclamation.

Two Pebble configurations were compared:

* **SST:** ordinary 4 KiB values in SSTs;
* **blob:** Pebble's experimental value separation for values of at least
  1 KiB, with a 20% garbage-rewrite target.

| Metric | SST | Blob |
| --- | ---: | ---: |
| Initial 512 MiB build | 96.3 MiB/s | 109.0 MiB/s |
| Durable root-only fork, p50 | 1.19 ms | 1.02 ms |
| Durable root-only fork, p99 | 2.73 ms | 3.72 ms |
| Diverging page writes | 17,682 pages/s | 18,913 pages/s |
| 128-page synced commit, p50 | 5.44 ms | 5.11 ms |
| Warm logical-page lookup, p50 | 12.7 us | 20.9 us |
| Warm logical-page lookup, p99 | 43.4 us | 72.4 us |
| Cache-dropped logical-page lookup, p50 | 703 us | 738 us |
| Cache-dropped logical-page lookup, p99 | 2.24 ms | 3.89 ms |
| Allocated after divergence | 706.7 MiB | 714.3 MiB |
| Allocated after branch deletion/compaction | 526.5 MiB | 518.8 MiB |
| Forced compaction | 3.43 s | 3.47 s |
| Reopen | 25.1 ms | 23.4 ms |
| Concurrent verification failures | 0 | 0 |

After deleting the remaining base keys and compacting, allocated storage fell
below 0.8 MiB in both modes. The ordinary SST mode was clearly better for
latency in this read-heavy branch workload. Blob mode modestly improved ingest
and branch-write throughput, but its warm-read median was 65% higher and its
cold-read p99 was 73% higher.

A separate 1,000-fork run used a 32 MiB base and independently synced every new
root. Fork latency was 1.01 ms p50, 1.17 ms p95, 1.60 ms p99, and 3.72 ms max.
The stored branch operation is O(1) in page count, but an immediately durable
fork still pays for storage synchronization. TTC should later measure grouped
sibling commits and explicitly define when a branch must survive power loss.

## Overwrite and reclamation stress

The churn test repeatedly overwrites every page in a 65,536-page (256 MiB)
working set. Four random-order overwrite cycles use 256-page synced batches,
two concurrent verifying readers, a flush, and a forced full compaction after
each cycle.

| Metric (mean over four cycles) | SST | Blob |
| --- | ---: | ---: |
| Foreground writes | 35,785 pages/s | 35,577 pages/s |
| Forced compaction | 4.87 s | 3.11 s |
| Concurrent read p50 | 8.68 us | 9.02 us |
| Concurrent read p99 | 119.8 us | 85.4 us |
| Allocated after compaction | 449.3 MiB | 506.5 MiB |
| Concurrent verification failures | 0 | 0 |

Blob mode rewrote blob files 19 times by the fourth cycle. It reduced measured
host write output by about 46% and made compaction 36% faster, but retained
321.7 MiB of live blob-file space for 256 MiB of referenced values, a 25.7%
value-space amplification. Ordinary SST values used about 57 MiB less allocated
space after compaction.

After all keys were deleted and compacted, both modes still had about 187 MiB
allocated until close and reopen. Inspection showed that this was not live TTC
data: Pebble deliberately retains obsolete WAL files for reuse. Pebble v2 sets
the recycler limit to `MemTableStopWritesThreshold + 1`; reopening discarded
the reserve and reduced the databases to 0.10 MiB (SST) and 0.16 MiB (blob).
This behavior is operationally important for many small TTC domains and needs
a measured bound or an upstream/configuration solution before adoption.

## Process-crash stress

For each mode, the crash driver repeatedly starts a writer, waits until database
recovery is complete, kills it with `SIGKILL` after a random 0--5 ms write
interval, reopens the database, and validates that the durable root refers only
to present, checksum-valid nodes and data. A synced batch atomically contains a
root publication and the immutable objects that root newly references.

Both SST and blob modes passed 100 kill/reopen cycles with no missing or
partially visible referenced object. This covers process termination and WAL
replay. It does not simulate lost writes, torn storage below the filesystem,
power loss, or bugs in TTC's future reachability/lease protocol.

## Raw host-file comparison

The `raw` harness provides a deliberately weaker lower bound using one ordinary
host file. A logical page is its byte offset, a read is one `pread`, and a write
overwrites that offset. It has no immutable objects, branch sharing,
reachability, atomic multi-object publication, checksums, or WAL recovery.
Consequently, its numbers isolate costs but do not represent an alternative
implementation of TTC semantics.

Five raw and three Pebble runs were used for the sequential-build medians.
Three raw runs were used for the write medians. Read figures compare the full
size runs. “Direct Pebble” gets one 4 KiB data key and isolates the engine from
TTC's proposed three-node page-map traversal. “TTC tree” gets those three
512-byte nodes and then the data key.

| Operation | Raw host file | Pebble SST | Relative result |
| --- | ---: | ---: | --- |
| Sequential 512 MiB build | 259.8 MiB/s | 96.5 MiB/s | 37.1% retained; 62.9% loss |
| Unsynced random updates | 40,564 pages/s | 37,737 pages/s | 93.0% retained; 7.0% loss |
| Durable random updates | 9,441 pages/s | 17,700--19,000 pages/s | Pebble is 1.9--2.0x faster |
| Durable root publication, p50 | 1.003 ms | 1.011 ms | 0.8% latency overhead |
| Durable root publication, p99 | 1.197 ms | 1.603 ms | 33.9% latency overhead |
| Warm direct data read, p50 | 0.81 us | 5.12 us | 6.3x latency; 83.6% throughput loss |
| Warm TTC-tree read, p50 | 0.81 us | 12.67 us | 15.7x latency; 94.0% throughput loss |
| Cold direct data read, p50 | 643 us | 666 us | 3.5% latency; 6.9% throughput loss |
| Cold TTC-tree read, p50 | 643 us | 752 us | 16.9% latency; 25.2% throughput loss |

For the root comparison, the raw file is preallocated and appends one 8-byte
root followed by `fdatasync`; Pebble inserts one named root key and synchronizes
its preallocated WAL. The nearly identical medians show that device/filesystem
sync latency dominates. Pebble's higher p99 remains relevant for fork latency.

The durable random-write result is not a mistake. Direct in-place writes must
flush scattered dirty pages at each durability boundary. Pebble first commits
them sequentially to its WAL and moves them into final files later, making the
foreground path about twice as fast here. Without synchronization, where this
advantage is removed, the engine plus immutable-tree updates cost about 7% in
throughput.

Cached reads expose the largest abstraction cost. One direct Pebble value
lookup is about 6.3 times the latency of one raw `pread`, and performing every
page-map level through the database raises this to 15.7 times. TTC should
therefore keep active page-map nodes in memory and avoid repeated database
lookups on established mappings. Cold reads are much closer because host I/O
dominates: one direct Pebble read costs 3.5% at the median, while the uncached
four-lookup path costs 16.9%.

A clean compacted 512 MiB base occupied 526.4 MiB in Pebble, 2.8% more than the
raw file. Before compaction and WAL cleanup it occupied 575.1 MiB, 12.3% more.
Overwrite churn and the WAL recycler can cause substantially larger temporary
amplification, as described above.

## Conclusion and gate

Pebble passes the first synthetic functional gate and is suitable for a
throwaway or replaceable TTC backend prototype. It is **not yet selected as
TTC's production storage format**.

For the first prototype, ordinary SST values are the safer default. Pebble's
experimental blob mode has a real write-amplification advantage under complete
overwrite churn, but loses branch-read latency and steady-state disk space on
this test. It should remain an optional experiment until real TTC workloads
show that its lower write amplification matters more.

Before adoption, the next gate must integrate the backend with TTC and measure:

1. actual guest page-fault latency and `mmap`/`read`/`write` coherence;
2. fork, dehydrate, rehydrate, remove, and daemon-crash paths with real branch
   reachability and loader leases;
3. databases, package installation, browser trees, and agent workspaces;
4. many small storage domains, including the WAL-recycler disk reserve;
5. slow disks, NFS/shared filesystems where supported, ENOSPC, injected I/O
   failures, and power-cut recovery; and
6. a simple custom arena baseline, so Pebble is chosen against a relevant
   alternative rather than in isolation.
