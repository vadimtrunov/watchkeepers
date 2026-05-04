package keepclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"
)

// TestClient_GetWatchkeeper_HappyPath asserts the happy round-trip: server
// returns the full watchkeeper row and the client decodes nullable
// timestamps as *time.Time (nil for SQL NULL, parsed value otherwise).
func TestClient_GetWatchkeeper_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Method = %q, want GET", r.Method)
		}
		if want := "/v1/watchkeepers/" + wkTestRowID; r.URL.Path != want {
			t.Errorf("Path = %q, want %q", r.URL.Path, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"`+wkTestRowID+`",
            "manifest_id":"`+wkTestManifestID+`",
            "lead_human_id":"`+wkTestLeadHumanID+`",
            "active_manifest_version_id":"`+wkTestActiveVerID+`",
            "status":"active",
            "spawned_at":"2026-05-01T10:00:00Z",
            "retired_at":null,
            "created_at":"2026-04-30T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	wk, err := c.GetWatchkeeper(context.Background(), wkTestRowID)
	if err != nil {
		t.Fatalf("GetWatchkeeper: %v", err)
	}
	if wk.ID != wkTestRowID || wk.Status != "active" {
		t.Errorf("got = %+v", wk)
	}
	if wk.ActiveManifestVersionID == nil || *wk.ActiveManifestVersionID != wkTestActiveVerID {
		t.Errorf("ActiveManifestVersionID = %v, want %q", wk.ActiveManifestVersionID, wkTestActiveVerID)
	}
	if wk.SpawnedAt == nil {
		t.Errorf("SpawnedAt is nil, want non-nil")
	} else if !wk.SpawnedAt.Equal(time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("SpawnedAt = %v, want 2026-05-01T10:00:00Z", wk.SpawnedAt)
	}
	if wk.RetiredAt != nil {
		t.Errorf("RetiredAt = %v, want nil", wk.RetiredAt)
	}
	if !wk.CreatedAt.Equal(time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("CreatedAt = %v, want 2026-04-30T12:00:00Z", wk.CreatedAt)
	}
}

// TestClient_GetWatchkeeper_404_MapsToErrNotFound asserts that a server 404
// with `{"error":"not_found"}` surfaces as *ServerError + ErrNotFound via
// errors.Is.
func TestClient_GetWatchkeeper_404_MapsToErrNotFound(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.GetWatchkeeper(context.Background(), wkTestRowID)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("errors.Is(err, ErrNotFound) = false; err = %v", err)
	}
	var se *ServerError
	if !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Errorf("ServerError.Status = %v, want 404 (err=%v)", se, err)
	}
}

// TestClient_GetWatchkeeper_EmptyID_ErrInvalidRequest asserts that an empty
// id rejects synchronously without a network round-trip.
func TestClient_GetWatchkeeper_EmptyID_ErrInvalidRequest(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.GetWatchkeeper(context.Background(), "")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestClient_ListWatchkeepers_HappyPath asserts the happy round-trip: server
// returns three rows in a `{"items":[…]}` envelope and the client decodes
// the slice with the right length.
func TestClient_ListWatchkeepers_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "items":[
                {"id":"a","manifest_id":"m","lead_human_id":"h","active_manifest_version_id":null,"status":"active","spawned_at":"2026-05-01T10:00:00Z","retired_at":null,"created_at":"2026-04-30T12:00:00Z"},
                {"id":"b","manifest_id":"m","lead_human_id":"h","active_manifest_version_id":null,"status":"pending","spawned_at":null,"retired_at":null,"created_at":"2026-04-30T11:00:00Z"},
                {"id":"c","manifest_id":"m","lead_human_id":"h","active_manifest_version_id":null,"status":"retired","spawned_at":"2026-04-29T10:00:00Z","retired_at":"2026-04-30T10:00:00Z","created_at":"2026-04-29T09:00:00Z"}
            ],
            "next_cursor":null
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.ListWatchkeepers(context.Background(), ListWatchkeepersRequest{})
	if err != nil {
		t.Fatalf("ListWatchkeepers: %v", err)
	}
	if len(resp.Items) != 3 {
		t.Fatalf("Items len = %d, want 3", len(resp.Items))
	}
	if resp.NextCursor != nil {
		t.Errorf("NextCursor = %v, want nil", resp.NextCursor)
	}
}

// TestClient_ListWatchkeepers_FilterAndLimitOnWire asserts the query string
// reflects the supplied Status filter and Limit, and is properly URL-encoded.
func TestClient_ListWatchkeepers_FilterAndLimitOnWire(t *testing.T) {
	t.Parallel()

	var rawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"items":[],"next_cursor":null}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if _, err := c.ListWatchkeepers(context.Background(), ListWatchkeepersRequest{
		Status: "active",
		Limit:  25,
	}); err != nil {
		t.Fatalf("ListWatchkeepers: %v", err)
	}
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", rawQuery, err)
	}
	if q.Get("status") != "active" {
		t.Errorf("status = %q, want active (raw=%q)", q.Get("status"), rawQuery)
	}
	if q.Get("limit") != "25" {
		t.Errorf("limit = %q, want 25 (raw=%q)", q.Get("limit"), rawQuery)
	}
}

// TestClient_ListWatchkeepers_LimitOutOfRange_ErrInvalidRequest asserts that
// limit=300 (above the 200 cap) and limit=-1 reject synchronously.
func TestClient_ListWatchkeepers_LimitOutOfRange_ErrInvalidRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		limit int
	}{
		{"too_high", 300},
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
			_, err := c.ListWatchkeepers(context.Background(), ListWatchkeepersRequest{
				Limit: tc.limit,
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

// TestClient_ListWatchkeepers_BadStatus_ErrInvalidRequest asserts an unknown
// status filter rejects synchronously without a network round-trip.
func TestClient_ListWatchkeepers_BadStatus_ErrInvalidRequest(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.ListWatchkeepers(context.Background(), ListWatchkeepersRequest{
		Status: "weird",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}
