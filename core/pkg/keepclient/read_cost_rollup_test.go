package keepclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// fixedFrom / fixedTo bracket the canonical test window. Picked so the
// RFC3339 round-trip (UTC) is byte-stable.
var (
	fixedFrom = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	fixedTo   = time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
)

// TestCostRollups_RoundTrip is the AC14 keepclient round-trip — client
// constructs the request, server returns a canned response, client
// decodes identically.
func TestCostRollups_RoundTrip(t *testing.T) {
	t.Parallel()

	var seenPath, seenQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"buckets":[
            {"bucket":"2026-04-10","agent_id":"agent-1","model":"claude-sonnet-4","input_tokens":100,"output_tokens":50,"n_calls":2},
            {"bucket":"2026-04-11","agent_id":"agent-1","model":"claude-sonnet-4","input_tokens":200,"output_tokens":75,"n_calls":3}
        ]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.CostRollups(context.Background(), CostRollupsRequest{
		AgentID: "agent-1",
		From:    fixedFrom,
		To:      fixedTo,
		Grain:   CostRollupGrainDaily,
	})
	if err != nil {
		t.Fatalf("CostRollups: %v", err)
	}
	if seenPath != "/v1/cost-rollups" {
		t.Errorf("path = %q, want /v1/cost-rollups", seenPath)
	}
	// The query string is url.Values-encoded; check fields individually
	// to stay tolerant of map iteration order.
	for _, want := range []string{
		"agent_id=agent-1",
		"from=2026-04-01T00%3A00%3A00Z",
		"to=2026-05-01T00%3A00%3A00Z",
		"grain=daily",
	} {
		if !contains(seenQuery, want) {
			t.Errorf("RawQuery = %q, missing %q", seenQuery, want)
		}
	}
	if got := len(resp.Buckets); got != 2 {
		t.Fatalf("len(Buckets) = %d, want 2", got)
	}
	if resp.Buckets[0].Bucket != "2026-04-10" || resp.Buckets[0].Model != "claude-sonnet-4" {
		t.Errorf("Buckets[0] = %+v", resp.Buckets[0])
	}
	if resp.Buckets[0].InputTokens != 100 || resp.Buckets[0].OutputTokens != 50 || resp.Buckets[0].NCalls != 2 {
		t.Errorf("Buckets[0] tokens/calls = (%d, %d, %d), want (100, 50, 2)",
			resp.Buckets[0].InputTokens, resp.Buckets[0].OutputTokens, resp.Buckets[0].NCalls)
	}
}

// contains is a tiny stdlib-only substring helper; keeps the test file
// free of `strings` so the imports list stays minimal.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestCostRollups_EmptyBucketsNonNil asserts the empty `[]` decodes
// into a non-nil slice (matches server's allocated-empty shape).
func TestCostRollups_EmptyBucketsNonNil(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"buckets":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.CostRollups(context.Background(), CostRollupsRequest{
		AgentID: "agent-1",
		From:    fixedFrom,
		To:      fixedTo,
		Grain:   CostRollupGrainWeekly,
	})
	if err != nil {
		t.Fatalf("CostRollups: %v", err)
	}
	if resp.Buckets == nil {
		t.Error("Buckets is nil; want non-nil empty slice")
	}
}

// TestCostRollups_PreflightRejectsBadRequest covers every client-side
// short-circuit. Each case must NOT issue a network round-trip.
func TestCostRollups_PreflightRejectsBadRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  CostRollupsRequest
	}{
		{"missing_agent_id", CostRollupsRequest{From: fixedFrom, To: fixedTo, Grain: CostRollupGrainDaily}},
		{"missing_from", CostRollupsRequest{AgentID: "a", To: fixedTo, Grain: CostRollupGrainDaily}},
		{"missing_to", CostRollupsRequest{AgentID: "a", From: fixedFrom, Grain: CostRollupGrainDaily}},
		{"to_before_from", CostRollupsRequest{AgentID: "a", From: fixedTo, To: fixedFrom, Grain: CostRollupGrainDaily}},
		{"unknown_grain", CostRollupsRequest{AgentID: "a", From: fixedFrom, To: fixedTo, Grain: "hourly"}},
		{"empty_grain", CostRollupsRequest{AgentID: "a", From: fixedFrom, To: fixedTo}},
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
			_, err := c.CostRollups(context.Background(), tc.req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("network hits = %d, want 0 (preflight must short-circuit)", got)
			}
		})
	}
}

// TestCostRollups_StatusMappings asserts every documented status code
// surfaces as the corresponding sentinel via errors.Is.
func TestCostRollups_StatusMappings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"400", 400, ErrInvalidRequest},
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
				_, _ = io.WriteString(w, `{"error":"some_code"}`)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			_, err := c.CostRollups(context.Background(), CostRollupsRequest{
				AgentID: "agent-1",
				From:    fixedFrom,
				To:      fixedTo,
				Grain:   CostRollupGrainDaily,
			})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(err, %v) = false; err = %v", tc.wantErr, err)
			}
		})
	}
}

// TestCostRollups_NoTokenSource asserts that calling without
// WithTokenSource returns ErrNoTokenSource synchronously.
func TestCostRollups_NoTokenSource(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.CostRollups(context.Background(), CostRollupsRequest{
		AgentID: "agent-1",
		From:    fixedFrom,
		To:      fixedTo,
		Grain:   CostRollupGrainDaily,
	})
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestCostRollups_AuthHeaderInjected confirms the Authorization header
// is set from the configured TokenSource.
func TestCostRollups_AuthHeaderInjected(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"buckets":[]}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("xyz")))
	_, err := c.CostRollups(context.Background(), CostRollupsRequest{
		AgentID: "agent-1",
		From:    fixedFrom,
		To:      fixedTo,
		Grain:   CostRollupGrainDaily,
	})
	if err != nil {
		t.Fatalf("CostRollups: %v", err)
	}
	if gotAuth != "Bearer xyz" {
		t.Errorf("Authorization = %q, want \"Bearer xyz\"", gotAuth)
	}
}
