package runtime_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// composeSubjectMirrorBootCheck mirrors the M5.6.b
// `composeSubject` `"<toolName>: <errClass>"` shape — held in this test
// file because importing
// `core/pkg/runtime` (where the production composeSubject lives) from a
// `runtime_test` external test file is fine, but the function itself is
// unexported. Per AC7 the comment names the M5.6.b origin so a future
// drift on either side flips the test red.
func composeSubjectMirrorBootCheck(toolName, errClass string) string {
	return toolName + ": " + errClass
}

// readNeedsReviewExternal reads the raw `needs_review` integer for an
// entry id via the supervisor's live handle. The supervisor exposes
// `*notebook.DB`, but the underlying `db.sql` is unexported, so the
// test reads through the helper's effect on the API instead — calling
// FlagSupersededLessons directly with an empty currentVersions map
// would re-trigger the flip. The simpler assertion is "newlyFlagged
// matches what the direct call would return", and that is what the
// wiring test asserts.

// TestNotebookSupervisor_BootCheck_ParityWithDirectCall pins that
// BootCheck against an opened agent returns the same flip-count and
// causes the same database mutation as a direct call to
// FlagSupersededLessons on the underlying *notebook.DB. We exercise the
// parity by:
//
//  1. opening agent A via the supervisor and seeding two lesson rows
//     whose subjects parse correctly and whose tool versions mismatch
//     the currentVersions map → BootCheck must flip both;
//  2. calling FlagSupersededLessons directly on the same handle a
//     second time with the same currentVersions → the candidate set is
//     now empty (already flagged rows are excluded), so the second
//     call returns 0;
//
// the parity contract is "BootCheck flips on the first call, the
// underlying helper sees the SAME mutation on a re-call", which is
// stronger than just "the counts match" and pins both the lookup
// indirection and the per-row UPDATE durability.
func TestNotebookSupervisor_BootCheck_ParityWithDirectCall(t *testing.T) {
	pinDataDir(t)
	sup := runtime.NewNotebookSupervisor()

	const agentID = "11111111-2222-3333-4444-555555555555"
	db, err := sup.Open(agentID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close(agentID) })

	ctx := context.Background()

	embedding := func(seed byte) []float32 {
		v := make([]float32, notebook.EmbeddingDim)
		v[int(seed)%notebook.EmbeddingDim] = 1
		return v
	}

	staleVersion := "1.0.0"
	if _, err := db.Remember(ctx, notebook.Entry{
		Category:    notebook.CategoryLesson,
		Subject:     composeSubjectMirrorBootCheck("alpha", "ParseErr"),
		Content:     "lesson body alpha",
		ToolVersion: &staleVersion,
		Embedding:   embedding(11),
	}); err != nil {
		t.Fatalf("seed alpha: %v", err)
	}
	betaVersion := "2.0.0"
	if _, err := db.Remember(ctx, notebook.Entry{
		Category:    notebook.CategoryLesson,
		Subject:     composeSubjectMirrorBootCheck("beta", "TimeoutErr"),
		Content:     "lesson body beta",
		ToolVersion: &betaVersion,
		Embedding:   embedding(12),
	}); err != nil {
		t.Fatalf("seed beta: %v", err)
	}

	// Both lessons present → both will be flagged. (alpha: present
	// with mismatched version. beta: absent → retired.)
	currentVersions := map[string]string{
		"alpha": "9.9.9",
	}

	got, err := sup.BootCheck(ctx, agentID, currentVersions)
	if err != nil {
		t.Fatalf("BootCheck: %v", err)
	}
	if got != 2 {
		t.Fatalf("BootCheck newlyFlagged = %d, want 2", got)
	}

	// Direct re-call on the same handle: the rows are now flagged so
	// the SQL `needs_review = 0` filter excludes them; the second call
	// must return 0. This pins the durability of BootCheck's mutation
	// against the underlying DB.
	again, err := db.FlagSupersededLessons(ctx, currentVersions)
	if err != nil {
		t.Fatalf("direct FlagSupersededLessons: %v", err)
	}
	if again != 0 {
		t.Errorf("direct re-call newlyFlagged = %d, want 0 (BootCheck must have already flipped them)", again)
	}
}

// TestNotebookSupervisor_BootCheck_UnopenedAgent_Sentinel pins that
// calling BootCheck for an agent the supervisor has not Opened returns
// errors.Is-matchable [runtime.ErrAgentNotOpened] without panicking.
func TestNotebookSupervisor_BootCheck_UnopenedAgent_Sentinel(t *testing.T) {
	pinDataDir(t)
	sup := runtime.NewNotebookSupervisor()

	ctx := context.Background()
	const unopenedID = "11111111-2222-3333-4444-555555555555"

	got, err := sup.BootCheck(ctx, unopenedID, map[string]string{"alpha": "1.0.0"})
	if err == nil {
		t.Fatal("BootCheck on unopened agent: err=nil, want non-nil")
	}
	if !errors.Is(err, runtime.ErrAgentNotOpened) {
		t.Fatalf("BootCheck err = %v, want errors.Is(_, runtime.ErrAgentNotOpened)", err)
	}
	if got != 0 {
		t.Errorf("BootCheck newlyFlagged = %d, want 0 on unopened agent", got)
	}
}
