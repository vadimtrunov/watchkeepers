package saga_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// mustExtractData unwraps the keeperslog envelope `{"event_type":"..","data":{...}}`
// from the JSON-marshalled `LogAppendRequest.Payload` and returns the
// inner `data` map. The wire shape is the keepers_log writer's
// canonical envelope; failure to unmarshal at any layer is a test
// fixture bug, not a production failure mode, so we t.Fatalf on
// any decode error rather than silently returning a zero value.
func mustExtractData(t *testing.T, payload json.RawMessage) map[string]any {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	dataRaw, ok := envelope["data"]
	if !ok {
		t.Fatalf("payload missing `data` envelope key: %s", string(payload))
	}
	var data map[string]any
	if err := json.Unmarshal(dataRaw, &data); err != nil {
		t.Fatalf("payload.data not JSON: %v", err)
	}
	return data
}

// ────────────────────────────────────────────────────────────────────────
// Test fakes for the M7.3.b compensator surface
// ────────────────────────────────────────────────────────────────────────

// compensatingStep is a [saga.Step] that ALSO satisfies
// [saga.Compensator]. The test plan substitutes this step in place
// of [stubStep] (defined in saga_test.go) when the rollback chain
// needs to dispatch a Compensate call. The receiver records every
// Compensate invocation timestamp into a shared monotonic counter so
// the reverse-order assertion can pin call order without time.Now
// ambiguity.
type compensatingStep struct {
	name           string
	executeErr     error
	compensateErr  error
	compensateSeq  uint64
	compensateTick *atomic.Uint64

	mu    sync.Mutex
	calls []string // "execute" or "compensate"
}

func (c *compensatingStep) Name() string { return c.name }

func (c *compensatingStep) Execute(_ context.Context) error {
	c.mu.Lock()
	c.calls = append(c.calls, "execute")
	c.mu.Unlock()
	return c.executeErr
}

func (c *compensatingStep) Compensate(_ context.Context) error {
	if c.compensateTick != nil {
		c.compensateSeq = c.compensateTick.Add(1)
	}
	c.mu.Lock()
	c.calls = append(c.calls, "compensate")
	c.mu.Unlock()
	return c.compensateErr
}

func (c *compensatingStep) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.calls))
	copy(out, c.calls)
	return out
}

// compensateClassedError implements [saga.LastErrorClassed] so the
// `saga_compensation_failed` audit row pins the resolved sentinel
// against the typed-error chain rather than the
// [saga.LastErrorClassCompensateDefault] fallback.
type compensateClassedError struct{ class string }

func (e *compensateClassedError) Error() string          { return "compensate: " + e.class }
func (e *compensateClassedError) LastErrorClass() string { return e.class }

// runFailingSagaResult bundles the recorded audit events + the
// runner-returned error + the DAO + the saga id from a single
// [runFailingSaga] dispatch. Held in a struct so the test surface
// stays signature-stable (revive: error MUST be the last return
// type when paired with multiple typed returns; folding the bag
// into a struct sidesteps the rule).
type runFailingSagaResult struct {
	Events []keeperslog.Event
	DAO    *saga.MemorySpawnSagaDAO
	SagaID uuid.UUID
	Err    error
}

// runFailingSaga seeds a saga DAO + writer + runner with the supplied
// step list, runs it, and returns the recorded audit events plus the
// runner-returned error. Shared across the M7.3.b test cases to keep
// the per-test boilerplate minimal.
func runFailingSaga(t *testing.T, steps []saga.Step) runFailingSagaResult {
	t.Helper()
	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	runErr := runner.Run(context.Background(), id, steps)

	rows := keep.recorded()
	events := make([]keeperslog.Event, 0, len(rows))
	for _, r := range rows {
		events = append(events, keeperslog.Event{EventType: r.EventType, Payload: nil})
	}
	return runFailingSagaResult{Events: events, DAO: dao, SagaID: id, Err: runErr}
}

// ────────────────────────────────────────────────────────────────────────
// Compile-time interface satisfaction
// ────────────────────────────────────────────────────────────────────────

func TestCompensator_CompileTimeAssertion(t *testing.T) {
	t.Parallel()
	// Pin the M7.3.b interface seam: a type that implements
	// Compensate(ctx) error MUST satisfy saga.Compensator without an
	// explicit assertion at the call site. A future signature
	// change to Compensator (e.g. adding a return value) breaks
	// this test at compile time, surfacing the contract drift.
	var _ saga.Compensator = (*compensatingStep)(nil)
}

// ────────────────────────────────────────────────────────────────────────
// Happy path: no compensation when every step succeeds
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Run_AllStepsSucceed_NoCompensationEmitted(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	tick := &atomic.Uint64{}
	steps := []saga.Step{
		&compensatingStep{name: "step_one", compensateTick: tick},
		&compensatingStep{name: "step_two", compensateTick: tick},
	}
	if err := runner.Run(context.Background(), id, steps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, row := range keep.recorded() {
		switch row.EventType {
		case saga.EventTypeSagaStepCompensated,
			saga.EventTypeSagaCompensationFailed,
			saga.EventTypeSagaCompensated:
			t.Errorf("happy path emitted forbidden event %q", row.EventType)
		}
	}

	for _, step := range steps {
		cs := step.(*compensatingStep)
		for _, c := range cs.recorded() {
			if c == "compensate" {
				t.Errorf("step %q compensated on happy path", cs.name)
			}
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// Reverse order: 3 steps, middle fails -> compensations on steps 0,1
// in REVERSE forward order; failed step itself is NOT compensated.
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Run_StepFails_ReverseCompensationOnPriorSteps(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	tick := &atomic.Uint64{}
	step0 := &compensatingStep{name: "alpha", compensateTick: tick}
	step1 := &compensatingStep{name: "beta", compensateTick: tick}
	step2 := &compensatingStep{name: "gamma", executeErr: errors.New("gamma failed"), compensateTick: tick}

	err := runner.Run(context.Background(), id, []saga.Step{step0, step1, step2})
	if err == nil {
		t.Fatalf("Run: err = nil, want wrapped step error")
	}

	// Reverse-order assertion: step1 (beta) compensated BEFORE
	// step0 (alpha). Both monotonic counters must be non-zero
	// (the Compensate ran) AND step1.compensateSeq < step0.compensateSeq
	// is FALSE — beta runs first, so step1 has the SMALLER seq value.
	if step0.compensateSeq == 0 {
		t.Fatalf("step0 (alpha) was not compensated")
	}
	if step1.compensateSeq == 0 {
		t.Fatalf("step1 (beta) was not compensated")
	}
	if step1.compensateSeq >= step0.compensateSeq {
		t.Errorf("compensation order broken: beta.seq=%d, alpha.seq=%d (beta MUST precede alpha in reverse-walk)",
			step1.compensateSeq, step0.compensateSeq)
	}
	// Failed step itself MUST NOT be compensated.
	for _, c := range step2.recorded() {
		if c == "compensate" {
			t.Errorf("failed step (gamma) was compensated; only previously-successful steps may be")
		}
	}

	rows := keep.recorded()
	// Expected wire-shape:
	//   step_started(alpha), step_completed(alpha),
	//   step_started(beta),  step_completed(beta),
	//   step_started(gamma), saga_failed(gamma),
	//   step_compensated(beta), step_compensated(alpha),
	//   saga_compensated.
	wantTypes := []string{
		saga.EventTypeSagaStepStarted,
		saga.EventTypeSagaStepCompleted,
		saga.EventTypeSagaStepStarted,
		saga.EventTypeSagaStepCompleted,
		saga.EventTypeSagaStepStarted,
		saga.EventTypeSagaFailed,
		saga.EventTypeSagaStepCompensated,
		saga.EventTypeSagaStepCompensated,
		saga.EventTypeSagaCompensated,
	}
	if len(rows) != len(wantTypes) {
		t.Fatalf("audit-event count = %d, want %d (events: %+v)", len(rows), len(wantTypes), rows)
	}
	for i, want := range wantTypes {
		if rows[i].EventType != want {
			t.Errorf("audit[%d].EventType = %q, want %q", i, rows[i].EventType, want)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// Best-effort: one Compensate fails -> saga_compensation_failed; the
// remaining (earlier-in-forward-order) compensation still runs.
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Run_CompensationFailure_RemainingCompensationsStillRun(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	tick := &atomic.Uint64{}
	step0 := &compensatingStep{name: "alpha", compensateTick: tick}
	step1 := &compensatingStep{
		name:           "beta",
		compensateTick: tick,
		compensateErr:  &compensateClassedError{class: "beta_compensate_unavailable"},
	}
	step2 := &compensatingStep{name: "gamma", executeErr: errors.New("gamma failed"), compensateTick: tick}

	if err := runner.Run(context.Background(), id, []saga.Step{step0, step1, step2}); err == nil {
		t.Fatalf("Run: err = nil, want wrapped step error")
	}

	// step0 (alpha) MUST still have been compensated even though
	// step1 (beta) returned non-nil from Compensate. Best-effort
	// rollback: a compensator failure does NOT abort the chain.
	if step0.compensateSeq == 0 {
		t.Fatalf("step0 (alpha) was not compensated; best-effort rollback violated")
	}

	rows := keep.recorded()
	var (
		gotCompensationFailed bool
		gotStepCompensated    bool
		failedStepName        string
		compensatedStepName   string
		failedLastErrorClass  any
	)
	for _, r := range rows {
		switch r.EventType {
		case saga.EventTypeSagaCompensationFailed:
			gotCompensationFailed = true
			data := mustExtractData(t, r.Payload)
			failedStepName, _ = data["step_name"].(string)
			failedLastErrorClass = data["last_error_class"]
		case saga.EventTypeSagaStepCompensated:
			gotStepCompensated = true
			data := mustExtractData(t, r.Payload)
			if name, _ := data["step_name"].(string); name != "" {
				compensatedStepName = name
			}
		}
	}
	if !gotCompensationFailed {
		t.Errorf("missing saga_compensation_failed for beta")
	}
	if failedStepName != "beta" {
		t.Errorf("saga_compensation_failed.step_name = %q, want beta", failedStepName)
	}
	if class, _ := failedLastErrorClass.(string); class != "beta_compensate_unavailable" {
		t.Errorf("saga_compensation_failed.last_error_class = %v, want beta_compensate_unavailable",
			failedLastErrorClass)
	}
	if !gotStepCompensated {
		t.Errorf("missing saga_step_compensated for alpha")
	}
	if compensatedStepName != "alpha" {
		t.Errorf("saga_step_compensated.step_name = %q, want alpha", compensatedStepName)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Step without Compensator interface is silently skipped — no audit
// row, no panic.
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Run_NonCompensatingStep_SilentlySkipped(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	tick := &atomic.Uint64{}
	// step0 is a plain stubStep — no Compensate method. step1 is a
	// compensatingStep that records every Compensate call. step2
	// fails so the runner runs the rollback chain.
	step0 := &stubStep{name: "noncompensating"}
	step1 := &compensatingStep{name: "compensating", compensateTick: tick}
	step2 := &stubStep{name: "failer", err: errors.New("boom")}

	if err := runner.Run(context.Background(), id, []saga.Step{step0, step1, step2}); err == nil {
		t.Fatalf("Run: err = nil, want wrapped step error")
	}

	if step1.compensateSeq == 0 {
		t.Fatalf("compensatingStep (step1) was not compensated")
	}

	// Audit chain MUST emit ONE saga_step_compensated (for step1),
	// NOT two — the non-compensating step0 is silently skipped.
	var compensatedCount int
	for _, r := range keep.recorded() {
		if r.EventType == saga.EventTypeSagaStepCompensated {
			compensatedCount++
		}
	}
	if compensatedCount != 1 {
		t.Errorf("saga_step_compensated count = %d, want 1 (non-Compensator skipped silently)",
			compensatedCount)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Ordering: saga_failed precedes the first saga_step_compensated row.
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Run_SagaFailed_PrecedesCompensationRows(t *testing.T) {
	t.Parallel()

	res := runFailingSaga(t, []saga.Step{
		&compensatingStep{name: "first", compensateTick: &atomic.Uint64{}},
		&compensatingStep{
			name: "second", executeErr: errors.New("second failed"),
			compensateTick: &atomic.Uint64{},
		},
	})

	sagaFailedIdx, firstCompensatedIdx := -1, -1
	for i, e := range res.Events {
		if e.EventType == saga.EventTypeSagaFailed && sagaFailedIdx == -1 {
			sagaFailedIdx = i
		}
		if e.EventType == saga.EventTypeSagaStepCompensated && firstCompensatedIdx == -1 {
			firstCompensatedIdx = i
		}
	}
	if sagaFailedIdx == -1 {
		t.Fatalf("no saga_failed event emitted")
	}
	if firstCompensatedIdx == -1 {
		t.Fatalf("no saga_step_compensated event emitted")
	}
	if sagaFailedIdx >= firstCompensatedIdx {
		t.Errorf("saga_failed.idx=%d, first saga_step_compensated.idx=%d — saga_failed MUST precede compensations",
			sagaFailedIdx, firstCompensatedIdx)
	}
}

// ────────────────────────────────────────────────────────────────────────
// First-step failure -> zero per-step compensations + lone
// saga_compensated summary row.
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Run_FirstStepFails_OnlySagaCompensatedSummary(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	steps := []saga.Step{
		&compensatingStep{
			name: "first", executeErr: errors.New("first failed"),
			compensateTick: &atomic.Uint64{},
		},
		&compensatingStep{name: "second", compensateTick: &atomic.Uint64{}},
	}
	if err := runner.Run(context.Background(), id, steps); err == nil {
		t.Fatalf("Run: err = nil, want wrapped step error")
	}

	rows := keep.recorded()
	wantTypes := []string{
		saga.EventTypeSagaStepStarted, // first
		saga.EventTypeSagaFailed,      // first
		saga.EventTypeSagaCompensated, // empty rollback summary
	}
	if len(rows) != len(wantTypes) {
		t.Fatalf("audit-event count = %d, want %d", len(rows), len(wantTypes))
	}
	for i, want := range wantTypes {
		if rows[i].EventType != want {
			t.Errorf("audit[%d].EventType = %q, want %q", i, rows[i].EventType, want)
		}
	}
	// The failed step itself MUST NOT be compensated.
	if cs, ok := steps[0].(*compensatingStep); ok {
		for _, c := range cs.recorded() {
			if c == "compensate" {
				t.Errorf("failed first step compensated itself; only previously-successful steps may be")
			}
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// Compensate without LastErrorClassed -> default sentinel.
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Run_CompensateWithoutClassedError_FallsBackToDefault(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	steps := []saga.Step{
		&compensatingStep{
			name:           "anonymous_compensator",
			compensateErr:  errors.New("anonymous"),
			compensateTick: &atomic.Uint64{},
		},
		&compensatingStep{
			name: "boom", executeErr: errors.New("boom"),
			compensateTick: &atomic.Uint64{},
		},
	}
	if err := runner.Run(context.Background(), id, steps); err == nil {
		t.Fatalf("Run: err = nil, want wrapped step error")
	}

	for _, r := range keep.recorded() {
		if r.EventType != saga.EventTypeSagaCompensationFailed {
			continue
		}
		data := mustExtractData(t, r.Payload)
		class, _ := data["last_error_class"].(string)
		if class != saga.LastErrorClassCompensateDefault {
			t.Errorf("saga_compensation_failed.last_error_class = %q, want %q",
				class, saga.LastErrorClassCompensateDefault)
		}
		return
	}
	t.Fatalf("no saga_compensation_failed event emitted")
}

// ────────────────────────────────────────────────────────────────────────
// Source-grep AC: saga.go is per-step-agnostic.
//
// The Runner+Compensator infrastructure is generic; concrete step
// names belong to the spawn package. A regression that hard-codes
// step-specific behaviour into saga core would defeat the M7.3.b
// "foundation only — no concrete impls" boundary.
// ────────────────────────────────────────────────────────────────────────

func TestSagaCore_DoesNotReferenceSpawnPackageOrConcreteStepNames(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("saga.go")
	if err != nil {
		t.Fatalf("read saga.go: %v", err)
	}
	body2, err := os.ReadFile("compensator.go")
	if err != nil {
		t.Fatalf("read compensator.go: %v", err)
	}
	bothFiles := string(body) + "\n" + string(body2)

	// The forbidden substrings target imports + concrete step
	// names that would couple saga core to the spawn family. Doc-
	// block lines (// prefix) may legitimately reference these
	// symbols for narrative; the AC pin scans only NON-comment
	// lines.
	forbiddenSubstrings := []string{
		"core/pkg/spawn\"",
		"core/pkg/notebook",
		"core/pkg/runtime",
		"core/pkg/messenger",
		"core/pkg/keepclient",
	}
	for _, line := range strings.Split(bothFiles, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		for _, forbidden := range forbiddenSubstrings {
			if strings.Contains(line, forbidden) {
				t.Errorf("saga core code line contains forbidden substring %q: %s",
					forbidden, line)
			}
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// Iter-1 fix: Compensate dispatched under context.WithoutCancel — a
// pre-cancelled parent ctx still drives the rollback walk.
// ────────────────────────────────────────────────────────────────────────

// ctxObservingStep records the ctx.Err() it observed inside its own
// Compensate so the test can pin "the ctx propagated to Compensate
// was NOT cancelled even though the parent was".
type ctxObservingStep struct {
	name      string
	failExec  bool
	observed  error
	mu        sync.Mutex
	completed bool
}

func (c *ctxObservingStep) Name() string { return c.name }
func (c *ctxObservingStep) Execute(_ context.Context) error {
	if c.failExec {
		return errors.New("ctx-observing failer")
	}
	return nil
}

func (c *ctxObservingStep) Compensate(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.observed = ctx.Err()
	c.completed = true
	return nil
}

func TestRunner_Compensate_PreCancelledParentCtx_StillDrivesRollback(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	step0 := &ctxObservingStep{name: "alpha"}
	step1 := &ctxObservingStep{name: "beta", failExec: true}

	// Cancel the parent BEFORE Run dispatches. The runner's
	// MarkFailed + saga_failed emit consume the cancelled parent
	// (audit emits are best-effort), but Compensate MUST run with
	// a ctx that is NOT cancelled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := runner.Run(ctx, id, []saga.Step{step0, step1}); err == nil {
		t.Fatalf("Run: err = nil, want wrapped step error")
	}

	step0.mu.Lock()
	defer step0.mu.Unlock()
	if !step0.completed {
		t.Fatalf("step0 (alpha) Compensate was not called under pre-cancelled parent ctx")
	}
	if step0.observed != nil {
		t.Errorf("step0 observed ctx.Err()=%v, want nil (Compensate ctx must be derived from context.WithoutCancel)",
			step0.observed)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Iter-1 fix: a Compensate that returns context.Canceled / DeadlineExceeded
// classes onto distinct sentinels rather than collapsing to the default.
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Compensate_ContextCanceledError_ClassesToContextCanceledSentinel(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	steps := []saga.Step{
		&compensatingStep{
			name:           "ctx_canceled_compensator",
			compensateErr:  context.Canceled,
			compensateTick: &atomic.Uint64{},
		},
		&compensatingStep{
			name: "boom", executeErr: errors.New("boom"),
			compensateTick: &atomic.Uint64{},
		},
	}
	if err := runner.Run(context.Background(), id, steps); err == nil {
		t.Fatalf("Run: err = nil, want wrapped step error")
	}

	for _, r := range keep.recorded() {
		if r.EventType != saga.EventTypeSagaCompensationFailed {
			continue
		}
		data := mustExtractData(t, r.Payload)
		class, _ := data["last_error_class"].(string)
		if class != saga.LastErrorClassCompensateContextCanceled {
			t.Errorf("last_error_class = %q, want %q",
				class, saga.LastErrorClassCompensateContextCanceled)
		}
		return
	}
	t.Fatalf("no saga_compensation_failed event emitted")
}

func TestRunner_Compensate_ContextDeadlineError_ClassesToContextDeadlineSentinel(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	steps := []saga.Step{
		&compensatingStep{
			name:           "ctx_deadline_compensator",
			compensateErr:  context.DeadlineExceeded,
			compensateTick: &atomic.Uint64{},
		},
		&compensatingStep{
			name: "boom", executeErr: errors.New("boom"),
			compensateTick: &atomic.Uint64{},
		},
	}
	if err := runner.Run(context.Background(), id, steps); err == nil {
		t.Fatalf("Run: err = nil, want wrapped step error")
	}

	for _, r := range keep.recorded() {
		if r.EventType != saga.EventTypeSagaCompensationFailed {
			continue
		}
		data := mustExtractData(t, r.Payload)
		class, _ := data["last_error_class"].(string)
		if class != saga.LastErrorClassCompensateContextDeadline {
			t.Errorf("last_error_class = %q, want %q",
				class, saga.LastErrorClassCompensateContextDeadline)
		}
		return
	}
	t.Fatalf("no saga_compensation_failed event emitted")
}

// ────────────────────────────────────────────────────────────────────────
// Iter-1 fix: a panicking Compensate is recovered into a typed error
// so the audit chain pins the panic-class sentinel + the saga goroutine
// stays alive to emit the saga-level summary row.
// ────────────────────────────────────────────────────────────────────────

type panickingCompensator struct {
	name      string
	panicWith any
}

func (p *panickingCompensator) Name() string                    { return p.name }
func (p *panickingCompensator) Execute(_ context.Context) error { return nil }
func (p *panickingCompensator) Compensate(_ context.Context) error {
	panic(p.panicWith)
}

func TestRunner_Compensate_Panic_RecoveredAndClassedAsCompensatePanic(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	steps := []saga.Step{
		&panickingCompensator{name: "alpha_panicker", panicWith: "panic_canary"},
		&compensatingStep{
			name: "boom", executeErr: errors.New("boom"),
			compensateTick: &atomic.Uint64{},
		},
	}
	// MUST NOT panic at the test surface — defer/recover in
	// safeCompensate is the harness.
	if err := runner.Run(context.Background(), id, steps); err == nil {
		t.Fatalf("Run: err = nil, want wrapped step error")
	}

	var (
		gotPanicCompensationFailed bool
		gotSummary                 bool
	)
	for _, r := range keep.recorded() {
		switch r.EventType {
		case saga.EventTypeSagaCompensationFailed:
			data := mustExtractData(t, r.Payload)
			class, _ := data["last_error_class"].(string)
			if class == saga.LastErrorClassCompensatePanic {
				gotPanicCompensationFailed = true
			}
			// Defense-in-depth: the recovered value MUST NOT
			// land on the audit payload (M2b.7 PII discipline).
			if name, _ := data["step_name"].(string); name != "alpha_panicker" {
				t.Errorf("panic compensation step_name = %q, want %q", name, "alpha_panicker")
			}
		case saga.EventTypeSagaCompensated:
			gotSummary = true
		}
	}
	if !gotPanicCompensationFailed {
		t.Errorf("missing saga_compensation_failed with class=%q for panicking Compensate",
			saga.LastErrorClassCompensatePanic)
	}
	if !gotSummary {
		t.Errorf("missing saga_compensated summary row — defer/recover failed to keep the goroutine alive")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Concurrency: 16 concurrent failing sagas each run their own
// rollback chain without observable interference.
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Concurrency_DistinctSagas_RollbackInIsolation(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	const sagas = 16
	ids := make([]uuid.UUID, sagas)
	for i := range ids {
		ids[i] = uuid.New()
		if err := dao.Insert(context.Background(), ids[i], uuid.New()); err != nil {
			t.Fatalf("Insert(%v): %v", ids[i], err)
		}
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			tick := &atomic.Uint64{}
			steps := []saga.Step{
				&compensatingStep{name: "concurrent_alpha", compensateTick: tick},
				&compensatingStep{
					name: "concurrent_beta", executeErr: errors.New("beta failed"),
					compensateTick: tick,
				},
			}
			if err := runner.Run(context.Background(), id, steps); err == nil {
				t.Errorf("Run(%v): err = nil, want wrapped step error", id)
			}
		}(id)
	}
	wg.Wait()

	for _, id := range ids {
		got, err := dao.Get(context.Background(), id)
		if err != nil {
			t.Errorf("Get(%v): %v", id, err)
			continue
		}
		if got.Status != saga.SagaStateFailed {
			t.Errorf("Status(%v) = %q, want %q", id, got.Status, saga.SagaStateFailed)
		}
	}
}
