#!/usr/bin/env node

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
