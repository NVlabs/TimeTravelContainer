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
    ttc.fork("demo", "demo@experiment", false)?.check()?;

    let result = ttc.exec("demo@experiment", &["uname", "-a"])?;
    print!("{}", result.stdout_text());
    result.check()?;
    Ok(())
}
```

`Client::command` accepts every non-interactive TTC command. Use
`command_with_options` for stdin, a timeout override, or an explicit request ID
for an intentional idempotent retry. `CommandResult` preserves raw stdout,
stderr, and exit status.

Set `TTC_RUNTIME_DIR` or `XDG_RUNTIME_DIR`, or use
`Client::new("/path/to/ttcd.sock")`.

## Test

```sh
cargo test --manifest-path sdk/rust/Cargo.toml
```

Tests use an in-process Unix-socket server and cover the protocol and stdin
flow control beyond 1 MiB.
