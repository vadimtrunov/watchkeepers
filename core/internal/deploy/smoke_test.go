package deploy_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// stripBashComments returns the script source with every shell-style
// comment (a `#` to end-of-line outside single-quoted strings) elided
// to whitespace. Iter-1 codex+critic flagged that the previous
// `strings.Contains(src, ...)` matchers could be satisfied by
// commented-out text — including the prose header block in
// scripts/smoke.sh that names every M7/M8/M9 package by path. We
// preserve line numbers (replace comment runs with spaces, not
// deletions) so future expansion can still cite file:line.
//
// The stripper is intentionally tiny: bash quoting rules are far
// richer than what we need here. The smoke script does not use `#`
// inside single-quoted strings, so the simple line-by-line strip is
// sufficient. A regression that introduces `#` inside a quoted
// payload would fall back to the cheap (and over-strict) behaviour;
// that's a contract test bug worth catching loudly.
func stripBashComments(src string) string {
	lines := strings.Split(src, "\n")
	out := make([]string, len(lines))
	for i, line := range lines {
		if idx := strings.Index(line, "#"); idx >= 0 {
			// Replace the comment tail with spaces so the column
			// offsets of any preceding code stay stable.
			out[i] = line[:idx] + strings.Repeat(" ", len(line)-idx)
		} else {
			out[i] = line
		}
	}
	return strings.Join(out, "\n")
}

// TestSmokeScriptIncludesM7M8M9Packages pins the M10.4 contract between
// the operator-facing `make smoke` target and the Go packages that
// own the M7 / M8 / M9 success-path coverage. A rename of any pinned
// package without a matching `scripts/smoke.sh` edit fails this test
// loudly in the same PR.
//
// The pinned packages are duplicated from the script (rather than
// extracted to a shared constant in a separate file) on purpose: the
// dupe is the forcing function. A future operator who renames
// `core/pkg/spawn` to `core/pkg/spawnsaga` MUST update both this list
// and the script's bash arrays in the same change, with the failure
// mode self-describing.
//
// Iter-1 codex+critic: substring-only matching can be satisfied by
// commented-out lines in the script's prose header (which lists every
// package by path). We strip `#` comments before matching so the
// contract only succeeds when the package glob appears in executable
// bash, not in documentation.
func TestSmokeScriptIncludesM7M8M9Packages(t *testing.T) {
	t.Parallel() // OK: read-only file inspection; no shared mutable state.

	path := filepath.Join(repoRoot(t), "scripts", "smoke.sh")
	raw, err := os.ReadFile(path) //nolint:gosec // test reads in-repo source-of-truth file
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	stripped := stripBashComments(string(raw))

	type pin struct {
		milestone string
		pkg       string
	}
	pins := []pin{
		// M7 spawn + retire saga + archive on retire.
		{"M7", "./core/pkg/spawn/..."},
		{"M7", "./core/pkg/lifecycle/..."},
		{"M7", "./core/pkg/notebook/..."},
		{"M7", "./core/pkg/archivestore/..."},
		// M8 coordinator + jira + cron-driven daily briefing.
		{"M8", "./core/pkg/coordinator/..."},
		{"M8", "./core/pkg/jira/..."},
		{"M8", "./core/pkg/cron/..."},
		// M9 tool authoring + approval + dry-run + share + capdict.
		{"M9", "./core/pkg/approval/..."},
		{"M9", "./core/pkg/toolregistry/..."},
		{"M9", "./core/pkg/toolshare/..."},
		{"M9", "./core/pkg/hostedexport/..."},
		{"M9", "./core/pkg/capdict/..."},
		{"M9", "./core/pkg/localpatch/..."},
		// Operator CLI seam (M10.2). Iter-1: tagged M10.2 not bare M10
		// to match the lesson's milestone-precise terminology.
		{"M10.2", "./core/cmd/wk/..."},
	}
	for _, p := range pins {
		if !strings.Contains(stripped, p.pkg) {
			t.Errorf("smoke.sh missing %s coverage for %q in executable bash (comments stripped). M10.4 contract: every M7/M8/M9 success-path package must appear in the smoke set.", p.milestone, p.pkg)
		}
	}
}

// TestSmokeScriptRunsBuildAndRaceTests pins the smoke contract: the
// script MUST run `go build ./...` (compile gate) AND `go test -race`
// against the curated set. Dropping either step would silently
// degrade the smoke surface to a single-phase gate.
//
// Iter-1: a `^[[:space:]]*` prefix in the regex ensures the match
// lands at line start (after optional indentation), so the prose
// header that mentions `go build ./...` in passing does not satisfy
// the gate. Combined with `stripBashComments`, the match only
// succeeds when the literal appears in executable bash.
func TestSmokeScriptRunsBuildAndRaceTests(t *testing.T) {
	t.Parallel() // OK: read-only file inspection.

	path := filepath.Join(repoRoot(t), "scripts", "smoke.sh")
	raw, err := os.ReadFile(path) //nolint:gosec // test reads in-repo source-of-truth file
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	stripped := stripBashComments(string(raw))

	// Both gates appear in executable bash, not just the prose
	// header. Each regex is anchored at line-start (after optional
	// indentation) to defend against future moves of the literals
	// into a string assignment buried elsewhere.
	cases := []struct {
		label   string
		pattern *regexp.Regexp
	}{
		{`go build ./...`, regexp.MustCompile(`(?m)^[\t ]*"?\$\{?go_bin\}?"? +build +\./\.\.\.`)},
		{`go test -race`, regexp.MustCompile(`(?m)^[\t ]*"?\$\{?go_bin\}?"? +test +-race`)},
	}
	for _, tc := range cases {
		if !tc.pattern.MatchString(stripped) {
			t.Errorf("smoke.sh missing %q in executable bash (anchored line-start regex %q). M10.4 contract: build gate + race tests both required.", tc.label, tc.pattern)
		}
	}
}

// TestSmokeScriptExecutable pins the operator-facing contract: the
// shell script must be marked executable so `make smoke` can invoke
// it directly without `bash scripts/smoke.sh`. A future `git add`
// that drops the +x bit (e.g. via a Windows checkout misconfig)
// would surface here.
//
// Iter-1 critic #8: Windows checkouts do not preserve POSIX exec
// bits, and `core.fileMode = false` (a common Windows-side git
// default) keeps the working tree at 0644 even when the index
// stores 100755. Skip on Windows to avoid false negatives on
// developer machines; the smoke gate's authoritative run is in
// the linux ubuntu-24.04 CI job where the bit is honoured.
func TestSmokeScriptExecutable(t *testing.T) {
	t.Parallel() // OK: read-only stat.

	if runtime.GOOS == "windows" {
		t.Skipf("smoke.sh exec bit not portable to windows; CI runs on ubuntu-24.04 where the bit is honoured")
	}

	path := filepath.Join(repoRoot(t), "scripts", "smoke.sh")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	mode := info.Mode().Perm()
	if mode&0o111 == 0 {
		t.Errorf("smoke.sh mode = %o, want at least one execute bit set (M10.4: make smoke invokes the script directly)", mode)
	}
}

// TestSmokeMakeTargetInvokesScript pins the Makefile's smoke target to
// delegate to `scripts/smoke.sh`. The previous placeholder echoed a
// "wired in later milestones" string and exited 0; replacing that
// non-gate with a real gate is the M10.4 contract. A future
// regression where the target reverts to `@exit 0` would silently
// pass smoke runs.
//
// Iter-1 critic #10: the previous version asserted the literal
// `@scripts/smoke.sh` substring. A benign refactor (drop the `@` to
// echo the recipe, switch to `bash ./scripts/smoke.sh`) would break
// the pin without changing behaviour. We now walk Makefile rules
// from the `^smoke:` line until the next blank line or non-tab-
// prefixed line, and assert SOME recipe line invokes
// `scripts/smoke.sh`.
func TestSmokeMakeTargetInvokesScript(t *testing.T) {
	t.Parallel() // OK: read-only file inspection.

	path := filepath.Join(repoRoot(t), "Makefile")
	raw, err := os.ReadFile(path) //nolint:gosec // test reads in-repo source-of-truth file
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	src := string(raw)

	if !strings.Contains(src, "smoke: ##") {
		t.Fatalf("Makefile: smoke target not found")
	}
	if strings.Contains(src, `"smoke: placeholder`) {
		t.Errorf("Makefile: smoke target still uses the pre-M10.4 placeholder; replace with scripts/smoke.sh invocation")
	}

	// Walk the recipe block under `smoke:` until the next blank line
	// or non-tab-prefixed line. POSIX Make requires recipe lines to
	// start with TAB; this lets us identify the boundary cleanly.
	lines := strings.Split(src, "\n")
	var inRecipe bool
	var recipe []string
	for _, ln := range lines {
		if !inRecipe {
			if strings.HasPrefix(ln, "smoke:") {
				inRecipe = true
				continue
			}
			continue
		}
		// In the recipe block. Boundary: blank line OR non-tab-
		// prefixed line.
		if ln == "" || !strings.HasPrefix(ln, "\t") {
			break
		}
		recipe = append(recipe, ln)
	}
	if len(recipe) == 0 {
		t.Fatalf("Makefile: smoke target has an empty recipe block")
	}
	joined := strings.Join(recipe, "\n")
	if !strings.Contains(joined, "scripts/smoke.sh") {
		t.Errorf("Makefile: smoke recipe must invoke scripts/smoke.sh somewhere in its block (M10.4 contract). Recipe:\n%s", joined)
	}
}
