package jira

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Sentinel errors that map to specific Atlassian REST API failure shapes
// (or transport-level conditions) that callers will want to match. Use
// [errors.Is] (e.g. `errors.Is(err, jira.ErrIssueNotFound)`) — error
// strings are documentation, not API.
var (
	// ErrInvalidAuth surfaces when Atlassian returns HTTP 401 or 403.
	// 401 ("Unauthorized") and 403 ("Forbidden") collapse onto the same
	// sentinel because both indicate the supplied credentials cannot
	// drive the requested call: 401 means the email/token pair did not
	// authenticate; 403 means the account authenticated but lacks the
	// project-level permission. Callers wrapping the client for
	// credential rotation match this sentinel to refresh.
	ErrInvalidAuth = errors.New("jira: invalid auth")

	// ErrIssueNotFound surfaces when Atlassian returns HTTP 404 from an
	// issue-keyed endpoint (`/rest/api/3/issue/{key}` and its children).
	// Atlassian uses 404 for BOTH "issue does not exist" and "you do
	// not have permission to view it" — a privacy-preserving pattern
	// the client cannot disambiguate without a separate permission
	// probe. Callers treat the two cases identically.
	ErrIssueNotFound = errors.New("jira: issue not found")

	// ErrFieldNotWhitelisted surfaces synchronously from
	// [Client.UpdateFields] when the caller attempts to write to a
	// field that is NOT in the [WithFieldWhitelist] set. The HTTP
	// exchange is NEVER attempted on this path — the whitelist is the
	// transport-layer security boundary, refused before the network.
	// A nil/empty whitelist refuses ALL writes (fail-closed default).
	ErrFieldNotWhitelisted = errors.New("jira: field not whitelisted")

	// ErrInvalidJQL surfaces when Atlassian returns HTTP 400 from
	// `/rest/api/3/search/jql` with an `errorMessages` payload
	// indicating a JQL parse / semantic error. Callers driving JQL
	// from user input match this sentinel to surface a friendly error
	// without leaking the raw Atlassian message.
	ErrInvalidJQL = errors.New("jira: invalid jql")

	// ErrRateLimited surfaces when Atlassian returns HTTP 429. The
	// wrapped chain carries an [*APIError] whose Status is 429 and
	// RetryAfter is populated (Atlassian publishes the budget via the
	// `Retry-After` header per RFC 9110 §10.2.3 — both integer-second
	// and HTTP-date forms parse).
	ErrRateLimited = errors.New("jira: rate limited")

	// ErrInvalidArgs surfaces synchronously when a public method's
	// arguments fail pre-flight validation (empty issue key, missing
	// required body, etc.). The HTTP exchange is NEVER attempted.
	// Callers match to distinguish programmer-bug from server-side
	// failure.
	ErrInvalidArgs = errors.New("jira: invalid args")

	// ErrMissingAuth surfaces synchronously from [NewClient] when no
	// [BasicAuthSource] is supplied via [WithBasicAuth]. Distinct from
	// [ErrInvalidAuth] — that one indicates the credentials reached
	// Atlassian and were rejected; this one indicates the client was
	// constructed without any credentials at all.
	ErrMissingAuth = errors.New("jira: missing auth")

	// ErrMissingBaseURL surfaces synchronously from [NewClient] when
	// no [WithBaseURL] is supplied. Atlassian Cloud has no canonical
	// hostname (every tenant is `https://<your>.atlassian.net`), so
	// the client cannot supply a default; the call must specify.
	ErrMissingBaseURL = errors.New("jira: missing base url")

	// ErrInvalidBaseURL surfaces synchronously from [NewClient] when
	// [WithBaseURL] received a string that did parse but failed the
	// adapter's invariants (missing scheme/host, non-empty path
	// prefix, …). Distinct from [ErrMissingBaseURL] so operators can
	// disambiguate "forgot to configure" from "configured wrongly".
	ErrInvalidBaseURL = errors.New("jira: invalid base url")
)

// APIError carries the parsed envelope from a non-2xx Atlassian REST
// response. Atlassian's standard failure shape is:
//
//	{
//	  "errorMessages": ["Issue does not exist or you do not have permission to see it."],
//	  "errors": {"summary": "..."}
//	}
//
// Match with [errors.Is] against the Err* sentinels — [APIError.Unwrap]
// maps the HTTP status (and, where Atlassian disambiguates via path or
// payload, the endpoint kind) onto the matching sentinel.
type APIError struct {
	// Status is the HTTP status code returned by Atlassian. Always
	// populated.
	Status int

	// Method is the HTTP method (GET, POST, PUT, …) of the failing
	// request. Useful in log entries.
	Method string

	// Endpoint is the REST path the request targeted (e.g.
	// `/rest/api/3/issue/PROJ-1`). Useful in log entries; never
	// populated with credentials or query parameters that may carry
	// secrets.
	Endpoint string

	// Messages carries the Atlassian `errorMessages` array (free-form
	// human-readable failure descriptions). May be empty when
	// Atlassian responds with a non-2xx but no JSON envelope (e.g. a
	// raw 502 from a load balancer).
	Messages []string

	// FieldErrors carries the Atlassian `errors` map keyed by field
	// name (each value is a human-readable per-field validation
	// message). May be nil.
	FieldErrors map[string]string

	// RetryAfter is the parsed `Retry-After` header value, only
	// populated for 429 responses; zero otherwise. Both integer-
	// seconds and HTTP-date forms parse per RFC 9110 §10.2.3.
	RetryAfter time.Duration

	// kind disambiguates the endpoint family for [APIError.Unwrap] —
	// e.g. an issue-keyed endpoint maps 404 to [ErrIssueNotFound]
	// while a non-issue endpoint leaves 404 unmapped. Set by the
	// dispatching method before wrapping.
	kind endpointKind
}

// endpointKind tags the endpoint family on APIError so Unwrap can
// disambiguate status codes that mean different things on different
// paths (e.g. 404 on /issue vs /search/jql). Intentionally unexported.
type endpointKind int

const (
	endpointGeneric endpointKind = iota
	endpointIssue
	endpointSearch
)

// Error implements the error interface with a self-describing format
// that includes the HTTP method, the endpoint, the status, and a
// truncated rendering of the first error message. The truncation is
// rune-aware so multibyte sequences do not split mid-character (the
// Atlassian envelope is mostly ASCII but the adapter cannot guarantee
// it).
func (e *APIError) Error() string {
	if e == nil {
		return "jira: <nil APIError>"
	}
	first := ""
	if len(e.Messages) > 0 {
		first = " msg=" + truncateRunes(strings.TrimSpace(e.Messages[0]), 200)
	}
	return fmt.Sprintf("jira: api error: method=%q endpoint=%q status=%d%s", e.Method, e.Endpoint, e.Status, first)
}

// truncateRunes returns s capped at maxRunes runes (NOT bytes) with a
// trailing ellipsis when truncation actually occurred. Atlassian
// surfaces are mostly ASCII so this normally short-circuits, but a
// rune-aware truncation guards against future UTF-8 payloads
// landing on a multi-byte boundary.
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
//	Status 401 / 403                                                       -> ErrInvalidAuth
//	Status 404 on issue-keyed endpoint                                     -> ErrIssueNotFound
//	Status 400 on /search/jql endpoint AND message mentions "jql"          -> ErrInvalidJQL
//	Status 429                                                             -> ErrRateLimited
//
// Statuses outside this table return nil — callers can still match the
// type with [errors.As] but `errors.Is(err, ErrSomething)` will return
// false (a deliberate signal that the response did not fit a documented
// sentinel). The 400-on-search disambiguation is deliberately
// conservative: a generic 400 (bad pageToken, malformed request body)
// must NOT alias `ErrInvalidJQL` so M8.2 tools can distinguish
// "operator wrote bad JQL" from "adapter / cursor bug".
func (e *APIError) Unwrap() error {
	if e == nil {
		return nil
	}
	switch e.Status {
	case 401, 403:
		return ErrInvalidAuth
	case 429:
		return ErrRateLimited
	case 404:
		if e.kind == endpointIssue {
			return ErrIssueNotFound
		}
	case 400:
		if e.kind == endpointSearch && hasJQLMarker(e.Messages) {
			return ErrInvalidJQL
		}
	}
	return nil
}

// hasJQLMarker reports whether any of the Atlassian error messages
// contain the substring "jql" (case-insensitive). Atlassian's
// canonical JQL parse-failure messages include the literal "JQL"
// (e.g. "The JQL query is invalid: …", "Field 'xxx' does not exist
// in JQL"). Other 400 conditions on `/search/jql` (bad pageToken,
// malformed body) typically do NOT mention JQL by name, so the
// marker is a reliable disambiguator.
func hasJQLMarker(msgs []string) bool {
	for _, m := range msgs {
		if strings.Contains(strings.ToLower(m), "jql") {
			return true
		}
	}
	return false
}
