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
 * the request was a notification (null id) and the spec forbids a
 * response.
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
    if (parsed.id === null && parsed.code === -32600) {
      // InvalidRequest with unrecoverable id — still respond with id null
      // per spec; clients that sent malformed envelopes either get a hint
      // or the connection is unusable anyway.
    }
    return serialize(errorResponse(parsed.id, parsed.code, parsed.message));
  }

  const { id, method, params } = parsed.request;
  const outcome = await dispatch(registry, method, params);

  // JSON-RPC 2.0: requests with a `null` id are NOT notifications (per
  // spec, notifications omit the id key entirely). The harness honors
  // the literal envelope: if id was sent, respond. We only suppress the
  // response when the method handler itself wishes to remain silent —
  // not implemented in M5.3.a.
  if (outcome.kind === "ok") {
    return serialize(successResponse(id, outcome.result));
  }

  return serialize(errorResponse(id, outcome.code, outcome.message, outcome.data));
}
