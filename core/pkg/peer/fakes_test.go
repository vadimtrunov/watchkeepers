package peer_test

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/k2k"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
)

// fakePeerLister is the hand-rolled fake the peer-tool tests inject in
// place of `*keepclient.Client`. Matches the M1.2 boundary contract
// for `ListPeers`. No mocking-library imports — the skill discipline
// requires hand-rolled fakes so the test surface stays scannable at
// review time. Mirrors `core/pkg/k2k/lifecycle_test.go`'s
// `fakeSlackChannels` discipline.
type fakePeerLister struct {
	mu       sync.Mutex
	peers    []keepclient.Peer
	listErr  error
	lastReq  keepclient.ListPeersRequest
	callsLen int
}

func (f *fakePeerLister) ListPeers(_ context.Context, req keepclient.ListPeersRequest) (*keepclient.ListPeersResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastReq = req
	f.callsLen++
	if f.listErr != nil {
		return nil, f.listErr
	}
	// Defensive copy of the peers slice so the test's setup does not
	// share backing storage with the returned response.
	items := make([]keepclient.Peer, len(f.peers))
	copy(items, f.peers)
	for i := range items {
		caps := make([]string, len(items[i].Capabilities))
		copy(caps, items[i].Capabilities)
		items[i].Capabilities = caps
	}
	return &keepclient.ListPeersResponse{Items: items}, nil
}

// fakeLifecycle records OpenParams and returns a controllable
// conversation + error. The test wiring uses the in-memory
// repository (via fakeRepo) for the real persistence work but routes
// Open() through this fake to skip Slack channel I/O.
type fakeLifecycle struct {
	mu       sync.Mutex
	repo     *k2k.MemoryRepository
	openErr  error
	lastOpen k2k.OpenParams
	calls    int
}

func (f *fakeLifecycle) Open(ctx context.Context, params k2k.OpenParams) (k2k.Conversation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastOpen = params
	f.calls++
	if f.openErr != nil {
		return k2k.Conversation{}, f.openErr
	}
	return f.repo.Open(ctx, params)
}

// fakeCapability records every ValidateForOrg call and decides admit /
// deny based on a configurable error. The default zero value admits
// every token (returns nil) — tests opting into denial set `err` to a
// capability sentinel.
type fakeCapability struct {
	mu        sync.Mutex
	calls     int
	lastToken string
	lastScope string
	lastOrg   string
	err       error
}

func (f *fakeCapability) ValidateForOrg(_ context.Context, token, scope, organizationID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastToken = token
	f.lastScope = scope
	f.lastOrg = organizationID
	return f.err
}

// uuidString returns a freshly minted UUID.String(). Hoisted so tests
// stay scannable without inline `uuid.New().String()`.
func uuidString() string {
	return uuid.New().String()
}
