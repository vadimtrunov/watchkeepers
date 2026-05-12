package approval

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestCallbackDispatcher() (
	d *CallbackDispatcher,
	pub *fakePublisher,
	store *fakeProposalLookup,
	clk *fakeClock,
	idGen *fakeIDGenerator,
	dryRun *fakeDryRunRequester,
	logger *fakeLogger,
	stored Proposal,
	decisions *fakeDecisionRecorder,
) {
	pub = &fakePublisher{}
	store = newFakeProposalLookup()
	clk = newFakeClock(time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC))
	idGen = &fakeIDGenerator{}
	dryRun = &fakeDryRunRequester{}
	logger = &fakeLogger{}
	decisions = newFakeDecisionRecorder()
	stored = newTestProposal()
	store.put(stored)
	d = NewCallbackDispatcher(CallbackDispatcherDeps{
		Lookup:          store,
		Decisions:       decisions,
		Publisher:       pub,
		Clock:           clk,
		IDGenerator:     idGen,
		DryRunRequester: dryRun,
		Logger:          logger,
	})
	return
}

func TestCallbackDispatcher_New_PanicsOnNilDeps(t *testing.T) {
	mk := func(mutate func(*CallbackDispatcherDeps)) (panicked bool, msg string) {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				msg = r.(string)
			}
		}()
		deps := CallbackDispatcherDeps{
			Lookup:      newFakeProposalLookup(),
			Decisions:   newFakeDecisionRecorder(),
			Publisher:   &fakePublisher{},
			Clock:       newFakeClock(time.Now()),
			IDGenerator: &fakeIDGenerator{},
		}
		mutate(&deps)
		_ = NewCallbackDispatcher(deps)
		return
	}
	for _, tt := range []struct {
		name   string
		mutate func(*CallbackDispatcherDeps)
		want   string
	}{
		{"Lookup", func(d *CallbackDispatcherDeps) { d.Lookup = nil }, "Lookup"},
		{"Decisions", func(d *CallbackDispatcherDeps) { d.Decisions = nil }, "Decisions"},
		{"Publisher", func(d *CallbackDispatcherDeps) { d.Publisher = nil }, "Publisher"},
		{"Clock", func(d *CallbackDispatcherDeps) { d.Clock = nil }, "Clock"},
		{"IDGenerator", func(d *CallbackDispatcherDeps) { d.IDGenerator = nil }, "IDGenerator"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			panicked, msg := mk(tt.mutate)
			if !panicked {
				t.Fatalf("expected panic")
			}
			if !strings.Contains(msg, tt.want) {
				t.Errorf("panic msg %q does not mention %q", msg, tt.want)
			}
		})
	}
}

func TestClick_Validate(t *testing.T) {
	good := Click{
		ActionID: EncodeApprovalActionID(mustNewUUIDv7()),
		Button:   ButtonActionApprove,
		LeadID:   "U123",
	}
	if err := good.Validate(); err != nil {
		t.Errorf("happy: %v", err)
	}

	bad := good
	bad.ActionID = ""
	if !errors.Is(bad.Validate(), ErrInvalidActionID) {
		t.Errorf("empty action_id must be ErrInvalidActionID")
	}

	bad = good
	bad.LeadID = ""
	if !errors.Is(bad.Validate(), ErrInvalidButtonValue) {
		t.Errorf("empty lead_id must be ErrInvalidButtonValue")
	}

	bad = good
	bad.Button = "x"
	if !errors.Is(bad.Validate(), ErrInvalidButtonValue) {
		t.Errorf("bad button must be ErrInvalidButtonValue")
	}

	bad = good
	bad.Button = ButtonActionTestInDM
	bad.LeadDMChannel = ""
	if !errors.Is(bad.Validate(), ErrCardMissingLeadDM) {
		t.Errorf("test-in-DM without DM channel must be ErrCardMissingLeadDM")
	}
}

func TestCallbackDispatcher_Approve_EmitsToolApproved(t *testing.T) {
	d, pub, _, _, _, _, _, stored, _ := newTestCallbackDispatcher()
	err := d.Dispatch(context.Background(), Click{
		ActionID: EncodeApprovalActionID(stored.ID),
		Button:   ButtonActionApprove,
		LeadID:   "U-LEAD-1",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	events := pub.eventsForTopic(TopicToolApproved)
	if len(events) != 1 {
		t.Fatalf("want 1 tool_approved, got %d", len(events))
	}
	got := events[0].event.(ToolApproved)
	if got.Route != RouteSlackNative {
		t.Errorf("Route: %s", got.Route)
	}
	if got.ApproverID != "U-LEAD-1" {
		t.Errorf("ApproverID: %s", got.ApproverID)
	}
	if got.SourceName != "" {
		t.Errorf("SourceName must be empty on slack-native: %s", got.SourceName)
	}
}

func TestCallbackDispatcher_Reject_EmitsToolRejected(t *testing.T) {
	d, pub, _, _, _, _, _, stored, _ := newTestCallbackDispatcher()
	err := d.Dispatch(context.Background(), Click{
		ActionID: EncodeApprovalActionID(stored.ID),
		Button:   ButtonActionReject,
		LeadID:   "U-LEAD-1",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(pub.eventsForTopic(TopicToolRejected)) != 1 {
		t.Errorf("expected 1 tool_rejected")
	}
}

func TestCallbackDispatcher_TestInMyDM_EmitsDryRunAndCallsRequester(t *testing.T) {
	d, pub, _, _, _, dryRun, _, stored, _ := newTestCallbackDispatcher()
	err := d.Dispatch(context.Background(), Click{
		ActionID:      EncodeApprovalActionID(stored.ID),
		Button:        ButtonActionTestInDM,
		LeadID:        "U-LEAD-1",
		LeadDMChannel: "D-LEAD-1",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	events := pub.eventsForTopic(TopicDryRunRequested)
	if len(events) != 1 {
		t.Fatalf("expected 1 dry_run_requested")
	}
	got := events[0].event.(DryRunRequested)
	if got.LeadDMChannel != "D-LEAD-1" {
		t.Errorf("LeadDMChannel: %s", got.LeadDMChannel)
	}
	if len(dryRun.calls) != 1 {
		t.Errorf("DryRunRequester not invoked: %d calls", len(dryRun.calls))
	}
}

func TestCallbackDispatcher_TestInMyDM_NilDryRunRequester_StillEmits(t *testing.T) {
	pub := &fakePublisher{}
	store := newFakeProposalLookup()
	stored := newTestProposal()
	store.put(stored)
	d := NewCallbackDispatcher(CallbackDispatcherDeps{
		Lookup:      store,
		Decisions:   newFakeDecisionRecorder(),
		Publisher:   pub,
		Clock:       newFakeClock(time.Now()),
		IDGenerator: &fakeIDGenerator{},
		// DryRunRequester is nil — documented M9.4.c-not-wired path.
	})
	err := d.Dispatch(context.Background(), Click{
		ActionID:      EncodeApprovalActionID(stored.ID),
		Button:        ButtonActionTestInDM,
		LeadID:        "U-LEAD-1",
		LeadDMChannel: "D-LEAD-1",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(pub.eventsForTopic(TopicDryRunRequested)) != 1 {
		t.Errorf("event must emit even when DryRunRequester is nil")
	}
}

func TestCallbackDispatcher_AskQuestions_EmitsQuestionAsked(t *testing.T) {
	d, pub, _, _, _, _, _, stored, _ := newTestCallbackDispatcher()
	err := d.Dispatch(context.Background(), Click{
		ActionID: EncodeApprovalActionID(stored.ID),
		Button:   ButtonActionAskQuestions,
		LeadID:   "U-LEAD-1",
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(pub.eventsForTopic(TopicQuestionAsked)) != 1 {
		t.Errorf("expected 1 question_asked")
	}
}

func TestCallbackDispatcher_ProposalNotFound_PassesThroughSentinel(t *testing.T) {
	d, _, _, _, _, _, _, _, _ := newTestCallbackDispatcher()
	missing := mustNewUUIDv7()
	err := d.Dispatch(context.Background(), Click{
		ActionID: EncodeApprovalActionID(missing),
		Button:   ButtonActionApprove,
		LeadID:   "U-LEAD-1",
	})
	if !errors.Is(err, ErrProposalNotFound) {
		t.Errorf("want ErrProposalNotFound, got %v", err)
	}
}

func TestCallbackDispatcher_InvalidActionID(t *testing.T) {
	d, _, _, _, _, _, _, _, _ := newTestCallbackDispatcher()
	err := d.Dispatch(context.Background(), Click{
		ActionID: "tool_approval:not-a-uuid",
		Button:   ButtonActionApprove,
		LeadID:   "U-LEAD-1",
	})
	if !errors.Is(err, ErrInvalidActionID) {
		t.Errorf("want ErrInvalidActionID, got %v", err)
	}
}

func TestCallbackDispatcher_PublishError_WrapsSentinel(t *testing.T) {
	d, pub, _, _, _, _, _, stored, _ := newTestCallbackDispatcher()
	pub.err = errors.New("eventbus closed")
	err := d.Dispatch(context.Background(), Click{
		ActionID: EncodeApprovalActionID(stored.ID),
		Button:   ButtonActionApprove,
		LeadID:   "U-LEAD-1",
	})
	if !errors.Is(err, ErrPublishToolApproved) {
		t.Errorf("want ErrPublishToolApproved, got %v", err)
	}
}

func TestCallbackDispatcher_CtxCancelled_Refuses(t *testing.T) {
	d, _, _, _, _, _, _, stored, _ := newTestCallbackDispatcher()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := d.Dispatch(ctx, Click{
		ActionID: EncodeApprovalActionID(stored.ID),
		Button:   ButtonActionApprove,
		LeadID:   "U-LEAD-1",
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestCallbackDispatcher_DryRunRequesterError_DoesNotMaskEvent(t *testing.T) {
	d, pub, _, _, _, dryRun, _, stored, _ := newTestCallbackDispatcher()
	dryRun.err = errors.New("executor not ready")
	err := d.Dispatch(context.Background(), Click{
		ActionID:      EncodeApprovalActionID(stored.ID),
		Button:        ButtonActionTestInDM,
		LeadID:        "U-LEAD-1",
		LeadDMChannel: "D-LEAD-1",
	})
	if err == nil || !strings.Contains(err.Error(), "executor not ready") {
		t.Errorf("error must surface executor failure: %v", err)
	}
	// Event must have landed before executor invocation.
	if len(pub.eventsForTopic(TopicDryRunRequested)) != 1 {
		t.Errorf("event must emit before executor invocation")
	}
}

func TestCallbackDispatcher_Concurrency_16Goroutines(t *testing.T) {
	d, pub, store, _, _, _, _, _, _ := newTestCallbackDispatcher()
	const n = 16
	ids := make([]uuid.UUID, n)
	for i := 0; i < n; i++ {
		p := newTestProposal()
		store.put(p)
		ids[i] = p.ID
	}
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			err := d.Dispatch(context.Background(), Click{
				ActionID: EncodeApprovalActionID(ids[i]),
				Button:   ButtonActionApprove,
				LeadID:   "U",
			})
			if err != nil {
				t.Errorf("Dispatch: %v", err)
			}
		}()
	}
	wg.Wait()
	if len(pub.eventsForTopic(TopicToolApproved)) != n {
		t.Errorf("expected %d approves, got %d", n, len(pub.eventsForTopic(TopicToolApproved)))
	}
}

func TestCallbackDispatcher_Approve_DuplicateClick_Idempotent(t *testing.T) {
	d, pub, _, _, _, _, _, stored, decisions := newTestCallbackDispatcher()

	click := Click{
		ActionID: EncodeApprovalActionID(stored.ID),
		Button:   ButtonActionApprove,
		LeadID:   "U-LEAD-1",
	}
	for i := 0; i < 3; i++ {
		if err := d.Dispatch(context.Background(), click); err != nil {
			t.Fatalf("click #%d: %v", i+1, err)
		}
	}
	if got := len(pub.eventsForTopic(TopicToolApproved)); got != 1 {
		t.Errorf("3 same-kind clicks must publish 1 event, got %d", got)
	}
	if got := decisions.markCount(); got != 3 {
		t.Errorf("MarkDecided expected 3 calls (1 first-time + 2 replay), got %d", got)
	}
}

func TestCallbackDispatcher_ApproveThenReject_Conflict(t *testing.T) {
	d, _, _, _, _, _, _, stored, _ := newTestCallbackDispatcher()

	if err := d.Dispatch(context.Background(), Click{
		ActionID: EncodeApprovalActionID(stored.ID),
		Button:   ButtonActionApprove,
		LeadID:   "U-LEAD-1",
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}
	err := d.Dispatch(context.Background(), Click{
		ActionID: EncodeApprovalActionID(stored.ID),
		Button:   ButtonActionReject,
		LeadID:   "U-LEAD-1",
	})
	if !errors.Is(err, ErrDecisionConflict) {
		t.Errorf("reject-after-approve: want ErrDecisionConflict, got %v", err)
	}
}

func TestCallbackDispatcher_PublishError_RollsBackDecision(t *testing.T) {
	d, pub, _, _, _, _, _, stored, decisions := newTestCallbackDispatcher()
	pub.err = errors.New("eventbus closed")

	click := Click{
		ActionID: EncodeApprovalActionID(stored.ID),
		Button:   ButtonActionApprove,
		LeadID:   "U-LEAD-1",
	}
	if err := d.Dispatch(context.Background(), click); !errors.Is(err, ErrPublishToolApproved) {
		t.Fatalf("first attempt: want ErrPublishToolApproved, got %v", err)
	}
	if got := decisions.unmarkCount(); got != 1 {
		t.Errorf("publish error must roll back the claim; got unmark count %d", got)
	}
	beforeRetry := len(pub.eventsForTopic(TopicToolApproved))

	// Retry with publish succeeding — the cleared claim must allow
	// re-publishing rather than silent-no-op'ing.
	pub.err = nil
	if err := d.Dispatch(context.Background(), click); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := len(pub.eventsForTopic(TopicToolApproved)); got <= beforeRetry {
		t.Errorf("retry must re-invoke Publish after rollback; before=%d after=%d", beforeRetry, got)
	}
}

func TestCallbackDispatcher_PIICanary_LoggerRedaction(t *testing.T) {
	const canaryPurpose = "CANARY_PURPOSE_PII_zzzzzz"
	const canaryDesc = "CANARY_DESC_PII_zzzzzz"
	const canaryCode = "CANARY_CODE_PII_zzzzzz"
	d, pub, store, _, _, _, logger, stored, _ := newTestCallbackDispatcher()
	stored.Input.Purpose = canaryPurpose
	stored.Input.PlainLanguageDescription = canaryDesc
	stored.Input.CodeDraft = canaryCode
	store.put(stored)
	pub.err = errors.New("force-log path")
	_ = d.Dispatch(context.Background(), Click{
		ActionID: EncodeApprovalActionID(stored.ID),
		Button:   ButtonActionApprove,
		LeadID:   "U-LEAD-1",
	})
	for _, e := range logger.snapshot() {
		joined := e.msg
		for _, v := range e.kv {
			joined += "|" + asString(v)
		}
		for _, canary := range []string{canaryPurpose, canaryDesc, canaryCode} {
			if strings.Contains(joined, canary) {
				t.Errorf("logger entry leaked canary %q: %s", canary, joined)
			}
		}
	}
}
