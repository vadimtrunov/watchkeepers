/**
 * Client wrappers for the Go-side `notebook.*` JSON-RPC methods (M5.5.d.a.b).
 *
 * Each function wraps {@link RpcClient.request} with a typed parameter and
 * return shape so callers get compile-time safety without hand-rolling the
 * method name string or casting the raw {@link JsonRpcValue} result.
 */

import { type RpcClient } from "./jsonrpc.js";
import type { JsonRpcValue } from "./types.js";

/**
 * Parameters for the `notebook.remember` method.
 *
 * All four fields are required by the Go-side handler; omitting any field
 * causes a -32602 InvalidParams error from the host.
 */
export interface RememberEntryParams {
  readonly agentID: string;
  readonly category: string;
  readonly subject: string;
  readonly content: string;
}

/**
 * Success response shape for `notebook.remember`.
 */
export interface RememberEntryResult {
  readonly id: string;
}

/**
 * Call `notebook.remember` on the Go-side host and return the persisted
 * entry id.
 *
 * Rejects with {@link RpcRequestError} (from `jsonrpc.ts`) when the host
 * returns a JSON-RPC error envelope, preserving the wire `code` so callers
 * can branch on -32602 / -32603 without string-matching the message.
 */
export async function rememberEntry(
  rpc: RpcClient,
  params: RememberEntryParams,
): Promise<RememberEntryResult> {
  const result = await rpc.request("notebook.remember", params as unknown as JsonRpcValue);
  return result as unknown as RememberEntryResult;
}
