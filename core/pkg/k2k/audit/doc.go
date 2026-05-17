// Package audit is the M1.4 K2K event-taxonomy emission seam. It owns
// the six standard event-type constants the Phase 2 ROADMAP M1.4 AC
// pins (`k2k_message_sent`, `k2k_message_received`,
// `k2k_conversation_opened`, `k2k_conversation_closed`,
// `k2k_over_budget`, `k2k_escalated`) and a thin [Emitter] interface
// the K2K lifecycle layer + the peer.* tool layer consume to keep
// their own files free of `keeperslog.` imports. The production
// [Writer] composes the closed-set payload shapes and forwards them
// to a [keeperslog.Writer]-shaped [Appender].
//
// # Why a dedicated package
//
// Two consumers (k2k.Lifecycle + peer.Tool) need to emit K2K-shaped
// audit rows; without a shared taxonomy package they would either
// duplicate the constants or import each other (creating a cycle).
// The seam also makes the source-grep AC sustainable: every peer.*
// file + the k2k.Lifecycle file ban `keeperslog.` and `.Append(`
// (M1.3.a/b/c/d source-grep tests) because audit emission MUST flow
// through the typed [Emitter] surface rather than ad-hoc keeperslog
// calls. This package is the single legitimate `keeperslog.` importer
// in the K2K + peer-tool surface.
//
// # Event taxonomy
//
// The six event types are closed-set constants. M1.4 ships emission
// helpers for the four lifecycle/message events
// ([Writer.EmitConversationOpened], [Writer.EmitConversationClosed],
// [Writer.EmitMessageSent], [Writer.EmitMessageReceived]); M1.5 owns
// the [EventOverBudget] emission (it lands alongside the budget
// enforcement seam) and M1.6 owns the [EventEscalated] emission (it
// lands alongside the escalation saga). All six constants are
// defined here so a future M1.5/M1.6 consumer joins the taxonomy
// rather than minting a parallel string.
//
// # Payload shape discipline
//
// Each emit helper builds a closed-set map of typed primitives —
// strings, uuid.UUID.String(), int64. No `interface{}` smuggling, no
// raw `[]byte` payloads (PII leaks happen there). The message bodies
// themselves are NEVER carried in the payload — the persisted
// [k2k.Message.Body] is the source of truth; the audit row records
// the message id + direction + the conversation id only. This
// preserves the M1.3.a / .b / .c / .d PII-discipline contract that
// "no payload bytes ever reach the returned error or any diagnostic
// surface".
//
// Out of scope
//
//   - Token-budget evaluation. [EventOverBudget] is a constant; M1.5
//     owns the budget-comparison code that decides when to emit.
//   - Escalation routing. [EventEscalated] is a constant; M1.6 owns
//     the timeout-and-escalate saga that emits it.
//   - Capability-token wiring. The audit emitter inherits the keep
//     HTTP layer's actor / scope from the [keeperslog.Appender] it
//     wraps; this package never sees a capability token directly.
package audit
