package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"
	"time"
)

// slackTokenPrefixes is the closed set of Slack token-family prefixes
// the binary MUST never carry. Source:
// https://api.slack.com/authentication/token-types — the four families
// listed are the only ones a Slack-integrated process ever holds.
//
//   - xoxb-* — bot user tokens
//   - xoxp-* — user tokens
//   - xoxe-* — app configuration / refresh tokens (the family the
//     spawn-dev-bot configuration token belongs to)
//   - xapp-* — app-level tokens (Socket Mode `apps.connections.open`)
//
// Source-level mentions of these prefixes ride OUTSIDE the binary
// (godoc strings about the prefix, never a literal value), so the
// regex is anchored to the prefix + at least eight token-character
// bytes — a string the documentation text would never accidentally
// produce.
var slackTokenPattern = regexp.MustCompile(`xox[bpe]-[A-Za-z0-9._-]{8,}|xapp-[A-Za-z0-9._-]{8,}`)

// TestSpawnDevBot_BinaryHasNoTokenLeaks verifies ROADMAP §M4 bullet
// 279 — "Parent-app credentials never leave the secrets interface
// (grep the built binary for raw tokens — none)". Builds the
// spawn-dev-bot binary into a tempdir, runs the [slackTokenPattern]
// regex against the resulting bytes, and fails if any Slack token
// prefix carries through.
//
// The test is the toggleable verification gate: as long as it passes
// on every CI run, bullet 279 stays satisfied for the parent-app
// bootstrap path. A future regression that embedded a token literal
// — e.g. via an accidental `slack.StaticToken("xoxe-real-token")` in
// production code — would surface here BEFORE the binary ever ran on
// an operator's machine.
//
// Skipped on Windows because exec.Command + the Go toolchain's
// build-temp handling diverges enough that the assertion's
// portability isn't worth the complexity for a path Phase 1 doesn't
// target. Linux + macOS CI runners are the production gate.
func TestSpawnDevBot_BinaryHasNoTokenLeaks(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("binary-grep test runs on linux/darwin only")
	}

	if _, err := exec.LookPath("go"); err != nil {
		t.Skipf("go toolchain not on PATH: %v", err)
	}

	tmp := t.TempDir()
	binPath := filepath.Join(tmp, "spawn-dev-bot.bin")

	// Bound the build so a wedged toolchain never hangs the test run;
	// 2m is generous for `go build` on this size of package even on a
	// cold cache. Mirrors the keep-build trim-path discipline so module
	// paths don't carry incidentally long alphanumeric strings into the
	// binary that would foil the regex.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-trimpath", "-o", binPath, "./core/cmd/spawn-dev-bot")
	cmd.Dir = repoRoot(t)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	data, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatalf("read built binary: %v", err)
	}

	matches := slackTokenPattern.FindAll(data, -1)
	if len(matches) == 0 {
		return
	}

	// Surface every match (capped to a generous limit so the failure
	// message is actionable but not unbounded).
	const maxReport = 10
	t.Errorf("built binary at %s contains %d Slack-token-shaped strings; first %d:", binPath, len(matches), min(len(matches), maxReport))
	for i, m := range matches {
		if i >= maxReport {
			break
		}
		// Print as %q so non-printable bytes surface readably.
		t.Errorf("  [%d] %q", i, redactMatch(m))
	}
}

// repoRoot walks up from the test's source file until it finds a
// go.mod, returning the directory that contains it. The test runs
// with cwd == package dir, so an explicit walk is the portable way to
// resolve "the module root" without hardcoding a relative depth.
func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("go.mod not found walking up from %s", wd)
		}
		dir = parent
	}
}

// redactMatch trims a long match to its first 32 bytes plus an ellipsis
// so test output stays bounded. Only the prefix + a small tail is
// needed to identify a leak; printing the entire matched run risks
// scrolling the test output past the actionable diagnostic.
func redactMatch(b []byte) []byte {
	const head = 32
	if len(b) <= head {
		return bytes.TrimSpace(b)
	}
	out := make([]byte, 0, head+3)
	out = append(out, b[:head]...)
	out = append(out, '.', '.', '.')
	return out
}
