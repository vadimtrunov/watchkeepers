package jira

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSearch_RejectsEmptyJQL(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called for empty JQL")
	})
	_, err := c.Search(context.Background(), "", SearchOptions{})
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err = %v; want wrapped ErrInvalidArgs", err)
	}
}

func TestSearch_HappyPath_RequestEncoding(t *testing.T) {
	t.Parallel()
	type capture struct {
		method      string
		path        string
		contentType string
		body        map[string]any
	}
	var seen capture
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		seen.method = r.Method
		seen.path = r.URL.Path
		seen.contentType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&seen.body)
		_, _ = io.WriteString(w, `{"isLast":true,"issues":[]}`)
	})
	_, err := c.Search(context.Background(), "project = PROJ", SearchOptions{
		Fields:     []string{"summary", "status"},
		MaxResults: 25,
		PageToken:  "cursor-x",
	})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if seen.method != "POST" {
		t.Errorf("Method = %s; want POST", seen.method)
	}
	if seen.path != "/rest/api/3/search/jql" {
		t.Errorf("Path = %s", seen.path)
	}
	if seen.contentType != "application/json" {
		t.Errorf("Content-Type = %s", seen.contentType)
	}
	if seen.body["jql"] != `project = PROJ` {
		t.Errorf("body.jql = %v", seen.body["jql"])
	}
	if seen.body["nextPageToken"] != "cursor-x" {
		t.Errorf("body.nextPageToken = %v", seen.body["nextPageToken"])
	}
	if v, ok := seen.body["maxResults"].(float64); !ok || v != 25 {
		t.Errorf("body.maxResults = %v; want 25", seen.body["maxResults"])
	}
	if fs, ok := seen.body["fields"].([]any); !ok || len(fs) != 2 {
		t.Errorf("body.fields = %v; want [summary, status]", seen.body["fields"])
	}
}

func TestSearch_HappyPath_ResponseDecode(t *testing.T) {
	t.Parallel()
	const respJSON = `{
		"isLast": false,
		"nextPageToken": "abc123",
		"issues": [
			{
				"id": "10001",
				"key": "PROJ-1",
				"fields": {
					"summary": "first issue",
					"status": {"id": "1", "name": "To Do"},
					"assignee": {"accountId": "557058:abc"},
					"reporter": {"accountId": "557058:def"},
					"created": "2024-09-15T14:30:00.000+0000",
					"updated": "2024-09-15T15:00:00.000+0000"
				}
			},
			{
				"id": "10002",
				"key": "PROJ-2",
				"fields": {
					"summary": "second issue",
					"status": {"id": "3", "name": "In Progress"},
					"assignee": null,
					"reporter": {"accountId": "557058:ghi"}
				}
			}
		]
	}`
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, respJSON)
	})
	res, err := c.Search(context.Background(), "project = PROJ", SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.IsLast || res.NextPageToken != "abc123" || len(res.Issues) != 2 {
		t.Fatalf("page envelope = %+v; want IsLast=false NextPageToken=abc123 Issues=2", res)
	}
	first := res.Issues[0]
	wantFirst := struct {
		Key, ID, Summary, Status, AssigneeID, ReporterID string
	}{"PROJ-1", "10001", "first issue", "To Do", "557058:abc", "557058:def"}
	gotFirst := struct {
		Key, ID, Summary, Status, AssigneeID, ReporterID string
	}{string(first.Key), first.ID, first.Summary, first.Status, first.AssigneeID, first.ReporterID}
	if gotFirst != wantFirst {
		t.Errorf("first issue typed fields = %+v; want %+v", gotFirst, wantFirst)
	}
	if first.Created.IsZero() || first.Fields == nil {
		t.Errorf("first.Created.IsZero=%v Fields-nil=%v; both should be populated", first.Created.IsZero(), first.Fields == nil)
	}
	if second := res.Issues[1]; second.AssigneeID != "" {
		t.Errorf("second.AssigneeID = %q; want empty for null assignee", second.AssigneeID)
	}
}

func TestSearch_400JQLError_ReturnsErrInvalidJQL(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errorMessages":["The JQL query is invalid: Field 'foo' does not exist."],"errors":{}}`)
	})
	_, err := c.Search(context.Background(), "foo = 1", SearchOptions{})
	if !errors.Is(err, ErrInvalidJQL) {
		t.Fatalf("err = %v; want wrapped ErrInvalidJQL", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v; want APIError in chain", err)
	}
	if len(apiErr.Messages) == 0 || !strings.Contains(apiErr.Messages[0], "JQL") {
		t.Errorf("Messages = %v; expected to carry the Atlassian error message", apiErr.Messages)
	}
}

func TestSearch_400NonJQLMessage_RemainsAPIErrorOnly(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"errorMessages":["The next page token is invalid"]}`)
	})
	_, err := c.Search(context.Background(), "project = PROJ", SearchOptions{PageToken: "stale-cursor"}) //nolint:gosec // G101: synthetic test cursor (PageToken is opaque, not a credential).
	if err == nil {
		t.Fatal("expected error on 400")
	}
	if errors.Is(err, ErrInvalidJQL) {
		t.Fatalf("err = %v; must NOT alias ErrInvalidJQL when message lacks 'jql' — discriminate stale-cursor / adapter-bug from operator-bad-JQL", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v; want raw APIError in chain", err)
	}
	if apiErr.Status != 400 {
		t.Errorf("Status = %d", apiErr.Status)
	}
}

func TestSearch_BrokenContract_IsLastFalseEmptyToken_GuardedByAdapter(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"isLast":false,"issues":[],"nextPageToken":""}`)
	})
	_, err := c.Search(context.Background(), "project = PROJ", SearchOptions{})
	if err == nil {
		t.Fatal("expected error: server returned isLast=false with empty NextPageToken — naive caller loops infinitely")
	}
	if !errors.Is(err, ErrInvalidArgs) {
		t.Errorf("err = %v; want wrapped ErrInvalidArgs (programmer-detectable contract violation)", err)
	}
}

func TestSearch_OmitsEmptyOptionalFields(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, ok := got["fields"]; ok {
			t.Errorf("body should not include fields when empty; got %v", got["fields"])
		}
		if _, ok := got["maxResults"]; ok {
			t.Errorf("body should not include maxResults when zero; got %v", got["maxResults"])
		}
		if _, ok := got["nextPageToken"]; ok {
			t.Errorf("body should not include nextPageToken when empty; got %v", got["nextPageToken"])
		}
		_, _ = io.WriteString(w, `{"isLast":true,"issues":[]}`)
	})
	_, err := c.Search(context.Background(), "project = PROJ", SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
}

func TestSearch_EmptyResultIsLast(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"isLast":true,"issues":[]}`)
	})
	res, err := c.Search(context.Background(), "project = NONE", SearchOptions{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !res.IsLast {
		t.Error("IsLast should be true on empty terminal page")
	}
	if len(res.Issues) != 0 {
		t.Errorf("Issues = %d; want 0", len(res.Issues))
	}
	if res.NextPageToken != "" {
		t.Errorf("NextPageToken = %q; want empty on terminal page", res.NextPageToken)
	}
}
