# TTC distributed-fork benchmark

`benchmark.py` runs two real `ttcd` instances, forks a live BusyBox workload
between them, drives either sequential or pseudo-random memory accesses in the
target, verifies both branches, and emits one self-contained JSON record per
run. The record includes every negotiated paging knob, fork and workload
latency, time to independence, durable final metrics when available, and the
last live source-session counters otherwise.

The two daemons use separate runtime directories and one shared TTC repository.
Each matrix cell restarts them so daemon-level policy changes cannot leak
between measurements. Image download is outside the timed region and cached in
the work directory.

Requirements are Python 3, OpenSSL, a C compiler capable of producing a static
binary, `ttc`, and `ttcd`. Network shaping also
requires `tc`, root or `CAP_NET_ADMIN`, and a disposable network interface. Do
not shape `lo` on a machine carrying unrelated traffic.

Run an unshaped baseline:

```bash
tools/ttc/distributed_benchmark/benchmark.py \
  --ttc "$PWD/../bin/ttc" --ttcd "$PWD/../bin/ttcd" \
  --work-dir /tmp/ttc-bench --output /tmp/ttc-bench/results.jsonl
```

Run the example policy/network matrix on a disposable privileged node:

```bash
sudo tools/ttc/distributed_benchmark/benchmark.py \
  --ttc /usr/local/bin/ttc --ttcd /usr/local/bin/ttcd \
  --matrix tools/ttc/distributed_benchmark/example-matrix.json \
  --repetitions 5 --work-dir /var/tmp/ttc-bench \
  --output /var/tmp/ttc-bench/results.jsonl
```

`delay_ms` is the one-way `netem` delay. The approximate request/response RTT
is twice that value. A zero `rate_mbit`, `delay_ms`, or `loss_percent` omits the
corresponding rule. Set `netem_device` to an empty string for no shaping.

For useful comparisons, randomize case order externally or provide the matrix
in a different order across repeated runs, run enough repetitions to report
median and tail latency, and monitor host memory/CPU contention separately.
The harness deliberately preserves raw JSONL rather than collapsing results
into a single score.

`memory_mib` controls the allocated working set, `rounds` controls repeated
passes, and `touch_fraction` (zero-exclusive through one-inclusive) limits each
pass to a sparse prefix of the sequential or pseudo-random access stream. This
makes it possible to measure prefetch waste without changing the fork-time
resident set.

`fanout` defaults to one. Values greater than one exercise the coordinated
sibling API: all target containers are captured from one source pause, then
run and verify the same workload concurrently. The JSON record contains one
transaction and loader status per target.

`capture_mode` defaults to `batch`. Set it to `sequential` with `fanout` greater
than one to issue one ordinary fork command per target. This provides a direct
comparison for batch fan-out's single source pause, shared capture, and
parallel target preparation.

Add `--verify-dehydrate` to dehydrate and rehydrate each remote target before
its access workload. This forces any remaining source-page dependency into the
shared durable snapshot, verifies process-free target reconstruction, and
records both transition latencies in the JSON result.

The harness compiles `page_worker.c` once into the work directory and imports
it through a TTC `:copy` mount. The worker prefaults one byte per 4 KiB page,
stops itself before the fork, and resumes only on `SIGCONT`. Thus readiness
polling and shell allocation behavior do not contaminate the access trace.
