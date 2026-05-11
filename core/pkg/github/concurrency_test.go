package github

import (
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

// TestClient_ConcurrentListPullRequests_NoRace runs 16 concurrent
// ListPullRequests calls against a shared httptest.Server using a
// shared [Client]. Required per the package's "safe for concurrent
// use after construction" contract; runs under `go test -race` to
// surface mutable-state slips.
func TestClient_ConcurrentListPullRequests_NoRace(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `[]`)
	})

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_, err := c.ListPullRequests(context.Background(), "o", "r", ListPullRequestsOptions{})
			if err != nil {
				t.Errorf("ListPullRequests: %v", err)
			}
		}()
	}
	wg.Wait()
	if got := calls.Load(); got != goroutines {
		t.Errorf("server calls = %d; want %d", got, goroutines)
	}
}
