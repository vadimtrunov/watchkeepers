package approval

import (
	"errors"
	"reflect"
	"testing"
)

// TestM94B_Topics_PinnedNames asserts the new topic constants match
// the roadmap text verbatim. A renamed topic is a downstream
// subscriber's behaviour change.
func TestM94B_Topics_PinnedNames(t *testing.T) {
	got := map[string]string{
		"TopicToolApproved":    TopicToolApproved,
		"TopicToolRejected":    TopicToolRejected,
		"TopicDryRunRequested": TopicDryRunRequested,
		"TopicQuestionAsked":   TopicQuestionAsked,
	}
	want := map[string]string{
		"TopicToolApproved":    "approval.tool_approved",
		"TopicToolRejected":    "approval.tool_rejected",
		"TopicDryRunRequested": "approval.tool_dry_run_requested",
		"TopicQuestionAsked":   "approval.tool_question_asked",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: want %q got %q", k, v, got[k])
		}
	}
}

// TestM94B_ToolApproved_FieldAllowlist pins the 10-field shape of
// [ToolApproved]. Adding a field requires bumping this allowlist AND
// documenting the new field's PII shape in the godoc.
func TestM94B_ToolApproved_FieldAllowlist(t *testing.T) {
	want := []string{
		"ProposalID",
		"ToolName",
		"ApproverID",
		"Route",
		"TargetSource",
		"SourceName",
		"PRURL",
		"MergedSHA",
		"ApprovedAt",
		"CorrelationID",
	}
	assertFieldsByName(t, reflect.TypeOf(ToolApproved{}), want)
}

func TestM94B_ToolRejected_FieldAllowlist(t *testing.T) {
	want := []string{
		"ProposalID",
		"ToolName",
		"RejecterID",
		"Route",
		"RejectedAt",
		"CorrelationID",
	}
	assertFieldsByName(t, reflect.TypeOf(ToolRejected{}), want)
}

func TestM94B_DryRunRequested_FieldAllowlist(t *testing.T) {
	want := []string{
		"ProposalID",
		"ToolName",
		"RequesterID",
		"LeadDMChannel",
		"RequestedAt",
		"CorrelationID",
	}
	assertFieldsByName(t, reflect.TypeOf(DryRunRequested{}), want)
}

func TestM94B_QuestionAsked_FieldAllowlist(t *testing.T) {
	want := []string{
		"ProposalID",
		"ToolName",
		"AskerID",
		"AskedAt",
		"CorrelationID",
	}
	assertFieldsByName(t, reflect.TypeOf(QuestionAsked{}), want)
}

func TestRoute_Validate(t *testing.T) {
	for _, ok := range []Route{RouteGitPR, RouteSlackNative} {
		if err := ok.Validate(); err != nil {
			t.Errorf("%s should be valid: %v", ok, err)
		}
	}
	for _, bad := range []Route{"", "telepathy", "GIT-PR"} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidRoute) {
			t.Errorf("%q should fail with ErrInvalidRoute: %v", bad, err)
		}
	}
}

// TestM94B_NoBodyFieldOnEvents asserts no PII-bearing field name
// (`CodeDraft`, `Purpose`, `PlainLanguageDescription`) is present on
// any of the new event payloads. Catches accidental copy-paste during
// payload extension.
func TestM94B_NoBodyFieldOnEvents(t *testing.T) {
	bannedNames := []string{"CodeDraft", "Purpose", "PlainLanguageDescription"}
	types := []reflect.Type{
		reflect.TypeOf(ToolApproved{}),
		reflect.TypeOf(ToolRejected{}),
		reflect.TypeOf(DryRunRequested{}),
		reflect.TypeOf(QuestionAsked{}),
	}
	for _, ty := range types {
		for i := 0; i < ty.NumField(); i++ {
			name := ty.Field(i).Name
			for _, b := range bannedNames {
				if name == b {
					t.Errorf("%s carries banned PII field %s", ty.Name(), name)
				}
			}
		}
	}
}

func assertFieldsByName(t *testing.T, ty reflect.Type, want []string) {
	t.Helper()
	if ty.NumField() != len(want) {
		t.Errorf("%s: want %d fields got %d", ty.Name(), len(want), ty.NumField())
	}
	for i, name := range want {
		if i >= ty.NumField() {
			t.Errorf("%s: missing field %q at index %d", ty.Name(), name, i)
			continue
		}
		if ty.Field(i).Name != name {
			t.Errorf("%s: field[%d]: want %q got %q", ty.Name(), i, name, ty.Field(i).Name)
		}
	}
}
