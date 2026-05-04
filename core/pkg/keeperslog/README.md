# keeperslog — structured Keeper's Log writer

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog`

This package is a thin wrapper around `keepclient.LogAppend` that adds
the cross-cutting concerns Phase 1 needs at every audit-event call site:

- a stable JSON envelope schema (`event_id`, `timestamp`, optional
  `trace_id` / `span_id` / `causation_id`, optional caller `data`),
- correlation-ID resolution from `context.Context` with auto-generation
  on absence,
- OpenTelemetry trace-context propagation via
  `trace.SpanContextFromContext`.

ROADMAP §M3 → M3.6.

## Public API

```go
type Writer struct{ /* opaque */ }

type LocalKeepClient interface {
    LogAppend(ctx context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error)
}

type Logger interface {
    Log(ctx context.Context, msg string, kv ...any)
}

type IDGenerator func() (string, error)

type Event struct {
    EventType   string
    CausationID string
    Payload     any
}

type Option func(*config)

func New(client LocalKeepClient, opts ...Option) *Writer
func WithClock(c func() time.Time) Option
func WithLogger(l Logger) Option
func WithIDGenerator(g IDGenerator) Option
func WithCorrelationIDGenerator(g IDGenerator) Option

func (*Writer) Append(ctx context.Context, event Event) (string, error)

func ContextWithCorrelationID(ctx context.Context, id string) context.Context
func CorrelationIDFromContext(ctx context.Context) (string, bool)
```

The single sentinel error lives in `errors.go`:

- `ErrInvalidEvent` — `Event.EventType == ""` on `Append`. Matchable
  via `errors.Is`. The keepclient is NOT contacted when this sentinel is
  returned; underlying transport / server errors surface as
  `fmt.Errorf("keeperslog: append: %w", err)` and remain matchable
  against `keepclient.ErrUnauthorized` etc. through the wrap chain.

## Quick start

```go
import (
    "context"
    "time"

    "github.com/google/uuid"
    "github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
    "github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

func wire(ctx context.Context, kc *keepclient.Client) error {
    w := keeperslog.New(kc)

    // Stamp a chain-origin correlation id once on the inbound ctx.
    correlation, _ := uuid.NewV7()
    ctx = keeperslog.ContextWithCorrelationID(ctx, correlation.String())

    _, err := w.Append(ctx, keeperslog.Event{
        EventType: "watchkeeper_spawned",
        Payload: map[string]any{
            "watchkeeper_id": "wk-7",
            "manifest_id":    "mf-3",
        },
    })
    return err
}
```

## Envelope schema

The JSON shape persisted to `keepers_log.payload`:

```jsonc
{
  "event_id": "<uuid-v7>", // always present
  "timestamp": "<RFC3339Nano UTC>", // always present
  "trace_id": "<32 hex chars>", // only when ctx has a valid OTel span
  "span_id": "<16 hex chars>", // only when ctx has a valid OTel span
  "causation_id": "<opaque string>", // only when Event.CausationID != ""
  "data": {
    /* caller's payload */
  }, // only when Event.Payload != nil
}
```

`event_type`, `correlation_id`, and the row's UUID + `created_at` are
columns on the `keepers_log` row, not envelope fields. The server stamps
`actor_*` and `scope` from the verified capability claim.

### What is NOT in the envelope

The M2 design constraint forbids infrastructure-metadata fields in
`keepers_log` rows. The envelope keys are constrained to the schema
above; **never** add `deployment_id`, `environment`, `host`, `pod`, or
similar infrastructure descriptors. Multi-environment isolation is
achieved by running separate Keep instances. This is enforced by code
review, not by the writer (the `Event.Payload` surface is opaque).

## Correlation-ID resolution

Resolution order on every `Append`:

1. ctx-stored value (from `ContextWithCorrelationID`) wins.
2. Otherwise the writer mints a fresh UUID v7 via
   `WithCorrelationIDGenerator` (default: `uuid.NewV7`).

Generated correlation ids are NOT pushed back onto the ctx — the chain
origin (cron fire, Slack interaction, watchkeeper boot) owns the id and
should call `ContextWithCorrelationID` once at the top of its handler.
The empty-string passthrough on `ContextWithCorrelationID` makes it
safe to call unconditionally.

## Trace-context propagation

The writer reads the OTel span context via
`trace.SpanContextFromContext`. When the span context is valid the
lower-case-hex `trace_id` and `span_id` are embedded into the envelope.
When the span context is unset/invalid those fields are OMITTED from
the JSON payload — no empty strings on the wire. This is a
vendor-neutral integration: any tracer wired into the Go process that
propagates through `context.Context` (OTel, Jaeger via OTel bridge,
etc.) flows through transparently.

If no tracer is configured at all, the writer behaves identically — the
default `trace.SpanContext` is invalid and the trace fields are
omitted. There is no requirement to wire OpenTelemetry just to use
`keeperslog`.

## Functional options

```go
// Defaults: time.Now, no logger, uuid.NewV7 for both id generators.
w := keeperslog.New(kc)

// With a deterministic clock (test).
w := keeperslog.New(kc, keeperslog.WithClock(fakeClock.Now))

// With a structured logger.
w := keeperslog.New(kc, keeperslog.WithLogger(myLogger))

// With deterministic id generators (test).
w := keeperslog.New(
    kc,
    keeperslog.WithIDGenerator(func() (string, error) { return "evt-1", nil }),
    keeperslog.WithCorrelationIDGenerator(func() (string, error) { return "corr-1", nil }),
)
```

`WithClock`, `WithLogger`, `WithIDGenerator`, and
`WithCorrelationIDGenerator` accept nil arguments as no-ops — callers
can always pass through whatever they have.

## Logger event vocabulary

| Event                       | Fields                                                 |
| --------------------------- | ------------------------------------------------------ |
| `keeperslog: appended`      | `event_type`, `correlation_id`, `event_id`, `row_id`   |
| `keeperslog: append failed` | `event_type`, `correlation_id`, `event_id`, `err_type` |

**Redaction discipline**: the logger NEVER sees `Event.Payload`. Only
metadata (`event_type`, `correlation_id`, `event_id`, `row_id`,
`err_type`) is logged. Failures log only the error TYPE
(`fmt.Sprintf("%T", err)`), never `err.Error()` — the error TYPE is
provably non-sensitive; the value may contain arbitrary upstream text.
Mirrors the M3.4.b config-loader and M3.5 capability-broker redaction
patterns documented in `docs/LESSONS.md`.

## Concurrency

`*Writer` is safe for concurrent use. The writer holds only immutable
configuration after `New` returns; per-call state lives on the
goroutine stack. Concurrency at the keepclient layer is governed by
the underlying `*http.Client`.

## Out of scope (deferred)

- **Capability-token wiring** — the writer consumes whatever
  `LocalKeepClient` the caller hands in. Token issuance (M3.5) and the
  per-call `keep:write` validation are deferred to the M5 harness
  consumer where call sites are concrete.
- **Batched / buffered appends** — every `Append` forwards a single
  keepclient call. Backpressure is the caller's concern; callers
  needing throughput build a buffered channel + worker on top.
- **Cross-process correlation-id distribution** — Phase 1 is
  single-process; cross-process correlation flows through the OTel
  trace-context propagation already covered above.
- **Outbox-consumer integration** — M3.7 ships separately; the writer
  does not (yet) emit outbox-friendly idempotency keys.

## See also

- `docs/ROADMAP-phase1.md` §M3 → M3.6 — milestone scope and acceptance.
- `docs/LESSONS.md` — M3.4.b/M3.5 redaction discipline, M3.2.b
  `LocalKeepClient` import-cycle-break pattern.
- `core/pkg/keepclient/` — underlying transport client.
- `core/pkg/capability/` — sibling security primitive; future
  integration target for `keep:write` token validation.
- `core/pkg/cron/`, `core/pkg/lifecycle/` — sibling M3 packages
  using the same `LocalX` interface + functional-options shape.
