package jira

import (
	"net/http"
	"net/url"
	"time"
)

// ClientOption configures a [Client] at [NewClient] time. Functional
// options are the package convention — mirrors the slack adapter so
// callers consuming both packages see one shape.
type ClientOption func(*clientConfig)

// WithBaseURL configures the Atlassian Cloud base URL the client will
// target. The required shape is `https://<your-tenant>.atlassian.net`
// — no trailing slash, no path prefix; the client appends
// `/rest/api/3/...` per call. Required: [NewClient] returns
// [ErrMissingBaseURL] when not supplied at all, or [ErrInvalidBaseURL]
// when the supplied value parses but fails the invariants below.
//
// Invariants (rejected synchronously at [NewClient] time):
//
//   - Scheme MUST be non-empty (typically `https`).
//   - Host MUST be non-empty.
//   - Path MUST be empty or `/`. A non-empty path prefix (e.g.
//     `https://example.atlassian.net/wiki`) would prepend itself to
//     every REST call (`/wiki/rest/api/3/issue/…`) and silently route
//     to a Confluence sub-product or a load-balancer rewrite path —
//     never to the Jira REST surface this adapter targets. Reject
//     fail-closed.
//
// The URL is parsed eagerly; an unparseable string OR a parsed URL
// that violates an invariant flags the config so [NewClient] can
// surface [ErrInvalidBaseURL]. This avoids returning an error from a
// functional option.
func WithBaseURL(raw string) ClientOption {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return func(c *clientConfig) { c.baseURLInvalid = true }
	}
	if u.Path != "" && u.Path != "/" {
		return func(c *clientConfig) { c.baseURLInvalid = true }
	}
	return func(c *clientConfig) {
		c.baseURL = u
		c.baseURLInvalid = false
	}
}

// WithBasicAuth configures the [BasicAuthSource] the client uses to
// resolve credentials per call. Required: [NewClient] returns
// [ErrMissingAuth] when not supplied.
//
// The supplied [BasicAuthSource] MUST be safe for concurrent use; the
// client invokes it from every goroutine that drives a public method.
func WithBasicAuth(src BasicAuthSource) ClientOption {
	return func(c *clientConfig) { c.auth = src }
}

// WithHTTPClient overrides the default *http.Client (a fresh one with
// [defaultHTTPTimeout]). Pass a tuned client for stricter timeouts,
// custom transports, or test fakes.
//
// A nil argument is silently ignored (the caller's intent is "leave
// the default"); the client never panics on a nil HTTPClient.
func WithHTTPClient(hc *http.Client) ClientOption {
	return func(c *clientConfig) {
		if hc == nil {
			return
		}
		c.httpClient = hc
	}
}

// WithLogger wires a structured-metadata-only diagnostic sink. See
// [Logger] for the redaction discipline. A nil argument is silently
// ignored (the client falls back to the silent default).
func WithLogger(l Logger) ClientOption {
	return func(c *clientConfig) {
		if l == nil {
			return
		}
		c.logger = l
	}
}

// WithFieldWhitelist configures the closed set of Atlassian field IDs
// [Client.UpdateFields] is permitted to write. The whitelist is the
// transport-layer security boundary: any [Client.UpdateFields] call
// that mentions a field NOT in this set returns [ErrFieldNotWhitelisted]
// synchronously, BEFORE the network exchange.
//
// Field IDs match the Atlassian wire shape — `summary`,
// `description`, `labels`, `duedate`, `customfield_10001`, …. Pass
// the IDs the caller's role authority matrix permits; M8.2's
// `update_ticket_field` tool drives this.
//
// Multi-call semantics — LAST CALL WINS, NOT UNION. Each
// [WithFieldWhitelist] invocation REPLACES whatever the previous
// invocation configured. Two consequences:
//
//  1. A wrapper helper that calls [WithFieldWhitelist]() with no
//     args RESETS the whitelist back to the fail-closed default
//     (refusing all writes). Useful for narrowing an inherited
//     parent configuration.
//
//  2. To accumulate, the caller assembles the full slice once and
//     passes every field as variadic arguments to a single call. The
//     adapter will not silently widen.
//
// This semantic differs from naïve option-stack accumulation
// (which would let a wider parent layer permanently widen a
// narrower child) and was chosen so the whitelist remains
// tamper-resistant under layered config helpers.
//
// Duplicates in the slice are deduplicated; empty strings are
// silently dropped.
func WithFieldWhitelist(fields ...string) ClientOption {
	return func(c *clientConfig) {
		if len(fields) == 0 {
			c.fieldWhitelist = nil
			return
		}
		wl := make(map[string]struct{}, len(fields))
		for _, f := range fields {
			if f == "" {
				continue
			}
			wl[f] = struct{}{}
		}
		c.fieldWhitelist = wl
	}
}

// WithClock overrides the wall-clock source the client consults for
// `Retry-After` HTTP-date arithmetic. Tests pin a deterministic clock;
// production wiring leaves this alone (defaults to [time.Now]).
//
// A nil argument is silently ignored.
func WithClock(now func() time.Time) ClientOption {
	return func(c *clientConfig) {
		if now == nil {
			return
		}
		c.clock = now
	}
}
