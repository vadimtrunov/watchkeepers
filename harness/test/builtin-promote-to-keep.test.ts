/**
 * Built-in `promote_to_keep` dispatch tests (M6.2.d).
 *
 * Drives {@link makeInvokeToolHandler} with a stubbed
 * {@link RpcClient.request} (a `vi.fn()` recorder) so the dispatch
 * path can be exercised end-to-end without standing up the
 * bidirectional NDJSON pipe. Asserts:
 *
 *   - happy path: registered name + active agent identity → routes to
 *     `watchmaster.promote_to_keep` with snake_case params
 *     (agent_id, notebook_entry_id, approval_token).
 *   - missing agent identity: handler throws
 *     {@link BuiltinAgentIDMissing}, dispatcher maps to
 *     {@link ToolErrorCode.ToolUnauthorized} BEFORE the outbound RPC.
 *   - empty `approval_token` (zod gate): handler throws
 *     {@link BuiltinInvalidInput}, dispatcher maps to
 *     {@link ToolErrorCode.ToolExecutionError}, no outbound RPC.
 *   - empty `agent_id` (zod gate): same as approval_token.
 *   - empty `notebook_entry_id` (zod gate): same as approval_token.
 *   - Go-side ApprovalRequired (-32007) / ToolUnauthorized (-32005) /
 *     ToolNotFound (-32011) propagation: dispatcher preserves wire
 *     code on rejection.
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
 * Mirrors the helpers in the M6.2.c builtin-retire-watchkeeper test
 * file.
 */
function makeStubRpc(
  result: JsonRpcValue = {
    chunk_id: "chunk-new",
    proposal_id: "prop-1",
    notebook_entry_id: "entry-1",
  },
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

const validPromoteInput = (): JsonRpcValue => ({
  agent_id: "agent-target-uuid",
  notebook_entry_id: "30000000-0000-7000-8000-000000000000",
  approval_token: "approval-token-abc",
});

// ── happy path ───────────────────────────────────────────────────────────────

describe("Builtin_PromoteToKeep_RoutesToWatchmasterPromoteToKeep", () => {
  it("dispatches watchmaster.promote_to_keep with snake_case params", async () => {
    setActiveToolset(["promote_to_keep"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    const out = await handler(makeBuiltinParams("promote_to_keep", validPromoteInput()));

    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith("watchmaster.promote_to_keep", {
      agent_id: "agent-target-uuid",
      notebook_entry_id: "30000000-0000-7000-8000-000000000000",
      approval_token: "approval-token-abc",
    });
    expect(out).toEqual({
      output: {
        chunk_id: "chunk-new",
        proposal_id: "prop-1",
        notebook_entry_id: "entry-1",
      },
    });
  });
});

// ── ACL gate (manifest never set agent identity) ─────────────────────────────

describe("Builtin_PromoteToKeep_NoAgentID_RejectsToolUnauthorized", () => {
  it("rejects with ToolUnauthorized when manifest never set agentID", async () => {
    setActiveToolset(["promote_to_keep"]);
    // Deliberately do NOT call setActiveAgentID.
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("promote_to_keep", validPromoteInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    expect(request).not.toHaveBeenCalled();
  });
});

// ── zod gate (empty approval_token / agent_id / notebook_entry_id / strict) ──

describe("Builtin_PromoteToKeep_EmptyApprovalToken_RejectedLocally", () => {
  it("rejects with ToolExecutionError on empty approval_token (zod gate, no outbound RPC)", async () => {
    setActiveToolset(["promote_to_keep"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("promote_to_keep", {
          agent_id: "agent-target-uuid",
          notebook_entry_id: "30000000-0000-7000-8000-000000000000",
          approval_token: "",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_PromoteToKeep_EmptyAgentID_RejectedLocally", () => {
  it("rejects with ToolExecutionError on empty agent_id (zod gate, no outbound RPC)", async () => {
    setActiveToolset(["promote_to_keep"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("promote_to_keep", {
          agent_id: "",
          notebook_entry_id: "30000000-0000-7000-8000-000000000000",
          approval_token: "approval-token-abc",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_PromoteToKeep_EmptyNotebookEntryID_RejectedLocally", () => {
  it("rejects with ToolExecutionError on empty notebook_entry_id (zod gate, no outbound RPC)", async () => {
    setActiveToolset(["promote_to_keep"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("promote_to_keep", {
          agent_id: "agent-target-uuid",
          notebook_entry_id: "",
          approval_token: "approval-token-abc",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_PromoteToKeep_StrictSchema", () => {
  it("rejects unknown wire-level fields (zod .strict())", async () => {
    setActiveToolset(["promote_to_keep"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("promote_to_keep", {
          agent_id: "agent-target-uuid",
          notebook_entry_id: "30000000-0000-7000-8000-000000000000",
          approval_token: "approval-token-abc",
          rogue_field: "spoofed",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });
    expect(request).not.toHaveBeenCalled();
  });
});

// ── Go-side error code propagation ───────────────────────────────────────────

describe("Builtin_PromoteToKeep_GoSideErrorCodes_Preserved", () => {
  it("preserves -32007 ApprovalRequired from Go-side handler", async () => {
    setActiveToolset(["promote_to_keep"]);
    setActiveAgentID("agent-watchmaster");
    const goError = new RpcRequestError(
      -32007,
      "watchmaster.promote_to_keep: spawn: approval required: empty approval_token",
    );
    const { rpc, request } = makeStubRpc(undefined, goError);
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("promote_to_keep", validPromoteInput())),
    ).rejects.toMatchObject({ code: -32007 });
    expect(request).toHaveBeenCalledTimes(1);
  });

  it("preserves -32005 ToolUnauthorized from Go-side handler", async () => {
    setActiveToolset(["promote_to_keep"]);
    setActiveAgentID("agent-watchmaster");
    const goError = new RpcRequestError(-32005, "watchmaster.promote_to_keep: spawn: unauthorized");
    const { rpc, request } = makeStubRpc(undefined, goError);
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("promote_to_keep", validPromoteInput())),
    ).rejects.toMatchObject({ code: -32005 });
    expect(request).toHaveBeenCalledTimes(1);
  });

  it("preserves -32011 ToolNotFound from Go-side handler (notebook entry missing)", async () => {
    setActiveToolset(["promote_to_keep"]);
    setActiveAgentID("agent-watchmaster");
    const goError = new RpcRequestError(
      -32011,
      "watchmaster.promote_to_keep: spawn: promote_to_keep load_proposal: notebook: entry not found",
    );
    const { rpc, request } = makeStubRpc(undefined, goError);
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("promote_to_keep", validPromoteInput())),
    ).rejects.toMatchObject({ code: -32011 });
    expect(request).toHaveBeenCalledTimes(1);
  });
});

// ── M5.5.b.a ACL gate (manifest WITHOUT the tool name) ──────────────────────

describe("Builtin_PromoteToKeep_NonWatchmasterManifest_ACLGateRejects", () => {
  it("rejects promote_to_keep when toolset omits the tool name", async () => {
    setActiveToolset(["remember"]);
    setActiveAgentID("agent-non-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("promote_to_keep", validPromoteInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    expect(request).not.toHaveBeenCalled();
  });

  it("rejects promote_to_keep when no manifest delivered (deny-by-default)", async () => {
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("promote_to_keep", validPromoteInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    expect(request).not.toHaveBeenCalled();
  });
});
