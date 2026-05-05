package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// -----------------------------------------------------------------------
// POST /v1/humans            — handleInsertHuman
// GET  /v1/humans/by-slack/{slack_user_id}
//                            — handleLookupHumanBySlackID
// PATCH /v1/watchkeepers/{id}/lead
//                            — handleSetWatchkeeperLead
// -----------------------------------------------------------------------
//
// M4.4 Human identity mapping. Two read/write surfaces back the messenger
// adapter's "Slack user ID → Keep human row" lookup contract:
//
//   1. POST /v1/humans inserts a fresh human row, optionally seeded with a
//      Slack user ID. The unique constraint on `slack_user_id` (migration
//      012) is the integrity backstop; the handler translates a 23505
//      violation to 409 slack_user_id_conflict so callers can decide
//      whether to retry as a lookup.
//   2. GET /v1/humans/by-slack/{slack_user_id} returns the row keyed by
//      `slack_user_id`. A missing row yields 404 not_found, matching the
//      get_watchkeeper contract.
//
// PATCH /v1/watchkeepers/{id}/lead exposes the lead → Watchkeeper
// relation explicitly so an operator (or the upcoming bot-binding flow)
// can rebind a Watchkeeper to a different human without rewriting the
// row through the insert endpoint.

// slackUserIDMaxBytes caps the slack_user_id input. Slack user IDs are
// typically 11 characters (`U` + 10 alphanumerics), but workspace IDs and
// future ID-shape changes mean the real upper bound is best treated as
// "small but not 1 byte". 64 bytes leaves room for any future Slack ID
// shape while keeping the lookup string short enough to be safe to log
// (no PII implications — Slack user IDs are public within a workspace).
const slackUserIDMaxBytes = 64

// humanRow mirrors the JSON shape of one watchkeeper.human row. Nullable
// columns use *string so the wire shape carries `null` rather than the Go
// zero value when the column was actually NULL in Postgres.
type humanRow struct {
	ID             string    `json:"id"`
	OrganizationID string    `json:"organization_id"`
	DisplayName    string    `json:"display_name"`
	Email          *string   `json:"email"`
	SlackUserID    *string   `json:"slack_user_id"`
	CreatedAt      time.Time `json:"created_at"`
}

// insertHumanRequest is the JSON body accepted by POST /v1/humans. Required
// fields are `organization_id` and `display_name`; `email` and
// `slack_user_id` are optional and round-trip as SQL NULL when omitted.
// `id` and `created_at` are intentionally absent: both are stamped
// server-side. DisallowUnknownFields rejects any other key.
type insertHumanRequest struct {
	OrganizationID string `json:"organization_id"`
	DisplayName    string `json:"display_name"`
	Email          string `json:"email,omitempty"`
	SlackUserID    string `json:"slack_user_id,omitempty"`
}

// insertHumanResponse is the 201 body returned by POST /v1/humans.
type insertHumanResponse struct {
	ID string `json:"id"`
}

// parseInsertHumanRequest validates the Content-Type, caps the body size,
// decodes the JSON payload, and enforces UUID shape on organization_id plus
// the slackUserIDMaxBytes ceiling on slack_user_id. Mirrors the
// parseInsertWatchkeeperRequest envelope so the 415 / 413 / 400 surface
// stays uniform across write endpoints.
func parseInsertHumanRequest(w http.ResponseWriter, req *http.Request) (insertHumanRequest, bool) {
	var body insertHumanRequest

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
	if !uuidPattern.MatchString(body.OrganizationID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return body, false
	}
	if body.DisplayName == "" {
		writeError(w, http.StatusBadRequest, "missing_display_name")
		return body, false
	}
	if len(body.SlackUserID) > slackUserIDMaxBytes {
		writeError(w, http.StatusBadRequest, "invalid_slack_user_id")
		return body, false
	}
	return body, true
}

// handleInsertHuman serves POST /v1/humans. It validates the body, inserts
// one row into watchkeeper.human under the scoped tx, and returns the new
// id. A unique-violation on `human_slack_user_id_key` is translated to
// 409 slack_user_id_conflict so the caller can decide whether to fall back
// to a lookup. RLS on the human table is intentionally not enabled at this
// milestone (matching the watchkeeper-table policy from migration 011);
// future TASK adds a policy keyed off the same `app.scope` GUC.
func handleInsertHuman(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		body, ok := parseInsertHumanRequest(w, req)
		if !ok {
			return
		}

		// email and slack_user_id are nullable; pass SQL NULL when empty so
		// the row carries NULL rather than an empty string that would defeat
		// the unique constraint's NULLs-are-distinct semantics.
		email := stringOrNil(body.Email)
		slackUserID := stringOrNil(body.SlackUserID)

		var id string
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
                INSERT INTO watchkeeper.human (
                    organization_id, display_name, email, slack_user_id
                )
                VALUES ($1, $2, $3, $4)
                RETURNING id
            `, body.OrganizationID, body.DisplayName, email, slackUserID).Scan(&id)
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				writeError(w, http.StatusConflict, "slack_user_id_conflict")
				return
			}
			writeError(w, http.StatusInternalServerError, "insert_human_failed")
			return
		}

		writeJSON(w, http.StatusCreated, insertHumanResponse{ID: id})
	})
}

// handleLookupHumanBySlackID serves GET /v1/humans/by-slack/{slack_user_id}.
// It returns the full row JSON keyed by `slack_user_id`; an unknown value
// surfaces as 404 not_found. The endpoint trusts the route pattern to
// supply a non-empty slack_user_id (the mux 404s an empty path segment),
// but still rejects an oversized value to keep the SQL parameter bound
// short enough that a malformed lookup never burns a megabyte query.
func handleLookupHumanBySlackID(r scopedRunner) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		claim, ok := ClaimFromContext(req.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "unauthorized")
			return
		}

		slackUserID := req.PathValue("slack_user_id")
		if slackUserID == "" {
			writeError(w, http.StatusBadRequest, "invalid_request")
			return
		}
		if len(slackUserID) > slackUserIDMaxBytes {
			writeError(w, http.StatusBadRequest, "invalid_slack_user_id")
			return
		}

		var out humanRow
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
                SELECT id, organization_id, display_name,
                       email, slack_user_id, created_at
                FROM watchkeeper.human
                WHERE slack_user_id = $1
            `, slackUserID).Scan(
				&out.ID, &out.OrganizationID, &out.DisplayName,
				&out.Email, &out.SlackUserID, &out.CreatedAt,
			)
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				writeError(w, http.StatusNotFound, "not_found")
				return
			}
			writeError(w, http.StatusInternalServerError, "lookup_human_failed")
			return
		}

		writeJSON(w, http.StatusOK, out)
	})
}

// updateWatchkeeperLeadRequest is the JSON body accepted by
// PATCH /v1/watchkeepers/{id}/lead. Only `lead_human_id` is allowed; the
// server's DisallowUnknownFields decoder rejects any other key. Empty
// values fail the UUID validation below before the row reaches Postgres.
type updateWatchkeeperLeadRequest struct {
	LeadHumanID string `json:"lead_human_id"`
}

// parseUpdateWatchkeeperLeadRequest validates the envelope and the
// lead_human_id UUID shape. The FK constraint on
// `watchkeeper.lead_human_id REFERENCES human(id)` is the integrity
// backstop; this validator just keeps a malformed UUID from surfacing as
// a confusing 500.
func parseUpdateWatchkeeperLeadRequest(w http.ResponseWriter, req *http.Request) (updateWatchkeeperLeadRequest, bool) {
	var body updateWatchkeeperLeadRequest

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
	if !uuidPattern.MatchString(body.LeadHumanID) {
		writeError(w, http.StatusBadRequest, "invalid_request")
		return body, false
	}
	return body, true
}

// handleSetWatchkeeperLead serves PATCH /v1/watchkeepers/{id}/lead. It
// updates the watchkeeper row's `lead_human_id` column. An unknown
// watchkeeper id yields 404 not_found; an FK violation on the
// human reference (23503) yields 400 invalid_lead_human_id so callers
// learn the human row does not exist without leaking SQL text. The
// status / spawned_at / retired_at columns are intentionally untouched
// by this handler — those transitions stay on the dedicated /status
// route.
func handleSetWatchkeeperLead(r scopedRunner) http.Handler {
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

		body, ok := parseUpdateWatchkeeperLeadRequest(w, req)
		if !ok {
			return
		}

		var notFound bool
		err := r.WithScope(req.Context(), claim, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `
                UPDATE watchkeeper.watchkeeper
                SET lead_human_id = $2
                WHERE id = $1
            `, id, body.LeadHumanID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				notFound = true
			}
			return nil
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23503" {
				writeError(w, http.StatusBadRequest, "invalid_lead_human_id")
				return
			}
			writeError(w, http.StatusInternalServerError, "set_watchkeeper_lead_failed")
			return
		}
		if notFound {
			writeError(w, http.StatusNotFound, "not_found")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})
}
