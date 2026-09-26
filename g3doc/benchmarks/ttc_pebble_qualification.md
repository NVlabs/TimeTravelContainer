# Pebble qualification for TTC storage

## Status

This is a synthetic qualification benchmark, not an adoption decision. It was
run on 12 August 2026 against Pebble v2.1.5. The benchmark models TTC's proposed
immutable page records, persistent CoW page-map nodes, named branch roots,
durable commits, branch deletion, and crash recovery. It does not exercise the
gVisor `MemoryFile`, guest page faults, VFS coherence, remote reads, or the
eventual TTC reachability collector.

The runtime subsequently postponed Pebble and implemented immutable **1 MiB
snapshot-graph records**. Standalone roots store those records directly in the
authoritative graph object store; clustered roots publish them in indexed
shared packs and cache them on demand. The 64 KiB loose-file and Pebble results
below remain historical qualification data; they do not qualify or predict the
current format.

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

## 64 KiB block and extent experiment

The `chunk64` harness tests two different meanings of 64 KiB storage:

* **64 KiB value:** one Pebble key is the CoW extent. A guest 4 KiB read gets
  and slices the whole value; a guest 4 KiB write gets, copies, modifies, and
  sets the whole value.
* **4 KiB values / 64 KiB SST block:** each page remains independently keyed,
  but adjacent keys are physically packed into 64 KiB target SST blocks. A
  cold lookup reads the containing SST block, while a write records only 4 KiB.

For comparison, the current arrangement of 4 KiB values in 4 KiB target SST
blocks and one raw host file were tested by the same process. The dataset was
256 MiB of incompressible data, the Pebble cache was 128 MiB, random-write
phases modified 4,096 pages in batches of 128, and cache-dropped reads used
`FADV_DONTNEED`. The following are medians of three runs; raw sequential write
was noisy (414--1,178 MiB/s), so its percentage comparisons are approximate.

| Operation | Raw file | 64 KiB value | 4 KiB values / 64 KiB block | 4 KiB values / 4 KiB block |
| --- | ---: | ---: | ---: | ---: |
| Sequential durable write | 862 MiB/s | 108 MiB/s | 84.6 MiB/s | 124 MiB/s |
| Sequential cold read | 219 MiB/s | 196 MiB/s | 213 MiB/s | 188 MiB/s |
| Random 4 KiB warm read | 886k/s | 83.7k/s | 85.1k/s | 176k/s |
| Random 4 KiB warm read, p50 | 1.02 us | 17.3 us | 17.1 us | 5.61 us |
| Random 4 KiB cold read | 1,580/s | 1,356/s | 1,346/s | 1,347/s |
| Random 4 KiB cold read, p50 | 618 us | 823 us | 832 us | 675 us |
| Random 4 KiB unsynced write | 394k/s | 1.52k/s | 55.7k/s | 57.2k/s |
| Random 4 KiB durable write | 31.6k/s | 1.12k/s | 19.3k/s | 19.4k/s |
| 128-write durable commit, p50 | 3.31 ms | 63.1 ms | 6.06 ms | 6.08 ms |

Relative to raw host I/O, the 64 KiB-value layout retained only 0.39% of
unsynced and 3.6% of durable random-write throughput. This is the unavoidable
16-times payload amplification plus read-modify-write and database overhead.
It also consumed 538 MiB immediately after building a 256 MiB live set because
the SST and recyclable WAL coexist. After the two 16 MiB guest-write phases it
temporarily occupied 674--768 MiB before later compaction and WAL cleanup.
This layout is not acceptable as TTC's general writable-filesystem format.

Keeping 4 KiB values while raising only the SST block size avoids that failure.
Its random-write results were essentially identical to the 4 KiB-block layout,
and its sequential read was 13% faster than the 4 KiB-block layout and within
3% of raw. However, warm random reads were 52% slower than 4 KiB blocks because
each cache miss/admission handles a much larger block; cold p50 was 23% higher.
It also did not improve sequential ingestion in this test.

The measured conclusion is therefore narrower than “use 64 KiB everywhere”:

1. Do not represent a writable 64 KiB extent as one Pebble value if 4 KiB
   updates are common.
2. Pebble can remain the only physical store by retaining 4 KiB values and
   grouping sixteen adjacent keys under one logical pack/extent prefix.
3. Range iteration over those keys provides a 64 KiB dehydration transfer;
   after loading, the sixteen pages remain independently CoW.
4. A 64 KiB SST block is an optional sequential/dehydration tuning, not a good
   universal default. The 4 KiB target is substantially better for hot random
   access. Intermediate 16 KiB and 32 KiB targets should be measured with real
   TTC workloads before choosing the default.

## Dehydrated-memory benchmark

Dehydrated memory is immutable and read-then-discard, so it does not require a
general mutable blob database. The `hydration` harness compares three models:

* TTC's packed snapshot file with hole-punch reclamation;
* one ordinary host file per 64 KiB transfer unit, unlinked after loading; and
* one directly ingested Pebble SST containing 64 KiB values.

The packed-file baseline writes a 256 MiB stream in 64 KiB units and uses one
`pread` plus `FALLOC_FL_PUNCH_HOLE|FALLOC_FL_KEEP_SIZE` for each randomly
consumed unit. Every 128 punches are followed by `fdatasync`. The host used for
this benchmark does not support `FICLONERANGE`, so this measures TTC's byte-copy
export fallback rather than its faster reflink path.

The loose-file baseline places at most 1,024 chunk files in each shard
directory. It retains the shard directory descriptors and uses `openat` and
`unlinkat`, avoiding repeated full-path traversal. Construction closes all
chunk files and uses one `syncfs` before external publication. This is an
optimistic but valid durability strategy; in production it may also flush
unrelated dirty data on the same filesystem. Every 128 consumed chunks, the
benchmark synchronizes the shard directories containing unlinks.

The optimized Pebble path writes one sorted external SST directly and ingests
it into a new per-snapshot database. It does not put snapshot bytes through a
WAL or memtable: the database remains unpublished until the SST is finished and
durably ingested, and an interrupted unpublished build can be discarded. Each
key contains one 64 KiB transfer unit. Rehydration only reads; the loader keeps
its existing resident-page bitmap, and the database directory is deleted when
the last unit is consumed. This avoids per-key tombstones and compaction.

The table contains medians of three full 256 MiB runs on the machine described
above. The loose layout used 4,096 chunk files across four shard directories;
the packed layout used one data file and Pebble used seven host files.

| Operation | Packed + punch | Loose 64 KiB files | Pebble 64 KiB values |
| --- | ---: | ---: | ---: |
| Build and durably publish | 1,137 MiB/s | 592 MiB/s | 943 MiB/s |
| Allocated after build | 256.00 MiB | 256.00 MiB | 256.20 MiB |
| Sequential cold load | 219.9 MiB/s | 69.2 MiB/s | 280.4 MiB/s |
| Warm random load | 128k units/s | 77.3k units/s | 58.0k units/s |
| Warm random load, p50 | 7.73 us | 12.53 us | 18.65 us |
| Cold random load | 1,785 units/s | 1,715 units/s | 1,673 units/s |
| Cold random load, p50 | 748 us | 769 us | 778 us |
| 16-worker cold random load | 20.6k units/s | 19.2k units/s | 18.0k units/s |
| 16-worker cold random load, p50 | 776 us | 795 us | 804 us |
| 16-worker cold random load, p99 | 1.60 ms | 1.60 ms | 1.70 ms |
| Random load and discard, total | 1,145 units/s | 1,095 units/s | 1,075 units/s |
| Delete fully consumed store | 0.070 ms | 1.59 ms | 12.8 ms |

Pebble decisively outperformed loose files for serialized bulk operations. Its
snapshot construction was 59% faster and its sequential iterator was 4.1
times as fast. Thousands of file creates and one open/close pair per chunk are
real costs even when directory descriptors are cached.

Pebble did not win the demand-paging path. Loose files had 33% higher warm
random throughput, 2.5% higher single-worker cold throughput, and 6.6% higher
16-worker cold throughput. At 16 workers, both layouts exceed 1 GiB/s of
64 KiB payload, so a parallel prefetcher removes the loose layout's serialized
sequential-read weakness.

Hole punching and loose-file unlinking returned space proportionally: about
192, 128, 64, and 0 MiB remained after consuming 25%, 50%, 75%, and 100%. The
immutable Pebble store remained about 256 MiB at every intermediate point and
returned all space only when its directory was deleted. A separately tested
tombstone per loaded key was worse: no proportional space was returned, it
added synced delete-WAL traffic, and it required later compaction.

At the tested 64 KiB size, the loose layout was the preferable
memory-hydration prototype. It has no embedded-database dependency, maps
naturally to remotely addressable chunks, supports prompt reclamation, and
matches or beats Pebble on the lazy fault path. Pebble performed better when
serialized construction or full sequential restoration dominated.

This inode concern motivated the later 1 MiB decision. The 256 MiB test uses
4,096 64 KiB chunk inodes; 16 GiB would use 262,144, whereas 1 MiB chunks use
16,384. The new size may read and retain as many as 256 pages for one sparse
fault, so it requires a new benchmark rather than extrapolation from this
table. The benchmark also models one snapshot; a real family must release a
manifest reference and unlink a chunk only when no dehydrated manifest or
loader lease still references it.

This does not qualify Pebble for mutable rootfs or volume data. It also does not
yet include state restoration, copying loaded bytes into `MemoryFile`, real
guest page faults, shared pages across several dehydrated branches, or a crash
between SST construction and TTC metadata publication. Those remain integration
acceptance tests.

## Conclusion and gate

Pebble passed this synthetic functional gate but is not in TTC's runtime. TTC
now uses immutable 1 MiB snapshot-graph records for memory hydration and
shared-filesystem remote fork. Pebble is not selected for memory, rootfs, or
managed-volume storage.

No Pebble integration work is planned. If that decision is revisited after the
snapshot-graph implementation is measured, ordinary SST values were the safer
mode in this historical test. Pebble's experimental blob mode had a real
write-amplification advantage under complete overwrite churn, but lost
branch-read latency and steady-state disk space.

Before any future Pebble adoption, a new gate would need to integrate the
backend with TTC and measure:

1. actual guest page-fault latency and `mmap`/`read`/`write` coherence;
2. fork, dehydrate, rehydrate, remove, and daemon-crash paths with real branch
   reachability and loader leases;
3. databases, package installation, browser trees, and agent workspaces;
4. many small storage domains, including the WAL-recycler disk reserve;
5. slow disks, NFS/shared filesystems where supported, ENOSPC, injected I/O
   failures, and power-cut recovery;
6. loose 1 MiB files over local ext4/XFS, shared NFS, and TTC's remote page
   transport, including injected metadata and network latency, with the 64 KiB
   results retained as a comparison; and
7. a simple custom arena baseline, so any final backend is chosen against a
   relevant alternative rather than in isolation.
