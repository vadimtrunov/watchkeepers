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
// id. A unique-violation on the `human_slack_user_id_key` constraint is
// translated to 409 slack_user_id_conflict so the caller can decide
// whether to fall back to a lookup; any other 23505 (e.g. a future
// `(organization_id, email)` UNIQUE) falls through to the generic
// `conflict` reason so a new constraint never silently mislabels itself.
//
// Cross-tenant posture (KNOWN GAP, M4.4 review):
// `organization_id` is currently accepted from the request body and
// trusted as-is. The `auth.Claim` carries Subject + Scope only; it does
// NOT yet expose an OrganizationID, so the handler cannot pin org from
// claim today. The same posture exists on `human` (no RLS — see migration
// 012) and on `watchkeeper.watchkeeper` (no RLS — see migration 011), so
// every authenticated caller can write any org's rows. This must be
// closed by a follow-up that (1) plumbs `organization_id` onto
// `auth.Claim` and the capability broker mint path and (2) lands a
// per-org RLS policy on `human` keyed off the same scope/org GUC the
// knowledge_chunk policy uses (migration 005). Until then, deploy with
// operator-only access at the network/auth boundary. See ROADMAP-phase1
// §M4 → M4.4 review.
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
				// Pin the 409 reason to the slack-uniqueness constraint by
				// name so a future UNIQUE on `human` (e.g. an
				// `(organization_id, email)` constraint) does not silently
				// surface as `slack_user_id_conflict`. Any other 23505 is
				// reported with a generic `conflict` reason — still
				// retryable, but unambiguous about the constraint family.
				if pgErr.ConstraintName == "human_slack_user_id_key" {
					writeError(w, http.StatusConflict, "slack_user_id_conflict")
					return
				}
				writeError(w, http.StatusConflict, "conflict")
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

		// req.PathValue returns the percent-decoded path segment, so the
		// 64-byte cap below applies to the decoded value. A request of the
		// form `.../by-slack/%55%55…` with 65 `%55` triplets decodes to 65
		// `U` characters and is rejected here, not at the Postgres `text`
		// boundary. PII posture: Slack user IDs are workspace-public
		// identifiers (not Slack `email` / `team_id`), so logging the
		// decoded value at error level is acceptable; the bound parameter
		// is what the unique constraint matches against, and the cap keeps
		// a malformed lookup from burning megabyte-sized query plans.
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
// `watchkeeper_lead_human_id_fkey` constraint yields 400
// invalid_lead_human_id so callers learn the human row does not exist
// without leaking SQL text. Any other 23503 (a future FK on the same
// row) falls through to the generic 500 path so a new constraint never
// silently mislabels itself as `invalid_lead_human_id`. The status /
// spawned_at / retired_at columns are intentionally untouched by this
// handler — those transitions stay on the dedicated /status route.
//
// Scope-role policy: the route runs under `WithScope`, so any verified
// claim — `org`, `user:<id>`, or `agent:<id>` — currently passes the
// auth wall. This is intentional pending the broker refactor: an agent
// must be able to surface a self-rebind request (e.g. "promote my
// pairing human") through the same endpoint operators call. A future
// hardening pass should either (a) tighten this to org/user only and
// expose a separate agent-only flow, or (b) keep the open posture and
// add an audit-log row keyed off claim.Subject. Until then, deploy with
// operator-only access at the network/auth boundary.
//
// Cross-tenant posture (KNOWN GAP, M4.4 review): the UPDATE matches
// `WHERE id = $1` only, so any authenticated caller can rebind any
// watchkeeper across organizations. Closing this requires (1)
// `auth.Claim.OrganizationID` (broker refactor) and (2) `WHERE id = $1
// AND organization_id = $claim_org`. The same gap exists on every
// watchkeeper-table mutator (see migration 011), so M4.4 inherits the
// posture rather than introducing it. See ROADMAP-phase1 §M4 → M4.4
// review for the consolidated fix.
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
			// Pin the 400 reason to the lead-human FK by name so a future
			// FK on this row (e.g. an `organization_id` cross-row check)
			// does not silently surface as `invalid_lead_human_id`. Any
			// other 23503 falls through to the generic 500 path; a
			// follow-up should add a stable reason for those cases once
			// the next FK lands.
			if errors.As(err, &pgErr) && pgErr.Code == "23503" &&
				pgErr.ConstraintName == "watchkeeper_lead_human_id_fkey" {
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
