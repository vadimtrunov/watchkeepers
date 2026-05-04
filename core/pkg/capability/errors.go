package capability

import "errors"

// ErrClosed is returned by every public [Broker] method after [Broker.Close]
// has been called. The broker is single-use: once Closed it cannot be
// resurrected; callers build a fresh [Broker] when they need a new
// lifecycle. Matchable via [errors.Is].
var ErrClosed = errors.New("capability: broker closed")

// ErrInvalidScope is returned synchronously by [Broker.Issue] when the
// supplied `scope` is the empty string. An empty scope can never match a
// concrete invocation site under the `pkg:verb` convention and is always
// a programmer error at the call site. Matchable via [errors.Is].
var ErrInvalidScope = errors.New("capability: invalid scope")

// ErrInvalidTTL is returned synchronously by [Broker.Issue] when the
// supplied `ttl` is non-positive (zero or negative). A non-positive TTL
// would issue an immediately-expired or never-valid token; rejected
// up-front so the broker map is never populated with such an entry.
// Matchable via [errors.Is].
var ErrInvalidTTL = errors.New("capability: invalid ttl")

// ErrInvalidToken is returned by [Broker.Validate] when the supplied
// `token` is not present in the broker (never issued, already revoked, or
// already pruned by the reaper). The error message and value never
// contain the input token bytes — see the redaction-discipline contract
// in the package godoc. Matchable via [errors.Is].
var ErrInvalidToken = errors.New("capability: invalid token")

// ErrTokenExpired is returned by [Broker.Validate] when the supplied
// token is present in the broker but its expiry timestamp is at or
// before the current clock reading (boundary inclusive: `now() ==
// expiry` is expired). On expiry the entry is removed lazily so a
// subsequent Validate with the same token returns [ErrInvalidToken].
// Matchable via [errors.Is].
var ErrTokenExpired = errors.New("capability: token expired")

// ErrScopeMismatch is returned by [Broker.Validate] when the supplied
// token is present and unexpired but its registered scope does not
// equal the `scope` argument. The error message never contains the
// input token bytes — see the redaction-discipline contract in the
// package godoc. Matchable via [errors.Is].
var ErrScopeMismatch = errors.New("capability: scope mismatch")
