// Package keepclient is the stdlib-only Go client for the Keep service.
//
// RED stub — types compile so the failing-test commit lints clean; real
// implementations land in the matching `feat(keep)` commit.
package keepclient

import (
	"context"
	"net/http"
)

// Client is the Keep service HTTP client (stub).
type Client struct {
	cfg clientConfig
}

// clientConfig is the internal options bag (stub).
type clientConfig struct {
	httpClient  *http.Client
	tokenSource TokenSource
	logger      func(ctx context.Context, msg string, kv ...any)
}

// Option configures a [Client] at construction time (stub).
type Option func(*clientConfig)

// NewClient is the constructor (stub — returns a zero-config Client so tests
// fail with a clear nil-pointer or zero-value mismatch rather than a compile
// error).
func NewClient(_ ...Option) *Client { return &Client{} }

// WithBaseURL is a stub option.
func WithBaseURL(_ string) Option { return func(*clientConfig) {} }

// WithHTTPClient is a stub option.
func WithHTTPClient(_ *http.Client) Option { return func(*clientConfig) {} }

// WithTokenSource is a stub option.
func WithTokenSource(_ TokenSource) Option { return func(*clientConfig) {} }

// WithLogger is a stub option.
func WithLogger(_ func(ctx context.Context, msg string, kv ...any)) Option {
	return func(*clientConfig) {}
}
