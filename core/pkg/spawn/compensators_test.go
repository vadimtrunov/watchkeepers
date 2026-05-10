// compensators_test.go ships the M7.3.c per-step Compensate test
// surface for the spawn-saga family. The file:
//
//   - Hand-rolls one fake per new seam (SlackAppTeardown,
//     OAuthInstallRevoker, NotebookProvisionArchiver, RuntimeTeardown)
//     using the M3.6 / M6.3.e / M7.1.c.* "no mocking lib" pattern.
//   - Provides nopXxx defaults the existing M7.1.c.* step test
//     helpers wire into the new required deps so the M7.1.c.*
//     test suites compile against the new constructor surface
//     without change of behaviour on the Execute path. The recording
//     fakes (recordingXxx) are reserved for the new Compensate-
//     specific test cases that assert call shape, error wrapping,
//     and PII discipline.
//   - Hosts the per-step Compensate unit tests: happy path,
//     missing-SpawnContext, nil AgentID, seam-error wrap, PII
//     redaction (canary harness), and source-grep AC for the
//     audit-emit boundary.
//
// The file lives in package `spawn_test` (the external test package
// already used by every M7.1.c.* step test) so the seams + sentinel
// types are accessed via their exported identifiers.
package spawn_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// ────────────────────────────────────────────────────────────────────────
// Noop seam impls — wired by the existing M7.1.c.* step test helpers
// to satisfy the new Compensator-required deps without changing
// happy-path behaviour. Each returns nil from its only method; the
// recording variants below are used by the Compensate-focused tests.
// ────────────────────────────────────────────────────────────────────────

type nopSlackAppTeardown struct{}

func (nopSlackAppTeardown) TeardownApp(_ context.Context, _ uuid.UUID) error { return nil }

type nopOAuthInstallRevoker struct{}

func (nopOAuthInstallRevoker) Revoke(_ context.Context, _ uuid.UUID) error { return nil }

type nopNotebookProvisionArchiver struct{}

// ArchiveAndFlagForReview returns a deterministic synthetic
// archive URI so the M7.3.c [NotebookProvisionStep.Compensate]'s
// non-empty + shape-valid checks pass on the noop path. The URI is
// scheme-prefixed (`memory://`) so the [net/url.Parse] check
// returns a non-empty Scheme.
func (nopNotebookProvisionArchiver) ArchiveAndFlagForReview(
	_ context.Context, watchkeeperID uuid.UUID,
) (string, error) {
	return "memory://archive/noop/" + watchkeeperID.String(), nil
}

type nopRuntimeTeardown struct{}

func (nopRuntimeTeardown) Teardown(_ context.Context, _ uuid.UUID, _ uuid.UUID) error {
	return nil
}

// ────────────────────────────────────────────────────────────────────────
// Recording fakes — used by Compensate-focused tests to pin call
// shape, sequencing, and per-call argument forwarding.
// ────────────────────────────────────────────────────────────────────────

type recordingSlackAppTeardown struct {
	mu        sync.Mutex
	calls     []uuid.UUID
	callCount atomic.Int32
	returnErr error
}

func (r *recordingSlackAppTeardown) TeardownApp(_ context.Context, watchkeeperID uuid.UUID) error {
	r.callCount.Add(1)
	r.mu.Lock()
	r.calls = append(r.calls, watchkeeperID)
	r.mu.Unlock()
	return r.returnErr
}

func (r *recordingSlackAppTeardown) recorded() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]uuid.UUID, len(r.calls))
	copy(out, r.calls)
	return out
}

type recordingOAuthInstallRevoker struct {
	mu        sync.Mutex
	calls     []uuid.UUID
	callCount atomic.Int32
	returnErr error
}

func (r *recordingOAuthInstallRevoker) Revoke(_ context.Context, watchkeeperID uuid.UUID) error {
	r.callCount.Add(1)
	r.mu.Lock()
	r.calls = append(r.calls, watchkeeperID)
	r.mu.Unlock()
	return r.returnErr
}

// recordingArchiveCall captures both the watchkeeperID + the URI the
// fake returned so the test can pin success-path URI propagation.
type recordingArchiveCall struct {
	watchkeeperID uuid.UUID
	uri           string
}

type recordingNotebookProvisionArchiver struct {
	mu        sync.Mutex
	calls     []recordingArchiveCall
	callCount atomic.Int32
	returnURI string // overrides the synthetic memory:// URI when non-empty
	returnErr error
}

func (r *recordingNotebookProvisionArchiver) ArchiveAndFlagForReview(
	_ context.Context, watchkeeperID uuid.UUID,
) (string, error) {
	r.callCount.Add(1)
	uri := r.returnURI
	if uri == "" && r.returnErr == nil {
		uri = "memory://archive/recorded/" + watchkeeperID.String()
	}
	r.mu.Lock()
	r.calls = append(r.calls, recordingArchiveCall{watchkeeperID: watchkeeperID, uri: uri})
	r.mu.Unlock()
	return uri, r.returnErr
}

func (r *recordingNotebookProvisionArchiver) recorded() []recordingArchiveCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordingArchiveCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// returningArchiver returns a configured (uri, err) literally — used
// by the empty-URI / malformed-URI cases where the recording fake's
// fall-through to a synthetic memory:// URI would mask the
// validation paths.
type returningArchiver struct {
	returnURI string
	returnErr error
}

func (r *returningArchiver) ArchiveAndFlagForReview(_ context.Context, _ uuid.UUID) (string, error) {
	return r.returnURI, r.returnErr
}

// recordingTeardownCall captures both ids forwarded into the seam so
// the test can pin the M7.3.c "manifestVersionID propagation" contract.
type recordingTeardownCall struct {
	watchkeeperID     uuid.UUID
	manifestVersionID uuid.UUID
}

type recordingRuntimeTeardown struct {
	mu        sync.Mutex
	calls     []recordingTeardownCall
	callCount atomic.Int32
	returnErr error
}

func (r *recordingRuntimeTeardown) Teardown(
	_ context.Context, watchkeeperID uuid.UUID, manifestVersionID uuid.UUID,
) error {
	r.callCount.Add(1)
	r.mu.Lock()
	r.calls = append(r.calls, recordingTeardownCall{
		watchkeeperID: watchkeeperID, manifestVersionID: manifestVersionID,
	})
	r.mu.Unlock()
	return r.returnErr
}

func (r *recordingRuntimeTeardown) recorded() []recordingTeardownCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordingTeardownCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// ────────────────────────────────────────────────────────────────────────
// Test plumbing
// ────────────────────────────────────────────────────────────────────────

func newCompensateSpawnContext(t *testing.T, watchkeeperID, manifestVersionID uuid.UUID) saga.SpawnContext {
	t.Helper()
	return saga.SpawnContext{
		ManifestVersionID: manifestVersionID,
		AgentID:           watchkeeperID,
		Claim: saga.SpawnClaim{
			OrganizationID:  "org-test",
			AgentID:         "agent-watchmaster",
			AuthorityMatrix: map[string]string{"slack_app_create": "lead_approval"},
		},
	}
}

// classedTeardownErr is a typed error implementing
// [saga.LastErrorClassed]. Used by the test cases that assert the
// per-step Compensate's wrap chain preserves a deeper
// LastErrorClassed link so the saga.Runner's
// `resolveCompensateLastErrorClass` picks the seam-class verbatim.
type classedTeardownErr struct{ class string }

func (e *classedTeardownErr) Error() string          { return "classed teardown: " + e.class }
func (e *classedTeardownErr) LastErrorClass() string { return e.class }

// piiCanary is the synthetic substring the Compensate-side PII tests
// inject into seam errors. The test asserts the canary IS preserved
// by `%w` (otherwise errors.Is wouldn't match) AND that the
// step-internal config (creds, app_id, approval token, app_name)
// does NOT leak into the wrap.
//
//nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.
const piiCanary = "x-canary-secret-do-not-leak-9C6F2E8D"

// ────────────────────────────────────────────────────────────────────────
// CreateAppStep.Compensate
// ────────────────────────────────────────────────────────────────────────

func TestCreateAppStep_Compensate_HappyPath_DispatchesTeardownWithWatchkeeperID(t *testing.T) {
	t.Parallel()

	teardown := &recordingSlackAppTeardown{}
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	step := spawn.NewCreateAppStep(spawn.CreateAppStepDeps{
		RPC:           newFakeSlackAppRPC(newTestCreds()),
		CredsDAO:      dao,
		Teardown:      teardown,
		AppName:       "test-app",
		ApprovalToken: "tok-test",
	})

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, watchkeeperID, uuid.New()))

	if err := step.Compensate(ctx); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if got := teardown.callCount.Load(); got != 1 {
		t.Fatalf("Teardown call count = %d, want 1", got)
	}
	if calls := teardown.recorded(); len(calls) != 1 || calls[0] != watchkeeperID {
		t.Errorf("Teardown calls = %v, want [%v]", calls, watchkeeperID)
	}
}

func TestCreateAppStep_Compensate_MissingSpawnContext_NoTeardownCall(t *testing.T) {
	t.Parallel()

	teardown := &recordingSlackAppTeardown{}
	step := spawn.NewCreateAppStep(spawn.CreateAppStepDeps{
		RPC:           newFakeSlackAppRPC(newTestCreds()),
		CredsDAO:      spawn.NewMemoryWatchkeeperSlackAppCredsDAO(),
		Teardown:      teardown,
		AppName:       "test-app",
		ApprovalToken: "tok-test",
	})

	err := step.Compensate(context.Background())
	if err == nil {
		t.Fatalf("Compensate: err = nil, want wrapped ErrMissingSpawnContext")
	}
	if !errors.Is(err, spawn.ErrMissingSpawnContext) {
		t.Errorf("errors.Is(err, ErrMissingSpawnContext) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: create_app step compensate:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: create_app step compensate:")
	}
	if got := teardown.callCount.Load(); got != 0 {
		t.Errorf("Teardown call count = %d, want 0 (missing SpawnContext must short-circuit)", got)
	}
}

func TestCreateAppStep_Compensate_NilAgentID_NoTeardownCall(t *testing.T) {
	t.Parallel()

	teardown := &recordingSlackAppTeardown{}
	step := spawn.NewCreateAppStep(spawn.CreateAppStepDeps{
		RPC:           newFakeSlackAppRPC(newTestCreds()),
		CredsDAO:      spawn.NewMemoryWatchkeeperSlackAppCredsDAO(),
		Teardown:      teardown,
		AppName:       "test-app",
		ApprovalToken: "tok-test",
	})

	sc := newCompensateSpawnContext(t, uuid.New(), uuid.New())
	sc.AgentID = uuid.Nil
	ctx := saga.WithSpawnContext(context.Background(), sc)

	err := step.Compensate(ctx)
	if err == nil {
		t.Fatalf("Compensate: err = nil, want wrapped ErrMissingAgentID")
	}
	if !errors.Is(err, spawn.ErrMissingAgentID) {
		t.Errorf("errors.Is(err, ErrMissingAgentID) = false; got %v", err)
	}
	if got := teardown.callCount.Load(); got != 0 {
		t.Errorf("Teardown call count = %d, want 0 (uuid.Nil AgentID must short-circuit)", got)
	}
}

func TestCreateAppStep_Compensate_TeardownError_WrapsAndPreservesLastErrorClass(t *testing.T) {
	t.Parallel()

	classedErr := &classedTeardownErr{class: "slack_app_teardown_unauthorized"}
	teardown := &recordingSlackAppTeardown{returnErr: classedErr}
	step := spawn.NewCreateAppStep(spawn.CreateAppStepDeps{
		RPC:           newFakeSlackAppRPC(newTestCreds()),
		CredsDAO:      spawn.NewMemoryWatchkeeperSlackAppCredsDAO(),
		Teardown:      teardown,
		AppName:       "test-app",
		ApprovalToken: "tok-test",
	})

	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, uuid.New(), uuid.New()))
	err := step.Compensate(ctx)
	if err == nil {
		t.Fatalf("Compensate: err = nil, want wrapped teardown error")
	}
	if !errors.Is(err, classedErr) {
		t.Errorf("errors.Is(err, classedErr) = false; got %v", err)
	}
	var classed saga.LastErrorClassed
	if !errors.As(err, &classed) {
		t.Fatalf("errors.As(err, &LastErrorClassed) = false; the wrap chain must preserve it")
	}
	if got := classed.LastErrorClass(); got != "slack_app_teardown_unauthorized" {
		t.Errorf("LastErrorClass() = %q, want %q", got, "slack_app_teardown_unauthorized")
	}
}

func TestCreateAppStep_Compensate_PIICanary_NeverLeaksAppNameOrApprovalToken(t *testing.T) {
	t.Parallel()

	piiErr := errors.New(piiCanary)
	teardown := &recordingSlackAppTeardown{returnErr: piiErr}
	step := spawn.NewCreateAppStep(spawn.CreateAppStepDeps{
		RPC:           newFakeSlackAppRPC(newTestCreds()),
		CredsDAO:      spawn.NewMemoryWatchkeeperSlackAppCredsDAO(),
		Teardown:      teardown,
		AppName:       "test-app-pii",
		ApprovalToken: "tok-pii-test",
	})

	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, uuid.New(), uuid.New()))
	err := step.Compensate(ctx)
	if err == nil {
		t.Fatalf("Compensate: err = nil, want wrapped piiErr")
	}
	if !strings.Contains(err.Error(), piiCanary) {
		t.Fatalf("wrap chain dropped the underlying error string: %q", err.Error())
	}
	if strings.Contains(err.Error(), "tok-pii-test") {
		t.Errorf("approval token leaked into wrap chain: %q", err.Error())
	}
	if strings.Contains(err.Error(), "test-app-pii") {
		t.Errorf("app_name leaked into wrap chain: %q", err.Error())
	}
}

func TestCreateAppStep_Compensate_ConcurrentSagas_IsolatedTeardownPerWatchkeeper(t *testing.T) {
	t.Parallel()

	teardown := &recordingSlackAppTeardown{}
	step := spawn.NewCreateAppStep(spawn.CreateAppStepDeps{
		RPC:           newFakeSlackAppRPC(newTestCreds()),
		CredsDAO:      spawn.NewMemoryWatchkeeperSlackAppCredsDAO(),
		Teardown:      teardown,
		AppName:       "test-app",
		ApprovalToken: "tok-test",
	})

	const sagas = 16
	ids := make([]uuid.UUID, sagas)
	for i := range ids {
		ids[i] = uuid.New()
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, id, uuid.New()))
			if err := step.Compensate(ctx); err != nil {
				t.Errorf("Compensate(%v): %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	if got := teardown.callCount.Load(); int(got) != sagas {
		t.Fatalf("Teardown call count = %d, want %d", got, sagas)
	}
	seen := make(map[uuid.UUID]bool, sagas)
	for _, id := range teardown.recorded() {
		if seen[id] {
			t.Errorf("watchkeeperID %v compensated twice; sagas must isolate", id)
		}
		seen[id] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("watchkeeperID %v missing from teardown record set", id)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// OAuthInstallStep.Compensate
// ────────────────────────────────────────────────────────────────────────

func TestOAuthInstallStep_Compensate_HappyPath_RevokeThenWipeAndReturnsNil(t *testing.T) {
	t.Parallel()

	revoker := &recordingOAuthInstallRevoker{}
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	watchkeeperID := uuid.New()

	if err := dao.Put(context.Background(), watchkeeperID, newTestCreds()); err != nil {
		t.Fatalf("dao.Put: %v", err)
	}
	expiresAt, _ := time.Parse(time.RFC3339, "2026-05-10T00:00:00Z")
	if err := dao.PutInstallTokens(
		context.Background(), watchkeeperID, []byte("ct-bot"), nil, nil, expiresAt,
	); err != nil {
		t.Fatalf("dao.PutInstallTokens: %v", err)
	}

	step := newDefaultOAuthInstallStepWithRevoker(t, revoker, dao)

	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, watchkeeperID, uuid.New()))
	if err := step.Compensate(ctx); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if got := revoker.callCount.Load(); got != 1 {
		t.Errorf("Revoke call count = %d, want 1", got)
	}
	if _, _, _, _, _, ok := dao.GetInstallTokens(watchkeeperID); ok {
		t.Errorf("install tokens still present after Wipe; expected absent")
	}
}

func TestOAuthInstallStep_Compensate_RevokeFails_WipeStillRuns_ErrorIsRevoke(t *testing.T) {
	t.Parallel()

	revokeErr := &classedTeardownErr{class: "oauth_install_revoke_unauthorized"}
	revoker := &recordingOAuthInstallRevoker{returnErr: revokeErr}
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	watchkeeperID := uuid.New()
	if err := dao.Put(context.Background(), watchkeeperID, newTestCreds()); err != nil {
		t.Fatalf("dao.Put: %v", err)
	}
	expiresAt, _ := time.Parse(time.RFC3339, "2026-05-10T00:00:00Z")
	if err := dao.PutInstallTokens(
		context.Background(), watchkeeperID, []byte("ct-bot"), nil, nil, expiresAt,
	); err != nil {
		t.Fatalf("dao.PutInstallTokens: %v", err)
	}

	step := newDefaultOAuthInstallStepWithRevoker(t, revoker, dao)
	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, watchkeeperID, uuid.New()))

	err := step.Compensate(ctx)
	if err == nil {
		t.Fatalf("Compensate: err = nil, want wrapped revoke error")
	}
	if !errors.Is(err, revokeErr) {
		t.Errorf("errors.Is(err, revokeErr) = false; got %v", err)
	}
	// Iter-1 critic Major: pin that the typed `LastErrorClass`
	// survives the wipe-mask path. The wipe-always discipline could
	// otherwise lose the revoke's typed class on a future refactor
	// that wraps revokeErr through a non-classed sentinel before
	// returning. The saga.Runner's resolveCompensateLastErrorClass
	// uses errors.As — pin the same surface here.
	var classed saga.LastErrorClassed
	if !errors.As(err, &classed) {
		t.Fatalf("errors.As(err, &LastErrorClassed) = false; wipe-mask path lost typed class")
	}
	if got := classed.LastErrorClass(); got != "oauth_install_revoke_unauthorized" {
		t.Errorf("LastErrorClass() = %q, want %q (revoke class must survive even when wipe also runs)",
			got, "oauth_install_revoke_unauthorized")
	}
	if _, _, _, _, _, ok := dao.GetInstallTokens(watchkeeperID); ok {
		t.Errorf("install tokens still present after Compensate; Wipe must run even on revoke failure")
	}
}

func TestOAuthInstallStep_Compensate_MissingSpawnContext_NoRevokeNoWipe(t *testing.T) {
	t.Parallel()

	revoker := &recordingOAuthInstallRevoker{}
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	step := newDefaultOAuthInstallStepWithRevoker(t, revoker, dao)

	err := step.Compensate(context.Background())
	if err == nil || !errors.Is(err, spawn.ErrMissingSpawnContext) {
		t.Fatalf("Compensate: err = %v, want wrapped ErrMissingSpawnContext", err)
	}
	if got := revoker.callCount.Load(); got != 0 {
		t.Errorf("Revoke call count = %d, want 0 (missing SpawnContext must short-circuit)", got)
	}
}

func TestOAuthInstallStep_Compensate_NilAgentID_NoRevoke(t *testing.T) {
	t.Parallel()

	revoker := &recordingOAuthInstallRevoker{}
	step := newDefaultOAuthInstallStepWithRevoker(t, revoker, spawn.NewMemoryWatchkeeperSlackAppCredsDAO())
	sc := newCompensateSpawnContext(t, uuid.New(), uuid.New())
	sc.AgentID = uuid.Nil
	ctx := saga.WithSpawnContext(context.Background(), sc)
	if err := step.Compensate(ctx); err == nil || !errors.Is(err, spawn.ErrMissingAgentID) {
		t.Fatalf("Compensate: err = %v, want wrapped ErrMissingAgentID", err)
	}
	if got := revoker.callCount.Load(); got != 0 {
		t.Errorf("Revoke call count = %d, want 0", got)
	}
}

func TestOAuthInstallStep_Compensate_PIICanary_NeverLeaksWorkspaceOrRedirectURI(t *testing.T) {
	t.Parallel()

	piiErr := errors.New(piiCanary)
	revoker := &recordingOAuthInstallRevoker{returnErr: piiErr}
	dao := spawn.NewMemoryWatchkeeperSlackAppCredsDAO()
	// Pre-seed so Wipe has a row to clear (the wipe-always path
	// shouldn't add anything to the wrap chain anyway).
	watchkeeperID := uuid.New()
	if err := dao.Put(context.Background(), watchkeeperID, newTestCreds()); err != nil {
		t.Fatalf("dao.Put: %v", err)
	}
	step := spawn.NewOAuthInstallStep(spawn.OAuthInstallStepDeps{
		Installer:   newFakeInstaller(canonicalInstallTokens()),
		CredsDAO:    dao,
		Encrypter:   newTestEncrypter(t),
		Revoker:     revoker,
		Workspace:   messenger.WorkspaceRef{ID: "T-PII-WORKSPACE", Name: "PII-NAME"},
		RedirectURI: "https://pii-redirect.example.com/oauth/callback",
	})

	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, watchkeeperID, uuid.New()))
	err := step.Compensate(ctx)
	if err == nil {
		t.Fatalf("Compensate: err = nil, want wrapped piiErr")
	}
	if !strings.Contains(err.Error(), piiCanary) {
		t.Fatalf("wrap chain dropped the underlying error string: %q", err.Error())
	}
	for _, leak := range []string{"T-PII-WORKSPACE", "PII-NAME", "pii-redirect.example.com"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("step config %q leaked into wrap chain: %q", leak, err.Error())
		}
	}
}

func TestNotebookProvisionStep_Compensate_PIICanary_NeverLeaksProfile(t *testing.T) {
	t.Parallel()

	piiErr := errors.New(piiCanary)
	archiver := &recordingNotebookProvisionArchiver{returnErr: piiErr}
	step := spawn.NewNotebookProvisionStep(spawn.NotebookProvisionStepDeps{
		Provisioner: newFakeNotebookProvisioner(),
		Archiver:    archiver,
		Profile: spawn.NotebookProfile{
			Personality: "PII-PERSONALITY-CANARY",
			Language:    "PII-LANG-CANARY",
		},
	})
	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, uuid.New(), uuid.New()))
	err := step.Compensate(ctx)
	if err == nil {
		t.Fatalf("Compensate: err = nil, want wrapped piiErr")
	}
	if !strings.Contains(err.Error(), piiCanary) {
		t.Fatalf("wrap chain dropped the underlying error string: %q", err.Error())
	}
	for _, leak := range []string{"PII-PERSONALITY-CANARY", "PII-LANG-CANARY"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("step config %q leaked into wrap chain: %q", leak, err.Error())
		}
	}
}

func TestRuntimeLaunchStep_Compensate_PIICanary_NeverLeaksProfileOrIntroText(t *testing.T) {
	t.Parallel()

	piiErr := errors.New(piiCanary)
	teardown := &recordingRuntimeTeardown{returnErr: piiErr}
	step := spawn.NewRuntimeLaunchStep(spawn.RuntimeLaunchStepDeps{
		Launcher: newFakeRuntimeLauncher(),
		Teardown: teardown,
		Profile: spawn.RuntimeLaunchProfile{
			Personality: "PII-PERSONALITY-CANARY",
			Language:    "PII-LANG-CANARY",
			IntroText:   "PII-INTRO-CANARY",
		},
	})
	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, uuid.New(), uuid.New()))
	err := step.Compensate(ctx)
	if err == nil {
		t.Fatalf("Compensate: err = nil, want wrapped piiErr")
	}
	if !strings.Contains(err.Error(), piiCanary) {
		t.Fatalf("wrap chain dropped the underlying error string: %q", err.Error())
	}
	for _, leak := range []string{"PII-PERSONALITY-CANARY", "PII-LANG-CANARY", "PII-INTRO-CANARY"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("step config %q leaked into wrap chain: %q", leak, err.Error())
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// BotProfileStep.Compensate — explicit no-op contract.
// ────────────────────────────────────────────────────────────────────────

func TestBotProfileStep_Compensate_AlwaysReturnsNil_NoSetterCall(t *testing.T) {
	t.Parallel()

	setter := newFakeBotProfileSetter()
	step := newBotProfileStep(t, setter, canonicalBotProfile())

	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, uuid.New(), uuid.New()))
	if err := step.Compensate(ctx); err != nil {
		t.Errorf("Compensate(with ctx): %v, want nil", err)
	}
	// Without SpawnContext — no-op MUST NOT validate the
	// SpawnContext (no work to gate on).
	if err := step.Compensate(context.Background()); err != nil {
		t.Errorf("Compensate(bare ctx): %v, want nil", err)
	}
	if got := setter.callCount.Load(); got != 0 {
		t.Errorf("Setter call count = %d, want 0 (BotProfile.Compensate must NOT touch the platform)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// NotebookProvisionStep.Compensate
// ────────────────────────────────────────────────────────────────────────

func TestNotebookProvisionStep_Compensate_HappyPath_ReturnsNilAfterArchive(t *testing.T) {
	t.Parallel()

	archiver := &recordingNotebookProvisionArchiver{}
	step := newNotebookProvisionStepWithArchiver(t, newFakeNotebookProvisioner(), archiver)
	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, watchkeeperID, uuid.New()))
	if err := step.Compensate(ctx); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if got := archiver.callCount.Load(); got != 1 {
		t.Errorf("Archive call count = %d, want 1", got)
	}
	if calls := archiver.recorded(); len(calls) != 1 || calls[0].watchkeeperID != watchkeeperID {
		t.Errorf("Archive calls = %v, want [{%v, ...}]", calls, watchkeeperID)
	}
}

func TestNotebookProvisionStep_Compensate_ArchiveError_Wraps(t *testing.T) {
	t.Parallel()

	classedErr := &classedTeardownErr{class: "notebook_archive_store_full"}
	archiver := &recordingNotebookProvisionArchiver{returnErr: classedErr}
	step := newNotebookProvisionStepWithArchiver(t, newFakeNotebookProvisioner(), archiver)
	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, uuid.New(), uuid.New()))
	err := step.Compensate(ctx)
	if err == nil || !errors.Is(err, classedErr) {
		t.Fatalf("Compensate: err = %v, want wrapped classedErr", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: notebook_provision step compensate:") {
		t.Errorf("err prefix = %q; want correctly-prefixed wrap", err.Error())
	}
}

func TestNotebookProvisionStep_Compensate_EmptyURI_FailsWithEmptyArchiveURISentinel(t *testing.T) {
	t.Parallel()

	emptyArchiver := &returningArchiver{returnURI: "", returnErr: nil}
	step := newNotebookProvisionStepWithArchiver(t, newFakeNotebookProvisioner(), emptyArchiver)
	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, uuid.New(), uuid.New()))
	err := step.Compensate(ctx)
	if err == nil || !errors.Is(err, spawn.ErrEmptyArchiveURI) {
		t.Fatalf("Compensate: err = %v, want wrapped ErrEmptyArchiveURI", err)
	}
}

func TestNotebookProvisionStep_Compensate_MalformedURI_FailsWithInvalidArchiveURISentinel(t *testing.T) {
	t.Parallel()

	bad := &returningArchiver{returnURI: "garbage-no-scheme", returnErr: nil}
	step := newNotebookProvisionStepWithArchiver(t, newFakeNotebookProvisioner(), bad)
	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, uuid.New(), uuid.New()))
	err := step.Compensate(ctx)
	if err == nil || !errors.Is(err, spawn.ErrInvalidArchiveURI) {
		t.Fatalf("Compensate: err = %v, want wrapped ErrInvalidArchiveURI", err)
	}
}

func TestNotebookProvisionStep_Compensate_MissingSpawnContext_NoArchiverCall(t *testing.T) {
	t.Parallel()

	archiver := &recordingNotebookProvisionArchiver{}
	step := newNotebookProvisionStepWithArchiver(t, newFakeNotebookProvisioner(), archiver)
	err := step.Compensate(context.Background())
	if err == nil || !errors.Is(err, spawn.ErrMissingSpawnContext) {
		t.Fatalf("Compensate: err = %v, want wrapped ErrMissingSpawnContext", err)
	}
	if got := archiver.callCount.Load(); got != 0 {
		t.Errorf("Archive call count = %d, want 0", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// RuntimeLaunchStep.Compensate — manifestVersionID propagation.
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Compensate_HappyPath_ForwardsBothIDs(t *testing.T) {
	t.Parallel()

	teardown := &recordingRuntimeTeardown{}
	step := newRuntimeLaunchStepWithTeardown(t, newFakeRuntimeLauncher(), teardown)
	watchkeeperID := uuid.New()
	manifestVersionID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, watchkeeperID, manifestVersionID))
	if err := step.Compensate(ctx); err != nil {
		t.Fatalf("Compensate: %v", err)
	}
	if got := teardown.callCount.Load(); got != 1 {
		t.Errorf("Teardown call count = %d, want 1", got)
	}
	calls := teardown.recorded()
	if len(calls) != 1 {
		t.Fatalf("Teardown calls = %v, want exactly 1", calls)
	}
	if calls[0].watchkeeperID != watchkeeperID {
		t.Errorf("Teardown.watchkeeperID = %v, want %v", calls[0].watchkeeperID, watchkeeperID)
	}
	if calls[0].manifestVersionID != manifestVersionID {
		t.Errorf("Teardown.manifestVersionID = %v, want %v (M7.3.c forwards both ids)", calls[0].manifestVersionID, manifestVersionID)
	}
}

func TestRuntimeLaunchStep_Compensate_NilManifestVersion_NoTeardownCall(t *testing.T) {
	t.Parallel()

	teardown := &recordingRuntimeTeardown{}
	step := newRuntimeLaunchStepWithTeardown(t, newFakeRuntimeLauncher(), teardown)
	sc := newCompensateSpawnContext(t, uuid.New(), uuid.New())
	sc.ManifestVersionID = uuid.Nil
	ctx := saga.WithSpawnContext(context.Background(), sc)
	err := step.Compensate(ctx)
	if err == nil || !errors.Is(err, spawn.ErrMissingManifestVersion) {
		t.Fatalf("Compensate: err = %v, want wrapped ErrMissingManifestVersion", err)
	}
	if got := teardown.callCount.Load(); got != 0 {
		t.Errorf("Teardown call count = %d, want 0", got)
	}
}

func TestRuntimeLaunchStep_Compensate_TeardownError_Wraps(t *testing.T) {
	t.Parallel()

	classedErr := &classedTeardownErr{class: "runtime_teardown_runtime_stuck"}
	teardown := &recordingRuntimeTeardown{returnErr: classedErr}
	step := newRuntimeLaunchStepWithTeardown(t, newFakeRuntimeLauncher(), teardown)
	ctx := saga.WithSpawnContext(context.Background(), newCompensateSpawnContext(t, uuid.New(), uuid.New()))
	err := step.Compensate(ctx)
	if err == nil || !errors.Is(err, classedErr) {
		t.Fatalf("Compensate: err = %v, want wrapped classedErr", err)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Source-grep AC: every per-step Compensate BODY (NOT the surrounding
// file — Execute paths legitimately emit through other seams) MUST
// NOT call keeperslog.Append directly. The audit chain belongs to the
// saga runner; a Compensate body that emits its own audit row would
// double-emit on the rollback path AND would couple the step's blast
// radius to the keeperslog wire vocabulary.
//
// The scan slices each step file to the Compensate function body
// only — mirrors [TestPerStepCompensate_ReadsSpawnContextFromCtx]'s
// slicing pattern. A whole-file scan would also trip on legitimate
// forward-path emits (e.g. slack_app.go's M6.1.b SlackAppRPC rows),
// and would miss a regression that adds an audit emit through a
// non-keeperslog name to a Compensate body. Iter-1 critic Critical:
// the prior whole-file scan was a "load-bearing AC pin that did not
// pin its claim" — fix mirrors the per-saga-state scan.
// ────────────────────────────────────────────────────────────────────────

func TestPerStepCompensate_DoesNotEmitToKeepersLog(t *testing.T) {
	t.Parallel()

	// The scan list intentionally INCLUDES botprofile_step.go even
	// though that file's Compensate body is `return nil` — a future
	// regression that adds an Append to the no-op MUST trip the AC.
	files := []string{
		"createapp_step.go",
		"oauthinstall_step.go",
		"botprofile_step.go",
		"notebookprovision_step.go",
		"runtimelaunch_step.go",
	}
	forbiddenSubstrings := []string{
		"keeperslog.New(",
		".Append(",
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %q: %v", f, err)
		}
		fnBody, ok := compensateFunctionBody(string(body))
		if !ok {
			t.Errorf("%s: no `Compensate(...)` function body found; cannot enforce no-Append AC", f)
			continue
		}
		for _, line := range strings.Split(fnBody, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, forbidden := range forbiddenSubstrings {
				if strings.Contains(line, forbidden) {
					t.Errorf("%s: Compensate body line contains forbidden audit-emit substring %q: %s",
						f, forbidden, line)
				}
			}
		}
	}
}

// compensateFunctionBody returns the substring of the supplied Go
// source between the `Compensate(...)` function signature and the
// matching closing brace of its top-level function body. Mirrors the
// slicing pattern in [TestPerStepCompensate_ReadsSpawnContextFromCtx]
// and is hoisted to a helper so both source-grep ACs share one
// definition.
//
// The scan window is bounded to the FIRST `Compensate(...)` function
// signature followed by `\n}\n` — this matches the project's gofumpt-
// formatted top-level function shape (closing brace on a line by
// itself, followed by a blank line). A step file with multiple
// `Compensate` declarations (none today; one per step) would require
// a more sophisticated scanner.
func compensateFunctionBody(src string) (string, bool) {
	// Try the canonical "ctx context.Context" shape first; fall back
	// to the discarded-ctx shape for the BotProfile no-op
	// (`func (s *BotProfileStep) Compensate(_ context.Context) error`).
	for _, sig := range []string{
		") Compensate(ctx context.Context) error {",
		") Compensate(_ context.Context) error {",
	} {
		idx := strings.Index(src, sig)
		if idx == -1 {
			continue
		}
		body := src[idx:]
		end := strings.Index(body, "\n}\n")
		if end == -1 {
			continue
		}
		return body[:end], true
	}
	return "", false
}

// ────────────────────────────────────────────────────────────────────────
// Source-grep AC: per-saga state contract pin (M7.3.b lesson #1).
// Every step's Compensate body (except the BotProfile no-op) MUST
// source watchkeeperID from the SpawnContext via
// SpawnContextFromContext(ctx). Pins the "no receiver-stash" rule.
// ────────────────────────────────────────────────────────────────────────

func TestPerStepCompensate_ReadsSpawnContextFromCtx(t *testing.T) {
	t.Parallel()

	// botprofile_step.go is INTENTIONALLY excluded — its Compensate
	// is the documented no-op (returns nil; no SpawnContext read; see
	// botprofile_step.go file-level "rollback contract — explicit
	// no-op" section). Iter-1 critic Minor: when (if) BotProfile
	// becomes a non-no-op, this scan list MUST be widened to include
	// it; otherwise a future regression that adds DAO-touching work
	// to BotProfile.Compensate would silently bypass the per-saga
	// state contract.
	files := []string{
		"createapp_step.go",
		"oauthinstall_step.go",
		"notebookprovision_step.go",
		"runtimelaunch_step.go",
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %q: %v", f, err)
		}
		text := string(body)
		idx := strings.Index(text, ") Compensate(ctx context.Context) error {")
		if idx == -1 {
			t.Errorf("%s: no `Compensate(ctx context.Context) error` body found", f)
			continue
		}
		body2 := text[idx:]
		end := strings.Index(body2, "\n}\n")
		if end == -1 {
			t.Errorf("%s: Compensate body has no closing brace within scan window", f)
			continue
		}
		fn := body2[:end]
		if !strings.Contains(fn, "saga.SpawnContextFromContext(ctx)") {
			t.Errorf("%s: Compensate body does not call SpawnContextFromContext(ctx); per-saga state contract violated", f)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// Test helpers — construct each step with the M7.3.c required
// Compensator-side dep wired via the supplied recording fake so the
// Compensate-focused tests above stay terse.
// ────────────────────────────────────────────────────────────────────────

func newDefaultOAuthInstallStepWithRevoker(t *testing.T, revoker spawn.OAuthInstallRevoker, dao spawn.WatchkeeperSlackAppCredsDAO) *spawn.OAuthInstallStep {
	t.Helper()
	return spawn.NewOAuthInstallStep(spawn.OAuthInstallStepDeps{
		Installer: newFakeInstaller(canonicalInstallTokens()),
		CredsDAO:  dao,
		Encrypter: newTestEncrypter(t),
		Revoker:   revoker,
		Workspace: messenger.WorkspaceRef{ID: "T0123", Name: "Test"},
	})
}

func newNotebookProvisionStepWithArchiver(t *testing.T, provisioner spawn.NotebookProvisioner, archiver spawn.NotebookProvisionArchiver) *spawn.NotebookProvisionStep {
	t.Helper()
	return spawn.NewNotebookProvisionStep(spawn.NotebookProvisionStepDeps{
		Provisioner: provisioner,
		Archiver:    archiver,
		Profile:     canonicalNotebookProfile(),
	})
}

func newRuntimeLaunchStepWithTeardown(t *testing.T, launcher spawn.RuntimeLauncher, teardown spawn.RuntimeTeardown) *spawn.RuntimeLaunchStep {
	t.Helper()
	return spawn.NewRuntimeLaunchStep(spawn.RuntimeLaunchStepDeps{
		Launcher: launcher,
		Teardown: teardown,
		Profile:  canonicalRuntimeLaunchProfile(),
	})
}
