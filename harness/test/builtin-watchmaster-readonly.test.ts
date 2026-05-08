/**
 * Built-in `list_watchkeepers` / `report_cost` / `report_health`
 * dispatch tests (M6.2.a).
 *
 * Drives {@link makeInvokeToolHandler} with a stubbed
 * {@link RpcClient.request} (a `vi.fn()` recorder) so the dispatch
 * path can be exercised end-to-end without standing up the
 * bidirectional NDJSON pipe. Asserts:
 *
 *   - happy path per tool: registered name + active agent identity →
 *     routes to the matching `watchmaster.*` Go-side method with the
 *     correct params shape.
 *   - missing agent identity (per tool): handler throws
 *     {@link BuiltinAgentIDMissing}, dispatcher maps to
 *     {@link ToolErrorCode.ToolUnauthorized} BEFORE the outbound RPC.
 *   - non-Watchmaster manifest (toolset missing the read-only tool
 *     names): M5.5.b.a ACL gate rejects with
 *     {@link ToolErrorCode.ToolUnauthorized} BEFORE dispatch reaches
 *     the builtin registry. Mirrors the AC7 ACL-gate test from the
 *     M6.1.b sibling.
 *   - unknown wire-level fields: zod `.strict()` rejects, no outbound
 *     RPC.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ToolErrorCode, makeInvokeToolHandler } from "../src/invokeTool.js";
import { type RpcClient } from "../src/jsonrpc.js";
import {
  __resetActiveToolsetForTests,
  setActiveAgentID,
  setActiveToolset,
} from "../src/manifest.js";
import type { JsonRpcValue } from "../src/types.js";

beforeEach(() => {
  __resetActiveToolsetForTests();
});
afterEach(() => {
  __resetActiveToolsetForTests();
});

/**
 * Build a stubbed {@link RpcClient} whose `request` is a `vi.fn()`
 * pre-baked to resolve with `result`. Mirrors the helpers in the
 * M6.1.b builtin-slack-app-create test file.
 */
function makeStubRpc(result: JsonRpcValue): {
  rpc: RpcClient;
  request: ReturnType<typeof vi.fn>;
} {
  const request = vi.fn().mockResolvedValue(result);
  const rpc = {
    request,
    handleResponse: () => false,
    handleResponseLine: () => ({ kind: "ok", response: { jsonrpc: "2.0", id: 0, result: null } }),
    pendingCount: () => 0,
  } as unknown as RpcClient;
  return { rpc, request };
}

function makeBuiltinParams(name: string, input: JsonRpcValue): JsonRpcValue {
  return { tool: { kind: "builtin", name }, input };
}

// ── list_watchkeepers ────────────────────────────────────────────────────────

describe("Builtin_ListWatchkeepers_RoutesToWatchmasterListWatchkeepers", () => {
  it("dispatches watchmaster.list_watchkeepers with snake_case params", async () => {
    setActiveToolset(["list_watchkeepers"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({
      items: [
        {
          id: "wk-1",
          manifest_id: "mf-1",
          lead_human_id: "h-1",
          status: "active",
          created_at: "2026-05-07T09:00:00Z",
        },
      ],
    });
    const handler = makeInvokeToolHandler(rpc);

    const out = await handler(
      makeBuiltinParams("list_watchkeepers", { status: "active", limit: 10 }),
    );

    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith("watchmaster.list_watchkeepers", {
      status: "active",
      limit: 10,
    });
    // The output preserves the items array verbatim from the stub
    // RPC response; assert by shape rather than `expect.any(Array)`
    // to avoid the @typescript-eslint/no-unsafe-assignment trap.
    const outRecord = out as { output: { items: unknown[] } };
    expect(Array.isArray(outRecord.output.items)).toBe(true);
    expect(outRecord.output.items).toHaveLength(1);
  });

  it("forwards an empty input as a zero-valued request (no filter, no limit)", async () => {
    setActiveToolset(["list_watchkeepers"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({ items: [] });
    const handler = makeInvokeToolHandler(rpc);

    await handler(makeBuiltinParams("list_watchkeepers", {}));

    expect(request).toHaveBeenCalledTimes(1);
    // Empty input → empty params object (the Go-side decoder applies
    // its own defaults).
    expect(request).toHaveBeenCalledWith("watchmaster.list_watchkeepers", {});
  });
});

describe("Builtin_ListWatchkeepers_NoAgentID_RejectsToolUnauthorized", () => {
  it("rejects with ToolUnauthorized when manifest never set agentID", async () => {
    setActiveToolset(["list_watchkeepers"]);
    // Deliberately do NOT call setActiveAgentID.
    const { rpc, request } = makeStubRpc({ items: [] });
    const handler = makeInvokeToolHandler(rpc);

    await expect(handler(makeBuiltinParams("list_watchkeepers", {}))).rejects.toMatchObject({
      code: ToolErrorCode.ToolUnauthorized,
    });
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_ListWatchkeepers_StrictSchema", () => {
  it("rejects unknown wire-level fields (zod .strict())", async () => {
    setActiveToolset(["list_watchkeepers"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({ items: [] });
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("list_watchkeepers", { unknown_field: 1 })),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });
    expect(request).not.toHaveBeenCalled();
  });
});

// ── report_cost ──────────────────────────────────────────────────────────────

describe("Builtin_ReportCost_RoutesToWatchmasterReportCost", () => {
  it("dispatches watchmaster.report_cost with snake_case params", async () => {
    setActiveToolset(["report_cost"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({
      agent_id: "agent-1",
      event_type_prefix: "llm_turn_cost",
      prompt_tokens: 100,
      completion_tokens: 50,
      event_count: 1,
      scanned_rows: 1,
    });
    const handler = makeInvokeToolHandler(rpc);

    const out = await handler(
      makeBuiltinParams("report_cost", {
        agent_id: "agent-1",
        event_type_prefix: "llm_turn_cost",
        limit: 25,
      }),
    );

    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith("watchmaster.report_cost", {
      agent_id: "agent-1",
      event_type_prefix: "llm_turn_cost",
      limit: 25,
    });
    expect(out).toMatchObject({ output: { prompt_tokens: 100, completion_tokens: 50 } });
  });

  it("forwards an empty input as a zero-valued request (org-wide, default prefix)", async () => {
    setActiveToolset(["report_cost"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({
      event_type_prefix: "llm_turn_cost",
      prompt_tokens: 0,
      completion_tokens: 0,
      event_count: 0,
      scanned_rows: 0,
    });
    const handler = makeInvokeToolHandler(rpc);

    await handler(makeBuiltinParams("report_cost", {}));

    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith("watchmaster.report_cost", {});
  });
});

describe("Builtin_ReportCost_NoAgentID_RejectsToolUnauthorized", () => {
  it("rejects with ToolUnauthorized when manifest never set agentID", async () => {
    setActiveToolset(["report_cost"]);
    const { rpc, request } = makeStubRpc({});
    const handler = makeInvokeToolHandler(rpc);

    await expect(handler(makeBuiltinParams("report_cost", {}))).rejects.toMatchObject({
      code: ToolErrorCode.ToolUnauthorized,
    });
    expect(request).not.toHaveBeenCalled();
  });
});

// ── report_health ────────────────────────────────────────────────────────────

describe("Builtin_ReportHealth_RoutesToWatchmasterReportHealth", () => {
  it("dispatches watchmaster.report_health with snake_case params", async () => {
    setActiveToolset(["report_health"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({
      count_pending: 1,
      count_active: 2,
      count_retired: 0,
      count_total: 3,
    });
    const handler = makeInvokeToolHandler(rpc);

    const out = await handler(makeBuiltinParams("report_health", {}));

    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith("watchmaster.report_health", {});
    expect(out).toMatchObject({ output: { count_total: 3 } });
  });

  it("forwards agent_id when set (single-row snapshot mode)", async () => {
    setActiveToolset(["report_health"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({
      item: { id: "wk-1", status: "active" },
      count_pending: 0,
      count_active: 0,
      count_retired: 0,
      count_total: 0,
    });
    const handler = makeInvokeToolHandler(rpc);

    await handler(makeBuiltinParams("report_health", { agent_id: "wk-1" }));

    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith("watchmaster.report_health", { agent_id: "wk-1" });
  });
});

describe("Builtin_ReportHealth_NoAgentID_RejectsToolUnauthorized", () => {
  it("rejects with ToolUnauthorized when manifest never set agentID", async () => {
    setActiveToolset(["report_health"]);
    const { rpc, request } = makeStubRpc({});
    const handler = makeInvokeToolHandler(rpc);

    await expect(handler(makeBuiltinParams("report_health", {}))).rejects.toMatchObject({
      code: ToolErrorCode.ToolUnauthorized,
    });
    expect(request).not.toHaveBeenCalled();
  });
});

// ── AC7 — harness ACL gate (manifest WITHOUT the read-only tool names) ──────

describe("Builtin_WatchmasterReadOnly_NonWatchmasterManifest_ACLGateRejects", () => {
  it("rejects list_watchkeepers when toolset omits the tool name (M5.5.b.a ACL gate)", async () => {
    // Non-Watchmaster manifest — toolset has `remember` but not the
    // M6.2.a read-only tool names. The M5.5.b.a ACL gate must reject
    // before dispatch reaches the builtin registry, so the outbound
    // JSON-RPC request never fires.
    setActiveToolset(["remember"]);
    setActiveAgentID("agent-non-watchmaster");
    const { rpc, request } = makeStubRpc({});
    const handler = makeInvokeToolHandler(rpc);

    await expect(handler(makeBuiltinParams("list_watchkeepers", {}))).rejects.toMatchObject({
      code: ToolErrorCode.ToolUnauthorized,
    });
    expect(request).not.toHaveBeenCalled();
  });

  it("rejects report_cost when toolset omits the tool name", async () => {
    setActiveToolset(["remember", "slack_app_create"]);
    setActiveAgentID("agent-non-watchmaster");
    const { rpc, request } = makeStubRpc({});
    const handler = makeInvokeToolHandler(rpc);

    await expect(handler(makeBuiltinParams("report_cost", {}))).rejects.toMatchObject({
      code: ToolErrorCode.ToolUnauthorized,
    });
    expect(request).not.toHaveBeenCalled();
  });

  it("rejects report_health when toolset omits the tool name", async () => {
    setActiveToolset(["list_watchkeepers"]); // partial Watchmaster toolset
    setActiveAgentID("agent-non-watchmaster");
    const { rpc, request } = makeStubRpc({});
    const handler = makeInvokeToolHandler(rpc);

    await expect(handler(makeBuiltinParams("report_health", {}))).rejects.toMatchObject({
      code: ToolErrorCode.ToolUnauthorized,
    });
    expect(request).not.toHaveBeenCalled();
  });

  it("rejects all three when no manifest has been delivered (deny-by-default)", async () => {
    setActiveAgentID("agent-watchmaster");
    const { rpc } = makeStubRpc({});
    const handler = makeInvokeToolHandler(rpc);

    for (const name of ["list_watchkeepers", "report_cost", "report_health"]) {
      await expect(handler(makeBuiltinParams(name, {}))).rejects.toMatchObject({
        code: ToolErrorCode.ToolUnauthorized,
      });
    }
  });
});
