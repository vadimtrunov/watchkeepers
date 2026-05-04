package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// Default tuning. Production callers can override every value via the
// matching [Option].
const (
	defaultIdempotencyCacheSize = 1024
	defaultPublishTimeout       = 5 * time.Second
	defaultMaxPublishRetries    = 3
	defaultRetryInitialDelay    = 25 * time.Millisecond
	defaultRetryMaxDelay        = 1 * time.Second

	// retryJitterFraction bounds the additive jitter applied to each
	// retry sleep at ±25% of the un-jittered delay. Mirrors the
	// keepclient resilient-stream backoff model.
	retryJitterFraction = 0.25
)

// Stream is the minimal subset of [keepclient.ResilientStream] the
// consumer drives. Defined as an interface here so tests can substitute
// a hand-rolled fake without standing up a Keep server, and so
// production code never imports the concrete `*keepclient.ResilientStream`
// type at all. Mirrors the lifecycle.LocalKeepClient + cron.LocalPublisher
// one-way import-cycle-break pattern documented in `docs/LESSONS.md`
// (M2b.6 / M3.2.b / M3.6).
type Stream interface {
	// Next returns the next [keepclient.Event] from the stream. The
	// concrete keepclient implementation transparently reconnects on
	// transport errors and forwards Last-Event-ID across reconnects.
	Next(ctx context.Context) (keepclient.Event, error)
	// Close releases the underlying transport. Idempotent.
	Close() error
}

// LocalSubscriber is the minimal subset of [keepclient.Client] that the
// consumer drives. The single method opens a fresh resilient SSE
// stream; the consumer drives [Stream.Next] until ctx cancel or
// terminal error. `*keepclient.Client` does NOT satisfy this interface
// directly — its `SubscribeResilient` returns the concrete
// `*keepclient.ResilientStream` (which DOES satisfy [Stream]). Callers
// adapt with a tiny inline closure:
//
//	sub := outbox.LocalSubscriberFunc(func(ctx context.Context) (outbox.Stream, error) {
//	    return kc.SubscribeResilient(ctx, opts...)
//	})
//
// Defining the interface this way (a single `Subscribe` method
// returning [Stream]) keeps the Phase 1 wiring obvious without coupling
// consumer to keepclient's resilient-option surface.
type LocalSubscriber interface {
	Subscribe(ctx context.Context) (Stream, error)
}

// LocalSubscriberFunc adapts an ordinary `func(ctx) (Stream, error)`
// into a [LocalSubscriber]. Mirrors `http.HandlerFunc` shape.
type LocalSubscriberFunc func(ctx context.Context) (Stream, error)

// Subscribe satisfies [LocalSubscriber] for [LocalSubscriberFunc].
func (f LocalSubscriberFunc) Subscribe(ctx context.Context) (Stream, error) {
	return f(ctx)
}

// LocalBus is the minimal subset of [eventbus.Bus] the consumer drives.
// `*eventbus.Bus` satisfies it as-is; the compile-time assertion lives
// in `consumer_test.go`, mirroring the cron.LocalPublisher pattern.
type LocalBus interface {
	Publish(ctx context.Context, topic string, event any) error
}

// Logger is the diagnostic sink wired in via [WithLogger]. The shape
// mirrors the secrets / capability / cron / lifecycle / keeperslog
// Logger interfaces: a single `Log(ctx, msg, kv...)` so callers can
// substitute structured loggers (slog, zap wrapper, etc.) without
// losing type compatibility.
//
// IMPORTANT (redaction discipline): implementations MUST NOT log raw
// event payloads. The consumer never passes [keepclient.Event.Payload]
// or [DeliveredEvent.Payload] through the logger; only metadata
// (event_id, event_type, attempt count, error type) appears in log
// entries. Callers wrapping their own Logger should preserve this
// invariant.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// DeliveredEvent is the value the consumer publishes onto the
// [LocalBus]. Wraps the decoded SSE [keepclient.Event] so subscribers
// can inspect every field (id / event_type / payload) without
// re-parsing. The shape is intentionally small and stable; future
// fields are additive only.
type DeliveredEvent struct {
	// ID is the outbox row UUID (the SSE `id:` field). Callers can
	// treat it as the idempotency key — the consumer guarantees no
	// duplicate emit within its in-memory dedup window.
	ID string
	// EventType is the SSE `event:` field. Used as the bus topic.
	EventType string
	// Payload is the raw JSON payload from the SSE `data:` field.
	// Carries the outbox row's `payload` jsonb verbatim — usually a
	// keeperslog envelope when the producer was the keeperslog writer.
	Payload json.RawMessage
}

// Option configures a [Consumer] at construction time. Pass options to
// [New]; later options override earlier ones for the same field.
type Option func(*config)

// config is the internal mutable bag the [Option] callbacks populate.
// Held in a separate type so [Consumer] itself stays immutable after
// [New] returns.
type config struct {
	logger            Logger
	idempotencyCache  int
	publishTimeout    time.Duration
	maxPublishRetries int
	retryInitialDelay time.Duration
	retryMaxDelay     time.Duration
	rand              func() float64
	sleeper           sleeper
}

// WithLogger wires a diagnostic sink onto the returned [*Consumer].
// When set, the consumer emits structured log entries on per-event
// publish failures, exhausted retries, malformed events, and
// stream-level errors. A nil logger is a no-op so callers can always
// pass through whatever they have.
//
// IMPORTANT: log entries NEVER carry [DeliveredEvent.Payload] data.
// Only metadata (event_id, event_type, attempt count, error type) is
// logged.
func WithLogger(l Logger) Option {
	return func(c *config) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithIdempotencyCacheSize overrides the in-memory LRU dedup capacity.
// Defaults to 1024. A non-positive size DISABLES dedup entirely
// (every event is published verbatim) — useful for tests that want to
// assert raw delivery, but a programmer error in production wiring.
func WithIdempotencyCacheSize(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.idempotencyCache = n
		} else {
			c.idempotencyCache = 0
		}
	}
}

// WithPublishTimeout caps the wall-clock duration the consumer will
// wait for a single bus publish to complete before treating it as a
// transient failure. Defaults to 5 seconds. Non-positive values fall
// back to the default.
func WithPublishTimeout(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.publishTimeout = d
		}
	}
}

// WithMaxPublishRetries sets the number of CONSECUTIVE publish
// attempts the consumer makes for a single event before giving up and
// surfacing [ErrPublishExhausted] via the [Logger]. Defaults to 3
// (initial attempt + 2 retries). Non-positive values fall back to the
// default.
func WithMaxPublishRetries(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.maxPublishRetries = n
		}
	}
}

// WithRetryInitialDelay overrides the first-attempt backoff between
// publish retries. Defaults to 25ms. Non-positive values fall back to
// the default.
func WithRetryInitialDelay(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.retryInitialDelay = d
		}
	}
}

// WithRetryMaxDelay caps the per-attempt publish-retry backoff.
// Defaults to 1 second. Non-positive values fall back to the default.
// The cap is applied BEFORE jitter so the realised sleep can briefly
// exceed maxDelay by up to ±25%.
func WithRetryMaxDelay(d time.Duration) Option {
	return func(c *config) {
		if d > 0 {
			c.retryMaxDelay = d
		}
	}
}

// withSleeperOption is the unexported test seam that lets the test
// suite inject a fake clock without exporting a public option.
func withSleeperOption(s sleeper) Option {
	return func(c *config) {
		if s != nil {
			c.sleeper = s
		}
	}
}

// withRandOption is the unexported test seam that lets the test suite
// substitute a deterministic jitter source.
func withRandOption(fn func() float64) Option {
	return func(c *config) {
		if fn != nil {
			c.rand = fn
		}
	}
}

// consumerState is a 3-state lifecycle (not-started / started /
// stopped). The states are linear (no resurrection), so a single int
// guarded by [Consumer.mu] is sufficient.
type consumerState int

const (
	stateNotStarted consumerState = iota
	stateStarted
	stateStopped
)

// Consumer is the outbox→bus bridge. Construct via [New]; the zero
// value is not usable. Methods are safe for concurrent use across
// goroutines once the consumer is constructed; lifecycle transitions
// are guarded by an internal mutex. The receiver wraps a single
// [LocalSubscriber] and a single [LocalBus] — production code in this
// package never depends on the concrete keepclient / eventbus types.
type Consumer struct {
	sub LocalSubscriber
	bus LocalBus
	cfg config

	dedup *lruDedup

	mu       sync.Mutex
	state    consumerState
	cancel   context.CancelFunc
	doneCh   chan struct{}
	stopOnce sync.Once
	stopErr  error
}

// New constructs a [Consumer] backed by the supplied [LocalSubscriber]
// and [LocalBus]. Both are required; passing nil for either is a
// programmer error and panics with a clear message — matches the panic
// discipline of [lifecycle.New], [cron.New], [keeperslog.New], and
// [keepclient.WithBaseURL]. A consumer with no subscriber or no bus
// could not do anything useful, and silently no-oping every fire would
// mask the bug.
//
// The defaults are: logger = nil (diagnostics disabled),
// idempotencyCache = 1024, publishTimeout = 5s, maxPublishRetries = 3,
// retryInitialDelay = 25ms, retryMaxDelay = 1s. Supplied options
// override them.
func New(sub LocalSubscriber, bus LocalBus, opts ...Option) *Consumer {
	if sub == nil {
		panic("outbox: New: subscriber must not be nil")
	}
	if bus == nil {
		panic("outbox: New: bus must not be nil")
	}
	cfg := config{
		idempotencyCache:  defaultIdempotencyCacheSize,
		publishTimeout:    defaultPublishTimeout,
		maxPublishRetries: defaultMaxPublishRetries,
		retryInitialDelay: defaultRetryInitialDelay,
		retryMaxDelay:     defaultRetryMaxDelay,
		sleeper:           realSleeper{},
		rand:              rand.Float64,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	var dedup *lruDedup
	if cfg.idempotencyCache > 0 {
		dedup = newLRUDedup(cfg.idempotencyCache)
	}
	return &Consumer{
		sub:    sub,
		bus:    bus,
		cfg:    cfg,
		dedup:  dedup,
		doneCh: make(chan struct{}),
	}
}

// Start begins the receive→publish loop on a background goroutine. The
// supplied `ctx` parents every per-event publish; cancelling `ctx`
// stops the consumer (the loop exits cleanly and [Consumer.Stop]
// becomes redundant but safe).
//
// Idempotent in the strict sense: a second call returns
// [ErrAlreadyStarted] without spawning a duplicate runner goroutine.
// After Stop, Start cannot resurrect the consumer — the state machine
// is linear: not-started → started → stopped.
//
// Returns synchronously after the loop goroutine is launched. The
// initial [LocalSubscriber.Subscribe] call happens INSIDE the
// goroutine, not inline; a failure there is logged via [Logger] and
// surfaces on the next [Consumer.Stop] as the wrapped error.
//
//nolint:contextcheck // intentional: ctx is parented via context.WithCancel for the loop goroutine; same pattern as cron.Scheduler.Start.
func (c *Consumer) Start(ctx context.Context) error {
	c.mu.Lock()
	switch c.state {
	case stateStarted:
		c.mu.Unlock()
		return ErrAlreadyStarted
	case stateStopped:
		c.mu.Unlock()
		return ErrAlreadyStopped
	case stateNotStarted:
		// fallthrough to start
	}
	if ctx == nil {
		ctx = context.Background()
	}
	loopCtx, cancel := context.WithCancel(ctx)
	c.cancel = cancel
	c.state = stateStarted
	c.mu.Unlock()

	go c.run(loopCtx)
	return nil
}

// Stop signals the receive loop to exit and blocks until the goroutine
// has returned. Idempotent: a second call returns the same result
// without re-running the shutdown sequence.
//
// Returns nil when the loop exited cleanly (ctx cancellation or stream
// EOF on a Stop-initiated cancel). Surfaces the underlying transport
// error when the loop exited because of an unrecoverable subscribe
// failure (e.g. [keepclient.ErrNoTokenSource], 401 on first connect).
//
// Stop on a never-Started consumer returns [ErrNotStarted].
func (c *Consumer) Stop() error {
	c.mu.Lock()
	state := c.state
	cancel := c.cancel
	doneCh := c.doneCh
	c.state = stateStopped
	c.mu.Unlock()

	if state == stateNotStarted {
		return ErrNotStarted
	}
	c.stopOnce.Do(func() {
		if cancel != nil {
			cancel()
		}
		<-doneCh
	})
	return c.stopErr
}

// run is the receive→publish loop. It opens a stream via the
// configured [LocalSubscriber] and reads events until ctx cancellation
// or unrecoverable error. Per-event publish failures are retried with
// bounded backoff; an exhausted retry budget logs and DROPS the event
// (no DLQ in Phase 1).
func (c *Consumer) run(ctx context.Context) {
	defer close(c.doneCh)

	stream, err := c.sub.Subscribe(ctx)
	if err != nil {
		c.log(
			ctx, "outbox: subscribe failed",
			"err_type", fmt.Sprintf("%T", err),
		)
		c.stopErr = fmt.Errorf("outbox: subscribe: %w", err)
		return
	}
	defer func() { _ = stream.Close() }()

	for {
		ev, nextErr := stream.Next(ctx)
		if nextErr != nil {
			// Context cancellation is a clean shutdown — return nil.
			// keepclient already wraps ctx.Err() into the returned
			// error so errors.Is matches.
			if errors.Is(nextErr, context.Canceled) || errors.Is(nextErr, context.DeadlineExceeded) {
				return
			}
			// ErrStreamClosed surfaces when the caller closed the
			// stream — also a clean shutdown.
			if errors.Is(nextErr, keepclient.ErrStreamClosed) {
				return
			}
			c.log(
				ctx, "outbox: stream error",
				"err_type", fmt.Sprintf("%T", nextErr),
			)
			c.stopErr = fmt.Errorf("outbox: stream: %w", nextErr)
			return
		}

		c.handleEvent(ctx, ev)
	}
}

// handleEvent is the per-event hot path: dedup → publish-with-retry →
// record-dedup. The order matters: we record the id ONLY on a
// successful publish so an exhausted-retry event can be re-attempted on
// a future redelivery (subject to the bus subscriber's own
// idempotency).
func (c *Consumer) handleEvent(ctx context.Context, ev keepclient.Event) {
	// Empty event_type is a server-side bug; the bus rejects it
	// anyway. Log and drop.
	if ev.EventType == "" {
		c.log(
			ctx, "outbox: malformed event (empty event_type)",
			"event_id", ev.ID,
		)
		return
	}

	// Dedup pre-check: if we've seen this id within the LRU window,
	// drop silently. Empty ids are NEVER deduplicated — the SSE spec
	// allows missing `id:` fields and an aggressive dedup would
	// collapse them all into one (mirrors keepclient lruDedup
	// contract).
	if c.dedup != nil && c.dedup.seen(ev.ID) {
		return
	}

	delivered := DeliveredEvent{
		ID:        ev.ID,
		EventType: ev.EventType,
		Payload:   ev.Payload,
	}

	if err := c.publishWithRetry(ctx, delivered); err != nil {
		// Publish exhausted or ctx cancelled mid-publish. Either way
		// we do NOT record the dedup entry — a later redelivery is
		// allowed to retry. The log entry distinguishes the two.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return
		}
		c.log(
			ctx, "outbox: publish exhausted",
			"event_id", ev.ID,
			"event_type", ev.EventType,
			"err_type", fmt.Sprintf("%T", err),
		)
		return
	}

	if c.dedup != nil {
		c.dedup.record(ev.ID)
	}
}

// publishWithRetry attempts the bus publish up to maxPublishRetries
// times with exponential-backoff-with-jitter between attempts. Each
// attempt runs under a per-attempt context derived from ctx with the
// configured publish timeout. Returns nil on success, ctx.Err() on
// caller-side cancellation, or a wrapped [ErrPublishExhausted] when
// the budget is exhausted.
func (c *Consumer) publishWithRetry(ctx context.Context, ev DeliveredEvent) error {
	var lastErr error
	for attempt := 0; attempt < c.cfg.maxPublishRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if attempt > 0 {
			delay := backoffFor(attempt-1, c.cfg.retryInitialDelay, c.cfg.retryMaxDelay, c.cfg.rand)
			if err := c.cfg.sleeper.Sleep(ctx, delay); err != nil {
				return err
			}
		}

		attemptCtx, cancel := context.WithTimeout(ctx, c.cfg.publishTimeout)
		err := c.bus.Publish(attemptCtx, ev.EventType, ev)
		cancel()
		if err == nil {
			return nil
		}
		// Caller ctx cancelled mid-publish surfaces as ctx.Err() on
		// the parent — treat as terminal.
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = err
		c.log(
			ctx, "outbox: publish attempt failed",
			"event_id", ev.ID,
			"event_type", ev.EventType,
			"attempt", attempt+1,
			"err_type", fmt.Sprintf("%T", err),
		)
	}
	return fmt.Errorf("%w: %w", ErrPublishExhausted, lastErr)
}

// log forwards a diagnostic message to the optional [Logger]. Nil-logger
// safe: a Consumer constructed without [WithLogger] silently drops.
func (c *Consumer) log(ctx context.Context, msg string, kv ...any) {
	if c.cfg.logger == nil {
		return
	}
	c.cfg.logger.Log(ctx, msg, kv...)
}

// sleeper is the seam the retry loop uses to wait between attempts.
// Production code uses [realSleeper] (which honours ctx cancellation);
// unit tests inject a fake that records the requested durations and
// returns immediately.
type sleeper interface {
	Sleep(ctx context.Context, d time.Duration) error
}

// realSleeper is the production sleeper. It selects on ctx.Done() and
// a time.After(d) channel so a cancelled context unblocks the sleep
// promptly.
type realSleeper struct{}

// Sleep blocks until d elapses or ctx is cancelled. A cancelled ctx
// returns ctx.Err() immediately.
func (realSleeper) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// backoffFor computes the delay for retry `n` (zero-based) with
// exponential growth `initial * 2^n`, clamped at maxDelay, and ±25%
// jitter sourced from randFn. Mirrors the keepclient resilient-stream
// model but lives here to avoid an export-just-for-this-call surface
// on keepclient. randFn must return a value in [0, 1); the realised
// delay is therefore in `[base * 0.75, base * 1.25]`.
func backoffFor(attempt int, initial, maxDelay time.Duration, randFn func() float64) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	base := initial
	for i := 0; i < attempt; i++ {
		next := base * 2
		if next < base || next > maxDelay {
			base = maxDelay
			break
		}
		base = next
	}
	if base > maxDelay {
		base = maxDelay
	}
	if randFn == nil {
		return base
	}
	span := float64(base) * retryJitterFraction
	offset := (randFn()*2 - 1) * span
	jittered := time.Duration(float64(base) + offset)
	if jittered < 0 {
		jittered = 0
	}
	return jittered
}

// lruDedup is a tiny fixed-size last-N seen-IDs cache. The consumer
// runs the receive→publish loop on a single goroutine, so the cache is
// touched by exactly one writer; no locking required. (The `sync`
// fields here are reserved for a future multi-worker variant; current
// code paths never take the lock.)
type lruDedup struct {
	mu    sync.Mutex
	size  int
	order []string
	set   map[string]struct{}
}

// newLRUDedup constructs an empty dedup cache with capacity size.
// `size` must be > 0; callers (config.idempotencyCache) clamp this.
func newLRUDedup(size int) *lruDedup {
	return &lruDedup{
		size:  size,
		order: make([]string, 0, size),
		set:   make(map[string]struct{}, size),
	}
}

// seen reports whether id is currently in the cache. Empty ids are
// never reported as seen (the SSE spec allows missing `id:` fields
// and the consumer must not collapse them all into one).
func (l *lruDedup) seen(id string) bool {
	if id == "" {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.set[id]
	return ok
}

// record adds id to the cache, evicting the oldest entry on overflow.
// Empty ids are never recorded.
func (l *lruDedup) record(id string) {
	if id == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.set[id]; ok {
		return
	}
	if len(l.order) == l.size {
		evict := l.order[0]
		l.order = l.order[1:]
		delete(l.set, evict)
	}
	l.order = append(l.order, id)
	l.set[id] = struct{}{}
}
