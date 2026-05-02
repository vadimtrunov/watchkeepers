package keepclient

import (
	"context"
	"net/http"
)

// SearchRequest is the typed request body for [Client.Search]. Embedding is
// the query vector; TopK caps the number of result rows. Both fields are
// validated client-side before any network round-trip — empty Embedding or
// non-positive TopK return [ErrInvalidRequest] synchronously to spare a round
// trip on obvious bugs.
type SearchRequest struct {
	// Embedding is the query vector. The server expects a non-empty slice
	// whose length matches the column's declared dimension (1536 for the
	// current schema, see knowledge_chunk.embedding); the client only
	// rejects the empty case so future schema migrations don't require a
	// client release.
	Embedding []float32 `json:"embedding"`
	// TopK is the maximum number of result rows to return. Must be > 0.
	// The server clamps the upper bound (see maxSearchTopK in the Keep
	// server); the client only enforces the lower bound.
	TopK int `json:"top_k"`
}

// SearchResult mirrors a single row from the server's POST /v1/search
// response. Field names and `omitempty` placement match the server's
// `searchResult` shape verbatim so a future field addition on the server
// surfaces as a zero value here without a client release.
type SearchResult struct {
	// ID is the knowledge_chunk row UUID.
	ID string `json:"id"`
	// Subject is the optional subject column (omitempty matches the server).
	Subject string `json:"subject,omitempty"`
	// Content is the indexed text content.
	Content string `json:"content"`
	// CreatedAt is the row's created_at timestamp (RFC3339 on the wire).
	CreatedAt string `json:"created_at"`
	// Distance is the pgvector cosine distance for this row. The server
	// snaps NaN/±Inf to 2.0 (the maximum cosine distance) so the client
	// always sees a finite float64.
	Distance float64 `json:"distance"`
}

// SearchResponse mirrors the server's `searchResponse` envelope.
type SearchResponse struct {
	// Results is the list of matching rows in ascending-distance order
	// (closest first). Empty when no rows match the scope.
	Results []SearchResult `json:"results"`
}

// Search calls POST /v1/search on the configured Keep service. It validates
// the request client-side (non-empty Embedding, positive TopK) and surfaces
// transport / server errors per the M2.8.a taxonomy: missing token source
// returns [ErrNoTokenSource]; non-2xx responses surface as [*ServerError]
// whose Unwrap() matches the documented sentinels.
func (c *Client) Search(ctx context.Context, req SearchRequest) (*SearchResponse, error) {
	if len(req.Embedding) == 0 || req.TopK <= 0 {
		return nil, ErrInvalidRequest
	}
	var out SearchResponse
	if err := c.do(ctx, http.MethodPost, "/v1/search", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
