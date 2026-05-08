// cost_test.go covers the M6.3.e LoggingProvider decorator end-to-end.
// Tests follow the project real-fakes discipline (M6.2.a precedent
// "Real-fakes-end-to-end test pattern" in docs/lessons/M6.md): real
// *keeperslog.Writer driving a recording fakeKeepClient where the
// envelope shape matters; hand-rolled fake llm.Provider in every
// scenario; NO mocking library.
//
// The 10 cases pinned by TASK §"Test plan":
//
//  1. Happy Complete                  -> 1 event with closed-set payload.
//  2. Happy Stream                    -> 1 event on MessageStop.
//  3. Error Complete                  -> 0 events.
//  4. Stream synchronous error        -> 0 events.
//  5. Stream close without MessageStop -> 0 events.
//  6. Concurrent Complete (-race)     -> race-free emit.
//  7. Correlation id propagation      -> ctx-stored corr id rides on row.
//  8. Logger emit error               -> LLM result still propagates.
//  9. PII guard regression            -> no body / system / tool-args.
//
// 10. Compile-time assertion          -> var _ llm.Provider check builds.
package cost_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
	"github.com/vadimtrunov/watchkeepers/core/pkg/llm/cost"
)

// ── compile-time assertions (case 10) ───────────────────────────────────────

// Pinned in `_test.go` so the production package stays free of test-only
// symbols. Mirrors the messenger / runtime / keeperslog hand-rolled-fake
// pattern documented in `docs/LESSONS.md`.
var (
	_ llm.Provider  = (*cost.LoggingProvider)(nil)
	_ cost.Appender = (*keeperslog.Writer)(nil)
	_ cost.Appender = (*recordingAppender)(nil)
	_ llm.Provider  = (*fakeProvider)(nil)
)

// ── happy-path Complete (case 1) ────────────────────────────────────────────

func TestLoggingProvider_Complete_HappyPath_EmitsOneEvent(t *testing.T) {
	t.Parallel()

	const agentID = "agent-test-001"
	wantUsage := llm.Usage{InputTokens: 1234, OutputTokens: 567}
	wantFinish := llm.FinishReasonStop
	wantModel := llm.Model("claude-sonnet-4")

	inner := &fakeProvider{
		completeResp: llm.CompleteResponse{
			Content:      "ok",
			FinishReason: wantFinish,
			Usage:        wantUsage,
		},
	}
	app := &recordingAppender{}

	p := cost.NewLoggingProvider(inner, cost.Dependencies{AgentID: agentID, Logger: app})

	resp, err := p.Complete(context.Background(), llm.CompleteRequest{
		Model:    wantModel,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Content != "ok" {
		t.Fatalf("forwarded resp.Content = %q, want %q", resp.Content, "ok")
	}

	calls := app.recorded()
	if len(calls) != 1 {
		t.Fatalf("appender call count = %d, want 1", len(calls))
	}
	got := calls[0]
	if got.EventType != cost.EventTypeLLMCallCompleted {
		t.Errorf("event_type = %q, want %q", got.EventType, cost.EventTypeLLMCallCompleted)
	}
	payload, ok := got.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T, want map[string]any", got.Payload)
	}
	if payload["agent_id"] != agentID {
		t.Errorf("agent_id = %v, want %q", payload["agent_id"], agentID)
	}
	if payload["model"] != string(wantModel) {
		t.Errorf("model = %v, want %q", payload["model"], wantModel)
	}
	if payload["input_tokens"] != wantUsage.InputTokens {
		t.Errorf("input_tokens = %v, want %d", payload["input_tokens"], wantUsage.InputTokens)
	}
	if payload["output_tokens"] != wantUsage.OutputTokens {
		t.Errorf("output_tokens = %v, want %d", payload["output_tokens"], wantUsage.OutputTokens)
	}
	if payload["prompt_tokens"] != wantUsage.InputTokens {
		t.Errorf("prompt_tokens (M6.2.a alias) = %v, want %d", payload["prompt_tokens"], wantUsage.InputTokens)
	}
	if payload["completion_tokens"] != wantUsage.OutputTokens {
		t.Errorf("completion_tokens (M6.2.a alias) = %v, want %d", payload["completion_tokens"], wantUsage.OutputTokens)
	}
	if payload["finish_reason"] != string(wantFinish) {
		t.Errorf("finish_reason = %v, want %q", payload["finish_reason"], wantFinish)
	}
}

// ── happy-path Stream (case 2) ──────────────────────────────────────────────

func TestLoggingProvider_Stream_HappyPath_EmitsOneEventOnMessageStop(t *testing.T) {
	t.Parallel()

	const agentID = "agent-stream-001"
	wantUsage := llm.Usage{InputTokens: 99, OutputTokens: 11}
	wantModel := llm.Model("claude-sonnet-4")

	inner := &fakeProvider{
		streamEvents: []llm.StreamEvent{
			{Kind: llm.StreamEventKindTextDelta, TextDelta: "partial"},
			{Kind: llm.StreamEventKindMessageStop, FinishReason: llm.FinishReasonStop, Usage: wantUsage},
		},
	}
	app := &recordingAppender{}
	p := cost.NewLoggingProvider(inner, cost.Dependencies{AgentID: agentID, Logger: app})

	var observed []llm.StreamEventKind
	sub, err := p.Stream(context.Background(), llm.StreamRequest{
		Model:    wantModel,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "stream"}},
	}, func(_ context.Context, ev llm.StreamEvent) error {
		observed = append(observed, ev.Kind)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	// Caller's handler still sees both events (TextDelta then MessageStop).
	if len(observed) != 2 {
		t.Fatalf("user handler observed %d events, want 2", len(observed))
	}
	if observed[1] != llm.StreamEventKindMessageStop {
		t.Errorf("final observed kind = %q, want %q", observed[1], llm.StreamEventKindMessageStop)
	}

	calls := app.recorded()
	if len(calls) != 1 {
		t.Fatalf("appender call count = %d, want 1 (one MessageStop)", len(calls))
	}
	payload := calls[0].Payload.(map[string]any)
	if payload["input_tokens"] != wantUsage.InputTokens || payload["output_tokens"] != wantUsage.OutputTokens {
		t.Errorf("usage payload = (%v, %v), want (%d, %d)",
			payload["input_tokens"], payload["output_tokens"],
			wantUsage.InputTokens, wantUsage.OutputTokens)
	}
	if payload["model"] != string(wantModel) {
		t.Errorf("model = %v, want %q", payload["model"], wantModel)
	}
	if payload["finish_reason"] != string(llm.FinishReasonStop) {
		t.Errorf("finish_reason = %v, want %q", payload["finish_reason"], llm.FinishReasonStop)
	}
}

// ── error Complete (case 3) ─────────────────────────────────────────────────

func TestLoggingProvider_Complete_Error_EmitsZeroEvents(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("provider: synthetic")
	inner := &fakeProvider{completeErr: sentinel}
	app := &recordingAppender{}
	p := cost.NewLoggingProvider(inner, cost.Dependencies{AgentID: "agent", Logger: app})

	_, err := p.Complete(context.Background(), llm.CompleteRequest{
		Model:    "claude",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("forwarded error = %v, want chain containing %v", err, sentinel)
	}
	if got := app.callCount(); got != 0 {
		t.Errorf("appender call count = %d, want 0 (error path)", got)
	}
}

// ── stream synchronous error (case 4) ───────────────────────────────────────

func TestLoggingProvider_Stream_SynchronousError_EmitsZeroEvents(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("provider: stream synthetic")
	inner := &fakeProvider{streamErr: sentinel}
	app := &recordingAppender{}
	p := cost.NewLoggingProvider(inner, cost.Dependencies{AgentID: "agent", Logger: app})

	sub, err := p.Stream(context.Background(), llm.StreamRequest{
		Model:    "claude",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, func(_ context.Context, _ llm.StreamEvent) error { return nil })
	if !errors.Is(err, sentinel) {
		t.Fatalf("forwarded error = %v, want chain containing %v", err, sentinel)
	}
	if sub != nil {
		t.Errorf("subscription = %v, want nil on synchronous error", sub)
	}
	if got := app.callCount(); got != 0 {
		t.Errorf("appender call count = %d, want 0 (sync stream error)", got)
	}
}

// ── stream closes without MessageStop (case 5) ──────────────────────────────

func TestLoggingProvider_Stream_NoMessageStop_EmitsZeroEvents(t *testing.T) {
	t.Parallel()

	inner := &fakeProvider{
		streamEvents: []llm.StreamEvent{
			{Kind: llm.StreamEventKindTextDelta, TextDelta: "partial-1"},
			{Kind: llm.StreamEventKindTextDelta, TextDelta: "partial-2"},
			// no MessageStop — simulates provider crash mid-stream.
		},
	}
	app := &recordingAppender{}
	p := cost.NewLoggingProvider(inner, cost.Dependencies{AgentID: "agent", Logger: app})

	sub, err := p.Stream(context.Background(), llm.StreamRequest{
		Model:    "claude",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	}, func(_ context.Context, _ llm.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	t.Cleanup(func() { _ = sub.Stop() })

	if got := app.callCount(); got != 0 {
		t.Errorf("appender call count = %d, want 0 (no MessageStop)", got)
	}
}

// ── concurrent Complete (case 6) ────────────────────────────────────────────

func TestLoggingProvider_Complete_Concurrent_RaceFree(t *testing.T) {
	t.Parallel()

	const goroutines = 5
	const callsPerG = 4

	inner := &fakeProvider{
		completeResp: llm.CompleteResponse{
			FinishReason: llm.FinishReasonStop,
			Usage:        llm.Usage{InputTokens: 10, OutputTokens: 5},
		},
	}
	app := &recordingAppender{}
	p := cost.NewLoggingProvider(inner, cost.Dependencies{AgentID: "agent-concurrent", Logger: app})

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < callsPerG; j++ {
				_, err := p.Complete(context.Background(), llm.CompleteRequest{
					Model:    "claude",
					Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}},
				})
				if err != nil {
					t.Errorf("Complete: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	if got, want := app.callCount(), goroutines*callsPerG; got != want {
		t.Errorf("appender call count = %d, want %d", got, want)
	}
}

// ── correlation id propagation (case 7) ─────────────────────────────────────

func TestLoggingProvider_Complete_PropagatesCorrelationID(t *testing.T) {
	t.Parallel()

	const corrID = "corr-deterministic-001"
	const agentID = "agent-corr"

	inner := &fakeProvider{
		completeResp: llm.CompleteResponse{
			FinishReason: llm.FinishReasonStop,
			Usage:        llm.Usage{InputTokens: 1, OutputTokens: 2},
		},
	}

	// Real *keeperslog.Writer over recording fake LocalKeepClient so the
	// envelope shape (correlation_id is a top-level keepclient field, NOT
	// a payload key) is exercised end-to-end.
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	p := cost.NewLoggingProvider(inner, cost.Dependencies{AgentID: agentID, Logger: writer})

	ctx := keeperslog.ContextWithCorrelationID(context.Background(), corrID)
	_, err := p.Complete(ctx, llm.CompleteRequest{
		Model:    "claude",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	rows := keep.recorded()
	if len(rows) != 1 {
		t.Fatalf("keepclient call count = %d, want 1", len(rows))
	}
	if rows[0].CorrelationID != corrID {
		t.Errorf("correlation_id = %q, want %q", rows[0].CorrelationID, corrID)
	}
	if rows[0].EventType != cost.EventTypeLLMCallCompleted {
		t.Errorf("event_type = %q, want %q", rows[0].EventType, cost.EventTypeLLMCallCompleted)
	}
}

// ── logger emit error does NOT short-circuit LLM result (case 8) ────────────

func TestLoggingProvider_LoggerEmitError_PropagatesLLMResult(t *testing.T) {
	t.Parallel()

	wantResp := llm.CompleteResponse{
		Content:      "the LLM call still won",
		FinishReason: llm.FinishReasonStop,
		Usage:        llm.Usage{InputTokens: 7, OutputTokens: 11},
	}
	inner := &fakeProvider{completeResp: wantResp}
	app := &recordingAppender{appendErr: errors.New("keep: down")}
	p := cost.NewLoggingProvider(inner, cost.Dependencies{AgentID: "agent", Logger: app})

	resp, err := p.Complete(context.Background(), llm.CompleteRequest{
		Model:    "claude",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete returned err = %v, want nil (logger error does not short-circuit)", err)
	}
	if resp.Content != wantResp.Content {
		t.Errorf("resp.Content = %q, want %q", resp.Content, wantResp.Content)
	}
	// The Append was attempted exactly once even though it failed.
	if got := app.callCount(); got != 1 {
		t.Errorf("appender call count = %d, want 1 (one attempt despite error)", got)
	}
}

// ── PII guard regression (case 9) ───────────────────────────────────────────

func TestLoggingProvider_PIIGuard_NoBodyOrSystemPromptOrToolArgs(t *testing.T) {
	t.Parallel()

	const userBody = "USER_BODY_SHOULD_NEVER_LEAK_xyzzy"
	const systemPrompt = "SYSTEM_PROMPT_SHOULD_NEVER_LEAK_plover"
	const toolArgFingerprint = "TOOL_ARG_SHOULD_NEVER_LEAK_grue"

	inner := &fakeProvider{
		completeResp: llm.CompleteResponse{
			Content: "assistant content (NEVER carried in the audit row by the cost decorator)",
			ToolCalls: []llm.ToolCall{
				{ID: "tc-1", Name: "search", Arguments: map[string]any{"q": toolArgFingerprint}},
			},
			FinishReason: llm.FinishReasonStop,
			Usage:        llm.Usage{InputTokens: 13, OutputTokens: 17},
		},
	}
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	p := cost.NewLoggingProvider(inner, cost.Dependencies{AgentID: "agent", Logger: writer})

	_, err := p.Complete(context.Background(), llm.CompleteRequest{
		Model:    "claude",
		System:   systemPrompt,
		Messages: []llm.Message{{Role: llm.RoleUser, Content: userBody}},
		Tools: []llm.ToolDefinition{
			{Name: "search", Description: "search tool", InputSchema: map[string]any{"type": "object"}},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	rows := keep.recorded()
	if len(rows) != 1 {
		t.Fatalf("keepclient call count = %d, want 1", len(rows))
	}
	wireBytes := []byte(rows[0].Payload)
	for _, fp := range []string{userBody, systemPrompt, toolArgFingerprint, "assistant content"} {
		if strings.Contains(string(wireBytes), fp) {
			t.Errorf("keepers_log payload leaked PII fingerprint %q. Wire bytes: %s", fp, string(wireBytes))
		}
	}

	// Closed-set check: decoded payload `data` map MUST contain ONLY
	// the seven documented keys.
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(wireBytes, &envelope); err != nil {
		t.Fatalf("unmarshal payload envelope: %v", err)
	}
	allowed := map[string]struct{}{
		"agent_id":          {},
		"model":             {},
		"input_tokens":      {},
		"output_tokens":     {},
		"prompt_tokens":     {},
		"completion_tokens": {},
		"finish_reason":     {},
	}
	for k := range envelope.Data {
		if _, ok := allowed[k]; !ok {
			t.Errorf("payload key %q is NOT in the closed set", k)
		}
	}
	if got, want := len(envelope.Data), len(allowed); got != want {
		t.Errorf("payload key count = %d, want %d (closed set: %v)", got, want, allowed)
	}
}

// ── nil-arg construction guards ─────────────────────────────────────────────

func TestNewLoggingProvider_NilUnderlying_Panics(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic, got none")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "underlying provider must not be nil") {
			t.Errorf("panic message = %q, want substring %q", msg, "underlying provider must not be nil")
		}
	}()
	cost.NewLoggingProvider(nil, cost.Dependencies{Logger: &recordingAppender{}})
}

func TestNewLoggingProvider_NilLogger_Panics(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic, got none")
		}
		msg, _ := r.(string)
		if !strings.Contains(msg, "deps.Logger must not be nil") {
			t.Errorf("panic message = %q, want substring %q", msg, "deps.Logger must not be nil")
		}
	}()
	cost.NewLoggingProvider(&fakeProvider{}, cost.Dependencies{})
}

// ── forwarding sanity: CountTokens / ReportCost don't emit ──────────────────

func TestLoggingProvider_CountTokens_ForwardsAndDoesNotEmit(t *testing.T) {
	t.Parallel()

	inner := &fakeProvider{countTokensResp: 42}
	app := &recordingAppender{}
	p := cost.NewLoggingProvider(inner, cost.Dependencies{AgentID: "agent", Logger: app})

	got, err := p.CountTokens(context.Background(), llm.CountTokensRequest{
		Model:    "claude",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "x"}},
	})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if got != 42 {
		t.Errorf("CountTokens = %d, want 42", got)
	}
	if n := app.callCount(); n != 0 {
		t.Errorf("appender call count = %d, want 0 (CountTokens does not emit)", n)
	}
}

func TestLoggingProvider_ReportCost_ForwardsAndDoesNotEmit(t *testing.T) {
	t.Parallel()

	inner := &fakeProvider{}
	app := &recordingAppender{}
	p := cost.NewLoggingProvider(inner, cost.Dependencies{AgentID: "agent", Logger: app})

	if err := p.ReportCost(context.Background(), "rt-1", llm.Usage{InputTokens: 1, OutputTokens: 2}); err != nil {
		t.Fatalf("ReportCost: %v", err)
	}
	if n := app.callCount(); n != 0 {
		t.Errorf("appender call count = %d, want 0 (ReportCost does not emit)", n)
	}
	if got := inner.reportCostCallCount(); got != 1 {
		t.Errorf("inner ReportCost call count = %d, want 1 (forwarded)", got)
	}
}

// ── helpers ─────────────────────────────────────────────────────────────────

// recordingAppender is a hand-rolled fake [cost.Appender] that records
// every Append call and optionally returns an injected error. Pattern
// mirrors keeperslog.fakeKeepClient (M3.6) and spawn.fakeKeepClient
// (M6.1.b) — no mocking library.
type recordingAppender struct {
	appendErr error

	mu    sync.Mutex
	calls []keeperslog.Event
}

func (a *recordingAppender) Append(_ context.Context, evt keeperslog.Event) (string, error) {
	a.mu.Lock()
	a.calls = append(a.calls, evt)
	a.mu.Unlock()
	if a.appendErr != nil {
		return "", a.appendErr
	}
	return "fake-row-id", nil
}

func (a *recordingAppender) recorded() []keeperslog.Event {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]keeperslog.Event, len(a.calls))
	copy(out, a.calls)
	return out
}

func (a *recordingAppender) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.calls)
}

// fakeLocalKeepClient is the [keeperslog.LocalKeepClient] stand-in used
// by the correlation-id and PII regression tests, so we can drive a real
// *keeperslog.Writer end-to-end and inspect the wire-shape envelope
// (which is the level the M2 redaction discipline applies to).
type fakeLocalKeepClient struct {
	mu    sync.Mutex
	calls []keepclient.LogAppendRequest
}

func (f *fakeLocalKeepClient) LogAppend(_ context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	return &keepclient.LogAppendResponse{ID: "fake-row"}, nil
}

func (f *fakeLocalKeepClient) recorded() []keepclient.LogAppendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keepclient.LogAppendRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeProvider is the hand-rolled [llm.Provider] used in this test
// suite. The package's own fakeProvider lives in `_test.go` (unexported)
// so we maintain a parallel local fake here. Keeps tests free of
// keepclient/keeperslog dependency leak across the llm package boundary.
type fakeProvider struct {
	completeResp    llm.CompleteResponse
	completeErr     error
	streamEvents    []llm.StreamEvent
	streamErr       error
	countTokensResp int
	countTokensErr  error
	reportCostErr   error

	completeCalls   atomic.Int64
	streamCalls     atomic.Int64
	reportCostCalls atomic.Int64
}

func (f *fakeProvider) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	f.completeCalls.Add(1)
	if f.completeErr != nil {
		return llm.CompleteResponse{}, f.completeErr
	}
	return f.completeResp, nil
}

func (f *fakeProvider) Stream(ctx context.Context, _ llm.StreamRequest, handler llm.StreamHandler) (llm.StreamSubscription, error) {
	f.streamCalls.Add(1)
	if f.streamErr != nil {
		return nil, f.streamErr
	}
	if handler == nil {
		return nil, llm.ErrInvalidHandler
	}
	for _, ev := range f.streamEvents {
		if err := handler(ctx, ev); err != nil {
			return &noopSub{}, nil
		}
	}
	return &noopSub{}, nil
}

func (f *fakeProvider) CountTokens(_ context.Context, _ llm.CountTokensRequest) (int, error) {
	if f.countTokensErr != nil {
		return 0, f.countTokensErr
	}
	return f.countTokensResp, nil
}

func (f *fakeProvider) ReportCost(_ context.Context, _ string, _ llm.Usage) error {
	f.reportCostCalls.Add(1)
	return f.reportCostErr
}

func (f *fakeProvider) reportCostCallCount() int64 {
	return f.reportCostCalls.Load()
}

// noopSub is the [llm.StreamSubscription] our fake returns. Real fakes
// surface stream-error propagation via the subscription; this suite
// exercises decorator behaviour independent of subscription error wiring,
// so a no-op shape is sufficient.
type noopSub struct{}

func (*noopSub) Stop() error { return nil }
