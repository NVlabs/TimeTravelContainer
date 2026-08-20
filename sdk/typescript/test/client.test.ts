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

import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { createServer, type Socket } from "node:net";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";
import test from "node:test";

import {
  API_VERSION,
  Client,
  CommandFailed,
  StreamCancelled,
  TTCError,
  defaultSocketPath,
} from "../src/index.js";

function frame(kind: string, payload: Buffer = Buffer.alloc(0)): Buffer {
  const result = Buffer.alloc(5 + payload.length);
  result.write(kind, 0, "ascii");
  result.writeUInt32BE(payload.length, 1);
  payload.copy(result, 5);
  return result;
}

async function withServer(
  handler: (socket: Socket, request: Record<string, unknown>, rest: Buffer) => void,
  run: (path: string) => Promise<void>,
): Promise<void> {
  const directory = await mkdtemp(join(tmpdir(), "ttcx-ts-"));
  const path = join(directory, "ttcd.sock");
  const server = createServer((socket) => {
    let header = Buffer.alloc(0);
    const receiveHeader = (chunk: Buffer): void => {
      header = Buffer.concat([header, chunk]);
      const newline = header.indexOf(10);
      if (newline < 0) return;
      socket.off("data", receiveHeader);
      const request = JSON.parse(header.subarray(0, newline).toString()) as Record<string, unknown>;
      handler(socket, request, header.subarray(newline + 1));
    };
    socket.on("data", receiveHeader);
  });
  await new Promise<void>((resolve, reject) => {
    server.once("error", reject);
    server.listen(path, resolve);
  });
  try {
    await run(path);
  } finally {
    await new Promise<void>((resolve) => server.close(() => resolve()));
    await rm(directory, { recursive: true, force: true });
  }
}

test("ping uses the direct protocol", async () => {
  await withServer(
    (socket, request) => {
      assert.deepEqual(request, { version: API_VERSION, action: "ping" });
      socket.write(frame("O", Buffer.from('{"version":4}\n')));
      socket.end(frame("X", Buffer.from([0, 0, 0, 0])));
    },
    async (path) => {
      assert.equal((await new Client(path).ping()).version, API_VERSION);
    },
  );
});

test("command streams flow-controlled stdin and preserves status", async () => {
  const input = Buffer.alloc((1 << 20) + 23, 120);
  await withServer(
    (socket, request, rest) => {
      assert.deepEqual(request.args, ["exec", "demo@main", "cat"]);
      assert.equal(request.stdin, true);
      assert.equal((request.request_id as string).length, 32);
      let receive = rest;
      const body: Buffer[] = [];
      const consume = (chunk?: Buffer): void => {
        if (chunk) receive = Buffer.concat([receive, chunk]);
        while (receive.length >= 5) {
          const size = receive.readUInt32BE(1);
          if (receive.length < 5 + size) return;
          const kind = String.fromCharCode(receive[0]);
          const payload = receive.subarray(5, 5 + size);
          receive = receive.subarray(5 + size);
          if (kind === "I") {
            body.push(Buffer.from(payload));
            const ack = Buffer.alloc(4);
            ack.writeUInt32BE(payload.length);
            socket.write(frame("A", ack));
          } else {
            assert.equal(kind, "C");
            assert.deepEqual(Buffer.concat(body), input);
            socket.write(frame("E", Buffer.from("failed\n")));
            const status = Buffer.alloc(4);
            status.writeUInt32BE(9);
            socket.end(frame("X", status));
          }
        }
      };
      socket.on("data", consume);
      consume();
    },
    async (path) => {
      const result = await new Client(path).command(["exec", "demo@main", "cat"], { input });
      assert.equal(result.status, 9);
      assert.equal(result.stderrText(), "failed\n");
      assert.throws(() => result.check(), CommandFailed);
    },
  );
});

test("default socket environment precedence", () => {
  assert.equal(
    defaultSocketPath({ TTC_RUNTIME_DIR: "/run/a", XDG_RUNTIME_DIR: "/run/b" }),
    "/run/a/ttcd.sock",
  );
  assert.equal(defaultSocketPath({ XDG_RUNTIME_DIR: "/run/b" }), "/run/b/ttc/ttcd.sock");
});

test("terminal observation frames preserve records", async () => {
  await withServer(
    (socket) => {
      socket.write(frame("S", Buffer.from('{"version":1,"output_bytes":4}')));
      socket.write(frame("S", Buffer.from('{"version":1,"output_bytes":9}')));
      socket.end(frame("X", Buffer.from([0, 0, 0, 0])));
    },
    async (path) => {
      const result = await new Client(path).command([
        "tty", "watch", "demo@main", "--session", "repl",
      ]);
      assert.equal(
        result.stdoutText(),
        '{"version":1,"output_bytes":4}\n{"version":1,"output_bytes":9}\n',
      );
    },
  );
});

test("batch remote fork uses the current CLI shape", async () => {
  await withServer(
    (socket, request) => {
      assert.deepEqual(request.args, [
        "fork",
        "--paused",
        "--target",
        "node-b",
        "demo@main",
        "remote@one",
        "remote@two",
      ]);
      socket.end(frame("X", Buffer.alloc(4)));
    },
    async (path) => {
      const result = await new Client(path).forkMany(
        "demo@main",
        ["remote@one", "remote@two"],
        { paused: true, targetNode: "node-b" },
      );
      assert.equal(result.ok, true);
    },
  );
});

test("elastic cluster helpers use the current CLI shape", async () => {
  await withServer(
    (socket, request) => {
      assert.deepEqual(request.args, ["migrate", "--target", "node-b", "demo@main"]);
      socket.end(frame("X", Buffer.alloc(4)));
    },
    async (path) => {
      assert.equal((await new Client(path).migrate("demo@main", "node-b")).ok, true);
    },
  );
  await withServer(
    (socket, request) => {
      assert.deepEqual(request.args, ["node", "ls", "--json"]);
      socket.write(frame("O", Buffer.from('[{"node_id":"node-a","state":"ready"}]\n')));
      socket.end(frame("X", Buffer.alloc(4)));
    },
    async (path) => {
      const nodes = await new Client(path).nodes<Array<{ node_id: string }>>();
      assert.equal(nodes[0]?.node_id, "node-a");
    },
  );
});

test("image helpers use Docker-compatible command shapes", async () => {
  assert.throws(() => new Client("/unused").load(), /requires input or archive/);

  await withServer(
    (socket, request) => {
      assert.deepEqual(request.args, [
        "commit",
        "--author", "TTC Harness",
        "--message", "prepared",
        "--change", "LABEL timetravel.usage=review",
        "--pause=false",
        "demo@main",
        "local/result:latest",
      ]);
      socket.end(frame("X", Buffer.alloc(4)));
    },
    async (path) => {
      const result = await new Client(path).commit("demo@main", "local/result:latest", {
        author: "TTC Harness",
        message: "prepared",
        changes: ["LABEL timetravel.usage=review"],
        pause: false,
      });
      assert.equal(result.ok, true);
    },
  );

  const archive = Buffer.from("image archive");
  await withServer(
    (socket, request, rest) => {
      assert.deepEqual(request.args, ["load", "--platform", "linux/amd64", "--quiet"]);
      assert.equal(request.stdin, true);
      let receive = rest;
      const body: Buffer[] = [];
      const consume = (chunk?: Buffer): void => {
        if (chunk) receive = Buffer.concat([receive, chunk]);
        while (receive.length >= 5) {
          const size = receive.readUInt32BE(1);
          if (receive.length < 5 + size) return;
          const kind = String.fromCharCode(receive[0]);
          const payload = receive.subarray(5, 5 + size);
          receive = receive.subarray(5 + size);
          if (kind === "I") {
            body.push(Buffer.from(payload));
            const ack = Buffer.alloc(4);
            ack.writeUInt32BE(payload.length);
            socket.write(frame("A", ack));
          } else {
            assert.equal(kind, "C");
            assert.deepEqual(Buffer.concat(body), archive);
            socket.end(frame("X", Buffer.alloc(4)));
          }
        }
      };
      socket.on("data", consume);
      consume();
    },
    async (path) => {
      const result = await new Client(path).load({
        archive,
        platform: "linux/amd64",
        quiet: true,
      });
      assert.equal(result.ok, true);
    },
  );

  await withServer(
    (socket, request) => {
      assert.deepEqual(request.args, [
        "save", "--output", "/srv/images.tar", "one:latest", "two:test",
      ]);
      socket.end(frame("X", Buffer.alloc(4)));
    },
    async (path) => {
      const result = await new Client(path).save(["one:latest", "two:test"], {
        output: "/srv/images.tar",
      });
      assert.equal(result.ok, true);
    },
  );
});

test("checkpoint normalizes the daemon-host path", async () => {
  await withServer(
    (socket, request) => {
      assert.deepEqual((request.args as string[]).slice(0, 4), [
        "checkpoint",
        "--compression",
        "none",
        "demo@main",
      ]);
      assert.equal((request.args as string[])[4], resolve("state.ttc"));
      socket.end(frame("X", Buffer.alloc(4)));
    },
    async (path) => {
      const result = await new Client(path).checkpoint("demo@main", "state.ttc", "none");
      assert.equal(result.ok, true);
    },
  );
});

test("stream delivers observations as whole objects and reports status", async () => {
  await withServer(
    (socket, request) => {
      assert.deepEqual(request.args, ["tty", "watch", "demo@main", "--session", "repl"]);
      assert.equal(request.stdin, false);
      socket.write(frame("S", Buffer.from('{"version":1,"output_bytes":1}')));
      socket.write(frame("S", Buffer.from('{"version":1,"output_bytes":2}')));
      socket.end(frame("X", Buffer.from([0, 0, 0, 0])));
    },
    async (path) => {
      const seen: number[] = [];
      const stream = new Client(path).watchTty("demo@main", "repl", {
        onObservation: (payload) => seen.push(JSON.parse(payload.toString()).output_bytes),
      });
      assert.equal(await stream.exited, 0);
      // Each S frame is one whole JSON object, so no reassembly is required.
      assert.deepEqual(seen, [1, 2]);
    },
  );
});

test("attach announces a terminal size before sending stdin", async () => {
  await withServer(
    (socket, request, rest) => {
      assert.deepEqual(request.args, ["attach", "--interactive", "--exec", "repl", "demo@main"]);
      assert.equal(request.stdin, true);
      let receive = rest;
      const frames: Array<{ kind: string; payload: Buffer }> = [];
      const consume = (chunk?: Buffer): void => {
        if (chunk) receive = Buffer.concat([receive, chunk]);
        while (receive.length >= 5) {
          const size = receive.readUInt32BE(1);
          if (receive.length < 5 + size) return;
          frames.push({
            kind: String.fromCharCode(receive[0]),
            payload: Buffer.from(receive.subarray(5, 5 + size)),
          });
          receive = receive.subarray(5 + size);
          if (frames.length === 2) {
            // The size must precede stdin: ttcd will not start the attach
            // without it, and a non-interactive caller never resizes.
            assert.equal(frames[0].kind, "W");
            assert.equal(frames[0].payload.length, 8);
            assert.equal(frames[0].payload.readUInt32BE(0), 40);
            assert.equal(frames[0].payload.readUInt32BE(4), 120);
            assert.equal(frames[1].kind, "I");
            assert.equal(frames[1].payload.toString(), "echo hi\n");
            socket.end(frame("X", Buffer.from([0, 0, 0, 0])));
          }
        }
      };
      socket.on("data", consume);
      consume();
    },
    async (path) => {
      const stream = new Client(path).attach("demo@main", {
        exec: "repl",
        size: { rows: 40, cols: 120 },
      });
      stream.write("echo hi\n");
      assert.equal(await stream.exited, 0);
    },
  );
});

test("stream never closes stdin implicitly", async () => {
  await withServer(
    (socket, request, rest) => {
      assert.equal(request.stdin, true);
      let receive = rest;
      const kinds: string[] = [];
      const consume = (chunk?: Buffer): void => {
        if (chunk) receive = Buffer.concat([receive, chunk]);
        while (receive.length >= 5) {
          const size = receive.readUInt32BE(1);
          if (receive.length < 5 + size) return;
          kinds.push(String.fromCharCode(receive[0]));
          receive = receive.subarray(5 + size);
        }
      };
      socket.on("data", consume);
      consume();
      // Sending EOF would kill an interactive shell, so a stream that merely
      // wrote input must not have produced a C frame.
      setTimeout(() => {
        assert.ok(!kinds.includes("C"), `unexpected EOF frame in ${kinds.join(",")}`);
        socket.end(frame("X", Buffer.from([0, 0, 0, 0])));
      }, 60);
    },
    async (path) => {
      const stream = new Client(path).stream(["attach", "demo@main"], { stdin: true });
      stream.write("still open\n");
      assert.equal(await stream.exited, 0);
    },
  );
});

test("an immediate resize remains behind the request header", async () => {
  await withServer(
    (socket, request, rest) => {
      assert.deepEqual(request.args, ["attach", "demo@main"]);
      let receive = rest;
      const consume = (chunk?: Buffer): void => {
        if (chunk) receive = Buffer.concat([receive, chunk]);
        if (receive.length < 13) return;
        assert.equal(String.fromCharCode(receive[0]!), "W");
        assert.equal(receive.readUInt32BE(1), 8);
        assert.equal(receive.readUInt32BE(5), 55);
        assert.equal(receive.readUInt32BE(9), 132);
        socket.end(frame("X", Buffer.alloc(4)));
      };
      socket.on("data", consume);
      consume();
    },
    async (path) => {
      const stream = new Client(path).stream(["attach", "demo@main"]);
      stream.resize(55, 132);
      assert.equal(await stream.exited, 0);
    },
  );
});

test("callback failures reject exited instead of escaping EventEmitter", async () => {
  await withServer(
    (socket) => socket.write(frame("O", Buffer.from("output"))),
    async (path) => {
      const expected = new Error("handler failed");
      const stream = new Client(path).stream(["logs", "--follow", "demo@main"], {
        onStdout: () => {
          throw expected;
        },
      });
      await assert.rejects(stream.exited, (error) => error === expected);
    },
  );
});

test("local close is cancellation and closeStdin rejects later writes", async () => {
  await withServer(
    () => {},
    async (path) => {
      const stream = new Client(path).stream(["attach", "demo@main"], { stdin: true });
      stream.closeStdin();
      assert.throws(() => stream.write("late"), TTCError);
      stream.close();
      await assert.rejects(stream.exited, StreamCancelled);
    },
  );
});
