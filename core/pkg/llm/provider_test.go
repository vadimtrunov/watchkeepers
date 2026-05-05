package llm

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// validCompleteRequest returns a [CompleteRequest] suitable for
// happy-path Complete calls. Tests that need a different shape modify
// the returned value.
func validCompleteRequest() CompleteRequest {
	return CompleteRequest{
		Model:  "claude-sonnet-4",
		System: "You are a test agent.",
		Messages: []Message{
			{Role: RoleUser, Content: "hello"},
		},
		MaxTokens:   1024,
		Temperature: 0.7,
	}
}

// validStreamRequest mirrors validCompleteRequest for streaming.
func validStreamRequest() StreamRequest {
	return StreamRequest{
		Model:  "claude-sonnet-4",
		System: "You are a test agent.",
		Messages: []Message{
			{Role: RoleUser, Content: "hello"},
		},
		MaxTokens: 1024,
	}
}

// TestComplete_HappyPathReturnsCannedResponse — the fake echoes the
// configured [CompleteResponse] verbatim, with [Usage] populated.
func TestComplete_HappyPathReturnsCannedResponse(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	f.completeResp = CompleteResponse{
		Content:      "hi there",
		FinishReason: FinishReasonStop,
		Usage: Usage{
			Model:        "claude-sonnet-4",
			InputTokens:  12,
			OutputTokens: 4,
			CostCents:    150,
		},
		Metadata: map[string]string{"response_id": "resp_123"},
	}

	resp, err := f.Complete(context.Background(), validCompleteRequest())
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "hi there" {
		t.Fatalf("resp.Content = %q, want hi there", resp.Content)
	}
	if resp.FinishReason != FinishReasonStop {
		t.Fatalf("resp.FinishReason = %q, want stop", resp.FinishReason)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 4 {
		t.Fatalf("resp.Usage = %+v, want In=12 Out=4", resp.Usage)
	}
	if resp.Usage.CostCents != 150 {
		t.Fatalf("resp.Usage.CostCents = %d, want 150", resp.Usage.CostCents)
	}
	if resp.Metadata["response_id"] != "resp_123" {
		t.Fatalf("metadata round-trip failed: %+v", resp.Metadata)
	}

	got := f.recordedCompletes()
	if len(got) != 1 || got[0].Model != "claude-sonnet-4" {
		t.Fatalf("recordedCompletes = %+v", got)
	}
}

// TestComplete_EmptyModelReturnsErrModelNotSupported — an empty model
// surfaces synchronously without recording the call.
func TestComplete_EmptyModelReturnsErrModelNotSupported(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	req := validCompleteRequest()
	req.Model = ""

	_, err := f.Complete(context.Background(), req)
	if !errors.Is(err, ErrModelNotSupported) {
		t.Fatalf("Complete err = %v, want errors.Is ErrModelNotSupported", err)
	}
	if got := f.recordedCompletes(); len(got) != 0 {
		t.Fatalf("invalid request reached the provider: recorded %d", len(got))
	}
}

// TestComplete_OffCatalogueModelReturnsErrModelNotSupported — when the
// fake's [FakeProvider.Models] catalogue is populated, an unknown model
// is rejected.
func TestComplete_OffCatalogueModelReturnsErrModelNotSupported(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	f.Models = map[Model]struct{}{
		"claude-sonnet-4": {},
	}
	req := validCompleteRequest()
	req.Model = "gpt-4o"

	_, err := f.Complete(context.Background(), req)
	if !errors.Is(err, ErrModelNotSupported) {
		t.Fatalf("Complete err = %v, want errors.Is ErrModelNotSupported", err)
	}
}

// TestComplete_EmptyMessagesReturnsErrInvalidPrompt — an empty messages
// slice surfaces [ErrInvalidPrompt] without contacting the provider.
func TestComplete_EmptyMessagesReturnsErrInvalidPrompt(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	req := validCompleteRequest()
	req.Messages = nil

	_, err := f.Complete(context.Background(), req)
	if !errors.Is(err, ErrInvalidPrompt) {
		t.Fatalf("Complete err = %v, want errors.Is ErrInvalidPrompt", err)
	}
}

// TestComplete_NilToolSchemaReturnsErrInvalidPrompt — a tool with a nil
// [ToolDefinition.InputSchema] is a programmer error and surfaces
// before the provider runs.
func TestComplete_NilToolSchemaReturnsErrInvalidPrompt(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	req := validCompleteRequest()
	req.Tools = []ToolDefinition{
		{Name: "echo", Description: "echo back", InputSchema: nil},
	}

	_, err := f.Complete(context.Background(), req)
	if !errors.Is(err, ErrInvalidPrompt) {
		t.Fatalf("Complete err = %v, want errors.Is ErrInvalidPrompt", err)
	}
}

// TestComplete_InjectedErrorPropagates — a configured
// [FakeProvider.completeErr] surfaces verbatim AFTER validation passes.
func TestComplete_InjectedErrorPropagates(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	f.completeErr = ErrTokenLimitExceeded

	_, err := f.Complete(context.Background(), validCompleteRequest())
	if !errors.Is(err, ErrTokenLimitExceeded) {
		t.Fatalf("Complete err = %v, want errors.Is ErrTokenLimitExceeded", err)
	}
	// Validation passed → call WAS recorded.
	if got := f.recordedCompletes(); len(got) != 1 {
		t.Fatalf("expected 1 recorded completion, got %d", len(got))
	}
}

// TestComplete_ToolCallsAreRoundTripped — the fake preserves the
// [CompleteResponse.ToolCalls] slice and the model-emitted ids.
func TestComplete_ToolCallsAreRoundTripped(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	f.completeResp = CompleteResponse{
		FinishReason: FinishReasonToolUse,
		ToolCalls: []ToolCall{
			{ID: "tc_1", Name: "echo", Arguments: map[string]any{"text": "hi"}},
			{ID: "tc_2", Name: "remember", Arguments: map[string]any{"key": "k"}},
		},
		Usage: Usage{Model: "claude-sonnet-4", InputTokens: 10, OutputTokens: 2},
	}

	req := validCompleteRequest()
	req.Tools = []ToolDefinition{
		{Name: "echo", Description: "echo", InputSchema: map[string]any{"type": "object"}},
		{Name: "remember", Description: "remember", InputSchema: map[string]any{"type": "object"}},
	}

	resp, err := f.Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.FinishReason != FinishReasonToolUse {
		t.Fatalf("resp.FinishReason = %q, want tool_use", resp.FinishReason)
	}
	if len(resp.ToolCalls) != 2 || resp.ToolCalls[0].ID != "tc_1" {
		t.Fatalf("resp.ToolCalls = %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[1].Arguments["key"] != "k" {
		t.Fatalf("tool call arguments not preserved: %+v", resp.ToolCalls[1])
	}
}

// TestStream_DeliversEventsInOrder — three text deltas plus a
// message_stop event reach the handler in the order published.
func TestStream_DeliversEventsInOrder(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	f.streamEvents = []StreamEvent{
		{Kind: StreamEventKindTextDelta, TextDelta: "hel"},
		{Kind: StreamEventKindTextDelta, TextDelta: "lo "},
		{Kind: StreamEventKindTextDelta, TextDelta: "world"},
		{
			Kind:         StreamEventKindMessageStop,
			FinishReason: FinishReasonStop,
			Usage:        Usage{Model: "claude-sonnet-4", InputTokens: 5, OutputTokens: 3},
		},
	}

	received := make([]StreamEvent, 0, 4)
	sub, err := f.Stream(context.Background(), validStreamRequest(), func(_ context.Context, ev StreamEvent) error {
		received = append(received, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	if len(received) != 4 {
		t.Fatalf("received %d events, want 4", len(received))
	}
	if received[0].Kind != StreamEventKindTextDelta || received[0].TextDelta != "hel" {
		t.Fatalf("received[0] = %+v", received[0])
	}
	if received[3].Kind != StreamEventKindMessageStop {
		t.Fatalf("received[3].Kind = %q, want message_stop", received[3].Kind)
	}
	if received[3].Usage.InputTokens != 5 || received[3].Usage.OutputTokens != 3 {
		t.Fatalf("received[3].Usage = %+v", received[3].Usage)
	}
	if received[3].FinishReason != FinishReasonStop {
		t.Fatalf("received[3].FinishReason = %q, want stop", received[3].FinishReason)
	}
}

// TestStream_HandlerErrorTerminatesStream — when the handler returns a
// non-nil error, dispatch stops, subsequent events do not reach the
// handler, and Stop surfaces the error via the [errors.Is] chain.
func TestStream_HandlerErrorTerminatesStream(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	f.streamEvents = []StreamEvent{
		{Kind: StreamEventKindTextDelta, TextDelta: "first"},
		{Kind: StreamEventKindTextDelta, TextDelta: "second"},
		{Kind: StreamEventKindMessageStop, FinishReason: FinishReasonStop},
	}

	var hits atomic.Int64
	sub, err := f.Stream(context.Background(), validStreamRequest(), func(_ context.Context, _ StreamEvent) error {
		hits.Add(1)
		return errFakeBoom
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("handler invoked %d times, want 1 (stream should stop after first error)", got)
	}
	stopErr := sub.Stop()
	if !errors.Is(stopErr, ErrStreamClosed) {
		t.Fatalf("Stop err = %v, want errors.Is ErrStreamClosed", stopErr)
	}
	if !errors.Is(stopErr, errFakeBoom) {
		t.Fatalf("Stop err = %v, want errors.Is errFakeBoom (cause preserved)", stopErr)
	}
}

// TestStream_NilHandlerReturnsErrInvalidHandler — passing a nil handler
// is a programmer error and surfaces synchronously without dispatching.
func TestStream_NilHandlerReturnsErrInvalidHandler(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	_, err := f.Stream(context.Background(), validStreamRequest(), nil)
	if !errors.Is(err, ErrInvalidHandler) {
		t.Fatalf("Stream(nil) err = %v, want errors.Is ErrInvalidHandler", err)
	}
}

// TestStream_StopIsIdempotentAndDrains — Stop returns nil for clean
// shutdowns; a second Stop also returns nil.
func TestStream_StopIsIdempotentAndDrains(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	f.streamEvents = []StreamEvent{
		{Kind: StreamEventKindMessageStop, FinishReason: FinishReasonStop},
	}
	sub, err := f.Stream(context.Background(), validStreamRequest(), func(_ context.Context, _ StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if err := sub.Stop(); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := sub.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// TestStream_ValidationErrorsTakePrecedence — empty model / empty
// messages / nil handler all surface the synchronous sentinels even
// when [FakeProvider.streamErr] is rigged.
func TestStream_ValidationErrorsTakePrecedence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(req *StreamRequest) StreamHandler
		want error
	}{
		{
			"empty model",
			func(req *StreamRequest) StreamHandler {
				req.Model = ""
				return func(_ context.Context, _ StreamEvent) error { return nil }
			},
			ErrModelNotSupported,
		},
		{
			"empty messages",
			func(req *StreamRequest) StreamHandler {
				req.Messages = nil
				return func(_ context.Context, _ StreamEvent) error { return nil }
			},
			ErrInvalidPrompt,
		},
		{
			"nil handler",
			func(_ *StreamRequest) StreamHandler { return nil },
			ErrInvalidHandler,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeProvider()
			f.streamErr = errFakeBoom
			req := validStreamRequest()
			h := tc.mut(&req)
			_, err := f.Stream(context.Background(), req, h)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Stream err = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

// TestCountTokens_DeterministicForFixedInput — calling CountTokens
// twice with the same request returns the same count, and the request
// is recorded in the fake's call log.
func TestCountTokens_DeterministicForFixedInput(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	req := CountTokensRequest{
		Model:  "claude-sonnet-4",
		System: "you are concise",
		Messages: []Message{
			{Role: RoleUser, Content: "hello world"},
			{Role: RoleAssistant, Content: "hi"},
		},
	}
	first, err := f.CountTokens(context.Background(), req)
	if err != nil {
		t.Fatalf("first CountTokens: %v", err)
	}
	second, err := f.CountTokens(context.Background(), req)
	if err != nil {
		t.Fatalf("second CountTokens: %v", err)
	}
	if first != second {
		t.Fatalf("CountTokens not deterministic: %d vs %d", first, second)
	}
	if first == 0 {
		t.Fatalf("CountTokens = 0, want > 0 for non-empty input")
	}

	got := f.recordedCountTokens()
	if len(got) != 2 || got[0].Model != "claude-sonnet-4" {
		t.Fatalf("recordedCountTokens = %+v", got)
	}
	if got[0].Messages[0].Role != RoleUser || got[0].Messages[1].Role != RoleAssistant {
		t.Fatalf("recordedCountTokens preserves message roles: %+v", got[0].Messages)
	}
}

// TestCountTokens_EmptyModelReturnsErrModelNotSupported pins the
// validation order: model first, then messages.
func TestCountTokens_EmptyModelReturnsErrModelNotSupported(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	_, err := f.CountTokens(context.Background(), CountTokensRequest{
		Messages: []Message{{Role: RoleUser, Content: "x"}},
	})
	if !errors.Is(err, ErrModelNotSupported) {
		t.Fatalf("CountTokens err = %v, want errors.Is ErrModelNotSupported", err)
	}
}

// TestCountTokens_EmptyMessagesReturnsErrInvalidPrompt — pin that the
// model is checked first; once the model is valid, empty messages
// surfaces [ErrInvalidPrompt].
func TestCountTokens_EmptyMessagesReturnsErrInvalidPrompt(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	_, err := f.CountTokens(context.Background(), CountTokensRequest{
		Model: "claude-sonnet-4",
	})
	if !errors.Is(err, ErrInvalidPrompt) {
		t.Fatalf("CountTokens err = %v, want errors.Is ErrInvalidPrompt", err)
	}
}

// TestCountTokens_CannedResponseOverridesSynthetic — the fake's
// configured [FakeProvider.countTokensResp] takes precedence over the
// synthetic count.
func TestCountTokens_CannedResponseOverridesSynthetic(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	f.countTokensResp = 999
	got, err := f.CountTokens(context.Background(), CountTokensRequest{
		Model:    "claude-sonnet-4",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if got != 999 {
		t.Fatalf("CountTokens = %d, want 999", got)
	}
}

// TestReportCost_RecordsUsageInLog — a single ReportCost call appends
// one entry to the fake's log with the runtimeID and usage round-tripped.
func TestReportCost_RecordsUsageInLog(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	usage := Usage{
		Model:        "claude-sonnet-4",
		InputTokens:  100,
		OutputTokens: 50,
		CostCents:    1500,
		Metadata:     map[string]string{"request_id": "req_42"},
	}
	if err := f.ReportCost(context.Background(), "fake-runtime-1", usage); err != nil {
		t.Fatalf("ReportCost: %v", err)
	}

	got := f.recordedReportCosts()
	if len(got) != 1 {
		t.Fatalf("recordedReportCosts len = %d, want 1", len(got))
	}
	if got[0].RuntimeID != "fake-runtime-1" {
		t.Fatalf("recordedReportCosts[0].RuntimeID = %q", got[0].RuntimeID)
	}
	if got[0].Usage.InputTokens != 100 || got[0].Usage.OutputTokens != 50 {
		t.Fatalf("recordedReportCosts[0].Usage = %+v", got[0].Usage)
	}
	if got[0].Usage.CostCents != 1500 {
		t.Fatalf("recordedReportCosts[0].Usage.CostCents = %d", got[0].Usage.CostCents)
	}
	if got[0].Usage.Metadata["request_id"] != "req_42" {
		t.Fatalf("usage metadata round-trip failed: %+v", got[0].Usage.Metadata)
	}
}

// TestReportCost_AcceptsUnseenRuntimeID — ReportCost is the create-
// or-update boundary; an unseen runtimeID is fine.
func TestReportCost_AcceptsUnseenRuntimeID(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	if err := f.ReportCost(context.Background(), "never-started", Usage{}); err != nil {
		t.Fatalf("ReportCost on unseen id: %v", err)
	}
}

// TestReportCost_DuplicateCallsAccumulate — the contract notes that the
// caller is responsible for calling ReportCost exactly once per turn;
// duplicates produce duplicate accounting. This pins the fake honours
// that semantic.
func TestReportCost_DuplicateCallsAccumulate(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	usage := Usage{Model: "m", InputTokens: 10, OutputTokens: 5}
	for i := 0; i < 3; i++ {
		if err := f.ReportCost(context.Background(), "rt-1", usage); err != nil {
			t.Fatalf("ReportCost #%d: %v", i, err)
		}
	}
	if got := f.recordedReportCosts(); len(got) != 3 {
		t.Fatalf("recordedReportCosts len = %d, want 3", len(got))
	}
}

// TestSentinelErrors_AllMatchableViaErrorsIs — mass-assertion that every
// package sentinel is matchable via [errors.Is] against itself. Pins
// the matchability contract documented in errors.go.
func TestSentinelErrors_AllMatchableViaErrorsIs(t *testing.T) {
	t.Parallel()

	cases := []error{
		ErrInvalidPrompt,
		ErrModelNotSupported,
		ErrTokenLimitExceeded,
		ErrInvalidHandler,
		ErrStreamClosed,
		ErrProviderUnavailable,
	}
	for _, e := range cases {
		if !errors.Is(e, e) {
			t.Errorf("errors.Is(%v, %v) = false, want true", e, e)
		}
	}
}

// TestRole_PortableEnumValues — pins documented Role values so a future
// rename / typo fails the test rather than silently breaking
// switch-on-Role callers.
func TestRole_PortableEnumValues(t *testing.T) {
	t.Parallel()

	cases := map[Role]string{
		RoleSystem:    "system",
		RoleUser:      "user",
		RoleAssistant: "assistant",
		RoleTool:      "tool",
	}
	for k, want := range cases {
		if string(k) != want {
			t.Errorf("Role %q != %q", k, want)
		}
	}
}

// TestStreamEventKind_PortableEnumValues — pins documented values of
// [StreamEventKind] so future renames don't silently break consumers.
func TestStreamEventKind_PortableEnumValues(t *testing.T) {
	t.Parallel()

	cases := map[StreamEventKind]string{
		StreamEventKindTextDelta:     "text_delta",
		StreamEventKindToolCallStart: "tool_call_start",
		StreamEventKindToolCallDelta: "tool_call_delta",
		StreamEventKindMessageStop:   "message_stop",
		StreamEventKindError:         "error",
	}
	for k, want := range cases {
		if string(k) != want {
			t.Errorf("StreamEventKind %q != %q", k, want)
		}
	}
}

// TestFinishReason_PortableEnumValues — pins documented values of
// [FinishReason].
func TestFinishReason_PortableEnumValues(t *testing.T) {
	t.Parallel()

	cases := map[FinishReason]string{
		FinishReasonStop:      "stop",
		FinishReasonMaxTokens: "max_tokens",
		FinishReasonToolUse:   "tool_use",
		FinishReasonError:     "error",
	}
	for k, want := range cases {
		if string(k) != want {
			t.Errorf("FinishReason %q != %q", k, want)
		}
	}
}

// TestConcurrentCompleteAndReportCost_NoRaces — exercises the fake
// under -race with parallel Complete / ReportCost calls. Pins the
// concurrency-safety contract every concrete provider must honour.
func TestConcurrentCompleteAndReportCost_NoRaces(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	f.completeResp = CompleteResponse{
		FinishReason: FinishReasonStop,
		Usage:        Usage{Model: "claude-sonnet-4", InputTokens: 1, OutputTokens: 1},
	}

	const N = 32
	var wg sync.WaitGroup
	wg.Add(2 * N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, err := f.Complete(context.Background(), validCompleteRequest())
			if err != nil {
				t.Errorf("Complete: %v", err)
			}
		}()
		go func(i int) {
			defer wg.Done()
			usage := Usage{Model: "claude-sonnet-4", InputTokens: int(i), OutputTokens: 1}
			if err := f.ReportCost(context.Background(), "rt", usage); err != nil {
				t.Errorf("ReportCost: %v", err)
			}
		}(i)
	}
	wg.Wait()

	if got := len(f.recordedCompletes()); got != N {
		t.Fatalf("recordedCompletes len = %d, want %d", got, N)
	}
	if got := len(f.recordedReportCosts()); got != N {
		t.Fatalf("recordedReportCosts len = %d, want %d", got, N)
	}
}

// TestStream_RecordedRequestPreservesMetadata — pins that the request
// metadata bag round-trips through Stream just like through Complete.
// Belt-and-braces against future provider-side mutations leaking back.
func TestStream_RecordedRequestPreservesMetadata(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	f.streamEvents = []StreamEvent{
		{Kind: StreamEventKindMessageStop, FinishReason: FinishReasonStop},
	}
	req := validStreamRequest()
	req.Metadata = map[string]string{"top_k": "40"}
	sub, err := f.Stream(context.Background(), req, func(_ context.Context, _ StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	got := f.recordedStreams()
	if len(got) != 1 || got[0].Metadata["top_k"] != "40" {
		t.Fatalf("recordedStreams = %+v", got)
	}
}

// TestStream_StopReturnsBeforeTimeout — Stop blocks at most briefly on
// a quiescent stream. The fake dispatches synchronously so the
// dispatch loop has already returned by Stop time; assert the call
// returns under a generous timeout to catch regressions in stop
// signalling.
func TestStream_StopReturnsBeforeTimeout(t *testing.T) {
	t.Parallel()

	f := newFakeProvider()
	sub, err := f.Stream(context.Background(), validStreamRequest(), func(_ context.Context, _ StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- sub.Stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Stop err = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Stop did not return within 2s")
	}
}
