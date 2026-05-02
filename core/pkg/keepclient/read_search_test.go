package keepclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestSearch_Success asserts the happy round-trip: a 200 response with one
// result decodes into a *SearchResponse with the same field values.
func TestSearch_Success(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/search" {
			t.Errorf("Path = %q, want /v1/search", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var got SearchRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if len(got.Embedding) != 3 || got.TopK != 5 {
			t.Errorf("decoded body = %+v, want Embedding len 3 + TopK 5", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `{"results":[{"id":"row-1","subject":"sub","content":"c","created_at":%q,"distance":0.25}]}`, created)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.Search(context.Background(), SearchRequest{
		Embedding: []float32{0.1, 0.2, 0.3},
		TopK:      5,
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if got := len(resp.Results); got != 1 {
		t.Fatalf("len(Results) = %d, want 1", got)
	}
	r0 := resp.Results[0]
	if r0.ID != "row-1" || r0.Subject != "sub" || r0.Content != "c" || r0.CreatedAt != created || r0.Distance != 0.25 {
		t.Errorf("Results[0] = %+v", r0)
	}
}

// TestSearch_AuthHeaderInjected asserts that the call carries the
// Authorization header from the configured TokenSource.
func TestSearch_AuthHeaderInjected(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("xyz")))
	if _, err := c.Search(context.Background(), SearchRequest{Embedding: []float32{1}, TopK: 1}); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if gotAuth != "Bearer xyz" {
		t.Errorf("Authorization = %q, want \"Bearer xyz\"", gotAuth)
	}
}

// TestSearch_NoTokenSource asserts that calling Search without configuring
// WithTokenSource returns ErrNoTokenSource synchronously and does NOT contact
// the network.
func TestSearch_NoTokenSource(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.Search(context.Background(), SearchRequest{Embedding: []float32{1}, TopK: 1})
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestSearch_PreflightValidation asserts that empty Embedding or non-positive
// TopK fail synchronously with ErrInvalidRequest before any network call.
func TestSearch_PreflightValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  SearchRequest
	}{
		{"empty_embedding", SearchRequest{Embedding: nil, TopK: 5}},
		{"zero_top_k", SearchRequest{Embedding: []float32{1, 2, 3}, TopK: 0}},
		{"negative_top_k", SearchRequest{Embedding: []float32{1, 2, 3}, TopK: -1}},
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
			_, err := c.Search(context.Background(), tc.req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("network hits = %d, want 0", got)
			}
		})
	}
}

// TestSearch_StatusMappings exhaustively asserts that every documented status
// code surfaces as the corresponding sentinel via errors.Is.
func TestSearch_StatusMappings(t *testing.T) {
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
			_, err := c.Search(context.Background(), SearchRequest{Embedding: []float32{1}, TopK: 1})
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

// TestSearch_TransportError asserts that a transport-level failure (server
// closed) surfaces as a wrapped error, not as a *ServerError.
func TestSearch_TransportError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(WithBaseURL(url), WithTokenSource(StaticToken("t")))
	_, err := c.Search(context.Background(), SearchRequest{Embedding: []float32{1}, TopK: 1})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *ServerError
	if errors.As(err, &se) {
		t.Errorf("transport error must not be a *ServerError; got %v", err)
	}
}

// TestSearch_ContextCancellation asserts that a cancelled context aborts the
// in-flight request and the resulting error wraps context.Canceled. The
// handler stalls on a per-test "release" channel so the cleanup path lets
// the server return promptly even if the client's connection-close racing
// the server-side request context did not fire.
func TestSearch_ContextCancellation(t *testing.T) {
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
	_, err := c.Search(ctx, SearchRequest{Embedding: []float32{1}, TopK: 1})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}
}

// TestSearch_NaNDistanceDecodes asserts that a row whose server-side NaN
// distance was snapped to 2.0 decodes as a regular float without error
// (regression guard for the M2.7.b+c NaN snap fixture).
func TestSearch_NaNDistanceDecodes(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"results":[{"id":"r","content":"c","created_at":"2026-05-02T00:00:00Z","distance":2.0}]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.Search(context.Background(), SearchRequest{Embedding: []float32{1}, TopK: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if resp.Results[0].Distance != 2.0 {
		t.Errorf("Distance = %v, want 2.0", resp.Results[0].Distance)
	}
}
