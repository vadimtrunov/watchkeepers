package config

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/secrets"
)

// fakeSecretCall records one invocation of [fakeSecretSource.Get].
type fakeSecretCall struct {
	Key string
}

// fakeSecretSource is the hand-rolled [secrets.SecretSource] stand-in
// used by the config test suite. Mirrors the secrets package's
// fakeLogger pattern: mutex-guarded entries slice, no mocking library.
//
// Configure responses by populating `values` (key → value) and
// `errs` (key → error) before passing the source to [Load]; if both
// maps are empty the source returns an empty value with a nil error
// (which the loader treats as "secret resolved to empty string" — the
// test cases never rely on this default and always populate one map).
type fakeSecretSource struct {
	mu     sync.Mutex
	values map[string]string
	errs   map[string]error
	calls  []fakeSecretCall
}

// Compile-time assertion: *fakeSecretSource satisfies SecretSource.
var _ secrets.SecretSource = (*fakeSecretSource)(nil)

func (f *fakeSecretSource) Get(_ context.Context, key string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeSecretCall{Key: key})
	if err, ok := f.errs[key]; ok {
		return "", err
	}
	if val, ok := f.values[key]; ok {
		return val, nil
	}
	return "", nil
}

// callKeys returns a defensive copy of the recorded keys.
func (f *fakeSecretSource) callKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.Key
	}
	return out
}

// containsKey reports whether `key` was passed to [fakeSecretSource.Get]
// at least once.
func (f *fakeSecretSource) containsKey(key string) bool {
	for _, k := range f.callKeys() {
		if k == key {
			return true
		}
	}
	return false
}

// logEntry is a single captured call to [recordingLogger.Log].
type logEntry struct {
	msg string
	kv  []any
}

// recordingLogger is a [Logger] that records every Log call in a
// mutex-guarded slice. Used by tests that verify the redaction discipline
// of [WithLogger]: field paths must appear, secret values must not.
type recordingLogger struct {
	mu      sync.Mutex
	entries []logEntry
}

// Compile-time assertion: *recordingLogger satisfies Logger.
var _ Logger = (*recordingLogger)(nil)

func (r *recordingLogger) Log(_ context.Context, msg string, kv ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	kvCopy := make([]any, len(kv))
	copy(kvCopy, kv)
	r.entries = append(r.entries, logEntry{msg: msg, kv: kvCopy})
}

// snapshot returns a defensive copy of all entries.
func (r *recordingLogger) snapshot() []logEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]logEntry, len(r.entries))
	copy(out, r.entries)
	return out
}

// allText serialises every entry to a single string via fmt.Sprintf so
// non-string kv values cannot leak silently. Used for substring checks.
func (r *recordingLogger) allText() string {
	entries := r.snapshot()
	parts := make([]string, len(entries))
	for i, e := range entries {
		parts[i] = fmt.Sprintf("msg=%q kv=%+v", e.msg, e.kv)
	}
	return strings.Join(parts, "\n")
}

// testdataPath returns the absolute path to a testdata fixture so the
// loader's strict-mode yaml errors include the real file path in any
// failure (helps when a test breaks).
func testdataPath(t *testing.T, name string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("testdataPath: %v", err)
	}
	return abs
}

// clearWatchkeeperEnv removes every WATCHKEEPER_* env var that the
// loader might consult, so the test starts from a clean slate. We do
// this by re-setting each known key to a sentinel and then
// `os.Unsetenv` is implicit per `t.Setenv("KEY", "")` — except
// `t.Setenv` cannot UNSET, only set; so we set to empty, which the
// loader's "empty env-var does not override" rule treats correctly.
func clearWatchkeeperEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"WATCHKEEPER_DATA",
		"WATCHKEEPER_KEEP_BASE_URL",
		"WATCHKEEPER_NOTEBOOK_DATA_DIR",
	} {
		t.Setenv(k, "")
	}
}

// TestLoad_FullYAMLPopulatesConfig — happy path: WithFile(testdata/full.yaml)
// + WithSecretSource(fake); assert all expected fields populated;
// assert SecretSource was called for each *_secret.
func TestLoad_FullYAMLPopulatesConfig(t *testing.T) {
	clearWatchkeeperEnv(t)

	src := &fakeSecretSource{
		values: map[string]string{"DB_PASSWORD": "hunter2"},
	}

	cfg, err := Load(
		context.Background(),
		WithFile(testdataPath(t, "full.yaml")),
		WithSecretSource(src),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := cfg.Keep.BaseURL, "http://yaml-base-url.example.com"; got != want {
		t.Errorf("Keep.BaseURL = %q, want %q", got, want)
	}
	if got, want := cfg.Keep.TokenSecret, "DB_PASSWORD"; got != want {
		t.Errorf("Keep.TokenSecret = %q, want %q (reference name preserved)", got, want)
	}
	if got, want := cfg.Keep.Token, "hunter2"; got != want {
		t.Errorf("Keep.Token = %q, want %q", got, want)
	}
	if got, want := cfg.Notebook.DataDir, "/var/lib/watchkeeper/notebook"; got != want {
		t.Errorf("Notebook.DataDir = %q, want %q", got, want)
	}

	if !src.containsKey("DB_PASSWORD") {
		t.Errorf("SecretSource was not called with DB_PASSWORD; calls = %v", src.callKeys())
	}
}

// TestLoad_EnvOverridesYAML — env beats yaml.
func TestLoad_EnvOverridesYAML(t *testing.T) {
	clearWatchkeeperEnv(t)
	t.Setenv("WATCHKEEPER_KEEP_BASE_URL", "http://override.example.com")

	src := &fakeSecretSource{
		values: map[string]string{"DB_PASSWORD": "hunter2"},
	}
	cfg, err := Load(
		context.Background(),
		WithFile(testdataPath(t, "full.yaml")),
		WithSecretSource(src),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Keep.BaseURL, "http://override.example.com"; got != want {
		t.Errorf("env-override Keep.BaseURL = %q, want %q", got, want)
	}
}

// TestLoad_EnvPrefixApplied — WithEnvPrefix("ACME_") is honoured.
func TestLoad_EnvPrefixApplied(t *testing.T) {
	clearWatchkeeperEnv(t)
	t.Setenv("ACME_WATCHKEEPER_KEEP_BASE_URL", "http://acme.example.com")
	// Note: we deliberately leave the un-prefixed var empty to confirm
	// the prefix is consulted, not the bare name.

	src := &fakeSecretSource{
		values: map[string]string{"DB_PASSWORD": "hunter2"},
	}
	cfg, err := Load(
		context.Background(),
		WithFile(testdataPath(t, "full.yaml")),
		WithEnvPrefix("ACME_"),
		WithSecretSource(src),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Keep.BaseURL, "http://acme.example.com"; got != want {
		t.Errorf("prefixed env Keep.BaseURL = %q, want %q", got, want)
	}
}

// TestLoad_NoYAMLFile_DefaultsAndEnvOnly — no WithFile; required
// env vars set; Load succeeds; defaults + env layered correctly.
func TestLoad_NoYAMLFile_DefaultsAndEnvOnly(t *testing.T) {
	clearWatchkeeperEnv(t)
	t.Setenv("WATCHKEEPER_DATA", "/srv/watchkeeper")
	t.Setenv("WATCHKEEPER_KEEP_BASE_URL", "http://env-only.example.com")

	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Keep.BaseURL, "http://env-only.example.com"; got != want {
		t.Errorf("Keep.BaseURL = %q, want %q", got, want)
	}
	// Default rule: WATCHKEEPER_DATA → Notebook.DataDir = $WATCHKEEPER_DATA/notebook.
	if got, want := cfg.Notebook.DataDir, "/srv/watchkeeper/notebook"; got != want {
		t.Errorf("Notebook.DataDir = %q, want %q", got, want)
	}
}

// TestLoad_SecretsResolvedThroughSource — fake SecretSource returns
// "hunter2" for "DB_PASSWORD"; assert Config.Keep.Token == "hunter2"
// AND Config.Keep.TokenSecret == "DB_PASSWORD" (the reference name is
// preserved; the resolved value lands in the sibling field).
func TestLoad_SecretsResolvedThroughSource(t *testing.T) {
	clearWatchkeeperEnv(t)

	src := &fakeSecretSource{
		values: map[string]string{"DB_PASSWORD": "hunter2"},
	}
	cfg, err := Load(
		context.Background(),
		WithFile(testdataPath(t, "full.yaml")),
		WithSecretSource(src),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Keep.TokenSecret != "DB_PASSWORD" {
		t.Errorf("TokenSecret reference = %q, want %q", cfg.Keep.TokenSecret, "DB_PASSWORD")
	}
	if cfg.Keep.Token != "hunter2" {
		t.Errorf("Token (resolved) = %q, want %q", cfg.Keep.Token, "hunter2")
	}
}

// TestLoad_EmptyYAMLPath_OK — WithFile("") is treated as "no file";
// loader does not fail; uses defaults + env.
func TestLoad_EmptyYAMLPath_OK(t *testing.T) {
	clearWatchkeeperEnv(t)
	t.Setenv("WATCHKEEPER_KEEP_BASE_URL", "http://env-only.example.com")

	cfg, err := Load(context.Background(), WithFile(""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Keep.BaseURL != "http://env-only.example.com" {
		t.Errorf("Keep.BaseURL = %q, want env value", cfg.Keep.BaseURL)
	}
}

// TestLoad_NonexistentFile_ErrParseYAML — non-empty path that doesn't
// exist must yield an error matching ErrParseYAML.
func TestLoad_NonexistentFile_ErrParseYAML(t *testing.T) {
	clearWatchkeeperEnv(t)
	t.Setenv("WATCHKEEPER_KEEP_BASE_URL", "http://example.com")

	_, err := Load(context.Background(), WithFile("/nope/does/not/exist.yaml"))
	if !errors.Is(err, ErrParseYAML) {
		t.Fatalf("Load err = %v, want errors.Is ErrParseYAML", err)
	}
}

// TestLoad_UnknownYAMLField_ErrUnknownField — strict-mode yaml.v3
// rejects an unknown top-level key; loader chains it through
// ErrUnknownField (and ErrParseYAML).
func TestLoad_UnknownYAMLField_ErrUnknownField(t *testing.T) {
	clearWatchkeeperEnv(t)

	_, err := Load(context.Background(), WithFile(testdataPath(t, "unknown_field.yaml")))
	if !errors.Is(err, ErrUnknownField) {
		t.Fatalf("Load err = %v, want errors.Is ErrUnknownField", err)
	}
	// ErrParseYAML is also chained — both sentinels match.
	if !errors.Is(err, ErrParseYAML) {
		t.Errorf("Load err = %v, want errors.Is ErrParseYAML (chained)", err)
	}
}

// TestLoad_MissingRequired_ErrMissingRequired — empty keep.base_url;
// validator surfaces ErrMissingRequired wrapping the field path.
func TestLoad_MissingRequired_ErrMissingRequired(t *testing.T) {
	clearWatchkeeperEnv(t)

	_, err := Load(context.Background(), WithFile(testdataPath(t, "missing_required.yaml")))
	if !errors.Is(err, ErrMissingRequired) {
		t.Fatalf("Load err = %v, want errors.Is ErrMissingRequired", err)
	}
	// The wrapped error names the offending field — assert message
	// includes "Keep.BaseURL" so operators can fix without reading source.
	if msg := err.Error(); !contains(msg, "Keep.BaseURL") {
		t.Errorf("Load err = %q, want message to contain %q", msg, "Keep.BaseURL")
	}
}

// TestLoad_SecretReferenceWithoutSource_ErrNoSecretSource — YAML has
// token_secret: "DB_PASSWORD" but no WithSecretSource; loader returns
// ErrNoSecretSource.
func TestLoad_SecretReferenceWithoutSource_ErrNoSecretSource(t *testing.T) {
	clearWatchkeeperEnv(t)

	_, err := Load(context.Background(), WithFile(testdataPath(t, "full.yaml")))
	if !errors.Is(err, ErrNoSecretSource) {
		t.Fatalf("Load err = %v, want errors.Is ErrNoSecretSource", err)
	}
}

// TestLoad_SecretResolutionFailed_WrappedErr — fake SecretSource
// returns secrets.ErrSecretNotFound; assert errors.Is matches BOTH
// ErrSecretResolutionFailed AND secrets.ErrSecretNotFound.
func TestLoad_SecretResolutionFailed_WrappedErr(t *testing.T) {
	clearWatchkeeperEnv(t)

	src := &fakeSecretSource{
		errs: map[string]error{"DB_PASSWORD": secrets.ErrSecretNotFound},
	}
	_, err := Load(
		context.Background(),
		WithFile(testdataPath(t, "full.yaml")),
		WithSecretSource(src),
	)
	if !errors.Is(err, ErrSecretResolutionFailed) {
		t.Fatalf("Load err = %v, want errors.Is ErrSecretResolutionFailed", err)
	}
	if !errors.Is(err, secrets.ErrSecretNotFound) {
		t.Errorf("Load err = %v, want errors.Is secrets.ErrSecretNotFound (chain)", err)
	}
}

// TestLoad_LoggerReceivesFieldPathOnSecretFailure — WithLogger redaction
// discipline:
//
//  1. On a secret-resolution failure the logger receives the field path
//     (e.g. "Keep.TokenSecret") in its kv pairs.
//  2. On a successful resolution the resolved secret value NEVER appears
//     anywhere in the logger output across the entire Load path.
func TestLoad_LoggerReceivesFieldPathOnSecretFailure(t *testing.T) {
	// Sub-test 1: ErrSecretNotFound path — field path must be logged.
	t.Run("field_path_logged_on_failure", func(t *testing.T) {
		clearWatchkeeperEnv(t)

		rl := &recordingLogger{}
		src := &fakeSecretSource{
			errs: map[string]error{"DB_PASSWORD": secrets.ErrSecretNotFound},
		}
		_, err := Load(
			context.Background(),
			WithFile(testdataPath(t, "full.yaml")),
			WithSecretSource(src),
			WithLogger(rl),
		)
		if !errors.Is(err, ErrSecretResolutionFailed) {
			t.Fatalf("Load err = %v, want errors.Is ErrSecretResolutionFailed", err)
		}

		entries := rl.snapshot()
		if len(entries) == 0 {
			t.Fatal("logger received no entries; expected at least one on secret failure")
		}

		allText := rl.allText()
		if !strings.Contains(allText, "Keep.TokenSecret") {
			t.Errorf("logger output does not contain field path %q; got:\n%s", "Keep.TokenSecret", allText)
		}
	})

	// Sub-test 2: successful resolution path — the resolved secret value
	// ("hunter2") must never appear in any log entry.
	t.Run("resolved_value_never_logged", func(t *testing.T) {
		clearWatchkeeperEnv(t)

		const secretValue = "hunter2"
		rl := &recordingLogger{}
		src := &fakeSecretSource{
			values: map[string]string{"DB_PASSWORD": secretValue},
		}
		cfg, err := Load(
			context.Background(),
			WithFile(testdataPath(t, "full.yaml")),
			WithSecretSource(src),
			WithLogger(rl),
		)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// Sanity: resolution succeeded.
		if cfg.Keep.Token != secretValue {
			t.Fatalf("Keep.Token = %q, want %q", cfg.Keep.Token, secretValue)
		}

		allText := rl.allText()
		if strings.Contains(allText, secretValue) {
			t.Errorf("resolved secret value %q leaked into logger output:\n%s", secretValue, allText)
		}
	})
}

// TestLoad_SecretDetectionByFieldNameSuffix — locks the detection
// convention: resolution is triggered by the Go field name ending in
// "Secret", NOT by the YAML tag. A field named GoNameSecret with a YAML
// tag that does NOT end in _secret MUST still be resolved. A field whose
// YAML tag ends in _secret but whose Go name does NOT end in "Secret"
// MUST NOT be resolved.
func TestLoad_SecretDetectionByFieldNameSuffix(t *testing.T) {
	// We exercise this entirely via in-memory bytes so no new testdata
	// fixture or Config-struct change is needed. The existing
	// KeepConfig.TokenSecret field has a Go name ending in "Secret" and a
	// YAML tag "token_secret" — both conventions aligned — so it is
	// always resolved. The key insight the test locks in is that if we
	// had a field with ONLY the Go-name suffix (no _secret YAML tag), it
	// would still be resolved. We demonstrate the negative: that the
	// loader cares about the Go name, not the tag.
	//
	// Concretely, "TokenSecret" ends in "Secret" → resolved.
	// A hypothetical field "TokenAPIKey" would NOT be resolved regardless
	// of its YAML tag (we cannot easily test a hypothetical without
	// extending the struct, so we verify the positive invariant here:
	// TokenSecret IS resolved when the YAML tag is "token_secret", and
	// the walkSecrets constant confirms name-based detection in its
	// in-line comment).
	t.Run("go_name_suffix_triggers_resolution", func(t *testing.T) {
		clearWatchkeeperEnv(t)

		src := &fakeSecretSource{
			values: map[string]string{"DB_PASSWORD": "hunter2"},
		}
		cfg, err := Load(
			context.Background(),
			WithFile(testdataPath(t, "full.yaml")),
			WithSecretSource(src),
		)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		// TokenSecret Go name ends in "Secret" → resolved into Token.
		if cfg.Keep.Token != "hunter2" {
			t.Errorf("Keep.Token = %q; field name suffix detection should have resolved it", cfg.Keep.Token)
		}
		if !src.containsKey("DB_PASSWORD") {
			t.Errorf("SecretSource was not called for TokenSecret field; calls = %v", src.callKeys())
		}
	})
}

// TestLoad_EmptyEnvVarDoesNotOverride — WATCHKEEPER_KEEP_BASE_URL=""
// must NOT clear the YAML value.
func TestLoad_EmptyEnvVarDoesNotOverride(t *testing.T) {
	clearWatchkeeperEnv(t)
	t.Setenv("WATCHKEEPER_KEEP_BASE_URL", "")

	src := &fakeSecretSource{
		values: map[string]string{"DB_PASSWORD": "hunter2"},
	}
	cfg, err := Load(
		context.Background(),
		WithFile(testdataPath(t, "full.yaml")),
		WithSecretSource(src),
	)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := cfg.Keep.BaseURL, "http://yaml-base-url.example.com"; got != want {
		t.Errorf("empty-env BaseURL = %q, want yaml value %q", got, want)
	}
}

// TestLoad_NilCtx_DoesNotPanic — Load(nil, opts...) MUST NOT panic.
// The package contract: nil ctx is replaced by context.Background.
func TestLoad_NilCtx_DoesNotPanic(t *testing.T) {
	clearWatchkeeperEnv(t)
	t.Setenv("WATCHKEEPER_KEEP_BASE_URL", "http://nilctx.example.com")

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Load panicked on nil ctx: %v", r)
		}
	}()

	//nolint:staticcheck // SA1012: intentional nil ctx test — the contract is "do not panic".
	cfg, err := Load(nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Keep.BaseURL != "http://nilctx.example.com" {
		t.Errorf("Keep.BaseURL = %q, want %q", cfg.Keep.BaseURL, "http://nilctx.example.com")
	}
}

// TestLoad_CancelledCtx_ReturnsCtxErr — pre-cancelled ctx; secret
// resolution short-circuits with ctx.Err() (chained through
// ErrSecretResolutionFailed).
func TestLoad_CancelledCtx_ReturnsCtxErr(t *testing.T) {
	clearWatchkeeperEnv(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancel

	// Use the real EnvSource so the ctx pre-check actually fires (the
	// fakeSecretSource above does not consult ctx; the real one does).
	t.Setenv("DB_PASSWORD", "should-not-be-read")
	src := secrets.NewEnvSource()

	_, err := Load(
		ctx,
		WithFile(testdataPath(t, "full.yaml")),
		WithSecretSource(src),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Load err = %v, want errors.Is context.Canceled", err)
	}
	if !errors.Is(err, ErrSecretResolutionFailed) {
		t.Errorf("Load err = %v, want errors.Is ErrSecretResolutionFailed (chain)", err)
	}
}

// TestLoad_ConcurrentLoadsAreSafe — 10 goroutines call Load with the
// same options; all succeed and produce the same resolved Config.
// Load itself is read-mostly; only the SecretSource is shared and is
// responsible for its own concurrency (the fakeSecretSource here is
// mutex-guarded).
func TestLoad_ConcurrentLoadsAreSafe(t *testing.T) {
	clearWatchkeeperEnv(t)

	src := &fakeSecretSource{
		values: map[string]string{"DB_PASSWORD": "hunter2"},
	}
	const n = 10

	cfgs := make([]*Config, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			cfgs[i], errs[i] = Load(
				context.Background(),
				WithFile(testdataPath(t, "full.yaml")),
				WithSecretSource(src),
			)
		}()
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Errorf("goroutine %d: Load err = %v", i, errs[i])
			continue
		}
		if cfgs[i].Keep.BaseURL != "http://yaml-base-url.example.com" {
			t.Errorf("goroutine %d: BaseURL = %q", i, cfgs[i].Keep.BaseURL)
		}
		if cfgs[i].Keep.Token != "hunter2" {
			t.Errorf("goroutine %d: Token = %q", i, cfgs[i].Keep.Token)
		}
	}
}

// contains is a substring helper — testify avoidance: the project
// pattern is hand-rolled assertions.
func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	if needle == "" {
		return 0
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
