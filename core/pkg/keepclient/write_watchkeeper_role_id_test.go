package keepclient

// write_watchkeeper_role_id_test.go covers the M7.1.a `role_id` field
// on [InsertWatchkeeperRequest]. The sibling tests in
// `write_watchkeeper_test.go` exercise the legacy omitempty paths;
// this file pins the on-wire shape for the non-empty case + the
// omitempty fold for the empty case.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestClient_InsertWatchkeeper_WithRoleID_OnWire asserts that a non-empty
// RoleID field is transmitted on the wire so the server's
// `parseInsertWatchkeeperRequest` decoder sees the `role_id` key and the
// matching INSERT statement binds it at the $5 slot. The decoder rejects
// unknown keys, so the wire spelling must match `role_id` exactly.
func TestClient_InsertWatchkeeper_WithRoleID_OnWire(t *testing.T) {
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
		_, _ = io.WriteString(w, `{"id":"`+wkTestRowID+`"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.InsertWatchkeeper(context.Background(), InsertWatchkeeperRequest{
		ManifestID:  wkTestManifestID,
		LeadHumanID: wkTestLeadHumanID,
		RoleID:      "frontline-watchkeeper",
	})
	if err != nil {
		t.Fatalf("InsertWatchkeeper: %v", err)
	}
	if resp.ID != wkTestRowID {
		t.Errorf("ID = %q, want %q", resp.ID, wkTestRowID)
	}
	if !strings.Contains(string(rawBody), `"role_id":"frontline-watchkeeper"`) {
		t.Errorf("body missing role_id field; got %s", rawBody)
	}
}

// TestClient_InsertWatchkeeper_OmitsEmptyRoleID asserts the omitempty
// contract: an empty RoleID must not be transmitted at all so the
// server's DisallowUnknownFields decoder never sees a stray empty key
// (which would be folded into SQL NULL anyway, but the on-wire shape
// matters for forward-compat with any future stricter validator).
func TestClient_InsertWatchkeeper_OmitsEmptyRoleID(t *testing.T) {
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
		_, _ = io.WriteString(w, `{"id":"`+wkTestRowID+`"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if _, err := c.InsertWatchkeeper(context.Background(), InsertWatchkeeperRequest{
		ManifestID:  wkTestManifestID,
		LeadHumanID: wkTestLeadHumanID,
	}); err != nil {
		t.Fatalf("InsertWatchkeeper: %v", err)
	}
	if strings.Contains(string(rawBody), `"role_id"`) {
		t.Errorf("body included role_id field; got %s", rawBody)
	}
}
