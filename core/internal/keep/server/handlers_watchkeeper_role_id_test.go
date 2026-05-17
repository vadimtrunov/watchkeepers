package server_test

// handlers_watchkeeper_role_id_test.go covers the M7.1.a `role_id`
// projection on the four /v1/watchkeepers handlers (insert, get-by-id,
// list). The dedicated file mirrors the M3.3 manifest_version-metadata
// pattern (see `handlers_manifest_metadata_test.go`): the sibling tests
// in `handlers_watchkeeper_test.go` exercise the legacy NULL-projection
// happy paths, and this file pins the non-NULL round-trip + the
// non-blank pre-tx gate on the insert path.

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

const wkRoleID = "frontline-watchkeeper"

// -----------------------------------------------------------------------
// Insert: POST /v1/watchkeepers — role_id round-trip.
// -----------------------------------------------------------------------

// TestInsertWatchkeeper_WithRoleID_201_PersistsBind asserts the happy
// round-trip: a non-empty `role_id` body field is forwarded through
// [stringOrNil] to the INSERT statement at the documented $5 slot. The
// trailing claim.OrganizationID stays at $4 so an SQL-binding regression
// that swaps the two args surfaces immediately.
func TestInsertWatchkeeper_WithRoleID_201_PersistsBind(t *testing.T) {
	const (
		// After M7.1.a the INSERT placeholders run $1..$5:
		//   $1 manifest_id, $2 lead_human_id, $3 active_mv_id,
		//   $4 claim.OrganizationID, $5 role_id.
		// We capture args[3] and args[4] verbatim so a regression
		// that reorders the SQL surfaces immediately.
		claimOrgArgIdx = 3
		roleIDArgIdx   = 4
	)
	var capturedClaimOrg any
	var capturedRoleID any
	var capturedSQL string
	queryRow := func(_ context.Context, sql string, args ...any) pgx.Row {
		capturedSQL = sql
		if len(args) > roleIDArgIdx {
			capturedClaimOrg = args[claimOrgArgIdx]
			capturedRoleID = args[roleIDArgIdx]
		}
		return server.NewFakeRow(func(dest ...any) error {
			if sp, ok := dest[0].(*string); ok {
				*sp = wkFakeID
			}
			return nil
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/watchkeepers", tok, map[string]any{
		"manifest_id":   wkManifestID,
		"lead_human_id": wkLeadHumanID,
		"role_id":       wkRoleID,
	}, "")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	// SQL must reference the role_id column at INSERT time so a
	// future refactor that drops the column from the statement (and
	// silently relies on the DB default) fails this assertion.
	if !strings.Contains(capturedSQL, "role_id") {
		t.Errorf("INSERT SQL missing role_id column; got: %s", capturedSQL)
	}
	if capturedRoleID != wkRoleID {
		t.Errorf("INSERT role_id bind = %v, want %q (stringOrNil must forward verbatim)", capturedRoleID, wkRoleID)
	}
	// claim.OrganizationID must remain at $4; a swap with role_id
	// would let any caller anchor a watchkeeper at another tenant's
	// human under a role of their choice.
	if got, ok := capturedClaimOrg.(string); !ok || got == "" {
		t.Errorf("claim.OrganizationID bind = %v, want non-empty string", capturedClaimOrg)
	}
}

// TestInsertWatchkeeper_OmitRoleID_BindsNullArg asserts the legacy path:
// a body that omits the `role_id` field binds SQL NULL at the $5 slot via
// [stringOrNil]. Pre-M7.1.a callers must continue to insert rows with a
// NULL role_id (the migration's partial index stays untouched for those
// rows so the M7.1.b predecessor-lookup query never sees them).
func TestInsertWatchkeeper_OmitRoleID_BindsNullArg(t *testing.T) {
	const roleIDArgIdx = 4
	var capturedRoleID any
	captured := false
	queryRow := func(_ context.Context, _ string, args ...any) pgx.Row {
		if len(args) > roleIDArgIdx {
			capturedRoleID = args[roleIDArgIdx]
			captured = true
		}
		return server.NewFakeRow(func(dest ...any) error {
			if sp, ok := dest[0].(*string); ok {
				*sp = wkFakeID
			}
			return nil
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/watchkeepers", tok, map[string]any{
		"manifest_id":   wkManifestID,
		"lead_human_id": wkLeadHumanID,
	}, "")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if !captured {
		t.Fatalf("queryRow not invoked — runner wiring broken")
	}
	// stringOrNil("") returns an untyped nil so the pgx driver binds
	// SQL NULL rather than an empty-string value that would fail the
	// future partial-index match shape.
	if capturedRoleID != nil {
		t.Errorf("role_id bind = %v, want nil (omitted → stringOrNil)", capturedRoleID)
	}
}

// TestInsertWatchkeeper_BlankRoleID_400 asserts the pre-tx non-blank gate:
// a whitespace-only `role_id` is a wiring bug (would round-trip to a
// non-NULL row that matches nothing in the M7.1.b predecessor-lookup
// query) and the parser rejects it with `invalid_request` BEFORE the row
// hits Postgres. Mirrors the M7.2.c `optionalArchiveURI` non-blank gate.
func TestInsertWatchkeeper_BlankRoleID_400(t *testing.T) {
	cases := []struct {
		name, role string
	}{
		{"single_space", " "},
		{"tab", "\t"},
		{"mixed_whitespace", " \t\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPost, "/v1/watchkeepers", tok, map[string]any{
				"manifest_id":   wkManifestID,
				"lead_human_id": wkLeadHumanID,
				"role_id":       tc.role,
			}, "")

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 for role=%q; body=%s", rec.Code, tc.role, rec.Body.String())
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
		})
	}
}

// -----------------------------------------------------------------------
// Get: GET /v1/watchkeepers/{id} — role_id projection.
// -----------------------------------------------------------------------

// TestGetWatchkeeper_RoleIDPresent_RoundTripsOnWire asserts the read-path
// projection: a non-NULL `role_id` column scans into the response JSON
// verbatim, preserving the M7.1.a column for downstream consumers
// (M7.1.b predecessor-lookup, M7.1.c NotebookInheritStep).
func TestGetWatchkeeper_RoleIDPresent_RoundTripsOnWire(t *testing.T) {
	spawnedAt := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	queryRow := func(_ context.Context, sql string, _ ...any) pgx.Row {
		// SELECT SQL must project role_id between archive_uri and
		// created_at; a regression that drops the column surfaces
		// here.
		if !strings.Contains(sql, "role_id") {
			t.Errorf("SELECT missing role_id column; got: %s", sql)
		}
		return server.NewFakeRow(func(dest ...any) error {
			// Order matches handleGetWatchkeeper's Scan list after M7.1.a:
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
			// archive_uri NULL on this active row.
			*dest[7].(**string) = nil
			// role_id non-NULL — the M7.1.a projection target.
			role := wkRoleID
			*dest[8].(**string) = &role
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
	if got["role_id"] != wkRoleID {
		t.Errorf("role_id = %v, want %q (full body=%s)", got["role_id"], wkRoleID, rec.Body.String())
	}
}

// TestGetWatchkeeper_RoleIDAbsent_WireCarriesNull asserts that a legacy
// row (column NULL in Postgres) projects as JSON `null` on the wire — the
// pointer-typed [watchkeeperRow.RoleID] field is intentionally NOT
// `omitempty` so a future consumer can distinguish "row exists with
// unknown role" from "row exists with NULL role" without parsing
// presence-bits.
func TestGetWatchkeeper_RoleIDAbsent_WireCarriesNull(t *testing.T) {
	spawnedAt := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	queryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRow(func(dest ...any) error {
			*dest[0].(*string) = wkFakeID
			*dest[1].(*string) = wkManifestID
			*dest[2].(*string) = wkLeadHumanID
			*dest[3].(**string) = nil
			*dest[4].(*string) = "active"
			*dest[5].(**time.Time) = &spawnedAt
			*dest[6].(**time.Time) = nil
			*dest[7].(**string) = nil
			// role_id NULL — legacy row.
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
	// Distinguish "key present with null" from "key absent" by
	// re-parsing into json.RawMessage so the encoder's null-vs-omit
	// behaviour is observable end-to-end.
	var got map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw, present := got["role_id"]
	if !present {
		t.Errorf("role_id key absent from response; want present-with-null (body=%s)", rec.Body.String())
	}
	if string(raw) != "null" {
		t.Errorf("role_id raw = %q, want \"null\" (body=%s)", string(raw), rec.Body.String())
	}
}
