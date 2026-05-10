package spawn_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// ────────────────────────────────────────────────────────────────────────
// Hand-rolled fakes (M3.6 / M6.3.e / M7.1.c.a-.c pattern — no mocking lib).
// ────────────────────────────────────────────────────────────────────────

// fakeNotebookProvisioner records every ProvisionNotebook call onto a
// SHARED record set, optionally returns a configured error to drive
// negative paths. Concurrency: all mutable state lives behind a mutex
// / atomics so concurrent Execute() calls can drive the same fake
// without data races (`go test -race` clean).
type fakeNotebookProvisioner struct {
	mu        sync.Mutex
	calls     []recordedProvisionNotebookCall
	callCount atomic.Int32
	returnErr error
}

type recordedProvisionNotebookCall struct {
	ctx           context.Context
	watchkeeperID uuid.UUID
	profile       spawn.NotebookProfile
}

func newFakeNotebookProvisioner() *fakeNotebookProvisioner {
	return &fakeNotebookProvisioner{}
}

// ProvisionNotebook records the supplied ctx, watchkeeperID, profile.
// Recording the ctx (rather than discarding it) is load-bearing:
// it pins the contract that the step forwards the caller's ctx
// verbatim to the seam, so a future regression to
// `context.Background()` or a derived ctx that strips
// cancellation / values surfaces as a test failure (iter-1 codex
// finding).
func (f *fakeNotebookProvisioner) ProvisionNotebook(ctx context.Context, watchkeeperID uuid.UUID, profile spawn.NotebookProfile) error {
	f.callCount.Add(1)
	f.mu.Lock()
	f.calls = append(f.calls, recordedProvisionNotebookCall{ctx: ctx, watchkeeperID: watchkeeperID, profile: profile})
	f.mu.Unlock()
	return f.returnErr
}

func (f *fakeNotebookProvisioner) recordedCalls() []recordedProvisionNotebookCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedProvisionNotebookCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// canonicalNotebookProfile returns the profile applied across the
// test suite. Mirrors a realistic Watchmaster-derived identity bundle
// (Personality + Language).
func canonicalNotebookProfile() spawn.NotebookProfile {
	return spawn.NotebookProfile{
		Personality: "calm and methodical coordinator focused on backend reliability",
		Language:    "en",
	}
}

func newNotebookSpawnContext(t *testing.T, watchkeeperID uuid.UUID) saga.SpawnContext {
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

func newNotebookProvisionStep(t *testing.T, provisioner spawn.NotebookProvisioner, profile spawn.NotebookProfile) *spawn.NotebookProvisionStep {
	t.Helper()
	return spawn.NewNotebookProvisionStep(spawn.NotebookProvisionStepDeps{
		Provisioner: provisioner,
		Archiver:    nopNotebookProvisionArchiver{},
		Profile:     profile,
	})
}

// ────────────────────────────────────────────────────────────────────────
// Compile-time + Name() coverage
// ────────────────────────────────────────────────────────────────────────

func TestNotebookProvisionStep_Name_ReturnsStableIdentifier(t *testing.T) {
	t.Parallel()

	step := newNotebookProvisionStep(t, newFakeNotebookProvisioner(), canonicalNotebookProfile())
	if got := step.Name(); got != "notebook_provision" {
		t.Errorf("Name() = %q, want %q", got, "notebook_provision")
	}
	if got := step.Name(); got != spawn.NotebookProvisionStepName {
		t.Errorf("Name() = %q, want %q (NotebookProvisionStepName)", got, spawn.NotebookProvisionStepName)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Constructor panics
// ────────────────────────────────────────────────────────────────────────

func TestNewNotebookProvisionStep_PanicsOnNilProvisioner(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewNotebookProvisionStep with nil Provisioner did not panic")
		}
	}()
	_ = spawn.NewNotebookProvisionStep(spawn.NotebookProvisionStepDeps{
		Provisioner: nil,
		Archiver:    nopNotebookProvisionArchiver{},
		Profile:     canonicalNotebookProfile(),
	})
}

func TestNewNotebookProvisionStep_PanicsOnNilArchiver(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewNotebookProvisionStep with nil Archiver did not panic")
		}
	}()
	_ = spawn.NewNotebookProvisionStep(spawn.NotebookProvisionStepDeps{
		Provisioner: newFakeNotebookProvisioner(),
		Archiver:    nil,
		Profile:     canonicalNotebookProfile(),
	})
}

// ────────────────────────────────────────────────────────────────────────
// Happy: Provisioner called once with watchkeeperID + profile; Execute
// returns nil
// ────────────────────────────────────────────────────────────────────────

func TestNotebookProvisionStep_Execute_HappyPath_ForwardsWatchkeeperIDAndProfile(t *testing.T) {
	t.Parallel()

	provisioner := newFakeNotebookProvisioner()
	profile := canonicalNotebookProfile()
	step := newNotebookProvisionStep(t, provisioner, profile)

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newNotebookSpawnContext(t, watchkeeperID))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := provisioner.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Provisioner.ProvisionNotebook call count = %d, want 1", len(calls))
	}
	if calls[0].watchkeeperID != watchkeeperID {
		t.Errorf("call.watchkeeperID = %q, want %q", calls[0].watchkeeperID, watchkeeperID)
	}
	if calls[0].profile.Personality != profile.Personality {
		t.Errorf("call.profile.Personality = %q, want %q", calls[0].profile.Personality, profile.Personality)
	}
	if calls[0].profile.Language != profile.Language {
		t.Errorf("call.profile.Language = %q, want %q", calls[0].profile.Language, profile.Language)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Edge: empty profile is permitted (production Provisioner no-ops on
// empty seed values; the step still dispatches)
// ────────────────────────────────────────────────────────────────────────

func TestNotebookProvisionStep_Execute_EmptyProfile_StillCallsProvisioner(t *testing.T) {
	t.Parallel()

	provisioner := newFakeNotebookProvisioner()
	step := newNotebookProvisionStep(t, provisioner, spawn.NotebookProfile{})

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newNotebookSpawnContext(t, watchkeeperID))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := provisioner.callCount.Load(); got != 1 {
		t.Errorf("Provisioner call count = %d, want 1 (empty profile is a documented no-op at the production wrapper; the step still dispatches)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Missing SpawnContext → wrapped error; no Provisioner call
// ────────────────────────────────────────────────────────────────────────

func TestNotebookProvisionStep_Execute_MissingSpawnContext_NoProvisionerCall(t *testing.T) {
	t.Parallel()

	provisioner := newFakeNotebookProvisioner()
	step := newNotebookProvisionStep(t, provisioner, canonicalNotebookProfile())

	err := step.Execute(context.Background())
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingSpawnContext")
	}
	if !errors.Is(err, spawn.ErrMissingSpawnContext) {
		t.Errorf("errors.Is(err, ErrMissingSpawnContext) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: notebook_provision step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: notebook_provision step:")
	}
	if got := provisioner.callCount.Load(); got != 0 {
		t.Errorf("Provisioner call count = %d, want 0 (missing SpawnContext fails before dispatch)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: AgentID = uuid.Nil → wrapped ErrMissingAgentID; no Provisioner
// call
// ────────────────────────────────────────────────────────────────────────

func TestNotebookProvisionStep_Execute_NilAgentID_NoProvisionerCall(t *testing.T) {
	t.Parallel()

	provisioner := newFakeNotebookProvisioner()
	step := newNotebookProvisionStep(t, provisioner, canonicalNotebookProfile())

	sc := saga.SpawnContext{
		ManifestVersionID: uuid.New(),
		AgentID:           uuid.Nil,
	}
	ctx := saga.WithSpawnContext(context.Background(), sc)

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingAgentID")
	}
	if !errors.Is(err, spawn.ErrMissingAgentID) {
		t.Errorf("errors.Is(err, ErrMissingAgentID) = false; got %v", err)
	}
	if got := provisioner.callCount.Load(); got != 0 {
		t.Errorf("Provisioner call count = %d, want 0", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Provisioner returns error → wrapped + returned
// ────────────────────────────────────────────────────────────────────────

func TestNotebookProvisionStep_Execute_ProvisionerError_WrapsAndReturns(t *testing.T) {
	t.Parallel()

	provisionerErr := errors.New("simulated notebook substrate failure")
	provisioner := newFakeNotebookProvisioner()
	provisioner.returnErr = provisionerErr
	step := newNotebookProvisionStep(t, provisioner, canonicalNotebookProfile())

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newNotebookSpawnContext(t, watchkeeperID))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped Provisioner error")
	}
	if !errors.Is(err, provisionerErr) {
		t.Errorf("errors.Is(err, provisionerErr) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: notebook_provision step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: notebook_provision step:")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Provisioner passthrough preserves typed sentinels (M7.1.c.a/.b.b
// "Reuse sentinel errors across saga steps" lesson — ErrCredsNotFound
// surfaces unchanged through the wrap chain).
// ────────────────────────────────────────────────────────────────────────

func TestNotebookProvisionStep_Execute_PreservesCredsNotFoundSentinel(t *testing.T) {
	t.Parallel()

	provisioner := newFakeNotebookProvisioner()
	provisioner.returnErr = spawn.ErrCredsNotFound
	step := newNotebookProvisionStep(t, provisioner, canonicalNotebookProfile())

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newNotebookSpawnContext(t, watchkeeperID))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrCredsNotFound")
	}
	if !errors.Is(err, spawn.ErrCredsNotFound) {
		t.Errorf("errors.Is(err, ErrCredsNotFound) = false; got %v", err)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: ctx already cancelled → wrapped ctx.Err(); no Provisioner call
// ────────────────────────────────────────────────────────────────────────

func TestNotebookProvisionStep_Execute_CancelledContext_NoProvisionerCall(t *testing.T) {
	t.Parallel()

	provisioner := newFakeNotebookProvisioner()
	step := newNotebookProvisionStep(t, provisioner, canonicalNotebookProfile())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx = saga.WithSpawnContext(ctx, newNotebookSpawnContext(t, uuid.New()))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ctx.Err()")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; got %v", err)
	}
	if got := provisioner.callCount.Load(); got != 0 {
		t.Errorf("Provisioner call count = %d, want 0 (cancellation precedes dispatch)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Security: error strings do NOT leak profile content (Personality /
// Language values) on any failure path. Mirrors M7.1.c.b.b's redaction
// harness.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookProvisionStep_Execute_ErrorPaths_DoNotLeakProfileContent(t *testing.T) {
	t.Parallel()

	//nolint:gosec // G101: synthetic redaction-harness canaries, not real credentials.
	const (
		secretPersonality = "secret-personality-CANARY"
		secretLanguage    = "secret-LANG-CANARY"
	)
	leakProfile := spawn.NotebookProfile{
		Personality: secretPersonality,
		Language:    secretLanguage,
	}
	leakSubstrings := []string{secretPersonality, secretLanguage}

	cases := []struct {
		name  string
		setup func() (step *spawn.NotebookProvisionStep, ctx context.Context)
	}{
		{
			name: "provisioner error",
			setup: func() (*spawn.NotebookProvisionStep, context.Context) {
				p := newFakeNotebookProvisioner()
				p.returnErr = errors.New("substrate fail")
				return newNotebookProvisionStep(t, p, leakProfile), saga.WithSpawnContext(
					context.Background(), newNotebookSpawnContext(t, uuid.New()),
				)
			},
		},
		{
			name: "missing spawn context",
			setup: func() (*spawn.NotebookProvisionStep, context.Context) {
				p := newFakeNotebookProvisioner()
				return newNotebookProvisionStep(t, p, leakProfile), context.Background()
			},
		},
		{
			name: "nil agent id",
			setup: func() (*spawn.NotebookProvisionStep, context.Context) {
				p := newFakeNotebookProvisioner()
				sc := saga.SpawnContext{ManifestVersionID: uuid.New(), AgentID: uuid.Nil}
				return newNotebookProvisionStep(t, p, leakProfile), saga.WithSpawnContext(context.Background(), sc)
			},
		},
		{
			name: "provisioner returns ErrCredsNotFound",
			setup: func() (*spawn.NotebookProvisionStep, context.Context) {
				p := newFakeNotebookProvisioner()
				p.returnErr = spawn.ErrCredsNotFound
				return newNotebookProvisionStep(t, p, leakProfile), saga.WithSpawnContext(
					context.Background(), newNotebookSpawnContext(t, uuid.New()),
				)
			},
		},
		{
			name: "cancelled context",
			setup: func() (*spawn.NotebookProvisionStep, context.Context) {
				p := newFakeNotebookProvisioner()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				ctx = saga.WithSpawnContext(ctx, newNotebookSpawnContext(t, uuid.New()))
				return newNotebookProvisionStep(t, p, leakProfile), ctx
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
			for _, secret := range leakSubstrings {
				if strings.Contains(msg, secret) {
					t.Errorf("error message %q contains secret substring %q (PII leak)", msg, secret)
				}
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────
// Ctx propagation: the seam receives the caller's ctx verbatim.
// Pins the contract that future maintainers cannot quietly swap to
// `context.Background()` or a derived ctx that strips deadlines /
// cancellation / values (iter-1 codex finding — without this pin a
// regression `s.provisioner.ProvisionNotebook(context.Background(), ...)`
// would still pass every other assertion).
// ────────────────────────────────────────────────────────────────────────

func TestNotebookProvisionStep_Execute_PropagatesCallerCtxValueToProvisioner(t *testing.T) {
	t.Parallel()

	type ctxKey struct{}
	sentinel := struct{ tag string }{tag: "iter1-pin"}

	provisioner := newFakeNotebookProvisioner()
	step := newNotebookProvisionStep(t, provisioner, canonicalNotebookProfile())

	ctx := context.WithValue(context.Background(), ctxKey{}, sentinel)
	ctx = saga.WithSpawnContext(ctx, newNotebookSpawnContext(t, uuid.New()))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := provisioner.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Provisioner call count = %d, want 1", len(calls))
	}
	got, ok := calls[0].ctx.Value(ctxKey{}).(struct{ tag string })
	if !ok || got != sentinel {
		t.Errorf("Provisioner.ctx did not carry the WithValue sentinel — step swapped to context.Background or stripped it (got %v, ok=%v)", got, ok)
	}
}

func TestNotebookProvisionStep_Execute_PropagatesCallerCtxCancellationToProvisioner(t *testing.T) {
	t.Parallel()

	provisioner := newFakeNotebookProvisioner()
	step := newNotebookProvisionStep(t, provisioner, canonicalNotebookProfile())

	parent, cancel := context.WithCancel(context.Background())
	ctx := saga.WithSpawnContext(parent, newNotebookSpawnContext(t, uuid.New()))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := provisioner.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Provisioner call count = %d, want 1", len(calls))
	}

	// Cancel the caller's parent context AFTER the call returned. The
	// recorded ctx must observe the cancellation, proving the step
	// did not detach (e.g. via context.Background or a fresh
	// context.WithoutCancel). This is the only post-hoc check that
	// pins cancellation propagation independent of the seam's
	// (synchronous) return value.
	cancel()
	if err := calls[0].ctx.Err(); err != context.Canceled {
		t.Errorf("Provisioner.ctx.Err() after caller cancel = %v, want context.Canceled (step detached the ctx from the caller's cancellation tree)", err)
	}
}

// ────────────────────────────────────────────────────────────────────────
// AC7 + lean-seam: step source does NOT call keeperslog.Writer.Append
// AND does NOT import core/pkg/notebook, core/pkg/messenger, or any
// DAO surface. Pins the M7.1.d "lean saga-step seam" lesson: the step
// stays free of substrate / platform / DAO imports — that work
// belongs to the production [NotebookProvisioner] wrapper.
// ────────────────────────────────────────────────────────────────────────

// TestNotebookProvisionStep_StaysLean mirrors and extends the
// M7.1.c.a / M7.1.c.b.b / M7.1.c.c source-grep AC pin: it reads
// notebookprovision_step.go, strips pure comment lines, then asserts
// the non-comment source contains none of a closed-set of forbidden
// substrings (audit emit, substrate import, platform import, DAO
// type). Stronger than a runtime assertion because it catches any
// future edit that adds a forbidden import or call regardless of
// test wiring (iter-1 codex finding: the original AC only rejected
// `keeperslog.` + `.Append(`, leaving the lesson's broader claim
// unenforced).
func TestNotebookProvisionStep_StaysLean(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("notebookprovision_step.go")
	if err != nil {
		t.Fatalf("read notebookprovision_step.go: %v", err)
	}

	var nonComment strings.Builder
	for _, line := range strings.Split(string(src), "\n") {
		if !strings.HasPrefix(strings.TrimLeft(line, " \t"), "//") {
			nonComment.WriteString(line)
			nonComment.WriteByte('\n')
		}
	}
	body := nonComment.String()

	forbidden := []struct {
		needle string
		reason string
	}{
		{"keeperslog.", "step references keeperslog (audit emit must not live in the step) — AC7 violated"},
		{".Append(", "step contains '.Append(' (audit emit must not live in the step) — AC7 violated"},
		{"core/pkg/notebook", "step imports core/pkg/notebook directly — substrate work belongs in the production wrapper, not the step (M7.1.d lean-seam lesson)"},
		{"core/pkg/messenger", "step imports core/pkg/messenger — platform-specific wiring belongs in the production wrapper, not the step (M7.1.d lean-seam lesson)"},
		{"WatchkeeperSlackAppCredsDAO", "step references the WatchkeeperSlackAppCredsDAO type — DAO surface belongs in the production wrapper, not the step"},
	}
	for _, f := range forbidden {
		if strings.Contains(body, f.needle) {
			t.Errorf("notebookprovision_step.go contains forbidden substring %q in non-comment code: %s", f.needle, f.reason)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// Concurrency: 16 goroutines, distinct watchkeeperIDs, race-detector clean
// ────────────────────────────────────────────────────────────────────────

func TestNotebookProvisionStep_Execute_Concurrency_DistinctWatchkeepers(t *testing.T) {
	t.Parallel()

	provisioner := newFakeNotebookProvisioner()
	step := newNotebookProvisionStep(t, provisioner, canonicalNotebookProfile())

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
			ctx := saga.WithSpawnContext(context.Background(), newNotebookSpawnContext(t, id))
			if err := step.Execute(ctx); err != nil {
				t.Errorf("Execute(%v): %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	if got := provisioner.callCount.Load(); got != n {
		t.Errorf("Provisioner call count = %d, want %d", got, n)
	}
	calls := provisioner.recordedCalls()
	seen := make(map[uuid.UUID]bool, n)
	for _, c := range calls {
		seen[c.watchkeeperID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("watchkeeperID %v missing from recorded calls", id)
		}
	}
}
