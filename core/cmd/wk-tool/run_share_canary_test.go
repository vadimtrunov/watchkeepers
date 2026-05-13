package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestRun_Share_TokenCanaryNotEchoedToStdoutOrStderr defends the
// CLI's redaction discipline at runtime. The `wk-tool share`
// subcommand reads the github PAT from an env var; a future
// regression that echoes the token to stdout (e.g. a debug
// printf), to stderr (e.g. an `err.Error()` chain wrapping a
// transport-level error that captures the URL with auth header
// substituted in), or via a structured log call, would round-trip
// the synthetic canary value into the buffer. Mirror the M9.5
// iter-1 M4 lesson (stdout JSONL canary) extended to both stdout
// AND stderr surfaces.
//
// The flow refuses on missing required flags BEFORE any github
// client construction, so the canary token never reaches the
// transport — but the test still validates the env-read path
// (the token is loaded BEFORE the share orchestrator runs).
func TestRun_Share_TokenCanaryNotEchoedToStdoutOrStderr(t *testing.T) {
	const canaryToken = "CANARY-GH-TOKEN-DO-NOT-LEAK-c4n4ry-t0k3n-abc" //nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := &fakeEnv{values: map[string]string{
		"WATCHKEEPER_DATA_DIR":     "/tmp/x",
		"WATCHKEEPER_GITHUB_TOKEN": canaryToken,
	}}
	// Pass minimal flags so the CLI reaches the env-read +
	// validate path. The share itself fails at the FS layer (no
	// such dir on the real OS) but the token has by then been
	// read from env and could conceivably appear in an error
	// chain.
	rc := run(context.Background(), []string{
		"share",
		"--source", "private",
		"--tool", "weekly_digest",
		"--target", "platform",
		"--target-owner", "watchkeepers",
		"--target-repo", "watchkeeper-tools",
		"--reason", "canary test",
		"--proposer", "agent-canary",
	}, stdout, stderr, env)
	// rc is non-zero (1 for runtime error since there's no tool
	// on disk OR 2 for usage); either way we assert no canary
	// leak regardless of which branch fired.
	_ = rc
	for _, surf := range []struct {
		name string
		buf  *bytes.Buffer
	}{
		{"stdout", stdout},
		{"stderr", stderr},
	} {
		if strings.Contains(surf.buf.String(), canaryToken) {
			t.Errorf("%s leaks github token canary: %q", surf.name, surf.buf.String())
		}
	}
}
