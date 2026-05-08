package cards

import (
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// RetireWatchkeeperCardInput is the typed projection of a
// [spawn.RetireWatchkeeperRequest] the renderer needs. Retire is a
// status-row mutation, not a manifest bump — there is no diff block;
// the card surfaces only the watchkeeper id + a clear "this will flip
// the row to retired" headline.
type RetireWatchkeeperCardInput struct {
	// AgentID is the existing watchkeeper row id.
	AgentID string
	// DisplayName is the optional human-readable name the caller may
	// inject so the card reads naturally. Empty is allowed; the card
	// shows the AgentID alone.
	DisplayName string
	// ApprovalToken is the opaque token the lead-approval saga
	// minted. Threaded into the action_id.
	ApprovalToken string
}

// RenderRetireWatchkeeper builds the approval card blocks for a
// `retire_watchkeeper` invocation.
func RenderRetireWatchkeeper(in RetireWatchkeeperCardInput) (blocks []Block, actionID string) {
	if in.AgentID == "" || in.ApprovalToken == "" {
		return nil, ""
	}
	actionID = EncodeActionID(spawn.PendingApprovalToolRetireWatchkeeper, in.ApprovalToken)
	header := headerBlock("Approve Watchkeeper retirement?")
	bodyText := fmt.Sprintf(
		"*Tool*: `retire_watchkeeper`\n*Agent ID*: `%s`",
		in.AgentID,
	)
	if in.DisplayName != "" {
		bodyText += fmt.Sprintf("\n*Display name*: %s", in.DisplayName)
	}
	bodyText += "\n_Approving will flip the watchkeeper status from `active` to `retired`. The transition is one-way._"
	return []Block{
		header,
		sectionMarkdown(bodyText),
		actionButtons(actionID),
		contextLine(fmt.Sprintf("approval_token: `%s`", in.ApprovalToken)),
	}, actionID
}
