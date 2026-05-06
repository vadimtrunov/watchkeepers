/**
 * `invokeTool` toolset ACL gate tests (M5.5.b.a).
 *
 * Asserts the deny-by-default posture (no `setManifest` ever called →
 * every `invokeTool` rejects with `ToolUnauthorized`), the happy path
 * (registered names dispatch through to the existing isolated-vm
 * runner), and the negative path (unknown names reject BEFORE the
 * dispatcher routes the call). The dispatcher non-invocation contract
 * is exercised by spying on `runIsolatedJs`.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import * as invokeToolModule from "../src/invokeTool.js";
import { ToolErrorCode, invokeToolHandler } from "../src/invokeTool.js";
import { __resetActiveToolsetForTests, setActiveToolset } from "../src/manifest.js";
import type { JsonRpcValue } from "../src/types.js";

beforeEach(() => {
  __resetActiveToolsetForTests();
});
afterEach(() => {
  __resetActiveToolsetForTests();
  vi.restoreAllMocks();
});

function makeIsolatedParams(name: string, source: string, input: JsonRpcValue): JsonRpcValue {
  return { tool: { kind: "isolated-vm", name, source }, input };
}

describe("invokeTool — toolset ACL gate (M5.5.b.a)", () => {
  it("denies all when setManifest has never been called (deny-by-default)", async () => {
    const spy = vi.spyOn(invokeToolModule, "runIsolatedJs");

    await expect(
      invokeToolHandler(makeIsolatedParams("echo", "return input;", { x: 1 })),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });

    expect(spy).not.toHaveBeenCalled();
  });

  it("denies all when setActiveToolset was called with an empty list", async () => {
    setActiveToolset([]);
    const spy = vi.spyOn(invokeToolModule, "runIsolatedJs");

    await expect(
      invokeToolHandler(makeIsolatedParams("echo", "return input;", { x: 1 })),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });

    expect(spy).not.toHaveBeenCalled();
  });

  it("allows registered names to dispatch through to runIsolatedJs", async () => {
    setActiveToolset(["echo"]);
    const out = await invokeToolHandler(
      makeIsolatedParams("echo", "return input.a + input.b;", { a: 2, b: 3 }),
    );
    expect(out).toEqual({ output: 5 });
  });

  it("rejects unknown tool names with ToolUnauthorized and never calls the dispatcher", async () => {
    setActiveToolset(["echo"]);
    const spy = vi.spyOn(invokeToolModule, "runIsolatedJs");

    await expect(
      invokeToolHandler(makeIsolatedParams("delete_universe", "return input;", null)),
    ).rejects.toMatchObject({ code: ToolErrorCode.ToolUnauthorized });

    expect(spy).not.toHaveBeenCalled();
  });
});
