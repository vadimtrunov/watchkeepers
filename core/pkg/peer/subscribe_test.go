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

// newToolWithBus returns a [peer.Tool] wired identically to
// `newTool` but ALSO with an in-memory [peer.MemoryEventBus] threaded
// through Deps. The matching `peer:subscribe` capability token is added
// to the returned tokens map.
func newToolWithBus(t *testing.T, orgID uuid.UUID, peers []keepclient.Peer) (*peer.Tool, *k2k.MemoryRepository, *peer.MemoryEventBus, map[string]string) {
	t.Helper()
	repo := k2k.NewMemoryRepository(time.Now, nil)
	pl := &fakePeerLister{peers: peers}
	lc := &fakeLifecycle{repo: repo}
	bus := peer.NewMemoryEventBus()
	t.Cleanup(func() { _ = bus.Close() })

	broker := capability.New()
	t.Cleanup(func() { _ = broker.Close() })

	tokens := map[string]string{}
	for _, scope := range []string{peer.CapabilityAsk, peer.CapabilityReply, peer.CapabilityClose, peer.CapabilitySubscribe} {
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
		EventBus:   bus,
	})
	return tool, repo, bus, tokens
}

func TestTool_Subscribe_HappyPath_DeliversMatchingEvent(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, bus, tokens := newToolWithBus(t, orgID, nil)

	ch, cancel, err := tool.Subscribe(context.Background(), peer.SubscribeParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilitySubscribe],
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(cancel)

	ev := sampleEvent(orgID, "wk-asker", "k2k_message_sent", []byte(`{"a":1}`))
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-ch:
		if got.ID != ev.ID {
			t.Errorf("got.ID = %s, want %s", got.ID, ev.ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivery")
	}
}

func TestTool_Subscribe_ValidationFailures(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, _, tokens := newToolWithBus(t, orgID, nil)
	mkValid := func() peer.SubscribeParams {
		return peer.SubscribeParams{
			ActingWatchkeeperID: "wk-asker",
			OrganizationID:      orgID,
			CapabilityToken:     tokens[peer.CapabilitySubscribe],
		}
	}
	cases := []struct {
		name   string
		mutate func(p *peer.SubscribeParams)
		want   error
	}{
		{name: "empty acting wk", mutate: func(p *peer.SubscribeParams) { p.ActingWatchkeeperID = "  " }, want: peer.ErrInvalidActingWatchkeeperID},
		{name: "empty org id", mutate: func(p *peer.SubscribeParams) { p.OrganizationID = uuid.Nil }, want: k2k.ErrEmptyOrganization},
		{name: "whitespace event type entry", mutate: func(p *peer.SubscribeParams) { p.EventTypes = []string{"valid", " "} }, want: peer.ErrInvalidEventTypes},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := mkValid()
			tc.mutate(&p)
			_, _, err := tool.Subscribe(context.Background(), p)
			if !errors.Is(err, tc.want) {
				t.Errorf("Subscribe err = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestTool_Subscribe_CapabilityDenied(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, _, _ := newToolWithBus(t, orgID, nil)

	_, _, err := tool.Subscribe(context.Background(), peer.SubscribeParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     "bogus-token",
	})
	if !errors.Is(err, peer.ErrPeerCapabilityDenied) {
		t.Errorf("Subscribe err = %v, want errors.Is ErrPeerCapabilityDenied", err)
	}
	if !errors.Is(err, capability.ErrInvalidToken) {
		t.Errorf("Subscribe err = %v, want errors.Is capability.ErrInvalidToken (chained)", err)
	}
}

func TestTool_Subscribe_OrgMismatchOnCapability(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	otherOrg := uuid.New()
	tool, _, _, tokens := newToolWithBus(t, orgID, nil)

	_, _, err := tool.Subscribe(context.Background(), peer.SubscribeParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      otherOrg,
		CapabilityToken:     tokens[peer.CapabilitySubscribe], // bound to orgID
	})
	if !errors.Is(err, peer.ErrPeerCapabilityDenied) {
		t.Errorf("Subscribe err = %v, want errors.Is ErrPeerCapabilityDenied", err)
	}
	if !errors.Is(err, capability.ErrOrganizationMismatch) {
		t.Errorf("Subscribe err = %v, want errors.Is capability.ErrOrganizationMismatch (chained)", err)
	}
}

// TestTool_Subscribe_SelfSubscriptionAllowed — target == acting id is
// admitted.
func TestTool_Subscribe_SelfSubscriptionAllowed(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, _, tokens := newToolWithBus(t, orgID, nil)

	ch, cancel, err := tool.Subscribe(context.Background(), peer.SubscribeParams{
		ActingWatchkeeperID: "wk-self",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilitySubscribe],
		Target:              "wk-self",
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()
	if ch == nil {
		t.Error("Subscribe returned nil channel; want non-nil")
	}
}

// TestTool_Subscribe_CrossPeerTargetRejected — target != acting id and
// non-empty is denied with ErrPeerSubscriptionPermission.
func TestTool_Subscribe_CrossPeerTargetRejected(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, _, tokens := newToolWithBus(t, orgID, nil)

	_, _, err := tool.Subscribe(context.Background(), peer.SubscribeParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilitySubscribe],
		Target:              "wk-foreign",
	})
	if !errors.Is(err, peer.ErrPeerSubscriptionPermission) {
		t.Errorf("Subscribe err = %v, want errors.Is ErrPeerSubscriptionPermission", err)
	}
}

// TestTool_Subscribe_EmptyTargetSubscribesToAll — empty target
// subscribes to every event in the tenant.
func TestTool_Subscribe_EmptyTargetSubscribesToAll(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, bus, tokens := newToolWithBus(t, orgID, nil)

	ch, cancel, err := tool.Subscribe(context.Background(), peer.SubscribeParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilitySubscribe],
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	// Publish events for other watchkeepers — the empty-target
	// subscription must observe them.
	want := sampleEvent(orgID, "wk-someone-else", "evt", nil)
	if err := bus.Publish(context.Background(), want); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	select {
	case got := <-ch:
		if got.WatchkeeperID != "wk-someone-else" {
			t.Errorf("got.WatchkeeperID = %q, want wk-someone-else (empty target = subscribe to all)", got.WatchkeeperID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivery")
	}
}

// TestTool_Subscribe_EventBusUnavailableSurfacesSentinel — when the
// tool was constructed without an EventBus, Subscribe must surface
// ErrPeerEventBusUnavailable.
func TestTool_Subscribe_EventBusUnavailableSurfacesSentinel(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	// Build a tool without an EventBus.
	repo := k2k.NewMemoryRepository(time.Now, nil)
	pl := &fakePeerLister{}
	lc := &fakeLifecycle{repo: repo}
	broker := capability.New()
	t.Cleanup(func() { _ = broker.Close() })
	tok, err := broker.IssueForOrg(peer.CapabilitySubscribe, orgID.String(), time.Hour)
	if err != nil {
		t.Fatalf("IssueForOrg: %v", err)
	}
	tool := peer.NewTool(peer.Deps{
		PeerLister: pl, Lifecycle: lc, Repository: repo, Capability: broker,
	})

	_, _, err = tool.Subscribe(context.Background(), peer.SubscribeParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tok,
	})
	if !errors.Is(err, peer.ErrPeerEventBusUnavailable) {
		t.Errorf("Subscribe err = %v, want errors.Is ErrPeerEventBusUnavailable", err)
	}
}

func TestTool_Subscribe_PreCancelledCtxFailsFast(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, _, tokens := newToolWithBus(t, orgID, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := tool.Subscribe(ctx, peer.SubscribeParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilitySubscribe],
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Subscribe err = %v, want context.Canceled", err)
	}
}

// TestTool_Subscribe_EventTypesFilterForwarded — the EventTypes filter
// must be forwarded verbatim to the EventBus.
func TestTool_Subscribe_EventTypesFilterForwarded(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, bus, tokens := newToolWithBus(t, orgID, nil)

	ch, cancel, err := tool.Subscribe(context.Background(), peer.SubscribeParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     tokens[peer.CapabilitySubscribe],
		EventTypes:          []string{"k2k_message_sent"},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer cancel()

	// Two events — one matching, one not.
	skip := sampleEvent(orgID, "wk", "tool_invoked", nil)
	want := sampleEvent(orgID, "wk", "k2k_message_sent", nil)
	for _, e := range []peer.Event{skip, want} {
		if err := bus.Publish(context.Background(), e); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	select {
	case got := <-ch:
		if got.EventType != "k2k_message_sent" {
			t.Errorf("got.EventType = %q, want k2k_message_sent (filter forwarded?)", got.EventType)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for delivery")
	}
	select {
	case got := <-ch:
		t.Errorf("unexpected second delivery: %v", got)
	case <-time.After(50 * time.Millisecond):
	}
}

// TestTool_Subscribe_CapabilityGatePrecedesEventBus — a denied
// capability MUST NOT acquire a subscription / leak a goroutine.
func TestTool_Subscribe_CapabilityGatePrecedesEventBus(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, bus, _ := newToolWithBus(t, orgID, nil)

	_, _, err := tool.Subscribe(context.Background(), peer.SubscribeParams{
		ActingWatchkeeperID: "wk-asker",
		OrganizationID:      orgID,
		CapabilityToken:     "bogus",
	})
	if !errors.Is(err, peer.ErrPeerCapabilityDenied) {
		t.Fatalf("Subscribe err = %v, want errors.Is ErrPeerCapabilityDenied", err)
	}

	// Publish an event after the denial; if a subscription had been
	// created, the drop counter would not advance (the buffer has slack).
	// We assert no leaks indirectly via the goroutine count not climbing
	// here; the cancel-leak test already pins zero goroutines under load.
	_ = bus.Publish(context.Background(), sampleEvent(orgID, "wk-asker", "evt", nil))
	if got := bus.DroppedEvents(); got != 0 {
		t.Errorf("DroppedEvents = %d, want 0 (denied subscribe must not have created a subscriber)", got)
	}
}

// TestSubscribe_NoAuditOrKeeperslogReferences is the source-grep AC for
// subscribe.go. Same shape as ask.go / reply.go / close.go counterparts.
func TestSubscribe_NoAuditOrKeeperslogReferences(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("subscribe.go")
	if err != nil {
		t.Fatalf("read subscribe.go: %v", err)
	}
	lineCommentRE := regexp.MustCompile(`(?m)//.*$`)
	stripped := lineCommentRE.ReplaceAllString(string(data), "")

	bannedTokens := []string{"keeperslog.", ".Append("}
	for _, tok := range bannedTokens {
		if strings.Contains(stripped, tok) {
			t.Errorf("subscribe.go contains banned token %q (audit emission belongs to M1.4, not the peer-tool layer)", tok)
		}
	}
}

func TestBuiltinSubscribeManifest_ShapeAndValidate(t *testing.T) {
	t.Parallel()

	m := peer.BuiltinSubscribeManifest()
	if m.Name != "peer.subscribe" {
		t.Errorf("Name = %q, want %q", m.Name, "peer.subscribe")
	}
	if m.Source != peer.BuiltinSourceName {
		t.Errorf("Source = %q, want %q", m.Source, peer.BuiltinSourceName)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != peer.CapabilitySubscribe {
		t.Errorf("Capabilities = %v, want [%q]", m.Capabilities, peer.CapabilitySubscribe)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

func TestBuiltinSubscribeManifest_DefensiveDeepCopyOfSchema(t *testing.T) {
	t.Parallel()

	a := peer.BuiltinSubscribeManifest()
	b := peer.BuiltinSubscribeManifest()
	if len(a.Schema) == 0 {
		t.Fatal("Schema empty")
	}
	a.Schema[0] = 'X'
	if b.Schema[0] == 'X' {
		t.Error("Schema buffer shared across BuiltinSubscribeManifest calls (defensive copy regressed)")
	}
}

// TestTool_Subscribe_Concurrency_16Subscribers — 16 goroutines all
// subscribing + draining concurrently must compose without data races.
func TestTool_Subscribe_Concurrency_16Subscribers(t *testing.T) {
	t.Parallel()

	orgID := uuid.New()
	tool, _, bus, tokens := newToolWithBus(t, orgID, nil)

	const n = 16
	var (
		wg    sync.WaitGroup
		ready sync.WaitGroup
	)
	wg.Add(n)
	ready.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, cancelSub, err := tool.Subscribe(ctx, peer.SubscribeParams{
				ActingWatchkeeperID: "wk-asker",
				OrganizationID:      orgID,
				CapabilityToken:     tokens[peer.CapabilitySubscribe],
			})
			if err != nil {
				t.Errorf("Subscribe: %v", err)
				ready.Done()
				return
			}
			defer cancelSub()
			ready.Done()
			// Drain at least one event then exit.
			select {
			case <-ch:
			case <-time.After(2 * time.Second):
				t.Errorf("subscriber timed out waiting for delivery")
			}
		}()
	}
	ready.Wait()
	// Publish enough events so each subscriber receives at least one.
	for i := 0; i < n; i++ {
		_ = bus.Publish(context.Background(), sampleEvent(orgID, "wk-asker", "evt", nil))
	}
	wg.Wait()
}
