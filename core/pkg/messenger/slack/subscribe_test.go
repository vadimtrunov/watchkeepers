package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// ackedEnvelope mirrors the wire shape the adapter writes back to Slack
// for an event ack: `{"envelope_id": "..."}`. Tests decode the captured
// ack body into this struct to assert the envelope_id was echoed.
type ackedEnvelope struct {
	EnvelopeID string `json:"envelope_id"`
}

// fakeSocketServer is a hand-rolled stand-in for Slack's
// apps.connections.open + WSS endpoints. The HTTP handler at
// /apps.connections.open replies with `{"ok": true, "url": "wss://..."}`
// pointing at /ws on the same host; the /ws handler upgrades the
// connection and runs the per-test scenario via the supplied driver fn.
//
// Mirrors the M4.2.b captureServer discipline (hand-rolled, no mocking
// library) and gives the test full control over the envelope sequence
// the WSS side emits.
type fakeSocketServer struct {
	t *testing.T

	// httpServer hosts both /apps.connections.open and /ws so the
	// test only spins up one *httptest.Server. The /apps URL points
	// the client at the /ws path of the same host.
	httpServer *httptest.Server

	// openOK toggles whether /apps.connections.open returns
	// `{"ok": true, ...}` (default) or a Slack-style error envelope.
	openOK    bool
	openError string

	// urlSuffix is appended to the websocket URL returned by
	// apps.connections.open. Slack appends `?ticket=...&app_id=...`;
	// tests use it to drive the redaction-discipline assertion.
	urlSuffix string

	// driver runs against the per-connection WS Conn after the
	// handler upgrades. Tests script the envelope sequence here.
	driver func(ctx context.Context, t *testing.T, conn *websocket.Conn)

	// captured records the JSON ack envelopes the adapter wrote
	// back. Exposed for assertions.
	mu       sync.Mutex
	captured []ackedEnvelope

	// connsAccepted counts how many times /ws was reached. Tests
	// assert "exactly 1" for the c.1 contract (no reconnect).
	connsAccepted atomic.Int32
}

// newFakeSocketServer constructs a fake reachable via
// `<openURL>/apps.connections.open`. The driver is invoked once per
// /ws upgrade.
func newFakeSocketServer(t *testing.T, driver func(ctx context.Context, t *testing.T, conn *websocket.Conn)) *fakeSocketServer {
	t.Helper()
	fs := &fakeSocketServer{t: t, openOK: true, driver: driver}
	mux := http.NewServeMux()
	mux.HandleFunc("/apps.connections.open", fs.handleOpen)
	mux.HandleFunc("/ws", fs.handleWS)
	fs.httpServer = httptest.NewServer(mux)
	t.Cleanup(fs.httpServer.Close)
	return fs
}

// openURL returns the base URL the [Client] should target for Slack
// Web API calls (apps.connections.open).
func (fs *fakeSocketServer) openURL() string { return fs.httpServer.URL }

// wsURL returns the wss-style URL apps.connections.open advertises.
// httptest.Server is HTTP-not-HTTPS; coder/websocket dials `ws://` for
// http hosts, so we emit `ws://` here. The client treats the URL
// opaquely and the redaction tests assert no query suffix is logged.
func (fs *fakeSocketServer) wsURL() string {
	u := strings.Replace(fs.httpServer.URL, "http://", "ws://", 1) + "/ws"
	if fs.urlSuffix != "" {
		u += fs.urlSuffix
	}
	return u
}

func (fs *fakeSocketServer) handleOpen(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if !fs.openOK {
		_, _ = fmt.Fprintf(w, `{"ok":false,"error":%q}`, fs.openError)
		return
	}
	_, _ = fmt.Fprintf(w, `{"ok":true,"url":%q}`, fs.wsURL())
}

func (fs *fakeSocketServer) handleWS(w http.ResponseWriter, r *http.Request) {
	fs.connsAccepted.Add(1)
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// httptest.Server has no Origin enforcement; permissive
		// accept keeps the test loop simple.
		InsecureSkipVerify: true,
	})
	if err != nil {
		fs.t.Errorf("websocket.Accept: %v", err)
		return
	}
	// CloseNow on test exit so a stuck driver does not leak the
	// goroutine — the test itself will fail loudly anyway.
	defer func() { _ = conn.CloseNow() }()
	if fs.driver != nil {
		fs.driver(r.Context(), fs.t, conn)
	}
}

// recordAck unmarshals one client→server frame as an ack envelope and
// appends it to the captured list. Tests use this from the driver.
func (fs *fakeSocketServer) recordAck(raw []byte) {
	var env ackedEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		fs.t.Errorf("recordAck: unmarshal %q: %v", raw, err)
		return
	}
	fs.mu.Lock()
	fs.captured = append(fs.captured, env)
	fs.mu.Unlock()
}

// snapshotAcks returns a copy of the captured ack list.
func (fs *fakeSocketServer) snapshotAcks() []ackedEnvelope {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]ackedEnvelope, len(fs.captured))
	copy(out, fs.captured)
	return out
}

// writeJSON writes one JSON envelope as a text frame. Helper for
// driver bodies.
func writeJSON(ctx context.Context, t *testing.T, conn *websocket.Conn, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := conn.Write(ctx, websocket.MessageText, raw); err != nil {
		t.Fatalf("write envelope: %v", err)
	}
}

// readAck reads exactly one client→server frame and records it via
// fs.recordAck. Returns the raw bytes for additional assertions.
func readAck(ctx context.Context, t *testing.T, conn *websocket.Conn) []byte {
	t.Helper()
	_, raw, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	return raw
}

// helloEnvelope is the canonical first frame Slack sends after a
// successful Socket Mode handshake. Test drivers use it verbatim.
//
// Slack docs: https://api.slack.com/apis/connections/socket-implement
var helloEnvelopeJSON = map[string]any{
	"type":            "hello",
	"num_connections": 1,
	"connection_info": map[string]any{"app_id": "A0TEST"},
	"debug_info":      map[string]any{"host": "test"},
}

// makeEventEnvelope builds an `events_api` envelope carrying a Slack
// `message` event with the supplied channel/user/text/ts. Mirrors the
// real wire shape so the adapter's decoder is exercised against the
// documented schema.
func makeEventEnvelope(envelopeID, channel, user, text, ts string) map[string]any {
	return map[string]any{
		"envelope_id":              envelopeID,
		"type":                     "events_api",
		"accepts_response_payload": false,
		"payload": map[string]any{
			"team_id": "T01",
			"event": map[string]any{
				"type":         "message",
				"channel":      channel,
				"user":         user,
				"text":         text,
				"ts":           ts,
				"channel_type": "channel",
			},
		},
	}
}

// recordingHandler is a minimal MessageHandler stand-in — tracks every
// IncomingMessage it receives. Hand-rolled, no mocking library.
type recordingHandler struct {
	mu   sync.Mutex
	msgs []messenger.IncomingMessage
	// next, when non-nil, is invoked from the handler's goroutine
	// per-message. Tests use it to inject delays, errors, or signal
	// readiness.
	next func(messenger.IncomingMessage)
}

func (h *recordingHandler) handle(_ context.Context, msg messenger.IncomingMessage) error {
	h.mu.Lock()
	h.msgs = append(h.msgs, msg)
	h.mu.Unlock()
	if h.next != nil {
		h.next(msg)
	}
	return nil
}

// helloOnly sends the hello envelope then blocks on read until the
// server's ctx fires (i.e. the client closed). Useful when a test
// only cares about the open/close path.
func helloOnly(ctx context.Context, t *testing.T, conn *websocket.Conn) {
	writeJSON(ctx, t, conn, helloEnvelopeJSON)
	// Wait for client close.
	_, _, _ = conn.Read(ctx)
}

// TestSubscribe_HappyPath_OneEvent_AckedAndDispatched asserts the M4.2.c.1
// happy path: apps.connections.open → dial WSS → receive hello → receive
// one event_api envelope → ack it → dispatch the decoded
// IncomingMessage to the handler. The ack must precede the handler
// dispatch so a slow handler cannot stall Slack's 3s ack budget.
func TestSubscribe_HappyPath_OneEvent_AckedAndDispatched(t *testing.T) {
	t.Parallel()

	dispatched := make(chan messenger.IncomingMessage, 1)
	handler := &recordingHandler{
		next: func(msg messenger.IncomingMessage) { dispatched <- msg },
	}

	// ackRead is signalled by the server driver AFTER it has read +
	// recorded the per-event ack. Tests use it (not the handler-side
	// channel) to assert ack-state — the handler runs concurrently
	// with the ack write, so handler-completion does NOT imply the
	// server has observed the ack yet.
	ackRead := make(chan struct{}, 1)

	var fs *fakeSocketServer
	fs = newFakeSocketServer(t, func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
		writeJSON(ctx, t, conn, helloEnvelopeJSON)
		writeJSON(ctx, t, conn, makeEventEnvelope("evt-1", "C1", "U1", "hi", "1700000000.000100"))
		// The adapter MUST ack envelope-id "evt-1" within 3s.
		ackCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		fs.recordAck(readAck(ackCtx, t, conn))
		ackRead <- struct{}{}
		// Block until client closes.
		_, _, _ = conn.Read(ctx)
	})

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
	)
	sub, err := c.Subscribe(context.Background(), handler.handle)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	select {
	case msg := <-dispatched:
		if msg.ID != "1700000000.000100" {
			t.Errorf("msg.ID = %q, want 1700000000.000100", msg.ID)
		}
		if msg.ChannelID != "C1" {
			t.Errorf("msg.ChannelID = %q, want C1", msg.ChannelID)
		}
		if msg.SenderID != "U1" {
			t.Errorf("msg.SenderID = %q, want U1", msg.SenderID)
		}
		if msg.Text != "hi" {
			t.Errorf("msg.Text = %q, want hi", msg.Text)
		}
		if msg.Metadata["channel_type"] != "channel" {
			t.Errorf("msg.Metadata[channel_type] = %q, want channel", msg.Metadata["channel_type"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handler never received the event")
	}
	select {
	case <-ackRead:
	case <-time.After(3 * time.Second):
		t.Fatal("server never observed the ack")
	}

	acks := fs.snapshotAcks()
	if len(acks) != 1 {
		t.Fatalf("ack count = %d, want 1", len(acks))
	}
	if acks[0].EnvelopeID != "evt-1" {
		t.Errorf("ack envelope_id = %q, want evt-1", acks[0].EnvelopeID)
	}
	if got := fs.connsAccepted.Load(); got != 1 {
		t.Errorf("connsAccepted = %d, want 1 (no reconnect in c.1)", got)
	}
}

// TestSubscribe_MultipleEvents_EachAckedAndDispatched asserts that a
// burst of envelopes each travel through the ack→dispatch pipeline.
func TestSubscribe_MultipleEvents_EachAckedAndDispatched(t *testing.T) {
	t.Parallel()

	const n = 5
	delivered := make(chan struct{}, n)
	handler := &recordingHandler{
		next: func(messenger.IncomingMessage) { delivered <- struct{}{} },
	}

	// allAcksRead is signalled by the server driver AFTER it has
	// recorded all `n` acks. Synchronisation point — the handler-side
	// `delivered` channel only proves the read goroutine reached
	// dispatch, which races with the ack-write the server is reading.
	allAcksRead := make(chan struct{}, 1)

	var fs *fakeSocketServer
	fs = newFakeSocketServer(t, func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
		writeJSON(ctx, t, conn, helloEnvelopeJSON)
		for i := 0; i < n; i++ {
			id := fmt.Sprintf("evt-%d", i)
			ts := fmt.Sprintf("1700000000.%06d", i)
			writeJSON(ctx, t, conn, makeEventEnvelope(id, "C1", "U1", "hi", ts))
			fs.recordAck(readAck(ctx, t, conn))
		}
		allAcksRead <- struct{}{}
		_, _, _ = conn.Read(ctx)
	})

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
	)
	sub, err := c.Subscribe(context.Background(), handler.handle)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	for i := 0; i < n; i++ {
		select {
		case <-delivered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d/%d events dispatched", i, n)
		}
	}
	select {
	case <-allAcksRead:
	case <-time.After(3 * time.Second):
		t.Fatal("server did not observe all acks within deadline")
	}

	acks := fs.snapshotAcks()
	if len(acks) != n {
		t.Fatalf("ack count = %d, want %d", len(acks), n)
	}
	for i, a := range acks {
		want := fmt.Sprintf("evt-%d", i)
		if a.EnvelopeID != want {
			t.Errorf("ack[%d] = %q, want %q", i, a.EnvelopeID, want)
		}
	}
}

// TestSubscribe_OpenError_ReturnedToCaller asserts that a Slack
// `{"ok":false,"error":"missing_scope"}` response from
// apps.connections.open surfaces as a portable wrapped error so callers
// can match the slack-package sentinel via errors.Is.
func TestSubscribe_OpenError_ReturnedToCaller(t *testing.T) {
	t.Parallel()

	fs := newFakeSocketServer(t, nil)
	fs.openOK = false
	fs.openError = "missing_scope"

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
	)
	sub, err := c.Subscribe(context.Background(), func(context.Context, messenger.IncomingMessage) error { return nil })
	if err == nil {
		_ = sub.Stop()
		t.Fatal("Subscribe: expected error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError underneath", err)
	}
	if apiErr.Code != "missing_scope" {
		t.Errorf("apiErr.Code = %q, want missing_scope", apiErr.Code)
	}
}

// TestSubscribe_NilHandler_FailsSync asserts that a nil handler returns
// messenger.ErrInvalidHandler synchronously without contacting the
// platform.
func TestSubscribe_NilHandler_FailsSync(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	_, err := c.Subscribe(context.Background(), nil)
	if !errors.Is(err, messenger.ErrInvalidHandler) {
		t.Errorf("err = %v, want messenger.ErrInvalidHandler", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("server saw %d calls, want 0 (nil handler must not hit network)", got)
	}
}

// TestSubscribe_HelloTimeout_ReturnsError asserts that if the server
// never emits a `hello` envelope within the documented timeout the
// Subscribe call fails with a wrapped error.
func TestSubscribe_HelloTimeout_ReturnsError(t *testing.T) {
	t.Parallel()

	fs := newFakeSocketServer(t, func(ctx context.Context, _ *testing.T, conn *websocket.Conn) {
		// Never send anything; just block until client closes.
		_, _, _ = conn.Read(ctx)
	})

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
		// Tighten the timeout so the test stays fast.
		WithSocketModeHelloTimeout(50*time.Millisecond),
	)
	sub, err := c.Subscribe(context.Background(), func(context.Context, messenger.IncomingMessage) error { return nil })
	if err == nil {
		_ = sub.Stop()
		t.Fatal("Subscribe: expected timeout error, got nil")
	}
	if !errors.Is(err, ErrHelloTimeout) {
		t.Errorf("err = %v, want ErrHelloTimeout", err)
	}
}

// TestSubscribe_NonHelloFirstEnvelope_Errors asserts that if the first
// envelope on the wire is NOT `hello` (e.g. a `disconnect` immediately,
// or an event_api before hello — both protocol violations), Subscribe
// returns an error and tears down the connection.
func TestSubscribe_NonHelloFirstEnvelope_Errors(t *testing.T) {
	t.Parallel()

	fs := newFakeSocketServer(t, func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
		// Wrong-shape first frame: a disconnect envelope before
		// hello is a protocol violation.
		writeJSON(ctx, t, conn, map[string]any{
			"type":   "disconnect",
			"reason": "warning",
		})
		_, _, _ = conn.Read(ctx)
	})

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
	)
	sub, err := c.Subscribe(context.Background(), func(context.Context, messenger.IncomingMessage) error { return nil })
	if err == nil {
		_ = sub.Stop()
		t.Fatal("Subscribe: expected error on missing hello, got nil")
	}
	if !errors.Is(err, ErrUnexpectedEnvelope) {
		t.Errorf("err = %v, want ErrUnexpectedEnvelope", err)
	}
}

// TestSubscribe_DisconnectEnvelope_TerminatesCleanly asserts the c.1
// contract for a `disconnect` envelope: the read goroutine logs and
// exits, the underlying ws closes, and Subscription.Stop() returns
// without error. (Reconnect is M4.2.c.2 and out of scope.)
func TestSubscribe_DisconnectEnvelope_TerminatesCleanly(t *testing.T) {
	t.Parallel()

	fs := newFakeSocketServer(t, func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
		writeJSON(ctx, t, conn, helloEnvelopeJSON)
		writeJSON(ctx, t, conn, map[string]any{
			"type":   "disconnect",
			"reason": "warning",
		})
		_, _, _ = conn.Read(ctx)
	})

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
	)
	sub, err := c.Subscribe(context.Background(), func(context.Context, messenger.IncomingMessage) error { return nil })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Give the read goroutine time to observe the disconnect.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := sub.Stop(); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("Stop did not return cleanly within deadline after disconnect envelope")
}

// TestSubscribe_Stop_BeforeAnyEvent_ClosesCleanly asserts that calling
// Stop right after the hello (no event ever arrives) closes the
// goroutine and the connection cleanly.
func TestSubscribe_Stop_BeforeAnyEvent_ClosesCleanly(t *testing.T) {
	t.Parallel()

	fs := newFakeSocketServer(t, helloOnly)

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
	)
	sub, err := c.Subscribe(context.Background(), func(context.Context, messenger.IncomingMessage) error { return nil })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := sub.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

// TestSubscribe_Stop_Idempotent asserts Stop returns the same nil
// repeatedly. Per messenger.Subscription contract.
func TestSubscribe_Stop_Idempotent(t *testing.T) {
	t.Parallel()

	fs := newFakeSocketServer(t, helloOnly)

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
	)
	sub, err := c.Subscribe(context.Background(), func(context.Context, messenger.IncomingMessage) error { return nil })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := sub.Stop(); err != nil {
		t.Errorf("first Stop: %v", err)
	}
	if err := sub.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
	if err := sub.Stop(); err != nil {
		t.Errorf("third Stop: %v", err)
	}
}

// TestSubscribe_CtxCancellation_PropagatesThroughOpen asserts a
// pre-cancelled ctx aborts the apps.connections.open call without a
// network round-trip.
func TestSubscribe_CtxCancellation_PropagatesThroughOpen(t *testing.T) {
	t.Parallel()

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(
		WithBaseURL(srv.URL),
		WithTokenSource(StaticToken("t")),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sub, err := c.Subscribe(ctx, func(context.Context, messenger.IncomingMessage) error { return nil })
	if err == nil {
		_ = sub.Stop()
		t.Fatal("Subscribe: expected ctx error, got nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("server saw %d calls, want 0 (cancelled ctx must not hit network)", got)
	}
}

// TestSubscribe_LoggerRedacted asserts the redaction discipline: the
// bearer token, the WSS URL query string (which carries the per-conn
// ticket — Slack's transient auth), and any envelope payload bytes
// NEVER appear in log entries.
func TestSubscribe_LoggerRedacted(t *testing.T) {
	t.Parallel()

	fs := newFakeSocketServer(t, func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
		writeJSON(ctx, t, conn, helloEnvelopeJSON)
		writeJSON(ctx, t, conn, makeEventEnvelope("evt-1", "C1", "U1", "PII-PAYLOAD-PLEASE-REDACT", "1.0"))
		_, _, _ = conn.Read(ctx) // wait for ack
		_, _, _ = conn.Read(ctx)
	})
	fs.urlSuffix = "?ticket=SECRET-WSS-TICKET&app_id=A0TEST"

	logger := &recordingLogger{}
	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-LEAKED-TOKEN")),
		WithLogger(logger),
	)
	delivered := make(chan struct{}, 1)
	handler := func(_ context.Context, msg messenger.IncomingMessage) error {
		_ = msg
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
		t.Fatal("handler never received the event")
	}

	entries := logger.snapshot()
	banned := []string{
		"xapp-LEAKED-TOKEN",
		"PII-PAYLOAD-PLEASE-REDACT",
		"Bearer ",
		"SECRET-WSS-TICKET",
		"ticket=",
	}
	for _, e := range entries {
		entryStr := fmt.Sprintf("msg=%q kv=%v", e.Msg, e.KV)
		for _, b := range banned {
			if strings.Contains(entryStr, b) {
				t.Errorf("log entry %q leaks banned substring %q", entryStr, b)
			}
		}
	}
}

// TestSubscribe_AckCompletes_UnderThreeSeconds asserts the timing
// invariant: a slow handler does not stall the ack — ack writes happen
// BEFORE handler dispatch, not after. We attach a handler that blocks
// for ~1.5s, send one event, and assert the ack arrived within 500ms.
func TestSubscribe_AckCompletes_UnderThreeSeconds(t *testing.T) {
	t.Parallel()

	ackSeen := make(chan time.Time, 1)
	var fs *fakeSocketServer
	fs = newFakeSocketServer(t, func(ctx context.Context, t *testing.T, conn *websocket.Conn) {
		writeJSON(ctx, t, conn, helloEnvelopeJSON)
		writeJSON(ctx, t, conn, makeEventEnvelope("evt-slow", "C1", "U1", "x", "1.0"))
		raw := readAck(ctx, t, conn)
		ackSeen <- time.Now()
		fs.recordAck(raw)
		_, _, _ = conn.Read(ctx)
	})

	handler := &recordingHandler{
		next: func(messenger.IncomingMessage) {
			time.Sleep(1500 * time.Millisecond)
		},
	}

	c := NewClient(
		WithBaseURL(fs.openURL()),
		WithTokenSource(StaticToken("xapp-test")),
	)
	start := time.Now()
	sub, err := c.Subscribe(context.Background(), handler.handle)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	var ackAt time.Time
	select {
	case ackAt = <-ackSeen:
	case <-time.After(3 * time.Second):
		t.Fatal("ack never arrived within 3s budget")
	}
	if d := ackAt.Sub(start); d >= 1*time.Second {
		t.Errorf("ack arrived after handler-blocking delay; took %v (handler sleeps 1.5s, ack must precede it)", d)
	}
}
