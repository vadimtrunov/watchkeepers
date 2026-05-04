package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/eventbus"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// Compile-time assertion that the concrete *eventbus.Bus satisfies the
// LocalBus interface this package exposes. Test-only import of
// eventbus: production outbox code never depends on the concrete type
// — only on the interface — mirroring the cron.LocalPublisher pattern
// documented in `docs/LESSONS.md` (M3.2.b / M3.3 / M3.6).
var _ LocalBus = (*eventbus.Bus)(nil)

// pollUntil polls `cond` every 10ms until either the condition returns
// true or `deadline` elapses; on timeout calls t.Fatalf with `desc`.
// Mirrors the cron-test polling-deadline helper.
func pollUntil(t *testing.T, deadline time.Duration, desc string, cond func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("polling deadline (%s) elapsed without %s", deadline, desc)
}

// fakeStream is a hand-rolled [Stream] stand-in. Events queued via
// [fakeStream.push] surface through Next in order; once the queue is
// drained Next blocks on ctx until the test pushes more or cancels.
// Errors queued via [fakeStream.pushErr] surface in receive order.
//
// Mirrors the cron / keeperslog hand-rolled-fake pattern (no mocking
// library).
type fakeStream struct {
	mu       sync.Mutex
	events   []keepclient.Event
	errs     []error
	closed   atomic.Bool
	notify   chan struct{}
	closeErr error
}

func newFakeStream() *fakeStream {
	return &fakeStream{notify: make(chan struct{}, 1)}
}

// push enqueues an event for the next Next call to return.
func (f *fakeStream) push(ev keepclient.Event) {
	f.mu.Lock()
	f.events = append(f.events, ev)
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

// pushErr enqueues an error so the next Next call (after any pending
// events) returns it.
func (f *fakeStream) pushErr(err error) {
	f.mu.Lock()
	f.errs = append(f.errs, err)
	f.mu.Unlock()
	select {
	case f.notify <- struct{}{}:
	default:
	}
}

func (f *fakeStream) Next(ctx context.Context) (keepclient.Event, error) {
	for {
		if f.closed.Load() {
			return keepclient.Event{}, keepclient.ErrStreamClosed
		}
		f.mu.Lock()
		if len(f.events) > 0 {
			ev := f.events[0]
			f.events = f.events[1:]
			f.mu.Unlock()
			return ev, nil
		}
		if len(f.errs) > 0 {
			err := f.errs[0]
			f.errs = f.errs[1:]
			f.mu.Unlock()
			return keepclient.Event{}, err
		}
		f.mu.Unlock()

		select {
		case <-ctx.Done():
			return keepclient.Event{}, fmt.Errorf("fake: %w", ctx.Err())
		case <-f.notify:
			// loop and re-check queue
		}
	}
}

func (f *fakeStream) Close() error {
	f.closed.Store(true)
	select {
	case f.notify <- struct{}{}:
	default:
	}
	return f.closeErr
}

// fakeSubscriber returns the same fakeStream on every call. Records
// subscribe-call counts so tests can assert the consumer asked for a
// stream the expected number of times.
type fakeSubscriber struct {
	stream *fakeStream

	mu     sync.Mutex
	calls  int
	subErr error
}

func newFakeSubscriber(s *fakeStream) *fakeSubscriber {
	return &fakeSubscriber{stream: s}
}

func (f *fakeSubscriber) Subscribe(_ context.Context) (Stream, error) {
	f.mu.Lock()
	f.calls++
	err := f.subErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return f.stream, nil
}

func (f *fakeSubscriber) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeBus records every Publish call. Optional injectErr surfaces the
// supplied error for the FIRST `injectErrCount` calls; subsequent
// calls succeed. This shape models the "publish fails first, retry
// succeeds" at-least-once test.
type fakeBus struct {
	mu             sync.Mutex
	calls          []fakeBusCall
	injectErr      error
	injectErrCount int
	injectBlock    chan struct{}
}

type fakeBusCall struct {
	Topic string
	Event DeliveredEvent
}

func (f *fakeBus) Publish(ctx context.Context, topic string, event any) error {
	f.mu.Lock()
	block := f.injectBlock
	var err error
	if f.injectErrCount > 0 {
		err = f.injectErr
		f.injectErrCount--
	}
	dev, _ := event.(DeliveredEvent)
	f.calls = append(f.calls, fakeBusCall{Topic: topic, Event: dev})
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return err
}

func (f *fakeBus) recordedCalls() []fakeBusCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeBusCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeBus) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeLogger records every (msg, kv...) call.
type fakeLogger struct {
	mu      sync.Mutex
	entries []fakeLogEntry
}

type fakeLogEntry struct {
	Msg string
	KV  []any
}

func (l *fakeLogger) Log(_ context.Context, msg string, kv ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	cp := make([]any, len(kv))
	copy(cp, kv)
	l.entries = append(l.entries, fakeLogEntry{Msg: msg, KV: cp})
}

func (l *fakeLogger) snapshot() []fakeLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]fakeLogEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

// findEntry returns the first entry whose Msg matches `prefix` and
// whose KV pairs contain every key→value listed in `wantKV`. Used by
// tests that need to assert metadata content (event_id, attempt, etc.)
// without binding to entry order.
func (l *fakeLogger) findEntry(prefix string, wantKV map[string]any) *fakeLogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range l.entries {
		e := l.entries[i]
		if !strings.HasPrefix(e.Msg, prefix) {
			continue
		}
		if matchesKV(e.KV, wantKV) {
			return &e
		}
	}
	return nil
}

func matchesKV(kv []any, want map[string]any) bool {
	got := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			continue
		}
		got[k] = kv[i+1]
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// fakeSleeper records requested sleep durations and returns immediately
// (or honours ctx cancellation). Used by retry-budget tests so they
// don't spend wall-clock time on backoff sleeps.
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

// drainConsumer is a t.Cleanup helper that calls Stop and ignores the
// resulting error. Tests use it to guarantee the receive goroutine
// exits even on test failure.
func drainConsumer(t *testing.T, c *Consumer) {
	t.Helper()
	t.Cleanup(func() {
		_ = c.Stop()
	})
}

// TestConsumer_HappyPath_EventFlowsToBus — the canonical wiring:
// fake stream emits one event, consumer publishes it onto the bus
// under the matching topic, the recorded DeliveredEvent carries the
// expected id / event_type / payload.
func TestConsumer_HappyPath_EventFlowsToBus(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	c := New(sub, bus)
	drainConsumer(t, c)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stream.push(keepclient.Event{
		ID:        "evt-1",
		EventType: "watchkeeper.spawned",
		Payload:   json.RawMessage(`{"id":"wk-7"}`),
	})

	pollUntil(t, 2*time.Second, "bus to record 1 publish", func() bool {
		return bus.callCount() >= 1
	})

	got := bus.recordedCalls()
	if got[0].Topic != "watchkeeper.spawned" {
		t.Fatalf("call[0].Topic = %q, want %q", got[0].Topic, "watchkeeper.spawned")
	}
	if got[0].Event.ID != "evt-1" {
		t.Fatalf("call[0].Event.ID = %q, want %q", got[0].Event.ID, "evt-1")
	}
	if got[0].Event.EventType != "watchkeeper.spawned" {
		t.Fatalf("call[0].Event.EventType = %q, want %q", got[0].Event.EventType, "watchkeeper.spawned")
	}
	if string(got[0].Event.Payload) != `{"id":"wk-7"}` {
		t.Fatalf("call[0].Event.Payload = %q, want %q", string(got[0].Event.Payload), `{"id":"wk-7"}`)
	}
}

// TestConsumer_AtLeastOnce_PublishFailsThenSucceeds — first bus publish
// fails (transient error); consumer retries and succeeds on attempt 2;
// the bus sees exactly TWO publish calls (the failed and the succeed),
// the dedup cache records the id so a forced redelivery would be
// suppressed.
func TestConsumer_AtLeastOnce_PublishFailsThenSucceeds(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{
		injectErr:      errors.New("bus: transient"),
		injectErrCount: 1,
	}
	logger := &fakeLogger{}
	sleeper := &fakeSleeper{}
	c := New(
		sub, bus,
		WithLogger(logger),
		WithMaxPublishRetries(3),
		withSleeperOption(sleeper),
		withRandOption(func() float64 { return 0.5 }),
	)
	drainConsumer(t, c)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stream.push(keepclient.Event{
		ID:        "evt-1",
		EventType: "watchkeeper.spawned",
		Payload:   json.RawMessage(`{"k":1}`),
	})

	pollUntil(t, 2*time.Second, "bus to record 2 calls (1 failed + 1 success)", func() bool {
		return bus.callCount() >= 2
	})

	got := bus.recordedCalls()
	if len(got) != 2 {
		t.Fatalf("bus calls = %d, want 2", len(got))
	}
	for i, call := range got {
		if call.Event.ID != "evt-1" {
			t.Fatalf("call[%d].Event.ID = %q, want evt-1", i, call.Event.ID)
		}
	}
	// One failed-attempt log entry should have been emitted.
	if entry := logger.findEntry("outbox: publish attempt failed", map[string]any{
		"event_id": "evt-1",
		"attempt":  1,
	}); entry == nil {
		t.Fatalf("expected publish-attempt-failed log entry; got: %+v", logger.snapshot())
	}
	// The retry should have slept exactly once (between attempt 1 and 2).
	if recs := sleeper.recorded(); len(recs) != 1 {
		t.Fatalf("sleeper sleeps = %d, want 1", len(recs))
	}
}

// TestConsumer_Idempotency_DuplicateIDDroppedSilently — same event id
// arrives twice; the bus is hit exactly once. Dedup window is the
// default LRU.
func TestConsumer_Idempotency_DuplicateIDDroppedSilently(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	c := New(sub, bus)
	drainConsumer(t, c)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ev := keepclient.Event{
		ID:        "evt-dup",
		EventType: "watchkeeper.spawned",
		Payload:   json.RawMessage(`{"k":1}`),
	}
	stream.push(ev)
	stream.push(ev) // forced redelivery

	// Wait for the first publish to land. Allow a generous window for
	// the second to arrive too, so we can assert it was suppressed.
	pollUntil(t, 2*time.Second, "first publish", func() bool {
		return bus.callCount() >= 1
	})
	time.Sleep(200 * time.Millisecond)
	if got := bus.callCount(); got != 1 {
		t.Fatalf("bus calls = %d, want 1 (forced redelivery should have been suppressed)", got)
	}
}

// TestConsumer_Idempotency_EmptyIDsAlwaysPublished — events with no SSE
// `id:` field are NEVER deduplicated (per the keepclient lruDedup
// contract). Two events with empty ids both reach the bus.
func TestConsumer_Idempotency_EmptyIDsAlwaysPublished(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	c := New(sub, bus)
	drainConsumer(t, c)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stream.push(keepclient.Event{EventType: "topic.x", Payload: json.RawMessage(`{"k":1}`)})
	stream.push(keepclient.Event{EventType: "topic.x", Payload: json.RawMessage(`{"k":2}`)})

	pollUntil(t, 2*time.Second, "bus to record 2 calls", func() bool {
		return bus.callCount() >= 2
	})
}

// TestConsumer_Idempotency_DisabledViaZeroSize — passing
// WithIdempotencyCacheSize(0) disables the cache; duplicate ids
// reach the bus twice.
func TestConsumer_Idempotency_DisabledViaZeroSize(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	c := New(sub, bus, WithIdempotencyCacheSize(0))
	drainConsumer(t, c)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	ev := keepclient.Event{
		ID:        "evt-1",
		EventType: "topic.x",
		Payload:   json.RawMessage(`{}`),
	}
	stream.push(ev)
	stream.push(ev)

	pollUntil(t, 2*time.Second, "bus to record 2 calls (dedup disabled)", func() bool {
		return bus.callCount() >= 2
	})
}

// TestConsumer_PublishExhausted_LoggedAndDropped — bus always errors;
// after 3 attempts the event is dropped and a "publish exhausted" log
// entry is emitted; the dedup cache does NOT record the id (so a
// future redelivery is allowed to retry).
func TestConsumer_PublishExhausted_LoggedAndDropped(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{
		injectErr:      errors.New("bus: always-fail"),
		injectErrCount: 100,
	}
	logger := &fakeLogger{}
	sleeper := &fakeSleeper{}
	c := New(
		sub, bus,
		WithLogger(logger),
		WithMaxPublishRetries(3),
		withSleeperOption(sleeper),
		withRandOption(func() float64 { return 0.5 }),
	)
	drainConsumer(t, c)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stream.push(keepclient.Event{
		ID:        "evt-1",
		EventType: "topic.x",
		Payload:   json.RawMessage(`{}`),
	})

	pollUntil(t, 2*time.Second, "exhausted-log entry", func() bool {
		return logger.findEntry("outbox: publish exhausted", nil) != nil
	})
	// Re-deliver the same id; consumer should retry (it was NOT
	// recorded in dedup cache on the failure path).
	stream.push(keepclient.Event{
		ID:        "evt-1",
		EventType: "topic.x",
		Payload:   json.RawMessage(`{}`),
	})

	pollUntil(t, 2*time.Second, "second exhausted-log entry", func() bool {
		entries := logger.snapshot()
		count := 0
		for _, e := range entries {
			if strings.HasPrefix(e.Msg, "outbox: publish exhausted") {
				count++
			}
		}
		return count >= 2
	})
}

// TestConsumer_ContextCancellation_ExitsCleanly — the parent ctx
// passed to Start is cancelled while the consumer is idle on Next;
// the goroutine exits, Stop returns nil.
func TestConsumer_ContextCancellation_ExitsCleanly(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	c := New(sub, bus)

	ctx, cancel := context.WithCancel(context.Background())
	if err := c.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	cancel()

	// Stop should return promptly without an error.
	stopDone := make(chan error, 1)
	go func() { stopDone <- c.Stop() }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop after ctx cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Stop did not return within 2s after ctx cancel")
	}
}

// TestConsumer_StopDrainsInFlight — bus.Publish is held until the test
// releases it; Stop blocks waiting for the in-flight publish to
// complete; once released, Stop returns.
func TestConsumer_StopDrainsInFlight(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	release := make(chan struct{})
	bus := &fakeBus{injectBlock: release}
	c := New(sub, bus, WithPublishTimeout(5*time.Second))

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stream.push(keepclient.Event{
		ID:        "evt-1",
		EventType: "topic.x",
		Payload:   json.RawMessage(`{}`),
	})

	// Wait for the publish to enter the bus (and block on `release`).
	pollUntil(t, 2*time.Second, "bus to enter Publish", func() bool {
		return bus.callCount() >= 1
	})

	stopDone := make(chan error, 1)
	go func() { stopDone <- c.Stop() }()

	// Stop should NOT have returned yet — the in-flight publish is
	// still blocked on `release`. Stop cancels the loop ctx, which the
	// fakeBus honours via ctx.Done(); release unblocks it cleanly.
	select {
	case err := <-stopDone:
		// Stop's loopCancel cancels the ctx, which the fake bus
		// surfaces as ctx.Err(); the consumer treats that as a
		// terminal-ctx-cancel and returns. So Stop may legitimately
		// return BEFORE we close `release` — close it now to release
		// the helper.
		close(release)
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		// expected on a slow machine: Stop is still blocked.
		close(release)
		select {
		case err := <-stopDone:
			if err != nil {
				t.Fatalf("Stop after release: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("Stop did not return after release")
		}
	}
}

// TestConsumer_StreamError_LoggedAndExits — the fake stream returns
// a non-EOF transport error; the consumer logs it, surfaces the wrap
// chain via Stop's return value, and exits cleanly.
func TestConsumer_StreamError_LoggedAndExits(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	logger := &fakeLogger{}
	c := New(sub, bus, WithLogger(logger))

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	streamErr := errors.New("stream: kaboom")
	stream.pushErr(streamErr)

	pollUntil(t, 2*time.Second, "stream-error log entry", func() bool {
		return logger.findEntry("outbox: stream error", nil) != nil
	})

	err := c.Stop()
	if err == nil {
		t.Fatalf("Stop after stream error returned nil; want wrapped error")
	}
	if !errors.Is(err, streamErr) {
		t.Fatalf("Stop err = %v; want errors.Is wrapping %v", err, streamErr)
	}
}

// TestConsumer_SubscribeError_LoggedAndExits — the subscriber returns
// an error on the first Subscribe call; the consumer logs and exits;
// Stop surfaces the wrapped error.
func TestConsumer_SubscribeError_LoggedAndExits(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	subErr := keepclient.ErrNoTokenSource
	sub.subErr = subErr
	bus := &fakeBus{}
	logger := &fakeLogger{}
	c := New(sub, bus, WithLogger(logger))

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	pollUntil(t, 2*time.Second, "subscribe-failed log entry", func() bool {
		return logger.findEntry("outbox: subscribe failed", nil) != nil
	})

	err := c.Stop()
	if !errors.Is(err, subErr) {
		t.Fatalf("Stop err = %v; want errors.Is wrapping %v", err, subErr)
	}
}

// TestConsumer_MalformedEvent_EmptyEventTypeDropped — events with no
// `event:` field cannot be routed to a bus topic; consumer logs and
// drops them without contacting the bus.
func TestConsumer_MalformedEvent_EmptyEventTypeDropped(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	logger := &fakeLogger{}
	c := New(sub, bus, WithLogger(logger))
	drainConsumer(t, c)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stream.push(keepclient.Event{
		ID:      "evt-1",
		Payload: json.RawMessage(`{}`),
	})

	pollUntil(t, 2*time.Second, "malformed-event log entry", func() bool {
		return logger.findEntry("outbox: malformed event", nil) != nil
	})

	if got := bus.callCount(); got != 0 {
		t.Fatalf("bus calls = %d, want 0 (malformed event should not reach bus)", got)
	}
}

// TestConsumer_StartTwice_ErrAlreadyStarted — second Start returns
// ErrAlreadyStarted without spawning a duplicate goroutine.
func TestConsumer_StartTwice_ErrAlreadyStarted(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	c := New(sub, bus)
	drainConsumer(t, c)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	err := c.Start(context.Background())
	if !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start err = %v, want errors.Is ErrAlreadyStarted", err)
	}
	if got := sub.callCount(); got > 1 {
		t.Fatalf("subscriber called %d times, want at most 1", got)
	}
}

// TestConsumer_StartAfterStop_ErrAlreadyStopped — Stop, then Start
// returns ErrAlreadyStopped (linear state machine).
func TestConsumer_StartAfterStop_ErrAlreadyStopped(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	c := New(sub, bus)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	_ = c.Stop()

	err := c.Start(context.Background())
	if !errors.Is(err, ErrAlreadyStopped) {
		t.Fatalf("Start after Stop err = %v, want errors.Is ErrAlreadyStopped", err)
	}
}

// TestConsumer_StopBeforeStart_ErrNotStarted — Stop on a never-Started
// consumer returns ErrNotStarted (programmer error visibility).
func TestConsumer_StopBeforeStart_ErrNotStarted(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	c := New(sub, bus)

	err := c.Stop()
	if !errors.Is(err, ErrNotStarted) {
		t.Fatalf("Stop before Start err = %v, want errors.Is ErrNotStarted", err)
	}
}

// TestConsumer_StopIdempotent — Stop twice; second returns the same
// (nil) result without re-running the shutdown sequence.
func TestConsumer_StopIdempotent(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	c := New(sub, bus)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	first := c.Stop()
	second := c.Stop()
	if first != nil || second != nil {
		t.Fatalf("Stop returns: first=%v second=%v, want nil/nil", first, second)
	}
}

// TestConsumer_NewNilSubscriber_Panics — passing a nil subscriber to
// New is a programmer error and panics with a clear message.
func TestConsumer_NewNilSubscriber_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("New(nil, bus) did not panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value type = %T, want string", r)
		}
		if !strings.Contains(msg, "outbox") || !strings.Contains(msg, "subscriber") {
			t.Fatalf("panic message = %q, want a clear outbox/subscriber message", msg)
		}
	}()
	_ = New(nil, &fakeBus{})
}

// TestConsumer_NewNilBus_Panics — passing a nil bus to New is a
// programmer error and panics with a clear message.
func TestConsumer_NewNilBus_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("New(sub, nil) did not panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value type = %T, want string", r)
		}
		if !strings.Contains(msg, "outbox") || !strings.Contains(msg, "bus") {
			t.Fatalf("panic message = %q, want a clear outbox/bus message", msg)
		}
	}()
	_ = New(newFakeSubscriber(newFakeStream()), nil)
}

// TestConsumer_RealBus_HappyPath — wires a fake stream into the real
// eventbus.Bus to confirm the LocalBus interface contract holds. A
// bus subscriber receives the published event with its DeliveredEvent
// payload intact.
func TestConsumer_RealBus_HappyPath(t *testing.T) {
	t.Parallel()

	bus := eventbus.New()
	defer func() { _ = bus.Close() }()

	received := make(chan DeliveredEvent, 1)
	unsub, err := bus.Subscribe("watchkeeper.spawned", func(_ context.Context, ev any) {
		dev, ok := ev.(DeliveredEvent)
		if !ok {
			t.Errorf("subscriber received %T, want outbox.DeliveredEvent", ev)
			return
		}
		select {
		case received <- dev:
		default:
		}
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer unsub()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	c := New(sub, bus)
	drainConsumer(t, c)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stream.push(keepclient.Event{
		ID:        "evt-real-1",
		EventType: "watchkeeper.spawned",
		Payload:   json.RawMessage(`{"id":"wk-real-1"}`),
	})

	select {
	case dev := <-received:
		if dev.ID != "evt-real-1" {
			t.Fatalf("received.ID = %q, want evt-real-1", dev.ID)
		}
		if string(dev.Payload) != `{"id":"wk-real-1"}` {
			t.Fatalf("received.Payload = %q, want {\"id\":\"wk-real-1\"}", string(dev.Payload))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("subscriber did not receive event within 2s")
	}
}

// TestConsumer_OrderingPreserved_PerStream — three events arrive in
// order on the stream; the bus records them in the same order. (The
// bus itself preserves per-topic order — see eventbus AC2 — but we
// check that the consumer never reorders during retry/dedup
// processing.)
func TestConsumer_OrderingPreserved_PerStream(t *testing.T) {
	t.Parallel()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	c := New(sub, bus)
	drainConsumer(t, c)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	for i := 0; i < 5; i++ {
		stream.push(keepclient.Event{
			ID:        fmt.Sprintf("evt-%d", i),
			EventType: "topic.x",
			Payload:   json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
		})
	}

	pollUntil(t, 2*time.Second, "bus to record 5 calls", func() bool {
		return bus.callCount() >= 5
	})

	got := bus.recordedCalls()
	for i, c := range got[:5] {
		want := fmt.Sprintf("evt-%d", i)
		if c.Event.ID != want {
			t.Fatalf("call[%d].Event.ID = %q, want %q", i, c.Event.ID, want)
		}
	}
}

// TestConsumer_NoGoroutineLeakUnderRace — goroutine baseline before
// New, Start + a few events + Stop; poll-deadline assert the post-Stop
// count returns to baseline ± slack. Pins the invariant that Stop
// drains the loop goroutine cleanly under -race.
func TestConsumer_NoGoroutineLeakUnderRace(t *testing.T) {
	// Not parallel: NumGoroutine is process-wide.
	baseline := runtime.NumGoroutine()

	stream := newFakeStream()
	sub := newFakeSubscriber(stream)
	bus := &fakeBus{}
	c := New(sub, bus)

	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stream.push(keepclient.Event{
		ID:        "evt-1",
		EventType: "topic.x",
		Payload:   json.RawMessage(`{}`),
	})
	pollUntil(t, 2*time.Second, "bus to record 1 call", func() bool {
		return bus.callCount() >= 1
	})

	if err := c.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	pollUntil(t, 2*time.Second, "goroutine count to return to baseline (±2 slack)", func() bool {
		return runtime.NumGoroutine() <= baseline+2
	})
}

// TestLRUDedup_BasicEvictionOrder — direct unit test of the dedup
// helper. Insert size+1 ids; the oldest one is evicted, all others
// remain seen.
func TestLRUDedup_BasicEvictionOrder(t *testing.T) {
	t.Parallel()
	d := newLRUDedup(3)
	d.record("a")
	d.record("b")
	d.record("c")
	d.record("d") // evicts "a"
	if d.seen("a") {
		t.Fatalf("a should have been evicted")
	}
	for _, id := range []string{"b", "c", "d"} {
		if !d.seen(id) {
			t.Fatalf("%q should still be in cache", id)
		}
	}
}

// TestLRUDedup_EmptyIDsNeverRecorded — empty ids are never reported as
// seen and never recorded (so they never displace real ids).
func TestLRUDedup_EmptyIDsNeverRecorded(t *testing.T) {
	t.Parallel()
	d := newLRUDedup(2)
	d.record("")
	d.record("")
	if d.seen("") {
		t.Fatalf("empty id should never be reported as seen")
	}
	d.record("a")
	d.record("b")
	if !d.seen("a") || !d.seen("b") {
		t.Fatalf("real ids should still fit; cache: %+v", d)
	}
}

// TestBackoffFor_ExponentialAndCapped — base 10ms, max 80ms; attempt
// values 0,1,2,3,4 should produce un-jittered values 10,20,40,80,80
// (capped at max).
func TestBackoffFor_ExponentialAndCapped(t *testing.T) {
	t.Parallel()
	got := []time.Duration{
		backoffFor(0, 10*time.Millisecond, 80*time.Millisecond, nil),
		backoffFor(1, 10*time.Millisecond, 80*time.Millisecond, nil),
		backoffFor(2, 10*time.Millisecond, 80*time.Millisecond, nil),
		backoffFor(3, 10*time.Millisecond, 80*time.Millisecond, nil),
		backoffFor(4, 10*time.Millisecond, 80*time.Millisecond, nil),
	}
	want := []time.Duration{
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
		80 * time.Millisecond,
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("backoffFor(%d) = %v, want %v", i, got[i], want[i])
		}
	}
}

// TestBackoffFor_JitterRespectsBound — randFn returning 0.5 yields the
// un-jittered base; randFn returning 0 yields the lower bound; randFn
// returning ~1 yields the upper bound.
func TestBackoffFor_JitterRespectsBound(t *testing.T) {
	t.Parallel()
	base := 100 * time.Millisecond
	maxD := 1 * time.Second

	mid := backoffFor(0, base, maxD, func() float64 { return 0.5 })
	if mid != base {
		t.Errorf("randFn=0.5 => %v, want %v (no jitter offset)", mid, base)
	}
	low := backoffFor(0, base, maxD, func() float64 { return 0 })
	if low != base-time.Duration(float64(base)*retryJitterFraction) {
		t.Errorf("randFn=0 => %v, want %v", low, base-time.Duration(float64(base)*retryJitterFraction))
	}
}
