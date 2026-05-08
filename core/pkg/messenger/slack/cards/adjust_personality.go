package cards

import (
	"fmt"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// AdjustPersonalityCardInput is the typed projection of an
// [spawn.AdjustPersonalityRequest] + the existing personality value
// the renderer needs to draw the diff block. The caller (M6.3.c
// outbound poster) is responsible for fetching `OldPersonality` from
// the latest manifest_version before drawing the card; the renderer
// is pure.
type AdjustPersonalityCardInput struct {
	// AgentID is the existing watchkeeper's manifest UUID.
	AgentID string
	// OldPersonality is the personality text on the latest
	// manifest_version (loaded by the caller before rendering). Empty
	// is allowed — it round-trips as `(empty)` in the diff block.
	OldPersonality string
	// NewPersonality is the proposed personality text the request
	// carries.
	NewPersonality string
	// ApprovalToken is the opaque token the lead-approval saga
	// minted. Threaded into the action_id.
	ApprovalToken string
}

// RenderAdjustPersonality builds the approval card blocks for an
// `adjust_personality` invocation. Includes a 3-5-line old-vs-new
// diff section per AC3.
func RenderAdjustPersonality(in AdjustPersonalityCardInput) (blocks []Block, actionID string) {
	if in.AgentID == "" || in.ApprovalToken == "" {
		return nil, ""
	}
	actionID = EncodeActionID(spawn.PendingApprovalToolAdjustPersonality, in.ApprovalToken)
	header := headerBlock("Approve personality change?")
	body := sectionMarkdown(fmt.Sprintf(
		"*Tool*: `adjust_personality`\n*Agent ID*: `%s`",
		in.AgentID,
	))
	diff := sectionMarkdown(diffLines("Personality", in.OldPersonality, in.NewPersonality))
	return []Block{
		header,
		body,
		diff,
		actionButtons(actionID),
		contextLine(fmt.Sprintf("approval_token: `%s`", in.ApprovalToken)),
	}, actionID
}
