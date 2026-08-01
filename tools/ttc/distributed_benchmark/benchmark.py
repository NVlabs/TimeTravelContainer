#!/usr/bin/env python3
"""Reproducible two-node TTC distributed-fork workload benchmark."""

import argparse
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
        self.data = root / "data"
        unique = f"{os.getpid()}-{repetition}-{time.time_ns()}"
        self.source = f"bench-source-{unique}"
        self.target = f"bench-target-{unique}"
        self.source_id = ""
        self.target_id = ""
        self.txid = ""
        self.netem_active = False

    def env(self, node):
        runtime = self.runtime_a if node == "a" else self.runtime_b
        result = os.environ.copy()
        result["TTC_RUNTIME_DIR"] = str(runtime)
        result["TTC_DATA_ROOT"] = str(self.data)
        return result

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
            "--cluster-transfer-bytes", self.case["transfer_bytes"],
            "--cluster-max-outstanding-bytes", self.case["max_outstanding_bytes"],
            "--cluster-max-parallel", self.case["max_parallel"],
            "--cluster-ack-batch-pages", self.case["ack_batch_pages"],
            "--cluster-ack-interval", self.case["ack_interval"],
            "--cluster-prefetch", self.case["prefetch"],
            "--cluster-push", self.case["push"],
        ]
        self.ttc("a", "daemon", "start", *common,
                 "--cluster-listen", "127.0.0.1:27443",
                 "--cluster-node-id", "node-a",
                 "--cluster-cert", self.root / "node-a.pem",
                 "--cluster-key", self.root / "node-a.key",
                 "--cluster-peer", "node-b=https://127.0.0.1:37443,node-b")
        try:
            self.ttc("b", "daemon", "start", *common,
                     "--cluster-listen", "127.0.0.1:37443",
                     "--cluster-node-id", "node-b",
                     "--cluster-cert", self.root / "node-b.pem",
                     "--cluster-key", self.root / "node-b.key",
                     "--cluster-peer", "node-a=https://127.0.0.1:27443,node-a")
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

    def source_status(self):
        if not self.source_id or not self.txid:
            return None
        result = run([
            self.args.ttcd, "runsc", "--root", self.runtime_a / "runsc",
            "timetravel", self.source_id, "distributed-status", self.txid,
        ], check=False, timeout=15)
        if result.returncode:
            return None
        return json.loads(result.stdout)

    def target_status(self):
        if not self.target_id:
            return None
        result = run([
            self.args.ttcd, "runsc", "--root", self.runtime_b / "runsc",
            "timetravel", self.target_id, "distributed-dependency", "main",
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
        self.wait_exec("a", self.source,
                       "while [ ! -f /tmp/ttc-bench-ready ]; do sleep 0.05; done", 120)
        self.shape_network()
        fork_start = time.monotonic_ns()
        self.ttc("a", "fork", "--target", "node-b", self.source, self.target, timeout=300)
        fork_end = time.monotonic_ns()
        target = self.inspect("b", self.target)
        self.target_id = target["TTC"]["container"]["id"]
        distributed = target["TTC"]["distributed_fork"]
        self.txid = distributed["identity"]["transaction_id"]
        work_start = time.monotonic_ns()
        self.ttc("b", "kill", "--signal", "CONT", self.target)
        self.wait_exec("b", self.target,
                       "while [ ! -f /tmp/ttc-bench-done ]; do sleep 0.01; done",
                       300)
        work_end = time.monotonic_ns()
        independent_ms = None
        deadline = time.monotonic() + self.args.independence_timeout
        final = target
        while time.monotonic() < deadline:
            final = self.inspect("b", self.target)
            if final["TTC"]["distributed_fork"]["state"] == "independent":
                independent_ms = (time.monotonic_ns() - fork_start) / 1e6
                break
            time.sleep(0.05)
        status = self.source_status()
        target_status = self.target_status()
        source_ok = self.ttc("a", "exec", self.source, "true", check=False).returncode == 0
        target_ok = self.ttc("b", "exec", self.target, "true", check=False).returncode == 0
        return {
            "schema_version": 1,
            "case": self.case,
            "repetition": self.repetition,
            "image": self.args.image,
            "fork_ms": (fork_end - fork_start) / 1e6,
            "workload_ms": (work_end - work_start) / 1e6,
            "independent_ms": independent_ms,
            "source_ok": source_ok,
            "target_ok": target_ok,
            "transaction": final["TTC"]["distributed_fork"],
            "source_live_status": status,
            "target_loader_status": target_status,
            "wall_ms": (time.time_ns() - started) / 1e6,
        }

    def cleanup(self):
        self.clear_network()
        # Stop the source first while its peer is reachable. This lets ttcd
        # request a complete target drain for demand-only runs; stopping the
        # target first can intentionally make source shutdown refuse to lose
        # remotely-owned pages.
        stopped_a = self.ttc("a", "daemon", "stop", check=False, timeout=120).returncode == 0
        self.ttc("b", "rm", "-f", self.target, check=False, timeout=30)
        stopped_b = self.ttc("b", "daemon", "stop", check=False, timeout=60).returncode == 0
        if stopped_a:
            shutil.rmtree(self.runtime_a, ignore_errors=True)
        if stopped_b:
            shutil.rmtree(self.runtime_b, ignore_errors=True)


DEFAULT_CASE = {
    "name": "default",
    "transfer_bytes": 65536,
    "max_outstanding_bytes": 67108864,
    "max_parallel": 4,
    "ack_batch_pages": 256,
    "ack_interval": "1ms",
    "prefetch": "sequential",
    "push": "dirty-orphan",
    "workload": "sequential",
    "memory_mib": 64,
    "rounds": 2,
    "touch_fraction": 1.0,
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
    parser.add_argument("--output", help="JSONL output (default: stdout only)")
    parser.add_argument("--independence-timeout", type=float, default=2.0)
    args = parser.parse_args()
    root = pathlib.Path(args.work_dir or tempfile.mkdtemp(prefix="ttc-distributed-benchmark."))
    root.mkdir(parents=True, exist_ok=True)
    make_certificates(root)
    output = open(args.output, "a", encoding="utf-8") if args.output else None
    try:
        for case in load_cases(args.matrix):
            for repetition in range(args.repetitions):
                experiment = Experiment(args, case, repetition, root)
                try:
                    record = experiment.run()
                except Exception as error:
                    record = {
                        "schema_version": 1, "case": case,
                        "repetition": repetition, "error": str(error),
                    }
                finally:
                    experiment.cleanup()
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
