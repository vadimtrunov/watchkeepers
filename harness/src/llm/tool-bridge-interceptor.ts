/**
 * SDK-iterator interceptor for the M5.7.c slice 4 tool bridge.
 *
 * The portable `LLMProvider` contract requires `complete()` to return
 * `tool_use` requests as `CompleteResponse.toolCalls` for the runtime
 * to execute. The Agent SDK takes the opposite stance — it owns tool
 * execution and only surfaces results. To bridge the gap, the
 * interceptor watches the SDK iterator: the first `SDKAssistantMessage`
 * with `tool_use` blocks causes it to capture them all, call
 * `iter.interrupt()`, and return so the runtime can take over.
 *
 * Streaming mirror: `interceptStream` runs the same state machine but
 * additionally fans out portable `StreamEvent`s to the handler as SDK
 * `stream_event` partial-assistant frames arrive. The closing
 * `message_stop` (or, in some SDK builds, the absence of a closing
 * frame after the assistant tool_use snapshot arrives) triggers
 * interrupt.
 *
 * Stub-handler escape: if the SDK invokes a Watchkeeper stub handler
 * before `interrupt()` lands, the stub throws a sentinel-tagged error
 * (see `tool-bridge-mcp-stub-server.ts`). The SDK wraps it in a
 * synthetic `tool_result(is_error)` user message. The interceptor
 * scans incoming messages for the sentinel substring and escalates with
 * `LLMError.providerUnavailable("agent SDK escaped tool intercept: ...")`
 * so the runtime never silently observes a corrupted turn.
 */

import { LLMError } from "./errors.js";
import { MCP_STUB_SENTINEL } from "./tool-bridge-mcp-stub-server.js";
import type { ToolNameCodec } from "./tool-bridge-name-codec.js";
import type { FinishReason, Model, StreamEvent, StreamHandler, ToolCall, Usage } from "./types.js";

export interface InterceptedTurn {
  readonly toolCalls: readonly ToolCall[];
  readonly text: string;
  readonly finishReason: FinishReason;
  readonly usage: Usage;
  readonly errorMessage: string | undefined;
}

interface SdkIter {
  [Symbol.asyncIterator](): AsyncIterator<unknown>;
  interrupt?: () => Promise<void>;
}

export interface AbortBag {
  isStopped: boolean;
  markStopped(cause: unknown): void;
}

export async function interceptComplete(
  iter: SdkIter,
  codec: ToolNameCodec,
  requestedModel: Model,
): Promise<InterceptedTurn> {
  const it = iter[Symbol.asyncIterator]();
  let text = "";
  let assistantUsage: Usage | undefined;
  let usage: Usage | undefined;
  let finishReason: FinishReason = "stop";
  let errorMessage: string | undefined;
  let toolCalls: ToolCall[] = [];

  for (let next = await it.next(); next.done !== true; next = await it.next()) {
    const msg = next.value;
    checkForStubEscape(msg);

    const parsed = parseMessage(msg, codec, requestedModel);
    if (parsed.text !== undefined) text += parsed.text;
    if (parsed.assistantUsage !== undefined) assistantUsage = parsed.assistantUsage;
    if (parsed.errorMessage !== undefined) errorMessage = parsed.errorMessage;

    if (parsed.toolCalls !== undefined && parsed.toolCalls.length > 0) {
      toolCalls = [...parsed.toolCalls];
      finishReason = "tool_use";
      await safeInterrupt(iter);
      break;
    }

    if (parsed.resultUsage !== undefined) {
      usage = parsed.resultUsage;
      if (parsed.resultFinishReason !== undefined) {
        finishReason = parsed.resultFinishReason;
      }
      break;
    }
  }

  const finalUsage = usage ??
    assistantUsage ?? {
      model: requestedModel,
      inputTokens: 0,
      outputTokens: 0,
      costCents: 0,
    };

  return { text, toolCalls, finishReason, usage: finalUsage, errorMessage };
}

export async function interceptStream(
  iter: SdkIter,
  handler: StreamHandler,
  codec: ToolNameCodec,
  requestedModel: Model,
  abortBag: AbortBag,
): Promise<void> {
  const it = iter[Symbol.asyncIterator]();
  let activeToolID: string | undefined;
  let toolUseSnapshot: ToolCall[] = [];

  try {
    while (!abortBag.isStopped) {
      const next = await it.next();
      if (next.done === true) break;
      const msg = next.value;
      checkForStubEscape(msg);

      // Partial assistant streaming events.
      const evt = translatePartialEvent(
        msg,
        codec,
        () => activeToolID,
        (id) => {
          activeToolID = id;
        },
      );
      if (evt !== undefined) {
        try {
          await handler(evt);
        } catch (handlerErr: unknown) {
          abortBag.markStopped(handlerErr);
          return;
        }
      }

      // Full assistant snapshot. The SDK emits this before invoking tool
      // handlers, so this is our cue that the assistant turn is finished;
      // downstream SDK builds may or may not also emit a result message.
      // Capture the tool_use blocks for the eventual message_stop event.
      const parsedAssistant = parseAssistantSnapshot(msg, codec);
      if (parsedAssistant !== undefined && parsedAssistant.length > 0) {
        toolUseSnapshot = parsedAssistant;
      }

      // Result message → final stop event.
      const result = parseResult(msg, requestedModel);
      if (result !== undefined) {
        const finishReason: FinishReason =
          toolUseSnapshot.length > 0 ? "tool_use" : result.finishReason;
        const stopEvent: StreamEvent =
          result.usage !== undefined
            ? { kind: "message_stop", finishReason, usage: result.usage }
            : { kind: "message_stop", finishReason };
        try {
          await handler(stopEvent);
        } catch (handlerErr: unknown) {
          abortBag.markStopped(handlerErr);
        }
        if (toolUseSnapshot.length > 0) {
          await safeInterrupt(iter);
        }
        return;
      }

      // If the SDK does not also emit a result message for this turn (some
      // builds skip it once the assistant message contains tool_use), the
      // assistant snapshot we just captured IS the end of the turn. Emit
      // the synthesised message_stop and interrupt now — the result branch
      // above would have already returned if the SDK had sent one.
      if (parsedAssistant !== undefined && parsedAssistant.length > 0) {
        const stopEvent: StreamEvent = { kind: "message_stop", finishReason: "tool_use" };
        try {
          await handler(stopEvent);
        } catch (handlerErr: unknown) {
          abortBag.markStopped(handlerErr);
        }
        await safeInterrupt(iter);
        return;
      }
    }
  } catch (sdkErr: unknown) {
    const errEvent: StreamEvent = {
      kind: "error",
      errorMessage: errorMessageOf(sdkErr),
    };
    try {
      await handler(errEvent);
    } catch {
      // Handler errors during the synthesised error event are swallowed;
      // the SDK error is the authoritative cause.
    }
    abortBag.markStopped(sdkErr);
  }
}

/* ---------------------------------------------------------------------------
 * Internal helpers
 * ------------------------------------------------------------------------ */

function checkForStubEscape(msg: unknown): void {
  if (msg === null || typeof msg !== "object") return;
  const m = msg as Record<string, unknown>;

  // SDK injects a tool_result(is_error) inside a user message after the
  // stub handler threw.
  if (m.type === "user") {
    const wrapped = m.message as Record<string, unknown> | undefined;
    const content = wrapped?.content;
    if (Array.isArray(content)) {
      for (const block of content) {
        if (block === null || typeof block !== "object") continue;
        const b = block as Record<string, unknown>;
        if (b.type !== "tool_result") continue;
        if (b.is_error !== true) continue;
        const text = stringifyToolResultContent(b.content);
        if (text.includes(MCP_STUB_SENTINEL)) {
          throw LLMError.providerUnavailable(`agent SDK escaped tool intercept: ${text}`);
        }
      }
    }
  }
}

function stringifyToolResultContent(content: unknown): string {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content
      .map((b) =>
        typeof b === "object" && b !== null && "text" in b
          ? String((b as { text: unknown }).text)
          : "",
      )
      .join("");
  }
  return "";
}

interface ParsedMessage {
  text?: string;
  toolCalls?: readonly ToolCall[];
  assistantUsage?: Usage;
  resultUsage?: Usage;
  resultFinishReason?: FinishReason;
  errorMessage?: string;
}

function parseMessage(msg: unknown, codec: ToolNameCodec, requestedModel: Model): ParsedMessage {
  if (msg === null || typeof msg !== "object") return {};
  const m = msg as Record<string, unknown>;
  if (m.type === "assistant") return parseAssistant(m, codec, requestedModel);
  if (m.type === "result") {
    const r = parseResult(msg, requestedModel);
    if (r === undefined) return {};
    const out: ParsedMessage = { resultFinishReason: r.finishReason };
    if (r.usage !== undefined) out.resultUsage = r.usage;
    if (r.errorMessage !== undefined) out.errorMessage = r.errorMessage;
    return out;
  }
  return {};
}

function extractToolCalls(blocks: readonly unknown[], codec: ToolNameCodec): ToolCall[] {
  const calls: ToolCall[] = [];
  for (const block of blocks) {
    if (block === null || typeof block !== "object") continue;
    const b = block as Record<string, unknown>;
    if (b.type !== "tool_use") continue;
    const rawName = typeof b.name === "string" ? b.name : "";
    calls.push({
      id: typeof b.id === "string" ? b.id : "",
      name: codec.decode(rawName),
      arguments: (b.input as Readonly<Record<string, unknown>> | undefined) ?? {},
    });
  }
  return calls;
}

function parseAssistant(
  m: Record<string, unknown>,
  codec: ToolNameCodec,
  requestedModel: Model,
): ParsedMessage {
  const wrapped = m.message as Record<string, unknown> | undefined;
  if (wrapped === undefined) return {};
  const blocks = wrapped.content;
  const out: ParsedMessage = {};
  let textBuf = "";
  if (Array.isArray(blocks)) {
    for (const block of blocks) {
      if (block === null || typeof block !== "object") continue;
      const b = block as Record<string, unknown>;
      if (b.type === "text" && typeof b.text === "string") {
        textBuf += b.text;
      }
    }
    const calls = extractToolCalls(blocks, codec);
    if (calls.length > 0) out.toolCalls = calls;
  }
  if (textBuf.length > 0) out.text = textBuf;
  const usageRaw = wrapped.usage as Record<string, unknown> | undefined;
  if (usageRaw !== undefined) {
    out.assistantUsage = {
      model: requestedModel,
      inputTokens: typeof usageRaw.input_tokens === "number" ? usageRaw.input_tokens : 0,
      outputTokens: typeof usageRaw.output_tokens === "number" ? usageRaw.output_tokens : 0,
      costCents: 0,
    };
  }
  const errField = m.error;
  if (typeof errField === "string" && errField !== "") {
    out.errorMessage = errField;
  }
  return out;
}

function parseAssistantSnapshot(msg: unknown, codec: ToolNameCodec): ToolCall[] | undefined {
  if (msg === null || typeof msg !== "object") return undefined;
  const m = msg as Record<string, unknown>;
  if (m.type !== "assistant") return undefined;
  const wrapped = m.message as Record<string, unknown> | undefined;
  const blocks = wrapped?.content;
  if (!Array.isArray(blocks)) return undefined;
  return extractToolCalls(blocks, codec);
}

interface ParsedResult {
  readonly finishReason: FinishReason;
  readonly usage: Usage | undefined;
  readonly errorMessage: string | undefined;
}

function parseResult(msg: unknown, requestedModel: Model): ParsedResult | undefined {
  if (msg === null || typeof msg !== "object") return undefined;
  const m = msg as Record<string, unknown>;
  if (m.type !== "result") return undefined;
  if (m.subtype === "success") {
    const usageRaw = m.usage as Record<string, unknown> | undefined;
    const input = typeof usageRaw?.input_tokens === "number" ? usageRaw.input_tokens : 0;
    const output = typeof usageRaw?.output_tokens === "number" ? usageRaw.output_tokens : 0;
    const costUsd = typeof m.total_cost_usd === "number" ? m.total_cost_usd : 0;
    return {
      finishReason: mapStopReason(m.stop_reason),
      usage: {
        model: requestedModel,
        inputTokens: input,
        outputTokens: output,
        costCents: Math.round(costUsd * 10000),
      },
      errorMessage: undefined,
    };
  }
  const err = typeof m.error === "string" ? m.error : "unknown agent SDK error";
  return {
    finishReason: "error",
    usage: {
      model: requestedModel,
      inputTokens: 0,
      outputTokens: 0,
      costCents: 0,
    },
    errorMessage: err,
  };
}

function mapStopReason(sdk: unknown): FinishReason {
  switch (sdk) {
    case "end_turn":
    case "stop_sequence":
    case "pause_turn":
    case "refusal":
    case null:
    case undefined:
      return "stop";
    case "max_tokens":
      return "max_tokens";
    case "tool_use":
      return "tool_use";
    default:
      return "stop";
  }
}

function translatePartialEvent(
  msg: unknown,
  codec: ToolNameCodec,
  getActive: () => string | undefined,
  setActive: (id: string | undefined) => void,
): StreamEvent | undefined {
  if (msg === null || typeof msg !== "object") return undefined;
  const m = msg as Record<string, unknown>;
  if (m.type !== "stream_event") return undefined;
  const event = m.event as Record<string, unknown> | undefined;
  if (event === undefined) return undefined;
  const innerType = event.type;
  if (innerType === "content_block_start") {
    const block = event.content_block as Record<string, unknown> | undefined;
    if (block?.type === "tool_use") {
      const id = typeof block.id === "string" ? block.id : "";
      setActive(id);
      const rawName = typeof block.name === "string" ? block.name : "";
      return {
        kind: "tool_call_start",
        toolCall: { id, name: codec.decode(rawName), arguments: {} },
      };
    }
    return undefined;
  }
  if (innerType === "content_block_delta") {
    const delta = event.delta as Record<string, unknown> | undefined;
    if (delta?.type === "text_delta") {
      return {
        kind: "text_delta",
        textDelta: typeof delta.text === "string" ? delta.text : "",
      };
    }
    if (delta?.type === "input_json_delta") {
      const id = getActive() ?? "";
      return {
        kind: "tool_call_delta",
        textDelta: typeof delta.partial_json === "string" ? delta.partial_json : "",
        toolCall: { id, name: "", arguments: {} },
      };
    }
    return undefined;
  }
  if (innerType === "content_block_stop") {
    setActive(undefined);
    return undefined;
  }
  return undefined;
}

async function safeInterrupt(iter: SdkIter): Promise<void> {
  if (typeof iter.interrupt !== "function") return;
  try {
    await iter.interrupt();
  } catch {
    // Best-effort interrupt: re-interrupting an already-finished query
    // is a no-op in spec but tolerate exceptions defensively.
  }
}

function errorMessageOf(e: unknown): string {
  if (e instanceof Error) return e.message;
  if (typeof e === "string") return e;
  if (typeof e === "object" && e !== null) {
    try {
      return JSON.stringify(e);
    } catch {
      return "[unserialisable error value]";
    }
  }
  return String(e);
}
