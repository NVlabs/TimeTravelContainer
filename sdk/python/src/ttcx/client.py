# Copyright 2026 The gVisor Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     https://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Direct client for ttcd's local Unix-socket API."""

from __future__ import annotations

import json
import os
import secrets
import select
import socket
import struct
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping, Sequence

API_VERSION = 4
MAX_FRAME = 1 << 20
STDIN_WINDOW = 1 << 20


class TTCError(Exception):
    """Base class for TTC SDK errors."""


class ProtocolError(TTCError):
    """The daemon closed the connection or returned an invalid frame."""


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


def default_socket_path(env: Mapping[str, str] | None = None) -> str:
    values = os.environ if env is None else env
    if runtime := values.get("TTC_RUNTIME_DIR"):
        return str(Path(runtime) / "ttcd.sock")
    if runtime := values.get("XDG_RUNTIME_DIR"):
        return str(Path(runtime) / "ttc" / "ttcd.sock")
    return f"/tmp/ttc-{os.geteuid()}/ttcd.sock"


def _frame(kind: bytes, payload: bytes = b"") -> bytes:
    if len(kind) != 1 or len(payload) > MAX_FRAME:
        raise ValueError("invalid TTC frame")
    return kind + struct.pack(">I", len(payload)) + payload


class Client:
    """A synchronous, non-interactive ttcd client."""

    def __init__(self, socket_path: str | os.PathLike[str] | None = None, timeout: float = 30.0):
        self.socket_path = os.fspath(socket_path) if socket_path is not None else default_socket_path()
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
        result = self._exchange(request, data, timeout)
        return result.check() if check else result

    def list(self) -> Any:
        return self.command(["ls", "--json"], check=True).json()

    def inspect(self, target: str) -> Any:
        return self.command(["inspect", target], check=True).json()

    def pull(self, image: str) -> Result:
        return self.command(["pull", image])

    def run(self, image: str, command: Sequence[str] = (), *, options: Sequence[str] = ()) -> Result:
        return self.command(["run", *options, image, *command])

    def exec(self, target: str, command: Sequence[str], *, input: bytes | str | None = None) -> Result:
        return self.command(["exec", target, *command], input=input)

    def fork(self, source: str, destination: str, *, paused: bool = False) -> Result:
        args = ["fork"]
        if paused:
            args.append("--paused")
        return self.command([*args, source, destination])

    def pause(self, target: str) -> Result:
        return self.command(["pause", target])

    def resume(self, target: str) -> Result:
        return self.command(["resume", target])

    def stop(self, target: str) -> Result:
        return self.command(["stop", target])

    def remove(self, target: str, *, force: bool = False) -> Result:
        return self.command(["rm", *(["--force"] if force else []), target])

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
                        continue
                    elif kind == b"X":
                        if len(payload) != 4:
                            raise ProtocolError("invalid status frame")
                        return Result(struct.unpack(">I", payload)[0], bytes(stdout), bytes(stderr))
                    else:
                        raise ProtocolError(f"unexpected ttcd frame type {kind!r}")
        finally:
            sock.close()
