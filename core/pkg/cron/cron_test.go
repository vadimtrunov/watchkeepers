package cron

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/eventbus"
)

// Compile-time assertion that the concrete *eventbus.Bus satisfies the
// LocalPublisher interface this package exposes. Test-only import of
// eventbus: production cron code never depends on the concrete type —
// only on the interface — mirroring the lifecycle.LocalKeepClient
// pattern documented in `docs/LESSONS.md` (M3.2.b). A future
// eventbus.Bus.Publish signature change breaks this line at the same
// compile step the production wiring breaks at.
var _ LocalPublisher = (*eventbus.Bus)(nil)

// pollUntil is the polling-deadline helper documented in
// `docs/LESSONS.md` (M2b.5). Polls `cond` every 10ms until either the
// condition returns true or `deadline` elapses; on timeout calls
// t.Fatalf with `desc`. Tests use this in lieu of fixed-sleep
// assertions to ride out the inherent jitter of robfig/cron's
// sub-second tick (every-second specs land within ~1.0–1.1s of each
// other depending on Start phase relative to the wall clock).
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

// TestScheduler_FactoryFiresAndPublishes — happy path: schedule
// `* * * * * *` (every second) with a factory that captures call count;
// Start; poll-deadline 5s for callCount ≥ 2; assert the factory ran on
// every fire (distinct events recorded).
func TestScheduler_FactoryFiresAndPublishes(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	s := New(pub)

	var calls atomic.Int32
	factory := func(_ context.Context) any {
		n := calls.Add(1)
		return map[string]any{"n": n}
	}
	if _, err := s.Schedule("* * * * * *", "watchkeeper.cron", factory); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { <-s.Stop().Done() })

	pollUntil(t, 5*time.Second, "publisher to record ≥2 calls", func() bool {
		return pub.callCount() >= 2
	})

	got := pub.recordedCalls()
	if len(got) < 2 {
		t.Fatalf("recorded %d calls, want ≥2", len(got))
	}
	// Each fire mints a fresh event via the factory; the recorded
	// events must all carry distinct n values (1, 2, …).
	seen := make(map[int32]int)
	for i, c := range got {
		ev, ok := c.Event.(map[string]any)
		if !ok {
			t.Fatalf("call[%d].Event type = %T, want map[string]any", i, c.Event)
		}
		n, _ := ev["n"].(int32)
		if seen[n] != 0 {
			t.Fatalf("duplicate event n=%d at calls %d and %d", n, seen[n]-1, i)
		}
		seen[n] = i + 1
	}
}

// TestScheduler_TopicAndEventDelivered — happy path: schedule with topic
// "watchkeeper.cron" and a factory returning {tick: i}; assert the topic
// flows through unchanged and the event payload carries the bumped
// counter.
func TestScheduler_TopicAndEventDelivered(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	s := New(pub)

	var tick atomic.Int32
	factory := func(_ context.Context) any {
		return map[string]any{"tick": tick.Add(1)}
	}
	if _, err := s.Schedule("* * * * * *", "watchkeeper.cron", factory); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { <-s.Stop().Done() })

	pollUntil(t, 5*time.Second, "publisher to record ≥1 call", func() bool {
		return pub.callCount() >= 1
	})

	got := pub.recordedCalls()
	if got[0].Topic != "watchkeeper.cron" {
		t.Fatalf("call[0].Topic = %q, want %q", got[0].Topic, "watchkeeper.cron")
	}
	ev, ok := got[0].Event.(map[string]any)
	if !ok {
		t.Fatalf("call[0].Event type = %T, want map[string]any", got[0].Event)
	}
	if ev["tick"] != int32(1) {
		t.Fatalf("call[0].Event tick = %v, want 1", ev["tick"])
	}
}

// TestScheduler_MultipleEntriesIndependent — two entries with different
// topics fire on the same `* * * * * *` schedule; assert both topics
// reach ≥1 publish and that the streams stay independent.
func TestScheduler_MultipleEntriesIndependent(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	s := New(pub)

	var aCount, bCount atomic.Int32
	if _, err := s.Schedule("* * * * * *", "topic.a", func(_ context.Context) any {
		return map[string]any{"a": aCount.Add(1)}
	}); err != nil {
		t.Fatalf("Schedule a: %v", err)
	}
	if _, err := s.Schedule("* * * * * *", "topic.b", func(_ context.Context) any {
		return map[string]any{"b": bCount.Add(1)}
	}); err != nil {
		t.Fatalf("Schedule b: %v", err)
	}

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { <-s.Stop().Done() })

	pollUntil(t, 5*time.Second, "both topics to record ≥1 publish", func() bool {
		return len(pub.callsForTopic("topic.a")) >= 1 && len(pub.callsForTopic("topic.b")) >= 1
	})

	for _, c := range pub.callsForTopic("topic.a") {
		ev, ok := c.Event.(map[string]any)
		if !ok || ev["a"] == nil {
			t.Fatalf("topic.a leaked into other stream: %+v", c)
		}
	}
	for _, c := range pub.callsForTopic("topic.b") {
		ev, ok := c.Event.(map[string]any)
		if !ok || ev["b"] == nil {
			t.Fatalf("topic.b leaked into other stream: %+v", c)
		}
	}
}

// TestScheduler_UnscheduleStopsDelivery — schedule, poll for the first
// publish, Unschedule, sleep 2s, assert no further publishes for that
// topic landed (count is stable). Uses a snapshot+sleep+recount because
// "no event happened" is inherently a deadline assertion.
func TestScheduler_UnscheduleStopsDelivery(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	s := New(pub)

	id, err := s.Schedule("* * * * * *", "watchkeeper.cron", func(_ context.Context) any {
		return struct{}{}
	})
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { <-s.Stop().Done() })

	pollUntil(t, 5*time.Second, "publisher to record ≥1 call", func() bool {
		return pub.callCount() >= 1
	})

	if err := s.Unschedule(id); err != nil {
		t.Fatalf("Unschedule: %v", err)
	}

	// Capture the count immediately after Unschedule. Allow a small
	// grace window for any in-flight fire that already entered the
	// runner queue before Unschedule took effect.
	time.Sleep(100 * time.Millisecond)
	before := pub.callCount()
	time.Sleep(2 * time.Second)
	after := pub.callCount()
	if after != before {
		t.Fatalf("calls after Unschedule = %d, want stable at %d", after, before)
	}
}

// TestScheduler_UnscheduleIdempotent — Unschedule twice with the same id;
// second call returns nil without panicking.
func TestScheduler_UnscheduleIdempotent(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	s := New(pub)
	t.Cleanup(func() { <-s.Stop().Done() })

	id, err := s.Schedule("* * * * * *", "topic", func(_ context.Context) any { return nil })
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := s.Unschedule(id); err != nil {
		t.Fatalf("first Unschedule: %v", err)
	}
	if err := s.Unschedule(id); err != nil {
		t.Fatalf("second Unschedule: %v", err)
	}
}

// TestScheduler_FactoryPanic_RecoveredAndLogged — factory panics on the
// first call; scheduler logs and continues; pub records 0 calls from
// the panicking fire (factory panicked before Publish) and ≥1 call from
// later fires (panic toggle reset after first call).
func TestScheduler_FactoryPanic_RecoveredAndLogged(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	logger := &fakeLogger{}
	s := New(pub, WithLogger(logger))

	var calls atomic.Int32
	if _, err := s.Schedule("* * * * * *", "topic", func(_ context.Context) any {
		n := calls.Add(1)
		if n == 1 {
			panic("boom")
		}
		return map[string]any{"n": n}
	}); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { <-s.Stop().Done() })

	// The first fire panics in the factory, before Publish is reached.
	// The second fire returns normally and reaches Publish.
	pollUntil(t, 5*time.Second, "publisher to record ≥1 call after first-fire panic", func() bool {
		return pub.callCount() >= 1
	})
	if logger.count() < 1 {
		t.Fatalf("logger entries = %d, want ≥1 (panic recovery should have logged)", logger.count())
	}
}

// TestScheduler_PublishError_LoggedNotHalted — fake pub always returns
// errPublishBoom; scheduler logs but keeps firing; pub.callCount
// reaches ≥2 within deadline.
func TestScheduler_PublishError_LoggedNotHalted(t *testing.T) {
	t.Parallel()
	errPublishBoom := errors.New("publish kaboom")
	pub := &fakePublisher{injectErr: errPublishBoom}
	logger := &fakeLogger{}
	s := New(pub, WithLogger(logger))

	if _, err := s.Schedule("* * * * * *", "topic", func(_ context.Context) any { return nil }); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { <-s.Stop().Done() })

	pollUntil(t, 5*time.Second, "publisher to record ≥2 errored calls", func() bool {
		return pub.callCount() >= 2
	})
	if logger.count() < 2 {
		t.Fatalf("logger entries = %d, want ≥2 (each errored publish should log)", logger.count())
	}
}

// TestScheduler_StartTwice_ErrAlreadyStarted — Start succeeds, second
// Start returns ErrAlreadyStarted without spawning a duplicate runner.
func TestScheduler_StartTwice_ErrAlreadyStarted(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	s := New(pub)
	t.Cleanup(func() { <-s.Stop().Done() })

	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	err := s.Start(context.Background())
	if !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start err = %v, want errors.Is ErrAlreadyStarted", err)
	}
}

// TestScheduler_StopDrains — fake publisher blocks until the stop-
// drain test releases it; assert Stop's returned ctx only Done()s after
// the in-flight publish returns.
func TestScheduler_StopDrains(t *testing.T) {
	t.Parallel()
	release := make(chan struct{}, 1)
	pub := &fakePublisher{injectBlock: release}
	s := New(pub)

	if _, err := s.Schedule("* * * * * *", "topic", func(_ context.Context) any { return nil }); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for the first fire to enter Publish (and block on `release`).
	pollUntil(t, 5*time.Second, "publisher to enter Publish", func() bool {
		return pub.callCount() >= 1
	})

	// Stop in a separate goroutine — its returned ctx will only Done()
	// after the in-flight Publish releases.
	stopCtx := s.Stop()
	select {
	case <-stopCtx.Done():
		t.Fatalf("Stop's ctx done before publisher released")
	case <-time.After(200 * time.Millisecond):
		// expected: still draining
	}

	close(release)

	select {
	case <-stopCtx.Done():
		// expected
	case <-time.After(5 * time.Second):
		t.Fatalf("Stop's ctx did not done after release")
	}
}

// TestScheduler_StopIdempotent — Stop twice; second returns the same
// already-done ctx without re-entering robfig.
func TestScheduler_StopIdempotent(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	s := New(pub)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	first := s.Stop()
	<-first.Done()
	second := s.Stop()
	if second != first {
		t.Fatalf("second Stop returned a different ctx (%p vs %p)", second, first)
	}
	select {
	case <-second.Done():
		// expected
	default:
		t.Fatalf("second Stop ctx not done")
	}
}

// TestScheduler_ScheduleAfterStop_ErrAlreadyStopped — Stop, then both
// Schedule and Unschedule return ErrAlreadyStopped.
func TestScheduler_ScheduleAfterStop_ErrAlreadyStopped(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	s := New(pub)
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	<-s.Stop().Done()

	if _, err := s.Schedule("* * * * * *", "topic", func(_ context.Context) any { return nil }); !errors.Is(err, ErrAlreadyStopped) {
		t.Fatalf("Schedule after Stop err = %v, want errors.Is ErrAlreadyStopped", err)
	}
	if err := s.Unschedule(0); !errors.Is(err, ErrAlreadyStopped) {
		t.Fatalf("Unschedule after Stop err = %v, want errors.Is ErrAlreadyStopped", err)
	}
}

// TestScheduler_ScheduleEmptySpec_ErrInvalidSpec — empty spec is a
// synchronous ErrInvalidSpec; the fake records ZERO calls (no fire
// could happen because no entry was registered).
func TestScheduler_ScheduleEmptySpec_ErrInvalidSpec(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	s := New(pub)
	t.Cleanup(func() { <-s.Stop().Done() })

	_, err := s.Schedule("", "topic", func(_ context.Context) any { return nil })
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("Schedule empty spec err = %v, want errors.Is ErrInvalidSpec", err)
	}
	if got := pub.callCount(); got != 0 {
		t.Fatalf("publisher recorded %d calls, want 0", got)
	}
}

// TestScheduler_ScheduleEmptyTopic_ErrInvalidTopic — empty topic is a
// synchronous ErrInvalidTopic; ZERO publisher calls.
func TestScheduler_ScheduleEmptyTopic_ErrInvalidTopic(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	s := New(pub)
	t.Cleanup(func() { <-s.Stop().Done() })

	_, err := s.Schedule("* * * * * *", "", func(_ context.Context) any { return nil })
	if !errors.Is(err, ErrInvalidTopic) {
		t.Fatalf("Schedule empty topic err = %v, want errors.Is ErrInvalidTopic", err)
	}
	if got := pub.callCount(); got != 0 {
		t.Fatalf("publisher recorded %d calls, want 0", got)
	}
}

// TestScheduler_ScheduleNilFactory_ErrInvalidFactory — nil factory is a
// synchronous ErrInvalidFactory; ZERO publisher calls.
func TestScheduler_ScheduleNilFactory_ErrInvalidFactory(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	s := New(pub)
	t.Cleanup(func() { <-s.Stop().Done() })

	_, err := s.Schedule("* * * * * *", "topic", nil)
	if !errors.Is(err, ErrInvalidFactory) {
		t.Fatalf("Schedule nil factory err = %v, want errors.Is ErrInvalidFactory", err)
	}
	if got := pub.callCount(); got != 0 {
		t.Fatalf("publisher recorded %d calls, want 0", got)
	}
}

// TestScheduler_ScheduleBadSpec_WrappedParseError — a non-empty but
// malformed spec surfaces as a wrapped parser error. errors.Is against
// ErrInvalidSpec does NOT match (this is a parser error, not the
// synchronous-validation case). ZERO publisher calls.
func TestScheduler_ScheduleBadSpec_WrappedParseError(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	s := New(pub)
	t.Cleanup(func() { <-s.Stop().Done() })

	_, err := s.Schedule("not a cron spec", "topic", func(_ context.Context) any { return nil })
	if err == nil {
		t.Fatalf("Schedule bad spec returned nil error")
	}
	if errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("Schedule bad spec err matched ErrInvalidSpec; want a parser error wrap")
	}
	if !strings.Contains(err.Error(), "cron: parse spec") {
		t.Fatalf("Schedule bad spec err message = %q, want prefix %q", err.Error(), "cron: parse spec")
	}
	if got := pub.callCount(); got != 0 {
		t.Fatalf("publisher recorded %d calls, want 0", got)
	}
}

// TestScheduler_NewNilPublisher_Panics — passing a nil publisher to New
// is a programmer error and panics with a clear message containing both
// `cron` and `nil`.
func TestScheduler_NewNilPublisher_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("New(nil) did not panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("New(nil) panic value type = %T, want string", r)
		}
		if !strings.Contains(msg, "cron") || !strings.Contains(msg, "nil") {
			t.Fatalf("New(nil) panic message = %q, want a clear cron/nil message", msg)
		}
	}()
	_ = New(nil)
}

// TestScheduler_NoGoroutineLeakUnderRace — goroutine baseline before
// New, schedule + Start + few fires + Stop; poll-deadline assert the
// post-Stop count returns to baseline ± slack. Pins the invariant that
// Stop drains every worker goroutine — no leaks under -race.
func TestScheduler_NoGoroutineLeakUnderRace(t *testing.T) {
	// Not parallel: NumGoroutine is process-wide, so a parallel test
	// could perturb the baseline.
	baseline := runtime.NumGoroutine()

	pub := &fakePublisher{}
	s := New(pub)
	if _, err := s.Schedule("* * * * * *", "topic", func(_ context.Context) any { return nil }); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	pollUntil(t, 5*time.Second, "publisher to record ≥1 call", func() bool {
		return pub.callCount() >= 1
	})

	<-s.Stop().Done()

	// Allow a brief settle window for goroutines to exit after Stop
	// returns.
	pollUntil(t, 2*time.Second, "goroutine count to return to baseline (±2 slack)", func() bool {
		return runtime.NumGoroutine() <= baseline+2
	})
}
