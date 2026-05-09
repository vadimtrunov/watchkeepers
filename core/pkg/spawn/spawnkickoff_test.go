package spawn_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// ────────────────────────────────────────────────────────────────────────
// Hand-rolled fakes (M3.6 / M6.3.e pattern — no mocking lib).
// ────────────────────────────────────────────────────────────────────────

// fakeLocalKeepClient stands in for [keeperslog.LocalKeepClient]; lets
// the kickoffer tests assert event_type ordering through a real
// *keeperslog.Writer without standing up the full HTTP keepclient.
type fakeLocalKeepClient struct {
	mu          sync.Mutex
	calls       []keepclient.LogAppendRequest
	errToReturn error
}

func (f *fakeLocalKeepClient) LogAppend(_ context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errToReturn != nil {
		return nil, f.errToReturn
	}
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

func (f *fakeLocalKeepClient) eventTypes() []string {
	rows := f.recorded()
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.EventType
	}
	return out
}

// recordingDAO wraps a [saga.MemorySpawnSagaDAO] and timestamps every
// Insert / Get / UpdateStep / MarkCompleted / MarkFailed call into a
// shared monotonic counter so the AC6 "Insert precedes Run" assertion
// can pin call order without time.Now timing ambiguity. The Run-side
// of the ordering (the saga runner's first action is Get) is captured
// because Get is on the same DAO surface.
type recordingDAO struct {
	inner *saga.MemorySpawnSagaDAO

	mu        sync.Mutex
	seq       uint64
	insertSeq []uint64
	getSeq    []uint64
	insertErr error
}

func newRecordingDAO() *recordingDAO {
	return &recordingDAO{inner: saga.NewMemorySpawnSagaDAO(nil)}
}

func (r *recordingDAO) tick() uint64 { return atomic.AddUint64(&r.seq, 1) }

func (r *recordingDAO) Insert(ctx context.Context, id uuid.UUID, mvID uuid.UUID) error {
	t := r.tick()
	r.mu.Lock()
	r.insertSeq = append(r.insertSeq, t)
	r.mu.Unlock()
	if r.insertErr != nil {
		return r.insertErr
	}
	return r.inner.Insert(ctx, id, mvID)
}

func (r *recordingDAO) Get(ctx context.Context, id uuid.UUID) (saga.Saga, error) {
	t := r.tick()
	r.mu.Lock()
	r.getSeq = append(r.getSeq, t)
	r.mu.Unlock()
	return r.inner.Get(ctx, id)
}

func (r *recordingDAO) UpdateStep(ctx context.Context, id uuid.UUID, step string) error {
	return r.inner.UpdateStep(ctx, id, step)
}

func (r *recordingDAO) MarkCompleted(ctx context.Context, id uuid.UUID) error {
	return r.inner.MarkCompleted(ctx, id)
}

func (r *recordingDAO) MarkFailed(ctx context.Context, id uuid.UUID, lastErr string) error {
	return r.inner.MarkFailed(ctx, id, lastErr)
}

// newKickoffer composes a real saga.Runner backed by `dao` plus a real
// *keeperslog.Writer, and returns a kickoffer wired to all three.
// Hoisted so the per-branch tests stay scannable.
func newKickoffer(t *testing.T, dao saga.SpawnSagaDAO, keep *fakeLocalKeepClient, agentID string) *spawn.SpawnKickoffer {
	t.Helper()
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	return spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: agentID,
	})
}

// testSpawnClaim is the canonical claim used across the kickoff
// tests. The matrix entry is the M6.1.a "slack_app_create=lead_approval"
// pair the M7.1.c.a CreateApp step's gate consults.
func testSpawnClaim() saga.SpawnClaim {
	return saga.SpawnClaim{
		OrganizationID: "org-test",
		AgentID:        "agent-watchmaster",
		AuthorityMatrix: map[string]string{
			"slack_app_create": "lead_approval",
		},
	}
}

// kickoffWithDefaults invokes the M7.1.c.c-extended Kickoff signature
// with a fresh watchkeeperID and the canonical [testSpawnClaim]. Lets
// existing M7.1.b tests stay focused on their specific assertions
// (audit ordering, payload PII discipline) without re-stating the
// per-call values on every line.
func kickoffWithDefaults(
	ctx context.Context,
	k *spawn.SpawnKickoffer,
	sagaID, manifestVersionID uuid.UUID,
	approvalToken string,
) error {
	return k.Kickoff(ctx, sagaID, manifestVersionID, uuid.New(), testSpawnClaim(), approvalToken)
}

// ────────────────────────────────────────────────────────────────────────
// Constructor panics
// ────────────────────────────────────────────────────────────────────────

func TestNewSpawnKickoffer_NilLogger_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewSpawnKickoffer with nil Logger did not panic")
		}
	}()
	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: keeperslog.New(keep)})
	_ = spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{DAO: dao, Runner: runner, AgentID: "bot"})
}

func TestNewSpawnKickoffer_NilDAO_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewSpawnKickoffer with nil DAO did not panic")
		}
	}()
	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	_ = spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{Logger: writer, Runner: runner, AgentID: "bot"})
}

func TestNewSpawnKickoffer_NilRunner_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewSpawnKickoffer with nil Runner did not panic")
		}
	}()
	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	_ = spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{Logger: keeperslog.New(keep), DAO: dao, AgentID: "bot"})
}

func TestNewSpawnKickoffer_EmptyAgentID_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewSpawnKickoffer with empty AgentID did not panic")
		}
	}()
	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	_ = spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{Logger: writer, DAO: dao, Runner: runner})
}

// ────────────────────────────────────────────────────────────────────────
// Happy path — AC2 (M7.1.b test plan: 1 manifest_approved_for_spawn +
// 1 saga_completed, in that order, since steps list is empty).
// ────────────────────────────────────────────────────────────────────────

func TestSpawnKickoff_HappyPath_EmitsTwoEventsInOrder(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	k := newKickoffer(t, dao, keep, "bot-watchmaster")

	sagaID := uuid.New()
	mvID := uuid.New()
	token := strings.Repeat("ZZ", 16)

	if err := kickoffWithDefaults(context.Background(), k, sagaID, mvID, token); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	wantEvents := []string{
		spawn.EventTypeManifestApprovedForSpawn,
		saga.EventTypeSagaCompleted,
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}

	// The saga row was persisted and reached the terminal completed
	// state (zero-step run completes immediately per M7.1.a).
	row, err := dao.Get(context.Background(), sagaID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if row.Status != saga.SagaStateCompleted {
		t.Errorf("Status = %q, want %q", row.Status, saga.SagaStateCompleted)
	}
	if row.ManifestVersionID != mvID {
		t.Errorf("ManifestVersionID = %q, want %q", row.ManifestVersionID, mvID)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Insert-before-Run ordering — AC6.
// ────────────────────────────────────────────────────────────────────────

// TestSpawnKickoff_InsertPrecedesRun pins the AC6 ordering pin: the
// recording DAO captures Insert and Get sequences; the runner's first
// action is Get(sagaID), so Insert.seq < Get.seq is the wire-shape
// regression test for "Insert before Run".
func TestSpawnKickoff_InsertPrecedesRun(t *testing.T) {
	t.Parallel()

	rec := newRecordingDAO()
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: rec, Logger: writer})
	k := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     rec,
		Runner:  runner,
		AgentID: "bot",
	})

	if err := kickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-test"); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.insertSeq) != 1 {
		t.Fatalf("Insert call count = %d, want 1", len(rec.insertSeq))
	}
	if len(rec.getSeq) != 1 {
		t.Fatalf("Get call count = %d, want 1 (saga.Runner.Run resolves the row first)", len(rec.getSeq))
	}
	if rec.insertSeq[0] >= rec.getSeq[0] {
		t.Errorf("Insert.seq=%d, Get.seq=%d — Insert MUST precede Run (Get is the runner's first action)",
			rec.insertSeq[0], rec.getSeq[0])
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative paths — append + insert error propagation.
// ────────────────────────────────────────────────────────────────────────

// TestSpawnKickoff_AppendError_StopsBeforeInsert pins the test plan
// "Kickoff propagates keeperslog.Append error → returned wrapped error;
// Insert/Run NOT called; no orphan saga row".
func TestSpawnKickoff_AppendError_StopsBeforeInsert(t *testing.T) {
	t.Parallel()

	rec := newRecordingDAO()
	appendErr := errors.New("keep client offline")
	keep := &fakeLocalKeepClient{errToReturn: appendErr}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: rec, Logger: writer})
	k := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     rec,
		Runner:  runner,
		AgentID: "bot",
	})

	err := kickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-test")
	if err == nil {
		t.Fatal("Kickoff returned nil, want non-nil error")
	}
	if !errors.Is(err, appendErr) {
		t.Errorf("errors.Is(err, appendErr) = false; want wrapped chain (got %v)", err)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.insertSeq) != 0 {
		t.Errorf("Insert calls = %d, want 0 (audit failure must stop before persistence)", len(rec.insertSeq))
	}
	if len(rec.getSeq) != 0 {
		t.Errorf("Get calls = %d, want 0 (Run must not fire after audit failure)", len(rec.getSeq))
	}
}

// TestSpawnKickoff_InsertError_AuditAlreadyEmitted pins the test plan
// "Kickoff propagates SpawnSagaDAO.Insert error → returned wrapped
// error; Run NOT called; audit event already emitted (acceptable —
// emit-before-state is the M6.3.e + M7.1.a pattern; the audit row is
// the canonical 'we tried' signal even when state-persistence fails)".
func TestSpawnKickoff_InsertError_AuditAlreadyEmitted(t *testing.T) {
	t.Parallel()

	rec := newRecordingDAO()
	rec.insertErr = errors.New("postgres unreachable")
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: rec, Logger: writer})
	k := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     rec,
		Runner:  runner,
		AgentID: "bot",
	})

	err := kickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-test")
	if err == nil {
		t.Fatal("Kickoff returned nil, want non-nil error")
	}
	if !errors.Is(err, rec.insertErr) {
		t.Errorf("errors.Is(err, insertErr) = false; want wrapped chain (got %v)", err)
	}

	// Audit row already emitted; the manifest_approved_for_spawn event
	// is the canonical "we tried" signal.
	wantEvents := []string{spawn.EventTypeManifestApprovedForSpawn}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Errorf("event_type chain = %v, want %v", got, wantEvents)
	}

	// Run was NOT called (no Get on the recording DAO).
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.getSeq) != 0 {
		t.Errorf("Get calls = %d, want 0 (Run must not fire after Insert failure)", len(rec.getSeq))
	}
}

// ────────────────────────────────────────────────────────────────────────
// Security — token-prefix discipline + PII keys-only payload (AC5).
// ────────────────────────────────────────────────────────────────────────

// TestSpawnKickoff_PayloadPIIDiscipline pins AC5 — the
// `manifest_approved_for_spawn` payload is restricted to exactly the
// three approved keys: `manifest_version_id`, `approval_token_prefix`,
// `agent_id`. NO other keys allowed; NO `approval_token` (full); NO
// `error` strings. Verified via json.Unmarshal of the raw payload row
// produced by the real *keeperslog.Writer.
func TestSpawnKickoff_PayloadPIIDiscipline(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	k := newKickoffer(t, dao, keep, "bot-watchmaster")

	sagaID := uuid.New()
	mvID := uuid.New()
	token := strings.Repeat("ZZ", 16) // 32-char canary

	if err := kickoffWithDefaults(context.Background(), k, sagaID, mvID, token); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	rows := keep.recorded()
	if len(rows) == 0 {
		t.Fatalf("recorded rows = 0; expected the audit chain")
	}

	// Pick out the manifest_approved_for_spawn row (always row 0 per
	// the audit-emit-before-Insert pattern).
	row := rows[0]
	if row.EventType != spawn.EventTypeManifestApprovedForSpawn {
		t.Fatalf("row[0].EventType = %q, want %q", row.EventType, spawn.EventTypeManifestApprovedForSpawn)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(row.Payload, &envelope); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	dataRaw, ok := envelope["data"]
	if !ok {
		t.Fatalf("payload missing `data` envelope: %s", row.Payload)
	}
	var data map[string]any
	if err := json.Unmarshal(dataRaw, &data); err != nil {
		t.Fatalf("data not JSON: %v", err)
	}

	allowed := map[string]bool{
		"manifest_version_id":   true,
		"approval_token_prefix": true,
		"agent_id":              true,
	}
	if len(data) != len(allowed) {
		t.Errorf("payload key count = %d, want %d (keys=%v)", len(data), len(allowed), data)
	}
	for k := range data {
		if !allowed[k] {
			t.Errorf("payload contains forbidden key %q (allowed: manifest_version_id, approval_token_prefix, agent_id)", k)
		}
	}

	// Required-keys check.
	for k := range allowed {
		if _, ok := data[k]; !ok {
			t.Errorf("payload missing required key %q", k)
		}
	}

	// PII guard: the full token MUST NOT appear anywhere in the
	// payload bytes; the `approval_token` (full) key MUST NOT exist;
	// `error` substring MUST NOT appear.
	rawPayload := string(row.Payload)
	if strings.Contains(rawPayload, token) {
		t.Errorf("payload leaked full approval_token: %s", rawPayload)
	}
	if strings.Contains(rawPayload, `"approval_token"`) {
		t.Errorf("payload contains forbidden `approval_token` key: %s", rawPayload)
	}
	if strings.Contains(rawPayload, `"error"`) {
		t.Errorf("payload contains forbidden `error` key: %s", rawPayload)
	}
}

// TestSpawnKickoff_TokenPrefixDiscipline pins the M6.3.b
// token-prefix-display lesson: the audit payload's
// `approval_token_prefix` is `tok-<first-6-chars>` and is NOT the
// full 32-char token.
func TestSpawnKickoff_TokenPrefixDiscipline(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	k := newKickoffer(t, dao, keep, "bot")

	fullToken := strings.Repeat("ZZ", 16) // 32-char canary
	const wantPrefix = "tok-ZZZZZZ"

	if err := kickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), fullToken); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	row := keep.recorded()[0]
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(row.Payload, &envelope); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		t.Fatalf("data not JSON: %v", err)
	}
	got, _ := data["approval_token_prefix"].(string)
	if got != wantPrefix {
		t.Errorf("approval_token_prefix = %q, want %q", got, wantPrefix)
	}
	if got == fullToken {
		t.Errorf("approval_token_prefix equals full token — token-prefix discipline failed")
	}
}

// TestSpawnKickoff_TokenPrefixShortToken pins the defensive fallback
// branch: tokens shorter than 6 runes round-trip in full (still
// prefixed) so the helper never panics.
func TestSpawnKickoff_TokenPrefixShortToken(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	k := newKickoffer(t, dao, keep, "bot")

	const shortToken = "abc"
	if err := kickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), shortToken); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}
	row := keep.recorded()[0]
	if !strings.Contains(string(row.Payload), `"approval_token_prefix":"tok-abc"`) {
		t.Errorf("payload missing tok-abc prefix: %s", row.Payload)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Event-type vocabulary discipline (M6.3.e prefix-collision guard).
// ────────────────────────────────────────────────────────────────────────

// TestEventTypeManifestApprovedForSpawn_NoPrefixCollision pins the
// closed-set vocabulary discipline: the kickoff event_type must equal
// exactly `manifest_approved_for_spawn` (a typo like
// `manifest_approved_for_spam` would slip past a `HasPrefix` check)
// AND must NOT collide with the `llm_turn_cost_*` family established
// in M6.3.e or the `saga_*` family established in M7.1.a — a future
// edit that accidentally renamed it would silently feed bogus rows
// into one of those aggregators.
func TestEventTypeManifestApprovedForSpawn_NoPrefixCollision(t *testing.T) {
	t.Parallel()

	et := spawn.EventTypeManifestApprovedForSpawn
	const want = "manifest_approved_for_spawn"
	if et != want {
		t.Errorf("event_type = %q, want exact %q", et, want)
	}
	// Defense-in-depth: even if the constant is renamed, it must not
	// alias into a sibling event-type family the aggregators consume.
	if strings.HasPrefix(et, "llm_turn_cost") {
		t.Errorf("event_type %q has forbidden llm_turn_cost prefix (M6.3.e)", et)
	}
	if strings.HasPrefix(et, "saga_") {
		t.Errorf("event_type %q has forbidden saga_ prefix (M7.1.a)", et)
	}
}

// equalStrings is a tiny helper that beats reflect.DeepEqual for
// readability on assertion failure. Delegates to [slices.Equal] from
// the stdlib so the bounds-checking discipline lives in one place.
func equalStrings(a, b []string) bool {
	return slices.Equal(a, b)
}

// ────────────────────────────────────────────────────────────────────────
// M7.1.c.c step-list registration: SpawnContext seeded on ctx; the
// runner walks the configured step list in order.
// ────────────────────────────────────────────────────────────────────────

// recordingSagaStep is a hand-rolled [saga.Step] that records the
// per-call SpawnContext (read off the supplied ctx) plus an order tick
// so a multi-step happy-path test can assert (a) every step received
// the same SpawnContext and (b) the runner invoked them in the
// registered order.
type recordingSagaStep struct {
	name string
	mu   *sync.Mutex
	seq  *atomic.Int32
	rec  *recordingStepLog
}

type recordingStepLog struct {
	mu      sync.Mutex
	entries []recordingStepEntry
}

type recordingStepEntry struct {
	stepName     string
	tickOrder    int32
	spawnContext saga.SpawnContext
	hadContext   bool
}

func newRecordingStepLog() *recordingStepLog { return &recordingStepLog{} }

func (l *recordingStepLog) recorded() []recordingStepEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]recordingStepEntry, len(l.entries))
	copy(out, l.entries)
	return out
}

func (s *recordingSagaStep) Name() string { return s.name }

func (s *recordingSagaStep) Execute(ctx context.Context) error {
	sc, ok := saga.SpawnContextFromContext(ctx)
	tick := s.seq.Add(1)
	s.rec.mu.Lock()
	s.rec.entries = append(s.rec.entries, recordingStepEntry{
		stepName:     s.name,
		tickOrder:    tick,
		spawnContext: sc,
		hadContext:   ok,
	})
	s.rec.mu.Unlock()
	return nil
}

// TestSpawnKickoff_RegisteredSteps_RunInOrderWithSeededContext pins the
// M7.1.c.c contract: the kickoffer hands [saga.Runner.Run] its
// registered step list (no longer hard-coded `[]saga.Step{}`); each
// step sees a SpawnContext seeded with the per-call manifest_version
// id, watchkeeper id (= AgentID), and Watchmaster claim. The assertion
// suite is the wire-shape regression test M7.1.d / M7.1.e will lean
// on when adding their own steps.
func TestSpawnKickoff_RegisteredSteps_RunInOrderWithSeededContext(t *testing.T) {
	t.Parallel()

	rec := newRecordingStepLog()
	tick := &atomic.Int32{}
	mu := &sync.Mutex{}
	step1 := &recordingSagaStep{name: "step_one", mu: mu, seq: tick, rec: rec}
	step2 := &recordingSagaStep{name: "step_two", mu: mu, seq: tick, rec: rec}
	step3 := &recordingSagaStep{name: "step_three", mu: mu, seq: tick, rec: rec}

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot-watchmaster",
		Steps:   []saga.Step{step1, step2, step3},
	})

	sagaID := uuid.New()
	mvID := uuid.New()
	wkID := uuid.New()
	claim := testSpawnClaim()
	const token = "tok-stepwiring"

	if err := k.Kickoff(context.Background(), sagaID, mvID, wkID, claim, token); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	entries := rec.recorded()
	if len(entries) != 3 {
		t.Fatalf("recorded step entries = %d, want 3", len(entries))
	}

	// Order pin — ticks are monotonically increasing per Execute call.
	wantNames := []string{"step_one", "step_two", "step_three"}
	for i, want := range wantNames {
		if entries[i].stepName != want {
			t.Errorf("step #%d name = %q, want %q", i, entries[i].stepName, want)
		}
	}
	if entries[0].tickOrder >= entries[1].tickOrder || entries[1].tickOrder >= entries[2].tickOrder {
		t.Errorf("step tick order = %v, want strictly ascending", []int32{entries[0].tickOrder, entries[1].tickOrder, entries[2].tickOrder})
	}

	// SpawnContext pin — every step saw the same per-saga values.
	for _, entry := range entries {
		if !entry.hadContext {
			t.Errorf("%s: SpawnContextFromContext(ctx) ok = false; want true", entry.stepName)
			continue
		}
		if entry.spawnContext.ManifestVersionID != mvID {
			t.Errorf("%s: SpawnContext.ManifestVersionID = %q, want %q",
				entry.stepName, entry.spawnContext.ManifestVersionID, mvID)
		}
		if entry.spawnContext.AgentID != wkID {
			t.Errorf("%s: SpawnContext.AgentID = %q, want %q (watchkeeperID)",
				entry.stepName, entry.spawnContext.AgentID, wkID)
		}
		if entry.spawnContext.Claim.OrganizationID != claim.OrganizationID {
			t.Errorf("%s: SpawnContext.Claim.OrganizationID = %q, want %q",
				entry.stepName, entry.spawnContext.Claim.OrganizationID, claim.OrganizationID)
		}
		if entry.spawnContext.Claim.AuthorityMatrix["slack_app_create"] != "lead_approval" {
			t.Errorf("%s: SpawnContext.Claim.AuthorityMatrix[slack_app_create] = %q, want lead_approval",
				entry.stepName, entry.spawnContext.Claim.AuthorityMatrix["slack_app_create"])
		}
	}

	// Saga reaches terminal state — three steps + one completed event
	// surface as one manifest_approved_for_spawn + three pairs of
	// (started, completed) + one saga_completed.
	wantEvents := []string{
		spawn.EventTypeManifestApprovedForSpawn,
		saga.EventTypeSagaStepStarted, saga.EventTypeSagaStepCompleted,
		saga.EventTypeSagaStepStarted, saga.EventTypeSagaStepCompleted,
		saga.EventTypeSagaStepStarted, saga.EventTypeSagaStepCompleted,
		saga.EventTypeSagaCompleted,
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
}

// TestSpawnKickoff_NilSteps_RetainsZeroStepBehaviour pins backward
// compatibility: a nil / empty Steps slice keeps the M7.1.b zero-step
// behaviour (one `manifest_approved_for_spawn` + one `saga_completed`),
// so callers that have not yet wired their step list still produce a
// completing saga.
func TestSpawnKickoff_NilSteps_RetainsZeroStepBehaviour(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot",
		// Steps deliberately omitted — nil slice.
	})

	if err := kickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-nil"); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	wantEvents := []string{
		spawn.EventTypeManifestApprovedForSpawn,
		saga.EventTypeSagaCompleted,
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
}

// TestSpawnKickoff_StepFailure_AuditsSagaFailed pins the failure
// pathway: a registered step returning an error surfaces as
// `saga_failed` on the audit chain (the saga.Runner owns this) AND
// the kickoffer returns the wrapped error. Confirms the M7.1.c.c
// wiring does not swallow step failures.
func TestSpawnKickoff_StepFailure_AuditsSagaFailed(t *testing.T) {
	t.Parallel()

	stepErr := errors.New("simulated step failure")
	failing := &errSagaStep{name: "boom", err: stepErr}

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot",
		Steps:   []saga.Step{failing},
	})

	err := k.Kickoff(context.Background(), uuid.New(), uuid.New(), uuid.New(), testSpawnClaim(), "tok-fail")
	if err == nil {
		t.Fatal("Kickoff returned nil, want wrapped step error")
	}
	if !errors.Is(err, stepErr) {
		t.Errorf("errors.Is(err, stepErr) = false; got %v", err)
	}

	wantEvents := []string{
		spawn.EventTypeManifestApprovedForSpawn,
		saga.EventTypeSagaStepStarted,
		saga.EventTypeSagaFailed,
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
}

// errSagaStep is a one-method [saga.Step] that always returns the
// configured error. Used to drive the failure-path test above.
type errSagaStep struct {
	name string
	err  error
}

func (s *errSagaStep) Name() string                    { return s.name }
func (s *errSagaStep) Execute(_ context.Context) error { return s.err }

// ────────────────────────────────────────────────────────────────────────
// M7.1.c.c iter-1 codex-review fix: fail-fast validation rejects
// uuid.Nil kickoff args BEFORE Append/Insert side effects.
// ────────────────────────────────────────────────────────────────────────

func TestSpawnKickoff_FailsFastOnNilArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name              string
		sagaID            uuid.UUID
		manifestVersionID uuid.UUID
		watchkeeperID     uuid.UUID
	}{
		{
			name:              "nil sagaID",
			sagaID:            uuid.Nil,
			manifestVersionID: uuid.New(),
			watchkeeperID:     uuid.New(),
		},
		{
			name:              "nil manifestVersionID",
			sagaID:            uuid.New(),
			manifestVersionID: uuid.Nil,
			watchkeeperID:     uuid.New(),
		},
		{
			name:              "nil watchkeeperID",
			sagaID:            uuid.New(),
			manifestVersionID: uuid.New(),
			watchkeeperID:     uuid.Nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rec := newRecordingDAO()
			keep := &fakeLocalKeepClient{}
			writer := keeperslog.New(keep)
			runner := saga.NewRunner(saga.Dependencies{DAO: rec, Logger: writer})
			k := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
				Logger:  writer,
				DAO:     rec,
				Runner:  runner,
				AgentID: "bot",
			})

			err := k.Kickoff(context.Background(), tc.sagaID, tc.manifestVersionID, tc.watchkeeperID, testSpawnClaim(), "tok-fail-fast")
			if err == nil {
				t.Fatalf("Kickoff: err = nil, want wrapped ErrInvalidKickoffArgs")
			}
			if !errors.Is(err, spawn.ErrInvalidKickoffArgs) {
				t.Errorf("errors.Is(err, ErrInvalidKickoffArgs) = false; got %v", err)
			}

			// AC: NO audit row emitted; NO saga row inserted; NO Run.
			if got := keep.recorded(); len(got) != 0 {
				t.Errorf("keep rows = %d, want 0 (fail-fast must precede audit)", len(got))
			}
			rec.mu.Lock()
			defer rec.mu.Unlock()
			if len(rec.insertSeq) != 0 {
				t.Errorf("Insert calls = %d, want 0 (fail-fast must precede persistence)", len(rec.insertSeq))
			}
			if len(rec.getSeq) != 0 {
				t.Errorf("Get calls = %d, want 0 (Run must not fire on fail-fast)", len(rec.getSeq))
			}
		})
	}
}
