package cards

import (
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// AdjustLanguageCardInput is the typed projection of an
// [spawn.AdjustLanguageRequest] + the existing language value the
// renderer needs to draw the diff block.
type AdjustLanguageCardInput struct {
	// AgentID is the existing watchkeeper's manifest UUID.
	AgentID string
	// OldLanguage is the language code on the latest
	// manifest_version. Empty is allowed.
	OldLanguage string
	// NewLanguage is the proposed language code.
	NewLanguage string
	// ApprovalToken is the opaque token the lead-approval saga
	// minted. Threaded into the action_id.
	ApprovalToken string
}

// RenderAdjustLanguage builds the approval card blocks for an
// `adjust_language` invocation. Includes a 3-5-line old-vs-new diff
// section per AC3.
func RenderAdjustLanguage(in AdjustLanguageCardInput) (blocks []Block, actionID string) {
	if in.AgentID == "" || in.ApprovalToken == "" {
		return nil, ""
	}
	actionID = EncodeActionID(spawn.PendingApprovalToolAdjustLanguage, in.ApprovalToken)
	header := headerBlock("Approve language change?")
	body := sectionMarkdown(fmt.Sprintf(
		"*Tool*: `adjust_language`\n*Agent ID*: `%s`",
		in.AgentID,
	))
	diff := sectionMarkdown(diffLines("Language", in.OldLanguage, in.NewLanguage))
	return []Block{
		header,
		body,
		diff,
		actionButtons(actionID),
		contextLine(fmt.Sprintf("approval_token: `%s`", in.ApprovalToken)),
	}, actionID
}
