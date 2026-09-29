#!/usr/bin/env python3

# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Print stable percentile summaries from the raw JSONL benchmark data."""

from __future__ import annotations

import json
import pathlib
import statistics


ROOT = pathlib.Path(__file__).resolve().parent


def rows(name: str) -> list[dict]:
    return [json.loads(line) for line in (ROOT / name).read_text().splitlines()]


def percentile(values: list[float], fraction: float) -> float:
    values = sorted(values)
    position = (len(values) - 1) * fraction
    lower = int(position)
    upper = min(lower + 1, len(values) - 1)
    return values[lower] + (values[upper] - values[lower]) * (position - lower)


def stats(data: list[dict], field: str) -> dict:
    values = [row[field] for row in data if row.get(field) is not None]
    return {
        "n": len(values),
        "min": min(values),
        "p50": percentile(values, 0.50),
        "p90": percentile(values, 0.90),
        "p95": percentile(values, 0.95),
        "max": max(values),
        "mean": statistics.mean(values),
    }


def main() -> None:
    ttc = rows("ttc-single.jsonl")
    ttc_optimized = rows("ttc-optimized-single.jsonl")
    ttc_batch = rows("ttc-optimized-batch-fanout.jsonl")
    forkd = rows("forkd-single.jsonl")
    repeated = rows("forkd-repeated-source.jsonl")
    summary = {
        "ttc_single": {
            field: stats(ttc, field)
            for field in (
                "fork_return_ms",
                "child_ready_ms",
                "fork_total_ms_internal",
                "source_downtime_ms",
                "kernel_fork_ms",
                "rootfs_alloc_delta_bytes",
            )
        },
        "ttc_optimized_single": {
            field: stats(ttc_optimized, field)
            for field in (
                "fork_return_ms",
                "child_ready_ms",
                "fork_total_ms_internal",
                "source_downtime_ms",
                "kernel_fork_ms",
                "rootfs_alloc_delta_bytes",
            )
        },
        "ttc_optimized_batch_fanout": {
            field: stats(ttc_batch, field)
            for field in (
                "forks_returned_ms",
                "all_children_ready_ms",
                "coordinated_source_downtime_ms",
            )
        },
        "forkd_single": {
            field: stats(forkd, field)
            for field in (
                "branch_return_ms",
                "snapshot_ready_ms",
                "source_pause_ms",
                "restored_child_return_ms",
                "restored_child_ready_ms",
                "snapshot_allocated_bytes",
            )
        },
        "forkd_end_to_end_live_child_ms": stats(
            [
                {
                    "value": row["snapshot_ready_ms"]
                    + row["restored_child_ready_ms"]
                }
                for row in forkd
            ],
            "value",
        ),
        "forkd_repeated_source": {
            field: stats(repeated, field)
            for field in (
                "branch_return_ms",
                "snapshot_ready_ms",
                "source_pause_ms",
            )
        },
        "ttc_fanout": json.loads((ROOT / "ttc-fanout.json").read_text()),
        "forkd_fanout": json.loads((ROOT / "forkd-fanout.json").read_text()),
    }
    print(json.dumps(summary, indent=2, sort_keys=True))


if __name__ == "__main__":
    main()
