# ttcd local API

The TTC SDKs use ttcd's versioned Unix-socket protocol directly. This document
records the shared contract implemented by the Python, TypeScript, Rust, and
MCP packages in this directory.

## Connection

The default socket is selected in this order:

1. `$TTC_SOCKET`
2. `$XDG_RUNTIME_DIR/ttc/ttcd.sock`
3. `/tmp/ttc-$EUID/ttcd.sock`

ttcd accepts clients owned by the same Unix user. Each operation uses a new
connection.

## Request

The client writes one newline-terminated JSON object:

```json
{
  "version": 5,
  "action": "command",
  "request_id": "optional-32-character-hex-id",
  "args": ["ls", "--json"],
  "stdin": false
}
```

`action` is `ping` or `command` for public SDK operations. Command arguments
are exactly the arguments that would follow the `ttc` executable. SDK clients
omit TTC's private runtime-path and daemon-selection options; `ttcd` supplies
its own paths, data root, isolation mode, and runtime platform.
`request_id` makes supported mutating commands idempotent when a caller
deliberately retries the same request.

Existing branches use full `CONTAINER@BRANCH` selectors. A local fork
destination is a branch name; a remote fork destination is a
`CONTAINER@BRANCH` selector on the target node. The protocol does not add
implicit `main` selection beyond what the CLI command itself documents.

`ttc ls --json` reports branch ancestry as `parent`. Root branches omit it.

Host file paths are interpreted by the daemon. SDK convenience methods for
checkpoint and restore make paths absolute; callers using raw `command`
requests should do the same for checkpoint, restore, copy, process JSON, env
files, and other host-path arguments.

## Frames

After the JSON header, both directions use:

```
1-byte type | 4-byte big-endian payload length | payload
```

Payloads are at most 1 MiB. Server frame types are `O` (stdout), `E` (stderr),
`X` (four-byte exit status), `A` (four-byte stdin acknowledgement), `T`
(terminal mode), and `S` (one complete JSON terminal-observation object).
Buffered SDK requests expose consecutive `S` objects as newline-delimited
stdout records. Client frame types are `I` (stdin bytes), `C` (stdin EOF), and
`W` (terminal resize).

A `W` payload is eight bytes: a big-endian `uint32` row count followed by a
big-endian `uint32` column count. `attach` does not begin streaming until the
client reports a size, and fails with `timed out waiting for attached client
terminal size` otherwise. A non-interactive caller must therefore send one `W`
frame immediately after the header rather than waiting for a real resize.

The bindings support non-interactive input and implement ttcd's 1 MiB stdin
flow-control window. Interactive terminal ownership remains the responsibility
of the `ttc` CLI.

Bindings additionally expose a streaming form for operations that outlive one
request/response exchange: `tty watch`, `events --follow`, `logs --follow`, and
`attach`. A stream delivers frames through callbacks as they arrive and keeps
stdin writable for the life of the operation. It must never send `C` implicitly,
because stdin EOF terminates an interactive shell; closing stdin is always an
explicit caller action.

Client frames must follow the JSON header even when the caller writes or
resizes immediately after constructing a stream. A local stream cancellation
is not a successful command status: only an `X` frame supplies status, so
bindings report cancellation separately. Writes after stdin EOF is requested
are rejected. Callback-based bindings convert callback failures into stream
failures rather than letting them escape a socket event loop.

Every request ends with exactly one `X` frame unless the connection or daemon
fails. A nonzero command status is an ordinary command result, not a wire error;
bindings expose it in `Result`/`CommandResult` and provide an explicit `check`
helper. Connection loss before `X` has an indeterminate outcome for mutations.
Retry a mutation only with the same `request_id`; using a new ID requests a new
operation.

The SDKs default to a 30-second client timeout. Long pulls, waits,
checkpoints, remote forks, and dehydration may need an operation-specific or
client-wide override. A client timeout does not cancel an operation already
accepted by `ttcd`; use daemon request inspection when its outcome is unclear.
