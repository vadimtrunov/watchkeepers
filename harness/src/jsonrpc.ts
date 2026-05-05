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
 * Outcome of {@link parseRequest} — either a valid request object or a
 * structured error the dispatcher can lift into a response.
 *
 * Discriminated on the `kind` field; callers MUST switch on it. The
 * `id` on the error variant is the best-effort recovered id (parser
 * tries to surface it for InvalidRequest cases) and `null` when not
 * recoverable (parse error / non-object body).
 */
export type ParseResult =
  | { readonly kind: "ok"; readonly request: JsonRpcRequest }
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

  if (!("id" in obj) || !isJsonRpcId(obj.id)) {
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
