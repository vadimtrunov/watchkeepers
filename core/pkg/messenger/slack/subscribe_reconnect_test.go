package slack

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// fakeSleeper records requested sleep durations and returns immediately
// (or honours ctx cancellation). Used by retry-budget tests so they
// don't spend wall-clock time on backoff sleeps. Mirrors the outbox
// fakeSleeper pattern.
type fakeSleeper struct {
	mu     sync.Mutex
	sleeps []time.Duration
}

func (f *fakeSleeper) Sleep(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	f.sleeps = append(f.sleeps, d)
	f.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func (f *fakeSleeper) recorded() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]time.Duration, len(f.sleeps))
	copy(out, f.sleeps)
	return out
}

// driverSequence allows the fake server to script a series of per-WS
// driver behaviours, one per accepted /ws upgrade. The first
// connection runs drivers[0], the second drivers[1], etc.
type driverSequence struct {
	mu      sync.Mutex
	drivers []func(ctx context.Context, t *testing.T, conn *websocket.Conn)
	idx     int
}

func (d *driverSequence) next() func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.idx >= len(d.drivers) {
		// Default: hello + block (test passed through all scripted
		// drivers without a fresh expectation).
		return helloOnly
	}
	drv := d.drivers[d.idx]
	d.idx++
	return drv
}

// runSequenceServer wires a fakeSocketServer whose driver dispatches
// to the next item in the sequence on each /ws upgrade.
func runSequenceServer(t *testing.T, drivers ...func(ctx context.Context, t *testing.T, conn *websocket.Conn)) *fakeSocketServer {
	t.Helper()
	seq := &driverSequence{drivers: drivers}
	return newFakeSocketServer(t, func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
		seq.next()(ctx, t, conn)
	})
}

// TestSubscribe_DisconnectEnvelope_ReconnectsTransparently asserts
// that when the server emits a disconnect envelope, the client opens
// a fresh connection (apps.connections.open + dial + hello) and
// resumes reading. The caller's handler observes events from BOTH
// connections without any indication of the reconnect.
func TestSubscribe_DisconnectEnvelope_ReconnectsTransparently(t *testing.T) {
	t.Parallel()

	delivered := make(chan messenger.IncomingMessage, 4)
	handler := &recordingHandler{
		next: func(msg messenger.IncomingMessage) { delivered <- msg },
	}

	var fs *fakeSocketServer
	fs = runSequenceServer(
		t,
		// Connection 1: hello, one event, disconnect envelope.
		func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			writeJSON(ctx, t, conn, makeEventEnvelope("evt-1", "C1", "U1", "first", "1700000000.000001"))
			fs.recordAck(readAck(ctx, t, conn))
			writeJSON(ctx, t, conn, map[string]any{"type": "disconnect", "reason": "warning"})
			_, _, _ = conn.Read(ctx)
		},
		// Connection 2: hello, one event, then block.
		func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			writeJSON(ctx, t, conn, makeEventEnvelope("evt-2", "C1", "U1", "second", "1700000000.000002"))
			fs.recordAck(readAck(ctx, t, conn))
			_, _, _ = conn.Read(ctx)
		},
	)

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
		// Tighten reconnect backoff so the test stays fast.
		WithSocketModeReconnectInitialDelay(1*time.Millisecond),
		WithSocketModeReconnectMaxDelay(10*time.Millisecond),
		// Long ping so it doesn't fire during the test window.
		WithSocketModePingInterval(30*time.Second),
	)
	sub, err := c.Subscribe(context.Background(), handler.handle)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	// Both events should arrive without observable reconnect.
	got := make([]string, 0, 2)
	for i := 0; i < 2; i++ {
		select {
		case msg := <-delivered:
			got = append(got, msg.Text)
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d/2 events dispatched (got %v)", i, got)
		}
	}
	if got[0] != "first" || got[1] != "second" {
		t.Errorf("messages = %v, want [first second]", got)
	}
	if n := fs.connsAccepted.Load(); n != 2 {
		t.Errorf("connsAccepted = %d, want 2 (one reconnect)", n)
	}
}

// TestSubscribe_TransportError_ReconnectsTransparently asserts that
// when the server abruptly drops the WS (CloseNow), the client
// reconnects and continues to receive events.
func TestSubscribe_TransportError_ReconnectsTransparently(t *testing.T) {
	t.Parallel()

	delivered := make(chan messenger.IncomingMessage, 4)
	handler := &recordingHandler{
		next: func(msg messenger.IncomingMessage) { delivered <- msg },
	}

	var fs *fakeSocketServer
	fs = runSequenceServer(
		t,
		// Connection 1: hello, one event, then abruptly drop.
		func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			writeJSON(ctx, t, conn, makeEventEnvelope("evt-1", "C1", "U1", "first", "1700000000.000001"))
			fs.recordAck(readAck(ctx, t, conn))
			// Force a hard close — the client side gets a transport error.
			_ = conn.CloseNow()
		},
		// Connection 2: hello, one event, block.
		func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			writeJSON(ctx, t, conn, makeEventEnvelope("evt-2", "C1", "U1", "second", "1700000000.000002"))
			fs.recordAck(readAck(ctx, t, conn))
			_, _, _ = conn.Read(ctx)
		},
	)

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
		WithSocketModeReconnectInitialDelay(1*time.Millisecond),
		WithSocketModeReconnectMaxDelay(10*time.Millisecond),
		WithSocketModePingInterval(30*time.Second),
	)
	sub, err := c.Subscribe(context.Background(), handler.handle)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	for i := 0; i < 2; i++ {
		select {
		case <-delivered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d/2 events dispatched", i)
		}
	}
	if n := fs.connsAccepted.Load(); n != 2 {
		t.Errorf("connsAccepted = %d, want 2 (one reconnect after drop)", n)
	}
}

// TestSubscribe_ReconnectBudgetExhausted_StopReturnsWrappedErr asserts
// that after N consecutive reconnect failures (server unreachable for
// the freshly-resolved URL), Stop returns ErrReconnectExhausted. The
// test waits for the read goroutine to exit naturally (via the
// internal `done` channel) before calling Stop so the pre-Stop
// budget-exhaustion path is exercised.
func TestSubscribe_ReconnectBudgetExhausted_StopReturnsWrappedErr(t *testing.T) {
	t.Parallel()

	// Driver sequence: first connection succeeds with hello + drop;
	// the OPEN call can keep returning success but the dial keeps
	// failing — we model that by having the dialer always fail after
	// the first conn.
	var dialCount atomic.Int32
	fs := runSequenceServer(
		t,
		func(ctx context.Context, _ *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			_ = conn.CloseNow()
		},
	)

	failingDialer := func(ctx context.Context, urlStr string) (*websocket.Conn, error) {
		n := dialCount.Add(1)
		if n == 1 {
			conn, resp, err := websocket.Dial(ctx, urlStr, nil)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			return conn, err
		}
		return nil, errors.New("dial: forced test failure")
	}

	sleeper := &fakeSleeper{}
	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
		WithSocketModeDialer(failingDialer),
		WithSocketModeMaxReconnectAttempts(3),
		WithSocketModeReconnectInitialDelay(1*time.Millisecond),
		WithSocketModeReconnectMaxDelay(2*time.Millisecond),
		WithSocketModePingInterval(30*time.Second),
		withSocketModeSleeperOption(sleeper),
	)

	sub, err := c.Subscribe(context.Background(), func(context.Context, messenger.IncomingMessage) error { return nil })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Wait for the session goroutine to exit on its own (budget
	// exhausted) before calling Stop. We poll the internal done
	// channel via a type assertion.
	ss, ok := sub.(*socketSubscription)
	if !ok {
		t.Fatalf("Subscribe returned %T, want *socketSubscription", sub)
	}
	select {
	case <-ss.done:
	case <-time.After(3 * time.Second):
		t.Fatal("session goroutine did not exit within 3s after budget exhaustion")
	}

	stopErr := sub.Stop()
	if !errors.Is(stopErr, ErrReconnectExhausted) {
		t.Errorf("Stop = %v, want errors.Is(., ErrReconnectExhausted)", stopErr)
	}
	if got := sleeper.recorded(); len(got) < 3 {
		t.Errorf("sleeper saw %d sleeps, want >= 3 (one per attempt)", len(got))
	}
}

// TestSubscribe_HelloTimeoutOnReconnect_RetriesWithBackoff asserts
// that a hello-timeout on a RECONNECT (after the initial subscription
// came up cleanly) does NOT terminate the subscription — the loop
// retries with backoff and eventually succeeds.
func TestSubscribe_HelloTimeoutOnReconnect_RetriesWithBackoff(t *testing.T) {
	t.Parallel()

	delivered := make(chan messenger.IncomingMessage, 4)
	handler := &recordingHandler{
		next: func(msg messenger.IncomingMessage) { delivered <- msg },
	}

	var fs *fakeSocketServer
	fs = runSequenceServer(
		t,
		// Conn 1: hello + one event + hard drop.
		func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			writeJSON(ctx, t, conn, makeEventEnvelope("evt-1", "C1", "U1", "first", "1.0"))
			fs.recordAck(readAck(ctx, t, conn))
			_ = conn.CloseNow()
		},
		// Conn 2: never sends hello — the client times out and
		// retries.
		func(ctx context.Context, _ *testing.T, conn *websocket.Conn) {
			_, _, _ = conn.Read(ctx)
		},
		// Conn 3: hello + one event + block.
		func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			writeJSON(ctx, t, conn, makeEventEnvelope("evt-3", "C1", "U1", "third", "1.1"))
			fs.recordAck(readAck(ctx, t, conn))
			_, _, _ = conn.Read(ctx)
		},
	)

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
		WithSocketModeReconnectInitialDelay(1*time.Millisecond),
		WithSocketModeReconnectMaxDelay(10*time.Millisecond),
		WithSocketModeMaxReconnectAttempts(5),
		// Tight hello timeout so the test runs fast.
		WithSocketModeHelloTimeout(50*time.Millisecond),
		WithSocketModePingInterval(30*time.Second),
	)
	sub, err := c.Subscribe(context.Background(), handler.handle)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	// Should receive both events (first + third).
	for i := 0; i < 2; i++ {
		select {
		case <-delivered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d/2 events dispatched", i)
		}
	}
	if n := fs.connsAccepted.Load(); n < 3 {
		t.Errorf("connsAccepted = %d, want >= 3 (initial + retry + success)", n)
	}
}

// TestSubscribe_PongTimeout_TriggersReconnect asserts that when no
// envelope arrives within pingInterval and the server fails to respond
// to the ping, the client reconnects.
func TestSubscribe_PongTimeout_TriggersReconnect(t *testing.T) {
	t.Parallel()

	delivered := make(chan messenger.IncomingMessage, 4)
	handler := &recordingHandler{
		next: func(msg messenger.IncomingMessage) { delivered <- msg },
	}

	var fs *fakeSocketServer
	fs = runSequenceServer(
		t,
		// Conn 1: hello, then silent — never reply to the ping.
		func(ctx context.Context, _ *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			// Drain whatever the client writes (pings) without responding.
			for {
				_, _, err := conn.Read(ctx)
				if err != nil {
					return
				}
			}
		},
		// Conn 2: hello + one event + block.
		func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			writeJSON(ctx, t, conn, makeEventEnvelope("evt-1", "C1", "U1", "after_pong_timeout", "1.0"))
			fs.recordAck(readAck(ctx, t, conn))
			_, _, _ = conn.Read(ctx)
		},
	)

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
		// Aggressive heartbeat so the test runs fast.
		WithSocketModePingInterval(50*time.Millisecond),
		WithSocketModePingTimeout(50*time.Millisecond),
		WithSocketModeReconnectInitialDelay(1*time.Millisecond),
		WithSocketModeReconnectMaxDelay(10*time.Millisecond),
	)
	sub, err := c.Subscribe(context.Background(), handler.handle)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	select {
	case msg := <-delivered:
		if msg.Text != "after_pong_timeout" {
			t.Errorf("msg.Text = %q, want after_pong_timeout", msg.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event after pong-timeout reconnect never arrived")
	}
}

// TestSubscribe_PongReceived_ResetsHeartbeat asserts that a pong
// reply within the deadline does NOT trigger a reconnect — the
// heartbeat resets and the next ping fires only after another
// pingInterval window.
func TestSubscribe_PongReceived_ResetsHeartbeat(t *testing.T) {
	t.Parallel()

	delivered := make(chan messenger.IncomingMessage, 4)
	handler := &recordingHandler{
		next: func(msg messenger.IncomingMessage) { delivered <- msg },
	}

	var fs *fakeSocketServer
	fs = runSequenceServer(
		t,
		// Single conn: hello, reply to pings with pongs, then send
		// one event after at least one ping cycle, then block.
		func(ctx context.Context, _ *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			pingsSeen := 0
			for {
				_, raw, err := conn.Read(ctx)
				if err != nil {
					return
				}
				// Try to parse as ping; reply with pong.
				if strings.Contains(string(raw), `"ping"`) {
					pingsSeen++
					_ = conn.Write(ctx, websocket.MessageText, []byte(`{"type":"pong","id":"p1"}`))
					if pingsSeen == 1 {
						// After first pong, send an event so the
						// caller sees the connection survived.
						writeJSON(ctx, t, conn, makeEventEnvelope("evt-after-pong", "C1", "U1", "alive", "1.0"))
						fs.recordAck(readAck(ctx, t, conn))
					}
				}
			}
		},
	)

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
		WithSocketModePingInterval(50*time.Millisecond),
		WithSocketModePingTimeout(200*time.Millisecond),
		WithSocketModeReconnectInitialDelay(1*time.Millisecond),
	)
	sub, err := c.Subscribe(context.Background(), handler.handle)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	select {
	case <-delivered:
	case <-time.After(5 * time.Second):
		t.Fatal("event after pong never arrived (heartbeat reset failed?)")
	}
	if n := fs.connsAccepted.Load(); n != 1 {
		t.Errorf("connsAccepted = %d, want 1 (no reconnect after pong)", n)
	}
}

// TestSubscribe_StopDuringReconnectBackoff_CleanShutdown asserts that
// a Stop() call while the read goroutine is parked in the backoff
// sleeper unblocks the sleep and exits cleanly.
func TestSubscribe_StopDuringReconnectBackoff_CleanShutdown(t *testing.T) {
	t.Parallel()

	// Server: drop after hello so the client enters reconnect
	// backoff. Use a long backoff so the test can confirm Stop wakes
	// the sleep.
	fs := runSequenceServer(
		t,
		func(ctx context.Context, _ *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			_ = conn.CloseNow()
		},
		// Block all subsequent connection attempts indefinitely.
		func(ctx context.Context, _ *testing.T, conn *websocket.Conn) {
			_, _, _ = conn.Read(ctx)
		},
	)

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
		WithSocketModeReconnectInitialDelay(5*time.Second),
		WithSocketModeReconnectMaxDelay(10*time.Second),
		WithSocketModeMaxReconnectAttempts(5),
		WithSocketModePingInterval(30*time.Second),
	)
	sub, err := c.Subscribe(context.Background(), func(context.Context, messenger.IncomingMessage) error { return nil })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Give the read goroutine a moment to enter the backoff sleep.
	time.Sleep(100 * time.Millisecond)

	stopDone := make(chan error, 1)
	go func() { stopDone <- sub.Stop() }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Errorf("Stop = %v, want nil (clean shutdown during backoff)", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within 2s during reconnect backoff")
	}
}

// TestSubscribe_StopDuringInflightHandler_BlocksUntilHandlerCompletes
// asserts the messenger.Subscription contract: Stop blocks until the
// in-flight handler returns. The handler is structured so the test
// can observe the ordering deterministically.
func TestSubscribe_StopDuringInflightHandler_BlocksUntilHandlerCompletes(t *testing.T) {
	t.Parallel()

	handlerStarted := make(chan struct{}, 1)
	releaseHandler := make(chan struct{})
	handler := &recordingHandler{
		next: func(messenger.IncomingMessage) {
			handlerStarted <- struct{}{}
			<-releaseHandler
		},
	}

	var fs *fakeSocketServer
	fs = runSequenceServer(
		t,
		func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			writeJSON(ctx, t, conn, makeEventEnvelope("evt-1", "C1", "U1", "x", "1.0"))
			fs.recordAck(readAck(ctx, t, conn))
			_, _, _ = conn.Read(ctx)
		},
	)

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
		WithSocketModePingInterval(30*time.Second),
	)
	sub, err := c.Subscribe(context.Background(), handler.handle)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	select {
	case <-handlerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never started")
	}

	stopReturned := make(chan error, 1)
	go func() { stopReturned <- sub.Stop() }()

	// Stop must NOT return while handler is in flight.
	select {
	case <-stopReturned:
		t.Fatal("Stop returned while handler was still in flight")
	case <-time.After(100 * time.Millisecond):
		// Expected: Stop blocked.
	}

	// Release handler; Stop should now return.
	close(releaseHandler)
	select {
	case <-stopReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after handler released")
	}
}

// TestSubscribe_NumGoroutine_ReturnsToBaseline opens N subscriptions
// back-to-back, each receives one event then Stop, and asserts the
// goroutine count returns to baseline. Catches goroutine leaks in the
// reconnect loop, the read pump, and the subscription state machine.
func TestSubscribe_NumGoroutine_ReturnsToBaseline(t *testing.T) {
	// Sequential — runtime.NumGoroutine is a process-global metric.
	// Run after a tiny stabilisation pause so any background goroutine
	// from previous tests has settled.
	time.Sleep(100 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const n = 10
	for i := 0; i < n; i++ {
		// Each iteration owns its own server so the close-handshake
		// goroutines from the previous iteration do not stack up.
		// httptest.Server's Close blocks until all in-flight handlers
		// return; our /ws handler exits when the conn is torn down by
		// the client's Stop. Without this per-iter teardown the leak
		// appears as O(n) goroutines accumulating for the whole test.
		runOneSubscriptionCycle(t, i)
	}

	// Settle. Background timers (websocket close-handshake deadlines)
	// can hang around for ~100-500ms after Stop returns.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		current := runtime.NumGoroutine()
		// ±2 slack covers the test runner's own jitter.
		if current <= baseline+2 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("NumGoroutine = %d, baseline = %d (delta > 2)", runtime.NumGoroutine(), baseline)
}

// runOneSubscriptionCycle drives one Subscribe → handler → Stop
// round-trip with the server closed before returning. Splitting this
// out scopes httpServer.Close() into the iteration so close-handshake
// goroutines do not accumulate across the loop.
func runOneSubscriptionCycle(t *testing.T, i int) {
	t.Helper()
	fs := newFakeSocketServer(t, func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
		writeJSON(ctx, t, conn, helloEnvelopeJSON)
		writeJSON(ctx, t, conn, makeEventEnvelope(fmt.Sprintf("evt-%d", i), "C1", "U1", "x", "1.0"))
		_, _, _ = conn.Read(ctx)
		_, _, _ = conn.Read(ctx)
	})
	defer fs.httpServer.Close()

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
		WithSocketModePingInterval(30*time.Second),
	)
	dispatched := make(chan struct{}, 1)
	handler := func(_ context.Context, _ messenger.IncomingMessage) error {
		select {
		case dispatched <- struct{}{}:
		default:
		}
		return nil
	}
	sub, err := c.Subscribe(context.Background(), handler)
	if err != nil {
		t.Fatalf("Subscribe %d: %v", i, err)
	}
	select {
	case <-dispatched:
	case <-time.After(3 * time.Second):
		t.Fatalf("subscription %d did not dispatch", i)
	}
	if err := sub.Stop(); err != nil {
		t.Errorf("Stop %d: %v", i, err)
	}
}

// TestSubscribe_AckBearingEnvelope_EmptyEnvelopeID_LogsWarning asserts
// that when an events_api / slash_commands / interactive envelope
// arrives WITHOUT envelope_id (Slack protocol violation), the client
// logs a warning and does NOT send an ack frame.
func TestSubscribe_AckBearingEnvelope_EmptyEnvelopeID_LogsWarning(t *testing.T) {
	t.Parallel()

	logger := &recordingLogger{}
	fs := runSequenceServer(
		t,
		func(ctx context.Context, _ *testing.T, conn *websocket.Conn) {
			writeJSON(ctx, t, conn, helloEnvelopeJSON)
			// events_api WITHOUT envelope_id: protocol violation.
			writeJSON(ctx, t, conn, map[string]any{
				"type":                     "events_api",
				"accepts_response_payload": false,
				// envelope_id intentionally omitted.
				"payload": map[string]any{
					"team_id": "T01",
					"event": map[string]any{
						"type":         "message",
						"channel":      "C1",
						"user":         "U1",
						"text":         "x",
						"ts":           "1.0",
						"channel_type": "channel",
					},
				},
			})
			_, _, _ = conn.Read(ctx)
		},
	)

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
		WithLogger(logger),
		WithSocketModePingInterval(30*time.Second),
	)
	delivered := make(chan struct{}, 1)
	handler := func(_ context.Context, _ messenger.IncomingMessage) error {
		delivered <- struct{}{}
		return nil
	}
	sub, err := c.Subscribe(context.Background(), handler)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	select {
	case <-delivered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler never received the empty-envelope_id event")
	}
	// No ack frame may have been written — the server-side recordAck
	// counter should be zero.
	if got := fs.snapshotAcks(); len(got) != 0 {
		t.Errorf("ack count = %d, want 0 (empty envelope_id must not be acked)", len(got))
	}

	// A warning entry must be present. We assert via the message
	// substring — the structured-logger contract guarantees the
	// message text is part of the documented surface.
	entries := logger.snapshot()
	var sawWarn bool
	for _, e := range entries {
		if strings.Contains(e.Msg, "ack-bearing envelope with empty envelope_id") {
			sawWarn = true
			break
		}
	}
	if !sawWarn {
		t.Errorf("no warning log entry for empty envelope_id; entries = %+v", entries)
	}
}

// TestSocketBackoffFor_ExponentialGrowthAndCap exercises the backoff
// helper directly: the un-jittered sequence doubles each step and
// caps at maxDelay. The realised value with jitter sits in the
// documented [base*0.75, base*1.25] band.
func TestSocketBackoffFor_ExponentialGrowthAndCap(t *testing.T) {
	t.Parallel()

	const base = 10 * time.Millisecond
	const maxD = 80 * time.Millisecond

	// nil rand: returns the un-jittered base.
	got := []time.Duration{
		socketBackoffFor(0, base, maxD, nil),
		socketBackoffFor(1, base, maxD, nil),
		socketBackoffFor(2, base, maxD, nil),
		socketBackoffFor(3, base, maxD, nil),
		socketBackoffFor(4, base, maxD, nil),
	}
	want := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond, // capped
		80 * time.Millisecond, // capped
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("socketBackoffFor(%d) = %v, want %v", i, got[i], want[i])
		}
	}

	// Jitter: rand=0.5 → midpoint (no jitter offset).
	mid := socketBackoffFor(0, base, maxD, func() float64 { return 0.5 })
	if mid != base {
		t.Errorf("rand=0.5 mid = %v, want %v", mid, base)
	}
	// Jitter: rand=0.0 → -25% offset.
	low := socketBackoffFor(0, base, maxD, func() float64 { return 0 })
	if low > base {
		t.Errorf("rand=0 low = %v, want <= %v", low, base)
	}
	// Jitter: rand close to 1 → +25% offset.
	high := socketBackoffFor(0, base, maxD, func() float64 { return 0.999 })
	if high < base {
		t.Errorf("rand=0.999 high = %v, want >= %v", high, base)
	}
}

// TestSocketBackoffFor_Bounds asserts the jitter span never produces
// a negative duration and respects the documented ±25% band.
func TestSocketBackoffFor_Bounds(t *testing.T) {
	t.Parallel()

	const base = 100 * time.Millisecond
	const maxD = 1 * time.Second

	for _, rnd := range []float64{0, 0.25, 0.5, 0.75, 0.999} {
		got := socketBackoffFor(0, base, maxD, func() float64 { return rnd })
		minD := time.Duration(float64(base) * (1 - reconnectJitterFraction))
		maxBand := time.Duration(float64(base) * (1 + reconnectJitterFraction))
		if got < minD || got > maxBand {
			t.Errorf("rand=%v: got %v, want in [%v, %v]", rnd, got, minD, maxBand)
		}
	}
}

// TestSubscribe_UnusedTestSeams_Compile exists solely so the unused
// linter does not complain about [withSocketModeSleeperOption] /
// [withSocketModeRandOption] when the budget-exhaustion tests above
// run on a host where they are otherwise disabled.
func TestSubscribe_UnusedTestSeams_Compile(_ *testing.T) {
	_ = withSocketModeSleeperOption(&fakeSleeper{})
	_ = withSocketModeRandOption(func() float64 { return 0.5 })
}
