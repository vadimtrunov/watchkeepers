package keepclient

import (
	"context"
	"encoding/json"
	"net/http"
)

// LogAppendRequest is the typed request body for [Client.LogAppend]. Field
// names and `omitempty` placement mirror the server's `logAppendRequest`
// shape verbatim (handlers_write.go). The struct intentionally has NO
// `scope` or `actor_*` fields — both are stamped server-side from the
// verified claim, so a body field would either be silently ignored or
// rejected by DisallowUnknownFields.
type LogAppendRequest struct {
	// EventType is the row's event_type column (free-form string).
	// Empty EventType is rejected client-side with [ErrInvalidRequest].
	EventType string `json:"event_type"`
	// CorrelationID groups related events. Optional and `omitempty` so an
	// unset value never reaches the wire; the server requires a canonical
	// UUID when present and rejects malformed values with 400.
	CorrelationID string `json:"correlation_id,omitempty"`
	// Payload is the row's jsonb payload, kept as raw JSON. Optional —
	// when omitted the server applies its default ('{}'::jsonb).
	Payload json.RawMessage `json:"payload,omitempty"`
}

// LogAppendResponse mirrors the server's `logAppendResponse` envelope
// returned by a successful POST /v1/keepers-log.
type LogAppendResponse struct {
	// ID is the freshly-inserted keepers_log row UUID.
	ID string `json:"id"`
}

// LogAppend calls POST /v1/keepers-log on the configured Keep service. It
// validates the request client-side (non-empty EventType) and surfaces
// transport / server errors per the M2.8.a taxonomy: missing token source
// returns [ErrNoTokenSource]; non-2xx responses surface as [*ServerError]
// whose Unwrap() matches the documented sentinels.
func (c *Client) LogAppend(ctx context.Context, req LogAppendRequest) (*LogAppendResponse, error) {
	if req.EventType == "" {
		return nil, ErrInvalidRequest
	}
	var out LogAppendResponse
	if err := c.do(ctx, http.MethodPost, "/v1/keepers-log", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
