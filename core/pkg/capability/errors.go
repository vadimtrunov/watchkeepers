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

// ErrInvalidOrganization is returned synchronously by
// [Broker.IssueForOrg] when the supplied `organizationID` is the empty
// string. An empty organizationID would defeat the per-tenant pinning
// that IssueForOrg exists to provide; rejected up-front so the broker
// map is never populated with such an entry. Matchable via [errors.Is].
var ErrInvalidOrganization = errors.New("capability: invalid organization")

// ErrOrganizationMismatch is returned by [Broker.ValidateForOrg] when
// the supplied token is present, unexpired, and scope-matched but its
// registered organizationID does not equal the `organizationID`
// argument. Per-tenant pinning is the M3.5.a contract: a token minted
// for tenant A must NEVER validate for tenant B even if the scope
// matches. The error message never contains the input token bytes —
// see the redaction-discipline contract in the package godoc.
// Matchable via [errors.Is].
var ErrOrganizationMismatch = errors.New("capability: organization mismatch")
