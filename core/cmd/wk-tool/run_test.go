package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEnv satisfies [envLookup] for tests so $WATCHKEEPER_DATA_DIR
// stays out of the process env.
type fakeEnv struct{ values map[string]string }

func (e *fakeEnv) LookupEnv(key string) (string, bool) {
	v, ok := e.values[key]
	return v, ok
}

func TestRun_NoArgs_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), nil, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "missing subcommand") {
		t.Errorf("stderr missing diagnostic: %q", stderr.String())
	}
}

func TestRun_UnknownSubcommand_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"frobnicate"}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr missing diagnostic: %q", stderr.String())
	}
}

func TestRun_Help_PrintsUsage(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"help"}, &stdout, &stderr, &fakeEnv{})
	if code != 0 {
		t.Errorf("exit: got %d want 0", code)
	}
	if !strings.Contains(stdout.String(), "wk-tool") {
		t.Errorf("usage missing: %q", stdout.String())
	}
}

func TestRun_LocalInstall_MissingReason_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"local-install",
		"--folder", "/dev/null",
		"--operator", "alice",
		"--data-dir", "/tmp/wk-test",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "--reason") {
		t.Errorf("stderr missing --reason diagnostic: %q", stderr.String())
	}
}

func TestRun_LocalInstall_MissingOperator_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"local-install",
		"--folder", "/dev/null",
		"--reason", "test",
		"--data-dir", "/tmp/wk-test",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "--operator") {
		t.Errorf("stderr missing --operator diagnostic: %q", stderr.String())
	}
}

func TestRun_Rollback_MissingTo_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"rollback",
		"--name", "demo_tool",
		"--reason", "test",
		"--operator", "alice",
		"--data-dir", "/tmp/wk-test",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "--to") {
		t.Errorf("stderr missing --to diagnostic: %q", stderr.String())
	}
}

func TestRun_LocalInstall_DataDirFromEnv(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	folder := t.TempDir()
	mustWrite(t, filepath.Join(folder, "manifest.json"), validManifest("demo_tool", "1.0.0"))
	mustWrite(t, filepath.Join(folder, "src/index.ts"), []byte(`export const x = 1;`))
	var stdout, stderr bytes.Buffer
	env := &fakeEnv{values: map[string]string{dataDirEnvKey: dataDir}}
	code := run(context.Background(), []string{
		"local-install",
		"--folder", folder,
		"--source", "local",
		"--reason", "Hot-fix for incident #4711.",
		"--operator", "alice",
	}, &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("exit: got %d want 0; stderr=%s", code, stderr.String())
	}
	// Live tree present.
	live := filepath.Join(dataDir, "tools", "local", "demo_tool", "manifest.json")
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live manifest missing: %v", err)
	}
	// Stdout has at least one JSONL line + a summary line.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("stdout had %d lines; want >=2: %q", len(lines), stdout.String())
	}
	var jsonLine string
	for _, l := range lines {
		if strings.HasPrefix(l, "{") {
			jsonLine = l
			break
		}
	}
	if jsonLine == "" {
		t.Fatalf("no JSON line in stdout: %q", stdout.String())
	}
	var decoded struct {
		Topic string `json:"topic"`
		Event struct {
			SourceName string `json:"SourceName"`
			ToolName   string `json:"ToolName"`
			Operation  string `json:"Operation"`
			Reason     string `json:"Reason"`
		} `json:"event"`
	}
	if err := json.Unmarshal([]byte(jsonLine), &decoded); err != nil {
		t.Fatalf("unmarshal jsonl: %v", err)
	}
	if decoded.Topic != "localpatch.local_patch_applied" {
		t.Errorf("topic: got %q", decoded.Topic)
	}
	if decoded.Event.Operation != "install" {
		t.Errorf("operation: got %q", decoded.Event.Operation)
	}
	if decoded.Event.SourceName != "local" {
		t.Errorf("source: got %q", decoded.Event.SourceName)
	}
}

func TestRun_LocalInstall_ThenRollback_RestoresPriorVersion(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	// First install: v0.9.0.
	folder1 := t.TempDir()
	mustWrite(t, filepath.Join(folder1, "manifest.json"), validManifest("demo_tool", "0.9.0"))
	mustWrite(t, filepath.Join(folder1, "src/x.ts"), []byte(`v0_9`))
	env := &fakeEnv{values: map[string]string{dataDirEnvKey: dataDir}}
	code := run(context.Background(), []string{
		"local-install",
		"--folder", folder1,
		"--source", "local",
		"--reason", "initial install",
		"--operator", "alice",
	}, io.Discard, io.Discard, env)
	if code != 0 {
		t.Fatalf("install v0.9 failed: code=%d", code)
	}
	// Second install: v1.0.0 — should snapshot v0.9.0.
	folder2 := t.TempDir()
	mustWrite(t, filepath.Join(folder2, "manifest.json"), validManifest("demo_tool", "1.0.0"))
	mustWrite(t, filepath.Join(folder2, "src/x.ts"), []byte(`v1_0`))
	code = run(context.Background(), []string{
		"local-install",
		"--folder", folder2,
		"--source", "local",
		"--reason", "upgrade to 1.0",
		"--operator", "alice",
	}, io.Discard, io.Discard, env)
	if code != 0 {
		t.Fatalf("install v1.0 failed: code=%d", code)
	}
	// Rollback to v0.9.0.
	var rbOut bytes.Buffer
	code = run(context.Background(), []string{
		"rollback",
		"--name", "demo_tool",
		"--to", "0.9.0",
		"--source", "local",
		"--reason", "v1.0 broke something",
		"--operator", "alice",
	}, &rbOut, io.Discard, env)
	if code != 0 {
		t.Fatalf("rollback failed: code=%d output=%s", code, rbOut.String())
	}
	// Live tree restored to v0.9 source content.
	live := filepath.Join(dataDir, "tools", "local", "demo_tool", "src/x.ts")
	got, err := os.ReadFile(live)
	if err != nil {
		t.Fatalf("read live: %v", err)
	}
	if string(got) != "v0_9" {
		t.Errorf("live src not restored: got %q want %q", got, "v0_9")
	}
}

// TestRun_LocalInstall_JSONLStdoutOmitsFolderContentCanary asserts
// the JSONL stdout output never contains operator-supplied folder
// content. Iter-1 critic M1 fix: the JSONL publisher is the
// operator-visible audit surface — the package-level field allowlist
// alone is insufficient because a future envelope change could
// surface fields the package never declared.
func TestRun_LocalInstall_JSONLStdoutOmitsFolderContentCanary(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	folder := t.TempDir()
	const (
		canary1 = "wath-canary-marker-jsonl-001"
		canary2 = "wath-canary-bodyleak-002"
		canary3 = "wath-canary-pattern-zzz-003"
	)
	mustWrite(t, filepath.Join(folder, "manifest.json"), validManifest("canary_tool", "1.0.0"))
	mustWrite(t, filepath.Join(folder, "src/index.ts"), []byte("export const tok = `"+canary1+"`;"))
	mustWrite(t, filepath.Join(folder, "src/secrets.ts"), []byte(`/* `+canary2+` */`))
	mustWrite(t, filepath.Join(folder, "fixtures/blob"), []byte(canary3))

	var stdout, stderr bytes.Buffer
	env := &fakeEnv{values: map[string]string{dataDirEnvKey: dataDir}}
	code := run(context.Background(), []string{
		"local-install",
		"--folder", folder,
		"--source", "local",
		"--reason", "canary test",
		"--operator", "alice",
	}, &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("exit: got %d want 0; stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	for _, canary := range []string{canary1, canary2, canary3} {
		if strings.Contains(out, canary) {
			t.Errorf("JSONL stdout leaks canary %q: %s", canary, out)
		}
	}
}

// TestRun_LocalInstall_DisallowedSource_Refused asserts the env-
// allowlist gate (iter-1 codex M6 fix).
func TestRun_LocalInstall_DisallowedSource_Refused(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"local-install",
		"--folder", "/dev/null",
		"--source", "github-tools",
		"--reason", "test",
		"--operator", "alice",
		"--data-dir", "/tmp/wk-test",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "allowlist") {
		t.Errorf("stderr missing allowlist diagnostic: %q", stderr.String())
	}
}

// TestRun_LocalInstall_DisallowedSource_AllowedViaEnv asserts the
// env-allowlist supports comma-separated overrides.
func TestRun_LocalInstall_DisallowedSource_AllowedViaEnv(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	folder := t.TempDir()
	mustWrite(t, filepath.Join(folder, "manifest.json"), validManifest("demo_tool", "1.0.0"))
	mustWrite(t, filepath.Join(folder, "src/x.ts"), []byte(`x`))
	env := &fakeEnv{values: map[string]string{
		dataDirEnvKey:      dataDir,
		localSourcesEnvKey: "ops,custom-local",
	}}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"local-install",
		"--folder", folder,
		"--source", "custom-local",
		"--reason", "extended allowlist test",
		"--operator", "alice",
	}, &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("exit: got %d want 0; stderr=%s", code, stderr.String())
	}
}

// TestRun_LocalInstall_MalformedOperator_Refused asserts the
// flag-level operator-id allowlist (iter-1 critic M2 fix) — the
// malformed value never reaches the wrapped error on stderr.
func TestRun_LocalInstall_MalformedOperator_Refused(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"local-install",
		"--folder", "/dev/null",
		"--source", "local",
		"--reason", "test",
		"--operator", "alice; rm -rf /",
		"--data-dir", "/tmp/wk-test",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit: got %d want 2", code)
	}
	if !strings.Contains(stderr.String(), "invalid --operator") {
		t.Errorf("stderr missing operator diagnostic: %q", stderr.String())
	}
	// The raw malformed value MUST NOT echo back.
	if strings.Contains(stderr.String(), "rm -rf") {
		t.Errorf("stderr echoes the raw malformed operator value: %q", stderr.String())
	}
}

// mustWrite writes `data` to `path`, creating parent dirs.
func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

// validManifest returns a minimal valid manifest body.
func validManifest(name, version string) []byte {
	return []byte(`{
"name":"` + name + `",
"version":"` + version + `",
"capabilities":["cap.test"],
"schema":{"type":"object"},
"dry_run_mode":"none"
}`)
}
