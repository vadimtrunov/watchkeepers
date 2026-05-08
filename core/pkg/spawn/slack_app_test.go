package spawn_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ── real fakes (M5.6.* / M5.7.* discipline — no mocks of the RPC under test) ──

// fakeSlackAdapter records every CreateApp call and returns either a
// canned AppID or an injected error. Mirrors the M5.6.b
// fakeRememberer / M5.6.c fakeKeepClient pattern — hand-rolled, no
// mocking lib, captures inputs by value so subsequent assertions
// cannot race a still-mutating caller.
type fakeSlackAdapter struct {
	resp    messenger.AppID
	respErr error

	mu    sync.Mutex
	calls []messenger.AppManifest
}

func (f *fakeSlackAdapter) CreateApp(_ context.Context, m messenger.AppManifest) (messenger.AppID, error) {
	f.mu.Lock()
	f.calls = append(f.calls, m)
	f.mu.Unlock()
	if f.respErr != nil {
		return "", f.respErr
	}
	return f.resp, nil
}

func (f *fakeSlackAdapter) recorded() []messenger.AppManifest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]messenger.AppManifest, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeKeepClient is the recording [keeperslog.LocalKeepClient] stand-in
// used to drive a real `*keeperslog.Writer` in the audit-chain
// assertions. Mirrors the keeperslog package's own fakeKeepClient
// (kept private there); reproduced here because that fake is in
// `_test.go` and not exported.
type fakeKeepClient struct {
	respErr error

	mu    sync.Mutex
	calls []keepclient.LogAppendRequest
}

func (f *fakeKeepClient) LogAppend(_ context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	idx := len(f.calls)
	f.mu.Unlock()
	if f.respErr != nil {
		return nil, f.respErr
	}
	return &keepclient.LogAppendResponse{ID: fmt.Sprintf("evt-%d", idx)}, nil
}

func (f *fakeKeepClient) recorded() []keepclient.LogAppendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keepclient.LogAppendRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// stubSecretSource returns fixed values per key. A miss returns
// [keeperslog.ErrInvalidEvent]-shaped error wrapped — using the
// keeperslog sentinel is fine because the spawn package wraps with
// `spawn: secrets:` and tests only assert presence of the wrap.
type stubSecretSource struct {
	values map[string]string
	getErr error
}

func (s *stubSecretSource) Get(_ context.Context, key string) (string, error) {
	if s.getErr != nil {
		return "", s.getErr
	}
	v, ok := s.values[key]
	if !ok {
		return "", errSecretNotFound
	}
	return v, nil
}

var errSecretNotFound = errors.New("secret not found")

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	testOrgID    = "00000000-0000-4000-8000-000000000000"
	testAgentID  = "10000000-0000-4000-8000-000000000000"
	testAppName  = "watchkeeper-bot"
	testAppDesc  = "test bot description"
	testApprovTk = "approval-token-abc123"
)

// validClaim returns a Claim whose AuthorityMatrix grants
// `slack_app_create=lead_approval`. The org id mirrors the M6.1.a
// seed migration's `system` org so a downstream consumer reading the
// row sees the canonical Watchmaster tenant.
func validClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: testOrgID,
		AgentID:        testAgentID,
		AuthorityMatrix: map[string]string{
			"slack_app_create": "lead_approval",
		},
	}
}

// validRequest returns a CreateAppRequest carrying every required
// field (AgentID, AppName, ApprovalToken). Tests mutate the returned
// value to drive negative paths (empty AppName, empty ApprovalToken).
func validRequest() spawn.CreateAppRequest {
	return spawn.CreateAppRequest{
		AgentID:        testAgentID,
		AppName:        testAppName,
		AppDescription: testAppDesc,
		Scopes:         []string{"chat:write", "users:read"},
		ApprovalToken:  testApprovTk,
	}
}

// newRPC constructs a SlackAppRPC over the supplied real-fakes plus a
// real *keeperslog.Writer wrapping `keep`. The secret source carries
// a non-empty value under the production key so the happy-path
// secrets read does not need extra wiring.
func newRPC(t *testing.T, adapter spawn.MessengerAdapter, keep *fakeKeepClient) spawn.SlackAppRPC {
	t.Helper()
	if adapter == nil {
		adapter = &fakeSlackAdapter{resp: "A0123ABCDEF"}
	}
	//nolint:gosec // G101: env var name, not a credential
	src := &stubSecretSource{values: map[string]string{
		"SLACK_APP_CONFIG_TOKEN": "xoxe-test-token",
	}}
	writer := keeperslog.New(keep)
	return spawn.NewSlackAppRPC(adapter, src, writer)
}

// eventTypes pulls the EventType column off every recorded
// LogAppend call. Used by tests pinning the audit-chain order.
func eventTypes(calls []keepclient.LogAppendRequest) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.EventType
	}
	return out
}

// payloadField decodes the JSON payload of `call` and returns the
// `data.<key>` value as a string (or empty if missing). Tests use
// this to assert the audit payload keys without coupling to the
// envelope's JSON ordering.
func payloadField(t *testing.T, call keepclient.LogAppendRequest, key string) string {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(call.Payload, &envelope); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	data, _ := envelope["data"].(map[string]any)
	if data == nil {
		return ""
	}
	v, _ := data[key].(string)
	return v
}

// ── Step 2 — happy path ──────────────────────────────────────────────────────

// TestCreateApp_HappyPath_EmitsTwoEventsAndReturnsAppID drives the
// full RPC happy path and pins the AC2 audit chain (requested →
// succeeded), AC6 chain order, and AC1 return shape. Sub-assertions
// are extracted into focused helpers per M5.6.f gocyclo precedent so
// each branch reads as one named claim instead of a wall of `if`s.
func TestCreateApp_HappyPath_EmitsTwoEventsAndReturnsAppID(t *testing.T) {
	adapter := &fakeSlackAdapter{resp: "A0123ABCDEF"}
	keep := &fakeKeepClient{}
	rpc := newRPC(t, adapter, keep)

	res, err := rpc.CreateApp(context.Background(), validRequest(), validClaim())
	if err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if res.AppID != "A0123ABCDEF" {
		t.Errorf("AppID = %q, want A0123ABCDEF", res.AppID)
	}

	assertAdapterCall(t, adapter)
	logCalls := assertAuditChainOrder(t, keep, []string{
		"watchmaster_slack_app_create_requested",
		"watchmaster_slack_app_create_succeeded",
	})
	assertRequestedPayload(t, logCalls[0])
	assertSucceededPayload(t, logCalls[1], "A0123ABCDEF")
}

// assertAdapterCall pins the [messenger.AppManifest] the privileged
// RPC forwarded to the underlying adapter — exactly one call carrying
// the manifest fields the request supplied.
func assertAdapterCall(t *testing.T, adapter *fakeSlackAdapter) {
	t.Helper()
	gotCalls := adapter.recorded()
	if len(gotCalls) != 1 {
		t.Fatalf("adapter calls = %d, want 1", len(gotCalls))
	}
	if gotCalls[0].Name != testAppName || gotCalls[0].Description != testAppDesc {
		t.Errorf("manifest = %+v, want Name=%q Description=%q", gotCalls[0], testAppName, testAppDesc)
	}
	if len(gotCalls[0].Scopes) != 2 {
		t.Errorf("scopes = %v, want 2 entries", gotCalls[0].Scopes)
	}
}

// assertAuditChainOrder verifies the recorded keepers_log calls match
// `wantTypes` in order and returns the recorded calls so per-event
// payload assertions can run on the same slice without re-fetching.
func assertAuditChainOrder(t *testing.T, keep *fakeKeepClient, wantTypes []string) []keepclient.LogAppendRequest {
	t.Helper()
	logCalls := keep.recorded()
	gotTypes := eventTypes(logCalls)
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("event count = %d (%v), want %d (%v)", len(gotTypes), gotTypes, len(wantTypes), wantTypes)
	}
	for i, want := range wantTypes {
		if gotTypes[i] != want {
			t.Errorf("event[%d] = %q, want %q", i, gotTypes[i], want)
		}
	}
	return logCalls
}

// assertRequestedPayload pins the AC2 payload keys on the `requested`
// event: agent_id, app_name, tool_name, event_class.
func assertRequestedPayload(t *testing.T, call keepclient.LogAppendRequest) {
	t.Helper()
	if got := payloadField(t, call, "agent_id"); got != testAgentID {
		t.Errorf("requested.agent_id = %q, want %q", got, testAgentID)
	}
	if got := payloadField(t, call, "app_name"); got != testAppName {
		t.Errorf("requested.app_name = %q, want %q", got, testAppName)
	}
	if got := payloadField(t, call, "tool_name"); got != "slack_app_create" {
		t.Errorf("requested.tool_name = %q, want %q", got, "slack_app_create")
	}
	if got := payloadField(t, call, "event_class"); got != "requested" {
		t.Errorf("requested.event_class = %q, want %q", got, "requested")
	}
}

// assertSucceededPayload pins the `event_class=succeeded` and
// platform-assigned `app_id` keys on the `succeeded` event.
func assertSucceededPayload(t *testing.T, call keepclient.LogAppendRequest, wantAppID string) {
	t.Helper()
	if got := payloadField(t, call, "event_class"); got != "succeeded" {
		t.Errorf("succeeded.event_class = %q, want %q", got, "succeeded")
	}
	if got := payloadField(t, call, "app_id"); got != wantAppID {
		t.Errorf("succeeded.app_id = %q, want %q", got, wantAppID)
	}
}

// TestCreateApp_HappyPath_ShareCorrelationID pins the M3.6 / M5.6.c
// discipline: requested + succeeded events carry the same
// correlation_id when the caller seeds one on ctx. Locks the audit
// chain together for downstream consumers that group by it.
func TestCreateApp_HappyPath_ShareCorrelationID(t *testing.T) {
	const corrID = "11111111-2222-3333-4444-555555555555"

	adapter := &fakeSlackAdapter{resp: "A0123ABCDEF"}
	keep := &fakeKeepClient{}
	rpc := newRPC(t, adapter, keep)

	ctx := keeperslog.ContextWithCorrelationID(context.Background(), corrID)
	if _, err := rpc.CreateApp(ctx, validRequest(), validClaim()); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}

	calls := keep.recorded()
	if len(calls) != 2 {
		t.Fatalf("call count = %d, want 2", len(calls))
	}
	for i, c := range calls {
		if c.CorrelationID != corrID {
			t.Errorf("event[%d].correlation_id = %q, want %q", i, c.CorrelationID, corrID)
		}
	}
}

// ── Step 2 — negative paths ──────────────────────────────────────────────────

// TestCreateApp_Unauthorized_NoAdapterCall_NoEvent verifies AC2/AC3:
// a claim missing the `slack_app_create=lead_approval` entry is
// rejected with [ErrUnauthorized] BEFORE any adapter call AND
// BEFORE any audit row.
func TestCreateApp_Unauthorized_NoAdapterCall_NoEvent(t *testing.T) {
	cases := []struct {
		name   string
		matrix map[string]string
	}{
		{name: "absent entry", matrix: map[string]string{"other_action": "lead_approval"}},
		{name: "wrong value", matrix: map[string]string{"slack_app_create": "allowed"}},
		{name: "forbidden", matrix: map[string]string{"slack_app_create": "forbidden"}},
		{name: "nil matrix", matrix: nil},
		{name: "empty value", matrix: map[string]string{"slack_app_create": ""}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &fakeSlackAdapter{resp: "A0123ABCDEF"}
			keep := &fakeKeepClient{}
			rpc := newRPC(t, adapter, keep)

			claim := validClaim()
			claim.AuthorityMatrix = tc.matrix
			_, err := rpc.CreateApp(context.Background(), validRequest(), claim)
			if !errors.Is(err, spawn.ErrUnauthorized) {
				t.Fatalf("err = %v, want spawn.ErrUnauthorized", err)
			}
			if got := adapter.recorded(); len(got) != 0 {
				t.Errorf("adapter called %d times on unauthorized path, want 0", len(got))
			}
			if got := keep.recorded(); len(got) != 0 {
				t.Errorf("keepers_log called %d times on unauthorized path, want 0", len(got))
			}
		})
	}
}

// TestCreateApp_EmptyApprovalToken_RejectedNoAdapterNoEvent verifies
// the AC2 ApprovalToken gate: empty token surfaces
// [ErrApprovalRequired] AFTER the unauthorized gate but BEFORE any
// adapter or audit-log work.
func TestCreateApp_EmptyApprovalToken_RejectedNoAdapterNoEvent(t *testing.T) {
	adapter := &fakeSlackAdapter{resp: "A0123ABCDEF"}
	keep := &fakeKeepClient{}
	rpc := newRPC(t, adapter, keep)

	req := validRequest()
	req.ApprovalToken = ""
	_, err := rpc.CreateApp(context.Background(), req, validClaim())
	if !errors.Is(err, spawn.ErrApprovalRequired) {
		t.Fatalf("err = %v, want spawn.ErrApprovalRequired", err)
	}
	if got := adapter.recorded(); len(got) != 0 {
		t.Errorf("adapter called %d times, want 0", len(got))
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times, want 0", len(got))
	}
}

// TestCreateApp_EmptyOrganizationID_RejectedSync verifies the M3.5.a
// tenant-scoping discipline: empty OrganizationID returns
// [ErrInvalidClaim] WITHOUT touching the adapter or the audit log.
func TestCreateApp_EmptyOrganizationID_RejectedSync(t *testing.T) {
	adapter := &fakeSlackAdapter{resp: "A0123ABCDEF"}
	keep := &fakeKeepClient{}
	rpc := newRPC(t, adapter, keep)

	claim := validClaim()
	claim.OrganizationID = ""
	_, err := rpc.CreateApp(context.Background(), validRequest(), claim)
	if !errors.Is(err, spawn.ErrInvalidClaim) {
		t.Fatalf("err = %v, want spawn.ErrInvalidClaim", err)
	}
	if got := adapter.recorded(); len(got) != 0 {
		t.Errorf("adapter called %d times, want 0", len(got))
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times, want 0", len(got))
	}
}

// TestCreateApp_EmptyRequestFields_RejectedSync covers the
// [ErrInvalidRequest] gate: empty AgentID / AppName fail before any
// privileged work.
func TestCreateApp_EmptyRequestFields_RejectedSync(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*spawn.CreateAppRequest)
	}{
		{
			name:   "empty AgentID",
			mutate: func(r *spawn.CreateAppRequest) { r.AgentID = "" },
		},
		{
			name:   "empty AppName",
			mutate: func(r *spawn.CreateAppRequest) { r.AppName = "" },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &fakeSlackAdapter{resp: "A0123ABCDEF"}
			keep := &fakeKeepClient{}
			rpc := newRPC(t, adapter, keep)

			req := validRequest()
			tc.mutate(&req)
			_, err := rpc.CreateApp(context.Background(), req, validClaim())
			if !errors.Is(err, spawn.ErrInvalidRequest) {
				t.Fatalf("err = %v, want spawn.ErrInvalidRequest", err)
			}
			if got := adapter.recorded(); len(got) != 0 {
				t.Errorf("adapter called %d times, want 0", len(got))
			}
			if got := keep.recorded(); len(got) != 0 {
				t.Errorf("keepers_log called %d times, want 0", len(got))
			}
		})
	}
}

// ── Step 2 — failure path (adapter error) ────────────────────────────────────

// TestCreateApp_AdapterError_EmitsRequestedAndFailed verifies AC6's
// failure-path audit chain: a Slack adapter error produces EXACTLY
// 2 events (requested + failed), in that order, and the error is
// surfaced wrapped to the caller.
func TestCreateApp_AdapterError_EmitsRequestedAndFailed(t *testing.T) {
	sentinel := errors.New("slack: rate limited")
	adapter := &fakeSlackAdapter{respErr: sentinel}
	keep := &fakeKeepClient{}
	rpc := newRPC(t, adapter, keep)

	_, err := rpc.CreateApp(context.Background(), validRequest(), validClaim())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain does not contain sentinel: %v", err)
	}

	logCalls := keep.recorded()
	gotTypes := eventTypes(logCalls)
	wantTypes := []string{
		"watchmaster_slack_app_create_requested",
		"watchmaster_slack_app_create_failed",
	}
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("event count = %d (%v), want 2 (%v)", len(gotTypes), gotTypes, wantTypes)
	}
	for i, want := range wantTypes {
		if gotTypes[i] != want {
			t.Errorf("event[%d] = %q, want %q", i, gotTypes[i], want)
		}
	}
	// The failed event must carry an `error_class` payload key.
	if got := payloadField(t, logCalls[1], "error_class"); got == "" {
		t.Errorf("failed.error_class is empty; want a Go type name")
	}
}

// TestCreateApp_RequestedAppendError_AbortsBeforeAdapter pins the
// "no audit row, no privileged action" contract: when the
// `requested` Append fails, the adapter is NEVER called.
func TestCreateApp_RequestedAppendError_AbortsBeforeAdapter(t *testing.T) {
	adapter := &fakeSlackAdapter{resp: "A0123ABCDEF"}
	keep := &fakeKeepClient{respErr: errors.New("keep: db down")}
	rpc := newRPC(t, adapter, keep)

	_, err := rpc.CreateApp(context.Background(), validRequest(), validClaim())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := adapter.recorded(); len(got) != 0 {
		t.Errorf("adapter called %d times despite Append failure, want 0", len(got))
	}
}

// TestCreateApp_SecretsMissing_FailsBeforeAdapterAndAudit pins the
// secrets-source short-circuit: a missing config token aborts BEFORE
// the audit row AND BEFORE the adapter call.
func TestCreateApp_SecretsMissing_FailsBeforeAdapterAndAudit(t *testing.T) {
	adapter := &fakeSlackAdapter{resp: "A0123ABCDEF"}
	keep := &fakeKeepClient{}
	src := &stubSecretSource{values: map[string]string{}} // no SLACK_APP_CONFIG_TOKEN
	writer := keeperslog.New(keep)
	rpc := spawn.NewSlackAppRPC(adapter, src, writer)

	_, err := rpc.CreateApp(context.Background(), validRequest(), validClaim())
	if err == nil {
		t.Fatal("expected error on missing secret, got nil")
	}
	if got := adapter.recorded(); len(got) != 0 {
		t.Errorf("adapter called on missing-secret path, want 0 calls")
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called on missing-secret path, want 0 calls")
	}
}

// ── constructor discipline ───────────────────────────────────────────────────

// TestNewSlackAppRPC_NilAdapter_Panics pins the panic-on-nil-dependency
// contract. Mirrors the keeperslog.New / NewToolErrorReflector
// discipline.
func TestNewSlackAppRPC_NilAdapter_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil adapter, got none")
		}
	}()
	src := &stubSecretSource{}
	writer := keeperslog.New(&fakeKeepClient{})
	_ = spawn.NewSlackAppRPC(nil, src, writer)
}

func TestNewSlackAppRPC_NilSecrets_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil secrets, got none")
		}
	}()
	writer := keeperslog.New(&fakeKeepClient{})
	_ = spawn.NewSlackAppRPC(&fakeSlackAdapter{}, nil, writer)
}

func TestNewSlackAppRPC_NilWriter_Panics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil writer, got none")
		}
	}()
	src := &stubSecretSource{}
	_ = spawn.NewSlackAppRPC(&fakeSlackAdapter{}, src, nil)
}

// TestWithSlackConfigTokenKey_OverridesLookup verifies the option
// threads through to the secrets read.
func TestWithSlackConfigTokenKey_OverridesLookup(t *testing.T) {
	adapter := &fakeSlackAdapter{resp: "A0123ABCDEF"}
	keep := &fakeKeepClient{}
	//nolint:gosec // G101: env var name, not a credential
	src := &stubSecretSource{values: map[string]string{
		"CUSTOM_TOKEN_KEY": "xoxe-custom",
	}}
	writer := keeperslog.New(keep)
	rpc := spawn.NewSlackAppRPC(adapter, src, writer, spawn.WithSlackConfigTokenKey("CUSTOM_TOKEN_KEY"))

	if _, err := rpc.CreateApp(context.Background(), validRequest(), validClaim()); err != nil {
		t.Fatalf("CreateApp: %v", err)
	}
	if got := adapter.recorded(); len(got) != 1 {
		t.Errorf("adapter calls = %d, want 1", len(got))
	}
}

// TestClaim_HasAuthority covers the small predicate inline since it
// is exported and reused by future M6.2 callers (watchkeeper_retire,
// manifest_version_bump).
func TestClaim_HasAuthority(t *testing.T) {
	cases := []struct {
		name   string
		claim  spawn.Claim
		action string
		want   bool
	}{
		{"absent", spawn.Claim{}, "slack_app_create", false},
		{"wrong value", spawn.Claim{AuthorityMatrix: map[string]string{"slack_app_create": "allowed"}}, "slack_app_create", false},
		{"lead_approval", spawn.Claim{AuthorityMatrix: map[string]string{"slack_app_create": "lead_approval"}}, "slack_app_create", true},
		{"different action", spawn.Claim{AuthorityMatrix: map[string]string{"slack_app_create": "lead_approval"}}, "watchkeeper_retire", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.claim.HasAuthority(tc.action); got != tc.want {
				t.Errorf("HasAuthority(%q) = %v, want %v", tc.action, got, tc.want)
			}
		})
	}
}

// TestCreateApp_RespectsContextCancellation pins ctx-cancel
// precedence per the slack package's M4.2.b/c.1 convention.
func TestCreateApp_RespectsContextCancellation(t *testing.T) {
	adapter := &fakeSlackAdapter{resp: "A0123ABCDEF"}
	keep := &fakeKeepClient{}
	rpc := newRPC(t, adapter, keep)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := rpc.CreateApp(ctx, validRequest(), validClaim())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := adapter.recorded(); len(got) != 0 {
		t.Errorf("adapter called on cancelled ctx, want 0 calls")
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called on cancelled ctx, want 0 calls")
	}
}
