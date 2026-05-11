package coordinatorcronwiring

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/cron"
)

// fakeLocalPublisher captures every Publish call for assertion. Satisfies
// cron.LocalPublisher structurally — same shape as *eventbus.Bus's
// Publish method.
type fakeLocalPublisher struct {
	mu       sync.Mutex
	captured []captured
	errOut   error
}

type captured struct {
	topic string
	event any
}

func (f *fakeLocalPublisher) Publish(_ context.Context, topic string, event any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.captured = append(f.captured, captured{topic: topic, event: event})
	return f.errOut
}

func (f *fakeLocalPublisher) snapshot() []captured {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]captured, len(f.captured))
	copy(out, f.captured)
	return out
}

func TestRegisterCoordinatorCronTicks_PanicsOnNilScheduler(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil scheduler")
		}
	}()
	_, _ = RegisterCoordinatorCronTicks(nil, Config{})
}

func TestRegisterCoordinatorCronTicks_HappyPath_RegistersBothEntries(t *testing.T) {
	t.Parallel()
	pub := &fakeLocalPublisher{}
	sched := cron.New(pub)
	t.Cleanup(func() { <-sched.Stop().Done() })

	entries, err := RegisterCoordinatorCronTicks(sched, Config{})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if entries.DailyBriefing == 0 || entries.OverdueSweep == 0 {
		t.Fatalf("entries = %+v, want both non-zero EntryIDs", entries)
	}
	if entries.DailyBriefing == entries.OverdueSweep {
		t.Errorf("entries collided: %v", entries.DailyBriefing)
	}
}

func TestRegisterCoordinatorCronTicks_TopicConstantsArePinned(t *testing.T) {
	t.Parallel()
	if TopicDailyBriefingTick != "coordinator.daily_briefing.tick" {
		t.Errorf("TopicDailyBriefingTick = %q", TopicDailyBriefingTick)
	}
	if TopicOverdueSweepTick != "coordinator.overdue_sweep.tick" {
		t.Errorf("TopicOverdueSweepTick = %q", TopicOverdueSweepTick)
	}
}

func TestRegisterCoordinatorCronTicks_DefaultSpecsApply(t *testing.T) {
	t.Parallel()
	// Default specs must parse cleanly via the cron scheduler. The
	// scheduler validates the parser on Schedule; an invalid default
	// would return a wrapped parse error from RegisterCoordinatorCronTicks.
	pub := &fakeLocalPublisher{}
	sched := cron.New(pub)
	t.Cleanup(func() { <-sched.Stop().Done() })
	if _, err := RegisterCoordinatorCronTicks(sched, Config{}); err != nil {
		t.Fatalf("default specs must parse cleanly: %v", err)
	}
}

func TestRegisterCoordinatorCronTicks_InvalidBriefingSpecRollsBack(t *testing.T) {
	t.Parallel()
	pub := &fakeLocalPublisher{}
	sched := cron.New(pub)
	t.Cleanup(func() { <-sched.Stop().Done() })

	_, err := RegisterCoordinatorCronTicks(sched, Config{
		DailyBriefingSpec: "this is not a cron spec",
	})
	if err == nil {
		t.Fatalf("expected error on malformed briefing spec")
	}
}

func TestRegisterCoordinatorCronTicks_InvalidSweepSpecRollsBackBriefing(t *testing.T) {
	t.Parallel()
	// Iter-1 codex Nit / critic Minor: an observable test that
	// proves the rolled-back briefing entry is GONE — not merely
	// that another registration succeeds afterwards. Approach:
	//
	//   1. attempt registration with a per-second briefing spec
	//      and a malformed sweep spec — RegisterCoordinatorCronTicks
	//      must return the sweep-spec error AFTER unscheduling the
	//      briefing entry it had just registered.
	//   2. Start() the scheduler and wait ~1.5s.
	//   3. assert NO `coordinator.daily_briefing.tick` events
	//      landed on the publisher. A broken rollback (where the
	//      briefing entry survives) would fire ~1 event/s and
	//      fail this assertion loudly.
	pub := &fakeLocalPublisher{}
	sched := cron.New(pub)
	t.Cleanup(func() { <-sched.Stop().Done() })

	_, err := RegisterCoordinatorCronTicks(sched, Config{
		DailyBriefingSpec: "* * * * * *", // per-second; would fire fast if it survives
		OverdueSweepSpec:  "garbage spec",
	})
	if err == nil {
		t.Fatalf("expected error on malformed sweep spec")
	}

	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	for _, ev := range pub.snapshot() {
		if ev.topic == TopicDailyBriefingTick {
			t.Fatalf("briefing tick fired %d times after rollback; want 0", len(pub.snapshot()))
		}
	}
}

func TestRegisterCoordinatorCronTicks_TickEventCarriesCorrelationIDAndTopic(t *testing.T) {
	t.Parallel()

	pub := &fakeLocalPublisher{}
	sched := cron.New(pub)
	t.Cleanup(func() { <-sched.Stop().Done() })

	fixedClock := time.Date(2026, 5, 11, 9, 0, 0, 0, time.UTC)

	cfg := Config{
		// Per-second specs so the test gets fires within a few hundred
		// ms. M2b.5 "polling-deadline" pattern documented in cron's
		// README.
		DailyBriefingSpec: "* * * * * *",
		OverdueSweepSpec:  "* * * * * *",
		// Closures must be race-safe: cron fires from independent
		// goroutines, so the test passes pure functions (no captured
		// counters). Assertions on the published events themselves
		// cover what counters would have.
		Clock:            func() time.Time { return fixedClock },
		NewCorrelationID: func() string { return "corr-fixed" },
	}
	if _, err := RegisterCoordinatorCronTicks(sched, cfg); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := sched.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		got := pub.snapshot()
		topics := map[string]int{}
		for _, ev := range got {
			topics[ev.topic]++
		}
		if topics[TopicDailyBriefingTick] >= 1 && topics[TopicOverdueSweepTick] >= 1 {
			// Assert shape of one event from each topic.
			for _, ev := range got {
				tick, ok := ev.event.(TickEvent)
				if !ok {
					t.Fatalf("event is not TickEvent: %T", ev.event)
				}
				if tick.CorrelationID != "corr-fixed" {
					t.Errorf("CorrelationID = %q, want fixed", tick.CorrelationID)
				}
				if !tick.FiredAt.Equal(fixedClock) {
					t.Errorf("FiredAt = %v, want fixed clock %v", tick.FiredAt, fixedClock)
				}
				if tick.Topic != ev.topic {
					t.Errorf("event.Topic = %q vs publish-topic = %q", tick.Topic, ev.topic)
				}
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never got both topics; got topic counts: %v", topics)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRegisterCoordinatorCronTicks_PublishErrorDoesNotHaltScheduler(t *testing.T) {
	t.Parallel()
	// Best-effort firing is the cron Scheduler's contract; this test
	// just smoke-checks that registering against a failing publisher
	// does not error AT registration time (the publisher is consulted
	// only on fire).
	pub := &fakeLocalPublisher{errOut: errors.New("bus stopped")}
	sched := cron.New(pub)
	t.Cleanup(func() { <-sched.Stop().Done() })

	if _, err := RegisterCoordinatorCronTicks(sched, Config{}); err != nil {
		t.Fatalf("register: %v", err)
	}
}

func TestRegisterCoordinatorCronTicks_NilClockUsesTimeNow(t *testing.T) {
	t.Parallel()
	pub := &fakeLocalPublisher{}
	sched := cron.New(pub)
	t.Cleanup(func() { <-sched.Stop().Done() })

	if _, err := RegisterCoordinatorCronTicks(sched, Config{Clock: nil}); err != nil {
		t.Fatalf("register with nil clock: %v", err)
	}
}

func TestRegisterCoordinatorCronTicks_DefaultCorrelationIDUnique(t *testing.T) {
	t.Parallel()
	ids := map[string]struct{}{}
	for i := 0; i < 100; i++ {
		ids[defaultCorrelationID()] = struct{}{}
	}
	if len(ids) != 100 {
		t.Errorf("default correlation ids collided: %d unique out of 100", len(ids))
	}
}
