package manifest

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// coordinatorSeedFixture mirrors the manifest_version row written by
// `deploy/migrations/024_coordinator_manifest_seed.sql`. It is the
// authoritative Go-side projection of the SQL seed; a drift between
// the two surfaces is caught by
// [TestCoordinatorSeed_MigrationContainsExpectedShape] (which reads
// the migration off disk and matches the canonical substrings against
// this fixture) plus the test below that pipes the fixture through
// [LoadManifest] and asserts the AC contract.
//
// The reassignment-boundary phrase, the lead-deferral phrase, the
// `update_ticket_field: self` authority entry, the `autonomous`
// autonomy value, and the single-tool toolset placeholder are all
// load-bearing — see the M8.2.a sub-item description in
// `docs/ROADMAP-phase1.md` and the `core/pkg/runtime/authority.go`
// vocabulary reference.
func coordinatorSeedFixture() *keepclient.ManifestVersion {
	const systemPrompt = "You are the Coordinator, a real-work agent that reads tickets," +
		" tracks reviewer attention, drafts daily briefings, and posts" +
		" comments + whitelisted field updates on behalf of the lead." +
		"\n\nIdentity: you are deferential to the lead human. You" +
		" propose, the lead approves anything beyond your authority" +
		" matrix. You operate under autonomous autonomy: the runtime" +
		" consults your authority matrix per action; entries valued" +
		` "self" execute without approval, entries valued "lead" or` +
		` "operator" require out-of-band approval before the runtime` +
		" dispatches the call." +
		"\n\nReassignment boundary: you NEVER reassign tickets. The" +
		" update_ticket_field tool refuses any args containing the" +
		" `assignee` key as a hard refusal. The deployment may" +
		" additionally configure the M8.1 jira adapter's field" +
		" whitelist to exclude `assignee`; the handler refusal is the" +
		" authoritative boundary in any case. If a path appears to" +
		" require ticket reassignment, ALWAYS surface a question to" +
		" the lead, not an action. Bypassing this boundary is a hard" +
		" violation." +
		"\n\nAudit discipline: every tool call you make lands in the" +
		" Keeper's Log via the runtime's tool-result reflection" +
		" layer. Treat tool calls as governed actions, not runtime" +
		" state — a comment posted in error is a permanent audit" +
		" artefact." +
		"\n\nPII discipline: NEVER include API tokens, OAuth bot" +
		" tokens, Slack workspace credentials, or Jira basic-auth" +
		" values in any tool argument, comment body, briefing payload," +
		" or response. Token redaction is a one-way trip; surface" +
		" would-be leaks as a question to the lead."

	const personality = "Tactical project coordinator: deferential on assignment / scope " +
		"changes, decisive on routine updates and reminders. Optimises " +
		"for clear comms and audit traceability over speed; prefers a " +
		"short clarifying question over a wrong action."

	const authorityMatrix = `{
		"update_ticket_field": "self",
		"manifest_version_bump": "lead"
	}`

	return &keepclient.ManifestVersion{
		ID:                         CoordinatorManifestVersionID,
		ManifestID:                 CoordinatorManifestID,
		VersionNo:                  1,
		SystemPrompt:               systemPrompt,
		Tools:                      json.RawMessage(`[{"name": "update_ticket_field"}]`),
		AuthorityMatrix:            json.RawMessage(authorityMatrix),
		KnowledgeSources:           json.RawMessage(`[]`),
		Personality:                personality,
		Language:                   "en",
		Model:                      "claude-sonnet-4-6",
		Autonomy:                   "autonomous",
		NotebookTopK:               5,
		NotebookRelevanceThreshold: 0.3,
	}
}

// TestCoordinatorSeed_LoadsViaLoadManifest is the happy-path proof:
// pipe the fixture mirroring `024_coordinator_manifest_seed.sql`
// through [LoadManifest] and assert the projected fields the M8.2.a
// sub-item calls out — SystemPrompt contains the reassignment-
// boundary and lead-deferral phrases, Toolset carries the single
// `update_ticket_field` entry, AuthorityMatrix["update_ticket_field"]
// equals "self", and Autonomy is `autonomous`.
//
// The fixture path mirrors the existing `loader_test.go` fakeFetcher
// pattern rather than spinning up a real test DB. Real-DB exercising
// of the seed is covered by the `keep-integration-ci` pipeline in
// `.github/workflows/ci.yml`.
func TestCoordinatorSeed_LoadsViaLoadManifest(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: coordinatorSeedFixture()}

	got, err := LoadManifest(context.Background(), f, CoordinatorManifestID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	const wantReassignmentPhrase = "NEVER reassign tickets"
	if !strings.Contains(got.SystemPrompt, wantReassignmentPhrase) {
		t.Errorf("SystemPrompt missing reassignment-boundary phrase %q; got:\n%s",
			wantReassignmentPhrase, got.SystemPrompt)
	}

	const wantLeadDeferralPhrase = "ALWAYS surface a"
	if !strings.Contains(got.SystemPrompt, wantLeadDeferralPhrase) {
		t.Errorf("SystemPrompt missing lead-deferral phrase %q; got:\n%s",
			wantLeadDeferralPhrase, got.SystemPrompt)
	}

	if names := got.Toolset.Names(); len(names) != 1 || names[0] != "update_ticket_field" {
		t.Errorf("Toolset = %v, want [update_ticket_field]", names)
	}

	if got, want := got.AuthorityMatrix["update_ticket_field"], "self"; got != want {
		t.Errorf("AuthorityMatrix[update_ticket_field] = %q, want %q", got, want)
	}

	if got.Autonomy != agentruntime.AutonomyAutonomous {
		t.Errorf("Autonomy = %q, want %q (seed value)", got.Autonomy, agentruntime.AutonomyAutonomous)
	}
}

// TestCoordinatorSeed_MigrationContainsExpectedShape is the binding
// cross-link between the Go fixture and the SQL seed: read
// `deploy/migrations/024_coordinator_manifest_seed.sql` off disk and
// assert the load-bearing literals are all present verbatim. A
// reword of the system prompt or a drift in the authority matrix
// fails this test, not production.
//
// This test is the closest equivalent in-process to "load the
// manifest from a fresh DB carrying the migration" without standing
// up a real Postgres — the migration text IS the source of truth for
// the seed shape, and string-matching the load-bearing pieces is
// sufficient to lock the contract.
func TestCoordinatorSeed_MigrationContainsExpectedShape(t *testing.T) {
	t.Parallel()

	migrationPath := repoRelative(t, "deploy/migrations/024_coordinator_manifest_seed.sql")
	bytes, err := os.ReadFile(migrationPath) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", migrationPath, err)
	}
	body := string(bytes)

	wantLiterals := []string{
		// Stable ids.
		CoordinatorManifestID,
		CoordinatorManifestVersionID,
		// Reused system tenant from migration 017.
		WatchmasterSystemOrganizationID,
		// Role-boundary phrase (reassignment).
		"NEVER reassign tickets",
		// Lead-deferral phrase.
		"ALWAYS surface a",
		// Authority matrix entries (verbatim with quotes — runtime
		// vocab `"self"` / `"lead"`, distinct from the M5.5.b.c.c.b
		// `"lead_approval"` / `"allowed"` enum used by the
		// Watchmaster seed).
		`"update_ticket_field": "self"`,
		`"manifest_version_bump": "lead"`,
		// Toolset placeholder — single tool for M8.2.a.
		`"update_ticket_field"`,
		// Model + autonomy.
		"claude-sonnet-4-6",
		"'autonomous'",
		// Idempotency clause.
		"ON CONFLICT (id) DO NOTHING",
	}
	for _, lit := range wantLiterals {
		if !strings.Contains(body, lit) {
			t.Errorf("migration missing expected literal %q", lit)
		}
	}
}

// TestCoordinatorSeed_AuthorityMatrixUsesRuntimeVocabulary asserts a
// regression guard: the seeded authority matrix uses the
// `core/pkg/runtime/authority.go` vocabulary (`"self"` / `"lead"` /
// `"operator"` / `"watchmaster"`), NOT the M5.5.b.c.c.b enum
// (`"allowed"` / `"lead_approval"` / `"forbidden"`) the Watchmaster
// seed (migration 017) carries.
//
// The two vocabularies are different: the Watchmaster's autonomy is
// `supervised` so the runtime gate short-circuits to "every action
// requires approval" regardless of matrix content (the M5.5.b.c.c.b
// values are dormant on that surface). The Coordinator's autonomy is
// `autonomous`, so the runtime gate consults the matrix on every
// tool call — only runtime-vocabulary values produce the intended
// gate semantics. A drift to `"allowed"` / `"lead_approval"` here
// would silently fail closed (per
// `core/pkg/runtime/authority.go::RequiresApproval` default branch),
// blocking every Coordinator tool call. Pin the boundary.
func TestCoordinatorSeed_AuthorityMatrixUsesRuntimeVocabulary(t *testing.T) {
	t.Parallel()

	allowed := map[string]bool{
		"self":        true,
		"lead":        true,
		"operator":    true,
		"watchmaster": true,
	}

	f := &fakeFetcher{response: coordinatorSeedFixture()}
	got, err := LoadManifest(context.Background(), f, CoordinatorManifestID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	for action, value := range got.AuthorityMatrix {
		if !allowed[value] {
			t.Errorf("AuthorityMatrix[%q] = %q, want one of {self, lead, operator, watchmaster} "+
				"(runtime vocabulary, not the M5.5.b.c.c.b enum)", action, value)
		}
	}
}
