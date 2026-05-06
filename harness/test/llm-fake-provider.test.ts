/**
 * `FakeProvider` vitest suite — covers the harness LLM provider surface
 * introduced in M5.3.c.c.a.
 *
 * Mirrors the structure of the Go fake's behavioural assertions in
 * `core/pkg/llm/fake_provider_test.go`: happy paths first, edge paths
 * (idempotent stop, handler-error short-circuit, deterministic synthetic
 * token count), then negative paths covering every error sentinel the
 * Go contract documents. One type-only test pins the discriminated
 * union narrowing and one isolation test pins defensive-copy semantics.
 */

import { describe, expect, it } from "vitest";

import {
  FakeProvider,
  LLMError,
  type CompleteRequest,
  type CompleteResponse,
  type CountTokensRequest,
  type Message,
  type StreamEvent,
  type StreamEventKind,
  type StreamRequest,
  type ToolDefinition,
  type Usage,
  model,
} from "../src/llm/index.js";

const MODEL = model("claude-sonnet-4");

const SYSTEM_PROMPT = "you are a watchkeeper";

const USER_MSG: Message = { role: "user", content: "ping" };

function basicCompleteReq(overrides?: Partial<CompleteRequest>): CompleteRequest {
  return {
    model: MODEL,
    system: SYSTEM_PROMPT,
    messages: [USER_MSG],
    ...overrides,
  };
}

function basicStreamReq(overrides?: Partial<StreamRequest>): StreamRequest {
  return {
    model: MODEL,
    system: SYSTEM_PROMPT,
    messages: [USER_MSG],
    ...overrides,
  };
}

function basicCountReq(overrides?: Partial<CountTokensRequest>): CountTokensRequest {
  return {
    model: MODEL,
    system: SYSTEM_PROMPT,
    messages: [USER_MSG],
    ...overrides,
  };
}

const ZERO_USAGE: Usage = {
  model: MODEL,
  inputTokens: 0,
  outputTokens: 0,
  costCents: 0,
};

describe("FakeProvider — complete (happy)", () => {
  it("returns the canned response verbatim and records the request", async () => {
    const fake = new FakeProvider();
    const canned: CompleteResponse = {
      content: "pong",
      toolCalls: [],
      finishReason: "stop",
      usage: { ...ZERO_USAGE, inputTokens: 1, outputTokens: 1 },
    };
    fake.completeResp = canned;

    const req = basicCompleteReq();
    const got = await fake.complete(req);

    expect(got).toEqual(canned);
    expect(fake.recordedCompletes()).toEqual([req]);
  });
});

describe("FakeProvider — stream (happy)", () => {
  it("dispatches every configured event to the handler in arrival order before resolving", async () => {
    const fake = new FakeProvider();
    const events: StreamEvent[] = [
      { kind: "text_delta", textDelta: "po" },
      { kind: "text_delta", textDelta: "ng" },
      {
        kind: "message_stop",
        finishReason: "stop",
        usage: { ...ZERO_USAGE, inputTokens: 2, outputTokens: 2 },
      },
    ];
    fake.streamEvents = events;

    const seen: StreamEvent[] = [];
    const sub = await fake.stream(basicStreamReq(), (ev) => {
      seen.push(ev);
    });

    expect(seen).toEqual(events);
    expect(fake.recordedStreams()).toHaveLength(1);
    await expect(sub.stop()).resolves.toBeUndefined();
  });
});

describe("FakeProvider — countTokens (happy)", () => {
  it("returns the deterministic synthetic count when no canned value is set", async () => {
    const fake = new FakeProvider();

    // System "you are a watchkeeper" = 21 chars => ceil(21/4) = 6
    // Message "ping" = 4 chars => ceil(4/4) = 1
    // No tools.
    const got = await fake.countTokens(basicCountReq());
    expect(got).toBe(6 + 1);
  });

  it("includes tools in the synthetic count", async () => {
    const fake = new FakeProvider();
    const tool: ToolDefinition = {
      name: "ping",
      description: "echoes",
      inputSchema: {},
    };
    // System=6 + Msg=1 + name "ping"=ceil(4/4)=1 + desc "echoes"=ceil(6/4)=2 = 10
    const got = await fake.countTokens(basicCountReq({ tools: [tool] }));
    expect(got).toBe(10);
  });

  it("returns the canned response when set", async () => {
    const fake = new FakeProvider();
    fake.countTokensResp = 42;
    const got = await fake.countTokens(basicCountReq());
    expect(got).toBe(42);
  });
});

describe("FakeProvider — reportCost (happy)", () => {
  it("records (runtimeID, usage) tuples and accumulates across calls", async () => {
    const fake = new FakeProvider();
    const usageA: Usage = { ...ZERO_USAGE, inputTokens: 1 };
    const usageB: Usage = { ...ZERO_USAGE, outputTokens: 2 };
    await fake.reportCost("runtime-1", usageA);
    await fake.reportCost("runtime-2", usageB);

    expect(fake.recordedReportCosts()).toEqual([
      { runtimeID: "runtime-1", usage: usageA },
      { runtimeID: "runtime-2", usage: usageB },
    ]);
  });
});

describe("FakeProvider — stream stop (edge)", () => {
  it("stop() is idempotent — second call returns the same result without re-running shutdown", async () => {
    const fake = new FakeProvider();
    fake.streamEvents = [{ kind: "text_delta", textDelta: "x" }];
    const sub = await fake.stream(basicStreamReq(), () => {
      // ignore — stop-idempotency assertion does not care about events
    });
    await expect(sub.stop()).resolves.toBeUndefined();
    // Second call: should not re-throw / re-run; identical result.
    await expect(sub.stop()).resolves.toBeUndefined();
  });

  it("handler-returned error short-circuits dispatch; subsequent stop() returns LLMError(stream_closed) with cause === handlerErr", async () => {
    const fake = new FakeProvider();
    const handlerErr = new Error("boom");
    fake.streamEvents = [
      { kind: "text_delta", textDelta: "first" },
      { kind: "text_delta", textDelta: "second" },
    ];

    const seen: string[] = [];
    const sub = await fake.stream(basicStreamReq(), (ev) => {
      seen.push(ev.textDelta ?? "");
      throw handlerErr;
    });

    expect(seen).toEqual(["first"]); // dispatch short-circuited

    let caught: unknown = undefined;
    try {
      await sub.stop();
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(LLMError);
    const llm = caught as LLMError;
    expect(llm.code).toBe("stream_closed");
    expect(llm.cause).toBe(handlerErr);

    // Idempotent: second stop() throws the same captured error.
    let caught2: unknown = undefined;
    try {
      await sub.stop();
    } catch (e) {
      caught2 = e;
    }
    expect(caught2).toBe(caught);
  });
});

describe("FakeProvider — model catalogue (edge)", () => {
  it("empty catalogue accepts any non-empty model", async () => {
    const fake = new FakeProvider();
    const req = basicCompleteReq({ model: model("any-model") });
    await expect(fake.complete(req)).resolves.toBeDefined();
  });

  it("non-empty catalogue rejects off-catalogue models with LLMError(model_not_supported)", async () => {
    const fake = new FakeProvider({ models: [MODEL] });
    const req = basicCompleteReq({ model: model("gpt-4o") });
    await expect(fake.complete(req)).rejects.toBeInstanceOf(LLMError);
    await expect(fake.complete(req)).rejects.toHaveProperty("code", "model_not_supported");
  });
});

describe("FakeProvider — negative paths (sentinels)", () => {
  it("complete({ model: '' }) throws LLMError(model_not_supported) synchronously and does NOT record", async () => {
    const fake = new FakeProvider();
    fake.completeErr = new Error("should never surface — validation precedes canned errors");
    const req = basicCompleteReq({ model: model("") });

    let caught: unknown = undefined;
    try {
      await fake.complete(req);
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(LLMError);
    expect((caught as LLMError).code).toBe("model_not_supported");
    expect(fake.recordedCompletes()).toEqual([]);
  });

  it("complete({ messages: [] }) throws LLMError(invalid_prompt)", async () => {
    const fake = new FakeProvider();
    const req = basicCompleteReq({ messages: [] });
    await expect(fake.complete(req)).rejects.toBeInstanceOf(LLMError);
    await expect(fake.complete(req)).rejects.toHaveProperty("code", "invalid_prompt");
    expect(fake.recordedCompletes()).toEqual([]);
  });

  it("stream(req, null) throws LLMError(invalid_handler)", async () => {
    const fake = new FakeProvider();
    // Cast through `unknown` because the static type forbids null — the
    // sentinel exists for callers who lose typing at a JSON boundary.
    const handler = null as unknown as Parameters<typeof fake.stream>[1];

    let caught: unknown = undefined;
    try {
      await fake.stream(basicStreamReq(), handler);
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(LLMError);
    expect((caught as LLMError).code).toBe("invalid_handler");
  });

  it("complete with tools: [{ inputSchema: null }] throws LLMError(invalid_prompt)", async () => {
    const fake = new FakeProvider();
    const req = basicCompleteReq({
      tools: [{ name: "bad", description: "", inputSchema: null }],
    });
    await expect(fake.complete(req)).rejects.toBeInstanceOf(LLMError);
    await expect(fake.complete(req)).rejects.toHaveProperty("code", "invalid_prompt");
  });

  it("validation error precedence: completeErr is NEVER surfaced when validation fails", async () => {
    const fake = new FakeProvider();
    fake.completeErr = new LLMError("provider_unavailable");
    // Validation should reject this before the canned error is consulted.
    const req = basicCompleteReq({ messages: [] });
    let caught: unknown = undefined;
    try {
      await fake.complete(req);
    } catch (e) {
      caught = e;
    }
    expect((caught as LLMError).code).toBe("invalid_prompt");
  });
});

describe("StreamEventKind — type-only narrowing", () => {
  it("an exhaustive switch over StreamEventKind compiles without a default branch", () => {
    // Type-level assertion: this function is unused at runtime but must
    // typecheck. If `StreamEventKind` ever loses a case, `assertNever`
    // breaks compilation.
    function classify(ev: StreamEvent): string {
      switch (ev.kind) {
        case "text_delta":
          return "text_delta";
        case "tool_call_start":
          return "tool_call_start";
        case "tool_call_delta":
          return "tool_call_delta";
        case "message_stop":
          return "message_stop";
        case "error":
          return "error";
      }
    }

    // Smoke runtime: every kind round-trips.
    const kinds: StreamEventKind[] = [
      "text_delta",
      "tool_call_start",
      "tool_call_delta",
      "message_stop",
      "error",
    ];
    for (const k of kinds) {
      expect(classify({ kind: k })).toBe(k);
    }
  });
});

describe("FakeProvider — defensive-copy isolation", () => {
  it("recordedCompletes() returns a defensive copy: mutating the returned array does not corrupt fake state", async () => {
    const fake = new FakeProvider();
    await fake.complete(basicCompleteReq());

    const snap1 = fake.recordedCompletes();
    expect(snap1).toHaveLength(1);

    snap1.length = 0;
    snap1.push(basicCompleteReq({ system: "tampered" }));

    const snap2 = fake.recordedCompletes();
    expect(snap2).toHaveLength(1);
    expect(snap2[0]?.system).toBe(SYSTEM_PROMPT);
  });
});
