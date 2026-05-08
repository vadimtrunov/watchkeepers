// Tests for the three M6.2.b lead-approval-gated Watchmaster
// manifest-bump JSON-RPC methods.
//
// Unit tests call each handler closure directly via the shared
// `callHandler` helper from notebook_remember_test.go — the production
// `spawn.ProposeSpawn` / `AdjustPersonality` / `AdjustLanguage` already
// have their own real-fakes test suite in core/pkg/spawn/, so this
// layer drives a stub `spawn.WatchmasterWriteClient` + recording
// keepersLogAppender to pin the wire-shape decoding + sentinel→code
// mapping the harnessrpc seam owns.
package harnessrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/harnessrpc"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ── stub WatchmasterWriteClient ─────────────────────────────────────────────

// stubWriteClient records every GetManifest / PutManifestVersion /
// UpdateWatchkeeperStatus call and returns canned responses (or
// injected errors). This is a stub of the harnessrpc seam, NOT a mock
// of the production write-side tools — those have their own real-fakes
// suite in core/pkg/spawn/.
type stubWriteClient struct {
	getResp   *keepclient.ManifestVersion
	getErr    error
	putResp   *keepclient.PutManifestVersionResponse
	putErr    error
	updateErr error

	mu          sync.Mutex
	getCalls    []string
	putCalls    []stubWriteClientPutCall
	updateCalls []stubWriteClientUpdateCall
}

type stubWriteClientPutCall struct {
	manifestID string
	req        keepclient.PutManifestVersionRequest
}

type stubWriteClientUpdateCall struct {
	id     string
	status string
}

func (s *stubWriteClient) GetManifest(_ context.Context, manifestID string) (*keepclient.ManifestVersion, error) {
	s.mu.Lock()
	s.getCalls = append(s.getCalls, manifestID)
	s.mu.Unlock()
	if s.getErr != nil {
		return nil, s.getErr
	}
	if s.getResp == nil {
		return &keepclient.ManifestVersion{ID: "mv-prev", VersionNo: 1, SystemPrompt: "sp"}, nil
	}
	return s.getResp, nil
}

func (s *stubWriteClient) PutManifestVersion(
	_ context.Context,
	manifestID string,
	req keepclient.PutManifestVersionRequest,
) (*keepclient.PutManifestVersionResponse, error) {
	s.mu.Lock()
	s.putCalls = append(s.putCalls, stubWriteClientPutCall{manifestID: manifestID, req: req})
	idx := len(s.putCalls)
	s.mu.Unlock()
	if s.putErr != nil {
		return nil, s.putErr
	}
	if s.putResp == nil {
		return &keepclient.PutManifestVersionResponse{ID: fmt.Sprintf("mv-%d", idx)}, nil
	}
	return s.putResp, nil
}

func (s *stubWriteClient) UpdateWatchkeeperStatus(_ context.Context, id, status string) error {
	s.mu.Lock()
	s.updateCalls = append(s.updateCalls, stubWriteClientUpdateCall{id: id, status: status})
	s.mu.Unlock()
	return s.updateErr
}

func (s *stubWriteClient) recordedUpdate() []stubWriteClientUpdateCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]stubWriteClientUpdateCall, len(s.updateCalls))
	copy(out, s.updateCalls)
	return out
}

func (s *stubWriteClient) recordedPut() []stubWriteClientPutCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]stubWriteClientPutCall, len(s.putCalls))
	copy(out, s.putCalls)
	return out
}

// ── recording keepersLogAppender ────────────────────────────────────────────

// recordingAppender is the [harnessrpc.keepersLogAppender] stand-in
// the manifest-bump handler tests drive. The harnessrpc seam owns the
// wire decoding; the audit-chain semantics (event order, payload keys)
// are pinned by the spawn-package real-fakes suite. Here we only need
// to confirm the handler forwards a writer onto spawn.* so a test
// failure surfaces as a missing call rather than a stack trace.
type recordingAppender struct {
	mu     sync.Mutex
	events []keeperslog.Event
}

func (r *recordingAppender) Append(_ context.Context, evt keeperslog.Event) (string, error) {
	r.mu.Lock()
	r.events = append(r.events, evt)
	idx := len(r.events)
	r.mu.Unlock()
	return fmt.Sprintf("evt-%d", idx), nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	wmWriteOrgID         = "00000000-0000-4000-8000-000000000000"
	wmWriteAgentID       = "10000000-0000-4000-8000-000000000000"
	wmWriteTargetAgentID = "20000000-0000-4000-8000-000000000000"
	wmWriteApprovalToken = "approval-token-xyz789"
)

// proposeSpawnClaim returns a Watchmaster-shaped claim authorising the
// `propose_spawn` action.
func proposeSpawnClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: wmWriteOrgID,
		AgentID:        wmWriteAgentID,
		AuthorityMatrix: map[string]string{
			"propose_spawn": "lead_approval",
		},
	}
}

// adjPersonalityClaim returns a claim authorising `adjust_personality`.
func adjPersonalityClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: wmWriteOrgID,
		AgentID:        wmWriteAgentID,
		AuthorityMatrix: map[string]string{
			"adjust_personality": "lead_approval",
		},
	}
}

// adjLanguageClaim returns a claim authorising `adjust_language`.
func adjLanguageClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: wmWriteOrgID,
		AgentID:        wmWriteAgentID,
		AuthorityMatrix: map[string]string{
			"adjust_language": "lead_approval",
		},
	}
}

// proposeParams returns a wire-shape params map carrying every required
// field for the propose_spawn handler.
func proposeParams() map[string]any {
	return map[string]any{
		"agent_id":       wmWriteTargetAgentID,
		"system_prompt":  "you are a watchkeeper",
		"personality":    "diligent",
		"language":       "en",
		"approval_token": wmWriteApprovalToken,
	}
}

// adjPersonalityParams returns wire params for the adjust_personality
// handler.
func adjPersonalityParams() map[string]any {
	return map[string]any{
		"agent_id":        wmWriteTargetAgentID,
		"new_personality": "introspective",
		"approval_token":  wmWriteApprovalToken,
	}
}

// adjLanguageParams returns wire params for the adjust_language
// handler.
func adjLanguageParams() map[string]any {
	return map[string]any{
		"agent_id":       wmWriteTargetAgentID,
		"new_language":   "fr",
		"approval_token": wmWriteApprovalToken,
	}
}

// fixedWriteClaimResolver returns a [harnessrpc.ClaimResolver] that
// always yields `claim`. Mirrors the M6.1.b fixedClaimResolver pattern.
func fixedWriteClaimResolver(claim spawn.Claim) harnessrpc.ClaimResolver {
	return func(_ context.Context) spawn.Claim { return claim }
}

// ── propose_spawn — happy path ───────────────────────────────────────────────

// TestProposeSpawnHandler_HappyPath_ReturnsManifestVersionID drives the
// handler happy path and pins:
//   - the wire-shape params decode (snake_case → spawn.ProposeSpawnRequest);
//   - the claim is resolved via the supplied closure;
//   - the response carries `manifest_version_id` + `version_no`.
func TestProposeSpawnHandler_HappyPath_ReturnsManifestVersionID(t *testing.T) {
	stub := &stubWriteClient{
		putResp: &keepclient.PutManifestVersionResponse{ID: "mv-new"},
	}
	logger := &recordingAppender{}
	handler := harnessrpc.NewProposeSpawnHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       stub,
		Logger:       logger,
		ResolveClaim: fixedWriteClaimResolver(proposeSpawnClaim()),
	})

	out, err := callHandler(t, handler, proposeParams())
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var got struct {
		ManifestVersionID string `json:"manifest_version_id"`
		VersionNo         int    `json:"version_no"`
	}
	if jsonErr := json.Unmarshal(out, &got); jsonErr != nil {
		t.Fatalf("decode result: %v", jsonErr)
	}
	if got.ManifestVersionID != "mv-new" || got.VersionNo != 1 {
		t.Errorf("result = %+v, want {mv-new, 1}", got)
	}

	puts := stub.recordedPut()
	if len(puts) != 1 || puts[0].manifestID != wmWriteTargetAgentID {
		t.Errorf("PutManifestVersion calls = %+v, want [{%s, ...}]", puts, wmWriteTargetAgentID)
	}
	if puts[0].req.Personality != "diligent" {
		t.Errorf("forwarded Personality = %q, want diligent", puts[0].req.Personality)
	}
}

// ── propose_spawn — params shape ─────────────────────────────────────────────

// TestProposeSpawnHandler_NilParams_RejectsInvalidParams pins the
// nil-params guard.
func TestProposeSpawnHandler_NilParams_RejectsInvalidParams(t *testing.T) {
	handler := harnessrpc.NewProposeSpawnHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(proposeSpawnClaim()),
	})
	_, handlerErr := handler(context.Background(), nil)
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// TestProposeSpawnHandler_MalformedJSON_RejectsInvalidParams pins the
// json.Unmarshal failure path.
func TestProposeSpawnHandler_MalformedJSON_RejectsInvalidParams(t *testing.T) {
	handler := harnessrpc.NewProposeSpawnHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(proposeSpawnClaim()),
	})
	_, handlerErr := handler(context.Background(), json.RawMessage(`{not valid json`))
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// ── propose_spawn — sentinel mapping ─────────────────────────────────────────

// TestProposeSpawnHandler_Unauthorized_MapsToToolUnauthorized pins the
// [spawn.ErrUnauthorized] → -32005 mapping.
func TestProposeSpawnHandler_Unauthorized_MapsToToolUnauthorized(t *testing.T) {
	// Drive the spawn package itself with a non-authorising claim so
	// the real ProposeSpawn surfaces ErrUnauthorized.
	handler := harnessrpc.NewProposeSpawnHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(spawn.Claim{OrganizationID: wmWriteOrgID}),
	})
	_, handlerErr := callHandler(t, handler, proposeParams())
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeToolUnauthorized)
}

// TestProposeSpawnHandler_ApprovalRequired_MapsToApprovalRequired pins
// the [spawn.ErrApprovalRequired] → -32007 mapping.
func TestProposeSpawnHandler_ApprovalRequired_MapsToApprovalRequired(t *testing.T) {
	handler := harnessrpc.NewProposeSpawnHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(proposeSpawnClaim()),
	})
	params := proposeParams()
	params["approval_token"] = ""
	_, handlerErr := callHandler(t, handler, params)
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeApprovalRequired)
}

// TestProposeSpawnHandler_InvalidClaim_MapsToInvalidParams pins the
// [spawn.ErrInvalidClaim] → -32602 mapping.
func TestProposeSpawnHandler_InvalidClaim_MapsToInvalidParams(t *testing.T) {
	handler := harnessrpc.NewProposeSpawnHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(spawn.Claim{}), // empty OrganizationID
	})
	_, handlerErr := callHandler(t, handler, proposeParams())
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// TestProposeSpawnHandler_InvalidRequest_MapsToInvalidParams pins the
// [spawn.ErrInvalidRequest] → -32602 mapping (empty AgentID).
func TestProposeSpawnHandler_InvalidRequest_MapsToInvalidParams(t *testing.T) {
	handler := harnessrpc.NewProposeSpawnHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(proposeSpawnClaim()),
	})
	params := proposeParams()
	params["agent_id"] = ""
	_, handlerErr := callHandler(t, handler, params)
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// TestProposeSpawnHandler_KeepClientError_MapsToInternalError pins the
// fall-through path: a non-sentinel error from the spawn package
// surfaces as a plain wrapped error (no RPCError), the dispatcher in
// host.go falls through to -32603.
func TestProposeSpawnHandler_KeepClientError_MapsToInternalError(t *testing.T) {
	sentinel := errors.New("keepclient: db down")
	stub := &stubWriteClient{putErr: sentinel}
	handler := harnessrpc.NewProposeSpawnHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       stub,
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(proposeSpawnClaim()),
	})
	_, handlerErr := callHandler(t, handler, proposeParams())
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

// ── adjust_personality — happy + sentinels ──────────────────────────────────

// TestAdjustPersonalityHandler_HappyPath_ReturnsBumpedVersion drives
// the handler happy path: snake_case decode, spawn call, response
// shape `{manifest_version_id, version_no}`.
func TestAdjustPersonalityHandler_HappyPath_ReturnsBumpedVersion(t *testing.T) {
	stub := &stubWriteClient{
		getResp: &keepclient.ManifestVersion{
			ID:           "mv-prev",
			ManifestID:   wmWriteTargetAgentID,
			VersionNo:    2,
			SystemPrompt: "sp",
			Personality:  "old",
		},
		putResp: &keepclient.PutManifestVersionResponse{ID: "mv-bumped"},
	}
	handler := harnessrpc.NewAdjustPersonalityHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       stub,
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(adjPersonalityClaim()),
	})
	out, err := callHandler(t, handler, adjPersonalityParams())
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var got struct {
		ManifestVersionID string `json:"manifest_version_id"`
		VersionNo         int    `json:"version_no"`
	}
	if jsonErr := json.Unmarshal(out, &got); jsonErr != nil {
		t.Fatalf("decode result: %v", jsonErr)
	}
	if got.ManifestVersionID != "mv-bumped" || got.VersionNo != 3 {
		t.Errorf("result = %+v, want {mv-bumped, 3}", got)
	}

	puts := stub.recordedPut()
	if len(puts) != 1 || puts[0].req.Personality != "introspective" {
		t.Errorf("forwarded Personality = %+v, want introspective override", puts)
	}
}

// TestAdjustPersonalityHandler_Unauthorized_MapsToToolUnauthorized
// pins the sentinel mapping for the personality handler.
func TestAdjustPersonalityHandler_Unauthorized_MapsToToolUnauthorized(t *testing.T) {
	handler := harnessrpc.NewAdjustPersonalityHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(spawn.Claim{OrganizationID: wmWriteOrgID}),
	})
	_, handlerErr := callHandler(t, handler, adjPersonalityParams())
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeToolUnauthorized)
}

// TestAdjustPersonalityHandler_ApprovalRequired_MapsToApprovalRequired
// pins the empty-token gate for the personality handler.
func TestAdjustPersonalityHandler_ApprovalRequired_MapsToApprovalRequired(t *testing.T) {
	handler := harnessrpc.NewAdjustPersonalityHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(adjPersonalityClaim()),
	})
	params := adjPersonalityParams()
	params["approval_token"] = ""
	_, handlerErr := callHandler(t, handler, params)
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeApprovalRequired)
}

// ── adjust_language — happy + sentinels ─────────────────────────────────────

// TestAdjustLanguageHandler_HappyPath_ReturnsBumpedVersion mirrors the
// AdjustPersonalityHandler happy path, but pins the language override.
func TestAdjustLanguageHandler_HappyPath_ReturnsBumpedVersion(t *testing.T) {
	stub := &stubWriteClient{
		getResp: &keepclient.ManifestVersion{
			ID:           "mv-prev",
			ManifestID:   wmWriteTargetAgentID,
			VersionNo:    7,
			SystemPrompt: "sp",
			Language:     "en",
		},
		putResp: &keepclient.PutManifestVersionResponse{ID: "mv-bumped"},
	}
	handler := harnessrpc.NewAdjustLanguageHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       stub,
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(adjLanguageClaim()),
	})
	out, err := callHandler(t, handler, adjLanguageParams())
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	var got struct {
		ManifestVersionID string `json:"manifest_version_id"`
		VersionNo         int    `json:"version_no"`
	}
	if jsonErr := json.Unmarshal(out, &got); jsonErr != nil {
		t.Fatalf("decode result: %v", jsonErr)
	}
	if got.ManifestVersionID != "mv-bumped" || got.VersionNo != 8 {
		t.Errorf("result = %+v, want {mv-bumped, 8}", got)
	}

	puts := stub.recordedPut()
	if len(puts) != 1 || puts[0].req.Language != "fr" {
		t.Errorf("forwarded Language = %+v, want fr override", puts)
	}
}

// TestAdjustLanguageHandler_Unauthorized_MapsToToolUnauthorized pins
// the sentinel mapping for the language handler.
func TestAdjustLanguageHandler_Unauthorized_MapsToToolUnauthorized(t *testing.T) {
	handler := harnessrpc.NewAdjustLanguageHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(spawn.Claim{OrganizationID: wmWriteOrgID}),
	})
	_, handlerErr := callHandler(t, handler, adjLanguageParams())
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeToolUnauthorized)
}

// TestAdjustLanguageHandler_NilParams_RejectsInvalidParams pins the
// nil-params guard for the language handler.
func TestAdjustLanguageHandler_NilParams_RejectsInvalidParams(t *testing.T) {
	handler := harnessrpc.NewAdjustLanguageHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       &stubWriteClient{},
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(adjLanguageClaim()),
	})
	_, handlerErr := handler(context.Background(), nil)
	assertRPCErrCode(t, handlerErr, harnessrpc.ErrCodeInvalidParams)
}

// ── constructor discipline ───────────────────────────────────────────────────

// TestNewProposeSpawnHandler_NilDeps_Panics pins the
// panic-on-nil-dependency contract for all three constructors.
func TestNewProposeSpawnHandler_NilDeps_Panics(t *testing.T) {
	cases := []struct {
		name string
		deps harnessrpc.WatchmasterWriteDeps
	}{
		{"nil client", harnessrpc.WatchmasterWriteDeps{
			Client: nil, Logger: &recordingAppender{},
			ResolveClaim: fixedWriteClaimResolver(proposeSpawnClaim()),
		}},
		{"nil logger", harnessrpc.WatchmasterWriteDeps{
			Client: &stubWriteClient{}, Logger: nil,
			ResolveClaim: fixedWriteClaimResolver(proposeSpawnClaim()),
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
			_ = harnessrpc.NewProposeSpawnHandler(tc.deps)
		})
	}
}

func TestNewAdjustPersonalityHandler_NilClient_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil client, got none")
		}
	}()
	_ = harnessrpc.NewAdjustPersonalityHandler(harnessrpc.WatchmasterWriteDeps{
		Client: nil, Logger: &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(adjPersonalityClaim()),
	})
}

func TestNewAdjustLanguageHandler_NilLogger_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil logger, got none")
		}
	}()
	_ = harnessrpc.NewAdjustLanguageHandler(harnessrpc.WatchmasterWriteDeps{
		Client: &stubWriteClient{}, Logger: nil,
		ResolveClaim: fixedWriteClaimResolver(adjLanguageClaim()),
	})
}

// ── e2e through the Host ─────────────────────────────────────────────────────

// TestWatchmasterWrite_E2E_ProposeSpawn drives `watchmaster.propose_spawn`
// through the full Host dispatch chain via the same io.Pipe shim
// `host_integration_test.go` already uses. Pins the wire contract for
// a TS caller: the params map decoded from a real NDJSON line carries
// the exact shape the TS builtin tool sends.
func TestWatchmasterWrite_E2E_ProposeSpawn(t *testing.T) {
	stub := &stubWriteClient{
		putResp: &keepclient.PutManifestVersionResponse{ID: "mv-new"},
	}
	host := harnessrpc.NewHost()
	host.Register("watchmaster.propose_spawn", harnessrpc.NewProposeSpawnHandler(harnessrpc.WatchmasterWriteDeps{
		Client:       stub,
		Logger:       &recordingAppender{},
		ResolveClaim: fixedWriteClaimResolver(proposeSpawnClaim()),
	}))
	shim, teardown := startBridge(t, host)
	defer teardown()

	raw, rpcErr, err := shim.request("watchmaster.propose_spawn", proposeParams())
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	if rpcErr != nil {
		t.Fatalf("unexpected RPC error: %+v", rpcErr)
	}
	var got struct {
		ManifestVersionID string `json:"manifest_version_id"`
		VersionNo         int    `json:"version_no"`
	}
	if jsonErr := json.Unmarshal(raw, &got); jsonErr != nil {
		t.Fatalf("decode result: %v", jsonErr)
	}
	if got.ManifestVersionID != "mv-new" || got.VersionNo != 1 {
		t.Errorf("e2e result = %+v, want {mv-new, 1}", got)
	}
}

// ── shared helpers ───────────────────────────────────────────────────────────

// assertRPCErrCode pins that `err` is a *harnessrpc.RPCError carrying
// `wantCode`. Centralised here so the per-test assertions stay short
// — every sentinel-mapping case shares this shape.
func assertRPCErrCode(t *testing.T, err error, wantCode int) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var rpcErr *harnessrpc.RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error is not *RPCError: %T %v", err, err)
	}
	if rpcErr.Code != wantCode {
		t.Errorf("code = %d, want %d", rpcErr.Code, wantCode)
	}
}
