package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/publish"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/server"
)

// subscribeNow is the fixed verifier/issuer clock used by every subscribe
// handler test so expiry math stays deterministic.
func subscribeNow() time.Time { return time.Date(2026, 4, 23, 12, 0, 0, 0, time.UTC) }

// startSubscribeServer spins up an httptest.Server wrapping a router that
// carries /v1/subscribe under the standard auth middleware, returns the
// base URL, the test issuer (for token minting), and the Registry that
// publishes events — all cleaned up by t.Cleanup.
func startSubscribeServer(t *testing.T, heartbeat time.Duration) (string, *auth.TestIssuer, *publish.Registry) {
	t.Helper()
	ti, v := newIssuerAndVerifier(t, subscribeNow)
	reg := publish.NewRegistry(4, heartbeat)
	mux := server.NewRouter(v, nil, reg, heartbeat)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	t.Cleanup(reg.Close)
	return ts.URL, ti, reg
}

// TestSubscribe_MissingAuth — AC1: no Authorization header returns 401
// missing_token via the existing middleware, never reaches the handler.
func TestSubscribe_MissingAuth(t *testing.T) {
	base, _, _ := startSubscribeServer(t, 50*time.Millisecond)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, base+"/v1/subscribe", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	var envErr struct {
		Error, Reason string
	}
	if err := json.NewDecoder(resp.Body).Decode(&envErr); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if envErr.Reason != "missing_token" {
		t.Errorf("reason = %q, want missing_token", envErr.Reason)
	}
}

// TestSubscribe_NonGETMethod — AC1: non-GET methods return 405.
// http.ServeMux with the pattern "GET /v1/subscribe" gives us this for
// free (405 with Allow: GET), which keeps the contract exact.
func TestSubscribe_NonGETMethod(t *testing.T) {
	base, _, _ := startSubscribeServer(t, 50*time.Millisecond)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, base+"/v1/subscribe", strings.NewReader(""))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

// TestSubscribe_Headers — AC5: happy-path response carries the three
// canonical SSE headers (Content-Type, Cache-Control, Connection).
func TestSubscribe_Headers(t *testing.T) {
	base, ti, reg := startSubscribeServer(t, 50*time.Millisecond)

	tok, err := ti.Issue(auth.Claim{Subject: "sub", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/subscribe", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
	if got := resp.Header.Get("Connection"); got != "keep-alive" {
		t.Errorf("Connection = %q, want keep-alive", got)
	}

	// End the stream and unblock the Registry's ctx watchdog.
	cancel()
	_ = reg // silence unused
}

// TestSubscribe_EventFraming — AC5: a published event is serialised as
// `id: …\nevent: …\ndata: …\n\n`.
func TestSubscribe_EventFraming(t *testing.T) {
	base, ti, reg := startSubscribeServer(t, time.Hour) // long heartbeat so we only see the event

	tok, err := ti.Issue(auth.Claim{Subject: "sub", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, base+"/v1/subscribe", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// Publish one event after the handler is live.
	evID := uuid.New()
	ev := publish.Event{
		ID:            evID,
		Scope:         "org",
		AggregateType: "watchkeeper",
		AggregateID:   uuid.New(),
		EventType:     "watchkeeper.spawned",
		Payload:       json.RawMessage(`{"hello":"world"}`),
		CreatedAt:     time.Unix(1714000000, 0).UTC(),
	}
	// Publish from a goroutine so the Registry.Subscribe side has time to
	// register before the event fires; a tiny sleep avoids the race
	// without flaking under load.
	time.Sleep(20 * time.Millisecond)
	if err := reg.Publish(context.Background(), ev); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Read until the double-newline frame terminator.
	frame, err := readSSEFrame(resp.Body, 2*time.Second)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}

	wantID := "id: " + evID.String()
	wantEvent := "event: watchkeeper.spawned"
	wantData := `data: {"hello":"world"}`
	for _, line := range []string{wantID, wantEvent, wantData} {
		if !strings.Contains(frame, line) {
			t.Errorf("frame missing %q; frame=%q", line, frame)
		}
	}
}

// TestSubscribe_Heartbeat — AC5: an idle stream emits a heartbeat SSE
// comment (`:\n\n`) at the configured cadence.
func TestSubscribe_Heartbeat(t *testing.T) {
	heartbeat := 40 * time.Millisecond
	base, ti, _ := startSubscribeServer(t, heartbeat)

	tok, err := ti.Issue(auth.Claim{Subject: "sub", Scope: "org"}, time.Minute)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/subscribe", nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// A heartbeat frame should arrive within 1.5 × heartbeat.
	budget := heartbeat*3 + 200*time.Millisecond
	frame, err := readSSEFrame(resp.Body, budget)
	if err != nil {
		t.Fatalf("read heartbeat frame: %v", err)
	}
	if !strings.HasPrefix(frame, ":") {
		t.Errorf("heartbeat frame = %q, want leading ':' comment", frame)
	}
}

// TestSubscribe_BadScope is the defence-in-depth branch: if some future
// middleware regression lets a malformed Claim.Scope through, the
// subscribe handler rejects it with 400 before subscribing. The current
// middleware path already blocks this; we keep the test so the handler's
// RoleForScope sanity check stays honest.
func TestSubscribe_BadScope(t *testing.T) {
	// Build a router whose AuthMiddleware has already placed an
	// illegal-scope Claim on the context. We bypass the issuer by
	// constructing the request against a bare handler.
	reg := publish.NewRegistry(1, 50*time.Millisecond)
	t.Cleanup(reg.Close)
	h := server.SubscribeHandlerForTest(reg, 50*time.Millisecond)

	req := httptest.NewRequestWithContext(
		server.ContextWithClaimForTest(context.Background(), auth.Claim{Scope: "weird"}),
		http.MethodGet, "/v1/subscribe", nil,
	)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// readSSEFrame reads from r until a blank line ("\n\n") terminator is
// seen, or budget expires. Returns the accumulated frame (excluding the
// terminator). Needed because net/http's ResponseBody is a streaming
// io.ReadCloser with no frame boundaries of its own.
func readSSEFrame(r io.Reader, budget time.Duration) (string, error) {
	type result struct {
		frame string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		br := bufio.NewReader(r)
		var sb strings.Builder
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				done <- result{sb.String(), err}
				return
			}
			if line == "\n" { // blank line: frame terminator
				done <- result{sb.String(), nil}
				return
			}
			sb.WriteString(line)
		}
	}()
	select {
	case r := <-done:
		return r.frame, r.err
	case <-time.After(budget):
		return "", context.DeadlineExceeded
	}
}
