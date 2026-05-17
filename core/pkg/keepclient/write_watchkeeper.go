package keepclient

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// validWatchkeeperStatuses is the closed set of valid target statuses for
// [Client.UpdateWatchkeeperStatus]. The server enforces the same shape via
// DisallowUnknownFields + a body-level switch; the client mirrors it so a
// caller typo never burns a network round-trip.
//
//nolint:gochecknoglobals // intentional module-scoped lookup table.
var validWatchkeeperStatuses = map[string]struct{}{
	"active":  {},
	"retired": {},
}

// InsertWatchkeeperRequest is the typed request body for
// [Client.InsertWatchkeeper]. Field names mirror the server's
// `insertWatchkeeperRequest` shape verbatim. The struct intentionally has NO
// `status`, `spawned_at`, or `retired_at` fields — those are stamped
// server-side and rejected by the server's DisallowUnknownFields decoder.
type InsertWatchkeeperRequest struct {
	// ManifestID is the parent manifest UUID. Required; empty values are
	// rejected client-side with [ErrInvalidRequest].
	ManifestID string `json:"manifest_id"`
	// LeadHumanID is the lead-operator human UUID. Required; empty values
	// are rejected client-side with [ErrInvalidRequest].
	LeadHumanID string `json:"lead_human_id"`
	// ActiveManifestVersionID is the optional pinned manifest version. The
	// server stores SQL NULL when omitted (omitempty so the empty value is
	// not transmitted at all).
	ActiveManifestVersionID string `json:"active_manifest_version_id,omitempty"`
	// RoleID is the optional M7.1.a opaque role-identity string. The server
	// stores SQL NULL when omitted (omitempty so the empty value is not
	// transmitted at all) and an existing legacy caller that never sets
	// the field continues to insert rows with a NULL role_id. Free-form
	// text: no client-side shape validation beyond non-empty (an empty
	// string is folded into the absent shape via omitempty, matching the
	// `ActiveManifestVersionID` pattern). Validation of the role-identity
	// string itself — uniqueness across active rows, catalogue membership,
	// etc. — is deferred to the upstream M7.1 writer (Manifest template
	// slug + Watchmaster spawn flow). The M7.1.b predecessor-lookup
	// endpoint reads this column server-side via the M7.1.a migration's
	// partial index `idx_watchkeeper_role_id_retired`.
	RoleID string `json:"role_id,omitempty"`
}

// InsertWatchkeeperResponse mirrors the server's `insertWatchkeeperResponse`
// envelope returned by a successful POST /v1/watchkeepers.
type InsertWatchkeeperResponse struct {
	// ID is the freshly-inserted watchkeeper row UUID.
	ID string `json:"id"`
}

// InsertWatchkeeper calls POST /v1/watchkeepers on the configured Keep
// service. The server inserts the row with status='pending' and
// spawned_at/retired_at NULL — those fields are stamped on later status
// transitions. Empty ManifestID or LeadHumanID return [ErrInvalidRequest]
// synchronously without a network round-trip.
func (c *Client) InsertWatchkeeper(ctx context.Context, req InsertWatchkeeperRequest) (*InsertWatchkeeperResponse, error) {
	if req.ManifestID == "" || req.LeadHumanID == "" {
		return nil, ErrInvalidRequest
	}
	var out InsertWatchkeeperResponse
	if err := c.do(ctx, http.MethodPost, "/v1/watchkeepers", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// updateWatchkeeperStatusRequest is the on-wire body shape for
// PATCH /v1/watchkeepers/{id}/status. The server's DisallowUnknownFields
// decoder rejects any key beyond `status` and `archive_uri`, so the client
// uses a single struct sized for both transitions:
//
//   - pending→active   carries `status:"active"` ONLY (omits archive_uri).
//   - active→retired   may carry an optional `archive_uri:"…"` alongside
//     `status:"retired"` to record the archived notebook URI emitted by
//     the M7.2.b NotebookArchive saga step. The server rejects an
//     archive_uri on the pending→active transition pre-tx.
//
// Iter-2 critic finding (Major): `ArchiveURI` is `*string` so the
// client mints the on-wire shape SYMMETRIC with the server's
// `optionalArchiveURI.Present` bit. A plain `string` + `omitempty`
// folded "absent" and "intentionally empty" into the same encoded form,
// which silently took the no-archive SQL branch on the server when a
// future caller forgot to pre-validate non-empty before construction.
// With a pointer-typed field a caller who fails to set the field at
// all sends the absent shape (legacy M6.2.c-compatible); a caller
// who sets `&""` sends the present-but-empty shape and the server's
// non-blank gate rejects it before any side effect.
type updateWatchkeeperStatusRequest struct {
	Status     string  `json:"status"`
	ArchiveURI *string `json:"archive_uri,omitempty"`
}

// UpdateWatchkeeperStatus calls PATCH /v1/watchkeepers/{id}/status. Allowed
// transitions are pending→active and active→retired; any other transition
// surfaces as [*ServerError] whose Unwrap matches [ErrInvalidStatusTransition]
// (and also [ErrInvalidRequest] since the server returns 400). Empty id or
// out-of-set status return [ErrInvalidRequest] synchronously without a
// network round-trip.
//
// This method drives the pending→active transition (the M7.1.e saga's
// runtime-launch step calls into the server's status-update path
// indirectly through the harness boot flow) AND the
// active→retired-WITHOUT-archive transition that the M6.2.c synchronous
// retire tool emits today. The M7.2.c MarkRetired saga step uses
// [Client.UpdateWatchkeeperRetired] instead so the archive URI ride-along
// stays explicit at the call site.
func (c *Client) UpdateWatchkeeperStatus(ctx context.Context, id, status string) error {
	if id == "" {
		return ErrInvalidRequest
	}
	if _, ok := validWatchkeeperStatuses[status]; !ok {
		return ErrInvalidRequest
	}
	body := updateWatchkeeperStatusRequest{Status: status}
	// PathEscape so caller-supplied ids with `/` or `?` cannot smuggle
	// extra path segments. The server validates UUID shape before any DB
	// work, so an escaped non-UUID still surfaces as a 4xx not a 5xx.
	return c.do(ctx, http.MethodPatch, "/v1/watchkeepers/"+url.PathEscape(id)+"/status", body, nil)
}

// UpdateWatchkeeperRetired calls PATCH /v1/watchkeepers/{id}/status with
// `status:"retired"` and the supplied non-blank, RFC-3986-shaped
// archive_uri. Empty id, empty archive_uri, whitespace-only
// archive_uri, OR a value that does not parse as an absolute URI
// (i.e. lacks a non-empty scheme — `"garbage"` and `"../../tmp"` both
// fail this gate) return [ErrInvalidRequest] synchronously without a
// network round-trip.
//
// The saga step (M7.2.c MarkRetired) is the sole production caller
// and pre-validates the URI shape upstream via the M7.2.b
// ErrInvalidArchiveURI gate. The whitespace check mirrors the
// server-side `parseUpdateWatchkeeperStatusRequest` defense-in-depth
// gate (iter-1 codex finding Minor); the absolute-URI check at the
// seam is iter-2 codex finding (Major): the wire contract documents
// `archive_uri` as an RFC 3986 URI with a non-empty scheme, but
// neither this method nor the server enforced that — strings like
// `"garbage"` would round-trip onto the column for any caller that
// bypassed the saga path. The pre-flight scheme check fails fast at
// the seam closest to the bug; the server enforces the same shape at
// the HTTP boundary as defense-in-depth.
//
// Server-side error mapping is identical to [Client.UpdateWatchkeeperStatus]:
// the active→retired transition rule applies and any other current row
// state surfaces as [*ServerError] whose Unwrap matches
// [ErrInvalidStatusTransition].
//
// This is the M7.2.c-introduced sibling of [Client.UpdateWatchkeeperStatus].
// It is intentionally a SEPARATE method (not a third positional argument
// or a functional-option) so:
//
//  1. The existing pending→active call sites cannot accidentally smuggle a
//     stray archive_uri — the type system rejects it.
//  2. The M7.2.c saga step's call site reads as a single intent — "mark
//     retired with an archive URI" — without a sentinel check on
//     archive_uri non-empty.
//  3. A future migration that requires archive_uri on retire flips the
//     contract on this method alone, leaving the legacy
//     [Client.UpdateWatchkeeperStatus] behaviour unchanged for the M6.2.c
//     synchronous tool's benefit.
func (c *Client) UpdateWatchkeeperRetired(ctx context.Context, id, archiveURI string) error {
	if id == "" {
		return ErrInvalidRequest
	}
	if strings.TrimSpace(archiveURI) == "" {
		return ErrInvalidRequest
	}
	// RFC 3986 absolute-URI shape: a parse error OR a missing scheme
	// rejects values like "garbage" or "../../tmp" that
	// [net/url.Parse] otherwise tolerates as path-only URIs (iter-2
	// codex finding Major).
	parsed, err := url.Parse(archiveURI)
	if err != nil || !parsed.IsAbs() {
		return ErrInvalidRequest
	}
	body := updateWatchkeeperStatusRequest{Status: "retired", ArchiveURI: &archiveURI}
	// PathEscape so caller-supplied ids with `/` or `?` cannot smuggle
	// extra path segments. The server validates UUID shape before any DB
	// work, so an escaped non-UUID still surfaces as a 4xx not a 5xx.
	return c.do(ctx, http.MethodPatch, "/v1/watchkeepers/"+url.PathEscape(id)+"/status", body, nil)
}
