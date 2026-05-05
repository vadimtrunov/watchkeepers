package keepclient

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Human mirrors the server's humanRow JSON. Nullable columns surface as
// `*string` so the pointer's nil-ness preserves the SQL NULL distinction
// (an explicit nil rather than the zero string).
type Human struct {
	// ID is the human row UUID.
	ID string `json:"id"`
	// OrganizationID is the parent organization UUID.
	OrganizationID string `json:"organization_id"`
	// DisplayName is the human-friendly handle for the row.
	DisplayName string `json:"display_name"`
	// Email is the optional contact email; nil when the column was NULL
	// in Postgres.
	Email *string `json:"email"`
	// SlackUserID is the optional Slack user identifier; nil when the
	// column was NULL in Postgres. The unique constraint on this column
	// (migration 012) guarantees that a non-nil value maps to exactly one
	// human row across the entire schema.
	SlackUserID *string `json:"slack_user_id"`
	// CreatedAt is the row's created_at timestamp.
	CreatedAt time.Time `json:"created_at"`
}

// InsertHumanRequest is the typed request body for [Client.InsertHuman].
// Field names mirror the server's `insertHumanRequest` shape verbatim.
// Optional fields use `omitempty` so the empty value does not appear on
// the wire — the server's DisallowUnknownFields decoder still accepts the
// canonical keys when present and the omitempty contract keeps the
// uniqueness semantics on slack_user_id intact (an empty string would
// collide with another empty string under the unique index, which is why
// the server normalises empty strings to SQL NULL).
type InsertHumanRequest struct {
	// OrganizationID is the parent organization UUID. Required; empty
	// values are rejected client-side with [ErrInvalidRequest].
	OrganizationID string `json:"organization_id"`
	// DisplayName is the human-friendly handle. Required; empty values
	// are rejected client-side with [ErrInvalidRequest].
	DisplayName string `json:"display_name"`
	// Email is the optional contact email; omitempty so SQL NULL fires.
	Email string `json:"email,omitempty"`
	// SlackUserID is the optional Slack user identifier; omitempty so
	// SQL NULL fires.
	SlackUserID string `json:"slack_user_id,omitempty"`
}

// InsertHumanResponse mirrors the server's `insertHumanResponse` envelope
// returned by a successful POST /v1/humans.
type InsertHumanResponse struct {
	// ID is the freshly-inserted human row UUID.
	ID string `json:"id"`
}

// InsertHuman calls POST /v1/humans on the configured Keep service. The
// server inserts the row and stamps `id` and `created_at`. Empty
// OrganizationID or DisplayName return [ErrInvalidRequest] synchronously
// without a network round-trip. A duplicate slack_user_id surfaces as a
// [*ServerError] whose Unwrap matches [ErrConflict].
func (c *Client) InsertHuman(ctx context.Context, req InsertHumanRequest) (*InsertHumanResponse, error) {
	if req.OrganizationID == "" || req.DisplayName == "" {
		return nil, ErrInvalidRequest
	}
	var out InsertHumanResponse
	if err := c.do(ctx, http.MethodPost, "/v1/humans", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// LookupHumanBySlackID calls GET /v1/humans/by-slack/{slack_user_id}. A
// missing row surfaces as [*ServerError] whose Unwrap matches
// [ErrNotFound]. Empty slackUserID returns [ErrInvalidRequest]
// synchronously without a network round-trip.
func (c *Client) LookupHumanBySlackID(ctx context.Context, slackUserID string) (*Human, error) {
	if slackUserID == "" {
		return nil, ErrInvalidRequest
	}
	var out Human
	// PathEscape so caller-supplied values containing `/` or `?` cannot
	// smuggle extra path segments. Slack user IDs are alphanumeric in
	// practice but the server's path-segment lookup must be unambiguous
	// regardless of caller hygiene.
	if err := c.do(ctx, http.MethodGet, "/v1/humans/by-slack/"+url.PathEscape(slackUserID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// setWatchkeeperLeadRequest is the on-wire body shape for
// PATCH /v1/watchkeepers/{id}/lead. Only `lead_human_id` is allowed; the
// server's DisallowUnknownFields decoder rejects any other key, so the
// client uses a small unexported struct to hold exactly that field.
type setWatchkeeperLeadRequest struct {
	LeadHumanID string `json:"lead_human_id"`
}

// SetWatchkeeperLead calls PATCH /v1/watchkeepers/{id}/lead. It rebinds
// the watchkeeper row's `lead_human_id` foreign key. Empty id or
// leadHumanID return [ErrInvalidRequest] synchronously without a network
// round-trip. An unknown watchkeeper id surfaces as [*ServerError] whose
// Unwrap matches [ErrNotFound]. An unknown human id surfaces as
// [*ServerError] whose Unwrap matches [ErrInvalidRequest].
func (c *Client) SetWatchkeeperLead(ctx context.Context, watchkeeperID, leadHumanID string) error {
	if watchkeeperID == "" || leadHumanID == "" {
		return ErrInvalidRequest
	}
	body := setWatchkeeperLeadRequest{LeadHumanID: leadHumanID}
	// PathEscape so caller-supplied ids with `/` or `?` cannot smuggle
	// extra path segments. The server validates UUID shape before any DB
	// work, so an escaped non-UUID still surfaces as a 4xx not a 5xx.
	return c.do(ctx, http.MethodPatch, "/v1/watchkeepers/"+url.PathEscape(watchkeeperID)+"/lead", body, nil)
}
