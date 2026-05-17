package keepclient

// read_latest_retired_by_role_test.go pins the M7.1.b predecessor-
// lookup client against an httptest server. Covered:
//
//   - happy path: 200 + JSON envelope → *Watchkeeper with role_id +
//     archive_uri pinned non-nil + retired_at preserved.
//   - 404 surfaces as both ErrNoPredecessor and ErrNotFound via
//     errors.Is on the same returned error value.
//   - synchronous empty-input rejection: empty organizationID and empty
//     roleID both return ErrInvalidRequest without a network round-trip.
//   - wire-shape AC: the request's query string carries the role_id
//     parameter URL-encoded and is the only query parameter.
//   - non-404 server error (e.g. 500) passes through without
//     ErrNoPredecessor wrapping.

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

// predecessorRowID is the row UUID staged on every happy-path test.
const (
	predecessorRowID      = "55555555-5555-4555-8555-555555555555"
	predecessorOrgID      = "66666666-6666-4666-8666-666666666666"
	predecessorRoleID     = "frontline-watchkeeper"
	predecessorArchiveURI = "s3://wk-archive/2026/05/" + predecessorRowID + ".jsonl"
)

// TestClient_LatestRetiredByRole_HappyPath — 200 + full envelope decodes
// to a *Watchkeeper with RoleID + ArchiveURI + RetiredAt non-nil. Pins
// the wire-shape on the JSON tags introduced in M7.1.a.
func TestClient_LatestRetiredByRole_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/v1/watchkeepers/latest-retired-by-role" {
			t.Errorf("Path = %q, want /v1/watchkeepers/latest-retired-by-role", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"`+predecessorRowID+`",
            "manifest_id":"`+wkTestManifestID+`",
            "lead_human_id":"`+wkTestLeadHumanID+`",
            "active_manifest_version_id":null,
            "status":"retired",
            "spawned_at":"2026-05-01T10:00:00Z",
            "retired_at":"2026-05-10T12:00:00Z",
            "archive_uri":"`+predecessorArchiveURI+`",
            "role_id":"`+predecessorRoleID+`",
            "created_at":"2026-04-30T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	wk, err := c.LatestRetiredByRole(context.Background(), predecessorOrgID, predecessorRoleID)
	if err != nil {
		t.Fatalf("LatestRetiredByRole: %v", err)
	}
	if wk == nil {
		t.Fatal("Watchkeeper = nil, want non-nil")
	}
	if wk.ID != predecessorRowID {
		t.Errorf("ID = %q, want %q", wk.ID, predecessorRowID)
	}
	if wk.RoleID == nil || *wk.RoleID != predecessorRoleID {
		t.Errorf("RoleID = %v, want %q", wk.RoleID, predecessorRoleID)
	}
	if wk.ArchiveURI == nil || *wk.ArchiveURI != predecessorArchiveURI {
		t.Errorf("ArchiveURI = %v, want %q", wk.ArchiveURI, predecessorArchiveURI)
	}
	if wk.RetiredAt == nil {
		t.Fatal("RetiredAt = nil, want non-nil")
	}
	if !wk.RetiredAt.Equal(time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)) {
		t.Errorf("RetiredAt = %v, want 2026-05-10T12:00:00Z", wk.RetiredAt)
	}
	if wk.Status != "retired" {
		t.Errorf("Status = %q, want retired", wk.Status)
	}
}

// TestClient_LatestRetiredByRole_404_MapsToErrNoPredecessor — a 404
// response surfaces as an error that matches BOTH ErrNoPredecessor and
// ErrNotFound via errors.Is on the same value.
func TestClient_LatestRetiredByRole_404_MapsToErrNoPredecessor(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.LatestRetiredByRole(context.Background(), predecessorOrgID, predecessorRoleID)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrNoPredecessor) {
		t.Errorf("errors.Is(err, ErrNoPredecessor) = false; err = %v", err)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("errors.Is(err, ErrNotFound) = false; err = %v", err)
	}
}

// TestClient_LatestRetiredByRole_EmptyOrgID_ErrInvalidRequest — an
// empty organizationID short-circuits without a network round-trip.
func TestClient_LatestRetiredByRole_EmptyOrgID_ErrInvalidRequest(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.LatestRetiredByRole(context.Background(), "", predecessorRoleID)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestClient_LatestRetiredByRole_EmptyRoleID_ErrInvalidRequest — an
// empty roleID short-circuits without a network round-trip.
func TestClient_LatestRetiredByRole_EmptyRoleID_ErrInvalidRequest(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.LatestRetiredByRole(context.Background(), predecessorOrgID, "")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestClient_LatestRetiredByRole_QueryStringWireShape — pins the
// `role_id=<encoded>` query parameter (URL-encoded; only parameter
// on the wire). A role_id carrying RFC-3986-reserved characters
// (`+`, `&`, `=`) round-trips through url.QueryEscape so the server
// receives the original string.
func TestClient_LatestRetiredByRole_QueryStringWireShape(t *testing.T) {
	t.Parallel()

	// Use a role_id with reserved characters to pin encoding.
	const trickyRoleID = "role+with&reserved=chars"

	var gotRawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, _ = c.LatestRetiredByRole(context.Background(), predecessorOrgID, trickyRoleID)

	q, err := url.ParseQuery(gotRawQuery)
	if err != nil {
		t.Fatalf("ParseQuery(%q): %v", gotRawQuery, err)
	}
	if q.Get("role_id") != trickyRoleID {
		t.Errorf("role_id = %q, want %q (raw=%q)", q.Get("role_id"), trickyRoleID, gotRawQuery)
	}
	// Confirm no other parameters sneak onto the wire (in particular
	// the synthetic `organizationID` arg MUST NOT be serialised — the
	// server resolves tenancy from the bearer token's claim).
	if got := len(q); got != 1 {
		t.Errorf("query param count = %d, want 1 (raw=%q)", got, gotRawQuery)
	}
}

// TestClient_LatestRetiredByRole_500_DoesNotMapToErrNoPredecessor — a
// non-404 server error MUST NOT wrap to ErrNoPredecessor. The saga
// step's no-op fallback must only fire on a genuine "no predecessor"
// response, not on a transient 5xx.
func TestClient_LatestRetiredByRole_500_DoesNotMapToErrNoPredecessor(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":"latest_retired_by_role_failed"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.LatestRetiredByRole(context.Background(), predecessorOrgID, predecessorRoleID)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrNoPredecessor) {
		t.Errorf("errors.Is(err, ErrNoPredecessor) = true on 500; err = %v", err)
	}
	if !errors.Is(err, ErrInternal) {
		t.Errorf("errors.Is(err, ErrInternal) = false on 500; err = %v", err)
	}
}

// TestClient_LatestRetiredByRole_403_PassesThrough — a 403 from the
// server (e.g. legacy claim without org) surfaces as ErrForbidden, NOT
// ErrNoPredecessor. The saga step uses the sentinel distinction to
// decide between "fall through to no-op" (ErrNoPredecessor only) and
// "bubble up the saga as a hard failure" (every other error class).
func TestClient_LatestRetiredByRole_403_PassesThrough(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"organization_required"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.LatestRetiredByRole(context.Background(), predecessorOrgID, predecessorRoleID)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if errors.Is(err, ErrNoPredecessor) {
		t.Errorf("errors.Is(err, ErrNoPredecessor) = true on 403; err = %v", err)
	}
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("errors.Is(err, ErrForbidden) = false on 403; err = %v", err)
	}
}
