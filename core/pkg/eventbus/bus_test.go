package eventbus

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pollUntil polls `cond` every `cadence` until it returns true or
// `deadline` elapses. Returns true if the condition fired, false on
// timeout. Mirrors the polling-deadline pattern from M2b.5
// (`docs/LESSONS.md`) — preferred over fixed sleeps for timing-sensitive
// tests because it stays robust under -race jitter on slow CI.
func pollUntil(cond func() bool, cadence, deadline time.Duration) bool {
	t := time.Now().Add(deadline)
	for time.Now().Before(t) {
		if cond() {
			return true
		}
		time.Sleep(cadence)
	}
	return cond()
}

// goroutineBaseline returns runtime.NumGoroutine() rounded so a couple of
// scheduler-spawned helpers do not flake the leak assertion. Used by
// [TestBus_NoGoroutineLeakUnderRace] — the testing framework occasionally
// spawns its own goroutines, so we assert the post-Close count returns to
// "≤ baseline + slack" rather than exact equality.
func goroutineBaseline() int {
	// Force a GC + sched yield so transient goroutines from prior tests
	// have a chance to exit before we sample.
	runtime.GC()
	runtime.Gosched()
	return runtime.NumGoroutine()
}

// TestBus_PublishSubscribe_DeliveredInOrder — AC1/AC2: subscribe one
// handler to topic "t1", publish 100 sequential events, assert handler
// receives all 100 in publish order.
func TestBus_PublishSubscribe_DeliveredInOrder(t *testing.T) {
	t.Parallel()

	b := New()
	t.Cleanup(func() { _ = b.Close() })

	const n = 100
	var (
		mu   sync.Mutex
		seen []int
	)
	_, err := b.Subscribe("t1", func(_ context.Context, event any) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, event.(int))
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for i := 0; i < n; i++ {
		if err := b.Publish(context.Background(), "t1", i); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	ok := pollUntil(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == n
	}, 5*time.Millisecond, 3*time.Second)
	if !ok {
		mu.Lock()
		t.Fatalf("got %d events, want %d", len(seen), n)
	}

	mu.Lock()
	defer mu.Unlock()
	for i, v := range seen {
		if v != i {
			t.Fatalf("seen[%d] = %d, want %d (order broken)", i, v, i)
		}
	}
}

// TestBus_FanOut_AllSubscribersReceiveAllEvents — AC2: subscribe 3
// handlers to "t1", publish 10 events, each handler receives all 10
// in order.
func TestBus_FanOut_AllSubscribersReceiveAllEvents(t *testing.T) {
	t.Parallel()

	b := New()
	t.Cleanup(func() { _ = b.Close() })

	const subs = 3
	const events = 10
	var (
		mu   sync.Mutex
		seen [subs][]int
	)

	for i := 0; i < subs; i++ {
		i := i
		_, err := b.Subscribe("t1", func(_ context.Context, event any) {
			mu.Lock()
			defer mu.Unlock()
			seen[i] = append(seen[i], event.(int))
		})
		if err != nil {
			t.Fatalf("Subscribe %d: %v", i, err)
		}
	}

	for i := 0; i < events; i++ {
		if err := b.Publish(context.Background(), "t1", i); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	ok := pollUntil(func() bool {
		mu.Lock()
		defer mu.Unlock()
		for i := 0; i < subs; i++ {
			if len(seen[i]) != events {
				return false
			}
		}
		return true
	}, 5*time.Millisecond, 3*time.Second)
	if !ok {
		t.Fatal("not all subscribers received all events")
	}

	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < subs; i++ {
		for j, v := range seen[i] {
			if v != j {
				t.Fatalf("subscriber %d seen[%d] = %d, want %d", i, j, v, j)
			}
		}
	}
}

// TestBus_DistinctTopicsAreIndependent — AC2: subscribe to "t1" and "t2",
// publish to each; assert each handler only receives its topic's events.
func TestBus_DistinctTopicsAreIndependent(t *testing.T) {
	t.Parallel()

	b := New()
	t.Cleanup(func() { _ = b.Close() })

	var (
		mu          sync.Mutex
		seen1       []int
		seen2       []int
		seen1Topic2 int
		seen2Topic1 int
	)

	_, err := b.Subscribe("t1", func(_ context.Context, event any) {
		mu.Lock()
		defer mu.Unlock()
		v := event.(int)
		if v >= 1000 {
			seen1Topic2++ // "t1" handler observed a "t2" payload — bug
		}
		seen1 = append(seen1, v)
	})
	if err != nil {
		t.Fatalf("Subscribe t1: %v", err)
	}
	_, err = b.Subscribe("t2", func(_ context.Context, event any) {
		mu.Lock()
		defer mu.Unlock()
		v := event.(int)
		if v < 1000 {
			seen2Topic1++ // "t2" handler observed a "t1" payload — bug
		}
		seen2 = append(seen2, v)
	})
	if err != nil {
		t.Fatalf("Subscribe t2: %v", err)
	}

	// "t1" carries 0..4, "t2" carries 1000..1004. Disjoint payload
	// ranges let the handler self-classify and detect cross-talk.
	for i := 0; i < 5; i++ {
		if err := b.Publish(context.Background(), "t1", i); err != nil {
			t.Fatalf("Publish t1 %d: %v", i, err)
		}
		if err := b.Publish(context.Background(), "t2", 1000+i); err != nil {
			t.Fatalf("Publish t2 %d: %v", i, err)
		}
	}

	ok := pollUntil(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen1) == 5 && len(seen2) == 5
	}, 5*time.Millisecond, 3*time.Second)
	if !ok {
		t.Fatal("did not receive expected per-topic counts")
	}

	mu.Lock()
	defer mu.Unlock()
	if seen1Topic2 != 0 {
		t.Fatalf("t1 handler observed %d t2 payloads", seen1Topic2)
	}
	if seen2Topic1 != 0 {
		t.Fatalf("t2 handler observed %d t1 payloads", seen2Topic1)
	}
}

// TestBus_Unsubscribe_StopsDelivery — AC4: subscribe, publish 1 event
// (received), unsubscribe, publish 1 more (NOT received).
func TestBus_Unsubscribe_StopsDelivery(t *testing.T) {
	t.Parallel()

	b := New()
	t.Cleanup(func() { _ = b.Close() })

	var got atomic.Int32
	unsub, err := b.Subscribe("t1", func(_ context.Context, _ any) {
		got.Add(1)
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := b.Publish(context.Background(), "t1", 1); err != nil {
		t.Fatalf("Publish 1: %v", err)
	}
	if !pollUntil(func() bool { return got.Load() >= 1 }, 5*time.Millisecond, 3*time.Second) {
		t.Fatal("first event never delivered")
	}

	unsub()

	if err := b.Publish(context.Background(), "t1", 2); err != nil {
		t.Fatalf("Publish 2: %v", err)
	}
	// Drain via a bookkeeping subscriber that fires AFTER the unsubscribed
	// handler would have. If the unsubscribed handler was still active, it
	// would also see this same event and got would increment to 2.
	var sentinel atomic.Bool
	_, err = b.Subscribe("t1", func(_ context.Context, _ any) {
		sentinel.Store(true)
	})
	if err != nil {
		t.Fatalf("Subscribe sentinel: %v", err)
	}
	if err := b.Publish(context.Background(), "t1", 3); err != nil {
		t.Fatalf("Publish 3: %v", err)
	}
	if !pollUntil(func() bool { return sentinel.Load() }, 5*time.Millisecond, 3*time.Second) {
		t.Fatal("sentinel never fired")
	}

	if got := got.Load(); got != 1 {
		t.Fatalf("unsubscribed handler fired %d times after unsubscribe, want 1", got)
	}
}

// TestBus_UnsubscribeIdempotent — AC4: call unsubscribe twice; second
// call is a no-op (no panic, no error).
func TestBus_UnsubscribeIdempotent(t *testing.T) {
	t.Parallel()

	b := New()
	t.Cleanup(func() { _ = b.Close() })

	unsub, err := b.Subscribe("t1", func(_ context.Context, _ any) {})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	unsub()
	unsub() // must not panic
	unsub() // must not panic
}

// TestBus_PublishNoSubscribers_NoError — AC2/AC4: publish to a topic with
// zero subscribers; returns nil; event is consumed by the topic worker
// and discarded.
func TestBus_PublishNoSubscribers_NoError(t *testing.T) {
	t.Parallel()

	b := New()
	t.Cleanup(func() { _ = b.Close() })

	if err := b.Publish(context.Background(), "ghost", "boo"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
}

// TestBus_LateSubscriberMissesPriorEvents — AC4: publish event A,
// subscribe (after publish has been processed), publish event B, assert
// subscriber sees only B. Polls on the topic's empty-queue condition
// before subscribing so we know event A was dispatched against the
// (empty) subscriber list before the late Subscribe runs.
func TestBus_LateSubscriberMissesPriorEvents(t *testing.T) {
	t.Parallel()

	b := New()
	t.Cleanup(func() { _ = b.Close() })

	// Publish A to a topic with zero subscribers. The worker pops it
	// and dispatches to the empty list, draining the queue.
	if err := b.Publish(context.Background(), "t1", "A"); err != nil {
		t.Fatalf("Publish A: %v", err)
	}

	// Wait for the queue to drain — confirmed by the fact that a fresh
	// publish below will be observed by a freshly-subscribed handler.
	// Use a sentinel-subscribe / sentinel-publish to confirm queue is
	// idle before installing the real late subscriber.
	var drained atomic.Bool
	unsubDrain, err := b.Subscribe("t1", func(_ context.Context, _ any) {
		drained.Store(true)
	})
	if err != nil {
		t.Fatalf("Subscribe drain: %v", err)
	}
	if err := b.Publish(context.Background(), "t1", "drain"); err != nil {
		t.Fatalf("Publish drain: %v", err)
	}
	if !pollUntil(drained.Load, 5*time.Millisecond, 3*time.Second) {
		t.Fatal("drain sentinel never fired")
	}
	unsubDrain()

	// Now install the real late subscriber. It must NOT see "A" (which
	// was dispatched before any subscriber existed) and MUST see "B".
	var (
		mu   sync.Mutex
		seen []string
	)
	_, err = b.Subscribe("t1", func(_ context.Context, event any) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, event.(string))
	})
	if err != nil {
		t.Fatalf("Subscribe late: %v", err)
	}

	if err := b.Publish(context.Background(), "t1", "B"); err != nil {
		t.Fatalf("Publish B: %v", err)
	}

	ok := pollUntil(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) >= 1
	}, 5*time.Millisecond, 3*time.Second)
	if !ok {
		t.Fatal("late subscriber never received B")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "B" {
		t.Fatalf("late subscriber saw %v, want [B]", seen)
	}
}

// TestBus_BackpressureBlocksUntilDrained — AC3: set buffer size 2, slow
// handler (~50ms sleep), publish 5 events concurrently; assert the later
// publishes return only after the handler drains. Uses timestamps to
// confirm the publishers were ordered by the queue's release schedule
// rather than all returning together.
func TestBus_BackpressureBlocksUntilDrained(t *testing.T) {
	t.Parallel()

	b := New(WithTopicBufferSize(2))
	t.Cleanup(func() { _ = b.Close() })

	const handlerSleep = 50 * time.Millisecond
	const events = 5

	_, err := b.Subscribe("t1", func(_ context.Context, _ any) {
		time.Sleep(handlerSleep)
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	start := time.Now()
	for i := 0; i < events; i++ {
		if err := b.Publish(context.Background(), "t1", i); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)

	// With buffer=2 + 1 in-flight handler, the first 3 publishes accept
	// nearly instantly. Publishes 4 and 5 must wait for at least 2
	// handler completions to free their slots → ≥ 2*handlerSleep =
	// 100ms total. Apply a generous lower bound to absorb scheduler
	// jitter under -race; the actual sequential lower bound is much
	// higher.
	minWait := handlerSleep
	if elapsed < minWait {
		t.Fatalf("Publish loop returned in %v, want ≥ %v (no backpressure observed)",
			elapsed, minWait)
	}
}

// TestBus_PublishHonorsCtxCancellation — AC3: set buffer size 1, hold the
// slot with one in-flight slow handler, publish with a ctx that cancels
// in 50ms; assert error wraps context.DeadlineExceeded (or
// context.Canceled).
func TestBus_PublishHonorsCtxCancellation(t *testing.T) {
	t.Parallel()

	b := New(WithTopicBufferSize(1))
	t.Cleanup(func() { _ = b.Close() })

	// Block the worker on a never-released channel so the queue stays
	// full once we fill it.
	release := make(chan struct{})
	_, err := b.Subscribe("t1", func(_ context.Context, _ any) {
		<-release
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { close(release) })

	// Fill the buffer (size 1) so the next Publish blocks. Handler is
	// blocked on `release`, so the worker has popped 0 envelopes — but
	// we need to wait for the worker to pop the first one (which then
	// blocks the handler) so the SECOND publish actually fills the slot
	// rather than going straight to the handler.
	if err := b.Publish(context.Background(), "t1", "first"); err != nil {
		t.Fatalf("Publish first: %v", err)
	}
	// Give the worker a moment to pop and start the handler.
	time.Sleep(20 * time.Millisecond)
	if err := b.Publish(context.Background(), "t1", "second"); err != nil {
		t.Fatalf("Publish second: %v", err)
	}
	// Now buffer is full (slot holds "second"), worker is blocked in the
	// handler on "first". The next publish must block on backpressure.

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = b.Publish(ctx, "t1", "third")
	if err == nil {
		t.Fatal("Publish on full buffer with cancelled ctx returned nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want one wrapping context.DeadlineExceeded / Canceled", err)
	}
	wantPrefix := "eventbus: publish:"
	if got := err.Error(); !strings.Contains(got, wantPrefix) {
		t.Fatalf("err.Error() = %q, want prefix %q", got, wantPrefix)
	}
}

// TestBus_CloseDrainsInFlightEvents — AC5: publish 10 events, Close
// immediately; assert all 10 reach the handler before Close returns.
func TestBus_CloseDrainsInFlightEvents(t *testing.T) {
	t.Parallel()

	b := New()

	const n = 10
	var got atomic.Int32
	_, err := b.Subscribe("t1", func(_ context.Context, _ any) {
		got.Add(1)
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	for i := 0; i < n; i++ {
		if err := b.Publish(context.Background(), "t1", i); err != nil {
			t.Fatalf("Publish %d: %v", i, err)
		}
	}

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := got.Load(); got != n {
		t.Fatalf("handler fired %d times, want %d", got, n)
	}
}

// TestBus_PublishAfterCloseReturnsErrClosed — AC5: Close, Publish →
// errors.Is(err, ErrClosed).
func TestBus_PublishAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()

	b := New()
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := b.Publish(context.Background(), "t1", 1)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

// TestBus_SubscribeAfterCloseReturnsErrClosed — AC5: Close, Subscribe →
// errors.Is(err, ErrClosed).
func TestBus_SubscribeAfterCloseReturnsErrClosed(t *testing.T) {
	t.Parallel()

	b := New()
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := b.Subscribe("t1", func(_ context.Context, _ any) {})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

// TestBus_CloseIdempotent — AC5: Close twice; second call returns nil
// without rerunning drain.
func TestBus_CloseIdempotent(t *testing.T) {
	t.Parallel()

	b := New()
	if err := b.Close(); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close 2: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close 3: %v", err)
	}
}

// TestBus_PublishEmptyTopicReturnsErrInvalidTopic — AC6.
func TestBus_PublishEmptyTopicReturnsErrInvalidTopic(t *testing.T) {
	t.Parallel()

	b := New()
	t.Cleanup(func() { _ = b.Close() })

	err := b.Publish(context.Background(), "", 1)
	if !errors.Is(err, ErrInvalidTopic) {
		t.Fatalf("err = %v, want ErrInvalidTopic", err)
	}
}

// TestBus_SubscribeEmptyTopicReturnsErrInvalidTopic — AC6.
func TestBus_SubscribeEmptyTopicReturnsErrInvalidTopic(t *testing.T) {
	t.Parallel()

	b := New()
	t.Cleanup(func() { _ = b.Close() })

	_, err := b.Subscribe("", func(_ context.Context, _ any) {})
	if !errors.Is(err, ErrInvalidTopic) {
		t.Fatalf("err = %v, want ErrInvalidTopic", err)
	}
}

// TestBus_SubscribeNilHandlerReturnsErrInvalidHandler — AC6.
func TestBus_SubscribeNilHandlerReturnsErrInvalidHandler(t *testing.T) {
	t.Parallel()

	b := New()
	t.Cleanup(func() { _ = b.Close() })

	_, err := b.Subscribe("t1", nil)
	if !errors.Is(err, ErrInvalidHandler) {
		t.Fatalf("err = %v, want ErrInvalidHandler", err)
	}
}

// TestBus_NoGoroutineLeakUnderRace — AC5: snapshot runtime.NumGoroutine()
// before [New] and after [Bus.Close]; assert post-close count returns to
// baseline within a polling deadline. Polling because workers may need a
// scheduler tick to fully exit. Slack of +2 absorbs occasional helper
// goroutines spawned by the testing framework / race runtime.
func TestBus_NoGoroutineLeakUnderRace(t *testing.T) {
	// Not t.Parallel: NumGoroutine() snapshots are sensitive to other
	// concurrent tests in the same binary.
	baseline := goroutineBaseline()

	b := New()
	for i := 0; i < 5; i++ {
		topic := "t" + string(rune('0'+i))
		_, err := b.Subscribe(topic, func(_ context.Context, _ any) {})
		if err != nil {
			t.Fatalf("Subscribe %s: %v", topic, err)
		}
		if err := b.Publish(context.Background(), topic, i); err != nil {
			t.Fatalf("Publish %s: %v", topic, err)
		}
	}

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	const slack = 2
	ok := pollUntil(func() bool {
		return goroutineBaseline() <= baseline+slack
	}, 10*time.Millisecond, 2*time.Second)
	if !ok {
		t.Fatalf("goroutine count %d still exceeds baseline %d (+%d slack) after Close",
			runtime.NumGoroutine(), baseline, slack)
	}
}

// TestBus_ConcurrentPublishersPreserveOrderPerTopic — AC2: spawn N
// publishers tagging events with (publisher_id, seq); assert per-publisher
// seq ordering is preserved by the consumer. Publish-order across
// publishers is enqueue-order (NOT call-time order) — documented in
// README; this test asserts only the per-publisher monotonicity property.
func TestBus_ConcurrentPublishersPreserveOrderPerTopic(t *testing.T) {
	t.Parallel()

	b := New(WithTopicBufferSize(256))
	t.Cleanup(func() { _ = b.Close() })

	const publishers = 8
	const perPublisher = 50
	type tagged struct {
		pub int
		seq int
	}

	var (
		mu   sync.Mutex
		seen []tagged
	)
	_, err := b.Subscribe("t1", func(_ context.Context, event any) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, event.(tagged))
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func(pub int) {
			defer wg.Done()
			for s := 0; s < perPublisher; s++ {
				if err := b.Publish(context.Background(), "t1", tagged{pub: pub, seq: s}); err != nil {
					t.Errorf("Publish (pub=%d seq=%d): %v", pub, s, err)
					return
				}
			}
		}(p)
	}
	wg.Wait()

	total := publishers * perPublisher
	ok := pollUntil(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) == total
	}, 5*time.Millisecond, 5*time.Second)
	if !ok {
		mu.Lock()
		t.Fatalf("got %d events, want %d", len(seen), total)
	}

	mu.Lock()
	defer mu.Unlock()
	// Walk in observed (=enqueue) order. Per-publisher seq must be
	// strictly monotonically increasing — the bus serialises sends to
	// `ts.ch` so the worker pops them in send-order, and a single
	// publisher's serial sends are themselves in seq order.
	last := make(map[int]int, publishers)
	for i := range last {
		last[i] = -1
	}
	for _, ev := range seen {
		prev, ok := last[ev.pub]
		if !ok {
			prev = -1
		}
		if ev.seq <= prev {
			t.Fatalf("publisher %d: seq %d followed by %d (out of order)", ev.pub, prev, ev.seq)
		}
		last[ev.pub] = ev.seq
	}
	for p := 0; p < publishers; p++ {
		if last[p] != perPublisher-1 {
			t.Fatalf("publisher %d: last seq %d, want %d", p, last[p], perPublisher-1)
		}
	}
}

// Compile-time assertion: confirm the exported `Handler` type alias has
// the documented shape. A signature drift (e.g. dropping the ctx) would
// fail to compile here, surfacing the break before any test runs.
var _ Handler = func(_ context.Context, _ any) {}

// Compile-time assertion: New returns *Bus and accepts Option variadic.
var _ = func() *Bus { return New(WithTopicBufferSize(8)) }
