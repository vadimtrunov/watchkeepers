package localpatch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAuditGrep_NoKeeperslogImports_NoAppendCalls asserts the
// localpatch package never imports `keeperslog` and never calls
// `.Append(` (the audit-log write API). The audit log entry for
// `local_patch_applied` lives in the M9.7 audit subscriber that
// observes [TopicLocalPatchApplied] — the package's job stops at
// publishing the event.
//
// Mirror M9.4.c TestExecute_NoKeeperslogImports +
// TestExecute_NoAuditAppendCalls + iter-1 critic n1 fix (block-
// comment / trailing-comment-aware substring stripper).
func TestAuditGrep_NoKeeperslogImports_NoAppendCalls(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		// Skip test files — they may legitimately reference the
		// banned tokens (e.g. asserting "production code does NOT
		// call Append" can use the token in a comment).
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %q: %v", f, err)
		}
		stripped := stripGoComments(string(raw))
		if strings.Contains(stripped, "keeperslog") {
			t.Errorf("%s: production code references `keeperslog`", f)
		}
		if strings.Contains(stripped, ".Append(") {
			t.Errorf("%s: production code calls `.Append(` (audit-log write)", f)
		}
	}
}

// stripGoComments removes block (`/* ... */`) and trailing line
// (`// ...$`) comments. Greedy block-comment regex first, then
// trailing-line cleanup. Iter-1 critic n1 lesson from M9.4.c: a
// line-prefix-only stripper false-negatives block comments AND
// false-positives `// keeperslog` trailing comments.
func stripGoComments(src string) string {
	src = blockCommentRE.ReplaceAllString(src, " ")
	src = lineCommentRE.ReplaceAllString(src, "")
	return src
}

var (
	blockCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineCommentRE  = regexp.MustCompile(`(?m)//[^\n]*$`)
)
