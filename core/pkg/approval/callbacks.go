// Doc-block at file head documenting the seam contract.
//
// resolution order: nil-dep check (panic) → Click.Validate
// (ErrInvalidActionID / ErrInvalidButtonValue / ErrCardMissingLeadDM)
// → ctx.Err pass-through → DecodeApprovalActionID → ProposalLookup
// (ErrProposalNotFound pass-through) → branch on Click.Button ∈
// {approve, reject, test_in_my_dm, ask_questions} → mint correlation
// id (proposal's, else fresh UUIDv7) → build event payload → ctx.Err
// pre-publish → Publisher.Publish (cancel-detached child ctx via
// context.WithoutCancel) → on `test_in_my_dm`: also invoke optional
// DryRunRequester seam → return Click outcome to caller.
//
// audit discipline: the dispatcher never imports `keeperslog` and
// never calls `.Append(` (see source-grep AC). The audit log entry for
// each button click lives in the M9.7 audit subscriber observing the
// approval-execution topics; the dispatcher emits the event and
// surfaces the error, with no audit side-effect at this layer.
//
// PII discipline: the dispatcher decodes only the action_id + button
// value + lead identifiers from the platform-side Slack BlockActions
// payload. It never touches `Proposal.Input.CodeDraft` /
// `Proposal.Input.Purpose` / `Proposal.Input.PlainLanguageDescription`
// even though [ProposalLookup.Lookup] returns the full proposal —
// only `ID`, `Input.Name`, `Input.TargetSource`, and `CorrelationID`
// are read.

package approval

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// DryRunRequester is the optional seam the M9.4.c dry-run executor
// will plug into. M9.4.b's [TopicDryRunRequested] event is the durable
// record; this seam exists so production wiring can additionally
// trigger an in-process executor call (instead of relying solely on
// the eventbus subscriber). A nil seam is the documented degradation
// path for "M9.4.c not yet wired" — the event still emits.
//
// Context contract: the dispatcher invokes [RequestDryRun] with a
// [context.WithoutCancel]-detached child ctx (see [publishDryRun] in
// this file). The detached ctx is INTENTIONAL — the click-handler's
// caller ctx may carry the slack/inbound ingress timeout, and
// aborting the dry-run executor on that timeout would surface the
// dry-run failure on a path the lead-facing card has already
// dismissed. Implementers MUST therefore:
//
//   - Apply their OWN bounded timeout (the production executor wraps
//     the call in `context.WithTimeout`).
//   - NOT rely on the supplied ctx for cancellation propagation from
//     the original Slack click.
//   - Return nil on successful dispatch (the dry-run was queued or
//     fired synchronously, depending on the executor).
//   - Return any error on failure; the dispatcher wraps it as
//     `approval: dry-run requester: %w` (the event publish has
//     already succeeded by the time this seam runs, so the failure
//     is "executor unavailable", not "event lost").
type DryRunRequester interface {
	RequestDryRun(ctx context.Context, proposalID uuid.UUID, leadDMChannel string) error
}

// Click is the typed input the callback dispatcher consumes. The
// platform-side wiring (a `messenger/slack/inbound.InteractionDispatcher`
// impl) decodes the BlockActions payload from Slack and constructs a
// [Click], then hands it to [CallbackDispatcher.Dispatch].
type Click struct {
	// ActionID is the opaque payload [DecodeApprovalActionID]
	// consumes. Required.
	ActionID string

	// Button is the [ButtonAction] decoded from the BlockActions
	// `value` field. Required; must pass [ButtonAction.Validate].
	Button ButtonAction

	// LeadID is the Slack user id of the lead who clicked. Required.
	LeadID string

	// LeadDMChannel is the Slack DM channel id for the clicking
	// lead. Required for [ButtonActionTestInDM] (the dry-run
	// executor MUST force Slack sends to the lead's DM); optional
	// for the other three actions (the dispatcher does not consume
	// it on those paths).
	LeadDMChannel string
}

// Validate enforces the Click pre-conditions. Returns the first
// applicable sentinel:
//
//   - [ErrInvalidActionID] when [ActionID] is empty (full shape check
//     deferred to [DecodeApprovalActionID]).
//   - [ErrInvalidButtonValue] when [Button] is outside the closed set
//     or [LeadID] is empty.
//   - [ErrCardMissingLeadDM] when [Button] is
//     [ButtonActionTestInDM] and [LeadDMChannel] is empty.
func (c Click) Validate() error {
	if c.ActionID == "" {
		return fmt.Errorf("%w: empty action_id", ErrInvalidActionID)
	}
	if c.LeadID == "" {
		return fmt.Errorf("%w: empty lead_id", ErrInvalidButtonValue)
	}
	if err := c.Button.Validate(); err != nil {
		return err
	}
	if c.Button == ButtonActionTestInDM && c.LeadDMChannel == "" {
		return ErrCardMissingLeadDM
	}
	return nil
}

// CallbackDispatcherDeps bundles the required dependencies for
// [NewCallbackDispatcher]. Non-`Logger` / non-`DryRunRequester` fields
// are required; nil values panic with a named-field message.
type CallbackDispatcherDeps struct {
	// Lookup resolves the stored [Proposal] by id. Required.
	Lookup ProposalLookup

	// Decisions is the [DecisionRecorder] seam that claims the
	// terminal `tool_approved` / `tool_rejected` decision exactly
	// once per proposal id. Required; guards against double-click,
	// platform-side BlockActions retry, and stale-card replay.
	Decisions DecisionRecorder

	// Publisher emits the per-button event. Required.
	Publisher Publisher

	// Clock stamps the per-event timestamp. Required.
	Clock Clock

	// IDGenerator mints the correlation-id fallback. Required.
	IDGenerator IDGenerator

	// DryRunRequester is invoked AFTER a successful
	// [TopicDryRunRequested] publish on [ButtonActionTestInDM]
	// clicks. Optional; nil is the documented "M9.4.c not yet wired"
	// degradation path.
	DryRunRequester DryRunRequester

	// Logger receives per-click diagnostic entries. Optional; a nil
	// [Logger] silently discards entries.
	Logger Logger
}

// CallbackDispatcher is the M9.4.b slack-native approval-card button
// dispatcher. Construct via [NewCallbackDispatcher]; the zero value is
// not usable. The dispatcher is safe for concurrent use across
// goroutines.
type CallbackDispatcher struct {
	deps CallbackDispatcherDeps
}

// NewCallbackDispatcher constructs a [*CallbackDispatcher]. Panics
// with a named-field message when any required dependency in `deps`
// is nil; mirrors [New] / [NewWebhook] / [NewReviewer].
func NewCallbackDispatcher(deps CallbackDispatcherDeps) *CallbackDispatcher {
	if deps.Lookup == nil {
		panic("approval: NewCallbackDispatcher: deps.Lookup must not be nil")
	}
	if deps.Decisions == nil {
		panic("approval: NewCallbackDispatcher: deps.Decisions must not be nil")
	}
	if deps.Publisher == nil {
		panic("approval: NewCallbackDispatcher: deps.Publisher must not be nil")
	}
	if deps.Clock == nil {
		panic("approval: NewCallbackDispatcher: deps.Clock must not be nil")
	}
	if deps.IDGenerator == nil {
		panic("approval: NewCallbackDispatcher: deps.IDGenerator must not be nil")
	}
	return &CallbackDispatcher{deps: deps}
}

// Dispatch runs the resolution order documented at the top of this
// file. The function-level godoc names the four per-button branches
// for the orchestrator's reference; per-branch behaviour is in the
// dedicated helpers below.
//
// Returns nil on a successful publish. Returns the wrapped sentinel
// on any failure:
//
//   - [ErrInvalidActionID] / [ErrInvalidButtonValue] /
//     [ErrCardMissingLeadDM] from [Click.Validate].
//   - [ErrInvalidActionID] from [DecodeApprovalActionID].
//   - ctx.Err on a cancelled context (validation runs first so the
//     caller's validation-vs-cancel boundary stays stable — same
//     discipline as [Proposer.Submit]).
//   - [ErrProposalNotFound] pass-through from [ProposalLookup.Lookup].
//   - [ErrPublishToolApproved] / [ErrPublishToolRejected] /
//     [ErrPublishDryRunRequested] / [ErrPublishQuestionAsked] on
//     a [Publisher.Publish] failure.
func (d *CallbackDispatcher) Dispatch(ctx context.Context, click Click) error {
	if err := click.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	proposalID, err := DecodeApprovalActionID(click.ActionID)
	if err != nil {
		return err
	}
	proposal, err := d.deps.Lookup.Lookup(ctx, proposalID)
	if err != nil {
		return err
	}
	corrID, err := d.newCorrelationID(proposal)
	if err != nil {
		return err
	}
	now := d.deps.Clock.Now()

	switch click.Button {
	case ButtonActionApprove:
		return d.publishApproved(ctx, proposal, click, corrID, now)
	case ButtonActionReject:
		return d.publishRejected(ctx, proposal, click, corrID, now)
	case ButtonActionTestInDM:
		return d.publishDryRun(ctx, proposal, click, corrID, now)
	case ButtonActionAskQuestions:
		return d.publishQuestion(ctx, proposal, click, corrID, now)
	}
	// Click.Validate has already constrained Button to the closed
	// set; this branch is unreachable on a validated click but is
	// retained as a defence in depth.
	return fmt.Errorf("%w: %q", ErrInvalidButtonValue, string(click.Button))
}

// publishApproved emits [TopicToolApproved] for an [ButtonActionApprove]
// click. The route is [RouteSlackNative]; PR url + merged sha
// + source name are empty (the slack-native flow does not run the
// git-pr re-sync; the M9.1.b registry rebuilder consumes the event
// directly).
//
// Idempotency: a same-kind replay (double-click, BlockActions retry)
// observes the [DecisionRecorder] claim and silent-no-ops. A
// conflicting-kind attempt (Approve after Reject already landed)
// surfaces [ErrDecisionConflict].
func (d *CallbackDispatcher) publishApproved(ctx context.Context, p Proposal, c Click, corrID string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	publishCtx := context.WithoutCancel(ctx)
	firstTime, err := d.deps.Decisions.MarkDecided(publishCtx, p.ID, DecisionApproved)
	if err != nil {
		d.logErr(ctx, "mark decided failed", "proposal_id", p.ID)
		return err
	}
	if !firstTime {
		// Same-kind replay — silent idempotent success.
		d.logErr(ctx, "duplicate approve click", "proposal_id", p.ID)
		return nil
	}
	event := ToolApproved{
		ProposalID:    p.ID,
		ToolName:      p.Input.Name,
		ApproverID:    c.LeadID,
		Route:         RouteSlackNative,
		TargetSource:  p.Input.TargetSource,
		ApprovedAt:    now,
		CorrelationID: corrID,
	}
	if err := d.deps.Publisher.Publish(publishCtx, TopicToolApproved, event); err != nil {
		_ = d.deps.Decisions.UnmarkDecided(publishCtx, p.ID, DecisionApproved)
		d.logErr(ctx, "publish tool_approved failed", "proposal_id", p.ID)
		return fmt.Errorf("%w: %w", ErrPublishToolApproved, err)
	}
	return nil
}

// publishRejected emits [TopicToolRejected] for a [ButtonActionReject]
// click. Same idempotency contract as [publishApproved].
func (d *CallbackDispatcher) publishRejected(ctx context.Context, p Proposal, c Click, corrID string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	publishCtx := context.WithoutCancel(ctx)
	firstTime, err := d.deps.Decisions.MarkDecided(publishCtx, p.ID, DecisionRejected)
	if err != nil {
		d.logErr(ctx, "mark decided failed", "proposal_id", p.ID)
		return err
	}
	if !firstTime {
		d.logErr(ctx, "duplicate reject click", "proposal_id", p.ID)
		return nil
	}
	event := ToolRejected{
		ProposalID:    p.ID,
		ToolName:      p.Input.Name,
		RejecterID:    c.LeadID,
		Route:         RouteSlackNative,
		RejectedAt:    now,
		CorrelationID: corrID,
	}
	if err := d.deps.Publisher.Publish(publishCtx, TopicToolRejected, event); err != nil {
		_ = d.deps.Decisions.UnmarkDecided(publishCtx, p.ID, DecisionRejected)
		d.logErr(ctx, "publish tool_rejected failed", "proposal_id", p.ID)
		return fmt.Errorf("%w: %w", ErrPublishToolRejected, err)
	}
	return nil
}

// publishDryRun emits [TopicDryRunRequested] for a
// [ButtonActionTestInDM] click AND optionally invokes the
// [DryRunRequester] seam. The event publish is the durable record; a
// nil [DryRunRequester] is the documented "M9.4.c not yet wired"
// degradation path — the event still emits.
func (d *CallbackDispatcher) publishDryRun(ctx context.Context, p Proposal, c Click, corrID string, now time.Time) error {
	event := DryRunRequested{
		ProposalID:    p.ID,
		ToolName:      p.Input.Name,
		RequesterID:   c.LeadID,
		LeadDMChannel: c.LeadDMChannel,
		RequestedAt:   now,
		CorrelationID: corrID,
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	publishCtx := context.WithoutCancel(ctx)
	if err := d.deps.Publisher.Publish(publishCtx, TopicDryRunRequested, event); err != nil {
		d.logErr(ctx, "publish tool_dry_run_requested failed", "proposal_id", p.ID)
		return fmt.Errorf("%w: %w", ErrPublishDryRunRequested, err)
	}
	if d.deps.DryRunRequester != nil {
		if err := d.deps.DryRunRequester.RequestDryRun(publishCtx, p.ID, c.LeadDMChannel); err != nil {
			d.logErr(ctx, "dry-run requester failed", "proposal_id", p.ID)
			// Surface the executor failure but DO NOT wrap with a
			// publish sentinel — the event already landed; the
			// caller distinguishes via `errors.Is`.
			return fmt.Errorf("approval: dry-run requester: %w", err)
		}
	}
	return nil
}

// publishQuestion emits [TopicQuestionAsked] for a
// [ButtonActionAskQuestions] click.
func (d *CallbackDispatcher) publishQuestion(ctx context.Context, p Proposal, c Click, corrID string, now time.Time) error {
	event := QuestionAsked{
		ProposalID:    p.ID,
		ToolName:      p.Input.Name,
		AskerID:       c.LeadID,
		AskedAt:       now,
		CorrelationID: corrID,
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	publishCtx := context.WithoutCancel(ctx)
	if err := d.deps.Publisher.Publish(publishCtx, TopicQuestionAsked, event); err != nil {
		d.logErr(ctx, "publish tool_question_asked failed", "proposal_id", p.ID)
		return fmt.Errorf("%w: %w", ErrPublishQuestionAsked, err)
	}
	return nil
}

func (d *CallbackDispatcher) newCorrelationID(p Proposal) (string, error) {
	if p.CorrelationID != "" {
		return p.CorrelationID, nil
	}
	id, err := d.deps.IDGenerator.NewUUID()
	if err != nil {
		return "", fmt.Errorf("approval: callback id generator: %w", err)
	}
	return id.String(), nil
}

func (d *CallbackDispatcher) logErr(ctx context.Context, msg string, kv ...any) {
	if d.deps.Logger != nil {
		d.deps.Logger.Log(ctx, "approval: "+msg, kv...)
	}
}
