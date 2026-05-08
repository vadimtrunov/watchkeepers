package intent_test

import (
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack/inbound/intent"
)

// TestParse_FixtureTable pins AC8 + the test plan "fixture table 15+
// phrasings". A single table entry per (input, expected intent[, role,
// team]) tuple. Adding a new phrasing here is the only step needed to
// extend the closed-set classifier.
func TestParse_FixtureTable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		input      string
		wantIntent intent.Intent
		wantRole   string
		wantTeam   string
	}{
		// ── propose_spawn (most specific) ───────────────────────
		{
			name:       "propose_a_coordinator_for_the_backend_team",
			input:      "propose a Coordinator for the backend team",
			wantIntent: intent.IntentProposeSpawn,
			wantRole:   "coordinator",
			wantTeam:   "backend",
		},
		{
			name:       "spawn_a_reviewer_for_the_growth_team",
			input:      "spawn a Reviewer for the growth team",
			wantIntent: intent.IntentProposeSpawn,
			wantRole:   "reviewer",
			wantTeam:   "growth",
		},
		{
			name:       "create_coordinator_on_the_data_team",
			input:      "create Coordinator on the data team",
			wantIntent: intent.IntentProposeSpawn,
			wantRole:   "coordinator",
			wantTeam:   "data",
		},
		{
			name:       "draft_a_reviewer_for_marketing_team",
			input:      "draft a Reviewer for marketing team",
			wantIntent: intent.IntentProposeSpawn,
			wantRole:   "reviewer",
			wantTeam:   "marketing",
		},
		// no-team-suffix fallback
		{
			name:       "propose_a_coordinator_for_backend",
			input:      "propose a Coordinator for backend",
			wantIntent: intent.IntentProposeSpawn,
			wantRole:   "coordinator",
			wantTeam:   "backend",
		},
		// ── list ──────────────────────────────────────────────────
		{
			name:       "whats_running",
			input:      "what's running?",
			wantIntent: intent.IntentReadList,
		},
		{
			name:       "list_watchkeepers",
			input:      "list watchkeepers",
			wantIntent: intent.IntentReadList,
		},
		{
			name:       "show_watchkeepers",
			input:      "show watchkeepers please",
			wantIntent: intent.IntentReadList,
		},
		{
			name:       "who_is_running",
			input:      "who is running right now",
			wantIntent: intent.IntentReadList,
		},
		// ── report_cost ───────────────────────────────────────────
		{
			name:       "show_costs",
			input:      "show costs",
			wantIntent: intent.IntentReportCost,
		},
		{
			name:       "how_much_did_we_spend",
			input:      "how much did we spend?",
			wantIntent: intent.IntentReportCost,
		},
		{
			name:       "billing_summary",
			input:      "give me the billing summary",
			wantIntent: intent.IntentReportCost,
		},
		// ── report_health ─────────────────────────────────────────
		{
			name:       "health_check",
			input:      "health check",
			wantIntent: intent.IntentReportHealth,
		},
		{
			name:       "are_we_alive",
			input:      "are we alive",
			wantIntent: intent.IntentReportHealth,
		},
		{
			name:       "whats_the_status",
			input:      "what's the status",
			wantIntent: intent.IntentReportHealth,
		},
		// ── unknown ───────────────────────────────────────────────
		{
			name:       "tell_me_a_joke",
			input:      "tell me a joke",
			wantIntent: intent.IntentUnknown,
		},
		{
			name:       "lunch_plans",
			input:      "what are we doing for lunch?",
			wantIntent: intent.IntentUnknown,
		},
		{
			name:       "empty_string",
			input:      "",
			wantIntent: intent.IntentUnknown,
		},
		{
			name:       "whitespace_only",
			input:      "   \t\n  ",
			wantIntent: intent.IntentUnknown,
		},
	}

	p := intent.NewParser()
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := p.Parse(tc.input)
			if got.Intent != tc.wantIntent {
				t.Errorf("Parse(%q).Intent = %q, want %q", tc.input, got.Intent, tc.wantIntent)
			}
			if got.Role != tc.wantRole {
				t.Errorf("Parse(%q).Role = %q, want %q", tc.input, got.Role, tc.wantRole)
			}
			if got.Team != tc.wantTeam {
				t.Errorf("Parse(%q).Team = %q, want %q", tc.input, got.Team, tc.wantTeam)
			}
		})
	}
}

// TestParse_Idempotent pins the determinism contract: same input ->
// same output across multiple invocations.
func TestParse_Idempotent(t *testing.T) {
	t.Parallel()

	p := intent.NewParser()
	inputs := []string{
		"propose a Coordinator for the backend team",
		"what's running?",
		"show costs",
		"health check",
		"tell me a joke",
		"",
	}
	for _, input := range inputs {
		first := p.Parse(input)
		for i := 0; i < 5; i++ {
			next := p.Parse(input)
			if next != first {
				t.Errorf("Parse(%q) drifted on iteration %d: %+v != %+v", input, i, next, first)
			}
		}
	}
}

// TestParse_PriorityProposeWinsOverReadOnly pins the resolution
// order: a propose phrasing wins even when the rest of the message
// trips a read-only keyword.
func TestParse_PriorityProposeWinsOverReadOnly(t *testing.T) {
	t.Parallel()

	p := intent.NewParser()
	got := p.Parse("propose a Coordinator for the growth team to track costs and health")
	if got.Intent != intent.IntentProposeSpawn {
		t.Errorf("Parse priority broken: got %q, want %q", got.Intent, intent.IntentProposeSpawn)
	}
}
