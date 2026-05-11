package toolregistry

import (
	"bufio"
	"os"
	"strings"
)

// readFile is a test-only wrapper around [os.ReadFile] that the
// source-grep AC tests consult. The wrapper exists so the test
// import set stays minimal (no direct `os` import in the manifest /
// scheduler tests).
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // test-only helper reading sibling .go files in the same package
}

// containsOutsideComments reports whether `body` contains `needle`
// on any non-comment line. Used by source-grep ACs to assert that
// banned tokens (e.g. `keeperslog.`) do not appear in production
// code while still allowing them to be discussed in doc-block
// comments.
//
// Implementation: walk lines, strip line-comments at `//`, and
// drop fully-commented lines. Block comments (`/* */`) are out of
// scope here because the production sources do not use them.
func containsOutsideComments(body, needle string) bool {
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		if strings.Contains(line, needle) {
			return true
		}
	}
	return false
}
