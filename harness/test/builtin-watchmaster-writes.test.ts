/**
 * Built-in `propose_spawn` / `adjust_personality` / `adjust_language`
 * dispatch tests (M6.2.b).
 *
 * Drives {@link makeInvokeToolHandler} with a stubbed
 * {@link RpcClient.request} (a `vi.fn()` recorder) so the dispatch
 * path can be exercised end-to-end without standing up the
 * bidirectional NDJSON pipe. Asserts:
 *
 *   - happy path per tool: registered name + active agent identity →
 *     routes to the matching `watchmaster.*` Go-side method with the
 *     correct method name and params shape (snake_case).
 *   - missing agent identity (per tool): handler throws
 *     {@link BuiltinAgentIDMissing}, dispatcher maps to
 *     {@link ToolErrorCode.ToolUnauthorized} BEFORE the outbound RPC.
 *   - empty `approval_token` (zod gate): handler throws
 *     {@link BuiltinInvalidInput}, dispatcher maps to
 *     {@link ToolErrorCode.ToolExecutionError}, no outbound RPC.
 *   - Go-side ApprovalRequired (-32007) propagation: the dispatcher
 *     preserves the wire code on a rejected outbound request.
 *   - Go-side ToolUnauthorized (-32005) propagation: same code-
 *     preservation contract.
 *   - non-Watchmaster manifest (toolset missing the manifest-bump
 *     tool names): M5.5.b.a ACL gate rejects with
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
 * Mirrors the helpers in the M6.1.b builtin-slack-app-create + M6.2.a
 * builtin-watchmaster-readonly test files.
 */
function makeStubRpc(
  result: JsonRpcValue = { manifest_version_id: "mv-new", version_no: 1 },
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

const validProposeInput = (): JsonRpcValue => ({
  agent_id: "agent-target-uuid",
  system_prompt: "you are a watchkeeper",
  personality: "diligent",
  language: "en",
  approval_token: "approval-token-abc",
});

const validAdjustPersonalityInput = (): JsonRpcValue => ({
  agent_id: "agent-target-uuid",
  new_personality: "introspective",
  approval_token: "approval-token-abc",
});

const validAdjustLanguageInput = (): JsonRpcValue => ({
  agent_id: "agent-target-uuid",
  new_language: "fr",
  approval_token: "approval-token-abc",
});

// ── propose_spawn ────────────────────────────────────────────────────────────

describe("Builtin_ProposeSpawn_RoutesToWatchmasterProposeSpawn", () => {
  it("dispatches watchmaster.propose_spawn with snake_case params", async () => {
    setActiveToolset(["propose_spawn"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({ manifest_version_id: "mv-new", version_no: 1 });
    const handler = makeInvokeToolHandler(rpc);

    const out = await handler(makeBuiltinParams("propose_spawn", validProposeInput()));

    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith("watchmaster.propose_spawn", {
      agent_id: "agent-target-uuid",
      system_prompt: "you are a watchkeeper",
      personality: "diligent",
      language: "en",
      approval_token: "approval-token-abc",
    });
    expect(out).toEqual({ output: { manifest_version_id: "mv-new", version_no: 1 } });
  });

  it("omits optional personality + language when absent from input", async () => {
    setActiveToolset(["propose_spawn"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await handler(
      makeBuiltinParams("propose_spawn", {
        agent_id: "agent-target-uuid",
        system_prompt: "sp",
        approval_token: "approval-token-abc",
      }),
    );

    expect(request).toHaveBeenCalledTimes(1);
    const callArgs = request.mock.calls[0]?.[1] as Record<string, unknown>;
    // `personality` and `language` were not in the input — must NOT
    // appear in the outbound params payload.
    expect("personality" in callArgs).toBe(false);
    expect("language" in callArgs).toBe(false);
  });
});

describe("Builtin_ProposeSpawn_NoAgentID_RejectsToolUnauthorized", () => {
  it("rejects with ToolUnauthorized when manifest never set agentID", async () => {
    setActiveToolset(["propose_spawn"]);
    // Deliberately do NOT call setActiveAgentID.
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("propose_spawn", validProposeInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_ProposeSpawn_EmptyApprovalToken_RejectedLocally", () => {
  it("rejects with ToolExecutionError on empty approval_token (zod gate, no outbound RPC)", async () => {
    setActiveToolset(["propose_spawn"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("propose_spawn", {
          agent_id: "agent-target-uuid",
          system_prompt: "sp",
          approval_token: "",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_ProposeSpawn_StrictSchema", () => {
  it("rejects unknown wire-level fields (zod .strict())", async () => {
    setActiveToolset(["propose_spawn"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("propose_spawn", {
          agent_id: "agent-target-uuid",
          system_prompt: "sp",
          approval_token: "approval-token-abc",
          rogue_field: "spoofed",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_ProposeSpawn_GoSideErrorCodes_Preserved", () => {
  it("preserves -32007 ApprovalRequired from Go-side handler", async () => {
    setActiveToolset(["propose_spawn"]);
    setActiveAgentID("agent-watchmaster");
    const goError = new RpcRequestError(
      -32007,
      "watchmaster.propose_spawn: spawn: approval required: empty approval_token",
    );
    const { rpc, request } = makeStubRpc(undefined, goError);
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("propose_spawn", validProposeInput())),
    ).rejects.toMatchObject({ code: -32007 });
    expect(request).toHaveBeenCalledTimes(1);
  });

  it("preserves -32005 ToolUnauthorized from Go-side handler", async () => {
    setActiveToolset(["propose_spawn"]);
    setActiveAgentID("agent-watchmaster");
    const goError = new RpcRequestError(-32005, "watchmaster.propose_spawn: spawn: unauthorized");
    const { rpc, request } = makeStubRpc(undefined, goError);
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("propose_spawn", validProposeInput())),
    ).rejects.toMatchObject({ code: -32005 });
    expect(request).toHaveBeenCalledTimes(1);
  });
});

// ── adjust_personality ───────────────────────────────────────────────────────

describe("Builtin_AdjustPersonality_RoutesToWatchmasterAdjustPersonality", () => {
  it("dispatches watchmaster.adjust_personality with snake_case params", async () => {
    setActiveToolset(["adjust_personality"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({ manifest_version_id: "mv-bumped", version_no: 4 });
    const handler = makeInvokeToolHandler(rpc);

    const out = await handler(
      makeBuiltinParams("adjust_personality", validAdjustPersonalityInput()),
    );

    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith("watchmaster.adjust_personality", {
      agent_id: "agent-target-uuid",
      new_personality: "introspective",
      approval_token: "approval-token-abc",
    });
    expect(out).toEqual({ output: { manifest_version_id: "mv-bumped", version_no: 4 } });
  });

  it("forwards an empty new_personality (round-trips as SQL NULL)", async () => {
    setActiveToolset(["adjust_personality"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await handler(
      makeBuiltinParams("adjust_personality", {
        agent_id: "agent-target-uuid",
        new_personality: "",
        approval_token: "approval-token-abc",
      }),
    );
    const callArgs = request.mock.calls[0]?.[1] as Record<string, unknown>;
    expect(callArgs.new_personality).toBe("");
  });
});

describe("Builtin_AdjustPersonality_NoAgentID_RejectsToolUnauthorized", () => {
  it("rejects with ToolUnauthorized when manifest never set agentID", async () => {
    setActiveToolset(["adjust_personality"]);
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("adjust_personality", validAdjustPersonalityInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_AdjustPersonality_EmptyApprovalToken_RejectedLocally", () => {
  it("rejects with ToolExecutionError on empty approval_token", async () => {
    setActiveToolset(["adjust_personality"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("adjust_personality", {
          agent_id: "agent-target-uuid",
          new_personality: "x",
          approval_token: "",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });
    expect(request).not.toHaveBeenCalled();
  });
});

// ── adjust_language ──────────────────────────────────────────────────────────

describe("Builtin_AdjustLanguage_RoutesToWatchmasterAdjustLanguage", () => {
  it("dispatches watchmaster.adjust_language with snake_case params", async () => {
    setActiveToolset(["adjust_language"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc({ manifest_version_id: "mv-bumped", version_no: 8 });
    const handler = makeInvokeToolHandler(rpc);

    const out = await handler(makeBuiltinParams("adjust_language", validAdjustLanguageInput()));

    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith("watchmaster.adjust_language", {
      agent_id: "agent-target-uuid",
      new_language: "fr",
      approval_token: "approval-token-abc",
    });
    expect(out).toEqual({ output: { manifest_version_id: "mv-bumped", version_no: 8 } });
  });
});

describe("Builtin_AdjustLanguage_NoAgentID_RejectsToolUnauthorized", () => {
  it("rejects with ToolUnauthorized when manifest never set agentID", async () => {
    setActiveToolset(["adjust_language"]);
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("adjust_language", validAdjustLanguageInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_AdjustLanguage_StrictSchema", () => {
  it("rejects unknown wire-level fields (zod .strict())", async () => {
    setActiveToolset(["adjust_language"]);
    setActiveAgentID("agent-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("adjust_language", {
          agent_id: "agent-target-uuid",
          new_language: "fr",
          approval_token: "approval-token-abc",
          unknown_field: 1,
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolExecutionError });
    expect(request).not.toHaveBeenCalled();
  });
});

// ── AC7 — harness ACL gate (manifest WITHOUT the manifest-bump tool names) ──

describe("Builtin_ManifestBump_NonWatchmasterManifest_ACLGateRejects", () => {
  it("rejects propose_spawn when toolset omits the tool name", async () => {
    setActiveToolset(["remember"]);
    setActiveAgentID("agent-non-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("propose_spawn", validProposeInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    expect(request).not.toHaveBeenCalled();
  });

  it("rejects adjust_personality when toolset omits the tool name", async () => {
    setActiveToolset(["remember", "list_watchkeepers"]);
    setActiveAgentID("agent-non-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("adjust_personality", validAdjustPersonalityInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    expect(request).not.toHaveBeenCalled();
  });

  it("rejects adjust_language when toolset omits the tool name", async () => {
    setActiveToolset(["adjust_personality"]); // partial Watchmaster toolset
    setActiveAgentID("agent-non-watchmaster");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("adjust_language", validAdjustLanguageInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    expect(request).not.toHaveBeenCalled();
  });

  it("rejects all three when no manifest has been delivered (deny-by-default)", async () => {
    setActiveAgentID("agent-watchmaster");
    const { rpc } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(makeBuiltinParams("propose_spawn", validProposeInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    await expect(
      handler(makeBuiltinParams("adjust_personality", validAdjustPersonalityInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
    await expect(
      handler(makeBuiltinParams("adjust_language", validAdjustLanguageInput())),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
  });
});
