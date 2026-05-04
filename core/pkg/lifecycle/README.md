# lifecycle — logical Watchkeeper Spawn / Retire / Health / List

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/lifecycle`

This package owns the logical-bookkeeping side of an agent's lifecycle.
It is a thin orchestration layer over the four watchkeeper CRUD methods
exposed by `core/pkg/keepclient` (M3.2.a). No SQL, no HTTP, no
goroutine of its own — every state-changing call is one (or two, in
`Spawn`'s case) keepclient round-trip(s).

## Public API

| Method                                                          | Purpose                                                                                                                                                                                                               | AC  |
| --------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --- |
| `New(client LocalKeepClient, opts ...Option) *Manager`          | Construct a `*Manager`. `client` is required; nil panics. Options apply on top of the defaults `clock = time.Now`, `logger = nil`.                                                                                    | AC3 |
| `Spawn(ctx, params SpawnParams) (id string, err error)`         | Two-step `InsertWatchkeeper` (status='pending') → `UpdateWatchkeeperStatus("active")`. Returns `(id, nil)` on success; partial-failure shape on Update failure (`(id, err)`) so the caller can retry just the Update. | AC4 |
| `Retire(ctx, id string) error`                                  | Passthrough to `UpdateWatchkeeperStatus(id, "retired")`. Wraps any error as `lifecycle: retire: %w`.                                                                                                                  | AC5 |
| `Health(ctx, id string) (*Status, error)`                       | `GetWatchkeeper` + projection into a slim `Status` (drops `ActiveManifestVersionID`).                                                                                                                                 | AC6 |
| `List(ctx, filter ListFilter) ([]*keepclient.Watchkeeper, err)` | Passthrough to `ListWatchkeepers` after a lockstep `Limit ∈ [0, 200]` bound check.                                                                                                                                    | AC7 |

Functional options:

- `WithLogger(l Logger)` — wires an audit-emit sink. Reserved for a
  future `watchkeeper_spawned` / `watchkeeper_retired` audit hook;
  current methods do not call it. Nil is a no-op.
- `WithClock(c func() time.Time)` — overrides the wall-clock source
  used by future audit-emit timestamps. Defaults to `time.Now`. Nil
  is a no-op.

`*keepclient.Client` satisfies both `LocalKeepClient` and `Logger`
structurally — wire your existing client straight in.

## Sentinels and error matching

`errors.go` exports a single sentinel:

- `ErrInvalidParams` — empty required `id` / `ManifestID` /
  `LeadHumanID`, or out-of-range `ListFilter.Limit`. Returned
  synchronously without a network round-trip.

The package does NOT re-export keepclient's sentinels. All keepclient
errors flow through `fmt.Errorf("…: %w", err)` so callers match them
directly with `errors.Is` against the keepclient symbol:

```go
if errors.Is(err, keepclient.ErrNotFound) { ... }
if errors.Is(err, keepclient.ErrInvalidStatusTransition) { ... }
```

The `lifecycle: spawn:` / `lifecycle: retire:` / `lifecycle: health:` /
`lifecycle: list:` prefixes are stable and useful for log scraping.

## Partial-failure contract (`Spawn`)

`Spawn` performs two keepclient calls in sequence:

1. `InsertWatchkeeper` creates a row with status='pending'.
2. `UpdateWatchkeeperStatus(id, "active")` transitions it to active.

Three outcomes:

- **All success** → `(id, nil)`.
- **Insert fails** → `("", fmt.Errorf("lifecycle: spawn: insert: %w",
err))`. No row exists; retry the whole call.
- **Update fails** → `(id, fmt.Errorf("lifecycle: spawn: activate:
%w", err))`. The row IS in the database in `pending` state, so the
  caller can retry just `Manager.Retire`-then-Update or
  `keepclient.UpdateWatchkeeperStatus(id, "active")` against the
  populated id without re-running the Insert.

This shape mirrors M2b.4 (`importPayload` / spool-then-rename) and
M2b.7 (mutation-audit `(id, err)` on LogAppend failure): when an
operation has multiple sub-steps, return `(populated_value, err)` so
the retry surface stays tight.

## LocalKeepClient interface (one-way decoupling)

The package depends on a local `LocalKeepClient` interface that
mirrors the four keepclient methods Spawn / Retire / Health / List
consume. Production code in this package never imports keepclient's
concrete `*Client` type; only the tests do, for the compile-time
assertion:

```go
var _ LocalKeepClient = (*keepclient.Client)(nil)
```

This mirrors the M2b.6 cross-package compile-time-check pattern
documented in `docs/LESSONS.md`: keepclient stays a one-way
dependency from production callers, the lifecycle production code
sees only the interface, and any future keepclient method-rename
breaks the assertion at the same compile step the production wiring
breaks at — no later.

## Concurrency

`*Manager` holds no shared mutable state beyond the immutable client
pointer + logger + clock. Concurrent calls to any method on a single
`*Manager` are independent at the lifecycle layer; the underlying
`LocalKeepClient` (`*keepclient.Client`'s `*http.Client` is safe for
concurrent use) governs request-level concurrency. The
`TestManager_ConcurrentSpawnsAreIndependent` test pins the invariant
under `go test -race`.

## Out of scope (deferred)

- **Process supervision** (`exec.Command`, child-process restart
  loops, signal forwarding) — M5.3.
- **Cron-driven lifecycle events** — M3.3 wires the cron primitives
  onto the same keepclient surface; lifecycle stays uninvolved.
- **Heartbeat publishing / external Health probing** — M3.6 owns the
  liveness side. `Health` here is a row read, not a probe.
- **Cross-host distribution / leader election** — Phase 1 is single
  host; multi-host coordination is out of Phase 1 entirely.
- **RLS on the `watchkeeper` table** — M3.2.a documented this as a
  known gap; lifecycle does not paper over it.
- **Audit emit** (`watchkeeper_spawned` / `watchkeeper_retired`
  events on Keeper's Log) — `WithLogger` reserves the seam; a
  follow-up milestone wires the actual emit.

## Example wiring

```go
import (
    "context"
    "net/http"

    "github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
    "github.com/vadimtrunov/watchkeepers/core/pkg/lifecycle"
)

func wire(ctx context.Context, ts keepclient.TokenSource) (*lifecycle.Manager, error) {
    kc, err := keepclient.New(
        keepclient.WithBaseURL("https://keep.example.com"),
        keepclient.WithHTTPClient(http.DefaultClient),
        keepclient.WithTokenSource(ts),
    )
    if err != nil {
        return nil, err
    }
    return lifecycle.New(kc), nil
}
```
