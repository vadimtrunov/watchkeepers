package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// Compile-time assertions: the production embedder + the test-only
// fake embedder satisfy [runtime.Embedder]; *notebook.DB satisfies
// [runtime.Rememberer]. Pinning these in `_test.go` keeps production
// code free of test-only symbols (mirrors the FakeRuntime pattern in
// fake_runtime_test.go) while still failing fast on a future shape
// drift in either package.
var (
	_ runtime.Embedder   = (*llm.FakeEmbeddingProvider)(nil)
	_ runtime.Rememberer = (*notebook.DB)(nil)
)

// reflectorAgentID is the canonical UUID used to open the per-agent
// notebook DB across the reflector tests. The supervisor / notebook
// path validates this format before any filesystem touch.
const reflectorAgentID = "22222222-3333-4444-5555-666666666666"

// freshReflectorDB opens a real per-agent notebook.DB rooted at
// t.TempDir() via the WATCHKEEPER_DATA env var. Per the M5.5.c.d.a /
// M5.5.d.c lesson "real SQLite over mocks", the reflector tests use
// the actual notebook.DB rather than a Remember mock.
func freshReflectorDB(t *testing.T) *notebook.DB {
	t.Helper()
	t.Setenv("WATCHKEEPER_DATA", t.TempDir())
	db, err := notebook.Open(context.Background(), reflectorAgentID)
	if err != nil {
		t.Fatalf("notebook.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// fixedClock returns a time.Time-returning closure stamping a fixed
// reference time. The reflector tests use it to assert deterministic
// active_after computation.
func fixedClock(ts time.Time) func() time.Time {
	return func() time.Time { return ts }
}

// recallAllLessons fetches every lesson row using a deterministic query
// embedding via the fake provider. The reflector tests use this to
// assert content / metadata round-trip without needing to reproduce
// the production embedding source string. TopK is generous (5) because
// these tests insert at most a handful of rows.
func recallAllLessons(t *testing.T, db *notebook.DB, activeAt time.Time) []notebook.RecallResult {
	t.Helper()
	probe, err := llm.NewFakeEmbeddingProvider().Embed(context.Background(), "probe")
	if err != nil {
		t.Fatalf("probe embed: %v", err)
	}
	res, err := db.Recall(context.Background(), notebook.RecallQuery{
		Embedding: probe,
		TopK:      5,
		Category:  notebook.CategoryLesson,
		ActiveAt:  activeAt,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	return res
}

// TestToolErrorReflector_Reflect_HappyPath_SandboxTimeout — Reflect on
// a sandbox_timeout error composes a lesson Entry whose Subject contains
// the tool name + error class, Content carries the error message,
// active_after = clock()+24h (default), and evidence_log_ref is empty
// (default logRefFunc).
func TestToolErrorReflector_Reflect_HappyPath_SandboxTimeout(t *testing.T) {
	// no t.Parallel: freshReflectorDB calls t.Setenv, which is
	// incompatible with parallel sub-tests in the same package.
	db := freshReflectorDB(t)
	clock := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	r, err := runtime.NewToolErrorReflector(
		db,
		runtime.WithEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithClock(fixedClock(clock)),
	)
	if err != nil {
		t.Fatalf("NewToolErrorReflector: %v", err)
	}

	if err := r.Reflect(
		context.Background(),
		reflectorAgentID,
		"sandbox.exec",
		"v1.2.3",
		"sandbox_timeout",
		"sandboxed process exceeded wall-clock budget",
	); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	// Read row back via direct Recall against the Lesson category. The
	// fake embedder is deterministic so the same query embedding hits
	// the same row.
	res := recallAllLessons(t, db, clock.Add(48*time.Hour))
	if len(res) != 1 {
		t.Fatalf("Recall result count = %d, want 1", len(res))
	}
	got := res[0]
	if !strings.Contains(got.Subject, "sandbox.exec") || !strings.Contains(got.Subject, "sandbox_timeout") {
		t.Errorf("Subject = %q, want to contain tool + class", got.Subject)
	}
	if !strings.Contains(got.Content, "sandboxed process exceeded wall-clock budget") {
		t.Errorf("Content = %q, want to contain error message", got.Content)
	}
	if got.Category != notebook.CategoryLesson {
		t.Errorf("Category = %q, want %q", got.Category, notebook.CategoryLesson)
	}
	if got.ToolVersion == nil || *got.ToolVersion != "v1.2.3" {
		t.Errorf("ToolVersion = %v, want v1.2.3", got.ToolVersion)
	}
	if got.EvidenceLogRef != nil && *got.EvidenceLogRef != "" {
		t.Errorf("EvidenceLogRef = %v, want nil/empty (default logRefFunc)", got.EvidenceLogRef)
	}
	wantActive := clock.Add(24 * time.Hour).UnixMilli()
	if got.ActiveAfter != wantActive {
		t.Errorf("ActiveAfter = %d, want %d (clock+24h)", got.ActiveAfter, wantActive)
	}
}

// TestToolErrorReflector_Reflect_HappyPath_CustomLogRef — A non-empty
// logRefFunc populates evidence_log_ref. A fixed clock yields a
// deterministic active_after.
func TestToolErrorReflector_Reflect_HappyPath_CustomLogRef(t *testing.T) {
	// no t.Parallel: t.Setenv inside freshReflectorDB.
	db := freshReflectorDB(t)
	clock := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	r, err := runtime.NewToolErrorReflector(
		db,
		runtime.WithEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithClock(fixedClock(clock)),
		runtime.WithLogRefFunc(func() string { return "log-ref-abc-123" }),
	)
	if err != nil {
		t.Fatalf("NewToolErrorReflector: %v", err)
	}

	if err := r.Reflect(
		context.Background(),
		reflectorAgentID,
		"http.fetch",
		"v0.0.1",
		"rpc_error",
		"upstream returned 502",
	); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	res := recallAllLessons(t, db, clock.Add(48*time.Hour))
	if len(res) != 1 {
		t.Fatalf("Recall result count = %d, want 1", len(res))
	}
	if res[0].EvidenceLogRef == nil || *res[0].EvidenceLogRef != "log-ref-abc-123" {
		t.Errorf("EvidenceLogRef = %v, want log-ref-abc-123", res[0].EvidenceLogRef)
	}
	if res[0].ActiveAfter != clock.Add(24*time.Hour).UnixMilli() {
		t.Errorf("ActiveAfter = %d, want clock+24h", res[0].ActiveAfter)
	}
}

// TestToolErrorReflector_Reflect_HappyPath_CustomCoolingOff — A custom
// cooling-off of 1h yields active_after = clock+1h.
func TestToolErrorReflector_Reflect_HappyPath_CustomCoolingOff(t *testing.T) {
	// no t.Parallel: t.Setenv inside freshReflectorDB.
	db := freshReflectorDB(t)
	clock := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	r, err := runtime.NewToolErrorReflector(
		db,
		runtime.WithEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithClock(fixedClock(clock)),
		runtime.WithCoolingOff(time.Hour),
	)
	if err != nil {
		t.Fatalf("NewToolErrorReflector: %v", err)
	}

	if err := r.Reflect(
		context.Background(),
		reflectorAgentID,
		"shell.run",
		"v2.0.0",
		"capability_denied",
		"missing exec capability",
	); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	res := recallAllLessons(t, db, clock.Add(2*time.Hour))
	if len(res) != 1 {
		t.Fatalf("Recall result count = %d, want 1", len(res))
	}
	wantActive := clock.Add(time.Hour).UnixMilli()
	if res[0].ActiveAfter != wantActive {
		t.Errorf("ActiveAfter = %d, want %d (clock+1h)", res[0].ActiveAfter, wantActive)
	}
}

// TestToolErrorReflector_Reflect_Edge_CancelledCtx — Reflect under a
// cancelled context returns ctx.Err from the embedder (best-effort path
// is still observable as a returned error to the reflector caller).
// The fake embedder honours ctx cancellation per its godoc; the
// reflector forwards ctx.Err without panicking.
func TestToolErrorReflector_Reflect_Edge_CancelledCtx(t *testing.T) {
	// no t.Parallel: t.Setenv inside freshReflectorDB.
	db := freshReflectorDB(t)

	r, err := runtime.NewToolErrorReflector(
		db,
		runtime.WithEmbedder(llm.NewFakeEmbeddingProvider()),
	)
	if err != nil {
		t.Fatalf("NewToolErrorReflector: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = r.Reflect(ctx, reflectorAgentID, "http.fetch", "v0", "rpc_error", "boom")
	if err == nil {
		t.Fatal("Reflect under cancelled ctx returned nil; want non-nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Reflect err = %v, want errors.Is context.Canceled", err)
	}
}

// TestToolErrorReflector_Reflect_Edge_MultiLineErrMsg — A multi-line
// errMsg keeps the Subject single-line (truncated at the first newline
// + ellipsis) and the Content preserves the newlines.
func TestToolErrorReflector_Reflect_Edge_MultiLineErrMsg(t *testing.T) {
	// no t.Parallel: t.Setenv inside freshReflectorDB.
	db := freshReflectorDB(t)

	r, err := runtime.NewToolErrorReflector(
		db,
		runtime.WithEmbedder(llm.NewFakeEmbeddingProvider()),
	)
	if err != nil {
		t.Fatalf("NewToolErrorReflector: %v", err)
	}

	multi := "panic: nil pointer\ngoroutine 1 [running]:\nmain.go:42"
	if err := r.Reflect(
		context.Background(),
		reflectorAgentID,
		"sandbox.exec",
		"v1",
		"panic",
		multi,
	); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	res := recallAllLessons(t, db, time.Now().Add(48*time.Hour))
	if len(res) != 1 {
		t.Fatalf("Recall result count = %d, want 1", len(res))
	}
	if strings.Contains(res[0].Subject, "\n") {
		t.Errorf("Subject contains newline: %q", res[0].Subject)
	}
	if !strings.Contains(res[0].Content, "goroutine 1 [running]:") {
		t.Errorf("Content lost newlines / detail: %q", res[0].Content)
	}
}

// TestToolErrorReflector_NewWithoutEmbedder_ReturnsErrEmbedderRequired
// — the constructor must reject a nil embedder via the package
// sentinel, matching the existing runtime sentinel idioms.
func TestToolErrorReflector_NewWithoutEmbedder_ReturnsErrEmbedderRequired(t *testing.T) {
	// no t.Parallel: t.Setenv inside freshReflectorDB.
	db := freshReflectorDB(t)

	r, err := runtime.NewToolErrorReflector(db)
	if !errors.Is(err, runtime.ErrEmbedderRequired) {
		t.Fatalf("NewToolErrorReflector(no embedder) err = %v, want errors.Is ErrEmbedderRequired", err)
	}
	if r != nil {
		t.Errorf("constructor returned non-nil reflector alongside error: %v", r)
	}
}

// TestToolErrorReflector_Reflect_Negative_EmbedError — when Embed
// returns a sentinel error, Reflect surfaces it (the wired runtime is
// the layer that swallows reflector errors per AC5; the reflector
// itself returns the underlying error so the wiring layer can log it).
func TestToolErrorReflector_Reflect_Negative_EmbedError(t *testing.T) {
	// no t.Parallel: t.Setenv inside freshReflectorDB.
	db := freshReflectorDB(t)
	sentinel := errors.New("embed: provider down")

	r, err := runtime.NewToolErrorReflector(
		db,
		runtime.WithEmbedder(llm.NewFakeEmbeddingProvider(llm.WithEmbedError(sentinel))),
	)
	if err != nil {
		t.Fatalf("NewToolErrorReflector: %v", err)
	}

	err = r.Reflect(context.Background(), reflectorAgentID, "x", "v0", "panic", "boom")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Reflect err = %v, want errors.Is sentinel embed error", err)
	}

	// And no row was written.
	res := recallAllLessons(t, db, time.Now().Add(48*time.Hour))
	if len(res) != 0 {
		t.Errorf("Recall after Embed failure returned %d rows, want 0", len(res))
	}
}

// TestToolErrorReflector_Reflect_Negative_RememberError — when the
// underlying Remember-er returns an error, Reflect surfaces it.
// Exercised via a tiny test-local stub that satisfies the public
// Rememberer-shaped interface declared on the reflector constructor.
type rememberStub struct {
	err error
}

func (s *rememberStub) Remember(_ context.Context, _ notebook.Entry) (string, error) {
	return "", s.err
}

func TestToolErrorReflector_Reflect_Negative_RememberError(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("remember: db locked")
	stub := &rememberStub{err: sentinel}

	r, err := runtime.NewToolErrorReflector(
		stub,
		runtime.WithEmbedder(llm.NewFakeEmbeddingProvider()),
	)
	if err != nil {
		t.Fatalf("NewToolErrorReflector: %v", err)
	}

	err = r.Reflect(context.Background(), reflectorAgentID, "x", "v0", "panic", "boom")
	if !errors.Is(err, sentinel) {
		t.Fatalf("Reflect err = %v, want errors.Is sentinel remember error", err)
	}
}
