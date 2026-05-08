package llm

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoAnthropicAPIKeyLiteralOutsideAllowlist enforces the M5.7.a
// grep-invariant: the credential literal naming the Claude Code API key
// MUST appear ONLY inside [secretsAdapterPath]. Any other production
// .go or .ts file carrying the literal fails the build.
//
// # What this protects
//
// The harness boot path consumes the credential through the
// EnvSecretSource adapter (harness/src/secrets/env.ts). Centralising
// the literal at one site lets a future M9 / vault-backed seam
// substitute the source without touching dozens of call sites, and it
// keeps the credential name out of stack traces, log messages, and
// stray doc-comments where it would broadcast the access pattern. This
// test is the contract enforcement; without it the centralisation is
// merely a convention that drifts.
//
// # Allowlist policy
//
// The single entry is the seam itself. When a future seam expands
// (Vault, AWS Secrets Manager, harnessrpc-bridged Go-core source) the
// allowlist updates alongside in the SAME PR that adds the seam, so
// the audit trail stays linear.
//
// # Test-file self-immunity
//
// The literal is constructed at runtime via concatenation
// ("ANTHROPIC_" + "API_KEY") so a naive substring grep over THIS file
// does not trip the invariant. Without that trick the test would fail
// on its own source on the first run.
//
// # Scope
//
// Walks the repo root looking at production .go files
// (excluding _test.go) and production .ts files under harness/src and
// tools-builtin/src (excluding *.test.ts and src/secrets/env.ts).
// Markdown, JSON, YAML, and other non-source files are out of scope —
// the literal in docs/ROADMAP-phase1.md and TASK-*.md is intentional
// and informational, not a credential read.
func TestNoAnthropicAPIKeyLiteralOutsideAllowlist(t *testing.T) {
	t.Parallel()

	// Constructed at runtime so this test file's source does not
	// itself contain the literal as a static substring.
	literal := "ANTHROPIC_" + "API_KEY"

	root := repoRoot(t)

	// Allowlisted production-code paths (relative to repoRoot, slash-
	// separated). Tests are EXEMPT separately by suffix below.
	allowlist := map[string]struct{}{
		"harness/src/secrets/env.ts": {},
	}

	// Production-code roots to scan. Out of scope: dist/, node_modules/,
	// coverage/, vendor/, .git/, .omc/, .claude/.
	scanRoots := []string{
		"core",
		"harness/src",
		"tools-builtin/src",
		"cli",
	}

	var violations []string

	for _, sub := range scanRoots {
		base := filepath.Join(root, sub)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			// Optional roots (e.g. cli/, tools-builtin/src may not
			// exist on all branches): skip silently.
			continue
		}
		walked, err := scanForLiteral(base, root, literal, allowlist)
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
		violations = append(violations, walked...)
	}

	if len(violations) > 0 {
		t.Fatalf("M5.7.a grep-invariant: credential literal must appear only in the allowlisted adapter; "+
			"found in %d file(s): %v\n"+
			"If you legitimately need a new call site, extend the allowlist in this test "+
			"AND document the reason in the same PR.", len(violations), violations)
	}
}

// scanForLiteral walks `base` and returns a list of repo-relative
// paths whose content contains `literal`, skipping non-production
// source files and the allowlist. Extracted from the test body to
// keep the test function under the gocyclo bound.
func scanForLiteral(
	base, root, literal string,
	allowlist map[string]struct{},
) ([]string, error) {
	var hits []string
	err := filepath.Walk(base, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if shouldSkipDir(info.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if !isProductionSourceFile(rel) {
			return nil
		}
		if _, ok := allowlist[rel]; ok {
			return nil
		}
		data, err := os.ReadFile(path) //nolint:gosec // walking known repo paths
		if err != nil {
			return err
		}
		if strings.Contains(string(data), literal) {
			hits = append(hits, rel)
		}
		return nil
	})
	return hits, err
}

// shouldSkipDir returns true for directories the invariant scan
// should not descend into (build artefacts, vendored deps, OMC state,
// VCS metadata).
func shouldSkipDir(name string) bool {
	switch name {
	case "node_modules", "dist", "coverage", "vendor",
		".git", ".omc", ".claude":
		return true
	default:
		return false
	}
}

// isProductionSourceFile returns true for .go / .ts files we want to
// audit, false for tests, type definitions, and other source kinds.
func isProductionSourceFile(rel string) bool {
	switch {
	case strings.HasSuffix(rel, "_test.go"):
		return false
	case strings.HasSuffix(rel, ".test.ts"):
		return false
	case strings.HasSuffix(rel, ".d.ts"):
		return false
	case strings.HasSuffix(rel, ".go"):
		return true
	case strings.HasSuffix(rel, ".ts"):
		// Limit TS scope to harness/src and tools-builtin/src so test
		// fixtures and ad-hoc scripts elsewhere do not trip the
		// invariant. The scan-root filter in the caller already
		// narrows this; the suffix check is the second gate.
		return strings.HasPrefix(rel, "harness/src/") ||
			strings.HasPrefix(rel, "tools-builtin/src/")
	default:
		return false
	}
}

// repoRoot walks up from this test file's directory until it finds a
// go.mod, then returns that directory. Test CWD is unreliable across
// `go test` invocations (per-package vs repo-wide).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed — cannot locate test file")
	}
	dir := filepath.Dir(thisFile)
	for i := 0; i < 16; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("repoRoot: no go.mod found above %s", filepath.Dir(thisFile))
	return ""
}

// TestAnthropicKeyInvariantRegressionMeta is the AC test-plan
// "invariant regression" pin: it constructs an in-memory file tree
// equivalent to a hypothetical violation and confirms the predicate
// `isProductionSourceFile` + `strings.Contains` combination would
// flag it. The real grep-invariant test cannot be flipped on the disk
// without polluting the repo, so this meta-test pins the predicate's
// own correctness — same coverage, no temp-file dance.
func TestAnthropicKeyInvariantRegressionMeta(t *testing.T) {
	t.Parallel()
	literal := "ANTHROPIC_" + "API_KEY"

	cases := []struct {
		path    string
		content string
		flagged bool
		reason  string
	}{
		{
			path:    "harness/src/llm/foo.ts",
			content: "const k = process.env." + literal + ";",
			flagged: true,
			reason:  "production .ts inside harness/src carrying the literal must flag",
		},
		{
			path:    "harness/src/secrets/env.ts",
			content: literal,
			flagged: false,
			reason:  "the allowlisted adapter file is exempt",
		},
		{
			path:    "harness/test/foo.test.ts",
			content: literal,
			flagged: false,
			reason:  "tests are excluded from the allowlist scan",
		},
		{
			path:    "harness/src/foo.d.ts",
			content: literal,
			flagged: false,
			reason:  "ambient type files are out of scope",
		},
		{
			path:    "core/pkg/llm/foo.go",
			content: literal,
			flagged: true,
			reason:  "production Go source must flag",
		},
		{
			path:    "core/pkg/llm/foo_test.go",
			content: literal,
			flagged: false,
			reason:  "Go test files are excluded",
		},
		{
			path:    "scripts/foo.ts",
			content: literal,
			flagged: false,
			reason:  "TS outside harness/src + tools-builtin/src is out of scope",
		},
	}

	allowlist := map[string]struct{}{
		"harness/src/secrets/env.ts": {},
	}

	for _, tc := range cases {
		isProd := isProductionSourceFile(tc.path)
		_, isAllow := allowlist[tc.path]
		flagged := isProd && !isAllow && strings.Contains(tc.content, literal)
		if flagged != tc.flagged {
			t.Errorf("predicate %q: got flagged=%v want %v (%s)", tc.path, flagged, tc.flagged, tc.reason)
		}
	}
}
