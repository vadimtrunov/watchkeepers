package toolregistry

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// canaryAuthToken is the synthetic auth credential injected through
// the [AuthSecretResolver] in PII tests. The harness scans every
// emitted event and every logged kv pair for this exact substring;
// any appearance means the redaction discipline has drifted.
//
//nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.
const canaryAuthToken = "CANARY_PII_AUTH_TOKEN_DO_NOT_LEAK_47ace0"

// canaryAuthRef is the reference NAME that resolves to
// [canaryAuthToken]. The reference is operator-public (it names a
// secret entry, not the value) and CAN appear in events / logs —
// the canary check explicitly skips it.
const canaryAuthRef = "CANARY_AUTH_REF"

func newTestScheduler(t *testing.T, sources []SourceConfig, opts ...Option) (*Scheduler, *fakeFS, *fakeGit, *fakeClock, *fakePublisher) {
	t.Helper()
	fakeFs := newFakeFS()
	fakeGitClient := &fakeGit{}
	fakeClk := newFakeClock(time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC))
	fakePub := &fakePublisher{}
	deps := Deps{
		FS:        fakeFs,
		Git:       fakeGitClient,
		Clock:     fakeClk,
		Publisher: fakePub,
		DataDir:   "/data",
	}
	s, err := New(deps, sources, opts...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, fakeFs, fakeGitClient, fakeClk, fakePub
}

func TestNew_PanicsOnNilFS(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil FS")
		}
		if !strings.Contains(fmt.Sprint(r), "deps.FS") {
			t.Errorf("panic message must mention deps.FS, got %q", r)
		}
	}()
	_, _ = New(Deps{Git: &fakeGit{}, Clock: newFakeClock(time.Now()), Publisher: &fakePublisher{}, DataDir: "/data"}, nil)
}

func TestNew_PanicsOnNilGit(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil Git")
		}
		if !strings.Contains(fmt.Sprint(r), "deps.Git") {
			t.Errorf("panic message must mention deps.Git, got %q", r)
		}
	}()
	_, _ = New(Deps{FS: newFakeFS(), Clock: newFakeClock(time.Now()), Publisher: &fakePublisher{}, DataDir: "/data"}, nil)
}

func TestNew_PanicsOnNilClock(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil Clock")
		}
		if !strings.Contains(fmt.Sprint(r), "deps.Clock") {
			t.Errorf("panic message must mention deps.Clock, got %q", r)
		}
	}()
	_, _ = New(Deps{FS: newFakeFS(), Git: &fakeGit{}, Publisher: &fakePublisher{}, DataDir: "/data"}, nil)
}

func TestNew_PanicsOnNilPublisher(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on nil Publisher")
		}
		if !strings.Contains(fmt.Sprint(r), "deps.Publisher") {
			t.Errorf("panic message must mention deps.Publisher, got %q", r)
		}
	}()
	_, _ = New(Deps{FS: newFakeFS(), Git: &fakeGit{}, Clock: newFakeClock(time.Now()), DataDir: "/data"}, nil)
}

func TestNew_EmptyDataDir(t *testing.T) {
	t.Parallel()
	deps := Deps{FS: newFakeFS(), Git: &fakeGit{}, Clock: newFakeClock(time.Now()), Publisher: &fakePublisher{}}
	_, err := New(deps, nil)
	if !errors.Is(err, ErrInvalidDataDir) {
		t.Fatalf("expected ErrInvalidDataDir, got %v", err)
	}
}

func TestNew_PropagatesSourceValidation(t *testing.T) {
	t.Parallel()
	deps := Deps{FS: newFakeFS(), Git: &fakeGit{}, Clock: newFakeClock(time.Now()), Publisher: &fakePublisher{}, DataDir: "/data"}
	_, err := New(deps, []SourceConfig{{Name: ""}})
	if !errors.Is(err, ErrInvalidSourceName) {
		t.Fatalf("expected ErrInvalidSourceName, got %v", err)
	}
}

func TestNew_DefensiveCopyOfSources(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "platform", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	s, _, _, _, _ := newTestScheduler(t, sources)
	sources[0].Name = "MUTATED"
	got := s.Sources()
	if got[0].Name != "platform" {
		t.Errorf("post-mutation Sources()[0].Name: got %q, want %q", got[0].Name, "platform")
	}
}

func TestSyncOnce_UnknownSource(t *testing.T) {
	t.Parallel()
	s, _, _, _, pub := newTestScheduler(t, nil)
	err := s.SyncOnce(context.Background(), "ghost")
	if !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("expected ErrUnknownSource, got %v", err)
	}
	if len(pub.snapshot()) != 0 {
		t.Errorf("expected no events on unknown source, got %d", len(pub.snapshot()))
	}
}

func TestSyncOnce_GitCloneFirstTime(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "platform", Kind: SourceKindGit, URL: "https://example.com/platform", Branch: "main", PullPolicy: PullPolicyOnBoot},
	}
	s, _, git, _, pub := newTestScheduler(t, sources)
	if err := s.SyncOnce(context.Background(), "platform"); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if git.numCloneCalls() != 1 {
		t.Errorf("clone calls: got %d, want 1", git.numCloneCalls())
	}
	if git.numPullCalls() != 0 {
		t.Errorf("pull calls: got %d, want 0", git.numPullCalls())
	}
	last, _ := git.lastClone()
	if last.URL != "https://example.com/platform" {
		t.Errorf("clone URL: got %q", last.URL)
	}
	if last.Branch != "main" {
		t.Errorf("clone Branch: got %q", last.Branch)
	}
	if last.Dir != filepath.Join("/data", "tools", "platform") {
		t.Errorf("clone Dir: got %q", last.Dir)
	}
	events := pub.eventsForTopic(TopicSourceSynced)
	if len(events) != 1 {
		t.Fatalf("source_synced events: got %d, want 1", len(events))
	}
	ev := events[0].event.(SourceSynced)
	if ev.SourceName != "platform" {
		t.Errorf("ev.SourceName: got %q", ev.SourceName)
	}
	if ev.LocalPath != filepath.Join("/data", "tools", "platform") {
		t.Errorf("ev.LocalPath: got %q", ev.LocalPath)
	}
	if ev.CorrelationID == "" {
		t.Errorf("ev.CorrelationID: empty")
	}
}

func TestSyncOnce_GitPullOnExistingClone(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "platform", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	s, fakeFs, git, _, pub := newTestScheduler(t, sources)
	// Pre-populate `.git` so the second SyncOnce takes the pull path.
	fakeFs.dirs[filepath.Join("/data", "tools", "platform", ".git")] = true

	if err := s.SyncOnce(context.Background(), "platform"); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if git.numCloneCalls() != 0 {
		t.Errorf("clone calls: got %d, want 0 (existing clone should pull)", git.numCloneCalls())
	}
	if git.numPullCalls() != 1 {
		t.Errorf("pull calls: got %d, want 1", git.numPullCalls())
	}
	last, _ := git.lastPull()
	if last.Dir != filepath.Join("/data", "tools", "platform") {
		t.Errorf("pull Dir: got %q", last.Dir)
	}
	if len(pub.eventsForTopic(TopicSourceSynced)) != 1 {
		t.Errorf("source_synced count: %d", len(pub.eventsForTopic(TopicSourceSynced)))
	}
}

func TestSyncOnce_DefaultBranchMainOnEmptyBranchField(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot}, // empty Branch
	}
	s, _, git, _, _ := newTestScheduler(t, sources)
	if err := s.SyncOnce(context.Background(), "x"); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	last, _ := git.lastClone()
	if last.Branch != "main" {
		t.Errorf("default branch: got %q, want %q", last.Branch, "main")
	}
}

func TestSyncOnce_LocalKindSkipsGit(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "local-1", Kind: SourceKindLocal, PullPolicy: PullPolicyOnBoot},
	}
	s, fakeFs, git, _, pub := newTestScheduler(t, sources)
	// Population is out-of-band (M9.5); pre-populate the directory
	// to model a successfully-installed local source.
	fakeFs.dirs[filepath.Join("/data", "tools", "local-1")] = true
	if err := s.SyncOnce(context.Background(), "local-1"); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if git.numCloneCalls() != 0 || git.numPullCalls() != 0 {
		t.Errorf("local kind must not invoke git (clone=%d, pull=%d)", git.numCloneCalls(), git.numPullCalls())
	}
	if len(pub.eventsForTopic(TopicSourceSynced)) != 1 {
		t.Errorf("expected source_synced for local kind")
	}
}

func TestSyncOnce_HostedKindSkipsGit(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "hosted-1", Kind: SourceKindHosted, PullPolicy: PullPolicyOnDemand},
	}
	s, fakeFs, git, _, pub := newTestScheduler(t, sources)
	// Population is out-of-band (M9.4 hosted-storage pipeline).
	fakeFs.dirs[filepath.Join("/data", "tools", "hosted-1")] = true
	if err := s.SyncOnce(context.Background(), "hosted-1"); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if git.numCloneCalls() != 0 || git.numPullCalls() != 0 {
		t.Errorf("hosted kind must not invoke git")
	}
	if len(pub.eventsForTopic(TopicSourceSynced)) != 1 {
		t.Errorf("expected source_synced for hosted kind")
	}
}

func TestSyncOnce_MkdirError(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	s, fakeFs, _, _, pub := newTestScheduler(t, sources)
	fakeFs.mkdirErr[filepath.Join("/data", "tools", "x")] = errSentinel

	err := s.SyncOnce(context.Background(), "x")
	if !errors.Is(err, ErrFSMkdir) {
		t.Fatalf("expected ErrFSMkdir, got %v", err)
	}
	failed := pub.eventsForTopic(TopicSourceFailed)
	if len(failed) != 1 {
		t.Fatalf("expected 1 source_failed event, got %d", len(failed))
	}
	ev := failed[0].event.(SourceFailed)
	if ev.Phase != "mkdir" {
		t.Errorf("Phase: got %q, want mkdir", ev.Phase)
	}
}

func TestSyncOnce_StatErrorOnGitKind(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	s, fakeFs, _, _, pub := newTestScheduler(t, sources)
	fakeFs.statErr[filepath.Join("/data", "tools", "x", ".git")] = errSentinel

	err := s.SyncOnce(context.Background(), "x")
	if !errors.Is(err, ErrFSStat) {
		t.Fatalf("expected ErrFSStat, got %v", err)
	}
	failed := pub.eventsForTopic(TopicSourceFailed)
	if len(failed) != 1 || failed[0].event.(SourceFailed).Phase != "stat" {
		t.Fatalf("expected source_failed phase=stat, got %+v", failed)
	}
}

func TestSyncOnce_CloneError(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	s, _, git, _, pub := newTestScheduler(t, sources)
	git.cloneErr = errSentinel

	err := s.SyncOnce(context.Background(), "x")
	if !errors.Is(err, ErrSyncClone) {
		t.Fatalf("expected ErrSyncClone, got %v", err)
	}
	failed := pub.eventsForTopic(TopicSourceFailed)
	if len(failed) != 1 || failed[0].event.(SourceFailed).Phase != "clone" {
		t.Fatalf("expected source_failed phase=clone, got %+v", failed)
	}
}

func TestSyncOnce_PullError(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	s, fakeFs, git, _, pub := newTestScheduler(t, sources)
	fakeFs.dirs[filepath.Join("/data", "tools", "x", ".git")] = true
	git.pullErr = errSentinel

	err := s.SyncOnce(context.Background(), "x")
	if !errors.Is(err, ErrSyncPull) {
		t.Fatalf("expected ErrSyncPull, got %v", err)
	}
	failed := pub.eventsForTopic(TopicSourceFailed)
	if len(failed) != 1 || failed[0].event.(SourceFailed).Phase != "pull" {
		t.Fatalf("expected source_failed phase=pull, got %+v", failed)
	}
}

func TestSyncOnce_DefaultSignatureVerifierAccepts(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	s, _, _, _, pub := newTestScheduler(t, sources)
	if err := s.SyncOnce(context.Background(), "x"); err != nil {
		t.Fatalf("default NoopSignatureVerifier should accept: %v", err)
	}
	if len(pub.eventsForTopic(TopicSourceSynced)) != 1 {
		t.Errorf("expected source_synced with default verifier")
	}
}

func TestSyncOnce_SignatureVerifierCalledAndErrorEmitsFailure(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	verifier := &fakeVerifier{verifyErr: errSentinel}
	s, _, _, _, pub := newTestScheduler(t, sources, WithSignatureVerifier(verifier))

	err := s.SyncOnce(context.Background(), "x")
	if !errors.Is(err, ErrSignatureVerification) {
		t.Fatalf("expected ErrSignatureVerification, got %v", err)
	}
	if verifier.numCalls() != 1 {
		t.Errorf("verifier called %d times, want 1", verifier.numCalls())
	}
	failed := pub.eventsForTopic(TopicSourceFailed)
	if len(failed) != 1 || failed[0].event.(SourceFailed).Phase != "signature" {
		t.Fatalf("expected source_failed phase=signature, got %+v", failed)
	}
}

func TestSyncOnce_SignatureVerifierReceivesNameAndDir(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "platform", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	verifier := &fakeVerifier{}
	s, _, _, _, _ := newTestScheduler(t, sources, WithSignatureVerifier(verifier))
	if err := s.SyncOnce(context.Background(), "platform"); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if verifier.numCalls() != 1 {
		t.Fatalf("verifier.numCalls: got %d, want 1", verifier.numCalls())
	}
	verifier.mu.Lock()
	defer verifier.mu.Unlock()
	c := verifier.calls[0]
	if c.sourceName != "platform" {
		t.Errorf("verifier sourceName: got %q", c.sourceName)
	}
	if c.dir != filepath.Join("/data", "tools", "platform") {
		t.Errorf("verifier dir: got %q", c.dir)
	}
}

func TestSyncOnce_AuthResolverPerCall(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot, AuthSecret: canaryAuthRef},
	}
	var calls atomic.Int32
	resolver := AuthSecretResolver(func(_ context.Context, sourceName, ref string) (string, error) {
		calls.Add(1)
		if sourceName != "x" {
			t.Errorf("resolver sourceName: got %q", sourceName)
		}
		if ref != canaryAuthRef {
			t.Errorf("resolver ref: got %q", ref)
		}
		return canaryAuthToken, nil
	})
	s, _, git, _, _ := newTestScheduler(t, sources, WithAuthSecretResolver(resolver))

	if err := s.SyncOnce(context.Background(), "x"); err != nil {
		t.Fatalf("SyncOnce 1: %v", err)
	}
	if err := s.SyncOnce(context.Background(), "x"); err != nil {
		t.Fatalf("SyncOnce 2: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("resolver call count: got %d, want 2 (per-call, not cached)", calls.Load())
	}
	// The resolved canary token MUST reach the GitClient (so a real
	// production clone would have credentials) but MUST NOT leak
	// anywhere else.
	last, _ := git.lastClone()
	if last.Auth != canaryAuthToken {
		t.Errorf("clone Auth: got %q, want %q (resolved token must reach GitClient)", last.Auth, canaryAuthToken)
	}
}

func TestSyncOnce_AuthResolverMissing(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot, AuthSecret: "SOME_REF"},
	}
	s, _, _, _, pub := newTestScheduler(t, sources)

	err := s.SyncOnce(context.Background(), "x")
	if !errors.Is(err, ErrAuthResolution) {
		t.Fatalf("expected ErrAuthResolution, got %v", err)
	}
	failed := pub.eventsForTopic(TopicSourceFailed)
	if len(failed) != 1 || failed[0].event.(SourceFailed).Phase != "auth" {
		t.Fatalf("expected source_failed phase=auth, got %+v", failed)
	}
}

func TestSyncOnce_AuthResolverError(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot, AuthSecret: "SOME_REF"},
	}
	resolver := AuthSecretResolver(func(_ context.Context, _, _ string) (string, error) {
		return "", errSentinel
	})
	s, _, _, _, pub := newTestScheduler(t, sources, WithAuthSecretResolver(resolver))

	err := s.SyncOnce(context.Background(), "x")
	if !errors.Is(err, ErrAuthResolution) {
		t.Fatalf("expected ErrAuthResolution, got %v", err)
	}
	failed := pub.eventsForTopic(TopicSourceFailed)
	if len(failed) != 1 || failed[0].event.(SourceFailed).Phase != "auth" {
		t.Fatalf("expected source_failed phase=auth, got %+v", failed)
	}
}

// PII canary harness: across every failure phase, neither the
// emitted [SourceFailed] event nor any logged kv pair must contain
// the resolved canary auth token. The reference NAME (publicly
// known) is allowed to appear; the resolved VALUE must not.
func TestPIIRedactionCanary_AcrossEveryFailurePath(t *testing.T) {
	t.Parallel()
	type scenario struct {
		label string
		setup func(*fakeFS, *fakeGit, *fakeVerifier)
	}
	scenarios := []scenario{
		{
			label: "mkdir",
			setup: func(fs *fakeFS, _ *fakeGit, _ *fakeVerifier) {
				fs.mkdirErr[filepath.Join("/data", "tools", "x")] = fmt.Errorf("mkdir failed leaked-token=%s", canaryAuthToken)
			},
		},
		{
			label: "stat",
			setup: func(fs *fakeFS, _ *fakeGit, _ *fakeVerifier) {
				fs.statErr[filepath.Join("/data", "tools", "x", ".git")] = fmt.Errorf("stat failed leaked-token=%s", canaryAuthToken)
			},
		},
		{
			label: "clone",
			setup: func(_ *fakeFS, g *fakeGit, _ *fakeVerifier) {
				g.cloneErr = fmt.Errorf("clone failed leaked-token=%s", canaryAuthToken)
			},
		},
		{
			label: "pull",
			setup: func(fs *fakeFS, g *fakeGit, _ *fakeVerifier) {
				fs.dirs[filepath.Join("/data", "tools", "x", ".git")] = true
				g.pullErr = fmt.Errorf("pull failed leaked-token=%s", canaryAuthToken)
			},
		},
		{
			label: "signature",
			setup: func(_ *fakeFS, _ *fakeGit, v *fakeVerifier) {
				v.verifyErr = fmt.Errorf("verify failed leaked-token=%s", canaryAuthToken)
			},
		},
		{
			label: "auth",
			// No external setup needed — the resolver below returns an error.
			setup: func(_ *fakeFS, _ *fakeGit, _ *fakeVerifier) {},
		},
	}

	for _, sc := range scenarios {
		sc := sc
		t.Run(sc.label, func(t *testing.T) {
			t.Parallel()
			sources := []SourceConfig{
				{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot, AuthSecret: canaryAuthRef},
			}
			logger := &fakeLogger{}
			verifier := &fakeVerifier{}
			resolver := AuthSecretResolver(func(_ context.Context, _, _ string) (string, error) {
				if sc.label == "auth" {
					return "", fmt.Errorf("resolver failed leaked-token=%s", canaryAuthToken)
				}
				return canaryAuthToken, nil
			})
			s, fakeFs, fakeGitClient, _, pub := newTestScheduler(
				t, sources,
				WithSignatureVerifier(verifier),
				WithAuthSecretResolver(resolver),
				WithLogger(logger),
			)
			sc.setup(fakeFs, fakeGitClient, verifier)

			_ = s.SyncOnce(context.Background(), "x")

			// Assert: NO event payload contains the canary value.
			for _, ev := range pub.snapshot() {
				if containsCanary(ev.event) {
					t.Errorf("[%s] event leaks canary: %+v", sc.label, ev)
				}
			}
			// Assert: NO logged kv pair contains the canary value.
			for _, e := range logger.snapshot() {
				if strings.Contains(e.msg, canaryAuthToken) {
					t.Errorf("[%s] log msg leaks canary: %q", sc.label, e.msg)
				}
				for _, v := range e.kv {
					if strings.Contains(fmt.Sprint(v), canaryAuthToken) {
						t.Errorf("[%s] log kv leaks canary: %v", sc.label, v)
					}
				}
			}
		})
	}
}

// containsCanary reports whether any string-typed field of `v`
// (walked via fmt.Sprintf("%+v", ...)) contains [canaryAuthToken].
// Sufficient for the published events because they expose all
// fields under the package's `%+v` shape.
func containsCanary(v any) bool {
	return strings.Contains(fmt.Sprintf("%+v", v), canaryAuthToken)
}

func TestSyncOnce_ConcurrencyDifferentSources(t *testing.T) {
	t.Parallel()
	sources := make([]SourceConfig, 16)
	for i := 0; i < 16; i++ {
		sources[i] = SourceConfig{
			Name:       fmt.Sprintf("src-%02d", i),
			Kind:       SourceKindGit,
			URL:        fmt.Sprintf("https://x/%d", i),
			PullPolicy: PullPolicyOnDemand,
		}
	}
	s, _, git, _, pub := newTestScheduler(t, sources)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if err := s.SyncOnce(context.Background(), fmt.Sprintf("src-%02d", idx)); err != nil {
				t.Errorf("src-%02d: SyncOnce: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()
	if got := git.numCloneCalls(); got != 16 {
		t.Errorf("clone calls: got %d, want 16", got)
	}
	if got := len(pub.eventsForTopic(TopicSourceSynced)); got != 16 {
		t.Errorf("source_synced events: got %d, want 16", got)
	}
}

func TestSyncOnce_ConcurrencySameSourceSerialised(t *testing.T) {
	t.Parallel()
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnDemand},
	}
	s, _, git, _, _ := newTestScheduler(t, sources)
	// Track concurrent entries to the Git client; if the per-source
	// mutex is broken, two goroutines will be in the critical
	// section at once.
	var inflight atomic.Int32
	var maxInflight atomic.Int32
	git.onClone = func(_ CloneOpts) {
		cur := inflight.Add(1)
		for {
			m := maxInflight.Load()
			if cur <= m || maxInflight.CompareAndSwap(m, cur) {
				break
			}
		}
		// Hold the critical section briefly so concurrent callers
		// have a chance to overlap if the mutex is broken.
		time.Sleep(time.Millisecond)
		inflight.Add(-1)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.SyncOnce(context.Background(), "x")
		}()
	}
	wg.Wait()
	if maxInflight.Load() > 1 {
		t.Errorf("per-source mutex broken: max inflight %d", maxInflight.Load())
	}
}

func TestSyncOnce_PublisherErrorOnSuccessIsSurfaced(t *testing.T) {
	t.Parallel()
	// Per the M5 iter-1 contract: a publisher failure on the success
	// path MUST surface to the caller. M9.1.b learns about changed
	// source contents EXCLUSIVELY through TopicSourceSynced; silently
	// dropping the publish would mask a missed hot-reload. The
	// on-disk state is already committed, so the next SyncOnce will
	// pick up where this one left off — but the caller has to know.
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	s, _, _, _, pub := newTestScheduler(t, sources)
	pub.publishErr = errSentinel

	err := s.SyncOnce(context.Background(), "x")
	if err == nil {
		t.Fatal("publisher errors on success path MUST surface to caller")
	}
	if !errors.Is(err, errSentinel) {
		t.Errorf("expected wrapping of publisher error, got %v", err)
	}
	if !strings.Contains(err.Error(), "publish synced") {
		t.Errorf("error message: got %q, want 'publish synced'-wrapped", err.Error())
	}
}

func TestSyncOnce_PublisherErrorOnFailureDoesNotMaskOriginalError(t *testing.T) {
	t.Parallel()
	// Per the M5 iter-1 contract: a publisher failure on the FAILURE
	// path stays SUPPRESSED. The SyncOnce caller already has the
	// original sync error; translating a publisher hiccup into a
	// different return value would muddle the failure classification.
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	s, _, git, _, pub := newTestScheduler(t, sources)
	git.cloneErr = errSentinel
	pub.publishErr = errors.New("publish failure")

	err := s.SyncOnce(context.Background(), "x")
	if !errors.Is(err, ErrSyncClone) {
		t.Fatalf("expected ErrSyncClone, got %v", err)
	}
	if strings.Contains(err.Error(), "publish") {
		t.Errorf("publisher failure leaked into clone-error return: %q", err.Error())
	}
}

func TestSyncOnce_EventTimestampFromClock(t *testing.T) {
	t.Parallel()
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	sources := []SourceConfig{
		{Name: "x", Kind: SourceKindGit, URL: "https://x", PullPolicy: PullPolicyOnBoot},
	}
	fakeFs := newFakeFS()
	fakeClk := newFakeClock(fixed)
	pub := &fakePublisher{}
	deps := Deps{
		FS:        fakeFs,
		Git:       &fakeGit{},
		Clock:     fakeClk,
		Publisher: pub,
		DataDir:   "/data",
	}
	s, err := New(deps, sources)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := s.SyncOnce(context.Background(), "x"); err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	ev := pub.eventsForTopic(TopicSourceSynced)[0].event.(SourceSynced)
	if !ev.SyncedAt.Equal(fixed) {
		t.Errorf("SyncedAt: got %v, want %v", ev.SyncedAt, fixed)
	}
}

func TestSyncOnce_NoCloneOptsURLLeakInFailureEvent(t *testing.T) {
	t.Parallel()
	// A git URL that embeds credentials (a real-world risk) must
	// never end up in the SourceFailed payload. The scheduler's
	// payload only carries the source NAME + error TYPE — the URL
	// is the operator's secret-bearing field.
	sources := []SourceConfig{
		{
			Name:       "x",
			Kind:       SourceKindGit,
			URL:        "https://user:" + canaryAuthToken + "@example.com/repo",
			PullPolicy: PullPolicyOnBoot,
		},
	}
	s, _, git, _, pub := newTestScheduler(t, sources)
	git.cloneErr = errSentinel

	_ = s.SyncOnce(context.Background(), "x")

	failed := pub.eventsForTopic(TopicSourceFailed)
	if len(failed) != 1 {
		t.Fatalf("expected 1 source_failed, got %d", len(failed))
	}
	if containsCanary(failed[0].event) {
		t.Errorf("source_failed payload leaked URL credentials: %+v", failed[0].event)
	}
}
