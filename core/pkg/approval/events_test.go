package approval

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTopicToolProposed_ConstantPinsRoadmapName pins the topic
// string to the literal name documented on the roadmap (M9.7 lists
// `tool_proposed` as one of the audit topics). The `approval.`
// prefix matches the package boundary.
func TestTopicToolProposed_ConstantPinsRoadmapName(t *testing.T) {
	t.Parallel()
	const want = "approval.tool_proposed"
	if TopicToolProposed != want {
		t.Errorf("TopicToolProposed: got %q, want %q", TopicToolProposed, want)
	}
}

// TestToolProposed_FieldAllowlist pins the payload to exactly the
// fields documented on [ToolProposed]'s godoc by name + type via
// reflection. Adding a new field requires the author bump this
// allowlist AND consciously document why the new field's PII shape
// is OK. Same discipline as M9.1.b
// `TestEffectiveToolsetUpdated_FieldAllowlist` and M9.2
// `TestToolShadowed_FieldAllowlist`.
func TestToolProposed_FieldAllowlist(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"ProposalID":    "uuid.UUID",
		"ToolName":      "string",
		"ProposerID":    "string",
		"TargetSource":  "approval.TargetSource",
		"CapabilityIDs": "[]string",
		"ProposedAt":    "time.Time",
		"CorrelationID": "string",
	}
	tp := reflect.TypeOf(ToolProposed{})
	got := map[string]string{}
	for i := 0; i < tp.NumField(); i++ {
		f := tp.Field(i)
		got[f.Name] = f.Type.String()
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ToolProposed field allowlist drift:\n got: %s\nwant: %s",
			dumpFields(got), dumpFields(want))
	}
}

func dumpFields(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := "{"
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s:%s", k, m[k])
	}
	return out + "}"
}

func TestNewToolProposedEvent_PopulatesAllFields(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	now := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	prop := Proposal{
		ID:            id,
		ProposerID:    "agent-1",
		Input:         validInput(),
		ProposedAt:    now,
		CorrelationID: "corr-1",
	}
	ev := newToolProposedEvent(prop)
	if ev.ProposalID != id {
		t.Errorf("ProposalID: got %v, want %v", ev.ProposalID, id)
	}
	if ev.ToolName != "count_open_prs" {
		t.Errorf("ToolName: got %q", ev.ToolName)
	}
	if ev.ProposerID != "agent-1" {
		t.Errorf("ProposerID: got %q", ev.ProposerID)
	}
	if ev.TargetSource != TargetSourcePlatform {
		t.Errorf("TargetSource: got %q", ev.TargetSource)
	}
	if len(ev.CapabilityIDs) != 1 || ev.CapabilityIDs[0] != "github:read" {
		t.Errorf("CapabilityIDs: got %v", ev.CapabilityIDs)
	}
	if !ev.ProposedAt.Equal(now) {
		t.Errorf("ProposedAt: got %v, want %v", ev.ProposedAt, now)
	}
	if ev.CorrelationID != "corr-1" {
		t.Errorf("CorrelationID: got %q", ev.CorrelationID)
	}
}

// TestNewToolProposedEvent_DefensiveCapabilityCopy: mutating the
// source Proposal's Capabilities slice post-construction must not
// affect the event's CapabilityIDs.
func TestNewToolProposedEvent_DefensiveCapabilityCopy(t *testing.T) {
	t.Parallel()
	prop := Proposal{
		ID:         uuid.New(),
		ProposerID: "agent-1",
		Input: ProposalInput{
			Name:                     "x",
			Purpose:                  "p",
			PlainLanguageDescription: "pld",
			CodeDraft:                "code",
			Capabilities:             []string{"a", "b"},
			TargetSource:             TargetSourcePlatform,
		},
		ProposedAt:    time.Now(),
		CorrelationID: "corr-1",
	}
	ev := newToolProposedEvent(prop)
	prop.Input.Capabilities[0] = "mutated"
	if ev.CapabilityIDs[0] != "a" {
		t.Errorf("event CapabilityIDs aliased caller mutation: %v", ev.CapabilityIDs)
	}
}
