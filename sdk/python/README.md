# ttcx for Python

`ttcx` is the Python SDK for TimeTravelContainer. It connects directly to the
per-user `ttcd` Unix socket; it does not launch or parse output from the `ttc`
CLI.

## Install

From this checkout:

```sh
python3 -m pip install ./sdk/python
```

For development:

```sh
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -e ./sdk/python
```

Start `ttcd` before using the SDK:

```sh
ttc daemon start
```

## Use

```python
from ttcx import Client

ttc = Client()
print(ttc.ping())

ttc.pull("alpine:3.20").check()
ttc.run("alpine:3.20", ["sleep", "infinity"],
        options=["-d", "--name", "demo"]).check()
ttc.fork("demo@main", "experiment").check()
ttc.dehydrate("demo@experiment").check()
ttc.rehydrate("demo@experiment").check()

for family in ttc.list():
    print(family)

result = ttc.exec("demo@experiment", ["sh", "-c", "uname -a"])
print(result.stdout_text())
result.check()
```

The convenience API covers run, start, named exec sessions, local and remote
batch fork, elastic migration/node management, pause/resume, dehydration,
commit, resource accounting, filesystem
diff, copy, terminal observations, checkpoint/restore, stop/restart, signal,
wait, and removal. For example, one source pause can create several branches:

```python
ttc.fork_many(
    "demo@main",
    ["candidate-a", "candidate-b"],
).check()
ttc.migrate("demo@candidate-a", "node-b").check()
print(ttc.nodes())
print(ttc.rebalance_node("node-a", target_node="node-b"))
```

Harness-oriented helpers return structured JSON or ordinary command results:

```python
ttc.exec_session("demo@candidate-a", ["sh"], session="build1", tty=True).check()
print(ttc.tty_stat("demo@candidate-a", "build1"))
print(ttc.stats("demo@candidate-a"))
print(ttc.diff("demo@candidate-a"))
ttc.commit("demo@candidate-a", "local/result:latest",
           changes=["LABEL timetravel.usage=review"]).check()
archive = ttc.save(["local/result:latest"]).stdout
ttc.load(archive=archive).check()
```

`load(archive=...)` and `save()` buffer the archive in the client. For large
archives, use `input`/`output` paths visible to `ttcd`, or the streaming command
API.

Families created by `run` are live-fork eligible by default. If an application
deliberately needs an incompatible external feature, pass both that option and
`--waive-live-fork` (for example
`options=["--network=host", "--waive-live-fork"]`); that family is permanently
non-forkable.

`Client.command([...])` exposes every other non-interactive TTC command:

```python
result = ttc.command(["port", "ls", "--json", "demo@main"])
if result.ok:
    print(result.json())
else:
    print(result.stderr_text())
```

Pass `input=b"..."` to a buffered command. Reuse an explicit `request_id` only
when deliberately retrying the same mutation. Long-lived operations use
`TTCStream` and callbacks:

```python
import json
import sys

watch = ttc.watch_tty(
    "demo@candidate-a", "build1",
    lambda payload: print(json.loads(payload)),
)
shell = ttc.attach(
    "demo@candidate-a", exec_session="build1", size=(40, 120),
    on_stdout=lambda chunk: sys.stdout.buffer.write(chunk),
)
shell.write("make test\n")
shell.resize(50, 160)
```

`close_stdin()` explicitly delivers EOF. `close()` is local cancellation, so
`wait()` raises `StreamCancelled` instead of fabricating status zero.
`watch_events()` and `follow_logs()` expose the other unbounded streams.

`checkpoint()` and `restore()` convert local paths to absolute paths before
sending them to `ttcd`. Other raw commands that name host files should likewise
use absolute paths because the daemon does not share the SDK process's current
working directory.

Set `TTC_RUNTIME_DIR` or `XDG_RUNTIME_DIR` as for the CLI, or pass
`Client("/path/to/ttcd.sock")`.

## Test

```sh
PYTHONPATH=sdk/python/src python3 -m unittest discover -s sdk/python/tests
```

The tests run against an in-process protocol server and cover framing,
terminal-observation records, nonzero results, and stdin flow control across a
payload larger than 1 MiB.
