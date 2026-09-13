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
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";
import test from "node:test";

import { Client as McpClient } from "@modelcontextprotocol/client";
import { StdioClientTransport } from "@modelcontextprotocol/client/stdio";

function frame(kind: string, payload: Buffer = Buffer.alloc(0)): Buffer {
  const result = Buffer.alloc(5 + payload.length);
  result.write(kind, 0, "ascii");
  result.writeUInt32BE(payload.length, 1);
  payload.copy(result, 5);
  return result;
}

function mockDaemon(path: string) {
  const requests: Record<string, unknown>[] = [];
  const server = createServer((socket: Socket) => {
    let header = Buffer.alloc(0);
    socket.on("data", (chunk: Buffer) => {
      header = Buffer.concat([header, chunk]);
      const newline = header.indexOf(10);
      if (newline < 0) return;
      const request = JSON.parse(header.subarray(0, newline).toString()) as Record<
        string,
        unknown
      >;
      requests.push(request);
      const args = request.args as string[] | undefined;
      const output = request.action === "ping"
        ? '{"version":8,"isolation_mode":"rootless"}\n'
        : args?.join(" ") === "ls --json"
          ? '[{"id":"demo","branches":[{"id":"main"},{"id":"child","parent":"main"}]}]\n'
          : `ran:${args?.join(" ")}\n`;
      socket.write(frame("O", Buffer.from(output)));
      socket.end(frame("X", Buffer.alloc(4)));
    });
  });
  return { server, requests };
}

test("MCP client lists tools and reaches ttcd directly", async () => {
  const runtime = await mkdtemp(join(tmpdir(), "ttcx-mcp-"));
  const path = join(runtime, "ttcd.sock");
  const mock = mockDaemon(path);
  await new Promise<void>((resolveListen, reject) => {
    mock.server.once("error", reject);
    mock.server.listen(path, resolveListen);
  });

  const transport = new StdioClientTransport({
    command: process.execPath,
    args: [resolve("dist/src/bin.js")],
    env: { ...process.env, TTC_SOCKET: path } as Record<string, string>,
    stderr: "pipe",
  });
  const client = new McpClient({ name: "ttcx-mcp-test", version: "0.1.0" });
  try {
    await client.connect(transport);
    const listed = await client.listTools();
    assert(listed.tools.some((tool) => tool.name === "ttc_fork"));
    assert(listed.tools.some((tool) => tool.name === "ttc_fork_many"));
    assert(listed.tools.some((tool) => tool.name === "ttc_migrate"));
    assert(listed.tools.some((tool) => tool.name === "ttc_nodes"));
    assert(listed.tools.some((tool) => tool.name === "ttc_stop_node"));
    assert(listed.tools.some((tool) => tool.name === "ttc_rebalance_node"));
    assert(listed.tools.some((tool) => tool.name === "ttc_checkpoint"));
    assert(listed.tools.some((tool) => tool.name === "ttc_restore"));
    assert(listed.tools.some((tool) => tool.name === "ttc_image_load"));
    assert(listed.tools.some((tool) => tool.name === "ttc_image_save"));
    assert(listed.tools.some((tool) => tool.name === "ttc_command"));

    const status = await client.callTool({ name: "ttc_status", arguments: {} });
    assert.equal(status.isError, undefined);
    assert.match((status.content[0] as { text: string }).text, /"version": 8/);

    const families = await client.callTool({
      name: "ttc_list",
      arguments: {},
    });
    assert.match((families.content[0] as { text: string }).text, /"parent": "main"/);
    assert.deepEqual(mock.requests[1].args, ["ls", "--json"]);
    assert.equal(mock.requests[1].options, undefined);

    await client.callTool({
      name: "ttc_fork_many",
      arguments: {
        source: "demo@main",
        destinations: ["remote@one", "remote@two"],
        paused: true,
        node: "node-b",
      },
    });
    assert.deepEqual(mock.requests[2].args, [
      "fork",
      "--paused",
      "--node",
      "node-b",
      "demo@main",
      "remote@one",
      "remote@two",
    ]);

    await client.callTool({
      name: "ttc_checkpoint",
      arguments: {
        target: "demo@main",
        file: "/tmp/demo.ttc",
        compression: "none",
      },
    });
    assert.deepEqual(mock.requests[3].args, [
      "checkpoint",
      "--compression",
      "none",
      "demo@main",
      "/tmp/demo.ttc",
    ]);

    await client.callTool({
      name: "ttc_stop",
      arguments: { target: "demo@main", timeoutSeconds: 12 },
    });
    assert.deepEqual(mock.requests[4].args, [
      "stop",
      "--time",
      "12",
      "demo@main",
    ]);

    await client.callTool({
      name: "ttc_run",
      arguments: {
        image: "alpine:3.20",
        name: "hosted",
        node: "node-b",
        options: ["--network=host"],
        waiveLiveFork: true,
      },
    });
    assert.deepEqual(mock.requests[5].args, [
      "run",
      "--node",
      "node-b",
      "--network=host",
      "-d",
      "--name",
      "hosted",
      "--waive-live-fork",
      "alpine:3.20",
    ]);

    await client.callTool({
      name: "ttc_image_load",
      arguments: {
        input: "/srv/images.tar.gz",
        platform: "linux/amd64",
        quiet: true,
      },
    });
    assert.deepEqual(mock.requests[6].args, [
      "load",
      "--input",
      "/srv/images.tar.gz",
      "--platform",
      "linux/amd64",
      "--quiet",
    ]);

    await client.callTool({
      name: "ttc_image_save",
      arguments: {
        images: ["one:latest", "two:test"],
        output: "/srv/images.tar",
      },
    });
    assert.deepEqual(mock.requests[7].args, [
      "save",
      "--output",
      "/srv/images.tar",
      "one:latest",
      "two:test",
    ]);

    await client.callTool({
      name: "ttc_wait",
      arguments: { target: "demo@main", condition: "durable", timeoutMs: 60_000 },
    });
    assert.deepEqual(mock.requests[8].args, [
      "wait",
      "--condition",
      "durable",
      "demo@main",
    ]);
  } finally {
    await client.close();
    await new Promise<void>((resolveClose) => mock.server.close(() => resolveClose()));
    await rm(runtime, { recursive: true, force: true });
  }
});
