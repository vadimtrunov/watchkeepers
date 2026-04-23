package publish_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/internal/keep/auth"
	"github.com/vadimtrunov/watchkeepers/core/internal/keep/publish"
)

// newEvent builds a publish.Event with a fresh id and the supplied scope.
// Tests only care about `Scope` for fan-out assertions; other fields are
// populated with deterministic values so the assertions stay readable.
func newEvent(scope string) publish.Event {
	return publish.Event{
		ID:            uuid.New(),
		Scope:         scope,
		AggregateType: "watchkeeper",
		AggregateID:   uuid.New(),
		EventType:     "watchkeeper.spawned",
		Payload:       json.RawMessage(`{"ok":true}`),
		CreatedAt:     time.Unix(1714000000, 0).UTC(),
	}
}

// drain reads up to `want` events from ch into a slice, returning early if
// the context deadline fires. Used so tests never hang on a stuck fan-out.
func drain(ctx context.Context, ch <-chan publish.Event, want int) []publish.Event {
	out := make([]publish.Event, 0, want)
	for len(out) < want {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-ctx.Done():
			return out
		}
	}
	return out
}

// TestRegistry_FanoutToMatchingScope is the happy path: three subscribers,
// one per scope; each receives exactly the events whose Scope matches its
// Claim.Scope and no others. This is the direct AC3 assertion.
func TestRegistry_FanoutToMatchingScope(t *testing.T) {
	reg := publish.NewRegistry(16, time.Hour)
	t.Cleanup(reg.Close)

	userID := uuid.New().String()
	agentID := uuid.New().String()
	userScope := "user:" + userID
	agentScope := "agent:" + agentID

	orgCh, orgUnsub := reg.Subscribe(context.Background(), auth.Claim{Scope: "org"})
	t.Cleanup(orgUnsub)
	userCh, userUnsub := reg.Subscribe(context.Background(), auth.Claim{Scope: userScope})
	t.Cleanup(userUnsub)
	agentCh, agentUnsub := reg.Subscribe(context.Background(), auth.Claim{Scope: agentScope})
	t.Cleanup(agentUnsub)

	orgEv := newEvent("org")
	userEv := newEvent(userScope)
	agentEv := newEvent(agentScope)
	for _, ev := range []publish.Event{orgEv, userEv, agentEv} {
		if err := reg.Publish(context.Background(), ev); err != nil {
			t.Fatalf("Publish(%s): %v", ev.Scope, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	gotOrg := drain(ctx, orgCh, 1)
	gotUser := drain(ctx, userCh, 1)
	gotAgent := drain(ctx, agentCh, 1)

	if len(gotOrg) != 1 || gotOrg[0].ID != orgEv.ID {
		t.Errorf("org subscriber: got %+v, want one event id %s", gotOrg, orgEv.ID)
	}
	if len(gotUser) != 1 || gotUser[0].ID != userEv.ID {
		t.Errorf("user subscriber: got %+v, want one event id %s", gotUser, userEv.ID)
	}
	if len(gotAgent) != 1 || gotAgent[0].ID != agentEv.ID {
		t.Errorf("agent subscriber: got %+v, want one event id %s", gotAgent, agentEv.ID)
	}

	// Cross-scope isolation: no channel should have a second pending event.
	select {
	case ev := <-orgCh:
		t.Errorf("org subscriber received cross-scope event: %+v", ev)
	case ev := <-userCh:
		t.Errorf("user subscriber received cross-scope event: %+v", ev)
	case ev := <-agentCh:
		t.Errorf("agent subscriber received cross-scope event: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestRegistry_NoHierarchyWidening guards AC3's "exact string match" clause:
// an `org` subscriber MUST NOT see `user:*` or `agent:*` events, and a
// `user:<uuidA>` subscriber MUST NOT see `user:<uuidB>` events.
func TestRegistry_NoHierarchyWidening(t *testing.T) {
	reg := publish.NewRegistry(8, time.Hour)
	t.Cleanup(reg.Close)

	userA := "user:" + uuid.New().String()
	userB := "user:" + uuid.New().String()

	orgCh, orgUnsub := reg.Subscribe(context.Background(), auth.Claim{Scope: "org"})
	t.Cleanup(orgUnsub)
	userACh, userAUnsub := reg.Subscribe(context.Background(), auth.Claim{Scope: userA})
	t.Cleanup(userAUnsub)

	// Publish only non-matching events from each subscriber's perspective.
	if err := reg.Publish(context.Background(), newEvent(userA)); err != nil {
		t.Fatalf("publish userA: %v", err)
	}
	if err := reg.Publish(context.Background(), newEvent(userB)); err != nil {
		t.Fatalf("publish userB: %v", err)
	}
	if err := reg.Publish(context.Background(), newEvent("agent:"+uuid.New().String())); err != nil {
		t.Fatalf("publish agent: %v", err)
	}

	// org must have seen nothing.
	select {
	case ev := <-orgCh:
		t.Errorf("org saw non-org event: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
	// userA must have received the userA event but not userB.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := drain(ctx, userACh, 1)
	if len(got) != 1 || got[0].Scope != userA {
		t.Errorf("userA got %+v, want exactly one userA event", got)
	}
	select {
	case ev := <-userACh:
		t.Errorf("userA saw non-matching event: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestRegistry_DropOnFullBuffer verifies AC4: when a subscriber's buffer
// fills, it is dropped (channel closed) and Publish still returns nil; a
// second independent subscriber on the same scope keeps receiving.
func TestRegistry_DropOnFullBuffer(t *testing.T) {
	reg := publish.NewRegistry(1, time.Hour)
	t.Cleanup(reg.Close)

	slowCh, slowUnsub := reg.Subscribe(context.Background(), auth.Claim{Scope: "org"})
	t.Cleanup(slowUnsub)
	// Second subscriber on the SAME scope; we drain it synchronously
	// between publishes so its 1-slot buffer never fills (otherwise the
	// drop-on-full logic would mark it too, which is not what this test
	// is asserting).
	liveCh, liveUnsub := reg.Subscribe(context.Background(), auth.Claim{Scope: "org"})
	t.Cleanup(liveUnsub)

	first := newEvent("org")
	second := newEvent("org")

	// Publish #1: both subscribers have empty buffers, both accept.
	if err := reg.Publish(context.Background(), first); err != nil {
		t.Fatalf("publish first: %v", err)
	}
	// Drain liveCh so publish #2 has room for the second event.
	select {
	case ev := <-liveCh:
		if ev.ID != first.ID {
			t.Errorf("liveCh[0].ID = %s, want %s", ev.ID, first.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("liveCh did not receive first event")
	}

	// Publish #2: slowCh is full (never drained) -> dropped + closed;
	// liveCh was drained, so it accepts the event.
	if err := reg.Publish(context.Background(), second); err != nil {
		t.Fatalf("publish second: %v", err)
	}

	// slowCh should observe exactly one event (the first) and then EOF.
	got := make([]publish.Event, 0, 2)
	drainTimeout := time.After(time.Second)
slowLoop:
	for {
		select {
		case ev, ok := <-slowCh:
			if !ok {
				break slowLoop
			}
			got = append(got, ev)
		case <-drainTimeout:
			t.Fatalf("slow subscriber did not EOF; got %d events", len(got))
		}
	}
	if len(got) != 1 || got[0].ID != first.ID {
		t.Errorf("slow subscriber got %+v, want exactly [first]", got)
	}

	// liveCh must still see the second event.
	select {
	case ev := <-liveCh:
		if ev.ID != second.ID {
			t.Errorf("liveCh[1].ID = %s, want %s", ev.ID, second.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("liveCh did not receive second event")
	}
}

// TestRegistry_CloseBroadcasts verifies AC6: Close() causes every active
// subscription channel to be closed (handlers return). A single Publish
// after Close is a no-op but does not panic.
func TestRegistry_CloseBroadcasts(t *testing.T) {
	reg := publish.NewRegistry(4, time.Hour)

	ch1, _ := reg.Subscribe(context.Background(), auth.Claim{Scope: "org"})
	ch2, _ := reg.Subscribe(context.Background(), auth.Claim{Scope: "org"})

	reg.Close()

	for i, ch := range []<-chan publish.Event{ch1, ch2} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Errorf("ch[%d]: expected closed channel, got value", i)
			}
		case <-time.After(time.Second):
			t.Fatalf("ch[%d]: channel did not close after Close()", i)
		}
	}

	// Idempotent: a second Close() must not panic.
	reg.Close()

	// Publish after Close is a no-op and must not block or panic.
	if err := reg.Publish(context.Background(), newEvent("org")); err != nil {
		t.Errorf("Publish after Close returned %v, want nil (no-op)", err)
	}
}

// TestRegistry_CloseReleasesWatchdogs is the regression guard for the
// per-subscription watchdog leak: Subscribe spawns a goroutine that used
// to park on `<-ctx.Done()`, so callers that passed context.Background()
// (including every test above that relies on t.Cleanup(reg.Close)) left
// one parked goroutine per Subscribe. Close must now release every
// watchdog immediately via the internal done channel.
//
// We use the package-internal WaitWatchdogs (export_test.go) rather than
// a full goleak dependency because adding a new module dependency just
// to satisfy this assertion was judged heavier than a tiny test seam.
func TestRegistry_CloseReleasesWatchdogs(t *testing.T) {
	reg := publish.NewRegistry(4, time.Hour)

	// Several subscribers, all with a context that will never fire so the
	// only way their watchdogs can exit is via Close().
	const subscribers = 5
	for i := 0; i < subscribers; i++ {
		_, _ = reg.Subscribe(context.Background(), auth.Claim{Scope: "org"})
	}

	reg.Close()

	// WaitWatchdogs blocks until the internal WaitGroup drains. Run it in
	// a goroutine so the test can enforce a wall-clock budget (200ms is
	// comfortably above scheduler jitter but far below the old "forever"
	// failure mode).
	exited := make(chan struct{})
	go func() {
		reg.WaitWatchdogs()
		close(exited)
	}()
	select {
	case <-exited:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Close() did not release watchdogs within 200ms")
	}
}

// TestRegistry_UnsubscribeRemoves guards the lifecycle: calling the
// unsubscribe func closes the channel and a subsequent Publish skips the
// subscriber (non-blocking send would otherwise panic on a closed chan).
func TestRegistry_UnsubscribeRemoves(t *testing.T) {
	reg := publish.NewRegistry(4, time.Hour)
	t.Cleanup(reg.Close)

	ch, unsub := reg.Subscribe(context.Background(), auth.Claim{Scope: "org"})
	unsub()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected closed channel after unsubscribe")
		}
	case <-time.After(time.Second):
		t.Fatal("channel did not close after unsubscribe")
	}

	// Publish after unsubscribe must not panic on the closed channel.
	if err := reg.Publish(context.Background(), newEvent("org")); err != nil {
		t.Errorf("Publish after unsubscribe: %v", err)
	}

	// Double-unsubscribe is idempotent.
	unsub()
}

// TestRegistry_ContextCancelRemoves verifies that cancelling the per-
// subscription context drops the subscriber so a subsequent re-subscribe
// with the same claim keeps receiving later events (AC3 edge case).
func TestRegistry_ContextCancelRemoves(t *testing.T) {
	reg := publish.NewRegistry(4, time.Hour)
	t.Cleanup(reg.Close)

	ctx, cancel := context.WithCancel(context.Background())
	ch, _ := reg.Subscribe(ctx, auth.Claim{Scope: "org"})
	cancel()

	select {
	case _, ok := <-ch:
		if ok {
			t.Error("expected closed channel after ctx cancel")
		}
	case <-time.After(time.Second):
		t.Fatal("channel did not close after ctx cancel")
	}

	// Re-subscribe with the same scope: must receive new events.
	ch2, unsub2 := reg.Subscribe(context.Background(), auth.Claim{Scope: "org"})
	t.Cleanup(unsub2)
	ev := newEvent("org")
	if err := reg.Publish(context.Background(), ev); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case got := <-ch2:
		if got.ID != ev.ID {
			t.Errorf("ch2 got %s, want %s", got.ID, ev.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("re-subscribed channel did not receive event")
	}
}

// TestRegistry_ConcurrentPublishSubscribe is a race-detector smoke test:
// many publishers and subscribers run concurrently, exercising the lock
// discipline inside Publish/Subscribe/Close. Failure mode is a race or
// panic under `go test -race` rather than a specific count assertion.
func TestRegistry_ConcurrentPublishSubscribe(t *testing.T) {
	reg := publish.NewRegistry(32, time.Hour)
	t.Cleanup(reg.Close)

	const subscribers = 16
	const publishes = 200
	var wg sync.WaitGroup

	wg.Add(subscribers)
	for i := 0; i < subscribers; i++ {
		go func() {
			defer wg.Done()
			ch, unsub := reg.Subscribe(context.Background(), auth.Claim{Scope: "org"})
			defer unsub()
			deadline := time.After(2 * time.Second)
			for {
				select {
				case _, ok := <-ch:
					if !ok {
						return
					}
				case <-deadline:
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < publishes; i++ {
			_ = reg.Publish(context.Background(), newEvent("org"))
		}
	}()

	wg.Wait()
}
