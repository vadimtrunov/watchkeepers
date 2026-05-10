package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeToolHandler is a hand-rolled stub used to assert dispatch
// invocation count + arguments + concurrency safety. Pattern mirrors
// the prior M*.c.* hand-rolled fakes (no mocking lib). `lastIn` is
// guarded by `mu` so concurrent dispatch tests do not race on the
// shared write — same discipline as `coordinator/fakeJiraUpdater`.
type fakeToolHandler struct {
	calls int32
	mu    sync.Mutex
	last  ToolCall
	out   ToolResult
	err   error
}

func (f *fakeToolHandler) handler() ToolHandler {
	return func(_ context.Context, call ToolCall) (ToolResult, error) {
		atomic.AddInt32(&f.calls, 1)
		f.mu.Lock()
		f.last = call
		f.mu.Unlock()
		return f.out, f.err
	}
}

func (f *fakeToolHandler) lastCall() ToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// coordinatorManifestForTests is the smallest [Manifest] shape that
// exercises the dispatcher's gate sequence: one tool in Toolset
// (`update_ticket_field`), autonomy=`autonomous`, authority matrix
// granting `self`. Distinct per-test mutations layer on top of this
// baseline (e.g. swapping autonomy to `supervised`, or adding a `lead`
// authority entry).
func coordinatorManifestForTests() Manifest {
	return Manifest{
		AgentID:      "coord-test-agent",
		SystemPrompt: "test prompt",
		Model:        "claude-sonnet-4-6",
		Autonomy:     AutonomyAutonomous,
		Toolset:      Toolset{{Name: "update_ticket_field"}},
		AuthorityMatrix: map[string]string{
			"update_ticket_field": "self",
		},
	}
}

func TestNewToolDispatcher_ReturnsUsableInstance(t *testing.T) {
	t.Parallel()

	d := NewToolDispatcher()
	if d == nil {
		t.Fatal("NewToolDispatcher returned nil")
	}
	if d.handlers == nil {
		t.Error("handlers map not initialised")
	}
}

func TestRegister_RejectsEmptyName(t *testing.T) {
	t.Parallel()

	d := NewToolDispatcher()
	err := d.Register("", (&fakeToolHandler{}).handler())
	if !errors.Is(err, ErrInvalidToolCall) {
		t.Fatalf("Register(\"\", h) = %v, want errors.Is ErrInvalidToolCall", err)
	}
}

func TestRegister_RejectsNilHandler(t *testing.T) {
	t.Parallel()

	d := NewToolDispatcher()
	err := d.Register("update_ticket_field", nil)
	if !errors.Is(err, ErrToolHandlerMissing) {
		t.Fatalf("Register(name, nil) = %v, want errors.Is ErrToolHandlerMissing", err)
	}
}

func TestRegister_OverridesPreviousHandler(t *testing.T) {
	t.Parallel()

	d := NewToolDispatcher()
	first := &fakeToolHandler{out: ToolResult{Output: map[string]any{"tag": "first"}}}
	second := &fakeToolHandler{out: ToolResult{Output: map[string]any{"tag": "second"}}}

	if err := d.Register("update_ticket_field", first.handler()); err != nil {
		t.Fatalf("Register first: %v", err)
	}
	if err := d.Register("update_ticket_field", second.handler()); err != nil {
		t.Fatalf("Register second: %v", err)
	}

	manifest := coordinatorManifestForTests()
	res, err := d.Dispatch(context.Background(), manifest, ToolCall{Name: "update_ticket_field"})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Output["tag"] != "second" {
		t.Errorf("Dispatch returned first handler's result; override did not take effect: %v", res)
	}
	if atomic.LoadInt32(&first.calls) != 0 {
		t.Errorf("first handler invoked %d times; want 0 after override", first.calls)
	}
	if atomic.LoadInt32(&second.calls) != 1 {
		t.Errorf("second handler invoked %d times; want 1", second.calls)
	}
}

func TestDispatch_HappyPath(t *testing.T) {
	t.Parallel()

	want := ToolResult{Output: map[string]any{"issue": "WK-42", "ok": true}}
	stub := &fakeToolHandler{out: want}
	d := NewToolDispatcher()
	if err := d.Register("update_ticket_field", stub.handler()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	manifest := coordinatorManifestForTests()
	call := ToolCall{Name: "update_ticket_field", Arguments: map[string]any{"key": "WK-42"}}

	got, err := d.Dispatch(context.Background(), manifest, call)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got.Output["issue"] != "WK-42" || got.Output["ok"] != true {
		t.Errorf("Output = %v, want %v", got.Output, want.Output)
	}
	if atomic.LoadInt32(&stub.calls) != 1 {
		t.Errorf("handler called %d times; want 1", stub.calls)
	}
	if got := stub.lastCall(); got.Arguments["key"] != "WK-42" {
		t.Errorf("handler received call.Arguments = %v, want key=WK-42", got.Arguments)
	}
}

func TestDispatch_PreCheckContextCancelled(t *testing.T) {
	t.Parallel()

	d := NewToolDispatcher()
	stub := &fakeToolHandler{}
	if err := d.Register("update_ticket_field", stub.handler()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := d.Dispatch(ctx, coordinatorManifestForTests(), ToolCall{Name: "update_ticket_field"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Dispatch on cancelled ctx returned %v, want context.Canceled", err)
	}
	if atomic.LoadInt32(&stub.calls) != 0 {
		t.Errorf("handler invoked despite ctx cancel; calls=%d", stub.calls)
	}
}

func TestDispatch_EmptyToolName(t *testing.T) {
	t.Parallel()

	d := NewToolDispatcher()
	_, err := d.Dispatch(context.Background(), coordinatorManifestForTests(), ToolCall{Name: ""})
	if !errors.Is(err, ErrInvalidToolCall) {
		t.Fatalf("Dispatch with empty Name returned %v, want ErrInvalidToolCall", err)
	}
}

func TestDispatch_ToolNotInToolset(t *testing.T) {
	t.Parallel()

	d := NewToolDispatcher()
	stub := &fakeToolHandler{}
	if err := d.Register("ghost_tool", stub.handler()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := d.Dispatch(context.Background(), coordinatorManifestForTests(), ToolCall{Name: "ghost_tool"})
	if !errors.Is(err, ErrToolUnauthorized) {
		t.Fatalf("Dispatch outside Toolset returned %v, want ErrToolUnauthorized", err)
	}
	if atomic.LoadInt32(&stub.calls) != 0 {
		t.Errorf("handler invoked despite Toolset deny; calls=%d", stub.calls)
	}
}

func TestDispatch_ToolsetGateRunsBeforeApprovalGate(t *testing.T) {
	t.Parallel()

	// Manifest has the tool out of Toolset BUT the matrix would require
	// lead approval if it were in. The dispatcher must surface
	// ErrToolUnauthorized, not ErrApprovalRequired — the manifest gate
	// is the strictest signal a typo could surface. Pin gate ordering.
	manifest := Manifest{
		AgentID:      "coord-test",
		SystemPrompt: "test",
		Model:        "claude-sonnet-4-6",
		Autonomy:     AutonomyAutonomous,
		Toolset:      Toolset{}, // empty
		AuthorityMatrix: map[string]string{
			"update_ticket_field": "lead",
		},
	}
	d := NewToolDispatcher()
	if err := d.Register("update_ticket_field", (&fakeToolHandler{}).handler()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := d.Dispatch(context.Background(), manifest, ToolCall{Name: "update_ticket_field"})
	if !errors.Is(err, ErrToolUnauthorized) {
		t.Fatalf("got %v, want ErrToolUnauthorized (membership precedes approval)", err)
	}
	if errors.Is(err, ErrApprovalRequired) {
		t.Errorf("got ErrApprovalRequired; membership gate must short-circuit before approval gate")
	}
}

func TestDispatch_ApprovalRequired_Autonomous_LeadValue(t *testing.T) {
	t.Parallel()

	manifest := coordinatorManifestForTests()
	manifest.AuthorityMatrix["update_ticket_field"] = "lead"

	stub := &fakeToolHandler{}
	d := NewToolDispatcher()
	if err := d.Register("update_ticket_field", stub.handler()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := d.Dispatch(context.Background(), manifest, ToolCall{Name: "update_ticket_field"})
	if !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("Dispatch returned %v, want errors.Is ErrApprovalRequired", err)
	}

	var apErr *ApprovalRequiredError
	if !errors.As(err, &apErr) {
		t.Fatalf("Dispatch error not unwrappable to *ApprovalRequiredError: %v", err)
	}
	if apErr.Action != "update_ticket_field" {
		t.Errorf("ApprovalRequiredError.Action = %q, want update_ticket_field", apErr.Action)
	}
	if apErr.Reason == "" {
		t.Error("ApprovalRequiredError.Reason is empty; runtime gate must produce a non-empty reason")
	}
	if !strings.Contains(apErr.Error(), "update_ticket_field") {
		t.Errorf("Error() = %q; want it to mention the action", apErr.Error())
	}
	if atomic.LoadInt32(&stub.calls) != 0 {
		t.Errorf("handler invoked despite approval-required deny; calls=%d", stub.calls)
	}
}

func TestDispatch_ApprovalRequired_Supervised(t *testing.T) {
	t.Parallel()

	// Supervised autonomy short-circuits to "every action requires
	// approval", regardless of matrix content (see authority.go table).
	// Pin the supervised path even though the Coordinator runs
	// autonomous in production — the Watchmaster is supervised and
	// future supervised agents will reuse this primitive.
	manifest := coordinatorManifestForTests()
	manifest.Autonomy = AutonomySupervised

	d := NewToolDispatcher()
	if err := d.Register("update_ticket_field", (&fakeToolHandler{}).handler()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := d.Dispatch(context.Background(), manifest, ToolCall{Name: "update_ticket_field"})
	if !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("Dispatch under supervised autonomy returned %v, want ErrApprovalRequired", err)
	}
}

func TestDispatch_ApprovalRequired_UnknownMatrixValueFailsClosed(t *testing.T) {
	t.Parallel()

	// An unrecognised matrix value (typo / future-vocab drift) MUST
	// fail closed. authority.go documents this as the defensive default.
	manifest := coordinatorManifestForTests()
	manifest.AuthorityMatrix["update_ticket_field"] = "council_of_elders"

	d := NewToolDispatcher()
	if err := d.Register("update_ticket_field", (&fakeToolHandler{}).handler()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := d.Dispatch(context.Background(), manifest, ToolCall{Name: "update_ticket_field"})
	if !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("unknown matrix value did not trigger ErrApprovalRequired: %v", err)
	}
	var apErr *ApprovalRequiredError
	if errors.As(err, &apErr) && !strings.Contains(apErr.Reason, "council_of_elders") {
		t.Errorf("reason %q does not surface the unknown value; future debugging requires it",
			apErr.Reason)
	}
}

func TestDispatch_HandlerMissing(t *testing.T) {
	t.Parallel()

	// Toolset declares the tool, matrix grants self, but no handler is
	// registered → ErrToolHandlerMissing (production wiring bug).
	d := NewToolDispatcher()
	_, err := d.Dispatch(
		context.Background(),
		coordinatorManifestForTests(),
		ToolCall{Name: "update_ticket_field"},
	)
	if !errors.Is(err, ErrToolHandlerMissing) {
		t.Fatalf("Dispatch with no handler returned %v, want ErrToolHandlerMissing", err)
	}
}

func TestDispatch_HandlerMissing_RunsAfterApprovalGate(t *testing.T) {
	t.Parallel()

	// If both gates would fire, the approval gate MUST surface first —
	// a missing handler in production is a wiring bug, but we don't
	// want to leak the absence as "needs approval" or vice versa.
	// Approval is the stricter signal and runs first.
	manifest := coordinatorManifestForTests()
	manifest.AuthorityMatrix["update_ticket_field"] = "lead"
	d := NewToolDispatcher() // no handler registered

	_, err := d.Dispatch(context.Background(), manifest, ToolCall{Name: "update_ticket_field"})
	if !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("got %v, want ErrApprovalRequired (approval precedes handler-missing)", err)
	}
	if errors.Is(err, ErrToolHandlerMissing) {
		t.Errorf("got ErrToolHandlerMissing; approval gate must short-circuit first")
	}
}

func TestDispatch_HandlerErrorForwarded(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("jira: 503 service unavailable")
	stub := &fakeToolHandler{err: wantErr}
	d := NewToolDispatcher()
	if err := d.Register("update_ticket_field", stub.handler()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := d.Dispatch(
		context.Background(),
		coordinatorManifestForTests(),
		ToolCall{Name: "update_ticket_field"},
	)
	if !errors.Is(err, wantErr) {
		t.Fatalf("handler error not forwarded; got %v, want %v", err, wantErr)
	}
}

func TestDispatch_HandlerToolResultErrorForwarded(t *testing.T) {
	t.Parallel()

	want := ToolResult{Error: "ticket not found", Output: map[string]any{"key": "WK-99"}}
	stub := &fakeToolHandler{out: want}
	d := NewToolDispatcher()
	if err := d.Register("update_ticket_field", stub.handler()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, err := d.Dispatch(
		context.Background(),
		coordinatorManifestForTests(),
		ToolCall{Name: "update_ticket_field"},
	)
	if err != nil {
		t.Fatalf("Dispatch returned err = %v; tool-side errors should ride on ToolResult.Error", err)
	}
	if got.Error != "ticket not found" {
		t.Errorf("ToolResult.Error = %q, want %q", got.Error, want.Error)
	}
}

func TestDispatch_ConcurrentInvocationSafety(t *testing.T) {
	t.Parallel()

	// 16 goroutines invoking the same tool against the same dispatcher
	// must all complete without race-detector trips. Shared state on
	// the handler is tracked via atomic counters; the dispatcher's own
	// state is read-locked during dispatch.
	stub := &fakeToolHandler{out: ToolResult{Output: map[string]any{"ok": true}}}
	d := NewToolDispatcher()
	if err := d.Register("update_ticket_field", stub.handler()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	manifest := coordinatorManifestForTests()

	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			call := ToolCall{
				Name:      "update_ticket_field",
				Arguments: map[string]any{"i": idx},
			}
			if _, err := d.Dispatch(context.Background(), manifest, call); err != nil {
				t.Errorf("goroutine %d Dispatch: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	if got := atomic.LoadInt32(&stub.calls); got != goroutines {
		t.Errorf("handler invoked %d times; want %d (one per goroutine)", got, goroutines)
	}
}

func TestDispatch_ConcurrentRegisterAndDispatch(t *testing.T) {
	t.Parallel()

	// Mixed concurrent Register + Dispatch must not race. Half the
	// goroutines re-register a fresh handler under the same name; the
	// other half dispatch. Either the new or the old handler may be
	// the one that ran any given dispatch — this test does NOT assert
	// handler identity (Register only writes, never deletes, so every
	// dispatch finds A handler). It is a `-race`-only assertion plus
	// a "no Dispatch surfaces ErrToolHandlerMissing during a
	// re-register" smoke; the runtime guarantee is that Register's
	// write-lock and Dispatch's read-lock serialise correctly.
	d := NewToolDispatcher()
	manifest := coordinatorManifestForTests()
	if err := d.Register("update_ticket_field", (&fakeToolHandler{}).handler()); err != nil {
		t.Fatalf("seed Register: %v", err)
	}

	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			if idx%2 == 0 {
				if err := d.Register(
					"update_ticket_field",
					(&fakeToolHandler{out: ToolResult{Output: map[string]any{"r": idx}}}).handler(),
				); err != nil {
					t.Errorf("goroutine %d Register: %v", idx, err)
				}
				return
			}
			if _, err := d.Dispatch(
				context.Background(), manifest,
				ToolCall{Name: "update_ticket_field"},
			); err != nil {
				t.Errorf("goroutine %d Dispatch: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()
}

// TestApprovalRequiredError_ErrorString_Format pins the rendering
// shape so callers writing log lines or surfacing the error to the
// agent get a stable string. A drift here would silently change the
// surface text in lessons / Keeper's Log.
func TestApprovalRequiredError_ErrorString_Format(t *testing.T) {
	t.Parallel()

	e := &ApprovalRequiredError{
		Action: "update_ticket_field",
		Reason: "authority matrix requires lead approval for update_ticket_field",
	}
	got := e.Error()
	wantPrefix := "runtime: approval required for update_ticket_field:"
	if !strings.HasPrefix(got, wantPrefix) {
		t.Errorf("Error() = %q, want prefix %q", got, wantPrefix)
	}
	if !strings.Contains(got, "lead approval") {
		t.Errorf("Error() = %q does not surface the reason", got)
	}
	// errors.Is identity check.
	if !errors.Is(e, ErrApprovalRequired) {
		t.Errorf("errors.Is(e, ErrApprovalRequired) = false; sentinel identity broken")
	}
	// errors.Is on a different sentinel must NOT match.
	other := errors.New("runtime: some other thing")
	if errors.Is(e, other) {
		t.Errorf("errors.Is(e, %v) = true; only ErrApprovalRequired must match", other)
	}
}

// TestApprovalRequiredError_FmtErrorfWraps demonstrates that callers
// can wrap the sentinel error with fmt.Errorf("...: %w", err) and
// still pull the typed value back via errors.As. Pin the contract
// because handlers in M8.2.b/c will likely wrap on their own paths.
func TestApprovalRequiredError_FmtErrorfWraps(t *testing.T) {
	t.Parallel()

	inner := &ApprovalRequiredError{Action: "x", Reason: "r"}
	wrapped := fmt.Errorf("coordinator: dispatch: %w", inner)

	var apErr *ApprovalRequiredError
	if !errors.As(wrapped, &apErr) {
		t.Fatalf("errors.As did not unwrap the wrapped *ApprovalRequiredError")
	}
	if apErr.Action != "x" {
		t.Errorf("unwrapped Action = %q, want x", apErr.Action)
	}
	if !errors.Is(wrapped, ErrApprovalRequired) {
		t.Errorf("errors.Is on wrapped error did not match ErrApprovalRequired")
	}
}
