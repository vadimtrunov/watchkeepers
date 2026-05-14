package auditsubscriber

import (
	"github.com/vadimtrunov/watchkeepers/core/pkg/approval"
	"github.com/vadimtrunov/watchkeepers/core/pkg/hostedexport"
	"github.com/vadimtrunov/watchkeepers/core/pkg/localpatch"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolshare"
)

// Audit-vocabulary event-type constants. The M9.7 roadmap names each
// audit row by its bare verb-phrase form (no package prefix) — these
// constants are the single source of truth for the
// `keepers_log.event_type` column. The package-prefixed
// [eventbus.Bus] topic strings live on the emitter packages
// (`toolregistry.TopicSourceSynced`, …).
//
// Keeping the bare event-type forms HERE (in the audit subscriber)
// rather than on the emitter packages keeps the boundary one-way:
// emitters publish package-prefixed topics; M9.7 alone knows the
// `keepers_log.event_type` vocabulary. A future event-type rename
// flows through this file; the emitter package stays untouched.
const (
	EventTypeSourceSynced       = "source_synced"
	EventTypeSourceFailed       = "source_failed"
	EventTypeToolShadowed       = "tool_shadowed"
	EventTypeToolProposed       = "tool_proposed"
	EventTypeToolApproved       = "tool_approved"
	EventTypeToolRejected       = "tool_rejected"
	EventTypeDryRunExecuted     = "tool_dry_run_executed"
	EventTypeLocalPatchApplied  = "local_patch_applied"
	EventTypeHostedToolExported = "hosted_tool_exported"
	EventTypeToolShareProposed  = "tool_share_proposed"
	EventTypeToolSharePROpened  = "tool_share_pr_opened"
)

// binding pairs an [eventbus.Bus] topic with its keeperslog
// event-type vocabulary form, the type-asserting [extractor], AND
// the human-readable Go type name of the expected payload. The
// closed-set list below is consumed by [Subscriber.Start] which
// subscribes one handler per binding in declaration order.
//
// `ExpectedType` is carried on the binding (not extracted from the
// extractor closure) so the dispatcher's type-mismatch diagnostic
// log entry can surface a useful, human-readable name (iter-1
// critic m10 lesson: `fmt.Sprintf("%T", err)` on a
// `fmt.Errorf("%w: ...")`-wrapped sentinel always yields
// `*fmt.wrapError`, which carries zero diagnostic signal — drop
// `err_type` from the type-mismatch log; surface `ExpectedType`
// from the binding instead).
type binding struct {
	Topic        string
	EventType    string
	ExpectedType string
	Extract      extractor
}

// allBindings is the closed-set list of 11 (topic, event_type,
// expected type, extractor) tuples the [Subscriber] subscribes to.
//
// Order is by emitter package, then by topic-vocabulary order
// within each package — matches the roadmap M9.7 enumeration so a
// future reader can diff against `docs/ROADMAP-phase1.md` line 512.
//
// Roadmap M9.7 names 19 events; 8 are deferred (no emitter has
// landed yet): `tool_ai_review_passed` / `tool_ai_review_failed`
// (M9.4.b AI-reviewer gate events), `tool_loaded` / `tool_retired`
// (M9.1.b registry hot-reload lifecycle),
// `signature_verification_failed` (M9.3 cosign/minisign),
// `hosted_tool_stored` (M9.6 hosted-mode landing — likely subsumed
// by `hosted_tool_exported` once the hosted-storage event lands),
// `tool_share_pr_merged` / `tool_share_pr_rejected` (M9.6 webhook
// receiver, explicitly deferred in the M9.6 lesson). When any of
// those emitters lands, a follow-up CL appends to this slice +
// bumps the lesson; the emitter-package audit_grep test still
// enforces the one-way flow.
var allBindings = []binding{
	{Topic: toolregistry.TopicSourceSynced, EventType: EventTypeSourceSynced, ExpectedType: "toolregistry.SourceSynced", Extract: extractSourceSynced},
	{Topic: toolregistry.TopicSourceFailed, EventType: EventTypeSourceFailed, ExpectedType: "toolregistry.SourceFailed", Extract: extractSourceFailed},
	{Topic: toolregistry.TopicToolShadowed, EventType: EventTypeToolShadowed, ExpectedType: "toolregistry.ToolShadowed", Extract: extractToolShadowed},
	{Topic: approval.TopicToolProposed, EventType: EventTypeToolProposed, ExpectedType: "approval.ToolProposed", Extract: extractToolProposed},
	{Topic: approval.TopicToolApproved, EventType: EventTypeToolApproved, ExpectedType: "approval.ToolApproved", Extract: extractToolApproved},
	{Topic: approval.TopicToolRejected, EventType: EventTypeToolRejected, ExpectedType: "approval.ToolRejected", Extract: extractToolRejected},
	{Topic: approval.TopicDryRunExecuted, EventType: EventTypeDryRunExecuted, ExpectedType: "approval.DryRunExecuted", Extract: extractDryRunExecuted},
	{Topic: localpatch.TopicLocalPatchApplied, EventType: EventTypeLocalPatchApplied, ExpectedType: "localpatch.LocalPatchApplied", Extract: extractLocalPatchApplied},
	{Topic: hostedexport.TopicHostedToolExported, EventType: EventTypeHostedToolExported, ExpectedType: "hostedexport.HostedToolExported", Extract: extractHostedToolExported},
	{Topic: toolshare.TopicToolShareProposed, EventType: EventTypeToolShareProposed, ExpectedType: "toolshare.ToolShareProposed", Extract: extractToolShareProposed},
	{Topic: toolshare.TopicToolSharePROpened, EventType: EventTypeToolSharePROpened, ExpectedType: "toolshare.ToolSharePROpened", Extract: extractToolSharePROpened},
}
