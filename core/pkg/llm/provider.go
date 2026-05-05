package llm

import (
	"context"
)

// Model is the provider-defined model identifier (e.g.
// `claude-sonnet-4`, `gpt-4o`). The bytes are provider-defined; the
// interface package treats the value as an opaque string and the
// provider validates it against its catalogue.
//
// Callers typically pass [Model] verbatim from a [runtime.Manifest.Model]
// projection (M5.5 loader) — the runtime forwards the manifest's model
// string to the provider as-is, no parsing.
type Model string

// Role discriminates the speaker of a [Message] in the conversational
// sequence. The set is small and intentionally additive — future roles
// extend the type without breaking switch-statements that include a
// default branch.
type Role string

const (
	// RoleSystem identifies the system / instruction message. Most
	// providers carry the system prompt out-of-band (Anthropic's
	// `system` top-level field); this package routes the system prompt
	// via [CompleteRequest.System] / [StreamRequest.System] and reserves
	// [RoleSystem] in the [Message] sequence for providers that prefer
	// the messages-only shape.
	RoleSystem Role = "system"

	// RoleUser identifies a user-turn message — the human input the
	// agent reacts to.
	RoleUser Role = "user"

	// RoleAssistant identifies an assistant-turn message — prior model
	// output the caller is replaying for context.
	RoleAssistant Role = "assistant"

	// RoleTool identifies a tool-result message — the output of a
	// previously invoked tool the model requested. The pairing of
	// tool-call id and tool-result message rides on
	// [Message.Metadata]; the interface package does not prescribe a
	// key vocabulary.
	RoleTool Role = "tool"
)

// Message is a single turn in the conversational sequence the provider
// sends to the model. The shape is intentionally minimal — a role plus
// the content body plus a metadata bag for provider-specific extensions.
//
// Message is distinct from [runtime.Message] (the user-input shape the
// runtime accepts via [runtime.AgentRuntime.SendMessage]). The runtime
// converts its [runtime.Message] into a [Message] sequence when driving
// the LLM — the conversion shape is the runtime's concern.
type Message struct {
	// Role is the speaker per [Role]. Required.
	Role Role

	// Content is the message body the provider forwards to the model.
	// Plain text or provider-flavoured markdown; multi-part content
	// (images, code blocks with language hints, citations) rides on
	// [Message.Metadata] until a portable abstraction emerges. Required.
	Content string

	// Metadata carries provider-specific extensions (Anthropic content
	// blocks, tool-call ids paired with [RoleTool] messages, …).
	// Providers consume only the keys they recognise. Nil is fine.
	Metadata map[string]string
}

// ToolDefinition describes a tool the model MAY request. The provider
// forwards the schema to the model so the model can emit a tool-call
// request the runtime ([runtime.AgentRuntime.InvokeTool]) executes.
//
// ToolDefinition is intentionally distinct from [runtime.ToolCall]:
// the former is the schema declaration sent INTO the model, the latter
// is the call request the model emits OUT. Concrete providers translate
// between the two when wiring [CompleteResponse.ToolCalls] /
// [StreamEvent].
type ToolDefinition struct {
	// Name is the tool's manifest-declared name (e.g.
	// `notebook.remember`). Required; aligns with
	// [runtime.Manifest.Toolset] entries so the runtime can authorise
	// model-emitted tool calls without a name-translation step.
	Name string

	// Description is the human-readable description the model uses to
	// decide when to call the tool. Required; the provider does not
	// edit it.
	Description string

	// InputSchema is the JSON-schema-shaped argument schema (an
	// object the provider serialises into the model's tool-spec format).
	// Required; nil schemas are rejected by [Provider.Complete] /
	// [Provider.Stream] with [ErrInvalidPrompt].
	InputSchema map[string]any
}

// ToolCall is a tool-call request the model emitted in its response.
// The provider populates these from the model's native tool-call
// representation; the runtime ([runtime.AgentRuntime.InvokeTool])
// executes them.
type ToolCall struct {
	// ID is the model-assigned identifier the provider preserves
	// verbatim. The runtime echoes the id back when it returns the
	// tool's result via a follow-up [Message] with [RoleTool] so the
	// model can correlate.
	ID string

	// Name is the tool name the model requested (matches a
	// [ToolDefinition.Name] from the request). Required.
	Name string

	// Arguments is the JSON-shaped argument payload the model emitted.
	// Empty / nil is fine — the tool decides whether its signature
	// requires arguments. The provider does not validate against the
	// [ToolDefinition.InputSchema]; that is the runtime's / tool's job.
	Arguments map[string]any
}

// FinishReason discriminates why a [CompleteResponse] turn ended. The
// set is small and intentionally additive — future reasons extend the
// type without breaking switch-statements that include a default
// branch (callers MUST include one and treat unknown reasons as
// informational).
type FinishReason string

const (
	// FinishReasonStop indicates the model emitted its end-of-turn
	// signal naturally.
	FinishReasonStop FinishReason = "stop"

	// FinishReasonMaxTokens indicates the model hit the configured
	// [CompleteRequest.MaxTokens] limit before the end-of-turn signal.
	// The caller MAY continue the turn by appending the partial
	// assistant message and re-issuing the request.
	FinishReasonMaxTokens FinishReason = "max_tokens"

	// FinishReasonToolUse indicates the model paused to request one or
	// more tool calls. The pending calls ride on
	// [CompleteResponse.ToolCalls]; the runtime executes them and the
	// caller continues the turn.
	FinishReasonToolUse FinishReason = "tool_use"

	// FinishReasonError indicates a provider-level error terminated
	// the turn before completion. The error string rides on
	// [CompleteResponse.ErrorMessage]; the method-call return value is
	// also non-nil.
	FinishReasonError FinishReason = "error"
)

// Usage is the post-turn token / cost accounting the provider returns
// alongside a [CompleteResponse] (synchronous) or on the
// [StreamEventKindMessageStop] event (streaming). The shape is portable
// across providers — every LLM API exposes prompt / completion token
// counts; cost-in-cents is optional because not every provider knows
// its own price catalogue.
type Usage struct {
	// Model is the [Model] the turn ran against. The provider
	// populates this verbatim from the request's model field — useful
	// when the cost tracker (M6.3) aggregates by model.
	Model Model

	// InputTokens is the token count the provider charged for the
	// prompt (system + messages + tool definitions). Non-negative.
	InputTokens int

	// OutputTokens is the token count the provider charged for the
	// model's response. Non-negative.
	OutputTokens int

	// CostCents is the provider-computed cost of the turn in
	// hundredths of a USD cent (i.e. 1/10000 of a dollar) so callers
	// can carry it as an integer without floating-point drift. Zero
	// when the provider does not know its own catalogue; the cost
	// tracker (M6.3) MAY recompute from token counts.
	CostCents int64

	// Metadata carries provider-specific extensions (cache-hit token
	// counts, request id for upstream support, …). Nil is fine.
	Metadata map[string]string
}

// CompleteRequest is the value supplied to [Provider.Complete]. The
// shape mirrors Anthropic's request envelope (system as a top-level
// field) since Claude Code is the Phase-1 default; providers with a
// messages-only shape compose the [Message] sequence themselves.
type CompleteRequest struct {
	// Model is the [Model] the provider routes the request to.
	// Required; an empty Model returns [ErrModelNotSupported].
	Model Model

	// System is the fully-composed system prompt the provider installs
	// at turn boundary. Optional but typically non-empty in practice;
	// empty means "no system prompt", not "use default". Mirrors
	// [runtime.Manifest.SystemPrompt].
	System string

	// Messages is the ordered conversational sequence. Required; an
	// empty / nil slice returns [ErrInvalidPrompt] without contacting
	// the provider.
	Messages []Message

	// MaxTokens caps the model's response length in output tokens.
	// Zero means "provider default" — concrete providers SHOULD
	// document the default they apply.
	MaxTokens int

	// Temperature controls sampling determinism in the [0.0, 1.0]
	// range. Zero means "provider default"; explicit zero (greedy
	// sampling) requires a sentinel — providers MAY accept a negative
	// value or rely on Metadata until a portable concept emerges.
	Temperature float64

	// Tools is the optional toolset the model MAY request. The runtime
	// authorises model-emitted [ToolCall]s against
	// [runtime.Manifest.Toolset] before execution; this slice is the
	// provider-side declaration the model sees. Nil / empty means "no
	// tools available".
	Tools []ToolDefinition

	// Metadata carries provider-specific extensions (Anthropic
	// `top_k`, OpenAI `logprobs`, Google `safetySettings`, …).
	// Providers consume only the keys they recognise. Nil is fine.
	Metadata map[string]string
}

// CompleteResponse is the value returned by [Provider.Complete] on a
// successful turn. Errors that ABORT the turn surface as the method's
// returned `error`; errors that END the turn early but produced a
// partial response ride on [CompleteResponse.ErrorMessage] with
// [CompleteResponse.FinishReason] = [FinishReasonError].
type CompleteResponse struct {
	// Content is the model's textual response. Empty when
	// [CompleteResponse.FinishReason] = [FinishReasonToolUse] and the
	// model emitted only tool calls.
	Content string

	// ToolCalls is the ordered list of tool calls the model requested.
	// Empty when the model produced text only. The runtime executes
	// each call via [runtime.AgentRuntime.InvokeTool] and appends the
	// results as [Message]s with [RoleTool] for the next turn.
	ToolCalls []ToolCall

	// FinishReason discriminates why the turn ended per
	// [FinishReason]. Required; an empty FinishReason from a provider
	// is a programmer error.
	FinishReason FinishReason

	// ErrorMessage carries the provider-reported error text when
	// [CompleteResponse.FinishReason] = [FinishReasonError]. Empty for
	// non-error terminations.
	ErrorMessage string

	// Usage is the post-turn token / cost accounting the provider
	// computed. Always populated; zero token counts indicate the
	// provider could not measure (rare).
	Usage Usage

	// Metadata carries provider-specific extensions (response id,
	// cache-control hints, …). Providers populate; callers consume
	// only the keys they recognise. Nil is fine.
	Metadata map[string]string
}

// StreamRequest is the value supplied to [Provider.Stream]. Shape
// mirrors [CompleteRequest] minus the synchronous-only knobs; future
// stream-only options land here as new fields.
type StreamRequest struct {
	// Model — see [CompleteRequest.Model].
	Model Model

	// System — see [CompleteRequest.System].
	System string

	// Messages — see [CompleteRequest.Messages].
	Messages []Message

	// MaxTokens — see [CompleteRequest.MaxTokens].
	MaxTokens int

	// Temperature — see [CompleteRequest.Temperature].
	Temperature float64

	// Tools — see [CompleteRequest.Tools].
	Tools []ToolDefinition

	// Metadata — see [CompleteRequest.Metadata].
	Metadata map[string]string
}

// StreamEventKind discriminates [StreamEvent] payloads. The set is
// small and intentionally additive — future kinds extend the type
// without breaking switch-statements that include a default branch
// (callers MUST include one and treat unknown kinds as informational).
type StreamEventKind string

const (
	// StreamEventKindTextDelta carries an incremental text chunk the
	// model just generated. The chunk text rides on
	// [StreamEvent.TextDelta]. Callers concatenate deltas in
	// arrival order to assemble the final assistant message.
	StreamEventKindTextDelta StreamEventKind = "text_delta"

	// StreamEventKindToolCallStart carries the start of a tool call
	// the model is about to request. The tool-call shell (id + name,
	// arguments may be empty) rides on [StreamEvent.ToolCall].
	// Subsequent [StreamEventKindToolCallDelta] events carry argument
	// fragments for the same tool call.
	StreamEventKindToolCallStart StreamEventKind = "tool_call_start"

	// StreamEventKindToolCallDelta carries an incremental tool-call
	// argument chunk for a previously-started tool call. The chunk's
	// JSON-fragment text rides on [StreamEvent.TextDelta] (reusing the
	// same field; the [StreamEvent.ToolCall] field carries only the
	// tool-call id for correlation). Callers concatenate fragments in
	// arrival order, parse JSON when the matching tool_call ends.
	StreamEventKindToolCallDelta StreamEventKind = "tool_call_delta"

	// StreamEventKindMessageStop carries the end-of-turn signal. The
	// final [Usage] rides on [StreamEvent.Usage]; the
	// [FinishReason] rides on [StreamEvent.FinishReason]. After this
	// event the provider closes the underlying stream and any
	// subsequent [StreamSubscription.Stop] returns nil.
	StreamEventKindMessageStop StreamEventKind = "message_stop"

	// StreamEventKindError carries a provider-level error that did
	// NOT terminate the underlying connection (e.g. a transient hiccup
	// the provider will retry). Terminal errors surface via
	// [StreamSubscription.Stop]'s wrapped cause, NOT through this
	// event. The error string rides on [StreamEvent.ErrorMessage].
	StreamEventKindError StreamEventKind = "error"
)

// StreamEvent is the value delivered to a [StreamHandler] from
// [Provider.Stream]. The discriminated [StreamEvent.Kind] tells the
// handler which optional payload field carries the chunk. Unknown kinds
// are forwards-compatible: handlers MUST treat them as informational
// and not panic on absent payload pointers.
type StreamEvent struct {
	// Kind discriminates the payload per [StreamEventKind]. Required;
	// an empty Kind is a programmer error in the provider implementation
	// and SHOULD be dropped by the handler.
	Kind StreamEventKind

	// TextDelta carries the incremental text chunk for
	// [StreamEventKindTextDelta] / [StreamEventKindToolCallDelta]
	// events. Empty for other kinds.
	TextDelta string

	// ToolCall carries the tool-call shell on
	// [StreamEventKindToolCallStart] (full id + name, arguments may be
	// empty) or just the id on [StreamEventKindToolCallDelta]. Nil for
	// non-tool kinds.
	ToolCall *ToolCall

	// FinishReason carries the turn-end discriminator on
	// [StreamEventKindMessageStop]. Empty for other kinds.
	FinishReason FinishReason

	// Usage carries the final token / cost accounting on
	// [StreamEventKindMessageStop]. Zero-valued [Usage] for other kinds.
	Usage Usage

	// ErrorMessage carries the error text for [StreamEventKindError].
	// Empty for non-error kinds. The interface intentionally does NOT
	// surface a Go `error` here — the handler does not need to react
	// programmatically; it just observes.
	ErrorMessage string

	// Metadata carries provider-specific extensions (chunk index,
	// upstream message id, …). Handlers consume only the keys they
	// recognise. Nil is fine.
	Metadata map[string]string
}

// StreamHandler is the callback supplied to [Provider.Stream]. The
// handler runs in a goroutine the provider owns; returning a non-nil
// error terminates the stream — the provider stops dispatching further
// events and the wrapped cause surfaces from the next
// [StreamSubscription.Stop] call via the [errors.Is] chain.
type StreamHandler func(ctx context.Context, ev StreamEvent) error

// StreamSubscription is the lifecycle handle returned by
// [Provider.Stream]. Calling [StreamSubscription.Stop] terminates the
// stream and returns once the in-flight [StreamHandler] (if any) has
// completed. Stop is idempotent. Mirrors [runtime.Subscription] /
// [messenger.Subscription] so callers wiring all three surfaces share a
// mental model.
type StreamSubscription interface {
	// Stop signals the underlying stream to close and blocks until the
	// dispatch loop exits. Idempotent — a second Stop returns the same
	// (typically nil) result without re-running the shutdown.
	Stop() error
}

// CountTokensRequest is the value supplied to [Provider.CountTokens].
// The provider uses the same tokeniser it would use for a prospective
// [Provider.Complete] / [Provider.Stream] call so callers can
// budget-check before paying for inference.
type CountTokensRequest struct {
	// Model — see [CompleteRequest.Model]. Required.
	Model Model

	// System — see [CompleteRequest.System].
	System string

	// Messages — see [CompleteRequest.Messages]. Required.
	Messages []Message

	// Tools — see [CompleteRequest.Tools]. Tool definitions count
	// against the prompt budget; empty / nil is fine.
	Tools []ToolDefinition

	// Metadata — see [CompleteRequest.Metadata].
	Metadata map[string]string
}

// Provider is the portable interface every concrete LLM provider
// implementation satisfies. The four methods cover the lifecycle of a
// single LLM turn: complete it synchronously ([Provider.Complete]),
// stream it as it generates ([Provider.Stream]), count tokens before
// sending ([Provider.CountTokens]), and report usage afterwards
// ([Provider.ReportCost]).
//
// Implementations are expected to be safe for concurrent use after
// construction; the interface itself does not impose synchronization
// requirements but every Phase 1 implementation does.
//
// The interface intentionally has NO knowledge of Anthropic content
// blocks, OpenAI function-calling envelopes, or the Claude Agent SDK —
// those concepts are concrete-provider concerns. A future Anthropic
// provider, an OpenAI provider, an in-process fake, and the Phase-1
// Claude Code provider (M5.2.b) all implement the same surface.
// M5.10's provider-swap conformance test exercises this contract.
type Provider interface {
	// Complete drives a single synchronous turn. Returns the model's
	// response with [CompleteResponse.Usage] populated. Returns
	// [ErrModelNotSupported] when [CompleteRequest.Model] is empty or
	// the provider's catalogue does not list it. Returns
	// [ErrInvalidPrompt] when [CompleteRequest.Messages] is empty or a
	// [ToolDefinition.InputSchema] is nil. Returns
	// [ErrTokenLimitExceeded] when the prompt exceeds the model's
	// context window. Returns [ErrProviderUnavailable] when the
	// upstream service is unreachable. Tool-call requests from the
	// model ride on [CompleteResponse.ToolCalls]; the runtime executes
	// them.
	Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)

	// Stream drives a single streaming turn, dispatching [StreamEvent]
	// values to `handler` as the model generates. Returns the
	// [StreamSubscription] the caller uses to cancel the stream early.
	// Validation errors mirror [Provider.Complete]; additionally
	// returns [ErrInvalidHandler] when `handler` is nil. The handler
	// runs in a goroutine the provider owns; a non-nil return from
	// the handler terminates the stream and the wrapped cause surfaces
	// from [StreamSubscription.Stop]. The final [Usage] rides on the
	// [StreamEventKindMessageStop] event.
	Stream(ctx context.Context, req StreamRequest, handler StreamHandler) (StreamSubscription, error)

	// CountTokens returns the deterministic token count the provider
	// would charge for `req` if it were submitted to [Provider.Complete]
	// / [Provider.Stream]. Does NOT contact the model — the count comes
	// from the provider's local tokeniser. Returns [ErrModelNotSupported]
	// when [CountTokensRequest.Model] is empty or the catalogue does
	// not list it. Returns [ErrInvalidPrompt] when
	// [CountTokensRequest.Messages] is empty.
	CountTokens(ctx context.Context, req CountTokensRequest) (int, error)

	// ReportCost records `usage` against the [runtime.AgentRuntime]
	// session identified by `runtimeID` for downstream cost tracking
	// (M6.3). The provider's bookkeeping accumulates the values; the
	// caller does NOT need to read them back from this surface — the
	// cost tracker subscribes to the provider via a separate channel
	// (out of scope here). Returns nil even when the runtimeID is
	// previously unseen — ReportCost is the create+update boundary,
	// not a query. The caller MUST call ReportCost exactly once per
	// completed turn (whether [Provider.Complete] or
	// [Provider.Stream]); duplicate calls produce duplicate accounting.
	ReportCost(ctx context.Context, runtimeID string, usage Usage) error
}
