package github

import (
	"net/http"
	"net/url"
	"time"
)

// ClientOption configures a [Client] at [NewClient] time. Functional
// options are the package convention — mirrors the jira and slack
// adapters so callers consuming all three packages see one shape.
type ClientOption func(*clientConfig)

// WithBaseURL configures the GitHub REST API base URL the client will
// target. The default is `https://api.github.com` (public GitHub.com).
// GitHub Enterprise Server deployments pass
// `https://<your-ghes-host>/api/v3` — non-empty path prefixes ARE
// accepted (unlike the jira adapter where path prefixes route to
// non-Jira sub-products) because GHES REQUIRES `/api/v3`.
//
// Invariants (rejected synchronously at [NewClient] time):
//
//   - Scheme MUST be non-empty (typically `https`).
//   - Host MUST be non-empty.
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
	return func(c *clientConfig) {
		c.baseURL = u
		c.baseURLInvalid = false
	}
}

// WithTokenSource configures the [TokenSource] the client uses to
// resolve credentials per call. REQUIRED: [NewClient] returns
// [ErrMissingAuth] when not supplied.
//
// The supplied [TokenSource] MUST be safe for concurrent use; the
// client invokes it from every goroutine that drives a public method.
func WithTokenSource(src TokenSource) ClientOption {
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

// WithClock overrides the wall-clock source the client consults for
// `X-RateLimit-Reset` arithmetic. Tests pin a deterministic clock;
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
