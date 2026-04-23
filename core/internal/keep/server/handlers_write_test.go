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

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

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

	rec := writeDo(t, h, http.MethodPost, "/v1/knowledge-chunks", tok, map[string]any{
		"subject":   "hello",
		"content":   "world",
		"embedding": []float32{0.1, 0.2, 0.3},
	}, "")

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

	rec := writeDo(t, h, http.MethodPost, "/v1/knowledge-chunks", tok, map[string]any{
		"subject":   "hello",
		"content":   "world",
		"embedding": []float32{0.1, 0.2, 0.3},
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

// TestStore_OversizedEmbedding — 4097 floats → 400 invalid_embedding.
func TestStore_OversizedEmbedding(t *testing.T) {
	runner := &server.FakeScopedRunner{}
	h, ti := writeRouterForTest(t, mustFixedNow(), runner)
	tok := mustMintToken(t, ti, "org")

	var sb strings.Builder
	sb.WriteString(`{"content":"x","embedding":[`)
	for i := 0; i < 4097; i++ {
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
	cases := []struct {
		name, method, path, wantErr string
		body                        any
	}{
		{
			"store",
			http.MethodPost, "/v1/knowledge-chunks", "store_failed",
			map[string]any{"content": "x", "embedding": []float32{0.1}},
		},
		{
			"log",
			http.MethodPost, "/v1/keepers-log", "log_append_failed",
			map[string]any{"event_type": "x"},
		},
		{
			"put_manifest",
			http.MethodPut, "/v1/manifests/cccccccc-cccc-4ccc-8ccc-cccccccccccc/versions",
			"put_manifest_version_failed",
			map[string]any{"version_no": 1, "system_prompt": "ok"},
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
