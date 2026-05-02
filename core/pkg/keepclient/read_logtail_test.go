package keepclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestLogTail_Success asserts the happy round-trip with one row carrying
// every optional column populated.
func TestLogTail_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/v1/keepers-log" {
			t.Errorf("Path = %q, want /v1/keepers-log", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"events":[{
            "id":"row-1",
            "event_type":"chunk_stored",
            "correlation_id":"corr-1",
            "actor_watchkeeper_id":"wk-1",
            "actor_human_id":"h-1",
            "payload":{"k":"v"},
            "created_at":"2026-05-02T12:00:00Z"
        }]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.LogTail(context.Background(), LogTailOptions{})
	if err != nil {
		t.Fatalf("LogTail: %v", err)
	}
	if got := len(resp.Events); got != 1 {
		t.Fatalf("len(Events) = %d, want 1", got)
	}
	ev := resp.Events[0]
	if ev.ID != "row-1" || ev.EventType != "chunk_stored" {
		t.Errorf("Events[0] = %+v", ev)
	}
	if ev.CorrelationID == nil || *ev.CorrelationID != "corr-1" {
		t.Errorf("CorrelationID = %v", ev.CorrelationID)
	}
	if ev.ActorWatchkeeperID == nil || *ev.ActorWatchkeeperID != "wk-1" {
		t.Errorf("ActorWatchkeeperID = %v", ev.ActorWatchkeeperID)
	}
	if ev.ActorHumanID == nil || *ev.ActorHumanID != "h-1" {
		t.Errorf("ActorHumanID = %v", ev.ActorHumanID)
	}
	if string(ev.Payload) == "" {
		t.Errorf("Payload empty; want raw JSON")
	}
}

// TestLogTail_OmitsLimitQueryWhenZero asserts the AC3 contract: zero Limit
// must NOT append `?limit=` to the URL — the server applies its own default.
func TestLogTail_OmitsLimitQueryWhenZero(t *testing.T) {
	t.Parallel()

	var seenRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRaw = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"events":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.LogTail(context.Background(), LogTailOptions{Limit: 0})
	if err != nil {
		t.Fatalf("LogTail: %v", err)
	}
	if seenRaw != "" {
		t.Errorf("RawQuery = %q, want empty (zero limit must omit ?limit=)", seenRaw)
	}
}

// TestLogTail_AppendsLimitQueryWhenPositive asserts that a positive Limit
// renders as `?limit=<n>` on the wire.
func TestLogTail_AppendsLimitQueryWhenPositive(t *testing.T) {
	t.Parallel()

	var seenRaw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenRaw = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"events":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.LogTail(context.Background(), LogTailOptions{Limit: 10})
	if err != nil {
		t.Fatalf("LogTail: %v", err)
	}
	if seenRaw != "limit=10" {
		t.Errorf("RawQuery = %q, want \"limit=10\"", seenRaw)
	}
}

// TestLogTail_PreflightRejectsNegativeLimit asserts that a negative Limit
// short-circuits with ErrInvalidRequest before any network round-trip.
func TestLogTail_PreflightRejectsNegativeLimit(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.LogTail(context.Background(), LogTailOptions{Limit: -1})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestLogTail_EmptyEventsNonNil asserts that an empty events array decodes
// into a non-nil empty slice (matching the server's allocated-empty shape).
func TestLogTail_EmptyEventsNonNil(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"events":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.LogTail(context.Background(), LogTailOptions{})
	if err != nil {
		t.Fatalf("LogTail: %v", err)
	}
	if resp.Events == nil {
		t.Error("Events is nil; want non-nil empty slice for empty `[]` payload")
	}
	if len(resp.Events) != 0 {
		t.Errorf("len(Events) = %d, want 0", len(resp.Events))
	}
}

// TestLogTail_OmitsActorFieldsWhenNull asserts that absent optional actor
// columns decode into nil pointers.
func TestLogTail_OmitsActorFieldsWhenNull(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"events":[{
            "id":"r","event_type":"e","payload":{},"created_at":"2026-05-02T12:00:00Z"
        }]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.LogTail(context.Background(), LogTailOptions{})
	if err != nil {
		t.Fatalf("LogTail: %v", err)
	}
	ev := resp.Events[0]
	if ev.CorrelationID != nil || ev.ActorWatchkeeperID != nil || ev.ActorHumanID != nil {
		t.Errorf("expected nil pointers; got %+v", ev)
	}
}

// TestLogTail_AuthHeaderInjected asserts the Authorization header is set
// from the configured TokenSource.
func TestLogTail_AuthHeaderInjected(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"events":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("xyz")))
	if _, err := c.LogTail(context.Background(), LogTailOptions{}); err != nil {
		t.Fatalf("LogTail: %v", err)
	}
	if gotAuth != "Bearer xyz" {
		t.Errorf("Authorization = %q, want \"Bearer xyz\"", gotAuth)
	}
}

// TestLogTail_NoTokenSource asserts that calling LogTail without configuring
// WithTokenSource returns ErrNoTokenSource synchronously.
func TestLogTail_NoTokenSource(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.LogTail(context.Background(), LogTailOptions{})
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestLogTail_StatusMappings verifies each documented status -> sentinel.
func TestLogTail_StatusMappings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"401", 401, ErrUnauthorized},
		{"403", 403, ErrForbidden},
		{"500", 500, ErrInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":"x"}`)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			_, err := c.LogTail(context.Background(), LogTailOptions{})
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(err, %v) = false; err = %v", tc.wantErr, err)
			}
			var se *ServerError
			if !errors.As(err, &se) || se.Status != tc.status {
				t.Errorf("ServerError.Status = %v, want %d", se, tc.status)
			}
		})
	}
}
