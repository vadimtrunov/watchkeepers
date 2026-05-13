// Package toolshare implements the M9.6 tool-share orchestrator
// that the `wk-tool share` CLI subcommand and the `promote_share_tool`
// agent capability wrap.
//
// One sharing operation lands here:
//
//   - Share — reads a tool tree from a configured source's live
//     directory under `<DataDir>/tools/<source>/<tool>/`, opens a
//     pull request against a target git repository (`platform`
//     [watchkeeper-tools] OR a customer-owned private repo) with
//     the tool's full bundle (manifest + source + tests) committed
//     onto a freshly-created share branch, and (optionally) DMs
//     the lead in Slack with a link to the PR. Emits a pair of
//     audit events: [TopicToolShareProposed] BEFORE the github
//     calls begin and [TopicToolSharePROpened] AFTER the PR
//     creation succeeds.
//
// The boundary at M9.6 is deliberately:
//
//   - **upstream of webhook PR-merge / PR-rejected handling.** The
//     downstream `tool_share_pr_merged` / `tool_share_pr_rejected`
//     audit events the M9.7 audit-surface list names land via a
//     webhook receiver on the watchkeeper-tools repo (operator
//     wires a GitHub webhook → core's webhook receiver from M9.4.b)
//     or via the standard scheduled re-sync (the lead merges the
//     share PR, on the next pull cycle the new tool version
//     appears). Either path is operator-configurable; the M9.6
//     core ships the create-PR side only.
//   - **downstream of `tool:share` capability gating.** The
//     agent-facing entry point in `core/pkg/harnessrpc/` validates
//     the `tool:share` scope via [capability.Broker.Validate]
//     BEFORE invoking this orchestrator. The orchestrator itself
//     does NOT re-validate — its trust boundary is "the caller has
//     already authorised this share".
//
// audit discipline (mirrors M9.5 localpatch + M9.4.b/c): the
// package never imports `keeperslog` and never calls `.Append(`.
// The audit channel for share is the two [eventbus.Bus] topics
// the M9.7 audit subscriber observes.
//
// PII discipline: both bus payloads carry metadata + accountability
// fields ONLY — `SourceName`, `ToolName`, `ToolVersion`,
// `ProposerID`, `Reason`, `TargetOwner`, `TargetRepo`, `TargetBase`,
// `PRNumber` (post-open only), `PRHTMLURL` (post-open only),
// `ProposedAt`, `CorrelationID`. The `Reason` field is
// agent-supplied free-form text — verbatim, bounded. The
// `ProposerID` is a stable public identifier (agent uuid or
// human handle), NEVER a credential or session token. Tool
// source code, the on-disk live path, the operator-supplied PAT
// or GitHub-App installation id, and the resolved Slack message
// timestamp NEVER land on the payload — the cryptographically-
// bounded summary (commit SHA returned by github) IS sufficient
// for the audit subscriber's downstream join.
//
// Sharer.Share resolution order (mirrors M9.5
// [localpatch.Installer.Install]):
//
//   - nil-dep check (panic with named-field message)
//   - input.Validate (sentinel pass-through)
//   - ctx.Err pass-through
//   - source-config lookup (any kind permitted — share targets the
//     deployment's effective tool tree)
//   - ProposerIdentityResolver
//   - on-disk read of `<DataDir>/tools/<source>/<tool>/manifest.json`
//   - target-repo resolution (platform vs private mapping)
//   - publish [TopicToolShareProposed] with [context.WithoutCancel]
//   - github.GetRef (base branch tip SHA)
//   - github.CreateRef (fresh share branch)
//   - github.CreateOrUpdateFile per regular file in the tool tree
//   - github.CreatePullRequest
//   - publish [TopicToolSharePROpened] with [context.WithoutCancel]
//   - (optional) Slack DM to the lead with the PR HTML URL
//
// Per-call seam discipline: the proposer-identity resolver, the
// target-repo resolver, the GitHub token source, and the Slack
// lead-id resolver are all per-invocation seams. Same lesson as
// M9.1.a's `AuthSecretResolver`, M9.5's `OperatorIdentityResolver`.
//
// Failure-isolation discipline: the Slack DM is best-effort —
// a Slack outage MUST NOT undo the PR open (the share IS the
// durable outcome; Slack is human courtesy). A Slack failure
// surfaces via the optional [Logger] but does NOT propagate as
// an error from [Sharer.Share].
package toolshare
