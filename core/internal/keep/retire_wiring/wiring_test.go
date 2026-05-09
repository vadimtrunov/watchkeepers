package retirewiring

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// fakeLocalKeepClient stands in for the production
// [keeperslog.LocalKeepClient]. Records every LogAppend request body
// so the M7.2.a iter-1 strengthened smoke can pin that AgentID flows
// through to the audit payload's `agent_id` data field.
type fakeLocalKeepClient struct {
	mu    sync.Mutex
	calls []keepclient.LogAppendRequest
}

func (f *fakeLocalKeepClient) LogAppend(_ context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	return &keepclient.LogAppendResponse{ID: "smoke-row"}, nil
}

func (f *fakeLocalKeepClient) recorded() []keepclient.LogAppendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keepclient.LogAppendRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// recordingSagaStep is a [saga.Step] that increments a counter on
// every Execute call. Lets the smoke test pin that the registered
// Steps slice actually reaches the kickoffer's runner (M7.2.a iter-1
// strengthening).
type recordingSagaStep struct {
	name string
	runs *atomic.Int32
}

func (s *recordingSagaStep) Name() string { return s.name }

func (s *recordingSagaStep) Execute(_ context.Context) error {
	s.runs.Add(1)
	return nil
}

// TestComposeRetireKickoffer_WiresKickofferNonNil pins the M7.2.a
// composition shape: the helper returns a non-nil RetireKickoffer
// + non-nil saga DAO. A nil kickoffer would panic inside
// [spawn.NewRetireKickoffer]; this smoke also asserts the helper
// returns the DAO so a future M7.2.c wiring can retrieve it for
// diagnostic purposes.
func TestComposeRetireKickoffer_WiresKickofferNonNil(t *testing.T) {
	t.Parallel()

	deps := RetireKickofferDeps{
		KeepClient: &fakeLocalKeepClient{},
		AgentID:    "bot-watchmaster",
	}

	kickoffer, sagaDAO, err := ComposeRetireKickoffer(deps)
	if err != nil {
		t.Fatalf("ComposeRetireKickoffer: %v", err)
	}
	if kickoffer == nil {
		t.Fatal("kickoffer = nil, want non-nil")
	}
	if sagaDAO == nil {
		t.Fatal("sagaDAO = nil, want non-nil")
	}
}

// TestComposeRetireKickoffer_RejectsNilDeps pins the wiring's
// fail-closed posture: every required dep is enforced and a missing
// one surfaces a clear error rather than a runtime panic deep inside
// the composition.
func TestComposeRetireKickoffer_RejectsNilDeps(t *testing.T) {
	t.Parallel()

	good := RetireKickofferDeps{
		KeepClient: &fakeLocalKeepClient{},
		AgentID:    "bot",
	}

	type override func(*RetireKickofferDeps)
	cases := map[string]override{
		"nil KeepClient": func(d *RetireKickofferDeps) { d.KeepClient = nil },
		"empty AgentID":  func(d *RetireKickofferDeps) { d.AgentID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deps := good
			mutate(&deps)
			k, dao, err := ComposeRetireKickoffer(deps)
			if err == nil {
				t.Fatalf("ComposeRetireKickoffer: err = nil, want non-nil for %q", name)
			}
			if k != nil || dao != nil {
				t.Errorf("ComposeRetireKickoffer: returned non-nil values on error path for %q", name)
			}
		})
	}
}

// TestComposeRetireKickoffer_StepsAndAgentIDFlowThrough pins the
// non-trivial wiring contract a future M7.2.c implementer relies on
// (M7.2.a iter-1 strengthening — non-nil-only smoke is too lax to
// catch silent regressions in the helper):
//
//  1. The Steps slice the caller supplies actually REACHES the
//     kickoffer's runner (a regression that drops `deps.Steps`
//     during composition would silently produce a zero-step saga).
//  2. The returned `sagaDAO` is the SAME instance the runner inside
//     the kickoffer holds — a future M7.2.c that calls
//     `sagaDAO.Get(sagaID)` after a Kickoff sees the persisted row.
//  3. The `AgentID` propagates verbatim into the audit payload's
//     `agent_id` data field — a regression that constructs a fresh
//     bot identity inside the helper would silently re-stamp every
//     audit row.
func TestComposeRetireKickoffer_StepsAndAgentIDFlowThrough(t *testing.T) {
	t.Parallel()

	keep := &fakeLocalKeepClient{}
	runs := &atomic.Int32{}
	step := &recordingSagaStep{name: "retire_smoke_step", runs: runs}
	const wantAgentID = "bot-smoke-agent"

	deps := RetireKickofferDeps{
		KeepClient: keep,
		AgentID:    wantAgentID,
		Steps:      []saga.Step{step},
	}

	kickoffer, sagaDAO, err := ComposeRetireKickoffer(deps)
	if err != nil {
		t.Fatalf("ComposeRetireKickoffer: %v", err)
	}

	sagaID := uuid.New()
	mvID := uuid.New()
	wkID := uuid.New()
	if err := kickoffer.Kickoff(context.Background(), sagaID, mvID, wkID, saga.SpawnClaim{}, "tok-smoke"); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	// Assertion 1: registered step ran (Steps reached the kickoffer).
	if got := runs.Load(); got != 1 {
		t.Errorf("recording step Execute count = %d, want 1 (helper must forward Steps to the kickoffer)", got)
	}

	// Assertion 2: returned sagaDAO is shared with the runner —
	// reading the row back surfaces the saga's terminal state.
	row, err := sagaDAO.Get(context.Background(), sagaID)
	if err != nil {
		t.Fatalf("sagaDAO.Get: %v (helper must return the same DAO instance the runner uses)", err)
	}
	if row.Status != saga.SagaStateCompleted {
		t.Errorf("saga Status = %q, want %q (DAO must be shared with the runner)",
			row.Status, saga.SagaStateCompleted)
	}

	// Assertion 3: AgentID propagated to the audit payload's
	// `agent_id` data field — pin via the first recorded call which
	// is always `retire_approved_for_watchkeeper` (audit-emit
	// precedes saga DAO Insert).
	calls := keep.recorded()
	if len(calls) == 0 {
		t.Fatal("no audit calls recorded; expected the retire_approved_for_watchkeeper row")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(calls[0].Payload, &envelope); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		t.Fatalf("data not JSON: %v", err)
	}
	if got, _ := data["agent_id"].(string); got != wantAgentID {
		t.Errorf("audit payload agent_id = %q, want %q (helper must forward AgentID verbatim)", got, wantAgentID)
	}
}
