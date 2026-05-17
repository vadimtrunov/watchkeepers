package peer_test

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/peer"
)

// makePeerLister returns a configured fakePeerLister seeded with `peers`.
// Hoisted for the filter resolver test surface.
func makePeerLister(peers []keepclient.Peer) *fakePeerLister {
	return &fakePeerLister{peers: peers}
}

func TestRoleFilter_Validate_EmptyFilterRejected(t *testing.T) {
	t.Parallel()

	res := peer.NewFilterResolver(makePeerLister(nil))
	_, err := res.Resolve(context.Background(), peer.RoleFilter{}, "wk-self")
	if !errors.Is(err, peer.ErrPeerRoleFilterEmpty) {
		t.Errorf("Resolve(empty filter) err = %v, want errors.Is ErrPeerRoleFilterEmpty", err)
	}
}

func TestRoleFilter_Validate_WhitespaceEntryRejected(t *testing.T) {
	t.Parallel()

	res := peer.NewFilterResolver(makePeerLister(nil))
	cases := []struct {
		name   string
		filter peer.RoleFilter
	}{
		{name: "whitespace role", filter: peer.RoleFilter{Roles: []string{"Lead", "  "}}},
		{name: "whitespace language", filter: peer.RoleFilter{Languages: []string{"en", ""}}},
		{name: "whitespace capability", filter: peer.RoleFilter{Capabilities: []string{"peer:ask", "\t"}}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := res.Resolve(context.Background(), tc.filter, "wk-self")
			if !errors.Is(err, peer.ErrPeerRoleFilterEmpty) {
				t.Errorf("Resolve err = %v, want errors.Is ErrPeerRoleFilterEmpty", err)
			}
		})
	}
}

func TestRoleFilter_Roles_CaseInsensitiveMatch(t *testing.T) {
	t.Parallel()

	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-1", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-2", Role: "Strategist", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-3", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
	}
	res := peer.NewFilterResolver(makePeerLister(peers))
	got, err := res.Resolve(context.Background(), peer.RoleFilter{Roles: []string{"lead"}}, "wk-self")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	ids := []string{got[0].WatchkeeperID, got[1].WatchkeeperID}
	wantIDs := map[string]struct{}{"wk-1": {}, "wk-3": {}}
	for _, id := range ids {
		if _, ok := wantIDs[id]; !ok {
			t.Errorf("unexpected id %s in result %v", id, ids)
		}
	}
}

func TestRoleFilter_Languages_CaseInsensitiveMatch(t *testing.T) {
	t.Parallel()

	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-1", Role: "Lead", Language: "EN", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-2", Role: "Lead", Language: "ru", Availability: keepclient.PeerAvailabilityAvailable},
	}
	res := peer.NewFilterResolver(makePeerLister(peers))
	got, err := res.Resolve(context.Background(), peer.RoleFilter{Languages: []string{"en"}}, "wk-self")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0].WatchkeeperID != "wk-1" {
		t.Errorf("got = %+v, want only wk-1", got)
	}
}

func TestRoleFilter_Capabilities_SetSupersetMatch(t *testing.T) {
	t.Parallel()

	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-1", Capabilities: []string{"peer:ask", "peer:reply"}, Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-2", Capabilities: []string{"peer:ask"}, Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-3", Capabilities: []string{"peer:ask", "peer:reply", "peer:broadcast"}, Availability: keepclient.PeerAvailabilityAvailable},
	}
	res := peer.NewFilterResolver(makePeerLister(peers))
	got, err := res.Resolve(context.Background(), peer.RoleFilter{Capabilities: []string{"peer:ask", "peer:reply"}}, "wk-self")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (peers declaring BOTH capabilities)", len(got))
	}
	ids := map[string]struct{}{got[0].WatchkeeperID: {}, got[1].WatchkeeperID: {}}
	if _, ok := ids["wk-1"]; !ok {
		t.Errorf("wk-1 missing from result %v", got)
	}
	if _, ok := ids["wk-3"]; !ok {
		t.Errorf("wk-3 missing from result %v", got)
	}
}

func TestRoleFilter_Capabilities_CaseSensitive(t *testing.T) {
	t.Parallel()

	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-1", Capabilities: []string{"Peer:Ask"}, Availability: keepclient.PeerAvailabilityAvailable},
	}
	res := peer.NewFilterResolver(makePeerLister(peers))
	got, err := res.Resolve(context.Background(), peer.RoleFilter{Capabilities: []string{"peer:ask"}}, "wk-self")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got = %+v, want empty (capability match is case-sensitive)", got)
	}
}

func TestRoleFilter_ExcludeSelf(t *testing.T) {
	t.Parallel()

	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-self", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-other", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
	}
	res := peer.NewFilterResolver(makePeerLister(peers))

	// Without ExcludeSelf: both peers admitted.
	got, err := res.Resolve(context.Background(), peer.RoleFilter{Roles: []string{"Lead"}}, "wk-self")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("without ExcludeSelf: got len = %d, want 2", len(got))
	}

	// With ExcludeSelf: only wk-other.
	got, err = res.Resolve(context.Background(), peer.RoleFilter{Roles: []string{"Lead"}, ExcludeSelf: true}, "wk-self")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0].WatchkeeperID != "wk-other" {
		t.Errorf("with ExcludeSelf: got = %+v, want only wk-other", got)
	}
}

func TestRoleFilter_NonAvailablePeersSkipped(t *testing.T) {
	t.Parallel()

	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-1", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-2", Role: "Lead", Availability: "throttled"},
		{WatchkeeperID: "wk-3", Role: "Lead", Availability: ""},
	}
	res := peer.NewFilterResolver(makePeerLister(peers))
	got, err := res.Resolve(context.Background(), peer.RoleFilter{Roles: []string{"Lead"}}, "wk-self")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 || got[0].WatchkeeperID != "wk-1" {
		t.Errorf("got = %+v, want only wk-1 (non-available peers must be dropped)", got)
	}
}

func TestRoleFilter_StableSortByWatchkeeperID(t *testing.T) {
	t.Parallel()

	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-c", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-a", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-b", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
	}
	res := peer.NewFilterResolver(makePeerLister(peers))
	got, err := res.Resolve(context.Background(), peer.RoleFilter{Roles: []string{"Lead"}}, "wk-self")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{"wk-a", "wk-b", "wk-c"}
	for i, g := range got {
		if g.WatchkeeperID != want[i] {
			t.Errorf("got[%d] = %s, want %s", i, g.WatchkeeperID, want[i])
		}
	}
}

func TestRoleFilter_DefensiveDeepCopyOfCapabilities(t *testing.T) {
	t.Parallel()

	original := []string{"peer:ask"}
	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-1", Role: "Lead", Capabilities: original, Availability: keepclient.PeerAvailabilityAvailable},
	}
	res := peer.NewFilterResolver(makePeerLister(peers))
	got, err := res.Resolve(context.Background(), peer.RoleFilter{Roles: []string{"Lead"}}, "wk-self")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	// Mutate the returned Capabilities slice; the source must NOT bleed.
	got[0].Capabilities[0] = "MUTATED"
	if original[0] != "peer:ask" {
		t.Errorf("mutation bled into source capabilities: %v", original)
	}
}

func TestRoleFilter_ListerErrorWrapped(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("lister offline")
	lister := &fakePeerLister{listErr: sentinel}
	res := peer.NewFilterResolver(lister)
	_, err := res.Resolve(context.Background(), peer.RoleFilter{Roles: []string{"Lead"}}, "wk-self")
	if !errors.Is(err, sentinel) {
		t.Errorf("Resolve err = %v, want errors.Is sentinel", err)
	}
}

func TestNewFilterResolver_PanicsOnNilLister(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Error("NewFilterResolver did not panic on nil lister")
		}
	}()
	peer.NewFilterResolver(nil)
}

func TestRoleFilter_ConcurrentResolveSafe(t *testing.T) {
	t.Parallel()

	peers := []keepclient.Peer{
		{WatchkeeperID: "wk-1", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
		{WatchkeeperID: "wk-2", Role: "Lead", Availability: keepclient.PeerAvailabilityAvailable},
	}
	res := peer.NewFilterResolver(makePeerLister(peers))
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := res.Resolve(context.Background(), peer.RoleFilter{Roles: []string{"Lead"}}, "wk-self")
			if err != nil {
				t.Errorf("Resolve: %v", err)
				return
			}
			if len(got) != 2 {
				t.Errorf("len = %d, want 2", len(got))
			}
		}()
	}
	wg.Wait()
}

// TestFilter_NoAuditOrKeeperslogReferences is the source-grep AC for
// filter.go.
func TestFilter_NoAuditOrKeeperslogReferences(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("filter.go")
	if err != nil {
		t.Fatalf("read filter.go: %v", err)
	}
	lineCommentRE := regexp.MustCompile(`(?m)//.*$`)
	stripped := lineCommentRE.ReplaceAllString(string(data), "")
	bannedTokens := []string{"keeperslog.", ".Append("}
	for _, tok := range bannedTokens {
		if strings.Contains(stripped, tok) {
			t.Errorf("filter.go contains banned token %q (audit emission belongs to M1.4)", tok)
		}
	}
}
