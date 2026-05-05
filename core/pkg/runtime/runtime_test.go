package runtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// validManifest returns a [Manifest] suitable for happy-path Start
// calls. Tests that need a different shape modify the returned value.
func validManifest() Manifest {
	return Manifest{
		AgentID:      "agent-1",
		SystemPrompt: "You are a test agent.",
		Model:        "claude-sonnet-4",
		Personality:  "concise",
		Language:     "en",
		Autonomy:     AutonomySupervised,
		Toolset:      []string{"echo", "remember"},
		AuthorityMatrix: map[string]string{
			"send_message": "self",
		},
		Metadata: map[string]string{"harness_version": "1"},
	}
}

// TestStart_HappyPathReturnsRuntimeWithNonEmptyID — happy path: Start
// records the manifest, returns a [Runtime] handle whose [Runtime.ID]
// is non-empty, and the manifest is captured verbatim.
func TestStart_HappyPathReturnsRuntimeWithNonEmptyID(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if rt == nil {
		t.Fatalf("Start returned nil Runtime")
	}
	if rt.ID() == "" {
		t.Fatalf("Runtime.ID empty")
	}
	if rt.ID() != "fake-runtime-1" {
		t.Fatalf("Runtime.ID = %q, want fake-runtime-1", rt.ID())
	}

	got := f.recordedStarts()
	if len(got) != 1 {
		t.Fatalf("recordedStarts len = %d, want 1", len(got))
	}
	if got[0].AgentID != "agent-1" || got[0].SystemPrompt == "" || got[0].Model != "claude-sonnet-4" {
		t.Fatalf("recordedStarts[0] = %+v", got[0])
	}
	if got[0].Metadata["harness_version"] != "1" {
		t.Fatalf("manifest metadata round-trip failed: %+v", got[0].Metadata)
	}
}

// TestStart_InvalidManifestRejectedSynchronously — empty AgentID /
// SystemPrompt / Model each surface [ErrInvalidManifest] WITHOUT
// touching the underlying runtime (no recorded call).
func TestStart_InvalidManifestRejectedSynchronously(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		manifest Manifest
	}{
		{"empty AgentID", Manifest{SystemPrompt: "x", Model: "m"}},
		{"empty SystemPrompt", Manifest{AgentID: "a", Model: "m"}},
		{"empty Model", Manifest{AgentID: "a", SystemPrompt: "x"}},
		{"all empty", Manifest{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeRuntime()
			_, err := f.Start(context.Background(), tc.manifest)
			if !errors.Is(err, ErrInvalidManifest) {
				t.Fatalf("Start err = %v, want errors.Is ErrInvalidManifest", err)
			}
			if got := f.recordedStarts(); len(got) != 0 {
				t.Fatalf("invalid manifest reached the runtime: recorded %d", len(got))
			}
		})
	}
}

// TestStart_OptionsApplied — functional options reach the underlying
// runtime. Pins the [WithStartMetadata] surface for future StartOption
// additions.
func TestStart_OptionsApplied(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	_, err := f.Start(
		context.Background(), validManifest(),
		WithStartMetadata(map[string]string{"workdir": "/tmp"}),
		WithStartMetadata(map[string]string{"workdir": "/var", "extra": "yes"}),
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The fake does not surface StartOptions back; this test exercises
	// option chaining + nil-safety. Confirm a nil map is a no-op.
	_, err = f.Start(context.Background(), validManifest(), WithStartMetadata(nil))
	if err != nil {
		t.Fatalf("Start with nil-meta option: %v", err)
	}
}

// TestSendMessage_DeliveredToRecordedLog — happy path: SendMessage
// records the runtime id + payload verbatim.
func TestSendMessage_DeliveredToRecordedLog(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	msg := Message{
		Text:     "hello agent",
		Metadata: map[string]string{"channel_id": "C123"},
	}
	if err := f.SendMessage(context.Background(), rt.ID(), msg); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	got := f.recordedSends()
	if len(got) != 1 {
		t.Fatalf("recordedSends len = %d, want 1", len(got))
	}
	if got[0].RuntimeID != rt.ID() {
		t.Fatalf("recordedSends[0].RuntimeID = %q, want %q", got[0].RuntimeID, rt.ID())
	}
	if got[0].Message.Text != "hello agent" {
		t.Fatalf("recordedSends[0].Message.Text = %q", got[0].Message.Text)
	}
	if got[0].Message.Metadata["channel_id"] != "C123" {
		t.Fatalf("metadata round-trip failed: %+v", got[0].Message.Metadata)
	}
}

// TestSendMessage_EmptyTextRejectedSynchronously — empty Text surfaces
// [ErrInvalidMessage] without touching the runtime.
func TestSendMessage_EmptyTextRejectedSynchronously(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	err = f.SendMessage(context.Background(), rt.ID(), Message{})
	if !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("SendMessage err = %v, want errors.Is ErrInvalidMessage", err)
	}
	if got := f.recordedSends(); len(got) != 0 {
		t.Fatalf("empty message reached the runtime: recorded %d", len(got))
	}
}

// TestSendMessage_UnknownRuntimeIDReturnsErrRuntimeNotFound — pins the
// [ErrRuntimeNotFound] vs [ErrTerminated] distinction.
func TestSendMessage_UnknownRuntimeIDReturnsErrRuntimeNotFound(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	err := f.SendMessage(context.Background(), "nope", Message{Text: "hi"})
	if !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("SendMessage err = %v, want errors.Is ErrRuntimeNotFound", err)
	}
}

// TestInvokeTool_HappyPathReturnsResult — the fake returns its canned
// [ToolResult] on a tool name present in the manifest's toolset.
func TestInvokeTool_HappyPathReturnsResult(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	f.invokeResult = ToolResult{
		Output:   map[string]any{"echoed": "hello"},
		Metadata: map[string]string{"tokens": "12"},
	}
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	res, err := f.InvokeTool(context.Background(), rt.ID(), ToolCall{
		Name:      "echo",
		Arguments: map[string]any{"text": "hello"},
	})
	if err != nil {
		t.Fatalf("InvokeTool: %v", err)
	}
	if res.Output["echoed"] != "hello" {
		t.Fatalf("InvokeTool result = %+v", res.Output)
	}
	if res.Metadata["tokens"] != "12" {
		t.Fatalf("metadata round-trip failed: %+v", res.Metadata)
	}

	got := f.recordedInvokes()
	if len(got) != 1 || got[0].Call.Name != "echo" {
		t.Fatalf("recordedInvokes = %+v", got)
	}
}

// TestInvokeTool_UnknownToolReturnsErrToolUnauthorized — names absent
// from the manifest's toolset surface [ErrToolUnauthorized] BEFORE the
// runtime touches the tool.
func TestInvokeTool_UnknownToolReturnsErrToolUnauthorized(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, err = f.InvokeTool(context.Background(), rt.ID(), ToolCall{Name: "delete_universe"})
	if !errors.Is(err, ErrToolUnauthorized) {
		t.Fatalf("InvokeTool err = %v, want errors.Is ErrToolUnauthorized", err)
	}
	if got := f.recordedInvokes(); len(got) != 0 {
		t.Fatalf("unauthorized tool reached the runtime: recorded %d", len(got))
	}
}

// TestInvokeTool_EmptyNameRejectedSynchronously — empty tool name
// surfaces [ErrInvalidToolCall] without touching the runtime.
func TestInvokeTool_EmptyNameRejectedSynchronously(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, err = f.InvokeTool(context.Background(), rt.ID(), ToolCall{})
	if !errors.Is(err, ErrInvalidToolCall) {
		t.Fatalf("InvokeTool err = %v, want errors.Is ErrInvalidToolCall", err)
	}
}

// TestInvokeTool_ReportsToolReportedError — when the tool itself fails
// (e.g. record-not-found), the runtime returns the result with
// [ToolResult.Error] populated and a nil error. Distinguishes
// transport / authorization failures (returned `error`) from tool-side
// failures.
func TestInvokeTool_ReportsToolReportedError(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	f.invokeResult = ToolResult{Error: "record not found"}
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res, err := f.InvokeTool(context.Background(), rt.ID(), ToolCall{Name: "echo"})
	if err != nil {
		t.Fatalf("InvokeTool err = %v, want nil (tool error rides on ToolResult.Error)", err)
	}
	if res.Error != "record not found" {
		t.Fatalf("ToolResult.Error = %q, want record not found", res.Error)
	}
}

// TestSubscribe_DeliversEventsInOrder — a sequence of [Event]
// values delivered via the fake's Deliver helper reaches the handler
// in the order published. Pins the per-session ordering contract.
func TestSubscribe_DeliversEventsInOrder(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	received := make(chan Event, 4)
	sub, err := f.Subscribe(context.Background(), rt.ID(), func(_ context.Context, ev Event) error {
		received <- ev
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	events := []Event{
		{Kind: EventKindMessage, RuntimeID: rt.ID(), Message: &Message{Text: "first"}},
		{Kind: EventKindToolCall, RuntimeID: rt.ID(), ToolCall: &ToolCall{Name: "echo"}},
		{Kind: EventKindToolResult, RuntimeID: rt.ID(), ToolResult: &ToolResult{Output: map[string]any{"k": "v"}}},
		{Kind: EventKindError, RuntimeID: rt.ID(), ErrorMessage: "transient hiccup"},
	}
	for _, ev := range events {
		if err := f.Deliver(context.Background(), rt.ID(), ev); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
	}

	for i, want := range events {
		select {
		case got := <-received:
			if got.Kind != want.Kind {
				t.Fatalf("event[%d].Kind = %q, want %q", i, got.Kind, want.Kind)
			}
			if got.RuntimeID != rt.ID() {
				t.Fatalf("event[%d].RuntimeID = %q, want %q", i, got.RuntimeID, rt.ID())
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("handler did not observe event[%d] within 2s", i)
		}
	}
}

// TestSubscribe_NilHandlerReturnsErrInvalidHandler — passing a nil
// handler is a programmer error and surfaces synchronously without
// spawning the dispatch loop.
func TestSubscribe_NilHandlerReturnsErrInvalidHandler(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	_, err = f.Subscribe(context.Background(), rt.ID(), nil)
	if !errors.Is(err, ErrInvalidHandler) {
		t.Fatalf("Subscribe(nil) err = %v, want errors.Is ErrInvalidHandler", err)
	}
}

// TestSubscribe_UnknownRuntimeReturnsErrRuntimeNotFound — pins the
// runtime-id validation order: nil-handler check first, then runtime
// lookup. An unknown id with a non-nil handler returns
// [ErrRuntimeNotFound].
func TestSubscribe_UnknownRuntimeReturnsErrRuntimeNotFound(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	_, err := f.Subscribe(context.Background(), "nope", func(_ context.Context, _ Event) error { return nil })
	if !errors.Is(err, ErrRuntimeNotFound) {
		t.Fatalf("Subscribe err = %v, want errors.Is ErrRuntimeNotFound", err)
	}
}

// TestSubscription_StopIsIdempotentAndDrains — Stop terminates
// delivery: the first Stop returns nil, a second Stop also returns
// nil, and a Deliver after Stop short-circuits with
// [ErrSubscriptionClosed] (the handler is not invoked).
func TestSubscription_StopIsIdempotentAndDrains(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var handlerHits atomic.Int64
	sub, err := f.Subscribe(context.Background(), rt.ID(), func(_ context.Context, _ Event) error {
		handlerHits.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if err := sub.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := sub.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}

	if err := f.Deliver(context.Background(), rt.ID(), Event{Kind: EventKindMessage, RuntimeID: rt.ID()}); !errors.Is(err, ErrSubscriptionClosed) {
		t.Fatalf("Deliver after Stop err = %v, want errors.Is ErrSubscriptionClosed", err)
	}
	if got := handlerHits.Load(); got != 0 {
		t.Fatalf("handler invoked %d times after Stop, want 0", got)
	}
}

// TestSubscription_StopSurfacesUnderlyingError — when the dispatch
// loop exited with a transport error before Stop was called, the next
// Stop surfaces it via the wrap chain.
func TestSubscription_StopSurfacesUnderlyingError(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	sub, err := f.Subscribe(context.Background(), rt.ID(), func(_ context.Context, _ Event) error { return nil })
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	fs, ok := sub.(*fakeSubscription)
	if !ok {
		t.Fatalf("sub is %T, want *fakeSubscription", sub)
	}
	fs.withStopErr(errFakeBoom)

	if err := sub.Stop(); !errors.Is(err, errFakeBoom) {
		t.Fatalf("Stop err = %v, want errors.Is errFakeBoom", err)
	}
}

// TestTerminate_SubsequentCallsReturnErrTerminated — after Terminate,
// SendMessage / InvokeTool / Subscribe on the same id all surface
// [ErrTerminated] (NOT [ErrRuntimeNotFound]).
func TestTerminate_SubsequentCallsReturnErrTerminated(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := f.Terminate(context.Background(), rt.ID()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	if err := f.SendMessage(context.Background(), rt.ID(), Message{Text: "post-term"}); !errors.Is(err, ErrTerminated) {
		t.Fatalf("SendMessage post-Terminate err = %v, want errors.Is ErrTerminated", err)
	}
	if _, err := f.InvokeTool(context.Background(), rt.ID(), ToolCall{Name: "echo"}); !errors.Is(err, ErrTerminated) {
		t.Fatalf("InvokeTool post-Terminate err = %v, want errors.Is ErrTerminated", err)
	}
	if _, err := f.Subscribe(context.Background(), rt.ID(), func(_ context.Context, _ Event) error { return nil }); !errors.Is(err, ErrTerminated) {
		t.Fatalf("Subscribe post-Terminate err = %v, want errors.Is ErrTerminated", err)
	}
}

// TestTerminate_IdempotentOnSameID — a second Terminate on the same
// id returns nil without re-running the shutdown.
func TestTerminate_IdempotentOnSameID(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	rt, err := f.Start(context.Background(), validManifest())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := f.Terminate(context.Background(), rt.ID()); err != nil {
		t.Fatalf("first Terminate: %v", err)
	}
	if err := f.Terminate(context.Background(), rt.ID()); err != nil {
		t.Fatalf("second Terminate: %v", err)
	}
	if got := f.recordedTerminates(); len(got) != 2 {
		t.Fatalf("recordedTerminates len = %d, want 2", len(got))
	}
}

// TestSentinelErrors_AllMatchableViaErrorsIs — mass-assertion that
// every package sentinel is matchable via [errors.Is] against itself.
// Pins the matchability contract documented in errors.go.
func TestSentinelErrors_AllMatchableViaErrorsIs(t *testing.T) {
	t.Parallel()

	cases := []error{
		ErrInvalidManifest,
		ErrInvalidMessage,
		ErrInvalidToolCall,
		ErrInvalidHandler,
		ErrRuntimeNotFound,
		ErrTerminated,
		ErrToolUnauthorized,
		ErrSubscriptionClosed,
	}
	for _, e := range cases {
		if !errors.Is(e, e) {
			t.Errorf("errors.Is(%v, %v) = false, want true", e, e)
		}
	}
}

// TestEventKind_PortableEnumValues — pins the documented values of
// [EventKind] so a future rename / typo fails the test rather than
// silently breaking switch-on-Kind callers.
func TestEventKind_PortableEnumValues(t *testing.T) {
	t.Parallel()

	cases := map[EventKind]string{
		EventKindMessage:    "message",
		EventKindToolCall:   "tool_call",
		EventKindToolResult: "tool_result",
		EventKindError:      "error",
	}
	for k, want := range cases {
		if string(k) != want {
			t.Errorf("EventKind %q != %q", k, want)
		}
	}
}

// TestAutonomyLevel_PortableEnumValues — pins the documented values of
// [AutonomyLevel] so a future rename does not silently break the
// manifest loader (M5.5) or the supervisor logic that compares against
// these constants.
func TestAutonomyLevel_PortableEnumValues(t *testing.T) {
	t.Parallel()

	cases := map[AutonomyLevel]string{
		AutonomyManual:     "manual",
		AutonomySupervised: "supervised",
		AutonomyAutonomous: "autonomous",
	}
	for k, want := range cases {
		if string(k) != want {
			t.Errorf("AutonomyLevel %q != %q", k, want)
		}
	}
}

// TestConcurrentStartAndSend_NoRaces — exercises the fake under -race
// with parallel Start / SendMessage calls. Pins the concurrency-safety
// contract every concrete runtime must honour.
func TestConcurrentStartAndSend_NoRaces(t *testing.T) {
	t.Parallel()

	f := newFakeRuntime()
	const N = 32
	ids := make([]ID, 0, N)
	var idsMu sync.Mutex

	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			rt, err := f.Start(context.Background(), validManifest())
			if err != nil {
				t.Errorf("Start: %v", err)
				return
			}
			idsMu.Lock()
			ids = append(ids, rt.ID())
			idsMu.Unlock()
		}()
	}
	wg.Wait()
	if len(ids) != N {
		t.Fatalf("started sessions = %d, want %d", len(ids), N)
	}

	wg.Add(N)
	for _, id := range ids {
		go func(id ID) {
			defer wg.Done()
			if err := f.SendMessage(context.Background(), id, Message{Text: "ping"}); err != nil {
				t.Errorf("SendMessage(%q): %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	if got := len(f.recordedSends()); got != N {
		t.Fatalf("recordedSends len = %d, want %d", got, N)
	}
}
