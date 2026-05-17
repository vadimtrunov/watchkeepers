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
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k/budget"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/peer"
)

// recordingBudgetEnforcer is the hand-rolled [budget.Enforcer] stand-in
// the peer-tool budget-integration tests inject. Records every Charge
// call so the test can assert the parameters the peer-tool layer
// forwards.
type recordingBudgetEnforcer struct {
	mu      sync.Mutex
	calls   []budget.ChargeParams
	result  budget.ChargeResult
	err     error
	overBud bool
}

func (r *recordingBudgetEnforcer) Charge(_ context.Context, params budget.ChargeParams) (budget.ChargeResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, params)
	if r.err != nil {
		return budget.ChargeResult{}, r.err
	}
	res := r.result
	if res.TokensUsed == 0 {
		res.TokensUsed = params.Delta
	}
	if res.TokenBudget == 0 {
		res.TokenBudget = params.TokenBudget
	}
	res.OverBudget = r.overBud
	return res, nil
}

func (r *recordingBudgetEnforcer) snapshot() []budget.ChargeParams {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]budget.ChargeParams, len(r.calls))
	copy(out, r.calls)
	return out
}

// newToolWithBudget mirrors newTool() (ask_test.go) but threads the
// supplied [budget.Enforcer] + optional [peer.TokenBudgetResolver] into
// the constructor so the budget integration is exercised.
func newToolWithBudget(t *testing.T, orgID uuid.UUID, peers []keepclient.Peer, enforcer budget.Enforcer, resolver peer.TokenBudgetResolver) (*peer.Tool, *k2k.MemoryRepository, *fakeLifecycle, map[string]string) {
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
		PeerLister:          pl,
		Lifecycle:           lc,
		Repository:          repo,
		Capability:          broker,
		Budget:              enforcer,
		TokenBudgetResolver: resolver,
	})
	return tool, repo, lc, tokens
}

func TestTool_Ask_ChargesBudgetAfterAppendMessage(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	target := samplePeer("Lead")
	enforcer := &recordingBudgetEnforcer{}
	tool, repo, lc, tokens := newToolWithBudget(t, orgID, []keepclient.Peer{target}, enforcer, nil)

	body := []byte("ask body with sixteen bytes!!!!") // 32 bytes ~ 8 tokens estimate
	startReplierAfter(t, repo, orgID, target)

	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "test",
		Body:                body,
		Timeout:             500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}

	calls := enforcer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("Charge called %d times, want 1 (Ask charges request body)", len(calls))
	}
	if calls[0].OrganizationID != orgID {
		t.Errorf("charge.OrganizationID = %s, want %s", calls[0].OrganizationID, orgID)
	}
	if calls[0].ActingWatchkeeperID != "wk-asker" {
		t.Errorf("charge.ActingWatchkeeperID = %q, want %q", calls[0].ActingWatchkeeperID, "wk-asker")
	}
	wantDelta := budget.EstimateTokensFromBody(body)
	if calls[0].Delta != wantDelta {
		t.Errorf("charge.Delta = %d, want %d", calls[0].Delta, wantDelta)
	}
	if calls[0].TokenBudget != budget.DefaultTokenBudget {
		t.Errorf("charge.TokenBudget = %d, want default %d (no override resolver)", calls[0].TokenBudget, budget.DefaultTokenBudget)
	}
	// Conversation row carries the stamped TokenBudget.
	if lc.lastOpen.TokenBudget != budget.DefaultTokenBudget {
		t.Errorf("lifecycle.Open(TokenBudget=%d), want default %d", lc.lastOpen.TokenBudget, budget.DefaultTokenBudget)
	}
}

func TestTool_Ask_NilBudget_NoCharge(t *testing.T) {
	t.Parallel()
	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, tokens := newToolWithBudget(t, orgID, []keepclient.Peer{target}, nil, nil)

	startReplierAfter(t, repo, orgID, target)

	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "test",
		Body:                []byte("hello"),
		Timeout:             500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
}

func TestTool_Ask_ResolverOverrideAppliedToOpenAndCharge(t *testing.T) {
	t.Parallel()
	orgID := uuid.New()
	target := samplePeer("Lead")
	enforcer := &recordingBudgetEnforcer{}
	resolver := peer.TokenBudgetResolver(func(_ context.Context, actingID string, _ uuid.UUID) (int64, bool, error) {
		if actingID == "wk-asker" {
			return 50, true, nil
		}
		return 0, false, nil
	})
	tool, repo, lc, tokens := newToolWithBudget(t, orgID, []keepclient.Peer{target}, enforcer, resolver)

	startReplierAfter(t, repo, orgID, target)

	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "test",
		Body:                []byte("hi"),
		Timeout:             500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if lc.lastOpen.TokenBudget != 50 {
		t.Errorf("lifecycle.Open(TokenBudget=%d), want 50 (resolver override)", lc.lastOpen.TokenBudget)
	}
	calls := enforcer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("Charge calls = %d, want 1", len(calls))
	}
	if calls[0].TokenBudget != 50 {
		t.Errorf("charge.TokenBudget = %d, want 50 (resolver override)", calls[0].TokenBudget)
	}
}

func TestTool_Ask_ResolverNotOk_FallsBackToDefault(t *testing.T) {
	t.Parallel()
	orgID := uuid.New()
	target := samplePeer("Lead")
	enforcer := &recordingBudgetEnforcer{}
	resolver := peer.TokenBudgetResolver(func(_ context.Context, _ string, _ uuid.UUID) (int64, bool, error) {
		return 0, false, nil // no override declared
	})
	tool, repo, lc, tokens := newToolWithBudget(t, orgID, []keepclient.Peer{target}, enforcer, resolver)
	startReplierAfter(t, repo, orgID, target)

	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "test",
		Body:                []byte("hi"),
		Timeout:             500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if lc.lastOpen.TokenBudget != budget.DefaultTokenBudget {
		t.Errorf("lifecycle.Open(TokenBudget=%d), want default %d", lc.lastOpen.TokenBudget, budget.DefaultTokenBudget)
	}
}

func TestTool_Ask_ResolverExplicitZero_DisablesEnforcement(t *testing.T) {
	t.Parallel()
	orgID := uuid.New()
	target := samplePeer("Lead")
	enforcer := &recordingBudgetEnforcer{}
	resolver := peer.TokenBudgetResolver(func(_ context.Context, _ string, _ uuid.UUID) (int64, bool, error) {
		return 0, true, nil // explicit override to 0
	})
	tool, repo, lc, tokens := newToolWithBudget(t, orgID, []keepclient.Peer{target}, enforcer, resolver)
	startReplierAfter(t, repo, orgID, target)

	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "test",
		Body:                []byte("hi"),
		Timeout:             500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if lc.lastOpen.TokenBudget != 0 {
		t.Errorf("lifecycle.Open(TokenBudget=%d), want 0 (explicit-zero override)", lc.lastOpen.TokenBudget)
	}
}

func TestTool_Ask_ResolverError_ShortCircuits(t *testing.T) {
	t.Parallel()
	orgID := uuid.New()
	target := samplePeer("Lead")
	enforcer := &recordingBudgetEnforcer{}
	resolverErr := errors.New("manifest unreachable")
	resolver := peer.TokenBudgetResolver(func(_ context.Context, _ string, _ uuid.UUID) (int64, bool, error) {
		return 0, false, resolverErr
	})
	tool, _, lc, tokens := newToolWithBudget(t, orgID, []keepclient.Peer{target}, enforcer, resolver)

	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "test",
		Body:                []byte("hi"),
		Timeout:             500 * time.Millisecond,
	})
	if !errors.Is(err, resolverErr) {
		t.Errorf("Ask err = %v, want errors.Is resolverErr", err)
	}
	if lc.calls != 0 {
		t.Errorf("lifecycle.Open called %d times, want 0 (resolver error must short-circuit before Open)", lc.calls)
	}
	if len(enforcer.snapshot()) != 0 {
		t.Errorf("Charge called %d times, want 0", len(enforcer.snapshot()))
	}
}

func TestTool_Reply_ChargesBudgetAfterAppend(t *testing.T) {
	t.Parallel()
	orgID := uuid.New()
	target := samplePeer("Lead")
	enforcer := &recordingBudgetEnforcer{}
	tool, repo, _, tokens := newToolWithBudget(t, orgID, []keepclient.Peer{target}, enforcer, nil)

	// Seed an open conversation with a known TokenBudget so Reply can
	// forward it into the charge.
	conv, err := repo.Open(context.Background(), k2k.OpenParams{
		OrganizationID: orgID,
		Participants:   []string{"wk-asker", target.WatchkeeperID},
		Subject:        "test",
		TokenBudget:    250,
	})
	if err != nil {
		t.Fatalf("seed Open: %v", err)
	}

	body := []byte("reply body!!!!") // 14 bytes -> ceil(14/4) = 4 tokens estimate
	err = tool.Reply(context.Background(), peer.ReplyParams{
		ActingWatchkeeperID: target.WatchkeeperID,
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityReply],
		ConversationID:      conv.ID,
		Body:                body,
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	calls := enforcer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("Charge calls = %d, want 1", len(calls))
	}
	if calls[0].ConversationID != conv.ID {
		t.Errorf("charge.ConversationID = %s, want %s", calls[0].ConversationID, conv.ID)
	}
	if calls[0].TokenBudget != 250 {
		t.Errorf("charge.TokenBudget = %d, want 250 (from row)", calls[0].TokenBudget)
	}
	wantDelta := budget.EstimateTokensFromBody(body)
	if calls[0].Delta != wantDelta {
		t.Errorf("charge.Delta = %d, want %d", calls[0].Delta, wantDelta)
	}
	if calls[0].ActingWatchkeeperID != target.WatchkeeperID {
		t.Errorf("charge.ActingWatchkeeperID = %q, want %q", calls[0].ActingWatchkeeperID, target.WatchkeeperID)
	}
}

func TestTool_Reply_NilBudget_NoCharge(t *testing.T) {
	t.Parallel()
	orgID := uuid.New()
	target := samplePeer("Lead")
	tool, repo, _, tokens := newToolWithBudget(t, orgID, []keepclient.Peer{target}, nil, nil)
	conv, err := repo.Open(context.Background(), k2k.OpenParams{
		OrganizationID: orgID,
		Participants:   []string{"wk-asker", target.WatchkeeperID},
		Subject:        "test",
		TokenBudget:    100,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	err = tool.Reply(context.Background(), peer.ReplyParams{
		ActingWatchkeeperID: target.WatchkeeperID,
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityReply],
		ConversationID:      conv.ID,
		Body:                []byte("ok"),
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
}

// TestTool_Ask_ChargeFailure_DoesNotPropagate pins the no-propagate
// contract: a budget Charge failure must NOT make the peer.Ask return
// an error — the persisted message + the conversation row are the
// load-bearing surfaces.
func TestTool_Ask_ChargeFailure_DoesNotPropagate(t *testing.T) {
	t.Parallel()
	orgID := uuid.New()
	target := samplePeer("Lead")
	enforcer := &recordingBudgetEnforcer{err: errors.New("charge boom")}
	tool, repo, _, tokens := newToolWithBudget(t, orgID, []keepclient.Peer{target}, enforcer, nil)
	startReplierAfter(t, repo, orgID, target)

	_, err := tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "test",
		Body:                []byte("hi"),
		Timeout:             500 * time.Millisecond,
	})
	if err != nil {
		t.Errorf("Ask err = %v, want nil (charge failure must NOT propagate)", err)
	}
}

// TestTool_Reply_ChargeFailure_DoesNotPropagate mirrors the Ask-side
// no-propagate contract for the Reply call.
func TestTool_Reply_ChargeFailure_DoesNotPropagate(t *testing.T) {
	t.Parallel()
	orgID := uuid.New()
	target := samplePeer("Lead")
	enforcer := &recordingBudgetEnforcer{err: errors.New("charge boom")}
	tool, repo, _, tokens := newToolWithBudget(t, orgID, []keepclient.Peer{target}, enforcer, nil)
	conv, err := repo.Open(context.Background(), k2k.OpenParams{
		OrganizationID: orgID,
		Participants:   []string{"wk-asker", target.WatchkeeperID},
		Subject:        "test",
		TokenBudget:    100,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	err = tool.Reply(context.Background(), peer.ReplyParams{
		ActingWatchkeeperID: target.WatchkeeperID,
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityReply],
		ConversationID:      conv.ID,
		Body:                []byte("hello"),
	})
	if err != nil {
		t.Errorf("Reply err = %v, want nil (charge failure must NOT propagate)", err)
	}
}

// TestTool_Reply_ChargeForwardsCorrelationID pins the iter-1 codex P2
// fix: when a reply causes an over-budget crossing, the resulting
// trigger payload carries the conversation's persisted
// `CorrelationID` (and not uuid.Nil) so the escalation can correlate
// back to the originating watch order / saga.
func TestTool_Reply_ChargeForwardsCorrelationID(t *testing.T) {
	t.Parallel()
	orgID := uuid.New()
	corrID := uuid.New()
	target := samplePeer("Lead")
	enforcer := &recordingBudgetEnforcer{}
	tool, repo, _, tokens := newToolWithBudget(t, orgID, []keepclient.Peer{target}, enforcer, nil)
	conv, err := repo.Open(context.Background(), k2k.OpenParams{
		OrganizationID: orgID,
		Participants:   []string{"wk-asker", target.WatchkeeperID},
		Subject:        "test",
		TokenBudget:    250,
		CorrelationID:  corrID,
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	err = tool.Reply(context.Background(), peer.ReplyParams{
		ActingWatchkeeperID: target.WatchkeeperID,
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityReply],
		ConversationID:      conv.ID,
		Body:                []byte("ok"),
	})
	if err != nil {
		t.Fatalf("Reply: %v", err)
	}
	calls := enforcer.snapshot()
	if len(calls) != 1 {
		t.Fatalf("Charge calls = %d, want 1", len(calls))
	}
	if calls[0].CorrelationID != corrID {
		t.Errorf("charge.CorrelationID = %s, want %s (forwarded from conv)", calls[0].CorrelationID, corrID)
	}
}

// TestTool_Ask_ChargeRunsDetachedAfterCallerCancel pins the iter-1
// codex P1 Major fix: peer.Ask calls Charge under a detached ctx so a
// caller-side cancellation arriving during/after the charge does NOT
// silently skip the counter advance (the load-bearing observability
// surface for an already-persisted message).
func TestTool_Ask_ChargeRunsDetachedAfterCallerCancel(t *testing.T) {
	t.Parallel()
	orgID := uuid.New()
	target := samplePeer("Lead")
	enforcer := &cancellingEnforcer{}
	tool, repo, _, tokens := newToolWithBudget(t, orgID, []keepclient.Peer{target}, enforcer, nil)
	startReplierAfter(t, repo, orgID, target)

	ctx, cancel := context.WithCancel(context.Background())
	// The enforcer flips the caller's ctx the moment Charge is
	// entered. If peer.Ask used the caller's ctx for Charge, the
	// ctx.Err() check inside cancellingEnforcer.Charge would observe
	// the cancellation. With the detached-ctx fix the Charge runs to
	// completion regardless.
	enforcer.cancelOn = cancel

	_, _ = tool.Ask(ctx, peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityAsk],
		Target:              target.WatchkeeperID,
		Subject:             "test",
		Body:                []byte("hi"),
		Timeout:             500 * time.Millisecond,
	})
	if got := enforcer.callCount(); got != 1 {
		t.Errorf("Charge calls = %d, want 1 (detached ctx must let charge complete)", got)
	}
	if enforcer.observedCtxErr() != nil {
		t.Errorf("Charge observed ctx.Err = %v, want nil (caller ctx was forwarded — detached fix broken)", enforcer.observedCtxErr())
	}
}

// cancellingEnforcer cancels the caller's ctx the moment Charge is
// entered + records whether the ctx passed into Charge was already
// cancelled by that point. If the peer-tool uses a detached ctx, the
// passed-in ctx's Err() stays nil after the cancel.
type cancellingEnforcer struct {
	mu       sync.Mutex
	n        int
	cancelOn context.CancelFunc
	ctxErr   error
}

func (c *cancellingEnforcer) Charge(ctx context.Context, _ budget.ChargeParams) (budget.ChargeResult, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	if c.cancelOn != nil {
		c.cancelOn()
	}
	c.mu.Lock()
	c.ctxErr = ctx.Err()
	c.mu.Unlock()
	return budget.ChargeResult{TokensUsed: 1}, nil
}

func (c *cancellingEnforcer) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *cancellingEnforcer) observedCtxErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ctxErr
}

// TestTool_Ask_BudgetEnforcerEndToEnd wires a real budget.Writer with a
// recordingAuditor that records every EmitOverBudget call. Pins the
// end-to-end M1.5 plumbing — Ask → AppendMessage → IncTokens → emit +
// trigger — without the per-charge mock indirection.
func TestTool_Ask_BudgetEnforcerEndToEnd(t *testing.T) {
	t.Parallel()
	orgID := uuid.New()
	target := samplePeer("Lead")
	repo := k2k.NewMemoryRepository(time.Now, nil)
	pl := &fakePeerLister{peers: []keepclient.Peer{target}}
	lc := &fakeLifecycle{repo: repo}
	broker := capability.New()
	t.Cleanup(func() { _ = broker.Close() })
	askTok, err := broker.IssueForOrg(peer.CapabilityAsk, orgID.String(), time.Hour)
	if err != nil {
		t.Fatalf("issue ask: %v", err)
	}
	auditor := &budgetAwareAuditor{}
	trigger := &budgetTriggerCounter{}
	enforcer := budget.NewWriter(repo, auditor, budget.WithEscalationTrigger(trigger))
	tool := peer.NewTool(peer.Deps{
		PeerLister: pl,
		Lifecycle:  lc,
		Repository: repo,
		Capability: broker,
		Budget:     enforcer,
		// Use a tiny per-call budget so the very first body crosses it.
		TokenBudgetResolver: func(_ context.Context, _ string, _ uuid.UUID) (int64, bool, error) {
			return 1, true, nil
		},
	})

	startReplierAfter(t, repo, orgID, target)
	_, err = tool.Ask(context.Background(), peer.AskParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     askTok,
		Target:              target.WatchkeeperID,
		Subject:             "test",
		Body:                []byte("bigger than one token body!!!!!!!"),
		Timeout:             500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if got := auditor.overBudgetCount(); got != 1 {
		t.Errorf("EmitOverBudget calls = %d, want 1", got)
	}
	if got := trigger.count(); got != 1 {
		t.Errorf("TriggerOverBudget calls = %d, want 1", got)
	}
}

// budgetAwareAuditor satisfies audit.Emitter; the over-budget method is
// real (other methods short-circuit).
type budgetAwareAuditor struct {
	mu         sync.Mutex
	overBudget []audit.OverBudgetEvent
}

func (b *budgetAwareAuditor) EmitConversationOpened(_ context.Context, _ audit.ConversationOpenedEvent) (string, error) {
	return "", nil
}

func (b *budgetAwareAuditor) EmitConversationClosed(_ context.Context, _ audit.ConversationClosedEvent) (string, error) {
	return "", nil
}

func (b *budgetAwareAuditor) EmitMessageSent(_ context.Context, _ audit.MessageSentEvent) (string, error) {
	return "", nil
}

func (b *budgetAwareAuditor) EmitMessageReceived(_ context.Context, _ audit.MessageReceivedEvent) (string, error) {
	return "", nil
}

func (b *budgetAwareAuditor) EmitOverBudget(_ context.Context, evt audit.OverBudgetEvent) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.overBudget = append(b.overBudget, evt)
	return "row", nil
}

func (b *budgetAwareAuditor) EmitEscalated(_ context.Context, _ audit.EscalatedEvent) (string, error) {
	return "", nil
}

func (b *budgetAwareAuditor) overBudgetCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.overBudget)
}

type budgetTriggerCounter struct {
	mu sync.Mutex
	n  int
}

func (c *budgetTriggerCounter) TriggerOverBudget(_ context.Context, _ budget.OverBudgetTrigger) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return nil
}

func (c *budgetTriggerCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}
