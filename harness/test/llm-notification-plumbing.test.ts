/**
 * Notification-writer plumbing test (M5.3.c.c.c.b.a).
 *
 * Pins the wiring contract that lets `runHarness` →
 * `createDefaultRegistry` → `wireLLMMethods` thread a single shared
 * stdout-bound {@link NotificationWriter} closure end-to-end, and
 * verifies the boot-time `harness/ready` notification's wire shape +
 * ordering. Streaming methods (`stream`, `stream/cancel`) are NOT
 * exercised here — they land in M5.3.c.c.c.b.b together with the
 * multi-event protocol that consumes the writer captured by
 * {@link wireLLMMethods}.
 */

import { Readable, Writable } from "node:stream";

import { describe, expect, it } from "vitest";

import { runHarness } from "../src/index.js";
import { FakeProvider } from "../src/llm/fake-provider.js";
import { wireLLMMethods } from "../src/llm/methods.js";
import type { NotificationWriter } from "../src/llm/notification-writer.js";
import {
  HARNESS_VERSION,
  createDefaultRegistry,
  type MethodHandler,
  type ShutdownSignal,
} from "../src/methods.js";
import type { JsonRpcNotification } from "../src/types.js";

class CollectingWritable extends Writable {
  public chunks: string[] = [];

  public override _write(
    chunk: Buffer | string,
    _encoding: BufferEncoding,
    callback: (error?: Error | null) => void,
  ): void {
    this.chunks.push(chunk.toString("utf-8"));
    callback();
  }

  public output(): string {
    return this.chunks.join("");
  }

  public lines(): string[] {
    return this.output()
      .split("\n")
      .filter((line) => line.length > 0);
  }
}

function readableFromLines(lines: readonly string[]): Readable {
  return Readable.from(lines.map((line) => line + "\n"));
}

function emptyStdin(): Readable {
  return Readable.from([]);
}

describe("NotificationWriter type", () => {
  it("accepts a void-returning sink for JsonRpcNotification", () => {
    const calls: JsonRpcNotification[] = [];
    const writer: NotificationWriter = (n) => {
      calls.push(n);
    };
    writer({ jsonrpc: "2.0", method: "ping" });
    writer({ jsonrpc: "2.0", method: "ping", params: { x: 1 } });

    expect(calls).toHaveLength(2);
    expect(calls[0]?.method).toBe("ping");
    expect(calls[1]?.params).toEqual({ x: 1 });
  });
});

describe("wireLLMMethods — writer parameter", () => {
  it("accepts the writer parameter without throwing and still registers the three sync methods", () => {
    const fake = new FakeProvider();
    const registry = new Map<string, MethodHandler>();
    const writer: NotificationWriter = () => {
      // intentionally empty — current handlers do not invoke the writer
      // (deferred to M5.3.c.c.c.b.b).
    };

    expect(() => {
      wireLLMMethods(registry, fake, writer);
    }).not.toThrow();

    expect(registry.has("complete")).toBe(true);
    expect(registry.has("countTokens")).toBe(true);
    expect(registry.has("reportCost")).toBe(true);
    expect(registry.size).toBe(3);
  });

  it("does NOT call the writer from any of the three sync handlers", async () => {
    const fake = new FakeProvider();
    const registry = new Map<string, MethodHandler>();
    let writerCalls = 0;
    const writer: NotificationWriter = () => {
      writerCalls += 1;
    };
    wireLLMMethods(registry, fake, writer);

    const completeHandler = registry.get("complete");
    const countHandler = registry.get("countTokens");
    const reportHandler = registry.get("reportCost");
    expect(completeHandler).toBeDefined();
    expect(countHandler).toBeDefined();
    expect(reportHandler).toBeDefined();
    if (
      completeHandler === undefined ||
      countHandler === undefined ||
      reportHandler === undefined
    ) {
      return;
    }

    await completeHandler({
      model: "claude-sonnet-4-6",
      messages: [{ role: "user", content: "hi" }],
    });
    await countHandler({
      model: "claude-sonnet-4-6",
      messages: [{ role: "user", content: "hi" }],
    });
    await reportHandler({
      runtimeID: "agent-1",
      usage: {
        model: "claude-sonnet-4-6",
        inputTokens: 1,
        outputTokens: 1,
        costCents: 0,
      },
    });

    expect(writerCalls).toBe(0);
  });
});

describe("createDefaultRegistry — writer threading", () => {
  it("baseline (no provider, no writer) registers hello/shutdown/invokeTool only", () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const registry = createDefaultRegistry(signal);
    expect(registry.has("hello")).toBe(true);
    expect(registry.has("shutdown")).toBe(true);
    expect(registry.has("invokeTool")).toBe(true);
    expect(registry.has("complete")).toBe(false);
  });

  it("with provider but without writer still registers the three LLM methods", () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const fake = new FakeProvider();
    const registry = createDefaultRegistry(signal, fake);
    expect(registry.has("complete")).toBe(true);
    expect(registry.has("countTokens")).toBe(true);
    expect(registry.has("reportCost")).toBe(true);
  });

  it("with provider AND writer threads both through to wireLLMMethods", () => {
    const signal: ShutdownSignal = { shouldExit: false };
    const fake = new FakeProvider();
    const writer: NotificationWriter = () => {
      // captured for future stream / stream/cancel handlers.
    };
    const registry = createDefaultRegistry(signal, fake, writer);
    expect(registry.has("complete")).toBe(true);
    expect(registry.has("countTokens")).toBe(true);
    expect(registry.has("reportCost")).toBe(true);
  });
});

describe("runHarness — harness/ready notification", () => {
  it("does NOT emit harness/ready when no provider is supplied", async () => {
    const stdin = emptyStdin();
    const stdout = new CollectingWritable();

    await runHarness(stdin, stdout);

    expect(stdout.output()).toBe("");
  });

  it("emits exactly one harness/ready notification at boot when a provider is supplied", async () => {
    const stdin = emptyStdin();
    const stdout = new CollectingWritable();
    const fake = new FakeProvider();

    await runHarness(stdin, stdout, undefined, fake);

    const lines = stdout.lines();
    expect(lines).toHaveLength(1);
    const firstLine = lines[0];
    if (firstLine === undefined) {
      throw new Error("expected one notification line");
    }

    const envelope = JSON.parse(firstLine) as {
      jsonrpc: string;
      method: string;
      id?: unknown;
      params: { harness: string; version: string; capabilities: readonly string[] };
    };
    expect(envelope.jsonrpc).toBe("2.0");
    expect(envelope.method).toBe("harness/ready");
    // Notifications carry no `id` field per JSON-RPC 2.0 §4.1.
    expect("id" in envelope).toBe(false);
    expect(envelope.params.harness).toBe("watchkeeper");
    expect(envelope.params.version).toBe(HARNESS_VERSION);
    expect(envelope.params.capabilities).toEqual([
      "complete",
      "countTokens",
      "reportCost",
      "stream",
      "stream/cancel",
    ]);
  });

  it("emits harness/ready BEFORE the response to a subsequent hello request", async () => {
    const stdin = readableFromLines(['{"jsonrpc":"2.0","id":1,"method":"hello"}']);
    const stdout = new CollectingWritable();
    const fake = new FakeProvider();

    await runHarness(stdin, stdout, undefined, fake);

    const lines = stdout.lines();
    expect(lines).toHaveLength(2);
    const [readyLine, helloLine] = lines;
    if (readyLine === undefined || helloLine === undefined) {
      throw new Error("expected two output lines");
    }

    const ready = JSON.parse(readyLine) as { method: string };
    expect(ready.method).toBe("harness/ready");

    const hello = JSON.parse(helloLine) as {
      id: number;
      result: { harness: string; version: string };
    };
    expect(hello.id).toBe(1);
    expect(hello.result.harness).toBe("watchkeeper");
  });

  it("does not re-emit harness/ready after a normal request/response cycle", async () => {
    const stdin = readableFromLines([
      '{"jsonrpc":"2.0","id":1,"method":"hello"}',
      '{"jsonrpc":"2.0","id":2,"method":"hello"}',
    ]);
    const stdout = new CollectingWritable();
    const fake = new FakeProvider();

    await runHarness(stdin, stdout, undefined, fake);

    const lines = stdout.lines();
    // 1× harness/ready + 2× hello response = 3 lines total.
    expect(lines).toHaveLength(3);
    const readyHits = lines.filter((line) => {
      const parsed = JSON.parse(line) as { method?: string };
      return parsed.method === "harness/ready";
    });
    expect(readyHits).toHaveLength(1);
  });

  it("emits harness/ready BEFORE a parse-error response when the first stdin line is garbage", async () => {
    const stdin = readableFromLines(["garbage", '{"jsonrpc":"2.0","id":1,"method":"shutdown"}']);
    const stdout = new CollectingWritable();
    const fake = new FakeProvider();

    await runHarness(stdin, stdout, undefined, fake);

    const lines = stdout.lines();
    expect(lines).toHaveLength(3);
    const [readyLine, parseErrLine, shutdownLine] = lines;
    if (readyLine === undefined || parseErrLine === undefined || shutdownLine === undefined) {
      throw new Error("expected three output lines");
    }

    const ready = JSON.parse(readyLine) as { method: string };
    expect(ready.method).toBe("harness/ready");

    const parseErr = JSON.parse(parseErrLine) as { id: null; error: { code: number } };
    expect(parseErr.id).toBeNull();
    expect(parseErr.error.code).toBe(-32700);

    const shutdownOk = JSON.parse(shutdownLine) as { id: number; result: { accepted: boolean } };
    expect(shutdownOk.id).toBe(1);
    expect(shutdownOk.result.accepted).toBe(true);
  });
});
