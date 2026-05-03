package keepclient

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"
)

// Default tuning for the resilient reconnect loop. Production callers can
// override every value via the corresponding ResilientOption.
const (
	defaultReconnectInitialDelay = 100 * time.Millisecond
	defaultReconnectMaxDelay     = 30 * time.Second
	defaultMaxReconnectAttempts  = 5
	// reconnectJitterFraction bounds the additive jitter applied to each
	// backoff sleep at ±25% of the un-jittered delay.
	reconnectJitterFraction = 0.25
)

// ResilientOption configures a [Client.SubscribeResilient] call.
//
// Pass options as variadic arguments after the context. Later options
// override earlier ones for the same field; the dedup options (`WithDedup`
// and `WithDedupLRU`) are mutually exclusive — last option wins.
type ResilientOption func(*resilientConfig)

// resilientConfig is the fully-resolved per-call configuration produced by
// applying every ResilientOption to a defaults-populated value. Held inside
// [ResilientStream] for the lifetime of the call.
type resilientConfig struct {
	initialDelay time.Duration
	maxDelay     time.Duration
	maxAttempts  int

	// dedup, when non-nil, is invoked for every freshly-received Event.ID
	// AFTER the resilient layer recorded the id but BEFORE delivery. A
	// return value of `true` causes the event to be silently skipped.
	dedup func(id string) bool

	// sleeper is the injection point that lets unit tests substitute a
	// fake clock. Production code uses [realSleeper]; tests pass a
	// [fakeSleeper] that records the requested durations.
	sleeper sleeper

	// rand is the (optional) jitter source. Tests may set a deterministic
	// implementation; production callers leave it at the default which
	// uses [math/rand/v2].
	rand func() float64
}

// WithReconnectInitialDelay overrides the first-attempt backoff after a
// transport error or clean EOF. Must be > 0; non-positive values fall back
// to the default of 100ms.
func WithReconnectInitialDelay(d time.Duration) ResilientOption {
	return func(c *resilientConfig) {
		if d > 0 {
			c.initialDelay = d
		}
	}
}

// WithReconnectMaxDelay caps the per-attempt backoff. Must be > 0;
// non-positive values fall back to the default of 30s. The cap is applied
// BEFORE jitter so the realised sleep can briefly exceed maxDelay by up to
// ±25%.
func WithReconnectMaxDelay(d time.Duration) ResilientOption {
	return func(c *resilientConfig) {
		if d > 0 {
			c.maxDelay = d
		}
	}
}

// WithMaxReconnectAttempts sets the maximum number of consecutive reconnect
// attempts before [ResilientStream.Next] surfaces [ErrReconnectExhausted].
// Must be > 0; non-positive values fall back to the default of 5.
func WithMaxReconnectAttempts(n int) ResilientOption {
	return func(c *resilientConfig) {
		if n > 0 {
			c.maxAttempts = n
		}
	}
}

// WithDedup registers a caller-supplied dedup predicate. predicate receives
// the [Event.ID] of every freshly-decoded event and returns `true` when
// the event should be SKIPPED (i.e. the caller has already observed it).
//
// The hook is consulted AFTER the resilient layer has recorded the event
// in its internal "last seen" pointer (so the next reconnect still sends
// the correct `Last-Event-ID`) but BEFORE the event is returned to the
// caller. Mutually exclusive with [WithDedupLRU] — last option wins.
func WithDedup(predicate func(id string) bool) ResilientOption {
	return func(c *resilientConfig) {
		c.dedup = predicate
	}
}

// WithDedupLRU enables a built-in last-N LRU dedup. The resilient layer
// records every delivered event id; on a duplicate id within the last
// `size` deliveries the event is skipped silently.
//
// `size` must be > 0; non-positive values disable the LRU (use the
// caller-supplied [WithDedup] hook instead). Mutually exclusive with
// [WithDedup] — last option wins.
//
// The dedup window covers DELIVERED events only — events that the caller's
// predicate skipped (or that this LRU itself skipped) are not recorded.
// This avoids the "I saw it once, skipped, then it arrived again because
// the LRU never recorded it" pathology.
func WithDedupLRU(size int) ResilientOption {
	return func(c *resilientConfig) {
		if size <= 0 {
			c.dedup = nil
			return
		}
		c.dedup = newLRUDedup(size).seenAndRecord
	}
}

// sleeper is the seam the resilient loop uses to wait between reconnect
// attempts. Production code uses [realSleeper] (which honours ctx
// cancellation); unit tests inject a fake that records the requested
// durations and returns immediately.
type sleeper interface {
	// Sleep blocks for d, returning ctx.Err() if ctx is cancelled before
	// d elapses. A nil return means the full duration was slept.
	Sleep(ctx context.Context, d time.Duration) error
}

// realSleeper is the production sleeper. It selects on ctx.Done() and a
// time.After(d) channel so a cancelled context unblocks the sleep
// promptly without leaking the underlying timer goroutine on Go 1.23+.
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

// withSleeperOption is the unexported test seam that lets the resilient
// test file inject a fake clock without exporting a public option.
func withSleeperOption(s sleeper) ResilientOption {
	return func(c *resilientConfig) {
		if s != nil {
			c.sleeper = s
		}
	}
}

// withRandOption is the unexported test seam that lets the resilient
// test file substitute a deterministic jitter source.
func withRandOption(fn func() float64) ResilientOption {
	return func(c *resilientConfig) {
		if fn != nil {
			c.rand = fn
		}
	}
}

// ResilientStream is the iterator returned by [Client.SubscribeResilient].
// It mirrors the [Stream] surface (`Next`, `Close`) and adds transparent
// reconnect-with-backoff, [Last-Event-ID] forwarding, and an optional
// dedup hook on top of the underlying single-shot [Stream].
//
// ResilientStream is NOT safe for concurrent calls into [ResilientStream.Next];
// callers must serialise reads. [ResilientStream.Close] is safe from any
// goroutine and is idempotent.
type ResilientStream struct {
	c   *Client
	cfg resilientConfig

	// loopCtx is the context the reconnect loop honours. It is derived
	// from the caller's first ctx via context.WithCancel so [Close] can
	// abort an in-flight backoff sleep promptly even if the caller's
	// per-call ctx has not been cancelled yet.
	loopCtx    context.Context
	loopCancel context.CancelFunc

	// Stream-level state mutated only on the single goroutine that owns
	// Next; protected against concurrent Close via mu.
	stream *Stream
	lastID string

	// mu guards closed and stream against a Close call racing with Next.
	mu     sync.Mutex
	closed bool
}

// SubscribeResilient opens a [ResilientStream] that wraps the underlying
// [Client.Subscribe] primitive with reconnect/backoff/dedup behaviour. The
// returned stream's [ResilientStream.Next] keeps re-opening the inner
// stream on transport-level failures up to [WithMaxReconnectAttempts]
// times before surfacing [ErrReconnectExhausted].
//
// Calling without [WithTokenSource] returns [ErrNoTokenSource]
// synchronously, before any network round-trip — same contract as
// [Client.Subscribe].
//
// Initial open: SubscribeResilient performs the FIRST [Subscribe] call
// synchronously and returns its error verbatim — a 401 on the very first
// open surfaces as a [*ServerError] (whose Unwrap chain matches the Err*
// sentinels), without entering the reconnect loop. Reconnect only kicks
// in for transport-level failures observed AFTER the first open succeeds.
//
// Server-side caveat (forward-compat): the current Keep server (M2.7.e)
// does NOT honor the `Last-Event-ID` request header — it always streams
// from the moment the new subscription is registered. The client still
// sends the header on every reconnect so a future server-side replay
// implementation lights up automatically; until then, callers should use
// [WithDedup] or [WithDedupLRU] to suppress duplicate IDs that the server
// re-emits across the reconnect window.
func (c *Client) SubscribeResilient(ctx context.Context, opts ...ResilientOption) (*ResilientStream, error) {
	if c.cfg.tokenSource == nil {
		return nil, ErrNoTokenSource
	}

	cfg := resilientConfig{
		initialDelay: defaultReconnectInitialDelay,
		maxDelay:     defaultReconnectMaxDelay,
		maxAttempts:  defaultMaxReconnectAttempts,
		sleeper:      realSleeper{},
		rand:         rand.Float64,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	loopCtx, cancel := context.WithCancel(ctx)
	stream, err := c.subscribeWithLastEventID(ctx, "")
	if err != nil {
		cancel()
		return nil, err
	}

	return &ResilientStream{
		c:          c,
		cfg:        cfg,
		loopCtx:    loopCtx,
		loopCancel: cancel,
		stream:     stream,
	}, nil
}

// Next returns the next [Event] from the stream, transparently reconnecting
// on transport errors or clean [io.EOF] from the inner [Stream]. Server-side
// errors (a [*ServerError] returned from a reconnect open) bubble up to the
// caller — the resilient layer assumes a server status response means the
// auth contract or scope is broken and must not be retried.
//
// Returns:
//   - The decoded [Event] on success (after the dedup hook, if configured,
//     decided to deliver it);
//   - [ErrReconnectExhausted] (wrapping the last transport error) once the
//     loop has burned [WithMaxReconnectAttempts] consecutive reconnect
//     failures;
//   - [ErrStreamClosed] when the caller has called [ResilientStream.Close];
//   - An error wrapping [context.Canceled] / [context.DeadlineExceeded]
//     when the supplied ctx is cancelled (including mid-backoff).
//
// Next is NOT safe for concurrent invocation; callers must serialise reads.
func (s *ResilientStream) Next(ctx context.Context) (Event, error) {
	for {
		if err := ctx.Err(); err != nil {
			return Event{}, fmt.Errorf("keepclient: subscribe resilient: %w", err)
		}

		s.mu.Lock()
		closed := s.closed
		stream := s.stream
		s.mu.Unlock()
		if closed {
			return Event{}, ErrStreamClosed
		}
		if stream == nil {
			// Defensive: SubscribeResilient guarantees a non-nil stream
			// on construction, and reconnect either replaces stream or
			// returns ErrReconnectExhausted. If we ever observe a nil
			// stream here it's a programmer error in this file.
			return Event{}, errors.New("keepclient: resilient stream has no inner stream")
		}

		ev, err := stream.Next(ctx)
		if err == nil {
			s.lastID = ev.ID
			if s.cfg.dedup != nil && s.cfg.dedup(ev.ID) {
				continue
			}
			return ev, nil
		}

		// Caller-side close races with the inner Next; surface the
		// closed sentinel rather than the underlying read-on-closed
		// transport error.
		s.mu.Lock()
		closed = s.closed
		s.mu.Unlock()
		if closed {
			return Event{}, ErrStreamClosed
		}

		// Context cancellation surfaces verbatim (Stream.Next already
		// wraps the ctx error itself).
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Event{}, err
		}

		// *ServerError on the wire (e.g. 401 mid-stream re-auth failure
		// returned by a reconnect open) breaks the auth contract — bubble
		// up immediately without retrying.
		var se *ServerError
		if errors.As(err, &se) {
			return Event{}, err
		}

		// Transport-level error or clean EOF -> reconnect with backoff.
		if reconnErr := s.reconnect(ctx, err); reconnErr != nil {
			return Event{}, reconnErr
		}
		// Loop and re-read from the freshly-opened stream.
	}
}

// reconnect drives the bounded backoff loop. lastErr is the trigger that
// kicked off the reconnect — it carries forward into a wrapped
// [ErrReconnectExhausted] once the attempt budget is exhausted.
//
// The current inner stream is closed before the first sleep so its
// underlying TCP connection is released even if the caller never reaches
// a successful reconnect.
func (s *ResilientStream) reconnect(ctx context.Context, lastErr error) error {
	// Close the broken inner stream — ignore close errors (the body may
	// already be torn down by the transport on a connection reset).
	s.mu.Lock()
	if s.stream != nil {
		_ = s.stream.Close()
		s.stream = nil
	}
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrStreamClosed
	}

	for attempt := 0; attempt < s.cfg.maxAttempts; attempt++ {
		delay := backoffFor(attempt, s.cfg.initialDelay, s.cfg.maxDelay, s.cfg.rand)
		// Use the loop ctx so a Close() on a parked stream wakes the
		// sleep too; merge it with the caller's ctx via the standard
		// "first cancel wins" idiom.
		mergedCtx, mergedCancel := mergeCtx(ctx, s.loopCtx)
		if err := s.cfg.sleeper.Sleep(mergedCtx, delay); err != nil {
			mergedCancel()
			// ctx cancelled during backoff (caller-side or via Close).
			s.mu.Lock()
			closedNow := s.closed
			s.mu.Unlock()
			if closedNow {
				return ErrStreamClosed
			}
			return fmt.Errorf("keepclient: subscribe resilient: %w", err)
		}
		mergedCancel()

		newStream, openErr := s.c.subscribeWithLastEventID(ctx, s.lastID)
		if openErr == nil {
			s.mu.Lock()
			if s.closed {
				s.mu.Unlock()
				_ = newStream.Close()
				return ErrStreamClosed
			}
			s.stream = newStream
			s.mu.Unlock()
			return nil
		}

		// Bubble *ServerError on a reconnect open — same auth-contract
		// rule as [ResilientStream.Next].
		var se *ServerError
		if errors.As(openErr, &se) {
			return openErr
		}

		lastErr = openErr
	}

	return fmt.Errorf("%w: %w", ErrReconnectExhausted, lastErr)
}

// Close releases the underlying [Stream] and cancels the internal loop
// context so any in-flight backoff sleep aborts promptly. Close is
// idempotent; subsequent calls return the same close error.
//
// After Close, [ResilientStream.Next] returns [ErrStreamClosed] (NOT
// [io.EOF], to disambiguate from a clean server-side end-of-stream).
func (s *ResilientStream) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	stream := s.stream
	s.stream = nil
	s.mu.Unlock()

	s.loopCancel()
	if stream != nil {
		return stream.Close()
	}
	return nil
}

// backoffFor computes the delay for attempt `n` (zero-based) with
// exponential growth `initial * 2^n`, clamped at maxDelay, and ±25%
// jitter sourced from randFn. randFn must return a value in [0, 1); the
// realised delay is therefore in `[base * 0.75, base * 1.25]`.
func backoffFor(attempt int, initial, maxDelay time.Duration, randFn func() float64) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	// Compute initial * 2^attempt while guarding against overflow when
	// attempt is large (the cap below contains it but the shift itself
	// can wrap on a 64-bit duration after ~63 doublings).
	base := initial
	for i := 0; i < attempt; i++ {
		next := base * 2
		// Saturate at maxDelay if the doubling overflowed or exceeded
		// the cap; treat both as "use the cap".
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
	// Jitter span is ±reconnectJitterFraction of base; randFn returns
	// [0, 1) so we shift it into [-1, 1) before scaling.
	span := float64(base) * reconnectJitterFraction
	offset := (randFn()*2 - 1) * span
	jittered := time.Duration(float64(base) + offset)
	if jittered < 0 {
		jittered = 0
	}
	return jittered
}

// mergeCtx returns a context that is cancelled when either parent is. The
// returned cancel must always be invoked (defer) to release the goroutine
// that joins the two parents.
func mergeCtx(a, b context.Context) (context.Context, context.CancelFunc) {
	if a == b {
		return a, func() {}
	}
	merged, cancel := context.WithCancel(a)
	stop := make(chan struct{})
	go func() {
		select {
		case <-b.Done():
			cancel()
		case <-stop:
		}
	}()
	return merged, func() {
		close(stop)
		cancel()
	}
}

// lruDedup is a tiny fixed-size last-N seen-IDs cache. It is single-reader
// safe by virtue of Next being single-reader; no locking required.
type lruDedup struct {
	size  int
	order []string
	seen  map[string]struct{}
}

// newLRUDedup constructs an empty dedup cache with capacity size. size must
// be > 0; callers (WithDedupLRU) clamp this.
func newLRUDedup(size int) *lruDedup {
	return &lruDedup{
		size:  size,
		order: make([]string, 0, size),
		seen:  make(map[string]struct{}, size),
	}
}

// seenAndRecord returns true if id has been recorded in the last `size`
// calls (i.e. should be skipped); otherwise it records id and returns
// false. Empty ids are never deduped — the SSE spec allows missing `id:`
// fields and the resilient layer must not collapse them all into one.
func (l *lruDedup) seenAndRecord(id string) bool {
	if id == "" {
		return false
	}
	if _, ok := l.seen[id]; ok {
		return true
	}
	if len(l.order) == l.size {
		evict := l.order[0]
		l.order = l.order[1:]
		delete(l.seen, evict)
	}
	l.order = append(l.order, id)
	l.seen[id] = struct{}{}
	return false
}
