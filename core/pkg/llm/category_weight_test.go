package llm

import (
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

// TestCategoryAutoInjectionWeights_PinsLessonAndObservationDefaults
// is the binding-time tie for the M7.2 policy: lesson stays at 1.0,
// observation halves to 0.5. A future regression that flips the
// numbers (or accidentally drops one of the categories) is caught
// here before it lands in production.
func TestCategoryAutoInjectionWeights_PinsLessonAndObservationDefaults(t *testing.T) {
	t.Parallel()
	if got := CategoryAutoInjectionWeights[notebook.CategoryLesson]; got != 1.0 {
		t.Errorf("lesson weight = %v, want 1.0", got)
	}
	if got := CategoryAutoInjectionWeights[notebook.CategoryObservation]; got != 0.5 {
		t.Errorf("observation weight = %v, want 0.5", got)
	}
}

// TestRecallResultsToMemories_AppliesCategoryWeights verifies the
// projection scales the relevance score by the per-category weight.
// At distance 0 the raw score is 1.0; a lesson stays at 1.0, an
// observation halves to 0.5. The result is the score the
// [BuildTurnRequest] threshold filter sees.
func TestRecallResultsToMemories_AppliesCategoryWeights(t *testing.T) {
	t.Parallel()
	in := []notebook.RecallResult{
		{Category: notebook.CategoryLesson, Subject: "L", Content: "lesson body", Distance: 0.0},
		{Category: notebook.CategoryObservation, Subject: "O", Content: "obs body", Distance: 0.0},
	}
	got := recallResultsToMemories(in)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].Category != notebook.CategoryLesson || got[0].Score != 1.0 {
		t.Errorf("lesson projection = %+v, want Score=1.0", got[0])
	}
	if got[1].Category != notebook.CategoryObservation || got[1].Score != 0.5 {
		t.Errorf("observation projection = %+v, want Score=0.5", got[1])
	}
}

// TestRecallResultsToMemories_UnknownCategoryDefaultsTo1 verifies a
// row whose Category is absent from [CategoryAutoInjectionWeights]
// (e.g. "preference", "pending_task", "relationship_note") gets the
// neutral 1.0 multiplier — only the categories explicitly named in
// the policy map are down-weighted.
func TestRecallResultsToMemories_UnknownCategoryDefaultsTo1(t *testing.T) {
	t.Parallel()
	in := []notebook.RecallResult{
		{Category: notebook.CategoryPreference, Subject: "P", Content: "pref body", Distance: 0.0},
	}
	got := recallResultsToMemories(in)
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].Score != 1.0 {
		t.Errorf("preference Score = %v, want 1.0 (no scaling)", got[0].Score)
	}
}

// TestRecallResultsToMemories_WeightedThresholdDropsObservationKeepsLesson
// is the M7.2 acceptance plus a binding-time tie: at a mid-range
// distance both rows would otherwise pass the threshold, but the
// observation's halved score falls below it, while the lesson's
// stays above. This is exactly the policy outcome the M7.2 roadmap
// pins: "lower auto-injection weight for observation than lesson".
func TestRecallResultsToMemories_WeightedThresholdDropsObservationKeepsLesson(t *testing.T) {
	t.Parallel()
	// distance 0.6 → raw score = 1 - 0.6/2 = 0.7.
	// lesson weighted = 0.7 * 1.0 = 0.7 → passes threshold 0.5.
	// observation weighted = 0.7 * 0.5 = 0.35 → below threshold 0.5.
	in := []notebook.RecallResult{
		{Category: notebook.CategoryLesson, Subject: "L", Content: "lesson body", Distance: 0.6},
		{Category: notebook.CategoryObservation, Subject: "O", Content: "obs body", Distance: 0.6},
	}
	memories := recallResultsToMemories(in)
	if len(memories) != 2 {
		t.Fatalf("len = %d, want 2", len(memories))
	}
	// Apply threshold 0.5 inline (the production threshold filter
	// lives in resolveRecalledMemory; we replicate it here so the
	// test pins the policy at the projection layer regardless of
	// the threshold filter's location).
	const threshold = float32(0.5)
	passed := make([]RecalledMemory, 0, 2)
	for _, m := range memories {
		if m.Score >= threshold {
			passed = append(passed, m)
		}
	}
	if len(passed) != 1 {
		t.Fatalf("len(passed) = %d, want 1 (only lesson)", len(passed))
	}
	if passed[0].Category != notebook.CategoryLesson {
		t.Errorf("passed[0].Category = %q, want lesson", passed[0].Category)
	}
}

// TestRecallResultsToMemories_PreservesCategoryField verifies the
// projection copies [notebook.RecallResult.Category] through to
// [RecalledMemory.Category] verbatim — downstream tooling can read
// the category without re-querying the notebook.
func TestRecallResultsToMemories_PreservesCategoryField(t *testing.T) {
	t.Parallel()
	in := []notebook.RecallResult{
		{Category: notebook.CategoryLesson, Subject: "s", Content: "c", Distance: 0},
		{Category: notebook.CategoryObservation, Subject: "s", Content: "c", Distance: 0},
		{Category: notebook.CategoryPreference, Subject: "s", Content: "c", Distance: 0},
	}
	got := recallResultsToMemories(in)
	wantCats := []string{
		notebook.CategoryLesson,
		notebook.CategoryObservation,
		notebook.CategoryPreference,
	}
	for i, c := range wantCats {
		if got[i].Category != c {
			t.Errorf("got[%d].Category = %q, want %q", i, got[i].Category, c)
		}
	}
}
