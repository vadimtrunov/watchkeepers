package keepclient

import (
	"context"
	"net/http"
)

// StoreRequest is the typed request body for [Client.Store]. Field names and
// `omitempty` placement mirror the server's `storeRequest` shape verbatim
// (handlers_write.go). The struct intentionally has NO `scope` field — scope
// is token-bound on the server, so a body field would either be silently
// ignored or rejected by DisallowUnknownFields. Both Content and Embedding
// are validated client-side before any network round-trip.
type StoreRequest struct {
	// Subject is the optional subject column for the knowledge_chunk row.
	// Empty Subject is omitted from the wire so the server's
	// DisallowUnknownFields decoder never sees a stray empty key.
	Subject string `json:"subject,omitempty"`
	// Content is the indexed text content. Empty Content is rejected
	// client-side with [ErrInvalidRequest].
	Content string `json:"content"`
	// Embedding is the chunk vector. The server requires exactly 1536 floats
	// (knowledge_chunk.embedding column dimension); the client only enforces
	// "non-empty" so a future schema migration does not require a release.
	Embedding []float32 `json:"embedding"`
}

// StoreResponse mirrors the server's `storeResponse` envelope returned by a
// successful POST /v1/knowledge-chunks. The bare `{"id":"<uuid>"}` shape
// keeps the contract minimal; callers fetch the full row via [Client.Search].
type StoreResponse struct {
	// ID is the freshly-inserted knowledge_chunk row UUID.
	ID string `json:"id"`
}

// Store calls POST /v1/knowledge-chunks on the configured Keep service. It
// validates the request client-side (non-empty Content, non-empty Embedding)
// and surfaces transport / server errors per the M2.8.a taxonomy: missing
// token source returns [ErrNoTokenSource]; non-2xx responses surface as
// [*ServerError] whose Unwrap() matches the documented sentinels.
func (c *Client) Store(ctx context.Context, req StoreRequest) (*StoreResponse, error) {
	if req.Content == "" || len(req.Embedding) == 0 {
		return nil, ErrInvalidRequest
	}
	var out StoreResponse
	if err := c.do(ctx, http.MethodPost, "/v1/knowledge-chunks", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
