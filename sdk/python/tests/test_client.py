# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

import json
import socket
import struct
import threading
import unittest
from pathlib import Path

from ttcx import Client, CommandFailed, StreamCancelled, TTCError
from ttcx.client import API_VERSION, socket_path, uses_terminal_control


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
            conn.sendall(frame(b"O", b'{"version":%d,"isolation_mode":"rootless"}\n' % API_VERSION))
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                self.assertEqual(Client(directory).ping()["version"], API_VERSION)
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
                result = Client(directory).command(["exec", "demo@main", "cat"], input=payload)
                self.assertEqual(result.status, 7)
                self.assertEqual(result.stdout, b"partial output")
                with self.assertRaises(CommandFailed):
                    result.check()
            finally:
                daemon.close()

    def test_api_version_is_the_daemons(self):
        # ttcd refuses any other: runsc/ttc/protocol/protocol.go.
        protocol = Path(__file__).resolve().parents[3] / "runsc/ttc/protocol/protocol.go"
        if not protocol.exists():
            self.skipTest("not in a TTC checkout")
        self.assertIn(f"const DaemonAPIVersion = {API_VERSION}\n", protocol.read_text())

    def test_exec_input_is_interactive(self):
        def handler(conn, request):
            self.assertEqual(request["args"], ["exec", "--interactive", "demo@main", "cat"])
            self.assertIs(request["stdin"], True)
            while read_exact(conn, 1) != b"C":
                data = read_exact(conn, struct.unpack(">I", read_exact(conn, 4))[0])
                conn.sendall(frame(b"A", struct.pack(">I", len(data))))
            read_exact(conn, 4)
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                self.assertTrue(Client(directory).exec("demo@main", ["cat"], input="hi").ok)
            finally:
                daemon.close()

    def test_tty_session_answers_the_terminal_request(self):
        def handler(conn, request):
            self.assertEqual(
                request["args"],
                ["exec", "--session", "repl", "--detach", "--tty", "demo@main", "sh"],
            )
            self.assertIs(request["terminal_control"], True)
            # ttcd asks for the terminal's size, and waits for it.
            conn.sendall(frame(b"T", b"\x00"))
            kind = read_exact(conn, 1)
            size = struct.unpack(">I", read_exact(conn, 4))[0]
            self.assertEqual((kind, size), (b"W", 8))
            self.assertEqual(struct.unpack(">II", read_exact(conn, size)), (0, 0))
            conn.sendall(frame(b"T", b"\x01"))
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                result = Client(directory).exec_session("demo@main", ["sh"], session="repl", tty=True)
                self.assertTrue(result.ok)
            finally:
                daemon.close()

    def test_terminal_control_follows_the_cli(self):
        for args, want in [
            (["attach", "demo@main"], True),
            (["start", "-a", "demo@main"], True),
            (["start", "demo@main"], False),
            (["run", "-dit", "ubuntu"], True),
            (["exec", "--tty=false", "demo@main", "sh"], False),
            (["exec", "demo@main", "--", "ls", "-t"], False),
            (["exec", "ls", "demo@main"], False),
            (["container", "exec", "-t", "demo@main", "sh"], True),
            (["ps"], False),
        ]:
            self.assertIs(uses_terminal_control(args), want, args)

    def test_socket_is_in_the_daemon_root(self):
        self.assertEqual(Client("/data/ttc").socket_path, "/data/ttc/ttcd.sock")
        self.assertEqual(socket_path("/local/ttc"), "/local/ttc/ttcd.sock")

    def test_terminal_observation_frames_preserve_records(self):
        def handler(conn, _request):
            conn.sendall(frame(b"S", b'{"version":1,"output_bytes":4}'))
            conn.sendall(frame(b"S", b'{"version":1,"output_bytes":9}'))
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                result = Client(directory).command(["tty", "watch", "demo@main", "--session", "repl"])
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
                    "--node",
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
                result = Client(directory).fork_many(
                    "demo@main",
                    ["remote@one", "remote@two"],
                    paused=True,
                    node="node-b",
                )
                self.assertTrue(result.ok)
            finally:
                daemon.close()

    def test_elastic_cluster_helpers_use_current_cli_shape(self):
        def migrate_handler(conn, request):
            self.assertEqual(
                request["args"],
                ["migrate", "--node", "node-b", "demo@main"],
            )
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", migrate_handler)
            try:
                self.assertTrue(Client(directory).migrate("demo@main", "node-b").ok)
            finally:
                daemon.close()

        def nodes_handler(conn, request):
            self.assertEqual(request["args"], ["node", "ls", "--json"])
            conn.sendall(frame(b"O", b'[{"node_id":"node-a","state":"ready"}]\n'))
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", nodes_handler)
            try:
                self.assertEqual(Client(directory).nodes()[0]["node_id"], "node-a")
            finally:
                daemon.close()

        def stop_handler(conn, request):
            self.assertEqual(
                request["args"],
                ["node", "stop", "--migrate-all", "node-a"],
            )
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", stop_handler)
            try:
                self.assertTrue(
                    Client(directory).stop_node("node-a", policy="migrate").ok
                )
            finally:
                daemon.close()

    def test_rebalance_lets_the_scheduler_choose(self):
        def handler(conn, request):
            self.assertEqual(request["args"], ["node", "rebalance", "node-a"])
            conn.sendall(frame(b"O", b'{"moves":[]}\n'))
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                self.assertEqual(Client(directory).rebalance_node("node-a"), {"moves": []})
            finally:
                daemon.close()

    def test_wait_durable_uses_current_cli_shape(self):
        def handler(conn, request):
            self.assertEqual(
                request["args"],
                ["wait", "--condition", "durable", "demo@main"],
            )
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                self.assertTrue(
                    Client(directory).wait("demo@main", condition="durable").ok
                )
                with self.assertRaisesRegex(ValueError, "condition"):
                    Client(directory).wait("demo@main", condition="invalid")
            finally:
                daemon.close()

    def test_image_helpers_use_docker_compatible_command_shapes(self):
        with self.assertRaisesRegex(ValueError, "requires input or archive"):
            Client("/unused").load()

        def commit_handler(conn, request):
            self.assertEqual(request["args"], [
                "commit",
                "--author", "TTC Harness",
                "--message", "prepared",
                "--change", "LABEL timetravel.usage=review",
                "--pause=false",
                "demo@main",
                "local/result:latest",
            ])
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", commit_handler)
            try:
                result = Client(directory).commit(
                    "demo@main",
                    "local/result:latest",
                    author="TTC Harness",
                    message="prepared",
                    changes=["LABEL timetravel.usage=review"],
                    pause=False,
                )
                self.assertTrue(result.ok)
            finally:
                daemon.close()

        archive = b"image archive"

        def load_handler(conn, request):
            self.assertEqual(
                request["args"],
                ["load", "--platform", "linux/amd64", "--quiet"],
            )
            self.assertIs(request["stdin"], True)
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
            self.assertEqual(bytes(received), archive)
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", load_handler)
            try:
                result = Client(directory).load(
                    archive=archive, platform="linux/amd64", quiet=True
                )
                self.assertTrue(result.ok)
            finally:
                daemon.close()

        def save_handler(conn, request):
            self.assertEqual(request["args"], [
                "save", "--output", "/srv/images.tar", "one:latest", "two:test",
            ])
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", save_handler)
            try:
                result = Client(directory).save(
                    ["one:latest", "two:test"], output="/srv/images.tar"
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
                    Client(directory)
                    .checkpoint("demo@main", "state.ttc", compression="none")
                    .ok
                )
            finally:
                daemon.close()

    def test_stream_delivers_observations_and_status(self):
        def handler(conn, request):
            self.assertEqual(
                request["args"],
                ["tty", "watch", "demo@main", "--session", "repl"],
            )
            conn.sendall(frame(b"S", b'{"output_bytes":1}'))
            conn.sendall(frame(b"S", b'{"output_bytes":2}'))
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                seen = []
                stream = Client(directory).watch_tty(
                    "demo@main", "repl", lambda payload: seen.append(json.loads(payload))
                )
                self.assertEqual(stream.wait(), 0)
                self.assertEqual([item["output_bytes"] for item in seen], [1, 2])
            finally:
                daemon.close()

    def test_stream_without_a_size_answers_the_terminal_request(self):
        def handler(conn, request):
            self.assertIs(request["terminal_control"], True)
            conn.sendall(frame(b"T", b"\x00"))
            kind = read_exact(conn, 1)
            size = struct.unpack(">I", read_exact(conn, 4))[0]
            self.assertEqual((kind, size), (b"W", 8))
            self.assertEqual(struct.unpack(">II", read_exact(conn, size)), (0, 0))
            conn.sendall(frame(b"O", b"$ "))
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                output = []
                stream = Client(directory).stream(
                    ["exec", "-t", "demo@main", "sh"], on_stdout=output.append
                )
                self.assertEqual(stream.wait(5), 0)
                self.assertEqual(output, [b"$ "])
            finally:
                daemon.close()

    def test_attach_sends_size_before_stdin(self):
        def handler(conn, request):
            self.assertEqual(
                request["args"],
                ["attach", "--interactive", "--exec", "repl", "demo@main"],
            )
            kind = read_exact(conn, 1)
            size = struct.unpack(">I", read_exact(conn, 4))[0]
            self.assertEqual((kind, size), (b"W", 8))
            self.assertEqual(struct.unpack(">II", read_exact(conn, size)), (40, 120))
            kind = read_exact(conn, 1)
            size = struct.unpack(">I", read_exact(conn, 4))[0]
            self.assertEqual((kind, read_exact(conn, size)), (b"I", b"echo hi\n"))
            conn.sendall(frame(b"A", struct.pack(">I", size)))
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                stream = Client(directory).attach(
                    "demo@main", exec_session="repl", size=(40, 120)
                )
                stream.write("echo hi\n")
                self.assertEqual(stream.wait(), 0)
            finally:
                daemon.close()

    def test_stream_flow_controls_more_than_one_window(self):
        payload = b"z" * ((1 << 20) + 31)

        def handler(conn, request):
            self.assertIs(request["stdin"], True)
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
            conn.sendall(frame(b"X", struct.pack(">I", 0)))

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                stream = Client(directory).stream(
                    ["exec", "demo@main", "cat"], stdin=True
                )
                stream.write(payload)
                stream.close_stdin()
                self.assertEqual(stream.wait(), 0)
            finally:
                daemon.close()

    def test_callback_failure_and_local_cancel_are_not_success(self):
        callback_error = RuntimeError("handler failed")

        def handler(conn, _request):
            conn.sendall(frame(b"O", b"output"))
            try:
                conn.recv(1)
            except ConnectionError:
                pass

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", handler)
            try:
                stream = Client(directory).stream(
                    ["logs", "--follow", "demo@main"],
                    on_stdout=lambda _payload: (_ for _ in ()).throw(callback_error),
                )
                with self.assertRaisesRegex(RuntimeError, "handler failed"):
                    stream.wait()
            finally:
                daemon.close()

        def idle(conn, _request):
            conn.recv(1)

        with __import__("tempfile").TemporaryDirectory() as directory:
            daemon = MockDaemon(Path(directory) / "ttcd.sock", idle)
            try:
                stream = Client(directory).stream(
                    ["attach", "demo@main"], stdin=True
                )
                stream.close_stdin()
                with self.assertRaises(TTCError):
                    stream.write("late")
                stream.close()
                with self.assertRaises(StreamCancelled):
                    stream.wait()
            finally:
                daemon.close()


if __name__ == "__main__":
    unittest.main()
