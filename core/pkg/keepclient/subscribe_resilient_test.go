package keepclient

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeSleeper is the test double that records every Sleep call without
// actually blocking the test goroutine. Sleep returns ctx.Err() before
// recording when the ctx is already cancelled, which the
// "ContextCancelDuringBackoff" case relies on to interrupt the loop
// promptly.
type fakeSleeper struct {
	mu       sync.Mutex
	calls    []time.Duration
	hold     chan struct{} // optional gate; if non-nil, Sleep blocks on it
	released bool
}

// Sleep records d and either returns immediately or blocks on f.hold so
// the test can synchronise with the resilient loop. ctx cancellation
// always wins.
func (f *fakeSleeper) Sleep(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	f.calls = append(f.calls, d)
	hold := f.hold
	f.mu.Unlock()
	if hold == nil {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-hold:
		return nil
	}
}

// recorded returns a copy of every duration the loop asked the sleeper
// for, in call order.
func (f *fakeSleeper) recorded() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, len(f.calls))
	copy(out, f.calls)
	return out
}

// release unblocks the gate if hold is in effect; safe to call exactly
// once.
func (f *fakeSleeper) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hold != nil && !f.released {
		close(f.hold)
		f.released = true
	}
}

// fixedRand returns a no-jitter rand source: returning 0.5 means the
// (randFn()*2 - 1) shift produces zero, so backoff is exactly the base.
func fixedRand() float64 { return 0.5 }

// resilientTestClient wraps a *Client + a no-timeout http.Client tuned
// for streaming-style tests. The 10s default timeout would otherwise cap
// the entire response and make EOF/transport-drop tests racy.
func resilientTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	return NewClient(
		WithBaseURL(baseURL),
		WithTokenSource(StaticToken("t")),
		WithHTTPClient(&http.Client{}),
	)
}

// TestSubscribeResilient_PassThroughHappy — server emits 3 events with no
// drops; resilient Next returns 3 events; the fourth Next blocks (server
// closes the stream cleanly) and the resilient layer attempts to
// reconnect. We close the stream to terminate the loop and assert
// ErrStreamClosed.
func TestSubscribeResilient_PassThroughHappy(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSEHeaders(t, w)
		sseFlusher(t, w, "id: a\nevent: e\ndata: {\"k\":1}\n\n")
		sseFlusher(t, w, "id: b\nevent: e\ndata: {\"k\":2}\n\n")
		sseFlusher(t, w, "id: c\nevent: e\ndata: {\"k\":3}\n\n")
	}))
	t.Cleanup(srv.Close)

	c := resilientTestClient(t, srv.URL)
	// Use a fakeSleeper that *holds* so the post-EOF reconnect parks in
	// Sleep — we then close the stream to unblock and assert the closed
	// sentinel surfaces.
	sl := &fakeSleeper{hold: make(chan struct{})}
	stream, err := c.SubscribeResilient(context.Background(),
		withSleeperOption(sl), withRandOption(fixedRand))
	if err != nil {
		t.Fatalf("SubscribeResilient: %v", err)
	}

	wantIDs := []string{"a", "b", "c"}
	for i, want := range wantIDs {
		ev, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
		if ev.ID != want {
			t.Errorf("Event[%d].ID = %q, want %q", i, ev.ID, want)
		}
	}

	// Fourth Next: the inner stream EOFs and the resilient loop enters
	// backoff. Issue Close from another goroutine to abort the sleep.
	done := make(chan error, 1)
	go func() {
		_, err := stream.Next(context.Background())
		done <- err
	}()
	// Wait until the loop has actually called Sleep at least once before
	// closing — otherwise Close + Next can race and surface ErrStreamClosed
	// before reconnect fires (which is also valid but not what this case
	// is asserting).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sl.recorded()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := stream.Close(); err != nil {
		t.Logf("Close: %v", err)
	}
	sl.release()

	select {
	case err := <-done:
		if !errors.Is(err, ErrStreamClosed) {
			t.Errorf("post-Close Next: err = %v, want ErrStreamClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not return after Close")
	}
}

// TestSubscribeResilient_ReconnectOnTransportError — handler closes the
// underlying response body mid-stream after event #2; resilient Next
// reconnects (second connection) and yields event #3.
func TestSubscribeResilient_ReconnectOnTransportError(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		writeSSEHeaders(t, w)
		switch n {
		case 1:
			sseFlusher(t, w, "id: 1\nevent: e\ndata: {}\n\n")
			sseFlusher(t, w, "id: 2\nevent: e\ndata: {}\n\n")
			// Hijack the connection and close it abruptly so the
			// client's read surfaces a non-EOF transport error rather
			// than a clean EOF (the body close path).
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("ResponseWriter does not implement http.Hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			_ = conn.Close()
		case 2:
			sseFlusher(t, w, "id: 3\nevent: e\ndata: {}\n\n")
		}
	}))
	t.Cleanup(srv.Close)

	c := resilientTestClient(t, srv.URL)
	sl := &fakeSleeper{}
	stream, err := c.SubscribeResilient(context.Background(),
		withSleeperOption(sl), withRandOption(fixedRand))
	if err != nil {
		t.Fatalf("SubscribeResilient: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	wantIDs := []string{"1", "2", "3"}
	for i, want := range wantIDs {
		ev, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
		if ev.ID != want {
			t.Errorf("Event[%d].ID = %q, want %q", i, ev.ID, want)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server hit count = %d, want 2 (initial + reconnect)", got)
	}
	if len(sl.recorded()) == 0 {
		t.Errorf("expected at least one backoff sleep, got 0")
	}
}

// TestSubscribeResilient_ReconnectOnEOF — handler closes the response
// cleanly after event #1; resilient Next reconnects and receives event #2.
func TestSubscribeResilient_ReconnectOnEOF(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		writeSSEHeaders(t, w)
		switch n {
		case 1:
			sseFlusher(t, w, "id: 1\nevent: e\ndata: {}\n\n")
			// Handler returns -> body closes cleanly -> client sees EOF.
		case 2:
			sseFlusher(t, w, "id: 2\nevent: e\ndata: {}\n\n")
		}
	}))
	t.Cleanup(srv.Close)

	c := resilientTestClient(t, srv.URL)
	sl := &fakeSleeper{}
	stream, err := c.SubscribeResilient(context.Background(),
		withSleeperOption(sl), withRandOption(fixedRand))
	if err != nil {
		t.Fatalf("SubscribeResilient: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	for i, want := range []string{"1", "2"} {
		ev, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
		if ev.ID != want {
			t.Errorf("Event[%d].ID = %q, want %q", i, ev.ID, want)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server hit count = %d, want 2", got)
	}
}

// TestSubscribeResilient_LastEventIDForwarded — the second request
// carries `Last-Event-ID: <id-of-last-event-from-first-connection>`. The
// first request has no such header.
func TestSubscribeResilient_LastEventIDForwarded(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		gotIDs  []string
		callCnt int32
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCnt, 1)
		mu.Lock()
		gotIDs = append(gotIDs, r.Header.Get("Last-Event-ID"))
		mu.Unlock()
		writeSSEHeaders(t, w)
		switch n {
		case 1:
			sseFlusher(t, w, "id: first-id\nevent: e\ndata: {}\n\n")
		case 2:
			sseFlusher(t, w, "id: second-id\nevent: e\ndata: {}\n\n")
		}
	}))
	t.Cleanup(srv.Close)

	c := resilientTestClient(t, srv.URL)
	sl := &fakeSleeper{}
	stream, err := c.SubscribeResilient(context.Background(),
		withSleeperOption(sl), withRandOption(fixedRand))
	if err != nil {
		t.Fatalf("SubscribeResilient: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	for i, want := range []string{"first-id", "second-id"} {
		ev, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
		if ev.ID != want {
			t.Errorf("Event[%d].ID = %q, want %q", i, ev.ID, want)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(gotIDs) != 2 {
		t.Fatalf("recorded %d Last-Event-ID headers, want 2", len(gotIDs))
	}
	if gotIDs[0] != "" {
		t.Errorf("first request Last-Event-ID = %q, want empty", gotIDs[0])
	}
	if gotIDs[1] != "first-id" {
		t.Errorf("second request Last-Event-ID = %q, want %q", gotIDs[1], "first-id")
	}
}

// TestSubscribeResilient_DedupPredicate — caller predicate skips id "dup-2";
// server emits ["a","dup-2","c"]; client Next returns 2 events (a, c).
func TestSubscribeResilient_DedupPredicate(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSEHeaders(t, w)
		sseFlusher(t, w, "id: a\nevent: e\ndata: {}\n\n")
		sseFlusher(t, w, "id: dup-2\nevent: e\ndata: {}\n\n")
		sseFlusher(t, w, "id: c\nevent: e\ndata: {}\n\n")
	}))
	t.Cleanup(srv.Close)

	c := resilientTestClient(t, srv.URL)
	sl := &fakeSleeper{}
	predicate := func(id string) bool { return id == "dup-2" }
	stream, err := c.SubscribeResilient(context.Background(),
		withSleeperOption(sl),
		withRandOption(fixedRand),
		WithDedup(predicate),
	)
	if err != nil {
		t.Fatalf("SubscribeResilient: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	got := []string{}
	for i := 0; i < 2; i++ {
		ev, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
		got = append(got, ev.ID)
	}
	if got[0] != "a" || got[1] != "c" {
		t.Errorf("delivered ids = %v, want [a c]", got)
	}
}

// TestSubscribeResilient_DedupLRU — WithDedupLRU(8); server replays the
// same event id twice across a reconnect; client emits it once.
func TestSubscribeResilient_DedupLRU(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		writeSSEHeaders(t, w)
		switch n {
		case 1:
			sseFlusher(t, w, "id: same-id\nevent: e\ndata: {\"v\":1}\n\n")
			// Body closes -> EOF -> resilient reconnects.
		case 2:
			// Replay the same id (forward-compat with a server that
			// honoured Last-Event-ID and re-emitted on the boundary).
			sseFlusher(t, w, "id: same-id\nevent: e\ndata: {\"v\":1}\n\n")
			sseFlusher(t, w, "id: next-id\nevent: e\ndata: {\"v\":2}\n\n")
		}
	}))
	t.Cleanup(srv.Close)

	c := resilientTestClient(t, srv.URL)
	sl := &fakeSleeper{}
	stream, err := c.SubscribeResilient(context.Background(),
		withSleeperOption(sl),
		withRandOption(fixedRand),
		WithDedupLRU(8),
	)
	if err != nil {
		t.Fatalf("SubscribeResilient: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	got := []string{}
	for i := 0; i < 2; i++ {
		ev, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
		got = append(got, ev.ID)
	}
	if got[0] != "same-id" || got[1] != "next-id" {
		t.Errorf("delivered ids = %v, want [same-id next-id]", got)
	}
}

// TestSubscribeResilient_BackoffSequence — table-driven assertion that
// the per-attempt delay roughly follows min(maxDelay, initial * 2^n) with
// no jitter (we inject the deterministic 0.5 rand source).
func TestSubscribeResilient_BackoffSequence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		attempt  int
		want     time.Duration
		initial  time.Duration
		maxDelay time.Duration
	}{
		{0, 100 * time.Millisecond, 100 * time.Millisecond, 30 * time.Second},
		{1, 200 * time.Millisecond, 100 * time.Millisecond, 30 * time.Second},
		{2, 400 * time.Millisecond, 100 * time.Millisecond, 30 * time.Second},
		{3, 800 * time.Millisecond, 100 * time.Millisecond, 30 * time.Second},
		// Cap: at attempt 9 the un-capped value would be 100ms * 2^9 = ~51s.
		{9, 30 * time.Second, 100 * time.Millisecond, 30 * time.Second},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("attempt=%d", tc.attempt), func(t *testing.T) {
			got := backoffFor(tc.attempt, tc.initial, tc.maxDelay, fixedRand)
			if got != tc.want {
				t.Errorf("backoffFor(%d) = %v, want %v", tc.attempt, got, tc.want)
			}
		})
	}

	// Jitter sanity: values fall inside [base*0.75, base*1.25].
	const base = 200 * time.Millisecond
	lo := time.Duration(float64(base) * 0.75)
	hi := time.Duration(float64(base) * 1.25)
	for _, r := range []float64{0.0, 0.25, 0.5, 0.75, 0.9999} {
		got := backoffFor(1, 100*time.Millisecond, 30*time.Second, func() float64 { return r })
		if got < lo || got > hi {
			t.Errorf("backoffFor with rand=%v = %v, want in [%v,%v]", r, got, lo, hi)
		}
	}
}

// TestSubscribeResilient_NoTokenSource — sync ErrNoTokenSource, zero
// network hits.
func TestSubscribeResilient_NoTokenSource(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	stream, err := c.SubscribeResilient(context.Background())
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
	if stream != nil {
		t.Errorf("stream = %v, want nil", stream)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestSubscribeResilient_ServerErrorBubbles — server returns 401 on the
// streaming open; client SubscribeResilient surfaces *ServerError /
// ErrUnauthorized synchronously and does NOT enter the reconnect loop.
func TestSubscribeResilient_ServerErrorBubbles(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":"unauthorized"}`)
	}))
	t.Cleanup(srv.Close)

	c := resilientTestClient(t, srv.URL)
	sl := &fakeSleeper{}
	stream, err := c.SubscribeResilient(context.Background(),
		withSleeperOption(sl), withRandOption(fixedRand))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if stream != nil {
		t.Errorf("stream = %v, want nil on initial 401", stream)
	}
	var se *ServerError
	if !errors.As(err, &se) || se.Status != http.StatusUnauthorized {
		t.Errorf("err = %v, want *ServerError(401)", err)
	}
	if !errors.Is(err, ErrUnauthorized) {
		t.Errorf("errors.Is(err, ErrUnauthorized) = false; err = %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server calls = %d, want 1 (no retry on auth failure)", got)
	}
	if got := len(sl.recorded()); got != 0 {
		t.Errorf("backoff sleeps = %d, want 0", got)
	}
}

// TestSubscribeResilient_MaxAttemptsExhausted — server always closes
// connection abruptly (transport error); after WithMaxReconnectAttempts(2)
// the next Next returns ErrReconnectExhausted wrapping the last
// transport error.
func TestSubscribeResilient_MaxAttemptsExhausted(t *testing.T) {
	t.Parallel()

	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		// First call serves one event so SubscribeResilient succeeds and
		// the client enters the streaming loop. Reconnect attempts
		// thereafter all hijack and abort.
		if n == 1 {
			writeSSEHeaders(t, w)
			sseFlusher(t, w, "id: 1\nevent: e\ndata: {}\n\n")
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("not hijacker")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("Hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		// Reconnect attempts: hijack + drop *before* writing headers so
		// the keepclient sees a transport error rather than a 5xx server
		// status (which would bubble up as *ServerError, not retried).
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Errorf("not hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("Hijack: %v", err)
			return
		}
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)

	c := resilientTestClient(t, srv.URL)
	sl := &fakeSleeper{}
	stream, err := c.SubscribeResilient(context.Background(),
		withSleeperOption(sl),
		withRandOption(fixedRand),
		WithMaxReconnectAttempts(2),
		WithReconnectInitialDelay(time.Millisecond),
	)
	if err != nil {
		t.Fatalf("SubscribeResilient: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	// First Next succeeds.
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatalf("first Next: %v", err)
	}
	// Second Next: inner stream errors, loop exhausts 2 reconnect attempts.
	_, err = stream.Next(context.Background())
	if !errors.Is(err, ErrReconnectExhausted) {
		t.Errorf("err = %v, want errors.Is(ErrReconnectExhausted)", err)
	}
	if got := len(sl.recorded()); got != 2 {
		t.Errorf("backoff sleeps = %d, want 2", got)
	}
}

// TestSubscribeResilient_ContextCancelDuringBackoff — cancel ctx during a
// backoff sleep; client Next returns an error wrapping context.Canceled
// promptly (does not finish the sleep).
func TestSubscribeResilient_ContextCancelDuringBackoff(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSEHeaders(t, w)
		sseFlusher(t, w, "id: 1\nevent: e\ndata: {}\n\n")
		// Body closes cleanly -> EOF -> resilient enters backoff.
	}))
	t.Cleanup(srv.Close)

	c := resilientTestClient(t, srv.URL)
	// Use the realSleeper with a long initial delay so the test has time
	// to cancel the ctx mid-sleep. We assert wall-clock that the error
	// surfaces well before the sleep would have elapsed.
	stream, err := c.SubscribeResilient(context.Background(),
		WithReconnectInitialDelay(2*time.Second),
		withRandOption(fixedRand),
	)
	if err != nil {
		t.Fatalf("SubscribeResilient: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	// Drain the first event so the next Next enters the backoff path.
	if _, err := stream.Next(context.Background()); err != nil {
		t.Fatalf("first Next: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	_, err = stream.Next(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error after cancel, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}
	if elapsed > time.Second {
		t.Errorf("Next did not return promptly; took %v", elapsed)
	}
}

// TestSubscribeResilient_CloseIdempotent — calling Close twice does not
// panic; subsequent Next returns ErrStreamClosed (NOT io.EOF).
func TestSubscribeResilient_CloseIdempotent(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSEHeaders(t, w)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	c := resilientTestClient(t, srv.URL)
	stream, err := c.SubscribeResilient(context.Background())
	if err != nil {
		t.Fatalf("SubscribeResilient: %v", err)
	}

	if err := stream.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	_, err = stream.Next(context.Background())
	if !errors.Is(err, ErrStreamClosed) {
		t.Errorf("Next after Close: err = %v, want ErrStreamClosed", err)
	}
}

// TestSubscribeResilient_EmptyIDDoesNotClobberLastID — a frame without an
// `id:` line must not overwrite the previously-recorded last-event-ID.
// Connection #1 emits `id: evt-a` then an id-less frame, then closes (EOF).
// Connection #2 must receive `Last-Event-ID: evt-a`, not an empty string.
func TestSubscribeResilient_EmptyIDDoesNotClobberLastID(t *testing.T) {
	t.Parallel()

	var (
		callCnt     int32
		mu          sync.Mutex
		secondReqID string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&callCnt, 1)
		writeSSEHeaders(t, w)
		switch n {
		case 1:
			// Frame with id: — establishes last-event-ID = "evt-a".
			sseFlusher(t, w, "id: evt-a\nevent: x\ndata: {}\n\n")
			// Frame WITHOUT an id: line — must NOT clobber "evt-a".
			sseFlusher(t, w, "event: y\ndata: {}\n\n")
			// Clean EOF forces a reconnect.
		case 2:
			mu.Lock()
			secondReqID = r.Header.Get("Last-Event-ID")
			mu.Unlock()
			sseFlusher(t, w, "id: evt-c\nevent: z\ndata: {}\n\n")
		}
	}))
	t.Cleanup(srv.Close)

	c := resilientTestClient(t, srv.URL)
	sl := &fakeSleeper{}
	stream, err := c.SubscribeResilient(context.Background(),
		withSleeperOption(sl), withRandOption(fixedRand))
	if err != nil {
		t.Fatalf("SubscribeResilient: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	// Drain all three events (two from conn #1, one from conn #2).
	wantEvents := []struct{ id, eventType string }{
		{"evt-a", "x"},
		{"", "y"}, // id-less frame: ev.ID will be empty
		{"evt-c", "z"},
	}
	for i, want := range wantEvents {
		ev, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
		if ev.ID != want.id {
			t.Errorf("Event[%d].ID = %q, want %q", i, ev.ID, want.id)
		}
		if ev.EventType != want.eventType {
			t.Errorf("Event[%d].EventType = %q, want %q", i, ev.EventType, want.eventType)
		}
	}

	// The critical assertion: second request must carry the id from evt-a,
	// not an empty string (which the id-less frame would have caused before
	// the guard was added).
	mu.Lock()
	got := secondReqID
	mu.Unlock()
	if got != "evt-a" {
		t.Errorf("second request Last-Event-ID = %q, want %q", got, "evt-a")
	}
}

// TestSubscribeResilient_OptionDefaults — verify the implicit defaults
// resolve via the public option constructors. Pure unit-level guard
// against accidental constant drift.
func TestSubscribeResilient_OptionDefaults(t *testing.T) {
	t.Parallel()

	cfg := resilientConfig{
		initialDelay: defaultReconnectInitialDelay,
		maxDelay:     defaultReconnectMaxDelay,
		maxAttempts:  defaultMaxReconnectAttempts,
	}
	// Non-positive overrides are no-ops.
	WithReconnectInitialDelay(0)(&cfg)
	WithReconnectMaxDelay(-1)(&cfg)
	WithMaxReconnectAttempts(0)(&cfg)
	if cfg.initialDelay != defaultReconnectInitialDelay {
		t.Errorf("initialDelay = %v, want default %v", cfg.initialDelay, defaultReconnectInitialDelay)
	}
	if cfg.maxDelay != defaultReconnectMaxDelay {
		t.Errorf("maxDelay = %v, want default %v", cfg.maxDelay, defaultReconnectMaxDelay)
	}
	if cfg.maxAttempts != defaultMaxReconnectAttempts {
		t.Errorf("maxAttempts = %d, want default %d", cfg.maxAttempts, defaultMaxReconnectAttempts)
	}

	// Positive overrides apply.
	WithReconnectInitialDelay(7 * time.Millisecond)(&cfg)
	WithReconnectMaxDelay(99 * time.Second)(&cfg)
	WithMaxReconnectAttempts(11)(&cfg)
	if cfg.initialDelay != 7*time.Millisecond {
		t.Errorf("initialDelay override = %v", cfg.initialDelay)
	}
	if cfg.maxDelay != 99*time.Second {
		t.Errorf("maxDelay override = %v", cfg.maxDelay)
	}
	if cfg.maxAttempts != 11 {
		t.Errorf("maxAttempts override = %d", cfg.maxAttempts)
	}

	// WithDedup followed by WithDedupLRU and vice versa: last option wins.
	WithDedup(func(string) bool { return true })(&cfg)
	if cfg.dedup == nil {
		t.Fatal("WithDedup did not install a predicate")
	}
	WithDedupLRU(4)(&cfg)
	// Ensure subsequent WithDedup overrides LRU too.
	predicate := func(id string) bool { return strings.HasPrefix(id, "skip-") }
	WithDedup(predicate)(&cfg)
	if cfg.dedup == nil || !cfg.dedup("skip-x") || cfg.dedup("keep-x") {
		t.Errorf("last-WithDedup behaviour incorrect")
	}
	// Non-positive WithDedupLRU disables dedup.
	WithDedupLRU(0)(&cfg)
	if cfg.dedup != nil {
		t.Errorf("WithDedupLRU(0) did not clear dedup; got non-nil func")
	}
}
