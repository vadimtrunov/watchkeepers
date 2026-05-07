package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// maxRequestBodyBytes is the shared 1 MiB cap on request body size across
// every write endpoint. Mirrors maxSearchBodyBytes on the read side — the
// value is identical (1 MiB) and the naming is simply broader because the
// write handlers each carry a distinct payload shape. A single client
// cannot force unbounded allocation by streaming a multi-GB JSON body.
const maxRequestBodyBytes = 1 << 20

// knowledgeChunkEmbeddingDim is the exact vector dimension required by the
// knowledge_chunk.embedding column (declared vector(1536) in migration 004).
// Any store request with a different number of floats is rejected with
// 400 invalid_embedding before the row reaches Postgres.
// maxEmbeddingDim (defined in handlers_read.go) is preserved for the read
// path's looser upper-bound check; only the write path enforces exact parity.
const knowledgeChunkEmbeddingDim = 1536

// uuidPattern matches the canonical RFC 4122 text form (8-4-4-4-12 hex with
// hyphens, any version/variant). We compile it once at package scope so the
// per-request check stays allocation-free. Used to validate the uuid body
// in `agent:<uuid>` / `user:<uuid>` before we hand it to Postgres as a
// typed parameter.
//
//nolint:gochecknoglobals // intentional module-scoped precompiled regex.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// languagePattern enforces the BCP 47-lite shape accepted for the
// manifest_version.language column: 2-3 lowercase letters (ISO 639-1/-3),
// optionally followed by a 2-letter uppercase ISO 3166-1 region (e.g. "en",
// "en-US", "pt-BR", "kab", "eng"). Mirrored at the SQL layer by the
// `manifest_version_language_format` CHECK constraint (migration 010); the
// server-side check returns a stable 400 reason code before Postgres
// surfaces a `23514` check_violation.
//
//nolint:gochecknoglobals // intentional module-scoped precompiled regex.
var languagePattern = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z]{2})?$`)

// manifestPersonalityMaxRunes is the per-row cap on the
// manifest_version.personality column expressed in Unicode codepoints, to
// match SQL `char_length` semantics enforced by the
// `manifest_version_personality_length` CHECK constraint (migration 010).
// `len(s)` would count bytes — multi-byte runes (e.g. CJK, accented Latin)
// would slip past a byte-based cap and only fail at the DB.
const manifestPersonalityMaxRunes = 1024

// manifestModelMaxRunes is the per-row cap on the manifest_version.model
// column expressed in Unicode codepoints, to match SQL `char_length`
// semantics enforced by the `manifest_version_model_length` CHECK
// constraint (migration 014). Same byte-vs-rune rationale as
// `manifestPersonalityMaxRunes` above.
const manifestModelMaxRunes = 100

// manifestAutonomyAllowed is the closed set of accepted values for the
// manifest_version.autonomy wire field, mirroring the SQL CHECK
// `manifest_version_autonomy_enum` from migration 015 and the runtime
// `AutonomyLevel` enum constants `runtime.AutonomySupervised` /
// `runtime.AutonomyAutonomous` (see core/pkg/runtime/runtime.go:33-48).
// `runtime.AutonomyManual` is intentionally absent at this milestone:
// M5.5.b.c.a is wire-schema-first and does not yet ship the manual flow.
// Empty `""` is accepted (round-trips to SQL NULL → runtime defaults to
// supervised) and short-circuits the lookup before this slice is consulted.
//
//nolint:gochecknoglobals // intentional module-scoped enum-membership set.
var manifestAutonomyAllowed = []string{"supervised", "autonomous"}

// -----------------------------------------------------------------------
// POST /v1/knowledge-chunks — handleStore
// -----------------------------------------------------------------------

// storeRequest is the JSON body accepted by POST /v1/knowledge-chunks.
// `scope` is intentionally absent from the struct so the field is rejected
// by DisallowUnknownFields: scope is token-bound, never client-supplied.
type storeRequest struct {
	Subject   string    `json:"subject"`
	Content   string    `json:"content"`
	Embedding []float32 `json:"embedding"`
}

// storeResponse is the 201 body returned by POST /v1/knowledge-chunks.
// A bare `{"id":"<uuid>"}` keeps the contract minimal; callers fetch the
// full row via POST /v1/search or a future dedicated GET if needed.
type storeResponse struct {
	ID string `json:"id"`
}

// parseStoreRequest validates the Content-Type, caps the body size, decodes
// and validates the JSON payload, and enforces the embedding / content
// bounds. Mirrors parseSearchRequest; extracted so handleStore stays within
// the gocyclo budget.
func parseStoreRequest(w http.ResponseWriter, req *http.Request) (storeRequest, bool) {
	var body storeRequest

	if !isJSONContentType(req.Header.Get("Content-Type")) {
		writeErrorReason(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "expected_application_json")
		return body, false
	}

	req.Body = http.MaxBytesReader(w, req.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErrorReason(w, http.StatusRequestEntityTooLarge, "request_too_large", "body_too_large")
			return body, false
		}
		writeError(w, http.StatusBadRequest, "invalid_body")
		return body, false
	}
	if body.Content == "" {
		writeError(w, http.StatusBadRequest, "missing_content")
		return body, false
	}
	if len(body.Embedding) == 0 {
		writeError(w, http.StatusBadRequest, "missing_embedding")
		return body, false
	}
	if len(body.Embedding) != knowledgeChunkEmbeddingDim {
		writeError(w, http.StatusBadRequest, "invalid_embedding")
		return body, false
	}
	return body, true
}

// handleStore serves POST /v1/knowledge-chunks. It validates the body,
// inserts one row into watchkeeper.knowledge_chunk under the scoped tx
// (scope = claim.Scope; clients cannot override), and returns the new id.
// RLS WITH CHECK from migration 005 is the final backstop against cross-
// scope writes even if the handler regressed.
func handleStore(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		body, ok := parseStoreRequest(w, req)
		if !ok {
			return
		}

		vec := embeddingToVector(body.Embedding)

		var id string
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			// `subject` is nullable in the table; pass empty string as NULL
			// via NULLIF so the wire shape stays simple (`"subject": ""`
			// round-trips to SQL NULL, matching the read-side contract that
			// hides empty subjects behind `coalesce(subject, '')`).
			return tx.QueryRow(ctx, `
                INSERT INTO watchkeeper.knowledge_chunk (scope, subject, content, embedding)
                VALUES ($1, NULLIF($2, ''), $3, $4::vector)
                RETURNING id
            `, claim.Scope, body.Subject, body.Content, vec).Scan(&id)
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "store_failed")
			return
		}

		writeJSON(w, http.StatusCreated, storeResponse{ID: id})
	})
}

// -----------------------------------------------------------------------
// POST /v1/keepers-log — handleLogAppend
// -----------------------------------------------------------------------

// logAppendRequest is the JSON body accepted by POST /v1/keepers-log.
// Actor columns are intentionally absent — they are stamped server-side
// from the token's scope, and client-supplied actor_* keys are rejected
// by DisallowUnknownFields (AC2 security).
type logAppendRequest struct {
	EventType     string          `json:"event_type"`
	CorrelationID string          `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// logAppendResponse is the 201 body returned by POST /v1/keepers-log.
type logAppendResponse struct {
	ID string `json:"id"`
}

// parseLogAppendRequest handles the 415 / 413 / 400 envelope and the
// field-level validation for POST /v1/keepers-log.
func parseLogAppendRequest(w http.ResponseWriter, req *http.Request) (logAppendRequest, bool) {
	var body logAppendRequest

	if !isJSONContentType(req.Header.Get("Content-Type")) {
		writeErrorReason(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "expected_application_json")
		return body, false
	}

	req.Body = http.MaxBytesReader(w, req.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErrorReason(w, http.StatusRequestEntityTooLarge, "request_too_large", "body_too_large")
			return body, false
		}
		writeError(w, http.StatusBadRequest, "invalid_body")
		return body, false
	}
	if body.EventType == "" {
		writeError(w, http.StatusBadRequest, "missing_event_type")
		return body, false
	}
	return body, true
}

// actorFromScope maps a verified claim.Scope to the (actor_watchkeeper_id,
// actor_human_id) pair to stamp on a keepers_log row. `agent:<uuid>` fills
// actor_watchkeeper_id; `user:<uuid>` fills actor_human_id; `org` leaves
// both NULL. Returns ok=false when a `user:` / `agent:` payload is not a
// canonical UUID — the caller should translate that into 400
// invalid_scope_uuid. The token's Subject is opaque and is not used here.
func actorFromScope(scope string) (watchkeeperID, humanID *string, ok bool) {
	if rest, found := strings.CutPrefix(scope, "agent:"); found {
		if !uuidPattern.MatchString(rest) {
			return nil, nil, false
		}
		id := rest
		return &id, nil, true
	}
	if rest, found := strings.CutPrefix(scope, "user:"); found {
		if !uuidPattern.MatchString(rest) {
			return nil, nil, false
		}
		id := rest
		return nil, &id, true
	}
	// scope == "org" or any other shape reaches here; the middleware will
	// already have rejected non-valid scopes, so anything else is an org
	// token (both actor columns NULL).
	return nil, nil, true
}

// handleLogAppend serves POST /v1/keepers-log. It validates the body,
// derives the actor columns from the verified claim (never from the body
// or query string), inserts one row under the scoped tx, and returns the
// server-generated id.
func handleLogAppend(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		body, ok := parseLogAppendRequest(w, req)
		if !ok {
			return
		}

		watchkeeperID, humanID, scopeOK := actorFromScope(claim.Scope)
		if !scopeOK {
			writeError(w, http.StatusBadRequest, "invalid_scope_uuid")
			return
		}

		// correlation_id is optional but must be a canonical UUID when present;
		// a malformed value would reach Postgres as an invalid uuid cast and
		// surface as a confusing 500 — reject it early with a stable 400.
		if body.CorrelationID != "" && !uuidPattern.MatchString(body.CorrelationID) {
			writeError(w, http.StatusBadRequest, "invalid_correlation_id")
			return
		}

		// Pass NULL when empty so the FK-free nullable column carries SQL NULL
		// rather than an empty string that would fail the uuid cast.
		var correlation any
		if body.CorrelationID != "" {
			correlation = body.CorrelationID
		}

		// payload defaults to the SQL default '{}'::jsonb when the client
		// omits it; pass a valid empty object literal rather than SQL NULL
		// (the column is NOT NULL with a jsonb default).
		payload := body.Payload
		if len(payload) == 0 {
			payload = json.RawMessage(`{}`)
		}

		var id string
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
                INSERT INTO watchkeeper.keepers_log (
                    event_type, correlation_id,
                    actor_watchkeeper_id, actor_human_id,
                    payload
                )
                VALUES ($1, $2, $3, $4, $5::jsonb)
                RETURNING id
            `, body.EventType, correlation, watchkeeperID, humanID, string(payload)).Scan(&id)
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "log_append_failed")
			return
		}

		writeJSON(w, http.StatusCreated, logAppendResponse{ID: id})
	})
}

// -----------------------------------------------------------------------
// PUT /v1/manifests/{manifest_id}/versions — handlePutManifestVersion
// -----------------------------------------------------------------------

// putManifestVersionRequest is the JSON body accepted by
// PUT /v1/manifests/{manifest_id}/versions. The three jsonb columns
// (`tools`, `authority_matrix`, `knowledge_sources`) are typed as
// json.RawMessage so the handler can round-trip any valid JSON without
// re-shaping it; SQL defaults cover the empty case.
type putManifestVersionRequest struct {
	VersionNo                  int             `json:"version_no"`
	SystemPrompt               string          `json:"system_prompt"`
	Tools                      json.RawMessage `json:"tools"`
	AuthorityMatrix            json.RawMessage `json:"authority_matrix"`
	KnowledgeSources           json.RawMessage `json:"knowledge_sources"`
	Personality                string          `json:"personality"`
	Language                   string          `json:"language"`
	Model                      string          `json:"model"`
	Autonomy                   string          `json:"autonomy"`
	NotebookTopK               int             `json:"notebook_top_k"`
	NotebookRelevanceThreshold float64         `json:"notebook_relevance_threshold"`
}

// putManifestVersionResponse is the 201 body returned on successful insert.
type putManifestVersionResponse struct {
	ID string `json:"id"`
}

// parsePutManifestVersionRequest handles the 415 / 413 / 400 envelope and
// the field-level validation for PUT /v1/manifests/{manifest_id}/versions.
//
//nolint:gocyclo // sequential field validations; each branch is a distinct AC; splitting would obscure the validation contract.
func parsePutManifestVersionRequest(w http.ResponseWriter, req *http.Request) (putManifestVersionRequest, bool) {
	var body putManifestVersionRequest

	if !isJSONContentType(req.Header.Get("Content-Type")) {
		writeErrorReason(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "expected_application_json")
		return body, false
	}

	req.Body = http.MaxBytesReader(w, req.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErrorReason(w, http.StatusRequestEntityTooLarge, "request_too_large", "body_too_large")
			return body, false
		}
		writeError(w, http.StatusBadRequest, "invalid_body")
		return body, false
	}
	if body.VersionNo <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_version_no")
		return body, false
	}
	if body.SystemPrompt == "" {
		writeError(w, http.StatusBadRequest, "missing_system_prompt")
		return body, false
	}
	// Symmetric with the SQL CHECK from migration 010: an empty Language
	// round-trips as SQL NULL (allowed); a non-empty value must match the
	// BCP 47-lite shape. Reject before the row hits Postgres so the caller
	// gets a stable `invalid_language` reason instead of an opaque 500.
	if body.Language != "" && !languagePattern.MatchString(body.Language) {
		writeError(w, http.StatusBadRequest, "invalid_language")
		return body, false
	}
	// Mirror the SQL `char_length(personality) <= 1024` cap. Use
	// utf8.RuneCountInString — `len(body.Personality)` counts bytes and
	// would let a 1024-rune CJK / accented-Latin payload (each rune up to
	// 4 bytes) bypass the cap on the wire only to fail later at Postgres.
	if utf8.RuneCountInString(body.Personality) > manifestPersonalityMaxRunes {
		writeError(w, http.StatusBadRequest, "personality_too_long")
		return body, false
	}
	// Mirror the SQL `char_length(model) <= 100` cap from migration 014.
	// utf8.RuneCountInString matches `char_length` codepoint semantics so
	// a CJK / accented-Latin payload at the rune boundary cannot bypass
	// the cap on the wire only to fail later at Postgres.
	if utf8.RuneCountInString(body.Model) > manifestModelMaxRunes {
		writeError(w, http.StatusBadRequest, "model_too_long")
		return body, false
	}
	// Mirror the SQL `manifest_version_autonomy_enum` CHECK from migration
	// 015 plus the `runtime.AutonomyLevel` enum (see runtime.go:33-48). An
	// empty Autonomy round-trips as SQL NULL (allowed; runtime defaults to
	// supervised); any non-empty value MUST be in `manifestAutonomyAllowed`.
	// Reject before the row hits Postgres so the caller gets a stable
	// `invalid_autonomy` reason instead of an opaque 500. Pattern mirrors
	// the `invalid_language` check above.
	if body.Autonomy != "" {
		ok := false
		for _, allowed := range manifestAutonomyAllowed {
			if body.Autonomy == allowed {
				ok = true
				break
			}
		}
		if !ok {
			writeError(w, http.StatusBadRequest, "invalid_autonomy")
			return body, false
		}
	}
	if !validateNotebookRecallFields(w, body) {
		return body, false
	}
	return body, true
}

// validateNotebookRecallFields checks the notebook_top_k and
// notebook_relevance_threshold field ranges on the PUT manifest version
// request body. It writes a 400 error and returns false on the first
// out-of-range value; returns true when both fields are acceptable.
// Extracted from parsePutManifestVersionRequest to keep that function
// under the gocyclo budget.
//
// Range rules (mirror migration 016 CHECK constraints):
//   - notebook_top_k: 0 (disabled) or 1–100 inclusive; negative or > 100 rejected.
//   - notebook_relevance_threshold: 0.0 (unset) or [0, 1] inclusive; < 0 or > 1 rejected.
func validateNotebookRecallFields(w http.ResponseWriter, body putManifestVersionRequest) bool {
	// Zero is accepted (means "auto-recall disabled" → intOrNil writes SQL
	// NULL); any non-zero value MUST satisfy `1 <= notebook_top_k <= 100`.
	// Negative values are also rejected with a stable `invalid_notebook_top_k`
	// reason so the caller gets a clear signal before the row reaches Postgres.
	if body.NotebookTopK < 0 || body.NotebookTopK > 100 {
		writeError(w, http.StatusBadRequest, "invalid_notebook_top_k")
		return false
	}
	// Zero is accepted (means "unset" → floatOrNil writes SQL NULL); any
	// non-zero value MUST satisfy `0 <= notebook_relevance_threshold <= 1`.
	if body.NotebookRelevanceThreshold < 0 || body.NotebookRelevanceThreshold > 1 {
		writeError(w, http.StatusBadRequest, "invalid_notebook_relevance_threshold")
		return false
	}
	return true
}

// handlePutManifestVersion serves PUT /v1/manifests/{manifest_id}/versions.
// It inserts a new manifest_version row under the scoped tx; a unique
// violation on `(manifest_id, version_no)` is translated to
// 409 version_conflict without leaking the raw Postgres error text.
//
// Cross-tenant posture (M3.5.a.3.2): the INSERT routes through a
// `WHERE EXISTS (SELECT 1 FROM watchkeeper.manifest WHERE id = $manifest_id
// AND organization_id = $claim_org)` subquery so a caller for org A
// cannot anchor a manifest_version at org B's manifest just by knowing
// the manifest UUID. The cross-tenant case produces no row through
// `RETURNING` → pgx.ErrNoRows → 404 not_found, mirroring the contract
// on handleInsertWatchkeeper. The schema half landed in M3.5.a.3.1
// (migration 013): `manifest.organization_id NOT NULL` plus per-role
// `ENABLE + FORCE ROW LEVEL SECURITY` on both `manifest` and
// `manifest_version`. RLS keyed off the `watchkeeper.org` GUC remains
// the defense-in-depth backstop; this handler-layer filter ensures the
// 404 surface (rather than an RLS-level error) by construction. An
// empty `claim.OrganizationID` (legacy pre-M3.5.a.1 token) is rejected
// up front with 403 organization_required before WithScope opens any
// transaction — no DB round-trip on legacy callers.
func handlePutManifestVersion(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		manifestID := req.PathValue("manifest_id")
		if manifestID == "" {
			writeError(w, http.StatusBadRequest, "missing_manifest_id")
			return
		}
		if !uuidPattern.MatchString(manifestID) {
			writeError(w, http.StatusBadRequest, "invalid_manifest_id")
			return
		}

		body, ok := parsePutManifestVersionRequest(w, req)
		if !ok {
			return
		}

		// M3.5.a.3.2: the claim must carry an explicit tenant. Phase 1
		// rejects legacy claims rather than fall through to an
		// unfiltered INSERT that would let any authenticated caller
		// write a version against any tenant's manifest. Fires before
		// WithScope so a malicious legacy token never opens a tx.
		if claim.OrganizationID == "" {
			writeError(w, http.StatusForbidden, "organization_required")
			return
		}

		// The three jsonb columns default to '[]' / '{}' / '[]' at the
		// table level; pass SQL NULL via nil interface when the client
		// omits the field so the default fires instead of Postgres
		// rejecting an empty json.RawMessage literal.
		tools := jsonbOrNil(body.Tools)
		authorityMatrix := jsonbOrNil(body.AuthorityMatrix)
		knowledgeSources := jsonbOrNil(body.KnowledgeSources)
		personality := stringOrNil(body.Personality)
		language := stringOrNil(body.Language)
		model := stringOrNil(body.Model)
		autonomy := stringOrNil(body.Autonomy)
		notebookTopK := intOrNil(body.NotebookTopK)
		notebookRelevanceThreshold := floatOrNil(body.NotebookRelevanceThreshold)

		var id string
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			// INSERT … SELECT … WHERE EXISTS shape mirrors
			// handleInsertWatchkeeper: Postgres rejects the row in a
			// single statement when the manifest's org does not match
			// the claim's tenant, returning no row through RETURNING.
			// The handler surfaces that as 404 not_found without
			// leaking row existence to the wrong tenant. The RLS
			// policy from migration 013 is the defense-in-depth
			// backstop; the explicit EXISTS keeps the 404 surface
			// (rather than the RLS-level error path) deterministic.
			return tx.QueryRow(
				ctx, `
                INSERT INTO watchkeeper.manifest_version (
                    manifest_id, version_no, system_prompt,
                    tools, authority_matrix, knowledge_sources,
                    personality, language, model, autonomy,
                    notebook_top_k, notebook_relevance_threshold
                )
                SELECT
                    $1, $2, $3,
                    coalesce($4::jsonb, '[]'::jsonb),
                    coalesce($5::jsonb, '{}'::jsonb),
                    coalesce($6::jsonb, '[]'::jsonb),
                    $7, $8, $9, $10,
                    $11, $12
                WHERE EXISTS (
                    SELECT 1 FROM watchkeeper.manifest
                    WHERE id = $1 AND organization_id = $13
                )
                RETURNING id
            `, manifestID, body.VersionNo, body.SystemPrompt,
				tools, authorityMatrix, knowledgeSources,
				personality, language, model, autonomy,
				notebookTopK, notebookRelevanceThreshold, claim.OrganizationID,
			).Scan(&id)
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "not_found")
				return
			}
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				writeError(w, http.StatusConflict, "version_conflict")
				return
			}
			writeError(w, http.StatusInternalServerError, "put_manifest_version_failed")
			return
		}

		writeJSON(w, http.StatusCreated, putManifestVersionResponse{ID: id})
	})
}

// jsonbOrNil returns nil for an empty / unset json.RawMessage so the SQL
// coalesce() branch fires the column default; otherwise it returns the
// bytes as a string so pgx binds them through the `::jsonb` cast.
func jsonbOrNil(m json.RawMessage) any {
	if len(m) == 0 {
		return nil
	}
	return string(m)
}

// stringOrNil returns nil for an empty string so the nullable column holds
// SQL NULL rather than an empty string.
func stringOrNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// intOrNil returns nil for a zero int so the nullable column holds SQL NULL
// rather than a zero integer. Zero is the wire-level sentinel meaning
// "unset / auto-recall disabled" for notebook_top_k; it round-trips to SQL
// NULL via this helper, matching the `coalesce(notebook_top_k, 0)` read
// convention.
func intOrNil(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// floatOrNil returns nil for a zero float64 so the nullable column holds SQL
// NULL rather than a zero value. Zero is the wire-level sentinel meaning
// "unset" for notebook_relevance_threshold; it round-trips to SQL NULL via
// this helper, matching the `coalesce(notebook_relevance_threshold, 0)` read
// convention.
func floatOrNil(f float64) any {
	if f == 0 {
		return nil
	}
	return f
}

// -----------------------------------------------------------------------
// POST /v1/watchkeepers — handleInsertWatchkeeper
// -----------------------------------------------------------------------

// insertWatchkeeperRequest is the JSON body accepted by POST /v1/watchkeepers.
// `status`, `spawned_at`, `retired_at` are intentionally absent: a fresh
// watchkeeper is always inserted with status='pending', and the timestamps
// are stamped server-side on the documented status transitions. Any of those
// fields in the body is rejected by DisallowUnknownFields.
type insertWatchkeeperRequest struct {
	ManifestID              string `json:"manifest_id"`
	LeadHumanID             string `json:"lead_human_id"`
	ActiveManifestVersionID string `json:"active_manifest_version_id,omitempty"`
}

// insertWatchkeeperResponse is the 201 body returned by POST /v1/watchkeepers.
type insertWatchkeeperResponse struct {
	ID string `json:"id"`
}

// parseInsertWatchkeeperRequest validates the Content-Type, caps the body
// size, decodes the JSON payload, and enforces UUID shape on the required
// fields. Mirrors the parseStoreRequest envelope so the 415 / 413 / 400
// surface stays uniform across write endpoints.
func parseInsertWatchkeeperRequest(w http.ResponseWriter, req *http.Request) (insertWatchkeeperRequest, bool) {
	var body insertWatchkeeperRequest

	if !isJSONContentType(req.Header.Get("Content-Type")) {
		writeErrorReason(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "expected_application_json")
		return body, false
	}

	req.Body = http.MaxBytesReader(w, req.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErrorReason(w, http.StatusRequestEntityTooLarge, "request_too_large", "body_too_large")
			return body, false
		}
		writeError(w, http.StatusBadRequest, "invalid_body")
		return body, false
	}
	if !uuidPattern.MatchString(body.ManifestID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return body, false
	}
	if !uuidPattern.MatchString(body.LeadHumanID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return body, false
	}
	if body.ActiveManifestVersionID != "" && !uuidPattern.MatchString(body.ActiveManifestVersionID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return body, false
	}
	return body, true
}

// handleInsertWatchkeeper serves POST /v1/watchkeepers. It validates the body,
// inserts one row into watchkeeper.watchkeeper under the scoped tx with
// status='pending' and NULL spawned_at/retired_at, and returns the new id.
// The status and timestamps are stamped server-side: clients cannot supply
// them via the request body (DisallowUnknownFields rejects those keys).
//
// Cross-tenant posture (M3.5.a.2 review fix): the body has no
// `organization_id` field, but `lead_human_id` is FK-validated against
// `watchkeeper.human(id)` and the FK alone does NOT enforce that the
// human belongs to the claim's tenant. Without an extra filter a caller
// for org A could anchor a watchkeeper at org B's human just by knowing
// the human UUID. The INSERT is wrapped in a `WHERE EXISTS` subquery on
// `watchkeeper.human` keyed by `(id, organization_id)` so a cross-tenant
// caller's INSERT … RETURNING produces no row → pgx.ErrNoRows → 404
// not_found, mirroring the 404 contract on handleSetWatchkeeperLead.
// `watchkeeper.watchkeeper` carries no `organization_id` column of its
// own (see migration 002); tenancy is inferred from the row's lead
// human. An empty `claim.OrganizationID` (legacy pre-M3.5.a.1 token) is
// rejected up front with 403 organization_required.
func handleInsertWatchkeeper(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		body, ok := parseInsertWatchkeeperRequest(w, req)
		if !ok {
			return
		}

		// M3.5.a.2 review fix: the claim must carry an explicit tenant.
		// Phase 1 rejects legacy claims rather than silently fall through
		// to an unfiltered INSERT that would let any authenticated caller
		// anchor a watchkeeper at any tenant's human.
		if claim.OrganizationID == "" {
			writeError(w, http.StatusForbidden, "organization_required")
			return
		}

		// active_manifest_version_id is nullable; pass SQL NULL when empty
		// so the FK-typed column holds NULL rather than an empty-string
		// value that would fail the uuid cast.
		activeManifestVersionID := stringOrNil(body.ActiveManifestVersionID)

		var id string
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			// JOIN-through-human org filter: the watchkeeper table has no
			// `organization_id` column of its own; tenancy is inferred
			// from the row's lead human. The INSERT … SELECT … WHERE
			// EXISTS shape lets Postgres reject the row in a single
			// statement when the lead human's org does not match the
			// claim, returning no row through RETURNING. The handler
			// surfaces that as 404 not_found.
			return tx.QueryRow(ctx, `
                INSERT INTO watchkeeper.watchkeeper (
                    manifest_id, lead_human_id, active_manifest_version_id,
                    status, spawned_at, retired_at
                )
                SELECT $1, $2, $3, 'pending', NULL, NULL
                WHERE EXISTS (
                    SELECT 1 FROM watchkeeper.human
                    WHERE id = $2 AND organization_id = $4
                )
                RETURNING id
            `, body.ManifestID, body.LeadHumanID, activeManifestVersionID, claim.OrganizationID).Scan(&id)
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "not_found")
				return
			}
			writeError(w, http.StatusInternalServerError, "insert_watchkeeper_failed")
			return
		}

		writeJSON(w, http.StatusCreated, insertWatchkeeperResponse{ID: id})
	})
}

// -----------------------------------------------------------------------
// PATCH /v1/watchkeepers/{id}/status — handleUpdateWatchkeeperStatus
// -----------------------------------------------------------------------

// updateWatchkeeperStatusRequest is the JSON body accepted by
// PATCH /v1/watchkeepers/{id}/status. `spawned_at` / `retired_at` are
// intentionally absent: those columns are stamped server-side on each
// documented transition. Any of those keys in the body is rejected by
// DisallowUnknownFields.
type updateWatchkeeperStatusRequest struct {
	Status string `json:"status"`
}

// parseUpdateWatchkeeperStatusRequest validates the envelope and the
// requested target status. It does NOT validate the transition rule: that
// requires reading the current row, which happens inside the scoped tx.
func parseUpdateWatchkeeperStatusRequest(w http.ResponseWriter, req *http.Request) (updateWatchkeeperStatusRequest, bool) {
	var body updateWatchkeeperStatusRequest

	if !isJSONContentType(req.Header.Get("Content-Type")) {
		writeErrorReason(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "expected_application_json")
		return body, false
	}

	req.Body = http.MaxBytesReader(w, req.Body, maxRequestBodyBytes)
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErrorReason(w, http.StatusRequestEntityTooLarge, "request_too_large", "body_too_large")
			return body, false
		}
		writeError(w, http.StatusBadRequest, "invalid_body")
		return body, false
	}
	if body.Status != "active" && body.Status != "retired" {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return body, false
	}
	return body, true
}

// handleUpdateWatchkeeperStatus serves PATCH /v1/watchkeepers/{id}/status.
// It enforces the watchkeeper lifecycle:
//
//	pending → active   (server stamps spawned_at = now())
//	active  → retired  (server stamps retired_at = now())
//
// Any other transition (e.g. retired→active, pending→retired) is rejected
// with 400 invalid_status_transition. An unknown id surfaces as 404
// not_found. The status check + UPDATE happen inside the same scoped tx so
// concurrent transitions are serialised by Postgres' row-level lock semantics
// (the SELECT … FOR UPDATE pattern); see migration 002 for the table CHECK
// constraint that backs this at the storage layer.
//
// Cross-tenant posture (M3.5.a.2 fix): the SELECT … FOR UPDATE filter
// matches BOTH on the watchkeeper id AND on the claim's tenant
// (resolved through the row's `lead_human_id → human.organization_id`
// relation; `watchkeeper.watchkeeper` carries no `organization_id`
// column of its own — see migration 002). A cross-tenant caller's
// SELECT returns no rows and the handler surfaces 404 not_found,
// hiding row existence from the wrong tenant. An empty
// `claim.OrganizationID` (legacy pre-M3.5.a.1 token) is rejected up
// front with 403 organization_required.
func handleUpdateWatchkeeperStatus(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		id := req.PathValue("id")
		if id == "" {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if !uuidPattern.MatchString(id) {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}

		body, ok := parseUpdateWatchkeeperStatusRequest(w, req)
		if !ok {
			return
		}

		// M3.5.a.2: legacy claims (no org) cannot pin tenancy and are
		// rejected. Mirrors handleInsertHuman / handleSetWatchkeeperLead.
		if claim.OrganizationID == "" {
			writeError(w, http.StatusForbidden, "organization_required")
			return
		}

		// transitionResult lets the closure signal the handler that the row
		// existed but the requested transition is not allowed, so the
		// outer code can map that to a 400 invalid_status_transition
		// without conflating it with a generic DB error.
		type transitionResult struct {
			notFound          bool
			invalidTransition bool
		}
		var res transitionResult

		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			// Lock the row so a concurrent PATCH cannot race the
			// transition validation. SELECT … FOR UPDATE OF w blocks any
			// other tx that targets the same row until ours commits.
			//
			// JOIN-through-human org filter: a cross-tenant caller's
			// SELECT returns zero rows and the handler emits 404. The
			// `FOR UPDATE OF w` form locks ONLY the watchkeeper row;
			// the human side is read-only and locking it would
			// over-serialize unrelated traffic.
			//
			// DO NOT drop the `OF w` qualifier in a future refactor.
			// A bare `FOR UPDATE` would also lock the joined `human`
			// row, which (a) widens the lock scope to include rows
			// that this handler does not mutate and (b) introduces a
			// lock-ordering hazard with other handlers that read
			// `watchkeeper.human` (e.g. `handleSetWatchkeeperLead`'s
			// `lead_human_id IN (SELECT … FROM watchkeeper.human …)`
			// subquery), opening a deadlock window when two
			// transactions touch the same human + watchkeeper pair in
			// opposite orders. Keep the qualifier explicit; the
			// human-row visibility comes from the JOIN's read snapshot,
			// not from a row lock.
			var current string
			row := tx.QueryRow(ctx, `
                SELECT w.status
                FROM watchkeeper.watchkeeper AS w
                JOIN watchkeeper.human AS h ON h.id = w.lead_human_id
                WHERE w.id = $1 AND h.organization_id = $2
                FOR UPDATE OF w
            `, id, claim.OrganizationID)
			if err := row.Scan(&current); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					res.notFound = true
					return nil
				}
				return err
			}

			// Allowed transitions only:
			//   pending → active
			//   active  → retired
			// Everything else is rejected without touching the row.
			switch {
			case current == "pending" && body.Status == "active":
				_, err := tx.Exec(ctx, `
                    UPDATE watchkeeper.watchkeeper
                    SET status = 'active', spawned_at = now()
                    WHERE id = $1
                `, id)
				return err
			case current == "active" && body.Status == "retired":
				_, err := tx.Exec(ctx, `
                    UPDATE watchkeeper.watchkeeper
                    SET status = 'retired', retired_at = now()
                    WHERE id = $1
                `, id)
				return err
			default:
				res.invalidTransition = true
				return nil
			}
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "update_watchkeeper_status_failed")
			return
		}
		if res.notFound {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}
		if res.invalidTransition {
			writeError(w, http.StatusBadRequest, "invalid_status_transition")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})
}
