/**
 * Built-in `retire_watchkeeper` dispatch tests (M6.2.c).
 *
 * Drives {@link makeInvokeToolHandler} with a stubbed
 * {@link RpcClient.request} (a `vi.fn()` recorder) so the dispatch
 * path can be exercised end-to-end without standing up the
 * bidirectional NDJSON pipe. Asserts:
 *
 *   - happy path: registered name + active agent identity → routes to
 *     `watchmaster.retire_watchkeeper` with snake_case params
 *     (agent_id, approval_token).
 *   - missing agent identity: handler throws
 *     {@link BuiltinAgentIDMissing}, dispatcher maps to
 *     {@link ToolErrorCode.ToolUnauthorized} BEFORE the outbound RPC.
 *   - empty `approval_token` (zod gate): handler throws
 *     {@link BuiltinInvalidInput}, dispatcher maps to
 *     {@link ToolErrorCode.ToolExecutionError}, no outbound RPC.
 *   - empty `agent_id` (zod gate): same as approval_token.
 *   - Go-side ApprovalRequired (-32007) / ToolUnauthorized (-32005)
 *     propagation: dispatcher preserves wire code on rejection.
 *   - non-Watchmaster manifest (toolset omits the tool name):
 *     M5.5.b.a ACL gate rejects with
 *     {@link ToolErrorCode.ToolUnauthorized} BEFORE dispatch.
 *   - unknown wire-level fields: zod `.strict()` rejects, no outbound
 *     RPC.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ToolErrorCode, makeInvokeToolHandler } from "../src/invokeTool.js";
import { RpcRequestError, type RpcClient } from "../src/jsonrpc.js";
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
 * pre-baked to resolve with `result` (or reject with `rejectError`).
 * Mirrors the helpers in the M6.2.b builtin-watchmaster-writes test
 * file.
 */
function makeStubRpc(
  result: JsonRpcValue = {},
  rejectError?: Error,
): {
  rpc: RpcClient;
  request: ReturnType<typeof vi.fn>;
} {
  const request =
    rejectError === undefined
      ? vi.fn().mockResolvedValue(result)
      : vi.fn().mockRejectedValue(rejectError);
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

const validRetireInput = (): JsonRpcValue => ({
  agent_id: "agent-target-uuid",
  approval_token: "approval-token-abc",
});

// ── happy path ───────────────────────────────────────────────────────────────

describe("Builtin_RetireWatchkeeper_RoutesToWatchmasterRetireWatchkeeper", () => {
  it("dispatches watchmaster.retire_watchkeeper with snake_case params", async () => {
    setActiveToolset(["retire_watchkeeper"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({});
    const handler = makeInvokeToolHandler(rpc);

    const out = await handler(makeBuiltinParams("retire_watchkeeper", validRetireInput()));

    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith("watchmaster.retire_watchkeeper", {
      agent_id: "agent-target-uuid",
      approval_token: "approval-token-abc",
    });
    expect(out).toEqual({ output: {} });
  });
});

// ── ACL gate (manifest never set agent identity) ─────────────────────────────

describe("Builtin_RetireWatchkeeper_NoAgentID_RejectsToolUnauthorized", () => {
  it("rejects with ToolUnauthorized when manifest never set agentID", async () => {
    setActiveToolset(["retire_watchkeeper"]);
    // Deliberately do NOT call setActiveAgentID.
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("retire_watchkeeper", validRetireInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    expect(request).not.toHaveBeenCalled();
  });
});

// ── zod gate (empty approval_token / agent_id / strict fields) ──────────────

describe("Builtin_RetireWatchkeeper_EmptyApprovalToken_RejectedLocally", () => {
  it("rejects with ToolExecutionError on empty approval_token (zod gate, no outbound RPC)", async () => {
    setActiveToolset(["retire_watchkeeper"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("retire_watchkeeper", {
          agent_id: "agent-target-uuid",
          approval_token: "",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_RetireWatchkeeper_EmptyAgentID_RejectedLocally", () => {
  it("rejects with ToolExecutionError on empty agent_id (zod gate, no outbound RPC)", async () => {
    setActiveToolset(["retire_watchkeeper"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("retire_watchkeeper", {
          agent_id: "",
          approval_token: "approval-token-abc",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_RetireWatchkeeper_StrictSchema", () => {
  it("rejects unknown wire-level fields (zod .strict())", async () => {
    setActiveToolset(["retire_watchkeeper"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("retire_watchkeeper", {
          agent_id: "agent-target-uuid",
          approval_token: "approval-token-abc",
          rogue_field: "spoofed",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });
    expect(request).not.toHaveBeenCalled();
  });
});

// ── Go-side error code propagation ───────────────────────────────────────────

describe("Builtin_RetireWatchkeeper_GoSideErrorCodes_Preserved", () => {
  it("preserves -32007 ApprovalRequired from Go-side handler", async () => {
    setActiveToolset(["retire_watchkeeper"]);
    setActiveAgentID("agent-watchmaster");
    const goError = new RpcRequestError(
      -32007,
      "watchmaster.retire_watchkeeper: spawn: approval required: empty approval_token",
    );
    const { rpc, request } = makeStubRpc(undefined, goError);
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("retire_watchkeeper", validRetireInput())),
    ).rejects.toMatchObject({ code: -32007 });
    expect(request).toHaveBeenCalledTimes(1);
  });

  it("preserves -32005 ToolUnauthorized from Go-side handler", async () => {
    setActiveToolset(["retire_watchkeeper"]);
    setActiveAgentID("agent-watchmaster");
    const goError = new RpcRequestError(
      -32005,
      "watchmaster.retire_watchkeeper: spawn: unauthorized",
    );
    const { rpc, request } = makeStubRpc(undefined, goError);
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("retire_watchkeeper", validRetireInput())),
    ).rejects.toMatchObject({ code: -32005 });
    expect(request).toHaveBeenCalledTimes(1);
  });
});

// ── M5.5.b.a ACL gate (manifest WITHOUT the tool name) ──────────────────────

describe("Builtin_RetireWatchkeeper_NonWatchmasterManifest_ACLGateRejects", () => {
  it("rejects retire_watchkeeper when toolset omits the tool name", async () => {
    setActiveToolset(["remember"]);
    setActiveAgentID("agent-non-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("retire_watchkeeper", validRetireInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    expect(request).not.toHaveBeenCalled();
  });

  it("rejects retire_watchkeeper when no manifest delivered (deny-by-default)", async () => {
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("retire_watchkeeper", validRetireInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    expect(request).not.toHaveBeenCalled();
  });
});
