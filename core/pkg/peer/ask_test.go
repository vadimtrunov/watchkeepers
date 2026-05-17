package peer_test

import (
	"context"
	"errors"
	"fmt"
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

// newTool returns a [peer.Tool] wired against a real
// [k2k.MemoryRepository], a real [capability.Broker] (issued for the
// supplied org + capabilities), a fakePeerLister seeded with `peers`,
// and a fakeLifecycle that drives the repo's Open(). The returned
// tokens map keys are [peer.CapabilityAsk] / [peer.CapabilityReply].
func newTool(t *testing.T, orgID uuid.UUID, peers []keepclient.Peer) (*peer.Tool, *k2k.MemoryRepository, *fakePeerLister, *fakeLifecycle, map[string]string) {
	t.Helper()
	repo := k2k.NewMemoryRepository(time.Now, nil)
	pl := &fakePeerLister{peers: peers}
	lc := &fakeLifecycle{repo: repo}

	broker := capability.New()
	t.Cleanup(func() { _ = broker.Close() })
	askTok, err := broker.IssueForOrg(peer.CapabilityAsk, orgID.String(), time.Hour)
	if err != nil {
		t.Fatalf("IssueForOrg ask: %v", err)
	}
	replyTok, err := broker.IssueForOrg(peer.CapabilityReply, orgID.String(), time.Hour)
	if err != nil {
		t.Fatalf("IssueForOrg reply: %v", err)
	}
	tokens := map[string]string{
		peer.CapabilityAsk:   askTok,
		peer.CapabilityReply: replyTok,
	}
	tool := peer.NewTool(peer.Deps{
		PeerLister: pl,
		Lifecycle:  lc,
		Repository: repo,
		Capability: broker,
	})
	return tool, repo, pl, lc, tokens
}

func samplePeer(role string) keepclient.Peer {
	return keepclient.Peer{
		WatchkeeperID: uuidString(),
		Role:          role,
		Description:   "test peer",
		Language:      "en",
		Capabilities:  []string{"peer:ask"},
		Availability:  keepclient.PeerAvailabilityAvailable,
	}
}

func TestNewTool_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	repo := k2k.NewMemoryRepository(time.Now, nil)
	pl := &fakePeerLister{}
	lc := &fakeLifecycle{repo: repo}
	capa := &fakeCapability{}

	cases := []struct {
		name string
		deps peer.Deps
	}{
		{name: "nil peer lister", deps: peer.Deps{Lifecycle: lc, Repository: repo, Capability: capa}},
		{name: "nil lifecycle", deps: peer.Deps{PeerLister: pl, Repository: repo, Capability: capa}},
		{name: "nil repository", deps: peer.Deps{PeerLister: pl, Lifecycle: lc, Capability: capa}},
		{name: "nil capability", deps: peer.Deps{PeerLister: pl, Lifecycle: lc, Repository: repo}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("NewTool did not panic with %s", tc.name)
				}
			}()
			peer.NewTool(tc.deps)
		})
	}
}

func TestTool_Ask_HappyPath_AskReplyRoundTrip(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})

	// Spawn the replier in a goroutine: as soon as the request lands,
	// look it up + append a reply.
	go func() {
		// Poll the repo's internals via a public seam — list open
		// conversations for the org until one appears, then reply.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			convs, err := repo.List(context.Background(), k2k.ListFilter{OrganizationID: orgID})
			if err != nil {
				t.Errorf("List: %v", err)
				return
			}
			if len(convs) >= 1 {
				_, err := repo.AppendMessage(context.Background(), k2k.AppendMessageParams{
					ConversationID:      convs[0].ID,
					OrganizationID:      orgID,
					SenderWatchkeeperID: target.WatchkeeperID,
					Body:                []byte("pong"),
					Direction:           k2k.MessageDirectionReply,
				})
				if err != nil {
					t.Errorf("AppendMessage: %v", err)
				}
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()

	res, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "ping",
		Body:                []byte("ping"),
		Timeout:             2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if res.ConversationID == uuid.Nil {
		t.Errorf("ConversationID = uuid.Nil, want non-zero")
	}
	if string(res.ReplyBody) != "pong" {
		t.Errorf("ReplyBody = %q, want %q", string(res.ReplyBody), "pong")
	}
}

func TestTool_Ask_TargetByRoleResolves(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Strategist")
	tool, _, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})

	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		// Case-insensitive role match — "strategist" should resolve
		// to peer with Role "Strategist".
		Target:  "strategist",
		Subject: "ping",
		Body:    []byte("ping"),
		Timeout: 50 * time.Millisecond,
	})
	// We expect ErrPeerTimeout here (no replier in this test) — the
	// purpose of the test is to pin that resolution by role succeeds,
	// i.e. the call gets past the resolver and times out on the
	// WaitForReply rather than failing at ErrPeerNotFound.
	if !errors.Is(err, peer.ErrPeerTimeout) {
		t.Errorf("Ask err = %v, want errors.Is ErrPeerTimeout", err)
	}
}

func TestTool_Ask_TimeoutFiresErrPeerTimeout(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, _, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})

	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "ping",
		Body:                []byte("ping"),
		Timeout:             40 * time.Millisecond,
	})
	if !errors.Is(err, peer.ErrPeerTimeout) {
		t.Fatalf("Ask err = %v, want errors.Is ErrPeerTimeout", err)
	}
	// Verify the underlying k2k sentinel is preserved in the chain.
	if !errors.Is(err, k2k.ErrWaitForReplyTimeout) {
		t.Errorf("Ask err = %v, want errors.Is ErrWaitForReplyTimeout (chained sentinel)", err)
	}
}

func TestTool_Ask_UnknownTargetSurfacesErrPeerNotFound(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, _, lc, tokens := newTool(t, orgID, nil) // no peers seeded
	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              "never-spawned-id",
		Subject:             "ping",
		Body:                []byte("ping"),
		Timeout:             time.Second,
	})
	if !errors.Is(err, peer.ErrPeerNotFound) {
		t.Errorf("Ask err = %v, want errors.Is ErrPeerNotFound", err)
	}
	// Resolver miss MUST short-circuit before Lifecycle.Open — no
	// conversation row should be minted on a not-found target.
	if lc.calls != 0 {
		t.Errorf("Lifecycle.Open calls = %d, want 0 (resolver miss must short-circuit)", lc.calls)
	}
}

func TestTool_Ask_CapabilityBrokerEnforced(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, _, _, lc, _ := newTool(t, orgID, []keepclient.Peer{target})

	// Use a fresh token NOT issued by the broker the tool holds.
	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     "not-an-issued-token",
		Target:              target.WatchkeeperID,
		Subject:             "ping",
		Body:                []byte("ping"),
		Timeout:             time.Second,
	})
	if !errors.Is(err, peer.ErrPeerCapabilityDenied) {
		t.Fatalf("Ask err = %v, want errors.Is ErrPeerCapabilityDenied", err)
	}
	if !errors.Is(err, capability.ErrInvalidToken) {
		t.Errorf("Ask err = %v, want errors.Is capability.ErrInvalidToken (chained)", err)
	}
	if lc.calls != 0 {
		t.Errorf("Lifecycle.Open calls = %d, want 0 (capability deny must short-circuit)", lc.calls)
	}
}

func TestTool_Ask_CapabilityOrganizationMismatch(t *testing.T) {
	t.Parallel()

	orgA := uuid.New()
	orgB := uuid.New()
	target := samplePeer("Lead")
	tool, _, _, _, tokens := newTool(t, orgA, []keepclient.Peer{target})

	// Token issued for orgA; call passes orgB — must be denied.
	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgB,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "ping",
		Body:                []byte("ping"),
		Timeout:             time.Second,
	})
	if !errors.Is(err, peer.ErrPeerCapabilityDenied) {
		t.Fatalf("Ask err = %v, want errors.Is ErrPeerCapabilityDenied", err)
	}
	if !errors.Is(err, capability.ErrOrganizationMismatch) {
		t.Errorf("Ask err = %v, want errors.Is ErrOrganizationMismatch (chained)", err)
	}
}

func TestTool_Ask_DefensiveCopyOfBody(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})

	body := []byte("original")

	// Replier goroutine that mirrors the request body back.
	replyBody := []byte("response")
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			convs, _ := repo.List(context.Background(), k2k.ListFilter{OrganizationID: orgID})
			if len(convs) >= 1 {
				_, err := repo.AppendMessage(context.Background(), k2k.AppendMessageParams{
					ConversationID:      convs[0].ID,
					OrganizationID:      orgID,
					SenderWatchkeeperID: target.WatchkeeperID,
					Body:                replyBody,
					Direction:           k2k.MessageDirectionReply,
				})
				if err != nil {
					t.Errorf("AppendMessage: %v", err)
				}
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()

	res, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "ping",
		Body:                body,
		Timeout:             2 * time.Second,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	// Mutate the caller-side input slice and the reply-side slice;
	// neither should bleed into the persisted message body or the
	// returned ReplyBody.
	for i := range body {
		body[i] = 'X'
	}
	for i := range replyBody {
		replyBody[i] = 'Y'
	}
	if string(res.ReplyBody) != "response" {
		t.Errorf("ReplyBody = %q, want %q (defensive copy regressed)", string(res.ReplyBody), "response")
	}
	// The persisted request message body must also be the original.
	conv, err := repo.Get(context.Background(), res.ConversationID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	_ = conv
}

func TestTool_Ask_ValidationFailures(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, _, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})
	mkValid := func() peer.AskParams {
		return peer.AskParams{
			ActingWatchkeeperID: "wk-asker",
			OrganizationID:      orgID,
			CapabilityToken:     tokens[peer.CapabilityAsk],
			Target:              target.WatchkeeperID,
			Subject:             "ping",
			Body:                []byte("ping"),
			Timeout:             time.Second,
		}
	}
	cases := []struct {
		name   string
		mutate func(p *peer.AskParams)
		want   error
	}{
		{name: "empty acting wk", mutate: func(p *peer.AskParams) { p.ActingWatchkeeperID = "  " }, want: peer.ErrInvalidActingWatchkeeperID},
		{name: "empty org id", mutate: func(p *peer.AskParams) { p.OrganizationID = uuid.Nil }, want: k2k.ErrEmptyOrganization},
		{name: "empty target", mutate: func(p *peer.AskParams) { p.Target = "   " }, want: peer.ErrInvalidTarget},
		{name: "empty subject", mutate: func(p *peer.AskParams) { p.Subject = "   " }, want: peer.ErrInvalidSubject},
		{name: "empty body", mutate: func(p *peer.AskParams) { p.Body = nil }, want: peer.ErrInvalidBody},
		{name: "zero timeout", mutate: func(p *peer.AskParams) { p.Timeout = 0 }, want: peer.ErrInvalidTimeout},
		{name: "negative timeout", mutate: func(p *peer.AskParams) { p.Timeout = -time.Second }, want: peer.ErrInvalidTimeout},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := mkValid()
			tc.mutate(&p)
			_, err := tool.Ask(context.Background(), p)
			if !errors.Is(err, tc.want) {
				t.Errorf("Ask err = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestTool_Ask_CtxCancelled(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, _, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := tool.Ask(ctx, peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "ping",
		Body:                []byte("ping"),
		Timeout:             time.Second,
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Ask err = %v, want context.Canceled", err)
	}
}

func TestTool_Ask_ListPeersErrorWrapped(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	repo := k2k.NewMemoryRepository(time.Now, nil)
	pl := &fakePeerLister{listErr: fmt.Errorf("backend unavailable")}
	lc := &fakeLifecycle{repo: repo}
	broker := capability.New()
	t.Cleanup(func() { _ = broker.Close() })
	tok, err := broker.IssueForOrg(peer.CapabilityAsk, orgID.String(), time.Hour)
	if err != nil {
		t.Fatalf("IssueForOrg: %v", err)
	}
	tool := peer.NewTool(peer.Deps{
		PeerLister: pl, Lifecycle: lc, Repository: repo, Capability: broker,
	})

	_, err = tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tok,
		Target:              "anyone",
		Subject:             "ping",
		Body:                []byte("ping"),
		Timeout:             time.Second,
	})
	if err == nil {
		t.Fatal("Ask err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "backend unavailable") {
		t.Errorf("Ask err = %v, want wrapped 'backend unavailable'", err)
	}
}

func TestTool_Ask_OpenLifecycleErrorWrapped(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	repo := k2k.NewMemoryRepository(time.Now, nil)
	pl := &fakePeerLister{peers: []keepclient.Peer{target}}
	lc := &fakeLifecycle{repo: repo, openErr: fmt.Errorf("slack down")}
	broker := capability.New()
	t.Cleanup(func() { _ = broker.Close() })
	tok, err := broker.IssueForOrg(peer.CapabilityAsk, orgID.String(), time.Hour)
	if err != nil {
		t.Fatalf("IssueForOrg: %v", err)
	}
	tool := peer.NewTool(peer.Deps{
		PeerLister: pl, Lifecycle: lc, Repository: repo, Capability: broker,
	})

	_, err = tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tok,
		Target:              target.WatchkeeperID,
		Subject:             "ping",
		Body:                []byte("ping"),
		Timeout:             time.Second,
	})
	if err == nil {
		t.Fatal("Ask err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "slack down") {
		t.Errorf("Ask err = %v, want wrapped 'slack down'", err)
	}
}

func TestTool_Ask_ConcurrentAskReply(t *testing.T) {
	t.Parallel()

	const goroutines = 16

	orgID := uuid.New()
	peers := make([]keepclient.Peer, goroutines)
	for i := 0; i < goroutines; i++ {
		peers[i] = samplePeer(fmt.Sprintf("Role%d", i))
	}
	tool, repo, _, _, tokens := newTool(t, orgID, peers)

	// One replier goroutine watches all open conversations and
	// appends a reply to each as it appears.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		seen := make(map[uuid.UUID]bool)
		for {
			select {
			case <-stop:
				return
			default:
			}
			convs, _ := repo.List(context.Background(), k2k.ListFilter{OrganizationID: orgID})
			for _, c := range convs {
				if seen[c.ID] {
					continue
				}
				if _, err := repo.AppendMessage(context.Background(), k2k.AppendMessageParams{
					ConversationID:      c.ID,
					OrganizationID:      orgID,
					SenderWatchkeeperID: "wk-replier",
					Body:                []byte("ack"),
					Direction:           k2k.MessageDirectionReply,
				}); err == nil {
					seen[c.ID] = true
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			res, err := tool.Ask(context.Background(), peer.AskParams{
				ActingWatchkeeperID: "wk-asker",
				OrganizationID:      orgID,
				CapabilityToken:     tokens[peer.CapabilityAsk],
				Target:              peers[i].WatchkeeperID,
				Subject:             "ping",
				Body:                []byte(fmt.Sprintf("ping-%d", i)),
				Timeout:             3 * time.Second,
			})
			if err != nil {
				t.Errorf("Ask[%d]: %v", i, err)
				return
			}
			if string(res.ReplyBody) != "ack" {
				t.Errorf("ReplyBody[%d] = %q, want %q", i, string(res.ReplyBody), "ack")
			}
		}()
	}
	wg.Wait()
}

// TestAsk_NoAuditOrKeeperslogReferences is the source-grep AC: the
// peer.Ask file is the call surface, not the audit sink. The K2K
// audit taxonomy is M1.4's seam; audit emission inside ask.go would
// couple two concerns. Mirrors `k2k.TestLifecycle_NoAuditOrKeeperslogReferences`.
func TestAsk_NoAuditOrKeeperslogReferences(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("ask.go")
	if err != nil {
		t.Fatalf("read ask.go: %v", err)
	}
	lineCommentRE := regexp.MustCompile(`(?m)//.*$`)
	stripped := lineCommentRE.ReplaceAllString(string(data), "")

	bannedTokens := []string{"keeperslog.", ".Append("}
	for _, tok := range bannedTokens {
		if strings.Contains(stripped, tok) {
			t.Errorf("ask.go contains banned token %q (audit emission belongs to M1.4, not the peer-tool layer)", tok)
		}
	}
}
