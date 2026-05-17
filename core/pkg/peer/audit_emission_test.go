package peer_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/capability"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k/audit"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/peer"
)

// recordingAuditor is the hand-rolled [audit.Emitter] stand-in the
// peer audit-wiring tests inject. Records every emit call. Distinct
// from the lifecycle's recordingAuditor (which lives in the k2k_test
// package) so each test surface keeps its own minimal fake.
type recordingAuditor struct {
	mu          sync.Mutex
	messageSent []audit.MessageSentEvent
	messageRecv []audit.MessageReceivedEvent
	sentErr     error
	recvErr     error
}

func (r *recordingAuditor) EmitConversationOpened(_ context.Context, _ audit.ConversationOpenedEvent) (string, error) {
	return "", errors.New("recordingAuditor: EmitConversationOpened not used in peer tests")
}

func (r *recordingAuditor) EmitConversationClosed(_ context.Context, _ audit.ConversationClosedEvent) (string, error) {
	return "", errors.New("recordingAuditor: EmitConversationClosed not used in peer tests")
}

func (r *recordingAuditor) EmitMessageSent(_ context.Context, evt audit.MessageSentEvent) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messageSent = append(r.messageSent, evt)
	if r.sentErr != nil {
		return "", r.sentErr
	}
	return "row-sent", nil
}

func (r *recordingAuditor) EmitMessageReceived(_ context.Context, evt audit.MessageReceivedEvent) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.messageRecv = append(r.messageRecv, evt)
	if r.recvErr != nil {
		return "", r.recvErr
	}
	return "row-recv", nil
}

func (r *recordingAuditor) sentSnapshot() []audit.MessageSentEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]audit.MessageSentEvent, len(r.messageSent))
	copy(out, r.messageSent)
	return out
}

func (r *recordingAuditor) recvSnapshot() []audit.MessageReceivedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]audit.MessageReceivedEvent, len(r.messageRecv))
	copy(out, r.messageRecv)
	return out
}

// newToolWithAuditor mirrors newTool() (ask_test.go) but threads the
// supplied [audit.Emitter] into Deps.Auditor so the audit emission
// path is exercised. Hoisted here so the audit-emission tests stay
// scannable without duplicating the broker / repo / fakeLifecycle
// boilerplate.
func newToolWithAuditor(t *testing.T, orgID uuid.UUID, peers []keepclient.Peer, auditor audit.Emitter) (*peer.Tool, *k2k.MemoryRepository, map[string]string) {
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
		Auditor:    auditor,
	})
	return tool, repo, tokens
}

// startReplierAfter spawns a goroutine that polls the repo until a
// conversation appears then appends a `pong` reply on behalf of the
// supplied target. Hoisted so the audit-emission tests stay under
// the gocyclo budget.
func startReplierAfter(t *testing.T, repo *k2k.MemoryRepository, orgID uuid.UUID, target keepclient.Peer) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			convs, _ := repo.List(context.Background(), k2k.ListFilter{OrganizationID: orgID})
			if len(convs) >= 1 {
				_, _ = repo.AppendMessage(context.Background(), k2k.AppendMessageParams{
					ConversationID:      convs[0].ID,
					OrganizationID:      orgID,
					SenderWatchkeeperID: target.WatchkeeperID,
					Body:                []byte("pong"),
					Direction:           k2k.MessageDirectionReply,
				})
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
}

// assertMessageSent checks the closed-set fields the request-side
// MessageSent audit row must carry. Hoisted so the round-trip happy
// path test stays under the gocyclo limit.
func assertMessageSent(t *testing.T, got audit.MessageSentEvent, want audit.MessageSentEvent) {
	t.Helper()
	if got.Direction != want.Direction {
		t.Errorf("MessageSent.Direction = %q, want %q", got.Direction, want.Direction)
	}
	if got.SenderWatchkeeperID != want.SenderWatchkeeperID {
		t.Errorf("MessageSent.Sender = %q, want %q", got.SenderWatchkeeperID, want.SenderWatchkeeperID)
	}
	if got.ConversationID != want.ConversationID {
		t.Errorf("MessageSent.ConversationID = %s, want %s", got.ConversationID, want.ConversationID)
	}
	if got.OrganizationID != want.OrganizationID {
		t.Errorf("MessageSent.OrganizationID = %s, want %s", got.OrganizationID, want.OrganizationID)
	}
	if got.MessageID == uuid.Nil {
		t.Errorf("MessageSent.MessageID is nil")
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("MessageSent.CreatedAt is zero")
	}
}

// assertMessageReceived mirrors assertMessageSent for the
// recipient-side audit row.
func assertMessageReceived(t *testing.T, got audit.MessageReceivedEvent, want audit.MessageReceivedEvent) {
	t.Helper()
	if got.Direction != want.Direction {
		t.Errorf("MessageReceived.Direction = %q, want %q", got.Direction, want.Direction)
	}
	if got.SenderWatchkeeperID != want.SenderWatchkeeperID {
		t.Errorf("MessageReceived.Sender = %q, want %q", got.SenderWatchkeeperID, want.SenderWatchkeeperID)
	}
	if got.RecipientWatchkeeperID != want.RecipientWatchkeeperID {
		t.Errorf("MessageReceived.Recipient = %q, want %q", got.RecipientWatchkeeperID, want.RecipientWatchkeeperID)
	}
	if got.ConversationID != want.ConversationID {
		t.Errorf("MessageReceived.ConversationID = %s, want %s", got.ConversationID, want.ConversationID)
	}
}

func TestTool_Ask_EmitsMessageSentForRequestAppend(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	auditor := &recordingAuditor{}
	tool, repo, tokens := newToolWithAuditor(t, orgID, []keepclient.Peer{target}, auditor)

	startReplierAfter(t, repo, orgID, target)

	res, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "ping",
		Body:                []byte("ping"),
		Timeout:             5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if string(res.ReplyBody) != "pong" {
		t.Errorf("ReplyBody = %q, want pong", string(res.ReplyBody))
	}

	sent := auditor.sentSnapshot()
	if len(sent) != 1 {
		t.Fatalf("MessageSent emits = %d, want 1 (request side)", len(sent))
	}
	assertMessageSent(t, sent[0], audit.MessageSentEvent{
		Direction:           string(k2k.MessageDirectionRequest),
		SenderWatchkeeperID: "wk-asker",
		ConversationID:      res.ConversationID,
		OrganizationID:      orgID,
	})

	recv := auditor.recvSnapshot()
	if len(recv) != 1 {
		t.Fatalf("MessageReceived emits = %d, want 1", len(recv))
	}
	assertMessageReceived(t, recv[0], audit.MessageReceivedEvent{
		Direction:              string(k2k.MessageDirectionReply),
		SenderWatchkeeperID:    target.WatchkeeperID,
		RecipientWatchkeeperID: "wk-asker",
		ConversationID:         res.ConversationID,
	})
}

func TestTool_Ask_NilAuditorIsNoop(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})

	startReplierAfter(t, repo, orgID, target)

	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "ping",
		Body:                []byte("ping"),
		Timeout:             5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Ask with nil Auditor: %v", err)
	}
}

func TestTool_Ask_AuditEmitFailureDoesNotPropagate(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	auditor := &recordingAuditor{sentErr: errors.New("keep down")}
	tool, repo, tokens := newToolWithAuditor(t, orgID, []keepclient.Peer{target}, auditor)

	startReplierAfter(t, repo, orgID, target)

	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "ping",
		Body:                []byte("ping"),
		Timeout:             5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Ask error-propagation when audit fails: %v (expected nil — audit is best-effort)", err)
	}
}

func TestTool_Reply_EmitsMessageSent(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	auditor := &recordingAuditor{}
	tool, repo, tokens := newToolWithAuditor(t, orgID, []keepclient.Peer{target}, auditor)

	// Open a conversation directly via the repository so the test can
	// drive Reply without a round-trip through Ask.
	conv, err := repo.Open(context.Background(), k2k.OpenParams{
		OrganizationID: orgID,
		Participants:   []string{"wk-replier", target.WatchkeeperID},
		Subject:        "test",
	})
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}

	if err := tool.Reply(context.Background(), peer.ReplyParams{
		ActingWatchkeeperID: "wk-replier",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityReply],
		ConversationID:      conv.ID,
		Body:                []byte("done"),
	}); err != nil {
		t.Fatalf("Reply: %v", err)
	}

	sent := auditor.sentSnapshot()
	if len(sent) != 1 {
		t.Fatalf("MessageSent emits = %d, want 1", len(sent))
	}
	if sent[0].Direction != string(k2k.MessageDirectionReply) {
		t.Errorf("MessageSent.Direction = %q, want reply", sent[0].Direction)
	}
	if sent[0].SenderWatchkeeperID != "wk-replier" {
		t.Errorf("MessageSent.Sender = %q, want wk-replier", sent[0].SenderWatchkeeperID)
	}
	if sent[0].ConversationID != conv.ID {
		t.Errorf("MessageSent.ConversationID = %s, want %s", sent[0].ConversationID, conv.ID)
	}
	if sent[0].MessageID == uuid.Nil {
		t.Errorf("MessageSent.MessageID is nil")
	}
	// Reply path emits NO MessageReceived row — the original requester
	// emits that one when its WaitForReply unblocks.
	if got := len(auditor.recvSnapshot()); got != 0 {
		t.Errorf("MessageReceived emits on Reply = %d, want 0", got)
	}
}

func TestTool_Reply_NilAuditorIsNoop(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, _, tokens := newTool(t, orgID, []keepclient.Peer{target})
	conv, err := repo.Open(context.Background(), k2k.OpenParams{
		OrganizationID: orgID,
		Participants:   []string{"wk-replier", target.WatchkeeperID},
		Subject:        "test",
	})
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	if err := tool.Reply(context.Background(), peer.ReplyParams{
		ActingWatchkeeperID: "wk-replier",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityReply],
		ConversationID:      conv.ID,
		Body:                []byte("done"),
	}); err != nil {
		t.Fatalf("Reply with nil Auditor: %v", err)
	}
}

func TestTool_Reply_AuditEmitFailureDoesNotPropagate(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	auditor := &recordingAuditor{sentErr: errors.New("keep down")}
	tool, repo, tokens := newToolWithAuditor(t, orgID, []keepclient.Peer{target}, auditor)
	conv, err := repo.Open(context.Background(), k2k.OpenParams{
		OrganizationID: orgID,
		Participants:   []string{"wk-replier", target.WatchkeeperID},
		Subject:        "test",
	})
	if err != nil {
		t.Fatalf("repo.Open: %v", err)
	}
	if err := tool.Reply(context.Background(), peer.ReplyParams{
		ActingWatchkeeperID: "wk-replier",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityReply],
		ConversationID:      conv.ID,
		Body:                []byte("done"),
	}); err != nil {
		t.Fatalf("Reply error-propagation when audit fails: %v (expected nil — audit is best-effort)", err)
	}
}

// TestPeerAuditorOptional asserts that the Deps.Auditor field is
// optional — the constructor accepts a nil value (no fifth panic
// branch alongside PeerLister / Lifecycle / Repository / Capability).
func TestPeerAuditorOptional(t *testing.T) {
	t.Parallel()

	repo := k2k.NewMemoryRepository(time.Now, nil)
	pl := &fakePeerLister{}
	lc := &fakeLifecycle{repo: repo}
	capa := &fakeCapability{}

	// No panic expected — Auditor is omitted.
	_ = peer.NewTool(peer.Deps{
		PeerLister: pl,
		Lifecycle:  lc,
		Repository: repo,
		Capability: capa,
	})
}
