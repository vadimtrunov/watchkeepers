package spawn_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	slackmessenger "github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// ────────────────────────────────────────────────────────────────────────
// Hand-rolled fakes (no mocking lib — M3.6 / M6.3.e pattern).
// ────────────────────────────────────────────────────────────────────────

// fakeSlackAppRPC is a hand-rolled [spawn.SlackAppRPC] that:
//   - Records every CreateApp call onto a SHARED record set so tests
//     can assert call count + argument shape across multiple
//     per-call clones.
//   - Optionally returns a configured error to drive the negative
//     paths.
//   - Implements [spawn.CreateAppCredsSinkInstaller] by returning a
//     CLONED fake whose only delta is the per-call sink. The shared
//     record set is held behind a pointer so all clones append into
//     the same slice — matching the production wiring contract that
//     `WithCreateAppCredsSink` returns a per-call clone (NOT
//     mutating-in-place); the original fake remains usable for
//     subsequent calls without leaking state across goroutines.
//   - Fires the configured creds payload through the installed sink
//     so the DAO bridge runs even though no real messenger.Client is
//     in the loop.
type fakeSlackAppRPC struct {
	// Configured behaviour (set on the original; copied into clones):
	returnErr error
	creds     slackmessenger.CreateAppCredentials

	// Per-call sink (nil on the original; populated on per-call
	// clones returned from WithCreateAppCredsSink).
	sink slackmessenger.CreateAppCredsSink

	// Shared record set (pointer so clones share the slice with the
	// original).
	rec *fakeSlackAppRPCRecord
}

type fakeSlackAppRPCRecord struct {
	mu          sync.Mutex
	calls       []recordedCreateApp
	withCalls   atomic.Int32
	sinkInvoked atomic.Bool
}

type recordedCreateApp struct {
	req   spawn.CreateAppRequest
	claim spawn.Claim
}

func newFakeSlackAppRPC(creds slackmessenger.CreateAppCredentials) *fakeSlackAppRPC {
	return &fakeSlackAppRPC{
		creds: creds,
		rec:   &fakeSlackAppRPCRecord{},
	}
}

func (f *fakeSlackAppRPC) WithCreateAppCredsSink(sink slackmessenger.CreateAppCredsSink) spawn.SlackAppRPC {
	f.rec.withCalls.Add(1)
	clone := *f
	clone.sink = sink
	return &clone
}

func (f *fakeSlackAppRPC) CreateApp(ctx context.Context, req spawn.CreateAppRequest, claim spawn.Claim) (spawn.CreateAppResult, error) {
	f.rec.mu.Lock()
	f.rec.calls = append(f.rec.calls, recordedCreateApp{req: req, claim: claim})
	f.rec.mu.Unlock()

	if f.returnErr != nil {
		return spawn.CreateAppResult{}, f.returnErr
	}

	// Mirror the real adapter's M4.2.d.2 ordering: the sink fires
	// AFTER the platform call succeeds, BEFORE we return the AppID.
	if f.sink != nil {
		f.rec.sinkInvoked.Store(true)
		if err := f.sink(ctx, f.creds); err != nil {
			// The real adapter wraps as `slack: apps.manifest.create:
			// credentials sink: <err>`; the test fake mirrors the
			// wrap depth so `errors.Is` chains stay realistic.
			return spawn.CreateAppResult{}, err
		}
	}
	return spawn.CreateAppResult{AppID: f.creds.AppID}, nil
}

func (f *fakeSlackAppRPC) recordedCalls() []recordedCreateApp {
	f.rec.mu.Lock()
	defer f.rec.mu.Unlock()
	out := make([]recordedCreateApp, len(f.rec.calls))
	copy(out, f.rec.calls)
	return out
}

// ────────────────────────────────────────────────────────────────────────
// Helpers
// ────────────────────────────────────────────────────────────────────────

func newSpawnContext(t *testing.T, watchkeeperID uuid.UUID) saga.SpawnContext {
	t.Helper()
	return saga.SpawnContext{
		ManifestVersionID: uuid.New(),
		AgentID:           watchkeeperID,
		Claim: saga.SpawnClaim{
			OrganizationID:  "org-test",
			AgentID:         "agent-watchmaster",
			AuthorityMatrix: map[string]string{"slack_app_create": "lead_approval"},
		},
	}
}

//nolint:gosec // G101: "tok-test-approval-token" is a synthetic test placeholder.
func newStep(t *testing.T, rpc spawn.SlackAppRPC, dao spawn.WatchkeeperSlackAppCredsDAO) *spawn.CreateAppStep {
	t.Helper()
	return spawn.NewCreateAppStep(spawn.CreateAppStepDeps{
		RPC:           rpc,
		CredsDAO:      dao,
		Teardown:      nopSlackAppTeardown{},
		AppName:       "test-app",
		Scopes:        []string{"chat:write"},
		ApprovalToken: "tok-test-approval-token",
	})
}

// ────────────────────────────────────────────────────────────────────────
// Compile-time + Name() coverage
// ────────────────────────────────────────────────────────────────────────

func TestCreateAppStep_Name_ReturnsStableIdentifier(t *testing.T) {
	t.Parallel()

	step := newStep(
		t,
		newFakeSlackAppRPC(newTestCreds()),
		spawn.NewMemoryWatchkeeperSlackAppCredsDAO(),
	)
	if got := step.Name(); got != "create_app" {
		t.Errorf("Name() = %q, want %q", got, "create_app")
	}
	if got := step.Name(); got != spawn.CreateAppStepName {
		t.Errorf("Name() = %q, want %q (CreateAppStepName)", got, spawn.CreateAppStepName)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Constructor panics
// ────────────────────────────────────────────────────────────────────────

func TestNewCreateAppStep_PanicsOnRequiredFields(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(*spawn.CreateAppStepDeps)
	}{
		{"nil RPC", func(d *spawn.CreateAppStepDeps) { d.RPC = nil }},
		{"nil CredsDAO", func(d *spawn.CreateAppStepDeps) { d.CredsDAO = nil }},
		{"nil Teardown", func(d *spawn.CreateAppStepDeps) { d.Teardown = nil }},
		{"empty AppName", func(d *spawn.CreateAppStepDeps) { d.AppName = "" }},
		{"empty ApprovalToken", func(d *spawn.CreateAppStepDeps) { d.ApprovalToken = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("NewCreateAppStep with %s did not panic", tc.name)
				}
			}()
			deps := spawn.CreateAppStepDeps{
				RPC:           newFakeSlackAppRPC(newTestCreds()),
				CredsDAO:      spawn.NewMemoryWatchkeeperSlackAppCredsDAO(),
				Teardown:      nopSlackAppTeardown{},
				AppName:       "test-app",
				ApprovalToken: "tok-test",
			}
			tc.mut(&deps)
			_ = spawn.NewCreateAppStep(deps)
		})
	}
}

// ────────────────────────────────────────────────────────────────────────
// Happy path (test-plan §"Happy")
// ────────────────────────────────────────────────────────────────────────

func TestCreateAppStep_Execute_HappyPath_PutsCredsKeyedByWatchkeeperID(t *testing.T) {
	t.Parallel()

	creds := newTestCreds()
	rpc := newFakeSlackAppRPC(creds)
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	step := newStep(t, rpc, dao)

	watchkeeperID := uuid.New()
	sc := newSpawnContext(t, watchkeeperID)
	ctx := saga.WithSpawnContext(context.Background(), sc)

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// AC2: RPC was called once with the SpawnContext-derived claim
	// + step-configured request shape.
	calls := rpc.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("RPC.CreateApp call count = %d, want 1", len(calls))
	}
	if calls[0].claim.OrganizationID != sc.Claim.OrganizationID {
		t.Errorf("claim.OrganizationID = %q, want %q",
			calls[0].claim.OrganizationID, sc.Claim.OrganizationID)
	}
	if calls[0].req.AppName != "test-app" {
		t.Errorf("req.AppName = %q, want %q", calls[0].req.AppName, "test-app")
	}

	// AC6: DAO row keyed by watchkeeperID, NOT by creds.AppID.
	got, err := dao.Get(context.Background(), watchkeeperID)
	if err != nil {
		t.Fatalf("dao.Get(watchkeeperID): %v", err)
	}
	if got != creds {
		t.Errorf("dao.Get returned %+v, want %+v", got, creds)
	}

	if !rpc.rec.sinkInvoked.Load() {
		t.Error("sink was not invoked; expected RPC adapter to fire it on success")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: RPC error (test-plan §"Negative — SlackAppRPC error")
// ────────────────────────────────────────────────────────────────────────

func TestCreateAppStep_Execute_RPCError_WrapsAndDoesNotCallDAOPut(t *testing.T) {
	t.Parallel()

	rpc := newFakeSlackAppRPC(newTestCreds())
	rpc.returnErr = spawn.ErrUnauthorized
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	step := newStep(t, rpc, dao)

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newSpawnContext(t, watchkeeperID))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrUnauthorized")
	}
	if !errors.Is(err, spawn.ErrUnauthorized) {
		t.Errorf("errors.Is(err, ErrUnauthorized) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: create_app step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: create_app step:")
	}

	// DAO MUST NOT have a row — the sink never fired.
	if _, getErr := dao.Get(context.Background(), watchkeeperID); !errors.Is(getErr, spawn.ErrCredsNotFound) {
		t.Errorf("dao.Get after RPC failure err = %v, want ErrCredsNotFound", getErr)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: DAO.Put error (test-plan §"Negative — DAO.Put error")
// ────────────────────────────────────────────────────────────────────────

func TestCreateAppStep_Execute_DAOPutError_ReturnsWrappedSinkError(t *testing.T) {
	t.Parallel()

	creds := newTestCreds()
	rpc := newFakeSlackAppRPC(creds)
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()

	watchkeeperID := uuid.New()
	// Pre-seed the DAO so the second Put surfaces ErrCredsAlreadyStored
	// — which is exactly the typed error the sink bridge surfaces.
	if err := dao.Put(context.Background(), watchkeeperID, creds); err != nil {
		t.Fatalf("pre-seed Put: %v", err)
	}

	step := newStep(t, rpc, dao)
	ctx := saga.WithSpawnContext(context.Background(), newSpawnContext(t, watchkeeperID))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrCredsAlreadyStored")
	}
	if !errors.Is(err, spawn.ErrCredsAlreadyStored) {
		t.Errorf("errors.Is(err, ErrCredsAlreadyStored) = false; got %v", err)
	}
	// Document for the reader: the underlying RPC call (Slack
	// manifest.create) is server-side complete. M7.1.c.a is
	// forward-only per ROADMAP — reconciliation belongs to a future
	// M7.x compensation step. The test pins the wrap chain only.
}

// ────────────────────────────────────────────────────────────────────────
// M7.3.d in-Execute partial-success cleanup: when the sink fires +
// captures `creds.AppID` then errors on the DAO Put, the post-RPC
// failure branch dispatches a best-effort teardown carrying the
// captured platform-side app id. The teardown error is silently
// discarded so the operator's load-bearing signal stays the
// original sink error.
// ────────────────────────────────────────────────────────────────────────

func TestCreateAppStep_Execute_DAOPutError_DispatchesInExecuteTeardownWithCapturedAppID(t *testing.T) {
	t.Parallel()

	creds := newTestCreds()
	rpc := newFakeSlackAppRPC(creds)
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	teardown := &recordingSlackAppTeardown{}

	watchkeeperID := uuid.New()
	// Pre-seed the DAO so the saga's Put surfaces ErrCredsAlreadyStored
	// from inside the sink — same partial-success pattern as the
	// previous test, but here we pin the in-Execute cleanup dispatch.
	if err := dao.Put(context.Background(), watchkeeperID, creds); err != nil {
		t.Fatalf("pre-seed Put: %v", err)
	}

	step := spawn.NewCreateAppStep(spawn.CreateAppStepDeps{
		RPC:           rpc,
		CredsDAO:      dao,
		Teardown:      teardown,
		AppName:       "test-app",
		Scopes:        []string{"chat:write"},
		ApprovalToken: "tok-test",
	})
	ctx := saga.WithSpawnContext(context.Background(), newSpawnContext(t, watchkeeperID))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrCredsAlreadyStored")
	}
	if !errors.Is(err, spawn.ErrCredsAlreadyStored) {
		t.Errorf("errors.Is(err, ErrCredsAlreadyStored) = false; got %v", err)
	}

	// In-Execute teardown MUST have been dispatched exactly once —
	// the sink captured creds.AppID before the DAO Put failed.
	if got := teardown.callCount.Load(); got != 1 {
		t.Fatalf("Teardown call count = %d, want 1 (in-Execute partial-success cleanup)", got)
	}
	calls := teardown.recorded()
	if len(calls) != 1 {
		t.Fatalf("Teardown recorded calls = %v, want exactly 1", calls)
	}
	if calls[0].watchkeeperID != watchkeeperID {
		t.Errorf("Teardown.watchkeeperID = %v, want %v", calls[0].watchkeeperID, watchkeeperID)
	}
	// The captured app id MUST be the platform-assigned id from the
	// sink callback (creds.AppID). An empty knownAppID would mean
	// the in-Execute path wired through the Compensate-style DAO-
	// fallback contract — wrong direction.
	if calls[0].knownAppID != creds.AppID {
		t.Errorf("Teardown.knownAppID = %q, want %q (captured from sink)", calls[0].knownAppID, creds.AppID)
	}
}

// TestCreateAppStep_Execute_CtxCancelledDuringSink_InExecuteTeardownStillRuns
// pins the M7.3.d iter-1 codex+critic Major #1 fix: when ctx is
// cancelled DURING the sink (after the platform call succeeded
// but before the post-RPC failure branch decides to skip cleanup),
// the in-Execute teardown MUST still dispatch. Mirrors the M7.3.b
// iter-1 #2 saga.compensate context.WithoutCancel discipline. The
// most likely real-world trigger is a request-bound parent ctx
// that fired Cancel mid-saga (HTTP timeout, operator-initiated
// abort) — without the WithoutCancel wrap the cleanup uniformly
// fails on EXACTLY the scenario it exists to defend against.
//
// Test plumbing: a wrapping DAO cancels its own parent ctx INSIDE
// the sink's Put callback (after capturedAppID was written, before
// returning a non-nil error). The post-RPC failure branch then
// dispatches teardown against a cancelled parent ctx; the
// recording teardown's call count MUST be 1 — the WithoutCancel
// wrap kept the dispatch alive.
func TestCreateAppStep_Execute_CtxCancelledDuringSink_InExecuteTeardownStillRuns(t *testing.T) {
	t.Parallel()

	creds := newTestCreds()
	rpc := newFakeSlackAppRPC(creds)
	memDAO := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	teardown := &recordingSlackAppTeardown{}

	ctx, cancel := context.WithCancel(saga.WithSpawnContext(context.Background(), newSpawnContext(t, uuid.New())))
	defer cancel()
	cancellingDAO := &cancellingPutDAO{
		MemoryWatchkeeperSlackAppCredsDAO: memDAO,
		cancel:                            cancel,
		err:                               errors.New("simulated DAO Put failure after cancel"),
	}

	step := spawn.NewCreateAppStep(spawn.CreateAppStepDeps{
		RPC:           rpc,
		CredsDAO:      cancellingDAO,
		Teardown:      teardown,
		AppName:       "test-app",
		Scopes:        []string{"chat:write"},
		ApprovalToken: "tok-test",
	})

	if err := step.Execute(ctx); err == nil {
		t.Fatalf("Execute: err = nil, want wrapped DAO error")
	}

	// The teardown MUST have been dispatched once even though the
	// parent ctx was cancelled inside the sink. Validates
	// context.WithoutCancel wrap on the in-Execute cleanup branch.
	if got := teardown.callCount.Load(); got != 1 {
		t.Errorf("Teardown call count under in-sink-cancellation = %d, want 1 (WithoutCancel discipline)", got)
	}
}

// cancellingPutDAO embeds [MemoryWatchkeeperSlackAppCredsDAO] and
// overrides [Put] to (a) cancel its supplied `cancel` func before
// returning, (b) return the configured error. Pinpoint test fixture
// for the M7.3.d in-Execute WithoutCancel test — a real Put would
// not normally cancel ctx, but a real downstream cause of a Put
// failure (HTTP deadline, operator abort) WOULD have already
// cancelled the saga's parent ctx by the time control returns to
// the step body.
type cancellingPutDAO struct {
	*spawn.MemoryWatchkeeperSlackAppCredsDAO
	cancel context.CancelFunc
	err    error
}

func (d *cancellingPutDAO) Put(_ context.Context, _ uuid.UUID, _ slackmessenger.CreateAppCredentials) error {
	d.cancel()
	return d.err
}

// TestCreateAppStep_Execute_RPCErrorBeforeSink_NoInExecuteTeardown
// pins the inverse: when the platform call (rpc.CreateApp) fails
// BEFORE the sink ever runs, capturedAppID stays empty and the
// in-Execute teardown is intentionally NOT dispatched (no platform
// state to clean up).
func TestCreateAppStep_Execute_RPCErrorBeforeSink_NoInExecuteTeardown(t *testing.T) {
	t.Parallel()

	rpc := newFakeSlackAppRPC(newTestCreds())
	rpc.returnErr = spawn.ErrUnauthorized // RPC fails synchronously; sink never fires
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	teardown := &recordingSlackAppTeardown{}

	step := spawn.NewCreateAppStep(spawn.CreateAppStepDeps{
		RPC:           rpc,
		CredsDAO:      dao,
		Teardown:      teardown,
		AppName:       "test-app",
		Scopes:        []string{"chat:write"},
		ApprovalToken: "tok-test",
	})
	ctx := saga.WithSpawnContext(context.Background(), newSpawnContext(t, uuid.New()))

	if err := step.Execute(ctx); err == nil || !errors.Is(err, spawn.ErrUnauthorized) {
		t.Fatalf("Execute: err = %v, want wrapped ErrUnauthorized", err)
	}
	if got := teardown.callCount.Load(); got != 0 {
		t.Errorf("Teardown call count = %d, want 0 (no platform-side state to clean up)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: SpawnContext missing (test-plan §"Negative — Context missing")
// ────────────────────────────────────────────────────────────────────────

func TestCreateAppStep_Execute_MissingSpawnContext_DoesNotCallRPC(t *testing.T) {
	t.Parallel()

	rpc := newFakeSlackAppRPC(newTestCreds())
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	step := newStep(t, rpc, dao)

	err := step.Execute(context.Background())
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingSpawnContext")
	}
	if !errors.Is(err, spawn.ErrMissingSpawnContext) {
		t.Errorf("errors.Is(err, ErrMissingSpawnContext) = false; got %v", err)
	}

	if got := rpc.recordedCalls(); len(got) != 0 {
		t.Errorf("RPC.CreateApp call count = %d, want 0 (fail-fast on missing SpawnContext)", len(got))
	}
}

// ────────────────────────────────────────────────────────────────────────
// Edge: ManifestVersionID is uuid.Nil (test-plan §"Edge — uuid.Nil")
// ────────────────────────────────────────────────────────────────────────

func TestCreateAppStep_Execute_NilManifestVersionID_DoesNotCallRPC(t *testing.T) {
	t.Parallel()

	rpc := newFakeSlackAppRPC(newTestCreds())
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	step := newStep(t, rpc, dao)

	sc := saga.SpawnContext{
		ManifestVersionID: uuid.Nil, // <- the bad value
		AgentID:           uuid.New(),
		Claim: saga.SpawnClaim{
			OrganizationID:  "org-test",
			AgentID:         "agent-watchmaster",
			AuthorityMatrix: map[string]string{"slack_app_create": "lead_approval"},
		},
	}
	ctx := saga.WithSpawnContext(context.Background(), sc)

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingManifestVersion")
	}
	if !errors.Is(err, spawn.ErrMissingManifestVersion) {
		t.Errorf("errors.Is(err, ErrMissingManifestVersion) = false; got %v", err)
	}
	if got := rpc.recordedCalls(); len(got) != 0 {
		t.Errorf("RPC.CreateApp call count = %d, want 0", len(got))
	}
}

// TestCreateAppStep_Execute_NilAgentID_DoesNotCallRPC pins the
// symmetric guard for AgentID = uuid.Nil — uuid.Nil cannot be a
// credential-store key, so the step rejects it before touching the
// RPC.
func TestCreateAppStep_Execute_NilAgentID_DoesNotCallRPC(t *testing.T) {
	t.Parallel()

	rpc := newFakeSlackAppRPC(newTestCreds())
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	step := newStep(t, rpc, dao)

	sc := saga.SpawnContext{
		ManifestVersionID: uuid.New(),
		AgentID:           uuid.Nil, // <- the bad value
		Claim: saga.SpawnClaim{
			OrganizationID:  "org-test",
			AgentID:         "agent-watchmaster",
			AuthorityMatrix: map[string]string{"slack_app_create": "lead_approval"},
		},
	}
	ctx := saga.WithSpawnContext(context.Background(), sc)

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingAgentID")
	}
	if !errors.Is(err, spawn.ErrMissingAgentID) {
		t.Errorf("errors.Is(err, ErrMissingAgentID) = false; got %v", err)
	}
	if got := rpc.recordedCalls(); len(got) != 0 {
		t.Errorf("RPC.CreateApp call count = %d, want 0", len(got))
	}
}

// ────────────────────────────────────────────────────────────────────────
// AC7: PII-safe audit — step does NOT call keeperslog.Writer.Append
// ────────────────────────────────────────────────────────────────────────

// TestCreateAppStep_DoesNotCallKeepersLogAppend pins AC7 via a source-grep
// assertion: it reads createapp_step.go, strips pure comment lines, then
// asserts the non-comment source contains neither a "keeperslog." reference
// nor a ".Append(" call. This is a stronger pin than a runtime assertion
// because it catches any future edit that adds a keeperslog import or an
// Append call regardless of whether a Writer is wired through the test
// harness. Comment lines (starting with optional whitespace + "//") are
// excluded so godoc cross-references like [keeperslog.Writer.Append] do not
// trip the assertion.
//
// AC7 wording: "step does not call keeperslog.Writer.Append directly —
// the audit chain belongs to SlackAppRPC + saga core."
func TestCreateAppStep_DoesNotCallKeepersLogAppend(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("createapp_step.go")
	if err != nil {
		t.Fatalf("read createapp_step.go: %v", err)
	}

	// Build a version of the source with comment-only lines removed so
	// godoc cross-references (e.g. "[keeperslog.Writer.Append]") do not
	// trigger the assertions.
	var nonComment strings.Builder
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "//") {
			nonComment.WriteString(line)
			nonComment.WriteByte('\n')
		}
	}
	body := nonComment.String()

	if strings.Contains(body, "keeperslog.") {
		t.Errorf("createapp_step.go imports/references keeperslog in non-comment code — AC7 says the step must not. Audit chain belongs to SlackAppRPC + saga core.")
	}
	if strings.Contains(body, ".Append(") {
		t.Errorf("createapp_step.go contains '.Append(' in non-comment code — AC7 says the step must not call keeperslog.Writer.Append.")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Security: no credential strings leak into error paths
// ────────────────────────────────────────────────────────────────────────

// TestCreateAppStep_Execute_ErrorPaths_DoNotLeakCredentials greps the
// returned error strings (and the test logs that the harness
// captures) for any of the configured creds substrings. A
// well-behaved error chain returns sentinel-only wraps; a regression
// that accidentally embeds creds.ClientSecret into a fmt.Errorf
// would trip this assertion.
func TestCreateAppStep_Execute_ErrorPaths_DoNotLeakCredentials(t *testing.T) {
	t.Parallel()

	creds := newTestCreds()
	credsStrings := []string{
		creds.ClientID,
		creds.ClientSecret,
		creds.VerificationToken,
		creds.SigningSecret,
	}

	cases := []struct {
		name  string
		setup func() (step *spawn.CreateAppStep, ctx context.Context)
	}{
		{
			name: "rpc error",
			setup: func() (*spawn.CreateAppStep, context.Context) {
				rpc := newFakeSlackAppRPC(creds)
				rpc.returnErr = spawn.ErrUnauthorized
				dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
				return newStep(t, rpc, dao), saga.WithSpawnContext(context.Background(), newSpawnContext(t, uuid.New()))
			},
		},
		{
			name: "dao put error",
			setup: func() (*spawn.CreateAppStep, context.Context) {
				rpc := newFakeSlackAppRPC(creds)
				dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
				wkID := uuid.New()
				_ = dao.Put(context.Background(), wkID, creds)
				return newStep(t, rpc, dao), saga.WithSpawnContext(context.Background(), newSpawnContext(t, wkID))
			},
		},
		{
			name: "missing spawn context",
			setup: func() (*spawn.CreateAppStep, context.Context) {
				rpc := newFakeSlackAppRPC(creds)
				dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
				return newStep(t, rpc, dao), context.Background()
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			step, ctx := tc.setup()
			err := step.Execute(ctx)
			if err == nil {
				t.Fatalf("Execute: err = nil, want non-nil for %s", tc.name)
			}
			msg := err.Error()
			for _, secret := range credsStrings {
				if bytes.Contains([]byte(msg), []byte(secret)) {
					t.Errorf("error message %q contains credential substring %q (PII leak)", msg, secret)
				}
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────
// Concurrency: distinct watchkeeper ids, race-detector clean (AC8)
// ────────────────────────────────────────────────────────────────────────

// TestCreateAppStep_Execute_Concurrency_DistinctWatchkeepers fires N
// concurrent Execute calls, each with a distinct SpawnContext (so
// each Execute writes a distinct DAO row). Combined with `go test
// -race`, this pins the DAO mutex contract AND the step's
// per-call-isolation discipline (the step must not retain any
// per-call state across goroutines).
func TestCreateAppStep_Execute_Concurrency_DistinctWatchkeepers(t *testing.T) {
	t.Parallel()

	creds := newTestCreds()
	rpc := newFakeSlackAppRPC(creds)
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	step := newStep(t, rpc, dao)

	const n = 16
	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.New()
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			ctx := saga.WithSpawnContext(context.Background(), newSpawnContext(t, id))
			if err := step.Execute(ctx); err != nil {
				t.Errorf("Execute(%v): %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	for _, id := range ids {
		if _, err := dao.Get(context.Background(), id); err != nil {
			t.Errorf("dao.Get(%v): %v", id, err)
		}
	}
}
