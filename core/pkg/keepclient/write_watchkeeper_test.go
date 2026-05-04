package keepclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	wkTestManifestID  = "11111111-1111-4111-8111-111111111111"
	wkTestLeadHumanID = "22222222-2222-4222-8222-222222222222"
	wkTestActiveVerID = "33333333-3333-4333-8333-333333333333"
	wkTestRowID       = "44444444-4444-4444-8444-444444444444"
)

// TestClient_InsertWatchkeeper_HappyPath asserts the happy round-trip: a 201
// response decodes the `{"id":"…"}` envelope and the server-side decoded
// body carries the expected manifest_id / lead_human_id / active version id.
func TestClient_InsertWatchkeeper_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/watchkeepers" {
			t.Errorf("Path = %q, want /v1/watchkeepers", r.URL.Path)
		}
		var got InsertWatchkeeperRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if got.ManifestID != wkTestManifestID {
			t.Errorf("ManifestID = %q, want %q", got.ManifestID, wkTestManifestID)
		}
		if got.LeadHumanID != wkTestLeadHumanID {
			t.Errorf("LeadHumanID = %q, want %q", got.LeadHumanID, wkTestLeadHumanID)
		}
		if got.ActiveManifestVersionID != wkTestActiveVerID {
			t.Errorf("ActiveManifestVersionID = %q, want %q", got.ActiveManifestVersionID, wkTestActiveVerID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"`+wkTestRowID+`"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.InsertWatchkeeper(context.Background(), InsertWatchkeeperRequest{
		ManifestID:              wkTestManifestID,
		LeadHumanID:             wkTestLeadHumanID,
		ActiveManifestVersionID: wkTestActiveVerID,
	})
	if err != nil {
		t.Fatalf("InsertWatchkeeper: %v", err)
	}
	if resp.ID != wkTestRowID {
		t.Errorf("ID = %q, want %q", resp.ID, wkTestRowID)
	}
}

// TestClient_InsertWatchkeeper_OmitsEmptyOptional asserts the omitempty
// contract on the wire: an empty ActiveManifestVersionID must not be
// transmitted at all so the server's DisallowUnknownFields decoder never
// sees a stray empty key. Required fields stay on the wire.
func TestClient_InsertWatchkeeper_OmitsEmptyOptional(t *testing.T) {
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
	if strings.Contains(string(rawBody), `"active_manifest_version_id"`) {
		t.Errorf("body included active_manifest_version_id field; got %s", rawBody)
	}
	if !strings.Contains(string(rawBody), `"manifest_id"`) {
		t.Errorf("body missing manifest_id field; got %s", rawBody)
	}
}

// TestClient_InsertWatchkeeper_RejectsClientSuppliedServerStamps asserts the
// security AC: the InsertWatchkeeperRequest type must not have status,
// spawned_at, or retired_at fields, so callers physically cannot push them
// through the typed surface.
func TestClient_InsertWatchkeeper_RejectsClientSuppliedServerStamps(t *testing.T) {
	t.Parallel()

	rt := reflect.TypeOf(InsertWatchkeeperRequest{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		name := strings.SplitN(tag, ",", 2)[0]
		switch {
		case strings.EqualFold(name, "status"), strings.EqualFold(f.Name, "Status"):
			t.Errorf("InsertWatchkeeperRequest must not expose a status field; found %q", f.Name)
		case strings.EqualFold(name, "spawned_at"), strings.EqualFold(f.Name, "SpawnedAt"):
			t.Errorf("InsertWatchkeeperRequest must not expose spawned_at; found %q", f.Name)
		case strings.EqualFold(name, "retired_at"), strings.EqualFold(f.Name, "RetiredAt"):
			t.Errorf("InsertWatchkeeperRequest must not expose retired_at; found %q", f.Name)
		}
	}
}

// TestClient_InsertWatchkeeper_EmptyUUID_ErrInvalidRequest asserts that an
// empty manifest_id rejects synchronously with ErrInvalidRequest and never
// contacts the server.
func TestClient_InsertWatchkeeper_EmptyUUID_ErrInvalidRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		req  InsertWatchkeeperRequest
	}{
		{"empty_manifest_id", InsertWatchkeeperRequest{LeadHumanID: wkTestLeadHumanID}},
		{"empty_lead_human_id", InsertWatchkeeperRequest{ManifestID: wkTestManifestID}},
		{"both_empty", InsertWatchkeeperRequest{}},
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
			_, err := c.InsertWatchkeeper(context.Background(), tc.req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("network hits = %d, want 0", got)
			}
		})
	}
}

// TestClient_InsertWatchkeeper_NoTokenSource_ErrNoTokenSource asserts that
// calling InsertWatchkeeper without configuring WithTokenSource returns
// ErrNoTokenSource synchronously and does NOT contact the network.
func TestClient_InsertWatchkeeper_NoTokenSource_ErrNoTokenSource(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	_, err := c.InsertWatchkeeper(context.Background(), InsertWatchkeeperRequest{
		ManifestID:  wkTestManifestID,
		LeadHumanID: wkTestLeadHumanID,
	})
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestClient_UpdateWatchkeeperStatus_HappyPath asserts the happy round-trip:
// the client sends the right path + body and decodes a 204 No Content.
func TestClient_UpdateWatchkeeperStatus_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("Method = %q, want PATCH", r.Method)
		}
		if want := "/v1/watchkeepers/" + wkTestRowID + "/status"; r.URL.Path != want {
			t.Errorf("Path = %q, want %q", r.URL.Path, want)
		}
		var got updateWatchkeeperStatusRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if got.Status != "active" {
			t.Errorf("Status = %q, want active", got.Status)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if err := c.UpdateWatchkeeperStatus(context.Background(), wkTestRowID, "active"); err != nil {
		t.Fatalf("UpdateWatchkeeperStatus: %v", err)
	}
}

// TestClient_UpdateWatchkeeperStatus_BadStatus_ErrInvalidRequest asserts
// out-of-set status values reject synchronously with ErrInvalidRequest.
func TestClient_UpdateWatchkeeperStatus_BadStatus_ErrInvalidRequest(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, status string
	}{
		{"weird", "weird"},
		{"empty", ""},
		{"pending_not_a_target", "pending"},
		{"uppercase", "ACTIVE"},
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
			err := c.UpdateWatchkeeperStatus(context.Background(), wkTestRowID, tc.status)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if got := atomic.LoadInt32(&hits); got != 0 {
				t.Errorf("network hits = %d, want 0", got)
			}
		})
	}
}

// TestClient_UpdateWatchkeeperStatus_EmptyID_ErrInvalidRequest asserts an
// empty id rejects synchronously without a network round-trip.
func TestClient_UpdateWatchkeeperStatus_EmptyID_ErrInvalidRequest(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.UpdateWatchkeeperStatus(context.Background(), "", "active")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestClient_UpdateWatchkeeperStatus_400_TransitionMaps asserts that a 400
// with `{"error":"invalid_status_transition"}` surfaces as a *ServerError
// matching both ErrInvalidRequest (the status) and the more specific
// ErrInvalidStatusTransition sentinel.
func TestClient_UpdateWatchkeeperStatus_400_TransitionMaps(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_status_transition"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.UpdateWatchkeeperStatus(context.Background(), wkTestRowID, "active")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrInvalidStatusTransition) {
		t.Errorf("errors.Is(err, ErrInvalidStatusTransition) = false; err = %v", err)
	}
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("errors.Is(err, ErrInvalidRequest) = false; err = %v", err)
	}
	var se *ServerError
	if !errors.As(err, &se) || se.Status != http.StatusBadRequest {
		t.Errorf("ServerError.Status = %v, want 400 (err=%v)", se, err)
	}
	if se != nil && se.Code != "invalid_status_transition" {
		t.Errorf("ServerError.Code = %q, want invalid_status_transition", se.Code)
	}
}

// TestClient_UpdateWatchkeeperStatus_NoTokenSource_ErrNoTokenSource asserts
// that calling without a token source returns ErrNoTokenSource synchronously.
func TestClient_UpdateWatchkeeperStatus_NoTokenSource_ErrNoTokenSource(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	err := c.UpdateWatchkeeperStatus(context.Background(), wkTestRowID, "active")
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}
