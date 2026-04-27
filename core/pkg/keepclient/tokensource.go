package keepclient

import "context"

// TokenSource produces a capability bearer token (stub interface — real doc
// lands in the matching feat commit).
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a stub adapter.
type StaticToken string

// Token is the stub implementation.
func (s StaticToken) Token(_ context.Context) (string, error) { return "", nil }

// TokenSourceFunc is a stub adapter.
type TokenSourceFunc func(ctx context.Context) (string, error)

// Token is the stub implementation.
func (f TokenSourceFunc) Token(_ context.Context) (string, error) { return "", nil }
