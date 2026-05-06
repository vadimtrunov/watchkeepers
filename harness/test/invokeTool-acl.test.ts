/**
 * `invokeTool` toolset ACL gate tests (M5.5.b.a).
 *
 * Asserts the deny-by-default posture (no `setManifest` ever called →
 * every `invokeTool` rejects with `ToolUnauthorized`), the happy path
 * (registered names dispatch through to the existing isolated-vm
 * runner), and the negative path (unknown names reject BEFORE the
 * dispatcher routes the call).
 *
 * Dispatcher non-invocation is proven structurally: the rejected
 * `source` is deliberately broken JavaScript that isolated-vm would
 * reject with `ToolExecutionError` if it were ever reached. The fact
 * that the error code is `ToolUnauthorized` (not `ToolExecutionError`)
 * proves the ACL short-circuited before `runIsolatedJs` was called —
 * no spy needed and no ESM-namespace-binding caveat to reason about.
 *
 * Worker-kind cases cover the `tool.name ?? tool.method` ACL fallback:
 *   - `method` only (no `name`) → gate uses `method` as the lookup key.
 *   - `name` present alongside `method` → gate uses `name`, ignoring
 *     `method`, so `name: "allowed", method: "forbidden"` is permitted
 *     while `name: "forbidden", method: "allowed"` is denied.
 */

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { ToolErrorCode, invokeToolHandler } from "../src/invokeTool.js";
import { __resetActiveToolsetForTests, setActiveToolset } from "../src/manifest.js";
import { MethodError } from "../src/methods.js";
import type { JsonRpcValue } from "../src/types.js";

beforeEach(() => {
  __resetActiveToolsetForTests();
});
afterEach(() => {
  __resetActiveToolsetForTests();
});

/** Minimal empty capabilities accepted by the wire validator. */
const EMPTY_CAPS = {
  fs: { read: [], write: [] },
  net: { allow: [] },
  env: { allow: [] },
  proc: { spawn: false },
};

function makeIsolatedParams(name: string, source: string, input: JsonRpcValue): JsonRpcValue {
  return { tool: { kind: "isolated-vm", name, source }, input };
}

/**
 * A source string that isolated-vm would reject with `ToolExecutionError`
 * if it were ever compiled. Used as structural proof that the ACL gate
 * short-circuited before the dispatcher was reached: if the rejection
 * carries `ToolUnauthorized` (not `ToolExecutionError`) the dispatcher
 * was never invoked.
 */
const BROKEN_SOURCE = "throw new SyntaxError('should not parse');";

function makeWorkerParams(method: string, input: JsonRpcValue, name?: string): JsonRpcValue {
  const tool: Record<string, JsonRpcValue> = {
    kind: "worker",
    method,
    capabilities: EMPTY_CAPS,
  };
  if (name !== undefined) tool.name = name;
  return { tool, input };
}

describe("invokeTool — toolset ACL gate (M5.5.b.a)", () => {
  // ── isolated-vm kind ────────────────────────────────────────────────

  it("denies all when setManifest has never been called (deny-by-default)", async () => {
    // BROKEN_SOURCE would cause ToolExecutionError if executed; receiving
    // ToolUnauthorized proves the gate fired before runIsolatedJs.
    await expect(
      invokeToolHandler(makeIsolatedParams("echo", BROKEN_SOURCE, { x: 1 })),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
  });

  it("denies all when setActiveToolset was called with an empty list", async () => {
    setActiveToolset([]);
    await expect(
      invokeToolHandler(makeIsolatedParams("echo", BROKEN_SOURCE, { x: 1 })),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
  });

  it("allows registered names to dispatch through to runIsolatedJs", async () => {
    setActiveToolset(["echo"]);
    const out = await invokeToolHandler(
      makeIsolatedParams("echo", "return input.a + input.b;", { a: 2, b: 3 }),
    );
    expect(out).toEqual({ output: 5 });
  });

  it("rejects unknown tool names with ToolUnauthorized and never reaches the dispatcher", async () => {
    setActiveToolset(["echo"]);
    // BROKEN_SOURCE would cause ToolExecutionError if executed; receiving
    // ToolUnauthorized proves short-circuit before runIsolatedJs.
    await expect(
      invokeToolHandler(makeIsolatedParams("delete_universe", BROKEN_SOURCE, null)),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
  });

  // ── worker kind — method/name ACL fallback ──────────────────────────

  it("rejects worker tool when method is not in the active toolset (method used as key)", async () => {
    setActiveToolset(["allowed"]);
    await expect(invokeToolHandler(makeWorkerParams("forbidden", null))).rejects.toMatchObject({
      code: ToolErrorCode.ToolUnauthorized,
    });
  });

  it("allows worker tool when method equals the registered name (method used as key)", async () => {
    setActiveToolset(["allowed"]);
    // The worker would be spawned; to avoid forking a real process we
    // only assert the error is NOT ToolUnauthorized — any execution-level
    // error means the ACL passed and the dispatcher was reached.
    let caught: unknown;
    try {
      await invokeToolHandler(makeWorkerParams("allowed", null));
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(MethodError);
    if (caught instanceof MethodError) {
      expect(caught.code).not.toBe(ToolErrorCode.ToolUnauthorized);
    }
  });

  it("name wins over method: name in toolset allows the call even when method is not", async () => {
    setActiveToolset(["allowed"]);
    // name="allowed" is registered; method="forbidden" is not — name wins.
    let caught: unknown;
    try {
      await invokeToolHandler(makeWorkerParams("forbidden", null, "allowed"));
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(MethodError);
    if (caught instanceof MethodError) {
      expect(caught.code).not.toBe(ToolErrorCode.ToolUnauthorized);
    }
  });

  it("name wins over method: name not in toolset denies the call even when method is", async () => {
    setActiveToolset(["allowed"]);
    // name="forbidden" is not registered; method="allowed" is — name wins, so denied.
    await expect(
      invokeToolHandler(makeWorkerParams("allowed", null, "forbidden")),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });
  });
});
