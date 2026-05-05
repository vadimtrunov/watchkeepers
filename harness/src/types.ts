/**
 * JSON-RPC 2.0 types — minimal subset used by the Watchkeeper harness.
 *
 * The harness implements JSON-RPC 2.0 over stdio with newline-delimited
 * JSON framing (NDJSON). One JSON value per line; UTF-8; LF separator.
 *
 * Wire-protocol contract is documented in `harness/README.md`. This file
 * carries only the structural types — request/response shapes, the small
 * set of standard error codes the harness raises, and a hand-rolled
 * notification envelope used for streaming events.
 *
 * Real wiring lands in M5.3 (Runtime adapter + Claude Code bridge) per
 * docs/ROADMAP-phase1.md. This module is the M5.3.a foundation slice —
 * no tool runner, no Claude Code wrapper, no zod-derived schemas yet.
 */

/**
 * JSON-RPC 2.0 specifies the version literal as the string "2.0". The
 * harness rejects any other value with {@link JsonRpcErrorCode.InvalidRequest}.
 */
export const JSON_RPC_VERSION = "2.0" as const;

/**
 * Identifier the JSON-RPC 2.0 spec accepts on requests / responses. The
 * harness preserves whatever the caller sent, including `null` for the
 * notification-style request shape (server treats null-id requests as
 * fire-and-forget — no response written). Numeric and string ids round
 * trip verbatim.
 */
export type JsonRpcId = string | number | null;

/**
 * Primitive-or-structured JSON value. Used for params and result payloads
 * so the harness never widens to `unknown` in user-facing types.
 */
export type JsonRpcValue =
  | string
  | number
  | boolean
  | null
  | { readonly [key: string]: JsonRpcValue }
  | readonly JsonRpcValue[];

/**
 * Standard JSON-RPC 2.0 error codes plus harness-reserved range. The
 * spec reserves `-32768` through `-32000` for protocol-level errors;
 * application-specific codes live outside that range. M5.3.a registers
 * only the protocol codes — application errors arrive in M5.3.b/c/d.
 */
export const JsonRpcErrorCode = {
  /** Malformed JSON / not parseable per RFC 8259. */
  ParseError: -32700,
  /** Valid JSON but not a valid Request object (missing method, wrong jsonrpc, ...). */
  InvalidRequest: -32600,
  /** Method does not exist on the server. */
  MethodNotFound: -32601,
  /** Method exists but params do not match the expected shape. */
  InvalidParams: -32602,
  /** Unhandled server-side error inside a method handler. */
  InternalError: -32603,
} as const;

/**
 * Numeric value of one of the {@link JsonRpcErrorCode} entries.
 */
export type JsonRpcErrorCodeValue = (typeof JsonRpcErrorCode)[keyof typeof JsonRpcErrorCode];

/**
 * JSON-RPC 2.0 request envelope. Params is optional per spec; the harness
 * does not distinguish between `params: undefined` and a missing key when
 * dispatching, but the wire shape elides the field for cleanliness.
 */
export interface JsonRpcRequest {
  readonly jsonrpc: typeof JSON_RPC_VERSION;
  readonly id: JsonRpcId;
  readonly method: string;
  readonly params?: JsonRpcValue;
}

/**
 * JSON-RPC 2.0 success response envelope. The {@link JsonRpcResponse}
 * union keeps result and error mutually exclusive at the type level.
 */
export interface JsonRpcSuccessResponse {
  readonly jsonrpc: typeof JSON_RPC_VERSION;
  readonly id: JsonRpcId;
  readonly result: JsonRpcValue;
}

/**
 * JSON-RPC 2.0 error response envelope. The error object's `data` field
 * is optional and intentionally typed as a JSON value so handlers can
 * surface structured diagnostics.
 */
export interface JsonRpcErrorResponse {
  readonly jsonrpc: typeof JSON_RPC_VERSION;
  readonly id: JsonRpcId;
  readonly error: {
    readonly code: number;
    readonly message: string;
    readonly data?: JsonRpcValue;
  };
}

/**
 * One of the two response shapes the harness ever writes — success or
 * error, never both.
 */
export type JsonRpcResponse = JsonRpcSuccessResponse | JsonRpcErrorResponse;

/**
 * JSON-RPC 2.0 notification — a request without an `id` field. The
 * harness uses notifications for server-to-client streaming events
 * (token deltas, tool-call observations, lifecycle signals). The Go core
 * is the receiver in this direction.
 *
 * Application-level notification methods land in M5.3.b/c/d as the tool
 * runner and the streaming Claude Code wrapper hook in.
 */
export interface JsonRpcNotification {
  readonly jsonrpc: typeof JSON_RPC_VERSION;
  readonly method: string;
  readonly params?: JsonRpcValue;
}

/**
 * Result shape of the `hello` method. Returned verbatim to the caller.
 * The `harness` literal identifies this implementation; future runtime
 * adapters MAY emit different harness identifiers behind the same
 * interface.
 */
export interface HelloResult {
  readonly harness: "watchkeeper";
  readonly version: string;
}

/**
 * Result shape of the `shutdown` method. The harness writes this
 * response, drains the output stream, and then exits the dispatch loop.
 */
export interface ShutdownResult {
  readonly accepted: true;
}
