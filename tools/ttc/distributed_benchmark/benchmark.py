#!/usr/bin/env python3
"""Reproducible two-node TTC distributed-fork workload benchmark."""

import argparse
import concurrent.futures
import errno
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import time


def run(argv, *, env=None, timeout=120, check=True, capture=True):
    result = subprocess.run(
        [str(arg) for arg in argv], env=env, timeout=timeout, check=False,
        text=True, stdout=subprocess.PIPE if capture else None,
        stderr=subprocess.PIPE if capture else None,
    )
    if check and result.returncode:
        raise RuntimeError(
            f"command failed ({result.returncode}): {' '.join(map(str, argv))}\n"
            f"stdout:\n{result.stdout or ''}\nstderr:\n{result.stderr or ''}"
        )
    return result


def remove_tree(path, timeout=30):
    """Remove a dedicated test tree after network-FS close propagation."""
    deadline = time.monotonic() + timeout
    while path.exists():
        try:
            shutil.rmtree(path)
            return
        except OSError as error:
            if error.errno not in (errno.EBUSY, errno.ENOTEMPTY) or time.monotonic() >= deadline:
                raise
            time.sleep(0.25)


def make_certificates(root):
    ca = root / "ca.pem"
    if ca.exists():
        return
    run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
         "-days", "2", "-subj", "/CN=ttc-benchmark-ca",
         "-keyout", root / "ca.key", "-out", ca])
    for node in ("node-a", "node-b"):
        run(["openssl", "req", "-newkey", "rsa:2048", "-nodes",
             "-subj", f"/CN={node}", "-keyout", root / f"{node}.key",
             "-out", root / f"{node}.csr"])
        ext = root / f"{node}.ext"
        ext.write_text(
            f"subjectAltName=DNS:{node},IP:127.0.0.1\n"
            "extendedKeyUsage=serverAuth,clientAuth\n", encoding="utf-8")
        run(["openssl", "x509", "-req", "-days", "2",
             "-in", root / f"{node}.csr", "-CA", ca,
             "-CAkey", root / "ca.key", "-CAcreateserial",
             "-extfile", ext, "-out", root / f"{node}.pem"])


def make_worker(root):
    assets = root / "assets"
    worker = assets / "page-worker"
    if worker.exists():
        return assets
    assets.mkdir(parents=True, exist_ok=True)
    source = pathlib.Path(__file__).with_name("page_worker.c")
    run(["cc", "-O2", "-static", "-Wall", "-Wextra", "-o", worker, source], timeout=120)
    worker.chmod(0o755)
    return assets


class Experiment:
    def __init__(self, args, case, repetition, root):
        self.args = args
        self.case = case
        self.repetition = repetition
        self.root = root
        self.runtime_a = root / "runtime-a"
        self.runtime_b = root / "runtime-b"
        self.data = args.data_dir or (root / "data")
        unique = f"{os.getpid()}-{repetition}-{time.time_ns()}"
        self.source = f"bench-source-{unique}"
        self.targets = [f"bench-target-{unique}-{i}" for i in range(int(case.get("fanout", 1)))]
        self.source_id = ""
        self.target_ids = []
        self.txids = []
        self.netem_active = False

    @staticmethod
    def branch(container):
        return f"{container}@main"

    def env(self, node):
        runtime = self.runtime_a if node == "a" else self.runtime_b
        result = os.environ.copy()
        result.update({
            "TTC_ROOT": str(self.data),
            "TTC_LOCAL_ROOT": str(runtime / "data"),
            "TTC_SOCKET": str(runtime / "run" / "ttcd.sock"),
        })
        result.pop("TTC_DATA_ROOT", None)
        result.pop("TTC_RUNTIME_DIR", None)
        return result

    def runsc_root(self, node):
        runtime = self.runtime_a if node == "a" else self.runtime_b
        return runtime / "data" / "runtime" / "runsc"

    def ttc(self, node, *argv, timeout=120, check=True):
        return run([self.args.ttc, *argv], env=self.env(node), timeout=timeout, check=check)

    def start_daemons(self):
        for path in (self.runtime_a, self.runtime_b, self.data):
            path.mkdir(parents=True, exist_ok=True)
            path.chmod(0o700)
        common = [
            "--unsafe-no-host-isolation", "--allow-slow-volume-copy",
            "--cluster-repository-id", "ttc-benchmark",
            "--cluster-ca", self.root / "ca.pem",
            "--cluster-page-listen", "127.0.0.1:0",
            "--cluster-page-advertise", "127.0.0.1:0",
            "--cluster-lease", self.args.cluster_lease,
            "--cluster-transfer-bytes", self.case["transfer_bytes"],
            "--cluster-max-outstanding-bytes", self.case["max_outstanding_bytes"],
            "--cluster-max-parallel", self.case["max_parallel"],
            "--cluster-ack-batch-pages", self.case["ack_batch_pages"],
            "--cluster-ack-interval", self.case["ack_interval"],
            "--cluster-prefetch", self.case["prefetch"],
            "--cluster-push", self.case["push"],
        ]
        node_a = [
            "--cluster-listen", "127.0.0.1:27443",
            "--cluster-node-id", "node-a",
            "--cluster-cert", self.root / "node-a.pem",
            "--cluster-key", self.root / "node-a.key",
        ]
        node_b = [
            "--cluster-listen", "127.0.0.1:37443",
            "--cluster-node-id", "node-b",
            "--cluster-cert", self.root / "node-b.pem",
            "--cluster-key", self.root / "node-b.key",
        ]
        if self.args.dynamic_discovery:
            node_a += ["--cluster-advertise", "https://127.0.0.1:27443,node-a"]
            node_b += ["--cluster-advertise", "https://127.0.0.1:37443,node-b"]
        else:
            node_a += ["--cluster-peer", "node-b=https://127.0.0.1:37443,node-b"]
            node_b += ["--cluster-peer", "node-a=https://127.0.0.1:27443,node-a"]
        self.ttc("a", "daemon", "start", *common, *node_a)
        try:
            self.ttc("b", "daemon", "start", *common, *node_b)
        except Exception:
            self.ttc("a", "daemon", "stop", check=False)
            raise

    def shape_network(self):
        device = self.case.get("netem_device", "")
        if not device:
            return
        command = ["tc", "qdisc", "replace", "dev", device, "root", "netem"]
        delay = float(self.case.get("delay_ms", 0))
        rate = float(self.case.get("rate_mbit", 0))
        loss = float(self.case.get("loss_percent", 0))
        if delay:
            command += ["delay", f"{delay}ms"]
        if rate:
            command += ["rate", f"{rate}mbit"]
        if loss:
            command += ["loss", f"{loss}%"]
        if len(command) == 7:
            return
        run(command)
        self.netem_active = True

    def clear_network(self):
        if self.netem_active:
            run(["tc", "qdisc", "del", "dev", self.case["netem_device"], "root"], check=False)
            self.netem_active = False

    def wait_exec(self, node, container, shell, timeout):
        return self.ttc(node, "exec", container, "sh", "-c", shell, timeout=timeout)

    def inspect(self, node, container):
        output = self.ttc(node, "inspect", container).stdout
        records = json.loads(output)
        if len(records) != 1:
            raise RuntimeError(f"inspect returned {len(records)} records")
        return records[0]

    def source_status(self, transaction):
        if not self.source_id or not transaction:
            return None
        result = run([
            self.args.ttcd, "runsc", "--root", self.runsc_root("a"),
            "timetravel", self.source_id, "distributed-status", transaction,
        ], check=False, timeout=15)
        if result.returncode:
            return None
        return json.loads(result.stdout)

    def target_status(self, target_id):
        if not target_id:
            return None
        result = run([
            self.args.ttcd, "runsc", "--root", self.runsc_root("b"),
            "timetravel", target_id, "distributed-dependency", "main",
        ], check=False, timeout=15)
        if result.returncode:
            return None
        return json.loads(result.stdout)

    def run(self):
        started = time.time_ns()
        self.start_daemons()
        # Pull is outside the timed region and is cached across matrix cells.
        if self.ttc("a", "image", "inspect", self.args.image, check=False).returncode != 0:
            self.ttc("a", "pull", self.args.image, timeout=600)
        assets = make_worker(self.root)
        result = self.ttc(
            "a", "run", "-d", "--name", self.source,
            "-v", f"{assets}:/bench:copy", self.args.image,
            "/bench/page-worker", self.case["memory_mib"], self.case["workload"],
            self.case["touch_fraction"], self.case["rounds"],
        )
        self.source_id = result.stdout.strip()
        self.wait_exec("a", self.branch(self.source),
                       "while [ ! -f /tmp/ttc-bench-ready ]; do sleep 0.05; done", 120)
        self.shape_network()
        fork_start = time.monotonic_ns()
        if self.case["capture_mode"] == "batch":
            self.ttc("a", "fork", "--target", "node-b", self.branch(self.source),
                     *(self.branch(target) for target in self.targets), timeout=300)
        elif self.case["capture_mode"] == "sequential":
            for target in self.targets:
                self.ttc("a", "fork", "--target", "node-b", self.branch(self.source),
                         self.branch(target), timeout=300)
        else:
            raise ValueError(f"unknown capture_mode {self.case['capture_mode']!r}")
        fork_end = time.monotonic_ns()
        inspected = [self.inspect("b", target) for target in self.targets]
        self.target_ids = [target["TTC"]["container"]["id"] for target in inspected]
        self.txids = [target["TTC"]["distributed_fork"]["identity"]["transaction_id"] for target in inspected]
        dehydrate_ms = None
        rehydrate_ms = None
        if self.args.verify_dehydrate:
            transition_start = time.monotonic_ns()
            for target in self.targets:
                self.ttc("b", "dehydrate", self.branch(target), timeout=300)
            transition_middle = time.monotonic_ns()
            for target in self.targets:
                self.ttc("b", "rehydrate", self.branch(target), timeout=300)
            transition_end = time.monotonic_ns()
            dehydrate_ms = (transition_middle - transition_start) / 1e6
            rehydrate_ms = (transition_end - transition_middle) / 1e6
        work_start = time.monotonic_ns()
        def run_target(target):
            branch = self.branch(target)
            self.ttc("b", "kill", "--signal", "CONT", branch)
            self.wait_exec("b", branch,
                           "while [ ! -f /tmp/ttc-bench-done ]; do sleep 0.01; done",
                           300)
        with concurrent.futures.ThreadPoolExecutor(max_workers=len(self.targets)) as executor:
            list(executor.map(run_target, self.targets))
        work_end = time.monotonic_ns()
        independent_ms = None
        deadline = time.monotonic() + self.args.independence_timeout
        final = inspected
        while time.monotonic() < deadline:
            final = [self.inspect("b", target) for target in self.targets]
            if all(target["TTC"]["distributed_fork"]["state"] == "independent" for target in final):
                independent_ms = (time.monotonic_ns() - fork_start) / 1e6
                break
            time.sleep(0.05)
        statuses = [self.source_status(txid) for txid in self.txids]
        target_statuses = [self.target_status(target_id) for target_id in self.target_ids]
        source_probe = self.ttc("a", "exec", self.branch(self.source), "true", check=False)
        target_probes = [self.ttc("b", "exec", self.branch(target), "true", check=False) for target in self.targets]
        routed_target_probes = [self.ttc("a", "exec", self.branch(target), "true", check=False) for target in self.targets]
        source_ok = source_probe.returncode == 0
        target_ok = [probe.returncode == 0 for probe in target_probes]
        routed_target_ok = [probe.returncode == 0 for probe in routed_target_probes]
        if not source_ok or not all(target_ok) or not all(routed_target_ok):
            diagnostics = []
            if not source_ok:
                diagnostics.append(f"source via node-a: {source_probe.stderr.strip()}")
            for index, probe in enumerate(target_probes):
                if probe.returncode:
                    diagnostics.append(f"target {index} via node-b: {probe.stderr.strip()}")
            for index, probe in enumerate(routed_target_probes):
                if probe.returncode:
                    diagnostics.append(f"target {index} via node-a: {probe.stderr.strip()}")
            raise RuntimeError("post-workload cluster routing validation failed:\n" + "\n".join(diagnostics))
        return {
            "schema_version": 2,
            "case": self.case,
            "repetition": self.repetition,
            "image": self.args.image,
            "fork_ms": (fork_end - fork_start) / 1e6,
            "workload_ms": (work_end - work_start) / 1e6,
            "dehydrate_ms": dehydrate_ms,
            "rehydrate_ms": rehydrate_ms,
            "independent_ms": independent_ms,
            "source_ok": source_ok,
            "target_ok": target_ok,
            "routed_target_ok": routed_target_ok,
            "dynamic_discovery": self.args.dynamic_discovery,
            "transactions": [target["TTC"]["distributed_fork"] for target in final],
            "source_live_statuses": statuses,
            "target_loader_statuses": target_statuses,
            "wall_ms": (time.time_ns() - started) / 1e6,
        }

    def cleanup(self):
        try:
            self.clear_network()
        except Exception:
            self.netem_active = False
        def safe_ttc(node, *argv, timeout=30):
            try:
                return self.ttc(node, *argv, check=False, timeout=timeout).returncode == 0
            except Exception:
                return False
        # Stop the source first while its peer is reachable. This lets ttcd
        # request a complete target drain for demand-only runs; stopping the
        # target first can intentionally make source shutdown refuse to lose
        # remotely-owned pages.
        stopped_a = safe_ttc("a", "daemon", "stop", timeout=120)
        if not stopped_a and self.source_id:
            # Preserve teardown even when the experiment exposed a dependency
            # bug: explicitly cancel each source snapshot while its Sentry is
            # still reachable, then retry daemon shutdown after target removal.
            for transaction in self.txids:
                try:
                    run([
                        self.args.ttcd, "runsc", "--root", self.runsc_root("a"),
                        "timetravel", self.source_id, "distributed-cancel", transaction,
                    ], check=False, timeout=15)
                except Exception:
                    pass
        for target in self.targets:
            safe_ttc("b", "rm", "-f", target)
        stopped_b = safe_ttc("b", "daemon", "stop", timeout=60)
        if not stopped_a:
            safe_ttc("a", "rm", "-f", self.source)
            stopped_a = safe_ttc("a", "daemon", "stop", timeout=60)
        if stopped_a:
            shutil.rmtree(self.runtime_a, ignore_errors=True)
        if stopped_b:
            shutil.rmtree(self.runtime_b, ignore_errors=True)
        if stopped_a and stopped_b:
            remove_tree(self.data)


DEFAULT_CASE = {
    "name": "default",
    "transfer_bytes": 1048576,
    "max_outstanding_bytes": 67108864,
    "max_parallel": 4,
    "ack_batch_pages": 256,
    "ack_interval": "1ms",
    "prefetch": "none",
    "push": "demand-only",
    "workload": "sequential",
    "memory_mib": 64,
    "rounds": 2,
    "touch_fraction": 1.0,
    "fanout": 1,
    "capture_mode": "batch",
    "netem_device": "",
    "delay_ms": 0,
    "rate_mbit": 0,
    "loss_percent": 0,
}


def load_cases(path):
    if not path:
        return [dict(DEFAULT_CASE)]
    document = json.loads(pathlib.Path(path).read_text(encoding="utf-8"))
    cases = document.get("cases", [])
    if not cases:
        raise ValueError("matrix must contain a non-empty cases array")
    result = []
    for supplied in cases:
        case = dict(DEFAULT_CASE)
        case.update(supplied)
        if case["transfer_bytes"] < 1048576:
            raise ValueError(
                f"case {case['name']!r} transfer_bytes must be at least "
                "1048576 for TTC's atomic sparse memory chunks"
            )
        result.append(case)
    return result


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--ttc", required=True)
    parser.add_argument("--ttcd", required=True)
    parser.add_argument("--image", default="busybox:1.36")
    parser.add_argument("--matrix", help="JSON matrix file; omit for one default case")
    parser.add_argument("--repetitions", type=int, default=1)
    parser.add_argument("--work-dir", help="persistent cache/results directory")
    parser.add_argument(
        "--data-dir",
        help="fresh shared TTC_ROOT; defaults to WORK_DIR/data",
    )
    parser.add_argument("--output", help="JSONL output (default: stdout only)")
    parser.add_argument("--independence-timeout", type=float, default=2.0)
    parser.add_argument(
        "--cluster-lease", default="3s",
        help="cluster membership lease (increase when shared storage is throttled)",
    )
    parser.add_argument(
        "--dynamic-discovery", action="store_true",
        help="discover peers from shared lease records instead of --cluster-peer",
    )
    parser.add_argument(
        "--verify-dehydrate", action="store_true",
        help="dehydrate and rehydrate every remote target before its workload",
    )
    parser.add_argument(
        "--preserve-on-failure", action="store_true",
        help="leave daemons and dedicated roots intact when a run fails",
    )
    args = parser.parse_args()
    root = pathlib.Path(args.work_dir or tempfile.mkdtemp(prefix="ttc-distributed-benchmark.")).resolve()
    if args.data_dir:
        args.data_dir = pathlib.Path(args.data_dir).resolve()
        if args.data_dir.exists():
            parser.error("--data-dir must name a new, dedicated test directory")
    root.mkdir(parents=True, exist_ok=True)
    make_certificates(root)
    output = open(args.output, "a", encoding="utf-8") if args.output else None
    try:
        for case in load_cases(args.matrix):
            for repetition in range(args.repetitions):
                experiment = Experiment(args, case, repetition, root)
                failed = False
                try:
                    record = experiment.run()
                except Exception as error:
                    failed = True
                    record = {
                        "schema_version": 2, "case": case,
                        "repetition": repetition, "error": str(error),
                    }
                finally:
                    if failed and args.preserve_on_failure:
                        record["preserved_work_dir"] = str(root)
                        record["preserved_data_dir"] = str(experiment.data)
                    else:
                        try:
                            experiment.cleanup()
                        except Exception as cleanup_error:
                            record["cleanup_error"] = str(cleanup_error)
                line = json.dumps(record, sort_keys=True)
                print(line, flush=True)
                if output:
                    output.write(line + "\n")
                    output.flush()
                if "error" in record:
                    return 1
    finally:
        if output:
            output.close()
        if not args.work_dir:
            shutil.rmtree(root, ignore_errors=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
