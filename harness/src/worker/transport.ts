/**
 * `IpcJsonRpcTransport` — JSON-RPC 2.0 duplex over a Node IPC channel.
 *
 * Wraps either end of a `child_process.fork` IPC channel (parent's
 * `child.send` / `child.on('message')` or child's `process.send` /
 * `process.on('message')`) as a JSON-RPC 2.0 duplex with id-correlated
 * request/response and fire-and-forget notifications. Per ADR §0001
 * the wire envelope mirrors the harness↔Go core JSON-RPC envelope; no
 * NDJSON framing because IPC delivers whole structured-clone objects.
 */

import { JSON_RPC_VERSION, type JsonRpcId, type JsonRpcValue } from "../types.js";

/** Minimal duplex contract — both `ChildProcess` and `process` expose this shape. */
export interface IpcChannel {
  readonly send: (message: unknown) => void;
  readonly onMessage: (handler: (message: unknown) => void) => void;
  readonly offMessage: (handler: (message: unknown) => void) => void;
}

export interface IncomingRequest {
  readonly id: JsonRpcId;
  readonly method: string;
  readonly params: JsonRpcValue | undefined;
}

export interface IncomingNotification {
  readonly method: string;
  readonly params: JsonRpcValue | undefined;
}

export interface TransportParseError {
  readonly reason: string;
  readonly raw: unknown;
}

interface PendingRequest {
  readonly resolve: (value: JsonRpcValue) => void;
  readonly reject: (err: Error) => void;
}

/** JSON-RPC error returned via {@link IpcJsonRpcTransport.request} when the peer replies with an error envelope. */
export class JsonRpcRemoteError extends Error {
  public readonly code: number;
  public readonly data: JsonRpcValue | undefined;

  public constructor(code: number, message: string, data?: JsonRpcValue) {
    super(message);
    this.name = "JsonRpcRemoteError";
    this.code = code;
    this.data = data;
  }
}

/**
 * Wraps an {@link IpcChannel} as a JSON-RPC 2.0 duplex. After
 * construction, register handlers via `onRequest` / `onNotification` /
 * `onParseError`, then use `request` / `notify`. Call `dispose` to
 * detach the listener and reject pending requests.
 */
export class IpcJsonRpcTransport {
  private readonly channel: IpcChannel;
  private readonly pending = new Map<number, PendingRequest>();
  private nextId = 1;
  private requestHandler: ((req: IncomingRequest) => void) | undefined;
  private notificationHandler: ((notif: IncomingNotification) => void) | undefined;
  private parseErrorHandler: ((err: TransportParseError) => void) | undefined;
  private disposed = false;
  private readonly listener = (message: unknown): void => {
    this.handleMessage(message);
  };

  public constructor(channel: IpcChannel) {
    this.channel = channel;
    channel.onMessage(this.listener);
  }

  public request(method: string, params?: JsonRpcValue): Promise<JsonRpcValue> {
    if (this.disposed) return Promise.reject(new Error("transport disposed"));
    const id = this.nextId++;
    return new Promise<JsonRpcValue>((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      const envelope =
        params === undefined
          ? { jsonrpc: JSON_RPC_VERSION, id, method }
          : { jsonrpc: JSON_RPC_VERSION, id, method, params };
      try {
        this.channel.send(envelope);
      } catch (err) {
        this.pending.delete(id);
        reject(err instanceof Error ? err : new Error(String(err)));
      }
    });
  }

  public notify(method: string, params?: JsonRpcValue): void {
    if (this.disposed) return;
    const envelope =
      params === undefined
        ? { jsonrpc: JSON_RPC_VERSION, method }
        : { jsonrpc: JSON_RPC_VERSION, method, params };
    try {
      this.channel.send(envelope);
    } catch {
      /* notification is fire-and-forget; swallow send failures */
    }
  }

  public sendResponse(id: JsonRpcId, result: JsonRpcValue): void {
    if (this.disposed) return;
    try {
      this.channel.send({ jsonrpc: JSON_RPC_VERSION, id, result });
    } catch {
      /* best-effort response; swallow send failures */
    }
  }

  public sendError(id: JsonRpcId, code: number, message: string, data?: JsonRpcValue): void {
    if (this.disposed) return;
    const envelope =
      data === undefined
        ? { jsonrpc: JSON_RPC_VERSION, id, error: { code, message } }
        : { jsonrpc: JSON_RPC_VERSION, id, error: { code, message, data } };
    try {
      this.channel.send(envelope);
    } catch {
      /* best-effort error response; swallow send failures */
    }
  }

  public onRequest(handler: (req: IncomingRequest) => void): void {
    this.requestHandler = handler;
  }

  public onNotification(handler: (notif: IncomingNotification) => void): void {
    this.notificationHandler = handler;
  }

  public onParseError(handler: (err: TransportParseError) => void): void {
    this.parseErrorHandler = handler;
  }

  public dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    this.channel.offMessage(this.listener);
    for (const { reject } of this.pending.values()) {
      reject(new Error("transport disposed"));
    }
    this.pending.clear();
  }

  private handleMessage(message: unknown): void {
    if (typeof message !== "object" || message === null || Array.isArray(message)) {
      this.reportParseError("message must be a JSON-RPC envelope object", message);
      return;
    }
    const env = message as {
      jsonrpc?: unknown;
      id?: unknown;
      method?: unknown;
      params?: unknown;
      result?: unknown;
      error?: unknown;
    };
    if (env.jsonrpc !== JSON_RPC_VERSION) {
      // If the envelope looks like a response for a known pending request, reject
      // that pending entry so the awaiter doesn't hang until dispose().
      if (("result" in env || "error" in env) && typeof env.id === "number") {
        const pending = this.pending.get(env.id);
        if (pending !== undefined) {
          this.pending.delete(env.id);
          pending.reject(
            new Error(`malformed response: jsonrpc field must be "${JSON_RPC_VERSION}"`),
          );
        }
      }
      this.reportParseError(`jsonrpc field must be "${JSON_RPC_VERSION}"`, message);
      return;
    }
    if ("result" in env || "error" in env) {
      this.handleResponse(env, message);
      return;
    }
    if (typeof env.method !== "string" || env.method.length === 0) {
      this.reportParseError("method field must be a non-empty string", message);
      return;
    }
    const params = "params" in env ? (env.params as JsonRpcValue | undefined) : undefined;
    if (!("id" in env)) {
      this.notificationHandler?.({ method: env.method, params });
      return;
    }
    if (typeof env.id !== "string" && typeof env.id !== "number" && env.id !== null) {
      this.reportParseError("id field must be string, number, or null", message);
      return;
    }
    this.requestHandler?.({ id: env.id, method: env.method, params });
  }

  private handleResponse(
    env: { id?: unknown; result?: unknown; error?: unknown },
    raw: unknown,
  ): void {
    if (typeof env.id !== "number") {
      this.reportParseError("response id must be a number (transport allocates numeric ids)", raw);
      return;
    }
    const pending = this.pending.get(env.id);
    if (pending === undefined) {
      this.reportParseError(`no pending request for id=${String(env.id)}`, raw);
      return;
    }
    this.pending.delete(env.id);
    if ("error" in env) {
      const errObj = env.error as { code?: unknown; message?: unknown; data?: unknown } | undefined;
      const code = typeof errObj?.code === "number" ? errObj.code : 0;
      const message = typeof errObj?.message === "string" ? errObj.message : "remote error";
      const data = errObj && "data" in errObj ? (errObj.data as JsonRpcValue) : undefined;
      pending.reject(new JsonRpcRemoteError(code, message, data));
      return;
    }
    pending.resolve((env.result ?? null) as JsonRpcValue);
  }

  private reportParseError(reason: string, raw: unknown): void {
    this.parseErrorHandler?.({ reason, raw });
  }
}

/** Adapt a `process`-shaped object (child side) to {@link IpcChannel}. Throws when the IPC channel is missing. */
export function ipcChannelFromProcess(proc: NodeJS.Process): IpcChannel {
  const send = proc.send?.bind(proc);
  if (send === undefined) {
    throw new Error("process.send is undefined — child must be forked with an IPC channel");
  }
  return {
    send: (message) => {
      send(message);
    },
    onMessage: (handler) => proc.on("message", handler),
    offMessage: (handler) => proc.off("message", handler),
  };
}

/** Narrow `ChildProcess`-shape so this module avoids the gated `child_process` import. */
export interface ChildProcessLike {
  send(message: unknown): boolean;
  on(event: "message", handler: (message: unknown) => void): unknown;
  off(event: "message", handler: (message: unknown) => void): unknown;
}

export function ipcChannelFromChildProcess(child: ChildProcessLike): IpcChannel {
  return {
    send: (message) => {
      child.send(message);
    },
    onMessage: (handler) => {
      child.on("message", handler);
    },
    offMessage: (handler) => {
      child.off("message", handler);
    },
  };
}
