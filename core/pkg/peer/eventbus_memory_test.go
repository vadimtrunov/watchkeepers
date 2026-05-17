package peer_test

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/peer"
)

// newMemoryBus is a small helper hoisting `NewMemoryEventBus` invocation
// so each test stays a one-liner. The default config (16-slot buffer,
// time.Now clock) is exactly what every test below wants.
func newMemoryBus(t *testing.T, opts ...peer.MemoryEventBusOption) *peer.MemoryEventBus {
	t.Helper()
	b := peer.NewMemoryEventBus(opts...)
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func sampleEvent(orgID uuid.UUID, wkID, eventType string, payload []byte) peer.Event {
	return peer.Event{
		ID:             uuid.New(),
		OrganizationID: orgID,
		WatchkeeperID:  wkID,
		EventType:      eventType,
		Payload:        payload,
	}
}

func TestMemoryEventBus_PublishSubscribeHappyPath(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{OrganizationID: orgID})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ev := sampleEvent(orgID, "wk-asker", "k2k_message_sent", []byte(`{"k":"v"}`))
	if err := bus.Publish(ctx, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-ch:
		if got.ID != ev.ID {
			t.Errorf("got.ID = %s, want %s", got.ID, ev.ID)
		}
		if got.EventType != ev.EventType {
			t.Errorf("got.EventType = %q, want %q", got.EventType, ev.EventType)
		}
		if string(got.Payload) != `{"k":"v"}` {
			t.Errorf("got.Payload = %q, want %q", got.Payload, `{"k":"v"}`)
		}
		if got.CreatedAt.IsZero() {
			t.Error("CreatedAt zero; want stamped by bus")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivery")
	}
}

func TestMemoryEventBus_PublishValidationFailures(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t)
	mk := func() peer.Event {
		return sampleEvent(orgID, "wk-asker", "k2k_message_sent", []byte(`{}`))
	}
	cases := []struct {
		name   string
		mutate func(e *peer.Event)
		want   error
	}{
		{name: "zero id", mutate: func(e *peer.Event) { e.ID = uuid.Nil }, want: peer.ErrInvalidEventID},
		{name: "zero org id", mutate: func(e *peer.Event) { e.OrganizationID = uuid.Nil }, want: peer.ErrInvalidOrganizationID},
		{name: "empty wk id", mutate: func(e *peer.Event) { e.WatchkeeperID = " " }, want: peer.ErrEmptyWatchkeeperID},
		{name: "empty event type", mutate: func(e *peer.Event) { e.EventType = "  " }, want: peer.ErrEmptyEventType},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := mk()
			tc.mutate(&e)
			if err := bus.Publish(context.Background(), e); err != tc.want {
				t.Errorf("Publish err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestMemoryEventBus_SubscribeValidatesOrgID(t *testing.T) {
	t.Parallel()
	bus := newMemoryBus(t)
	_, _, err := bus.Subscribe(context.Background(), peer.SubscribeFilter{})
	if err != peer.ErrInvalidOrganizationID {
		t.Errorf("Subscribe err = %v, want %v", err, peer.ErrInvalidOrganizationID)
	}
}

func TestMemoryEventBus_FilterByTargetWatchkeeperID(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{
		OrganizationID:      orgID,
		TargetWatchkeeperID: "wk-target",
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Publish two events: one matching, one not.
	if err := bus.Publish(ctx, sampleEvent(orgID, "wk-other", "k2k_message_sent", nil)); err != nil {
		t.Fatalf("Publish other: %v", err)
	}
	target := sampleEvent(orgID, "wk-target", "k2k_message_sent", nil)
	if err := bus.Publish(ctx, target); err != nil {
		t.Fatalf("Publish target: %v", err)
	}

	select {
	case got := <-ch:
		if got.WatchkeeperID != "wk-target" {
			t.Errorf("got %q first; want wk-target (filter must drop wk-other)", got.WatchkeeperID)
		}
		if got.ID != target.ID {
			t.Errorf("got.ID = %s, want %s (target event)", got.ID, target.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivery")
	}

	// No second delivery — the unmatched event must NOT arrive.
	select {
	case got := <-ch:
		t.Errorf("unexpected second delivery: %v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestMemoryEventBus_FilterByEventTypes(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{
		OrganizationID: orgID,
		EventTypes:     []string{"k2k_message_sent", "tool_invoked"},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Three events: one match k2k_message_sent, one tool_invoked, one
	// k2k_conversation_opened (must be filtered out).
	want1 := sampleEvent(orgID, "wk", "k2k_message_sent", nil)
	want2 := sampleEvent(orgID, "wk", "tool_invoked", nil)
	skip := sampleEvent(orgID, "wk", "k2k_conversation_opened", nil)
	for _, e := range []peer.Event{want1, skip, want2} {
		if err := bus.Publish(ctx, e); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	got := make(map[uuid.UUID]bool)
	deadline := time.After(time.Second)
	for len(got) < 2 {
		select {
		case ev := <-ch:
			got[ev.ID] = true
		case <-deadline:
			t.Fatalf("timed out; got = %v", got)
		}
	}
	if !got[want1.ID] || !got[want2.ID] {
		t.Errorf("got = %v, want want1+want2", got)
	}
	if got[skip.ID] {
		t.Error("filter delivered skipped event")
	}
}

func TestMemoryEventBus_PerTenantIsolation(t *testing.T) {
	t.Parallel()

	orgA := uuid.New()
	orgB := uuid.New()
	bus := newMemoryBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chA, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{OrganizationID: orgA})
	if err != nil {
		t.Fatalf("Subscribe A: %v", err)
	}

	if err := bus.Publish(ctx, sampleEvent(orgB, "wk", "evt", nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// chA must NOT see orgB's event.
	select {
	case got := <-chA:
		t.Errorf("orgA subscriber observed foreign-tenant event: %v", got)
	case <-time.After(80 * time.Millisecond):
	}
}

// TestMemoryEventBus_CtxCancelClosesChannel — the M1.3.c AC pins
// "channel closed on ctx cancellation". The test subscribes, cancels
// the supplied ctx, and asserts the channel closes.
func TestMemoryEventBus_CtxCancelClosesChannel(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t)
	ctx, cancel := context.WithCancel(context.Background())

	ch, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{OrganizationID: orgID})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel returned a value after ctx cancel; want closed")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close on ctx cancel")
	}
}

// TestMemoryEventBus_CancelFuncClosesChannel — the M1.3.c AC pins
// "CancelFunc closes the channel".
func TestMemoryEventBus_CancelFuncClosesChannel(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t)
	ch, cancelFn, err := bus.Subscribe(context.Background(), peer.SubscribeFilter{OrganizationID: orgID})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancelFn()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel returned a value after CancelFunc; want closed")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel close on CancelFunc")
	}

	// Double-cancel is a no-op.
	cancelFn()
}

// TestMemoryEventBus_CancelLeakPinsZeroGoroutines exercises the M1.3.c
// AC "cancel-leak test pins zero goroutines after 100 subscribe/cancel
// cycles". The test captures the goroutine count before and after 100
// subscribe-then-cancel cycles; the post-count must NOT exceed the
// pre-count by more than a small constant (the bus's bookkeeping
// goroutines).
func TestMemoryEventBus_CancelLeakPinsZeroGoroutines(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t)

	// Settle: spawn-cancel one subscription to warm up the bus before
	// taking the baseline.
	ctxWarm, cancelWarm := context.WithCancel(context.Background())
	if _, _, err := bus.Subscribe(ctxWarm, peer.SubscribeFilter{OrganizationID: orgID}); err != nil {
		t.Fatalf("warm Subscribe: %v", err)
	}
	cancelWarm()
	time.Sleep(20 * time.Millisecond)
	runtime.GC()

	before := runtime.NumGoroutine()

	for i := 0; i < 100; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		ch, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{OrganizationID: orgID})
		if err != nil {
			t.Fatalf("Subscribe[%d]: %v", i, err)
		}
		cancel()
		// Drain the closed channel so the test does not race the bus's
		// teardown goroutine.
		//nolint:revive // empty-block: draining a closed channel is the explicit purpose of the loop body.
		for range ch {
		}
	}

	// Give the watchdog goroutines a chance to exit.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
		runtime.GC()
	}

	after := runtime.NumGoroutine()
	// Allow a small slack (+2) for the test harness's own bookkeeping
	// goroutines (the t.Parallel scheduler, the deadline timer).
	if after > before+2 {
		t.Errorf("goroutine leak: before=%d after=%d (delta>2)", before, after)
	}

	// Also assert the bus's internal subscription map drained.
	// activeSubscriptions is an unexported helper we reach via the
	// black-box test boundary; the bus does NOT export it intentionally.
	// We test the externally-visible drop counter instead.
	if dropped := bus.DroppedEvents(); dropped > 0 {
		// No publishes happened in this test — a non-zero drop counter
		// would indicate a different bug (not a leak), but it is
		// load-bearing that this test does not provoke drops.
		t.Errorf("DroppedEvents = %d, want 0 (no publishes occurred)", dropped)
	}
}

// TestMemoryEventBus_SlowConsumerDropsAndCounts exercises the M1.3.c AC
// "slow-consumer drop policy (bounded buffer + counter on dropped
// events)". The test wires a 4-slot bus, publishes 16 events without
// draining, and asserts the drop counter advanced by exactly 12.
func TestMemoryEventBus_SlowConsumerDropsAndCounts(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t, peer.WithMemoryBufferSize(4))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{OrganizationID: orgID})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	before := bus.DroppedEvents()
	for i := 0; i < 16; i++ {
		if err := bus.Publish(ctx, sampleEvent(orgID, "wk", "evt", nil)); err != nil {
			t.Fatalf("Publish[%d]: %v", i, err)
		}
	}

	// Buffer = 4 → 4 delivered, 12 dropped.
	delivered := 0
	drainDeadline := time.After(200 * time.Millisecond)
drainLoop:
	for {
		select {
		case <-ch:
			delivered++
		case <-drainDeadline:
			break drainLoop
		}
	}
	if delivered != 4 {
		t.Errorf("delivered = %d, want 4 (bounded buffer)", delivered)
	}
	got := bus.DroppedEvents() - before
	if got != 12 {
		t.Errorf("DroppedEvents delta = %d, want 12 (16 publishes - 4 buffer slots)", got)
	}
}

// TestMemoryEventBus_DefensiveDeepCopyPayloadOnPublish — caller-side
// mutation of the payload bytes after Publish returns must not bleed
// into the delivered event.
func TestMemoryEventBus_DefensiveDeepCopyPayloadOnPublish(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{OrganizationID: orgID})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	payload := []byte("hello")
	ev := sampleEvent(orgID, "wk", "evt", payload)
	if err := bus.Publish(ctx, ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Mutate the input bytes; the delivered event must not change.
	payload[0] = 'X'

	select {
	case got := <-ch:
		if string(got.Payload) != "hello" {
			t.Errorf("got.Payload = %q, want %q (defensive copy regressed)", got.Payload, "hello")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivery")
	}
}

// TestMemoryEventBus_DefensiveDeepCopyPayloadOnDeliver — consumer-side
// mutation of the delivered payload must not bleed into the next
// matching subscriber.
func TestMemoryEventBus_DefensiveDeepCopyPayloadOnDeliver(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ch1, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{OrganizationID: orgID})
	if err != nil {
		t.Fatalf("Subscribe1: %v", err)
	}
	ch2, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{OrganizationID: orgID})
	if err != nil {
		t.Fatalf("Subscribe2: %v", err)
	}

	if err := bus.Publish(ctx, sampleEvent(orgID, "wk", "evt", []byte("hello"))); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	ev1 := <-ch1
	if len(ev1.Payload) > 0 {
		ev1.Payload[0] = 'X'
	}

	ev2 := <-ch2
	if string(ev2.Payload) != "hello" {
		t.Errorf("ch2 payload = %q, want %q (defensive per-subscriber copy regressed)", ev2.Payload, "hello")
	}
}

// TestMemoryEventBus_DefensiveDeepCopyEventTypesFilter — caller-side
// mutation of the SubscribeFilter.EventTypes slice after Subscribe
// returns must not bleed into the held filter.
func TestMemoryEventBus_DefensiveDeepCopyEventTypesFilter(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	types := []string{"k2k_message_sent"}
	ch, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{
		OrganizationID: orgID,
		EventTypes:     types,
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// Mutate the caller's slice — should not pollute the bus's matcher.
	types[0] = "tool_invoked"

	if err := bus.Publish(ctx, sampleEvent(orgID, "wk", "k2k_message_sent", nil)); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case got := <-ch:
		if got.EventType != "k2k_message_sent" {
			t.Errorf("got.EventType = %q, want k2k_message_sent (filter mutation bled)", got.EventType)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivery")
	}
}

// TestMemoryEventBus_PreCancelledCtxFailsFast — neither Publish nor
// Subscribe must perform any side effect when the supplied ctx is
// already cancelled.
func TestMemoryEventBus_PreCancelledCtxFailsFast(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := bus.Publish(ctx, sampleEvent(orgID, "wk", "evt", nil)); err != context.Canceled {
		t.Errorf("Publish err = %v, want context.Canceled", err)
	}
	if _, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{OrganizationID: orgID}); err != context.Canceled {
		t.Errorf("Subscribe err = %v, want context.Canceled", err)
	}
}

// TestMemoryEventBus_ConcurrentPublishSubscribe — 16 concurrent
// publishers + 4 subscribers must compose without data races (run
// under -race). With a 1024-slot buffer (>16*32 = 512 in-flight) the
// drop counter must stay at zero. The test exists primarily to exercise
// the -race detector on the fan-out path.
func TestMemoryEventBus_ConcurrentPublishSubscribe(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := newMemoryBus(t, peer.WithMemoryBufferSize(1024))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const subscribers = 4
	const publishers = 16
	const perPublisher = 32

	var wg sync.WaitGroup
	chans := make([]<-chan peer.Event, subscribers)
	for i := 0; i < subscribers; i++ {
		ch, _, err := bus.Subscribe(ctx, peer.SubscribeFilter{OrganizationID: orgID})
		if err != nil {
			t.Fatalf("Subscribe[%d]: %v", i, err)
		}
		chans[i] = ch
	}

	wg.Add(publishers)
	for i := 0; i < publishers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perPublisher; j++ {
				_ = bus.Publish(ctx, sampleEvent(orgID, "wk", "evt", nil))
			}
		}()
	}
	wg.Wait()

	// Each subscriber should receive every publish (buffer = 1024 >>
	// total = 512). Drain with a generous deadline.
	expected := publishers * perPublisher
	for _, ch := range chans {
		count := 0
		drainDeadline := time.After(2 * time.Second)
	drainLoop:
		for {
			select {
			case <-ch:
				count++
				if count >= expected {
					break drainLoop
				}
			case <-drainDeadline:
				break drainLoop
			}
		}
		if count != expected {
			t.Errorf("subscriber received %d, want %d", count, expected)
		}
	}
	if got := bus.DroppedEvents(); got != 0 {
		t.Errorf("DroppedEvents = %d, want 0 (buffer = 1024 should absorb 512 publishes)", got)
	}
}

func TestMemoryEventBus_DoublePublishAfterCloseFails(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	bus := peer.NewMemoryEventBus()
	if err := bus.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// A second Close is idempotent.
	if err := bus.Close(); err != nil {
		t.Errorf("second Close err = %v, want nil", err)
	}
	// Publish after Close fails.
	if err := bus.Publish(context.Background(), sampleEvent(orgID, "wk", "evt", nil)); err == nil {
		t.Error("Publish after Close = nil, want error")
	}
	// Subscribe after Close fails.
	if _, _, err := bus.Subscribe(context.Background(), peer.SubscribeFilter{OrganizationID: orgID}); err == nil {
		t.Error("Subscribe after Close = nil, want error")
	}
}
