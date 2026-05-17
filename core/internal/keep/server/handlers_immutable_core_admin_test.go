package server_test

// handlers_immutable_core_admin_test.go — Phase 2 §M3.2 admin-only
// editability enforcement for the manifest_version.immutable_core
// column. The handler MUST reject any non-root manifest_version PUT
// whose `immutable_core` differs from the parent row's
// `immutable_core` — the "admin-only at spawn, direct DB edit
// otherwise" contract from `docs/ROADMAP-phase2.md` §M3.2.
//
// Sibling test file (mirrors the M3.3 pattern of splitting a new
// column-family's tests out of the already-large
// handlers_write_test.go). Reuses the package-level helpers
// `mustMintToken`, `writeRouterForTest`, `writeDo`, `fakeUUID`, and
// `putManifestID`. Each test stages its own [server.FakeTxFns.QueryRow]
// so it can observe the parity SELECT and the INSERT independently —
// the gate fires inside the scoped tx between the two.
//
// Coverage matrix (the four MUST-pin cases the rdd brief calls out):
//
//   - admin spawn: VersionNo=1 with arbitrary immutable_core → 201
//   - modification: VersionNo>1 + differing immutable_core → 403
//     `immutable_core_modified`
//   - parity: VersionNo>1 + byte-identical immutable_core → 201
//   - org isolation: cross-tenant manifest_id → 404 not_found
//
// Plus the orthogonal corner cases:
//
//   - NULL parent + NULL candidate → parity match → 201
//   - NULL parent + non-NULL candidate → modification → 403
//   - non-NULL parent + NULL candidate → modification → 403
//   - structural-equal key reorder → parity match → 201

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

// pmvParityHarness wires a QueryRow closure that stages a fake parity
// SELECT result first (only when `skipParity` is false), then a fake
// INSERT row. The boolean `parentDistinct` value the parity SELECT
// returns drives the gate: true → 403 immutable_core_modified,
// false → INSERT proceeds → 201.
//
// `parentNotFound` simulates the cross-tenant case (the JOIN filter
// `m.organization_id = $claim_org` drops the row → pgx.ErrNoRows).
// `skipParity=true` MUST be set on VersionNo=1 tests so the first
// QueryRow lands on the INSERT branch (the handler skips the gate
// when there is no parent row to compare against).
// `insertSeen` lets each test assert whether the INSERT path actually
// ran — a 403 must short-circuit before the INSERT.
type pmvParityHarness struct {
	parentDistinct bool
	parentNotFound bool
	skipParity     bool
	insertID       string
	calls          int
	insertSeen     bool
}

// queryRow is the closure plugged into [server.FakeTxFns.QueryRow]. It
// dispatches by call ordinal: when `skipParity` is false the first
// call is the parity SELECT and the second is the INSERT; when
// `skipParity` is true every call is treated as the INSERT (matches
// the handler's VersionNo=1 fast path).
func (h *pmvParityHarness) queryRow(_ context.Context, _ string, _ ...any) pgx.Row {
	h.calls++
	if !h.skipParity && h.calls == 1 {
		if h.parentNotFound {
			return server.NewFakeRowErr(pgx.ErrNoRows)
		}
		return server.NewFakeRow(func(dest ...any) error {
			if bp, ok := dest[0].(*bool); ok {
				*bp = h.parentDistinct
			}
			return nil
		})
	}
	h.insertSeen = true
	return server.NewFakeRow(func(dest ...any) error {
		if sp, ok := dest[0].(*string); ok {
			*sp = h.insertID
		}
		return nil
	})
}

// runParityPUT mints a router whose runner stages `h`'s parity+INSERT
// behaviour and sends a PUT body. Returns the standard recorder so each
// test can assert status + body via the existing testing convention.
func runParityPUT(t *testing.T, h *pmvParityHarness, body string) *httptest.ResponseRecorder {
	t.Helper()
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: h.queryRow})}
	router, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")
	return writeDo(t, router, http.MethodPut,
		"/v1/manifests/"+putManifestID+"/versions", tok, nil, body)
}

// wantErrorCode decodes the standard `{"error":"<code>"}` envelope and
// asserts the code matches.
func wantErrorCode(t *testing.T, rec *httptest.ResponseRecorder, want string) {
	t.Helper()
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v; body=%s", err, rec.Body.String())
	}
	if env.Error != want {
		t.Errorf("error = %q, want %q; body=%s", env.Error, want, rec.Body.String())
	}
}

// -----------------------------------------------------------------------
// Admin spawn path — VersionNo=1 (root) bypasses the parity gate
// -----------------------------------------------------------------------

// TestPutManifestVersion_AdminSpawn_VersionNo1_201 asserts that a root
// version PUT (`version_no:1`) carrying ANY `immutable_core` payload
// flows through the INSERT without hitting the parity gate. Mechanical
// proof: the harness stages a single QueryRow (the INSERT) and asserts
// the parity-SELECT was not invoked. Mirrors the spec's "admin sets at
// spawn" path — the handler MUST NOT consult a parent row when none
// exists by construction.
func TestPutManifestVersion_AdminSpawn_VersionNo1_201(t *testing.T) {
	h := &pmvParityHarness{skipParity: true, insertID: fakeUUID}
	const body = `{"version_no":1,"system_prompt":"ok",` +
		`"immutable_core":{"role_boundaries":["delete_prod"],` +
		`"cost_limits":{"per_task_tokens":50000}}}`
	rec := runParityPUT(t, h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if h.calls != 1 {
		t.Errorf("QueryRow calls = %d, want 1 (parity SELECT must NOT run on VersionNo=1); body=%s", h.calls, rec.Body.String())
	}
	if !h.insertSeen {
		t.Errorf("INSERT QueryRow not invoked on root-version PUT; body=%s", rec.Body.String())
	}
}

// -----------------------------------------------------------------------
// Non-admin modification — VersionNo>1 with differing immutable_core
// -----------------------------------------------------------------------

// TestPutManifestVersion_NonRoot_ImmutableCoreModified_403 asserts that
// a VersionNo>1 PUT whose `immutable_core` differs from the parent row
// is rejected with stable 403 reason `immutable_core_modified` BEFORE
// the INSERT runs. The harness stages a parity SELECT that returns
// `blocked=true`; the assertion is on (a) the 403 status, (b) the
// stable error code, and (c) the absence of an INSERT call (a 403 must
// not stomp the row).
func TestPutManifestVersion_NonRoot_ImmutableCoreModified_403(t *testing.T) {
	h := &pmvParityHarness{parentDistinct: true, insertID: fakeUUID}
	const body = `{"version_no":2,"system_prompt":"ok",` +
		`"immutable_core":{"role_boundaries":["NEW_CAPABILITY"]}}`
	rec := runParityPUT(t, h, body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	wantErrorCode(t, rec, "immutable_core_modified")
	if h.insertSeen {
		t.Errorf("INSERT QueryRow invoked despite 403 immutable_core_modified; the gate must short-circuit before the INSERT")
	}
}

// -----------------------------------------------------------------------
// Parity match — VersionNo>1 with byte-identical immutable_core
// -----------------------------------------------------------------------

// TestPutManifestVersion_NonRoot_ImmutableCoreParity_201 asserts that a
// VersionNo>1 PUT whose `immutable_core` matches the parent row's
// (harness reports `blocked=false`) flows through the INSERT and
// returns 201. This is the legitimate self-tune path (personality /
// language / model / autonomy / notebook recall edits via
// [copyManifestForBump]) — the gate MUST NOT block these.
func TestPutManifestVersion_NonRoot_ImmutableCoreParity_201(t *testing.T) {
	h := &pmvParityHarness{insertID: fakeUUID}
	const body = `{"version_no":2,"system_prompt":"ok",` +
		`"immutable_core":{"role_boundaries":["delete_prod"]}}`
	rec := runParityPUT(t, h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if !h.insertSeen {
		t.Errorf("INSERT QueryRow not invoked on parity-match PUT; body=%s", rec.Body.String())
	}
}

// TestPutManifestVersion_NonRoot_BothNullImmutableCore_201 asserts that
// VersionNo>1 + a body that omits `immutable_core` against a parent
// row that also has SQL-NULL `immutable_core` passes the gate
// (`IS DISTINCT FROM` on (NULL, NULL) returns false). This is the
// legacy-row path — a manifest that predates Phase 2 §M3.1 carries
// NULL `immutable_core` and the self-tune CLI omits the field;
// both sides line up via the same NULL.
func TestPutManifestVersion_NonRoot_BothNullImmutableCore_201(t *testing.T) {
	h := &pmvParityHarness{insertID: fakeUUID}
	const body = `{"version_no":2,"system_prompt":"ok"}`
	rec := runParityPUT(t, h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// TestPutManifestVersion_NonRoot_NullParentSetCandidate_403 asserts the
// asymmetric case: parent row carries SQL-NULL `immutable_core` and
// the candidate sets a non-NULL value. The gate trips with 403
// `immutable_core_modified` — promoting NULL to a value IS a
// modification under the admin-only contract. (The legitimate path
// for first-time immutable_core declaration is direct DB UPDATE +
// core restart, not a Watchmaster bump.)
func TestPutManifestVersion_NonRoot_NullParentSetCandidate_403(t *testing.T) {
	h := &pmvParityHarness{parentDistinct: true, insertID: fakeUUID}
	const body = `{"version_no":2,"system_prompt":"ok",` +
		`"immutable_core":{"role_boundaries":["delete_prod"]}}`
	rec := runParityPUT(t, h, body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	wantErrorCode(t, rec, "immutable_core_modified")
}

// TestPutManifestVersion_NonRoot_SetParentNullCandidate_403 asserts the
// reverse asymmetry: parent row carries a non-NULL `immutable_core`
// object and the candidate omits the field (would round-trip as SQL
// NULL via [jsonbOrNil]). Demoting a declared governance object to
// NULL on a bump is a modification — gate trips with 403.
func TestPutManifestVersion_NonRoot_SetParentNullCandidate_403(t *testing.T) {
	h := &pmvParityHarness{parentDistinct: true, insertID: fakeUUID}
	const body = `{"version_no":2,"system_prompt":"ok"}`
	rec := runParityPUT(t, h, body)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body=%s", rec.Code, rec.Body.String())
	}
	wantErrorCode(t, rec, "immutable_core_modified")
}

// TestPutManifestVersion_NonRoot_KeyReorderStructuralMatch_201 asserts
// that a candidate `immutable_core` carrying the same buckets in a
// different key order than the parent does NOT trip the gate. The
// harness reports `blocked=false` because the production SQL uses
// `jsonb IS DISTINCT FROM jsonb` (structural compare, not text
// compare). Documents the pattern: callers (and the M3.4 rollback
// tool) MAY round-trip immutable_core through any JSON-emit path
// without anxiety about key-order regressions.
func TestPutManifestVersion_NonRoot_KeyReorderStructuralMatch_201(t *testing.T) {
	h := &pmvParityHarness{insertID: fakeUUID}
	const body = `{"version_no":2,"system_prompt":"ok",` +
		`"immutable_core":{"cost_limits":{"per_task_tokens":50000},` +
		`"role_boundaries":["delete_prod"]}}`
	rec := runParityPUT(t, h, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// -----------------------------------------------------------------------
// Cross-tenant / org isolation
// -----------------------------------------------------------------------

// TestPutManifestVersion_NonRoot_CrossTenantManifest_404 asserts that a
// VersionNo>1 PUT whose `manifest_id` belongs to a different tenant
// gets `pgx.ErrNoRows` on the parity SELECT (the JOIN filter
// `m.organization_id = $claim_org` drops the row) and surfaces as
// 404 not_found through the same error block the INSERT path uses for
// cross-tenant rejection. The gate does NOT leak existence of the
// foreign-tenant manifest — the 404 is identical to the
// "manifest_id does not exist anywhere" response.
func TestPutManifestVersion_NonRoot_CrossTenantManifest_404(t *testing.T) {
	h := &pmvParityHarness{parentNotFound: true, insertID: fakeUUID}
	const body = `{"version_no":2,"system_prompt":"ok",` +
		`"immutable_core":{"role_boundaries":["delete_prod"]}}`
	rec := runParityPUT(t, h, body)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	wantErrorCode(t, rec, "not_found")
	if h.insertSeen {
		t.Errorf("INSERT QueryRow invoked after cross-tenant parity-SELECT pgx.ErrNoRows; the handler must surface 404 from the gate path")
	}
}
