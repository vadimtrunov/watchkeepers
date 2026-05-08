package approval_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/cards"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/inbound"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/inbound/approval"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ── real fakes (M5.6 discipline — no mocks of unit under test) ─────────

// fakeReplayer records every Replay call; substitutes a canned error
// when `errToReturn` is non-nil.
type fakeReplayer struct {
	mu          sync.Mutex
	calls       []fakeReplayerCall
	errToReturn error
}

type fakeReplayerCall struct {
	Tool          string
	ParamsJSON    string
	ApprovalToken string
}

func (f *fakeReplayer) Replay(_ context.Context, tool string, paramsJSON json.RawMessage, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeReplayerCall{
		Tool:          tool,
		ParamsJSON:    string(paramsJSON),
		ApprovalToken: token,
	})
	return f.errToReturn
}

func (f *fakeReplayer) recorded() []fakeReplayerCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeReplayerCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeSpawnKickoff records every Kickoff call; substitutes a canned
// error when `errToReturn` is non-nil. Used by the propose_spawn
// branch tests to assert the dispatcher routes to SpawnKickoff (and
// not to the existing Replayer) for the M6.3.b spawn tool.
type fakeSpawnKickoff struct {
	mu          sync.Mutex
	calls       []fakeKickoffCall
	errToReturn error
}

type fakeKickoffCall struct {
	SagaID            uuid.UUID
	ManifestVersionID uuid.UUID
	ApprovalToken     string
}

func (f *fakeSpawnKickoff) Kickoff(_ context.Context, sagaID uuid.UUID, mvID uuid.UUID, token string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeKickoffCall{
		SagaID:            sagaID,
		ManifestVersionID: mvID,
		ApprovalToken:     token,
	})
	return f.errToReturn
}

func (f *fakeSpawnKickoff) recorded() []fakeKickoffCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeKickoffCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeKeepClient is a recording [keeperslog.LocalKeepClient]; lets
// the dispatcher tests assert event_type ordering without standing
// up the full HTTP keepclient stack.
type fakeKeepClient struct {
	mu   sync.Mutex
	rows []keepclient.LogAppendRequest
}

func (f *fakeKeepClient) LogAppend(_ context.Context, req keepclient.LogAppendRequest) (*keepclient.LogAppendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, req)
	return &keepclient.LogAppendResponse{ID: "row"}, nil
}

func (f *fakeKeepClient) appended() []keepclient.LogAppendRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]keepclient.LogAppendRequest, len(f.rows))
	copy(out, f.rows)
	return out
}

// eventTypes returns the chronological list of `event_type` strings
// the keepclient saw.
func (f *fakeKeepClient) eventTypes() []string {
	rows := f.appended()
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.EventType
	}
	return out
}

// newDispatcher wires a Dispatcher with the supplied DAO + Replayer
// onto a fresh in-process audit chain. Stamps a no-op SpawnKickoff so
// the M7.1.b panic-on-nil constructor remains satisfied for tests
// that exercise non-spawn branches. Returns the dispatcher plus the
// recording keepclient for assertions.
func newDispatcher(t *testing.T, dao spawn.PendingApprovalDAO, rp approval.Replayer) (*approval.Dispatcher, *fakeKeepClient) {
	t.Helper()
	fkc := &fakeKeepClient{}
	w := keeperslog.New(fkc)
	d := approval.New(dao, rp, &fakeSpawnKickoff{}, approval.WithAuditAppender(w))
	return d, fkc
}

// newDispatcherWithKickoff wires a Dispatcher exposing a recording
// SpawnKickoff so propose_spawn branch tests can assert the kickoff
// call shape. Otherwise identical to [newDispatcher].
func newDispatcherWithKickoff(
	t *testing.T,
	dao spawn.PendingApprovalDAO,
	rp approval.Replayer,
	sk approval.SpawnKickoff,
) (*approval.Dispatcher, *fakeKeepClient) {
	t.Helper()
	fkc := &fakeKeepClient{}
	w := keeperslog.New(fkc)
	d := approval.New(dao, rp, sk, approval.WithAuditAppender(w))
	return d, fkc
}

// blockActionsPayloadJSON builds a Slack `block_actions` payload as
// the inbound handler would forward it (Raw is the post-form-decode
// payload bytes).
func blockActionsPayloadJSON(actionID, value string) []byte {
	return []byte(`{"type":"block_actions","actions":[{"action_id":"` + actionID + `","value":"` + value + `"}]}`)
}

// seedPending inserts a pending row carrying `tool` + `paramsJSON`
// and returns its token. Hoisted so the per-branch tests stay
// scannable.
func seedPending(t *testing.T, dao *spawn.MemoryPendingApprovalDAO, tool, paramsJSON string) string {
	t.Helper()
	token := "tok-" + tool
	if err := dao.Insert(context.Background(), token, tool, json.RawMessage(paramsJSON)); err != nil {
		t.Fatalf("seed Insert: %v", err)
	}
	return token
}

// ── tests ──────────────────────────────────────────────────────────────

// TestDispatch_ApproveHappy pins AC6 + AC9 + the test plan
// "Handler happy approve": 4 audit events in order, replay invoked,
// DAO state flipped to approved.
func TestDispatch_ApproveHappy(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	tool := spawn.PendingApprovalToolAdjustPersonality
	params := `{"agent_id":"a-1","new_personality":"calm"}`
	token := seedPending(t, dao, tool, params)

	rp := &fakeReplayer{}
	d, fkc := newDispatcher(t, dao, rp)

	actionID := cards.EncodeActionID(tool, token)
	err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON(actionID, cards.ButtonValueApprove),
	})
	if err != nil {
		t.Fatalf("DispatchInteraction: %v", err)
	}

	wantEvents := []string{
		"approval_card_action_received",
		"approval_resolved",
		"approval_replay_succeeded",
	}
	if got := fkc.eventTypes(); !equalStringSlice(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}

	calls := rp.recorded()
	if len(calls) != 1 {
		t.Fatalf("Replay calls = %d, want 1", len(calls))
	}
	if calls[0].Tool != tool {
		t.Errorf("Replay tool = %q, want %q", calls[0].Tool, tool)
	}
	if calls[0].ApprovalToken != token {
		t.Errorf("Replay token = %q, want %q", calls[0].ApprovalToken, token)
	}
	if calls[0].ParamsJSON != params {
		t.Errorf("Replay paramsJSON = %q, want %q", calls[0].ParamsJSON, params)
	}

	// DAO state is approved.
	row, _ := dao.Get(context.Background(), token)
	if row.State != spawn.PendingApprovalStateApproved {
		t.Errorf("DAO state = %q, want approved", row.State)
	}
}

// TestDispatch_RejectHappy pins the test plan
// "Handler happy reject": 2 audit events, NO replay.
func TestDispatch_RejectHappy(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	tool := spawn.PendingApprovalToolAdjustLanguage
	token := seedPending(t, dao, tool, `{"agent_id":"a-1","new_language":"en"}`)

	rp := &fakeReplayer{}
	d, fkc := newDispatcher(t, dao, rp)

	actionID := cards.EncodeActionID(tool, token)
	err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON(actionID, cards.ButtonValueReject),
	})
	if err != nil {
		t.Fatalf("DispatchInteraction: %v", err)
	}

	wantEvents := []string{
		"approval_card_action_received",
		"approval_resolved",
	}
	if got := fkc.eventTypes(); !equalStringSlice(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
	if calls := rp.recorded(); len(calls) != 0 {
		t.Errorf("Replay called %d times, want 0", len(calls))
	}
	row, _ := dao.Get(context.Background(), token)
	if row.State != spawn.PendingApprovalStateRejected {
		t.Errorf("DAO state = %q, want rejected", row.State)
	}
}

// TestDispatch_InvalidButtonValue pins AC9: a payload with a valid
// action_id but an unrecognised button value emits 1 audit event with
// reason=invalid_button_value (not malformed_action_id); no DAO
// mutation; no replay.
func TestDispatch_InvalidButtonValue(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	tool := spawn.PendingApprovalToolProposeSpawn
	token := seedPending(t, dao, tool, `{}`)

	rp := &fakeReplayer{}
	d, fkc := newDispatcher(t, dao, rp)

	// Valid action_id, bogus button value.
	actionID := cards.EncodeActionID(tool, token)
	err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON(actionID, "bogus-value"),
	})
	if err == nil {
		t.Fatal("DispatchInteraction returned nil, want non-nil error")
	}
	if !errors.Is(err, cards.ErrInvalidButtonValue) {
		t.Errorf("err = %v, want ErrInvalidButtonValue", err)
	}

	rows := fkc.appended()
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	if rows[0].EventType != "approval_card_action_received" {
		t.Errorf("event_type = %q, want approval_card_action_received", rows[0].EventType)
	}
	if !strings.Contains(string(rows[0].Payload), `"reason":"invalid_button_value"`) {
		t.Errorf("payload missing invalid_button_value reason: %s", rows[0].Payload)
	}
	if strings.Contains(string(rows[0].Payload), `"reason":"malformed_action_id"`) {
		t.Errorf("payload must NOT contain malformed_action_id reason: %s", rows[0].Payload)
	}

	if calls := rp.recorded(); len(calls) != 0 {
		t.Errorf("Replay called %d times, want 0", len(calls))
	}
}

// TestDispatch_MalformedActionID pins AC9: 1 audit event with
// reason=malformed_action_id; no DAO mutation; no replay.
func TestDispatch_MalformedActionID(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	rp := &fakeReplayer{}
	d, fkc := newDispatcher(t, dao, rp)

	err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON("garbage-action-id", cards.ButtonValueApprove),
	})
	if err == nil {
		t.Fatal("DispatchInteraction returned nil, want non-nil error")
	}
	if !errors.Is(err, cards.ErrInvalidActionID) {
		t.Errorf("err = %v, want ErrInvalidActionID", err)
	}

	rows := fkc.appended()
	if len(rows) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(rows))
	}
	if rows[0].EventType != "approval_card_action_received" {
		t.Errorf("event_type = %q, want approval_card_action_received", rows[0].EventType)
	}
	if !strings.Contains(string(rows[0].Payload), `"reason":"malformed_action_id"`) {
		t.Errorf("payload missing malformed_action_id reason: %s", rows[0].Payload)
	}

	if calls := rp.recorded(); len(calls) != 0 {
		t.Errorf("Replay called %d times, want 0", len(calls))
	}
}

// TestDispatch_UnknownToken pins AC9: 2 audit events
// (received + resolved with reason=unknown_token); no replay.
func TestDispatch_UnknownToken(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	rp := &fakeReplayer{}
	d, fkc := newDispatcher(t, dao, rp)

	tool := spawn.PendingApprovalToolProposeSpawn
	actionID := cards.EncodeActionID(tool, "nonexistent-token")
	err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON(actionID, cards.ButtonValueApprove),
	})
	if err == nil {
		t.Fatal("DispatchInteraction returned nil, want non-nil error")
	}

	wantEvents := []string{
		"approval_card_action_received",
		"approval_resolved",
	}
	if got := fkc.eventTypes(); !equalStringSlice(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
	rows := fkc.appended()
	if !strings.Contains(string(rows[1].Payload), `"reason":"unknown_token"`) {
		t.Errorf("resolved payload missing unknown_token reason: %s", rows[1].Payload)
	}
	if calls := rp.recorded(); len(calls) != 0 {
		t.Errorf("Replay called %d times, want 0", len(calls))
	}
}

// TestDispatch_StaleState pins AC9: 2 audit events with
// reason=stale_state; no replay; DAO row unchanged.
func TestDispatch_StaleState(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	tool := spawn.PendingApprovalToolRetireWatchkeeper
	token := seedPending(t, dao, tool, `{"agent_id":"a-1"}`)
	// Pre-resolve so the dispatcher hits a terminal state.
	if err := dao.Resolve(context.Background(), token, spawn.PendingApprovalStateApproved); err != nil {
		t.Fatalf("seed Resolve: %v", err)
	}

	rp := &fakeReplayer{}
	d, fkc := newDispatcher(t, dao, rp)

	actionID := cards.EncodeActionID(tool, token)
	err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON(actionID, cards.ButtonValueApprove),
	})
	if err == nil {
		t.Fatal("DispatchInteraction returned nil, want non-nil error")
	}

	wantEvents := []string{
		"approval_card_action_received",
		"approval_resolved",
	}
	if got := fkc.eventTypes(); !equalStringSlice(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
	rows := fkc.appended()
	if !strings.Contains(string(rows[1].Payload), `"reason":"stale_state"`) {
		t.Errorf("resolved payload missing stale_state reason: %s", rows[1].Payload)
	}
	if calls := rp.recorded(); len(calls) != 0 {
		t.Errorf("Replay called %d times, want 0", len(calls))
	}
	// DAO state still approved (initial seed value).
	row, _ := dao.Get(context.Background(), token)
	if row.State != spawn.PendingApprovalStateApproved {
		t.Errorf("DAO state = %q, want approved (no rollback on stale)", row.State)
	}
}

// TestDispatch_ApproveDownstreamError pins AC9: 4 audit events ending
// with approval_replay_failed; DAO state stays `approved` (no
// rollback).
func TestDispatch_ApproveDownstreamError(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	// Use a non-spawn tool so the Approved-branch routes through the
	// Replayer (the M7.1.b spawn-saga branch is exercised separately).
	tool := spawn.PendingApprovalToolAdjustPersonality
	token := seedPending(t, dao, tool, `{"agent_id":"a-1"}`)

	replayErr := errors.New("downstream tool exploded")
	rp := &fakeReplayer{errToReturn: replayErr}
	d, fkc := newDispatcher(t, dao, rp)

	actionID := cards.EncodeActionID(tool, token)
	err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON(actionID, cards.ButtonValueApprove),
	})
	if err == nil {
		t.Fatal("DispatchInteraction returned nil, want non-nil error")
	}

	wantEvents := []string{
		"approval_card_action_received",
		"approval_resolved",
		"approval_replay_failed",
	}
	if got := fkc.eventTypes(); !equalStringSlice(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}

	// DAO state stayed `approved` — no rollback on replay error.
	row, _ := dao.Get(context.Background(), token)
	if row.State != spawn.PendingApprovalStateApproved {
		t.Errorf("DAO state = %q, want approved (no rollback)", row.State)
	}

	// `error_class` carried, error VALUE NOT.
	failedRow := fkc.appended()[2]
	if !strings.Contains(string(failedRow.Payload), `"error_class":"*errors.errorString"`) {
		t.Errorf("failed payload missing error_class: %s", failedRow.Payload)
	}
	if strings.Contains(string(failedRow.Payload), "downstream tool exploded") {
		t.Errorf("failed payload leaked error VALUE: %s", failedRow.Payload)
	}
}

// TestDispatch_ForeignInteractionType ACKs silently — no audit row,
// no DAO call. M6.3.b only owns block_actions.
func TestDispatch_ForeignInteractionType(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	rp := &fakeReplayer{}
	d, fkc := newDispatcher(t, dao, rp)

	err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "view_submission",
		Raw:  []byte(`{"type":"view_submission"}`),
	})
	if err != nil {
		t.Errorf("DispatchInteraction: %v", err)
	}
	if rows := fkc.appended(); len(rows) != 0 {
		t.Errorf("audit rows = %d, want 0", len(rows))
	}
	if calls := rp.recorded(); len(calls) != 0 {
		t.Errorf("Replay called %d times, want 0", len(calls))
	}
}

// TestDispatch_PIIDiscipline pins AC8 + the test plan
// "PII guard regression": audit payloads contain neither
// `params_json` nor any per-field VALUE the params snapshot carried.
//
// The check is on canary VALUES rather than field names because
// several legitimate payload keys (e.g. `tool_name`, `decision`)
// overlap by substring with M6.2.x request-struct field names
// (e.g. `personality` shows up inside `adjust_personality`). The
// PII concern is leaking the request CONTENT through the audit
// chain — a canary on the value bytes catches the only failure mode
// that matters.
func TestDispatch_PIIDiscipline(t *testing.T) {
	t.Parallel()

	const (
		canarySystemPrompt    = "CANARY-SYSTEM-PROMPT-VALUE"
		canaryNewPersonality  = "CANARY-NEW-PERSONALITY-VALUE"
		canaryNewLanguage     = "CANARY-NEW-LANGUAGE-VALUE"
		canaryAgentID         = "CANARY-AGENT-ID-VALUE"
		canaryParamsJSONField = "params_json" // field name MUST NOT appear on any audit row.
	)
	leakValues := []string{
		canarySystemPrompt,
		canaryNewPersonality,
		canaryNewLanguage,
		canaryAgentID,
		canaryParamsJSONField,
	}

	tool := spawn.PendingApprovalToolAdjustPersonality
	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	// Seed with params carrying every canary as a field VALUE — any
	// code path that ever forwards `params_json` content onto the
	// audit chain will fail this test.
	params := `{"agent_id":"` + canaryAgentID + `","system_prompt":"` + canarySystemPrompt + `","new_personality":"` + canaryNewPersonality + `","new_language":"` + canaryNewLanguage + `"}`
	token := seedPending(t, dao, tool, params)

	rp := &fakeReplayer{}
	d, fkc := newDispatcher(t, dao, rp)

	actionID := cards.EncodeActionID(tool, token)
	if err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON(actionID, cards.ButtonValueApprove),
	}); err != nil {
		t.Fatalf("DispatchInteraction: %v", err)
	}

	// leakFieldNames lists param-struct field names that must NOT appear
	// in any audit payload as JSON keys. These are the field names from
	// the M6.2.x request structs that are NOT part of the closed-set
	// audit vocabulary (tool_name, approval_token, decision, reason,
	// error_class, interaction_type).
	leakFieldNames := []string{
		`"system_prompt"`,
		`"new_personality"`,
		`"new_language"`,
		`"display_name"`,
		`"agent_id"`,
	}

	for _, row := range fkc.appended() {
		payload := string(row.Payload)
		for _, leak := range leakValues {
			if strings.Contains(payload, leak) {
				t.Errorf("audit row %q leaked value %q in payload: %s", row.EventType, leak, payload)
			}
		}
		for _, fieldName := range leakFieldNames {
			if strings.Contains(payload, fieldName) {
				t.Errorf("audit row %q leaked field name %q in payload: %s", row.EventType, fieldName, payload)
			}
		}
	}
}

// TestDispatch_NoAuditAppenderIsNoop sanity-checks the audit-nil
// fallback path: a dispatcher built without an audit appender
// must not panic (test-only mode). Mirrors the inbound handler's
// nil-audit policy.
func TestDispatch_NoAuditAppenderIsNoop(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	// Use a non-spawn tool so the test exercises the Replayer fallback
	// path (the spawn branch has its own test for the audit-nil
	// fallback semantics).
	tool := spawn.PendingApprovalToolAdjustPersonality
	token := seedPending(t, dao, tool, `{}`)

	rp := &fakeReplayer{}
	d := approval.New(dao, rp, &fakeSpawnKickoff{}) // no WithAuditAppender
	actionID := cards.EncodeActionID(tool, token)
	if err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON(actionID, cards.ButtonValueApprove),
	}); err != nil {
		t.Errorf("DispatchInteraction: %v", err)
	}
	if calls := rp.recorded(); len(calls) != 1 {
		t.Errorf("Replay calls = %d, want 1", len(calls))
	}
}

// TestNew_PanicsOnNilDeps pins the panic discipline (M6.3.b
// dependency-required pattern): nil DAO, nil Replayer, or nil
// SpawnKickoff is a programmer bug, not a runtime branch.
func TestNew_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	defer func() { _ = recover() }()
	_ = approval.New(nil, &fakeReplayer{}, &fakeSpawnKickoff{})
	t.Fatal("New(nil, replayer, kickoff) did not panic")
}

func TestNew_PanicsOnNilReplayer(t *testing.T) {
	t.Parallel()

	defer func() { _ = recover() }()
	_ = approval.New(spawn.NewMemoryPendingApprovalDAO(nil), nil, &fakeSpawnKickoff{})
	t.Fatal("New(dao, nil, kickoff) did not panic")
}

// TestNew_PanicsOnNilSpawnKickoff pins the new M7.1.b dependency:
// SpawnKickoff is a required parameter; a nil kickoff is a programmer
// bug, not a runtime branch.
func TestNew_PanicsOnNilSpawnKickoff(t *testing.T) {
	t.Parallel()

	defer func() { _ = recover() }()
	_ = approval.New(spawn.NewMemoryPendingApprovalDAO(nil), &fakeReplayer{}, nil)
	t.Fatal("New(dao, replayer, nil) did not panic")
}

// ── propose_spawn branch tests (M7.1.b) ───────────────────────────────────

// TestDispatch_ProposeSpawn_ApproveRoutesToKickoff pins AC3 + the M7.1.b
// happy-path test plan: an approve action on a `propose_spawn` pending
// row routes to SpawnKickoff (NOT to the existing Replayer); the audit
// chain is identical to the non-spawn approve path because the kickoff
// itself owns the `manifest_approved_for_spawn` event emission.
func TestDispatch_ProposeSpawn_ApproveRoutesToKickoff(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	tool := spawn.PendingApprovalToolProposeSpawn
	token := seedPending(t, dao, tool, `{"agent_id":"a-1"}`)

	rp := &fakeReplayer{}
	sk := &fakeSpawnKickoff{}
	d, fkc := newDispatcherWithKickoff(t, dao, rp, sk)

	actionID := cards.EncodeActionID(tool, token)
	if err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON(actionID, cards.ButtonValueApprove),
	}); err != nil {
		t.Fatalf("DispatchInteraction: %v", err)
	}

	// SpawnKickoff was invoked exactly once; the existing Replayer was
	// NOT invoked at all (the propose_spawn branch fans out to the saga).
	if calls := sk.recorded(); len(calls) != 1 {
		t.Errorf("SpawnKickoff calls = %d, want 1", len(calls))
	} else {
		if calls[0].ApprovalToken != token {
			t.Errorf("SpawnKickoff token = %q, want %q", calls[0].ApprovalToken, token)
		}
		if calls[0].SagaID == uuid.Nil {
			t.Errorf("SpawnKickoff sagaID = nil, want a non-zero uuid")
		}
		if calls[0].ManifestVersionID == uuid.Nil {
			t.Errorf("SpawnKickoff manifestVersionID = nil, want a non-zero uuid")
		}
	}
	if calls := rp.recorded(); len(calls) != 0 {
		t.Errorf("Replay calls = %d, want 0 (propose_spawn must NOT route to Replayer)", len(calls))
	}

	// Audit chain mirrors the regular approve flow; the kickoff-side
	// `manifest_approved_for_spawn` + `saga_completed` events are
	// emitted by the kickoffer (see spawn package tests).
	wantEvents := []string{
		"approval_card_action_received",
		"approval_resolved",
		"approval_replay_succeeded",
	}
	if got := fkc.eventTypes(); !equalStringSlice(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
}

// TestDispatch_NonSpawn_ApproveRoutesToReplayer pins the M7.1.b
// negative-side guard: an approve action on a non-spawn tool stays on
// the existing Replayer path; SpawnKickoff is NOT invoked.
func TestDispatch_NonSpawn_ApproveRoutesToReplayer(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	tool := spawn.PendingApprovalToolAdjustPersonality
	token := seedPending(t, dao, tool, `{"agent_id":"a-1","new_personality":"calm"}`)

	rp := &fakeReplayer{}
	sk := &fakeSpawnKickoff{}
	d, fkc := newDispatcherWithKickoff(t, dao, rp, sk)

	actionID := cards.EncodeActionID(tool, token)
	if err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON(actionID, cards.ButtonValueApprove),
	}); err != nil {
		t.Fatalf("DispatchInteraction: %v", err)
	}

	if calls := sk.recorded(); len(calls) != 0 {
		t.Errorf("SpawnKickoff calls = %d, want 0", len(calls))
	}
	if calls := rp.recorded(); len(calls) != 1 {
		t.Errorf("Replay calls = %d, want 1", len(calls))
	}
	wantEvents := []string{
		"approval_card_action_received",
		"approval_resolved",
		"approval_replay_succeeded",
	}
	if got := fkc.eventTypes(); !equalStringSlice(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
}

// TestDispatch_ProposeSpawn_RejectDoesNotKickoff pins the test plan
// "Reject on a propose_spawn pending row → neither SpawnKickoff nor
// Replayer called; existing rejection path unchanged".
func TestDispatch_ProposeSpawn_RejectDoesNotKickoff(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	tool := spawn.PendingApprovalToolProposeSpawn
	token := seedPending(t, dao, tool, `{"agent_id":"a-1"}`)

	rp := &fakeReplayer{}
	sk := &fakeSpawnKickoff{}
	d, fkc := newDispatcherWithKickoff(t, dao, rp, sk)

	actionID := cards.EncodeActionID(tool, token)
	if err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON(actionID, cards.ButtonValueReject),
	}); err != nil {
		t.Fatalf("DispatchInteraction: %v", err)
	}

	if calls := sk.recorded(); len(calls) != 0 {
		t.Errorf("SpawnKickoff calls = %d, want 0", len(calls))
	}
	if calls := rp.recorded(); len(calls) != 0 {
		t.Errorf("Replay calls = %d, want 0", len(calls))
	}
	wantEvents := []string{
		"approval_card_action_received",
		"approval_resolved",
	}
	if got := fkc.eventTypes(); !equalStringSlice(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
	row, _ := dao.Get(context.Background(), token)
	if row.State != spawn.PendingApprovalStateRejected {
		t.Errorf("DAO state = %q, want rejected", row.State)
	}
}

// TestDispatch_ProposeSpawn_KickoffError surfaces the kickoff error on
// the audit chain via `approval_replay_failed` (the same event-type
// vocabulary as the Replayer-error path so a downstream consumer can
// branch on `error_class` rather than re-keying its query).
func TestDispatch_ProposeSpawn_KickoffError(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	tool := spawn.PendingApprovalToolProposeSpawn
	token := seedPending(t, dao, tool, `{"agent_id":"a-1"}`)

	kickErr := errors.New("kickoff exploded")
	rp := &fakeReplayer{}
	sk := &fakeSpawnKickoff{errToReturn: kickErr}
	d, fkc := newDispatcherWithKickoff(t, dao, rp, sk)

	actionID := cards.EncodeActionID(tool, token)
	err := d.DispatchInteraction(context.Background(), inbound.Interaction{
		Type: "block_actions",
		Raw:  blockActionsPayloadJSON(actionID, cards.ButtonValueApprove),
	})
	if err == nil {
		t.Fatal("DispatchInteraction returned nil, want non-nil error")
	}

	wantEvents := []string{
		"approval_card_action_received",
		"approval_resolved",
		"approval_replay_failed",
	}
	if got := fkc.eventTypes(); !equalStringSlice(got, wantEvents) {
		t.Fatalf("event_type chain = %v, want %v", got, wantEvents)
	}
	// Replay was NOT invoked (the spawn branch routes to SpawnKickoff).
	if calls := rp.recorded(); len(calls) != 0 {
		t.Errorf("Replay calls = %d, want 0", len(calls))
	}
	// Failed-row payload carries the error CLASS but never the value.
	failedRow := fkc.appended()[2]
	if !strings.Contains(string(failedRow.Payload), `"error_class":"*errors.errorString"`) {
		t.Errorf("failed payload missing error_class: %s", failedRow.Payload)
	}
	if strings.Contains(string(failedRow.Payload), "kickoff exploded") {
		t.Errorf("failed payload leaked error VALUE: %s", failedRow.Payload)
	}
}

// equalStringSlice is a tiny helper that beats reflect.DeepEqual for
// readability on assertion failure.
func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
