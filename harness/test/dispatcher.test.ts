/**
 * End-to-end NDJSON round-trip tests — feed a line in, assert the
 * response line out. Exercises the parse → dispatch → serialize chain
 * without touching real stdio.
 */

import { describe, expect, it } from "vitest";

import { handleLine } from "../src/dispatcher.js";
import { HARNESS_VERSION, createDefaultRegistry, type ShutdownSignal } from "../src/methods.js";
import { JSON_RPC_VERSION, JsonRpcErrorCode } from "../src/types.js";

interface JsonRpcSuccessLine {
  jsonrpc: string;
  id: string | number | null;
  result: { harness?: string; version?: string; accepted?: boolean };
}

interface JsonRpcErrorLine {
  jsonrpc: string;
  id: string | number | null;
  error: { code: number; message: string };
}

function decodeLine(line: string | undefined): unknown {
  if (line === undefined) throw new Error("expected a response line");
  expect(line.endsWith("\n")).toBe(true);
  return JSON.parse(line.slice(0, -1));
}

describe("handleLine", () => {
  it("dispatches hello and returns the harness banner", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const line = await handleLine(registry, '{"jsonrpc":"2.0","id":1,"method":"hello"}');
    const decoded = decodeLine(line) as JsonRpcSuccessLine;

    expect(decoded.jsonrpc).toBe(JSON_RPC_VERSION);
    expect(decoded.id).toBe(1);
    expect(decoded.result.harness).toBe("watchkeeper");
    expect(decoded.result.version).toBe(HARNESS_VERSION);
  });

  it("dispatches shutdown and flips the signal", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const line = await handleLine(registry, '{"jsonrpc":"2.0","id":"s1","method":"shutdown"}');
    const decoded = decodeLine(line) as JsonRpcSuccessLine;

    expect(decoded.id).toBe("s1");
    expect(decoded.result.accepted).toBe(true);
    expect(signal.shouldExit).toBe(true);
  });

  it("returns ParseError for malformed JSON", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const line = await handleLine(registry, "not json");
    const decoded = decodeLine(line) as JsonRpcErrorLine;

    expect(decoded.id).toBeNull();
    expect(decoded.error.code).toBe(JsonRpcErrorCode.ParseError);
  });

  it("returns InvalidRequest when jsonrpc field is wrong", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const line = await handleLine(registry, '{"jsonrpc":"1.0","id":7,"method":"hello"}');
    const decoded = decodeLine(line) as JsonRpcErrorLine;

    expect(decoded.error.code).toBe(JsonRpcErrorCode.InvalidRequest);
    // recovered id should round-trip even on InvalidRequest
    expect(decoded.id).toBe(7);
  });

  it("returns MethodNotFound for unknown methods", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const line = await handleLine(registry, '{"jsonrpc":"2.0","id":2,"method":"no.such"}');
    const decoded = decodeLine(line) as JsonRpcErrorLine;

    expect(decoded.id).toBe(2);
    expect(decoded.error.code).toBe(JsonRpcErrorCode.MethodNotFound);
    expect(decoded.error.message).toContain("no.such");
  });

  it("preserves null id on success", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const line = await handleLine(registry, '{"jsonrpc":"2.0","id":null,"method":"hello"}');
    const decoded = decodeLine(line) as JsonRpcSuccessLine;

    expect(decoded.id).toBeNull();
    expect(decoded.result.harness).toBe("watchkeeper");
  });

  it("invokes the handler on a notification but writes nothing to stdout", async () => {
    let invoked = 0;
    const registry = new Map([
      [
        "event.tick",
        () => {
          invoked += 1;
          return null;
        },
      ],
    ]);

    const line = await handleLine(registry, '{"jsonrpc":"2.0","method":"event.tick"}');

    expect(line).toBeUndefined();
    expect(invoked).toBe(1);
  });

  it("silently drops notifications for unknown methods (no stdout)", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const line = await handleLine(registry, '{"jsonrpc":"2.0","method":"no.such.notify"}');

    expect(line).toBeUndefined();
  });

  it("still surfaces ParseError for malformed JSON even when intent is a notification", async () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);

    const line = await handleLine(registry, "not json at all");
    const decoded = decodeLine(line) as JsonRpcErrorLine;

    expect(decoded.id).toBeNull();
    expect(decoded.error.code).toBe(JsonRpcErrorCode.ParseError);
  });
});
