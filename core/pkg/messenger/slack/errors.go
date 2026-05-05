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
	case "user_not_found":
		return ErrUserNotFound
	case "app_not_found":
		return ErrAppNotFound
	case "invalid_auth", "not_authed":
		return ErrInvalidAuth
	case "token_expired":
		return ErrTokenExpired
	case "ratelimited":
		return ErrRateLimited
	}
	if e.Status == 429 {
		return ErrRateLimited
	}
	return nil
}
