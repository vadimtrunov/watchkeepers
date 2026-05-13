package harnessrpc_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/capability"
	"github.com/vadimtrunov/watchkeepers/core/pkg/github"
	"github.com/vadimtrunov/watchkeepers/core/pkg/harnessrpc"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolshare"
)

type stubCapability struct {
	err error
}

func (s stubCapability) Validate(_ context.Context, _, _ string) error { return s.err }

type stubSharer struct {
	res toolshare.ShareResult
	err error
	mu  *toolshare.ShareRequest
}

func (s *stubSharer) Share(_ context.Context, req toolshare.ShareRequest) (toolshare.ShareResult, error) {
	if s.mu != nil {
		*s.mu = req
	}
	return s.res, s.err
}

func goodDeps() harnessrpc.PromoteShareToolDeps {
	return harnessrpc.PromoteShareToolDeps{
		Capability: stubCapability{},
		Sharer:     &stubSharer{res: toolshare.ShareResult{PRNumber: 42, PRHTMLURL: "https://github.com/o/r/pull/42", BranchName: "share/X", ToolVersion: "1.2.0", CorrelationID: "abc", LeadNotified: true}},
	}
}

func goodParams() json.RawMessage {
	return json.RawMessage(`{
		"proposer_id":"agent-001",
		"source_name":"private",
		"tool_name":"weekly_digest",
		"target_hint":"platform",
		"reason":"graduating",
		"capability_token":"tok"
	}`)
}

func TestNewPromoteShareToolHandler_NilCapability_Panics(t *testing.T) {
	deps := goodDeps()
	deps.Capability = nil
	assertPanic(t, "capability", func() { harnessrpc.NewPromoteShareToolHandler(deps) })
}

func TestNewPromoteShareToolHandler_NilSharer_Panics(t *testing.T) {
	deps := goodDeps()
	deps.Sharer = nil
	assertPanic(t, "sharer", func() { harnessrpc.NewPromoteShareToolHandler(deps) })
}

func TestPromoteShareTool_NilParams_Refused(t *testing.T) {
	h := harnessrpc.NewPromoteShareToolHandler(goodDeps())
	_, err := h(context.Background(), nil)
	assertRPCErrCode(t, err, harnessrpc.ErrCodeInvalidParams)
}

func TestPromoteShareTool_MalformedParams_Refused(t *testing.T) {
	h := harnessrpc.NewPromoteShareToolHandler(goodDeps())
	_, err := h(context.Background(), json.RawMessage(`{not-json`))
	assertRPCErrCode(t, err, harnessrpc.ErrCodeInvalidParams)
}

func TestPromoteShareTool_EmptyCapabilityToken_Refused(t *testing.T) {
	h := harnessrpc.NewPromoteShareToolHandler(goodDeps())
	params := json.RawMessage(`{"proposer_id":"a","source_name":"s","tool_name":"t","target_hint":"platform","reason":"r","capability_token":""}`)
	_, err := h(context.Background(), params)
	assertRPCErrCode(t, err, harnessrpc.ErrCodeToolUnauthorized)
}

func TestPromoteShareTool_InvalidToken_Refused(t *testing.T) {
	deps := goodDeps()
	deps.Capability = stubCapability{err: capability.ErrInvalidToken}
	h := harnessrpc.NewPromoteShareToolHandler(deps)
	_, err := h(context.Background(), goodParams())
	assertRPCErrCode(t, err, harnessrpc.ErrCodeToolUnauthorized)
}

func TestPromoteShareTool_TokenExpired_Refused(t *testing.T) {
	deps := goodDeps()
	deps.Capability = stubCapability{err: capability.ErrTokenExpired}
	h := harnessrpc.NewPromoteShareToolHandler(deps)
	_, err := h(context.Background(), goodParams())
	assertRPCErrCode(t, err, harnessrpc.ErrCodeToolUnauthorized)
}

func TestPromoteShareTool_ScopeMismatch_Refused(t *testing.T) {
	deps := goodDeps()
	deps.Capability = stubCapability{err: capability.ErrScopeMismatch}
	h := harnessrpc.NewPromoteShareToolHandler(deps)
	_, err := h(context.Background(), goodParams())
	assertRPCErrCode(t, err, harnessrpc.ErrCodeToolUnauthorized)
}

func TestPromoteShareTool_HappyPath_ForwardsToSharer(t *testing.T) {
	var captured toolshare.ShareRequest
	deps := goodDeps()
	sharer := &stubSharer{
		res: toolshare.ShareResult{PRNumber: 42, PRHTMLURL: "https://github.com/o/r/pull/42", BranchName: "share/X", ToolVersion: "1.2.0", CorrelationID: "corr", LeadNotified: true},
		mu:  &captured,
	}
	deps.Sharer = sharer
	h := harnessrpc.NewPromoteShareToolHandler(deps)
	rawResult, err := h(context.Background(), goodParams())
	if err != nil {
		t.Fatalf("handler err=%v", err)
	}
	if captured.SourceName != "private" || captured.ToolName != "weekly_digest" {
		t.Errorf("forward source/tool: %+v", captured)
	}
	if captured.TargetHint != toolshare.TargetSourcePlatform {
		t.Errorf("forward target hint=%q", captured.TargetHint)
	}
	if captured.Reason != "graduating" {
		t.Errorf("forward reason=%q", captured.Reason)
	}
	// Result envelope.
	data, _ := json.Marshal(rawResult)
	js := string(data)
	for _, want := range []string{`"pr_number":42`, `"pr_html_url":"https://github.com/o/r/pull/42"`, `"branch_name":"share/X"`, `"correlation_id":"corr"`, `"lead_notified":true`} {
		if !strings.Contains(js, want) {
			t.Errorf("result json missing %q: %s", want, js)
		}
	}
}

func TestPromoteShareTool_ShareError_Mapped(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"invalid request", toolshare.ErrInvalidShareRequest, harnessrpc.ErrCodeInvalidParams},
		{"invalid proposer", toolshare.ErrInvalidProposerID, harnessrpc.ErrCodeInvalidParams},
		{"invalid target", toolshare.ErrInvalidTarget, harnessrpc.ErrCodeInvalidParams},
		{"unknown source", toolshare.ErrUnknownSource, harnessrpc.ErrCodeInvalidParams},
		{"tool missing", toolshare.ErrToolMissing, harnessrpc.ErrCodeInvalidParams},
		{"manifest read", toolshare.ErrManifestRead, harnessrpc.ErrCodeInvalidParams},
		{"empty identity", toolshare.ErrEmptyResolvedIdentity, harnessrpc.ErrCodeToolUnauthorized},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deps := goodDeps()
			deps.Sharer = &stubSharer{err: c.err}
			h := harnessrpc.NewPromoteShareToolHandler(deps)
			_, err := h(context.Background(), goodParams())
			assertRPCErrCode(t, err, c.code)
		})
	}
}

func TestPromoteShareTool_IdentityResolutionError_InternalError(t *testing.T) {
	// Iter-1 M5 fix (reviewer A): `ErrIdentityResolution` no longer
	// maps to ToolUnauthorized — a resolver outage is an internal
	// error, not an authz failure.
	deps := goodDeps()
	deps.Sharer = &stubSharer{err: toolshare.ErrIdentityResolution}
	h := harnessrpc.NewPromoteShareToolHandler(deps)
	_, err := h(context.Background(), goodParams())
	var rpcErr *harnessrpc.RPCError
	if errors.As(err, &rpcErr) {
		t.Fatalf("err=%v want generic err for dispatcher to default-map to -32603", err)
	}
}

func TestPromoteShareTool_SourceLookupMismatch_InternalError(t *testing.T) {
	// Iter-1 M9 fix (reviewer B): `ErrSourceLookupMismatch` is a
	// programmer / wiring error (resolver returned record with
	// disagreeing Name field). Falls through to InternalError.
	deps := goodDeps()
	deps.Sharer = &stubSharer{err: toolshare.ErrSourceLookupMismatch}
	h := harnessrpc.NewPromoteShareToolHandler(deps)
	_, err := h(context.Background(), goodParams())
	var rpcErr *harnessrpc.RPCError
	if errors.As(err, &rpcErr) {
		t.Fatalf("err=%v want generic err for dispatcher to default-map to -32603", err)
	}
}

func TestPromoteShareTool_GitHubInvalidAuth_Unauthorized(t *testing.T) {
	// Iter-1 n5 fix (reviewer A): github.ErrInvalidAuth surfaces
	// as ToolUnauthorized so the agent caller discriminates a
	// "PAT expired / revoked" condition.
	deps := goodDeps()
	deps.Sharer = &stubSharer{err: github.ErrInvalidAuth}
	h := harnessrpc.NewPromoteShareToolHandler(deps)
	_, err := h(context.Background(), goodParams())
	assertRPCErrCode(t, err, harnessrpc.ErrCodeToolUnauthorized)
}

func TestPromoteShareTool_OverBoundCapabilityToken_InvalidParams(t *testing.T) {
	// Iter-1 n8 fix (reviewer A): cap the token length to defend
	// the broker's hot path against unbounded garbage.
	tok := strings.Repeat("x", harnessrpc.MaxCapabilityTokenLength+1)
	params := json.RawMessage(`{"proposer_id":"a","source_name":"s","tool_name":"t","target_hint":"platform","reason":"r","capability_token":"` + tok + `"}`)
	h := harnessrpc.NewPromoteShareToolHandler(goodDeps())
	_, err := h(context.Background(), params)
	assertRPCErrCode(t, err, harnessrpc.ErrCodeInvalidParams)
}

func TestPromoteShareTool_UnclassifiedShareError_InternalError(t *testing.T) {
	deps := goodDeps()
	deps.Sharer = &stubSharer{err: errors.New("upstream meltdown")}
	h := harnessrpc.NewPromoteShareToolHandler(deps)
	_, err := h(context.Background(), goodParams())
	// Unclassified errors fall through unwrapped — the dispatcher
	// translates non-*RPCError to -32603 InternalError. Test
	// asserts no *RPCError is returned (default mapping).
	var rpcErr *harnessrpc.RPCError
	if errors.As(err, &rpcErr) {
		t.Fatalf("got *RPCError %v want generic err for dispatcher to default-map", err)
	}
	if !strings.Contains(err.Error(), "watchmaster.promote_share_tool") {
		t.Errorf("err=%q does not name method", err.Error())
	}
}

// ---- helpers ----

func assertPanic(t *testing.T, want string, fn func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("expected panic mentioning %q, got none", want)
		}
		msg := fmt.Sprintf("%v", r)
		if !strings.Contains(msg, want) {
			t.Fatalf("panic %q does not mention %q", msg, want)
		}
	}()
	fn()
}
