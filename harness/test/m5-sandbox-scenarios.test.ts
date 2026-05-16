/**
 * M5 sandbox scenarios — B4 verification suite.
 *
 * Closes the three §M5 acceptance bullets that `docs/ROADMAP-phase1.md`
 * pins under DoD Phase B (item B4):
 *
 *   - "A runaway test tool is killed by the wall-clock limit."
 *   - "A tool that tries undeclared network access is rejected by the
 *      capability broker."
 *   - "Replacing `LLMProvider` with `FakeProvider` runs the full harness
 *      suite without code changes outside the provider package."
 *
 * Each bullet has unit-level coverage already (`invokeTool.test.ts`
 * wall-clock cases, `worker-broker.test.ts` net.connect denials, the
 * `llm-conformance.test.ts` swap-without-touching-core suite). This
 * file is the cross-cutting scenario layer: every assertion drives the
 * full `runHarness` stdio loop end-to-end so the M5 bullets read as
 * single, runnable scenarios — mirroring the B2 (`core/internal/m3chain`)
 * and B3 (`ratelimiter_load_test.go`) patterns from the same DoD-closure
 * Phase B.
 *
 * Why scenario-shaped rather than additional unit tests: the M5
 * verification bullets are user-visible behaviour, not implementation
 * details. They MUST pass through the NDJSON wire boundary the Go core
 * actually drives in production (M5.1 supervisor) — a regression that
 * only breaks at the dispatcher layer would slip past per-package unit
 * tests but would fail here loudly.
 */

import { readFileSync } from "node:fs";
import { Readable, Writable } from "node:stream";
import { fileURLToPath } from "node:url";

import { afterEach, beforeEach, describe, expect, it } from "vitest";

import { runHarness } from "../src/index.js";
import { ToolErrorCode } from "../src/invokeTool.js";
// NOTE: imports below come from PROVIDER-NEUTRAL LEAF modules (not the
// `llm/index.js` barrel). The barrel re-exports `ClaudeCodeProvider`, so a
// barrel-shaped import here would defeat the structural meta-test at the
// bottom of this file (the discipline being: this scenario file MUST not
// reach for the real adapter, directly or via a re-export). The barrel
// remains the right entry point for production callers — the rule here is
// scoped to the B4 file.
import { FakeProvider } from "../src/llm/fake-provider.js";
import {
  model as toModel,
  type CompleteResponse,
  type StreamEvent,
  type Usage,
} from "../src/llm/types.js";
import { __resetActiveToolsetForTests } from "../src/manifest.js";
import type { ShutdownSignal } from "../src/methods.js";
import { type JsonRpcValue } from "../src/types.js";

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

interface JsonRpcSuccess {
  readonly jsonrpc: "2.0";
  readonly id: number | string | null;
  readonly result: JsonRpcValue;
}

interface JsonRpcFailure {
  readonly jsonrpc: "2.0";
  readonly id: number | string | null;
  readonly error: { readonly code: number; readonly message: string; readonly data?: JsonRpcValue };
}

interface JsonRpcNotificationLine {
  readonly jsonrpc: "2.0";
  readonly method: string;
  readonly params?: JsonRpcValue;
}

type JsonRpcLine = JsonRpcSuccess | JsonRpcFailure | JsonRpcNotificationLine;

function isSuccess(line: JsonRpcLine): line is JsonRpcSuccess {
  return "result" in line;
}

function isFailure(line: JsonRpcLine): line is JsonRpcFailure {
  return "error" in line;
}

function isNotification(line: JsonRpcLine): line is JsonRpcNotificationLine {
  return "method" in line;
}

class CollectingWritable extends Writable {
  public readonly chunks: string[] = [];

  public override _write(
    chunk: Buffer | string,
    _encoding: BufferEncoding,
    callback: (err?: Error | null) => void,
  ): void {
    this.chunks.push(chunk.toString("utf-8"));
    callback();
  }

  public parsedLines(): JsonRpcLine[] {
    return this.chunks
      .join("")
      .split("\n")
      .filter((s) => s.length > 0)
      .map((s) => JSON.parse(s) as JsonRpcLine);
  }
}

function readableFromRequests(...requests: readonly object[]): Readable {
  return Readable.from(requests.map((r) => JSON.stringify(r) + "\n"));
}

beforeEach(() => {
  __resetActiveToolsetForTests();
});

afterEach(() => {
  __resetActiveToolsetForTests();
});

// ---------------------------------------------------------------------------
// Bullet 1 — runaway tool killed by wall-clock limit
// ---------------------------------------------------------------------------

describe("M5 verification bullet — runaway tool killed by wall-clock limit (B4)", () => {
  it("a `while(true){}` isolated-vm tool surfaces ToolTimeout end-to-end through runHarness", async () => {
    const stdin = readableFromRequests(
      { jsonrpc: "2.0", id: 1, method: "setManifest", params: { toolset: ["runaway"] } },
      {
        jsonrpc: "2.0",
        id: 2,
        method: "invokeTool",
        params: {
          tool: { kind: "isolated-vm", name: "runaway", source: "while(true){}" },
          input: null,
          limits: { wallClockMs: 50 },
        },
      },
      { jsonrpc: "2.0", id: 3, method: "shutdown" },
    );
    const stdout = new CollectingWritable();

    const started = Date.now();
    await runHarness(stdin, stdout);
    const elapsed = Date.now() - started;

    // No notifications expected — provider is absent so no harness/ready
    // fires, and isolated-vm does not emit progress events.
    const responses = stdout.parsedLines();
    expect(responses).toHaveLength(3);

    const setManifestResp = responses[0];
    if (setManifestResp === undefined || !isSuccess(setManifestResp)) {
      throw new Error(`expected setManifest success, got ${JSON.stringify(setManifestResp)}`);
    }
    expect(setManifestResp.id).toBe(1);

    const invokeResp = responses[1];
    if (invokeResp === undefined || !isFailure(invokeResp)) {
      throw new Error(`expected invokeTool failure, got ${JSON.stringify(invokeResp)}`);
    }
    expect(invokeResp.id).toBe(2);
    // The wire surface lifts the typed ToolErrorCode into the JSON-RPC
    // error.code field (it lives in the application range -32000..-32099
    // by design — see `ToolErrorCode` in `invokeTool.ts`).
    expect(invokeResp.error.code).toBe(ToolErrorCode.ToolTimeout);
    expect(invokeResp.error.message).toMatch(/timed out/i);

    const shutdownResp = responses[2];
    if (shutdownResp === undefined || !isSuccess(shutdownResp)) {
      throw new Error(`expected shutdown success, got ${JSON.stringify(shutdownResp)}`);
    }
    expect(shutdownResp.id).toBe(3);

    // Sanity: the wall-clock kill must complete well inside the test
    // budget. 5 s is the per-test default; an outlier here would be a
    // dispatcher hang regression, not just a slow runner.
    expect(elapsed).toBeLessThan(2000);
  }, 5000);
});

// ---------------------------------------------------------------------------
// Bullet 2 — undeclared network access rejected by capability broker
// ---------------------------------------------------------------------------

describe("M5 verification bullet — undeclared net rejected by capability broker (B4)", () => {
  it("a worker tool requesting net.connect against an empty net.allow surfaces ToolCapabilityDenied without spawning", async () => {
    // Empty allow-list across every axis — the request below is a
    // statically-deniable net.connect that the host-side `gateToolInvocation`
    // MUST reject before paying the fork cost (ADR §0001).
    const sealedCaps = {
      fs: { read: [], write: [] },
      net: { allow: [] },
      env: { allow: [] },
      proc: { spawn: false },
    } as const;

    const stdin = readableFromRequests(
      { jsonrpc: "2.0", id: 1, method: "setManifest", params: { toolset: ["sealed-net"] } },
      {
        jsonrpc: "2.0",
        id: 2,
        method: "invokeTool",
        params: {
          tool: {
            kind: "worker",
            name: "sealed-net",
            method: "noop",
            capabilities: sealedCaps,
            requiredOps: [{ kind: "net.connect", host: "api.example.com", port: 443 }],
          },
          input: null,
        },
      },
      { jsonrpc: "2.0", id: 3, method: "shutdown" },
    );
    const stdout = new CollectingWritable();

    const started = Date.now();
    await runHarness(stdin, stdout);
    const elapsed = Date.now() - started;

    const responses = stdout.parsedLines();
    expect(responses).toHaveLength(3);

    const invokeResp = responses[1];
    if (invokeResp === undefined || !isFailure(invokeResp)) {
      throw new Error(`expected invokeTool failure, got ${JSON.stringify(invokeResp)}`);
    }
    expect(invokeResp.id).toBe(2);
    expect(invokeResp.error.code).toBe(ToolErrorCode.ToolCapabilityDenied);
    expect(invokeResp.error.message).toMatch(/net\.connect denied/i);
    expect(invokeResp.error.message).toMatch(/api\.example\.com:443/);

    // The "without paying the fork cost" half of the contract. A real
    // worker fork on this Node runtime costs ~50 ms (cold) / ~20 ms (warm).
    // A 500 ms ceiling rejects any regression that accidentally spawned
    // the worker before the gate ran while leaving ~10× headroom for the
    // dispatcher loop + readline buffering + cold isolated-vm module load
    // on a slow 2-vCPU CI runner.
    expect(elapsed).toBeLessThan(500);
  }, 5000);
});

// ---------------------------------------------------------------------------
// Bullet 3 — FakeProvider drives the full harness loop
// ---------------------------------------------------------------------------

const FAKE_MODEL = toModel("claude-sonnet-4-6");

const ZERO_USAGE: Usage = {
  model: FAKE_MODEL,
  inputTokens: 0,
  outputTokens: 0,
  costCents: 0,
};

function seededFake(): FakeProvider {
  const fake = new FakeProvider();
  const cannedComplete: CompleteResponse = {
    content: "pong",
    toolCalls: [],
    finishReason: "stop",
    usage: { ...ZERO_USAGE, inputTokens: 1, outputTokens: 1 },
  };
  fake.completeResp = cannedComplete;
  fake.countTokensResp = 42;
  const cannedStream: readonly StreamEvent[] = [
    { kind: "text_delta", textDelta: "pong" },
    { kind: "message_stop", finishReason: "stop", usage: ZERO_USAGE },
  ];
  fake.streamEvents = cannedStream;
  return fake;
}

describe("M5 verification bullet — FakeProvider drives the full harness loop (B4)", () => {
  it("complete + countTokens + reportCost + stream all answer with FakeProvider wired into runHarness", async () => {
    const fake = seededFake();
    const stdin = readableFromRequests(
      { jsonrpc: "2.0", id: 1, method: "hello" },
      {
        jsonrpc: "2.0",
        id: 2,
        method: "complete",
        params: { model: FAKE_MODEL, messages: [{ role: "user", content: "ping" }] },
      },
      {
        jsonrpc: "2.0",
        id: 3,
        method: "countTokens",
        params: { model: FAKE_MODEL, messages: [{ role: "user", content: "ping" }] },
      },
      {
        jsonrpc: "2.0",
        id: 4,
        method: "reportCost",
        params: {
          runtimeID: "rt-1",
          usage: { ...ZERO_USAGE, inputTokens: 1, outputTokens: 1 },
        },
      },
      {
        jsonrpc: "2.0",
        id: 5,
        method: "stream",
        params: { model: FAKE_MODEL, messages: [{ role: "user", content: "ping" }] },
      },
      { jsonrpc: "2.0", id: 6, method: "shutdown" },
    );
    const stdout = new CollectingWritable();
    const signal: ShutdownSignal = { shouldExit: false };

    await runHarness(stdin, stdout, signal, fake);

    const lines = stdout.parsedLines();

    // Bucket assertions over total-count: a regression in the bullet
    // under test must trip an exact-shape invariant (6 id-bearing
    // responses, 2 stream/event notifications, ≥1 harness/ready),
    // while a benign expansion of the wire surface (e.g., a future
    // harness/metric heartbeat) must NOT bring this scenario down.
    // A flat `=== 9` total would conflate those two failure modes.
    const responsesByID = new Map<number, JsonRpcSuccess | JsonRpcFailure>();
    const streamNotifications: JsonRpcNotificationLine[] = [];
    const readyNotifications: JsonRpcNotificationLine[] = [];
    for (const line of lines) {
      if (isNotification(line)) {
        if (line.method === "harness/ready") {
          readyNotifications.push(line);
        } else if (line.method === "stream/event") {
          streamNotifications.push(line);
        }
        continue;
      }
      if (typeof line.id === "number") {
        responsesByID.set(line.id, line);
      }
    }
    expect(readyNotifications.length).toBeGreaterThanOrEqual(1);
    expect(responsesByID.size).toBe(6);

    for (const id of [1, 2, 3, 4, 5, 6] as const) {
      const r = responsesByID.get(id);
      if (r === undefined || !isSuccess(r)) {
        throw new Error(`expected success for id ${String(id)}, got ${JSON.stringify(r)}`);
      }
    }
    // FakeProvider mirrors actually returned values across all four LLM
    // methods so a regression that wires the wrong handler shape would
    // surface as a content mismatch here, not just a 200/OK.
    expect((responsesByID.get(2) as JsonRpcSuccess).result).toMatchObject({ content: "pong" });
    expect((responsesByID.get(3) as JsonRpcSuccess).result).toEqual({ inputTokens: 42 });
    expect((responsesByID.get(4) as JsonRpcSuccess).result).toEqual({ accepted: true });

    // Stream notifications: one text_delta, one message_stop, both
    // tagged with the same streamID the `stream` result returned.
    expect(streamNotifications).toHaveLength(2);
    const [first, last] = streamNotifications;
    if (first === undefined || last === undefined) {
      throw new Error("expected two stream/event notifications");
    }
    expect(first.method).toBe("stream/event");
    expect(last.method).toBe("stream/event");
    const streamResult = (responsesByID.get(5) as JsonRpcSuccess).result as { streamID: string };
    expect((first.params as { streamID: string }).streamID).toBe(streamResult.streamID);
    expect((last.params as { streamID: string }).streamID).toBe(streamResult.streamID);

    // FakeProvider call recording is the integrity check for the
    // "wire actually reached the provider" half of the contract.
    expect(fake.recordedCompletes()).toHaveLength(1);
    expect(fake.recordedCountTokens()).toHaveLength(1);
    expect(fake.recordedReportCosts()).toHaveLength(1);
    expect(fake.recordedStreams()).toHaveLength(1);
  }, 5000);

  it("this test file's own imports do not reach for the real adapter (structural swap-discipline guard)", () => {
    // What this proves AND what it does not:
    //
    // The M5 bullet says "Replacing `LLMProvider` with `FakeProvider`
    // runs the full harness suite without code changes outside the
    // provider package". The runtime half is proved above (FakeProvider
    // drives complete+countTokens+reportCost+stream end-to-end through
    // runHarness). The structural half is a *file-local* discipline:
    // this scenario file's OWN import statements MUST NOT name the real
    // adapter, directly or through the `llm/index.js` barrel (which
    // re-exports `ClaudeCodeProvider`). That is what this assertion
    // guards. It DOES NOT prove that the running process never loads
    // `claude-code-provider.ts` — `runHarness` itself imports the
    // adapter eagerly from `harness/src/index.ts`, so the class is
    // always in the module graph. The roadmap bullet is about
    // SOURCE-LEVEL provider-swap mechanics ("without code changes
    // outside the provider package"), not about the transitive runtime
    // module graph.
    //
    // Regex shape: anchor at line start and require the line to begin
    // with `import` or `export` so a prose example inside a docstring
    // like `// example: import X from "claude-code-provider"` does not
    // trip the guard. The two forbidden specifier substrings are the
    // leaf path AND the barrel path — both would transitively pull the
    // real adapter into this file's binding graph.
    const selfPath = fileURLToPath(import.meta.url);
    const selfSource = readFileSync(selfPath, "utf-8");
    const importRegex = /^\s*(?:import|export)[^"';]*from\s+["']([^"']+)["']/gm;
    const importSpecifiers: string[] = [];
    for (const m of selfSource.matchAll(importRegex)) {
      const spec = m[1];
      if (spec !== undefined) importSpecifiers.push(spec);
    }
    expect(importSpecifiers.length).toBeGreaterThan(0);
    for (const spec of importSpecifiers) {
      expect(spec).not.toMatch(/claude-code-provider/);
      // The `llm/index.js` barrel re-exports `ClaudeCodeProvider`, so a
      // barrel-shaped import here would smuggle the real adapter symbol
      // into this file's bindings even though the path does not contain
      // the literal `claude-code-provider` substring.
      expect(spec).not.toMatch(/\/llm\/index\.js$/);
      expect(spec).not.toMatch(/\/llm["']$/);
    }
  });
});
