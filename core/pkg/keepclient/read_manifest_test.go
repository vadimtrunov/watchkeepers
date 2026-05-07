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
