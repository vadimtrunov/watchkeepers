# outbox — Keep outbox consumer

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/outbox`

This package is the in-process bridge between the Keep service's
`/v1/subscribe` SSE stream and the local in-process event bus. It
closes the loop the M2 outbox pattern opens: producers in Keep write
rows to `watchkeeper.outbox` in the same transaction that mutates
business state; the Keep server's outbox publisher worker (M2.7.e.b)
drains those rows onto an SSE stream; this package consumes the stream
and re-publishes each event onto the local in-process bus so
downstream subscribers receive them without reaching back into Keep's
transactional store.

ROADMAP §M3 → M3.7.

## Public API

```go
type Consumer struct{ /* opaque */ }

type LocalSubscriber interface {
    Subscribe(ctx context.Context) (Stream, error)
}

type LocalSubscriberFunc func(ctx context.Context) (Stream, error)

type Stream interface {
    Next(ctx context.Context) (keepclient.Event, error)
    Close() error
}

type LocalBus interface {
    Publish(ctx context.Context, topic string, event any) error
}

type Logger interface {
    Log(ctx context.Context, msg string, kv ...any)
}

type DeliveredEvent struct {
    ID        string
    EventType string
    Payload   json.RawMessage
}

type Option func(*config)

func New(sub LocalSubscriber, bus LocalBus, opts ...Option) *Consumer
func WithLogger(l Logger) Option
func WithIdempotencyCacheSize(n int) Option
func WithPublishTimeout(d time.Duration) Option
func WithMaxPublishRetries(n int) Option
func WithRetryInitialDelay(d time.Duration) Option
func WithRetryMaxDelay(d time.Duration) Option

func (*Consumer) Start(ctx context.Context) error
func (*Consumer) Stop() error
```

The sentinel errors live in `errors.go`:

- `ErrAlreadyStarted` — `Start` called twice.
- `ErrAlreadyStopped` — `Start` called after `Stop`.
- `ErrNotStarted` — `Stop` called before `Start`.
- `ErrPublishExhausted` — surfaces via `Logger` when a single event
  exhausts its publish-retry budget. Wrap chain carries the last
  underlying eventbus error. Matchable via `errors.Is`.

## Quick start

```go
import (
    "context"
    "time"

    "github.com/vadimtrunov/watchkeepers/core/pkg/eventbus"
    "github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
    "github.com/vadimtrunov/watchkeepers/core/pkg/outbox"
)

func wire(ctx context.Context, kc *keepclient.Client, bus *eventbus.Bus) error {
    sub := outbox.LocalSubscriberFunc(func(ctx context.Context) (outbox.Stream, error) {
        return kc.SubscribeResilient(ctx,
            keepclient.WithDedupLRU(1024),
            keepclient.WithMaxReconnectAttempts(10),
        )
    })

    c := outbox.New(sub, bus,
        outbox.WithLogger(myLogger),
        outbox.WithIdempotencyCacheSize(2048),
        outbox.WithPublishTimeout(5*time.Second),
        outbox.WithMaxPublishRetries(3),
    )
    return c.Start(ctx)
}
```

`*keepclient.Client` does NOT satisfy `LocalSubscriber` directly — its
`SubscribeResilient` method returns `*keepclient.ResilientStream`
(which DOES satisfy `Stream`), and `SubscribeResilient` accepts
variadic options. Adapt with the `LocalSubscriberFunc` shown above.

## At-least-once delivery

The consumer guarantees at-least-once semantics for events the Keep
server emits. Events flow as:

```text
Keep server   --(SSE)-->   keepclient.SubscribeResilient
                                     |
                                     v
                              outbox.Consumer
                                     |
                  publish (with retry/backoff)
                                     v
                                eventbus.Bus
```

Publish to the bus is retried with bounded backoff
(`WithMaxPublishRetries`, default 3) before giving up. After the
budget is exhausted the consumer logs `outbox: publish exhausted` via
the optional `Logger` and DROPS the event. The dedup cache is NOT
updated on the failure path, so a future redelivery is allowed to
retry. There is no DLQ in Phase 1.

Reconnect handling is delegated to `keepclient.SubscribeResilient`:
the consumer asks the resilient stream for the next event and trusts
it to backoff/Last-Event-ID forward as configured. Forced redeliveries
(e.g. server restart that re-emits the same outbox row before its
publisher worker re-stamped) are detected by the idempotency cache
below.

## Idempotency strategy

Each outbox row carries a UUID `id` column that the Keep server emits
as the SSE `id:` field; the consumer treats this id as the event's
idempotency key. Before publishing to the bus the consumer checks an
in-memory bounded LRU; if the id has been seen recently the event is
dropped silently (forced-redelivery suppression).

**Trade-off**: an in-memory LRU does NOT survive process restart, so
a redelivery of the SAME event id across a restart MAY be
re-published. Phase 1 accepts this — bus subscribers are required to
be idempotent on their own (per the ROADMAP cross-cutting
constraint). A persistent dedup store (Postgres `seen_event` table or
BoltDB) is explicitly out of scope for M3.7; revisit when a
non-idempotent subscriber lands.

LRU cache size defaults to 1024 (override via
`WithIdempotencyCacheSize`); empty event ids are NEVER deduplicated.

## Backpressure

`eventbus.Bus` applies backpressure on `Publish` — a full per-topic
queue blocks the publisher until a worker drains a slot. The consumer
applies a per-event publish timeout (`WithPublishTimeout`, default 5s)
on top: if the bus stays blocked past the timeout the publish attempt
is treated as a transient error, retried with backoff per
`WithMaxPublishRetries`, and eventually surfaced as
`ErrPublishExhausted` (dropped). This bounds the consumer's hot path
even when the bus is misconfigured or stalled by a deadlocked
subscriber.

## Topic strategy

The consumer routes every event to the bus topic that EQUALS the
event's `EventType` field (e.g. `watchkeeper.spawned` → topic
`watchkeeper.spawned`). Bus subscribers therefore filter by the same
string the Keep server emitted on the wire. Per the ROADMAP M3.1
contract, the bus delivers events ordered within a topic; the
consumer preserves that ordering by publishing one event at a time in
receive order.

An empty `EventType` falls back to the empty string, which the bus
rejects with `eventbus.ErrInvalidTopic`; the consumer treats that as
a malformed-event log entry and drops the event without retrying.

## Trace + correlation propagation

The Keep server emits events whose `payload` field is the original
outbox `payload` jsonb (the `keeperslog` envelope when the producer
was the keeperslog writer — `event_id`, `correlation_id`, `trace_id`,
`span_id`, `data`). The consumer does NOT decode the payload; it
forwards the entire decoded SSE event verbatim onto the bus as a
`DeliveredEvent`. Bus subscribers that care about trace/correlation
propagation parse the payload themselves. The consumer never
fabricates trace ids or correlation ids; if Keep omitted them the bus
subscriber sees the omission verbatim.

## Functional options

```go
// Defaults: nil logger, LRU = 1024, timeout = 5s, retries = 3,
// initialDelay = 25ms, maxDelay = 1s.
c := outbox.New(sub, bus)

// With a structured logger.
c := outbox.New(sub, bus, outbox.WithLogger(myLogger))

// With dedup disabled (programmer error in production but useful in
// tests that need to assert raw delivery).
c := outbox.New(sub, bus, outbox.WithIdempotencyCacheSize(0))

// With aggressive retries.
c := outbox.New(sub, bus,
    outbox.WithMaxPublishRetries(10),
    outbox.WithRetryInitialDelay(50*time.Millisecond),
    outbox.WithRetryMaxDelay(2*time.Second),
)
```

All options accept nil / non-positive arguments as no-ops so callers
can always pass through whatever they have.

## Logger event vocabulary

| Event                            | Fields                                          |
| -------------------------------- | ----------------------------------------------- |
| `outbox: subscribe failed`       | `err_type`                                      |
| `outbox: stream error`           | `err_type`                                      |
| `outbox: publish attempt failed` | `event_id`, `event_type`, `attempt`, `err_type` |
| `outbox: publish exhausted`      | `event_id`, `event_type`, `err_type`            |
| `outbox: malformed event …`      | `event_id`                                      |

**Redaction discipline**: the logger NEVER sees `Payload` data. Only
metadata (`event_id`, `event_type`, `attempt`, `err_type`) is logged.
Failures log only the error TYPE (`fmt.Sprintf("%T", err)`), never
`err.Error()` — the error TYPE is provably non-sensitive; the value
may contain arbitrary upstream text. Mirrors the M3.4.b config-loader
and M3.5 capability-broker redaction patterns documented in
`docs/LESSONS.md`.

## Concurrency

`*Consumer` is safe for concurrent use across goroutines once
constructed. Lifecycle transitions (`Start` / `Stop`) are guarded by
an internal mutex; the receive→publish loop runs on a single
background goroutine spawned by `Start`. The dedup cache is touched
only by that goroutine but is internally lock-protected for safety
under future multi-worker variants.

## Out of scope (deferred)

- **Persistent idempotency store** — see "Idempotency strategy"
  above. Phase 1 uses an in-memory LRU; persistence revisits in
  Phase 2.
- **Dead-letter queue** for events that exhaust publish retries — see
  "At-least-once delivery". Phase 1 logs and drops; later phases
  build a queue when a non-idempotent subscriber arrives.
- **Per-topic backpressure tuning** — every event uses the same
  `WithPublishTimeout` / `WithMaxPublishRetries` knobs. Topic-
  specific tuning waits for a real-world hot-spot.
- **Capability-token wiring** — the consumer consumes whatever
  `LocalSubscriber` the caller hands in. Token issuance (M3.5) and
  per-call validation are deferred to the M5 harness consumer where
  call sites are concrete.

## See also

- `docs/ROADMAP-phase1.md` §M3 → M3.7 — milestone scope and
  acceptance.
- `docs/LESSONS.md` — M3.4.b/M3.5/M3.6 redaction discipline,
  M3.2.b/M3.3 `LocalX` import-cycle-break pattern.
- `core/pkg/keepclient/` — underlying transport client (the
  `SubscribeResilient` method this consumer drives).
- `core/pkg/eventbus/` — the in-process bus this consumer publishes
  to.
- `core/pkg/keeperslog/` — sibling M3 package writing structured
  events that may flow through the outbox.
