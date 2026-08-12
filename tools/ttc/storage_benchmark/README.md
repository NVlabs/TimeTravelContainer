# TTC storage candidate benchmark

This standalone module stress-tests Pebble with TTC-shaped immutable pages,
persistent CoW trees, branch roots, overwrite churn, reclamation, and process
crashes. It is a candidate-engine benchmark, not TTC runtime code.

Run the branch workload in both storage modes:

```shell
go run . --mode=sst --dir=/tmp/ttc-pebble-sst >sst.json
go run . --mode=blob --dir=/tmp/ttc-pebble-blob >blob.json
```

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

The commands remove the supplied database directory before starting. Do not
point `--dir` at data that must be retained.

The August 2026 test method, environment, results, and limitations are in
[`g3doc/benchmarks/ttc_pebble_qualification.md`](../../../g3doc/benchmarks/ttc_pebble_qualification.md).

