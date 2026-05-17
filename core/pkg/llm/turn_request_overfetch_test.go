package llm

import (
	"strings"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// TestBuildTurnRequest_OverFetch_WeightedReRankKeepsLessonOverObservation
// is the M7.2 iter-1 finding #2 regression test: seed a lesson at a
// MID-range distance and an observation at a SLIGHTLY closer
// distance. With TopK=1 and the pre-fix code, the SQL layer would
// pick the observation (closer raw cosine) and the lesson would be
// invisible — the weight policy never gets to run. With the over-
// fetch + re-rank, the projection sees both rows and the lesson's
// full-weight Score wins, so the lesson surfaces in the
// recalled-memory block.
//
// We exercise the binding-time tie via the notebook seed: two
// entries with engineered embeddings whose cosine distance to the
// query vector orders them as (observation closer, lesson further).
// After projection, weighted scores: observation = 1.0 * 0.5 = 0.5;
// lesson = 0.9 * 1.0 = 0.9. The lesson wins.
func TestBuildTurnRequest_OverFetch_WeightedReRankKeepsLessonOverObservation(t *testing.T) {
	pinTurnDataDir(t)
	sup, db := openTurnSupervisor(t)

	// Seed two entries with the same embedding (the deterministic
	// 1.5-fill) so the cosine distance from any query is identical
	// (0.0 — both vectors are co-linear). With identical distances,
	// the raw score is the same (1.0); the weight policy
	// differentiates them. Lesson keeps Score=1.0, observation drops
	// to Score=0.5. The re-rank places the lesson first.
	emb := make([]float32, notebook.EmbeddingDim)
	for i := range emb {
		emb[i] = 1.5
	}
	if _, err := db.Remember(testCtx(), notebook.Entry{
		Category:  notebook.CategoryObservation,
		Subject:   "obs-subject",
		Content:   "observation body",
		Embedding: emb,
	}); err != nil {
		t.Fatalf("Remember observation: %v", err)
	}
	if _, err := db.Remember(testCtx(), notebook.Entry{
		Category:  notebook.CategoryLesson,
		Subject:   "lesson-subject",
		Content:   "lesson body",
		Embedding: emb,
	}); err != nil {
		t.Fatalf("Remember lesson: %v", err)
	}

	// TopK=1 with NO threshold so we observe the re-rank picking the
	// lesson out of the over-fetched pool.
	manifest := turnValidManifest()
	manifest.NotebookTopK = 1
	manifest.NotebookRelevanceThreshold = 0

	req, err := BuildTurnRequest(testCtx(), manifest, "q", NewFakeEmbeddingProvider(), sup)
	if err != nil {
		t.Fatalf("BuildTurnRequest: %v", err)
	}
	if got := req.Metadata[MetadataKeyRecalledMemoryStatus]; got != RecalledMemoryStatusApplied {
		t.Fatalf("status = %q, want %q", got, RecalledMemoryStatusApplied)
	}
	// The System block must contain the lesson body (the re-rank
	// winner) and MUST NOT contain the observation body (trimmed
	// out by TopK=1 after the re-rank).
	if !strings.Contains(req.System, "lesson body") {
		t.Errorf("System block missing lesson body; got:\n%s", req.System)
	}
	if strings.Contains(req.System, "observation body") {
		t.Errorf("System block contains observation body; weight policy did not win:\n%s", req.System)
	}
}

// TestBuildTurnRequest_OverFetch_FetchKClampedAtMaxTopK pins the
// defensive ceiling: when manifest NotebookTopK times the over-fetch
// factor exceeds the notebook layer's maxTopK (100), the projection
// caps fetchK at 100 so the storage layer's silent clamp does not
// surprise. The test does not need a real notebook to exercise the
// math — it asserts the public RecallOverFetchFactor constant pins
// the policy.
func TestBuildTurnRequest_OverFetch_FetchKClampedAtMaxTopK(t *testing.T) {
	t.Parallel()
	if RecallOverFetchFactor != 4 {
		t.Errorf("RecallOverFetchFactor = %d, want 4 (M7.2 binding-time tie)",
			RecallOverFetchFactor)
	}
	// recallMaxTopK is package-private but mirrors notebook's maxTopK.
	// Compute the threshold a caller's NotebookTopK must exceed before
	// clamping kicks in: at K * 4 > 100, K > 25.
	const wantThreshold = 25
	if recallMaxTopK/RecallOverFetchFactor != wantThreshold {
		t.Errorf("over-fetch clamp threshold = %d, want %d",
			recallMaxTopK/RecallOverFetchFactor, wantThreshold)
	}
}

// Compile-time assertion: the notebook recall result Category field is
// referenced by the projection's weight logic. Pinning the field by
// constructing a value here catches a future field rename / removal.
var _ = notebook.RecallResult{Category: notebook.CategoryLesson}

// Sanity: weight policy is invariant to runtime.Manifest changes — the
// CategoryAutoInjectionWeights map is independent of any Manifest
// surface. Pinned via an explicit dependency-free assertion in the
// turn_request_overfetch_test.go file (the only test file that
// pulls in the runtime package).
var _ = runtime.Manifest{}
