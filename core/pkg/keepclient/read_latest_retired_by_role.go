package keepclient

// read_latest_retired_by_role.go is the M7.1.b predecessor-lookup
// client. It calls
//
//	GET /v1/watchkeepers/latest-retired-by-role?role_id=<role>
//
// and decodes the response into a single [*Watchkeeper] mirroring the
// shape of [Client.GetWatchkeeper]. The server returns the freshest row
// (highest `retired_at`) whose `role_id` matches and which carries a
// non-null `archive_uri`, filtered by the caller's tenant via the JOIN-
// on-human pattern established by M3.5.a. A 404 surfaces here as the
// typed [ErrNoPredecessor] sentinel so the M7.1.c NotebookInheritStep
// can distinguish "no predecessor" from a transport / auth error and
// fall through to its no-op branch cleanly.
//
// Tenancy posture: the on-wire request carries NO tenant parameter —
// the server reads `claim.OrganizationID` from the bearer token. The
// `organizationID` argument on [Client.LatestRetiredByRole] exists for
// caller-side intent (it pins which tenant the call is being made
// against) and synchronous validation (an empty string is rejected
// without a network round-trip). It is NOT serialised onto the
// request; the server-side tenant is the claim's, never the client's.
// This matches the M3.5.a.1 sibling-method convention where the
// `organization_id` arg both documents intent and short-circuits
// obviously-broken callers before they burn a round-trip on a 403.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
)

// ErrNoPredecessor is returned by [Client.LatestRetiredByRole] when the
// server responds 404 — no retired watchkeeper with the supplied
// role_id exists in the caller's tenant. Match with [errors.Is].
//
// The underlying error is also a [*ServerError] with Status=404, so a
// caller that only cares about 404 can match [ErrNotFound] instead;
// [ErrNoPredecessor] is the typed sentinel the M7.1.c
// NotebookInheritStep saga step uses to distinguish "no predecessor
// exists for this role" (expected, fall through to no-op) from "the
// generic /v1/watchkeepers/{id} GET surfaced 404" (impossible for the
// inheritance lookup, but a separate sentinel keeps caller-side
// intent clear).
//
// The sentinel is package-scoped and NOT a [*ServerError] itself — the
// `Unwrap` chain on the underlying [*ServerError] adds it to the slice
// returned by [ServerError.Unwrap] for the specific
// `/v1/watchkeepers/latest-retired-by-role` 404 case via the
// [Client.LatestRetiredByRole] wrapping below, so callers can match
// both [ErrNoPredecessor] and [ErrNotFound] on the same error value.
//
// package (`ErrNotFound`, `ErrConflict`, …); `ErrNoPredecessor` is
// idiomatic here even though `errname` prefers `…Error` suffixes.
//
//nolint:errname // sentinel naming convention matches the rest of this
var ErrNoPredecessor = errors.New("keepclient: no predecessor")

// LatestRetiredByRole calls
//
//	GET /v1/watchkeepers/latest-retired-by-role?role_id=<roleID>
//
// returning the freshest retired watchkeeper carrying the supplied
// role_id in the caller's tenant. A missing predecessor surfaces as a
// wrapped error whose [errors.Is] chain matches BOTH [ErrNoPredecessor]
// and [ErrNotFound] (the underlying transport status is 404). Empty
// `organizationID` or empty `roleID` returns [ErrInvalidRequest]
// synchronously without a network round-trip — this prevents a
// caller-side bug (forgot to plumb the tenant / role through) from
// burning a 403 or 400 on the server.
//
// The `organizationID` argument is used ONLY for the synchronous empty
// check; the server reads the tenant from the bearer token's claim.
// Passing a non-empty `organizationID` that disagrees with the claim's
// `org_id` is NOT a wire-level error — the server filters by claim,
// not by argument — but doing so signals a caller-side wiring bug.
// The M7.1.c saga step that wraps this client always resolves the
// `organizationID` from the same claim source the token was minted
// from, so the two stay in lockstep by construction.
func (c *Client) LatestRetiredByRole(ctx context.Context, organizationID, roleID string) (*Watchkeeper, error) {
	if organizationID == "" || roleID == "" {
		return nil, ErrInvalidRequest
	}

	q := url.Values{}
	q.Set("role_id", roleID)
	path := "/v1/watchkeepers/latest-retired-by-role?" + q.Encode()

	var out Watchkeeper
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		// The server returns standard 404 not_found when no row
		// matches; surface the typed sentinel here so the M7.1.c
		// saga step can [errors.Is] this exact shape without
		// conflating it with a generic 404 from a different
		// /v1/watchkeepers route. Wrap with %w so the underlying
		// *ServerError's Unwrap chain (ErrNotFound + the underlying
		// status) remains matchable via errors.As / errors.Is.
		if errors.Is(err, ErrNotFound) {
			return nil, &noPredecessorError{wrapped: err}
		}
		return nil, err
	}
	return &out, nil
}

// noPredecessorError chains [ErrNoPredecessor] in front of the
// underlying [*ServerError] so callers can match BOTH sentinels on the
// same value:
//
//	errors.Is(err, keepclient.ErrNoPredecessor) // true
//	errors.Is(err, keepclient.ErrNotFound)      // true (via wrapped)
//
// The struct is unexported because the sentinel comparison is the
// stable contract, not the wrapping type. A reflect-based or
// type-assertion match against `*noPredecessorError` is NOT supported
// and may break in a future refactor.
type noPredecessorError struct {
	wrapped error
}

// Error returns the underlying *ServerError's message prefixed with the
// sentinel description, so a logged error string is self-describing.
func (e *noPredecessorError) Error() string {
	if e.wrapped == nil {
		return ErrNoPredecessor.Error()
	}
	return ErrNoPredecessor.Error() + ": " + e.wrapped.Error()
}

// Unwrap returns both the sentinel and the wrapped underlying error so
// `errors.Is(err, ErrNoPredecessor)` AND `errors.Is(err, ErrNotFound)`
// both match on the same value. The Go 1.20+ multi-error Unwrap shape
// is supported by errors.Is via the iterating fallback.
func (e *noPredecessorError) Unwrap() []error {
	if e.wrapped == nil {
		return []error{ErrNoPredecessor}
	}
	return []error{ErrNoPredecessor, e.wrapped}
}
