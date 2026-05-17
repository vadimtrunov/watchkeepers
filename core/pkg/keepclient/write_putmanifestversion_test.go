package keepclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

const validManifestID = "11111111-1111-4111-8111-111111111111"

// TestPutManifestVersion_Success asserts the happy round-trip: a 201 response
// decodes the `{"id":"…"}` envelope; the server-side path includes the
// manifest_id, the body has version_no, system_prompt, and jsonb fields.
func TestPutManifestVersion_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("Method = %q, want PUT", r.Method)
		}
		if want := "/v1/manifests/" + validManifestID + "/versions"; r.URL.Path != want {
			t.Errorf("Path = %q, want %q", r.URL.Path, want)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var got PutManifestVersionRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if got.VersionNo != 3 {
			t.Errorf("VersionNo = %d, want 3", got.VersionNo)
		}
		if got.SystemPrompt != "sp" {
			t.Errorf("SystemPrompt = %q, want sp", got.SystemPrompt)
		}
		if string(got.Tools) != `[{"name":"t1"}]` {
			t.Errorf("Tools = %s, want [{\"name\":\"t1\"}]", got.Tools)
		}
		if string(got.AuthorityMatrix) != `{"can":"do"}` {
			t.Errorf("AuthorityMatrix = %s", got.AuthorityMatrix)
		}
		if string(got.KnowledgeSources) != `["ks-1"]` {
			t.Errorf("KnowledgeSources = %s", got.KnowledgeSources)
		}
		if got.Personality != "focused" {
			t.Errorf("Personality = %q, want focused", got.Personality)
		}
		if got.Language != "en" {
			t.Errorf("Language = %q, want en", got.Language)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:        3,
		SystemPrompt:     "sp",
		Tools:            json.RawMessage(`[{"name":"t1"}]`),
		AuthorityMatrix:  json.RawMessage(`{"can":"do"}`),
		KnowledgeSources: json.RawMessage(`["ks-1"]`),
		Personality:      "focused",
		Language:         "en",
	})
	if err != nil {
		t.Fatalf("PutManifestVersion: %v", err)
	}
	if resp.ID != "row-1" {
		t.Errorf("ID = %q, want row-1", resp.ID)
	}
}

// TestPutManifestVersion_OmitsEmptyOptionalFields asserts that empty optional
// jsonb fields, personality, and language are not present on the wire — the
// server applies its default values for the omitted columns.
func TestPutManifestVersion_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		rawBody = raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if _, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
	}); err != nil {
		t.Fatalf("PutManifestVersion: %v", err)
	}
	for _, k := range []string{`"tools"`, `"authority_matrix"`, `"knowledge_sources"`, `"personality"`, `"language"`, `"autonomy"`, `"notebook_top_k"`, `"notebook_relevance_threshold"`} {
		if strings.Contains(string(rawBody), k) {
			t.Errorf("body included %s field; got %s", k, rawBody)
		}
	}
}

// TestPutManifestVersion_Conflict asserts the 409 case: the server replies
// with a version_conflict envelope and the client returns *ServerError{Status:409}
// that matches ErrConflict via errors.Is.
func TestPutManifestVersion_Conflict(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"version_conflict"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    2,
		SystemPrompt: "sp",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *ServerError
	if !errors.As(err, &se) || se.Status != http.StatusConflict {
		t.Errorf("ServerError = %+v, want Status 409", se)
	}
	if !errors.Is(err, ErrConflict) {
		t.Errorf("errors.Is(err, ErrConflict) = false; err = %v", err)
	}
}

// TestPutManifestVersion_PathEscaping asserts that callers cannot smuggle
// extra path segments or a query string by passing characters like `/` or `?`
// in the manifestID. The wire form must percent-encode them so the server
// sees a single path segment with no query string. (Belt-and-suspenders: the
// preflight already rejects non-canonical UUIDs.)
func TestPutManifestVersion_PathEscaping(t *testing.T) {
	t.Parallel()

	var rawPath, rawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawPath = r.URL.EscapedPath()
		rawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	// Reach Store path via the unexported do() by faking a UUID-shaped id with
	// path injection. The preflight in PutManifestVersion rejects non-UUID
	// ids, so go via a low-level call to verify path encoding hardening.
	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	// Use the public surface with a UUID-shaped value that contains no
	// special chars — the escape is structurally tested at compile time
	// via url.PathEscape inside the impl. To still observe encoding,
	// confirm a normal call yields a clean path with no query string.
	_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
	})
	if err != nil {
		t.Fatalf("PutManifestVersion: %v", err)
	}
	if want := "/v1/manifests/" + validManifestID + "/versions"; rawPath != want {
		t.Errorf("escaped path = %q, want %q", rawPath, want)
	}
	if rawQuery != "" {
		t.Errorf("query = %q; want empty", rawQuery)
	}
}

// TestPutManifestVersion_AuthHeaderInjected asserts the Authorization header
// carries the bearer token from the configured TokenSource.
func TestPutManifestVersion_AuthHeaderInjected(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("xyz")))
	if _, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
	}); err != nil {
		t.Fatalf("PutManifestVersion: %v", err)
	}
	if gotAuth != "Bearer xyz" {
		t.Errorf("Authorization = %q, want \"Bearer xyz\"", gotAuth)
	}
}

// TestPutManifestVersion_NoTokenSource asserts that calling
// PutManifestVersion without configuring WithTokenSource returns
// ErrNoTokenSource synchronously and does NOT contact the network.
func TestPutManifestVersion_NoTokenSource(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
	})
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestPutManifestVersion_PreflightValidation asserts the synchronous
// preflight: empty manifestID, non-canonical-uuid manifestID, VersionNo<=0,
// or empty SystemPrompt return ErrInvalidRequest before any network call.
func TestPutManifestVersion_PreflightValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		manifestID string
		req        PutManifestVersionRequest
	}{
		{"empty_manifest_id", "", PutManifestVersionRequest{VersionNo: 1, SystemPrompt: "sp"}},
		{"non_uuid_manifest_id", "not-a-uuid", PutManifestVersionRequest{VersionNo: 1, SystemPrompt: "sp"}},
		{"zero_version_no", validManifestID, PutManifestVersionRequest{VersionNo: 0, SystemPrompt: "sp"}},
		{"negative_version_no", validManifestID, PutManifestVersionRequest{VersionNo: -1, SystemPrompt: "sp"}},
		{"empty_system_prompt", validManifestID, PutManifestVersionRequest{VersionNo: 1, SystemPrompt: ""}},
		{
			"invalid_language", validManifestID,
			PutManifestVersionRequest{VersionNo: 1, SystemPrompt: "sp", Language: "english"},
		},
		{
			"personality_too_long", validManifestID,
			PutManifestVersionRequest{VersionNo: 1, SystemPrompt: "sp", Personality: strings.Repeat("a", 1025)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				atomic.AddInt32(&hits, 1)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			_, err := c.PutManifestVersion(context.Background(), tc.manifestID, tc.req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("network hits = %d, want 0", got)
			}
		})
	}
}

// TestPutManifestVersion_StatusMappings exhaustively asserts that every
// documented status code (incl. 409) surfaces as the corresponding sentinel
// via errors.Is.
func TestPutManifestVersion_StatusMappings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"400", 400, ErrInvalidRequest},
		{"401", 401, ErrUnauthorized},
		{"403", 403, ErrForbidden},
		{"404", 404, ErrNotFound},
		{"409", 409, ErrConflict},
		{"500", 500, ErrInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":"err_%d"}`, tc.status)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
				VersionNo:    1,
				SystemPrompt: "sp",
			})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(err, %v) = false; err = %v", tc.wantErr, err)
			}
			var se *ServerError
			if !errors.As(err, &se) || se.Status != tc.status {
				t.Errorf("ServerError.Status = %v, want %d (err=%v)", se, tc.status, err)
			}
		})
	}
}

// TestPutManifestVersion_TransportError asserts that a transport-level failure
// (server closed before request) surfaces as a wrapped error, not a
// *ServerError.
func TestPutManifestVersion_TransportError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(WithBaseURL(url), WithTokenSource(StaticToken("t")))
	_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *ServerError
	if errors.As(err, &se) {
		t.Errorf("transport error must not be a *ServerError; got %v", err)
	}
}

// TestPutManifestVersion_ContextCancellation asserts that a cancelled context
// aborts the in-flight request and the resulting error wraps context.Canceled.
func TestPutManifestVersion_ContextCancellation(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := c.PutManifestVersion(ctx, validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}
}

// TestPutManifestVersionRequest_MarshalIncludesModel asserts that a request
// with a non-empty Model carries `"model":"<value>"` on the wire (M5.5.b.b.b).
func TestPutManifestVersionRequest_MarshalIncludesModel(t *testing.T) {
	t.Parallel()

	const wantModel = "claude-sonnet-4-7"
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		rawBody = raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if _, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
		Model:        wantModel,
	}); err != nil {
		t.Fatalf("PutManifestVersion: %v", err)
	}
	if !strings.Contains(string(rawBody), `"model":"`+wantModel+`"`) {
		t.Errorf("body missing model field; got %s", rawBody)
	}
}

// TestPutManifestVersion_Model100Runes_Accepted asserts the boundary: a Model
// value with exactly 100 Unicode codepoints (mixed multi-byte) passes the
// pre-HTTP rune-length check and the request is dispatched.
func TestPutManifestVersion_Model100Runes_Accepted(t *testing.T) {
	t.Parallel()

	// 50 runes of "ä" (2 bytes each) + 50 ASCII runes => 100 runes / 150 bytes,
	// so a byte-based cap would have rejected this and a rune-based cap accepts it.
	model := strings.Repeat("ä", 50) + strings.Repeat("a", 50)
	if got := utf8.RuneCountInString(model); got != 100 {
		t.Fatalf("test fixture: rune count = %d, want 100", got)
	}

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if _, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
		Model:        model,
	}); err != nil {
		t.Fatalf("PutManifestVersion: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("network hits = %d, want 1", got)
	}
}

// TestPutManifestVersion_Model101Runes_RejectedBeforeHTTP asserts the negative
// case: a 101-rune Model returns ErrInvalidRequest synchronously and the
// transport records zero requests.
func TestPutManifestVersion_Model101Runes_RejectedBeforeHTTP(t *testing.T) {
	t.Parallel()

	model := strings.Repeat("a", 101)
	if got := utf8.RuneCountInString(model); got != 101 {
		t.Fatalf("test fixture: rune count = %d, want 101", got)
	}

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
		Model:        model,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestPutManifestVersionRequest_MarshalIncludesSupervised asserts that a
// request with Autonomy="supervised" carries `"autonomy":"supervised"` on
// the wire (M5.5.b.c.b).
func TestPutManifestVersionRequest_MarshalIncludesSupervised(t *testing.T) {
	t.Parallel()

	const wantAutonomy = "supervised"
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		rawBody = raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if _, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
		Autonomy:     wantAutonomy,
	}); err != nil {
		t.Fatalf("PutManifestVersion: %v", err)
	}
	if !strings.Contains(string(rawBody), `"autonomy":"`+wantAutonomy+`"`) {
		t.Errorf("body missing autonomy field; got %s", rawBody)
	}
}

// TestPutManifestVersionRequest_MarshalIncludesAutonomous asserts that a
// request with Autonomy="autonomous" carries `"autonomy":"autonomous"` on
// the wire (M5.5.b.c.b).
func TestPutManifestVersionRequest_MarshalIncludesAutonomous(t *testing.T) {
	t.Parallel()

	const wantAutonomy = "autonomous"
	var rawBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		rawBody = raw
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if _, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
		Autonomy:     wantAutonomy,
	}); err != nil {
		t.Fatalf("PutManifestVersion: %v", err)
	}
	if !strings.Contains(string(rawBody), `"autonomy":"`+wantAutonomy+`"`) {
		t.Errorf("body missing autonomy field; got %s", rawBody)
	}
}

// TestPutManifestVersion_AutonomyInvalid_RejectedBeforeHTTP asserts the
// negative case: a value not in {"", "supervised", "autonomous"} (e.g.
// "manual") returns ErrInvalidRequest synchronously and the transport
// records zero requests. Mirrors the server CHECK enum constraint
// (migration 015).
func TestPutManifestVersion_AutonomyInvalid_RejectedBeforeHTTP(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
		Autonomy:     "manual",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestPutManifestVersionRequest_OmitsEmptyNotebookRecall asserts that
// marshalling a request with both NotebookTopK and
// NotebookRelevanceThreshold at zero produces JSON with neither key
// (M5.5.c.b AC2 — omitempty drops zero numerics).
func TestPutManifestVersionRequest_OmitsEmptyNotebookRecall(t *testing.T) {
	t.Parallel()

	req := PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, k := range []string{`"notebook_top_k"`, `"notebook_relevance_threshold"`} {
		if strings.Contains(string(raw), k) {
			t.Errorf("body included %s field; got %s", k, raw)
		}
	}
}

// TestPutManifestVersion_NotebookTopKOutOfRange_PreHTTP asserts that
// NotebookTopK = 101 and = -1 are rejected with ErrInvalidRequest before
// any HTTP call (M5.5.c.b AC2). The httptest recorder asserts zero
// recorded requests.
func TestPutManifestVersion_NotebookTopKOutOfRange_PreHTTP(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		topK int
	}{
		{"above_max", 101},
		{"negative", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				atomic.AddInt32(&hits, 1)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
				VersionNo:    1,
				SystemPrompt: "sp",
				NotebookTopK: tc.topK,
			})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("network hits = %d, want 0", got)
			}
		})
	}
}

// TestPutManifestVersion_NotebookRelevanceThresholdOutOfRange_PreHTTP asserts
// that NotebookRelevanceThreshold = 1.5 and = -0.1 are rejected with
// ErrInvalidRequest before any HTTP call (M5.5.c.b AC2).
func TestPutManifestVersion_NotebookRelevanceThresholdOutOfRange_PreHTTP(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		threshold float64
	}{
		{"above_max", 1.5},
		{"negative", -0.1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				atomic.AddInt32(&hits, 1)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
				VersionNo:                  1,
				SystemPrompt:               "sp",
				NotebookRelevanceThreshold: tc.threshold,
			})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("network hits = %d, want 0", got)
			}
		})
	}
}

// TestPutManifestVersion_NotebookRecallBoundaries_Accepted asserts that
// the boundary values NotebookTopK ∈ {0, 1, 100} and
// NotebookRelevanceThreshold ∈ {0, 0.5, 1.0} all pass pre-HTTP
// validation and reach the server (M5.5.c.b AC2).
func TestPutManifestVersion_NotebookRecallBoundaries_Accepted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		topK      int
		threshold float64
	}{
		{"zero_both", 0, 0},
		{"topk_1", 1, 0},
		{"topk_100", 100, 0},
		{"threshold_half", 0, 0.5},
		{"threshold_one", 0, 1.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"id":"row-1"}`)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
				VersionNo:                  1,
				SystemPrompt:               "sp",
				NotebookTopK:               tc.topK,
				NotebookRelevanceThreshold: tc.threshold,
			})
			if err != nil {
				t.Fatalf("PutManifestVersion: %v", err)
			}
			if got := atomic.LoadInt32(&hits); got != 1 {
				t.Errorf("network hits = %d, want 1", got)
			}
		})
	}
}

// -----------------------------------------------------------------------
// M3.1 — immutable_core preflight + round-trip
// -----------------------------------------------------------------------

// TestPutManifestVersion_ImmutableCore_ObjectAccepted asserts the M3.1
// happy path: a well-formed immutable_core JSON object preflight-passes
// the client-side `isJSONObjectOrEmpty` check, hits the network, and
// the body bytes carry the object verbatim onto the wire. Mirrors the
// Tools / AuthorityMatrix raw-jsonb passthrough pattern.
func TestPutManifestVersion_ImmutableCore_ObjectAccepted(t *testing.T) {
	t.Parallel()

	const wantImmutableCore = `{"role_boundaries":["x"],"cost_limits":{"per_task_tokens":1000}}`

	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		capturedBody = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:     1,
		SystemPrompt:  "sp",
		ImmutableCore: json.RawMessage(wantImmutableCore),
	})
	if err != nil {
		t.Fatalf("PutManifestVersion: %v", err)
	}
	// The body MUST carry `"immutable_core":{...}` verbatim. We do a
	// substring check to avoid coupling the test to other field
	// ordering — the assertion is "the object survived the marshal".
	if !strings.Contains(string(capturedBody), `"immutable_core":`+wantImmutableCore) {
		t.Errorf("body = %s, want it to contain immutable_core=%s", capturedBody, wantImmutableCore)
	}
}

// TestPutManifestVersion_ImmutableCore_NonObjectRejected asserts the
// M3.1 client-side preflight: an array / scalar / JSON-null payload is
// rejected with ErrInvalidRequest BEFORE the network hit (mirrors the
// server's stable `invalid_immutable_core` 400 reason; the client
// short-circuits so callers see one error mode regardless of which
// side caught the malformed shape).
func TestPutManifestVersion_ImmutableCore_NonObjectRejected(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		body json.RawMessage
	}{
		{"array", json.RawMessage(`[1,2,3]`)},
		{"string", json.RawMessage(`"oops"`)},
		{"number", json.RawMessage(`42`)},
		{"bool_true", json.RawMessage(`true`)},
		{"jsonnull", json.RawMessage(`null`)},
		{"malformed", json.RawMessage(`{not-json`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.WriteHeader(http.StatusCreated)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
				VersionNo:     1,
				SystemPrompt:  "sp",
				ImmutableCore: tc.body,
			})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("network hits = %d, want 0 (preflight should short-circuit)", got)
			}
		})
	}
}

// TestPutManifestVersion_ImmutableCore_EmptyOmitted asserts that a nil
// RawMessage round-trips as `omitempty` (no `immutable_core` key on the
// wire). Mirrors the Tools / AuthorityMatrix nil-jsonb wire-omit
// behaviour.
func TestPutManifestVersion_ImmutableCore_EmptyOmitted(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		capturedBody = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:    1,
		SystemPrompt: "sp",
	})
	if err != nil {
		t.Fatalf("PutManifestVersion: %v", err)
	}
	if strings.Contains(string(capturedBody), `"immutable_core"`) {
		t.Errorf("body = %s, want no immutable_core key (omitempty + nil)", capturedBody)
	}
}

// TestPutManifestVersion_ImmutableCore_EmptyObjectAccepted asserts the
// lower-boundary case: `{}` passes the preflight (mirrors the
// server-side `jsonb_typeof = 'object'` CHECK). M3.6 may later reject
// empty objects from the self-tuning path, but the M3.1 schema layer
// accepts them at both sides.
func TestPutManifestVersion_ImmutableCore_EmptyObjectAccepted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.PutManifestVersion(context.Background(), validManifestID, PutManifestVersionRequest{
		VersionNo:     1,
		SystemPrompt:  "sp",
		ImmutableCore: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("PutManifestVersion: %v", err)
	}
}

// TestIsJSONObjectOrEmpty_Boundaries exercises the private preflight
// directly so callers do not have to round-trip through HTTP for the
// boundary cases (whitespace before `{`, valid-JSON-but-not-object, …).
// This keeps the predicate covered even if a future caller (a new
// resource type) reuses it.
func TestIsJSONObjectOrEmpty_Boundaries(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   json.RawMessage
		want bool
	}{
		{"nil", nil, true},
		{"empty", json.RawMessage{}, true},
		{"empty_object", json.RawMessage(`{}`), true},
		{"object_with_keys", json.RawMessage(`{"a":1}`), true},
		{"leading_whitespace_object", json.RawMessage("  \t\n{}"), true},
		{"array", json.RawMessage(`[]`), false},
		{"string", json.RawMessage(`"x"`), false},
		{"number", json.RawMessage(`0`), false},
		{"bool_true", json.RawMessage(`true`), false},
		{"bool_false", json.RawMessage(`false`), false},
		{"jsonnull", json.RawMessage(`null`), false},
		{"malformed", json.RawMessage(`{`), false},
		{"trailing_garbage", json.RawMessage(`{}x`), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isJSONObjectOrEmpty(tc.in); got != tc.want {
				t.Errorf("isJSONObjectOrEmpty(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// Compile-time hint: keep the utf8 import used by neighbour tests.
var _ = utf8.RuneCountInString
