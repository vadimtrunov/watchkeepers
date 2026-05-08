package saga_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// Compile-time assertion: *keeperslog.Writer satisfies saga.Appender.
// AC6: pins the integration shape so a future change to either side
// fails the build here.
var _ saga.Appender = (*keeperslog.Writer)(nil)

// ────────────────────────────────────────────────────────────────────────
// Hand-rolled fakes (per the M3.6 / M6.3.e pattern — no mocking lib).
// ────────────────────────────────────────────────────────────────────────

// fakeLocalKeepClient is the [keeperslog.LocalKeepClient] stand-in that
// records every LogAppend call so the saga audit chain can be inspected
// end-to-end through a real *keeperslog.Writer.
type fakeLocalKeepClient struct {
	mu    sync.Mutex
	calls []keepclient.LogAppendRequest
}

func (f *fakeLocalKeepClient) LogAppend(_ context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	return &keepclient.LogAppendResponse{ID: "fake-row"}, nil
}

func (f *fakeLocalKeepClient) recorded() []keepclient.LogAppendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keepclient.LogAppendRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// stubStep is a placeholder [saga.Step] used by the run-loop tests. It
// records the call sequence (so the order assertion can pin
// state-write-precedes-execute) and returns the configured error.
type stubStep struct {
	name      string
	err       error
	onExecute func(ctx context.Context) // optional pre-execute hook
}

func (s *stubStep) Name() string { return s.name }

func (s *stubStep) Execute(ctx context.Context) error {
	if s.onExecute != nil {
		s.onExecute(ctx)
	}
	return s.err
}

// classedError implements [saga.LastErrorClassed] so the failure-path
// test can pin the resolved sentinel value.
type classedError struct {
	class string
}

func (e *classedError) Error() string          { return "classed: " + e.class }
func (e *classedError) LastErrorClass() string { return e.class }

// recordingDAO wraps a [saga.MemorySpawnSagaDAO] and timestamps every
// call (UpdateStep / MarkCompleted / MarkFailed / Get) into a shared
// monotonic counter so the test plan's "UpdateStep is invoked BEFORE
// Execute" assertion can pin call order without time.Now timing
// ambiguity. The stub step uses the same counter to record its execute
// invocation.
type recordingDAO struct {
	inner *saga.MemorySpawnSagaDAO

	mu          sync.Mutex
	seq         uint64
	updateSteps []orderedCall
	marksDone   []orderedCall
	marksFail   []orderedCall
}

type orderedCall struct {
	seq  uint64
	args string
}

func (r *recordingDAO) tick() uint64 {
	return atomic.AddUint64(&r.seq, 1)
}

func (r *recordingDAO) Insert(ctx context.Context, id uuid.UUID, mvID uuid.UUID) error {
	return r.inner.Insert(ctx, id, mvID)
}

func (r *recordingDAO) Get(ctx context.Context, id uuid.UUID) (saga.Saga, error) {
	return r.inner.Get(ctx, id)
}

func (r *recordingDAO) UpdateStep(ctx context.Context, id uuid.UUID, step string) error {
	t := r.tick()
	r.mu.Lock()
	r.updateSteps = append(r.updateSteps, orderedCall{seq: t, args: step})
	r.mu.Unlock()
	return r.inner.UpdateStep(ctx, id, step)
}

func (r *recordingDAO) MarkCompleted(ctx context.Context, id uuid.UUID) error {
	t := r.tick()
	r.mu.Lock()
	r.marksDone = append(r.marksDone, orderedCall{seq: t})
	r.mu.Unlock()
	return r.inner.MarkCompleted(ctx, id)
}

func (r *recordingDAO) MarkFailed(ctx context.Context, id uuid.UUID, lastErr string) error {
	t := r.tick()
	r.mu.Lock()
	r.marksFail = append(r.marksFail, orderedCall{seq: t, args: lastErr})
	r.mu.Unlock()
	return r.inner.MarkFailed(ctx, id, lastErr)
}

func newRecordingDAO() *recordingDAO {
	return &recordingDAO{inner: saga.NewMemorySpawnSagaDAO(nil)}
}

// ────────────────────────────────────────────────────────────────────────
// Constructor panics
// ────────────────────────────────────────────────────────────────────────

func TestNewRunner_NilDAO_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewRunner with nil DAO did not panic")
		}
	}()
	keep := &fakeLocalKeepClient{}
	_ = saga.NewRunner(saga.Dependencies{Logger: keeperslog.New(keep)})
}

func TestNewRunner_NilLogger_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewRunner with nil Logger did not panic")
		}
	}()
	_ = saga.NewRunner(saga.Dependencies{DAO: saga.NewMemorySpawnSagaDAO(nil)})
}

// ────────────────────────────────────────────────────────────────────────
// Happy paths
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Run_TwoSuccessfulSteps_EmitsFiveAuditEvents(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})

	id := uuid.New()
	mvID := uuid.New()
	if err := dao.Insert(context.Background(), id, mvID); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	steps := []saga.Step{
		&stubStep{name: "step_one"},
		&stubStep{name: "step_two"},
	}
	if err := runner.Run(context.Background(), id, steps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := keep.recorded()
	wantTypes := []string{
		saga.EventTypeSagaStepStarted,
		saga.EventTypeSagaStepCompleted,
		saga.EventTypeSagaStepStarted,
		saga.EventTypeSagaStepCompleted,
		saga.EventTypeSagaCompleted,
	}
	if len(got) != len(wantTypes) {
		t.Fatalf("audit-event count = %d, want %d (events: %+v)", len(got), len(wantTypes), got)
	}
	for i, want := range wantTypes {
		if got[i].EventType != want {
			t.Errorf("audit[%d].EventType = %q, want %q", i, got[i].EventType, want)
		}
	}

	persisted, err := dao.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.Status != saga.SagaStateCompleted {
		t.Errorf("Status = %q, want %q", persisted.Status, saga.SagaStateCompleted)
	}
	if persisted.CurrentStep != "step_two" {
		t.Errorf("CurrentStep = %q, want %q", persisted.CurrentStep, "step_two")
	}
}

func TestRunner_Run_ZeroSteps_EmitsOnlySagaCompleted(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	if err := runner.Run(context.Background(), id, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := keep.recorded()
	if len(got) != 1 {
		t.Fatalf("audit-event count = %d, want 1", len(got))
	}
	if got[0].EventType != saga.EventTypeSagaCompleted {
		t.Errorf("EventType = %q, want %q", got[0].EventType, saga.EventTypeSagaCompleted)
	}

	persisted, err := dao.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.Status != saga.SagaStateCompleted {
		t.Errorf("Status = %q, want %q", persisted.Status, saga.SagaStateCompleted)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Failure path
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Run_SecondStepFails_StopsBeforeThirdStep(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	step3Executed := false
	stepFailErr := &classedError{class: "slack_app_create_unauthorized"}
	steps := []saga.Step{
		&stubStep{name: "step_one"},
		&stubStep{name: "step_two", err: stepFailErr},
		&stubStep{
			name: "step_three",
			onExecute: func(_ context.Context) {
				step3Executed = true
			},
		},
	}

	err := runner.Run(context.Background(), id, steps)
	if err == nil {
		t.Fatalf("Run: err = nil, want wrapped step error")
	}
	if !errors.Is(err, stepFailErr) {
		t.Errorf("errors.Is(err, stepFailErr) = false; chain must wrap original")
	}
	if step3Executed {
		t.Errorf("step three was executed; expected fail-fast at step two")
	}

	got := keep.recorded()
	wantTypes := []string{
		saga.EventTypeSagaStepStarted,   // step_one
		saga.EventTypeSagaStepCompleted, // step_one
		saga.EventTypeSagaStepStarted,   // step_two
		saga.EventTypeSagaFailed,        // step_two
	}
	if len(got) != len(wantTypes) {
		t.Fatalf("audit-event count = %d, want %d (events: %+v)", len(got), len(wantTypes), got)
	}
	for i, want := range wantTypes {
		if got[i].EventType != want {
			t.Errorf("audit[%d].EventType = %q, want %q", i, got[i].EventType, want)
		}
	}

	persisted, persistErr := dao.Get(context.Background(), id)
	if persistErr != nil {
		t.Fatalf("Get: %v", persistErr)
	}
	if persisted.Status != saga.SagaStateFailed {
		t.Errorf("Status = %q, want %q", persisted.Status, saga.SagaStateFailed)
	}
	if persisted.LastError != "slack_app_create_unauthorized" {
		t.Errorf("LastError = %q, want %q", persisted.LastError, "slack_app_create_unauthorized")
	}
	if persisted.CurrentStep != "step_two" {
		t.Errorf("CurrentStep = %q, want %q", persisted.CurrentStep, "step_two")
	}
}

func TestRunner_Run_StepWithoutClassedError_FallsBackToDefault(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	steps := []saga.Step{
		&stubStep{name: "step_one", err: errors.New("anonymous")},
	}
	if err := runner.Run(context.Background(), id, steps); err == nil {
		t.Fatalf("Run: err = nil, want wrapped step error")
	}

	persisted, err := dao.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.LastError != saga.LastErrorClassDefault {
		t.Errorf("LastError = %q, want %q", persisted.LastError, saga.LastErrorClassDefault)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Ordering: state-write precedes execute (AC4 / M6.3.c analogue).
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Run_UpdateStepInvokedBeforeExecute(t *testing.T) {
	t.Parallel()

	rec := newRecordingDAO()
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: rec, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := rec.inner.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	var executeSeq uint64
	step := &stubStep{
		name: "step_one",
		onExecute: func(_ context.Context) {
			executeSeq = rec.tick()
		},
	}

	if err := runner.Run(context.Background(), id, []saga.Step{step}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.updateSteps) != 1 {
		t.Fatalf("UpdateStep call count = %d, want 1", len(rec.updateSteps))
	}
	if rec.updateSteps[0].seq >= executeSeq {
		t.Errorf("UpdateStep.seq=%d, Execute.seq=%d — UpdateStep must precede Execute",
			rec.updateSteps[0].seq, executeSeq)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative paths
// ────────────────────────────────────────────────────────────────────────

func TestRunner_Run_UnknownSagaID_ReturnsWrappedSentinel_EmitsNoAudit(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	err := runner.Run(context.Background(), uuid.New(), []saga.Step{&stubStep{name: "x"}})
	if err == nil {
		t.Fatalf("Run unknown id: err = nil, want wrapped ErrSagaNotFound")
	}
	if !errors.Is(err, saga.ErrSagaNotFound) {
		t.Errorf("errors.Is(err, ErrSagaNotFound) = false; got %v", err)
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("audit-event count = %d, want 0 (fail-fast before first step)", len(got))
	}
}

// ────────────────────────────────────────────────────────────────────────
// PII discipline (AC5 / M2b.7)
// ────────────────────────────────────────────────────────────────────────

// TestRunner_AuditPayloads_PIIDiscipline_KeysOnly is the wire-shape
// regression test pinned by AC5: payloads carry only `saga_id`,
// `step_name`, and (failure only) `last_error_class`. Verified via
// json.Unmarshal of the raw payload row produced by the real
// *keeperslog.Writer; the test pins the SET of keys so a future
// change adding a key (e.g. raw step params) trips this assertion.
func TestRunner_AuditPayloads_PIIDiscipline_KeysOnly(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})

	id := uuid.New()
	if err := dao.Insert(context.Background(), id, uuid.New()); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	steps := []saga.Step{
		&stubStep{name: "step_ok"},
		&stubStep{name: "step_bad", err: &classedError{class: "boom"}},
	}
	if err := runner.Run(context.Background(), id, steps); err == nil {
		t.Fatalf("Run: err = nil, want wrapped step error")
	}

	rows := keep.recorded()
	if len(rows) == 0 {
		t.Fatalf("recorded rows = 0; expected the audit chain")
	}

	for i, row := range rows {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(row.Payload, &envelope); err != nil {
			t.Fatalf("row[%d] payload not JSON: %v", i, err)
		}
		dataRaw, ok := envelope["data"]
		if !ok {
			// saga_completed is intentionally never emitted on the
			// failure path; every row in this test should carry a
			// `data` envelope.
			t.Fatalf("row[%d] (event_type=%s) missing `data` envelope", i, row.EventType)
		}
		var data map[string]any
		if err := json.Unmarshal(dataRaw, &data); err != nil {
			t.Fatalf("row[%d] data not JSON: %v", i, err)
		}
		assertPayloadKeys(t, i, row.EventType, data)
	}
}

func assertPayloadKeys(t *testing.T, idx int, eventType string, data map[string]any) {
	t.Helper()

	allowed := map[string]bool{
		"saga_id":          true,
		"step_name":        true,
		"last_error_class": true,
	}
	for k := range data {
		if !allowed[k] {
			t.Errorf("row[%d] event_type=%q payload contains forbidden key %q (allowed: saga_id, step_name, last_error_class)",
				idx, eventType, k)
		}
	}

	switch eventType {
	case saga.EventTypeSagaStepStarted, saga.EventTypeSagaStepCompleted:
		if _, ok := data["saga_id"]; !ok {
			t.Errorf("row[%d] event_type=%q missing saga_id", idx, eventType)
		}
		if _, ok := data["step_name"]; !ok {
			t.Errorf("row[%d] event_type=%q missing step_name", idx, eventType)
		}
		if _, ok := data["last_error_class"]; ok {
			t.Errorf("row[%d] event_type=%q must NOT carry last_error_class", idx, eventType)
		}
	case saga.EventTypeSagaFailed:
		if _, ok := data["saga_id"]; !ok {
			t.Errorf("row[%d] event_type=%q missing saga_id", idx, eventType)
		}
		if _, ok := data["step_name"]; !ok {
			t.Errorf("row[%d] event_type=%q missing step_name", idx, eventType)
		}
		if _, ok := data["last_error_class"]; !ok {
			t.Errorf("row[%d] event_type=%q missing last_error_class", idx, eventType)
		}
	case saga.EventTypeSagaCompleted:
		if _, ok := data["saga_id"]; !ok {
			t.Errorf("row[%d] event_type=%q missing saga_id", idx, eventType)
		}
		if _, ok := data["step_name"]; ok {
			t.Errorf("row[%d] event_type=%q must NOT carry step_name (saga-level event)", idx, eventType)
		}
	default:
		t.Errorf("row[%d] unexpected event_type=%q", idx, eventType)
	}
}

// TestEventTypes_NoLLMTurnCostPrefix is the M6.3.e prefix-collision
// regression guard: the saga emits its own closed-set vocabulary that
// must NOT collide with the `llm_turn_cost` prefix consumed by
// ReportCost. A future edit that accidentally renames a saga event to
// `llm_turn_cost_*` would silently feed bogus rows into the cost
// aggregator; pin it here.
func TestEventTypes_NoLLMTurnCostPrefix(t *testing.T) {
	t.Parallel()

	for _, et := range []string{
		saga.EventTypeSagaStepStarted,
		saga.EventTypeSagaStepCompleted,
		saga.EventTypeSagaFailed,
		saga.EventTypeSagaCompleted,
	} {
		if strings.HasPrefix(et, "llm_turn_cost") {
			t.Errorf("event_type %q has forbidden llm_turn_cost prefix (M6.3.e)", et)
		}
		if !strings.HasPrefix(et, "saga_") {
			t.Errorf("event_type %q missing saga_ prefix (vocabulary discipline)", et)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// Concurrency
// ────────────────────────────────────────────────────────────────────────

// TestRunner_Concurrency_DistinctSagaIDs runs N concurrent sagas with
// distinct ids and asserts each transitions independently. Combined
// with `go test -race`, this pins the MemorySpawnSagaDAO mutex
// contract.
func TestRunner_Concurrency_DistinctSagaIDs(t *testing.T) {
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
			steps := []saga.Step{
				&stubStep{name: "concurrent_step_one"},
				&stubStep{name: "concurrent_step_two"},
			}
			if err := runner.Run(context.Background(), id, steps); err != nil {
				t.Errorf("Run(%v): %v", id, err)
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
		if got.Status != saga.SagaStateCompleted {
			t.Errorf("Status(%v) = %q, want %q", id, got.Status, saga.SagaStateCompleted)
		}
	}
}
