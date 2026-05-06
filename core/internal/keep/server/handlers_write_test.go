package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

// makeVec1536 returns a JSON array literal of exactly 1536 zero-valued floats,
// suitable for use as a raw body fragment in write-handler unit tests that
// must satisfy the knowledgeChunkEmbeddingDim == 1536 constraint.
func makeVec1536() string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i := 0; i < 1536; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteByte('0')
	}
	sb.WriteByte(']')
	return sb.String()
}

// writeRouterForTest builds a full Keep router whose /v1/* routes (read
// and write) run against a *FakeScopedRunner we control. The returned
// issuer shares its signing key with the verifier so the test can mint
// valid tokens for every scope.
func writeRouterForTest(t *testing.T, now func() time.Time, runner *server.FakeScopedRunner) (http.Handler, *auth.TestIssuer) {
	t.Helper()
	v, err := auth.NewHMACVerifier(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewHMACVerifier: %v", err)
	}
	ti, err := auth.NewTestIssuer(mwSigningKey, "keep-test", now)
	if err != nil {
		t.Fatalf("NewTestIssuer: %v", err)
	}
	return server.NewRouterWithRunner(v, runner), ti
}

// writeDo issues an authed HTTP request against the wired handler. The
// body is marshalled as JSON; nil body sends an empty body.
func writeDo(t *testing.T, h http.Handler, method, path, tok string, body any, rawBody string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	switch {
	case rawBody != "":
		reader = strings.NewReader(rawBody)
	case body != nil:
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = strings.NewReader(string(raw))
	default:
		reader = strings.NewReader("")
	}
	req := httptest.NewRequestWithContext(context.Background(), method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// mustFixedNow returns a deterministic clock for tests.
func mustFixedNow() func() time.Time {
	return func() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }
}

// -----------------------------------------------------------------------
// Happy path
// -----------------------------------------------------------------------

// TestStore_Happy asserts POST /v1/knowledge-chunks reaches the runner
// with claim.Scope bound, and returns 201 + id. The fake runner supplies
// FnReturns=nil so the handler's RETURNING Scan path is exercised via the
// runner seam (tx is nil, so we route Fn through a short-circuit below).
func TestStore_Happy(t *testing.T) {
	runner := &server.FakeScopedRunner{FnReturns: errors.New("sentinel: skip tx.Scan")}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "agent:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")

	rec := writeDo(t, h, http.MethodPost, "/v1/knowledge-chunks", tok, nil,
		`{"subject":"hello","content":"world","embedding":`+makeVec1536()+`}`)

	// Sentinel error from FnReturns -> 500 store_failed; the test goal is
	// to confirm the claim.Scope reaches the runner (no DB needed).
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 via sentinel; body=%s", rec.Code, rec.Body.String())
	}
	if !runner.FnInvoked {
		t.Fatal("WithScope never invoked")
	}
	if runner.LastClaim.Scope != "agent:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa" {
		t.Errorf("claim scope = %q; want agent:…", runner.LastClaim.Scope)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "store_failed" {
		t.Errorf("error = %q, want store_failed", env.Error)
	}
}

// TestLogAppend_ActorFromAgentScope confirms that an agent-scope token
// reaches actorFromScope logic: claim.Scope has the agent: prefix, and
// the runner is invoked. The exact column binding is verified by the
// integration suite (fake runner has no tx).
func TestLogAppend_ActorFromAgentScope(t *testing.T) {
	runner := &server.FakeScopedRunner{FnReturns: errors.New("sentinel")}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "agent:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")

	rec := writeDo(t, h, http.MethodPost, "/v1/keepers-log", tok, map[string]any{
		"event_type": "watchkeeper_spawned",
		"payload":    map[string]string{"k": "v"},
	}, "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 via sentinel; body=%s", rec.Code, rec.Body.String())
	}
	if !runner.FnInvoked {
		t.Fatal("WithScope never invoked")
	}
	if !strings.HasPrefix(runner.LastClaim.Scope, "agent:") {
		t.Errorf("claim scope = %q; want agent:…", runner.LastClaim.Scope)
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "log_append_failed" {
		t.Errorf("error = %q, want log_append_failed", env.Error)
	}
}

// TestLogAppend_ActorFromUserScope mirrors the agent case but for user.
func TestLogAppend_ActorFromUserScope(t *testing.T) {
	runner := &server.FakeScopedRunner{FnReturns: errors.New("sentinel")}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "user:bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")

	rec := writeDo(t, h, http.MethodPost, "/v1/keepers-log", tok, map[string]any{
		"event_type": "human_approved",
	}, "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 via sentinel; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.HasPrefix(runner.LastClaim.Scope, "user:") {
		t.Errorf("claim scope = %q; want user:…", runner.LastClaim.Scope)
	}
}

// TestLogAppend_ActorFromOrgScope confirms an org-scope token produces a
// claim with Scope="org" reaching the runner.
func TestLogAppend_ActorFromOrgScope(t *testing.T) {
	runner := &server.FakeScopedRunner{FnReturns: errors.New("sentinel")}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/keepers-log", tok, map[string]any{
		"event_type": "org_event",
	}, "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 via sentinel; body=%s", rec.Code, rec.Body.String())
	}
	if runner.LastClaim.Scope != "org" {
		t.Errorf("claim scope = %q; want org", runner.LastClaim.Scope)
	}
}

// TestPutManifestVersion_Happy asserts the PUT route mounts and threads
// the claim through; fake runner short-circuits with a sentinel so no DB
// is touched. We still exercise the manifest_id path-value extraction.
func TestPutManifestVersion_Happy(t *testing.T) {
	runner := &server.FakeScopedRunner{FnReturns: errors.New("sentinel")}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/cccccccc-cccc-4ccc-8ccc-cccccccccccc/versions",
		tok, map[string]any{
			"version_no":    3,
			"system_prompt": "you are a watchkeeper",
		}, "")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 via sentinel; body=%s", rec.Code, rec.Body.String())
	}
	if !runner.FnInvoked {
		t.Fatal("WithScope never invoked")
	}
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "put_manifest_version_failed" {
		t.Errorf("error = %q, want put_manifest_version_failed", env.Error)
	}
}

// -----------------------------------------------------------------------
// Happy path — 201 + id envelope
// -----------------------------------------------------------------------

const fakeUUID = "11111111-1111-4111-8111-111111111111"

// TestStore_Happy_201 asserts that POST /v1/knowledge-chunks returns
// 201 and a JSON body of {"id":"<uuid>"} when the runner succeeds.
// FakeID drives a fakeTx whose QueryRow.Scan writes the id into the
// handler's `var id string`, exercising the full 201 response path.
func TestStore_Happy_201(t *testing.T) {
	runner := &server.FakeScopedRunner{FakeID: fakeUUID}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "agent:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")

	rec := writeDo(t, h, http.MethodPost, "/v1/knowledge-chunks", tok, nil,
		`{"subject":"hello","content":"world","embedding":`+makeVec1536()+`}`)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	var env struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.ID != fakeUUID {
		t.Errorf("id = %q, want %q", env.ID, fakeUUID)
	}
}

// TestLogAppend_Happy_201 asserts that POST /v1/keepers-log returns
// 201 and {"id":"<uuid>"} when the runner succeeds (org scope so both
// actor columns are NULL — the simplest variant that confirms the full
// response envelope without duplicating the actor-extraction coverage
// already in TestLogAppend_ActorFrom* tests).
func TestLogAppend_Happy_201(t *testing.T) {
	runner := &server.FakeScopedRunner{FakeID: fakeUUID}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/keepers-log", tok, map[string]any{
		"event_type": "org_event",
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
	if env.ID != fakeUUID {
		t.Errorf("id = %q, want %q", env.ID, fakeUUID)
	}
}

// TestPutManifestVersion_Happy_201 asserts that
// PUT /v1/manifests/{id}/versions returns 201 and {"id":"<uuid>"} when
// the runner succeeds, exercising the full success envelope.
func TestPutManifestVersion_Happy_201(t *testing.T) {
	runner := &server.FakeScopedRunner{FakeID: fakeUUID}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/cccccccc-cccc-4ccc-8ccc-cccccccccccc/versions",
		tok, map[string]any{
			"version_no":    3,
			"system_prompt": "you are a watchkeeper",
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
	if env.ID != fakeUUID {
		t.Errorf("id = %q, want %q", env.ID, fakeUUID)
	}
}

// -----------------------------------------------------------------------
// Edge: Content-Type / body size / unknown fields / field validation
// -----------------------------------------------------------------------

// TestWrite_UnsupportedMediaType — every write route must reject a
// non-JSON Content-Type with 415 before body read.
func TestWrite_UnsupportedMediaType(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	cases := []struct {
		name, method, path string
	}{
		{"store", http.MethodPost, "/v1/knowledge-chunks"},
		{"log", http.MethodPost, "/v1/keepers-log"},
		{"put_manifest", http.MethodPut, "/v1/manifests/cccccccc-cccc-4ccc-8ccc-cccccccccccc/versions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path,
				strings.NewReader(`{}`))
			req.Header.Set("Content-Type", "text/plain")
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnsupportedMediaType {
				t.Fatalf("status = %d, want 415; body=%s", rec.Code, rec.Body.String())
			}
			var env struct {
				Error, Reason string
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error != "unsupported_media_type" || env.Reason != "expected_application_json" {
				t.Errorf("body = %q; want error=unsupported_media_type reason=expected_application_json", rec.Body.String())
			}
		})
	}
}

// TestWrite_OversizedBody — every write route must reject >1 MiB bodies
// with 413 before attempting to allocate the full payload.
func TestWrite_OversizedBody(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	// The decoder reads until MaxBytesReader trips. We embed the pad as a
	// string field that, with DisallowUnknownFields, would also fail as an
	// unknown field — but MaxBytesReader fires first because it's a
	// streaming cap.
	pad := strings.Repeat("a", (1<<20)+1024)
	body := `{"event_type":"x","pad":"` + pad + `"}`

	cases := []struct {
		name, method, path string
	}{
		{"store", http.MethodPost, "/v1/knowledge-chunks"},
		{"log", http.MethodPost, "/v1/keepers-log"},
		{"put_manifest", http.MethodPut, "/v1/manifests/cccccccc-cccc-4ccc-8ccc-cccccccccccc/versions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), tc.method, tc.path,
				strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tok)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413; body=%s", rec.Code, rec.Body.String())
			}
			var env struct {
				Error, Reason string
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error != "request_too_large" || env.Reason != "body_too_large" {
				t.Errorf("body = %q; want error=request_too_large reason=body_too_large", rec.Body.String())
			}
		})
	}
}

// TestLogAppend_RejectsActorField — clients MUST NOT be able to forge an
// actor column via a JSON field. DisallowUnknownFields must reject the
// request with 400.
func TestLogAppend_RejectsActorField(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "agent:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")

	rec := writeDo(t, h, http.MethodPost, "/v1/keepers-log", tok, nil,
		`{"event_type":"x","actor_watchkeeper_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"}`)

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
	if env.Error != "invalid_body" {
		t.Errorf("error = %q, want invalid_body", env.Error)
	}
}

// TestStore_RejectsScopeField — the store body MUST NOT accept a scope
// override; DisallowUnknownFields rejects it.
func TestStore_RejectsScopeField(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "agent:aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")

	rec := writeDo(t, h, http.MethodPost, "/v1/knowledge-chunks", tok, nil,
		`{"content":"x","embedding":[0.1],"scope":"org"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
	if runner.FnInvoked {
		t.Error("runner was invoked; expected rejection before tx")
	}
}

// TestStore_WrongDimEmbedding — any embedding that is not exactly 1536 floats
// must be rejected with 400 invalid_embedding. This covers the too-short
// (1535 floats), too-long (1537 floats), and empty (handled separately as
// missing_embedding) cases. The schema declares vector(1536), so any other
// dimension would fail inside Postgres and surface as a misleading 500.
func TestStore_WrongDimEmbedding(t *testing.T) {
	cases := []struct {
		name string
		dims int
	}{
		{"too_short_1", 1},
		{"too_short_1535", 1535},
		{"too_long_1537", 1537},
		{"too_long_4097", 4097},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			var sb strings.Builder
			sb.WriteString(`{"content":"x","embedding":[`)
			for i := 0; i < tc.dims; i++ {
				if i > 0 {
					sb.WriteByte(',')
				}
				sb.WriteByte('0')
			}
			sb.WriteString(`]}`)

			rec := writeDo(t, h, http.MethodPost, "/v1/knowledge-chunks", tok, nil, sb.String())
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
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
		})
	}
}

// TestPutManifestVersion_InvalidVersionNo — version_no = 0 → 400.
func TestPutManifestVersion_InvalidVersionNo(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	for _, vn := range []int{0, -1} {
		t.Run(strconv.Itoa(vn), func(t *testing.T) {
			rec := writeDo(t, h, http.MethodPut,
				"/v1/manifests/cccccccc-cccc-4ccc-8ccc-cccccccccccc/versions", tok,
				map[string]any{
					"version_no":    vn,
					"system_prompt": "ok",
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
			if env.Error != "invalid_version_no" {
				t.Errorf("error = %q, want invalid_version_no", env.Error)
			}
		})
	}
}

// TestWrite_EmptyRequiredFields — empty content / event_type /
// system_prompt → 400 with the field-specific error code.
func TestWrite_EmptyRequiredFields(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	cases := []struct {
		name, method, path, body, wantErr string
	}{
		{
			"store_missing_content",
			http.MethodPost, "/v1/knowledge-chunks",
			`{"content":"","embedding":[0.1]}`,
			"missing_content",
		},
		{
			"log_missing_event_type",
			http.MethodPost, "/v1/keepers-log",
			`{"event_type":""}`,
			"missing_event_type",
		},
		{
			"put_manifest_missing_system_prompt",
			http.MethodPut, "/v1/manifests/cccccccc-cccc-4ccc-8ccc-cccccccccccc/versions",
			`{"version_no":1,"system_prompt":""}`,
			"missing_system_prompt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := writeDo(t, h, tc.method, tc.path, tok, nil, tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
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
		})
	}
}

// -----------------------------------------------------------------------
// Negative: runner errors — 409 unique / 500 generic
// -----------------------------------------------------------------------

// TestPutManifestVersion_UniqueViolation — a pgx unique-violation from the
// runner must surface as 409 version_conflict with no Postgres text in
// the response body.
func TestPutManifestVersion_UniqueViolation(t *testing.T) {
	uniqueErr := &pgconn.PgError{
		Code:           "23505",
		Message:        "duplicate key value violates unique constraint",
		ConstraintName: "manifest_version_manifest_id_version_no_key",
		Detail:         "Key (manifest_id, version_no)=(…, 1) already exists.",
	}
	runner := &server.FakeScopedRunner{FnReturns: uniqueErr}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/cccccccc-cccc-4ccc-8ccc-cccccccccccc/versions", tok,
		map[string]any{
			"version_no":    1,
			"system_prompt": "ok",
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
	if env.Error != "version_conflict" {
		t.Errorf("error = %q, want version_conflict", env.Error)
	}
	// The raw Postgres text ("duplicate key value…", "manifest_version_…")
	// must not leak.
	rawBody := rec.Body.String()
	for _, forbidden := range []string{"duplicate", "manifest_version_", "already exists"} {
		if strings.Contains(rawBody, forbidden) {
			t.Errorf("response body leaked %q: %s", forbidden, rawBody)
		}
	}
}

// TestWrite_GenericRunnerError — a non-pgx error from the runner must
// surface as 500 with the per-endpoint stable code, never the raw text.
func TestWrite_GenericRunnerError(t *testing.T) {
	type genericCase struct {
		name, method, path, wantErr string
		body                        any
		rawBody                     string
	}
	cases := []genericCase{
		{
			name:    "store",
			method:  http.MethodPost,
			path:    "/v1/knowledge-chunks",
			wantErr: "store_failed",
			rawBody: `{"content":"x","embedding":` + makeVec1536() + `}`,
		},
		{
			name:    "log",
			method:  http.MethodPost,
			path:    "/v1/keepers-log",
			wantErr: "log_append_failed",
			body:    map[string]any{"event_type": "x"},
		},
		{
			name:    "put_manifest",
			method:  http.MethodPut,
			path:    "/v1/manifests/cccccccc-cccc-4ccc-8ccc-cccccccccccc/versions",
			wantErr: "put_manifest_version_failed",
			body:    map[string]any{"version_no": 1, "system_prompt": "ok"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{FnReturns: errors.New("database unreachable")}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, tc.method, tc.path, tok, tc.body, tc.rawBody)
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
				t.Errorf("raw runner error leaked into response body: %s", rec.Body.String())
			}
		})
	}
}

// TestWrite_UnauthenticatedRejects — every write route sits behind the
// auth wall. No Authorization header -> 401 missing_token.
func TestWrite_UnauthenticatedRejects(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, _ := writeRouterForTest(t, mustFixedNow(), runner)

	cases := []struct {
		name, method, path string
	}{
		{"store", http.MethodPost, "/v1/knowledge-chunks"},
		{"log", http.MethodPost, "/v1/keepers-log"},
		{"put_manifest", http.MethodPut, "/v1/manifests/cccccccc-cccc-4ccc-8ccc-cccccccccccc/versions"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := writeDo(t, h, tc.method, tc.path, "", map[string]any{"x": 1}, "")
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body=%s", rec.Code, rec.Body.String())
			}
			var env struct {
				Error, Reason string
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error != "unauthorized" || env.Reason != "missing_token" {
				t.Errorf("body = %q; want error=unauthorized reason=missing_token", rec.Body.String())
			}
		})
	}
}

// -----------------------------------------------------------------------
// UUID prevalidation
// -----------------------------------------------------------------------

// TestLogAppend_InvalidCorrelationID — a malformed correlation_id must be
// rejected with 400 invalid_correlation_id before the row reaches Postgres.
func TestLogAppend_InvalidCorrelationID(t *testing.T) {
	cases := []struct {
		name          string
		correlationID string
	}{
		{"not_uuid", "not-a-uuid"},
		{"too_short", "1234-5678"},
		{"empty_segments", "--------"},
		{"with_braces", "{aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPost, "/v1/keepers-log", tok, nil,
				`{"event_type":"x","correlation_id":"`+tc.correlationID+`"}`)

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
			if env.Error != "invalid_correlation_id" {
				t.Errorf("error = %q, want invalid_correlation_id", env.Error)
			}
		})
	}
}

// TestLogAppend_ValidCorrelationID — a well-formed correlation_id must pass
// prevalidation and reach the runner (sentinel error confirms the tx path).
func TestLogAppend_ValidCorrelationID(t *testing.T) {
	runner := &server.FakeScopedRunner{FnReturns: errors.New("sentinel")}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPost, "/v1/keepers-log", tok, nil,
		`{"event_type":"x","correlation_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}`)

	// Sentinel from runner means 500 log_append_failed, not 400 — prevalidation passed.
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (sentinel); body=%s", rec.Code, rec.Body.String())
	}
	if !runner.FnInvoked {
		t.Error("runner not invoked; valid correlation_id should pass prevalidation")
	}
}

// TestPutManifestVersion_InvalidLanguage — a non-empty `language` body field
// that does not match the BCP 47-lite regex must be rejected with 400
// invalid_language before the row reaches Postgres. Mirrors the SQL
// `manifest_version_language_format` CHECK from migration 010.
func TestPutManifestVersion_InvalidLanguage(t *testing.T) {
	cases := []struct {
		name, language string
	}{
		{"too_long", "english"},
		{"uppercase", "EN"},
		{"region_lowercase", "en-us"},
		{"region_too_long", "en-USA"},
		{"digits", "123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPut,
				"/v1/manifests/cccccccc-cccc-4ccc-8ccc-cccccccccccc/versions",
				tok, map[string]any{
					"version_no":    1,
					"system_prompt": "ok",
					"language":      tc.language,
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
			if env.Error != "invalid_language" {
				t.Errorf("error = %q, want invalid_language", env.Error)
			}
		})
	}
}

// TestPutManifestVersion_WithModel_201_RoundTrip — PUT body carries
// `model:"claude-sonnet-4"`; the handler must thread the value through
// the INSERT (M5.5.b.b.a AC4) and a subsequent GET on the same fake
// runner returns the captured model on the wire. Round-trip is asserted
// at the wire shape: (1) the INSERT's bound args contain the model
// string, and (2) the GET response JSON has `model:"claude-sonnet-4"`.
// The handler tests do not run a real DB; the fake tx captures the
// model arg from PUT and a fakeRow Scan closure replays it for GET.
func TestPutManifestVersion_WithModel_201_RoundTrip(t *testing.T) {
	const wantModel = "claude-sonnet-4"
	var capturedModel string
	var gotSQL string
	queryRow := func(_ context.Context, sql string, args ...any) pgx.Row {
		gotSQL = sql
		// PUT INSERT branch: capture the bound model arg. The handler
		// passes `stringOrNil(body.Model)` so a non-empty model becomes
		// a `string`; a NULL model becomes `nil`. Walk the args slice
		// and grab the first string that matches the input value.
		for _, a := range args {
			if s, ok := a.(string); ok && s == wantModel {
				capturedModel = s
			}
		}
		return server.NewFakeRow(func(dest ...any) error {
			if sp, ok := dest[0].(*string); ok {
				*sp = fakeUUID
			}
			return nil
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	// Step 1: PUT with model — assert 201 and model arg threaded.
	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/"+putManifestID+"/versions", tok,
		map[string]any{
			"version_no":    1,
			"system_prompt": "ok",
			"model":         wantModel,
		}, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	if capturedModel != wantModel {
		t.Fatalf("model arg not bound on INSERT; capturedModel=%q want=%q", capturedModel, wantModel)
	}
	if !strings.Contains(gotSQL, "model") {
		t.Errorf("INSERT SQL missing model column; got SQL: %s", gotSQL)
	}

	// Step 2: GET — stage a SELECT row that replays the captured model.
	getQueryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
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
			*dest[3].(*string) = "ok"
			*dest[4].(*json.RawMessage) = json.RawMessage(`[]`)
			*dest[5].(*json.RawMessage) = json.RawMessage(`{}`)
			*dest[6].(*json.RawMessage) = json.RawMessage(`[]`)
			*dest[7].(*string) = ""
			*dest[8].(*string) = ""
			*dest[9].(*string) = capturedModel
			*dest[10].(*time.Time) = time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
			return nil
		})
	}
	getRunner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: getQueryRow})}
	gh, gti := writeRouterForTest(t, mustFixedNow(), getRunner)
	gtok := mustMintToken(t, gti, "org")

	greq := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/v1/manifests/"+putManifestID, nil)
	greq.Header.Set("Authorization", "Bearer "+gtok)
	grec := httptest.NewRecorder()
	gh.ServeHTTP(grec, greq)
	if grec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", grec.Code, grec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(grec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET decode: %v", err)
	}
	if got["model"] != wantModel {
		t.Errorf("GET response model = %v, want %q; body=%s", got["model"], wantModel, grec.Body.String())
	}
}

// TestPutManifestVersion_ModelOmitted_201_GetHasNoModelKey — when the
// PUT body omits `model`, a subsequent GET must NOT include a `model`
// key in the response JSON (omitempty). Mirrors the wire-omit posture
// of `personality` / `language`.
func TestPutManifestVersion_ModelOmitted_201_GetHasNoModelKey(t *testing.T) {
	// Step 1: PUT without model — assert 201 and runner sees exactly six
	// nil args (tools, authority_matrix, knowledge_sources, personality,
	// language, model). Counting all six means a regression that removes
	// only the model binding surfaces as count=5, not a silent pass.
	var nilArgCount int
	queryRow := func(_ context.Context, _ string, args ...any) pgx.Row {
		// Handler passes stringOrNil("") / jsonbOrNil(nil) which both
		// return the untyped nil interface for omitted fields.
		for _, a := range args {
			if a == nil {
				nilArgCount++
			}
		}
		return server.NewFakeRow(func(dest ...any) error {
			if sp, ok := dest[0].(*string); ok {
				*sp = fakeUUID
			}
			return nil
		})
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/"+putManifestID+"/versions", tok,
		map[string]any{
			"version_no":    1,
			"system_prompt": "ok",
		}, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("PUT status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
	// Expect exactly 6 nil args: tools, authority_matrix, knowledge_sources,
	// personality, language, model. If model wiring is absent the count drops
	// to 5 and this assertion catches the regression.
	const wantNilArgs = 6
	if nilArgCount != wantNilArgs {
		t.Errorf("nil arg count = %d, want %d (tools/authority_matrix/knowledge_sources/personality/language/model)", nilArgCount, wantNilArgs)
	}

	// Step 2: GET — SELECT returns coalesce(model, '') == "" so the
	// response JSON must not carry a `model` key.
	getQueryRow := func(_ context.Context, _ string, _ ...any) pgx.Row {
		return server.NewFakeRow(func(dest ...any) error {
			*dest[0].(*string) = fakeUUID
			*dest[1].(*string) = putManifestID
			*dest[2].(*int) = 1
			*dest[3].(*string) = "ok"
			*dest[4].(*json.RawMessage) = json.RawMessage(`[]`)
			*dest[5].(*json.RawMessage) = json.RawMessage(`{}`)
			*dest[6].(*json.RawMessage) = json.RawMessage(`[]`)
			*dest[7].(*string) = ""
			*dest[8].(*string) = ""
			*dest[9].(*string) = "" // model NULL → coalesce → ""
			*dest[10].(*time.Time) = time.Date(2026, 5, 7, 0, 0, 0, 0, time.UTC)
			return nil
		})
	}
	getRunner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: getQueryRow})}
	gh, gti := writeRouterForTest(t, mustFixedNow(), getRunner)
	gtok := mustMintToken(t, gti, "org")

	greq := httptest.NewRequestWithContext(context.Background(), http.MethodGet,
		"/v1/manifests/"+putManifestID, nil)
	greq.Header.Set("Authorization", "Bearer "+gtok)
	grec := httptest.NewRecorder()
	gh.ServeHTTP(grec, greq)
	if grec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200; body=%s", grec.Code, grec.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(grec.Body.Bytes(), &got); err != nil {
		t.Fatalf("GET decode: %v", err)
	}
	if _, present := got["model"]; present {
		t.Errorf("GET response carries model key when body omitted it; body=%s", grec.Body.String())
	}
}

// TestPutManifestVersion_ModelExactly100Chars_201 — boundary check: a
// model exactly 100 unicode codepoints long must be accepted (mirror of
// the SQL `char_length(model) <= 100` CHECK).
func TestPutManifestVersion_ModelExactly100Chars_201(t *testing.T) {
	runner := &server.FakeScopedRunner{FakeID: fakeUUID}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/"+putManifestID+"/versions", tok,
		map[string]any{
			"version_no":    1,
			"system_prompt": "ok",
			"model":         strings.Repeat("a", 100),
		}, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body=%s", rec.Code, rec.Body.String())
	}
}

// TestPutManifestVersion_ModelOver100Chars_400_ModelTooLong — a model
// longer than 100 Unicode codepoints must be rejected with 400
// model_too_long before the row reaches Postgres. Mirrors the SQL
// `manifest_version_model_length` CHECK from migration 014.
func TestPutManifestVersion_ModelOver100Chars_400_ModelTooLong(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/"+putManifestID+"/versions", tok,
		map[string]any{
			"version_no":    1,
			"system_prompt": "ok",
			"model":         strings.Repeat("a", 101),
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
	if env.Error != "model_too_long" {
		t.Errorf("error = %q, want model_too_long", env.Error)
	}
}

// TestPutManifestVersion_PersonalityTooLong — a personality longer than
// 1024 Unicode codepoints must be rejected with 400 personality_too_long
// before the row reaches Postgres. Mirrors the SQL
// `manifest_version_personality_length` CHECK from migration 010.
func TestPutManifestVersion_PersonalityTooLong(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/cccccccc-cccc-4ccc-8ccc-cccccccccccc/versions",
		tok, map[string]any{
			"version_no":    1,
			"system_prompt": "ok",
			"personality":   strings.Repeat("a", 1025),
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
	if env.Error != "personality_too_long" {
		t.Errorf("error = %q, want personality_too_long", env.Error)
	}
}

// TestPutManifestVersion_InvalidManifestID — a malformed manifest_id path
// segment must be rejected with 400 invalid_manifest_id before the body is
// decoded or the runner is called.
func TestPutManifestVersion_InvalidManifestID(t *testing.T) {
	cases := []struct {
		name       string
		manifestID string
	}{
		{"not_uuid", "not-a-uuid"},
		{"too_short", "1234-5678"},
		{"plain_word", "manifest"},
		{"with_braces", "{cccccccc-cccc-4ccc-8ccc-cccccccccccc}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &server.FakeScopedRunner{}
			h, ti := writeRouterForTest(t, mustFixedNow(), runner)
			tok := mustMintToken(t, ti, "org")

			rec := writeDo(t, h, http.MethodPut,
				"/v1/manifests/"+tc.manifestID+"/versions", tok,
				map[string]any{"version_no": 1, "system_prompt": "ok"}, "")

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
			if env.Error != "invalid_manifest_id" {
				t.Errorf("error = %q, want invalid_manifest_id", env.Error)
			}
		})
	}
}

// -----------------------------------------------------------------------
// M3.5.a.3.2 cross-tenant rejection coverage on PUT manifest version
// -----------------------------------------------------------------------
//
// The four mutating handlers wired in M3.5.a.2 (handleInsertHuman,
// handleSetWatchkeeperLead, handleUpdateWatchkeeperStatus,
// handleInsertWatchkeeper) carry parallel CrossTenant + LegacyClaim
// rejection tests in handlers_human_test.go and
// handlers_watchkeeper_test.go. M3.5.a.3.2 closes the gap on
// handlePutManifestVersion; the two tests below mirror that contract:
//
//   * cross-tenant manifest_id → 404 not_found (no row-existence oracle,
//     same posture as handleSetWatchkeeperLead and handleInsertWatchkeeper);
//   * legacy claim with no OrganizationID → 403 organization_required
//     before WithScope ever runs (no DB round-trip on legacy callers).

const (
	putManifestVersionFakeID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	putManifestID            = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
)

// TestPutManifestVersion_CrossTenantRejected — claim carries org X; the
// supplied manifest_id resolves (in a real DB) to a manifest in org Y.
// M3.5.a.3.2 wires the INSERT through a `WHERE EXISTS (SELECT 1 FROM
// watchkeeper.manifest WHERE id = $manifest_id AND organization_id =
// $claim_org)` subquery, so a cross-tenant caller cannot anchor a
// manifest_version at another tenant's manifest. Under the new SQL the
// INSERT … RETURNING returns no row, surfaced as pgx.ErrNoRows → 404
// not_found (mirrors the contract on handleInsertWatchkeeper). We
// assert (1) the SQL contains the organization_id binding and (2) the
// claim org is bound as an SQL argument.
func TestPutManifestVersion_CrossTenantRejected(t *testing.T) {
	const otherOrgID = "77777777-7777-4777-8777-777777777777"
	var gotSQL string
	var gotArgs []any
	queryRow := func(_ context.Context, sql string, args ...any) pgx.Row {
		gotSQL = sql
		gotArgs = args
		// Stage ErrNoRows: under the new SQL the INSERT … SELECT … WHERE
		// EXISTS clause matches zero rows when the manifest's org does
		// not equal the claim org, so RETURNING produces no row and
		// Scan surfaces pgx.ErrNoRows.
		return server.NewFakeRowErr(pgx.ErrNoRows)
	}
	runner := &server.FakeScopedRunner{Tx: server.NewFakeTx(server.FakeTxFns{QueryRow: queryRow})}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintTokenForOrg(t, ti, "org", otherOrgID)

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/"+putManifestID+"/versions", tok,
		map[string]any{
			"version_no":    1,
			"system_prompt": "ok",
		}, "")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", rec.Code, rec.Body.String())
	}
	// Org filter MUST be on the SQL — without it any authenticated
	// caller could write a version against any tenant's manifest.
	if !strings.Contains(gotSQL, "organization_id") {
		t.Errorf("INSERT missing organization_id filter; got SQL: %s", gotSQL)
	}
	// The handler must bind claim.OrganizationID as one of the SQL
	// arguments. The exact slot can shift if the placeholder layout
	// changes; assert membership rather than a fixed index so the test
	// is robust to a re-ordering refactor while still proving the
	// claim org reaches Postgres.
	found := false
	for _, a := range gotArgs {
		if a == otherOrgID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("claim org %q not bound to any SQL arg; args=%v", otherOrgID, gotArgs)
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

// TestPutManifestVersion_LegacyClaimRejected — claim carries an EMPTY
// OrganizationID (the pre-M3.5.a.1 wire shape). M3.5.a.3.2 contract
// mirrors the four other M3.5.a.2 handlers: 403 organization_required
// before WithScope ever runs, so the runner never sees the claim and
// no DB round-trip happens for a legacy token.
func TestPutManifestVersion_LegacyClaimRejected(t *testing.T) {
	runner := &server.FakeScopedRunner{FakeID: putManifestVersionFakeID}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintLegacyToken(t, ti, "org")

	rec := writeDo(t, h, http.MethodPut,
		"/v1/manifests/"+putManifestID+"/versions", tok,
		map[string]any{
			"version_no":    1,
			"system_prompt": "ok",
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
