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

The convenience API covers run, start, exec, local and remote batch fork,
pause/resume, dehydration, checkpoint/restore, stop/restart, signal, wait, and
removal. For example, one source pause can create several remote branches in
the same target family:

```python
ttc.fork_many(
    "demo@main",
    ["remote@candidate-a", "remote@candidate-b"],
    target_node="node-b",
).check()
```

`Client.command([...])` exposes every other non-interactive TTC command:

```python
result = ttc.command(["port", "ls", "--json", "demo@main"])
if result.ok:
    print(result.json())
else:
    print(result.stderr_text())
```

Pass `input=b"..."` to stream stdin. Reuse an explicit `request_id` only when
deliberately retrying the same mutation. Interactive TTY attachment remains a
CLI feature.

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
nonzero results, and stdin flow control across a payload larger than 1 MiB.
