// Package slack is the Slack adapter for the portable
// [github.com/vadimtrunov/watchkeepers/core/pkg/messenger.Adapter]
// interface. ROADMAP §M4 → M4.2.
//
// # Scope (M4.2.a — foundation only)
//
// This iteration ships the FOUNDATION other M4.2 sub-bullets build on:
//
//   - A tier-aware [RateLimiter] that respects Slack's per-method
//     burst+sustained budgets (tier-1 .. tier-4 — see
//     https://api.slack.com/apis/rate-limits).
//   - A low-level [Client] HTTP wrapper around Slack Web API (`https://
//     slack.com/api/<method>`) with bearer-token auth, JSON encoding,
//     Slack `{ok: false, error: "..."}` envelope decoding, and
//     `Retry-After`-aware HTTP 429 handling.
//   - A sentinel-error vocabulary mapping common Slack `error` codes
//     onto matchable [errors.Is] sentinels.
//
// What this package does NOT yet do (deferred to later M4.2 sub-bullets):
//
//   - M4.2.b — `SendMessage` / `SetBotProfile` (`chat.postMessage`,
//     `users.profile.set`, `bots.info`).
//   - M4.2.c — `Subscribe` via Socket Mode (WebSocket event intake).
//   - M4.2.d — `CreateApp` / `InstallApp` (Slack Manifest API + OAuth
//     install flow).
//
// The [Client] therefore does NOT yet implement
// [messenger.Adapter] — the compile-time assertion lands in M4.2.d once
// all six methods exist. M4.2.b/c/d build their adapter methods on top
// of [Client.Do] and the [RateLimiter].
//
// # Design choices
//
// **Rate limiter — per-tier buckets, not per-method.** Slack publishes
// per-method limits keyed off four tiers (tier-1: 1 req/min sustained;
// tier-2: 20/min; tier-3: 50/min; tier-4: 100/min). A per-method bucket
// would multiply the bookkeeping by ~200 endpoints with no observable
// benefit — methods within a tier share the same effective budget on a
// per-app+per-team basis. The limiter therefore holds one bucket per
// tier; callers pass the method name and the limiter looks up its tier
// via a small registry. Methods absent from the registry default to
// tier-3 (Slack's documented fallback for unclassified Web API calls).
//
// **HTTP 429 — propagate, do not auto-retry.** When the limiter has
// granted a token and Slack still answers `429 Too Many Requests`
// (e.g. server-side burst-capacity drift, multi-replica deploy), the
// client returns [ErrRateLimited] wrapped with the `Retry-After` value
// the server reported. The rate limiter is the primary throttle; 429 is
// the safety net. Auto-retry would multiply request complexity (timing,
// idempotency, ctx interactions) without a clear win for M4.2.b's
// needs — `chat.postMessage` is not idempotent without `client_msg_id`,
// which the caller manages, not the transport. Callers that want retry
// wrap [Client.Do] explicitly.
//
// # Stdlib-only
//
// The package depends only on the Go standard library plus the
// existing in-repo `messenger` parent package. No third-party Slack
// SDK. Mirrors the keepclient discipline (M2.8.a) — a thin self-rolled
// client keeps dependency surface small and avoids inheriting an SDK's
// concurrency / retry / backoff opinions that conflict with our own.
//
// # Concurrency
//
// [Client] and [RateLimiter] are safe for concurrent use across
// goroutines once constructed. The limiter's per-tier bucket is
// internally locked; the client carries only immutable configuration
// after [NewClient] returns.
//
// # Redaction discipline
//
// The [Client]'s optional [Logger] (configured via [WithLogger]) NEVER
// receives the bearer token, the request body, or the Slack response
// body. Only structured metadata (method name, HTTP status, error
// type, Retry-After value) appears in log entries. Mirrors the M3.4.b /
// M3.5 / M3.6 / M3.7 redaction patterns documented in
// `docs/LESSONS.md`.
//
// # See also
//
//   - `core/pkg/messenger` — the portable adapter interface this
//     package will implement once M4.2.b/c/d land.
//   - `core/pkg/keepclient` — sibling stdlib-only HTTP client whose
//     functional-option / sentinel-error patterns this package mirrors.
//   - `docs/ROADMAP-phase1.md` §M4 → M4.2.
//   - https://api.slack.com/apis/rate-limits — Slack's published
//     per-method tier mapping.
package slack
