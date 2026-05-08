// Package cards renders Slack Block Kit approval cards for the
// Watchmaster lead-approval saga (M6.3.b). Each renderer takes a typed
// input struct (a snapshot of the M6.2.x tool request the Watchmaster
// just emitted) and returns:
//
//  1. A `[]Block` slice the caller posts to Slack via the existing
//     outbound messenger (M6.3.c will own the outbound posting; M6.3.b
//     ships the renderer in isolation).
//  2. The opaque `action_id` string both Approve / Reject buttons
//     carry. The format is `approval:<tool>:<token>`; the matching
//     [DecodeActionID] helper round-trips it into a `(tool, token)`
//     tuple the inbound dispatcher branches on.
//
// All renderers are PURE FUNCTIONS — no I/O, no clock, no dependency
// injection. The caller is responsible for picking the M6.2.x request
// snapshot (typically the same struct the tool itself received) and
// for posting the resulting blocks to Slack.
//
// Block Kit shape:
//
//   - One `header` block carrying a tool-specific title.
//   - One or more `section` blocks carrying the request fields. The
//     `adjust_personality` and `adjust_language` cards include a
//     three-to-five-line old-vs-new diff section per AC3.
//   - One `actions` block holding two `button` elements: Approve
//     (style="primary") and Reject (style="danger"). Both buttons
//     share the same action_id payload — the inbound dispatcher
//     branches on the `value` field (`approved` | `rejected`).
//
// The package depends only on encoding/json + the Go stdlib + the
// in-repo spawn package (for the closed-set tool-name constants).
// Specifically: no third-party Slack SDK, mirroring the parent
// messenger/slack stdlib-only discipline (M4.2.a).
package cards

import (
	"errors"
	"fmt"
	"strings"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// Block is the Slack Block Kit shape this package emits. The Block Kit
// surface is documented at <https://api.slack.com/block-kit>;
// production callers marshal the slice with [encoding/json] and POST
// the resulting array to `chat.postMessage` (or any
// `blocks`-compatible payload). The package keeps the Block / Element
// types as typed structs so callers cannot trip on a typo'd tag and so
// the inbound dispatcher's contract stays compile-checked.
type Block struct {
	Type     string    `json:"type"`
	Text     *Text     `json:"text,omitempty"`
	BlockID  string    `json:"block_id,omitempty"`
	Fields   []Text    `json:"fields,omitempty"`
	Elements []Element `json:"elements,omitempty"`
}

// Text is the Block Kit `text` object. Either `plain_text` or
// `mrkdwn`; the renderer picks per-block.
type Text struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Emoji bool   `json:"emoji,omitempty"`
}

// Element is a Block Kit interactive element. M6.3.b emits only
// `button` elements; the type is generic so a future M6.3.c extension
// (overflow menus, view-open buttons) can compose without a re-shape.
type Element struct {
	Type     string `json:"type"`
	Text     *Text  `json:"text,omitempty"`
	ActionID string `json:"action_id,omitempty"`
	Value    string `json:"value,omitempty"`
	Style    string `json:"style,omitempty"`
}

// Block Kit type tags. Hoisted so the renderers and the dispatcher
// share one source of truth for the on-wire vocabulary.
const (
	blockTypeHeader  = "header"
	blockTypeSection = "section"
	blockTypeActions = "actions"
	blockTypeContext = "context"

	textTypePlain  = "plain_text"
	textTypeMrkdwn = "mrkdwn"

	elementTypeButton = "button"

	buttonStylePrimary = "primary"
	buttonStyleDanger  = "danger"
)

// Button-value vocabulary. The dispatcher decodes `value` to pick the
// terminal state to write into the pending_approvals row.
const (
	ButtonValueApprove = "approved"
	ButtonValueReject  = "rejected"
)

// actionIDPrefix + actionIDSeparator make the action_id format stable.
// Format: `approval:<tool>:<token>`; the prefix lets the dispatcher
// reject foreign action_ids (e.g. from a non-approval card) without
// parsing the rest. The separator is a single colon — chosen because
// neither the closed-set tool names nor the M6.2.x approval tokens
// (UUIDv7 strings in the M6.3.c envisioned implementation) carry a
// raw colon.
const (
	actionIDPrefix    = "approval"
	actionIDSeparator = ":"
)

// ErrInvalidActionID is the typed error [DecodeActionID] returns when
// the input does not parse as `approval:<tool>:<token>`. Matchable via
// [errors.Is]. The inbound dispatcher's audit chain emits
// `reason=malformed_action_id` for any value matching this error.
var ErrInvalidActionID = errors.New("cards: invalid action_id")

// EncodeActionID composes the opaque action_id payload the Approve /
// Reject buttons carry. Returns the empty string when either input is
// empty — the caller (always one of this package's renderers) treats
// an empty result as a programmer bug, not a runtime branch.
//
// Format: `approval:<tool>:<token>`. The token field is opaque from
// this package's perspective: it round-trips bytes-for-bytes through
// [DecodeActionID].
func EncodeActionID(tool, token string) string {
	if tool == "" || token == "" {
		return ""
	}
	return actionIDPrefix + actionIDSeparator + tool + actionIDSeparator + token
}

// DecodeActionID parses a button action_id back into `(tool, token)`.
// Returns [ErrInvalidActionID] when the input does not have the
// `approval:<tool>:<token>` shape, when the prefix mismatches, when
// either field is empty, or when the tool name is not one of the four
// closed-set values from spawn.PendingApprovalTool*.
//
// Strict validation here lets the dispatcher's audit chain pin
// `reason=malformed_action_id` to a single check: a value that
// survives this function is guaranteed to route to one of the four
// known tool branches.
func DecodeActionID(actionID string) (tool string, token string, err error) {
	parts := strings.SplitN(actionID, actionIDSeparator, 3)
	if len(parts) != 3 {
		return "", "", fmt.Errorf("%w: expected 3 parts, got %d", ErrInvalidActionID, len(parts))
	}
	if parts[0] != actionIDPrefix {
		return "", "", fmt.Errorf("%w: bad prefix %q", ErrInvalidActionID, parts[0])
	}
	if parts[1] == "" {
		return "", "", fmt.Errorf("%w: empty tool", ErrInvalidActionID)
	}
	if parts[2] == "" {
		return "", "", fmt.Errorf("%w: empty token", ErrInvalidActionID)
	}
	if !isKnownTool(parts[1]) {
		return "", "", fmt.Errorf("%w: unknown tool %q", ErrInvalidActionID, parts[1])
	}
	return parts[1], parts[2], nil
}

// isKnownTool reports whether the tool name is one of the four
// closed-set values M6.3.b supports. Pinned against the spawn-package
// constants so a re-key of the harness builtin tool registry surfaces
// here as a compile error.
func isKnownTool(tool string) bool {
	switch tool {
	case spawn.PendingApprovalToolProposeSpawn,
		spawn.PendingApprovalToolAdjustPersonality,
		spawn.PendingApprovalToolAdjustLanguage,
		spawn.PendingApprovalToolRetireWatchkeeper:
		return true
	}
	return false
}

// headerBlock returns a single header block carrying `title`. Hoisted
// because all four renderers emit one.
func headerBlock(title string) Block {
	return Block{
		Type: blockTypeHeader,
		Text: &Text{Type: textTypePlain, Text: title, Emoji: true},
	}
}

// sectionMarkdown returns a `section` block carrying mrkdwn-formatted
// `text`. Used for both per-field rows and the diff block.
func sectionMarkdown(text string) Block {
	return Block{
		Type: blockTypeSection,
		Text: &Text{Type: textTypeMrkdwn, Text: text},
	}
}

// actionButtons returns the trailing two-button actions block. Both
// buttons carry the same `actionID` payload; the dispatcher branches
// on the `value` field.
func actionButtons(actionID string) Block {
	return Block{
		Type: blockTypeActions,
		Elements: []Element{
			{
				Type:     elementTypeButton,
				Text:     &Text{Type: textTypePlain, Text: "Approve", Emoji: true},
				ActionID: actionID,
				Value:    ButtonValueApprove,
				Style:    buttonStylePrimary,
			},
			{
				Type:     elementTypeButton,
				Text:     &Text{Type: textTypePlain, Text: "Reject", Emoji: true},
				ActionID: actionID,
				Value:    ButtonValueReject,
				Style:    buttonStyleDanger,
			},
		},
	}
}

// contextLine returns a context block with a single mrkdwn line.
// Hoisted so the per-tool footers stay scannable.
func contextLine(text string) Block {
	return Block{
		Type: blockTypeContext,
		Elements: []Element{
			{Type: textTypeMrkdwn, Text: &Text{Type: textTypeMrkdwn, Text: text}},
		},
	}
}

// diffLines returns the canonical 3-5 line old-vs-new diff section
// the AdjustPersonality / AdjustLanguage cards embed per AC3. Format:
//
//	*Field*: old → new
//	```
//	- <old>
//	+ <new>
//	```
//
// Falls back to `(empty)` for either side when the supplied value is
// the empty string.
func diffLines(field, oldValue, newValue string) string {
	const placeholder = "(empty)"
	if oldValue == "" {
		oldValue = placeholder
	}
	if newValue == "" {
		newValue = placeholder
	}
	var b strings.Builder
	b.WriteString("*")
	b.WriteString(field)
	b.WriteString("*\n")
	b.WriteString("```\n")
	b.WriteString("- ")
	b.WriteString(oldValue)
	b.WriteString("\n")
	b.WriteString("+ ")
	b.WriteString(newValue)
	b.WriteString("\n")
	b.WriteString("```")
	return b.String()
}
