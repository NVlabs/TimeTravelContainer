#!/usr/bin/env python3
"""End-to-end elastic TTC capacity-provider and drain smoke test.

The same file is the provider executable. ttcd invokes it with no arguments;
the TTC_CAPACITY_REQUEST_ID environment variable selects provider mode.
"""

import argparse
import errno
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
    # Network filesystems and asynchronous daemon teardown may report a short
    # EBUSY/ENOTEMPTY window after the last TTC process exits. Retry removal of
    # this dedicated test tree, but keep every unexpected or persistent error
    # visible to the test.
    deadline = time.monotonic() + 30
    while path.exists():
        for current, directories, _files in os.walk(path, topdown=True):
            if not os.path.islink(current):
                os.chmod(current, 0o700)
            for directory in directories:
                child = os.path.join(current, directory)
                if not os.path.islink(child):
                    os.chmod(child, 0o700)
        try:
            shutil.rmtree(path)
            return
        except OSError as error:
            if error.errno not in (errno.EBUSY, errno.ENOTEMPTY) or time.monotonic() >= deadline:
                raise
            time.sleep(0.25)


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
        "--workload-command",
        help="override the long-running source command (default: counter loop)",
    )
    parser.add_argument(
        "--verify-command",
        help="shell command to run independently in every migrated branch",
    )
    parser.add_argument("--command-timeout", type=int, default=600)
    parser.add_argument(
        "--cluster-lease", default="3s",
        help="cluster membership lease (increase when throttling shared storage)",
    )
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
    provider = pathlib.Path(__file__).resolve()
    provider.chmod(0o755)

    def env(runtime):
        result = os.environ.copy()
        result.update({
            "TTC_ROOT": str(data),
            "TTC_LOCAL_ROOT": str(runtime / "data"),
            "TTC_CAPACITY_TEST_ROOT": str(root),
        })
        result.pop("TTC_DATA_ROOT", None)
        result.pop("TTC_RUNTIME_DIR", None)
        return result

    def ttc(runtime, *argv, check=True, timeout=180):
        return run([args.ttc, *argv], env=env(runtime), check=check, timeout=timeout)

    common = ["--unsafe-no-host-isolation", "--cluster-lease", args.cluster_lease]
    started_a = False
    started_b = False
    succeeded = False
    began = time.monotonic()
    try:
        ttc(runtime_a, "daemon", "start", *common,
            "--listen", "127.0.0.1:28443",
            "--advertise", "127.0.0.1:28443", "--node", "node-a",
            "--memory-target", "512GiB",
            "--cluster-capacity-command", provider)
        started_a = True
        run_args = ["run", "-d", "--name", "capacity-smoke"]
        if args.network == "ttc":
            run_args.append("--network=ttc")
        if args.nat:
            run_args.append("--nat")
        workload = args.workload_command or (
            "i=0; while :; do i=$((i+1)); echo $i > /tmp/counter; sleep 0.05; done"
        )
        run_args.extend([args.image, "sh", "-c", workload])
        ttc(runtime_a, *run_args, timeout=args.command_timeout)
        prepare_seconds = None
        if args.prepare_command:
            prepare_began = time.monotonic()
            ttc(runtime_a, "exec", "capacity-smoke@main", "sh", "-lc",
                args.prepare_command, timeout=args.command_timeout)
            prepare_seconds = round(time.monotonic() - prepare_began, 3)
        ttc(runtime_a, "fork", "capacity-smoke@main", "child")

        drain = ttc(runtime_a, "node", "stop", "node-a", "--migrate-all", check=False)
        if drain.returncode == 0 or "capacity request" not in drain.stderr:
            raise RuntimeError(f"initial drain did not request capacity: {drain}")

        accepted = wait_for(
            "provider retry and acceptance",
            lambda: json.loads((root / "provider-accepted.json").read_text(encoding="utf-8")),
            timeout=30,
        )
        ttc(runtime_b, "daemon", "start", *common,
            "--listen", "127.0.0.1:38443",
            "--advertise", "127.0.0.1:38443", "--node", "node-b",
            "--memory-target", "2TiB",
            "--cluster-capacity-command", provider)
        started_b = True

        request_path = data / "cluster" / "capacity-requests" / "node-a.json"
        wait_for("membership-confirmed outbox consumption", lambda: not request_path.exists())
        wait_for("automatic drain and daemon stop",
                 lambda: not (runtime_a / "data" / "ttcd.sock").exists(),
                 timeout=args.command_timeout)
        started_a = False
        inspected = json.loads(ttc(runtime_b, "inspect", "capacity-smoke").stdout)[0]
        branches = inspected["TTC"]["container"]["branches"]
        if {branch["id"]: branch.get("node_id") for branch in branches} != {"main": "node-b", "child": "node-b"}:
            raise RuntimeError(f"unexpected placements after drain: {branches!r}")
        verification_seconds = {}
        for branch in ("main", "child"):
            counter = ttc(runtime_b, "exec", f"capacity-smoke@{branch}", "cat", "/tmp/counter").stdout.strip()
            if not counter.isdigit() or int(counter) <= 0:
                raise RuntimeError(f"branch {branch} counter did not survive: {counter!r}")
            if args.verify_command:
                verify_began = time.monotonic()
                ttc(runtime_b, "exec", f"capacity-smoke@{branch}", "sh", "-lc",
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
        ttc(runtime_b, "rm", "-f", "capacity-smoke", timeout=args.command_timeout)
        succeeded = True
    finally:
        if args.preserve_on_failure and not succeeded:
            print(f"preserved failed qualification at {root}", file=sys.stderr)
            # Preserve the environment without suppressing the exception that
            # explains why qualification failed.
            started_a = False
            started_b = False
            args.keep = True
        # On failure, retain shared family metadata until both physical owners
        # have stopped their local branches. Removing it first strands
        # unaddressable Sentries and turns cleanup failure into misleading
        # migration failure. Source shutdown runs first so it can settle any
        # dependency still served to B.
        if started_b and succeeded:
            # Remove image-owned mapped-UID trees while a daemon can still
            # enter its extraction namespace. A plain host rmtree may not own
            # those inodes, even in unsafe-no-host-isolation test mode.
            # The family itself was already removed by its owner above.
            ttc(runtime_b, "rmi", "--force", args.image, check=False, timeout=args.command_timeout)
        if started_a:
            ttc(runtime_a, "daemon", "stop", "--stop-all", check=False, timeout=args.command_timeout)
        if started_b:
            ttc(runtime_b, "daemon", "stop", "--stop-all", check=False, timeout=args.command_timeout)
        if not args.keep:
            # The stop response precedes the serving goroutine's final durable
            # intent cleanup by a small interval. Wait for both sockets to
            # disappear so that cleanup cannot recreate the test root.
            wait_for(
                "daemon shutdown",
                lambda: not (runtime_a / "data" / "ttcd.sock").exists()
                and not (runtime_b / "data" / "ttcd.sock").exists(),
                timeout=args.command_timeout,
            )
            time.sleep(1)
            remove_tree(root)
            if args.data_dir is not None and data.exists():
                remove_tree(data)


if __name__ == "__main__":
    if os.environ.get("TTC_CAPACITY_REQUEST_ID"):
        sys.exit(provider_main())
    main()
