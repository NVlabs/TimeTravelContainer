# ttcx MCP server

`ttcx-mcp` exposes TimeTravelContainer to MCP clients and coding agents. It
uses the TypeScript `ttcx` SDK to connect directly to a running `ttcd`; it does
not spawn the `ttc` CLI.

## Install and run

From this checkout:

```sh
cd sdk
npm install
npm run build --workspace=ttcx-mcp
node mcp/dist/src/bin.js --root /path/to/ttc-root
```

The server uses stdio transport. Configure an MCP host with a command similar
to:

```json
{
  "mcpServers": {
    "ttc": {
      "command": "npx",
      "args": ["-y", "ttcx-mcp", "--root", "/path/to/ttc-root"]
    }
  }
}
```

`--root` is required: the `TTC_ROOT` of a standalone `ttcd`, or the
`TTC_LOCAL_ROOT` of a node of a cluster. The server connects to `ttcd.sock`
there; it reads no environment variables and does not access the daemon's
storage.

## Tools

The server provides tools for daemon status, listing and inspection, image
pull, run/start/exec, durable named exec sessions, local or remote batch fork,
elastic node listing/migration/stop/rebalance, pause/resume,
quarantine/unquarantine, dehydrate/rehydrate, Docker-style image load/save and commit,
resource accounting, filesystem diff, copy, synchronous terminal observation, checkpoint/restore,
stop/restart, signal, wait, and removal. Tool names use the `ttc_` prefix, for
example `ttc_fork_many`, `ttc_exec_logs`, `ttc_stats`, `ttc_diff`,
`ttc_tty_stat`, and `ttc_checkpoint`. `ttc_command` provides the rest of TTC's
non-interactive command surface.

`ttc_inspect` accepts a container family name and returns every branch under
`TTC.branches`. `ttc_stats` accepts a full branch selector and includes
placement, immutable snapshot/root IDs, publication errors, graph/GC counts,
and private/shared resource usage.

`ttc_image_load` and `ttc_image_save` accept archive paths on the machine
running `ttcd`; the MCP transport does not copy image archives through JSON.

`ttc_run` creates a live-fork-eligible family by default. Set
`waiveLiveFork: true` only when also requesting a deliberately incompatible
feature such as host networking; the resulting family can never be forked.
The run, start, fork, rehydrate, and restore tools accept an optional `node`;
omitting it delegates placement to TTC. `ttc_stop_node` requires an explicit
`stop`, `dehydrate`, or `migrate` policy when the node still owns live work.

Long operations use the SDK's 30-second timeout by default. Set `timeoutMs` on
`ttc_wait` or `ttc_command` when a pull, checkpoint, remote fork, or wait may
take longer. Set `ttc_wait.condition` to `durable` when an orchestrator must
wait until the branch's current immutable publication is committed to shared
storage.

Tools return TTC's output as text and mark nonzero command results as MCP
errors. `ttc_list` includes each non-root branch's `parent`, allowing MCP hosts
to render fork trees without parsing human output. Interactive TTY sessions
remain available through the CLI. Checkpoint
paths refer to the `ttcd` host; use absolute paths when the MCP process and
daemon have different working directories.

## Test

```sh
cd sdk
npm test --workspace=ttcx-mcp
```

The test starts the compiled stdio server, connects with the official MCP
client SDK, lists tools, and verifies that calls reach a mock ttcd Unix socket.
