package runtime

import (
	"context"
	"encoding/json"
)

// ID is the opaque handle the [AgentRuntime] assigns when
// [AgentRuntime.Start] succeeds. The bytes are runtime-defined; callers
// treat the value as an opaque string and pass it back to subsequent
// methods ([AgentRuntime.SendMessage], [AgentRuntime.InvokeTool],
// [AgentRuntime.Subscribe], [AgentRuntime.Terminate]) verbatim.
//
// Different runtime implementations encode different bytes here — the
// Claude Code TS-harness runtime (M5.3) emits a "<pid>:<boot-nonce>"
// pair, a future in-process runtime might emit a UUID, an SDK-embedded
// runtime might emit the SDK's session id. The interface package never
// inspects the bytes.
//
// Callers typically alias-import this package (e.g.
// `agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"`)
// because the bare name collides with the standard library; the
// resulting `agentruntime.ID` reads cleanly at the call site.
type ID string

// AutonomyLevel describes the supervision regime under which the runtime
// drives the agent. Values are vendor-neutral string aliases so the
// runtime, the manifest loader, and downstream policies can compare
// them without an enum-import cycle. The set is intentionally small for
// Phase 1; future levels are additive and MUST preserve existing
// values' meaning.
type AutonomyLevel string

const (
	// AutonomyManual blocks every tool invocation on a human ack via
	// the lead-approval flow before the runtime executes it. The
	// runtime still drives the LLM but pauses on tool calls.
	AutonomyManual AutonomyLevel = "manual"

	// AutonomySupervised lets the runtime execute tool calls but
	// requires the leader to approve manifest / personality / language
	// changes. The default for fresh Watchkeepers.
	AutonomySupervised AutonomyLevel = "supervised"

	// AutonomyAutonomous lets the runtime execute tool calls and
	// adjust its own [Manifest.Personality] / [Manifest.Language]
	// without per-change approval (subject to the authority matrix).
	AutonomyAutonomous AutonomyLevel = "autonomous"
)

// Manifest is the portable subset of an agent's runtime configuration
// the [AgentRuntime] needs to boot a session. ROADMAP §M5 → M5.5
// promotes a wire-format `keepclient.ManifestVersion` into a Manifest
// here; the M5.5 loader is responsible for the mapping (template-
// composing [Manifest.Personality] and [Manifest.Language] into
// [Manifest.SystemPrompt], decoding the `tools` jsonb into
// [Manifest.Toolset], and projecting the `authority_matrix` into
// [Manifest.AuthorityMatrix]).
//
// The fields here are the minimum the runtime needs at Start time.
// Cross-cutting fields (rate-limiter knobs, secrets handles) flow via
// [StartOptions.Metadata] until a portable concept emerges.
type Manifest struct {
	// AgentID is the stable identifier of the Watchkeeper this runtime
	// session is for. Treat as opaque on this surface; the runtime
	// MAY embed it in subprocess argv / env for diagnostics. Required;
	// an empty AgentID returns [ErrInvalidManifest] from
	// [AgentRuntime.Start].
	AgentID string

	// SystemPrompt is the fully-composed system prompt the runtime
	// installs at session bootstrap. The M5.5 loader is responsible
	// for templating Personality and Language into this string; the
	// runtime does NOT re-template. Required; an empty SystemPrompt
	// returns [ErrInvalidManifest].
	SystemPrompt string

	// Personality is the persona blob the manifest carries verbatim.
	// The runtime does not consume it directly (the loader already
	// folded it into SystemPrompt); it is preserved here so meta-tools
	// (M6 Watchmaster's `adjust_personality`) can introspect.
	Personality string

	// Language is the language code (BCP-47) the manifest carries
	// verbatim. The runtime forwards it to the LLM provider when the
	// provider exposes a language hint; otherwise it is informational.
	Language string

	// Model is the [LLMProvider] model identifier (e.g.
	// `claude-sonnet-4`). The runtime passes it to the provider as-is;
	// validation against the provider's catalogue is the provider's
	// job (M5.2). An empty Model returns [ErrInvalidManifest].
	Model string

	// Autonomy is the supervision regime per [AutonomyLevel]. The
	// runtime consults it when a tool invocation needs human approval.
	// An empty Autonomy defaults to [AutonomySupervised].
	Autonomy AutonomyLevel

	// Toolset is the set of tools the agent is permitted to call via
	// [AgentRuntime.InvokeTool], carrying both names and (optionally)
	// per-tool versions projected from the manifest_version.tools
	// jsonb. The runtime MUST reject calls for names outside this set
	// with [ErrToolUnauthorized] before touching the tool. An empty /
	// nil Toolset means "no tools".
	//
	// Consumers that historically required `[]string` (the M5.5.b.a
	// ACL gate, the LLM `tools` request projection, every test fixture
	// that iterates the field) call [Toolset.Names] to recover the
	// prior shape; version-aware callers (M5.6.e.b boot-time
	// superseded-lesson scan) iterate the slice directly to read each
	// [ToolEntry.Version]. The field type migrated from `[]string` to
	// [Toolset] in M5.6.e.a; no behavioural change beyond the new
	// Version surface.
	Toolset Toolset

	// AuthorityMatrix is the projection of the manifest's
	// authority_matrix the runtime consults at lifecycle / approval
	// gates. The shape is portable string→string so the runtime
	// package never depends on the manifest jsonb schema; the M5.5
	// loader is the projection's owner.
	AuthorityMatrix map[string]string

	// NotebookTopK is the optional notebook recall top-K count. Zero
	// means "unset" — the runtime uses its own default. Populated by
	// the M5.5.c.b loader from keepclient.ManifestVersion.NotebookTopK;
	// consumed by the M5.5.c.d recall layer.
	NotebookTopK int

	// NotebookRelevanceThreshold is the optional notebook recall relevance
	// threshold (0–1). Zero means "unset". Populated by the M5.5.c.b
	// loader from keepclient.ManifestVersion.NotebookRelevanceThreshold;
	// consumed by the M5.5.c.d recall layer.
	NotebookRelevanceThreshold float64

	// ImmutableCore is the optional projection of the manifest's
	// immutable_core jsonb column — the five mechanical-immutability
	// buckets that govern what the agent, self-tuning, or lead may NOT
	// override regardless of any other field on this manifest. Populated
	// by the M3.1 manifest loader from
	// [github.com/vadimtrunov/watchkeepers/core/pkg/keepclient.ManifestVersion.ImmutableCore];
	// consumed by the M3.2 admin-only enforcement gate and the M3.6
	// self-tuning validator.
	//
	// A nil pointer means "no immutable core declared yet" (legacy row
	// predating M3.1). The runtime SHOULD treat that as fail-secure —
	// no governance overrides — and ship a warning rather than silently
	// allow blanket access. A non-nil pointer projects the M3.1 buckets
	// verbatim; forward-compatible bucket extensions (Phase 2 §M3.4
	// `merge_fields` / `rollback`) ride in [ImmutableCore.Extra] until
	// a typed field lands.
	ImmutableCore *ImmutableCore

	// Reason is the optional free-text rationale the proposer attached
	// to this manifest_version row (Phase 2 §M3.3). The runtime does
	// not consume it directly — it is preserved here so meta-tools
	// (M3.4 `manifest.history`, `manifest.diff`) and the M3.5 Slack UX
	// can surface "why this version" without re-querying the Keep.
	// Empty means "no reason recorded" (legacy row predating M3.3 OR a
	// row that simply omitted the field on PUT).
	Reason string

	// PreviousVersionID is the optional UUID of the manifest_version
	// row this version is derived from (Phase 2 §M3.3). Empty for the
	// root version of every manifest (no previous). The M3.4
	// `manifest.history` / `manifest.diff` tools walk the chain via
	// this field; the runtime itself does not consume it.
	PreviousVersionID string

	// Proposer is the optional free-text identifier of the actor that
	// proposed this version (Phase 2 §M3.3) — typically a Watchkeeper
	// UUID, a human handle, or the literal "watchmaster" for
	// system-initiated rollback proposals. Empty means "no proposer
	// recorded" (legacy row OR a row that omitted the field on PUT).
	// The runtime preserves it for meta-tools / audit; it does not
	// drive any policy decision at this milestone.
	Proposer string

	// Metadata carries runtime-specific extensions (TS-harness module
	// path, Claude Code subprocess flags, isolate options, …). The
	// runtime consumes only the keys it recognises and ignores the
	// rest. Nil is fine.
	Metadata map[string]string
}

// ImmutableCore captures the five mechanical-immutability buckets the
// Phase 2 §M3.1 schema attaches to every Manifest. The shape is portable
// string-keyed maps / scalars so the runtime package never depends on
// the manifest jsonb encoding; the M3.1 manifest loader is the
// projection's owner.
//
// M3.1 is schema-only — every bucket field is intentionally permissive
// at this milestone: admin-only editability gating lives in M3.2
// (handler-layer) and the self-tuning validator lives in M3.6. The
// shape here only fixes the canonical key names so downstream consumers
// can target stable fields without re-parsing the raw jsonb at every
// read site.
//
// Forward compatibility: unknown wire keys decode into [Extra] verbatim
// so a Phase 2 §M3.4 `merge_fields` payload that adds a sixth bucket is
// not silently dropped by the M3.1 loader. Consumers that do not
// recognise an Extra key MUST tolerate it (mirrors the [Manifest.Metadata]
// precedent).
type ImmutableCore struct {
	// RoleBoundaries is the explicit list of capability names the
	// Watchkeeper is NOT allowed to have, regardless of what
	// self-tuning proposes. Nil / empty means "no role boundaries
	// declared" — the runtime treats that as fail-secure (no
	// overrides allowed).
	RoleBoundaries []string `json:"role_boundaries,omitempty"`

	// SecurityConstraints is a free-form map of data-handling rules,
	// forbidden data destinations, and classification floors. Keys are
	// constraint names; values are the rule payload. The M3.6
	// validator is the authoritative consumer; the M3.1 schema layer
	// treats the map as opaque.
	SecurityConstraints map[string]any `json:"security_constraints,omitempty"`

	// EscalationProtocols declares when and to whom to escalate;
	// cannot be disabled. Keys are protocol names (e.g. `pii_leak`,
	// `cost_breach`); values are the route payload (target channel,
	// SLA window, …). The runtime consults this map at every
	// approval-gated tool invocation; M3.1 stores the projection only.
	EscalationProtocols map[string]any `json:"escalation_protocols,omitempty"`

	// CostLimits declares the max token spend caps (per task, per
	// day, per week). Keys are the cap window names; values are the
	// cap payload (typically a numeric token / dollar threshold). The
	// runtime cost ledger enforces these in M2; M3.1 ships the
	// projection so the ledger can resolve them by name.
	CostLimits map[string]any `json:"cost_limits,omitempty"`

	// AuditRequirements declares what MUST be logged; cannot be
	// reduced. Keys are audit categories (e.g. `manifest_changes`,
	// `tool_invocations`); values are the requirement payload
	// (retention window, redaction policy, …). The Keeper's Log
	// writer consults this map; M3.1 stores the projection only.
	AuditRequirements map[string]any `json:"audit_requirements,omitempty"`

	// Extra carries any additional buckets the wire payload declared
	// beyond the five canonical M3.1 buckets above. Forward-compatible
	// projection — a Phase 2 §M3.4 `merge_fields` payload that adds a
	// sixth bucket flows through Extra rather than getting silently
	// dropped. Consumers that do not recognise an Extra key MUST
	// tolerate it (mirrors the [Manifest.Metadata] precedent). The
	// field is excluded from JSON round-trips (`json:"-"`) because the
	// M3.1 loader populates it manually from the raw jsonb after
	// decoding the canonical buckets — re-marshalling the runtime
	// ImmutableCore back onto the wire is NOT a supported round-trip
	// at this milestone (the wire shape is owned by keepclient's
	// [json.RawMessage] surface).
	Extra map[string]json.RawMessage `json:"-"`
}

// StartOptions is the value supplied to [AgentRuntime.Start] alongside
// a [Manifest]. Currently empty-by-design; future fields (working
// directory, environment overrides, secrets handle) land here as the
// concrete runtimes (M5.3+) discover what they need. Defining the type
// up front keeps the call site stable: [AgentRuntime.Start] takes
// `(ctx, manifest, ...StartOption)` and grows via functional options
// rather than positional-arg refactors.
type StartOptions struct {
	// Metadata carries runtime-specific Start-time extensions. Same
	// opacity contract as [Manifest.Metadata]; the runtime consumes
	// only the keys it recognises.
	Metadata map[string]string
}

// StartOption mutates the [StartOptions] passed to [AgentRuntime.Start].
// Functional-options pattern; mirrors the [capability.Option] /
// [outbox.Option] surfaces. Implementations apply the options in order;
// later options override earlier ones for the same field.
type StartOption func(*StartOptions)

// WithStartMetadata seeds [StartOptions.Metadata] with the supplied
// map. A nil map is a no-op so callers can always pass through whatever
// they have. Subsequent calls merge: keys present in both maps take
// the later option's value.
func WithStartMetadata(meta map[string]string) StartOption {
	return func(o *StartOptions) {
		if len(meta) == 0 {
			return
		}
		if o.Metadata == nil {
			o.Metadata = make(map[string]string, len(meta))
		}
		for k, v := range meta {
			o.Metadata[k] = v
		}
	}
}

// Runtime is the lifecycle handle returned by [AgentRuntime.Start]. The
// concrete type the runtime returns is opaque; callers interact with
// it via [Runtime.ID] and pass that id to subsequent
// [AgentRuntime.SendMessage] / [AgentRuntime.InvokeTool] /
// [AgentRuntime.Subscribe] / [AgentRuntime.Terminate] calls. The handle
// itself is intentionally a single-method interface so future runtime
// implementations can attach cancellation / cleanup helpers without
// breaking callers.
type Runtime interface {
	// ID returns the [ID] the runtime assigned at Start time.
	// Stable for the lifetime of the handle; safe for concurrent reads.
	ID() ID
}

// Message is the value supplied to [AgentRuntime.SendMessage]. The
// shape is intentionally minimal — a body plus a metadata bag for
// runtime-specific extensions. Future fields are additive only;
// callers needing runtime-specific knobs reach for [Message.Metadata]
// until a portable concept exists.
type Message struct {
	// Text is the message body the runtime forwards to the agent. The
	// runtime treats it as an opaque user-turn input — no parsing, no
	// templating. Required; empty Text returns [ErrInvalidMessage]
	// synchronously.
	Text string

	// Metadata carries runtime-specific extensions (channel id,
	// thread anchor, sender platform id, …). The runtime consumes
	// only the keys it recognises. Nil is fine.
	Metadata map[string]string
}

// ToolCall is the value supplied to [AgentRuntime.InvokeTool]. Captures
// the tool name plus the JSON-shaped arguments the runtime forwards to
// the underlying tool implementation. The shape is intentionally
// portable — neither the call vocabulary nor the args schema is
// runtime-specific.
type ToolCall struct {
	// Name is the tool's manifest-declared name (e.g.
	// `notebook.remember`). Required; empty Name returns
	// [ErrInvalidToolCall]. Names not in [Manifest.Toolset] return
	// [ErrToolUnauthorized].
	Name string

	// Arguments is the JSON-shaped payload the runtime hands to the
	// tool. Empty / nil is fine — the tool decides whether its
	// signature requires arguments.
	Arguments map[string]any

	// ToolVersion is the optional manifest-projected version string
	// (e.g. "v1.2.3") of the tool the runtime is invoking. Populated
	// by version-aware callers (the M5.5 manifest loader). The
	// runtime forwards it verbatim to the auto-reflection layer
	// ([ToolErrorReflector.Reflect]) so a learned `lesson` row can be
	// scoped to the version that produced the failure — Recall queries
	// at boot-time supersession check (M5.6.e) compare against the
	// active manifest's version. Empty is fine; the reflector stores
	// SQL NULL when this field is empty so version-unaware callers
	// produce version-less rows that still surface in unfiltered
	// recalls.
	ToolVersion string

	// Metadata carries runtime-specific extensions (call id, parent
	// turn id, supervisor approval ack, …). The runtime consumes
	// only the keys it recognises. Nil is fine.
	Metadata map[string]string
}

// ToolResult is the value returned by [AgentRuntime.InvokeTool] on
// successful execution. Tool-side errors that do not abort the runtime
// (e.g. a tool reports `not found`) flow back here with [ToolResult.Error]
// populated; transport-level / authorization errors surface as the
// returned `error`.
type ToolResult struct {
	// Output is the tool's payload the runtime feeds back to the agent
	// turn loop. Opaque JSON shape; the runtime does not inspect it.
	Output map[string]any

	// Error is the tool-reported failure when the tool ran but its
	// own logic decided the call could not be satisfied (e.g.
	// `record not found`). The runtime forwards the error string to
	// the agent so it can react. Empty when the call succeeded.
	Error string

	// Metadata carries runtime-specific extensions (cost, latency,
	// cache hit indicator, …). The runtime consumes only the keys it
	// recognises. Nil is fine.
	Metadata map[string]string
}

// EventKind discriminates [Event] payloads. The set is small and
// intentionally additive — future kinds extend the type without
// breaking switch-statements that have a default branch (callers MUST
// include one and treat unknown kinds as informational).
type EventKind string

const (
	// EventKindMessage carries an agent-emitted text turn. The body
	// text rides on [Event.Message].
	EventKindMessage EventKind = "message"

	// EventKindToolCall carries a tool-invocation request the runtime
	// observed BEFORE executing it. Useful for audit / approval
	// pipelines. The call rides on [Event.ToolCall].
	EventKindToolCall EventKind = "tool_call"

	// EventKindToolResult carries the result of a tool invocation
	// after the runtime executed it. The result rides on
	// [Event.ToolResult].
	EventKindToolResult EventKind = "tool_result"

	// EventKindError carries a runtime-level error that did not
	// terminate the session (e.g. a transient provider hiccup the
	// runtime will retry). The error string rides on
	// [Event.ErrorMessage]. Terminal errors surface via the
	// method-call return value, NOT through Subscribe.
	EventKindError EventKind = "error"
)

// Event is the value delivered to an [EventHandler] from
// [AgentRuntime.Subscribe]. The discriminated [Event.Kind] tells
// the handler which of the optional payload pointers
// ([Event.Message], [Event.ToolCall],
// [Event.ToolResult], [Event.ErrorMessage]) carries the
// payload. Unknown kinds are forwards-compatible: handlers MUST treat
// them as informational and not panic on absent payload pointers.
type Event struct {
	// Kind discriminates the payload. Required; an empty Kind is a
	// programmer error in the runtime implementation and SHOULD be
	// dropped by the handler.
	Kind EventKind

	// RuntimeID is the [ID] of the session that produced the
	// event. Always populated; mirrors the [Manifest.AgentID] →
	// [ID] mapping the runtime stamped at Start.
	RuntimeID ID

	// Message is non-nil when Kind == [EventKindMessage]. Carries an
	// agent-emitted text turn.
	Message *Message

	// ToolCall is non-nil when Kind == [EventKindToolCall]. Carries
	// the call the runtime observed before executing.
	ToolCall *ToolCall

	// ToolResult is non-nil when Kind == [EventKindToolResult].
	// Carries the result the runtime emitted after executing.
	ToolResult *ToolResult

	// ErrorMessage carries the error text when Kind == [EventKindError].
	// Empty for non-error kinds. The interface intentionally does NOT
	// surface a Go `error` here — the handler does not need to react
	// programmatically; it just observes.
	ErrorMessage string

	// Metadata carries runtime-specific extensions (turn index,
	// upstream provider request id, …). Handlers consume only the
	// keys they recognise. Nil is fine.
	Metadata map[string]string
}

// EventHandler is the callback supplied to [AgentRuntime.Subscribe].
// The handler runs in a goroutine the runtime owns; returning a non-nil
// error is logged by the runtime but does NOT redeliver the event
// (Phase 1 is at-most-once at this layer; durable redelivery rides on
// the M3.7 outbox upstream).
type EventHandler func(ctx context.Context, ev Event) error

// Subscription is the lifecycle handle returned by
// [AgentRuntime.Subscribe]. Calling [Subscription.Stop] terminates the
// event stream and returns once the in-flight [EventHandler] (if any)
// has completed. Stop is idempotent. Mirrors the
// [messenger.Subscription] shape so callers wiring both surfaces share
// a mental model.
type Subscription interface {
	// Stop signals the underlying stream to close and blocks until
	// the dispatch loop exits. Idempotent — a second Stop returns the
	// same (typically nil) result without re-running the shutdown.
	Stop() error
}

// AgentRuntime is the portable interface every concrete agent runtime
// implementation satisfies. The five methods cover the lifecycle of an
// agent session: provision the session ([AgentRuntime.Start]), feed it
// human input ([AgentRuntime.SendMessage]), execute tool invocations on
// the agent's behalf ([AgentRuntime.InvokeTool]), observe streaming
// events the agent emits ([AgentRuntime.Subscribe]), and tear it down
// ([AgentRuntime.Terminate]).
//
// Implementations are expected to be safe for concurrent use after
// construction; the interface itself does not impose synchronization
// requirements but every Phase 1 implementation does.
//
// The interface intentionally has NO knowledge of Claude Code,
// isolate-vm, JSON-RPC, or the TS harness — those concepts are M5.2
// (LLMProvider) / M5.3 (TS harness) / M5.4 (resource limits) concerns.
// A future in-process runtime, an SDK-embedded runtime, or a fake
// runtime for tests all implement the same surface. M5.10's
// provider-swap conformance test exercises this contract.
type AgentRuntime interface {
	// Start provisions a fresh agent session driven by `manifest`.
	// Returns the [Runtime] handle whose [Runtime.ID] the caller passes
	// back to subsequent methods. Returns [ErrInvalidManifest] when
	// the manifest fails synchronous validation (empty AgentID, empty
	// SystemPrompt, empty Model).
	Start(ctx context.Context, manifest Manifest, opts ...StartOption) (Runtime, error)

	// SendMessage feeds `msg` to the runtime session identified by
	// `runtimeID` as a user-turn input. Returns [ErrRuntimeNotFound]
	// when `runtimeID` is unknown or has already been Terminated.
	// Returns [ErrInvalidMessage] when `msg.Text` is empty.
	SendMessage(ctx context.Context, runtimeID ID, msg Message) error

	// InvokeTool runs the tool identified by `call.Name` with
	// `call.Arguments` against the session identified by `runtimeID`.
	// Returns [ErrRuntimeNotFound] when `runtimeID` is unknown.
	// Returns [ErrToolUnauthorized] when the tool name is absent from
	// the session manifest's [Manifest.Toolset]. Returns
	// [ErrInvalidToolCall] when `call.Name` is empty. Tool-side
	// failures (the tool ran but reported an error) ride on
	// [ToolResult.Error]; this method's `error` return is reserved for
	// transport / authorization failures.
	InvokeTool(ctx context.Context, runtimeID ID, call ToolCall) (ToolResult, error)

	// Subscribe opens a streaming-event channel for the session
	// identified by `runtimeID` and dispatches every emitted
	// [Event] to `handler`. The returned [Subscription]
	// terminates the stream when [Subscription.Stop] is called.
	// Returns [ErrRuntimeNotFound] when `runtimeID` is unknown.
	// Returns [ErrInvalidHandler] when `handler` is nil.
	Subscribe(ctx context.Context, runtimeID ID, handler EventHandler) (Subscription, error)

	// Terminate ends the session identified by `runtimeID` and
	// releases its resources. Subsequent calls (SendMessage,
	// InvokeTool, Subscribe) on the same id return [ErrTerminated].
	// Idempotent: a second Terminate returns nil.
	Terminate(ctx context.Context, runtimeID ID) error
}
