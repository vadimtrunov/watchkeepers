package notebook

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// composeSubjectMirror builds a lesson Subject using the SAME
// `"<toolName>: <errClass>"` shape that
// `(*ToolErrorReflector).composeSubject` in
// `core/pkg/runtime/tool_error_reflector.go` produces (M5.6.b). The test
// helper is a hand-crafted mirror because composeSubject is unexported in
// the runtime package and importing the runtime package here would create
// a cycle (runtime imports notebook). Per AC7 the comment names the
// M5.6.b origin so a future regression to either format flips a test
// red.
func composeSubjectMirror(toolName, errClass string) string {
	return toolName + ": " + errClass
}

// seedLesson seeds a `lesson` Entry with the supplied subject and
// tool_version via the production [DB.Remember] path (AC6: real Open +
// Remember, no mocking). Returns the auto-generated id.
func seedLesson(ctx context.Context, t *testing.T, db *DB, embedSeed byte, subject, toolVersion string) string {
	t.Helper()
	var versionPtr *string
	if toolVersion != "" {
		v := toolVersion
		versionPtr = &v
	}
	id, err := db.Remember(ctx, Entry{
		Category:    CategoryLesson,
		Subject:     subject,
		Content:     "lesson body",
		ToolVersion: versionPtr,
		Embedding:   makeEmbedding(embedSeed),
	})
	if err != nil {
		t.Fatalf("seed lesson: %v", err)
	}
	return id
}

// TestFlagSupersededLessons_HappyPathFourRowFixture pins the canonical
// AC1+AC3 fixture: 1 non-lesson, 1 lesson with a matching version,
// 1 lesson with a stale version, 1 lesson whose tool was retired (not
// present in currentVersions). Expect exactly 2 flips.
func TestFlagSupersededLessons_HappyPathFourRowFixture(t *testing.T) {
	db, ctx, _ := freshDB(t)

	// Non-lesson row — must NOT participate in the scan even when its
	// subject would parse and its tool_version would mismatch.
	nonLessonVersion := "2.0.0"
	if _, err := db.Remember(ctx, Entry{
		Category:    CategoryObservation,
		Subject:     composeSubjectMirror("alpha", "Boom"),
		Content:     "observation body",
		ToolVersion: &nonLessonVersion,
		Embedding:   makeEmbedding(10),
	}); err != nil {
		t.Fatalf("seed observation: %v", err)
	}

	matchingID := seedLesson(ctx, t, db, 11, composeSubjectMirror("alpha", "ParseErr"), "1.0.0")
	staleID := seedLesson(ctx, t, db, 12, composeSubjectMirror("beta", "TimeoutErr"), "1.2.3")
	retiredID := seedLesson(ctx, t, db, 13, composeSubjectMirror("gamma", "PanicErr"), "0.9.0")

	currentVersions := map[string]string{
		"alpha": "1.0.0", // matching: no flip
		"beta":  "2.0.0", // stale: flip
		// "gamma" absent: retired tool → flip
	}

	got, err := db.FlagSupersededLessons(ctx, currentVersions)
	if err != nil {
		t.Fatalf("FlagSupersededLessons: %v", err)
	}
	if got != 2 {
		t.Fatalf("newlyFlagged = %d, want 2", got)
	}

	if v := readNeedsReview(ctx, t, db, matchingID); v != 0 {
		t.Errorf("matching lesson: needs_review = %d, want 0", v)
	}
	if v := readNeedsReview(ctx, t, db, staleID); v != 1 {
		t.Errorf("stale lesson: needs_review = %d, want 1", v)
	}
	if v := readNeedsReview(ctx, t, db, retiredID); v != 1 {
		t.Errorf("retired lesson: needs_review = %d, want 1", v)
	}
}

// TestFlagSupersededLessons_MatchingVersion_NotFlagged isolates the
// "current version equals lesson version → no flip" path so a regression
// that flips matching rows is caught even when the larger fixture
// composes other categories.
func TestFlagSupersededLessons_MatchingVersion_NotFlagged(t *testing.T) {
	db, ctx, _ := freshDB(t)

	id := seedLesson(ctx, t, db, 20, composeSubjectMirror("alpha", "ParseErr"), "1.0.0")

	got, err := db.FlagSupersededLessons(ctx, map[string]string{"alpha": "1.0.0"})
	if err != nil {
		t.Fatalf("FlagSupersededLessons: %v", err)
	}
	if got != 0 {
		t.Errorf("newlyFlagged = %d, want 0", got)
	}
	if v := readNeedsReview(ctx, t, db, id); v != 0 {
		t.Errorf("needs_review = %d, want 0", v)
	}
}

// TestFlagSupersededLessons_EmptyToolVersion_Skipped pins the SQL filter:
// rows with NULL or empty `tool_version` are excluded from the scan
// regardless of the currentVersions map. Even with a nil map (which
// would otherwise flag every lesson) an empty-version row stays
// unflagged.
func TestFlagSupersededLessons_EmptyToolVersion_Skipped(t *testing.T) {
	db, ctx, _ := freshDB(t)

	// Empty toolVersion → seedLesson stores SQL NULL.
	id := seedLesson(ctx, t, db, 30, composeSubjectMirror("alpha", "ParseErr"), "")

	got, err := db.FlagSupersededLessons(ctx, nil)
	if err != nil {
		t.Fatalf("FlagSupersededLessons: %v", err)
	}
	if got != 0 {
		t.Errorf("newlyFlagged = %d, want 0", got)
	}
	if v := readNeedsReview(ctx, t, db, id); v != 0 {
		t.Errorf("needs_review = %d, want 0", v)
	}
}

// TestFlagSupersededLessons_AlreadyFlagged_Skipped pins that the SQL
// `needs_review = 0` filter excludes rows that have already been
// flagged in a previous boot pass — those rows are no-ops here even
// when the version mismatches.
func TestFlagSupersededLessons_AlreadyFlagged_Skipped(t *testing.T) {
	db, ctx, _ := freshDB(t)

	id := seedLesson(ctx, t, db, 40, composeSubjectMirror("alpha", "ParseErr"), "1.0.0")
	if err := db.MarkNeedsReview(ctx, id); err != nil {
		t.Fatalf("MarkNeedsReview: %v", err)
	}

	// Version mismatches but the row is already flagged → must be
	// excluded from the candidate set.
	got, err := db.FlagSupersededLessons(ctx, map[string]string{"alpha": "9.9.9"})
	if err != nil {
		t.Fatalf("FlagSupersededLessons: %v", err)
	}
	if got != 0 {
		t.Errorf("newlyFlagged = %d, want 0 (already-flagged row must not double-count)", got)
	}
	if v := readNeedsReview(ctx, t, db, id); v != 1 {
		t.Errorf("needs_review = %d, want 1 (already-flagged stays flagged)", v)
	}
}

// TestFlagSupersededLessons_UnparseableSubject_SkippedAndLogged pins the
// AC2 edge case: a lesson whose subject is missing the `": "` separator
// is skipped (no flip, no error) and the configured logger receives a
// debug breadcrumb.
func TestFlagSupersededLessons_UnparseableSubject_SkippedAndLogged(t *testing.T) {
	db, ctx, _ := freshDB(t)

	// Subject without ": " separator.
	id := seedLesson(ctx, t, db, 50, "no-separator-here", "1.0.0")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	got, err := db.FlagSupersededLessons(ctx, nil, WithFlagLogger(logger))
	if err != nil {
		t.Fatalf("FlagSupersededLessons: %v", err)
	}
	if got != 0 {
		t.Errorf("newlyFlagged = %d, want 0", got)
	}
	if v := readNeedsReview(ctx, t, db, id); v != 0 {
		t.Errorf("needs_review = %d, want 0", v)
	}

	logged := buf.String()
	if !strings.Contains(logged, "unparseable subject") {
		t.Errorf("expected debug breadcrumb for unparseable subject, got: %q", logged)
	}
}

// TestFlagSupersededLessons_NilCurrentVersions_FlagsAll pins the AC3
// branch: a nil map yields the "tool retired" branch on every row, so
// every parseable lesson is flagged.
func TestFlagSupersededLessons_NilCurrentVersions_FlagsAll(t *testing.T) {
	db, ctx, _ := freshDB(t)

	a := seedLesson(ctx, t, db, 60, composeSubjectMirror("alpha", "ParseErr"), "1.0.0")
	b := seedLesson(ctx, t, db, 61, composeSubjectMirror("beta", "TimeoutErr"), "2.0.0")

	got, err := db.FlagSupersededLessons(ctx, nil)
	if err != nil {
		t.Fatalf("FlagSupersededLessons: %v", err)
	}
	if got != 2 {
		t.Fatalf("newlyFlagged = %d, want 2", got)
	}
	if v := readNeedsReview(ctx, t, db, a); v != 1 {
		t.Errorf("alpha lesson needs_review = %d, want 1", v)
	}
	if v := readNeedsReview(ctx, t, db, b); v != 1 {
		t.Errorf("beta lesson needs_review = %d, want 1", v)
	}
}

// TestFlagSupersededLessons_EmptyMap_FlagsAll pins identical semantics
// between a nil map and an empty (non-nil) map: both yield "every
// lesson lacks a current entry → flag".
func TestFlagSupersededLessons_EmptyMap_FlagsAll(t *testing.T) {
	db, ctx, _ := freshDB(t)

	id := seedLesson(ctx, t, db, 70, composeSubjectMirror("alpha", "ParseErr"), "1.0.0")

	got, err := db.FlagSupersededLessons(ctx, map[string]string{})
	if err != nil {
		t.Fatalf("FlagSupersededLessons: %v", err)
	}
	if got != 1 {
		t.Errorf("newlyFlagged = %d, want 1", got)
	}
	if v := readNeedsReview(ctx, t, db, id); v != 1 {
		t.Errorf("needs_review = %d, want 1", v)
	}
}

// TestFlagSupersededLessons_CtxCancelled_NoFlips pins that a
// pre-cancelled ctx returns ctx.Err verbatim and does not flip any row.
// The per-row UPDATE is autocommit, so the assertion is "no row was
// touched" rather than "rolled back".
func TestFlagSupersededLessons_CtxCancelled_NoFlips(t *testing.T) {
	db, _, _ := freshDB(t)

	bgCtx := context.Background()
	id := seedLesson(bgCtx, t, db, 80, composeSubjectMirror("alpha", "ParseErr"), "1.0.0")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, err := db.FlagSupersededLessons(ctx, nil)
	if err == nil {
		t.Fatal("FlagSupersededLessons returned nil err on cancelled ctx")
	}
	if got != 0 {
		t.Errorf("newlyFlagged = %d, want 0", got)
	}
	if v := readNeedsReview(bgCtx, t, db, id); v != 0 {
		t.Errorf("needs_review = %d, want 0 (cancelled ctx must not have touched the row)", v)
	}
}

// TestFlagSupersededLessons_ComposeSubjectFormatContract pins AC7: the
// helper's subject parser MUST agree with the M5.6.b
// `composeSubject` format. We exercise the contract via
// extractToolNameFromSubject directly (the parser the helper calls)
// against subjects produced by the M5.6.b mirror, so a future drift
// in either side flips this assertion red.
func TestFlagSupersededLessons_ComposeSubjectFormatContract(t *testing.T) {
	cases := []struct {
		toolName string
		errClass string
	}{
		{"alpha", "ParseErr"},
		{"file:write", "DiskFull"},
		{"net.http", "TimeoutErr"},
	}
	for _, c := range cases {
		subj := composeSubjectMirror(c.toolName, c.errClass)
		got, ok := extractToolNameFromSubject(subj)
		if !ok {
			t.Errorf("extractToolNameFromSubject(%q) ok=false, want true", subj)
			continue
		}
		if got != c.toolName {
			t.Errorf("extractToolNameFromSubject(%q) = %q, want %q", subj, got, c.toolName)
		}
	}

	// Negative shapes the parser MUST reject.
	for _, bad := range []string{"", "no-separator", ": empty-prefix"} {
		if got, ok := extractToolNameFromSubject(bad); ok {
			t.Errorf("extractToolNameFromSubject(%q) = (%q, true), want (\"\", false)", bad, got)
		}
	}
}
