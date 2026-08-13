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

The convenience API covers run, start, exec, local and remote batch fork,
pause/resume, dehydration, checkpoint/restore, stop/restart, signal, wait, and
removal. A coordinated remote fan-out looks like:

```rust
ttc.fork_many(
    "demo@main",
    &["remote@candidate-a", "remote@candidate-b"],
    false,
    Some("node-b"),
)?.check()?;
```

`Client::command` accepts every other non-interactive TTC command. Use
`command_with_options` for stdin, a timeout override, or an explicit request ID
for an intentional idempotent retry. `CommandResult` preserves raw stdout,
stderr, and exit status.

`checkpoint()` and `restore()` make file paths absolute before sending them to
the daemon. Use absolute host paths in raw file-oriented commands because
`ttcd` does not share the client process's working directory.

Set `TTC_RUNTIME_DIR` or `XDG_RUNTIME_DIR`, or use
`Client::new("/path/to/ttcd.sock")`.

## Test

```sh
cargo test --manifest-path sdk/rust/Cargo.toml
```

Tests use an in-process Unix-socket server and cover the protocol and stdin
flow control beyond 1 MiB.
