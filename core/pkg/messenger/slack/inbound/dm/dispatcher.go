package dm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/cards"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/inbound"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/inbound/intent"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// keepers_log event_type vocabulary the DM router emits onto the
// audit chain. Pinned per AC4 of M6.3.c TASK. Project convention:
// snake_case past-tense verbs prefixed with the surface owner
// (`slack_dm`).
const (
	auditEventReceived           = "slack_dm_received"
	auditEventDispatchedReadOnly = "slack_dm_dispatched_read_only"
	auditEventDispatchedBump     = "slack_dm_dispatched_manifest_bump"
	auditEventUnknownIntent      = "slack_dm_unknown_intent"
	auditEventFailed             = "slack_dm_failed"
)

// reason vocabulary on `slack_dm_failed`. Closed set so a downstream
// Recall query can group failures without parsing free-form strings.
const (
	reasonReadToolError  = "read_tool_error"
	reasonProposeError   = "propose_spawn_error"
	reasonDAOInsertError = "dao_insert_error"
	reasonOutboundError  = "outbound_error"
	reasonRenderError    = "render_error"
)

// payload key vocabulary on the audit rows. Snake_case, hoisted so
// the test assertions and the production builders share names.
const (
	payloadKeyChannelID  = "channel_id"
	payloadKeyIntent     = "intent"
	payloadKeyToolName   = "tool_name"
	payloadKeyReason     = "reason"
	payloadKeyErrorClass = "error_class"
)

// Inner-event types the dispatcher routes on. Slack delivers DMs as
// `event.type == "message"` with `channel_type == "im"`. Other inner
// types (`app_home_opened`, `reaction_added`, etc.) are ACKed silently
// — M6.3.c does not own them.
const (
	innerEventTypeMessage = "message"
	channelTypeIM         = "im"
)

// helpDMText is the text body posted on `IntentUnknown`. The render
// is deliberately compact and lists every supported intent so the
// admin can correct course on the next DM.
const helpDMText = "I didn't understand that. Try one of:\n" +
	"• `what's running?`\n" +
	"• `show costs`\n" +
	"• `health check`\n" +
	"• `propose a Coordinator for the backend team`"

// AuditAppender is the minimal subset of [keeperslog.Writer] the
// dispatcher consumes. Mirrors the M6.3.a/b dispatcher pattern.
type AuditAppender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// Compile-time assertion: production [*keeperslog.Writer] satisfies
// [AuditAppender].
var _ AuditAppender = (*keeperslog.Writer)(nil)

// ReadToolDispatcher is the seam the dispatcher consults on the
// read-only branches. Tool-agnostic on the same axis as M6.3.b's
// [approval.Replayer]: hand it a typed intent name + the raw text
// (for downstream extraction of optional narrowing params) and a
// pre-resolved [spawn.Claim], get back a Slack-block payload + plain
// text fallback to post.
//
// Production wiring composes the M6.2.a Watchmaster read functions
// (ListWatchkeepers / ReportCost / ReportHealth) behind this seam.
// Lives behind an interface so unit tests substitute a fake without
// standing up a keepclient stack.
type ReadToolDispatcher interface {
	// Dispatch invokes the read-only tool matching `intent`. Returns
	// the rendered block payload + a plain-text fallback the outbound
	// DM carries. A non-nil error surfaces on the audit chain as
	// `slack_dm_failed` with `reason=read_tool_error` and the
	// dispatcher posts an apology DM.
	Dispatch(ctx context.Context, in ReadToolRequest) (ReadToolResponse, error)
}

// ReadToolRequest is the input shape for [ReadToolDispatcher.Dispatch].
type ReadToolRequest struct {
	// Intent is the closed-set vocabulary value the parser emitted.
	// One of [intent.IntentReadList], [intent.IntentReportCost],
	// [intent.IntentReportHealth].
	Intent intent.Intent

	// Claim is the resolved auth tuple for the calling admin. The DM
	// router's caller (the wiring layer in main) materialises a
	// Watchmaster claim before invoking the dispatcher; the dispatcher
	// itself never reads secrets.
	Claim spawn.Claim
}

// ReadToolResponse is the output shape from [ReadToolDispatcher.Dispatch].
type ReadToolResponse struct {
	// Blocks is the Slack Block Kit payload to post. May be nil for
	// text-only responses.
	Blocks []cards.Block

	// FallbackText is the plain-text body posted alongside (or
	// instead of) Blocks. Required.
	FallbackText string
}

// ProposeSpawnInvoker is the seam the dispatcher consults on the
// `propose_spawn` branch. Returns the projection needed to render
// the approval card AND the raw params snapshot the
// [spawn.PendingApprovalDAO] persists for the M6.3.b replayer.
//
// Production wiring composes [spawn.ProposeSpawn] behind this seam,
// minting a fresh approval token + agent UUID before the underlying
// keep call.
type ProposeSpawnInvoker interface {
	// Invoke kicks off a propose-spawn draft. Returns the rendered
	// card-input projection plus the params JSON for DAO persistence.
	// A non-nil error surfaces as `slack_dm_failed` with
	// `reason=propose_spawn_error`.
	Invoke(ctx context.Context, in ProposeSpawnRequest) (ProposeSpawnResponse, error)
}

// ProposeSpawnRequest is the input shape for [ProposeSpawnInvoker.Invoke].
type ProposeSpawnRequest struct {
	// Team is the parsed team token from the DM phrasing. Empty if
	// not embedded in the DM (the invoker fills a default or fails).
	Team string

	// Role is the parsed role token. Empty if not embedded.
	Role string

	// Claim is the resolved auth tuple for the calling admin.
	Claim spawn.Claim
}

// ProposeSpawnResponse is the output shape from [ProposeSpawnInvoker.Invoke].
type ProposeSpawnResponse struct {
	// CardInput is the projection [cards.RenderProposeSpawn] consumes.
	// Must carry a non-empty AgentID + ApprovalToken on success.
	CardInput cards.ProposeSpawnCardInput

	// ParamsJSON is the raw request snapshot the DAO persists for
	// the M6.3.b replayer. Required.
	ParamsJSON json.RawMessage
}

// Outbound is the seam the dispatcher consults to post a DM back to
// the admin. The single method intentionally mirrors the
// `*slack.Client.SendMessage` cross-section so production wiring is a
// thin adapter.
//
// `channelID` is the IM channel id — the dispatcher passes through
// the `event.channel` value Slack delivered, which doubles as the
// DM channel for inbound DMs.
type Outbound interface {
	// Post sends a message body (blocks + plain-text fallback) to
	// `channelID`. Returns nil on success or a wrapped error that
	// surfaces as `slack_dm_failed` with `reason=outbound_error`.
	Post(ctx context.Context, channelID string, blocks []cards.Block, fallbackText string) error
}

// Dispatcher is the M6.3.c implementation of
// [inbound.EventDispatcher]. Construct via [New]; the zero value is
// NOT usable.
//
// All fields are immutable after construction; the dispatcher is
// safe for concurrent use across goroutines.
type Dispatcher struct {
	parser    intent.Parser
	readDisp  ReadToolDispatcher
	proposer  ProposeSpawnInvoker
	dao       spawn.PendingApprovalDAO
	outbound  Outbound
	audit     AuditAppender
	claim     spawn.Claim
	tokenMint func() string
}

// Compile-time assertion: [*Dispatcher] satisfies
// [inbound.EventDispatcher].
var _ inbound.EventDispatcher = (*Dispatcher)(nil)

// Option configures a [Dispatcher] at construction time.
type Option func(*Dispatcher)

// WithAuditAppender wires the audit sink. A nil appender is ignored;
// dispatchers without an explicit appender skip audit emission entirely
// (test-only mode).
func WithAuditAppender(a AuditAppender) Option {
	return func(d *Dispatcher) {
		if a != nil {
			d.audit = a
		}
	}
}

// WithClaim wires the resolved auth tuple the dispatcher threads into
// every tool invocation. Defaults to the zero-value Claim — production
// callers MUST wire one (the M6.2.x tools fail closed with
// [spawn.ErrInvalidClaim] when OrganizationID is empty).
func WithClaim(c spawn.Claim) Option {
	return func(d *Dispatcher) {
		d.claim = c
	}
}

// WithTokenMint overrides the approval-token generator. Defaults to
// UUID v7. Tests pin a deterministic generator for assertions.
func WithTokenMint(fn func() string) Option {
	return func(d *Dispatcher) {
		if fn != nil {
			d.tokenMint = fn
		}
	}
}

// New constructs a [Dispatcher] backed by the supplied seams. Parser,
// ReadToolDispatcher, ProposeSpawnInvoker, DAO, and Outbound are
// required; nil values panic with a clear message.
func New(
	parser intent.Parser,
	readDisp ReadToolDispatcher,
	proposer ProposeSpawnInvoker,
	dao spawn.PendingApprovalDAO,
	outbound Outbound,
	opts ...Option,
) *Dispatcher {
	if parser == nil {
		panic("dm: New: Parser must not be nil")
	}
	if readDisp == nil {
		panic("dm: New: ReadToolDispatcher must not be nil")
	}
	if proposer == nil {
		panic("dm: New: ProposeSpawnInvoker must not be nil")
	}
	if dao == nil {
		panic("dm: New: PendingApprovalDAO must not be nil")
	}
	if outbound == nil {
		panic("dm: New: Outbound must not be nil")
	}
	d := &Dispatcher{
		parser:    parser,
		readDisp:  readDisp,
		proposer:  proposer,
		dao:       dao,
		outbound:  outbound,
		tokenMint: defaultTokenMint,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// messageEvent is the subset of the Slack `message` inner event the
// dispatcher decodes. Slack's full payload carries ~30 fields; we
// only consume the channel id, channel type, user id, optional bot id,
// optional subtype, and text.
type messageEvent struct {
	Type        string `json:"type"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	User        string `json:"user"`
	BotID       string `json:"bot_id"`
	Subtype     string `json:"subtype"`
	Text        string `json:"text"`
}

// DispatchEvent satisfies [inbound.EventDispatcher]. See package doc
// for the resolution order.
func (d *Dispatcher) DispatchEvent(ctx context.Context, ev inbound.Event) error {
	if ev.Type != innerEventTypeMessage {
		// Foreign inner-event types are ACKed silently. M6.3.c only
		// owns the message-DM path.
		return nil
	}
	if len(ev.Inner) == 0 {
		return nil
	}

	var m messageEvent
	if err := json.Unmarshal(ev.Inner, &m); err != nil {
		// A signature-valid envelope that fails to decode is the
		// inbound handler's bug, not ours; return without an audit
		// row so the operator sees this only via the inbound handler's
		// `slack_webhook_received` row.
		return fmt.Errorf("dm: decode inner message: %w", err)
	}

	// Filter (per AC2): non-DM channels, bot messages, edits, and
	// empty text all bypass the audit chain entirely. AC6 pins
	// 0 audit rows on each.
	if m.ChannelType != channelTypeIM {
		return nil
	}
	if m.Subtype != "" || m.BotID != "" || m.User == "" {
		return nil
	}
	if m.Text == "" {
		return nil
	}

	// Step 1 audit row — `received`. NEVER carries the message text.
	d.appendReceived(ctx, m.Channel)

	// Step 2 — classify.
	parsed := d.parser.Parse(m.Text)
	switch parsed.Intent {
	case intent.IntentReadList, intent.IntentReportCost, intent.IntentReportHealth:
		return d.handleReadOnly(ctx, m.Channel, parsed.Intent)
	case intent.IntentProposeSpawn:
		return d.handlePropose(ctx, m.Channel, parsed)
	default:
		return d.handleUnknown(ctx, m.Channel)
	}
}

// handleReadOnly drives the read-only branch: invoke the read tool,
// post the rendered DM, emit the closing audit row.
func (d *Dispatcher) handleReadOnly(ctx context.Context, channelID string, in intent.Intent) error {
	resp, err := d.readDisp.Dispatch(ctx, ReadToolRequest{Intent: in, Claim: d.claim})
	if err != nil {
		d.appendFailed(ctx, channelID, string(in), reasonReadToolError, err)
		// Best-effort apology DM; ignore secondary outbound failure.
		_ = d.outbound.Post(ctx, channelID, nil, apologyText())
		return fmt.Errorf("dm: read tool: %w", err)
	}
	if err := d.outbound.Post(ctx, channelID, resp.Blocks, resp.FallbackText); err != nil {
		d.appendFailed(ctx, channelID, string(in), reasonOutboundError, err)
		return fmt.Errorf("dm: outbound: %w", err)
	}
	d.appendDispatchedReadOnly(ctx, channelID, in)
	return nil
}

// handlePropose drives the propose-spawn branch: invoke the proposer,
// persist the pending row, render the approval card, post the DM,
// emit the closing audit row.
func (d *Dispatcher) handlePropose(ctx context.Context, channelID string, parsed intent.Result) error {
	token := d.tokenMint()
	in := ProposeSpawnRequest{Team: parsed.Team, Role: parsed.Role, Claim: d.claim}

	resp, err := d.proposer.Invoke(ctx, in)
	if err != nil {
		d.appendFailed(ctx, channelID, string(intent.IntentProposeSpawn), reasonProposeError, err)
		_ = d.outbound.Post(ctx, channelID, nil, apologyText())
		return fmt.Errorf("dm: propose: %w", err)
	}

	// Plumb the freshly-minted token onto the card input so the
	// rendered action_id round-trips through the M6.3.b dispatcher.
	resp.CardInput.ApprovalToken = token

	if err := d.dao.Insert(ctx, token, spawn.PendingApprovalToolProposeSpawn, resp.ParamsJSON); err != nil {
		d.appendFailed(ctx, channelID, string(intent.IntentProposeSpawn), reasonDAOInsertError, err)
		_ = d.outbound.Post(ctx, channelID, nil, apologyText())
		return fmt.Errorf("dm: dao insert: %w", err)
	}

	blocks, actionID := cards.RenderProposeSpawn(resp.CardInput)
	if actionID == "" {
		// Should not happen — invoker contract requires non-empty
		// AgentID. Emit failed audit + apology so the operator notices.
		err := errors.New("empty card render")
		d.appendFailed(ctx, channelID, string(intent.IntentProposeSpawn), reasonRenderError, err)
		_ = d.outbound.Post(ctx, channelID, nil, apologyText())
		return fmt.Errorf("dm: render: %w", err)
	}

	if err := d.outbound.Post(ctx, channelID, blocks, "Approval required for new Watchkeeper spawn"); err != nil {
		d.appendFailed(ctx, channelID, string(intent.IntentProposeSpawn), reasonOutboundError, err)
		return fmt.Errorf("dm: outbound: %w", err)
	}

	d.appendDispatchedBump(ctx, channelID)
	return nil
}

// handleUnknown drives the fall-through branch: post the help DM and
// emit the unknown-intent audit row. NEVER calls a tool.
func (d *Dispatcher) handleUnknown(ctx context.Context, channelID string) error {
	if err := d.outbound.Post(ctx, channelID, nil, helpDMText); err != nil {
		d.appendFailed(ctx, channelID, string(intent.IntentUnknown), reasonOutboundError, err)
		return fmt.Errorf("dm: outbound help: %w", err)
	}
	d.appendUnknown(ctx, channelID)
	return nil
}

// apologyText is the plain-text body posted on tool / DAO / outbound
// failures. Deliberately content-free: NO error string, NO request
// echo. Mirrors the M6.3.b PII discipline.
func apologyText() string {
	return "Sorry — I couldn't complete that request. The error has been logged."
}

// defaultTokenMint mints a fresh UUID v7 string for every
// propose-spawn approval. Falls back to a synthetic prefix when the
// uuid package errors (extremely rare; the call has no real failure
// mode in v7) so the dispatcher never panics on token allocation.
func defaultTokenMint() string {
	id, err := uuid.NewV7()
	if err != nil {
		return "tok-error"
	}
	return id.String()
}

// ── audit-row builders ───────────────────────────────────────────────

func (d *Dispatcher) appendReceived(ctx context.Context, channelID string) {
	if d.audit == nil {
		return
	}
	_, _ = d.audit.Append(ctx, keeperslog.Event{
		EventType: auditEventReceived,
		Payload: map[string]any{
			payloadKeyChannelID: channelID,
		},
	})
}

func (d *Dispatcher) appendDispatchedReadOnly(ctx context.Context, channelID string, in intent.Intent) {
	if d.audit == nil {
		return
	}
	_, _ = d.audit.Append(ctx, keeperslog.Event{
		EventType: auditEventDispatchedReadOnly,
		Payload: map[string]any{
			payloadKeyChannelID: channelID,
			payloadKeyIntent:    string(in),
			payloadKeyToolName:  string(in),
		},
	})
}

func (d *Dispatcher) appendDispatchedBump(ctx context.Context, channelID string) {
	if d.audit == nil {
		return
	}
	_, _ = d.audit.Append(ctx, keeperslog.Event{
		EventType: auditEventDispatchedBump,
		Payload: map[string]any{
			payloadKeyChannelID: channelID,
			payloadKeyIntent:    string(intent.IntentProposeSpawn),
			payloadKeyToolName:  spawn.PendingApprovalToolProposeSpawn,
		},
	})
}

func (d *Dispatcher) appendUnknown(ctx context.Context, channelID string) {
	if d.audit == nil {
		return
	}
	_, _ = d.audit.Append(ctx, keeperslog.Event{
		EventType: auditEventUnknownIntent,
		Payload: map[string]any{
			payloadKeyChannelID: channelID,
			payloadKeyIntent:    string(intent.IntentUnknown),
		},
	})
}

func (d *Dispatcher) appendFailed(ctx context.Context, channelID, toolName, reason string, err error) {
	if d.audit == nil {
		return
	}
	payload := map[string]any{
		payloadKeyChannelID:  channelID,
		payloadKeyReason:     reason,
		payloadKeyErrorClass: classifyError(err),
	}
	if toolName != "" {
		payload[payloadKeyToolName] = toolName
	}
	_, _ = d.audit.Append(ctx, keeperslog.Event{EventType: auditEventFailed, Payload: payload})
}

// classifyError extracts a stable string suitable for the
// `error_class` audit slot. Mirrors the M6.3.b helper of the same
// name (defined in approval/dispatcher.go) — duplicated here so the
// dispatcher has no dependency cycle with approval-internal helpers.
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
