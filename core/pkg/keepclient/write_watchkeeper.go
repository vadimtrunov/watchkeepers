package keepclient

import (
	"context"
	"net/http"
	"net/url"
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
// PATCH /v1/watchkeepers/{id}/status. Only `status` is allowed; the server's
// DisallowUnknownFields decoder rejects any other key, so the client uses a
// small unexported struct to hold exactly that field.
type updateWatchkeeperStatusRequest struct {
	Status string `json:"status"`
}

// UpdateWatchkeeperStatus calls PATCH /v1/watchkeepers/{id}/status. Allowed
// transitions are pending→active and active→retired; any other transition
// surfaces as [*ServerError] whose Unwrap matches [ErrInvalidStatusTransition]
// (and also [ErrInvalidRequest] since the server returns 400). Empty id or
// out-of-set status return [ErrInvalidRequest] synchronously without a
// network round-trip.
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
