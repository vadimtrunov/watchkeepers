// eventbus.go ships the M1.3.c [EventBus] interface — the
// Publish / Subscribe seam the [Tool.Subscribe] built-in consumes and
// the M1.4 audit subscriber will publish to. The interface is the
// unit-test seam; the production impl is [PostgresEventBus] in
// `eventbus_postgres.go` and the dev/test impl is [MemoryEventBus] in
// `eventbus_memory.go`.
//
// Bounded-buffer / slow-consumer discipline: every implementation
// delivers events into a bounded per-subscription buffer. When the
// buffer is full the delivery layer DROPS the event and increments an
// in-memory `dropped_events` counter rather than blocking the publisher.
// This protects the publisher (and any other subscribers behind the
// same bus) from a single slow consumer wedging the whole stream — the
// classic head-of-line-blocking problem.
//
// Channel closure: the delivery channel is closed when (and only when)
// either:
//
//   - the subscription's ctx cancels — the bus observes the
//     cancellation and closes the channel,
//   - the returned [CancelFunc] is invoked.
//
// A closed channel is a closed-set signal to the consumer that the
// subscription is over; the consumer MUST NOT continue ranging over it
// expecting fresh events. The bus does NOT close the channel on
// transient publish errors — those are surfaced to the publisher only.
//
// Concurrency: every method on [EventBus] is safe for concurrent use
// across goroutines.
//
// See `docs/ROADMAP-phase2.md` §M1 → M1.3 → M1.3.c for the AC.

package peer

import "context"

// EventBus is the M1.3.c Publish / Subscribe seam. Mirrors the
// [k2k.Repository] discipline (narrow interface, every method documents
// its validation order, defensive deep-copy boundary), but at the
// event-stream layer.
//
// Lifecycle ordering: a publisher calls [Publish] with a validated
// [Event]; the bus fan-outs the event to every matching subscriber's
// delivery channel (subject to the bounded-buffer / slow-consumer drop
// policy). A subscriber calls [Subscribe] with a [SubscribeFilter]; the
// bus returns a fresh delivery channel + a [CancelFunc] that
// short-circuits the subscription. The channel is closed on ctx cancel
// or CancelFunc invocation.
type EventBus interface {
	// Publish persists / fan-outs `event` to every matching subscriber.
	//
	// Validation order (fail-fast precedes persistence):
	//   1. ctx.Err — refuses a pre-cancelled ctx.
	//   2. event.ID != uuid.Nil — [ErrInvalidEventID].
	//   3. event.OrganizationID != uuid.Nil — [ErrInvalidOrganizationID].
	//   4. trimmed event.WatchkeeperID != "" — [ErrEmptyWatchkeeperID].
	//   5. trimmed event.EventType != "" — [ErrEmptyEventType].
	//
	// The in-memory adapter delivers synchronously under its read lock
	// (fan-out + per-subscriber buffered-channel send); the Postgres
	// adapter INSERTs the row + relies on the migration's
	// `peer_event_published` trigger to fire `NOTIFY peer_events` so
	// every listening backend wakes up. [Event.Payload] is defensively
	// deep-copied before persistence so caller-side mutation cannot
	// bleed.
	Publish(ctx context.Context, event Event) error

	// Subscribe registers a new subscription matching `filter` and
	// returns a fresh delivery channel + a [CancelFunc].
	//
	// Validation order (fail-fast precedes side effects):
	//   1. ctx.Err — refuses a pre-cancelled ctx.
	//   2. filter.OrganizationID != uuid.Nil —
	//      [ErrInvalidOrganizationID].
	//
	// The returned channel is buffered with the bus's configured
	// per-subscription buffer size. The bus drops events into the
	// channel non-blocking; a slow consumer trips the drop policy
	// (event is discarded + the bus's drop counter increments).
	//
	// Channel-closure invariants:
	//   - the channel is closed when `ctx` cancels OR [CancelFunc] is
	//     invoked, whichever fires first;
	//   - a closed channel signals end-of-subscription;
	//   - the bus does NOT close the channel on transient publish
	//     errors (those reach the publisher only);
	//   - calling [CancelFunc] more than once is a no-op.
	//
	// Filter shape:
	//   - empty [SubscribeFilter.TargetWatchkeeperID] selects every
	//     watchkeeper in the supplied tenant;
	//   - empty / nil [SubscribeFilter.EventTypes] selects every event
	//     type.
	Subscribe(ctx context.Context, filter SubscribeFilter) (<-chan Event, CancelFunc, error)

	// DroppedEvents returns the running count of events the bus has
	// dropped on slow-consumer subscribers' behalf. The counter is a
	// monotonic uint64; callers compare against a snapshot to detect a
	// drop-rate spike. The counter is bus-wide (not per-subscription)
	// at M1.3.c — a future iteration may decompose per-subscription if
	// the diagnostic shape becomes load-bearing.
	DroppedEvents() uint64
}
