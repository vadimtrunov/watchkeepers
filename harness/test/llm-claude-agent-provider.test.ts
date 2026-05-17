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
import type { LLMError, StreamEvent, ToolDefinition } from "../src/llm/index.js";
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
    // The bridge decodes the SDK's mcp__watchkeeper__<encoded> name back to
    // the runtime name using the codec built from req.tools. The fake SDK
    // response uses the plain runtime name (as if it were encoded without an
    // MCP prefix) — the codec maps notebook_recall → notebook.recall.
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
                // Bridge codec maps mcp__watchkeeper__notebook_recall → notebook.recall
                name: "mcp__watchkeeper__notebook_recall",
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
      tools: [{ name: "notebook.recall", description: "recall", inputSchema: { type: "object" } }],
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

  it("returns a zero-usage response when the SDK emits no result message", async () => {
    // The interceptor (tool-bridge-interceptor.ts) synthesises a zero-usage
    // turn from the assistant message instead of throwing; this is a
    // deliberate behaviour change from the pre-bridge inline loop which
    // raised provider_unavailable. The bridge is more resilient: a truncated
    // SDK response still yields valid content rather than crashing the caller.
    const provider = new ClaudeAgentProvider({
      queryImpl: fakeQuery([
        {
          type: "assistant",
          message: {
            content: [{ type: "text", text: "partial" }],
            usage: { input_tokens: 0, output_tokens: 0 },
          },
        },
      ]),
    });
    const resp = await provider.complete({
      model: model("claude-sonnet-4-6"),
      messages: [{ role: "user", content: "x" }],
    });
    expect(resp.content).toBe("partial");
    expect(resp.usage.inputTokens).toBe(0);
    expect(resp.usage.outputTokens).toBe(0);
    expect(resp.finishReason).toBe("stop");
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
 * (5) Stream — happy paths + validation + cancellation
 * --------------------------------------------------------------------- */

/**
 * Drain a subscription to completion by collecting every dispatched
 * event into `collected`. Mirrors the receiver loop a real caller would
 * write — handler pushes events into the array, then we wait for the
 * `message_stop` event before returning.
 *
 * Pass `tools` when the SDK messages include `content_block_start` tool_use
 * blocks — the bridge codec needs them to decode MCP-prefixed names back to
 * runtime names.
 */
async function drainStream(
  msgs: readonly unknown[],
  tools?: readonly ToolDefinition[],
): Promise<readonly StreamEvent[]> {
  const collected: StreamEvent[] = [];
  let stopped = false;
  const handler = (ev: StreamEvent): void => {
    collected.push(ev);
    if (ev.kind === "message_stop") stopped = true;
  };
  const provider = new ClaudeAgentProvider({ queryImpl: fakeQuery(msgs) });
  await provider.stream(
    {
      model: model("claude-sonnet-4-6"),
      messages: [{ role: "user", content: "x" }],
      ...(tools !== undefined ? { tools } : {}),
    },
    handler,
  );
  // Yield to the dispatch loop until it emits message_stop. The
  // `stopped` flag is closure-mutated by the handler above; ESLint's
  // type narrowing does not track the mutation so we silence its
  // "always truthy" complaint with an explicit disable.
  for (let i = 0; i < 50; i++) {
    // eslint-disable-next-line @typescript-eslint/no-unnecessary-condition -- handler closure mutates `stopped` after each setImmediate yield.
    if (stopped) break;
    await new Promise((r) => setImmediate(r));
  }
  return collected;
}

describe("ClaudeAgentProvider.stream — happy paths", () => {
  it("translates SDKPartialAssistantMessage text_delta events into text_delta StreamEvents", async () => {
    const events = await drainStream([
      {
        type: "stream_event",
        event: {
          type: "content_block_delta",
          delta: { type: "text_delta", text: "hel" },
        },
      },
      {
        type: "stream_event",
        event: {
          type: "content_block_delta",
          delta: { type: "text_delta", text: "lo" },
        },
      },
      {
        type: "result",
        subtype: "success",
        stop_reason: "end_turn",
        usage: { input_tokens: 1, output_tokens: 2 },
        total_cost_usd: 0,
      },
    ]);
    const text = events
      .filter((e) => e.kind === "text_delta")
      .map((e) => e.textDelta ?? "")
      .join("");
    expect(text).toBe("hello");
    const stop = events.find((e) => e.kind === "message_stop");
    expect(stop?.finishReason).toBe("stop");
    expect(stop?.usage?.outputTokens).toBe(2);
  });

  it("translates content_block_start tool_use into tool_call_start", async () => {
    // The bridge codec decodes the MCP-prefixed name back to the runtime name.
    // SDK events carry `mcp__watchkeeper__notebook_recall`; the test passes the
    // matching tool so the codec can resolve it to `notebook.recall`.
    const events = await drainStream(
      [
        {
          type: "stream_event",
          event: {
            type: "content_block_start",
            content_block: {
              type: "tool_use",
              id: "tu_42",
              name: "mcp__watchkeeper__notebook_recall",
            },
          },
        },
        {
          type: "stream_event",
          event: {
            type: "content_block_delta",
            delta: { type: "input_json_delta", partial_json: '{"q":"a' },
          },
        },
        {
          type: "stream_event",
          event: {
            type: "content_block_delta",
            delta: { type: "input_json_delta", partial_json: 'uth"}' },
          },
        },
        {
          type: "stream_event",
          event: { type: "content_block_stop" },
        },
        {
          type: "result",
          subtype: "success",
          stop_reason: "tool_use",
          usage: { input_tokens: 1, output_tokens: 1 },
          total_cost_usd: 0,
        },
      ],
      [{ name: "notebook.recall", description: "recall", inputSchema: { type: "object" } }],
    );
    const start = events.find((e) => e.kind === "tool_call_start");
    expect(start?.toolCall?.id).toBe("tu_42");
    expect(start?.toolCall?.name).toBe("notebook.recall");
    const deltas = events.filter((e) => e.kind === "tool_call_delta");
    expect(deltas.length).toBe(2);
    expect(deltas[0]?.textDelta).toBe('{"q":"a');
    expect(deltas[0]?.toolCall?.id).toBe("tu_42");
    expect(deltas[1]?.textDelta).toBe('uth"}');
    const stop = events.find((e) => e.kind === "message_stop");
    expect(stop?.finishReason).toBe("tool_use");
  });

  it("drops unrelated SDK message types (system / status / api_retry) without surfacing events", async () => {
    const events = await drainStream([
      { type: "system", subtype: "ready" },
      { type: "status", message: "warming up" },
      { type: "api_retry", retry_attempt: 1 },
      {
        type: "stream_event",
        event: { type: "content_block_delta", delta: { type: "text_delta", text: "hi" } },
      },
      {
        type: "result",
        subtype: "success",
        stop_reason: "end_turn",
        usage: { input_tokens: 1, output_tokens: 1 },
        total_cost_usd: 0,
      },
    ]);
    const kinds = events.map((e) => e.kind);
    // Only the text_delta + message_stop should surface.
    expect(kinds).toEqual(["text_delta", "message_stop"]);
  });
});

describe("ClaudeAgentProvider.stream — validation", () => {
  it("rejects null handler with invalid_handler", async () => {
    const provider = new ClaudeAgentProvider({ queryImpl: fakeQuery([]) });
    await expect(
      provider.stream(
        {
          model: model("claude-sonnet-4-6"),
          messages: [{ role: "user", content: "x" }],
        },
        null as unknown as (ev: StreamEvent) => void,
      ),
    ).rejects.toMatchObject({ code: "invalid_handler" });
  });

  it("rejects empty model with model_not_supported", async () => {
    const provider = new ClaudeAgentProvider({ queryImpl: fakeQuery([]) });
    await expect(
      provider.stream(
        { model: model(""), messages: [{ role: "user", content: "x" }] },
        () => undefined,
      ),
    ).rejects.toMatchObject({ code: "model_not_supported" });
  });

  it("rejects empty messages with invalid_prompt", async () => {
    const provider = new ClaudeAgentProvider({ queryImpl: fakeQuery([]) });
    await expect(
      provider.stream({ model: model("claude-sonnet-4-6"), messages: [] }, () => undefined),
    ).rejects.toMatchObject({ code: "invalid_prompt" });
  });
});

describe("ClaudeAgentProvider.stream — cancellation", () => {
  it("stop() race — concurrent callers observe the same settled promise", async () => {
    // Build a generator that yields one text_delta event; the handler will
    // throw on it, latching _cause inside the subscription.
    // eslint-disable-next-line @typescript-eslint/require-await -- async generator yields a fixed sequence; no awaitable I/O inside.
    const gen = (async function* () {
      yield {
        type: "stream_event",
        event: { type: "content_block_delta", delta: { type: "text_delta", text: "hi" } },
      };
      // No result — dispatch loop ends after the handler throws.
    })();
    const queryLike = Object.assign(gen, {
      interrupt: (): Promise<void> => Promise.resolve(),
    });
    const provider = new ClaudeAgentProvider({
      queryImpl: (() => queryLike) as unknown as QueryImpl,
    });
    const handlerErr = new Error("handler boom");
    const sub = await provider.stream(
      { model: model("claude-sonnet-4-6"), messages: [{ role: "user", content: "ping" }] },
      // eslint-disable-next-line @typescript-eslint/no-unused-vars -- handler intentionally ignores its argument and throws to simulate a downstream error.
      (_e: StreamEvent): void => {
        throw handlerErr;
      },
    );
    // Drain until the dispatch loop marks the subscription stopped via the
    // abortBag (the handler throw latches _cause).
    for (let i = 0; i < 50; i++) {
      if ((sub as unknown as { isStopped: boolean }).isStopped) break;
      await new Promise((r) => setImmediate(r));
    }
    // Fire two stop() calls in parallel. Both must observe the same outcome —
    // same status (both resolved or both rejected) and, if rejected, the same
    // error object (memoised promise shares the single rejection).
    const [a, b] = await Promise.allSettled([sub.stop(), sub.stop()]);
    expect(a.status).toBe(b.status);
    if (a.status === "rejected" && b.status === "rejected") {
      // The memoised promise ensures both callers share the identical rejection.
      expect(a.reason).toBe(b.reason);
    }
  });

  it("stop() interrupts the underlying Query and is idempotent", async () => {
    let interruptCalls = 0;
    // eslint-disable-next-line @typescript-eslint/require-await -- generator yields a fixed sequence; no awaitable I/O.
    const gen = (async function* () {
      yield {
        type: "stream_event",
        event: { type: "content_block_delta", delta: { type: "text_delta", text: "partial" } },
      };
      // never yields a result — caller must stop() to unblock
    })();
    const queryLike = Object.assign(gen, {
      // eslint-disable-next-line @typescript-eslint/require-await -- mirror the SDK Query.interrupt signature (Promise return) while keeping the body synchronous for the counter assertion.
      interrupt: async (): Promise<void> => {
        interruptCalls++;
      },
    });
    const provider = new ClaudeAgentProvider({
      queryImpl: (() => queryLike) as unknown as QueryImpl,
    });
    const sub = await provider.stream(
      { model: model("claude-sonnet-4-6"), messages: [{ role: "user", content: "x" }] },
      () => undefined,
    );
    await sub.stop();
    await sub.stop(); // idempotent
    expect(interruptCalls).toBe(1);
  });
});

/* -----------------------------------------------------------------------
 * (6) Unimplemented surfaces — pinned until M5.7.c.c lands
 * --------------------------------------------------------------------- */

describe("ClaudeAgentProvider — pending slices", () => {
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

/* -----------------------------------------------------------------------
 * (7) ClaudeAgentProvider.complete with tools (M5.7.c slice 4)
 * --------------------------------------------------------------------- */

describe("ClaudeAgentProvider.complete with tools (M5.7.c slice 4)", () => {
  it("registers a watchkeeper MCP server in options.mcpServers", async () => {
    let observedOptions: unknown;
    const fake = ((opts: { prompt: string; options?: Record<string, unknown> }) => {
      observedOptions = opts.options;
      // eslint-disable-next-line @typescript-eslint/require-await -- generator yields a fixed sequence; no awaitable I/O inside.
      return (async function* () {
        yield {
          type: "result",
          subtype: "success",
          usage: { input_tokens: 0, output_tokens: 0 },
          total_cost_usd: 0,
          stop_reason: "end_turn",
        };
      })();
    }) as unknown as QueryImpl;
    const provider = new ClaudeAgentProvider({ queryImpl: fake });
    await provider.complete({
      model: model("claude-3-5-sonnet-latest"),
      messages: [{ role: "user", content: "hi" }],
      tools: [{ name: "notebook.remember", description: "save", inputSchema: { type: "object" } }],
    });
    const opts = observedOptions as { mcpServers?: Record<string, unknown> } | undefined;
    const mcpServers = opts?.mcpServers;
    expect(mcpServers).toBeDefined();
    if (mcpServers !== undefined) {
      expect(Object.keys(mcpServers)).toContain("watchkeeper");
    }
  });

  it("returns toolCalls with runtime names (decoded from mcp prefix)", async () => {
    const fake = fakeQuery([
      {
        type: "assistant",
        message: {
          content: [
            { type: "text", text: "I'll save that." },
            {
              type: "tool_use",
              id: "tu_1",
              name: "mcp__watchkeeper__notebook_remember",
              input: { key: "k", value: "v" },
            },
          ],
          usage: { input_tokens: 1, output_tokens: 1 },
        },
      },
    ]);
    const provider = new ClaudeAgentProvider({ queryImpl: fake });
    const resp = await provider.complete({
      model: model("claude-3-5-sonnet-latest"),
      messages: [{ role: "user", content: "save k=v" }],
      tools: [{ name: "notebook.remember", description: "save", inputSchema: { type: "object" } }],
    });
    expect(resp.finishReason).toBe("tool_use");
    expect(resp.toolCalls).toEqual([
      { id: "tu_1", name: "notebook.remember", arguments: { key: "k", value: "v" } },
    ]);
  });

  it("omits mcpServers entirely when tools is undefined", async () => {
    let observedOptions: unknown;
    const fake = ((opts: { prompt: string; options?: Record<string, unknown> }) => {
      observedOptions = opts.options;
      // eslint-disable-next-line @typescript-eslint/require-await -- generator yields a fixed sequence; no awaitable I/O inside.
      return (async function* () {
        yield {
          type: "result",
          subtype: "success",
          usage: { input_tokens: 0, output_tokens: 0 },
          total_cost_usd: 0,
          stop_reason: "end_turn",
        };
      })();
    }) as unknown as QueryImpl;
    const provider = new ClaudeAgentProvider({ queryImpl: fake });
    await provider.complete({
      model: model("claude-3-5-sonnet-latest"),
      messages: [{ role: "user", content: "hi" }],
    });
    const opts = observedOptions as { mcpServers?: Record<string, unknown> } | undefined;
    expect(opts?.mcpServers).toBeUndefined();
  });
});

/* -----------------------------------------------------------------------
 * (8) ClaudeAgentProvider.stream with tools (M5.7.c slice 4)
 * --------------------------------------------------------------------- */

describe("ClaudeAgentProvider.stream with tools (M5.7.c slice 4)", () => {
  it("emits tool_call_start, tool_call_delta and message_stop with finishReason=tool_use", async () => {
    const fake = (() =>
      // eslint-disable-next-line @typescript-eslint/require-await -- generator yields a fixed sequence; no awaitable I/O inside.
      (async function* () {
        yield {
          type: "stream_event",
          event: {
            type: "content_block_start",
            index: 0,
            content_block: {
              type: "tool_use",
              id: "tu_1",
              name: "mcp__watchkeeper__notebook_remember",
            },
          },
        };
        yield {
          type: "stream_event",
          event: {
            type: "content_block_delta",
            index: 0,
            delta: { type: "input_json_delta", partial_json: '{"k":1}' },
          },
        };
        yield {
          type: "stream_event",
          event: { type: "content_block_stop", index: 0 },
        };
        yield {
          type: "assistant",
          message: {
            content: [
              {
                type: "tool_use",
                id: "tu_1",
                name: "mcp__watchkeeper__notebook_remember",
                input: { k: 1 },
              },
            ],
            usage: { input_tokens: 1, output_tokens: 1 },
          },
        };
        yield {
          type: "result",
          subtype: "success",
          usage: { input_tokens: 1, output_tokens: 1 },
          total_cost_usd: 0,
          stop_reason: "tool_use",
        };
      })()) as unknown as QueryImpl;
    const provider = new ClaudeAgentProvider({ queryImpl: fake });
    const events: StreamEvent[] = [];
    const sub = await provider.stream(
      {
        model: model("claude-3-5-sonnet-latest"),
        messages: [{ role: "user", content: "save k=1" }],
        tools: [
          { name: "notebook.remember", description: "save", inputSchema: { type: "object" } },
        ],
      },
      (e) => {
        events.push(e);
      },
    );
    // Drain by polling the subscription until it reports stopped.
    // Cast through unknown to access the concrete isStopped property that
    // ClaudeAgentStreamSubscription exposes but the StreamSubscription
    // interface does not declare (the interface only needs stop()).
    while (!(sub as unknown as { isStopped: boolean }).isStopped) {
      await new Promise((r) => setTimeout(r, 0));
    }
    const kinds = events.map((e) => e.kind);
    expect(kinds).toContain("tool_call_start");
    expect(kinds).toContain("tool_call_delta");
    const stop = events.find((e) => e.kind === "message_stop");
    expect(stop?.finishReason).toBe("tool_use");
  });
});
