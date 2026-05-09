// spawn_context.go defines the [SpawnContext] value the saga
// [Runner]'s caller threads through `context.Context` so individual
// [Step] implementations can read manifest_version_id / agent_id /
// claim without re-plumbing each Step's own constructor.
//
// The value is intentionally tiny (three fields) and lives at the
// saga-package layer rather than at the spawn-package layer because:
//
//  1. Future M7.1.c.b–.e steps will all consume it — repeating the
//     `WithFoo` / `FooFromContext` pair on every step package would
//     drift over time.
//  2. The spawn package depends on saga (concrete steps live in
//     `core/pkg/spawn` and import `core/pkg/spawn/saga`); housing
//     the value at the saga layer keeps the dependency direction
//     one-way.
//  3. The matching [SpawnClaim] is structurally identical to
//     `spawn.Claim` but redeclared here to avoid the dependency
//     cycle — the step author converts on call.
package saga

import (
	"context"

	"github.com/google/uuid"
)

// spawnContextKeyType is a private type used as the key for storing
// [SpawnContext] in a `context.Context`. The unexported type ensures
// no other package can collide with the key (string-keyed
// `context.WithValue` calls cannot reach this key by accident).
type spawnContextKeyType struct{}

// SpawnContextKey is the context key under which [WithSpawnContext]
// stores the [SpawnContext] value. Exported as an `any` so callers
// reading the key for diagnostics (e.g. a tracing extractor) can
// reference it without importing the unexported type. This is the
// idiomatic context.Key pattern.
//
//nolint:gochecknoglobals // package-level singleton; idiomatic context.Key pattern.
var SpawnContextKey any = spawnContextKeyType{}

// SpawnClaim is the structural mirror of `spawn.Claim` redeclared at
// the saga layer to avoid a `saga -> spawn` import cycle. The step
// author converts the saga-layer claim into the spawn-layer claim
// when calling the privileged RPC; the field shape is identical so
// the conversion is a single struct literal.
//
// All fields mirror their `spawn.Claim` counterparts:
//
//   - OrganizationID is the tenant the call is scoped to (M3.5.a
//     tenant-scoping discipline).
//   - AgentID is the manifest-projected id of the calling agent.
//   - AuthorityMatrix is the manifest-projected authority matrix the
//     gate consults (e.g. `slack_app_create` -> `lead_approval`).
type SpawnClaim struct {
	OrganizationID  string
	AgentID         string
	AuthorityMatrix map[string]string
}

// SpawnContext is the value the saga's caller threads through
// `context.Context` so individual [Step] implementations can read the
// saga's manifest_version_id / agent_id / claim without re-plumbing
// each Step's own constructor.
//
// All fields are typed (no `interface{}`) per the package's
// fail-fast-on-shape discipline. A zero value is structurally valid
// (every field zero-valued); steps that require non-zero fields
// validate them in their own `Execute` body and return a wrapped
// error before doing any externally-visible work.
type SpawnContext struct {
	// ManifestVersionID is the manifest_version this saga is
	// spawning. Stored at saga-row insert time and never mutated; the
	// CreateApp step reads it back so it can compose the
	// CreateAppRequest with the correct app name / scopes / etc.
	ManifestVersionID uuid.UUID

	// AgentID is the watchkeeper id this saga is provisioning. Used
	// by the CreateApp step as the DAO key for the slack_app_creds
	// row — the watchkeeper id is the stable saga-row id; the
	// Slack-assigned app_id can change across re-create scenarios.
	AgentID uuid.UUID

	// Claim is the auth tuple the dispatcher already minted at saga
	// kickoff time. Forwarded verbatim to the privileged RPC so the
	// step does not re-mint or re-validate it.
	Claim SpawnClaim

	// OAuthCode is the Slack-issued single-use authorization code the
	// M7.1.c.b.b OAuthInstall step exchanges for bot/user tokens via
	// `oauth.v2.access`. Phase-1 admin-grant flow: the operator pastes
	// the code captured from Slack's admin install UI when seeding the
	// saga; future M7.x will replace this with an auto-install callback
	// HTTP route. Empty until the operator supplies it; the install
	// step short-circuits with a wrapped sentinel BEFORE contacting
	// Slack when this field is empty (resolution failure is a security
	// boundary — see `core/pkg/spawn/oauthinstall_step.go`). Steps that
	// do not consume this field continue to ignore it (additive change
	// — every previously-zero usage stays valid).
	OAuthCode string
}

// WithSpawnContext returns a new context carrying `sc` under
// [SpawnContextKey]. Replaces any [SpawnContext] previously stored on
// the parent context (last write wins; the saga only seeds one).
func WithSpawnContext(parent context.Context, sc SpawnContext) context.Context {
	return context.WithValue(parent, spawnContextKeyType{}, sc)
}

// SpawnContextFromContext returns the [SpawnContext] stored on `ctx`
// by a prior [WithSpawnContext] call, plus a boolean reporting
// whether such a value was present. Callers that require the value
// to be present (e.g. the CreateApp step) wrap a missing-context
// error rather than proceeding with a zero-valued [SpawnContext].
func SpawnContextFromContext(ctx context.Context) (SpawnContext, bool) {
	sc, ok := ctx.Value(spawnContextKeyType{}).(SpawnContext)
	return sc, ok
}
