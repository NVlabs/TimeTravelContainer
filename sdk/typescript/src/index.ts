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

import { randomBytes } from "node:crypto";
import { join, resolve } from "node:path";
import { tmpdir } from "node:os";
import { createConnection, type Socket } from "node:net";

export const API_VERSION = 4;
const MAX_FRAME = 1 << 20;
const STDIN_WINDOW = 1 << 20;

export class TTCError extends Error {}
export class ProtocolError extends TTCError {}

export class CommandFailed extends TTCError {
  constructor(public readonly result: Result) {
    super(
      `TTC command exited with status ${result.status}: ${result.stderrText().trim()}`,
    );
  }
}

export class Result {
  constructor(
    public readonly status: number,
    public readonly stdout: Buffer,
    public readonly stderr: Buffer,
  ) {}

  get ok(): boolean {
    return this.status === 0;
  }

  stdoutText(encoding: BufferEncoding = "utf8"): string {
    return this.stdout.toString(encoding);
  }

  stderrText(encoding: BufferEncoding = "utf8"): string {
    return this.stderr.toString(encoding);
  }

  json<T = unknown>(): T {
    return JSON.parse(this.stdoutText()) as T;
  }

  check(): this {
    if (!this.ok) {
      throw new CommandFailed(this);
    }
    return this;
  }
}

export interface ClientOptions {
  socketPath?: string;
  timeoutMs?: number;
}

export interface CommandOptions {
  input?: string | Uint8Array;
  requestId?: string;
  timeoutMs?: number;
  check?: boolean;
}

export interface ForkOptions {
  paused?: boolean;
  targetNode?: string;
}

export function defaultSocketPath(
  env: NodeJS.ProcessEnv = process.env,
): string {
  if (env.TTC_RUNTIME_DIR) {
    return join(env.TTC_RUNTIME_DIR, "ttcd.sock");
  }
  if (env.XDG_RUNTIME_DIR) {
    return join(env.XDG_RUNTIME_DIR, "ttc", "ttcd.sock");
  }
  return join(tmpdir(), `ttc-${process.getuid?.() ?? 0}`, "ttcd.sock");
}

function frame(kind: string, payload: Uint8Array = new Uint8Array()): Buffer {
  if (kind.length !== 1 || payload.byteLength > MAX_FRAME) {
    throw new RangeError("invalid TTC frame");
  }
  const result = Buffer.allocUnsafe(5 + payload.byteLength);
  result.write(kind, 0, 1, "ascii");
  result.writeUInt32BE(payload.byteLength, 1);
  Buffer.from(payload).copy(result, 5);
  return result;
}

export class Client {
  readonly socketPath: string;
  readonly timeoutMs: number;

  constructor(options: ClientOptions | string = {}) {
    if (typeof options === "string") {
      this.socketPath = options;
      this.timeoutMs = 30_000;
    } else {
      this.socketPath = options.socketPath ?? defaultSocketPath();
      this.timeoutMs = options.timeoutMs ?? 30_000;
    }
  }

  async ping(options: { timeoutMs?: number } = {}): Promise<Record<string, unknown>> {
    const result = await this.exchange(
      { version: API_VERSION, action: "ping" },
      undefined,
      options.timeoutMs,
    );
    return result.check().json<Record<string, unknown>>();
  }

  async command(args: readonly string[], options: CommandOptions = {}): Promise<Result> {
    if (args.length === 0 || args.some((arg) => typeof arg !== "string")) {
      throw new TypeError("args must be a non-empty array of strings");
    }
    const input =
      typeof options.input === "string"
        ? Buffer.from(options.input)
        : options.input === undefined
          ? undefined
          : Buffer.from(options.input);
    const result = await this.exchange(
      {
        version: API_VERSION,
        action: "command",
        request_id: options.requestId ?? randomBytes(16).toString("hex"),
        args: [...args],
        stdin: options.input !== undefined,
      },
      input,
      options.timeoutMs,
    );
    return options.check ? result.check() : result;
  }

  async list<T = unknown>(): Promise<T> {
    return (await this.command(["ls", "--json"], { check: true })).json<T>();
  }

  async inspect<T = unknown>(target: string): Promise<T> {
    return (await this.command(["inspect", target], { check: true })).json<T>();
  }

  pull(image: string): Promise<Result> {
    return this.command(["pull", image]);
  }

  run(
    image: string,
    command: readonly string[] = [],
    options: readonly string[] = [],
  ): Promise<Result> {
    return this.command(["run", ...options, image, ...command]);
  }

  start(target: string): Promise<Result> {
    return this.command(["start", target]);
  }

  exec(
    target: string,
    command: readonly string[],
    options: Pick<CommandOptions, "input" | "timeoutMs"> = {},
  ): Promise<Result> {
    return this.command(["exec", target, ...command], options);
  }

  fork(
    source: string,
    destination: string,
    options: boolean | ForkOptions = {},
  ): Promise<Result> {
    const normalized =
      typeof options === "boolean" ? { paused: options } : options;
    return this.forkMany(source, [destination], normalized);
  }

  forkMany(
    source: string,
    destinations: readonly string[],
    options: ForkOptions = {},
  ): Promise<Result> {
    if (destinations.length === 0) {
      throw new RangeError("destinations must not be empty");
    }
    return this.command([
      "fork",
      ...(options.paused ? ["--paused"] : []),
      ...(options.targetNode ? ["--target", options.targetNode] : []),
      source,
      ...destinations,
    ]);
  }

  pause(target: string): Promise<Result> {
    return this.command(["pause", target]);
  }

  resume(target: string): Promise<Result> {
    return this.command(["resume", target]);
  }

  dehydrate(target: string): Promise<Result> {
    return this.command(["dehydrate", target]);
  }

  rehydrate(target: string): Promise<Result> {
    return this.command(["rehydrate", target]);
  }

  checkpoint(
    target: string,
    file: string,
    compression: "none" | "flate-best-speed" = "flate-best-speed",
  ): Promise<Result> {
    return this.command([
      "checkpoint",
      "--compression",
      compression,
      target,
      resolve(file),
    ]);
  }

  restore(file: string, name?: string): Promise<Result> {
    return this.command(["restore", ...(name ? ["--name", name] : []), resolve(file)]);
  }

  stop(target: string, timeoutSeconds?: number): Promise<Result> {
    return this.command([
      "stop",
      ...(timeoutSeconds === undefined ? [] : ["--time", String(timeoutSeconds)]),
      target,
    ]);
  }

  restart(target: string, timeoutSeconds?: number): Promise<Result> {
    return this.command([
      "restart",
      ...(timeoutSeconds === undefined ? [] : ["--time", String(timeoutSeconds)]),
      target,
    ]);
  }

  kill(target: string, signal?: string | number): Promise<Result> {
    return this.command([
      "kill",
      ...(signal === undefined ? [] : ["--signal", String(signal)]),
      target,
    ]);
  }

  wait(target: string, options: Pick<CommandOptions, "timeoutMs"> = {}): Promise<Result> {
    return this.command(["wait", target], options);
  }

  remove(target: string, force = false): Promise<Result> {
    return this.command(["rm", ...(force ? ["--force"] : []), target]);
  }

  private exchange(
    request: Record<string, unknown>,
    input: Buffer | undefined,
    timeoutMs: number | undefined,
  ): Promise<Result> {
    return new Promise((resolve, reject) => {
      const socket = createConnection(this.socketPath);
      const stdout: Buffer[] = [];
      const stderr: Buffer[] = [];
      let receive = Buffer.alloc(0);
      let inputOffset = 0;
      let unacknowledged = 0;
      let eofSent = input === undefined;
      let settled = false;

      const finish = (error?: Error, result?: Result): void => {
        if (settled) return;
        settled = true;
        socket.destroy();
        if (error) reject(error);
        else resolve(result!);
      };

      const timer = setTimeout(
        () => finish(new TTCError(`ttcd operation timed out after ${timeoutMs ?? this.timeoutMs} ms`)),
        timeoutMs ?? this.timeoutMs,
      );
      timer.unref();

      const pumpInput = (): void => {
        if (input === undefined || settled) return;
        while (inputOffset < input.length && unacknowledged < STDIN_WINDOW) {
          const size = Math.min(
            MAX_FRAME,
            STDIN_WINDOW - unacknowledged,
            input.length - inputOffset,
          );
          const payload = input.subarray(inputOffset, inputOffset + size);
          inputOffset += size;
          unacknowledged += size;
          if (!socket.write(frame("I", payload))) break;
        }
        if (inputOffset === input.length && !eofSent) {
          eofSent = true;
          socket.write(frame("C"));
        }
      };

      socket.once("connect", () => {
        socket.write(`${JSON.stringify(request)}\n`);
        pumpInput();
      });
      socket.on("drain", pumpInput);
      socket.on("error", (error) => finish(error));
      socket.on("close", () => {
        clearTimeout(timer);
        if (!settled) {
          finish(new ProtocolError("ttcd closed the connection before returning status"));
        }
      });
      socket.on("data", (chunk: Buffer) => {
        receive = Buffer.concat([receive, chunk]);
        while (receive.length >= 5) {
          const length = receive.readUInt32BE(1);
          if (length > MAX_FRAME) {
            clearTimeout(timer);
            finish(new ProtocolError(`oversized ttcd frame: ${length} bytes`));
            return;
          }
          if (receive.length < 5 + length) return;
          const kind = String.fromCharCode(receive[0]);
          const payload = receive.subarray(5, 5 + length);
          receive = receive.subarray(5 + length);
          switch (kind) {
            case "O":
              stdout.push(Buffer.from(payload));
              break;
            case "E":
              stderr.push(Buffer.from(payload));
              break;
            case "A": {
              if (payload.length !== 4) {
                clearTimeout(timer);
                finish(new ProtocolError("invalid stdin acknowledgement"));
                return;
              }
              const acknowledged = payload.readUInt32BE(0);
              if (acknowledged > unacknowledged) {
                clearTimeout(timer);
                finish(new ProtocolError("ttcd acknowledged unsent stdin"));
                return;
              }
              unacknowledged -= acknowledged;
              pumpInput();
              break;
            }
            case "T":
              break;
            case "X":
              if (payload.length !== 4) {
                clearTimeout(timer);
                finish(new ProtocolError("invalid status frame"));
                return;
              }
              clearTimeout(timer);
              finish(
                undefined,
                new Result(
                  payload.readUInt32BE(0),
                  Buffer.concat(stdout),
                  Buffer.concat(stderr),
                ),
              );
              return;
            default:
              clearTimeout(timer);
              finish(new ProtocolError(`unexpected ttcd frame type ${JSON.stringify(kind)}`));
              return;
          }
        }
      });
    });
  }
}
