# ttcx for TypeScript

`ttcx` is the TypeScript and Node.js SDK for TimeTravelContainer. It connects
directly to `ttcd` over its per-user Unix socket and never shells out to `ttc`.
Node.js 20 or newer is required.

## Install

From this checkout:

```sh
npm install ./sdk/typescript
```

Start `ttcd`, then use the client:

```ts
import { Client } from "ttcx";

const ttc = new Client();
console.log(await ttc.ping());

(await ttc.pull("alpine:3.20")).check();
(await ttc.run("alpine:3.20", ["sleep", "infinity"],
               ["-d", "--name", "demo"])).check();
(await ttc.fork("demo@main", "experiment")).check();
(await ttc.dehydrate("demo@experiment")).check();
(await ttc.rehydrate("demo@experiment")).check();

console.log(await ttc.list());
const result = await ttc.exec("demo@experiment", ["uname", "-a"]);
console.log(result.stdoutText());
result.check();
```

The convenience API covers run, start, named exec sessions, local and remote
batch fork, elastic migration/node management, pause/resume, dehydration,
commit, resource accounting, filesystem
diff, copy, terminal observations, checkpoint/restore, stop/restart, signal,
wait, and removal. A coordinated local fan-out uses one source pause:

```ts
(await ttc.forkMany(
  "demo@main",
  ["candidate-a", "candidate-b"],
)).check();
(await ttc.migrate("demo@candidate-a", "node-b")).check();
console.log(await ttc.nodes());
console.log(await ttc.rebalanceNode("node-a", "node-b"));
```

Harness-oriented helpers are available directly:

```ts
(await ttc.execSession("demo@candidate-a", ["sh"], "build1", { tty: true })).check();
console.log(await ttc.ttyStat("demo@candidate-a", "build1"));
console.log(await ttc.stats("demo@candidate-a"));
console.log(await ttc.diff("demo@candidate-a"));
(await ttc.commit("demo@candidate-a", "local/result:latest",
                  { changes: ["LABEL timetravel.usage=review"] })).check();
const archive = await ttc.save(["local/result:latest"]);
await ttc.load({ archive: archive.stdout });
```

`load({ archive })` and `save()` buffer the archive in the client. For large
archives, use `input`/`output` paths visible to `ttcd`, or the streaming command
API.

`ttyStat()` is the synchronous terminal-observation API. Long-lived operations
use `TtcStream` without buffering output:

```ts
const watch = ttc.watchTty("demo@candidate-a", "build1", {
  onObservation: (payload) => console.log(JSON.parse(payload.toString())),
});

const shell = ttc.attach("demo@candidate-a", {
  exec: "build1",
  size: { rows: 40, cols: 120 },
  onStdout: (chunk) => process.stdout.write(chunk),
});
shell.write("make test\n");
shell.resize(50, 160);
```

Call `closeStdin()` only when the guest should observe EOF. `close()` cancels
the local stream and rejects `exited` with `StreamCancelled`; only a daemon
status frame resolves `exited` successfully. `watchEvents()` and
`followLogs()` expose the other unbounded streams.

Families created by `run` are live-fork eligible by default. Use an option set
such as `["--network=host", "--waive-live-fork"]` only when deliberately
creating a permanently non-forkable family.

Use `client.command(["port", "ls", "--json", "demo@main"])` for any other
non-interactive TTC command. Results keep stdout, stderr, and exit status
separate. Pass `input` for stdin and an explicit `requestId` when deliberately
retrying the same mutation.

`checkpoint()` and `restore()` make file paths absolute before sending them to
the daemon. Use absolute host paths in raw file-oriented commands because
`ttcd` does not share the client process's working directory.

Set `TTC_SOCKET` or `XDG_RUNTIME_DIR`, or construct
`new Client({socketPath: "/path/to/ttcd.sock"})`. SDK clients never need
`TTC_ROOT` or `TTC_LOCAL_ROOT`.

## Develop and test

From `sdk/typescript`:

```sh
npm install
npm test
```
