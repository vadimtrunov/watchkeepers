// Package publish defines the in-process publish/subscribe API used by the
// Keep service to stream capability-scoped events to SSE subscribers.
//
// M2.7.e.a ships the transport (Registry, Publisher, Event) and the SSE
// handler; M2.7.e.b will wire the outbox worker into Publisher.Publish.
// The shape is deliberately a superset of the outbox DDL (aggregate_*,
// event_type, payload, created_at, id) plus an explicit Scope string —
// the outbox table has no scope column, so scope derivation is the
// worker's responsibility and out of scope for this milestone.
package publish

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Event is the in-process, scope-bound envelope the Registry fans out to
// matching subscribers. Field names mirror the outbox column names so a
// future worker can construct an Event from one row verbatim plus the
// derived Scope.
type Event struct {
	// ID uniquely identifies the event. Emitted as the SSE `id:` field
	// on the wire; also used by outbox de-duplication in M2.7.e.b.
	ID uuid.UUID
	// Scope is the exact-match key the Registry uses for fan-out. Same
	// syntax as the auth-layer claim scopes: "org", "user:<uuid>", or
	// "agent:<uuid>". Hierarchy widening is explicitly NOT supported.
	Scope string
	// AggregateType is the entity family the event is about (e.g.
	// "watchkeeper", "manifest"). Mirrors watchkeeper.outbox.aggregate_type.
	AggregateType string
	// AggregateID is the specific aggregate the event is about. Mirrors
	// watchkeeper.outbox.aggregate_id.
	AggregateID uuid.UUID
	// EventType is the domain event name (e.g. "watchkeeper.spawned").
	// Emitted as the SSE `event:` field on the wire.
	EventType string
	// Payload is the event body. Must be valid JSON; emitted verbatim as
	// the SSE `data:` field. Never re-encoded by the transport.
	Payload json.RawMessage
	// CreatedAt is the event's domain-time of emission. Mirrors
	// watchkeeper.outbox.created_at.
	CreatedAt time.Time
}

// Publisher is the narrow interface the future outbox worker depends on.
// Keeping it one-method makes the seam obvious and lets tests pass a
// no-op double without reimplementing the full Registry.
type Publisher interface {
	// Publish fans the event out to every subscriber whose Claim.Scope
	// equals event.Scope (exact string match). Never blocks: a full
	// subscriber buffer causes that subscriber to be dropped, and the
	// call still returns nil. Returns a non-nil error only on context
	// expiry or post-Close (the latter is a no-op and still returns nil
	// so a worker never crashes during shutdown).
	Publish(ctx context.Context, ev Event) error
}
