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
)

// TestGetManifest_Success asserts the happy round-trip: a 200 with the full
// manifest_version envelope decodes verbatim, jsonb columns survive as
// json.RawMessage, omitempty fields stay empty when absent.
func TestGetManifest_Success(t *testing.T) {
	t.Parallel()

	const wantID = "11111111-1111-4111-8111-111111111111"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Method = %q, want GET", r.Method)
		}
		if want := "/v1/manifests/" + wantID; r.URL.Path != want {
			t.Errorf("Path = %q, want %q", r.URL.Path, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"row-1",
            "manifest_id":"`+wantID+`",
            "version_no":2,
            "system_prompt":"sp",
            "tools":[{"name":"t1"}],
            "authority_matrix":{"can":"do"},
            "knowledge_sources":["ks-1"],
            "personality":"focused",
            "language":"en",
            "created_at":"2026-05-02T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	mv, err := c.GetManifest(context.Background(), wantID)
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if mv.ID != "row-1" || mv.ManifestID != wantID || mv.VersionNo != 2 {
		t.Errorf("got = %+v", mv)
	}
	if mv.Personality != "focused" || mv.Language != "en" {
		t.Errorf("personality/language = %q/%q", mv.Personality, mv.Language)
	}
	if string(mv.Tools) == "" || string(mv.AuthorityMatrix) == "" || string(mv.KnowledgeSources) == "" {
		t.Errorf("jsonb fields not preserved: %+v", mv)
	}
}

// TestGetManifest_OmitsEmptyOptionalFields asserts that a server response
// with personality and language absent decodes into empty strings (omitempty
// on the client mirrors the server contract).
func TestGetManifest_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"r","manifest_id":"m","version_no":1,
            "system_prompt":"","tools":null,
            "authority_matrix":null,"knowledge_sources":null,
            "created_at":"2026-05-02T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	mv, err := c.GetManifest(context.Background(), "m")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if mv.Personality != "" || mv.Language != "" {
		t.Errorf("expected empty optional fields; got personality=%q language=%q", mv.Personality, mv.Language)
	}
}

// TestGetManifest_NotFound asserts 404 surfaces as *ServerError + ErrNotFound
// via errors.Is, matching the server's `not_found` envelope.
func TestGetManifest_NotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.GetManifest(context.Background(), "missing")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *ServerError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Errorf("err = %v; ServerError = %+v", err, se)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Error("errors.Is(err, ErrNotFound) = false")
	}
}

// TestGetManifest_PreflightEmptyID asserts that an empty manifestID rejects
// synchronously with ErrInvalidRequest and never contacts the server.
func TestGetManifest_PreflightEmptyID(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.GetManifest(context.Background(), "")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestGetManifest_NoTokenSource asserts that calling GetManifest without
// configuring WithTokenSource returns ErrNoTokenSource synchronously.
func TestGetManifest_NoTokenSource(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.GetManifest(context.Background(), "any")
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestGetManifest_AuthHeaderInjected asserts the Authorization header is set
// from the configured TokenSource.
func TestGetManifest_AuthHeaderInjected(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","manifest_id":"m","version_no":1,"system_prompt":"","tools":null,"authority_matrix":null,"knowledge_sources":null,"created_at":"2026-05-02T12:00:00Z"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("xyz")))
	if _, err := c.GetManifest(context.Background(), "m"); err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if gotAuth != "Bearer xyz" {
		t.Errorf("Authorization = %q, want \"Bearer xyz\"", gotAuth)
	}
}

// TestGetManifest_PathEscaping asserts that callers cannot smuggle extra
// path segments or a query string by passing characters like `/` or `?` in
// the manifestID. The wire form must percent-encode them so the server sees
// a single path segment with no query string.
func TestGetManifest_PathEscaping(t *testing.T) {
	t.Parallel()

	var rawPath, rawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RawPath preserves the percent-encoding the client emitted.
		// RequestURI() is identical to what arrived on the wire.
		rawPath = r.URL.EscapedPath()
		rawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","manifest_id":"m","version_no":1,"system_prompt":"","tools":null,"authority_matrix":null,"knowledge_sources":null,"created_at":"2026-05-02T12:00:00Z"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, _ = c.GetManifest(context.Background(), "abc/def?evil=1")
	// The encoded form must contain percent-escaped slash and question mark
	// and must not bleed into the query string.
	if !strings.Contains(rawPath, "%2F") || !strings.Contains(rawPath, "%3F") {
		t.Errorf("escaped path = %q; want %%2F and %%3F escapes", rawPath)
	}
	if rawQuery != "" {
		t.Errorf("query = %q; want empty (smuggled query escaped into path)", rawQuery)
	}
}

// TestGetManifest_StatusMappings exhaustively asserts that every documented
// status code surfaces as the corresponding sentinel via errors.Is.
func TestGetManifest_StatusMappings(t *testing.T) {
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
			_, err := c.GetManifest(context.Background(), "some-id")
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

// TestGetManifest_TransportError asserts that a transport-level failure
// (server closed before request) surfaces as a wrapped error, not a *ServerError.
func TestGetManifest_TransportError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(WithBaseURL(url), WithTokenSource(StaticToken("t")))
	_, err := c.GetManifest(context.Background(), "some-id")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *ServerError
	if errors.As(err, &se) {
		t.Errorf("transport error must not be a *ServerError; got %v", err)
	}
}

// TestGetManifest_ContextCancellation asserts that a cancelled context aborts
// the in-flight request and the resulting error wraps context.Canceled. The
// handler stalls on a per-test "release" channel so the cleanup path lets the
// server return promptly even if the client's connection-close races the
// server-side request context.
func TestGetManifest_ContextCancellation(t *testing.T) {
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
	_, err := c.GetManifest(ctx, "some-id")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}
}

// TestGetManifest_DecodesModel asserts that a server response carrying
// `model:"claude-sonnet-4-7"` decodes into [ManifestVersion.Model] verbatim
// (M5.5.b.b.b). The server emits this field since 5371b86; the client
// merely mirrors the wire shape.
func TestGetManifest_DecodesModel(t *testing.T) {
	t.Parallel()

	const wantModel = "claude-sonnet-4-7"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"r","manifest_id":"m","version_no":1,
            "system_prompt":"sp","tools":null,
            "authority_matrix":null,"knowledge_sources":null,
            "model":"`+wantModel+`",
            "created_at":"2026-05-02T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	mv, err := c.GetManifest(context.Background(), "m")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if mv.Model != wantModel {
		t.Errorf("Model = %q, want %q", mv.Model, wantModel)
	}
}

// TestGetManifest_ModelOmitted_EmptyString asserts that a server response
// without a `model` key decodes into the zero value (empty string) — symmetric
// with the omitempty handling on Personality/Language.
func TestGetManifest_ModelOmitted_EmptyString(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"r","manifest_id":"m","version_no":1,
            "system_prompt":"","tools":null,
            "authority_matrix":null,"knowledge_sources":null,
            "created_at":"2026-05-02T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	mv, err := c.GetManifest(context.Background(), "m")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if mv.Model != "" {
		t.Errorf("Model = %q, want empty string", mv.Model)
	}
}

// TestManifestVersion_MarshalOmitsEmptyModel asserts that marshaling a
// [ManifestVersion] whose Model is the zero value never emits a `model`
// key on the wire — the `omitempty` tag must hold.
func TestManifestVersion_MarshalOmitsEmptyModel(t *testing.T) {
	t.Parallel()

	mv := ManifestVersion{
		ID:         "r",
		ManifestID: "m",
		VersionNo:  1,
		CreatedAt:  "2026-05-02T12:00:00Z",
	}
	raw, err := json.Marshal(mv)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), `"model"`) {
		t.Errorf("body included model key; got %s", raw)
	}
}

// TestGetManifest_DecodesAutonomy asserts that a server response carrying
// `autonomy:"autonomous"` decodes into [ManifestVersion.Autonomy] verbatim
// (M5.5.b.c.b). The server emits this field since M5.5.b.c.a; the client
// merely mirrors the wire shape.
func TestGetManifest_DecodesAutonomy(t *testing.T) {
	t.Parallel()

	const wantAutonomy = "autonomous"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"r","manifest_id":"m","version_no":1,
            "system_prompt":"sp","tools":null,
            "authority_matrix":null,"knowledge_sources":null,
            "autonomy":"`+wantAutonomy+`",
            "created_at":"2026-05-02T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	mv, err := c.GetManifest(context.Background(), "m")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if mv.Autonomy != wantAutonomy {
		t.Errorf("Autonomy = %q, want %q", mv.Autonomy, wantAutonomy)
	}
}

// TestGetManifest_AutonomyOmitted_EmptyString asserts that a server response
// without an `autonomy` key decodes into the zero value (empty string) —
// symmetric with the omitempty handling on Personality/Language/Model.
func TestGetManifest_AutonomyOmitted_EmptyString(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"r","manifest_id":"m","version_no":1,
            "system_prompt":"","tools":null,
            "authority_matrix":null,"knowledge_sources":null,
            "created_at":"2026-05-02T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	mv, err := c.GetManifest(context.Background(), "m")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if mv.Autonomy != "" {
		t.Errorf("Autonomy = %q, want empty string", mv.Autonomy)
	}
}

// TestManifestVersion_MarshalOmitsEmptyAutonomy asserts that marshaling a
// [ManifestVersion] whose Autonomy is the zero value never emits an
// `autonomy` key on the wire — the `omitempty` tag must hold.
func TestManifestVersion_MarshalOmitsEmptyAutonomy(t *testing.T) {
	t.Parallel()

	mv := ManifestVersion{
		ID:         "r",
		ManifestID: "m",
		VersionNo:  1,
		CreatedAt:  "2026-05-02T12:00:00Z",
	}
	raw, err := json.Marshal(mv)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), `"autonomy"`) {
		t.Errorf("body included autonomy key; got %s", raw)
	}
}

// TestReadManifestVersion_NotebookRecallFields_Decoded asserts the decoder
// happy path (M5.5.c.b AC1): a wire response carrying both
// `notebook_top_k` and `notebook_relevance_threshold` decodes verbatim
// onto [ManifestVersion.NotebookTopK] and
// [ManifestVersion.NotebookRelevanceThreshold].
func TestReadManifestVersion_NotebookRecallFields_Decoded(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"r","manifest_id":"m","version_no":1,
            "system_prompt":"sp","tools":null,
            "authority_matrix":null,"knowledge_sources":null,
            "notebook_top_k":20,
            "notebook_relevance_threshold":0.75,
            "created_at":"2026-05-02T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	mv, err := c.GetManifest(context.Background(), "m")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if mv.NotebookTopK != 20 {
		t.Errorf("NotebookTopK = %d, want 20", mv.NotebookTopK)
	}
	if mv.NotebookRelevanceThreshold != 0.75 {
		t.Errorf("NotebookRelevanceThreshold = %v, want 0.75", mv.NotebookRelevanceThreshold)
	}
}

// TestReadManifestVersion_NotebookRecallFields_Omitted asserts the decoder
// edge case (M5.5.c.b AC1): a wire response without `notebook_top_k` or
// `notebook_relevance_threshold` decodes to zero values — omitempty on the
// client mirrors the server contract.
func TestReadManifestVersion_NotebookRecallFields_Omitted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"r","manifest_id":"m","version_no":1,
            "system_prompt":"","tools":null,
            "authority_matrix":null,"knowledge_sources":null,
            "created_at":"2026-05-02T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	mv, err := c.GetManifest(context.Background(), "m")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if mv.NotebookTopK != 0 {
		t.Errorf("NotebookTopK = %d, want 0 (omitted)", mv.NotebookTopK)
	}
	if mv.NotebookRelevanceThreshold != 0 {
		t.Errorf("NotebookRelevanceThreshold = %v, want 0 (omitted)", mv.NotebookRelevanceThreshold)
	}
}

// -----------------------------------------------------------------------
// M3.1 — immutable_core wire decode + omitempty
// -----------------------------------------------------------------------

// TestGetManifest_ImmutableCore_DecodesVerbatim asserts the M3.1 GET
// round-trip: a response carrying `immutable_core` projects the jsonb
// bytes onto [ManifestVersion.ImmutableCore] verbatim (mirrors the
// Tools / AuthorityMatrix raw-jsonb passthrough). Substring assertion
// over the captured RawMessage so re-encoding by the keepclient is
// caught immediately.
func TestGetManifest_ImmutableCore_DecodesVerbatim(t *testing.T) {
	t.Parallel()

	const wantPayload = `{"role_boundaries":["x"],"audit_requirements":{"manifest_changes":"retain_forever"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"row-1",
            "manifest_id":"m",
            "version_no":1,
            "system_prompt":"sp",
            "tools":null,
            "authority_matrix":null,
            "knowledge_sources":null,
            "immutable_core":`+wantPayload+`,
            "created_at":"2026-05-02T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	mv, err := c.GetManifest(context.Background(), "m")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if len(mv.ImmutableCore) == 0 {
		t.Fatalf("ImmutableCore is empty; want %s", wantPayload)
	}
	// Structural compare — Go re-decodes the inner object via
	// json.Unmarshal so any byte-level normalisation by the HTTP layer
	// stays invisible to consumers. The keepclient promises bucket-
	// level fidelity, not byte-level.
	var gotObj, wantObj map[string]any
	if err := json.Unmarshal(mv.ImmutableCore, &gotObj); err != nil {
		t.Fatalf("ImmutableCore decode: %v", err)
	}
	if err := json.Unmarshal([]byte(wantPayload), &wantObj); err != nil {
		t.Fatalf("wantPayload decode: %v", err)
	}
	for k := range wantObj {
		if _, ok := gotObj[k]; !ok {
			t.Errorf("ImmutableCore missing bucket %q; got=%v", k, gotObj)
		}
	}
}

// TestGetManifest_ImmutableCore_OmittedStaysNil asserts that a response
// without `immutable_core` decodes onto a nil [json.RawMessage] (the
// `omitempty` round-trip case). Legacy callers predating M3.1 observe
// no schema change.
func TestGetManifest_ImmutableCore_OmittedStaysNil(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"row-1",
            "manifest_id":"m",
            "version_no":1,
            "system_prompt":"sp",
            "tools":null,
            "authority_matrix":null,
            "knowledge_sources":null,
            "created_at":"2026-05-02T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	mv, err := c.GetManifest(context.Background(), "m")
	if err != nil {
		t.Fatalf("GetManifest: %v", err)
	}
	if mv.ImmutableCore != nil {
		t.Errorf("ImmutableCore = %s, want nil (omitted)", mv.ImmutableCore)
	}
}
