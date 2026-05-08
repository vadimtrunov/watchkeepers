package cards

import (
	"fmt"
	"strings"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// PromoteToKeepCardInput is the typed projection of a
// [notebook.Proposal] the renderer needs for the M6.2.d
// `promote_to_keep` lead-approval card. Held as a separate type so the
// renderer is decoupled from the notebook-package internal shape — a
// future field rename in `notebook.Proposal` does not force a card
// renderer churn.
//
// PII discipline: this struct INTENTIONALLY does NOT carry the source
// proposal's `Embedding` (the float32 vector lifted from
// `entry_vec`). The audit chain already excludes it (M2b.7 / M6.2.d
// payload-key allowlist), and the approval card surfaces only the
// human-readable provenance + a truncated content preview. Omitting
// the field at the type boundary makes the discipline a compile-time
// guarantee — there is no way to plumb embedding bytes into the card
// shape.
type PromoteToKeepCardInput struct {
	// AgentID is the calling agent's UUID — the source notebook owner.
	AgentID string
	// NotebookEntryID is the UUID v7 of the source `entry` row.
	NotebookEntryID string
	// Subject is the source entry's subject. Optional; empty renders
	// `_(no subject)_`.
	Subject string
	// Content is the textual body that would be promoted into
	// `watchkeeper.knowledge_chunk`. Required; empty content fails the
	// guard and returns `(nil, "")` (mirrors the AgentID guard — empty
	// content is a programmer bug per the notebook NOT NULL
	// constraint).
	Content string
	// Category mirrors the source entry's category — one of the five
	// values in the notebook category enum.
	Category string
	// Scope governs the visibility of the (eventually-persisted) Keep
	// row. One of [notebook.ScopeOrg], `notebook.ScopeUserPrefix +
	// "<uuid>"`, or `notebook.ScopeAgentPrefix + "<uuid>"`. The `org`
	// scope triggers a prominent visibility-warning block; narrower
	// scopes are surfaced in the context line.
	Scope string
	// SourceCreatedAt is the source entry's `created_at` (epoch ms)
	// echoed for provenance display. Not currently rendered in the
	// body — reserved so the renderer can grow a second context line
	// without a struct churn.
	SourceCreatedAt int64
	// ProposedAt is the moment of promotion (epoch ms) the notebook
	// stamped at proposal time. Same provenance role as
	// SourceCreatedAt above.
	ProposedAt int64
	// ApprovalToken is the opaque token the lead-approval saga minted
	// at request time. Threaded into the action_id so the inbound
	// dispatcher can correlate the button click back to the
	// pending_approvals row without reading the request body. Never
	// surfaced in full — the context line shows only the truncated
	// prefix via [tokenPrefix].
	ApprovalToken string
}

// content-preview truncation budgets. The card surface caps the
// subject at 150 runes and the content excerpt at 500 runes; both
// over-budget bodies are tail-clipped with a single `…` rune. Slack
// section blocks themselves accept up to ~3000 chars of mrkdwn — the
// 500-rune cap keeps the card scannable in the lead's DM stream.
const (
	promoteSubjectMaxRunes = 150
	promoteContentMaxRunes = 500
)

// noSubjectPlaceholder is the mrkdwn-italic phrase rendered when the
// proposal has no subject. Distinct from [fallback]'s `_(empty)_`
// because the lead-approval card spells out the absent field for
// clarity ("no subject" vs "empty value").
const noSubjectPlaceholder = "_(no subject)_"

// scopeWarningOrg is the prominent mrkdwn line surfaced in block 3
// when `scope == notebook.ScopeOrg`. Operators reading the card need
// to know at a glance that approving will publish to every
// Watchkeeper in the org; narrower scopes (`user:` / `agent:`) carry
// no such warning and instead get a compact context-line entry.
const scopeWarningOrg = ":warning: This entry will become visible to all Watchkeepers in the org."

// RenderPromoteToKeep builds the approval card blocks for a
// `promote_to_keep` invocation plus the matching action_id. Returns
// nil blocks + empty action_id when AgentID, ApprovalToken, or
// Content is empty (programmer bug — every M6.2.d invocation sets all
// three before the saga posts the card).
//
// Block layout (AC3):
//
//  1. Header: "Approve promote-to-keep?"
//  2. Section markdown: tool / agent_id / notebook_entry_id /
//     category / scope.
//  3. Section markdown: scope-visibility warning, only when
//     `scope == notebook.ScopeOrg`.
//  4. Section markdown: subject + truncated content preview, both
//     wrapped in a fenced code block so embedded user mrkdwn does not
//     leak into the card chrome.
//  5. Action buttons (Approve / Reject) via [actionButtons].
//  6. Context line carrying the truncated approval token via
//     [tokenPrefix]. Narrower scopes also surface their full scope
//     string here.
func RenderPromoteToKeep(in PromoteToKeepCardInput) (blocks []Block, actionID string) {
	if in.AgentID == "" || in.ApprovalToken == "" || in.Content == "" {
		return nil, ""
	}
	actionID = EncodeActionID(spawn.PendingApprovalToolPromoteToKeep, in.ApprovalToken)

	body := fmt.Sprintf(
		"*Tool*: `promote_to_keep`\n*Agent ID*: `%s`\n*Notebook Entry*: `%s`\n*Category*: %s\n*Scope*: `%s`",
		in.AgentID,
		fallback(in.NotebookEntryID),
		fallback(in.Category),
		fallback(in.Scope),
	)

	blocks = append(
		blocks,
		headerBlock("Approve promote-to-keep?"),
		sectionMarkdown(body),
	)

	// Block 3: scope warning ONLY for org-wide visibility. Narrower
	// scopes carry no warning here; they surface in the context line.
	if in.Scope == notebook.ScopeOrg {
		blocks = append(blocks, sectionMarkdown(scopeWarningOrg))
	}

	blocks = append(
		blocks,
		sectionMarkdown(promoteContentPreview(in.Subject, in.Content)),
		actionButtons(actionID),
		contextLine(promoteContextText(in.Scope, in.ApprovalToken)),
	)

	return blocks, actionID
}

// promoteContentPreview returns the subject + truncated content body
// rendered as a single mrkdwn section. Subject capped at
// [promoteSubjectMaxRunes]; content capped at [promoteContentMaxRunes].
// Both are wrapped in a fenced code block so embedded mrkdwn in the
// user content (asterisks, backticks, links) is rendered verbatim by
// Slack rather than interpreted as card formatting.
func promoteContentPreview(subject, content string) string {
	subjectDisplay := noSubjectPlaceholder
	if subject != "" {
		subjectDisplay = truncateRunes(subject, promoteSubjectMaxRunes)
	}
	contentDisplay := truncateRunes(content, promoteContentMaxRunes)

	var b strings.Builder
	b.WriteString("*Subject*: ")
	b.WriteString(subjectDisplay)
	b.WriteString("\n*Content (preview)*\n```\n")
	b.WriteString(contentDisplay)
	b.WriteString("\n```")
	return b.String()
}

// promoteContextText returns the context-line mrkdwn for the card
// footer. Always carries the truncated approval-token prefix; for
// narrower scopes (`user:<uuid>` / `agent:<uuid>`) it ALSO carries
// the full scope string so operators auditing the row know the exact
// visibility binding without re-opening the underlying entry.
func promoteContextText(scope, approvalToken string) string {
	tok := tokenPrefix(approvalToken)
	if scope == "" || scope == notebook.ScopeOrg {
		return fmt.Sprintf("approval_token: `%s`", tok)
	}
	return fmt.Sprintf("Scope: `%s` · approval_token: `%s`", scope, tok)
}

// truncateRunes returns `s` clipped to at most `maxRunes` runes,
// suffixed with a single `…` when truncation occurred. Returns the
// empty string verbatim. Hoisted as a package-level helper so future
// renderers can reuse the same truncation discipline as
// [systemPromptPreview]. The parameter is named `maxRunes` (not
// `max`) so we do not shadow the Go 1.21 built-in.
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 || s == "" {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}
