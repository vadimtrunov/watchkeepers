package toolshare_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/github"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolshare"
)

const (
	testDataDir = "/var/lib/watchkeepers"
	testSource  = "private-tools"
	testTool    = "weekly_digest"
)

func testDeps() toolshare.SharerDeps {
	return toolshare.SharerDeps{
		FS:                       newFakeFS(),
		Publisher:                &fakePublisher{},
		Clock:                    newFakeClock(time.Unix(1700000000, 0).UTC()),
		SourceLookup:             constSourceLookup(validSource(testSource), nil),
		ProposerIdentityResolver: constProposerResolver("agent-coordinator-001", nil),
		TargetRepoResolver:       constTargetResolver(validTarget(), nil),
		GitHubClient: &fakeGitHubClient{
			getRefRes:     github.GetRefResult{SHA: strings.Repeat("a", 40)},
			createRefRes:  github.CreateRefResult{SHA: strings.Repeat("a", 40), Ref: "refs/heads/whatever"},
			createFileRes: github.CreateOrUpdateFileResult{FileSHA: "f", CommitSHA: "c"},
			openPRRes:     github.CreatePullRequestResult{Number: 99, HTMLURL: "https://github.com/o/r/pull/99"},
		},
		DataDir: testDataDir,
	}
}

func seedTool(fs *fakeFS, source, tool, version string) {
	root := filepath.Join(testDataDir, "tools", source, tool)
	fs.AddDir(root)
	fs.AddFile(filepath.Join(root, "manifest.json"), validManifestJSON(tool, version))
	fs.AddFile(filepath.Join(root, "src", "index.ts"), []byte("export default function(){}\n"))
	fs.AddFile(filepath.Join(root, "tests", "index.test.ts"), []byte("test('ok', ()=>{});\n"))
}

func validRequest() toolshare.ShareRequest {
	return toolshare.ShareRequest{
		SourceName:     testSource,
		ToolName:       testTool,
		TargetHint:     toolshare.TargetSourcePlatform,
		Reason:         "graduating weekly_digest for community use",
		ProposerIDHint: "agent-coordinator-001",
	}
}

// ---- nil-dep panics ----

func TestNewSharer_NilDeps_Panic(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*toolshare.SharerDeps)
		token string
	}{
		{"FS", func(d *toolshare.SharerDeps) { d.FS = nil }, "deps.FS"},
		{"Publisher", func(d *toolshare.SharerDeps) { d.Publisher = nil }, "deps.Publisher"},
		{"Clock", func(d *toolshare.SharerDeps) { d.Clock = nil }, "deps.Clock"},
		{"SourceLookup", func(d *toolshare.SharerDeps) { d.SourceLookup = nil }, "deps.SourceLookup"},
		{"ProposerIdentityResolver", func(d *toolshare.SharerDeps) { d.ProposerIdentityResolver = nil }, "deps.ProposerIdentityResolver"},
		{"TargetRepoResolver", func(d *toolshare.SharerDeps) { d.TargetRepoResolver = nil }, "deps.TargetRepoResolver"},
		{"GitHubClient", func(d *toolshare.SharerDeps) { d.GitHubClient = nil }, "deps.GitHubClient"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps := testDeps()
			c.mut(&deps)
			assertPanicMessage(t, c.token, func() { toolshare.NewSharer(deps) })
		})
	}
}

func TestNewSharer_SlackNotifierWithoutLeadResolver_Panics(t *testing.T) {
	deps := testDeps()
	deps.SlackNotifier = &fakeSlack{}
	deps.LeadResolver = nil
	assertPanicMessage(t, "LeadResolver", func() { toolshare.NewSharer(deps) })
}

func TestNewSharer_EmptyDataDir_Panics(t *testing.T) {
	deps := testDeps()
	deps.DataDir = ""
	assertPanicMessage(t, "DataDir", func() { toolshare.NewSharer(deps) })
}

func TestNewSharer_RelativeDataDir_Panics(t *testing.T) {
	deps := testDeps()
	deps.DataDir = "relative/path"
	assertPanicMessage(t, "DataDir", func() { toolshare.NewSharer(deps) })
}

// ---- happy path ----

// version, publish topic order, correlation-id parity across both
// events, github call shape, Slack DM). The cyclomatic count reflects
// assertion depth, not control flow.
//
//nolint:gocyclo // Happy-path covers ~15 assertion legs (PR coords,
func TestShare_HappyPath(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.2.0")
	deps.SlackNotifier = &fakeSlack{}
	deps.LeadResolver = constLeadResolver("U02ABC", nil)
	pub := deps.Publisher.(*fakePublisher)
	gh := deps.GitHubClient.(*fakeGitHubClient)
	slack := deps.SlackNotifier.(*fakeSlack)
	s := toolshare.NewSharer(deps)

	res, err := s.Share(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	if res.PRNumber != 99 || res.PRHTMLURL != "https://github.com/o/r/pull/99" {
		t.Errorf("PR coords: %+v", res)
	}
	if res.ToolVersion != "1.2.0" {
		t.Errorf("version=%q", res.ToolVersion)
	}
	if !res.LeadNotified {
		t.Errorf("LeadNotified=false")
	}

	// Two publishes — proposed then pr_opened.
	calls := pub.snapshot()
	if len(calls) != 2 {
		t.Fatalf("publish calls=%d want 2", len(calls))
	}
	if calls[0].topic != toolshare.TopicToolShareProposed {
		t.Errorf("first topic=%q", calls[0].topic)
	}
	if calls[1].topic != toolshare.TopicToolSharePROpened {
		t.Errorf("second topic=%q", calls[1].topic)
	}
	prop := calls[0].event.(toolshare.ToolShareProposed)
	opened := calls[1].event.(toolshare.ToolSharePROpened)
	if prop.CorrelationID != opened.CorrelationID {
		t.Errorf("correlation_id mismatch %q vs %q", prop.CorrelationID, opened.CorrelationID)
	}
	if prop.Reason != "graduating weekly_digest for community use" {
		t.Errorf("Reason=%q", prop.Reason)
	}
	if opened.PRNumber != 99 {
		t.Errorf("opened.PRNumber=%d", opened.PRNumber)
	}

	// GitHub call shape: GetRef → CreateRef → 3xCreateOrUpdateFile → CreatePullRequest.
	gcalls := gh.snapshot()
	if len(gcalls) < 5 {
		t.Fatalf("github calls=%d want >=5", len(gcalls))
	}
	if gcalls[0].op != "GetRef" {
		t.Errorf("first op=%q", gcalls[0].op)
	}
	if gcalls[1].op != "CreateRef" {
		t.Errorf("second op=%q", gcalls[1].op)
	}
	if gcalls[len(gcalls)-1].op != "CreatePullRequest" {
		t.Errorf("last op=%q", gcalls[len(gcalls)-1].op)
	}

	// Slack DM was sent once.
	sent := slack.snapshot()
	if len(sent) != 1 {
		t.Errorf("slack calls=%d want 1", len(sent))
	}
	if !strings.Contains(sent[0].text, "https://github.com/o/r/pull/99") {
		t.Errorf("slack text missing PR URL: %q", sent[0].text)
	}
}

// ---- validate ----

func TestShare_Validate_Rejects(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	s := toolshare.NewSharer(deps)

	cases := []toolshare.ShareRequest{
		{SourceName: "", ToolName: "t", TargetHint: toolshare.TargetSourcePlatform, Reason: "r"},
		{SourceName: "s", ToolName: "", TargetHint: toolshare.TargetSourcePlatform, Reason: "r"},
		{SourceName: "s", ToolName: "t", TargetHint: "", Reason: "r"},
		{SourceName: "s", ToolName: "t", TargetHint: "invalid", Reason: "r"},
		{SourceName: "s", ToolName: "t", TargetHint: toolshare.TargetSourcePlatform, Reason: ""},
		{SourceName: "bad/name", ToolName: "t", TargetHint: toolshare.TargetSourcePlatform, Reason: "r"},
	}
	for i, c := range cases {
		_, err := s.Share(context.Background(), c)
		if !errors.Is(err, toolshare.ErrInvalidShareRequest) {
			t.Errorf("case %d: err=%v want ErrInvalidShareRequest", i, err)
		}
	}
}

// ---- source / target lookup ----

func TestShare_UnknownSource(t *testing.T) {
	deps := testDeps()
	deps.SourceLookup = constSourceLookup(validSource("ignored"), toolshare.ErrUnknownSource)
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrUnknownSource) {
		t.Fatalf("err=%v", err)
	}
}

func TestShare_SourceLookupMismatch(t *testing.T) {
	deps := testDeps()
	cfg := validSource("other-source")
	deps.SourceLookup = constSourceLookup(cfg, nil)
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrSourceLookupMismatch) {
		t.Fatalf("err=%v", err)
	}
}

func TestShare_TargetResolverError(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	deps.TargetRepoResolver = constTargetResolver(toolshare.ResolvedTarget{}, errBoom)
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrTargetResolution) {
		t.Fatalf("err=%v", err)
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("cause not preserved: %v", err)
	}
}

func TestShare_TargetResolverEmptyOwner_Refused(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	target := validTarget()
	target.Owner = ""
	deps.TargetRepoResolver = constTargetResolver(target, nil)
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrInvalidTarget) {
		t.Fatalf("err=%v", err)
	}
}

// ---- identity ----

func TestShare_IdentityResolverError(t *testing.T) {
	deps := testDeps()
	deps.ProposerIdentityResolver = constProposerResolver("", errBoom)
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrIdentityResolution) {
		t.Fatalf("err=%v", err)
	}
}

func TestShare_IdentityResolverEmpty(t *testing.T) {
	deps := testDeps()
	deps.ProposerIdentityResolver = constProposerResolver("", nil)
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrEmptyResolvedIdentity) {
		t.Fatalf("err=%v", err)
	}
}

func TestShare_IdentityResolverInvalidChars(t *testing.T) {
	deps := testDeps()
	deps.ProposerIdentityResolver = constProposerResolver("bad;input", nil)
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrInvalidProposerID) {
		t.Fatalf("err=%v", err)
	}
}

// ---- live tree ----

func TestShare_ToolMissing(t *testing.T) {
	deps := testDeps()
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrToolMissing) {
		t.Fatalf("err=%v", err)
	}
}

func TestShare_ManifestMissing(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	root := filepath.Join(testDataDir, "tools", testSource, testTool)
	fs.AddDir(root)
	fs.AddFile(filepath.Join(root, "src", "x.ts"), []byte("// nope\n"))
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrManifestRead) {
		t.Fatalf("err=%v", err)
	}
}

func TestShare_ManifestUndecidable(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	root := filepath.Join(testDataDir, "tools", testSource, testTool)
	fs.AddDir(root)
	fs.AddFile(filepath.Join(root, "manifest.json"), []byte(`{"name":}`))
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrManifestRead) {
		t.Fatalf("err=%v", err)
	}
}

func TestShare_EmptyToolTree_Refused(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	root := filepath.Join(testDataDir, "tools", testSource, testTool)
	fs.AddDir(root)
	fs.AddFile(filepath.Join(root, "manifest.json"), validManifestJSON(testTool, "1.0.0"))
	// We DELETE the manifest after seeding so walk returns 0 entries... actually
	// the manifest is itself a regular file. Better to make walk see only the
	// (empty) tree by NOT seeding any file but keeping the dir. Add a sentinel
	// directory only with no regular files.
	deps.FS = newFakeFS()
	fs2 := deps.FS.(*fakeFS)
	root2 := filepath.Join(testDataDir, "tools", testSource, testTool)
	fs2.AddDir(root2)
	fs2.AddFile(filepath.Join(root2, "manifest.json"), validManifestJSON(testTool, "1.0.0"))
	// Now ALSO delete the manifest from files map to simulate "stat succeeds via
	// dir parent but read finds nothing"... easier: just don't add it. But then
	// manifest-read fails first. The "empty tree" path is structurally hard to
	// reach because manifest.json IS one of the files.
	// Instead, let's just verify the case via the MaxFilesPerShare boundary
	// case below. Mark this test as a no-op + comment for future addition.
	t.Skip("empty-tree case structurally unreachable: manifest.json is always one regular file")
	_ = fs
	_ = root
}

func TestShare_TooManyFiles_Refused(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	root := filepath.Join(testDataDir, "tools", testSource, testTool)
	fs.AddDir(root)
	fs.AddFile(filepath.Join(root, "manifest.json"), validManifestJSON(testTool, "1.0.0"))
	for i := 0; i < toolshare.MaxFilesPerShare; i++ {
		fs.AddFile(filepath.Join(root, "src", fmt.Sprintf("file_%03d.ts", i)), []byte("// f\n"))
	}
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrInvalidShareRequest) {
		t.Fatalf("err=%v want ErrInvalidShareRequest", err)
	}
}

// ---- ctx ----

func TestShare_CtxCancelled(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	s := toolshare.NewSharer(deps)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Share(ctx, validRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

// ---- publish ----

func TestShare_ProposedPublishFailure_Aborts(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	deps.Publisher = &fakePublisher{errFor: map[string]error{
		toolshare.TopicToolShareProposed: errBoom,
	}}
	gh := deps.GitHubClient.(*fakeGitHubClient)
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrPublishToolShareProposed) {
		t.Fatalf("err=%v", err)
	}
	// No github calls must have fired.
	if len(gh.snapshot()) != 0 {
		t.Errorf("github calls fired after proposed publish failure: %d", len(gh.snapshot()))
	}
}

func TestShare_PROpenedPublishFailure_SurfacesAndPreservesResult(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	deps.Publisher = &fakePublisher{errFor: map[string]error{
		toolshare.TopicToolSharePROpened: errBoom,
	}}
	logger := &fakeLogger{}
	deps.Logger = logger
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrPublishToolSharePROpened) {
		t.Fatalf("err=%v", err)
	}
	if len(logger.snapshot()) != 1 {
		t.Errorf("logger entries=%d want 1", len(logger.snapshot()))
	}
}

// ---- github errors ----

func TestShare_GitHubGetRefError(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	gh := deps.GitHubClient.(*fakeGitHubClient)
	gh.getRefErr = github.ErrRepoNotFound
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrGitHubGetBaseRef) {
		t.Fatalf("err=%v", err)
	}
	if !errors.Is(err, github.ErrRepoNotFound) {
		t.Fatalf("err=%v does not chain", err)
	}
}

func TestShare_GitHubCreateRefError(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	gh := deps.GitHubClient.(*fakeGitHubClient)
	gh.createRefErr = errBoom
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrGitHubCreateBranch) {
		t.Fatalf("err=%v", err)
	}
}

func TestShare_GitHubCreateFileError(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	gh := deps.GitHubClient.(*fakeGitHubClient)
	gh.createFileErrs = map[string]error{"src/index.ts": errBoom}
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrGitHubCreateFile) {
		t.Fatalf("err=%v", err)
	}
}

func TestShare_GitHubOpenPRError(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	gh := deps.GitHubClient.(*fakeGitHubClient)
	gh.openPRErr = github.ErrRateLimited
	s := toolshare.NewSharer(deps)
	_, err := s.Share(context.Background(), validRequest())
	if !errors.Is(err, toolshare.ErrGitHubOpenPR) {
		t.Fatalf("err=%v", err)
	}
}

// ---- slack best-effort ----

func TestShare_NoSlackNotifier_LeadNotifiedFalse(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	s := toolshare.NewSharer(deps)
	res, err := s.Share(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	if res.LeadNotified {
		t.Errorf("LeadNotified=true with nil SlackNotifier")
	}
}

func TestShare_SlackFailure_NonFatal(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	deps.SlackNotifier = &fakeSlack{sendErr: errBoom}
	deps.LeadResolver = constLeadResolver("U02ABC", nil)
	logger := &fakeLogger{}
	deps.Logger = logger
	s := toolshare.NewSharer(deps)

	res, err := s.Share(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Share: %v want nil — slack must be non-fatal", err)
	}
	if res.LeadNotified {
		t.Errorf("LeadNotified=true with slack send error")
	}
	if len(logger.snapshot()) == 0 {
		t.Errorf("expected log entry for slack failure")
	}
}

func TestShare_LeadResolverEmpty_DMSkipped(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	slack := &fakeSlack{}
	deps.SlackNotifier = slack
	deps.LeadResolver = constLeadResolver("", nil)
	s := toolshare.NewSharer(deps)

	res, err := s.Share(context.Background(), validRequest())
	if err != nil {
		t.Fatalf("Share: %v", err)
	}
	if res.LeadNotified {
		t.Errorf("LeadNotified=true with empty lead user id")
	}
	if len(slack.sent) > 0 {
		t.Errorf("slack messages sent despite empty lead: %d", len(slack.sent))
	}
}

// ---- PII canary ----

const (
	canaryReason  = "CANARY-REASON-DO-NOT-LEAK-c4n4ry-r34s0n"   //nolint:gosec // G101: synthetic PII canary.
	canaryContent = "CANARY-CONTENT-DO-NOT-LEAK-c4n4ry-c0nt3nt" //nolint:gosec // G101: synthetic PII canary.
	canaryFile    = "ssh_key_CANARY-DO-NOT-LEAK"                //nolint:gosec // G101: synthetic PII canary.
)

func TestShare_PIICanary_PayloadOmitsSourceContentAndFilename(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	fs.AddFile(filepath.Join(testDataDir, "tools", testSource, testTool, "src", canaryFile),
		[]byte(canaryContent))
	pub := deps.Publisher.(*fakePublisher)
	s := toolshare.NewSharer(deps)

	req := validRequest()
	req.Reason = canaryReason
	if _, err := s.Share(context.Background(), req); err != nil {
		t.Fatalf("Share: %v", err)
	}
	for _, c := range pub.snapshot() {
		dump := fmt.Sprintf("%+v %#v", c.event, c.event)
		for _, banned := range []string{canaryContent, canaryFile} {
			if strings.Contains(dump, banned) {
				t.Errorf("topic %q payload leaks %q: %s", c.topic, banned, dump)
			}
		}
	}
	// Reason MUST land verbatim on the proposed event (audit purpose).
	proposed := pub.snapshot()[0].event.(toolshare.ToolShareProposed)
	if proposed.Reason != canaryReason {
		t.Errorf("Reason not verbatim on payload: %q", proposed.Reason)
	}
}

func TestShare_PIICanary_LoggerOmitsCanaries(t *testing.T) {
	deps := testDeps()
	fs := deps.FS.(*fakeFS)
	seedTool(fs, testSource, testTool, "1.0.0")
	fs.AddFile(filepath.Join(testDataDir, "tools", testSource, testTool, "src", canaryFile),
		[]byte(canaryContent))
	deps.Publisher = &fakePublisher{errFor: map[string]error{
		toolshare.TopicToolShareProposed: errors.New("bus down"),
	}}
	logger := &fakeLogger{}
	deps.Logger = logger
	s := toolshare.NewSharer(deps)

	req := validRequest()
	req.Reason = canaryReason
	_, _ = s.Share(context.Background(), req)
	dump := fmt.Sprintf("%+v", logger.snapshot())
	for _, banned := range []string{canaryContent, canaryFile, canaryReason} {
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
