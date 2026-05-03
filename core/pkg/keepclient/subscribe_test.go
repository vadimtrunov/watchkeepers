package keepclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// sseFlusher is the minimal helper test handlers use to write a frame and
// flush. Tests assert framing precision so we centralise the write+flush
// pattern here.
func sseFlusher(t *testing.T, w http.ResponseWriter, frame string) {
	t.Helper()
	if _, err := io.WriteString(w, frame); err != nil {
		t.Fatalf("write frame: %v", err)
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatalf("ResponseWriter does not implement http.Flusher")
	}
	flusher.Flush()
}

// writeSSEHeaders writes the canonical SSE response headers + 200 status the
// real server emits and flushes so the client sees the response status
// before any frames are written. Centralised so each test does not duplicate
// the four header lines plus the easy-to-forget Flush.
func writeSSEHeaders(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, ok := w.(http.Flusher)
	if !ok {
		t.Fatalf("ResponseWriter does not implement http.Flusher")
	}
	flusher.Flush()
}

// TestSubscribe_MultiEvent — server emits 3 framed events with distinct
// ids/types/payloads; client Next returns 3 events in order; final Next
// returns io.EOF.
func TestSubscribe_MultiEvent(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/subscribe" {
			t.Errorf("Path = %q, want /v1/subscribe", r.URL.Path)
		}
		writeSSEHeaders(t, w)
		sseFlusher(t, w, "id: 11111111-1111-1111-1111-111111111111\nevent: chunk_stored\ndata: {\"k\":1}\n\n")
		sseFlusher(t, w, "id: 22222222-2222-2222-2222-222222222222\nevent: manifest_version_committed\ndata: {\"k\":2}\n\n")
		sseFlusher(t, w, "id: 33333333-3333-3333-3333-333333333333\nevent: keepers_log_appended\ndata: {\"k\":3}\n\n")
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	stream, err := c.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	wantIDs := []string{
		"11111111-1111-1111-1111-111111111111",
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
	}
	wantTypes := []string{"chunk_stored", "manifest_version_committed", "keepers_log_appended"}
	wantPayloads := []string{`{"k":1}`, `{"k":2}`, `{"k":3}`}

	for i := range wantIDs {
		ev, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
		if ev.ID != wantIDs[i] {
			t.Errorf("Event[%d].ID = %q, want %q", i, ev.ID, wantIDs[i])
		}
		if ev.EventType != wantTypes[i] {
			t.Errorf("Event[%d].EventType = %q, want %q", i, ev.EventType, wantTypes[i])
		}
		if string(ev.Payload) != wantPayloads[i] {
			t.Errorf("Event[%d].Payload = %q, want %q", i, ev.Payload, wantPayloads[i])
		}
	}

	// Final Next: server closed body after the third event.
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("trailing Next: err = %v, want io.EOF", err)
	}
}

// TestSubscribe_PayloadIsRawMessage — server emits `data:` with a complex
// nested JSON; client preserves it byte-for-byte in Event.Payload.
func TestSubscribe_PayloadIsRawMessage(t *testing.T) {
	t.Parallel()

	const wantPayload = `{"a":1,"nested":{"b":[1,2,3],"c":null,"d":"x y"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSEHeaders(t, w)
		sseFlusher(t, w, "id: x\nevent: e\ndata: "+wantPayload+"\n\n")
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	stream, err := c.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	ev, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if string(ev.Payload) != wantPayload {
		t.Errorf("Payload = %q, want %q", ev.Payload, wantPayload)
	}
}

// TestSubscribe_HeartbeatsSkipped — server emits `:\n\n` heartbeats
// interleaved with two data events; Next returns only the two data events.
func TestSubscribe_HeartbeatsSkipped(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSEHeaders(t, w)
		sseFlusher(t, w, ":\n\n")
		sseFlusher(t, w, "id: a\nevent: e\ndata: {\"i\":1}\n\n")
		sseFlusher(t, w, ":\n\n")
		sseFlusher(t, w, ":\n\n")
		sseFlusher(t, w, "id: b\nevent: e\ndata: {\"i\":2}\n\n")
		sseFlusher(t, w, ":\n\n")
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	stream, err := c.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	for i, want := range []string{"a", "b"} {
		ev, err := stream.Next(context.Background())
		if err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
		if ev.ID != want {
			t.Errorf("Event[%d].ID = %q, want %q", i, ev.ID, want)
		}
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("trailing Next: err = %v, want io.EOF", err)
	}
}

// TestSubscribe_MultilineData — `data:` split across two `\n` lines (per SSE
// spec) joins with `\n` between segments.
func TestSubscribe_MultilineData(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSEHeaders(t, w)
		// Two data lines: "first" and "second" -> Payload "first\nsecond"
		sseFlusher(t, w, "id: m\nevent: e\ndata: first\ndata: second\n\n")
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	stream, err := c.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	ev, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if got, want := string(ev.Payload), "first\nsecond"; got != want {
		t.Errorf("Payload = %q, want %q", got, want)
	}
}

// TestSubscribe_EventWithoutData — `event:` frame with no `data:` line
// decodes with Payload == nil; caller can branch on that.
func TestSubscribe_EventWithoutData(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSEHeaders(t, w)
		sseFlusher(t, w, "id: nope\nevent: ping\n\n")
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	stream, err := c.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	ev, err := stream.Next(context.Background())
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if ev.ID != "nope" || ev.EventType != "ping" {
		t.Errorf("Event = %+v, want ID=nope EventType=ping", ev)
	}
	if ev.Payload != nil {
		t.Errorf("Payload = %q, want nil", ev.Payload)
	}
}

// TestSubscribe_AuthHeaderInjected — `Authorization: Bearer <token>` and
// `Accept: text/event-stream` present on the wire.
func TestSubscribe_AuthHeaderInjected(t *testing.T) {
	t.Parallel()

	var (
		gotAuth   string
		gotAccept string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		writeSSEHeaders(t, w)
		// Close immediately — the test only inspects request headers.
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("xyz")))
	stream, err := c.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	if gotAuth != "Bearer xyz" {
		t.Errorf("Authorization = %q, want \"Bearer xyz\"", gotAuth)
	}
	if !strings.Contains(gotAccept, "text/event-stream") {
		t.Errorf("Accept = %q, want it to contain text/event-stream", gotAccept)
	}
}

// TestSubscribe_NoTokenSource — Subscribe without WithTokenSource returns
// ErrNoTokenSource synchronously, zero network hits.
func TestSubscribe_NoTokenSource(t *testing.T) {
	t.Parallel()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL))
	stream, err := c.Subscribe(context.Background())
	if !errors.Is(err, ErrNoTokenSource) {
		t.Fatalf("err = %v, want ErrNoTokenSource", err)
	}
	if stream != nil {
		t.Errorf("stream = %v, want nil on error", stream)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("network hits = %d, want 0", got)
	}
}

// TestSubscribe_StatusMappings — table-driven {400,401,403,404,500} →
// matching sentinel via errors.Is; *ServerError.Status populated.
func TestSubscribe_StatusMappings(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		status  int
		wantErr error
	}{
		{"400", 400, ErrInvalidRequest},
		{"401", 401, ErrUnauthorized},
		{"403", 403, ErrForbidden},
		{"404", 404, ErrNotFound},
		{"500", 500, ErrInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprintf(w, `{"error":"err_%d"}`, tc.status)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
			stream, err := c.Subscribe(context.Background())
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if stream != nil {
				t.Errorf("stream = %v, want nil on error", stream)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("errors.Is(err, %v) = false; err = %v", tc.wantErr, err)
			}
			var se *ServerError
			if !errors.As(err, &se) || se.Status != tc.status {
				t.Errorf("ServerError.Status = %v, want %d (err=%v)", se, tc.status, err)
			}
		})
	}
}

// TestSubscribe_TransportError — server closed before request; error is
// not a *ServerError.
func TestSubscribe_TransportError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(WithBaseURL(url), WithTokenSource(StaticToken("t")))
	stream, err := c.Subscribe(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if stream != nil {
		t.Errorf("stream = %v, want nil on error", stream)
	}
	var se *ServerError
	if errors.As(err, &se) {
		t.Errorf("transport error must not be a *ServerError; got %v", err)
	}
}

// TestSubscribe_ContextCancellation — cancel mid-stream; the next Next
// returns an error wrapping context.Canceled; Close is safe afterwards.
func TestSubscribe_ContextCancellation(t *testing.T) {
	t.Parallel()

	// release lets Cleanup unblock the handler goroutine after the test
	// finishes, even if the request context cancellation races the
	// transport-level body close.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSEHeaders(t, w)
		// Send one event so the client's first Next returns success.
		sseFlusher(t, w, "id: 1\nevent: e\ndata: {}\n\n")
		// Then idle until either request ctx is cancelled or the
		// test releases us at cleanup.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	// Streaming endpoint -> no Client.Timeout (would cap the whole stream).
	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")), WithHTTPClient(&http.Client{}))
	stream, err := c.Subscribe(ctx)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	// First Next succeeds.
	if _, err := stream.Next(ctx); err != nil {
		t.Fatalf("first Next: %v", err)
	}

	// Cancel after the first event. The next Next must wrap context.Canceled.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err = stream.Next(ctx)
	if err == nil {
		t.Fatal("expected error after cancel, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}

	// Close is idempotent and safe to call here.
	if err := stream.Close(); err != nil {
		// A close after the transport already tore the body down may
		// surface a benign error; don't fail the test on it. We only
		// require that it does not panic.
		t.Logf("stream.Close after cancel: %v", err)
	}
}

// TestSubscribe_ServerEOF — server closes connection cleanly after 2
// events; the third Next returns io.EOF.
func TestSubscribe_ServerEOF(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSSEHeaders(t, w)
		sseFlusher(t, w, "id: 1\nevent: e\ndata: {}\n\n")
		sseFlusher(t, w, "id: 2\nevent: e\ndata: {}\n\n")
		// Handler returns -> body is closed cleanly.
	}))
	t.Cleanup(srv.Close)

	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")))
	stream, err := c.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = stream.Close() })

	for i := 0; i < 2; i++ {
		if _, err := stream.Next(context.Background()); err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
	}
	if _, err := stream.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Errorf("third Next: err = %v, want io.EOF", err)
	}
}

// TestSubscribe_CloseIdempotent — calling Close twice does not panic;
// calling Next after Close returns ErrStreamClosed (NOT io.EOF, to
// disambiguate from a clean server-side EOF).
func TestSubscribe_CloseIdempotent(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSEHeaders(t, w)
		// Block so the body is alive when the test calls Close.
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})

	// Streaming endpoints cannot share the default 10s Client.Timeout
	// (which caps the entire request lifetime including body reads).
	// Override with a no-timeout client for the streaming tests that
	// hold the connection open without flushing further frames.
	c := NewClient(WithBaseURL(srv.URL), WithTokenSource(StaticToken("t")), WithHTTPClient(&http.Client{}))
	stream, err := c.Subscribe(context.Background())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// First close.
	if err := stream.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	// Second close must not panic and must return the cached result.
	if err := stream.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}

	_, err = stream.Next(context.Background())
	if !errors.Is(err, ErrStreamClosed) {
		t.Errorf("Next after Close: err = %v, want ErrStreamClosed", err)
	}
	if errors.Is(err, io.EOF) {
		t.Errorf("Next after Close must NOT return io.EOF (cannot disambiguate from clean server EOF)")
	}
}
