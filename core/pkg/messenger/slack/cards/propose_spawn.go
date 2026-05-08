package cards

import (
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// ProposeSpawnCardInput is the typed projection of a
// [spawn.ProposeSpawnRequest] the renderer needs. Held as a separate
// type so the renderer is decoupled from the spawn-package internal
// shape — a future field rename in `ProposeSpawnRequest` does not
// force a card-renderer churn.
type ProposeSpawnCardInput struct {
	// AgentID is the freshly-allocated manifest UUID the new
	// watchkeeper will be pinned to.
	AgentID string
	// Personality is the proposed free-text personality (≤ 1024 runes).
	Personality string
	// Language is the proposed language code (BCP 47-lite shape).
	Language string
	// SystemPrompt is the manifest system prompt text. The card shows
	// only the first ~200 runes of the prompt; long prompts are
	// truncated with an ellipsis. PII discipline: the system prompt
	// is operator-supplied content, not user PII, so showing the head
	// is safe.
	SystemPrompt string
	// ApprovalToken is the opaque token the lead-approval saga
	// minted at request time. Threaded into the action_id so the
	// inbound dispatcher can correlate the button click back to the
	// pending_approvals row without reading the request body.
	ApprovalToken string
}

// RenderProposeSpawn builds the approval card blocks for a
// `propose_spawn` invocation plus the matching action_id. Returns nil
// blocks + empty action_id when AgentID or ApprovalToken is empty
// (programmer bug — every M6.2.x tool sets both before posting the
// card).
func RenderProposeSpawn(in ProposeSpawnCardInput) (blocks []Block, actionID string) {
	if in.AgentID == "" || in.ApprovalToken == "" {
		return nil, ""
	}
	actionID = EncodeActionID(spawn.PendingApprovalToolProposeSpawn, in.ApprovalToken)
	body := fmt.Sprintf(
		"*Tool*: `propose_spawn`\n*Agent ID*: `%s`\n*Personality*: %s\n*Language*: %s",
		in.AgentID,
		fallback(in.Personality),
		fallback(in.Language),
	)
	blocks = []Block{
		headerBlock("Approve new Watchkeeper spawn?"),
		sectionMarkdown(body),
		sectionMarkdown(systemPromptPreview(in.SystemPrompt)),
		actionButtons(actionID),
		contextLine(fmt.Sprintf("approval_token: `%s`", in.ApprovalToken)),
	}
	return blocks, actionID
}

// systemPromptPreview returns a Slack mrkdwn block previewing the
// first 200 runes of the system prompt. Long prompts are truncated
// with `…`. Empty prompts show `(empty)`.
func systemPromptPreview(prompt string) string {
	const previewMaxRunes = 200
	if prompt == "" {
		return "*System prompt*: _(empty)_"
	}
	display := prompt
	runes := []rune(prompt)
	if len(runes) > previewMaxRunes {
		display = string(runes[:previewMaxRunes]) + "…"
	}
	return "*System prompt (preview)*\n```\n" + display + "\n```"
}

// fallback returns `(empty)` for an empty string. Hoisted so all
// renderers share the placeholder.
func fallback(s string) string {
	if s == "" {
		return "_(empty)_"
	}
	return s
}
