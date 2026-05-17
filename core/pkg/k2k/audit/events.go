package audit

// The six closed-set K2K event-type constants the M1.4 ROADMAP AC
// pins. Hoisted to typed constants so the M1.4 lifecycle/message
// emitters (this package), the M1.5 budget enforcer, and the M1.6
// escalation saga share one source of truth. A future audit
// subscriber querying `keepers_log.event_type IN (...)` can index
// against this closed set without parsing free-form strings.
const (
	// EventConversationOpened is emitted by [Writer.EmitConversationOpened]
	// after [k2k.Lifecycle.Open] successfully mints a row + Slack channel
	// and binds the channel id onto the persisted state. Payload carries
	// the conversation id, organization id, participants (ids only, no
	// roles / display names), subject, correlation id (when present),
	// slack channel id, and the `opened_at` timestamp the repository
	// stamped.
	EventConversationOpened = "k2k_conversation_opened"

	// EventConversationClosed is emitted by [Writer.EmitConversationClosed]
	// after [k2k.Lifecycle.Close] successfully archives the row. Payload
	// carries the conversation id, organization id, close_reason, and the
	// `closed_at` timestamp the repository stamped. The `close_summary`
	// is NOT carried in the audit payload — it is operator-supplied free
	// text persisted on the row; the audit subscriber joins on the row
	// when summary content is required.
	EventConversationClosed = "k2k_conversation_closed"

	// EventMessageSent is emitted by [Writer.EmitMessageSent] after a
	// `request`-direction or `reply`-direction [k2k.Message] is
	// persisted. Payload carries the message id, conversation id,
	// organization id, sender watchkeeper id, direction (request|reply),
	// and the `created_at` timestamp the repository stamped. The body
	// bytes are NEVER carried (PII discipline — the persisted message
	// row is the source of truth).
	EventMessageSent = "k2k_message_sent"

	// EventMessageReceived is emitted by [Writer.EmitMessageReceived]
	// for the recipient side of a `reply`-direction message — the
	// original requester observes the reply via [k2k.Repository.WaitForReply]
	// and the peer-tool layer emits this audit row alongside the
	// sender's [EventMessageSent]. Payload mirrors [EventMessageSent]'s
	// shape with an additional `recipient_watchkeeper_id` field so the
	// audit subscriber can correlate the two halves of the request-reply
	// round-trip.
	EventMessageReceived = "k2k_message_received"

	// EventOverBudget is emitted by the M1.5 budget enforcement seam
	// when [k2k.Repository.IncTokens] crosses the conversation's
	// configured budget. M1.4 ships the constant; M1.5 ships the
	// emission helper alongside its enforcement code.
	EventOverBudget = "k2k_over_budget"

	// EventEscalated is emitted by the M1.6 escalation saga when a peer
	// timeout or budget overage escalates to a human lead (or, on lead
	// unresponsive, to the Watchmaster). M1.4 ships the constant; M1.6
	// ships the emission helper alongside the saga.
	EventEscalated = "k2k_escalated"
)

// EventTypes returns a defensive copy of the six closed-set K2K event
// type strings. Hoisted so a future audit subscriber, a runtime
// diagnostic, or an M9.4.a tool-manifest validator can iterate over
// the taxonomy without re-declaring it. The returned slice is freshly
// allocated; mutating it does not affect callers.
func EventTypes() []string {
	return []string{
		EventConversationOpened,
		EventConversationClosed,
		EventMessageSent,
		EventMessageReceived,
		EventOverBudget,
		EventEscalated,
	}
}
