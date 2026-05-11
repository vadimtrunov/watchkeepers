// Package github is the GitHub REST API adapter for the Coordinator
// Watchkeeper role's outbound integrations. ROADMAP §M8 → M8.2.d.
//
// # Scope (M8.2.d)
//
// The package ships a stdlib-only HTTP client carrying one read
// operation against GitHub's REST API:
//
//   - [Client.ListPullRequests] — list pull requests on a repository
//     via `GET /repos/{owner}/{repo}/pulls` with page-number
//     pagination ([ListPullRequestsResult.NextPage]).
//
// The single operation layers on the unexported `do` transport which
// carries the auth (GitHub bearer token via the `Authorization:
// Bearer …` header), JSON decoding, error-envelope decoding,
// structured-metadata-only logging, and rate-limit aware status
// surfacing. The four sentinel errors ([ErrInvalidAuth],
// [ErrRepoNotFound], [ErrInvalidArgs], [ErrRateLimited]) cover the
// failure modes M8.2.d callers will need to discriminate; statuses
// outside this set surface as [*APIError] for callers that need raw
// status-code access.
//
// # Design choices
//
// **Stdlib-only.** Like the jira and slack adapters, this package
// depends only on the Go standard library. The wider Go ecosystem
// ships several GitHub SDKs (`go-github`, etc.); each carries its own
// concurrency / retry / JSON-shape opinions that conflict with the
// jira + slack discipline this codebase has settled on. A thin
// self-rolled client keeps the dependency surface small, the tests
// fast, and the code shape uniform across every external integration.
// Mirrors the M8.1 jira-adapter discipline.
//
// **Default base URL = `https://api.github.com`; [WithBaseURL]
// overrides for GitHub Enterprise Server.** Unlike Atlassian Cloud
// (where every tenant has its own subdomain), GitHub.com has a single
// canonical API host; GitHub Enterprise Server uses
// `https://<your-ghes-host>/api/v3`. The default suits the public-
// cloud path; GHES operators pass [WithBaseURL]. Non-empty path
// prefixes ARE accepted (unlike jira) because GHES requires the
// `/api/v3` prefix.
//
// **Bearer token via [TokenSource], per-call resolver.** GitHub
// supports both bearer auth (`Authorization: Bearer <token>`,
// canonical for PATs + GitHub Apps) and basic auth (legacy, no
// longer recommended). The adapter ships bearer-only via the
// per-call [TokenSource] resolver shape — mirrors `slack.TokenSource`
// and `jira.BasicAuthSource`. Production wiring backs the resolver
// with the M3.4.b secrets interface so token rotation is observable.
// [WithTokenSource] is REQUIRED at [NewClient] time — fail-closed
// default.
//
// **HTTP 429 + 403 (with `x-ratelimit-remaining: 0`) → propagate,
// do not auto-retry.** GitHub publishes rate-limit budgets via
// `X-RateLimit-Reset` (Unix-epoch seconds, NOT Retry-After). The
// client computes `RetryAfter` from `X-RateLimit-Reset` minus the
// configured clock, surfaces both 429 and the documented 403-with-
// `x-ratelimit-remaining: 0` shape via [ErrRateLimited]. Auto-retry
// would multiply request complexity (idempotency, ctx interactions)
// without a clear win for M8.2.d's bounded request rate. Callers
// that want retry wrap [Client.ListPullRequests] explicitly.
//
// **No rate limiter in M8.2.d.** GitHub's documented per-token 5000
// req/h budget is a soft ceiling far above M8.2.d's daily-briefing
// + occasional reviewer-nudge polling. A dedicated limiter is a
// future addition if traffic pattern proves it necessary.
//
// # Concurrency
//
// [Client] is safe for concurrent use across goroutines once
// constructed via [NewClient]. Configuration is immutable; the
// underlying *http.Client and [TokenSource] MUST themselves be
// concurrent-safe (the package's defaults are; tests substituting an
// httptest.Server and a [StaticToken] satisfy this trivially).
//
// # Redaction discipline
//
// The [Client]'s optional [Logger] (configured via [WithLogger]) NEVER
// receives the token, the Authorization header, the request body, or
// the GitHub response body. Only structured metadata (HTTP method,
// endpoint path, status, error class, X-RateLimit-Reset value)
// appears in log entries. Mirrors the M3.4.b / M3.5 / M8.1 redaction
// patterns documented in `docs/LESSONS.md`.
//
// # See also
//
//   - `core/pkg/jira` — sibling stdlib-only adapter whose functional-
//     option / sentinel-error / Logger patterns this package mirrors.
//   - `core/pkg/messenger/slack` — the older sibling that established
//     the [TokenSource] resolver shape.
//   - `docs/ROADMAP-phase1.md` §M8 → M8.2 → M8.2.d.
//   - https://docs.github.com/en/rest — GitHub's REST API reference.
//   - https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api —
//     rate-limit header reference.
package github
