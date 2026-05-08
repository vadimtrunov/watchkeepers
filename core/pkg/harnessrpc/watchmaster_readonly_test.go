// Tests for the three M6.2.a read-only Watchmaster JSON-RPC methods.
//
// Unit tests call each handler closure directly via the shared
// `callHandler` helper from notebook_remember_test.go — the production
// `spawn.ListWatchkeepers` / `ReportCost` / `ReportHealth` already have
// their own real-fakes test suite in core/pkg/spawn/, so this layer
// drives a stub `spawn.WatchmasterReadClient` to pin the wire-shape
// decoding + sentinel→code mapping the harnessrpc seam owns.
package harnessrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/harnessrpc"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ── stub WatchmasterReadClient ───────────────────────────────────────────────

// stubReadClient records every call and returns a canned response (or
// an injected error). This is a stub of the harnessrpc seam, NOT a
// mock of the production read tools — those have their own real-fakes
// suite in core/pkg/spawn/.
type stubReadClient struct {
	listResp *keepclient.ListWatchkeepersResponse
	listErr  error
	tailResp *keepclient.LogTailResponse
	tailErr  error

	mu        sync.Mutex
	listCalls []keepclient.ListWatchkeepersRequest
	tailCalls []keepclient.LogTailOptions
}

func (s *stubReadClient) ListWatchkeepers(_ context.Context, req keepclient.ListWatchkeepersRequest) (*keepclient.ListWatchkeepersResponse, error) {
	s.mu.Lock()
	s.listCalls = append(s.listCalls, req)
	s.mu.Unlock()
	if s.listErr != nil {
		return nil, s.listErr
	}
	if s.listResp == nil {
		return &keepclient.ListWatchkeepersResponse{}, nil
	}
	return s.listResp, nil
}

func (s *stubReadClient) LogTail(_ context.Context, opts keepclient.LogTailOptions) (*keepclient.LogTailResponse, error) {
	s.mu.Lock()
	s.tailCalls = append(s.tailCalls, opts)
	s.mu.Unlock()
	if s.tailErr != nil {
		return nil, s.tailErr
	}
	if s.tailResp == nil {
		return &keepclient.LogTailResponse{}, nil
	}
	return s.tailResp, nil
}

func (s *stubReadClient) recordedList() []keepclient.ListWatchkeepersRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]keepclient.ListWatchkeepersRequest, len(s.listCalls))
	copy(out, s.listCalls)
	return out
}

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	wmTestOrgID   = "00000000-0000-4000-8000-000000000000"
	wmTestAgentID = "10000000-0000-4000-8000-000000000000"
)

// wmReadOnlyClaim returns a claim valid for the M6.2.a read-only tools.
// Empty AuthorityMatrix is fine — the tools do NOT consult it.
func wmReadOnlyClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: wmTestOrgID,
		AgentID:        wmTestAgentID,
	}
}

// fixedReadClaimResolver returns a [harnessrpc.ClaimResolver] that
// always yields `claim`. Mirrors the M6.1.b fixedClaimResolver pattern.
func fixedReadClaimResolver(claim spawn.Claim) harnessrpc.ClaimResolver {
	return func(_ context.Context) spawn.Claim { return claim }
}

// ── list_watchkeepers — happy path + sentinels ──────────────────────────────

// TestListWatchkeepersHandler_HappyPath_ReturnsItems pins the wire-
// shape decode + result projection: snake_case params decode, the
// stub gets the forwarded request, the response carries `items`.
func TestListWatchkeepersHandler_HappyPath_ReturnsItems(t *testing.T) {
	createdAt := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)
	stub := &stubReadClient{
		listResp: &keepclient.ListWatchkeepersResponse{
			Items: []keepclient.Watchkeeper{
				{ID: "wk-1", ManifestID: "mf-1", LeadHumanID: "h-1", Status: "active", CreatedAt: createdAt},
			},
		},
	}
	handler := harnessrpc.NewListWatchkeepersHandler(harnessrpc.WatchmasterReadDeps{
		Client:       stub,
		ResolveClaim: fixedReadClaimResolver(wmReadOnlyClaim()),
	})

	out, err := callHandler(t, handler, map[string]any{"status": "active", "limit": 10})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}

	var got struct {
		Items []spawn.WatchkeeperRow `json:"items"`
	}
	if jsonErr := json.Unmarshal(out, &got); jsonErr != nil {
		t.Fatalf("decode result: %v", jsonErr)
	}
	if len(got.Items) != 1 || got.Items[0].ID != "wk-1" {
		t.Errorf("items = %+v, want one row with ID=wk-1", got.Items)
	}
	calls := stub.recordedList()
	if len(calls) != 1 || calls[0].Status != "active" || calls[0].Limit != 10 {
		t.Errorf("forwarded request = %+v, want {Status:active, Limit:10}", calls)
	}
}

// TestListWatchkeepersHandler_NilParams_AllowsDefaults pins the
// no-param case: list_watchkeepers has no required fields, so empty
// params is a valid "no filter, server default limit" request. The
// handler MUST NOT reject with InvalidParams.
func TestListWatchkeepersHandler_NilParams_AllowsDefaults(t *testing.T) {
	stub := &stubReadClient{}
	handler := harnessrpc.NewListWatchkeepersHandler(harnessrpc.WatchmasterReadDeps{
		Client:       stub,
		ResolveClaim: fixedReadClaimResolver(wmReadOnlyClaim()),
	})

	_, handlerErr := handler(context.Background(), nil)
	if handlerErr != nil {
		t.Fatalf("nil params should not error: %v", handlerErr)
	}
	calls := stub.recordedList()
	if len(calls) != 1 || calls[0].Status != "" || calls[0].Limit != 0 {
		t.Errorf("forwarded request = %+v, want zero-valued forward", calls)
	}
}

// TestListWatchkeepersHandler_MalformedJSON_RejectsInvalidParams pins
// the json.Unmarshal failure path: a malformed envelope surfaces as
// -32602 and never reaches the read tool.
func TestListWatchkeepersHandler_MalformedJSON_RejectsInvalidParams(t *testing.T) {
	stub := &stubReadClient{}
	handler := harnessrpc.NewListWatchkeepersHandler(harnessrpc.WatchmasterReadDeps{
		Client:       stub,
		ResolveClaim: fixedReadClaimResolver(wmReadOnlyClaim()),
	})

	_, handlerErr := handler(context.Background(), json.RawMessage(`{not valid}`))
	var rpcErr *harnessrpc.RPCError
	if !errors.As(handlerErr, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", handlerErr, handlerErr)
	}
	if rpcErr.Code != harnessrpc.ErrCodeInvalidParams {
		t.Errorf("code = %d, want %d", rpcErr.Code, harnessrpc.ErrCodeInvalidParams)
	}
}

// TestListWatchkeepersHandler_InvalidClaim_MapsToInvalidParams pins
// the [spawn.ErrInvalidClaim] → -32602 mapping.
func TestListWatchkeepersHandler_InvalidClaim_MapsToInvalidParams(t *testing.T) {
	stub := &stubReadClient{}
	emptyClaim := spawn.Claim{} // OrganizationID empty
	handler := harnessrpc.NewListWatchkeepersHandler(harnessrpc.WatchmasterReadDeps{
		Client:       stub,
		ResolveClaim: fixedReadClaimResolver(emptyClaim),
	})

	_, handlerErr := callHandler(t, handler, map[string]any{})
	var rpcErr *harnessrpc.RPCError
	if !errors.As(handlerErr, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", handlerErr, handlerErr)
	}
	if rpcErr.Code != harnessrpc.ErrCodeInvalidParams {
		t.Errorf("code = %d, want %d (InvalidParams)", rpcErr.Code, harnessrpc.ErrCodeInvalidParams)
	}
}

// TestListWatchkeepersHandler_ClientError_MapsToInternalError pins the
// fall-through path: a non-sentinel error surfaces as -32603 wrapping
// the underlying message.
func TestListWatchkeepersHandler_ClientError_MapsToInternalError(t *testing.T) {
	sentinel := errors.New("keepclient: db down")
	stub := &stubReadClient{listErr: sentinel}
	handler := harnessrpc.NewListWatchkeepersHandler(harnessrpc.WatchmasterReadDeps{
		Client:       stub,
		ResolveClaim: fixedReadClaimResolver(wmReadOnlyClaim()),
	})

	_, handlerErr := callHandler(t, handler, map[string]any{})
	var rpcErr *harnessrpc.RPCError
	if errors.As(handlerErr, &rpcErr) {
		t.Errorf("expected internal-error fall-through, got *RPCError: %+v", rpcErr)
	}
	if !errors.Is(handlerErr, sentinel) {
		t.Errorf("error chain does not contain sentinel: %v", handlerErr)
	}
}

// ── report_cost — happy path + sentinels ─────────────────────────────────────

// TestReportCostHandler_HappyPath_ReturnsTotals pins the report_cost
// happy path: the handler decodes the wire params, forwards to the
// read tool, and returns the tokens / counts envelope.
func TestReportCostHandler_HappyPath_ReturnsTotals(t *testing.T) {
	stub := &stubReadClient{
		tailResp: &keepclient.LogTailResponse{
			Events: []keepclient.LogEvent{
				{
					ID:        "row-1",
					EventType: "llm_turn_cost",
					Payload:   marshalCostEnvelope(t, "agent-1", 100, 50),
				},
			},
		},
	}
	handler := harnessrpc.NewReportCostHandler(harnessrpc.WatchmasterReadDeps{
		Client:       stub,
		ResolveClaim: fixedReadClaimResolver(wmReadOnlyClaim()),
	})

	out, err := callHandler(t, handler, map[string]any{"agent_id": "agent-1", "limit": 10})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}

	var got struct {
		AgentID          string `json:"agent_id"`
		EventTypePrefix  string `json:"event_type_prefix"`
		PromptTokens     int64  `json:"prompt_tokens"`
		CompletionTokens int64  `json:"completion_tokens"`
		EventCount       int    `json:"event_count"`
		ScannedRows      int    `json:"scanned_rows"`
	}
	if jsonErr := json.Unmarshal(out, &got); jsonErr != nil {
		t.Fatalf("decode result: %v", jsonErr)
	}
	if got.PromptTokens != 100 || got.CompletionTokens != 50 {
		t.Errorf("tokens = (%d, %d), want (100, 50)", got.PromptTokens, got.CompletionTokens)
	}
	if got.EventCount != 1 || got.ScannedRows != 1 {
		t.Errorf("counts = (event=%d, scanned=%d), want (1, 1)", got.EventCount, got.ScannedRows)
	}
	if got.AgentID != "agent-1" {
		t.Errorf("agent_id echo = %q, want agent-1", got.AgentID)
	}
	if got.EventTypePrefix != "llm_turn_cost" {
		t.Errorf("event_type_prefix = %q, want %q", got.EventTypePrefix, "llm_turn_cost")
	}
}

// TestReportCostHandler_InvalidClaim_MapsToInvalidParams pins the
// [spawn.ErrInvalidClaim] → -32602 mapping.
func TestReportCostHandler_InvalidClaim_MapsToInvalidParams(t *testing.T) {
	stub := &stubReadClient{}
	handler := harnessrpc.NewReportCostHandler(harnessrpc.WatchmasterReadDeps{
		Client:       stub,
		ResolveClaim: fixedReadClaimResolver(spawn.Claim{}),
	})

	_, handlerErr := callHandler(t, handler, map[string]any{})
	var rpcErr *harnessrpc.RPCError
	if !errors.As(handlerErr, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", handlerErr, handlerErr)
	}
	if rpcErr.Code != harnessrpc.ErrCodeInvalidParams {
		t.Errorf("code = %d, want %d (InvalidParams)", rpcErr.Code, harnessrpc.ErrCodeInvalidParams)
	}
}

// TestReportCostHandler_NegativeLimit_MapsToInvalidParams pins the
// [spawn.ErrInvalidRequest] → -32602 mapping.
func TestReportCostHandler_NegativeLimit_MapsToInvalidParams(t *testing.T) {
	stub := &stubReadClient{}
	handler := harnessrpc.NewReportCostHandler(harnessrpc.WatchmasterReadDeps{
		Client:       stub,
		ResolveClaim: fixedReadClaimResolver(wmReadOnlyClaim()),
	})

	_, handlerErr := callHandler(t, handler, map[string]any{"limit": -1})
	var rpcErr *harnessrpc.RPCError
	if !errors.As(handlerErr, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", handlerErr, handlerErr)
	}
	if rpcErr.Code != harnessrpc.ErrCodeInvalidParams {
		t.Errorf("code = %d, want %d", rpcErr.Code, harnessrpc.ErrCodeInvalidParams)
	}
}

// ── report_health — happy path + sentinels ──────────────────────────────────

// TestReportHealthHandler_OrgWide_ReturnsCounts pins the org-wide
// happy path.
func TestReportHealthHandler_OrgWide_ReturnsCounts(t *testing.T) {
	createdAt := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)
	stub := &stubReadClient{
		listResp: &keepclient.ListWatchkeepersResponse{
			Items: []keepclient.Watchkeeper{
				{ID: "wk-1", Status: "active", CreatedAt: createdAt},
				{ID: "wk-2", Status: "pending", CreatedAt: createdAt},
				{ID: "wk-3", Status: "active", CreatedAt: createdAt},
			},
		},
	}
	handler := harnessrpc.NewReportHealthHandler(harnessrpc.WatchmasterReadDeps{
		Client:       stub,
		ResolveClaim: fixedReadClaimResolver(wmReadOnlyClaim()),
	})

	out, err := callHandler(t, handler, map[string]any{})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}

	var got struct {
		Item         *spawn.WatchkeeperHealth `json:"item"`
		CountPending int                      `json:"count_pending"`
		CountActive  int                      `json:"count_active"`
		CountRetired int                      `json:"count_retired"`
		CountTotal   int                      `json:"count_total"`
	}
	if jsonErr := json.Unmarshal(out, &got); jsonErr != nil {
		t.Fatalf("decode result: %v", jsonErr)
	}
	if got.Item != nil {
		t.Errorf("item = %+v, want nil for org-wide", got.Item)
	}
	if got.CountActive != 2 || got.CountPending != 1 || got.CountRetired != 0 {
		t.Errorf("counts = (active=%d, pending=%d, retired=%d), want (2, 1, 0)",
			got.CountActive, got.CountPending, got.CountRetired)
	}
	if got.CountTotal != 3 {
		t.Errorf("total = %d, want 3", got.CountTotal)
	}
}

// TestReportHealthHandler_ByAgentID_ReturnsItem pins the narrow-by-id
// branch.
func TestReportHealthHandler_ByAgentID_ReturnsItem(t *testing.T) {
	spawnedAt := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	createdAt := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)
	stub := &stubReadClient{
		listResp: &keepclient.ListWatchkeepersResponse{
			Items: []keepclient.Watchkeeper{
				{ID: "wk-1", Status: "active", SpawnedAt: &spawnedAt, CreatedAt: createdAt},
			},
		},
	}
	handler := harnessrpc.NewReportHealthHandler(harnessrpc.WatchmasterReadDeps{
		Client:       stub,
		ResolveClaim: fixedReadClaimResolver(wmReadOnlyClaim()),
	})

	out, err := callHandler(t, handler, map[string]any{"agent_id": "wk-1"})
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}

	var got struct {
		Item *spawn.WatchkeeperHealth `json:"item"`
	}
	if jsonErr := json.Unmarshal(out, &got); jsonErr != nil {
		t.Fatalf("decode result: %v", jsonErr)
	}
	if got.Item == nil || got.Item.ID != "wk-1" || got.Item.Status != "active" {
		t.Errorf("item = %+v, want {ID:wk-1, Status:active}", got.Item)
	}
}

// TestReportHealthHandler_InvalidClaim_MapsToInvalidParams pins the
// [spawn.ErrInvalidClaim] → -32602 mapping.
func TestReportHealthHandler_InvalidClaim_MapsToInvalidParams(t *testing.T) {
	stub := &stubReadClient{}
	handler := harnessrpc.NewReportHealthHandler(harnessrpc.WatchmasterReadDeps{
		Client:       stub,
		ResolveClaim: fixedReadClaimResolver(spawn.Claim{}),
	})

	_, handlerErr := callHandler(t, handler, map[string]any{})
	var rpcErr *harnessrpc.RPCError
	if !errors.As(handlerErr, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", handlerErr, handlerErr)
	}
	if rpcErr.Code != harnessrpc.ErrCodeInvalidParams {
		t.Errorf("code = %d, want %d", rpcErr.Code, harnessrpc.ErrCodeInvalidParams)
	}
}

// ── constructor discipline ───────────────────────────────────────────────────

// TestNewListWatchkeepersHandler_NilClient_Panics pins the
// panic-on-nil-dependency contract.
func TestNewListWatchkeepersHandler_NilClient_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil client, got none")
		}
	}()
	_ = harnessrpc.NewListWatchkeepersHandler(harnessrpc.WatchmasterReadDeps{
		Client:       nil,
		ResolveClaim: fixedReadClaimResolver(wmReadOnlyClaim()),
	})
}

func TestNewListWatchkeepersHandler_NilResolver_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil resolver, got none")
		}
	}()
	_ = harnessrpc.NewListWatchkeepersHandler(harnessrpc.WatchmasterReadDeps{
		Client:       &stubReadClient{},
		ResolveClaim: nil,
	})
}

func TestNewReportCostHandler_NilClient_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil client, got none")
		}
	}()
	_ = harnessrpc.NewReportCostHandler(harnessrpc.WatchmasterReadDeps{
		Client:       nil,
		ResolveClaim: fixedReadClaimResolver(wmReadOnlyClaim()),
	})
}

func TestNewReportHealthHandler_NilClient_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil client, got none")
		}
	}()
	_ = harnessrpc.NewReportHealthHandler(harnessrpc.WatchmasterReadDeps{
		Client:       nil,
		ResolveClaim: fixedReadClaimResolver(wmReadOnlyClaim()),
	})
}

// ── e2e through the Host ─────────────────────────────────────────────────────

// TestWatchmasterReadOnly_E2E_ListWatchkeepers drives
// `watchmaster.list_watchkeepers` through the full Host dispatch
// chain via the same io.Pipe shim `host_integration_test.go` and
// `slack_app_create_test.go` already use.
func TestWatchmasterReadOnly_E2E_ListWatchkeepers(t *testing.T) {
	createdAt := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)
	stub := &stubReadClient{
		listResp: &keepclient.ListWatchkeepersResponse{
			Items: []keepclient.Watchkeeper{
				{ID: "wk-1", ManifestID: "mf-1", LeadHumanID: "h-1", Status: "active", CreatedAt: createdAt},
			},
		},
	}
	host := harnessrpc.NewHost()
	host.Register("watchmaster.list_watchkeepers", harnessrpc.NewListWatchkeepersHandler(harnessrpc.WatchmasterReadDeps{
		Client:       stub,
		ResolveClaim: fixedReadClaimResolver(wmReadOnlyClaim()),
	}))

	shim, teardown := startBridge(t, host)
	defer teardown()

	raw, rpcErr, err := shim.request("watchmaster.list_watchkeepers", map[string]any{"status": "active"})
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}
	var got struct {
		Items []spawn.WatchkeeperRow `json:"items"`
	}
	if jsonErr := json.Unmarshal(raw, &got); jsonErr != nil {
		t.Fatalf("decode result: %v", jsonErr)
	}
	if len(got.Items) != 1 || got.Items[0].ID != "wk-1" {
		t.Errorf("items = %+v", got.Items)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// marshalCostEnvelope encodes a `data` payload into the keeperslog
// envelope shape ({"data": {agent_id, prompt_tokens, completion_tokens}}).
// Tests use this to construct LogTail fixtures.
func marshalCostEnvelope(t *testing.T, agentID string, prompt int, completion int) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"data": map[string]any{
			"agent_id":          agentID,
			"prompt_tokens":     prompt,
			"completion_tokens": completion,
		},
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return encoded
}
