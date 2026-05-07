/**
 * `setManifest` JSON-RPC method tests (M5.5.b.a) — covers the deny-by-
 * default initial state, happy-path activation of a toolset, and the
 * negative cases that surface as JSON-RPC InvalidParams (-32602).
 *
 * The manifest module exposes `getActiveToolset()` / `setActiveToolset()`
 * as plain functions; tests reset the state at the top of each case via
 * a private `__resetActiveToolsetForTests` so module-level state does
 * not leak across the file.
 */

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import {
  __resetActiveToolsetForTests,
  getActiveAgentID,
  getActiveToolset,
  setActiveToolset,
} from "../src/manifest.js";
import { createDefaultRegistry, type ShutdownSignal } from "../src/methods.js";
import { JsonRpcErrorCode, type JsonRpcValue } from "../src/types.js";

beforeEach(() => {
  __resetActiveToolsetForTests();
});
afterEach(() => {
  __resetActiveToolsetForTests();
});

describe("manifest module — initial state", () => {
  it("getActiveToolset() returns undefined before setActiveToolset is called", () => {
    expect(getActiveToolset()).toBeUndefined();
  });

  it("setActiveToolset stores the supplied list and getActiveToolset returns it", () => {
    setActiveToolset(["echo", "sum"]);
    expect(getActiveToolset()).toEqual(["echo", "sum"]);
  });
});

describe("setManifest JSON-RPC method", () => {
  // Async wrapper so a synchronous `throw new MethodError(...)` inside
  // the handler surfaces as a rejected Promise — matches what the
  // dispatcher would observe over the wire and what `expect(...).rejects`
  // can pattern-match against.
  async function callSetManifest(params: JsonRpcValue | undefined): Promise<JsonRpcValue> {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);
    const handler = registry.get("setManifest");
    if (handler === undefined) throw new Error("setManifest handler not registered");
    return await handler(params);
  }

  it("registers as a known method on the default registry", () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);
    expect(registry.has("setManifest")).toBe(true);
  });

  it("sets the active toolset on a valid call and returns { ok: true }", async () => {
    const result = await callSetManifest({ toolset: ["echo", "sum"] });
    expect(result).toEqual({ ok: true });
    expect(getActiveToolset()).toEqual(["echo", "sum"]);
  });

  it("accepts an empty toolset (deny-all)", async () => {
    const result = await callSetManifest({ toolset: [] });
    expect(result).toEqual({ ok: true });
    expect(getActiveToolset()).toEqual([]);
  });

  it("rejects a non-array toolset with InvalidParams (-32602)", async () => {
    await expect(callSetManifest({ toolset: "echo" })).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  it("rejects a toolset containing non-string entries with InvalidParams (-32602)", async () => {
    await expect(callSetManifest({ toolset: [1, 2] })).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  it("rejects a missing toolset key with InvalidParams (-32602)", async () => {
    await expect(callSetManifest({})).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  it("rejects a non-object params payload with InvalidParams (-32602)", async () => {
    await expect(callSetManifest("not an object")).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  it("rejects undefined params with InvalidParams (-32602)", async () => {
    await expect(callSetManifest(undefined)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  // ── M5.5.d.b: agentID extension ─────────────────────────────────────

  it("SetManifest_AcceptsAgentID_Persists", async () => {
    const result = await callSetManifest({ toolset: ["remember"], agentID: "agent-7" });
    expect(result).toEqual({ ok: true });
    expect(getActiveToolset()).toEqual(["remember"]);
    expect(getActiveAgentID()).toBe("agent-7");
  });

  it("SetManifest_OmitsAgentID_StillWorks", async () => {
    // Backward-compat: existing callers omit agentID. The handler
    // must accept the call and leave activeAgentID undefined so
    // built-in tools requiring an identity fail with
    // ToolUnauthorized rather than silently using a stale id.
    const result = await callSetManifest({ toolset: ["echo"] });
    expect(result).toEqual({ ok: true });
    expect(getActiveToolset()).toEqual(["echo"]);
    expect(getActiveAgentID()).toBeUndefined();
  });

  it("clears agentID when a subsequent setManifest call omits it", async () => {
    // First call: identity is set.
    await callSetManifest({ toolset: ["a"], agentID: "agent-9" });
    expect(getActiveAgentID()).toBe("agent-9");

    // Second call: identity is omitted — must clear, not retain.
    // (Otherwise a session reset would leave a stale agentID
    //  associated with a fresh toolset.)
    await callSetManifest({ toolset: ["b"] });
    expect(getActiveToolset()).toEqual(["b"]);
    expect(getActiveAgentID()).toBeUndefined();
  });

  it("rejects a non-string agentID with InvalidParams (-32602)", async () => {
    await expect(callSetManifest({ toolset: [], agentID: 42 })).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });
});
