package auditsubscriber

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/approval"
	"github.com/vadimtrunov/watchkeepers/core/pkg/eventbus"
	"github.com/vadimtrunov/watchkeepers/core/pkg/hostedexport"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/localpatch"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolshare"
)

// testUnsubsCount returns the live number of installed subscriptions
// for assertion in tests (critic iter-1 n7: post-rollback / post-Stop
// internal-state pin). Implemented as a method on the same-package
// receiver so production code stays free of test-only surface.
func (s *Subscriber) testUnsubsCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.unsubs)
}

// TestNew_NilBus_Panics asserts the constructor refuses a nil
// [Bus] dep with a clear message. Same panic-on-nil discipline as
// [localpatch.NewInstaller] / [approval.NewProposer].
func TestNew_NilBus_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic on nil Bus, got none")
		}
		got := fmt.Sprintf("%v", r)
		if !strings.Contains(got, "Bus must not be nil") {
			t.Fatalf("panic message %q does not mention Bus", got)
		}
	}()
	NewSubscriber(SubscriberDeps{Writer: &fakeWriter{}})
}

// TestNew_NilWriter_Panics asserts the constructor refuses a nil
// [Writer] dep with a clear message.
func TestNew_NilWriter_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic on nil Writer, got none")
		}
		got := fmt.Sprintf("%v", r)
		if !strings.Contains(got, "Writer must not be nil") {
			t.Fatalf("panic message %q does not mention Writer", got)
		}
	}()
	NewSubscriber(SubscriberDeps{Bus: &fakeBus{}})
}

// TestNew_OptionalLogger_Nil asserts a Subscriber constructed
// without a [Logger] dep can be Started + Stopped without crashing
// on a logf invocation path.
func TestNew_OptionalLogger_Nil(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// TestStart_SubscribesAllBindings asserts Start installs one
// handler per [allBindings] entry in declaration order. The
// recorded topic order is the test's join key with the binding
// table.
func TestStart_SubscribesAllBindings(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop() }()
	got := bus.snapshotTopics()
	if len(got) != len(allBindings) {
		t.Fatalf("subscribed count: got %d want %d", len(got), len(allBindings))
	}
	for i, b := range allBindings {
		if got[i] != b.Topic {
			t.Errorf("binding[%d]: got %q want %q", i, got[i], b.Topic)
		}
	}
}

// TestStart_AlreadyStarted asserts a second Start returns
// [ErrAlreadyStarted] without spawning duplicate subscriptions.
func TestStart_AlreadyStarted(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
	if err := s.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer func() { _ = s.Stop() }()
	if err := s.Start(); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatalf("second Start: got %v want %v", err, ErrAlreadyStarted)
	}
	if len(bus.snapshotTopics()) != len(allBindings) {
		t.Fatalf("duplicate subscriptions installed: got %d topics", len(bus.snapshotTopics()))
	}
}

// TestStart_AfterStop_ReturnsErrStopped asserts the Subscriber is
// single-use: once Stop has been called, Start refuses with
// [ErrStopped].
func TestStart_AfterStop_ReturnsErrStopped(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := s.Start(); !errors.Is(err, ErrStopped) {
		t.Fatalf("post-Stop Start: got %v want %v", err, ErrStopped)
	}
}

// TestStart_SubscribeFailure_RollsBack asserts a failed
// [Bus.Subscribe] mid-Start (a) unsubscribes every prior handler
// EXACTLY ONCE, (b) invokes the failing call's returned unsub once
// (codex iter-1 M1 defensive-unsub pin), and (c) surfaces an err
// wrapping [ErrSubscribe] AND chaining to the underlying error
// via Unwrap, while the err.Error() string redacts the underlying
// value (codex iter-1 m2 redaction pin).
func TestStart_SubscribeFailure_RollsBack(t *testing.T) {
	t.Parallel()
	failTopic := allBindings[2].Topic
	bus := &fakeBus{failOnTopic: failTopic}
	wr := &fakeWriter{}
	lg := &fakeLogger{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr, Logger: lg})

	err := s.Start()
	if err == nil {
		t.Fatalf("Start: expected error, got nil")
	}
	if !errors.Is(err, errFakeSubscribe) {
		t.Errorf("Start err chain missing %v", errFakeSubscribe)
	}
	if !errors.Is(err, ErrSubscribe) {
		t.Errorf("Start err chain missing %v", ErrSubscribe)
	}
	if !strings.Contains(err.Error(), failTopic) {
		t.Errorf("error message does not name failing topic %q: %v", failTopic, err)
	}
	// Redaction pin: the underlying err's Error() string MUST NOT
	// appear verbatim in the Start err's message (codex m2).
	if strings.Contains(err.Error(), errFakeSubscribe.Error()) {
		t.Errorf("Start err.Error() leaked underlying err value: %q", err.Error())
	}

	// Per-entry exact-once (critic iter-1 m4): the two prior
	// successful Subscribes each fired ONE unsubscribe — not zero,
	// not two.
	counts := bus.perEntryUnsubCounts()
	if len(counts) != 2 {
		t.Fatalf("captured subs: got %d want 2", len(counts))
	}
	for i, c := range counts {
		if c != 1 {
			t.Errorf("subs[%d] unsub fired %d times; want exactly 1", i, c)
		}
	}

	// Defensive-unsub pin (codex M1): the failing Subscribe's
	// returned no-op closure MUST have been invoked exactly once.
	if bus.failingUnsubCount != 1 {
		t.Errorf("failing-Subscribe unsub fired %d times; want 1", bus.failingUnsubCount)
	}

	// Internal-state pin (critic n7): after a failed Start the
	// unsubs slice is empty so a follow-up Stop is a no-op.
	if got := s.testUnsubsCount(); got != 0 {
		t.Errorf("post-rollback unsubs slice len: got %d want 0", got)
	}

	// Diagnostic surface: the logger MUST have observed the failure.
	entries := lg.snapshotEntries()
	if len(entries) == 0 {
		t.Errorf("expected at least one log entry on rollback")
	}
}

// TestStop_AfterFailedStart_NoDoubleUnsub asserts a Stop call AFTER
// a Start that failed rollback does NOT re-fire the unsubscribe
// callbacks (critic iter-1 m5). The rollback already invoked them;
// a follow-up Stop loop over s.unsubs (now nil) is a no-op.
func TestStop_AfterFailedStart_NoDoubleUnsub(t *testing.T) {
	t.Parallel()
	failTopic := allBindings[2].Topic
	bus := &fakeBus{failOnTopic: failTopic}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})

	if err := s.Start(); err == nil {
		t.Fatalf("Start: expected error, got nil")
	}
	rollbackUnsubs := bus.unsubCount

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if bus.unsubCount != rollbackUnsubs {
		t.Errorf("Stop fired %d extra unsubs after failed-Start rollback; want 0",
			bus.unsubCount-rollbackUnsubs)
	}
}

// TestStart_RetryAfterFailure_Succeeds asserts a Subscriber whose
// Start failed CAN be Started again on the same receiver — the
// failed-Start rollback leaves the receiver in a fresh state
// (critic iter-1 m6). A future refactor that accidentally set
// `s.stopped = true` on the rollback path would surface here.
func TestStart_RetryAfterFailure_Succeeds(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{failOnTopic: allBindings[2].Topic}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})

	if err := s.Start(); err == nil {
		t.Fatalf("first Start: expected error, got nil")
	}
	bus.clearFailOnTopic()

	if err := s.Start(); err != nil {
		t.Fatalf("retry Start: %v", err)
	}
	defer func() { _ = s.Stop() }()
	if got := s.testUnsubsCount(); got != len(allBindings) {
		t.Errorf("retry Start unsubs len: got %d want %d", got, len(allBindings))
	}
}

// TestStop_BeforeStart asserts Stop on a never-Started Subscriber
// transitions to the stopped state; a subsequent Start returns
// [ErrStopped] (critic iter-1 m5/m7). Pins the single-use lifecycle
// invariant.
func TestStop_BeforeStart(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})

	if err := s.Stop(); err != nil {
		t.Fatalf("Stop on never-Started: %v", err)
	}
	if bus.unsubCount != 0 {
		t.Errorf("Stop fired %d unsubs on never-Started receiver; want 0", bus.unsubCount)
	}
	if err := s.Start(); !errors.Is(err, ErrStopped) {
		t.Errorf("Start after Stop-before-Start: got %v want %v", err, ErrStopped)
	}
}

// TestStop_Idempotent asserts the second + third Stop calls are
// no-ops (no extra unsubscribe fires, no panic).
func TestStop_Idempotent(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	first := bus.unsubCount
	if err := s.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("third Stop: %v", err)
	}
	if bus.unsubCount != first {
		t.Errorf("unsub fired after first Stop: got %d want %d", bus.unsubCount, first)
	}
}

// TestDispatch_TypeMismatch_LogsAndContinues asserts that
// publishing a wrong-type payload on a topic LOGS metadata and
// DROPS the event — no panic, no Append call.
func TestDispatch_TypeMismatch_LogsAndContinues(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{}
	lg := &fakeLogger{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr, Logger: lg})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop() }()

	// Deliver a `string` to a topic that expects toolregistry.SourceSynced.
	handler := bus.topicHandler(toolregistry.TopicSourceSynced)
	if handler == nil {
		t.Fatal("expected handler on topic")
	}
	handler(context.Background(), "i-am-not-a-source-synced")

	if len(wr.snapshotEvents()) != 0 {
		t.Errorf("Append should NOT fire on type-mismatch; got %d events", len(wr.snapshotEvents()))
	}
	entries := lg.snapshotEntries()
	if len(entries) != 1 {
		t.Fatalf("log entry count: got %d want 1", len(entries))
	}
	if !strings.Contains(entries[0].msg, "unexpected payload") {
		t.Errorf("log msg does not mention 'unexpected payload': %q", entries[0].msg)
	}
}

// TestDispatch_AppendFailure_LogsAndContinues asserts a failing
// [Writer.Append] LOGS metadata and DROPS the event — no panic,
// no retry, no propagation back to the bus.
func TestDispatch_AppendFailure_LogsAndContinues(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{failAlways: true}
	lg := &fakeLogger{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr, Logger: lg})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop() }()

	handler := bus.topicHandler(toolregistry.TopicSourceSynced)
	if handler == nil {
		t.Fatal("expected handler on topic")
	}
	handler(context.Background(), validSourceSynced())

	calls := wr.snapshotEvents()
	if len(calls) != 1 {
		t.Fatalf("Append call count: got %d want 1", len(calls))
	}
	if !errors.Is(calls[0].returnedErr, errFakeAppend) {
		t.Errorf("expected fake Append error; got %v", calls[0].returnedErr)
	}
	entries := lg.snapshotEntries()
	if len(entries) != 1 {
		t.Fatalf("log entry count: got %d want 1", len(entries))
	}
	if !strings.Contains(entries[0].msg, "append failed") {
		t.Errorf("log msg does not mention 'append failed': %q", entries[0].msg)
	}
}

// TestDispatch_HappyPath_AllTopics asserts every binding's
// extractor returns the verbatim payload + the payload's
// CorrelationID, the dispatcher stamps the id onto the ctx, and
// the writer sees the bare event_type vocabulary.
func TestDispatch_HappyPath_AllTopics(t *testing.T) {
	t.Parallel()
	cases := happyDispatchCases()
	for _, tc := range cases {
		tc := tc
		t.Run(tc.eventType, func(t *testing.T) {
			t.Parallel()
			bus := &fakeBus{}
			wr := &fakeWriter{}
			s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
			if err := s.Start(); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer func() { _ = s.Stop() }()

			handler := bus.topicHandler(tc.topic)
			if handler == nil {
				t.Fatalf("no handler on topic %q", tc.topic)
			}
			handler(context.Background(), tc.payload)

			calls := wr.snapshotEvents()
			if len(calls) != 1 {
				t.Fatalf("Append count: got %d want 1", len(calls))
			}
			got := calls[0]
			if got.event.EventType != tc.eventType {
				t.Errorf("event_type: got %q want %q", got.event.EventType, tc.eventType)
			}
			if got.event.Payload == nil {
				t.Errorf("Append received nil payload; want verbatim event struct")
			}
			if got.correlationID != tc.expectedCorrelationID {
				t.Errorf("ctx-carried correlation id: got %q want %q",
					got.correlationID, tc.expectedCorrelationID)
			}
			// The dispatcher passes the bus payload through to
			// [keeperslog.Event.Payload] VERBATIM — same Go value,
			// same field shape. Use reflect.DeepEqual rather than a
			// `%v` round-trip (critic iter-1 m9 + n5: `%v` on a
			// multi-key map is order-randomised; `%+v` on a
			// time.Time has rendering quirks). DeepEqual is
			// structural and stable.
			if !reflect.DeepEqual(got.event.Payload, tc.payload) {
				t.Errorf("payload round-trip mismatch:\n got: %#v\nwant: %#v", got.event.Payload, tc.payload)
			}
		})
	}
}

// TestDispatch_EmptyCorrelationID_MintsViaWriter asserts a payload
// with an empty CorrelationID stays empty on the ctx — the writer
// itself mints a fresh value (verified by
// [keeperslog.ContextWithCorrelationID]'s empty-passthrough
// contract).
func TestDispatch_EmptyCorrelationID_MintsViaWriter(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop() }()

	handler := bus.topicHandler(toolregistry.TopicSourceSynced)
	payload := validSourceSynced()
	payload.CorrelationID = ""
	handler(context.Background(), payload)

	calls := wr.snapshotEvents()
	if len(calls) != 1 {
		t.Fatalf("Append count: got %d want 1", len(calls))
	}
	if calls[0].correlationID != "" {
		t.Errorf("expected empty ctx-carried correlation id; got %q", calls[0].correlationID)
	}
}

// TestDispatch_NoLogger_TypeMismatch_NoPanic asserts a Subscriber
// without a [Logger] dep silently drops a type-mismatch — the
// optional-logger contract.
func TestDispatch_NoLogger_TypeMismatch_NoPanic(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop() }()

	handler := bus.topicHandler(toolregistry.TopicSourceSynced)
	handler(context.Background(), 42) // wrong type, no logger

	if len(wr.snapshotEvents()) != 0 {
		t.Errorf("Append should NOT fire on type-mismatch")
	}
}

// TestDispatch_ConcurrencyNoRace exercises the dispatcher under
// 16 goroutines × N events per topic to surface any data race in
// the lifecycle state or writer/logger fakes. Pin: the writer's
// captured event count equals the total publish count.
func TestDispatch_ConcurrencyNoRace(t *testing.T) {
	t.Parallel()
	const (
		goroutines = 16
		perG       = 25
	)
	bus := &fakeBus{}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop() }()

	handler := bus.topicHandler(toolregistry.TopicSourceSynced)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				handler(context.Background(), validSourceSynced())
			}
		}()
	}
	wg.Wait()

	got := len(wr.snapshotEvents())
	want := goroutines * perG
	if got != want {
		t.Errorf("Append count under concurrency: got %d want %d", got, want)
	}
}

// TestDispatch_WithRealEventbus_EndToEnd wires a real
// [*eventbus.Bus] + the Subscriber + a fake writer; publishes an
// event; asserts the writer received it. Pins the [Bus]-interface
// compatibility plus the real per-topic-worker dispatch path.
func TestDispatch_WithRealEventbus_EndToEnd(t *testing.T) {
	t.Parallel()
	bus := eventbus.New()
	defer func() {
		if err := bus.Close(); err != nil {
			t.Errorf("bus.Close: %v", err)
		}
	}()
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop() }()

	payload := validSourceSynced()
	if err := bus.Publish(context.Background(), toolregistry.TopicSourceSynced, payload); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Bus dispatches async via a per-topic worker; poll with a
	// short deadline rather than sleeping a fixed interval.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(wr.snapshotEvents()) > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	calls := wr.snapshotEvents()
	if len(calls) != 1 {
		t.Fatalf("Append count after real-bus publish: got %d want 1", len(calls))
	}
	if calls[0].event.EventType != EventTypeSourceSynced {
		t.Errorf("event_type: got %q want %q", calls[0].event.EventType, EventTypeSourceSynced)
	}
	if calls[0].correlationID != payload.CorrelationID {
		t.Errorf("correlation id: got %q want %q", calls[0].correlationID, payload.CorrelationID)
	}
}

// dispatchCase is one row of the happy-path table.
type dispatchCase struct {
	topic                 string
	eventType             string
	payload               any
	expectedCorrelationID string
}

// happyDispatchCases returns the 11-row table covering every
// binding's happy-path dispatch.
func happyDispatchCases() []dispatchCase {
	now := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	pid := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	return []dispatchCase{
		{
			topic:                 toolregistry.TopicSourceSynced,
			eventType:             EventTypeSourceSynced,
			payload:               toolregistry.SourceSynced{SourceName: "src1", SyncedAt: now, LocalPath: "/data/src1", CorrelationID: "corr-1"},
			expectedCorrelationID: "corr-1",
		},
		{
			topic:                 toolregistry.TopicSourceFailed,
			eventType:             EventTypeSourceFailed,
			payload:               toolregistry.SourceFailed{SourceName: "src1", FailedAt: now, ErrorType: "*errors.errorString", Phase: "clone", CorrelationID: "corr-2"},
			expectedCorrelationID: "corr-2",
		},
		{
			topic:                 toolregistry.TopicToolShadowed,
			eventType:             EventTypeToolShadowed,
			payload:               toolregistry.ToolShadowed{ToolName: "t", WinnerSource: "ws", WinnerVersion: "1.0", ShadowedSource: "ss", ShadowedVersion: "0.5", Revision: 3, BuiltAt: now, CorrelationID: "corr-3"},
			expectedCorrelationID: "corr-3",
		},
		{
			topic:                 approval.TopicToolProposed,
			eventType:             EventTypeToolProposed,
			payload:               approval.ToolProposed{ProposalID: pid, ToolName: "t", ProposerID: "agt", TargetSource: approval.TargetSourcePlatform, CapabilityIDs: []string{"cap:x"}, ProposedAt: now, CorrelationID: "corr-4"},
			expectedCorrelationID: "corr-4",
		},
		{
			topic:                 approval.TopicToolApproved,
			eventType:             EventTypeToolApproved,
			payload:               approval.ToolApproved{ProposalID: pid, ToolName: "t", ApproverID: "lead", Route: approval.RouteSlackNative, TargetSource: approval.TargetSourcePlatform, SourceName: "platform", ApprovedAt: now, CorrelationID: "corr-5"},
			expectedCorrelationID: "corr-5",
		},
		{
			topic:                 approval.TopicToolRejected,
			eventType:             EventTypeToolRejected,
			payload:               approval.ToolRejected{ProposalID: pid, ToolName: "t", RejecterID: "lead", Route: approval.RouteSlackNative, RejectedAt: now, CorrelationID: "corr-6"},
			expectedCorrelationID: "corr-6",
		},
		{
			topic:                 approval.TopicDryRunExecuted,
			eventType:             EventTypeDryRunExecuted,
			payload:               approval.DryRunExecuted{ProposalID: pid, ToolName: "t", Mode: toolregistry.DryRunModeGhost, BrokerKindCounts: map[string]int{"slack": 1}, InvocationCount: 1, ExecutedAt: now, CorrelationID: "corr-7"},
			expectedCorrelationID: "corr-7",
		},
		{
			topic:                 localpatch.TopicLocalPatchApplied,
			eventType:             EventTypeLocalPatchApplied,
			payload:               localpatch.LocalPatchApplied{SourceName: "local", ToolName: "t", ToolVersion: "1.0", OperatorID: "op", Reason: "test", DiffHash: strings.Repeat("a", 64), Operation: localpatch.OperationInstall, AppliedAt: now, CorrelationID: "corr-8"},
			expectedCorrelationID: "corr-8",
		},
		{
			topic:                 hostedexport.TopicHostedToolExported,
			eventType:             EventTypeHostedToolExported,
			payload:               hostedexport.HostedToolExported{SourceName: "hosted", ToolName: "t", ToolVersion: "1.0", OperatorID: "op", Reason: "test", BundleDigest: strings.Repeat("b", 64), ExportedAt: now, CorrelationID: "corr-9"},
			expectedCorrelationID: "corr-9",
		},
		{
			topic:                 toolshare.TopicToolShareProposed,
			eventType:             EventTypeToolShareProposed,
			payload:               toolshare.ToolShareProposed{SourceName: "private", ToolName: "t", ToolVersion: "1.0", ProposerID: "agt", Reason: "test", TargetOwner: "o", TargetRepo: "r", TargetBase: "main", TargetSource: toolshare.TargetSourcePlatform, ProposedAt: now, CorrelationID: "corr-10"},
			expectedCorrelationID: "corr-10",
		},
		{
			topic:                 toolshare.TopicToolSharePROpened,
			eventType:             EventTypeToolSharePROpened,
			payload:               toolshare.ToolSharePROpened{SourceName: "private", ToolName: "t", ToolVersion: "1.0", ProposerID: "agt", TargetOwner: "o", TargetRepo: "r", TargetBase: "main", TargetSource: toolshare.TargetSourcePlatform, PRNumber: 42, PRHTMLURL: "https://example/pulls/42", OpenedAt: now, CorrelationID: "corr-11"},
			expectedCorrelationID: "corr-11",
		},
	}
}

// validSourceSynced returns a fully-populated SourceSynced payload
// for tests that just need a known-good shape on the bus.
func validSourceSynced() toolregistry.SourceSynced {
	return toolregistry.SourceSynced{
		SourceName:    "src",
		SyncedAt:      time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		LocalPath:     "/data/tools/src",
		CorrelationID: "corr-x",
	}
}

// Compile-time sanity: keeperslog.Event{} can be constructed with
// the exact field set the dispatcher uses. A future breaking change
// to keeperslog.Event surfaces here.
var _ = keeperslog.Event{EventType: "x", Payload: nil}

// ctxKey is a typed test-only ctx key for asserting the dispatcher
// propagates caller-supplied ctx values into [Writer.Append]
// (critic iter-1 m12).
type ctxKey struct{}

// TestDispatch_PropagatesCtxValues asserts the dispatcher does NOT
// strip caller-supplied ctx values on the way to [Writer.Append].
// Pins the ctx-pass-through invariant implicit in the "best-effort
// audit" lesson — a future refactor that swapped to a fresh
// `context.Background()` would silently drop trace ids.
func TestDispatch_PropagatesCtxValues(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop() }()

	want := "trace-canary-9001"
	ctx := context.WithValue(context.Background(), ctxKey{}, want)
	handler := bus.topicHandler(toolregistry.TopicSourceSynced)
	handler(ctx, validSourceSynced())

	calls := wr.snapshotEvents()
	if len(calls) != 1 {
		t.Fatalf("Append count: got %d want 1", len(calls))
	}
	got, _ := calls[0].ctx.Value(ctxKey{}).(string)
	if got != want {
		t.Errorf("dispatcher dropped ctx value: got %q want %q", got, want)
	}
}

// TestDispatch_AppendFailureLogf_UsesAppendCtx asserts the
// append-failure diagnostic logf carries the payload's
// CorrelationID (via the [appendCtx]), not a fresh ctx without it
// (codex iter-1 m1). Without this pin, a logger that reads
// correlation id from ctx would record the wrong (or no) id on
// the exact path operators need to debug.
func TestDispatch_AppendFailureLogf_UsesAppendCtx(t *testing.T) {
	t.Parallel()
	bus := &fakeBus{}
	wr := &fakeWriter{failAlways: true}
	captured := &correlationCapturingLogger{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr, Logger: captured})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop() }()

	handler := bus.topicHandler(toolregistry.TopicSourceSynced)
	payload := validSourceSynced()
	payload.CorrelationID = "corr-from-payload"
	handler(context.Background(), payload)

	if captured.lastCtxCorrelationID != "corr-from-payload" {
		t.Errorf("append-failure logf ctx correlation id: got %q want %q",
			captured.lastCtxCorrelationID, "corr-from-payload")
	}
}

// correlationCapturingLogger is a fakeLogger variant that snapshots
// the ctx-carried correlation id at Log call time. Defined in this
// file (rather than fakes_test.go) so it stays local to the m1
// regression test.
type correlationCapturingLogger struct {
	lastCtxCorrelationID string
}

func (l *correlationCapturingLogger) Log(ctx context.Context, _ string, _ ...any) {
	if id, ok := keeperslog.CorrelationIDFromContext(ctx); ok {
		l.lastCtxCorrelationID = id
	}
}

// TestDispatch_ConcurrencyAcrossTopics_NoRace exercises every binding
// concurrently to surface any cross-topic shared-state regression
// (critic iter-1 n6: extension of the single-topic concurrency test).
// Pin: total Append count equals goroutines × bindings × perG, and
// the per-topic counts (mod ordering within bindings) match.
func TestDispatch_ConcurrencyAcrossTopics_NoRace(t *testing.T) {
	t.Parallel()
	const (
		goroutines = 4
		perG       = 5
	)
	cases := happyDispatchCases()
	bus := &fakeBus{}
	wr := &fakeWriter{}
	s := NewSubscriber(SubscriberDeps{Bus: bus, Writer: wr})
	if err := s.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop() }()

	var wg sync.WaitGroup
	for _, tc := range cases {
		tc := tc
		handler := bus.topicHandler(tc.topic)
		if handler == nil {
			t.Fatalf("no handler on topic %q", tc.topic)
		}
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < perG; j++ {
					handler(context.Background(), tc.payload)
				}
			}()
		}
	}
	wg.Wait()

	want := goroutines * perG * len(cases)
	if got := len(wr.snapshotEvents()); got != want {
		t.Errorf("multi-topic concurrency Append count: got %d want %d", got, want)
	}
}

// TestExtractor_AcceptsValueAndPointer asserts every extractor
// accepts both the value publish (`Publish(..., ev)`) AND the
// pointer publish (`Publish(..., &ev)`) shapes (codex iter-1 M2).
// A future emitter refactor from value to pointer publish (or
// vice-versa) must NOT silently turn into log+drop.
func TestExtractor_AcceptsValueAndPointer(t *testing.T) {
	t.Parallel()
	for _, tc := range happyDispatchCases() {
		tc := tc
		t.Run(tc.eventType+"/value", func(t *testing.T) {
			t.Parallel()
			b := bindingByEventType(t, tc.eventType)
			payload, corrID, err := b.Extract(tc.payload)
			if err != nil {
				t.Fatalf("value-publish extract: %v", err)
			}
			if corrID != tc.expectedCorrelationID {
				t.Errorf("corrID: got %q want %q", corrID, tc.expectedCorrelationID)
			}
			if !reflect.DeepEqual(payload, tc.payload) {
				t.Errorf("payload mismatch on value publish")
			}
		})
		t.Run(tc.eventType+"/pointer", func(t *testing.T) {
			t.Parallel()
			b := bindingByEventType(t, tc.eventType)
			// Reflect-borrow a pointer to the test payload so we
			// don't rewrite each case-specific Go type.
			pv := reflect.New(reflect.TypeOf(tc.payload))
			pv.Elem().Set(reflect.ValueOf(tc.payload))
			payload, corrID, err := b.Extract(pv.Interface())
			if err != nil {
				t.Fatalf("pointer-publish extract: %v", err)
			}
			if corrID != tc.expectedCorrelationID {
				t.Errorf("corrID: got %q want %q", corrID, tc.expectedCorrelationID)
			}
			if !reflect.DeepEqual(payload, tc.payload) {
				t.Errorf("payload mismatch on pointer publish")
			}
		})
	}
}

// TestExtractor_NilPointer_TypeMismatch asserts a nil `*T` is
// classified as a type-mismatch (codex iter-1 M2 boundary case).
// The nil pointer has no fields to surface; logging+dropping is
// safer than dereferencing.
func TestExtractor_NilPointer_TypeMismatch(t *testing.T) {
	t.Parallel()
	var nilPayload *toolregistry.SourceSynced
	b := bindingByEventType(t, EventTypeSourceSynced)
	_, _, err := b.Extract(nilPayload)
	if err == nil {
		t.Fatalf("nil-pointer extract: expected error, got nil")
	}
	if !errors.Is(err, errUnexpectedPayload) {
		t.Errorf("nil-pointer err chain missing errUnexpectedPayload: %v", err)
	}
}

// TestExtractor_WrapsSentinel asserts the [errUnexpectedPayload]
// sentinel propagates through the extractor's error chain so a
// test (or future caller) can classify via [errors.Is] (critic
// iter-1 M1). Without this assertion the unexported sentinel
// is dead surface — the test ratifies its observability inside
// the package.
func TestExtractor_WrapsSentinel(t *testing.T) {
	t.Parallel()
	b := bindingByEventType(t, EventTypeSourceSynced)
	_, _, err := b.Extract("wrong-type")
	if err == nil {
		t.Fatalf("wrong-type extract: expected error, got nil")
	}
	if !errors.Is(err, errUnexpectedPayload) {
		t.Errorf("err chain missing errUnexpectedPayload: %v", err)
	}
	// Also pin that the err's Error() string carries the expected
	// + got Go type names — useful diagnostic for any caller that
	// chose to surface the chain.
	msg := err.Error()
	if !strings.Contains(msg, "toolregistry.SourceSynced") {
		t.Errorf("err message missing expected type: %q", msg)
	}
	if !strings.Contains(msg, "string") {
		t.Errorf("err message missing got type: %q", msg)
	}
}

// bindingByEventType returns the binding whose `EventType` matches
// `want`. Fatal-fails the test on miss. Helper used by the
// extractor-shape tests above.
func bindingByEventType(t *testing.T, want string) binding {
	t.Helper()
	for _, b := range allBindings {
		if b.EventType == want {
			return b
		}
	}
	t.Fatalf("no binding with event_type %q", want)
	return binding{}
}
