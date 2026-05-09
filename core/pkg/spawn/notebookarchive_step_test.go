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
// Hand-rolled fakes (M3.6 / M6.3.e / M7.1.c-.e pattern — no mocking lib).
// ────────────────────────────────────────────────────────────────────────

// fakeNotebookArchiver records every ArchiveNotebook call onto a
// shared record set, optionally returns a configured error or
// configured URI to drive negative paths. Concurrency: all mutable
// state lives behind a mutex / atomics so concurrent Execute() calls
// can drive the same fake without data races (`go test -race` clean).
type fakeNotebookArchiver struct {
	mu        sync.Mutex
	calls     []recordedArchiveCall
	callCount atomic.Int32
	returnURI string
	returnErr error
}

type recordedArchiveCall struct {
	ctx           context.Context
	watchkeeperID uuid.UUID
}

func newFakeNotebookArchiver() *fakeNotebookArchiver {
	return &fakeNotebookArchiver{
		returnURI: "file:///snapshots/wk/2026-05-09T12-34-56Z.tar.gz",
	}
}

// ArchiveNotebook records the supplied ctx + watchkeeperID. Recording
// the ctx (rather than discarding it) is load-bearing per the M7.1.d /
// M7.1.e ctx-propagation lesson: it pins the contract that the step
// forwards the caller's ctx verbatim to the seam, so a future
// regression to `context.Background()` or a derived ctx that strips
// cancellation / values surfaces as a test failure.
func (f *fakeNotebookArchiver) ArchiveNotebook(ctx context.Context, watchkeeperID uuid.UUID) (string, error) {
	f.callCount.Add(1)
	f.mu.Lock()
	f.calls = append(f.calls, recordedArchiveCall{ctx: ctx, watchkeeperID: watchkeeperID})
	f.mu.Unlock()
	if f.returnErr != nil {
		return "", f.returnErr
	}
	return f.returnURI, nil
}

func (f *fakeNotebookArchiver) recordedCalls() []recordedArchiveCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedArchiveCall, len(f.calls))
	copy(out, f.calls)
	return out
}

func newRetireSpawnContext(t *testing.T, watchkeeperID uuid.UUID) saga.SpawnContext {
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

func newRetireCtx(t *testing.T, watchkeeperID uuid.UUID) (context.Context, *saga.RetireResult) {
	t.Helper()
	result := &saga.RetireResult{}
	ctx := saga.WithSpawnContext(context.Background(), newRetireSpawnContext(t, watchkeeperID))
	ctx = saga.WithRetireResult(ctx, result)
	return ctx, result
}

func newNotebookArchiveStep(t *testing.T, archiver spawn.NotebookArchiver) *spawn.NotebookArchiveStep {
	t.Helper()
	return spawn.NewNotebookArchiveStep(spawn.NotebookArchiveStepDeps{
		Archiver: archiver,
	})
}

// ────────────────────────────────────────────────────────────────────────
// Compile-time + Name() coverage
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Name_ReturnsStableIdentifier(t *testing.T) {
	t.Parallel()

	step := newNotebookArchiveStep(t, newFakeNotebookArchiver())
	if got := step.Name(); got != "notebook_archive" {
		t.Errorf("Name() = %q, want %q", got, "notebook_archive")
	}
	if got := step.Name(); got != spawn.NotebookArchiveStepName {
		t.Errorf("Name() = %q, want %q (NotebookArchiveStepName)", got, spawn.NotebookArchiveStepName)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Constructor panics
// ────────────────────────────────────────────────────────────────────────

func TestNewNotebookArchiveStep_PanicsOnNilArchiver(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewNotebookArchiveStep with nil Archiver did not panic")
		}
	}()
	_ = spawn.NewNotebookArchiveStep(spawn.NotebookArchiveStepDeps{Archiver: nil})
}

// ────────────────────────────────────────────────────────────────────────
// Happy path — Archiver called once with watchkeeperID; URI lands on
// RetireResult; Execute returns nil.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Execute_HappyPath_PublishesURIToRetireResult(t *testing.T) {
	t.Parallel()

	archiver := newFakeNotebookArchiver()
	const wantURI = "file:///snapshots/wk-happy/2026-05-09T12-34-56Z.tar.gz"
	archiver.returnURI = wantURI
	step := newNotebookArchiveStep(t, archiver)

	watchkeeperID := uuid.New()
	ctx, result := newRetireCtx(t, watchkeeperID)

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := archiver.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Archiver.ArchiveNotebook call count = %d, want 1", len(calls))
	}
	if calls[0].watchkeeperID != watchkeeperID {
		t.Errorf("call.watchkeeperID = %q, want %q", calls[0].watchkeeperID, watchkeeperID)
	}
	if result.ArchiveURI != wantURI {
		t.Errorf("RetireResult.ArchiveURI = %q, want %q (step must publish the URI on success)",
			result.ArchiveURI, wantURI)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Missing SpawnContext → wrapped error; no Archiver call;
// RetireResult untouched.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Execute_MissingSpawnContext_NoArchiverCall(t *testing.T) {
	t.Parallel()

	archiver := newFakeNotebookArchiver()
	step := newNotebookArchiveStep(t, archiver)

	// Seed RetireResult but NOT SpawnContext — the step must reject
	// before dispatch and leave the outbox untouched.
	result := &saga.RetireResult{ArchiveURI: "untouched"}
	ctx := saga.WithRetireResult(context.Background(), result)

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingSpawnContext")
	}
	if !errors.Is(err, spawn.ErrMissingSpawnContext) {
		t.Errorf("errors.Is(err, ErrMissingSpawnContext) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: notebook_archive step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: notebook_archive step:")
	}
	if got := archiver.callCount.Load(); got != 0 {
		t.Errorf("Archiver call count = %d, want 0 (missing SpawnContext fails before dispatch)", got)
	}
	if result.ArchiveURI != "untouched" {
		t.Errorf("RetireResult.ArchiveURI mutated on failure path: got %q", result.ArchiveURI)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: AgentID = uuid.Nil → wrapped ErrMissingAgentID; no Archiver
// call; RetireResult untouched.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Execute_NilAgentID_NoArchiverCall(t *testing.T) {
	t.Parallel()

	archiver := newFakeNotebookArchiver()
	step := newNotebookArchiveStep(t, archiver)

	sc := saga.SpawnContext{ManifestVersionID: uuid.New(), AgentID: uuid.Nil}
	result := &saga.RetireResult{ArchiveURI: "untouched"}
	ctx := saga.WithSpawnContext(context.Background(), sc)
	ctx = saga.WithRetireResult(ctx, result)

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingAgentID")
	}
	if !errors.Is(err, spawn.ErrMissingAgentID) {
		t.Errorf("errors.Is(err, ErrMissingAgentID) = false; got %v", err)
	}
	if got := archiver.callCount.Load(); got != 0 {
		t.Errorf("Archiver call count = %d, want 0", got)
	}
	if result.ArchiveURI != "untouched" {
		t.Errorf("RetireResult.ArchiveURI mutated on failure path: got %q", result.ArchiveURI)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Missing RetireResult outbox → wrapped ErrMissingRetireResult;
// no Archiver call. Pins the M7.2.b kickoffer-side seeding contract:
// without an outbox, the step has nowhere to publish the URI for the
// M7.2.c MarkRetired step to consume, so it MUST fail-closed.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Execute_MissingRetireResult_NoArchiverCall(t *testing.T) {
	t.Parallel()

	archiver := newFakeNotebookArchiver()
	step := newNotebookArchiveStep(t, archiver)

	// Seed SpawnContext but NOT RetireResult.
	ctx := saga.WithSpawnContext(context.Background(), newRetireSpawnContext(t, uuid.New()))

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrMissingRetireResult")
	}
	if !errors.Is(err, spawn.ErrMissingRetireResult) {
		t.Errorf("errors.Is(err, ErrMissingRetireResult) = false; got %v", err)
	}
	if got := archiver.callCount.Load(); got != 0 {
		t.Errorf("Archiver call count = %d, want 0 (missing RetireResult fails before dispatch)", got)
	}
}

// TestNotebookArchiveStep_Execute_WithRetireResultNilPanic pins the
// iter-1-strengthened seam contract: passing a nil pointer to
// [saga.WithRetireResult] panics at the seam, BEFORE any saga step
// runs. The prior contract accepted nil and forced every step to
// double-check; codex iter-1 surfaced this as a weaker API. The
// step itself now only handles the missing-key branch; the
// seam-side panic is the new invariant.
func TestNotebookArchiveStep_Execute_WithRetireResultNilPanic(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("saga.WithRetireResult(nil) did not panic; iter-1 strengthening lost")
		}
	}()
	_ = saga.WithRetireResult(context.Background(), nil)
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Archiver returns error → wrapped + returned; RetireResult
// untouched (failure ⇒ no observable result side effect).
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Execute_ArchiverError_WrapsAndReturns(t *testing.T) {
	t.Parallel()

	archiverErr := errors.New("simulated archive substrate failure")
	archiver := newFakeNotebookArchiver()
	archiver.returnErr = archiverErr
	step := newNotebookArchiveStep(t, archiver)

	ctx, result := newRetireCtx(t, uuid.New())

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped Archiver error")
	}
	if !errors.Is(err, archiverErr) {
		t.Errorf("errors.Is(err, archiverErr) = false; got %v", err)
	}
	if !strings.HasPrefix(err.Error(), "spawn: notebook_archive step:") {
		t.Errorf("err prefix = %q; want %q-prefixed wrap", err.Error(), "spawn: notebook_archive step:")
	}
	if result.ArchiveURI != "" {
		t.Errorf("RetireResult.ArchiveURI = %q, want \"\" (failure must not publish a URI)", result.ArchiveURI)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Archiver passthrough preserves typed sentinels (M7.1.c.b.b /
// M7.1.d / M7.1.e "Reuse sentinel errors across saga steps" lesson —
// ErrCredsNotFound surfaces unchanged through the wrap chain).
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Execute_PreservesCredsNotFoundSentinel(t *testing.T) {
	t.Parallel()

	archiver := newFakeNotebookArchiver()
	archiver.returnErr = spawn.ErrCredsNotFound
	step := newNotebookArchiveStep(t, archiver)

	ctx, result := newRetireCtx(t, uuid.New())

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrCredsNotFound")
	}
	if !errors.Is(err, spawn.ErrCredsNotFound) {
		t.Errorf("errors.Is(err, ErrCredsNotFound) = false; got %v", err)
	}
	// Iter-1 critic finding (Cr3): pin the "failure ⇒ no observable
	// outbox side effect" invariant uniformly across every error
	// path's test, not just the obvious archiver-error / empty-URI
	// ones. A regression that wrote the URI before the sentinel
	// passthrough would otherwise slip past this test.
	if result.ArchiveURI != "" {
		t.Errorf("RetireResult.ArchiveURI = %q, want \"\" (sentinel-passthrough failure must not publish anything)", result.ArchiveURI)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Archiver returns ("", nil) → wrapped ErrEmptyArchiveURI;
// RetireResult untouched. An empty URI on the success path is a wiring
// bug in the production wrapper (the substrate's contract is that a
// successful archive always yields a non-empty URI); the step
// fail-closes loudly so M7.2.c does not silently persist an empty
// archive_uri onto the watchkeeper row.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Execute_EmptyURIOnSuccess_FailsClosed(t *testing.T) {
	t.Parallel()

	archiver := newFakeNotebookArchiver()
	archiver.returnURI = ""
	step := newNotebookArchiveStep(t, archiver)

	ctx, result := newRetireCtx(t, uuid.New())

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ErrEmptyArchiveURI")
	}
	if !errors.Is(err, spawn.ErrEmptyArchiveURI) {
		t.Errorf("errors.Is(err, ErrEmptyArchiveURI) = false; got %v", err)
	}
	if result.ArchiveURI != "" {
		t.Errorf("RetireResult.ArchiveURI = %q, want \"\" (empty-URI failure path must not publish anything)", result.ArchiveURI)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: ctx already cancelled → wrapped ctx.Err(); no Archiver call;
// RetireResult untouched.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Execute_CancelledContext_NoArchiverCall(t *testing.T) {
	t.Parallel()

	archiver := newFakeNotebookArchiver()
	step := newNotebookArchiveStep(t, archiver)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ctx = saga.WithSpawnContext(ctx, newRetireSpawnContext(t, uuid.New()))
	result := &saga.RetireResult{}
	ctx = saga.WithRetireResult(ctx, result)

	err := step.Execute(ctx)
	if err == nil {
		t.Fatalf("Execute: err = nil, want wrapped ctx.Err()")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false; got %v", err)
	}
	if got := archiver.callCount.Load(); got != 0 {
		t.Errorf("Archiver call count = %d, want 0 (cancellation precedes dispatch)", got)
	}
	if result.ArchiveURI != "" {
		t.Errorf("RetireResult.ArchiveURI = %q, want \"\" (cancellation must not publish anything)", result.ArchiveURI)
	}
}

// ────────────────────────────────────────────────────────────────────────
// Ctx propagation: the seam receives the caller's ctx verbatim. Pins
// the contract that future maintainers cannot quietly swap to
// `context.Background()` or a derived ctx that strips deadlines /
// cancellation / values (M7.1.d / M7.1.e ctx-propagation lesson).
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Execute_PropagatesCallerCtxValueToArchiver(t *testing.T) {
	t.Parallel()

	type ctxKey struct{}
	sentinel := struct{ tag string }{tag: "iter1-pin"}

	archiver := newFakeNotebookArchiver()
	step := newNotebookArchiveStep(t, archiver)

	ctx := context.WithValue(context.Background(), ctxKey{}, sentinel)
	ctx = saga.WithSpawnContext(ctx, newRetireSpawnContext(t, uuid.New()))
	ctx = saga.WithRetireResult(ctx, &saga.RetireResult{})

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := archiver.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Archiver call count = %d, want 1", len(calls))
	}
	got, ok := calls[0].ctx.Value(ctxKey{}).(struct{ tag string })
	if !ok || got != sentinel {
		t.Errorf("Archiver.ctx did not carry the WithValue sentinel — step swapped to context.Background or stripped it (got %v, ok=%v)", got, ok)
	}
}

func TestNotebookArchiveStep_Execute_PropagatesCallerCtxCancellationToArchiver(t *testing.T) {
	t.Parallel()

	archiver := newFakeNotebookArchiver()
	step := newNotebookArchiveStep(t, archiver)

	parent, cancel := context.WithCancel(context.Background())
	ctx := saga.WithSpawnContext(parent, newRetireSpawnContext(t, uuid.New()))
	ctx = saga.WithRetireResult(ctx, &saga.RetireResult{})

	if err := step.Execute(ctx); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	calls := archiver.recordedCalls()
	if len(calls) != 1 {
		t.Fatalf("Archiver call count = %d, want 1", len(calls))
	}

	// Cancel the caller's parent context AFTER the call returned. The
	// recorded ctx must observe the cancellation, proving the step
	// did not detach (e.g. via context.Background or context.WithoutCancel).
	cancel()
	if err := calls[0].ctx.Err(); err != context.Canceled {
		t.Errorf("Archiver.ctx.Err() after caller cancel = %v, want context.Canceled (step detached the ctx from the caller's cancellation tree)", err)
	}
}

// ────────────────────────────────────────────────────────────────────────
// AC7 + lean-seam: step source does NOT call keeperslog.Writer.Append
// AND does NOT import core/pkg/notebook, core/pkg/archivestore,
// core/pkg/messenger, or any DAO surface. Pins the M7.1.d / M7.1.e
// "lean saga-step seam" lesson: the step stays free of substrate /
// platform / DAO imports — that work belongs to the production
// [NotebookArchiver] wrapper.
// ────────────────────────────────────────────────────────────────────────

// TestNotebookArchiveStep_StaysLean mirrors and extends the M7.1.d /
// M7.1.e source-grep AC pin: it reads notebookarchive_step.go, strips
// pure comment lines, then asserts the non-comment source contains
// none of a closed-set of forbidden substrings (audit emit, substrate
// imports, platform imports, DAO type). Stronger than a runtime
// assertion because it catches any future edit that adds a forbidden
// import or call regardless of test wiring.
func TestNotebookArchiveStep_StaysLean(t *testing.T) {
	t.Parallel()

	src, err := os.ReadFile("notebookarchive_step.go")
	if err != nil {
		t.Fatalf("read notebookarchive_step.go: %v", err)
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
		{"core/pkg/keepclient", "step imports core/pkg/keepclient — keep-client wiring belongs in the production wrapper, not the step"},
		{"WatchkeeperSlackAppCredsDAO", "step references the WatchkeeperSlackAppCredsDAO type — DAO surface belongs in the production wrapper, not the step"},
	}
	for _, f := range forbidden {
		if strings.Contains(body, f.needle) {
			t.Errorf("notebookarchive_step.go contains forbidden substring %q in non-comment code: %s", f.needle, f.reason)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// Negative: Archiver returns malformed URI on success → wrapped
// ErrInvalidArchiveURI; RetireResult untouched. Iter-1 codex finding:
// M7.2.c persists the URI directly onto the watchkeeper row, so a
// `garbage` / whitespace / no-scheme value from a buggy wrapper
// would poison the retire trail without the shape check. The step
// validates via `net/url.Parse + Scheme != ""` to fail-closed at the
// step boundary.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Execute_MalformedURIOnSuccess_FailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		uri  string
	}{
		// `garbage` — no scheme, no `://` separator. The codex iter-1
		// example: a wrapper that returns the substrate's stack-trace
		// fragment instead of the canonical URI.
		{name: "no scheme", uri: "garbage"},
		// `   ` — whitespace-only. url.Parse accepts it but Scheme is empty.
		{name: "whitespace", uri: "   "},
		// `/path/only` — relative path, no scheme. archivestore.Get
		// would reject this with ErrInvalidURI; the step rejects
		// upstream so M7.2.c never persists a path-only value.
		{name: "path only", uri: "/var/tmp/garbage.tar.gz"},
		// `:not-a-uri` — url.Parse returns an error.
		{name: "url parse error", uri: ":not-a-uri"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			archiver := newFakeNotebookArchiver()
			archiver.returnURI = tc.uri
			step := newNotebookArchiveStep(t, archiver)

			ctx, result := newRetireCtx(t, uuid.New())
			err := step.Execute(ctx)
			if err == nil {
				t.Fatalf("Execute: err = nil, want wrapped ErrInvalidArchiveURI for %q", tc.uri)
			}
			if !errors.Is(err, spawn.ErrInvalidArchiveURI) {
				t.Errorf("errors.Is(err, ErrInvalidArchiveURI) = false; got %v", err)
			}
			if result.ArchiveURI != "" {
				t.Errorf("RetireResult.ArchiveURI = %q, want \"\" (malformed-URI failure must not publish anything)", result.ArchiveURI)
			}
		})
	}
}

// TestNotebookArchiveStep_Execute_WellFormedSchemes_Pass pins the
// positive side of the URI-shape gate: every scheme the production
// archivestore family produces (file:, s3:, gs:, fake: for tests)
// passes the gate untouched. A future maintainer who tightens the
// gate to a closed-set scheme list (e.g. only `s3:`) would break
// the retire flow against the LocalFS backend; this test catches it.
func TestNotebookArchiveStep_Execute_WellFormedSchemes_Pass(t *testing.T) {
	t.Parallel()

	cases := []string{
		"file:///snapshots/wk/2026-05-09T12-34-56Z.tar.gz",
		"s3://bucket/notebook/wk-id/2026-05-09T12-34-56Z.tar.gz",
		"gs://bucket/key.tar.gz",
		"fake://test/snap.tar.gz",
	}
	for _, uri := range cases {
		t.Run(uri, func(t *testing.T) {
			t.Parallel()
			archiver := newFakeNotebookArchiver()
			archiver.returnURI = uri
			step := newNotebookArchiveStep(t, archiver)

			ctx, result := newRetireCtx(t, uuid.New())
			if err := step.Execute(ctx); err != nil {
				t.Fatalf("Execute(%q): %v", uri, err)
			}
			if result.ArchiveURI != uri {
				t.Errorf("RetireResult.ArchiveURI = %q, want %q", result.ArchiveURI, uri)
			}
		})
	}
}

// ────────────────────────────────────────────────────────────────────────
// Sentinel distinctness — pin that the M7.2.b-introduced sentinels
// (ErrMissingRetireResult, ErrEmptyArchiveURI, ErrInvalidArchiveURI)
// are mutually distinct under errors.Is. A future refactor that
// aliases two of them onto a common type would break differentiation
// at the M7.3 compensator boundary; this test catches it.
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Sentinels_AreMutuallyDistinct(t *testing.T) {
	t.Parallel()

	pairs := []struct {
		a, b error
		name string
	}{
		{spawn.ErrMissingRetireResult, spawn.ErrEmptyArchiveURI, "missing-vs-empty"},
		{spawn.ErrMissingRetireResult, spawn.ErrInvalidArchiveURI, "missing-vs-invalid"},
		{spawn.ErrEmptyArchiveURI, spawn.ErrInvalidArchiveURI, "empty-vs-invalid"},
	}
	for _, p := range pairs {
		if errors.Is(p.a, p.b) || errors.Is(p.b, p.a) {
			t.Errorf("%s: errors.Is unexpectedly aliased the two sentinels", p.name)
		}
	}
}

// ────────────────────────────────────────────────────────────────────────
// Iter-1 critic finding (Cr1) — PII canary redaction harness mirroring
// M7.1.c.b.b / M7.1.d / M7.1.e. The step's doc-block at lines 30-37
// claims (a) the archive URI is the success payload, NOT something the
// step embeds in failure error strings, AND (b) the watchkeeperID is
// already on the saga audit chain via SpawnContext.AgentID, so the
// step does not re-leak it through error messages. Without this
// harness those claims have no test enforcement: a future maintainer
// who decorates the wrap chain with `uri=%q` or `wk=%v` for "richer
// diagnostics" would slip past every other test undetected. The
// harness drives Execute through every error-string-building path
// with canary substrings in the watchkeeperID + archiver URI and
// asserts neither leaks via err.Error().
// ────────────────────────────────────────────────────────────────────────

func TestNotebookArchiveStep_Execute_ErrorPaths_DoNotLeakWatchkeeperIDOrURI(t *testing.T) {
	t.Parallel()

	// Fixed UUID with a recognisable substring so an accidental
	// `%v` / `%s` of the AgentID into an error message lights up
	// the canary-grep below. Real production UUIDs are random;
	// using a fixed one here lets us assert by substring.
	canaryWatchkeeperID := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	const canaryWatchkeeperIDSubstring = "cccccccc-cccc-cccc-cccc-cccccccccccc"
	//nolint:gosec // G101: synthetic redaction-harness canary, not a real URI.
	const canaryArchiveURI = "file:///CANARY-archive-uri/should-never-leak-into-errors.tar.gz"
	leakSubstrings := []string{canaryWatchkeeperIDSubstring, "CANARY-archive-uri"}

	cases := []struct {
		name  string
		setup func() (step *spawn.NotebookArchiveStep, ctx context.Context)
	}{
		{
			name: "archiver error (canary URI configured but archiver returns err)",
			setup: func() (*spawn.NotebookArchiveStep, context.Context) {
				archiver := newFakeNotebookArchiver()
				archiver.returnURI = canaryArchiveURI
				archiver.returnErr = errors.New("substrate fail")
				ctx := saga.WithSpawnContext(context.Background(), newRetireSpawnContext(t, canaryWatchkeeperID))
				ctx = saga.WithRetireResult(ctx, &saga.RetireResult{})
				return newNotebookArchiveStep(t, archiver), ctx
			},
		},
		{
			name: "missing spawn context",
			setup: func() (*spawn.NotebookArchiveStep, context.Context) {
				archiver := newFakeNotebookArchiver()
				archiver.returnURI = canaryArchiveURI
				ctx := saga.WithRetireResult(context.Background(), &saga.RetireResult{})
				return newNotebookArchiveStep(t, archiver), ctx
			},
		},
		{
			name: "nil agent id (with canary watchkeeper context elsewhere)",
			setup: func() (*spawn.NotebookArchiveStep, context.Context) {
				archiver := newFakeNotebookArchiver()
				archiver.returnURI = canaryArchiveURI
				sc := saga.SpawnContext{ManifestVersionID: uuid.New(), AgentID: uuid.Nil}
				ctx := saga.WithSpawnContext(context.Background(), sc)
				ctx = saga.WithRetireResult(ctx, &saga.RetireResult{})
				return newNotebookArchiveStep(t, archiver), ctx
			},
		},
		{
			name: "missing RetireResult",
			setup: func() (*spawn.NotebookArchiveStep, context.Context) {
				archiver := newFakeNotebookArchiver()
				archiver.returnURI = canaryArchiveURI
				ctx := saga.WithSpawnContext(context.Background(), newRetireSpawnContext(t, canaryWatchkeeperID))
				return newNotebookArchiveStep(t, archiver), ctx
			},
		},
		{
			name: "archiver returns ErrCredsNotFound",
			setup: func() (*spawn.NotebookArchiveStep, context.Context) {
				archiver := newFakeNotebookArchiver()
				archiver.returnURI = canaryArchiveURI
				archiver.returnErr = spawn.ErrCredsNotFound
				ctx := saga.WithSpawnContext(context.Background(), newRetireSpawnContext(t, canaryWatchkeeperID))
				ctx = saga.WithRetireResult(ctx, &saga.RetireResult{})
				return newNotebookArchiveStep(t, archiver), ctx
			},
		},
		{
			name: "cancelled context",
			setup: func() (*spawn.NotebookArchiveStep, context.Context) {
				archiver := newFakeNotebookArchiver()
				archiver.returnURI = canaryArchiveURI
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				ctx = saga.WithSpawnContext(ctx, newRetireSpawnContext(t, canaryWatchkeeperID))
				ctx = saga.WithRetireResult(ctx, &saga.RetireResult{})
				return newNotebookArchiveStep(t, archiver), ctx
			},
		},
		{
			name: "empty URI on success",
			setup: func() (*spawn.NotebookArchiveStep, context.Context) {
				archiver := newFakeNotebookArchiver()
				archiver.returnURI = "" // forces ErrEmptyArchiveURI
				ctx := saga.WithSpawnContext(context.Background(), newRetireSpawnContext(t, canaryWatchkeeperID))
				ctx = saga.WithRetireResult(ctx, &saga.RetireResult{})
				return newNotebookArchiveStep(t, archiver), ctx
			},
		},
		{
			name: "malformed URI on success",
			setup: func() (*spawn.NotebookArchiveStep, context.Context) {
				archiver := newFakeNotebookArchiver()
				archiver.returnURI = canaryArchiveURI + " <-- with-trailing-junk-no-wait-this-IS-still-valid-scheme"
				// Use something that fails the gate: bare path.
				archiver.returnURI = "/garbage/" + canaryWatchkeeperIDSubstring
				ctx := saga.WithSpawnContext(context.Background(), newRetireSpawnContext(t, canaryWatchkeeperID))
				ctx = saga.WithRetireResult(ctx, &saga.RetireResult{})
				return newNotebookArchiveStep(t, archiver), ctx
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

func TestNotebookArchiveStep_Execute_Concurrency_DistinctWatchkeepers(t *testing.T) {
	t.Parallel()

	archiver := newFakeNotebookArchiver()
	const wantURI = "file:///snapshots/concurrency.tar.gz"
	archiver.returnURI = wantURI
	step := newNotebookArchiveStep(t, archiver)

	const n = 16
	ids := make([]uuid.UUID, n)
	results := make([]*saga.RetireResult, n)
	for i := range ids {
		ids[i] = uuid.New()
		results[i] = &saga.RetireResult{}
	}

	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := saga.WithSpawnContext(context.Background(), newRetireSpawnContext(t, ids[i]))
			ctx = saga.WithRetireResult(ctx, results[i])
			if err := step.Execute(ctx); err != nil {
				t.Errorf("Execute(%v): %v", ids[i], err)
			}
		}(i)
	}
	wg.Wait()

	if got := archiver.callCount.Load(); got != n {
		t.Errorf("Archiver call count = %d, want %d", got, n)
	}
	calls := archiver.recordedCalls()
	seen := make(map[uuid.UUID]bool, n)
	for _, c := range calls {
		seen[c.watchkeeperID] = true
	}
	for _, id := range ids {
		if !seen[id] {
			t.Errorf("watchkeeperID %v missing from recorded calls", id)
		}
	}
	for i, r := range results {
		if r.ArchiveURI != wantURI {
			t.Errorf("results[%d].ArchiveURI = %q, want %q (each per-call outbox must receive the URI on its own success path)",
				i, r.ArchiveURI, wantURI)
		}
	}
}
