// cost.go implements [LoggingProvider] — a decorator around [llm.Provider]
// that emits one `llm_turn_cost_completed` keepers_log row per successful LLM
// turn (synchronous Complete or streaming Stream terminating in a
// MessageStop event). See package godoc (doc.go) for the full contract,
// payload shape, PII discipline, and logger-error policy.
package cost

import (
	"context"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
)

// EventTypeLLMCallCompleted is the closed-set keepers_log event_type the
// decorator emits per successful LLM turn. Pinned as a package constant
// so a future re-key is a one-line change here that the decorator AND
// downstream consumers (M6.2.a `report_cost`, M6.3.f rollups) pick up
// via the compiler.
//
// The wire value carries the "llm_turn_cost" prefix so the M6.2.a
// deployed prefix-based aggregator (defaultReportCostEventTypePrefix =
// "llm_turn_cost") matches every row this decorator emits.
const EventTypeLLMCallCompleted = "llm_turn_cost_completed"

// payloadKey* are the closed-set payload keys emitted on every
// `llm_turn_cost_completed` event. Hoisted to constants so a typo in one
// emit site cannot drift from the other; the M6.2.a aggregator (which
// reads `prompt_tokens` / `completion_tokens`) is shielded by the
// dual-emit pair.
//
// IMPORTANT: this package emits ONLY these keys plus the [Appender]'s
// envelope (event_id, timestamp, correlation_id, trace_id, span_id).
// NEVER message body, system prompt, tool-call arguments, or any other
// prompt content — see doc.go § "Closed-set payload — PII discipline".
const (
	payloadKeyAgentID          = "agent_id"
	payloadKeyModel            = "model"
	payloadKeyInputTokens      = "input_tokens"
	payloadKeyOutputTokens     = "output_tokens"
	payloadKeyPromptTokens     = "prompt_tokens"
	payloadKeyCompletionTokens = "completion_tokens"
	payloadKeyFinishReason     = "finish_reason"
)

// Appender is the minimal subset of [keeperslog.Writer] the decorator
// consumes — only the [keeperslog.Writer.Append] method is touched.
// Defined as an interface in this package so unit tests can substitute
// a hand-rolled fake that asserts the audit-row contract directly,
// and so production code never depends on the concrete *keeperslog.Writer
// type at all (mirrors the messenger.AuditAppender + keeperslog.LocalKeepClient
// import-cycle-break pattern documented in `docs/LESSONS.md`).
//
// `*keeperslog.Writer` satisfies this interface as-is; the compile-time
// assertion lives in `cost_test.go`.
type Appender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// Dependencies is the construction-time bag wired into [NewLoggingProvider].
// Held in a struct so a future addition (e.g. an org-id stamp, a
// per-model price catalogue) lands as a new field without breaking the
// constructor signature.
type Dependencies struct {
	// AgentID is the watchkeeper UUID the wrapper stamps onto every
	// emitted `agent_id` payload key. Required at construction; an
	// empty AgentID is allowed (the keepers_log row still records the
	// turn) but downstream M6.2.a `report_cost` per-agent narrowing
	// will not match. Phase-1 callers always populate this from the
	// runtime's manifest UUID.
	AgentID string

	// Logger is the audit-emit seam. Required; a nil Logger is rejected
	// at construction with a clear panic message — a decorator with no
	// logger silently drops every recording, which masks the very bug
	// this package exists to prevent.
	Logger Appender
}

// LoggingProvider is the decorator that wraps an inner [llm.Provider],
// forwards every method call verbatim, and emits a `llm_turn_cost_completed`
// keepers_log row per successful turn. Construct via [NewLoggingProvider];
// the zero value is not usable.
//
// LoggingProvider satisfies [llm.Provider] (compile-time assertion below)
// and is therefore safe to substitute anywhere the interface is consumed.
// All four methods (Complete, Stream, CountTokens, ReportCost) forward
// to the underlying provider; only Complete and Stream emit audit rows.
// CountTokens and ReportCost are forward-only — the cost recorder cares
// about completed turns, not preflight counts or duplicate accounting.
//
// Concurrency: safe for concurrent use after construction. Holds only
// immutable configuration; per-call state lives on the goroutine stack.
type LoggingProvider struct {
	inner   llm.Provider
	agentID string
	logger  Appender
}

// Compile-time assertion: [*LoggingProvider] satisfies [llm.Provider].
// AC6: pinned at the package level so `go build ./...` rejects a future
// drift in the [llm.Provider] surface (e.g. a fifth method added to the
// interface).
var _ llm.Provider = (*LoggingProvider)(nil)

// NewLoggingProvider constructs a [LoggingProvider] wrapping `underlying`
// with the supplied [Dependencies]. Both `underlying` and `deps.Logger`
// are required; a nil value for either panics with a clear message —
// matches the panic discipline of [keeperslog.New], [lifecycle.New], and
// [cron.New]. A LoggingProvider with no underlying provider or no
// logger cannot do anything useful, and silently no-oping every call
// would mask the bug.
//
// `deps.AgentID` is allowed to be empty; see [Dependencies.AgentID].
func NewLoggingProvider(underlying llm.Provider, deps Dependencies) *LoggingProvider {
	if underlying == nil {
		panic("cost: NewLoggingProvider: underlying provider must not be nil")
	}
	if deps.Logger == nil {
		panic("cost: NewLoggingProvider: deps.Logger must not be nil")
	}
	return &LoggingProvider{
		inner:   underlying,
		agentID: deps.AgentID,
		logger:  deps.Logger,
	}
}

// Complete forwards the request to the underlying provider and, on
// successful return (`err == nil`), emits one `llm_turn_cost_completed`
// keepers_log row carrying the closed-set token-accounting payload.
// On `err != nil`, the error is forwarded verbatim and NO event is
// emitted (per AC2: the LLM call did not complete a turn).
//
// A logger emit error does NOT short-circuit the LLM result — the
// (resp, nil) tuple still propagates to the caller. See doc.go
// § "Logger-error policy".
func (p *LoggingProvider) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	resp, err := p.inner.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	p.emit(ctx, req.Model, resp.FinishReason, resp.Usage)
	return resp, nil
}

// Stream forwards the streaming request to the underlying provider and
// intercepts the user's [llm.StreamHandler] so the decorator can
// observe every event. On a [llm.StreamEventKindMessageStop] event the
// decorator emits one `llm_turn_cost_completed` keepers_log row BEFORE
// forwarding the event to the user's handler (so the audit row is
// durable even when the user-side handler subsequently panics or
// returns an error). See doc.go § "Stream interception".
//
// Synchronous Stream errors and streams that close without a
// MessageStop emit zero rows. A nil handler is forwarded as-is so the
// underlying provider returns its native [llm.ErrInvalidHandler].
func (p *LoggingProvider) Stream(ctx context.Context, req llm.StreamRequest, handler llm.StreamHandler) (llm.StreamSubscription, error) {
	if handler == nil {
		// Forward the nil handler so the underlying provider returns
		// its native ErrInvalidHandler. We cannot wrap nil — there is
		// nothing to call.
		return p.inner.Stream(ctx, req, handler)
	}
	wrapped := func(streamCtx context.Context, ev llm.StreamEvent) error {
		if ev.Kind == llm.StreamEventKindMessageStop {
			// Emit BEFORE forwarding so the audit row is durable even
			// when the user's handler returns an error or panics. The
			// emit is a best-effort write; logger failure does not
			// short-circuit the user-handler delivery.
			p.emit(streamCtx, req.Model, ev.FinishReason, ev.Usage)
		}
		return handler(streamCtx, ev)
	}
	return p.inner.Stream(ctx, req, wrapped)
}

// CountTokens forwards verbatim. The decorator does NOT emit a
// keepers_log row for preflight token counting; cost recording is keyed
// off completed turns, not budget probes.
func (p *LoggingProvider) CountTokens(ctx context.Context, req llm.CountTokensRequest) (int, error) {
	return p.inner.CountTokens(ctx, req)
}

// ReportCost forwards verbatim. The provider's bookkeeping (per
// runtimeID) is the upstream concern; this decorator's audit-emit lane
// is independent and runs on every Complete/Stream turn directly. Mixing
// the two would risk duplicate accounting on the keepers_log side.
func (p *LoggingProvider) ReportCost(ctx context.Context, runtimeID string, usage llm.Usage) error {
	return p.inner.ReportCost(ctx, runtimeID, usage)
}

// emit composes the closed-set `llm_turn_cost_completed` payload and forwards
// it to the [Appender]. The Appender failure is intentionally swallowed
// — the LLM call must complete from the caller's perspective even when
// the keepers_log write fails (per AC7 / doc.go § "Logger-error policy").
// The Appender's own diagnostic sink (if wired via [keeperslog.WithLogger])
// records the failure on a separate channel.
func (p *LoggingProvider) emit(ctx context.Context, model llm.Model, finishReason llm.FinishReason, usage llm.Usage) {
	_, _ = p.logger.Append(ctx, keeperslog.Event{
		EventType: EventTypeLLMCallCompleted,
		Payload:   buildPayload(p.agentID, model, finishReason, usage),
	})
}

// buildPayload assembles the closed-set audit payload. Pulled into a
// helper so both Complete and Stream share the exact same shape — a
// drift between the two emit sites would silently corrupt the M6.2.a
// aggregator's totals. Returns a `map[string]any` because that is the
// shape [keeperslog.Event.Payload] consumes; the keepers_log writer
// JSON-marshals it under the envelope `data` key.
//
// PII guard: this function is the SOLE composer of the payload; if a
// future change adds a key, code review picks it up here. The function
// MUST NOT accept a [llm.CompleteRequest] / [llm.StreamRequest] / any
// type carrying message body, system prompt, or tool arguments — only
// the post-call metadata.
func buildPayload(agentID string, model llm.Model, finishReason llm.FinishReason, usage llm.Usage) map[string]any {
	return map[string]any{
		payloadKeyAgentID:          agentID,
		payloadKeyModel:            string(model),
		payloadKeyInputTokens:      usage.InputTokens,
		payloadKeyOutputTokens:     usage.OutputTokens,
		payloadKeyPromptTokens:     usage.InputTokens,
		payloadKeyCompletionTokens: usage.OutputTokens,
		payloadKeyFinishReason:     string(finishReason),
	}
}
