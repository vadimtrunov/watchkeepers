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
// AuthorityMatrix, KnowledgeSources, ImmutableCore) are kept as
// [json.RawMessage] so a future schema evolution does not require a
// client release; callers that need typed access decode them locally.
//
// Phase 2 §M3.3 adds three optional metadata fields: [ManifestVersion.Reason]
// (free-text rationale, capped at 1024 codepoints), [ManifestVersion.PreviousVersionID]
// (UUID of the manifest_version this row is derived from, NULL for root),
// and [ManifestVersion.Proposer] (free-text actor identifier, capped at
// 256 codepoints). All three are `omitempty` so legacy rows that predate
// M3.3 round-trip as a wire response with no schema change.
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
	// Model is the optional LLM model identifier the manifest pins to
	// (omitempty matches the server: empty string is omitted from the wire
	// response). Capped at 100 Unicode codepoints by the server CHECK
	// constraint (migration 014); the client mirrors the cap on PUT.
	Model string `json:"model,omitempty"`
	// Autonomy is the optional manifest autonomy level (omitempty matches
	// the server: empty string is omitted from the wire response). When
	// non-empty, must be one of {"supervised", "autonomous"} per the server
	// CHECK enum constraint (migration 015); the client mirrors the
	// constraint on PUT.
	Autonomy string `json:"autonomy,omitempty"`
	// NotebookTopK is the optional notebook recall top-K count (omitempty
	// matches the server: zero is omitted from the wire response). When
	// non-zero, must be in [1, 100]; the client mirrors the range on PUT
	// (migration 016). Zero is treated as "unset" and the runtime uses its
	// own default.
	NotebookTopK int `json:"notebook_top_k,omitempty"`
	// NotebookRelevanceThreshold is the optional notebook recall relevance
	// threshold (omitempty matches the server: zero is omitted from the wire
	// response). When non-zero, must be in (0, 1]; the client mirrors the
	// range on PUT (migration 016). Zero is treated as "unset".
	NotebookRelevanceThreshold float64 `json:"notebook_relevance_threshold,omitempty"`
	// ImmutableCore is the optional manifest immutable_core jsonb column,
	// kept as raw JSON (matches the Tools / AuthorityMatrix /
	// KnowledgeSources precedent — a future bucket extension does NOT
	// require a client release). When present on the wire the server
	// CHECK constraint (migration 030) guarantees it is a JSON object;
	// an empty / absent column round-trips as a nil [json.RawMessage] via
	// `omitempty`.
	//
	// The five buckets carried by the object (see Phase 2 §M3.1 in
	// `docs/ROADMAP-phase2.md`) are `role_boundaries`,
	// `security_constraints`, `escalation_protocols`, `cost_limits`, and
	// `audit_requirements`. M3.1 is schema-only — the admin-only
	// editability enforcement lands in M3.2 (handler-layer) and the
	// self-tuning validator lands in M3.6. The typed projection into
	// [runtime.Manifest.ImmutableCore] lives in the M3.1 manifest loader
	// extension.
	ImmutableCore json.RawMessage `json:"immutable_core,omitempty"`
	// Reason is the optional free-text rationale the proposer attached to
	// the manifest_version row (Phase 2 §M3.3). Capped at 1024 Unicode
	// codepoints by the server CHECK constraint
	// `manifest_version_reason_length` (migration 031). Legacy rows
	// predating M3.3 round-trip as the empty string (the server uses
	// `coalesce(reason, '')` so the wire never carries `null`) and
	// `omitempty` drops the key entirely from the response so legacy
	// callers observe no schema change.
	Reason string `json:"reason,omitempty"`
	// PreviousVersionID is the optional UUID of the manifest_version row
	// this version is derived from (Phase 2 §M3.3). NULL for the root
	// version of every manifest (no previous). When non-empty the FK
	// target lives in the same manifest_version table — cross-manifest
	// references are impossible by construction once callers respect
	// `manifest_id`-scoped reads (M3.4 `manifest.history` is the
	// authoritative consumer). Pointer-typed so the SQL NULL case
	// projects as a Go nil rather than the empty string; `omitempty`
	// then drops the key from the wire.
	PreviousVersionID *string `json:"previous_version_id,omitempty"`
	// Proposer is the optional free-text identifier of the actor that
	// proposed this manifest_version row (Phase 2 §M3.3). The M3.4
	// tools may write a Watchkeeper UUID, a human handle, or the
	// literal string "watchmaster" for system-initiated rollback
	// proposals — no FK constraint at this milestone. Capped at 256
	// Unicode codepoints by the server CHECK constraint
	// `manifest_version_proposer_length` (migration 031). Legacy rows
	// round-trip as the empty string; `omitempty` drops the key from
	// the response.
	Proposer string `json:"proposer,omitempty"`
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
