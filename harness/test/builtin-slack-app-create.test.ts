/**
 * Built-in `slack_app_create` tool dispatch tests (M6.1.b).
 *
 * Drives {@link makeInvokeToolHandler} with a stubbed
 * {@link RpcClient.request} (a `vi.fn()` recorder) so the dispatch
 * path can be exercised end-to-end without standing up the
 * bidirectional NDJSON pipe. Asserts:
 *
 *   - happy path: registered name + active agent identity → routes
 *     to `slack.app_create` with the correct method name and
 *     params shape (snake_case, agent_id from manifest, scopes
 *     defaulting to []).
 *   - missing agent identity: handler throws
 *     {@link BuiltinAgentIDMissing}, dispatcher maps to
 *     {@link ToolErrorCode.ToolUnauthorized} BEFORE the outbound RPC.
 *   - empty `approval_token` (zod gate): handler throws
 *     {@link BuiltinInvalidInput}, dispatcher maps to
 *     {@link ToolErrorCode.ToolExecutionError}, no outbound RPC.
 *   - Go-side ApprovalRequired (-32007) propagation: the dispatcher
 *     preserves the wire code on a rejected outbound request rather
 *     than flattening to ToolExecutionError.
 *   - Go-side ToolUnauthorized (-32005) propagation: same code-
 *     preservation contract, mirrors the TS-side ACL gate.
 *   - non-Watchmaster manifest (toolset missing `slack_app_create`):
 *     M5.5.b.a ACL gate rejects with {@link ToolErrorCode.ToolUnauthorized}
 *     BEFORE dispatch.
 *   - wire-level `agent_id`: zod schema rejects (audit-spoofing
 *     guard) — the dispatcher injects agent_id from the manifest.
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
 * pre-baked to resolve with `result` (or reject with `rejectError` when
 * provided). Mirrors the helper in `invokeTool-builtin.test.ts` so the
 * two builtin-tool test files share the same shape.
 */
function makeStubRpc(
  result: JsonRpcValue = { app_id: "A0123ABCDEF" },
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

const validInput = (): JsonRpcValue => ({
  app_name: "watchkeeper-bot",
  app_description: "test bot",
  scopes: ["chat:write", "users:read"],
  approval_token: "approval-token-abc123",
});

describe("Builtin_SlackAppCreate_RoutesToSlackAppCreate", () => {
  it("dispatches slack.app_create with manifest agent_id + zod-validated input", async () => {
    setActiveToolset(["slack_app_create"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({ app_id: "A0123ABCDEF" });
    const handler = makeInvokeToolHandler(rpc);

    const out = await handler(makeBuiltinParams("slack_app_create", validInput()));

    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith("slack.app_create", {
      agent_id: "agent-watchmaster",
      app_name: "watchkeeper-bot",
      app_description: "test bot",
      scopes: ["chat:write", "users:read"],
      approval_token: "approval-token-abc123",
    });
    expect(out).toEqual({ output: { app_id: "A0123ABCDEF" } });
  });

  it("defaults scopes to [] when omitted from input", async () => {
    setActiveToolset(["slack_app_create"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({ app_id: "A0123ABCDEF" });
    const handler = makeInvokeToolHandler(rpc);

    await handler(
      makeBuiltinParams("slack_app_create", {
        app_name: "watchkeeper-bot",
        approval_token: "approval-token-abc123",
      }),
    );

    expect(request).toHaveBeenCalledTimes(1);
    const callArgs = request.mock.calls[0]?.[1] as Record<string, unknown>;
    expect(callArgs.scopes).toEqual([]);
    // app_description omitted — must NOT be in the params payload.
    expect("app_description" in callArgs).toBe(false);
  });
});

describe("Builtin_SlackAppCreate_NoAgentID_RejectsToolUnauthorized", () => {
  it("rejects with ToolUnauthorized when manifest never set agentID", async () => {
    setActiveToolset(["slack_app_create"]);
    // Deliberately do NOT call setActiveAgentID.
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("slack_app_create", validInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });

    // Critical: the rejection must short-circuit BEFORE the outbound
    // JSON-RPC call. Otherwise the harness leaks a half-formed
    // `slack.app_create` request the Go side has to reject.
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_SlackAppCreate_EmptyApprovalToken_RejectedLocally", () => {
  it("rejects with ToolExecutionError on empty approval_token (zod gate, no outbound RPC)", async () => {
    setActiveToolset(["slack_app_create"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("slack_app_create", {
          app_name: "watchkeeper-bot",
          approval_token: "",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });

    // The local zod gate must fail-closed BEFORE the outbound RPC so
    // the Go side never burns a tier-2 rate-limit token on a known-bad
    // request.
    expect(request).not.toHaveBeenCalled();
  });

  it("rejects with ToolExecutionError on empty app_name (zod gate, no outbound RPC)", async () => {
    setActiveToolset(["slack_app_create"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("slack_app_create", {
          app_name: "",
          approval_token: "approval-token-abc123",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });

    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_SlackAppCreate_GoSideErrorCodes_Preserved", () => {
  it("preserves -32007 ApprovalRequired from Go-side handler", async () => {
    setActiveToolset(["slack_app_create"]);
    setActiveAgentID("agent-watchmaster");
    const goError = new RpcRequestError(
      -32007,
      "slack.app_create: spawn: approval required: empty approval_token",
    );
    const { rpc, request } = makeStubRpc(undefined, goError);
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("slack_app_create", validInput())),
    ).rejects.toMatchObject({ code: -32007 });

    expect(request).toHaveBeenCalledTimes(1);
  });

  it("preserves -32005 ToolUnauthorized from Go-side handler (e.g. claim missing lead_approval)", async () => {
    setActiveToolset(["slack_app_create"]);
    setActiveAgentID("agent-non-watchmaster");
    const goError = new RpcRequestError(
      -32005,
      "slack.app_create: spawn: unauthorized: claim lacks slack_app_create=lead_approval",
    );
    const { rpc, request } = makeStubRpc(undefined, goError);
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("slack_app_create", validInput())),
    ).rejects.toMatchObject({ code: -32005 });

    expect(request).toHaveBeenCalledTimes(1);
  });
});

describe("Builtin_SlackAppCreate_NonWatchmasterManifest_ACLGateRejects", () => {
  it("rejects with ToolUnauthorized when toolset omits slack_app_create (M5.5.b.a ACL gate)", async () => {
    // Non-Watchmaster manifest — toolset has `remember` but not
    // `slack_app_create`. The M5.5.b.a ACL gate must reject before
    // dispatch reaches the builtin registry.
    setActiveToolset(["remember"]);
    setActiveAgentID("agent-non-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("slack_app_create", validInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });

    // The ACL gate must short-circuit BEFORE the outbound RPC.
    expect(request).not.toHaveBeenCalled();
  });

  it("rejects with ToolUnauthorized when no manifest has been delivered", async () => {
    // Deny-by-default posture — no setActiveToolset call.
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("slack_app_create", validInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });

    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_SlackAppCreate_WireLevelAgentID_Rejected", () => {
  it("rejects with ToolExecutionError when input carries a wire-level agent_id (audit-spoofing guard)", async () => {
    setActiveToolset(["slack_app_create"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    // The zod schema is .strict() so a wire-level `agent_id` field
    // surfaces as a validation error rather than silently overriding
    // the manifest-supplied value.
    await expect(
      handler(
        makeBuiltinParams("slack_app_create", {
          app_name: "watchkeeper-bot",
          agent_id: "spoofed-agent",
          approval_token: "approval-token-abc123",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });

    expect(request).not.toHaveBeenCalled();
  });
});
