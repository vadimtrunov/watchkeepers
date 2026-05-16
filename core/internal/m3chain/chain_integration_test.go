package m3chain_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/cron"
	"github.com/vadimtrunov/watchkeepers/core/pkg/eventbus"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/lifecycle"
)

// chainTopic is the bus topic name used by the integration scenario.
// Pinned as a const so factory + handler agree on a single string.
const chainTopic = "watchkeeper.cron.tick"

// chainFireSpec is the cron spec used by the integration tests. Every
// second so each test can observe a fire within ~1.5 s wall-clock
// without resorting to private cron hooks. The Scheduler is constructed
// with WithSeconds() inside cron.New, so the 6-field form is required.
const chainFireSpec = "* * * * * *"

// chainWaitTimeout caps how long any test waits for a handler signal.
// Sized to absorb CI jitter (~5 s) while still failing reasonably fast
// if the chain is broken.
const chainWaitTimeout = 5 * time.Second

// tickEvent is the event payload published onto the bus by the cron
// factory. Mirrors the production `TickEvent` shape in
// `core/internal/keep/coordinator_cron_wiring/wiring.go` — correlation
// id rides on the payload, NOT on ctx, because `cron.Scheduler` passes
// its runCtx unchanged to both factory and publisher, so a factory
// cannot enrich the ctx the handler eventually receives.
type tickEvent struct {
	CorrelationID string
	WatchkeeperID string
	FiredAt       time.Time
}

// fakeKeepLogClient is the hand-rolled [keeperslog.LocalKeepClient]
// stand-in used by the integration scenario. Records every LogAppend
// call (request value only — the request carries all fields the
// assertions exercise). Pattern mirrors the keeperslog package's own
// `fakeKeepClient` (writer_test.go); the divergence from that fake is
// the per-row monotonic `nextRowN` so the assertion can distinguish
// successive appends (the upstream fake returns the same "fake-log-id"
// every time, which is fine for unit tests in writer_test.go but
// hides ordering bugs in a chain scenario).
type fakeKeepLogClient struct {
	mu       sync.Mutex
	calls    []keepclient.LogAppendRequest
	nextRowN int
}

// LogAppend records the call and returns a fresh row id each time so
// callers can distinguish appends in the order they happened.
func (f *fakeKeepLogClient) LogAppend(_ context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	f.nextRowN++
	return &keepclient.LogAppendResponse{ID: fmt.Sprintf("fake-log-row-%d", f.nextRowN)}, nil
}

// recordedCalls returns a defensive copy of the recorded request log.
// Always taken under the mutex so a still-firing scheduler cannot race
// the assertion goroutine.
func (f *fakeKeepLogClient) recordedCalls() []keepclient.LogAppendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keepclient.LogAppendRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeLifecycleClient is the hand-rolled [lifecycle.LocalKeepClient]
// stand-in. It implements only the surface lifecycle.Manager actually
// touches in the integration scenario — Insert + UpdateStatus paths
// (Spawn) are exercised; Get / List are stubbed to satisfy the
// interface but return ErrNotImplemented to fail loudly if any future
// test accidentally relies on them.
type fakeLifecycleClient struct {
	mu      sync.Mutex
	rows    map[string]*keepclient.Watchkeeper
	clock   func() time.Time
	nextRow int
}

func newFakeLifecycleClient(clock func() time.Time) *fakeLifecycleClient {
	if clock == nil {
		clock = time.Now
	}
	return &fakeLifecycleClient{
		rows:  make(map[string]*keepclient.Watchkeeper),
		clock: clock,
	}
}

func (f *fakeLifecycleClient) InsertWatchkeeper(_ context.Context, req keepclient.InsertWatchkeeperRequest) (*keepclient.InsertWatchkeeperResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextRow++
	id := fmt.Sprintf("watchkeeper-%d", f.nextRow)
	now := f.clock().UTC()
	row := &keepclient.Watchkeeper{
		ID:          id,
		ManifestID:  req.ManifestID,
		LeadHumanID: req.LeadHumanID,
		Status:      "pending",
		CreatedAt:   now,
	}
	if req.ActiveManifestVersionID != "" {
		v := req.ActiveManifestVersionID
		row.ActiveManifestVersionID = &v
	}
	f.rows[id] = row
	return &keepclient.InsertWatchkeeperResponse{ID: id}, nil
}

func (f *fakeLifecycleClient) UpdateWatchkeeperStatus(_ context.Context, id, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[id]
	if !ok {
		return fmt.Errorf("fakeLifecycleClient: watchkeeper %q not found", id)
	}
	row.Status = status
	if status == "active" {
		t := f.clock().UTC()
		row.SpawnedAt = &t
	}
	return nil
}

func (f *fakeLifecycleClient) GetWatchkeeper(ctx context.Context, id string) (*keepclient.Watchkeeper, error) {
	// Honour ctx.Done() up-front so a future test that passes a cancelled
	// ctx into the chain observes the same behaviour the production
	// keepclient (HTTP round-trip) would surface — `context.Canceled`
	// rather than a stale successful read. Mirrors the production
	// keepclient.Client.do path which wraps net/http through ctx.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[id]
	if !ok {
		return nil, fmt.Errorf("fakeLifecycleClient: watchkeeper %q not found", id)
	}
	// Value-copy via dereference avoids handing the caller a pointer
	// into the fake's internal map — a later mutation by the test (or by
	// a concurrent UpdateStatus) cannot then race the caller's read.
	out := *row
	return &out, nil
}

func (f *fakeLifecycleClient) ListWatchkeepers(_ context.Context, _ keepclient.ListWatchkeepersRequest) (*keepclient.ListWatchkeepersResponse, error) {
	return nil, fmt.Errorf("fakeLifecycleClient.ListWatchkeepers: not used by m3chain tests")
}

// envelope captures the JSON shape keeperslog.Writer emits as the
// `payload` column. We assert on `event_id` (per-event unique) and
// `data` (the caller-supplied payload).
type envelope struct {
	EventID string          `json:"event_id"`
	Data    json.RawMessage `json:"data"`
}

// chainEventData is the test-local schema for the `data` envelope key.
// The factory writes it on the "cron_fired" event; the handler reads
// the correlation id off the bus event and writes it (plus the
// watchkeeper id, plus a tag identifying which side wrote) on the
// "handler_ran" event. Asserting on these fields gives end-to-end
// coverage: a missing watchkeeper id or a mismatched correlation id
// fails the test loudly.
type chainEventData struct {
	WatchkeeperID string `json:"watchkeeper_id"`
	FiredAtRFC    string `json:"fired_at,omitempty"`
	Side          string `json:"side"`
}

// fixedClock returns a deterministic clock function pinned to t. Both
// the keeperslog Writer and the fakeLifecycleClient consume it so the
// envelope's `timestamp` field is reproducible across runs without
// being load-bearing for the integration assertion (which keys on
// correlation id, not time).
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// chainHarness bundles the wired components so each subtest can build a
// fresh, isolated chain. Returning the writer alongside the scheduler +
// bus lets tests directly Append from the factory closure without
// re-wiring the writer per test. The exported clock field is the same
// function the keeperslog writer + fakeLifecycleClient consume, so the
// cron factory can stamp deterministic `fired_at` timestamps on the
// event payload (review iter-1 #4: avoids mixing real and fixed time in
// the same test).
type chainHarness struct {
	logClient *fakeKeepLogClient
	lifeMgr   *lifecycle.Manager
	bus       *eventbus.Bus
	sched     *cron.Scheduler
	writer    *keeperslog.Writer
	clock     func() time.Time
}

// newChainHarness assembles a fresh chain: fake clients, a bus, a
// scheduler publishing onto the bus, a keeperslog writer over the fake
// log client, and a lifecycle manager over the fake lifecycle client.
// Each component is constructed exactly once per test so concurrent
// state (subscriber lists, recorded calls) cannot leak across runs.
//
// Cleanup ordering (LIFO): the t.Cleanup chain runs in REVERSE
// registration order, so `sched.Stop()` runs BEFORE `bus.Close()`.
// That matches the safe shutdown sequence: stop publishing (no new
// envelopes enter any topic queue) THEN close the bus (drain in-flight
// envelopes through their handlers). Reordering the two Cleanup
// registrations here would invert that — bus would close while cron
// is still firing, and the next fire's Publish would surface
// `eventbus.ErrClosed` to the test's logger.
func newChainHarness(t testing.TB) *chainHarness {
	t.Helper()
	clock := fixedClock(time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC))
	logClient := &fakeKeepLogClient{}
	lifeClient := newFakeLifecycleClient(clock)
	bus := eventbus.New()
	t.Cleanup(func() { _ = bus.Close() })
	sched := cron.New(bus)
	t.Cleanup(func() { _ = sched.Stop() })
	writer := keeperslog.New(logClient, keeperslog.WithClock(clock))
	mgr := lifecycle.New(lifeClient)
	return &chainHarness{
		logClient: logClient,
		lifeMgr:   mgr,
		bus:       bus,
		sched:     sched,
		writer:    writer,
		clock:     clock,
	}
}

// spawnMockWatchkeeper drives lifecycle.Manager.Spawn against the
// fakeLifecycleClient and returns the resulting watchkeeper id. The
// id flows into the cron factory + handler so every keeperslog row
// carries the spawned watchkeeper's identity in its payload — the
// bullet 256 phrase "spawn a mock Watchkeeper" is satisfied by an
// actual lifecycle round-trip, not a hand-crafted constant.
func spawnMockWatchkeeper(t *testing.T, h *chainHarness) string {
	t.Helper()
	id, err := h.lifeMgr.Spawn(context.Background(), lifecycle.SpawnParams{
		ManifestID:  uuid.NewString(),
		LeadHumanID: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("spawnMockWatchkeeper: %v", err)
	}
	if id == "" {
		t.Fatalf("spawnMockWatchkeeper: empty id")
	}
	return id
}

// makeFactory builds the cron EventFactory used by the integration
// scenarios. Each fire:
//
//   - mints a fresh UUID v7 correlation id (time-sortable, matches the
//     production `coordinator_cron_wiring` prior art and the keeperslog
//     writer's default generator);
//   - synchronously appends a `cron_fired` event to keeperslog with the
//     watchkeeper id on the payload;
//   - returns a [tickEvent] carrying the same correlation id (and
//     watchkeeper id) so the handler can mirror the id onto its own
//     `handler_ran` audit row.
//
// The closure captures the writer, watchkeeper id, and harness clock
// (the latter so the event payload's `fired_at` is deterministic
// across runs — review iter-1 #4 caught a stale `time.Now()` here).
// The bus itself never touches any of them.
func makeFactory(t *testing.T, writer *keeperslog.Writer, watchkeeperID string, clock func() time.Time) cron.EventFactory {
	t.Helper()
	return func(ctx context.Context) any {
		corrIDUUID, err := uuid.NewV7()
		if err != nil {
			t.Errorf("factory: uuid.NewV7: %v", err)
			return nil
		}
		corrID := corrIDUUID.String()
		ctxWithID := keeperslog.ContextWithCorrelationID(ctx, corrID)
		now := clock().UTC()
		if _, err := writer.Append(ctxWithID, keeperslog.Event{
			EventType: "cron_fired",
			Payload: chainEventData{
				WatchkeeperID: watchkeeperID,
				FiredAtRFC:    now.Format(time.RFC3339Nano),
				Side:          "factory",
			},
		}); err != nil {
			t.Errorf("factory: cron_fired Append: %v", err)
		}
		return tickEvent{
			CorrelationID: corrID,
			WatchkeeperID: watchkeeperID,
			FiredAt:       now,
		}
	}
}

// makeHandler builds the bus subscriber used by the integration
// scenarios. The handler mirrors what a real Watchkeeper runtime would
// do on receiving a cron tick: read the correlation id off the event
// payload, seed it onto its own ctx, and emit a `handler_ran` audit
// row.
//
// The `signal` channel is sent on once per fire so the test can
// polling-deadline-wait for the handler to complete; tests close it on
// teardown to avoid leaking goroutines if the handler somehow fires
// extra times.
//
// Defence-in-depth: `eventbus.Handler` godoc states "Handlers MUST
// NOT panic. A panicking handler will crash the topic's worker
// goroutine and silently drop subsequent events on that topic." The
// recover here turns a panic into a t.Errorf so a regression that
// causes the handler to panic (a typed-assertion miss, a Logger that
// itself panics, etc.) surfaces as a hard test failure rather than a
// hung subtest.
func makeHandler(t *testing.T, writer *keeperslog.Writer, signal chan<- tickEvent) eventbus.Handler {
	t.Helper()
	return func(ctx context.Context, event any) {
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("handler: panic recovered: %v", r)
			}
		}()
		tick, ok := event.(tickEvent)
		if !ok {
			t.Errorf("handler: event has wrong type %T", event)
			return
		}
		ctxWithID := keeperslog.ContextWithCorrelationID(ctx, tick.CorrelationID)
		if _, err := writer.Append(ctxWithID, keeperslog.Event{
			EventType: "handler_ran",
			Payload: chainEventData{
				WatchkeeperID: tick.WatchkeeperID,
				Side:          "handler",
			},
		}); err != nil {
			t.Errorf("handler: handler_ran Append: %v", err)
			return
		}
		// Non-blocking send: if the receiver has already moved on (e.g.
		// the test asserted, the deadline expired, and the test is
		// tearing down), the second/extra fire still completes its
		// audit row but does not block the worker goroutine.
		select {
		case signal <- tick:
		default:
		}
	}
}

// TestM3Chain_CronToHandlerCorrelation is the canonical bullet-256
// scenario: spawn a mock Watchkeeper, fire a cron event, observe the
// handler receives the same correlation id the factory emitted, and
// assert Keeper's Log persisted both events with that id.
//
// Single fire is the assertion: the polling-deadline wait observes
// exactly one tickEvent on `signal`, and then `sched.Stop()` halts
// further fires before the assertion runs. Extra fires that may have
// slipped in between Stop and the recordedCalls snapshot are tolerated
// by checking "at least the expected pair exists" rather than "exactly
// 2 calls" — the bus is closed after Stop, so the upper bound is
// bounded by the cron's 1-second cadence.
func TestM3Chain_CronToHandlerCorrelation(t *testing.T) {
	h := newChainHarness(t)
	watchkeeperID := spawnMockWatchkeeper(t, h)

	signal := make(chan tickEvent, 4)
	unsubscribe, err := h.bus.Subscribe(chainTopic, makeHandler(t, h.writer, signal))
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer unsubscribe()

	if _, err := h.sched.Schedule(chainFireSpec, chainTopic, makeFactory(t, h.writer, watchkeeperID, h.clock)); err != nil {
		t.Fatalf("sched.Schedule: %v", err)
	}
	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatalf("sched.Start: %v", err)
	}

	observed := waitForTick(t, signal, chainWaitTimeout)

	// Halt further fires before the assertion snapshot so we read a
	// stable view. Stop returns a ctx that closes once any in-flight
	// fire-time closure (factory + Publish) has returned; waiting on
	// it avoids racing the assertion with a tail-end factory Append.
	//
	// Asymmetry caveat: stopCtx waits for cron's fire-time CLOSURE,
	// NOT for the bus subscriber to finish dispatching. The cron
	// closure ends right after Publish enqueues the envelope; the
	// handler may still be mid-Append when stopCtx is done. So the
	// snapshot can show a cron_fired row whose paired handler_ran is
	// not yet written — `len(cronFired) == len(handlerRan)` is NOT a
	// safe invariant here. The per-correlation lookup below tolerates
	// that asymmetry by keying on the OBSERVED tick's correlation id
	// (which by construction did reach the handler — the signal arrived).
	// A future reader tempted to tighten this to a parity check would
	// reintroduce the flake the review iter-1 (#2) called out.
	stopCtx := h.sched.Stop()
	<-stopCtx.Done()

	assertMatchingPairInLog(t, h.logClient.recordedCalls(), observed.CorrelationID, watchkeeperID)
}

// waitForTick blocks on `signal` up to `timeout`, returning the first
// observed tickEvent. Timeout calls t.Fatalf with a descriptive message
// so each caller does not have to inline the same select+fatal block
// (and so gocyclo on the test function stays under its threshold —
// review iter-1 surfaced this when the unrefactored test hit 16).
func waitForTick(t *testing.T, signal <-chan tickEvent, timeout time.Duration) tickEvent {
	t.Helper()
	select {
	case tick := <-signal:
		return tick
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for handler signal after %s", timeout)
		return tickEvent{} // unreachable; t.Fatalf halts
	}
}

// assertMatchingPairInLog is the bullet-256 headline assertion split
// out from TestM3Chain_CronToHandlerCorrelation: given a recorded log
// of LogAppend calls, an observed correlation id, and the
// spawned-watchkeeper id, verify (a) a cron_fired row with that
// correlation id exists, (b) a handler_ran row with that correlation
// id exists, (c) the watchkeeper id flows through both payloads, and
// (d) the side tag distinguishes factory vs handler rows. Extracted
// into a helper so the top-level test's cyclomatic complexity stays
// below the gocyclo threshold (review iter-1 fix).
func assertMatchingPairInLog(t *testing.T, calls []keepclient.LogAppendRequest, corrID, watchkeeperID string) {
	t.Helper()
	cronFired := findCallsByType(calls, "cron_fired")
	handlerRan := findCallsByType(calls, "handler_ran")
	if len(cronFired) < 1 || len(handlerRan) < 1 {
		t.Fatalf("missing rows: cron_fired=%d handler_ran=%d (total=%d)",
			len(cronFired), len(handlerRan), len(calls))
	}
	matchedCronFired := findCallByCorrelation(cronFired, corrID)
	matchedHandlerRan := findCallByCorrelation(handlerRan, corrID)
	if matchedCronFired == nil {
		t.Fatalf("no cron_fired row with correlation_id %q — cron_fired ids: %v",
			corrID, correlationIDs(cronFired))
	}
	if matchedHandlerRan == nil {
		t.Fatalf("no handler_ran row with correlation_id %q — handler_ran ids: %v",
			corrID, correlationIDs(handlerRan))
	}
	// Correlation-id parity is the bullet's headline guarantee; the
	// per-correlation lookup above proves both rows share corrID, so
	// the only remaining concern is that corrID itself is non-empty.
	if corrID == "" {
		t.Fatalf("correlation id empty — bullet 256 requires a non-empty matching id")
	}
	assertPayloadParity(t, matchedCronFired, matchedHandlerRan, watchkeeperID)
}

// assertPayloadParity validates payload-level invariants that hold ONLY
// if both audit rows came from the same (factory, handler) pair: the
// watchkeeper id is identical in both, and each row's `side` tag
// correctly identifies which closure wrote it.
func assertPayloadParity(t *testing.T, cronFiredCall, handlerRanCall *keepclient.LogAppendRequest, watchkeeperID string) {
	t.Helper()
	cronWK := extractWatchkeeperID(t, cronFiredCall.Payload)
	handlerWK := extractWatchkeeperID(t, handlerRanCall.Payload)
	if cronWK != watchkeeperID || handlerWK != watchkeeperID {
		t.Fatalf("watchkeeper id parity broken: spawned=%q cron_fired=%q handler_ran=%q",
			watchkeeperID, cronWK, handlerWK)
	}
	if got := extractSide(t, cronFiredCall.Payload); got != "factory" {
		t.Fatalf("cron_fired payload side=%q, want %q", got, "factory")
	}
	if got := extractSide(t, handlerRanCall.Payload); got != "handler" {
		t.Fatalf("handler_ran payload side=%q, want %q", got, "handler")
	}
}

// TestM3Chain_MultipleFires_DistinctCorrelationIDs collects two
// successive ticks and verifies (a) each pair (cron_fired+handler_ran)
// shares a correlation id, (b) the two pairs use DIFFERENT correlation
// ids. Catches a regression that pinned the correlation id as a
// process-global static instead of minting fresh per fire — the same
// per-call-resolver lesson the bench iter-1 round caught.
func TestM3Chain_MultipleFires_DistinctCorrelationIDs(t *testing.T) {
	h := newChainHarness(t)
	watchkeeperID := spawnMockWatchkeeper(t, h)

	signal := make(chan tickEvent, 8)
	unsubscribe, err := h.bus.Subscribe(chainTopic, makeHandler(t, h.writer, signal))
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer unsubscribe()

	if _, err := h.sched.Schedule(chainFireSpec, chainTopic, makeFactory(t, h.writer, watchkeeperID, h.clock)); err != nil {
		t.Fatalf("sched.Schedule: %v", err)
	}
	if err := h.sched.Start(context.Background()); err != nil {
		t.Fatalf("sched.Start: %v", err)
	}

	observed := make([]tickEvent, 0, 2)
	// Budget: cron fires at every second boundary, so the worst-case
	// happy path is ~3 s wall-clock (≤1 s to first boundary + 1 s + 1 s
	// drain + jitter). 2 × chainWaitTimeout = 10 s gives ~3× headroom
	// for CI runners that drop a tick when the prior fire's closure
	// runs longer than the cron cadence (robfig skips ticks rather
	// than queues them). Tighter budgets here flaked the iter-1 review
	// hypothetical; loosen further only if a CI environment under
	// extreme contention proves the current 10 s insufficient.
	deadline := time.After(2 * chainWaitTimeout)
	for len(observed) < 2 {
		select {
		case tick := <-signal:
			observed = append(observed, tick)
		case <-deadline:
			t.Fatalf("timed out waiting for 2 handler signals; got %d", len(observed))
		}
	}

	stopCtx := h.sched.Stop()
	<-stopCtx.Done()

	if observed[0].CorrelationID == observed[1].CorrelationID {
		t.Fatalf("two fires shared correlation id %q — factory must mint fresh per fire",
			observed[0].CorrelationID)
	}

	calls := h.logClient.recordedCalls()
	for _, tick := range observed {
		if findCallByCorrelation(findCallsByType(calls, "cron_fired"), tick.CorrelationID) == nil {
			t.Fatalf("no cron_fired row with correlation_id %q", tick.CorrelationID)
		}
		if findCallByCorrelation(findCallsByType(calls, "handler_ran"), tick.CorrelationID) == nil {
			t.Fatalf("no handler_ran row with correlation_id %q", tick.CorrelationID)
		}
	}
}

// TestM3Chain_LifecycleStateAfterSpawn confirms the "spawn a mock
// Watchkeeper" half of bullet 256 actually persists state in the
// (faked) Keep — without this, a regression that returns an id from
// Spawn without inserting a row would still pass the correlation
// assertion above. Belt-and-braces: the row is queryable via the same
// LocalKeepClient surface lifecycle uses.
func TestM3Chain_LifecycleStateAfterSpawn(t *testing.T) {
	h := newChainHarness(t)
	id := spawnMockWatchkeeper(t, h)

	// lifecycle.Manager.Health is the read path on the same client. If
	// Spawn really did Insert+UpdateStatus, Health returns status=active.
	status, err := h.lifeMgr.Health(context.Background(), id)
	if err != nil {
		t.Fatalf("lifeMgr.Health(%q): %v", id, err)
	}
	if status.Status != "active" {
		t.Fatalf("post-Spawn status = %q, want %q", status.Status, "active")
	}
	if status.SpawnedAt == nil {
		t.Fatalf("post-Spawn SpawnedAt nil — fake should stamp on pending→active transition")
	}
}

// findCallsByType returns every recorded LogAppend call whose
// event_type matches `eventType`. Used by the assertions to slice the
// call log into "cron_fired" vs "handler_ran" buckets.
func findCallsByType(calls []keepclient.LogAppendRequest, eventType string) []keepclient.LogAppendRequest {
	out := make([]keepclient.LogAppendRequest, 0, len(calls))
	for _, c := range calls {
		if c.EventType == eventType {
			out = append(out, c)
		}
	}
	return out
}

// findCallByCorrelation returns the first call whose correlation id
// matches `corrID`, or nil if absent. Returns a pointer so the caller
// can `== nil` test cheaply.
func findCallByCorrelation(calls []keepclient.LogAppendRequest, corrID string) *keepclient.LogAppendRequest {
	for i := range calls {
		if calls[i].CorrelationID == corrID {
			return &calls[i]
		}
	}
	return nil
}

// correlationIDs is a diagnostic helper that flattens a slice of calls
// to their correlation ids — used in t.Fatalf messages so a missing
// match prints the candidate set rather than just "not found".
func correlationIDs(calls []keepclient.LogAppendRequest) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.CorrelationID)
	}
	return out
}

// extractWatchkeeperID decodes the keeperslog envelope's `data` key
// and returns the watchkeeper id the test schema wrote there. Fails
// the test on JSON decode error so a malformed payload surfaces as a
// hard failure rather than a silently empty id.
func extractWatchkeeperID(t *testing.T, payload json.RawMessage) string {
	t.Helper()
	data := extractData(t, payload)
	return data.WatchkeeperID
}

// extractSide returns the `side` tag the factory / handler stamped on
// the payload. Driven through extractData so the JSON-decode path is
// exercised once in a single helper.
func extractSide(t *testing.T, payload json.RawMessage) string {
	t.Helper()
	data := extractData(t, payload)
	return data.Side
}

// extractData decodes the outer keeperslog envelope and then the
// embedded chainEventData. Trim guards against an envelope that
// happens to start with whitespace; a payload that does not parse
// fails the test loudly.
//
// Three distinct "no data" cases are distinguished for diagnostics:
//   - payload bytes empty (no row written at all — should not happen);
//   - envelope `data` key absent (env.Data is nil, len 0);
//   - envelope `data` key present but literal JSON `null` (env.Data is
//     the 4-byte slice `null`, which json.Unmarshal into a struct
//     would silently leave at the zero value — caught here explicitly
//     so a future refactor that sets Payload=nil surfaces as a clear
//     test failure rather than a zero-valued chainEventData).
func extractData(t *testing.T, payload json.RawMessage) chainEventData {
	t.Helper()
	if len(strings.TrimSpace(string(payload))) == 0 {
		t.Fatalf("extractData: empty payload")
	}
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("extractData: outer unmarshal: %v (payload=%s)", err, string(payload))
	}
	if len(env.Data) == 0 {
		t.Fatalf("extractData: envelope `data` key missing (payload=%s)", string(payload))
	}
	if string(env.Data) == "null" {
		t.Fatalf("extractData: envelope `data` is literal JSON null — factory or handler emitted a nil Payload (payload=%s)", string(payload))
	}
	var data chainEventData
	if err := json.Unmarshal(env.Data, &data); err != nil {
		t.Fatalf("extractData: inner unmarshal: %v (data=%s)", err, string(env.Data))
	}
	return data
}
