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

import { McpServer } from "@modelcontextprotocol/server";
import * as z from "zod/v4";
import { Client, type Result } from "ttcx";

type ToolResponse = {
  content: Array<{ type: "text"; text: string }>;
  isError?: boolean;
};

function commandResponse(result: Result): ToolResponse {
  const parts: string[] = [];
  if (result.stdout.length) parts.push(result.stdoutText());
  if (result.stderr.length) parts.push(result.stderrText());
  if (!parts.length) parts.push(`TTC command completed with status ${result.status}.`);
  return {
    content: [{ type: "text", text: parts.join("") }],
    ...(result.ok ? {} : { isError: true }),
  };
}

function jsonResponse(value: unknown): ToolResponse {
  return {
    content: [{ type: "text", text: JSON.stringify(value, null, 2) }],
  };
}

export function createServer(client: Client): McpServer {
  const server = new McpServer(
    { name: "ttcx-mcp", version: "0.1.0" },
    {
      instructions:
        "Manage TimeTravelContainer through ttcd. Tools are non-interactive; use ttc directly for a TTY.",
    },
  );

  server.registerTool(
    "ttc_status",
    {
      description: "Show ttcd status and capabilities.",
      inputSchema: {},
      annotations: { readOnlyHint: true, idempotentHint: true },
    },
    async () => jsonResponse(await client.ping()),
  );

  server.registerTool(
    "ttc_list",
    {
      description: "List TTC container families and their live-fork branches.",
      inputSchema: {},
      annotations: { readOnlyHint: true, idempotentHint: true },
    },
    async () => jsonResponse(await client.list()),
  );

  server.registerTool(
    "ttc_inspect",
    {
      description: "Inspect a TTC container family, including all branch details.",
      inputSchema: { container: z.string().describe("Container family name or ID") },
      annotations: { readOnlyHint: true, idempotentHint: true },
    },
    async ({ container }) => jsonResponse(await client.inspect(container)),
  );

  server.registerTool(
    "ttc_pull",
    {
      description: "Pull an OCI image into TTC.",
      inputSchema: { image: z.string() },
      annotations: { idempotentHint: true, openWorldHint: true },
    },
    async ({ image }) => commandResponse(await client.pull(image)),
  );

  server.registerTool(
    "ttc_image_load",
    {
      description: "Load a Docker-save or OCI image archive from a path visible to ttcd.",
      inputSchema: {
        input: z.string().describe("Archive path on the ttcd host"),
        platform: z.string().optional(),
        quiet: z.boolean().default(false),
      },
      annotations: { destructiveHint: true },
    },
    async ({ input, platform, quiet }) =>
      commandResponse(await client.load({ input, platform, quiet })),
  );

  server.registerTool(
    "ttc_image_save",
    {
      description: "Save one or more TTC images to an archive path visible to ttcd.",
      inputSchema: {
        images: z.array(z.string()).min(1),
        output: z.string().describe("Archive path on the ttcd host"),
        platform: z.string().optional(),
      },
      annotations: { destructiveHint: true },
    },
    async ({ images, output, platform }) =>
      commandResponse(await client.save(images, { output, platform })),
  );

  server.registerTool(
    "ttc_run",
    {
      description: "Start a non-interactive container from an OCI image.",
      inputSchema: {
        image: z.string(),
        name: z.string().optional(),
        node: z.string().optional().describe("Preferred cluster node; omit for scheduler placement"),
        command: z.array(z.string()).default([]),
        detach: z.boolean().default(true),
        waiveLiveFork: z
          .boolean()
          .default(false)
          .describe(
            "Explicitly create a permanently non-forkable family; required by incompatible options such as host networking",
          ),
        options: z
          .array(z.string())
          .default([])
          .describe("Additional ttc run options placed before the image"),
      },
      annotations: { destructiveHint: true, openWorldHint: true },
    },
    async ({ image, name, node, command, detach, waiveLiveFork, options }) => {
      const runOptions = [...options];
      if (detach) runOptions.push("-d");
      if (name) runOptions.push("--name", name);
      if (waiveLiveFork) runOptions.push("--waive-live-fork");
      return commandResponse(await client.run(image, command, runOptions, node));
    },
  );

  server.registerTool(
    "ttc_exec",
    {
      description: "Run a non-interactive command in an existing TTC branch.",
      inputSchema: {
        target: z.string().describe("Full container@branch selector"),
        command: z.array(z.string()).min(1),
        stdin: z.string().optional(),
		session: z.string().optional().describe("Durable detached exec-session name"),
		tty: z.boolean().default(false),
      },
      annotations: { destructiveHint: true, openWorldHint: true },
    },
	async ({ target, command, stdin, session, tty }) => {
		if (session) {
			if (stdin !== undefined) throw new Error("stdin is unavailable when creating a detached exec session");
			return commandResponse(await client.execSession(target, command, session, { tty, interactive: tty }));
		}
		return commandResponse(await client.exec(target, command, { input: stdin }));
	},
  );

  server.registerTool(
	"ttc_commit",
	{
		description: "Commit one branch filesystem as a runnable OCI image using Docker-style image configuration changes.",
		inputSchema: {
			target: z.string(),
			image: z.string(),
			author: z.string().optional(),
			message: z.string().optional(),
			changes: z.array(z.string()).default([]).describe("Dockerfile instructions such as LABEL timetravel.usage=review"),
			pause: z.boolean().default(true),
		},
		annotations: { destructiveHint: true },
	},
	async ({ target, image, author, message, changes, pause }) => commandResponse(await client.commit(target, image, { author, message, changes, pause })),
  );

  server.registerTool(
	"ttc_stats",
	{
		description: "Report per-branch private/shared memory and filesystem usage plus family totals.",
		inputSchema: { target: z.string() },
		annotations: { readOnlyHint: true, idempotentHint: true },
	},
	async ({ target }) => jsonResponse(await client.stats(target)),
  );

  server.registerTool(
	"ttc_diff",
	{
		description: "List paths changed in a branch's CoW filesystem layer.",
		inputSchema: { target: z.string() },
		annotations: { readOnlyHint: true, idempotentHint: true },
	},
	async ({ target }) => jsonResponse(await client.diff(target)),
  );

  server.registerTool(
	"ttc_cp",
	{
		description: "Copy between the host and a TTC branch, including a dehydrated branch.",
		inputSchema: { source: z.string(), destination: z.string() },
		annotations: { destructiveHint: true },
	},
	async ({ source, destination }) => commandResponse(await client.copy(source, destination)),
  );

  server.registerTool(
	"ttc_exec_sessions",
	{
		description: "List named durable exec sessions in a branch.",
		inputSchema: { target: z.string() },
		annotations: { readOnlyHint: true, idempotentHint: true },
	},
	async ({ target }) => jsonResponse(await client.execSessions(target)),
  );

  server.registerTool(
	"ttc_exec_logs",
	{
		description: "Read retained output from a named exec session.",
		inputSchema: { target: z.string(), session: z.string() },
		annotations: { readOnlyHint: true, idempotentHint: true },
	},
	async ({ target, session }) => commandResponse(await client.execLogs(target, session)),
  );

  server.registerTool(
	"ttc_tty_stat",
	{
		description: "Observe a named terminal session without consuming its input or output.",
		inputSchema: { target: z.string(), session: z.string() },
		annotations: { readOnlyHint: true, idempotentHint: true },
	},
	async ({ target, session }) => jsonResponse(await client.ttyStat(target, session)),
  );

  server.registerTool(
    "ttc_quarantine",
    {
      description: "Disconnect a branch from TTC-managed external networking while preserving its internal state.",
      inputSchema: {
        target: z.string().describe("Full container@branch selector"),
      },
      annotations: { destructiveHint: true },
    },
    async ({ target }) => commandResponse(await client.quarantine(target)),
  );

  server.registerTool(
    "ttc_unquarantine",
    {
      description: "Reconnect a quarantined branch to TTC-managed external networking.",
      inputSchema: {
        target: z.string().describe("Full container@branch selector"),
      },
      annotations: { destructiveHint: true },
    },
    async ({ target }) => commandResponse(await client.unquarantine(target)),
  );

  server.registerTool(
    "ttc_fork",
    {
      description: "Live-fork a running TTC container branch.",
      inputSchema: {
        source: z.string().describe("Full source container@branch selector"),
        destination: z
          .string()
          .describe("New branch name in the source family"),
        paused: z.boolean().default(false),
        node: z.string().optional().describe("Configured remote TTC node"),
      },
      annotations: { destructiveHint: true },
    },
	async ({ source, destination, paused, node }) =>
	  commandResponse(
		await client.fork(source, destination, { paused, node }),
      ),
  );

  server.registerTool(
    "ttc_fork_many",
    {
      description: "Fork several sibling branches from one coordinated source pause.",
      inputSchema: {
        source: z.string().describe("Full source container@branch selector"),
        destinations: z.array(z.string()).min(1),
        paused: z.boolean().default(false),
        node: z.string().optional().describe("Configured remote TTC node"),
      },
      annotations: { destructiveHint: true },
    },
	async ({ source, destinations, paused, node }) =>
	  commandResponse(
		await client.forkMany(source, destinations, { paused, node }),
      ),
  );

  server.registerTool(
    "ttc_migrate",
    {
      description: "Move a TTC branch to another elastic-cluster node without changing its identity.",
      inputSchema: {
        target: z.string().describe("Full container@branch selector"),
        node: z.string().describe("Destination TTC node ID"),
      },
      annotations: { destructiveHint: true },
    },
	async ({ target, node }) =>
	  commandResponse(await client.migrate(target, node)),
  );

  server.registerTool(
    "ttc_nodes",
    {
      description: "List elastic TTC nodes, leases, states, and memory load.",
      inputSchema: {},
      annotations: { readOnlyHint: true, idempotentHint: true },
    },
    async () => jsonResponse(await client.nodes()),
  );

  server.registerTool(
    "ttc_stop_node",
    {
      description: "Cordon a node, apply an explicit branch policy, and stop its daemon.",
	  inputSchema: {
		node: z.string(),
		policy: z.enum(["stop", "dehydrate", "migrate"]).optional(),
      },
      annotations: { destructiveHint: true },
    },
	async ({ node, policy }) =>
	  commandResponse(await client.stopNode(node, policy)),
  );

  server.registerTool(
    "ttc_rebalance_node",
    {
      description: "Relieve a node's configured memory pressure using CoW-aware placement.",
	  inputSchema: {
		node: z.string(),
		destinationNode: z.string().optional().describe("Restrict placement to one node"),
      },
      annotations: { destructiveHint: true },
    },
	async ({ node, destinationNode }) =>
	  jsonResponse(await client.rebalanceNode(node, destinationNode)),
  );

  for (const [name, description] of [
    ["pause", "Pause a running TTC branch."],
    ["resume", "Resume a paused TTC branch."],
    ["dehydrate", "Persist a TTC branch's complete live state and release its runtime."],
  ] as const) {
    server.registerTool(
      `ttc_${name}`,
      {
        description,
        inputSchema: { target: z.string() },
        annotations: { destructiveHint: true, idempotentHint: true },
      },
      async ({ target }) => commandResponse(await client[name](target)),
    );
  }

  server.registerTool(
    "ttc_rehydrate",
    {
      description: "Restore a dehydrated TTC branch on a chosen or scheduler-selected node.",
      inputSchema: {
        target: z.string(),
        node: z.string().optional().describe("Preferred cluster node; omit for scheduler placement"),
      },
      annotations: { destructiveHint: true, idempotentHint: true },
    },
    async ({ target, node }) => commandResponse(await client.rehydrate(target, node)),
  );

  server.registerTool(
    "ttc_start",
    {
      description: "Start a created or exited TTC branch.",
      inputSchema: {
        target: z.string().describe("Full container@branch selector"),
        node: z.string().optional().describe("Preferred cluster node; omit for scheduler placement"),
      },
      annotations: { destructiveHint: true },
    },
    async ({ target, node }) => commandResponse(await client.start(target, node)),
  );

  server.registerTool(
    "ttc_stop",
    {
      description: "Stop one TTC branch, or every branch in a family.",
      inputSchema: {
        target: z.string().describe("Container family or full container@branch selector"),
        timeoutSeconds: z.number().int().min(-1).optional(),
      },
      annotations: { destructiveHint: true, idempotentHint: true },
    },
    async ({ target, timeoutSeconds }) =>
      commandResponse(await client.stop(target, timeoutSeconds)),
  );

  server.registerTool(
    "ttc_restart",
    {
      description: "Stop and start a TTC branch with fresh process state.",
      inputSchema: {
        target: z.string().describe("Full container@branch selector"),
        timeoutSeconds: z.number().int().min(-1).optional(),
      },
      annotations: { destructiveHint: true },
    },
    async ({ target, timeoutSeconds }) =>
      commandResponse(await client.restart(target, timeoutSeconds)),
  );

  server.registerTool(
    "ttc_kill",
    {
      description: "Send a signal to a TTC branch's init process.",
      inputSchema: {
        target: z.string().describe("Full container@branch selector"),
        signal: z.union([z.string(), z.number().int()]).optional(),
      },
      annotations: { destructiveHint: true },
    },
    async ({ target, signal }) => commandResponse(await client.kill(target, signal)),
  );

  server.registerTool(
    "ttc_wait",
    {
      description: "Wait for a TTC branch to stop, or for its captured state to become shared-durable.",
      inputSchema: {
        target: z.string().describe("Full container@branch selector"),
        condition: z.enum(["not-running", "durable"]).default("not-running"),
        timeoutMs: z.number().int().positive().optional(),
      },
      annotations: { readOnlyHint: true },
    },
    async ({ target, condition, timeoutMs }) =>
      commandResponse(await client.wait(target, { condition, timeoutMs })),
  );

  server.registerTool(
    "ttc_checkpoint",
    {
      description: "Export one live or paused branch to a checkpoint file on the daemon host.",
      inputSchema: {
        target: z.string().describe("Full container@branch selector"),
        file: z.string().describe("Path on the daemon host"),
        compression: z.enum(["none", "flate-best-speed"]).default("flate-best-speed"),
      },
      annotations: { destructiveHint: true, openWorldHint: true },
    },
    async ({ target, file, compression }) =>
      commandResponse(await client.checkpoint(target, file, compression)),
  );

  server.registerTool(
    "ttc_restore",
    {
      description: "Restore a checkpoint as a new unrelated TTC container family.",
      inputSchema: {
        file: z.string().describe("Checkpoint path on the daemon host"),
        name: z.string().optional(),
        node: z.string().optional().describe("Preferred cluster node; omit for scheduler placement"),
      },
      annotations: { destructiveHint: true, openWorldHint: true },
    },
    async ({ file, name, node }) => commandResponse(await client.restore(file, name, node)),
  );

  server.registerTool(
    "ttc_remove",
    {
      description: "Remove a TTC container or branch.",
      inputSchema: {
        target: z.string(),
        force: z.boolean().default(false),
      },
      annotations: { destructiveHint: true, idempotentHint: true },
    },
    async ({ target, force }) => commandResponse(await client.remove(target, force)),
  );

  server.registerTool(
    "ttc_command",
    {
      description:
        "Run any non-interactive TTC command not covered by another tool. Arguments exclude the ttc executable name.",
      inputSchema: {
        args: z.array(z.string()).min(1),
        stdin: z.string().optional(),
        requestId: z
          .string()
          .optional()
          .describe("Reuse only to retry the exact same mutating request"),
        timeoutMs: z
          .number()
          .int()
          .positive()
          .optional()
          .describe("Override the SDK's 30-second operation timeout"),
      },
      annotations: { destructiveHint: true, openWorldHint: true },
    },
    async ({ args, stdin, requestId, timeoutMs }) =>
      commandResponse(
        await client.command(args, { input: stdin, requestId, timeoutMs }),
      ),
  );

  return server;
}
