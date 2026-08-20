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
export class StreamCancelled extends TTCError {}

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

export interface CommitOptions {
  author?: string;
  message?: string;
  changes?: readonly string[];
  pause?: boolean;
}

export interface ImageLoadOptions {
  input?: string;
  archive?: string | Uint8Array;
  platform?: string;
  quiet?: boolean;
}

export interface ImageSaveOptions {
  output?: string;
  platform?: string;
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

/**
 * Builds a `W` terminal-size frame.
 *
 * The payload is eight bytes: big-endian rows followed by big-endian columns,
 * each a uint32. See `decodeDaemonTerminalSize` in runsc/ttc/cmd.
 */
function sizeFrame(rows: number, cols: number): Buffer {
  for (const [name, value] of [["rows", rows], ["cols", cols]] as const) {
    if (!Number.isSafeInteger(value) || value < 0 || value > 0xffff_ffff) {
      throw new RangeError(`${name} must be a uint32`);
    }
  }
  const payload = Buffer.allocUnsafe(8);
  payload.writeUInt32BE(rows, 0);
  payload.writeUInt32BE(cols, 4);
  return frame("W", payload);
}

/**
 * Callbacks for a streaming operation. Every one is optional.
 *
 * `onObservation` receives one complete `S` frame payload. ttcd guarantees each
 * such frame is a whole JSON object, so callers never reassemble across frame
 * boundaries.
 */
export interface StreamHandlers {
  onStdout?: (chunk: Buffer) => void;
  onStderr?: (chunk: Buffer) => void;
  onObservation?: (payload: Buffer) => void;
  onTerminal?: (payload: Buffer) => void;
}

export interface StreamOptions extends StreamHandlers {
  /**
   * Declare an stdin channel. Required before `write` is legal. Unlike a
   * buffered command, a stream never sends EOF implicitly: closing stdin
   * terminates an interactive shell, so it must be an explicit act.
   */
  stdin?: boolean;
  requestId?: string;
  /**
   * Abort after this many milliseconds. Endless operations such as
   * `tty watch`, `events --follow`, and `attach` normally leave this unset.
   */
  timeoutMs?: number;
  /**
   * Terminal size to announce immediately after the header.
   *
   * `attach` blocks until the client reports one and fails with "timed out
   * waiting for attached client terminal size", so a non-interactive caller
   * must supply it. It is sent inside the connect handler rather than by a
   * separate `resize` call, which would otherwise be queued ahead of the
   * header.
   */
  size?: { rows: number; cols: number };
}

/**
 * One live ttcd operation with incremental delivery.
 *
 * `Client.command` buffers to completion, which cannot express an endless
 * `tty watch` stream or an `attach` that must stay writable for hours. A stream
 * delivers frames as they arrive and keeps stdin open until the caller closes
 * it or the daemon reports status.
 */
export class TtcStream {
  readonly args: readonly string[];
  /** Resolves with the command status, or rejects on a transport failure. */
  readonly exited: Promise<number>;

  private readonly socket: Socket;
  private readonly handlers: StreamHandlers;
  private receive: Buffer = Buffer.alloc(0);
  private pending: Buffer[] = [];
  private unacknowledged = 0;
  private settled = false;
  private eofSent = false;
  private eofRequested = false;
  /**
   * Frames may only be written once the header has been sent. Node queues
   * pre-connect writes and flushes them in order, so pumping early would put
   * stdin bytes ahead of the JSON header and desynchronize the daemon.
   */
  private connected = false;
  private pendingSize: { rows: number; cols: number } | undefined;
  private readonly stdinDeclared: boolean;
  private timer: ReturnType<typeof setTimeout> | undefined;
  private resolveExit!: (status: number) => void;
  private rejectExit!: (error: Error) => void;

  /**
   * Takes the fully built request rather than reconstructing it, so that
   * non-`command` actions such as `ping`, whose header carries no `args` or
   * `stdin` keys at all, round-trip byte for byte.
   */
  constructor(socketPath: string, request: Record<string, unknown>, options: StreamOptions = {}) {
    this.args = Array.isArray(request.args) ? [...(request.args as string[])] : [];
    this.handlers = options;
    this.stdinDeclared = options.stdin === true;
    if (options.size) sizeFrame(options.size.rows, options.size.cols);
    this.pendingSize = options.size;
    this.exited = new Promise<number>((res, rej) => {
      this.resolveExit = res;
      this.rejectExit = rej;
    });
    // Fire-and-forget streams are a normal usage, and without this Node reports
    // an unhandled rejection when such a stream fails.
    this.exited.catch(() => {});

    this.socket = createConnection(socketPath);
    this.socket.setNoDelay(true);

    if (options.timeoutMs !== undefined) {
      this.timer = setTimeout(
        () => this.finish(new TTCError(`TTC operation timed out after ${options.timeoutMs} ms`)),
        options.timeoutMs,
      );
      this.timer.unref?.();
    }

    this.socket.once("connect", () => {
      this.socket.write(`${JSON.stringify(request)}\n`);
      if (this.pendingSize) {
        this.socket.write(sizeFrame(this.pendingSize.rows, this.pendingSize.cols));
        this.pendingSize = undefined;
      }
      this.connected = true;
      this.pump();
    });
    this.socket.on("drain", () => this.pump());
    this.socket.on("error", (error) => this.finish(error));
    this.socket.on("close", () => {
      if (!this.settled) {
        this.finish(new ProtocolError("ttcd closed the connection before returning status"));
      }
    });
    this.socket.on("data", (chunk: Buffer) => this.consume(chunk));
  }

  /**
   * Queues stdin bytes, respecting ttcd's 1 MiB acknowledgement window. Data
   * beyond the window waits for an `A` frame rather than being dropped.
   */
  write(data: Buffer | string): void {
    if (!this.stdinDeclared) {
      throw new TTCError("stream was not opened with stdin enabled");
    }
    if (this.settled) throw new TTCError("stream is already settled");
    if (this.eofRequested || this.eofSent) throw new TTCError("stdin already closed");
    this.pending.push(typeof data === "string" ? Buffer.from(data) : data);
    this.pump();
  }

  /**
   * Requests stdin EOF, which ends an interactive shell.
   *
   * The `C` frame is sent only once every queued byte has been written. Input
   * larger than the flow-control window is still in `pending` at this point,
   * and discarding it would silently truncate the stream.
   */
  closeStdin(): void {
    if (!this.stdinDeclared) {
      throw new TTCError("stream was not opened with stdin enabled");
    }
    if (this.settled) throw new TTCError("stream is already settled");
    if (this.eofRequested) return;
    this.eofRequested = true;
    this.pump();
  }

  /** Sends a `W` resize frame carrying big-endian rows then columns. */
  resize(rows: number, cols: number): void {
    if (this.settled) throw new TTCError("stream is already settled");
    // Validate synchronously even when the connection is still pending.
    sizeFrame(rows, cols);
    if (!this.connected) {
      // Only the latest pre-connect size matters. It must be sent by the
      // connect handler after the JSON header, never queued on Socket first.
      this.pendingSize = { rows, cols };
      return;
    }
    this.socket.write(sizeFrame(rows, cols));
  }

  /** Cancels locally; `exited` rejects because no daemon status was received. */
  close(): void {
    this.finish(new StreamCancelled("TTC stream was cancelled before returning status"));
  }

  private pump(): void {
    if (this.settled || !this.connected) return;
    while (this.pending.length > 0 && this.unacknowledged < STDIN_WINDOW) {
      const head = this.pending[0]!;
      const size = Math.min(MAX_FRAME, STDIN_WINDOW - this.unacknowledged, head.length);
      const payload = head.subarray(0, size);
      if (size === head.length) this.pending.shift();
      else this.pending[0] = head.subarray(size);
      this.unacknowledged += size;
      if (!this.socket.write(frame("I", payload))) return;
    }
    if (this.eofRequested && !this.eofSent && this.pending.length === 0) {
      this.eofSent = true;
      this.socket.write(frame("C"));
    }
  }

  private consume(chunk: Buffer): void {
    this.receive = this.receive.length === 0 ? chunk : Buffer.concat([this.receive, chunk]);
    while (this.receive.length >= 5) {
      const length = this.receive.readUInt32BE(1);
      if (length > MAX_FRAME) {
        this.finish(new ProtocolError(`oversized TTC frame: ${length} bytes`));
        return;
      }
      if (this.receive.length < 5 + length) return;
      const kind = String.fromCharCode(this.receive[0]!);
      const payload = Buffer.from(this.receive.subarray(5, 5 + length));
      this.receive = this.receive.subarray(5 + length);
      if (!this.dispatch(kind, payload)) return;
    }
  }

  /** Returns false once the stream has settled and parsing must stop. */
  private dispatch(kind: string, payload: Buffer): boolean {
    switch (kind) {
      case "O":
        return this.deliver(this.handlers.onStdout, payload);
      case "E":
        return this.deliver(this.handlers.onStderr, payload);
      case "S":
        return this.deliver(this.handlers.onObservation, payload);
      case "T":
        return this.deliver(this.handlers.onTerminal, payload);
      case "A": {
        if (payload.length !== 4) {
          this.finish(new ProtocolError("invalid stdin acknowledgement"));
          return false;
        }
        const acknowledged = payload.readUInt32BE(0);
        if (acknowledged > this.unacknowledged) {
          this.finish(new ProtocolError("ttcd acknowledged unsent stdin"));
          return false;
        }
        this.unacknowledged -= acknowledged;
        this.pump();
        return true;
      }
      case "X": {
        if (payload.length !== 4) {
          this.finish(new ProtocolError("invalid status frame"));
          return false;
        }
        this.finish(undefined, payload.readUInt32BE(0));
        return false;
      }
      default:
        this.finish(new ProtocolError(`unexpected TTC frame ${JSON.stringify(kind)}`));
        return false;
    }
  }

  private deliver(handler: ((payload: Buffer) => void) | undefined, payload: Buffer): boolean {
    try {
      handler?.(payload);
    } catch (error) {
      this.finish(error instanceof Error ? error : new TTCError(String(error)));
      return false;
    }
    // A callback may intentionally cancel the stream. Do not deliver frames
    // that happened to arrive in the same socket chunk after cancellation.
    return !this.settled;
  }

  private finish(error?: Error, status?: number): void {
    if (this.settled) return;
    this.settled = true;
    if (this.timer) clearTimeout(this.timer);
    this.socket.destroy();
    if (error) this.rejectExit(error);
    else this.resolveExit(status ?? 0);
  }
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

  execSession(
    target: string,
    command: readonly string[],
    session: string,
    options: { tty?: boolean; interactive?: boolean } = {},
  ): Promise<Result> {
    return this.command([
      "exec", "--session", session, "--detach",
      ...(options.tty ? ["--tty"] : []),
      ...(options.interactive ? ["--interactive"] : []),
      target, ...command,
    ]);
  }

  async execSessions<T = unknown>(target: string): Promise<T> {
    return (await this.command(["exec", "ls", "--json", target], { check: true })).json<T>();
  }

  execLogs(target: string, session: string): Promise<Result> {
    return this.command(["logs", "--exec", session, target]);
  }

  execWait(target: string, session: string): Promise<Result> {
    return this.command(["exec", "wait", `${target}:${session}`], { timeoutMs: undefined });
  }

  async ttyStat<T = unknown>(target: string, session: string): Promise<T> {
    return (await this.command(["tty", "stat", target, "--session", session, "--json"], { check: true })).json<T>();
  }

  commit(target: string, image: string, options: CommitOptions = {}): Promise<Result> {
    const args = ["commit"];
    if (options.author !== undefined) args.push("--author", options.author);
    if (options.message !== undefined) args.push("--message", options.message);
    for (const change of options.changes ?? []) args.push("--change", change);
    if (options.pause === false) args.push("--pause=false");
    return this.command([...args, target, image]);
  }

  load(options: ImageLoadOptions = {}): Promise<Result> {
    if (options.input !== undefined && options.archive !== undefined) {
      throw new TypeError("image load accepts either input or archive, not both");
    }
    if (options.input === undefined && options.archive === undefined) {
      throw new TypeError("image load requires input or archive");
    }
    const args = ["load"];
    if (options.input !== undefined) args.push("--input", resolve(options.input));
    if (options.platform !== undefined) args.push("--platform", options.platform);
    if (options.quiet) args.push("--quiet");
    return this.command(args, { input: options.archive });
  }

  save(images: readonly string[], options: ImageSaveOptions = {}): Promise<Result> {
    if (images.length === 0) throw new TypeError("image save requires at least one image");
    const args = ["save"];
    if (options.output !== undefined) args.push("--output", resolve(options.output));
    if (options.platform !== undefined) args.push("--platform", options.platform);
    return this.command([...args, ...images]);
  }

  async stats<T = unknown>(target: string): Promise<T> {
    return (await this.command(["stats", "--json", target], { check: true })).json<T>();
  }

  async diff<T = unknown>(target: string): Promise<T> {
    return (await this.command(["diff", "--json", target], { check: true })).json<T>();
  }

  copy(source: string, destination: string): Promise<Result> {
    return this.command(["cp", source, destination]);
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

  migrate(target: string, targetNode: string): Promise<Result> {
    return this.command(["migrate", "--target", targetNode, target]);
  }

  async nodes<T = unknown>(): Promise<T> {
    return (await this.command(["node", "ls", "--json"], { check: true })).json<T>();
  }

  drainNode(node: string, targetNode?: string): Promise<Result> {
    return this.command([
      "node", "drain",
      ...(targetNode ? ["--target", targetNode] : []),
      node,
    ]);
  }

  async rebalanceNode<T = unknown>(node: string, targetNode?: string): Promise<T> {
    return (await this.command([
      "node", "rebalance",
      ...(targetNode ? ["--target", targetNode] : []),
      node,
    ], { check: true })).json<T>();
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

  /**
   * Opens a streaming operation.
   *
   * Use this for anything that outlives a single request/response: `tty watch`,
   * `events --follow`, `logs --follow`, and `attach`.
   */
  stream(args: readonly string[], options: StreamOptions = {}): TtcStream {
    if (args.length === 0 || args.some((arg) => typeof arg !== "string")) {
      throw new TypeError("args must be a non-empty array of strings");
    }
    return new TtcStream(
      this.socketPath,
      {
        version: API_VERSION,
        action: "command",
        request_id: options.requestId ?? randomBytes(16).toString("hex"),
        args: [...args],
        stdin: options.stdin === true,
      },
      options,
    );
  }

  /**
   * Streams terminal observations for a named exec session.
   *
   * The Sentry pushes a snapshot on registration, coalesced snapshots after
   * observable state changes, and a periodic heartbeat. Each payload delivered
   * to `onObservation` is one complete JSON object.
   *
   * Observations do not follow a fork: a child branch starts with no observers,
   * so watch the child explicitly after forking.
   */
  watchTty(target: string, session: string, handlers: StreamHandlers): TtcStream {
    return this.stream(["tty", "watch", target, "--session", session], handlers);
  }

  /**
   * Attaches to a branch's init console or a managed exec session.
   *
   * Stdin stays open until `closeStdin` or `close` is called. Sending EOF ends
   * an interactive shell, so neither happens implicitly.
   */
  attach(
    target: string,
    options: {
      exec?: string;
      stdin?: boolean;
      takeover?: boolean;
      size?: { rows: number; cols: number };
    } & StreamHandlers = {},
  ): TtcStream {
    const { exec, stdin = true, takeover = false, size, ...handlers } = options;
    return this.stream(
      [
        "attach",
        ...(stdin ? ["--interactive"] : ["--no-stdin"]),
        ...(takeover ? ["--takeover"] : []),
        ...(exec ? ["--exec", exec] : []),
        target,
      ],
      // ttcd will not begin the attach until it learns a terminal size, so a
      // programmatic caller that never resizes still needs a default.
      { ...handlers, stdin, size: size ?? { rows: 24, cols: 80 } },
    );
  }

  /** Follows the daemon lifecycle event log as newline-delimited JSON. */
  watchEvents(
    handlers: StreamHandlers & { since?: string; filters?: Readonly<Record<string, string>> } = {},
  ): TtcStream {
    const { since, filters, ...rest } = handlers;
    const filterArgs = Object.entries(filters ?? {}).flatMap(([key, value]) => [
      "--filter",
      `${key}=${value}`,
    ]);
    return this.stream(
      ["events", "--json", "--follow", ...(since ? ["--since", since] : []), ...filterArgs],
      rest,
    );
  }

  /** Follows init or managed exec session logs. */
  followLogs(
    target: string,
    options: { exec?: string; tail?: string | number } & StreamHandlers = {},
  ): TtcStream {
    const { exec, tail, ...handlers } = options;
    return this.stream(
      [
        "logs",
        "--follow",
        ...(tail === undefined ? [] : ["-n", String(tail)]),
        ...(exec ? ["--exec", exec] : []),
        target,
      ],
      handlers,
    );
  }

  private async exchange(
    request: Record<string, unknown>,
    input: Buffer | undefined,
    timeoutMs: number | undefined,
  ): Promise<Result> {
    const stdout: Buffer[] = [];
    const stderr: Buffer[] = [];
    const stream = new TtcStream(this.socketPath, request, {
      stdin: input !== undefined,
      timeoutMs: timeoutMs ?? this.timeoutMs,
      onStdout: (chunk) => stdout.push(chunk),
      onStderr: (chunk) => stderr.push(chunk),
      // Terminal observations are complete JSON objects. Preserve frame
      // boundaries as newline-delimited stdout records.
      onObservation: (payload) => stdout.push(payload, Buffer.from("\n")),
    });
    if (input !== undefined) {
      stream.write(input);
      stream.closeStdin();
    }
    const status = await stream.exited;
    return new Result(status, Buffer.concat(stdout), Buffer.concat(stderr));
  }
}
