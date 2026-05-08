package cards_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/cards"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// TestEncodeDecodeActionID_RoundTrip pins AC2: a tool/token pair
// round-trips through Encode → Decode without mutation, for each of
// the four supported tool names.
func TestEncodeDecodeActionID_RoundTrip(t *testing.T) {
	t.Parallel()

	tools := []string{
		spawn.PendingApprovalToolProposeSpawn,
		spawn.PendingApprovalToolAdjustPersonality,
		spawn.PendingApprovalToolAdjustLanguage,
		spawn.PendingApprovalToolRetireWatchkeeper,
	}
	for _, tool := range tools {
		token := "tok-" + tool
		encoded := cards.EncodeActionID(tool, token)
		if encoded == "" {
			t.Errorf("EncodeActionID(%q,%q) = empty", tool, token)
			continue
		}
		gotTool, gotToken, err := cards.DecodeActionID(encoded)
		if err != nil {
			t.Errorf("DecodeActionID(%q): %v", encoded, err)
			continue
		}
		if gotTool != tool {
			t.Errorf("decoded tool = %q, want %q", gotTool, tool)
		}
		if gotToken != token {
			t.Errorf("decoded token = %q, want %q", gotToken, token)
		}
	}
}

// TestEncodeActionID_Empty pins the EncodeActionID guard: empty tool
// or token returns the empty string (programmer-bug signal).
func TestEncodeActionID_Empty(t *testing.T) {
	t.Parallel()

	if got := cards.EncodeActionID("", "tok"); got != "" {
		t.Errorf("EncodeActionID(\"\",\"tok\") = %q, want \"\"", got)
	}
	if got := cards.EncodeActionID("propose_spawn", ""); got != "" {
		t.Errorf("EncodeActionID(\"propose_spawn\",\"\") = %q, want \"\"", got)
	}
}

// TestDecodeActionID_Malformed pins AC9: garbage strings return
// [ErrInvalidActionID]. Covers prefix mismatch, wrong arity, empty
// fields, and unknown tool names.
func TestDecodeActionID_Malformed(t *testing.T) {
	t.Parallel()

	cases := []string{
		"",
		"approval",
		"approval:propose_spawn",
		"foo:propose_spawn:tok",
		"approval::tok",
		"approval:propose_spawn:",
		"approval:unknown_tool:tok",
		"approval propose_spawn tok", // wrong separator
	}
	for _, c := range cases {
		_, _, err := cards.DecodeActionID(c)
		if !errors.Is(err, cards.ErrInvalidActionID) {
			t.Errorf("DecodeActionID(%q) err = %v, want ErrInvalidActionID", c, err)
		}
	}
}

// TestRenderProposeSpawn_HappyPath pins AC1: the propose_spawn card
// carries the expected fields + Approve/Reject buttons + correct
// action_id.
func TestRenderProposeSpawn_HappyPath(t *testing.T) {
	t.Parallel()

	in := cards.ProposeSpawnCardInput{
		AgentID:       "00000000-0000-4000-8000-000000000001",
		Personality:   "calm and methodical",
		Language:      "en",
		SystemPrompt:  "you are a helpful watchkeeper",
		ApprovalToken: "tok-ps-1",
	}
	blocks, actionID := cards.RenderProposeSpawn(in)
	if len(blocks) == 0 {
		t.Fatal("blocks empty")
	}
	if actionID != "approval:propose_spawn:tok-ps-1" {
		t.Errorf("actionID = %q, want %q", actionID, "approval:propose_spawn:tok-ps-1")
	}
	encoded := mustMarshal(t, blocks)
	wantSubstrings := []string{
		"propose_spawn",
		in.AgentID,
		"calm and methodical",
		"\"action_id\":\"approval:propose_spawn:tok-ps-1\"",
		"\"value\":\"approved\"",
		"\"value\":\"rejected\"",
		"\"style\":\"primary\"",
		"\"style\":\"danger\"",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(encoded, s) {
			t.Errorf("blocks JSON missing %q\n%s", s, encoded)
		}
	}
}

// TestRenderProposeSpawn_GuardEmptyInputs pins the renderer guard: a
// missing AgentID or ApprovalToken returns nil + empty action_id.
func TestRenderProposeSpawn_GuardEmptyInputs(t *testing.T) {
	t.Parallel()

	blocks, actionID := cards.RenderProposeSpawn(cards.ProposeSpawnCardInput{ApprovalToken: "tok"})
	if blocks != nil || actionID != "" {
		t.Errorf("missing AgentID guard failed: blocks=%v actionID=%q", blocks, actionID)
	}
	blocks, actionID = cards.RenderProposeSpawn(cards.ProposeSpawnCardInput{AgentID: "agent-1"})
	if blocks != nil || actionID != "" {
		t.Errorf("missing ApprovalToken guard failed: blocks=%v actionID=%q", blocks, actionID)
	}
}

// TestRenderAdjustPersonality_HappyAndDiff pins AC1 + AC3: the card
// carries the expected fields + Approve/Reject buttons + a diff block
// that includes both old and new personality on dedicated lines.
func TestRenderAdjustPersonality_HappyAndDiff(t *testing.T) {
	t.Parallel()

	in := cards.AdjustPersonalityCardInput{
		AgentID:        "agent-1",
		OldPersonality: "playful and fast",
		NewPersonality: "calm and steady",
		ApprovalToken:  "tok-ap-1",
	}
	blocks, actionID := cards.RenderAdjustPersonality(in)
	if actionID != "approval:adjust_personality:tok-ap-1" {
		t.Errorf("actionID = %q", actionID)
	}
	encoded := mustMarshal(t, blocks)
	wantDiff := []string{
		"- playful and fast",
		"+ calm and steady",
		"adjust_personality",
		"\"action_id\":\"approval:adjust_personality:tok-ap-1\"",
	}
	for _, s := range wantDiff {
		if !strings.Contains(encoded, s) {
			t.Errorf("missing %q\n%s", s, encoded)
		}
	}
}

// TestRenderAdjustLanguage_HappyAndDiff pins AC1 + AC3: the card
// carries the expected diff block.
func TestRenderAdjustLanguage_HappyAndDiff(t *testing.T) {
	t.Parallel()

	in := cards.AdjustLanguageCardInput{
		AgentID:       "agent-2",
		OldLanguage:   "en",
		NewLanguage:   "uk",
		ApprovalToken: "tok-al-1",
	}
	blocks, actionID := cards.RenderAdjustLanguage(in)
	if actionID != "approval:adjust_language:tok-al-1" {
		t.Errorf("actionID = %q", actionID)
	}
	encoded := mustMarshal(t, blocks)
	wantDiff := []string{
		"- en",
		"+ uk",
		"adjust_language",
		"\"action_id\":\"approval:adjust_language:tok-al-1\"",
	}
	for _, s := range wantDiff {
		if !strings.Contains(encoded, s) {
			t.Errorf("missing %q\n%s", s, encoded)
		}
	}
}

// TestRenderRetireWatchkeeper_HappyPath pins AC1: the retire card
// carries the expected fields + Approve/Reject buttons + correct
// action_id. No diff block (status-row mutation, not a manifest bump).
func TestRenderRetireWatchkeeper_HappyPath(t *testing.T) {
	t.Parallel()

	in := cards.RetireWatchkeeperCardInput{
		AgentID:       "agent-r-1",
		DisplayName:   "Watchkeeper Foo",
		ApprovalToken: "tok-r-1",
	}
	blocks, actionID := cards.RenderRetireWatchkeeper(in)
	if actionID != "approval:retire_watchkeeper:tok-r-1" {
		t.Errorf("actionID = %q", actionID)
	}
	encoded := mustMarshal(t, blocks)
	wantSubstrings := []string{
		"retire_watchkeeper",
		"agent-r-1",
		"Watchkeeper Foo",
		"\"action_id\":\"approval:retire_watchkeeper:tok-r-1\"",
		"\"value\":\"approved\"",
		"\"value\":\"rejected\"",
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(encoded, s) {
			t.Errorf("missing %q\n%s", s, encoded)
		}
	}
}

// TestRenderAdjustPersonality_DiffEmptyOldValue pins AC3 fallback:
// an empty old value is rendered as `(empty)` so the diff block
// remains 3-5 lines even on a fresh manifest.
func TestRenderAdjustPersonality_DiffEmptyOldValue(t *testing.T) {
	t.Parallel()

	in := cards.AdjustPersonalityCardInput{
		AgentID:        "agent-1",
		OldPersonality: "",
		NewPersonality: "calm and steady",
		ApprovalToken:  "tok-ap-2",
	}
	blocks, _ := cards.RenderAdjustPersonality(in)
	encoded := mustMarshal(t, blocks)
	if !strings.Contains(encoded, "- (empty)") {
		t.Errorf("expected `- (empty)` placeholder for old value\n%s", encoded)
	}
}

// mustMarshal helper to JSON-encode blocks for substring assertions.
// Returns the encoded string.
func mustMarshal(t *testing.T, blocks []cards.Block) string {
	t.Helper()
	out, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(out)
}
