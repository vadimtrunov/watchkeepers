// Package slack is the Slack adapter for the portable
// [github.com/vadimtrunov/watchkeepers/core/pkg/messenger.Adapter]
// interface. ROADMAP §M4 → M4.2.
//
// # Scope (M4.2.a foundation + M4.2.b SendMessage/SetBotProfile + M4.2.c Subscribe with reconnect + M4.2.d.1 CreateApp/LookupUser + M4.2.d.2 InstallApp)
//
// The package currently ships:
//
//   - M4.2.a — a tier-aware [RateLimiter] that respects Slack's
//     per-method burst+sustained budgets (tier-1 .. tier-4 — see
//     https://api.slack.com/apis/rate-limits); a low-level [Client]
//     HTTP wrapper around Slack Web API (`https://slack.com/api/
//     <method>`) with bearer-token auth, JSON encoding, Slack
//     `{ok: false, error: "..."}` envelope decoding, and
//     `Retry-After`-aware HTTP 429 handling; a sentinel-error
//     vocabulary mapping common Slack `error` codes onto matchable
//     [errors.Is] sentinels.
//
//   - M4.2.b — [Client.SendMessage] (`chat.postMessage`) and
//     [Client.SetBotProfile] (`users.profile.set`). Both are layered
//     on [Client.Do] and consume the rate-limiter / sentinel-error
//     foundation. The [messenger.Message.ThreadID] typed field maps
//     to `thread_ts`; documented Slack-specific knobs (mrkdwn, parse,
//     link_names, unfurl_links, unfurl_media, icon_emoji, icon_url,
//     username, reply_broadcast for SendMessage; status_emoji,
//     status_expiration, real_name, … for SetBotProfile) ride in
//     [messenger.Message.Metadata] and [messenger.BotProfile.Metadata]
//     respectively.
//
//   - M4.2.c.1 — [Client.Subscribe] via Socket Mode happy-path. POSTs
//     `apps.connections.open` (rate-limited, tier-1) to obtain a
//     one-shot WSS URL, dials it via [github.com/coder/websocket],
//     waits for the `hello` envelope, and runs a single read loop
//     that ACKS each event_api envelope back to Slack within the
//     documented 3-second budget BEFORE dispatching the decoded
//     [messenger.IncomingMessage] to the handler.
//
//   - M4.2.c.2 — resilient reconnect layered on top of c.1.
//     [Client.Subscribe] now reconnects transparently on
//     `disconnect` envelope, transport error, or pong-timeout. The
//     loop dials a fresh `apps.connections.open` URL, awaits a new
//     `hello`, and resumes reading; the caller's
//     [messenger.MessageHandler] keeps receiving events without
//     observing the reconnect. Backoff-with-jitter mirrors the
//     outbox / keepclient resilient-stream model. Application-layer
//     ping/pong (configurable interval + timeout) detects half-open
//     connections. After [WithSocketModeMaxReconnectAttempts]
//     consecutive failures the subscription unwinds with the wrapped
//     [ErrReconnectExhausted] sentinel surfaced via
//     [messenger.Subscription.Stop].
//
//   - M4.2.d.1 — [Client.CreateApp] (`apps.manifest.create`) and
//     [Client.LookupUser] (`users.info` / `bots.info` /
//     `users.lookupByEmail`). Manifest serialisation honours Slack's
//     mixed-type schema (string display fields, []string scopes,
//     bool settings flags) per the M4.2.b wire-format LESSON; the
//     manifest body is built from a `map[string]any` so boolean
//     leaves like `socket_mode_enabled` land on the wire as JSON
//     bools (a `map[string]string` envelope would force every leaf
//     into a JSON string and break Slack validation). LookupUser
//     discriminates by Slack id-prefix (`U`/`W` → users.info,
//     `B` → bots.info) and routes by populated [messenger.UserQuery]
//     field; an `@handle` query returns [messenger.ErrUnsupported]
//     (Slack does not expose handle-resolution outside paged
//     `users.list`).
//
//   - M4.2.d.2 — [Client.InstallApp] (`oauth.v2.access`). Exchanges a
//     Slack-issued OAuth authorization code (the admin-preapproval
//     flow's dev-workspace path is documented on the [Client.InstallApp]
//     comment) for the bot/user token bundle. Tokens NEVER ride on the
//     returned [messenger.Installation] — they are delivered out-of-band
//     to the caller-supplied [InstallTokenSink] (configured via
//     [WithInstallTokenSink]); only non-secret platform identifiers
//     (bot user id, team id, enterprise id, scope, token type,
//     enterprise-install flag) populate the Installation per the M4.1
//     design "Tokens themselves are stored in the secrets interface,
//     NOT here". The per-install OAuth code + client credentials ride
//     via the typed [InstallParamsResolver] (configured via
//     [WithInstallParamsResolver]) because [messenger.WorkspaceRef]
//     does not carry a Metadata bag in M4.1.
//
// What this package does NOT yet do (deferred to later M4.2 sub-bullets):
//
//   - M4.2.b follow-ups — [messenger.Message.Attachments] support
//     (Slack `blocks` for hosted-URL attachments + `files.upload` for
//     inline bytes); [messenger.BotProfile.AvatarPNG] support
//     (`users.setPhoto` multipart). Both currently return
//     [messenger.ErrUnsupported] so the contract reserves the field
//     rather than silently dropping it.
//
// All six [messenger.Adapter] methods are implemented; the compile-time
// assertion `var _ messenger.Adapter = (*Client)(nil)` lives in
// `adapter_assertion_test.go` and pins the conformance during
// `go test`.
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
