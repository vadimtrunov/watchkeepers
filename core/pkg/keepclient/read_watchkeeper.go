package keepclient

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// maxWatchkeeperListLimit mirrors the server's hard cap. Reproducing it
// client-side spares a round trip on obvious bugs and keeps the client and
// server bounds in lockstep documentation-wise.
const maxWatchkeeperListLimit = 200

// Watchkeeper mirrors the server's watchkeeperRow JSON. Nullable timestamps
// and the nullable foreign key surface as *time.Time / *string so the
// pointer's nil-ness preserves the SQL NULL distinction (an explicit nil
// rather than the zero time / empty string).
type Watchkeeper struct {
	// ID is the watchkeeper row UUID.
	ID string `json:"id"`
	// ManifestID is the parent manifest UUID.
	ManifestID string `json:"manifest_id"`
	// LeadHumanID is the lead-operator human UUID.
	LeadHumanID string `json:"lead_human_id"`
	// ActiveManifestVersionID is the optional pinned manifest version
	// UUID; nil when the column was NULL in Postgres.
	ActiveManifestVersionID *string `json:"active_manifest_version_id"`
	// Status is one of "pending" | "active" | "retired".
	Status string `json:"status"`
	// SpawnedAt is set on the pending→active transition; nil while pending.
	SpawnedAt *time.Time `json:"spawned_at"`
	// RetiredAt is set on the active→retired transition; nil before retire.
	RetiredAt *time.Time `json:"retired_at"`
	// ArchiveURI is the storage URI of the archived per-agent notebook
	// recorded by the M7.2.c MarkRetired saga step. Nil when the row has
	// not yet been retired, when it was retired before M7.2.c shipped, or
	// when a future M7.3 compensator path retires the watchkeeper without
	// an archive (e.g. a saga that fails before the M7.2.b NotebookArchive
	// step runs). The server-side column is `archive_uri text NULL` —
	// migration `022_watchkeepers_archive_uri.sql`.
	ArchiveURI *string `json:"archive_uri"`
	// CreatedAt is the row's created_at timestamp.
	CreatedAt time.Time `json:"created_at"`
}

// ListWatchkeepersRequest configures a [Client.ListWatchkeepers] call. The
// zero value is valid and means "no status filter, server-default limit".
type ListWatchkeepersRequest struct {
	// Status filters by lifecycle state when non-empty. Allowed values
	// are "pending" | "active" | "retired"; any other non-empty string
	// returns [ErrInvalidRequest] without a network round-trip.
	Status string
	// Limit caps the number of rows returned. 0 means "do not send the
	// query parameter at all and let the server apply its default" (50).
	// Negative or > 200 returns [ErrInvalidRequest] without a network
	// round-trip; the server enforces the same upper bound.
	Limit int
}

// ListWatchkeepersResponse mirrors the server's listWatchkeepersResponse
// envelope. NextCursor is reserved for a future seek-pagination
// follow-up; M3.2.a always returns nil.
type ListWatchkeepersResponse struct {
	// Items is the list of rows in `created_at DESC` order.
	Items []Watchkeeper `json:"items"`
	// NextCursor is reserved for a future pagination cursor; nil today.
	NextCursor *string `json:"next_cursor"`
}

// validListStatuses is the closed set of statuses accepted by the server's
// `?status=` filter. Mirrored client-side so a caller typo never burns a
// network round-trip.
//
//nolint:gochecknoglobals // intentional module-scoped lookup table.
var validListStatuses = map[string]struct{}{
	"pending": {},
	"active":  {},
	"retired": {},
}

// GetWatchkeeper calls GET /v1/watchkeepers/{id}. A missing row surfaces as
// [*ServerError] whose Unwrap matches [ErrNotFound]. Empty id returns
// [ErrInvalidRequest] synchronously without a network round-trip.
func (c *Client) GetWatchkeeper(ctx context.Context, id string) (*Watchkeeper, error) {
	if id == "" {
		return nil, ErrInvalidRequest
	}
	var out Watchkeeper
	// PathEscape so caller-supplied ids with `/` or `?` cannot smuggle
	// extra path segments. The server validates UUID shape before any DB
	// work, so an escaped non-UUID still surfaces as a 4xx not a 5xx.
	if err := c.do(ctx, http.MethodGet, "/v1/watchkeepers/"+url.PathEscape(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListWatchkeepers calls GET /v1/watchkeepers with the optional status
// filter and limit. Out-of-range limit and unknown status values return
// [ErrInvalidRequest] synchronously without a network round-trip.
func (c *Client) ListWatchkeepers(ctx context.Context, req ListWatchkeepersRequest) (*ListWatchkeepersResponse, error) {
	if req.Status != "" {
		if _, ok := validListStatuses[req.Status]; !ok {
			return nil, ErrInvalidRequest
		}
	}
	if req.Limit < 0 || req.Limit > maxWatchkeeperListLimit {
		return nil, ErrInvalidRequest
	}

	q := url.Values{}
	if req.Status != "" {
		q.Set("status", req.Status)
	}
	if req.Limit > 0 {
		q.Set("limit", strconv.Itoa(req.Limit))
	}
	path := "/v1/watchkeepers"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var out ListWatchkeepersResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
