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
// Hand-rolled fakes (M3.6 / M6.3.e / M7.1.c.a-.d pattern — no mocking lib).
// ────────────────────────────────────────────────────────────────────────

// fakeRuntimeLauncher records every LaunchRuntime call onto a SHARED
// record set, optionally returns a configured error to drive negative
// paths. Concurrency: all mutable state lives behind a mutex / atomics
// so concurrent Execute() calls can drive the same fake without data
// races (`go test -race` clean).
type fakeRuntimeLauncher struct {
	mu        sync.Mutex
	calls     []recordedLaunchRuntimeCall
	callCount atomic.Int32
	returnErr error
}

type recordedLaunchRuntimeCall struct {
	ctx               context.Context
	watchkeeperID     uuid.UUID
	manifestVersionID uuid.UUID
	profile           spawn.RuntimeLaunchProfile
}

func newFakeRuntimeLauncher() *fakeRuntimeLauncher {
	return &fakeRuntimeLauncher{}
}

// LaunchRuntime records the supplied ctx, watchkeeperID,
// manifestVersionID, profile. Recording the ctx (rather than
// discarding it) is load-bearing: it pins the contract that the step
// forwards the caller's ctx verbatim to the seam, so a future
// regression to `context.Background()` or a derived ctx that strips
// cancellation / values surfaces as a test failure (M7.1.d iter-1
// codex finding generalised here). Recording manifestVersionID pins
// the M7.1.e iter-1 codex finding: the seam must receive the
// saga-pinned manifest version, not re-resolve "current active
// manifest" by watchkeeper id.
func (f *fakeRuntimeLauncher) LaunchRuntime(ctx context.Context, watchkeeperID uuid.UUID, manifestVersionID uuid.UUID, profile spawn.RuntimeLaunchProfile) error {
	f.callCount.Add(1)
	f.mu.Lock()
	f.calls = append(f.calls, recordedLaunchRuntimeCall{
		ctx:               ctx,
		watchkeeperID:     watchkeeperID,
		manifestVersionID: manifestVersionID,
		profile:           profile,
	})
	f.mu.Unlock()
	return f.returnErr
}

func (f *fakeRuntimeLauncher) recordedCalls() []recordedLaunchRuntimeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedLaunchRuntimeCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// canonicalRuntimeLaunchProfile returns the profile applied across the
// test suite. Mirrors a realistic Watchmaster-derived launch bundle
// (Personality + Language + IntroText).
func canonicalRuntimeLaunchProfile() spawn.RuntimeLaunchProfile {
	return spawn.RuntimeLaunchProfile{
		Personality: "calm and methodical coordinator focused on backend reliability",
		Language:    "en",
		IntroText:   "Hello team — I'm online and ready to coordinate.",
	}
}

func newRuntimeLaunchSpawnContext(t *testing.T, watchkeeperID uuid.UUID) saga.SpawnContext {
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

func newRuntimeLaunchStep(t *testing.T, launcher spawn.RuntimeLauncher, profile spawn.RuntimeLaunchProfile) *spawn.RuntimeLaunchStep {
	t.Helper()
	return spawn.NewRuntimeLaunchStep(spawn.RuntimeLaunchStepDeps{
		Launcher: launcher,
		Profile:  profile,
	})
}

// ────────────────────────────────────────────────────────────────────────
// Compile-time + Name() coverage
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Name_ReturnsStableIdentifier(t *testing.T) {
	t.Parallel()

	step := newRuntimeLaunchStep(t, newFakeRuntimeLauncher(), canonicalRuntimeLaunchProfile())
	if got := step.Name(); got != "runtime_launch" {
		t.Errorf("Name() = %q, want %q", got, "runtime_launch")
	}
	if got := step.Name(); got != spawn.RuntimeLaunchStepName {
		t.Errorf("Name() = %q, want %q (RuntimeLaunchStepName)", got, spawn.RuntimeLaunchStepName)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Constructor panics
// ────────────────────────────────────────────────────────────────────────

func TestNewRuntimeLaunchStep_PanicsOnNilLauncher(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewRuntimeLaunchStep with nil Launcher did not panic")
		}
	}()
	_ = spawn.NewRuntimeLaunchStep(spawn.RuntimeLaunchStepDeps{
		Launcher: nil,
		Profile:  canonicalRuntimeLaunchProfile(),
	})
}

// ────────────────────────────────────────────────────────────────────────
// Happy: Launcher called once with watchkeeperID + profile; Execute
// returns nil
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Execute_HappyPath_ForwardsWatchkeeperIDAndProfile(t *testing.T) {
	t.Parallel()

	launcher := newFakeRuntimeLauncher()
	profile := canonicalRuntimeLaunchProfile()
	step := newRuntimeLaunchStep(t, launcher, profile)

	watchkeeperID := uuid.New()
	sc := newRuntimeLaunchSpawnContext(t, watchkeeperID)
	ctx := saga.WithSpawnContext(context.Background(), sc)

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := launcher.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Launcher.LaunchRuntime call count = %d, want 1", len(calls))
	}
	if calls[0].watchkeeperID != watchkeeperID {
		t.Errorf("call.watchkeeperID = %q, want %q", calls[0].watchkeeperID, watchkeeperID)
	}
	if calls[0].manifestVersionID != sc.ManifestVersionID {
		t.Errorf("call.manifestVersionID = %q, want %q (must forward saga-pinned manifest version, NOT re-resolve current)", calls[0].manifestVersionID, sc.ManifestVersionID)
	}
	if calls[0].profile.Personality != profile.Personality {
		t.Errorf("call.profile.Personality = %q, want %q", calls[0].profile.Personality, profile.Personality)
	}
	if calls[0].profile.Language != profile.Language {
		t.Errorf("call.profile.Language = %q, want %q", calls[0].profile.Language, profile.Language)
	}
	if calls[0].profile.IntroText != profile.IntroText {
		t.Errorf("call.profile.IntroText = %q, want %q", calls[0].profile.IntroText, profile.IntroText)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Edge: empty profile is permitted (production Launcher partial-no-ops
// on empty seed values; the step still dispatches)
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Execute_EmptyProfile_StillCallsLauncher(t *testing.T) {
	t.Parallel()

	launcher := newFakeRuntimeLauncher()
	step := newRuntimeLaunchStep(t, launcher, spawn.RuntimeLaunchProfile{})

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newRuntimeLaunchSpawnContext(t, watchkeeperID))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got := launcher.callCount.Load(); got != 1 {
		t.Errorf("Launcher call count = %d, want 1 (empty profile is a documented no-op at the production wrapper; the step still dispatches)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Missing SpawnContext → wrapped error; no Launcher call
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Execute_MissingSpawnContext_NoLauncherCall(t *testing.T) {
	t.Parallel()

	launcher := newFakeRuntimeLauncher()
	step := newRuntimeLaunchStep(t, launcher, canonicalRuntimeLaunchProfile())

	err := step.Execute(context.Background())
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingSpawnContext")
	}
	if !errors.Is(err, spawn.ErrMissingSpawnContext) {
		t.Errorf("errors.Is(err, ErrMissingSpawnContext) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: runtime_launch step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: runtime_launch step:")
	}
	if got := launcher.callCount.Load(); got != 0 {
		t.Errorf("Launcher call count = %d, want 0 (missing SpawnContext fails before dispatch)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: ManifestVersionID = uuid.Nil → wrapped ErrMissingManifestVersion;
// no Launcher call. Pins the M7.1.e iter-1 codex finding: the saga's
// manifest-version pin is load-bearing for runtime determinism — a
// uuid.Nil here would force the wrapper to fall back to "current active
// manifest" and silently boot the wrong runtime config.
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Execute_NilManifestVersionID_NoLauncherCall(t *testing.T) {
	t.Parallel()

	launcher := newFakeRuntimeLauncher()
	step := newRuntimeLaunchStep(t, launcher, canonicalRuntimeLaunchProfile())

	sc := saga.SpawnContext{
		ManifestVersionID: uuid.Nil,
		AgentID:           uuid.New(),
	}
	ctx := saga.WithSpawnContext(context.Background(), sc)

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingManifestVersion")
	}
	if !errors.Is(err, spawn.ErrMissingManifestVersion) {
		t.Errorf("errors.Is(err, ErrMissingManifestVersion) = false; got %v", err)
	}
	if got := launcher.callCount.Load(); got != 0 {
		t.Errorf("Launcher call count = %d, want 0 (nil manifest version fails before dispatch)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: AgentID = uuid.Nil → wrapped ErrMissingAgentID; no Launcher
// call
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Execute_NilAgentID_NoLauncherCall(t *testing.T) {
	t.Parallel()

	launcher := newFakeRuntimeLauncher()
	step := newRuntimeLaunchStep(t, launcher, canonicalRuntimeLaunchProfile())

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
	if got := launcher.callCount.Load(); got != 0 {
		t.Errorf("Launcher call count = %d, want 0", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Launcher returns error → wrapped + returned
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Execute_LauncherError_WrapsAndReturns(t *testing.T) {
	t.Parallel()

	launcherErr := errors.New("simulated runtime boot failure")
	launcher := newFakeRuntimeLauncher()
	launcher.returnErr = launcherErr
	step := newRuntimeLaunchStep(t, launcher, canonicalRuntimeLaunchProfile())

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newRuntimeLaunchSpawnContext(t, watchkeeperID))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped Launcher error")
	}
	if !errors.Is(err, launcherErr) {
		t.Errorf("errors.Is(err, launcherErr) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: runtime_launch step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: runtime_launch step:")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Launcher passthrough preserves typed sentinels (M7.1.c.a/.b.b/.d
// "Reuse sentinel errors across saga steps" lesson — ErrCredsNotFound
// surfaces unchanged through the wrap chain).
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Execute_PreservesCredsNotFoundSentinel(t *testing.T) {
	t.Parallel()

	launcher := newFakeRuntimeLauncher()
	launcher.returnErr = spawn.ErrCredsNotFound
	step := newRuntimeLaunchStep(t, launcher, canonicalRuntimeLaunchProfile())

	watchkeeperID := uuid.New()
	ctx := saga.WithSpawnContext(context.Background(), newRuntimeLaunchSpawnContext(t, watchkeeperID))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrCredsNotFound")
	}
	if !errors.Is(err, spawn.ErrCredsNotFound) {
		t.Errorf("errors.Is(err, ErrCredsNotFound) = false; got %v", err)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: ctx already cancelled → wrapped ctx.Err(); no Launcher call
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Execute_CancelledContext_NoLauncherCall(t *testing.T) {
	t.Parallel()

	launcher := newFakeRuntimeLauncher()
	step := newRuntimeLaunchStep(t, launcher, canonicalRuntimeLaunchProfile())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx = saga.WithSpawnContext(ctx, newRuntimeLaunchSpawnContext(t, uuid.New()))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ctx.Err()")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; got %v", err)
	}
	if got := launcher.callCount.Load(); got != 0 {
		t.Errorf("Launcher call count = %d, want 0 (cancellation precedes dispatch)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Security: error strings do NOT leak profile content (Personality /
// Language / IntroText values) on any failure path. Mirrors M7.1.d's
// redaction harness.
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Execute_ErrorPaths_DoNotLeakProfileContent(t *testing.T) {
	t.Parallel()

	//nolint:gosec // G101: synthetic redaction-harness canaries, not real credentials.
	const (
		secretPersonality = "secret-personality-CANARY"
		secretLanguage    = "secret-LANG-CANARY"
		secretIntroText   = "secret-INTRO-CANARY"
	)
	leakProfile := spawn.RuntimeLaunchProfile{
		Personality: secretPersonality,
		Language:    secretLanguage,
		IntroText:   secretIntroText,
	}
	leakSubstrings := []string{secretPersonality, secretLanguage, secretIntroText}

	cases := []struct {
		name  string
		setup func() (step *spawn.RuntimeLaunchStep, ctx context.Context)
	}{
		{
			name: "launcher error",
			setup: func() (*spawn.RuntimeLaunchStep, context.Context) {
				l := newFakeRuntimeLauncher()
				l.returnErr = errors.New("substrate fail")
				return newRuntimeLaunchStep(t, l, leakProfile), saga.WithSpawnContext(
					context.Background(), newRuntimeLaunchSpawnContext(t, uuid.New()),
				)
			},
		},
		{
			name: "missing spawn context",
			setup: func() (*spawn.RuntimeLaunchStep, context.Context) {
				l := newFakeRuntimeLauncher()
				return newRuntimeLaunchStep(t, l, leakProfile), context.Background()
			},
		},
		{
			name: "nil manifest version id",
			setup: func() (*spawn.RuntimeLaunchStep, context.Context) {
				l := newFakeRuntimeLauncher()
				sc := saga.SpawnContext{ManifestVersionID: uuid.Nil, AgentID: uuid.New()}
				return newRuntimeLaunchStep(t, l, leakProfile), saga.WithSpawnContext(context.Background(), sc)
			},
		},
		{
			name: "nil agent id",
			setup: func() (*spawn.RuntimeLaunchStep, context.Context) {
				l := newFakeRuntimeLauncher()
				sc := saga.SpawnContext{ManifestVersionID: uuid.New(), AgentID: uuid.Nil}
				return newRuntimeLaunchStep(t, l, leakProfile), saga.WithSpawnContext(context.Background(), sc)
			},
		},
		{
			name: "launcher returns ErrCredsNotFound",
			setup: func() (*spawn.RuntimeLaunchStep, context.Context) {
				l := newFakeRuntimeLauncher()
				l.returnErr = spawn.ErrCredsNotFound
				return newRuntimeLaunchStep(t, l, leakProfile), saga.WithSpawnContext(
					context.Background(), newRuntimeLaunchSpawnContext(t, uuid.New()),
				)
			},
		},
		{
			name: "cancelled context",
			setup: func() (*spawn.RuntimeLaunchStep, context.Context) {
				l := newFakeRuntimeLauncher()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				ctx = saga.WithSpawnContext(ctx, newRuntimeLaunchSpawnContext(t, uuid.New()))
				return newRuntimeLaunchStep(t, l, leakProfile), ctx
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
// Ctx propagation: the seam receives the caller's ctx verbatim. Pins
// the contract that future maintainers cannot quietly swap to
// `context.Background()` or a derived ctx that strips deadlines /
// cancellation / values (M7.1.d iter-1 codex finding generalised here).
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Execute_PropagatesCallerCtxValueToLauncher(t *testing.T) {
	t.Parallel()

	type ctxKey struct{}
	sentinel := struct{ tag string }{tag: "iter1-pin"}

	launcher := newFakeRuntimeLauncher()
	step := newRuntimeLaunchStep(t, launcher, canonicalRuntimeLaunchProfile())

	ctx := context.WithValue(context.Background(), ctxKey{}, sentinel)
	ctx = saga.WithSpawnContext(ctx, newRuntimeLaunchSpawnContext(t, uuid.New()))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := launcher.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Launcher call count = %d, want 1", len(calls))
	}
	got, ok := calls[0].ctx.Value(ctxKey{}).(struct{ tag string })
	if !ok || got != sentinel {
		t.Errorf("Launcher.ctx did not carry the WithValue sentinel — step swapped to context.Background or stripped it (got %v, ok=%v)", got, ok)
	}
}

func TestRuntimeLaunchStep_Execute_PropagatesCallerCtxCancellationToLauncher(t *testing.T) {
	t.Parallel()

	launcher := newFakeRuntimeLauncher()
	step := newRuntimeLaunchStep(t, launcher, canonicalRuntimeLaunchProfile())

	parent, cancel := context.WithCancel(context.Background())
	ctx := saga.WithSpawnContext(parent, newRuntimeLaunchSpawnContext(t, uuid.New()))

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := launcher.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Launcher call count = %d, want 1", len(calls))
	}

	// Cancel the caller's parent context AFTER the call returned. The
	// recorded ctx must observe the cancellation, proving the step
	// did not detach (e.g. via context.Background or a fresh
	// context.WithoutCancel). This is the only post-hoc check that
	// pins cancellation propagation independent of the seam's
	// (synchronous) return value.
	cancel()
	if err := calls[0].ctx.Err(); err != context.Canceled {
		t.Errorf("Launcher.ctx.Err() after caller cancel = %v, want context.Canceled (step detached the ctx from the caller's cancellation tree)", err)
	}
}

// ────────────────────────────────────────────────────────────────────────
// AC7 + lean-seam: step source does NOT call keeperslog.Writer.Append
// AND does NOT import core/pkg/runtime, core/pkg/messenger, core/pkg/notebook,
// or any DAO surface. Pins the M7.1.d "lean saga-step seam" lesson
// extended for M7.1.e: the step stays free of substrate / platform /
// DAO imports — that work belongs to the production [RuntimeLauncher]
// wrapper.
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_StaysLean(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("runtimelaunch_step.go")
	if err != nil {
		t.Fatalf("read runtimelaunch_step.go: %v", err)
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
		{"core/pkg/runtime", "step imports core/pkg/runtime directly — runtime substrate work belongs in the production wrapper, not the step (M7.1.e lean-seam lesson)"},
		{"core/pkg/messenger", "step imports core/pkg/messenger — platform-specific intro-post wiring belongs in the production wrapper, not the step (M7.1.e lean-seam lesson)"},
		{"core/pkg/notebook", "step imports core/pkg/notebook — substrate work belongs in the production wrapper, not the step (M7.1.d lean-seam lesson carried forward)"},
		{"WatchkeeperSlackAppCredsDAO", "step references the WatchkeeperSlackAppCredsDAO type — DAO surface belongs in the production wrapper, not the step"},
	}
	for _, f := range forbidden {
		if strings.Contains(body, f.needle) {
			t.Errorf("runtimelaunch_step.go contains forbidden substring %q in non-comment code: %s", f.needle, f.reason)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// Concurrency: 16 goroutines, distinct watchkeeperIDs, race-detector clean
// ────────────────────────────────────────────────────────────────────────

func TestRuntimeLaunchStep_Execute_Concurrency_DistinctWatchkeepers(t *testing.T) {
	t.Parallel()

	launcher := newFakeRuntimeLauncher()
	step := newRuntimeLaunchStep(t, launcher, canonicalRuntimeLaunchProfile())

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
			ctx := saga.WithSpawnContext(context.Background(), newRuntimeLaunchSpawnContext(t, id))
			if err := step.Execute(ctx); err != nil {
				t.Errorf("Execute(%v): %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	if got := launcher.callCount.Load(); got != n {
		t.Errorf("Launcher call count = %d, want %d", got, n)
	}
	calls := launcher.recordedCalls()
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
