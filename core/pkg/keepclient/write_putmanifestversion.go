package keepclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
)

// uuidPattern matches the canonical RFC 4122 text form (8-4-4-4-12 hex with
// hyphens, any version/variant). Mirrors the server-side regex; rejecting a
// non-UUID manifestID client-side spares a network round-trip on obvious
// bugs and keeps the path-escape belt-and-suspenders trivially safe.
//
//nolint:gochecknoglobals // intentional package-scoped precompiled regex.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// PutManifestVersionRequest is the typed request body for
// [Client.PutManifestVersion]. Field names and `omitempty` placement mirror
// the server's `putManifestVersionRequest` shape verbatim (handlers_write.go).
// The three jsonb columns (Tools, AuthorityMatrix, KnowledgeSources) are
// kept as [json.RawMessage] so a future schema evolution does not require a
// client release; SQL defaults at the column level cover the empty case.
type PutManifestVersionRequest struct {
	// VersionNo is the monotonically-increasing version number for the
	// manifest. Must be > 0 (rejected client-side with [ErrInvalidRequest]).
	VersionNo int `json:"version_no"`
	// SystemPrompt is the manifest system prompt text. Empty SystemPrompt
	// is rejected client-side with [ErrInvalidRequest].
	SystemPrompt string `json:"system_prompt"`
	// Tools is the jsonb tools column, kept as raw JSON. Optional —
	// `omitempty` so an unset value never reaches the wire and the
	// server's column default ('[]'::jsonb) fires.
	Tools json.RawMessage `json:"tools,omitempty"`
	// AuthorityMatrix is the jsonb authority_matrix column. Optional —
	// `omitempty` so the server's default ('{}'::jsonb) fires when absent.
	AuthorityMatrix json.RawMessage `json:"authority_matrix,omitempty"`
	// KnowledgeSources is the jsonb knowledge_sources column. Optional —
	// `omitempty` so the server's default ('[]'::jsonb) fires when absent.
	KnowledgeSources json.RawMessage `json:"knowledge_sources,omitempty"`
	// Personality is the optional personality text. `omitempty` so an
	// empty string round-trips to SQL NULL on the server.
	Personality string `json:"personality,omitempty"`
	// Language is the optional language code. `omitempty` so an empty
	// string round-trips to SQL NULL on the server.
	Language string `json:"language,omitempty"`
}

// PutManifestVersionResponse mirrors the server's
// `putManifestVersionResponse` envelope returned by a successful
// PUT /v1/manifests/{manifest_id}/versions.
type PutManifestVersionResponse struct {
	// ID is the freshly-inserted manifest_version row UUID.
	ID string `json:"id"`
}

// PutManifestVersion calls PUT /v1/manifests/{manifestID}/versions on the
// configured Keep service. It validates the request client-side (canonical
// UUID manifestID, positive VersionNo, non-empty SystemPrompt) and surfaces
// transport / server errors per the M2.8.a taxonomy. A duplicate
// (manifest_id, version_no) pair surfaces as a [*ServerError] whose Unwrap()
// matches [ErrConflict] (status 409).
//
// The manifestID is URL-escaped via [url.PathEscape] so a caller cannot break
// the path even though the preflight already rejects non-canonical UUIDs.
func (c *Client) PutManifestVersion(ctx context.Context, manifestID string, req PutManifestVersionRequest) (*PutManifestVersionResponse, error) {
	if manifestID == "" || !uuidPattern.MatchString(manifestID) {
		return nil, ErrInvalidRequest
	}
	if req.VersionNo <= 0 || req.SystemPrompt == "" {
		return nil, ErrInvalidRequest
	}
	var out PutManifestVersionResponse
	path := "/v1/manifests/" + url.PathEscape(manifestID) + "/versions"
	if err := c.do(ctx, http.MethodPut, path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
