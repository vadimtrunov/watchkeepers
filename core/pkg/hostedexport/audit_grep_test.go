package hostedexport_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestM96A_AuditGrep_NoKeeperslogImport asserts the production code
// in this package never imports `keeperslog` directly. Production
// code emits audit events on the [eventbus.Bus] only; the audit
// subscriber (M9.7) translates them to keeperslog rows downstream.
// Mirror M9.5's `TestM95_AuditGrep_NoKeeperslogImport`.
func TestM96A_AuditGrep_NoKeeperslogImport(t *testing.T) {
	for _, name := range productionGoFiles(t, ".") {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %q: %v", name, err)
		}
		stripped := stripCommentsForAudit(string(body))
		if strings.Contains(stripped, "keeperslog") {
			t.Errorf("%q imports/uses keeperslog in non-comment code", name)
		}
	}
}

// TestM96A_AuditGrep_NoAppendCall asserts the production code never
// calls a `.Append(` method (the keeperslog Append entry point).
// Mirror M9.5's `TestM95_AuditGrep_NoAppendCall`.
func TestM96A_AuditGrep_NoAppendCall(t *testing.T) {
	for _, name := range productionGoFiles(t, ".") {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %q: %v", name, err)
		}
		stripped := stripCommentsForAudit(string(body))
		if strings.Contains(stripped, ".Append(") {
			t.Errorf("%q calls .Append( in non-comment code", name)
		}
	}
}

// productionGoFiles returns the *.go files under `dir` excluding
// `_test.go` files. Mirror M9.5's helper of the same name.
func productionGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %q: %v", dir, err)
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	return out
}

// stripCommentsForAudit removes both line-comments (`// ...`) and
// block-comments (`/* ... */`) from `src` so the audit grep cannot
// be fooled by a comment naming `keeperslog` or `.Append(`. Mirror
// M9.5's helper of the same name with the same block-comment +
// trailing-line-comment-aware behaviour.
// the M9.5 audit-grep helper has the identical shape.
//
//nolint:gocyclo // Token-state machine; complexity is structural and
func stripCommentsForAudit(src string) string {
	var b strings.Builder
	b.Grow(len(src))
	i := 0
	inString := false
	inRune := false
	for i < len(src) {
		c := src[i]
		if !inString && !inRune {
			if c == '/' && i+1 < len(src) {
				next := src[i+1]
				if next == '/' {
					end := strings.IndexByte(src[i:], '\n')
					if end < 0 {
						return b.String()
					}
					i += end
					continue
				}
				if next == '*' {
					end := strings.Index(src[i+2:], "*/")
					if end < 0 {
						return b.String()
					}
					i += end + 4
					continue
				}
			}
			if c == '"' {
				inString = true
				b.WriteByte(c)
				i++
				continue
			}
			if c == '\'' {
				inRune = true
				b.WriteByte(c)
				i++
				continue
			}
		} else if inString {
			if c == '\\' && i+1 < len(src) {
				b.WriteByte(c)
				b.WriteByte(src[i+1])
				i += 2
				continue
			}
			if c == '"' {
				inString = false
			}
		} else if inRune {
			if c == '\\' && i+1 < len(src) {
				b.WriteByte(c)
				b.WriteByte(src[i+1])
				i += 2
				continue
			}
			if c == '\'' {
				inRune = false
			}
		}
		b.WriteByte(c)
		i++
	}
	return b.String()
}
