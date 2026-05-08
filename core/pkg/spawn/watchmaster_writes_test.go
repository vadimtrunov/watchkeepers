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
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ── real fakes (M5.6 / M6.1.b discipline — no mocks of the unit under test) ──

// fakeWriteClient records every GetManifest / PutManifestVersion /
// UpdateWatchkeeperStatus call and returns canned responses (or
// injected errors). Mirrors the M6.1.b fakeSlackAdapter / M6.2.a
// fakeReadClient shape — hand-rolled, no mocking lib, captures inputs
// by value so subsequent assertions cannot race a still-mutating
// caller.
type fakeWriteClient struct {
	getResp   *keepclient.ManifestVersion
	getErr    error
	putResp   *keepclient.PutManifestVersionResponse
	putErr    error
	updateErr error

	mu          sync.Mutex
	getCalls    []string
	putCalls    []fakeWriteClientPutCall
	updateCalls []fakeWriteClientUpdateCall
}

type fakeWriteClientPutCall struct {
	manifestID string
	req        keepclient.PutManifestVersionRequest
}

type fakeWriteClientUpdateCall struct {
	id     string
	status string
}

func (f *fakeWriteClient) GetManifest(_ context.Context, manifestID string) (*keepclient.ManifestVersion, error) {
	f.mu.Lock()
	f.getCalls = append(f.getCalls, manifestID)
	f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.getResp == nil {
		return &keepclient.ManifestVersion{ID: "mv-default", VersionNo: 1, SystemPrompt: "sp"}, nil
	}
	return f.getResp, nil
}

func (f *fakeWriteClient) PutManifestVersion(
	_ context.Context,
	manifestID string,
	req keepclient.PutManifestVersionRequest,
) (*keepclient.PutManifestVersionResponse, error) {
	f.mu.Lock()
	f.putCalls = append(f.putCalls, fakeWriteClientPutCall{manifestID: manifestID, req: req})
	idx := len(f.putCalls)
	f.mu.Unlock()
	if f.putErr != nil {
		return nil, f.putErr
	}
	if f.putResp == nil {
		return &keepclient.PutManifestVersionResponse{ID: fmt.Sprintf("mv-%d", idx)}, nil
	}
	return f.putResp, nil
}

func (f *fakeWriteClient) UpdateWatchkeeperStatus(_ context.Context, id, status string) error {
	f.mu.Lock()
	f.updateCalls = append(f.updateCalls, fakeWriteClientUpdateCall{id: id, status: status})
	f.mu.Unlock()
	return f.updateErr
}

func (f *fakeWriteClient) recordedUpdate() []fakeWriteClientUpdateCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeWriteClientUpdateCall, len(f.updateCalls))
	copy(out, f.updateCalls)
	return out
}

func (f *fakeWriteClient) recordedGet() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.getCalls))
	copy(out, f.getCalls)
	return out
}

func (f *fakeWriteClient) recordedPut() []fakeWriteClientPutCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeWriteClientPutCall, len(f.putCalls))
	copy(out, f.putCalls)
	return out
}

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	testTargetAgent = "20000000-0000-4000-8000-000000000000"
	testProposeTk   = "approval-token-propose-abc"
	testAdjustTk    = "approval-token-adjust-abc"
)

// proposeClaim returns a claim authorising `propose_spawn`.
func proposeClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: testOrgID,
		AgentID:        testAgentID,
		AuthorityMatrix: map[string]string{
			"propose_spawn": "lead_approval",
		},
	}
}

// adjustPersonalityClaim returns a claim authorising `adjust_personality`.
func adjustPersonalityClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: testOrgID,
		AgentID:        testAgentID,
		AuthorityMatrix: map[string]string{
			"adjust_personality": "lead_approval",
		},
	}
}

// adjustLanguageClaim returns a claim authorising `adjust_language`.
func adjustLanguageClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: testOrgID,
		AgentID:        testAgentID,
		AuthorityMatrix: map[string]string{
			"adjust_language": "lead_approval",
		},
	}
}

// validProposeRequest returns a request carrying every required field.
func validProposeRequest() spawn.ProposeSpawnRequest {
	return spawn.ProposeSpawnRequest{
		AgentID:       testTargetAgent,
		SystemPrompt:  "you are a watchkeeper",
		Personality:   "diligent",
		Language:      "en",
		ApprovalToken: testProposeTk,
	}
}

// validAdjustPersonalityRequest returns a request carrying every required field.
func validAdjustPersonalityRequest() spawn.AdjustPersonalityRequest {
	return spawn.AdjustPersonalityRequest{
		AgentID:        testTargetAgent,
		NewPersonality: "introspective",
		ApprovalToken:  testAdjustTk,
	}
}

// validAdjustLanguageRequest returns a request carrying every required field.
func validAdjustLanguageRequest() spawn.AdjustLanguageRequest {
	return spawn.AdjustLanguageRequest{
		AgentID:       testTargetAgent,
		NewLanguage:   "fr",
		ApprovalToken: testAdjustTk,
	}
}

// newWriter constructs a real *keeperslog.Writer over the supplied
// recording fake LocalKeepClient (`fakeKeepClient` from
// slack_app_test.go). Mirrors the M6.1.b newRPC helper pattern.
func newWriter(t *testing.T, keep *fakeKeepClient) *keeperslog.Writer {
	t.Helper()
	return keeperslog.New(keep)
}

// existingManifest returns a representative latest manifest_version
// row used by the adjust-* happy-path tests. Carries every persisted
// field set so the copy-and-bump path can assert that none drop on
// the bump.
func existingManifest() *keepclient.ManifestVersion {
	return &keepclient.ManifestVersion{
		ID:                         "mv-prev",
		ManifestID:                 testTargetAgent,
		VersionNo:                  3,
		SystemPrompt:               "you are a watchkeeper",
		Tools:                      json.RawMessage(`["remember"]`),
		AuthorityMatrix:            json.RawMessage(`{}`),
		KnowledgeSources:           json.RawMessage(`[]`),
		Personality:                "old personality",
		Language:                   "en",
		Model:                      "gpt-4",
		Autonomy:                   "supervised",
		NotebookTopK:               5,
		NotebookRelevanceThreshold: 0.7,
	}
}

// payloadFieldAny decodes `data.<key>` as `any` so callers can assert
// on non-string values (bool, number) without re-decoding twice.
func payloadFieldAny(t *testing.T, call keepclient.LogAppendRequest, key string) any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(call.Payload, &envelope); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	data, _ := envelope["data"].(map[string]any)
	if data == nil {
		return nil
	}
	return data[key]
}

// ── ProposeSpawn — happy path ────────────────────────────────────────────────

// TestProposeSpawn_HappyPath_EmitsTwoEventsAndReturnsVersionID drives
// the propose_spawn happy path and pins:
//   - the AC2 audit chain (requested → succeeded);
//   - AC6 chain order;
//   - the AC1 return shape;
//   - the keepclient.PutManifestVersion is called with VersionNo=1
//     plus the proposed personality/language fields.
func TestProposeSpawn_HappyPath_EmitsTwoEventsAndReturnsVersionID(t *testing.T) {
	client := &fakeWriteClient{
		putResp: &keepclient.PutManifestVersionResponse{ID: "mv-new"},
	}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	res, err := spawn.ProposeSpawn(
		context.Background(),
		client,
		writer,
		validProposeRequest(),
		proposeClaim(),
	)
	if err != nil {
		t.Fatalf("ProposeSpawn: %v", err)
	}
	if res.ManifestVersionID != "mv-new" {
		t.Errorf("ManifestVersionID = %q, want mv-new", res.ManifestVersionID)
	}
	if res.VersionNo != 1 {
		t.Errorf("VersionNo = %d, want 1", res.VersionNo)
	}

	put := client.recordedPut()
	if len(put) != 1 {
		t.Fatalf("PutManifestVersion calls = %d, want 1", len(put))
	}
	if put[0].manifestID != testTargetAgent {
		t.Errorf("manifestID = %q, want %q", put[0].manifestID, testTargetAgent)
	}
	if put[0].req.VersionNo != 1 {
		t.Errorf("VersionNo on put = %d, want 1", put[0].req.VersionNo)
	}
	if put[0].req.Personality != "diligent" {
		t.Errorf("Personality = %q, want diligent", put[0].req.Personality)
	}
	if put[0].req.Language != "en" {
		t.Errorf("Language = %q, want en", put[0].req.Language)
	}

	logCalls := assertAuditChainOrder(t, keep, []string{
		"watchmaster_propose_spawn_requested",
		"watchmaster_propose_spawn_succeeded",
	})
	if got := payloadField(t, logCalls[0], "tool_name"); got != "propose_spawn" {
		t.Errorf("requested.tool_name = %q, want propose_spawn", got)
	}
	if got := payloadField(t, logCalls[0], "agent_id"); got != testTargetAgent {
		t.Errorf("requested.agent_id = %q, want %q", got, testTargetAgent)
	}
	if got := payloadFieldAny(t, logCalls[0], "approval_token_present"); got != true {
		t.Errorf("requested.approval_token_present = %v, want true", got)
	}
	if got := payloadField(t, logCalls[1], "manifest_version_id"); got != "mv-new" {
		t.Errorf("succeeded.manifest_version_id = %q, want mv-new", got)
	}
}

// ── ProposeSpawn — negative paths ────────────────────────────────────────────

// TestProposeSpawn_Unauthorized_NoPutNoEvent verifies AC7: a claim
// missing `propose_spawn=lead_approval` is rejected BEFORE any keep
// write or audit row.
func TestProposeSpawn_Unauthorized_NoPutNoEvent(t *testing.T) {
	cases := []struct {
		name   string
		matrix map[string]string
	}{
		{name: "absent entry", matrix: map[string]string{"other_action": "lead_approval"}},
		{name: "wrong value", matrix: map[string]string{"propose_spawn": "allowed"}},
		{name: "nil matrix", matrix: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeWriteClient{}
			keep := &fakeKeepClient{}
			writer := newWriter(t, keep)
			claim := proposeClaim()
			claim.AuthorityMatrix = tc.matrix

			_, err := spawn.ProposeSpawn(context.Background(), client, writer, validProposeRequest(), claim)
			if !errors.Is(err, spawn.ErrUnauthorized) {
				t.Fatalf("err = %v, want ErrUnauthorized", err)
			}
			if got := client.recordedPut(); len(got) != 0 {
				t.Errorf("PutManifestVersion called %d times on unauthorized path", len(got))
			}
			if got := keep.recorded(); len(got) != 0 {
				t.Errorf("keepers_log called %d times on unauthorized path", len(got))
			}
		})
	}
}

// TestProposeSpawn_EmptyApprovalToken_RejectedNoPutNoEvent verifies
// AC7's approval-token gate: empty token surfaces ErrApprovalRequired
// AFTER the unauthorized gate but BEFORE any keep work.
func TestProposeSpawn_EmptyApprovalToken_RejectedNoPutNoEvent(t *testing.T) {
	client := &fakeWriteClient{}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	req := validProposeRequest()
	req.ApprovalToken = ""
	_, err := spawn.ProposeSpawn(context.Background(), client, writer, req, proposeClaim())
	if !errors.Is(err, spawn.ErrApprovalRequired) {
		t.Fatalf("err = %v, want ErrApprovalRequired", err)
	}
	if got := client.recordedPut(); len(got) != 0 {
		t.Errorf("PutManifestVersion called %d times, want 0", len(got))
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times, want 0", len(got))
	}
}

// TestProposeSpawn_EmptyOrganizationID_RejectedSync verifies M3.5.a
// tenant-scoping: empty OrganizationID returns ErrInvalidClaim.
func TestProposeSpawn_EmptyOrganizationID_RejectedSync(t *testing.T) {
	client := &fakeWriteClient{}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)
	claim := proposeClaim()
	claim.OrganizationID = ""

	_, err := spawn.ProposeSpawn(context.Background(), client, writer, validProposeRequest(), claim)
	if !errors.Is(err, spawn.ErrInvalidClaim) {
		t.Fatalf("err = %v, want ErrInvalidClaim", err)
	}
	if got := client.recordedPut(); len(got) != 0 {
		t.Errorf("PutManifestVersion called %d times, want 0", len(got))
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times, want 0", len(got))
	}
}

// TestProposeSpawn_EmptyRequestFields_RejectedSync covers
// ErrInvalidRequest gates: empty AgentID / SystemPrompt fail
// synchronously before any privileged work.
func TestProposeSpawn_EmptyRequestFields_RejectedSync(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*spawn.ProposeSpawnRequest)
	}{
		{"empty AgentID", func(r *spawn.ProposeSpawnRequest) { r.AgentID = "" }},
		{"empty SystemPrompt", func(r *spawn.ProposeSpawnRequest) { r.SystemPrompt = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeWriteClient{}
			keep := &fakeKeepClient{}
			writer := newWriter(t, keep)
			req := validProposeRequest()
			tc.mutate(&req)

			_, err := spawn.ProposeSpawn(context.Background(), client, writer, req, proposeClaim())
			if !errors.Is(err, spawn.ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if got := client.recordedPut(); len(got) != 0 {
				t.Errorf("PutManifestVersion called %d times, want 0", len(got))
			}
		})
	}
}

// TestProposeSpawn_PutManifestVersionError_EmitsRequestedAndFailed
// verifies AC6's failure-path audit chain: a PutManifestVersion error
// produces EXACTLY 2 events (requested + failed), in that order.
func TestProposeSpawn_PutManifestVersionError_EmitsRequestedAndFailed(t *testing.T) {
	sentinel := errors.New("keepclient: db down")
	client := &fakeWriteClient{putErr: sentinel}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	_, err := spawn.ProposeSpawn(context.Background(), client, writer, validProposeRequest(), proposeClaim())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain does not contain sentinel: %v", err)
	}

	logCalls := assertAuditChainOrder(t, keep, []string{
		"watchmaster_propose_spawn_requested",
		"watchmaster_propose_spawn_failed",
	})
	if got := payloadField(t, logCalls[1], "error_class"); got == "" {
		t.Errorf("failed.error_class is empty; want a Go type name")
	}
}

// TestProposeSpawn_RespectsContextCancellation pins ctx-cancel
// precedence (matches the M6.1.b convention).
func TestProposeSpawn_RespectsContextCancellation(t *testing.T) {
	client := &fakeWriteClient{}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := spawn.ProposeSpawn(ctx, client, writer, validProposeRequest(), proposeClaim())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := client.recordedPut(); len(got) != 0 {
		t.Errorf("PutManifestVersion called on cancelled ctx, want 0 calls")
	}
}

// ── AdjustPersonality — happy path + sentinels ──────────────────────────────

// TestAdjustPersonality_HappyPath_CopiesAndBumpsVersion verifies the
// adjust-flow end-to-end: read existing, copy fields, override
// Personality, bump VersionNo, write new row, emit 2 audit events.
func TestAdjustPersonality_HappyPath_CopiesAndBumpsVersion(t *testing.T) {
	client := &fakeWriteClient{
		getResp: existingManifest(),
		putResp: &keepclient.PutManifestVersionResponse{ID: "mv-bumped"},
	}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	res, err := spawn.AdjustPersonality(context.Background(), client, writer, validAdjustPersonalityRequest(), adjustPersonalityClaim())
	if err != nil {
		t.Fatalf("AdjustPersonality: %v", err)
	}
	assertBumpResult(t, res, "mv-bumped", 4)
	assertGetCalledOnce(t, client, testTargetAgent)
	pr := assertPutCalledOnce(t, client)
	assertPersonalityOverride(t, pr)
	assertCopiedFields(t, pr)
	assertAdjustPersonalityAuditChain(t, keep)
}

// assertBumpResult pins the result-shape return: ManifestVersionID and
// VersionNo must match the caller's expectation.
func assertBumpResult(t *testing.T, res spawn.ManifestBumpResult, wantID string, wantVer int) {
	t.Helper()
	if res.ManifestVersionID != wantID || res.VersionNo != wantVer {
		t.Errorf("result = %+v, want {ManifestVersionID:%s, VersionNo:%d}", res, wantID, wantVer)
	}
}

// assertGetCalledOnce verifies GetManifest fired exactly once with the
// expected manifest id.
func assertGetCalledOnce(t *testing.T, client *fakeWriteClient, wantID string) {
	t.Helper()
	gets := client.recordedGet()
	if len(gets) != 1 || gets[0] != wantID {
		t.Errorf("GetManifest calls = %v, want [%s]", gets, wantID)
	}
}

// assertPutCalledOnce verifies PutManifestVersion fired exactly once
// and returns the recorded request for further per-field assertions.
func assertPutCalledOnce(t *testing.T, client *fakeWriteClient) keepclient.PutManifestVersionRequest {
	t.Helper()
	puts := client.recordedPut()
	if len(puts) != 1 {
		t.Fatalf("PutManifestVersion calls = %d, want 1", len(puts))
	}
	return puts[0].req
}

// assertPersonalityOverride pins the AdjustPersonality-specific
// override + bumped VersionNo on the put-shape request.
func assertPersonalityOverride(t *testing.T, pr keepclient.PutManifestVersionRequest) {
	t.Helper()
	if pr.VersionNo != 4 {
		t.Errorf("VersionNo on put = %d, want 4", pr.VersionNo)
	}
	if pr.Personality != "introspective" {
		t.Errorf("Personality = %q, want introspective (override)", pr.Personality)
	}
}

// assertCopiedFields pins the AC1 copy-discipline contract: every
// non-targeted field must round-trip from the existing manifest into
// the put-shape request.
func assertCopiedFields(t *testing.T, pr keepclient.PutManifestVersionRequest) {
	t.Helper()
	if pr.SystemPrompt != "you are a watchkeeper" {
		t.Errorf("SystemPrompt = %q, want copy of existing", pr.SystemPrompt)
	}
	if pr.Language != "en" {
		t.Errorf("Language = %q, want copy of existing", pr.Language)
	}
	if pr.Model != "gpt-4" {
		t.Errorf("Model = %q, want copy of existing", pr.Model)
	}
	if pr.Autonomy != "supervised" {
		t.Errorf("Autonomy = %q, want copy of existing", pr.Autonomy)
	}
	if pr.NotebookTopK != 5 || pr.NotebookRelevanceThreshold != 0.7 {
		t.Errorf("Notebook* = (%d, %v), want (5, 0.7)", pr.NotebookTopK, pr.NotebookRelevanceThreshold)
	}
}

// assertAdjustPersonalityAuditChain pins the 2-event audit chain +
// payload keys for the AdjustPersonality happy path.
func assertAdjustPersonalityAuditChain(t *testing.T, keep *fakeKeepClient) {
	t.Helper()
	logCalls := assertAuditChainOrder(t, keep, []string{
		"watchmaster_adjust_personality_requested",
		"watchmaster_adjust_personality_succeeded",
	})
	if got := payloadField(t, logCalls[0], "tool_name"); got != "adjust_personality" {
		t.Errorf("requested.tool_name = %q, want adjust_personality", got)
	}
	if got := payloadFieldAny(t, logCalls[1], "version_no"); got != float64(4) {
		t.Errorf("succeeded.version_no = %v, want 4", got)
	}
}

// TestAdjustPersonality_Unauthorized_NoGetNoPutNoEvent pins the AC7
// claim-authority gate: an unauthorised claim is rejected BEFORE any
// keep call.
func TestAdjustPersonality_Unauthorized_NoGetNoPutNoEvent(t *testing.T) {
	client := &fakeWriteClient{getResp: existingManifest()}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)
	claim := adjustPersonalityClaim()
	claim.AuthorityMatrix = nil

	_, err := spawn.AdjustPersonality(context.Background(), client, writer, validAdjustPersonalityRequest(), claim)
	if !errors.Is(err, spawn.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if got := client.recordedGet(); len(got) != 0 {
		t.Errorf("GetManifest called %d times on unauthorized path", len(got))
	}
	if got := client.recordedPut(); len(got) != 0 {
		t.Errorf("PutManifestVersion called %d times on unauthorized path", len(got))
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times on unauthorized path", len(got))
	}
}

// TestAdjustPersonality_EmptyApprovalToken_Rejected pins the AC7
// approval-token gate.
func TestAdjustPersonality_EmptyApprovalToken_Rejected(t *testing.T) {
	client := &fakeWriteClient{getResp: existingManifest()}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)
	req := validAdjustPersonalityRequest()
	req.ApprovalToken = ""

	_, err := spawn.AdjustPersonality(context.Background(), client, writer, req, adjustPersonalityClaim())
	if !errors.Is(err, spawn.ErrApprovalRequired) {
		t.Fatalf("err = %v, want ErrApprovalRequired", err)
	}
	if got := client.recordedGet(); len(got) != 0 {
		t.Errorf("GetManifest called %d times, want 0", len(got))
	}
}

// TestAdjustPersonality_PutError_EmitsRequestedAndFailed pins AC6:
// failure-path audit chain has exactly 2 events.
func TestAdjustPersonality_PutError_EmitsRequestedAndFailed(t *testing.T) {
	sentinel := errors.New("keepclient: 409 conflict")
	client := &fakeWriteClient{getResp: existingManifest(), putErr: sentinel}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	_, err := spawn.AdjustPersonality(context.Background(), client, writer, validAdjustPersonalityRequest(), adjustPersonalityClaim())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain does not contain sentinel: %v", err)
	}
	assertAuditChainOrder(t, keep, []string{
		"watchmaster_adjust_personality_requested",
		"watchmaster_adjust_personality_failed",
	})
}

// TestAdjustPersonality_GetError_NoAuditChain pins the
// "GetManifest BEFORE the audit chain" contract: a read failure on
// the prior manifest aborts WITHOUT emitting any audit row, since the
// privileged action never reaches the `requested` boundary.
func TestAdjustPersonality_GetError_NoAuditChain(t *testing.T) {
	client := &fakeWriteClient{getErr: errors.New("keepclient: 404 not found")}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	_, err := spawn.AdjustPersonality(context.Background(), client, writer, validAdjustPersonalityRequest(), adjustPersonalityClaim())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times on get-error path, want 0", len(got))
	}
	if got := client.recordedPut(); len(got) != 0 {
		t.Errorf("PutManifestVersion called %d times on get-error path, want 0", len(got))
	}
}

// ── AdjustLanguage — happy path + sentinels ──────────────────────────────────

// TestAdjustLanguage_HappyPath_CopiesAndBumpsVersion mirrors the
// AdjustPersonality happy path but pins the Language override.
func TestAdjustLanguage_HappyPath_CopiesAndBumpsVersion(t *testing.T) {
	client := &fakeWriteClient{
		getResp: existingManifest(),
		putResp: &keepclient.PutManifestVersionResponse{ID: "mv-bumped"},
	}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	res, err := spawn.AdjustLanguage(context.Background(), client, writer, validAdjustLanguageRequest(), adjustLanguageClaim())
	if err != nil {
		t.Fatalf("AdjustLanguage: %v", err)
	}
	if res.ManifestVersionID != "mv-bumped" || res.VersionNo != 4 {
		t.Errorf("result = %+v, want {ManifestVersionID:mv-bumped, VersionNo:4}", res)
	}

	puts := client.recordedPut()
	if len(puts) != 1 {
		t.Fatalf("PutManifestVersion calls = %d, want 1", len(puts))
	}
	pr := puts[0].req
	if pr.Language != "fr" {
		t.Errorf("Language = %q, want fr (override)", pr.Language)
	}
	if pr.Personality != "old personality" {
		t.Errorf("Personality = %q, want copy of existing (untouched)", pr.Personality)
	}
	if pr.VersionNo != 4 {
		t.Errorf("VersionNo on put = %d, want 4", pr.VersionNo)
	}

	assertAuditChainOrder(t, keep, []string{
		"watchmaster_adjust_language_requested",
		"watchmaster_adjust_language_succeeded",
	})
}

// TestAdjustLanguage_Unauthorized_NoPut pins the claim-authority gate
// for the language tool.
func TestAdjustLanguage_Unauthorized_NoPut(t *testing.T) {
	client := &fakeWriteClient{getResp: existingManifest()}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)
	claim := adjustLanguageClaim()
	claim.AuthorityMatrix = map[string]string{"adjust_personality": "lead_approval"} // wrong action

	_, err := spawn.AdjustLanguage(context.Background(), client, writer, validAdjustLanguageRequest(), claim)
	if !errors.Is(err, spawn.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if got := client.recordedGet(); len(got) != 0 {
		t.Errorf("GetManifest called %d times, want 0", len(got))
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times, want 0", len(got))
	}
}

// TestAdjustLanguage_EmptyOrganizationID_Rejected pins the M3.5.a gate.
func TestAdjustLanguage_EmptyOrganizationID_Rejected(t *testing.T) {
	client := &fakeWriteClient{}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)
	claim := adjustLanguageClaim()
	claim.OrganizationID = ""

	_, err := spawn.AdjustLanguage(context.Background(), client, writer, validAdjustLanguageRequest(), claim)
	if !errors.Is(err, spawn.ErrInvalidClaim) {
		t.Fatalf("err = %v, want ErrInvalidClaim", err)
	}
}

// TestAdjustLanguage_PutError_EmitsRequestedAndFailed pins AC6 for the
// language tool.
func TestAdjustLanguage_PutError_EmitsRequestedAndFailed(t *testing.T) {
	sentinel := errors.New("keepclient: 500")
	client := &fakeWriteClient{getResp: existingManifest(), putErr: sentinel}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	_, err := spawn.AdjustLanguage(context.Background(), client, writer, validAdjustLanguageRequest(), adjustLanguageClaim())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	assertAuditChainOrder(t, keep, []string{
		"watchmaster_adjust_language_requested",
		"watchmaster_adjust_language_failed",
	})
}

// ── shared ───────────────────────────────────────────────────────────────────

// TestManifestBumpTools_NilClient_ReturnsInvalidRequest pins the
// dependency-discipline contract: a nil client surfaces as
// ErrInvalidRequest BEFORE any privileged work.
func TestManifestBumpTools_NilClient_ReturnsInvalidRequest(t *testing.T) {
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	_, err := spawn.ProposeSpawn(context.Background(), nil, writer, validProposeRequest(), proposeClaim())
	if !errors.Is(err, spawn.ErrInvalidRequest) {
		t.Errorf("ProposeSpawn nil-client err = %v, want ErrInvalidRequest", err)
	}
	_, err = spawn.AdjustPersonality(context.Background(), nil, writer, validAdjustPersonalityRequest(), adjustPersonalityClaim())
	if !errors.Is(err, spawn.ErrInvalidRequest) {
		t.Errorf("AdjustPersonality nil-client err = %v, want ErrInvalidRequest", err)
	}
	_, err = spawn.AdjustLanguage(context.Background(), nil, writer, validAdjustLanguageRequest(), adjustLanguageClaim())
	if !errors.Is(err, spawn.ErrInvalidRequest) {
		t.Errorf("AdjustLanguage nil-client err = %v, want ErrInvalidRequest", err)
	}
}

// TestManifestBumpTools_ShareCorrelationID pins the M3.6 / M5.6.c
// discipline: requested + succeeded events carry the same
// correlation_id when the caller seeds one on ctx.
func TestManifestBumpTools_ShareCorrelationID(t *testing.T) {
	const corrID = "11111111-2222-3333-4444-666666666666"
	client := &fakeWriteClient{
		getResp: existingManifest(),
		putResp: &keepclient.PutManifestVersionResponse{ID: "mv-new"},
	}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	ctx := keeperslog.ContextWithCorrelationID(context.Background(), corrID)
	if _, err := spawn.AdjustPersonality(ctx, client, writer, validAdjustPersonalityRequest(), adjustPersonalityClaim()); err != nil {
		t.Fatalf("AdjustPersonality: %v", err)
	}
	calls := keep.recorded()
	if len(calls) != 2 {
		t.Fatalf("audit calls = %d, want 2", len(calls))
	}
	for i, c := range calls {
		if c.CorrelationID != corrID {
			t.Errorf("event[%d].correlation_id = %q, want %q", i, c.CorrelationID, corrID)
		}
	}
}
