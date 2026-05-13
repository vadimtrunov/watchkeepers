package hostedexport_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/hostedexport"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

const (
	testDataDir = "/var/lib/watchkeepers"
	testSource  = "hosted-private"
	testTool    = "weekly_digest"
)

func testDeps() hostedexport.ExporterDeps {
	return hostedexport.ExporterDeps{
		FS:                       newFakeFS(),
		Publisher:                &fakePublisher{},
		Clock:                    newFakeClock(time.Unix(1700000000, 0).UTC()),
		SourceLookup:             constSourceLookup(validHostedSource(testSource), nil),
		OperatorIdentityResolver: constOperatorResolver("alice", nil),
		DataDir:                  testDataDir,
	}
}

func seedTool(fs *fakeFS, source, tool, version string) {
	root := filepath.Join(testDataDir, "tools", source, tool)
	fs.AddDir(root)
	fs.AddFile(filepath.Join(root, "manifest.json"), validManifestJSON(tool, version))
	fs.AddFile(filepath.Join(root, "src", "index.ts"), []byte("export default function(){}\n"))
	fs.AddFileMode(filepath.Join(root, "scripts", "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	fs.AddFile(filepath.Join(root, "tests", "index.test.ts"), []byte("test('ok', ()=>{});\n"))
}

func validRequest(dest string) hostedexport.ExportRequest {
	return hostedexport.ExportRequest{
		SourceName:     testSource,
		ToolName:       testTool,
		Destination:    dest,
		Reason:         "promoting weekly_digest to git for review",
		OperatorIDHint: "alice",
	}
}

// ---- nil-dep panics ----

func TestNewExporter_NilFS_Panics(t *testing.T) {
	deps := testDeps()
	deps.FS = nil
	assertPanicMessage(t, "deps.FS", func() { hostedexport.NewExporter(deps) })
}

func TestNewExporter_NilPublisher_Panics(t *testing.T) {
	deps := testDeps()
	deps.Publisher = nil
	assertPanicMessage(t, "deps.Publisher", func() { hostedexport.NewExporter(deps) })
}

func TestNewExporter_NilClock_Panics(t *testing.T) {
	deps := testDeps()
	deps.Clock = nil
	assertPanicMessage(t, "deps.Clock", func() { hostedexport.NewExporter(deps) })
}

func TestNewExporter_NilSourceLookup_Panics(t *testing.T) {
	deps := testDeps()
	deps.SourceLookup = nil
	assertPanicMessage(t, "deps.SourceLookup", func() { hostedexport.NewExporter(deps) })
}

func TestNewExporter_NilIdentityResolver_Panics(t *testing.T) {
	deps := testDeps()
	deps.OperatorIdentityResolver = nil
	assertPanicMessage(t, "deps.OperatorIdentityResolver", func() { hostedexport.NewExporter(deps) })
}

func TestNewExporter_EmptyDataDir_Panics(t *testing.T) {
	deps := testDeps()
	deps.DataDir = ""
	assertPanicMessage(t, "deps.DataDir", func() { hostedexport.NewExporter(deps) })
}

func TestNewExporter_RelativeDataDir_Panics(t *testing.T) {
	deps := testDeps()
	deps.DataDir = "relative/path"
	assertPanicMessage(t, "deps.DataDir", func() { hostedexport.NewExporter(deps) })
}

func TestNewExporter_OverBoundDataDir_Panics(t *testing.T) {
	deps := testDeps()
	deps.DataDir = "/" + strings.Repeat("a", hostedexport.MaxDataDirLength)
	assertPanicMessage(t, "deps.DataDir", func() { hostedexport.NewExporter(deps) })
}

// ---- happy path ----

// disk, exec-bit preservation, event-payload shape, publish count);
// the cyclomatic count reflects assertion depth, not control flow.
//
//nolint:gocyclo // Happy-path covers ~12 assertion legs (bundle on
func TestExport_HappyPath_WritesBundleAndPublishesEvent(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.2.0")
	pub := deps.Publisher.(*fakePublisher)

	dest := "/tmp/export/weekly_digest"
	ex := hostedexport.NewExporter(deps)

	res, err := ex.Export(context.Background(), validRequest(dest))
	if err != nil {
		t.Fatalf("Export failed: %v", err)
	}
	if res.ToolVersion != "1.2.0" {
		t.Errorf("ToolVersion=%q want %q", res.ToolVersion, "1.2.0")
	}
	if len(res.BundleDigest) != 64 {
		t.Errorf("BundleDigest len=%d want 64", len(res.BundleDigest))
	}
	if res.CorrelationID == "" {
		t.Errorf("CorrelationID empty")
	}
	// Destination tree mirrors the live tree (minus the live root prefix).
	for _, rel := range []string{"manifest.json", "src/index.ts", "scripts/run.sh", "tests/index.test.ts"} {
		path := filepath.Join(dest, rel)
		if _, err := fs.Stat(path); err != nil {
			t.Errorf("destination missing %q: %v", path, err)
		}
	}
	// Exec-bit preserved.
	info, err := fs.Stat(filepath.Join(dest, "scripts", "run.sh"))
	if err != nil {
		t.Fatalf("stat run.sh: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("run.sh exec-bit lost: %v", info.Mode())
	}
	// Single publish call on the hosted_tool_exported topic.
	calls := pub.snapshot()
	if len(calls) != 1 {
		t.Fatalf("publish calls=%d want 1", len(calls))
	}
	if calls[0].topic != hostedexport.TopicHostedToolExported {
		t.Errorf("topic=%q want %q", calls[0].topic, hostedexport.TopicHostedToolExported)
	}
	ev, ok := calls[0].event.(hostedexport.HostedToolExported)
	if !ok {
		t.Fatalf("event is %T not HostedToolExported", calls[0].event)
	}
	if ev.SourceName != testSource || ev.ToolName != testTool || ev.ToolVersion != "1.2.0" {
		t.Errorf("event identifiers: %+v", ev)
	}
	if ev.OperatorID != "alice" {
		t.Errorf("OperatorID=%q want alice", ev.OperatorID)
	}
	if ev.Reason != "promoting weekly_digest to git for review" {
		t.Errorf("Reason=%q", ev.Reason)
	}
	if ev.BundleDigest != res.BundleDigest {
		t.Errorf("BundleDigest mismatch event=%q result=%q", ev.BundleDigest, res.BundleDigest)
	}
}

func TestExport_HappyPath_PublishUsesContextWithoutCancel(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	pub := deps.Publisher.(*fakePublisher)

	ctx, cancel := context.WithCancel(context.Background())
	ex := hostedexport.NewExporter(deps)
	defer cancel()

	if _, err := ex.Export(ctx, validRequest("/tmp/x")); err != nil {
		t.Fatalf("Export: %v", err)
	}
	// Cancel the parent — the captured publish ctx must NOT carry the cancel.
	cancel()
	calls := pub.snapshot()
	if len(calls) != 1 {
		t.Fatalf("publish calls=%d", len(calls))
	}
	if err := calls[0].ctx.Err(); err != nil {
		t.Errorf("publish ctx carries cancellation: %v", err)
	}
}

// ---- validate ----

func TestExport_Validate_RejectsBadInputs(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	ex := hostedexport.NewExporter(deps)

	cases := []struct {
		name string
		req  hostedexport.ExportRequest
	}{
		{"empty source", hostedexport.ExportRequest{SourceName: "", ToolName: testTool, Destination: "/d", Reason: "x", OperatorIDHint: "a"}},
		{"empty tool", hostedexport.ExportRequest{SourceName: testSource, ToolName: "", Destination: "/d", Reason: "x", OperatorIDHint: "a"}},
		{"empty destination", hostedexport.ExportRequest{SourceName: testSource, ToolName: testTool, Destination: "", Reason: "x", OperatorIDHint: "a"}},
		{"relative destination", hostedexport.ExportRequest{SourceName: testSource, ToolName: testTool, Destination: "rel/dest", Reason: "x", OperatorIDHint: "a"}},
		{"empty reason", hostedexport.ExportRequest{SourceName: testSource, ToolName: testTool, Destination: "/d", Reason: "", OperatorIDHint: "a"}},
		{"disallowed source chars", hostedexport.ExportRequest{SourceName: "bad/name", ToolName: testTool, Destination: "/d", Reason: "x", OperatorIDHint: "a"}},
		{"over-bound reason", hostedexport.ExportRequest{SourceName: testSource, ToolName: testTool, Destination: "/d", Reason: strings.Repeat("r", hostedexport.MaxReasonLength+1), OperatorIDHint: "a"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ex.Export(context.Background(), c.req)
			if !errors.Is(err, hostedexport.ErrInvalidExportRequest) {
				t.Fatalf("err=%v want ErrInvalidExportRequest", err)
			}
		})
	}
}

// ---- source lookup paths ----

func TestExport_UnknownSource(t *testing.T) {
	deps := testDeps()
	deps.SourceLookup = constSourceLookup(toolregistry.SourceConfig{}, hostedexport.ErrUnknownSource)
	ex := hostedexport.NewExporter(deps)

	_, err := ex.Export(context.Background(), validRequest("/d"))
	if !errors.Is(err, hostedexport.ErrUnknownSource) {
		t.Fatalf("err=%v want ErrUnknownSource", err)
	}
}

func TestExport_SourceLookupMismatch(t *testing.T) {
	deps := testDeps()
	deps.SourceLookup = constSourceLookup(validHostedSource("other-source"), nil)
	ex := hostedexport.NewExporter(deps)

	_, err := ex.Export(context.Background(), validRequest("/d"))
	if !errors.Is(err, hostedexport.ErrSourceLookupMismatch) {
		t.Fatalf("err=%v want ErrSourceLookupMismatch", err)
	}
}

func TestExport_NonHostedSourceKind_Rejected(t *testing.T) {
	deps := testDeps()
	deps.SourceLookup = constSourceLookup(validLocalSource(testSource), nil)
	ex := hostedexport.NewExporter(deps)

	_, err := ex.Export(context.Background(), validRequest("/d"))
	if !errors.Is(err, hostedexport.ErrInvalidSourceKind) {
		t.Fatalf("err=%v want ErrInvalidSourceKind", err)
	}
}

// Iter-1 m5 fix (reviewer A): non-ErrUnknownSource resolver
// failures (DB outage, config-read failure, etc.) now propagate
// VERBATIM rather than being wrapped as ErrUnknownSource. The
// operator's errors.Is triage routes to the actual cause.
func TestExport_SourceLookupGenericErrorPropagatesVerbatim(t *testing.T) {
	deps := testDeps()
	sentinel := errors.New("upstream resolver outage")
	deps.SourceLookup = constSourceLookup(toolregistry.SourceConfig{}, sentinel)
	ex := hostedexport.NewExporter(deps)

	_, err := ex.Export(context.Background(), validRequest("/d"))
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v does not wrap sentinel", err)
	}
	if errors.Is(err, hostedexport.ErrUnknownSource) {
		t.Fatalf("err=%v MUST NOT be classified as ErrUnknownSource (m5 fix)", err)
	}
}

// ---- operator identity ----

func TestExport_IdentityResolverError_Wrapped(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	sentinel := errors.New("oidc verification failed")
	deps.OperatorIdentityResolver = constOperatorResolver("", sentinel)
	ex := hostedexport.NewExporter(deps)

	_, err := ex.Export(context.Background(), validRequest("/d"))
	if !errors.Is(err, hostedexport.ErrIdentityResolution) {
		t.Fatalf("err=%v want ErrIdentityResolution", err)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("err=%v does not preserve cause", err)
	}
}

func TestExport_IdentityResolverEmpty_Refused(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	deps.OperatorIdentityResolver = constOperatorResolver("", nil)
	ex := hostedexport.NewExporter(deps)

	_, err := ex.Export(context.Background(), validRequest("/d"))
	if !errors.Is(err, hostedexport.ErrEmptyResolvedIdentity) {
		t.Fatalf("err=%v want ErrEmptyResolvedIdentity", err)
	}
}

func TestExport_IdentityResolverInvalidChars_Refused(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	deps.OperatorIdentityResolver = constOperatorResolver("alice; rm -rf /", nil)
	ex := hostedexport.NewExporter(deps)

	_, err := ex.Export(context.Background(), validRequest("/d"))
	if !errors.Is(err, hostedexport.ErrInvalidOperatorID) {
		t.Fatalf("err=%v want ErrInvalidOperatorID", err)
	}
}

func TestValidateOperatorID(t *testing.T) {
	for _, c := range []struct {
		name  string
		id    string
		valid bool
	}{
		{"happy", "alice@example.com", true},
		{"happy-uuid", "00000000-0000-0000-0000-000000000000", true},
		{"empty", "", false},
		{"shell-meta", "alice; rm", false},
		{"too long", strings.Repeat("a", hostedexport.MaxOperatorIDLength+1), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := hostedexport.ValidateOperatorID(c.id)
			if c.valid && err != nil {
				t.Fatalf("err=%v want nil", err)
			}
			if !c.valid && !errors.Is(err, hostedexport.ErrInvalidOperatorID) {
				t.Fatalf("err=%v want ErrInvalidOperatorID", err)
			}
		})
	}
}

// ---- live tree / manifest ----

func TestExport_ToolMissing_Refused(t *testing.T) {
	deps := testDeps()
	// No seedTool call — the live tree does not exist.
	ex := hostedexport.NewExporter(deps)

	_, err := ex.Export(context.Background(), validRequest("/d"))
	if !errors.Is(err, hostedexport.ErrToolMissing) {
		t.Fatalf("err=%v want ErrToolMissing", err)
	}
}

func TestExport_ManifestAbsent_Refused(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	root := filepath.Join(testDataDir, "tools", testSource, testTool)
	fs.AddDir(root)
	fs.AddFile(filepath.Join(root, "src", "index.ts"), []byte("// no manifest\n"))
	ex := hostedexport.NewExporter(deps)

	_, err := ex.Export(context.Background(), validRequest("/d"))
	if !errors.Is(err, hostedexport.ErrManifestRead) {
		t.Fatalf("err=%v want ErrManifestRead", err)
	}
}

func TestExport_ManifestUndecidable_Refused(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	root := filepath.Join(testDataDir, "tools", testSource, testTool)
	fs.AddDir(root)
	fs.AddFile(filepath.Join(root, "manifest.json"), []byte(`{"name":}`)) // malformed
	ex := hostedexport.NewExporter(deps)

	_, err := ex.Export(context.Background(), validRequest("/d"))
	if !errors.Is(err, hostedexport.ErrManifestRead) {
		t.Fatalf("err=%v want ErrManifestRead", err)
	}
}

// ---- destination ----

func TestExport_DestinationNotEmpty_Refused(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	dest := "/tmp/preoccupied"
	fs.AddFile(filepath.Join(dest, "preexisting.txt"), []byte("oops"))
	ex := hostedexport.NewExporter(deps)

	_, err := ex.Export(context.Background(), validRequest(dest))
	if !errors.Is(err, hostedexport.ErrDestinationNotEmpty) {
		t.Fatalf("err=%v want ErrDestinationNotEmpty", err)
	}
}

func TestExport_DestinationAbsent_AcceptedAndCreated(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	dest := "/tmp/fresh-dest"
	ex := hostedexport.NewExporter(deps)

	if _, err := ex.Export(context.Background(), validRequest(dest)); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, err := fs.Stat(filepath.Join(dest, "manifest.json")); err != nil {
		t.Errorf("manifest absent at dest: %v", err)
	}
}

func TestExport_DestinationEmptyExistingDir_AcceptedAndPopulated(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	dest := "/tmp/empty-dest"
	fs.AddDir(dest)
	ex := hostedexport.NewExporter(deps)

	if _, err := ex.Export(context.Background(), validRequest(dest)); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if _, err := fs.Stat(filepath.Join(dest, "manifest.json")); err != nil {
		t.Errorf("manifest absent at dest: %v", err)
	}
}

// ---- ctx ----

func TestExport_CtxCancelledBeforeSideEffects(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	pub := deps.Publisher.(*fakePublisher)
	ex := hostedexport.NewExporter(deps)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dest := "/tmp/ctxcancel"
	_, err := ex.Export(ctx, validRequest(dest))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled", err)
	}
	if _, statErr := fs.Stat(dest); statErr == nil {
		t.Errorf("destination written despite ctx-cancel")
	}
	if got := pub.snapshot(); len(got) != 0 {
		t.Errorf("publish called %d times despite ctx-cancel", len(got))
	}
}

// ---- publish ----

func TestExport_PublishFailure_SurfacesAndPreservesResult(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	deps.Publisher = &fakePublisher{err: errors.New("bus down")}
	logger := &fakeLogger{}
	deps.Logger = logger
	ex := hostedexport.NewExporter(deps)

	res, err := ex.Export(context.Background(), validRequest("/d"))
	if !errors.Is(err, hostedexport.ErrPublishHostedToolExported) {
		t.Fatalf("err=%v want ErrPublishHostedToolExported", err)
	}
	if res.ToolVersion != "1.0.0" {
		t.Errorf("res.ToolVersion=%q want 1.0.0", res.ToolVersion)
	}
	if len(logger.snapshot()) != 1 {
		t.Errorf("logger entries=%d want 1", len(logger.snapshot()))
	}
	// Destination is populated despite the publish failure.
	if _, err := fs.Stat("/d/manifest.json"); err != nil {
		t.Errorf("destination missing after publish failure: %v", err)
	}
}

// ---- PII canary ----

const (
	canaryReason  = "CANARY-REASON-DO-NOT-LEAK-c4n4ry-r34s0n-abc"   //nolint:gosec // G101: synthetic PII canary, not a real credential.
	canaryContent = "CANARY-CONTENT-DO-NOT-LEAK-c4n4ry-c0nt3nt-abc" //nolint:gosec // G101: synthetic PII canary, not a real credential.
	canaryFile    = "ssh_private_key_CANARY-DO-NOT-LEAK-abc"        //nolint:gosec // G101: synthetic PII canary, not a real credential.
)

func TestExport_PIICanary_EventOmitsCanaries(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	// Inject canary substrings into tool source + filename.
	fs.AddFile(filepath.Join(testDataDir, "tools", testSource, testTool, "src", canaryFile),
		[]byte(canaryContent))
	pub := deps.Publisher.(*fakePublisher)
	ex := hostedexport.NewExporter(deps)

	req := validRequest("/d")
	req.Reason = canaryReason
	if _, err := ex.Export(context.Background(), req); err != nil {
		t.Fatalf("Export: %v", err)
	}
	calls := pub.snapshot()
	if len(calls) != 1 {
		t.Fatalf("publish calls=%d", len(calls))
	}
	dump := fmt.Sprintf("%+v %#v", calls[0].event, calls[0].event)
	// Reason IS verbatim on the payload — the operator's
	// accountability statement IS the audit purpose (mirror M9.5
	// localpatch.Reason). So only the source bytes + filename
	// canaries are checked.
	for _, banned := range []string{canaryContent, canaryFile} {
		if strings.Contains(dump, banned) {
			t.Errorf("event payload leaks canary %q: %s", banned, dump)
		}
	}
	if !strings.Contains(dump, canaryReason) {
		t.Errorf("event payload SHOULD carry verbatim Reason (audit purpose); dump=%s", dump)
	}
}

func TestExport_PIICanary_LoggerOmitsCanaries(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	fs.AddFile(filepath.Join(testDataDir, "tools", testSource, testTool, "src", canaryFile),
		[]byte(canaryContent))
	deps.Publisher = &fakePublisher{err: errors.New("bus down")} // forces logger call
	logger := &fakeLogger{}
	deps.Logger = logger
	ex := hostedexport.NewExporter(deps)

	req := validRequest("/d")
	req.Reason = canaryReason
	_, _ = ex.Export(context.Background(), req)
	// Logger must NEVER see the operator-supplied reason, the
	// destination path, or any tool source bytes / filenames.
	entries := logger.snapshot()
	if len(entries) == 0 {
		t.Fatalf("logger had no entries on publish failure")
	}
	dump := fmt.Sprintf("%+v", entries)
	for _, banned := range []string{canaryContent, canaryFile, canaryReason, "/d"} {
		if strings.Contains(dump, banned) {
			t.Errorf("logger leaks %q: %s", banned, dump)
		}
	}
}

// ---- helpers ----

func assertPanicMessage(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic mentioning %q, got none", want)
		}
		msg := fmt.Sprintf("%v", r)
		if !strings.Contains(msg, want) {
			t.Fatalf("panic %q does not mention %q", msg, want)
		}
	}()
	fn()
}

// ---- concurrency ----

func TestExport_Concurrent_DistinctTargets(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	// Seed N distinct tools under the same hosted source.
	const N = 16
	for i := 0; i < N; i++ {
		tool := fmt.Sprintf("tool_%02d", i)
		root := filepath.Join(testDataDir, "tools", testSource, tool)
		fs.AddDir(root)
		fs.AddFile(filepath.Join(root, "manifest.json"), validManifestJSON(tool, "1.0.0"))
		fs.AddFile(filepath.Join(root, "src", "index.ts"), []byte("// "+tool+"\n"))
	}
	pub := deps.Publisher.(*fakePublisher)
	ex := hostedexport.NewExporter(deps)

	var wg sync.WaitGroup
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			req := hostedexport.ExportRequest{
				SourceName:     testSource,
				ToolName:       fmt.Sprintf("tool_%02d", i),
				Destination:    fmt.Sprintf("/tmp/dest_%02d", i),
				Reason:         "concurrent test",
				OperatorIDHint: "alice",
			}
			if _, err := ex.Export(context.Background(), req); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent export: %v", err)
	}
	if got := pub.snapshot(); len(got) != N {
		t.Errorf("publish calls=%d want %d", len(got), N)
	}
	// Correlation IDs must all be distinct (per-(source,tool) atomic nonce).
	seen := map[string]bool{}
	for _, c := range pub.snapshot() {
		ev := c.event.(hostedexport.HostedToolExported)
		if seen[ev.CorrelationID] {
			t.Errorf("duplicate correlation_id %q", ev.CorrelationID)
		}
		seen[ev.CorrelationID] = true
	}
}

// keep imported names referenced in case of unused warnings.
var (
	_ = time.Now
	_ = errors.New
)
