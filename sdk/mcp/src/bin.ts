#!/usr/bin/env node

/*
 * SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
 * SPDX-License-Identifier: Apache-2.0
 */

import { serveStdio } from "@modelcontextprotocol/server/stdio";
import { Client } from "ttcx";
import { createServer } from "./server.js";

// rootArgument returns the root of the ttcd to serve, which --root names.
function rootArgument(args: string[]): string | undefined {
  for (let i = 0; i < args.length; i++) {
    if (args[i] === "--root") return args[i + 1];
    if (args[i].startsWith("--root=")) return args[i].slice("--root=".length);
  }
  return undefined;
}

const root = rootArgument(process.argv.slice(2));
if (!root) {
  process.stderr.write("usage: ttcx-mcp --root PATH, the TTC_ROOT (or TTC_LOCAL_ROOT) of the ttcd to use\n");
  process.exit(2);
}
const client = new Client(root);
void serveStdio(() => createServer(client));
