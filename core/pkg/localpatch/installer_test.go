package localpatch

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

const (
	testDataDir       = "/data"
	testSourceName    = "local-ops"
	testToolName      = "demo_tool"
	testToolVersion   = "1.0.0"
	testOperatorID    = "operator-alice"
	testReason        = "Hot-fix for incident #4711: missing capability declaration on demo_tool."
	testFolderPath    = "/staging/demo_tool"
	canaryToken       = "wath-canary-001"           //nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.
	canaryAlt         = "wath-canary-alt-002"       //nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.
	canaryFilePath    = "src/canary_marker_zzz.txt" // PII canary path
	canaryFileContent = "wath-canary-content-003"   //nolint:gosec // G101: synthetic redaction-harness canary, not a real credential.
)

// installerFixture wires a fully-populated [Installer] against an
// in-memory FS pre-loaded with a valid tool folder. Helpers below
// expose handles to mutate the fixture per test.
type installerFixture struct {
	fs       *fakeFS
	pub      *fakePublisher
	clk      *fakeClock
	lookup   *constSourceLookup
	resolver *constOperatorResolver
	logger   *fakeLogger
	inst     *Installer
}

func newInstallerFixture(t *testing.T) *installerFixture {
	t.Helper()
	f := newFakeFS()
	f.AddFile(filepath.Join(testFolderPath, "manifest.json"), validManifestJSON(testToolName, testToolVersion))
	f.AddFile(filepath.Join(testFolderPath, "src/index.ts"), []byte(`export const run = () => 0;`))
	pub := &fakePublisher{}
	clk := newFakeClock(time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC))
	lookup := newConstSourceLookup(validLocalSource(testSourceName))
	resolver := &constOperatorResolver{id: testOperatorID}
	logger := &fakeLogger{}
	inst := NewInstaller(InstallerDeps{
		FS:                       f,
		Publisher:                pub,
		Clock:                    clk,
		SourceLookup:             lookup.Lookup,
		OperatorIdentityResolver: resolver.Resolve,
		DataDir:                  testDataDir,
		Logger:                   logger,
	})
	return &installerFixture{
		fs:       f,
		pub:      pub,
		clk:      clk,
		lookup:   lookup,
		resolver: resolver,
		logger:   logger,
		inst:     inst,
	}
}

func validInstallReq() InstallRequest {
	return InstallRequest{
		SourceName:     testSourceName,
		FolderPath:     testFolderPath,
		Reason:         testReason,
		OperatorIDHint: testOperatorID,
	}
}

func TestNewInstaller_NilDepsPanic(t *testing.T) {
	t.Parallel()
	base := InstallerDeps{
		FS:        newFakeFS(),
		Publisher: &fakePublisher{},
		Clock:     newFakeClock(time.Now()),
		SourceLookup: func(context.Context, string) (toolregistry.SourceConfig, error) {
			return validLocalSource(testSourceName), nil
		},
		OperatorIdentityResolver: func(context.Context, string) (string, error) { return testOperatorID, nil },
		DataDir:                  testDataDir,
	}

	cases := []struct {
		name    string
		mutate  func(*InstallerDeps)
		wantSub string
	}{
		{"FS nil", func(d *InstallerDeps) { d.FS = nil }, "deps.FS"},
		{"Publisher nil", func(d *InstallerDeps) { d.Publisher = nil }, "deps.Publisher"},
		{"Clock nil", func(d *InstallerDeps) { d.Clock = nil }, "deps.Clock"},
		{"SourceLookup nil", func(d *InstallerDeps) { d.SourceLookup = nil }, "deps.SourceLookup"},
		{"OperatorIdentityResolver nil", func(d *InstallerDeps) { d.OperatorIdentityResolver = nil }, "deps.OperatorIdentityResolver"},
		{"DataDir empty", func(d *InstallerDeps) { d.DataDir = "" }, "deps.DataDir"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("expected panic, got nil")
				}
				msg, ok := r.(string)
				if !ok {
					t.Fatalf("expected string panic, got %T: %v", r, r)
				}
				if !strings.Contains(msg, tc.wantSub) {
					t.Errorf("panic msg %q does not contain %q", msg, tc.wantSub)
				}
			}()
			d := base
			tc.mutate(&d)
			_ = NewInstaller(d)
		})
	}
}

// TestInstall_HappyPath_FirstInstall asserts the install pipeline
// end-to-end on a clean fixture (no prior live tree). Many
// independent assertions live in one function to keep the failure
// reporter tight — splitting them into subtests would force every
// subtest to re-bootstrap the fixture.
//
//nolint:gocyclo // Many independent post-install assertions on one fixture.
func TestInstall_HappyPath_FirstInstall(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	ev, err := fx.inst.Install(context.Background(), validInstallReq())
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Live tools dir populated.
	wantManifest := filepath.Join(testDataDir, "tools", testSourceName, testToolName, "manifest.json")
	if !fx.fs.hasFile(wantManifest) {
		t.Fatalf("live manifest missing at %q", wantManifest)
	}
	wantSrc := filepath.Join(testDataDir, "tools", testSourceName, testToolName, "src/index.ts")
	if !fx.fs.hasFile(wantSrc) {
		t.Fatalf("live src missing at %q", wantSrc)
	}

	// No snapshot taken (first install).
	wantSnap := filepath.Join(testDataDir, historyDirName, testSourceName, testToolName, testToolVersion)
	if fx.fs.hasDir(wantSnap) {
		t.Errorf("unexpected snapshot dir on first install: %q", wantSnap)
	}

	// Event payload is well-formed.
	if ev.SourceName != testSourceName || ev.ToolName != testToolName || ev.ToolVersion != testToolVersion {
		t.Errorf("event header mismatch: %+v", ev)
	}
	if ev.OperatorID != testOperatorID {
		t.Errorf("OperatorID: got %q want %q", ev.OperatorID, testOperatorID)
	}
	if ev.Reason != testReason {
		t.Errorf("Reason: got %q want %q", ev.Reason, testReason)
	}
	if ev.Operation != OperationInstall {
		t.Errorf("Operation: got %q want %q", ev.Operation, OperationInstall)
	}
	if len(ev.DiffHash) != 64 {
		t.Errorf("DiffHash should be 64 hex chars, got %d", len(ev.DiffHash))
	}
	if ev.CorrelationID == "" {
		t.Errorf("CorrelationID empty")
	}

	// Bus saw exactly one publish on the right topic.
	events := fx.pub.snapshot()
	if len(events) != 1 {
		t.Fatalf("Publish count: got %d want 1", len(events))
	}
	if events[0].topic != TopicLocalPatchApplied {
		t.Errorf("topic: got %q want %q", events[0].topic, TopicLocalPatchApplied)
	}
	got, ok := events[0].event.(LocalPatchApplied)
	if !ok {
		t.Fatalf("event type: got %T", events[0].event)
	}
	if got != ev {
		t.Errorf("bus payload != returned payload")
	}
}

func TestInstall_HappyPath_OverwritesPriorSnapshotsThePrior(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	// Pre-populate the live tree as a prior version.
	priorManifest := filepath.Join(testDataDir, "tools", testSourceName, testToolName, "manifest.json")
	priorSrc := filepath.Join(testDataDir, "tools", testSourceName, testToolName, "src/old.ts")
	fx.fs.AddFile(priorManifest, validManifestJSON(testToolName, "0.9.0"))
	fx.fs.AddFile(priorSrc, []byte(`export const old = () => "v0.9";`))

	if _, err := fx.inst.Install(context.Background(), validInstallReq()); err != nil {
		t.Fatalf("Install: %v", err)
	}

	// Snapshot of prior is on disk under the prior version.
	wantSnapManifest := filepath.Join(testDataDir, historyDirName, testSourceName, testToolName, "0.9.0", "manifest.json")
	if !fx.fs.hasFile(wantSnapManifest) {
		t.Errorf("snapshot manifest missing: %q", wantSnapManifest)
	}
	wantSnapSrc := filepath.Join(testDataDir, historyDirName, testSourceName, testToolName, "0.9.0", "src/old.ts")
	if !fx.fs.hasFile(wantSnapSrc) {
		t.Errorf("snapshot src missing: %q", wantSnapSrc)
	}

	// Live tree no longer carries the old src file (RemoveAll-then-copy).
	if fx.fs.hasFile(filepath.Join(testDataDir, "tools", testSourceName, testToolName, "src/old.ts")) {
		t.Errorf("stale prior file still present in live tree")
	}
}

func TestInstall_RefusesNonLocalSource(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	fx.lookup = newConstSourceLookup(validGitSource(testSourceName))
	fx.inst = NewInstaller(InstallerDeps{
		FS: fx.fs, Publisher: fx.pub, Clock: fx.clk,
		SourceLookup:             fx.lookup.Lookup,
		OperatorIdentityResolver: fx.resolver.Resolve,
		DataDir:                  testDataDir,
		Logger:                   fx.logger,
	})

	_, err := fx.inst.Install(context.Background(), validInstallReq())
	if !errors.Is(err, ErrInvalidSourceKind) {
		t.Fatalf("err: got %v want ErrInvalidSourceKind", err)
	}
	if len(fx.pub.snapshot()) != 0 {
		t.Errorf("publish fired despite refused source kind")
	}
}

func TestInstall_RefusesUnknownSource(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	fx.lookup = newConstSourceLookup() // no sources
	fx.inst = NewInstaller(InstallerDeps{
		FS: fx.fs, Publisher: fx.pub, Clock: fx.clk,
		SourceLookup:             fx.lookup.Lookup,
		OperatorIdentityResolver: fx.resolver.Resolve,
		DataDir:                  testDataDir,
	})

	_, err := fx.inst.Install(context.Background(), validInstallReq())
	if !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("err: got %v want ErrUnknownSource", err)
	}
}

func TestInstall_ValidateRejectsMissingFields(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*InstallRequest)
		wantSub string
	}{
		{"empty source", func(r *InstallRequest) { r.SourceName = "" }, "source_name"},
		{"empty folder", func(r *InstallRequest) { r.FolderPath = "" }, "folder_path"},
		{"empty reason", func(r *InstallRequest) { r.Reason = "" }, "reason"},
		{"oversized reason", func(r *InstallRequest) { r.Reason = strings.Repeat("x", MaxReasonLength+1) }, "reason has"},
		{"oversized hint", func(r *InstallRequest) { r.OperatorIDHint = strings.Repeat("x", MaxOperatorIDLength+1) }, "operator_id_hint has"},
		{"bad source chars", func(r *InstallRequest) { r.SourceName = "../escape" }, "disallowed characters"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newInstallerFixture(t)
			req := validInstallReq()
			tc.mutate(&req)
			_, err := fx.inst.Install(context.Background(), req)
			if !errors.Is(err, ErrInvalidInstallRequest) {
				t.Fatalf("err: got %v want ErrInvalidInstallRequest", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err msg missing %q: %v", tc.wantSub, err)
			}
		})
	}
}

func TestInstall_OperatorResolverErrorWrapped(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	cause := errors.New("oidc verification failed")
	fx.resolver.err = cause
	_, err := fx.inst.Install(context.Background(), validInstallReq())
	if !errors.Is(err, ErrIdentityResolution) {
		t.Fatalf("err: got %v want ErrIdentityResolution", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("err does not preserve cause: %v", err)
	}
}

func TestInstall_OperatorResolverEmptyRefused(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	fx.resolver.id = ""
	_, err := fx.inst.Install(context.Background(), validInstallReq())
	if !errors.Is(err, ErrEmptyResolvedIdentity) {
		t.Fatalf("err: got %v want ErrEmptyResolvedIdentity", err)
	}
}

func TestInstall_OperatorResolverInvalidCharsRefused(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	fx.resolver.id = "alice; rm -rf /"
	_, err := fx.inst.Install(context.Background(), validInstallReq())
	if !errors.Is(err, ErrInvalidOperatorID) {
		t.Fatalf("err: got %v want ErrInvalidOperatorID", err)
	}
}

func TestInstall_OperatorResolverOverboundRefused(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	fx.resolver.id = strings.Repeat("a", MaxOperatorIDLength+1)
	_, err := fx.inst.Install(context.Background(), validInstallReq())
	if !errors.Is(err, ErrInvalidOperatorID) {
		t.Fatalf("err: got %v want ErrInvalidOperatorID", err)
	}
}

func TestInstall_FolderManifestMissingRefused(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	// Wipe the manifest from the staging folder.
	delete(fx.fs.files, filepath.Clean(filepath.Join(testFolderPath, "manifest.json")))
	delete(fx.fs.fileModes, filepath.Clean(filepath.Join(testFolderPath, "manifest.json")))
	_, err := fx.inst.Install(context.Background(), validInstallReq())
	if !errors.Is(err, ErrManifestRead) {
		t.Fatalf("err: got %v want ErrManifestRead", err)
	}
}

func TestInstall_FolderManifestUnsafeNameRefused(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	bad := validManifestJSON("../escape", testToolVersion)
	fx.fs.AddFile(filepath.Join(testFolderPath, "manifest.json"), bad)
	_, err := fx.inst.Install(context.Background(), validInstallReq())
	if !errors.Is(err, ErrManifestRead) {
		t.Fatalf("err: got %v want ErrManifestRead", err)
	}
}

func TestInstall_PriorTreeUndecidableRefused(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	// Live tree exists with a missing manifest — undecidable.
	livePath := filepath.Join(testDataDir, "tools", testSourceName, testToolName)
	fx.fs.AddDir(livePath)
	fx.fs.AddFile(filepath.Join(livePath, "src/old.ts"), []byte(`old`))
	_, err := fx.inst.Install(context.Background(), validInstallReq())
	if !errors.Is(err, ErrManifestRead) {
		t.Fatalf("err: got %v want ErrManifestRead (prior undecidable)", err)
	}
	// Live tree must NOT have been overwritten.
	if !fx.fs.hasFile(filepath.Join(livePath, "src/old.ts")) {
		t.Errorf("live tree corrupted before validation completed")
	}
}

func TestInstall_SourceLookupMismatchSurfaced(t *testing.T) {
	t.Parallel()
	// Iter-1 codex M5 fix: a buggy SourceLookup returning a config
	// whose Name disagrees with the request surfaces
	// ErrSourceLookupMismatch (NOT ErrUnknownSource).
	fx := newInstallerFixture(t)
	fx.lookup = newConstSourceLookup(toolregistry.SourceConfig{
		Name:       "OTHER-NAME",
		Kind:       toolregistry.SourceKindLocal,
		PullPolicy: toolregistry.PullPolicyOnDemand,
	})
	// Make the lookup return its config for the REQUEST'S name.
	fx.lookup.configs[testSourceName] = fx.lookup.configs["OTHER-NAME"]
	fx.inst = NewInstaller(InstallerDeps{
		FS: fx.fs, Publisher: fx.pub, Clock: fx.clk,
		SourceLookup:             fx.lookup.Lookup,
		OperatorIdentityResolver: fx.resolver.Resolve,
		DataDir:                  testDataDir,
	})
	_, err := fx.inst.Install(context.Background(), validInstallReq())
	if !errors.Is(err, ErrSourceLookupMismatch) {
		t.Fatalf("err: got %v want ErrSourceLookupMismatch", err)
	}
}

func TestInstall_SameVersionRepeat_PreservesOriginalSnapshot(t *testing.T) {
	t.Parallel()
	// Iter-1 codex M7 fix: same-version repeat install must NOT
	// overwrite an existing snapshot at that version. Pre-stage a
	// snapshot at v1.0.0 with sentinel content; install v1.0.0 over
	// an existing v1.0.0 live tree; assert the snapshot bytes are
	// preserved.
	fx := newInstallerFixture(t)
	const sentinel = "PRESERVED-SNAPSHOT-BYTES"
	snapDir := filepath.Join(testDataDir, historyDirName, testSourceName, testToolName, testToolVersion)
	fx.fs.AddFile(filepath.Join(snapDir, "manifest.json"), validManifestJSON(testToolName, testToolVersion))
	fx.fs.AddFile(filepath.Join(snapDir, "src/index.ts"), []byte(sentinel))
	// Live tree at the same version.
	live := filepath.Join(testDataDir, "tools", testSourceName, testToolName)
	fx.fs.AddFile(filepath.Join(live, "manifest.json"), validManifestJSON(testToolName, testToolVersion))
	fx.fs.AddFile(filepath.Join(live, "src/index.ts"), []byte(`current_live_v1_0`))

	if _, err := fx.inst.Install(context.Background(), validInstallReq()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	got, err := fx.fs.ReadFile(filepath.Join(snapDir, "src/index.ts"))
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if string(got) != sentinel {
		t.Errorf("snapshot overwritten on same-version repeat install: got %q want %q", got, sentinel)
	}
}

func TestInstall_CtxCancelledRefusedBeforeSideEffects(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := fx.inst.Install(ctx, validInstallReq())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err: got %v want context.Canceled", err)
	}
	if len(fx.pub.snapshot()) != 0 {
		t.Errorf("publish fired despite ctx-cancel")
	}
	if fx.fs.writeCalls != 0 {
		t.Errorf("write fired despite ctx-cancel: %d calls", fx.fs.writeCalls)
	}
}

func TestInstall_PublishUsesContextWithoutCancel(t *testing.T) {
	t.Parallel()
	// Iter-1 codex M1 fix: actually exercise context.WithoutCancel.
	// The previous shape cancelled the parent ctx INSIDE the
	// publisher AFTER the ctx.Err() capture — the assertion was a
	// no-op. New approach: cancel the parent DURING a side-effect
	// (WriteFile of the manifest) so the publish below runs under
	// an already-cancelled parent ctx. The installer's publish path
	// MUST use context.WithoutCancel — otherwise the publish call
	// site's ctx.Err() would be non-nil.
	fx := newInstallerFixture(t)
	parent, cancel := context.WithCancel(context.Background())
	rec := &ctxRecordingPublisher{}
	fx.fs.onWrite = func(path string) {
		if strings.HasSuffix(path, "manifest.json") && strings.Contains(path, "tools/"+testSourceName) {
			cancel()
		}
	}
	fx.inst = NewInstaller(InstallerDeps{
		FS: fx.fs, Publisher: rec, Clock: fx.clk,
		SourceLookup:             fx.lookup.Lookup,
		OperatorIdentityResolver: fx.resolver.Resolve,
		DataDir:                  testDataDir,
	})
	if _, err := fx.inst.Install(parent, validInstallReq()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if rec.calls != 1 {
		t.Fatalf("publisher calls: got %d want 1", rec.calls)
	}
	if rec.lastErr != nil {
		t.Errorf("publisher saw ctx.Err=%v at publish time; expected nil under context.WithoutCancel (parent was cancelled before publish)", rec.lastErr)
	}
	// Confirm the parent ctx WAS already cancelled at publish time —
	// otherwise the assertion is meaningless.
	if parent.Err() == nil {
		t.Errorf("parent ctx unexpectedly not cancelled — test scaffolding bug")
	}
}

// ctxRecordingPublisher records the ctx.Err observed at Publish time
// for the WithoutCancel assertion above.
type ctxRecordingPublisher struct {
	mu      sync.Mutex
	calls   int
	lastErr error
}

func (p *ctxRecordingPublisher) Publish(ctx context.Context, _ string, _ any) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	p.lastErr = ctx.Err()
	return nil
}

func TestInstall_PublishFailureSurfacesAndPreservesEvent(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	cause := errors.New("bus closed")
	fx.pub.publishErr = cause
	ev, err := fx.inst.Install(context.Background(), validInstallReq())
	if !errors.Is(err, ErrPublishLocalPatchApplied) {
		t.Fatalf("err: got %v want ErrPublishLocalPatchApplied", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("err does not chain cause: %v", err)
	}
	// Returned event is populated even on publish failure.
	if ev.SourceName != testSourceName {
		t.Errorf("returned event empty on publish failure: %+v", ev)
	}
	// Live tree IS populated despite publish failure.
	if !fx.fs.hasFile(filepath.Join(testDataDir, "tools", testSourceName, testToolName, "manifest.json")) {
		t.Errorf("live tree not populated despite publish-only failure")
	}
}

func TestInstall_PIICanary_EventOmitsCanaryReason(t *testing.T) {
	t.Parallel()
	// Canary discipline (mirrors M9.4.c TestExecute_EventPayloadOmitsArgsCanary):
	// the operator-supplied Reason IS surfaced verbatim on the bus
	// payload — that is the audit purpose. But the FOLDER content
	// MUST NOT bleed onto the bus payload (only DiffHash carries a
	// summary). Stuff a canary string into the folder; assert the
	// verbatim %+v dump of the published event payload never contains
	// it.
	fx := newInstallerFixture(t)
	fx.fs.AddFile(filepath.Join(testFolderPath, canaryFilePath), []byte(canaryFileContent))
	fx.fs.AddFile(filepath.Join(testFolderPath, "src/secrets.ts"), []byte("export const tok = `"+canaryToken+"`;"))

	if _, err := fx.inst.Install(context.Background(), validInstallReq()); err != nil {
		t.Fatalf("Install: %v", err)
	}
	events := fx.pub.snapshot()
	if len(events) != 1 {
		t.Fatalf("event count: got %d want 1", len(events))
	}
	dump := stringDump(events[0].event)
	for _, banned := range []string{canaryToken, canaryFileContent, canaryFilePath} {
		if strings.Contains(dump, banned) {
			t.Errorf("event payload leaks %q: %s", banned, dump)
		}
	}
}

func TestInstall_LoggerOmitsCanaries(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t)
	fx.fs.AddFile(filepath.Join(testFolderPath, canaryFilePath), []byte(canaryFileContent))
	// Force publish failure so the diagnostic logger fires.
	fx.pub.publishErr = errors.New("publish failure")
	_, _ = fx.inst.Install(context.Background(), validInstallReq())

	for _, e := range fx.logger.snapshot() {
		dump := stringDump(append([]any{e.msg}, e.kv...))
		for _, banned := range []string{canaryToken, canaryFileContent, canaryFilePath, canaryAlt} {
			if strings.Contains(dump, banned) {
				t.Errorf("log entry %q leaks %q", e.msg, banned)
			}
		}
	}
}

// stringDump renders `v` via both the verbose `%+v` and the Go-syntax
// `%#v` verbs so a canary substring landing in EITHER form is a leak.
// Hoisted so each canary test reads as a single substring contains.
func stringDump(v any) string {
	return fmt.Sprintf("%+v %#v", v, v)
}
