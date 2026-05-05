/**
 * `invokeTool` JSON-RPC method — pure-JS sandbox path tests.
 *
 * Drives both the handler directly and the dispatcher entry point so the
 * wire-shape contract (AC1) and the in-process timeout / sandbox-isolation
 * contract (AC4 / AC5) are exercised end-to-end without spawning a
 * subprocess. Real wall-clock timeouts are used because `isolated-vm`
 * runs in C++ — `vi.useFakeTimers()` cannot interrupt a C++ tight loop.
 */

import { describe, expect, it } from "vitest";

import { handleLine } from "../src/dispatcher.js";
import { ToolErrorCode, invokeToolHandler } from "../src/invokeTool.js";
import { createDefaultRegistry, type ShutdownSignal } from "../src/methods.js";
import { JSON_RPC_VERSION, JsonRpcErrorCode, type JsonRpcValue } from "../src/types.js";

interface JsonRpcSuccessLine {
  jsonrpc: string;
  id: string | number | null;
  result: { output: JsonRpcValue };
}

interface JsonRpcErrorLine {
  jsonrpc: string;
  id: string | number | null;
  error: { code: number; message: string; data?: JsonRpcValue };
}

function decodeLine(line: string | undefined): unknown {
  if (line === undefined) throw new Error("expected a response line");
  expect(line.endsWith("\n")).toBe(true);
  return JSON.parse(line.slice(0, -1));
}

function makeParams(
  source: string,
  input: JsonRpcValue,
  limits?: { wallClockMs?: number; memoryMb?: number },
): JsonRpcValue {
  return limits === undefined
    ? { tool: { kind: "isolated-vm", source }, input }
    : {
        tool: { kind: "isolated-vm", source },
        input,
        limits: {
          ...(limits.wallClockMs === undefined ? {} : { wallClockMs: limits.wallClockMs }),
          ...(limits.memoryMb === undefined ? {} : { memoryMb: limits.memoryMb }),
        },
      };
}

describe("invokeToolHandler — happy paths", () => {
  it("returns identity output for `return input;`", async () => {
    const out = await invokeToolHandler(makeParams("return input;", { x: 1 }));
    expect(out).toEqual({ output: { x: 1 } });
  });

  it("computes arithmetic on object input", async () => {
    const out = await invokeToolHandler(makeParams("return input.a * input.b;", { a: 2, b: 3 }));
    expect(out).toEqual({ output: 6 });
  });

  it("computes addition (AC2 verbatim)", async () => {
    const out = await invokeToolHandler(makeParams("return input.a + input.b;", { a: 2, b: 3 }));
    expect(out).toEqual({ output: 5 });
  });

  it("round-trips a nested object verbatim", async () => {
    const input = { deep: { arr: [1, 2, 3] }, s: "x" };
    const out = await invokeToolHandler(makeParams("return input;", input));
    expect(out).toEqual({ output: input });
  });
});

describe("invokeToolHandler — input edge cases", () => {
  it("round-trips null input", async () => {
    const out = await invokeToolHandler(makeParams("return input;", null));
    expect(out).toEqual({ output: null });
  });

  it("round-trips a primitive number", async () => {
    const out = await invokeToolHandler(makeParams("return input;", 42));
    expect(out).toEqual({ output: 42 });
  });

  it("round-trips a primitive string", async () => {
    const out = await invokeToolHandler(makeParams("return input;", "hello"));
    expect(out).toEqual({ output: "hello" });
  });

  it("normalizes an undefined return to null", async () => {
    const out = await invokeToolHandler(makeParams("return undefined;", null));
    expect(out).toEqual({ output: null });
  });
});

describe("invokeToolHandler — execution errors", () => {
  it("translates a thrown Error to ToolExecutionError with the original message", async () => {
    await expect(
      invokeToolHandler(makeParams('throw new Error("nope");', null)),
    ).rejects.toMatchObject({
      name: "MethodError",
      code: ToolErrorCode.ToolExecutionError,
      message: expect.stringContaining("nope") as unknown,
    });
  });

  it("translates a runtime ReferenceError (require not defined) to ToolExecutionError", async () => {
    await expect(invokeToolHandler(makeParams('require("fs");', null))).rejects.toMatchObject({
      code: ToolErrorCode.ToolExecutionError,
    });
  });

  it("translates `process.exit(0)` (process is undefined) to ToolExecutionError", async () => {
    await expect(
      invokeToolHandler(makeParams("globalThis.process.exit(0);", null)),
    ).rejects.toMatchObject({
      code: ToolErrorCode.ToolExecutionError,
    });
  });
});

describe("invokeToolHandler — wall-clock timeout", () => {
  it("kills `while(true){}` with limits.wallClockMs = 50 and surfaces ToolTimeout", async () => {
    const start = Date.now();
    await expect(
      invokeToolHandler(makeParams("while(true){}", null, { wallClockMs: 50 })),
    ).rejects.toMatchObject({
      code: ToolErrorCode.ToolTimeout,
    });
    const elapsed = Date.now() - start;
    expect(elapsed).toBeLessThan(2000);
  }, 5000);

  it("enforces the default wall-clock timeout when limits is omitted", async () => {
    // Default is 1000 ms; assert the call does not hang the test runner
    // and that the surfaced error is a ToolTimeout.
    const start = Date.now();
    await expect(invokeToolHandler(makeParams("while(true){}", null))).rejects.toMatchObject({
      code: ToolErrorCode.ToolTimeout,
    });
    const elapsed = Date.now() - start;
    expect(elapsed).toBeLessThan(5000);
  }, 10000);
});

describe("invokeToolHandler — sandbox isolation (AC5)", () => {
  it("reports `typeof process === 'undefined'` inside the isolate", async () => {
    const out = await invokeToolHandler(makeParams("return typeof process;", null));
    expect(out).toEqual({ output: "undefined" });
  });

  it("confirms process / require / fetch are all absent in one assertion", async () => {
    const source = `return (
      typeof process === "undefined" &&
      typeof require === "undefined" &&
      typeof globalThis.fetch === "undefined"
    );`;
    const out = await invokeToolHandler(makeParams(source, null));
    expect(out).toEqual({ output: true });
  });
});

describe("invokeToolHandler — InvalidParams (AC6)", () => {
  it("rejects a missing `tool` key with InvalidParams", async () => {
    const params: JsonRpcValue = { input: null };
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  it("rejects an unsupported `tool.kind` (e.g. worker-process) with InvalidParams", async () => {
    const params: JsonRpcValue = {
      tool: { kind: "worker-process", source: "return 1;" },
      input: null,
    };
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  it("rejects a non-string `tool.source` with InvalidParams", async () => {
    const params: JsonRpcValue = {
      tool: { kind: "isolated-vm", source: 123 },
      input: null,
    };
    await expect(invokeToolHandler(params)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  it("rejects a non-object params payload with InvalidParams", async () => {
    await expect(invokeToolHandler("not an object")).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });

  it("rejects undefined params with InvalidParams", async () => {
    await expect(invokeToolHandler(undefined)).rejects.toMatchObject({
      code: JsonRpcErrorCode.InvalidParams,
    });
  });
});

describe("invokeTool — dispatcher integration", () => {
  it("routes a JSON-RPC request through handleLine and returns the success envelope", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const request = JSON.stringify({
      jsonrpc: JSON_RPC_VERSION,
      id: 11,
      method: "invokeTool",
      params: {
        tool: { kind: "isolated-vm", source: "return input.a + input.b;" },
        input: { a: 2, b: 3 },
      },
    });

    const line = await handleLine(registry, request);
    const decoded = decodeLine(line) as JsonRpcSuccessLine;

    expect(decoded.jsonrpc).toBe(JSON_RPC_VERSION);
    expect(decoded.id).toBe(11);
    expect(decoded.result).toEqual({ output: 5 });
  });

  it("surfaces ToolExecutionError through the dispatcher error envelope", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const request = JSON.stringify({
      jsonrpc: JSON_RPC_VERSION,
      id: "err-1",
      method: "invokeTool",
      params: {
        tool: { kind: "isolated-vm", source: 'throw new Error("nope");' },
        input: null,
      },
    });

    const line = await handleLine(registry, request);
    const decoded = decodeLine(line) as JsonRpcErrorLine;

    expect(decoded.id).toBe("err-1");
    expect(decoded.error.code).toBe(ToolErrorCode.ToolExecutionError);
    expect(decoded.error.message).toContain("nope");
  });

  it("surfaces InvalidParams through the dispatcher error envelope when shape is wrong", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const request = JSON.stringify({
      jsonrpc: JSON_RPC_VERSION,
      id: 12,
      method: "invokeTool",
      params: { tool: { kind: "worker-process", source: "return 1;" }, input: null },
    });

    const line = await handleLine(registry, request);
    const decoded = decodeLine(line) as JsonRpcErrorLine;

    expect(decoded.id).toBe(12);
    expect(decoded.error.code).toBe(JsonRpcErrorCode.InvalidParams);
  });
});
