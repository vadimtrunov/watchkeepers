package manifest

// WatchmasterManifestID is the stable UUID identifying the canonical
// Watchmaster manifest seeded by `deploy/migrations/017_watchmaster_manifest_seed.sql`.
//
// Downstream callers (M6.1.b privileged Slack App creation RPC, M6.2
// Watchmaster toolset wiring, M6.3 operator surface / Slack DMs) MUST
// reference this constant rather than hard-coding the UUID literal so
// a future re-seed (e.g., per-tenant Watchmasters) is a one-line change
// here. The literal is asserted verbatim against the migration file in
// [TestWatchmasterSeedConstantMatchesMigration] (see
// `core/pkg/manifest/watchmaster_seed_test.go`); a drift between the
// SQL seed and the Go constant fails CI, not production.
//
// The companion id `WatchmasterSystemOrganizationID` (the "system"
// tenant the Watchmaster runs under) is also seeded by the same
// migration; M6.1.b/M6.2/M6.3 reference both via these constants.
const (
	// WatchmasterManifestID is the `manifest.id` UUID for the
	// Watchmaster row seeded by migration 017.
	WatchmasterManifestID = "10000000-0000-4000-8000-000000000000"

	// WatchmasterSystemOrganizationID is the `organization.id` UUID
	// for the "system" tenant under which the Watchmaster manifest is
	// scoped. Also seeded by migration 017.
	WatchmasterSystemOrganizationID = "00000000-0000-4000-8000-000000000000"
)
