package toolregistry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ----- M1 — DecodeManifest must reject raw-junk trailing input -----

func TestDecodeManifest_RejectsRawJunkTrailingBytes(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"trailing-garbage": `{"name":"x","version":"1","capabilities":["c"],"schema":{}} garbage`,
		"trailing-open":    `{"name":"x","version":"1","capabilities":["c"],"schema":{}} [`,
		"trailing-comma":   `{"name":"x","version":"1","capabilities":["c"],"schema":{}},`,
	}
	for label, raw := range cases {
		_, err := DecodeManifest([]byte(raw))
		if !errors.Is(err, ErrManifestParse) {
			t.Errorf("%s: expected ErrManifestParse, got %v", label, err)
		}
	}
}

// ----- M2 — local / hosted MUST NOT create the directory -----

func TestSyncOnce_LocalKindFailsOnMissingDir(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "local-1", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	s, fakeFs, _, _, pub := newTestScheduler(t, sources)
	// fakeFs has no entry for `/data/tools/local-1` — Stat returns
	// fs.ErrNotExist. The scheduler must NOT MkdirAll and must NOT
	// emit source_synced.
	err := s.SyncOnce(context.Background(), "local-1")
	if !errors.Is(err, ErrLocalSourceMissing) {
		t.Fatalf("expected ErrLocalSourceMissing, got %v", err)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected chain through fs.ErrNotExist, got %v", err)
	}
	if fakeFs.mkdirCalls != 0 {
		t.Errorf("MkdirAll calls: got %d, want 0 (local kind must not create dirs)", fakeFs.mkdirCalls)
	}
	if len(pub.eventsForTopic(TopicSourceSynced)) != 0 {
		t.Errorf("source_synced must not fire for missing local dir")
	}
	failed := pub.eventsForTopic(TopicSourceFailed)
	if len(failed) != 1 || failed[0].event.(SourceFailed).Phase != "stat" {
		t.Fatalf("expected source_failed phase=stat, got %+v", failed)
	}
}

func TestSyncOnce_HostedKindFailsOnMissingDir(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "hosted-1", Kind: SourceKindHosted, PullPolicy: PullPolicyOnDemand},
	}
	s, _, _, _, pub := newTestScheduler(t, sources)
	err := s.SyncOnce(context.Background(), "hosted-1")
	if !errors.Is(err, ErrLocalSourceMissing) {
		t.Fatalf("expected ErrLocalSourceMissing, got %v", err)
	}
	if len(pub.eventsForTopic(TopicSourceSynced)) != 0 {
		t.Errorf("source_synced must not fire for missing hosted dir")
	}
}

func TestSyncOnce_LocalKindSucceedsWhenDirPresent(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "local-1", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	s, fakeFs, _, _, pub := newTestScheduler(t, sources)
	fakeFs.dirs[filepath.Join("/data", "tools", "local-1")] = true

	if err := s.SyncOnce(context.Background(), "local-1"); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if fakeFs.mkdirCalls != 0 {
		t.Errorf("MkdirAll calls: got %d, want 0 (local kind must not create dirs)", fakeFs.mkdirCalls)
	}
	if len(pub.eventsForTopic(TopicSourceSynced)) != 1 {
		t.Errorf("expected source_synced for present local dir")
	}
}

func TestSyncOnce_LocalKindStatErrorOtherThanNotExist(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "local-1", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	s, fakeFs, _, _, pub := newTestScheduler(t, sources)
	fakeFs.statErr[filepath.Join("/data", "tools", "local-1")] = errSentinel

	err := s.SyncOnce(context.Background(), "local-1")
	if !errors.Is(err, ErrFSStat) {
		t.Fatalf("expected ErrFSStat (not ErrLocalSourceMissing) for non-not-exist stat error, got %v", err)
	}
	failed := pub.eventsForTopic(TopicSourceFailed)
	if len(failed) != 1 || failed[0].event.(SourceFailed).Phase != "stat" {
		t.Fatalf("expected source_failed phase=stat, got %+v", failed)
	}
}

// ----- M3 — Empty resolved auth credential must surface as auth failure -----

func TestSyncOnce_EmptyResolvedAuthIsAuthFailure(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot, AuthSecret: "SOME_REF"},
	}
	resolver := AuthSecretResolver(func(_ context.Context, _, _ string) (string, error) {
		return "", nil // <- bug: resolver returns empty string for non-empty ref
	})
	s, _, git, _, pub := newTestScheduler(t, sources, WithAuthSecretResolver(resolver))

	err := s.SyncOnce(context.Background(), "x")
	if !errors.Is(err, ErrAuthResolution) {
		t.Fatalf("expected ErrAuthResolution, got %v", err)
	}
	if !errors.Is(err, ErrEmptyResolvedAuth) {
		t.Errorf("expected chain through ErrEmptyResolvedAuth, got %v", err)
	}
	if git.numCloneCalls() != 0 {
		t.Errorf("git Clone must not run with empty resolved auth (got %d calls)", git.numCloneCalls())
	}
	failed := pub.eventsForTopic(TopicSourceFailed)
	if len(failed) != 1 || failed[0].event.(SourceFailed).Phase != "auth" {
		t.Fatalf("expected source_failed phase=auth, got %+v", failed)
	}
}

// ----- M4 — Source-name validation rejects path-traversal -----

func TestSourceConfig_Validate_RejectsPathTraversal(t *testing.T) {
	t.Parallel()
	cases := []string{
		"..",
		"../..",
		"../../etc",
		"/absolute/path",
		"./relative",
		"name/with/slash",
		"name\\with\\backslash",
		"name:with:colon",
		"name.with.dot",
		"   ", // already covered by trim-space check, but explicit
		"",    // already covered, but explicit
	}
	for _, name := range cases {
		sc := SourceConfig{Name: name, Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot}
		err := sc.Validate()
		if !errors.Is(err, ErrInvalidSourceName) {
			t.Errorf("Validate(%q): expected ErrInvalidSourceName, got %v", name, err)
		}
	}
}

func TestSourceConfig_Validate_AllowsValidIdentifiers(t *testing.T) {
	t.Parallel()
	cases := []string{
		"platform",
		"private-tools",
		"platform_v2",
		"a",
		"x-1",
		"X_Y_Z_123",
	}
	for _, name := range cases {
		sc := SourceConfig{Name: name, Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot}
		if err := sc.Validate(); err != nil {
			t.Errorf("Validate(%q): unexpected err: %v", name, err)
		}
	}
}

func TestSyncOnce_DefenseInDepthRefusesUnsafePath(t *testing.T) {
	t.Parallel()
	// Direct construction of a Scheduler with an unsafe source name
	// bypasses ValidateSources. The defense-in-depth check inside
	// SyncOnce must catch the traversal even when the validator
	// would have caught it earlier in a normal config flow.
	deps := Deps{
		FS:        newFakeFS(),
		Git:       &fakeGit{},
		Clock:     newFakeClock(zeroTime),
		Publisher: &fakePublisher{},
		DataDir:   "/data",
	}
	s := &Scheduler{
		deps: deps,
		sources: []SourceConfig{
			{Name: "../../etc/passwd", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
		},
		bySource: map[string]SourceConfig{
			"../../etc/passwd": {Name: "../../etc/passwd", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
		},
		perSourceMu: map[string]*sync.Mutex{"../../etc/passwd": {}},
		verifier:    NoopSignatureVerifier{},
	}
	err := s.SyncOnce(context.Background(), "../../etc/passwd")
	if !errors.Is(err, ErrUnsafeLocalPath) {
		t.Fatalf("expected ErrUnsafeLocalPath, got %v", err)
	}
}

// ----- M5 — Publisher error on success surfaces; on failure stays suppressed -----
// (Covered by TestSyncOnce_PublisherErrorOnSuccessIsSurfaced and
// TestSyncOnce_PublisherErrorOnFailureDoesNotMaskOriginalError in
// scheduler_test.go after the iter-1 contract update.)

// ----- M6 — Lesson + config doc-comment references are accurate -----

func TestNoStaleJiraResolverReference(t *testing.T) {
	t.Parallel()
	productionFiles := []string{
		"config.go",
		"scheduler.go",
		"doc.go",
		"manifest.go",
	}
	for _, name := range productionFiles {
		raw, err := readFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(raw)
		// The historical bad reference was `WithJiraBasicAuthResolver`
		// — a name that does not exist in core/pkg/jira.
		if strings.Contains(body, "WithJiraBasicAuthResolver") {
			t.Errorf("%s: stale reference to non-existent WithJiraBasicAuthResolver — use jira.BasicAuthSource or jira.WithBasicAuth", name)
		}
	}
}

// ----- m3 — DataDir whitespace-only is rejected -----

func TestNew_RejectsWhitespaceOnlyDataDir(t *testing.T) {
	t.Parallel()
	cases := []string{"   ", "\t", "\n"}
	for _, in := range cases {
		deps := Deps{
			FS: newFakeFS(), Git: &fakeGit{}, Clock: newFakeClock(zeroTime), Publisher: &fakePublisher{}, DataDir: in,
		}
		_, err := New(deps, nil)
		if !errors.Is(err, ErrInvalidDataDir) {
			t.Errorf("New(DataDir=%q): expected ErrInvalidDataDir, got %v", in, err)
		}
	}
}

// ----- m2 — auth_secret outside git is rejected at validation -----

func TestSourceConfig_Validate_RejectsAuthSecretOutsideGit(t *testing.T) {
	t.Parallel()
	cases := []SourceKind{SourceKindLocal, SourceKindHosted}
	for _, kind := range cases {
		sc := SourceConfig{Name: "x", Kind: kind, PullPolicy: PullPolicyOnBoot, AuthSecret: "TOKEN_REF"}
		err := sc.Validate()
		if !errors.Is(err, ErrAuthSecretNotAllowed) {
			t.Errorf("Validate(kind=%q,auth_secret=set): expected ErrAuthSecretNotAllowed, got %v", kind, err)
		}
	}
}

// ----- m5 — SourceFailed.ErrorType captures the leaf type, not the wrapper -----

type customSentinelErr struct{ msg string }

func (e *customSentinelErr) Error() string { return e.msg }

func TestSourceFailed_ErrorTypeIsLeaf(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	s, _, git, _, pub := newTestScheduler(t, sources)
	leaf := &customSentinelErr{msg: "underlying"}
	git.cloneErr = fmt.Errorf("clone wrap: %w", leaf)

	_ = s.SyncOnce(context.Background(), "x")
	failed := pub.eventsForTopic(TopicSourceFailed)
	if len(failed) != 1 {
		t.Fatalf("expected 1 source_failed, got %d", len(failed))
	}
	ev := failed[0].event.(SourceFailed)
	if !strings.Contains(ev.ErrorType, "customSentinelErr") {
		t.Errorf("ErrorType should reflect the leaf (*customSentinelErr), got %q", ev.ErrorType)
	}
}

func TestLeafErrType_NilSafe(t *testing.T) {
	t.Parallel()
	if got := leafErrType(nil); got != "<nil>" {
		t.Errorf("leafErrType(nil): got %q, want %q", got, "<nil>")
	}
}

// ----- BootstrapOnBoot field decodes from yaml -----

func TestDecodeSourcesYAML_BootstrapOnBoot(t *testing.T) {
	t.Parallel()
	raw := []byte(`
tool_sources:
  - name: private
    kind: git
    url: https://x
    pull_policy: cron
    cron_spec: "0 */15 * * * *"
    bootstrap_on_boot: true
`)
	sources, err := DecodeSourcesYAML(raw)
	if err != nil {
		t.Fatalf("DecodeSourcesYAML: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("len: got %d, want 1", len(sources))
	}
	if !sources[0].BootstrapOnBoot {
		t.Errorf("BootstrapOnBoot: got %v, want true", sources[0].BootstrapOnBoot)
	}
}

// ----- Auth=empty when AuthSecret is empty -----

func TestSyncOnce_PublicSourceClonesWithoutAuth(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "public", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	s, _, git, _, _ := newTestScheduler(t, sources)
	if err := s.SyncOnce(context.Background(), "public"); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	last, _ := git.lastClone()
	if last.Auth != "" {
		t.Errorf("Auth: got %q, want empty (no AuthSecret configured)", last.Auth)
	}
}

// zeroTime is a fixed test clock reference shared by iter-1 tests
// that need a clock but don't care about specific values.
var zeroTime = time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
