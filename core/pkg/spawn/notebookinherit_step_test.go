package spawn_test

// notebookinherit_step_test.go pins the Phase 2 §M7.1.c
// NotebookInheritStep behaviour:
//
//   - happy path (predecessor exists) → Inherit dispatched +
//     `notebook_inherited` audit row emitted with the closed-set
//     payload.
//   - opt-out (`sc.NoInherit == true`) → no seam touched, no audit.
//   - no-predecessor (`keepclient.ErrNoPredecessor`) → no seam touched
//     after the lookup, no audit.
//   - empty role / empty org → no seam touched, no audit (defensive
//     extensions of the no-predecessor shape).
//   - lookup error (non-404) → wrapped error, no Inherit call, no
//     audit.
//   - inherit error → wrapped error, no audit emit.
//   - audit emit error → wrapped error (the data is already in the
//     DB; the saga's downstream provisioner's compensator owns the
//     archive-not-delete rollback of the file).
//   - construction panics on nil deps.
//   - missing SpawnContext / nil AgentID → wrapped sentinel, no seam
//     touched.
//   - cancelled ctx → wrapped ctx.Err(), no seam touched.
//   - fault injection — the inheritor seam may fail at any step
//     (fetch / open / import / count); the wrap chain preserves
//     errors.Is matchability for [keepclient.ErrNoPredecessor].
//   - compensator chain unchanged — NotebookInheritStep does NOT
//     implement saga.Compensator (the inherited file is owned by
//     NotebookProvisionStep's compensator); the chain skips silently.

import (
	"context"
	"errors"
	"fmt"
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
// Hand-rolled fakes (M3.6 / M6.3.e / M7.1.c.a-.c pattern — no mocking lib).
// ────────────────────────────────────────────────────────────────────────

type recordedLatestRetiredCall struct {
	ctx            context.Context
	organizationID string
	roleID         string
}

type fakePredecessorLookup struct {
	mu        sync.Mutex
	calls     []recordedLatestRetiredCall
	callCount atomic.Int32
	returnWk  *keepclient.Watchkeeper
	returnErr error
}

func (f *fakePredecessorLookup) LatestRetiredByRole(ctx context.Context, organizationID, roleID string) (*keepclient.Watchkeeper, error) {
	f.callCount.Add(1)
	f.mu.Lock()
	f.calls = append(f.calls, recordedLatestRetiredCall{ctx: ctx, organizationID: organizationID, roleID: roleID})
	f.mu.Unlock()
	return f.returnWk, f.returnErr
}

func (f *fakePredecessorLookup) recordedCalls() []recordedLatestRetiredCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedLatestRetiredCall, len(f.calls))
	copy(out, f.calls)
	return out
}

type recordedInheritCall struct {
	ctx           context.Context
	watchkeeperID uuid.UUID
	archiveURI    string
}

type fakeNotebookInheritor struct {
	mu          sync.Mutex
	calls       []recordedInheritCall
	callCount   atomic.Int32
	returnCount int
	returnErr   error
}

func (f *fakeNotebookInheritor) Inherit(ctx context.Context, watchkeeperID uuid.UUID, archiveURI string) (int, error) {
	f.callCount.Add(1)
	f.mu.Lock()
	f.calls = append(f.calls, recordedInheritCall{ctx: ctx, watchkeeperID: watchkeeperID, archiveURI: archiveURI})
	f.mu.Unlock()
	return f.returnCount, f.returnErr
}

func (f *fakeNotebookInheritor) recordedCalls() []recordedInheritCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedInheritCall, len(f.calls))
	copy(out, f.calls)
	return out
}

type recordedAuditAppend struct {
	ctx   context.Context
	event keeperslog.Event
}

type fakeInheritAuditAppender struct {
	mu        sync.Mutex
	calls     []recordedAuditAppend
	callCount atomic.Int32
	returnID  string
	returnErr error
}

func (f *fakeInheritAuditAppender) Append(ctx context.Context, event keeperslog.Event) (string, error) {
	f.callCount.Add(1)
	f.mu.Lock()
	f.calls = append(f.calls, recordedAuditAppend{ctx: ctx, event: event})
	f.mu.Unlock()
	return f.returnID, f.returnErr
}

func (f *fakeInheritAuditAppender) recordedCalls() []recordedAuditAppend {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedAuditAppend, len(f.calls))
	copy(out, f.calls)
	return out
}

// ────────────────────────────────────────────────────────────────────────
// Test fixtures.
// ────────────────────────────────────────────────────────────────────────

const (
	inheritTestOrgID     = "org-test"
	inheritTestRoleID    = "frontline-watchkeeper"
	inheritArchiveURI    = "s3://watchkeepers-archive/agent-pred-001.sqlite"
	inheritPredecessorID = "pred-001"
)

func newInheritSpawnContext(t *testing.T, watchkeeperID uuid.UUID, opts ...func(*saga.SpawnContext)) saga.SpawnContext {
	t.Helper()
	sc := saga.SpawnContext{
		ManifestVersionID: uuid.New(),
		AgentID:           watchkeeperID,
		Claim: saga.SpawnClaim{
			OrganizationID:  inheritTestOrgID,
			AgentID:         "agent-watchmaster",
			AuthorityMatrix: map[string]string{},
		},
		RoleID:    inheritTestRoleID,
		NoInherit: false,
	}
	for _, opt := range opts {
		opt(&sc)
	}
	return sc
}

func archiveURIPtr(s string) *string { return &s }

func newPredecessorWatchkeeper() *keepclient.Watchkeeper {
	role := inheritTestRoleID
	return &keepclient.Watchkeeper{
		ID:         inheritPredecessorID,
		Status:     "retired",
		ArchiveURI: archiveURIPtr(inheritArchiveURI),
		RoleID:     &role,
	}
}

func newInheritStep(
	t *testing.T,
	predecessor spawn.PredecessorLookup,
	inheritor spawn.NotebookInheritor,
	audit spawn.InheritAuditAppender,
) *spawn.NotebookInheritStep {
	t.Helper()
	return spawn.NewNotebookInheritStep(spawn.NotebookInheritStepDeps{
		Predecessor:   predecessor,
		Inheritor:     inheritor,
		AuditAppender: audit,
	})
}

// ────────────────────────────────────────────────────────────────────────
// Construction tests.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookInheritStep_Name_ReturnsStableIdentifier(t *testing.T) {
	t.Parallel()

	step := newInheritStep(t, &fakePredecessorLookup{}, &fakeNotebookInheritor{}, &fakeInheritAuditAppender{})

	if got := step.Name(); got != spawn.NotebookInheritStepName {
		t.Errorf("Name() = %q, want %q", got, spawn.NotebookInheritStepName)
	}
	if spawn.NotebookInheritStepName != "notebook_inherit" {
		t.Errorf("NotebookInheritStepName = %q, want %q (closed-set wire vocabulary)", spawn.NotebookInheritStepName, "notebook_inherit")
	}
}

func TestNewNotebookInheritStep_PanicsOnNilPredecessor(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil Predecessor")
		}
	}()
	_ = spawn.NewNotebookInheritStep(spawn.NotebookInheritStepDeps{
		Predecessor:   nil,
		Inheritor:     &fakeNotebookInheritor{},
		AuditAppender: &fakeInheritAuditAppender{},
	})
}

func TestNewNotebookInheritStep_PanicsOnNilInheritor(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil Inheritor")
		}
	}()
	_ = spawn.NewNotebookInheritStep(spawn.NotebookInheritStepDeps{
		Predecessor:   &fakePredecessorLookup{},
		Inheritor:     nil,
		AuditAppender: &fakeInheritAuditAppender{},
	})
}

func TestNewNotebookInheritStep_PanicsOnNilAuditAppender(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil AuditAppender")
		}
	}()
	_ = spawn.NewNotebookInheritStep(spawn.NotebookInheritStepDeps{
		Predecessor:   &fakePredecessorLookup{},
		Inheritor:     &fakeNotebookInheritor{},
		AuditAppender: nil,
	})
}

// ────────────────────────────────────────────────────────────────────────
// Happy path.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookInheritStep_Execute_HappyPath_DispatchesAndAudits(t *testing.T) {
	t.Parallel()

	wkID := uuid.New()
	lookup := &fakePredecessorLookup{returnWk: newPredecessorWatchkeeper()}
	inheritor := &fakeNotebookInheritor{returnCount: 42}
	appender := &fakeInheritAuditAppender{returnID: "evt-1"}
	step := newInheritStep(t, lookup, inheritor, appender)
	ctx := saga.WithSpawnContext(context.Background(), newInheritSpawnContext(t, wkID))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	lookupCalls := lookup.recordedCalls()
	if len(lookupCalls) != 1 {
		t.Fatalf("lookup calls = %d, want 1", len(lookupCalls))
	}
	if lookupCalls[0].organizationID != inheritTestOrgID {
		t.Errorf("lookup org = %q, want %q", lookupCalls[0].organizationID, inheritTestOrgID)
	}
	if lookupCalls[0].roleID != inheritTestRoleID {
		t.Errorf("lookup role = %q, want %q", lookupCalls[0].roleID, inheritTestRoleID)
	}

	inheritCalls := inheritor.recordedCalls()
	if len(inheritCalls) != 1 {
		t.Fatalf("inherit calls = %d, want 1", len(inheritCalls))
	}
	if inheritCalls[0].watchkeeperID != wkID {
		t.Errorf("inherit wkID = %v, want %v", inheritCalls[0].watchkeeperID, wkID)
	}
	if inheritCalls[0].archiveURI != inheritArchiveURI {
		t.Errorf("inherit archiveURI = %q, want %q", inheritCalls[0].archiveURI, inheritArchiveURI)
	}

	auditCalls := appender.recordedCalls()
	if len(auditCalls) != 1 {
		t.Fatalf("audit calls = %d, want 1", len(auditCalls))
	}
	if auditCalls[0].event.EventType != spawn.EventTypeNotebookInherited {
		t.Errorf("audit event_type = %q, want %q", auditCalls[0].event.EventType, spawn.EventTypeNotebookInherited)
	}

	payload, ok := auditCalls[0].event.Payload.(map[string]any)
	if !ok {
		t.Fatalf("audit payload type = %T, want map[string]any", auditCalls[0].event.Payload)
	}
	if got := payload["predecessor_watchkeeper_id"]; got != inheritPredecessorID {
		t.Errorf("payload predecessor_watchkeeper_id = %v, want %q", got, inheritPredecessorID)
	}
	if got := payload["archive_uri"]; got != inheritArchiveURI {
		t.Errorf("payload archive_uri = %v, want %q", got, inheritArchiveURI)
	}
	if got := payload["entries_imported"]; got != 42 {
		t.Errorf("payload entries_imported = %v, want 42", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// No-op short-circuits.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookInheritStep_Execute_NoInheritOptOut_NoSeamsTouched_NoAudit(t *testing.T) {
	t.Parallel()

	wkID := uuid.New()
	lookup := &fakePredecessorLookup{returnWk: newPredecessorWatchkeeper()}
	inheritor := &fakeNotebookInheritor{returnCount: 10}
	appender := &fakeInheritAuditAppender{}
	step := newInheritStep(t, lookup, inheritor, appender)
	sc := newInheritSpawnContext(t, wkID, func(sc *saga.SpawnContext) { sc.NoInherit = true })
	ctx := saga.WithSpawnContext(context.Background(), sc)

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := lookup.callCount.Load(); got != 0 {
		t.Errorf("lookup callCount = %d, want 0", got)
	}
	if got := inheritor.callCount.Load(); got != 0 {
		t.Errorf("inherit callCount = %d, want 0", got)
	}
	if got := appender.callCount.Load(); got != 0 {
		t.Errorf("audit callCount = %d, want 0 (NoInherit must NOT emit notebook_inherited)", got)
	}
}

func TestNotebookInheritStep_Execute_EmptyRoleID_NoSeamsTouched_NoAudit(t *testing.T) {
	t.Parallel()

	wkID := uuid.New()
	lookup := &fakePredecessorLookup{}
	inheritor := &fakeNotebookInheritor{}
	appender := &fakeInheritAuditAppender{}
	step := newInheritStep(t, lookup, inheritor, appender)
	sc := newInheritSpawnContext(t, wkID, func(sc *saga.SpawnContext) { sc.RoleID = "" })
	ctx := saga.WithSpawnContext(context.Background(), sc)

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := lookup.callCount.Load(); got != 0 {
		t.Errorf("lookup callCount = %d, want 0", got)
	}
	if got := appender.callCount.Load(); got != 0 {
		t.Errorf("audit callCount = %d, want 0", got)
	}
}

func TestNotebookInheritStep_Execute_EmptyOrgID_NoSeamsTouched_NoAudit(t *testing.T) {
	t.Parallel()

	wkID := uuid.New()
	lookup := &fakePredecessorLookup{}
	inheritor := &fakeNotebookInheritor{}
	appender := &fakeInheritAuditAppender{}
	step := newInheritStep(t, lookup, inheritor, appender)
	sc := newInheritSpawnContext(t, wkID, func(sc *saga.SpawnContext) { sc.Claim.OrganizationID = "" })
	ctx := saga.WithSpawnContext(context.Background(), sc)

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := lookup.callCount.Load(); got != 0 {
		t.Errorf("lookup callCount = %d, want 0", got)
	}
	if got := appender.callCount.Load(); got != 0 {
		t.Errorf("audit callCount = %d, want 0", got)
	}
}

func TestNotebookInheritStep_Execute_NoPredecessor_NoInheritCall_NoAudit(t *testing.T) {
	t.Parallel()

	wkID := uuid.New()
	lookup := &fakePredecessorLookup{returnErr: keepclient.ErrNoPredecessor}
	inheritor := &fakeNotebookInheritor{}
	appender := &fakeInheritAuditAppender{}
	step := newInheritStep(t, lookup, inheritor, appender)
	ctx := saga.WithSpawnContext(context.Background(), newInheritSpawnContext(t, wkID))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := lookup.callCount.Load(); got != 1 {
		t.Errorf("lookup callCount = %d, want 1", got)
	}
	if got := inheritor.callCount.Load(); got != 0 {
		t.Errorf("inherit callCount = %d, want 0 (no predecessor)", got)
	}
	if got := appender.callCount.Load(); got != 0 {
		t.Errorf("audit callCount = %d, want 0 (no predecessor must NOT emit notebook_inherited)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Error paths.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookInheritStep_Execute_LookupTransportError_ScrubsToSentinel(t *testing.T) {
	t.Parallel()

	// Iter-1 codex P1 fix (PII boundary): the underlying transport
	// error string carries the request URL with the role_id query
	// parameter; the step MUST surface a scrubbed sentinel
	// ([ErrPredecessorLookupFailed]) rather than %w-wrapping the
	// underlying chain. The sentinel-only contract makes the
	// returned error's string content predictable and bounded.
	pii := "GET /v1/watchkeepers/latest-retired-by-role?role_id=frontline-watchkeeper: 503"
	wkID := uuid.New()
	lookup := &fakePredecessorLookup{returnErr: errors.New(pii)}
	inheritor := &fakeNotebookInheritor{}
	appender := &fakeInheritAuditAppender{}
	step := newInheritStep(t, lookup, inheritor, appender)
	ctx := saga.WithSpawnContext(context.Background(), newInheritSpawnContext(t, wkID))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: nil err, want ErrPredecessorLookupFailed")
	}
	if !errors.Is(err, spawn.ErrPredecessorLookupFailed) {
		t.Errorf("errors.Is(err, ErrPredecessorLookupFailed) = false; err = %v", err)
	}
	// PII guard: the returned error string MUST NOT carry the
	// upstream error substring (which carries the role_id).
	if strings.Contains(err.Error(), "frontline-watchkeeper") {
		t.Errorf("err string leaks role_id substring: %q", err.Error())
	}
	if strings.Contains(err.Error(), "/v1/watchkeepers") {
		t.Errorf("err string leaks request URL substring: %q", err.Error())
	}
	if got := inheritor.callCount.Load(); got != 0 {
		t.Errorf("inherit callCount = %d, want 0 (lookup failed)", got)
	}
	if got := appender.callCount.Load(); got != 0 {
		t.Errorf("audit callCount = %d, want 0 (lookup failed)", got)
	}
}

func TestNotebookInheritStep_Execute_PredecessorEmptyArchiveURI_ReturnsScrubbedSentinel(t *testing.T) {
	t.Parallel()

	wkID := uuid.New()
	wk := newPredecessorWatchkeeper()
	wk.ArchiveURI = nil
	lookup := &fakePredecessorLookup{returnWk: wk}
	inheritor := &fakeNotebookInheritor{}
	appender := &fakeInheritAuditAppender{}
	step := newInheritStep(t, lookup, inheritor, appender)
	ctx := saga.WithSpawnContext(context.Background(), newInheritSpawnContext(t, wkID))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: nil err, want ErrInvalidPredecessorEnvelope")
	}
	if !errors.Is(err, spawn.ErrInvalidPredecessorEnvelope) {
		t.Errorf("errors.Is(err, ErrInvalidPredecessorEnvelope) = false; err = %v", err)
	}
	if got := inheritor.callCount.Load(); got != 0 {
		t.Errorf("inherit callCount = %d, want 0", got)
	}
	if got := appender.callCount.Load(); got != 0 {
		t.Errorf("audit callCount = %d, want 0", got)
	}
}

func TestNotebookInheritStep_Execute_InheritorError_ScrubsToSentinel_NoAudit(t *testing.T) {
	t.Parallel()

	// Iter-1 codex P1 fix (PII boundary): the underlying inherit
	// error string carries the archive URI substring; the step
	// MUST surface a scrubbed sentinel ([ErrInheritFailed]) rather
	// than %w-wrapping the underlying chain.
	pii := "fetch: GET s3://watchkeepers-archive/agent-pred-001.sqlite: connection refused"
	wkID := uuid.New()
	lookup := &fakePredecessorLookup{returnWk: newPredecessorWatchkeeper()}
	inheritor := &fakeNotebookInheritor{returnErr: errors.New(pii)}
	appender := &fakeInheritAuditAppender{}
	step := newInheritStep(t, lookup, inheritor, appender)
	ctx := saga.WithSpawnContext(context.Background(), newInheritSpawnContext(t, wkID))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: nil err, want ErrInheritFailed")
	}
	if !errors.Is(err, spawn.ErrInheritFailed) {
		t.Errorf("errors.Is(err, ErrInheritFailed) = false; err = %v", err)
	}
	// PII guard: the returned error string MUST NOT carry the
	// archive URI substring.
	if strings.Contains(err.Error(), "s3://watchkeepers-archive") {
		t.Errorf("err string leaks archive URI substring: %q", err.Error())
	}
	if got := appender.callCount.Load(); got != 0 {
		t.Errorf("audit callCount = %d, want 0 (inherit failed)", got)
	}
}

func TestNotebookInheritStep_Execute_AuditError_BestEffort_StepSucceeds(t *testing.T) {
	t.Parallel()

	// Iter-1 codex+critic P1 fix (best-effort audit emit): the
	// inherited data is already in the destination DB when the
	// audit emit dispatches. A transient audit-sink outage MUST
	// NOT poison a successful inheritance — the saga.Runner's
	// `saga_step_completed` row provides the minimum trail; the
	// `notebook_inherited` row is the rich-payload supplement.
	// The step returns nil on this path; ops are alerted via
	// slog.Warn (which this test does not capture; the contract
	// is that Execute returns nil and the inherit was preserved).
	sentinel := errors.New("audit-down")
	wkID := uuid.New()
	lookup := &fakePredecessorLookup{returnWk: newPredecessorWatchkeeper()}
	inheritor := &fakeNotebookInheritor{returnCount: 7}
	appender := &fakeInheritAuditAppender{returnErr: sentinel}
	step := newInheritStep(t, lookup, inheritor, appender)
	ctx := saga.WithSpawnContext(context.Background(), newInheritSpawnContext(t, wkID))

	err := step.Execute(ctx)
	if err != nil {
		t.Fatalf("Execute: %v, want nil (audit emit failure must be best-effort)", err)
	}
	if got := inheritor.callCount.Load(); got != 1 {
		t.Errorf("inherit callCount = %d, want 1 (data was imported before audit failed)", got)
	}
	if got := appender.callCount.Load(); got != 1 {
		t.Errorf("audit callCount = %d, want 1 (the emit was attempted before degrading)", got)
	}
}

func TestNotebookInheritStep_Execute_MissingSpawnContext_NoSeamsTouched(t *testing.T) {
	t.Parallel()

	lookup := &fakePredecessorLookup{}
	inheritor := &fakeNotebookInheritor{}
	appender := &fakeInheritAuditAppender{}
	step := newInheritStep(t, lookup, inheritor, appender)

	err := step.Execute(context.Background())
	if err == nil {
		t.Fatalf("Execute: nil err, want wrapped ErrMissingSpawnContext")
	}
	if !errors.Is(err, spawn.ErrMissingSpawnContext) {
		t.Errorf("errors.Is(err, ErrMissingSpawnContext) = false; err = %v", err)
	}
	if got := lookup.callCount.Load(); got != 0 {
		t.Errorf("lookup callCount = %d, want 0", got)
	}
}

func TestNotebookInheritStep_Execute_NilAgentID_NoSeamsTouched(t *testing.T) {
	t.Parallel()

	lookup := &fakePredecessorLookup{}
	inheritor := &fakeNotebookInheritor{}
	appender := &fakeInheritAuditAppender{}
	step := newInheritStep(t, lookup, inheritor, appender)
	sc := newInheritSpawnContext(t, uuid.Nil)
	ctx := saga.WithSpawnContext(context.Background(), sc)

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: nil err, want wrapped ErrMissingAgentID")
	}
	if !errors.Is(err, spawn.ErrMissingAgentID) {
		t.Errorf("errors.Is(err, ErrMissingAgentID) = false; err = %v", err)
	}
	if got := lookup.callCount.Load(); got != 0 {
		t.Errorf("lookup callCount = %d, want 0", got)
	}
}

func TestNotebookInheritStep_Execute_CancelledContext_NoSeamsTouched(t *testing.T) {
	t.Parallel()

	lookup := &fakePredecessorLookup{}
	inheritor := &fakeNotebookInheritor{}
	appender := &fakeInheritAuditAppender{}
	step := newInheritStep(t, lookup, inheritor, appender)
	ctx, cancel := context.WithCancel(saga.WithSpawnContext(context.Background(), newInheritSpawnContext(t, uuid.New())))
	cancel()

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: nil err, want wrapped ctx.Err()")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; err = %v", err)
	}
	if got := lookup.callCount.Load(); got != 0 {
		t.Errorf("lookup callCount = %d, want 0", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Wrap-chain contract.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookInheritStep_Execute_LookupErrorWrapsErrNoPredecessor_FallsThrough(t *testing.T) {
	t.Parallel()

	// Build a wrapped error that the keepclient SHIM might return —
	// errors.Is(err, ErrNoPredecessor) is true through fmt.Errorf
	// wrapping. The step must still recognise the no-predecessor
	// branch.
	wrapped := fmt.Errorf("transport: %w", keepclient.ErrNoPredecessor)
	wkID := uuid.New()
	lookup := &fakePredecessorLookup{returnErr: wrapped}
	inheritor := &fakeNotebookInheritor{}
	appender := &fakeInheritAuditAppender{}
	step := newInheritStep(t, lookup, inheritor, appender)
	ctx := saga.WithSpawnContext(context.Background(), newInheritSpawnContext(t, wkID))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v, want nil (wrapped ErrNoPredecessor must fall through)", err)
	}
	if got := appender.callCount.Load(); got != 0 {
		t.Errorf("audit callCount = %d, want 0 (no predecessor)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Compile-time contract: the step does NOT implement saga.Compensator.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookInheritStep_DoesNotImplementCompensator(t *testing.T) {
	t.Parallel()
	step := newInheritStep(t, &fakePredecessorLookup{}, &fakeNotebookInheritor{}, &fakeInheritAuditAppender{})

	var x any = step
	if _, ok := x.(saga.Compensator); ok {
		t.Errorf("NotebookInheritStep implements saga.Compensator; want it to NOT implement (the inherited file is owned by NotebookProvisionStep.Compensate)")
	}
}

// TestNotebookInheritStep_Execute_ConcurrentSagas_SharedInstance pins
// the cross-saga concurrency invariant (iter-1 critic P2): one shared
// [NotebookInheritStep] instance MUST handle concurrent Execute calls
// with distinct [saga.SpawnContext] values without cross-talk. The
// step holds only immutable configuration; per-call state lives on
// the goroutine stack and on the per-call context. A future
// regression that stashes per-saga state on the receiver would
// surface here as a lookup callCount lower than the goroutine count
// or as a wrong-wkID Inherit call.
func TestNotebookInheritStep_Execute_ConcurrentSagas_SharedInstance(t *testing.T) {
	t.Parallel()

	lookup := &fakePredecessorLookup{returnWk: newPredecessorWatchkeeper()}
	inheritor := &fakeNotebookInheritor{returnCount: 1}
	appender := &fakeInheritAuditAppender{returnID: "evt"}
	step := newInheritStep(t, lookup, inheritor, appender)

	const goroutines = 16
	wkIDs := make([]uuid.UUID, goroutines)
	for i := range wkIDs {
		wkIDs[i] = uuid.New()
	}

	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			ctx := saga.WithSpawnContext(context.Background(), newInheritSpawnContext(t, wkIDs[i]))
			errs[i] = step.Execute(ctx)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine[%d]: %v", i, err)
		}
	}
	if got := lookup.callCount.Load(); int(got) != goroutines {
		t.Errorf("lookup callCount = %d, want %d (one per goroutine)", got, goroutines)
	}
	if got := inheritor.callCount.Load(); int(got) != goroutines {
		t.Errorf("inherit callCount = %d, want %d (one per goroutine)", got, goroutines)
	}
	if got := appender.callCount.Load(); int(got) != goroutines {
		t.Errorf("audit callCount = %d, want %d (one per goroutine)", got, goroutines)
	}

	// Verify each goroutine saw its own wkID on the Inherit
	// dispatch. The fake records the wkID per call; we assert the
	// set matches the per-goroutine wkID slice (order-insensitive
	// since goroutines race).
	seen := make(map[uuid.UUID]bool, goroutines)
	for _, c := range inheritor.recordedCalls() {
		seen[c.watchkeeperID] = true
	}
	for i, want := range wkIDs {
		if !seen[want] {
			t.Errorf("inheritor missed wkID for goroutine[%d] = %v", i, want)
		}
	}
}
