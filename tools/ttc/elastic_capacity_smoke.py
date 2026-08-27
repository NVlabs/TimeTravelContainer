#!/usr/bin/env python3
"""End-to-end elastic TTC capacity-provider and drain smoke test.

The same file is the provider executable. ttcd invokes it with no arguments;
the TTC_CAPACITY_REQUEST_ID environment variable selects provider mode.
"""

import argparse
import json
import os
import pathlib
import shutil
import subprocess
import sys
import tempfile
import time


def run(argv, *, env=None, timeout=180, check=True):
    result = subprocess.run(
        [str(arg) for arg in argv], env=env, timeout=timeout, check=False,
        text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
    )
    if check and result.returncode:
        raise RuntimeError(
            f"command failed ({result.returncode}): {' '.join(map(str, argv))}\n"
            f"stdout:\n{result.stdout}\nstderr:\n{result.stderr}"
        )
    return result


def provider_main():
    root = pathlib.Path(os.environ["TTC_CAPACITY_TEST_ROOT"])
    payload = json.load(sys.stdin)
    request_id = os.environ["TTC_CAPACITY_REQUEST_ID"]
    if payload["request_id"] != request_id:
        raise RuntimeError("request ID differs between JSON and environment")
    calls = root / "provider-calls.jsonl"
    with calls.open("a", encoding="utf-8") as output:
        output.write(json.dumps(payload, sort_keys=True) + "\n")
    first_failure = root / "provider-first-failure"
    try:
        first_failure.touch(exist_ok=False)
    except FileExistsError:
        (root / "provider-accepted.json").write_text(
            json.dumps(payload, sort_keys=True), encoding="utf-8"
        )
        return 0
    print("injected scheduler outage", file=sys.stderr)
    return 75


def make_certificates(root):
    run([
        "openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
        "-days", "2", "-subj", "/CN=ttc-capacity-smoke-ca",
        "-keyout", root / "ca.key", "-out", root / "ca.pem",
    ])
    for node in ("node-a", "node-b"):
        run([
            "openssl", "req", "-newkey", "rsa:2048", "-nodes",
            "-subj", f"/CN={node}", "-keyout", root / f"{node}.key",
            "-out", root / f"{node}.csr",
        ])
        extension = root / f"{node}.ext"
        extension.write_text(
            f"subjectAltName=DNS:{node},IP:127.0.0.1\n"
            "extendedKeyUsage=serverAuth,clientAuth\n", encoding="utf-8",
        )
        run([
            "openssl", "x509", "-req", "-days", "2",
            "-in", root / f"{node}.csr", "-CA", root / "ca.pem",
            "-CAkey", root / "ca.key", "-CAcreateserial",
            "-extfile", extension, "-out", root / f"{node}.pem",
        ])


def wait_for(description, predicate, timeout=180):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            last = predicate()
            if last:
                return last
        except (FileNotFoundError, json.JSONDecodeError):
            pass
        time.sleep(0.25)
    raise RuntimeError(f"timed out waiting for {description}; last={last!r}")


def remove_tree(path):
    # Images legitimately contain mode-000 files and non-writable directories.
    # Deletion depends on each parent directory's write bit, not the child's.
    for current, directories, _files in os.walk(path, topdown=True):
        if not os.path.islink(current):
            os.chmod(current, 0o700)
        for directory in directories:
            child = os.path.join(current, directory)
            if not os.path.islink(child):
                os.chmod(child, 0o700)
    shutil.rmtree(path)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--ttc", required=True, type=pathlib.Path)
    parser.add_argument("--work-dir", type=pathlib.Path)
    parser.add_argument(
        "--data-dir", type=pathlib.Path,
        help="shared TTC data root (defaults to WORK_DIR/data)",
    )
    parser.add_argument("--image", default="busybox:1.36")
    parser.add_argument("--network", choices=("none", "ttc"), default="none")
    parser.add_argument("--nat", action="store_true")
    parser.add_argument(
        "--prepare-command",
        help="shell command to run in the source before forking",
    )
    parser.add_argument(
        "--verify-command",
        help="shell command to run independently in every migrated branch",
    )
    parser.add_argument("--command-timeout", type=int, default=600)
    parser.add_argument("--keep", action="store_true")
    parser.add_argument(
        "--preserve-on-failure", action="store_true",
        help="leave daemons and workloads running when qualification fails",
    )
    args = parser.parse_args()
    if args.nat and args.network != "ttc":
        parser.error("--nat requires --network=ttc")
    args.ttc = args.ttc.resolve()
    root = args.work_dir or pathlib.Path(tempfile.mkdtemp(prefix="ttc-elastic-capacity-"))
    root = root.resolve()
    root.mkdir(parents=True, exist_ok=True)
    root.chmod(0o700)
    data = (args.data_dir or (root / "data")).resolve()
    if args.data_dir is not None and data.exists():
        parser.error("--data-dir must name a new, dedicated test directory")
    runtime_a = root / "runtime-a"
    runtime_b = root / "runtime-b"
    for path in (data, runtime_a, runtime_b):
        path.mkdir(mode=0o700, parents=True, exist_ok=True)
    make_certificates(root)
    provider = pathlib.Path(__file__).resolve()
    provider.chmod(0o755)

    def env(runtime):
        result = os.environ.copy()
        result.update({
            "TTC_DATA_ROOT": str(data),
            "TTC_RUNTIME_DIR": str(runtime),
            "TTC_CAPACITY_TEST_ROOT": str(root),
        })
        return result

    def ttc(runtime, *argv, check=True, timeout=180):
        return run([args.ttc, *argv], env=env(runtime), check=check, timeout=timeout)

    common = [
        "--unsafe-no-host-isolation", "--allow-slow-volume-copy",
        "--cluster-repository-id", "capacity-smoke",
        "--cluster-ca", root / "ca.pem",
        "--cluster-page-listen", "127.0.0.1:0",
        "--cluster-page-advertise", "127.0.0.1:0",
        "--cluster-lease", "3s",
    ]
    started_a = False
    started_b = False
    succeeded = False
    began = time.monotonic()
    try:
        ttc(runtime_a, "daemon", "start", *common,
            "--cluster-listen", "127.0.0.1:28443",
            "--cluster-advertise", "https://127.0.0.1:28443,node-a",
            "--cluster-node-id", "node-a",
            "--cluster-cert", root / "node-a.pem", "--cluster-key", root / "node-a.key",
            "--cluster-memory-target", "512GiB",
            "--cluster-capacity-command", provider)
        started_a = True
        run_args = ["run", "-d", "--name", "capacity-smoke"]
        if args.network == "ttc":
            run_args.append("--network=ttc")
        if args.nat:
            run_args.append("--nat")
        run_args.extend([
            args.image, "sh", "-c",
            "i=0; while :; do i=$((i+1)); echo $i > /tmp/counter; sleep 0.05; done",
        ])
        ttc(runtime_a, *run_args, timeout=args.command_timeout)
        prepare_seconds = None
        if args.prepare_command:
            prepare_began = time.monotonic()
            ttc(runtime_a, "exec", "capacity-smoke@main", "sh", "-lc",
                args.prepare_command, timeout=args.command_timeout)
            prepare_seconds = round(time.monotonic() - prepare_began, 3)
        ttc(runtime_a, "fork", "capacity-smoke@main", "child")

        drain = ttc(runtime_a, "node", "drain", "node-a", check=False)
        if drain.returncode == 0 or "capacity request" not in drain.stderr:
            raise RuntimeError(f"initial drain did not request capacity: {drain}")

        accepted = wait_for(
            "provider retry and acceptance",
            lambda: json.loads((root / "provider-accepted.json").read_text(encoding="utf-8")),
            timeout=30,
        )
        ttc(runtime_b, "daemon", "start", *common,
            "--cluster-listen", "127.0.0.1:38443",
            "--cluster-advertise", "https://127.0.0.1:38443,node-b",
            "--cluster-node-id", "node-b",
            "--cluster-cert", root / "node-b.pem", "--cluster-key", root / "node-b.key",
            "--cluster-memory-target", "2TiB",
            "--cluster-capacity-command", provider)
        started_b = True

        request_path = data / "cluster" / "capacity-requests" / "node-a.json"
        wait_for("membership-confirmed outbox consumption", lambda: not request_path.exists())
        intent_path = data / "cluster" / "drains" / "node-a.json"
        wait_for(
            "automatic drain completion",
            lambda: json.loads(intent_path.read_text(encoding="utf-8")).get("state") == "complete",
        )
        inspected = json.loads(ttc(runtime_a, "inspect", "capacity-smoke").stdout)[0]
        branches = inspected["TTC"]["container"]["branches"]
        if {branch["id"]: branch.get("node_id") for branch in branches} != {"main": "node-b", "child": "node-b"}:
            raise RuntimeError(f"unexpected placements after drain: {branches!r}")
        verification_seconds = {}
        for branch in ("main", "child"):
            counter = ttc(runtime_a, "exec", f"capacity-smoke@{branch}", "cat", "/tmp/counter").stdout.strip()
            if not counter.isdigit() or int(counter) <= 0:
                raise RuntimeError(f"branch {branch} counter did not survive: {counter!r}")
            if args.verify_command:
                verify_began = time.monotonic()
                ttc(runtime_a, "exec", f"capacity-smoke@{branch}", "sh", "-lc",
                    args.verify_command, timeout=args.command_timeout)
                verification_seconds[branch] = round(time.monotonic() - verify_began, 3)
        calls = [json.loads(line) for line in (root / "provider-calls.jsonl").read_text(encoding="utf-8").splitlines()]
        if len(calls) < 2 or {call["request_id"] for call in calls} != {accepted["request_id"]}:
            raise RuntimeError(f"provider retries were not idempotent: {calls!r}")
        result = {
            "result": "pass",
            "wall_seconds": round(time.monotonic() - began, 3),
            "request_id": accepted["request_id"],
            "provider_attempts": len(calls),
            "migrated_branches": 2,
            "image": args.image,
        }
        if prepare_seconds is not None:
            result["prepare_seconds"] = prepare_seconds
        if verification_seconds:
            result["verification_seconds"] = verification_seconds
        print(json.dumps(result, indent=2, sort_keys=True))
        ttc(runtime_a, "rm", "-f", "capacity-smoke")
        succeeded = True
    finally:
        if args.preserve_on_failure and not succeeded:
            print(f"preserved failed qualification at {root}", file=sys.stderr)
        else:
            if started_a:
                # Remove image-owned mapped-UID trees while the daemon can still
                # enter its extraction namespace. A plain host rmtree may not own
                # those inodes, even in unsafe-no-host-isolation test mode.
                ttc(runtime_a, "rm", "-f", "capacity-smoke", check=False, timeout=30)
                ttc(runtime_a, "rmi", "--force", args.image, check=False, timeout=30)
            if started_b:
                ttc(runtime_b, "daemon", "stop", "--stop-all", check=False, timeout=30)
            if started_a:
                ttc(runtime_a, "daemon", "stop", "--stop-all", check=False, timeout=30)
            if not args.keep:
                # The stop response precedes the serving goroutine's final durable
                # intent cleanup by a small interval. Wait for both sockets to
                # disappear so that cleanup cannot recreate the test root.
                wait_for(
                    "daemon shutdown",
                    lambda: not (runtime_a / "ttcd.sock").exists() and not (runtime_b / "ttcd.sock").exists(),
                    timeout=30,
                )
                time.sleep(1)
                remove_tree(root)
                if args.data_dir is not None and data.exists():
                    remove_tree(data)


if __name__ == "__main__":
    if os.environ.get("TTC_CAPACITY_REQUEST_ID"):
        sys.exit(provider_main())
    main()
