package cards_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/cards"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// TestRenderPromoteToKeep_FixtureTable pins AC1, AC3, AC4 (and the
// scope-routing branches from the test plan) via a single fixture
// table so each new branch lands as one row, not a new function.
//
// Each row asserts a deterministic set of substrings the rendered
// blocks must contain (and, optionally, must NOT contain). The
// `wantBlocks` count pins AC3 ordering: the renderer emits exactly
// 5 blocks for narrower scopes (header / body / preview / actions /
// context) and 6 blocks for `org` scope (the scope warning slots in
// at index 2).
func TestRenderPromoteToKeep_FixtureTable(t *testing.T) {
	t.Parallel()

	const (
		agentID    = "00000000-0000-4000-8000-000000000001"
		entryID    = "00000000-0000-7000-8000-000000000abc"
		approvalTk = "tok-ptk-xyz" //nolint:gosec // G101 false positive: deterministic test fixture
		category   = "decision"
	)

	longSubject := strings.Repeat("s", 200)       // > 150 → truncated
	longContent := strings.Repeat("c", 600)       // > 500 → truncated
	multiLine := "line one\nline two\nline three" // newline preservation

	cases := []struct {
		name           string
		in             cards.PromoteToKeepCardInput
		wantNil        bool
		wantBlocks     int
		wantActionID   string
		wantSubstrings []string
		notSubstrings  []string
	}{
		{
			name: "happy_org_scope",
			in: cards.PromoteToKeepCardInput{
				AgentID:         agentID,
				NotebookEntryID: entryID,
				Subject:         "incident response checklist",
				Content:         "always page the on-call before mutating prod",
				Category:        category,
				Scope:           notebook.ScopeOrg,
				ApprovalToken:   approvalTk,
			},
			wantBlocks:   6,
			wantActionID: "approval:promote_to_keep:" + approvalTk,
			wantSubstrings: []string{
				"Approve promote-to-keep?",
				"promote_to_keep",
				agentID,
				entryID,
				category,
				notebook.ScopeOrg,
				"incident response checklist",
				"always page the on-call",
				"will become visible to all Watchkeepers",
				"\"action_id\":\"approval:promote_to_keep:" + approvalTk + "\"",
				"\"value\":\"approved\"",
				"\"value\":\"rejected\"",
				"tok-tok-pt…",
			},
		},
		{
			name: "agent_scope_no_warning",
			in: cards.PromoteToKeepCardInput{
				AgentID:         agentID,
				NotebookEntryID: entryID,
				Subject:         "agent-scoped tip",
				Content:         "remember the prod runbook lives in /ops",
				Category:        category,
				Scope:           notebook.ScopeAgentPrefix + agentID,
				ApprovalToken:   approvalTk,
			},
			wantBlocks:   5,
			wantActionID: "approval:promote_to_keep:" + approvalTk,
			wantSubstrings: []string{
				notebook.ScopeAgentPrefix + agentID,
				"Scope: `" + notebook.ScopeAgentPrefix + agentID + "`",
			},
			notSubstrings: []string{
				"will become visible to all Watchkeepers",
			},
		},
		{
			name: "user_scope_no_warning",
			in: cards.PromoteToKeepCardInput{
				AgentID:         agentID,
				NotebookEntryID: entryID,
				Subject:         "user-scoped tip",
				Content:         "user prefers metric units",
				Category:        category,
				Scope:           notebook.ScopeUserPrefix + agentID,
				ApprovalToken:   approvalTk,
			},
			wantBlocks:   5,
			wantActionID: "approval:promote_to_keep:" + approvalTk,
			wantSubstrings: []string{
				notebook.ScopeUserPrefix + agentID,
				"Scope: `" + notebook.ScopeUserPrefix + agentID + "`",
			},
			notSubstrings: []string{
				"will become visible to all Watchkeepers",
			},
		},
		{
			name: "long_subject_truncated",
			in: cards.PromoteToKeepCardInput{
				AgentID:         agentID,
				NotebookEntryID: entryID,
				Subject:         longSubject,
				Content:         "short content body",
				Category:        category,
				Scope:           notebook.ScopeOrg,
				ApprovalToken:   approvalTk,
			},
			wantBlocks:   6,
			wantActionID: "approval:promote_to_keep:" + approvalTk,
			wantSubstrings: []string{
				strings.Repeat("s", 150) + "…",
			},
			notSubstrings: []string{
				strings.Repeat("s", 200), // raw 200-rune body must not survive
			},
		},
		{
			name: "long_content_truncated",
			in: cards.PromoteToKeepCardInput{
				AgentID:         agentID,
				NotebookEntryID: entryID,
				Subject:         "ok subject",
				Content:         longContent,
				Category:        category,
				Scope:           notebook.ScopeOrg,
				ApprovalToken:   approvalTk,
			},
			wantBlocks:   6,
			wantActionID: "approval:promote_to_keep:" + approvalTk,
			wantSubstrings: []string{
				strings.Repeat("c", 500) + "…",
			},
			notSubstrings: []string{
				strings.Repeat("c", 600),
			},
		},
		{
			name: "multiline_content_preserved",
			in: cards.PromoteToKeepCardInput{
				AgentID:         agentID,
				NotebookEntryID: entryID,
				Subject:         "multi line",
				Content:         multiLine,
				Category:        category,
				Scope:           notebook.ScopeOrg,
				ApprovalToken:   approvalTk,
			},
			wantBlocks:   6,
			wantActionID: "approval:promote_to_keep:" + approvalTk,
			wantSubstrings: []string{
				// JSON-encoded newlines surface as `\n` literals.
				`line one\nline two\nline three`,
			},
		},
		{
			name: "empty_subject_renders_placeholder",
			in: cards.PromoteToKeepCardInput{
				AgentID:         agentID,
				NotebookEntryID: entryID,
				Subject:         "",
				Content:         "body without subject",
				Category:        category,
				Scope:           notebook.ScopeOrg,
				ApprovalToken:   approvalTk,
			},
			wantBlocks:   6,
			wantActionID: "approval:promote_to_keep:" + approvalTk,
			wantSubstrings: []string{
				"_(no subject)_",
			},
		},
		{
			name: "empty_content_returns_nil",
			in: cards.PromoteToKeepCardInput{
				AgentID:         agentID,
				NotebookEntryID: entryID,
				Subject:         "subject only",
				Content:         "",
				Category:        category,
				Scope:           notebook.ScopeOrg,
				ApprovalToken:   approvalTk,
			},
			wantNil: true,
		},
		{
			name: "empty_agent_id_returns_nil",
			in: cards.PromoteToKeepCardInput{
				AgentID:         "",
				NotebookEntryID: entryID,
				Content:         "body",
				ApprovalToken:   approvalTk,
			},
			wantNil: true,
		},
		{
			name: "empty_approval_token_returns_nil",
			in: cards.PromoteToKeepCardInput{
				AgentID:         agentID,
				NotebookEntryID: entryID,
				Content:         "body",
				ApprovalToken:   "",
			},
			wantNil: true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			blocks, actionID := cards.RenderPromoteToKeep(tc.in)

			if tc.wantNil {
				if blocks != nil || actionID != "" {
					t.Fatalf("guard failed: blocks=%v actionID=%q", blocks, actionID)
				}
				return
			}
			if actionID != tc.wantActionID {
				t.Errorf("actionID = %q, want %q", actionID, tc.wantActionID)
			}
			if len(blocks) != tc.wantBlocks {
				t.Errorf("len(blocks) = %d, want %d", len(blocks), tc.wantBlocks)
			}

			encoded := mustMarshal(t, blocks)
			for _, s := range tc.wantSubstrings {
				if !strings.Contains(encoded, s) {
					t.Errorf("blocks JSON missing %q\n%s", s, encoded)
				}
			}
			for _, s := range tc.notSubstrings {
				if strings.Contains(encoded, s) {
					t.Errorf("blocks JSON unexpectedly contains %q\n%s", s, encoded)
				}
			}
		})
	}
}

// TestRenderPromoteToKeep_BlockOrder pins AC3: the org-scope render
// emits blocks in the exact ordered sequence the AC mandates
// (header → body → scope-warning → preview → actions → context). The
// fixture-table assertion above checks counts + content; this test
// pins the per-index `Type` to catch a future re-shuffle.
func TestRenderPromoteToKeep_BlockOrder(t *testing.T) {
	t.Parallel()

	in := cards.PromoteToKeepCardInput{
		AgentID:         "agent-1",
		NotebookEntryID: "entry-1",
		Subject:         "s",
		Content:         "c",
		Category:        "decision",
		Scope:           notebook.ScopeOrg,
		ApprovalToken:   "tok-1",
	}
	blocks, _ := cards.RenderPromoteToKeep(in)
	wantTypes := []string{"header", "section", "section", "section", "actions", "context"}
	gotTypes := make([]string, 0, len(blocks))
	for _, b := range blocks {
		gotTypes = append(gotTypes, b.Type)
	}
	if !reflect.DeepEqual(gotTypes, wantTypes) {
		t.Errorf("block types = %v, want %v", gotTypes, wantTypes)
	}
}

// TestRenderPromoteToKeep_ActionIDRoundTrip pins AC7: the new
// `promote_to_keep` tool name round-trips cleanly through
// EncodeActionID → DecodeActionID. The shared
// TestEncodeDecodeActionID_RoundTrip in cards_test.go does NOT cover
// promote_to_keep (closed-set list lives at the spawn package); this
// test extends coverage without touching the shared fixture.
func TestRenderPromoteToKeep_ActionIDRoundTrip(t *testing.T) {
	t.Parallel()

	const tool = spawn.PendingApprovalToolPromoteToKeep
	const approvalTk = "tok-ptk-roundtrip" //nolint:gosec // G101 false positive: deterministic test fixture

	encoded := cards.EncodeActionID(tool, approvalTk)
	if encoded == "" {
		t.Fatal("EncodeActionID returned empty")
	}
	if encoded != "approval:promote_to_keep:"+approvalTk {
		t.Errorf("encoded = %q, want %q", encoded, "approval:promote_to_keep:"+approvalTk)
	}
	gotTool, gotToken, err := cards.DecodeActionID(encoded)
	if err != nil {
		t.Fatalf("DecodeActionID: %v", err)
	}
	if gotTool != tool {
		t.Errorf("tool = %q, want %q", gotTool, tool)
	}
	if gotToken != approvalTk {
		t.Errorf("token = %q, want %q", gotToken, approvalTk)
	}
}

// TestRenderPromoteToKeep_PIIGuard pins AC6: neither the full
// approval token (only the truncated `tok-<6>…` prefix is allowed
// outside the action_id attribute) nor any embedding bytes appear in
// the rendered blocks.
//
// Embedding bytes are excluded BY CONSTRUCTION — the input struct has
// no `Embedding` field, so this test pins the structural absence via
// reflection so a future field addition surfaces as a test failure
// here, not as a silent PII leak.
func TestRenderPromoteToKeep_PIIGuard(t *testing.T) {
	t.Parallel()

	// Structural guarantee: no `Embedding` field on the input struct.
	// A future addition would force this assertion to fail.
	typ := reflect.TypeOf(cards.PromoteToKeepCardInput{})
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		if strings.Contains(strings.ToLower(name), "embedding") {
			t.Errorf("PromoteToKeepCardInput must not carry embedding bytes; got field %q", name)
		}
	}

	const approvalTk = "tok-ptk-secret-bytes-do-not-leak" //nolint:gosec // G101 false positive: deterministic test fixture
	in := cards.PromoteToKeepCardInput{
		AgentID:         "agent-1",
		NotebookEntryID: "entry-1",
		Subject:         "subj",
		Content:         "body",
		Category:        "decision",
		Scope:           notebook.ScopeOrg,
		ApprovalToken:   approvalTk,
	}
	blocks, actionID := cards.RenderPromoteToKeep(in)
	encoded := mustMarshal(t, blocks)

	// The full token is allowed in the action_id payload (the
	// dispatcher needs it to look up the pending_approvals row); it
	// must not appear ANYWHERE else in the blocks JSON.
	withoutActionID := strings.ReplaceAll(encoded, actionID, "")
	if strings.Contains(withoutActionID, approvalTk) {
		t.Errorf("full approval token leaked outside action_id\n%s", encoded)
	}
}
