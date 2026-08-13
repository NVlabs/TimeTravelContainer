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

import { API_VERSION, Client, CommandFailed, defaultSocketPath } from "../src/index.js";

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
