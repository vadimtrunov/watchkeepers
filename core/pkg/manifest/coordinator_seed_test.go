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

// coordinatorSeedV2Fixture mirrors the manifest_version row written by
// `deploy/migrations/025_coordinator_manifest_v2_seed.sql` (M8.2.b).
// Supersedes [coordinatorSeedFixture] because keepclient.GetManifest
// returns the manifest_version row with the highest version_no for a
// given manifest_id, so production loads V2 once migration 025 has
// run.
//
// Shape diff vs V1: extends `Tools` with `find_overdue_tickets`,
// extends `AuthorityMatrix` with `find_overdue_tickets=self`, and
// appends a narrative paragraph to `SystemPrompt` documenting the
// new tool (iter-1 critic minor: V1's prompt did not mention it; the
// agent had no guidance on when to invoke). Personality, model,
// autonomy, and notebook recall tunables are unchanged from V1 — the
// role definition and reassignment boundary still hold.
func coordinatorSeedV2Fixture() *keepclient.ManifestVersion {
	v1 := coordinatorSeedFixture()
	const v2AuthorityMatrix = `{
		"update_ticket_field": "self",
		"find_overdue_tickets": "self",
		"manifest_version_bump": "lead"
	}`
	const v2PromptAppendix = "\n\nReading tools: use find_overdue_tickets to surface" +
		" tickets assigned to a teammate that have not been updated" +
		" recently. Required args: project_key (Atlassian project)," +
		" assignee_account_id (Atlassian Cloud accountId), status" +
		" (array of workflow status names to include)," +
		" age_threshold_days (integer 1..365). The handler caps the" +
		" result at 200 issues across 10 pages and returns" +
		" truncated=true when the cap fires; narrow the scope or" +
		" lower the threshold and re-run on truncation."
	v1.ID = CoordinatorManifestVersionV2ID
	v1.VersionNo = 2
	v1.SystemPrompt += v2PromptAppendix
	v1.Tools = json.RawMessage(`[
		{"name": "update_ticket_field"},
		{"name": "find_overdue_tickets"}
	]`)
	v1.AuthorityMatrix = json.RawMessage(v2AuthorityMatrix)
	return v1
}

// coordinatorSeedV3Fixture mirrors the manifest_version row written by
// `deploy/migrations/026_coordinator_manifest_v3_seed.sql` (M8.2.c).
// Supersedes [coordinatorSeedV2Fixture] for production boot.
//
// Shape diff vs V2: extends `Tools` with `fetch_watch_orders`,
// `nudge_reviewer`, `post_daily_briefing`; extends `AuthorityMatrix`
// granting `self` on each; appends three narrative paragraphs to
// `SystemPrompt` documenting the new tools. Personality, model,
// autonomy, and notebook recall tunables are unchanged from V2.
func coordinatorSeedV3Fixture() *keepclient.ManifestVersion {
	v2 := coordinatorSeedV2Fixture()
	const v3AuthorityMatrix = `{
		"update_ticket_field": "self",
		"find_overdue_tickets": "self",
		"fetch_watch_orders": "self",
		"nudge_reviewer": "self",
		"post_daily_briefing": "self",
		"manifest_version_bump": "lead"
	}`
	const v3PromptAppendix = "\n\nSlack inbox: use fetch_watch_orders to read recent" +
		" direct messages from the lead. Required args: lead_user_id" +
		" (Slack user id matching [UWB][A-Z0-9]+), lookback_minutes" +
		" (integer 1..1440). The handler resolves the 1:1 IM channel" +
		" via conversations.open and reads the history; caps at 200" +
		" messages across 10 pages. Use this when the lead has" +
		" likely DM'd new orders since your last turn." +
		"\n\nReviewer nudges: use nudge_reviewer to DM a teammate" +
		" about a stale review. Required args: reviewer_user_id" +
		" (Slack user id), text (≤2000 chars). Optional: title (≤200" +
		" chars). Slack auto-opens the DM; you do NOT need to resolve" +
		" the channel id first. Compose the nudge as a SINGLE," +
		" actionable message — link to the PR + the asked action." +
		" Avoid daily spam: nudge at most once per reviewer per PR" +
		" per 24h window." +
		"\n\nDaily briefing: use post_daily_briefing to post a" +
		" structured daily summary to a configured channel." +
		" Required args: channel_id (Slack C…/G…/D… channel)," +
		" title (≤200 chars, non-empty), sections (array of" +
		" {heading, bullets}, ≤20 sections, each ≤20 bullets ≤1000" +
		" chars). The handler caps the rendered text at 8000 chars" +
		" and refuses overflow; trim sections when the limit fires."
	v2.ID = CoordinatorManifestVersionV3ID
	v2.VersionNo = 3
	v2.SystemPrompt += v3PromptAppendix
	v2.Tools = json.RawMessage(`[
		{"name": "update_ticket_field"},
		{"name": "find_overdue_tickets"},
		{"name": "fetch_watch_orders"},
		{"name": "nudge_reviewer"},
		{"name": "post_daily_briefing"}
	]`)
	v2.AuthorityMatrix = json.RawMessage(v3AuthorityMatrix)
	return v2
}

// coordinatorSeedV4Fixture mirrors the manifest_version row written by
// `deploy/migrations/027_coordinator_manifest_v4_seed.sql` (M8.2.d).
// Supersedes [coordinatorSeedV3Fixture] for production boot.
//
// Shape diff vs V3: extends `Tools` with `find_stale_prs`; extends
// `AuthorityMatrix` granting `self` on it; appends a narrative
// paragraph to `SystemPrompt` documenting the new tool. Personality,
// model, autonomy, and notebook recall tunables are unchanged from V3.
func coordinatorSeedV4Fixture() *keepclient.ManifestVersion {
	v3 := coordinatorSeedV3Fixture()
	const v4AuthorityMatrix = `{
		"update_ticket_field": "self",
		"find_overdue_tickets": "self",
		"fetch_watch_orders": "self",
		"nudge_reviewer": "self",
		"post_daily_briefing": "self",
		"find_stale_prs": "self",
		"manifest_version_bump": "lead"
	}`
	const v4PromptAppendix = "\n\nGitHub PRs: use find_stale_prs to surface pull requests" +
		" awaiting a teammate's review for too long. Required args:" +
		" repo_owner (GitHub user/org login), repo_name (repository" +
		" name), reviewer_login (GitHub login of the reviewer to" +
		" filter by), age_threshold_days (integer 1..365). The" +
		" handler scans open PRs in the repo, filters to those where" +
		" the supplied reviewer is in the requested-reviewers list AND" +
		" the PR has not been updated in more than the threshold; caps" +
		" at 200 PRs across 10 pages. Reviewer login matching is" +
		" case-insensitive. Use this when composing the daily briefing" +
		" or deciding which reviewer to nudge."
	v3.ID = CoordinatorManifestVersionV4ID
	v3.VersionNo = 4
	v3.SystemPrompt += v4PromptAppendix
	v3.Tools = json.RawMessage(`[
		{"name": "update_ticket_field"},
		{"name": "find_overdue_tickets"},
		{"name": "fetch_watch_orders"},
		{"name": "nudge_reviewer"},
		{"name": "post_daily_briefing"},
		{"name": "find_stale_prs"}
	]`)
	v3.AuthorityMatrix = json.RawMessage(v4AuthorityMatrix)
	return v3
}

// TestCoordinatorSeed_LoadsViaLoadManifest is the happy-path proof:
// pipe the V4 fixture mirroring `027_coordinator_manifest_v4_seed.sql`
// through [LoadManifest] and assert the projected fields the M8.2.d
// sub-item calls out — SystemPrompt still contains the reassignment-
// boundary and lead-deferral phrases (unchanged from V1/V2/V3),
// Toolset carries the V1+V2+V3+V4 tool names, AuthorityMatrix grants
// `self` on each, and Autonomy is `autonomous`.
//
// V4 is the right baseline for "what production loads" because
// keepclient.GetManifest returns the highest-version_no row
// (`core/pkg/keepclient/read_manifest.go:63-67`). The V1/V2/V3
// baselines are independently asserted by the corresponding
// migration-shape tests reading migrations 024/025/026 off disk.
//
// The fixture path mirrors the existing `loader_test.go` fakeFetcher
// pattern rather than spinning up a real test DB. Real-DB exercising
// of the seed is covered by the `keep-integration-ci` pipeline in
// `.github/workflows/ci.yml`.
func TestCoordinatorSeed_LoadsViaLoadManifest(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: coordinatorSeedV4Fixture()}

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

	// V2 appendix: narrative guidance for `find_overdue_tickets`
	// (preserved verbatim in V3).
	const wantFindOverduePhrase = "find_overdue_tickets to surface"
	if !strings.Contains(got.SystemPrompt, wantFindOverduePhrase) {
		t.Errorf("SystemPrompt missing V2 read-tool guidance %q; got:\n%s",
			wantFindOverduePhrase, got.SystemPrompt)
	}

	// V3 appendix: narrative guidance for the three Slack tools.
	for _, phrase := range []string{
		"fetch_watch_orders to read",
		"nudge_reviewer to DM",
		"post_daily_briefing to post",
	} {
		if !strings.Contains(got.SystemPrompt, phrase) {
			t.Errorf("SystemPrompt missing V3 slack-tool guidance %q; got:\n%s",
				phrase, got.SystemPrompt)
		}
	}

	// V4 appendix: narrative guidance for the GitHub stale-PRs tool.
	const wantFindStalePRsPhrase = "find_stale_prs to surface"
	if !strings.Contains(got.SystemPrompt, wantFindStalePRsPhrase) {
		t.Errorf("SystemPrompt missing V4 github-tool guidance %q; got:\n%s",
			wantFindStalePRsPhrase, got.SystemPrompt)
	}

	wantNames := map[string]bool{
		"update_ticket_field":  true,
		"find_overdue_tickets": true,
		"fetch_watch_orders":   true,
		"nudge_reviewer":       true,
		"post_daily_briefing":  true,
		"find_stale_prs":       true,
	}
	names := got.Toolset.Names()
	if len(names) != len(wantNames) {
		t.Errorf("Toolset = %v, want %v", names, wantNames)
	}
	for _, n := range names {
		if !wantNames[n] {
			t.Errorf("Toolset contains unexpected entry %q; want one of %v", n, wantNames)
		}
	}

	for _, action := range []string{
		"update_ticket_field",
		"find_overdue_tickets",
		"fetch_watch_orders",
		"nudge_reviewer",
		"post_daily_briefing",
		"find_stale_prs",
	} {
		if got, want := got.AuthorityMatrix[action], "self"; got != want {
			t.Errorf("AuthorityMatrix[%s] = %q, want %q", action, got, want)
		}
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

	// Pin the boundary on the V4 fixture (the one production loads
	// after migration 027 ships) so any future add that drifts to
	// `lead_approval` fails this test, not production.
	f := &fakeFetcher{response: coordinatorSeedV4Fixture()}
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

// TestCoordinatorSeedV4_MigrationContainsExpectedShape is the V4
// equivalent of the V1/V2/V3 migration-shape tests: reads
// `deploy/migrations/027_coordinator_manifest_v4_seed.sql` off disk
// and asserts the load-bearing literals (V4 manifest_version id,
// version_no=4, the toolset extension with `find_stale_prs`, the new
// authority-matrix entry granting `self`, the unchanged system-prompt
// phrases, the V4 narrative-guidance phrase, idempotency clause).
// Drift fails CI, not production.
//
// Migrations 024 / 025 / 026 / 027 are pinned by separate
// migration-shape tests; the four tests together pin the M8.2.a/b/c/d
// progression.
func TestCoordinatorSeedV4_MigrationContainsExpectedShape(t *testing.T) {
	t.Parallel()

	migrationPath := repoRelative(t, "deploy/migrations/027_coordinator_manifest_v4_seed.sql")
	bytes, err := os.ReadFile(migrationPath) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", migrationPath, err)
	}
	body := string(bytes)

	wantLiterals := []string{
		// Stable ids — V4 manifest_version row + REUSED manifest id.
		CoordinatorManifestVersionV4ID,
		CoordinatorManifestID,
		// Reused system tenant.
		WatchmasterSystemOrganizationID,
		// Version progression — V4 lands at version_no=4.
		"  4,",
		// System-prompt phrases (V1/V2/V3 phrases unchanged; V4
		// preserves them).
		"NEVER reassign tickets",
		"ALWAYS surface a",
		"find_overdue_tickets to surface",
		"fetch_watch_orders to read",
		"nudge_reviewer to DM",
		"post_daily_briefing to post",
		// V4 narrative-guidance appendix — pinned so a future reword
		// that drops the phrase fails CI loudly.
		"find_stale_prs to surface",
		// Toolset extension: V1/V2/V3 entries preserved, V4 entry added.
		`"update_ticket_field"`,
		`"find_overdue_tickets"`,
		`"fetch_watch_orders"`,
		`"nudge_reviewer"`,
		`"post_daily_briefing"`,
		`"find_stale_prs"`,
		// Authority matrix entries — runtime vocab; M8.2.d grants
		// `self` on the new tool.
		`"update_ticket_field": "self"`,
		`"find_overdue_tickets": "self"`,
		`"fetch_watch_orders": "self"`,
		`"nudge_reviewer": "self"`,
		`"post_daily_briefing": "self"`,
		`"find_stale_prs": "self"`,
		`"manifest_version_bump": "lead"`,
		// Model + autonomy unchanged from V1.
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

// TestCoordinatorSeedV3_MigrationContainsExpectedShape is the V3
// equivalent of the V1/V2 migration-shape tests: reads
// `deploy/migrations/026_coordinator_manifest_v3_seed.sql` off disk
// and asserts the load-bearing literals (V3 manifest_version id,
// version_no=3, the toolset extension with the three new Slack tools,
// the new authority-matrix entries granting `self` on each, the
// unchanged system-prompt phrases, the V3 narrative-guidance phrases,
// idempotency clause). Drift fails CI, not production.
//
// Migration 024's and 025's shapes are independently asserted by their
// corresponding tests; the three tests together pin the M8.2.a/b/c
// progression. The M8.2.d sub-item will add a fourth migration-shape
// test under this same pattern.
func TestCoordinatorSeedV3_MigrationContainsExpectedShape(t *testing.T) {
	t.Parallel()

	migrationPath := repoRelative(t, "deploy/migrations/026_coordinator_manifest_v3_seed.sql")
	bytes, err := os.ReadFile(migrationPath) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", migrationPath, err)
	}
	body := string(bytes)

	wantLiterals := []string{
		// Stable ids — V3 manifest_version row + REUSED manifest id.
		CoordinatorManifestVersionV3ID,
		CoordinatorManifestID,
		// Reused system tenant (referenced via the set_config preamble).
		WatchmasterSystemOrganizationID,
		// Version progression — V3 lands at version_no=3.
		"  3,",
		// System-prompt phrases (V1/V2 phrases unchanged; V3 preserves
		// them).
		"NEVER reassign tickets",
		"ALWAYS surface a",
		"find_overdue_tickets to surface",
		// V3 narrative-guidance appendix for the three new Slack
		// tools — pinned per-phrase so a future reword that drops
		// one fails CI loudly.
		"fetch_watch_orders to read",
		"nudge_reviewer to DM",
		"post_daily_briefing to post",
		// Toolset extension: V1/V2 entries preserved, V3 entries added.
		`"update_ticket_field"`,
		`"find_overdue_tickets"`,
		`"fetch_watch_orders"`,
		`"nudge_reviewer"`,
		`"post_daily_briefing"`,
		// Authority matrix entries — runtime vocab; M8.2.c grants
		// `self` on each of the three new tools.
		`"update_ticket_field": "self"`,
		`"find_overdue_tickets": "self"`,
		`"fetch_watch_orders": "self"`,
		`"nudge_reviewer": "self"`,
		`"post_daily_briefing": "self"`,
		`"manifest_version_bump": "lead"`,
		// Model + autonomy unchanged from V1.
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

// TestCoordinatorSeedV2_MigrationContainsExpectedShape is the V2
// equivalent of [TestCoordinatorSeed_MigrationContainsExpectedShape]:
// reads `deploy/migrations/025_coordinator_manifest_v2_seed.sql` off
// disk and asserts the load-bearing literals (V2 manifest_version id,
// version_no=2, the toolset extension with `find_overdue_tickets`,
// the new authority-matrix entry granting `self`, the unchanged
// system-prompt phrases, idempotency clause). Drift fails CI, not
// production.
//
// Migration 024's shape is independently asserted by
// [TestCoordinatorSeed_MigrationContainsExpectedShape]; the two tests
// together pin both the M8.2.a baseline and the M8.2.b extension.
// Future M8.2.c / M8.2.d sub-items add a third / fourth migration-
// shape test under this same pattern.
func TestCoordinatorSeedV2_MigrationContainsExpectedShape(t *testing.T) {
	t.Parallel()

	migrationPath := repoRelative(t, "deploy/migrations/025_coordinator_manifest_v2_seed.sql")
	bytes, err := os.ReadFile(migrationPath) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", migrationPath, err)
	}
	body := string(bytes)

	wantLiterals := []string{
		// Stable ids — V2 manifest_version row + REUSED V1 manifest id.
		CoordinatorManifestVersionV2ID,
		CoordinatorManifestID,
		// Reused system tenant from migration 017 (referenced via the
		// set_config preamble, not as a fresh INSERT).
		WatchmasterSystemOrganizationID,
		// Version progression — V1 stays at version_no=1, V2 lands at 2.
		"  2,",
		// System-prompt phrases (V1 phrases unchanged; V2 keeps them).
		"NEVER reassign tickets",
		"ALWAYS surface a",
		// V2 narrative-guidance appendix for the new read tool
		// (iter-1 critic minor — V1 prompt did not mention the tool).
		"find_overdue_tickets to surface",
		// Toolset extension: V1 entry preserved, V2 entry added.
		`"update_ticket_field"`,
		`"find_overdue_tickets"`,
		// Authority matrix entries (verbatim with quotes — runtime
		// vocab `"self"` / `"lead"`, distinct from the M5.5.b.c.c.b
		// `"lead_approval"` / `"allowed"` enum used by the
		// Watchmaster seed). The M8.2.b authority entry MUST be
		// `self` (read-only).
		`"update_ticket_field": "self"`,
		`"find_overdue_tickets": "self"`,
		`"manifest_version_bump": "lead"`,
		// Model + autonomy unchanged from V1.
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
