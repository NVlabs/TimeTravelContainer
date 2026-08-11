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
(await ttc.fork("demo@main", "demo@experiment")).check();
(await ttc.dehydrate("demo@experiment")).check();
(await ttc.rehydrate("demo@experiment")).check();

console.log(await ttc.list());
const result = await ttc.exec("demo@experiment", ["uname", "-a"]);
console.log(result.stdoutText());
result.check();
```

Use `client.command(["port", "ls", "--json", "demo@main"])` for any
non-interactive TTC command. Results keep stdout, stderr, and exit status
separate. Pass `input` for stdin and an explicit `requestId` when deliberately
retrying the same mutation.

Set `TTC_RUNTIME_DIR` or `XDG_RUNTIME_DIR`, or construct
`new Client({socketPath: "/path/to/ttcd.sock"})`.

## Develop and test

From `sdk/typescript`:

```sh
npm install
npm test
```
