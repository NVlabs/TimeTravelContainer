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
      const output =
        request.action === "ping"
          ? '{"version":4,"isolation_mode":"rootless"}\n'
          : `ran:${(request.args as string[]).join(" ")}\n`;
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
    env: { ...process.env, TTC_RUNTIME_DIR: runtime } as Record<string, string>,
    stderr: "pipe",
  });
  const client = new McpClient({ name: "ttcx-mcp-test", version: "0.1.0" });
  try {
    await client.connect(transport);
    const listed = await client.listTools();
    assert(listed.tools.some((tool) => tool.name === "ttc_fork"));
    assert(listed.tools.some((tool) => tool.name === "ttc_command"));

    const status = await client.callTool({ name: "ttc_status", arguments: {} });
    assert.equal(status.isError, undefined);
    assert.match((status.content[0] as { text: string }).text, /"version": 4/);

    const command = await client.callTool({
      name: "ttc_command",
      arguments: { args: ["ls", "--json"] },
    });
    assert.match((command.content[0] as { text: string }).text, /ran:ls --json/);
    assert.deepEqual(mock.requests[1].args, ["ls", "--json"]);
    assert.equal(mock.requests[1].options, undefined);
  } finally {
    await client.close();
    await new Promise<void>((resolveClose) => mock.server.close(() => resolveClose()));
    await rm(runtime, { recursive: true, force: true });
  }
});
