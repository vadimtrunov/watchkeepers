// Doc-block at file head documenting the seam contract.
//
// resolution order: nil-dep check (panic) → Request.Validate
// (ErrInvalidDryRunRequest / ErrInvalidBrokerKind / ErrInvalidBrokerInvocation
// / ErrInvocationsExceedLimit) → DryRunMode.Validate
// (ErrInvalidDryRunMode) → ctx.Err pass-through → mode branch:
//
//   - DryRunModeNone → return ErrPreApprovalWarning IMMEDIATELY (no
//     resolver call, no clock read, no id mint, no publish). The
//     warning is the durable record at this layer; the caller (runtime
//     glue) surfaces it to the lead.
//   - DryRunModeGhost → build outcomes (Effective == Original;
//     Disposition = DispositionGhosted); no forwarder call regardless
//     of wiring.
//   - DryRunModeScoped → ScopeResolver (ErrScopeResolution /
//     ErrEmptyResolvedScope / ErrInvalidScope) → per-invocation
//     rewrite (Slack channel forced to LeadDMChannel; Jira project
//     forced to JiraSandboxProject) → optional Forwarder.Forward (any
//     error wraps as ErrBrokerForward; failure short-circuits the
//     remaining invocations so a partial trace surfaces to the caller).
//
// Ghost path: build outcomes → ctx.Err pre-publish (no real side
// effects yet) → Publisher.Publish (TopicDryRunExecuted, metadata-
// only payload, cancel-detached child ctx via
// context.WithoutCancel) → return Trace.
//
// Scoped path (iter-1 critic M2/M3 + codex C/M1 fixes): per-iteration
// ctx.Err refuses to keep firing real broker writes after caller-
// cancel; on cancel after at least one Forward success, the partial
// trace IS published (the event is the durable record of side-
// effects-fired). On Forward failure, the failing invocation is
// appended to the trace with [Outcome.ForwardErrMsg] populated so the
// audit subscriber sees WHICH invocation failed; if at least one
// prior Forward succeeded, the partial trace is published. Effective
// is re-cloned AFTER a successful Forward so a forwarder mutating the
// supplied map cannot corrupt the stored trace. Pre-publish ctx-gate
// is SKIPPED when at least one side effect has fired — the event MUST
// land regardless of caller-cancel.
//
// CorrelationID: the canonical UUIDv7 string of the proposal id.
// [Request.Validate] refuses a zero proposal id at the entry boundary
// so this derivation is always defined; no [IDGenerator] dependency
// is required (iter-1 codex E / critic m8 fix removed it).
//
// audit discipline: the executor never imports `keeperslog` and never
// calls `.Append(` (see source-grep AC). The audit log entry for each
// dry-run lives in the M9.7 audit subscriber observing
// TopicDryRunExecuted.
//
// PII discipline: BrokerInvocation.Args carry caller-supplied content
// (the would-be Slack message body, the Jira issue summary). The
// in-process Trace surfaces them to the caller — that is the
// "would have done" report. The eventbus payload
// (DryRunExecuted) carries METADATA ONLY: ProposalID, ToolName, Mode,
// BrokerKindCounts, ExecutedAt, CorrelationID — never the
// per-invocation Args. The reflection-based field allowlist on
// DryRunExecuted pins the contract.

package approval

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// BrokerKind is the closed-set discriminator for which downstream
// broker a [BrokerInvocation] targets. The set is closed by design:
// adding a new broker requires a new rewrite branch in
// [Executor.Execute] under [toolregistry.DryRunModeScoped] AND a new
// validation case here. A future broker (e.g. GitHub PR write) MUST
// extend BOTH together.
type BrokerKind string

const (
	// BrokerSlack identifies a Slack write-side broker operation. In
	// scoped mode the per-invocation rewrite forces the Slack channel
	// to the lead's DM (see [Scope.LeadDMChannel]).
	BrokerSlack BrokerKind = "slack"

	// BrokerJira identifies a Jira write-side broker operation. In
	// scoped mode the per-invocation rewrite forces the Jira project
	// to the deployment-configured sandbox project (see
	// [Scope.JiraSandboxProject]).
	BrokerJira BrokerKind = "jira"
)

// Validate reports whether `k` is in the closed [BrokerKind] set.
// Returns [ErrInvalidBrokerKind] otherwise (including the empty
// string — same fail-loud discipline as the other closed-set enums
// in this package).
func (k BrokerKind) Validate() error {
	switch k {
	case BrokerSlack, BrokerJira:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidBrokerKind, string(k))
	}
}

// Bounds enforced by [BrokerInvocation.Validate] and
// [Request.Validate]. The numbers are deliberate at the entry boundary
// so an adversarial agent cannot land an unbounded trace whose
// in-process retention OR (in a hypothetical future where Args land on
// the bus) eventbus payload would DOS downstream consumers. Same
// defensive-bounds discipline as [ProposalInput]'s per-field bounds.
const (
	// MaxBrokerOpLength bounds [BrokerInvocation.Op]. Conventional ops
	// (`send_message`, `create_issue`, `transition_issue`) are well
	// under 64 bytes.
	MaxBrokerOpLength = 64

	// MaxBrokerArgCount bounds the number of keys in
	// [BrokerInvocation.Args].
	MaxBrokerArgCount = 16

	// MaxBrokerArgKeyLength bounds an Args map key.
	MaxBrokerArgKeyLength = 64

	// MaxBrokerArgValueLength bounds an Args map value. 1 KiB fits any
	// realistic short-form broker arg (channel id, project key,
	// issue summary preview); the full message body of a Slack send is
	// itself bounded by the platform.
	MaxBrokerArgValueLength = 1024

	// MaxInvocationsPerRequest bounds [Request.Invocations]. A
	// dry-running tool emitting more than 32 broker calls in a single
	// invocation is almost certainly mis-scoped.
	MaxInvocationsPerRequest = 32
)

// BrokerInvocation captures a single intended write-side broker call
// the dry-running tool would make. Constructed by the runtime-glue
// caller (out of scope for M9.4.c — a future TS-runtime adapter
// produces these from the proposed code_draft's broker imports);
// consumed by [Executor.Execute].
//
// The shape is deliberately minimal: Op is an opaque string the
// downstream broker understands (e.g. `send_message`, `create_issue`,
// `transition_issue`), and Args is a bounded string map.
// Caller-supplied content (Slack message body, Jira summary) flows
// through Args; the executor preserves it in the in-process Trace but
// NEVER on the eventbus payload (see PII discipline at file head).
type BrokerInvocation struct {
	// Kind discriminates which broker this call targets. Required;
	// must pass [BrokerKind.Validate].
	Kind BrokerKind

	// Op is the broker-specific operation name. Required; non-empty;
	// bounded by [MaxBrokerOpLength].
	Op string

	// Args is the broker-specific argument map. May be nil (no args).
	// Bounded by [MaxBrokerArgCount] keys; each key is bounded by
	// [MaxBrokerArgKeyLength] bytes and each value by
	// [MaxBrokerArgValueLength] bytes.
	Args map[string]string
}

// Validate runs the shape contract on a [BrokerInvocation]. Returns
// the first applicable sentinel ([ErrInvalidBrokerKind] →
// [ErrInvalidBrokerInvocation] → [ErrInvalidBrokerArgs]) wrapped with
// field context.
func (b BrokerInvocation) Validate() error {
	if err := b.Kind.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(b.Op) == "" {
		return fmt.Errorf("%w: empty op", ErrInvalidBrokerInvocation)
	}
	if len(b.Op) > MaxBrokerOpLength {
		return fmt.Errorf("%w: op has %d bytes (max %d)", ErrInvalidBrokerInvocation, len(b.Op), MaxBrokerOpLength)
	}
	if len(b.Args) > MaxBrokerArgCount {
		return fmt.Errorf("%w: %d args (max %d)", ErrInvalidBrokerArgs, len(b.Args), MaxBrokerArgCount)
	}
	for k, v := range b.Args {
		if len(k) > MaxBrokerArgKeyLength {
			return fmt.Errorf("%w: arg key %q has %d bytes (max %d)", ErrInvalidBrokerArgs, k, len(k), MaxBrokerArgKeyLength)
		}
		if len(v) > MaxBrokerArgValueLength {
			return fmt.Errorf("%w: arg %q value has %d bytes (max %d)", ErrInvalidBrokerArgs, k, len(v), MaxBrokerArgValueLength)
		}
	}
	return nil
}

// Scope is the per-deployment dry-run sandbox configuration consumed
// by [toolregistry.DryRunModeScoped]. Resolved per-call via
// [ScopeResolver] so per-tenant scope rotation takes effect on the
// next invocation without a process restart.
//
// Both fields are required (non-empty); the resolver MUST return a
// fully-populated [Scope] or surface an error. Returning a zero-valued
// [Scope] alongside `nil` error is a programmer bug caught at
// [Executor.Execute] time via [ErrEmptyResolvedScope] (mirrors
// M9.4.a's [ErrEmptyResolvedIdentity] fail-loud discipline).
type Scope struct {
	// LeadDMChannel is the Slack DM channel id every [BrokerSlack]
	// invocation's `channel` arg is forced to under scoped mode. The
	// resolver typically resolves it from the proposal's recorded
	// requester id (from M9.4.b's [TopicDryRunRequested] event).
	LeadDMChannel string

	// JiraSandboxProject is the Jira project key every [BrokerJira]
	// invocation's `project` arg is forced to under scoped mode. The
	// resolver typically reads it from the deployment-level operator
	// config (a per-deployment static).
	JiraSandboxProject string
}

// Validate enforces both fields non-empty. Returns [ErrInvalidScope]
// with field context.
func (s Scope) Validate() error {
	if strings.TrimSpace(s.LeadDMChannel) == "" {
		return fmt.Errorf("%w: empty lead_dm_channel", ErrInvalidScope)
	}
	if strings.TrimSpace(s.JiraSandboxProject) == "" {
		return fmt.Errorf("%w: empty jira_sandbox_project", ErrInvalidScope)
	}
	return nil
}

// ScopeResolver is the per-call seam resolving the [Scope] for a given
// proposal. Production wiring resolves the lead DM channel from the
// recorded requester id AND reads the per-deployment Jira sandbox
// project key from operator config. Same function-shape per-call seam
// discipline as M9.4.a's [IdentityResolver] and M9.4.b's
// [SourceForTarget] / [WebhookSecretResolver] — a tenant-scoped value
// MUST NOT be pinned as a process-global static.
//
// Contract:
//
//   - Return a fully-populated [Scope] (both fields non-empty) on
//     success.
//   - Return `(Scope{}, <cause>)` on resolution failure. [Executor.Execute]
//     wraps the cause with [ErrScopeResolution] for the caller —
//     implementers MUST NOT pre-wrap (double-wrap chains break
//     `errors.Is` triage).
//   - Returning `(Scope{}, nil)` is a programmer error caught as
//     [ErrEmptyResolvedScope].
//
// Two-sentinel split: a fully-zero-valued [Scope] return (both fields
// empty AND no error) surfaces [ErrEmptyResolvedScope]. A partially-
// populated [Scope] (one field present, one empty / whitespace-only)
// surfaces [ErrInvalidScope] via [Scope.Validate]. Callers can
// `errors.Is` either sentinel to triage "resolver returned nothing"
// from "resolver returned a half-populated record".
type ScopeResolver func(ctx context.Context, proposalID uuid.UUID) (Scope, error)

// BrokerForwarder is the OPTIONAL seam that forwards rewritten
// invocations to real brokers under [toolregistry.DryRunModeScoped].
// A nil forwarder is the documented "M9.4.c primitive not yet wired
// to real brokers" degradation path — the Trace still surfaces the
// rewrites; only the real broker call is skipped.
//
// Implementers route by [BrokerInvocation.Kind] to the appropriate
// broker client. The rewrite has ALREADY been applied by the executor
// before [Forward] is called — implementers MUST use the supplied
// invocation verbatim (do NOT re-apply the lead-DM / sandbox-project
// rewrite). Returning an error short-circuits the remaining
// invocations on the [Request]; the partial trace surfaces to the
// caller wrapped with [ErrBrokerForward].
//
// Mutation contract: implementers MUST NOT mutate the supplied
// [BrokerInvocation] (in particular, the [BrokerInvocation.Args]
// map). The executor surfaces the SAME invocation on the returned
// [Trace] under [Outcome.Effective]; a forwarder mutating Args
// would silently corrupt the stored trace. The executor defends
// against this by re-cloning [Outcome.Effective] AFTER a successful
// [Forward] (iter-1 codex M1 defence), but a well-behaved
// implementer should not require that defence in the first place.
type BrokerForwarder interface {
	Forward(ctx context.Context, inv BrokerInvocation) error
}

// Disposition enumerates how a single [BrokerInvocation] was handled
// by the executor in the current mode. The Trace pins the per-
// invocation disposition so an audit subscriber can reconstruct
// exactly what each mode did per call.
type Disposition string

const (
	// DispositionGhosted means the invocation was stubbed — no real
	// broker call. Emitted under [toolregistry.DryRunModeGhost].
	DispositionGhosted Disposition = "ghosted"

	// DispositionScoped means the invocation was rewritten and (if a
	// [BrokerForwarder] is wired) forwarded to a real broker scoped to
	// the per-deployment sandbox surface. Emitted under
	// [toolregistry.DryRunModeScoped]. A scoped outcome carrying a
	// non-empty [Outcome.ForwardErrMsg] is the failing-invocation
	// marker on a partial trace returned with [ErrBrokerForward].
	DispositionScoped Disposition = "scoped"
)

// Validate reports whether `d` is in the closed [Disposition] set.
// Returns [ErrInvalidDisposition] otherwise (including the empty
// string). Symmetric with [BrokerKind.Validate] and [Route.Validate];
// callers MAY use this to validate values decoded from an audit row.
func (d Disposition) Validate() error {
	switch d {
	case DispositionGhosted, DispositionScoped:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidDisposition, string(d))
	}
}

// Outcome captures the per-invocation result the executor produces.
// `Original` is the caller-supplied invocation as-is; `Effective` is
// the invocation as it would land on the broker AFTER any per-mode
// rewrite. Under ghost mode, Original == Effective by construction.
type Outcome struct {
	// Original is the [BrokerInvocation] as supplied on the [Request].
	// Defensively copied so caller-side mutation of the Args map post-
	// Execute does not bleed into the Trace.
	Original BrokerInvocation

	// Effective is the [BrokerInvocation] as it would land on the
	// real broker after the per-mode rewrite. Defensively built (not
	// aliased to Original) so a downstream consumer mutating
	// Effective does not corrupt Original. Under
	// [toolregistry.DryRunModeScoped], the executor re-clones
	// Effective AFTER a successful [BrokerForwarder.Forward] call so
	// a forwarder mutating the supplied invocation cannot corrupt
	// the stored trace (iter-1 codex M1 defence-in-depth).
	Effective BrokerInvocation

	// Disposition records how this invocation was handled.
	Disposition Disposition

	// ForwardErrMsg is populated ONLY when this outcome represents a
	// failing scoped-mode invocation — the rewrite succeeded but the
	// optional [BrokerForwarder.Forward] returned an error. Empty
	// string on every other outcome. Carries the cause's
	// `err.Error()` (an opaque message string, NOT the typed error)
	// so the trace remains a value-type record that crosses goroutine
	// / process boundaries cleanly. Audit subscribers reading the
	// trace use this to triage which invocation in a partial trace
	// failed.
	ForwardErrMsg string
}

// Trace is the per-Request audit of every invocation's disposition.
// Returned from [Executor.Execute] for the caller; the in-process
// trace surfaces the FULL Original.Args and Effective.Args (the "would
// have done X, Y, Z" report). The eventbus boundary excludes Args by
// construction (see [DryRunExecuted]).
type Trace struct {
	// ProposalID is the proposal whose code_draft drove the
	// invocations.
	ProposalID uuid.UUID

	// ToolName is the proposed tool name (mirrors
	// [ProposalInput.Name]).
	ToolName string

	// Mode is the [toolregistry.DryRunMode] the executor branched on.
	Mode toolregistry.DryRunMode

	// Outcomes is the per-invocation result, in input order.
	Outcomes []Outcome

	// ExecutedAt is the wall-clock timestamp from the configured
	// [Clock] at the start of [Executor.Execute].
	ExecutedAt time.Time

	// CorrelationID is the canonical string form of [ProposalID] (the
	// caller-supplied proposal id is UUIDv7 by [Proposer.Submit]
	// construction so this string is time-ordered + unique).
	// [Request.Validate] refuses a zero proposal id at the entry
	// boundary so this derivation is always defined.
	CorrelationID string
}

// Request is the [Executor.Execute] input.
type Request struct {
	// ProposalID identifies the proposal whose code_draft drove the
	// invocations. Required (non-zero).
	ProposalID uuid.UUID

	// ToolName is the proposed tool name. Required; non-empty;
	// bounded by [MaxToolNameLength].
	ToolName string

	// Mode is the [toolregistry.DryRunMode] the executor branches on.
	// Required; must pass [toolregistry.DryRunMode.Validate].
	Mode toolregistry.DryRunMode

	// Invocations is the list of broker calls the tool would make.
	// Empty is acceptable (a tool that performs no broker writes
	// produces a Trace with zero outcomes). Bounded by
	// [MaxInvocationsPerRequest].
	Invocations []BrokerInvocation
}

// Validate enforces the shape contract on a [Request].
func (r Request) Validate() error {
	if r.ProposalID == uuid.Nil {
		return fmt.Errorf("%w: zero proposal id", ErrInvalidDryRunRequest)
	}
	if strings.TrimSpace(r.ToolName) == "" {
		return fmt.Errorf("%w: empty tool_name", ErrInvalidDryRunRequest)
	}
	if len(r.ToolName) > MaxToolNameLength {
		return fmt.Errorf("%w: tool_name has %d bytes (max %d)", ErrInvalidDryRunRequest, len(r.ToolName), MaxToolNameLength)
	}
	if err := r.Mode.Validate(); err != nil {
		return err
	}
	if len(r.Invocations) > MaxInvocationsPerRequest {
		return fmt.Errorf("%w: %d invocations (max %d)", ErrInvocationsExceedLimit, len(r.Invocations), MaxInvocationsPerRequest)
	}
	for i, inv := range r.Invocations {
		if err := inv.Validate(); err != nil {
			return fmt.Errorf("%w: invocations[%d]", err, i)
		}
	}
	return nil
}

// ExecutorDeps bundles the required + optional dependencies for
// [NewExecutor].
type ExecutorDeps struct {
	// Publisher emits [TopicDryRunExecuted] events. Required.
	Publisher Publisher

	// Clock stamps [Trace.ExecutedAt]. Required.
	Clock Clock

	// ScopeResolver resolves the [Scope] under
	// [toolregistry.DryRunModeScoped]. Required (a deployment that
	// supports `scoped` MUST configure this; deployments that only
	// ever see `ghost` / `none` may pass a resolver that always
	// errors — the resolver is consulted only on the scoped branch).
	ScopeResolver ScopeResolver

	// Forwarder forwards rewritten invocations to real brokers under
	// [toolregistry.DryRunModeScoped]. Optional; nil is the
	// documented "M9.4.c primitive not yet wired" degradation path —
	// the Trace still records DispositionScoped with the rewritten
	// Effective, only the real broker call is skipped.
	Forwarder BrokerForwarder

	// Logger receives diagnostic log entries. Optional; a nil
	// [Logger] silently discards entries.
	Logger Logger
}

// Executor is the M9.4.c dry-run runtime executor. Construct via
// [NewExecutor]; the zero value is not usable. Safe for concurrent
// use across goroutines once constructed.
type Executor struct {
	deps ExecutorDeps
}

// NewExecutor constructs an [*Executor]. Panics with a named-field
// message when any required dependency is nil; mirrors [New] /
// [NewWebhook] / [NewReviewer] / [NewCallbackDispatcher].
func NewExecutor(deps ExecutorDeps) *Executor {
	if deps.Publisher == nil {
		panic("approval: NewExecutor: deps.Publisher must not be nil")
	}
	if deps.Clock == nil {
		panic("approval: NewExecutor: deps.Clock must not be nil")
	}
	if deps.ScopeResolver == nil {
		panic("approval: NewExecutor: deps.ScopeResolver must not be nil")
	}
	return &Executor{deps: deps}
}

// Execute runs the dry-run executor on `req` and returns the
// resulting [Trace]. The resolution order is documented at the top of
// this file.
//
// Returns nil on a successful publish AND (under scoped mode) every
// invocation forwarding successfully. Returns the wrapped sentinel
// on any failure:
//
//   - [ErrInvalidDryRunRequest] / [ErrInvalidBrokerKind] /
//     [ErrInvalidBrokerInvocation] / [ErrInvalidBrokerArgs] /
//     [ErrInvocationsExceedLimit] from [Request.Validate].
//   - [ErrInvalidDryRunMode] from [toolregistry.DryRunMode.Validate].
//   - ctx.Err on a cancelled context (validation runs first so the
//     caller's validation-vs-cancel boundary stays stable).
//   - [ErrPreApprovalWarning] under [toolregistry.DryRunModeNone].
//   - [ErrScopeResolution] / [ErrEmptyResolvedScope] /
//     [ErrInvalidScope] under [toolregistry.DryRunModeScoped] when the
//     [ScopeResolver] fails.
//   - [ErrBrokerForward] when an attached [BrokerForwarder] rejects an
//     invocation.
//   - [ErrPublishDryRunExecuted] on a [Publisher.Publish] failure.
//
// The returned [Trace] is meaningful on most non-publish errors: a
// scoped-mode failure mid-stream returns the partial trace built so
// far so the caller can surface "this is how far we got". The
// [ErrPreApprovalWarning] branch returns a zero-valued Trace because
// no invocations were ever evaluated.
func (e *Executor) Execute(ctx context.Context, req Request) (Trace, error) {
	if err := req.Validate(); err != nil {
		return Trace{}, err
	}
	if err := ctx.Err(); err != nil {
		return Trace{}, err
	}

	switch req.Mode {
	case toolregistry.DryRunModeNone:
		return Trace{}, fmt.Errorf("%w: tool %q", ErrPreApprovalWarning, req.ToolName)
	case toolregistry.DryRunModeGhost:
		return e.executeGhost(ctx, req)
	case toolregistry.DryRunModeScoped:
		return e.executeScoped(ctx, req)
	}
	// req.Validate already ran DryRunMode.Validate so this branch is
	// unreachable on a validated request. Retained as defence-in-depth
	// against a future enum addition that forgets to extend the switch.
	return Trace{}, fmt.Errorf("%w: %q", toolregistry.ErrInvalidDryRunMode, string(req.Mode))
}

// executeGhost stubs every broker call. Original == Effective by
// construction; no Forwarder is consulted regardless of wiring (ghost
// is "no real write, ever"). Each Outcome.Disposition is
// [DispositionGhosted]. No real side effects fire under ghost, so the
// pre-publish ctx-gate is retained — a caller-side mid-flight cancel
// refuses a phantom event for a "would have done" report no consumer
// is waiting on.
func (e *Executor) executeGhost(ctx context.Context, req Request) (Trace, error) {
	outcomes := make([]Outcome, 0, len(req.Invocations))
	for _, inv := range req.Invocations {
		original := cloneBrokerInvocation(inv)
		// Effective is a separate clone so a downstream consumer
		// mutating Effective does not corrupt Original.
		effective := cloneBrokerInvocation(inv)
		outcomes = append(outcomes, Outcome{
			Original:    original,
			Effective:   effective,
			Disposition: DispositionGhosted,
		})
	}
	// No real side effects yet — a pre-publish ctx-cancel refuses the
	// phantom event for an abandoned caller.
	if err := ctx.Err(); err != nil {
		return Trace{}, err
	}
	return e.publish(ctx, req, outcomes)
}

// executeScoped resolves the [Scope] per-call, applies the per-broker
// rewrite to each invocation, and (when a [BrokerForwarder] is
// wired) forwards the rewritten invocation.
//
// Loop-iteration ctx-discipline (iter-1 critic M2 fix): a caller-side
// cancel during the forwarder loop SHORT-CIRCUITS the remaining
// invocations BEFORE firing real broker writes. If at least one
// forward already fired (`sideEffectsFired`), the partial trace IS
// published — real side effects are durable, and the audit
// subscriber needs the record (iter-1 critic M3 fix). If no side
// effects fired yet, the executor returns `(Trace{}, ctx.Err())`.
//
// Forwarder-error discipline (iter-1 codex C / critic M1 fix): on
// `Forward(...) != nil`, the failing invocation is appended to the
// trace with [Outcome.ForwardErrMsg] populated (so audit subscribers
// see WHICH invocation failed). If at least one prior forward
// succeeded, the partial trace IS published; otherwise it is not.
// The wrapped [ErrBrokerForward] surfaces to the caller either way.
//
// Forwarder-mutation defence (iter-1 codex M1 fix): after a
// successful `Forward`, the executor RE-CLONES `effective` before
// appending to outcomes. A forwarder that mutates the supplied
// invocation cannot corrupt the stored trace.
func (e *Executor) executeScoped(ctx context.Context, req Request) (Trace, error) {
	scope, err := e.deps.ScopeResolver(ctx, req.ProposalID)
	if err != nil {
		return Trace{}, fmt.Errorf("%w: %w", ErrScopeResolution, err)
	}
	if scope == (Scope{}) {
		return Trace{}, ErrEmptyResolvedScope
	}
	if err := scope.Validate(); err != nil {
		return Trace{}, err
	}
	outcomes := make([]Outcome, 0, len(req.Invocations))
	sideEffectsFired := false
	for i, inv := range req.Invocations {
		// Per-iteration ctx-check: refuse to keep firing real broker
		// writes after the caller has cancelled. If we have already
		// fired at least one side effect, publish the partial trace
		// so the audit subscriber sees what we did.
		if err := ctx.Err(); err != nil {
			if sideEffectsFired {
				return e.publishPartial(ctx, req, outcomes, err)
			}
			return Trace{}, err
		}
		original := cloneBrokerInvocation(inv)
		effective, rewriteErr := rewriteForScope(inv, scope)
		if rewriteErr != nil {
			if sideEffectsFired {
				return e.publishPartial(ctx, req, outcomes, rewriteErr)
			}
			return Trace{}, fmt.Errorf("%w: invocations[%d]", rewriteErr, i)
		}
		if e.deps.Forwarder != nil {
			// Pass a defensive clone to Forward so a buggy forwarder
			// mutating its argument cannot corrupt the `effective`
			// map we store on the trace below (codex iter-1 M1
			// defence-in-depth). The stored `effective` retains the
			// rewrite-as-applied semantic verbatim.
			forwardArg := cloneBrokerInvocation(effective)
			if err := e.deps.Forwarder.Forward(ctx, forwardArg); err != nil {
				e.logErr(ctx, "broker forward failed", "proposal_id", req.ProposalID, "invocation_index", i)
				// Append the failing outcome (codex iter-1 C fix)
				// with the original (un-mutated) effective and the
				// opaque cause message.
				outcomes = append(outcomes, Outcome{
					Original:      original,
					Effective:     effective,
					Disposition:   DispositionScoped,
					ForwardErrMsg: err.Error(),
				})
				wrapped := fmt.Errorf("%w: invocations[%d]: %w", ErrBrokerForward, i, err)
				if sideEffectsFired {
					return e.publishPartial(ctx, req, outcomes, wrapped)
				}
				return Trace{
					ProposalID:    req.ProposalID,
					ToolName:      req.ToolName,
					Mode:          req.Mode,
					Outcomes:      outcomes,
					ExecutedAt:    e.deps.Clock.Now(),
					CorrelationID: req.ProposalID.String(),
				}, wrapped
			}
			sideEffectsFired = true
		}
		outcomes = append(outcomes, Outcome{
			Original:    original,
			Effective:   effective,
			Disposition: DispositionScoped,
		})
	}
	// All forwards succeeded. Skip the pre-publish ctx-gate IF any
	// side effect fired — the event IS the durable record of what we
	// did, and a caller-cancel between the last Forward and publish
	// MUST NOT discard it. If no side effects fired (nil Forwarder OR
	// empty invocations), the pre-publish gate is OK to refuse.
	if !sideEffectsFired {
		if err := ctx.Err(); err != nil {
			return Trace{}, err
		}
	}
	return e.publish(ctx, req, outcomes)
}

// publishPartial publishes the partial trace from executeScoped's
// side-effects-fired path, then returns the supplied `cause` error
// to the caller. A publish failure is wrapped as
// [ErrPublishDryRunExecuted] and joined with the cause via
// [errors.Join] so the caller can `errors.Is` either kind.
//
// The publish uses [context.WithoutCancel] so a mid-flight caller
// cancel cannot race the eventbus send — the durable record of
// side-effects-fired MUST land regardless.
func (e *Executor) publishPartial(ctx context.Context, req Request, outcomes []Outcome, cause error) (Trace, error) {
	now := e.deps.Clock.Now()
	corrID := req.ProposalID.String()
	trace := Trace{
		ProposalID:    req.ProposalID,
		ToolName:      req.ToolName,
		Mode:          req.Mode,
		Outcomes:      outcomes,
		ExecutedAt:    now,
		CorrelationID: corrID,
	}
	publishCtx := context.WithoutCancel(ctx)
	event := newDryRunExecutedEvent(req, outcomes, now, corrID)
	if pubErr := e.deps.Publisher.Publish(publishCtx, TopicDryRunExecuted, event); pubErr != nil {
		e.logErr(ctx, "publish tool_dry_run_executed (partial) failed", "proposal_id", req.ProposalID)
		// Join the publish failure with the original cause so the
		// caller can `errors.Is(err, ErrPublishDryRunExecuted)` AND
		// `errors.Is(err, ErrBrokerForward)` independently.
		return trace, errors.Join(cause, fmt.Errorf("%w: %w", ErrPublishDryRunExecuted, pubErr))
	}
	return trace, cause
}

// rewriteForScope produces the [BrokerInvocation] as it would land on
// the real broker under scoped mode. The rewrite is per-broker:
//
//   - [BrokerSlack]: the `channel` arg is forced to
//     [Scope.LeadDMChannel]. Other args are preserved verbatim.
//   - [BrokerJira]: the `project` arg is forced to
//     [Scope.JiraSandboxProject]. Other args are preserved verbatim.
//
// The Args map on the returned invocation is a fresh map (NOT an
// alias of the input) so a downstream consumer mutating it does not
// bleed into the caller's input.
//
// Args-bound enforcement (iter-1 codex M2 fix): the rewrite MAY
// insert a `channel` / `project` key the caller did not supply
// (e.g. a Slack `update_message` op that targets by `ts`). Without
// a post-rewrite bound check, an invocation at the
// [MaxBrokerArgCount] boundary plus a missing rewrite key would
// produce an effective invocation OVER the bound and bypass the
// authoring-boundary DOS guard. This function therefore refuses the
// rewrite with [ErrInvalidBrokerArgs] when the effective key count
// would exceed [MaxBrokerArgCount].
func rewriteForScope(inv BrokerInvocation, scope Scope) (BrokerInvocation, error) {
	// Conservatively allocate one extra slot for the forced rewrite
	// key. The post-rewrite bound check below catches over-bound.
	args := make(map[string]string, len(inv.Args)+1)
	for k, v := range inv.Args {
		args[k] = v
	}
	switch inv.Kind {
	case BrokerSlack:
		args["channel"] = scope.LeadDMChannel
	case BrokerJira:
		args["project"] = scope.JiraSandboxProject
	}
	if len(args) > MaxBrokerArgCount {
		return BrokerInvocation{}, fmt.Errorf("%w: rewrite produced %d args (max %d)", ErrInvalidBrokerArgs, len(args), MaxBrokerArgCount)
	}
	return BrokerInvocation{
		Kind: inv.Kind,
		Op:   inv.Op,
		Args: args,
	}, nil
}

// publish emits the [TopicDryRunExecuted] event for a successful
// per-mode execution and returns the populated [Trace]. The publish
// uses [context.WithoutCancel] so a mid-flight caller cancel does not
// race the eventbus send fast-path (same discipline as
// [Proposer.Submit] / the M9.4.b callback dispatcher). The caller is
// responsible for any pre-publish ctx-gate; this helper assumes the
// caller has already decided whether to refuse a phantom event.
func (e *Executor) publish(ctx context.Context, req Request, outcomes []Outcome) (Trace, error) {
	now := e.deps.Clock.Now()
	corrID := req.ProposalID.String()
	trace := Trace{
		ProposalID:    req.ProposalID,
		ToolName:      req.ToolName,
		Mode:          req.Mode,
		Outcomes:      outcomes,
		ExecutedAt:    now,
		CorrelationID: corrID,
	}
	publishCtx := context.WithoutCancel(ctx)
	event := newDryRunExecutedEvent(req, outcomes, now, corrID)
	if err := e.deps.Publisher.Publish(publishCtx, TopicDryRunExecuted, event); err != nil {
		e.logErr(ctx, "publish tool_dry_run_executed failed", "proposal_id", req.ProposalID)
		return Trace{}, fmt.Errorf("%w: %w", ErrPublishDryRunExecuted, err)
	}
	return trace, nil
}

func (e *Executor) logErr(ctx context.Context, msg string, kv ...any) {
	if e.deps.Logger != nil {
		e.deps.Logger.Log(ctx, "approval: "+msg, kv...)
	}
}

// cloneBrokerInvocation defensively deep-copies the Args map so a
// caller-side mutation of the supplied invocation after Execute
// returns does not bleed into the stored [Outcome]. Op and Kind are
// value-typed strings — aliasing the backing bytes is safe (Go
// strings are immutable).
//
// Symmetry discipline (iter-1 critic m6 fix): the clone ALWAYS
// allocates an Args map, even when the input has nil Args. This
// keeps `Original.Args` and `Effective.Args` field-symmetric
// (`rewriteForScope` also always allocates non-nil); a caller doing
// `reflect.DeepEqual(o.Original, o.Effective)` on a nil-Args
// invocation under ghost mode now compares two empty maps rather
// than `nil` vs `map[string]string{}`.
func cloneBrokerInvocation(in BrokerInvocation) BrokerInvocation {
	args := make(map[string]string, len(in.Args))
	for k, v := range in.Args {
		args[k] = v
	}
	return BrokerInvocation{Kind: in.Kind, Op: in.Op, Args: args}
}
