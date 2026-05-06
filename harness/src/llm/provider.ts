/**
 * {@link LLMProvider} — portable interface every concrete LLM provider
 * implementation satisfies. TypeScript twin of the Go
 * `llm.Provider` interface in `core/pkg/llm/provider.go` (lines 437–482).
 *
 * The four methods cover the lifecycle of a single LLM turn:
 *
 *   - {@link LLMProvider.complete}    — synchronous turn.
 *   - {@link LLMProvider.stream}      — streaming turn dispatched to a handler.
 *   - {@link LLMProvider.countTokens} — pre-flight token budget check.
 *   - {@link LLMProvider.reportCost}  — post-flight cost accounting.
 *
 * Implementations are expected to be safe for concurrent use after
 * construction. The interface intentionally has NO knowledge of
 * Anthropic content blocks, OpenAI function-calling envelopes, or the
 * Claude Agent SDK — those concepts are concrete-provider concerns.
 *
 * Errors surface as rejected Promises carrying an {@link LLMError}; the
 * doc comment on each method enumerates the codes it may raise.
 */

// `LLMError` is referenced in JSDoc only — `import type` keeps the
// symbol available to the doc-link resolver without polluting the
// runtime module graph.
// eslint-disable-next-line @typescript-eslint/no-unused-vars
import type { LLMError } from "./errors.js";
import type {
  CompleteRequest,
  CompleteResponse,
  CountTokensRequest,
  StreamHandler,
  StreamRequest,
  StreamSubscription,
  Usage,
} from "./types.js";

/**
 * Portable LLM provider surface. See module doc comment.
 */
export interface LLMProvider {
  /**
   * Drive a single synchronous turn. Resolves with the model's response
   * with {@link CompleteResponse.usage} populated.
   *
   * Rejects with an {@link LLMError} carrying:
   *
   *   - `model_not_supported`  — `req.model` is empty or off-catalogue.
   *   - `invalid_prompt`       — `req.messages` is empty or a tool's
   *                              `inputSchema` is `null`.
   *   - `token_limit_exceeded` — prompt exceeds the model's context window.
   *   - `provider_unavailable` — the upstream service is unreachable.
   *
   * Tool-call requests from the model ride on
   * {@link CompleteResponse.toolCalls}; the runtime executes them.
   */
  complete(req: CompleteRequest): Promise<CompleteResponse>;

  /**
   * Drive a single streaming turn, dispatching {@link StreamEvent} values
   * to `handler` as the model generates. Resolves with the
   * {@link StreamSubscription} the caller uses to cancel the stream
   * early.
   *
   * Rejects with an {@link LLMError} carrying:
   *
   *   - `model_not_supported`  — `req.model` is empty or off-catalogue.
   *   - `invalid_prompt`       — `req.messages` is empty or a tool's
   *                              `inputSchema` is `null`.
   *   - `invalid_handler`      — `handler` is `null` / `undefined`.
   *   - `token_limit_exceeded` — prompt exceeds the model's context window.
   *   - `provider_unavailable` — the upstream service is unreachable.
   *
   * The handler runs in a task the provider owns; a thrown / rejected
   * handler terminates the stream and the wrapped cause surfaces from
   * {@link StreamSubscription.stop} as `LLMError(stream_closed, cause)`.
   * The final {@link Usage} rides on the `message_stop` event.
   */
  stream(req: StreamRequest, handler: StreamHandler): Promise<StreamSubscription>;

  /**
   * Return the deterministic token count the provider would charge for
   * `req`. Does NOT contact the model — the count comes from the
   * provider's local tokeniser.
   *
   * Rejects with an {@link LLMError} carrying:
   *
   *   - `model_not_supported` — `req.model` is empty or off-catalogue.
   *   - `invalid_prompt`      — `req.messages` is empty.
   */
  countTokens(req: CountTokensRequest): Promise<number>;

  /**
   * Record `usage` against the runtime session identified by
   * `runtimeID` for downstream cost tracking (M6.3 in the Go core).
   *
   * The provider's bookkeeping accumulates the values; the caller does
   * NOT need to read them back from this surface. Resolves cleanly even
   * when `runtimeID` is previously unseen — `reportCost` is the
   * create+update boundary, not a query. The caller MUST call
   * `reportCost` exactly once per completed turn.
   */
  reportCost(runtimeID: string, usage: Usage): Promise<void>;
}
