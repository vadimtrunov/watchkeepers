package server_test

// handlers_human_test.go covers the three M4.4 handlers (POST /v1/humans,
// GET /v1/humans/by-slack/{slack_user_id}, PATCH /v1/watchkeepers/{id}/lead)
// using the same FakeScopedRunner seam as the rest of the server tests.
// None of these tests open a real pgx pool; they stage tx-level behaviour
// via the fake helpers in export_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

const (
	humanFakeID    = "55555555-5555-4555-8555-555555555555"
	humanOrgID     = "66666666-6666-4666-8666-666666666666"
	humanSlackID   = "U07ABCDE123"
	humanFakeEmail = "lead@example.test"
)

// -----------------------------------------------------------------------
// Insert: POST /v1/humans
// -----------------------------------------------------------------------

// TestInsertHuman_HappyPath asserts a minimal body returns 201 + id and
// the runner sees claim.Scope.
func TestInsertHuman_HappyPath(t *testing.T) {
	runner := &server.FakeScopedRunner{FakeID: humanFakeID}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/humans", tok, map[string]any{
		"organization_id": humanOrgID,
		"display_name":    "Lead Operator",
	}, "")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.ID != humanFakeID {
		t.Errorf("id = %q, want %q", env.ID, humanFakeID)
	}
	if !runner.FnInvoked {
		t.Error("WithScope not invoked")
	}
}

// TestInsertHuman_OptionalFields asserts the optional email/slack_user_id
// fields are bound through to the runner's SQL args.
func TestInsertHuman_OptionalFields(t *testing.T) {
	var gotArgs []any
	queryRow := func(_ context.Context, _ string, args ...any) pgx.Row {
		gotArgs = args
		return server.NewFakeRow(func(dest ...any) error {
			*dest[0].(*string) = humanFakeID
			return nil
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/humans", tok, map[string]any{
		"organization_id": humanOrgID,
		"display_name":    "Lead Operator",
		"email":           humanFakeEmail,
		"slack_user_id":   humanSlackID,
	}, "")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(gotArgs) != 4 {
		t.Fatalf("args len = %d, want 4; args=%v", len(gotArgs), gotArgs)
	}
	if gotArgs[2] != humanFakeEmail {
		t.Errorf("email arg = %v, want %q", gotArgs[2], humanFakeEmail)
	}
	if gotArgs[3] != humanSlackID {
		t.Errorf("slack_user_id arg = %v, want %q", gotArgs[3], humanSlackID)
	}
}

// TestInsertHuman_OmittedOptionalsBindNil asserts the empty-string
// shorthand for absent email/slack_user_id reaches the runner as a nil
// `any` so SQL NULL fires (preserving the unique-constraint
// NULL-distinct semantics on slack_user_id).
func TestInsertHuman_OmittedOptionalsBindNil(t *testing.T) {
	var gotArgs []any
	queryRow := func(_ context.Context, _ string, args ...any) pgx.Row {
		gotArgs = args
		return server.NewFakeRow(func(dest ...any) error {
			*dest[0].(*string) = humanFakeID
			return nil
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/humans", tok, map[string]any{
		"organization_id": humanOrgID,
		"display_name":    "Lead Operator",
	}, "")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if len(gotArgs) != 4 {
		t.Fatalf("args len = %d, want 4; args=%v", len(gotArgs), gotArgs)
	}
	if gotArgs[2] != nil {
		t.Errorf("email arg = %v (%T), want nil any (SQL NULL)", gotArgs[2], gotArgs[2])
	}
	if gotArgs[3] != nil {
		t.Errorf("slack_user_id arg = %v (%T), want nil any (SQL NULL)", gotArgs[3], gotArgs[3])
	}
}

// TestInsertHuman_DuplicateSlackID_409 — a 23505 unique violation on
// `human_slack_user_id_key` is translated to 409 slack_user_id_conflict
// without leaking the raw SQL error text.
func TestInsertHuman_DuplicateSlackID_409(t *testing.T) {
	queryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRowErr(&pgconn.PgError{
			Code:           "23505",
			ConstraintName: "human_slack_user_id_key",
			Message:        "duplicate key value violates unique constraint \"human_slack_user_id_key\"",
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/humans", tok, map[string]any{
		"organization_id": humanOrgID,
		"display_name":    "Lead Operator",
		"slack_user_id":   humanSlackID,
	}, "")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "slack_user_id_conflict" {
		t.Errorf("error = %q, want slack_user_id_conflict", env.Error)
	}
	// Raw Postgres text must NOT leak.
	for _, forbidden := range []string{"duplicate", "human_slack_user_id_key", "violates"} {
		if strings.Contains(rec.Body.String(), forbidden) {
			t.Errorf("response body leaked %q: %s", forbidden, rec.Body.String())
		}
	}
}

// TestInsertHuman_InvalidUUID_400 — non-canonical organization_id is
// rejected before the row reaches Postgres.
func TestInsertHuman_InvalidUUID_400(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/humans", tok, map[string]any{
		"organization_id": "not-a-uuid",
		"display_name":    "Lead Operator",
	}, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked; expected rejection before tx")
	}
}

// TestInsertHuman_MissingDisplayName_400 — empty display_name surfaces a
// stable reason code before the row reaches Postgres.
func TestInsertHuman_MissingDisplayName_400(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/humans", tok, map[string]any{
		"organization_id": humanOrgID,
	}, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "missing_display_name" {
		t.Errorf("error = %q, want missing_display_name", env.Error)
	}
}

// TestInsertHuman_OversizedSlackID_400 — a slack_user_id exceeding the
// 64-byte ceiling is rejected before Postgres sees it.
func TestInsertHuman_OversizedSlackID_400(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/humans", tok, map[string]any{
		"organization_id": humanOrgID,
		"display_name":    "Lead Operator",
		"slack_user_id":   strings.Repeat("U", 65),
	}, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked; expected rejection before tx")
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "invalid_slack_user_id" {
		t.Errorf("error = %q, want invalid_slack_user_id", env.Error)
	}
}

// TestInsertHuman_UnknownField_400 — body containing the server-stamped
// `id` or `created_at` field is rejected by DisallowUnknownFields.
func TestInsertHuman_UnknownField_400(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"id", `{"organization_id":"` + humanOrgID + `","display_name":"x","id":"` + humanFakeID + `"}`},
		{"created_at", `{"organization_id":"` + humanOrgID + `","display_name":"x","created_at":"2026-01-01T00:00:00Z"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPost, "/v1/humans", tok, nil, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if runner.FnInvoked {
				t.Error("runner was invoked; expected rejection before tx")
			}
		})
	}
}

// -----------------------------------------------------------------------
// Lookup: GET /v1/humans/by-slack/{slack_user_id}
// -----------------------------------------------------------------------

// TestLookupHumanBySlackID_HappyPath — a configured row returns 200 and
// the JSON shape mirrors humanRow with nullable email surfacing as a
// non-nil pointer.
func TestLookupHumanBySlackID_HappyPath(t *testing.T) {
	queryRow := func(_ context.Context, _ string, args ...any) pgx.Row {
		// Bound argument MUST be the path parameter.
		if len(args) < 1 || args[0] != humanSlackID {
			t.Errorf("args[0] = %v, want %q", args[0], humanSlackID)
		}
		return server.NewFakeRow(func(dest ...any) error {
			// Order matches handleLookupHumanBySlackID's Scan list:
			//   id, organization_id, display_name,
			//   email, slack_user_id, created_at
			*dest[0].(*string) = humanFakeID
			*dest[1].(*string) = humanOrgID
			*dest[2].(*string) = "Lead Operator"
			email := humanFakeEmail
			*dest[3].(**string) = &email
			slackID := humanSlackID
			*dest[4].(**string) = &slackID
			*dest[5].(*time.Time) = time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
			return nil
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/humans/by-slack/"+humanSlackID, tok, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["id"] != humanFakeID {
		t.Errorf("id = %v, want %q", got["id"], humanFakeID)
	}
	if got["slack_user_id"] != humanSlackID {
		t.Errorf("slack_user_id = %v, want %q", got["slack_user_id"], humanSlackID)
	}
	if got["email"] != humanFakeEmail {
		t.Errorf("email = %v, want %q", got["email"], humanFakeEmail)
	}
}

// TestLookupHumanBySlackID_NotFound — pgx.ErrNoRows from the runner
// surfaces as 404 not_found via the handler's error mapping.
func TestLookupHumanBySlackID_NotFound(t *testing.T) {
	queryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRowErr(pgx.ErrNoRows)
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/humans/by-slack/U_NOT_FOUND", tok, nil, "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "not_found" {
		t.Errorf("error = %q, want not_found", env.Error)
	}
}

// TestLookupHumanBySlackID_Oversized_400 — a slack_user_id path segment
// longer than 64 bytes is rejected before the SQL parameter is bound.
func TestLookupHumanBySlackID_Oversized_400(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/humans/by-slack/"+strings.Repeat("U", 65), tok, nil, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked; expected rejection before tx")
	}
}

// TestLookupHumanBySlackID_NoToken_401 — request without bearer token is
// rejected by the auth wall before the handler runs.
func TestLookupHumanBySlackID_NoToken_401(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, _ := writeRouterForTest(t, mustFixedNow(), runner)

	rec := writeDo(t, h, http.MethodGet, "/v1/humans/by-slack/"+humanSlackID, "", nil, "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked; expected rejection before tx")
	}
}

// -----------------------------------------------------------------------
// SetLead: PATCH /v1/watchkeepers/{id}/lead
// -----------------------------------------------------------------------

// stageSetLeadTx returns a pgx.Tx whose Exec returns CommandTag
// "UPDATE <rowsAffected>" and captures the bound SQL + args.
func stageSetLeadTx(rowsAffected int64, gotSQL *string, gotArgs *[]any, execErr error) pgx.Tx {
	exec := func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
		if gotSQL != nil {
			*gotSQL = sql
		}
		if gotArgs != nil {
			*gotArgs = args
		}
		if execErr != nil {
			return pgconn.CommandTag{}, execErr
		}
		// pgconn.NewCommandTag parses a wire-format CommandTag string; an
		// "UPDATE <n>" tag yields RowsAffected() == n.
		return pgconn.NewCommandTag("UPDATE " + strconv.FormatInt(rowsAffected, 10)), nil
	}
	return server.NewFakeTx(server.FakeTxFns{Exec: exec})
}

// TestSetWatchkeeperLead_HappyPath — happy path returns 204 and the bound
// SQL targets watchkeeper.watchkeeper.lead_human_id with the supplied
// values.
func TestSetWatchkeeperLead_HappyPath(t *testing.T) {
	var gotSQL string
	var gotArgs []any
	runner := &server.FakeScopedRunner{Tx: stageSetLeadTx(1, &gotSQL, &gotArgs, nil)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/lead", tok,
		map[string]any{"lead_human_id": humanFakeID}, "")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotSQL, "SET lead_human_id = $2") {
		t.Errorf("SQL did not bind lead_human_id; got: %s", gotSQL)
	}
	if !strings.Contains(gotSQL, "WHERE id = $1") {
		t.Errorf("SQL did not bind id filter; got: %s", gotSQL)
	}
	if len(gotArgs) != 2 {
		t.Fatalf("args len = %d, want 2; args=%v", len(gotArgs), gotArgs)
	}
	if gotArgs[0] != wkFakeID {
		t.Errorf("args[0] = %v, want %q", gotArgs[0], wkFakeID)
	}
	if gotArgs[1] != humanFakeID {
		t.Errorf("args[1] = %v, want %q", gotArgs[1], humanFakeID)
	}
}

// TestSetWatchkeeperLead_UnknownWatchkeeper_404 — RowsAffected == 0
// surfaces as 404 not_found.
func TestSetWatchkeeperLead_UnknownWatchkeeper_404(t *testing.T) {
	runner := &server.FakeScopedRunner{Tx: stageSetLeadTx(0, nil, nil, nil)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/lead", tok,
		map[string]any{"lead_human_id": humanFakeID}, "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
}

// TestSetWatchkeeperLead_FKViolation_400 — a 23503 foreign-key violation
// (unknown human id) surfaces as 400 invalid_lead_human_id.
func TestSetWatchkeeperLead_FKViolation_400(t *testing.T) {
	fkErr := &pgconn.PgError{
		Code:           "23503",
		ConstraintName: "watchkeeper_lead_human_id_fkey",
		Message:        "insert or update on table \"watchkeeper\" violates foreign key constraint",
	}
	runner := &server.FakeScopedRunner{Tx: stageSetLeadTx(0, nil, nil, fkErr)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/lead", tok,
		map[string]any{"lead_human_id": humanFakeID}, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "invalid_lead_human_id" {
		t.Errorf("error = %q, want invalid_lead_human_id", env.Error)
	}
}

// TestSetWatchkeeperLead_InvalidUUID_400 — both the path id and the body
// lead_human_id must match the canonical UUID shape.
func TestSetWatchkeeperLead_InvalidUUID_400(t *testing.T) {
	cases := []struct {
		name, path string
		body       map[string]any
	}{
		{"path_uuid", "/v1/watchkeepers/not-a-uuid/lead", map[string]any{"lead_human_id": humanFakeID}},
		{"body_uuid", "/v1/watchkeepers/" + wkFakeID + "/lead", map[string]any{"lead_human_id": "not-a-uuid"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPatch, tc.path, tok, tc.body, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if runner.FnInvoked {
				t.Error("runner was invoked; expected rejection before tx")
			}
		})
	}
}

// TestSetWatchkeeperLead_UnknownField_400 — body containing any key other
// than `lead_human_id` is rejected by DisallowUnknownFields.
func TestSetWatchkeeperLead_UnknownField_400(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/lead", tok, nil,
		`{"lead_human_id":"`+humanFakeID+`","status":"active"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked; expected rejection before tx")
	}
}

// TestSetWatchkeeperLead_NoToken_401 — request without bearer token is
// rejected by the auth wall before the handler runs.
func TestSetWatchkeeperLead_NoToken_401(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, _ := writeRouterForTest(t, mustFixedNow(), runner)

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/lead", "",
		map[string]any{"lead_human_id": humanFakeID}, "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

// TestSetWatchkeeperLead_RunnerErrorBubblesUp — a non-FK error from the
// runner surfaces as 500 set_watchkeeper_lead_failed (and is NOT confused
// with the 400 FK-violation branch).
func TestSetWatchkeeperLead_RunnerErrorBubblesUp(t *testing.T) {
	runner := &server.FakeScopedRunner{FnReturns: errors.New("transport blew up")}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/lead", tok,
		map[string]any{"lead_human_id": humanFakeID}, "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "set_watchkeeper_lead_failed" {
		t.Errorf("error = %q, want set_watchkeeper_lead_failed", env.Error)
	}
}
