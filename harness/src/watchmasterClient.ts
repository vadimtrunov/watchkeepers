/**
 * Client wrappers for the M6.2.a Watchmaster read-only JSON-RPC methods:
 *
 *   - `watchmaster.list_watchkeepers`
 *   - `watchmaster.report_cost`
 *   - `watchmaster.report_health`
 *
 * Wraps {@link RpcClient.request} with typed parameter / return shapes
 * so callers (the builtin tools in `builtinTools.ts`) get compile-time
 * safety without hand-rolling method-name strings or casting raw
 * {@link JsonRpcValue} results.
 *
 * Mirrors the M6.1.b slackClient.ts / M5.5.d.a.b notebookClient.ts
 * pattern: one tiny module per Go-side method group keeps the registry
 * side-effect-free and the call sites scannable.
 */

import { type RpcClient } from "./jsonrpc.js";
import type { JsonRpcValue } from "./types.js";

// ── list_watchkeepers ────────────────────────────────────────────────────────

/**
 * Parameters for `watchmaster.list_watchkeepers`. Both fields are
 * optional: empty `status` means "no lifecycle filter", `limit === 0`
 * means "let the server apply its default".
 */
export interface ListWatchkeepersParams {
  readonly status?: string;
  readonly limit?: number;
}

/**
 * Single watchkeeper row in the `list_watchkeepers` response. Mirrors
 * the Go-side `spawn.WatchkeeperRow` shape verbatim — snake_case field
 * names, RFC3339 timestamps, empty strings preserve SQL NULL.
 */
export interface WatchkeeperRow {
  readonly id: string;
  readonly manifest_id: string;
  readonly lead_human_id: string;
  readonly active_manifest_version_id?: string;
  readonly status: string;
  readonly spawned_at?: string;
  readonly retired_at?: string;
  readonly created_at: string;
}

/**
 * Success response shape for `watchmaster.list_watchkeepers`.
 */
export interface ListWatchkeepersResult {
  readonly items: readonly WatchkeeperRow[];
}

/**
 * Call `watchmaster.list_watchkeepers` on the Go-side host and return
 * the projected rows. Rejects with {@link RpcRequestError} on a
 * Go-side failure, preserving the wire `code` for caller dispatch.
 */
export async function listWatchkeepers(
  rpc: RpcClient,
  params: ListWatchkeepersParams,
): Promise<ListWatchkeepersResult> {
  const result = await rpc.request(
    "watchmaster.list_watchkeepers",
    params as unknown as JsonRpcValue,
  );
  return result as unknown as ListWatchkeepersResult;
}

// ── report_cost ──────────────────────────────────────────────────────────────

/**
 * Parameters for `watchmaster.report_cost`. Every field is optional;
 * the Go-side defaults handle the unset case (no agent narrowing,
 * `llm_turn_cost` event prefix, server-default keepers_log scan limit).
 */
export interface ReportCostParams {
  readonly agent_id?: string;
  readonly event_type_prefix?: string;
  readonly limit?: number;
}

/**
 * Success response shape for `watchmaster.report_cost`. Mirrors the
 * Go-side `spawn.ReportCostResult`.
 */
export interface ReportCostResult {
  readonly agent_id?: string;
  readonly event_type_prefix: string;
  readonly prompt_tokens: number;
  readonly completion_tokens: number;
  readonly event_count: number;
  readonly scanned_rows: number;
}

/**
 * Call `watchmaster.report_cost` on the Go-side host and return the
 * aggregated token totals.
 */
export async function reportCost(
  rpc: RpcClient,
  params: ReportCostParams,
): Promise<ReportCostResult> {
  const result = await rpc.request("watchmaster.report_cost", params as unknown as JsonRpcValue);
  return result as unknown as ReportCostResult;
}

// ── report_health ────────────────────────────────────────────────────────────

/**
 * Parameters for `watchmaster.report_health`. Empty `agent_id` →
 * org-wide aggregation; non-empty → single-row snapshot.
 */
export interface ReportHealthParams {
  readonly agent_id?: string;
}

/**
 * Single-row snapshot returned when `report_health` narrows by
 * `agent_id`. Mirrors the Go-side `spawn.WatchkeeperHealth`.
 */
export interface WatchkeeperHealth {
  readonly id: string;
  readonly status: string;
  readonly spawned_at?: string;
  readonly retired_at?: string;
}

/**
 * Success response shape for `watchmaster.report_health`. When the
 * request narrowed by `agent_id`, `item` carries the snapshot and the
 * count fields are zero. When the request was org-wide, `item` is
 * absent and the count fields are populated.
 */
export interface ReportHealthResult {
  readonly item?: WatchkeeperHealth;
  readonly count_pending: number;
  readonly count_active: number;
  readonly count_retired: number;
  readonly count_total: number;
}

/**
 * Call `watchmaster.report_health` on the Go-side host and return the
 * lifecycle snapshot or org-wide counts.
 */
export async function reportHealth(
  rpc: RpcClient,
  params: ReportHealthParams,
): Promise<ReportHealthResult> {
  const result = await rpc.request("watchmaster.report_health", params as unknown as JsonRpcValue);
  return result as unknown as ReportHealthResult;
}
