package manifest

// CoordinatorManifestID is the stable UUID identifying the canonical
// Coordinator manifest seeded by
// `deploy/migrations/024_coordinator_manifest_seed.sql`.
//
// Downstream callers (the M8.2.a tool dispatch primitive, the M8.2.b
// Jira read tool, the M8.2.c Slack tools bundle, the M8.2.d GitHub
// PR tool) MUST reference this constant rather than hard-coding the
// UUID literal so a future re-seed (e.g., per-tenant Coordinators in
// M8.3+) is a one-line change here. The literal is asserted verbatim
// against the migration file in
// [TestCoordinatorSeed_MigrationContainsExpectedShape] (see
// `core/pkg/manifest/coordinator_seed_test.go`); a drift between the
// SQL seed and the Go constant fails CI, not production.
//
// The Coordinator runs under the same "system" tenant the Watchmaster
// runs under: organization id `00000000-0000-4000-8000-000000000000`,
// already exported as [WatchmasterSystemOrganizationID] in
// `watchmaster.go`. Phase 1 keeps both meta-agents under the same
// namespace; per-tenant Coordinators land in M8.3+.
//
// Unlike the Watchmaster (autonomy=`supervised`, every action gated
// by lead approval), the Coordinator runs under autonomy=`autonomous`
// — the runtime consults its authority matrix per action via
// [github.com/vadimtrunov/watchkeepers/core/pkg/runtime.RequiresApproval].
// The seed authority matrix uses the runtime authority vocabulary
// (`"self"` / `"lead"` / `"operator"` / `"watchmaster"`); the M5.5
// loader projects the matrix verbatim and the runtime gate consults
// it on every InvokeTool call.
const (
	// CoordinatorManifestID is the `manifest.id` UUID for the
	// Coordinator row seeded by migration 024.
	CoordinatorManifestID = "20000000-0000-4000-8000-000000000000"

	// CoordinatorManifestVersionID is the initial `manifest_version.id`
	// (version_no=1) UUID seeded by migration 024. Exported because the
	// migration-shape test asserts it appears verbatim in the SQL file.
	CoordinatorManifestVersionID = "21000000-0000-4000-8000-000000000000"

	// CoordinatorManifestVersionV2ID is the `manifest_version.id`
	// (version_no=2) UUID seeded by migration 025 (M8.2.b). Supersedes
	// [CoordinatorManifestVersionID] — keepclient.GetManifest returns
	// the manifest_version row with the highest version_no for a given
	// manifest_id (`core/pkg/keepclient/read_manifest.go:63-67`), so
	// after migration 025 runs the runtime loads V2 for boot. The V2
	// row extends the toolset with `find_overdue_tickets` and grants
	// `self` on it in the authority matrix; the system prompt,
	// personality, model, autonomy, and notebook recall tunables are
	// unchanged from V1.
	//
	// Future M8.2.c / M8.2.d sub-items add V3 / V4 rows under the same
	// pattern (new id + version_no, INSERT-only, ON CONFLICT (id) DO
	// NOTHING) so historical versions remain recoverable from the
	// migration sequence.
	CoordinatorManifestVersionV2ID = "22000000-0000-4000-8000-000000000000"
)
