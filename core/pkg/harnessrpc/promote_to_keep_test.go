// Tests for the M6.2.d promote_to_keep JSON-RPC handler.
//
// Unit tests call the handler closure directly via the shared
// `callHandler` helper from notebook_remember_test.go — the production
// `spawn.PromoteToKeep` already has its own real-fakes test suite
// in core/pkg/spawn/promote_to_keep_test.go, so this layer drives a
// stub `spawn.WatchmasterWriteClient` + stub
// `spawn.WatchmasterNotebookClient` + recording keepersLogAppender to
// pin the wire-shape decoding + sentinel→code mapping the harnessrpc
// seam owns.
package harnessrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/harnessrpc"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ── stub WatchmasterNotebookClient ──────────────────────────────────────────

// stubNotebookClient records every PromoteToKeep call and returns
// either a canned proposal or an injected error. Stub of the
// harnessrpc seam, NOT a mock of notebook.DB.PromoteToKeep — that has
// its own real-fakes suite in core/pkg/notebook/.
type stubNotebookClient struct {
	resp *notebook.Proposal
	err  error

	mu    sync.Mutex
	calls []string
}

func (s *stubNotebookClient) PromoteToKeep(_ context.Context, entryID string) (*notebook.Proposal, error) {
	s.mu.Lock()
	s.calls = append(s.calls, entryID)
	s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return s.resp, nil
}

func (s *stubNotebookClient) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.calls))
	copy(out, s.calls)
	return out
}

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	wmPromoteEntryID    = "30000000-0000-7000-8000-000000000000"
	wmPromoteProposalID = "40000000-0000-7000-8000-000000000000"
)

// promoteClaim returns a Watchmaster-shaped claim authorising the
// `promote_to_keep` action.
func promoteClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: wmWriteOrgID,
		AgentID:        wmWriteAgentID,
		AuthorityMatrix: map[string]string{
			"promote_to_keep": "lead_approval",
		},
	}
}

// promoteParams returns a wire-shape params map carrying every
// required field for the promote_to_keep handler.
func promoteParams() map[string]any {
	return map[string]any{
		"agent_id":          wmWriteTargetAgentID,
		"notebook_entry_id": wmPromoteEntryID,
		"approval_token":    wmWriteApprovalToken,
	}
}

// canonicalStubProposal returns a canned [*notebook.Proposal] the
// stubNotebookClient hands back on the happy path.
func canonicalStubProposal() *notebook.Proposal {
	return &notebook.Proposal{
		Subject:         "lesson subject",
		Content:         "lesson content body",
		Embedding:       []float32{0.1, 0.2, 0.3},
		ProposalID:      wmPromoteProposalID,
		AgentID:         wmWriteAgentID,
		NotebookEntryID: wmPromoteEntryID,
		Category:        "lesson",
		Scope:           notebook.ScopeOrg,
		SourceCreatedAt: 1700000000000,
		ProposedAt:      1700000001000,
	}
}

// ── promote_to_keep — happy path ─────────────────────────────────────────────

// TestPromoteToKeepHandler_HappyPath_ReturnsChunkID drives the handler
// happy path and pins:
//   - the wire-shape params decode (snake_case → spawn.PromoteToKeepRequest);
//   - the claim is resolved via the supplied closure;
//   - the response carries `chunk_id` + `proposal_id` + `notebook_entry_id`;
//   - Store was invoked exactly once with the proposal's fields.
func TestPromoteToKeepHandler_HappyPath_ReturnsChunkID(t *testing.T) {
	stub := &stubWriteClient{}
	notebookStub := &stubNotebookClient{resp: canonicalStubProposal()}
	logger := &recordingAppender{}
	handler := harnessrpc.NewPromoteToKeepHandler(harnessrpc.WatchmasterPromoteDeps{
		Client:         stub,
		NotebookClient: notebookStub,
		Logger:         logger,
		ResolveClaim:   fixedWriteClaimResolver(promoteClaim()),
	})

	out, err := callHandler(t, handler, promoteParams())
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var resp map[string]string
	if err := json.Unmarshal(out, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["chunk_id"] == "" {
		t.Errorf("chunk_id is empty, want non-empty")
	}
	if resp["proposal_id"] != wmPromoteProposalID {
		t.Errorf("proposal_id = %q, want %q", resp["proposal_id"], wmPromoteProposalID)
	}
	if resp["notebook_entry_id"] != wmPromoteEntryID {
		t.Errorf("notebook_entry_id = %q, want %q", resp["notebook_entry_id"], wmPromoteEntryID)
	}
	if got := notebookStub.recorded(); len(got) != 1 || got[0] != wmPromoteEntryID {
		t.Errorf("notebook calls = %v, want [%s]", got, wmPromoteEntryID)
	}
	if got := stub.recordedStore(); len(got) != 1 {
		t.Errorf("Store calls = %d, want 1", len(got))
	}
}

// ── promote_to_keep — params shape ───────────────────────────────────────────

// TestPromoteToKeepHandler_NilParams_RejectsInvalidParams pins the
// nil-params guard.
func TestPromoteToKeepHandler_NilParams_RejectsInvalidParams(t *testing.T) {
	handler := harnessrpc.NewPromoteToKeepHandler(harnessrpc.WatchmasterPromoteDeps{
		Client:         &stubWriteClient{},
		NotebookClient: &stubNotebookClient{resp: canonicalStubProposal()},
		Logger:         &recordingAppender{},
		ResolveClaim:   fixedWriteClaimResolver(promoteClaim()),
	})
	_, handlerErr := handler(context.Background(), nil)
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// TestPromoteToKeepHandler_MalformedJSON_RejectsInvalidParams pins
// the json.Unmarshal failure path.
func TestPromoteToKeepHandler_MalformedJSON_RejectsInvalidParams(t *testing.T) {
	handler := harnessrpc.NewPromoteToKeepHandler(harnessrpc.WatchmasterPromoteDeps{
		Client:         &stubWriteClient{},
		NotebookClient: &stubNotebookClient{resp: canonicalStubProposal()},
		Logger:         &recordingAppender{},
		ResolveClaim:   fixedWriteClaimResolver(promoteClaim()),
	})
	_, handlerErr := handler(context.Background(), json.RawMessage(`{not valid json`))
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// ── promote_to_keep — sentinel mapping ───────────────────────────────────────

// TestPromoteToKeepHandler_Unauthorized_MapsToToolUnauthorized pins
// the [spawn.ErrUnauthorized] → -32005 mapping.
func TestPromoteToKeepHandler_Unauthorized_MapsToToolUnauthorized(t *testing.T) {
	handler := harnessrpc.NewPromoteToKeepHandler(harnessrpc.WatchmasterPromoteDeps{
		Client:         &stubWriteClient{},
		NotebookClient: &stubNotebookClient{resp: canonicalStubProposal()},
		Logger:         &recordingAppender{},
		ResolveClaim:   fixedWriteClaimResolver(spawn.Claim{OrganizationID: wmWriteOrgID}),
	})
	_, handlerErr := callHandler(t, handler, promoteParams())
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeToolUnauthorized)
}

// TestPromoteToKeepHandler_ApprovalRequired_MapsToApprovalRequired
// pins the [spawn.ErrApprovalRequired] → -32007 mapping.
func TestPromoteToKeepHandler_ApprovalRequired_MapsToApprovalRequired(t *testing.T) {
	handler := harnessrpc.NewPromoteToKeepHandler(harnessrpc.WatchmasterPromoteDeps{
		Client:         &stubWriteClient{},
		NotebookClient: &stubNotebookClient{resp: canonicalStubProposal()},
		Logger:         &recordingAppender{},
		ResolveClaim:   fixedWriteClaimResolver(promoteClaim()),
	})
	params := promoteParams()
	params["approval_token"] = ""
	_, handlerErr := callHandler(t, handler, params)
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeApprovalRequired)
}

// TestPromoteToKeepHandler_InvalidClaim_MapsToInvalidParams pins
// the [spawn.ErrInvalidClaim] → -32602 mapping.
func TestPromoteToKeepHandler_InvalidClaim_MapsToInvalidParams(t *testing.T) {
	handler := harnessrpc.NewPromoteToKeepHandler(harnessrpc.WatchmasterPromoteDeps{
		Client:         &stubWriteClient{},
		NotebookClient: &stubNotebookClient{resp: canonicalStubProposal()},
		Logger:         &recordingAppender{},
		ResolveClaim:   fixedWriteClaimResolver(spawn.Claim{}), // empty OrganizationID
	})
	_, handlerErr := callHandler(t, handler, promoteParams())
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// TestPromoteToKeepHandler_InvalidRequest_MapsToInvalidParams pins
// the [spawn.ErrInvalidRequest] → -32602 mapping (empty entry id).
func TestPromoteToKeepHandler_InvalidRequest_MapsToInvalidParams(t *testing.T) {
	handler := harnessrpc.NewPromoteToKeepHandler(harnessrpc.WatchmasterPromoteDeps{
		Client:         &stubWriteClient{},
		NotebookClient: &stubNotebookClient{resp: canonicalStubProposal()},
		Logger:         &recordingAppender{},
		ResolveClaim:   fixedWriteClaimResolver(promoteClaim()),
	})
	params := promoteParams()
	params["notebook_entry_id"] = ""
	_, handlerErr := callHandler(t, handler, params)
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// TestPromoteToKeepHandler_NotebookErrNotFound_MapsToToolNotFound pins
// the documented sentinel mapping for the M6.2.d-introduced
// notebook.ErrNotFound case: the handler surfaces a -32011 RPC error
// (ErrCodeToolNotFound) so a TS caller can branch without
// string-matching.
func TestPromoteToKeepHandler_NotebookErrNotFound_MapsToToolNotFound(t *testing.T) {
	notebookStub := &stubNotebookClient{err: notebook.ErrNotFound}
	handler := harnessrpc.NewPromoteToKeepHandler(harnessrpc.WatchmasterPromoteDeps{
		Client:         &stubWriteClient{},
		NotebookClient: notebookStub,
		Logger:         &recordingAppender{},
		ResolveClaim:   fixedWriteClaimResolver(promoteClaim()),
	})
	_, handlerErr := callHandler(t, handler, promoteParams())
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeToolNotFound)
}

// TestPromoteToKeepHandler_NotebookErrInvalidEntry_MapsToInvalidParams
// pins the documented mapping: a malformed UUID (non-canonical) is a
// wire-shape bug and surfaces as -32602 InvalidParams.
func TestPromoteToKeepHandler_NotebookErrInvalidEntry_MapsToInvalidParams(t *testing.T) {
	notebookStub := &stubNotebookClient{err: notebook.ErrInvalidEntry}
	handler := harnessrpc.NewPromoteToKeepHandler(harnessrpc.WatchmasterPromoteDeps{
		Client:         &stubWriteClient{},
		NotebookClient: notebookStub,
		Logger:         &recordingAppender{},
		ResolveClaim:   fixedWriteClaimResolver(promoteClaim()),
	})
	_, handlerErr := callHandler(t, handler, promoteParams())
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// TestPromoteToKeepHandler_StoreError_MapsToInternalError pins the
// fall-through path: a non-sentinel error from the keep client
// surfaces as a plain wrapped error (no RPCError); the dispatcher in
// host.go falls through to -32603.
func TestPromoteToKeepHandler_StoreError_MapsToInternalError(t *testing.T) {
	sentinel := errors.New("keepclient: 500")
	stub := &stubWriteClient{storeErr: sentinel}
	handler := harnessrpc.NewPromoteToKeepHandler(harnessrpc.WatchmasterPromoteDeps{
		Client:         stub,
		NotebookClient: &stubNotebookClient{resp: canonicalStubProposal()},
		Logger:         &recordingAppender{},
		ResolveClaim:   fixedWriteClaimResolver(promoteClaim()),
	})
	_, handlerErr := callHandler(t, handler, promoteParams())
	if handlerErr == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *harnessrpc.RPCError
	if errors.As(handlerErr, &rpcErr) {
		t.Errorf("expected internal error fall-through, got *RPCError: %+v", rpcErr)
	}
	if !errors.Is(handlerErr, sentinel) {
		t.Errorf("error chain does not contain sentinel: %v", handlerErr)
	}
}

// ── constructor discipline ───────────────────────────────────────────────────

// TestNewPromoteToKeepHandler_NilDeps_Panics pins the
// panic-on-nil-dependency contract for the promote constructor —
// matches the M6.2.b/c discipline.
func TestNewPromoteToKeepHandler_NilDeps_Panics(t *testing.T) {
	cases := []struct {
		name string
		deps harnessrpc.WatchmasterPromoteDeps
	}{
		{"nil client", harnessrpc.WatchmasterPromoteDeps{
			Client: nil, NotebookClient: &stubNotebookClient{}, Logger: &recordingAppender{},
			ResolveClaim: fixedWriteClaimResolver(promoteClaim()),
		}},
		{"nil notebook", harnessrpc.WatchmasterPromoteDeps{
			Client: &stubWriteClient{}, NotebookClient: nil, Logger: &recordingAppender{},
			ResolveClaim: fixedWriteClaimResolver(promoteClaim()),
		}},
		{"nil logger", harnessrpc.WatchmasterPromoteDeps{
			Client: &stubWriteClient{}, NotebookClient: &stubNotebookClient{}, Logger: nil,
			ResolveClaim: fixedWriteClaimResolver(promoteClaim()),
		}},
		{"nil resolver", harnessrpc.WatchmasterPromoteDeps{
			Client: &stubWriteClient{}, NotebookClient: &stubNotebookClient{}, Logger: &recordingAppender{},
			ResolveClaim: nil,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic, got none")
				}
			}()
			_ = harnessrpc.NewPromoteToKeepHandler(tc.deps)
		})
	}
}

// ── e2e through the Host ─────────────────────────────────────────────────────

// TestWatchmasterWrite_E2E_PromoteToKeep drives
// `watchmaster.promote_to_keep` through the full Host dispatch chain
// via the same io.Pipe shim host_integration_test.go already uses. Pins
// the wire contract for a TS caller.
func TestWatchmasterWrite_E2E_PromoteToKeep(t *testing.T) {
	stub := &stubWriteClient{}
	host := harnessrpc.NewHost()
	host.Register("watchmaster.promote_to_keep", harnessrpc.NewPromoteToKeepHandler(harnessrpc.WatchmasterPromoteDeps{
		Client:         stub,
		NotebookClient: &stubNotebookClient{resp: canonicalStubProposal()},
		Logger:         &recordingAppender{},
		ResolveClaim:   fixedWriteClaimResolver(promoteClaim()),
	}))
	shim, teardown := startBridge(t, host)
	defer teardown()

	raw, rpcErr, err := shim.request("watchmaster.promote_to_keep", promoteParams())
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}
	var resp map[string]string
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["chunk_id"] == "" {
		t.Errorf("e2e chunk_id is empty, want non-empty")
	}
	if got := stub.recordedStore(); len(got) != 1 {
		t.Errorf("Store calls = %d, want 1", len(got))
	}
}
