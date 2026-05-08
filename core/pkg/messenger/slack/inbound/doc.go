// Package inbound is the Slack inbound HTTP webhook scaffolding for the
// Events API and Interactivity API. ROADMAP §M6 → M6.3 → M6.3.a.
//
// # Scope (M6.3.a)
//
// The package ships an [http.Handler] factory ([NewHandler]) that mounts
// two routes:
//
//   - `POST /v1/slack/events` — Slack Events API. Handles
//     `url_verification` (returns the supplied challenge as `text/plain`
//     per the Slack registration handshake) AND `event_callback`
//     (decodes the envelope, dispatches the inner event to the
//     configured [EventDispatcher], ACKs HTTP 200 within Slack's
//     3-second budget).
//   - `POST /v1/slack/interactions` — Slack Interactivity API. Decodes
//     the `payload=<json>` form-encoded body, dispatches by `type`
//     field (`block_actions`, `view_submission`, …) to the configured
//     [InteractionDispatcher], ACKs HTTP 200.
//
// Both endpoints verify the inbound request signature per
// <https://api.slack.com/authentication/verifying-requests-from-slack>:
// `signature = "v0=" + hex(hmac_sha256(signingSecret, "v0:" + ts + ":" + raw_body))`,
// compared with constant-time [hmac.Equal]. A 5-minute timestamp window
// (configurable via [WithTimestampWindow]) guards against replay
// attacks. The raw body is read ONCE, used for the signature check,
// then re-supplied to the JSON decoder via a fresh [io.ReadCloser]
// (no double-read of [http.Request.Body]).
//
// Body size is capped at 1 MB by default (configurable via
// [WithMaxBodyBytes]); over-cap requests return HTTP 413 without
// touching the dispatchers.
//
// Both endpoints emit a `slack_webhook_received` audit event on the
// happy path and `slack_webhook_rejected` on every negative branch via
// the configured [AuditAppender] (typically `*keeperslog.Writer`). The
// audit payload carries `event_type`, the request method, the route,
// and a request-id correlation; the body content NEVER appears in any
// audit row, log line, or error string (PII-aware redaction discipline
// — see M3.4.b / M2b.7 / M6.2.d).
//
// # NOT in scope (deferred to later M6.3 sub-bullets)
//
//   - Business interpretation of incoming events. The dispatchers are
//     skeleton interfaces; M6.3.b/c/d wire intents (DM ingestion,
//     approval-card actions, view submissions).
//   - Outbound Slack actions beyond what M6.1.b already ships.
//   - Cost-tracker integration (M6.3.e/f).
//   - Rate-limit / retry handling on the inbound path.
//   - Event deduplication via the `event_id` cache. Slack retries
//     within ~10s; the operator runbook documents the dedup gap.
//
// # Package location decision (M6.3.a)
//
// The handler lives at `core/pkg/messenger/slack/inbound/` rather than
// at a top-level `core/pkg/slackdm/`. Two reasons:
//
//  1. Symmetry. The existing `core/pkg/messenger/slack/` package owns
//     every Slack-platform concern (rate-limiter, sentinel errors,
//     manifest types, signing-secret persistence via
//     [slack.CreateAppCredentials]). Inbound webhook verification
//     consumes the same signing-secret bytes; nesting the inbound
//     package under the slack adapter root keeps the platform-specific
//     surface together for callers and reviewers.
//  2. Layering. The inbound subpackage depends ONLY on
//     [keeperslog.Writer] for the audit chain — it does NOT import
//     the parent `slack` package, so there is no cyclic-import risk
//     and no transport-coupling between the inbound HTTP server and
//     the outbound Web API client. Tests in the parent package test
//     outbound concerns; tests here test inbound concerns.
//
// A future top-level `core/pkg/slackdm/` may host higher-level DM
// orchestration (intent recognition, approval saga state machines)
// that composes both inbound and outbound. M6.3.a defers that
// decision until the business surface stabilises.
//
// # Stdlib-only
//
// The package depends only on the Go standard library plus the
// existing in-repo `keeperslog` and (for tests) the `messenger/slack`
// parent. No third-party Slack SDK; mirrors the parent package's
// stdlib-only discipline (M4.2.a).
//
// # Concurrency
//
// The handler returned from [NewHandler] is safe for concurrent use
// across goroutines once constructed. It carries only immutable
// configuration; per-request state lives on the goroutine stack.
//
// # See also
//
//   - `core/pkg/messenger/slack` — the parent Slack adapter package.
//   - `core/pkg/keeperslog` — audit writer.
//   - `docs/ROADMAP-phase1.md` §M6 → M6.3 → M6.3.a.
//   - <https://api.slack.com/authentication/verifying-requests-from-slack>
//     — Slack's published signature-verification algorithm.
package inbound
