package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
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

// Reconnect tuning defaults — mirror outbox/keepclient backoff-with-jitter
// model. Production callers can override every value via the matching
// [ClientOption]. The defaults assume Slack's documented Socket Mode
// disconnect cadence (warning ~15s; refresh_requested every ~24h) and
// give the client enough budget to ride a brief network blip without
// surfacing the failure to the operator.
const (
	defaultReconnectInitialDelay = 100 * time.Millisecond
	defaultReconnectMaxDelay     = 30 * time.Second
	defaultMaxReconnectAttempts  = 5

	// defaultPingInterval is the application-layer ping cadence. If no
	// envelope is received within this interval the read loop emits a
	// ping; if no pong arrives within [defaultPingTimeout] the loop
	// triggers a reconnect. Slack's keepalive cadence is documented as
	// 15-30s — 30s is the conservative choice.
	defaultPingInterval = 30 * time.Second

	// defaultPingTimeout is the per-ping pong deadline. A pong-timeout
	// triggers a reconnect with backoff.
	defaultPingTimeout = 10 * time.Second

	// reconnectJitterFraction bounds the additive jitter applied to each
	// backoff sleep at ±25% of the un-jittered delay.
	reconnectJitterFraction = 0.25
)

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

// WithSocketModeReconnectInitialDelay overrides the first-attempt
// backoff before the resilient reconnect loop dials a fresh WSS URL.
// Defaults to 100ms. Non-positive values fall back to the default.
func WithSocketModeReconnectInitialDelay(d time.Duration) ClientOption {
	return func(c *clientConfig) {
		if d > 0 {
			c.socketReconnectInitialDelay = d
		}
	}
}

// WithSocketModeReconnectMaxDelay caps the per-attempt reconnect
// backoff. Defaults to 30s. Non-positive values fall back to the
// default. The cap is applied BEFORE jitter so the realised sleep can
// briefly exceed maxDelay by up to ±25%.
func WithSocketModeReconnectMaxDelay(d time.Duration) ClientOption {
	return func(c *clientConfig) {
		if d > 0 {
			c.socketReconnectMaxDelay = d
		}
	}
}

// WithSocketModeMaxReconnectAttempts sets the maximum number of
// CONSECUTIVE reconnect attempts the read loop performs before
// surfacing [ErrReconnectExhausted] via [messenger.Subscription.Stop].
// Defaults to 5. Non-positive values fall back to the default.
func WithSocketModeMaxReconnectAttempts(n int) ClientOption {
	return func(c *clientConfig) {
		if n > 0 {
			c.socketMaxReconnectAttempts = n
		}
	}
}

// WithSocketModePingInterval overrides the application-layer ping
// cadence. The read loop sends a ping if no envelope (Slack-side or
// our own pong receipt) has arrived within this interval. Defaults to
// 30s. Non-positive values fall back to the default.
func WithSocketModePingInterval(d time.Duration) ClientOption {
	return func(c *clientConfig) {
		if d > 0 {
			c.socketPingInterval = d
		}
	}
}

// WithSocketModePingTimeout overrides the per-ping pong deadline. If
// no pong arrives within this window after a ping is sent, the read
// loop triggers a reconnect. Defaults to 10s. Non-positive values
// fall back to the default.
func WithSocketModePingTimeout(d time.Duration) ClientOption {
	return func(c *clientConfig) {
		if d > 0 {
			c.socketPingTimeout = d
		}
	}
}

// withSocketModeSleeperOption is the unexported test seam that lets
// the test suite inject a fake clock into the reconnect backoff loop
// without exporting a public option.
func withSocketModeSleeperOption(s sleeper) ClientOption {
	return func(c *clientConfig) {
		if s != nil {
			c.socketSleeper = s
		}
	}
}

// withSocketModeRandOption is the unexported test seam that lets the
// test suite substitute a deterministic jitter source into the
// reconnect backoff loop.
func withSocketModeRandOption(fn func() float64) ClientOption {
	return func(c *clientConfig) {
		if fn != nil {
			c.socketRand = fn
		}
	}
}

// Subscribe opens a Slack Socket Mode connection and dispatches each
// inbound message envelope to `handler` with transparent reconnect.
//
// Lifecycle:
//
//  1. POST `apps.connections.open` (rate-limited via the configured
//     [RateLimiter]) to obtain a one-shot WSS URL.
//  2. Dial the WSS URL.
//  3. Wait for the `hello` envelope (timeout via
//     [WithSocketModeHelloTimeout]). On INITIAL hello-timeout or
//     wrong-shape first frame the connection is closed and the
//     error returned to the caller (subscription never came up). On
//     hello-timeout during a RECONNECT the loop continues with the
//     bounded retry budget.
//  4. Spawn a goroutine that reads envelopes, ACKS each event-bearing
//     envelope back to Slack BEFORE invoking the handler, then
//     dispatches the decoded [messenger.IncomingMessage]
//     synchronously (so [messenger.Subscription.Stop] blocks for the
//     in-flight handler per the messenger contract).
//  5. On `disconnect` envelope, transport error, or pong-timeout the
//     loop closes the current WSS, dials a fresh
//     `apps.connections.open` URL with backoff-with-jitter, and
//     resumes reading. The caller's [messenger.MessageHandler] keeps
//     receiving events without observing the reconnect.
//  6. After [WithSocketModeMaxReconnectAttempts] consecutive
//     reconnect failures the read loop exits with the wrapped
//     [ErrReconnectExhausted] surfaced via the next
//     [messenger.Subscription.Stop] call.
//
// The handler returning a non-nil error is logged and discarded —
// Phase 1 is at-most-once at the adapter layer per the
// [messenger.MessageHandler] contract; durable redelivery is M3.7's
// job further upstream.
//
// Note: the handler is invoked with a context whose lifetime is the
// [messenger.Subscription], NOT the Subscribe-call ctx. The handler
// observes cancellation when [messenger.Subscription.Stop] is called;
// cancelling the original Subscribe-call ctx after Subscribe has
// returned has no effect on the handler.
//
// Error mapping (initial open):
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
// Error mapping (reconnect-loop exhaustion, surfaced via Stop):
//
//   - [ErrReconnectExhausted] wrapping the last transport error.
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

	// Initial open: synchronous open + dial + hello so the caller sees
	// any auth/scope/wire failure before Subscribe returns. Reconnect
	// failures (which only happen AFTER this initial open) surface
	// via [messenger.Subscription.Stop].
	conn, err := c.openSocketModeConn(ctx)
	if err != nil {
		return nil, err
	}

	sub := newSocketSubscription(c, conn)
	//nolint:contextcheck // intentional: read loop runs on a Background-derived ctx the Stop() owns; the Subscribe ctx is consumed for the open + dial + hello, then handed off.
	sub.run(handler)

	return sub, nil
}

// openSocketModeConn drives one round of apps.connections.open + dial
// + hello. Returned conn has already received its `hello` envelope and
// is ready for the read loop. Caller owns the conn (closes on error or
// when the read loop unwinds).
func (c *Client) openSocketModeConn(ctx context.Context) (*websocket.Conn, error) {
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

	// 2. Dial the WSS URL. The dial uses the supplied ctx — a ctx
	//    cancel mid-dial aborts the handshake.
	conn, err := c.dialWebSocket(ctx, resp.URL)
	if err != nil {
		return nil, fmt.Errorf("slack: socket mode: dial: %w", err)
	}

	// 3. Wait for hello. On any failure, close the conn cleanly and
	//    surface the error to the caller — the round failed.
	helloCtx, cancelHello := context.WithTimeout(ctx, c.socketHelloTimeout())
	defer cancelHello()
	if err := awaitHello(helloCtx, conn); err != nil {
		_ = conn.Close(websocket.StatusNormalClosure, "hello not received")
		return nil, err
	}
	c.cfg.logger.Log(ctx, "slack: socket mode: hello received", "url", redactWSURL(resp.URL))
	return conn, nil
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

// socketReconnectInitialDelay returns the configured first-attempt
// reconnect backoff, falling back to the package default when unset.
func (c *Client) socketReconnectInitialDelay() time.Duration {
	if c.cfg.socketReconnectInitialDelay > 0 {
		return c.cfg.socketReconnectInitialDelay
	}
	return defaultReconnectInitialDelay
}

// socketReconnectMaxDelay returns the configured per-attempt cap for
// reconnect backoff, falling back to the package default when unset.
func (c *Client) socketReconnectMaxDelay() time.Duration {
	if c.cfg.socketReconnectMaxDelay > 0 {
		return c.cfg.socketReconnectMaxDelay
	}
	return defaultReconnectMaxDelay
}

// socketMaxReconnectAttempts returns the configured retry budget for
// the reconnect loop, falling back to the package default when unset.
func (c *Client) socketMaxReconnectAttempts() int {
	if c.cfg.socketMaxReconnectAttempts > 0 {
		return c.cfg.socketMaxReconnectAttempts
	}
	return defaultMaxReconnectAttempts
}

// socketPingInterval returns the configured ping cadence, falling
// back to the package default when unset.
func (c *Client) socketPingInterval() time.Duration {
	if c.cfg.socketPingInterval > 0 {
		return c.cfg.socketPingInterval
	}
	return defaultPingInterval
}

// socketPingTimeout returns the configured pong deadline, falling
// back to the package default when unset.
func (c *Client) socketPingTimeout() time.Duration {
	if c.cfg.socketPingTimeout > 0 {
		return c.cfg.socketPingTimeout
	}
	return defaultPingTimeout
}

// socketSleeper returns the configured sleeper (test seam) or the
// production default that honours ctx cancellation.
func (c *Client) socketSleeper() sleeper {
	if c.cfg.socketSleeper != nil {
		return c.cfg.socketSleeper
	}
	return realSleeper{}
}

// socketRand returns the configured jitter source (test seam) or the
// production default ([math/rand/v2.Float64]).
func (c *Client) socketRand() func() float64 {
	if c.cfg.socketRand != nil {
		return c.cfg.socketRand
	}
	return rand.Float64
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
// [Client.Subscribe]. It owns the CURRENT *websocket.Conn (mutated
// across reconnects under [socketSubscription.connMu]) and one read
// goroutine. Stop is idempotent and blocks until the goroutine exits.
type socketSubscription struct {
	client *Client

	// connMu guards conn against concurrent read-loop reconnect and
	// caller-driven Stop. Only the read goroutine writes conn.
	connMu sync.Mutex
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

// currentConn returns the active connection under lock. Returns nil
// once Stop has cleared it.
func (s *socketSubscription) currentConn() *websocket.Conn {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return s.conn
}

// swapConn replaces the active connection with newConn and returns
// the previous (caller is responsible for closing it). Called from
// the read goroutine on a successful reconnect.
func (s *socketSubscription) swapConn(newConn *websocket.Conn) *websocket.Conn {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	old := s.conn
	s.conn = newConn
	return old
}

// run spawns the read goroutine. The goroutine reads envelopes one at
// a time; per envelope it acks (when the type calls for it), decodes,
// and dispatches to the handler synchronously. On disconnect envelope,
// transport error, or pong-timeout the loop reconnects with backoff.
// On runCtx cancellation or exhausted retry budget the goroutine
// closes `done` and exits — Stop reads `done` to know when the
// goroutine has observed the cancel.
func (s *socketSubscription) run(handler messenger.MessageHandler) {
	go s.sessionLoop(handler)
}

// Stop satisfies [messenger.Subscription]. Cancels the read loop,
// closes the WebSocket with a normal-closure status, and blocks until
// the read goroutine exits (so an in-flight handler completes before
// Stop returns). Idempotent — subsequent calls return the same value
// without reissuing the close-frame.
//
// Returns nil for a clean stop (caller-initiated, ctx cancellation).
// Returns [ErrReconnectExhausted] (wrapped) if the read goroutine
// exited because the bounded reconnect retry budget was burned.
func (s *socketSubscription) Stop() error {
	s.stopOnce.Do(func() {
		// Cancelling first unblocks any pending conn.Read so the
		// read loop can observe the close and exit. Close after
		// the cancel sends the normal-closure frame to Slack.
		s.runCancel()
		// websocket.Close sends a close frame and waits for the
		// peer's close response (with a short internal deadline);
		// errors here are non-fatal — the conn is being torn down
		// regardless. Close errors are intentionally swallowed: the
		// documented contract is "Stop returns nil on a clean stop"
		// and "the wrapped reconnect-exhaustion error otherwise" —
		// a close-frame write race never overrides either.
		if conn := s.currentConn(); conn != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "subscription stop")
		}
		<-s.done
	})
	return s.stopErr
}
