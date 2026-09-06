# ttcx for Rust

`ttcx` is the Rust SDK for TimeTravelContainer. It speaks ttcd's local
Unix-socket protocol directly and does not execute the `ttc` CLI.

## Add the crate

During local development:

```toml
[dependencies]
ttcx = { path = "sdk/rust" }
```

Start `ttcd`, then create a client:

```rust
use ttcx::Client;

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let ttc = Client::default();
    println!("{}", ttc.ping()?);

    ttc.pull("alpine:3.20")?.check()?;
    ttc.run(
        "alpine:3.20",
        &["sleep", "infinity"],
        &["-d", "--name", "demo"],
    )?.check()?;
    ttc.fork("demo@main", "experiment", false)?.check()?;
    ttc.dehydrate("demo@experiment")?.check()?;
    ttc.rehydrate("demo@experiment")?.check()?;

    let result = ttc.exec("demo@experiment", &["uname", "-a"])?;
    print!("{}", result.stdout_text());
    result.check()?;
    Ok(())
}
```

The convenience API covers run, start, named exec sessions, local and remote
batch fork, elastic migration/node management, pause/resume, dehydration,
commit, resource accounting, filesystem
diff, copy, terminal observations, checkpoint/restore, stop/restart, signal,
wait, and removal. A coordinated local fan-out looks like:

```rust
ttc.fork_many(
    "demo@main",
    &["candidate-a", "candidate-b"],
    false,
    None,
)?.check()?;
ttc.migrate("demo@candidate-a", "node-b")?.check()?;
println!("{}", ttc.nodes()?);
println!("{}", ttc.rebalance_node("node-a", Some("node-b"))?);
```

`exec_session`, `exec_sessions`, `exec_logs`, and `exec_wait` manage durable
named work. `tty_stat`, `stats`, and `diff` return structured JSON; `commit`
accepts Docker-style image changes, while `load` and `save` exchange standard
image archives. `copy` exposes branch-aware file transfer.

The byte-oriented `load`/`save` forms buffer archives in the client. Prefer
daemon-host `input`/`output` paths or `stream` for large archives.

Long-lived operations return `TtcStream` and complete frame events:

```rust
use ttcx::StreamEvent;

let watch = ttc.watch_tty("demo@candidate-a", "build1")?;
loop {
    match watch.recv()? {
        StreamEvent::Observation(json) => println!("{}", String::from_utf8_lossy(&json)),
        StreamEvent::Exited(status) => break,
        _ => {}
    }
}
```

`attach`, `watch_events`, and `follow_logs` use the same channel. Explicitly
call `close_stdin` for guest EOF. `close` produces `Error::Cancelled`; only a
daemon `Exited` event is a command status.

`Client::command` accepts every other non-interactive TTC command. Use
`command_with_options` for stdin, a timeout override, or an explicit request ID
for an intentional idempotent retry. `CommandResult` preserves raw stdout,
stderr, and exit status.

Families created by `run` are live-fork eligible by default. Pass
`--waive-live-fork` together with any deliberately incompatible external
feature (for example host networking); the resulting family is permanently
non-forkable.

`checkpoint()` and `restore()` make file paths absolute before sending them to
the daemon. Use absolute host paths in raw file-oriented commands because
`ttcd` does not share the client process's working directory.

Set `TTC_SOCKET` or `XDG_RUNTIME_DIR`, or use
`Client::new("/path/to/ttcd.sock")`. SDK clients never need `TTC_ROOT` or
`TTC_LOCAL_ROOT`.

## Test

```sh
cargo test --manifest-path sdk/rust/Cargo.toml
```

Tests use an in-process Unix-socket server and cover the protocol, terminal
observation records, and stdin flow control beyond 1 MiB.
