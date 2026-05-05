package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// fakeFS is the in-memory [fileSystem] tests inject so file IO is
// deterministic and concurrency-safe across t.Parallel suites.
type fakeFS struct {
	mu      sync.Mutex
	read    map[string][]byte
	written map[string]fakeWrite
}

// fakeWrite records the bytes + mode each WriteFile call observes.
// Tests assert the credentials file mode is 0o600 by checking this map.
type fakeWrite struct {
	data []byte
	perm os.FileMode
}

func newFakeFS(read map[string][]byte) *fakeFS {
	if read == nil {
		read = map[string][]byte{}
	}
	return &fakeFS{read: read, written: map[string]fakeWrite{}}
}

func (f *fakeFS) ReadFile(path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if data, ok := f.read[path]; ok {
		return data, nil
	}
	return nil, fmt.Errorf("fakeFS: %s not found", path)
}

func (f *fakeFS) WriteFile(path string, data []byte, perm os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := append([]byte(nil), data...)
	f.written[path] = fakeWrite{data: cp, perm: perm}
	return nil
}

func (f *fakeFS) get(path string) (fakeWrite, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w, ok := f.written[path]
	return w, ok
}

// fakeEnv is the in-memory [envLookup] tests inject. Empty values are
// distinguishable from missing keys so the EnvSource-equivalent
// "empty == not set" rule can be exercised.
type fakeEnv map[string]string

func (e fakeEnv) LookupEnv(key string) (string, bool) {
	v, ok := e[key]
	return v, ok
}

// canonicalManifestYAML is the minimal valid manifest reused across
// tests so each setup stays compact. Mirrors the shape an operator
// would hand-edit: name + description + scopes + a metadata key the
// slack adapter is documented to forward.
const canonicalManifestYAML = `name: watchkeeper-dev
description: Watchkeeper dev bot
scopes:
  - chat:write
  - users:read
metadata:
  socket_mode_enabled: "true"
`

// canonicalCreateAppOK is the canonical Slack `apps.manifest.create`
// happy-path response body. Carries every credential field the sink
// must capture so the assertions cover the full surface in one place.
//
// SECURITY NOTE: every value is a synthetic placeholder. None of these
// strings are real Slack credentials. The fake values are scrubbed in
// the binary-grep test (TestSpawnDevBot_BinaryHasNoTokenLeaks) by
// requiring the prefix `xoxe-` / `xoxb-` / `xoxp-` / `xapp-` to be
// absent — the placeholders below avoid those prefixes precisely so
// the grep test stays meaningful.
const canonicalCreateAppOK = `{
		"ok": true,
		"app_id": "A0123ABCDEF",
		"credentials": {
			"client_id": "0123456789.0987654321",
			"client_secret": "fake-client-secret-AAAA",
			"verification_token": "fake-verification-BBBB",
			"signing_secret": "fake-signing-secret-CCCC"
		}
	}`

// stubSlackServer returns an httptest server that responds to
// /apps.manifest.create with the supplied body and status, capturing
// the request body so tests can assert wire-format expectations.
func stubSlackServer(t *testing.T, status int, body string) (*httptest.Server, *bytes.Buffer) {
	t.Helper()
	captured := &bytes.Buffer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/apps.manifest.create" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if _, err := io.Copy(captured, r.Body); err != nil {
			t.Errorf("read body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// TestRun_HappyPath_LiveAgainstHTTPTestStub asserts the canonical
// end-to-end flow: read manifest → resolve config token → call
// CreateApp → sink fires → credentials JSON written → redacted summary
// printed. The httptest stub stands in for Slack so the test runs with
// zero external dependencies.
func TestRun_HappyPath_LiveAgainstHTTPTestStub(t *testing.T) {
	t.Parallel()

	srv, captured := stubSlackServer(t, http.StatusOK, canonicalCreateAppOK)

	fs := newFakeFS(map[string][]byte{
		"/manifest.yaml": []byte(canonicalManifestYAML),
	})
	// #nosec G101 -- test fixture, not a real credential.
	env := fakeEnv{"WATCHKEEPER_SLACK_CONFIG_TOKEN": "xoxe.xoxp-1-test-config-token"}

	var stdout, stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{
			"--manifest", "/manifest.yaml",
			"--credentials-out", "/creds.json",
			"--base-url", srv.URL,
		},
		&stdout, &stderr,
		env, fs,
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	// Stdout carries the redacted summary — AppID present, no
	// credentials.
	out := stdout.String()
	if !strings.Contains(out, "A0123ABCDEF") {
		t.Errorf("stdout missing app_id; got %s", out)
	}
	for _, leaked := range []string{
		"fake-client-secret-AAAA", "fake-verification-BBBB",
		"fake-signing-secret-CCCC", "0123456789.0987654321",
	} {
		if strings.Contains(out, leaked) {
			t.Errorf("stdout leaked credential %q; got %s", leaked, out)
		}
	}

	// Credentials file written with mode 0o600 carrying the full
	// credentials bundle.
	w, ok := fs.get("/creds.json")
	if !ok {
		t.Fatal("credentials file not written")
	}
	if w.perm != credentialsFileMode {
		t.Errorf("credentials file mode = %#o, want %#o", w.perm, credentialsFileMode)
	}
	var got credentialsFile
	if err := json.Unmarshal(w.data, &got); err != nil {
		t.Fatalf("decode credentials file: %v", err)
	}
	// #nosec G101 -- test fixtures, not real credentials.
	want := credentialsFile{
		AppID:             "A0123ABCDEF",
		ClientID:          "0123456789.0987654321",
		ClientSecret:      "fake-client-secret-AAAA",
		SigningSecret:     "fake-signing-secret-CCCC",
		VerificationToken: "fake-verification-BBBB",
	}
	if got != want {
		t.Errorf("credentials file = %+v, want %+v", got, want)
	}

	// Wire body carries the manifest with the documented scopes and
	// metadata flags routed through the slack adapter.
	if !strings.Contains(captured.String(), `"name":"watchkeeper-dev"`) {
		t.Errorf("request body missing manifest name; got %s", captured.String())
	}
	if !strings.Contains(captured.String(), `"socket_mode_enabled":true`) {
		t.Errorf("request body missing socket_mode_enabled bool; got %s", captured.String())
	}
}

// TestRun_DryRun_DoesNotContactSlackOrSecrets asserts that --dry-run
// prints the resolved Slack manifest body and exits 0 WITHOUT reading
// the config token or hitting the network. Useful as a CI gate
// against malformed manifests.
func TestRun_DryRun_DoesNotContactSlackOrSecrets(t *testing.T) {
	t.Parallel()

	// httptest server that fails the test if hit — proves dry-run is
	// fully offline.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("dry-run must not contact Slack")
	}))
	t.Cleanup(srv.Close)

	fs := newFakeFS(map[string][]byte{
		"/manifest.yaml": []byte(canonicalManifestYAML),
	})
	env := fakeEnv{} // empty — proves --dry-run does not look up the config token

	var stdout, stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{
			"--manifest", "/manifest.yaml",
			"--credentials-out", "/creds.json",
			"--base-url", srv.URL,
			"--dry-run",
		},
		&stdout, &stderr,
		env, fs,
	)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}

	// Stdout carries a JSON document; the manifest name is present.
	if !strings.Contains(stdout.String(), `"name": "watchkeeper-dev"`) {
		t.Errorf("dry-run output missing manifest name; got %s", stdout.String())
	}
	// No credentials file is written in dry-run.
	if _, ok := fs.get("/creds.json"); ok {
		t.Errorf("dry-run wrote a credentials file; should not")
	}
}

// TestRun_FlagValidation_MissingRequired asserts each required-flag
// validation surfaces exit code 2 with a stable diagnostic phrase.
// Mirrors LESSON M2.1.b — locale-independent error text.
func TestRun_FlagValidation_MissingRequired(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		args   []string
		phrase string
	}{
		{"manifest", []string{"--credentials-out", "/c.json"}, "--manifest is required"},
		{"credentials_out", []string{"--manifest", "/m.yaml"}, "--credentials-out is required"},
		{
			"config_token_key_empty",
			[]string{"--manifest", "/m.yaml", "--credentials-out", "/c.json", "--config-token-key", ""},
			"--config-token-key is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fs := newFakeFS(nil)
			var stdout, stderr bytes.Buffer
			code := run(context.Background(), tc.args, &stdout, &stderr, fakeEnv{}, fs)
			if code != 2 {
				t.Errorf("exit code = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), tc.phrase) {
				t.Errorf("stderr = %q, want phrase %q", stderr.String(), tc.phrase)
			}
		})
	}
}

// TestRun_ConfigTokenNotFound asserts the "secret missing" path exits
// 1 with a redaction-clean diagnostic — the missing key NAME appears,
// but no value (the value is empty by definition).
func TestRun_ConfigTokenNotFound(t *testing.T) {
	t.Parallel()

	fs := newFakeFS(map[string][]byte{
		"/manifest.yaml": []byte(canonicalManifestYAML),
	})
	var stdout, stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{
			"--manifest", "/manifest.yaml",
			"--credentials-out", "/creds.json",
		},
		&stdout, &stderr,
		fakeEnv{}, fs,
	)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "WATCHKEEPER_SLACK_CONFIG_TOKEN") {
		t.Errorf("stderr missing config-token key name; got %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "config token not found") {
		t.Errorf("stderr missing stable phrase; got %s", stderr.String())
	}
}

// TestRun_SlackInvalidManifest_ExitsOne asserts that a Slack
// `invalid_manifest` response surfaces as exit code 1 (runtime
// failure) with a diagnostic that includes the portable sentinel.
func TestRun_SlackInvalidManifest_ExitsOne(t *testing.T) {
	t.Parallel()

	srv, _ := stubSlackServer(t, http.StatusOK, `{"ok":false,"error":"invalid_manifest"}`)
	fs := newFakeFS(map[string][]byte{
		"/manifest.yaml": []byte(canonicalManifestYAML),
	})
	// #nosec G101 -- test fixture, not a real credential.
	env := fakeEnv{"WATCHKEEPER_SLACK_CONFIG_TOKEN": "xoxe.xoxp-1-test"}

	var stdout, stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{
			"--manifest", "/manifest.yaml",
			"--credentials-out", "/creds.json",
			"--base-url", srv.URL,
		},
		&stdout, &stderr,
		env, fs,
	)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "create app") {
		t.Errorf("stderr missing failure context; got %s", stderr.String())
	}
	// No credentials file written on failure.
	if _, ok := fs.get("/creds.json"); ok {
		t.Errorf("credentials file written on Slack failure; should not")
	}
}

// TestParseManifest_HappyPath asserts the YAML decoder accepts the
// canonical shape and produces the expected fields.
func TestParseManifest_HappyPath(t *testing.T) {
	t.Parallel()

	m, err := parseManifest([]byte(canonicalManifestYAML))
	if err != nil {
		t.Fatalf("parseManifest: %v", err)
	}
	if m.Name != "watchkeeper-dev" {
		t.Errorf("Name = %q, want watchkeeper-dev", m.Name)
	}
	if len(m.Scopes) != 2 || m.Scopes[0] != "chat:write" || m.Scopes[1] != "users:read" {
		t.Errorf("Scopes = %v, want [chat:write users:read]", m.Scopes)
	}
	if m.Metadata["socket_mode_enabled"] != "true" {
		t.Errorf("Metadata[socket_mode_enabled] = %q, want true", m.Metadata["socket_mode_enabled"])
	}
}

// TestParseManifest_MissingName asserts the empty-name validation
// surfaces the portable sentinel.
func TestParseManifest_MissingName(t *testing.T) {
	t.Parallel()

	_, err := parseManifest([]byte(`description: missing name`))
	if !errors.Is(err, ErrManifestNameMissing) {
		t.Errorf("err = %v, want ErrManifestNameMissing", err)
	}
}

// TestParseManifest_MalformedYAML asserts that malformed YAML wraps
// the parse-error sentinel.
func TestParseManifest_MalformedYAML(t *testing.T) {
	t.Parallel()

	_, err := parseManifest([]byte("name: ok\nscopes: [unterminated"))
	if !errors.Is(err, ErrManifestParse) {
		t.Errorf("err = %v, want ErrManifestParse", err)
	}
}

// TestParseManifest_StrictMode_RejectsTypoedKey asserts strict mode
// flags an unrecognised top-level key. Operator-friendly: a typo'd
// `scopess:` line never silently drops the scope list.
func TestParseManifest_StrictMode_RejectsTypoedKey(t *testing.T) {
	t.Parallel()

	in := []byte("name: ok\nscopess: [chat:write]")
	_, err := parseManifest(in)
	if !errors.Is(err, ErrManifestParse) {
		t.Errorf("err = %v, want ErrManifestParse", err)
	}
}
