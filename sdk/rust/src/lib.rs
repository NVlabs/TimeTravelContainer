// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//! Direct Rust client for the TimeTravelContainer daemon.

use serde_json::{json, Value};
use std::env;
use std::fmt;
use std::fs::File;
use std::io::{self, Read, Write};
use std::os::unix::net::UnixStream;
use std::path::{Path, PathBuf};
use std::time::Duration;

pub const API_VERSION: u32 = 4;
const MAX_FRAME: usize = 1 << 20;
const STDIN_WINDOW: usize = 1 << 20;

/// Transport or wire-protocol failure.
#[derive(Debug)]
pub enum Error {
    Io(io::Error),
    Json(serde_json::Error),
    Protocol(String),
    InvalidArguments(String),
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Io(error) => write!(f, "{error}"),
            Self::Json(error) => write!(f, "{error}"),
            Self::Protocol(message) | Self::InvalidArguments(message) => f.write_str(message),
        }
    }
}

impl std::error::Error for Error {}

impl From<io::Error> for Error {
    fn from(error: io::Error) -> Self {
        Self::Io(error)
    }
}

impl From<serde_json::Error> for Error {
    fn from(error: serde_json::Error) -> Self {
        Self::Json(error)
    }
}

/// A TTC command that completed with a nonzero status.
#[derive(Debug)]
pub struct CommandFailed {
    pub result: CommandResult,
}

impl fmt::Display for CommandFailed {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(
            f,
            "TTC command exited with status {}: {}",
            self.result.status,
            self.result.stderr_text().trim()
        )
    }
}

impl std::error::Error for CommandFailed {}

/// The complete stdout, stderr, and status from one operation.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CommandResult {
    pub status: u32,
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
}

impl CommandResult {
    pub fn ok(&self) -> bool {
        self.status == 0
    }

    pub fn stdout_text(&self) -> String {
        String::from_utf8_lossy(&self.stdout).into_owned()
    }

    pub fn stderr_text(&self) -> String {
        String::from_utf8_lossy(&self.stderr).into_owned()
    }

    pub fn json(&self) -> Result<Value, serde_json::Error> {
        serde_json::from_slice(&self.stdout)
    }

    pub fn check(self) -> Result<Self, CommandFailed> {
        if self.ok() {
            Ok(self)
        } else {
            Err(CommandFailed { result: self })
        }
    }
}

#[derive(Clone, Debug, Default)]
pub struct CommandOptions {
    pub input: Option<Vec<u8>>,
    pub request_id: Option<String>,
    pub timeout: Option<Duration>,
}

/// Returns ttcd's default per-user socket path.
pub fn default_socket_path() -> PathBuf {
    if let Some(path) = env::var_os("TTC_RUNTIME_DIR") {
        return PathBuf::from(path).join("ttcd.sock");
    }
    if let Some(path) = env::var_os("XDG_RUNTIME_DIR") {
        return PathBuf::from(path).join("ttc").join("ttcd.sock");
    }
    let uid = unsafe { libc::geteuid() };
    env::temp_dir().join(format!("ttc-{uid}")).join("ttcd.sock")
}

#[derive(Clone, Debug)]
pub struct Client {
    socket_path: PathBuf,
    timeout: Duration,
}

impl Default for Client {
    fn default() -> Self {
        Self::new(default_socket_path())
    }
}

impl Client {
    pub fn new(socket_path: impl Into<PathBuf>) -> Self {
        Self {
            socket_path: socket_path.into(),
            timeout: Duration::from_secs(30),
        }
    }

    pub fn with_timeout(mut self, timeout: Duration) -> Self {
        self.timeout = timeout;
        self
    }

    pub fn socket_path(&self) -> &Path {
        &self.socket_path
    }

    pub fn ping(&self) -> Result<Value, Error> {
        let result = self.exchange(
            json!({"version": API_VERSION, "action": "ping"}),
            None,
            None,
        )?;
        if !result.ok() {
            return Err(Error::Protocol(format!(
                "ttcd ping failed with status {}: {}",
                result.status,
                result.stderr_text()
            )));
        }
        Ok(result.json()?)
    }

    pub fn command<I, S>(&self, args: I) -> Result<CommandResult, Error>
    where
        I: IntoIterator<Item = S>,
        S: AsRef<str>,
    {
        self.command_with_options(args, CommandOptions::default())
    }

    pub fn command_with_options<I, S>(
        &self,
        args: I,
        options: CommandOptions,
    ) -> Result<CommandResult, Error>
    where
        I: IntoIterator<Item = S>,
        S: AsRef<str>,
    {
        let args: Vec<String> = args
            .into_iter()
            .map(|arg| arg.as_ref().to_owned())
            .collect();
        if args.is_empty() {
            return Err(Error::InvalidArguments(
                "args must contain at least one string".to_owned(),
            ));
        }
        let request_id = match options.request_id {
            Some(id) => id,
            None => random_request_id()?,
        };
        let has_input = options.input.is_some();
        let request = json!({
            "version": API_VERSION,
            "action": "command",
            "request_id": request_id,
            "args": args,
            "stdin": has_input,
        });
        self.exchange(request, options.input.as_deref(), options.timeout)
    }

    pub fn list(&self) -> Result<Value, Error> {
        checked_json(self.command(["ls", "--json"])?)
    }

    pub fn inspect(&self, target: &str) -> Result<Value, Error> {
        checked_json(self.command(["inspect", target])?)
    }

    pub fn pull(&self, image: &str) -> Result<CommandResult, Error> {
        self.command(["pull", image])
    }

    pub fn run(
        &self,
        image: &str,
        command: &[&str],
        options: &[&str],
    ) -> Result<CommandResult, Error> {
        let mut args = vec!["run"];
        args.extend_from_slice(options);
        args.push(image);
        args.extend_from_slice(command);
        self.command(args)
    }

    pub fn exec(&self, target: &str, command: &[&str]) -> Result<CommandResult, Error> {
        let mut args = vec!["exec", target];
        args.extend_from_slice(command);
        self.command(args)
    }

    pub fn fork(
        &self,
        source: &str,
        destination: &str,
        paused: bool,
    ) -> Result<CommandResult, Error> {
        if paused {
            self.command(["fork", "--paused", source, destination])
        } else {
            self.command(["fork", source, destination])
        }
    }

    pub fn pause(&self, target: &str) -> Result<CommandResult, Error> {
        self.command(["pause", target])
    }

    pub fn resume(&self, target: &str) -> Result<CommandResult, Error> {
        self.command(["resume", target])
    }

    pub fn dehydrate(&self, target: &str) -> Result<CommandResult, Error> {
        self.command(["dehydrate", target])
    }

    pub fn rehydrate(&self, target: &str) -> Result<CommandResult, Error> {
        self.command(["rehydrate", target])
    }

    pub fn stop(&self, target: &str) -> Result<CommandResult, Error> {
        self.command(["stop", target])
    }

    pub fn remove(&self, target: &str, force: bool) -> Result<CommandResult, Error> {
        if force {
            self.command(["rm", "--force", target])
        } else {
            self.command(["rm", target])
        }
    }

    fn exchange(
        &self,
        request: Value,
        input: Option<&[u8]>,
        timeout: Option<Duration>,
    ) -> Result<CommandResult, Error> {
        let mut stream = UnixStream::connect(&self.socket_path)?;
        let timeout = timeout.unwrap_or(self.timeout);
        stream.set_read_timeout(Some(timeout))?;
        stream.set_write_timeout(Some(timeout))?;
        serde_json::to_writer(&mut stream, &request)?;
        stream.write_all(b"\n")?;

        let mut stdout = Vec::new();
        let mut stderr = Vec::new();
        let mut input_offset = 0usize;
        let mut unacknowledged = 0usize;
        let mut eof_sent = input.is_none();

        loop {
            if let Some(input) = input {
                while input_offset < input.len() && unacknowledged < STDIN_WINDOW {
                    let size = MAX_FRAME
                        .min(STDIN_WINDOW - unacknowledged)
                        .min(input.len() - input_offset);
                    write_frame(&mut stream, b'I', &input[input_offset..input_offset + size])?;
                    input_offset += size;
                    unacknowledged += size;
                }
                if input_offset == input.len() && !eof_sent {
                    write_frame(&mut stream, b'C', &[])?;
                    eof_sent = true;
                }
            }

            let (kind, payload) = read_frame(&mut stream)?;
            match kind {
                b'O' => stdout.extend_from_slice(&payload),
                b'E' => stderr.extend_from_slice(&payload),
                b'A' => {
                    if payload.len() != 4 {
                        return Err(Error::Protocol("invalid stdin acknowledgement".to_owned()));
                    }
                    let acknowledged =
                        u32::from_be_bytes(payload.try_into().expect("length checked")) as usize;
                    if acknowledged > unacknowledged {
                        return Err(Error::Protocol("ttcd acknowledged unsent stdin".to_owned()));
                    }
                    unacknowledged -= acknowledged;
                }
                b'T' => {}
                b'X' => {
                    if payload.len() != 4 {
                        return Err(Error::Protocol("invalid status frame".to_owned()));
                    }
                    return Ok(CommandResult {
                        status: u32::from_be_bytes(payload.try_into().expect("length checked")),
                        stdout,
                        stderr,
                    });
                }
                _ => {
                    return Err(Error::Protocol(format!(
                        "unexpected ttcd frame type 0x{kind:02x}"
                    )))
                }
            }
        }
    }
}
fn checked_json(result: CommandResult) -> Result<Value, Error> {
    if !result.ok() {
        return Err(Error::Protocol(format!(
            "TTC command exited with status {}: {}",
            result.status,
            result.stderr_text()
        )));
    }
    Ok(result.json()?)
}

fn random_request_id() -> Result<String, Error> {
    let mut bytes = [0u8; 16];
    File::open("/dev/urandom")?.read_exact(&mut bytes)?;
    let mut result = String::with_capacity(32);
    for byte in bytes {
        use fmt::Write as _;
        write!(result, "{byte:02x}").expect("writing to String cannot fail");
    }
    Ok(result)
}

fn write_frame(stream: &mut UnixStream, kind: u8, payload: &[u8]) -> Result<(), Error> {
    if payload.len() > MAX_FRAME {
        return Err(Error::Protocol("outgoing frame is too large".to_owned()));
    }
    stream.write_all(&[kind])?;
    stream.write_all(&(payload.len() as u32).to_be_bytes())?;
    stream.write_all(payload)?;
    Ok(())
}

fn read_frame(stream: &mut UnixStream) -> Result<(u8, Vec<u8>), Error> {
    let mut header = [0u8; 5];
    stream.read_exact(&mut header).map_err(|error| {
        if error.kind() == io::ErrorKind::UnexpectedEof {
            Error::Protocol("ttcd closed the connection before returning status".to_owned())
        } else {
            Error::Io(error)
        }
    })?;
    let length = u32::from_be_bytes(header[1..].try_into().expect("four bytes")) as usize;
    if length > MAX_FRAME {
        return Err(Error::Protocol(format!(
            "oversized ttcd frame: {length} bytes"
        )));
    }
    let mut payload = vec![0; length];
    stream.read_exact(&mut payload)?;
    Ok((header[0], payload))
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::fs;
    use std::os::unix::net::UnixListener;
    use std::thread;

    fn temporary_socket(name: &str) -> PathBuf {
        let path = env::temp_dir().join(format!(
            "ttcx-rust-{}-{}-{name}",
            std::process::id(),
            random_request_id().unwrap()
        ));
        fs::create_dir(&path).unwrap();
        path.join("ttcd.sock")
    }

    fn mock_server<F>(path: &Path, handler: F) -> thread::JoinHandle<()>
    where
        F: FnOnce(UnixStream, Value) + Send + 'static,
    {
        let listener = UnixListener::bind(path).unwrap();
        thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut header = Vec::new();
            while !header.ends_with(b"\n") {
                let mut byte = [0u8; 1];
                stream.read_exact(&mut byte).unwrap();
                header.push(byte[0]);
            }
            handler(stream, serde_json::from_slice(&header).unwrap());
        })
    }

    #[test]
    fn ping_uses_direct_protocol() {
        let path = temporary_socket("ping");
        let server = mock_server(&path, |mut stream, request| {
            assert_eq!(request, json!({"version": API_VERSION, "action": "ping"}));
            write_frame(&mut stream, b'O', br#"{"version":4}"#).unwrap();
            write_frame(&mut stream, b'X', &0u32.to_be_bytes()).unwrap();
        });
        assert_eq!(Client::new(&path).ping().unwrap()["version"], API_VERSION);
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn command_streams_flow_controlled_stdin_and_preserves_status() {
        let path = temporary_socket("stdin");
        let expected = vec![b'x'; STDIN_WINDOW + 29];
        let server_expected = expected.clone();
        let server = mock_server(&path, move |mut stream, request| {
            assert_eq!(request["args"], json!(["exec", "demo", "cat"]));
            assert_eq!(request["stdin"], true);
            assert_eq!(request["request_id"].as_str().unwrap().len(), 32);
            let mut received = Vec::new();
            loop {
                let (kind, payload) = read_frame(&mut stream).unwrap();
                if kind == b'C' {
                    break;
                }
                assert_eq!(kind, b'I');
                received.extend_from_slice(&payload);
                write_frame(&mut stream, b'A', &(payload.len() as u32).to_be_bytes()).unwrap();
            }
            assert_eq!(received, server_expected);
            write_frame(&mut stream, b'E', b"failed\n").unwrap();
            write_frame(&mut stream, b'X', &11u32.to_be_bytes()).unwrap();
        });
        let result = Client::new(&path)
            .command_with_options(
                ["exec", "demo", "cat"],
                CommandOptions {
                    input: Some(expected),
                    ..Default::default()
                },
            )
            .unwrap();
        assert_eq!(result.status, 11);
        assert_eq!(result.stderr_text(), "failed\n");
        assert_eq!(result.check().unwrap_err().result.status, 11);
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }
}
