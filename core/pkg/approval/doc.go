// Package approval implements the M9.4.a authoring surface for the
// Watchkeeper tool registry: the `propose_tool` agent-facing input
// validator, the per-deployment `approval_mode` configuration, and
// the typed `target_source` enum.
//
// The package is deliberately decoupled from `core/pkg/toolregistry`:
// it consumes the registry's [toolregistry.SourceConfig] only for the
// cross-field check that enforces `slack-native` (or `both`) when any
// configured source has [toolregistry.SourceKindHosted]. The registry
// does NOT import this package — the dependency arrow points one way.
//
// Scope at M9.4.a:
//
//   - Input validation for [Proposer.Submit].
//   - Strict YAML decoding of `approval_mode:` config blocks.
//   - Emission of `TopicToolProposed` events carrying metadata only
//     (proposal id, tool name, proposer id, target source, capability
//     ids, timestamp, correlation id) — never `code_draft`, never
//     `plain_language_description`. Same PII-boundary discipline as
//     M9.1.b's [toolregistry.EffectiveToolsetUpdated] and M9.2's
//     [toolregistry.ToolShadowed]. Deliberate deviation from the
//     M9.4 roadmap text "capability count": the M9.4.b approval-card
//     renderer needs the capability ID list to look up
//     human-readable translations (M9.3.a `dict/capabilities.yaml`),
//     and capability IDs are public dictionary entries (not PII), so
//     the payload carries the list rather than just a count.
//
// Out of scope (deferred to M9.4.b/c/d):
//
//   - Persistence of [Proposal] records (events are the durable log
//     at this layer; a SQL DAO joins via M9.4.b's webhook flow).
//   - The git-pr webhook receiver, the slack-native AI reviewer, and
//     the Slack approval card with `[Approve] [Reject] [Test in my DM]
//     [Ask questions]` button callbacks (M9.4.b).
//   - The dry-run runtime executor honouring
//     [toolregistry.Manifest.DryRunMode] at `InvokeTool` time
//     (M9.4.c).
//   - The shared CI workflow YAML template published by the
//     `watchkeeper-tools` repo (M9.4.d).
package approval
