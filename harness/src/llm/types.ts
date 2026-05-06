/**
 * Portable {@link LLMProvider} request / response types — TypeScript
 * twin of `core/pkg/llm/provider.go` (M5.3.c.c.a).
 *
 * Every type here mirrors a Go counterpart: field names follow the Go
 * names verbatim (lower-camel-case in TS vs. upper-camel-case in Go is
 * the only stylistic divergence). Doc comments cite the Go counterpart
 * inline so a reviewer can diff the two contracts at a glance.
 *
 * Discriminated unions are strict-by-default — payload structs do NOT
 * extend `Record<string, unknown>` so a TS `switch (ev.kind)` over
 * {@link StreamEventKind} narrows correctly without a `default` branch.
 *
 * NOTE: harness JSON-RPC loop integration lands in M5.3.c.c.c; the real
 * Claude Code adapter lands in M5.3.c.c.b. This file is the contract
 * surface only.
 */

/* eslint-disable @typescript-eslint/no-explicit-any -- mirrors the Go
   `map[string]any` shape on JSON-Schema-ish argument payloads; tightening
   to a concrete recursive type happens when a TS-side caller materialises
   one. */

/**
 * Branded provider-defined model identifier. Mirrors Go `llm.Model`.
 *
 * Bytes are provider-defined; the interface treats the value as opaque
 * and leaves catalogue validation to the concrete provider. Construct via
 * the {@link model} helper to keep the brand intact across module
 * boundaries.
 */
export type Model = string & { readonly __brand: "llm.Model" };

/**
 * Lift a plain string into the branded {@link Model} alias. Pure;
 * the helper exists so callers do not need to write the cast inline.
 */
export function model(value: string): Model {
  return value as Model;
}

/**
 * Conversational-turn speaker. Mirrors the Go `llm.Role` literal set
 * (`system` / `user` / `assistant` / `tool`).
 */
export const ROLES = ["system", "user", "assistant", "tool"] as const;
export type Role = (typeof ROLES)[number];

/**
 * Single conversational turn forwarded to the model. Mirrors Go
 * `llm.Message`.
 */
export interface Message {
  /** Speaker per {@link Role}. */
  readonly role: Role;
  /** Message body (plain text or provider-flavoured markdown). */
  readonly content: string;
  /**
   * Provider-specific extension bag (Anthropic content blocks,
   * tool-call ids paired with `role: 'tool'` messages, …). Optional.
   */
  readonly metadata?: Readonly<Record<string, string>>;
}

/**
 * Tool the model MAY request. Mirrors Go `llm.ToolDefinition`.
 *
 * `inputSchema: null` is rejected with {@link LLMErrorCode.InvalidPrompt}
 * by every provider — surfaced as `null` rather than `undefined` so
 * tests pinning the Go contract can pass `null` directly.
 */
export interface ToolDefinition {
  /** Manifest-declared tool name (e.g. `notebook.remember`). */
  readonly name: string;
  /** Human-readable description the model uses to decide when to call. */
  readonly description: string;
  /**
   * JSON-Schema-ish argument shape. `null` is invalid and surfaces as
   * {@link LLMErrorCode.InvalidPrompt}.
   */
  readonly inputSchema: Readonly<Record<string, any>> | null;
}

/**
 * Tool-call request the model emitted. Mirrors Go `llm.ToolCall`.
 */
export interface ToolCall {
  /** Model-assigned identifier preserved verbatim. */
  readonly id: string;
  /** Tool name (matches a {@link ToolDefinition.name}). */
  readonly name: string;
  /** JSON-shaped argument payload. */
  readonly arguments: Readonly<Record<string, any>>;
}

/**
 * Why a {@link CompleteResponse} turn ended. Mirrors the Go
 * `llm.FinishReason` literal set.
 */
export const FINISH_REASONS = ["stop", "max_tokens", "tool_use", "error"] as const;
export type FinishReason = (typeof FINISH_REASONS)[number];

/**
 * Post-turn token / cost accounting. Mirrors Go `llm.Usage`.
 */
export interface Usage {
  /** Model the turn ran against. */
  readonly model: Model;
  /** Prompt-side token count. Non-negative. */
  readonly inputTokens: number;
  /** Response-side token count. Non-negative. */
  readonly outputTokens: number;
  /**
   * Provider-computed cost in 1/10000 USD (`int64` in Go); zero when the
   * provider does not know its own catalogue.
   */
  readonly costCents: number;
  /** Provider-specific extension bag. */
  readonly metadata?: Readonly<Record<string, string>>;
}

/**
 * Synchronous request envelope. Mirrors Go `llm.CompleteRequest`.
 */
export interface CompleteRequest {
  /** Model identifier. Empty / off-catalogue → `model_not_supported`. */
  readonly model: Model;
  /** Composed system prompt. Empty means "no system prompt". */
  readonly system?: string;
  /** Ordered conversational sequence. Empty → `invalid_prompt`. */
  readonly messages: readonly Message[];
  /** Response-token cap. Zero means "provider default". */
  readonly maxTokens?: number;
  /** Sampling temperature in [0,1]. Zero means "provider default". */
  readonly temperature?: number;
  /** Optional toolset declaration. */
  readonly tools?: readonly ToolDefinition[];
  /** Provider-specific extension bag. */
  readonly metadata?: Readonly<Record<string, string>>;
}

/**
 * Synchronous response envelope. Mirrors Go `llm.CompleteResponse`.
 */
export interface CompleteResponse {
  /** Model's textual response. Empty when only tool calls were emitted. */
  readonly content: string;
  /** Ordered tool-call requests. */
  readonly toolCalls: readonly ToolCall[];
  /** Why the turn ended per {@link FinishReason}. */
  readonly finishReason: FinishReason;
  /** Provider-reported error text when `finishReason === 'error'`. */
  readonly errorMessage?: string;
  /** Post-turn accounting. Always populated. */
  readonly usage: Usage;
  /** Provider-specific extension bag. */
  readonly metadata?: Readonly<Record<string, string>>;
}

/**
 * Streaming request envelope. Mirrors Go `llm.StreamRequest` — same as
 * {@link CompleteRequest} minus the synchronous-only knobs.
 */
export interface StreamRequest {
  readonly model: Model;
  readonly system?: string;
  readonly messages: readonly Message[];
  readonly maxTokens?: number;
  readonly temperature?: number;
  readonly tools?: readonly ToolDefinition[];
  readonly metadata?: Readonly<Record<string, string>>;
}

/**
 * Stream-event kind discriminator. Mirrors Go `llm.StreamEventKind`.
 *
 * The full set is exhaustive: `switch (ev.kind)` over a {@link StreamEvent}
 * narrows to every variant without needing a `default` branch
 * (verified by the type-only test in M5.3.c.c.a's vitest suite).
 */
export const STREAM_EVENT_KINDS = [
  "text_delta",
  "tool_call_start",
  "tool_call_delta",
  "message_stop",
  "error",
] as const;
export type StreamEventKind = (typeof STREAM_EVENT_KINDS)[number];

/**
 * Single stream event delivered to a {@link StreamHandler}. Mirrors Go
 * `llm.StreamEvent`.
 *
 * The `kind` field is the discriminator; payload fields are optional and
 * carry data only for the kinds documented on the matching Go constant
 * in `core/pkg/llm/provider.go`.
 */
export interface StreamEvent {
  /** Discriminator per {@link StreamEventKind}. */
  readonly kind: StreamEventKind;
  /**
   * Incremental text chunk (`text_delta` / `tool_call_delta`).
   */
  readonly textDelta?: string;
  /**
   * Tool-call shell (`tool_call_start`) or correlation id only
   * (`tool_call_delta`).
   */
  readonly toolCall?: ToolCall;
  /** Turn-end discriminator (`message_stop`). */
  readonly finishReason?: FinishReason;
  /** Final accounting (`message_stop`). */
  readonly usage?: Usage;
  /** Error text (`error`). */
  readonly errorMessage?: string;
  /** Provider-specific extension bag. */
  readonly metadata?: Readonly<Record<string, string>>;
}

/**
 * Callback invoked by {@link LLMProvider.stream} for each event.
 * Mirrors Go `llm.StreamHandler`.
 *
 * Return a rejected Promise (or throw) to terminate the stream early —
 * the wrapped cause surfaces from the next
 * {@link StreamSubscription.stop} call.
 */
export type StreamHandler = (event: StreamEvent) => void | Promise<void>;

/**
 * Lifecycle handle returned by {@link LLMProvider.stream}. Mirrors Go
 * `llm.StreamSubscription`.
 *
 * `stop()` is idempotent — a second call returns the same result without
 * re-running shutdown.
 */
export interface StreamSubscription {
  /**
   * Signal the stream to close. Resolves once the dispatch loop exits.
   * Idempotent.
   */
  stop(): Promise<void>;
}

/**
 * Token-counting request envelope. Mirrors Go `llm.CountTokensRequest`.
 */
export interface CountTokensRequest {
  readonly model: Model;
  readonly system?: string;
  readonly messages: readonly Message[];
  readonly tools?: readonly ToolDefinition[];
  readonly metadata?: Readonly<Record<string, string>>;
}
