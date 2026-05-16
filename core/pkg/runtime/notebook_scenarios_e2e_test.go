package runtime_test

// M5 Notebook scenarios — B5 verification suite.
//
// Closes the two §M5 acceptance bullets that `docs/ROADMAP-phase1.md`
// pins under DoD Phase B (item B5):
//
//   - "Auto-injection: seed Notebook with a lesson, start a new turn,
//      observe lesson content in the prompt window."
//   - "Auto-reflection: force a tool error, check that a `lesson` entry
//      appears with `active_after` 24h in the future and `lesson_learned`
//      is in Keeper's Log."
//
// Each bullet has unit-level coverage already (the M5.6.d
// `TestBuildTurnRequest_*` suite in `core/pkg/llm/turn_request_external_test.go`
// pins the recall-into-System wiring; the M5.6.b/c wiring suite in
// `wired_runtime_test.go` and the M5.6.f harness in
// `reflection_lifecycle_e2e_test.go` pin the reflector + keepers-log
// edges). This file is the cross-cutting scenario layer — one self-
// contained test per bullet, each driving the full production
// composition end-to-end so a regression that only breaks at the
// wiring layer surfaces here loudly.
//
// Why scenario-shaped rather than additional unit tests: the §M5
// verification bullets are user-visible behaviour (a seeded lesson
// reaches the agent's prompt; a forced tool error produces a 24h
// cooling-off lesson AND a keepers-log row). They MUST pass through
// the same production seams a real agent boot uses (`BuildTurnRequest`
// for the inject path; `NewWiredRuntime` with both
// `WithToolErrorReflector` and `WithKeepersLogWriter` for the reflect
// path). The M5.6.f shared-state harness (`reflection_lifecycle_e2e_test.go`)
// chains four behaviours through one notebook so it can pin the
// cross-behaviour invariants (cooling-off filter survives across
// turns); this B5 file is the orthogonal axis — each bullet stands
// alone so the verification table maps one-to-one to a test that
// pins it.

import (
	"context"
	"encoding/json"
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

// b5SeededLessonSubject + b5SeededLessonContent are the marker strings
// the auto-injection scenario seeds into the notebook and then greps
// out of `req.System`. Kept as package-private constants (rather than
// inline string literals) so the seed step and the assertion step share
// a single source of truth — refactors that rename one side surface as
// a runtime failure on the missing-marker assertion instead of as a
// silent overlap with an unrelated string in the System block.
const (
	b5SeededLessonSubject = "b5-seeded-marker-subject"
	b5SeededLessonContent = "b5-seeded-marker-content"
)

// TestM5Verification_B5_AutoInjection_SeededLessonVisibleInPromptWindow
// pins B5 bullet 5: "seed Notebook with a lesson, start a new turn,
// observe lesson content in the prompt window."
//
// The production seam for "start a new turn" is `llm.BuildTurnRequest`,
// which assembles the per-turn `CompleteRequest` consumed by the
// runtime adapter. The "prompt window" is the assembled
// `CompleteRequest.System` slot — the same string the LLM provider
// receives at the wire boundary. Asserting against `req.System` is
// stronger than asserting against the notebook DB or the recall
// result: it pins the full assembly path (Embed → Recall → cooling-off
// filter → needs_review filter → WithRecalledMemory render → System
// concat) end-to-end.
//
// The seeded lesson has `active_after = 0` (immediately active) so
// the cooling-off filter does NOT exclude it — that exclusion path is
// covered orthogonally by the M5.6.f e2e Behaviour3 test. The recall-
// status metadata key is asserted as `applied` (and as Fatalf) so a
// regression that silently demotes the recall outcome (e.g., to
// `embed_error`) is caught even if the content match incidentally
// still succeeds via a future System-block change. The discriminator
// failure is the more informative one; the subsequent content checks
// would otherwise emit three near-identical cascading errors.
func TestM5Verification_B5_AutoInjection_SeededLessonVisibleInPromptWindow(t *testing.T) {
	// no t.Parallel: openTurnSupervisor → pinTurnDataDir → t.Setenv,
	// which is incompatible with parallel sub-tests in the same package.
	t.Setenv("WATCHKEEPER_DATA", t.TempDir())

	sup := runtime.NewNotebookSupervisor()
	db, err := sup.Open(reflectorAgentID)
	if err != nil {
		t.Fatalf("supervisor.Open(%q): %v", reflectorAgentID, err)
	}
	t.Cleanup(func() { _ = sup.Close(reflectorAgentID) })

	embedder := llm.NewFakeEmbeddingProvider()
	seedVec, err := embedder.Embed(context.Background(), b5SeededLessonSubject+"\n"+b5SeededLessonContent)
	if err != nil {
		t.Fatalf("embedder.Embed(seed): %v", err)
	}
	if _, err := db.Remember(context.Background(), notebook.Entry{
		Category:    notebook.CategoryLesson,
		Subject:     b5SeededLessonSubject,
		Content:     b5SeededLessonContent,
		ActiveAfter: 0, // immediately active — cooling-off filter must NOT exclude this row
		Embedding:   seedVec,
	}); err != nil {
		t.Fatalf("db.Remember(seeded lesson): %v", err)
	}

	manifest := runtime.Manifest{
		AgentID:                    reflectorAgentID,
		SystemPrompt:               "test system prompt",
		Model:                      "claude-sonnet-4",
		Autonomy:                   runtime.AutonomySupervised,
		NotebookTopK:               5,
		NotebookRelevanceThreshold: 0, // disabled — every recall match passes
	}

	req, err := llm.BuildTurnRequest(
		context.Background(),
		manifest,
		b5SeededLessonSubject+"\n"+b5SeededLessonContent,
		embedder,
		sup,
	)
	if err != nil {
		t.Fatalf("BuildTurnRequest: %v", err)
	}
	if req == nil {
		t.Fatal("BuildTurnRequest returned nil request")
	}

	// Status metadata first, as a Fatalf: a recall-pipeline demotion
	// (`disabled_topk_zero`, `agent_not_registered`, `embed_error`,
	// `recall_error`, `no_matches`) means the seeded lesson did not
	// reach the prompt-assembly stage. Continuing into the System
	// content checks would emit three near-identical cascading errors
	// that drown out the actual failure signal.
	if got := req.Metadata[llm.MetadataKeyRecalledMemoryStatus]; got != llm.RecalledMemoryStatusApplied {
		t.Fatalf("Metadata[%q] = %q, want %q (recall pipeline demoted; seeded lesson never reached assembly)",
			llm.MetadataKeyRecalledMemoryStatus, got, llm.RecalledMemoryStatusApplied)
	}

	// Header check is Fatalf for the same reason: a missing recalled-
	// memory header means the block was never rendered, so subject /
	// content checks below cannot possibly pass and would only emit
	// noise.
	if !strings.Contains(req.System, llm.RecalledMemoryHeader) {
		t.Fatalf("System missing recalled-memory header %q; System = %q",
			llm.RecalledMemoryHeader, req.System)
	}
	if !strings.Contains(req.System, b5SeededLessonSubject) {
		t.Errorf("System missing seeded lesson subject %q; System = %q",
			b5SeededLessonSubject, req.System)
	}
	if !strings.Contains(req.System, b5SeededLessonContent) {
		t.Errorf("System missing seeded lesson content %q; System = %q",
			b5SeededLessonContent, req.System)
	}
}

// TestM5Verification_B5_AutoReflection_ForcedToolErrorYieldsCoolingOffLessonAndKeepersLogRow
// pins B5 bullet 6: "force a tool error, check that a `lesson` entry
// appears with `active_after` 24h in the future and `lesson_learned`
// is in Keeper's Log."
//
// The production seam for the auto-reflection path is `NewWiredRuntime`
// with BOTH `WithToolErrorReflector` and `WithKeepersLogWriter` — the
// M5.6.c wiring the runtime adapter assembles when both options are
// supplied. The wired-runtime constructor threads the writer into the
// reflector via `WithKeepersLog` regardless of option declaration
// order (codex iter-1 review finding M#2); the orthogonal sub-test
// below pins that order-independence as a separate scenario so a
// future regression that makes the constructor order-sensitive cannot
// pass B5 unchanged. The over-fakeAgentRuntime composition is still a
// test composition — no production caller wires a fakeAgentRuntime
// today — but the option-application path through `NewWiredRuntime`
// is the seam a real M5.6.c caller uses.
//
// Assertions:
//   - The forced `errToolBoom` reaches the caller unchanged (the
//     reflector is best-effort and must NOT mask the inner error).
//   - The recording keepers-log client observes exactly one
//     `LogAppend` call with `EventType = "lesson_learned"` AND a
//     payload whose decoded JSON contains the forced tool name +
//     err class (so a regression that emits `lesson_learned` with
//     an empty / wrong payload still fails B5, not just "some event
//     was emitted").
//   - The notebook contains exactly one lesson row whose
//     `active_after` equals `t0 + runtime.DefaultCoolingOff` (the
//     production default — binding-time tied via the exported
//     constant rather than a coupled `24*time.Hour` literal).
//   - The lesson row's `evidence_log_ref` matches the keepers-log
//     event id the recording client returned (proves the cross-
//     reference is live, not just two writes-in-isolation).
//   - The lesson row's `ToolVersion` is the value the caller passed
//     on the `ToolCall` (closes the lateral coverage gap relative
//     to the M5.6.f e2e harness; M5.6.e hot-load needs-review
//     machinery keys off this field).
func TestM5Verification_B5_AutoReflection_ForcedToolErrorYieldsCoolingOffLessonAndKeepersLogRow(t *testing.T) {
	// no t.Parallel: notebook.Open → WATCHKEEPER_DATA → t.Setenv.
	t.Setenv("WATCHKEEPER_DATA", t.TempDir())

	db, err := notebook.Open(context.Background(), reflectorAgentID)
	if err != nil {
		t.Fatalf("notebook.Open(%q): %v", reflectorAgentID, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// t0 anchors the reflector's clock. Truncated to the second so the
	// reflector's UnixMilli stamp matches the assertion's UnixMilli
	// projection bit-for-bit (mirrors `setupE2EHarness:79`). Using
	// `time.Now()` rather than a fixed `time.Date(...)` literal keeps
	// the test from reading as a calendar-bound artifact while still
	// being deterministic — the 24h cooling-off math is relative and
	// the assertion compares the reflector's stamp against the same
	// `t0` it was called with.
	t0 := time.Now().Truncate(time.Second)

	const keepEventID = "b5-keep-event-id-001"
	const forcedToolName = "sandbox.exec"
	const forcedToolVersion = "v1.0.0"

	keep := &recordingKeepClient{resp: &keepclient.LogAppendResponse{ID: keepEventID}}
	writer := keeperslog.New(keep)

	reflector, err := runtime.NewToolErrorReflector(
		db,
		runtime.WithEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithClock(fixedClock(t0)),
	)
	if err != nil {
		t.Fatalf("NewToolErrorReflector: %v", err)
	}

	fake := newFakeAgentRuntime(t, errToolBoom)
	rt, err := fake.Start(context.Background(), runtime.Manifest{
		AgentID:      reflectorAgentID,
		SystemPrompt: "test system prompt",
		Model:        "claude-sonnet-4",
		Toolset:      runtime.Toolset{{Name: forcedToolName}},
	})
	if err != nil {
		t.Fatalf("fake.Start: %v", err)
	}

	// Production-facing composition: pass the writer via
	// WithKeepersLogWriter on the WIRED RUNTIME (M5.6.c). The wired
	// constructor threads it into the reflector via WithKeepersLog so
	// every reflected tool-error emits a lesson_learned event AND
	// populates the lesson's evidence_log_ref with the returned event
	// id. The sub-test below pins option order independence.
	wired := runtime.NewWiredRuntime(
		fake,
		runtime.WithToolErrorReflector(reflector),
		runtime.WithKeepersLogWriter(writer),
		runtime.WithAgentID(reflectorAgentID),
	)

	_, gotErr := wired.InvokeTool(context.Background(), rt.ID(), runtime.ToolCall{
		Name:        forcedToolName,
		ToolVersion: forcedToolVersion,
		Arguments:   map[string]any{"cmd": "ls"},
	})
	if !errors.Is(gotErr, errToolBoom) {
		t.Fatalf("InvokeTool err = %v, want errors.Is errToolBoom (best-effort reflector must not mask inner error)", gotErr)
	}

	assertB5KeepersLogLessonLearnedEvent(t, keep, forcedToolName)
	assertB5NotebookLessonRow(t, db, t0, keepEventID, forcedToolName, forcedToolVersion)
}

// assertB5KeepersLogLessonLearnedEvent pins the keepers-log half of
// B5 bullet 6: exactly one `LogAppend` with `EventType = "lesson_learned"`,
// AND a JSON payload that contains the forced tool name + err message.
// EventType alone proves "some event" was emitted, not that the row
// describes THIS tool failure — a regression that emitted
// `lesson_learned` with an empty / wrong payload would still satisfy
// a bare-EventType check (iter-1 codex m#5 finding).
func assertB5KeepersLogLessonLearnedEvent(t *testing.T, keep *recordingKeepClient, forcedToolName string) {
	t.Helper()
	if got := keep.callCount(); got != 1 {
		t.Fatalf("keep.callCount() = %d, want 1 (exactly one lesson_learned emission per forced tool error)", got)
	}
	calls := keep.recordedCalls()
	if got := calls[0].EventType; got != "lesson_learned" {
		t.Errorf("EventType = %q, want %q", got, "lesson_learned")
	}
	var payload map[string]any
	if err := json.Unmarshal(calls[0].Payload, &payload); err != nil {
		t.Fatalf("LogAppendRequest.Payload unmarshal: %v; raw = %s", err, string(calls[0].Payload))
	}
	payloadJSON := string(calls[0].Payload)
	if !strings.Contains(payloadJSON, forcedToolName) {
		t.Errorf("Payload missing forced tool name %q; payload = %s", forcedToolName, payloadJSON)
	}
	if !strings.Contains(payloadJSON, errToolBoom.Error()) {
		t.Errorf("Payload missing forced error message %q; payload = %s", errToolBoom.Error(), payloadJSON)
	}
}

// assertB5NotebookLessonRow pins the notebook half of B5 bullet 6: a
// single lesson row whose Category, ActiveAfter (= t0 +
// runtime.DefaultCoolingOff), EvidenceLogRef (cross-reference to
// keepEventID), ToolVersion, Subject, and Content all reflect the
// forced tool failure. Caveat on `recallAllLessons`: it runs an ANN
// recall capped at TopK=5, not a deterministic enumeration; fine for
// this single-row fixture but a future expansion seeding >5 rows
// needs a `recallAllLessonsBounded` variant or a direct enumeration
// path (iter-1 codex Major #3 caveat).
func assertB5NotebookLessonRow(
	t *testing.T,
	db *notebook.DB,
	t0 time.Time,
	wantEvidenceLogRef, forcedToolName, forcedToolVersion string,
) {
	t.Helper()
	entries := recallAllLessons(t, db, t0.Add(48*time.Hour))
	if len(entries) != 1 {
		t.Fatalf("recallAllLessons count = %d, want 1 (exactly one lesson per forced tool error)", len(entries))
	}
	entry := entries[0]
	if entry.Category != notebook.CategoryLesson {
		t.Errorf("Category = %q, want %q", entry.Category, notebook.CategoryLesson)
	}
	// Binding-time tie to the production default via the exported
	// constant — a future bump to `runtime.DefaultCoolingOff` updates
	// the assertion in lockstep without re-declaring a coupled literal.
	wantActiveAfter := t0.Add(runtime.DefaultCoolingOff).UnixMilli()
	if entry.ActiveAfter != wantActiveAfter {
		t.Errorf("ActiveAfter = %d (%s), want %d (%s — t0 + runtime.DefaultCoolingOff)",
			entry.ActiveAfter, time.UnixMilli(entry.ActiveAfter).UTC().Format(time.RFC3339),
			wantActiveAfter, time.UnixMilli(wantActiveAfter).UTC().Format(time.RFC3339))
	}
	if entry.EvidenceLogRef == nil {
		t.Errorf("EvidenceLogRef = nil, want pointer to %q (lesson must cross-reference its keepers-log event)", wantEvidenceLogRef)
	} else if *entry.EvidenceLogRef != wantEvidenceLogRef {
		t.Errorf("EvidenceLogRef = %q, want %q", *entry.EvidenceLogRef, wantEvidenceLogRef)
	}
	if entry.ToolVersion == nil {
		t.Errorf("ToolVersion = nil, want pointer to %q (M5.6.e hot-load needs-review machinery keys off this field)", forcedToolVersion)
	} else if *entry.ToolVersion != forcedToolVersion {
		t.Errorf("ToolVersion = %q, want %q", *entry.ToolVersion, forcedToolVersion)
	}
	if !strings.Contains(entry.Subject, forcedToolName) {
		t.Errorf("Subject = %q does not contain forced tool name %q", entry.Subject, forcedToolName)
	}
	if !strings.Contains(entry.Content, errToolBoom.Error()) {
		t.Errorf("Content = %q does not contain forced error message %q", entry.Content, errToolBoom.Error())
	}
}

// TestM5Verification_B5_AutoReflection_WiredRuntimeOptionOrderIndependent
// pins the M5.6.c wiring contract `NewWiredRuntime` documents:
// `WithToolErrorReflector` and `WithKeepersLogWriter` may be supplied
// in either order, and the constructor's post-options pass threads
// the writer into the reflector regardless. Without this sub-test the
// primary auto-reflection scenario only exercises one permutation
// (reflector-then-writer); a regression that made the constructor
// order-sensitive while preserving that exact permutation would slip
// through.
//
// Same assertions as the primary scenario, except scoped to the
// keepers-log + lesson cross-reference (the rest is already covered
// above). The fakeAgentRuntime is re-instantiated per sub-test so
// recording counts are independent.
func TestM5Verification_B5_AutoReflection_WiredRuntimeOptionOrderIndependent(t *testing.T) {
	// no t.Parallel: notebook.Open → WATCHKEEPER_DATA → t.Setenv.
	t.Setenv("WATCHKEEPER_DATA", t.TempDir())

	db, err := notebook.Open(context.Background(), reflectorAgentID)
	if err != nil {
		t.Fatalf("notebook.Open(%q): %v", reflectorAgentID, err)
	}
	t.Cleanup(func() { _ = db.Close() })

	t0 := time.Now().Truncate(time.Second)
	const keepEventID = "b5-keep-event-id-002"
	const forcedToolName = "sandbox.exec"
	const forcedToolVersion = "v1.0.0"

	keep := &recordingKeepClient{resp: &keepclient.LogAppendResponse{ID: keepEventID}}
	writer := keeperslog.New(keep)

	reflector, err := runtime.NewToolErrorReflector(
		db,
		runtime.WithEmbedder(llm.NewFakeEmbeddingProvider()),
		runtime.WithClock(fixedClock(t0)),
	)
	if err != nil {
		t.Fatalf("NewToolErrorReflector: %v", err)
	}

	fake := newFakeAgentRuntime(t, errToolBoom)
	rt, err := fake.Start(context.Background(), runtime.Manifest{
		AgentID:      reflectorAgentID,
		SystemPrompt: "test system prompt",
		Model:        "claude-sonnet-4",
		Toolset:      runtime.Toolset{{Name: forcedToolName}},
	})
	if err != nil {
		t.Fatalf("fake.Start: %v", err)
	}

	// REVERSED option order: writer first, reflector last. The
	// constructor's post-options decoration pass must still wire the
	// writer into the reflector for `evidence_log_ref` to land on the
	// lesson row.
	wired := runtime.NewWiredRuntime(
		fake,
		runtime.WithKeepersLogWriter(writer),
		runtime.WithToolErrorReflector(reflector),
		runtime.WithAgentID(reflectorAgentID),
	)

	_, gotErr := wired.InvokeTool(context.Background(), rt.ID(), runtime.ToolCall{
		Name:        forcedToolName,
		ToolVersion: forcedToolVersion,
		Arguments:   map[string]any{"cmd": "ls"},
	})
	if !errors.Is(gotErr, errToolBoom) {
		t.Fatalf("InvokeTool err = %v, want errors.Is errToolBoom", gotErr)
	}

	if got := keep.callCount(); got != 1 {
		t.Fatalf("keep.callCount() = %d, want 1 under REVERSED option order", got)
	}
	entries := recallAllLessons(t, db, t0.Add(48*time.Hour))
	if len(entries) != 1 {
		t.Fatalf("recallAllLessons count = %d, want 1 under REVERSED option order", len(entries))
	}
	entry := entries[0]
	if entry.EvidenceLogRef == nil || *entry.EvidenceLogRef != keepEventID {
		t.Errorf("EvidenceLogRef = %v, want pointer to %q under REVERSED option order (constructor's writer-into-reflector decoration must run regardless of declaration order)",
			entry.EvidenceLogRef, keepEventID)
	}
}
