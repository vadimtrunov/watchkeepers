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
// Hand-rolled fakes (M3.6 / M6.3.e / M7.1.c-.e / M7.2.b pattern — no
// mocking lib).
// ────────────────────────────────────────────────────────────────────────

// fakeWatchkeeperRetirer records every MarkRetired call onto a shared
// record set, optionally returns a configured error to drive negative
// paths. Concurrency: all mutable state lives behind a mutex / atomics
// so concurrent Execute() calls can drive the same fake without data
// races (`go test -race` clean).
type fakeWatchkeeperRetirer struct {
	mu        sync.Mutex
	calls     []recordedRetireCall
	callCount atomic.Int32
	returnErr error
}

type recordedRetireCall struct {
	ctx           context.Context
	watchkeeperID uuid.UUID
	archiveURI    string
}

func newFakeWatchkeeperRetirer() *fakeWatchkeeperRetirer {
	return &fakeWatchkeeperRetirer{}
}

// MarkRetired records the supplied ctx + watchkeeperID + archiveURI.
// Recording the ctx (rather than discarding it) is load-bearing per
// the M7.1.d / M7.1.e / M7.2.b ctx-propagation lesson: it pins the
// contract that the step forwards the caller's ctx verbatim to the
// seam, so a future regression to `context.Background()` or a derived
// ctx that strips cancellation / values surfaces as a test failure.
func (f *fakeWatchkeeperRetirer) MarkRetired(ctx context.Context, watchkeeperID uuid.UUID, archiveURI string) error {
	f.callCount.Add(1)
	f.mu.Lock()
	f.calls = append(f.calls, recordedRetireCall{ctx: ctx, watchkeeperID: watchkeeperID, archiveURI: archiveURI})
	f.mu.Unlock()
	return f.returnErr
}

func (f *fakeWatchkeeperRetirer) recordedCalls() []recordedRetireCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedRetireCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func newMarkRetiredSpawnContext(t *testing.T, watchkeeperID uuid.UUID) saga.SpawnContext {
	t.Helper()
	return saga.SpawnContext{
		ManifestVersionID: uuid.New(),
		AgentID:           watchkeeperID,
		Claim: saga.SpawnClaim{
			OrganizationID:  "org-test",
			AgentID:         "agent-watchmaster",
			AuthorityMatrix: map[string]string{"retire_watchkeeper": "lead_approval"},
		},
	}
}

// newMarkRetiredCtx seeds both the SpawnContext (with the supplied
// watchkeeperID as AgentID) and the RetireResult outbox (with the
// supplied archiveURI already published, mirroring the post-M7.2.b
// state). Returns the context and the result pointer so the test
// can also assert outbox immutability if needed.
func newMarkRetiredCtx(t *testing.T, watchkeeperID uuid.UUID, archiveURI string) (context.Context, *saga.RetireResult) {
	t.Helper()
	result := &saga.RetireResult{ArchiveURI: archiveURI}
	ctx := saga.WithSpawnContext(context.Background(), newMarkRetiredSpawnContext(t, watchkeeperID))
	ctx = saga.WithRetireResult(ctx, result)
	return ctx, result
}

func newMarkRetiredStep(t *testing.T, retirer spawn.WatchkeeperRetirer) *spawn.MarkRetiredStep {
	t.Helper()
	return spawn.NewMarkRetiredStep(spawn.MarkRetiredStepDeps{
		Retirer: retirer,
	})
}

// ────────────────────────────────────────────────────────────────────────
// Compile-time + Name() coverage
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Name_ReturnsStableIdentifier(t *testing.T) {
	t.Parallel()

	step := newMarkRetiredStep(t, newFakeWatchkeeperRetirer())
	if got := step.Name(); got != "mark_retired" {
		t.Errorf("Name() = %q, want %q", got, "mark_retired")
	}
	if got := step.Name(); got != spawn.MarkRetiredStepName {
		t.Errorf("Name() = %q, want %q (MarkRetiredStepName)", got, spawn.MarkRetiredStepName)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Constructor panics
// ────────────────────────────────────────────────────────────────────────

func TestNewMarkRetiredStep_PanicsOnNilRetirer(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewMarkRetiredStep with nil Retirer did not panic")
		}
	}()
	_ = spawn.NewMarkRetiredStep(spawn.MarkRetiredStepDeps{Retirer: nil})
}

// ────────────────────────────────────────────────────────────────────────
// Happy path — Retirer called once with watchkeeperID + archiveURI;
// Execute returns nil.
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Execute_HappyPath_ForwardsArgsToRetirer(t *testing.T) {
	t.Parallel()

	retirer := newFakeWatchkeeperRetirer()
	step := newMarkRetiredStep(t, retirer)

	const wantURI = "file:///snapshots/wk-happy/2026-05-09T12-34-56Z.tar.gz"
	watchkeeperID := uuid.New()
	ctx, _ := newMarkRetiredCtx(t, watchkeeperID, wantURI)

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := retirer.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Retirer.MarkRetired call count = %d, want 1", len(calls))
	}
	if calls[0].watchkeeperID != watchkeeperID {
		t.Errorf("call.watchkeeperID = %q, want %q", calls[0].watchkeeperID, watchkeeperID)
	}
	if calls[0].archiveURI != wantURI {
		t.Errorf("call.archiveURI = %q, want %q (step must forward the URI from RetireResult.ArchiveURI verbatim)",
			calls[0].archiveURI, wantURI)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Missing SpawnContext → wrapped error; no Retirer call.
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Execute_MissingSpawnContext_NoRetirerCall(t *testing.T) {
	t.Parallel()

	retirer := newFakeWatchkeeperRetirer()
	step := newMarkRetiredStep(t, retirer)

	// Seed RetireResult but NOT SpawnContext — the step must reject
	// before dispatch.
	result := &saga.RetireResult{ArchiveURI: "file:///dummy.tar.gz"}
	ctx := saga.WithRetireResult(context.Background(), result)

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingSpawnContext")
	}
	if !errors.Is(err, spawn.ErrMissingSpawnContext) {
		t.Errorf("errors.Is(err, ErrMissingSpawnContext) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: mark_retired step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: mark_retired step:")
	}
	if got := retirer.callCount.Load(); got != 0 {
		t.Errorf("Retirer call count = %d, want 0 (missing SpawnContext fails before dispatch)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: AgentID = uuid.Nil → wrapped ErrMissingAgentID; no Retirer
// call.
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Execute_NilAgentID_NoRetirerCall(t *testing.T) {
	t.Parallel()

	retirer := newFakeWatchkeeperRetirer()
	step := newMarkRetiredStep(t, retirer)

	sc := saga.SpawnContext{ManifestVersionID: uuid.New(), AgentID: uuid.Nil}
	result := &saga.RetireResult{ArchiveURI: "file:///dummy.tar.gz"}
	ctx := saga.WithSpawnContext(context.Background(), sc)
	ctx = saga.WithRetireResult(ctx, result)

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingAgentID")
	}
	if !errors.Is(err, spawn.ErrMissingAgentID) {
		t.Errorf("errors.Is(err, ErrMissingAgentID) = false; got %v", err)
	}
	if got := retirer.callCount.Load(); got != 0 {
		t.Errorf("Retirer call count = %d, want 0", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Missing RetireResult outbox → wrapped ErrMissingRetireResult
// (sentinel reused from M7.2.b per the saga-family one-sentinel-per-class
// rule); no Retirer call.
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Execute_MissingRetireResult_NoRetirerCall(t *testing.T) {
	t.Parallel()

	retirer := newFakeWatchkeeperRetirer()
	step := newMarkRetiredStep(t, retirer)

	// Seed SpawnContext but NOT RetireResult.
	ctx := saga.WithSpawnContext(context.Background(), newMarkRetiredSpawnContext(t, uuid.New()))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingRetireResult")
	}
	if !errors.Is(err, spawn.ErrMissingRetireResult) {
		t.Errorf("errors.Is(err, ErrMissingRetireResult) = false; got %v", err)
	}
	if got := retirer.callCount.Load(); got != 0 {
		t.Errorf("Retirer call count = %d, want 0 (missing RetireResult fails before dispatch)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: RetireResult.ArchiveURI is empty → wrapped
// ErrMissingArchiveURI; no Retirer call. Pins the M7.2.c contract that
// the upstream M7.2.b producer must publish a non-empty URI; an empty
// value here is a wiring / step-ordering bug.
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Execute_EmptyArchiveURI_NoRetirerCall(t *testing.T) {
	t.Parallel()

	retirer := newFakeWatchkeeperRetirer()
	step := newMarkRetiredStep(t, retirer)

	// Seed both SpawnContext + RetireResult but leave ArchiveURI empty.
	ctx, _ := newMarkRetiredCtx(t, uuid.New(), "")

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingArchiveURI")
	}
	if !errors.Is(err, spawn.ErrMissingArchiveURI) {
		t.Errorf("errors.Is(err, ErrMissingArchiveURI) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: mark_retired step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: mark_retired step:")
	}
	if got := retirer.callCount.Load(); got != 0 {
		t.Errorf("Retirer call count = %d, want 0 (missing ArchiveURI fails before dispatch)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Retirer returns error → wrapped + returned to the runner.
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Execute_RetirerError_WrapsAndReturns(t *testing.T) {
	t.Parallel()

	retirerErr := errors.New("simulated keepclient transport failure")
	retirer := newFakeWatchkeeperRetirer()
	retirer.returnErr = retirerErr
	step := newMarkRetiredStep(t, retirer)

	ctx, _ := newMarkRetiredCtx(t, uuid.New(), "file:///snapshots/wk/2026-05-09T12-34-56Z.tar.gz")

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped Retirer error")
	}
	if !errors.Is(err, retirerErr) {
		t.Errorf("errors.Is(err, retirerErr) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: mark_retired step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: mark_retired step:")
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Retirer passthrough preserves typed sentinels (M7.1.c.b.b /
// M7.1.d / M7.1.e / M7.2.b "Reuse sentinel errors across saga steps"
// lesson). For M7.2.c the load-bearing sentinel is
// [keepclient.ErrInvalidStatusTransition] surfacing on a non-active row;
// stand in with [ErrCredsNotFound] (already imported via the spawn
// package) so the test does not introduce a cross-package dependency
// for the unit-test target. The rule is the same: any typed error
// from the seam survives the wrap chain unchanged via errors.Is.
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Execute_PreservesTypedSentinel(t *testing.T) {
	t.Parallel()

	retirer := newFakeWatchkeeperRetirer()
	retirer.returnErr = spawn.ErrCredsNotFound
	step := newMarkRetiredStep(t, retirer)

	ctx, _ := newMarkRetiredCtx(t, uuid.New(), "file:///snapshots/wk/2026-05-09T12-34-56Z.tar.gz")

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrCredsNotFound")
	}
	if !errors.Is(err, spawn.ErrCredsNotFound) {
		t.Errorf("errors.Is(err, ErrCredsNotFound) = false; got %v", err)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: ctx already cancelled → wrapped ctx.Err(); no Retirer call.
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Execute_CancelledContext_NoRetirerCall(t *testing.T) {
	t.Parallel()

	retirer := newFakeWatchkeeperRetirer()
	step := newMarkRetiredStep(t, retirer)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx = saga.WithSpawnContext(ctx, newMarkRetiredSpawnContext(t, uuid.New()))
	ctx = saga.WithRetireResult(ctx, &saga.RetireResult{ArchiveURI: "file:///dummy.tar.gz"})

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ctx.Err()")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; got %v", err)
	}
	if got := retirer.callCount.Load(); got != 0 {
		t.Errorf("Retirer call count = %d, want 0 (cancellation precedes dispatch)", got)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Ctx propagation: the seam receives the caller's ctx verbatim. Pins
// the contract that future maintainers cannot quietly swap to
// `context.Background()` or a derived ctx that strips deadlines /
// cancellation / values (M7.1.d / M7.1.e / M7.2.b ctx-propagation
// lesson).
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Execute_PropagatesCallerCtxValueToRetirer(t *testing.T) {
	t.Parallel()

	type ctxKey struct{}
	sentinel := struct{ tag string }{tag: "iter1-pin"}

	retirer := newFakeWatchkeeperRetirer()
	step := newMarkRetiredStep(t, retirer)

	ctx := context.WithValue(context.Background(), ctxKey{}, sentinel)
	ctx = saga.WithSpawnContext(ctx, newMarkRetiredSpawnContext(t, uuid.New()))
	ctx = saga.WithRetireResult(ctx, &saga.RetireResult{ArchiveURI: "file:///dummy.tar.gz"})

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := retirer.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Retirer call count = %d, want 1", len(calls))
	}
	got, ok := calls[0].ctx.Value(ctxKey{}).(struct{ tag string })
	if !ok || got != sentinel {
		t.Errorf("Retirer.ctx did not carry the WithValue sentinel — step swapped to context.Background or stripped it (got %v, ok=%v)", got, ok)
	}
}

func TestMarkRetiredStep_Execute_PropagatesCallerCtxCancellationToRetirer(t *testing.T) {
	t.Parallel()

	retirer := newFakeWatchkeeperRetirer()
	step := newMarkRetiredStep(t, retirer)

	parent, cancel := context.WithCancel(context.Background())
	ctx := saga.WithSpawnContext(parent, newMarkRetiredSpawnContext(t, uuid.New()))
	ctx = saga.WithRetireResult(ctx, &saga.RetireResult{ArchiveURI: "file:///dummy.tar.gz"})

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := retirer.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Retirer call count = %d, want 1", len(calls))
	}

	cancel()
	if err := calls[0].ctx.Err(); err != context.Canceled {
		t.Errorf("Retirer.ctx.Err() after caller cancel = %v, want context.Canceled (step detached the ctx from the caller's cancellation tree)", err)
	}
}

// ────────────────────────────────────────────────────────────────────────
// AC7 + lean-seam: step source does NOT call keeperslog.Writer.Append
// AND does NOT import core/pkg/notebook, core/pkg/archivestore,
// core/pkg/messenger, core/pkg/runtime, core/pkg/keepclient, or any
// DAO surface. Pins the M7.1.d / M7.1.e / M7.2.b "lean saga-step seam"
// lesson: the step stays free of substrate / platform / DAO / client
// imports — that work belongs to the production [WatchkeeperRetirer]
// wrapper.
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_StaysLean(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("markretired_step.go")
	if err != nil {
		t.Fatalf("read markretired_step.go: %v", err)
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
		{"core/pkg/archivestore", "step imports core/pkg/archivestore directly — backend-store work belongs in the production wrapper, not the step (M7.2.b lean-seam lesson)"},
		{"core/pkg/messenger", "step imports core/pkg/messenger — platform-specific wiring belongs in the production wrapper, not the step (M7.1.d lean-seam lesson)"},
		{"core/pkg/runtime", "step imports core/pkg/runtime — harness wiring belongs in the production wrapper, not the step (M7.1.e lean-seam lesson)"},
		{"core/pkg/keepclient", "step imports core/pkg/keepclient — keep-client wiring belongs in the production wrapper, not the step (M7.2.c lean-seam lesson)"},
		{"WatchkeeperSlackAppCredsDAO", "step references the WatchkeeperSlackAppCredsDAO type — DAO surface belongs in the production wrapper, not the step"},
	}
	for _, f := range forbidden {
		if strings.Contains(body, f.needle) {
			t.Errorf("markretired_step.go contains forbidden substring %q in non-comment code: %s", f.needle, f.reason)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// Sentinel distinctness — pin that the M7.2.c-introduced sentinel
// (ErrMissingArchiveURI) is distinct from the upstream M7.2.b
// sentinels under errors.Is. A future refactor that aliases two of
// them onto a common type would break differentiation at the M7.3
// compensator boundary; this test catches it.
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Sentinels_DistinctFromM72bFamily(t *testing.T) {
	t.Parallel()

	pairs := []struct {
		a, b error
		name string
	}{
		{spawn.ErrMissingArchiveURI, spawn.ErrMissingRetireResult, "missing-uri-vs-missing-outbox"},
		{spawn.ErrMissingArchiveURI, spawn.ErrEmptyArchiveURI, "missing-uri-vs-empty-on-success"},
		{spawn.ErrMissingArchiveURI, spawn.ErrInvalidArchiveURI, "missing-uri-vs-malformed-on-success"},
	}
	for _, p := range pairs {
		if errors.Is(p.a, p.b) || errors.Is(p.b, p.a) {
			t.Errorf("%s: errors.Is unexpectedly aliased the two sentinels", p.name)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// PII canary redaction harness mirroring M7.1.c.b.b / M7.1.d / M7.1.e /
// M7.2.b. The step's doc-block claims (a) the archive URI is the
// load-bearing payload, NOT something the step embeds in failure error
// strings; AND (b) the watchkeeperID is already on the saga audit chain
// via SpawnContext.AgentID, so the step does not re-leak it through
// error messages. Without this harness those claims have no test
// enforcement: a future maintainer who decorates the wrap chain with
// `uri=%q` or `wk=%v` for "richer diagnostics" would slip past every
// other test undetected. The harness drives Execute through every
// error-string-building path with canary substrings in the
// watchkeeperID + archiveURI and asserts neither leaks via err.Error().
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Execute_ErrorPaths_DoNotLeakWatchkeeperIDOrURI(t *testing.T) {
	t.Parallel()

	canaryWatchkeeperID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	const canaryWatchkeeperIDSubstring = "dddddddd-dddd-dddd-dddd-dddddddddddd"
	//nolint:gosec // G101: synthetic redaction-harness canary, not a real URI.
	const canaryArchiveURI = "file:///CANARY-archive-uri/should-never-leak-into-mark-retired-errors.tar.gz"
	leakSubstrings := []string{canaryWatchkeeperIDSubstring, "CANARY-archive-uri"}

	cases := []struct {
		name  string
		setup func() (step *spawn.MarkRetiredStep, ctx context.Context)
	}{
		{
			name: "retirer error (canary URI configured)",
			setup: func() (*spawn.MarkRetiredStep, context.Context) {
				retirer := newFakeWatchkeeperRetirer()
				retirer.returnErr = errors.New("substrate fail")
				ctx := saga.WithSpawnContext(context.Background(), newMarkRetiredSpawnContext(t, canaryWatchkeeperID))
				ctx = saga.WithRetireResult(ctx, &saga.RetireResult{ArchiveURI: canaryArchiveURI})
				return newMarkRetiredStep(t, retirer), ctx
			},
		},
		{
			name: "missing spawn context",
			setup: func() (*spawn.MarkRetiredStep, context.Context) {
				retirer := newFakeWatchkeeperRetirer()
				ctx := saga.WithRetireResult(context.Background(), &saga.RetireResult{ArchiveURI: canaryArchiveURI})
				return newMarkRetiredStep(t, retirer), ctx
			},
		},
		{
			name: "nil agent id (with canary watchkeeper context elsewhere)",
			setup: func() (*spawn.MarkRetiredStep, context.Context) {
				retirer := newFakeWatchkeeperRetirer()
				sc := saga.SpawnContext{ManifestVersionID: uuid.New(), AgentID: uuid.Nil}
				ctx := saga.WithSpawnContext(context.Background(), sc)
				ctx = saga.WithRetireResult(ctx, &saga.RetireResult{ArchiveURI: canaryArchiveURI})
				return newMarkRetiredStep(t, retirer), ctx
			},
		},
		{
			name: "missing RetireResult",
			setup: func() (*spawn.MarkRetiredStep, context.Context) {
				retirer := newFakeWatchkeeperRetirer()
				ctx := saga.WithSpawnContext(context.Background(), newMarkRetiredSpawnContext(t, canaryWatchkeeperID))
				return newMarkRetiredStep(t, retirer), ctx
			},
		},
		{
			name: "empty archive URI on RetireResult (canary watchkeeperID still in SpawnContext)",
			setup: func() (*spawn.MarkRetiredStep, context.Context) {
				retirer := newFakeWatchkeeperRetirer()
				ctx := saga.WithSpawnContext(context.Background(), newMarkRetiredSpawnContext(t, canaryWatchkeeperID))
				ctx = saga.WithRetireResult(ctx, &saga.RetireResult{ArchiveURI: ""})
				return newMarkRetiredStep(t, retirer), ctx
			},
		},
		{
			name: "retirer returns ErrCredsNotFound (sentinel passthrough)",
			setup: func() (*spawn.MarkRetiredStep, context.Context) {
				retirer := newFakeWatchkeeperRetirer()
				retirer.returnErr = spawn.ErrCredsNotFound
				ctx := saga.WithSpawnContext(context.Background(), newMarkRetiredSpawnContext(t, canaryWatchkeeperID))
				ctx = saga.WithRetireResult(ctx, &saga.RetireResult{ArchiveURI: canaryArchiveURI})
				return newMarkRetiredStep(t, retirer), ctx
			},
		},
		{
			name: "cancelled context",
			setup: func() (*spawn.MarkRetiredStep, context.Context) {
				retirer := newFakeWatchkeeperRetirer()
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				ctx = saga.WithSpawnContext(ctx, newMarkRetiredSpawnContext(t, canaryWatchkeeperID))
				ctx = saga.WithRetireResult(ctx, &saga.RetireResult{ArchiveURI: canaryArchiveURI})
				return newMarkRetiredStep(t, retirer), ctx
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
					t.Errorf("error message %q contains canary substring %q (PII leak — step or wrap chain embeds %s)", msg, secret, secret)
				}
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────
// Concurrency: 16 goroutines, distinct watchkeeperIDs + distinct
// RetireResult outboxes per call, race-detector clean. Pins that the
// step holds NO per-saga state on the receiver (the receiver is the
// shared step instance; per-saga state lives on the per-call ctx).
// ────────────────────────────────────────────────────────────────────────

func TestMarkRetiredStep_Execute_Concurrency_DistinctWatchkeepers(t *testing.T) {
	t.Parallel()

	retirer := newFakeWatchkeeperRetirer()
	step := newMarkRetiredStep(t, retirer)

	const n = 16
	ids := make([]uuid.UUID, n)
	uris := make([]string, n)
	for i := range ids {
		ids[i] = uuid.New()
		uris[i] = "file:///snapshots/concurrency-" + ids[i].String() + ".tar.gz"
	}

	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, _ := newMarkRetiredCtx(t, ids[i], uris[i])
			if err := step.Execute(ctx); err != nil {
				t.Errorf("Execute(%v): %v", ids[i], err)
			}
		}(i)
	}
	wg.Wait()

	if got := retirer.callCount.Load(); got != n {
		t.Errorf("Retirer call count = %d, want %d", got, n)
	}
	calls := retirer.recordedCalls()
	seen := make(map[uuid.UUID]string, n)
	for _, c := range calls {
		seen[c.watchkeeperID] = c.archiveURI
	}
	for i, id := range ids {
		gotURI, ok := seen[id]
		if !ok {
			t.Errorf("watchkeeperID %v missing from recorded calls", id)
			continue
		}
		if gotURI != uris[i] {
			t.Errorf("watchkeeperID %v: archiveURI = %q, want %q (concurrent calls must not cross-contaminate URIs)",
				id, gotURI, uris[i])
		}
	}
}
