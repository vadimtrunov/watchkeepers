package server_test

// handlers_watchkeeper_test.go covers the four /v1/watchkeepers handlers
// (insert, status patch, get-by-id, list) using the same FakeScopedRunner
// seam as the rest of the server tests. None of these tests open a real
// pgx pool; they stage tx-level behaviour via the test-only fake helpers
// in export_test.go (NewFakeTx, NewFakeRow*, NewFakeRows).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

const (
	wkFakeID      = "11111111-1111-4111-8111-111111111111"
	wkManifestID  = "22222222-2222-4222-8222-222222222222"
	wkLeadHumanID = "33333333-3333-4333-8333-333333333333"
	wkActiveVerID = "44444444-4444-4444-8444-444444444444"
)

// -----------------------------------------------------------------------
// Insert: POST /v1/watchkeepers
// -----------------------------------------------------------------------

// TestInsertWatchkeeper_PendingByDefault asserts the happy path: a minimal
// body returns 201 + id and the runner sees claim.Scope.
func TestInsertWatchkeeper_PendingByDefault(t *testing.T) {
	runner := &server.FakeScopedRunner{FakeID: wkFakeID}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/watchkeepers", tok, map[string]any{
		"manifest_id":   wkManifestID,
		"lead_human_id": wkLeadHumanID,
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
	if env.ID != wkFakeID {
		t.Errorf("id = %q, want %q", env.ID, wkFakeID)
	}
	if !runner.FnInvoked {
		t.Error("WithScope not invoked")
	}
}

// TestInsertWatchkeeper_UnknownField_400 — body containing the
// server-stamped `spawned_at` field is rejected by DisallowUnknownFields.
func TestInsertWatchkeeper_UnknownField_400(t *testing.T) {
	cases := []struct {
		name, body string
	}{
		{"spawned_at", `{"manifest_id":"` + wkManifestID + `","lead_human_id":"` + wkLeadHumanID + `","spawned_at":"2026-01-01T00:00:00Z"}`},
		{"retired_at", `{"manifest_id":"` + wkManifestID + `","lead_human_id":"` + wkLeadHumanID + `","retired_at":"2026-01-01T00:00:00Z"}`},
		{"status", `{"manifest_id":"` + wkManifestID + `","lead_human_id":"` + wkLeadHumanID + `","status":"active"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPost, "/v1/watchkeepers", tok, nil, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if runner.FnInvoked {
				t.Error("runner was invoked; expected rejection before tx")
			}
		})
	}
}

// TestInsertWatchkeeper_InvalidUUID_400 — non-canonical manifest_id is
// rejected before the row reaches Postgres.
func TestInsertWatchkeeper_InvalidUUID_400(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/watchkeepers", tok, map[string]any{
		"manifest_id":   "not-a-uuid",
		"lead_human_id": wkLeadHumanID,
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
	if env.Error != "invalid_request" {
		t.Errorf("error = %q, want invalid_request", env.Error)
	}
}

// TestInsertWatchkeeper_NoToken_401 — request without bearer token is
// rejected by the auth wall before the handler runs.
func TestInsertWatchkeeper_NoToken_401(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, _ := writeRouterForTest(t, mustFixedNow(), runner)

	rec := writeDo(t, h, http.MethodPost, "/v1/watchkeepers", "", map[string]any{
		"manifest_id":   wkManifestID,
		"lead_human_id": wkLeadHumanID,
	}, "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
}

// TestInsertWatchkeeper_CrossTenantRejected — claim carries org X; the
// supplied lead_human_id resolves (in a real DB) to a human in org Y.
// M3.5.a.2 review fix: the INSERT routes through a WHERE EXISTS subquery
// on watchkeeper.human keyed by the claim org, so a cross-tenant caller
// cannot anchor a watchkeeper at another tenant's human. Under the new
// SQL the INSERT … RETURNING returns no row, surfaced as pgx.ErrNoRows
// → 404 not_found (mirrors the 404 contract on
// handleSetWatchkeeperLead). We assert (1) the handler bound the claim
// org parameter and (2) the response is 404 not_found.
func TestInsertWatchkeeper_CrossTenantRejected(t *testing.T) {
	const otherOrgID = "77777777-7777-4777-8777-777777777777"
	var gotSQL string
	var gotArgs []any
	queryRow := func(_ context.Context, sql string, args ...any) pgx.Row {
		gotSQL = sql
		gotArgs = args
		// Stage ErrNoRows: under the new SQL the INSERT … SELECT …
		// WHERE EXISTS clause matches zero rows when the lead human's
		// org does not equal the claim org, so RETURNING produces no
		// row and Scan surfaces pgx.ErrNoRows.
		return server.NewFakeRowErr(pgx.ErrNoRows)
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintTokenForOrg(t, ti, "org", otherOrgID)

	rec := writeDo(t, h, http.MethodPost, "/v1/watchkeepers", tok, map[string]any{
		"manifest_id":   wkManifestID,
		"lead_human_id": wkLeadHumanID,
	}, "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	// Org filter MUST be on the SQL — without it any authenticated
	// caller could anchor a watchkeeper at any human in any tenant.
	if !strings.Contains(gotSQL, "organization_id") {
		t.Errorf("INSERT missing organization_id filter; got SQL: %s", gotSQL)
	}
	// The handler binds the claim org as the fourth parameter
	// (manifest_id, lead_human_id, active_manifest_version_id, claim_org).
	if len(gotArgs) < 4 {
		t.Fatalf("args len = %d, want >= 4 (manifest_id, lead_human_id, active_mv_id, claim_org); args=%v", len(gotArgs), gotArgs)
	}
	if gotArgs[3] != otherOrgID {
		t.Errorf("args[3] = %v, want claim org %q", gotArgs[3], otherOrgID)
	}
}

// TestInsertWatchkeeper_LegacyClaimRejected — claim carries an EMPTY
// OrganizationID (the pre-M3.5.a.1 wire shape). M3.5.a.2 review fix
// mirrors the contract on handleInsertHuman / handleSetWatchkeeperLead /
// handleUpdateWatchkeeperStatus: 403 organization_required before any
// DB work runs.
func TestInsertWatchkeeper_LegacyClaimRejected(t *testing.T) {
	runner := &server.FakeScopedRunner{FakeID: wkFakeID}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintLegacyToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/watchkeepers", tok, map[string]any{
		"manifest_id":   wkManifestID,
		"lead_human_id": wkLeadHumanID,
	}, "")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked despite empty-org claim; expected pre-tx 403")
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "organization_required" {
		t.Errorf("error = %q, want organization_required", env.Error)
	}
}

// -----------------------------------------------------------------------
// Update: PATCH /v1/watchkeepers/{id}/status
// -----------------------------------------------------------------------

// stageUpdateTx returns a pgx.Tx whose first QueryRow returns the supplied
// `current` status (or pgx.ErrNoRows when notFound is true), and whose Exec
// returns success. Captures the executed SQL fragment so the caller can
// assert which UPDATE branch fired.
func stageUpdateTx(t *testing.T, current string, notFound bool, gotSQL *string) pgx.Tx {
	t.Helper()
	queryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		if notFound {
			return server.NewFakeRowErr(pgx.ErrNoRows)
		}
		return server.NewFakeRow(func(dest ...any) error {
			if len(dest) > 0 {
				if sp, ok := dest[0].(*string); ok {
					*sp = current
				}
			}
			return nil
		})
	}
	exec := func(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
		if gotSQL != nil {
			*gotSQL = sql
		}
		return pgconn.CommandTag{}, nil
	}
	return server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow, Exec: exec})
}

// TestUpdateWatchkeeperStatus_PendingToActive_StampsSpawnedAt — pending row,
// PATCH to active: the handler must execute the UPDATE branch that stamps
// spawned_at = now().
func TestUpdateWatchkeeperStatus_PendingToActive_StampsSpawnedAt(t *testing.T) {
	var execSQL string
	runner := &server.FakeScopedRunner{Tx: stageUpdateTx(t, "pending", false, &execSQL)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "active"}, "")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(execSQL, "spawned_at = now()") {
		t.Errorf("UPDATE did not stamp spawned_at; got SQL: %s", execSQL)
	}
	if !strings.Contains(execSQL, "status = 'active'") {
		t.Errorf("UPDATE did not set status='active'; got SQL: %s", execSQL)
	}
}

// TestUpdateWatchkeeperStatus_ActiveToRetired_StampsRetiredAt — active row,
// PATCH to retired: the handler must execute the UPDATE branch that stamps
// retired_at = now().
func TestUpdateWatchkeeperStatus_ActiveToRetired_StampsRetiredAt(t *testing.T) {
	var execSQL string
	runner := &server.FakeScopedRunner{Tx: stageUpdateTx(t, "active", false, &execSQL)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "retired"}, "")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(execSQL, "retired_at = now()") {
		t.Errorf("UPDATE did not stamp retired_at; got SQL: %s", execSQL)
	}
	if !strings.Contains(execSQL, "status = 'retired'") {
		t.Errorf("UPDATE did not set status='retired'; got SQL: %s", execSQL)
	}
}

// TestUpdateWatchkeeperStatus_RetiredToActive_400 — retired→active is
// forbidden; handler returns 400 invalid_status_transition without executing
// the UPDATE.
func TestUpdateWatchkeeperStatus_RetiredToActive_400(t *testing.T) {
	var execSQL string
	runner := &server.FakeScopedRunner{Tx: stageUpdateTx(t, "retired", false, &execSQL)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "active"}, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if execSQL != "" {
		t.Errorf("UPDATE was executed despite forbidden transition: %s", execSQL)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "invalid_status_transition" {
		t.Errorf("error = %q, want invalid_status_transition", env.Error)
	}
}

// TestUpdateWatchkeeperStatus_PendingToRetiredDirect_400 — pending→retired
// without an intermediate active stop is forbidden.
func TestUpdateWatchkeeperStatus_PendingToRetiredDirect_400(t *testing.T) {
	var execSQL string
	runner := &server.FakeScopedRunner{Tx: stageUpdateTx(t, "pending", false, &execSQL)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "retired"}, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if execSQL != "" {
		t.Errorf("UPDATE was executed despite forbidden transition: %s", execSQL)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "invalid_status_transition" {
		t.Errorf("error = %q, want invalid_status_transition", env.Error)
	}
}

// TestUpdateWatchkeeperStatus_UnknownID_404 — unknown id yields pgx.ErrNoRows
// from the SELECT FOR UPDATE; handler maps that to 404 not_found.
func TestUpdateWatchkeeperStatus_UnknownID_404(t *testing.T) {
	runner := &server.FakeScopedRunner{Tx: stageUpdateTx(t, "", true, nil)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "active"}, "")

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

// TestUpdateWatchkeeperStatus_UnknownField_400 — body containing the
// server-stamped `retired_at` field is rejected by DisallowUnknownFields.
func TestUpdateWatchkeeperStatus_UnknownField_400(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok, nil,
		`{"status":"active","retired_at":"2026-01-01T00:00:00Z"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked; expected rejection before tx")
	}
}

// stageUpdateTxCapturingArgs is the M7.2.c sibling of [stageUpdateTx]
// that also captures the Exec args slice so the caller can assert
// the archive_uri parameter binding on the active→retired branch.
func stageUpdateTxCapturingArgs(t *testing.T, current string, gotSQL *string, gotArgs *[]any) pgx.Tx {
	t.Helper()
	queryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRow(func(dest ...any) error {
			if len(dest) > 0 {
				if sp, ok := dest[0].(*string); ok {
					*sp = current
				}
			}
			return nil
		})
	}
	exec := func(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
		if gotSQL != nil {
			*gotSQL = sql
		}
		if gotArgs != nil {
			*gotArgs = append([]any(nil), args...)
		}
		return pgconn.CommandTag{}, nil
	}
	return server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow, Exec: exec})
}

// TestUpdateWatchkeeperStatus_ActiveToRetiredWithArchiveURI_StampsColumn —
// M7.2.c: the active→retired branch persists the supplied archive_uri
// onto the new column when the body field is non-empty. Pins the SQL
// shape (`archive_uri = $2`) and the parameter binding so a future
// regression that drops the URI silently surfaces here.
func TestUpdateWatchkeeperStatus_ActiveToRetiredWithArchiveURI_StampsColumn(t *testing.T) {
	const wantURI = "file:///snapshots/wk-active/2026-05-09T12-34-56Z.tar.gz"
	var execSQL string
	var execArgs []any
	runner := &server.FakeScopedRunner{Tx: stageUpdateTxCapturingArgs(t, "active", &execSQL, &execArgs)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "retired", "archive_uri": wantURI}, "")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(execSQL, "retired_at = now()") {
		t.Errorf("UPDATE did not stamp retired_at; got SQL: %s", execSQL)
	}
	if !strings.Contains(execSQL, "archive_uri = $2") {
		t.Errorf("UPDATE did not bind archive_uri = $2 (M7.2.c regression); got SQL: %s", execSQL)
	}
	if len(execArgs) < 2 {
		t.Fatalf("Exec args len = %d, want >= 2 (id, archive_uri); args=%v", len(execArgs), execArgs)
	}
	if execArgs[1] != wantURI {
		t.Errorf("Exec args[1] = %v, want %q (archive_uri must be bound verbatim)", execArgs[1], wantURI)
	}
}

// TestUpdateWatchkeeperStatus_ActiveToRetiredNoArchiveURI_LeavesColumnNull —
// M7.2.c: omitting `archive_uri` in the body keeps the legacy
// (M6.2.c synchronous tool) wire-shape green. The handler runs the
// no-archive UPDATE branch which sets `archive_uri = NULL` explicitly
// (defense-in-depth: a re-mark on a previously-retired row never
// carries a stale archive_uri forward) and binds only the id parameter.
func TestUpdateWatchkeeperStatus_ActiveToRetiredNoArchiveURI_LeavesColumnNull(t *testing.T) {
	var execSQL string
	var execArgs []any
	runner := &server.FakeScopedRunner{Tx: stageUpdateTxCapturingArgs(t, "active", &execSQL, &execArgs)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "retired"}, "")

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(execSQL, "retired_at = now()") {
		t.Errorf("UPDATE did not stamp retired_at; got SQL: %s", execSQL)
	}
	if !strings.Contains(execSQL, "archive_uri = NULL") {
		t.Errorf("UPDATE did not set archive_uri = NULL on the no-archive branch (M7.2.c iter-2 explicit-NULL pattern); got SQL: %s", execSQL)
	}
	if strings.Contains(execSQL, "archive_uri = $") {
		t.Errorf("UPDATE bound archive_uri as a parameter despite omitted body field; got SQL: %s", execSQL)
	}
	if len(execArgs) != 1 {
		t.Errorf("Exec args len = %d, want 1 (id only — no archive_uri binding); args=%v", len(execArgs), execArgs)
	}
}

// TestUpdateWatchkeeperStatus_PendingToActiveWithArchiveURI_400 — M7.2.c:
// archive_uri only makes sense on the active→retired transition. A
// pending→active body that smuggles archive_uri is rejected pre-tx
// with 400 invalid_request so a wiring bug cannot poison the column
// on the spawn-side path.
func TestUpdateWatchkeeperStatus_PendingToActiveWithArchiveURI_400(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "active", "archive_uri": "file:///wrong-direction.tar.gz"}, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked; expected pre-tx rejection on archive_uri+active combination")
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "invalid_request" {
		t.Errorf("error = %q, want invalid_request", env.Error)
	}
}

// TestUpdateWatchkeeperStatus_BlankArchiveURI_400 — M7.2.c: a whitespace-
// only archive_uri string (e.g. `" "` from a buggy client) is rejected
// pre-tx so the column never receives a blank string. The saga step
// pre-validates URI shape before it reaches the wire (M7.2.b
// ErrInvalidArchiveURI), so this server-side gate is defense-in-depth.
func TestUpdateWatchkeeperStatus_BlankArchiveURI_400(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "retired", "archive_uri": "   "}, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked; expected pre-tx rejection on blank archive_uri")
	}
}

// TestUpdateWatchkeeperStatus_ExplicitNullArchiveURI_OnRetired_400 —
// M7.2.c iter-2 codex finding (Major): the on-wire shape
// `"archive_uri": null` is structurally distinct from absent and must
// not silently fall into the legacy no-archive branch. Test pins the
// `Present && Null` rejection at the parser layer with no DB touch.
func TestUpdateWatchkeeperStatus_ExplicitNullArchiveURI_OnRetired_400(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "retired", "archive_uri": nil}, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked; expected pre-tx rejection on explicit-null archive_uri")
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "invalid_request" {
		t.Errorf("error = %q, want invalid_request", env.Error)
	}
}

// TestUpdateWatchkeeperStatus_ExplicitNullArchiveURI_OnActive_400 —
// M7.2.c iter-2 codex finding (Major): a `"archive_uri": null` paired
// with `status:"active"` is also a wiring bug. The original `*string`
// shape silently accepted it (Go's json decodes null to a nil
// pointer, which the old `body.ArchiveURI != nil` guard treated as
// absent); the new `optionalArchiveURI` shape's `Present && Null`
// branch rejects it pre-tx.
func TestUpdateWatchkeeperStatus_ExplicitNullArchiveURI_OnActive_400(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "active", "archive_uri": nil}, "")

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked; expected pre-tx rejection on explicit-null archive_uri")
	}
}

// TestUpdateWatchkeeperStatus_SchemelessArchiveURI_400 — M7.2.c iter-2
// codex finding (Major): the wire contract documents archive_uri as
// an RFC 3986 URI with a non-empty scheme, but the iter-1 server only
// rejected blank values. Strings like `"garbage"` or `"../../tmp"`
// would round-trip onto the column for any caller that bypassed the
// saga path; the absolute-URI gate at the HTTP boundary closes that
// hole as defense-in-depth (the saga step + keepclient pre-validate
// the same shape upstream).
func TestUpdateWatchkeeperStatus_SchemelessArchiveURI_400(t *testing.T) {
	cases := []struct {
		name string
		uri  string
	}{
		{name: "bare_word", uri: "garbage"},
		{name: "relative_path", uri: "../../tmp"},
		{name: "leading_slash_only", uri: "/snapshots/wk.tar.gz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
				map[string]any{"status": "retired", "archive_uri": tc.uri}, "")

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if runner.FnInvoked {
				t.Errorf("runner was invoked on %q; expected pre-tx rejection on schemeless URI", tc.uri)
			}
			var env struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error != "invalid_request" {
				t.Errorf("error = %q, want invalid_request", env.Error)
			}
		})
	}
}

// TestUpdateWatchkeeperStatus_AbsoluteURISchemes_AcceptedRetire — M7.2.c
// iter-2 positive complement to TestUpdateWatchkeeperStatus_SchemelessArchiveURI_400:
// the absolute-URI gate accepts every scheme the spawn-side
// archivestore can mint (file://, s3://, gs://, plus a synthetic
// `test://` to pin "any non-empty scheme" rather than a hardcoded
// allowlist).
func TestUpdateWatchkeeperStatus_AbsoluteURISchemes_AcceptedRetire(t *testing.T) {
	cases := []struct {
		name string
		uri  string
	}{
		{name: "file_scheme", uri: "file:///snapshots/wk-active/2026-05-09T12-34-56Z.tar.gz"},
		{name: "s3_scheme", uri: "s3://archives-bucket/wk/2026-05-09T12-34-56Z.tar.gz"},
		{name: "gs_scheme", uri: "gs://archives-bucket/wk/2026-05-09T12-34-56Z.tar.gz"},
		{name: "test_scheme", uri: "test://fake/host/path.tar.gz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var execSQL string
			var execArgs []any
			runner := &server.FakeScopedRunner{Tx: stageUpdateTxCapturingArgs(t, "active", &execSQL, &execArgs)}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
				map[string]any{"status": "retired", "archive_uri": tc.uri}, "")

			if rec.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204 for %q; body=%s", rec.Code, tc.uri, rec.Body.String())
			}
			if len(execArgs) < 2 || execArgs[1] != tc.uri {
				t.Errorf("Exec args[1] = %v, want %q", execArgs, tc.uri)
			}
		})
	}
}

// -----------------------------------------------------------------------
// Get: GET /v1/watchkeepers/{id}
// -----------------------------------------------------------------------

// TestGetWatchkeeper_ReturnsFullRow — happy path, GET returns every column
// including nullable timestamps.
func TestGetWatchkeeper_ReturnsFullRow(t *testing.T) {
	spawnedAt := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	queryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRow(func(dest ...any) error {
			// Order matches handleGetWatchkeeper's Scan list:
			//   id, manifest_id, lead_human_id,
			//   active_manifest_version_id, status,
			//   spawned_at, retired_at, archive_uri, role_id, created_at
			*dest[0].(*string) = wkFakeID
			*dest[1].(*string) = wkManifestID
			*dest[2].(*string) = wkLeadHumanID
			active := wkActiveVerID
			*dest[3].(**string) = &active
			*dest[4].(*string) = "active"
			*dest[5].(**time.Time) = &spawnedAt
			*dest[6].(**time.Time) = nil
			// archive_uri NULL on this active-status row.
			*dest[7].(**string) = nil
			// role_id NULL on this row (M7.1.a happy path: legacy row).
			*dest[8].(**string) = nil
			*dest[9].(*time.Time) = time.Date(2026, 4, 30, 10, 0, 0, 0, time.UTC)
			return nil
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/watchkeepers/"+wkFakeID, tok, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["id"] != wkFakeID {
		t.Errorf("id = %v, want %q", got["id"], wkFakeID)
	}
	if got["status"] != "active" {
		t.Errorf("status = %v, want active", got["status"])
	}
	if got["active_manifest_version_id"] != wkActiveVerID {
		t.Errorf("active_manifest_version_id = %v, want %q", got["active_manifest_version_id"], wkActiveVerID)
	}
	if got["retired_at"] != nil {
		t.Errorf("retired_at = %v, want null", got["retired_at"])
	}
	if got["spawned_at"] == nil {
		t.Errorf("spawned_at unexpectedly null in body=%s", rec.Body.String())
	}
}

// TestGetWatchkeeper_NotFound — pgx.ErrNoRows from the runner surfaces as
// 404 not_found via the handler's error mapping.
func TestGetWatchkeeper_NotFound(t *testing.T) {
	queryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRowErr(pgx.ErrNoRows)
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/watchkeepers/"+wkFakeID, tok, nil, "")

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

// -----------------------------------------------------------------------
// List: GET /v1/watchkeepers
// -----------------------------------------------------------------------

// makeListScans builds a slice of row-Scan closures matching the Scan
// signature in handleListWatchkeepers — one closure per supplied status.
func makeListScans(t *testing.T, statuses []string) []func(dest ...any) error {
	t.Helper()
	out := make([]func(dest ...any) error, 0, len(statuses))
	for i, status := range statuses {
		i, status := i, status
		out = append(out, func(dest ...any) error {
			*dest[0].(*string) = wkFakeID
			*dest[1].(*string) = wkManifestID
			*dest[2].(*string) = wkLeadHumanID
			*dest[3].(**string) = nil
			*dest[4].(*string) = status
			*dest[5].(**time.Time) = nil
			*dest[6].(**time.Time) = nil
			// archive_uri NULL on every staged row — list test does not
			// exercise the M7.2.c retire-with-archive path.
			*dest[7].(**string) = nil
			// role_id NULL on every staged row — M7.1.a list test does
			// not exercise the role_id projection (covered by its own
			// dedicated test below).
			*dest[8].(**string) = nil
			// Stagger created_at by row index so the DESC order is
			// observable end-to-end (the fake does not actually sort —
			// it just returns rows in the order the test stages them).
			// Row 0 is the newest (largest created_at); row N is oldest.
			base := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
			*dest[9].(*time.Time) = base.Add(-time.Duration(i) * time.Hour)
			return nil
		})
	}
	return out
}

// TestListWatchkeepers_FilterByStatus — three rows of different statuses;
// the handler binds `?status=active` to the WHERE clause and the Query
// fake returns only the active row. We assert the on-wire `items` length.
func TestListWatchkeepers_FilterByStatus(t *testing.T) {
	var gotSQL string
	var gotArgs []any
	query := func(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
		gotSQL = sql
		gotArgs = args
		// The handler issues a single query per call; the fake returns the
		// "active" row only, mirroring how Postgres would respond to the
		// WHERE status='active' filter.
		return server.NewFakeRows(makeListScans(t, []string{"active"}), nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/watchkeepers?status=active", tok, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items len = %d, want 1; body=%s", len(got.Items), rec.Body.String())
	}
	if got.Items[0]["status"] != "active" {
		t.Errorf("status = %v, want active", got.Items[0]["status"])
	}
	// Confirm the handler bound the status filter to the SQL parameter.
	if !strings.Contains(gotSQL, "WHERE status = $1") {
		t.Errorf("SQL did not bind status filter; got: %s", gotSQL)
	}
	if len(gotArgs) < 1 || gotArgs[0] != "active" {
		t.Errorf("args = %v, want first=\"active\"", gotArgs)
	}
}

// TestListWatchkeepers_DefaultLimit — when ?limit is omitted the handler
// passes the default limit (50) to the SQL bind, even if the runner returns
// fewer rows. We assert the bound argument.
func TestListWatchkeepers_DefaultLimit(t *testing.T) {
	var gotArgs []any
	query := func(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
		gotArgs = args
		// Stage a single row so the handler's append loop runs at least
		// once; the assertion is on the bound LIMIT, not on row count.
		return server.NewFakeRows(makeListScans(t, []string{"pending"}), nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/watchkeepers", tok, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(gotArgs) < 1 {
		t.Fatalf("no SQL args bound; want default LIMIT (50)")
	}
	limit, ok := gotArgs[0].(int)
	if !ok || limit != 50 {
		t.Errorf("default LIMIT = %v (%T), want 50 (int)", gotArgs[0], gotArgs[0])
	}
}

// TestListWatchkeepers_LimitOutOfRange_400 — limit values outside (0, 200]
// are rejected with 400 before the runner is invoked.
func TestListWatchkeepers_LimitOutOfRange_400(t *testing.T) {
	cases := []struct {
		name, limit string
	}{
		{"zero", "0"},
		{"negative", "-1"},
		{"too_high", "300"},
		{"non_numeric", "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodGet, "/v1/watchkeepers?limit="+tc.limit, tok, nil, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if runner.FnInvoked {
				t.Error("runner invoked; expected rejection before tx")
			}
		})
	}
}

// -----------------------------------------------------------------------
// Cross-cutting: runner errors + auth wall
// -----------------------------------------------------------------------

// TestWatchkeeper_RunnerErrorBubblesUp — a non-pgx runner error surfaces as
// 500 with the per-endpoint stable code, never the raw text.
func TestWatchkeeper_RunnerErrorBubblesUp(t *testing.T) {
	cases := []struct {
		name, method, path, wantErr string
		body                        any
	}{
		{
			name:    "insert",
			method:  http.MethodPost,
			path:    "/v1/watchkeepers",
			wantErr: "insert_watchkeeper_failed",
			body:    map[string]any{"manifest_id": wkManifestID, "lead_human_id": wkLeadHumanID},
		},
		{
			name:    "update",
			method:  http.MethodPatch,
			path:    "/v1/watchkeepers/" + wkFakeID + "/status",
			wantErr: "update_watchkeeper_status_failed",
			body:    map[string]any{"status": "active"},
		},
		{
			name:    "get",
			method:  http.MethodGet,
			path:    "/v1/watchkeepers/" + wkFakeID,
			wantErr: "get_watchkeeper_failed",
		},
		{
			name:    "list",
			method:  http.MethodGet,
			path:    "/v1/watchkeepers",
			wantErr: "list_watchkeepers_failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{FnReturns: errors.New("database unreachable")}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, tc.method, tc.path, tok, tc.body, "")
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
			}
			var env struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error != tc.wantErr {
				t.Errorf("error = %q, want %q", env.Error, tc.wantErr)
			}
			if strings.Contains(rec.Body.String(), "database unreachable") {
				t.Errorf("raw runner error leaked: %s", rec.Body.String())
			}
		})
	}
}

// TestListWatchkeepers_OrderedByCreatedAtDESC — stages 3 rows whose
// created_at values are staggered (row 0 newest, row 2 oldest) and asserts
// the handler returns them in descending order. The fake does not sort; the
// test relies on makeListScans producing the rows in the expected order so
// that any regression where the handler re-orders or skips rows is caught.
func TestListWatchkeepers_OrderedByCreatedAtDESC(t *testing.T) {
	statuses := []string{"pending", "active", "retired"}
	query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		return server.NewFakeRows(makeListScans(t, statuses), nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/watchkeepers", tok, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Items) != 3 {
		t.Fatalf("items len = %d, want 3; body=%s", len(got.Items), rec.Body.String())
	}
	// Parse created_at strings back to time.Time for comparison.
	parseCreatedAt := func(item map[string]any) time.Time {
		t.Helper()
		s, ok := item["created_at"].(string)
		if !ok {
			t.Fatalf("created_at not a string: %v", item["created_at"])
		}
		ts, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			t.Fatalf("parse created_at %q: %v", s, err)
		}
		return ts
	}
	t0 := parseCreatedAt(got.Items[0])
	t1 := parseCreatedAt(got.Items[1])
	t2 := parseCreatedAt(got.Items[2])
	if !t0.After(t1) {
		t.Errorf("items[0].created_at (%v) should be after items[1].created_at (%v)", t0, t1)
	}
	if !t1.After(t2) {
		t.Errorf("items[1].created_at (%v) should be after items[2].created_at (%v)", t1, t2)
	}
}

// TestUpdateWatchkeeperStatus_BadTargetStatus_400 — body with a status value
// that is not a valid target ("weird" is not a recognised status; "pending" is
// not a permitted transition target) must be rejected with 400 before the
// runner is invoked.
func TestUpdateWatchkeeperStatus_BadTargetStatus_400(t *testing.T) {
	cases := []struct {
		name   string
		status string
	}{
		{"unknown_status", "weird"},
		{"pending_not_valid_target", "pending"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
				map[string]any{"status": tc.status}, "")

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
			if env.Error == "" {
				t.Errorf("error field empty; want non-empty error code")
			}
		})
	}
}

// TestUpdateWatchkeeperStatus_InvalidPathID_400 — a path segment that is not a
// valid UUID must be rejected with 400 before the runner is invoked.
func TestUpdateWatchkeeperStatus_InvalidPathID_400(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/not-a-uuid/status", tok,
		map[string]any{"status": "active"}, "")

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
	if env.Error == "" {
		t.Errorf("error field empty; want non-empty error code")
	}
}

// -----------------------------------------------------------------------
// M3.5.a.2 cross-tenant rejection coverage (watchkeeper-table mutators)
// -----------------------------------------------------------------------

// TestUpdateWatchkeeperStatus_CrossTenantNotFound — the watchkeeper row
// is invisible to the SELECT FOR UPDATE because its lead human's
// organization_id does not match the claim's org. M3.5.a.2 routes the
// SELECT through a JOIN on watchkeeper.human keyed by the claim org,
// so a cross-tenant caller sees pgx.ErrNoRows → 404, not silent
// success. We also assert the handler bound the claim org parameter
// to the SELECT.
func TestUpdateWatchkeeperStatus_CrossTenantNotFound(t *testing.T) {
	const otherOrgID = "77777777-7777-4777-8777-777777777777"
	var gotSelectSQL string
	var gotSelectArgs []any
	queryRow := func(_ context.Context, sql string, args ...any) pgx.Row {
		gotSelectSQL = sql
		gotSelectArgs = args
		// Stage ErrNoRows: under the new SQL the row is filtered out by
		// the JOIN-through-human org clause, mirroring how Postgres
		// would respond when claim_org != row_org.
		return server.NewFakeRowErr(pgx.ErrNoRows)
	}
	exec := func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
		t.Fatal("UPDATE should not run when the SELECT FOR UPDATE returned no rows")
		return pgconn.CommandTag{}, nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow, Exec: exec})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintTokenForOrg(t, ti, "org", otherOrgID)

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "active"}, "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotSelectSQL, "organization_id") {
		t.Errorf("SELECT FOR UPDATE missing organization_id filter; got SQL: %s", gotSelectSQL)
	}
	// Handler binds the claim org as the second parameter (after id).
	if len(gotSelectArgs) < 2 {
		t.Fatalf("args len = %d, want >= 2 (id, claim_org); args=%v", len(gotSelectArgs), gotSelectArgs)
	}
	if gotSelectArgs[1] != otherOrgID {
		t.Errorf("args[1] = %v, want claim org %q", gotSelectArgs[1], otherOrgID)
	}
}

// TestUpdateWatchkeeperStatus_LegacyClaimRejected — empty
// OrganizationID surfaces as 403 organization_required before the
// runner is invoked. Mirrors the contract in handleInsertHuman /
// handleSetWatchkeeperLead.
func TestUpdateWatchkeeperStatus_LegacyClaimRejected(t *testing.T) {
	runner := &server.FakeScopedRunner{Tx: stageUpdateTx(t, "pending", false, nil)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintLegacyToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPatch, "/v1/watchkeepers/"+wkFakeID+"/status", tok,
		map[string]any{"status": "active"}, "")

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked despite empty-org claim; expected pre-tx 403")
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "organization_required" {
		t.Errorf("error = %q, want organization_required", env.Error)
	}
}

// TestWatchkeeper_UnauthenticatedRejects — every watchkeeper route sits
// behind the auth wall; no Authorization header → 401.
func TestWatchkeeper_UnauthenticatedRejects(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, _ := writeRouterForTest(t, mustFixedNow(), runner)

	cases := []struct {
		name, method, path string
		body               any
	}{
		{"insert", http.MethodPost, "/v1/watchkeepers", map[string]any{"manifest_id": wkManifestID, "lead_human_id": wkLeadHumanID}},
		{"update", http.MethodPatch, "/v1/watchkeepers/" + wkFakeID + "/status", map[string]any{"status": "active"}},
		{"get", http.MethodGet, "/v1/watchkeepers/" + wkFakeID, nil},
		{"list", http.MethodGet, "/v1/watchkeepers", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := writeDo(t, h, tc.method, tc.path, "", tc.body, "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
