package toolshare

import "time"

// TopicToolShareProposed is the [eventbus.Bus] topic [Sharer.Share]
// emits BEFORE invoking any github write. Indicates the share
// orchestrator has validated the request, resolved the proposer +
// target, and is about to begin the github branch / file / PR
// chain. M9.7's audit subscriber surfaces a `tool_share_proposed`
// row from this event.
//
// The `toolshare.` namespace prefix mirrors the M9 package-prefix
// discipline (`localpatch.local_patch_applied`,
// `hostedexport.hosted_tool_exported`, `approval.tool_proposed`).
//
// Iter-1 m4 fix (reviewer B): a [TopicToolShareProposed] event
// without a matching [TopicToolSharePROpened] event indicates the
// github phase failed (GetRef / CreateRef / CreateOrUpdateFile /
// CreatePullRequest) OR the ctx was cancelled before
// [TopicToolSharePROpened] could fire. Audit subscribers that join
// by [ToolShareProposed.CorrelationID] MUST tolerate orphan rows.
// A future M9.7 `tool_share_aborted` topic would close this gap;
// for M9.6 the audit subscriber's "is this proposed still
// pending" question has no terminal counterpart.
const TopicToolShareProposed = "toolshare.tool_share_proposed"

// TopicToolSharePROpened is the [eventbus.Bus] topic [Sharer.Share]
// emits AFTER the github PR creation has succeeded. Carries the
// PR number + HTML URL the operator (and the M9.7 audit
// subscriber) needs to join back to the proposed event.
const TopicToolSharePROpened = "toolshare.tool_share_pr_opened"

// ToolShareProposed is the payload published on
// [TopicToolShareProposed]. Metadata + accountability fields only.
//
// PII discipline (see also the package doc):
//
//   - `SourceName` / `ToolName` / `ToolVersion` are public
//     identifiers.
//   - `ProposerID` is the resolved stable public identifier of the
//     proposing agent / human — bounded, allowlisted (see
//     [ErrInvalidProposerID]).
//   - `Reason` is agent-supplied free-form audit text — verbatim,
//     bounded [MaxReasonLength]. Mirrors
//     `localpatch.LocalPatchApplied.Reason`.
//   - `TargetOwner` / `TargetRepo` / `TargetBase` identify the
//     target git repository / base branch.
//   - `TargetSource` is the closed-set [TargetSource] discriminator
//     (`platform` | `private`).
//   - `ProposedAt` is the [Clock] timestamp captured AFTER the
//     proposer/target resolution AND BEFORE the github calls.
//   - `CorrelationID` is process-monotonic. Subscribers MUST NOT
//     parse it. The SAME correlation id is reused for the matching
//     [TopicToolSharePROpened] event so the audit subscriber can
//     join the two rows.
//
// Tool source code, the on-disk live path, the GitHub token, and
// the operator-supplied Slack channel id NEVER land on the payload.
//
//nolint:revive // Type name parity with TopicToolShareProposed.
type ToolShareProposed struct {
	SourceName    string
	ToolName      string
	ToolVersion   string
	ProposerID    string
	Reason        string
	TargetOwner   string
	TargetRepo    string
	TargetBase    string
	TargetSource  TargetSource
	ProposedAt    time.Time
	CorrelationID string
}

// ToolSharePROpened is the payload published on
// [TopicToolSharePROpened] after the github calls have committed
// the share branch + opened the PR.
//
// Carries the PR coordinates in addition to the identifiers from
// [ToolShareProposed]. `CorrelationID` matches the prior
// [TopicToolShareProposed] event's value verbatim so a downstream
// join is exact.
//
// Iter-1 m8 fix (reviewer B): the `Reason` field is deliberately
// OMITTED from this payload (asymmetric with [ToolShareProposed]).
// An audit subscriber that needs the reason joins on
// [CorrelationID] against the prior `tool_share_proposed` row.
// Single-source-of-truth: the agent-supplied audit text exists
// ONCE in the audit chain, on the proposed event.
//
//nolint:revive // Type name parity with TopicToolSharePROpened.
type ToolSharePROpened struct {
	SourceName    string
	ToolName      string
	ToolVersion   string
	ProposerID    string
	TargetOwner   string
	TargetRepo    string
	TargetBase    string
	TargetSource  TargetSource
	PRNumber      int
	PRHTMLURL     string
	OpenedAt      time.Time
	CorrelationID string
}

// newToolShareProposedEvent constructs the proposed-event payload.
// Centralised mapper: a future addition to the payload flows
// through this constructor, and the reflection-based allowlist
// test on [ToolShareProposed] catches silent drift.
func newToolShareProposedEvent(
	sourceName, toolName, toolVersion, proposerID, reason string,
	target ResolvedTarget,
	proposedAt time.Time,
	correlationID string,
) ToolShareProposed {
	return ToolShareProposed{
		SourceName:    sourceName,
		ToolName:      toolName,
		ToolVersion:   toolVersion,
		ProposerID:    proposerID,
		Reason:        reason,
		TargetOwner:   target.Owner,
		TargetRepo:    target.Repo,
		TargetBase:    target.Base,
		TargetSource:  target.Source,
		ProposedAt:    proposedAt,
		CorrelationID: correlationID,
	}
}

// newToolSharePROpenedEvent constructs the pr-opened-event payload.
func newToolSharePROpenedEvent(
	sourceName, toolName, toolVersion, proposerID string,
	target ResolvedTarget,
	prNumber int, prHTMLURL string,
	openedAt time.Time,
	correlationID string,
) ToolSharePROpened {
	return ToolSharePROpened{
		SourceName:    sourceName,
		ToolName:      toolName,
		ToolVersion:   toolVersion,
		ProposerID:    proposerID,
		TargetOwner:   target.Owner,
		TargetRepo:    target.Repo,
		TargetBase:    target.Base,
		TargetSource:  target.Source,
		PRNumber:      prNumber,
		PRHTMLURL:     prHTMLURL,
		OpenedAt:      openedAt,
		CorrelationID: correlationID,
	}
}
