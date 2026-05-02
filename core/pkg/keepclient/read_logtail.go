package keepclient

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
)

// LogTailOptions configures a [Client.LogTail] call. The zero value is valid
// and means "use the server's default limit"; positive Limit appends
// `?limit=<n>` to the request URL; negative Limit is rejected synchronously
// with [ErrInvalidRequest].
type LogTailOptions struct {
	// Limit caps the number of events returned. Zero means "do not send
	// the query parameter at all and let the server apply its default."
	// Positive values are sent as `?limit=<n>`; the server clamps the
	// upper bound (see maxLogLimit in the Keep server). Negative values
	// are rejected client-side without a network round-trip.
	Limit int
}

// LogEvent mirrors a single row of the server's `keepersLogResponse`. The
// optional null-capable columns (CorrelationID, ActorWatchkeeperID,
// ActorHumanID) are *string with omitempty so an absent column decodes as
// nil. Payload stays as [json.RawMessage] because it is jsonb on the server
// and already valid JSON on the wire.
type LogEvent struct {
	// ID is the keepers_log row UUID.
	ID string `json:"id"`
	// EventType is the row's event_type column (free-form string).
	EventType string `json:"event_type"`
	// CorrelationID groups related events; nil when the column was NULL.
	CorrelationID *string `json:"correlation_id,omitempty"`
	// ActorWatchkeeperID is non-nil when the actor was a watchkeeper agent.
	ActorWatchkeeperID *string `json:"actor_watchkeeper_id,omitempty"`
	// ActorHumanID is non-nil when the actor was a human user.
	ActorHumanID *string `json:"actor_human_id,omitempty"`
	// Payload is the row's jsonb payload, kept as raw JSON.
	Payload json.RawMessage `json:"payload"`
	// CreatedAt is the row's created_at timestamp (RFC3339 on the wire).
	CreatedAt string `json:"created_at"`
}

// LogTailResponse mirrors the server's `keepersLogResponse` envelope.
type LogTailResponse struct {
	// Events is the list of log rows in `created_at DESC` order. The
	// server returns a non-nil empty slice when no rows are visible to
	// the calling scope; the client preserves that shape on decode.
	Events []LogEvent `json:"events"`
}

// LogTail calls GET /v1/keepers-log. With opts.Limit == 0 the request is
// sent without a `limit` query parameter and the server applies its own
// default; with opts.Limit > 0 the request appends `?limit=<n>`; with
// opts.Limit < 0 the call returns [ErrInvalidRequest] synchronously
// without a network round-trip.
func (c *Client) LogTail(ctx context.Context, opts LogTailOptions) (*LogTailResponse, error) {
	if opts.Limit < 0 {
		return nil, ErrInvalidRequest
	}
	path := "/v1/keepers-log"
	if opts.Limit > 0 {
		path += "?limit=" + strconv.Itoa(opts.Limit)
	}
	var out LogTailResponse
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
