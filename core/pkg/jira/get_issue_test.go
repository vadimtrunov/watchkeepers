package jira

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGetIssue_RejectsEmptyKey(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called for empty key")
	})
	_, err := c.GetIssue(context.Background(), "", nil)
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err = %v; want wrapped ErrInvalidArgs", err)
	}
}

func TestGetIssue_HappyPath_TypedFields(t *testing.T) {
	t.Parallel()
	const respJSON = `{
		"id": "12345",
		"key": "PROJ-99",
		"fields": {
			"summary": "release blocker",
			"status": {"id": "10000", "name": "In Review"},
			"assignee": {"accountId": "557058:assignee"},
			"reporter": {"accountId": "557058:reporter"},
			"created": "2024-09-15T14:30:00.000+0000",
			"updated": "2024-09-15T15:00:00.000+0000",
			"customfield_10001": "team-x"
		}
	}`
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Errorf("Method = %s; want GET", r.Method)
		}
		if r.URL.Path != "/rest/api/3/issue/PROJ-99" {
			t.Errorf("Path = %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, respJSON)
	})
	got, err := c.GetIssue(context.Background(), "PROJ-99", nil)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.Key != "PROJ-99" {
		t.Errorf("Key = %q", got.Key)
	}
	if got.ID != "12345" {
		t.Errorf("ID = %q", got.ID)
	}
	if got.Summary != "release blocker" {
		t.Errorf("Summary = %q", got.Summary)
	}
	if got.Status != "In Review" {
		t.Errorf("Status = %q", got.Status)
	}
	if got.AssigneeID != "557058:assignee" {
		t.Errorf("AssigneeID = %q", got.AssigneeID)
	}
	if got.ReporterID != "557058:reporter" {
		t.Errorf("ReporterID = %q", got.ReporterID)
	}
	if _, ok := got.Fields["customfield_10001"]; !ok {
		t.Error("custom field missing from raw Fields map")
	}
	if got.Created.IsZero() || got.Updated.IsZero() {
		t.Error("Created/Updated time fields are zero")
	}
}

func TestGetIssue_FieldsQueryEncoded(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		got := r.URL.Query().Get("fields")
		if got != "summary,status" {
			t.Errorf("fields query = %q; want \"summary,status\"", got)
		}
		_, _ = io.WriteString(w, `{"key":"PROJ-1","fields":{"summary":"x"}}`)
	})
	_, err := c.GetIssue(context.Background(), "PROJ-1", []string{"summary", "status"})
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
}

func TestGetIssue_NoFields_NoQueryParam(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("fields"); got != "" {
			t.Errorf("fields query = %q; want empty when caller passed nil", got)
		}
		_, _ = io.WriteString(w, `{"key":"PROJ-1","fields":{}}`)
	})
	_, _ = c.GetIssue(context.Background(), "PROJ-1", nil)
}

func TestGetIssue_NullAssignee_EmptyAccessor(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"key":"PROJ-1","fields":{"assignee":null,"reporter":null}}`)
	})
	got, err := c.GetIssue(context.Background(), "PROJ-1", nil)
	if err != nil {
		t.Fatalf("GetIssue: %v", err)
	}
	if got.AssigneeID != "" {
		t.Errorf("AssigneeID = %q; want empty for null assignee", got.AssigneeID)
	}
	if got.ReporterID != "" {
		t.Errorf("ReporterID = %q; want empty for null reporter", got.ReporterID)
	}
}

func TestGetIssue_404Wrapped(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errorMessages":["Issue does not exist or you do not have permission to see it."]}`)
	})
	_, err := c.GetIssue(context.Background(), "PROJ-99", nil)
	if !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("err = %v; want ErrIssueNotFound", err)
	}
	if !strings.Contains(err.Error(), "PROJ-99") {
		t.Errorf("err = %q; expected wrap to include the issue key", err)
	}
}

func TestGetIssue_RejectsMalformedKey_NoNetwork(t *testing.T) {
	t.Parallel()
	cases := []string{
		"PROJ",                 // missing dash + number
		"PROJ-",                // empty number
		"PROJ-0",               // leading-zero number rejected
		"PROJ-00",              // leading-zero number rejected
		"proj-1",               // lowercase project
		"PROJ-1/extra",         // trailing path segment
		"../../etc/passwd",     // path traversal
		"PROJ-1?fields=secret", // query injection
		"PROJ-1#fragment",      // fragment injection
		" PROJ-1",              // leading whitespace
	}
	for _, k := range cases {
		t.Run(k, func(t *testing.T) {
			c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
				t.Fatalf("HTTP must not be called for malformed key %q", k)
			})
			_, err := c.GetIssue(context.Background(), IssueKey(k), nil)
			if !errors.Is(err, ErrInvalidArgs) {
				t.Errorf("GetIssue(%q) err = %v; want wrapped ErrInvalidArgs", k, err)
			}
		})
	}
}

func TestIssueWire_NumericIDFallback(t *testing.T) {
	t.Parallel()
	got := jsonNumberOrString([]byte(`12345`))
	if got != "12345" {
		t.Errorf("jsonNumberOrString(number) = %q; want \"12345\"", got)
	}
	got = jsonNumberOrString([]byte(`"abc"`))
	if got != "abc" {
		t.Errorf("jsonNumberOrString(string) = %q; want \"abc\"", got)
	}
	got = jsonNumberOrString(nil)
	if got != "" {
		t.Errorf("jsonNumberOrString(nil) = %q; want empty", got)
	}
}
