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

import json
import socket
import struct
import threading
import unittest
from pathlib import Path

from ttcx import Client, CommandFailed
from ttcx.client import API_VERSION, default_socket_path


def frame(kind: bytes, payload: bytes = b"") -> bytes:
    return kind + struct.pack(">I", len(payload)) + payload


def read_exact(conn: socket.socket, size: int) -> bytes:
    chunks = bytearray()
    while len(chunks) < size:
        chunk = conn.recv(size - len(chunks))
        if not chunk:
            raise EOFError
        chunks.extend(chunk)
    return bytes(chunks)


class MockDaemon:
    def __init__(self, path: Path, handler):
        self.path = path
        self.handler = handler
        self.error = None
        self.listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.listener.bind(str(path))
        self.listener.listen(1)
        self.thread = threading.Thread(target=self.run)
        self.thread.start()

    def run(self):
        try:
            conn, _ = self.listener.accept()
            with conn:
                header = bytearray()
                while not header.endswith(b"\n"):
                    header.extend(read_exact(conn, 1))
                self.handler(conn, json.loads(header))
        except BaseException as error:
            self.error = error
        finally:
            self.listener.close()

    def close(self):
        self.thread.join(5)
        assert not self.thread.is_alive()
        if self.error:
            raise self.error


class ClientTest(unittest.TestCase):
    def test_ping_uses_direct_protocol(self):
        def handler(conn, request):
            self.assertEqual(request, {"version": API_VERSION, "action": "ping"})
            conn.sendall(frame(b"O", b'{"version":4,"isolation_mode":"rootless"}\n'))
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                self.assertEqual(Client(daemon.path).ping()["version"], API_VERSION)
            finally:
                daemon.close()

    def test_command_streams_stdin_and_preserves_nonzero_result(self):
        payload = b"x" * ((1 << 20) + 17)

        def handler(conn, request):
            self.assertEqual(request["args"], ["exec", "demo@main", "cat"])
            self.assertIs(request["stdin"], True)
            self.assertEqual(len(request["request_id"]), 32)
            received = bytearray()
            while True:
                kind = read_exact(conn, 1)
                size = struct.unpack(">I", read_exact(conn, 4))[0]
                data = read_exact(conn, size)
                if kind == b"C":
                    break
                self.assertEqual(kind, b"I")
                received.extend(data)
                conn.sendall(frame(b"A", struct.pack(">I", len(data))))
            self.assertEqual(bytes(received), payload)
            conn.sendall(frame(b"O", b"partial output"))
            conn.sendall(frame(b"E", b"expected failure\n"))
            conn.sendall(frame(b"X", struct.pack(">I", 7)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                result = Client(daemon.path).command(["exec", "demo@main", "cat"], input=payload)
                self.assertEqual(result.status, 7)
                self.assertEqual(result.stdout, b"partial output")
                with self.assertRaises(CommandFailed):
                    result.check()
            finally:
                daemon.close()

    def test_default_socket_path_precedence(self):
        self.assertEqual(
            default_socket_path({"TTC_RUNTIME_DIR": "/run/a", "XDG_RUNTIME_DIR": "/run/b"}),
            "/run/a/ttcd.sock",
        )
        self.assertEqual(
            default_socket_path({"XDG_RUNTIME_DIR": "/run/b"}),
            "/run/b/ttc/ttcd.sock",
        )

    def test_terminal_observation_frames_preserve_records(self):
        def handler(conn, _request):
            conn.sendall(frame(b"S", b'{"version":1,"output_bytes":4}'))
            conn.sendall(frame(b"S", b'{"version":1,"output_bytes":9}'))
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                result = Client(daemon.path).command(["tty", "watch", "demo@main", "--session", "repl"])
                self.assertEqual(
                    result.stdout,
                    b'{"version":1,"output_bytes":4}\n{"version":1,"output_bytes":9}\n',
                )
            finally:
                daemon.close()

    def test_batch_remote_fork_uses_current_cli_shape(self):
        def handler(conn, request):
            self.assertEqual(
                request["args"],
                [
                    "fork",
                    "--paused",
                    "--target",
                    "node-b",
                    "demo@main",
                    "remote@one",
                    "remote@two",
                ],
            )
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                result = Client(daemon.path).fork_many(
                    "demo@main",
                    ["remote@one", "remote@two"],
                    paused=True,
                    target_node="node-b",
                )
                self.assertTrue(result.ok)
            finally:
                daemon.close()

    def test_checkpoint_normalizes_daemon_host_path(self):
        def handler(conn, request):
            self.assertEqual(request["args"][:4], [
                "checkpoint",
                "--compression",
                "none",
                "demo@main",
            ])
            self.assertEqual(request["args"][4], str(Path("state.ttc").resolve()))
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                self.assertTrue(
                    Client(daemon.path)
                    .checkpoint("demo@main", "state.ttc", compression="none")
                    .ok
                )
            finally:
                daemon.close()


if __name__ == "__main__":
    unittest.main()
