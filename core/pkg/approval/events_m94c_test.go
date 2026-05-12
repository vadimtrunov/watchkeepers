package approval

import (
	"reflect"
	"testing"
	"time"
)

// TestM94C_TopicDryRunExecuted_PinnedName pins the topic constant to
// the roadmap text. A renamed topic is a downstream subscriber's
// behaviour change (M9.7 audit subscriber consumes this name
// verbatim).
func TestM94C_TopicDryRunExecuted_PinnedName(t *testing.T) {
	const want = "approval.tool_dry_run_executed"
	if TopicDryRunExecuted != want {
		t.Errorf("TopicDryRunExecuted: got %q want %q", TopicDryRunExecuted, want)
	}
}

// TestM94C_DryRunExecuted_FieldAllowlist pins the 7-field shape of
// [DryRunExecuted] in declaration order BY NAME AND TYPE (iter-1
// critic m5 fix). M9.4.a's [ToolProposed] allowlist already pins by
// name+type via the map-of-type-strings shape; M9.4.b's allowlist
// helper pins by name only. M9.4.c picks up the stricter
// name+type discipline so a future field-type swap (e.g.
// `ProposalID uuid.UUID` → `ProposalID string`) surfaces here rather
// than at a downstream subscriber.
func TestM94C_DryRunExecuted_FieldAllowlist(t *testing.T) {
	want := []struct {
		Name string
		Type string
	}{
		{"ProposalID", "uuid.UUID"},
		{"ToolName", "string"},
		{"Mode", "toolregistry.DryRunMode"},
		{"BrokerKindCounts", "map[string]int"},
		{"InvocationCount", "int"},
		{"ExecutedAt", "time.Time"},
		{"CorrelationID", "string"},
	}
	ty := reflect.TypeOf(DryRunExecuted{})
	if ty.NumField() != len(want) {
		t.Errorf("DryRunExecuted: want %d fields got %d", len(want), ty.NumField())
	}
	for i, w := range want {
		if i >= ty.NumField() {
			t.Errorf("missing field %q at index %d", w.Name, i)
			continue
		}
		f := ty.Field(i)
		if f.Name != w.Name {
			t.Errorf("field[%d]: want name %q got %q", i, w.Name, f.Name)
		}
		if f.Type.String() != w.Type {
			t.Errorf("field[%d] %s: want type %q got %q", i, f.Name, w.Type, f.Type.String())
		}
	}
}

// TestM94C_DryRunExecuted_NoBodyOrArgsField asserts no PII-bearing
// field name (`CodeDraft`, `Purpose`, `PlainLanguageDescription`) AND
// no `Args` / `Original` / `Effective` field is present on the
// payload. The `Args`-shaped fields stay in-process on [Trace]; an
// accidental copy onto the bus payload would surface here.
func TestM94C_DryRunExecuted_NoBodyOrArgsField(t *testing.T) {
	bannedNames := []string{
		"CodeDraft", "Purpose", "PlainLanguageDescription",
		"Args", "Original", "Effective", "Outcomes",
	}
	ty := reflect.TypeOf(DryRunExecuted{})
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		for _, b := range bannedNames {
			if name == b {
				t.Errorf("DryRunExecuted carries banned field %s", name)
			}
		}
	}
}

// TestNewDryRunExecutedEvent_PopulatesAllFields asserts the
// constructor maps every field from a [Request] + outcomes into the
// payload exactly once (centralised mapping discipline mirrors
// [newToolProposedEvent]).
func TestNewDryRunExecutedEvent_PopulatesAllFields(t *testing.T) {
	req := validRequest("ghost")
	outcomes := []Outcome{
		{Original: BrokerInvocation{Kind: BrokerSlack, Op: "send_message"}, Disposition: DispositionGhosted},
		{Original: BrokerInvocation{Kind: BrokerJira, Op: "create_issue"}, Disposition: DispositionGhosted},
		{Original: BrokerInvocation{Kind: BrokerSlack, Op: "update_message"}, Disposition: DispositionGhosted},
	}
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	ev := newDryRunExecutedEvent(req, outcomes, now, "corr-1")
	if !ev.ExecutedAt.Equal(now) {
		t.Errorf("ExecutedAt: got %v want %v", ev.ExecutedAt, now)
	}
	if ev.ProposalID != req.ProposalID {
		t.Errorf("ProposalID: got %v want %v", ev.ProposalID, req.ProposalID)
	}
	if ev.ToolName != req.ToolName {
		t.Errorf("ToolName: got %q", ev.ToolName)
	}
	if ev.Mode != req.Mode {
		t.Errorf("Mode: got %q want %q", ev.Mode, req.Mode)
	}
	if ev.InvocationCount != 3 {
		t.Errorf("InvocationCount: got %d want 3", ev.InvocationCount)
	}
	if ev.BrokerKindCounts[string(BrokerSlack)] != 2 {
		t.Errorf("slack count: got %d want 2", ev.BrokerKindCounts[string(BrokerSlack)])
	}
	if ev.BrokerKindCounts[string(BrokerJira)] != 1 {
		t.Errorf("jira count: got %d want 1", ev.BrokerKindCounts[string(BrokerJira)])
	}
	if ev.CorrelationID != "corr-1" {
		t.Errorf("CorrelationID: got %q", ev.CorrelationID)
	}
}

// TestDisposition_Constants pins the two disposition constants. Same
// closed-set enum discipline as [Route] / [DryRunMode] / [BrokerKind].
func TestDisposition_Constants(t *testing.T) {
	if string(DispositionGhosted) != "ghosted" {
		t.Errorf("DispositionGhosted: got %q want %q", DispositionGhosted, "ghosted")
	}
	if string(DispositionScoped) != "scoped" {
		t.Errorf("DispositionScoped: got %q want %q", DispositionScoped, "scoped")
	}
}
