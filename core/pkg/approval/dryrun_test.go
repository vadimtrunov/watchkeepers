package approval

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// ----- helpers -----

// newTestExecutor wires an [*Executor] with default fakes and returns
// the executor + every fake so tests can drive specific scenarios.
func newTestExecutor() (*Executor, *fakePublisher, *fakeClock, *fakeBrokerForwarder, *fakeLogger) {
	pub := &fakePublisher{}
	clk := newFakeClock(time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC))
	fwd := &fakeBrokerForwarder{}
	logger := &fakeLogger{}
	defaultScope := Scope{
		LeadDMChannel:      "D-LEAD-DM-123",
		JiraSandboxProject: "SANDBOX",
	}
	e := NewExecutor(ExecutorDeps{
		Publisher:     pub,
		Clock:         clk,
		ScopeResolver: constScopeResolver(defaultScope, nil),
		Forwarder:     fwd,
		Logger:        logger,
	})
	return e, pub, clk, fwd, logger
}

// validRequest returns a fully-populated [Request] that passes
// every field validation. Base fixture for tests that mutate a
// single field.
func validRequest(mode toolregistry.DryRunMode) Request {
	id := mustNewUUIDv7()
	return Request{
		ProposalID: id,
		ToolName:   "count_open_prs",
		Mode:       mode,
		Invocations: []BrokerInvocation{
			{
				Kind: BrokerSlack,
				Op:   "send_message",
				Args: map[string]string{
					"channel": "C-PROD-123",
					"text":    "Hello world",
				},
			},
			{
				Kind: BrokerJira,
				Op:   "create_issue",
				Args: map[string]string{
					"project": "PROD",
					"summary": "Investigate flake",
				},
			},
		},
	}
}

// ----- constructor panics -----

func TestNewExecutor_PanicsOnNilPublisher(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(fmt.Sprint(r), "deps.Publisher must not be nil") {
			t.Errorf("expected named-field panic, got %v", r)
		}
	}()
	NewExecutor(ExecutorDeps{
		Clock:         newFakeClock(time.Now()),
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "d", JiraSandboxProject: "S"}, nil),
	})
}

func TestNewExecutor_PanicsOnNilClock(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(fmt.Sprint(r), "deps.Clock must not be nil") {
			t.Errorf("expected named-field panic, got %v", r)
		}
	}()
	NewExecutor(ExecutorDeps{
		Publisher:     &fakePublisher{},
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "d", JiraSandboxProject: "S"}, nil),
	})
}

func TestNewExecutor_PanicsOnNilScopeResolver(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil || !strings.Contains(fmt.Sprint(r), "deps.ScopeResolver must not be nil") {
			t.Errorf("expected named-field panic, got %v", r)
		}
	}()
	NewExecutor(ExecutorDeps{
		Publisher: &fakePublisher{},
		Clock:     newFakeClock(time.Now()),
	})
}

func TestNewExecutor_NilForwarderAccepted(t *testing.T) {
	t.Parallel()
	e := NewExecutor(ExecutorDeps{
		Publisher:     &fakePublisher{},
		Clock:         newFakeClock(time.Now()),
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "d", JiraSandboxProject: "S"}, nil),
	})
	if e == nil {
		t.Fatal("expected non-nil executor")
	}
}

func TestNewExecutor_NilLoggerAccepted(t *testing.T) {
	t.Parallel()
	e := NewExecutor(ExecutorDeps{
		Publisher:     &fakePublisher{},
		Clock:         newFakeClock(time.Now()),
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "d", JiraSandboxProject: "S"}, nil),
	})
	if e == nil {
		t.Fatal("expected non-nil executor")
	}
}

// ----- closed-set enum validators -----

func TestBrokerKind_Validate(t *testing.T) {
	t.Parallel()
	for _, ok := range []BrokerKind{BrokerSlack, BrokerJira} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%s should be valid: %v", ok, err)
		}
	}
	for _, bad := range []BrokerKind{"", "github", "SLACK"} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidBrokerKind) {
			t.Errorf("%q should fail with ErrInvalidBrokerKind: %v", bad, err)
		}
	}
}

func TestScope_Validate(t *testing.T) {
	t.Parallel()
	good := Scope{LeadDMChannel: "D1", JiraSandboxProject: "SANDBOX"}
	if err := good.Validate(); err != nil {
		t.Errorf("good scope: %v", err)
	}
	for _, bad := range []Scope{
		{LeadDMChannel: "", JiraSandboxProject: "SANDBOX"},
		{LeadDMChannel: "D1", JiraSandboxProject: ""},
		{LeadDMChannel: "  ", JiraSandboxProject: "SANDBOX"},
		{LeadDMChannel: "D1", JiraSandboxProject: "  "},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidScope) {
			t.Errorf("%+v: expected ErrInvalidScope, got %v", bad, err)
		}
	}
}

// ----- Request.Validate -----

func TestRequest_Validate_RejectsZeroProposalID(t *testing.T) {
	t.Parallel()
	req := validRequest(toolregistry.DryRunModeGhost)
	req.ProposalID = uuid.Nil
	if err := req.Validate(); !errors.Is(err, ErrInvalidDryRunRequest) {
		t.Errorf("expected ErrInvalidDryRunRequest, got %v", err)
	}
}

func TestRequest_Validate_RejectsEmptyToolName(t *testing.T) {
	t.Parallel()
	req := validRequest(toolregistry.DryRunModeGhost)
	req.ToolName = "  "
	if err := req.Validate(); !errors.Is(err, ErrInvalidDryRunRequest) {
		t.Errorf("expected ErrInvalidDryRunRequest, got %v", err)
	}
}

func TestRequest_Validate_RejectsOversizedToolName(t *testing.T) {
	t.Parallel()
	req := validRequest(toolregistry.DryRunModeGhost)
	req.ToolName = strings.Repeat("a", MaxToolNameLength+1)
	if err := req.Validate(); !errors.Is(err, ErrInvalidDryRunRequest) {
		t.Errorf("expected ErrInvalidDryRunRequest, got %v", err)
	}
}

func TestRequest_Validate_RejectsInvalidMode(t *testing.T) {
	t.Parallel()
	req := validRequest(toolregistry.DryRunModeGhost)
	req.Mode = "telepathy"
	if err := req.Validate(); !errors.Is(err, toolregistry.ErrInvalidDryRunMode) {
		t.Errorf("expected ErrInvalidDryRunMode, got %v", err)
	}
}

func TestRequest_Validate_RejectsEmptyMode(t *testing.T) {
	t.Parallel()
	req := validRequest(toolregistry.DryRunModeGhost)
	req.Mode = ""
	if err := req.Validate(); !errors.Is(err, toolregistry.ErrInvalidDryRunMode) {
		t.Errorf("expected ErrInvalidDryRunMode, got %v", err)
	}
}

func TestRequest_Validate_RejectsTooManyInvocations(t *testing.T) {
	t.Parallel()
	req := validRequest(toolregistry.DryRunModeGhost)
	req.Invocations = make([]BrokerInvocation, MaxInvocationsPerRequest+1)
	for i := range req.Invocations {
		req.Invocations[i] = BrokerInvocation{Kind: BrokerSlack, Op: "send_message"}
	}
	if err := req.Validate(); !errors.Is(err, ErrInvocationsExceedLimit) {
		t.Errorf("expected ErrInvocationsExceedLimit, got %v", err)
	}
}

func TestRequest_Validate_AcceptsEmptyInvocations(t *testing.T) {
	t.Parallel()
	req := validRequest(toolregistry.DryRunModeGhost)
	req.Invocations = nil
	if err := req.Validate(); err != nil {
		t.Errorf("expected nil for empty invocations (a tool with no broker writes is valid), got %v", err)
	}
}

func TestRequest_Validate_BoundaryAcceptedAtMaxInvocations(t *testing.T) {
	t.Parallel()
	req := validRequest(toolregistry.DryRunModeGhost)
	req.Invocations = make([]BrokerInvocation, MaxInvocationsPerRequest)
	for i := range req.Invocations {
		req.Invocations[i] = BrokerInvocation{Kind: BrokerSlack, Op: "send_message"}
	}
	if err := req.Validate(); err != nil {
		t.Errorf("boundary at MaxInvocationsPerRequest should pass, got %v", err)
	}
}

func TestBrokerInvocation_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		inv  BrokerInvocation
		want error
	}{
		{
			name: "happy",
			inv:  BrokerInvocation{Kind: BrokerSlack, Op: "send_message"},
			want: nil,
		},
		{
			name: "invalid_kind",
			inv:  BrokerInvocation{Kind: "github", Op: "create_pr"},
			want: ErrInvalidBrokerKind,
		},
		{
			name: "empty_op",
			inv:  BrokerInvocation{Kind: BrokerSlack, Op: ""},
			want: ErrInvalidBrokerInvocation,
		},
		{
			name: "whitespace_op",
			inv:  BrokerInvocation{Kind: BrokerSlack, Op: "  "},
			want: ErrInvalidBrokerInvocation,
		},
		{
			name: "oversized_op",
			inv:  BrokerInvocation{Kind: BrokerSlack, Op: strings.Repeat("a", MaxBrokerOpLength+1)},
			want: ErrInvalidBrokerInvocation,
		},
		{
			name: "too_many_args",
			inv: func() BrokerInvocation {
				args := make(map[string]string, MaxBrokerArgCount+1)
				for i := 0; i < MaxBrokerArgCount+1; i++ {
					args[fmt.Sprintf("k%d", i)] = "v"
				}
				return BrokerInvocation{Kind: BrokerSlack, Op: "x", Args: args}
			}(),
			want: ErrInvalidBrokerArgs,
		},
		{
			name: "oversized_arg_value",
			inv: BrokerInvocation{Kind: BrokerSlack, Op: "x", Args: map[string]string{
				"k": strings.Repeat("v", MaxBrokerArgValueLength+1),
			}},
			want: ErrInvalidBrokerArgs,
		},
		{
			name: "oversized_arg_key",
			inv: BrokerInvocation{Kind: BrokerSlack, Op: "x", Args: map[string]string{
				strings.Repeat("k", MaxBrokerArgKeyLength+1): "v",
			}},
			want: ErrInvalidBrokerArgs,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := c.inv.Validate()
			if c.want == nil && got != nil {
				t.Errorf("expected nil, got %v", got)
			}
			if c.want != nil && !errors.Is(got, c.want) {
				t.Errorf("expected %v, got %v", c.want, got)
			}
		})
	}
}

func TestRequest_Validate_PropagatesPerInvocationSentinel(t *testing.T) {
	t.Parallel()
	req := validRequest(toolregistry.DryRunModeGhost)
	req.Invocations[1].Op = ""
	err := req.Validate()
	if !errors.Is(err, ErrInvalidBrokerInvocation) {
		t.Errorf("expected ErrInvalidBrokerInvocation, got %v", err)
	}
	if !strings.Contains(err.Error(), "invocations[1]") {
		t.Errorf("expected index annotation, got %v", err)
	}
}

// ----- ctx-cancel discipline -----

func TestExecute_CtxPreCancelled_NoneMode(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := newTestExecutor()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.Execute(ctx, validRequest(toolregistry.DryRunModeNone))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestExecute_CtxPreCancelled_GhostMode(t *testing.T) {
	t.Parallel()
	e, pub, _, _, _ := newTestExecutor()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.Execute(ctx, validRequest(toolregistry.DryRunModeGhost))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("publisher invoked on pre-cancelled ctx: %v", pub.snapshot())
	}
}

func TestExecute_ValidationBeforeCtxCancel(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := newTestExecutor()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := validRequest(toolregistry.DryRunModeGhost)
	req.ProposalID = uuid.Nil
	_, err := e.Execute(ctx, req)
	if !errors.Is(err, ErrInvalidDryRunRequest) {
		t.Errorf("expected validation to fire before ctx.Err, got %v", err)
	}
}

// ----- none mode -----

func TestExecute_NoneMode_ReturnsPreApprovalWarning(t *testing.T) {
	t.Parallel()
	e, pub, _, fwd, _ := newTestExecutor()
	req := validRequest(toolregistry.DryRunModeNone)
	trace, err := e.Execute(context.Background(), req)
	if !errors.Is(err, ErrPreApprovalWarning) {
		t.Errorf("expected ErrPreApprovalWarning, got %v", err)
	}
	if !strings.Contains(err.Error(), req.ToolName) {
		t.Errorf("expected error to name tool, got %v", err)
	}
	if len(trace.Outcomes) != 0 {
		t.Errorf("expected empty trace under none, got %d outcomes", len(trace.Outcomes))
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("publisher invoked under none mode: %v", pub.snapshot())
	}
	if len(fwd.snapshot()) != 0 {
		t.Errorf("forwarder invoked under none mode: %v", fwd.snapshot())
	}
}

// ----- ghost mode -----

func TestExecute_GhostMode_NoForwarderCalls(t *testing.T) {
	t.Parallel()
	e, pub, _, fwd, _ := newTestExecutor()
	req := validRequest(toolregistry.DryRunModeGhost)
	trace, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(fwd.snapshot()) != 0 {
		t.Errorf("ghost mode forwarded to broker: %v", fwd.snapshot())
	}
	if len(trace.Outcomes) != 2 {
		t.Fatalf("expected 2 outcomes, got %d", len(trace.Outcomes))
	}
	for _, o := range trace.Outcomes {
		if o.Disposition != DispositionGhosted {
			t.Errorf("expected DispositionGhosted, got %q", o.Disposition)
		}
	}
	if pubEv := pub.eventsForTopic(TopicDryRunExecuted); len(pubEv) != 1 {
		t.Errorf("expected 1 TopicDryRunExecuted event, got %d", len(pubEv))
	}
}

func TestExecute_GhostMode_OriginalEqualsEffective(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := newTestExecutor()
	req := validRequest(toolregistry.DryRunModeGhost)
	originalChannel := req.Invocations[0].Args["channel"]
	originalProject := req.Invocations[1].Args["project"]
	trace, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := trace.Outcomes[0].Original.Args["channel"]; got != originalChannel {
		t.Errorf("Original.channel mutated: got %q want %q", got, originalChannel)
	}
	if got := trace.Outcomes[0].Effective.Args["channel"]; got != originalChannel {
		t.Errorf("ghost Effective.channel must equal Original: got %q want %q", got, originalChannel)
	}
	if got := trace.Outcomes[1].Effective.Args["project"]; got != originalProject {
		t.Errorf("ghost Effective.project must equal Original: got %q want %q", got, originalProject)
	}
}

// ----- scoped mode -----

func TestExecute_ScopedMode_RewritesSlackChannel(t *testing.T) {
	t.Parallel()
	e, _, _, fwd, _ := newTestExecutor()
	req := validRequest(toolregistry.DryRunModeScoped)
	trace, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	const wantChannel = "D-LEAD-DM-123"
	// Original preserved
	if got := trace.Outcomes[0].Original.Args["channel"]; got != "C-PROD-123" {
		t.Errorf("Original.channel mutated: %q", got)
	}
	// Effective rewritten
	if got := trace.Outcomes[0].Effective.Args["channel"]; got != wantChannel {
		t.Errorf("Effective.channel: got %q want %q", got, wantChannel)
	}
	// Forwarder saw rewritten
	calls := fwd.snapshot()
	if len(calls) != 2 {
		t.Fatalf("expected 2 forward calls, got %d", len(calls))
	}
	if got := calls[0].Args["channel"]; got != wantChannel {
		t.Errorf("forwarder slack channel: got %q want %q", got, wantChannel)
	}
}

// TestExecute_ScopedMode_JiraSandboxCanary asserts the per-deployment
// sandbox project id is the destination every Jira invocation lands on
// under scoped mode. The Original is preserved; the Effective is
// rewritten; the Forwarder sees the sandbox. Canary per the roadmap
// text: "a canary asserts the scoped Jira destination is the sandbox
// project ID configured at the deployment level".
func TestExecute_ScopedMode_JiraSandboxCanary(t *testing.T) {
	t.Parallel()
	e, _, _, fwd, _ := newTestExecutor()
	req := validRequest(toolregistry.DryRunModeScoped)
	const wantProject = "SANDBOX"
	trace, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Original preserved
	if got := trace.Outcomes[1].Original.Args["project"]; got != "PROD" {
		t.Errorf("Original.project mutated: %q", got)
	}
	// Effective rewritten
	if got := trace.Outcomes[1].Effective.Args["project"]; got != wantProject {
		t.Errorf("Effective.project: got %q want %q", got, wantProject)
	}
	calls := fwd.snapshot()
	if got := calls[1].Args["project"]; got != wantProject {
		t.Errorf("forwarder jira project: got %q want %q (DEPLOYMENT SANDBOX CANARY)", got, wantProject)
	}
}

func TestExecute_ScopedMode_NilForwarderStillSurfacesTrace(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	clk := newFakeClock(time.Now())
	e := NewExecutor(ExecutorDeps{
		Publisher:     pub,
		Clock:         clk,
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "D1", JiraSandboxProject: "SBX"}, nil),
	})
	trace, err := e.Execute(context.Background(), validRequest(toolregistry.DryRunModeScoped))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(trace.Outcomes) != 2 {
		t.Errorf("expected 2 outcomes despite nil forwarder, got %d", len(trace.Outcomes))
	}
	for _, o := range trace.Outcomes {
		if o.Disposition != DispositionScoped {
			t.Errorf("expected DispositionScoped, got %q", o.Disposition)
		}
	}
}

func TestExecute_ScopedMode_ScopeResolverError(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	clk := newFakeClock(time.Now())
	resolverErr := errors.New("vault unreachable")
	e := NewExecutor(ExecutorDeps{
		Publisher:     pub,
		Clock:         clk,
		ScopeResolver: constScopeResolver(Scope{}, resolverErr),
	})
	_, err := e.Execute(context.Background(), validRequest(toolregistry.DryRunModeScoped))
	if !errors.Is(err, ErrScopeResolution) {
		t.Errorf("expected ErrScopeResolution, got %v", err)
	}
	if !errors.Is(err, resolverErr) {
		t.Errorf("expected wrapped cause, got %v", err)
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("publisher invoked on scope-resolver error: %v", pub.snapshot())
	}
}

func TestExecute_ScopedMode_EmptyResolvedScope(t *testing.T) {
	t.Parallel()
	e := NewExecutor(ExecutorDeps{
		Publisher:     &fakePublisher{},
		Clock:         newFakeClock(time.Now()),
		ScopeResolver: constScopeResolver(Scope{}, nil),
	})
	_, err := e.Execute(context.Background(), validRequest(toolregistry.DryRunModeScoped))
	if !errors.Is(err, ErrEmptyResolvedScope) {
		t.Errorf("expected ErrEmptyResolvedScope, got %v", err)
	}
}

func TestExecute_ScopedMode_PartialScopeFailsValidation(t *testing.T) {
	t.Parallel()
	e := NewExecutor(ExecutorDeps{
		Publisher:     &fakePublisher{},
		Clock:         newFakeClock(time.Now()),
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "D1"}, nil),
	})
	_, err := e.Execute(context.Background(), validRequest(toolregistry.DryRunModeScoped))
	if !errors.Is(err, ErrInvalidScope) {
		t.Errorf("expected ErrInvalidScope, got %v", err)
	}
}

// TestExecute_ScopedMode_ForwarderErrorOnFirstReturnsPartialTrace
// covers the first-invocation-fail branch (no prior side effect).
// The failing invocation IS appended to the trace with
// [Outcome.ForwardErrMsg] populated (iter-1 codex C fix). Publish
// does NOT fire because no side effects landed.
func TestExecute_ScopedMode_ForwarderErrorOnFirstReturnsPartialTrace(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	clk := newFakeClock(time.Now())
	forwardErr := errors.New("slack 500")
	fwd := &fakeBrokerForwarder{err: forwardErr, errAfter: 1}
	e := NewExecutor(ExecutorDeps{
		Publisher:     pub,
		Clock:         clk,
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "D1", JiraSandboxProject: "SBX"}, nil),
		Forwarder:     fwd,
	})
	trace, err := e.Execute(context.Background(), validRequest(toolregistry.DryRunModeScoped))
	if !errors.Is(err, ErrBrokerForward) {
		t.Errorf("expected ErrBrokerForward, got %v", err)
	}
	if !errors.Is(err, forwardErr) {
		t.Errorf("expected wrapped cause, got %v", err)
	}
	if !strings.Contains(err.Error(), "invocations[0]") {
		t.Errorf("expected index annotation, got %v", err)
	}
	// Failing invocation IS appended (iter-1 codex C fix).
	if len(trace.Outcomes) != 1 {
		t.Fatalf("expected 1 outcome (the failing one), got %d", len(trace.Outcomes))
	}
	failing := trace.Outcomes[0]
	if failing.Disposition != DispositionScoped {
		t.Errorf("failing outcome disposition: got %q want %q", failing.Disposition, DispositionScoped)
	}
	if failing.ForwardErrMsg != forwardErr.Error() {
		t.Errorf("failing outcome ForwardErrMsg: got %q want %q", failing.ForwardErrMsg, forwardErr.Error())
	}
	if len(fwd.snapshot()) != 1 {
		t.Errorf("expected forwarder to have attempted 1 call before failing, got %d", len(fwd.snapshot()))
	}
	// No side effects landed → publish does NOT fire.
	if len(pub.snapshot()) != 0 {
		t.Errorf("publisher invoked despite no successful side effects: %v", pub.snapshot())
	}
}

// TestExecute_ScopedMode_ForwarderErrorAfterSuccessPublishesPartial
// covers the M9.4.c critical "side effects fired then forwarder
// failed mid-stream" path (iter-1 critic M3 fix). The forwarder
// succeeds on call 1, fails on call 2. The partial trace MUST:
//   - contain 2 outcomes (success + failing)
//   - have the failing outcome's ForwardErrMsg populated
//   - be PUBLISHED on the eventbus (the event is the durable record
//     of side-effects-fired; the audit subscriber needs it)
func TestExecute_ScopedMode_ForwarderErrorAfterSuccessPublishesPartial(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	clk := newFakeClock(time.Now())
	forwardErr := errors.New("jira 500")
	fwd := &fakeBrokerForwarder{err: forwardErr, errAfter: 2}
	e := NewExecutor(ExecutorDeps{
		Publisher:     pub,
		Clock:         clk,
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "D1", JiraSandboxProject: "SBX"}, nil),
		Forwarder:     fwd,
	})
	trace, err := e.Execute(context.Background(), validRequest(toolregistry.DryRunModeScoped))
	if !errors.Is(err, ErrBrokerForward) {
		t.Errorf("expected ErrBrokerForward, got %v", err)
	}
	if !errors.Is(err, forwardErr) {
		t.Errorf("expected wrapped cause, got %v", err)
	}
	if !strings.Contains(err.Error(), "invocations[1]") {
		t.Errorf("expected index annotation for invocation[1], got %v", err)
	}
	if len(trace.Outcomes) != 2 {
		t.Fatalf("expected 2 outcomes (success + failing), got %d", len(trace.Outcomes))
	}
	if trace.Outcomes[0].ForwardErrMsg != "" {
		t.Errorf("first outcome ForwardErrMsg should be empty, got %q", trace.Outcomes[0].ForwardErrMsg)
	}
	if trace.Outcomes[1].ForwardErrMsg != forwardErr.Error() {
		t.Errorf("second outcome ForwardErrMsg: got %q want %q", trace.Outcomes[1].ForwardErrMsg, forwardErr.Error())
	}
	// Partial trace IS published (iter-1 critic M3 fix).
	events := pub.eventsForTopic(TopicDryRunExecuted)
	if len(events) != 1 {
		t.Errorf("expected partial trace to be published, got %d events", len(events))
	}
}

// TestExecute_ScopedMode_PerIterationCtxCancelHonoured covers the
// per-loop-iteration ctx-check (iter-1 critic M2 fix). With a 3-
// invocation request, cancelling AFTER invocation 1 means
// invocation 2 + 3 MUST NOT reach the forwarder. The partial trace
// IS published (side effects fired).
func TestExecute_ScopedMode_PerIterationCtxCancelHonoured(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	clk := newFakeClock(time.Now())
	cancelAfterCall := 1
	var calls int32
	ctx, cancel := context.WithCancel(context.Background())
	fwd := &cancellingBrokerForwarder{
		callCount: &calls,
		cancelOn:  int32(cancelAfterCall),
		cancel:    cancel,
	}
	e := NewExecutor(ExecutorDeps{
		Publisher:     pub,
		Clock:         clk,
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "D1", JiraSandboxProject: "SBX"}, nil),
		Forwarder:     fwd,
	})
	req := validRequest(toolregistry.DryRunModeScoped)
	req.Invocations = append(req.Invocations, BrokerInvocation{
		Kind: BrokerSlack,
		Op:   "send_message",
		Args: map[string]string{"channel": "C-3", "text": "third"},
	})
	trace, err := e.Execute(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if got := int(calls); got != 1 {
		t.Errorf("expected exactly 1 forwarder call before cancel propagation, got %d", got)
	}
	if len(trace.Outcomes) != 1 {
		t.Errorf("expected 1 outcome (the one before cancel), got %d", len(trace.Outcomes))
	}
	// Partial trace is published — side effect fired.
	if got := len(pub.eventsForTopic(TopicDryRunExecuted)); got != 1 {
		t.Errorf("expected partial trace publish on side-effects-fired cancel, got %d events", got)
	}
}

// TestExecute_ScopedMode_PublishDespiteCancelAfterSideEffects covers
// the "all forwards succeeded; caller cancelled before publish" path
// (iter-1 critic M3 fix). The pre-publish ctx-gate is SKIPPED when
// side effects fired; the publish MUST land regardless.
func TestExecute_ScopedMode_PublishDespiteCancelAfterSideEffects(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	clk := newFakeClock(time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	var lastForwardDone sync.WaitGroup
	lastForwardDone.Add(1)
	fwd := &cancellingBrokerForwarder{
		callCount: new(int32),
		cancelOn:  2,
		cancel:    cancel,
		onCancel:  &lastForwardDone,
	}
	e := NewExecutor(ExecutorDeps{
		Publisher:     pub,
		Clock:         clk,
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "D1", JiraSandboxProject: "SBX"}, nil),
		Forwarder:     fwd,
	})
	req := validRequest(toolregistry.DryRunModeScoped) // 2 invocations
	trace, err := e.Execute(ctx, req)
	// Cancel fires DURING the 2nd Forward; the 2nd Forward succeeds
	// (cancellingBrokerForwarder cancels but returns nil). After the
	// loop, the executor SKIPS the pre-publish ctx-gate because
	// sideEffectsFired=true, and publish lands.
	if err != nil {
		t.Errorf("expected nil err (publish despite cancel), got %v", err)
	}
	if len(trace.Outcomes) != 2 {
		t.Errorf("expected 2 outcomes, got %d", len(trace.Outcomes))
	}
	if got := len(pub.eventsForTopic(TopicDryRunExecuted)); got != 1 {
		t.Errorf("expected publish despite caller-cancel, got %d events", got)
	}
}

// TestExecute_ScopedMode_ForwarderMutationDoesNotCorruptTrace covers
// the iter-1 codex M1 defence: a forwarder that mutates the supplied
// invocation's Args map (a buggy implementer) MUST NOT corrupt the
// stored [Outcome.Effective].
func TestExecute_ScopedMode_ForwarderMutationDoesNotCorruptTrace(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	clk := newFakeClock(time.Now())
	fwd := &mutatingBrokerForwarder{
		injectKey: "_forwarder_injected",
		injectVal: "TAMPERED",
	}
	e := NewExecutor(ExecutorDeps{
		Publisher:     pub,
		Clock:         clk,
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "D1", JiraSandboxProject: "SBX"}, nil),
		Forwarder:     fwd,
	})
	trace, err := e.Execute(context.Background(), validRequest(toolregistry.DryRunModeScoped))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for i, o := range trace.Outcomes {
		if _, leaked := o.Effective.Args["_forwarder_injected"]; leaked {
			t.Errorf("outcome[%d].Effective leaked forwarder-mutated key", i)
		}
	}
}

// TestRewriteForScope_RefusesOverboundArgs covers the iter-1 codex
// M2 fix: a Slack invocation at exactly MaxBrokerArgCount with NO
// pre-existing `channel` key would be grown to MaxBrokerArgCount+1
// by the rewrite. The rewrite MUST refuse with
// [ErrInvalidBrokerArgs].
func TestExecute_ScopedMode_RewriteOverboundRefused(t *testing.T) {
	t.Parallel()
	pub := &fakePublisher{}
	clk := newFakeClock(time.Now())
	e := NewExecutor(ExecutorDeps{
		Publisher:     pub,
		Clock:         clk,
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "D1", JiraSandboxProject: "SBX"}, nil),
	})
	// MaxBrokerArgCount keys, none named "channel" → rewrite adds
	// "channel" → MaxBrokerArgCount+1 → refused.
	args := make(map[string]string, MaxBrokerArgCount)
	for i := 0; i < MaxBrokerArgCount; i++ {
		args[fmt.Sprintf("k%d", i)] = "v"
	}
	req := Request{
		ProposalID: mustNewUUIDv7(),
		ToolName:   "tight_packer",
		Mode:       toolregistry.DryRunModeScoped,
		Invocations: []BrokerInvocation{
			{Kind: BrokerSlack, Op: "send_message", Args: args},
		},
	}
	_, err := e.Execute(context.Background(), req)
	if !errors.Is(err, ErrInvalidBrokerArgs) {
		t.Errorf("expected ErrInvalidBrokerArgs after rewrite-grew-past-bound, got %v", err)
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("publisher invoked despite rewrite refusal: %v", pub.snapshot())
	}
}

// TestDisposition_Validate covers iter-1 critic m4 fix: Disposition
// closed-set enum validator.
func TestDisposition_Validate(t *testing.T) {
	t.Parallel()
	for _, ok := range []Disposition{DispositionGhosted, DispositionScoped} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%s should be valid: %v", ok, err)
		}
	}
	for _, bad := range []Disposition{"", "GHOSTED", "rejected"} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidDisposition) {
			t.Errorf("%q should fail with ErrInvalidDisposition: %v", bad, err)
		}
	}
}

// ----- publish-error wrapping -----

func TestExecute_PublishError_Wrapped(t *testing.T) {
	t.Parallel()
	pubErr := errors.New("eventbus closed")
	pub := &fakePublisher{err: pubErr}
	clk := newFakeClock(time.Now())
	e := NewExecutor(ExecutorDeps{
		Publisher:     pub,
		Clock:         clk,
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "D1", JiraSandboxProject: "SBX"}, nil),
	})
	_, err := e.Execute(context.Background(), validRequest(toolregistry.DryRunModeGhost))
	if !errors.Is(err, ErrPublishDryRunExecuted) {
		t.Errorf("expected ErrPublishDryRunExecuted, got %v", err)
	}
	if !errors.Is(err, pubErr) {
		t.Errorf("expected wrapped cause, got %v", err)
	}
}

// ----- publish-ctx detached from caller cancel -----

func TestExecute_PublishCtxDetachedFromCallerCancel(t *testing.T) {
	t.Parallel()
	sp := &slowPublisher{delay: 50 * time.Millisecond}
	clk := newFakeClock(time.Now())
	e := NewExecutor(ExecutorDeps{
		Publisher:     sp,
		Clock:         clk,
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "D1", JiraSandboxProject: "SBX"}, nil),
	})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, err := e.Execute(ctx, validRequest(toolregistry.DryRunModeGhost))
	if err != nil {
		t.Errorf("Execute: expected detached-ctx publish to succeed, got %v", err)
	}
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.contextCancels != 0 {
		t.Errorf("publish ctx propagated caller cancel (saw %d cancels)", sp.contextCancels)
	}
}

// ----- defensive deep copy -----

func TestExecute_DefensiveDeepCopy_OriginalArgs(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := newTestExecutor()
	req := validRequest(toolregistry.DryRunModeGhost)
	trace, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Mutate the caller's request post-Execute.
	req.Invocations[0].Args["channel"] = "MUTATED"
	if got := trace.Outcomes[0].Original.Args["channel"]; got != "C-PROD-123" {
		t.Errorf("caller-side mutation leaked into Trace: got %q", got)
	}
}

func TestExecute_DefensiveDeepCopy_RewriteDoesNotCorruptOriginal(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := newTestExecutor()
	req := validRequest(toolregistry.DryRunModeScoped)
	trace, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// Mutate Effective; Original must not change.
	trace.Outcomes[0].Effective.Args["text"] = "tampered"
	if got := trace.Outcomes[0].Original.Args["text"]; got != "Hello world" {
		t.Errorf("Effective mutation bled into Original: got %q", got)
	}
}

// ----- PII discipline -----

// TestExecute_EventPayloadOmitsArgsCanary stuffs synthetic ASCII
// canaries into every BrokerInvocation.Args value and asserts the
// verbatim %+v dump of the published [DryRunExecuted] event payload
// never contains them. The eventbus boundary excludes Args by
// construction — the canary harness catches a future addition that
// silently flows Args onto the payload.
func TestExecute_EventPayloadOmitsArgsCanary(t *testing.T) {
	t.Parallel()
	e, pub, _, _, _ := newTestExecutor()
	const slackBodyCanary = "Z9_SLACK_BODY_CANARY_Z9"
	const jiraSummaryCanary = "Z9_JIRA_SUMMARY_CANARY_Z9"
	const slackChannelCanary = "Z9_SLACK_CHANNEL_CANARY_Z9"
	const jiraProjectCanary = "Z9_JIRA_PROJECT_CANARY_Z9"

	req := validRequest(toolregistry.DryRunModeGhost)
	req.Invocations[0].Args["channel"] = slackChannelCanary
	req.Invocations[0].Args["text"] = slackBodyCanary
	req.Invocations[1].Args["project"] = jiraProjectCanary
	req.Invocations[1].Args["summary"] = jiraSummaryCanary

	_, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	events := pub.eventsForTopic(TopicDryRunExecuted)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	payload := fmt.Sprintf("%+v", events[0].event)
	for _, canary := range []string{slackBodyCanary, jiraSummaryCanary, slackChannelCanary, jiraProjectCanary} {
		if strings.Contains(payload, canary) {
			t.Errorf("eventbus payload contains caller-supplied Args canary %q; payload=%s", canary, payload)
		}
	}
}

// TestExecute_LoggerOmitsArgsCanary asserts the diagnostic logger
// never receives an Args body, even on failure paths. The forwarder-
// error branch is the densest log call site under scoped mode.
func TestExecute_LoggerOmitsArgsCanary(t *testing.T) {
	t.Parallel()
	clk := newFakeClock(time.Now())
	logger := &fakeLogger{}
	forwardErr := errors.New("slack 500")
	fwd := &fakeBrokerForwarder{err: forwardErr}
	e := NewExecutor(ExecutorDeps{
		Publisher:     &fakePublisher{},
		Clock:         clk,
		ScopeResolver: constScopeResolver(Scope{LeadDMChannel: "D-LEAD", JiraSandboxProject: "SBX"}, nil),
		Forwarder:     fwd,
		Logger:        logger,
	})
	const canary = "Z9_LOG_CANARY_Z9"
	req := validRequest(toolregistry.DryRunModeScoped)
	req.Invocations[0].Args["text"] = canary
	req.Invocations[1].Args["summary"] = canary

	_, _ = e.Execute(context.Background(), req)
	for _, entry := range logger.snapshot() {
		dump := fmt.Sprintf("%+v", entry)
		if strings.Contains(dump, canary) {
			t.Errorf("logger entry contains Args canary: %s", dump)
		}
	}
}

// TestExecute_TraceSurfacesArgsForCaller asserts the in-process Trace
// DOES surface the per-invocation Args (the "would have done X, Y, Z"
// report). PII discipline is "Args stay in-process; eventbus is
// metadata-only" — this test pins the in-process side of the boundary.
func TestExecute_TraceSurfacesArgsForCaller(t *testing.T) {
	t.Parallel()
	e, _, _, _, _ := newTestExecutor()
	const canary = "Z9_TRACE_CANARY_Z9"
	req := validRequest(toolregistry.DryRunModeGhost)
	req.Invocations[0].Args["text"] = canary

	trace, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got := trace.Outcomes[0].Original.Args["text"]; got != canary {
		t.Errorf("in-process Trace should surface caller Args: got %q want %q", got, canary)
	}
}

// ----- source-grep AC: no keeperslog imports, no .Append( calls -----
//
// CWD invariant: `go test` runs with the package directory as the
// working directory; the relative `dryrun.go` reads the file under
// test. Same discipline as M9.4.a's `proposer_test.go` source-grep.
// stripLineComments removes any `// ... $` trailing comment from a
// line so the source-grep AC can target real symbol references
// without false-positives on inline comments OR multi-line block
// comments (iter-1 critic n1 fix). Block comments `/* ... */` are
// also stripped end-to-end. The implementation is line-oriented so
// it does not need full Go AST parsing; it is sufficient for the
// audit-discipline AC which targets the literal symbol surface.
func stripLineComments(src string) string {
	// Remove `/* ... */` block comments end-to-end (greedy across
	// lines).
	for {
		i := strings.Index(src, "/*")
		if i < 0 {
			break
		}
		j := strings.Index(src[i:], "*/")
		if j < 0 {
			break
		}
		src = src[:i] + src[i+j+2:]
	}
	// Strip line `//` comments (best-effort; does not handle `//`
	// inside string literals — none in this file by convention).
	out := make([]string, 0, 128)
	for _, line := range strings.Split(src, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestDryRunSource_NoKeeperslogImport(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("dryrun.go")
	if err != nil {
		t.Fatalf("read dryrun.go: %v", err)
	}
	stripped := stripLineComments(string(body))
	if strings.Contains(stripped, "keeperslog") {
		t.Errorf("dryrun.go references keeperslog outside a comment — audit discipline violated")
	}
}

func TestDryRunSource_NoAppendCall(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("dryrun.go")
	if err != nil {
		t.Fatalf("read dryrun.go: %v", err)
	}
	stripped := stripLineComments(string(body))
	if strings.Contains(stripped, ".Append(") {
		t.Errorf("dryrun.go uses .Append( outside a comment — audit discipline violated")
	}
}

// ----- concurrency -----

func TestExecute_Concurrency_GhostMode(t *testing.T) {
	t.Parallel()
	e, pub, _, fwd, _ := newTestExecutor()
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := validRequest(toolregistry.DryRunModeGhost)
			if _, err := e.Execute(context.Background(), req); err != nil {
				t.Errorf("goroutine Execute: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(pub.eventsForTopic(TopicDryRunExecuted)) != n {
		t.Errorf("expected %d events under 16-goroutine ghost concurrency, got %d", n, len(pub.eventsForTopic(TopicDryRunExecuted)))
	}
	if len(fwd.snapshot()) != 0 {
		t.Errorf("ghost mode forwarded under concurrency: %d", len(fwd.snapshot()))
	}
}

func TestExecute_Concurrency_ScopedMode(t *testing.T) {
	t.Parallel()
	e, pub, _, fwd, _ := newTestExecutor()
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := validRequest(toolregistry.DryRunModeScoped)
			if _, err := e.Execute(context.Background(), req); err != nil {
				t.Errorf("goroutine Execute: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(pub.eventsForTopic(TopicDryRunExecuted)) != n {
		t.Errorf("expected %d events under 16-goroutine scoped concurrency, got %d", n, len(pub.eventsForTopic(TopicDryRunExecuted)))
	}
	if len(fwd.snapshot()) != 2*n {
		t.Errorf("expected %d forward calls (2 per request), got %d", 2*n, len(fwd.snapshot()))
	}
}

// ----- trace metadata -----

func TestExecute_GhostMode_TraceFields(t *testing.T) {
	t.Parallel()
	e, _, clk, _, _ := newTestExecutor()
	req := validRequest(toolregistry.DryRunModeGhost)
	trace, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if trace.ProposalID != req.ProposalID {
		t.Errorf("ProposalID: got %v want %v", trace.ProposalID, req.ProposalID)
	}
	if trace.ToolName != req.ToolName {
		t.Errorf("ToolName: got %q want %q", trace.ToolName, req.ToolName)
	}
	if trace.Mode != toolregistry.DryRunModeGhost {
		t.Errorf("Mode: got %q want %q", trace.Mode, toolregistry.DryRunModeGhost)
	}
	if !trace.ExecutedAt.Equal(clk.Now()) {
		t.Errorf("ExecutedAt: got %v want %v", trace.ExecutedAt, clk.Now())
	}
	if trace.CorrelationID != req.ProposalID.String() {
		t.Errorf("CorrelationID: got %q want %q", trace.CorrelationID, req.ProposalID.String())
	}
}

func TestExecute_GhostMode_EmitsEventBrokerKindCounts(t *testing.T) {
	t.Parallel()
	e, pub, _, _, _ := newTestExecutor()
	req := validRequest(toolregistry.DryRunModeGhost)
	_, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	events := pub.eventsForTopic(TopicDryRunExecuted)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	ev, ok := events[0].event.(DryRunExecuted)
	if !ok {
		t.Fatalf("unexpected event type %T", events[0].event)
	}
	if ev.BrokerKindCounts[string(BrokerSlack)] != 1 {
		t.Errorf("slack count: got %d want 1 (%+v)", ev.BrokerKindCounts[string(BrokerSlack)], ev.BrokerKindCounts)
	}
	if ev.BrokerKindCounts[string(BrokerJira)] != 1 {
		t.Errorf("jira count: got %d want 1 (%+v)", ev.BrokerKindCounts[string(BrokerJira)], ev.BrokerKindCounts)
	}
	if ev.InvocationCount != 2 {
		t.Errorf("InvocationCount: got %d want 2", ev.InvocationCount)
	}
	if ev.Mode != toolregistry.DryRunModeGhost {
		t.Errorf("Mode: got %q want %q", ev.Mode, toolregistry.DryRunModeGhost)
	}
}

func TestExecute_GhostMode_EmptyInvocationsStillPublishes(t *testing.T) {
	t.Parallel()
	e, pub, _, _, _ := newTestExecutor()
	req := validRequest(toolregistry.DryRunModeGhost)
	req.Invocations = nil
	trace, err := e.Execute(context.Background(), req)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(trace.Outcomes) != 0 {
		t.Errorf("expected 0 outcomes for empty invocations, got %d", len(trace.Outcomes))
	}
	events := pub.eventsForTopic(TopicDryRunExecuted)
	if len(events) != 1 {
		t.Fatalf("expected 1 event for empty invocations, got %d", len(events))
	}
	ev := events[0].event.(DryRunExecuted)
	if ev.InvocationCount != 0 {
		t.Errorf("InvocationCount: got %d want 0", ev.InvocationCount)
	}
}
