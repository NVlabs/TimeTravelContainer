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
use std::sync::{mpsc, Arc, Condvar, Mutex};
use std::time::Duration;

pub const API_VERSION: u32 = 7;
const MAX_FRAME: usize = 1 << 20;
const STDIN_WINDOW: usize = 1 << 20;

/// Transport or wire-protocol failure.
#[derive(Debug)]
pub enum Error {
    Io(io::Error),
    Json(serde_json::Error),
    Protocol(String),
    InvalidArguments(String),
    Cancelled,
}

impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Io(error) => write!(f, "{error}"),
            Self::Json(error) => write!(f, "{error}"),
            Self::Protocol(message) | Self::InvalidArguments(message) => f.write_str(message),
            Self::Cancelled => f.write_str("TTC stream was cancelled before returning status"),
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

#[derive(Clone, Debug, Default)]
pub struct StreamOptions {
    pub stdin: bool,
    pub request_id: Option<String>,
    pub timeout: Option<Duration>,
    /// Initial (rows, columns), sent immediately after the request header.
    pub size: Option<(u32, u32)>,
}

#[derive(Clone, Debug)]
pub struct CommitImageOptions {
    pub author: Option<String>,
    pub message: Option<String>,
    pub changes: Vec<String>,
    pub pause: bool,
}

impl Default for CommitImageOptions {
    fn default() -> Self {
        Self {
            author: None,
            message: None,
            changes: Vec::new(),
            pause: true,
        }
    }
}

#[derive(Clone, Debug, Default)]
pub struct ImageLoadOptions {
    pub input: Option<PathBuf>,
    pub archive: Option<Vec<u8>>,
    pub platform: Option<String>,
    pub quiet: bool,
}

#[derive(Clone, Debug, Default)]
pub struct ImageSaveOptions {
    pub output: Option<PathBuf>,
    pub platform: Option<String>,
}

/// One complete frame delivered by a long-lived TTC operation.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum StreamEvent {
    Stdout(Vec<u8>),
    Stderr(Vec<u8>),
    Observation(Vec<u8>),
    Terminal(Vec<u8>),
    Exited(u32),
}

#[derive(Debug, Default)]
struct StreamState {
    unacknowledged: usize,
    settled: bool,
    eof_requested: bool,
    eof_sent: bool,
}

#[derive(Debug)]
struct StreamShared {
    state: Mutex<StreamState>,
    changed: Condvar,
    input: Mutex<()>,
    writer: Mutex<UnixStream>,
    shutdown: UnixStream,
}

/// A streaming TTC operation. Output arrives through `recv`; stdin and resize
/// remain writable until the daemon returns an `Exited` event.
pub struct TtcStream {
    pub args: Vec<String>,
    shared: Arc<StreamShared>,
    events: mpsc::Receiver<Result<StreamEvent, Error>>,
    event_sender: mpsc::Sender<Result<StreamEvent, Error>>,
    stdin_declared: bool,
}

impl TtcStream {
    pub fn write(&self, data: &[u8]) -> Result<(), Error> {
        if !self.stdin_declared {
            return Err(Error::InvalidArguments(
                "stream was not opened with stdin enabled".to_owned(),
            ));
        }
        let _input = self.shared.input.lock().unwrap();
        {
            let state = self.shared.state.lock().unwrap();
            if state.eof_requested || state.eof_sent {
                return Err(Error::InvalidArguments("stdin already closed".to_owned()));
            }
            ensure_stream_active(&state)?;
        }
        let mut offset = 0;
        while offset < data.len() {
            let mut state = self.shared.state.lock().unwrap();
            while !state.settled && state.unacknowledged >= STDIN_WINDOW {
                state = self.shared.changed.wait(state).unwrap();
            }
            ensure_stream_active(&state)?;
            let size = MAX_FRAME
                .min(STDIN_WINDOW - state.unacknowledged)
                .min(data.len() - offset);
            state.unacknowledged += size;
            drop(state);
            let write_result = {
                let mut writer = self.shared.writer.lock().unwrap();
                write_frame(&mut writer, b'I', &data[offset..offset + size])
            };
            if let Err(error) = write_result {
                let message = error.to_string();
                settle_stream(
                    &self.shared,
                    &self.event_sender,
                    Err(Error::Protocol(message)),
                );
                return Err(error);
            }
            offset += size;
        }
        Ok(())
    }

    pub fn close_stdin(&self) -> Result<(), Error> {
        if !self.stdin_declared {
            return Err(Error::InvalidArguments(
                "stream was not opened with stdin enabled".to_owned(),
            ));
        }
        let _input = self.shared.input.lock().unwrap();
        {
            let mut state = self.shared.state.lock().unwrap();
            if state.eof_requested || state.eof_sent {
                return Ok(());
            }
            ensure_stream_active(&state)?;
            state.eof_requested = true;
        }
        let write_result = {
            let mut writer = self.shared.writer.lock().unwrap();
            write_frame(&mut writer, b'C', &[])
        };
        if let Err(error) = write_result {
            let message = error.to_string();
            settle_stream(
                &self.shared,
                &self.event_sender,
                Err(Error::Protocol(message)),
            );
            return Err(error);
        }
        self.shared.state.lock().unwrap().eof_sent = true;
        Ok(())
    }

    pub fn resize(&self, rows: u32, cols: u32) -> Result<(), Error> {
        let mut payload = [0u8; 8];
        payload[..4].copy_from_slice(&rows.to_be_bytes());
        payload[4..].copy_from_slice(&cols.to_be_bytes());
        {
            let state = self.shared.state.lock().unwrap();
            ensure_stream_active(&state)?;
        }
        let write_result = {
            let mut writer = self.shared.writer.lock().unwrap();
            write_frame(&mut writer, b'W', &payload)
        };
        if let Err(error) = write_result {
            let message = error.to_string();
            settle_stream(
                &self.shared,
                &self.event_sender,
                Err(Error::Protocol(message)),
            );
            return Err(error);
        }
        Ok(())
    }

    pub fn recv(&self) -> Result<StreamEvent, Error> {
        self.events.recv().map_err(|_| {
            Error::Protocol("TTC stream event channel closed before status".to_owned())
        })?
    }

    pub fn recv_timeout(&self, timeout: Duration) -> Result<StreamEvent, Error> {
        self.events
            .recv_timeout(timeout)
            .map_err(|error| Error::Protocol(format!("waiting for TTC stream event: {error}")))?
    }

    pub fn wait(&self) -> Result<u32, Error> {
        loop {
            if let StreamEvent::Exited(status) = self.recv()? {
                return Ok(status);
            }
        }
    }

    pub fn close(&self) {
        settle_stream(&self.shared, &self.event_sender, Err(Error::Cancelled));
    }
}

impl Drop for TtcStream {
    fn drop(&mut self) {
        self.close();
    }
}

fn ensure_stream_active(state: &StreamState) -> Result<(), Error> {
    if state.settled {
        Err(Error::Protocol("TTC stream is already settled".to_owned()))
    } else {
        Ok(())
    }
}

fn settle_stream(
    shared: &Arc<StreamShared>,
    sender: &mpsc::Sender<Result<StreamEvent, Error>>,
    event: Result<StreamEvent, Error>,
) -> bool {
    let mut state = shared.state.lock().unwrap();
    if state.settled {
        return false;
    }
    state.settled = true;
    shared.changed.notify_all();
    drop(state);
    let _ = shared.shutdown.shutdown(std::net::Shutdown::Both);
    let _ = sender.send(event);
    true
}

/// Returns ttcd's default per-user socket path.
pub fn default_socket_path() -> PathBuf {
    if let Some(path) = env::var_os("TTC_SOCKET") {
        return PathBuf::from(path);
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

    /// Inspects a container family; branch details are included in TTC.branches.
    pub fn inspect(&self, container: &str) -> Result<Value, Error> {
        checked_json(self.command(["inspect", container])?)
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
        self.run_on(image, command, options, None)
    }

    pub fn run_on(
        &self,
        image: &str,
        command: &[&str],
        options: &[&str],
        node: Option<&str>,
    ) -> Result<CommandResult, Error> {
        let mut args = vec!["run".to_owned()];
        if let Some(node) = node {
            args.extend(["--node".to_owned(), node.to_owned()]);
        }
        args.extend(options.iter().map(|value| (*value).to_owned()));
        args.push(image.to_owned());
        args.extend(command.iter().map(|value| (*value).to_owned()));
        self.command(args)
    }

    pub fn start(&self, target: &str) -> Result<CommandResult, Error> {
        self.command(["start", target])
    }

    pub fn start_on(&self, target: &str, node: &str) -> Result<CommandResult, Error> {
        self.command(["start", "--node", node, target])
    }

    pub fn exec(&self, target: &str, command: &[&str]) -> Result<CommandResult, Error> {
        let mut args = vec!["exec", target];
        args.extend_from_slice(command);
        self.command(args)
    }

    pub fn exec_session(
        &self,
        target: &str,
        command: &[&str],
        session: &str,
        tty: bool,
        interactive: bool,
    ) -> Result<CommandResult, Error> {
        let mut args = vec![
            "exec".to_owned(),
            "--session".to_owned(),
            session.to_owned(),
            "--detach".to_owned(),
        ];
        if tty {
            args.push("--tty".to_owned());
        }
        if interactive {
            args.push("--interactive".to_owned());
        }
        args.push(target.to_owned());
        args.extend(command.iter().map(|value| (*value).to_owned()));
        self.command(args)
    }

    pub fn exec_sessions(&self, target: &str) -> Result<serde_json::Value, Error> {
        Ok(self.command(["exec", "ls", "--json", target])?.json()?)
    }

    pub fn exec_logs(&self, target: &str, session: &str) -> Result<CommandResult, Error> {
        self.command(["logs", "--exec", session, target])
    }

    pub fn exec_wait(&self, target: &str, session: &str) -> Result<CommandResult, Error> {
        self.command(vec![
            "exec".to_owned(),
            "wait".to_owned(),
            format!("{target}:{session}"),
        ])
    }

    pub fn tty_stat(&self, target: &str, session: &str) -> Result<serde_json::Value, Error> {
        Ok(self
            .command(["tty", "stat", target, "--session", session, "--json"])?
            .json()?)
    }

    pub fn commit(
        &self,
        target: &str,
        image: &str,
        options: CommitImageOptions,
    ) -> Result<CommandResult, Error> {
        let mut args = vec!["commit".to_owned()];
        if let Some(author) = options.author {
            args.extend(["--author".to_owned(), author]);
        }
        if let Some(message) = options.message {
            args.extend(["--message".to_owned(), message]);
        }
        for change in options.changes {
            args.extend(["--change".to_owned(), change]);
        }
        if !options.pause {
            args.push("--pause=false".to_owned());
        }
        args.push(target.to_owned());
        args.push(image.to_owned());
        self.command(args)
    }

    pub fn load(&self, options: ImageLoadOptions) -> Result<CommandResult, Error> {
        if options.input.is_some() && options.archive.is_some() {
            return Err(Error::InvalidArguments(
                "image load accepts either input or archive, not both".to_owned(),
            ));
        }
        if options.input.is_none() && options.archive.is_none() {
            return Err(Error::InvalidArguments(
                "image load requires input or archive".to_owned(),
            ));
        }
        let mut args = vec!["load".to_owned()];
        if let Some(input) = options.input {
            let input = absolute_path(&input)?;
            args.extend(["--input".to_owned(), input.to_string_lossy().into_owned()]);
        }
        if let Some(platform) = options.platform {
            args.extend(["--platform".to_owned(), platform]);
        }
        if options.quiet {
            args.push("--quiet".to_owned());
        }
        self.command_with_options(
            args,
            CommandOptions {
                input: options.archive,
                ..Default::default()
            },
        )
    }

    pub fn save(&self, images: &[&str], options: ImageSaveOptions) -> Result<CommandResult, Error> {
        if images.is_empty() {
            return Err(Error::InvalidArguments(
                "image save requires at least one image".to_owned(),
            ));
        }
        let mut args = vec!["save".to_owned()];
        if let Some(output) = options.output {
            let output = absolute_path(&output)?;
            args.extend(["--output".to_owned(), output.to_string_lossy().into_owned()]);
        }
        if let Some(platform) = options.platform {
            args.extend(["--platform".to_owned(), platform]);
        }
        args.extend(images.iter().map(|image| (*image).to_owned()));
        self.command(args)
    }

    pub fn stats(&self, target: &str) -> Result<serde_json::Value, Error> {
        Ok(self.command(["stats", "--json", target])?.json()?)
    }

    pub fn diff(&self, target: &str) -> Result<serde_json::Value, Error> {
        Ok(self.command(["diff", "--json", target])?.json()?)
    }

    pub fn copy(&self, source: &str, destination: &str) -> Result<CommandResult, Error> {
        self.command(["cp", source, destination])
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

    pub fn fork_many(
        &self,
        source: &str,
        destinations: &[&str],
        paused: bool,
        node: Option<&str>,
    ) -> Result<CommandResult, Error> {
        if destinations.is_empty() {
            return Err(Error::InvalidArguments(
                "destinations must not be empty".to_owned(),
            ));
        }
        let mut args = vec!["fork".to_owned()];
        if paused {
            args.push("--paused".to_owned());
        }
        if let Some(node) = node {
            args.push("--node".to_owned());
            args.push(node.to_owned());
        }
        args.push(source.to_owned());
        args.extend(
            destinations
                .iter()
                .map(|destination| (*destination).to_owned()),
        );
        self.command(args)
    }

    /// Moves a branch to another elastic-cluster node without changing its identity.
    pub fn migrate(&self, target: &str, node: &str) -> Result<CommandResult, Error> {
        self.command(["migrate", "--node", node, target])
    }

    /// Returns the current dynamic cluster membership and memory accounting.
    pub fn nodes(&self) -> Result<serde_json::Value, Error> {
        let result = self.command(["node", "ls", "--json"])?;
        checked_json(result)
    }

    pub fn stop_node(&self, node: &str, policy: Option<&str>) -> Result<CommandResult, Error> {
        let mut args = vec!["node".to_owned(), "stop".to_owned()];
        if let Some(policy) = policy {
            if policy != "stop" && policy != "dehydrate" && policy != "migrate" {
                return Err(Error::InvalidArguments(
                    "node stop policy must be stop, dehydrate, or migrate".to_owned(),
                ));
            }
            args.push(format!("--{policy}-all"));
        }
        args.push(node.to_owned());
        self.command(args)
    }

    pub fn rebalance_node(
        &self,
        node: &str,
        destination_node: Option<&str>,
    ) -> Result<serde_json::Value, Error> {
        let mut args = vec!["node".to_owned(), "rebalance".to_owned()];
        if let Some(destination_node) = destination_node {
            args.extend(["--node".to_owned(), destination_node.to_owned()]);
        }
        args.push(node.to_owned());
        checked_json(self.command(args)?)
    }

    pub fn pause(&self, target: &str) -> Result<CommandResult, Error> {
        self.command(["pause", target])
    }

    pub fn resume(&self, target: &str) -> Result<CommandResult, Error> {
        self.command(["resume", target])
    }

    pub fn quarantine(&self, target: &str) -> Result<CommandResult, Error> {
        self.command(["quarantine", target])
    }

    pub fn unquarantine(&self, target: &str) -> Result<CommandResult, Error> {
        self.command(["unquarantine", target])
    }

    pub fn dehydrate(&self, target: &str) -> Result<CommandResult, Error> {
        self.command(["dehydrate", target])
    }

    pub fn rehydrate(&self, target: &str) -> Result<CommandResult, Error> {
        self.command(["rehydrate", target])
    }

    pub fn rehydrate_on(&self, target: &str, node: &str) -> Result<CommandResult, Error> {
        self.command(["rehydrate", "--node", node, target])
    }

    pub fn checkpoint(
        &self,
        target: &str,
        file: impl AsRef<Path>,
        compression: &str,
    ) -> Result<CommandResult, Error> {
        let file = absolute_path(file.as_ref())?;
        self.command([
            "checkpoint".to_owned(),
            "--compression".to_owned(),
            compression.to_owned(),
            target.to_owned(),
            file.to_string_lossy().into_owned(),
        ])
    }

    pub fn restore(
        &self,
        file: impl AsRef<Path>,
        name: Option<&str>,
    ) -> Result<CommandResult, Error> {
        let file = absolute_path(file.as_ref())?;
        let mut args = vec!["restore".to_owned()];
        if let Some(name) = name {
            args.extend(["--name".to_owned(), name.to_owned()]);
        }
        args.push(file.to_string_lossy().into_owned());
        self.command(args)
    }

    pub fn restore_on(
        &self,
        file: impl AsRef<Path>,
        name: Option<&str>,
        node: &str,
    ) -> Result<CommandResult, Error> {
        let file = absolute_path(file.as_ref())?;
        let mut args = vec!["restore".to_owned(), "--node".to_owned(), node.to_owned()];
        if let Some(name) = name {
            args.extend(["--name".to_owned(), name.to_owned()]);
        }
        args.push(file.to_string_lossy().into_owned());
        self.command(args)
    }

    pub fn stop(&self, target: &str) -> Result<CommandResult, Error> {
        self.timed_lifecycle_command("stop", target, None)
    }

    pub fn stop_with_timeout(
        &self,
        target: &str,
        timeout_seconds: i64,
    ) -> Result<CommandResult, Error> {
        self.timed_lifecycle_command("stop", target, Some(timeout_seconds))
    }

    pub fn restart(
        &self,
        target: &str,
        timeout_seconds: Option<i64>,
    ) -> Result<CommandResult, Error> {
        self.timed_lifecycle_command("restart", target, timeout_seconds)
    }

    pub fn kill(&self, target: &str, signal: Option<&str>) -> Result<CommandResult, Error> {
        let mut args = vec!["kill".to_owned()];
        if let Some(signal) = signal {
            args.extend(["--signal".to_owned(), signal.to_owned()]);
        }
        args.push(target.to_owned());
        self.command(args)
    }

    pub fn wait(&self, target: &str) -> Result<CommandResult, Error> {
        self.wait_for(target, "not-running")
    }

    pub fn wait_for(&self, target: &str, condition: &str) -> Result<CommandResult, Error> {
        if condition != "not-running" && condition != "durable" {
            return Err(Error::InvalidArguments(
                "condition must be 'not-running' or 'durable'".to_owned(),
            ));
        }
        self.command(["wait", "--condition", condition, target])
    }

    pub fn remove(&self, target: &str, force: bool) -> Result<CommandResult, Error> {
        if force {
            self.command(["rm", "--force", target])
        } else {
            self.command(["rm", target])
        }
    }

    pub fn stream<I, S>(&self, args: I, options: StreamOptions) -> Result<TtcStream, Error>
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
        let stream_args = args.clone();
        let request = json!({
            "version": API_VERSION,
            "action": "command",
            "request_id": request_id,
            "args": args,
            "stdin": options.stdin,
        });
        let mut writer = UnixStream::connect(&self.socket_path)?;
        writer.set_read_timeout(options.timeout)?;
        writer.set_write_timeout(options.timeout)?;
        serde_json::to_writer(&mut writer, &request)?;
        writer.write_all(b"\n")?;
        if let Some((rows, cols)) = options.size {
            let mut payload = [0u8; 8];
            payload[..4].copy_from_slice(&rows.to_be_bytes());
            payload[4..].copy_from_slice(&cols.to_be_bytes());
            write_frame(&mut writer, b'W', &payload)?;
        }
        let reader = writer.try_clone()?;
        let shutdown = writer.try_clone()?;
        let shared = Arc::new(StreamShared {
            state: Mutex::new(StreamState::default()),
            changed: Condvar::new(),
            input: Mutex::new(()),
            writer: Mutex::new(writer),
            shutdown,
        });
        let (sender, events) = mpsc::channel();
        let reader_shared = Arc::clone(&shared);
        let reader_sender = sender.clone();
        std::thread::spawn(move || stream_read_loop(reader, reader_shared, reader_sender));
        Ok(TtcStream {
            args: stream_args,
            shared,
            events,
            event_sender: sender,
            stdin_declared: options.stdin,
        })
    }

    pub fn watch_tty(&self, target: &str, session: &str) -> Result<TtcStream, Error> {
        self.stream(
            ["tty", "watch", target, "--session", session],
            StreamOptions::default(),
        )
    }

    pub fn attach(
        &self,
        target: &str,
        exec_session: Option<&str>,
        stdin: bool,
        takeover: bool,
        size: (u32, u32),
    ) -> Result<TtcStream, Error> {
        let mut args = vec!["attach".to_owned()];
        args.push(if stdin { "--interactive" } else { "--no-stdin" }.to_owned());
        if takeover {
            args.push("--takeover".to_owned());
        }
        if let Some(session) = exec_session {
            args.extend(["--exec".to_owned(), session.to_owned()]);
        }
        args.push(target.to_owned());
        self.stream(
            args,
            StreamOptions {
                stdin,
                size: Some(size),
                ..Default::default()
            },
        )
    }

    pub fn watch_events(
        &self,
        since: Option<&str>,
        filters: &[(&str, &str)],
    ) -> Result<TtcStream, Error> {
        let mut args = vec![
            "events".to_owned(),
            "--json".to_owned(),
            "--follow".to_owned(),
        ];
        if let Some(since) = since {
            args.extend(["--since".to_owned(), since.to_owned()]);
        }
        for (key, value) in filters {
            args.extend(["--filter".to_owned(), format!("{key}={value}")]);
        }
        self.stream(args, StreamOptions::default())
    }

    pub fn follow_logs(
        &self,
        target: &str,
        exec_session: Option<&str>,
        tail: Option<&str>,
    ) -> Result<TtcStream, Error> {
        let mut args = vec!["logs".to_owned(), "--follow".to_owned()];
        if let Some(tail) = tail {
            args.extend(["-n".to_owned(), tail.to_owned()]);
        }
        if let Some(session) = exec_session {
            args.extend(["--exec".to_owned(), session.to_owned()]);
        }
        args.push(target.to_owned());
        self.stream(args, StreamOptions::default())
    }

    fn timed_lifecycle_command(
        &self,
        command: &str,
        target: &str,
        timeout_seconds: Option<i64>,
    ) -> Result<CommandResult, Error> {
        let mut args = vec![command.to_owned()];
        if let Some(timeout) = timeout_seconds {
            args.extend(["--time".to_owned(), timeout.to_string()]);
        }
        args.push(target.to_owned());
        self.command(args)
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
                b'S' => {
                    // Terminal observations are complete JSON objects. Keep
                    // their frame boundaries in the buffered command result.
                    stdout.extend_from_slice(&payload);
                    stdout.push(b'\n');
                }
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

fn stream_read_loop(
    mut reader: UnixStream,
    shared: Arc<StreamShared>,
    sender: mpsc::Sender<Result<StreamEvent, Error>>,
) {
    loop {
        let (kind, payload) = match read_frame(&mut reader) {
            Ok(frame) => frame,
            Err(error) => {
                settle_stream(&shared, &sender, Err(error));
                return;
            }
        };
        match kind {
            b'A' => {
                if payload.len() != 4 {
                    settle_stream(
                        &shared,
                        &sender,
                        Err(Error::Protocol("invalid stdin acknowledgement".to_owned())),
                    );
                    return;
                }
                let acknowledged =
                    u32::from_be_bytes(payload.try_into().expect("length checked")) as usize;
                let mut state = shared.state.lock().unwrap();
                if acknowledged > state.unacknowledged {
                    drop(state);
                    settle_stream(
                        &shared,
                        &sender,
                        Err(Error::Protocol("ttcd acknowledged unsent stdin".to_owned())),
                    );
                    return;
                }
                state.unacknowledged -= acknowledged;
                shared.changed.notify_all();
            }
            b'X' => {
                if payload.len() != 4 {
                    settle_stream(
                        &shared,
                        &sender,
                        Err(Error::Protocol("invalid status frame".to_owned())),
                    );
                    return;
                }
                let status = u32::from_be_bytes(payload.try_into().expect("length checked"));
                settle_stream(&shared, &sender, Ok(StreamEvent::Exited(status)));
                return;
            }
            b'O' | b'E' | b'S' | b'T' => {
                if shared.state.lock().unwrap().settled {
                    return;
                }
                let event = match kind {
                    b'O' => StreamEvent::Stdout(payload),
                    b'E' => StreamEvent::Stderr(payload),
                    b'S' => StreamEvent::Observation(payload),
                    b'T' => StreamEvent::Terminal(payload),
                    _ => unreachable!(),
                };
                if sender.send(Ok(event)).is_err() {
                    settle_stream(&shared, &sender, Err(Error::Cancelled));
                    return;
                }
            }
            _ => {
                settle_stream(
                    &shared,
                    &sender,
                    Err(Error::Protocol(format!(
                        "unexpected ttcd frame type 0x{kind:02x}"
                    ))),
                );
                return;
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

fn absolute_path(path: &Path) -> Result<PathBuf, Error> {
    if path.is_absolute() {
        Ok(path.to_owned())
    } else {
        Ok(env::current_dir()?.join(path))
    }
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
            write_frame(&mut stream, b'O', br#"{"version":7}"#).unwrap();
            write_frame(&mut stream, b'X', &0u32.to_be_bytes()).unwrap();
        });
        assert_eq!(Client::new(&path).ping().unwrap()["version"], API_VERSION);
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn terminal_observation_frames_preserve_records() {
        let path = temporary_socket("terminal-observations");
        let server = mock_server(&path, |mut stream, _request| {
            write_frame(&mut stream, b'S', br#"{"version":1,"output_bytes":4}"#).unwrap();
            write_frame(&mut stream, b'S', br#"{"version":1,"output_bytes":9}"#).unwrap();
            write_frame(&mut stream, b'X', &0u32.to_be_bytes()).unwrap();
        });
        let result = Client::new(&path)
            .command(["tty", "watch", "demo@main", "--session", "repl"])
            .unwrap();
        assert_eq!(
            result.stdout,
            b"{\"version\":1,\"output_bytes\":4}\n{\"version\":1,\"output_bytes\":9}\n"
        );
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn durable_wait_uses_current_cli_shape() {
        let path = temporary_socket("durable-wait");
        let server = mock_server(&path, |mut stream, request| {
            assert_eq!(
                request["args"],
                json!(["wait", "--condition", "durable", "demo@main"])
            );
            write_frame(&mut stream, b'X', &0u32.to_be_bytes()).unwrap();
        });
        assert!(Client::new(&path)
            .wait_for("demo@main", "durable")
            .unwrap()
            .ok());
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn stream_delivers_observations_and_status() {
        let path = temporary_socket("stream-observations");
        let server = mock_server(&path, |mut stream, request| {
            assert_eq!(
                request["args"],
                json!(["tty", "watch", "demo@main", "--session", "repl"])
            );
            write_frame(&mut stream, b'S', br#"{"output_bytes":1}"#).unwrap();
            write_frame(&mut stream, b'S', br#"{"output_bytes":2}"#).unwrap();
            write_frame(&mut stream, b'X', &0u32.to_be_bytes()).unwrap();
        });
        let stream = Client::new(&path).watch_tty("demo@main", "repl").unwrap();
        assert_eq!(
            stream.recv().unwrap(),
            StreamEvent::Observation(br#"{"output_bytes":1}"#.to_vec())
        );
        assert_eq!(
            stream.recv().unwrap(),
            StreamEvent::Observation(br#"{"output_bytes":2}"#.to_vec())
        );
        assert_eq!(stream.wait().unwrap(), 0);
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn attach_orders_size_before_stdin() {
        let path = temporary_socket("stream-attach");
        let server = mock_server(&path, |mut socket, request| {
            assert_eq!(
                request["args"],
                json!(["attach", "--interactive", "--exec", "repl", "demo@main"])
            );
            let (kind, payload) = read_frame(&mut socket).unwrap();
            assert_eq!(kind, b'W');
            assert_eq!(&payload[..4], &40u32.to_be_bytes());
            assert_eq!(&payload[4..], &120u32.to_be_bytes());
            let (kind, payload) = read_frame(&mut socket).unwrap();
            assert_eq!((kind, payload.as_slice()), (b'I', b"echo hi\n".as_slice()));
            write_frame(&mut socket, b'A', &(payload.len() as u32).to_be_bytes()).unwrap();
            write_frame(&mut socket, b'X', &0u32.to_be_bytes()).unwrap();
        });
        let stream = Client::new(&path)
            .attach("demo@main", Some("repl"), true, false, (40, 120))
            .unwrap();
        stream.write(b"echo hi\n").unwrap();
        assert_eq!(stream.wait().unwrap(), 0);
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn stream_flow_controls_more_than_one_window() {
        let path = temporary_socket("stream-flow-control");
        let expected = vec![b'z'; STDIN_WINDOW + 31];
        let server_expected = expected.clone();
        let server = mock_server(&path, move |mut socket, request| {
            assert_eq!(request["stdin"], true);
            let mut received = Vec::new();
            loop {
                let (kind, payload) = read_frame(&mut socket).unwrap();
                if kind == b'C' {
                    break;
                }
                assert_eq!(kind, b'I');
                received.extend_from_slice(&payload);
                write_frame(&mut socket, b'A', &(payload.len() as u32).to_be_bytes()).unwrap();
            }
            assert_eq!(received, server_expected);
            write_frame(&mut socket, b'X', &0u32.to_be_bytes()).unwrap();
        });
        let stream = Client::new(&path)
            .stream(
                ["exec", "demo@main", "cat"],
                StreamOptions {
                    stdin: true,
                    ..Default::default()
                },
            )
            .unwrap();
        stream.write(&expected).unwrap();
        stream.close_stdin().unwrap();
        assert_eq!(stream.wait().unwrap(), 0);
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn close_is_cancellation_and_stdin_stays_closed() {
        let path = temporary_socket("stream-cancel");
        let server = mock_server(&path, |mut socket, _request| {
            let _ = read_frame(&mut socket);
            let mut byte = [0u8; 1];
            let _ = socket.read(&mut byte);
        });
        let stream = Client::new(&path)
            .stream(
                ["attach", "demo@main"],
                StreamOptions {
                    stdin: true,
                    ..Default::default()
                },
            )
            .unwrap();
        stream.close_stdin().unwrap();
        assert!(matches!(
            stream.write(b"late"),
            Err(Error::InvalidArguments(_))
        ));
        stream.close();
        assert!(matches!(stream.wait(), Err(Error::Cancelled)));
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn command_streams_flow_controlled_stdin_and_preserves_status() {
        let path = temporary_socket("stdin");
        let expected = vec![b'x'; STDIN_WINDOW + 29];
        let server_expected = expected.clone();
        let server = mock_server(&path, move |mut stream, request| {
            assert_eq!(request["args"], json!(["exec", "demo@main", "cat"]));
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
                ["exec", "demo@main", "cat"],
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

    #[test]
    fn batch_remote_fork_uses_current_cli_shape() {
        let path = temporary_socket("fork-many");
        let server = mock_server(&path, |mut stream, request| {
            assert_eq!(
                request["args"],
                json!([
                    "fork",
                    "--paused",
                    "--node",
                    "node-b",
                    "demo@main",
                    "remote@one",
                    "remote@two"
                ])
            );
            write_frame(&mut stream, b'X', &0u32.to_be_bytes()).unwrap();
        });
        let result = Client::new(&path)
            .fork_many(
                "demo@main",
                &["remote@one", "remote@two"],
                true,
                Some("node-b"),
            )
            .unwrap();
        assert!(result.ok());
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn elastic_migration_uses_current_cli_shape() {
        let path = temporary_socket("migrate");
        let server = mock_server(&path, |mut stream, request| {
            assert_eq!(
                request["args"],
                json!(["migrate", "--node", "node-b", "demo@main"])
            );
            write_frame(&mut stream, b'X', &0u32.to_be_bytes()).unwrap();
        });
        let result = Client::new(&path).migrate("demo@main", "node-b").unwrap();
        assert!(result.ok());
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn elastic_node_stop_uses_current_cli_shape() {
        let path = temporary_socket("node-stop");
        let server = mock_server(&path, |mut stream, request| {
            assert_eq!(
                request["args"],
                json!(["node", "stop", "--migrate-all", "node-a"])
            );
            write_frame(&mut stream, b'X', &0u32.to_be_bytes()).unwrap();
        });
        let result = Client::new(&path)
            .stop_node("node-a", Some("migrate"))
            .unwrap();
        assert!(result.ok());
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn image_helpers_use_docker_compatible_command_shapes() {
        assert!(matches!(
            Client::new("/unused").load(ImageLoadOptions::default()),
            Err(Error::InvalidArguments(_))
        ));

        let path = temporary_socket("image-commit");
        let server = mock_server(&path, |mut stream, request| {
            assert_eq!(
                request["args"],
                json!([
                    "commit",
                    "--author",
                    "TTC Harness",
                    "--message",
                    "prepared",
                    "--change",
                    "LABEL timetravel.usage=review",
                    "--pause=false",
                    "demo@main",
                    "local/result:latest"
                ])
            );
            write_frame(&mut stream, b'X', &0u32.to_be_bytes()).unwrap();
        });
        let result = Client::new(&path)
            .commit(
                "demo@main",
                "local/result:latest",
                CommitImageOptions {
                    author: Some("TTC Harness".to_owned()),
                    message: Some("prepared".to_owned()),
                    changes: vec!["LABEL timetravel.usage=review".to_owned()],
                    pause: false,
                },
            )
            .unwrap();
        assert!(result.ok());
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();

        let path = temporary_socket("image-load");
        let server = mock_server(&path, |mut stream, request| {
            assert_eq!(
                request["args"],
                json!(["load", "--platform", "linux/amd64", "--quiet"])
            );
            assert_eq!(request["stdin"], true);
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
            assert_eq!(received, b"image archive");
            write_frame(&mut stream, b'X', &0u32.to_be_bytes()).unwrap();
        });
        let result = Client::new(&path)
            .load(ImageLoadOptions {
                archive: Some(b"image archive".to_vec()),
                platform: Some("linux/amd64".to_owned()),
                quiet: true,
                ..Default::default()
            })
            .unwrap();
        assert!(result.ok());
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();

        let path = temporary_socket("image-save");
        let server = mock_server(&path, |mut stream, request| {
            assert_eq!(
                request["args"],
                json!([
                    "save",
                    "--output",
                    "/srv/images.tar",
                    "one:latest",
                    "two:test"
                ])
            );
            write_frame(&mut stream, b'X', &0u32.to_be_bytes()).unwrap();
        });
        let result = Client::new(&path)
            .save(
                &["one:latest", "two:test"],
                ImageSaveOptions {
                    output: Some(PathBuf::from("/srv/images.tar")),
                    ..Default::default()
                },
            )
            .unwrap();
        assert!(result.ok());
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }

    #[test]
    fn checkpoint_normalizes_daemon_host_path() {
        let path = temporary_socket("checkpoint");
        let expected = env::current_dir().unwrap().join("state.ttc");
        let server = mock_server(&path, move |mut stream, request| {
            assert_eq!(
                request["args"],
                json!([
                    "checkpoint",
                    "--compression",
                    "none",
                    "demo@main",
                    expected.to_string_lossy()
                ])
            );
            write_frame(&mut stream, b'X', &0u32.to_be_bytes()).unwrap();
        });
        let result = Client::new(&path)
            .checkpoint("demo@main", "state.ttc", "none")
            .unwrap();
        assert!(result.ok());
        server.join().unwrap();
        fs::remove_dir_all(path.parent().unwrap()).unwrap();
    }
}
