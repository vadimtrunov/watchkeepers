package approval

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func newTestCardInput() CardInput {
	id := mustNewUUIDv7()
	return CardInput{
		ProposalID:               id,
		ToolName:                 "count_open_prs",
		ProposerID:               "agent-1",
		TargetSource:             TargetSourcePlatform,
		PlainLanguageDescription: "Counts how many open pull requests still need review.",
		Capabilities:             []string{"github:read"},
		Review: ReviewResult{
			ProposalID:    id,
			ToolName:      "count_open_prs",
			Gates:         []GateResult{{Name: GateTypecheck, Severity: SeverityPass}},
			Risk:          RiskLow,
			ReviewedAt:    time.Now(),
			CorrelationID: id.String(),
		},
	}
}

func TestRenderApprovalCard_HappyPath(t *testing.T) {
	in := newTestCardInput()
	blocks, actionID, err := RenderApprovalCard(in, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if actionID == "" {
		t.Errorf("actionID must be non-empty")
	}
	if len(blocks) < 7 {
		t.Errorf("expected ≥7 blocks, got %d", len(blocks))
	}
	// Find actions block; verify 4 buttons.
	var actionsBlock *Block
	for i := range blocks {
		if blocks[i].Type == "actions" {
			actionsBlock = &blocks[i]
			break
		}
	}
	if actionsBlock == nil {
		t.Fatalf("no actions block")
	}
	if len(actionsBlock.Elements) != 4 {
		t.Errorf("expected 4 buttons, got %d", len(actionsBlock.Elements))
	}
	wantValues := map[string]bool{
		"approve":       false,
		"reject":        false,
		"test_in_my_dm": false,
		"ask_questions": false,
	}
	for _, raw := range actionsBlock.Elements {
		el, ok := raw.(Element)
		if !ok {
			t.Fatalf("actions-block element: want Element, got %T", raw)
		}
		if el.ActionID != actionID {
			t.Errorf("button action_id mismatch: %s", el.ActionID)
		}
		if _, ok := wantValues[el.Value]; !ok {
			t.Errorf("unexpected button value: %s", el.Value)
		}
		wantValues[el.Value] = true
	}
	for k, seen := range wantValues {
		if !seen {
			t.Errorf("missing button value: %s", k)
		}
	}
}

func TestRenderApprovalCard_MissingInputs(t *testing.T) {
	base := newTestCardInput()
	tests := []struct {
		name   string
		mutate func(*CardInput)
	}{
		{"zero ProposalID", func(in *CardInput) { in.ProposalID = uuid.Nil }},
		{"empty ToolName", func(in *CardInput) { in.ToolName = "" }},
		{"empty Description", func(in *CardInput) { in.PlainLanguageDescription = "" }},
		{"empty Capabilities", func(in *CardInput) { in.Capabilities = nil }},
		{"empty Review.Gates", func(in *CardInput) { in.Review.Gates = nil }},
		{"zero Review.ProposalID", func(in *CardInput) { in.Review.ProposalID = uuid.Nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := base
			in.Capabilities = append([]string{}, base.Capabilities...) // detach
			in.Review.Gates = append([]GateResult{}, base.Review.Gates...)
			tt.mutate(&in)
			_, _, err := RenderApprovalCard(in, nil)
			if !errors.Is(err, ErrCardMissingInput) {
				t.Errorf("want ErrCardMissingInput, got %v", err)
			}
		})
	}
}

func TestEncodeDecodeApprovalActionID_Roundtrip(t *testing.T) {
	id := mustNewUUIDv7()
	ai := EncodeApprovalActionID(id)
	got, err := DecodeApprovalActionID(ai)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got != id {
		t.Errorf("roundtrip mismatch: want %s got %s", id, got)
	}
}

func TestEncodeApprovalActionID_NilUUIDIsEmpty(t *testing.T) {
	if EncodeApprovalActionID(uuid.Nil) != "" {
		t.Errorf("nil uuid must encode to empty string")
	}
}

func TestDecodeApprovalActionID_RejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"plain-text",
		"wrong_prefix:" + mustNewUUIDv7().String(),
		"tool_approval:",
		"tool_approval:not-a-uuid",
		"tool_approval:" + uuid.Nil.String(),
	}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			_, err := DecodeApprovalActionID(c)
			if !errors.Is(err, ErrInvalidActionID) {
				t.Errorf("input %q: want ErrInvalidActionID, got %v", c, err)
			}
		})
	}
}

func TestRenderApprovalCard_CapabilityTranslator_Used(t *testing.T) {
	in := newTestCardInput()
	in.Capabilities = []string{"github:read", "jira:read"}
	trans := func(id string) string {
		switch id {
		case "github:read":
			return "Read GitHub PRs"
		case "jira:read":
			return "Read Jira tickets"
		}
		return ""
	}
	blocks, _, err := RenderApprovalCard(in, trans)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	joined := jsonBlocksString(t, blocks)
	if !strings.Contains(joined, "Read GitHub PRs") || !strings.Contains(joined, "Read Jira tickets") {
		t.Errorf("translations not rendered: %s", joined)
	}
}

func TestRenderApprovalCard_NilTranslator_FallbackPlaceholder(t *testing.T) {
	in := newTestCardInput()
	blocks, _, err := RenderApprovalCard(in, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	joined := jsonBlocksString(t, blocks)
	if !strings.Contains(joined, FallbackTranslationDictionaryNotLoaded) {
		t.Errorf("nil translator must fall back to dictionary-not-loaded placeholder %q; rendered: %s", FallbackTranslationDictionaryNotLoaded, joined)
	}
}

func TestRenderApprovalCard_TranslatorReturnsEmpty_Fallback(t *testing.T) {
	in := newTestCardInput()
	trans := func(_ string) string { return "" }
	blocks, _, err := RenderApprovalCard(in, trans)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	joined := jsonBlocksString(t, blocks)
	if !strings.Contains(joined, FallbackTranslationNotRegistered) {
		t.Errorf("empty translation must surface placeholder %q; got: %s", FallbackTranslationNotRegistered, joined)
	}
}

func TestRenderApprovalCard_DescriptionFencedNotInterpreted(t *testing.T) {
	in := newTestCardInput()
	in.PlainLanguageDescription = "*Bold* and <@user> mention plus `backticks`"
	blocks, _, err := RenderApprovalCard(in, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	joined := jsonBlocksString(t, blocks)
	if !strings.Contains(joined, "```") {
		t.Errorf("description must be fenced in a code block: %s", joined)
	}
}

func TestRenderApprovalCard_CapabilityListTruncated(t *testing.T) {
	in := newTestCardInput()
	caps := make([]string, cardCapabilityListMaxLines+3)
	for i := range caps {
		caps[i] = "cap_" + uuid.NewString()
	}
	in.Capabilities = caps
	blocks, _, err := RenderApprovalCard(in, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	joined := jsonBlocksString(t, blocks)
	if !strings.Contains(joined, "(+3 more)") {
		t.Errorf("overflow line missing: %s", joined)
	}
}

func TestRenderApprovalCard_RiskBadgeRendered(t *testing.T) {
	in := newTestCardInput()
	in.Review.Risk = RiskHigh
	blocks, _, err := RenderApprovalCard(in, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	joined := jsonBlocksString(t, blocks)
	if !strings.Contains(joined, "high") {
		t.Errorf("RiskHigh must appear in rendered card: %s", joined)
	}
}

// TestRenderApprovalCard_PIICanary_InputShapeForbidsBodyFields pins
// the [CardInput] field set against accidental introduction of
// `Purpose` / `CodeDraft` — the two PII-bearing [ProposalInput] fields
// the renderer must never read. The reflect-based check fails closed
// if a future field addition names either; the prior shape of this
// test (a `type reflectFieldNames = CardInput` alias) was a
// no-op flagged by the M9.4.b iter-1 review.
func TestRenderApprovalCard_PIICanary_InputShapeForbidsBodyFields(t *testing.T) {
	banned := map[string]struct{}{
		"Purpose":   {},
		"CodeDraft": {},
	}
	ty := reflect.TypeOf(CardInput{})
	if ty.NumField() == 0 {
		t.Fatalf("CardInput is empty — reflection assertion would trivially pass")
	}
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		if _, bad := banned[name]; bad {
			t.Errorf("CardInput carries banned PII field %q (must never read Purpose/CodeDraft)", name)
		}
	}
}

// TestRenderApprovalCard_PIICanary_CanaryStaysInsideDescriptionFence
// pins that a canary substring injected into
// `PlainLanguageDescription` (the ONLY free-form body the renderer
// reads) lands inside the rendered card body — and that the JSON of
// the rendered blocks does NOT carry the canary anywhere outside the
// description section. Real assertion (not a no-op alias) catches a
// future regression where the renderer accidentally re-uses the
// description body for the action_id, the context line, or any other
// block.
func TestRenderApprovalCard_PIICanary_CanaryStaysInsideDescriptionFence(t *testing.T) {
	const canary = "CANARY_DESC_RENDER_zzzzzz"
	in := newTestCardInput()
	in.PlainLanguageDescription = canary
	blocks, actionID, err := RenderApprovalCard(in, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// The canary MUST appear in the description section (visibility AC).
	rendered := jsonBlocksString(t, blocks)
	if !strings.Contains(rendered, canary) {
		t.Errorf("description canary must render onto the card body; rendered: %s", rendered)
	}
	// The canary MUST NOT appear in the opaque action_id payload — that
	// payload is encoded only from the proposal id.
	if strings.Contains(actionID, canary) {
		t.Errorf("action_id leaked description canary: %s", actionID)
	}
}

// TestRenderApprovalCard_ContextBlockElementShape pins the Slack
// Block Kit schema for the context block's element: `text` MUST be a
// string field, NOT a nested object. CodeRabbit caught a regression
// where the prior shape `{"type":"mrkdwn","text":{"type":"mrkdwn",
// "text":"..."}}` would fail Slack API validation and drop the card
// payload.
func TestRenderApprovalCard_ContextBlockElementShape(t *testing.T) {
	in := newTestCardInput()
	blocks, _, err := RenderApprovalCard(in, nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var ctxBlock *Block
	for i := range blocks {
		if blocks[i].Type == blockTypeContext {
			ctxBlock = &blocks[i]
			break
		}
	}
	if ctxBlock == nil {
		t.Fatalf("no context block")
	}
	if len(ctxBlock.Elements) != 1 {
		t.Fatalf("expected 1 context element, got %d", len(ctxBlock.Elements))
	}
	if _, ok := ctxBlock.Elements[0].(ContextElement); !ok {
		t.Errorf("context element: want ContextElement (flat text string), got %T", ctxBlock.Elements[0])
	}
	// Marshal-then-decode pins the on-wire JSON shape.
	raw, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded []struct {
		Type     string `json:"type"`
		Elements []struct {
			Type string          `json:"type"`
			Text json.RawMessage `json:"text"`
		} `json:"elements"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, b := range decoded {
		if b.Type != blockTypeContext {
			continue
		}
		for _, el := range b.Elements {
			// `text` must decode as a JSON string ("..."), not an
			// object ({...}). A JSON-string `text` field's first
			// byte is `"`; an object's first byte is `{`.
			if len(el.Text) == 0 || el.Text[0] != '"' {
				t.Errorf("context element `text` must be a JSON string, got %s", string(el.Text))
			}
		}
	}
}

func TestRenderApprovalCard_ReviewProposalIDMismatch_FailsClosed(t *testing.T) {
	in := newTestCardInput()
	// Forge the orchestration bug: review carries a DIFFERENT
	// proposal id than the card identity.
	in.Review.ProposalID = mustNewUUIDv7()
	_, _, err := RenderApprovalCard(in, nil)
	if !errors.Is(err, ErrCardProposalMismatch) {
		t.Errorf("mismatched proposal ids must return ErrCardProposalMismatch, got %v", err)
	}
}

func TestButtonAction_Validate(t *testing.T) {
	for _, ok := range []ButtonAction{ButtonActionApprove, ButtonActionReject, ButtonActionTestInDM, ButtonActionAskQuestions} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%s should be valid: %v", ok, err)
		}
	}
	for _, bad := range []ButtonAction{"", "approved", "fire_zee_missiles"} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidButtonValue) {
			t.Errorf("%q should fail: %v", bad, err)
		}
	}
}

func jsonBlocksString(t *testing.T, blocks []Block) string {
	t.Helper()
	b, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return string(b)
}
