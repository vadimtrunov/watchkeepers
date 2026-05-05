/**
 * NDJSON line dispatcher — glues {@link parseRequest} to the method
 * registry and writes well-formed JSON-RPC responses.
 *
 * Pure: takes a line in, returns a serialized line out (or `undefined`
 * if the request was a null-id notification with no response). The
 * stdio entry point in `index.ts` owns I/O; this module owns protocol
 * mechanics so it can be tested without spawning a subprocess.
 */

import { errorResponse, parseRequest, serialize, successResponse } from "./jsonrpc.js";
import { dispatch, type MethodRegistry } from "./methods.js";

/**
 * Process a single NDJSON line. Returns the serialized response line
 * (LF-terminated) ready to be written to stdout, or `undefined` when
 * the line was a JSON-RPC notification (no `id` member, per §4.1) — in
 * that case the spec forbids a response even on unknown method or
 * handler error.
 *
 * Errors that occur before the dispatcher can identify the request id
 * (parse error, malformed envelope) are responded to with `id: null`
 * per JSON-RPC 2.0 §5.1.
 */
export async function handleLine(
  registry: MethodRegistry,
  line: string,
): Promise<string | undefined> {
  const parsed = parseRequest(line);

  if (parsed.kind === "error") {
    // InvalidRequest with unrecoverable id — still respond with id null
    // per spec; clients that sent malformed envelopes either get a hint
    // or the connection is unusable anyway.
    return serialize(errorResponse(parsed.id, parsed.code, parsed.message));
  }

  if (parsed.kind === "notification") {
    // JSON-RPC 2.0 §4.1: the server MUST NOT reply to a Notification.
    // Dispatch the method (so handlers like a future `event.tick`
    // observer still fire) and discard the outcome — including
    // MethodNotFound and handler errors.
    const { method, params } = parsed.notification;
    await dispatch(registry, method, params);
    return undefined;
  }

  // JSON-RPC 2.0: requests carry an `id` field (string | number | null).
  // The harness honors the literal envelope and always replies on the
  // request path; only id-less notifications suppress the response.
  const { id, method, params } = parsed.request;
  const outcome = await dispatch(registry, method, params);

  if (outcome.kind === "ok") {
    return serialize(successResponse(id, outcome.result));
  }

  return serialize(errorResponse(id, outcome.code, outcome.message, outcome.data));
}
