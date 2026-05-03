package notebook

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flakyStore is a [Storer] stand-in for [TestPeriodicBackup_TickFailureContinues]:
// every odd-numbered Put call (1st, 3rd, …) returns errOnOdd; every
// even-numbered call succeeds with `nextURI`. The toggle exercises the
// AC2 promise that a tick failure does NOT exit the loop — the next tick
// must still fire and a healthy Put must still land.
type flakyStore struct {
	errOnOdd error
	nextURI  string
	called   atomic.Int32
}

func (f *flakyStore) Put(_ context.Context, agentID string, src io.Reader) (string, error) {
	n := f.called.Add(1)
	// Drain src so the Archive goroutine on the producer side completes
	// regardless of the success/failure branch (matches a well-behaved
	// real ArchiveStore that consumes the stream before deciding).
	if _, err := io.Copy(io.Discard, src); err != nil {
		return "", err
	}
	if n%2 == 1 {
		return "", f.errOnOdd
	}
	if f.nextURI != "" {
		return f.nextURI, nil
	}
	return "fake://test/" + agentID + "/snap.tar.gz", nil
}

// TestPeriodicBackup_HappyPath — AC2/AC5: cadence=10ms; let the loop run
// for ~30ms; cancel ctx; assert at least 2 ticks fired (store.putCalled
// ≥2; logger.called ≥2; logger received at least one event with
// EventType=="notebook_backed_up"); returns ctx.Err() wrapping
// context.Canceled.
func TestPeriodicBackup_HappyPath(t *testing.T) {
	src, parentCtx, _ := freshDB(t)
	seedRetireDB(parentCtx, t, src)

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	store := &fakeStore{nextURI: "fake://test/backup.tar.gz"}
	logger := &fakeLogger{}

	done := make(chan error, 1)
	go func() {
		done <- PeriodicBackup(ctx, src, retireAgentID, store, logger, 25*time.Millisecond, nil)
	}()

	// Wait until at least 2 ticks have fired end-to-end (logger.called is
	// the last step of the pipeline, so polling on it ensures the prior
	// Put has also landed). Polling instead of a fixed sleep keeps the
	// test robust under -race jitter.
	deadline := time.Now().Add(3 * time.Second)
	for logger.called.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("PeriodicBackup returned nil error on ctx cancel")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want one wrapping context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PeriodicBackup did not return within 2s of ctx cancel")
	}

	if got := store.putCalled.Load(); got < 2 {
		t.Fatalf("store.Put called %d times, want >= 2", got)
	}
	if got := logger.called.Load(); got < 2 {
		t.Fatalf("logger.LogAppend called %d times, want >= 2", got)
	}
	if logger.received.EventType != backupEventType {
		t.Fatalf("event_type = %q, want %q", logger.received.EventType, backupEventType)
	}
}

// tickResult captures one onTick invocation for [TestPeriodicBackup_TickFailureContinues].
type tickResult struct {
	uri string
	err error
}

// classifyTicks returns whether the captured tick log contains at least
// one error tick AND at least one success tick. Extracted from
// [TestPeriodicBackup_TickFailureContinues] to keep that test body under
// the gocyclo threshold.
func classifyTicks(ticks []tickResult) (sawErr, sawOK bool) {
	for _, t := range ticks {
		if t.err != nil {
			sawErr = true
		}
		if t.err == nil && t.uri != "" {
			sawOK = true
		}
	}
	return sawErr, sawOK
}

// TestPeriodicBackup_TickFailureContinues — AC2/AC5: store.putErr triggers
// on calls 1 and 3 but not 2; cadence=5ms; run for ~20ms; assert tick 2
// succeeded (uri non-empty in onTick callback for at least one tick) AND
// ticks 1+3 surfaced errors via onTick; loop did NOT exit early.
func TestPeriodicBackup_TickFailureContinues(t *testing.T) {
	src, parentCtx, _ := freshDB(t)
	seedRetireDB(parentCtx, t, src)

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	// Toggle put error per call. Calls 1, 3, 5, ... fail; calls 2, 4, ...
	// succeed. The exact tick-count is jittery on CI but as long as we
	// observe both an error tick AND a success tick the AC is satisfied.
	store := &flakyStore{
		errOnOdd: errors.New("flaky put"),
		nextURI:  "fake://test/flaky.tar.gz",
	}
	logger := &fakeLogger{}

	var (
		mu    sync.Mutex
		ticks []tickResult
	)
	onTick := func(uri string, err error) {
		mu.Lock()
		defer mu.Unlock()
		ticks = append(ticks, tickResult{uri: uri, err: err})
	}

	done := make(chan error, 1)
	go func() {
		done <- PeriodicBackup(ctx, src, retireAgentID, store, logger, 15*time.Millisecond, onTick)
	}()

	// Poll until we observe at least one odd (error) tick AND one even
	// (success) tick. Polling rather than a fixed sleep keeps the test
	// robust under -race jitter on slow CI.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		sawErr, sawOK := classifyTicks(ticks)
		mu.Unlock()
		if sawErr && sawOK {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want one wrapping context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PeriodicBackup did not return within 2s of ctx cancel")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(ticks) < 2 {
		t.Fatalf("observed %d ticks via callback, want >= 2", len(ticks))
	}
	sawErr, sawOK := classifyTicks(ticks)
	if !sawErr {
		t.Fatalf("expected at least one tick to surface an error via onTick; ticks=%+v", ticks)
	}
	if !sawOK {
		t.Fatalf("expected at least one tick to succeed (loop did not skip recovery); ticks=%+v", ticks)
	}
}

// TestPeriodicBackup_BadCadence — AC1/AC5: cadence=0 and cadence=-1 both
// → ErrInvalidCadence synchronously, no goroutines started (we can only
// check observable side-effects: store.putCalled must remain 0).
func TestPeriodicBackup_BadCadence(t *testing.T) {
	src, ctx, _ := freshDB(t)
	seedRetireDB(ctx, t, src)

	store := &fakeStore{}
	logger := &fakeLogger{}

	for _, cadence := range []time.Duration{0, -1, -time.Hour} {
		err := PeriodicBackup(ctx, src, retireAgentID, store, logger, cadence, nil)
		if !errors.Is(err, ErrInvalidCadence) {
			t.Fatalf("cadence=%v: err = %v, want ErrInvalidCadence", cadence, err)
		}
	}
	if got := store.putCalled.Load(); got != 0 {
		t.Fatalf("store.Put called %d times for bad cadence, want 0", got)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times for bad cadence, want 0", got)
	}
}

// TestPeriodicBackup_BadAgentID — AC1/AC5: non-UUID id → ErrInvalidEntry
// synchronously.
func TestPeriodicBackup_BadAgentID(t *testing.T) {
	src, ctx, _ := freshDB(t)
	seedRetireDB(ctx, t, src)

	store := &fakeStore{}
	logger := &fakeLogger{}

	err := PeriodicBackup(ctx, src, "not-a-uuid", store, logger, 10*time.Millisecond, nil)
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("err = %v, want ErrInvalidEntry", err)
	}
	if got := store.putCalled.Load(); got != 0 {
		t.Fatalf("store.Put called %d times for bad agent id, want 0", got)
	}
	if got := logger.called.Load(); got != 0 {
		t.Fatalf("logger.LogAppend called %d times for bad agent id, want 0", got)
	}
}

// TestPeriodicBackup_NilOnTick — AC2/AC5: onTick=nil works without
// panicking; loop ticks happen but no callback observed (assert via
// store counter).
func TestPeriodicBackup_NilOnTick(t *testing.T) {
	src, parentCtx, _ := freshDB(t)
	seedRetireDB(parentCtx, t, src)

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	store := &fakeStore{nextURI: "fake://test/nil-tick.tar.gz"}
	logger := &fakeLogger{}

	done := make(chan error, 1)
	go func() {
		done <- PeriodicBackup(ctx, src, retireAgentID, store, logger, 25*time.Millisecond, nil)
	}()

	// Poll until at least 2 ticks have fired end-to-end (logger.called
	// increments at the end of archiveAndAudit so it lags putCalled by
	// the LogAppend duration).
	deadline := time.Now().Add(3 * time.Second)
	for logger.called.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want one wrapping context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PeriodicBackup did not return within 2s of ctx cancel")
	}

	if got := store.putCalled.Load(); got < 2 {
		t.Fatalf("store.Put called %d times, want >= 2", got)
	}
}

// TestPeriodicBackup_CtxCancelMidTick — AC2/AC5: store.Put blocks on a
// channel; cancel ctx while the first tick is in-flight; assert
// PeriodicBackup returns context.Canceled-wrapped error and the in-flight
// Put was unblocked (the goroutine-leak fix from M2b.4 carries through
// archiveAndAudit).
func TestPeriodicBackup_CtxCancelMidTick(t *testing.T) {
	src, parentCtx, _ := freshDB(t)
	seedRetireDB(parentCtx, t, src)

	ctx, cancel := context.WithCancel(parentCtx)
	store := &fakeStore{putBlock: make(chan struct{})}
	logger := &fakeLogger{}

	done := make(chan error, 1)
	go func() {
		done <- PeriodicBackup(ctx, src, retireAgentID, store, logger, 5*time.Millisecond, nil)
	}()

	// Wait for the first tick to enter Put and start blocking.
	deadline := time.Now().Add(500 * time.Millisecond)
	for store.putCalled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if store.putCalled.Load() == 0 {
		cancel()
		t.Fatal("first tick did not enter Put within 500ms")
	}

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("PeriodicBackup returned nil error on ctx cancel")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want one wrapping context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PeriodicBackup did not return within 2s of ctx cancel; goroutine likely leaked")
	}
}

// TestPeriodicBackup_EventTypeIsBackedUp — AC2/AC5: assert
// fakeLogger.received.EventType == "notebook_backed_up" (not
// "notebook_archived" — verifies the helper extraction didn't accidentally
// hardcode the retire event type).
func TestPeriodicBackup_EventTypeIsBackedUp(t *testing.T) {
	src, parentCtx, _ := freshDB(t)
	seedRetireDB(parentCtx, t, src)

	ctx, cancel := context.WithCancel(parentCtx)
	defer cancel()

	store := &fakeStore{nextURI: "fake://test/event-type.tar.gz"}
	logger := &fakeLogger{}

	done := make(chan error, 1)
	go func() {
		done <- PeriodicBackup(ctx, src, retireAgentID, store, logger, 5*time.Millisecond, nil)
	}()

	// Wait for at least one tick to complete.
	deadline := time.Now().Add(500 * time.Millisecond)
	for logger.called.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	cancel()
	<-done

	if got := logger.called.Load(); got == 0 {
		t.Fatal("logger.LogAppend never called")
	}
	if logger.received.EventType != "notebook_backed_up" {
		t.Fatalf("event_type = %q, want %q", logger.received.EventType, "notebook_backed_up")
	}
	if logger.received.EventType == retireEventType {
		t.Fatalf("event_type = %q, helper accidentally hardcoded retire event type", logger.received.EventType)
	}
}
