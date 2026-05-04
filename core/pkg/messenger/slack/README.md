# messenger/slack — Slack adapter for the portable messenger interface

Module: `github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack`

This package will become the Slack implementation of the portable
[`messenger.Adapter`](../README.md) interface defined in M4.1.
ROADMAP §M4 → M4.2.

## Scope (M4.2.a — foundation only)

This iteration ships the **foundation** other M4.2 sub-bullets build
on. The package today is intentionally narrow:

- `Client` — low-level HTTP wrapper around Slack Web API
  (`https://slack.com/api/<method>`) with bearer-token auth, JSON
  body encoding, Slack envelope decoding (`{"ok": ..., "error": ...}`),
  and `Retry-After`-aware HTTP 429 handling. Single public method:
  `Client.Do(ctx, method, params, out)`.
- `RateLimiter` — tier-aware token-bucket throttle that respects
  Slack's per-method tier-1..tier-4 budgets. `Wait(ctx, method)` is
  the blocking primitive; `Allow(method)` is the non-blocking probe.
- Sentinel errors (`ErrRateLimited`, `ErrInvalidAuth`,
  `ErrTokenExpired`, `ErrChannelNotFound`, `ErrUserNotFound`,
  `ErrAppNotFound`, `ErrUnknownMethod`) plus the `*APIError` envelope
  type. Match with `errors.Is`.

### What this package does NOT yet do

Deferred to later M4.2 sub-bullets:

| Sub-bullet | Scope                                                                                  |
| ---------- | -------------------------------------------------------------------------------------- |
| **M4.2.b** | `SendMessage` / `SetBotProfile` (`chat.postMessage`, `users.profile.set`, `bots.info`) |
| **M4.2.c** | `Subscribe` via Socket Mode (WebSocket event intake)                                   |
| **M4.2.d** | `CreateApp` / `InstallApp` (Slack Manifest API + OAuth install flow)                   |

`Client` therefore does NOT yet implement `messenger.Adapter`. The
compile-time `var _ messenger.Adapter = (*Adapter)(nil)` assertion
lands in M4.2.d once all six adapter methods exist.

## Public API

```go
// Construction.
func NewClient(opts ...ClientOption) *Client
func NewRateLimiter(opts ...RateLimiterOption) *RateLimiter

// ClientOption.
func WithBaseURL(raw string) ClientOption
func WithHTTPClient(hc *http.Client) ClientOption
func WithTokenSource(ts TokenSource) ClientOption
func WithRateLimiter(rl *RateLimiter) ClientOption
func WithLogger(l Logger) ClientOption
func WithClock(c func() time.Time) ClientOption

// RateLimiterOption.
func WithTierLimit(tier Tier, limit TierLimit) RateLimiterOption
func WithMethodTier(method string, tier Tier) RateLimiterOption
func WithRateLimiterClock(c func() time.Time) RateLimiterOption

// Operation.
func (c *Client) Do(ctx context.Context, method string, params, out any) error
func (rl *RateLimiter) Wait(ctx context.Context, method string) error
func (rl *RateLimiter) Allow(method string) bool
func (rl *RateLimiter) Tier(method string) Tier
```

Sentinel errors (matchable via `errors.Is`):

- `ErrRateLimited` — limiter rejected OR Slack returned 429 / `error: "ratelimited"`.
- `ErrInvalidAuth` — `error: "invalid_auth" | "not_authed"`, or no token source configured.
- `ErrTokenExpired` — `error: "token_expired"`.
- `ErrChannelNotFound` — `error: "channel_not_found"`.
- `ErrUserNotFound` — `error: "user_not_found"`.
- `ErrAppNotFound` — `error: "app_not_found"`.
- `ErrUnknownMethod` — empty method name supplied to `Client.Do` or `RateLimiter.Wait`.

`APIError` carries the parsed envelope on every Slack-side failure
(`Status`, `Code`, `Method`, `RetryAfter`). Match with `errors.As`
to inspect the parsed body.

## Quick start

```go
import (
    "context"
    "net/http"
    "time"

    "github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
)

func ping(ctx context.Context) error {
    rl := slack.NewRateLimiter()
    c := slack.NewClient(
        slack.WithTokenSource(slack.StaticToken("xoxb-...")),
        slack.WithRateLimiter(rl),
        slack.WithHTTPClient(&http.Client{Timeout: 10 * time.Second}),
    )
    var resp struct {
        OK   bool   `json:"ok"`
        URL  string `json:"url"`
        User string `json:"user"`
    }
    return c.Do(ctx, "auth.test", nil, &resp)
}
```

## Design choices

### Rate limiter — per-tier buckets, not per-method

Slack publishes per-method limits keyed off **four tiers**
(<https://api.slack.com/apis/rate-limits>):

| Tier   | Sustained (req/min) | Example methods                                       |
| ------ | ------------------- | ----------------------------------------------------- |
| tier-1 | ~1                  | `apps.connections.open`                               |
| tier-2 | ~20                 | `users.list`, `apps.manifest.create`                  |
| tier-3 | ~50                 | `chat.update`, `users.profile.set`, `auth.test`       |
| tier-4 | ~100                | `chat.postMessage`, `users.info`, `users.profile.get` |

A per-method bucket would multiply bookkeeping by ~200 endpoints
without observable benefit — methods within a tier share the same
effective budget on a per-app + per-team basis. The limiter holds
one bucket per tier and looks up each method's tier via a small
registry. Methods absent from the registry default to **tier-3**
(Slack's documented fallback for unclassified Web API methods).

Callers can override individual mappings via `WithMethodTier(name, tier)`
and adjust per-tier budgets via `WithTierLimit(tier, TierLimit{...})`.

### HTTP 429 — propagate, do not auto-retry

When the limiter has granted a token and Slack still answers
`429 Too Many Requests` (e.g. server-side burst-capacity drift,
multi-replica deploy), `Client.Do` returns `*APIError` wrapping
`ErrRateLimited`, with `RetryAfter` populated from the response
header. The rate limiter is the **primary throttle**; 429 is the
**safety net**.

Auto-retry would multiply request complexity (timing, idempotency,
ctx interactions) without a clear win for M4.2.b's needs —
`chat.postMessage` is not idempotent without `client_msg_id`, which
the **caller** manages, not the transport. Callers that want retry
wrap `Client.Do` explicitly.

### Stdlib-only

Depends only on the Go standard library plus the in-repo
`messenger` parent package. No third-party Slack SDK. Mirrors the
keepclient discipline (M2.8.a): a thin self-rolled client keeps
dependency surface small and avoids inheriting an SDK's concurrency
/ retry / backoff opinions that conflict with our own.

## Concurrency

`Client` and `RateLimiter` are safe for concurrent use across
goroutines once constructed. The limiter's per-tier bucket is
internally mutex-locked; the client carries only immutable
configuration after `NewClient` returns. The full test suite runs
under `-race`.

## Redaction discipline

The `Client`'s optional `Logger` (configured via `WithLogger`)
**NEVER** receives:

- the bearer token (or any `Authorization` header value);
- the request body;
- the Slack response body.

Only structured metadata appears in log entries:

| Event                         | Fields                                    |
| ----------------------------- | ----------------------------------------- |
| `slack: request begin`        | `method`                                  |
| `slack: request ok`           | `method`, `status`                        |
| `slack: request failed`       | `method`, `err_type`                      |
| `slack: api error`            | `method`, `status`, `code`                |
| `slack: http error`           | `method`, `status`, `code`                |
| `slack: rate limited`         | `method`, `status`, `retry_after_seconds` |
| `slack: token resolve failed` | `method`, `err_type`                      |

`err_type` carries `fmt.Sprintf("%T", err)` — provably non-sensitive.
Mirrors the M3.4.b / M3.5 / M3.6 / M3.7 redaction patterns
documented in `docs/LESSONS.md`.

## Running the tests

```bash
# Full suite under race detector (no real Slack workspace required —
# every test uses httptest.Server fakes).
go test -race -count=1 ./core/pkg/messenger/slack/...

# Verbose listing, useful when adding new test cases.
go test -race -count=1 -v ./core/pkg/messenger/slack/...
```

The package has **no real Slack workspace dependency** — every test
talks to an `httptest.Server` whose handler emulates the documented
Slack response shapes. Verification of the live behaviour
(ROADMAP §M4.2 verification bullet about `make spawn-dev-bot`) waits
on the dev workspace external prerequisite + M4.2.b/c/d adapter
methods.

## See also

- `core/pkg/messenger` — the portable adapter interface this package
  will satisfy once M4.2.b/c/d land.
- `core/pkg/keepclient` — sibling stdlib-only HTTP client whose
  functional-option / sentinel-error patterns this package mirrors.
- `docs/ROADMAP-phase1.md` §M4 → M4.2.
- <https://api.slack.com/apis/rate-limits> — Slack's published
  per-method tier mapping.
