// event.go ships the M1.3.c peer-event domain types — [Event] (the
// payload shape every [EventBus] [Publish] / [Subscribe] flow exchanges)
// and [SubscribeFilter] (the closed-set query shape [EventBus.Subscribe]
// accepts). The taxonomy of `event_type` strings is deliberately open at
// M1.3.c: every consumer (M1.4 audit subscriber, M1.7 archive-on-summary
// writer, future M5.* tool emitters) names its own event_type within its
// own package; this leaf only ships the seam.
//
// Mirrors the M1.1.a / M1.3.a "closed-set typed struct over positional
// args" discipline: a future addition (e.g. an explicit `CorrelationID`
// header for the M1.6 escalation saga) lands as a new field rather than
// a breaking signature change.
//
// PII discipline: [Event.Payload] is treated as opaque JSON bytes. The
// in-memory and Postgres [EventBus] implementations defensively
// deep-copy the slice on every Publish + Subscribe delivery boundary so
// caller-side mutation cannot bleed in either direction.

package peer

import (
	"time"

	"github.com/google/uuid"
)

// Event is the closed-set payload shape every [EventBus] flow exchanges.
// Hoisted to a struct (rather than a long positional arg list) so a
// future addition (e.g. an explicit `CorrelationID` for the M1.6
// escalation saga) lands as a new field rather than a breaking signature
// change. Mirrors the [k2k.Message] / [k2k.Conversation] shape.
type Event struct {
	// ID is the row's primary key, minted by the publisher (the
	// in-memory adapter mints a fresh `uuid.New()`; the Postgres adapter
	// either reuses the caller-supplied id when non-zero or mints one).
	// Required (non-zero); [EventBus.Publish] fail-fasts via
	// [ErrInvalidEventID] otherwise.
	ID uuid.UUID

	// OrganizationID is the tenant key for the event. The matching
	// migration's RLS policies match against this column via the
	// `watchkeeper.org` session GUC; per-tenant isolation lives at the
	// Postgres layer in production and is mirrored by the in-memory
	// adapter's filter discipline. Required (non-zero); [EventBus.Publish]
	// fail-fasts via [ErrInvalidOrganizationID] otherwise. Subscriber
	// filtering is keyed off this column AND the
	// [SubscribeFilter.TargetWatchkeeperID] / [SubscribeFilter.EventTypes]
	// fields so a cross-tenant subscriber cannot observe a foreign-tenant
	// event.
	OrganizationID uuid.UUID

	// WatchkeeperID is the id of the watchkeeper the event pertains to —
	// the subject, not necessarily the publisher. The
	// [SubscribeFilter.TargetWatchkeeperID] field matches against this
	// column so a [Tool.Subscribe] caller that wants events about peer
	// X passes X's id verbatim. Required (non-empty after whitespace-
	// trim); [EventBus.Publish] fail-fasts via [ErrEmptyWatchkeeperID]
	// otherwise.
	WatchkeeperID string

	// EventType is the closed-set or namespaced taxonomy label
	// describing the event. M1.3.c does NOT pin a finite enum here —
	// downstream consumers (M1.4 audit taxonomy: `k2k_message_sent`,
	// `k2k_conversation_opened`, `k2k_conversation_closed`; M5.*
	// tool emitters) own their own type strings. The
	// [SubscribeFilter.EventTypes] field matches verbatim. Required
	// (non-empty after whitespace-trim); [EventBus.Publish] fail-fasts
	// via [ErrEmptyEventType] otherwise.
	EventType string

	// Payload is the opaque JSON-encoded event body. Treated as
	// reference-typed (defensively deep-copied on every Publish +
	// Subscribe delivery boundary) so caller-side mutation cannot bleed.
	// Empty / nil payloads are allowed — an event whose semantics are
	// fully captured by its `EventType` does not require a body.
	Payload []byte

	// CreatedAt is the wall-clock time the event was minted. Stamped by
	// the in-memory adapter's `now` clock on Publish; stamped by the
	// Postgres adapter's `now()` default on INSERT.
	CreatedAt time.Time
}

// SubscribeFilter is the closed-set query shape [EventBus.Subscribe]
// accepts. Hoisted to a struct so a future addition (e.g. a
// `CorrelationID` filter for the M1.6 escalation saga) lands as a new
// field rather than a breaking signature change.
//
// The filter is intentionally narrow at M1.3.c — the M1.4 audit
// subscriber and the [Tool.Subscribe] consumer both want
// (target, event_types) pairs only. Richer filters (time window,
// correlation id, payload-substring match) land alongside the consumer
// that needs them per the YAGNI discipline established in M6.3.b.
type SubscribeFilter struct {
	// OrganizationID restricts the subscription to events under the
	// supplied tenant. Required (non-zero); the bus fail-fasts via
	// [ErrInvalidOrganizationID] otherwise. The in-memory adapter scopes
	// every delivery to this id; the Postgres adapter additionally relies
	// on the RLS policy keyed off `watchkeeper.org` so a misconfigured
	// caller surfaces as zero deliveries rather than silent cross-tenant
	// access.
	OrganizationID uuid.UUID

	// TargetWatchkeeperID restricts the subscription to events whose
	// [Event.WatchkeeperID] matches verbatim (exact, case-sensitive).
	// Empty selects every watchkeeper under the supplied tenant — the
	// M1.4 audit subscriber consumes the un-filtered stream this way.
	TargetWatchkeeperID string

	// EventTypes restricts the subscription to events whose
	// [Event.EventType] appears in this set (exact, case-sensitive).
	// `nil` / empty selects every event type. The bus defensively
	// deep-copies the slice on subscribe so caller-side mutation cannot
	// bleed.
	EventTypes []string
}

// CancelFunc cancels a subscription, closing the delivery channel and
// releasing any goroutines / connections held on the subscriber's
// behalf. Idempotent — calling it more than once is a no-op. Calling it
// AFTER the subscription's ctx has cancelled is also a no-op (the bus
// closes the channel on ctx cancel without waiting for CancelFunc).
//
// Mirrors the standard-library `context.CancelFunc` shape so a future
// reader who knows the stdlib idiom recognises the pattern immediately.
type CancelFunc func()
