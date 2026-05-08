package spawn_test

import (
	"context"
	"errors"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ── helpers ──────────────────────────────────────────────────────────────────

const testRetireTk = "approval-token-retire-abc"

// retireClaim returns a claim authorising `retire_watchkeeper`.
func retireClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: testOrgID,
		AgentID:        testAgentID,
		AuthorityMatrix: map[string]string{
			"retire_watchkeeper": "lead_approval",
		},
	}
}

// validRetireRequest returns a request carrying every required field.
func validRetireRequest() spawn.RetireWatchkeeperRequest {
	return spawn.RetireWatchkeeperRequest{
		AgentID:       testTargetAgent,
		ApprovalToken: testRetireTk,
	}
}

// ── happy path ───────────────────────────────────────────────────────────────

// TestRetireWatchkeeper_HappyPath_EmitsTwoEventsAndUpdatesStatus drives
// the retire_watchkeeper happy path and pins:
//   - AC1 return shape (no value);
//   - AC2 audit chain (requested → succeeded);
//   - AC7 chain order;
//   - the keepclient.UpdateWatchkeeperStatus is called with
//     (target agent_id, "retired").
func TestRetireWatchkeeper_HappyPath_EmitsTwoEventsAndUpdatesStatus(t *testing.T) {
	client := &fakeWriteClient{}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	err := spawn.RetireWatchkeeper(
		context.Background(),
		client,
		writer,
		validRetireRequest(),
		retireClaim(),
	)
	if err != nil {
		t.Fatalf("RetireWatchkeeper: %v", err)
	}

	upd := client.recordedUpdate()
	if len(upd) != 1 {
		t.Fatalf("UpdateWatchkeeperStatus calls = %d, want 1", len(upd))
	}
	if upd[0].id != testTargetAgent {
		t.Errorf("id = %q, want %q", upd[0].id, testTargetAgent)
	}
	if upd[0].status != "retired" {
		t.Errorf("status = %q, want retired", upd[0].status)
	}
	// No GetManifest pre-read on the retire path (mirrors ProposeSpawn,
	// not adjust_personality / adjust_language).
	if got := client.recordedGet(); len(got) != 0 {
		t.Errorf("GetManifest called %d times on retire path, want 0", len(got))
	}
	// No PutManifestVersion either — retire is a status-row mutation.
	if got := client.recordedPut(); len(got) != 0 {
		t.Errorf("PutManifestVersion called %d times on retire path, want 0", len(got))
	}

	logCalls := assertAuditChainOrder(t, keep, []string{
		"watchmaster_retire_watchkeeper_requested",
		"watchmaster_retire_watchkeeper_succeeded",
	})
	if got := payloadField(t, logCalls[0], "tool_name"); got != "retire_watchkeeper" {
		t.Errorf("requested.tool_name = %q, want retire_watchkeeper", got)
	}
	if got := payloadField(t, logCalls[0], "agent_id"); got != testTargetAgent {
		t.Errorf("requested.agent_id = %q, want %q", got, testTargetAgent)
	}
	if got := payloadFieldAny(t, logCalls[0], "approval_token_present"); got != true {
		t.Errorf("requested.approval_token_present = %v, want true", got)
	}
	if got := payloadField(t, logCalls[1], "event_class"); got != "succeeded" {
		t.Errorf("succeeded.event_class = %q, want succeeded", got)
	}
}

// ── negative paths ───────────────────────────────────────────────────────────

// TestRetireWatchkeeper_Unauthorized_NoUpdateNoEvent verifies AC8: a
// claim missing `retire_watchkeeper=lead_approval` is rejected BEFORE
// any keep write or audit row.
func TestRetireWatchkeeper_Unauthorized_NoUpdateNoEvent(t *testing.T) {
	cases := []struct {
		name   string
		matrix map[string]string
	}{
		{name: "absent entry", matrix: map[string]string{"other_action": "lead_approval"}},
		{name: "wrong value", matrix: map[string]string{"retire_watchkeeper": "allowed"}},
		{name: "nil matrix", matrix: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeWriteClient{}
			keep := &fakeKeepClient{}
			writer := newWriter(t, keep)
			claim := retireClaim()
			claim.AuthorityMatrix = tc.matrix

			err := spawn.RetireWatchkeeper(context.Background(), client, writer, validRetireRequest(), claim)
			if !errors.Is(err, spawn.ErrUnauthorized) {
				t.Fatalf("err = %v, want ErrUnauthorized", err)
			}
			if got := client.recordedUpdate(); len(got) != 0 {
				t.Errorf("UpdateWatchkeeperStatus called %d times on unauthorized path", len(got))
			}
			if got := keep.recorded(); len(got) != 0 {
				t.Errorf("keepers_log called %d times on unauthorized path", len(got))
			}
		})
	}
}

// TestRetireWatchkeeper_EmptyApprovalToken_RejectedNoUpdateNoEvent
// verifies AC8's approval-token gate: empty token surfaces
// ErrApprovalRequired AFTER the unauthorized gate but BEFORE any keep
// work.
func TestRetireWatchkeeper_EmptyApprovalToken_RejectedNoUpdateNoEvent(t *testing.T) {
	client := &fakeWriteClient{}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	req := validRetireRequest()
	req.ApprovalToken = ""
	err := spawn.RetireWatchkeeper(context.Background(), client, writer, req, retireClaim())
	if !errors.Is(err, spawn.ErrApprovalRequired) {
		t.Fatalf("err = %v, want ErrApprovalRequired", err)
	}
	if got := client.recordedUpdate(); len(got) != 0 {
		t.Errorf("UpdateWatchkeeperStatus called %d times, want 0", len(got))
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times, want 0", len(got))
	}
}

// TestRetireWatchkeeper_EmptyOrganizationID_RejectedSync verifies the
// M3.5.a tenant-scoping gate.
func TestRetireWatchkeeper_EmptyOrganizationID_RejectedSync(t *testing.T) {
	client := &fakeWriteClient{}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)
	claim := retireClaim()
	claim.OrganizationID = ""

	err := spawn.RetireWatchkeeper(context.Background(), client, writer, validRetireRequest(), claim)
	if !errors.Is(err, spawn.ErrInvalidClaim) {
		t.Fatalf("err = %v, want ErrInvalidClaim", err)
	}
	if got := client.recordedUpdate(); len(got) != 0 {
		t.Errorf("UpdateWatchkeeperStatus called %d times, want 0", len(got))
	}
	if got := keep.recorded(); len(got) != 0 {
		t.Errorf("keepers_log called %d times, want 0", len(got))
	}
}

// TestRetireWatchkeeper_EmptyAgentID_RejectedSync covers the
// ErrInvalidRequest gate: empty AgentID fails synchronously before any
// privileged work.
func TestRetireWatchkeeper_EmptyAgentID_RejectedSync(t *testing.T) {
	client := &fakeWriteClient{}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)
	req := validRetireRequest()
	req.AgentID = ""

	err := spawn.RetireWatchkeeper(context.Background(), client, writer, req, retireClaim())
	if !errors.Is(err, spawn.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if got := client.recordedUpdate(); len(got) != 0 {
		t.Errorf("UpdateWatchkeeperStatus called %d times, want 0", len(got))
	}
}

// TestRetireWatchkeeper_NilClient_ReturnsInvalidRequest pins the
// dependency-discipline contract: a nil client surfaces as
// ErrInvalidRequest BEFORE any privileged work.
func TestRetireWatchkeeper_NilClient_ReturnsInvalidRequest(t *testing.T) {
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	err := spawn.RetireWatchkeeper(context.Background(), nil, writer, validRetireRequest(), retireClaim())
	if !errors.Is(err, spawn.ErrInvalidRequest) {
		t.Errorf("nil-client err = %v, want ErrInvalidRequest", err)
	}
}

// TestRetireWatchkeeper_NilLogger_ReturnsInvalidRequest pins the
// dependency-discipline contract for the writer seam.
func TestRetireWatchkeeper_NilLogger_ReturnsInvalidRequest(t *testing.T) {
	client := &fakeWriteClient{}
	err := spawn.RetireWatchkeeper(context.Background(), client, nil, validRetireRequest(), retireClaim())
	if !errors.Is(err, spawn.ErrInvalidRequest) {
		t.Errorf("nil-logger err = %v, want ErrInvalidRequest", err)
	}
}

// ── failure path (audit-chain regression pin per AC7) ────────────────────────

// TestRetireWatchkeeper_UpdateError_EmitsRequestedAndFailed verifies
// AC7's failure-path audit chain: an UpdateWatchkeeperStatus error
// produces EXACTLY 2 events (requested + failed), in that order, and
// the error is surfaced wrapped to the caller. Pins exact event count
// + order — failure is the regression-prone path.
func TestRetireWatchkeeper_UpdateError_EmitsRequestedAndFailed(t *testing.T) {
	sentinel := errors.New("keepclient: invalid status transition")
	client := &fakeWriteClient{updateErr: sentinel}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)

	err := spawn.RetireWatchkeeper(context.Background(), client, writer, validRetireRequest(), retireClaim())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain does not contain sentinel: %v", err)
	}

	// Pin the exact event count + order — AC7 audit-order regression.
	logCalls := assertAuditChainOrder(t, keep, []string{
		"watchmaster_retire_watchkeeper_requested",
		"watchmaster_retire_watchkeeper_failed",
	})
	if got := payloadField(t, logCalls[1], "error_class"); got == "" {
		t.Errorf("failed.error_class is empty; want a Go type name")
	}
	if got := payloadField(t, logCalls[1], "event_class"); got != "failed" {
		t.Errorf("failed.event_class = %q, want failed", got)
	}
}

// TestRetireWatchkeeper_RespectsContextCancellation pins ctx-cancel
// precedence (matches the M6.1.b / M6.2.b convention).
func TestRetireWatchkeeper_RespectsContextCancellation(t *testing.T) {
	client := &fakeWriteClient{}
	keep := &fakeKeepClient{}
	writer := newWriter(t, keep)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := spawn.RetireWatchkeeper(ctx, client, writer, validRetireRequest(), retireClaim())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := client.recordedUpdate(); len(got) != 0 {
		t.Errorf("UpdateWatchkeeperStatus called on cancelled ctx, want 0 calls")
	}
}
