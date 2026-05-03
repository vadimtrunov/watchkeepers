//go:build integration

// Integration tests for the Keep write API (M2.7.d). Requires a Postgres 16
// with pgvector reachable via KEEP_INTEGRATION_DB_URL and with migrations
// 001..008 applied (CI wires this through the Keep Integration CI job).
// Reuses the helpers from read_integration_test.go (same package
// main_test): newTestEnv, bootKeep, doJSON, issuerForTest, mintToken,
// newUUID, queryVec1536, pgxBeginAndRun.
//
// Run locally with:
//
//	KEEP_INTEGRATION_DB_URL=postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable \
//	  go test -tags=integration -v -run 'TestWriteAPI_' ./core/cmd/keep/...
package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// writeIDResponse matches the `{"id":"<uuid>"}` envelope that every write
// endpoint returns on 201.
type writeIDResponse struct {
	ID string `json:"id"`
}

// storeRequestBody is the POST /v1/knowledge-chunks request shape;
// defined here so the test stays a black-box client.
type storeRequestBody struct {
	Subject   string    `json:"subject,omitempty"`
	Content   string    `json:"content"`
	Embedding []float32 `json:"embedding"`
}

type logAppendRequestBody struct {
	EventType     string          `json:"event_type"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

type putManifestVersionRequestBody struct {
	VersionNo        int             `json:"version_no"`
	SystemPrompt     string          `json:"system_prompt"`
	Tools            json.RawMessage `json:"tools,omitempty"`
	AuthorityMatrix  json.RawMessage `json:"authority_matrix,omitempty"`
	KnowledgeSources json.RawMessage `json:"knowledge_sources,omitempty"`
	Personality      string          `json:"personality,omitempty"`
	Language         string          `json:"language,omitempty"`
}

// keepersLogRow models the JSON shape returned by GET /v1/keepers-log.
// Pointer fields mirror the nullable columns on the wire.
type keepersLogRow struct {
	ID                 string          `json:"id"`
	EventType          string          `json:"event_type"`
	CorrelationID      *string         `json:"correlation_id,omitempty"`
	ActorWatchkeeperID *string         `json:"actor_watchkeeper_id,omitempty"`
	ActorHumanID       *string         `json:"actor_human_id,omitempty"`
	Payload            json.RawMessage `json:"payload"`
	CreatedAt          time.Time       `json:"created_at"`
}

type keepersLogTailResponse struct {
	Events []keepersLogRow `json:"events"`
}

// -----------------------------------------------------------------------
// Happy paths
// -----------------------------------------------------------------------

// TestWriteAPI_Store_AgentScopeVisibility — a chunk stored under
// agent:<wk> is visible to a subsequent /v1/search under the same scope,
// and invisible to a search under a distinct user:<u> scope.
func TestWriteAPI_Store_AgentScopeVisibility(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	agentTok := mintToken(t, ti, env.agentScope)
	userTok := mintToken(t, ti, env.userScope)

	marker := env.subjectTag + "-write-agent"
	storeStatus, storeBody := doJSON(t, http.MethodPost,
		"http://"+addr+"/v1/knowledge-chunks", agentTok, storeRequestBody{
			Subject:   marker,
			Content:   "fresh agent content",
			Embedding: queryVec1536(),
		})
	if storeStatus != http.StatusCreated {
		t.Fatalf("store status = %d; body = %s", storeStatus, storeBody)
	}
	var created writeIDResponse
	if err := json.Unmarshal(storeBody, &created); err != nil {
		t.Fatalf("decode store response: %v", err)
	}
	if created.ID == "" {
		t.Fatal("store returned empty id")
	}

	// Search under the same agent scope — must see the marker.
	searchStatus, searchRaw := doJSON(t, http.MethodPost,
		"http://"+addr+"/v1/search", agentTok, searchBody{
			Embedding: queryVec1536(), TopK: 20,
		})
	if searchStatus != http.StatusOK {
		t.Fatalf("agent search status = %d; body = %s", searchStatus, searchRaw)
	}
	var resp searchResponse
	if err := json.Unmarshal(searchRaw, &resp); err != nil {
		t.Fatalf("decode agent search: %v", err)
	}
	if !containsSubject(resp, marker) {
		t.Errorf("agent search did not return marker %q; results=%+v", marker, resp.Results)
	}

	// Search under a different user scope — must NOT see the marker.
	_, userSearchRaw := doJSON(t, http.MethodPost,
		"http://"+addr+"/v1/search", userTok, searchBody{
			Embedding: queryVec1536(), TopK: 20,
		})
	var userResp searchResponse
	if err := json.Unmarshal(userSearchRaw, &userResp); err != nil {
		t.Fatalf("decode user search: %v", err)
	}
	if containsSubject(userResp, marker) {
		t.Errorf("user scope leaked agent-scoped marker %q", marker)
	}
}

// containsSubject reports whether any result row has the given subject.
func containsSubject(r searchResponse, subject string) bool {
	for _, row := range r.Results {
		if row.Subject == subject {
			return true
		}
	}
	return false
}

// TestWriteAPI_LogAppend_AgentActor — log_append under agent:<wk> stamps
// actor_watchkeeper_id and leaves actor_human_id NULL.
func TestWriteAPI_LogAppend_AgentActor(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, env.agentScope)

	payload := fmt.Sprintf(`{"tag":%q,"kind":"write-agent"}`, env.subjectTag)
	_, body := doJSON(t, http.MethodPost,
		"http://"+addr+"/v1/keepers-log", tok, logAppendRequestBody{
			EventType: env.subjectTag + "-write-agent",
			Payload:   json.RawMessage(payload),
		})
	var created writeIDResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.ID == "" {
		t.Fatalf("empty id; body=%s", body)
	}

	ev := fetchLogEvent(t, addr, tok, created.ID)
	if ev.ActorWatchkeeperID == nil || *ev.ActorWatchkeeperID != env.watchkeeperID {
		t.Errorf("actor_watchkeeper_id = %v, want %q", ev.ActorWatchkeeperID, env.watchkeeperID)
	}
	if ev.ActorHumanID != nil {
		t.Errorf("actor_human_id = %v, want NULL", ev.ActorHumanID)
	}
}

// TestWriteAPI_LogAppend_UserActor — log_append under user:<u> stamps
// actor_human_id and leaves actor_watchkeeper_id NULL.
func TestWriteAPI_LogAppend_UserActor(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, env.userScope)

	payload := fmt.Sprintf(`{"tag":%q,"kind":"write-user"}`, env.subjectTag)
	_, body := doJSON(t, http.MethodPost,
		"http://"+addr+"/v1/keepers-log", tok, logAppendRequestBody{
			EventType: env.subjectTag + "-write-user",
			Payload:   json.RawMessage(payload),
		})
	var created writeIDResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v", err)
	}

	ev := fetchLogEvent(t, addr, tok, created.ID)
	if ev.ActorHumanID == nil || *ev.ActorHumanID != env.humanID {
		t.Errorf("actor_human_id = %v, want %q", ev.ActorHumanID, env.humanID)
	}
	if ev.ActorWatchkeeperID != nil {
		t.Errorf("actor_watchkeeper_id = %v, want NULL", ev.ActorWatchkeeperID)
	}
}

// fetchLogEvent locates a previously-appended event by id via GET
// /v1/keepers-log. Iterates the tail (up to 200) since there is no
// dedicated by-id read on the current HTTP surface. Used by the happy-
// path actor assertions.
func fetchLogEvent(t *testing.T, addr, tok, id string) keepersLogRow {
	t.Helper()
	status, raw := doJSON(t, http.MethodGet,
		"http://"+addr+"/v1/keepers-log?limit=200", tok, nil)
	if status != http.StatusOK {
		t.Fatalf("log tail status = %d; body = %s", status, raw)
	}
	var resp keepersLogTailResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode log tail: %v", err)
	}
	for _, ev := range resp.Events {
		if ev.ID == id {
			return ev
		}
	}
	t.Fatalf("event id %s not found in tail (first %d events)", id, len(resp.Events))
	return keepersLogRow{} // unreachable
}

// TestWriteAPI_PutManifestVersion_NewVersion — insert a version_no > the
// seeded max; GET returns it as latest.
func TestWriteAPI_PutManifestVersion_NewVersion(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	// Seeded versions are 1 and 2 (see read_integration_test seed).
	_, body := doJSON(t, http.MethodPut,
		"http://"+addr+"/v1/manifests/"+env.manifestID+"/versions", tok,
		putManifestVersionRequestBody{
			VersionNo:    3,
			SystemPrompt: "v3 prompt (" + env.subjectTag + ")",
		})
	var created writeIDResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if created.ID == "" {
		t.Fatalf("empty id; body=%s", body)
	}

	// GET the latest — must be the new v3.
	status, manifestRaw := doJSON(t, http.MethodGet,
		"http://"+addr+"/v1/manifests/"+env.manifestID, tok, nil)
	if status != http.StatusOK {
		t.Fatalf("get manifest status = %d; body = %s", status, manifestRaw)
	}
	var resp manifestResponse
	if err := json.Unmarshal(manifestRaw, &resp); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if resp.VersionNo != 3 {
		t.Errorf("version_no = %d, want 3 (latest)", resp.VersionNo)
	}
	if resp.ID != created.ID {
		t.Errorf("id = %s, want %s", resp.ID, created.ID)
	}
}

// TestWriteAPI_PutManifestVersion_OptionalFields — verify personality +
// language round-trip via GET.
func TestWriteAPI_PutManifestVersion_OptionalFields(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	// Use a high version_no to avoid collisions with the seed.
	_, body := doJSON(t, http.MethodPut,
		"http://"+addr+"/v1/manifests/"+env.manifestID+"/versions", tok,
		putManifestVersionRequestBody{
			VersionNo:    10,
			SystemPrompt: "curious v10 prompt",
			Personality:  "curious",
			Language:     "fr",
		})
	var created writeIDResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	if created.ID == "" {
		t.Fatalf("empty id; body=%s", body)
	}

	// GET the latest — must carry the optional fields.
	type fullManifest struct {
		ID           string `json:"id"`
		VersionNo    int    `json:"version_no"`
		Personality  string `json:"personality"`
		Language     string `json:"language"`
		SystemPrompt string `json:"system_prompt"`
	}
	status, manifestRaw := doJSON(t, http.MethodGet,
		"http://"+addr+"/v1/manifests/"+env.manifestID, tok, nil)
	if status != http.StatusOK {
		t.Fatalf("get status = %d; body = %s", status, manifestRaw)
	}
	var got fullManifest
	if err := json.Unmarshal(manifestRaw, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Personality != "curious" {
		t.Errorf("personality = %q, want curious", got.Personality)
	}
	if got.Language != "fr" {
		t.Errorf("language = %q, want fr", got.Language)
	}
	if got.VersionNo != 10 {
		t.Errorf("version_no = %d, want 10", got.VersionNo)
	}
}

// -----------------------------------------------------------------------
// Negative / security
// -----------------------------------------------------------------------

// TestWriteAPI_PutManifestVersion_DuplicateRejected — a duplicate
// (manifest_id, version_no) returns 409 version_conflict and the existing
// row stays unchanged.
func TestWriteAPI_PutManifestVersion_DuplicateRejected(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	// Seed has version_no=2 already; try to insert another v2.
	status, body := doJSON(t, http.MethodPut,
		"http://"+addr+"/v1/manifests/"+env.manifestID+"/versions", tok,
		putManifestVersionRequestBody{
			VersionNo:    2,
			SystemPrompt: "duplicate attempt",
		})
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", status, body)
	}
	var envErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envErr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envErr.Error != "version_conflict" {
		t.Errorf("error = %q, want version_conflict", envErr.Error)
	}
	// Raw Postgres text must NOT leak.
	for _, forbidden := range []string{"duplicate", "manifest_version_", "already exists"} {
		if strings.Contains(string(body), forbidden) {
			t.Errorf("response body leaked %q: %s", forbidden, body)
		}
	}

	// Confirm the original v2 row is still intact (the fixture's
	// manifest_version id equals env.manifestVersionID with system_prompt
	// 'v2 prompt').
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var prompt string
	if err := env.pool.QueryRow(ctx, `
        SELECT system_prompt FROM watchkeeper.manifest_version
        WHERE id = $1
    `, env.manifestVersionID).Scan(&prompt); err != nil {
		t.Fatalf("query seeded v2: %v", err)
	}
	if prompt != "v2 prompt" {
		t.Errorf("v2 system_prompt = %q, want 'v2 prompt' (conflict must not overwrite)", prompt)
	}
}

// TestWriteAPI_OversizedBody — every write endpoint rejects 1 MiB + 1
// bodies with 413.
func TestWriteAPI_OversizedBody(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	pad := strings.Repeat("a", (1<<20)+16)
	oversized := `{"event_type":"x","pad":"` + pad + `"}`

	cases := []struct {
		name, method, url string
	}{
		{"store", http.MethodPost, "http://" + addr + "/v1/knowledge-chunks"},
		{"log", http.MethodPost, "http://" + addr + "/v1/keepers-log"},
		{
			"put_manifest", http.MethodPut,
			"http://" + addr + "/v1/manifests/" + env.manifestID + "/versions",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(context.Background(), tc.method, tc.url,
				strings.NewReader(oversized))
			if err != nil {
				t.Fatalf("build req: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("do: %v", err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusRequestEntityTooLarge {
				t.Errorf("status = %d, want 413; body = %s", resp.StatusCode, body)
			}
		})
	}
}

// TestWriteAPI_MissingAuthorization — every write endpoint returns 401
// missing_token without a header.
func TestWriteAPI_MissingAuthorization(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	cases := []struct {
		name, method, url string
		body              any
	}{
		{
			"store", http.MethodPost, "http://" + addr + "/v1/knowledge-chunks",
			storeRequestBody{Content: "x", Embedding: []float32{0.1}},
		},
		{
			"log", http.MethodPost, "http://" + addr + "/v1/keepers-log",
			logAppendRequestBody{EventType: "x"},
		},
		{
			"put_manifest", http.MethodPut,
			"http://" + addr + "/v1/manifests/" + env.manifestID + "/versions",
			putManifestVersionRequestBody{VersionNo: 99, SystemPrompt: "x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doJSON(t, tc.method, tc.url, "", tc.body)
			if status != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401; body = %s", status, body)
			}
			var envErr struct {
				Error, Reason string
			}
			if err := json.Unmarshal(body, &envErr); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if envErr.Reason != "missing_token" {
				t.Errorf("reason = %q, want missing_token", envErr.Reason)
			}
		})
	}
}

// TestWriteAPI_LogAppend_QueryActorIgnored — a `actor_watchkeeper_id=…`
// query string MUST NOT change the stamped actor; the token's scope is
// the only input.
func TestWriteAPI_LogAppend_QueryActorIgnored(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, env.agentScope)

	other := newUUID(t) // a bogus watchkeeper id the attacker wants to forge
	url := "http://" + addr + "/v1/keepers-log?actor_watchkeeper_id=" + other
	_, body := doJSON(t, http.MethodPost, url, tok, logAppendRequestBody{
		EventType: env.subjectTag + "-qactor",
		Payload:   json.RawMessage(fmt.Sprintf(`{"tag":%q}`, env.subjectTag)),
	})
	var created writeIDResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}

	ev := fetchLogEvent(t, addr, tok, created.ID)
	if ev.ActorWatchkeeperID == nil {
		t.Fatal("actor_watchkeeper_id is NULL; want token-derived value")
	}
	if *ev.ActorWatchkeeperID == other {
		t.Errorf("query string overrode actor: got %q", *ev.ActorWatchkeeperID)
	}
	if *ev.ActorWatchkeeperID != env.watchkeeperID {
		t.Errorf("actor = %q, want token-derived %q", *ev.ActorWatchkeeperID, env.watchkeeperID)
	}
}

// TestWriteAPI_Store_ScopeBodyRejected — a `"scope":"org"` field in the
// body MUST be rejected by DisallowUnknownFields (or, as a backstop, the
// inserted row's scope must equal the token's claim). Either outcome is
// acceptable security-wise — the test asserts the first (rejection) per
// the AC. The WITH CHECK policy from migration 005 is the final backstop.
func TestWriteAPI_Store_ScopeBodyRejected(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, env.agentScope)

	bodyJSON := `{"content":"x","embedding":[0.1],"scope":"org"}`

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		"http://"+addr+"/v1/knowledge-chunks", strings.NewReader(bodyJSON))
	if err != nil {
		t.Fatalf("build req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (DisallowUnknownFields); body = %s", resp.StatusCode, body)
	}
}

// doJSONNoFatal executes an HTTP request like doJSON but returns
// (status, body, err) instead of calling t.Fatalf. Safe to call from
// goroutines; callers must forward non-nil err to an error channel and
// then call wg.Done.
func doJSONNoFatal(method, url, authTok string, body any) (int, []byte, error) {
	var r io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal body: %w", err)
		}
		r = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, url, r)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if authTok != "" {
		req.Header.Set("Authorization", "Bearer "+authTok)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("http do: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, fmt.Errorf("read body: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

// TestWriteAPI_ConcurrentScopeIsolation — 20 concurrent stores + logs
// under user:A and agent:B must never cross-contaminate. Any such leak
// would prove SET LOCAL bleeding across pooled sessions.
func TestWriteAPI_ConcurrentScopeIsolation(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	userTok := mintToken(t, ti, env.userScope)
	agentTok := mintToken(t, ti, env.agentScope)

	const iterations = 20
	var wg sync.WaitGroup
	errs := make(chan error, iterations*4)

	storeRun := func(tok, tag string) {
		defer wg.Done()
		status, body, err := doJSONNoFatal(http.MethodPost,
			"http://"+addr+"/v1/knowledge-chunks", tok,
			storeRequestBody{Subject: tag, Content: "c", Embedding: queryVec1536()})
		if err != nil {
			errs <- fmt.Errorf("store transport error: %w", err)
			return
		}
		if status != http.StatusCreated {
			errs <- fmt.Errorf("store status = %d body = %s", status, body)
		}
	}
	logRun := func(tok, tag string) {
		defer wg.Done()
		status, body, err := doJSONNoFatal(http.MethodPost,
			"http://"+addr+"/v1/keepers-log", tok,
			logAppendRequestBody{EventType: tag, Payload: json.RawMessage(`{"x":1}`)})
		if err != nil {
			errs <- fmt.Errorf("log transport error: %w", err)
			return
		}
		if status != http.StatusCreated {
			errs <- fmt.Errorf("log status = %d body = %s", status, body)
		}
	}

	for i := 0; i < iterations; i++ {
		wg.Add(4)
		go storeRun(userTok, env.subjectTag+"-cu")
		go storeRun(agentTok, env.subjectTag+"-ca")
		go logRun(userTok, env.subjectTag+"-clu")
		go logRun(agentTok, env.subjectTag+"-cla")
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// After the storm, verify row-level scope integrity: every
	// subject-prefix-"cu" row must carry scope=userScope; every "-ca" row
	// must carry scope=agentScope. A SET LOCAL leak would yield the
	// opposite for some fraction of rows.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pgxBeginAndRun(ctx, env.pool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
            SELECT subject, scope FROM watchkeeper.knowledge_chunk
            WHERE subject LIKE $1
        `, env.subjectTag+"-c%")
		if err != nil {
			return err
		}
		defer rows.Close()
		seen := 0
		for rows.Next() {
			var subject, scope string
			if err := rows.Scan(&subject, &scope); err != nil {
				return err
			}
			seen++
			want := ""
			switch {
			case strings.HasSuffix(subject, "-cu"):
				want = env.userScope
			case strings.HasSuffix(subject, "-ca"):
				want = env.agentScope
			}
			if want != "" && scope != want {
				return fmt.Errorf("subject %q stored with scope %q, want %q", subject, scope, want)
			}
		}
		if seen < 2*iterations {
			return fmt.Errorf("saw %d concurrent-store rows, want >= %d", seen, 2*iterations)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("scope audit: %v", err)
	}
}

// -----------------------------------------------------------------------
// Regression
// -----------------------------------------------------------------------

// TestWriteAPI_PutManifestVersion_LanguageBCP47Variants — every accepted
// BCP 47-lite shape (`en`, `en-US`, `eng`, `pt-BR`) plus the all-NULL case
// inserts cleanly under the migration 010 CHECK constraint. Each created
// manifest_version row is removed via t.Cleanup so the seed stays stable.
func TestWriteAPI_PutManifestVersion_LanguageBCP47Variants(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	// Each variant gets a distinct version_no above the seed (1 and 2) so
	// the inserts never collide on the unique (manifest_id, version_no)
	// index. The "null" case asserts the empty-string round-trips as SQL
	// NULL (the existing happy path) under the new constraints.
	cases := []struct {
		name      string
		versionNo int
		language  string
	}{
		{"en", 100, "en"},
		{"en_us", 101, "en-US"},
		{"eng", 102, "eng"},
		{"pt_br", 103, "pt-BR"},
		{"null", 104, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body := doJSON(t, http.MethodPut,
				"http://"+addr+"/v1/manifests/"+env.manifestID+"/versions", tok,
				putManifestVersionRequestBody{
					VersionNo:    tc.versionNo,
					SystemPrompt: "bcp47 " + tc.name,
					Language:     tc.language,
				})
			if status != http.StatusCreated {
				t.Fatalf("status = %d, want 201; body = %s", status, body)
			}
			var created writeIDResponse
			if err := json.Unmarshal(body, &created); err != nil {
				t.Fatalf("decode: %v; body=%s", err, body)
			}
			if created.ID == "" {
				t.Fatalf("empty id; body=%s", body)
			}
			id := created.ID
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := env.pool.Exec(ctx,
					`DELETE FROM watchkeeper.manifest_version WHERE id = $1::uuid`, id); err != nil {
					t.Logf("cleanup manifest_version %s: %v", id, err)
				}
			})
		})
	}
}

// TestWriteAPI_PutManifestVersion_InvalidLanguage — a `language` field that
// fails the BCP 47-lite shape (`english`) must surface as 400
// invalid_language from the handler, never reaching the SQL CHECK as a 500.
func TestWriteAPI_PutManifestVersion_InvalidLanguage(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	status, body := doJSON(t, http.MethodPut,
		"http://"+addr+"/v1/manifests/"+env.manifestID+"/versions", tok,
		putManifestVersionRequestBody{
			VersionNo:    200,
			SystemPrompt: "invalid lang",
			Language:     "english",
		})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", status, body)
	}
	var envErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envErr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envErr.Error != "invalid_language" {
		t.Errorf("error = %q, want invalid_language", envErr.Error)
	}
}

// TestWriteAPI_PutManifestVersion_PersonalityTooLong — a personality
// payload exceeding 1024 Unicode codepoints must surface as 400
// personality_too_long from the handler before hitting the SQL CHECK.
func TestWriteAPI_PutManifestVersion_PersonalityTooLong(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	ti := issuerForTest(t)
	tok := mintToken(t, ti, "org")

	status, body := doJSON(t, http.MethodPut,
		"http://"+addr+"/v1/manifests/"+env.manifestID+"/versions", tok,
		putManifestVersionRequestBody{
			VersionNo:    201,
			SystemPrompt: "personality cap",
			Personality:  strings.Repeat("a", 1025),
		})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", status, body)
	}
	var envErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &envErr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envErr.Error != "personality_too_long" {
		t.Errorf("error = %q, want personality_too_long", envErr.Error)
	}
}

// TestWriteAPI_HealthStillOpen — adding write routes must not affect
// /health. Regression guard per the TASK test plan.
func TestWriteAPI_HealthStillOpen(t *testing.T) {
	env := newTestEnv(t)
	addr, teardown := bootKeep(t, env)
	defer teardown()

	status, body := doJSON(t, http.MethodGet, "http://"+addr+"/health", "", nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d; body = %s", status, body)
	}
	if strings.TrimSpace(string(body)) != `{"status":"ok"}` {
		t.Errorf("body = %q", body)
	}
}
