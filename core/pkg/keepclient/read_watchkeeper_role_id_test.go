package keepclient

// read_watchkeeper_role_id_test.go covers the M7.1.a `role_id`
// projection on the keepclient.Watchkeeper struct. The sibling tests in
// `read_watchkeeper_test.go` exercise the legacy NULL-projection happy
// paths; this file pins the non-NULL round-trip on the GET path and the
// list path.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

const wkTestRoleID = "frontline-watchkeeper"

// TestClient_GetWatchkeeper_RoleIDPresent — a server response carrying a
// non-empty `role_id` field decodes into a non-nil `*string`. Pins the
// M7.1.a projection so a regression that drops the JSON tag surfaces as
// a nil RoleID on a row known to carry one.
func TestClient_GetWatchkeeper_RoleIDPresent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"`+wkTestRowID+`",
            "manifest_id":"`+wkTestManifestID+`",
            "lead_human_id":"`+wkTestLeadHumanID+`",
            "active_manifest_version_id":null,
            "status":"active",
            "spawned_at":"2026-05-01T10:00:00Z",
            "retired_at":null,
            "archive_uri":null,
            "role_id":"`+wkTestRoleID+`",
            "created_at":"2026-04-30T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	wk, err := c.GetWatchkeeper(context.Background(), wkTestRowID)
	if err != nil {
		t.Fatalf("GetWatchkeeper: %v", err)
	}
	if wk.RoleID == nil {
		t.Fatalf("RoleID = nil, want non-nil with value %q", wkTestRoleID)
	}
	if *wk.RoleID != wkTestRoleID {
		t.Errorf("RoleID = %q, want %q", *wk.RoleID, wkTestRoleID)
	}
}

// TestClient_GetWatchkeeper_RoleIDNull — a server response with an
// explicit `"role_id":null` decodes to a nil `*string`, matching the
// SQL-NULL semantics of the M7.1.a column on every row predating the
// upstream writers + every legacy insert that omits the optional field.
func TestClient_GetWatchkeeper_RoleIDNull(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "id":"`+wkTestRowID+`",
            "manifest_id":"`+wkTestManifestID+`",
            "lead_human_id":"`+wkTestLeadHumanID+`",
            "active_manifest_version_id":null,
            "status":"active",
            "spawned_at":"2026-05-01T10:00:00Z",
            "retired_at":null,
            "archive_uri":null,
            "role_id":null,
            "created_at":"2026-04-30T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	wk, err := c.GetWatchkeeper(context.Background(), wkTestRowID)
	if err != nil {
		t.Fatalf("GetWatchkeeper: %v", err)
	}
	if wk.RoleID != nil {
		t.Errorf("RoleID = %v, want nil for explicit null", wk.RoleID)
	}
}

// TestClient_ListWatchkeepers_RoleIDProjected — the list-path response
// carries the role_id per-row and the client decodes the pointer field
// the same way it does on the single-row path. Mirrors the
// `TestClient_ListWatchkeepers_HappyPath` shape in
// `read_watchkeeper_test.go` but with a single row carrying a non-NULL
// role_id so the projection is the focal assertion.
func TestClient_ListWatchkeepers_RoleIDProjected(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
            "items":[
                {
                    "id":"`+wkTestRowID+`",
                    "manifest_id":"`+wkTestManifestID+`",
                    "lead_human_id":"`+wkTestLeadHumanID+`",
                    "active_manifest_version_id":null,
                    "status":"active",
                    "spawned_at":"2026-05-01T10:00:00Z",
                    "retired_at":null,
                    "archive_uri":null,
                    "role_id":"`+wkTestRoleID+`",
                    "created_at":"2026-04-30T12:00:00Z"
                }
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
	if len(resp.Items) != 1 {
		t.Fatalf("Items len = %d, want 1", len(resp.Items))
	}
	if resp.Items[0].RoleID == nil || *resp.Items[0].RoleID != wkTestRoleID {
		t.Errorf("Items[0].RoleID = %v, want %q", resp.Items[0].RoleID, wkTestRoleID)
	}
}
