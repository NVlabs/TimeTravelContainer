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

The server provides `ttc_status`, `ttc_list`, `ttc_inspect`, `ttc_pull`,
`ttc_run`, `ttc_exec`, `ttc_fork`, `ttc_pause`, `ttc_resume`, `ttc_stop`, and
`ttc_remove`. `ttc_command` provides the rest of TTC's non-interactive command
surface.

Tools return TTC's output as text and mark nonzero command results as MCP
errors. Interactive TTY sessions remain available through the CLI.

## Test

```sh
cd sdk
npm test --workspace=ttcx-mcp
```

The test starts the compiled stdio server, connects with the official MCP
client SDK, lists tools, and verifies that calls reach a mock ttcd Unix socket.
