# cron — cron-spec scheduler emitting events onto the event bus

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/cron`

This package wraps [`github.com/robfig/cron/v3`](https://github.com/robfig/cron)
behind a thin `Scheduler` facade and publishes a fresh event onto a
`LocalPublisher` (structurally satisfied by `*eventbus.Bus`) on every
fire. ROADMAP §M3 → M3.3.

## Public API

```go
type EventFactory = func(ctx context.Context) any
type EntryID = robfigcron.EntryID

type LocalPublisher interface {
    Publish(ctx context.Context, topic string, event any) error
}

type Logger interface {
    Log(ctx context.Context, msg string, kv ...any)
}

func New(pub LocalPublisher, opts ...Option) *Scheduler
func WithLogger(l Logger) Option
func WithLocation(loc *time.Location) Option

func (*Scheduler) Schedule(spec, topic string, factory EventFactory) (EntryID, error)
func (*Scheduler) Unschedule(id EntryID) error
func (*Scheduler) Start(ctx context.Context) error
func (*Scheduler) Stop() context.Context
```

Sentinel errors live in `errors.go`:

- `ErrInvalidSpec` — empty spec on `Schedule`.
- `ErrInvalidTopic` — empty topic on `Schedule`.
- `ErrInvalidFactory` — nil factory on `Schedule`.
- `ErrAlreadyStarted` — second `Start` on the same scheduler.
- `ErrAlreadyStopped` — `Schedule` / `Unschedule` after `Stop`.

## Quick start

```go
import (
    "context"

    "github.com/vadimtrunov/watchkeepers/core/pkg/cron"
    "github.com/vadimtrunov/watchkeepers/core/pkg/eventbus"
)

func wire(ctx context.Context) error {
    bus := eventbus.New()
    defer bus.Close()

    sched := cron.New(bus)

    _, err := sched.Schedule(
        "0 */30 * * * *", // every 30 minutes (6-field, seconds first)
        "watchkeeper.cron.tick",
        func(ctx context.Context) any {
            return map[string]any{
                "correlation_id": newCorrelationID(),
                "fired_at":       time.Now().UTC(),
            }
        },
    )
    if err != nil {
        return err
    }

    if err := sched.Start(ctx); err != nil {
        return err
    }
    defer func() { <-sched.Stop().Done() }()

    <-ctx.Done()
    return nil
}
```

## Spec format

The internal `*robfigcron.Cron` is constructed with `cron.WithSeconds()`,
so all specs use the **6-field** form (`sec min hour dom mon dow`):

| Spec             | Meaning                      |
| ---------------- | ---------------------------- |
| `* * * * * *`    | Every second (testing only). |
| `0 */5 * * * *`  | At second 0 every 5 minutes. |
| `0 0 * * * *`    | At the top of every hour.    |
| `0 30 9 * * 1-5` | 09:30 every weekday.         |

A 5-field spec (e.g. `*/5 * * * *`) returns a wrapped parser error from
`Schedule` — prepend a `0` second-field to convert. Descriptors like
`@daily` / `@hourly` are also accepted by the underlying parser.

## Why a factory closure (not a static event)

M3-wide verification requires that cron-fired and handler-ran events
carry **matching** correlation ids. A static event captured at
`Schedule` time would reuse the same id on every fire — wrong. The
factory pattern lets the caller mint a fresh id (and timestamp) per
fire while the scheduler stays untyped (`any`):

```go
sched.Schedule("0 0 * * * *", "watchkeeper.heartbeat", func(ctx context.Context) any {
    return HeartbeatEvent{
        CorrelationID: uuid.NewString(),
        FiredAt:       time.Now().UTC(),
    }
})
```

## Best-effort firing

Per-fire failures do **not** halt the scheduler. The next tick will
retry. This mirrors `notebook.PeriodicBackup`'s best-effort tick
semantics (M2b.5 LESSONS):

- **Factory panic** → recovered, logged via `WithLogger`, scheduling
  continues. The fire that panicked produces no publish.
- **Publisher error** → logged via `WithLogger`, scheduling continues.
  The bad fire still appears in the publisher's call log because the
  panic-recovery wrapper sits _outside_ `Publish`.

Operators observe failures via the `Logger` (log / metric); the
scheduler itself never enters a "broken" state until `Stop` is called.

## Lifecycle

A `Scheduler` is single-use and follows a linear state machine:
**not-started → started → stopped**.

| Method                     | not-started    | started             | stopped             |
| -------------------------- | -------------- | ------------------- | ------------------- |
| `Schedule(spec, topic, …)` | OK             | OK (live add)       | `ErrAlreadyStopped` |
| `Unschedule(id)`           | OK (no-op)     | OK                  | `ErrAlreadyStopped` |
| `Start(ctx)`               | OK             | `ErrAlreadyStarted` | `ErrAlreadyStopped` |
| `Stop()`                   | OK (immediate) | OK (drains)         | OK (idempotent)     |

`Stop()` returns the underlying robfig stop-context; callers can
`<-ctx.Done()` to wait for any in-flight `Job.Run` to return — useful
for graceful shutdown:

```go
ctx, cancel := context.WithCancel(context.Background())
defer cancel()
if err := sched.Start(ctx); err != nil { /* … */ }
defer func() {
    stopCtx := sched.Stop()
    select {
    case <-stopCtx.Done():
    case <-time.After(10 * time.Second):
        // log: drain timeout
    }
}()
```

A second `Stop()` returns the same already-done ctx without re-entering
the underlying drain.

## LocalPublisher interface (one-way decoupling)

The package depends on a local `LocalPublisher` interface that mirrors
the single `eventbus.Bus.Publish` method `Scheduler` consumes.
Production cron code never imports `eventbus` directly; only the tests
do, for the compile-time assertion:

```go
var _ LocalPublisher = (*eventbus.Bus)(nil)
```

This mirrors the M2b.6 cross-package compile-time-check pattern
documented in `docs/LESSONS.md` and the M3.2.b `lifecycle.LocalKeepClient`
discipline. A future `eventbus.Bus.Publish` signature change breaks
that one assertion line at the same compile step the production wiring
breaks at — no later.

## Concurrency

`*Scheduler` is safe for concurrent use. State transitions and entry
registration are guarded by an internal mutex. Per-fire closures run
on robfig's worker goroutine; a slow publisher therefore stalls only
the cron-fire pipeline, not the rest of the program.

`*eventbus.Bus`'s own concurrency contract still applies: a slow
handler stalls only its own topic, not the cron's next-tick scheduling
(see `core/pkg/eventbus/README.md` for details).

## Out of scope (deferred)

- **Durable schedule persistence** — cron entries are in-memory only.
  A restart re-runs the wiring code that registered them. A future
  milestone may add a Postgres-backed schedule table read at boot.
- **Distributed-lock / leader election** — Phase 1 is single-host;
  multi-host coordination (only one of N replicas fires a given entry)
  is out of Phase 1 entirely.
- **Clock injection / mocking** — robfig/cron v3 has no built-in clock
  seam; tests use sub-second specs (`* * * * * *` via
  `cron.WithSeconds()`) plus the polling-deadline assertion pattern
  (M2b.5 LESSONS). A future revision could fork robfig's `cron.Cron`
  to accept a `clockwork.Clock` if deterministic tests become a
  blocker.
- **Event-bus topic management** — the caller picks the topic string
  and is responsible for ensuring at least one subscriber exists.
  Publishing to an unsubscribed topic is not an error (the bus
  enqueues + drops on the worker side, see eventbus README).
- **Per-entry locations / time zones** — the location (`WithLocation`)
  applies to ALL entries on a single Scheduler. Callers needing mixed
  time zones build separate `*Scheduler` instances.
