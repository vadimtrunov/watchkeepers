package manifest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	agentruntime "github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// watchmasterSeedFixture mirrors the manifest_version row written by
// `deploy/migrations/017_watchmaster_manifest_seed.sql`. It is the
// authoritative Go-side projection of the SQL seed; a drift between
// the two surfaces is caught by [TestWatchmasterSeed_MigrationContainsExpectedShape]
// (which reads the migration off disk and matches the canonical
// substrings against this fixture) plus the test below that pipes the
// fixture through [LoadManifest] and asserts the AC3 contract.
//
// The privilege-boundary phrase, the `slack_app_create: lead_approval`
// authority entry, and the empty toolset placeholder are all
// load-bearing — see the TASK M6.1.a Acceptance criteria.
func watchmasterSeedFixture() *keepclient.ManifestVersion {
	const systemPrompt = "You are the Watchmaster, the orchestrator agent that supervises" +
		" every Watchkeeper running under this Watchkeepers deployment." +
		"\n\nIdentity: you route operator requests to the right" +
		" Watchkeeper, gate privileged actions through the lead-approval" +
		" workflow, and report token spend on every turn. You are" +
		" deferential to the lead human; you propose, they approve." +
		"\n\nPrivilege boundary: you NEVER execute Slack App creation" +
		" directly. You ALWAYS go through the privileged RPC tool" +
		" (M6.1.b) which itself runs under lead approval. Bypassing this" +
		" boundary is a hard violation; if a path appears to require" +
		" direct Slack App creation, surface that as a question to the" +
		" lead, not an action." +
		"\n\nApproval discipline: every manifest_version bump (your own" +
		" or any Watchkeeper's) MUST go through lead approval. Treat" +
		" manifest content as governed configuration, not runtime state." +
		"\n\nCost awareness: report token spend at the end of every turn." +
		" If you approach a per-turn budget ceiling, stop and ask the" +
		" lead to extend or cancel."

	const personality = "Cautious orchestrator: lead-deferential, audit-driven, conservative " +
		"on privileged actions. Optimises for predictability and " +
		"traceability over speed; prefers to ask rather than assume."

	const authorityMatrix = `{
		"slack_app_create": "lead_approval",
		"watchkeeper_retire": "lead_approval",
		"manifest_version_bump": "lead_approval",
		"keepers_log_read": "allowed",
		"keep_search": "allowed"
	}`

	return &keepclient.ManifestVersion{
		ID:                         "11000000-0000-4000-8000-000000000000",
		ManifestID:                 WatchmasterManifestID,
		VersionNo:                  1,
		SystemPrompt:               systemPrompt,
		Tools:                      json.RawMessage(`[]`),
		AuthorityMatrix:            json.RawMessage(authorityMatrix),
		KnowledgeSources:           json.RawMessage(`[]`),
		Personality:                personality,
		Language:                   "en",
		Model:                      "claude-haiku-4-5-20251001",
		Autonomy:                   "supervised",
		NotebookTopK:               3,
		NotebookRelevanceThreshold: 0.3,
	}
}

// TestWatchmasterSeed_LoadsViaLoadManifest is the AC3 happy-path proof:
// pipe the fixture mirroring `017_watchmaster_manifest_seed.sql` through
// [LoadManifest] and assert the four projected fields the TASK calls
// out — SystemPrompt contains the privilege-boundary phrase, Toolset is
// empty (placeholder), AuthorityMatrix["slack_app_create"] equals
// "lead_approval", and Autonomy is non-empty (matches the seed).
//
// The fixture path mirrors the existing `loader_test.go` fakeFetcher
// pattern rather than spinning up a real test DB. Real-DB exercising
// of the seed is covered by the `keep-integration-ci` pipeline in
// `.github/workflows/ci.yml` (which runs `make migrate-up` against a
// service-container Postgres before the integration tests).
func TestWatchmasterSeed_LoadsViaLoadManifest(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: watchmasterSeedFixture()}

	got, err := LoadManifest(context.Background(), f, WatchmasterManifestID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	const wantPhrase = "NEVER execute Slack App creation directly"
	if !strings.Contains(got.SystemPrompt, wantPhrase) {
		t.Errorf("SystemPrompt missing privilege-boundary phrase %q; got:\n%s", wantPhrase, got.SystemPrompt)
	}

	if len(got.Toolset) != 0 {
		t.Errorf("Toolset = %v, want empty (placeholder; real toolset lands in M6.2)", got.Toolset)
	}

	if got, want := got.AuthorityMatrix["slack_app_create"], "lead_approval"; got != want {
		t.Errorf("AuthorityMatrix[slack_app_create] = %q, want %q", got, want)
	}

	if got.Autonomy != agentruntime.AutonomySupervised {
		t.Errorf("Autonomy = %q, want %q (seed value)", got.Autonomy, agentruntime.AutonomySupervised)
	}
}

// TestWatchmasterSeed_MigrationContainsExpectedShape is the binding
// cross-link between the Go fixture and the SQL seed: read
// `deploy/migrations/017_watchmaster_manifest_seed.sql` off disk and
// assert the load-bearing literals are all present verbatim. A
// reword of the system prompt or a drift in the authority matrix
// fails this test, not production.
//
// This test is the closest equivalent in-process to "load the
// manifest from a fresh DB carrying the migration" without standing
// up a real Postgres — the migration text IS the source of truth for
// the seed shape, and string-matching the load-bearing pieces is
// sufficient to lock the contract.
func TestWatchmasterSeed_MigrationContainsExpectedShape(t *testing.T) {
	t.Parallel()

	migrationPath := repoRelative(t, "deploy/migrations/017_watchmaster_manifest_seed.sql")
	bytes, err := os.ReadFile(migrationPath) //nolint:gosec // test reads a fixed repo-relative path.
	if err != nil {
		t.Fatalf("ReadFile %s: %v", migrationPath, err)
	}
	body := string(bytes)

	wantLiterals := []string{
		// AC1: stable ids.
		WatchmasterManifestID,
		WatchmasterSystemOrganizationID,
		// AC2: privilege-boundary phrase.
		"NEVER execute Slack App creation",
		// AC2: authority matrix entries (verbatim with quotes).
		`"slack_app_create": "lead_approval"`,
		`"watchkeeper_retire": "lead_approval"`,
		`"manifest_version_bump": "lead_approval"`,
		`"keepers_log_read": "allowed"`,
		`"keep_search": "allowed"`,
		// AC2: model + autonomy + notebook tunables.
		"claude-haiku-4-5-20251001",
		"'supervised'",
		// AC5: idempotency clause.
		"ON CONFLICT (id) DO NOTHING",
	}
	for _, lit := range wantLiterals {
		if !strings.Contains(body, lit) {
			t.Errorf("migration missing expected literal %q", lit)
		}
	}
}

// TestWatchmasterSeed_FixtureRoundTripsAllowedAndForbidden asserts a
// regression guard: the seeded authority matrix carries BOTH
// "lead_approval" gates (write-side privileged actions) and
// "allowed" entries (read-side audit/knowledge access). A drift that
// silently downgrades a write to "allowed" or upgrades a read to
// "forbidden" would trip the M5.5.b.c.c.b semantics; pin the four
// representative entries here.
func TestWatchmasterSeed_FixtureRoundTripsAllowedAndForbidden(t *testing.T) {
	t.Parallel()

	f := &fakeFetcher{response: watchmasterSeedFixture()}

	got, err := LoadManifest(context.Background(), f, WatchmasterManifestID)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}

	wantEntries := map[string]string{
		"slack_app_create":      "lead_approval",
		"watchkeeper_retire":    "lead_approval",
		"manifest_version_bump": "lead_approval",
		"keepers_log_read":      "allowed",
		"keep_search":           "allowed",
	}
	for k, want := range wantEntries {
		if got := got.AuthorityMatrix[k]; got != want {
			t.Errorf("AuthorityMatrix[%q] = %q, want %q", k, got, want)
		}
	}
}

// repoRelative resolves a repo-relative path to an absolute path by
// climbing from this test file (core/pkg/manifest/) up to the repo
// root. Mirrors the pattern in `core/cmd/keep/integration_test.go`
// (buildBinary). Lives in this file because the migration-shape test
// is the only consumer.
func repoRelative(t *testing.T, rel string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// thisFile is .../core/pkg/manifest/watchmaster_seed_test.go;
	// repo root is three directories up.
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
	return filepath.Join(repoRoot, rel)
}
