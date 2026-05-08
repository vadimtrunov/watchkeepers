package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/llm"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// e2eHarness bundles every collaborator the M5.6.f reflection-lifecycle
// e2e test drives. It is constructed once via [setupE2EHarness] and
// shared across the four subtests so each subtest's assertions ride on
// top of state mutated by the previous ones — the goal is to lock the
// FULL chain (reflector write -> keepers-log row -> notebook entry ->
// cooling-off filter -> post-cooling-off recall) end-to-end.
//
// t0 is the fixed instant the reflector's clock returns; coolingOff
// is the default 24h window the reflector adds to t0 to compute
// active_after. agentID doubles as the supervisor key and the
// notebook on-disk filename, so the test wires both with the same UUID.
type e2eHarness struct {
	ctx        context.Context
	t0         time.Time
	coolingOff time.Duration
	agentID    string

	db         *notebook.DB
	supervisor *runtime.NotebookSupervisor
	keep       *recordingKeepClient
	writer     *keeperslog.Writer
	embedder   *llm.FakeEmbeddingProvider
	reflector  *runtime.ToolErrorReflector
	wired      *runtime.WiredRuntime
	rtID       runtime.ID

	manifest runtime.Manifest

	// keepEventID is the canned event id the recordingKeepClient returns
	// from LogAppend. Captured here so subtest assertions can compare
	// the lesson row's evidence_log_ref against the SAME id without
	// re-reading the recording.
	keepEventID string
}

// setupE2EHarness constructs the wired stack the M5.6.f integration test
// drives. Real notebook.DB (via t.TempDir() under WATCHKEEPER_DATA),
// real keeperslog.Writer (over a recordingKeepClient stub), real
// llm.FakeEmbeddingProvider, real ToolErrorReflector, real WiredRuntime
// over a fakeAgentRuntime whose InvokeTool always returns errToolBoom.
//
// The supervisor opens the agent's notebook via the same path the
// production code uses (NotebookSupervisor.Open -> notebook.Open) so
// BuildTurnRequest's Lookup path returns the same *notebook.DB the
// reflector writes through. This is the wiring that distinguishes the
// e2e from the unit-level reflector / wired-runtime tests.
func setupE2EHarness(t *testing.T) *e2eHarness {
	t.Helper()
	t.Setenv("WATCHKEEPER_DATA", t.TempDir())

	const keepEventID = "e2e-keep-event-id-001"
	// t0 is anchored to time.Now() (truncated to the second) so the
	// cooling-off rows the reflector writes have active_after = t0+24h
	// strictly in the future relative to the recall layer's time.Now()
	// and the seeded past-active-after row in Behaviour4 has
	// active_after = t0-1h strictly in the past.
	t0 := time.Now().Truncate(time.Second)

	supervisor := runtime.NewNotebookSupervisor()
	db, err := supervisor.Open(reflectorAgentID)
	if err != nil {
		t.Fatalf("supervisor.Open: %v", err)
	}
	t.Cleanup(func() { _ = supervisor.Close(reflectorAgentID) })

	keep := &recordingKeepClient{resp: &keepclient.LogAppendResponse{ID: keepEventID}}
	writer := keeperslog.New(keep)
	embedder := llm.NewFakeEmbeddingProvider()

	reflector, err := runtime.NewToolErrorReflector(
		db,
		runtime.WithEmbedder(embedder),
		runtime.WithClock(func() time.Time { return t0 }),
		runtime.WithKeepersLog(writer),
		runtime.WithLogRefFunc(func() string { return "" }),
	)
	if err != nil {
		t.Fatalf("NewToolErrorReflector: %v", err)
	}

	fake := newFakeAgentRuntime(t, errToolBoom)
	rt, err := fake.Start(context.Background(), runtime.Manifest{
		AgentID:      reflectorAgentID,
		SystemPrompt: "test system prompt",
		Model:        "claude-sonnet-4",
		Toolset:      runtime.Toolset{{Name: "sandbox.exec"}},
	})
	if err != nil {
		t.Fatalf("fake.Start: %v", err)
	}

	wired := runtime.NewWiredRuntime(
		fake,
		runtime.WithToolErrorReflector(reflector),
		runtime.WithAgentID(reflectorAgentID),
	)

	// turnManifest carries the recall knobs BuildTurnRequest needs to
	// exercise the cooling-off filter: TopK > 0 so the recall runs,
	// threshold == 0 so every score passes the post-filter.
	turnManifest := runtime.Manifest{
		AgentID:                    reflectorAgentID,
		SystemPrompt:               "test system prompt",
		Model:                      "claude-sonnet-4",
		Autonomy:                   runtime.AutonomySupervised,
		NotebookTopK:               5,
		NotebookRelevanceThreshold: 0,
	}

	return &e2eHarness{
		ctx:         context.Background(),
		t0:          t0,
		coolingOff:  24 * time.Hour,
		agentID:     reflectorAgentID,
		db:          db,
		supervisor:  supervisor,
		keep:        keep,
		writer:      writer,
		embedder:    embedder,
		reflector:   reflector,
		wired:       wired,
		rtID:        rt.ID(),
		manifest:    turnManifest,
		keepEventID: keepEventID,
	}
}

// TestReflectionLifecycle_E2E_ForcedToolError pins the M5.6.f end-to-end
// integration: a single forced tool error must produce a keepers-log
// lesson_learned row, a notebook lesson Entry whose six observable
// columns match the spec, and must NOT surface in the very next
// BuildTurnRequest's recalled-memory block (cooling-off filter). A
// second seeded lesson with active_after in the past DOES surface —
// proving the filter is active-only.
//
// The four subtests run in declared order on a SHARED harness so each
// behaviour rides on the previous behaviour's state. Sub-test 3 must
// observe the lesson written by Sub-test 1; Sub-test 4 seeds a second
// lesson directly via db.Remember and exercises a second
// BuildTurnRequest.
func TestReflectionLifecycle_E2E_ForcedToolError(t *testing.T) {
	// no t.Parallel: setupE2EHarness calls t.Setenv. Subtests share state
	// by design (see godoc above) so they MUST run sequentially.
	h := setupE2EHarness(t)

	t.Run("Behaviour1_AutoReflectionWritesLesson", func(t *testing.T) {
		assertBehaviour1AutoReflectionWritesLesson(t, h)
	})
	t.Run("Behaviour2_OriginalToolErrorPreserved", func(t *testing.T) {
		assertBehaviour2OriginalToolErrorPreserved(t, h)
	})
	t.Run("Behaviour3_CoolingOffSuppressesAutoInjection", func(t *testing.T) {
		assertBehaviour3CoolingOffSuppressesAutoInjection(t, h)
	})
	t.Run("Behaviour4_PostCoolingOffRecall", func(t *testing.T) {
		assertBehaviour4PostCoolingOffRecall(t, h)
	})
}

// assertBehaviour1AutoReflectionWritesLesson pins AC3: after the forced-
// error InvokeTool the recordingKeepClient holds exactly one
// lesson_learned LogAppend call, and the notebook contains exactly one
// lesson Entry whose category, tool_version, evidence_log_ref,
// active_after, subject and content all match the spec.
func assertBehaviour1AutoReflectionWritesLesson(t *testing.T, h *e2eHarness) {
	t.Helper()

	_, err := h.wired.InvokeTool(h.ctx, h.rtID, runtime.ToolCall{
		Name:        "sandbox.exec",
		ToolVersion: "v1.2.3",
		Arguments:   map[string]any{"cmd": "ls"},
	})
	if !errors.Is(err, errToolBoom) {
		t.Fatalf("InvokeTool err = %v, want errors.Is errToolBoom", err)
	}

	// keepers-log: exactly one append with EventType = "lesson_learned".
	if got := h.keep.callCount(); got != 1 {
		t.Fatalf("keep.callCount() = %d, want 1", got)
	}
	calls := h.keep.recordedCalls()
	if got := calls[0].EventType; got != "lesson_learned" {
		t.Errorf("EventType = %q, want %q", got, "lesson_learned")
	}

	// notebook: exactly one lesson Entry with all six observable columns.
	entries := recallAllLessons(t, h.db, h.t0.Add(48*time.Hour))
	if len(entries) != 1 {
		t.Fatalf("recallAllLessons count = %d, want 1", len(entries))
	}
	got := entries[0]
	if got.Category != notebook.CategoryLesson {
		t.Errorf("Category = %q, want %q", got.Category, notebook.CategoryLesson)
	}
	if got.ToolVersion == nil || *got.ToolVersion != "v1.2.3" {
		t.Errorf("ToolVersion = %v, want pointer to %q", got.ToolVersion, "v1.2.3")
	}
	if got.EvidenceLogRef == nil || *got.EvidenceLogRef != h.keepEventID {
		t.Errorf("EvidenceLogRef = %v, want pointer to %q", got.EvidenceLogRef, h.keepEventID)
	}
	wantActiveAfter := h.t0.Add(h.coolingOff).UnixMilli()
	if got.ActiveAfter != wantActiveAfter {
		t.Errorf("ActiveAfter = %d, want %d (t0+24h)", got.ActiveAfter, wantActiveAfter)
	}
	if !strings.Contains(got.Subject, "sandbox.exec") {
		t.Errorf("Subject = %q does not contain tool name", got.Subject)
	}
	if !strings.Contains(got.Content, errToolBoom.Error()) {
		t.Errorf("Content = %q does not contain inner error message", got.Content)
	}
	// needs_review is not surfaced via RecallResult (Recall filters
	// needs_review=1 rows at the SQL layer); the row's presence in the
	// recall result IS the proof that needs_review = 0.
}

// assertBehaviour2OriginalToolErrorPreserved pins AC4: the error returned
// to the caller is the same sentinel errToolBoom the fakeAgentRuntime
// injects — the reflector must not replace or wrap it.
//
// This subtest calls InvokeTool a second time, intentionally adding a
// second cooling-off lesson row. Behaviour3 must exclude BOTH cooling-off
// rows; Behaviour4 therefore starts with two rows cooling-off + one seeded.
func assertBehaviour2OriginalToolErrorPreserved(t *testing.T, h *e2eHarness) {
	t.Helper()

	_, err := h.wired.InvokeTool(h.ctx, h.rtID, runtime.ToolCall{
		Name:        "sandbox.exec",
		ToolVersion: "v1.2.3",
		Arguments:   map[string]any{"cmd": "ls"},
	})
	if !errors.Is(err, errToolBoom) {
		t.Fatalf("errors.Is(err, errToolBoom) = false, got %v (reflector must not mask original error)", err)
	}
}

// assertBehaviour3CoolingOffSuppressesAutoInjection pins AC5: immediately
// after the forced-error invocations, a BuildTurnRequest call for the same
// agent does NOT include the just-written lesson in the System block and
// MetadataKeyCoolingOffFiltered is set to a strictly-positive count.
func assertBehaviour3CoolingOffSuppressesAutoInjection(t *testing.T, h *e2eHarness) {
	t.Helper()

	req, err := llm.BuildTurnRequest(
		h.ctx,
		h.manifest,
		"how do I run sandbox.exec safely?",
		h.embedder,
		h.supervisor,
	)
	if err != nil {
		t.Fatalf("BuildTurnRequest: %v", err)
	}
	if req == nil {
		t.Fatal("BuildTurnRequest returned nil request")
	}

	// The lessons' subject is "sandbox.exec: <errClass>" and content
	// embeds errToolBoom's message. Neither must appear — both rows are
	// inside the cooling-off window.
	if strings.Contains(req.System, "sandbox.exec") {
		t.Errorf("System block contains cooling-off lesson subject %q; System = %q",
			"sandbox.exec", req.System)
	}
	if strings.Contains(req.System, errToolBoom.Error()) {
		t.Errorf("System block contains cooling-off lesson content %q; System = %q",
			errToolBoom.Error(), req.System)
	}

	// Diagnostic counter must be present and strictly positive. After
	// Behaviour1 + Behaviour2 there are two cooling-off rows; we assert
	// > 0 rather than == "2" so a future change in the number of forced
	// invocations does not flip this test red.
	got, ok := req.Metadata[llm.MetadataKeyCoolingOffFiltered]
	if !ok {
		t.Fatalf("Metadata[%q] absent; want strictly-positive count",
			llm.MetadataKeyCoolingOffFiltered)
	}
	if got == "" || got == "0" {
		t.Errorf("Metadata[%q] = %q, want strictly-positive count",
			llm.MetadataKeyCoolingOffFiltered, got)
	}
}

// assertBehaviour4PostCoolingOffRecall pins AC6: a lesson seeded directly
// via db.Remember with active_after = t0-1h (past) DOES appear in the next
// BuildTurnRequest's System block while the two cooling-off lessons remain
// excluded.
func assertBehaviour4PostCoolingOffRecall(t *testing.T, h *e2eHarness) {
	t.Helper()

	const seededSubject = "post-cooling-off-marker"
	const seededContent = "this lesson is past its cooling-off window"
	seedQuery := seededSubject + "\n" + seededContent
	seedVec, err := h.embedder.Embed(h.ctx, seedQuery)
	if err != nil {
		t.Fatalf("embedder.Embed(seed): %v", err)
	}
	if _, err := h.db.Remember(h.ctx, notebook.Entry{
		Category:    notebook.CategoryLesson,
		Subject:     seededSubject,
		Content:     seededContent,
		ActiveAfter: h.t0.Add(-time.Hour).UnixMilli(),
		Embedding:   seedVec,
	}); err != nil {
		t.Fatalf("db.Remember(past-active-after): %v", err)
	}

	// BuildTurnRequest runs against three rows:
	//   - 2 cooling-off (Behaviour1 + Behaviour2) — excluded.
	//   - 1 past-active-after (just seeded) — INCLUDED.
	// The query string equals the seed string so the fake embedder
	// returns an identical vector; the seeded row has distance 0.
	req, err := llm.BuildTurnRequest(
		h.ctx,
		h.manifest,
		seedQuery,
		h.embedder,
		h.supervisor,
	)
	if err != nil {
		t.Fatalf("BuildTurnRequest (post-cooling-off): %v", err)
	}
	if req == nil {
		t.Fatal("BuildTurnRequest returned nil request")
	}
	if got := req.Metadata[llm.MetadataKeyRecalledMemoryStatus]; got != llm.RecalledMemoryStatusApplied {
		t.Fatalf("Metadata[%q] = %q, want %q",
			llm.MetadataKeyRecalledMemoryStatus, got, llm.RecalledMemoryStatusApplied)
	}
	if !strings.Contains(req.System, seededSubject) {
		t.Errorf("System block missing seeded subject %q; System = %q", seededSubject, req.System)
	}
	if !strings.Contains(req.System, seededContent) {
		t.Errorf("System block missing seeded content %q; System = %q", seededContent, req.System)
	}
	// Cooling-off lessons must still be excluded.
	if strings.Contains(req.System, errToolBoom.Error()) {
		t.Errorf("System block contains cooling-off lesson's err message; System = %q", req.System)
	}
}
