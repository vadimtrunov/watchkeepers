package peer_test

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/capability"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/peer"
)

// mintConversation opens a conversation in the supplied repo so
// reply tests can target it without going through the whole `Ask`
// flow.
func mintConversation(t *testing.T, repo *k2k.MemoryRepository, orgID uuid.UUID) k2k.Conversation {
	t.Helper()
	conv, err := repo.Open(context.Background(), k2k.OpenParams{
		OrganizationID: orgID,
		Participants:   []string{"bot-a", "bot-b"},
		Subject:        "test",
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return conv
}

func TestTool_Reply_HappyPath(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversation(t, repo, orgID)

	err := tool.Reply(context.Background(), peer.ReplyParams{
		ActingWatchkeeperID: "wk-replier",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityReply],
		ConversationID:      conv.ID,
		Body:                []byte("pong"),
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	// The reply must have landed in the repo and be visible to
	// WaitForReply.
	msg, err := repo.WaitForReply(context.Background(), conv.ID, time.Time{}, time.Second)
	if err != nil {
		t.Fatalf("WaitForReply: %v", err)
	}
	if string(msg.Body) != "pong" {
		t.Errorf("Body = %q, want %q", string(msg.Body), "pong")
	}
}

func TestTool_Reply_ValidationFailures(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversation(t, repo, orgID)
	mkValid := func() peer.ReplyParams {
		return peer.ReplyParams{
			ActingWatchkeeperID: "wk-replier",
			OrganizationID:      orgID,
			CapabilityToken:     tokens[peer.CapabilityReply],
			ConversationID:      conv.ID,
			Body:                []byte("pong"),
		}
	}
	cases := []struct {
		name   string
		mutate func(p *peer.ReplyParams)
		want   error
	}{
		{name: "empty acting wk", mutate: func(p *peer.ReplyParams) { p.ActingWatchkeeperID = "  " }, want: peer.ErrInvalidActingWatchkeeperID},
		{name: "empty org id", mutate: func(p *peer.ReplyParams) { p.OrganizationID = uuid.Nil }, want: k2k.ErrEmptyOrganization},
		{name: "empty conversation id", mutate: func(p *peer.ReplyParams) { p.ConversationID = uuid.Nil }, want: peer.ErrInvalidConversationID},
		{name: "empty body", mutate: func(p *peer.ReplyParams) { p.Body = nil }, want: peer.ErrInvalidBody},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := mkValid()
			tc.mutate(&p)
			err := tool.Reply(context.Background(), p)
			if !errors.Is(err, tc.want) {
				t.Errorf("Reply err = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestTool_Reply_CapabilityDenied(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, _ := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversation(t, repo, orgID)

	err := tool.Reply(context.Background(), peer.ReplyParams{
		ActingWatchkeeperID: "wk-replier",
		OrganizationID:      orgID,
		CapabilityToken:     "not-an-issued-token",
		ConversationID:      conv.ID,
		Body:                []byte("pong"),
	})
	if !errors.Is(err, peer.ErrPeerCapabilityDenied) {
		t.Fatalf("Reply err = %v, want errors.Is ErrPeerCapabilityDenied", err)
	}
	if !errors.Is(err, capability.ErrInvalidToken) {
		t.Errorf("Reply err = %v, want errors.Is capability.ErrInvalidToken (chained)", err)
	}
}

func TestTool_Reply_UnknownConversation(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, _, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})

	err := tool.Reply(context.Background(), peer.ReplyParams{
		ActingWatchkeeperID: "wk-replier",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityReply],
		ConversationID:      uuid.New(),
		Body:                []byte("pong"),
	})
	if !errors.Is(err, peer.ErrPeerConversationNotFound) {
		t.Fatalf("Reply err = %v, want errors.Is ErrPeerConversationNotFound", err)
	}
	if !errors.Is(err, k2k.ErrConversationNotFound) {
		t.Errorf("Reply err = %v, want errors.Is k2k.ErrConversationNotFound (chained)", err)
	}
}

func TestTool_Reply_ArchivedConversation(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversation(t, repo, orgID)
	if err := repo.Close(context.Background(), conv.ID, "done"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := tool.Reply(context.Background(), peer.ReplyParams{
		ActingWatchkeeperID: "wk-replier",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityReply],
		ConversationID:      conv.ID,
		Body:                []byte("pong"),
	})
	if !errors.Is(err, peer.ErrPeerConversationClosed) {
		t.Errorf("Reply err = %v, want errors.Is ErrPeerConversationClosed", err)
	}
}

func TestTool_Reply_CtxCancelled(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversation(t, repo, orgID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := tool.Reply(ctx, peer.ReplyParams{
		ActingWatchkeeperID: "wk-replier",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityReply],
		ConversationID:      conv.ID,
		Body:                []byte("pong"),
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Reply err = %v, want context.Canceled", err)
	}
}

func TestTool_Reply_DefensiveCopyOfBody(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv := mintConversation(t, repo, orgID)

	body := []byte("response")
	err := tool.Reply(context.Background(), peer.ReplyParams{
		ActingWatchkeeperID: "wk-replier",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityReply],
		ConversationID:      conv.ID,
		Body:                body,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	// Mutate caller-side buffer; the persisted message body should not
	// observe the mutation.
	for i := range body {
		body[i] = 'X'
	}
	msg, err := repo.WaitForReply(context.Background(), conv.ID, time.Time{}, time.Second)
	if err != nil {
		t.Fatalf("WaitForReply: %v", err)
	}
	if string(msg.Body) != "response" {
		t.Errorf("persisted Body = %q, want %q (defensive copy regressed)", string(msg.Body), "response")
	}
}

// TestReply_NoAuditOrKeeperslogReferences is the source-grep AC for
// reply.go. Same shape as the ask.go counterpart.
func TestReply_NoAuditOrKeeperslogReferences(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("reply.go")
	if err != nil {
		t.Fatalf("read reply.go: %v", err)
	}
	lineCommentRE := regexp.MustCompile(`(?m)//.*$`)
	stripped := lineCommentRE.ReplaceAllString(string(data), "")

	bannedTokens := []string{"keeperslog.", ".Append("}
	for _, tok := range bannedTokens {
		if strings.Contains(stripped, tok) {
			t.Errorf("reply.go contains banned token %q (audit emission belongs to M1.4, not the peer-tool layer)", tok)
		}
	}
}
