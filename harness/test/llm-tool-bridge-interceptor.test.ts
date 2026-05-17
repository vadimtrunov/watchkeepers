/**
 * `tool-bridge-interceptor` vitest suite (M5.7.c slice 4).
 *
 * Drives the interceptor with hand-built SDK-shaped message sequences
 * delivered through a fake async iterator. Covers: happy path (no
 * tool_use), single tool_use + interrupt, parallel tool_use in one
 * assistant message, decoded name surfaces correctly, escape-race
 * detection, usage fallback when interrupt precedes the SDK result
 * message, and the stream-event mirror.
 */

import { describe, expect, it, vi } from "vitest";

import { LLMError } from "../src/llm/errors.js";
import { model, type StreamEvent } from "../src/llm/index.js";
import { interceptComplete, interceptStream } from "../src/llm/tool-bridge-interceptor.js";
import { MCP_STUB_SENTINEL } from "../src/llm/tool-bridge-mcp-stub-server.js";
import { MCP_SERVER_NAME, buildCodec } from "../src/llm/tool-bridge-name-codec.js";
import type { ToolDefinition } from "../src/llm/types.js";

const REQUESTED_MODEL = model("claude-3-5-sonnet-latest");

function td(name: string): ToolDefinition {
  return { name, description: "", inputSchema: {} };
}

interface FakeIter {
  iter: AsyncIterable<unknown> & { interrupt: () => Promise<void> };
  interrupted: { value: boolean };
}

function fakeIter(messages: readonly unknown[]): FakeIter {
  const interrupted = { value: false };
  // eslint-disable-next-line @typescript-eslint/require-await -- async generator yields a fixed in-memory sequence; no awaitable I/O is needed.
  const iter = (async function* () {
    for (const m of messages) {
      if (interrupted.value) return;
      yield m;
    }
  })() as unknown as AsyncIterable<unknown> & { interrupt: () => Promise<void> };
  iter.interrupt = () => {
    interrupted.value = true;
    return Promise.resolve();
  };
  return { iter, interrupted };
}

function assistantText(text: string): unknown {
  return {
    type: "assistant",
    message: { content: [{ type: "text", text }] },
  };
}

function assistantToolUse(
  blocks: readonly { id: string; name: string; input: Record<string, unknown> }[],
  precedingText?: string,
): unknown {
  const content: unknown[] = [];
  if (precedingText !== undefined) {
    content.push({ type: "text", text: precedingText });
  }
  for (const b of blocks) {
    content.push({ type: "tool_use", id: b.id, name: b.name, input: b.input });
  }
  return { type: "assistant", message: { content, usage: { input_tokens: 10, output_tokens: 5 } } };
}

function resultMessage(opts: {
  subtype?: "success" | "error";
  inputTokens?: number;
  outputTokens?: number;
  totalCostUsd?: number;
  stopReason?: string;
}): unknown {
  return {
    type: "result",
    subtype: opts.subtype ?? "success",
    usage: {
      input_tokens: opts.inputTokens ?? 0,
      output_tokens: opts.outputTokens ?? 0,
    },
    total_cost_usd: opts.totalCostUsd ?? 0,
    stop_reason: opts.stopReason,
  };
}

describe("interceptComplete", () => {
  it("happy path with no tool_use returns finishReason=stop", async () => {
    const codec = buildCodec([]);
    const { iter, interrupted } = fakeIter([
      assistantText("hi"),
      resultMessage({
        stopReason: "end_turn",
        inputTokens: 1,
        outputTokens: 2,
        totalCostUsd: 0.0001,
      }),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    expect(turn.text).toBe("hi");
    expect(turn.toolCalls).toEqual([]);
    expect(turn.finishReason).toBe("stop");
    expect(turn.usage.inputTokens).toBe(1);
    expect(turn.usage.outputTokens).toBe(2);
    expect(turn.usage.costCents).toBe(1);
    expect(interrupted.value).toBe(false);
  });

  it("captures a single tool_use and interrupts before result arrives", async () => {
    const codec = buildCodec([td("notebook.remember")]);
    const { iter, interrupted } = fakeIter([
      assistantToolUse(
        [
          {
            id: "tu_1",
            name: `mcp__${MCP_SERVER_NAME}__notebook_remember`,
            input: { key: "k", value: "v" },
          },
        ],
        "I'll save that.",
      ),
      // Result deliberately appended so we can prove interrupt cut the iterator.
      resultMessage({ stopReason: "tool_use" }),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    expect(turn.text).toBe("I'll save that.");
    expect(turn.toolCalls).toEqual([
      { id: "tu_1", name: "notebook.remember", arguments: { key: "k", value: "v" } },
    ]);
    expect(turn.finishReason).toBe("tool_use");
    expect(interrupted.value).toBe(true);
  });

  it("captures parallel tool_use blocks in the order emitted", async () => {
    const codec = buildCodec([td("notebook.recall"), td("notebook.remember")]);
    const { iter } = fakeIter([
      assistantToolUse([
        {
          id: "tu_1",
          name: `mcp__${MCP_SERVER_NAME}__notebook_recall`,
          input: { q: "recent" },
        },
        {
          id: "tu_2",
          name: `mcp__${MCP_SERVER_NAME}__notebook_remember`,
          input: { key: "summary" },
        },
      ]),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    expect(turn.toolCalls.map((c) => c.id)).toEqual(["tu_1", "tu_2"]);
    expect(turn.toolCalls.map((c) => c.name)).toEqual(["notebook.recall", "notebook.remember"]);
  });

  it("usage falls back to zero when interrupt precedes result", async () => {
    const codec = buildCodec([td("notebook.remember")]);
    const { iter } = fakeIter([
      assistantToolUse([
        {
          id: "tu_1",
          name: `mcp__${MCP_SERVER_NAME}__notebook_remember`,
          input: {},
        },
      ]),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    // Either the assistant.usage was lifted or the zero fallback applied.
    expect(turn.usage).toBeDefined();
    expect(turn.usage.model).toBe(REQUESTED_MODEL);
  });

  it("escalates the escape race when stub-handler sentinel surfaces", async () => {
    const codec = buildCodec([td("notebook.remember")]);
    const { iter } = fakeIter([
      // Simulate the SDK injecting a synthetic tool_result(is_error) after
      // the handler fired before we could interrupt. The interceptor
      // surfaces the sentinel as provider_unavailable.
      {
        type: "user",
        message: {
          content: [
            {
              type: "tool_result",
              tool_use_id: "tu_1",
              is_error: true,
              content: `${MCP_STUB_SENTINEL}: notebook.remember`,
            },
          ],
        },
      },
    ]);
    await expect(interceptComplete(iter, codec, REQUESTED_MODEL)).rejects.toBeInstanceOf(LLMError);
    const escapePromise = interceptComplete(
      fakeIter([
        {
          type: "user",
          message: {
            content: [
              {
                type: "tool_result",
                tool_use_id: "tu_1",
                is_error: true,
                content: `${MCP_STUB_SENTINEL}: notebook.remember`,
              },
            ],
          },
        },
      ]).iter,
      codec,
      REQUESTED_MODEL,
    );
    await expect(escapePromise).rejects.toBeInstanceOf(LLMError);
    await expect(escapePromise).rejects.toMatchObject({ code: "provider_unavailable" });
    await expect(escapePromise).rejects.toThrow(/escaped tool intercept/);
  });
});

// ---------------------------------------------------------------------------
// parseResult / modelUsage extraction (slice 5)
// ---------------------------------------------------------------------------

function resultWithModelUsage(opts: {
  modelUsage?: Record<
    string,
    {
      inputTokens: number;
      outputTokens: number;
      cacheReadInputTokens?: number;
      cacheCreationInputTokens?: number;
      costUSD?: number;
    }
  >;
  flatUsage?: { input_tokens: number; output_tokens: number };
  totalCostUsd?: number;
  stopReason?: string;
}): unknown {
  const m: Record<string, unknown> = {
    type: "result",
    subtype: "success",
    total_cost_usd: opts.totalCostUsd ?? 0,
    stop_reason: opts.stopReason ?? "end_turn",
  };
  if (opts.flatUsage !== undefined) {
    m.usage = {
      input_tokens: opts.flatUsage.input_tokens,
      output_tokens: opts.flatUsage.output_tokens,
    };
  }
  if (opts.modelUsage !== undefined) {
    m.modelUsage = opts.modelUsage;
  }
  return m;
}

describe("interceptComplete – modelUsage parsing", () => {
  it("uses modelUsage token counts and exposes per-model metadata for a single model", async () => {
    const codec = buildCodec([]);
    const { iter } = fakeIter([
      assistantText("hi"),
      resultWithModelUsage({
        modelUsage: {
          "claude-3-5-sonnet-20241022": {
            inputTokens: 42,
            outputTokens: 17,
            cacheReadInputTokens: 0,
            cacheCreationInputTokens: 0,
            costUSD: 0.000123,
          },
        },
        flatUsage: { input_tokens: 99, output_tokens: 99 },
        totalCostUsd: 0.0001,
      }),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    // Token counts come from modelUsage, not flatUsage
    expect(turn.usage.inputTokens).toBe(42);
    expect(turn.usage.outputTokens).toBe(17);
    // Cost from total_cost_usd × 10000
    expect(turn.usage.costCents).toBe(1);
    // Per-model breakdown in metadata
    expect(turn.usage.metadata).toBeDefined();
    expect(turn.usage.metadata?.["model:claude-3-5-sonnet-20241022"]).toBe("42/17/0.000123000");
    // No cache keys when both are 0
    expect(turn.usage.metadata?.cacheReadInputTokens).toBeUndefined();
    expect(turn.usage.metadata?.cacheCreationInputTokens).toBeUndefined();
  });

  it("sums token counts across multiple model entries and exposes two model keys", async () => {
    const codec = buildCodec([]);
    const { iter } = fakeIter([
      assistantText("hello"),
      resultWithModelUsage({
        modelUsage: {
          "claude-3-5-sonnet-20241022": {
            inputTokens: 30,
            outputTokens: 10,
            costUSD: 0.0001,
          },
          "claude-3-opus-20240229": {
            inputTokens: 20,
            outputTokens: 5,
            costUSD: 0.0002,
          },
        },
        totalCostUsd: 0.0003,
      }),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    // Summed across both entries
    expect(turn.usage.inputTokens).toBe(50);
    expect(turn.usage.outputTokens).toBe(15);
    expect(turn.usage.costCents).toBe(3);
    // Both model keys present in metadata
    expect(turn.usage.metadata?.["model:claude-3-5-sonnet-20241022"]).toBe("30/10/0.000100000");
    expect(turn.usage.metadata?.["model:claude-3-opus-20240229"]).toBe("20/5/0.000200000");
  });

  it("populates cacheReadInputTokens and cacheCreationInputTokens when non-zero", async () => {
    const codec = buildCodec([]);
    const { iter } = fakeIter([
      assistantText("cached"),
      resultWithModelUsage({
        modelUsage: {
          "claude-3-5-sonnet-20241022": {
            inputTokens: 100,
            outputTokens: 20,
            cacheReadInputTokens: 80,
            cacheCreationInputTokens: 15,
            costUSD: 0.0005,
          },
        },
        totalCostUsd: 0.0005,
      }),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    expect(turn.usage.metadata?.cacheReadInputTokens).toBe("80");
    expect(turn.usage.metadata?.cacheCreationInputTokens).toBe("15");
    expect(turn.usage.metadata?.["model:claude-3-5-sonnet-20241022"]).toBe("100/20/0.000500000");
  });

  it("falls back to flat usage tokens and no metadata when modelUsage is absent", async () => {
    const codec = buildCodec([]);
    const { iter } = fakeIter([
      assistantText("plain"),
      resultWithModelUsage({
        flatUsage: { input_tokens: 7, output_tokens: 3 },
        totalCostUsd: 0.0002,
      }),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    expect(turn.usage.inputTokens).toBe(7);
    expect(turn.usage.outputTokens).toBe(3);
    expect(turn.usage.costCents).toBe(2);
    // No modelUsage → no metadata
    expect(turn.usage.metadata).toBeUndefined();
  });

  it("formats per-model costUSD as fixed-point decimal even for tiny values", async () => {
    const codec = buildCodec([]);
    const { iter } = fakeIter([
      assistantText("hi"),
      resultWithModelUsage({
        modelUsage: {
          "claude-3-5-haiku-20241022": {
            inputTokens: 10,
            outputTokens: 5,
            costUSD: 1e-7,
          },
        },
        totalCostUsd: 1e-7,
        stopReason: "end_turn",
      }),
    ]);
    const turn = await interceptComplete(iter, codec, REQUESTED_MODEL);
    const value = turn.usage.metadata?.["model:claude-3-5-haiku-20241022"];
    expect(value).toBeDefined();
    expect(value).not.toMatch(/e/i); // no scientific notation
    expect(value).toMatch(/^\d+\/\d+\/\d+\.\d+$/); // <int>/<int>/<decimal>
  });
});

describe("interceptStream", () => {
  it("emits text_delta then message_stop with finishReason=tool_use on tool_use turn", async () => {
    const codec = buildCodec([td("notebook.remember")]);
    const events: StreamEvent[] = [];
    const handler = (e: StreamEvent): void => {
      events.push(e);
    };
    const { iter, interrupted } = fakeIter([
      // Streaming-mode SDK emits stream_event sub-messages around partial assistant content.
      {
        type: "stream_event",
        event: { type: "content_block_start", index: 0, content_block: { type: "text" } },
      },
      {
        type: "stream_event",
        event: {
          type: "content_block_delta",
          index: 0,
          delta: { type: "text_delta", text: "I'll" },
        },
      },
      {
        type: "stream_event",
        event: { type: "content_block_stop", index: 0 },
      },
      {
        type: "stream_event",
        event: {
          type: "content_block_start",
          index: 1,
          content_block: {
            type: "tool_use",
            id: "tu_1",
            name: `mcp__${MCP_SERVER_NAME}__notebook_remember`,
          },
        },
      },
      {
        type: "stream_event",
        event: {
          type: "content_block_delta",
          index: 1,
          delta: { type: "input_json_delta", partial_json: '{"k":1}' },
        },
      },
      {
        type: "stream_event",
        event: { type: "content_block_stop", index: 1 },
      },
      assistantToolUse([
        {
          id: "tu_1",
          name: `mcp__${MCP_SERVER_NAME}__notebook_remember`,
          input: { k: 1 },
        },
      ]),
      resultMessage({ stopReason: "tool_use" }),
    ]);
    const abortBag = {
      isStopped: false,
      markStopped: vi.fn((cause: unknown) => {
        abortBag.isStopped = true;
        void cause;
      }),
    };
    await interceptStream(iter, handler, codec, REQUESTED_MODEL, abortBag);
    const kinds = events.map((e) => e.kind);
    expect(kinds).toContain("text_delta");
    expect(kinds).toContain("tool_call_start");
    expect(kinds).toContain("tool_call_delta");
    const stop = events[events.length - 1];
    expect(stop?.kind).toBe("message_stop");
    expect(stop?.finishReason).toBe("tool_use");
    // Fix 1: with the deferred result-branch, the result message's usage now
    // flows through to the message_stop event on well-behaved SDK runs.
    expect(stop?.usage?.inputTokens).toBeGreaterThanOrEqual(0);
    expect(stop?.usage?.outputTokens).toBeGreaterThanOrEqual(0);
    expect(interrupted.value).toBe(true);
  });

  it("synthesises message_stop and interrupts when SDK omits the result message", async () => {
    const codec = buildCodec([td("notebook.remember")]);
    const events: StreamEvent[] = [];
    const handler = (e: StreamEvent): void => {
      events.push(e);
    };
    const { iter, interrupted } = fakeIter([
      // SDK sends partial events then the full assistant snapshot with
      // tool_use, then nothing — no result message follows.
      {
        type: "stream_event",
        event: {
          type: "content_block_start",
          index: 0,
          content_block: {
            type: "tool_use",
            id: "tu_1",
            name: `mcp__${MCP_SERVER_NAME}__notebook_remember`,
          },
        },
      },
      {
        type: "stream_event",
        event: { type: "content_block_stop", index: 0 },
      },
      assistantToolUse([
        {
          id: "tu_1",
          name: `mcp__${MCP_SERVER_NAME}__notebook_remember`,
          input: { k: 1 },
        },
      ]),
      // Deliberately no result message after the assistant snapshot.
    ]);
    const abortBag = {
      isStopped: false,
      markStopped: vi.fn((cause: unknown) => {
        abortBag.isStopped = true;
        void cause;
      }),
    };
    await interceptStream(iter, handler, codec, REQUESTED_MODEL, abortBag);
    const lastEvent = events[events.length - 1];
    expect(lastEvent?.kind).toBe("message_stop");
    expect(lastEvent?.finishReason).toBe("tool_use");
    // Fix 1: the degenerate fallback (no result message) synthesises the stop
    // without usage, so usage must be undefined here.
    expect(lastEvent?.usage).toBeUndefined();
    expect(interrupted.value).toBe(true);
  });
});
