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
node mcp/dist/src/bin.js
```

The server uses stdio transport. Configure an MCP host with a command similar
to:

```json
{
  "mcpServers": {
    "ttc": {
      "command": "npx",
      "args": ["-y", "ttcx-mcp"],
      "env": {
        "TTC_RUNTIME_DIR": "/run/user/1000/ttc"
      }
    }
  }
}
```

The environment entry is optional when the host inherits the same
`TTC_RUNTIME_DIR` or `XDG_RUNTIME_DIR` used to start ttcd.

## Tools

The server provides tools for daemon status, listing and inspection, image
pull, run/start/exec, durable named exec sessions, local or remote batch fork,
pause/resume, dehydrate/rehydrate, commit with OCI labels, resource accounting,
filesystem diff, copy, synchronous terminal observation, checkpoint/restore,
stop/restart, signal, wait, and removal. Tool names use the `ttc_` prefix, for
example `ttc_fork_many`, `ttc_exec_logs`, `ttc_stats`, `ttc_diff`,
`ttc_tty_stat`, and `ttc_checkpoint`. `ttc_command` provides the rest of TTC's
non-interactive command surface.

`ttc_run` creates a live-fork-eligible family by default. Set
`waiveLiveFork: true` only when also requesting a deliberately incompatible
feature such as host networking; the resulting family can never be forked.

Long operations use the SDK's 30-second timeout by default. Set `timeoutMs` on
`ttc_wait` or `ttc_command` when a pull, checkpoint, remote fork, or wait may
take longer.

Tools return TTC's output as text and mark nonzero command results as MCP
errors. Interactive TTY sessions remain available through the CLI. Checkpoint
paths refer to the `ttcd` host; use absolute paths when the MCP process and
daemon have different working directories.

## Test

```sh
cd sdk
npm test --workspace=ttcx-mcp
```

The test starts the compiled stdio server, connects with the official MCP
client SDK, lists tools, and verifies that calls reach a mock ttcd Unix socket.
