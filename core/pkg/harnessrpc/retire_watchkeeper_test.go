// Tests for the M6.2.c retire_watchkeeper JSON-RPC handler.
//
// Unit tests call the handler closure directly via the shared
// `callHandler` helper from notebook_remember_test.go — the production
// `spawn.RetireWatchkeeper` already has its own real-fakes test suite
// in core/pkg/spawn/retire_watchkeeper_test.go, so this layer drives a
// stub `spawn.WatchmasterWriteClient` + recording keepersLogAppender
// to pin the wire-shape decoding + sentinel→code mapping the
// harnessrpc seam owns.
package harnessrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/harnessrpc"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// retireClaim returns a Watchmaster-shaped claim authorising the
// `retire_watchkeeper` action.
func retireClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: wmWriteOrgID,
		AgentID:        wmWriteAgentID,
		AuthorityMatrix: map[string]string{
			"retire_watchkeeper": "lead_approval",
		},
	}
}

// retireParams returns wire-shape params for the retire_watchkeeper
// handler.
func retireParams() map[string]any {
	return map[string]any{
		"agent_id":       wmWriteTargetAgentID,
		"approval_token": wmWriteApprovalToken,
	}
}

// ── retire_watchkeeper — happy path ──────────────────────────────────────────

// TestRetireWatchkeeperHandler_HappyPath_FlipsStatus drives the
// handler happy path and pins:
//   - the wire-shape params decode (snake_case → spawn.RetireWatchkeeperRequest);
//   - the claim is resolved via the supplied closure;
//   - the response is the empty success envelope (no fields);
//   - UpdateWatchkeeperStatus was invoked exactly once with
//     (target agent_id, "retired").
func TestRetireWatchkeeperHandler_HappyPath_FlipsStatus(t *testing.T) {
	stub := &stubWriteClient{}
	logger := &recordingAppender{}
	handler := harnessrpc.NewRetireWatchkeeperHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       stub,
		Logger:       logger,
		ResolveClaim: fixedWriteClaimResolver(retireClaim()),
	})

	out, err := callHandler(t, handler, retireParams())
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	// Empty-object envelope on success — retire has no return value.
	if string(out) != "{}" {
		t.Errorf("response = %s, want {}", string(out))
	}

	upd := stub.recordedUpdate()
	if len(upd) != 1 {
		t.Fatalf("UpdateWatchkeeperStatus calls = %+v, want 1", upd)
	}
	if upd[0].id != wmWriteTargetAgentID {
		t.Errorf("id = %q, want %q", upd[0].id, wmWriteTargetAgentID)
	}
	if upd[0].status != "retired" {
		t.Errorf("status = %q, want retired", upd[0].status)
	}
}

// ── retire_watchkeeper — params shape ────────────────────────────────────────

// TestRetireWatchkeeperHandler_NilParams_RejectsInvalidParams pins the
// nil-params guard.
func TestRetireWatchkeeperHandler_NilParams_RejectsInvalidParams(t *testing.T) {
	handler := harnessrpc.NewRetireWatchkeeperHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(retireClaim()),
	})
	_, handlerErr := handler(context.Background(), nil)
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// TestRetireWatchkeeperHandler_MalformedJSON_RejectsInvalidParams pins
// the json.Unmarshal failure path.
func TestRetireWatchkeeperHandler_MalformedJSON_RejectsInvalidParams(t *testing.T) {
	handler := harnessrpc.NewRetireWatchkeeperHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(retireClaim()),
	})
	_, handlerErr := handler(context.Background(), json.RawMessage(`{not valid json`))
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// ── retire_watchkeeper — sentinel mapping ────────────────────────────────────

// TestRetireWatchkeeperHandler_Unauthorized_MapsToToolUnauthorized pins
// the [spawn.ErrUnauthorized] → -32005 mapping.
func TestRetireWatchkeeperHandler_Unauthorized_MapsToToolUnauthorized(t *testing.T) {
	// Drive the spawn package itself with a non-authorising claim so
	// the real RetireWatchkeeper surfaces ErrUnauthorized.
	handler := harnessrpc.NewRetireWatchkeeperHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(spawn.Claim{OrganizationID: wmWriteOrgID}),
	})
	_, handlerErr := callHandler(t, handler, retireParams())
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeToolUnauthorized)
}

// TestRetireWatchkeeperHandler_ApprovalRequired_MapsToApprovalRequired
// pins the [spawn.ErrApprovalRequired] → -32007 mapping.
func TestRetireWatchkeeperHandler_ApprovalRequired_MapsToApprovalRequired(t *testing.T) {
	handler := harnessrpc.NewRetireWatchkeeperHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(retireClaim()),
	})
	params := retireParams()
	params["approval_token"] = ""
	_, handlerErr := callHandler(t, handler, params)
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeApprovalRequired)
}

// TestRetireWatchkeeperHandler_InvalidClaim_MapsToInvalidParams pins
// the [spawn.ErrInvalidClaim] → -32602 mapping.
func TestRetireWatchkeeperHandler_InvalidClaim_MapsToInvalidParams(t *testing.T) {
	handler := harnessrpc.NewRetireWatchkeeperHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(spawn.Claim{}), // empty OrganizationID
	})
	_, handlerErr := callHandler(t, handler, retireParams())
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// TestRetireWatchkeeperHandler_InvalidRequest_MapsToInvalidParams pins
// the [spawn.ErrInvalidRequest] → -32602 mapping (empty AgentID).
func TestRetireWatchkeeperHandler_InvalidRequest_MapsToInvalidParams(t *testing.T) {
	handler := harnessrpc.NewRetireWatchkeeperHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(retireClaim()),
	})
	params := retireParams()
	params["agent_id"] = ""
	_, handlerErr := callHandler(t, handler, params)
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// TestRetireWatchkeeperHandler_KeepClientError_MapsToInternalError
// pins the fall-through path: a non-sentinel error from the spawn
// package surfaces as a plain wrapped error (no RPCError); the
// dispatcher in host.go falls through to -32603.
func TestRetireWatchkeeperHandler_KeepClientError_MapsToInternalError(t *testing.T) {
	sentinel := errors.New("keepclient: invalid status transition")
	stub := &stubWriteClient{updateErr: sentinel}
	handler := harnessrpc.NewRetireWatchkeeperHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       stub,
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(retireClaim()),
	})
	_, handlerErr := callHandler(t, handler, retireParams())
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

// TestNewRetireWatchkeeperHandler_NilDeps_Panics pins the
// panic-on-nil-dependency contract for the retire constructor —
// matches the M6.2.b NewProposeSpawnHandler discipline.
func TestNewRetireWatchkeeperHandler_NilDeps_Panics(t *testing.T) {
	cases := []struct {
		name string
		deps harnessrpc.WatchmasterWriteDeps
	}{
		{"nil client", harnessrpc.WatchmasterWriteDeps{
			Client: nil, Logger: &recordingAppender{},
			ResolveClaim: fixedWriteClaimResolver(retireClaim()),
		}},
		{"nil logger", harnessrpc.WatchmasterWriteDeps{
			Client: &stubWriteClient{}, Logger: nil,
			ResolveClaim: fixedWriteClaimResolver(retireClaim()),
		}},
		{"nil resolver", harnessrpc.WatchmasterWriteDeps{
			Client: &stubWriteClient{}, Logger: &recordingAppender{}, ResolveClaim: nil,
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil {
					t.Fatal("expected panic, got none")
				}
			}()
			_ = harnessrpc.NewRetireWatchkeeperHandler(tc.deps)
		})
	}
}

// ── e2e through the Host ─────────────────────────────────────────────────────

// TestWatchmasterWrite_E2E_RetireWatchkeeper drives
// `watchmaster.retire_watchkeeper` through the full Host dispatch chain
// via the same io.Pipe shim host_integration_test.go already uses. Pins
// the wire contract for a TS caller.
func TestWatchmasterWrite_E2E_RetireWatchkeeper(t *testing.T) {
	stub := &stubWriteClient{}
	host := harnessrpc.NewHost()
	host.Register("watchmaster.retire_watchkeeper", harnessrpc.NewRetireWatchkeeperHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       stub,
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(retireClaim()),
	}))
	shim, teardown := startBridge(t, host)
	defer teardown()

	raw, rpcErr, err := shim.request("watchmaster.retire_watchkeeper", retireParams())
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}
	if string(raw) != "{}" {
		t.Errorf("e2e response = %s, want {}", string(raw))
	}
	if got := stub.recordedUpdate(); len(got) != 1 || got[0].status != "retired" {
		t.Errorf("UpdateWatchkeeperStatus = %+v, want 1 call with status=retired", got)
	}
}
