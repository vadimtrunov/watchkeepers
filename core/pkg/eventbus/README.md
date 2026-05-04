# eventbus — in-process pub/sub event bus

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/eventbus`

This package provides the foundational in-process pub/sub event bus that
the rest of ROADMAP §M3 builds on (lifecycle, cron, keeperslog, outbox).
It is **in-process only** — there is no cross-binary delivery — and it
deliberately stays tiny: a `Bus` with `Subscribe`, `Publish`, and `Close`,
plus a fixed handler signature.

## Public API

```go
type Handler = func(ctx context.Context, event any)

func New(opts ...Option) *Bus
func WithTopicBufferSize(n int) Option

func (*Bus) Subscribe(topic string, handler Handler) (unsubscribe func(), err error)
func (*Bus) Publish(ctx context.Context, topic string, event any) error
func (*Bus) Close() error
```

Sentinel errors live in `errors.go`:

- `ErrClosed` — `Publish` / `Subscribe` after `Close`.
- `ErrInvalidTopic` — empty topic on `Publish` / `Subscribe`.
- `ErrInvalidHandler` — nil handler on `Subscribe`.

## Quick start

```go
bus := eventbus.New(eventbus.WithTopicBufferSize(128))
defer bus.Close()

unsub, err := bus.Subscribe("notebook.entry.remembered", func(ctx context.Context, ev any) {
    log.Printf("got entry: %v", ev)
})
if err != nil {
    log.Fatal(err)
}
defer unsub()

if err := bus.Publish(context.Background(), "notebook.entry.remembered", entryID); err != nil {
    log.Printf("publish: %v", err)
}
```

## Ordering

Each topic is served by exactly one worker goroutine. Events flow through
a buffered channel — the topic queue — and the worker pops them one at a
time and dispatches them to a snapshot of the current subscriber list
sequentially in registration order. Two consequences:

- **Per-topic ordered delivery is guaranteed.** Events published to one
  topic are delivered to every subscriber in publish order.
- **Different topics are independent.** Publish order across topics is
  not preserved; a slow handler on topic `A` does NOT stall topic `B`
  (separate worker goroutines).

### Concurrent publishers and "publish order"

When two goroutines call `Publish` on the same topic simultaneously,
"publish order" means **enqueue order** to the bus's channel — NOT
call-time order. The bus serialises their sends through the topic's
channel; whichever enters the channel first is dispatched first. The
property that always holds:

> A single publisher's sequential `Publish` calls land in the queue in
> the order they were issued, so per-publisher ordering is preserved.

If you need a global total order across publishers, push through a
single goroutine.

### Late subscribers

A subscriber added during a topic's worker iteration does NOT
retroactively receive the in-flight event. The worker takes a snapshot of
the subscriber list per envelope; new subscribers see only events
published AFTER the `Subscribe` call returns.

## Backpressure

Each per-topic channel is a buffered Go channel sized by
`WithTopicBufferSize(n)` (default `64`).

- Buffer not full → `Publish` enqueues and returns nil immediately.
- Buffer full → `Publish` blocks until either:
  - a worker drains a slot (the queue accepts the envelope), or
  - the supplied `ctx` is cancelled (returns
    `fmt.Errorf("eventbus: publish: %w", ctx.Err())`), or
  - `Close()` is called (returns `ErrClosed`).

The bus never silently drops an event. Subscribers wanting durable
replay should layer the upcoming M3.7 outbox on top of the bus rather
than relying on the in-memory queue.

### Slow handlers

A slow handler stalls only its own topic. Because each topic has a
dedicated worker goroutine, a handler that blocks for 10s on topic `A`
will not delay events on topic `B`. Within topic `A` the queue fills
behind the slow handler and publishers eventually backpressure per the
section above. Keep handlers fast (or push slow work through an
out-of-band goroutine inside the handler).

## Shutdown

`Close()` is idempotent and synchronous:

1. Flips `closed` so subsequent `Publish` / `Subscribe` return
   `ErrClosed`.
2. Wakes any publishers blocked on backpressure (they receive
   `ErrClosed` and return without enqueueing).
3. Waits for in-flight `Publish` goroutines to finish their send-or-bail
   select.
4. Closes every per-topic channel; each worker drains its remaining
   buffered envelopes and exits.
5. Waits for every worker goroutine to exit, then returns nil.

Subsequent `Close` calls return nil without rerunning the drain.

Under `go test -race` the bus passes a goroutine-baseline leak check —
no goroutines outlive `Close`.

## Validation

Both `Publish` and `Subscribe` validate eagerly:

- Empty topic → `ErrInvalidTopic` (no I/O, no goroutine spawn).
- Nil handler on `Subscribe` → `ErrInvalidHandler`.
- Calls after `Close` → `ErrClosed`.

The returned `unsubscribe` callback is idempotent: the first call
removes the handler; subsequent calls are no-ops (no panic, no error).
On a `Subscribe` error the returned `unsubscribe` is a no-op closure so
callers can `defer unsubscribe()` without an explicit nil check.

## Handler contract

```go
type Handler = func(ctx context.Context, event any)
```

The `ctx` is the same context passed to `Publish`. Handlers SHOULD honour
`ctx.Done()` for any blocking I/O.

Handlers MUST NOT panic. A panicking handler will crash its topic's
worker goroutine and silently drop subsequent events on that topic.
Bounded panic recovery is deferred to a future revision (see
[Future extensions](#future-extensions)).

## Future extensions

This package is intentionally minimal. Higher-level features are layered
on top by other M3 milestones:

- **Correlation IDs** — M3.6 `keeperslog` wraps published events with a
  typed envelope carrying a correlation id and emits a corresponding
  `keepers_log` row. The bus itself stays untyped (`any`).
- **Durable replay** — M3.7 `outbox` ships a transactional outbox
  pattern: events are appended to a Postgres outbox table inside the
  same transaction as the originating mutation, then a relayer reads the
  outbox and publishes to this bus. Restart-survival lives there, not
  here.
- **Handler panic recovery** — out of scope for v1. May land later as
  an opt-in option that catches `recover()` and surfaces the panic
  through a configurable error sink.
- **Cross-process / cross-binary delivery** — out of scope. The bus is
  in-process only; cross-process delivery flows through the Keep server's
  HTTP/SSE surfaces.
