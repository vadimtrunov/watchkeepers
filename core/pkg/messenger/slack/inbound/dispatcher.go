package inbound

import (
	"context"
	"encoding/json"
)

// EventDispatcher is the seam the [http.Handler] returned from
// [NewHandler] consults whenever the Events API endpoint receives a
// `type=event_callback` envelope. M6.3.a ships only a skeleton
// dispatcher (M6.3.c will wire intent recognition for the DM path);
// implementors return nil to ACK and rely on out-of-band correlation
// (request-id stamped on the audit row) for follow-up traces.
//
// The dispatcher receives the DECODED [Event] envelope rather than the
// raw bytes so the M6.3.b–d follow-ups can extract the event_type +
// inner payload without re-parsing. The handler ACKs HTTP 200 to Slack
// regardless of the dispatcher's return value — a synchronous error
// from the dispatcher would push past Slack's 3-second budget; the
// handler logs the failure on the audit chain and the operator runs
// the recovery path async.
type EventDispatcher interface {
	// DispatchEvent is invoked for every successfully-decoded
	// `event_callback` envelope. Implementors MUST treat the call as
	// fire-and-forget for ACK purposes — the handler returns 200 to
	// Slack regardless of the err. A returned error surfaces on the
	// handler's audit row so M6.3.b–d wiring can correlate failures.
	//
	// The ctx carries the request's correlation id (via
	// [keeperslog.ContextWithCorrelationID]); implementors may
	// derive child contexts but should not block on long-running
	// work in this call — Slack's 3-second budget is shared between
	// signature verification, decoding, dispatch, and the response
	// write.
	DispatchEvent(ctx context.Context, ev Event) error
}

// InteractionDispatcher is the seam the handler consults whenever the
// Interactivity endpoint receives a payload. Mirrors [EventDispatcher]
// for the Interactivity API surface (block_actions, view_submission,
// view_closed, message_action, …). M6.3.a ships only a skeleton; the
// M6.3.c approval-card wiring lands the production implementation.
type InteractionDispatcher interface {
	// DispatchInteraction is invoked for every successfully-decoded
	// Interactivity payload. The contract mirrors
	// [EventDispatcher.DispatchEvent]: fire-and-forget for ACK
	// purposes, errors surface on the audit chain.
	DispatchInteraction(ctx context.Context, p Interaction) error
}

// Event is the decoded `event_callback` envelope passed to
// [EventDispatcher.DispatchEvent]. Captures only the M6.3.a-relevant
// fields; M6.3.b/c will extend this struct with intent-specific shapes
// or carry the raw `event` payload through [Event.Inner] for callers
// that need the full Slack envelope.
type Event struct {
	// TeamID is the Slack workspace id (`team_id`) the event
	// originated in. Routed to the per-tenant dispatcher in M6.3.b.
	TeamID string `json:"team_id"`

	// APIAppID is the Slack app id (`api_app_id`) the event was
	// delivered to. Distinct from TeamID — multi-tenant deployments
	// route on the (TeamID, APIAppID) tuple.
	APIAppID string `json:"api_app_id"`

	// EventID is Slack's per-event identifier (`event_id`). The
	// future dedup-cache (deferred from M6.3.a) will key on this
	// field; the M6.3.a audit row already carries it for downstream
	// correlation.
	EventID string `json:"event_id"`

	// EventTime is Slack's per-event Unix timestamp
	// (`event_time`). May be zero on test fixtures.
	EventTime int64 `json:"event_time"`

	// Type is the inner-event `event.type` string (e.g.,
	// `message`, `app_home_opened`, `reaction_added`). Hoisted for
	// dispatcher convenience; the full inner JSON rides via Inner.
	Type string `json:"type"`

	// Inner is the raw inner `event` JSON exactly as Slack sent it.
	// Held as [json.RawMessage] so the M6.3.b/c handlers decode into
	// their own intent-specific types without paying the
	// generic-decode tax twice. May be nil for event_callbacks with
	// no inner payload (uncommon in practice).
	Inner json.RawMessage `json:"-"`
}

// Interaction is the decoded Interactivity payload passed to
// [InteractionDispatcher.DispatchInteraction]. Captures only the
// M6.3.a-relevant fields; the full payload rides via [Interaction.Raw]
// so M6.3.c approval-card wiring decodes into its own intent-specific
// types without paying the generic-decode tax twice.
type Interaction struct {
	// Type is the Interactivity payload type
	// (`block_actions`, `view_submission`, `view_closed`,
	// `message_action`, …). The dispatcher routes on this field.
	Type string `json:"type"`

	// TeamID is the Slack workspace id the interaction originated
	// in. Tenant-routing key.
	TeamID string `json:"team_id"`

	// APIAppID is the Slack app id the interaction was delivered to.
	APIAppID string `json:"api_app_id"`

	// Raw is the full payload JSON exactly as Slack sent it (after
	// form-decoding the `payload` field). M6.3.c decodes
	// intent-specific shapes from this slice.
	Raw json.RawMessage `json:"-"`
}

// noopEventDispatcher is the default [EventDispatcher] wired by
// [NewHandler] when no dispatcher is supplied via [WithEventDispatcher].
// Returning nil ACKs every event with no side effect — the right
// default for M6.3.a (scaffolding only) and for unit tests that exercise
// the negative paths without standing up an intent recogniser.
type noopEventDispatcher struct{}

// DispatchEvent satisfies [EventDispatcher] for the no-op fallback.
func (noopEventDispatcher) DispatchEvent(_ context.Context, _ Event) error { return nil }

// noopInteractionDispatcher is the default [InteractionDispatcher]
// wired by [NewHandler] when no dispatcher is supplied via
// [WithInteractionDispatcher]. Same rationale as
// [noopEventDispatcher].
type noopInteractionDispatcher struct{}

// DispatchInteraction satisfies [InteractionDispatcher] for the no-op
// fallback.
func (noopInteractionDispatcher) DispatchInteraction(_ context.Context, _ Interaction) error {
	return nil
}
