# TTC storage candidate benchmark

This standalone module stress-tests Pebble with TTC-shaped immutable pages,
persistent CoW trees, branch roots, overwrite churn, reclamation, and process
crashes. It is a candidate-engine benchmark, not TTC runtime code.

Run the branch workload in both storage modes:

```shell
go run . --mode=sst --dir=/tmp/ttc-pebble-sst >sst.json
go run . --mode=blob --dir=/tmp/ttc-pebble-blob >blob.json
```

Add `--sync-updates=false` to measure the foreground update path without a
durability barrier.

Run complete-overwrite and compaction churn:

```shell
go run ./churn --mode=sst --dir=/tmp/ttc-churn-sst >churn-sst.json
go run ./churn --mode=blob --dir=/tmp/ttc-churn-blob >churn-blob.json
```

Run repeated `SIGKILL` and WAL-recovery validation:

```shell
go run ./crash --mode=sst --rounds=100 --dir=/tmp/ttc-crash-sst
go run ./crash --mode=blob --rounds=100 --dir=/tmp/ttc-crash-blob
```

Run the intentionally weaker raw-host-file lower bound:

```shell
go run ./raw --dir=/tmp/ttc-raw >raw.json
go run ./raw --dir=/tmp/ttc-raw-nosync --sync-updates=false >raw-nosync.json
```

Compare raw storage with a single 64 KiB extent value, 4 KiB values in 64 KiB
SST blocks, and 4 KiB values in 4 KiB SST blocks:

```shell
go run ./chunk64 --dir=/tmp/ttc-chunk64 >chunk64.json
```

Use `--layouts` to run a subset, for example
`--layouts=raw,pebble-4k-values-64k-block`.

Compare immutable 64 KiB memory-hydration units in a directly ingested Pebble
SST, one ordinary file per unit with unlink-on-load reclamation, and a packed
file using read-then-hole-punch reclamation:

```shell
go run ./hydration --dir=/tmp/ttc-hydration >hydration.json
```

The default uses whole-store Pebble deletion after complete consumption. Use
`--pebble-discard=tombstone` to reproduce the rejected per-key deletion policy,
or `--pebble-build=nowal` to compare memtable/flush construction with direct SST
ingestion.

The commands remove the supplied database directory before starting. Do not
point `--dir` at data that must be retained.

The August 2026 test method, environment, results, and limitations are in
[`g3doc/benchmarks/ttc_pebble_qualification.md`](../../../g3doc/benchmarks/ttc_pebble_qualification.md).
