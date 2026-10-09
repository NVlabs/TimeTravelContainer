# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Direct client for ttcd's local Unix-socket API."""

from __future__ import annotations

import json
import os
import secrets
import select
import socket
import struct
import threading
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

# The version of ttcd's API, which must equal the daemon's (DaemonAPIVersion in
# runsc/ttc/protocol/protocol.go): ttcd refuses any other.
API_VERSION = 10
MAX_FRAME = 1 << 20
STDIN_WINDOW = 1 << 20


class TTCError(Exception):
    """Base class for TTC SDK errors."""


class ProtocolError(TTCError):
    """The daemon closed the connection or returned an invalid frame."""


class StreamCancelled(TTCError):
    """A local caller closed a stream before ttcd returned its status."""


class CommandFailed(TTCError):
    """Raised by Result.check() for a nonzero TTC command."""

    def __init__(self, result: "Result") -> None:
        super().__init__(
            f"TTC command exited with status {result.status}: "
            f"{result.stderr_text().strip()}"
        )
        self.result = result


@dataclass(frozen=True)
class Result:
    """The complete result of one daemon operation."""

    status: int
    stdout: bytes
    stderr: bytes

    @property
    def ok(self) -> bool:
        return self.status == 0

    def stdout_text(self, encoding: str = "utf-8") -> str:
        return self.stdout.decode(encoding, errors="replace")

    def stderr_text(self, encoding: str = "utf-8") -> str:
        return self.stderr.decode(encoding, errors="replace")

    def json(self) -> Any:
        return json.loads(self.stdout)

    def check(self) -> "Result":
        if not self.ok:
            raise CommandFailed(self)
        return self


SOCKET_NAME = "ttcd.sock"


def socket_path(root: str | os.PathLike[str]) -> str:
    """Returns the socket of the ttcd whose root is root.

    A daemon's root, the TTC_ROOT of a standalone ttcd or the TTC_LOCAL_ROOT
    of a node of a cluster, identifies it.
    """
    return os.path.join(os.fspath(root), SOCKET_NAME)


def _frame(kind: bytes, payload: bytes = b"") -> bytes:
    if len(kind) != 1 or len(payload) > MAX_FRAME:
        raise ValueError("invalid TTC frame")
    return kind + struct.pack(">I", len(payload)) + payload


def _size_frame(rows: int, cols: int) -> bytes:
    for name, value in (("rows", rows), ("cols", cols)):
        if (
            not isinstance(value, int)
            or isinstance(value, bool)
            or not 0 <= value <= 0xFFFFFFFF
        ):
            raise ValueError(f"{name} must be a uint32")
    return _frame(b"W", struct.pack(">II", rows, cols))


# The values of a boolean flag that Go's strconv.ParseBool accepts.
_TRUE = ("1", "t", "T", "TRUE", "true", "True")
_FALSE = ("0", "f", "F", "FALSE", "false", "False")


def _bool_flag(args: Sequence[str], short: str, long: str) -> bool:
    value = False
    for arg in args:
        if arg == "--":
            break
        if arg == long:
            value = True
        elif arg.startswith(long + "="):
            parsed = arg[len(long) + 1 :]
            if parsed in _TRUE:
                value = True
            elif parsed in _FALSE:
                value = False
        elif len(arg) > 1 and arg[0] == "-" and arg[1] != "-" and short in arg[1:]:
            value = True
    return value


def uses_terminal_control(args: Sequence[str]) -> bool:
    """Reports whether a command can ask its client for a terminal, as the CLI
    decides (protocol.CommandUsesTerminalControl): a daemon of a cluster relays
    the terminal frames of such a command to the node that runs it."""
    if args and args[0] == "container":
        args = args[1:]
    if not args:
        return False
    if args[0] == "attach":
        return True
    if args[0] == "start":
        return _bool_flag(args[1:], "a", "--attach")
    if args[0] in ("run", "exec"):
        if args[0] == "exec" and len(args) > 1 and args[1] in ("ls", "wait", "kill"):
            return False
        return _bool_flag(args[1:], "t", "--tty")
    return False


class TTCStream:
    """One long-lived ttcd operation with incremental callbacks and stdin."""

    def __init__(
        self,
        socket_path: str,
        request: Mapping[str, Any],
        *,
        stdin: bool = False,
        timeout: float | None = None,
        size: tuple[int, int] | None = None,
        on_stdout: Callable[[bytes], None] | None = None,
        on_stderr: Callable[[bytes], None] | None = None,
        on_observation: Callable[[bytes], None] | None = None,
        on_terminal: Callable[[bytes], None] | None = None,
    ) -> None:
        self.args = tuple(request.get("args", ()))
        self._stdin_declared = stdin
        self._callbacks = {
            b"O": on_stdout,
            b"E": on_stderr,
            b"S": on_observation,
            b"T": on_terminal,
        }
        initial_size = _size_frame(*size) if size is not None else None
        self._size_sent = size is not None
        self._socket = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self._socket.settimeout(timeout)
        self._send_lock = threading.Lock()
        self._input_lock = threading.Lock()
        self._state = threading.Condition()
        self._unacknowledged = 0
        self._settled = False
        self._eof_requested = False
        self._eof_sent = False
        self._status: int | None = None
        self._error: BaseException | None = None

        # Python's connect is synchronous, so the header and initial size are
        # fully ordered before this object becomes visible to the caller.
        try:
            self._socket.connect(socket_path)
            header = json.dumps(request, separators=(",", ":")).encode() + b"\n"
            self._socket.sendall(header)
            if initial_size is not None:
                self._socket.sendall(initial_size)
        except BaseException:
            self._socket.close()
            raise
        self._reader = threading.Thread(target=self._read_loop, daemon=True)
        self._reader.start()

    def write(self, data: bytes | str) -> None:
        """Write stdin, blocking only while ttcd's 1 MiB window is full."""
        if not self._stdin_declared:
            raise TTCError("stream was not opened with stdin enabled")
        payload = data.encode() if isinstance(data, str) else bytes(data)
        with self._input_lock:
            with self._state:
                if self._eof_requested or self._eof_sent:
                    raise TTCError("stdin already closed")
            offset = 0
            while offset < len(payload):
                with self._state:
                    while not self._settled and self._unacknowledged >= STDIN_WINDOW:
                        self._state.wait()
                    self._raise_if_settled()
                    size = min(
                        MAX_FRAME,
                        STDIN_WINDOW - self._unacknowledged,
                        len(payload) - offset,
                    )
                    self._unacknowledged += size
                try:
                    self._send(_frame(b"I", payload[offset : offset + size]))
                except BaseException as error:
                    self._finish(error=error)
                    raise
                offset += size

    def close_stdin(self) -> None:
        """Send stdin EOF after any currently executing write completes."""
        if not self._stdin_declared:
            raise TTCError("stream was not opened with stdin enabled")
        with self._input_lock:
            with self._state:
                if self._eof_requested or self._eof_sent:
                    return
                self._raise_if_settled()
                self._eof_requested = True
            try:
                self._send(_frame(b"C"))
            except BaseException as error:
                self._finish(error=error)
                raise
            with self._state:
                self._eof_sent = True

    def resize(self, rows: int, cols: int) -> None:
        self._send(_size_frame(rows, cols), size=True)

    def close(self) -> None:
        """Cancel locally without fabricating a successful daemon status."""
        self._finish(
            error=StreamCancelled(
                "TTC stream was cancelled before returning status"
            )
        )

    def wait(self, timeout: float | None = None) -> int:
        """Wait for ttcd's status frame, raising on transport failure/cancellation."""
        deadline = None if timeout is None else time.monotonic() + timeout
        with self._state:
            while not self._settled:
                remaining = None if deadline is None else deadline - time.monotonic()
                if remaining is not None and remaining <= 0:
                    raise TimeoutError("timed out waiting for TTC stream status")
                self._state.wait(remaining)
            if self._error is not None:
                raise self._error
            assert self._status is not None
            return self._status

    def _send(self, data: bytes, *, size: bool = False) -> None:
        with self._state:
            self._raise_if_settled()
        with self._send_lock:
            if size:
                self._size_sent = True
            self._socket.sendall(data)

    def _answer_terminal(self) -> None:
        # ttcd asks the client of a terminal command for its terminal's size,
        # and waits for it: a stream given none has no terminal, which the CLI
        # answers so too when its own is none.
        with self._send_lock:
            if self._size_sent:
                return
            self._size_sent = True
            self._socket.sendall(_size_frame(0, 0))

    def _raise_if_settled(self) -> None:
        if not self._settled:
            return
        if self._error is not None:
            raise self._error
        raise TTCError(f"stream already exited with status {self._status}")

    def _read_loop(self) -> None:
        try:
            while True:
                kind, payload = self._read_frame()
                if kind == b"A":
                    if len(payload) != 4:
                        raise ProtocolError("invalid stdin acknowledgement")
                    acknowledged = struct.unpack(">I", payload)[0]
                    with self._state:
                        if acknowledged > self._unacknowledged:
                            raise ProtocolError("ttcd acknowledged unsent stdin")
                        self._unacknowledged -= acknowledged
                        self._state.notify_all()
                    continue
                if kind == b"X":
                    if len(payload) != 4:
                        raise ProtocolError("invalid status frame")
                    self._finish(status=struct.unpack(">I", payload)[0])
                    return
                callback = self._callbacks.get(kind)
                if kind not in self._callbacks:
                    raise ProtocolError(f"unexpected ttcd frame type {kind!r}")
                if callback is not None:
                    callback(payload)
                if kind == b"T":
                    self._answer_terminal()
                with self._state:
                    if self._settled:
                        return
        except BaseException as error:
            self._finish(error=error)

    def _read_frame(self) -> tuple[bytes, bytes]:
        header = self._read_exact(5)
        length = struct.unpack(">I", header[1:])[0]
        if length > MAX_FRAME:
            raise ProtocolError(f"oversized ttcd frame: {length} bytes")
        return header[:1], self._read_exact(length)

    def _read_exact(self, size: int) -> bytes:
        result = bytearray()
        while len(result) < size:
            chunk = self._socket.recv(size - len(result))
            if not chunk:
                raise ProtocolError("ttcd closed the connection before returning status")
            result.extend(chunk)
        return bytes(result)

    def _finish(
        self, *, error: BaseException | None = None, status: int | None = None
    ) -> None:
        with self._state:
            if self._settled:
                return
            self._settled = True
            self._error = error
            self._status = status
            self._state.notify_all()
        try:
            self._socket.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass
        self._socket.close()


class Client:
    """A synchronous, non-interactive ttcd client."""

    def __init__(self, root: str | os.PathLike[str], timeout: float = 30.0):
        """Returns a client of the ttcd whose root is root."""
        self.root = os.fspath(root)
        self.socket_path = socket_path(self.root)
        self.timeout = timeout

    def ping(self, *, timeout: float | None = None) -> dict[str, Any]:
        result = self._exchange({"version": API_VERSION, "action": "ping"}, b"", timeout)
        return result.check().json()

    def command(
        self,
        args: Sequence[str],
        *,
        input: bytes | str | None = None,
        request_id: str | None = None,
        timeout: float | None = None,
        check: bool = False,
    ) -> Result:
        if not args or any(not isinstance(arg, str) for arg in args):
            raise ValueError("args must be a non-empty sequence of strings")
        data = input.encode() if isinstance(input, str) else (input or b"")
        request: dict[str, Any] = {
            "version": API_VERSION,
            "action": "command",
            "request_id": request_id or secrets.token_hex(16),
            "args": list(args),
            "stdin": input is not None,
        }
        if uses_terminal_control(args):
            request["terminal_control"] = True
        result = self._exchange(request, data, timeout)
        return result.check() if check else result

    def list(self) -> Any:
        return self.command(["ls", "--json"], check=True).json()

    def inspect(self, container: str) -> Any:
        """Inspect a container family; branch details are included in TTC.branches."""
        return self.command(["inspect", container], check=True).json()

    def pull(self, image: str) -> Result:
        return self.command(["pull", image])

    def run(self, image: str, command: Sequence[str] = (), *, options: Sequence[str] = (), node: str | None = None) -> Result:
        return self.command(["run", *(["--node", node] if node else []), *options, image, *command])

    def start(self, target: str, *, node: str | None = None) -> Result:
        return self.command(["start", *(["--node", node] if node else []), target])

    def exec(self, target: str, command: Sequence[str], *, input: bytes | str | None = None) -> Result:
        # ttcd gives a process its client's input only with --interactive.
        options = ["--interactive"] if input is not None else []
        return self.command(["exec", *options, target, *command], input=input)

    def exec_session(
        self,
        target: str,
        command: Sequence[str],
        *,
        session: str,
        tty: bool = False,
        interactive: bool = False,
    ) -> Result:
        options = ["--session", session, "--detach"]
        if tty:
            options.append("--tty")
        if interactive:
            options.append("--interactive")
        return self.command(["exec", *options, target, *command])

    def exec_sessions(self, target: str) -> Any:
        return self.command(["exec", "ls", "--json", target], check=True).json()

    def exec_logs(self, target: str, session: str) -> Result:
        return self.command(["logs", "--exec", session, target])

    def exec_wait(self, target: str, session: str) -> Result:
        return self.command(["exec", "wait", f"{target}:{session}"], timeout=None)

    def tty_stat(self, target: str, session: str) -> Any:
        return self.command(
            ["tty", "stat", target, "--session", session, "--json"], check=True
        ).json()

    def commit(
        self,
        target: str,
        image: str,
        *,
        author: str | None = None,
        message: str | None = None,
        changes: Sequence[str] = (),
        pause: bool = True,
    ) -> Result:
        options: list[str] = []
        if author is not None:
            options.extend(["--author", author])
        if message is not None:
            options.extend(["--message", message])
        for change in changes:
            options.extend(["--change", change])
        if not pause:
            options.append("--pause=false")
        return self.command(["commit", *options, target, image])

    def load(
        self,
        *,
        input: str | None = None,
        archive: bytes | None = None,
        platform: str | None = None,
        quiet: bool = False,
    ) -> Result:
        if input is not None and archive is not None:
            raise ValueError("image load accepts either input or archive, not both")
        if input is None and archive is None:
            raise ValueError("image load requires input or archive")
        args = ["load"]
        if input is not None:
            args.extend(["--input", str(Path(input).expanduser().resolve())])
        if platform is not None:
            args.extend(["--platform", platform])
        if quiet:
            args.append("--quiet")
        return self.command(args, input=archive)

    def save(
        self,
        images: Sequence[str],
        *,
        output: str | None = None,
        platform: str | None = None,
    ) -> Result:
        if not images:
            raise ValueError("image save requires at least one image")
        args = ["save"]
        if output is not None:
            args.extend(["--output", str(Path(output).expanduser().resolve())])
        if platform is not None:
            args.extend(["--platform", platform])
        return self.command([*args, *images])

    def stats(self, target: str) -> Any:
        return self.command(["stats", "--json", target], check=True).json()

    def diff(self, target: str) -> Any:
        return self.command(["diff", "--json", target], check=True).json()

    def copy(self, source: str, destination: str) -> Result:
        return self.command(["cp", source, destination])

    def fork(
        self,
        source: str,
        destination: str,
        *,
        paused: bool = False,
        node: str | None = None,
    ) -> Result:
        return self.fork_many(
            source, [destination], paused=paused, node=node
        )

    def fork_many(
        self,
        source: str,
        destinations: Sequence[str],
        *,
        paused: bool = False,
        node: str | None = None,
    ) -> Result:
        if not destinations:
            raise ValueError("destinations must not be empty")
        args = ["fork"]
        if paused:
            args.append("--paused")
        if node is not None:
            args.extend(["--node", node])
        return self.command([*args, source, *destinations])

    def migrate(self, target: str, node: str) -> Result:
        """Move a branch to another elastic-cluster node without renaming it."""
        return self.command(["migrate", "--node", node, target])

    def nodes(self) -> Any:
        """Return dynamic cluster membership and current memory accounting."""
        return self.command(["node", "ls", "--json"], check=True).json()

    def stop_node(self, node: str, *, policy: str | None = None) -> Result:
        if policy not in (None, "stop", "dehydrate", "migrate"):
            raise ValueError("policy must be stop, dehydrate, migrate, or None")
        option = f"--{policy}-all" if policy else None
        return self.command(["node", "stop", *([option] if option else []), node])

    def rebalance_node(self, node: str) -> Any:
        """Move a pressure-relieving subset of node's branches to nodes that
        the cluster's scheduler chooses, and return the plan."""
        return self.command(["node", "rebalance", node], check=True).json()

    def pause(self, target: str) -> Result:
        return self.command(["pause", target])

    def resume(self, target: str) -> Result:
        return self.command(["resume", target])

    def quarantine(self, target: str) -> Result:
        """Disconnect a branch's runtime-managed external network edge."""
        return self.command(["quarantine", target])

    def unquarantine(self, target: str) -> Result:
        """Reconnect a quarantined branch to TTC-managed external networking."""
        return self.command(["unquarantine", target])

    def dehydrate(self, target: str) -> Result:
        return self.command(["dehydrate", target])

    def rehydrate(self, target: str, *, node: str | None = None) -> Result:
        return self.command(["rehydrate", *(["--node", node] if node else []), target])

    def checkpoint(
        self,
        target: str,
        file: str | os.PathLike[str],
        *,
        compression: str = "flate-best-speed",
    ) -> Result:
        destination = str(Path(file).expanduser().resolve())
        return self.command(
            ["checkpoint", "--compression", compression, target, destination]
        )

    def restore(
        self, file: str | os.PathLike[str], *, name: str | None = None, node: str | None = None
    ) -> Result:
        source = str(Path(file).expanduser().resolve())
        return self.command(["restore", *(["--node", node] if node else []), *(["--name", name] if name else []), source])

    def stop(self, target: str, *, timeout: int | None = None) -> Result:
        return self.command(
            ["stop", *(["--time", str(timeout)] if timeout is not None else []), target]
        )

    def restart(self, target: str, *, timeout: int | None = None) -> Result:
        return self.command(
            ["restart", *(["--time", str(timeout)] if timeout is not None else []), target]
        )

    def kill(self, target: str, *, signal: str | int | None = None) -> Result:
        return self.command(
            ["kill", *(["--signal", str(signal)] if signal is not None else []), target]
        )

    def wait(self, target: str, *, condition: str = "not-running") -> Result:
        if condition not in ("not-running", "durable"):
            raise ValueError("condition must be 'not-running' or 'durable'")
        return self.command(["wait", "--condition", condition, target], timeout=None)

    def remove(self, target: str, *, force: bool = False) -> Result:
        return self.command(["rm", *(["--force"] if force else []), target])

    def stream(
        self,
        args: Sequence[str],
        *,
        stdin: bool = False,
        request_id: str | None = None,
        timeout: float | None = None,
        size: tuple[int, int] | None = None,
        on_stdout: Callable[[bytes], None] | None = None,
        on_stderr: Callable[[bytes], None] | None = None,
        on_observation: Callable[[bytes], None] | None = None,
        on_terminal: Callable[[bytes], None] | None = None,
    ) -> TTCStream:
        """Open a long-lived operation without buffering its output."""
        if not args or any(not isinstance(arg, str) for arg in args):
            raise ValueError("args must be a non-empty sequence of strings")
        request: dict[str, Any] = {
            "version": API_VERSION,
            "action": "command",
            "request_id": request_id or secrets.token_hex(16),
            "args": list(args),
            "stdin": stdin,
        }
        if uses_terminal_control(args):
            request["terminal_control"] = True
        return TTCStream(
            self.socket_path,
            request,
            stdin=stdin,
            timeout=timeout,
            size=size,
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            on_observation=on_observation,
            on_terminal=on_terminal,
        )

    def watch_tty(
        self,
        target: str,
        session: str,
        on_observation: Callable[[bytes], None],
    ) -> TTCStream:
        return self.stream(
            ["tty", "watch", target, "--session", session],
            on_observation=on_observation,
        )

    def attach(
        self,
        target: str,
        *,
        exec_session: str | None = None,
        stdin: bool = True,
        takeover: bool = False,
        size: tuple[int, int] = (24, 80),
        on_stdout: Callable[[bytes], None] | None = None,
        on_stderr: Callable[[bytes], None] | None = None,
        on_terminal: Callable[[bytes], None] | None = None,
    ) -> TTCStream:
        return self.stream(
            [
                "attach",
                *(["--interactive"] if stdin else ["--no-stdin"]),
                *(["--takeover"] if takeover else []),
                *(["--exec", exec_session] if exec_session else []),
                target,
            ],
            stdin=stdin,
            size=size,
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            on_terminal=on_terminal,
        )

    def watch_events(
        self,
        *,
        since: str | None = None,
        filters: Mapping[str, str] | None = None,
        on_stdout: Callable[[bytes], None] | None = None,
        on_stderr: Callable[[bytes], None] | None = None,
    ) -> TTCStream:
        filter_args = [
            value
            for key, match in (filters or {}).items()
            for value in ("--filter", f"{key}={match}")
        ]
        return self.stream(
            [
                "events",
                "--json",
                "--follow",
                *(["--since", since] if since else []),
                *filter_args,
            ],
            on_stdout=on_stdout,
            on_stderr=on_stderr,
        )

    def follow_logs(
        self,
        target: str,
        *,
        exec_session: str | None = None,
        tail: str | int | None = None,
        on_stdout: Callable[[bytes], None] | None = None,
        on_stderr: Callable[[bytes], None] | None = None,
    ) -> TTCStream:
        return self.stream(
            [
                "logs",
                "--follow",
                *(["-n", str(tail)] if tail is not None else []),
                *(["--exec", exec_session] if exec_session else []),
                target,
            ],
            on_stdout=on_stdout,
            on_stderr=on_stderr,
        )

    def _exchange(
        self, request: Mapping[str, Any], input_data: bytes, timeout: float | None
    ) -> Result:
        limit = self.timeout if timeout is None else timeout
        deadline = None if limit is None else time.monotonic() + limit
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            sock.settimeout(limit)
            sock.connect(self.socket_path)
            header = json.dumps(request, separators=(",", ":")).encode() + b"\n"
            sock.sendall(header)
            sock.setblocking(False)

            receive = bytearray()
            stdout = bytearray()
            stderr = bytearray()
            send_buffer = b""
            size_sent = False
            input_offset = 0
            unacknowledged = 0
            eof_sent = not bool(request.get("stdin"))

            while True:
                if deadline is not None and time.monotonic() >= deadline:
                    raise TimeoutError(f"ttcd operation timed out after {limit} seconds")

                if not send_buffer and not eof_sent:
                    available = STDIN_WINDOW - unacknowledged
                    if input_offset < len(input_data) and available > 0:
                        size = min(MAX_FRAME, available, len(input_data) - input_offset)
                        payload = input_data[input_offset : input_offset + size]
                        input_offset += size
                        unacknowledged += size
                        send_buffer = _frame(b"I", payload)
                    elif input_offset == len(input_data):
                        send_buffer = _frame(b"C")
                        eof_sent = True

                wait = None if deadline is None else max(0.0, deadline - time.monotonic())
                readable, writable, _ = select.select(
                    [sock], [sock] if send_buffer else [], [], wait
                )
                if writable:
                    sent = sock.send(send_buffer)
                    send_buffer = send_buffer[sent:]
                if not readable:
                    continue
                chunk = sock.recv(65536)
                if not chunk:
                    raise ProtocolError("ttcd closed the connection before returning status")
                receive.extend(chunk)
                while len(receive) >= 5:
                    length = struct.unpack(">I", receive[1:5])[0]
                    if length > MAX_FRAME:
                        raise ProtocolError(f"oversized ttcd frame: {length} bytes")
                    if len(receive) < 5 + length:
                        break
                    kind = bytes(receive[:1])
                    payload = bytes(receive[5 : 5 + length])
                    del receive[: 5 + length]
                    if kind == b"O":
                        stdout.extend(payload)
                    elif kind == b"S":
                        # Terminal observations are complete JSON objects. Keep
                        # command() lossless by exposing each object as one
                        # newline-delimited stdout record.
                        stdout.extend(payload)
                        stdout.extend(b"\n")
                    elif kind == b"E":
                        stderr.extend(payload)
                    elif kind == b"A":
                        if len(payload) != 4:
                            raise ProtocolError("invalid stdin acknowledgement")
                        acknowledged = struct.unpack(">I", payload)[0]
                        if acknowledged > unacknowledged:
                            raise ProtocolError("ttcd acknowledged unsent stdin")
                        unacknowledged -= acknowledged
                    elif kind == b"T":
                        # ttcd asks the client of a terminal command, such as
                        # a detached exec --tty, for its terminal's size, and
                        # waits for it: this client has none, which the CLI
                        # answers so too when its own is none.
                        if not size_sent:
                            send_buffer += _size_frame(0, 0)
                            size_sent = True
                    elif kind == b"X":
                        if len(payload) != 4:
                            raise ProtocolError("invalid status frame")
                        return Result(struct.unpack(">I", payload)[0], bytes(stdout), bytes(stderr))
                    else:
                        raise ProtocolError(f"unexpected ttcd frame type {kind!r}")
        finally:
            sock.close()
