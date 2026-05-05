# capability — scoped-token broker (issue / validate / TTL)

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/capability`

This package provides the **broker primitive** for issuing and validating
opaque scoped capability tokens with TTL. ROADMAP §M3 → M3.5.

The broker mints cryptographically random tokens (via `crypto/rand`),
each bound to a single string scope and an absolute expiry. Callers
later present a token at invocation sites and the broker decides
admit / deny via `Validate`. Two cleanup paths run in parallel: lazy
cleanup on every `Validate`, and an optional background reaper for
issue-heavy / validate-light workloads.

## Public API

```go
type Broker struct{ /* opaque */ }

type Logger interface {
    Log(ctx context.Context, msg string, kv ...any)
}

type Option func(*config)

func New(opts ...Option) *Broker
func WithClock(c func() time.Time) Option
func WithLogger(l Logger) Option
func WithReaperInterval(d time.Duration) Option

func (*Broker) Issue(scope string, ttl time.Duration) (string, error)
func (*Broker) IssueForOrg(scope, organizationID string, ttl time.Duration) (string, error)
func (*Broker) Validate(ctx context.Context, token, scope string) error
func (*Broker) ValidateForOrg(ctx context.Context, token, scope, organizationID string) error
func (*Broker) Revoke(token string) error
func (*Broker) Close() error
```

Sentinel errors live in `errors.go`:

- `ErrClosed` — any method called after `Close`.
- `ErrInvalidScope` — empty scope on `Issue` / `IssueForOrg`.
- `ErrInvalidTTL` — non-positive ttl on `Issue` / `IssueForOrg`.
- `ErrInvalidOrganization` — empty organizationID on `IssueForOrg` /
  `ValidateForOrg`.
- `ErrInvalidToken` — token absent on `Validate` / `ValidateForOrg`.
- `ErrTokenExpired` — token present but `clock() >= expiry`.
- `ErrScopeMismatch` — token present, unexpired, scope mismatch.
- `ErrOrganizationMismatch` — token present, unexpired, scope-matched
  but its registered organizationID does not equal the
  `organizationID` argument.

All matchable via `errors.Is`.

## Quick start

```go
import (
    "context"
    "errors"
    "time"

    "github.com/vadimtrunov/watchkeepers/core/pkg/capability"
)

func wire(ctx context.Context) error {
    b := capability.New(capability.WithReaperInterval(time.Minute))
    defer b.Close()

    tok, err := b.Issue("keep:write", 5*time.Minute)
    if err != nil {
        return err
    }

    // ... pass tok to the caller; later, at the invocation site:

    if err := b.Validate(ctx, tok, "keep:write"); err != nil {
        switch {
        case errors.Is(err, capability.ErrTokenExpired):
            // Caller must request a fresh token.
        case errors.Is(err, capability.ErrScopeMismatch):
            // Caller is using the wrong token for this verb.
        case errors.Is(err, capability.ErrInvalidToken):
            // Token never issued or already revoked.
        default:
            return err
        }
    }
    return nil
}
```

## Per-tenant pinning (M3.5.a)

`IssueForOrg` mints a token bound to BOTH a `pkg:verb` scope AND a
tenant identifier (`organizationID`). `ValidateForOrg` admits the
token only when both dimensions match. The contract is the M3.5.a
cross-tenant guarantee: a token minted for tenant A MUST NOT
validate for tenant B even when the scope matches.

```go
const orgA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
tok, err := b.IssueForOrg("keep:write", orgA, 5*time.Minute)
// ...
if err := b.ValidateForOrg(ctx, tok, "keep:write", orgA); err != nil {
    switch {
    case errors.Is(err, capability.ErrOrganizationMismatch):
        // Token is for a different tenant — reject.
    // ... other ErrTokenExpired / ErrScopeMismatch / ErrInvalidToken.
    }
}
```

**Backward compatibility.** Tokens minted via the legacy `Issue` path
have an empty stored organizationID; presenting them to
`ValidateForOrg` rejects with `ErrOrganizationMismatch`. Tokens minted
via `IssueForOrg` still pass the legacy `Validate` (scope-only)
without the per-tenant check, so callers that haven't adopted the new
validator keep working — adopt the validator first, then migrate the
mint side.

**`auth.Claim` integration.** The keep service's verifier carries
`OrganizationID` on `auth.Claim` (see
`core/internal/keep/auth/auth.go`). Future keep handlers will read the
field from the verified claim and pass it to `ValidateForOrg`,
replacing today's request-body-trust pattern. M3.5.a.2 wires that
through `handleInsertHuman` and `handleSetWatchkeeperLead`; M3.5.a.1
(this milestone) lays the foundation.

## Scope convention: `pkg:verb`

Scopes are opaque strings to the broker — only equality matters. The
project-wide convention is `pkg:verb`: the package being protected
followed by the verb being authorised. Examples:

| Scope                | Meaning                          |
| -------------------- | -------------------------------- |
| `keep:write`         | write to the keepclient backend  |
| `keep:read`          | read from the keepclient backend |
| `notebook:append`    | append to the notebook log       |
| `notebook:read`      | read entries from the notebook   |
| `archivestore:write` | append to an archivestore        |
| `archivestore:read`  | read from an archivestore        |

Single-string equality keeps Phase 1 simple. Richer permission models
(sets, hierarchies, JWT-style claims) are deferred to Phase 2+.

## Token format

Tokens are 32 bytes of cryptographic random material (`crypto/rand`)
encoded as URL-safe base64 with no padding (`base64.RawURLEncoding`),
yielding a 43-character ASCII string with the alphabet
`A-Z a-z 0-9 - _`. Tokens are opaque to callers — do not parse, do
not split, do not assume internal structure.

## Expiry boundary semantics

`now() >= expiry` is treated as expired — i.e. the boundary is
inclusive on the right. A `Validate` at exactly the expiry instant
returns `ErrTokenExpired`; a `Validate` strictly before returns nil
(assuming scope matches). This convention removes the off-by-one
ambiguity at the boundary.

## Lifecycle and cleanup

Expired entries are removed via two complementary mechanisms:

1. **Lazy cleanup on `Validate`** — when a `Validate` call lands on an
   expired entry, the entry is removed before `ErrTokenExpired` is
   returned. The next `Validate` of the same token returns
   `ErrInvalidToken`.
2. **Optional background reaper** — when `WithReaperInterval` is
   supplied with a positive duration the broker spawns a single
   goroutine that wakes every interval, sweeps the map, and removes
   any expired entries. Useful for issue-heavy / validate-light
   workloads where lazy cleanup alone would let expired entries
   accumulate.

The reaper is OFF by default. `Close` stops the reaper goroutine,
drains it, and clears the map.

## Redaction discipline (security contract)

This is a security-sensitive package. Two unconditional rules:

1. **The full 43-character token is NEVER logged.** When a `Logger` is
   wired via `WithLogger`, log entries carry only a `token_prefix`
   field with the first 8 characters of the token. The full token
   value never appears in any log payload, key, or value — not on
   issue, not on validate, not on revoke, not on expiry pruning, not
   on close.
2. **Token bytes never appear in error messages or error values.**
   `ErrInvalidToken`, `ErrTokenExpired`, and `ErrScopeMismatch` are
   bare sentinels. No formatting (`fmt.Errorf("token %s ...", token)`)
   ever wraps the input token bytes. Caller `err.Error()` strings are
   therefore safe to log even when the caller itself is uncareful.

The test `TestBroker_LoggerNeverSeesFullToken` enforces both rules
via `fmt.Sprintf("%+v", entry)` grep on the recording logger, mirroring
the M3.4.a/M3.4.b redaction test pattern documented in
`docs/LESSONS.md`.

### Logger event vocabulary

| Event                               | Fields                                                           |
| ----------------------------------- | ---------------------------------------------------------------- |
| `capability: issued`                | `scope`, `[organization_id]`, `token_prefix`, `expiry` (RFC3339) |
| `capability: validated`             | `scope`, `[organization_id]`, `token_prefix`                     |
| `capability: scope_mismatch`        | `expected_scope`, `[organization_id]`, `token_prefix`            |
| `capability: organization_mismatch` | `scope`, `expected_organization_id`, `token_prefix`              |
| `capability: expired`               | `scope`, `[organization_id]`, `token_prefix`                     |
| `capability: revoked`               | `token_prefix`                                                   |
| `capability: reaper_pruned`         | `scope`, `token_prefix`                                          |
| `capability: closed`                | (no fields)                                                      |

`organization_id` (bracketed in the table) is emitted only by the
per-tenant `IssueForOrg` / `ValidateForOrg` paths — the legacy
`Issue` / `Validate` paths omit it. The full organizationID value
is logged unredacted: it is a tenant identifier, not a secret.

## Functional options

`Broker` is constructed via `New` with zero or more `Option` values.
Unlike `cron.New` / `lifecycle.New`, **no required dependency exists**;
`New()` (no args) is a fully functional broker that uses `time.Now`,
drops diagnostics, and runs only the lazy-cleanup path.

```go
// Defaults: time.Now, no logger, no reaper.
b := capability.New()

// With a deterministic clock (test).
b := capability.New(capability.WithClock(fakeClock.Now))

// With a structured logger.
b := capability.New(capability.WithLogger(myLogger))

// With a background reaper sweeping every minute.
b := capability.New(capability.WithReaperInterval(time.Minute))
```

`WithClock` and `WithLogger` accept nil arguments as no-ops.
`WithReaperInterval` treats non-positive durations as "disabled."

## Concurrency

`*Broker` is safe for concurrent use. Reads use an `sync.RWMutex`
RLock; mutations and the expiry-delete path acquire a write lock.
The reaper goroutine acquires the same write lock for its sweep.

## Deferred integrations

The following are out of scope for M3.5 and tracked as TODO for a
follow-up TASK:

- **Wiring into `keepclient`**: a future TASK will plumb a
  `*Broker` into `lifecycle.New` and require a valid `keep:write` /
  `keep:read` token on every `KeepClient` method call. Deferred until
  a real harness consumer (post-M5) exists; speculative wiring would
  balloon the M3.5 diff.
- **Wiring into `notebook`**: same shape, scopes `notebook:append` /
  `notebook:read`.
- **Wiring into `archivestore`**: same shape, scopes
  `archivestore:write` / `archivestore:read`.

A successor TASK will pick up these integrations once the harness
consumer's needs are concrete.

## Out of scope (deferred)

- **Signed / JWT-style self-contained tokens** — a Phase 2+
  multi-host concern. Phase 1 is single-process so an in-memory map
  suffices.
- **Cross-process token distribution and revocation broadcast** —
  Phase 2+ multi-host concern.
- **Rich permission models** (sets, hierarchies, JWT claims). The
  single `pkg:verb` string is sufficient for Phase 1.
- **HMAC key rotation** — irrelevant to the in-memory random-token
  design; relevant only if/when signed tokens land.
- **Per-token rate limiting** — orthogonal to the broker;
  callers compose rate limiting on top.

## See also

- `docs/ROADMAP-phase1.md` §M3 → M3.5 — milestone scope and acceptance.
- `docs/LESSONS.md` — M3.4.a/M3.4.b redaction test pattern, M2b.5
  polling-deadline pattern.
- `core/pkg/secrets/` — sibling security-sensitive package; same
  Logger interface shape and redaction discipline.
- `core/pkg/cron/` — Close/reaper-goroutine pattern reference.
