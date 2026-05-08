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

	if err := k.Kickoff(context.Background(), sagaID, mvID, token); err != nil {
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

	if err := k.Kickoff(context.Background(), uuid.New(), uuid.New(), "tok-test"); err != nil {
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

	err := k.Kickoff(context.Background(), uuid.New(), uuid.New(), "tok-test")
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

	err := k.Kickoff(context.Background(), uuid.New(), uuid.New(), "tok-test")
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

	if err := k.Kickoff(context.Background(), sagaID, mvID, token); err != nil {
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

	if err := k.Kickoff(context.Background(), uuid.New(), uuid.New(), fullToken); err != nil {
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
	if err := k.Kickoff(context.Background(), uuid.New(), uuid.New(), shortToken); err != nil {
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
// closed-set vocabulary discipline: the kickoff event_type must NOT
// collide with the `llm_turn_cost_*` family established in M6.3.e or
// the `saga_*` family established in M7.1.a — a future edit that
// accidentally renamed it would silently feed bogus rows into one of
// those aggregators.
func TestEventTypeManifestApprovedForSpawn_NoPrefixCollision(t *testing.T) {
	t.Parallel()

	et := spawn.EventTypeManifestApprovedForSpawn
	if !strings.HasPrefix(et, "manifest_approved_for_spawn") {
		t.Errorf("event_type %q does not start with manifest_approved_for_spawn", et)
	}
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
