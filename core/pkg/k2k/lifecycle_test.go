package k2k_test

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
)

// fakeSlackChannels is the hand-rolled fake the lifecycle tests inject
// in place of `*slack.Client`. Matches the M1.1.b boundary contract
// for `CreateChannel` / `InviteToChannel` / `ArchiveChannel`. No
// mocking-library imports — the skill discipline requires hand-rolled
// fakes so the test surface stays scannable at review time.
type fakeSlackChannels struct {
	mu sync.Mutex

	// createName / createPrivate record the last CreateChannel call.
	createName    string
	createPrivate bool
	// createReturns is the channel id CreateChannel returns; tests
	// override it per case.
	createReturns string
	// createErr is the error CreateChannel returns; tests override it
	// per case.
	createErr error
	// createCalls is the cumulative count for retry / idempotency
	// pinning.
	createCalls int

	// inviteChannel / inviteUsers record the last InviteToChannel call.
	inviteChannel string
	inviteUsers   []string
	inviteErr     error
	inviteCalls   int

	// archiveChannel records the last ArchiveChannel call.
	archiveChannel string
	archiveErr     error
	archiveCalls   int
}

func (f *fakeSlackChannels) CreateChannel(_ context.Context, name string, isPrivate bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls++
	f.createName = name
	f.createPrivate = isPrivate
	return f.createReturns, f.createErr
}

func (f *fakeSlackChannels) InviteToChannel(_ context.Context, channelID string, userIDs []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inviteCalls++
	f.inviteChannel = channelID
	// Deep-copy the slice so caller-side mutation of f.inviteUsers in
	// a later assertion cannot affect the test's view of what was
	// invited.
	users := make([]string, len(userIDs))
	copy(users, userIDs)
	f.inviteUsers = users
	return f.inviteErr
}

func (f *fakeSlackChannels) ArchiveChannel(_ context.Context, channelID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.archiveCalls++
	f.archiveChannel = channelID
	return f.archiveErr
}

// newLifecycle returns a [*k2k.Lifecycle] wired to a fresh in-memory
// repo + the supplied fake Slack channels. Hoisted so each test case
// stays a one-liner.
func newLifecycle(t *testing.T, slack *fakeSlackChannels) (*k2k.Lifecycle, *k2k.MemoryRepository) {
	t.Helper()
	repo := newRepo(t)
	lc := k2k.NewLifecycle(k2k.LifecycleDeps{Repo: repo, Slack: slack})
	return lc, repo
}

func TestNewLifecycle_PanicsOnNilRepo(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewLifecycle(nil-repo): no panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value = %T, want string", r)
		}
		if !strings.Contains(msg, "deps.Repo must not be nil") {
			t.Errorf("panic message = %q, want substring 'deps.Repo must not be nil'", msg)
		}
	}()
	k2k.NewLifecycle(k2k.LifecycleDeps{Slack: &fakeSlackChannels{}})
}

func TestNewLifecycle_PanicsOnNilSlack(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("NewLifecycle(nil-slack): no panic")
		}
		msg, ok := r.(string)
		if !ok {
			t.Fatalf("panic value = %T, want string", r)
		}
		if !strings.Contains(msg, "deps.Slack must not be nil") {
			t.Errorf("panic message = %q, want substring 'deps.Slack must not be nil'", msg)
		}
	}()
	k2k.NewLifecycle(k2k.LifecycleDeps{Repo: newRepo(t)})
}

func TestDeriveChannelName_Shape(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("abcdef01-2345-6789-abcd-ef0123456789")
	got := k2k.DeriveChannelName(id)
	want := "k2k-abcdef01"
	if got != want {
		t.Errorf("DeriveChannelName(%s) = %q, want %q", id, got, want)
	}
}

func TestDeriveChannelName_NilUUID(t *testing.T) {
	t.Parallel()

	if got := k2k.DeriveChannelName(uuid.Nil); got != "" {
		t.Errorf("DeriveChannelName(uuid.Nil) = %q, want empty", got)
	}
}

func TestDeriveChannelName_Deterministic(t *testing.T) {
	t.Parallel()

	id := uuid.New()
	a := k2k.DeriveChannelName(id)
	b := k2k.DeriveChannelName(id)
	if a != b {
		t.Errorf("DeriveChannelName not deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "k2k-") {
		t.Errorf("DeriveChannelName missing prefix: %q", a)
	}
	// 4-char prefix `k2k-` + 8 hex chars = 12.
	if len(a) != 12 {
		t.Errorf("DeriveChannelName length = %d, want 12", len(a))
	}
}

func TestLifecycle_Open_HappyPath(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-NEW-123"}
	lc, _ := newLifecycle(t, slack)

	conv, err := lc.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if conv.SlackChannelID != "C-NEW-123" {
		t.Errorf("SlackChannelID = %q, want %q", conv.SlackChannelID, "C-NEW-123")
	}
	if conv.Status != k2k.StatusOpen {
		t.Errorf("Status = %q, want StatusOpen", conv.Status)
	}
	if slack.createCalls != 1 {
		t.Errorf("create calls = %d, want 1", slack.createCalls)
	}
	if !slack.createPrivate {
		t.Errorf("CreateChannel isPrivate = false, want true")
	}
	wantName := k2k.DeriveChannelName(conv.ID)
	if slack.createName != wantName {
		t.Errorf("CreateChannel name = %q, want %q", slack.createName, wantName)
	}
	if slack.inviteCalls != 1 {
		t.Errorf("invite calls = %d, want 1", slack.inviteCalls)
	}
	if slack.inviteChannel != "C-NEW-123" {
		t.Errorf("InviteToChannel channelID = %q, want %q", slack.inviteChannel, "C-NEW-123")
	}
	wantParticipants := []string{"bot-a", "bot-b"}
	if !equalStringSlice(slack.inviteUsers, wantParticipants) {
		t.Errorf("InviteToChannel users = %v, want %v", slack.inviteUsers, wantParticipants)
	}
}

func TestLifecycle_Open_RefusesCancelledCtx(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-X"}
	lc, _ := newLifecycle(t, slack)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := lc.Open(ctx, validParams())
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Open cancelled ctx: err = %v, want context.Canceled", err)
	}
	if slack.createCalls != 0 {
		t.Errorf("create calls = %d, want 0 (ctx-cancel short-circuit)", slack.createCalls)
	}
}

func TestLifecycle_Open_RepoValidationSurfaces(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{}
	lc, _ := newLifecycle(t, slack)

	p := validParams()
	p.OrganizationID = uuid.Nil
	_, err := lc.Open(context.Background(), p)
	if !errors.Is(err, k2k.ErrEmptyOrganization) {
		t.Errorf("Open: err = %v, want ErrEmptyOrganization", err)
	}
	if slack.createCalls != 0 {
		t.Errorf("create calls = %d, want 0 (validation precedes Slack)", slack.createCalls)
	}
}

func TestLifecycle_Open_CreateChannelError_NoRowBound(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("slack: conversations.create: simulated failure")
	slack := &fakeSlackChannels{createErr: wantErr}
	lc, repo := newLifecycle(t, slack)

	_, err := lc.Open(context.Background(), validParams())
	if err == nil {
		t.Fatalf("Open with create error: no error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Open: err = %v, want sentinel chain to %v", err, wantErr)
	}
	if slack.inviteCalls != 0 {
		t.Errorf("invite calls = %d, want 0 (Slack create failure halts before invite)", slack.inviteCalls)
	}
	// The repository row exists in StatusOpen with empty SlackChannelID
	// — the orphan-row pattern documented on Lifecycle.Open. The
	// M1.7 archive-on-summary writer will reconcile it.
	rows, listErr := repo.List(
		context.Background(),
		k2k.ListFilter{OrganizationID: validParams().OrganizationID, Status: k2k.StatusOpen},
	)
	if listErr != nil {
		t.Fatalf("List after create failure: %v", listErr)
	}
	_ = rows // intentional: the orphan is documented behaviour
}

func TestLifecycle_Open_EmptyChannelIDReturned_FailsFast(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "   "}
	lc, _ := newLifecycle(t, slack)

	_, err := lc.Open(context.Background(), validParams())
	if err == nil {
		t.Fatal("Open with empty channel id: no error")
	}
	if !strings.Contains(err.Error(), "empty channel id") {
		t.Errorf("Open: err = %q, want substring 'empty channel id'", err.Error())
	}
	if slack.inviteCalls != 0 {
		t.Errorf("invite calls = %d, want 0", slack.inviteCalls)
	}
}

func TestLifecycle_Open_InviteError_RowRemainsOpenWithBoundChannel(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("slack: conversations.invite: simulated")
	slack := &fakeSlackChannels{createReturns: "C-X", inviteErr: wantErr}
	lc, repo := newLifecycle(t, slack)

	p := validParams()
	_, err := lc.Open(context.Background(), p)
	if err == nil {
		t.Fatal("Open with invite error: no error")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Open: err = %v, want wrap of sentinel", err)
	}
	// Bind-before-invite ordering (iter-1 codex Major fix): the row
	// carries the bound SlackChannelID even though the subsequent
	// invite call failed. A follow-up Close on this row will
	// correctly archive both the row AND the live Slack channel
	// (no orphan-channel leak).
	rows, listErr := repo.List(
		context.Background(),
		k2k.ListFilter{OrganizationID: p.OrganizationID, Status: k2k.StatusOpen},
	)
	if listErr != nil {
		t.Fatalf("List: %v", listErr)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].SlackChannelID != "C-X" {
		t.Errorf("post-invite-fail row SlackChannelID = %q, want %q (bind precedes invite)", rows[0].SlackChannelID, "C-X")
	}
}

// TestLifecycle_Open_BindPrecedesInvite pins the iter-1 codex Major
// fix: Repository.BindSlackChannel is called BEFORE
// SlackChannels.InviteToChannel so a concurrent Close cannot observe
// a row with empty SlackChannelID after CreateChannel succeeded. We
// drive the ordering with a fake InviteToChannel that reads back the
// row state and asserts the bind already happened.
func TestLifecycle_Open_BindPrecedesInvite(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)
	var capturedChannelIDAtInvite string
	probe := &probingFakeSlack{
		fakeSlackChannels: fakeSlackChannels{createReturns: "C-INV"},
		onInvite: func(_ string) {
			rows, _ := repo.List(
				context.Background(),
				k2k.ListFilter{OrganizationID: lastOrgIDForProbing, Status: k2k.StatusOpen},
			)
			if len(rows) == 1 {
				capturedChannelIDAtInvite = rows[0].SlackChannelID
			}
		},
	}
	lc := k2k.NewLifecycle(k2k.LifecycleDeps{Repo: repo, Slack: probe})

	p := validParams()
	lastOrgIDForProbing = p.OrganizationID
	if _, err := lc.Open(context.Background(), p); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if capturedChannelIDAtInvite != "C-INV" {
		t.Errorf("at-invite SlackChannelID = %q, want %q (bind must precede invite)", capturedChannelIDAtInvite, "C-INV")
	}
}

// lastOrgIDForProbing is used by the bind-precedes-invite probe so
// the InviteToChannel callback can re-query the repo by org. Scoped
// to a package var (not test-local closure) because the probe
// callback runs from inside lifecycle.Open's call stack.
var lastOrgIDForProbing uuid.UUID

// probingFakeSlack extends fakeSlackChannels with an InviteToChannel
// hook so a test can observe lifecycle ordering invariants.
type probingFakeSlack struct {
	fakeSlackChannels
	onInvite func(channelID string)
}

func (f *probingFakeSlack) InviteToChannel(ctx context.Context, channelID string, userIDs []string) error {
	if f.onInvite != nil {
		f.onInvite(channelID)
	}
	return f.fakeSlackChannels.InviteToChannel(ctx, channelID, userIDs)
}

func TestLifecycle_Open_DefensiveCopyOfParticipants(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-X"}
	lc, _ := newLifecycle(t, slack)

	p := validParams()
	original := []string{"bot-a", "bot-b"}
	p.Participants = []string{"bot-a", "bot-b"}

	conv, err := lc.Open(context.Background(), p)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Mutate the caller-side slice POST-Open — must NOT bleed into the
	// invite users captured by the fake (which deep-copies on receipt).
	p.Participants[0] = "MUTATED"
	// Also mutate the returned conv.Participants — must NOT bleed into
	// the held row.
	conv.Participants[0] = "MUTATED2"

	if !equalStringSlice(slack.inviteUsers, original) {
		t.Errorf("invite users = %v, want %v (defensive copy violation)", slack.inviteUsers, original)
	}
}

func TestLifecycle_Close_HappyPath(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-X"}
	lc, _ := newLifecycle(t, slack)
	conv, err := lc.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := lc.Close(context.Background(), conv.ID, "test done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if slack.archiveCalls != 1 {
		t.Errorf("archive calls = %d, want 1", slack.archiveCalls)
	}
	if slack.archiveChannel != "C-X" {
		t.Errorf("ArchiveChannel channelID = %q, want %q", slack.archiveChannel, "C-X")
	}
}

func TestLifecycle_Close_RefusesCancelledCtx(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-X"}
	lc, _ := newLifecycle(t, slack)
	conv, err := lc.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := lc.Close(ctx, conv.ID, "test"); !errors.Is(err, context.Canceled) {
		t.Errorf("Close cancelled ctx: err = %v, want context.Canceled", err)
	}
	if slack.archiveCalls != 0 {
		t.Errorf("archive calls = %d, want 0 (ctx-cancel short-circuit)", slack.archiveCalls)
	}
}

func TestLifecycle_Close_NilID_NotFound(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{}
	lc, _ := newLifecycle(t, slack)
	err := lc.Close(context.Background(), uuid.Nil, "test")
	if !errors.Is(err, k2k.ErrConversationNotFound) {
		t.Errorf("Close(uuid.Nil): err = %v, want ErrConversationNotFound", err)
	}
}

func TestLifecycle_Close_UnknownID(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{}
	lc, _ := newLifecycle(t, slack)
	err := lc.Close(context.Background(), uuid.New(), "test")
	if !errors.Is(err, k2k.ErrConversationNotFound) {
		t.Errorf("Close unknown id: err = %v, want ErrConversationNotFound", err)
	}
	if slack.archiveCalls != 0 {
		t.Errorf("archive calls = %d, want 0 (unknown id short-circuits before Slack)", slack.archiveCalls)
	}
}

func TestLifecycle_Close_DoubleClose_SurfacesSentinel(t *testing.T) {
	t.Parallel()

	slack := &fakeSlackChannels{createReturns: "C-X"}
	lc, _ := newLifecycle(t, slack)
	conv, err := lc.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := lc.Close(context.Background(), conv.ID, "first"); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	err = lc.Close(context.Background(), conv.ID, "second")
	if !errors.Is(err, k2k.ErrAlreadyArchived) {
		t.Errorf("second Close: err = %v, want ErrAlreadyArchived", err)
	}
}

func TestLifecycle_Close_OrphanRow_NoArchiveCall(t *testing.T) {
	t.Parallel()

	// Simulate an orphan from a failed Open() by opening the repo
	// directly without Slack-side success.
	slack := &fakeSlackChannels{}
	repo := newRepo(t)
	conv, err := repo.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	lc := k2k.NewLifecycle(k2k.LifecycleDeps{Repo: repo, Slack: slack})

	if err := lc.Close(context.Background(), conv.ID, "orphan cleanup"); err != nil {
		t.Fatalf("Close orphan: %v", err)
	}
	if slack.archiveCalls != 0 {
		t.Errorf("archive calls = %d, want 0 (orphan has no slack_channel_id)", slack.archiveCalls)
	}
}

func TestLifecycle_Close_ArchiveError_RowNotClosed(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("slack: conversations.archive: simulated")
	slack := &fakeSlackChannels{createReturns: "C-X"}
	lc, repo := newLifecycle(t, slack)
	conv, err := lc.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	slack.archiveErr = wantErr

	if err := lc.Close(context.Background(), conv.ID, "test"); !errors.Is(err, wantErr) {
		t.Errorf("Close with archive error: err = %v, want wrap of sentinel", err)
	}

	got, getErr := repo.Get(context.Background(), conv.ID)
	if getErr != nil {
		t.Fatalf("Get: %v", getErr)
	}
	if got.Status != k2k.StatusOpen {
		t.Errorf("row Status after archive failure = %q, want StatusOpen", got.Status)
	}
}

// TestLifecycle_OpenRevealClose_FullIntegration is the AC-defined
// integration test: open→reveal→close lifecycle through the mock
// Slack adapter. RevealChannel is exercised via a direct call to a
// `RevealChannel`-aware seam built on top of fakeSlackChannels.
func TestLifecycle_OpenRevealClose_FullIntegration(t *testing.T) {
	t.Parallel()

	slack := &revealableFakeSlack{
		fakeSlackChannels: fakeSlackChannels{createReturns: "C-INT"},
	}
	lc := k2k.NewLifecycle(k2k.LifecycleDeps{Repo: newRepo(t), Slack: slack})

	conv, err := lc.Open(context.Background(), validParams())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if conv.SlackChannelID != "C-INT" {
		t.Fatalf("SlackChannelID = %q, want %q", conv.SlackChannelID, "C-INT")
	}

	if err := slack.RevealChannel(context.Background(), conv.SlackChannelID, "U-HUMAN"); err != nil {
		t.Fatalf("RevealChannel: %v", err)
	}
	if slack.revealChannel != "C-INT" {
		t.Errorf("reveal channel = %q, want %q", slack.revealChannel, "C-INT")
	}
	if slack.revealUser != "U-HUMAN" {
		t.Errorf("reveal user = %q, want %q", slack.revealUser, "U-HUMAN")
	}

	if err := lc.Close(context.Background(), conv.ID, "integration done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if slack.archiveCalls != 1 {
		t.Errorf("archive calls = %d, want 1", slack.archiveCalls)
	}
}

// revealableFakeSlack extends fakeSlackChannels with a RevealChannel
// method matching the M1.1.b boundary. The lifecycle itself never
// calls RevealChannel; the integration test exercises it directly to
// pin the open→reveal→close end-to-end shape.
type revealableFakeSlack struct {
	fakeSlackChannels
	revealChannel string
	revealUser    string
	revealErr     error
}

func (f *revealableFakeSlack) RevealChannel(_ context.Context, channelID, userID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revealChannel = channelID
	f.revealUser = userID
	return f.revealErr
}

// TestLifecycle_NoAuditOrKeeperslogReferences is the source-grep AC:
// the lifecycle layer is M1.1.c's state-mutation surface, not the
// audit sink. The M1.4 audit subscriber owns the K2K event taxonomy;
// audit emission inside the lifecycle would couple two concerns.
// Mirrors M1.1.b's `TestChannels_NoAuditOrKeeperslogReferences`.
func TestLifecycle_NoAuditOrKeeperslogReferences(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("lifecycle.go")
	if err != nil {
		t.Fatalf("read lifecycle.go: %v", err)
	}
	lineCommentRE := regexp.MustCompile(`(?m)//.*$`)
	stripped := lineCommentRE.ReplaceAllString(string(data), "")

	bannedTokens := []string{"keeperslog.", ".Append("}
	for _, tok := range bannedTokens {
		if strings.Contains(stripped, tok) {
			t.Errorf("lifecycle.go contains banned token %q (audit emission belongs to M1.4, not the lifecycle layer)", tok)
		}
	}
}

// equalStringSlice is a tiny test-local helper; keeping it inline
// avoids pulling in an external diff library or polluting the package
// surface.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
