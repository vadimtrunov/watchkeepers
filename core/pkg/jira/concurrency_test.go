package jira

import (
	"context"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
)

// osReadFile is a thin alias for os.ReadFile, exposed so the
// update_fields_test source-grep test does not need an additional
// import (its file kept tight to its concern). The aliasing avoids
// pulling `os` into multiple test files.
func osReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

// TestClient_ConcurrentSearch_NoRace runs 16 concurrent Search calls
// against a shared httptest.Server using a shared [Client]. Required
// per the package's "safe for concurrent use after construction"
// contract; runs under `go test -race` to surface mutable-state slips.
func TestClient_ConcurrentSearch_NoRace(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"isLast":true,"issues":[]}`)
	})

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, err := c.Search(context.Background(), "project = PROJ", SearchOptions{})
			if err != nil {
				t.Errorf("Search: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != goroutines {
		t.Errorf("server calls = %d; want %d", got, goroutines)
	}
}

func TestClient_ConcurrentMixedOps_NoRace(t *testing.T) {
	t.Parallel()
	c, _ := newTestClient(
		t,
		func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "POST" && r.URL.Path == "/rest/api/3/search/jql":
				_, _ = io.WriteString(w, `{"isLast":true,"issues":[]}`)
			case r.Method == "GET":
				_, _ = io.WriteString(w, `{"key":"PROJ-1","fields":{}}`)
			case r.Method == "POST" && r.URL.Path == "/rest/api/3/issue/PROJ-1/comment":
				_, _ = io.WriteString(w, `{"id":"1","author":{"accountId":"x"},"body":{"type":"doc","version":1,"content":[]},"created":"2024-09-15T14:30:00.000+0000"}`)
			case r.Method == "PUT":
				w.WriteHeader(http.StatusNoContent)
			default:
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
		},
		WithFieldWhitelist("summary"),
	)

	const groups = 4
	const perGroup = 4
	var wg sync.WaitGroup
	wg.Add(groups * perGroup)
	for i := 0; i < perGroup; i++ {
		go func() {
			defer wg.Done()
			_, err := c.Search(context.Background(), "project = PROJ", SearchOptions{})
			if err != nil {
				t.Errorf("Search: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			_, err := c.GetIssue(context.Background(), "PROJ-1", nil)
			if err != nil {
				t.Errorf("GetIssue: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			_, err := c.AddComment(context.Background(), "PROJ-1", "hello")
			if err != nil {
				t.Errorf("AddComment: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			err := c.UpdateFields(context.Background(), "PROJ-1", map[string]any{"summary": "x"})
			if err != nil {
				t.Errorf("UpdateFields: %v", err)
			}
		}()
	}
	wg.Wait()
}
