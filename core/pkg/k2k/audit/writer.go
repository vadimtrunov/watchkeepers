package audit

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
)

// Payload keys for the K2K event taxonomy. Hoisted to constants so
// the emitter helpers + a future audit subscriber join on stable keys
// rather than free-form strings. Mirrors the cost-recorder's
// `payloadKey*` discipline in `core/pkg/llm/cost/cost.go`.
const (
	payloadKeyConversationID         = "conversation_id"
	payloadKeyOrganizationID         = "organization_id"
	payloadKeyParticipants           = "participants"
	payloadKeySubject                = "subject"
	payloadKeyCorrelationID          = "correlation_id"
	payloadKeySlackChannelID         = "slack_channel_id"
	payloadKeyOpenedAt               = "opened_at"
	payloadKeyClosedAt               = "closed_at"
	payloadKeyCloseReason            = "close_reason"
	payloadKeyMessageID              = "message_id"
	payloadKeySenderWatchkeeperID    = "sender_watchkeeper_id"
	payloadKeyRecipientWatchkeeperID = "recipient_watchkeeper_id"
	payloadKeyDirection              = "direction"
	payloadKeyCreatedAt              = "created_at"
)

// Appender is the minimal subset of [*keeperslog.Writer] the audit
// emitter consumes — only the [keeperslog.Writer.Append] method is
// touched. Defined as an interface in this package so unit tests can
// substitute a hand-rolled fake that asserts the audit-row contract
// directly, and so production code never depends on the concrete
// [*keeperslog.Writer] type at all. Mirrors the
// [messenger.AuditAppender] + [llm/cost.Appender] +
// [keeperslog.LocalKeepClient] import-cycle-break pattern documented
// in `docs/LESSONS.md`.
//
// [*keeperslog.Writer] satisfies this interface as-is; a compile-time
// assertion lives in `writer_test.go`.
type Appender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// Emitter is the consumer-facing seam the K2K lifecycle layer + the
// peer.* tool layer wire to keep their own files free of
// `keeperslog.` imports (M1.3.a / .b / .c / .d source-grep AC). The
// interface is intentionally narrow — one method per closed-set event
// type — so a fake in the consumer's test suite asserts the audit
// contract directly without implementing every keeperslog detail.
//
// Every method is best-effort + non-blocking from the caller's
// perspective: a non-nil error indicates the audit row failed to
// persist, but the calling lifecycle / tool layer SHOULD NOT
// short-circuit its own success on an audit failure — audit emission
// is an observability concern, not a correctness gate. Logging the
// error at the caller is sufficient; the keeperslog writer's
// [keeperslog.WithLogger] sink already records the failure mode.
//
// Returned event id is the underlying keeperslog row id (UUID
// string); callers may stash it for cross-correlation but the
// contract does not pin its format beyond non-empty on success.
//
// Implementations MUST be safe for concurrent use across goroutines.
// The production [Writer] is goroutine-safe by construction
// (composes the already-goroutine-safe [keeperslog.Writer]).
type Emitter interface {
	// EmitConversationOpened records a [EventConversationOpened] audit
	// row. Called by [k2k.Lifecycle.Open] after the row + Slack
	// channel are bound. Returns the keeperslog row id on success.
	EmitConversationOpened(ctx context.Context, evt ConversationOpenedEvent) (string, error)

	// EmitConversationClosed records a [EventConversationClosed] audit
	// row. Called by [k2k.Lifecycle.Close] after the repository
	// transitions the row to archived. Returns the keeperslog row id
	// on success.
	EmitConversationClosed(ctx context.Context, evt ConversationClosedEvent) (string, error)

	// EmitMessageSent records a [EventMessageSent] audit row. Called
	// by [peer.Tool.Ask] (request-direction) and [peer.Tool.Reply]
	// (reply-direction) after the message row is appended.
	EmitMessageSent(ctx context.Context, evt MessageSentEvent) (string, error)

	// EmitMessageReceived records a [EventMessageReceived] audit row.
	// Called by [peer.Tool.Reply] for the recipient side of a reply
	// (the original requester observes the reply); pairs with
	// [EmitMessageSent] so a subscriber can join the two halves of a
	// round-trip on `conversation_id` + `message_id`.
	EmitMessageReceived(ctx context.Context, evt MessageReceivedEvent) (string, error)
}

// ConversationOpenedEvent is the closed-set input shape
// [Writer.EmitConversationOpened] accepts. Hoisted to a struct so a
// future addition (e.g. an explicit `WatchOrderID` field for the
// M3.5.a Watch-Order correlation) lands as a new field rather than a
// breaking signature change. Mirrors the
// [llm/cost.LoggingProvider]'s closed-set payload discipline.
type ConversationOpenedEvent struct {
	// ConversationID is the persisted [k2k.Conversation.ID]. Required
	// (non-zero); [Writer.EmitConversationOpened] surfaces
	// [ErrInvalidEvent] otherwise.
	ConversationID uuid.UUID

	// OrganizationID is the persisted [k2k.Conversation.OrganizationID].
	// Required (non-zero).
	OrganizationID uuid.UUID

	// Participants is the persisted [k2k.Conversation.Participants]
	// slice (bot ids only). Defensively deep-copied before being
	// stamped onto the audit payload so caller-side mutation cannot
	// bleed.
	Participants []string

	// Subject is the operator-facing free-text
	// [k2k.Conversation.Subject]. Forwarded verbatim.
	Subject string

	// CorrelationID is the persisted [k2k.Conversation.CorrelationID].
	// `uuid.Nil` is allowed (the field is omitted from the payload
	// when unset); the keeperslog correlation_id column is populated
	// independently via [keeperslog.ContextWithCorrelationID].
	CorrelationID uuid.UUID

	// SlackChannelID is the persisted [k2k.Conversation.SlackChannelID]
	// after the M1.1.c lifecycle wiring binds the Slack channel id.
	// Required (non-empty after trim); a row + channel bound BEFORE
	// the audit emit is the lifecycle layer's contract.
	SlackChannelID string

	// OpenedAt is the persisted [k2k.Conversation.OpenedAt]. Required
	// (non-zero); the emitter surfaces [ErrInvalidEvent] otherwise.
	OpenedAt time.Time
}

// ConversationClosedEvent is the closed-set input shape
// [Writer.EmitConversationClosed] accepts.
type ConversationClosedEvent struct {
	// ConversationID is the persisted [k2k.Conversation.ID]. Required.
	ConversationID uuid.UUID

	// OrganizationID is the persisted [k2k.Conversation.OrganizationID].
	// Required.
	OrganizationID uuid.UUID

	// CloseReason is the persisted [k2k.Conversation.CloseReason] —
	// the stable closed-set code the lifecycle layer wrote (e.g.
	// `peer.close` from [peer.CloseLifecycleReason], or the M1.6
	// escalation auto-archive sentinel). Forwarded verbatim. Empty
	// string is allowed for lifecycle paths that do not supply a
	// reason.
	CloseReason string

	// ClosedAt is the persisted [k2k.Conversation.ClosedAt]. Required
	// (non-zero).
	ClosedAt time.Time
}

// MessageSentEvent is the closed-set input shape
// [Writer.EmitMessageSent] accepts. The body bytes are NEVER carried
// in the payload — the persisted [k2k.Message.Body] is the source of
// truth (PII discipline).
type MessageSentEvent struct {
	// MessageID is the persisted [k2k.Message.ID]. Required.
	MessageID uuid.UUID

	// ConversationID is the persisted [k2k.Message.ConversationID].
	// Required.
	ConversationID uuid.UUID

	// OrganizationID is the persisted [k2k.Message.OrganizationID].
	// Required.
	OrganizationID uuid.UUID

	// SenderWatchkeeperID is the persisted
	// [k2k.Message.SenderWatchkeeperID]. Required (non-empty after
	// trim).
	SenderWatchkeeperID string

	// Direction is the persisted [k2k.Message.Direction] — one of
	// `request` / `reply`. Required (non-empty); the emitter does not
	// re-validate against the closed [k2k.MessageDirection] set —
	// callers are expected to forward whatever the storage layer
	// returned.
	Direction string

	// CreatedAt is the persisted [k2k.Message.CreatedAt]. Required
	// (non-zero).
	CreatedAt time.Time
}

// MessageReceivedEvent is the closed-set input shape
// [Writer.EmitMessageReceived] accepts. Adds
// [RecipientWatchkeeperID] vs [MessageSentEvent] so the subscriber
// can correlate the two halves of a request-reply round-trip.
type MessageReceivedEvent struct {
	// MessageID is the persisted [k2k.Message.ID]. Required.
	MessageID uuid.UUID

	// ConversationID is the persisted [k2k.Message.ConversationID].
	// Required.
	ConversationID uuid.UUID

	// OrganizationID is the persisted [k2k.Message.OrganizationID].
	// Required.
	OrganizationID uuid.UUID

	// SenderWatchkeeperID is the original sender of the message —
	// load-bearing for the subscriber's request-reply correlation.
	// Required (non-empty after trim).
	SenderWatchkeeperID string

	// RecipientWatchkeeperID is the acting watchkeeper observing the
	// received message. Required (non-empty after trim).
	RecipientWatchkeeperID string

	// Direction mirrors [MessageSentEvent.Direction].
	Direction string

	// CreatedAt is the persisted [k2k.Message.CreatedAt]. Required
	// (non-zero).
	CreatedAt time.Time
}

// Writer is the production [Emitter] implementation backed by a
// [keeperslog.Writer]-shaped [Appender]. Construct via [NewWriter];
// the zero value is NOT usable — callers must always go through the
// constructor so the dependency invariants are enforced at
// construction time. Mirrors the saga-step + [k2k.NewLifecycle] +
// [llm/cost.NewLoggingProvider] discipline.
//
// Concurrency: safe for concurrent use after construction. Holds only
// immutable configuration; per-call state lives on the goroutine
// stack.
type Writer struct {
	appender Appender
}

// NewWriter constructs a [Writer] backed by the supplied [Appender].
// Panics on a nil appender — a Writer with no sink cannot do anything
// useful, and silently no-oping every call would mask the very bug
// this package exists to prevent. Matches the panic discipline of
// [keeperslog.New], [lifecycle.New], [cron.New], and
// [llm/cost.NewLoggingProvider].
func NewWriter(appender Appender) *Writer {
	if appender == nil {
		panic("audit: NewWriter: appender must not be nil")
	}
	return &Writer{appender: appender}
}

// Compile-time assertion: [*Writer] satisfies [Emitter].
var _ Emitter = (*Writer)(nil)

// EmitConversationOpened records a [EventConversationOpened] audit
// row. Validation runs BEFORE the appender round-trip; a malformed
// event returns [ErrInvalidEvent] without touching keeperslog.
func (w *Writer) EmitConversationOpened(ctx context.Context, evt ConversationOpenedEvent) (string, error) {
	if evt.ConversationID == uuid.Nil {
		return "", fmt.Errorf("%w: conversation_id must not be zero", ErrInvalidEvent)
	}
	if evt.OrganizationID == uuid.Nil {
		return "", fmt.Errorf("%w: organization_id must not be zero", ErrInvalidEvent)
	}
	if evt.SlackChannelID == "" {
		return "", fmt.Errorf("%w: slack_channel_id must not be empty", ErrInvalidEvent)
	}
	if evt.OpenedAt.IsZero() {
		return "", fmt.Errorf("%w: opened_at must not be zero", ErrInvalidEvent)
	}

	payload := map[string]any{
		payloadKeyConversationID: evt.ConversationID.String(),
		payloadKeyOrganizationID: evt.OrganizationID.String(),
		payloadKeyParticipants:   cloneStrings(evt.Participants),
		payloadKeySubject:        evt.Subject,
		payloadKeySlackChannelID: evt.SlackChannelID,
		payloadKeyOpenedAt:       evt.OpenedAt.UTC().Format(time.RFC3339Nano),
	}
	if evt.CorrelationID != uuid.Nil {
		payload[payloadKeyCorrelationID] = evt.CorrelationID.String()
	}

	return w.appender.Append(ctx, keeperslog.Event{
		EventType: EventConversationOpened,
		Payload:   payload,
	})
}

// EmitConversationClosed records a [EventConversationClosed] audit
// row.
func (w *Writer) EmitConversationClosed(ctx context.Context, evt ConversationClosedEvent) (string, error) {
	if evt.ConversationID == uuid.Nil {
		return "", fmt.Errorf("%w: conversation_id must not be zero", ErrInvalidEvent)
	}
	if evt.OrganizationID == uuid.Nil {
		return "", fmt.Errorf("%w: organization_id must not be zero", ErrInvalidEvent)
	}
	if evt.ClosedAt.IsZero() {
		return "", fmt.Errorf("%w: closed_at must not be zero", ErrInvalidEvent)
	}

	payload := map[string]any{
		payloadKeyConversationID: evt.ConversationID.String(),
		payloadKeyOrganizationID: evt.OrganizationID.String(),
		payloadKeyCloseReason:    evt.CloseReason,
		payloadKeyClosedAt:       evt.ClosedAt.UTC().Format(time.RFC3339Nano),
	}

	return w.appender.Append(ctx, keeperslog.Event{
		EventType: EventConversationClosed,
		Payload:   payload,
	})
}

// EmitMessageSent records a [EventMessageSent] audit row.
func (w *Writer) EmitMessageSent(ctx context.Context, evt MessageSentEvent) (string, error) {
	if err := validateMessageCore(evt.MessageID, evt.ConversationID, evt.OrganizationID, evt.SenderWatchkeeperID, evt.Direction, evt.CreatedAt); err != nil {
		return "", err
	}

	payload := map[string]any{
		payloadKeyMessageID:           evt.MessageID.String(),
		payloadKeyConversationID:      evt.ConversationID.String(),
		payloadKeyOrganizationID:      evt.OrganizationID.String(),
		payloadKeySenderWatchkeeperID: evt.SenderWatchkeeperID,
		payloadKeyDirection:           evt.Direction,
		payloadKeyCreatedAt:           evt.CreatedAt.UTC().Format(time.RFC3339Nano),
	}

	return w.appender.Append(ctx, keeperslog.Event{
		EventType: EventMessageSent,
		Payload:   payload,
	})
}

// EmitMessageReceived records a [EventMessageReceived] audit row.
func (w *Writer) EmitMessageReceived(ctx context.Context, evt MessageReceivedEvent) (string, error) {
	if err := validateMessageCore(evt.MessageID, evt.ConversationID, evt.OrganizationID, evt.SenderWatchkeeperID, evt.Direction, evt.CreatedAt); err != nil {
		return "", err
	}
	if evt.RecipientWatchkeeperID == "" {
		return "", fmt.Errorf("%w: recipient_watchkeeper_id must not be empty", ErrInvalidEvent)
	}

	payload := map[string]any{
		payloadKeyMessageID:              evt.MessageID.String(),
		payloadKeyConversationID:         evt.ConversationID.String(),
		payloadKeyOrganizationID:         evt.OrganizationID.String(),
		payloadKeySenderWatchkeeperID:    evt.SenderWatchkeeperID,
		payloadKeyRecipientWatchkeeperID: evt.RecipientWatchkeeperID,
		payloadKeyDirection:              evt.Direction,
		payloadKeyCreatedAt:              evt.CreatedAt.UTC().Format(time.RFC3339Nano),
	}

	return w.appender.Append(ctx, keeperslog.Event{
		EventType: EventMessageReceived,
		Payload:   payload,
	})
}

// validateMessageCore reports a wrapped [ErrInvalidEvent] for any
// zero-valued required field on a message-shaped event. Hoisted so
// the two message emitters share one validator + the test surface
// pins one shape.
func validateMessageCore(messageID, conversationID, organizationID uuid.UUID, senderID, direction string, createdAt time.Time) error {
	if messageID == uuid.Nil {
		return fmt.Errorf("%w: message_id must not be zero", ErrInvalidEvent)
	}
	if conversationID == uuid.Nil {
		return fmt.Errorf("%w: conversation_id must not be zero", ErrInvalidEvent)
	}
	if organizationID == uuid.Nil {
		return fmt.Errorf("%w: organization_id must not be zero", ErrInvalidEvent)
	}
	if senderID == "" {
		return fmt.Errorf("%w: sender_watchkeeper_id must not be empty", ErrInvalidEvent)
	}
	if direction == "" {
		return fmt.Errorf("%w: direction must not be empty", ErrInvalidEvent)
	}
	if createdAt.IsZero() {
		return fmt.Errorf("%w: created_at must not be zero", ErrInvalidEvent)
	}
	return nil
}

// cloneStrings returns a defensive deep-copy of `in`. Hoisted here to
// keep the writer helpers self-contained; mirrors the same helper in
// `core/pkg/k2k/conversation.go` and `core/pkg/peer/filter.go`.
func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
