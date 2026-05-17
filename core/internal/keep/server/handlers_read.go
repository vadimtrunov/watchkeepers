package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/db"
)

// scopedRunner is the narrow interface the read handlers need from the
// db package. Passing it as an injectable seam (rather than calling
// db.WithScope directly) keeps unit tests honest: tests supply a
// runner that invokes fn against a fake pgx.Tx without opening a real
// pool.
type scopedRunner interface {
	WithScope(ctx context.Context, claim auth.Claim, fn func(context.Context, pgx.Tx) error) error
}

// poolRunner adapts *pgxpool.Pool to scopedRunner by delegating to
// db.WithScope. This is the production wiring path.
type poolRunner struct {
	pool *pgxpool.Pool
}

// WithScope implements scopedRunner for the production pool.
func (p poolRunner) WithScope(ctx context.Context, claim auth.Claim, fn func(context.Context, pgx.Tx) error) error {
	return db.WithScope(ctx, p.pool, claim, fn)
}

// Search and log-tail request limits. AC4 clamps search top_k to [1, 50]
// and log-tail limit to [1, 200]; zero/negative values are rejected with
// 400 rather than silently defaulting so clients learn about the contract
// before traffic goes live.
//
// maxSearchBodyBytes caps the raw POST /v1/search body to 1 MiB so a single
// authenticated client cannot force unbounded allocation by streaming a
// multi-GB JSON body. maxEmbeddingDim mirrors the largest reasonable model
// output dimension (OpenAI text-embedding-3-large is 3072); 4096 leaves
// headroom without exposing a DoS surface.
const (
	maxSearchTopK      = 50
	defaultLogLimit    = 50
	maxLogLimit        = 200
	maxSearchBodyBytes = 1 << 20
	maxEmbeddingDim    = 4096
)

// searchRequest is the JSON body accepted by POST /v1/search. The
// field names are explicit (no omitempty) so the validator fires on
// zero-valued ints rather than silently clamping them.
type searchRequest struct {
	Embedding []float32 `json:"embedding"`
	TopK      int       `json:"top_k"`
}

// searchResult mirrors a single row from knowledge_chunk plus the
// cosine distance returned by the pgvector `<=>` operator. Field
// names match the column names for consistency with the rest of the
// read responses.
type searchResult struct {
	ID        string    `json:"id"`
	Subject   string    `json:"subject,omitempty"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	Distance  float64   `json:"distance"`
}

type searchResponse struct {
	Results []searchResult `json:"results"`
}

// parseSearchRequest validates the Content-Type, caps the body size,
// decodes the JSON payload, and enforces the embedding/top_k bounds.
// On any failure it writes the canonical error envelope to w and
// returns ok=false so the caller must abort. Extracted from
// handleSearch to keep that handler under the gocyclo budget.
func parseSearchRequest(w http.ResponseWriter, req *http.Request) (searchRequest, bool) {
	var body searchRequest

	// Enforce application/json Content-Type (charset parameter allowed).
	// Missing or mismatched types are rejected up-front so we don't
	// allocate a JSON decoder for a non-JSON body.
	if !isJSONContentType(req.Header.Get("Content-Type")) {
		writeErrorReason(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "expected_application_json")
		return body, false
	}

	// Cap the body to maxSearchBodyBytes so a single client cannot force
	// unbounded allocation. This also bounds the read that
	// DisallowUnknownFields would otherwise have to perform in full.
	req.Body = http.MaxBytesReader(w, req.Body, maxSearchBodyBytes)

	dec := json.NewDecoder(req.Body)
	// Body size is already capped by MaxBytesReader above, so the full
	// read DisallowUnknownFields forces stays bounded.
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
	if len(body.Embedding) == 0 {
		writeError(w, http.StatusBadRequest, "missing_embedding")
		return body, false
	}
	if len(body.Embedding) > maxEmbeddingDim {
		writeError(w, http.StatusBadRequest, "invalid_embedding")
		return body, false
	}
	if body.TopK <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_top_k")
		return body, false
	}
	if body.TopK > maxSearchTopK {
		body.TopK = maxSearchTopK
	}
	return body, true
}

// handleSearch serves POST /v1/search. It validates the body, runs the
// pgvector cosine-distance KNN under the scoped tx, and returns the
// result rows ordered by ascending distance (closest first).
func handleSearch(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			// Defense-in-depth: middleware should have rejected this.
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		body, ok := parseSearchRequest(w, req)
		if !ok {
			return
		}

		vec := embeddingToVector(body.Embedding)

		out := make([]searchResult, 0, body.TopK)
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
                SELECT id, coalesce(subject, ''), content, created_at,
                       embedding <=> $1::vector AS distance
                FROM watchkeeper.knowledge_chunk
                ORDER BY embedding <=> $1::vector
                LIMIT $2
            `, vec, body.TopK)
			if err != nil {
				return fmt.Errorf("search query: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var rec searchResult
				if err := rows.Scan(&rec.ID, &rec.Subject, &rec.Content, &rec.CreatedAt, &rec.Distance); err != nil {
					return fmt.Errorf("search scan: %w", err)
				}
				// pgvector cosine distance between two zero vectors is
				// undefined (0/0) and comes back as NaN; the Go JSON
				// encoder rejects NaN mid-stream, truncating the response
				// after status + headers have flushed. Snap NaN/±Inf to
				// the max cosine distance (2.0 for vector_cosine_ops) so
				// the client gets a serialisable number and these rows
				// sort last.
				if math.IsNaN(rec.Distance) || math.IsInf(rec.Distance, 0) {
					rec.Distance = 2.0
				}
				out = append(out, rec)
			}
			return rows.Err()
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "search_failed")
			return
		}

		writeJSON(w, http.StatusOK, searchResponse{Results: out})
	})
}

// manifestVersionResponse is the JSON shape of a single manifest
// version row returned by GET /v1/manifests/{manifest_id}. Field names
// mirror the database columns verbatim.
type manifestVersionResponse struct {
	ID                         string          `json:"id"`
	ManifestID                 string          `json:"manifest_id"`
	VersionNo                  int             `json:"version_no"`
	SystemPrompt               string          `json:"system_prompt"`
	Tools                      json.RawMessage `json:"tools"`
	AuthorityMatrix            json.RawMessage `json:"authority_matrix"`
	KnowledgeSources           json.RawMessage `json:"knowledge_sources"`
	Personality                string          `json:"personality,omitempty"`
	Language                   string          `json:"language,omitempty"`
	Model                      string          `json:"model,omitempty"`
	Autonomy                   string          `json:"autonomy,omitempty"`
	NotebookTopK               int             `json:"notebook_top_k,omitempty"`
	NotebookRelevanceThreshold float64         `json:"notebook_relevance_threshold,omitempty"`
	// ImmutableCore is the optional manifest immutable_core jsonb column
	// per Phase 2 §M3.1. A NULL column in the DB scans as a zero-length
	// [json.RawMessage] (see the read handler below — pgx skips Scan
	// writes on SQL NULL when the destination is a pointer-typed slice);
	// `omitempty` then drops the key from the wire response so legacy
	// callers that never set it observe no schema change. When non-NULL
	// the server CHECK constraint `manifest_version_immutable_core_shape`
	// (migration 030) guarantees the payload is a JSON object.
	ImmutableCore json.RawMessage `json:"immutable_core,omitempty"`
	// Reason is the optional free-text rationale the proposer attached to
	// this manifest_version (Phase 2 §M3.3). SQL NULL coalesces to the
	// empty string on the SELECT (`coalesce(reason, '')`); `omitempty`
	// then drops the key from the wire response so legacy callers that
	// never set it observe no schema change.
	Reason string `json:"reason,omitempty"`
	// PreviousVersionID is the optional UUID of the manifest_version this
	// row is derived from (Phase 2 §M3.3). NULL in the DB scans into a
	// nil *string via pgx; the read handler promotes the pointer to the
	// response field only when non-nil so `omitempty` can drop the key
	// from the wire. Pointer-typed so the SQL NULL case is distinguishable
	// from the empty-string case (mirrors the immutable_core
	// pointer-to-RawMessage scan precedent).
	PreviousVersionID *string `json:"previous_version_id,omitempty"`
	// Proposer is the optional free-text identifier of the actor that
	// proposed this version (Phase 2 §M3.3). SQL NULL coalesces to the
	// empty string on the SELECT (`coalesce(proposer, '')`); `omitempty`
	// then drops the key from the response.
	Proposer  string    `json:"proposer,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// handleGetManifest serves GET /v1/manifests/{manifest_id}. It returns
// the manifest_version row with the highest version_no for the given
// manifest_id. A missing row produces a 404 JSON envelope so clients
// can distinguish "no manifest" from transport errors.
func handleGetManifest(r scopedRunner) http.Handler {
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

		var out manifestVersionResponse
		// immutableCore is scanned as a pointer-to-RawMessage so the SQL
		// NULL case (no immutable_core declared yet) projects as a Go nil
		// rather than the JSON `null` literal — `omitempty` on the
		// response field then drops the key entirely from the wire so
		// legacy GET callers observe no schema change. Mirrors the
		// nullable-jsonb pattern recommended by pgx; pgx writes through
		// the pointer only on a non-NULL row.
		var immutableCore *json.RawMessage
		// M3.3 previous_version_id is scanned as a *string so SQL NULL
		// projects to a Go nil rather than the empty string. The
		// handler promotes the pointer onto the response struct only
		// when non-nil; `omitempty` then drops the key from the wire so
		// legacy GET callers observe no schema change. Mirrors the
		// immutable_core pointer-to-RawMessage scan pattern.
		var previousVersionID *string
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
                SELECT id, manifest_id, version_no, system_prompt,
                       tools, authority_matrix, knowledge_sources,
                       coalesce(personality, ''), coalesce(language, ''),
                       coalesce(model, ''),
                       coalesce(autonomy, ''),
                       coalesce(notebook_top_k, 0),
                       coalesce(notebook_relevance_threshold, 0),
                       immutable_core,
                       coalesce(reason, ''),
                       previous_version_id,
                       coalesce(proposer, ''),
                       created_at
                FROM watchkeeper.manifest_version
                WHERE manifest_id = $1
                ORDER BY version_no DESC
                LIMIT 1
            `, manifestID).Scan(
				&out.ID, &out.ManifestID, &out.VersionNo, &out.SystemPrompt,
				&out.Tools, &out.AuthorityMatrix, &out.KnowledgeSources,
				&out.Personality, &out.Language,
				&out.Model,
				&out.Autonomy,
				&out.NotebookTopK,
				&out.NotebookRelevanceThreshold,
				&immutableCore,
				&out.Reason,
				&previousVersionID,
				&out.Proposer,
				&out.CreatedAt,
			)
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "not_found")
				return
			}
			writeError(w, http.StatusInternalServerError, "get_manifest_failed")
			return
		}
		// Promote the pointer-to-RawMessage scan target onto the
		// response struct only when the column was non-NULL; on SQL
		// NULL the pointer stays nil and `omitempty` drops the field
		// from the wire (preserving legacy GET shape for callers that
		// have not set immutable_core yet).
		if immutableCore != nil {
			out.ImmutableCore = *immutableCore
		}
		// Promote the *string scan target onto the response struct only
		// when the column was non-NULL; on SQL NULL the pointer stays
		// nil and `omitempty` drops `previous_version_id` from the wire
		// (root version of the manifest). Mirrors the immutable_core
		// promotion above.
		if previousVersionID != nil {
			out.PreviousVersionID = previousVersionID
		}

		writeJSON(w, http.StatusOK, out)
	})
}

// keepersLogEvent mirrors a keepers_log row. Null-capable columns
// (correlation_id, actor_*) use string pointers + omitempty so the
// on-wire shape is clean; payload stays as json.RawMessage because it
// is already valid JSON coming out of Postgres.
type keepersLogEvent struct {
	ID                 string          `json:"id"`
	EventType          string          `json:"event_type"`
	CorrelationID      *string         `json:"correlation_id,omitempty"`
	ActorWatchkeeperID *string         `json:"actor_watchkeeper_id,omitempty"`
	ActorHumanID       *string         `json:"actor_human_id,omitempty"`
	Payload            json.RawMessage `json:"payload"`
	CreatedAt          time.Time       `json:"created_at"`
}

type keepersLogResponse struct {
	Events []keepersLogEvent `json:"events"`
}

// handleLogTail serves GET /v1/keepers-log. It supports ?limit=<n>
// (default 50, cap 200, reject 0 or negative) and returns rows in
// strict created_at DESC order.
func handleLogTail(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		limit := defaultLogLimit
		if raw := req.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 {
				writeError(w, http.StatusBadRequest, "invalid_limit")
				return
			}
			if n > maxLogLimit {
				n = maxLogLimit
			}
			limit = n
		}

		out := make([]keepersLogEvent, 0, limit)
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
                SELECT id, event_type, correlation_id,
                       actor_watchkeeper_id, actor_human_id,
                       payload, created_at
                FROM watchkeeper.keepers_log
                ORDER BY created_at DESC
                LIMIT $1
            `, limit)
			if err != nil {
				return fmt.Errorf("log_tail query: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var rec keepersLogEvent
				if err := rows.Scan(
					&rec.ID, &rec.EventType, &rec.CorrelationID,
					&rec.ActorWatchkeeperID, &rec.ActorHumanID,
					&rec.Payload, &rec.CreatedAt,
				); err != nil {
					return fmt.Errorf("log_tail scan: %w", err)
				}
				out = append(out, rec)
			}
			return rows.Err()
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "log_tail_failed")
			return
		}

		writeJSON(w, http.StatusOK, keepersLogResponse{Events: out})
	})
}

// embeddingToVector converts a Go []float32 into the `[x,y,...]` string
// literal pgvector accepts for the `::vector` cast. Using a string is
// the widely-documented pgvector wire format and keeps this package
// free of the pgvector/pgvector-go driver dependency (M2.7 Phase 1 does
// not ship a real embedding provider; the client passes the vector
// directly per the TASK scope).
func embeddingToVector(emb []float32) string {
	// Fast path for the common case; strconv formatting keeps numeric
	// output portable and independent of the caller's locale.
	buf := make([]byte, 0, len(emb)*8+2)
	buf = append(buf, '[')
	for i, f := range emb {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = strconv.AppendFloat(buf, float64(f), 'f', -1, 32)
	}
	buf = append(buf, ']')
	return string(buf)
}

// writeJSON marshals body and writes it as application/json. Encoder
// errors mid-stream cannot recover the status code, so they're dropped
// intentionally (alternative would be a second response that the
// client cannot read after headers are flushed).
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError emits a {"error":"<code>"} envelope. It shares the shape
// used by writeAuthError but lets read-path handlers surface their own
// semantic codes. Callers must pass stable string literals as code — no
// JSON escaping is performed on the value.
func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"`+code+`"}`)
}

// writeErrorReason emits a {"error":"<code>","reason":"<reason>"}
// envelope for the richer error shape used by the input-validation
// rejections (oversized body, wrong Content-Type). Callers must pass
// stable string literals for code and reason.
func writeErrorReason(w http.ResponseWriter, status int, code, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"`+code+`","reason":"`+reason+`"}`)
}

// isJSONContentType reports whether the given Content-Type header value
// is application/json (charset parameter allowed). A missing or
// malformed header is treated as a mismatch.
func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mediaType == "application/json"
}

// -----------------------------------------------------------------------
// GET /v1/watchkeepers/{id} — handleGetWatchkeeper
// GET /v1/watchkeepers       — handleListWatchkeepers
// -----------------------------------------------------------------------

// Watchkeeper list pagination bounds. The default limit matches log_tail
// (50); the cap matches log_tail (200) so any single authenticated caller
// cannot request megabyte-scale list responses. The cursor field on the
// response envelope is reserved for a future seek-pagination follow-up.
const (
	defaultWatchkeeperListLimit = 50
	maxWatchkeeperListLimit     = 200
)

// watchkeeperRow mirrors the JSON shape of one watchkeeper.watchkeeper row.
// Nullable timestamps and the nullable foreign keys use *time.Time / *string
// so the wire shape carries `null` rather than the Go zero value when the
// column was actually NULL in Postgres.
type watchkeeperRow struct {
	ID                      string     `json:"id"`
	ManifestID              string     `json:"manifest_id"`
	LeadHumanID             string     `json:"lead_human_id"`
	ActiveManifestVersionID *string    `json:"active_manifest_version_id"`
	Status                  string     `json:"status"`
	SpawnedAt               *time.Time `json:"spawned_at"`
	RetiredAt               *time.Time `json:"retired_at"`
	// ArchiveURI is the M7.2.c archive storage URI recorded on the
	// active→retired transition; nil when the column was NULL in Postgres
	// (row was retired before M7.2.c shipped, or via the M6.2.c
	// synchronous tool that does not carry an archive_uri).
	ArchiveURI *string `json:"archive_uri"`
	// RoleID is the M7.1.a opaque role-identity string used by the M7.1
	// inheritance saga family. Nil when the column was NULL in Postgres
	// (every row predating the M7.1.a migration + every legacy insert
	// that omits the optional `role_id` body field). The server-side
	// column is `role_id text NULL` — migration
	// `032_watchkeepers_role_id.sql`. The M7.1.b predecessor-lookup
	// endpoint + M7.1.c NotebookInheritStep saga step land in
	// subsequent leaves and read this column via the migration's
	// partial index `idx_watchkeeper_role_id_retired`.
	RoleID    *string   `json:"role_id"`
	CreatedAt time.Time `json:"created_at"`
}

// listWatchkeepersResponse is the envelope returned by GET /v1/watchkeepers.
// `next_cursor` is reserved for a future seek-pagination follow-up; M3.2.a
// always returns `null` so the wire shape is forward-compatible.
type listWatchkeepersResponse struct {
	Items      []watchkeeperRow `json:"items"`
	NextCursor *string          `json:"next_cursor"`
}

// handleGetWatchkeeper serves GET /v1/watchkeepers/{id}. It returns the full
// row JSON; an unknown id surfaces as 404 not_found.
//
// Documented limitation: row-level security on `watchkeeper.watchkeeper` is
// not enabled at this milestone (see migration 011), so any authenticated
// caller can fetch any row. A future migration adds an RLS policy keyed off
// the same `app.scope` GUC the existing knowledge_chunk policy uses.
func handleGetWatchkeeper(r scopedRunner) http.Handler {
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

		var out watchkeeperRow
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
                SELECT id, manifest_id, lead_human_id,
                       active_manifest_version_id, status,
                       spawned_at, retired_at, archive_uri, role_id, created_at
                FROM watchkeeper.watchkeeper
                WHERE id = $1
            `, id).Scan(
				&out.ID, &out.ManifestID, &out.LeadHumanID,
				&out.ActiveManifestVersionID, &out.Status,
				&out.SpawnedAt, &out.RetiredAt, &out.ArchiveURI, &out.RoleID, &out.CreatedAt,
			)
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "not_found")
				return
			}
			writeError(w, http.StatusInternalServerError, "get_watchkeeper_failed")
			return
		}

		writeJSON(w, http.StatusOK, out)
	})
}

// handleListWatchkeepers serves GET /v1/watchkeepers. It supports
// `?status=pending|active|retired` filtering and `?limit=<n>` (default 50,
// max 200, reject 0/negative/oversize). Rows are returned in
// `created_at DESC` order. The response envelope's `next_cursor` is reserved
// for a future seek-pagination follow-up; this milestone always returns null.
//
// Documented limitation: row-level security on `watchkeeper.watchkeeper` is
// not enabled at this milestone (see migration 011); future TASK adds an
// RLS policy.
func handleListWatchkeepers(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		statusFilter := req.URL.Query().Get("status")
		switch statusFilter {
		case "", "pending", "active", "retired":
			// allowed
		default:
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}

		limit := defaultWatchkeeperListLimit
		if raw := req.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n <= 0 || n > maxWatchkeeperListLimit {
				writeError(w, http.StatusBadRequest, "invalid_request")
				return
			}
			limit = n
		}

		out := make([]watchkeeperRow, 0, limit)
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			// Two SQL variants instead of a single conditional WHERE clause
			// keep the pgx parameter binding straightforward — pgx does not
			// support an "ignore-when-NULL" filter pattern without extra
			// CASE plumbing, and a duplicated short query is cheaper to
			// read than a conditional one.
			var (
				rows pgx.Rows
				err  error
			)
			if statusFilter == "" {
				rows, err = tx.Query(ctx, `
                    SELECT id, manifest_id, lead_human_id,
                           active_manifest_version_id, status,
                           spawned_at, retired_at, archive_uri, role_id, created_at
                    FROM watchkeeper.watchkeeper
                    ORDER BY created_at DESC
                    LIMIT $1
                `, limit)
			} else {
				rows, err = tx.Query(ctx, `
                    SELECT id, manifest_id, lead_human_id,
                           active_manifest_version_id, status,
                           spawned_at, retired_at, archive_uri, role_id, created_at
                    FROM watchkeeper.watchkeeper
                    WHERE status = $1
                    ORDER BY created_at DESC
                    LIMIT $2
                `, statusFilter, limit)
			}
			if err != nil {
				return fmt.Errorf("list_watchkeepers query: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var rec watchkeeperRow
				if err := rows.Scan(
					&rec.ID, &rec.ManifestID, &rec.LeadHumanID,
					&rec.ActiveManifestVersionID, &rec.Status,
					&rec.SpawnedAt, &rec.RetiredAt, &rec.ArchiveURI, &rec.RoleID, &rec.CreatedAt,
				); err != nil {
					return fmt.Errorf("list_watchkeepers scan: %w", err)
				}
				out = append(out, rec)
			}
			return rows.Err()
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list_watchkeepers_failed")
			return
		}

		writeJSON(w, http.StatusOK, listWatchkeepersResponse{Items: out, NextCursor: nil})
	})
}

// -----------------------------------------------------------------------
// GET /v1/watchkeepers/latest-retired-by-role — handleGetLatestRetiredByRole
// -----------------------------------------------------------------------

// handleGetLatestRetiredByRole serves
//
//	GET /v1/watchkeepers/latest-retired-by-role?role_id=<role>
//
// returning the freshest `retired_at IS NOT NULL AND archive_uri IS NOT NULL`
// watchkeeper row whose `role_id` matches the query parameter, scoped to
// the caller's tenant. Backs the M7.1 inheritance saga
// (`NotebookInheritStep`, M7.1.c): when a fresh watchkeeper spawns
// carrying a `role_id`, the saga looks up its most recent retired peer
// and seeds the new notebook from the predecessor's archive.
//
// Cross-tenant posture: the watchkeeper.watchkeeper table carries no
// `organization_id` column of its own (see migration 002 — tenancy
// flows through `lead_human_id → human.organization_id`). The SELECT
// JOINs `watchkeeper.human` and filters on `h.organization_id =
// $claim_org`, matching the M3.5.a.2 JOIN-on-human pattern established
// by `handleUpdateWatchkeeperStatus`. A cross-tenant role match
// produces zero rows → 404 not_found (NOT 403): the predecessor row
// EXISTS in the database but is INVISIBLE to the caller's tenant, so
// the indistinguishable-from-absent surface is the deliberate
// choice — leaking 403-vs-404 here would reveal "a role with this id
// exists in some OTHER org". Mirrors the contract on
// `handleGetWatchkeeper`'s cross-tenant case after RLS hardening lands.
//
// Legacy claims (empty `claim.OrganizationID` — the pre-M3.5.a.1 wire
// shape) are rejected up front with 403 organization_required before
// `WithScope` opens any transaction. The 403-vs-404 split is the
// reverse of the cross-tenant case: a legacy token signals a
// misconfigured caller (rolling-deploy compat), not an attacker
// probing for row existence, so the deterministic 403 is more useful
// than a misleading 404.
//
// Index usage: the SELECT's WHERE clause matches the partial composite
// index `idx_watchkeeper_role_id_retired` introduced by migration
// `032_watchkeepers_role_id.sql` (M7.1.a, after the iter-1 follow-up
// in #164): `(role_id, retired_at DESC) WHERE retired_at IS NOT NULL
// AND archive_uri IS NOT NULL AND role_id IS NOT NULL`. The query
// repeats the `role_id IS NOT NULL` predicate explicitly (in
// addition to the `role_id = $1` equality) so the planner picks the
// partial index without relying on its implication-detection logic
// (`a = $1` ⇒ `a IS NOT NULL`) — explicit predicate alignment with
// `predicate_implied_by` keeps the index usable across PG versions
// and prepared-statement plan caching. The ORDER BY matches the
// index's DESC sort so the planner can satisfy the LIMIT 1 with an
// index-only scan against the retired-with-archive-and-non-null-
// role subset.
//
// Audit / PII discipline: this handler is read-only and emits no
// `keeperslog.` events. The M1.4 audit subscriber and the M7.1.c
// `notebook_inherited` event own the inheritance observation surface.
// The handler echoes the request's role_id back to the caller via the
// row's `RoleID` field only — no claim subject, no internal tenant id,
// no token fragments cross the response envelope.
func handleGetLatestRetiredByRole(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		// M3.5.a.2: legacy claims (no org) cannot pin tenancy and are
		// rejected. Mirrors handleUpdateWatchkeeperStatus /
		// handleSetWatchkeeperLead / handleInsertWatchkeeper. Fires
		// before WithScope so a malicious legacy token never opens a
		// tx.
		if claim.OrganizationID == "" {
			writeError(w, http.StatusForbidden, "organization_required")
			return
		}

		// Reject both empty and whitespace-only role_id at the seam
		// closest to the caller. Whitespace-only would otherwise hit
		// the SQL filter `w.role_id = $1` with a value no row carries,
		// return 404 → keepclient.ErrNoPredecessor on the client side,
		// and silently disable inheritance on the M7.1.c saga step.
		// Mirrors the same `strings.TrimSpace` gate in
		// parseInsertWatchkeeperRequest. iter-1 codex finding (P2 /
		// Major).
		roleID := req.URL.Query().Get("role_id")
		if roleID == "" || strings.TrimSpace(roleID) == "" {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}

		var out watchkeeperRow
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			// JOIN-through-human org filter (M3.5.a.2 pattern): the
			// `watchkeeper.watchkeeper` table has no
			// `organization_id` column; tenancy flows through
			// `lead_human_id → human.organization_id`. A
			// cross-tenant caller's SELECT returns zero rows and the
			// handler emits 404 not_found (the predecessor row
			// EXISTS but is invisible to the caller's tenant).
			//
			// Index plan: the WHERE filters on `role_id`, the
			// `retired_at IS NOT NULL` predicate, AND
			// `archive_uri IS NOT NULL` matches the partial index
			// `idx_watchkeeper_role_id_retired` introduced by
			// migration 032. The ORDER BY retired_at DESC matches
			// the index's sort key so the planner can satisfy
			// LIMIT 1 with an index-only seek on the retired-with-
			// archive subset (orders-of-magnitude smaller than the
			// full table on a healthy deployment).
			//
			// Field projection mirrors handleGetWatchkeeper's Scan
			// list — the wire shape of `Watchkeeper` on the
			// keepclient side is the single source of truth for
			// what an inheritance lookup needs (lead_human_id +
			// active_manifest_version_id are surfaced for the
			// M7.1.c saga step's downstream consumers).
			return tx.QueryRow(ctx, `
                SELECT w.id, w.manifest_id, w.lead_human_id,
                       w.active_manifest_version_id, w.status,
                       w.spawned_at, w.retired_at, w.archive_uri,
                       w.role_id, w.created_at
                FROM watchkeeper.watchkeeper AS w
                JOIN watchkeeper.human AS h ON h.id = w.lead_human_id
                WHERE w.role_id = $1
                  AND w.role_id IS NOT NULL
                  AND h.organization_id = $2
                  AND w.retired_at IS NOT NULL
                  AND w.archive_uri IS NOT NULL
                ORDER BY w.retired_at DESC
                LIMIT 1
            `, roleID, claim.OrganizationID).Scan(
				&out.ID, &out.ManifestID, &out.LeadHumanID,
				&out.ActiveManifestVersionID, &out.Status,
				&out.SpawnedAt, &out.RetiredAt, &out.ArchiveURI,
				&out.RoleID, &out.CreatedAt,
			)
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "not_found")
				return
			}
			writeError(w, http.StatusInternalServerError, "latest_retired_by_role_failed")
			return
		}

		writeJSON(w, http.StatusOK, out)
	})
}
