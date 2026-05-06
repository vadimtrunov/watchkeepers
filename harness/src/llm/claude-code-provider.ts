/**
 * {@link ClaudeCodeProvider} — concrete {@link LLMProvider} adapter
 * wrapping `@anthropic-ai/sdk` (M5.3.c.c.b). Translates the portable
 * `CompleteRequest` / `StreamRequest` / `CountTokensRequest` envelopes
 * into the SDK's `messages.create` / `messages.stream` /
 * `messages.countTokens` calls and back, and maps SDK error classes
 * onto the seven {@link LLMErrorCode} sentinels.
 *
 * NOT a M5.5 manifest-aware boot, NOT a M5.7 secrets bridge — those land
 * on later sub-items. The provider takes its api key via the constructor
 * (no `process.env` lookup) so the harness or a test can wire any
 * source it wants.
 */

import Anthropic from "@anthropic-ai/sdk";

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
 * Constructor options. `apiKey` is REQUIRED — the provider does NOT read
 * `process.env.ANTHROPIC_API_KEY` (M5.7 owns secrets-interface plumbing).
 * `defaultModel` is optional; today every request specifies its own model
 * so the field is reserved for future fallback wiring.
 */
export interface ClaudeCodeProviderOptions {
  readonly apiKey: string;
  readonly defaultModel?: Model;
  readonly baseURL?: string;
}

/**
 * Concrete {@link LLMProvider} backed by `@anthropic-ai/sdk`. See module
 * doc comment.
 */
export class ClaudeCodeProvider implements LLMProvider {
  private readonly client: Anthropic;
  /**
   * Reserved for future fallback wiring (M5.5 manifest boot may call
   * `complete` without an inline model). Today every request specifies
   * its own model so the field is unused at the call sites.
   */
  public readonly defaultModel: Model | undefined;
  private readonly costs = new Map<string, MutableUsage>();

  public constructor(opts: ClaudeCodeProviderOptions) {
    // Pass `baseURL` only when defined so `exactOptionalPropertyTypes`
    // does not reject `baseURL: undefined`.
    const sdkOpts: { apiKey: string; baseURL?: string } = { apiKey: opts.apiKey };
    if (opts.baseURL !== undefined) sdkOpts.baseURL = opts.baseURL;
    this.client = new Anthropic(sdkOpts);
    this.defaultModel = opts.defaultModel;
  }

  public async complete(req: CompleteRequest): Promise<CompleteResponse> {
    validateModel(req.model);
    validateMessages(req.messages);
    validateTools(req.tools);

    const params = buildCreateParams(req);
    let raw: AnthropicCreateResponse;
    try {
      // Cast at the SDK boundary: our portable `inputSchema` is the
      // JSON-Schema-ish bag the LLMProvider contract documents; the
      // SDK's discriminated `ToolUnion` requires an additional `type`
      // field that the runtime is responsible for tagging (M5.3.c.b).
      // The cast keeps this layer narrow.
      raw = (await this.client.messages.create(
        params as unknown as Parameters<typeof this.client.messages.create>[0],
      )) as unknown as AnthropicCreateResponse;
    } catch (e) {
      throw mapAnthropicError(e);
    }
    return translateCompleteResponse(raw, req.model);
  }

  public async stream(req: StreamRequest, handler: StreamHandler): Promise<StreamSubscription> {
    validateModel(req.model);
    validateMessages(req.messages);
    // Handler check precedes tool check to mirror the FakeProvider /
    // Go ordering: a nil handler is the most fundamental shape error.
    // eslint-disable-next-line @typescript-eslint/no-unnecessary-condition -- TS forbids null at the type level, but runtime callers crossing JSON-RPC / FFI boundaries can still pass null; the sentinel mirrors Go's `ErrInvalidHandler`.
    if (handler === null || handler === undefined) {
      throw LLMError.invalidHandler();
    }
    validateTools(req.tools);

    const params = buildCreateParams(req);
    let sdkStream: AnthropicMessageStream;
    try {
      sdkStream = this.client.messages.stream(
        params as unknown as Parameters<typeof this.client.messages.stream>[0],
      ) as unknown as AnthropicMessageStream;
    } catch (e) {
      throw mapAnthropicError(e);
    }

    const sub = new ClaudeStreamSubscription(sdkStream);

    // Snapshot of the running tool_use block so `input_json_delta`
    // events can correlate back to the tool call id.
    let activeToolID: string | undefined;
    // Final usage / finish reason captured from `message_delta` so the
    // synthesised `message_stop` event carries them.
    let finalUsage: Usage | undefined;
    let finalFinishReason: FinishReason | undefined;

    try {
      for await (const ev of sdkStream) {
        if (sub.isStopped) break;
        const out = translateStreamEvent(ev, req.model, {
          getActiveToolID: () => activeToolID,
          setActiveToolID: (id) => {
            activeToolID = id;
          },
          recordFinal: (reason, usage) => {
            if (reason !== undefined) finalFinishReason = reason;
            if (usage !== undefined) finalUsage = usage;
          },
        });
        if (out === undefined) continue;
        // Synthesise message_stop with the latched final reason + usage.
        if (out.kind === "message_stop") {
          const synth: StreamEvent = {
            kind: "message_stop",
            ...(finalFinishReason !== undefined ? { finishReason: finalFinishReason } : {}),
            ...(finalUsage !== undefined ? { usage: finalUsage } : {}),
          };
          if (await dispatch(handler, synth, sub)) break;
          continue;
        }
        if (await dispatch(handler, out, sub)) break;
      }
    } catch (sdkErr: unknown) {
      // SDK iteration blew up mid-flight: synthesise an error event
      // for the handler, then latch the cause for stop().
      const errEvent: StreamEvent = {
        kind: "error",
        errorMessage: errorMessageOf(sdkErr),
      };
      try {
        await handler(errEvent);
      } catch {
        // Handler errors during the synthesised error event are
        // swallowed — the SDK error is the authoritative cause.
      }
      sub.markStopped(sdkErr);
    }

    return sub;
  }

  public async countTokens(req: CountTokensRequest): Promise<number> {
    validateModel(req.model);
    validateMessages(req.messages);

    const params = buildCountTokensParams(req);
    try {
      const resp = (await this.client.messages.countTokens(
        params as unknown as Parameters<typeof this.client.messages.countTokens>[0],
      )) as unknown as AnthropicCountTokensResponse;
      return resp.input_tokens;
    } catch (e) {
      throw mapAnthropicError(e);
    }
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
   * defensive snapshot (not the live accumulator) so callers cannot
   * silently mutate state.
   */
  public getReportedCost(runtimeID: string): Usage | undefined {
    const v = this.costs.get(runtimeID);
    if (v === undefined) return undefined;
    return { ...v };
  }
}

/* -----------------------------------------------------------------------
 * Validation helpers — symmetric with FakeProvider's static checks.
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

interface AnthropicMessageParam {
  role: "user" | "assistant";
  content: string;
}

interface AnthropicCreateParams {
  model: string;
  max_tokens: number;
  messages: AnthropicMessageParam[];
  system?: string;
  temperature?: number;
  tools?: AnthropicToolParam[];
  metadata?: Record<string, string>;
}

interface AnthropicToolParam {
  name: string;
  description: string;
  // Mirrors the portable `ToolDefinition.inputSchema` opacity. The
  // SDK's stricter `InputSchema` shape lives behind the call-site cast
  // in `complete` / `stream`, not here.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- mirrors `ToolDefinition.inputSchema`.
  input_schema: Readonly<Record<string, any>>;
}

interface AnthropicContentBlock {
  type: string;
  text?: string;
  id?: string;
  name?: string;
  input?: Record<string, unknown>;
}

interface AnthropicUsage {
  input_tokens: number;
  output_tokens: number;
}

interface AnthropicCreateResponse {
  id: string;
  stop_reason: string | null;
  content: AnthropicContentBlock[];
  usage: AnthropicUsage;
}

interface AnthropicCountTokensResponse {
  input_tokens: number;
}

// Default token cap when the caller does not supply one. The SDK
// requires `max_tokens` so we pick a defensive value rather than fail.
const DEFAULT_MAX_TOKENS = 1024;

function buildCreateParams(req: CompleteRequest | StreamRequest): AnthropicCreateParams {
  // Anthropic's REST contract: only `user` and `assistant` roles ride
  // the `messages[]` array. `system` lifts to the dedicated field;
  // `tool` messages are folded back into the prior user turn as
  // tool_result blocks — that translation is the runtime's job
  // (M5.3.c.c.c) and not handled here. For now we forward
  // `user`/`assistant` verbatim and ignore other roles to keep this
  // sub-item's surface narrow.
  const messages: AnthropicMessageParam[] = [];
  let systemPrompt = req.system;
  for (const m of req.messages) {
    if (m.role === "user" || m.role === "assistant") {
      messages.push({ role: m.role, content: m.content });
    } else if (m.role === "system") {
      // If the caller mixed an inline system message we concatenate.
      systemPrompt =
        systemPrompt === undefined || systemPrompt === ""
          ? m.content
          : `${systemPrompt}\n\n${m.content}`;
    }
    // tool-role messages: deliberately skipped at this layer.
  }

  const params: AnthropicCreateParams = {
    model: req.model,
    max_tokens: req.maxTokens ?? DEFAULT_MAX_TOKENS,
    messages,
  };
  if (systemPrompt !== undefined && systemPrompt !== "") params.system = systemPrompt;
  if (req.temperature !== undefined && req.temperature !== 0) {
    params.temperature = req.temperature;
  }
  if (req.tools !== undefined && req.tools.length > 0) {
    params.tools = req.tools.map(translateTool);
  }
  if (req.metadata !== undefined) {
    params.metadata = { ...req.metadata };
  }
  return params;
}

function buildCountTokensParams(req: CountTokensRequest): AnthropicCreateParams {
  // countTokens accepts the same param shape minus max_tokens; we reuse
  // buildCreateParams and let the SDK ignore unknown fields.
  return buildCreateParams({
    model: req.model,
    ...(req.system !== undefined ? { system: req.system } : {}),
    messages: req.messages,
    ...(req.tools !== undefined ? { tools: req.tools } : {}),
    ...(req.metadata !== undefined ? { metadata: req.metadata } : {}),
  });
}

function translateTool(t: ToolDefinition): AnthropicToolParam {
  // `inputSchema: null` is rejected upstream by validateTools; the
  // assertion is just for the type system.
  if (t.inputSchema === null) {
    throw LLMError.invalidPrompt();
  }
  return {
    name: t.name,
    description: t.description,
    input_schema: t.inputSchema,
  };
}

function translateCompleteResponse(raw: AnthropicCreateResponse, model: Model): CompleteResponse {
  const textParts: string[] = [];
  const toolCalls: ToolCall[] = [];
  for (const block of raw.content) {
    if (block.type === "text" && typeof block.text === "string") {
      textParts.push(block.text);
    } else if (block.type === "tool_use") {
      toolCalls.push({
        id: block.id ?? "",
        name: block.name ?? "",
        arguments: block.input ?? {},
      });
    }
  }
  const usage: Usage = {
    model,
    inputTokens: raw.usage.input_tokens,
    outputTokens: raw.usage.output_tokens,
    costCents: 0,
  };
  return {
    content: textParts.join(""),
    toolCalls,
    finishReason: mapStopReason(raw.stop_reason),
    usage,
    metadata: { anthropic_id: raw.id },
  };
}

function mapStopReason(sdk: string | null | undefined): FinishReason {
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

/* -----------------------------------------------------------------------
 * Streaming translation.
 * --------------------------------------------------------------------- */

interface AnthropicMessageStream extends AsyncIterable<AnthropicStreamEvent> {
  readonly controller: { abort: () => void };
}

interface AnthropicStreamEvent {
  type: string;
  index?: number;
  content_block?: AnthropicContentBlock;
  delta?: {
    type?: string;
    text?: string;
    partial_json?: string;
    stop_reason?: string | null;
  };
  usage?: AnthropicUsage;
}

interface StreamCorrelation {
  getActiveToolID: () => string | undefined;
  setActiveToolID: (id: string | undefined) => void;
  recordFinal: (reason: FinishReason | undefined, usage: Usage | undefined) => void;
}

function translateStreamEvent(
  ev: AnthropicStreamEvent,
  model: Model,
  corr: StreamCorrelation,
): StreamEvent | undefined {
  switch (ev.type) {
    case "content_block_start": {
      const block = ev.content_block;
      if (block?.type === "tool_use") {
        const id = block.id ?? "";
        corr.setActiveToolID(id);
        return {
          kind: "tool_call_start",
          toolCall: { id, name: block.name ?? "", arguments: {} },
        };
      }
      // Text/other block-starts have no portable counterpart.
      return undefined;
    }
    case "content_block_delta": {
      const delta = ev.delta;
      if (delta?.type === "text_delta") {
        return { kind: "text_delta", textDelta: delta.text ?? "" };
      }
      if (delta?.type === "input_json_delta") {
        const id = corr.getActiveToolID() ?? "";
        return {
          kind: "tool_call_delta",
          textDelta: delta.partial_json ?? "",
          toolCall: { id, name: "", arguments: {} },
        };
      }
      return undefined;
    }
    case "content_block_stop": {
      corr.setActiveToolID(undefined);
      return undefined;
    }
    case "message_delta": {
      const usage =
        ev.usage !== undefined
          ? {
              model,
              inputTokens: ev.usage.input_tokens,
              outputTokens: ev.usage.output_tokens,
              costCents: 0,
            }
          : undefined;
      const reason =
        ev.delta?.stop_reason !== undefined && ev.delta.stop_reason !== null
          ? mapStopReason(ev.delta.stop_reason)
          : undefined;
      corr.recordFinal(reason, usage);
      return undefined;
    }
    case "message_stop":
      return { kind: "message_stop" };
    default:
      return undefined;
  }
}

async function dispatch(
  handler: StreamHandler,
  ev: StreamEvent,
  sub: ClaudeStreamSubscription,
): Promise<boolean> {
  try {
    await handler(ev);
    return false;
  } catch (handlerErr: unknown) {
    sub.markStopped(handlerErr);
    return true;
  }
}

/**
 * Internal {@link StreamSubscription} backing
 * {@link ClaudeCodeProvider.stream}. Mirrors `FakeStreamSubscription`'s
 * one-shot stop / cause-latching semantics; first stop() also calls
 * `controller.abort()` on the SDK stream.
 */
class ClaudeStreamSubscription implements StreamSubscription {
  private readonly sdkStream: AnthropicMessageStream;
  private _stopped = false;
  private _cause: unknown = undefined;
  private _stopRan = false;
  private _stopResult: LLMError | undefined;

  public constructor(sdkStream: AnthropicMessageStream) {
    this.sdkStream = sdkStream;
  }

  public get isStopped(): boolean {
    return this._stopped;
  }

  /** Latch a transport / handler cause; idempotent against re-marking. */
  public markStopped(cause: unknown): void {
    if (this._stopped) return;
    this._stopped = true;
    this._cause = cause;
  }

  // eslint-disable-next-line @typescript-eslint/require-await -- Promise return is the interface contract; SDK abort is synchronous.
  public async stop(): Promise<void> {
    if (this._stopRan) {
      if (this._stopResult !== undefined) {
        throw this._stopResult;
      }
      return;
    }
    this._stopRan = true;
    this._stopped = true;
    try {
      this.sdkStream.controller.abort();
    } catch {
      // Best-effort abort: re-aborting an already-finished stream is a
      // no-op in the spec but tolerate exceptions defensively.
    }
    if (this._cause !== undefined) {
      this._stopResult = LLMError.streamClosed(undefined, this._cause);
      throw this._stopResult;
    }
  }
}

/* -----------------------------------------------------------------------
 * Error mapping — single source of truth for SDK error → LLMError.
 * --------------------------------------------------------------------- */

const CONTEXT_LENGTH_RE = /context.{0,20}length|too long|exceed.{0,20}context/i;
const MODEL_NOT_FOUND_RE = /model.{0,20}(not.found|not.supported|invalid)/i;

/**
 * Translate a thrown SDK value into an {@link LLMError}. Centralises the
 * mapping rules for `complete` / `stream` / `countTokens` so the error
 * contract stays consistent across surfaces.
 *
 * Mapping summary (full table on TASK AC6):
 *
 *   - `AuthenticationError` / `RateLimitError` / `InternalServerError` /
 *     `APIConnectionError` / `APIConnectionTimeoutError` →
 *     `provider_unavailable`.
 *   - `BadRequestError` matched against context-length /
 *     model-not-found regexes → `token_limit_exceeded` /
 *     `model_not_supported`; otherwise `invalid_prompt`.
 *   - HTTP status `529` (Anthropic overloaded) → `provider_unavailable`.
 *   - Any other thrown value → `provider_unavailable` wrapping cause.
 *
 * NOTE: `@anthropic-ai/sdk@^0.94.0` does NOT export a separate
 * `OverloadedError` class — overloaded responses surface as a generic
 * `APIError` with status 529, so we detect them via the status field.
 */
export function mapAnthropicError(e: unknown): LLMError {
  if (e instanceof LLMError) return e;

  if (e instanceof Anthropic.AuthenticationError) {
    return LLMError.providerUnavailable(undefined, e);
  }
  if (e instanceof Anthropic.RateLimitError) {
    return LLMError.providerUnavailable(undefined, e);
  }
  if (e instanceof Anthropic.InternalServerError) {
    return LLMError.providerUnavailable(undefined, e);
  }
  if (e instanceof Anthropic.APIConnectionError) {
    // APIConnectionTimeoutError extends APIConnectionError.
    return LLMError.providerUnavailable(undefined, e);
  }
  if (e instanceof Anthropic.BadRequestError) {
    const msg = e.message;
    if (CONTEXT_LENGTH_RE.test(msg)) {
      return LLMError.tokenLimitExceeded(undefined, e);
    }
    if (MODEL_NOT_FOUND_RE.test(msg)) {
      return LLMError.modelNotSupported(undefined, e);
    }
    return LLMError.invalidPrompt(undefined, e);
  }
  // Generic APIError catches Overloaded (status 529) and any other
  // status that did not get its own subclass.
  if (e instanceof Anthropic.APIError) {
    return LLMError.providerUnavailable(undefined, e);
  }
  return LLMError.providerUnavailable(undefined, e);
}

function errorMessageOf(e: unknown): string {
  if (e instanceof Error) return e.message;
  if (typeof e === "string") return e;
  return String(e);
}

/* -----------------------------------------------------------------------
 * Internals.
 * --------------------------------------------------------------------- */

interface MutableUsage {
  model: Model;
  inputTokens: number;
  outputTokens: number;
  costCents: number;
  metadata?: Readonly<Record<string, string>>;
}
