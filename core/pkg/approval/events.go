package approval

import (
	"time"

	"github.com/google/uuid"
)

// TopicToolProposed is the [eventbus.Bus] topic [Proposer.Submit]
// emits to once a [ProposalInput] passes validation and a durable
// [Proposal.ID] has been minted. Subscribers (M9.4.b webhook + AI
// reviewer wiring, M9.7 audit) consume this topic to drive the
// approval pipeline.
//
// Topic-name discipline: prefix `approval.` mirrors the
// `toolregistry.` prefix used by M9.1.a/b/M9.2 — the topic-name
// boundary is the package boundary.
const TopicToolProposed = "approval.tool_proposed"

// ToolProposed is the metadata-only payload published on
// [TopicToolProposed]. The payload exists so a subscriber can decide
// "I should fetch this proposal record" without dragging the full
// `code_draft` body onto the eventbus.
//
// PII discipline: the payload carries the proposal id, the tool
// name (a public identifier), the proposer agent id (already public
// across the runtime), the target-source enum, the list of
// capability ids (public dictionary entries per M9.3), the
// timestamp, and a correlation id. It DELIBERATELY does NOT carry:
//
//   - `Purpose` — agent free-form rationale that may name
//     customer-specific context.
//   - `PlainLanguageDescription` — lead-facing prose that may
//     embed customer names, Slack-mention syntax, or backticks (the
//     M9.2 iter-1 M3 lesson applies: untrusted authoring content
//     stays out of any rendered template until the renderer scrubs
//     it).
//   - `CodeDraft` — proprietary / AI-authored source code; a
//     verbose subscriber log would dump it otherwise.
//
// Subscribers that need the omitted fields query the future M9.4.b
// proposal store / M9.7 audit subscriber by `ProposalID`.
//
// Same field-allowlist discipline as M9.1.b's
// [toolregistry.EffectiveToolsetUpdated] and M9.2's
// [toolregistry.ToolShadowed]: a reflection-based test pins the
// payload to 7 fields by name + type, so adding a field requires
// the author bump the allowlist test AND consciously document why
// the new field's PII shape is OK.
type ToolProposed struct {
	// ProposalID matches [Proposal.ID]; the join key for any
	// subscriber querying the proposal store.
	ProposalID uuid.UUID

	// ToolName is the proposed [ProposalInput.Name] (validated by
	// [ProposalInput.Validate] to be lower_snake_case and bounded
	// in length). Becomes [toolregistry.Manifest.Name] post-
	// approval.
	ToolName string

	// ProposerID is the agent identity the [IdentityResolver]
	// returned at [Proposer.Submit] time — non-empty by construction.
	ProposerID string

	// TargetSource is the enum the agent chose (no `local` — see
	// [TargetSource] godoc).
	TargetSource TargetSource

	// CapabilityIDs is the defensively-deep-copied list of
	// capability ids from [ProposalInput.Capabilities]. The list
	// shape (not just a count) is on the payload because
	// capability ids are public dictionary entries and the
	// downstream M9.4.b approval card needs them to render
	// human-readable capability translations.
	CapabilityIDs []string

	// ProposedAt mirrors [Proposal.ProposedAt] — captured from the
	// configured [Clock] inside [Proposer.Submit].
	ProposedAt time.Time

	// CorrelationID mirrors [Proposal.CorrelationID]; the same
	// opaque shape as [toolregistry.SourceSynced.CorrelationID].
	CorrelationID string
}

// newToolProposedEvent constructs a [ToolProposed] event payload
// from a [Proposal]. Centralising the mapping keeps the field-
// parity contract auditable: a future addition to [Proposal] that
// needs to land on the event flows through this constructor, and
// the parallel reflection-based allowlist test on [ToolProposed]
// catches silent drift. Mirrors the M9.2 `newToolShadowedEvent`
// discipline.
func newToolProposedEvent(p Proposal) ToolProposed {
	caps := make([]string, len(p.Input.Capabilities))
	copy(caps, p.Input.Capabilities)
	return ToolProposed{
		ProposalID:    p.ID,
		ToolName:      p.Input.Name,
		ProposerID:    p.ProposerID,
		TargetSource:  p.Input.TargetSource,
		CapabilityIDs: caps,
		ProposedAt:    p.ProposedAt,
		CorrelationID: p.CorrelationID,
	}
}
