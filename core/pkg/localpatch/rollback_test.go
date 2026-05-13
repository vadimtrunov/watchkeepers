package localpatch

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

const (
	testTargetVersion = "0.9.0"
	testCurrentLive   = "1.0.0"
	testRollbackReas  = "v1.0.0 panics on unicode tool names — see incident #4711"
)

func newRollbackerFixture(t *testing.T) *installerFixture {
	t.Helper()
	fx := newInstallerFixture(t)
	// Pre-populate a snapshot at testTargetVersion + a live tree at testCurrentLive.
	snapDir := filepath.Join(testDataDir, historyDirName, testSourceName, testToolName, testTargetVersion)
	fx.fs.AddFile(filepath.Join(snapDir, "manifest.json"), validManifestJSON(testToolName, testTargetVersion))
	fx.fs.AddFile(filepath.Join(snapDir, "src/index.ts"), []byte(`export const stable = () => "v0.9";`))
	livePath := filepath.Join(testDataDir, "tools", testSourceName, testToolName)
	fx.fs.AddFile(filepath.Join(livePath, "manifest.json"), validManifestJSON(testToolName, testCurrentLive))
	fx.fs.AddFile(filepath.Join(livePath, "src/index.ts"), []byte(`export const broken = () => panic;`))
	return fx
}

func newRollbackerFromFixture(fx *installerFixture) *Rollbacker {
	return NewRollbacker(RollbackerDeps{
		FS: fx.fs, Publisher: fx.pub, Clock: fx.clk,
		SourceLookup:             fx.lookup.Lookup,
		OperatorIdentityResolver: fx.resolver.Resolve,
		DataDir:                  testDataDir,
		Logger:                   fx.logger,
	})
}

func validRollbackReq() RollbackRequest {
	return RollbackRequest{
		SourceName:     testSourceName,
		ToolName:       testToolName,
		TargetVersion:  testTargetVersion,
		Reason:         testRollbackReas,
		OperatorIDHint: testOperatorID,
	}
}

func TestNewRollbacker_NilDepsPanic(t *testing.T) {
	t.Parallel()
	base := RollbackerDeps{
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
		mutate  func(*RollbackerDeps)
		wantSub string
	}{
		{"FS nil", func(d *RollbackerDeps) { d.FS = nil }, "deps.FS"},
		{"Publisher nil", func(d *RollbackerDeps) { d.Publisher = nil }, "deps.Publisher"},
		{"Clock nil", func(d *RollbackerDeps) { d.Clock = nil }, "deps.Clock"},
		{"SourceLookup nil", func(d *RollbackerDeps) { d.SourceLookup = nil }, "deps.SourceLookup"},
		{"OperatorIdentityResolver nil", func(d *RollbackerDeps) { d.OperatorIdentityResolver = nil }, "deps.OperatorIdentityResolver"},
		{"DataDir empty", func(d *RollbackerDeps) { d.DataDir = "" }, "deps.DataDir"},
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
					t.Fatalf("expected string panic, got %T", r)
				}
				if !strings.Contains(msg, tc.wantSub) {
					t.Errorf("panic msg %q does not contain %q", msg, tc.wantSub)
				}
			}()
			d := base
			tc.mutate(&d)
			_ = NewRollbacker(d)
		})
	}
}

func TestRollback_HappyPath_RestoresAndForwardSnapshots(t *testing.T) {
	t.Parallel()
	fx := newRollbackerFixture(t)
	rb := newRollbackerFromFixture(fx)
	ev, err := rb.Rollback(context.Background(), validRollbackReq())
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Live tree restored to v0.9.
	liveManifest := filepath.Join(testDataDir, "tools", testSourceName, testToolName, "manifest.json")
	got, err := fx.fs.ReadFile(liveManifest)
	if err != nil {
		t.Fatalf("read live manifest: %v", err)
	}
	if !strings.Contains(string(got), `"version":"0.9.0"`) {
		t.Errorf("live manifest not restored: %s", got)
	}

	// Forward snapshot of the previously-live v1.0.0 created.
	forwardSnap := filepath.Join(testDataDir, historyDirName, testSourceName, testToolName, testCurrentLive, "manifest.json")
	if !fx.fs.hasFile(forwardSnap) {
		t.Errorf("forward snapshot of live missing at %q", forwardSnap)
	}

	// Event payload uses Operation=rollback + correct ToolVersion.
	if ev.Operation != OperationRollback {
		t.Errorf("Operation: got %q want %q", ev.Operation, OperationRollback)
	}
	if ev.ToolVersion != testTargetVersion {
		t.Errorf("ToolVersion: got %q want %q", ev.ToolVersion, testTargetVersion)
	}
	if ev.Reason != testRollbackReas {
		t.Errorf("Reason: got %q want %q", ev.Reason, testRollbackReas)
	}
	if len(ev.DiffHash) != 64 {
		t.Errorf("DiffHash: got %d hex chars want 64", len(ev.DiffHash))
	}
}

func TestRollback_SnapshotMissingRefused(t *testing.T) {
	t.Parallel()
	fx := newInstallerFixture(t) // no snapshots seeded
	rb := newRollbackerFromFixture(fx)
	_, err := rb.Rollback(context.Background(), validRollbackReq())
	if !errors.Is(err, ErrSnapshotMissing) {
		t.Fatalf("err: got %v want ErrSnapshotMissing", err)
	}
	if len(fx.pub.snapshot()) != 0 {
		t.Errorf("publish fired despite missing snapshot")
	}
}

func TestRollback_NonLocalSourceRefused(t *testing.T) {
	t.Parallel()
	fx := newRollbackerFixture(t)
	fx.lookup = newConstSourceLookup(validGitSource(testSourceName))
	rb := NewRollbacker(RollbackerDeps{
		FS: fx.fs, Publisher: fx.pub, Clock: fx.clk,
		SourceLookup:             fx.lookup.Lookup,
		OperatorIdentityResolver: fx.resolver.Resolve,
		DataDir:                  testDataDir,
	})
	_, err := rb.Rollback(context.Background(), validRollbackReq())
	if !errors.Is(err, ErrInvalidSourceKind) {
		t.Fatalf("err: got %v want ErrInvalidSourceKind", err)
	}
}

func TestRollback_ValidateRejects(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(*RollbackRequest)
		wantSub string
	}{
		{"empty source", func(r *RollbackRequest) { r.SourceName = "" }, "source_name"},
		{"empty tool", func(r *RollbackRequest) { r.ToolName = "" }, "tool_name"},
		{"empty version", func(r *RollbackRequest) { r.TargetVersion = "" }, "target_version"},
		{"empty reason", func(r *RollbackRequest) { r.Reason = "" }, "reason"},
		{"bad version chars", func(r *RollbackRequest) { r.TargetVersion = "../escape" }, "disallowed characters"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fx := newRollbackerFixture(t)
			rb := newRollbackerFromFixture(fx)
			req := validRollbackReq()
			tc.mutate(&req)
			_, err := rb.Rollback(context.Background(), req)
			if !errors.Is(err, ErrInvalidRollbackRequest) {
				t.Fatalf("err: got %v want ErrInvalidRollbackRequest", err)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err msg missing %q: %v", tc.wantSub, err)
			}
		})
	}
}

func TestRollback_SnapshotManifestVersionMismatchRefused(t *testing.T) {
	t.Parallel()
	fx := newRollbackerFixture(t)
	// Tamper: snapshot dir is named v0.9.0 but manifest says v0.8.0.
	tampered := validManifestJSON(testToolName, "0.8.0")
	fx.fs.AddFile(filepath.Join(testDataDir, historyDirName, testSourceName, testToolName, testTargetVersion, "manifest.json"), tampered)
	rb := newRollbackerFromFixture(fx)
	_, err := rb.Rollback(context.Background(), validRollbackReq())
	if !errors.Is(err, ErrManifestRead) {
		t.Fatalf("err: got %v want ErrManifestRead (tampered snapshot)", err)
	}
}

func TestRollback_CtxCancelledRefusedBeforeSideEffects(t *testing.T) {
	t.Parallel()
	fx := newRollbackerFixture(t)
	rb := newRollbackerFromFixture(fx)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := rb.Rollback(ctx, validRollbackReq())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err: got %v want context.Canceled", err)
	}
	if len(fx.pub.snapshot()) != 0 {
		t.Errorf("publish fired despite ctx-cancel")
	}
}

func TestRollback_PublishFailureSurfacesAndPreservesEvent(t *testing.T) {
	t.Parallel()
	fx := newRollbackerFixture(t)
	cause := errors.New("bus closed")
	fx.pub.publishErr = cause
	rb := newRollbackerFromFixture(fx)
	ev, err := rb.Rollback(context.Background(), validRollbackReq())
	if !errors.Is(err, ErrPublishLocalPatchApplied) {
		t.Fatalf("err: got %v want ErrPublishLocalPatchApplied", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("err does not chain cause: %v", err)
	}
	if ev.SourceName != testSourceName {
		t.Errorf("returned event empty on publish failure: %+v", ev)
	}
}

func TestRollback_SameVersionRollbackSkipsForwardSnapshot(t *testing.T) {
	t.Parallel()
	fx := newRollbackerFixture(t)
	// Rewrite the live manifest to MATCH the target version — operator
	// is requesting a rollback to the version they already have.
	livePath := filepath.Join(testDataDir, "tools", testSourceName, testToolName)
	fx.fs.AddFile(filepath.Join(livePath, "manifest.json"), validManifestJSON(testToolName, testTargetVersion))
	rb := newRollbackerFromFixture(fx)
	if _, err := rb.Rollback(context.Background(), validRollbackReq()); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// No new forward snapshot dir should have appeared (operation
	// degenerates to a re-copy).
	forwardSnap := filepath.Join(testDataDir, historyDirName, testSourceName, testToolName, testCurrentLive)
	if fx.fs.hasDir(forwardSnap) {
		t.Errorf("forward snapshot of v1.0.0 created on same-version rollback")
	}
}

func TestRollback_UndecidableLiveTreeRefused(t *testing.T) {
	t.Parallel()
	// Iter-1 codex M2 fix: rollback over an undecidable live tree
	// (manifest missing / malformed) must refuse with
	// ErrManifestRead BEFORE the destructive replaceLive, symmetric
	// with TestInstall_PriorTreeUndecidableRefused.
	fx := newRollbackerFixture(t)
	// Corrupt the live manifest.
	livePath := filepath.Join(testDataDir, "tools", testSourceName, testToolName)
	fx.fs.AddFile(filepath.Join(livePath, "manifest.json"), []byte(`{not valid json`))
	rb := newRollbackerFromFixture(fx)
	_, err := rb.Rollback(context.Background(), validRollbackReq())
	if !errors.Is(err, ErrManifestRead) {
		t.Fatalf("err: got %v want ErrManifestRead", err)
	}
	// Live src must NOT have been touched.
	src, srcErr := fx.fs.ReadFile(filepath.Join(livePath, "src/index.ts"))
	if srcErr != nil {
		t.Fatalf("read live src: %v", srcErr)
	}
	if !strings.Contains(string(src), "broken") {
		t.Errorf("rollback proceeded over undecidable live tree: %q", src)
	}
}

func TestRollback_SourceLookupMismatchSurfaced(t *testing.T) {
	t.Parallel()
	fx := newRollbackerFixture(t)
	fx.lookup.configs[testSourceName] = toolregistry.SourceConfig{
		Name:       "OTHER-NAME",
		Kind:       toolregistry.SourceKindLocal,
		PullPolicy: toolregistry.PullPolicyOnDemand,
	}
	rb := newRollbackerFromFixture(fx)
	_, err := rb.Rollback(context.Background(), validRollbackReq())
	if !errors.Is(err, ErrSourceLookupMismatch) {
		t.Fatalf("err: got %v want ErrSourceLookupMismatch", err)
	}
}

func TestRollback_OperatorResolverErrorWrapped(t *testing.T) {
	t.Parallel()
	fx := newRollbackerFixture(t)
	cause := errors.New("oidc verification failed")
	fx.resolver.err = cause
	rb := newRollbackerFromFixture(fx)
	_, err := rb.Rollback(context.Background(), validRollbackReq())
	if !errors.Is(err, ErrIdentityResolution) {
		t.Fatalf("err: got %v want ErrIdentityResolution", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("err does not chain cause: %v", err)
	}
}
