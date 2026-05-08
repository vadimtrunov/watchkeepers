package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/cards"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/inbound"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// keepers_log event_type values the dispatcher emits onto the audit
// chain. Pinned per AC6 and the M6.3.b TASK §"Acceptance criteria".
// Project convention: snake_case past-tense verbs prefixed with the
// surface owner (`approval`).
const (
	auditEventActionReceived  = "approval_card_action_received"
	auditEventResolved        = "approval_resolved"
	auditEventReplaySucceeded = "approval_replay_succeeded"
	auditEventReplayFailed    = "approval_replay_failed"
)

// reason vocabulary on the negative branches. Closed set so a
// downstream Recall query can group failures without parsing free-form
// strings.
const (
	reasonMalformedActionID  = "malformed_action_id"
	reasonInvalidButtonValue = "invalid_button_value"
	reasonUnknownToken       = "unknown_token"
	reasonStaleState         = "stale_state"
	reasonReplayError        = "replay_error"
)

// payload key vocabulary on the audit rows. Snake_case, hoisted so
// the test assertions and the production builders share names.
const (
	payloadKeyToolName       = "tool_name"
	payloadKeyApprovalToken  = "approval_token"
	payloadKeyDecision       = "decision"
	payloadKeyReason         = "reason"
	payloadKeyErrorClass     = "error_class"
	payloadKeyInteractionTyp = "interaction_type"
)

// interactionTypeBlockActions is the Slack Interactivity payload type
// the dispatcher routes on. Other types (`view_submission`,
// `view_closed`, …) are out of scope for M6.3.b; the dispatcher ACKs
// silently and emits no audit row for foreign types.
const interactionTypeBlockActions = "block_actions"

// AuditAppender is the minimal subset of [keeperslog.Writer] the
// dispatcher consumes — only Append. Defined locally so unit tests
// can substitute a tiny fake. Mirrors the inbound-handler
// AuditAppender pattern.
type AuditAppender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// Compile-time assertion: the production [*keeperslog.Writer]
// satisfies [AuditAppender].
var _ AuditAppender = (*keeperslog.Writer)(nil)

// Replayer is the seam the dispatcher consults on the `approved`
// branch to re-invoke the matching M6.2.x tool. The interface is
// tool-agnostic: the dispatcher hands the replayer the tool name,
// the params_json snapshot, and the approval_token; the replayer
// decodes the params, picks the matching M6.2.x function, and
// returns nil on success or a wrapped error on failure.
//
// The production implementation (M6.3.c or later) composes a
// [spawn.WatchmasterWriteClient] + [spawn.WatchmasterNotebookClient]
// + [keeperslog.Writer] + a [spawn.Claim] resolver into a struct
// that branches on `tool` to call the right M6.2.x function.
// M6.3.b ships the interface and the tests; the production wiring
// lands in a follow-up.
//
// IMPORTANT: a non-nil error return DOES NOT trigger a DAO rollback.
// The dispatcher emits `approval_replay_failed` and surfaces the
// error on the returned audit chain; the operator retries via a
// fresh approval flow (per AC9).
type Replayer interface {
	// Replay re-invokes the M6.2.x tool whose name is `tool` with the
	// params snapshot in `paramsJSON`. The approval_token is included
	// so the replayer can thread it back into the tool's request
	// struct (every M6.2.x tool validates a non-empty ApprovalToken).
	Replay(ctx context.Context, tool string, paramsJSON json.RawMessage, approvalToken string) error
}

// Option configures a [Dispatcher] at construction time.
type Option func(*Dispatcher)

// WithAuditAppender wires the [AuditAppender] consulted on every
// dispatch. A nil appender is ignored; dispatchers without an
// explicit appender skip audit emission entirely (test-only mode).
func WithAuditAppender(a AuditAppender) Option {
	return func(d *Dispatcher) {
		if a != nil {
			d.audit = a
		}
	}
}

// Dispatcher is the M6.3.b implementation of
// [inbound.InteractionDispatcher]. Construct via [New]; the zero
// value is NOT usable (DAO + Replayer + SpawnKickoff are required).
//
// All fields are immutable after construction; the dispatcher is
// safe for concurrent use across goroutines.
type Dispatcher struct {
	dao          spawn.PendingApprovalDAO
	replayer     Replayer
	spawnKickoff SpawnKickoff
	audit        AuditAppender
}

// New constructs a [Dispatcher] backed by the supplied DAO, Replayer,
// and SpawnKickoff. All three are required; passing a nil dependency
// panics with a clear message (M6.3.b dependency-required pattern).
// An audit appender is OPTIONAL — production callers MUST wire one
// (AC6 mandates emission); the nil fallback exists solely for unit
// tests that exercise non-audit branches.
func New(dao spawn.PendingApprovalDAO, replayer Replayer, spawnKickoff SpawnKickoff, opts ...Option) *Dispatcher {
	if dao == nil {
		panic("approval: New: DAO must not be nil")
	}
	if replayer == nil {
		panic("approval: New: Replayer must not be nil")
	}
	if spawnKickoff == nil {
		panic("approval: New: SpawnKickoff must not be nil")
	}
	d := &Dispatcher{dao: dao, replayer: replayer, spawnKickoff: spawnKickoff}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Compile-time assertion: [*Dispatcher] satisfies
// [inbound.InteractionDispatcher].
var _ inbound.InteractionDispatcher = (*Dispatcher)(nil)

// blockActionsPayload is the subset of the Slack `block_actions`
// payload the dispatcher decodes. The handler's [inbound.Interaction]
// already carried the parsed `type` field; we re-parse `actions[]` to
// pull the action_id + value off the clicked button.
type blockActionsPayload struct {
	Actions []blockActionPayload `json:"actions"`
}

// blockActionPayload is the shape of an individual entry in
// `actions[]`. Slack's payload also carries `type`, `block_id`, etc.,
// but M6.3.b only consumes action_id + value.
type blockActionPayload struct {
	ActionID string `json:"action_id"`
	Value    string `json:"value"`
}

// DispatchInteraction satisfies [inbound.InteractionDispatcher]. The
// resolution order matches the package docblock; see that for the
// audit-chain shape on each branch.
func (d *Dispatcher) DispatchInteraction(ctx context.Context, p inbound.Interaction) error {
	if p.Type != interactionTypeBlockActions {
		// Foreign payload types are ACKed silently; M6.3.b does not
		// own them. M6.3.c may extend this dispatcher or compose a
		// sibling for `view_submission`.
		return nil
	}

	var bap blockActionsPayload
	if err := json.Unmarshal(p.Raw, &bap); err != nil {
		// A signature-valid envelope that fails to decode is the
		// inbound handler's bug, not ours; surface a single audit
		// row so the operator notices and stop.
		d.appendActionReceived(ctx, "", "", "", reasonMalformedActionID)
		return fmt.Errorf("approval: decode block_actions: %w", err)
	}
	if len(bap.Actions) == 0 {
		d.appendActionReceived(ctx, "", "", "", reasonMalformedActionID)
		return errors.New("approval: block_actions payload missing actions")
	}

	// M6.3.b only consumes the FIRST action — Slack only ever sets
	// one entry per Approve/Reject card per AC2 (single button per
	// click).
	action := bap.Actions[0]
	tool, token, err := cards.DecodeActionID(action.ActionID)
	if err != nil {
		// AC9: malformed action_id → 1 event with
		// reason=malformed_action_id. No DAO call. No replay.
		d.appendActionReceived(ctx, "", "", action.Value, reasonMalformedActionID)
		return fmt.Errorf("approval: decode action_id: %w", err)
	}

	// Validate the button value before consulting the DAO so a
	// malformed `value` (typo on the button payload) cannot smuggle
	// a row into a non-terminal state.
	decision, err := decisionFromButtonValue(action.Value)
	if err != nil {
		reason := reasonMalformedActionID
		if errors.Is(err, cards.ErrInvalidButtonValue) {
			reason = reasonInvalidButtonValue
		}
		d.appendActionReceived(ctx, tool, token, action.Value, reason)
		return fmt.Errorf("approval: decode button value: %w", err)
	}

	// Step 1 audit row — `received`. Always emitted on every branch
	// that survives the action_id decode.
	d.appendActionReceived(ctx, tool, token, action.Value, "")

	// Step 2 — DAO.Resolve. The state-machine check happens here:
	// unknown tokens, stale states, and invalid decisions surface
	// as typed errors.
	if err := d.dao.Resolve(ctx, token, decision); err != nil {
		d.appendResolveError(ctx, tool, token, decision, err)
		return fmt.Errorf("approval: dao resolve: %w", err)
	}

	// Step 3 audit row — `resolved`.
	d.appendResolved(ctx, tool, token, decision, "")

	// Step 4 — replay branch. Only on approved.
	if decision == spawn.PendingApprovalStateRejected {
		return nil
	}

	row, err := d.dao.Get(ctx, token)
	if err != nil {
		// A row we just resolved cannot vanish; surface the error
		// onto the audit chain as a replay failure with the typed
		// error class so a downstream consumer can group it.
		d.appendReplayFailed(ctx, tool, token, err)
		return fmt.Errorf("approval: dao get after resolve: %w", err)
	}

	// Branch on tool name (M7.1.b): the `propose_spawn` tool routes
	// into the spawn-saga kickoff; every other M6.2.x tool stays on
	// the existing replayer path. The reject branch (handled above)
	// is unchanged.
	if row.ToolName == spawn.PendingApprovalToolProposeSpawn {
		if err := d.spawnKickoff.Kickoff(ctx, uuid.New(), uuid.New(), token); err != nil {
			// Mirrors the replayer-error policy: emit the failed
			// audit row and surface the error; do NOT roll back the
			// DAO transition.
			d.appendReplayFailed(ctx, tool, token, err)
			return fmt.Errorf("approval: spawn kickoff: %w", err)
		}
		d.appendReplaySucceeded(ctx, tool, token)
		return nil
	}

	if err := d.replayer.Replay(ctx, tool, row.ParamsJSON, token); err != nil {
		// AC9: do NOT roll back the DAO transition. Audit the
		// failure and return — the operator restarts the flow.
		d.appendReplayFailed(ctx, tool, token, err)
		return fmt.Errorf("approval: replay: %w", err)
	}

	d.appendReplaySucceeded(ctx, tool, token)
	return nil
}

// decisionFromButtonValue maps the Slack button `value` to the
// matching [spawn.PendingApprovalDecision]. The closed-set vocabulary
// lives in the cards package; this helper is the reverse map.
func decisionFromButtonValue(v string) (spawn.PendingApprovalDecision, error) {
	switch v {
	case cards.ButtonValueApprove:
		return spawn.PendingApprovalStateApproved, nil
	case cards.ButtonValueReject:
		return spawn.PendingApprovalStateRejected, nil
	}
	return "", fmt.Errorf("%w: button value %q", cards.ErrInvalidButtonValue, v)
}

// appendActionReceived emits the leading `approval_card_action_received`
// audit row. `reason` is empty on the happy branch and one of the
// closed-set reason vocabulary values on the negative branches.
func (d *Dispatcher) appendActionReceived(ctx context.Context, tool, token, buttonValue, reason string) {
	if d.audit == nil {
		return
	}
	payload := map[string]any{
		payloadKeyInteractionTyp: interactionTypeBlockActions,
	}
	if tool != "" {
		payload[payloadKeyToolName] = tool
	}
	if token != "" {
		payload[payloadKeyApprovalToken] = token
	}
	if buttonValue != "" {
		payload[payloadKeyDecision] = buttonValue
	}
	if reason != "" {
		payload[payloadKeyReason] = reason
	}
	_, _ = d.audit.Append(ctx, keeperslog.Event{EventType: auditEventActionReceived, Payload: payload})
}

// appendResolved emits the `approval_resolved` audit row on the happy
// branch. `reason` is empty on the happy branch.
func (d *Dispatcher) appendResolved(
	ctx context.Context,
	tool, token string,
	decision spawn.PendingApprovalDecision,
	reason string,
) {
	if d.audit == nil {
		return
	}
	payload := map[string]any{
		payloadKeyToolName:      tool,
		payloadKeyApprovalToken: token,
		payloadKeyDecision:      string(decision),
	}
	if reason != "" {
		payload[payloadKeyReason] = reason
	}
	_, _ = d.audit.Append(ctx, keeperslog.Event{EventType: auditEventResolved, Payload: payload})
}

// appendResolveError emits the `approval_resolved` row on the
// negative branches: unknown token (NotFound) or stale state. The
// `reason` field maps the typed DAO error onto the closed-set
// vocabulary.
func (d *Dispatcher) appendResolveError(
	ctx context.Context,
	tool, token string,
	decision spawn.PendingApprovalDecision,
	err error,
) {
	if d.audit == nil {
		return
	}
	reason := reasonReplayError
	switch {
	case errors.Is(err, spawn.ErrPendingApprovalNotFound):
		reason = reasonUnknownToken
	case errors.Is(err, spawn.ErrPendingApprovalStaleState):
		reason = reasonStaleState
	}
	d.appendResolved(ctx, tool, token, decision, reason)
}

// appendReplaySucceeded emits the `approval_replay_succeeded` row
// after the replayer returned nil.
func (d *Dispatcher) appendReplaySucceeded(ctx context.Context, tool, token string) {
	if d.audit == nil {
		return
	}
	_, _ = d.audit.Append(ctx, keeperslog.Event{
		EventType: auditEventReplaySucceeded,
		Payload: map[string]any{
			payloadKeyToolName:      tool,
			payloadKeyApprovalToken: token,
		},
	})
}

// appendReplayFailed emits the `approval_replay_failed` row carrying
// the Go type of the underlying error in `error_class`. The error
// VALUE is NEVER written to the audit row — mirrors the M3.4.b /
// M2b.7 redaction discipline.
func (d *Dispatcher) appendReplayFailed(ctx context.Context, tool, token string, err error) {
	if d.audit == nil {
		return
	}
	_, _ = d.audit.Append(ctx, keeperslog.Event{
		EventType: auditEventReplayFailed,
		Payload: map[string]any{
			payloadKeyToolName:      tool,
			payloadKeyApprovalToken: token,
			payloadKeyErrorClass:    classifyError(err),
		},
	})
}

// classifyError extracts a stable string suitable for the
// `error_class` audit slot. Mirrors the M6.1.b / M6.2.x helper of
// the same name (defined in spawn/slack_app.go) — duplicated here
// so the dispatcher has no dependency cycle with spawn-internal
// helpers.
func classifyError(err error) string {
	if err == nil {
		return ""
	}
	cur := err
	for {
		next := errors.Unwrap(cur)
		if next == nil {
			return fmt.Sprintf("%T", cur)
		}
		cur = next
	}
}
