package capdict

// CanonicalCapabilities is the closed-set authority for the
// Phase 1 capability vocabulary. The two-way bijection test in
// `canonical_test.go` pins this slice against `dict/capabilities.yaml`
// — adding a capability requires one YAML row AND one entry here,
// and the CI completeness check fails when either side drifts.
//
// Mirrors the M9.7 `auditsubscriber.allBindings` / `roadmapNames`
// closed-set pin. The slice is sorted by id so a future reader can
// confirm coverage by eyeballing.
//
// Adding a capability:
//
//  1. Append the id (lower_snake_case + colon namespace) here in
//     sorted order.
//  2. Append the matching `"<id>": {description: "..."}` entry in
//     `dict/capabilities.yaml`.
//  3. Run `go test ./core/pkg/capdict/...` — the bijection test
//     surfaces the missing side.
//
// Removing a capability follows the same shape in reverse — both
// sides drop in one CL.
var CanonicalCapabilities = []string{
	"filesystem:read",
	"github:list",
	"github:read",
	"github:write",
	"jira:read",
	"jira:write",
	"keep:read",
	"keep:write",
	"network:http",
	"notebook:read",
	"notebook:write",
	"slack:read",
	"slack:send",
	"tool:share",
}
