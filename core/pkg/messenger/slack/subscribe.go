package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// appsConnectionsOpenMethod is the Slack Web API method [Client.Subscribe]
// posts to obtain the per-connection WSS URL. Hoisted to a package
// constant so the rate-limiter registry (`defaultMethodTiers`) and the
// request path stay in sync via the compiler.
const appsConnectionsOpenMethod = "apps.connections.open"

// defaultHelloTimeout is the maximum we wait for the `hello` envelope
// after dialling the WSS URL before giving up. Slack typically emits
// the hello inside 200ms; 5s leaves enough margin for slow networks
// without hanging the caller.
const defaultHelloTimeout = 5 * time.Second

// defaultAckTimeout is the per-ack write deadline. Slack's Socket Mode
// contract gives the client 3s to ack each event envelope; we apply
// the same budget to the underlying ws write so a wedged write never
// blocks the read loop indefinitely.
const defaultAckTimeout = 3 * time.Second

// Sentinel errors specific to the Subscribe path.
var (
	// ErrHelloTimeout surfaces from [Client.Subscribe] when the WSS
	// peer fails to emit a `hello` envelope within the configured
	// (or default) timeout. Callers retrying a subscription typically
	// log + dial again; the M4.2.c.2 reconnect wrapper handles this
	// case transparently. Matchable via [errors.Is].
	ErrHelloTimeout = errors.New("slack: socket mode: hello timeout")

	// ErrUnexpectedEnvelope surfaces from [Client.Subscribe] when the
	// first envelope on the WSS connection is not `hello` (e.g. a
	// `disconnect` straight away, or an event_api before hello —
	// both protocol violations). Matchable via [errors.Is].
	ErrUnexpectedEnvelope = errors.New("slack: socket mode: unexpected envelope")
)

// appsConnectionsOpenResponse is the subset of the Slack response
// [Client.Subscribe] decodes from `apps.connections.open`. The full
// envelope also carries `error` (already lifted by [Client.Do] into
// an [*APIError]) and a `response_metadata` block we do not need.
type appsConnectionsOpenResponse struct {
	OK  bool   `json:"ok"`
	URL string `json:"url"`
}

// WithSocketModeHelloTimeout overrides the per-Subscribe wait for the
// initial `hello` envelope. Pass on the [Client] (NewClient) so every
// Subscribe call inherits the value. A non-positive duration falls
// back to [defaultHelloTimeout].
func WithSocketModeHelloTimeout(d time.Duration) ClientOption {
	return func(c *clientConfig) {
		if d > 0 {
			c.socketHelloTimeout = d
		}
	}
}

// socketModeDialer is the function shape the [WithSocketModeDialer]
// hook accepts. The hook receives the resolved URL (with the per-conn
// ticket query string) and the inherited Subscribe context, and
// returns an established *websocket.Conn or a wrapped dial error.
type socketModeDialer func(ctx context.Context, urlStr string) (*websocket.Conn, error)

// WithSocketModeDialer overrides the function used to dial the WSS
// URL. Defaults to [websocket.Dial]. Tests substitute a hook that
// returns an in-memory pair, but the production happy-path uses the
// stdlib transport via the coder/websocket library.
//
// A nil hook is ignored so callers can apply a conditional override
// without explicit branching.
func WithSocketModeDialer(d socketModeDialer) ClientOption {
	return func(c *clientConfig) {
		if d != nil {
			c.socketDialer = d
		}
	}
}

// Subscribe opens a Slack Socket Mode connection and dispatches each
// inbound message envelope to `handler`.
//
// Lifecycle:
//
//  1. POST `apps.connections.open` (rate-limited via the configured
//     [RateLimiter]) to obtain a one-shot WSS URL.
//  2. Dial the WSS URL.
//  3. Wait for the `hello` envelope (timeout via
//     [WithSocketModeHelloTimeout]). On timeout or wrong-shape first
//     frame, the connection is closed and the error returned.
//  4. Spawn a goroutine that reads envelopes, ACKS each event-bearing
//     envelope back to Slack BEFORE invoking the handler, then
//     dispatches the decoded [messenger.IncomingMessage]
//     synchronously (so [messenger.Subscription.Stop] blocks for the
//     in-flight handler per the messenger contract).
//  5. Return a [messenger.Subscription] handle whose Stop closes the
//     connection cleanly and waits for the read goroutine to exit.
//
// The handler returning a non-nil error is logged and discarded —
// Phase 1 is at-most-once at the adapter layer per the
// [messenger.MessageHandler] contract; durable redelivery is M3.7's
// job further upstream.
//
// What this method does NOT do (deferred to M4.2.c.2):
//
//   - Reconnect on `disconnect` envelope or transport error. The c.1
//     contract treats both as terminal: the read goroutine exits, the
//     connection closes, [messenger.Subscription.Stop] returns nil.
//   - Application-layer ping/pong heartbeat. The coder/websocket
//     library handles control-frame echoes natively.
//   - Exponential backoff or retry budget.
//
// Error mapping:
//
//   - nil handler                   → [messenger.ErrInvalidHandler]
//   - apps.connections.open `error` → [*APIError] (Code populated)
//   - apps.connections.open 429     → [*APIError] wrapping [ErrRateLimited]
//   - hello timeout                 → [ErrHelloTimeout]
//   - non-hello first frame         → [ErrUnexpectedEnvelope]
//   - WSS dial / handshake failure  → wrapped error (no portable sentinel)
//   - ctx cancellation              → wrapped [context.Canceled] /
//     [context.DeadlineExceeded]
//
// Concurrency: a single Subscribe call spawns ONE read goroutine
// the returned subscription owns. Multiple Subscribe calls on the
// same [Client] are independent (Client itself is stateless after
// construction).
func (c *Client) Subscribe(ctx context.Context, handler messenger.MessageHandler) (messenger.Subscription, error) {
	if handler == nil {
		return nil, fmt.Errorf("slack: %s: %w", appsConnectionsOpenMethod, messenger.ErrInvalidHandler)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// 1. apps.connections.open — rate-limited, token-resolved,
	//    redacted via [Client.Do]. The response carries the
	//    per-connection WSS URL.
	var resp appsConnectionsOpenResponse
	if err := c.Do(ctx, appsConnectionsOpenMethod, nil, &resp); err != nil {
		return nil, err
	}
	if resp.URL == "" {
		return nil, fmt.Errorf("slack: %s: response missing url", appsConnectionsOpenMethod)
	}

	// 2. Dial the WSS URL. The dial uses the Subscribe ctx — a ctx
	//    cancel mid-dial aborts the handshake.
	conn, err := c.dialWebSocket(ctx, resp.URL)
	if err != nil {
		return nil, fmt.Errorf("slack: socket mode: dial: %w", err)
	}

	// 3. Wait for hello. On any failure, close the conn cleanly and
	//    surface the error to the caller — the subscription never
	//    came up.
	helloCtx, cancelHello := context.WithTimeout(ctx, c.socketHelloTimeout())
	defer cancelHello()
	if err := awaitHello(helloCtx, conn); err != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "hello not received")
		return nil, err
	}
	c.cfg.logger.Log(ctx, "slack: socket mode: hello received", "url", redactWSURL(resp.URL))

	// 4. Spawn the read loop. The subscription owns the goroutine
	//    and the conn.
	sub := newSocketSubscription(c, conn)
	//nolint:contextcheck // intentional: read loop runs on a Background-derived ctx the Stop() owns; the Subscribe ctx is consumed for the open + dial + hello, then handed off.
	sub.run(handler)

	return sub, nil
}

// dialWebSocket runs the configured [WithSocketModeDialer] hook (or
// the default coder/websocket Dial) to upgrade the supplied URL to a
// WSS connection. The handshake honours ctx cancellation.
func (c *Client) dialWebSocket(ctx context.Context, urlStr string) (*websocket.Conn, error) {
	if c.cfg.socketDialer != nil {
		return c.cfg.socketDialer(ctx, urlStr)
	}
	conn, resp, err := websocket.Dial(ctx, urlStr, nil)
	if err != nil {
		return nil, err
	}
	// Per the coder/websocket contract, the http.Response body is
	// always nil after a successful upgrade — but the linter does
	// not see that, so close defensively when non-nil to satisfy
	// bodyclose without leaking on the success path.
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return conn, nil
}

// socketHelloTimeout returns the configured hello timeout, falling
// back to [defaultHelloTimeout] when unset.
func (c *Client) socketHelloTimeout() time.Duration {
	if c.cfg.socketHelloTimeout > 0 {
		return c.cfg.socketHelloTimeout
	}
	return defaultHelloTimeout
}

// awaitHello reads a single envelope from `conn` and asserts the
// `type` field equals "hello". A timeout from ctx surfaces as
// [ErrHelloTimeout]; a wrong-shape first envelope surfaces as
// [ErrUnexpectedEnvelope].
func awaitHello(ctx context.Context, conn *websocket.Conn) error {
	_, raw, err := conn.Read(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return ErrHelloTimeout
		}
		return fmt.Errorf("slack: socket mode: read hello: %w", err)
	}
	var env rawEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("slack: socket mode: decode hello: %w", err)
	}
	if env.Type != envelopeTypeHello {
		return fmt.Errorf("slack: socket mode: first envelope type=%q: %w", env.Type, ErrUnexpectedEnvelope)
	}
	return nil
}

// socketSubscription is the [messenger.Subscription] returned by
// [Client.Subscribe]. It owns one *websocket.Conn and one read
// goroutine. Stop is idempotent and blocks until the goroutine exits.
type socketSubscription struct {
	client *Client
	conn   *websocket.Conn

	// runCtx is cancelled by Stop to break the read loop. The read
	// loop calls conn.Read(runCtx); cancelling runCtx unblocks the
	// read with ctx.Err().
	runCtx    context.Context
	runCancel context.CancelFunc

	// done is closed by the read goroutine when it exits; Stop waits
	// on it to honour the messenger.Subscription "blocks until
	// in-flight handler completes" contract.
	done chan struct{}

	stopOnce sync.Once
	stopErr  error
}

// newSocketSubscription wires the per-subscription bookkeeping. The
// runCtx is decoupled from the Subscribe-call ctx so a parent ctx
// cancel does NOT race the user's Stop call (the read loop honours
// the runCtx; Stop cancels it explicitly).
//
// (*socketSubscription).Stop via stopOnce — the linter does not see
// the indirect store.
//
//nolint:gosec // G118: the cancel func is invoked deterministically in
func newSocketSubscription(c *Client, conn *websocket.Conn) *socketSubscription {
	ctx, cancel := context.WithCancel(context.Background())
	return &socketSubscription{
		client:    c,
		conn:      conn,
		runCtx:    ctx,
		runCancel: cancel,
		done:      make(chan struct{}),
	}
}

// run spawns the read goroutine. The goroutine reads envelopes one at
// a time; per envelope it acks (when the type calls for it), decodes,
// and dispatches to the handler synchronously. On any read error,
// disconnect envelope, or runCtx cancellation, the goroutine closes
// `done` and exits — Stop reads `done` to know when the goroutine has
// observed the cancel.
func (s *socketSubscription) run(handler messenger.MessageHandler) {
	go s.readLoop(handler)
}

// readLoop is the per-subscription event-pump. Single goroutine,
// single shared *websocket.Conn — no concurrent reads. Acks are
// written back on the same connection but always BEFORE the handler
// dispatch so a slow handler cannot miss the 3s ack budget.
func (s *socketSubscription) readLoop(handler messenger.MessageHandler) {
	defer close(s.done)
	for {
		_, raw, err := s.conn.Read(s.runCtx)
		if err != nil {
			s.logReadError(err)
			return
		}
		var env rawEnvelope
		if jerr := json.Unmarshal(raw, &env); jerr != nil {
			s.client.cfg.logger.Log(
				s.runCtx, "slack: socket mode: decode envelope failed",
				"err_type", fmt.Sprintf("%T", jerr),
			)
			continue
		}
		if env.Type == envelopeTypeDisconnect {
			s.client.cfg.logger.Log(
				s.runCtx, "slack: socket mode: disconnect requested",
				"reason", env.Reason,
			)
			// c.1 contract: terminal close. M4.2.c.2 will route
			// this back into a reconnect.
			return
		}
		if env.EnvelopeID != "" {
			s.ackEnvelope(env.EnvelopeID)
		}
		s.dispatch(env, handler)
	}
}

// logReadError writes a structured log entry for a read-side failure.
// Distinguishes ctx-cancellation (clean Stop) from network errors so
// the operator can tell the two apart in production.
func (s *socketSubscription) logReadError(err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: read loop stopped",
			"reason", "context canceled",
		)
		return
	}
	// websocket.CloseError carries the close-frame status; expose
	// the numeric code so operators can correlate with Slack's
	// disconnect log without leaking the close-frame reason
	// (which Slack documents as free-text and may carry PII).
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: read loop stopped",
			"reason", "ws close",
			"code", int(ce.Code),
		)
		return
	}
	s.client.cfg.logger.Log(
		s.runCtx, "slack: socket mode: read loop stopped",
		"reason", "transport error",
		"err_type", fmt.Sprintf("%T", err),
	)
}

// ackEnvelope writes a `{"envelope_id": "..."}` text frame back to
// Slack. Slack's contract: ack within 3s. The write uses a per-ack
// timeout independent of runCtx so a stuck Slack write never blocks
// the read loop forever.
func (s *socketSubscription) ackEnvelope(envelopeID string) {
	body, err := json.Marshal(ackPayload{EnvelopeID: envelopeID})
	if err != nil {
		// Marshalling a single-string struct cannot fail in
		// practice; defensive log + continue.
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: marshal ack failed",
			"err_type", fmt.Sprintf("%T", err),
		)
		return
	}
	ackCtx, cancel := context.WithTimeout(s.runCtx, defaultAckTimeout)
	defer cancel()
	if werr := s.conn.Write(ackCtx, websocket.MessageText, body); werr != nil {
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: ack write failed",
			"err_type", fmt.Sprintf("%T", werr),
		)
		return
	}
	s.client.cfg.logger.Log(
		s.runCtx, "slack: socket mode: ack sent",
		"type", "events_api",
	)
}

// dispatch decodes a non-hello, non-disconnect envelope and calls the
// handler when the envelope contains a Slack `message` event. Other
// payload kinds (slash_commands, interactive, non-message events) are
// dropped — M4.2.c.1 wires the message-shaped path only.
//
// Handler errors are logged but not redelivered (per the
// [messenger.MessageHandler] contract).
func (s *socketSubscription) dispatch(env rawEnvelope, handler messenger.MessageHandler) {
	msg, ok := decodeIncoming(env)
	if !ok {
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: envelope dropped",
			"type", env.Type,
		)
		return
	}
	if err := handler(s.runCtx, msg); err != nil {
		s.client.cfg.logger.Log(
			s.runCtx, "slack: socket mode: handler error",
			"err_type", fmt.Sprintf("%T", err),
		)
	}
}

// Stop satisfies [messenger.Subscription]. Cancels the read loop,
// closes the WebSocket with a normal-closure status, and blocks until
// the read goroutine exits (so an in-flight handler completes before
// Stop returns). Idempotent — subsequent calls return the same nil
// without reissuing the close-frame.
func (s *socketSubscription) Stop() error {
	s.stopOnce.Do(func() {
		// Cancelling first unblocks any pending conn.Read so the
		// read loop can observe the close and exit. Close after
		// the cancel sends the normal-closure frame to Slack.
		s.runCancel()
		// websocket.Close sends a close frame and waits for the
		// peer's close response (with a short internal deadline);
		// errors here are non-fatal — the conn is being torn down
		// regardless.
		if err := s.conn.Close(websocket.StatusNormalClosure, "subscription stop"); err != nil {
			// Only surface the error when the read loop did NOT
			// already report a cleaner close — for the c.1
			// contract a normal Stop returns nil.
			s.stopErr = nil
		}
		<-s.done
	})
	return s.stopErr
}
