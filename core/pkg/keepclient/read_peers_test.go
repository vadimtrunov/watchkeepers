package keepclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"sync/atomic"
	"testing"
)

const (
	peerTestWKA  = "11111111-1111-4111-8111-111111111111"
	peerTestWKB  = "22222222-2222-4222-8222-222222222222"
	peerRoleA    = "Coordinator"
	peerRoleB    = "Reviewer"
	peerLangEN   = "en"
	peerLangUS   = "en-US"
	peerDescA    = "Tactical project coordinator"
	peerDescB    = "Diligent PR reviewer"
	peerCapAName = "update_ticket_field"
	peerCapB1    = "github.fetch_pr"
	peerCapB2    = "github.post_review_comment"
)

// TestClient_ListPeers_HappyPath asserts the happy round-trip: server
// returns a two-row items envelope and the client decodes every wire
// field (role / description / language / capabilities / availability).
func TestClient_ListPeers_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/v1/peers" {
			t.Errorf("Path = %q, want /v1/peers", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "items":[
                {"watchkeeper_id":"`+peerTestWKA+`","role":"`+peerRoleA+`","description":"`+peerDescA+`","language":"`+peerLangEN+`","capabilities":["`+peerCapAName+`"],"availability":"available"},
                {"watchkeeper_id":"`+peerTestWKB+`","role":"`+peerRoleB+`","description":"`+peerDescB+`","language":"`+peerLangUS+`","capabilities":["`+peerCapB1+`","`+peerCapB2+`"],"availability":"available"}
            ],
            "next_cursor":null
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.ListPeers(context.Background(), ListPeersRequest{})
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if resp.NextCursor != nil {
		t.Errorf("NextCursor = %v, want nil", resp.NextCursor)
	}
	if len(resp.Items) != 2 {
		t.Fatalf("Items len = %d, want 2", len(resp.Items))
	}
	got := resp.Items[0]
	want := Peer{
		WatchkeeperID: peerTestWKA,
		Role:          peerRoleA,
		Description:   peerDescA,
		Language:      peerLangEN,
		Capabilities:  []string{peerCapAName},
		Availability:  PeerAvailabilityAvailable,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("items[0] = %+v, want %+v", got, want)
	}
	if av := resp.Items[1].Availability; av != PeerAvailabilityAvailable {
		t.Errorf("items[1].Availability = %q, want %q", av, PeerAvailabilityAvailable)
	}
}

// TestClient_ListPeers_LimitOnWire asserts the query string reflects the
// supplied Limit and is properly URL-encoded.
func TestClient_ListPeers_LimitOnWire(t *testing.T) {
	t.Parallel()

	var rawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[],"next_cursor":null}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if _, err := c.ListPeers(context.Background(), ListPeersRequest{Limit: 25}); err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", rawQuery, err)
	}
	if q.Get("limit") != "25" {
		t.Errorf("limit = %q, want 25 (raw=%q)", q.Get("limit"), rawQuery)
	}
}

// TestClient_ListPeers_ZeroLimitOmitsQueryParam asserts that
// `Limit: 0` (the zero value) does NOT send `?limit=` at all, so the
// server applies its documented default (50). A naive
// `strconv.Itoa(0)` would land `?limit=0` on the wire which the server
// rejects as out-of-range.
func TestClient_ListPeers_ZeroLimitOmitsQueryParam(t *testing.T) {
	t.Parallel()

	var rawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[],"next_cursor":null}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if _, err := c.ListPeers(context.Background(), ListPeersRequest{}); err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if rawQuery != "" {
		t.Errorf("raw query = %q, want empty (no limit parameter)", rawQuery)
	}
}

// TestClient_ListPeers_LimitOutOfRange_ErrInvalidRequest pins the
// client-side range guard: limit=300 and limit=-1 reject synchronously
// without a network round-trip.
func TestClient_ListPeers_LimitOutOfRange_ErrInvalidRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		limit int
	}{
		{"too_high_201", 201},
		{"way_too_high", 99999},
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
			_, err := c.ListPeers(context.Background(), ListPeersRequest{Limit: tc.limit})
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("network hits = %d, want 0", got)
			}
		})
	}
}

// TestClient_ListPeers_MaxLimitAccepted pins the inclusive boundary —
// limit=200 is accepted (no synchronous rejection).
func TestClient_ListPeers_MaxLimitAccepted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[],"next_cursor":null}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if _, err := c.ListPeers(context.Background(), ListPeersRequest{Limit: 200}); err != nil {
		t.Fatalf("ListPeers(limit=200): %v", err)
	}
}

// TestClient_ListPeers_500_MapsToErrInternal pins the error taxonomy:
// a server 500 with `{"error":"list_peers_failed"}` surfaces as
// *ServerError + ErrInternal via errors.Is.
func TestClient_ListPeers_500_MapsToErrInternal(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"list_peers_failed"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.ListPeers(context.Background(), ListPeersRequest{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInternal) {
		t.Errorf("errors.Is(err, ErrInternal) = false; err = %v", err)
	}
	var se *ServerError
	if !errors.As(err, &se) || se.Status != http.StatusInternalServerError {
		t.Errorf("ServerError.Status = %v, want 500 (err=%v)", se, err)
	}
	if se != nil && se.Code != "list_peers_failed" {
		t.Errorf("ServerError.Code = %q, want list_peers_failed", se.Code)
	}
}

// TestClient_ListPeers_401_MapsToErrUnauthorized pins the 401 taxonomy.
func TestClient_ListPeers_401_MapsToErrUnauthorized(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"unauthorized"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.ListPeers(context.Background(), ListPeersRequest{})
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("errors.Is(err, ErrUnauthorized) = false; err = %v", err)
	}
}

// TestClient_ListPeers_NoTokenSource_ErrNoTokenSource pins the
// pre-network guard: omitting WithTokenSource rejects the call before
// any HTTP traffic.
func TestClient_ListPeers_NoTokenSource_ErrNoTokenSource(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.ListPeers(context.Background(), ListPeersRequest{})
	if !errors.Is(err, ErrNoTokenSource) {
		t.Errorf("errors.Is(err, ErrNoTokenSource) = false; err = %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0 (no token source must short-circuit)", got)
	}
}

// TestClient_ListPeers_EmptyItemsNonNil pins the documented client-
// side contract: an empty active set decodes as `[]Peer{}` (non-nil,
// length 0). Defends the M1.3.d in-memory filter from a nil-dereference
// if a future server bug surfaces "items":null.
func TestClient_ListPeers_EmptyItemsNonNil(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, body string
	}{
		{"empty_array", `{"items":[],"next_cursor":null}`},
		{"null_items", `{"items":null,"next_cursor":null}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			resp, err := c.ListPeers(context.Background(), ListPeersRequest{})
			if err != nil {
				t.Fatalf("ListPeers: %v", err)
			}
			if resp.Items == nil {
				t.Errorf("Items is nil; want non-nil empty slice")
			}
			if len(resp.Items) != 0 {
				t.Errorf("Items len = %d, want 0", len(resp.Items))
			}
		})
	}
}

// TestClient_ListPeers_CapabilitiesNonNil pins the defensive
// normalization: a row whose `capabilities` is null on the wire (a
// possible regression mode) surfaces as `[]string{}` so the M1.3.d
// in-memory filter can range without a nil guard.
func TestClient_ListPeers_CapabilitiesNonNil(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "items":[
                {"watchkeeper_id":"`+peerTestWKA+`","role":"`+peerRoleA+`","description":"","language":"","capabilities":null,"availability":"available"}
            ],
            "next_cursor":null
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.ListPeers(context.Background(), ListPeersRequest{})
	if err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("Items len = %d, want 1", len(resp.Items))
	}
	if resp.Items[0].Capabilities == nil {
		t.Errorf("Capabilities is nil; want non-nil empty slice (defensive normalization)")
	}
}

// TestClient_ListPeers_TokenInjected pins the auth contract: every
// call to `/v1/peers` carries the bearer token, mirroring the
// `/v1/watchkeepers` discipline.
func TestClient_ListPeers_TokenInjected(t *testing.T) {
	t.Parallel()

	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[],"next_cursor":null}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("opaque-token-12345")))
	if _, err := c.ListPeers(context.Background(), ListPeersRequest{}); err != nil {
		t.Fatalf("ListPeers: %v", err)
	}
	if gotAuth != "Bearer opaque-token-12345" {
		t.Errorf("Authorization = %q, want Bearer opaque-token-12345", gotAuth)
	}
}

// TestClient_ListPeers_ContextCanceled — ctx cancellation aborts the
// in-flight request and surfaces a wrapped context.Canceled error
// (NOT *ServerError, which would imply the server returned a response).
func TestClient_ListPeers_ContextCanceled(t *testing.T) {
	t.Parallel()

	// A handler that never responds; ctx cancel is the only escape.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")), WithHTTPClient(newTestHTTPClient(2_000_000_000)))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.ListPeers(ctx, ListPeersRequest{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}
}
