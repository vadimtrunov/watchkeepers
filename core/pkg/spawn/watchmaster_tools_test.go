package spawn_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ── shared real-fakes for the read tools ────────────────────────────────────

// fakeReadClient is the recording stand-in the M6.2.a read-only tools
// drive. Records every call by value so subsequent assertions cannot
// race a still-mutating caller. Mirrors the M6.1.b fakeSlackAdapter /
// fakeKeepClient shape — hand-rolled, no mocking lib.
type fakeReadClient struct {
	listResp *keepclient.ListWatchkeepersResponse
	listErr  error
	tailResp *keepclient.LogTailResponse
	tailErr  error

	mu        sync.Mutex
	listCalls []keepclient.ListWatchkeepersRequest
	tailCalls []keepclient.LogTailOptions
}

func (f *fakeReadClient) ListWatchkeepers(_ context.Context, req keepclient.ListWatchkeepersRequest) (*keepclient.ListWatchkeepersResponse, error) {
	f.mu.Lock()
	f.listCalls = append(f.listCalls, req)
	f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.listResp == nil {
		return &keepclient.ListWatchkeepersResponse{Items: nil}, nil
	}
	return f.listResp, nil
}

func (f *fakeReadClient) LogTail(_ context.Context, opts keepclient.LogTailOptions) (*keepclient.LogTailResponse, error) {
	f.mu.Lock()
	f.tailCalls = append(f.tailCalls, opts)
	f.mu.Unlock()
	if f.tailErr != nil {
		return nil, f.tailErr
	}
	if f.tailResp == nil {
		return &keepclient.LogTailResponse{Events: nil}, nil
	}
	return f.tailResp, nil
}

func (f *fakeReadClient) recordedList() []keepclient.ListWatchkeepersRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keepclient.ListWatchkeepersRequest, len(f.listCalls))
	copy(out, f.listCalls)
	return out
}

func (f *fakeReadClient) recordedTail() []keepclient.LogTailOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keepclient.LogTailOptions, len(f.tailCalls))
	copy(out, f.tailCalls)
	return out
}

// readOnlyClaim returns a claim valid for the M6.2.a read-only tools.
// The AuthorityMatrix is intentionally empty — read-only tools do NOT
// consult the matrix; the claim only needs OrganizationID set.
func readOnlyClaim() spawn.Claim {
	return spawn.Claim{
		OrganizationID: testOrgID,
		AgentID:        testAgentID,
	}
}

// ── ListWatchkeepers ────────────────────────────────────────────────────────

// TestListWatchkeepers_HappyPath_ProjectsRows pins the AC1 happy
// path: the tool forwards the request to the read client and projects
// every row into the wire shape, including pointer-time → empty-string
// handling for nullable timestamps.
func TestListWatchkeepers_HappyPath_ProjectsRows(t *testing.T) {
	spawnedAt := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	createdAt := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)
	versionID := "11111111-2222-3333-4444-555555555555"
	client := &fakeReadClient{
		listResp: &keepclient.ListWatchkeepersResponse{
			Items: []keepclient.Watchkeeper{
				{
					ID:                      "wk-1",
					ManifestID:              "mf-1",
					LeadHumanID:             "human-1",
					ActiveManifestVersionID: &versionID,
					Status:                  "active",
					SpawnedAt:               &spawnedAt,
					CreatedAt:               createdAt,
				},
				{
					ID:          "wk-2",
					ManifestID:  "mf-2",
					LeadHumanID: "human-2",
					Status:      "pending",
					CreatedAt:   createdAt,
				},
			},
		},
	}

	res, err := spawn.ListWatchkeepers(
		context.Background(),
		client,
		spawn.ListWatchkeepersRequest{Status: "active", Limit: 25},
		readOnlyClaim(),
	)
	if err != nil {
		t.Fatalf("ListWatchkeepers: %v", err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("Items = %d, want 2", len(res.Items))
	}
	if res.Items[0].ID != "wk-1" || res.Items[0].Status != "active" {
		t.Errorf("Items[0] = %+v", res.Items[0])
	}
	if res.Items[0].ActiveManifestVersionID != versionID {
		t.Errorf("Items[0].ActiveManifestVersionID = %q, want %q", res.Items[0].ActiveManifestVersionID, versionID)
	}
	if res.Items[0].SpawnedAt == "" {
		t.Errorf("Items[0].SpawnedAt empty, want non-empty RFC3339")
	}
	if res.Items[1].SpawnedAt != "" {
		t.Errorf("Items[1].SpawnedAt = %q, want empty (pending row)", res.Items[1].SpawnedAt)
	}
	if res.Items[1].ActiveManifestVersionID != "" {
		t.Errorf("Items[1].ActiveManifestVersionID = %q, want empty (NULL column)", res.Items[1].ActiveManifestVersionID)
	}

	calls := client.recordedList()
	if len(calls) != 1 {
		t.Fatalf("ListWatchkeepers calls = %d, want 1", len(calls))
	}
	if calls[0].Status != "active" || calls[0].Limit != 25 {
		t.Errorf("forwarded request = %+v, want {Status:active, Limit:25}", calls[0])
	}
}

// TestListWatchkeepers_EmptyOrganizationID_RejectedSync verifies the
// M3.5.a tenant-scoping discipline: empty OrganizationID returns
// [ErrInvalidClaim] WITHOUT touching the read client.
func TestListWatchkeepers_EmptyOrganizationID_RejectedSync(t *testing.T) {
	client := &fakeReadClient{}
	claim := readOnlyClaim()
	claim.OrganizationID = ""
	_, err := spawn.ListWatchkeepers(
		context.Background(),
		client,
		spawn.ListWatchkeepersRequest{},
		claim,
	)
	if !errors.Is(err, spawn.ErrInvalidClaim) {
		t.Fatalf("err = %v, want spawn.ErrInvalidClaim", err)
	}
	if got := client.recordedList(); len(got) != 0 {
		t.Errorf("client called %d times on invalid claim, want 0", len(got))
	}
}

// TestListWatchkeepers_RespectsContextCancellation pins ctx-cancel
// precedence per the slack package's M4.2.b/c.1 convention.
func TestListWatchkeepers_RespectsContextCancellation(t *testing.T) {
	client := &fakeReadClient{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := spawn.ListWatchkeepers(
		ctx,
		client,
		spawn.ListWatchkeepersRequest{},
		readOnlyClaim(),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if got := client.recordedList(); len(got) != 0 {
		t.Errorf("client called %d times on cancelled ctx, want 0", len(got))
	}
}

// TestListWatchkeepers_ReadClientError_Wrapped pins error-wrap
// discipline: the underlying keepclient error surfaces wrapped with
// `spawn:` so callers can errors.Is the original.
func TestListWatchkeepers_ReadClientError_Wrapped(t *testing.T) {
	sentinel := errors.New("keepclient: db down")
	client := &fakeReadClient{listErr: sentinel}
	_, err := spawn.ListWatchkeepers(
		context.Background(),
		client,
		spawn.ListWatchkeepersRequest{},
		readOnlyClaim(),
	)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("error chain does not contain sentinel: %v", err)
	}
}

// ── ReportCost ───────────────────────────────────────────────────────────────

// TestReportCost_HappyPath_AggregatesEvents drives ReportCost over a
// 3-event keepers_log fixture written via a real keeperslog.Writer
// over a recording fakeKeepClient (per AC6: real fakes, no mocks of
// units under test). Pins the AC2 sum semantics: prompt + completion
// totals match the fixture sums and EventCount/ScannedRows reflect
// the filter result.
func TestReportCost_HappyPath_AggregatesEvents(t *testing.T) {
	// Build three rows: one matching cost event, one non-matching
	// event_type, one matching but for a different agent. The
	// per-agent filter restricts to the first row only.
	const targetAgent = "agent-watchmaster"
	const otherAgent = "agent-other"

	events := []keepclient.LogEvent{
		{
			ID:        "row-1",
			EventType: "llm_turn_cost_streaming",
			Payload:   marshalEnvelope(t, map[string]any{"agent_id": targetAgent, "prompt_tokens": 100, "completion_tokens": 50}),
		},
		{
			ID:        "row-2",
			EventType: "lesson_learned",
			Payload:   marshalEnvelope(t, map[string]any{"agent_id": targetAgent, "prompt_tokens": 999}),
		},
		{
			ID:        "row-3",
			EventType: "llm_turn_cost_completion",
			Payload:   marshalEnvelope(t, map[string]any{"agent_id": otherAgent, "prompt_tokens": 200, "completion_tokens": 100}),
		},
	}
	client := &fakeReadClient{tailResp: &keepclient.LogTailResponse{Events: events}}

	res, err := spawn.ReportCost(
		context.Background(),
		client,
		spawn.ReportCostRequest{AgentID: targetAgent, Limit: 10},
		readOnlyClaim(),
	)
	if err != nil {
		t.Fatalf("ReportCost: %v", err)
	}
	if res.PromptTokens != 100 {
		t.Errorf("PromptTokens = %d, want 100 (only row-1 matches agent+prefix)", res.PromptTokens)
	}
	if res.CompletionTokens != 50 {
		t.Errorf("CompletionTokens = %d, want 50", res.CompletionTokens)
	}
	if res.EventCount != 1 {
		t.Errorf("EventCount = %d, want 1", res.EventCount)
	}
	if res.ScannedRows != 3 {
		t.Errorf("ScannedRows = %d, want 3 (all rows from the tail were scanned)", res.ScannedRows)
	}
	if res.AgentID != targetAgent {
		t.Errorf("AgentID echo = %q, want %q", res.AgentID, targetAgent)
	}
	if res.EventTypePrefix != "llm_turn_cost" {
		t.Errorf("EventTypePrefix = %q, want %q", res.EventTypePrefix, "llm_turn_cost")
	}

	// Forwarded LogTail call carried the requested limit.
	tailCalls := client.recordedTail()
	if len(tailCalls) != 1 || tailCalls[0].Limit != 10 {
		t.Errorf("tail calls = %+v, want one call with Limit=10", tailCalls)
	}
}

// TestReportCost_OrgWide_AggregatesAllAgents pins the org-wide
// branch: an empty AgentID sums every matching row regardless of
// payload.agent_id.
func TestReportCost_OrgWide_AggregatesAllAgents(t *testing.T) {
	events := []keepclient.LogEvent{
		{
			ID:        "row-1",
			EventType: "llm_turn_cost",
			Payload:   marshalEnvelope(t, map[string]any{"agent_id": "agent-a", "prompt_tokens": 100, "completion_tokens": 50}),
		},
		{
			ID:        "row-2",
			EventType: "llm_turn_cost",
			Payload:   marshalEnvelope(t, map[string]any{"agent_id": "agent-b", "prompt_tokens": 200, "completion_tokens": 75}),
		},
	}
	client := &fakeReadClient{tailResp: &keepclient.LogTailResponse{Events: events}}
	res, err := spawn.ReportCost(
		context.Background(),
		client,
		spawn.ReportCostRequest{}, // empty AgentID → org-wide
		readOnlyClaim(),
	)
	if err != nil {
		t.Fatalf("ReportCost: %v", err)
	}
	if res.PromptTokens != 300 || res.CompletionTokens != 125 {
		t.Errorf("totals = (%d, %d), want (300, 125)", res.PromptTokens, res.CompletionTokens)
	}
	if res.EventCount != 2 {
		t.Errorf("EventCount = %d, want 2", res.EventCount)
	}
}

// TestReportCost_NoMatchingEvents_ReturnsZero pins the Phase 1
// behaviour: when no cost events exist (the runtime hasn't yet wired
// emission), the tool returns zero totals without error.
func TestReportCost_NoMatchingEvents_ReturnsZero(t *testing.T) {
	events := []keepclient.LogEvent{
		{
			ID:        "row-1",
			EventType: "lesson_learned",
			Payload:   marshalEnvelope(t, map[string]any{"prompt_tokens": 999}),
		},
	}
	client := &fakeReadClient{tailResp: &keepclient.LogTailResponse{Events: events}}
	res, err := spawn.ReportCost(
		context.Background(),
		client,
		spawn.ReportCostRequest{},
		readOnlyClaim(),
	)
	if err != nil {
		t.Fatalf("ReportCost: %v", err)
	}
	if res.PromptTokens != 0 || res.CompletionTokens != 0 || res.EventCount != 0 {
		t.Errorf("totals/count = (%d, %d, %d), want all zero", res.PromptTokens, res.CompletionTokens, res.EventCount)
	}
	if res.ScannedRows != 1 {
		t.Errorf("ScannedRows = %d, want 1", res.ScannedRows)
	}
}

// TestReportCost_RealKeepersLogWriter_E2E pins the AC6 real-fakes
// discipline: drive a real *keeperslog.Writer (not a stub) over the
// recording fakeKeepClient, append three cost events, then call
// ReportCost via a fakeReadClient pre-loaded with the recorded rows.
// The token totals match the writer's input.
func TestReportCost_RealKeepersLogWriter_E2E(t *testing.T) {
	// Use the existing fakeKeepClient (defined in slack_app_test.go)
	// and a real *keeperslog.Writer to produce the rows.
	keep := &fakeKeepClient{}
	writer := keeperslog.New(keep)
	for i := 0; i < 3; i++ {
		_, err := writer.Append(context.Background(), keeperslog.Event{
			EventType: "llm_turn_cost",
			Payload: map[string]any{
				"agent_id":          testAgentID,
				"prompt_tokens":     100,
				"completion_tokens": 50,
			},
		})
		if err != nil {
			t.Fatalf("writer.Append: %v", err)
		}
	}

	// Lift the recorded LogAppendRequest payloads back into LogEvents
	// the LogTail-fake can return.
	logged := keep.recorded()
	events := make([]keepclient.LogEvent, len(logged))
	for i, l := range logged {
		events[i] = keepclient.LogEvent{
			ID:        l.CorrelationID, // any non-empty value works for the test
			EventType: l.EventType,
			Payload:   l.Payload,
		}
	}

	read := &fakeReadClient{tailResp: &keepclient.LogTailResponse{Events: events}}
	res, err := spawn.ReportCost(
		context.Background(),
		read,
		spawn.ReportCostRequest{AgentID: testAgentID},
		readOnlyClaim(),
	)
	if err != nil {
		t.Fatalf("ReportCost: %v", err)
	}
	if res.PromptTokens != 300 || res.CompletionTokens != 150 {
		t.Errorf("totals = (%d, %d), want (300, 150)", res.PromptTokens, res.CompletionTokens)
	}
	if res.EventCount != 3 {
		t.Errorf("EventCount = %d, want 3", res.EventCount)
	}
}

// TestReportCost_EmptyOrganizationID_RejectedSync mirrors the M3.5.a
// tenant-scoping check.
func TestReportCost_EmptyOrganizationID_RejectedSync(t *testing.T) {
	client := &fakeReadClient{}
	claim := readOnlyClaim()
	claim.OrganizationID = ""
	_, err := spawn.ReportCost(
		context.Background(),
		client,
		spawn.ReportCostRequest{},
		claim,
	)
	if !errors.Is(err, spawn.ErrInvalidClaim) {
		t.Fatalf("err = %v, want spawn.ErrInvalidClaim", err)
	}
}

// TestReportCost_NegativeLimit_Rejected pins the input-validation
// gate: a negative Limit returns ErrInvalidRequest synchronously
// without touching the read client.
func TestReportCost_NegativeLimit_Rejected(t *testing.T) {
	client := &fakeReadClient{}
	_, err := spawn.ReportCost(
		context.Background(),
		client,
		spawn.ReportCostRequest{Limit: -1},
		readOnlyClaim(),
	)
	if !errors.Is(err, spawn.ErrInvalidRequest) {
		t.Fatalf("err = %v, want spawn.ErrInvalidRequest", err)
	}
	if got := client.recordedTail(); len(got) != 0 {
		t.Errorf("client called %d times on negative-limit, want 0", len(got))
	}
}

// TestReportCost_RespectsContextCancellation pins ctx-cancel
// precedence.
func TestReportCost_RespectsContextCancellation(t *testing.T) {
	client := &fakeReadClient{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := spawn.ReportCost(
		ctx,
		client,
		spawn.ReportCostRequest{},
		readOnlyClaim(),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// ── ReportHealth ─────────────────────────────────────────────────────────────

// TestReportHealth_OrgWide_CountsByStatus pins the org-wide
// aggregation: counts populate, Item stays nil, CountTotal = sum.
func TestReportHealth_OrgWide_CountsByStatus(t *testing.T) {
	createdAt := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)
	client := &fakeReadClient{
		listResp: &keepclient.ListWatchkeepersResponse{
			Items: []keepclient.Watchkeeper{
				{ID: "wk-1", Status: "active", CreatedAt: createdAt},
				{ID: "wk-2", Status: "active", CreatedAt: createdAt},
				{ID: "wk-3", Status: "pending", CreatedAt: createdAt},
				{ID: "wk-4", Status: "retired", CreatedAt: createdAt},
			},
		},
	}
	res, err := spawn.ReportHealth(
		context.Background(),
		client,
		spawn.ReportHealthRequest{},
		readOnlyClaim(),
	)
	if err != nil {
		t.Fatalf("ReportHealth: %v", err)
	}
	if res.Item != nil {
		t.Errorf("Item = %+v, want nil for org-wide query", res.Item)
	}
	if res.CountActive != 2 || res.CountPending != 1 || res.CountRetired != 1 {
		t.Errorf("counts = (active=%d, pending=%d, retired=%d), want (2, 1, 1)",
			res.CountActive, res.CountPending, res.CountRetired)
	}
	if res.CountTotal != 4 {
		t.Errorf("CountTotal = %d, want 4", res.CountTotal)
	}
}

// TestReportHealth_ByAgentID_ReturnsSnapshot pins the narrow-by-id
// branch: the tool returns a single-row snapshot, counts stay zero.
func TestReportHealth_ByAgentID_ReturnsSnapshot(t *testing.T) {
	spawnedAt := time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC)
	createdAt := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)
	client := &fakeReadClient{
		listResp: &keepclient.ListWatchkeepersResponse{
			Items: []keepclient.Watchkeeper{
				{ID: "wk-1", Status: "active", SpawnedAt: &spawnedAt, CreatedAt: createdAt},
				{ID: "wk-2", Status: "pending", CreatedAt: createdAt},
			},
		},
	}
	res, err := spawn.ReportHealth(
		context.Background(),
		client,
		spawn.ReportHealthRequest{AgentID: "wk-1"},
		readOnlyClaim(),
	)
	if err != nil {
		t.Fatalf("ReportHealth: %v", err)
	}
	if res.Item == nil {
		t.Fatalf("Item is nil, want a snapshot for wk-1")
	}
	if res.Item.ID != "wk-1" || res.Item.Status != "active" {
		t.Errorf("Item = %+v, want {ID:wk-1, Status:active}", res.Item)
	}
	if res.Item.SpawnedAt == "" {
		t.Errorf("Item.SpawnedAt is empty, want non-empty RFC3339")
	}
	if res.CountActive != 0 || res.CountPending != 0 || res.CountRetired != 0 {
		t.Errorf("counts non-zero on by-agent path: %+v", res)
	}
}

// TestReportHealth_ByAgentID_NoMatch_ReturnsEmpty pins the "agent
// hasn't been spawned yet" case: no error, Item nil, counts zero.
func TestReportHealth_ByAgentID_NoMatch_ReturnsEmpty(t *testing.T) {
	createdAt := time.Date(2026, 5, 7, 9, 0, 0, 0, time.UTC)
	client := &fakeReadClient{
		listResp: &keepclient.ListWatchkeepersResponse{
			Items: []keepclient.Watchkeeper{
				{ID: "wk-1", Status: "active", CreatedAt: createdAt},
			},
		},
	}
	res, err := spawn.ReportHealth(
		context.Background(),
		client,
		spawn.ReportHealthRequest{AgentID: "wk-missing"},
		readOnlyClaim(),
	)
	if err != nil {
		t.Fatalf("ReportHealth: %v", err)
	}
	if res.Item != nil || res.CountTotal != 0 {
		t.Errorf("res = %+v, want empty zero-value", res)
	}
}

// TestReportHealth_EmptyOrganizationID_RejectedSync mirrors the
// M3.5.a tenant-scoping check.
func TestReportHealth_EmptyOrganizationID_RejectedSync(t *testing.T) {
	client := &fakeReadClient{}
	claim := readOnlyClaim()
	claim.OrganizationID = ""
	_, err := spawn.ReportHealth(
		context.Background(),
		client,
		spawn.ReportHealthRequest{},
		claim,
	)
	if !errors.Is(err, spawn.ErrInvalidClaim) {
		t.Fatalf("err = %v, want spawn.ErrInvalidClaim", err)
	}
}

// TestReportHealth_RespectsContextCancellation pins ctx-cancel
// precedence.
func TestReportHealth_RespectsContextCancellation(t *testing.T) {
	client := &fakeReadClient{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := spawn.ReportHealth(
		ctx,
		client,
		spawn.ReportHealthRequest{},
		readOnlyClaim(),
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// ── helpers ──────────────────────────────────────────────────────────────────

// marshalEnvelope encodes `data` into the same envelope keeperslog.Writer
// produces ({"data": <payload>}). Tests use this to construct LogTail
// fixtures that round-trip through the ReportCost tool's payload
// decoder verbatim.
func marshalEnvelope(t *testing.T, data map[string]any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		t.Fatalf("marshalEnvelope: %v", err)
	}
	return encoded
}
