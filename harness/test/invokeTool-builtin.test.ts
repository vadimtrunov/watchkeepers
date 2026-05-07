/**
 * Built-in tool dispatch tests (M5.5.d.b).
 *
 * Drives {@link makeInvokeToolHandler} with a stubbed
 * {@link RpcClient.request} (a `vi.fn()` recorder) so the dispatch
 * path can be exercised end-to-end without standing up the
 * bidirectional NDJSON pipe. Asserts:
 *
 *   - happy path: registered name + active agent identity → routes
 *     to `notebook.remember` with the correct method name and
 *     params shape.
 *   - missing agent identity: handler throws
 *     {@link BuiltinAgentIDMissing}, dispatcher maps to
 *     {@link ToolErrorCode.ToolUnauthorized}.
 *   - unknown name: dispatcher rejects with
 *     {@link ToolErrorCode.ToolUnknown} BEFORE calling RpcClient.
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
 * pre-baked to resolve with `result`. The remaining surface
 * (`handleResponse`, `handleResponseLine`, `pendingCount`) is stubbed
 * to no-op values — built-in tool dispatch only ever calls
 * `request`.
 */
function makeStubRpc(result: JsonRpcValue = { id: "entry-uuid" }): {
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

describe("Builtin_Remember_RoutesToNotebookRemember", () => {
  it("dispatches notebook.remember with {agentID, category, subject, content}", async () => {
    setActiveToolset(["remember"]);
    setActiveAgentID("agent-42");
    const { rpc, request } = makeStubRpc({ id: "entry-uuid-1" });
    const handler = makeInvokeToolHandler(rpc);

    const out = await handler(
      makeBuiltinParams("remember", {
        category: "lesson",
        subject: "Go testing",
        content: "Use t.TempDir",
      }),
    );

    expect(request).toHaveBeenCalledTimes(1);
    expect(request).toHaveBeenCalledWith("notebook.remember", {
      agentID: "agent-42",
      category: "lesson",
      subject: "Go testing",
      content: "Use t.TempDir",
    });
    expect(out).toEqual({ output: { id: "entry-uuid-1" } });
  });
});

describe("Builtin_Remember_NoAgentID_RejectsToolUnauthorized", () => {
  it("rejects with ToolUnauthorized when manifest never set agentID", async () => {
    setActiveToolset(["remember"]);
    // Deliberately do NOT call setActiveAgentID — the handler must
    // throw BuiltinAgentIDMissing which maps to ToolUnauthorized.
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(
      handler(
        makeBuiltinParams("remember", {
          category: "lesson",
          subject: "",
          content: "irrelevant — never reached",
        }),
      ),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });

    // Critical: the rejection must short-circuit BEFORE the outbound
    // JSON-RPC call. Otherwise the harness leaks a half-formed
    // `notebook.remember` request the Go side has to reject.
    expect(request).not.toHaveBeenCalled();
  });
});

describe("Builtin_Unknown_RejectsToolUnknown", () => {
  it("rejects with ToolUnknown when the name is not in builtinHandlers", async () => {
    setActiveToolset(["mystery"]);
    setActiveAgentID("agent-42");
    const { rpc, request } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    await expect(handler(makeBuiltinParams("mystery", { any: "input" }))).rejects.toMatchObject({
      code: ToolErrorCode.ToolUnknown,
    });

    // No outbound call: registry miss must fail closed before
    // touching the RpcClient.
    expect(request).not.toHaveBeenCalled();
  });
});

describe("invokeTool builtin — wire-shape validation", () => {
  it("rejects builtin tool with missing name as InvalidParams", async () => {
    setActiveToolset(["remember"]);
    setActiveAgentID("agent-42");
    const { rpc } = makeStubRpc();
    const handler = makeInvokeToolHandler(rpc);

    // Wire shape with `kind: "builtin"` but no `name` — validateTool
    // must reject before dispatch.
    const params: JsonRpcValue = { tool: { kind: "builtin" }, input: {} };
    await expect(handler(params)).rejects.toMatchObject({
      // -32602 — JsonRpcErrorCode.InvalidParams
      code: -32602,
    });
  });
});
