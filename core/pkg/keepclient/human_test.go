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
	humanTestID     = "55555555-5555-4555-8555-555555555555"
	humanTestOrgID  = "66666666-6666-4666-8666-666666666666"
	humanTestSlack  = "U07ABCDE123"
	humanTestEmail  = "lead@example.test"
	humanTestRowID  = "77777777-7777-4777-8777-777777777777"
	humanTestWKID   = "88888888-8888-4888-8888-888888888888"
	humanTestLeadID = "99999999-9999-4999-8999-999999999999"
)

// TestClient_InsertHuman_HappyPath asserts the happy round-trip: a 201
// response decodes the `{"id":"…"}` envelope and the server-side decoded
// body carries the expected organization_id, display_name, email, and
// slack_user_id.
func TestClient_InsertHuman_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("Method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/v1/humans" {
			t.Errorf("Path = %q, want /v1/humans", r.URL.Path)
		}
		var got InsertHumanRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		if got.OrganizationID != humanTestOrgID {
			t.Errorf("OrganizationID = %q, want %q", got.OrganizationID, humanTestOrgID)
		}
		if got.DisplayName != "Lead Operator" {
			t.Errorf("DisplayName = %q, want %q", got.DisplayName, "Lead Operator")
		}
		if got.Email != humanTestEmail {
			t.Errorf("Email = %q, want %q", got.Email, humanTestEmail)
		}
		if got.SlackUserID != humanTestSlack {
			t.Errorf("SlackUserID = %q, want %q", got.SlackUserID, humanTestSlack)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"`+humanTestRowID+`"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	resp, err := c.InsertHuman(context.Background(), InsertHumanRequest{
		OrganizationID: humanTestOrgID,
		DisplayName:    "Lead Operator",
		Email:          humanTestEmail,
		SlackUserID:    humanTestSlack,
	})
	if err != nil {
		t.Fatalf("InsertHuman: %v", err)
	}
	if resp.ID != humanTestRowID {
		t.Errorf("ID = %q, want %q", resp.ID, humanTestRowID)
	}
}

// TestClient_InsertHuman_OmitsEmptyOptionals asserts the omitempty
// contract on the wire: an empty Email or SlackUserID must not be
// transmitted at all so the server never sees a stray empty key (and the
// unique-by-NULLs-distinct semantics on slack_user_id are preserved).
func TestClient_InsertHuman_OmitsEmptyOptionals(t *testing.T) {
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
		_, _ = io.WriteString(w, `{"id":"`+humanTestRowID+`"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if _, err := c.InsertHuman(context.Background(), InsertHumanRequest{
		OrganizationID: humanTestOrgID,
		DisplayName:    "Lead Operator",
	}); err != nil {
		t.Fatalf("InsertHuman: %v", err)
	}
	for _, forbidden := range []string{`"email"`, `"slack_user_id"`} {
		if strings.Contains(string(rawBody), forbidden) {
			t.Errorf("body included %s field; got %s", forbidden, rawBody)
		}
	}
	if !strings.Contains(string(rawBody), `"organization_id"`) {
		t.Errorf("body missing organization_id field; got %s", rawBody)
	}
	if !strings.Contains(string(rawBody), `"display_name"`) {
		t.Errorf("body missing display_name field; got %s", rawBody)
	}
}

// TestClient_InsertHuman_EmptyRequiredFields_Synchronous — empty
// OrganizationID or DisplayName returns ErrInvalidRequest synchronously
// without a network round-trip.
func TestClient_InsertHuman_EmptyRequiredFields_Synchronous(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))

	cases := []struct {
		name string
		req  InsertHumanRequest
	}{
		{"missing_org", InsertHumanRequest{DisplayName: "x"}},
		{"missing_name", InsertHumanRequest{OrganizationID: humanTestOrgID}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.InsertHuman(context.Background(), tc.req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
	if hits.Load() != 0 {
		t.Errorf("server received %d hits; want 0 (synchronous reject)", hits.Load())
	}
}

// TestClient_InsertHuman_NoFieldsForServerStamps — the typed struct
// physically excludes server-stamped fields so callers cannot push them
// through the wire shape.
func TestClient_InsertHuman_NoFieldsForServerStamps(t *testing.T) {
	t.Parallel()

	rt := reflect.TypeOf(InsertHumanRequest{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		name := strings.SplitN(tag, ",", 2)[0]
		switch {
		case strings.EqualFold(name, "id"), strings.EqualFold(f.Name, "ID"):
			t.Errorf("InsertHumanRequest must not expose an id field; found %q", f.Name)
		case strings.EqualFold(name, "created_at"), strings.EqualFold(f.Name, "CreatedAt"):
			t.Errorf("InsertHumanRequest must not expose created_at; found %q", f.Name)
		}
	}
}

// TestClient_InsertHuman_Conflict_409 — a 409 with
// `{"error":"slack_user_id_conflict"}` decodes to a *ServerError that
// matches both ErrConflict via errors.Is.
func TestClient_InsertHuman_Conflict_409(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"error":"slack_user_id_conflict"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.InsertHuman(context.Background(), InsertHumanRequest{
		OrganizationID: humanTestOrgID,
		DisplayName:    "x",
		SlackUserID:    humanTestSlack,
	})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want ErrConflict", err)
	}
	var sErr *ServerError
	if !errors.As(err, &sErr) {
		t.Fatalf("err = %v, want *ServerError", err)
	}
	if sErr.Code != "slack_user_id_conflict" {
		t.Errorf("ServerError.Code = %q, want slack_user_id_conflict", sErr.Code)
	}
}

// TestClient_LookupHumanBySlackID_HappyPath — round-trip: a 200 response
// decodes into the typed Human struct with nullable email surfacing as
// a non-nil pointer.
func TestClient_LookupHumanBySlackID_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("Method = %q, want GET", r.Method)
		}
		if r.URL.Path != "/v1/humans/by-slack/"+humanTestSlack {
			t.Errorf("Path = %q, want /v1/humans/by-slack/%s", r.URL.Path, humanTestSlack)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
            "id":"`+humanTestRowID+`",
            "organization_id":"`+humanTestOrgID+`",
            "display_name":"Lead Operator",
            "email":"`+humanTestEmail+`",
            "slack_user_id":"`+humanTestSlack+`",
            "created_at":"2026-05-01T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	got, err := c.LookupHumanBySlackID(context.Background(), humanTestSlack)
	if err != nil {
		t.Fatalf("LookupHumanBySlackID: %v", err)
	}
	if got.ID != humanTestRowID {
		t.Errorf("ID = %q, want %q", got.ID, humanTestRowID)
	}
	if got.Email == nil || *got.Email != humanTestEmail {
		t.Errorf("Email = %v, want %q", got.Email, humanTestEmail)
	}
	if got.SlackUserID == nil || *got.SlackUserID != humanTestSlack {
		t.Errorf("SlackUserID = %v, want %q", got.SlackUserID, humanTestSlack)
	}
}

// TestClient_LookupHumanBySlackID_NullEmail — a JSON `email: null`
// surfaces as a nil *string, distinguishable from the zero string.
func TestClient_LookupHumanBySlackID_NullEmail(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{
            "id":"`+humanTestRowID+`",
            "organization_id":"`+humanTestOrgID+`",
            "display_name":"x",
            "email":null,
            "slack_user_id":"`+humanTestSlack+`",
            "created_at":"2026-05-01T12:00:00Z"
        }`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	got, err := c.LookupHumanBySlackID(context.Background(), humanTestSlack)
	if err != nil {
		t.Fatalf("LookupHumanBySlackID: %v", err)
	}
	if got.Email != nil {
		t.Errorf("Email = %v, want nil (SQL NULL)", got.Email)
	}
}

// TestClient_LookupHumanBySlackID_NotFound_404 — a 404 surfaces as
// *ServerError matching ErrNotFound.
func TestClient_LookupHumanBySlackID_NotFound_404(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.LookupHumanBySlackID(context.Background(), "U_MISSING")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestClient_LookupHumanBySlackID_EmptyID_Synchronous — empty slackUserID
// returns ErrInvalidRequest synchronously without a network round-trip.
func TestClient_LookupHumanBySlackID_EmptyID_Synchronous(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, err := c.LookupHumanBySlackID(context.Background(), "")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}
	if hits.Load() != 0 {
		t.Errorf("server received %d hits; want 0 (synchronous reject)", hits.Load())
	}
}

// TestClient_LookupHumanBySlackID_PathEscape — caller-supplied values
// containing reserved URL characters are escaped so they cannot smuggle
// extra path segments.
func TestClient_LookupHumanBySlackID_PathEscape(t *testing.T) {
	t.Parallel()

	var gotRawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RawPath preserves the percent-encoded form; Path is the
		// decoded value. We must assert against RawPath because Path
		// would decode `%2F` back to `/` and mask the escape.
		gotRawPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	_, _ = c.LookupHumanBySlackID(context.Background(), "U/ABC?x")
	// The escaped form has `/` -> `%2F` and `?` -> `%3F`; the server's
	// raw request URI must not contain raw `/` or `?` after the prefix
	// (otherwise a caller-supplied value could smuggle path segments).
	if !strings.HasPrefix(gotRawPath, "/v1/humans/by-slack/") {
		t.Fatalf("path = %q, want /v1/humans/by-slack/ prefix", gotRawPath)
	}
	suffix := strings.TrimPrefix(gotRawPath, "/v1/humans/by-slack/")
	if strings.Contains(suffix, "/") {
		t.Errorf("suffix %q contains raw slash; want %%2F escape", suffix)
	}
	if strings.Contains(suffix, "?") {
		t.Errorf("suffix %q contains raw question mark; want %%3F escape", suffix)
	}
}

// TestClient_SetWatchkeeperLead_HappyPath — happy round-trip: 204 from
// the server returns nil, and the request body carries lead_human_id.
func TestClient_SetWatchkeeperLead_HappyPath(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Errorf("Method = %q, want PATCH", r.Method)
		}
		if r.URL.Path != "/v1/watchkeepers/"+humanTestWKID+"/lead" {
			t.Errorf("Path = %q, want /v1/watchkeepers/%s/lead", r.URL.Path, humanTestWKID)
		}
		var got setWatchkeeperLeadRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.LeadHumanID != humanTestLeadID {
			t.Errorf("LeadHumanID = %q, want %q", got.LeadHumanID, humanTestLeadID)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	if err := c.SetWatchkeeperLead(context.Background(), humanTestWKID, humanTestLeadID); err != nil {
		t.Errorf("SetWatchkeeperLead: %v", err)
	}
}

// TestClient_SetWatchkeeperLead_EmptyArgs_Synchronous — empty
// watchkeeperID or leadHumanID returns ErrInvalidRequest synchronously.
func TestClient_SetWatchkeeperLead_EmptyArgs_Synchronous(t *testing.T) {
	t.Parallel()

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	cases := []struct {
		name              string
		watchkeeperID, ld string
	}{
		{"empty_wk", "", humanTestLeadID},
		{"empty_lead", humanTestWKID, ""},
		{"both_empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := c.SetWatchkeeperLead(context.Background(), tc.watchkeeperID, tc.ld)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
	if hits.Load() != 0 {
		t.Errorf("server received %d hits; want 0 (synchronous reject)", hits.Load())
	}
}

// TestClient_SetWatchkeeperLead_NotFound_404 — a 404 response surfaces as
// *ServerError matching ErrNotFound (unknown watchkeeper id).
func TestClient_SetWatchkeeperLead_NotFound_404(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.SetWatchkeeperLead(context.Background(), humanTestWKID, humanTestLeadID)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// TestClient_SetWatchkeeperLead_FKViolation_400 — a 400 with
// `{"error":"invalid_lead_human_id"}` surfaces as *ServerError matching
// ErrInvalidRequest, with the code preserved on the typed value.
func TestClient_SetWatchkeeperLead_FKViolation_400(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_lead_human_id"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	err := c.SetWatchkeeperLead(context.Background(), humanTestWKID, humanTestLeadID)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}
	var sErr *ServerError
	if !errors.As(err, &sErr) {
		t.Fatalf("err = %v, want *ServerError", err)
	}
	if sErr.Code != "invalid_lead_human_id" {
		t.Errorf("ServerError.Code = %q, want invalid_lead_human_id", sErr.Code)
	}
}
