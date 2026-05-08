// Package cost is the per-Watchkeeper LLM token-spend recorder. ROADMAP §M6 → M6.3 → M6.3.e.
//
// The package exposes a single decorator type [LoggingProvider] that wraps
// any [llm.Provider] implementation and intercepts the post-call [llm.Usage]
// the underlying provider returns. On every successful turn (synchronous
// [llm.Provider.Complete] or streaming [llm.Provider.Stream] terminating
// in a [llm.StreamEventKindMessageStop] event), the decorator emits one
// `llm_turn_cost_completed` keepers_log row carrying the closed-set token
// accounting payload. Downstream cost rollups (M6.3.f) and the M6.2.a
// `report_cost` Watchmaster read tool consume these rows.
//
// # Why a decorator
//
// The portable [llm.Provider] interface intentionally has no awareness of
// the keepers_log audit chain — concrete providers (M5.2.b's Claude Code
// provider, the in-process fake, future Anthropic / OpenAI providers) all
// satisfy the same surface. Interposing the audit-emit at the Provider
// seam keeps the cost-recording lane out of every concrete provider's
// hot path AND makes the recording mechanism testable in isolation
// (real-fakes pattern: real *keeperslog.Writer over fake LocalKeepClient
// over fake llm.Provider).
//
// # Closed-set payload — PII discipline
//
// Per the M2b.7 redaction discipline (and aligned with the M6.3.a
// inbound-handler audit-emit pattern), the emitted event NEVER carries
// message body, system prompt text, tool-call arguments, or any other
// prompt content. Only metadata keys are written:
//
//	{
//	  "agent_id":          "<string, from Dependencies.AgentID>",
//	  "model":             "<string, from req.Model>",
//	  "input_tokens":      <int, from Usage.InputTokens>,
//	  "output_tokens":     <int, from Usage.OutputTokens>,
//	  "prompt_tokens":     <int, dual-emit alias for M6.2.a aggregator>,
//	  "completion_tokens": <int, dual-emit alias for M6.2.a aggregator>,
//	  "finish_reason":     "<string, from response/event FinishReason>"
//	}
//
// The `prompt_tokens` / `completion_tokens` keys are emitted alongside
// the canonical `input_tokens` / `output_tokens` keys so the M6.2.a
// `ReportCost` Watchmaster tool (which scans for the legacy vocabulary)
// aggregates the rows without an extra alignment step. The two pairs
// always carry the same numeric values; future rollups SHOULD prefer
// the `input_tokens` / `output_tokens` shape per the [llm.Usage] type.
//
// # Logger-error policy (AC7)
//
// The decorator is a recording surface, NOT a guard. A failure in the
// underlying [keeperslog.Writer] (network outage, server rejection,
// transient backend error) does NOT short-circuit the user-facing LLM
// result: the original [llm.CompleteResponse] / [llm.StreamSubscription]
// still propagates to the caller. Audit-emit failures are observability
// concerns to be reported on a separate channel (the [keeperslog.Writer]
// already logs the failure via its [keeperslog.WithLogger] sink). The
// LLM call result wins. This contract is pinned by
// `TestLoggingProvider_LoggerEmitError_PropagatesLLMResult` in
// `cost_test.go`.
//
// # Stream interception
//
// [LoggingProvider.Stream] wraps the caller's [llm.StreamHandler] in a
// thin interceptor that observes every event the provider dispatches.
// On a [llm.StreamEventKindMessageStop] event the interceptor emits
// the cost row BEFORE forwarding the event to the user's handler — the
// keepers_log row is durable before the user-side handler can panic
// or short-circuit. Synchronous [llm.Provider.Stream] errors and
// streams that close without a MessageStop emit zero rows (the
// stream did not complete a turn).
//
// # Concurrency
//
// [LoggingProvider] is safe for concurrent use after construction. The
// decorator holds only immutable configuration; per-call state lives
// on the goroutine stack. The underlying [llm.Provider] and the
// [Appender] (the seam against [*keeperslog.Writer]) are required to
// be goroutine-safe — both Phase-1 implementations (FakeProvider, the
// keeperslog Writer) are.
//
// # Out of scope (deferred)
//
//   - Daily / weekly time-window rollups (M6.3.f).
//   - Budget enforcement / overage Slack alerts (separate future
//     M-stage).
//   - Cost in dollars (model→price mapping); M6.3.f's rollup owns
//     the price catalogue. This package emits token counts only.
//   - Wiring the decorator into runtime startup; M6.3.e ships the
//     decorator + tests, the runtime composes it in a follow-up.
//   - TS-side harness instrumentation; the harness LLM call path is
//     out of scope for this Go package.
package cost
