package dm_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/cards"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/inbound"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/inbound/dm"
	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/inbound/intent"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ── real fakes (M5.6 discipline — no mocks of unit under test) ─────────

// fakeReadDisp records every Dispatch call; substitutes a canned
// response or error.
type fakeReadDisp struct {
	mu    sync.Mutex
	calls []dm.ReadToolRequest
	resp  dm.ReadToolResponse
	err   error
}

func (f *fakeReadDisp) Dispatch(_ context.Context, in dm.ReadToolRequest) (dm.ReadToolResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	return f.resp, f.err
}

func (f *fakeReadDisp) recorded() []dm.ReadToolRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dm.ReadToolRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeProposer records Invoke calls; substitutes a canned response or
// error.
type fakeProposer struct {
	mu    sync.Mutex
	calls []dm.ProposeSpawnRequest
	resp  dm.ProposeSpawnResponse
	err   error
}

func (f *fakeProposer) Invoke(_ context.Context, in dm.ProposeSpawnRequest) (dm.ProposeSpawnResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	return f.resp, f.err
}

func (f *fakeProposer) recorded() []dm.ProposeSpawnRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]dm.ProposeSpawnRequest, len(f.calls))
	copy(out, f.calls)
	return out
}

// fakeOutbound records Post calls; substitutes a canned error.
type fakeOutbound struct {
	mu    sync.Mutex
	posts []fakeOutboundPost
	err   error
}

type fakeOutboundPost struct {
	ChannelID    string
	Blocks       []cards.Block
	FallbackText string
}

func (f *fakeOutbound) Post(_ context.Context, channelID string, blocks []cards.Block, fallbackText string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.posts = append(f.posts, fakeOutboundPost{ChannelID: channelID, Blocks: blocks, FallbackText: fallbackText})
	return f.err
}

func (f *fakeOutbound) recorded() []fakeOutboundPost {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeOutboundPost, len(f.posts))
	copy(out, f.posts)
	return out
}

// fakeKeepClient is a recording [keeperslog.LocalKeepClient]; mirrors
// the M6.3.b dispatcher test fake.
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

func (f *fakeKeepClient) eventTypes() []string {
	rows := f.appended()
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.EventType
	}
	return out
}

// ── helpers ────────────────────────────────────────────────────────────

// newDispatcher constructs the SUT with sensible defaults. The
// returned fakes are aliased onto local copies so the test asserts on
// the same objects it injected.
type harness struct {
	d         *dm.Dispatcher
	dao       *spawn.MemoryPendingApprovalDAO
	read      *fakeReadDisp
	proposer  *fakeProposer
	outbound  *fakeOutbound
	keepers   *fakeKeepClient
	tokenSeen string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	read := &fakeReadDisp{}
	proposer := &fakeProposer{}
	outbound := &fakeOutbound{}
	fkc := &fakeKeepClient{}
	w := keeperslog.New(fkc)

	const tok = "tok-fixture"
	h := &harness{
		dao:       dao,
		read:      read,
		proposer:  proposer,
		outbound:  outbound,
		keepers:   fkc,
		tokenSeen: tok,
	}
	h.d = dm.New(
		intent.NewParser(),
		read,
		proposer,
		dao,
		outbound,
		dm.WithAuditAppender(w),
		dm.WithClaim(spawn.Claim{OrganizationID: "org-1", AgentID: "wm-1"}),
		dm.WithTokenMint(func() (string, error) { return tok, nil }),
	)
	return h
}

// dmEvent builds an inbound.Event whose Inner is a Slack `message`
// event with the supplied attributes.
func dmEvent(text string, opts ...func(*messageEventBuilder)) inbound.Event {
	b := messageEventBuilder{
		Type:        "message",
		Channel:     "D-CHANNEL",
		ChannelType: "im",
		User:        "U-ADMIN",
		Text:        text,
	}
	for _, o := range opts {
		o(&b)
	}
	body, _ := json.Marshal(b)
	return inbound.Event{Type: "message", Inner: body}
}

type messageEventBuilder struct {
	Type        string `json:"type"`
	Channel     string `json:"channel"`
	ChannelType string `json:"channel_type"`
	User        string `json:"user"`
	BotID       string `json:"bot_id,omitempty"`
	Subtype     string `json:"subtype,omitempty"`
	Text        string `json:"text"`
}

func withChannelType(ct string) func(*messageEventBuilder) {
	return func(b *messageEventBuilder) { b.ChannelType = ct }
}

func withSubtype(s string) func(*messageEventBuilder) {
	return func(b *messageEventBuilder) { b.Subtype = s }
}

func withBotID(id string) func(*messageEventBuilder) {
	return func(b *messageEventBuilder) { b.BotID = id; b.User = "" }
}

func equalStrings(a, b []string) bool {
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

// ── tests ──────────────────────────────────────────────────────────────

// TestDispatch_ReadOnly_Happy pins the test plan:
// "what's running?" → ReadToolDispatcher.list_watchkeepers called;
// outbound DM posted; 2 audit events.
func TestDispatch_ReadOnly_Happy(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.read.resp = dm.ReadToolResponse{
		Blocks:       []cards.Block{{Type: "section"}},
		FallbackText: "running watchkeepers: 3",
	}

	if err := h.d.DispatchEvent(context.Background(), dmEvent("what's running?")); err != nil {
		t.Fatalf("DispatchEvent: %v", err)
	}

	want := []string{"slack_dm_received", "slack_dm_dispatched_read_only"}
	if got := h.keepers.eventTypes(); !equalStrings(got, want) {
		t.Fatalf("event chain = %v, want %v", got, want)
	}
	if calls := h.read.recorded(); len(calls) != 1 || calls[0].Intent != intent.IntentReadList {
		t.Errorf("read tool calls = %v", calls)
	}
	if posts := h.outbound.recorded(); len(posts) != 1 {
		t.Fatalf("outbound posts = %d, want 1", len(posts))
	} else if posts[0].ChannelID != "D-CHANNEL" || posts[0].FallbackText != "running watchkeepers: 3" {
		t.Errorf("outbound payload = %+v", posts[0])
	}
}

// TestDispatch_ReadOnly_Cost / Health pin the bucket routing.
func TestDispatch_ReadOnly_Cost(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.read.resp = dm.ReadToolResponse{FallbackText: "ok"}
	if err := h.d.DispatchEvent(context.Background(), dmEvent("show costs")); err != nil {
		t.Fatalf("DispatchEvent: %v", err)
	}
	if calls := h.read.recorded(); len(calls) != 1 || calls[0].Intent != intent.IntentReportCost {
		t.Errorf("read tool calls = %v", calls)
	}
}

func TestDispatch_ReadOnly_Health(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.read.resp = dm.ReadToolResponse{FallbackText: "ok"}
	if err := h.d.DispatchEvent(context.Background(), dmEvent("health check")); err != nil {
		t.Fatalf("DispatchEvent: %v", err)
	}
	if calls := h.read.recorded(); len(calls) != 1 || calls[0].Intent != intent.IntentReportHealth {
		t.Errorf("read tool calls = %v", calls)
	}
}

// TestDispatch_Propose_Happy pins the test plan:
// "propose a Coordinator for the backend team" → propose_spawn called;
// DAO.Insert called with token+params; approval card rendered;
// outbound DM posted; 2 audit events.
func TestDispatch_Propose_Happy(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	params := []byte(`{"role":"coordinator","team":"backend"}`)
	h.proposer.resp = dm.ProposeSpawnResponse{
		CardInput: cards.ProposeSpawnCardInput{
			AgentID:      "agent-uuid-1",
			Personality:  "calm",
			Language:     "en",
			SystemPrompt: "you are a watchkeeper",
		},
		ParamsJSON: params,
	}

	err := h.d.DispatchEvent(context.Background(), dmEvent("propose a Coordinator for the backend team"))
	if err != nil {
		t.Fatalf("DispatchEvent: %v", err)
	}

	want := []string{"slack_dm_received", "slack_dm_dispatched_manifest_bump"}
	if got := h.keepers.eventTypes(); !equalStrings(got, want) {
		t.Fatalf("event chain = %v, want %v", got, want)
	}

	calls := h.proposer.recorded()
	if len(calls) != 1 {
		t.Fatalf("propose calls = %d, want 1", len(calls))
	}
	if calls[0].Team != "backend" || calls[0].Role != "coordinator" {
		t.Errorf("propose call = %+v", calls[0])
	}

	row, err := h.dao.Get(context.Background(), h.tokenSeen)
	if err != nil {
		t.Fatalf("DAO Get: %v", err)
	}
	if row.ToolName != spawn.PendingApprovalToolProposeSpawn {
		t.Errorf("DAO row tool = %q", row.ToolName)
	}
	if string(row.ParamsJSON) != string(params) {
		t.Errorf("DAO row params = %s", row.ParamsJSON)
	}

	posts := h.outbound.recorded()
	if len(posts) != 1 || len(posts[0].Blocks) == 0 {
		t.Fatalf("outbound posts = %+v", posts)
	}
}

// TestDispatch_Unknown pins the test plan:
// "tell me a joke" → 2 audits; outbound DM with help text; NO tool
// call.
func TestDispatch_Unknown(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := h.d.DispatchEvent(context.Background(), dmEvent("tell me a joke")); err != nil {
		t.Fatalf("DispatchEvent: %v", err)
	}
	want := []string{"slack_dm_received", "slack_dm_unknown_intent"}
	if got := h.keepers.eventTypes(); !equalStrings(got, want) {
		t.Fatalf("event chain = %v, want %v", got, want)
	}
	if calls := h.read.recorded(); len(calls) != 0 {
		t.Errorf("read tool calls = %d, want 0", len(calls))
	}
	if calls := h.proposer.recorded(); len(calls) != 0 {
		t.Errorf("propose calls = %d, want 0", len(calls))
	}
	if posts := h.outbound.recorded(); len(posts) != 1 {
		t.Fatalf("outbound posts = %d, want 1", len(posts))
	} else if !strings.Contains(posts[0].FallbackText, "didn't understand") {
		t.Errorf("outbound text = %q (missing help)", posts[0].FallbackText)
	}
}

// TestDispatch_NonDMChannel pins AC6: channel posts produce 0 audits.
func TestDispatch_NonDMChannel(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := h.d.DispatchEvent(context.Background(), dmEvent("what's running?", withChannelType("channel"))); err != nil {
		t.Errorf("DispatchEvent: %v", err)
	}
	if rows := h.keepers.appended(); len(rows) != 0 {
		t.Errorf("audit rows = %d, want 0", len(rows))
	}
	if calls := h.read.recorded(); len(calls) != 0 {
		t.Errorf("read tool calls = %d, want 0", len(calls))
	}
	if posts := h.outbound.recorded(); len(posts) != 0 {
		t.Errorf("outbound posts = %d, want 0", len(posts))
	}
}

// TestDispatch_BotMessage pins AC6: bot messages produce 0 audits.
func TestDispatch_BotMessage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := h.d.DispatchEvent(context.Background(), dmEvent("what's running?", withBotID("B0BOT"))); err != nil {
		t.Errorf("DispatchEvent: %v", err)
	}
	if rows := h.keepers.appended(); len(rows) != 0 {
		t.Errorf("audit rows = %d, want 0", len(rows))
	}
}

// TestDispatch_MessageSubtype pins AC6: edits / channel-join etc. emit
// 0 audits.
func TestDispatch_MessageSubtype(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := h.d.DispatchEvent(context.Background(), dmEvent("what's running?", withSubtype("message_changed"))); err != nil {
		t.Errorf("DispatchEvent: %v", err)
	}
	if rows := h.keepers.appended(); len(rows) != 0 {
		t.Errorf("audit rows = %d, want 0", len(rows))
	}
}

// TestDispatch_EmptyText pins AC6: empty text → 0 audits.
func TestDispatch_EmptyText(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := h.d.DispatchEvent(context.Background(), dmEvent("")); err != nil {
		t.Errorf("DispatchEvent: %v", err)
	}
	if rows := h.keepers.appended(); len(rows) != 0 {
		t.Errorf("audit rows = %d, want 0", len(rows))
	}
}

// TestDispatch_ReadToolError pins AC6: tool error path → 2 audits
// (received + failed) + apology DM.
func TestDispatch_ReadToolError(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.read.err = errors.New("downstream read tool exploded")

	if err := h.d.DispatchEvent(context.Background(), dmEvent("what's running?")); err == nil {
		t.Fatal("DispatchEvent returned nil, want non-nil")
	}

	want := []string{"slack_dm_received", "slack_dm_failed"}
	if got := h.keepers.eventTypes(); !equalStrings(got, want) {
		t.Fatalf("event chain = %v, want %v", got, want)
	}
	rows := h.keepers.appended()
	failedPayload := string(rows[1].Payload)
	if !strings.Contains(failedPayload, `"reason":"read_tool_error"`) {
		t.Errorf("failed payload missing reason: %s", failedPayload)
	}
	if strings.Contains(failedPayload, "downstream read tool exploded") {
		t.Errorf("failed payload leaked error VALUE: %s", failedPayload)
	}

	posts := h.outbound.recorded()
	if len(posts) != 1 {
		t.Fatalf("outbound posts = %d, want 1 (apology)", len(posts))
	}
	if !strings.Contains(posts[0].FallbackText, "couldn't complete") {
		t.Errorf("apology text = %q", posts[0].FallbackText)
	}
}

// TestDispatch_DAOInsertError pins AC6: DAO Insert error → 2 audits.
func TestDispatch_DAOInsertError(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.proposer.resp = dm.ProposeSpawnResponse{
		CardInput:  cards.ProposeSpawnCardInput{AgentID: "agent-1"},
		ParamsJSON: []byte(`{}`),
	}
	// Pre-populate DAO with the same token so Insert collides.
	if err := h.dao.Insert(context.Background(), h.tokenSeen, spawn.PendingApprovalToolProposeSpawn, []byte(`{}`)); err != nil {
		t.Fatalf("seed Insert: %v", err)
	}

	err := h.d.DispatchEvent(context.Background(), dmEvent("propose a Coordinator for the backend team"))
	if err == nil {
		t.Fatal("DispatchEvent returned nil, want non-nil")
	}

	want := []string{"slack_dm_received", "slack_dm_failed"}
	if got := h.keepers.eventTypes(); !equalStrings(got, want) {
		t.Fatalf("event chain = %v, want %v", got, want)
	}
	failed := string(h.keepers.appended()[1].Payload)
	if !strings.Contains(failed, `"reason":"dao_insert_error"`) {
		t.Errorf("failed payload missing dao_insert_error: %s", failed)
	}
}

// TestDispatch_PIIDiscipline pins AC7: audit payloads contain
// neither raw DM text nor any tool params (replicates the M6.3.b
// canary pattern adapted for the DM router's branches).
func TestDispatch_PIIDiscipline(t *testing.T) {
	t.Parallel()

	const (
		canaryRawText      = "CANARY-RAW-DM-TEXT-VALUE"
		canaryRole         = "CANARY-ROLE-VALUE"
		canaryTeam         = "CANARY-TEAM-VALUE"
		canarySystemPrompt = "CANARY-SYSTEM-PROMPT-VALUE"
		canaryPersonality  = "CANARY-PERSONALITY-VALUE"
	)
	leakValues := []string{canaryRawText, canaryRole, canaryTeam, canarySystemPrompt, canaryPersonality}
	leakFields := []string{`"text"`, `"system_prompt"`, `"new_personality"`, `"new_language"`, `"params_json"`}

	h := newHarness(t)
	h.proposer.resp = dm.ProposeSpawnResponse{
		CardInput: cards.ProposeSpawnCardInput{
			AgentID:      "agent-1",
			Personality:  canaryPersonality,
			SystemPrompt: canarySystemPrompt,
		},
		ParamsJSON: []byte(
			`{"role":"` + canaryRole + `","team":"` + canaryTeam + `","system_prompt":"` + canarySystemPrompt + `"}`,
		),
	}
	// The DM text intentionally embeds the canary so the test would
	// fail if any audit row reflected it.
	if err := h.d.DispatchEvent(
		context.Background(),
		dmEvent("propose a Coordinator for the backend team "+canaryRawText),
	); err != nil {
		t.Fatalf("DispatchEvent: %v", err)
	}

	for _, row := range h.keepers.appended() {
		body := string(row.Payload)
		for _, leak := range leakValues {
			if strings.Contains(body, leak) {
				t.Errorf("row %q leaked value %q: %s", row.EventType, leak, body)
			}
		}
		for _, name := range leakFields {
			if strings.Contains(body, name) {
				t.Errorf("row %q leaked field name %q: %s", row.EventType, name, body)
			}
		}
	}
}

// TestDispatch_OutboundSmoke pins the test plan:
// SendMessage seam called with the right channel id + blocks payload.
func TestDispatch_OutboundSmoke(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	wantBlocks := []cards.Block{{Type: "section", Text: &cards.Text{Type: "mrkdwn", Text: "Hello"}}}
	h.read.resp = dm.ReadToolResponse{Blocks: wantBlocks, FallbackText: "Hello"}

	if err := h.d.DispatchEvent(context.Background(), dmEvent("what's running?")); err != nil {
		t.Fatalf("DispatchEvent: %v", err)
	}

	posts := h.outbound.recorded()
	if len(posts) != 1 {
		t.Fatalf("outbound posts = %d, want 1", len(posts))
	}
	if posts[0].ChannelID != "D-CHANNEL" {
		t.Errorf("channel id = %q, want D-CHANNEL", posts[0].ChannelID)
	}
	if len(posts[0].Blocks) != 1 || posts[0].Blocks[0].Type != "section" {
		t.Errorf("blocks = %+v, want one section", posts[0].Blocks)
	}
}

// TestDispatch_NonMessageEvent ACKs silently.
func TestDispatch_NonMessageEvent(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if err := h.d.DispatchEvent(context.Background(), inbound.Event{Type: "app_home_opened"}); err != nil {
		t.Errorf("DispatchEvent: %v", err)
	}
	if rows := h.keepers.appended(); len(rows) != 0 {
		t.Errorf("audit rows = %d, want 0", len(rows))
	}
}

// TestDispatch_Propose_RenderError pins that a card-render failure (empty
// actionID) leaves 0 DAO rows: the pending row must NOT be inserted when
// the card cannot be rendered.
func TestDispatch_Propose_RenderError(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	// AgentID is intentionally empty so RenderProposeSpawn returns an
	// empty actionID, triggering the render-error branch.
	h.proposer.resp = dm.ProposeSpawnResponse{
		CardInput:  cards.ProposeSpawnCardInput{AgentID: ""},
		ParamsJSON: []byte(`{}`),
	}

	err := h.d.DispatchEvent(context.Background(), dmEvent("propose a Coordinator for the backend team"))
	if err == nil {
		t.Fatal("DispatchEvent returned nil, want non-nil")
	}

	want := []string{"slack_dm_received", "slack_dm_failed"}
	if got := h.keepers.eventTypes(); !equalStrings(got, want) {
		t.Fatalf("event chain = %v, want %v", got, want)
	}
	failed := string(h.keepers.appended()[1].Payload)
	if !strings.Contains(failed, `"reason":"render_error"`) {
		t.Errorf("failed payload missing render_error: %s", failed)
	}

	// The invariant: no DAO row persisted when render fails.
	_, daoErr := h.dao.Get(context.Background(), h.tokenSeen)
	if daoErr == nil {
		t.Error("DAO row exists after render-error branch — orphaned pending row")
	}

	posts := h.outbound.recorded()
	if len(posts) != 1 {
		t.Fatalf("outbound posts = %d, want 1 (apology)", len(posts))
	}
	if !strings.Contains(posts[0].FallbackText, "couldn't complete") {
		t.Errorf("apology text = %q", posts[0].FallbackText)
	}
}

// TestDispatch_Propose_TokenMintError pins that a token-mint failure
// results in 2 audits (received + failed with reason=token_mint_error),
// 0 DAO rows, 0 proposer calls, and an apology DM.
func TestDispatch_Propose_TokenMintError(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	read := &fakeReadDisp{}
	proposer := &fakeProposer{}
	outbound := &fakeOutbound{}
	fkc := &fakeKeepClient{}
	w := keeperslog.New(fkc)

	mintErr := errors.New("uuid entropy failure")
	d := dm.New(
		intent.NewParser(),
		read,
		proposer,
		dao,
		outbound,
		dm.WithAuditAppender(w),
		dm.WithClaim(spawn.Claim{OrganizationID: "org-1", AgentID: "wm-1"}),
		dm.WithTokenMint(func() (string, error) { return "", mintErr }),
	)

	err := d.DispatchEvent(context.Background(), dmEvent("propose a Coordinator for the backend team"))
	if err == nil {
		t.Fatal("DispatchEvent returned nil, want non-nil")
	}

	want := []string{"slack_dm_received", "slack_dm_failed"}
	if got := fkc.eventTypes(); !equalStrings(got, want) {
		t.Fatalf("event chain = %v, want %v", got, want)
	}
	failed := string(fkc.appended()[1].Payload)
	if !strings.Contains(failed, `"reason":"token_mint_error"`) {
		t.Errorf("failed payload missing token_mint_error: %s", failed)
	}

	// 0 DAO rows: Get on any token must return not-found.
	if _, daoErr := dao.Get(context.Background(), "tok-any"); daoErr == nil {
		t.Error("DAO row exists after token-mint-error branch — unexpected pending row")
	}

	// 0 proposer calls.
	if calls := proposer.recorded(); len(calls) != 0 {
		t.Errorf("proposer calls = %d, want 0", len(calls))
	}

	// Apology DM posted.
	posts := outbound.recorded()
	if len(posts) != 1 {
		t.Fatalf("outbound posts = %d, want 1 (apology)", len(posts))
	}
	if !strings.Contains(posts[0].FallbackText, "couldn't complete") {
		t.Errorf("apology text = %q", posts[0].FallbackText)
	}
}

// TestNew_PanicsOnNilDeps pins the panic discipline.
func TestNew_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	dao := spawn.NewMemoryPendingApprovalDAO(nil)
	cases := []struct {
		name string
		fn   func()
	}{
		{"nil_parser", func() {
			_ = dm.New(nil, &fakeReadDisp{}, &fakeProposer{}, dao, &fakeOutbound{})
		}},
		{"nil_read", func() {
			_ = dm.New(intent.NewParser(), nil, &fakeProposer{}, dao, &fakeOutbound{})
		}},
		{"nil_proposer", func() {
			_ = dm.New(intent.NewParser(), &fakeReadDisp{}, nil, dao, &fakeOutbound{})
		}},
		{"nil_dao", func() {
			_ = dm.New(intent.NewParser(), &fakeReadDisp{}, &fakeProposer{}, nil, &fakeOutbound{})
		}},
		{"nil_outbound", func() {
			_ = dm.New(intent.NewParser(), &fakeReadDisp{}, &fakeProposer{}, dao, nil)
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Fatalf("New(%s) did not panic", tc.name)
				}
			}()
			tc.fn()
		})
	}
}
