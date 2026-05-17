package server_test

// handlers_peers_test.go covers `GET /v1/peers` — the M1.2 list-peers
// endpoint. Tests reuse the same FakeScopedRunner / FakeTx seams as the
// rest of the server tests; no real pgx pool is opened.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

const (
	peerWKAID        = "11111111-1111-4111-8111-111111111111"
	peerWKBID        = "22222222-2222-4222-8222-222222222222"
	peerWKCID        = "33333333-3333-4333-8333-333333333333"
	peerToolsCoord   = `[{"name":"update_ticket_field"}]`
	peerToolsTwo     = `[{"name":"github.fetch_pr"},{"name":"github.post_review_comment"}]`
	peerToolsBad     = `not json`
	peerToolsNonArr  = `{"name":"oops"}`
	peerToolsBlankN  = `[{"name":""},{"name":"good"}]`
	peerToolsNoName  = `[{"description":"x"}]`
	peerRolePersona  = "Tactical project coordinator: short clarifying questions over wrong actions."
	peerRoleLangEN   = "en"
	peerRoleNameCoor = "Coordinator"
	peerRoleNameRev  = "Reviewer"
)

// makePeerScans builds row-scan closures matching handleListPeers' five-
// column Scan signature: (id, role display_name, description/personality,
// language, tools jsonb). Tests pass per-row inputs that the closure
// transcribes verbatim into the Scan dest pointers.
type peerRowInput struct {
	id, role, description, language, tools string
}

func makePeerScans(t *testing.T, rows []peerRowInput) []func(dest ...any) error {
	t.Helper()
	out := make([]func(dest ...any) error, 0, len(rows))
	for _, r := range rows {
		r := r
		out = append(out, func(dest ...any) error {
			*dest[0].(*string) = r.id
			*dest[1].(*string) = r.role
			*dest[2].(*string) = r.description
			*dest[3].(*string) = r.language
			*dest[4].(*[]byte) = []byte(r.tools)
			return nil
		})
	}
	return out
}

// -----------------------------------------------------------------------
// Happy path
// -----------------------------------------------------------------------

// TestListPeers_Happy stages three active rows + asserts the wire shape
// exposes role / description / language / capabilities / availability for
// every row, that capabilities decodes the tools jsonb tool-name list,
// and that the response carries the items envelope with next_cursor=null.
func TestListPeers_Happy(t *testing.T) {
	rows := []peerRowInput{
		{peerWKAID, peerRoleNameCoor, peerRolePersona, peerRoleLangEN, peerToolsCoord},
		{peerWKBID, peerRoleNameRev, "Diligent PR reviewer", "en-US", peerToolsTwo},
	}
	query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		return server.NewFakeRows(makePeerScans(t, rows), nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Items      []map[string]any `json:"items"`
		NextCursor *string          `json:"next_cursor"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.NextCursor != nil {
		t.Errorf("next_cursor = %v, want nil", got.NextCursor)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items len = %d, want 2; body=%s", len(got.Items), rec.Body.String())
	}
	first := got.Items[0]
	if first["watchkeeper_id"] != peerWKAID {
		t.Errorf("items[0].watchkeeper_id = %v, want %q", first["watchkeeper_id"], peerWKAID)
	}
	if first["role"] != peerRoleNameCoor {
		t.Errorf("items[0].role = %v, want %q", first["role"], peerRoleNameCoor)
	}
	if first["description"] != peerRolePersona {
		t.Errorf("items[0].description = %v, want %q", first["description"], peerRolePersona)
	}
	if first["language"] != peerRoleLangEN {
		t.Errorf("items[0].language = %v, want %q", first["language"], peerRoleLangEN)
	}
	if first["availability"] != "available" {
		t.Errorf("items[0].availability = %v, want \"available\"", first["availability"])
	}
	caps, _ := first["capabilities"].([]any)
	if len(caps) != 1 || caps[0] != "update_ticket_field" {
		t.Errorf("items[0].capabilities = %v, want [update_ticket_field]", caps)
	}
	second := got.Items[1]
	caps2, _ := second["capabilities"].([]any)
	if len(caps2) != 2 || caps2[0] != "github.fetch_pr" || caps2[1] != "github.post_review_comment" {
		t.Errorf("items[1].capabilities = %v, want [github.fetch_pr github.post_review_comment]", caps2)
	}
}

// TestListPeers_EmptyItemsArrayNotNull pins the wire shape: an empty
// active-watchkeeper set returns `"items":[]` (not `"items":null`) so
// downstream consumers (M1.3.d's RoleFilter resolver) can range over the
// slice without a nil guard.
func TestListPeers_EmptyItemsArrayNotNull(t *testing.T) {
	query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		return server.NewFakeRows(nil, nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Errorf("response missing literal \"items\":[]; body=%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"items":null`) {
		t.Errorf("response contained \"items\":null; body=%s", rec.Body.String())
	}
}

// TestListPeers_EmptyCapabilitiesEmittedAsArray pins the per-row
// capabilities shape: a row whose `tools` is the empty jsonb array
// serialises `"capabilities":[]`, not `"capabilities":null`. M1.3.d
// ranges over the slice; a null would break the filter loop.
func TestListPeers_EmptyCapabilitiesEmittedAsArray(t *testing.T) {
	rows := []peerRowInput{
		{peerWKAID, peerRoleNameCoor, "", "", "[]"},
	}
	query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		return server.NewFakeRows(makePeerScans(t, rows), nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"capabilities":[]`) {
		t.Errorf("response missing \"capabilities\":[]; body=%s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"capabilities":null`) {
		t.Errorf("response contained \"capabilities\":null; body=%s", rec.Body.String())
	}
}

// TestListPeers_MalformedToolsYieldsEmptyCapabilities pins the defensive
// parse: a row whose `tools` is malformed JSON (manifest authoring bug)
// degrades to `capabilities:[]` rather than 500ing the whole endpoint,
// so the rest of the active set stays visible.
func TestListPeers_MalformedToolsYieldsEmptyCapabilities(t *testing.T) {
	cases := []struct {
		name, tools string
	}{
		{"invalid_json", peerToolsBad},
		{"non_array_top_level", peerToolsNonArr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := []peerRowInput{
				{peerWKAID, peerRoleNameCoor, "", "", tc.tools},
			}
			query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
				return server.NewFakeRows(makePeerScans(t, rows), nil), nil
			}
			runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), `"capabilities":[]`) {
				t.Errorf("malformed tools did not degrade to empty capabilities; body=%s", rec.Body.String())
			}
		})
	}
}

// TestListPeers_SkipsBlankAndMissingToolNames pins the defensive parse
// for individual entries: a tool object whose `name` is missing or
// blank is skipped, NOT appended as "". This keeps M1.3.d's
// capability filter free of spurious empty-string matches.
func TestListPeers_SkipsBlankAndMissingToolNames(t *testing.T) {
	cases := []struct {
		name, tools, wantCap string
	}{
		{"blank_name", peerToolsBlankN, "good"},
		{"missing_name", peerToolsNoName, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rows := []peerRowInput{
				{peerWKAID, peerRoleNameCoor, "", "", tc.tools},
			}
			query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
				return server.NewFakeRows(makePeerScans(t, rows), nil), nil
			}
			runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
			}
			var got struct {
				Items []map[string]any `json:"items"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			caps, _ := got.Items[0]["capabilities"].([]any)
			if tc.wantCap == "" {
				if len(caps) != 0 {
					t.Errorf("expected empty capabilities, got %v", caps)
				}
				return
			}
			if len(caps) != 1 || caps[0] != tc.wantCap {
				t.Errorf("capabilities = %v, want [%q]", caps, tc.wantCap)
			}
		})
	}
}

// -----------------------------------------------------------------------
// SQL contract
// -----------------------------------------------------------------------

// TestListPeers_SQLFiltersActiveAndJoinsManifest pins the load-bearing
// shape of the SQL the handler issues: WHERE wk.status = 'active', an
// INNER join against manifest_version on active_manifest_version_id
// (which skips spawn-in-progress rows whose pin is still NULL), and an
// INNER join against manifest for display_name. Without this assertion
// a future refactor that switched to a LEFT join would silently
// surface unpinned rows + null role names.
func TestListPeers_SQLFiltersActiveAndJoinsManifest(t *testing.T) {
	var gotSQL string
	query := func(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
		gotSQL = sql
		return server.NewFakeRows(nil, nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotSQL, "wk.status = 'active'") {
		t.Errorf("SQL missing status='active' filter; got: %s", gotSQL)
	}
	if !strings.Contains(gotSQL, "JOIN watchkeeper.manifest_version mv") {
		t.Errorf("SQL missing manifest_version join; got: %s", gotSQL)
	}
	if !strings.Contains(gotSQL, "mv.id = wk.active_manifest_version_id") {
		t.Errorf("SQL missing INNER join condition on active_manifest_version_id; got: %s", gotSQL)
	}
	if !strings.Contains(gotSQL, "JOIN watchkeeper.manifest m") {
		t.Errorf("SQL missing manifest join; got: %s", gotSQL)
	}
	// iter-1 codex P2 fix: the manifest join MUST follow
	// `mv.manifest_id`, not `wk.manifest_id`. Migration 002's
	// invariant (active_manifest_version_id's manifest_id matches
	// watchkeeper.manifest_id) is not SQL-enforced; routing through
	// `mv.manifest_id` keeps every projected row internally
	// consistent across the role / description / language /
	// capabilities tuple even when a malformed row lands.
	if !strings.Contains(gotSQL, "m.id = mv.manifest_id") {
		t.Errorf("SQL must join manifest via mv.manifest_id (iter-1 P2 fix), not wk.manifest_id; got: %s", gotSQL)
	}
	if strings.Contains(gotSQL, "m.id = wk.manifest_id") {
		t.Errorf("SQL joins manifest via wk.manifest_id — iter-1 codex P2 fix requires routing through mv.manifest_id so divergent-manifest rows do not mix role with description/language/capabilities; got: %s", gotSQL)
	}
	if !strings.Contains(gotSQL, "ORDER BY wk.created_at DESC") {
		t.Errorf("SQL missing deterministic ORDER BY; got: %s", gotSQL)
	}
	if strings.Contains(gotSQL, "LEFT JOIN") || strings.Contains(gotSQL, "LEFT OUTER JOIN") {
		t.Errorf("SQL used LEFT join — M1.2 contract requires INNER joins to skip unpinned rows; got: %s", gotSQL)
	}
}

// TestListPeers_DefaultLimit asserts the handler binds the default
// LIMIT (50) when ?limit is omitted.
func TestListPeers_DefaultLimit(t *testing.T) {
	var gotArgs []any
	query := func(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
		gotArgs = args
		return server.NewFakeRows(nil, nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")
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

// TestListPeers_ExplicitLimit asserts the handler binds a caller-
// supplied ?limit verbatim.
func TestListPeers_ExplicitLimit(t *testing.T) {
	var gotArgs []any
	query := func(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
		gotArgs = args
		return server.NewFakeRows(nil, nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers?limit=25", tok, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if len(gotArgs) < 1 {
		t.Fatalf("no SQL args bound")
	}
	limit, ok := gotArgs[0].(int)
	if !ok || limit != 25 {
		t.Errorf("LIMIT = %v (%T), want 25 (int)", gotArgs[0], gotArgs[0])
	}
}

// TestListPeers_MaxLimitAccepted pins the upper bound: limit=200 is
// accepted (the cap is inclusive).
func TestListPeers_MaxLimitAccepted(t *testing.T) {
	var gotArgs []any
	query := func(_ context.Context, _ string, args ...any) (pgx.Rows, error) {
		gotArgs = args
		return server.NewFakeRows(nil, nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers?limit=200", tok, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if limit, _ := gotArgs[0].(int); limit != 200 {
		t.Errorf("LIMIT = %v, want 200", gotArgs[0])
	}
}

// -----------------------------------------------------------------------
// Validation
// -----------------------------------------------------------------------

// TestListPeers_LimitOutOfRange_400 pins the limit validator: zero,
// negative, oversize, and non-numeric values reject pre-tx.
func TestListPeers_LimitOutOfRange_400(t *testing.T) {
	cases := []struct {
		name, limit string
	}{
		{"zero", "0"},
		{"negative", "-1"},
		{"too_high", "201"},
		{"way_too_high", "99999"},
		{"non_numeric", "abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodGet, "/v1/peers?limit="+tc.limit, tok, nil, "")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if runner.FnInvoked {
				t.Error("runner invoked; expected rejection before tx")
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
// Auth + runner-error
// -----------------------------------------------------------------------

// TestListPeers_NoToken_401 — request without bearer token is rejected
// at the auth wall, never reaches the runner.
func TestListPeers_NoToken_401(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, _ := writeRouterForTest(t, mustFixedNow(), runner)

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", "", nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked; expected rejection at auth wall")
	}
}

// TestListPeers_RunnerErrorBubblesUp — a non-pgx runner error surfaces
// as 500 list_peers_failed; the raw error text never leaks to the wire.
func TestListPeers_RunnerErrorBubblesUp(t *testing.T) {
	runner := &server.FakeScopedRunner{FnReturns: errors.New("database unreachable")}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "list_peers_failed" {
		t.Errorf("error = %q, want list_peers_failed", env.Error)
	}
	if strings.Contains(rec.Body.String(), "database unreachable") {
		t.Errorf("raw runner error leaked: %s", rec.Body.String())
	}
}

// TestListPeers_RunnerSeesClaim — the runner observes the verified
// claim so org-scoped reads (and any future per-org RLS extension on
// the watchkeeper table) get the correct GUC.
func TestListPeers_RunnerSeesClaim(t *testing.T) {
	query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		return server.NewFakeRows(nil, nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !runner.FnInvoked {
		t.Error("WithScope not invoked")
	}
	if runner.LastClaim.Scope != "org" {
		t.Errorf("claim.Scope = %q, want org", runner.LastClaim.Scope)
	}
}

// TestListPeers_QueryError_Returns500 — a pgx Query failure (network /
// catalog lookup) surfaces as 500 list_peers_failed.
func TestListPeers_QueryError_Returns500(t *testing.T) {
	query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		return nil, errors.New("connection reset")
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "connection reset") {
		t.Errorf("raw pgx error leaked: %s", rec.Body.String())
	}
}

// TestListPeers_ScanError_Returns500 — a per-row Scan error (driver
// type mismatch) aborts the iteration with 500 list_peers_failed.
func TestListPeers_ScanError_Returns500(t *testing.T) {
	scans := []func(dest ...any) error{
		func(_ ...any) error { return errors.New("scan failed: invalid byte sequence") },
	}
	query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		return server.NewFakeRows(scans, nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "scan failed") {
		t.Errorf("raw scan error leaked: %s", rec.Body.String())
	}
}

// TestListPeers_RowsErr_Returns500 — a deferred rows.Err() failure
// (after Next returned false) propagates as 500. The handler must
// drain rows.Err() even on a successful iteration loop.
func TestListPeers_RowsErr_Returns500(t *testing.T) {
	query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		return server.NewFakeRows(nil, errors.New("trailing rows.Err")), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}
}

// TestListPeers_OrderedDeterministic — the handler returns rows in the
// order the underlying Query yields them (DESC by created_at,
// enforced SQL-side). The fake here does not sort; the test pins the
// pass-through guarantee so a future refactor cannot re-order rows in
// Go without breaking this assertion.
func TestListPeers_OrderedDeterministic(t *testing.T) {
	rows := []peerRowInput{
		{peerWKAID, "RoleA", "descA", "en", `[]`},
		{peerWKBID, "RoleB", "descB", "en", `[]`},
		{peerWKCID, "RoleC", "descC", "en", `[]`},
	}
	query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		return server.NewFakeRows(makePeerScans(t, rows), nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")
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
		t.Fatalf("items len = %d, want 3", len(got.Items))
	}
	wantIDs := []string{peerWKAID, peerWKBID, peerWKCID}
	for i, w := range wantIDs {
		if got.Items[i]["watchkeeper_id"] != w {
			t.Errorf("items[%d].watchkeeper_id = %v, want %q", i, got.Items[i]["watchkeeper_id"], w)
		}
	}
}

// -----------------------------------------------------------------------
// Source-grep AC (audit ban)
// -----------------------------------------------------------------------

// TestHandleListPeers_NoAuditOrKeeperslogReferences pins the M1 source-
// grep AC: the read handler must NOT emit audit events or call
// `keeperslog.Append` — M1.4 owns the K2K event taxonomy. A future
// contributor adding audit emission inside the read path trips this
// test before the call-site change can ride out of review. Mirrors
// M1.1.b's `TestChannels_NoAuditOrKeeperslogReferences` and M1.1.c's
// `TestLifecycle_NoAuditOrKeeperslogReferences`.
func TestHandleListPeers_NoAuditOrKeeperslogReferences(t *testing.T) {
	raw, err := os.ReadFile("handlers_peers.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	src := stripLineComments(string(raw))
	if strings.Contains(src, "keeperslog.") {
		t.Error("handlers_peers.go references `keeperslog.` outside comments; audit emission belongs to M1.4")
	}
	// `.Append(` is the audit-sink call shape. The handler must not
	// invoke it directly; the only Append-like call permitted is the
	// Go-stdlib append builtin which lowercases the verb.
	appendCall := regexp.MustCompile(`\bAppend\s*\(`)
	if appendCall.MatchString(src) {
		t.Error("handlers_peers.go calls `.Append(` outside comments; audit emission belongs to M1.4")
	}
}

// stripLineComments removes `// …` line comments so the source-grep AC
// does not false-positive on a docblock that mentions `keeperslog.`
// for documentation purposes. Block comments (`/* … */`) are not used
// in handlers_peers.go; the simpler line-comment strip is sufficient.
func stripLineComments(src string) string {
	var out strings.Builder
	out.Grow(len(src))
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}

// -----------------------------------------------------------------------
// Header content-type
// -----------------------------------------------------------------------

// TestListPeers_ResponseContentTypeJSON pins the Content-Type so a
// future refactor that switched to a raw byte writer cannot silently
// regress.
func TestListPeers_ResponseContentTypeJSON(t *testing.T) {
	query := func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
		return server.NewFakeRows(nil, nil), nil
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{Query: query})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodGet, "/v1/peers", tok, nil, "")
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}
