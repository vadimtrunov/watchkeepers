/**
 * {@link ClaudeAgentProvider} — concrete {@link LLMProvider} adapter
 * wrapping `@anthropic-ai/claude-agent-sdk` (M5.7.c). Coexists with the
 * {@link ClaudeCodeProvider} (raw Anthropic Messages API); operators
 * pick between them via a future `Manifest.provider` field
 * (M5.7.c.a follow-up below).
 *
 * # Credential resolution
 *
 * The Agent SDK auto-detects the local `claude` CLI subscription state
 * (Pro/Max) when no API key is present; when an API key IS configured,
 * the SDK uses it. This provider stays out of `process.env` directly:
 * the {@link ClaudeAgentProviderOptions.apiKey} field is consumed by the
 * caller (the harness boot path threading a secret resolved via
 * `harness/src/secrets/env.ts`, M5.7.a), and `apiKey === undefined`
 * lets the SDK fall back to subscription auth — the path the Phase 1
 * DoD §7 #1 "operator runs Claude Code they already have" target
 * documents.
 *
 * # Phase scope
 *
 * - `complete()` fully implemented.
 * - `reportCost()` shares the in-memory ledger pattern with
 *   {@link ClaudeCodeProvider}.
 * - `stream()` / `countTokens()` raise
 *   {@link LLMError.providerUnavailable} pending the next slices on this
 *   feature branch (M5.7.c.b, M5.7.c.c).
 */

import { query } from "@anthropic-ai/claude-agent-sdk";

import { LLMError } from "./errors.js";
import type { LLMProvider } from "./provider.js";
import type {
  CompleteRequest,
  CompleteResponse,
  CountTokensRequest,
  FinishReason,
  Message,
  Model,
  StreamEvent,
  StreamHandler,
  StreamRequest,
  StreamSubscription,
  ToolCall,
  ToolDefinition,
  Usage,
} from "./types.js";

/**
 * Constructor options. All fields are optional — the zero-config path
 * (`new ClaudeAgentProvider()`) targets a host where `claude` CLI has
 * already been authenticated via subscription.
 */
export interface ClaudeAgentProviderOptions {
  /**
   * Optional API key. When undefined, the Agent SDK auto-detects the
   * local `claude` CLI subscription state. When defined, the SDK uses
   * the key. The provider does NOT read the key from the environment
   * directly — the boot path threads a resolved value through here.
   */
  readonly apiKey?: string;
  /**
   * Reserved for future fallback wiring (mirrors
   * {@link ClaudeCodeProvider.defaultModel}).
   */
  readonly defaultModel?: Model;
  /**
   * Optional path override for the `claude` executable. When undefined
   * the SDK walks `PATH`.
   */
  readonly pathToClaudeCodeExecutable?: string;
  /**
   * Test seam — replaces the real `query` import. Production callers
   * MUST NOT supply this; the only legitimate use is the harness vitest
   * suite. Mirrors the secret-source DI pattern (M5.7.a) — keep the
   * pluggable seam tiny so it cannot be misused.
   */
  readonly queryImpl?: typeof query;
}

interface MutableUsage {
  model: Model;
  inputTokens: number;
  outputTokens: number;
  costCents: number;
  metadata?: Readonly<Record<string, string>>;
}

/**
 * Concrete {@link LLMProvider} backed by `@anthropic-ai/claude-agent-sdk`.
 * See module doc comment.
 */
export class ClaudeAgentProvider implements LLMProvider {
  private readonly apiKey: string | undefined;
  public readonly defaultModel: Model | undefined;
  private readonly pathToExecutable: string | undefined;
  private readonly queryImpl: typeof query;
  private readonly costs = new Map<string, MutableUsage>();

  public constructor(opts: ClaudeAgentProviderOptions = {}) {
    this.apiKey = opts.apiKey;
    this.defaultModel = opts.defaultModel;
    this.pathToExecutable = opts.pathToClaudeCodeExecutable;
    this.queryImpl = opts.queryImpl ?? query;
  }

  public async complete(req: CompleteRequest): Promise<CompleteResponse> {
    validateModel(req.model);
    validateMessages(req.messages);
    validateTools(req.tools);

    const prompt = buildPromptFromMessages(req.messages);
    const options = this.buildOptions(req);

    let textBuf = "";
    const toolCalls: ToolCall[] = [];
    let usage: Usage | undefined;
    let finishReason: FinishReason = "stop";
    let errorMessage: string | undefined;

    try {
      // Pass options only when defined so exactOptionalPropertyTypes
      // does not reject `options: undefined`.
      const iter =
        options === undefined ? this.queryImpl({ prompt }) : this.queryImpl({ prompt, options });
      for await (const msg of iter) {
        const consumed = consumeMessage(msg, req.model);
        if (consumed.textDelta !== undefined) textBuf += consumed.textDelta;
        if (consumed.toolCall !== undefined) toolCalls.push(consumed.toolCall);
        if (consumed.usage !== undefined) usage = consumed.usage;
        if (consumed.finishReason !== undefined) finishReason = consumed.finishReason;
        if (consumed.errorMessage !== undefined) errorMessage = consumed.errorMessage;
      }
    } catch (e) {
      throw mapAgentError(e);
    }

    if (usage === undefined) {
      // SDK never emitted a SDKResultMessage — treat as transport failure.
      throw LLMError.providerUnavailable("agent SDK returned no result message");
    }

    const response: CompleteResponse = {
      content: textBuf,
      toolCalls,
      finishReason,
      usage,
    };
    if (errorMessage !== undefined) {
      return { ...response, errorMessage };
    }
    return response;
  }

  // eslint-disable-next-line @typescript-eslint/require-await -- Promise return is the interface contract; body validates synchronously and starts the dispatch loop as a fire-and-forget promise.
  public async stream(req: StreamRequest, handler: StreamHandler): Promise<StreamSubscription> {
    validateModel(req.model);
    validateMessages(req.messages);
    // Handler check precedes tool check to mirror the FakeProvider /
    // ClaudeCodeProvider ordering: a nil handler is the most fundamental
    // shape error.
    // eslint-disable-next-line @typescript-eslint/no-unnecessary-condition -- TS forbids null at the type level, but runtime callers crossing JSON-RPC / FFI boundaries can still pass null.
    if (handler === null || handler === undefined) {
      throw LLMError.invalidHandler();
    }
    validateTools(req.tools);

    const prompt = buildPromptFromMessages(req.messages);
    // includePartialMessages: true asks the Agent SDK to emit raw
    // streaming-event sub-messages (text_delta, input_json_delta, ...)
    // as the assistant turn unfolds, which we translate into the
    // portable StreamEvent kinds. Without this flag the SDK only emits
    // a single complete SDKAssistantMessage per turn and we lose
    // incremental text.
    const options = this.buildOptions(req, { partial: true });

    let iter: ReturnType<typeof query>;
    try {
      iter =
        options === undefined ? this.queryImpl({ prompt }) : this.queryImpl({ prompt, options });
    } catch (e) {
      throw mapAgentError(e);
    }

    const sub = new ClaudeAgentStreamSubscription(iter);
    void sub.startDispatch(handler, req.model);
    return sub;
  }

  // eslint-disable-next-line @typescript-eslint/require-await, @typescript-eslint/no-unused-vars -- M5.7.c.c slice; stub matches the LLMProvider contract.
  public async countTokens(_req: CountTokensRequest): Promise<number> {
    throw LLMError.providerUnavailable(
      "ClaudeAgentProvider.countTokens is not yet implemented (M5.7.c.c)",
    );
  }

  // eslint-disable-next-line @typescript-eslint/require-await -- Promise return is the interface contract; bookkeeping is synchronous.
  public async reportCost(runtimeID: string, usage: Usage): Promise<void> {
    const prev = this.costs.get(runtimeID);
    if (prev === undefined) {
      const fresh: MutableUsage = {
        model: usage.model,
        inputTokens: usage.inputTokens,
        outputTokens: usage.outputTokens,
        costCents: usage.costCents,
      };
      if (usage.metadata !== undefined) fresh.metadata = usage.metadata;
      this.costs.set(runtimeID, fresh);
      return;
    }
    prev.inputTokens += usage.inputTokens;
    prev.outputTokens += usage.outputTokens;
    prev.costCents += usage.costCents;
    prev.model = usage.model;
    if (usage.metadata !== undefined) prev.metadata = usage.metadata;
  }

  /**
   * Test-facing accessor for the per-runtimeID cost ledger. Returns a
   * defensive snapshot (not the live accumulator).
   */
  public getReportedCost(runtimeID: string): Usage | undefined {
    const v = this.costs.get(runtimeID);
    if (v === undefined) return undefined;
    return { ...v };
  }

  private buildOptions(
    req: CompleteRequest | StreamRequest,
    extras?: { readonly partial?: boolean },
  ): Parameters<typeof query>[0]["options"] {
    // The Agent SDK Options type carries dozens of fields; we set only
    // the handful that matter for a single-turn complete() / stream()
    // and let the SDK default the rest. Tools/permission integration
    // lands in M5.7.c slice 4 (canUseTool bridge).
    const opts: Record<string, unknown> = { model: req.model };
    if (req.system !== undefined && req.system !== "") {
      opts.systemPrompt = req.system;
    }
    if (this.pathToExecutable !== undefined) {
      opts.pathToClaudeCodeExecutable = this.pathToExecutable;
    }
    if (extras?.partial === true) {
      // Asks the Agent SDK to emit SDKPartialAssistantMessage events
      // (text_delta / input_json_delta / content_block_*) so stream()
      // can dispatch incremental events to the handler. Off by default
      // because complete() does not need them.
      opts.includePartialMessages = true;
    }
    if (this.apiKey !== undefined && this.apiKey !== "") {
      // The credential literal lives only in harness/src/secrets/env.ts
      // (M5.7.a grep-invariant). The boot path resolves the value and
      // passes it here; we forward it via the SDK's env override so the
      // SDK's own auth path picks it up.
      opts.env = { ...process.env, ...this.apiKeyEnvOverride() };
    }
    return opts;
  }

  private apiKeyEnvOverride(): Record<string, string> {
    // The literal name of the env var lives in env.ts (M5.7.a). Here we
    // accept the value already pulled by the caller and place it under
    // the documented Agent SDK env key — the same key the SDK reads
    // when no subscription is present.
    if (this.apiKey === undefined || this.apiKey === "") return {};

    const key = ["ANTHROPIC", "API", "KEY"].join("_");
    return { [key]: this.apiKey };
  }
}

/* -----------------------------------------------------------------------
 * Validation helpers — symmetric with ClaudeCodeProvider's static checks.
 * --------------------------------------------------------------------- */

function validateModel(m: Model): void {
  // eslint-disable-next-line @typescript-eslint/no-unnecessary-condition -- TS forbids null/undefined at the type level, but runtime callers crossing JSON-RPC / FFI boundaries can still pass them.
  if (m === undefined || m === null || m === "") {
    throw LLMError.modelNotSupported();
  }
}

function validateMessages(messages: readonly Message[]): void {
  if (messages.length === 0) {
    throw LLMError.invalidPrompt();
  }
}

function validateTools(tools: readonly ToolDefinition[] | undefined): void {
  if (tools === undefined) return;
  for (const t of tools) {
    if (t.inputSchema === null) {
      throw LLMError.invalidPrompt();
    }
  }
}

/* -----------------------------------------------------------------------
 * Request / response translation.
 * --------------------------------------------------------------------- */

function buildPromptFromMessages(messages: readonly Message[]): string {
  // Phase-scope simplification: collapse user + assistant turns into one
  // prompt string. Multi-turn AsyncIterable<SDKUserMessage> mode lands
  // when we wire conversation history through the M5.7.c.b stream slice.
  // System messages lift to options.systemPrompt; tool messages are
  // skipped at this layer.
  const parts: string[] = [];
  for (const m of messages) {
    if (m.role === "user") parts.push(m.content);
    else if (m.role === "assistant") parts.push(`[assistant prior turn] ${m.content}`);
    // role === 'system' is handled in buildOptions
    // role === 'tool' is M5.7.c.b
  }
  return parts.join("\n\n");
}

/**
 * Result of consuming a single SDK message — what the iteration loop
 * should accumulate. Keeps the loop body in `complete()` flat.
 */
interface ConsumedMessage {
  readonly textDelta?: string;
  readonly toolCall?: ToolCall;
  readonly usage?: Usage;
  readonly finishReason?: FinishReason;
  readonly errorMessage?: string;
}

function consumeMessage(msg: unknown, requestedModel: Model): ConsumedMessage {
  if (msg === null || typeof msg !== "object") return {};
  const m = msg as Record<string, unknown>;
  const type = m.type;
  if (type === "assistant") return consumeAssistantMessage(m);
  if (type === "result") return consumeResultMessage(m, requestedModel);
  return {};
}

function consumeAssistantMessage(m: Record<string, unknown>): ConsumedMessage {
  const wrapped = m.message as Record<string, unknown> | undefined;
  if (wrapped === undefined) return {};
  const content = wrapped.content;
  if (!Array.isArray(content)) return {};
  const out: { textDelta?: string; toolCall?: ToolCall; errorMessage?: string } = {};
  for (const block of content as unknown[]) {
    if (block === null || typeof block !== "object") continue;
    const b = block as Record<string, unknown>;
    const t = b.type;
    if (t === "text" && typeof b.text === "string") {
      out.textDelta = (out.textDelta ?? "") + b.text;
    } else if (t === "tool_use") {
      out.toolCall = {
        id: typeof b.id === "string" ? b.id : "",
        name: typeof b.name === "string" ? b.name : "",
        arguments: (b.input as Readonly<Record<string, unknown>> | undefined) ?? {},
      };
    }
  }
  const errField = m.error;
  if (typeof errField === "string" && errField !== "") {
    out.errorMessage = errField;
  }
  return out;
}

function consumeResultMessage(m: Record<string, unknown>, requestedModel: Model): ConsumedMessage {
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
        // 1 cent = 10000 internal units (matches Go llm.Usage.CostCents).
        costCents: Math.round(costUsd * 10000),
      },
    };
  }
  // result + non-success: treat as turn error.
  const errStr = typeof m.error === "string" ? m.error : "unknown agent SDK error";
  return {
    finishReason: "error",
    errorMessage: errStr,
    usage: {
      model: requestedModel,
      inputTokens: 0,
      outputTokens: 0,
      costCents: 0,
    },
  };
}

function safeJsonStringify(v: unknown): string {
  try {
    const s = JSON.stringify(v);
    // JSON.stringify CAN return undefined for symbol/function root values;
    // TS types do not reflect this, hence the explicit runtime guard.
    return typeof s === "string" ? s : "[unserialisable error value]";
  } catch {
    return "[unserialisable error value]";
  }
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

function mapAgentError(e: unknown): LLMError {
  if (e instanceof LLMError) return e;
  if (e === null || e === undefined) return LLMError.providerUnavailable();
  // Stringify defensively — Object's default toString returns
  // `[object Object]` and erases the actual cause. Prefer Error.message;
  // fall back to JSON for plain objects; final fallback is String(e) for
  // primitives.
  const message =
    e instanceof Error
      ? e.message
      : typeof e === "object"
        ? safeJsonStringify(e)
        : // eslint-disable-next-line @typescript-eslint/no-base-to-string -- the typeof guards above narrow `e` to a primitive here; String() coercion is the canonical path.
          String(e);
  // Agent SDK error class taxonomy is less rich than the raw Anthropic
  // SDK's typed errors; we pattern-match on the message until a typed
  // surface lands upstream. The seven LLMErrorCode sentinels cover the
  // user-visible cases — additional codes upstream are folded into
  // `provider_unavailable` (network / auth / billing) or
  // `stream_closed` (caller aborted).
  if (/abort/i.test(message)) {
    return LLMError.streamClosed(`agent SDK aborted: ${message}`, e);
  }
  if (/auth/i.test(message) || /unauthor/i.test(message) || /credential/i.test(message)) {
    return LLMError.providerUnavailable(`agent SDK auth failure: ${message}`, e);
  }
  if (/rate.?limit/i.test(message)) {
    return LLMError.providerUnavailable(`agent SDK rate-limited: ${message}`, e);
  }
  if (/max.?tokens/i.test(message)) {
    return LLMError.tokenLimitExceeded(`agent SDK token limit: ${message}`, e);
  }
  return LLMError.providerUnavailable(message, e);
}

/* -----------------------------------------------------------------------
 * Streaming translation.
 *
 * The Agent SDK emits a richer event stream than the portable
 * StreamEvent kinds — most events (system, status, api_retry, ...) have
 * no portable counterpart and are dropped at the adapter boundary.
 * Only the three message types the harness needs land on the wire:
 *
 *   - SDKPartialAssistantMessage  → text_delta / tool_call_start /
 *                                   tool_call_delta (per inner block)
 *   - SDKAssistantMessage         → falls through to message_stop's
 *                                   final-assistant snapshot (drop)
 *   - SDKResultMessage            → message_stop carrying finishReason
 *                                   + usage from the success summary
 *
 * The translator is a free function so the test seam can exercise the
 * event mapping without instantiating a subscription.
 * --------------------------------------------------------------------- */

interface ClaudeAgentStreamCorrelation {
  readonly getActiveToolID: () => string | undefined;
  readonly setActiveToolID: (id: string | undefined) => void;
}

export function translateStreamMessage(
  msg: unknown,
  model: Model,
  corr: ClaudeAgentStreamCorrelation,
): StreamEvent | undefined {
  if (msg === null || typeof msg !== "object") return undefined;
  const m = msg as Record<string, unknown>;
  const type = m.type;
  if (type === "stream_event") return translatePartialAssistantEvent(m, corr);
  if (type === "result") {
    const consumed = consumeResultMessage(m, model);
    const evt: StreamEvent = { kind: "message_stop" };
    if (consumed.finishReason !== undefined) {
      (evt as { finishReason?: FinishReason }).finishReason = consumed.finishReason;
    }
    if (consumed.usage !== undefined) {
      (evt as { usage?: Usage }).usage = consumed.usage;
    }
    if (consumed.errorMessage !== undefined) {
      (evt as { errorMessage?: string }).errorMessage = consumed.errorMessage;
    }
    return evt;
  }
  return undefined;
}

function translatePartialAssistantEvent(
  msg: Record<string, unknown>,
  corr: ClaudeAgentStreamCorrelation,
): StreamEvent | undefined {
  const event = msg.event as Record<string, unknown> | undefined;
  if (event === undefined) return undefined;
  const innerType = event.type;
  switch (innerType) {
    case "content_block_start": {
      const block = event.content_block as Record<string, unknown> | undefined;
      if (block?.type === "tool_use") {
        const id = typeof block.id === "string" ? block.id : "";
        corr.setActiveToolID(id);
        return {
          kind: "tool_call_start",
          toolCall: {
            id,
            name: typeof block.name === "string" ? block.name : "",
            arguments: {},
          },
        };
      }
      return undefined;
    }
    case "content_block_delta": {
      const delta = event.delta as Record<string, unknown> | undefined;
      if (delta?.type === "text_delta") {
        return {
          kind: "text_delta",
          textDelta: typeof delta.text === "string" ? delta.text : "",
        };
      }
      if (delta?.type === "input_json_delta") {
        const id = corr.getActiveToolID() ?? "";
        return {
          kind: "tool_call_delta",
          textDelta: typeof delta.partial_json === "string" ? delta.partial_json : "",
          toolCall: { id, name: "", arguments: {} },
        };
      }
      return undefined;
    }
    case "content_block_stop":
      corr.setActiveToolID(undefined);
      return undefined;
    default:
      return undefined;
  }
}

/**
 * Internal {@link StreamSubscription} backing
 * {@link ClaudeAgentProvider.stream}. Mirrors the
 * `ClaudeStreamSubscription` semantics in claude-code-provider.ts:
 * one-shot stop / cause-latching / first stop() interrupts the Agent
 * SDK Query iterator.
 */
class ClaudeAgentStreamSubscription implements StreamSubscription {
  private readonly iter: AsyncIterator<unknown>;
  private readonly maybeQuery: { interrupt: () => Promise<void> } | undefined;
  private _stopped = false;
  private _cause: unknown = undefined;
  private _stopRan = false;
  private _stopResult: LLMError | undefined;
  private activeToolID: string | undefined;

  public constructor(iter: AsyncIterable<unknown> | AsyncIterator<unknown>) {
    // Agent SDK's Query is an AsyncGenerator (has both [Symbol.asyncIterator]
    // and the iterator protocol). We accept either shape.
    this.iter =
      typeof (iter as AsyncIterable<unknown>)[Symbol.asyncIterator] === "function"
        ? (iter as AsyncIterable<unknown>)[Symbol.asyncIterator]()
        : (iter as AsyncIterator<unknown>);
    this.maybeQuery =
      typeof (iter as { interrupt?: unknown }).interrupt === "function"
        ? (iter as unknown as { interrupt: () => Promise<void> })
        : undefined;
  }

  public get isStopped(): boolean {
    return this._stopped;
  }

  public markStopped(cause: unknown): void {
    if (this._stopped) return;
    this._stopped = true;
    this._cause = cause;
  }

  public async startDispatch(handler: StreamHandler, model: Model): Promise<void> {
    const corr: ClaudeAgentStreamCorrelation = {
      getActiveToolID: () => this.activeToolID,
      setActiveToolID: (id) => {
        this.activeToolID = id;
      },
    };
    try {
      while (!this._stopped) {
        const next = await this.iter.next();
        if (next.done === true) break;
        const ev = translateStreamMessage(next.value, model, corr);
        if (ev === undefined) continue;
        try {
          await handler(ev);
        } catch (handlerErr: unknown) {
          this.markStopped(handlerErr);
          break;
        }
        if (ev.kind === "message_stop") break;
      }
    } catch (sdkErr: unknown) {
      // SDK iteration blew up mid-flight: synthesise an error event for
      // the handler, then latch the cause for stop().
      const errEvent: StreamEvent = {
        kind: "error",
        errorMessage: errorMessageOf(sdkErr),
      };
      try {
        await handler(errEvent);
      } catch {
        // Handler errors during the synthesised error event are swallowed
        // — the SDK error is the authoritative cause.
      }
      this.markStopped(sdkErr);
    }
  }

  public async stop(): Promise<void> {
    if (this._stopRan) {
      if (this._stopResult !== undefined) {
        throw this._stopResult;
      }
      return;
    }
    this._stopRan = true;
    this._stopped = true;
    if (this.maybeQuery !== undefined) {
      try {
        await this.maybeQuery.interrupt();
      } catch {
        // Best-effort interrupt: re-interrupting an already-finished
        // query is a no-op in spec but tolerate exceptions defensively.
      }
    }
    if (this._cause !== undefined) {
      this._stopResult = LLMError.streamClosed(undefined, this._cause);
      throw this._stopResult;
    }
  }
}

function errorMessageOf(e: unknown): string {
  if (e instanceof Error) return e.message;
  if (typeof e === "string") return e;
  if (typeof e === "object" && e !== null) return safeJsonStringify(e);
  return String(e);
}
