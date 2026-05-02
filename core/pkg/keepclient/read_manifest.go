package keepclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// ManifestVersion mirrors the server's `manifestVersionResponse` shape
// returned by GET /v1/manifests/{manifest_id}. Field names and `omitempty`
// placement match the server verbatim. The jsonb columns (Tools,
// AuthorityMatrix, KnowledgeSources) are kept as [json.RawMessage] so a
// future schema evolution does not require a client release; callers that
// need typed access decode them locally.
type ManifestVersion struct {
	// ID is the manifest_version row UUID.
	ID string `json:"id"`
	// ManifestID is the parent manifest UUID.
	ManifestID string `json:"manifest_id"`
	// VersionNo is the monotonically-increasing version number for the
	// manifest. The server returns the highest version_no row.
	VersionNo int `json:"version_no"`
	// SystemPrompt is the manifest system prompt text.
	SystemPrompt string `json:"system_prompt"`
	// Tools is the jsonb tools column, kept as raw JSON.
	Tools json.RawMessage `json:"tools"`
	// AuthorityMatrix is the jsonb authority_matrix column, kept as raw JSON.
	AuthorityMatrix json.RawMessage `json:"authority_matrix"`
	// KnowledgeSources is the jsonb knowledge_sources column, kept as raw JSON.
	KnowledgeSources json.RawMessage `json:"knowledge_sources"`
	// Personality is the optional personality text (omitempty matches the
	// server: empty string is omitted from the wire response).
	Personality string `json:"personality,omitempty"`
	// Language is the optional language code (omitempty matches the server).
	Language string `json:"language,omitempty"`
	// CreatedAt is the row's created_at timestamp (RFC3339 on the wire).
	CreatedAt string `json:"created_at"`
}

// GetManifest calls GET /v1/manifests/{manifestID}. The server returns the
// manifest_version row with the highest version_no for the given manifest;
// a missing row surfaces as a [*ServerError] whose Unwrap matches
// [ErrNotFound]. Empty manifestID is rejected synchronously with
// [ErrInvalidRequest] before any network round-trip.
func (c *Client) GetManifest(ctx context.Context, manifestID string) (*ManifestVersion, error) {
	if manifestID == "" {
		return nil, ErrInvalidRequest
	}
	var out ManifestVersion
	// PathEscape so caller-supplied IDs with `/` or `?` cannot smuggle
	// extra path segments. The server validates UUID shape before any DB
	// work, so an escaped non-UUID still surfaces as a 4xx not a 5xx.
	if err := c.do(ctx, http.MethodGet, "/v1/manifests/"+url.PathEscape(manifestID), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
