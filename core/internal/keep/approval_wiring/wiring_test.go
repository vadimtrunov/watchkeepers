package approvalwiring

import (
	"context"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// fakeLocalKeepClient stands in for the production
// [keeperslog.LocalKeepClient] so the smoke test can stand the
// composition up without an HTTP keep service. Records nothing — the
// smoke is asserting the wiring shape, not call ordering.
type fakeLocalKeepClient struct {
	mu sync.Mutex
}

func (f *fakeLocalKeepClient) LogAppend(_ context.Context, _ keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return &keepclient.LogAppendResponse{ID: "smoke-row"}, nil
}

// TestComposeApprovalDispatcher_WiresKickoffNonNil pins AC4 + the
// "Wiring" test plan case: the production composition wires
// `approval.New` with a non-nil SpawnKickoff. A nil kickoff would
// panic inside `ComposeApprovalDispatcher` (the dispatcher's
// constructor enforces it); this smoke also asserts the helper
// returns the kickoffer pointer so a future M7.1.c–.e wiring can
// retrieve it for diagnostic purposes.
func TestComposeApprovalDispatcher_WiresKickoffNonNil(t *testing.T) {
	t.Parallel()

	deps := ApprovalDispatcherDeps{
		KeepClient:         &fakeLocalKeepClient{},
		PendingApprovalDAO: spawn.NewMemoryPendingApprovalDAO(nil),
		Replayer:           nopReplayer{},
		AgentID:            "bot-watchmaster",
	}

	dispatcher, kickoffer, err := ComposeApprovalDispatcher(deps)
	if err != nil {
		t.Fatalf("ComposeApprovalDispatcher: %v", err)
	}
	if dispatcher == nil {
		t.Fatal("dispatcher = nil, want non-nil")
	}
	if kickoffer == nil {
		t.Fatal("kickoffer = nil, want non-nil (AC4: SpawnKickoff must be wired)")
	}
}

// TestComposeApprovalDispatcher_RejectsNilDeps pins the wiring's
// fail-closed posture: every dep is required and a missing one
// surfaces a clear error rather than a runtime panic deep inside the
// composition.
func TestComposeApprovalDispatcher_RejectsNilDeps(t *testing.T) {
	t.Parallel()

	good := ApprovalDispatcherDeps{
		KeepClient:         &fakeLocalKeepClient{},
		PendingApprovalDAO: spawn.NewMemoryPendingApprovalDAO(nil),
		Replayer:           nopReplayer{},
		AgentID:            "bot",
	}

	type override func(*ApprovalDispatcherDeps)
	cases := map[string]override{
		"nil KeepClient":         func(d *ApprovalDispatcherDeps) { d.KeepClient = nil },
		"nil PendingApprovalDAO": func(d *ApprovalDispatcherDeps) { d.PendingApprovalDAO = nil },
		"nil Replayer":           func(d *ApprovalDispatcherDeps) { d.Replayer = nil },
		"empty AgentID":          func(d *ApprovalDispatcherDeps) { d.AgentID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			deps := good
			mutate(&deps)
			d, k, err := ComposeApprovalDispatcher(deps)
			if err == nil {
				t.Fatalf("ComposeApprovalDispatcher: err = nil, want non-nil for %q", name)
			}
			if d != nil || k != nil {
				t.Errorf("ComposeApprovalDispatcher: returned non-nil values on error path for %q", name)
			}
		})
	}
}
