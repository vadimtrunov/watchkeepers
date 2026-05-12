package approval

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// TopicToolProposed is the [eventbus.Bus] topic [Proposer.Submit]
// emits to once a [ProposalInput] passes validation and a durable
// [Proposal.ID] has been minted. Subscribers (M9.4.b webhook + AI
// reviewer wiring, M9.7 audit) consume this topic to drive the
// approval pipeline.
//
// Topic-name discipline: prefix `approval.` mirrors the
// `toolregistry.` prefix used by M9.1.a/b/M9.2 — the topic-name
// boundary is the package boundary.
const TopicToolProposed = "approval.tool_proposed"

// ToolProposed is the metadata-only payload published on
// [TopicToolProposed]. The payload exists so a subscriber can decide
// "I should fetch this proposal record" without dragging the full
// `code_draft` body onto the eventbus.
//
// PII discipline: the payload carries the proposal id, the tool
// name (a public identifier), the proposer agent id (already public
// across the runtime), the target-source enum, the list of
// capability ids (public dictionary entries per M9.3), the
// timestamp, and a correlation id. It DELIBERATELY does NOT carry:
//
//   - `Purpose` — agent free-form rationale that may name
//     customer-specific context.
//   - `PlainLanguageDescription` — lead-facing prose that may
//     embed customer names, Slack-mention syntax, or backticks (the
//     M9.2 iter-1 M3 lesson applies: untrusted authoring content
//     stays out of any rendered template until the renderer scrubs
//     it).
//   - `CodeDraft` — proprietary / AI-authored source code; a
//     verbose subscriber log would dump it otherwise.
//
// Subscribers that need the omitted fields query the future M9.4.b
// proposal store / M9.7 audit subscriber by `ProposalID`.
//
// Same field-allowlist discipline as M9.1.b's
// [toolregistry.EffectiveToolsetUpdated] and M9.2's
// [toolregistry.ToolShadowed]: a reflection-based test pins the
// payload to 7 fields by name + type, so adding a field requires
// the author bump the allowlist test AND consciously document why
// the new field's PII shape is OK.
type ToolProposed struct {
	// ProposalID matches [Proposal.ID]; the join key for any
	// subscriber querying the proposal store.
	ProposalID uuid.UUID

	// ToolName is the proposed [ProposalInput.Name] (validated by
	// [ProposalInput.Validate] to be lower_snake_case and bounded
	// in length). Becomes [toolregistry.Manifest.Name] post-
	// approval.
	ToolName string

	// ProposerID is the agent identity the [IdentityResolver]
	// returned at [Proposer.Submit] time — non-empty by construction.
	ProposerID string

	// TargetSource is the enum the agent chose (no `local` — see
	// [TargetSource] godoc).
	TargetSource TargetSource

	// CapabilityIDs is the defensively-deep-copied list of
	// capability ids from [ProposalInput.Capabilities]. The list
	// shape (not just a count) is on the payload because
	// capability ids are public dictionary entries and the
	// downstream M9.4.b approval card needs them to render
	// human-readable capability translations.
	CapabilityIDs []string

	// ProposedAt mirrors [Proposal.ProposedAt] — captured from the
	// configured [Clock] inside [Proposer.Submit].
	ProposedAt time.Time

	// CorrelationID mirrors [Proposal.CorrelationID]; the same
	// opaque shape as [toolregistry.SourceSynced.CorrelationID].
	CorrelationID string
}

// newToolProposedEvent constructs a [ToolProposed] event payload
// from a [Proposal]. Centralising the mapping keeps the field-
// parity contract auditable: a future addition to [Proposal] that
// needs to land on the event flows through this constructor, and
// the parallel reflection-based allowlist test on [ToolProposed]
// catches silent drift. Mirrors the M9.2 `newToolShadowedEvent`
// discipline.
func newToolProposedEvent(p Proposal) ToolProposed {
	caps := make([]string, len(p.Input.Capabilities))
	copy(caps, p.Input.Capabilities)
	return ToolProposed{
		ProposalID:    p.ID,
		ToolName:      p.Input.Name,
		ProposerID:    p.ProposerID,
		TargetSource:  p.Input.TargetSource,
		CapabilityIDs: caps,
		ProposedAt:    p.ProposedAt,
		CorrelationID: p.CorrelationID,
	}
}

// M9.4.b approval-execution topics. Each topic carries a metadata-only
// payload — same field-allowlist discipline as [ToolProposed]: a
// reflection-based test pins each payload to its declared fields by
// name + type, so adding a field requires the author bump the
// allowlist test AND consciously document the new field's PII shape.
//
// Topic-name discipline: prefix `approval.` mirrors the
// `toolregistry.` prefix used by M9.1.a/b/M9.2.
const (
	// TopicToolApproved is emitted when a proposal lands a `tool_approved`
	// decision via either the git-pr webhook (M9.4.b git-pr route) or the
	// slack-native approval-card `[Approve]` button (M9.4.b slack-native
	// route). The downstream M9.1.b registry rebuilder consumes this to
	// hot-load the now-approved tool; the M9.7 audit subscriber persists
	// the decision.
	TopicToolApproved = "approval.tool_approved"

	// TopicToolRejected is emitted when a proposal lands a `tool_rejected`
	// decision via the slack-native approval-card `[Reject]` button.
	// (The git-pr route has no symmetric "reject" event; a PR that closes
	// without merging does not emit anything — the proposal record simply
	// ages out.)
	TopicToolRejected = "approval.tool_rejected"

	// TopicDryRunRequested is emitted when the lead clicks
	// `[Test in my DM]` on a slack-native approval card. M9.4.c's
	// dry-run executor subscribes and runs the proposed tool with the
	// `scoped` dry-run mode forced to the lead's DM channel. M9.4.b
	// emits the event; the executor itself is M9.4.c work.
	TopicDryRunRequested = "approval.tool_dry_run_requested"

	// TopicQuestionAsked is emitted when the lead clicks `[Ask questions]`
	// on a slack-native approval card. Subscribers (operator dashboards,
	// the M9.7 audit row) record that the lead wants a human-readable
	// follow-up; M9.4.b does not itself open a thread.
	TopicQuestionAsked = "approval.tool_question_asked"

	// TopicDryRunExecuted is emitted by the M9.4.c [Executor] after a
	// `ghost` or `scoped` mode execution completes. The payload is
	// metadata-only (proposal id, tool name, mode, per-broker invocation
	// counts, timestamp, correlation id) — the full per-invocation
	// trace (with caller-supplied `Args` bodies) is returned to the
	// in-process caller via [Trace] but NEVER flows onto the eventbus.
	// The `none` mode does not emit this topic (the executor returns
	// [ErrPreApprovalWarning] before any side effect).
	TopicDryRunExecuted = "approval.tool_dry_run_executed"
)

// Route identifies which approval flow produced a decision.
// Closed-set discriminator pattern: a new route requires a new wiring
// path in M9.4.b's surface code.
type Route string

const (
	// RouteGitPR identifies the [TopicToolApproved] events
	// emitted by the git-pr webhook receiver on PR merge.
	RouteGitPR Route = "git-pr"

	// RouteSlackNative identifies the [TopicToolApproved] /
	// [TopicToolRejected] / [TopicDryRunRequested] / [TopicQuestionAsked]
	// events emitted by the slack-native approval-card callback
	// dispatcher.
	RouteSlackNative Route = "slack-native"
)

// ToolApproved is the metadata-only payload published on
// [TopicToolApproved]. Same PII boundary as [ToolProposed]: the payload
// carries identifiers (proposal id, tool name, approver id, route) but
// NEVER `Purpose`, `PlainLanguageDescription`, or `CodeDraft`. The git-pr
// route adds `PRURL` + `MergedSHA` (both public URLs / opaque shas, no
// PII).
type ToolApproved struct {
	// ProposalID matches [Proposal.ID]; the join key for any
	// subscriber querying the proposal store.
	ProposalID uuid.UUID

	// ToolName is the proposed [ProposalInput.Name].
	ToolName string

	// ApproverID identifies who approved. For [RouteGitPR],
	// this is the git platform identifier of the merging human (from
	// the webhook payload's `approver` field). For
	// [RouteSlackNative], this is the Slack user id of the
	// lead who clicked `[Approve]`.
	ApproverID string

	// Route discriminates which flow produced the approval.
	Route Route

	// TargetSource mirrors the proposal's target source — repeated on
	// the event so downstream consumers do not need a proposal-store
	// round-trip to know which source the now-approved tool belongs
	// to.
	TargetSource TargetSource

	// SourceName is the [toolregistry.SourceConfig.Name] the M9.1.a
	// scheduler will be asked to re-sync. Empty when the resolver was
	// unable to map the [TargetSource] to a configured source — the
	// caller MUST treat empty as a wiring bug, not a runtime branch.
	SourceName string

	// PRURL is the public URL of the merged PR. Populated on the
	// git-pr route; empty on slack-native.
	PRURL string

	// MergedSHA is the merge commit sha. Populated on the git-pr
	// route; empty on slack-native.
	MergedSHA string

	// ApprovedAt is the wall-clock timestamp captured from the
	// configured [Clock] at decision time.
	ApprovedAt time.Time

	// CorrelationID is the canonical string form of [ProposalID]
	// (UUIDv7) when the lookup resolved a [Proposal]; otherwise an
	// IDGenerator-minted fresh UUIDv7 string. Mirrors
	// [Proposal.CorrelationID]'s "time-ordered + unique" discipline.
	CorrelationID string
}

// ToolRejected is the metadata-only payload published on
// [TopicToolRejected]. Same field-allowlist + PII discipline as
// [ToolApproved].
type ToolRejected struct {
	// ProposalID matches [Proposal.ID].
	ProposalID uuid.UUID

	// ToolName is the proposed [ProposalInput.Name].
	ToolName string

	// RejecterID is the Slack user id of the lead who clicked
	// `[Reject]` on the slack-native approval card.
	RejecterID string

	// Route discriminates which flow produced the rejection. M9.4.b
	// only emits via [RouteSlackNative]; the field exists for
	// symmetry with [ToolApproved] (M9.4.b's git-pr route has no
	// reject event — a closed-without-merge PR ages out silently).
	Route Route

	// RejectedAt is the wall-clock timestamp at decision time.
	RejectedAt time.Time

	// CorrelationID mirrors [ToolApproved.CorrelationID].
	CorrelationID string
}

// DryRunRequested is the metadata-only payload published on
// [TopicDryRunRequested]. The M9.4.c dry-run executor subscribes to this
// topic and runs the proposed tool with the `scoped` dry-run mode
// forced to the lead's DM channel.
type DryRunRequested struct {
	// ProposalID matches [Proposal.ID].
	ProposalID uuid.UUID

	// ToolName is the proposed [ProposalInput.Name].
	ToolName string

	// RequesterID is the Slack user id of the lead who clicked
	// `[Test in my DM]`.
	RequesterID string

	// LeadDMChannel is the resolved Slack DM channel id where the
	// dry-run output MUST be forced. Required (non-empty); the
	// production wiring resolves it before dispatch.
	LeadDMChannel string

	// RequestedAt is the wall-clock timestamp at click time.
	RequestedAt time.Time

	// CorrelationID mirrors [ToolApproved.CorrelationID].
	CorrelationID string
}

// QuestionAsked is the metadata-only payload published on
// [TopicQuestionAsked]. Subscribers record the operator-visible signal
// that the lead wants a human-readable follow-up on the proposal.
type QuestionAsked struct {
	// ProposalID matches [Proposal.ID].
	ProposalID uuid.UUID

	// ToolName is the proposed [ProposalInput.Name].
	ToolName string

	// AskerID is the Slack user id of the lead who clicked
	// `[Ask questions]`.
	AskerID string

	// AskedAt is the wall-clock timestamp at click time.
	AskedAt time.Time

	// CorrelationID mirrors [ToolApproved.CorrelationID].
	CorrelationID string
}

// DryRunExecuted is the metadata-only payload published on
// [TopicDryRunExecuted]. Same PII boundary as [ToolProposed]: the
// payload carries opaque identifiers + counts, NEVER the per-
// invocation `Args` bodies. The full per-invocation trace stays
// in-process on the [Trace] return value of [Executor.Execute].
//
// `BrokerKindCounts` is a per-[BrokerKind] count of invocations the
// dry-running tool would have made; the SHAPE (not the bodies) lands
// on the eventbus so a downstream operator dashboard can render
// "tool X would have made 3 Slack sends, 1 Jira write in dry-run".
// The shape is fixed at the M9.4.c [BrokerKind] closed set; adding a
// new broker requires bumping the allowlist test AND documenting the
// new key's PII shape (none of the broker count keys are PII — they
// are public dictionary entries).
type DryRunExecuted struct {
	// ProposalID matches [Proposal.ID]; the join key for any subscriber
	// querying the proposal store.
	ProposalID uuid.UUID

	// ToolName is the proposed [ProposalInput.Name].
	ToolName string

	// Mode is the [toolregistry.DryRunMode] the executor branched on
	// (`ghost` or `scoped` — `none` never publishes this event).
	// Carried as the typed string so a future decoder validates
	// incoming wire values uniformly (iter-1 codex D fix).
	Mode toolregistry.DryRunMode

	// BrokerKindCounts is a per-[BrokerKind] count of invocations.
	// Defensively-deep-copied map (the executor builds a fresh map on
	// each publish; downstream subscribers receive a fresh map shape
	// over the eventbus).
	BrokerKindCounts map[string]int

	// InvocationCount is the total number of invocations across all
	// brokers — redundant with `sum(BrokerKindCounts)` but carried for
	// dashboard ergonomics (a single field beats a map fold).
	InvocationCount int

	// ExecutedAt mirrors [Trace.ExecutedAt] — captured from the
	// configured [Clock] inside [Executor.Execute].
	ExecutedAt time.Time

	// CorrelationID mirrors [Trace.CorrelationID].
	CorrelationID string
}

// newDryRunExecutedEvent constructs the [DryRunExecuted] payload from
// the [Executor]'s per-mode outcome list. Centralising the mapping
// keeps the field-parity contract auditable (mirrors
// [newToolProposedEvent]). The constructor builds a FRESH
// `BrokerKindCounts` map so caller-side mutation of the returned
// event does not corrupt any in-memory state.
func newDryRunExecutedEvent(req Request, outcomes []Outcome, executedAt time.Time, corrID string) DryRunExecuted {
	counts := make(map[string]int, 2)
	for _, o := range outcomes {
		counts[string(o.Original.Kind)]++
	}
	return DryRunExecuted{
		ProposalID:       req.ProposalID,
		ToolName:         req.ToolName,
		Mode:             req.Mode,
		BrokerKindCounts: counts,
		InvocationCount:  len(outcomes),
		ExecutedAt:       executedAt,
		CorrelationID:    corrID,
	}
}

// Validate reports whether `r` is in the closed [Route] set.
// Returns [ErrInvalidRoute] otherwise (including the empty
// string).
func (r Route) Validate() error {
	switch r {
	case RouteGitPR, RouteSlackNative:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidRoute, string(r))
	}
}
