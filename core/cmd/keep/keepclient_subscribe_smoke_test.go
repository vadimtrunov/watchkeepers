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
	"io"
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
// publish.Registry to active subscribers. Returns the inserted row id so
// the caller can register a t.Cleanup to delete it and prevent leftover
// unstamped rows from polluting subsequent tests.
func publishOutboxEvent(t *testing.T, env *testEnv, scope, eventType, payloadJSON string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id string
	if err := env.pool.QueryRow(ctx, `
		INSERT INTO watchkeeper.outbox (
		    aggregate_type, aggregate_id, event_type, payload, scope
		) VALUES ($1, $2, $3, $4::jsonb, $5)
		RETURNING id
	`, "watchkeeper", env.watchkeeperID, eventType, payloadJSON, scope).Scan(&id); err != nil {
		t.Fatalf("publish outbox %q: %v", eventType, err)
	}
	return id
}

// closingTransport is a custom http.RoundTripper that wraps an inner
// transport and forcibly closes the response body of the FIRST response
// after the configured number of complete SSE *data* frames have been
// read. Heartbeat frames (`:\n\n` comment lines) do NOT count — counting
// them would race with the heartbeat ticker (KEEP_SUBSCRIBE_HEARTBEAT)
// and cause the drop to fire before any real event is delivered, making
// the reconnect path non-deterministic. Used by
// TestKeepclientSubscribe_ReconnectSmoke to inject a transport-level
// drop without restarting the server.
type closingTransport struct {
	inner           http.RoundTripper
	dropped         atomic.Bool
	dropAfterFrames int
}

// RoundTrip returns the response with its Body wrapped so that, on the
// first call, after observing dropAfterFrames complete SSE data frames
// the body returns io.EOF on the next Read, forcing the keepclient to
// reconnect. Subsequent calls pass straight through (no-op).
func (t *closingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		return resp, err
	}
	if t.dropped.Load() {
		return resp, nil
	}
	resp.Body = &frameCountingBody{
		inner:           resp.Body,
		dropAfterFrames: t.dropAfterFrames,
		mark:            &t.dropped,
	}
	return resp, nil
}

// frameCountingBody counts complete SSE data frames (delimited by
// "\n\n") as data streams through Read. A frame is considered a data
// frame only if it contains at least one line beginning with "data:" —
// SSE heartbeat comments (`:\n\n`) are skipped so they cannot trip the
// drop before a real event is delivered. Once dropAfterFrames data
// frames have been observed Read returns io.EOF on the next call,
// simulating a clean connection drop that the keepclient resilient
// layer must reconnect from.
type frameCountingBody struct {
	inner           io.ReadCloser
	dropAfterFrames int
	frames          int
	frameBuf        []byte // bytes of the current SSE frame, reset on each "\n\n" boundary
	prevByteNL      bool   // last byte appended to frameBuf was '\n'
	mark            *atomic.Bool
	closed          bool
}

func (c *frameCountingBody) Read(p []byte) (int, error) {
	if c.closed {
		return 0, io.EOF
	}
	n, err := c.inner.Read(p)
	for i := 0; i < n; i++ {
		b := p[i]
		if b == '\n' && c.prevByteNL {
			// Frame boundary. Count only if this frame had a data: line.
			frame := c.frameBuf
			if frameHasDataLine(frame) {
				c.frames++
			}
			c.frameBuf = c.frameBuf[:0]
			c.prevByteNL = false
			continue
		}
		c.frameBuf = append(c.frameBuf, b)
		c.prevByteNL = (b == '\n')
	}
	if c.frames >= c.dropAfterFrames {
		c.closed = true
		_ = c.inner.Close()
		c.mark.Store(true)
		// Return the bytes already read in this call so the client sees the
		// complete frame, then EOF on the very next read.
		if n > 0 {
			return n, nil
		}
		return 0, io.EOF
	}
	return n, err
}

// frameHasDataLine reports whether the SSE frame buffer contains at
// least one line whose prefix is "data:". Lines are separated by '\n';
// the buffer may end with the trailing '\n' that preceded the frame
// terminator. A heartbeat frame's buffer is just ":\n", which has no
// data: line and is therefore ignored by the counter.
func frameHasDataLine(buf []byte) bool {
	const prefix = "data:"
	for i := 0; i < len(buf); {
		// Find end of current line.
		j := i
		for j < len(buf) && buf[j] != '\n' {
			j++
		}
		line := buf[i:j]
		if len(line) >= len(prefix) && string(line[:len(prefix)]) == prefix {
			return true
		}
		if j == len(buf) {
			break
		}
		i = j + 1
	}
	return false
}

func (c *frameCountingBody) Close() error {
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
		id := publishOutboxEvent(t, env, "org", ev.eventType, ev.payload)
		t.Cleanup(func() { deleteOutboxRow(t, env.pool, id) })
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
		// Drop after the first complete SSE frame has been delivered.
		// Counting frames (by "\n\n" boundaries) is deterministic across
		// CI and local environments because it doesn't depend on TCP
		// buffering or payload size: the body returns EOF on the read
		// immediately after the first "\n\n" terminator is seen.
		dropAfterFrames: 1,
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

	id1 := publishOutboxEvent(t, env, "org", "reconnect.first", `{"step":1}`)
	t.Cleanup(func() { deleteOutboxRow(t, env.pool, id1) })

	readCtx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	ev1, err := stream.Next(readCtx1)
	cancel1()
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	if ev1.EventType != "reconnect.first" {
		t.Errorf("first Event.EventType = %q, want reconnect.first", ev1.EventType)
	}

	// The frameCountingBody will have already marked the drop after
	// delivering the first complete frame; the reconnect happens
	// transparently inside SubscribeResilient before the next Next call.
	// Publish the second event now — it will be delivered on the
	// reconnected stream.
	id2 := publishOutboxEvent(t, env, "org", "reconnect.second", `{"step":2}`)
	t.Cleanup(func() { deleteOutboxRow(t, env.pool, id2) })

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
