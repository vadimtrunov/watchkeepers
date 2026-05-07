package llm

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// turnTestAgentID is a canonical RFC-4122 UUID used by the BuildTurnRequest
// tests. Re-used across cases so the on-disk filename pattern is stable.
const turnTestAgentID = "33333333-4444-5555-6666-777777777777"

// testCtx returns a fresh background context for tests; tiny helper to keep
// each test call site uncluttered.
func testCtx() context.Context { return context.Background() }

// turnValidManifest returns a [runtime.Manifest] suitable for happy-path
// BuildTurnRequest calls. Tests that need a different shape modify the
// returned value before passing it to the helper.
//
// NotebookRelevanceThreshold is deliberately 0 (disabled) so the base
// manifest exercises the no-filter path. Tests that specifically validate
// threshold behaviour set their own value before calling BuildTurnRequest.
func turnValidManifest() runtime.Manifest {
	return runtime.Manifest{
		AgentID:                    turnTestAgentID,
		SystemPrompt:               "You are a test agent.",
		Model:                      "claude-sonnet-4",
		Autonomy:                   runtime.AutonomySupervised,
		NotebookTopK:               5,
		NotebookRelevanceThreshold: 0,
	}
}

// turnKnownEmbedding returns a deterministic length-1536 float32 slice whose
// entries are all 0.1. Used by threshold tests that need an embedding shared
// between the inserted notebook entry and the embedder so Distance == 0 and
// Score == 1.0 after projection.
func turnKnownEmbedding() []float32 {
	v := make([]float32, notebook.EmbeddingDim)
	for i := range v {
		v[i] = 0.1
	}
	return v
}

// insertKnownEmbeddingEntry seeds the notebook with an entry whose embedding
// equals turnKnownEmbedding(). A query embedder that returns the same vector
// will recall this entry with Distance == 0 and projected Score == 1.0.
func insertKnownEmbeddingEntry(t *testing.T, db *notebook.DB) {
	t.Helper()
	_, err := db.Remember(testCtx(), notebook.Entry{
		Category:  notebook.CategoryLesson,
		Subject:   "known-subject",
		Content:   "known-content",
		Embedding: turnKnownEmbedding(),
	})
	if err != nil {
		t.Fatalf("Remember (known embedding): %v", err)
	}
}

// pinTurnDataDir points $WATCHKEEPER_DATA at a fresh `t.TempDir()` so the
// supervisor's per-agent SQLite files land in a sandboxed tree the test
// framework cleans up. Mirrors the helper used in
// notebook_supervisor_test.go.
func pinTurnDataDir(t *testing.T) {
	t.Helper()
	t.Setenv("WATCHKEEPER_DATA", t.TempDir())
}

// openTurnSupervisor returns a fresh supervisor with the test agent's
// notebook opened. The cleanup hook closes it. Returns (sup, db) so the
// caller can seed the notebook before invoking BuildTurnRequest.
func openTurnSupervisor(t *testing.T) (*runtime.NotebookSupervisor, *notebook.DB) {
	t.Helper()
	sup := runtime.NewNotebookSupervisor()
	db, err := sup.Open(turnTestAgentID)
	if err != nil {
		t.Fatalf("supervisor.Open: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close(turnTestAgentID) })
	return sup, db
}

// insertOneTurnEntry seeds the agent's notebook with a single entry so a
// happy-path Recall returns a non-empty slice. The embedding is a
// deterministic length-1536 vector (every element 1.5) — well-formed
// numerically; the recall query just needs SOMETHING in the table.
func insertOneTurnEntry(t *testing.T, db *notebook.DB) {
	t.Helper()
	emb := make([]float32, notebook.EmbeddingDim)
	for i := range emb {
		emb[i] = 1.5
	}
	_, err := db.Remember(testCtx(), notebook.Entry{
		Category:  notebook.CategoryLesson,
		Subject:   "test-subject",
		Content:   "test-content",
		Embedding: emb,
	})
	if err != nil {
		t.Fatalf("Remember: %v", err)
	}
}

// TestBuildTurnRequest_HappyPath_RecallsAndApplies pins the canonical
// success path: topK > 0, supervisor knows the agent, embed succeeds,
// recall returns at least one match, and the recalled-memory block is
// appended to the System slot.
func TestBuildTurnRequest_HappyPath_RecallsAndApplies(t *testing.T) {
	pinTurnDataDir(t)
	sup, db := openTurnSupervisor(t)
	insertOneTurnEntry(t, db)

	embedder := NewFakeEmbeddingProvider()
	manifest := turnValidManifest()

	req, err := BuildTurnRequest(testCtx(), manifest, "what did we learn?", embedder, sup)
	if err != nil {
		t.Fatalf("BuildTurnRequest: %v", err)
	}
	if req == nil {
		t.Fatal("BuildTurnRequest returned nil request")
	}
	if got := req.Metadata[MetadataKeyRecalledMemoryStatus]; got != RecalledMemoryStatusApplied {
		t.Errorf("Metadata[%q] = %q, want %q", MetadataKeyRecalledMemoryStatus, got, RecalledMemoryStatusApplied)
	}
	if !strings.Contains(req.System, RecalledMemoryHeader) {
		t.Errorf("System missing recalled-memory header; System = %q", req.System)
	}
	if !strings.Contains(req.System, "test-content") {
		t.Errorf("System missing inserted content; System = %q", req.System)
	}
}

// TestBuildTurnRequest_TopKZero_DisabledStatus pins the
// `disabled_topk_zero` branch: when manifest.NotebookTopK is 0 recall is
// skipped entirely and System is unchanged from the manifest's
// SystemPrompt.
func TestBuildTurnRequest_TopKZero_DisabledStatus(t *testing.T) {
	pinTurnDataDir(t)
	sup, _ := openTurnSupervisor(t)

	manifest := turnValidManifest()
	manifest.NotebookTopK = 0

	req, err := BuildTurnRequest(testCtx(), manifest, "q", NewFakeEmbeddingProvider(), sup)
	if err != nil {
		t.Fatalf("BuildTurnRequest: %v", err)
	}
	if got := req.Metadata[MetadataKeyRecalledMemoryStatus]; got != RecalledMemoryStatusDisabled {
		t.Errorf("Metadata = %q, want %q", got, RecalledMemoryStatusDisabled)
	}
	if req.System != manifest.SystemPrompt {
		t.Errorf("System = %q, want %q (unchanged)", req.System, manifest.SystemPrompt)
	}
}

// TestBuildTurnRequest_AgentNotRegistered_Status pins the
// `agent_not_registered` branch: the supervisor has never opened the
// agent, so Lookup returns false. Embed and recall are skipped.
func TestBuildTurnRequest_AgentNotRegistered_Status(t *testing.T) {
	pinTurnDataDir(t)
	sup := runtime.NewNotebookSupervisor()
	// No Open call — agent absent from registry.

	req, err := BuildTurnRequest(testCtx(), turnValidManifest(), "q", NewFakeEmbeddingProvider(), sup)
	if err != nil {
		t.Fatalf("BuildTurnRequest: %v", err)
	}
	if got := req.Metadata[MetadataKeyRecalledMemoryStatus]; got != RecalledMemoryStatusAgentNotRegistered {
		t.Errorf("Metadata = %q, want %q", got, RecalledMemoryStatusAgentNotRegistered)
	}
	if req.System != turnValidManifest().SystemPrompt {
		t.Errorf("System = %q, want unchanged", req.System)
	}
}

// TestBuildTurnRequest_EmbedError_FailSoft pins the `embed_error` branch:
// embedder returns an error; the request is still usable; the helper's
// returned error joins the embed error so strict-mode callers see it.
func TestBuildTurnRequest_EmbedError_FailSoft(t *testing.T) {
	pinTurnDataDir(t)
	sup, _ := openTurnSupervisor(t)

	sentinel := errors.New("embed: provider unavailable")
	embedder := NewFakeEmbeddingProvider(WithEmbedError(sentinel))

	req, err := BuildTurnRequest(testCtx(), turnValidManifest(), "q", embedder, sup)
	if req == nil {
		t.Fatal("request is nil despite fail-soft contract")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false, got %v", err)
	}
	if got := req.Metadata[MetadataKeyRecalledMemoryStatus]; got != RecalledMemoryStatusEmbedError {
		t.Errorf("Metadata = %q, want %q", got, RecalledMemoryStatusEmbedError)
	}
}

// TestBuildTurnRequest_RecallError_FailSoft pins the `recall_error` branch:
// the notebook DB is closed before BuildTurnRequest is called, so
// db.Recall errors. The request is still usable; the helper's returned
// error joins the recall error.
func TestBuildTurnRequest_RecallError_FailSoft(t *testing.T) {
	pinTurnDataDir(t)
	sup, db := openTurnSupervisor(t)
	// Close the underlying *notebook.DB without removing it from the
	// supervisor's registry. supervisor.Lookup still returns the closed
	// handle; the subsequent db.Recall fails with an "sql: database is
	// closed" error.
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	req, err := BuildTurnRequest(testCtx(), turnValidManifest(), "q", NewFakeEmbeddingProvider(), sup)
	if req == nil {
		t.Fatal("request is nil despite fail-soft contract")
	}
	if err == nil {
		t.Errorf("returned error is nil; want non-nil joined recall error")
	}
	if got := req.Metadata[MetadataKeyRecalledMemoryStatus]; got != RecalledMemoryStatusRecallError {
		t.Errorf("Metadata = %q, want %q", got, RecalledMemoryStatusRecallError)
	}
}

// TestBuildTurnRequest_NoMatches_Status pins the `no_matches` branch:
// recall succeeds but the table is empty, so recall returns []. No
// recalled-memory block is appended; no error is returned.
func TestBuildTurnRequest_NoMatches_Status(t *testing.T) {
	pinTurnDataDir(t)
	sup, _ := openTurnSupervisor(t)
	// No insertOneTurnEntry — notebook is empty.

	req, err := BuildTurnRequest(testCtx(), turnValidManifest(), "q", NewFakeEmbeddingProvider(), sup)
	if err != nil {
		t.Fatalf("BuildTurnRequest: %v", err)
	}
	if got := req.Metadata[MetadataKeyRecalledMemoryStatus]; got != RecalledMemoryStatusNoMatches {
		t.Errorf("Metadata = %q, want %q", got, RecalledMemoryStatusNoMatches)
	}
	if strings.Contains(req.System, RecalledMemoryHeader) {
		t.Errorf("System contains recalled-memory header on no-matches path; System = %q", req.System)
	}
}

// TestBuildTurnRequest_CtxCancelledAtEntry_ReturnsCtxErr pins the only
// hard-error case: a pre-cancelled context returns (nil, ctx.Err()) before
// any embed / recall work happens.
func TestBuildTurnRequest_CtxCancelledAtEntry_ReturnsCtxErr(t *testing.T) {
	pinTurnDataDir(t)
	sup := runtime.NewNotebookSupervisor()

	ctx, cancel := context.WithCancel(testCtx())
	cancel() // cancel before calling BuildTurnRequest

	req, err := BuildTurnRequest(ctx, turnValidManifest(), "q", NewFakeEmbeddingProvider(), sup)
	if req != nil {
		t.Errorf("request = %+v, want nil on ctx-cancelled-at-entry", req)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("errors.Is(err, context.Canceled) = false, got %v", err)
	}
}

// TestBuildTurnRequest_NegativeTopK_DisabledStatus pins that negative TopK
// is treated identically to zero: auto-recall is disabled and the status is
// RecalledMemoryStatusDisabled. Negative values are a manifest-shape pathology
// and must NOT fall through to the recall pipeline (which would surface as a
// recall_error instead).
func TestBuildTurnRequest_NegativeTopK_DisabledStatus(t *testing.T) {
	pinTurnDataDir(t)
	sup, _ := openTurnSupervisor(t)

	manifest := turnValidManifest()
	manifest.NotebookTopK = -1

	req, err := BuildTurnRequest(testCtx(), manifest, "q", NewFakeEmbeddingProvider(), sup)
	if err != nil {
		t.Fatalf("BuildTurnRequest: %v", err)
	}
	if got := req.Metadata[MetadataKeyRecalledMemoryStatus]; got != RecalledMemoryStatusDisabled {
		t.Errorf("Metadata = %q, want %q (negative TopK should disable recall)", got, RecalledMemoryStatusDisabled)
	}
	if req.System != manifest.SystemPrompt {
		t.Errorf("System = %q, want %q (unchanged)", req.System, manifest.SystemPrompt)
	}
}

// TestBuildTurnRequest_RelevanceThresholdPostFilter pins that recalled
// memories are post-filtered by Score >= manifest.NotebookRelevanceThreshold
// before injection. Entries whose projected score falls below the threshold
// are excluded; when all are excluded the status falls to "no_matches".
//
// Both sub-tests use an embedder that returns turnKnownEmbedding() and an
// entry stored with the same vector, so Distance == 0 and projected Score ==
// 1.0 after the distance→relevance conversion. This makes the threshold
// assertions deterministic regardless of the underlying ANN metric.
func TestBuildTurnRequest_RelevanceThresholdPostFilter(t *testing.T) {
	pinTurnDataDir(t)

	// knownEmbedder always returns turnKnownEmbedding() for any query so
	// the stored entry (also turnKnownEmbedding()) has Distance == 0 → Score == 1.0.
	knownEmbedder := NewFakeEmbeddingProvider(WithEmbedFunc(func(_ context.Context, _ string) ([]float32, error) {
		return turnKnownEmbedding(), nil
	}))

	t.Run("some_pass_threshold", func(t *testing.T) {
		sup, db := openTurnSupervisor(t)
		insertKnownEmbeddingEntry(t, db)

		manifest := turnValidManifest()
		manifest.NotebookRelevanceThreshold = 0.7 // Score=1.0 >= 0.7 → should pass

		req, err := BuildTurnRequest(testCtx(), manifest, "q", knownEmbedder, sup)
		if err != nil {
			t.Fatalf("BuildTurnRequest: %v", err)
		}
		if got := req.Metadata[MetadataKeyRecalledMemoryStatus]; got != RecalledMemoryStatusApplied {
			t.Errorf("Metadata = %q, want %q", got, RecalledMemoryStatusApplied)
		}
		if !strings.Contains(req.System, RecalledMemoryHeader) {
			t.Errorf("System missing recalled-memory header; System = %q", req.System)
		}
	})

	t.Run("all_filtered_by_threshold", func(t *testing.T) {
		sup, db := openTurnSupervisor(t)
		insertKnownEmbeddingEntry(t, db)

		manifest := turnValidManifest()
		manifest.NotebookRelevanceThreshold = 1.1 // Score cannot exceed 1.0 → all filtered

		req, err := BuildTurnRequest(testCtx(), manifest, "q", knownEmbedder, sup)
		if err != nil {
			t.Fatalf("BuildTurnRequest: %v", err)
		}
		if got := req.Metadata[MetadataKeyRecalledMemoryStatus]; got != RecalledMemoryStatusNoMatches {
			t.Errorf("Metadata = %q, want %q (all filtered)", got, RecalledMemoryStatusNoMatches)
		}
		if strings.Contains(req.System, RecalledMemoryHeader) {
			t.Errorf("System contains recalled-memory header when all filtered; System = %q", req.System)
		}
	})
}

// TestBuildTurnRequest_NilEmbedder_FailSoft pins the nil-embedder branch:
// when embedder is nil the helper returns a usable request with status
// embed_error and a non-nil error — it must not panic.
func TestBuildTurnRequest_NilEmbedder_FailSoft(t *testing.T) {
	pinTurnDataDir(t)
	sup, _ := openTurnSupervisor(t)

	req, err := BuildTurnRequest(testCtx(), turnValidManifest(), "q", nil, sup)
	if req == nil {
		t.Fatal("request is nil despite fail-soft contract (nil embedder)")
	}
	if err == nil {
		t.Error("returned error is nil; want non-nil for nil embedder")
	}
	if got := req.Metadata[MetadataKeyRecalledMemoryStatus]; got != RecalledMemoryStatusEmbedError {
		t.Errorf("Metadata = %q, want %q", got, RecalledMemoryStatusEmbedError)
	}
}
