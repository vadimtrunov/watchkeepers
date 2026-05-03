package keepclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestStore_Success asserts the happy round-trip: a 201 response decodes the
// `{"id":"…"}` envelope and the server-side decoded body carries the expected
// subject, content, and embedding length.
func TestStore_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/knowledge-chunks" {
			t.Errorf("Path = %q, want /v1/knowledge-chunks", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var got StoreRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if got.Subject != "subj" {
			t.Errorf("Subject = %q, want %q", got.Subject, "subj")
		}
		if got.Content != "hello" {
			t.Errorf("Content = %q, want %q", got.Content, "hello")
		}
		if len(got.Embedding) != 3 {
			t.Errorf("Embedding len = %d, want 3", len(got.Embedding))
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.Store(context.Background(), StoreRequest{
		Subject:   "subj",
		Content:   "hello",
		Embedding: []float32{0.1, 0.2, 0.3},
	})
	if err != nil {
		t.Fatalf("Store: %v", err)
	}
	if resp.ID != "row-1" {
		t.Errorf("ID = %q, want %q", resp.ID, "row-1")
	}
}

// TestStore_OmitsEmptySubject asserts the omitempty contract on the wire:
// an empty Subject must not be transmitted at all so the server's
// DisallowUnknownFields would not regress on a subject="" addition.
func TestStore_OmitsEmptySubject(t *testing.T) {
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
	if _, err := c.Store(context.Background(), StoreRequest{
		Content:   "hello",
		Embedding: []float32{0.1},
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if strings.Contains(string(rawBody), `"subject"`) {
		t.Errorf("body included subject field; got %s", rawBody)
	}
}

// TestStore_RejectsClientSuppliedScope asserts the security AC: the
// StoreRequest type must not have a `scope` field, so callers physically
// cannot push a scope through the typed surface. This is a compile-time
// guarantee mirrored at runtime by reflecting on the struct.
func TestStore_RejectsClientSuppliedScope(t *testing.T) {
	t.Parallel()

	rt := reflect.TypeOf(StoreRequest{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		name := strings.SplitN(tag, ",", 2)[0]
		if strings.EqualFold(name, "scope") || strings.EqualFold(f.Name, "Scope") {
			t.Errorf("StoreRequest must not expose a scope field; found %q (json tag %q)", f.Name, tag)
		}
	}
}

// TestStore_AuthHeaderInjected asserts the Authorization header carries the
// bearer token from the configured TokenSource.
func TestStore_AuthHeaderInjected(t *testing.T) {
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
	if _, err := c.Store(context.Background(), StoreRequest{
		Content:   "x",
		Embedding: []float32{0.1},
	}); err != nil {
		t.Fatalf("Store: %v", err)
	}
	if gotAuth != "Bearer xyz" {
		t.Errorf("Authorization = %q, want \"Bearer xyz\"", gotAuth)
	}
}

// TestStore_NoTokenSource asserts that calling Store without configuring
// WithTokenSource returns ErrNoTokenSource synchronously and does NOT contact
// the network.
func TestStore_NoTokenSource(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.Store(context.Background(), StoreRequest{
		Content:   "x",
		Embedding: []float32{0.1},
	})
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestStore_PreflightValidation asserts that empty Content or empty Embedding
// fail synchronously with ErrInvalidRequest before any network call.
func TestStore_PreflightValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  StoreRequest
	}{
		{"empty_content", StoreRequest{Content: "", Embedding: []float32{0.1}}},
		{"empty_embedding", StoreRequest{Content: "x", Embedding: nil}},
		{"both_empty", StoreRequest{}},
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
			_, err := c.Store(context.Background(), tc.req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("network hits = %d, want 0", got)
			}
		})
	}
}

// TestStore_StatusMappings exhaustively asserts that every documented status
// code surfaces as the corresponding sentinel via errors.Is.
func TestStore_StatusMappings(t *testing.T) {
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
			_, err := c.Store(context.Background(), StoreRequest{
				Content:   "x",
				Embedding: []float32{0.1},
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

// TestStore_TransportError asserts that a transport-level failure (server
// closed before request) surfaces as a wrapped error, not a *ServerError.
func TestStore_TransportError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(WithBaseURL(url), WithTokenSource(StaticToken("t")))
	_, err := c.Store(context.Background(), StoreRequest{
		Content:   "x",
		Embedding: []float32{0.1},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *ServerError
	if errors.As(err, &se) {
		t.Errorf("transport error must not be a *ServerError; got %v", err)
	}
}

// TestStore_ContextCancellation asserts that a cancelled context aborts the
// in-flight request and the resulting error wraps context.Canceled.
func TestStore_ContextCancellation(t *testing.T) {
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
	_, err := c.Store(ctx, StoreRequest{
		Content:   "x",
		Embedding: []float32{0.1},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}
}
