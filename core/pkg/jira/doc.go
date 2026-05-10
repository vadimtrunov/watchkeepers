// Package jira is the Atlassian Cloud REST API adapter for the
// Coordinator Watchkeeper role's outbound integrations. ROADMAP §M8 →
// M8.1.
//
// # Scope (M8.1)
//
// The package ships a stdlib-only HTTP client carrying four operations
// against Atlassian Cloud's REST API v3:
//
//   - [Client.Search] — JQL search via `POST /rest/api/3/search/jql`
//     with cursor pagination ([SearchResult.NextPageToken]).
//   - [Client.GetIssue] — issue read via
//     `GET /rest/api/3/issue/{key}?fields=…`.
//   - [Client.AddComment] — comment append via
//     `POST /rest/api/3/issue/{key}/comment`. The caller supplies plain
//     text; the adapter wraps it in a minimal Atlassian Document Format
//     (ADF) envelope on the wire.
//   - [Client.UpdateFields] — whitelist-checked field writes via
//     `PUT /rest/api/3/issue/{key}`. The whitelist is configured at
//     [NewClient] time via [WithFieldWhitelist]; a nil/empty whitelist
//     refuses ALL writes (fail-closed default).
//
// All four operations layer on the unexported `do` transport which
// carries the auth (Atlassian Cloud HTTP Basic, email + API token),
// JSON encoding/decoding, error-envelope decoding, structured-metadata-
// only logging, and `Retry-After`-aware 429 surfacing. The five
// sentinel errors ([ErrInvalidAuth], [ErrIssueNotFound],
// [ErrFieldNotWhitelisted], [ErrInvalidJQL], [ErrRateLimited]) cover
// the failure modes M8.2 callers will need to discriminate; statuses
// outside this set surface as [*APIError] for callers that need raw
// status-code access.
//
// # Design choices
//
// **Stdlib-only.** Like the slack adapter, this package depends only
// on the Go standard library. The wider Go ecosystem ships several
// Atlassian SDKs (`go-jira`, `andygrunwald/go-jira`, …); each carries
// its own concurrency / retry / JSON-shape opinions that conflict with
// the keepclient + slack discipline this codebase has settled on. A
// thin self-rolled client keeps the dependency surface small, the
// tests fast, and the code shape uniform across every external
// integration. Mirrors the keepclient discipline (M2.8.a).
//
// **REST API v3, ADF for comments.** Atlassian publishes two REST
// versions for Cloud — v2 (legacy, deprecated for several sub-APIs,
// plain-text comments) and v3 (canonical, ADF-encoded comments).
// M8.1 ships v3 because Atlassian's own documentation and forward-
// compatibility guidance both point at v3; v2 sub-APIs have been
// flagged for sunset multiple times. The ADF conversion is a small
// price: plain text in / plain text out, with a documented lossy round-
// trip for complex blocks (mentions, code blocks, tables) — the
// Coordinator's tools produce plain text and consume plain text, so
// the lossy projection is acceptable for the M8.1 scope.
//
// **Whitelist as constructor option, fail-closed default.**
// [WithFieldWhitelist] supplies the closed set of field IDs the
// client will write. A client constructed without [WithFieldWhitelist]
// refuses ALL [Client.UpdateFields] calls with [ErrFieldNotWhitelisted]
// — read operations work normally. The whitelist is enforced
// synchronously, BEFORE the network exchange; an attempted write to a
// non-whitelisted field never crosses the wire. The check is deliberately
// at the adapter layer rather than at the M8.2 tool layer because the
// adapter is the platform-facing security boundary; the per-role
// authority matrix M8.2 introduces sits ON TOP of, not INSTEAD of, this
// transport-level guard.
//
// **HTTP 429 — propagate, do not auto-retry.** Atlassian publishes the
// Retry-After budget on 429 responses; the client surfaces it through
// [APIError.RetryAfter] and wraps to [ErrRateLimited]. Auto-retry
// would multiply request complexity (idempotency, ctx interactions)
// without a clear win for M8.2's bounded request rate — daily-briefing
// + occasional reviewer nudges sit far below Atlassian's per-tenant
// budget. Callers that want retry wrap the public methods explicitly
// (e.g. retry [Client.Search] on `errors.Is(err, ErrRateLimited)`
// after waiting [APIError.RetryAfter]).
//
// **No rate limiter in M8.1.** Atlassian Cloud does not publish per-
// method rate tiers (Slack does — that drove the slack-package
// per-tier limiter); the documented per-tenant 5-minute window is a
// soft ceiling. M8.2's tools poll on cron at human-scale frequency.
// A dedicated limiter is a future addition if M8.2's traffic pattern
// proves it necessary.
//
// # Concurrency
//
// [Client] is safe for concurrent use across goroutines once
// constructed via [NewClient]. Configuration is immutable; the
// underlying *http.Client and [BasicAuthSource] MUST themselves be
// concurrent-safe (the package's defaults are; tests substituting an
// httptest.Server and a [StaticBasicAuth] satisfy this trivially).
//
// # Redaction discipline
//
// The [Client]'s optional [Logger] (configured via [WithLogger]) NEVER
// receives the email, the API token, the Authorization header, the
// request body, or the Atlassian response body. Only structured
// metadata (HTTP method, endpoint path, status, error class, first
// error message capped at 200 bytes, Retry-After value) appears in
// log entries. Mirrors the M3.4.b / M3.5 / M3.6 / M3.7 redaction
// patterns documented in `docs/LESSONS.md`.
//
// # See also
//
//   - `core/pkg/messenger/slack` — sibling stdlib-only adapter whose
//     functional-option / sentinel-error / Logger patterns this package
//     mirrors.
//   - `core/pkg/keepclient` — the older sibling that established the
//     "thin self-rolled HTTP client over JSON" discipline.
//   - `docs/ROADMAP-phase1.md` §M8 → M8.1.
//   - https://developer.atlassian.com/cloud/jira/platform/rest/v3/ —
//     Atlassian's REST API v3 reference.
//   - https://developer.atlassian.com/cloud/jira/platform/apis/document/structure/ —
//     ADF (Atlassian Document Format) reference used by [Client.AddComment].
package jira
