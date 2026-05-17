package budget_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k/audit"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k/budget"
)

// recordingIncTokensRepo is the hand-rolled [budget.Repository] stand-in
// the test suite uses. Records every IncTokens call and returns a
// configurable post-increment value + error. No mocking-library imports
// per the project's hand-rolled-fakes discipline (see
// `docs/lessons/M1.md` 2026-05-17 entry on `recordingAuditor`).
type recordingIncTokensRepo struct {
	mu       sync.Mutex
	calls    []recordedInc
	postInc  int64
	err      error
	startVal int64
}

type recordedInc struct {
	id    uuid.UUID
	delta int64
}

func (r *recordingIncTokensRepo) IncTokens(_ context.Context, id uuid.UUID, delta int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedInc{id: id, delta: delta})
	if r.err != nil {
		return 0, r.err
	}
	r.startVal += delta
	r.postInc = r.startVal
	return r.postInc, nil
}

func (r *recordingIncTokensRepo) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// recordingEmitter is the hand-rolled [audit.Emitter] stand-in.
// Records every EmitOverBudget call; every other method short-circuits
// with an "unused" error so a stray emit from a future contributor
// fails loudly. Mirrors the M1.4 audit-emission test discipline.
type recordingEmitter struct {
	mu         sync.Mutex
	overBudget []audit.OverBudgetEvent
	err        error
}

func (r *recordingEmitter) EmitConversationOpened(_ context.Context, _ audit.ConversationOpenedEvent) (string, error) {
	return "", errors.New("recordingEmitter: EmitConversationOpened not used")
}

func (r *recordingEmitter) EmitConversationClosed(_ context.Context, _ audit.ConversationClosedEvent) (string, error) {
	return "", errors.New("recordingEmitter: EmitConversationClosed not used")
}

func (r *recordingEmitter) EmitMessageSent(_ context.Context, _ audit.MessageSentEvent) (string, error) {
	return "", errors.New("recordingEmitter: EmitMessageSent not used")
}

func (r *recordingEmitter) EmitMessageReceived(_ context.Context, _ audit.MessageReceivedEvent) (string, error) {
	return "", errors.New("recordingEmitter: EmitMessageReceived not used")
}

func (r *recordingEmitter) EmitOverBudget(_ context.Context, evt audit.OverBudgetEvent) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overBudget = append(r.overBudget, evt)
	if r.err != nil {
		return "", r.err
	}
	return "row-over-budget", nil
}

func (r *recordingEmitter) EmitEscalated(_ context.Context, _ audit.EscalatedEvent) (string, error) {
	return "", errors.New("recordingEmitter: EmitEscalated not used")
}

func (r *recordingEmitter) overBudgetSnapshot() []audit.OverBudgetEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]audit.OverBudgetEvent, len(r.overBudget))
	copy(out, r.overBudget)
	return out
}

// recordingTrigger is the hand-rolled [budget.EscalationTrigger]
// stand-in.
type recordingTrigger struct {
	mu       sync.Mutex
	triggers []budget.OverBudgetTrigger
	err      error
}

func (r *recordingTrigger) TriggerOverBudget(_ context.Context, params budget.OverBudgetTrigger) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.triggers = append(r.triggers, params)
	return r.err
}

func (r *recordingTrigger) snapshot() []budget.OverBudgetTrigger {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]budget.OverBudgetTrigger, len(r.triggers))
	copy(out, r.triggers)
	return out
}

// recordingLogger captures Printf calls so the diagnostic-logger
// integration test can assert the log line shape.
type recordingLogger struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordingLogger) Printf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *recordingLogger) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}

// fixedTime + sampleParams reduce boilerplate in the per-test setup.
var fixedTime = time.Date(2026, time.May, 17, 12, 0, 0, 0, time.UTC)

func sampleParams(t *testing.T) budget.ChargeParams {
	t.Helper()
	return budget.ChargeParams{
		ConversationID:      uuid.New(),
		OrganizationID:      uuid.New(),
		ActingWatchkeeperID: "wk-actor",
		TokenBudget:         100,
		Delta:               10,
	}
}

func TestNewWriter_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic, got none")
		}
	}()
	_ = budget.NewWriter(nil, &recordingEmitter{})
}

func TestNewWriter_PanicsOnNilEmitter(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic, got none")
		}
	}()
	_ = budget.NewWriter(&recordingIncTokensRepo{}, nil)
}

func TestNoopEscalationTrigger_ReturnsNil(t *testing.T) {
	t.Parallel()
	err := budget.NoopEscalationTrigger{}.TriggerOverBudget(context.Background(), budget.OverBudgetTrigger{
		ConversationID: uuid.New(),
		OrganizationID: uuid.New(),
		ObservedAt:     fixedTime,
	})
	if err != nil {
		t.Fatalf("TriggerOverBudget returned %v, want nil", err)
	}
}

func TestEstimateTokensFromBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body []byte
		want int64
	}{
		{"empty", nil, 0},
		{"single byte", []byte{0x01}, 1},
		{"three bytes minimum-1", []byte("abc"), 1},
		{"exactly one token", []byte("abcd"), 1},
		{"five bytes ceil", []byte("abcde"), 2},
		{"sixteen bytes", []byte("abcdefghijklmnop"), 4},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := budget.EstimateTokensFromBody(tc.body)
			if got != tc.want {
				t.Errorf("EstimateTokensFromBody(%q) = %d, want %d", string(tc.body), got, tc.want)
			}
		})
	}
}

func TestResolveBudget(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name          string
		defaultBudget int64
		override      int64
		ok            bool
		want          int64
	}{
		{"no override falls back to default", 100, 0, false, 100},
		{"explicit zero override disables enforcement", 100, 0, true, 0},
		{"positive override wins", 100, 200, true, 200},
		{"negative override clamps to zero", 100, -5, true, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := budget.ResolveBudget(tc.defaultBudget, tc.override, tc.ok)
			if got != tc.want {
				t.Errorf("ResolveBudget(%d,%d,%v) = %d, want %d", tc.defaultBudget, tc.override, tc.ok, got, tc.want)
			}
		})
	}
}

func TestCharge_ValidationFailures(t *testing.T) {
	t.Parallel()
	repo := &recordingIncTokensRepo{}
	emitter := &recordingEmitter{}
	w := budget.NewWriter(repo, emitter)

	bad := []struct {
		name   string
		params budget.ChargeParams
	}{
		{"zero conversation", budget.ChargeParams{
			OrganizationID: uuid.New(), ActingWatchkeeperID: "wk", Delta: 1,
		}},
		{"zero organization", budget.ChargeParams{
			ConversationID: uuid.New(), ActingWatchkeeperID: "wk", Delta: 1,
		}},
		{"empty acting", budget.ChargeParams{
			ConversationID: uuid.New(), OrganizationID: uuid.New(), Delta: 1,
		}},
		{"whitespace acting", budget.ChargeParams{
			ConversationID: uuid.New(), OrganizationID: uuid.New(), ActingWatchkeeperID: "  ", Delta: 1,
		}},
		{"zero delta", budget.ChargeParams{
			ConversationID: uuid.New(), OrganizationID: uuid.New(), ActingWatchkeeperID: "wk", Delta: 0,
		}},
		{"negative delta", budget.ChargeParams{
			ConversationID: uuid.New(), OrganizationID: uuid.New(), ActingWatchkeeperID: "wk", Delta: -1,
		}},
		{"negative budget", budget.ChargeParams{
			ConversationID: uuid.New(), OrganizationID: uuid.New(), ActingWatchkeeperID: "wk", Delta: 1, TokenBudget: -1,
		}},
	}
	for _, tc := range bad {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := w.Charge(context.Background(), tc.params)
			if !errors.Is(err, budget.ErrInvalidChargeParams) {
				t.Errorf("Charge err = %v, want errors.Is ErrInvalidChargeParams", err)
			}
		})
	}
	if repo.callCount() != 0 {
		t.Errorf("validation failures triggered IncTokens %d times, want 0", repo.callCount())
	}
	if len(emitter.overBudgetSnapshot()) != 0 {
		t.Errorf("validation failures triggered EmitOverBudget, want none")
	}
}

func TestCharge_PreCancelledCtxRefuses(t *testing.T) {
	t.Parallel()
	repo := &recordingIncTokensRepo{}
	emitter := &recordingEmitter{}
	w := budget.NewWriter(repo, emitter)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.Charge(ctx, sampleParams(t))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Charge err = %v, want context.Canceled", err)
	}
	if repo.callCount() != 0 {
		t.Errorf("pre-cancelled ctx triggered IncTokens, want none")
	}
}

func TestCharge_UnderBudget_NoEmitNoTrigger(t *testing.T) {
	t.Parallel()
	repo := &recordingIncTokensRepo{startVal: 0}
	emitter := &recordingEmitter{}
	trigger := &recordingTrigger{}
	w := budget.NewWriter(repo, emitter, budget.WithEscalationTrigger(trigger))

	res, err := w.Charge(context.Background(), budget.ChargeParams{
		ConversationID:      uuid.New(),
		OrganizationID:      uuid.New(),
		ActingWatchkeeperID: "wk",
		TokenBudget:         100,
		Delta:               10,
	})
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if res.OverBudget {
		t.Errorf("OverBudget = true, want false (10 < 100)")
	}
	if res.TokensUsed != 10 {
		t.Errorf("TokensUsed = %d, want 10", res.TokensUsed)
	}
	if got := len(emitter.overBudgetSnapshot()); got != 0 {
		t.Errorf("EmitOverBudget called %d times, want 0", got)
	}
	if got := len(trigger.snapshot()); got != 0 {
		t.Errorf("TriggerOverBudget called %d times, want 0", got)
	}
}

// overBudgetSetup wires a Writer pinned to a fixed clock with a
// repository pre-seeded at 95 tokens — so a single 10-token charge
// (95 + 10 = 105) reliably trips the 100-budget crossing. Hoisted so
// the result-side and the emit/trigger-side assertions live in
// separate tests and each stays under the gocyclo budget.
func overBudgetSetup(t *testing.T) (*budget.Writer, *recordingEmitter, *recordingTrigger, uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	repo := &recordingIncTokensRepo{startVal: 95}
	emitter := &recordingEmitter{}
	trigger := &recordingTrigger{}
	w := budget.NewWriter(
		repo, emitter,
		budget.WithEscalationTrigger(trigger),
		budget.WithNow(func() time.Time { return fixedTime }),
	)
	return w, emitter, trigger, uuid.New(), uuid.New(), uuid.New()
}

func TestCharge_OverBudget_ResultShape(t *testing.T) {
	t.Parallel()
	w, _, _, convID, orgID, corrID := overBudgetSetup(t)
	res, err := w.Charge(context.Background(), budget.ChargeParams{
		ConversationID:      convID,
		OrganizationID:      orgID,
		ActingWatchkeeperID: "wk-acting",
		TokenBudget:         100,
		Delta:               10,
		CorrelationID:       corrID,
	})
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if !res.OverBudget {
		t.Errorf("OverBudget = false, want true (105 > 100)")
	}
	if res.TokensUsed != 105 {
		t.Errorf("TokensUsed = %d, want 105", res.TokensUsed)
	}
	if res.TokenBudget != 100 {
		t.Errorf("TokenBudget = %d, want 100", res.TokenBudget)
	}
}

func TestCharge_OverBudget_EmitsAuditEvent(t *testing.T) {
	t.Parallel()
	w, emitter, _, convID, orgID, corrID := overBudgetSetup(t)
	_, err := w.Charge(context.Background(), budget.ChargeParams{
		ConversationID:      convID,
		OrganizationID:      orgID,
		ActingWatchkeeperID: "wk-acting",
		TokenBudget:         100,
		Delta:               10,
		CorrelationID:       corrID,
	})
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	evs := emitter.overBudgetSnapshot()
	if len(evs) != 1 {
		t.Fatalf("EmitOverBudget called %d times, want 1", len(evs))
	}
	assertOverBudgetEvent(t, evs[0], convID, orgID)
}

// assertOverBudgetEvent walks every expected field on the over-budget
// audit event so the per-field error messages stay informative without
// inflating the calling test's cyclomatic complexity.
func assertOverBudgetEvent(t *testing.T, ev audit.OverBudgetEvent, convID, orgID uuid.UUID) {
	t.Helper()
	if ev.ConversationID != convID {
		t.Errorf("event.ConversationID = %s, want %s", ev.ConversationID, convID)
	}
	if ev.OrganizationID != orgID {
		t.Errorf("event.OrganizationID = %s, want %s", ev.OrganizationID, orgID)
	}
	if ev.TokenBudget != 100 {
		t.Errorf("event.TokenBudget = %d, want 100", ev.TokenBudget)
	}
	if ev.TokensUsed != 105 {
		t.Errorf("event.TokensUsed = %d, want 105", ev.TokensUsed)
	}
	if !ev.ObservedAt.Equal(fixedTime) {
		t.Errorf("event.ObservedAt = %v, want %v", ev.ObservedAt, fixedTime)
	}
}

func TestCharge_OverBudget_TriggersEscalation(t *testing.T) {
	t.Parallel()
	w, _, trigger, convID, orgID, corrID := overBudgetSetup(t)
	_, err := w.Charge(context.Background(), budget.ChargeParams{
		ConversationID:      convID,
		OrganizationID:      orgID,
		ActingWatchkeeperID: "wk-acting",
		TokenBudget:         100,
		Delta:               10,
		CorrelationID:       corrID,
	})
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	trigs := trigger.snapshot()
	if len(trigs) != 1 {
		t.Fatalf("TriggerOverBudget called %d times, want 1", len(trigs))
	}
	assertOverBudgetTrigger(t, trigs[0], convID, corrID)
}

// assertOverBudgetTrigger walks every expected field on the escalation
// trigger payload. Hoisted for the same reason as assertOverBudgetEvent.
func assertOverBudgetTrigger(t *testing.T, tr budget.OverBudgetTrigger, convID, corrID uuid.UUID) {
	t.Helper()
	if tr.ConversationID != convID {
		t.Errorf("trigger.ConversationID = %s, want %s", tr.ConversationID, convID)
	}
	if tr.ActingWatchkeeperID != "wk-acting" {
		t.Errorf("trigger.ActingWatchkeeperID = %q, want %q", tr.ActingWatchkeeperID, "wk-acting")
	}
	if tr.TokensUsed != 105 {
		t.Errorf("trigger.TokensUsed = %d, want 105", tr.TokensUsed)
	}
	if tr.TokenBudget != 100 {
		t.Errorf("trigger.TokenBudget = %d, want 100", tr.TokenBudget)
	}
	if tr.CorrelationID != corrID {
		t.Errorf("trigger.CorrelationID = %s, want %s", tr.CorrelationID, corrID)
	}
	if !tr.ObservedAt.Equal(fixedTime) {
		t.Errorf("trigger.ObservedAt = %v, want %v", tr.ObservedAt, fixedTime)
	}
}

// TestCharge_OnlyFirstCrossingEmitsAndTriggers pins the iter-1 codex
// P1 Major fix: once a conversation is over budget, subsequent
// charges that stay over budget must NOT re-emit or re-trigger.
func TestCharge_OnlyFirstCrossingEmitsAndTriggers(t *testing.T) {
	t.Parallel()
	repo := &recordingIncTokensRepo{startVal: 0}
	emitter := &recordingEmitter{}
	trigger := &recordingTrigger{}
	w := budget.NewWriter(
		repo, emitter,
		budget.WithEscalationTrigger(trigger),
		budget.WithNow(func() time.Time { return fixedTime }),
	)
	convID := uuid.New()
	orgID := uuid.New()
	mkParams := func(delta int64) budget.ChargeParams {
		return budget.ChargeParams{
			ConversationID:      convID,
			OrganizationID:      orgID,
			ActingWatchkeeperID: "wk",
			TokenBudget:         100,
			Delta:               delta,
		}
	}
	// First charge: 0 + 90 = 90 (still under budget).
	res, err := w.Charge(context.Background(), mkParams(90))
	if err != nil {
		t.Fatalf("Charge #1: %v", err)
	}
	if res.OverBudget {
		t.Errorf("OverBudget after 90 = true, want false")
	}
	// Second charge: 90 + 20 = 110 (CROSSES the 100 budget). This call
	// fires the side effects (the only one that should).
	res, err = w.Charge(context.Background(), mkParams(20))
	if err != nil {
		t.Fatalf("Charge #2: %v", err)
	}
	if !res.OverBudget {
		t.Errorf("OverBudget after 110 = false, want true")
	}
	if got := len(emitter.overBudgetSnapshot()); got != 1 {
		t.Errorf("emit count after crossing = %d, want 1", got)
	}
	if got := len(trigger.snapshot()); got != 1 {
		t.Errorf("trigger count after crossing = %d, want 1", got)
	}
	// Third + fourth charge: stays over budget. Result.OverBudget stays
	// true (it's the post-state) but emit + trigger MUST NOT fire again.
	res, err = w.Charge(context.Background(), mkParams(5))
	if err != nil {
		t.Fatalf("Charge #3: %v", err)
	}
	if !res.OverBudget {
		t.Errorf("OverBudget after 115 = false, want true")
	}
	res, err = w.Charge(context.Background(), mkParams(5))
	if err != nil {
		t.Fatalf("Charge #4: %v", err)
	}
	if !res.OverBudget {
		t.Errorf("OverBudget after 120 = false, want true")
	}
	if got := len(emitter.overBudgetSnapshot()); got != 1 {
		t.Errorf("emit count after 4 charges = %d, want 1 (only first crossing)", got)
	}
	if got := len(trigger.snapshot()); got != 1 {
		t.Errorf("trigger count after 4 charges = %d, want 1 (only first crossing)", got)
	}
}

func TestCharge_ZeroBudgetDisablesEnforcement(t *testing.T) {
	t.Parallel()
	repo := &recordingIncTokensRepo{startVal: 1_000_000} // huge tokens used
	emitter := &recordingEmitter{}
	trigger := &recordingTrigger{}
	w := budget.NewWriter(repo, emitter, budget.WithEscalationTrigger(trigger))

	res, err := w.Charge(context.Background(), budget.ChargeParams{
		ConversationID:      uuid.New(),
		OrganizationID:      uuid.New(),
		ActingWatchkeeperID: "wk",
		TokenBudget:         0, // disabled
		Delta:               500_000,
	})
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if res.OverBudget {
		t.Errorf("OverBudget = true with TokenBudget=0, want false")
	}
	if len(emitter.overBudgetSnapshot()) != 0 || len(trigger.snapshot()) != 0 {
		t.Errorf("zero budget triggered emit/trigger; want neither (over=%d trig=%d)",
			len(emitter.overBudgetSnapshot()), len(trigger.snapshot()))
	}
}

func TestCharge_IncTokensError_Propagates(t *testing.T) {
	t.Parallel()
	repo := &recordingIncTokensRepo{err: k2k.ErrAlreadyArchived}
	emitter := &recordingEmitter{}
	w := budget.NewWriter(repo, emitter)
	_, err := w.Charge(context.Background(), sampleParams(t))
	if !errors.Is(err, k2k.ErrAlreadyArchived) {
		t.Errorf("Charge err = %v, want errors.Is k2k.ErrAlreadyArchived", err)
	}
}

func TestCharge_EmitFailure_DoesNotPropagate(t *testing.T) {
	t.Parallel()
	repo := &recordingIncTokensRepo{startVal: 99}
	emitter := &recordingEmitter{err: errors.New("emit boom")}
	trigger := &recordingTrigger{}
	logger := &recordingLogger{}
	w := budget.NewWriter(
		repo, emitter,
		budget.WithEscalationTrigger(trigger),
		budget.WithLogger(logger),
	)
	res, err := w.Charge(context.Background(), budget.ChargeParams{
		ConversationID:      uuid.New(),
		OrganizationID:      uuid.New(),
		ActingWatchkeeperID: "wk",
		TokenBudget:         100,
		Delta:               5, // 104 > 100
	})
	if err != nil {
		t.Fatalf("Charge err = %v, want nil (emit failure non-propagating)", err)
	}
	if !res.OverBudget {
		t.Errorf("OverBudget = false, want true")
	}
	if len(trigger.snapshot()) != 1 {
		t.Errorf("trigger called %d times, want 1 (emit failure must NOT short-circuit trigger)", len(trigger.snapshot()))
	}
	lines := logger.snapshot()
	if len(lines) != 1 {
		t.Fatalf("logger lines = %d, want 1", len(lines))
	}
	if !regexp.MustCompile(`emit over-budget.+emit boom`).MatchString(lines[0]) {
		t.Errorf("log line = %q, want match emit-failure shape", lines[0])
	}
}

func TestCharge_TriggerFailure_DoesNotPropagate(t *testing.T) {
	t.Parallel()
	repo := &recordingIncTokensRepo{startVal: 99}
	emitter := &recordingEmitter{}
	trigger := &recordingTrigger{err: errors.New("trigger boom")}
	logger := &recordingLogger{}
	w := budget.NewWriter(
		repo, emitter,
		budget.WithEscalationTrigger(trigger),
		budget.WithLogger(logger),
	)
	res, err := w.Charge(context.Background(), budget.ChargeParams{
		ConversationID:      uuid.New(),
		OrganizationID:      uuid.New(),
		ActingWatchkeeperID: "wk",
		TokenBudget:         100,
		Delta:               5,
	})
	if err != nil {
		t.Fatalf("Charge err = %v, want nil (trigger failure non-propagating)", err)
	}
	if !res.OverBudget {
		t.Errorf("OverBudget = false, want true")
	}
	if len(emitter.overBudgetSnapshot()) != 1 {
		t.Errorf("emit called %d times, want 1 (trigger failure must NOT short-circuit emit)", len(emitter.overBudgetSnapshot()))
	}
	lines := logger.snapshot()
	if len(lines) != 1 {
		t.Fatalf("logger lines = %d, want 1", len(lines))
	}
	if !regexp.MustCompile(`trigger over-budget.+trigger boom`).MatchString(lines[0]) {
		t.Errorf("log line = %q, want match trigger-failure shape", lines[0])
	}
}

// TestCharge_DetachedCtx_OverBudgetEmitDoesNotDropOnCancel pins the
// "detached ctx" invariant: a caller-side cancellation arriving AFTER
// IncTokens succeeded must NOT systematically drop the over-budget emit
// or the trigger. The harness cancels the caller's ctx the moment
// IncTokens returns; the emit + trigger still fire because they run
// under `context.WithoutCancel`.
func TestCharge_DetachedCtx_OverBudgetEmitDoesNotDropOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())

	// repo wrapper that cancels the caller's ctx the moment IncTokens
	// returns its successful int64.
	repoInner := &recordingIncTokensRepo{startVal: 99}
	repo := &cancellingRepo{inner: repoInner, cancel: cancel}
	emitter := &recordingEmitter{}
	trigger := &recordingTrigger{}
	w := budget.NewWriter(repo, emitter, budget.WithEscalationTrigger(trigger))

	res, err := w.Charge(ctx, budget.ChargeParams{
		ConversationID:      uuid.New(),
		OrganizationID:      uuid.New(),
		ActingWatchkeeperID: "wk",
		TokenBudget:         100,
		Delta:               5,
	})
	if err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if !res.OverBudget {
		t.Errorf("OverBudget = false, want true")
	}
	if len(emitter.overBudgetSnapshot()) != 1 {
		t.Errorf("emit called %d times under detached ctx, want 1", len(emitter.overBudgetSnapshot()))
	}
	if len(trigger.snapshot()) != 1 {
		t.Errorf("trigger called %d times under detached ctx, want 1", len(trigger.snapshot()))
	}
}

type cancellingRepo struct {
	inner  *recordingIncTokensRepo
	cancel context.CancelFunc
}

func (c *cancellingRepo) IncTokens(ctx context.Context, id uuid.UUID, delta int64) (int64, error) {
	v, err := c.inner.IncTokens(ctx, id, delta)
	c.cancel()
	return v, err
}

// TestCharge_ConcurrentChargesComposeCorrectly runs 16 goroutines each
// performing 50 charges against the same conversation row backed by
// the in-memory k2k repository. Asserts (a) the final tokens_used
// equals the sum of all deltas (atomic increment composes), (b) the
// over-budget emit count == over-budget trigger count >= 1.
func TestCharge_ConcurrentChargesComposeCorrectly(t *testing.T) {
	t.Parallel()
	const (
		goroutines = 16
		perG       = 50
		delta      = int64(10)
		tokenBudg  = int64(1000)
	)
	convID := uuid.New()
	orgID := uuid.New()

	repo := k2k.NewMemoryRepository(time.Now, nil)
	// Seed the conversation so IncTokens has a row to find.
	conv, err := repo.Open(context.Background(), k2k.OpenParams{
		OrganizationID: orgID,
		Participants:   []string{"wk-a", "wk-b"},
		Subject:        "race",
		TokenBudget:    tokenBudg,
	})
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}
	convID = conv.ID

	emitter := &recordingEmitter{}
	trigger := &recordingTrigger{}
	w := budget.NewWriter(repo, emitter, budget.WithEscalationTrigger(trigger))

	var (
		wg   sync.WaitGroup
		fail atomic.Int32
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				_, err := w.Charge(context.Background(), budget.ChargeParams{
					ConversationID:      convID,
					OrganizationID:      orgID,
					ActingWatchkeeperID: "wk-a",
					TokenBudget:         tokenBudg,
					Delta:               delta,
				})
				if err != nil {
					fail.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	if fail.Load() != 0 {
		t.Fatalf("got %d failed charges, want 0", fail.Load())
	}

	final, err := repo.Get(context.Background(), convID)
	if err != nil {
		t.Fatalf("Get final: %v", err)
	}
	wantTotal := int64(goroutines) * int64(perG) * delta
	if final.TokensUsed != wantTotal {
		t.Errorf("final TokensUsed = %d, want %d (atomic increment broke)", final.TokensUsed, wantTotal)
	}
	// Exactly one charge crossed the 1000 budget (iter-1 codex P1 fix:
	// only the first crossing fires the side effects, never any
	// subsequent post-budget charges). emit_count == trigger_count
	// == 1 regardless of how many goroutines stayed over budget.
	emits := len(emitter.overBudgetSnapshot())
	trigs := len(trigger.snapshot())
	if emits != trigs {
		t.Errorf("emit count = %d, trigger count = %d (want equal)", emits, trigs)
	}
	if emits != 1 {
		t.Errorf("emit count = %d, want exactly 1 (only the first crossing fires side effects)", emits)
	}
}

// TestSourceFile_NoKeeperslogReferences is the source-grep AC mirroring
// the M1.3.\* / M1.4 discipline: this package's call sites do not
// import `keeperslog` or call `.Append(` directly — they route through
// the M1.4 `audit.Emitter` typed seam. Mirrors
// `TestEmitsViaAuditEmitter_NoKeeperslogImport` discipline.
func TestSourceFile_NoKeeperslogReferences(t *testing.T) {
	t.Parallel()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	pkgDir := filepath.Dir(thisFile)
	srcFiles := []string{"doc.go", "errors.go", "budget.go"}
	for _, name := range srcFiles {
		data, err := os.ReadFile(filepath.Join(pkgDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		// The package's audit imports route through
		// `github.com/.../k2k/audit`; the source-grep ban only fires
		// on the direct `keeperslog.` package-prefix.
		if regexp.MustCompile(`(?m)^[^/]*\bkeeperslog\.`).Match(data) {
			t.Errorf("%s contains `keeperslog.` reference; audit must route through audit.Emitter", name)
		}
		if regexp.MustCompile(`(?m)^[^/]*\.Append\(`).Match(data) {
			t.Errorf("%s contains `.Append(` reference; audit emission must use audit.Emitter typed methods", name)
		}
	}
}
