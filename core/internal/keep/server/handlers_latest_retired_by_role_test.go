package server_test

// handlers_latest_retired_by_role_test.go covers the M7.1.b predecessor-
// lookup handler
//
//	GET /v1/watchkeepers/latest-retired-by-role?role_id=<role>
//
// Covered:
//   - happy path: a row staged in the fake tx surfaces as 200 + JSON
//     envelope with role_id + archive_uri pinned non-nil.
//   - cross-tenant case: ErrNoRows from the JOIN-on-human filter
//     surfaces as 404 not_found (the predecessor row exists in
//     another tenant; the caller MUST NOT see 403 here — 403 vs 404
//     would leak row-existence to the wrong tenant).
//   - legacy-claim case: an empty claim.OrganizationID surfaces as
//     403 organization_required before the runner is invoked.
//   - missing role_id query param: 400 invalid_request before the
//     runner is invoked.
//   - SQL shape AC: the staged QueryRow captures the SQL string and
//     args; the assertion pins the partial-index-aware WHERE clause
//     (role_id, retired_at IS NOT NULL, archive_uri IS NOT NULL),
//     the JOIN-on-human filter, the ORDER BY retired_at DESC LIMIT 1
//     shape, and that the claim's OrganizationID is bound as $2.
//   - unauthenticated case: missing Authorization → 401.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

const (
	lrbrRoleID      = "frontline-watchkeeper"
	lrbrRowID       = "55555555-5555-4555-8555-555555555555"
	lrbrArchiveURI  = "s3://wk-archive/2026/05/" + lrbrRowID + ".jsonl"
	lrbrSpawnedAt   = "2026-05-01T10:00:00Z"
	lrbrRetiredAt   = "2026-05-10T12:00:00Z"
	lrbrCreatedAt   = "2026-04-30T12:00:00Z"
	lrbrOtherOrgID  = "77777777-7777-4777-8777-777777777777"
	lrbrManifestID  = "22222222-2222-4222-8222-222222222222"
	lrbrLeadHumanID = "33333333-3333-4333-8333-333333333333"
)

// lrbrHappyTx returns a pgx.Tx whose QueryRow stages the canonical
// retired-with-archive row: matching role_id, claim_org, retired_at
// recently stamped, archive_uri set.
func lrbrHappyTx(t *testing.T, gotSQL *string, gotArgs *[]any) pgx.Tx {
	t.Helper()
	retiredAt, err := time.Parse(time.RFC3339, lrbrRetiredAt)
	if err != nil {
		t.Fatalf("parse retiredAt: %v", err)
	}
	spawnedAt, err := time.Parse(time.RFC3339, lrbrSpawnedAt)
	if err != nil {
		t.Fatalf("parse spawnedAt: %v", err)
	}
	createdAt, err := time.Parse(time.RFC3339, lrbrCreatedAt)
	if err != nil {
		t.Fatalf("parse createdAt: %v", err)
	}
	archive := lrbrArchiveURI
	role := lrbrRoleID
	queryRow := func(_ context.Context, sql string, args ...any) pgx.Row {
		if gotSQL != nil {
			*gotSQL = sql
		}
		if gotArgs != nil {
			*gotArgs = args
		}
		return server.NewFakeRow(func(dest ...any) error {
			// Order MUST match handleGetLatestRetiredByRole's
			// Scan list:
			//   id, manifest_id, lead_human_id,
			//   active_manifest_version_id, status,
			//   spawned_at, retired_at, archive_uri,
			//   role_id, created_at
			if len(dest) != 10 {
				t.Fatalf("Scan dest len = %d, want 10", len(dest))
			}
			*dest[0].(*string) = lrbrRowID
			*dest[1].(*string) = lrbrManifestID
			*dest[2].(*string) = lrbrLeadHumanID
			// active_manifest_version_id stays nil for a retired row
			// in the inheritance fixture.
			*dest[4].(*string) = "retired"
			*dest[5].(**time.Time) = &spawnedAt
			*dest[6].(**time.Time) = &retiredAt
			*dest[7].(**string) = &archive
			*dest[8].(**string) = &role
			*dest[9].(*time.Time) = createdAt
			return nil
		})
	}
	return server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})
}

// TestGetLatestRetiredByRole_HappyPath — 200 + the staged row's id,
// role_id, archive_uri, retired_at echo back through the envelope.
func TestGetLatestRetiredByRole_HappyPath(t *testing.T) {
	var gotSQL string
	var gotArgs []any
	runner := &server.FakeScopedRunner{Tx: lrbrHappyTx(t, &gotSQL, &gotArgs)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet,
		"/v1/watchkeepers/latest-retired-by-role?role_id="+lrbrRoleID, tok, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		ID         string  `json:"id"`
		RoleID     *string `json:"role_id"`
		ArchiveURI *string `json:"archive_uri"`
		Status     string  `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if env.ID != lrbrRowID {
		t.Errorf("id = %q, want %q", env.ID, lrbrRowID)
	}
	if env.RoleID == nil || *env.RoleID != lrbrRoleID {
		t.Errorf("role_id = %v, want %q", env.RoleID, lrbrRoleID)
	}
	if env.ArchiveURI == nil || *env.ArchiveURI != lrbrArchiveURI {
		t.Errorf("archive_uri = %v, want %q", env.ArchiveURI, lrbrArchiveURI)
	}
	if env.Status != "retired" {
		t.Errorf("status = %q, want retired", env.Status)
	}
}

// TestGetLatestRetiredByRole_SQLShape — pins the SQL query so a refactor
// that drops the JOIN-on-human filter, the retired_at / archive_uri
// IS NOT NULL predicates, or the ORDER BY retired_at DESC LIMIT 1
// clause breaks loudly. Also pins the param binding ($1 = role_id,
// $2 = claim org).
func TestGetLatestRetiredByRole_SQLShape(t *testing.T) {
	var gotSQL string
	var gotArgs []any
	runner := &server.FakeScopedRunner{Tx: lrbrHappyTx(t, &gotSQL, &gotArgs)}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet,
		"/v1/watchkeepers/latest-retired-by-role?role_id="+lrbrRoleID, tok, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	// SQL fragment AC — keep these substrings exact so a downstream
	// refactor that loosens the WHERE / ORDER BY / LIMIT breaks loudly.
	wantFragments := []string{
		"JOIN watchkeeper.human",
		"h.id = w.lead_human_id",
		"w.role_id = $1",
		"h.organization_id = $2",
		"w.retired_at IS NOT NULL",
		"w.archive_uri IS NOT NULL",
		"ORDER BY w.retired_at DESC",
		"LIMIT 1",
	}
	for _, frag := range wantFragments {
		if !strings.Contains(gotSQL, frag) {
			t.Errorf("SQL missing %q; got:\n%s", frag, gotSQL)
		}
	}

	if len(gotArgs) != 2 {
		t.Fatalf("args len = %d, want 2 (role_id, claim_org); args=%v", len(gotArgs), gotArgs)
	}
	if gotArgs[0] != lrbrRoleID {
		t.Errorf("args[0] = %v, want role_id %q", gotArgs[0], lrbrRoleID)
	}
	if gotArgs[1] != testClaimOrgID {
		t.Errorf("args[1] = %v, want claim org %q", gotArgs[1], testClaimOrgID)
	}
}

// TestGetLatestRetiredByRole_CrossTenantNotFound — the JOIN-on-human
// filter hides the row from a caller whose claim's tenant does not
// match `h.organization_id`. Stage ErrNoRows (the shape pgx returns
// for a filtered-out row); assert 404 not_found AND that the claim's
// foreign org is bound to $2.
func TestGetLatestRetiredByRole_CrossTenantNotFound(t *testing.T) {
	var gotSQL string
	var gotArgs []any
	queryRow := func(_ context.Context, sql string, args ...any) pgx.Row {
		gotSQL = sql
		gotArgs = args
		return server.NewFakeRowErr(pgx.ErrNoRows)
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintTokenForOrg(t, ti, "org", lrbrOtherOrgID)

	rec := writeDo(t, h, http.MethodGet,
		"/v1/watchkeepers/latest-retired-by-role?role_id="+lrbrRoleID, tok, nil, "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotSQL, "organization_id") {
		t.Errorf("SELECT missing organization_id filter; got SQL: %s", gotSQL)
	}
	if len(gotArgs) < 2 {
		t.Fatalf("args len = %d, want >= 2", len(gotArgs))
	}
	if gotArgs[1] != lrbrOtherOrgID {
		t.Errorf("args[1] = %v, want claim org %q", gotArgs[1], lrbrOtherOrgID)
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

// TestGetLatestRetiredByRole_LegacyClaimRejected — empty
// OrganizationID surfaces as 403 organization_required before the
// runner is invoked. Mirrors the contract in
// handleUpdateWatchkeeperStatus / handleInsertWatchkeeper.
func TestGetLatestRetiredByRole_LegacyClaimRejected(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintLegacyToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet,
		"/v1/watchkeepers/latest-retired-by-role?role_id="+lrbrRoleID, tok, nil, "")

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

// TestGetLatestRetiredByRole_MissingRoleID_400 — a missing or empty
// role_id query param is rejected with 400 invalid_request before
// the runner fires.
func TestGetLatestRetiredByRole_MissingRoleID_400(t *testing.T) {
	cases := []struct {
		name, path string
	}{
		{"absent", "/v1/watchkeepers/latest-retired-by-role"},
		{"empty", "/v1/watchkeepers/latest-retired-by-role?role_id="},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodGet, tc.path, tok, nil, "")

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if runner.FnInvoked {
				t.Error("runner invoked despite missing role_id; expected pre-tx 400")
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

// TestGetLatestRetiredByRole_Unauthenticated — no Authorization → 401
// before the handler runs. Mirrors the contract on every other
// /v1/watchkeepers route.
func TestGetLatestRetiredByRole_Unauthenticated(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, _ := writeRouterForTest(t, mustFixedNow(), runner)

	rec := writeDo(t, h, http.MethodGet,
		"/v1/watchkeepers/latest-retired-by-role?role_id="+lrbrRoleID, "", nil, "")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner invoked despite missing auth; expected pre-handler 401")
	}
}
