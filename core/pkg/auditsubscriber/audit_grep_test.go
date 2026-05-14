package auditsubscriber

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestAuditGrep_KeeperslogImportPresent_AppendCallPresent is the
// inverse-shape companion to the M9.5 / M9.6 audit-grep tests in
// `localpatch`, `hostedexport`, and `toolshare`. Those packages
// REFUSE to import `keeperslog` and REFUSE to call `.Append(` — the
// audit-write boundary lives one layer down. THIS package is that
// one layer down. The positive assertion below pins the bridge IS
// wired:
//
//   - at least one production file references `keeperslog`;
//   - at least one production file calls `.Append(`.
//
// If a future refactor accidentally rips out the writer call (or
// moves it to a different package), this test surfaces the regression
// before the audit pipeline goes silent.
//
// Block-comment / trailing-line-comment stripper mirrors
// `localpatch.stripGoComments` (M9.4.c iter-1 critic n1 fix).
func TestAuditGrep_KeeperslogImportPresent_AppendCallPresent(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	var importFound, appendFound bool
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			// Test files may legitimately reference both tokens
			// (fakes, table-driven assertions). Production-only.
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %q: %v", f, err)
		}
		stripped := stripGoComments(string(raw))
		if strings.Contains(stripped, "keeperslog") {
			importFound = true
		}
		if strings.Contains(stripped, ".Append(") {
			appendFound = true
		}
	}
	if !importFound {
		t.Error("expected at least one production file to reference `keeperslog`; M9.7 is the bridge — the import is the contract")
	}
	if !appendFound {
		t.Error("expected at least one production file to call `.Append(`; M9.7 is the audit-write side — the call is the contract")
	}
}

// stripGoComments removes block (`/* ... */`) and trailing line
// (`// ...$`) comments. Greedy block-comment regex first, then
// trailing-line cleanup. Mirrors `localpatch.stripGoComments`.
func stripGoComments(src string) string {
	src = blockCommentRE.ReplaceAllString(src, " ")
	src = lineCommentRE.ReplaceAllString(src, "")
	return src
}

var (
	blockCommentRE = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineCommentRE  = regexp.MustCompile(`(?m)//[^\n]*$`)
)

// TestStripGoComments_BlockAndLineComments pins the comment-stripper
// behaviour mirroring the M9.4.c iter-1 critic n1 fix carried into
// localpatch / hostedexport / toolshare audit_grep test files
// (critic iter-1 m11). Without a unit test the M9.7 copy of
// `stripGoComments` could drift from the sibling packages on a
// future regex fix and silently false-positive an audit_grep run.
func TestStripGoComments_BlockAndLineComments(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "block-comment-on-own-line",
			in:   "import (\n\t/* keeperslog stub */\n\t\"x\"\n)\n",
			want: "import (\n\t \n\t\"x\"\n)\n",
		},
		{
			name: "trailing-line-comment",
			in:   "var x = 1 // keeperslog import\nvar y = 2\n",
			want: "var x = 1 \nvar y = 2\n",
		},
		{
			name: "inline-block-comment",
			in:   "x := f(/* keeperslog */ 42)\n",
			want: "x := f(  42)\n",
		},
		{
			name: "multiline-block-comment",
			in:   "/* line a\nline b\nkeeperslog\n*/var x = 1",
			want: " var x = 1",
		},
		{
			name: "no-comment-passthrough",
			in:   "import \"github.com/.../keeperslog\"\n",
			want: "import \"github.com/.../keeperslog\"\n",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := stripGoComments(tc.in)
			if got != tc.want {
				t.Errorf("stripGoComments(%q):\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}
