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

func TestAddComment_RejectsEmptyKey(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called for empty key")
	})
	_, err := c.AddComment(context.Background(), "", "hello")
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err = %v; want ErrInvalidArgs", err)
	}
}

func TestAddComment_RejectsEmptyBody(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatalf("HTTP must not be called for empty body")
	})
	_, err := c.AddComment(context.Background(), "PROJ-1", "")
	if !errors.Is(err, ErrInvalidArgs) {
		t.Fatalf("err = %v; want ErrInvalidArgs", err)
	}
}

func TestAddComment_HappyPath_ADFRoundTrip(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/3/issue/PROJ-1/comment" {
			t.Errorf("Path = %s", r.URL.Path)
		}
		var got map[string]any
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		body, ok := got["body"].(map[string]any)
		if !ok {
			t.Fatalf("body field is not an object: %v", got["body"])
		}
		if body["type"] != "doc" {
			t.Errorf("ADF root type = %v; want doc", body["type"])
		}
		if v, ok := body["version"].(float64); !ok || v != 1 {
			t.Errorf("ADF version = %v; want 1", body["version"])
		}
		// Echo back a server-normalised ADF body.
		_, _ = io.WriteString(w, `{
			"id": "10100",
			"author": {"accountId": "557058:author"},
			"body": {
				"type": "doc",
				"version": 1,
				"content": [
					{"type": "paragraph", "content": [{"type": "text", "text": "hello world"}]}
				]
			},
			"created": "2024-09-15T14:30:00.000+0000"
		}`)
	})
	got, err := c.AddComment(context.Background(), "PROJ-1", "hello world")
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if got.ID != "10100" {
		t.Errorf("ID = %q", got.ID)
	}
	if got.AuthorID != "557058:author" {
		t.Errorf("AuthorID = %q", got.AuthorID)
	}
	if got.Body != "hello world" {
		t.Errorf("Body (projected) = %q; want \"hello world\"", got.Body)
	}
	if got.RawBody == nil {
		t.Error("RawBody is nil; expected verbatim ADF preservation")
	}
	if got.Created.IsZero() {
		t.Error("Created is zero")
	}
}

func TestAddComment_MultiLineWrap(t *testing.T) {
	t.Parallel()
	const want = "line one\nline two\n\nline four"
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		body, _ := got["body"].(map[string]any)
		content, _ := body["content"].([]any)
		if len(content) != 4 {
			t.Errorf("paragraph count = %d; want 4 (line1, line2, empty, line4)", len(content))
		}
		// Echo verbatim so projection round-trips.
		out := map[string]any{
			"id":      "1",
			"author":  map[string]any{"accountId": "x"},
			"body":    body,
			"created": "2024-09-15T14:30:00.000+0000",
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	got, err := c.AddComment(context.Background(), "PROJ-1", want)
	if err != nil {
		t.Fatalf("AddComment: %v", err)
	}
	if got.Body != want {
		t.Errorf("round-trip Body = %q; want %q", got.Body, want)
	}
}

func TestADFExtractText_HandlesMention(t *testing.T) {
	t.Parallel()
	const adf = `{
		"type": "doc",
		"version": 1,
		"content": [{
			"type": "paragraph",
			"content": [
				{"type": "text", "text": "ping "},
				{"type": "mention", "attrs": {"id": "557058:abc", "text": "@Alice"}},
				{"type": "text", "text": " please review"}
			]
		}]
	}`
	got := adfExtractText([]byte(adf))
	if !strings.Contains(got, "@Alice") {
		t.Errorf("projection lost mention: got %q", got)
	}
	if !strings.HasPrefix(got, "ping ") || !strings.HasSuffix(got, " please review") {
		t.Errorf("projection text = %q", got)
	}
}

func TestADFExtractText_HandlesHardBreak(t *testing.T) {
	t.Parallel()
	const adf = `{
		"type": "doc",
		"version": 1,
		"content": [{
			"type": "paragraph",
			"content": [
				{"type": "text", "text": "a"},
				{"type": "hardBreak"},
				{"type": "text", "text": "b"}
			]
		}]
	}`
	got := adfExtractText([]byte(adf))
	if got != "a\nb" {
		t.Errorf("projection = %q; want \"a\\nb\"", got)
	}
}

func TestADFExtractText_MentionWithNilAttrsDoesNotPanic(t *testing.T) {
	t.Parallel()
	const adf = `{
		"type": "doc",
		"version": 1,
		"content": [{
			"type": "paragraph",
			"content": [
				{"type": "text", "text": "ping "},
				{"type": "mention"},
				{"type": "text", "text": " review"}
			]
		}]
	}`
	got := adfExtractText([]byte(adf))
	if got != "ping  review" {
		t.Errorf("projection = %q; want \"ping  review\" (mention with nil Attrs collapses to empty, no panic)", got)
	}
}

func TestADFExtractText_MentionWithNonStringTextAttr(t *testing.T) {
	t.Parallel()
	const adf = `{
		"type": "doc",
		"version": 1,
		"content": [{
			"type": "paragraph",
			"content": [
				{"type": "mention", "attrs": {"id": 12345}}
			]
		}]
	}`
	got := adfExtractText([]byte(adf))
	if got != "" {
		t.Errorf("projection = %q; want \"\" — mention with non-string text attr falls through to empty", got)
	}
}

func TestADFExtractText_HandlesUnknownNodeBestEffort(t *testing.T) {
	t.Parallel()
	const adf = `{
		"type": "doc",
		"version": 1,
		"content": [{
			"type": "codeBlock",
			"content": [{"type": "text", "text": "go vet ./..."}]
		}]
	}`
	got := adfExtractText([]byte(adf))
	if got != "go vet ./..." {
		t.Errorf("projection of unknown block = %q; want \"go vet ./...\"", got)
	}
}

func TestADFExtractText_GarbageReturnsEmpty(t *testing.T) {
	t.Parallel()
	if got := adfExtractText([]byte("not-json")); got != "" {
		t.Errorf("garbage projection = %q; want empty", got)
	}
	if got := adfExtractText(nil); got != "" {
		t.Errorf("nil projection = %q; want empty", got)
	}
}

func TestAddComment_404Wrapped(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"errorMessages":["Issue does not exist."]}`)
	})
	_, err := c.AddComment(context.Background(), "PROJ-99", "hi")
	if !errors.Is(err, ErrIssueNotFound) {
		t.Fatalf("err = %v; want ErrIssueNotFound", err)
	}
}
