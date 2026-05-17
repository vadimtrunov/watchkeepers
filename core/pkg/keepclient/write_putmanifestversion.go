package keepclient

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"unicode/utf8"
)

// uuidPattern matches the canonical RFC 4122 text form (8-4-4-4-12 hex with
// hyphens, any version/variant). Mirrors the server-side regex; rejecting a
// non-UUID manifestID client-side spares a network round-trip on obvious
// bugs and keeps the path-escape belt-and-suspenders trivially safe.
//
//nolint:gochecknoglobals // intentional package-scoped precompiled regex.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// clientLanguagePattern mirrors the server-side `languagePattern` (and the
// SQL CHECK from migration 010) used for [PutManifestVersionRequest.Language]:
// 2-3 lowercase letters (ISO 639-1/-3) optionally followed by a 2-letter
// uppercase ISO 3166-1 region. Rejecting client-side spares a network
// round-trip on the obvious malformed shapes ("english", "EN", "en-us").
//
//nolint:gochecknoglobals // intentional package-scoped precompiled regex.
var clientLanguagePattern = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z]{2})?$`)

// clientPersonalityMaxRunes mirrors the server-side
// `manifestPersonalityMaxRunes` cap and the SQL `char_length(personality)
// <= 1024` CHECK from migration 010. Counted in Unicode codepoints so a
// CJK / accented-Latin payload cannot smuggle past a byte-based cap.
const clientPersonalityMaxRunes = 1024

// manifestModelMaxRunes mirrors the server-side `manifestModelMaxRunes`
// cap and the SQL `char_length(model) <= 100` CHECK from migration 014.
// Counted in Unicode codepoints so a non-ASCII payload cannot smuggle
// past a byte-based cap.
const manifestModelMaxRunes = 100

// manifestAutonomyAllowed mirrors the server-side enum CHECK constraint
// from migration 015 (`autonomy IN ('supervised','autonomous')`) plus the
// NULL/empty-string case (server treats NULL as the runtime default of
// "supervised"). Membership-test set so a future enum extension stays a
// single-line edit.
//
//nolint:gochecknoglobals // intentional package-scoped immutable allowed-set.
var manifestAutonomyAllowed = map[string]struct{}{
	"":           {},
	"supervised": {},
	"autonomous": {},
}

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
	// Personality is the optional free-text personality. Capped at 1024
	// Unicode codepoints (matching SQL char_length semantics). Server and
	// DB CHECK constraint (migration 010) enforce the same cap.
	Personality string `json:"personality,omitempty"`
	// Language is the optional language code. When non-empty, it must
	// match BCP 47-lite shape `<lang>(-<REGION>)?`: 2-3 lowercase letters
	// for ISO 639-1/-3, optionally followed by a 2-letter ISO 3166-1
	// uppercase region (e.g. "en", "en-US", "pt-BR", "kab"). Server and
	// DB CHECK constraint (migration 010) enforce the same regex.
	Language string `json:"language,omitempty"`
	// Model is the optional LLM model identifier the manifest pins to.
	// Capped at 100 Unicode codepoints (utf8.RuneCountInString, not len)
	// to mirror SQL char_length semantics. Server and DB CHECK constraint
	// (migration 014) enforce the same cap.
	Model string `json:"model,omitempty"`
	// Autonomy is the optional manifest autonomy level. When non-empty,
	// must be one of {"supervised", "autonomous"} — the empty string
	// round-trips as SQL NULL and the server defaults the runtime to
	// "supervised". Server and DB CHECK constraint (migration 015)
	// enforce the same enum.
	Autonomy string `json:"autonomy,omitempty"`
	// NotebookTopK is the optional notebook recall top-K count. When
	// non-zero, must be in [1, 100]; zero round-trips as SQL NULL (treated
	// as "unset"; omitempty drops it from the wire). Server and DB CHECK
	// constraint (migration 016) enforce the same range.
	NotebookTopK int `json:"notebook_top_k,omitempty"`
	// NotebookRelevanceThreshold is the optional notebook recall relevance
	// threshold. When non-zero, must be in (0, 1]; zero round-trips as SQL
	// NULL (treated as "unset"; omitempty drops it from the wire). Server
	// and DB CHECK constraint (migration 016) enforce the same range.
	NotebookRelevanceThreshold float64 `json:"notebook_relevance_threshold,omitempty"`
	// ImmutableCore is the optional manifest immutable_core jsonb column,
	// kept as raw JSON (matches the Tools / AuthorityMatrix /
	// KnowledgeSources precedent). When non-empty, the JSON document MUST
	// be a top-level object — the server CHECK constraint
	// `manifest_version_immutable_core_shape` (migration 030) and the
	// server-side `parsePutManifestVersionRequest` both enforce object
	// shape and surface the stable 400 reason `invalid_immutable_core`
	// on a non-object payload. The client mirrors the same precheck so
	// the malformed shape (array / scalar / JSON `null` literal)
	// short-circuits before the network hit.
	//
	// The five buckets carried by the object (see Phase 2 §M3.1 in
	// `docs/ROADMAP-phase2.md`) are `role_boundaries`,
	// `security_constraints`, `escalation_protocols`, `cost_limits`, and
	// `audit_requirements`. M3.1 is schema-only — admin-only editability
	// enforcement lands in M3.2 and the self-tuning validator lands in
	// M3.6, so the client does NOT presume to validate bucket contents
	// here.
	ImmutableCore json.RawMessage `json:"immutable_core,omitempty"`
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
	// Symmetric with parsePutManifestVersionRequest on the server: an
	// empty Language is allowed (round-trips as SQL NULL); a non-empty
	// value must match the BCP 47-lite shape. Personality is capped at
	// 1024 Unicode codepoints (utf8.RuneCountInString, not len) to mirror
	// SQL char_length semantics. Both checks short-circuit before any
	// network hit.
	if req.Language != "" && !clientLanguagePattern.MatchString(req.Language) {
		return nil, ErrInvalidRequest
	}
	if utf8.RuneCountInString(req.Personality) > clientPersonalityMaxRunes {
		return nil, ErrInvalidRequest
	}
	// Model mirrors the server's CHECK ≤ 100 runes (migration 014).
	// Boundary at exactly 100 is accepted; rune-count semantics so a
	// non-ASCII payload cannot smuggle past a byte-based cap.
	if utf8.RuneCountInString(req.Model) > manifestModelMaxRunes {
		return nil, ErrInvalidRequest
	}
	// Autonomy mirrors the server's CHECK enum (migration 015): empty,
	// "supervised", or "autonomous". Anything else short-circuits before
	// the network hit so the caller sees ErrInvalidRequest, not a 400 from
	// the server.
	if _, ok := manifestAutonomyAllowed[req.Autonomy]; !ok {
		return nil, ErrInvalidRequest
	}
	// NotebookTopK mirrors the server's CHECK range (migration 016):
	// [0, 100] where 0 means "unset". Out-of-range short-circuits before
	// the network hit.
	if req.NotebookTopK < 0 || req.NotebookTopK > 100 {
		return nil, ErrInvalidRequest
	}
	// NotebookRelevanceThreshold mirrors the server's CHECK range
	// (migration 016): [0, 1] where 0 means "unset". Out-of-range
	// short-circuits before the network hit.
	if req.NotebookRelevanceThreshold < 0 || req.NotebookRelevanceThreshold > 1 {
		return nil, ErrInvalidRequest
	}
	// ImmutableCore mirrors the server's CHECK shape (migration 030) +
	// the `parsePutManifestVersionRequest` precheck: an empty / nil
	// RawMessage round-trips as SQL NULL via `omitempty`; any non-empty
	// payload MUST be a JSON object. Reject arrays / scalars / the JSON
	// `null` literal client-side so the caller sees ErrInvalidRequest
	// rather than a 400 from the server. The bucket contents themselves
	// are NOT validated here — admin-only enforcement lands in M3.2 and
	// the self-tuning validator lands in M3.6.
	if !isJSONObjectOrEmpty(req.ImmutableCore) {
		return nil, ErrInvalidRequest
	}
	var out PutManifestVersionResponse
	path := "/v1/manifests/" + url.PathEscape(manifestID) + "/versions"
	if err := c.do(ctx, http.MethodPut, path, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// isJSONObjectOrEmpty returns true when raw is empty / nil (the
// `omitempty` round-trip case) OR carries a JSON object literal
// (`{...}`). Arrays, scalars, and the JSON `null` literal return
// false — mirrors the server-side `manifest_version_immutable_core_shape`
// CHECK from migration 030 plus the `parsePutManifestVersionRequest`
// precheck on the server.
//
// The check is structural: it strips JSON whitespace via
// [json.Valid] + a single-byte open-brace probe through
// [bytes.TrimLeft] (whitespace set per JSON RFC 8259 §2 — space, tab,
// LF, CR). Validation of the payload's bucket contents is intentionally
// out of scope for M3.1 (admin-only editability lands in M3.2; the
// self-tuning validator lands in M3.6).
func isJSONObjectOrEmpty(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return true
	}
	if !json.Valid(raw) {
		return false
	}
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	return len(trimmed) > 0 && trimmed[0] == '{'
}
