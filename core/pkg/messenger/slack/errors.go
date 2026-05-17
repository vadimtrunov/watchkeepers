package slack

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors that map to specific Slack `error` codes (or HTTP
// transport conditions). Callers match with [errors.Is]
// (e.g. `errors.Is(err, slack.ErrInvalidAuth)`) rather than comparing
// error text — error strings are documentation, not API.
var (
	// ErrRateLimited surfaces when the rate limiter rejects a synchronous
	// [RateLimiter.Allow] call OR when Slack returns HTTP 429 even after
	// the limiter granted a token. The wrapped chain carries an
	// [*APIError] (when 429-with-envelope) whose Status is 429 and
	// RetryAfter is populated. Matchable via [errors.Is].
	ErrRateLimited = errors.New("slack: rate limited")

	// ErrInvalidAuth surfaces when Slack returns `error: "invalid_auth"`
	// or `error: "not_authed"` in the response envelope (the bearer
	// token is missing, malformed, or revoked). Distinct from
	// [ErrTokenExpired]; an expired token may surface as either
	// depending on the endpoint.
	ErrInvalidAuth = errors.New("slack: invalid auth")

	// ErrTokenExpired surfaces when Slack returns
	// `error: "token_expired"`. Callers managing token rotation match
	// this sentinel to trigger a refresh.
	ErrTokenExpired = errors.New("slack: token expired")

	// ErrChannelNotFound surfaces when Slack returns
	// `error: "channel_not_found"` (the channel id does not resolve OR
	// the calling bot lacks access to it). Aligns with the portable
	// [messenger.ErrChannelNotFound] sentinel — adapter wrappers built
	// on top of [Client] should re-export the portable form.
	ErrChannelNotFound = errors.New("slack: channel not found")

	// ErrAppNotFound surfaces when Slack returns
	// `error: "app_not_found"`. Aligns with the portable
	// [messenger.ErrAppNotFound] sentinel.
	ErrAppNotFound = errors.New("slack: app not found")

	// ErrUserNotFound surfaces when Slack returns
	// `error: "user_not_found"`. Aligns with the portable
	// [messenger.ErrUserNotFound] sentinel.
	ErrUserNotFound = errors.New("slack: user not found")

	// ErrUnknownMethod is returned synchronously by [Client.Do] when
	// the supplied method name is empty. The HTTP exchange is NOT
	// attempted on this path. Matchable via [errors.Is].
	ErrUnknownMethod = errors.New("slack: unknown method")

	// ErrReconnectExhausted surfaces from the Socket Mode subscription
	// when the bounded retry budget for transparent reconnects has
	// been burned without recovering a working WSS connection. The
	// wrapped chain carries the LAST observed error (transport drop,
	// hello timeout, etc.). Callers awaiting [messenger.Subscription.Stop]
	// receive this wrapped error; matchable via [errors.Is].
	ErrReconnectExhausted = errors.New("slack: socket mode: reconnect budget exhausted")

	// ErrMissingScope surfaces when Slack returns
	// `error: "missing_scope"` — the bearer token lacks one of the
	// OAuth scopes required for the method (e.g. `im:history` for
	// `conversations.history` against an IM channel,
	// `im:write` for `conversations.open`). Callers managing scope
	// requests match this sentinel to surface a re-consent prompt
	// rather than a transient retry.
	ErrMissingScope = errors.New("slack: missing required oauth scope")

	// ErrCannotDMBot surfaces when Slack returns
	// `error: "cannot_dm_bot"` from `conversations.open` — the target
	// user id resolves to a bot account that the calling bot cannot
	// DM. The Coordinator surfaces this as a refusal to the agent so
	// it can re-plan; matchable via [errors.Is].
	ErrCannotDMBot = errors.New("slack: cannot DM bot account")

	// ErrChannelNameTaken surfaces when Slack returns
	// `error: "name_taken"` from `conversations.create` — a channel
	// with the requested name already exists. [Client.CreateChannel]
	// transparently resolves the existing channel id via
	// `conversations.list` and returns it (idempotent semantics
	// per M1.1.b AC), so callers normally do NOT observe this sentinel
	// from CreateChannel directly. It is exported for callers that
	// invoke `conversations.create` through the low-level [Client.Do]
	// surface and want to branch on the documented Slack code.
	// Matchable via [errors.Is].
	ErrChannelNameTaken = errors.New("slack: channel name already taken")

	// ErrInvalidChannelName surfaces when Slack returns
	// `error: "invalid_name"` / `invalid_name_punctuation` /
	// `invalid_name_required` / `invalid_name_specials` /
	// `invalid_name_maxlength` from `conversations.create` — the
	// supplied channel name fails Slack-side validation (Slack's
	// rules: 1-80 chars, lowercase letters/digits/`-`/`_`, no
	// leading/trailing punctuation). The caller's K2K lifecycle
	// must normalise the name before reaching this sentinel — the
	// `k2k-<uuid-prefix>` shape from M1.1.c is by construction
	// compliant, so this sentinel is primarily a defensive surface
	// for the operator-facing CLI. Matchable via [errors.Is].
	ErrInvalidChannelName = errors.New("slack: invalid channel name")

	// ErrAlreadyArchived surfaces when Slack returns
	// `error: "already_archived"` from `conversations.archive` — the
	// channel has already been archived. [Client.ArchiveChannel] lifts
	// this as a NON-error (idempotent close semantics: archiving an
	// already-archived channel is a no-op success), so callers do NOT
	// observe this sentinel from ArchiveChannel directly. Exported
	// for callers using the low-level [Client.Do] surface.
	// Matchable via [errors.Is].
	ErrAlreadyArchived = errors.New("slack: channel already archived")

	// ErrIsArchived surfaces when Slack returns
	// `error: "is_archived"` from `conversations.invite` — the channel
	// the caller is trying to invite into has already been archived.
	// Distinct from [ErrAlreadyArchived] which is observed at archive
	// time; [ErrIsArchived] is observed at invite time. K2K lifecycle
	// callers MUST surface this as a refusal rather than a transient
	// retry. Matchable via [errors.Is].
	ErrIsArchived = errors.New("slack: channel is archived")

	// ErrAlreadyInChannel surfaces when Slack returns
	// `error: "already_in_channel"` from `conversations.invite` — one
	// of the supplied user ids is already a member of the target
	// channel. [Client.InviteToChannel] / [Client.RevealChannel] lift
	// this as a NON-error (idempotent invite semantics: re-inviting an
	// already-present member is a no-op success), so callers do NOT
	// observe this sentinel from those methods directly. Exported for
	// callers using the low-level [Client.Do] surface.
	// Matchable via [errors.Is].
	ErrAlreadyInChannel = errors.New("slack: user already in channel")

	// ErrCantInviteSelf surfaces when Slack returns
	// `error: "cant_invite_self"` from `conversations.invite` — the
	// calling bot tried to invite its own bot-user id. The K2K
	// lifecycle wiring (M1.1.c) MUST filter the calling bot from the
	// invite list before reaching this sentinel; it is exported as a
	// defensive surface for callers that batch-invite a list including
	// the caller. Matchable via [errors.Is].
	ErrCantInviteSelf = errors.New("slack: cannot invite self")
)

// APIError carries the parsed envelope from a Slack response that
// indicates failure. Two failure shapes feed it:
//
//  1. A 2xx response whose JSON body has `{"ok": false, "error": "..."}`
//     — the canonical Slack Web API failure shape.
//  2. A non-2xx HTTP response (4xx / 5xx). For 429 the `RetryAfter`
//     field carries the parsed `Retry-After` header.
//
// Match with [errors.Is] against the Err* sentinels — [APIError.Unwrap]
// returns the matching sentinel for the table documented on
// [APIError.Unwrap].
type APIError struct {
	// Status is the HTTP status code returned by Slack. Always
	// populated.
	Status int

	// Code is the Slack `error` field from the envelope (empty when
	// the response was non-2xx without a JSON envelope, e.g. a raw
	// 502 from a load balancer).
	Code string

	// Method is the Slack Web API method name the request targeted
	// (e.g. `chat.postMessage`). Useful in log entries.
	Method string

	// RetryAfter is the parsed `Retry-After` header value (only
	// populated for 429 responses; zero otherwise). Slack documents
	// the header as integer seconds; the client parses both
	// integer-seconds and HTTP-date forms per RFC 9110 §10.2.3.
	RetryAfter time.Duration
}

// Error implements the error interface with a self-describing format
// that includes the method, the HTTP status, and the parsed Slack
// `error` code (when present).
func (e *APIError) Error() string {
	if e == nil {
		return "slack: <nil APIError>"
	}
	switch {
	case e.Code == "" && e.Method == "":
		return fmt.Sprintf("slack: api error: status=%d", e.Status)
	case e.Code == "":
		return fmt.Sprintf("slack: api error: method=%q status=%d", e.Method, e.Status)
	case e.Method == "":
		return fmt.Sprintf("slack: api error: status=%d code=%q", e.Status, e.Code)
	default:
		return fmt.Sprintf("slack: api error: method=%q status=%d code=%q", e.Method, e.Status, e.Code)
	}
}

// Unwrap maps the response code or HTTP status to one of the package
// sentinels per the table:
//
//	Code "channel_not_found"             -> ErrChannelNotFound
//	Code "user_not_found"                -> ErrUserNotFound
//	Code "app_not_found"                 -> ErrAppNotFound
//	Code "invalid_auth" / "not_authed"   -> ErrInvalidAuth
//	Code "token_expired"                 -> ErrTokenExpired
//	Code "ratelimited"                   -> ErrRateLimited
//	Status 429                           -> ErrRateLimited
//
// Codes outside this table return nil — callers can still match the
// type with [errors.As] but `errors.Is(err, ErrSomething)` will return
// false (a deliberate signal that the response did not fit a documented
// sentinel).
func (e *APIError) Unwrap() error {
	if e == nil {
		return nil
	}
	switch e.Code {
	case "channel_not_found":
		return ErrChannelNotFound
	case "user_not_found", "users_not_found":
		return ErrUserNotFound
	case "app_not_found":
		return ErrAppNotFound
	case "invalid_auth", "not_authed":
		return ErrInvalidAuth
	case "token_expired":
		return ErrTokenExpired
	case "ratelimited":
		return ErrRateLimited
	case "missing_scope":
		return ErrMissingScope
	case "cannot_dm_bot":
		return ErrCannotDMBot
	case "name_taken":
		return ErrChannelNameTaken
	case "invalid_name",
		"invalid_name_punctuation",
		"invalid_name_required",
		"invalid_name_specials",
		"invalid_name_maxlength":
		return ErrInvalidChannelName
	case "already_archived":
		return ErrAlreadyArchived
	case "is_archived":
		return ErrIsArchived
	case "already_in_channel":
		return ErrAlreadyInChannel
	case "cant_invite_self":
		return ErrCantInviteSelf
	}
	if e.Status == 429 {
		return ErrRateLimited
	}
	return nil
}
