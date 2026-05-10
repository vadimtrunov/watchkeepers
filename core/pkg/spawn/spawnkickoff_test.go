package spawn_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
//
// Records every per-call `ctx` in addition to the request body so
// retire-kickoff ctx-propagation tests can assert the kickoffer
// forwards the caller's ctx verbatim through the [keeperslog.Writer]
// seam (M7.2.a iter-1: recording fake, not discarding — M7.1.e
// lesson #6 held forward).
type fakeLocalKeepClient struct {
	mu          sync.Mutex
	calls       []keepclient.LogAppendRequest
	ctxs        []context.Context
	errToReturn error
}

func (f *fakeLocalKeepClient) LogAppend(ctx context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.errToReturn != nil {
		return nil, f.errToReturn
	}
	f.calls = append(f.calls, req)
	f.ctxs = append(f.ctxs, ctx)
	return &keepclient.LogAppendResponse{ID: "fake-row"}, nil
}

func (f *fakeLocalKeepClient) recorded() []keepclient.LogAppendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keepclient.LogAppendRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

func (f *fakeLocalKeepClient) recordedCtxs() []context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]context.Context, len(f.ctxs))
	copy(out, f.ctxs)
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

	mu                 sync.Mutex
	seq                uint64
	insertSeq          []uint64
	insertIfAbsentSeq  []uint64
	getSeq             []uint64
	updateStepSeq      []uint64
	markCompletedSeq   []uint64
	markFailedSeq      []uint64
	insertErr          error
	insertIfAbsentErr  error
	insertIfAbsentKeys []string
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

func (r *recordingDAO) InsertIfAbsent(
	ctx context.Context,
	id uuid.UUID,
	mvID uuid.UUID,
	wkID uuid.UUID,
	idempotencyKey string,
) (saga.IdempotentInsertResult, error) {
	t := r.tick()
	r.mu.Lock()
	r.insertIfAbsentSeq = append(r.insertIfAbsentSeq, t)
	r.insertIfAbsentKeys = append(r.insertIfAbsentKeys, idempotencyKey)
	r.mu.Unlock()
	if r.insertIfAbsentErr != nil {
		return saga.IdempotentInsertResult{}, r.insertIfAbsentErr
	}
	return r.inner.InsertIfAbsent(ctx, id, mvID, wkID, idempotencyKey)
}

func (r *recordingDAO) Get(ctx context.Context, id uuid.UUID) (saga.Saga, error) {
	t := r.tick()
	r.mu.Lock()
	r.getSeq = append(r.getSeq, t)
	r.mu.Unlock()
	return r.inner.Get(ctx, id)
}

func (r *recordingDAO) UpdateStep(ctx context.Context, id uuid.UUID, step string) error {
	t := r.tick()
	r.mu.Lock()
	r.updateStepSeq = append(r.updateStepSeq, t)
	r.mu.Unlock()
	return r.inner.UpdateStep(ctx, id, step)
}

func (r *recordingDAO) MarkCompleted(ctx context.Context, id uuid.UUID) error {
	t := r.tick()
	r.mu.Lock()
	r.markCompletedSeq = append(r.markCompletedSeq, t)
	r.mu.Unlock()
	return r.inner.MarkCompleted(ctx, id)
}

func (r *recordingDAO) MarkFailed(ctx context.Context, id uuid.UUID, lastErr string) error {
	t := r.tick()
	r.mu.Lock()
	r.markFailedSeq = append(r.markFailedSeq, t)
	r.mu.Unlock()
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
// recording DAO captures InsertIfAbsent and Get sequences; the runner's
// first action is Get(sagaID), so InsertIfAbsent.seq < Get.seq is the
// wire-shape regression test for "Insert before Run". Mirrors the
// M7.1.b assertion under the M7.3.a-renamed DAO method.
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
	if len(rec.insertIfAbsentSeq) != 1 {
		t.Fatalf("InsertIfAbsent call count = %d, want 1", len(rec.insertIfAbsentSeq))
	}
	if len(rec.getSeq) != 1 {
		t.Fatalf("Get call count = %d, want 1 (saga.Runner.Run resolves the row first)", len(rec.getSeq))
	}
	if rec.insertIfAbsentSeq[0] >= rec.getSeq[0] {
		t.Errorf("InsertIfAbsent.seq=%d, Get.seq=%d — InsertIfAbsent MUST precede Run (Get is the runner's first action)",
			rec.insertIfAbsentSeq[0], rec.getSeq[0])
	}
	// Insert (legacy method) MUST NOT be called by the kickoffer —
	// every M7.3.a kickoff routes through InsertIfAbsent. Pinned so
	// a future regression that swaps back to Insert (silently
	// disabling idempotency dedup) is caught.
	if len(rec.insertSeq) != 0 {
		t.Errorf("Insert (legacy) call count = %d, want 0 (M7.3.a routes through InsertIfAbsent)", len(rec.insertSeq))
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative paths — append + insert error propagation.
// ────────────────────────────────────────────────────────────────────────

// TestSpawnKickoff_AppendError_StopsBeforeRun pins the test plan
// "Kickoff propagates keeperslog.Append error → returned wrapped
// error; Run NOT called; no orphan saga state-write past
// InsertIfAbsent". M7.3.a inverts the M7.1.b ordering: InsertIfAbsent
// runs FIRST so the kickoffer can choose insert-vs-replay event_type.
// On the insert branch, the row IS persisted before Append fires (no
// way around — the result.Inserted bool comes from the DAO call); the
// caller's retry hits the replay branch and surfaces the error there.
// On the replay branch, no extra state-write happens — the existing
// row is unaffected.
func TestSpawnKickoff_AppendError_StopsBeforeRun(t *testing.T) {
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
	if len(rec.insertIfAbsentSeq) != 1 {
		t.Errorf("InsertIfAbsent calls = %d, want 1 (decides insert-vs-replay before Append)", len(rec.insertIfAbsentSeq))
	}
	if len(rec.getSeq) != 0 {
		t.Errorf("Get calls = %d, want 0 (Run must not fire after audit failure)", len(rec.getSeq))
	}
}

// TestSpawnKickoff_InsertIfAbsentError_NoAuditRow pins the M7.3.a
// inversion of the M7.1.b "audit-emit precedes Insert" invariant: the
// kickoffer no longer emits manifest_approved_for_spawn BEFORE the
// DAO call, because the event_type depends on whether the row is a
// fresh insert or an idempotency replay. On a transient
// InsertIfAbsent error the kickoffer surfaces the wrapped error and
// emits NO audit row; the operator's retry on the same idempotency
// key (approval_token) will dedup cleanly via the partial UNIQUE
// index. The downstream dispatcher's existing `approval_replay_failed`
// audit row covers the operator-visible failure surface.
func TestSpawnKickoff_InsertIfAbsentError_NoAuditRow(t *testing.T) {
	t.Parallel()

	rec := newRecordingDAO()
	rec.insertIfAbsentErr = errors.New("postgres unreachable")
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
	if !errors.Is(err, rec.insertIfAbsentErr) {
		t.Errorf("errors.Is(err, insertIfAbsentErr) = false; want wrapped chain (got %v)", err)
	}

	// NO audit row was emitted — the kickoffer cannot know whether
	// the call was an original or a replay until InsertIfAbsent
	// returns, so a transient error short-circuits cleanly.
	if got := keep.eventTypes(); len(got) != 0 {
		t.Errorf("event_type chain = %v, want empty (transient DAO error must not leak partial audit)", got)
	}

	// Run was NOT called (no Get on the recording DAO).
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.getSeq) != 0 {
		t.Errorf("Get calls = %d, want 0 (Run must not fire after InsertIfAbsent failure)", len(rec.getSeq))
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

	// Pick out the manifest_approved_for_spawn row (always row 0 —
	// it's the first event the kickoffer emits after a successful
	// InsertIfAbsent on the insert branch; the saga_completed row
	// follows for the zero-step kickoff).
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
		// M7.3.b: the runner emits a `saga_compensated` summary
		// row on every failure path (zero per-step rows here
		// because `boom` is a non-Compensator stub).
		saga.EventTypeSagaCompensated,
		// M7.3.b: the kickoffer emits the operator-visible
		// rejection row AFTER the saga's own failure chain so a
		// downstream consumer can mark the Manifest rejected.
		spawn.EventTypeManifestRejectedAfterSpawnFailure,
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
		approvalToken     string
	}{
		{
			name:              "nil sagaID",
			sagaID:            uuid.Nil,
			manifestVersionID: uuid.New(),
			watchkeeperID:     uuid.New(),
			approvalToken:     "tok-fail-fast",
		},
		{
			name:              "nil manifestVersionID",
			sagaID:            uuid.New(),
			manifestVersionID: uuid.Nil,
			watchkeeperID:     uuid.New(),
			approvalToken:     "tok-fail-fast",
		},
		{
			name:              "nil watchkeeperID",
			sagaID:            uuid.New(),
			manifestVersionID: uuid.New(),
			watchkeeperID:     uuid.Nil,
			approvalToken:     "tok-fail-fast",
		},
		{
			// M7.3.a: empty approvalToken would silently bypass the
			// idempotency dedup at the DAO layer
			// (saga.ErrEmptyIdempotencyKey). The kickoffer pins the
			// non-empty contract upstream so a future caller cannot
			// supply "" and re-introduce the double-create regression
			// the M7.3.a column was introduced to prevent.
			name:              "empty approvalToken",
			sagaID:            uuid.New(),
			manifestVersionID: uuid.New(),
			watchkeeperID:     uuid.New(),
			approvalToken:     "",
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

			err := k.Kickoff(context.Background(), tc.sagaID, tc.manifestVersionID, tc.watchkeeperID, testSpawnClaim(), tc.approvalToken)
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
			if len(rec.insertIfAbsentSeq) != 0 {
				t.Errorf("InsertIfAbsent calls = %d, want 0 (fail-fast must precede persistence)", len(rec.insertIfAbsentSeq))
			}
			if len(rec.getSeq) != 0 {
				t.Errorf("Get calls = %d, want 0 (Run must not fire on fail-fast)", len(rec.getSeq))
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────
// M7.3.a — idempotency replay flow.
// ────────────────────────────────────────────────────────────────────────

// TestSpawnKickoff_ReplayedCall_EmitsReplayedEvent_NoSecondRun pins
// the M7.3.a replay path: a second Kickoff with the same approval_token
// emits exactly one `manifest_approval_replayed_for_spawn` row,
// short-circuits before [saga.Runner.Run] (no second Get on the DAO),
// returns nil, and does NOT re-emit `manifest_approved_for_spawn`. The
// underlying saga row's id is stable across the two Kickoff calls
// (the supplied second-call sagaID is discarded by the partial UNIQUE
// index, so the first-call sagaID wins).
func TestSpawnKickoff_ReplayedCall_EmitsReplayedEvent_NoSecondRun(t *testing.T) {
	t.Parallel()

	rec := newRecordingDAO()
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: rec, Logger: writer})
	k := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     rec,
		Runner:  runner,
		AgentID: "bot-watchmaster",
	})

	firstSagaID := uuid.New()
	secondSagaID := uuid.New() // discarded by the dedup index
	mvID := uuid.New()
	//nolint:gosec // G101: synthetic test idempotency-key constant, not a real credential.
	const token = "tok-replay-dedup"

	if err := kickoffWithDefaults(context.Background(), k, firstSagaID, mvID, token); err != nil {
		t.Fatalf("first Kickoff: %v", err)
	}
	// Second Kickoff with the SAME approvalToken simulates a Slack
	// retry / double-click: the dispatcher mints a fresh sagaID +
	// watchkeeperID via uuid.New() but supplies the SAME token. The
	// kickoffer routes through InsertIfAbsent and surfaces the replay.
	if err := kickoffWithDefaults(context.Background(), k, secondSagaID, mvID, token); err != nil {
		t.Fatalf("second Kickoff (replay): %v", err)
	}

	wantEvents := []string{
		spawn.EventTypeManifestApprovedForSpawn,         // 1st kickoff insert
		saga.EventTypeSagaCompleted,                     // 1st kickoff zero-step run completes
		spawn.EventTypeManifestApprovalReplayedForSpawn, // 2nd kickoff replay
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.insertIfAbsentSeq) != 2 {
		t.Errorf("InsertIfAbsent calls = %d, want 2 (one per Kickoff)", len(rec.insertIfAbsentSeq))
	}
	if !equalStrings(rec.insertIfAbsentKeys, []string{token, token}) {
		t.Errorf("InsertIfAbsent idempotency_keys = %v, want both %q", rec.insertIfAbsentKeys, token)
	}
	if len(rec.getSeq) != 1 {
		t.Errorf("Get calls = %d, want 1 (Runner.Run fired only on 1st Kickoff)", len(rec.getSeq))
	}
}

// TestSpawnKickoff_ReplayedCall_PayloadShape pins the M7.3.a
// `manifest_approval_replayed_for_spawn` payload's closed-set keys:
// saga_id, manifest_version_id, approval_token_prefix, agent_id,
// previous_status. NEVER carries the full token, the original step's
// params, or any error string.
func TestSpawnKickoff_ReplayedCall_PayloadShape(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	k := newKickoffer(t, dao, keep, "bot-watchmaster")

	firstSagaID := uuid.New()
	mvID := uuid.New()
	token := strings.Repeat("XX", 16) // 32-char canary

	if err := kickoffWithDefaults(context.Background(), k, firstSagaID, mvID, token); err != nil {
		t.Fatalf("first Kickoff: %v", err)
	}
	if err := kickoffWithDefaults(context.Background(), k, uuid.New(), mvID, token); err != nil {
		t.Fatalf("second Kickoff (replay): %v", err)
	}

	replayRow := findEventRow(t, keep, spawn.EventTypeManifestApprovalReplayedForSpawn)
	data := decodePayloadData(t, replayRow.Payload)

	assertReplayedPayloadKeys(t, data, []string{
		"saga_id", "manifest_version_id", "watchkeeper_id",
		"approval_token_prefix", "agent_id", "previous_status",
	})
	assertReplayedPayloadPII(t, replayRow.Payload, token)

	if got := data["previous_status"]; got != string(saga.SagaStateCompleted) {
		t.Errorf("previous_status = %v, want %q", got, saga.SagaStateCompleted)
	}
	if got := data["saga_id"]; got != firstSagaID.String() {
		t.Errorf("saga_id = %v, want first-call %q", got, firstSagaID)
	}
}

// findEventRow returns the first recorded row whose EventType matches
// `wantType`. Hoisted from the M7.3.a payload-shape tests so the
// per-test cyclomatic complexity stays under the project's 15-cap.
func findEventRow(t *testing.T, keep *fakeLocalKeepClient, wantType string) *keepclient.LogAppendRequest {
	t.Helper()
	rows := keep.recorded()
	for i := range rows {
		if rows[i].EventType == wantType {
			return &rows[i]
		}
	}
	t.Fatalf("no %q row in %v", wantType, keep.eventTypes())
	return nil
}

// decodePayloadData JSON-decodes the keeperslog envelope's `data`
// sub-object. Hoisted so the M7.3.a payload-shape tests share the
// envelope-unwrap discipline without inflating gocyclo.
func decodePayloadData(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		t.Fatalf("data not JSON: %v", err)
	}
	return data
}

// assertReplayedPayloadKeys pins the closed-set replay-payload key
// vocabulary.
func assertReplayedPayloadKeys(t *testing.T, data map[string]any, want []string) {
	t.Helper()
	allowed := make(map[string]bool, len(want))
	for _, k := range want {
		allowed[k] = true
	}
	if len(data) != len(allowed) {
		t.Errorf("replayed payload key count = %d, want %d (keys=%v)", len(data), len(allowed), data)
	}
	for k := range data {
		if !allowed[k] {
			t.Errorf("replayed payload contains forbidden key %q", k)
		}
	}
	for k := range allowed {
		if _, ok := data[k]; !ok {
			t.Errorf("replayed payload missing required key %q", k)
		}
	}
}

// assertReplayedPayloadPII pins the M2b.7 PII discipline on the replay
// row: full approval_token absent, no `approval_token` key, no `error`
// substring.
func assertReplayedPayloadPII(t *testing.T, raw []byte, fullToken string) {
	t.Helper()
	s := string(raw)
	if strings.Contains(s, fullToken) {
		t.Errorf("replayed payload leaked full approval_token: %s", s)
	}
	if strings.Contains(s, `"approval_token"`) {
		t.Errorf("replayed payload contains forbidden `approval_token` key: %s", s)
	}
	if strings.Contains(s, `"error"`) {
		t.Errorf("replayed payload contains forbidden `error` key: %s", s)
	}
}

// TestSpawnKickoff_ReplayedPayload_UsesExistingIDs pins codex iter-1
// Major: the replayed-event payload sources `manifest_version_id` and
// `watchkeeper_id` from the existing saga row, NOT the second-call's
// discarded args. A retried approval that supplied a different
// manifestVersionID or watchkeeperID would otherwise produce a
// self-contradictory row.
func TestSpawnKickoff_ReplayedPayload_UsesExistingIDs(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	k := newKickoffer(t, dao, keep, "bot-watchmaster")

	firstSagaID := uuid.New()
	firstMvID := uuid.New()
	firstWkID := uuid.New()
	const token = "tok-existing-ids"

	// First call seeds the row with the FIRST-call's ids.
	if err := k.Kickoff(context.Background(), firstSagaID, firstMvID, firstWkID, testSpawnClaim(), token); err != nil {
		t.Fatalf("first Kickoff: %v", err)
	}
	// Second call (replay) supplies DIFFERENT ids; the kickoffer
	// MUST emit the FIRST-call's ids on the replay row, not the
	// second-call's discarded values.
	secondSagaID := uuid.New()
	secondMvID := uuid.New()
	secondWkID := uuid.New()
	if err := k.Kickoff(context.Background(), secondSagaID, secondMvID, secondWkID, testSpawnClaim(), token); err != nil {
		t.Fatalf("second Kickoff (replay): %v", err)
	}

	rows := keep.recorded()
	var replayRow *keepclient.LogAppendRequest
	for i := range rows {
		if rows[i].EventType == spawn.EventTypeManifestApprovalReplayedForSpawn {
			replayRow = &rows[i]
			break
		}
	}
	if replayRow == nil {
		t.Fatalf("no replay row in %v", keep.eventTypes())
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(replayRow.Payload, &envelope); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	var data map[string]any
	if err := json.Unmarshal(envelope["data"], &data); err != nil {
		t.Fatalf("data not JSON: %v", err)
	}

	if got := data["saga_id"]; got != firstSagaID.String() {
		t.Errorf("saga_id = %v, want FIRST-call %q (not second-call %q)", got, firstSagaID, secondSagaID)
	}
	if got := data["manifest_version_id"]; got != firstMvID.String() {
		t.Errorf("manifest_version_id = %v, want FIRST-call %q (not second-call %q)", got, firstMvID, secondMvID)
	}
	if got := data["watchkeeper_id"]; got != firstWkID.String() {
		t.Errorf("watchkeeper_id = %v, want FIRST-call %q (not second-call %q)", got, firstWkID, secondWkID)
	}
}

// TestSpawnKickoff_PendingStatus_CatchUpResumesSaga pins codex iter-1
// Critical: when the original Kickoff's audit-append failed AFTER the
// InsertIfAbsent succeeded, the saga is left in `pending` status. A
// retry call MUST detect the pending state and resume — emit the
// missed `manifest_approved_for_spawn`, seed SpawnContext from the
// persisted row, and call saga.Runner.Run for the existing sagaID.
// Without the catch-up branch the row would be stuck forever (the
// approval_token cannot be re-resolved through the upstream
// pending_approvals state machine).
func TestSpawnKickoff_PendingStatus_CatchUpResumesSaga(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}

	// First Kickoff: pre-seed a `pending` saga DIRECTLY via the DAO so
	// the test has full control over the partial state. This mirrors
	// the production failure mode: InsertIfAbsent succeeded but the
	// keeperslog Append failed before saga.Runner.Run could run.
	firstSagaID := uuid.New()
	firstMvID := uuid.New()
	firstWkID := uuid.New()
	const token = "tok-catch-up"
	if _, err := dao.InsertIfAbsent(context.Background(), firstSagaID, firstMvID, firstWkID, token); err != nil {
		t.Fatalf("seed InsertIfAbsent: %v", err)
	}
	// Sanity — the seed row is in `pending`.
	if got, _ := dao.Get(context.Background(), firstSagaID); got.Status != saga.SagaStatePending {
		t.Fatalf("seeded saga status = %q, want %q", got.Status, saga.SagaStatePending)
	}

	// Second Kickoff: the operator retry. Catch-up branch should
	// emit manifest_approved_for_spawn AND run the saga (zero-step
	// saga completes immediately, so saga_completed follows).
	k := newKickoffer(t, dao, keep, "bot-watchmaster")
	if err := kickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), token); err != nil {
		t.Fatalf("retry Kickoff (catch-up): %v", err)
	}

	wantEvents := []string{
		spawn.EventTypeManifestApprovedForSpawn,
		saga.EventTypeSagaCompleted,
	}
	if got := keep.eventTypes(); !equalStrings(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v (catch-up emits original audit chain)", got, wantEvents)
	}

	// The persisted saga reaches `completed`; the catch-up uses the
	// FIRST-call's sagaID (the second-call's id is discarded).
	got, err := dao.Get(context.Background(), firstSagaID)
	if err != nil {
		t.Fatalf("Get firstSagaID: %v", err)
	}
	if got.Status != saga.SagaStateCompleted {
		t.Errorf("Status = %q, want %q (catch-up resumes Run)", got.Status, saga.SagaStateCompleted)
	}
	if got.WatchkeeperID != firstWkID {
		t.Errorf("Persisted WatchkeeperID = %v, want first-call %v (catch-up keeps original)",
			got.WatchkeeperID, firstWkID)
	}
}

// TestEventTypeManifestApprovalReplayedForSpawn_NoPrefixCollision pins
// the closed-set vocabulary discipline for the M7.3.a replay event.
// Mirrors TestEventTypeManifestApprovedForSpawn_NoPrefixCollision.
func TestEventTypeManifestApprovalReplayedForSpawn_NoPrefixCollision(t *testing.T) {
	t.Parallel()

	et := spawn.EventTypeManifestApprovalReplayedForSpawn
	const want = "manifest_approval_replayed_for_spawn"
	if et != want {
		t.Errorf("event_type = %q, want exact %q", et, want)
	}
	if strings.HasPrefix(et, "llm_turn_cost") {
		t.Errorf("event_type %q has forbidden llm_turn_cost prefix (M6.3.e)", et)
	}
	if strings.HasPrefix(et, "saga_") {
		t.Errorf("event_type %q has forbidden saga_ prefix (M7.1.a)", et)
	}
	if et == spawn.EventTypeManifestApprovedForSpawn {
		t.Errorf("event_type %q must differ from EventTypeManifestApprovedForSpawn", et)
	}
}

// TestSpawnKickoff_ConcurrentReplays_ExactlyOneRun hammers the
// idempotency contract under contention: 16 goroutines call Kickoff
// concurrently with the SAME approvalToken. The test uses a
// blocking saga step so the first goroutine's [saga.Runner.Run]
// transitions the saga out of `pending` (via UpdateStep →
// SagaStateInFlight) BEFORE releasing the other 15 goroutines —
// once Status != pending, the M7.3.a-iter-1 catch-up branch falls
// back to the true-replay branch and the original "exactly one
// approved + 15 replays" semantic holds deterministically.
//
// M7.3.b deflake note: the prior version of this test used
// [kickoffWithDefaults] (zero-step saga); zero-step Run transitions
// pending → completed under MarkCompleted's wlock, leaving NO
// `in_flight` window other goroutines could observe — so the
// catch-up branch fired non-deterministically depending on goroutine
// scheduling. Replacing the zero-step saga with a one-blocking-step
// saga + barrier moves the assertion from race-window-dependent to
// state-machine-deterministic; the failing-step + concurrent-rollback
// invariant is covered by the M7.3.b
// `TestRunner_Concurrency_DistinctSagas_RollbackInIsolation` saga-
// core test, so this test stays scoped to the kickoffer's idempotency
// contract.
func TestSpawnKickoff_ConcurrentReplays_ExactlyOneRun(t *testing.T) {
	t.Parallel()

	rec := newRecordingDAO()
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: rec, Logger: writer})

	// Blocking step: closes `entered` once UpdateStep has flipped
	// Status to `in_flight` and Runner has dispatched Execute;
	// blocks on `release` until the other 15 goroutines land
	// their InsertIfAbsent calls.
	step := &blockingStepImpl{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	k := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     rec,
		Runner:  runner,
		AgentID: "bot",
		Steps:   []saga.Step{step},
	})

	const writers = 16
	const token = "tok-concurrent"
	mvID := uuid.New()

	var wg sync.WaitGroup
	// Goroutine #1 is the saga winner: it acquires InsertIfAbsent
	// first, runs the saga, hits the blocking step, signals
	// `entered`, then waits on `release` to complete.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := kickoffWithDefaults(context.Background(), k, uuid.New(), mvID, token); err != nil {
			t.Errorf("winner Kickoff: %v", err)
		}
	}()

	// Wait for the winner to flip Status → in_flight via UpdateStep
	// + reach the blocking step.
	<-step.entered

	// Now spawn the 15 replays — every one of them races
	// InsertIfAbsent against the in_flight saga; the M7.3.a
	// `Status != pending` branch routes them through the
	// true-replay path (emit replayed + return).
	for i := 0; i < writers-1; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := kickoffWithDefaults(context.Background(), k, uuid.New(), mvID, token); err != nil {
				t.Errorf("replay Kickoff: %v", err)
			}
		}()
	}
	// Wait for all 15 replays to finish before releasing the winner.
	// Goroutine #1 holds the blocking step open, so the other 15
	// observe Status=in_flight regardless of scheduling order. The
	// 5s watchdog converts a regression that deadlocks the catch-up
	// branch (e.g. an InsertIfAbsent that never returns) from a
	// hung test into a fast failure with a clear diagnostic.
	deadline := time.After(5 * time.Second)
poll:
	for {
		rec.mu.Lock()
		seen := len(rec.insertIfAbsentSeq)
		rec.mu.Unlock()
		if seen >= writers {
			break poll
		}
		select {
		case <-deadline:
			rec.mu.Lock()
			seenNow := len(rec.insertIfAbsentSeq)
			rec.mu.Unlock()
			t.Fatalf("timeout waiting for %d concurrent InsertIfAbsent calls; seen=%d (regression candidate: a goroutine deadlocked in the catch-up branch)",
				writers, seenNow)
		case <-time.After(time.Millisecond):
		}
	}
	close(step.release)

	wg.Wait()

	approvedCount := 0
	replayedCount := 0
	for _, et := range keep.eventTypes() {
		switch et {
		case spawn.EventTypeManifestApprovedForSpawn:
			approvedCount++
		case spawn.EventTypeManifestApprovalReplayedForSpawn:
			replayedCount++
		}
	}
	if approvedCount != 1 {
		t.Errorf("manifest_approved_for_spawn rows = %d, want 1 (the winner of the InsertIfAbsent race)", approvedCount)
	}
	if replayedCount != writers-1 {
		t.Errorf("manifest_approval_replayed_for_spawn rows = %d, want %d", replayedCount, writers-1)
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.insertIfAbsentSeq) != writers {
		t.Errorf("InsertIfAbsent calls = %d, want %d (one per Kickoff)", len(rec.insertIfAbsentSeq), writers)
	}
	if len(rec.getSeq) != 1 {
		t.Errorf("Get calls = %d, want 1 (Runner.Run fires exactly once)", len(rec.getSeq))
	}
}

// findRejectionPayload scans the recorded keepers_log requests for
// the [spawn.EventTypeManifestRejectedAfterSpawnFailure] row and
// returns its decoded `data` envelope. t.Fatalf on any decode
// failure or missing row — pulled out of the inline test body to
// keep the parent test's cyclomatic complexity below the
// project's gocyclo threshold.
func findRejectionPayload(t *testing.T, rows []keepclient.LogAppendRequest) map[string]any {
	t.Helper()
	for _, row := range rows {
		if row.EventType != spawn.EventTypeManifestRejectedAfterSpawnFailure {
			continue
		}
		var envelope map[string]any
		if err := json.Unmarshal(row.Payload, &envelope); err != nil {
			t.Fatalf("rejection row payload not JSON: %v", err)
		}
		dataAny, ok := envelope["data"]
		if !ok {
			t.Fatalf("rejection row missing data envelope")
		}
		data, ok := dataAny.(map[string]any)
		if !ok {
			t.Fatalf("rejection row data not a map; got %T", dataAny)
		}
		return data
	}
	t.Fatalf("no manifest_rejected_after_spawn_failure row recorded")
	return nil
}

// blockingStepImpl is the concrete [saga.Step] used by the M7.3.b
// deflaked concurrency test. Lives at file scope so the saga.Step
// interface satisfaction is type-checked once at compile time
// rather than via a closure-capturing inline type literal.
//
// `entered` closes once on the first Execute (the winner saga); the
// remaining concurrent Kickoff calls go through the kickoffer's
// replay branch (Status != pending) WITHOUT reaching this step, so
// `entered` is closed exactly once by definition.
type blockingStepImpl struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blockingStepImpl) Name() string { return "blocking" }

func (b *blockingStepImpl) Execute(_ context.Context) error {
	close(b.entered)
	<-b.release
	return nil
}

// ────────────────────────────────────────────────────────────────────────
// M7.3.b: manifest_rejected_after_spawn_failure emit on saga rollback
// ────────────────────────────────────────────────────────────────────────

// selectiveFailLocalKeepClient is a [keeperslog.LocalKeepClient]
// stand-in that records every LogAppend AND fails ONLY on calls
// whose `event_type` matches the configured target. Used to drive
// the M7.3.b "rejection-emit best-effort" path: a transient
// keeperslog outage that drops the rejection row MUST NOT shadow
// the original Run error.
type selectiveFailLocalKeepClient struct {
	mu             sync.Mutex
	calls          []keepclient.LogAppendRequest
	failOnEventTyp string
	failErr        error
}

func (f *selectiveFailLocalKeepClient) LogAppend(_ context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	if req.EventType == f.failOnEventTyp {
		return nil, f.failErr
	}
	return &keepclient.LogAppendResponse{ID: "fake-row"}, nil
}

func (f *selectiveFailLocalKeepClient) recordedTypes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.EventType
	}
	return out
}

// TestSpawnKickoff_StepFails_EmitsRejectionAfterSagaCompensated pins
// the M7.3.b ordering: the saga's own `saga_failed` + `saga_compensated`
// rows precede the kickoffer's `manifest_rejected_after_spawn_failure`
// row. A downstream consumer that watches the audit chain and joins
// the rejection row to the saga's failure rows by `saga_id` MUST
// see the saga's wrap-up rows first.
func TestSpawnKickoff_StepFails_EmitsRejectionAfterSagaCompensated(t *testing.T) {
	t.Parallel()

	stepErr := errors.New("step blew up")
	failing := &errSagaStep{name: "explode", err: stepErr}

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot-7-3-b",
		Steps:   []saga.Step{failing},
	})

	mvID := uuid.New()
	wkID := uuid.New()
	err := k.Kickoff(context.Background(), uuid.New(), mvID, wkID, testSpawnClaim(), "tok-rej-1")
	if err == nil {
		t.Fatal("Kickoff returned nil, want wrapped step error")
	}
	if !errors.Is(err, stepErr) {
		t.Errorf("errors.Is(err, stepErr) = false; got %v", err)
	}

	gotTypes := keep.eventTypes()
	wantTypes := []string{
		spawn.EventTypeManifestApprovedForSpawn,
		saga.EventTypeSagaStepStarted,
		saga.EventTypeSagaFailed,
		saga.EventTypeSagaCompensated,
		spawn.EventTypeManifestRejectedAfterSpawnFailure,
	}
	if !equalStrings(gotTypes, wantTypes) {
		t.Fatalf("event_type chain = %v, want %v", gotTypes, wantTypes)
	}

	// Pin the rejection-row payload shape: closed-set keys only,
	// the watchkeeper id matches the kickoffer's input (no
	// duplicate watchkeeper minted by the runner), the agent_id is
	// the kickoffer's bot id (NOT the watchkeeper-being-spawned id),
	// the approval_token is rendered as the tok-<6-prefix>.
	rejectionPayload := findRejectionPayload(t, keep.recorded())

	allowed := map[string]bool{
		"saga_id":               true,
		"manifest_version_id":   true,
		"watchkeeper_id":        true,
		"agent_id":              true,
		"approval_token_prefix": true,
	}
	for k := range rejectionPayload {
		if !allowed[k] {
			t.Errorf("rejection payload contains forbidden key %q (allowed: %v)", k, allowed)
		}
	}
	if got := rejectionPayload["manifest_version_id"]; got != mvID.String() {
		t.Errorf("payload.manifest_version_id = %v, want %v", got, mvID.String())
	}
	if got := rejectionPayload["watchkeeper_id"]; got != wkID.String() {
		t.Errorf("payload.watchkeeper_id = %v, want %v", got, wkID.String())
	}
	if got := rejectionPayload["agent_id"]; got != "bot-7-3-b" {
		t.Errorf("payload.agent_id = %v, want bot-7-3-b", got)
	}
	if got, _ := rejectionPayload["approval_token_prefix"].(string); got != "tok-tok-re" {
		// approvalTokenPrefix prepends "tok-" + first 6 runes of
		// the input token; "tok-rej-1" → "tok-rej-1"[:6] = "tok-re"
		// → emit "tok-tok-re".
		t.Errorf("payload.approval_token_prefix = %q, want %q", got, "tok-tok-re")
	}
}

// TestSpawnKickoff_HappyPath_NoRejectionEmit pins the negative AC: the
// kickoffer MUST NOT emit `manifest_rejected_after_spawn_failure` on a
// successful saga run. Without this guard a regression that always
// emits the row (e.g. moves the emit out of the failure branch) would
// pollute the audit chain with phantom rejections.
func TestSpawnKickoff_HappyPath_NoRejectionEmit(t *testing.T) {
	t.Parallel()

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &fakeLocalKeepClient{}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot-happy",
	})

	if err := kickoffWithDefaults(context.Background(), k, uuid.New(), uuid.New(), "tok-happy"); err != nil {
		t.Fatalf("Kickoff: %v", err)
	}

	for _, et := range keep.eventTypes() {
		if et == spawn.EventTypeManifestRejectedAfterSpawnFailure {
			t.Errorf("happy path emitted forbidden rejection event %q", et)
		}
	}
}

// TestSpawnKickoff_RejectionAppendFails_OriginalRunErrorPreserved
// pins the M7.3.b best-effort rejection-emit contract: a transient
// keeperslog outage that drops the rejection row MUST NOT shadow the
// original step error. The operator's wrap-chain answer to "what
// failed?" stays the saga's step error, regardless of whether the
// rejection-row append succeeded.
func TestSpawnKickoff_RejectionAppendFails_OriginalRunErrorPreserved(t *testing.T) {
	t.Parallel()

	stepErr := errors.New("step blew up")
	failing := &errSagaStep{name: "explode", err: stepErr}
	appendErr := errors.New("keeperslog offline")

	dao := saga.NewMemorySpawnSagaDAO(nil)
	keep := &selectiveFailLocalKeepClient{
		failOnEventTyp: spawn.EventTypeManifestRejectedAfterSpawnFailure,
		failErr:        appendErr,
	}
	writer := keeperslog.New(keep)
	runner := saga.NewRunner(saga.Dependencies{DAO: dao, Logger: writer})
	k := spawn.NewSpawnKickoffer(spawn.SpawnKickoffDeps{
		Logger:  writer,
		DAO:     dao,
		Runner:  runner,
		AgentID: "bot-best-effort",
		Steps:   []saga.Step{failing},
	})

	err := k.Kickoff(context.Background(), uuid.New(), uuid.New(), uuid.New(), testSpawnClaim(), "tok-best")
	if err == nil {
		t.Fatal("Kickoff returned nil, want wrapped step error")
	}
	if !errors.Is(err, stepErr) {
		t.Errorf("errors.Is(err, stepErr) = false; rejection-append failure must NOT shadow the original Run error (got %v)", err)
	}
	if errors.Is(err, appendErr) {
		t.Errorf("errors.Is(err, appendErr) = true; rejection-append error must be silently swallowed (got %v)", err)
	}

	// Defense-in-depth: even though the append errored, the
	// kickoffer MUST have ATTEMPTED the call. Pin the attempt by
	// checking the recorded list (the fake records every call,
	// regardless of return value).
	gotRejection := false
	for _, et := range keep.recordedTypes() {
		if et == spawn.EventTypeManifestRejectedAfterSpawnFailure {
			gotRejection = true
			break
		}
	}
	if !gotRejection {
		t.Errorf("kickoffer skipped the rejection-emit attempt; best-effort emit MUST still call Append")
	}
}

// TestSpawnKickoffSourceCode_NoManifestDAOWrite is the M7.3.b
// scope-defending source-grep AC: the rejection-emit MUST stay
// audit-only. A regression that adds a Manifest-DAO Update / Reject
// call from spawnkickoff.go would extend the kickoffer's blast
// radius into the M2 manifest-storage seam without an explicit
// dependency, which the file's lean-import shape is designed to
// reject. The future M7.4-or-equivalent reconciler that consumes
// the rejection audit row owns the Manifest mutation.
func TestSpawnKickoffSourceCode_NoManifestDAOWrite(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile("spawnkickoff.go")
	if err != nil {
		t.Fatalf("read spawnkickoff.go: %v", err)
	}
	src := string(body)

	// Scan only NON-comment lines so the doc-block narrative
	// (which legitimately mentions the future Manifest mutation)
	// is not flagged. The forbidden substrings target the
	// manifest-DAO surfaces a regression would touch — `Manifest`
	// + a DAO verb (`Reject`, `Update`, `MarkRejected`).
	forbiddenSubstrings := []string{
		"ManifestDAO",
		"manifestDAO",
		"MarkManifestRejected",
		"RejectManifest",
		"core/pkg/manifest",
		// Defense-in-depth against a regression that lands a
		// Manifest mutation under a non-flagged identifier — e.g.
		// a typed UpdateManifestStatus method on a satellite DAO,
		// a generic Update that takes a Manifest pointer, or a
		// manifest_status field-write through a DB query helper.
		"manifest_status",
		"Manifest.Status",
		"UpdateManifest",
		"manifest_versions", // any direct DB-table reference is
		// out of scope for the audit-only kickoffer.
	}
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		for _, forbidden := range forbiddenSubstrings {
			if strings.Contains(line, forbidden) {
				t.Errorf("spawnkickoff.go contains forbidden substring %q (M7.3.b: rejection-emit MUST stay audit-only): %s",
					forbidden, line)
			}
		}
	}
}

// TestEventTypeManifestRejectedAfterSpawnFailure_PrefixCollisionFree
// pins the M7.3.b rejection event_type against the M6.3.e prefix
// vocabulary discipline: the event MUST use the `manifest_` prefix
// (mirroring `manifest_approved_for_spawn` /
// `manifest_approval_replayed_for_spawn`) and MUST NOT collide with
// the `llm_turn_cost_*` family or the `saga_*` family. A future
// rename that drops the prefix would feed bogus rows to the wrong
// audit-chain consumer.
func TestEventTypeManifestRejectedAfterSpawnFailure_PrefixCollisionFree(t *testing.T) {
	t.Parallel()

	et := spawn.EventTypeManifestRejectedAfterSpawnFailure
	if !strings.HasPrefix(et, "manifest_") {
		t.Errorf("event_type %q missing manifest_ prefix (vocabulary discipline)", et)
	}
	if strings.HasPrefix(et, "llm_turn_cost") {
		t.Errorf("event_type %q has forbidden llm_turn_cost prefix (M6.3.e)", et)
	}
	if strings.HasPrefix(et, "saga_") {
		t.Errorf("event_type %q has forbidden saga_ prefix (M7.1.a vocabulary)", et)
	}
	if strings.HasPrefix(et, "retire_") {
		t.Errorf("event_type %q has forbidden retire_ prefix (M7.2.a vocabulary)", et)
	}
	// Distinct from the existing manifest_* events to avoid
	// payload-shape ambiguity.
	if et == spawn.EventTypeManifestApprovedForSpawn {
		t.Errorf("event_type collides with EventTypeManifestApprovedForSpawn")
	}
	if et == spawn.EventTypeManifestApprovalReplayedForSpawn {
		t.Errorf("event_type collides with EventTypeManifestApprovalReplayedForSpawn")
	}
}
