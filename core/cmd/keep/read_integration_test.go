//go:build integration

// Integration tests for the Keep read API (M2.7.b+c). Requires a Postgres
// 16 with pgvector reachable via KEEP_INTEGRATION_DB_URL and with
// migrations 001..007 applied (CI wires this through the Keep Integration
// CI job). Run locally with:
//
//	KEEP_INTEGRATION_DB_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable \
//	  go test -tags=integration -v -run 'TestReadAPI_' ./core/cmd/keep/...
//
// Each test seeds its own hermetic fixture (scoped by a per-test suffix so
// parallel runs never collide), then either talks to the boot-spawned
// Keep binary over HTTP or asserts a middleware branch that does not need
// the real server. Claims are minted by auth.TestIssuer; the binary
// verifies with the matching signing key taken from the test env.
package main_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
)

// readBootEnv returns the env slice to pass to the Keep binary for every
// read-API integration test. Keeping it in one place avoids drift between
// tests if a new required env var lands later.
func readBootEnv(dsn, addr string) []string {
	return append(
		os.Environ(),
		"KEEP_DATABASE_URL="+dsn,
		"KEEP_HTTP_ADDR="+addr,
		"KEEP_SHUTDOWN_TIMEOUT=5s",
		"KEEP_TOKEN_SIGNING_KEY="+testTokenSigningKeyB64,
		"KEEP_TOKEN_ISSUER="+testTokenIssuer,
	)
}

// issuerForTest returns an auth.TestIssuer whose key and issuer exactly
// match what the spawned binary accepts.
func issuerForTest(t *testing.T) *auth.TestIssuer {
	t.Helper()
	key, err := base64Decode(testTokenSigningKeyB64)
	if err != nil {
		t.Fatalf("decode signing key: %v", err)
	}
	ti, err := auth.NewTestIssuer(key, testTokenIssuer, time.Now)
	if err != nil {
		t.Fatalf("NewTestIssuer: %v", err)
	}
	return ti
}

// base64Decode is a tiny helper around base64.StdEncoding; keeping it as a
// named function makes the fatal-log callsites read naturally.
func base64Decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

// testEnv holds a live *pgxpool.Pool plus the seeded fixture ids for one
// integration test. Tests build it via newTestEnv(t) and rely on t.Cleanup
// to close the pool; the fixture rows stay in the shared DB but every id
// is suffixed so parallel tests never observe each other's data.
type testEnv struct {
	pool *pgxpool.Pool
	dsn  string

	orgID             string
	humanID           string
	watchkeeperID     string
	manifestID        string
	manifestVersionID string
	userScope         string
	agentScope        string

	// subjects uniquely tag the fixture rows so concurrent tests can
	// count their own rows without interfering.
	subjectTag string
}

// newUUID generates a Postgres-compatible UUID string using crypto/rand.
// Returns the v4 format `xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx`. Declared
// here rather than via github.com/google/uuid to keep the Go module
// surface minimal — this file is the only consumer of UUID generation
// outside of the DB.
func newUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	// RFC 4122 version 4 + variant.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexStr := hex.EncodeToString(b[:])
	return hexStr[0:8] + "-" + hexStr[8:12] + "-" + hexStr[12:16] + "-" + hexStr[16:20] + "-" + hexStr[20:32]
}

// newTestEnv opens a pool against KEEP_INTEGRATION_DB_URL, seeds a minimal
// fixture set, and registers cleanup. The fixture shape matches AC6:
// 1 organisation, 1 human, 1 watchkeeper, 1 manifest + two versions, three
// knowledge_chunk rows (org/user:<u>/agent:<a>), three keepers_log rows.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dsn := requireDBURL(t)

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	t.Cleanup(pool.Close)

	tag := newUUID(t)
	env := &testEnv{
		pool:              pool,
		dsn:               dsn,
		orgID:             newUUID(t),
		humanID:           newUUID(t),
		manifestID:        newUUID(t),
		watchkeeperID:     newUUID(t),
		manifestVersionID: newUUID(t),
		subjectTag:        "rt-" + tag[:8],
	}
	env.userScope = "user:" + env.humanID
	env.agentScope = "agent:" + env.watchkeeperID

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// All fixture rows are committed in one transaction so a failure
	// halfway through leaves no junk behind.
	if err := pgxBeginAndRun(ctx, pool, func(ctx context.Context, tx pgx.Tx) error {
		return env.seed(ctx, tx)
	}); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}

	t.Cleanup(func() {
		// Best-effort cleanup. If a test panicked mid-scope the rows stay
		// in place; the next CI run's TRUNCATE from migrate-schema-test
		// will clear them. We filter by subject_tag so parallel tests
		// never delete each other's rows.
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelCleanup()
		_, _ = pool.Exec(cleanupCtx, `
            DELETE FROM watchkeeper.knowledge_chunk WHERE subject LIKE $1;
        `, env.subjectTag+"-%")
		_, _ = pool.Exec(cleanupCtx, `
            DELETE FROM watchkeeper.keepers_log WHERE payload->>'tag' = $1;
        `, env.subjectTag)
	})

	return env
}

// pgxBeginAndRun wraps a pgx.Tx block with the same commit-on-success /
// rollback-on-error discipline as db.WithScope, but without the role /
// scope swap — we need owner privileges for seed inserts.
func pgxBeginAndRun(ctx context.Context, pool *pgxpool.Pool, fn func(context.Context, pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// seed inserts the org/human/watchkeeper/manifest/version/chunks/log rows.
// The embedding is a deterministic zero vector (1536 zeros) because the
// KNN tests only care about row visibility — not distances — and we avoid
// non-determinism that would flake in CI.
func (e *testEnv) seed(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(
		ctx,
		`INSERT INTO watchkeeper.organization (id, display_name) VALUES ($1, $2)`,
		e.orgID, "Integration Org "+e.subjectTag,
	); err != nil {
		return fmt.Errorf("org: %w", err)
	}
	if _, err := tx.Exec(
		ctx,
		`INSERT INTO watchkeeper.human (id, organization_id, display_name) VALUES ($1, $2, $3)`,
		e.humanID, e.orgID, "Integration Human "+e.subjectTag,
	); err != nil {
		return fmt.Errorf("human: %w", err)
	}
	if _, err := tx.Exec(
		ctx,
		`INSERT INTO watchkeeper.manifest (id, display_name, created_by_human_id) VALUES ($1, $2, $3)`,
		e.manifestID, "Integration Manifest "+e.subjectTag, e.humanID,
	); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	// Two versions so AC4's "highest version_no wins" branch has data.
	// First version id is server-generated (gen_random_uuid default); only
	// the latest (version 2) is pinned because the test asserts on it.
	if _, err := tx.Exec(ctx, `
        INSERT INTO watchkeeper.manifest_version (manifest_id, version_no, system_prompt, personality, language)
        VALUES ($1, 1, 'v1 prompt', 'calm', 'en')
    `, e.manifestID); err != nil {
		return fmt.Errorf("mv1: %w", err)
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO watchkeeper.manifest_version (id, manifest_id, version_no, system_prompt, personality, language)
        VALUES ($1, $2, 2, 'v2 prompt', 'focused', 'en')
    `, e.manifestVersionID, e.manifestID); err != nil {
		return fmt.Errorf("mv2: %w", err)
	}
	if _, err := tx.Exec(ctx, `
        INSERT INTO watchkeeper.watchkeeper (id, manifest_id, lead_human_id, active_manifest_version_id, status, spawned_at)
        VALUES ($1, $2, $3, $4, 'active', now())
    `, e.watchkeeperID, e.manifestID, e.humanID, e.manifestVersionID); err != nil {
		return fmt.Errorf("watchkeeper: %w", err)
	}

	// pgvector cosine distance against a pure-zero vector is undefined
	// (division by zero) and surfaces as NaN through pgx, which the Go
	// json encoder then rejects. Use a small non-zero component so every
	// fixture row has a finite cosine distance regardless of the query
	// vector shape (see lesson from M2.1 seeding pattern).
	onesVec := "[" + strings.Repeat("0.1,", 1535) + "0.1]"
	chunks := []struct{ scope, subject string }{
		{"org", e.subjectTag + "-org"},
		{e.userScope, e.subjectTag + "-user"},
		{e.agentScope, e.subjectTag + "-agent"},
	}
	for _, c := range chunks {
		if _, err := tx.Exec(ctx, `
            INSERT INTO watchkeeper.knowledge_chunk (scope, subject, content, embedding)
            VALUES ($1, $2, $3, $4::vector)
        `, c.scope, c.subject, "content for "+c.subject, onesVec); err != nil {
			return fmt.Errorf("chunk %s: %w", c.scope, err)
		}
	}

	// Three keepers_log rows with deterministic created_at offsets so the
	// ORDER BY test has a stable expectation.
	base := time.Now().UTC().Truncate(time.Second)
	events := []struct {
		eventType string
		offset    time.Duration
	}{
		{"oldest", -2 * time.Minute},
		{"middle", -time.Minute},
		{"newest", 0},
	}
	for _, ev := range events {
		payload := fmt.Sprintf(`{"tag":"%s","event":"%s"}`, e.subjectTag, ev.eventType)
		if _, err := tx.Exec(ctx, `
            INSERT INTO watchkeeper.keepers_log (event_type, payload, created_at)
            VALUES ($1, $2::jsonb, $3)
        `, ev.eventType, payload, base.Add(ev.offset)); err != nil {
			return fmt.Errorf("keepers_log %s: %w", ev.eventType, err)
		}
	}

	return nil
}

// bootKeep compiles + starts the Keep binary against env.dsn. It returns
// the listening address and a teardown function that SIGTERMs the process
// and waits for exit.
func bootKeep(t *testing.T, env *testEnv) (addr string, teardown func()) {
	t.Helper()
	bin := buildBinary(t)
	addr = pickLocalAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = readBootEnv(env.dsn, addr)
	var stderr strings.Builder
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start binary: %v", err)
	}
	waitForHealth(t, addr)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	teardown = func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(exitBudget):
			_ = cmd.Process.Kill()
		}
		cancel()
	}
	return addr, teardown
}

// doJSON executes an HTTP request against the spawned binary and returns
// the decoded status code + raw body. Every test reuses it to keep its
// own body short.
func doJSON(t *testing.T, method, url, auth string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, r)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("http do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, respBody
}

// mintToken mints a short-lived token with the given scope using the
// test issuer. Every happy-path test in the file calls this.
func mintToken(t *testing.T, ti *auth.TestIssuer, scope string) string {
	t.Helper()
	tok, err := ti.Issue(auth.Claim{Subject: "test-subject", Scope: scope}, 5*time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// mintTokenWithOrg mints a short-lived token with the given scope and
// OrganizationID. Used by handlers that enforce claim.OrganizationID != ""
// (e.g. handleInsertHuman, handleSetWatchkeeperLead added in M3.5.a.2).
func mintTokenWithOrg(t *testing.T, ti *auth.TestIssuer, scope, orgID string) string {
	t.Helper()
	tok, err := ti.Issue(auth.Claim{Subject: "test-subject", Scope: scope, OrganizationID: orgID}, 5*time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	return tok
}

// searchBody is the request shape expected by POST /v1/search; defined
// here rather than imported from server so the test stays a black-box
// client.
type searchBody struct {
	Embedding []float32 `json:"embedding"`
	TopK      int       `json:"top_k"`
}

type searchResponse struct {
	Results []struct {
		ID       string  `json:"id"`
		Subject  string  `json:"subject"`
		Content  string  `json:"content"`
		Distance float64 `json:"distance"`
	} `json:"results"`
}

// queryVec1536 builds a 1536-wide query embedding with the same non-zero
// component as the fixture rows. The cosine distance between two parallel
// vectors is 0, so every seeded row ends up with `distance = 0` — this is
// finite and JSON-serializable (a pure-zero vector would cause pgvector to
// return NaN, which encoding/json rejects mid-stream and silently
// truncates the response).
func queryVec1536() []float32 {
	out := make([]float32, 1536)
	for i := range out {
		out[i] = 0.1
	}
	return out
}

// -----------------------------------------------------------------------
// Happy paths
// -----------------------------------------------------------------------

func TestReadAPI_Search_AgentScope(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, env.agentScope)

	status, body := doJSON(t, http.MethodPost, "http://"+addr+"/v1/search", tok, searchBody{
		Embedding: queryVec1536(),
		TopK:      10,
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d; body = %s", status, body)
	}
	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}

	// Expect subjects: tag-org and tag-agent; never tag-user.
	var sawOrg, sawAgent bool
	for _, r := range resp.Results {
		switch r.Subject {
		case env.subjectTag + "-org":
			sawOrg = true
		case env.subjectTag + "-agent":
			sawAgent = true
		case env.subjectTag + "-user":
			t.Errorf("agent scope saw user-scoped row: %+v", r)
		}
	}
	if !sawOrg || !sawAgent {
		t.Errorf("expected both org + agent rows; got %+v", resp.Results)
	}
}

func TestReadAPI_Search_UserScope(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, env.userScope)

	status, body := doJSON(t, http.MethodPost, "http://"+addr+"/v1/search", tok, searchBody{
		Embedding: queryVec1536(),
		TopK:      10,
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d; body = %s", status, body)
	}
	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var sawOrg, sawUser bool
	for _, r := range resp.Results {
		switch r.Subject {
		case env.subjectTag + "-org":
			sawOrg = true
		case env.subjectTag + "-user":
			sawUser = true
		case env.subjectTag + "-agent":
			t.Errorf("user scope saw agent-scoped row: %+v", r)
		}
	}
	if !sawOrg || !sawUser {
		t.Errorf("expected both org + user rows; got %+v", resp.Results)
	}
}

type manifestResponse struct {
	ID         string `json:"id"`
	ManifestID string `json:"manifest_id"`
	VersionNo  int    `json:"version_no"`
}

func TestReadAPI_GetManifest_LatestVersion(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	status, body := doJSON(t, http.MethodGet,
		"http://"+addr+"/v1/manifests/"+env.manifestID, tok, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body = %s", status, body)
	}
	var resp manifestResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.VersionNo != 2 {
		t.Errorf("version_no = %d, want 2 (latest)", resp.VersionNo)
	}
	if resp.ID != env.manifestVersionID {
		t.Errorf("id = %s, want %s", resp.ID, env.manifestVersionID)
	}
}

type logTailResponse struct {
	Events []struct {
		ID        string    `json:"id"`
		EventType string    `json:"event_type"`
		CreatedAt time.Time `json:"created_at"`
	} `json:"events"`
}

func TestReadAPI_LogTail_Ordering(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	status, body := doJSON(t, http.MethodGet,
		"http://"+addr+"/v1/keepers-log?limit=200", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body = %s", status, body)
	}
	var resp logTailResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Filter down to just our three seeded rows — other tests running
	// in parallel may have added more.
	var ours []string
	for _, ev := range resp.Events {
		switch ev.EventType {
		case "oldest", "middle", "newest":
			ours = append(ours, ev.EventType)
		}
	}
	// Ordering matters: newest first.
	wantPrefix := []string{"newest", "middle", "oldest"}
	if len(ours) < len(wantPrefix) {
		t.Fatalf("got %d of our events; want >= %d (events=%v)", len(ours), len(wantPrefix), resp.Events)
	}
	// Take the first three matching rows in global desc order; asserting
	// stable order across all global events is too brittle for a parallel
	// run, so we confirm the relative order of our seeded rows stays
	// monotonic by checking the first occurrence of each.
	seen := map[string]int{"newest": -1, "middle": -1, "oldest": -1}
	for i, ev := range resp.Events {
		if idx, ok := seen[ev.EventType]; ok && idx == -1 {
			seen[ev.EventType] = i
		}
	}
	if seen["newest"] >= seen["middle"] || seen["middle"] >= seen["oldest"] {
		t.Errorf("expected newest < middle < oldest in response order; got %+v", seen)
	}
}

// -----------------------------------------------------------------------
// Edge cases
// -----------------------------------------------------------------------

func TestReadAPI_LogTail_LimitClamp(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	// limit=0 -> 400 (TASK test plan explicit).
	status, _ := doJSON(t, http.MethodGet, "http://"+addr+"/v1/keepers-log?limit=0", tok, nil)
	if status != http.StatusBadRequest {
		t.Errorf("limit=0 status = %d, want 400", status)
	}

	// limit=999 -> 200 with at most 200 rows.
	status, body := doJSON(t, http.MethodGet, "http://"+addr+"/v1/keepers-log?limit=999", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("limit=999 status = %d; body = %s", status, body)
	}
	var resp logTailResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Events) > 200 {
		t.Errorf("limit=999 returned %d events; want <= 200", len(resp.Events))
	}
}

func TestReadAPI_GetManifest_NotFound(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	bogus := newUUID(t)
	status, body := doJSON(t, http.MethodGet, "http://"+addr+"/v1/manifests/"+bogus, tok, nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", status, body)
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Error != "not_found" {
		t.Errorf("error = %q, want not_found", envelope.Error)
	}
}

// -----------------------------------------------------------------------
// Negative paths
// -----------------------------------------------------------------------

func TestReadAPI_NoAuthorizationHeader(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	status, body := doJSON(t, http.MethodGet, "http://"+addr+"/v1/keepers-log", "", nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	var envelope struct {
		Error  string `json:"error"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Reason != "missing_token" {
		t.Errorf("reason = %q, want missing_token", envelope.Reason)
	}
}

func TestReadAPI_BadSignature(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	// Mint a token with a *different* key; the binary's verifier will
	// reject signature.
	otherKey, _ := base64Decode(testTokenSigningKeyB64)
	otherKey[0] ^= 0xFF
	ti, err := auth.NewTestIssuer(otherKey, testTokenIssuer, time.Now)
	if err != nil {
		t.Fatalf("NewTestIssuer: %v", err)
	}
	tok, err := ti.Issue(auth.Claim{Subject: "x", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	status, body := doJSON(t, http.MethodGet, "http://"+addr+"/v1/keepers-log", tok, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	var envelope struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Reason != "bad_signature" {
		t.Errorf("reason = %q, want bad_signature", envelope.Reason)
	}
}

func TestReadAPI_ExpiredToken(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	// Build an issuer pegged to the past so exp is already in the past.
	past := time.Now().Add(-time.Hour)
	key, _ := base64Decode(testTokenSigningKeyB64)
	ti, err := auth.NewTestIssuer(key, testTokenIssuer, func() time.Time { return past })
	if err != nil {
		t.Fatalf("NewTestIssuer: %v", err)
	}
	tok, err := ti.Issue(auth.Claim{Subject: "x", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	status, body := doJSON(t, http.MethodGet, "http://"+addr+"/v1/keepers-log", tok, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	var envelope struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Reason != "expired" {
		t.Errorf("reason = %q, want expired", envelope.Reason)
	}
}

func TestReadAPI_BadScope(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok, err := ti.Issue(auth.Claim{Subject: "x", Scope: "weird"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	status, body := doJSON(t, http.MethodGet, "http://"+addr+"/v1/keepers-log", tok, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	var envelope struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envelope.Reason != "bad_scope" {
		t.Errorf("reason = %q, want bad_scope", envelope.Reason)
	}
}

// -----------------------------------------------------------------------
// Security
// -----------------------------------------------------------------------

// TestReadAPI_ScopeIsTokenBound attempts to sneak a `scope=` query param
// past the token — the handler must ignore it and only use the token
// claim. The test mints an agent-scope token then asks for search with a
// bogus `scope=user:foo` query string; the response must still match the
// agent-scope visibility contract.
func TestReadAPI_ScopeIsTokenBound(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, env.agentScope)

	// Build a URL with a scope= query that would, if honoured, elevate
	// visibility to user rows. The handlers never read req.URL query
	// for scope, so the response must match the agent contract.
	reqBody := searchBody{Embedding: queryVec1536(), TopK: 10}
	status, body := doJSON(t, http.MethodPost,
		"http://"+addr+"/v1/search?scope=user:"+env.humanID, tok, reqBody)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body = %s", status, body)
	}
	var resp searchResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range resp.Results {
		if r.Subject == env.subjectTag+"-user" {
			t.Errorf("scope= query override leaked user row into agent scope: %+v", r)
		}
	}
}

// TestReadAPI_ConcurrentScopeIsolation fires two concurrent requests with
// different scopes and confirms neither sees the other's rows. This
// guards against SET LOCAL leaking across pooled sessions.
func TestReadAPI_ConcurrentScopeIsolation(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	userTok := mintToken(t, ti, env.userScope)
	agentTok := mintToken(t, ti, env.agentScope)

	// 20 parallel round-trips per scope so a leak is overwhelmingly
	// likely to surface in at least one pairing.
	const iterations = 20
	var wg sync.WaitGroup
	errs := make(chan error, iterations*2)
	run := func(tok, mustNotSee string) {
		defer wg.Done()
		status, body := doJSON(t, http.MethodPost,
			"http://"+addr+"/v1/search", tok, searchBody{Embedding: queryVec1536(), TopK: 10})
		if status != http.StatusOK {
			errs <- fmt.Errorf("status = %d body = %s", status, body)
			return
		}
		var resp searchResponse
		if err := json.Unmarshal(body, &resp); err != nil {
			errs <- err
			return
		}
		for _, r := range resp.Results {
			if r.Subject == mustNotSee {
				errs <- fmt.Errorf("scope leak: saw %q", mustNotSee)
				return
			}
		}
	}
	for i := 0; i < iterations; i++ {
		wg.Add(2)
		go run(userTok, env.subjectTag+"-agent")
		go run(agentTok, env.subjectTag+"-user")
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

// TestReadAPI_HealthUnauthenticated is the regression guard from the
// TASK test plan: /health must stay open.
func TestReadAPI_HealthUnauthenticated(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	status, body := doJSON(t, http.MethodGet, "http://"+addr+"/health", "", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body = %s", status, body)
	}
}
