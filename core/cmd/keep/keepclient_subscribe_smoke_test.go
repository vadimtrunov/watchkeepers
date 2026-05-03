//go:build integration

// Integration smoke tests for the keepclient resilient subscribe surface
// (M2.8.d.b). Reuse the seed/boot/teardown harness defined in
// read_integration_test.go and the subscribe-specific env layering from
// subscribe_integration_test.go (short heartbeat + small subscribe buffer
// + fast outbox poll), then drive the real Keep binary via the public
// keepclient SubscribeResilient API. Gated by the shared
// KEEP_INTEGRATION_DB_URL env var, so it participates in
// `make keep-integration-test` alongside the rest of the integration
// suite without new env requirements.
//
// Server-side caveat: the current Keep server (M2.7.e) does NOT honor the
// `Last-Event-ID` request header — it only streams events that arrive
// AFTER the new subscription registers. The smoke tests therefore
// publish their fixture rows AFTER the resilient subscribe is
// established (and, for the reconnect variant, after the reconnect has
// taken effect) so the worker fans them out into a live channel.
package main_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// subscribeSmokeBootEnv adds the outbox-poll + heartbeat + buffer
// overrides on top of the read boot env so the smoke harness sees
// published events within a tight wall-clock budget rather than waiting
// for the production-default 1s outbox poll.
func subscribeSmokeBootEnv(dsn, addr string) []string {
	return append(readBootEnv(dsn, addr),
		"KEEP_OUTBOX_POLL_INTERVAL=100ms",
		"KEEP_SUBSCRIBE_HEARTBEAT=100ms",
		"KEEP_SUBSCRIBE_BUFFER=8",
	)
}

// bootKeepSubscribeSmoke compiles + starts the Keep binary tuned for
// subscribe smoke testing. Mirrors bootKeepSubscribe (subscribe_integration_test.go)
// but layers the outbox-poll override on top so this file's tests do not
// depend on a 1s tick when proving the worker -> registry -> client
// pipeline.
func bootKeepSubscribeSmoke(t *testing.T, env *testEnv) (string, *exec.Cmd, func()) {
	t.Helper()
	bin := buildBinary(t)
	addr := pickLocalAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, bin)
	cmd.Env = subscribeSmokeBootEnv(env.dsn, addr)
	var stderr strings.Builder
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	cmd.Stdout = os.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start binary: %v", err)
	}
	waitForHealth(t, addr)

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	teardown := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(exitBudget):
			_ = cmd.Process.Kill()
		}
		cancel()
	}
	return addr, cmd, teardown
}

// publishOutboxEvent writes one row into watchkeeper.outbox under the
// supplied scope; the running outbox worker (KEEP_OUTBOX_POLL_INTERVAL
// override) picks it up within the next tick and fans it out via the
// publish.Registry to active subscribers. The returned event_type is the
// canonical wire shape an SSE consumer observes via Event.EventType.
func publishOutboxEvent(t *testing.T, env *testEnv, scope, eventType, payloadJSON string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := env.pool.Exec(ctx, `
		INSERT INTO watchkeeper.outbox (
		    aggregate_type, aggregate_id, event_type, payload, scope
		) VALUES ($1, $2, $3, $4::jsonb, $5)
	`, "watchkeeper", env.watchkeeperID, eventType, payloadJSON, scope); err != nil {
		t.Fatalf("publish outbox %q: %v", eventType, err)
	}
}

// closingTransport is a custom http.RoundTripper that wraps an inner
// transport and forcibly closes the response body of the FIRST response
// after the configured event count has been read. Used by
// TestKeepclientSubscribe_ReconnectSmoke to inject a transport-level
// drop without restarting the server.
type closingTransport struct {
	inner          http.RoundTripper
	dropped        atomic.Bool
	dropAfterBytes int64
}

// RoundTrip returns the response with its Body wrapped so that, on the
// first call, after reading more than dropAfterBytes the body returns an
// error (simulating a connection reset). Subsequent calls pass straight
// through (no-op).
func (t *closingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if t.dropped.Load() {
		return resp, nil
	}
	resp.Body = &countingBodyCloser{
		inner:          resp.Body,
		dropAfterBytes: t.dropAfterBytes,
		mark:           &t.dropped,
	}
	return resp, nil
}

// countingBodyCloser counts bytes read; once the threshold is crossed it
// closes the underlying body AND returns an error, so the keepclient
// scanner surfaces a transport-level read failure (not a clean EOF).
type countingBodyCloser struct {
	inner          io.ReadCloser
	dropAfterBytes int64
	read           int64
	mark           *atomic.Bool
	closed         bool
}

func (c *countingBodyCloser) Read(p []byte) (int, error) {
	if c.closed {
		return 0, io.ErrUnexpectedEOF
	}
	if c.read >= c.dropAfterBytes {
		c.closed = true
		_ = c.inner.Close()
		c.mark.Store(true)
		return 0, &net.OpError{Op: "read", Err: errors.New("simulated connection reset")}
	}
	n, err := c.inner.Read(p)
	c.read += int64(n)
	return n, err
}

func (c *countingBodyCloser) Close() error {
	c.closed = true
	return c.inner.Close()
}

// TestKeepclientSubscribe_Smoke — boots the real Keep binary, opens a
// resilient subscribe over the org scope, publishes 3 outbox events
// under the same scope, and asserts the client receives all 3 in order
// via SubscribeResilient.Next.
func TestKeepclientSubscribe_Smoke(t *testing.T) {
	env := newTestEnv(t)
	addr, _, teardown := bootKeepSubscribeSmoke(t, env)
	defer teardown()

	c := newKeepClient(t, addr, "org", issuerForTest(t))

	subCtx, subCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer subCancel()
	stream, err := c.SubscribeResilient(subCtx)
	if err != nil {
		t.Fatalf("SubscribeResilient: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// Give the server a brief moment to register the subscription before
	// publishing so the outbox worker tick fans events into a live channel.
	// The handler-side registration is synchronous on the request but the
	// publish.Registry write (the bit Publish enumerates) only happens
	// after the SSE response headers have been written and the handler
	// has called reg.Subscribe — Subscribe's open returns BEFORE that.
	// One heartbeat interval (100ms) is enough headroom in CI.
	time.Sleep(200 * time.Millisecond)

	want := []struct{ eventType, payload string }{
		{"smoke.first", `{"i":1}`},
		{"smoke.second", `{"i":2}`},
		{"smoke.third", `{"i":3}`},
	}
	for _, ev := range want {
		publishOutboxEvent(t, env, "org", ev.eventType, ev.payload)
	}

	for i, w := range want {
		readCtx, readCancel := context.WithTimeout(context.Background(), 5*time.Second)
		ev, err := stream.Next(readCtx)
		readCancel()
		if err != nil {
			t.Fatalf("Next(%d): %v", i, err)
		}
		if ev.EventType != w.eventType {
			t.Errorf("Event[%d].EventType = %q, want %q", i, ev.EventType, w.eventType)
		}
		// Compare payloads as decoded JSON so whitespace/key-order
		// differences from the publish round-trip don't flake.
		var got, exp any
		if err := json.Unmarshal(ev.Payload, &got); err != nil {
			t.Fatalf("decode got: %v (raw=%q)", err, ev.Payload)
		}
		if err := json.Unmarshal([]byte(w.payload), &exp); err != nil {
			t.Fatalf("decode want: %v", err)
		}
		gotBytes, _ := json.Marshal(got)
		expBytes, _ := json.Marshal(exp)
		if string(gotBytes) != string(expBytes) {
			t.Errorf("Event[%d].Payload = %s, want %s", i, gotBytes, expBytes)
		}
		if ev.ID == "" {
			t.Errorf("Event[%d].ID is empty; the server should always emit id:", i)
		}
	}
}

// TestKeepclientSubscribe_ReconnectSmoke — boots the binary, opens a
// resilient subscribe, observes one event, induces a transport drop via
// a custom RoundTripper that closes the response body mid-stream after
// the first event, then publishes a second event. The resilient layer
// must reconnect to the live binary and deliver the second event.
//
// Mechanism (b) from the TASK design notes: the closingTransport wraps
// http.DefaultTransport, so the server is never restarted; only the
// client-side TCP connection breaks.
func TestKeepclientSubscribe_ReconnectSmoke(t *testing.T) {
	env := newTestEnv(t)
	addr, _, teardown := bootKeepSubscribeSmoke(t, env)
	defer teardown()

	tr := &closingTransport{
		inner: http.DefaultTransport,
		// Drop after enough bytes to have read the first complete SSE
		// frame (id + event + data + blank line). Real frames are well
		// under 256 bytes for these tiny payloads, so 256 is past the
		// first frame but well before any second one would arrive.
		dropAfterBytes: 256,
	}
	httpClient := &http.Client{Transport: tr}

	tok := mintToken(t, issuerForTest(t), "org")
	c := keepclient.NewClient(
		keepclient.WithBaseURL("http://"+addr),
		keepclient.WithTokenSource(keepclient.StaticToken(tok)),
		keepclient.WithHTTPClient(httpClient),
	)

	subCtx, subCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer subCancel()
	stream, err := c.SubscribeResilient(subCtx,
		keepclient.WithReconnectInitialDelay(50*time.Millisecond),
		keepclient.WithMaxReconnectAttempts(5),
		keepclient.WithDedupLRU(16),
	)
	if err != nil {
		t.Fatalf("SubscribeResilient: %v", err)
	}
	defer func() { _ = stream.Close() }()

	// Brief settle so the subscriber registers before the first publish.
	time.Sleep(200 * time.Millisecond)

	publishOutboxEvent(t, env, "org", "reconnect.first", `{"step":1}`)

	readCtx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	ev1, err := stream.Next(readCtx1)
	cancel1()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if ev1.EventType != "reconnect.first" {
		t.Errorf("first Event.EventType = %q, want reconnect.first", ev1.EventType)
	}

	// Wait briefly so the closingTransport has time to register the read
	// past the threshold (the first frame plus the next inbound chunk),
	// then publish a second event. The transport will trip the drop on
	// the very next read, which forces the client into reconnect.
	time.Sleep(100 * time.Millisecond)
	publishOutboxEvent(t, env, "org", "reconnect.second", `{"step":2}`)

	readCtx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	ev2, err := stream.Next(readCtx2)
	cancel2()
	if err != nil {
		t.Fatalf("second Next (after reconnect): %v", err)
	}
	if ev2.EventType != "reconnect.second" {
		t.Errorf("second Event.EventType = %q, want reconnect.second", ev2.EventType)
	}
	if !tr.dropped.Load() {
		t.Errorf("closingTransport never tripped its drop; reconnect path not exercised")
	}
}
