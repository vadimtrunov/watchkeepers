package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeEnv satisfies [envLookup] for tests so $WATCHKEEPER_DATA_DIR /
// $WATCHKEEPER_KEEP_BASE_URL / $WATCHKEEPER_OPERATOR_TOKEN stay out of
// the process env. Mirror `core/cmd/wk-tool/run_test.go` (M9.5).
type fakeEnv struct{ values map[string]string }

func (e *fakeEnv) LookupEnv(key string) (string, bool) {
	v, ok := e.values[key]
	return v, ok
}

// --- root dispatcher --------------------------------------------------------

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
	if !strings.Contains(stdout.String(), "Usage: wk") {
		t.Errorf("usage missing: %q", stdout.String())
	}
}

// --- migrated wk-tool tests under `wk tool <subcommand>` --------------------

func TestRun_LocalInstall_MissingReason_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"tool", "local-install",
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
		"tool", "local-install",
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
		"tool", "rollback",
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
		"tool", "local-install",
		"--folder", folder,
		"--source", "local",
		"--reason", "Hot-fix for incident #4711.",
		"--operator", "alice",
	}, &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("exit: got %d want 0; stderr=%s", code, stderr.String())
	}
	live := filepath.Join(dataDir, "tools", "local", "demo_tool", "manifest.json")
	if _, err := os.Stat(live); err != nil {
		t.Errorf("live manifest missing: %v", err)
	}
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
}

func TestRun_LocalInstall_ThenRollback_RestoresPriorVersion(t *testing.T) {
	t.Parallel()
	dataDir := t.TempDir()
	folder1 := t.TempDir()
	mustWrite(t, filepath.Join(folder1, "manifest.json"), validManifest("demo_tool", "0.9.0"))
	mustWrite(t, filepath.Join(folder1, "src/x.ts"), []byte(`v0_9`))
	env := &fakeEnv{values: map[string]string{dataDirEnvKey: dataDir}}
	code := run(context.Background(), []string{
		"tool", "local-install",
		"--folder", folder1,
		"--source", "local",
		"--reason", "initial install",
		"--operator", "alice",
	}, io.Discard, io.Discard, env)
	if code != 0 {
		t.Fatalf("install v0.9 failed: code=%d", code)
	}
	folder2 := t.TempDir()
	mustWrite(t, filepath.Join(folder2, "manifest.json"), validManifest("demo_tool", "1.0.0"))
	mustWrite(t, filepath.Join(folder2, "src/x.ts"), []byte(`v1_0`))
	code = run(context.Background(), []string{
		"tool", "local-install",
		"--folder", folder2,
		"--source", "local",
		"--reason", "upgrade to 1.0",
		"--operator", "alice",
	}, io.Discard, io.Discard, env)
	if code != 0 {
		t.Fatalf("install v1.0 failed: code=%d", code)
	}
	var rbOut bytes.Buffer
	code = run(context.Background(), []string{
		"tool", "rollback",
		"--name", "demo_tool",
		"--to", "0.9.0",
		"--source", "local",
		"--reason", "v1.0 broke something",
		"--operator", "alice",
	}, &rbOut, io.Discard, env)
	if code != 0 {
		t.Fatalf("rollback failed: code=%d output=%s", code, rbOut.String())
	}
	live := filepath.Join(dataDir, "tools", "local", "demo_tool", "src/x.ts")
	got, err := os.ReadFile(live)
	if err != nil {
		t.Fatalf("read live: %v", err)
	}
	if string(got) != "v0_9" {
		t.Errorf("live src not restored: got %q want %q", got, "v0_9")
	}
}

// TestRun_LocalInstall_JSONLStdoutOmitsFolderContentCanary preserves the
// M9.5 iter-1 critic M1 redaction-harness coverage across the migration.
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
		"tool", "local-install",
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

func TestRun_LocalInstall_DisallowedSource_Refused(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"tool", "local-install",
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
		"tool", "local-install",
		"--folder", folder,
		"--source", "custom-local",
		"--reason", "extended allowlist test",
		"--operator", "alice",
	}, &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("exit: got %d want 0; stderr=%s", code, stderr.String())
	}
}

func TestRun_LocalInstall_MalformedOperator_Refused(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"tool", "local-install",
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
	if strings.Contains(stderr.String(), "rm -rf") {
		t.Errorf("stderr echoes the raw malformed operator value: %q", stderr.String())
	}
}

// --- migrated wk-tool m96 + share tests --------------------------------------

func TestRun_HostedExport_MissingFlags_UsageError(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := &fakeEnv{values: map[string]string{}}
	rc := run(context.Background(), []string{"tool", "hosted", "export"}, stdout, stderr, env)
	if rc != 2 {
		t.Fatalf("rc=%d want 2; stderr=%s", rc, stderr.String())
	}
	if !strings.Contains(stderr.String(), "missing required flag") {
		t.Errorf("stderr=%q want missing-flags diagnostic", stderr.String())
	}
}

func TestRun_Share_MissingTokenEnv_UsageError(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := &fakeEnv{values: map[string]string{
		"WATCHKEEPER_DATA_DIR": "/tmp/x",
	}}
	rc := run(context.Background(), []string{
		"tool", "share",
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

func TestRun_Share_InvalidTarget_UsageError(t *testing.T) {
	t.Parallel()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := &fakeEnv{values: map[string]string{
		"WATCHKEEPER_DATA_DIR":     "/tmp/x",
		"WATCHKEEPER_GITHUB_TOKEN": "ghp_synthetic_token", //nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.
	}}
	rc := run(context.Background(), []string{
		"tool", "share",
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
	if strings.Contains(stderr.String(), "rm -rf /") {
		t.Errorf("stderr echoes bad --target value: %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "must be 'platform' or 'private'") {
		t.Errorf("stderr=%q does not name accepted values", stderr.String())
	}
}

func TestRun_HostedExport_EndToEnd_OnRealFS(t *testing.T) {
	t.Parallel()
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
		"tool", "hosted", "export",
		"--source", "hosted-private",
		"--tool", "weekly_digest",
		"--destination", dest,
		"--reason", "graduating",
		"--operator", "alice@example.com",
	}, stdout, stderr, env)
	if rc != 0 {
		t.Fatalf("rc=%d want 0; stderr=%s; stdout=%s", rc, stderr.String(), stdout.String())
	}
	if _, err := os.Stat(filepath.Join(dest, "manifest.json")); err != nil {
		t.Errorf("dest manifest missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "src", "index.ts")); err != nil {
		t.Errorf("dest src missing: %v", err)
	}
	if !strings.Contains(stdout.String(), "tool hosted export ok") {
		t.Errorf("stdout missing ok summary: %s", stdout.String())
	}
}

func TestRun_HostedExport_MalformedOperator_Refused(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	dataDir := filepath.Join(tmp, "data")
	root := filepath.Join(dataDir, "tools", "hosted-private", "weekly_digest")
	_ = os.MkdirAll(root, 0o755)
	_ = os.WriteFile(filepath.Join(root, "manifest.json"), []byte(`{"name":"weekly_digest","version":"1.0.0","capabilities":["read:logs"],"schema":{"type":"object"},"dry_run_mode":"none"}`), 0o600)

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := &fakeEnv{values: map[string]string{"WATCHKEEPER_DATA_DIR": dataDir}}
	rc := run(context.Background(), []string{
		"tool", "hosted", "export",
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

// TestRun_Share_TokenCanaryNotEchoedToStdoutOrStderr preserves the
// M9.4.b redaction-harness coverage across the migration. The token
// is loaded from $WATCHKEEPER_GITHUB_TOKEN; if a future regression
// echoes it to stdout / stderr the canary substring fires.
func TestRun_Share_TokenCanaryNotEchoedToStdoutOrStderr(t *testing.T) {
	t.Parallel()
	const canaryToken = "CANARY-GH-TOKEN-DO-NOT-LEAK-c4n4ry-t0k3n-abc" //nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	env := &fakeEnv{values: map[string]string{
		"WATCHKEEPER_DATA_DIR":     "/tmp/x",
		"WATCHKEEPER_GITHUB_TOKEN": canaryToken,
	}}
	rc := run(context.Background(), []string{
		"tool", "share",
		"--source", "private",
		"--tool", "weekly_digest",
		"--target", "platform",
		"--target-owner", "watchkeepers",
		"--target-repo", "watchkeeper-tools",
		"--reason", "canary test",
		"--proposer", "agent-canary",
	}, stdout, stderr, env)
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

// --- new Keep-backed subcommand tests ---------------------------------------

// fakeKeep stands up a tiny in-process Keep server that satisfies the
// keepclient's auth contract (`/v1/...` requires a bearer token; `/health`
// does not). Each handler is registered per-test so the server only
// responds to the routes the test exercises.
func fakeKeep(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for pattern, h := range handlers {
		mux.HandleFunc(pattern, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func keepEnv(srv *httptest.Server) *fakeEnv {
	return &fakeEnv{values: map[string]string{
		keepBaseURLEnvKey:   srv.URL,
		operatorTokenEnvKey: "test-bearer-token",
	}}
}

func TestRun_Spawn_HappyPath(t *testing.T) {
	t.Parallel()
	srv := fakeKeep(t, map[string]http.HandlerFunc{
		"POST /v1/watchkeepers": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"id":"00000000-0000-0000-0000-000000000001"}`)
		},
	})
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"spawn",
		"--manifest", "11111111-1111-1111-1111-111111111111",
		"--lead", "22222222-2222-2222-2222-222222222222",
		"--reason", "iter-1-C2",
	}, &stdout, &stderr, keepEnv(srv))
	if code != 0 {
		t.Fatalf("exit=%d want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "watchkeeper_id=00000000-0000-0000-0000-000000000001") {
		t.Errorf("stdout missing id: %q", stdout.String())
	}
	// Iter-1 critic C2: --reason is echoed in the operator-facing summary.
	if !strings.Contains(stdout.String(), `reason="iter-1-C2"`) {
		t.Errorf("stdout missing reason echo: %q", stdout.String())
	}
}

// TestRun_Spawn_NoInheritFlag_EchoedOnSuccess pins the Phase 2
// §M7.1.c operator opt-out flag at the CLI surface: `--no-inherit`
// is accepted AND reflected in the success message so an audit-aware
// reader of a shell transcript has a record of the operator's intent.
// The flag is currently a CLI-surface declaration only — the full
// round-trip into `saga.SpawnContext.NoInherit` lands when the
// future Slack-bot binary wires the kickoffer; see the
// `core/internal/keep/approval_wiring/wiring.go` DEFERRED WIRING
// note and the M7.1.c lesson appendix.
func TestRun_Spawn_NoInheritFlag_EchoedOnSuccess(t *testing.T) {
	t.Parallel()
	srv := fakeKeep(t, map[string]http.HandlerFunc{
		"POST /v1/watchkeepers": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"id":"00000000-0000-0000-0000-000000000099"}`)
		},
	})
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"spawn",
		"--manifest", "11111111-1111-1111-1111-111111111111",
		"--lead", "22222222-2222-2222-2222-222222222222",
		"--reason", "M7.1.c smoke",
		"--no-inherit",
	}, &stdout, &stderr, keepEnv(srv))
	if code != 0 {
		t.Fatalf("exit=%d want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no_inherit=true") {
		t.Errorf("stdout missing no_inherit echo: %q", stdout.String())
	}
}

// TestRun_Spawn_DefaultNoInherit_FalseEcho pins the default
// (omitted-flag) shape of the success message: `no_inherit=false`
// must always be present so a downstream parser does not need to
// branch on flag presence.
func TestRun_Spawn_DefaultNoInherit_FalseEcho(t *testing.T) {
	t.Parallel()
	srv := fakeKeep(t, map[string]http.HandlerFunc{
		"POST /v1/watchkeepers": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"id":"00000000-0000-0000-0000-000000000098"}`)
		},
	})
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"spawn",
		"--manifest", "11111111-1111-1111-1111-111111111111",
		"--lead", "22222222-2222-2222-2222-222222222222",
		"--reason", "M7.1.c default",
	}, &stdout, &stderr, keepEnv(srv))
	if code != 0 {
		t.Fatalf("exit=%d want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no_inherit=false") {
		t.Errorf("stdout missing default no_inherit=false echo: %q", stdout.String())
	}
}

func TestRun_Spawn_MissingKeepEnv_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"spawn",
		"--manifest", "11111111-1111-1111-1111-111111111111",
		"--lead", "22222222-2222-2222-2222-222222222222",
		"--reason", "iter-1-C2",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "WATCHKEEPER_KEEP_BASE_URL") {
		t.Errorf("stderr missing env var name: %q", stderr.String())
	}
}

// TestIter1_Spawn_MissingReason_UsageError pins critic C2 — every
// write subcommand requires --reason and the diagnostic names the
// flag explicitly.
func TestIter1_Spawn_MissingReason_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"spawn",
		"--manifest", "11111111-1111-1111-1111-111111111111",
		"--lead", "22222222-2222-2222-2222-222222222222",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "--reason") {
		t.Errorf("stderr missing --reason diagnostic: %q", stderr.String())
	}
}

func TestRun_List_HappyPath(t *testing.T) {
	t.Parallel()
	srv := fakeKeep(t, map[string]http.HandlerFunc{
		"GET /v1/watchkeepers": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"items":[
				{"id":"a","manifest_id":"m1","lead_human_id":"h1","active_manifest_version_id":null,"status":"active","spawned_at":null,"retired_at":null,"archive_uri":null,"created_at":"2025-01-01T00:00:00Z"}
			]}`)
		},
	})
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"list"}, &stdout, &stderr, keepEnv(srv))
	if code != 0 {
		t.Fatalf("exit=%d want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "a\tactive\tm1\th1") {
		t.Errorf("stdout missing row: %q", stdout.String())
	}
}

func TestRun_Inspect_HappyPath(t *testing.T) {
	t.Parallel()
	srv := fakeKeep(t, map[string]http.HandlerFunc{
		"GET /v1/watchkeepers/{id}": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"id":"a","manifest_id":"m1","lead_human_id":"h1","active_manifest_version_id":null,"status":"active","spawned_at":null,"retired_at":null,"archive_uri":null,"created_at":"2025-01-01T00:00:00Z"}`)
		},
	})
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"inspect", "a"}, &stdout, &stderr, keepEnv(srv))
	if code != 0 {
		t.Fatalf("exit=%d want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "id              a") || !strings.Contains(stdout.String(), "status          active") {
		t.Errorf("stdout missing key lines: %q", stdout.String())
	}
}

func TestRun_Retire_HappyPath(t *testing.T) {
	t.Parallel()
	srv := fakeKeep(t, map[string]http.HandlerFunc{
		"PATCH /v1/watchkeepers/{id}/status": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		},
	})
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"retire",
		"--archive-uri", "file:///var/archives/notebook/00000000-0000-0000-0000-000000000001.sqlite",
		"--reason", "iter-1-C1",
		"00000000-0000-0000-0000-000000000001",
	}, &stdout, &stderr, keepEnv(srv))
	if code != 0 {
		t.Fatalf("exit=%d want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "retire ok watchkeeper_id=00000000-0000-0000-0000-000000000001") {
		t.Errorf("stdout missing: %q", stdout.String())
	}
}

// TestIter1_Retire_MissingArchive_UsageError pins critic C1 —
// `wk retire` without --archive-uri must NOT take the legacy
// no-archive path; the CLI gate is the only thing keeping the
// M7.2.c archive-on-retire invariant whole on the operator surface.
func TestIter1_Retire_MissingArchive_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"retire",
		"--reason", "iter-1-C1",
		"00000000-0000-0000-0000-000000000001",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "--archive-uri") {
		t.Errorf("stderr missing --archive-uri diagnostic: %q", stderr.String())
	}
}

// TestIter1_Retire_MissingReason_UsageError pins critic C2 on retire.
func TestIter1_Retire_MissingReason_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"retire",
		"--archive-uri", "file:///var/archives/notebook/00000000-0000-0000-0000-000000000001.sqlite",
		"00000000-0000-0000-0000-000000000001",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "--reason") {
		t.Errorf("stderr missing --reason diagnostic: %q", stderr.String())
	}
}

func TestRun_Logs_FiltersByActor(t *testing.T) {
	t.Parallel()
	const wkID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const otherID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	srv := fakeKeep(t, map[string]http.HandlerFunc{
		"GET /v1/keepers-log": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"events":[
				{"id":"1","event_type":"e1","actor_watchkeeper_id":"`+wkID+`","payload":{},"created_at":"2025-01-01T00:00:00Z"},
				{"id":"2","event_type":"e2","actor_watchkeeper_id":"`+otherID+`","payload":{},"created_at":"2025-01-01T00:00:01Z"},
				{"id":"3","event_type":"e3","actor_watchkeeper_id":"`+wkID+`","payload":{},"created_at":"2025-01-01T00:00:02Z"}
			]}`)
		},
	})
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"logs", wkID}, &stdout, &stderr, keepEnv(srv))
	if code != 0 {
		t.Fatalf("exit=%d want 0; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "e1") || !strings.Contains(stdout.String(), "e3") {
		t.Errorf("stdout missing matching events: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "e2") {
		t.Errorf("stdout leaked non-matching event: %q", stdout.String())
	}
}

// TestIter1_Logs_MalformedUUID_UsageError pins critic M6 — `wk logs`
// rejects non-canonical-UUID positionals client-side instead of
// silently producing zero matches against a malformed actor filter.
func TestIter1_Logs_MalformedUUID_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"logs", "not-a-uuid"}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "canonical UUID") {
		t.Errorf("stderr missing UUID-shape diagnostic: %q", stderr.String())
	}
}

// TestIter1_Logs_NegativeLimit_UsageError pins critic M6 — `wk logs
// --limit -1` surfaces a usage error (exit 2) instead of the
// keepclient's runtime "invalid request" (exit 1).
func TestIter1_Logs_NegativeLimit_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"logs", "--limit", "-1", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "non-negative") {
		t.Errorf("stderr missing negative-limit diagnostic: %q", stderr.String())
	}
}

// TestIter1_Inspect_WhitespacePositional_UsageError pins critic M9 —
// trim+reject whitespace-only <wk-id> before any HTTP round-trip.
func TestIter1_Inspect_WhitespacePositional_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"inspect", "   "}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "must be non-empty") {
		t.Errorf("stderr missing non-empty diagnostic: %q", stderr.String())
	}
}

// TestIter1_NotebookShow_ExtraPositional_UsageError pins critic M8 —
// popPositionalNotebook now rejects extra positional args after
// <wk-id> with a usage error (mirrors popPositional's strictness).
func TestIter1_NotebookShow_ExtraPositional_UsageError(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"notebook", "show",
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 2 {
		t.Errorf("exit=%d want 2", code)
	}
	if !strings.Contains(stderr.String(), "extra positional") {
		t.Errorf("stderr missing extra-positional diagnostic: %q", stderr.String())
	}
}

// TestIter1_ApprovalsInspect_FlagBeforeStub pins critic m7 — the
// stub returns exit 3 BEFORE flag.Parse, so a future-shape flag
// (e.g. `--id <uuid>`) does not leak as exit 2.
func TestIter1_ApprovalsInspect_FlagBeforeStub(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"approvals", "inspect", "--id", "anything"}, &stdout, &stderr, &fakeEnv{})
	if code != 3 {
		t.Errorf("exit=%d want 3", code)
	}
}

// TestIter1_PersonalitySet_ConflictHint pins critic M1 — the
// read-merge-write GET→PUT race surfaces a tailored
// "concurrent edit detected" diagnostic when the server returns 409.
func TestIter1_PersonalitySet_ConflictHint(t *testing.T) {
	t.Parallel()
	srv := fakeKeep(t, map[string]http.HandlerFunc{
		"GET /v1/watchkeepers/{id}": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"id":"a","manifest_id":"22222222-2222-2222-2222-222222222222","lead_human_id":"h1","active_manifest_version_id":null,"status":"active","spawned_at":null,"retired_at":null,"archive_uri":null,"created_at":"2025-01-01T00:00:00Z"}`)
		},
		"GET /v1/manifests/{manifest_id}": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"id":"v1","manifest_id":"22222222-2222-2222-2222-222222222222","version_no":3,"system_prompt":"hello","tools":[],"authority_matrix":{},"knowledge_sources":[],"personality":"old","language":"en","created_at":"2025-01-01T00:00:00Z"}`)
		},
		"PUT /v1/manifests/{manifest_id}/versions": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":"version_no_taken","reason":"another writer won the race"}`)
		},
	})
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"personality", "set",
		"--value", "new tone",
		"--reason", "iter-1-M1",
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
	}, &stdout, &stderr, keepEnv(srv))
	if code != 1 {
		t.Errorf("exit=%d want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "concurrent edit detected") {
		t.Errorf("stderr missing conflict hint: %q", stderr.String())
	}
}

// TestIter1_NotebookExport_DestinationExists_UsageError pins critic M7 —
// O_EXCL refuses to overwrite an existing destination file (and the
// CLI surfaces it cleanly as exit 1 with a file-create diagnostic).
func TestIter1_NotebookExport_DestinationExists_UsageError(t *testing.T) {
	t.Parallel()
	dst := filepath.Join(t.TempDir(), "existing.archive")
	if err := os.WriteFile(dst, []byte("prior content"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{
		"notebook", "export",
		"--destination", dst,
		"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
	}, &stdout, &stderr, &fakeEnv{})
	if code != 1 {
		t.Errorf("exit=%d want 1", code)
	}
	// The O_EXCL diagnostic must surface in stderr.
	if !strings.Contains(stderr.String(), "create") {
		t.Errorf("stderr missing create diagnostic: %q", stderr.String())
	}
}

// --- stub tests -------------------------------------------------------------

func TestRun_ApprovalsPending_NotWired(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"approvals", "pending"}, &stdout, &stderr, &fakeEnv{})
	if code != 3 {
		t.Errorf("exit=%d want 3", code)
	}
	if !strings.Contains(stderr.String(), "M10.2.c follow-up") {
		t.Errorf("stderr missing follow-up: %q", stderr.String())
	}
}

func TestRun_ToolsSourcesSync_NotWired(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"tools", "sources", "sync"}, &stdout, &stderr, &fakeEnv{})
	if code != 3 {
		t.Errorf("exit=%d want 3", code)
	}
}

func TestRun_BudgetSet_NotWired(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"budget", "set"}, &stdout, &stderr, &fakeEnv{})
	if code != 3 {
		t.Errorf("exit=%d want 3", code)
	}
}

func TestRun_ToolList_NotWired(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"tool", "list"}, &stdout, &stderr, &fakeEnv{})
	if code != 3 {
		t.Errorf("exit=%d want 3", code)
	}
}

func TestRun_NotebookArchive_NotWired(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := run(context.Background(), []string{"notebook", "archive"}, &stdout, &stderr, &fakeEnv{})
	if code != 3 {
		t.Errorf("exit=%d want 3", code)
	}
}

// --- helpers ----------------------------------------------------------------

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", path, err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

func validManifest(name, version string) []byte {
	return []byte(`{
"name":"` + name + `",
"version":"` + version + `",
"capabilities":["cap.test"],
"schema":{"type":"object"},
"dry_run_mode":"none"
}`)
}
