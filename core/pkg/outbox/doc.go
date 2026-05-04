// Package outbox is the in-process outbox consumer — it subscribes to
// the Keep service's `/v1/subscribe` SSE stream via [keepclient] and
// forwards every received event onto an in-process event bus
// ([eventbus.Bus] satisfies the [LocalBus] interface as-is). ROADMAP
// §M3 → M3.7.
//
// The consumer closes the loop the M2 outbox pattern opens: producers
// inside Keep write rows to `watchkeeper.outbox` in the same
// transaction that mutates business state; the Keep server's outbox
// publisher worker (M2.7.e.b) drains those rows onto an SSE stream;
// this package consumes the stream and re-publishes each event onto
// the local in-process bus so downstream subscribers (lifecycle,
// keeperslog, future watchkeeper handlers) receive them without
// reaching back into Keep's transactional store.
//
// # Why a wrapper, not direct subscription
//
// Every Watchkeeper subsystem that wants to react to outbox events
// would otherwise need to (a) hold a [keepclient.Client], (b) parse
// SSE frames, (c) implement reconnect/backoff, (d) deduplicate
// redeliveries, and (e) marshal events onto its own preferred queue.
// The consumer centralises (a)–(d) once and exposes a vendor-neutral
// [LocalBus.Publish] surface for (e). Subsystems subscribe to the bus
// instead of to Keep.
//
// # At-least-once delivery
//
// The consumer guarantees at-least-once semantics for events the Keep
// server emits. Events flow as:
//
//	Keep server   --(SSE)-->   keepclient.SubscribeResilient
//	                                     |
//	                                     v
//	                              outbox.Consumer
//	                                     |
//	                  publish (with retry/backoff)
//	                                     v
//	                                eventbus.Bus
//
// Publish to the bus is retried with bounded backoff on transient
// errors ([eventbus.Publish] surfaces ctx-cancel / Close as terminal;
// any other error is treated as transient). After
// [WithMaxPublishRetries] consecutive failures the consumer logs
// [ErrPublishExhausted] via the optional [Logger] and DROPS the event
// from in-memory state — there is no DLQ in Phase 1. The event remains
// stamped `published_at` in Keep, however, so a subsequent restart will
// NOT redeliver it. Callers needing stronger durability guarantees
// build a DLQ on top of [Logger] + their own queue.
//
// Reconnect handling is delegated to [keepclient.SubscribeResilient]:
// the consumer asks the resilient stream for the next event and
// trusts it to backoff/Last-Event-ID forward as configured. Forced
// redeliveries (e.g. server restart that re-emits the same outbox row
// before its publisher worker re-stamped) are detected by the
// idempotency cache below.
//
// # Idempotency strategy
//
// Each outbox row carries a UUID `id` column that the Keep server
// emits as the SSE `id:` field; the consumer treats this id as the
// event's idempotency key. Before publishing to the bus the consumer
// checks an in-memory bounded LRU; if the id has been seen recently
// the event is dropped silently (forced-redelivery suppression).
//
// Trade-off: an in-memory LRU does NOT survive process restart, so a
// redelivery of the SAME event id across a restart MAY be
// re-published. Phase 1 accepts this — bus subscribers are required
// to be idempotent on their own (per the ROADMAP cross-cutting
// constraint). A persistent dedup store (Postgres `seen_event` table
// or BoltDB) is explicitly out of scope for M3.7; revisit when a
// non-idempotent subscriber lands.
//
// LRU cache size defaults to 1024 (override via
// [WithIdempotencyCacheSize]); empty event ids are NEVER deduplicated
// (mirrors the [keepclient] resilient-stream LRU contract).
//
// # Backpressure
//
// [eventbus.Bus] applies backpressure on Publish — a full per-topic
// queue blocks the publisher until a worker drains a slot. The
// consumer applies a per-event publish timeout
// ([WithPublishTimeout], default 5s) on top: if the bus stays blocked
// past the timeout the publish attempt is treated as a transient
// error, retried with backoff per [WithMaxPublishRetries], and
// eventually surfaced as [ErrPublishExhausted] (dropped). This bounds
// the consumer's hot path even when the bus is misconfigured or
// stalled by a deadlocked subscriber — without it a single bad
// subscriber would freeze the entire outbox-to-bus pipeline.
//
// Within the timeout window the bus's blocking semantics still apply:
// the consumer respects ctx cancellation and Close mid-publish.
//
// # Topic strategy
//
// The consumer routes every event to the bus topic that EQUALS the
// event's `EventType` field (e.g. `watchkeeper.spawned` → topic
// `watchkeeper.spawned`). Bus subscribers therefore filter by the
// same string the Keep server emitted on the wire. Per the ROADMAP
// M3.1 contract, the bus delivers events ordered within a topic; the
// consumer preserves that ordering by publishing one event at a time
// in receive order.
//
// An empty `EventType` falls back to the empty string, which the bus
// rejects with [eventbus.ErrInvalidTopic]; the consumer treats that as
// a malformed-event log entry and drops the event without retrying
// (an empty event_type is a server-side bug; redelivery would not
// help).
//
// # Trace + correlation propagation
//
// The Keep server emits events whose `payload` field is the original
// outbox `payload` jsonb (the keeperslog envelope when the producer
// was the keeperslog writer — `event_id`, `correlation_id`,
// `trace_id`, `span_id`, `data`). The consumer does NOT decode the
// payload; it forwards the entire decoded SSE event verbatim onto the
// bus as a [DeliveredEvent]. Bus subscribers that care about
// trace/correlation propagation parse the payload themselves. The
// consumer never fabricates trace ids or correlation ids; if Keep
// omitted them the bus subscriber sees the omission verbatim.
//
// # Out of scope (deferred)
//
//   - Persistent idempotency store — see "Idempotency strategy" above.
//   - DLQ (dead-letter queue) for events that exhaust publish retries
//     — see "At-least-once delivery". Phase 1 logs and drops; later
//     phases build a queue when a non-idempotent subscriber arrives.
//   - Per-topic backpressure tuning — every event uses the same
//     [WithPublishTimeout] / [WithMaxPublishRetries] knobs. Topic-
//     specific tuning waits for a real-world hot-spot.
//   - Capability-token wiring for the keepclient — the consumer
//     consumes whatever [LocalSubscriber] the caller hands in. Token
//     issuance (M3.5) and per-call validation are deferred to the M5
//     harness consumer where call sites are concrete.
package outbox
