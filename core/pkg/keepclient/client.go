// Package keepclient is the stdlib-only Go client for the Keep service.
//
// The package exposes a [Client] type constructed via functional options
// ([Option]) plus a typed error taxonomy ([ServerError] and the Err* sentinels)
// that mirrors the server's `{"error":"<code>","reason":"<reason>"}` envelopes.
// M2.8.a ships only the transport plumbing and the open `GET /health` smoke
// endpoint via [Client.Health]; business endpoints land in M2.8.b/c/d.
//
// Token handling: any request whose path argument begins with the literal
// prefix `/v1/` requires a [TokenSource] (configured via [WithTokenSource]).
// The Keep server's `/health` route is open and never consumes a token.
package keepclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// defaultHTTPTimeout is the timeout applied to the [Client]'s default
// *http.Client when the caller does not supply one via [WithHTTPClient]. It
// matches the Keep server's slowest documented read endpoint with comfortable
// headroom; callers with stricter budgets should pass a tuned client.
const defaultHTTPTimeout = 10 * time.Second

// Client is the Keep service HTTP client. Construct via [NewClient]. The zero
// value is not usable — callers must always go through [NewClient] so the
// default HTTP client and no-op logger are installed.
//
// Client is safe for concurrent use across goroutines once constructed.
type Client struct {
	cfg clientConfig
}

// clientConfig is the internal, mutable bag the [Option] callbacks populate.
// It is not exported so we can evolve fields without breaking callers.
type clientConfig struct {
	baseURL     *url.URL
	httpClient  *http.Client
	tokenSource TokenSource
	logger      func(ctx context.Context, msg string, kv ...any)
}

// Option configures a [Client] at construction time. Pass options to
// [NewClient]; later options override earlier ones for the same field.
type Option func(*clientConfig)

// NewClient returns a configured [Client]. Callers must pass [WithBaseURL];
// passing an empty or unparseable base URL panics from inside [WithBaseURL]
// (programmer error — there is no error return on this constructor).
//
// All other options are optional. The default HTTP client carries a
// 10-second timeout; the default logger is a no-op; the default token source
// is nil (calls to `/v1/*` paths without one return [ErrNoTokenSource]).
func NewClient(opts ...Option) *Client {
	cfg := clientConfig{
		httpClient: &http.Client{Timeout: defaultHTTPTimeout},
		logger:     func(context.Context, string, ...any) {},
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Client{cfg: cfg}
}

// WithBaseURL sets the base URL every request is resolved against. The
// supplied string must be non-empty and parseable by [net/url.Parse]; on
// either failure WithBaseURL panics with a clear message. The trailing
// slash is tolerated — `http://x/` and `http://x` join `/health` the same
// way (no double slash) because the underlying join uses
// [net/url.URL.ResolveReference].
func WithBaseURL(raw string) Option {
	if raw == "" {
		panic("keepclient: WithBaseURL: base URL must not be empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		panic(fmt.Sprintf("keepclient: WithBaseURL: %v", err))
	}
	if u.Scheme == "" || u.Host == "" {
		panic(fmt.Sprintf("keepclient: WithBaseURL: %q is missing scheme or host", raw))
	}
	return func(c *clientConfig) { c.baseURL = u }
}

// WithHTTPClient overrides the default *http.Client. Pass a tuned client to
// adjust the request timeout or transport. A nil argument is ignored so
// callers can apply a conditional override without explicit branching.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *clientConfig) {
		if hc != nil {
			c.httpClient = hc
		}
	}
}

// WithTokenSource registers the [TokenSource] consulted for `/v1/*` requests.
// The Keep server's `/health` endpoint is open and never triggers a Token
// call. A nil argument leaves the source unset (calls to `/v1/*` will then
// fail with [ErrNoTokenSource] before any network round-trip).
func WithTokenSource(ts TokenSource) Option {
	return func(c *clientConfig) { c.tokenSource = ts }
}

// WithLogger overrides the default no-op logger. The hook is called from
// [Client.do] at request begin and end with stable kv pairs (`method`,
// `path`, `status`, `err`). A nil argument is ignored so callers can apply
// a conditional override without explicit branching.
func WithLogger(fn func(ctx context.Context, msg string, kv ...any)) Option {
	return func(c *clientConfig) {
		if fn != nil {
			c.logger = fn
		}
	}
}
