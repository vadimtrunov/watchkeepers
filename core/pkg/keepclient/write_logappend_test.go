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

// TestLogAppend_Success asserts the happy round-trip: a 201 response decodes
// the `{"id":"…"}` envelope; the server-side decoded body has event_type,
// optional correlation_id, and payload.
func TestLogAppend_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/keepers-log" {
			t.Errorf("Path = %q, want /v1/keepers-log", r.URL.Path)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var got LogAppendRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if got.EventType != "chunk_stored" {
			t.Errorf("EventType = %q, want chunk_stored", got.EventType)
		}
		if got.CorrelationID != "11111111-1111-4111-8111-111111111111" {
			t.Errorf("CorrelationID = %q, want canonical UUID", got.CorrelationID)
		}
		if string(got.Payload) != `{"k":"v"}` {
			t.Errorf("Payload = %s, want {\"k\":\"v\"}", got.Payload)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"row-1"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.LogAppend(context.Background(), LogAppendRequest{
		EventType:     "chunk_stored",
		CorrelationID: "11111111-1111-4111-8111-111111111111",
		Payload:       json.RawMessage(`{"k":"v"}`),
	})
	if err != nil {
		t.Fatalf("LogAppend: %v", err)
	}
	if resp.ID != "row-1" {
		t.Errorf("ID = %q, want row-1", resp.ID)
	}
}

// TestLogAppend_OmitsEmptyOptionalFields asserts the omitempty contract on the
// wire: an empty CorrelationID and empty Payload must not be transmitted at
// all, so the server's DisallowUnknownFields decoder never sees stray empty
// keys and the server's payload default ('{}'::jsonb) fires.
func TestLogAppend_OmitsEmptyOptionalFields(t *testing.T) {
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
	if _, err := c.LogAppend(context.Background(), LogAppendRequest{
		EventType: "boot",
	}); err != nil {
		t.Fatalf("LogAppend: %v", err)
	}
	if strings.Contains(string(rawBody), `"correlation_id"`) {
		t.Errorf("body included correlation_id field; got %s", rawBody)
	}
	if strings.Contains(string(rawBody), `"payload"`) {
		t.Errorf("body included payload field; got %s", rawBody)
	}
}

// TestLogAppend_RejectsClientSuppliedActorOrScope asserts the security AC: the
// LogAppendRequest type must not have actor_* or scope fields, so callers
// physically cannot push them through the typed surface (server stamps them
// from the verified claim).
func TestLogAppend_RejectsClientSuppliedActorOrScope(t *testing.T) {
	t.Parallel()

	rt := reflect.TypeOf(LogAppendRequest{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		name := strings.SplitN(tag, ",", 2)[0]
		switch {
		case strings.EqualFold(name, "scope"), strings.EqualFold(f.Name, "Scope"):
			t.Errorf("LogAppendRequest must not expose a scope field; found %q", f.Name)
		case strings.HasPrefix(name, "actor_"), strings.HasPrefix(strings.ToLower(f.Name), "actor"):
			t.Errorf("LogAppendRequest must not expose actor fields; found %q (json %q)", f.Name, tag)
		}
	}
}

// TestLogAppend_AuthHeaderInjected asserts the Authorization header carries
// the bearer token from the configured TokenSource.
func TestLogAppend_AuthHeaderInjected(t *testing.T) {
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
	if _, err := c.LogAppend(context.Background(), LogAppendRequest{EventType: "x"}); err != nil {
		t.Fatalf("LogAppend: %v", err)
	}
	if gotAuth != "Bearer xyz" {
		t.Errorf("Authorization = %q, want \"Bearer xyz\"", gotAuth)
	}
}

// TestLogAppend_NoTokenSource asserts that calling LogAppend without
// configuring WithTokenSource returns ErrNoTokenSource synchronously and does
// NOT contact the network.
func TestLogAppend_NoTokenSource(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.LogAppend(context.Background(), LogAppendRequest{EventType: "x"})
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestLogAppend_PreflightValidation asserts that empty EventType fails
// synchronously with ErrInvalidRequest before any network call.
func TestLogAppend_PreflightValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  LogAppendRequest
	}{
		{"empty_event_type", LogAppendRequest{}},
		{"empty_event_type_with_payload", LogAppendRequest{Payload: json.RawMessage(`{"x":1}`)}},
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
			_, err := c.LogAppend(context.Background(), tc.req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("network hits = %d, want 0", got)
			}
		})
	}
}

// TestLogAppend_StatusMappings exhaustively asserts that every documented
// status code surfaces as the corresponding sentinel via errors.Is.
func TestLogAppend_StatusMappings(t *testing.T) {
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
			_, err := c.LogAppend(context.Background(), LogAppendRequest{EventType: "x"})
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

// TestLogAppend_TransportError asserts that a transport-level failure (server
// closed before request) surfaces as a wrapped error, not a *ServerError.
func TestLogAppend_TransportError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(WithBaseURL(url), WithTokenSource(StaticToken("t")))
	_, err := c.LogAppend(context.Background(), LogAppendRequest{EventType: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var se *ServerError
	if errors.As(err, &se) {
		t.Errorf("transport error must not be a *ServerError; got %v", err)
	}
}

// TestLogAppend_ContextCancellation asserts that a cancelled context aborts
// the in-flight request and the resulting error wraps context.Canceled.
func TestLogAppend_ContextCancellation(t *testing.T) {
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
	_, err := c.LogAppend(ctx, LogAppendRequest{EventType: "x"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}
}
