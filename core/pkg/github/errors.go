package github

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel errors that map to specific GitHub REST API failure shapes
// (or transport-level conditions) that callers will want to match. Use
// [errors.Is] (e.g. `errors.Is(err, github.ErrRepoNotFound)`) — error
// strings are documentation, not API.
var (
	// ErrInvalidAuth surfaces when GitHub returns HTTP 401. 401
	// ("Unauthorized") indicates the supplied token did not
	// authenticate; callers wrapping the client for credential rotation
	// match this sentinel to refresh.
	ErrInvalidAuth = errors.New("github: invalid auth")

	// ErrRepoNotFound surfaces when GitHub returns HTTP 404 from a
	// repo-keyed endpoint (`/repos/{owner}/{repo}/...`). GitHub uses
	// 404 for BOTH "repo does not exist" and "the authenticated
	// principal does not have permission to view it" — a privacy-
	// preserving pattern the client cannot disambiguate. Callers treat
	// the two cases identically.
	ErrRepoNotFound = errors.New("github: repo not found")

	// ErrRateLimited surfaces when GitHub signals a rate-limit
	// exhaustion. Two distinct wire shapes trigger this sentinel:
	//
	//   - HTTP 429 (primary rate-limit; canonical for secondary-rate-
	//     limit + abuse-rate-limit responses, see GitHub docs).
	//   - HTTP 403 with response header `X-RateLimit-Remaining: 0`
	//     (primary-rate-limit-reached-budget response, GitHub's
	//     documented shape for the per-token 5000/h ceiling).
	//
	// The wrapped chain carries an [*APIError] whose Status is the
	// returned status and RetryAfter is populated from
	// `X-RateLimit-Reset` (Unix-epoch seconds; minus the configured
	// clock).
	ErrRateLimited = errors.New("github: rate limited")

	// ErrInvalidArgs surfaces synchronously when a public method's
	// arguments fail pre-flight validation (empty owner / repo,
	// malformed shape, etc.). The HTTP exchange is NEVER attempted.
	// Callers match to distinguish programmer-bug from server-side
	// failure.
	ErrInvalidArgs = errors.New("github: invalid args")

	// ErrMissingAuth surfaces synchronously from [NewClient] when no
	// [TokenSource] is supplied via [WithTokenSource]. Distinct from
	// [ErrInvalidAuth] — that one indicates the credentials reached
	// GitHub and were rejected; this one indicates the client was
	// constructed without any credentials at all.
	ErrMissingAuth = errors.New("github: missing auth")

	// ErrInvalidBaseURL surfaces synchronously from [NewClient] when
	// [WithBaseURL] received a string that failed parse or basic
	// shape invariants (missing scheme/host). The default base URL
	// is `https://api.github.com`; this sentinel only surfaces when
	// an override was supplied that didn't parse cleanly.
	ErrInvalidBaseURL = errors.New("github: invalid base url")
)

// APIError carries the parsed envelope from a non-2xx GitHub REST
// response. GitHub's standard failure shape is:
//
//	{
//	  "message": "Not Found",
//	  "documentation_url": "https://docs.github.com/..."
//	}
//
// Match with [errors.Is] against the Err* sentinels — [APIError.Unwrap]
// maps the HTTP status (and, where GitHub disambiguates via header,
// the endpoint kind) onto the matching sentinel.
type APIError struct {
	// Status is the HTTP status code returned by GitHub. Always
	// populated.
	Status int

	// Method is the HTTP method (GET, POST, …) of the failing request.
	// Useful in log entries.
	Method string

	// Endpoint is the REST path the request targeted (e.g.
	// `/repos/owner/repo/pulls`). Useful in log entries; never
	// populated with credentials.
	Endpoint string

	// Message carries the GitHub `message` field (free-form human-
	// readable failure description). May be empty when GitHub
	// responds with a non-2xx but no JSON envelope.
	Message string

	// DocumentationURL carries the GitHub `documentation_url` field
	// when present. May be empty.
	DocumentationURL string

	// RetryAfter is the parsed retry budget; populated from
	// `X-RateLimit-Reset` when the response triggers [ErrRateLimited]
	// (429 OR 403-with-`X-RateLimit-Remaining: 0`). Zero otherwise.
	RetryAfter time.Duration

	// RateLimitRemaining is the parsed `X-RateLimit-Remaining` header
	// value, populated for both success and failure responses when
	// GitHub sets the header. -1 when the header was absent / not
	// parseable.
	RateLimitRemaining int

	// kind disambiguates the endpoint family for [APIError.Unwrap] —
	// e.g. a repo-keyed endpoint maps 404 to [ErrRepoNotFound] while
	// a non-repo endpoint leaves 404 unmapped. Set by the dispatching
	// method before wrapping.
	kind endpointKind
}

// endpointKind tags the endpoint family on APIError so Unwrap can
// disambiguate status codes that mean different things on different
// paths (e.g. 404 on /repos vs other endpoints). Intentionally
// unexported.
type endpointKind int

const (
	endpointGeneric endpointKind = iota
	endpointRepo
)

// Error implements the error interface with a self-describing format
// that includes the HTTP method, the endpoint, the status, and a
// truncated rendering of the message. The truncation is rune-aware so
// multibyte sequences do not split mid-character.
func (e *APIError) Error() string {
	if e == nil {
		return "github: <nil APIError>"
	}
	msg := ""
	if e.Message != "" {
		msg = " msg=" + truncateRunes(strings.TrimSpace(e.Message), 200)
	}
	return fmt.Sprintf("github: api error: method=%q endpoint=%q status=%d%s", e.Method, e.Endpoint, e.Status, msg)
}

// truncateRunes returns s capped at maxRunes runes (NOT bytes) with a
// trailing ellipsis when truncation actually occurred. GitHub surfaces
// are mostly ASCII so this normally short-circuits, but a rune-aware
// truncation guards against future UTF-8 payloads landing on a
// multi-byte boundary.
func truncateRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "…"
}

// Unwrap maps the HTTP status and endpoint kind to one of the package
// sentinels per the table:
//
//	Status 401                                                                     -> ErrInvalidAuth
//	Status 404 on repo-keyed endpoint                                              -> ErrRepoNotFound
//	Status 429                                                                     -> ErrRateLimited
//	Status 403 AND X-RateLimit-Remaining: 0                                        -> ErrRateLimited
//
// Statuses outside this table return nil — callers can still match the
// type with [errors.As] but `errors.Is(err, ErrSomething)` will return
// false (a deliberate signal that the response did not fit a documented
// sentinel). The 403-with-remaining=0 path is GitHub's documented
// primary-rate-limit shape; a plain 403 (forbidden — token lacks the
// scope) does NOT alias [ErrInvalidAuth] because the failure mode is
// authorisation, not authentication, and callers may want to surface
// "scope missing" distinctly.
func (e *APIError) Unwrap() error {
	if e == nil {
		return nil
	}
	switch e.Status {
	case 401:
		return ErrInvalidAuth
	case 429:
		return ErrRateLimited
	case 403:
		if e.RateLimitRemaining == 0 {
			return ErrRateLimited
		}
	case 404:
		if e.kind == endpointRepo {
			return ErrRepoNotFound
		}
	}
	return nil
}
