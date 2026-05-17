package spawn_test

// notebookinherit_fault_injection_test.go pins the Phase 2 §M7.1.c
// compensator-chain invariant for [NotebookInheritStep]: a successful
// inherit followed by a downstream step failure MUST leave the
// compensator chain unchanged — specifically:
//
//   - The downstream step that failed gets NO compensator dispatch
//     (its Execute returned an error → no Execute success → no
//     Compensate per the M7.3.b "skip non-implementers AND skip
//     un-executed steps" contract).
//   - The new [NotebookInheritStep] does NOT implement
//     [saga.Compensator]; the chain silently skips it. The audit
//     row contract emits `saga_step_compensated` ONLY for steps that
//     implement Compensator (i.e. NOT for `notebook_inherit`).
//
// This is the M7.1.c acceptance criterion "fault-injection: compensator
// chain unchanged when this step succeeds then a downstream step
// fails".

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// stubFailingStep returns the supplied error on Execute. Implements
// [saga.Compensator] with a counter so the test can confirm the
// FAILED step's Compensate is NOT dispatched (per the M7.3.b
// "the failed step's forward dispatch errored → no Execute success
// → no Compensate" invariant).
type stubFailingStep struct {
	name      string
	failErr   error
	compCalls int
}

func (s *stubFailingStep) Name() string                    { return s.name }
func (s *stubFailingStep) Execute(_ context.Context) error { return s.failErr }
func (s *stubFailingStep) Compensate(_ context.Context) error {
	s.compCalls++
	return nil
}

// TestNotebookInheritStep_CompensatorChainUnchanged_OnDownstreamFailure
// wires:
//
//  1. NotebookInheritStep (Execute succeeds — predecessor exists,
//     inherit dispatched, audit row emitted).
//  2. stubFailingStep (Execute returns a sentinel — drives the
//     saga's reverse-rollback walk).
//
// Asserts:
//   - The saga.Runner.Run returns the wrapped step error.
//   - The audit chain emits exactly ONE `notebook_inherited` row
//     (from the inherit step's forward dispatch).
//   - The audit chain emits NO `saga_step_compensated` row whose
//     step_name is `notebook_inherit` (the step does NOT implement
//     Compensator; the runner silently skips it).
//   - The stubFailingStep's Compensate counter is 0 (the failed
//     step's Compensate is NEVER dispatched — its Execute did NOT
//     succeed, so per the M7.3.b "skip un-executed steps" contract
//     it is not in the rollback set).
func TestNotebookInheritStep_CompensatorChainUnchanged_OnDownstreamFailure(t *testing.T) {
	t.Parallel()

	wkID := uuid.New()
	// Inherit step seams. The AuditAppender is wired to the SAME
	// keeperslog.Writer the saga runner uses so the `notebook_inherited`
	// row lands on the same `keep` recorder as the saga's
	// `saga_step_*` rows — mirrors the production wiring where one
	// writer underlies every audit emit.
	lookup := &fakePredecessorLookup{returnWk: newPredecessorWatchkeeper()}
	inheritor := &fakeNotebookInheritor{returnCount: 5}

	// Saga runner.
	sagaDAO := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: sagaDAO, Logger: writer})

	inheritStep := spawn.NewNotebookInheritStep(spawn.NotebookInheritStepDeps{
		Predecessor:   lookup,
		Inheritor:     inheritor,
		AuditAppender: writer,
	})

	// Downstream stub that fails.
	failureSentinel := errors.New("downstream-boom")
	downstream := &stubFailingStep{name: "downstream_after_inherit", failErr: failureSentinel}

	sagaID := uuid.New()
	manifestVersionID := uuid.New()
	if err := sagaDAO.Insert(context.Background(), sagaID, manifestVersionID); err != nil {
		t.Fatalf("sagaDAO.Insert: %v", err)
	}

	sc := newInheritSpawnContext(t, wkID)
	sc.ManifestVersionID = manifestVersionID
	ctx := saga.WithSpawnContext(context.Background(), sc)

	err := runner.Run(ctx, sagaID, []saga.Step{inheritStep, downstream})
	if err == nil {
		t.Fatalf("Run: nil err, want wrapped downstream failure")
	}
	if !errors.Is(err, failureSentinel) {
		t.Errorf("errors.Is(err, sentinel) = false; err = %v", err)
	}

	// Inherit step's forward dispatch succeeded — its seams were
	// invoked and the audit row landed on `keep` via the shared
	// keeperslog.Writer.
	if got := lookup.callCount.Load(); got != 1 {
		t.Errorf("lookup callCount = %d, want 1", got)
	}
	if got := inheritor.callCount.Load(); got != 1 {
		t.Errorf("inherit callCount = %d, want 1", got)
	}

	// The failed step's Compensate MUST NOT fire (Execute did not
	// succeed → not in the reverse-rollback set).
	if downstream.compCalls != 0 {
		t.Errorf("downstream.Compensate calls = %d, want 0 (failed step is not compensated)", downstream.compCalls)
	}

	// Audit chain must contain NO `saga_step_compensated` row whose
	// step_name is `notebook_inherit` — the new step does not
	// implement Compensator and is silently skipped during the
	// reverse-rollback walk.
	rows := keep.recorded()
	for _, r := range rows {
		if r.EventType != saga.EventTypeSagaStepCompensated {
			continue
		}
		data := mustExtractDataPayload(t, r.Payload)
		if name, _ := data["step_name"].(string); name == spawn.NotebookInheritStepName {
			t.Errorf("found saga_step_compensated row for notebook_inherit; want NONE (step does not implement Compensator)")
		}
	}

	// The `notebook_inherited` audit row IS present (and exactly
	// once — the saga's forward dispatch ran the step exactly once).
	inheritedRowCount := 0
	for _, r := range rows {
		if r.EventType == spawn.EventTypeNotebookInherited {
			inheritedRowCount++
		}
	}
	if inheritedRowCount != 1 {
		t.Errorf("notebook_inherited row count = %d, want 1", inheritedRowCount)
	}
}

// TestNotebookInheritStep_NoPredecessor_NoAuditRow_DownstreamRunsThen
// confirms the no-predecessor branch (`keepclient.ErrNoPredecessor`):
// the step is a no-op + NO audit row; the saga continues to the next
// step normally. This pins the "no audit event" half of the
// acceptance criterion for the no-predecessor path under a real
// saga.Runner dispatch.
func TestNotebookInheritStep_NoPredecessor_NoAuditRow_UnderRunner(t *testing.T) {
	t.Parallel()

	wkID := uuid.New()
	lookup := &fakePredecessorLookup{returnErr: keepclient.ErrNoPredecessor}
	inheritor := &fakeNotebookInheritor{}

	sagaDAO := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: sagaDAO, Logger: writer})

	inheritStep := spawn.NewNotebookInheritStep(spawn.NotebookInheritStepDeps{
		Predecessor:   lookup,
		Inheritor:     inheritor,
		AuditAppender: writer,
	})

	sagaID := uuid.New()
	manifestVersionID := uuid.New()
	if err := sagaDAO.Insert(context.Background(), sagaID, manifestVersionID); err != nil {
		t.Fatalf("sagaDAO.Insert: %v", err)
	}

	sc := newInheritSpawnContext(t, wkID)
	sc.ManifestVersionID = manifestVersionID
	ctx := saga.WithSpawnContext(context.Background(), sc)

	if err := runner.Run(ctx, sagaID, []saga.Step{inheritStep}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := lookup.callCount.Load(); got != 1 {
		t.Errorf("lookup callCount = %d, want 1", got)
	}
	if got := inheritor.callCount.Load(); got != 0 {
		t.Errorf("inheritor callCount = %d, want 0 (no predecessor)", got)
	}

	// Confirm the saga's audit chain has NO `notebook_inherited` row.
	for _, r := range keep.recorded() {
		if r.EventType == spawn.EventTypeNotebookInherited {
			t.Errorf("found notebook_inherited row on no-predecessor path; want NONE")
		}
	}
}

// TestNotebookInheritStep_NoInheritOptOut_NoAuditRow_UnderRunner
// confirms the operator opt-out branch (`sc.NoInherit == true`):
// the step is a no-op + NO audit row + NO seam dispatch. This pins
// the "no audit event" half of the acceptance criterion for the
// --no-inherit path under a real saga.Runner dispatch.
func TestNotebookInheritStep_NoInheritOptOut_NoAuditRow_UnderRunner(t *testing.T) {
	t.Parallel()

	wkID := uuid.New()
	lookup := &fakePredecessorLookup{returnWk: newPredecessorWatchkeeper()}
	inheritor := &fakeNotebookInheritor{returnCount: 100}

	sagaDAO := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: sagaDAO, Logger: writer})

	inheritStep := spawn.NewNotebookInheritStep(spawn.NotebookInheritStepDeps{
		Predecessor:   lookup,
		Inheritor:     inheritor,
		AuditAppender: writer,
	})

	sagaID := uuid.New()
	manifestVersionID := uuid.New()
	if err := sagaDAO.Insert(context.Background(), sagaID, manifestVersionID); err != nil {
		t.Fatalf("sagaDAO.Insert: %v", err)
	}

	sc := newInheritSpawnContext(t, wkID, func(sc *saga.SpawnContext) { sc.NoInherit = true })
	sc.ManifestVersionID = manifestVersionID
	ctx := saga.WithSpawnContext(context.Background(), sc)

	if err := runner.Run(ctx, sagaID, []saga.Step{inheritStep}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := lookup.callCount.Load(); got != 0 {
		t.Errorf("lookup callCount = %d, want 0 (NoInherit must short-circuit BEFORE lookup)", got)
	}

	for _, r := range keep.recorded() {
		if r.EventType == spawn.EventTypeNotebookInherited {
			t.Errorf("found notebook_inherited row on NoInherit path; want NONE")
		}
	}
}
