// promote_to_keep.go implements the M6.2.d lead-approval-gated
// Watchmaster tool that lifts a single Notebook entry into the
// `watchkeeper.knowledge_chunk` table on Keep, via a read-then-write
// across the notebook→keep boundary:
//
//  1. Read the notebook entry via [WatchmasterNotebookClient.PromoteToKeep]
//     (concrete impl: [notebook.DB.PromoteToKeep]). This call already
//     emits its own `notebook_promotion_proposed` audit event on
//     success; failure aborts BEFORE any audit row from THIS tool is
//     written ("GetManifest pre-emit short-circuit" lesson — read
//     failure aborts BEFORE the audit-requested boundary).
//  2. Emit `watchmaster_promote_to_keep_requested` (security audit).
//  3. Map the [notebook.Proposal] to a [keepclient.StoreRequest] and
//     call [WatchmasterWriteClient.Store] to insert the row into
//     `watchkeeper.knowledge_chunk`.
//  4. Emit `notebook_promoted_to_keep` (domain event the ROADMAP names —
//     the M6.2 follow-up the notebook.PromoteToKeep doc points at) AND
//     `watchmaster_promote_to_keep_succeeded` (security audit). On keep
//     Store failure, emit `watchmaster_promote_to_keep_failed` instead;
//     the domain event is NOT emitted on failure.
//
// Shape mirrors [AdjustPersonality] / [AdjustLanguage] (read-then-write
// with pre-emit short-circuit), NOT [ProposeSpawn] (no pre-read). The
// gate stack mirrors M6.1.b SlackAppRPC.CreateApp / M6.2.b manifest
// bumps verbatim — validate `claim.OrganizationID`, validate
// `claim.AuthorityMatrix["promote_to_keep"] == "lead_approval"`,
// validate `req.ApprovalToken` non-empty.
//
// PII / large-field discipline (M2b.7): audit payloads NEVER include
// `content` or `embedding` keys. Same rule notebook.PromoteToKeep
// follows. The domain event payload carries `notebook_entry_id`,
// `proposal_id`, `chunk_id`, `subject`, `category`, `scope`,
// `source_created_at`, `proposed_at`, `promoted_at` — provenance only,
// no body bytes.
package spawn

import (
	"context"
	"fmt"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

// authorityActionPromoteToKeep is the action string the Watchmaster
// manifest publishes in `authority_matrix` to gate the
// promote_to_keep tool. Hoisted to a package constant so a re-key is a
// one-line change here that the gate AND the keepers_log payload pick
// up via the compiler.
const authorityActionPromoteToKeep = "promote_to_keep"

// keepersLogEventPromote* values the promote tool emits on the audit
// chain. Project convention: snake_case `<tool>_<event_class>` past
// tense, prefixed with the surface owner (`watchmaster`). The fourth
// event (`notebook_promoted_to_keep`) is a domain event named by the
// ROADMAP (see core/pkg/notebook/promote.go header), not a security
// audit row — kept distinct from the `_requested|_succeeded|_failed`
// security-audit triplet so downstream subscribers can tell "agent
// wants to share X" / "Watchmaster approved & persisted X" / per-action
// audit apart.
const (
	keepersLogEventPromoteRequested = "watchmaster_promote_to_keep_requested"
	keepersLogEventPromoteSucceeded = "watchmaster_promote_to_keep_succeeded"
	keepersLogEventPromoteFailed    = "watchmaster_promote_to_keep_failed"
	keepersLogEventNotebookPromoted = "notebook_promoted_to_keep"
)

// payloadValuePromoteToKeep is the `tool_name` payload value the
// promote audit rows carry. Matches the wire-side tool registry name in
// `harness/src/builtinTools.ts` so a downstream consumer can group
// audit rows by tool_name without string-rewriting.
const payloadValuePromoteToKeep = "promote_to_keep"

// payloadKey* constants specific to the promote_to_keep audit chain
// (the shared keys — agent_id, tool_name, event_class, error_class —
// live in slack_app.go; the manifest-bump-specific keys live in
// watchmaster_writes.go). Per AC2: the domain event payload carries
// provenance fields but NEVER `content` or `embedding`.
const (
	payloadKeyNotebookEntryID = "notebook_entry_id"
	payloadKeyProposalID      = "proposal_id"
	payloadKeyChunkID         = "chunk_id"
	payloadKeySubject         = "subject"
	payloadKeyCategory        = "category"
	payloadKeyScope           = "scope"
	payloadKeySourceCreatedAt = "source_created_at"
	payloadKeyProposedAt      = "proposed_at"
	payloadKeyPromotedAt      = "promoted_at"
)

// WatchmasterNotebookClient is the minimal subset of the
// [*notebook.DB] surface the Watchmaster promote_to_keep tool consumes.
// Defined as an interface in this package so tests can substitute a
// hand-rolled stub without standing up a real SQLite file, and so
// production code in this package never imports the concrete `*notebook.DB`
// type at all (mirrors the [WatchmasterWriteClient] /
// keeperslog.LocalKeepClient pattern).
//
// `*notebook.DB` satisfies this interface as-is; the compile-time
// assertion lives below.
//
// Errors mirror [notebook.DB.PromoteToKeep] verbatim:
// [notebook.ErrInvalidEntry] (non-canonical UUID),
// [notebook.ErrNotFound] (well-formed but missing entry), or any
// underlying DB error.
type WatchmasterNotebookClient interface {
	PromoteToKeep(ctx context.Context, entryID string) (*notebook.Proposal, error)
}

// Compile-time assertion: every [*notebook.DB] satisfies
// [WatchmasterNotebookClient] by definition. Pins the integration shape
// against future drift in the notebook package.
var _ WatchmasterNotebookClient = (*notebook.DB)(nil)

// PromoteToKeepRequest is the value supplied to [PromoteToKeep]. The
// caller supplies the calling agent's `AgentID` (the Watchmaster's
// claim agent id is the alternative source) plus the
// `NotebookEntryID` of the source entry to promote.
type PromoteToKeepRequest struct {
	// AgentID is the calling agent's id (used as the audit row's
	// `agent_id` and the source notebook owner). Required when the
	// claim does not already supply it.
	AgentID string

	// NotebookEntryID is the UUID v7 of the source `entry` row in
	// the notebook DB. Required; empty fails synchronously with
	// [ErrInvalidRequest].
	NotebookEntryID string

	// ApprovalToken is the opaque token the lead-approval saga
	// (M6.3) issues. M6.2.d validation is non-empty only; M6.3
	// owns the cryptography. Required; empty fails with
	// [ErrApprovalRequired] AFTER the authority-matrix gate
	// passes (so a forbidden caller is told `unauthorized`,
	// not `approval required`).
	ApprovalToken string
}

// PromoteToKeepResult is the value returned from a successful
// [PromoteToKeep] call. Carries the freshly-inserted
// `knowledge_chunk` row UUID (the keep server's id) plus the
// `proposal_id` the notebook stamped on this promotion attempt so the
// caller can correlate the audit chain without re-reading.
type PromoteToKeepResult struct {
	// ChunkID is the freshly-inserted `watchkeeper.knowledge_chunk`
	// row UUID. Always populated on success.
	ChunkID string
	// ProposalID is the notebook-side proposal id stamped at read
	// time. Distinct from ChunkID — the proposal exists per
	// PromoteToKeep call, the chunk per successful keep insert.
	ProposalID string
	// NotebookEntryID is the source entry id, echoed back from the
	// request so callers correlating multi-step flows do not have
	// to thread it through.
	NotebookEntryID string
}

// PromoteToKeep validates the lead-approval gate stack, reads the
// notebook entry into a [notebook.Proposal] (pre-emit short-circuit),
// emits the `requested` audit row, calls
// [WatchmasterWriteClient.Store] to insert the keep row, then emits
// the `notebook_promoted_to_keep` domain event AND
// `watchmaster_promote_to_keep_succeeded` security audit row. On Store
// failure, emits `watchmaster_promote_to_keep_failed` instead and
// returns the wrapped error.
//
// Resolution order:
//
//  1. Validate ctx (cancellation takes precedence over input shape).
//  2. Validate client + logger non-nil → [ErrInvalidRequest].
//  3. Validate notebookClient non-nil → [ErrInvalidRequest].
//  4. Validate claim.OrganizationID non-empty (M3.5.a discipline) →
//     [ErrInvalidClaim].
//  5. Validate claim.AuthorityMatrix["promote_to_keep"] equals
//     "lead_approval" → [ErrUnauthorized] otherwise.
//  6. Validate req.NotebookEntryID non-empty → [ErrInvalidRequest].
//  7. Validate req.ApprovalToken non-empty → [ErrApprovalRequired].
//  8. Read the notebook entry via notebookClient.PromoteToKeep. On
//     failure return a wrapped error WITHOUT emitting any audit row
//     from this tool (pre-emit short-circuit). Note: the underlying
//     notebook.PromoteToKeep emits its own
//     `notebook_promotion_proposed` event ON SUCCESS only; a read
//     failure leaves no audit row at all.
//  9. Emit `watchmaster_promote_to_keep_requested` keepers_log event.
//  10. Call client.Store with the proposal's Subject/Content/Embedding.
//  11. On success emit `notebook_promoted_to_keep` (domain event) AND
//     `watchmaster_promote_to_keep_succeeded` (security audit). On
//     failure emit `watchmaster_promote_to_keep_failed`.
func PromoteToKeep(
	ctx context.Context,
	client WatchmasterWriteClient,
	notebookClient WatchmasterNotebookClient,
	logger keepersLogAppender,
	req PromoteToKeepRequest,
	claim Claim,
) (PromoteToKeepResult, error) {
	if err := ctx.Err(); err != nil {
		return PromoteToKeepResult{}, err
	}
	if err := validateClaimAndDeps(client, logger, claim); err != nil {
		return PromoteToKeepResult{}, err
	}
	if notebookClient == nil {
		return PromoteToKeepResult{}, fmt.Errorf("%w: nil notebook client", ErrInvalidRequest)
	}
	if !claim.HasAuthority(authorityActionPromoteToKeep) {
		return PromoteToKeepResult{}, ErrUnauthorized
	}
	if req.NotebookEntryID == "" {
		return PromoteToKeepResult{}, fmt.Errorf("%w: empty NotebookEntryID", ErrInvalidRequest)
	}
	if req.ApprovalToken == "" {
		return PromoteToKeepResult{}, ErrApprovalRequired
	}

	// Step 8: pre-emit short-circuit — read the notebook entry first.
	// A failure here aborts BEFORE the audit chain begins (no
	// `requested` row was written). Surface wrapped without emitting
	// any audit row — consistent with the M6.1.b secrets-source and
	// M6.2.b GetManifest short-circuit patterns.
	proposal, err := notebookClient.PromoteToKeep(ctx, req.NotebookEntryID)
	if err != nil {
		return PromoteToKeepResult{}, fmt.Errorf("spawn: promote_to_keep load_proposal: %w", err)
	}

	agentID := pickAgentForBump(req.AgentID, claim)

	// Step 9: audit-requested.
	if _, err := logger.Append(ctx, promoteRequestedEvent(agentID, req.NotebookEntryID, proposal.ProposalID)); err != nil {
		return PromoteToKeepResult{}, fmt.Errorf("spawn: keepers_log requested: %w", err)
	}

	// Step 10: keep write.
	resp, err := client.Store(ctx, keepclient.StoreRequest{
		Subject:   proposal.Subject,
		Content:   proposal.Content,
		Embedding: proposal.Embedding,
	})
	if err != nil {
		_, _ = logger.Append(ctx, promoteFailedEvent(agentID, req.NotebookEntryID, proposal.ProposalID, err))
		return PromoteToKeepResult{}, fmt.Errorf("spawn: promote_to_keep store: %w", err)
	}

	// Step 11: domain event + audit-succeeded.
	promotedAt := time.Now().UnixMilli()
	_, _ = logger.Append(ctx, notebookPromotedEvent(agentID, req.NotebookEntryID, resp.ID, proposal, promotedAt))
	_, _ = logger.Append(ctx, promoteSucceededEvent(agentID, req.NotebookEntryID, resp.ID, proposal.ProposalID))

	return PromoteToKeepResult{
		ChunkID:         resp.ID,
		ProposalID:      proposal.ProposalID,
		NotebookEntryID: req.NotebookEntryID,
	}, nil
}

// promoteRequestedEvent / promoteSucceededEvent / promoteFailedEvent /
// notebookPromotedEvent build the audit-chain events for the
// promote_to_keep tool. Mirror the M6.1.b / M6.2.b / M6.2.c helper
// layout — shared payload keys (agent_id, tool_name, event_class) come
// first, per-event keys come next.
//
// PII discipline: NONE of these helpers reads `Content` or `Embedding`
// off the proposal. The per-event payload keys are pinned in this file
// so a future drift surfaces as a compile error here, not a silent
// audit-log leak.

func promoteRequestedEvent(agentID, entryID, proposalID string) keeperslog.Event {
	return keeperslog.Event{
		EventType: keepersLogEventPromoteRequested,
		Payload: map[string]any{
			payloadKeyAgentID:              agentID,
			payloadKeyToolName:             payloadValuePromoteToKeep,
			payloadKeyEventClass:           payloadValueEventRequested,
			payloadKeyApprovalTokenPresent: true,
			payloadKeyNotebookEntryID:      entryID,
			payloadKeyProposalID:           proposalID,
		},
	}
}

func promoteSucceededEvent(agentID, entryID, chunkID, proposalID string) keeperslog.Event {
	return keeperslog.Event{
		EventType: keepersLogEventPromoteSucceeded,
		Payload: map[string]any{
			payloadKeyAgentID:         agentID,
			payloadKeyToolName:        payloadValuePromoteToKeep,
			payloadKeyEventClass:      payloadValueEventSucceeded,
			payloadKeyNotebookEntryID: entryID,
			payloadKeyProposalID:      proposalID,
			payloadKeyChunkID:         chunkID,
		},
	}
}

func promoteFailedEvent(agentID, entryID, proposalID string, err error) keeperslog.Event {
	return keeperslog.Event{
		EventType: keepersLogEventPromoteFailed,
		Payload: map[string]any{
			payloadKeyAgentID:         agentID,
			payloadKeyToolName:        payloadValuePromoteToKeep,
			payloadKeyEventClass:      payloadValueEventFailed,
			payloadKeyErrorClass:      classifyError(err),
			payloadKeyNotebookEntryID: entryID,
			payloadKeyProposalID:      proposalID,
		},
	}
}

// notebookPromotedEvent builds the `notebook_promoted_to_keep` DOMAIN
// event (NOT a security-audit row — see the package docblock and the
// notebook.promoteEventType doc for why the two are kept distinct).
// Carries provenance only — Subject + Category + Scope + the timestamps
// — and never `content` or `embedding`.
func notebookPromotedEvent(agentID, entryID, chunkID string, p *notebook.Proposal, promotedAt int64) keeperslog.Event {
	return keeperslog.Event{
		EventType: keepersLogEventNotebookPromoted,
		Payload: map[string]any{
			payloadKeyAgentID:         agentID,
			payloadKeyToolName:        payloadValuePromoteToKeep,
			payloadKeyNotebookEntryID: entryID,
			payloadKeyProposalID:      p.ProposalID,
			payloadKeyChunkID:         chunkID,
			payloadKeySubject:         p.Subject,
			payloadKeyCategory:        p.Category,
			payloadKeyScope:           p.Scope,
			payloadKeySourceCreatedAt: p.SourceCreatedAt,
			payloadKeyProposedAt:      p.ProposedAt,
			payloadKeyPromotedAt:      promotedAt,
		},
	}
}
