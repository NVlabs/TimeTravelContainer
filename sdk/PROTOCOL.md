# ttcd local API

The TTC SDKs use ttcd's versioned Unix-socket protocol directly. This document
records the shared contract implemented by the Python, TypeScript, Rust, and
MCP packages in this directory.

## Connection

The default socket is selected in this order:

1. `$TTC_RUNTIME_DIR/ttcd.sock`
2. `$XDG_RUNTIME_DIR/ttc/ttcd.sock`
3. `/tmp/ttc-$EUID/ttcd.sock`

ttcd accepts clients owned by the same Unix user. Each operation uses a new
connection.

## Request

The client writes one newline-terminated JSON object:

```json
{
  "version": 4,
  "action": "command",
  "request_id": "optional-32-character-hex-id",
  "args": ["ls", "--json"],
  "stdin": false
}
```

`action` is `ping` or `command` for public SDK operations. SDK clients omit
ttc's private runtime-path options; ttcd supplies its own paths and data root.
`request_id` makes supported mutating commands idempotent when a caller
deliberately retries the same request.

## Frames

After the JSON header, both directions use:

```
1-byte type | 4-byte big-endian payload length | payload
```

Payloads are at most 1 MiB. Server frame types are `O` (stdout), `E` (stderr),
`X` (four-byte exit status), `A` (four-byte stdin acknowledgement), and `T`
(terminal state). Client frame types are `I` (stdin bytes), `C` (stdin EOF),
and `W` (terminal resize).

The bindings support non-interactive input and implement ttcd's 1 MiB stdin
flow-control window. Interactive terminal ownership remains the responsibility
of the `ttc` CLI.
