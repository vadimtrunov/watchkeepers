package capdict

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAuditGrep_NoKeeperslogImport_NoAppendCall is the negative-
// shape audit-grep companion shared by every M9 emitter and pure
// package. The capdict package is a PURE DECODER — it has no
// audit-write surface; a future maintainer who tries to plumb
// `keeperslog.Writer.Append` into the loader is wrong (the audit
// row for "dictionary loaded" lives in the production wiring layer,
// NOT here).
//
// Mirrors `core/pkg/localpatch/audit_grep_test.go` /
// `core/pkg/toolshare/audit_grep_test.go` shape.
func TestAuditGrep_NoKeeperslogImport_NoAppendCall(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) failed")
	}
	pkgDir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("read capdict dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(pkgDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := stripGoComments(string(raw))
		if strings.Contains(src, "keeperslog") {
			t.Errorf("%s: production code must not import keeperslog (pure decoder discipline)", name)
		}
		if strings.Contains(src, ".Append(") {
			t.Errorf("%s: production code must not call .Append( (no audit surface in capdict)", name)
		}
	}
}

// stripGoComments removes `//`-line and `/* … */`-block comments
// from `src` so the audit-grep assertion is not tripped by
// references to forbidden symbols inside documentation. Same
// block-comment-aware stripper landed in M9.5 / M9.6 / M9.7 audit-
// grep tests (each copy is independent — a future fix in one would
// not propagate; M9.7 iter-1 m11 pinned this stripper). Distinct
// copy here rather than an imported helper to keep the audit-grep
// test self-contained inside its package boundary.
func stripGoComments(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	i := 0
	for i < len(src) {
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '/' {
			j := i + 2
			for j < len(src) && src[j] != '\n' {
				j++
			}
			i = j
			continue
		}
		if i+1 < len(src) && src[i] == '/' && src[i+1] == '*' {
			j := i + 2
			for j+1 < len(src) && (src[j] != '*' || src[j+1] != '/') {
				j++
			}
			i = j + 2
			if i > len(src) {
				i = len(src)
			}
			continue
		}
		b.WriteByte(src[i])
		i++
	}
	return b.String()
}

// TestStripGoComments_BlockAndLineComments pins the comment
// stripper against representative inputs (block / line / inline /
// multiline / no-comment). Mirrors M9.7 iter-1 m11 fix.
func TestStripGoComments_BlockAndLineComments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no comments", "package x\nvar y = 1\n", "package x\nvar y = 1\n"},
		{"line comment", "package x // doc\nvar y = 1\n", "package x \nvar y = 1\n"},
		{"trailing line comment no newline", "package x // doc", "package x "},
		{"block comment inline", "package x /* doc */ var y = 1\n", "package x  var y = 1\n"},
		{"block comment multiline", "package x\n/*\nkeeperslog\n*/\nvar y = 1\n", "package x\n\nvar y = 1\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripGoComments(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
