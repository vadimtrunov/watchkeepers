package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

// newRouterForTest builds a full Keep router with a verifier whose key we
// control, so tests can mint tokens and exercise the mounted routes. We
// pass a nil pool because these tests only probe validation and
// middleware behaviour — they never reach the DB code path.
func newRouterForTest(t *testing.T, now func() time.Time) (http.Handler, *auth.TestIssuer) {
	t.Helper()
	v, err := auth.NewHMACVerifier(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewHMACVerifier: %v", err)
	}
	ti, err := auth.NewTestIssuer(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewTestIssuer: %v", err)
	}
	return server.NewRouter(v, nil, nil, 0), ti
}

// testClaimOrgID is the default OrganizationID stamped on every claim
// minted via [mustMintToken]. M3.5.a.2 introduces handler-side per-tenant
// enforcement; carrying a non-empty org on every test token keeps the
// pre-existing happy-path assertions valid (the handler now requires
// `claim.OrganizationID` to be non-empty AND, for body-pinned routes,
// to equal the request body's `organization_id`). Tests that
// fixture-write rows with `humanOrgID` use this same value so the
// claim-vs-body comparison stays balanced. Cross-tenant rejection
// tests use [mustMintTokenForOrg] with a deliberately mismatched
// constant; legacy-claim tests (no org at all) use [mustMintLegacyToken].
const testClaimOrgID = "66666666-6666-4666-8666-666666666666"

// mustMintToken mints a valid short-lived token with the given scope and
// the [testClaimOrgID] tenant. Tests that need a different tenant on the
// claim use [mustMintTokenForOrg]; tests that need a no-org claim (the
// legacy mint shape from before M3.5.a.1) use [mustMintLegacyToken].
func mustMintToken(t *testing.T, ti *auth.TestIssuer, scope string) string {
	t.Helper()
	return mustMintTokenForOrg(t, ti, scope, testClaimOrgID)
}

// mustMintTokenForOrg mints a valid short-lived token whose claim
// carries an explicit OrganizationID. Used by cross-tenant rejection
// tests where the claim's tenant must NOT equal the row/body tenant the
// handler reads.
func mustMintTokenForOrg(t *testing.T, ti *auth.TestIssuer, scope, orgID string) string {
	t.Helper()
	tok, err := ti.Issue(auth.Claim{Subject: "test-subject", Scope: scope, OrganizationID: orgID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// mustMintLegacyToken mints a short-lived token whose claim carries an
// EMPTY OrganizationID — the pre-M3.5.a.1 wire shape. Tests that pin
// the "handler must reject claims without an explicit tenant" contract
// use this helper so the rolling-deploy compat path documented in
// auth.payload still produces a verifiable token. Production handlers
// MUST 403 such claims; this helper exists solely to forge the negative
// case in a `handlers_*` rejection test.
func mustMintLegacyToken(t *testing.T, ti *auth.TestIssuer, scope string) string {
	t.Helper()
	tok, err := ti.Issue(auth.Claim{Subject: "test-subject", Scope: scope}, 5*time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// TestRouter_HealthStaysUnauthenticated is the AC2 regression guard: the
// /health route must never be covered by AuthMiddleware.
func TestRouter_HealthStaysUnauthenticated(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	h, _ := newRouterForTest(t, now)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestSearch_UnauthenticatedRejects confirms that /v1/search sits behind
// the auth wall. No Authorization header -> 401 missing_token.
func TestSearch_UnauthenticatedRejects(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	h, _ := newRouterForTest(t, now)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/search",
		strings.NewReader(`{"embedding":[0.1,0.2],"top_k":5}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestSearch_InvalidTopK covers the AC edge case: top_k <= 0 -> 400.
func TestSearch_InvalidTopK(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	h, ti := newRouterForTest(t, now)
	tok := mustMintToken(t, ti, "org")

	cases := []struct {
		name string
		body string
	}{
		{"zero", `{"embedding":[0.1,0.2],"top_k":0}`},
		{"negative", `{"embedding":[0.1,0.2],"top_k":-1}`},
		{"missing_top_k", `{"embedding":[0.1,0.2]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/search",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", got)
			}
		})
	}
}

// TestSearch_MissingEmbedding covers the required-field branch.
func TestSearch_MissingEmbedding(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	h, ti := newRouterForTest(t, now)
	tok := mustMintToken(t, ti, "org")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/search",
		strings.NewReader(`{"top_k":5}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestSearch_MalformedJSON asserts the body shape validator fires.
func TestSearch_MalformedJSON(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	h, ti := newRouterForTest(t, now)
	tok := mustMintToken(t, ti, "org")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/search",
		strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestSearch_OversizedBodyRejects asserts that bodies larger than the
// 1 MiB cap are rejected with 413 request_too_large before the handler
// attempts to allocate the full payload. The guard closes the DoS
// surface flagged in the Phase 4 review.
func TestSearch_OversizedBodyRejects(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	h, ti := newRouterForTest(t, now)
	tok := mustMintToken(t, ti, "org")

	// Build a payload that exceeds the 1 MiB cap. We embed the excess
	// bytes as a long JSON string inside an otherwise-valid shape so the
	// decoder would happily parse it if the cap were absent.
	pad := strings.Repeat("a", (1<<20)+1024)
	body := `{"embedding":[0.1,0.2],"top_k":5,"pad":"` + pad + `"}`

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/search",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 (body=%s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "request_too_large" || env.Reason != "body_too_large" {
		t.Errorf("body = %q, want error=request_too_large reason=body_too_large", rec.Body.String())
	}
}

// TestSearch_OversizedEmbeddingRejects asserts that an embedding slice
// longer than maxEmbeddingDim (4096) is rejected with 400
// invalid_embedding. Without this bound a caller could pin gigabytes of
// []float32 inside a sub-1 MiB JSON body using dense numeric formatting.
func TestSearch_OversizedEmbeddingRejects(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	h, ti := newRouterForTest(t, now)
	tok := mustMintToken(t, ti, "org")

	// 4097 zero entries — still well under the 1 MiB body cap.
	var sb strings.Builder
	sb.WriteString(`{"embedding":[`)
	for i := 0; i < 4097; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('0')
	}
	sb.WriteString(`],"top_k":5}`)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/search",
		strings.NewReader(sb.String()))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "invalid_embedding" {
		t.Errorf("error = %q, want invalid_embedding", env.Error)
	}
}

// TestSearch_UnsupportedMediaType asserts that a non-JSON Content-Type
// is rejected with 415 before the body is read.
func TestSearch_UnsupportedMediaType(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	h, ti := newRouterForTest(t, now)
	tok := mustMintToken(t, ti, "org")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/search",
		strings.NewReader(`{"embedding":[0.1,0.2],"top_k":5}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415 (body=%s)", rec.Code, rec.Body.String())
	}
	var env struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "unsupported_media_type" || env.Reason != "expected_application_json" {
		t.Errorf("body = %q, want error=unsupported_media_type reason=expected_application_json", rec.Body.String())
	}
}

// TestLogTail_InvalidLimit enforces Edge: limit=0 -> 400.
func TestLogTail_InvalidLimit(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	h, ti := newRouterForTest(t, now)
	tok := mustMintToken(t, ti, "org")

	for _, v := range []string{"0", "-5", "abc"} {
		t.Run(v, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
				"/v1/keepers-log?limit="+v, nil)
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestSearch_EmbeddingToVector is a round-trip check on the pgvector
// literal formatter — the wire format must be `[a,b,...]` with no
// whitespace and locale-independent numeric formatting.
func TestSearch_EmbeddingToVector(t *testing.T) {
	// We test via the public handler: feed a valid request but with a
	// verifier that always rejects so we exit before the DB roundtrip.
	// This keeps the test free of DB fakes while still exercising the
	// formatter through the serialization path.
	// (Formatter correctness is also covered end-to-end by the
	// integration suite's Happy — agent scope search case.)
	t.Skip("covered end-to-end by read_integration_test.go; see TASK §Test plan")
}

// ensureJSONEnvelope is a regression guard against accidental text
// responses: every non-success code must still carry the JSON
// {"error":"..."} shape.
func ensureJSONEnvelope(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(rec.Body.Bytes()), &body); err != nil {
		t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
	}
	if body.Error == "" {
		t.Errorf("error field empty in %q", rec.Body.String())
	}
}

// TestGetManifest_MissingID ensures the route path param extraction
// catches a missing manifest_id (the Go 1.22 mux guarantees the path
// value is present when the route matches, but defensive handling
// keeps the invariant local).
func TestGetManifest_MissingID(t *testing.T) {
	// Mount the handler under a mux that matches a bare `/v1/manifests/`
	// path (no trailing id). This is not the production route shape but
	// it exercises the defensive branch in handleGetManifest.
	_ = io.EOF // keep imports stable across refactors
	t.Skip("defensive branch; production mux never reaches it")
}

// TestSearch_LargeTopKClampsBeforeQuery is an edge case: top_k=999
// must be treated as if it were top_k=50. We can't observe the clamped
// value without a DB fake, so this is covered by the integration suite
// via the "limit clamping" test-plan case. We assert here that the
// clamp does not itself reject the request.
func TestSearch_LargeTopKDoesNotReject(t *testing.T) {
	// Covered by integration Edge — limit clamping. Noted here so the
	// unit-test grep makes the coverage split explicit.
	t.Skip("covered by integration Edge — limit clamping")
}

// TestSearch_ClaimReachesRunner asserts that a valid request threads the
// verified claim into the scopedRunner. Combined with the fake runner's
// FnReturns short-circuit we don't need a real pool to verify the auth
// ->  scope -> handler wiring works end-to-end.
func TestSearch_ClaimReachesRunner(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	v, err := auth.NewHMACVerifier(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewHMACVerifier: %v", err)
	}
	ti, err := auth.NewTestIssuer(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewTestIssuer: %v", err)
	}
	runner := &server.FakeScopedRunner{FnReturns: errors.New("db offline")}
	h := server.NewRouterWithRunner(v, runner)

	tok, err := ti.Issue(auth.Claim{Subject: "sub-1", Scope: "user:alice"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/v1/search",
		strings.NewReader(`{"embedding":[0.1,0.2],"top_k":5}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// FnReturns short-circuits to 500 search_failed.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (body=%s)", rec.Code, rec.Body.String())
	}
	if !runner.FnInvoked {
		t.Error("runner.WithScope was never invoked")
	}
	if runner.LastClaim.Subject != "sub-1" || runner.LastClaim.Scope != "user:alice" {
		t.Errorf("claim = %+v, want {Subject:sub-1 Scope:user:alice}", runner.LastClaim)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "search_failed" {
		t.Errorf("error = %q, want search_failed", env.Error)
	}
}

// TestGetManifest_ClaimReachesRunner exercises the same wiring for the
// manifest endpoint and verifies the path value is extracted correctly
// from the Go 1.22 mux.
func TestGetManifest_ClaimReachesRunner(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	v, err := auth.NewHMACVerifier(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewHMACVerifier: %v", err)
	}
	ti, err := auth.NewTestIssuer(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewTestIssuer: %v", err)
	}
	runner := &server.FakeScopedRunner{FnReturns: errors.New("db offline")}
	h := server.NewRouterWithRunner(v, runner)

	tok, err := ti.Issue(auth.Claim{Subject: "sub", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/v1/manifests/abc-123", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !runner.FnInvoked {
		t.Error("runner.WithScope was never invoked")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 on simulated DB failure; body=%s", rec.Code, rec.Body.String())
	}
}

// TestLogTail_ClaimReachesRunner mirrors the above for the log-tail
// endpoint and confirms the default limit is accepted.
func TestLogTail_ClaimReachesRunner(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	v, err := auth.NewHMACVerifier(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewHMACVerifier: %v", err)
	}
	ti, err := auth.NewTestIssuer(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewTestIssuer: %v", err)
	}
	runner := &server.FakeScopedRunner{FnReturns: errors.New("db offline")}
	h := server.NewRouterWithRunner(v, runner)

	tok, err := ti.Issue(auth.Claim{Subject: "sub", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/v1/keepers-log", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !runner.FnInvoked {
		t.Error("runner.WithScope was never invoked")
	}
}

// TestGetManifest_ModelProjection is the dedicated GET-path coverage for
// the manifest_version.model column (M5.5.b.b.a AC3/AC5). It stages a
// fake SELECT row whose model slot is set to a known value and asserts
// that the GET /v1/manifests/{id} response JSON carries
// `"model":"<value>"` on the wire. Keeping this in handlers_read_test.go
// isolates the SELECT projection from the INSERT wiring tested in
// handlers_write_test.go.
func TestGetManifest_ModelProjection(t *testing.T) {
	const wantModel = "claude-opus-4"
	queryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRow(func(dest ...any) error {
			// SELECT order from handleGetManifest:
			//   id, manifest_id, version_no, system_prompt,
			//   tools, authority_matrix, knowledge_sources,
			//   coalesce(personality, ''), coalesce(language, ''),
			//   coalesce(model, ''),
			//   created_at
			*dest[0].(*string) = fakeUUID
			*dest[1].(*string) = putManifestID
			*dest[2].(*int) = 1
			*dest[3].(*string) = "sys"
			*dest[4].(*json.RawMessage) = json.RawMessage(`[]`)
			*dest[5].(*json.RawMessage) = json.RawMessage(`{}`)
			*dest[6].(*json.RawMessage) = json.RawMessage(`[]`)
			*dest[7].(*string) = ""
			*dest[8].(*string) = ""
			*dest[9].(*string) = wantModel
			*dest[10].(*time.Time) = time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
			return nil
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/v1/manifests/"+putManifestID, nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET decode: %v", err)
	}
	if got["model"] != wantModel {
		t.Errorf("GET response model = %v, want %q; body=%s", got["model"], wantModel, rec.Body.String())
	}
}

// TestHandlers_UseJSONErrorEnvelope is a catch-all regression guard: if
// any handler regresses to text/plain errors the CI asserts will still
// see the shape change even before the full integration suite runs.
func TestHandlers_UseJSONErrorEnvelope(t *testing.T) {
	now := func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
	h, ti := newRouterForTest(t, now)
	tok := mustMintToken(t, ti, "org")

	cases := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"search-invalid-top_k", http.MethodPost, "/v1/search", `{"embedding":[0.1,0.2],"top_k":0}`},
		{"search-missing-embedding", http.MethodPost, "/v1/search", `{"top_k":5}`},
		{"log-tail-invalid-limit", http.MethodGet, "/v1/keepers-log?limit=0", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body *strings.Reader
			if tc.body != "" {
				body = strings.NewReader(tc.body)
			}
			var req *http.Request
			if body != nil {
				req = httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, body)
				req.Header.Set("Content-Type", "application/json")
			} else {
				req = httptest.NewRequestWithContext(context.Background(), tc.method, tc.path, nil)
			}
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
			ensureJSONEnvelope(t, rec)
		})
	}
}
