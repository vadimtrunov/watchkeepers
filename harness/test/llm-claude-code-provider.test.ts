/**
 * `ClaudeCodeProvider` vitest suite (M5.3.c.c.b).
 *
 * Mocks `@anthropic-ai/sdk` at the import boundary so the provider runs
 * without network access. The mock factory exposes:
 *
 *   - `MockAnthropic` — class double driving `messages.create`,
 *     `messages.stream`, `messages.countTokens`. Per-test mutation of
 *     the `mockState` module-scope object steers behaviour.
 *   - Stand-in error classes (`AuthenticationError`, `BadRequestError`,
 *     `RateLimitError`, `OverloadedError`, `InternalServerError`,
 *     `APIConnectionError`) so the provider's error mapping can match
 *     them via `instanceof`.
 *
 * Test ordering follows the FakeProvider precedent:
 *   1. happy paths (complete / stream / countTokens / reportCost)
 *   2. edge paths (stop idempotency, handler-error short-circuit, mid-flight error)
 *   3. validation (model / messages / tools / handler — pre-SDK)
 *   4. negative SDK errors (each mapped to its `LLMError` code)
 */

import Anthropic from "@anthropic-ai/sdk";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ClaudeCodeProvider } from "../src/llm/claude-code-provider.js";
import { LLMError, model, type StreamEvent, type Usage } from "../src/llm/index.js";

// -- SDK mock --------------------------------------------------------------
//
// `vi.mock` is hoisted to the top of the file at runtime, so the factory
// MUST construct everything it returns from inside the callback (no
// closure references to module-scope `let`/`const` symbols, except for
// imports of vitest itself).

interface MockStream {
  controller: { abort: () => void };
  [Symbol.asyncIterator]: () => AsyncIterator<unknown>;
}

interface SdkSpies {
  createSpy: ReturnType<typeof vi.fn>;
  streamSpy: ReturnType<typeof vi.fn>;
  countTokensSpy: ReturnType<typeof vi.fn>;
}

vi.mock("@anthropic-ai/sdk", () => {
  class APIError extends Error {
    public status?: number;
    public constructor(message: string, status?: number) {
      super(message);
      this.name = "APIError";
      if (status !== undefined) this.status = status;
    }
  }
  class AuthenticationError extends APIError {
    public constructor(message = "auth failed") {
      super(message, 401);
      this.name = "AuthenticationError";
    }
  }
  class BadRequestError extends APIError {
    public constructor(message = "bad request") {
      super(message, 400);
      this.name = "BadRequestError";
    }
  }
  class RateLimitError extends APIError {
    public constructor(message = "rate limited") {
      super(message, 429);
      this.name = "RateLimitError";
    }
  }
  // Stand-in for the overloaded scenario. The real SDK does not export
  // a separate `OverloadedError` — overloaded responses surface as a
  // generic APIError with status 529. We expose this class so tests can
  // throw something with status 529 and verify the mapping handles it.
  class OverloadedError extends APIError {
    public constructor(message = "overloaded") {
      super(message, 529);
      this.name = "OverloadedError";
    }
  }
  class InternalServerError extends APIError {
    public constructor(message = "server error") {
      super(message, 500);
      this.name = "InternalServerError";
    }
  }
  class APIConnectionError extends APIError {
    public constructor(message = "connection failed") {
      super(message);
      this.name = "APIConnectionError";
    }
  }
  class APIConnectionTimeoutError extends APIConnectionError {
    public constructor(message = "connection timed out") {
      super(message);
      this.name = "APIConnectionTimeoutError";
    }
  }

  // Per-instance spies live on `globalThis` so the suite can read them
  // back without a factory-to-module bridge. Vitest reset()'s them
  // between tests in `beforeEach`.
  const createSpy = vi.fn();
  const streamSpy = vi.fn();
  const countTokensSpy = vi.fn();
  (globalThis as { __claudeSdkSpies?: SdkSpies }).__claudeSdkSpies = {
    createSpy,
    streamSpy,
    countTokensSpy,
  };

  class MockAnthropic {
    public readonly apiKey: string | undefined;
    public readonly baseURL: string | undefined;
    public readonly messages: {
      create: typeof createSpy;
      stream: typeof streamSpy;
      countTokens: typeof countTokensSpy;
    };
    public constructor(opts?: { apiKey?: string; baseURL?: string }) {
      this.apiKey = opts?.apiKey;
      this.baseURL = opts?.baseURL;
      this.messages = {
        create: createSpy,
        stream: streamSpy,
        countTokens: countTokensSpy,
      };
    }

    public static APIError = APIError;
    public static AuthenticationError = AuthenticationError;
    public static BadRequestError = BadRequestError;
    public static RateLimitError = RateLimitError;
    public static OverloadedError = OverloadedError;
    public static InternalServerError = InternalServerError;
    public static APIConnectionError = APIConnectionError;
    public static APIConnectionTimeoutError = APIConnectionTimeoutError;
  }

  return {
    default: MockAnthropic,
    APIError,
    AuthenticationError,
    BadRequestError,
    RateLimitError,
    OverloadedError,
    InternalServerError,
    APIConnectionError,
    APIConnectionTimeoutError,
  };
});

// Pull the spies back via the typed globalThis bridge populated inside
// the vi.mock factory (the factory is hoisted, so by the time this line
// executes the spies are already assigned).
const spies = (globalThis as { __claudeSdkSpies?: SdkSpies }).__claudeSdkSpies;
if (spies === undefined) {
  throw new Error("vi.mock factory did not populate __claudeSdkSpies");
}
const { createSpy, streamSpy, countTokensSpy } = spies;

// Helper: build an async-iterable stream wrapper from a raw event list.
function makeStream(events: readonly unknown[]): MockStream {
  let aborted = false;
  const controller = {
    abort: vi.fn(() => {
      aborted = true;
    }),
  };
  return {
    controller,
    [Symbol.asyncIterator]() {
      let i = 0;
      return {
        // eslint-disable-next-line @typescript-eslint/require-await -- async iterator protocol requires Promise return.
        next: async (): Promise<IteratorResult<unknown>> => {
          if (aborted) return { value: undefined, done: true };
          if (i >= events.length) return { value: undefined, done: true };
          const v = events[i];
          i += 1;
          return { value: v, done: false };
        },
      };
    },
  };
}

// The mock factory exposes simple `(message?: string)` constructors,
// but TypeScript sees the real SDK's stricter `(status, error, message, ...)`
// signatures. Cast each binding to a stand-in ctor type so test code can
// `new ErrClass("msg")` without the static type complaining.
type SimpleErrCtor = new (message?: string) => Error;

const AuthenticationError = Anthropic.AuthenticationError as unknown as SimpleErrCtor;
const BadRequestError = Anthropic.BadRequestError as unknown as SimpleErrCtor;
const RateLimitError = Anthropic.RateLimitError as unknown as SimpleErrCtor;
const OverloadedError = (Anthropic as unknown as { OverloadedError: SimpleErrCtor })
  .OverloadedError;
const InternalServerError = Anthropic.InternalServerError as unknown as SimpleErrCtor;
const APIConnectionError = Anthropic.APIConnectionError as unknown as SimpleErrCtor;

const MODEL = model("claude-sonnet-4-5-20250929");

beforeEach(() => {
  createSpy.mockReset();
  streamSpy.mockReset();
  countTokensSpy.mockReset();
  // Default: every spy throws "not configured" so a forgotten test setup
  // surfaces a loud error rather than silently passing.
  createSpy.mockImplementation(() => {
    throw new Error("createSpy not configured");
  });
  streamSpy.mockImplementation(() => {
    throw new Error("streamSpy not configured");
  });
  countTokensSpy.mockImplementation(() => {
    throw new Error("countTokensSpy not configured");
  });
});

afterEach(() => {
  // Spies are reset in beforeEach; nothing else to clean up.
});

// -- happy paths -----------------------------------------------------------

describe("ClaudeCodeProvider — complete (happy)", () => {
  it("translates a text-only response into CompleteResponse", async () => {
    createSpy.mockResolvedValue({
      id: "msg_01",
      stop_reason: "end_turn",
      content: [{ type: "text", text: "hi" }],
      usage: { input_tokens: 5, output_tokens: 1 },
    });

    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    const got = await provider.complete({
      model: MODEL,
      messages: [{ role: "user", content: "ping" }],
    });

    expect(got.content).toBe("hi");
    expect(got.toolCalls).toEqual([]);
    expect(got.finishReason).toBe("stop");
    expect(got.usage.inputTokens).toBe(5);
    expect(got.usage.outputTokens).toBe(1);
    expect(got.usage.model).toBe(MODEL);
    expect(got.metadata?.anthropic_id).toBe("msg_01");
    expect(createSpy).toHaveBeenCalledTimes(1);
  });

  it("packs tool_use content blocks into ToolCalls and maps stop_reason", async () => {
    createSpy.mockResolvedValue({
      id: "msg_02",
      stop_reason: "tool_use",
      content: [
        { type: "text", text: "let me look that up" },
        { type: "tool_use", id: "call_1", name: "lookup", input: { q: "hi" } },
      ],
      usage: { input_tokens: 9, output_tokens: 4 },
    });

    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    const got = await provider.complete({
      model: MODEL,
      messages: [{ role: "user", content: "ping" }],
    });

    expect(got.content).toBe("let me look that up");
    expect(got.finishReason).toBe("tool_use");
    expect(got.toolCalls).toEqual([{ id: "call_1", name: "lookup", arguments: { q: "hi" } }]);
  });

  it("maps SDK stop_reason values to FinishReason", async () => {
    const cases: [string, string][] = [
      ["end_turn", "stop"],
      ["max_tokens", "max_tokens"],
      ["stop_sequence", "stop"],
      ["tool_use", "tool_use"],
    ];
    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    for (const [sdkReason, expected] of cases) {
      createSpy.mockResolvedValueOnce({
        id: "x",
        stop_reason: sdkReason,
        content: [],
        usage: { input_tokens: 0, output_tokens: 0 },
      });
      const got = await provider.complete({
        model: MODEL,
        messages: [{ role: "user", content: "ping" }],
      });
      expect(got.finishReason).toBe(expected);
    }
  });
});

describe("ClaudeCodeProvider — stream (happy)", () => {
  it("dispatches text-delta events then message_stop with final usage", async () => {
    streamSpy.mockReturnValue(
      makeStream([
        { type: "content_block_start", index: 0, content_block: { type: "text", text: "" } },
        { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "he" } },
        { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "ll" } },
        { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "o" } },
        { type: "content_block_stop", index: 0 },
        {
          type: "message_delta",
          delta: { stop_reason: "end_turn" },
          usage: { input_tokens: 3, output_tokens: 3 },
        },
        { type: "message_stop" },
      ]),
    );

    const seen: StreamEvent[] = [];
    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    const sub = await provider.stream(
      { model: MODEL, messages: [{ role: "user", content: "ping" }] },
      (ev) => {
        seen.push(ev);
      },
    );

    expect(seen.filter((e) => e.kind === "text_delta")).toHaveLength(3);
    expect(
      seen
        .map((e) => e.textDelta)
        .filter(Boolean)
        .join(""),
    ).toBe("hello");
    const stop = seen.find((e) => e.kind === "message_stop");
    expect(stop?.finishReason).toBe("stop");
    expect(stop?.usage?.inputTokens).toBe(3);
    expect(stop?.usage?.outputTokens).toBe(3);

    await expect(sub.stop()).resolves.toBeUndefined();
  });

  it("dispatches a tool_use sequence with correlated tool-call id", async () => {
    streamSpy.mockReturnValue(
      makeStream([
        {
          type: "content_block_start",
          index: 0,
          content_block: { type: "tool_use", id: "call_T", name: "lookup", input: {} },
        },
        {
          type: "content_block_delta",
          index: 0,
          delta: { type: "input_json_delta", partial_json: '{"q":' },
        },
        {
          type: "content_block_delta",
          index: 0,
          delta: { type: "input_json_delta", partial_json: '"hi"}' },
        },
        { type: "content_block_stop", index: 0 },
        {
          type: "message_delta",
          delta: { stop_reason: "tool_use" },
          usage: { input_tokens: 2, output_tokens: 5 },
        },
        { type: "message_stop" },
      ]),
    );

    const seen: StreamEvent[] = [];
    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    await provider.stream({ model: MODEL, messages: [{ role: "user", content: "ping" }] }, (ev) => {
      seen.push(ev);
    });

    const start = seen.find((e) => e.kind === "tool_call_start");
    expect(start?.toolCall?.id).toBe("call_T");
    expect(start?.toolCall?.name).toBe("lookup");

    const deltas = seen.filter((e) => e.kind === "tool_call_delta");
    expect(deltas).toHaveLength(2);
    for (const d of deltas) {
      expect(d.toolCall?.id).toBe("call_T");
    }
    expect(deltas.map((d) => d.textDelta).join("")).toBe('{"q":"hi"}');

    const stop = seen.find((e) => e.kind === "message_stop");
    expect(stop?.finishReason).toBe("tool_use");
  });
});

describe("ClaudeCodeProvider — countTokens (happy)", () => {
  it("returns the SDK's input_tokens", async () => {
    countTokensSpy.mockResolvedValue({ input_tokens: 42 });
    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    const got = await provider.countTokens({
      model: MODEL,
      messages: [{ role: "user", content: "ping" }],
    });
    expect(got).toBe(42);
    expect(countTokensSpy).toHaveBeenCalledTimes(1);
  });
});

describe("ClaudeCodeProvider — reportCost (happy)", () => {
  it("accumulates per-runtimeID usage; getReportedCost surfaces totals", async () => {
    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    const u1: Usage = {
      model: MODEL,
      inputTokens: 1,
      outputTokens: 2,
      costCents: 10,
    };
    const u2: Usage = {
      model: MODEL,
      inputTokens: 3,
      outputTokens: 4,
      costCents: 20,
    };
    await provider.reportCost("rt-1", u1);
    await provider.reportCost("rt-1", u2);

    const total = provider.getReportedCost("rt-1");
    expect(total?.inputTokens).toBe(4);
    expect(total?.outputTokens).toBe(6);
    expect(total?.costCents).toBe(30);
    expect(total?.model).toBe(MODEL);

    // Different runtime IDs are isolated.
    await provider.reportCost("rt-2", u1);
    expect(provider.getReportedCost("rt-2")?.inputTokens).toBe(1);
    expect(provider.getReportedCost("rt-1")?.inputTokens).toBe(4);

    expect(provider.getReportedCost("missing")).toBeUndefined();
  });
});

// -- edge paths ------------------------------------------------------------

describe("ClaudeCodeProvider — stream stop (edge)", () => {
  it("stop() aborts the SDK controller exactly once", async () => {
    const stream = makeStream([
      { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "x" } },
      {
        type: "message_delta",
        delta: { stop_reason: "end_turn" },
        usage: { input_tokens: 1, output_tokens: 1 },
      },
      { type: "message_stop" },
    ]);
    streamSpy.mockReturnValue(stream);

    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    const sub = await provider.stream(
      { model: MODEL, messages: [{ role: "user", content: "ping" }] },
      () => {
        // ignore
      },
    );

    await expect(sub.stop()).resolves.toBeUndefined();
    await expect(sub.stop()).resolves.toBeUndefined();
    expect(stream.controller.abort).toHaveBeenCalledTimes(1);
  });

  it("handler error short-circuits dispatch; stop() returns LLMError(stream_closed) wrapping cause", async () => {
    streamSpy.mockReturnValue(
      makeStream([
        { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "first" } },
        { type: "content_block_delta", index: 0, delta: { type: "text_delta", text: "second" } },
      ]),
    );

    const handlerErr = new Error("boom");
    const seen: string[] = [];
    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    const sub = await provider.stream(
      { model: MODEL, messages: [{ role: "user", content: "ping" }] },
      (ev) => {
        seen.push(ev.textDelta ?? "");
        throw handlerErr;
      },
    );
    expect(seen).toEqual(["first"]);

    let caught: unknown;
    try {
      await sub.stop();
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(LLMError);
    expect((caught as LLMError).code).toBe("stream_closed");
    expect((caught as LLMError).cause).toBe(handlerErr);

    let caught2: unknown;
    try {
      await sub.stop();
    } catch (e) {
      caught2 = e;
    }
    expect(caught2).toBe(caught);
  });

  it("SDK stream errors mid-flight: handler observes error event; stop() returns stream_closed", async () => {
    const sdkErr = new Error("sdk-broke");
    const stream: MockStream = {
      controller: { abort: vi.fn() },
      [Symbol.asyncIterator]() {
        let yielded = false;
        return {
          // eslint-disable-next-line @typescript-eslint/require-await -- async iterator protocol requires Promise return; this iterator throws after the first yield instead of awaiting.
          next: async (): Promise<IteratorResult<unknown>> => {
            if (!yielded) {
              yielded = true;
              return {
                value: {
                  type: "content_block_delta",
                  index: 0,
                  delta: { type: "text_delta", text: "x" },
                },
                done: false,
              };
            }
            throw sdkErr;
          },
        };
      },
    };
    streamSpy.mockReturnValue(stream);

    const seen: StreamEvent[] = [];
    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    const sub = await provider.stream(
      { model: MODEL, messages: [{ role: "user", content: "ping" }] },
      (ev) => {
        seen.push(ev);
      },
    );

    expect(seen.some((e) => e.kind === "error" && e.errorMessage?.includes("sdk-broke"))).toBe(
      true,
    );
    let caught: unknown;
    try {
      await sub.stop();
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(LLMError);
    expect((caught as LLMError).code).toBe("stream_closed");
    expect((caught as LLMError).cause).toBe(sdkErr);
  });
});

// -- validation paths (pre-SDK) -------------------------------------------

describe("ClaudeCodeProvider — validation (pre-SDK)", () => {
  it("complete with empty model rejects with model_not_supported and never calls SDK", async () => {
    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    await expect(
      provider.complete({
        model: model(""),
        messages: [{ role: "user", content: "ping" }],
      }),
    ).rejects.toMatchObject({ code: "model_not_supported" });
    expect(createSpy).not.toHaveBeenCalled();
  });

  it("complete with empty messages rejects with invalid_prompt and never calls SDK", async () => {
    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    await expect(provider.complete({ model: MODEL, messages: [] })).rejects.toMatchObject({
      code: "invalid_prompt",
    });
    expect(createSpy).not.toHaveBeenCalled();
  });

  it("stream with null handler rejects with invalid_handler", async () => {
    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    const handler = null as unknown as Parameters<typeof provider.stream>[1];
    await expect(
      provider.stream({ model: MODEL, messages: [{ role: "user", content: "ping" }] }, handler),
    ).rejects.toMatchObject({ code: "invalid_handler" });
    expect(streamSpy).not.toHaveBeenCalled();
  });

  it("complete with a tool whose inputSchema is null rejects with invalid_prompt", async () => {
    const provider = new ClaudeCodeProvider({ apiKey: "k" });
    await expect(
      provider.complete({
        model: MODEL,
        messages: [{ role: "user", content: "ping" }],
        tools: [{ name: "bad", description: "", inputSchema: null }],
      }),
    ).rejects.toMatchObject({ code: "invalid_prompt" });
    expect(createSpy).not.toHaveBeenCalled();
  });
});

// -- negative SDK errors (mapped to LLMError) -----------------------------

describe("ClaudeCodeProvider — SDK error mapping", () => {
  const provider = new ClaudeCodeProvider({ apiKey: "k" });
  const okReq = {
    model: MODEL,
    messages: [{ role: "user" as const, content: "ping" }],
  };

  it("AuthenticationError → provider_unavailable (cause preserved)", async () => {
    const err = new AuthenticationError("bad key");
    createSpy.mockRejectedValueOnce(err);
    let caught: unknown;
    try {
      await provider.complete(okReq);
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(LLMError);
    expect((caught as LLMError).code).toBe("provider_unavailable");
    expect((caught as LLMError).cause).toBe(err);
  });

  it("RateLimitError → provider_unavailable", async () => {
    createSpy.mockRejectedValueOnce(new RateLimitError());
    await expect(provider.complete(okReq)).rejects.toMatchObject({
      code: "provider_unavailable",
    });
  });

  it("OverloadedError → provider_unavailable", async () => {
    createSpy.mockRejectedValueOnce(new OverloadedError());
    await expect(provider.complete(okReq)).rejects.toMatchObject({
      code: "provider_unavailable",
    });
  });

  it("InternalServerError → provider_unavailable", async () => {
    createSpy.mockRejectedValueOnce(new InternalServerError());
    await expect(provider.complete(okReq)).rejects.toMatchObject({
      code: "provider_unavailable",
    });
  });

  it("APIConnectionError → provider_unavailable", async () => {
    createSpy.mockRejectedValueOnce(new APIConnectionError());
    await expect(provider.complete(okReq)).rejects.toMatchObject({
      code: "provider_unavailable",
    });
  });

  it("BadRequestError matching context-length pattern → token_limit_exceeded", async () => {
    createSpy.mockRejectedValueOnce(new BadRequestError("Input is too long for the model context"));
    await expect(provider.complete(okReq)).rejects.toMatchObject({
      code: "token_limit_exceeded",
    });
  });

  it("BadRequestError matching model-not-found pattern → model_not_supported", async () => {
    createSpy.mockRejectedValueOnce(new BadRequestError("Model claude-foo not found"));
    await expect(provider.complete(okReq)).rejects.toMatchObject({
      code: "model_not_supported",
    });
  });

  it("Other BadRequestError → invalid_prompt", async () => {
    createSpy.mockRejectedValueOnce(new BadRequestError("missing required field: messages"));
    await expect(provider.complete(okReq)).rejects.toMatchObject({
      code: "invalid_prompt",
    });
  });

  it("Unknown error → provider_unavailable wrapping cause", async () => {
    const err = new Error("kaboom");
    createSpy.mockRejectedValueOnce(err);
    let caught: unknown;
    try {
      await provider.complete(okReq);
    } catch (e) {
      caught = e;
    }
    expect(caught).toBeInstanceOf(LLMError);
    expect((caught as LLMError).code).toBe("provider_unavailable");
    expect((caught as LLMError).cause).toBe(err);
  });

  it("countTokens propagates SDK errors via the same mapping", async () => {
    countTokensSpy.mockRejectedValueOnce(new AuthenticationError());
    await expect(
      provider.countTokens({
        model: MODEL,
        messages: [{ role: "user", content: "ping" }],
      }),
    ).rejects.toMatchObject({ code: "provider_unavailable" });
  });
});
