// filter.go ships the M1.3.d `peer.RoleFilter` resolver. Fans out over
// `keepclient.Client.ListPeers` from M1.2 and applies the filter in
// memory so every M1.3.* consumer sees the same active-peer snapshot.
//
// The resolver is intentionally a thin layer over [Lister.ListPeers]:
// no caching, no cursor follow-up, no parallel fetch. Filtering happens
// client-side because the M1.2 endpoint deliberately does NOT surface a
// role / language / capability query API — see
// `core/pkg/keepclient/read_peers.go` for the rationale. A future
// follow-up may push the filter to the server when the active set
// exceeds the maxPeerListLimit; M1.3.d's bounded worker pool + the
// roadmap's <200-peer projection make in-memory filtering the right
// trade-off today.
//
// audit discipline: this file does NOT import `keeperslog` and does
// NOT call `.Append(`. Target resolution is a read-side seam; the
// audit taxonomy belongs to M1.4. A source-grep AC test pins this.
//
// PII discipline: the filter fields are operator-supplied strings; the
// resolver case-folds on match BUT defensively deep-copies the result
// slice and every `Capabilities` slice so caller-side mutation cannot
// bleed back into a cached snapshot. The resolved `[]keepclient.Peer`
// IS the truth surface for [Tool.Broadcast]'s downstream Ask fan-out.

package peer

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// RoleFilter is the closed-set filter [Tool.Broadcast] applies over
// the [Lister.ListPeers] active-peer snapshot. Every field is an
// AND-joined conjunct: a peer must satisfy [Roles] AND [Languages]
// AND [Capabilities] to be admitted. Empty slices on a given field
// mean "match every value on that axis" — leaving [Languages] empty
// admits every language; populating it restricts the broadcast.
//
// The match semantics per field:
//
//   - [Roles]: case-insensitive equality match against
//     [keepclient.Peer.Role]. A target with role "Lead" matches a
//     filter of `Roles: []string{"lead"}`.
//   - [Languages]: case-insensitive equality match against
//     [keepclient.Peer.Language]. Empty / whitespace-only entries are
//     rejected at the resolver with [ErrPeerRoleFilterEmpty] (a
//     filter with three empty strings is indistinguishable from no
//     filter on the axis at all).
//   - [Capabilities]: every entry must appear in
//     [keepclient.Peer.Capabilities] (set-superset match, NOT
//     intersect-non-empty). A filter of
//     `Capabilities: []string{"peer:ask", "peer:reply"}` admits only
//     peers declaring BOTH capabilities. Case-sensitive: capability
//     ids are machine-minted, not operator-facing free text.
//
// The [ExcludeSelf] toggle drops the acting watchkeeper from the
// resolved target set. Default false because the resolver is
// independent of [Tool.Broadcast]'s acting-id parameter — set the
// toggle when the broadcast is initiated by a peer that should not
// receive its own message (the common case for the M1.6 escalation
// fan-out).
type RoleFilter struct {
	// Roles is the closed-set role-name filter. Empty admits every
	// role. Each entry must be non-empty after whitespace-trim; the
	// resolver fail-fasts via [ErrPeerRoleFilterEmpty] otherwise (an
	// empty entry would silently broaden the match to every peer on
	// the axis, which is a caller bug).
	Roles []string

	// Languages is the closed-set language-code filter. Empty admits
	// every language. Each entry must be non-empty after whitespace-
	// trim; the resolver fail-fasts via [ErrPeerRoleFilterEmpty]
	// otherwise.
	Languages []string

	// Capabilities is the set-superset capability filter. Empty admits
	// every capability set. Each entry must be non-empty after
	// whitespace-trim; the resolver fail-fasts via
	// [ErrPeerRoleFilterEmpty] otherwise. Case-sensitive match against
	// [keepclient.Peer.Capabilities].
	Capabilities []string

	// ExcludeSelf, when true, drops the acting watchkeeper from the
	// resolved set. The acting-id is supplied by the [Tool.Broadcast]
	// caller; the resolver itself is acting-id-agnostic until the
	// drop step.
	ExcludeSelf bool
}

// isEmpty reports whether the filter has no positive criteria (every
// slice is nil or empty). A wildcard fan-out would be a caller bug
// (token-budget runaway + stampede risk); the resolver surfaces
// [ErrPeerRoleFilterEmpty] so the caller has to opt in deliberately.
//
// [ExcludeSelf] alone is NOT enough to make the filter non-empty: a
// filter that only drops the caller still admits every other peer on
// every axis, which is the wildcard case.
func (f RoleFilter) isEmpty() bool {
	return len(f.Roles) == 0 && len(f.Languages) == 0 && len(f.Capabilities) == 0
}

// validate runs the per-field non-empty-entry check. Hoisted to a
// helper so [Tool.Broadcast] can short-circuit on an invalid filter
// BEFORE the `keepclient.ListPeers` round-trip.
func (f RoleFilter) validate() error {
	if f.isEmpty() {
		return ErrPeerRoleFilterEmpty
	}
	for _, axis := range [][]string{f.Roles, f.Languages, f.Capabilities} {
		for _, entry := range axis {
			if strings.TrimSpace(entry) == "" {
				return ErrPeerRoleFilterEmpty
			}
		}
	}
	return nil
}

// FilterResolver is the narrow seam [Tool.Broadcast] consumes to turn
// a [RoleFilter] into a `[]keepclient.Peer` target set. The default
// implementation ([defaultFilterResolver]) consults [Lister.ListPeers]
// once and applies the filter in memory; tests inject a hand-rolled
// fake that returns a deterministic slice without standing up an HTTP
// fetch path.
//
// Implementations MUST return a defensive deep-copy of every
// [keepclient.Peer] (including the Capabilities slice) so caller-side
// mutation cannot bleed into a future call. Implementations MUST
// surface [ErrPeerRoleFilterEmpty] on a filter with no positive
// criteria (or any whitespace-only entry).
type FilterResolver interface {
	// Resolve returns the active peers matching `filter`. The
	// `actingID` parameter is consulted only when
	// [RoleFilter.ExcludeSelf] is true. Returns an empty (non-nil)
	// slice if no active peer matches; the caller surfaces
	// [ErrPeerNoTargets] on an empty result.
	Resolve(ctx context.Context, filter RoleFilter, actingID string) ([]keepclient.Peer, error)
}

// defaultFilterResolver is the production-mode resolver. Wraps a
// [Lister] (typically `*keepclient.Client`) and applies the filter in
// memory over the returned active-peer snapshot.
type defaultFilterResolver struct {
	lister Lister
}

// NewFilterResolver returns the production-mode [FilterResolver]
// backed by `lister`. The `lister` must be non-nil; the constructor
// panics otherwise. Mirrors [NewTool]'s wiring discipline.
func NewFilterResolver(lister Lister) FilterResolver {
	if lister == nil {
		panic("peer: NewFilterResolver: lister must not be nil")
	}
	return &defaultFilterResolver{lister: lister}
}

// Resolve runs the [FilterResolver.Resolve] contract over
// [Lister.ListPeers]. Resolution order:
//
//  1. validate the filter (non-empty, no whitespace-only entries);
//     short-circuit with [ErrPeerRoleFilterEmpty] on failure.
//  2. fetch the active-peer snapshot via [Lister.ListPeers].
//  3. apply [RoleFilter.Roles] (case-insensitive),
//     [RoleFilter.Languages] (case-insensitive), and
//     [RoleFilter.Capabilities] (case-sensitive, set-superset).
//  4. drop only peers reporting
//     [keepclient.PeerAvailabilityAvailable] — a future "throttled"
//     state stays out of the broadcast without a client release.
//  5. apply [RoleFilter.ExcludeSelf] over `actingID`.
//  6. defensively deep-copy every survivor (including
//     [keepclient.Peer.Capabilities]).
//  7. sort the result by [keepclient.Peer.WatchkeeperID] so the
//     downstream fan-out has a stable order (the M1.2 endpoint
//     returns `created_at DESC` but a future seek-pagination follow-
//     up may change that; pinning the sort here decouples).
func (r *defaultFilterResolver) Resolve(
	ctx context.Context,
	filter RoleFilter,
	actingID string,
) ([]keepclient.Peer, error) {
	if err := filter.validate(); err != nil {
		return nil, err
	}
	resp, err := r.lister.ListPeers(ctx, keepclient.ListPeersRequest{})
	if err != nil {
		return nil, fmt.Errorf("peer: resolve filter: %w", err)
	}

	// Pre-compute lower-case lookup sets for case-insensitive axes so
	// the inner-loop check is O(1) per axis rather than O(len(filter)).
	rolesLower := lowerSet(filter.Roles)
	langsLower := lowerSet(filter.Languages)
	caps := stringSet(filter.Capabilities)

	out := make([]keepclient.Peer, 0, len(resp.Items))
	for _, p := range resp.Items {
		if p.Availability != keepclient.PeerAvailabilityAvailable {
			continue
		}
		if filter.ExcludeSelf && p.WatchkeeperID == actingID {
			continue
		}
		if len(rolesLower) > 0 {
			if _, ok := rolesLower[strings.ToLower(p.Role)]; !ok {
				continue
			}
		}
		if len(langsLower) > 0 {
			if _, ok := langsLower[strings.ToLower(p.Language)]; !ok {
				continue
			}
		}
		if len(caps) > 0 && !hasAllCapabilities(p.Capabilities, caps) {
			continue
		}
		// Defensive deep-copy: the resolver's snapshot lives only for
		// this call; copying here means the caller can hold the
		// returned slice indefinitely without aliasing the lister's
		// internal buffers.
		copied := p
		if p.Capabilities != nil {
			copied.Capabilities = make([]string, len(p.Capabilities))
			copy(copied.Capabilities, p.Capabilities)
		}
		out = append(out, copied)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].WatchkeeperID < out[j].WatchkeeperID
	})
	return out, nil
}

// lowerSet returns a `map[string]struct{}` keyed by `strings.ToLower`
// of each entry. Empty / whitespace-only entries are skipped (the
// validator already rejects them; the defensive skip here keeps the
// helper safe to reuse).
func lowerSet(in []string) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out[strings.ToLower(v)] = struct{}{}
	}
	return out
}

// stringSet returns a `map[string]struct{}` keyed by the verbatim
// entry (no case-fold). Used for [RoleFilter.Capabilities] where the
// match is case-sensitive.
func stringSet(in []string) map[string]struct{} {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		out[v] = struct{}{}
	}
	return out
}

// hasAllCapabilities reports whether `peerCaps` is a superset of
// `required`. Returns true only when EVERY required capability appears
// in the peer's declared set. Case-sensitive match.
func hasAllCapabilities(peerCaps []string, required map[string]struct{}) bool {
	if len(required) == 0 {
		return true
	}
	peerSet := make(map[string]struct{}, len(peerCaps))
	for _, c := range peerCaps {
		peerSet[c] = struct{}{}
	}
	for r := range required {
		if _, ok := peerSet[r]; !ok {
			return false
		}
	}
	return true
}
