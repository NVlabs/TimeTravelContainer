#!/usr/bin/env python3
"""Matched live-fork microbenchmark for TTC and forkd on kvm-node.

The setup phase is intentionally excluded from measured intervals. Each
runtime holds and dirties a 512 MiB anonymous mmap in Python 3.12.
"""

from __future__ import annotations

import json
import os
import pathlib
import subprocess
import time
import urllib.error
import urllib.request


OUT = pathlib.Path("/bench/results/2026-07-28-kvm-node")
OUT.mkdir(parents=True, exist_ok=True)
TTC = [
    "/bench/ttc/bin/ttc",
    "--data-root",
    "/bench/ttc/data",
    "--root",
    "/bench/ttc/runsc",
    "--daemon-socket",
    "/bench/ttc/ttcd.sock",
    "--runsc",
    "/bench/ttc/bin/runsc",
]
TTC_ENV = dict(os.environ, XDG_RUNTIME_DIR="/bench/ttc/runtime")
FORKD_ENV = dict(
    os.environ,
    HOME="/bench/forkd/home",
    PATH="/bench/forkd:/bench/forkd/home/.local/bin:" + os.environ["PATH"],
)
API = "http://127.0.0.1:8889"
WORKLOAD_B64 = (
    "aW1wb3J0IG1tYXAsdGltZQpTSVpFPTUxMioxMDI0KjEwMjQKbT1tbWFwLm1tYXAoLTEs"
    "U0laRSkKZm9yIGkgaW4gcmFuZ2UoMCxTSVpFLDQwOTYpOiBtW2ldPTEKb3BlbigiL3Rt"
    "cC90dC1yZWFkeSIsInciKS53cml0ZSgicmVhZHkiKQp3aGlsZSBUcnVlOiB0aW1lLnNs"
    "ZWVwKDM2MDApCg=="
)


def run(args: list[str], *, env=None, check=True, timeout=60) -> subprocess.CompletedProcess:
    return subprocess.run(
        args,
        env=env,
        check=check,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
    )


def ms_since(start_ns: int) -> float:
    return (time.perf_counter_ns() - start_ns) / 1_000_000


def http(method: str, path: str, body=None, timeout=60):
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(
        API + path,
        data=data,
        method=method,
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as response:
            raw = response.read()
            return None if not raw else json.loads(raw)
    except urllib.error.HTTPError as e:
        detail = e.read().decode(errors="replace")
        raise RuntimeError(f"{method} {path}: HTTP {e.code}: {detail}") from e


def pss_kib(pid: int | None) -> int | None:
    if not pid:
        return None
    try:
        for line in pathlib.Path(f"/proc/{pid}/smaps_rollup").read_text().splitlines():
            if line.startswith("Pss:"):
                return int(line.split()[1])
    except (FileNotFoundError, PermissionError):
        pass
    return None


def cpu_ticks(pid: int | None) -> int | None:
    if not pid:
        return None
    try:
        fields = pathlib.Path(f"/proc/{pid}/stat").read_text().split()
        return int(fields[13]) + int(fields[14])
    except (FileNotFoundError, PermissionError):
        return None


def allocated_tree(path: pathlib.Path) -> int:
    total = 0
    for root, dirs, files in os.walk(path):
        for name in dirs + files:
            try:
                total += (pathlib.Path(root) / name).stat().st_blocks * 512
            except FileNotFoundError:
                pass
    return total


def append_jsonl(name: str, row: dict) -> None:
    with (OUT / name).open("a") as f:
        f.write(json.dumps(row, sort_keys=True) + "\n")


def ttc_inspect() -> dict:
    return json.loads(run(TTC + ["inspect", "ttc-bench"], env=TTC_ENV).stdout)[0]


def ttc_branch_meta(tag: str) -> dict:
    branches = ttc_inspect()["TTC"]["runtime"]["branches"]
    return next(b for b in branches if b["id"] == tag)


def bench_ttc(n: int = 30) -> None:
    out = OUT / "ttc-single.jsonl"
    out.unlink(missing_ok=True)
    info = ttc_inspect()
    sentry = int(info["State"]["Pid"])
    upper = pathlib.Path(
        f"/bench/ttc/data/containers/{info['Id']}/rootfs-upper"
    )
    for i in range(n):
        tag = f"bench-single-{i:03d}"
        before_pss = pss_kib(sentry)
        before_cpu = cpu_ticks(sentry)
        before_alloc = allocated_tree(upper)
        start = time.perf_counter_ns()
        proc = run(TTC + ["fork", "ttc-bench", tag], env=TTC_ENV, timeout=30)
        fork_return_ms = ms_since(start)
        meta = ttc_branch_meta(tag)
        run(
            TTC + ["exec", f"ttc-bench@{tag}", "test", "-f", "/tmp/tt-ready"],
            env=TTC_ENV,
            timeout=30,
        )
        ready_ms = ms_since(start)
        row = {
            "trial": i,
            "fork_return_ms": fork_return_ms,
            "child_ready_ms": ready_ms,
            "fork_total_ms_internal": meta["fork_total_ns"] / 1_000_000,
            "source_downtime_ms": meta["source_downtime_ns"] / 1_000_000,
            "kernel_fork_ms": meta["kernel_fork_ns"] / 1_000_000,
            "sentry_pss_before_kib": before_pss,
            "sentry_pss_with_child_kib": pss_kib(sentry),
            "sentry_cpu_ticks": (
                None
                if before_cpu is None or cpu_ticks(sentry) is None
                else cpu_ticks(sentry) - before_cpu
            ),
            "rootfs_alloc_delta_bytes": allocated_tree(upper) - before_alloc,
            "stdout": proc.stdout.strip(),
        }
        append_jsonl(out.name, row)
        run(TTC + ["branch", "rm", f"ttc-bench@{tag}"], env=TTC_ENV)
        print(f"TTC {i + 1}/{n}: {fork_return_ms:.2f} ms", flush=True)


def sandboxes() -> list[dict]:
    return http("GET", "/v1/sandboxes") or []


def delete_all_sandboxes() -> None:
    for s in sandboxes():
        try:
            http("DELETE", f"/v1/sandboxes/{s['id']}")
        except RuntimeError:
            pass


def delete_snapshot(tag: str) -> None:
    try:
        http("DELETE", f"/v1/snapshots/{tag}")
    except RuntimeError as e:
        if "HTTP 404" not in str(e):
            raise


def guest_exec(sid: str, args: list[str], timeout=30) -> dict:
    return http(
        "POST",
        f"/v1/sandboxes/{sid}/exec",
        {"args": args, "timeout_secs": timeout},
        timeout=timeout + 5,
    )


def guest_workload_alive(sid: str) -> bool:
    code = (
        "import glob;"
        "assert any(b'tt-work.py' in open(p,'rb').read() "
        "for p in glob.glob('/proc/[0-9]*/cmdline'))"
    )
    result = guest_exec(sid, ["python3", "-c", code], timeout=5)
    return result["exit_code"] == 0


def spawn_source() -> dict:
    before = {s["id"] for s in sandboxes()}
    run(
        ["/bench/forkd/forkd", "spawn", "--tag", "bench-base", "-n", "1", "--live-fork"],
        env=FORKD_ENV,
        timeout=30,
    )
    source = next(s for s in sandboxes() if s["id"] not in before)
    command = (
        "rm -f /tmp/tt-ready; "
        f"echo {WORKLOAD_B64} | base64 -d >/tmp/tt-work.py; "
        "nohup python3 /tmp/tt-work.py >/tmp/tt-work.log 2>&1 &"
    )
    result = guest_exec(source["id"], ["sh", "-c", command])
    if result["exit_code"] != 0:
        raise RuntimeError(f"workload launch failed: {result}")
    deadline = time.monotonic() + 20
    while time.monotonic() < deadline:
        try:
            if guest_workload_alive(source["id"]):
                return source
        except Exception:
            pass
        time.sleep(0.05)
    raise TimeoutError("forkd workload did not become ready")


def snapshot_row(tag: str) -> dict | None:
    return next((s for s in (http("GET", "/v1/snapshots") or []) if s["tag"] == tag), None)


def bench_forkd(n: int = 15) -> None:
    out = OUT / "forkd-single.jsonl"
    out.unlink(missing_ok=True)
    delete_all_sandboxes()
    for i in range(n):
        tag = f"bench-live-{i:03d}"
        delete_snapshot(tag)
        source = spawn_source()
        pid = int(source["pid"])
        before_pss = pss_kib(pid)
        before_cpu = cpu_ticks(pid)
        start = time.perf_counter_ns()
        run(
            [
                "/bench/forkd/forkd",
                "snapshot",
                "--from-sandbox",
                source["id"],
                "--tag",
                tag,
                "--live",
                "--no-wait",
            ],
            env=FORKD_ENV,
            timeout=30,
        )
        branch_return_ms = ms_since(start)
        deadline = time.monotonic() + 30
        row = None
        while time.monotonic() < deadline:
            row = snapshot_row(tag)
            if row and row.get("status") == "ready":
                break
            time.sleep(0.01)
        if not row or row.get("status") != "ready":
            raise TimeoutError(f"snapshot {tag} did not become ready")
        snapshot_ready_ms = ms_since(start)
        if not guest_workload_alive(source["id"]):
            raise RuntimeError("source workload not alive after branch")
        source_after_pss = pss_kib(pid)
        source_after_cpu = cpu_ticks(pid)
        memory = pathlib.Path(row["dir"]) / "memory.bin"
        memory_alloc = memory.stat().st_blocks * 512
        http("DELETE", f"/v1/sandboxes/{source['id']}")

        before = {s["id"] for s in sandboxes()}
        child_start = time.perf_counter_ns()
        run(
            ["/bench/forkd/forkd", "spawn", "--tag", tag, "-n", "1"],
            env=FORKD_ENV,
            timeout=30,
        )
        child_return_ms = ms_since(child_start)
        child = next(s for s in sandboxes() if s["id"] not in before)
        if not guest_workload_alive(child["id"]):
            raise RuntimeError("restored child workload not alive")
        child_ready_ms = ms_since(child_start)
        result = {
            "trial": i,
            "branch_return_ms": branch_return_ms,
            "snapshot_ready_ms": snapshot_ready_ms,
            "source_pause_ms": row.get("pause_ms"),
            "restored_child_return_ms": child_return_ms,
            "restored_child_ready_ms": child_ready_ms,
            "source_pss_before_kib": before_pss,
            "source_pss_after_branch_kib": source_after_pss,
            "source_cpu_ticks": (
                None
                if before_cpu is None or source_after_cpu is None
                else source_after_cpu - before_cpu
            ),
            "child_pss_kib": pss_kib(int(child["pid"])),
            "snapshot_logical_bytes": memory.stat().st_size,
            "snapshot_allocated_bytes": memory_alloc,
        }
        append_jsonl(out.name, result)
        http("DELETE", f"/v1/sandboxes/{child['id']}")
        delete_snapshot(tag)
        print(
            f"forkd {i + 1}/{n}: return {branch_return_ms:.2f} ms, "
            f"ready {snapshot_ready_ms:.2f} ms, pause {row.get('pause_ms')} ms",
            flush=True,
        )


def bench_forkd_repeated(n: int = 6) -> None:
    out = OUT / "forkd-repeated-source.jsonl"
    out.unlink(missing_ok=True)
    delete_all_sandboxes()
    source = spawn_source()
    try:
        for i in range(n):
            tag = f"bench-repeat-{i:03d}"
            delete_snapshot(tag)
            start = time.perf_counter_ns()
            run(
                [
                    "/bench/forkd/forkd",
                    "snapshot",
                    "--from-sandbox",
                    source["id"],
                    "--tag",
                    tag,
                    "--live",
                    "--no-wait",
                ],
                env=FORKD_ENV,
                timeout=30,
            )
            returned = ms_since(start)
            deadline = time.monotonic() + 30
            row = None
            while time.monotonic() < deadline:
                row = snapshot_row(tag)
                if row and row.get("status") == "ready":
                    break
                time.sleep(0.01)
            append_jsonl(
                out.name,
                {
                    "trial": i,
                    "branch_return_ms": returned,
                    "snapshot_ready_ms": ms_since(start),
                    "source_pause_ms": None if row is None else row.get("pause_ms"),
                    "warning": None if row is None else row.get("warning"),
                    "source_alive": guest_workload_alive(source["id"]),
                },
            )
            delete_snapshot(tag)
    finally:
        try:
            http("DELETE", f"/v1/sandboxes/{source['id']}")
        except Exception:
            pass


def bench_ttc_fanout(n: int = 10, *, batch: bool = False) -> None:
    out = OUT / ("ttc-batch-fanout.json" if batch else "ttc-fanout.json")
    prefix = "bench-batch" if batch else "bench-fanout"
    tags = [f"{prefix}-{i:02d}" for i in range(n)]
    info = ttc_inspect()
    sentry = int(info["State"]["Pid"])
    pss_before = pss_kib(sentry)
    start = time.perf_counter_ns()
    try:
        if batch:
            run(TTC + ["fork", "ttc-bench", *tags], env=TTC_ENV, timeout=30)
        else:
            for tag in tags:
                run(TTC + ["fork", "ttc-bench", tag], env=TTC_ENV, timeout=30)
        forked_ms = ms_since(start)
        for tag in tags:
            run(
                TTC + ["exec", f"ttc-bench@{tag}", "test", "-f", "/tmp/tt-ready"],
                env=TTC_ENV,
                timeout=30,
            )
        ready_ms = ms_since(start)
        metas = [ttc_branch_meta(tag) for tag in tags]
        out.write_text(
            json.dumps(
                {
                    "n": n,
                    "batch": batch,
                    "forks_returned_ms": forked_ms,
                    "all_children_ready_ms": ready_ms,
                    "sum_source_downtime_ms": sum(
                        m["source_downtime_ns"] for m in metas
                    )
                    / 1_000_000,
                    "coordinated_source_downtime_ms": max(
                        m["source_downtime_ns"] for m in metas
                    )
                    / 1_000_000,
                    "max_single_fork_ms": max(m["fork_total_ns"] for m in metas)
                    / 1_000_000,
                    "sentry_pss_before_kib": pss_before,
                    "sentry_pss_with_children_kib": pss_kib(sentry),
                },
                indent=2,
                sort_keys=True,
            )
            + "\n"
        )
    finally:
        for tag in tags:
            run(
                TTC + ["branch", "rm", f"ttc-bench@{tag}"],
                env=TTC_ENV,
                check=False,
            )


def bench_forkd_fanout(n: int = 10) -> None:
    out = OUT / "forkd-fanout.json"
    tag = "bench-fanout-live"
    delete_all_sandboxes()
    delete_snapshot(tag)
    source = spawn_source()
    branch_start = time.perf_counter_ns()
    run(
        [
            "/bench/forkd/forkd",
            "snapshot",
            "--from-sandbox",
            source["id"],
            "--tag",
            tag,
            "--live",
            "--no-wait",
        ],
        env=FORKD_ENV,
        timeout=30,
    )
    branch_return_ms = ms_since(branch_start)
    deadline = time.monotonic() + 30
    snap = None
    while time.monotonic() < deadline:
        snap = snapshot_row(tag)
        if snap and snap.get("status") == "ready":
            break
        time.sleep(0.01)
    if not snap or snap.get("status") != "ready":
        raise TimeoutError(f"snapshot {tag} did not become ready")
    snapshot_ready_ms = ms_since(branch_start)
    http("DELETE", f"/v1/sandboxes/{source['id']}")

    before = {s["id"] for s in sandboxes()}
    start = time.perf_counter_ns()
    spawn = run(
        [
            "/bench/forkd/forkd",
            "spawn",
            "--tag",
            tag,
            "-n",
            str(n),
            "--per-child-netns",
        ],
        env=FORKD_ENV,
        timeout=60,
    )
    forks_returned_ms = ms_since(start)
    children = [s for s in sandboxes() if s["id"] not in before]
    if len(children) != n:
        raise RuntimeError(f"expected {n} children, got {len(children)}")
    for child in children:
        pong = http("POST", f"/v1/sandboxes/{child['id']}/ping", {})
        if not pong or not pong.get("pong"):
            raise RuntimeError(f"child ping failed: {child['id']}: {pong}")
    all_children_ready_ms = ms_since(start)
    out.write_text(
        json.dumps(
            {
                "n": n,
                "source_pause_ms": snap.get("pause_ms"),
                "branch_return_ms": branch_return_ms,
                "snapshot_ready_ms": snapshot_ready_ms,
                "forks_returned_ms": forks_returned_ms,
                "all_children_ready_ms": all_children_ready_ms,
                "children_pss_sum_kib": sum(
                    pss_kib(int(c["pid"])) or 0 for c in children
                ),
                "spawn_stdout": spawn.stdout.strip(),
            },
            indent=2,
            sort_keys=True,
        )
        + "\n"
    )
    delete_all_sandboxes()
    delete_snapshot(tag)


def main() -> None:
    (OUT / "run-meta.json").write_text(
        json.dumps(
            {
                "started_at_unix": time.time(),
                "workload": "Python 3.12, anonymous 512 MiB mmap, every 4 KiB page touched",
                "ttc_trials": 30,
                "forkd_fresh_source_trials": 15,
                "forkd_repeated_source_trials": 6,
            },
            indent=2,
            sort_keys=True,
        )
        + "\n"
    )
    bench_ttc()
    bench_ttc_fanout()
    bench_forkd()
    bench_forkd_repeated()
    bench_forkd_fanout()
    delete_all_sandboxes()
    print("benchmark complete", flush=True)


if __name__ == "__main__":
    main()
