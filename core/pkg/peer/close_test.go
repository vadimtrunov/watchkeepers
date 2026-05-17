package peer_test

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/capability"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/peer"
)

// mintConversationWithParticipants opens a conversation in the supplied
// repo whose participants slice carries the supplied ids. Hoisted so
// each Close test case stays a one-liner.
func mintConversationWithParticipants(t *testing.T, repo *k2k.MemoryRepository, orgID uuid.UUID, participants []string) k2k.Conversation {
	t.Helper()
	conv, err := repo.Open(context.Background(), k2k.OpenParams{
		OrganizationID: orgID,
		Participants:   participants,
		Subject:        "test",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return conv
}

func TestTool_Close_HappyPath_ArchivesAndPersistsSummary(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, lc, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversationWithParticipants(t, repo, orgID, []string{"wk-asker", target.WatchkeeperID})

	err := tool.Close(context.Background(), peer.CloseParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityClose],
		ConversationID:      conv.ID,
		Summary:             "all done — handed off to Lead",
	})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Lifecycle.Close was called exactly once with the peer.close
	// stable reason code.
	lc.mu.Lock()
	gotCloseCalls := lc.closeCalls
	gotCloseReason := lc.lastClose.reason
	gotCloseID := lc.lastClose.id
	lc.mu.Unlock()
	if gotCloseCalls != 1 {
		t.Errorf("Lifecycle.Close calls = %d, want 1", gotCloseCalls)
	}
	if gotCloseReason != peer.CloseLifecycleReason {
		t.Errorf("Lifecycle.Close reason = %q, want %q", gotCloseReason, peer.CloseLifecycleReason)
	}
	if gotCloseID != conv.ID {
		t.Errorf("Lifecycle.Close id = %s, want %s", gotCloseID, conv.ID)
	}

	// Repository reflects archive + summary.
	persisted, err := repo.Get(context.Background(), conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.Status != k2k.StatusArchived {
		t.Errorf("Status = %q, want %q", persisted.Status, k2k.StatusArchived)
	}
	if persisted.CloseReason != peer.CloseLifecycleReason {
		t.Errorf("CloseReason = %q, want %q", persisted.CloseReason, peer.CloseLifecycleReason)
	}
	if persisted.CloseSummary != "all done — handed off to Lead" {
		t.Errorf("CloseSummary = %q, want %q", persisted.CloseSummary, "all done — handed off to Lead")
	}
}

func TestTool_Close_ValidationFailures(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversationWithParticipants(t, repo, orgID, []string{"wk-asker", target.WatchkeeperID})
	mkValid := func() peer.CloseParams {
		return peer.CloseParams{
			ActingWatchkeeperID: "wk-asker",
			OrganizationID:      orgID,
			CapabilityToken:     tokens[peer.CapabilityClose],
			ConversationID:      conv.ID,
			Summary:             "done",
		}
	}
	cases := []struct {
		name   string
		mutate func(p *peer.CloseParams)
		want   error
	}{
		{name: "empty acting wk", mutate: func(p *peer.CloseParams) { p.ActingWatchkeeperID = "  " }, want: peer.ErrInvalidActingWatchkeeperID},
		{name: "empty org id", mutate: func(p *peer.CloseParams) { p.OrganizationID = uuid.Nil }, want: k2k.ErrEmptyOrganization},
		{name: "empty conversation id", mutate: func(p *peer.CloseParams) { p.ConversationID = uuid.Nil }, want: peer.ErrInvalidConversationID},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := mkValid()
			tc.mutate(&p)
			err := tool.Close(context.Background(), p)
			if !errors.Is(err, tc.want) {
				t.Errorf("Close err = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestTool_Close_CapabilityDenied(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, lc, _ := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversationWithParticipants(t, repo, orgID, []string{"wk-asker", target.WatchkeeperID})

	err := tool.Close(context.Background(), peer.CloseParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     "not-an-issued-token",
		ConversationID:      conv.ID,
		Summary:             "done",
	})
	if !errors.Is(err, peer.ErrPeerCapabilityDenied) {
		t.Fatalf("Close err = %v, want errors.Is ErrPeerCapabilityDenied", err)
	}
	if !errors.Is(err, capability.ErrInvalidToken) {
		t.Errorf("Close err = %v, want errors.Is capability.ErrInvalidToken (chained)", err)
	}

	// A denied capability must not have driven Lifecycle.Close.
	lc.mu.Lock()
	gotCloseCalls := lc.closeCalls
	lc.mu.Unlock()
	if gotCloseCalls != 0 {
		t.Errorf("Lifecycle.Close calls = %d under denied capability, want 0 (fail-fast precedes state mutation)", gotCloseCalls)
	}

	// Conversation still open.
	persisted, err := repo.Get(context.Background(), conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.Status != k2k.StatusOpen {
		t.Errorf("Status = %q, want %q (capability-denied must not mutate state)", persisted.Status, k2k.StatusOpen)
	}
}

func TestTool_Close_UnknownConversation(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, _, _, lc, tokens := newTool(t, orgID, []keepclient.Peer{target})

	err := tool.Close(context.Background(), peer.CloseParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityClose],
		ConversationID:      uuid.New(),
		Summary:             "done",
	})
	if !errors.Is(err, peer.ErrPeerConversationNotFound) {
		t.Fatalf("Close err = %v, want errors.Is ErrPeerConversationNotFound", err)
	}
	if !errors.Is(err, k2k.ErrConversationNotFound) {
		t.Errorf("Close err = %v, want errors.Is k2k.ErrConversationNotFound (chained)", err)
	}

	// Unknown conversation must short-circuit BEFORE Lifecycle.Close.
	lc.mu.Lock()
	gotCloseCalls := lc.closeCalls
	lc.mu.Unlock()
	if gotCloseCalls != 0 {
		t.Errorf("Lifecycle.Close calls = %d for unknown conversation, want 0", gotCloseCalls)
	}
}

func TestTool_Close_NonParticipantRejected(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, lc, tokens := newTool(t, orgID, []keepclient.Peer{target})
	// Conversation participants do NOT include "wk-outsider".
	conv := mintConversationWithParticipants(t, repo, orgID, []string{"wk-asker", target.WatchkeeperID})

	err := tool.Close(context.Background(), peer.CloseParams{
		ActingWatchkeeperID: "wk-outsider",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityClose],
		ConversationID:      conv.ID,
		Summary:             "trying to muscle in",
	})
	if !errors.Is(err, peer.ErrPeerClosePermission) {
		t.Fatalf("Close err = %v, want errors.Is ErrPeerClosePermission", err)
	}

	// The error message must NOT leak the participants list (PII /
	// information-disclosure boundary). It carries the conversation id
	// only.
	if msg := err.Error(); strings.Contains(msg, "wk-asker") || strings.Contains(msg, target.WatchkeeperID) {
		t.Errorf("Close err message %q leaks participant ids; should carry conversation id only", msg)
	}

	// Non-participant rejection must short-circuit BEFORE Lifecycle.Close.
	lc.mu.Lock()
	gotCloseCalls := lc.closeCalls
	lc.mu.Unlock()
	if gotCloseCalls != 0 {
		t.Errorf("Lifecycle.Close calls = %d for non-participant, want 0", gotCloseCalls)
	}

	// Conversation still open.
	persisted, err := repo.Get(context.Background(), conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.Status != k2k.StatusOpen {
		t.Errorf("Status = %q, want %q (non-participant must not mutate state)", persisted.Status, k2k.StatusOpen)
	}
}

func TestTool_Close_IdempotentDoubleClose_PreservesFirstSummary(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, lc, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversationWithParticipants(t, repo, orgID, []string{"wk-asker", target.WatchkeeperID})

	// First close — load-bearing summary.
	if err := tool.Close(context.Background(), peer.CloseParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityClose],
		ConversationID:      conv.ID,
		Summary:             "first summary",
	}); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	// Second close — must return nil AND must not overwrite the first
	// summary. The second close's summary is intentionally different so
	// the assertion catches an accidental overwrite.
	if err := tool.Close(context.Background(), peer.CloseParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityClose],
		ConversationID:      conv.ID,
		Summary:             "second summary",
	}); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	persisted, err := repo.Get(context.Background(), conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.Status != k2k.StatusArchived {
		t.Errorf("Status = %q, want %q after double close", persisted.Status, k2k.StatusArchived)
	}
	if persisted.CloseSummary != "first summary" {
		t.Errorf("CloseSummary = %q, want %q (first summary must be preserved across idempotent double close)", persisted.CloseSummary, "first summary")
	}

	// Lifecycle.Close should have been called exactly once — the second
	// close observed the archived row via Get and short-circuited.
	lc.mu.Lock()
	gotCloseCalls := lc.closeCalls
	lc.mu.Unlock()
	if gotCloseCalls != 1 {
		t.Errorf("Lifecycle.Close calls = %d across double close, want 1 (idempotent short-circuit)", gotCloseCalls)
	}
}

func TestTool_Close_IdempotentOnAlreadyArchivedRace(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, lc, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversationWithParticipants(t, repo, orgID, []string{"wk-asker", target.WatchkeeperID})

	// Inject ErrAlreadyArchived to simulate the Get-observed-open ->
	// Lifecycle.Close-lost-the-race interleaving. The repo row is still
	// open at Get time (race not yet won by the concurrent close) but
	// Lifecycle.Close returns ErrAlreadyArchived as if a concurrent
	// close beat us.
	lc.mu.Lock()
	lc.closeErr = k2k.ErrAlreadyArchived
	lc.mu.Unlock()

	err := tool.Close(context.Background(), peer.CloseParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityClose],
		ConversationID:      conv.ID,
		Summary:             "concurrent-loser",
	})
	if err != nil {
		t.Fatalf("Close: %v, want nil (idempotent on ErrAlreadyArchived from Lifecycle)", err)
	}

	// The race-loser must not have written its summary (the race-winner
	// owns the persisted summary). Verify via the underlying repo: the
	// row is still open (because the fake returned ErrAlreadyArchived
	// without driving the repo) but SetCloseSummary was not called.
	persisted, err := repo.Get(context.Background(), conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.CloseSummary != "" {
		t.Errorf("CloseSummary = %q, want \"\" (race-loser must not write summary)", persisted.CloseSummary)
	}
}

func TestTool_Close_CtxCancelled(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversationWithParticipants(t, repo, orgID, []string{"wk-asker", target.WatchkeeperID})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := tool.Close(ctx, peer.CloseParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityClose],
		ConversationID:      conv.ID,
		Summary:             "done",
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Close err = %v, want context.Canceled", err)
	}
}

func TestTool_Close_EmptySummaryAllowed(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversationWithParticipants(t, repo, orgID, []string{"wk-asker", target.WatchkeeperID})

	if err := tool.Close(context.Background(), peer.CloseParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityClose],
		ConversationID:      conv.ID,
		Summary:             "",
	}); err != nil {
		t.Fatalf("Close (empty summary): %v", err)
	}
	persisted, err := repo.Get(context.Background(), conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.Status != k2k.StatusArchived {
		t.Errorf("Status = %q, want %q", persisted.Status, k2k.StatusArchived)
	}
	if persisted.CloseSummary != "" {
		t.Errorf("CloseSummary = %q, want \"\" (empty summary forwarded verbatim)", persisted.CloseSummary)
	}
}

func TestTool_Close_LifecycleErrorWraps(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, lc, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversationWithParticipants(t, repo, orgID, []string{"wk-asker", target.WatchkeeperID})

	sentinel := errors.New("slack: archive failed")
	lc.mu.Lock()
	lc.closeErr = sentinel
	lc.mu.Unlock()

	err := tool.Close(context.Background(), peer.CloseParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityClose],
		ConversationID:      conv.ID,
		Summary:             "done",
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Close err = %v, want errors.Is sentinel", err)
	}

	// Conversation must still be open (the fake's closeErr short-
	// circuited before driving the repo). Summary must not have been
	// written.
	persisted, perr := repo.Get(context.Background(), conv.ID)
	if perr != nil {
		t.Fatalf("Get: %v", perr)
	}
	if persisted.Status != k2k.StatusOpen {
		t.Errorf("Status = %q, want %q after lifecycle error", persisted.Status, k2k.StatusOpen)
	}
	if persisted.CloseSummary != "" {
		t.Errorf("CloseSummary = %q, want \"\" after lifecycle error", persisted.CloseSummary)
	}
}

func TestTool_Close_OrgMismatchOnCapability(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	otherOrg := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversationWithParticipants(t, repo, orgID, []string{"wk-asker", target.WatchkeeperID})

	err := tool.Close(context.Background(), peer.CloseParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      otherOrg,
		CapabilityToken:     tokens[peer.CapabilityClose], // bound to orgID, not otherOrg
		ConversationID:      conv.ID,
		Summary:             "done",
	})
	if !errors.Is(err, peer.ErrPeerCapabilityDenied) {
		t.Fatalf("Close err = %v, want errors.Is ErrPeerCapabilityDenied", err)
	}
	if !errors.Is(err, capability.ErrOrganizationMismatch) {
		t.Errorf("Close err = %v, want errors.Is capability.ErrOrganizationMismatch (chained)", err)
	}
}

// TestTool_Close_Concurrency_DoubleCloseRace pins idempotent behaviour
// under concurrent close attempts: 16 goroutines racing on the same
// conversation must all return nil and the first close's summary
// must win the persisted state.
func TestTool_Close_Concurrency_DoubleCloseRace(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversationWithParticipants(t, repo, orgID, []string{"wk-asker", target.WatchkeeperID})

	const n = 16
	var (
		wg      sync.WaitGroup
		errs    [n]error
		summary = "concurrent close"
	)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			errs[i] = tool.Close(context.Background(), peer.CloseParams{
				ActingWatchkeeperID: "wk-asker",
				OrganizationID:      orgID,
				CapabilityToken:     tokens[peer.CapabilityClose],
				ConversationID:      conv.ID,
				Summary:             summary,
			})
		}()
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("goroutine %d: Close = %v, want nil", i, e)
		}
	}
	persisted, err := repo.Get(context.Background(), conv.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if persisted.Status != k2k.StatusArchived {
		t.Errorf("Status = %q, want %q after concurrent double close", persisted.Status, k2k.StatusArchived)
	}
	if persisted.CloseSummary != summary {
		t.Errorf("CloseSummary = %q, want %q (one of the concurrent calls won)", persisted.CloseSummary, summary)
	}
}

// TestClose_NoAuditOrKeeperslogReferences is the source-grep AC for
// close.go. Same shape as the ask.go / reply.go counterparts.
func TestClose_NoAuditOrKeeperslogReferences(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("close.go")
	if err != nil {
		t.Fatalf("read close.go: %v", err)
	}
	lineCommentRE := regexp.MustCompile(`(?m)//.*$`)
	stripped := lineCommentRE.ReplaceAllString(string(data), "")

	bannedTokens := []string{"keeperslog.", ".Append("}
	for _, tok := range bannedTokens {
		if strings.Contains(stripped, tok) {
			t.Errorf("close.go contains banned token %q (audit emission belongs to M1.4, not the peer-tool layer)", tok)
		}
	}
}

func TestBuiltinCloseManifest_ShapeAndValidate(t *testing.T) {
	t.Parallel()

	m := peer.BuiltinCloseManifest()
	if m.Name != "peer.close" {
		t.Errorf("Name = %q, want %q", m.Name, "peer.close")
	}
	if m.Source != peer.BuiltinSourceName {
		t.Errorf("Source = %q, want %q", m.Source, peer.BuiltinSourceName)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != peer.CapabilityClose {
		t.Errorf("Capabilities = %v, want [%q]", m.Capabilities, peer.CapabilityClose)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestBuiltinCloseManifest_DefensiveDeepCopyOfSchema(t *testing.T) {
	t.Parallel()

	a := peer.BuiltinCloseManifest()
	b := peer.BuiltinCloseManifest()
	if len(a.Schema) == 0 {
		t.Fatal("Schema empty")
	}
	a.Schema[0] = 'X'
	if b.Schema[0] == 'X' {
		t.Error("Schema buffer shared across BuiltinCloseManifest calls (defensive copy regressed)")
	}
}

// TestTool_Close_GetErrorOtherThanNotFoundWraps ensures non-NotFound
// errors from Repository.Get are wrapped verbatim rather than translated
// to ErrPeerConversationNotFound.
func TestTool_Close_GetErrorOtherThanNotFoundWraps(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	// Build a tool with a Repository fake that returns a generic Get
	// error. We can't reuse newTool because it wires the in-memory
	// repository as both Repository AND fakeLifecycle's repo; here we
	// need a custom error-injecting repo. Hand-build the deps from
	// scratch with an error-injecting fakeRepo wrapper.
	repoBacking := k2k.NewMemoryRepository(time.Now, nil)
	sentinel := errors.New("repo: storage offline")
	er := &errInjectingRepo{Repository: repoBacking, getErr: sentinel}

	broker := capability.New()
	t.Cleanup(func() { _ = broker.Close() })
	closeTok, err := broker.IssueForOrg(peer.CapabilityClose, orgID.String(), time.Hour)
	if err != nil {
		t.Fatalf("IssueForOrg close: %v", err)
	}

	pl := &fakePeerLister{peers: []keepclient.Peer{target}}
	lc := &fakeLifecycle{repo: repoBacking}

	tool := peer.NewTool(peer.Deps{
		PeerLister: pl,
		Lifecycle:  lc,
		Repository: er,
		Capability: broker,
	})

	err = tool.Close(context.Background(), peer.CloseParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     closeTok,
		ConversationID:      uuid.New(),
		Summary:             "done",
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Close err = %v, want errors.Is sentinel", err)
	}
	if errors.Is(err, peer.ErrPeerConversationNotFound) {
		t.Errorf("Close err = %v, must NOT be translated to ErrPeerConversationNotFound for non-NotFound Get error", err)
	}
}

// errInjectingRepo wraps a k2k.Repository and lets tests inject an
// error on Get without re-implementing every other method.
type errInjectingRepo struct {
	k2k.Repository
	getErr error
}

func (e *errInjectingRepo) Get(ctx context.Context, id uuid.UUID) (k2k.Conversation, error) {
	if e.getErr != nil {
		return k2k.Conversation{}, e.getErr
	}
	return e.Repository.Get(ctx, id)
}
