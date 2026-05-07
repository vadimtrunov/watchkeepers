/**
 * Wire-format helpers for JSON-RPC 2.0 over stdio.
 *
 * Framing: newline-delimited JSON (NDJSON). One JSON value per line,
 * UTF-8 encoded, LF (\n) separator. Trailing CR is tolerated on input
 * (Windows-friendly) but never emitted. Empty lines are skipped.
 *
 * Why NDJSON instead of LSP-style Content-Length headers: the Go core
 * controls both ends of the pipe, so we never need to recover from a
 * truncated payload — a partial line means the producer crashed and the
 * supervisor restarts the harness anyway. NDJSON keeps the parser ~30
 * lines instead of ~120 and degrades gracefully under `cat | harness`
 * smoke tests. If a future bidirectional embedding requires Content-
 * Length framing, swap this module — the public method registry does
 * not depend on the framing.
 */

import {
  JSON_RPC_VERSION,
  JsonRpcErrorCode,
  type JsonRpcErrorCodeValue,
  type JsonRpcErrorResponse,
  type JsonRpcId,
  type JsonRpcNotification,
  type JsonRpcRequest,
  type JsonRpcResponse,
  type JsonRpcSuccessResponse,
  type JsonRpcValue,
} from "./types.js";

/**
 * Outcome of {@link parseResponse} — a valid success response, a valid
 * error response, or a structured parse error the
 * {@link RpcClient} surfaces back to the caller (typically logged-and-
 * dropped because no pending entry can be correlated).
 *
 * Symmetric with {@link ParseResult} but typed for the inbound-response
 * direction (Go → harness): the harness emitted the request and is now
 * matching `id` against its pending-request map.
 */
export type ParseResponseResult =
  | { readonly kind: "ok"; readonly response: JsonRpcResponse }
  | {
      readonly kind: "error";
      readonly id: JsonRpcId;
      readonly code: JsonRpcErrorCodeValue;
      readonly message: string;
    };

/**
 * Outcome of {@link parseRequest} — a valid request, a valid notification
 * (JSON-RPC 2.0 §4.1, no `id` member), or a structured error the
 * dispatcher can lift into a response.
 *
 * Discriminated on the `kind` field; callers MUST switch on it. The
 * `id` on the error variant is the best-effort recovered id (parser
 * tries to surface it for InvalidRequest cases) and `null` when not
 * recoverable (parse error / non-object body).
 */
export type ParseResult =
  | { readonly kind: "ok"; readonly request: JsonRpcRequest }
  | { readonly kind: "notification"; readonly notification: JsonRpcNotification }
  | {
      readonly kind: "error";
      readonly id: JsonRpcId;
      readonly code: JsonRpcErrorCodeValue;
      readonly message: string;
    };

/**
 * Parse a single line of NDJSON wire format into a JSON-RPC request or
 * a structured parse error.
 *
 * Validates the JSON-RPC 2.0 envelope: `jsonrpc === "2.0"`, `method` is
 * a string, `id` is string | number | null. Does NOT validate `params`
 * shape — that is the method handler's job (per JSON-RPC spec, the
 * server returns InvalidParams when the handler rejects).
 *
 * Returns a structured error result rather than throwing so the caller
 * can lift it into a JSON-RPC error response without a try/catch.
 */
export function parseRequest(line: string): ParseResult {
  let raw: unknown;
  try {
    raw = JSON.parse(line);
  } catch (err) {
    return {
      kind: "error",
      id: null,
      code: JsonRpcErrorCode.ParseError,
      message: err instanceof Error ? err.message : "invalid JSON",
    };
  }

  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    return {
      kind: "error",
      id: null,
      code: JsonRpcErrorCode.InvalidRequest,
      message: "request must be a JSON object",
    };
  }

  const obj = raw as {
    jsonrpc?: unknown;
    id?: unknown;
    method?: unknown;
    params?: unknown;
  };
  const recoveredId = isJsonRpcId(obj.id) ? obj.id : null;

  if (obj.jsonrpc !== JSON_RPC_VERSION) {
    return {
      kind: "error",
      id: recoveredId,
      code: JsonRpcErrorCode.InvalidRequest,
      message: `jsonrpc field must be "${JSON_RPC_VERSION}"`,
    };
  }

  if (typeof obj.method !== "string" || obj.method.length === 0) {
    return {
      kind: "error",
      id: recoveredId,
      code: JsonRpcErrorCode.InvalidRequest,
      message: "method field must be a non-empty string",
    };
  }

  // JSON-RPC 2.0 §4.1: a Notification is a Request without an `id`
  // member. Servers MUST NOT reply. Parse the id-less envelope as a
  // distinct shape so the dispatcher can dispatch the method without
  // writing a response.
  if (!("id" in obj)) {
    const notif: JsonRpcNotification =
      "params" in obj
        ? {
            jsonrpc: JSON_RPC_VERSION,
            method: obj.method,
            params: obj.params as JsonRpcValue,
          }
        : {
            jsonrpc: JSON_RPC_VERSION,
            method: obj.method,
          };
    return { kind: "notification", notification: notif };
  }

  if (!isJsonRpcId(obj.id)) {
    return {
      kind: "error",
      id: recoveredId,
      code: JsonRpcErrorCode.InvalidRequest,
      message: "id field must be string, number, or null",
    };
  }

  const request: JsonRpcRequest =
    "params" in obj
      ? {
          jsonrpc: JSON_RPC_VERSION,
          id: obj.id,
          method: obj.method,
          params: obj.params as JsonRpcValue,
        }
      : {
          jsonrpc: JSON_RPC_VERSION,
          id: obj.id,
          method: obj.method,
        };

  return { kind: "ok", request };
}

function isJsonRpcId(value: unknown): value is JsonRpcId {
  return value === null || typeof value === "string" || typeof value === "number";
}

/**
 * Build a success response envelope. Result is required per spec, even
 * when the underlying method semantically has no payload (callers pass
 * an empty object or `null`).
 */
export function successResponse(id: JsonRpcId, result: JsonRpcValue): JsonRpcSuccessResponse {
  return { jsonrpc: JSON_RPC_VERSION, id, result };
}

/**
 * Build an error response envelope. `data` is optional structured
 * diagnostics; omit when the message alone is sufficient.
 */
export function errorResponse(
  id: JsonRpcId,
  code: number,
  message: string,
  data?: JsonRpcValue,
): JsonRpcErrorResponse {
  return data === undefined
    ? { jsonrpc: JSON_RPC_VERSION, id, error: { code, message } }
    : { jsonrpc: JSON_RPC_VERSION, id, error: { code, message, data } };
}

/**
 * Build a server-to-client notification envelope. Notifications carry
 * no id and never receive a response. Use for streaming events (token
 * deltas, tool-call observations) once the dispatcher gains streaming
 * methods in M5.3.b/c/d.
 */
export function notification(method: string, params?: JsonRpcValue): JsonRpcNotification {
  return params === undefined
    ? { jsonrpc: JSON_RPC_VERSION, method }
    : { jsonrpc: JSON_RPC_VERSION, method, params };
}

/**
 * Serialize a response or notification to a single NDJSON line,
 * including the trailing LF. Callers write the result verbatim to
 * stdout.
 */
export function serialize(envelope: JsonRpcResponse | JsonRpcNotification): string {
  return JSON.stringify(envelope) + "\n";
}

/**
 * Serialize an outbound request envelope to a single NDJSON line. Used
 * by {@link RpcClient.request} when emitting harness → Go calls; the
 * envelope shape mirrors {@link serialize} so both directions share the
 * same wire format.
 */
export function serializeRequest(envelope: JsonRpcRequest): string {
  return JSON.stringify(envelope) + "\n";
}

/**
 * Parse a single line of NDJSON wire format into a JSON-RPC response or
 * a structured parse error. Symmetric with {@link parseRequest} but for
 * the inbound-response direction (Go → harness).
 *
 * Validates the JSON-RPC 2.0 response envelope: `jsonrpc === "2.0"`,
 * `id` is string | number | null, and exactly one of `result` / `error`
 * is present. The `error` object is shape-checked for `code: number` +
 * `message: string`. Does NOT validate `result` shape — the matching
 * pending-request callback handles application-level type narrowing.
 *
 * Returns a structured error result rather than throwing so the caller
 * (the {@link RpcClient} dispatcher) can log-and-drop unparseable lines
 * without aborting the read loop.
 */
export function parseResponse(line: string): ParseResponseResult {
  let raw: unknown;
  try {
    raw = JSON.parse(line);
  } catch (err) {
    return {
      kind: "error",
      id: null,
      code: JsonRpcErrorCode.ParseError,
      message: err instanceof Error ? err.message : "invalid JSON",
    };
  }

  if (typeof raw !== "object" || raw === null || Array.isArray(raw)) {
    return {
      kind: "error",
      id: null,
      code: JsonRpcErrorCode.InvalidRequest,
      message: "response must be a JSON object",
    };
  }

  const obj = raw as {
    jsonrpc?: unknown;
    id?: unknown;
    result?: unknown;
    error?: unknown;
  };
  const recoveredId = isJsonRpcId(obj.id) ? obj.id : null;

  if (obj.jsonrpc !== JSON_RPC_VERSION) {
    return {
      kind: "error",
      id: recoveredId,
      code: JsonRpcErrorCode.InvalidRequest,
      message: `jsonrpc field must be "${JSON_RPC_VERSION}"`,
    };
  }

  if (!("id" in obj) || !isJsonRpcId(obj.id)) {
    return {
      kind: "error",
      id: recoveredId,
      code: JsonRpcErrorCode.InvalidRequest,
      message: "id field must be string, number, or null",
    };
  }

  const hasResult = "result" in obj;
  const hasError = "error" in obj;
  if (hasResult === hasError) {
    return {
      kind: "error",
      id: obj.id,
      code: JsonRpcErrorCode.InvalidRequest,
      message: "response must carry exactly one of result or error",
    };
  }

  if (hasResult) {
    const success: JsonRpcSuccessResponse = {
      jsonrpc: JSON_RPC_VERSION,
      id: obj.id,
      result: obj.result as JsonRpcValue,
    };
    return { kind: "ok", response: success };
  }

  // Error response: shape-check {code: number, message: string}.
  if (typeof obj.error !== "object" || obj.error === null || Array.isArray(obj.error)) {
    return {
      kind: "error",
      id: obj.id,
      code: JsonRpcErrorCode.InvalidRequest,
      message: "error field must be an object",
    };
  }
  const errObj = obj.error as { code?: unknown; message?: unknown; data?: unknown };
  if (typeof errObj.code !== "number") {
    return {
      kind: "error",
      id: obj.id,
      code: JsonRpcErrorCode.InvalidRequest,
      message: "error.code field must be a number",
    };
  }
  if (typeof errObj.message !== "string") {
    return {
      kind: "error",
      id: obj.id,
      code: JsonRpcErrorCode.InvalidRequest,
      message: "error.message field must be a string",
    };
  }

  const errorResp: JsonRpcErrorResponse =
    "data" in errObj
      ? {
          jsonrpc: JSON_RPC_VERSION,
          id: obj.id,
          error: {
            code: errObj.code,
            message: errObj.message,
            data: errObj.data as JsonRpcValue,
          },
        }
      : {
          jsonrpc: JSON_RPC_VERSION,
          id: obj.id,
          error: { code: errObj.code, message: errObj.message },
        };
  return { kind: "ok", response: errorResp };
}

/**
 * Typed error a rejected {@link RpcClient.request} promise carries when
 * the peer responded with a JSON-RPC error envelope. Includes the
 * wire-level `code` and the original `data` payload so callers can
 * branch on `code === JsonRpcErrorCode.MethodNotFound` etc. without
 * string-matching the message.
 */
export class RpcRequestError extends Error {
  public readonly code: number;
  public readonly data: JsonRpcValue | undefined;

  public constructor(code: number, message: string, data?: JsonRpcValue) {
    super(message);
    this.name = "RpcRequestError";
    this.code = code;
    this.data = data;
  }
}

/**
 * Sink for outbound NDJSON lines emitted by {@link RpcClient.request}.
 * The harness wires this to its stdout writer; tests pass a synchronous
 * buffer. Synchronous-only by design — the caller (a `Writable.write`
 * wrapper) handles backpressure.
 */
export type LineWriter = (line: string) => void;

interface PendingEntry {
  readonly resolve: (value: JsonRpcValue) => void;
  readonly reject: (err: Error) => void;
}

/**
 * Optional logger for unmatched-id responses. The {@link RpcClient}
 * never throws on unknown ids (it would tear the read loop down for a
 * benign protocol skew); it logs-and-drops via this hook so tests can
 * observe the event and production wires it to a structured logger.
 */
export type UnknownIdLogger = (id: JsonRpcId, response: JsonRpcResponse) => void;

/**
 * Bidirectional client for the harness → Go direction of the JSON-RPC
 * channel. Owns:
 *
 *   - an auto-incrementing numeric id allocator (per-instance, no
 *     global state);
 *   - a `Map<id, {resolve, reject}>` of pending requests;
 *   - the {@link request} method that emits an envelope via the
 *     supplied {@link LineWriter} and returns a `Promise` keyed on the
 *     just-allocated id;
 *   - the {@link handleResponseLine} method that the read loop calls
 *     for each inbound NDJSON line classified as a response (i.e. the
 *     dispatcher saw `result` or `error` in place of `method`).
 *
 * The class is instantiable and stateless beyond the id counter +
 * pending map, so the harness can construct one per spawned channel
 * (today: one per harness lifetime).
 *
 * Cleanup: on every resolve / reject the pending entry is removed from
 * the map. Unmatched-id responses are logged via {@link UnknownIdLogger}
 * (defaults to a no-op) and dropped — they MUST NOT throw because
 * doing so would crash the read loop on a benign Go-side bug.
 *
 * Concurrency: JS is single-threaded; {@link request} can be called
 * from any async context without a lock — the id counter increments in
 * a single tick and the map mutations are synchronous.
 */
export class RpcClient {
  private nextId = 1;
  private readonly pending = new Map<number, PendingEntry>();
  private readonly write: LineWriter;
  private readonly onUnknownId: UnknownIdLogger;

  public constructor(write: LineWriter, onUnknownId?: UnknownIdLogger) {
    this.write = write;
    this.onUnknownId = onUnknownId ?? (() => undefined);
  }

  /**
   * Emit a JSON-RPC request and return a promise that resolves to the
   * peer's `result` or rejects with {@link RpcRequestError} when the
   * peer responds with an error envelope. The promise NEVER resolves
   * if no response arrives — callers that need a deadline MUST race
   * against an external timer.
   *
   * The result is widened to `unknown` because the wire format does
   * not carry a TypeScript type witness; callers should narrow via a
   * runtime schema (zod) at the application boundary.
   */
  public request(method: string, params?: JsonRpcValue): Promise<JsonRpcValue> {
    const id = this.nextId++;
    const envelope: JsonRpcRequest =
      params === undefined
        ? { jsonrpc: JSON_RPC_VERSION, id, method }
        : { jsonrpc: JSON_RPC_VERSION, id, method, params };

    return new Promise<JsonRpcValue>((resolve, reject) => {
      this.pending.set(id, { resolve, reject });
      try {
        this.write(serializeRequest(envelope));
      } catch (err) {
        // Writer threw before the line was queued: synchronously remove
        // the pending entry and surface the failure. Keeping the entry
        // would leak (no response will ever arrive).
        this.pending.delete(id);
        reject(err instanceof Error ? err : new Error(String(err)));
      }
    });
  }

  /**
   * Feed a parsed inbound response line into the pending-request map.
   * Returns `true` when the response matched a pending entry, `false`
   * when the id was unknown (logged-and-dropped). NEVER throws.
   */
  public handleResponse(response: JsonRpcResponse): boolean {
    if (typeof response.id !== "number") {
      this.onUnknownId(response.id, response);
      return false;
    }
    const entry = this.pending.get(response.id);
    if (entry === undefined) {
      this.onUnknownId(response.id, response);
      return false;
    }
    this.pending.delete(response.id);
    if ("result" in response) {
      entry.resolve(response.result);
    } else {
      entry.reject(
        new RpcRequestError(response.error.code, response.error.message, response.error.data),
      );
    }
    return true;
  }

  /**
   * Convenience: parse a raw NDJSON line and feed the result through
   * {@link handleResponse}. Returns the parse outcome so callers can
   * log malformed input. Lines that fail to parse never reject any
   * pending promise — the read loop just keeps draining.
   */
  public handleResponseLine(line: string): ParseResponseResult {
    const parsed = parseResponse(line);
    if (parsed.kind === "ok") {
      this.handleResponse(parsed.response);
    }
    return parsed;
  }

  /**
   * Number of in-flight requests. Test-only accessor; production code
   * should not depend on this — it is exposed so the pending-map
   * cleanup invariant can be asserted directly.
   */
  public pendingCount(): number {
    return this.pending.size;
  }
}
