/**
 * `ClaudeAgentProvider` vitest suite (M5.7.c).
 *
 * Drives the provider via the `queryImpl` DI seam — a fake async
 * generator stands in for `@anthropic-ai/claude-agent-sdk`'s `query`
 * function so the unit suite runs without spawning a real `claude`
 * subprocess. Production callers MUST NOT use the seam; it exists only
 * for this suite (mirrors M5.7.a secret-source DI discipline — keep the
 * pluggable seam tiny).
 *
 * Coverage ordering follows the ClaudeCodeProvider precedent:
 *   1. happy paths (complete + reportCost)
 *   2. SDK-result-shape edge cases (cost rounding, missing usage)
 *   3. validation (model / messages / tools)
 *   4. error mapping (auth / rate-limit / token / abort → 7 sentinels)
 *   5. unimplemented surfaces (stream + countTokens raise
 *      provider_unavailable until M5.7.c.b/c land)
 */

import type { query } from "@anthropic-ai/claude-agent-sdk";
import { describe, expect, it } from "vitest";

import { ClaudeAgentProvider } from "../src/llm/claude-agent-provider.js";
import type { LLMError } from "../src/llm/index.js";
import { model, type Usage } from "../src/llm/index.js";

// The provider's queryImpl seam expects a `typeof query`; the fake
// returns an async iterable carrying SDK-shaped message objects. We
// cast through unknown at the seam because the production Query type
// carries extra control surfaces (control_request, supportedCommands,
// …) we do not need to mimic in unit tests.
type QueryImpl = typeof query;

/**
 * Build a fake `query` impl that yields the supplied SDK message
 * sequence. The Agent SDK's real return type is a `Query` (async-iterable
 * with extra control methods); for the unit suite we only need the
 * `[Symbol.asyncIterator]` shape.
 */
function fakeQuery(messages: readonly unknown[]): QueryImpl {
  return (() =>
    // eslint-disable-next-line @typescript-eslint/require-await -- generator yields a fixed sequence; no awaitable I/O inside.
    (async function* () {
      for (const m of messages) yield m;
    })()) as unknown as QueryImpl;
}

function fakeQueryThatThrows(err: unknown): QueryImpl {
  return (() =>
    // eslint-disable-next-line @typescript-eslint/require-await -- generator throws synchronously to model an SDK-side rejection; no await needed.
    (async function* () {
      throw err;
      yield {};
    })()) as unknown as QueryImpl;
}

/* -----------------------------------------------------------------------
 * (1) Happy paths
 * --------------------------------------------------------------------- */

describe("ClaudeAgentProvider.complete — happy paths", () => {
  it("collects assistant text + result usage into a CompleteResponse", async () => {
    const provider = new ClaudeAgentProvider({
      queryImpl: fakeQuery([
        {
          type: "assistant",
          message: {
            content: [
              { type: "text", text: "hello " },
              { type: "text", text: "world" },
            ],
          },
        },
        {
          type: "result",
          subtype: "success",
          stop_reason: "end_turn",
          usage: { input_tokens: 12, output_tokens: 5 },
          total_cost_usd: 0.0042,
        },
      ]),
    });

    const resp = await provider.complete({
      model: model("claude-sonnet-4-6"),
      messages: [{ role: "user", content: "hi" }],
    });

    expect(resp.content).toBe("hello world");
    expect(resp.finishReason).toBe("stop");
    expect(resp.toolCalls).toEqual([]);
    expect(resp.usage.inputTokens).toBe(12);
    expect(resp.usage.outputTokens).toBe(5);
    // 0.0042 USD × 10000 = 42 internal units (matches Go llm.Usage.CostCents).
    expect(resp.usage.costCents).toBe(42);
    expect(resp.usage.model).toBe("claude-sonnet-4-6");
  });

  it("extracts tool_use blocks into toolCalls with tool_use finish reason", async () => {
    const provider = new ClaudeAgentProvider({
      queryImpl: fakeQuery([
        {
          type: "assistant",
          message: {
            content: [
              { type: "text", text: "let me check" },
              {
                type: "tool_use",
                id: "tu_01",
                name: "notebook.recall",
                input: { query: "auth" },
              },
            ],
          },
        },
        {
          type: "result",
          subtype: "success",
          stop_reason: "tool_use",
          usage: { input_tokens: 8, output_tokens: 12 },
          total_cost_usd: 0.001,
        },
      ]),
    });

    const resp = await provider.complete({
      model: model("claude-sonnet-4-6"),
      messages: [{ role: "user", content: "remember the auth incident" }],
    });

    expect(resp.content).toBe("let me check");
    expect(resp.finishReason).toBe("tool_use");
    expect(resp.toolCalls).toEqual([
      { id: "tu_01", name: "notebook.recall", arguments: { query: "auth" } },
    ]);
  });

  it("maps result subtype=error to finishReason=error + errorMessage", async () => {
    const provider = new ClaudeAgentProvider({
      queryImpl: fakeQuery([
        {
          type: "result",
          subtype: "error",
          error: "billing_error",
        },
      ]),
    });

    const resp = await provider.complete({
      model: model("claude-sonnet-4-6"),
      messages: [{ role: "user", content: "ping" }],
    });

    expect(resp.finishReason).toBe("error");
    expect(resp.errorMessage).toBe("billing_error");
    expect(resp.usage.inputTokens).toBe(0);
  });

  it("lifts a system role message to options.systemPrompt", async () => {
    let capturedOptions: { systemPrompt?: string } | undefined;
    const provider = new ClaudeAgentProvider({
      queryImpl: ((params: { options?: { systemPrompt?: string } }) => {
        capturedOptions = params.options;
        // eslint-disable-next-line @typescript-eslint/require-await -- generator yields a fixed sequence; no awaitable I/O.
        return (async function* () {
          yield {
            type: "assistant",
            message: { content: [{ type: "text", text: "ok" }] },
          };
          yield {
            type: "result",
            subtype: "success",
            stop_reason: "end_turn",
            usage: { input_tokens: 1, output_tokens: 1 },
            total_cost_usd: 0,
          };
        })();
      }) as unknown as QueryImpl,
    });

    await provider.complete({
      model: model("claude-sonnet-4-6"),
      system: "you are the Watchmaster",
      messages: [{ role: "user", content: "hi" }],
    });

    expect(capturedOptions?.systemPrompt).toBe("you are the Watchmaster");
  });
});

describe("ClaudeAgentProvider.reportCost", () => {
  it("accumulates usage across multiple reports for the same runtimeID", async () => {
    const provider = new ClaudeAgentProvider();
    const usage = (input: number, output: number, cost: number): Usage => ({
      model: model("claude-sonnet-4-6"),
      inputTokens: input,
      outputTokens: output,
      costCents: cost,
    });

    await provider.reportCost("rt-1", usage(10, 20, 100));
    await provider.reportCost("rt-1", usage(5, 5, 50));
    await provider.reportCost("rt-2", usage(7, 3, 30));

    const total1 = provider.getReportedCost("rt-1");
    const total2 = provider.getReportedCost("rt-2");
    expect(total1?.inputTokens).toBe(15);
    expect(total1?.outputTokens).toBe(25);
    expect(total1?.costCents).toBe(150);
    expect(total2?.inputTokens).toBe(7);
    expect(total2?.costCents).toBe(30);
  });
});

/* -----------------------------------------------------------------------
 * (2) SDK-result edge cases
 * --------------------------------------------------------------------- */

describe("ClaudeAgentProvider.complete — result-shape edges", () => {
  it("rounds total_cost_usd × 10000 to nearest integer cent unit", async () => {
    const provider = new ClaudeAgentProvider({
      queryImpl: fakeQuery([
        {
          type: "assistant",
          message: { content: [{ type: "text", text: "ok" }] },
        },
        {
          type: "result",
          subtype: "success",
          stop_reason: "end_turn",
          // 0.12345 USD × 10000 = 1234.5 → rounded to 1235.
          usage: { input_tokens: 1, output_tokens: 1 },
          total_cost_usd: 0.12345,
        },
      ]),
    });
    const resp = await provider.complete({
      model: model("claude-sonnet-4-6"),
      messages: [{ role: "user", content: "x" }],
    });
    expect(resp.usage.costCents).toBe(1235);
  });

  it("treats missing usage fields as zero", async () => {
    const provider = new ClaudeAgentProvider({
      queryImpl: fakeQuery([
        {
          type: "result",
          subtype: "success",
          stop_reason: "end_turn",
          usage: {},
        },
      ]),
    });
    const resp = await provider.complete({
      model: model("claude-sonnet-4-6"),
      messages: [{ role: "user", content: "x" }],
    });
    expect(resp.usage.inputTokens).toBe(0);
    expect(resp.usage.outputTokens).toBe(0);
    expect(resp.usage.costCents).toBe(0);
  });

  it("raises provider_unavailable when the SDK emits no result message", async () => {
    const provider = new ClaudeAgentProvider({
      queryImpl: fakeQuery([
        {
          type: "assistant",
          message: { content: [{ type: "text", text: "partial" }] },
        },
      ]),
    });
    await expect(
      provider.complete({
        model: model("claude-sonnet-4-6"),
        messages: [{ role: "user", content: "x" }],
      }),
    ).rejects.toMatchObject({
      code: "provider_unavailable",
    });
  });

  it("maps stop_reason=max_tokens to finishReason=max_tokens", async () => {
    const provider = new ClaudeAgentProvider({
      queryImpl: fakeQuery([
        {
          type: "result",
          subtype: "success",
          stop_reason: "max_tokens",
          usage: { input_tokens: 1, output_tokens: 1 },
          total_cost_usd: 0,
        },
      ]),
    });
    const resp = await provider.complete({
      model: model("claude-sonnet-4-6"),
      messages: [{ role: "user", content: "x" }],
    });
    expect(resp.finishReason).toBe("max_tokens");
  });
});

/* -----------------------------------------------------------------------
 * (3) Validation — pre-SDK checks
 * --------------------------------------------------------------------- */

describe("ClaudeAgentProvider.complete — validation", () => {
  it("rejects empty model with model_not_supported", async () => {
    const provider = new ClaudeAgentProvider({ queryImpl: fakeQuery([]) });
    await expect(
      provider.complete({
        model: model(""),
        messages: [{ role: "user", content: "x" }],
      }),
    ).rejects.toMatchObject({ code: "model_not_supported" });
  });

  it("rejects empty messages with invalid_prompt", async () => {
    const provider = new ClaudeAgentProvider({ queryImpl: fakeQuery([]) });
    await expect(
      provider.complete({
        model: model("claude-sonnet-4-6"),
        messages: [],
      }),
    ).rejects.toMatchObject({ code: "invalid_prompt" });
  });

  it("rejects a tool with null inputSchema with invalid_prompt", async () => {
    const provider = new ClaudeAgentProvider({ queryImpl: fakeQuery([]) });
    await expect(
      provider.complete({
        model: model("claude-sonnet-4-6"),
        messages: [{ role: "user", content: "x" }],
        tools: [
          {
            name: "broken_tool",
            description: "missing schema",
            inputSchema: null,
          },
        ],
      }),
    ).rejects.toMatchObject({ code: "invalid_prompt" });
  });
});

/* -----------------------------------------------------------------------
 * (4) Error mapping — SDK throws → 7 LLMErrorCode sentinels
 * --------------------------------------------------------------------- */

describe("ClaudeAgentProvider.complete — SDK error mapping", () => {
  const cases: readonly {
    label: string;
    err: Error;
    expectedCode: LLMError["code"];
  }[] = [
    {
      label: "auth failure → provider_unavailable",
      err: new Error("authentication failed: invalid credential"),
      expectedCode: "provider_unavailable",
    },
    {
      label: "rate-limit → provider_unavailable",
      err: new Error("rate limit reached for tier 2"),
      expectedCode: "provider_unavailable",
    },
    {
      label: "max-tokens runtime → token_limit_exceeded",
      err: new Error("response exceeded max tokens budget"),
      expectedCode: "token_limit_exceeded",
    },
    {
      label: "abort → stream_closed",
      err: new Error("aborted by AbortController"),
      expectedCode: "stream_closed",
    },
    {
      label: "unknown error → provider_unavailable",
      err: new Error("ENOTFOUND api.anthropic.com"),
      expectedCode: "provider_unavailable",
    },
  ];

  for (const tc of cases) {
    it(tc.label, async () => {
      const provider = new ClaudeAgentProvider({
        queryImpl: fakeQueryThatThrows(tc.err),
      });
      await expect(
        provider.complete({
          model: model("claude-sonnet-4-6"),
          messages: [{ role: "user", content: "x" }],
        }),
      ).rejects.toMatchObject({ code: tc.expectedCode });
    });
  }
});

/* -----------------------------------------------------------------------
 * (5) Unimplemented surfaces — pinned until M5.7.c.b/c land
 * --------------------------------------------------------------------- */

describe("ClaudeAgentProvider — pending slices", () => {
  it("stream() throws provider_unavailable (M5.7.c.b slot)", async () => {
    const provider = new ClaudeAgentProvider();
    await expect(
      provider.stream(
        {
          model: model("claude-sonnet-4-6"),
          messages: [{ role: "user", content: "x" }],
        },
        () => undefined,
      ),
    ).rejects.toMatchObject({ code: "provider_unavailable" });
  });

  it("countTokens() throws provider_unavailable (M5.7.c.c slot)", async () => {
    const provider = new ClaudeAgentProvider();
    await expect(
      provider.countTokens({
        model: model("claude-sonnet-4-6"),
        messages: [{ role: "user", content: "x" }],
      }),
    ).rejects.toMatchObject({ code: "provider_unavailable" });
  });
});
