package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
const (
	maxSearchTopK   = 50
	defaultLogLimit = 50
	maxLogLimit     = 200
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

		var body searchRequest
		dec := json.NewDecoder(req.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "invalid_body")
			return
		}
		if len(body.Embedding) == 0 {
			writeError(w, http.StatusBadRequest, "missing_embedding")
			return
		}
		if body.TopK <= 0 {
			writeError(w, http.StatusBadRequest, "invalid_top_k")
			return
		}
		if body.TopK > maxSearchTopK {
			body.TopK = maxSearchTopK
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
	ID               string          `json:"id"`
	ManifestID       string          `json:"manifest_id"`
	VersionNo        int             `json:"version_no"`
	SystemPrompt     string          `json:"system_prompt"`
	Tools            json.RawMessage `json:"tools"`
	AuthorityMatrix  json.RawMessage `json:"authority_matrix"`
	KnowledgeSources json.RawMessage `json:"knowledge_sources"`
	Personality      string          `json:"personality,omitempty"`
	Language         string          `json:"language,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
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
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
                SELECT id, manifest_id, version_no, system_prompt,
                       tools, authority_matrix, knowledge_sources,
                       coalesce(personality, ''), coalesce(language, ''),
                       created_at
                FROM watchkeeper.manifest_version
                WHERE manifest_id = $1
                ORDER BY version_no DESC
                LIMIT 1
            `, manifestID).Scan(
				&out.ID, &out.ManifestID, &out.VersionNo, &out.SystemPrompt,
				&out.Tools, &out.AuthorityMatrix, &out.KnowledgeSources,
				&out.Personality, &out.Language,
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
// semantic codes.
func writeError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":"`+code+`"}`)
}
