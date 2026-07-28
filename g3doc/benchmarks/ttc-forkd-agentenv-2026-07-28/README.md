# TTC, forkd, and AgentENV live-fork experiment

Date: 2026-07-28

This experiment compares the operations that are closest to a live container
fork in TimeTravelContainer (TTC), forkd, and AgentENV. It is not a simple
leaderboard: the three systems expose materially different completion and
filesystem semantics.

## Result summary

All runnable TTC and forkd trials used Python 3.12 with a 512 MiB anonymous
mapping whose every 4 KiB page had been written.

| Measurement | TTC | forkd |
|---|---:|---:|
| Source pause, p50 | 32.30 ms | 2.00 ms |
| Client returns, p50 | 62.13 ms, child already exists | 15.39 ms, snapshot is still writing |
| New child verified, p50 | 113.00 ms including a separate `ttc exec` probe | 1,356.92 ms from branch request through snapshot completion and child probe |
| Fork artifact allocated on disk, p50 | 4 KiB rootfs metadata | 1 GiB memory image |
| Trials | 30 | 15 fresh sources |

The TTC branch is running when `ttc fork` returns. The 113 ms TTC figure adds
an independent CLI/daemon `ttc exec` round trip to prove liveness; it is not
the branch's actual start boundary.

`forkd snapshot --live --no-wait` has a different contract. Its 15 ms return
means that the source has resumed and an asynchronous memory copy is underway.
The resulting tag cannot be restored until that copy finishes. Snapshot
completion took 1,262.69 ms p50, after which restoring and probing one child
took another 94.81 ms p50.

Thus:

* forkd minimized source downtime most effectively in this experiment;
* TTC produced an immediately usable branch about 22 times sooner than the
  corresponding forkd branch-to-restored-child pipeline; and
* TTC did so without writing a RAM snapshot, while forkd allocated the complete
  1 GiB guest memory image for every live branch.

## Ten-child fan-out

TTC was subsequently extended with `ttc fork SOURCE CHILD1 CHILD2 ...`.
It freezes the source once, creates a paused primary child, resumes the source,
and then captures reusable metadata and creates the remaining persistent
rootfs branches from that frozen primary.

| Measurement | TTC, sequential baseline | TTC, coordinated batch | forkd |
|---|---:|---:|---:|
| Children | 10 | 10 | 10 |
| Source downtime | 331.68 ms summed over 10 freezes | 31.26 ms, one freeze | 2 ms during prior snapshot |
| Fork/spawn calls returned | 635.55 ms | 255.49 ms | 60.19 ms |
| All children probed | 798.68 ms | 445.12 ms | 130.73 ms |
| Preparation included | No separate snapshot | No separate snapshot | No; used a previously completed snapshot |
| Trials | 1 | 5 | 1 |

These are deliberately reported separately from the single live-fork result.
The coordinated TTC path reduced API return time by 59.8%, child-probe time by
44.3%, and application downtime by 90.6% relative to the original sequential
TTC fan-out. The five TTC batch figures above are medians. Its children include
independent persistent reflink-CoW rootfs branches.

forkd restored ten microVMs in parallel from one already-materialized memory
snapshot. Producing that snapshot took another 1,294.69 ms and paused its
source for 2 ms. forkd therefore remains substantially faster once a template
has been prepared, but that preparation and its 1 GiB memory artifact are not
included in its fan-out timing.

## Follow-up memory-metadata result

The TTC follow-up also replaced generic per-PMA state encoding with immutable
in-memory PMA snapshots and direct range-reference acquisition by children.
This is the intended lazy/refcounted metadata path: application pages remain
in the shared MemoryFile and are never copied or written to a checkpoint.

For this workload, however, ordinary single-fork source downtime was
32.41 ms p50 after the change versus 32.30 ms in the baseline. The difference
is noise, not an improvement. A densely populated 512 MiB anonymous mapping is
represented by compact PMA extents rather than one metadata object per 4 KiB
page, so the baseline inference that populated-page metadata dominated the
32 ms pause was incorrect. The remaining single-fork cost is in validation,
filesystem preparation, whole-kernel metadata save/load, and child restore;
the next profiling pass should expose those existing `LiveForkMetrics`
components through the benchmark output before optimizing further.

## AgentENV result

AgentENV v0.1.0 could not build a template on this host, so no performance
number is reported.

The host exposed `/dev/kvm`, `/dev/net/tun`, and `/dev/ublk-control`; KVM VM
creation worked; the process had the required capabilities; ublk device
permissions were corrected; and the service had unlimited locked memory.
Template construction nevertheless failed at:

```text
start ublk dev: start_dev ctrl request: Invalid argument (os error 22)
```

The same error reproduced in the official privileged
`ghcr.io/kvcache-ai/aenv-server:latest` image. Disabling ublk let the service
start, but template construction then rejected the request because AgentENV's
template path requires `ublk.enabled=true` with the OverlayBD device type.

This is a compatibility result for the host's Ubuntu 6.17.0-14 kernel, not a
claim about AgentENV performance on a supported, known-good kernel. A fair
rerun should boot the physical node with a kernel on which AgentENV's ublk
stack is known to work, such as the stock Ubuntu 24.04 6.8 series, and repeat
the same working-set and readiness protocol.

## Semantics that affect interpretation

TTC's operation is an in-memory fork of a running gVisor container family. The
new branch includes process/kernel state and a private, persistent reflink-CoW
rootfs branch.

forkd's live operation first creates a restorable memory snapshot. A later
spawn maps that memory image privately. In forkd v0.5.3, the snapshot metadata
continues to reference the same ext4 rootfs backing path; the live branch path
does not create an independent rootfs copy. The benchmark therefore avoided
post-branch filesystem writes. forkd's timing is useful for memory/process
state, but it is not an equivalent whole-container filesystem fork.

AgentENV is also KVM/Firecracker based and uses an ublk/OverlayBD storage path.
It could not reach its template-ready state on this kernel.

## Environment

* Host: AMD Ryzen Threadripper PRO 5975WX, 32 cores / 64 threads, 125 GiB RAM.
* Kernel: Ubuntu `6.17.0-14-generic`.
* Node: privileged Ubuntu 24.04 Docker container with the host device tree and
  cgroup v2 hierarchy exposed.
* Storage: 80 GiB XFS loop filesystem at `/bench`, `reflink=1`, mounted
  `noatime`. The loop image itself resided on the outer Docker overlay.
* KSM: disabled.
* Huge pages: none reserved.
* TTC baseline: source commit
  `dc14e0885920ebd54b05b56d628c3561c8e49052`, rootless isolated daemon,
  reflink volume mode.
* TTC optimized follow-up: source commit
  `32a872c5f`, using the same daemon, image, workload, and storage.
* forkd: release v0.5.3; source comparison commit
  `b5c2d7c1f8f8f00b2c5c2e693d30137c427e4147`.
* AgentENV: release v0.1.0; source comparison commit
  `6296bc4be7ad79eb3a278eb5264ef011c341adf5`.

The XFS loop layer makes absolute storage-copy throughput host-specific.
TTC's data root and forkd's rootfs/snapshot paths nevertheless used the same
filesystem during the measurements.

## forkd deployment repair

forkd's documented Firecracker branch
`forkd-v0.4-mem-backend-shared-v1.12` at
`7d80afade831f01e3178c5b76daf8b4cbc02d176` did not build as cloned: its
tracked call sites referenced two source files absent from the Git tree. The
experiment reconstructed the small request/config definitions from their call
sites.

After that build, Firecracker's seccomp policy killed the VMM when the live
branch endpoint used userfaultfd. `strace` identified the exact missing
operations. The benchmark binary adds only:

* `USERFAULTFD_IOC_NEW`;
* `UFFDIO_API`; and
* `UFFDIO_REGISTER`.

The patches are included beside the raw results. They restore the branch's
documented live-fork path; they do not optimize its measured implementation.

## TTC conclusions and next priorities

1. Preserve the product message around semantics: TTC creates a runnable,
   in-memory, rootless whole-container branch with a forked filesystem. Compare
   `request -> runnable isolated child`, not just API acknowledgement.
2. The coordinated sibling interface is now implemented as
   `ttc fork SOURCE CHILD1 CHILD2 ...`. It amortizes CLI/RPC and source-freeze
   work and moves reusable metadata capture plus sibling rootfs branching
   after source resume.
3. The PMA snapshot fast path is now implemented, but it did not reduce the
   single-fork median for this compact-mapping workload. Measure validation,
   filesystem preparation, metadata save/load, and child restore separately
   across VMA- and FD-heavy cases before choosing the next critical-path
   optimization.
4. Keep the internal `fork_total_ns`, `source_downtime_ns`, and
   `kernel_fork_ns` metrics. Expose them in a machine-readable benchmark mode
   so results do not require `ttc inspect`.
5. Turn this harness into a scaling suite over working-set size, VMAs,
   processes/threads, FDs, internal sockets, and filesystem dirtiness.

## Reproducing and inspecting

Raw samples and scripts are in this directory:

* `run_benchmarks.py` runs the matched workload;
* `summarize.py` computes interpolated p50/p90/p95 values;
* `ttc-single.jsonl` and `forkd-single.jsonl` contain every trial;
* `ttc-optimized-single.jsonl` contains the 20 post-change single-fork trials;
* `ttc-optimized-batch-fanout.jsonl` contains five coordinated fan-out trials;
* `forkd-repeated-source.jsonl` records six branches from one source;
* `ttc-fanout.json` and `forkd-fanout.json` contain the fan-out trials; and
* `patches/firecracker/` contains the Firecracker build repairs.

Run:

```bash
python3 summarize.py
```

PSS samples are retained in the raw rows, but they are intentionally excluded
from the headline comparison. TTC keeps guest pages in gVisor-managed shmem
and forkd shares file/memfd pages among Firecracker and the controller, so
per-process PSS does not attribute the same underlying memory in comparable
ways.

## Cleanup issue found after measurement

The post-benchmark `ttc rm -f ttc-bench` stopped both the runsc sandbox and
gofer processes, but then waited for 120 seconds and failed with:

```text
waiting sandbox "...": sandbox is still running: exit status 128
```

The durable mutation record completed with status 1 and retained the container
metadata. After the daemon exited, documented recovery mode
`ttc --direct rm -f ttc-bench` removed that stale family immediately. This did
not affect benchmark samples because it happened after all measurements, but
it is a real lifecycle/reconciliation bug. TTC should treat confirmed sandbox
process death as authoritative during forced removal and clear stale runsc
state without waiting for the full stop timeout. The raw durable request is in
`ttc-cleanup-failure.json`.

The optimized follow-up reproduced a second form after a clean daemon
shutdown: direct forced removal blocked in `runsc delete --force` despite no
live sandbox or gofer processes. The hung cleanup commands were terminated and
the four confirmed benchmark-only container directories were removed
manually. Installed tools, pulled image data, and raw benchmark results were
retained.
