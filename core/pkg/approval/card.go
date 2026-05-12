// Doc-block at file head documenting the seam contract.
//
// resolution order: input validation (ErrCardMissingInput) → render
// each block in fixed order (header → metadata section → plain-
// language-description section → capability translation section → gate
// result section → risk badge section → 4-button actions block →
// context line with proposal-id prefix) → return ([]Block, action_id).
//
// audit discipline: the renderer never imports `keeperslog` and never
// calls `.Append(` (see source-grep AC). The renderer is a PURE
// FUNCTION — no I/O, no clock, no dependency injection beyond the
// optional [CapabilityTranslator] seam. The audit log entry for the
// card-rendered branch lives in the M9.7 audit subscriber observing
// the (future) `tool_approval_card_rendered` topic; M9.4.b leaves
// that emission to the orchestrator.
//
// PII discipline: the renderer is fed [CardInput] which DOES
// carry `PlainLanguageDescription` (rendered into the card body for
// the lead) but NEVER `Purpose` or `CodeDraft`. The
// `PlainLanguageDescription` is fenced in a code block so embedded
// mrkdwn (asterisks, backticks, mentions) is rendered verbatim rather
// than interpreted as card formatting — mirrors the M6.3.c
// `promote_to_keep` discipline. The action_id encodes only the
// proposal id + button name (no proposer id, no tool name).
//
// Block-kit shape:
//
//   - Header block: "Approve tool proposal?"
//   - Section block: tool / proposer / target_source metadata
//   - Section block: fenced plain-language description (preview)
//   - Section block: capability translations (one bullet per cap)
//   - Section block: gate results (one line per gate w/ emoji)
//   - Section block: risk-level badge
//   - Actions block: 4 buttons (Approve / Reject / Test in my DM /
//     Ask questions) all carrying the SAME `action_id` payload; the
//     dispatcher branches on `value` decoded back into [ButtonAction].
//   - Context block: truncated proposal id for operator-visible
//     correlation.

package approval

import (
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Block is the Block Kit shape this renderer emits. Mirrors the
// `messenger/slack/cards.Block` type but is redeclared here so the
// `approval` package does not depend on the `messenger/slack/cards`
// package's action_id codec (which is pinned to the M6.3 saga's
// closed-set tool vocabulary). Operators marshal the slice with
// [encoding/json] and POST it to Slack's `chat.postMessage`.
//
// `Elements` is `[]any` because the Block Kit `actions` block carries
// [Element] (button) values while the `context` block carries
// [ContextElement] (flat-text mrkdwn) values — the two element kinds
// have INCOMPATIBLE JSON shapes (context-element `text` is a string;
// action-element `text` is a nested [Text] object). A single typed
// slice would force one shape on both blocks and Slack's API would
// reject the resulting payload as malformed.
type Block struct {
	Type     string `json:"type"`
	Text     *Text  `json:"text,omitempty"`
	BlockID  string `json:"block_id,omitempty"`
	Fields   []Text `json:"fields,omitempty"`
	Elements []any  `json:"elements,omitempty"`
}

// Text is the Block Kit text object. Either `plain_text` or `mrkdwn`.
type Text struct {
	Type  string `json:"type"`
	Text  string `json:"text"`
	Emoji bool   `json:"emoji,omitempty"`
}

// Element is a Block Kit interactive element. M9.4.b emits only
// `button` elements (rendered inside an `actions` block). Slack's
// context block uses a DIFFERENT shape — see [ContextElement].
type Element struct {
	Type     string `json:"type"`
	Text     *Text  `json:"text,omitempty"`
	ActionID string `json:"action_id,omitempty"`
	Value    string `json:"value,omitempty"`
	Style    string `json:"style,omitempty"`
}

// ContextElement is the Block Kit context-block element shape: a flat
// `{"type":"mrkdwn","text":"..."}` object with `text` as a plain
// string (NOT a nested [Text] object — that nesting fails Slack API
// validation and drops the entire card payload, per Slack's Block
// Kit composition-object reference).
type ContextElement struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Block-kit vocabulary. Hoisted constants so the renderer and the
// dispatcher share one source of truth.
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

// ButtonAction is the closed-set vocabulary the approval card emits
// on its 4 buttons. The dispatcher decodes the `value` field to pick
// the action — see [DecodeActionID].
type ButtonAction string

const (
	// ButtonActionApprove fires [TopicToolApproved] via the
	// callback dispatcher with [RouteSlackNative].
	ButtonActionApprove ButtonAction = "approve"

	// ButtonActionReject fires [TopicToolRejected].
	ButtonActionReject ButtonAction = "reject"

	// ButtonActionTestInDM fires [TopicDryRunRequested]. The M9.4.c
	// dry-run executor subscribes and runs the proposed tool in
	// `scoped` dry-run mode forced to the lead's DM channel.
	ButtonActionTestInDM ButtonAction = "test_in_my_dm"

	// ButtonActionAskQuestions fires [TopicQuestionAsked].
	ButtonActionAskQuestions ButtonAction = "ask_questions"
)

// Validate reports whether `b` is in the closed [ButtonAction] set.
// Returns [ErrInvalidButtonValue] otherwise (including the empty
// string).
func (b ButtonAction) Validate() error {
	switch b {
	case ButtonActionApprove, ButtonActionReject, ButtonActionTestInDM, ButtonActionAskQuestions:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidButtonValue, string(b))
	}
}

// action_id namespace constants. Format:
// `tool_approval:<proposal_id>:<button>`. The `tool_approval` prefix
// lets the dispatcher reject foreign action_ids (e.g. from the M6.3
// spawn-approval card) without parsing the rest.
const (
	approvalActionIDPrefix    = "tool_approval"
	approvalActionIDSeparator = ":"
)

// CapabilityTranslator is the optional seam the renderer consumes to
// produce a human-readable line per capability id. Mirrors the
// per-call resolver discipline from M9.1.a / M9.4.a — production
// wiring satisfies it via a closure over M9.3's
// `dict/capabilities.yaml` (mandatory at M9.3 ship, but the renderer
// degrades gracefully when M9.3 is not yet wired).
//
// Contract:
//
//   - Return a non-empty translation on success. The returned string
//     is the human-readable line rendered after `<cap_id>: `.
//   - Return the empty string on miss. The renderer falls back to
//     `(no translation registered for <cap_id>)` so the lead can
//     still review the raw capability id.
//
// A nil [CapabilityTranslator] is the documented degradation path
// for "M9.3 not yet wired". Every cap renders as
// `<cap_id> (translation pending — M9.3)`.
type CapabilityTranslator func(capabilityID string) string

// CardInput is the typed projection of a [Proposal] +
// [ReviewResult] the renderer consumes. Held as a separate type so
// the renderer is decoupled from the proposer's / reviewer's
// internal shapes — a future field rename in either does not force a
// card-renderer churn.
//
// PII discipline (same as [Proposal] godoc): this struct INTENTIONALLY
// does NOT carry `Purpose` or `CodeDraft`. The
// `PlainLanguageDescription` is the only free-form body the renderer
// sees, and it is rendered fenced so embedded mrkdwn does not leak
// into card chrome.
type CardInput struct {
	// ProposalID is the durable proposal identifier — encoded into
	// the action_id payload so the callback dispatcher can look up
	// the full proposal record without re-parsing the card body.
	ProposalID uuid.UUID

	// ToolName is the proposed [Proposal.Input.Name].
	ToolName string

	// ProposerID is the agent identity from [Proposal.ProposerID].
	ProposerID string

	// TargetSource mirrors [Proposal.Input.TargetSource].
	TargetSource TargetSource

	// PlainLanguageDescription is the lead-facing summary from
	// [Proposal.Input.PlainLanguageDescription]. Required; the
	// renderer fails closed when empty (a non-engineer lead cannot
	// review without it).
	PlainLanguageDescription string

	// Capabilities is the list of capability ids from
	// [Proposal.Input.Capabilities].
	Capabilities []string

	// Review is the [ReviewResult] from the in-process AI reviewer.
	// Required (non-zero ProposalID + non-empty Gates).
	Review ReviewResult
}

// truncation budgets for card sections. Slack section blocks accept
// up to ~3000 chars of mrkdwn; the budgets below keep the card
// scannable in the lead's DM stream even when every field is at its
// `ProposalInput` upper bound.
const (
	cardDescriptionMaxRunes    = 800
	cardCapabilityListMaxLines = 32
)

// RenderApprovalCard builds the Block-Kit slice for the slack-native
// approval card plus an opaque `action_id` payload shared by all 4
// buttons. Returns nil blocks + empty action_id + [ErrCardMissingInput]
// when any required input is zero-valued.
//
// The renderer is PURE: no I/O, no clock, no dependency injection
// beyond the optional [CapabilityTranslator]. Callers post the
// returned blocks via the production [messenger.Adapter].
func RenderApprovalCard(in CardInput, translate CapabilityTranslator) ([]Block, string, error) {
	if err := validateCardInput(in); err != nil {
		return nil, "", err
	}

	actionID := encodeApprovalActionID(in.ProposalID)

	blocks := make([]Block, 0, 8)
	blocks = append(blocks, headerBlock("Approve tool proposal?"))
	blocks = append(blocks, sectionMrkdwn(metadataMrkdwn(in)))
	blocks = append(blocks, sectionMrkdwn(descriptionMrkdwn(in.PlainLanguageDescription)))
	blocks = append(blocks, sectionMrkdwn(capabilityTranslationsMrkdwn(in.Capabilities, translate)))
	blocks = append(blocks, sectionMrkdwn(gateResultsMrkdwn(in.Review.Gates)))
	blocks = append(blocks, sectionMrkdwn(riskBadgeMrkdwn(in.Review.Risk)))
	blocks = append(blocks, actionButtons(actionID))
	blocks = append(blocks, contextLine(proposalIDFooter(in.ProposalID)))

	return blocks, actionID, nil
}

// EncodeApprovalActionID composes the opaque action_id payload the 4
// buttons carry. The format is stable across this package:
// `tool_approval:<proposal-uuid>`. The clicked button is identified
// via the BlockActions `value` field (one of the [ButtonAction]
// constants), NOT via the action_id — so the same action_id rides on
// all 4 buttons and the dispatcher branches by `value`.
//
// Exported for use by test harnesses and integration fixtures. Returns
// the empty string when [ProposalID] is the zero UUID (caller bug —
// the renderer's input validator catches this branch first).
func EncodeApprovalActionID(proposalID uuid.UUID) string {
	return encodeApprovalActionID(proposalID)
}

// DecodeApprovalActionID parses an action_id back into a proposal id.
// Returns [ErrInvalidActionID] when the input does not have the
// `tool_approval:<proposal-uuid>` shape OR when the embedded uuid
// fails to parse.
func DecodeApprovalActionID(actionID string) (uuid.UUID, error) {
	parts := strings.SplitN(actionID, approvalActionIDSeparator, 2)
	if len(parts) != 2 {
		return uuid.Nil, fmt.Errorf("%w: expected 2 parts, got %d", ErrInvalidActionID, len(parts))
	}
	if parts[0] != approvalActionIDPrefix {
		return uuid.Nil, fmt.Errorf("%w: bad prefix %q", ErrInvalidActionID, parts[0])
	}
	if parts[1] == "" {
		return uuid.Nil, fmt.Errorf("%w: empty proposal id", ErrInvalidActionID)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: %w", ErrInvalidActionID, err)
	}
	if id == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: proposal id is nil uuid", ErrInvalidActionID)
	}
	return id, nil
}

func encodeApprovalActionID(proposalID uuid.UUID) string {
	if proposalID == uuid.Nil {
		return ""
	}
	return approvalActionIDPrefix + approvalActionIDSeparator + proposalID.String()
}

// validateCardInput enforces the renderer's pre-conditions.
func validateCardInput(in CardInput) error {
	if in.ProposalID == uuid.Nil {
		return fmt.Errorf("%w: proposal id required", ErrCardMissingInput)
	}
	if strings.TrimSpace(in.ToolName) == "" {
		return fmt.Errorf("%w: tool name required", ErrCardMissingInput)
	}
	if strings.TrimSpace(in.PlainLanguageDescription) == "" {
		return fmt.Errorf("%w: plain_language_description required", ErrCardMissingInput)
	}
	if len(in.Capabilities) == 0 {
		return fmt.Errorf("%w: capabilities required", ErrCardMissingInput)
	}
	if in.Review.ProposalID == uuid.Nil || len(in.Review.Gates) == 0 {
		return fmt.Errorf("%w: review result required", ErrCardMissingInput)
	}
	// The review MUST refer to the same proposal as the card's
	// identity — otherwise the renderer would ship a card with
	// proposal A's metadata + proposal B's gate results, a class of
	// orchestration bug the M9.4.b iter-1 review flagged. Fail
	// closed at the renderer boundary so a misorchestrated call
	// never lands in the lead's DM.
	if in.Review.ProposalID != in.ProposalID {
		return fmt.Errorf(
			"%w: card.ProposalID=%s review.ProposalID=%s",
			ErrCardProposalMismatch, in.ProposalID, in.Review.ProposalID,
		)
	}
	return nil
}

// metadataMrkdwn renders the first content section: tool name, proposer
// id, target source. Uses inline-code backticks for opaque identifiers
// (proposer id, target source) so embedded mrkdwn does not leak.
func metadataMrkdwn(in CardInput) string {
	return fmt.Sprintf(
		"*Tool*: `%s`\n*Proposer*: `%s`\n*Target source*: `%s`",
		in.ToolName,
		in.ProposerID,
		string(in.TargetSource),
	)
}

// descriptionMrkdwn renders the plain-language description fenced in
// a code block so embedded mrkdwn (asterisks, backticks, mentions) is
// rendered verbatim rather than interpreted as card formatting. The
// content is run-truncated at [cardDescriptionMaxRunes].
func descriptionMrkdwn(desc string) string {
	display := truncateRunes(desc, cardDescriptionMaxRunes)
	var b strings.Builder
	b.WriteString("*Description*\n```\n")
	b.WriteString(display)
	b.WriteString("\n```")
	return b.String()
}

// capabilityTranslationsMrkdwn renders one bullet per capability id.
// Translation comes from the optional [CapabilityTranslator]; a nil
// translator OR an empty-string translation falls back to a "pending
// — M9.3" placeholder so the operator sees the raw cap id and knows
// the dictionary is not yet wired.
//
// The list is capped at [cardCapabilityListMaxLines] bullets — above
// the cap the renderer surfaces a trailing `… (+N more)` line.
func capabilityTranslationsMrkdwn(caps []string, translate CapabilityTranslator) string {
	var b strings.Builder
	b.WriteString("*Capabilities*\n")
	lines := caps
	overflow := 0
	if len(caps) > cardCapabilityListMaxLines {
		lines = caps[:cardCapabilityListMaxLines]
		overflow = len(caps) - cardCapabilityListMaxLines
	}
	for _, c := range lines {
		var translation string
		if translate != nil {
			translation = translate(c)
		}
		switch {
		case translation == "" && translate == nil:
			fmt.Fprintf(&b, "• `%s` (translation pending — M9.3)\n", c)
		case translation == "":
			fmt.Fprintf(&b, "• `%s` (no translation registered)\n", c)
		default:
			fmt.Fprintf(&b, "• `%s`: %s\n", c, translation)
		}
	}
	if overflow > 0 {
		fmt.Fprintf(&b, "… (+%d more)\n", overflow)
	}
	// Trim trailing newline so the section block does not render an
	// orphan empty line.
	return strings.TrimRight(b.String(), "\n")
}

// gateResultsMrkdwn renders one line per gate result with a leading
// emoji. The text format keeps the line scannable; the bounded
// `Detail` string (≤ [MaxGateDetailLength]) is appended verbatim.
func gateResultsMrkdwn(gates []GateResult) string {
	var b strings.Builder
	b.WriteString("*Gates*\n")
	for _, g := range gates {
		emoji := severityEmoji(g.Severity)
		line := fmt.Sprintf("%s `%s` — %s", emoji, string(g.Name), string(g.Severity))
		if g.Detail != "" {
			line += " — " + g.Detail
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// riskBadgeMrkdwn renders the heuristic risk badge as a single
// emoji-prefixed line. Hoisted from `severityEmoji` for clarity.
func riskBadgeMrkdwn(risk RiskLevel) string {
	emoji := riskEmoji(risk)
	return fmt.Sprintf("%s *Risk*: %s", emoji, string(risk))
}

// proposalIDFooter renders the bottom-of-card context line carrying a
// truncated proposal-id prefix the operator uses to correlate the
// rendered card with the M9.7 audit row. Format: `proposal: tk-<8>…`.
func proposalIDFooter(id uuid.UUID) string {
	s := id.String()
	prefix := s
	if len(s) > 8 {
		prefix = s[:8] + "…"
	}
	return fmt.Sprintf("proposal: `tk-%s`", prefix)
}

// severityEmoji maps [Severity] to a Block-Kit-renderable emoji
// shortcode. The shortcodes are universal — Slack and Discord both
// resolve them.
func severityEmoji(s Severity) string {
	switch s {
	case SeverityPass:
		return ":white_check_mark:"
	case SeverityWarn:
		return ":warning:"
	case SeverityFail:
		return ":x:"
	default:
		return ":grey_question:"
	}
}

// riskEmoji maps [RiskLevel] to a Block-Kit-renderable emoji
// shortcode.
func riskEmoji(r RiskLevel) string {
	switch r {
	case RiskLow:
		return ":green_circle:"
	case RiskMedium:
		return ":large_yellow_circle:"
	case RiskHigh:
		return ":red_circle:"
	default:
		return ":grey_question:"
	}
}

// headerBlock returns a single header block carrying `title`.
func headerBlock(title string) Block {
	return Block{
		Type: blockTypeHeader,
		Text: &Text{Type: textTypePlain, Text: title, Emoji: true},
	}
}

// sectionMrkdwn returns a section block carrying mrkdwn-formatted
// `text`. Used for the metadata / description / capabilities / gate-
// results / risk-badge sections.
func sectionMrkdwn(text string) Block {
	return Block{
		Type: blockTypeSection,
		Text: &Text{Type: textTypeMrkdwn, Text: text},
	}
}

// contextLine returns a context block with a single mrkdwn line. The
// element is a flat [ContextElement] (not [Element]): Slack's context
// block schema specifies `{"type":"mrkdwn","text":"..."}` with `text`
// as a string. A nested [Text] object here fails Slack API validation
// and drops the entire card payload.
func contextLine(text string) Block {
	return Block{
		Type: blockTypeContext,
		Elements: []any{
			ContextElement{Type: textTypeMrkdwn, Text: text},
		},
	}
}

// actionButtons returns the 4-button actions block. All buttons share
// the same `actionID` payload; the dispatcher branches on the `value`
// field (one of the [ButtonAction] constants).
func actionButtons(actionID string) Block {
	return Block{
		Type: blockTypeActions,
		Elements: []any{
			Element{
				Type:     elementTypeButton,
				Text:     &Text{Type: textTypePlain, Text: "Approve", Emoji: true},
				ActionID: actionID,
				Value:    string(ButtonActionApprove),
				Style:    buttonStylePrimary,
			},
			Element{
				Type:     elementTypeButton,
				Text:     &Text{Type: textTypePlain, Text: "Reject", Emoji: true},
				ActionID: actionID,
				Value:    string(ButtonActionReject),
				Style:    buttonStyleDanger,
			},
			Element{
				Type:     elementTypeButton,
				Text:     &Text{Type: textTypePlain, Text: "Test in my DM", Emoji: true},
				ActionID: actionID,
				Value:    string(ButtonActionTestInDM),
			},
			Element{
				Type:     elementTypeButton,
				Text:     &Text{Type: textTypePlain, Text: "Ask questions", Emoji: true},
				ActionID: actionID,
				Value:    string(ButtonActionAskQuestions),
			},
		},
	}
}

// truncateRunes returns `s` clipped to at most `maxRunes` runes,
// suffixed with a single `…` when truncation occurred. Returns the
// empty string verbatim. Mirrors the `messenger/slack/cards.truncateRunes`
// helper.
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
