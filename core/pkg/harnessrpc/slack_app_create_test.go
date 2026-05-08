// Tests for the slack.app_create JSON-RPC method (M6.1.b).
//
// Unit tests call the handler closure directly — the production
// `slackAppRPC` already has its own real-fakes test suite in
// core/pkg/spawn/, so this layer drives a stub `spawn.SlackAppRPC` to
// pin the wire-shape decoding + sentinel→code mapping the harnessrpc
// seam owns.
package harnessrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/harnessrpc"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ── stub SlackAppRPC ─────────────────────────────────────────────────────────

// stubSlackAppRPC records every CreateApp call and returns either a
// canned [spawn.CreateAppResult] or an injected error. This is a stub
// of the harnessrpc seam, NOT a mock of the production
// `slackAppRPC` — that unit has its own real-fakes suite in
// core/pkg/spawn/.
type stubSlackAppRPC struct {
	resp    spawn.CreateAppResult
	respErr error

	mu     sync.Mutex
	calls  []stubSlackAppRPCCall
	claims []spawn.Claim
}

type stubSlackAppRPCCall struct {
	req   spawn.CreateAppRequest
	claim spawn.Claim
}

func (s *stubSlackAppRPC) CreateApp(_ context.Context, req spawn.CreateAppRequest, claim spawn.Claim) (spawn.CreateAppResult, error) {
	s.mu.Lock()
	s.calls = append(s.calls, stubSlackAppRPCCall{req: req, claim: claim})
	s.claims = append(s.claims, claim)
	s.mu.Unlock()
	if s.respErr != nil {
		return spawn.CreateAppResult{}, s.respErr
	}
	return s.resp, nil
}

func (s *stubSlackAppRPC) recorded() []stubSlackAppRPCCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]stubSlackAppRPCCall, len(s.calls))
	copy(out, s.calls)
	return out
}

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	slackTestOrgID    = "00000000-0000-4000-8000-000000000000"
	slackTestAgentID  = "10000000-0000-4000-8000-000000000000"
	slackTestAppName  = "watchkeeper-bot"
	slackTestApprovTk = "approval-token-abc123"
)

// slackTestClaim returns a Watchmaster-shaped claim authorising the
// `slack_app_create` action. Used by tests that inject a fixed claim
// resolver.
func slackTestClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: slackTestOrgID,
		AgentID:        slackTestAgentID,
		AuthorityMatrix: map[string]string{
			"slack_app_create": "lead_approval",
		},
	}
}

// slackTestParams returns a wire-shape params map carrying every
// required field. Tests mutate the returned map to drive negative
// paths (missing approval_token, etc.).
func slackTestParams() map[string]any {
	return map[string]any{
		"agent_id":        slackTestAgentID,
		"app_name":        slackTestAppName,
		"app_description": "test bot",
		"scopes":          []string{"chat:write"},
		"approval_token":  slackTestApprovTk,
	}
}

// fixedClaimResolver returns a [harnessrpc.ClaimResolver] that always
// yields `claim`, ignoring the ctx. Mirrors the pattern M5.5.d.a.b
// uses for a fixed supervisor in tests — captures only the value the
// tests need to assert.
func fixedClaimResolver(claim spawn.Claim) harnessrpc.ClaimResolver {
	return func(_ context.Context) spawn.Claim { return claim }
}

// ── happy path ───────────────────────────────────────────────────────────────

// TestSlackAppCreateHandler_HappyPath_ReturnsAppID drives the handler
// happy path and pins:
//   - the wire-shape params decode (snake_case → spawn.CreateAppRequest);
//   - the claim is resolved via the supplied closure;
//   - the response carries `app_id`.
func TestSlackAppCreateHandler_HappyPath_ReturnsAppID(t *testing.T) {
	stub := &stubSlackAppRPC{resp: spawn.CreateAppResult{AppID: messenger.AppID("A0123ABCDEF")}}
	handler := harnessrpc.NewSlackAppCreateHandler(stub, fixedClaimResolver(slackTestClaim()))

	out, err := callHandler(t, handler, slackTestParams())
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}

	var got struct {
		AppID string `json:"app_id"`
	}
	if jsonErr := json.Unmarshal(out, &got); jsonErr != nil {
		t.Fatalf("decode result: %v", jsonErr)
	}
	if got.AppID != "A0123ABCDEF" {
		t.Errorf("app_id = %q, want %q", got.AppID, "A0123ABCDEF")
	}

	calls := stub.recorded()
	if len(calls) != 1 {
		t.Fatalf("CreateApp calls = %d, want 1", len(calls))
	}
	gotReq := calls[0].req
	if gotReq.AgentID != slackTestAgentID {
		t.Errorf("req.AgentID = %q, want %q", gotReq.AgentID, slackTestAgentID)
	}
	if gotReq.AppName != slackTestAppName {
		t.Errorf("req.AppName = %q, want %q", gotReq.AppName, slackTestAppName)
	}
	if gotReq.ApprovalToken != slackTestApprovTk {
		t.Errorf("req.ApprovalToken = %q, want %q", gotReq.ApprovalToken, slackTestApprovTk)
	}
	if len(gotReq.Scopes) != 1 || gotReq.Scopes[0] != "chat:write" {
		t.Errorf("req.Scopes = %v, want [chat:write]", gotReq.Scopes)
	}

	gotClaim := calls[0].claim
	if gotClaim.OrganizationID != slackTestOrgID {
		t.Errorf("claim.OrganizationID = %q, want %q", gotClaim.OrganizationID, slackTestOrgID)
	}
}

// ── negative paths — params shape ────────────────────────────────────────────

// TestSlackAppCreateHandler_NilParams_RejectsInvalidParams pins the
// nil-params guard. The handler MUST NOT call rpc.CreateApp when the
// envelope's params slot is empty.
func TestSlackAppCreateHandler_NilParams_RejectsInvalidParams(t *testing.T) {
	stub := &stubSlackAppRPC{}
	handler := harnessrpc.NewSlackAppCreateHandler(stub, fixedClaimResolver(slackTestClaim()))

	_, handlerErr := handler(context.Background(), nil)
	if handlerErr == nil {
		t.Fatal("expected error on nil params, got nil")
	}
	var rpcErr *harnessrpc.RPCError
	if !errors.As(handlerErr, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", handlerErr, handlerErr)
	}
	if rpcErr.Code != harnessrpc.ErrCodeInvalidParams {
		t.Errorf("code = %d, want %d (InvalidParams)", rpcErr.Code, harnessrpc.ErrCodeInvalidParams)
	}
	if got := stub.recorded(); len(got) != 0 {
		t.Errorf("CreateApp called %d times on nil-params path, want 0", len(got))
	}
}

// TestSlackAppCreateHandler_MalformedJSON_RejectsInvalidParams pins
// the json.Unmarshal failure path: a malformed JSON envelope surfaces
// as -32602 and never reaches the RPC.
func TestSlackAppCreateHandler_MalformedJSON_RejectsInvalidParams(t *testing.T) {
	stub := &stubSlackAppRPC{}
	handler := harnessrpc.NewSlackAppCreateHandler(stub, fixedClaimResolver(slackTestClaim()))

	_, handlerErr := handler(context.Background(), json.RawMessage(`{not valid json`))
	if handlerErr == nil {
		t.Fatal("expected error on malformed params, got nil")
	}
	var rpcErr *harnessrpc.RPCError
	if !errors.As(handlerErr, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", handlerErr, handlerErr)
	}
	if rpcErr.Code != harnessrpc.ErrCodeInvalidParams {
		t.Errorf("code = %d, want %d (InvalidParams)", rpcErr.Code, harnessrpc.ErrCodeInvalidParams)
	}
	if got := stub.recorded(); len(got) != 0 {
		t.Errorf("CreateApp called %d times on malformed-params path, want 0", len(got))
	}
}

// ── negative paths — sentinel mapping ────────────────────────────────────────

// TestSlackAppCreateHandler_Unauthorized_MapsToToolUnauthorized pins
// the [spawn.ErrUnauthorized] → -32005 mapping. Mirrors the TS-side
// `ToolErrorCode.ToolUnauthorized` so a wire caller sees one code per
// "you cannot do this" failure mode regardless of which side gated.
func TestSlackAppCreateHandler_Unauthorized_MapsToToolUnauthorized(t *testing.T) {
	stub := &stubSlackAppRPC{respErr: spawn.ErrUnauthorized}
	handler := harnessrpc.NewSlackAppCreateHandler(stub, fixedClaimResolver(slackTestClaim()))

	_, handlerErr := callHandler(t, handler, slackTestParams())
	if handlerErr == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *harnessrpc.RPCError
	if !errors.As(handlerErr, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", handlerErr, handlerErr)
	}
	if rpcErr.Code != harnessrpc.ErrCodeToolUnauthorized {
		t.Errorf("code = %d, want %d (ToolUnauthorized)", rpcErr.Code, harnessrpc.ErrCodeToolUnauthorized)
	}
}

// TestSlackAppCreateHandler_ApprovalRequired_MapsToApprovalRequired
// pins the [spawn.ErrApprovalRequired] → -32007 mapping. The
// approval-required code is application-range so a TS caller can
// branch on it without string-matching.
func TestSlackAppCreateHandler_ApprovalRequired_MapsToApprovalRequired(t *testing.T) {
	stub := &stubSlackAppRPC{respErr: spawn.ErrApprovalRequired}
	handler := harnessrpc.NewSlackAppCreateHandler(stub, fixedClaimResolver(slackTestClaim()))

	_, handlerErr := callHandler(t, handler, slackTestParams())
	if handlerErr == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *harnessrpc.RPCError
	if !errors.As(handlerErr, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", handlerErr, handlerErr)
	}
	if rpcErr.Code != harnessrpc.ErrCodeApprovalRequired {
		t.Errorf("code = %d, want %d (ApprovalRequired)", rpcErr.Code, harnessrpc.ErrCodeApprovalRequired)
	}
}

// TestSlackAppCreateHandler_InvalidClaim_MapsToInvalidParams pins the
// [spawn.ErrInvalidClaim] → -32602 mapping (an empty OrganizationID
// is a wire-shape error from the caller's perspective).
func TestSlackAppCreateHandler_InvalidClaim_MapsToInvalidParams(t *testing.T) {
	wrapped := fmt.Errorf("%w: empty OrganizationID", spawn.ErrInvalidClaim)
	stub := &stubSlackAppRPC{respErr: wrapped}
	handler := harnessrpc.NewSlackAppCreateHandler(stub, fixedClaimResolver(slackTestClaim()))

	_, handlerErr := callHandler(t, handler, slackTestParams())
	if handlerErr == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *harnessrpc.RPCError
	if !errors.As(handlerErr, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", handlerErr, handlerErr)
	}
	if rpcErr.Code != harnessrpc.ErrCodeInvalidParams {
		t.Errorf("code = %d, want %d (InvalidParams)", rpcErr.Code, harnessrpc.ErrCodeInvalidParams)
	}
}

// TestSlackAppCreateHandler_InvalidRequest_MapsToInvalidParams pins
// the [spawn.ErrInvalidRequest] → -32602 mapping.
func TestSlackAppCreateHandler_InvalidRequest_MapsToInvalidParams(t *testing.T) {
	wrapped := fmt.Errorf("%w: empty AppName", spawn.ErrInvalidRequest)
	stub := &stubSlackAppRPC{respErr: wrapped}
	handler := harnessrpc.NewSlackAppCreateHandler(stub, fixedClaimResolver(slackTestClaim()))

	_, handlerErr := callHandler(t, handler, slackTestParams())
	if handlerErr == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *harnessrpc.RPCError
	if !errors.As(handlerErr, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", handlerErr, handlerErr)
	}
	if rpcErr.Code != harnessrpc.ErrCodeInvalidParams {
		t.Errorf("code = %d, want %d (InvalidParams)", rpcErr.Code, harnessrpc.ErrCodeInvalidParams)
	}
}

// TestSlackAppCreateHandler_AdapterError_MapsToInternalError pins the
// fall-through path: a non-sentinel error from the spawn package
// (e.g. a wrapped Slack API error) surfaces as -32603 with the
// underlying message wrapped.
func TestSlackAppCreateHandler_AdapterError_MapsToInternalError(t *testing.T) {
	sentinel := errors.New("spawn: adapter create_app: slack: rate limited")
	stub := &stubSlackAppRPC{respErr: sentinel}
	handler := harnessrpc.NewSlackAppCreateHandler(stub, fixedClaimResolver(slackTestClaim()))

	_, handlerErr := callHandler(t, handler, slackTestParams())
	if handlerErr == nil {
		t.Fatal("expected error, got nil")
	}
	// Non-sentinel error path returns a plain wrapped error, NOT an
	// *RPCError — the dispatcher in host.go falls through to the
	// generic -32603 ErrCodeInternalError mapping.
	var rpcErr *harnessrpc.RPCError
	if errors.As(handlerErr, &rpcErr) {
		t.Errorf("expected internal error fall-through, got *RPCError: %+v", rpcErr)
	}
	if !errors.Is(handlerErr, sentinel) {
		t.Errorf("error chain does not contain sentinel: %v", handlerErr)
	}
}

// ── e2e through the Host ─────────────────────────────────────────────────────

// TestSlackAppCreate_E2E_FullDispatchChain drives `slack.app_create`
// through the full Host dispatch chain via the same io.Pipe shim
// `host_integration_test.go` and `remember_e2e_test.go` already use.
// Pins the wire contract for a TS caller: the params map decoded
// from a real NDJSON line carries the exact shape the TS
// `slack_app_create` builtin tool sends.
func TestSlackAppCreate_E2E_FullDispatchChain(t *testing.T) {
	stub := &stubSlackAppRPC{resp: spawn.CreateAppResult{AppID: messenger.AppID("A0123ABCDEF")}}

	host := harnessrpc.NewHost()
	host.Register("slack.app_create", harnessrpc.NewSlackAppCreateHandler(stub, fixedClaimResolver(slackTestClaim())))

	shim, teardown := startBridge(t, host)
	defer teardown()

	raw, rpcErr, err := shim.request("slack.app_create", slackTestParams())
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}

	var result struct {
		AppID string `json:"app_id"`
	}
	if jsonErr := json.Unmarshal(raw, &result); jsonErr != nil {
		t.Fatalf("decode result: %v", jsonErr)
	}
	if result.AppID != "A0123ABCDEF" {
		t.Errorf("app_id = %q, want %q", result.AppID, "A0123ABCDEF")
	}

	if got := stub.recorded(); len(got) != 1 {
		t.Errorf("CreateApp calls = %d, want 1", len(got))
	}
}
