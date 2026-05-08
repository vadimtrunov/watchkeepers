/**
 * Client wrapper for the Go-side `slack.app_create` JSON-RPC method (M6.1.b).
 *
 * Wraps {@link RpcClient.request} with a typed parameter and return shape so
 * callers (the `slack_app_create` builtin tool in `builtinTools.ts`) get
 * compile-time safety without hand-rolling the method name string or casting
 * the raw {@link JsonRpcValue} result.
 *
 * Mirrors the M5.5.d.a.b notebookClient.ts pattern: one tiny module per
 * Go-side method group keeps the registry side-effect-free and the call
 * sites scannable.
 */

import { type RpcClient } from "./jsonrpc.js";
import type { JsonRpcValue } from "./types.js";

/**
 * Parameters for the `slack.app_create` method. Field names use
 * snake_case to match the Go-side `slackAppCreateParams` decoder in
 * `core/pkg/harnessrpc/slack_app_create.go` and the keepers_log
 * payload keys.
 *
 * Required fields: agent_id, app_name, approval_token. Optional:
 * app_description, scopes. The Go-side handler re-validates each
 * field; this type only shapes the call into the Go decoder's
 * contract.
 */
export interface SlackAppCreateParams {
  readonly agent_id: string;
  readonly app_name: string;
  readonly app_description?: string;
  readonly scopes?: readonly string[];
  readonly approval_token: string;
}

/**
 * Success response shape for `slack.app_create`. Carries the
 * platform-assigned Slack app id verbatim from the Go-side
 * [spawn.CreateAppResult.AppID].
 */
export interface SlackAppCreateResult {
  readonly app_id: string;
}

/**
 * Call `slack.app_create` on the Go-side host and return the
 * platform-assigned app id.
 *
 * Rejects with {@link RpcRequestError} (from `jsonrpc.ts`) when the host
 * returns a JSON-RPC error envelope, preserving the wire `code` so callers
 * can branch on -32005 (ToolUnauthorized) / -32007 (ApprovalRequired) /
 * -32602 (InvalidParams) / -32603 (InternalError) without string-matching
 * the message.
 */
export async function createSlackApp(
  rpc: RpcClient,
  params: SlackAppCreateParams,
): Promise<SlackAppCreateResult> {
  const result = await rpc.request("slack.app_create", params as unknown as JsonRpcValue);
  return result as unknown as SlackAppCreateResult;
}
