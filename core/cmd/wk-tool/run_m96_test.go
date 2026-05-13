package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hosted-export usage error: missing required flags.
func TestRun_HostedExport_MissingFlags_UsageError(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := &fakeEnv{values: map[string]string{}}
	rc := run(context.Background(), []string{"hosted-export"}, stdout, stderr, env)
	if rc != 2 {
		t.Fatalf("rc=%d want 2; stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "missing required flag") {
		t.Errorf("stderr=%q want missing-flags diagnostic", stderr.String())
	}
}

// share usage error: missing token-env.
func TestRun_Share_MissingTokenEnv_UsageError(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := &fakeEnv{values: map[string]string{
		"WATCHKEEPER_DATA_DIR": "/tmp/x",
	}}
	rc := run(context.Background(), []string{
		"share",
		"--source", "private",
		"--tool", "weekly_digest",
		"--target-owner", "watchkeepers",
		"--target-repo", "watchkeeper-tools",
		"--reason", "graduating",
		"--proposer", "agent-coord",
	}, stdout, stderr, env)
	if rc != 2 {
		t.Fatalf("rc=%d want 2; stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "WATCHKEEPER_GITHUB_TOKEN") {
		t.Errorf("stderr=%q should name the env var", stderr.String())
	}
}

// share usage error: invalid --target is reported via the
// missing-flag accumulator (iter-1 M10 fix) and does NOT echo
// the bad value to stderr (iter-1 M5 fix).
func TestRun_Share_InvalidTarget_UsageError(t *testing.T) {
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := &fakeEnv{values: map[string]string{
		"WATCHKEEPER_DATA_DIR":     "/tmp/x",
		"WATCHKEEPER_GITHUB_TOKEN": "ghp_synthetic_token", //nolint:gosec // G101: test fixture, not a real credential.
	}}
	rc := run(context.Background(), []string{
		"share",
		"--source", "private",
		"--tool", "tool",
		"--target", "rm -rf /",
		"--target-owner", "o",
		"--target-repo", "r",
		"--reason", "x",
		"--proposer", "p",
	}, stdout, stderr, env)
	if rc != 2 {
		t.Fatalf("rc=%d want 2; stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "target") {
		t.Errorf("stderr=%q want target diagnostic", stderr.String())
	}
	// Iter-1 M5 fix: do not echo the bad value verbatim.
	if strings.Contains(stderr.String(), "rm -rf /") {
		t.Errorf("stderr echoes bad --target value: %q", stderr.String())
	}
	// Iter-1 M10 fix: accumulation works — the message format
	// names "target (must be 'platform' or 'private')".
	if !strings.Contains(stderr.String(), "must be 'platform' or 'private'") {
		t.Errorf("stderr=%q does not name accepted values", stderr.String())
	}
}

// hosted-export end-to-end on a real-FS temp dir.
func TestRun_HostedExport_EndToEnd_OnRealFS(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	root := filepath.Join(dataDir, "tools", "hosted-private", "weekly_digest")
	if err := os.MkdirAll(filepath.Join(root, "src"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	manifest := []byte(`{"name":"weekly_digest","version":"1.0.0","capabilities":["read:logs"],"schema":{"type":"object"},"dry_run_mode":"none"}`)
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "src", "index.ts"), []byte("export default 1;\n"), 0o600); err != nil {
		t.Fatalf("write src: %v", err)
	}
	dest := filepath.Join(tmp, "export-output")

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := &fakeEnv{values: map[string]string{"WATCHKEEPER_DATA_DIR": dataDir}}
	rc := run(context.Background(), []string{
		"hosted-export",
		"--source", "hosted-private",
		"--tool", "weekly_digest",
		"--destination", dest,
		"--reason", "graduating",
		"--operator", "alice@example.com",
	}, stdout, stderr, env)
	if rc != 0 {
		t.Fatalf("rc=%d want 0; stderr=%s; stdout=%s", rc, stderr.String(), stdout.String())
	}
	// Destination tree mirrors the source.
	if _, err := os.Stat(filepath.Join(dest, "manifest.json")); err != nil {
		t.Errorf("dest manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "src", "index.ts")); err != nil {
		t.Errorf("dest src missing: %v", err)
	}
	// JSONL summary line ends with `correlation_id=<value>\n`.
	if !strings.Contains(stdout.String(), "hosted-export ok") {
		t.Errorf("stdout missing ok summary: %s", stdout.String())
	}
}

// hosted-export refuses a malformed operator id without echoing it
// onto stderr (M9.5 iter-1 critic M2 hygiene).
func TestRun_HostedExport_MalformedOperator_Refused(t *testing.T) {
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	root := filepath.Join(dataDir, "tools", "hosted-private", "weekly_digest")
	_ = os.MkdirAll(root, 0o755)
	_ = os.WriteFile(filepath.Join(root, "manifest.json"), []byte(`{"name":"weekly_digest","version":"1.0.0","capabilities":["read:logs"],"schema":{"type":"object"},"dry_run_mode":"none"}`), 0o600)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := &fakeEnv{values: map[string]string{"WATCHKEEPER_DATA_DIR": dataDir}}
	rc := run(context.Background(), []string{
		"hosted-export",
		"--source", "hosted-private",
		"--tool", "weekly_digest",
		"--destination", filepath.Join(tmp, "dst"),
		"--reason", "x",
		"--operator", "alice; rm -rf /",
	}, stdout, stderr, env)
	if rc != 2 {
		t.Fatalf("rc=%d want 2; stderr=%s", rc, stderr.String())
	}
	if strings.Contains(stderr.String(), "rm -rf") {
		t.Errorf("stderr echoes malformed operator id: %q", stderr.String())
	}
}
