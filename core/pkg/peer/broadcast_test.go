package peer_test

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/capability"
	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/peer"
)

// newToolWithBroadcast wires a peer.Tool with broker tokens for every
// peer.* scope plus an in-memory K2K repo, an auto-replier goroutine
// (started by the test) that mirrors request bodies back as replies.
// Returns the tool, repo, fakeLifecycle, and the issued tokens map.
func newToolWithBroadcast(t *testing.T, orgID uuid.UUID, peers []keepclient.Peer) (*peer.Tool, *k2k.MemoryRepository, *fakeLifecycle, map[string]string) {
	t.Helper()
	repo := k2k.NewMemoryRepository(time.Now, nil)
	pl := &fakePeerLister{peers: peers}
	lc := &fakeLifecycle{repo: repo}

	broker := capability.New()
	t.Cleanup(func() { _ = broker.Close() })

	tokens := map[string]string{}
	for _, scope := range []string{
		peer.CapabilityAsk,
		peer.CapabilityReply,
		peer.CapabilityClose,
		peer.CapabilitySubscribe,
		peer.CapabilityBroadcast,
	} {
		tok, err := broker.IssueForOrg(scope, orgID.String(), time.Hour)
		if err != nil {
			t.Fatalf("IssueForOrg %s: %v", scope, err)
		}
		tokens[scope] = tok
	}

	tool := peer.NewTool(peer.Deps{
		PeerLister: pl,
		Lifecycle:  lc,
		Repository: repo,
		Capability: broker,
	})
	return tool, repo, lc, tokens
}

// autoReplier launches a background goroutine that polls the repo for
// open conversations and appends a `reply`-direction message for each
// (so the broadcast's per-target Ask never times out). The returned
// cancel func stops the loop; tests call it via t.Cleanup.
func autoReplier(t *testing.T, repo *k2k.MemoryRepository, orgID uuid.UUID) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	replied := make(map[uuid.UUID]struct{})
	var mu sync.Mutex
	go func() {
		defer close(done)
		tick := time.NewTicker(2 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				convs, err := repo.List(ctx, k2k.ListFilter{OrganizationID: orgID})
				if err != nil {
					return
				}
				for _, c := range convs {
					mu.Lock()
					if _, ok := replied[c.ID]; ok {
						mu.Unlock()
						continue
					}
					replied[c.ID] = struct{}{}
					mu.Unlock()
					// Pick the second participant as the replier (the first
					// is the asker; the second is the target).
					replier := ""
					for _, p := range c.Participants {
						if p != "" && p != "wk-self" && p != "wk-asker" {
							replier = p
							break
						}
					}
					if replier == "" && len(c.Participants) > 0 {
						replier = c.Participants[len(c.Participants)-1]
					}
					_, err := repo.AppendMessage(ctx, k2k.AppendMessageParams{
						ConversationID:      c.ID,
						OrganizationID:      orgID,
						SenderWatchkeeperID: replier,
						Body:                []byte("ack"),
						Direction:           k2k.MessageDirectionReply,
					})
					if err != nil && !errors.Is(err, context.Canceled) {
						// Test-side errors are non-fatal — the test
						// itself asserts on result/timeout shape.
						_ = err
					}
				}
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

func TestTool_Broadcast_HappyPath_AllTargetsReceive(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-a", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-b", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-c", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
	}
	tool, repo, _, tokens := newToolWithBroadcast(t, orgID, peers)
	t.Cleanup(autoReplier(t, repo, orgID))

	res, err := tool.Broadcast(context.Background(), peer.BroadcastParams{
		ActingWatchkeeperID: "wk-self",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityBroadcast],
		Filter:              peer.RoleFilter{Roles: []string{"Lead"}},
		Subject:             "ping all leads",
		Body:                []byte("status?"),
	})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if len(res.Targets) != 3 {
		t.Fatalf("len(Targets) = %d, want 3", len(res.Targets))
	}
	for _, target := range res.Targets {
		if target.Err != nil {
			t.Errorf("target %s err = %v, want nil", target.WatchkeeperID, target.Err)
		}
		if target.ConversationID == uuid.Nil {
			t.Errorf("target %s conv id = uuid.Nil, want minted", target.WatchkeeperID)
		}
	}
}

func TestTool_Broadcast_EmptyFilterSurfacesErrPeerRoleFilterEmpty(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, _, tokens := newToolWithBroadcast(t, orgID, nil)

	_, err := tool.Broadcast(context.Background(), peer.BroadcastParams{
		ActingWatchkeeperID: "wk-self",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityBroadcast],
		Filter:              peer.RoleFilter{}, // empty
		Subject:             "ping",
		Body:                []byte("status?"),
	})
	if !errors.Is(err, peer.ErrPeerRoleFilterEmpty) {
		t.Errorf("err = %v, want errors.Is ErrPeerRoleFilterEmpty", err)
	}
}

func TestTool_Broadcast_NoMatchSurfacesErrPeerNoTargets(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-a", Role: "Strategist", Availability: keepclient.PeerAvailabilityAvailable},
	}
	tool, _, _, tokens := newToolWithBroadcast(t, orgID, peers)

	_, err := tool.Broadcast(context.Background(), peer.BroadcastParams{
		ActingWatchkeeperID: "wk-self",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityBroadcast],
		Filter:              peer.RoleFilter{Roles: []string{"Lead"}},
		Subject:             "ping",
		Body:                []byte("status?"),
	})
	if !errors.Is(err, peer.ErrPeerNoTargets) {
		t.Errorf("err = %v, want errors.Is ErrPeerNoTargets", err)
	}
}

// partialFailureLifecycle drives one target's Lifecycle.Open to a
// configurable error so the broadcast layer's partial-failure
// observability can be pinned without relying on Ask timeouts.
type partialFailureLifecycle struct {
	inner      *fakeLifecycle
	failPeerID string
	failErr    error
}

func (p *partialFailureLifecycle) Open(ctx context.Context, params k2k.OpenParams) (k2k.Conversation, error) {
	// Drive the matching peer to the configured error; everything
	// else routes through the underlying fakeLifecycle.
	for _, pid := range params.Participants {
		if pid == p.failPeerID {
			return k2k.Conversation{}, p.failErr
		}
	}
	return p.inner.Open(ctx, params)
}

func (p *partialFailureLifecycle) Close(ctx context.Context, id uuid.UUID, reason string) error {
	return p.inner.Close(ctx, id, reason)
}

func TestTool_Broadcast_PartialFailuresCollected(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-a", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-b", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
	}
	repo := k2k.NewMemoryRepository(time.Now, nil)
	pl := &fakePeerLister{peers: peers}
	innerLC := &fakeLifecycle{repo: repo}
	sentinel := errors.New("lifecycle: simulated open failure for wk-b")
	lc := &partialFailureLifecycle{inner: innerLC, failPeerID: "wk-b", failErr: sentinel}

	broker := capability.New()
	t.Cleanup(func() { _ = broker.Close() })
	bcastTok, err := broker.IssueForOrg(peer.CapabilityBroadcast, orgID.String(), time.Hour)
	if err != nil {
		t.Fatalf("IssueForOrg: %v", err)
	}

	tool := peer.NewTool(peer.Deps{
		PeerLister: pl,
		Lifecycle:  lc,
		Repository: repo,
		Capability: broker,
	})

	res, err := tool.Broadcast(context.Background(), peer.BroadcastParams{
		ActingWatchkeeperID: "wk-self",
		OrganizationID:      orgID,
		CapabilityToken:     bcastTok,
		Filter:              peer.RoleFilter{Roles: []string{"Lead"}},
		Subject:             "ping",
		Body:                []byte("status?"),
	})
	if err != nil {
		t.Fatalf("Broadcast err = %v, want nil (partial failures must not abort the fan-out)", err)
	}
	if len(res.Targets) != 2 {
		t.Fatalf("len(Targets) = %d, want 2", len(res.Targets))
	}
	got := map[string]error{}
	for _, target := range res.Targets {
		got[target.WatchkeeperID] = target.Err
	}
	if got["wk-a"] != nil {
		t.Errorf("wk-a err = %v, want nil", got["wk-a"])
	}
	if !errors.Is(got["wk-b"], sentinel) {
		t.Errorf("wk-b err = %v, want errors.Is sentinel", got["wk-b"])
	}
}

func TestTool_Broadcast_ExcludeSelfHonoured(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-self", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-other", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
	}
	tool, repo, _, tokens := newToolWithBroadcast(t, orgID, peers)
	t.Cleanup(autoReplier(t, repo, orgID))

	res, err := tool.Broadcast(context.Background(), peer.BroadcastParams{
		ActingWatchkeeperID: "wk-self",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityBroadcast],
		Filter:              peer.RoleFilter{Roles: []string{"Lead"}, ExcludeSelf: true},
		Subject:             "ping",
		Body:                []byte("status?"),
	})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if len(res.Targets) != 1 {
		t.Fatalf("len(Targets) = %d, want 1", len(res.Targets))
	}
	if res.Targets[0].WatchkeeperID != "wk-other" {
		t.Errorf("WatchkeeperID = %s, want wk-other (self excluded)", res.Targets[0].WatchkeeperID)
	}
}

func TestTool_Broadcast_CapabilityBrokerEnforced(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-a", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
	}
	tool, _, lc, _ := newToolWithBroadcast(t, orgID, peers)

	_, err := tool.Broadcast(context.Background(), peer.BroadcastParams{
		ActingWatchkeeperID: "wk-self",
		OrganizationID:      orgID,
		CapabilityToken:     "not-an-issued-token",
		Filter:              peer.RoleFilter{Roles: []string{"Lead"}},
		Subject:             "ping",
		Body:                []byte("status?"),
	})
	if !errors.Is(err, peer.ErrPeerCapabilityDenied) {
		t.Fatalf("err = %v, want errors.Is ErrPeerCapabilityDenied", err)
	}
	if lc.calls != 0 {
		t.Errorf("Lifecycle.Open calls = %d, want 0 (capability deny must short-circuit before resolve)", lc.calls)
	}
}

func TestTool_Broadcast_ValidationFailures(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-a", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
	}
	tool, _, _, tokens := newToolWithBroadcast(t, orgID, peers)

	mkValid := func() peer.BroadcastParams {
		return peer.BroadcastParams{
			ActingWatchkeeperID: "wk-self",
			OrganizationID:      orgID,
			CapabilityToken:     tokens[peer.CapabilityBroadcast],
			Filter:              peer.RoleFilter{Roles: []string{"Lead"}},
			Subject:             "ping",
			Body:                []byte("status?"),
		}
	}
	cases := []struct {
		name   string
		mutate func(p *peer.BroadcastParams)
		want   error
	}{
		{name: "empty acting", mutate: func(p *peer.BroadcastParams) { p.ActingWatchkeeperID = "" }, want: peer.ErrInvalidActingWatchkeeperID},
		{name: "whitespace acting", mutate: func(p *peer.BroadcastParams) { p.ActingWatchkeeperID = "   " }, want: peer.ErrInvalidActingWatchkeeperID},
		{name: "nil org", mutate: func(p *peer.BroadcastParams) { p.OrganizationID = uuid.Nil }, want: k2k.ErrEmptyOrganization},
		{name: "empty subject", mutate: func(p *peer.BroadcastParams) { p.Subject = "" }, want: peer.ErrInvalidSubject},
		{name: "whitespace subject", mutate: func(p *peer.BroadcastParams) { p.Subject = "  " }, want: peer.ErrInvalidSubject},
		{name: "empty body", mutate: func(p *peer.BroadcastParams) { p.Body = nil }, want: peer.ErrInvalidBody},
		{name: "zero body", mutate: func(p *peer.BroadcastParams) { p.Body = []byte{} }, want: peer.ErrInvalidBody},
		{name: "negative concurrency", mutate: func(p *peer.BroadcastParams) { p.Concurrency = -1 }, want: peer.ErrInvalidBroadcastConcurrency},
		{name: "empty filter", mutate: func(p *peer.BroadcastParams) { p.Filter = peer.RoleFilter{} }, want: peer.ErrPeerRoleFilterEmpty},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			params := mkValid()
			tc.mutate(&params)
			_, err := tool.Broadcast(context.Background(), params)
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestTool_Broadcast_CtxCancelBeforeResolveSurfacesCtxErr(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, _, tokens := newToolWithBroadcast(t, orgID, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := tool.Broadcast(ctx, peer.BroadcastParams{
		ActingWatchkeeperID: "wk-self",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityBroadcast],
		Filter:              peer.RoleFilter{Roles: []string{"Lead"}},
		Subject:             "ping",
		Body:                []byte("status?"),
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// countingFilterResolver is a hand-rolled FilterResolver that returns
// a fixed target set and lets the test inject a per-Ask gate (via a
// channel-backed hook) so the bounded worker pool can be observed.
type countingFilterResolver struct {
	targets []keepclient.Peer
	err     error
	calls   atomic.Int32
}

func (c *countingFilterResolver) Resolve(_ context.Context, _ peer.RoleFilter, _ string) ([]keepclient.Peer, error) {
	c.calls.Add(1)
	if c.err != nil {
		return nil, c.err
	}
	out := make([]keepclient.Peer, len(c.targets))
	copy(out, c.targets)
	return out, nil
}

// blockingLifecycle is a Lifecycle fake whose Open blocks on a shared
// gate channel so the worker-pool bound test can pin the in-flight
// worker count. Each Open call increments `inflight` (recording the
// peak) and then waits for a token on the gate before returning a
// fresh conversation via the underlying repo.
type blockingLifecycle struct {
	repo *k2k.MemoryRepository
	gate chan struct{}

	inflight atomic.Int32
	peak     atomic.Int32
}

func (b *blockingLifecycle) Open(ctx context.Context, params k2k.OpenParams) (k2k.Conversation, error) {
	cur := b.inflight.Add(1)
	defer b.inflight.Add(-1)
	for {
		pk := b.peak.Load()
		if cur <= pk || b.peak.CompareAndSwap(pk, cur) {
			break
		}
	}
	select {
	case <-b.gate:
	case <-ctx.Done():
		return k2k.Conversation{}, ctx.Err()
	}
	return b.repo.Open(ctx, params)
}

func (b *blockingLifecycle) Close(ctx context.Context, id uuid.UUID, reason string) error {
	return b.repo.Close(ctx, id, reason)
}

func TestTool_Broadcast_WorkerPoolBoundEnforced(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	// 100 targets, bound = 8 → expect peak in-flight ≤ 8.
	const total = 100
	const bound = 8

	targets := make([]keepclient.Peer, total)
	for i := 0; i < total; i++ {
		targets[i] = keepclient.Peer{
			WatchkeeperID: uuid.New().String(),
			Role:          "Lead",
			Availability:  keepclient.PeerAvailabilityAvailable,
			Capabilities:  []string{"peer:ask"},
		}
	}

	// The countingFilterResolver bypasses ListPeers for the broadcast
	// target-resolution step. The blockingLifecycle gates every
	// per-target Lifecycle.Open on a shared channel; its `inflight`
	// counter therefore tracks the in-flight worker count, which is
	// what the worker-pool bound caps. A noop fakePeerLister wires
	// the constructor's non-nil requirement.
	cfr := &countingFilterResolver{targets: targets}
	gate := make(chan struct{}, total)
	repo := k2k.NewMemoryRepository(time.Now, nil)
	blc := &blockingLifecycle{repo: repo, gate: gate}

	broker := capability.New()
	t.Cleanup(func() { _ = broker.Close() })
	bcastTok, _ := broker.IssueForOrg(peer.CapabilityBroadcast, orgID.String(), time.Hour)

	tool := peer.NewTool(peer.Deps{
		PeerLister:     &fakePeerLister{peers: targets},
		Lifecycle:      blc,
		Repository:     repo,
		Capability:     broker,
		FilterResolver: cfr,
	})

	// Spawn the broadcast in a goroutine; release the gate one token at
	// a time so the bounded pool is forced to throttle.
	done := make(chan struct {
		res peer.BroadcastResult
		err error
	}, 1)
	go func() {
		res, err := tool.Broadcast(context.Background(), peer.BroadcastParams{
			ActingWatchkeeperID: "wk-self",
			OrganizationID:      orgID,
			CapabilityToken:     bcastTok,
			Filter:              peer.RoleFilter{Roles: []string{"Lead"}},
			Subject:             "ping",
			Body:                []byte("status?"),
			Concurrency:         bound,
		})
		done <- struct {
			res peer.BroadcastResult
			err error
		}{res, err}
	}()

	// Let the workers ramp up: wait until inflight settles at the bound,
	// then start releasing the gate.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if blc.inflight.Load() >= int32(bound) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	// Observe the bound at this point.
	peakDuringRamp := blc.peak.Load()
	if peakDuringRamp > int32(bound) {
		t.Errorf("observed peak in-flight = %d, want ≤ %d", peakDuringRamp, bound)
	}

	// Release the gate `total` times so every Lifecycle.Open
	// completes; the worker returns immediately after AppendMessage
	// (no WaitForReply) and the next worker can grab a semaphore slot.
	go func() {
		for i := 0; i < total; i++ {
			select {
			case gate <- struct{}{}:
			case <-time.After(5 * time.Second):
				return
			}
		}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Broadcast err = %v, want nil", got.err)
		}
		if len(got.res.Targets) != total {
			t.Errorf("len(Targets) = %d, want %d", len(got.res.Targets), total)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Broadcast did not return")
	}

	peakFinal := blc.peak.Load()
	if peakFinal > int32(bound) {
		t.Errorf("final peak in-flight = %d, want ≤ %d", peakFinal, bound)
	}
}

func TestTool_Broadcast_DefaultConcurrencyApplied(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-a", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
	}
	tool, repo, _, tokens := newToolWithBroadcast(t, orgID, peers)
	t.Cleanup(autoReplier(t, repo, orgID))

	// Concurrency=0 → DefaultBroadcastConcurrency. The test asserts the
	// call completes (the bound is at least 1).
	res, err := tool.Broadcast(context.Background(), peer.BroadcastParams{
		ActingWatchkeeperID: "wk-self",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilityBroadcast],
		Filter:              peer.RoleFilter{Roles: []string{"Lead"}},
		Subject:             "ping",
		Body:                []byte("status?"),
		Concurrency:         0,
	})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if len(res.Targets) != 1 {
		t.Errorf("len(Targets) = %d, want 1", len(res.Targets))
	}
}

// recordingRepo wraps a real MemoryRepository and records every
// AppendMessage body for the body-defensive-copy assertion.
type recordingRepo struct {
	inner *k2k.MemoryRepository

	mu     sync.Mutex
	bodies [][]byte
}

func (r *recordingRepo) Get(ctx context.Context, id uuid.UUID) (k2k.Conversation, error) {
	return r.inner.Get(ctx, id)
}

func (r *recordingRepo) AppendMessage(ctx context.Context, params k2k.AppendMessageParams) (k2k.Message, error) {
	r.mu.Lock()
	snap := make([]byte, len(params.Body))
	copy(snap, params.Body)
	r.bodies = append(r.bodies, snap)
	r.mu.Unlock()
	return r.inner.AppendMessage(ctx, params)
}

func (r *recordingRepo) WaitForReply(ctx context.Context, id uuid.UUID, since time.Time, timeout time.Duration) (k2k.Message, error) {
	return r.inner.WaitForReply(ctx, id, since, timeout)
}

func (r *recordingRepo) SetCloseSummary(ctx context.Context, id uuid.UUID, summary string) error {
	return r.inner.SetCloseSummary(ctx, id, summary)
}

func TestTool_Broadcast_DefensiveCopyOfBody(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-a", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable, Capabilities: []string{"peer:ask"}},
	}
	repo := k2k.NewMemoryRepository(time.Now, nil)
	rec := &recordingRepo{inner: repo}
	pl := &fakePeerLister{peers: peers}
	lc := &fakeLifecycle{repo: repo}
	broker := capability.New()
	t.Cleanup(func() { _ = broker.Close() })
	bcastTok, err := broker.IssueForOrg(peer.CapabilityBroadcast, orgID.String(), time.Hour)
	if err != nil {
		t.Fatalf("IssueForOrg broadcast: %v", err)
	}

	tool := peer.NewTool(peer.Deps{
		PeerLister: pl,
		Lifecycle:  lc,
		Repository: rec,
		Capability: broker,
	})

	body := []byte("original")
	res, err := tool.Broadcast(context.Background(), peer.BroadcastParams{
		ActingWatchkeeperID: "wk-self",
		OrganizationID:      orgID,
		CapabilityToken:     bcastTok,
		Filter:              peer.RoleFilter{Roles: []string{"Lead"}},
		Subject:             "ping",
		Body:                body,
	})
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if len(res.Targets) != 1 || res.Targets[0].Err != nil {
		t.Fatalf("Targets[0] = %+v, want non-error", res.Targets[0])
	}

	// Mutate the caller-side input slice AFTER the call.
	for i := range body {
		body[i] = 'X'
	}

	// The recording repo captured the AppendMessage body the broadcast
	// layer dispatched. That snapshot must remain "original" — caller-
	// side mutation must not bleed into the in-flight Ask.
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.bodies) == 0 {
		t.Fatal("recordingRepo captured no AppendMessage bodies")
	}
	for i, b := range rec.bodies {
		if string(b) != "original" {
			t.Errorf("AppendMessage[%d].Body snapshot = %q, want %q (defensive copy regressed)", i, string(b), "original")
		}
	}
}

func TestTool_Broadcast_Concurrent16GoroutinesRace(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-a", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-b", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
	}
	tool, repo, _, tokens := newToolWithBroadcast(t, orgID, peers)
	t.Cleanup(autoReplier(t, repo, orgID))

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := tool.Broadcast(context.Background(), peer.BroadcastParams{
				ActingWatchkeeperID: "wk-self",
				OrganizationID:      orgID,
				CapabilityToken:     tokens[peer.CapabilityBroadcast],
				Filter:              peer.RoleFilter{Roles: []string{"Lead"}},
				Subject:             "ping",
				Body:                []byte("status?"),
			})
			if err != nil {
				t.Errorf("Broadcast: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestBroadcast_NoAuditOrKeeperslogReferences is the source-grep AC.
func TestBroadcast_NoAuditOrKeeperslogReferences(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("broadcast.go")
	if err != nil {
		t.Fatalf("read broadcast.go: %v", err)
	}
	lineCommentRE := regexp.MustCompile(`(?m)//.*$`)
	stripped := lineCommentRE.ReplaceAllString(string(data), "")
	bannedTokens := []string{"keeperslog.", ".Append("}
	for _, tok := range bannedTokens {
		if strings.Contains(stripped, tok) {
			t.Errorf("broadcast.go contains banned token %q (audit emission belongs to M1.4)", tok)
		}
	}
}

func TestBuiltinBroadcastManifest_ShapeAndValidate(t *testing.T) {
	t.Parallel()

	m := peer.BuiltinBroadcastManifest()
	if m.Name != "peer.broadcast" {
		t.Errorf("Name = %q, want %q", m.Name, "peer.broadcast")
	}
	if m.Source != peer.BuiltinSourceName {
		t.Errorf("Source = %q, want %q", m.Source, peer.BuiltinSourceName)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != peer.CapabilityBroadcast {
		t.Errorf("Capabilities = %v, want [%q]", m.Capabilities, peer.CapabilityBroadcast)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestBuiltinBroadcastManifest_DefensiveDeepCopyOfSchema(t *testing.T) {
	t.Parallel()

	a := peer.BuiltinBroadcastManifest()
	b := peer.BuiltinBroadcastManifest()
	if len(a.Schema) == 0 {
		t.Fatal("Schema empty")
	}
	a.Schema[0] = 'X'
	if b.Schema[0] == 'X' {
		t.Error("Schema buffer shared across BuiltinBroadcastManifest calls (defensive copy regressed)")
	}
}
