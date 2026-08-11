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

`Client.command([...])` exposes every non-interactive TTC command without
waiting for a convenience method:

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

Set `TTC_RUNTIME_DIR` or `XDG_RUNTIME_DIR` as for the CLI, or pass
`Client("/path/to/ttcd.sock")`.

## Test

```sh
PYTHONPATH=sdk/python/src python3 -m unittest discover -s sdk/python/tests
```

The tests run against an in-process protocol server and cover framing,
nonzero results, and stdin flow control across a payload larger than 1 MiB.
