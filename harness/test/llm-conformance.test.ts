/**
 * `LLMProvider` conformance suite (M5.7.b).
 *
 * Parameterised vitest suite proving that EVERY concrete
 * {@link LLMProvider} implementation honours the same shape contract for
 * the four lifecycle methods documented in `harness/src/llm/provider.ts`:
 * `complete`, `stream`, `countTokens`, `reportCost`.
 *
 * # Cases
 *
 * Three cases register in the suite:
 *
 *   - `FakeProvider` — always-on; deterministic canned responses.
 *   - `ClaudeCodeProvider` — env-gated. Skipped when
 *     {@link getAnthropicApiKey} returns `undefined` against an
 *     {@link EnvSecretSource}. The test file consumes the M5.7.a
 *     adapter (NOT `process.env` directly) so the grep-invariant
 *     pinned by `core/pkg/llm/anthropic_key_invariant_test.go` keeps
 *     `harness/src/secrets/env.ts` as the sole literal site.
 *   - `ClaudeAgentProvider` — env-gated identically. Skips `countTokens`
 *     per-method (deliberate stub deferred to M5.7.c.c); the remaining
 *     three lifecycle methods run.
 *
 * # Why CONTRACT, not content
 *
 * `ClaudeCodeProvider` runs against a live model and is therefore
 * non-deterministic — exact-content assertions would flake. The suite
 * pins SHAPE (`typeof content === "string"`, `inputTokens` is a
 * non-negative integer, the stream sequence ends with a `message_stop`
 * event preceded by at least one `text_delta`, …) so a future
 * provider swap cannot silently break consumers downstream.
 *
 * # Out of scope
 *
 * Recorded HTTP playback fixtures (deferred), golden outputs, load
 * testing. The suite is the swap-without-touching-core proof.
 */

import { describe, expect, it } from "vitest";

import { ClaudeAgentProvider } from "../src/llm/claude-agent-provider.js";
import { ClaudeCodeProvider } from "../src/llm/claude-code-provider.js";
import { FakeProvider } from "../src/llm/fake-provider.js";
import {
  type CompleteRequest,
  type CountTokensRequest,
  type LLMProvider,
  type Message,
  type StreamEvent,
  type StreamRequest,
  type Usage,
  model,
} from "../src/llm/index.js";
import { EnvSecretSource, getAnthropicApiKey } from "../src/secrets/env.js";

// -- shared scaffolding ----------------------------------------------------

const FAKE_MODEL = model("claude-sonnet-4-5-20250929");
const REAL_MODEL = model("claude-sonnet-4-5-20250929");

const TINY_PROMPT: Message = { role: "user", content: "ping" };

function tinyCompleteReq(m = FAKE_MODEL): CompleteRequest {
  return { model: m, messages: [TINY_PROMPT], maxTokens: 32 };
}

function tinyStreamReq(m = FAKE_MODEL): StreamRequest {
  return { model: m, messages: [TINY_PROMPT], maxTokens: 32 };
}

function tinyCountReq(m = FAKE_MODEL): CountTokensRequest {
  return { model: m, messages: [TINY_PROMPT] };
}

function syntheticUsage(m = FAKE_MODEL): Usage {
  return { model: m, inputTokens: 4, outputTokens: 7, costCents: 0 };
}

/**
 * Pre-seed the fake with a deterministic non-empty completion + a
 * canonical stream sequence (one `text_delta`, one `message_stop`) so
 * the contract assertions have something concrete to inspect. The real
 * provider derives the same shape from the live SDK; the fake is hand-
 * fed identical shape here.
 */
function seededFake(): FakeProvider {
  const fake = new FakeProvider();
  const m = FAKE_MODEL;
  fake.completeResp = {
    content: "pong",
    toolCalls: [],
    finishReason: "stop",
    usage: { model: m, inputTokens: 4, outputTokens: 1, costCents: 0 },
  };
  fake.streamEvents = [
    { kind: "text_delta", textDelta: "po" },
    { kind: "text_delta", textDelta: "ng" },
    {
      kind: "message_stop",
      finishReason: "stop",
      usage: { model: m, inputTokens: 4, outputTokens: 2, costCents: 0 },
    },
  ];
  fake.countTokensResp = 3;
  return fake;
}

type ConformanceMethod = "complete" | "stream" | "countTokens" | "reportCost";

interface ProviderCase {
  readonly name: string;
  readonly factory: () => LLMProvider;
  readonly model: ReturnType<typeof model>;
  readonly skipReason?: string;
  /**
   * Optional list of LLMProvider methods whose conformance test should
   * be skipped for THIS case (whole-case skip uses `skipReason`).
   * Surfaces deliberate-stub gaps without dropping the whole case from
   * the swap-without-touching-core proof.
   */
  readonly skipMethods?: readonly ConformanceMethod[];
}

// `getAnthropicApiKey` returns `undefined` when the env var is unset or
// empty (M5.7.a adapter contract). Truthy `skipReason` triggers
// `describe.skipIf` to skip the whole nested describe; falsy runs it.
const apiKey = getAnthropicApiKey(new EnvSecretSource());
const skipReal = apiKey === undefined ? "no ANTHROPIC API key (M5.7.a adapter)" : "";

const CASES: readonly ProviderCase[] = [
  {
    name: "FakeProvider",
    factory: seededFake,
    model: FAKE_MODEL,
  },
  {
    name: "ClaudeCodeProvider",
    factory: () => {
      // `apiKey` is `string | undefined`. The skipIf gate above prevents
      // this factory from running when undefined; the assertion is for
      // the type system + a defence in depth.
      if (apiKey === undefined) throw new Error("unreachable: skipIf gate failed");
      return new ClaudeCodeProvider({ apiKey });
    },
    model: REAL_MODEL,
    ...(skipReal !== "" ? { skipReason: skipReal } : {}),
  },
  {
    name: "ClaudeAgentProvider",
    factory: () => {
      if (apiKey === undefined) throw new Error("unreachable: skipIf gate failed");
      return new ClaudeAgentProvider({ apiKey });
    },
    model: REAL_MODEL,
    // countTokens is a deliberate stub deferred to M5.7.c.c — surface
    // the gap explicitly rather than letting the conformance assertion
    // discover it via a thrown providerUnavailable error.
    skipMethods: ["countTokens"],
    ...(skipReal !== "" ? { skipReason: skipReal } : {}),
  },
];

// -- conformance assertions -----------------------------------------------

for (const c of CASES) {
  describe.skipIf(c.skipReason !== undefined && c.skipReason !== "")(
    `LLMProvider conformance — ${c.name}`,
    () => {
      const shouldSkip = (method: ConformanceMethod): boolean =>
        c.skipMethods?.includes(method) === true;

      it.skipIf(shouldSkip("complete"))(
        "complete: returns a CompleteResponse with the documented shape (AC4)",
        async () => {
          const provider = c.factory();
          const resp = await provider.complete(tinyCompleteReq(c.model));

          expect(typeof resp.content).toBe("string");
          expect(resp.content.length).toBeGreaterThan(0);
          expect(Array.isArray(resp.toolCalls)).toBe(true);
          expect(["stop", "max_tokens", "tool_use", "error"]).toContain(resp.finishReason);
          expect(typeof resp.usage).toBe("object");
          expect(typeof resp.usage.inputTokens).toBe("number");
          expect(Number.isInteger(resp.usage.inputTokens)).toBe(true);
          expect(resp.usage.inputTokens).toBeGreaterThanOrEqual(0);
          expect(typeof resp.usage.outputTokens).toBe("number");
          expect(Number.isInteger(resp.usage.outputTokens)).toBe(true);
          expect(resp.usage.outputTokens).toBeGreaterThanOrEqual(0);
        },
        30_000,
      );

      it.skipIf(shouldSkip("stream"))(
        "stream: emits >=1 delta-shaped event ending in a message_stop (AC5)",
        async () => {
          const provider = c.factory();
          const events: StreamEvent[] = [];
          const sub = await provider.stream(tinyStreamReq(c.model), (ev) => {
            events.push(ev);
          });
          await sub.stop();

          expect(events.length).toBeGreaterThan(0);
          const last = events[events.length - 1];
          expect(last).toBeDefined();
          expect(last?.kind).toBe("message_stop");

          const deltas = events.filter(
            (e) => e.kind === "text_delta" || e.kind === "tool_call_delta",
          );
          expect(deltas.length).toBeGreaterThan(0);

          // Every event must carry a recognised `kind` (type narrowing
          // pin — exhaustive switch is the contract surface).
          for (const e of events) {
            expect([
              "text_delta",
              "tool_call_start",
              "tool_call_delta",
              "message_stop",
              "error",
            ]).toContain(e.kind);
          }
        },
        60_000,
      );

      it.skipIf(shouldSkip("countTokens"))(
        "countTokens: returns a non-negative integer (AC6)",
        async () => {
          const provider = c.factory();
          const n = await provider.countTokens(tinyCountReq(c.model));

          expect(typeof n).toBe("number");
          expect(Number.isInteger(n)).toBe(true);
          expect(n).toBeGreaterThanOrEqual(0);
        },
        30_000,
      );

      it.skipIf(shouldSkip("reportCost"))(
        "reportCost: accepts a Usage record and resolves cleanly (AC7)",
        async () => {
          const provider = c.factory();
          // Per the interface contract, `reportCost` resolves cleanly for
          // a previously unseen runtimeID — it is the create+update
          // boundary, not a query. Pin the resolution and (where the
          // provider exposes a read-back) the shape of the recorded
          // value. We do NOT assert exact numbers — the FakeProvider
          // returns recorded calls verbatim, the ClaudeCodeProvider
          // accumulates internally, and both are valid shapes.
          await expect(
            provider.reportCost("rt-conformance", syntheticUsage(c.model)),
          ).resolves.toBeUndefined();

          // FakeProvider exposes a recorded-call accessor; the real
          // providers (ClaudeCodeProvider, ClaudeAgentProvider) both
          // expose `getReportedCost(runtimeID)` returning a
          // `Usage | undefined`. Both shapes are asserted in their
          // respective branches without coupling to the recorded-calls
          // accessor.
          if (provider instanceof FakeProvider) {
            const calls = provider.recordedReportCosts();
            expect(calls.length).toBeGreaterThan(0);
            const last = calls[calls.length - 1];
            expect(last?.runtimeID).toBe("rt-conformance");
            expect(typeof last?.usage.inputTokens).toBe("number");
            expect(typeof last?.usage.outputTokens).toBe("number");
          } else if (
            provider instanceof ClaudeCodeProvider ||
            provider instanceof ClaudeAgentProvider
          ) {
            const recorded = provider.getReportedCost("rt-conformance");
            expect(recorded).toBeDefined();
            expect(typeof recorded?.inputTokens).toBe("number");
            expect(typeof recorded?.outputTokens).toBe("number");
          }
        },
        30_000,
      );
    },
  );
}

// -- skipIf mechanism unit test (test-plan row 5) --------------------------
//
// Pins the skipIf wiring itself: when the predicate is truthy, the
// nested describe block does NOT run its `it` bodies. We verify by
// observing a side-effect counter the inner block would bump if it
// ran. If skipIf misbehaved, the counter would be > 0.

let skipIfRanCounter = 0;
describe.skipIf("any truthy reason")("skipIf wiring — must not run", () => {
  it("body that would have run if skipIf were broken", () => {
    skipIfRanCounter += 1;
  });
});

describe("skipIf wiring — meta-assertion", () => {
  it("skip-gated describe blocks do not execute their it bodies", () => {
    expect(skipIfRanCounter).toBe(0);
  });
});

// -- it.skipIf mechanism unit test -----------------------------------------
//
// Pins the it.skipIf per-test wiring itself: when the predicate is truthy,
// the individual `it` body does NOT run. We verify by observing a side-
// effect counter the body would bump if it ran. If it.skipIf misbehaved,
// the counter would be > 0.

let itSkipIfRanCounter = 0;
describe("it.skipIf wiring — must skip per-test", () => {
  it.skipIf(true)("body that would run if it.skipIf were broken", () => {
    itSkipIfRanCounter += 1;
  });
  it("does not execute the skipped it body", () => {
    expect(itSkipIfRanCounter).toBe(0);
  });
});
