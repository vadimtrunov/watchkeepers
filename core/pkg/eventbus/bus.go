package eventbus

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
)

// defaultTopicBufferSize is the per-topic buffered-channel capacity used when
// no [WithTopicBufferSize] option is supplied. Sized for in-process bursts
// (a busy lifecycle event) without holding so many events that a stalled
// handler eats an unbounded amount of memory.
const defaultTopicBufferSize = 64

// Handler is the callback invoked by a topic worker for every event
// published to that topic. The bus dispatches handlers sequentially within
// a topic in registration order; a slow handler stalls only its own topic.
//
// Handlers MUST NOT panic. A panicking handler will crash the topic's
// worker goroutine and silently drop subsequent events on that topic.
// Recovery / panic isolation is deferred (see README "Future extensions").
//
// The `ctx` argument is the same context passed to [Bus.Publish]; handlers
// SHOULD honour `ctx.Done()` for any blocking I/O they perform.
type Handler = func(ctx context.Context, event any)

// Option configures a [Bus] at construction time. Pass options to [New];
// later options override earlier ones for the same field.
type Option func(*config)

// config is the internal mutable bag the [Option] callbacks populate. Held
// in a separate type so [Bus] itself stays immutable after [New] returns.
type config struct {
	bufferSize int
}

// WithTopicBufferSize sets the buffered capacity of every per-topic queue
// channel. The default is 64. A non-positive `n` is treated as a programmer
// error and falls back to the default — a zero-buffer queue would force
// every Publish to synchronise with the worker, defeating the asynchronous
// fan-out model.
func WithTopicBufferSize(n int) Option {
	return func(c *config) {
		if n > 0 {
			c.bufferSize = n
		}
	}
}

// envelope wraps a single Publish call so the topic worker can dispatch
// with the publisher's context (used by Handler implementations to honour
// cancellation downstream of the Publish call site).
type envelope struct {
	ctx   context.Context
	event any
}

// handlerEntry pairs a registered Handler with a process-unique id so the
// returned `unsubscribe` callback can remove the entry without comparing
// function pointers (which Go forbids for `==` on `func` types).
type handlerEntry struct {
	id      uint64
	handler Handler
}

// topicState owns the per-topic worker goroutine, its bounded queue
// channel, and the snapshot-style subscriber list. The list is replaced
// (copy-on-write) on Subscribe / unsubscribe so the worker goroutine
// holds a stable slice for the duration of any one event dispatch — a
// new subscriber added mid-dispatch does NOT retroactively receive the
// event currently in flight (AC4).
type topicState struct {
	ch chan envelope

	mu   sync.RWMutex
	subs []handlerEntry
}

// Bus is the in-process pub/sub event bus. Construct via [New]; the zero
// value is not usable. A Bus is safe for concurrent use across goroutines
// once constructed and remains usable until [Bus.Close] returns; after
// Close every Publish/Subscribe call returns [ErrClosed].
//
// See package godoc and `core/pkg/eventbus/README.md` for the ordering,
// backpressure, and shutdown contracts.
type Bus struct {
	cfg config

	closed    atomic.Bool
	closeOnce sync.Once
	done      chan struct{} // closed by Close to wake blocked Publishes

	mu     sync.Mutex // guards `topics`
	topics map[string]*topicState

	// publishWG counts in-flight Publish calls. Close waits for it to
	// drain before closing the per-topic channels so a Publish that has
	// already selected `ts.ch <-` cannot race a `close(ts.ch)`.
	publishWG sync.WaitGroup

	wg sync.WaitGroup // tracks per-topic worker goroutines

	nextSubID atomic.Uint64
}

// New constructs a [Bus] with the supplied options applied. The default
// per-topic buffer is 64 envelopes; override with [WithTopicBufferSize].
//
// The returned bus has no topics until the first Subscribe / Publish call
// touches one — topic workers are spawned lazily so a Bus with zero
// subscribers leaks zero goroutines.
func New(opts ...Option) *Bus {
	cfg := config{bufferSize: defaultTopicBufferSize}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &Bus{
		cfg:    cfg,
		done:   make(chan struct{}),
		topics: make(map[string]*topicState),
	}
}

// getOrCreateTopic returns the [topicState] for `topic`, lazily spawning
// the worker goroutine on first use. Returns nil if the bus has been
// closed; callers map a nil return to [ErrClosed].
//
// The implementation takes `b.mu` briefly to (a) re-check `b.closed` under
// the same lock Close acquires when snapshotting per-topic channels —
// closing this TOCTOU window is what prevents a leaked worker on a
// never-closed channel — (b) double-check the topic map, (c) install a
// fresh [topicState] on miss, and (d) spawn the worker goroutine before
// releasing the lock. Workers register themselves in `b.wg` so [Bus.Close]
// can wait for them to drain.
func (b *Bus) getOrCreateTopic(topic string) *topicState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ts, ok := b.topics[topic]; ok {
		return ts
	}
	// Re-check under `b.mu` so we cannot install a new topicState (and
	// spawn its worker) after Close has already snapshotted the topics
	// map and proceeded to close per-topic channels. Close acquires the
	// SAME `b.mu` for its snapshot, so either:
	//   - Close has not yet snapshotted: re-check sees closed=false,
	//     install proceeds, Close blocks on b.mu, sees the new entry in
	//     its snapshot, closes its channel, worker exits cleanly.
	//   - Close has finished its snapshot: closed=true was stored before
	//     Close ever took b.mu, the re-check sees it, returns nil,
	//     no install, no leak.
	if b.closed.Load() {
		return nil
	}
	ts := &topicState{
		ch: make(chan envelope, b.cfg.bufferSize),
	}
	b.topics[topic] = ts
	b.wg.Add(1)
	go b.runTopic(ts)
	return ts
}

// runTopic is the per-topic worker loop. It blocks on the topic's channel
// and dispatches each envelope to a snapshot of the subscriber list
// sequentially in registration order. The loop exits when the channel is
// closed by [Bus.Close] (after which range drains any buffered envelopes
// and then returns).
//
// The subscriber-list snapshot is taken at DISPATCH time (here), NOT at
// enqueue time (in Publish). Consequence for late subscribers: a Subscribe
// that returns AFTER an envelope was enqueued by Publish but BEFORE this
// loop pops + dispatches it WILL observe that envelope. The guarantee is
// therefore "no retroactive delivery of events whose dispatch has already
// begun," not the stronger "no delivery of events whose Publish completed
// before Subscribe returned." See [Bus.Subscribe] godoc.
func (b *Bus) runTopic(ts *topicState) {
	defer b.wg.Done()
	for env := range ts.ch {
		ts.mu.RLock()
		// Snapshot the slice header — the slice itself is replaced
		// copy-on-write by Subscribe / unsubscribe, so the captured
		// reference remains safe for the duration of this dispatch even
		// if a concurrent Subscribe swaps the field underneath.
		subs := ts.subs
		ts.mu.RUnlock()
		for _, h := range subs {
			h.handler(env.ctx, env.event)
		}
	}
}

// Subscribe registers `handler` for `topic` and returns a single-shot
// `unsubscribe` callback. Subsequent Publish calls to that topic will
// invoke the handler in registration order relative to other subscribers.
//
// Calling `unsubscribe` is idempotent: the first call removes the handler;
// subsequent calls are no-ops (no panic, no error). The returned function
// is safe to call from any goroutine.
//
// Late-subscriber semantics (AC4): the bus snapshots the subscriber list
// at DISPATCH time, not enqueue time. A subscriber added between an event's
// Publish (which only enqueues) and the worker's dispatch of that event
// WILL receive the event. The guarantee Subscribe provides is therefore
// the weaker one: a subscriber does NOT retroactively receive an event
// whose dispatch has already begun, but events still queued behind earlier
// envelopes are visible to it. Callers needing strict
// "events published strictly AFTER my Subscribe returns" semantics must
// quiesce publishers themselves.
//
// Returns [ErrClosed] if the bus has been [Bus.Close]d, [ErrInvalidTopic]
// if `topic` is the empty string, [ErrInvalidHandler] if `handler` is nil.
// On any of those errors the returned unsubscribe is a no-op closure so
// callers can `defer unsubscribe()` without an explicit nil check.
func (b *Bus) Subscribe(topic string, handler Handler) (func(), error) {
	noop := func() {}
	if topic == "" {
		return noop, ErrInvalidTopic
	}
	if handler == nil {
		return noop, ErrInvalidHandler
	}
	if b.closed.Load() {
		return noop, ErrClosed
	}

	// Hold b.mu across the closed-check, topic install/lookup, AND the
	// ts.mu.Lock acquisition. This serialises Subscribe-against-Close for
	// BOTH new and existing topics: without locking ts.mu before releasing
	// b.mu, a Subscribe on an existing topic could pass the closed-check,
	// release b.mu, then race a Close that snapshots channels, drains
	// publishWG, closes channels, and waits for workers to exit — the
	// resulting subscription would land on a dead topic with the worker
	// already gone (a silent contract break, not just a leak).
	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()
		return noop, ErrClosed
	}
	ts, ok := b.topics[topic]
	if !ok {
		ts = &topicState{
			ch: make(chan envelope, b.cfg.bufferSize),
		}
		b.topics[topic] = ts
		b.wg.Add(1)
		go b.runTopic(ts)
	}
	ts.mu.Lock()
	// Release b.mu only after ts.mu is held — Close cannot snapshot+close
	// this topic's channel while ts.mu is held by Subscribe because Close
	// itself takes b.mu first; the b.mu→ts.mu nesting here is one-way (no
	// AB/BA cycle: ts.mu is never taken before b.mu anywhere else).
	b.mu.Unlock()

	id := b.nextSubID.Add(1)
	// Copy-on-write append: a fresh slice replaces the field so any
	// in-flight runTopic dispatch holding the previous header keeps
	// iterating its snapshot without seeing the new entry (AC4).
	next := make([]handlerEntry, len(ts.subs)+1)
	copy(next, ts.subs)
	next[len(ts.subs)] = handlerEntry{id: id, handler: handler}
	ts.subs = next
	ts.mu.Unlock()

	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			ts.mu.Lock()
			defer ts.mu.Unlock()
			// Copy-on-write delete: skip the entry whose id matches.
			// `nextSubID` is a process-monotonic counter so id collisions
			// are impossible within one Bus lifetime.
			filtered := make([]handlerEntry, 0, len(ts.subs))
			for _, h := range ts.subs {
				if h.id != id {
					filtered = append(filtered, h)
				}
			}
			ts.subs = filtered
		})
	}
	return unsubscribe, nil
}

// Publish enqueues `event` for delivery to every subscriber of `topic`.
// Delivery is asynchronous: the call returns once the event is in the
// topic's queue (or the queue accepts it after backpressure clears).
//
// Backpressure: when the topic's buffered queue is full, Publish blocks
// until either (a) a worker drains a slot, (b) `ctx` is cancelled, or
// (c) [Bus.Close] is called. On cancellation the call returns
// `fmt.Errorf("eventbus: publish: %w", ctx.Err())` so callers can
// `errors.Is(err, context.Canceled)` / `context.DeadlineExceeded`. On
// concurrent Close the call returns [ErrClosed]. No silent drop ever
// occurs.
//
// Returns [ErrClosed] if the bus has been [Bus.Close]d, [ErrInvalidTopic]
// if `topic` is the empty string. Publishing to a topic with zero
// subscribers is not an error: the event is enqueued, the worker pops it
// and dispatches to the empty subscriber list, no-op.
func (b *Bus) Publish(ctx context.Context, topic string, event any) error {
	if topic == "" {
		return ErrInvalidTopic
	}
	// Fast-path closed check avoids touching b.mu in the hot path when the
	// bus is already shut down.
	if b.closed.Load() {
		return ErrClosed
	}

	// Register this in-flight Publish under b.mu, atomic with a re-check of
	// the closed flag. Close acquires the same b.mu before its
	// publishWG.Wait, so a 0→1 transition of publishWG can never race with
	// the first Wait — eliminating a sync.WaitGroup misuse the race detector
	// flags as `race.Read(&wg.sema)` (Add) vs `race.Write(&wg.sema)` (Wait).
	//
	// Holding publishWG across the entire send below also prevents Close
	// from closing the per-topic channel from underneath an in-flight send;
	// Close.publishWG.Wait drains in-flight Publishes BEFORE close(ts.ch).
	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()
		return ErrClosed
	}
	b.publishWG.Add(1)
	b.mu.Unlock()
	defer b.publishWG.Done()

	ts := b.getOrCreateTopic(topic)
	// Close-race fence: getOrCreateTopic returns nil if Close ran between
	// the fast-path closed-flag check above and the under-lock re-check
	// inside the helper. Without this, a first-touch Publish on an unseen
	// topic could spawn a fresh worker on a never-closed channel after
	// Close had already snapshotted the topic set, leaking the goroutine.
	if ts == nil {
		return ErrClosed
	}

	// Fast path: try a non-blocking send. The non-blocking send avoids
	// burning the slow-path select for the common case where the queue
	// has spare capacity (the overwhelming majority of publishes in a
	// healthy system).
	select {
	case ts.ch <- envelope{ctx: ctx, event: event}:
		return nil
	default:
	}

	// Slow path: queue is full. Block on the send while also honouring
	// caller-side context cancellation and bus-side Close. Both ctx.Done
	// and b.done unblock the publisher with the appropriate error so the
	// caller never blocks indefinitely on a stalled handler.
	select {
	case ts.ch <- envelope{ctx: ctx, event: event}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("eventbus: publish: %w", ctx.Err())
	case <-b.done:
		return ErrClosed
	}
}

// Close stops the bus. Subsequent Publish/Subscribe calls return
// [ErrClosed]. All in-flight events already in any topic's buffered queue
// are drained: each topic worker finishes dispatching its remaining
// envelopes to the current subscriber set before exiting. Close blocks
// until every worker goroutine has returned and then returns nil.
//
// Close is idempotent: subsequent calls return nil without rerunning the
// drain. The first Close does the work; later Close calls observe the
// `closeOnce` guard and short-circuit.
//
// Close DOES wake publishers blocked on backpressure (they receive
// [ErrClosed] and return without enqueueing). The implementation flips
// `closed` to true BEFORE closing the per-topic channels and waits for
// in-flight Publish goroutines to finish their send-or-bail select before
// closing the channels, so a `send on closed channel` panic is impossible
// by construction.
func (b *Bus) Close() error {
	b.closeOnce.Do(func() {
		// 1. Mark closed and snapshot the per-topic channel set under
		//    b.mu. Holding b.mu here serialises against Publish's
		//    register-under-mu step (and getOrCreateTopic), so a Publish
		//    that observes closed=false while holding b.mu has already
		//    incremented publishWG before Close's publishWG.Wait runs —
		//    eliminating the WaitGroup-Add-races-Wait misuse that the
		//    race detector flags on `wg.sema`. Conversely, any Publish
		//    that takes b.mu after Close releases it sees closed=true
		//    and bails before touching publishWG.
		b.mu.Lock()
		b.closed.Store(true)
		channels := make([]chan envelope, 0, len(b.topics))
		for _, ts := range b.topics {
			channels = append(channels, ts.ch)
		}
		b.mu.Unlock()

		// 2. Wake any publishers blocked on the backpressure select. They
		//    return ErrClosed and decrement publishWG.
		close(b.done)

		// 3. Wait for all in-flight Publish goroutines to finish. This
		//    barrier guarantees no Publish is mid-`ts.ch <-` when we
		//    close the channels below; otherwise we would risk a panic
		//    on send-to-closed-channel.
		b.publishWG.Wait()

		// 4. Close each per-topic channel. Worker goroutines exit their
		//    `range` loop once their channel is closed and any remaining
		//    buffered envelopes have been drained.
		for _, ch := range channels {
			close(ch)
		}

		// 5. Wait for all worker goroutines to exit. The combination of
		//    publishWG.Wait above and wg.Wait here means Close returns
		//    only after every event in every queue has been dispatched
		//    AND every worker has cleanly returned (AC5 / no leak).
		b.wg.Wait()
	})
	return nil
}
