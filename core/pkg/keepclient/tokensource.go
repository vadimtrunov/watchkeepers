package keepclient

import "context"

// TokenSource produces a capability bearer token for a single outbound
// request. Implementations may cache, refresh, or re-mint as needed; the
// client treats the returned string as opaque and never logs it.
//
// Token must respect the supplied context (cancel/deadline) so a stalled
// refresh does not pin a request past its budget.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a [TokenSource] that always returns the supplied literal.
// Convenient for tests and short-lived scripts; production code should
// prefer a refresh-capable source so an expired token does not silently
// fail every request.
type StaticToken string

// Token returns the literal value of the [StaticToken]. The context is
// honored only by virtue of being passed through; no work is performed.
func (s StaticToken) Token(_ context.Context) (string, error) {
	return string(s), nil
}

// TokenSourceFunc adapts a plain function to the [TokenSource] interface.
// The signature matches [TokenSource.Token] verbatim so callers can wire a
// closure (e.g. one that delegates to `auth.TestIssuer.Issue`) without
// declaring a new type.
type TokenSourceFunc func(ctx context.Context) (string, error)

// Token calls the underlying function. It satisfies [TokenSource].
func (f TokenSourceFunc) Token(ctx context.Context) (string, error) {
	return f(ctx)
}
