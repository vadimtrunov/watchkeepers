package spawn_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ── real fakes (M5.6 / M6.1.b / M6.2.b discipline — no mocks of unit under test) ──

// fakeNotebookClient records every PromoteToKeep call and returns
// either a canned [*notebook.Proposal] or an injected error. Stub of
// the [WatchmasterNotebookClient] seam, NOT a mock of
// notebook.DB.PromoteToKeep — that has its own real-fakes suite in
// core/pkg/notebook/.
type fakeNotebookClient struct {
	resp *notebook.Proposal
	err  error

	mu    sync.Mutex
	calls []string
}

func (f *fakeNotebookClient) PromoteToKeep(_ context.Context, entryID string) (*notebook.Proposal, error) {
	f.mu.Lock()
	f.calls = append(f.calls, entryID)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return f.resp, nil
}

func (f *fakeNotebookClient) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// ── helpers ──────────────────────────────────────────────────────────────────

const (
	testNotebookEntryID = "30000000-0000-7000-8000-000000000000"
	testProposalID      = "40000000-0000-7000-8000-000000000000"
	testPromoteTk       = "approval-token-promote-abc"
)

// promoteClaim returns a claim authorising `promote_to_keep`.
func promoteClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: testOrgID,
		AgentID:        testAgentID,
		AuthorityMatrix: map[string]string{
			"promote_to_keep": "lead_approval",
		},
	}
}

// validPromoteRequest returns a request carrying every required field.
func validPromoteRequest() spawn.PromoteToKeepRequest {
	return spawn.PromoteToKeepRequest{
		AgentID:         testTargetAgent,
		NotebookEntryID: testNotebookEntryID,
		ApprovalToken:   testPromoteTk,
	}
}

// canonicalProposal returns a canned [*notebook.Proposal] the
// fakeNotebookClient hands back on the happy path. Carries every
// provenance field set so the domain-event payload assertions can
// pin each one.
func canonicalProposal() *notebook.Proposal {
	return &notebook.Proposal{
		Subject:         "lesson subject",
		Content:         "lesson content body — must NOT leak into audit",
		Embedding:       []float32{0.1, 0.2, 0.3},
		ProposalID:      testProposalID,
		AgentID:         testAgentID,
		NotebookEntryID: testNotebookEntryID,
		Category:        "lesson",
		Scope:           notebook.ScopeOrg,
		SourceCreatedAt: 1700000000000,
		ProposedAt:      1700000001000,
	}
}

// ── PromoteToKeep — happy path (test plan #1, #9, #10) ──────────────────────

// TestPromoteToKeep_HappyPath_EmitsThreeEventsAndReturnsChunkID drives
// the promote_to_keep happy path and pins:
//   - AC1 return shape (ChunkID, ProposalID, NotebookEntryID);
//   - AC2 audit chain: 3 events in order (requested →
//     notebook_promoted_to_keep → succeeded);
//   - AC7 chain order;
//   - the keepclient.Store is called with the proposal's
//     Subject/Content/Embedding;
//   - test plan #10 PII guard: audit payloads contain neither `content`
//     nor `embedding` keys.
//
// Sub-assertions are extracted into focused helpers per the M5.6.f
// gocyclo precedent so each branch reads as one named claim instead of
// a wall of `if`s.
func TestPromoteToKeep_HappyPath_EmitsThreeEventsAndReturnsChunkID(t *testing.T) {
	client := &fakeWriteClient{
		storeResp: &keepclient.StoreResponse{ID: "chunk-new"},
	}
	notebookCli := &fakeNotebookClient{resp: canonicalProposal()}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	res, err := spawn.PromoteToKeep(
		context.Background(),
		client,
		notebookCli,
		writer,
		validPromoteRequest(),
		promoteClaim(),
	)
	if err != nil {
		t.Fatalf("PromoteToKeep: %v", err)
	}

	assertPromoteResult(t, res, "chunk-new")
	assertNotebookCalledOnce(t, notebookCli, testNotebookEntryID)
	assertStoreCalledOnce(t, client)

	logCalls := assertAuditChainOrder(t, keep, []string{
		"watchmaster_promote_to_keep_requested",
		"notebook_promoted_to_keep",
		"watchmaster_promote_to_keep_succeeded",
	})
	assertPromoteRequestedPayload(t, logCalls[0])
	assertPromoteDomainPayload(t, logCalls[1], "chunk-new")
	assertPromoteSucceededPayload(t, logCalls[2], "chunk-new")
	assertNoPIIInPayloads(t, logCalls)
}

// assertPromoteResult pins the AC1 return-shape: ChunkID + ProposalID +
// NotebookEntryID must match the caller's expectation.
func assertPromoteResult(t *testing.T, res spawn.PromoteToKeepResult, wantChunkID string) {
	t.Helper()
	if res.ChunkID != wantChunkID {
		t.Errorf("ChunkID = %q, want %q", res.ChunkID, wantChunkID)
	}
	if res.ProposalID != testProposalID {
		t.Errorf("ProposalID = %q, want %q", res.ProposalID, testProposalID)
	}
	if res.NotebookEntryID != testNotebookEntryID {
		t.Errorf("NotebookEntryID = %q, want %q", res.NotebookEntryID, testNotebookEntryID)
	}
}

// assertNotebookCalledOnce pins exactly-one notebook PromoteToKeep call.
func assertNotebookCalledOnce(t *testing.T, n *fakeNotebookClient, wantID string) {
	t.Helper()
	got := n.recorded()
	if len(got) != 1 || got[0] != wantID {
		t.Errorf("notebook.PromoteToKeep calls = %v, want [%s]", got, wantID)
	}
}

// assertStoreCalledOnce pins exactly-one keep Store call carrying the
// canned proposal's Subject/Content/Embedding fields verbatim.
func assertStoreCalledOnce(t *testing.T, client *fakeWriteClient) {
	t.Helper()
	stores := client.recordedStore()
	if len(stores) != 1 {
		t.Fatalf("Store calls = %d, want 1", len(stores))
	}
	if stores[0].Subject != "lesson subject" {
		t.Errorf("Store.Subject = %q, want lesson subject", stores[0].Subject)
	}
	if stores[0].Content != "lesson content body — must NOT leak into audit" {
		t.Errorf("Store.Content = %q, want canonical body", stores[0].Content)
	}
	if len(stores[0].Embedding) != 3 {
		t.Errorf("Store.Embedding len = %d, want 3", len(stores[0].Embedding))
	}
}

// assertPromoteRequestedPayload pins the AC2 payload keys on the
// `requested` event.
func assertPromoteRequestedPayload(t *testing.T, call keepclient.LogAppendRequest) {
	t.Helper()
	if got := payloadField(t, call, "tool_name"); got != "promote_to_keep" {
		t.Errorf("requested.tool_name = %q, want promote_to_keep", got)
	}
	if got := payloadField(t, call, "agent_id"); got != testTargetAgent {
		t.Errorf("requested.agent_id = %q, want %q", got, testTargetAgent)
	}
	if got := payloadField(t, call, "notebook_entry_id"); got != testNotebookEntryID {
		t.Errorf("requested.notebook_entry_id = %q, want %q", got, testNotebookEntryID)
	}
	if got := payloadField(t, call, "proposal_id"); got != testProposalID {
		t.Errorf("requested.proposal_id = %q, want %q", got, testProposalID)
	}
	if got := payloadFieldAny(t, call, "approval_token_present"); got != true {
		t.Errorf("requested.approval_token_present = %v, want true", got)
	}
}

// assertPromoteDomainPayload pins every AC2 provenance field on the
// `notebook_promoted_to_keep` domain event.
func assertPromoteDomainPayload(t *testing.T, call keepclient.LogAppendRequest, wantChunkID string) {
	t.Helper()
	if got := payloadField(t, call, "notebook_entry_id"); got != testNotebookEntryID {
		t.Errorf("domain.notebook_entry_id = %q, want %q", got, testNotebookEntryID)
	}
	if got := payloadField(t, call, "proposal_id"); got != testProposalID {
		t.Errorf("domain.proposal_id = %q, want %q", got, testProposalID)
	}
	if got := payloadField(t, call, "chunk_id"); got != wantChunkID {
		t.Errorf("domain.chunk_id = %q, want %q", got, wantChunkID)
	}
	if got := payloadField(t, call, "subject"); got != "lesson subject" {
		t.Errorf("domain.subject = %q, want lesson subject", got)
	}
	if got := payloadField(t, call, "category"); got != "lesson" {
		t.Errorf("domain.category = %q, want lesson", got)
	}
	if got := payloadField(t, call, "scope"); got != "org" {
		t.Errorf("domain.scope = %q, want org", got)
	}
	if got := payloadFieldAny(t, call, "source_created_at"); got != float64(1700000000000) {
		t.Errorf("domain.source_created_at = %v, want 1700000000000", got)
	}
	if got := payloadFieldAny(t, call, "proposed_at"); got != float64(1700000001000) {
		t.Errorf("domain.proposed_at = %v, want 1700000001000", got)
	}
	if got := payloadFieldAny(t, call, "promoted_at"); got == nil {
		t.Errorf("domain.promoted_at missing, want non-nil")
	}
}

// assertPromoteSucceededPayload pins the `event_class=succeeded` and
// `chunk_id` keys on the `succeeded` event.
func assertPromoteSucceededPayload(t *testing.T, call keepclient.LogAppendRequest, wantChunkID string) {
	t.Helper()
	if got := payloadField(t, call, "chunk_id"); got != wantChunkID {
		t.Errorf("succeeded.chunk_id = %q, want %q", got, wantChunkID)
	}
	if got := payloadField(t, call, "event_class"); got != "succeeded" {
		t.Errorf("succeeded.event_class = %q, want succeeded", got)
	}
}

// assertNoPIIInPayloads pins the M2b.7 PII discipline: audit payloads
// MUST NOT carry `content` or `embedding` keys on ANY event of the
// audit chain.
func assertNoPIIInPayloads(t *testing.T, logCalls []keepclient.LogAppendRequest) {
	t.Helper()
	for i, call := range logCalls {
		if hasPayloadKey(t, call, "content") {
			t.Errorf("event[%d] (%s) leaked `content` into audit payload — PII violation", i, call.EventType)
		}
		if hasPayloadKey(t, call, "embedding") {
			t.Errorf("event[%d] (%s) leaked `embedding` into audit payload — PII violation", i, call.EventType)
		}
	}
}

// hasPayloadKey returns true when `data.<key>` exists in the call's
// envelope. Used by the PII regression test to assert key absence
// without coupling to a specific value.
func hasPayloadKey(t *testing.T, call keepclient.LogAppendRequest, key string) bool {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(call.Payload, &envelope); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	data, _ := envelope["data"].(map[string]any)
	if data == nil {
		return false
	}
	_, ok := data[key]
	return ok
}

// ── PromoteToKeep — negative paths (test plan #2-#5) ────────────────────────

// TestPromoteToKeep_Unauthorized_NoNotebookCallNoStoreNoEvent verifies
// AC8: a claim missing `promote_to_keep=lead_approval` is rejected
// BEFORE any notebook read, keep write, or audit row.
func TestPromoteToKeep_Unauthorized_NoNotebookCallNoStoreNoEvent(t *testing.T) {
	cases := []struct {
		name   string
		matrix map[string]string
	}{
		{name: "absent entry", matrix: map[string]string{"other_action": "lead_approval"}},
		{name: "wrong value", matrix: map[string]string{"promote_to_keep": "allowed"}},
		{name: "nil matrix", matrix: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeWriteClient{}
			notebookCli := &fakeNotebookClient{resp: canonicalProposal()}
			keep := &fakeKeepClient{}
			writer := newWriter(t, keep)
			claim := promoteClaim()
			claim.AuthorityMatrix = tc.matrix

			_, err := spawn.PromoteToKeep(context.Background(), client, notebookCli, writer, validPromoteRequest(), claim)
			if !errors.Is(err, spawn.ErrUnauthorized) {
				t.Fatalf("err = %v, want ErrUnauthorized", err)
			}
			if got := notebookCli.recorded(); len(got) != 0 {
				t.Errorf("notebook.PromoteToKeep called %d times on unauthorized path", len(got))
			}
			if got := client.recordedStore(); len(got) != 0 {
				t.Errorf("Store called %d times on unauthorized path", len(got))
			}
			if got := keep.recorded(); len(got) != 0 {
				t.Errorf("keepers_log called %d times on unauthorized path", len(got))
			}
		})
	}
}

// TestPromoteToKeep_EmptyApprovalToken_RejectedNoEvent verifies the
// approval-token gate: empty token surfaces ErrApprovalRequired AFTER
// the unauthorized gate but BEFORE any notebook or keep work.
func TestPromoteToKeep_EmptyApprovalToken_RejectedNoEvent(t *testing.T) {
	client := &fakeWriteClient{}
	notebookCli := &fakeNotebookClient{resp: canonicalProposal()}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	req := validPromoteRequest()
	req.ApprovalToken = ""
	_, err := spawn.PromoteToKeep(context.Background(), client, notebookCli, writer, req, promoteClaim())
	if !errors.Is(err, spawn.ErrApprovalRequired) {
		t.Fatalf("err = %v, want ErrApprovalRequired", err)
	}
	if got := notebookCli.recorded(); len(got) != 0 {
		t.Errorf("notebook.PromoteToKeep called %d times, want 0", len(got))
	}
	if got := client.recordedStore(); len(got) != 0 {
		t.Errorf("Store called %d times, want 0", len(got))
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times, want 0", len(got))
	}
}

// TestPromoteToKeep_EmptyOrganizationID_RejectedSync verifies the
// M3.5.a tenant-scoping gate.
func TestPromoteToKeep_EmptyOrganizationID_RejectedSync(t *testing.T) {
	client := &fakeWriteClient{}
	notebookCli := &fakeNotebookClient{resp: canonicalProposal()}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)
	claim := promoteClaim()
	claim.OrganizationID = ""

	_, err := spawn.PromoteToKeep(context.Background(), client, notebookCli, writer, validPromoteRequest(), claim)
	if !errors.Is(err, spawn.ErrInvalidClaim) {
		t.Fatalf("err = %v, want ErrInvalidClaim", err)
	}
	if got := notebookCli.recorded(); len(got) != 0 {
		t.Errorf("notebook.PromoteToKeep called %d times, want 0", len(got))
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times, want 0", len(got))
	}
}

// TestPromoteToKeep_EmptyNotebookEntryID_RejectedPreNetwork covers the
// ErrInvalidRequest gate: empty NotebookEntryID fails synchronously
// before any notebook read or keep write.
func TestPromoteToKeep_EmptyNotebookEntryID_RejectedPreNetwork(t *testing.T) {
	client := &fakeWriteClient{}
	notebookCli := &fakeNotebookClient{resp: canonicalProposal()}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	req := validPromoteRequest()
	req.NotebookEntryID = ""
	_, err := spawn.PromoteToKeep(context.Background(), client, notebookCli, writer, req, promoteClaim())
	if !errors.Is(err, spawn.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if got := notebookCli.recorded(); len(got) != 0 {
		t.Errorf("notebook.PromoteToKeep called %d times, want 0", len(got))
	}
	if got := client.recordedStore(); len(got) != 0 {
		t.Errorf("Store called %d times, want 0", len(got))
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times, want 0", len(got))
	}
}

// TestPromoteToKeep_NilNotebookClient_ReturnsInvalidRequest pins the
// dependency-discipline contract.
func TestPromoteToKeep_NilNotebookClient_ReturnsInvalidRequest(t *testing.T) {
	client := &fakeWriteClient{}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	_, err := spawn.PromoteToKeep(context.Background(), client, nil, writer, validPromoteRequest(), promoteClaim())
	if !errors.Is(err, spawn.ErrInvalidRequest) {
		t.Errorf("nil-notebook err = %v, want ErrInvalidRequest", err)
	}
}

// TestPromoteToKeep_NilWriteClient_ReturnsInvalidRequest pins the
// dependency-discipline contract for the keep-write seam.
func TestPromoteToKeep_NilWriteClient_ReturnsInvalidRequest(t *testing.T) {
	notebookCli := &fakeNotebookClient{resp: canonicalProposal()}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	_, err := spawn.PromoteToKeep(context.Background(), nil, notebookCli, writer, validPromoteRequest(), promoteClaim())
	if !errors.Is(err, spawn.ErrInvalidRequest) {
		t.Errorf("nil-write-client err = %v, want ErrInvalidRequest", err)
	}
}

// TestPromoteToKeep_NilLogger_ReturnsInvalidRequest pins the
// dependency-discipline contract for the writer seam.
func TestPromoteToKeep_NilLogger_ReturnsInvalidRequest(t *testing.T) {
	client := &fakeWriteClient{}
	notebookCli := &fakeNotebookClient{resp: canonicalProposal()}

	_, err := spawn.PromoteToKeep(context.Background(), client, notebookCli, nil, validPromoteRequest(), promoteClaim())
	if !errors.Is(err, spawn.ErrInvalidRequest) {
		t.Errorf("nil-logger err = %v, want ErrInvalidRequest", err)
	}
}

// ── PromoteToKeep — pre-emit short-circuit (test plan #6) ───────────────────

// TestPromoteToKeep_NotebookErrNotFound_ZeroEventsFromThisTool verifies
// the AC1 / AC7 pre-emit short-circuit: a notebook read that returns
// [notebook.ErrNotFound] surfaces a wrapped error and emits ZERO audit
// rows from this tool. The notebook.PromoteToKeep itself only emits its
// own `notebook_promotion_proposed` on success — read failure means no
// audit row from the notebook side either, so the recorded count stays 0.
func TestPromoteToKeep_NotebookErrNotFound_ZeroEventsFromThisTool(t *testing.T) {
	client := &fakeWriteClient{}
	notebookCli := &fakeNotebookClient{err: notebook.ErrNotFound}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	_, err := spawn.PromoteToKeep(context.Background(), client, notebookCli, writer, validPromoteRequest(), promoteClaim())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, notebook.ErrNotFound) {
		t.Errorf("error chain does not contain notebook.ErrNotFound: %v", err)
	}
	// Notebook read was attempted exactly once.
	if got := notebookCli.recorded(); len(got) != 1 {
		t.Errorf("notebook.PromoteToKeep calls = %d, want 1", len(got))
	}
	// Keep Store NEVER called.
	if got := client.recordedStore(); len(got) != 0 {
		t.Errorf("Store called %d times on notebook-error path, want 0", len(got))
	}
	// ZERO audit rows from THIS tool — pre-emit short-circuit.
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times on notebook-error path, want 0", len(got))
	}
}

// ── PromoteToKeep — failure path (test plan #7, #8) ─────────────────────────

// TestPromoteToKeep_StoreError_EmitsRequestedAndFailed verifies AC7's
// failure-path audit chain: a keep Store error produces EXACTLY 2
// events (requested + failed), in that order, and the error is
// surfaced wrapped to the caller. Pins exact event count + order — no
// `notebook_promoted_to_keep` domain event on failure (test plan #7).
func TestPromoteToKeep_StoreError_EmitsRequestedAndFailed(t *testing.T) {
	sentinel := errors.New("keepclient: 500")
	client := &fakeWriteClient{storeErr: sentinel}
	notebookCli := &fakeNotebookClient{resp: canonicalProposal()}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	_, err := spawn.PromoteToKeep(context.Background(), client, notebookCli, writer, validPromoteRequest(), promoteClaim())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain does not contain sentinel: %v", err)
	}

	// Pin exact event count + order — AC7 audit-order regression on
	// failure: ONLY 2 events (requested + failed), NO domain event.
	logCalls := assertAuditChainOrder(t, keep, []string{
		"watchmaster_promote_to_keep_requested",
		"watchmaster_promote_to_keep_failed",
	})
	if got := payloadField(t, logCalls[1], "error_class"); got == "" {
		t.Errorf("failed.error_class is empty; want a Go type name")
	}
	if got := payloadField(t, logCalls[1], "event_class"); got != "failed" {
		t.Errorf("failed.event_class = %q, want failed", got)
	}
	if got := payloadField(t, logCalls[1], "notebook_entry_id"); got != testNotebookEntryID {
		t.Errorf("failed.notebook_entry_id = %q, want %q", got, testNotebookEntryID)
	}

	// PII guard on failure path too.
	for i, call := range logCalls {
		if hasPayloadKey(t, call, "content") {
			t.Errorf("event[%d] leaked `content` on failure path", i)
		}
		if hasPayloadKey(t, call, "embedding") {
			t.Errorf("event[%d] leaked `embedding` on failure path", i)
		}
	}
}

// TestPromoteToKeep_RespectsContextCancellation pins ctx-cancel
// precedence (matches the M6.1.b / M6.2.b/c convention).
func TestPromoteToKeep_RespectsContextCancellation(t *testing.T) {
	client := &fakeWriteClient{}
	notebookCli := &fakeNotebookClient{resp: canonicalProposal()}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := spawn.PromoteToKeep(ctx, client, notebookCli, writer, validPromoteRequest(), promoteClaim())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := notebookCli.recorded(); len(got) != 0 {
		t.Errorf("notebook.PromoteToKeep called on cancelled ctx, want 0 calls")
	}
	if got := client.recordedStore(); len(got) != 0 {
		t.Errorf("Store called on cancelled ctx, want 0 calls")
	}
}
