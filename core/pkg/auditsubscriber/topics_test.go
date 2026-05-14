package auditsubscriber

import (
	"errors"
	"reflect"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/approval"
	"github.com/vadimtrunov/watchkeepers/core/pkg/hostedexport"
	"github.com/vadimtrunov/watchkeepers/core/pkg/localpatch"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolshare"
)

// TestAllBindings_ClosedSet pins the (topic, event_type) wire
// contract for the 11 audit topics M9.7 subscribes to. Adding a
// new binding requires (a) a new row in [allBindings], (b) a new
// constant in this file's expected table, (c) a new dispatch-
// case in `subscriber_test.go`, AND (d) a new PII-canary row in
// `piicanary_test.go`. The four-way bump catches silent drift.
//
// Iter-1 critic m8 fix: the test ALSO calls every binding's
// Extract with the expected zero-value payload AND asserts no
// error. A future swap of `extractSourceSynced` for
// `extractSourceFailed` on a binding row would silently pass a
// non-nil check; the live-call assertion catches it.
func TestAllBindings_ClosedSet(t *testing.T) {
	t.Parallel()
	want := []struct {
		Topic        string
		EventType    string
		ExpectedType string
		ZeroValue    any
	}{
		{toolregistry.TopicSourceSynced, EventTypeSourceSynced, "toolregistry.SourceSynced", toolregistry.SourceSynced{}},
		{toolregistry.TopicSourceFailed, EventTypeSourceFailed, "toolregistry.SourceFailed", toolregistry.SourceFailed{}},
		{toolregistry.TopicToolShadowed, EventTypeToolShadowed, "toolregistry.ToolShadowed", toolregistry.ToolShadowed{}},
		{approval.TopicToolProposed, EventTypeToolProposed, "approval.ToolProposed", approval.ToolProposed{}},
		{approval.TopicToolApproved, EventTypeToolApproved, "approval.ToolApproved", approval.ToolApproved{}},
		{approval.TopicToolRejected, EventTypeToolRejected, "approval.ToolRejected", approval.ToolRejected{}},
		{approval.TopicDryRunExecuted, EventTypeDryRunExecuted, "approval.DryRunExecuted", approval.DryRunExecuted{}},
		{localpatch.TopicLocalPatchApplied, EventTypeLocalPatchApplied, "localpatch.LocalPatchApplied", localpatch.LocalPatchApplied{}},
		{hostedexport.TopicHostedToolExported, EventTypeHostedToolExported, "hostedexport.HostedToolExported", hostedexport.HostedToolExported{}},
		{toolshare.TopicToolShareProposed, EventTypeToolShareProposed, "toolshare.ToolShareProposed", toolshare.ToolShareProposed{}},
		{toolshare.TopicToolSharePROpened, EventTypeToolSharePROpened, "toolshare.ToolSharePROpened", toolshare.ToolSharePROpened{}},
	}
	if len(allBindings) != len(want) {
		t.Fatalf("allBindings count: got %d want %d", len(allBindings), len(want))
	}
	for i, w := range want {
		got := allBindings[i]
		if got.Topic != w.Topic {
			t.Errorf("allBindings[%d].Topic: got %q want %q", i, got.Topic, w.Topic)
		}
		if got.EventType != w.EventType {
			t.Errorf("allBindings[%d].EventType: got %q want %q", i, got.EventType, w.EventType)
		}
		if got.ExpectedType != w.ExpectedType {
			t.Errorf("allBindings[%d].ExpectedType: got %q want %q", i, got.ExpectedType, w.ExpectedType)
		}
		if got.Extract == nil {
			t.Errorf("allBindings[%d].Extract is nil", i)
			continue
		}
		// Pin extractor↔topic correctness by calling Extract with
		// the expected zero-value payload. A miswired binding
		// (e.g. `Extract: extractSourceFailed` on the SourceSynced
		// topic) surfaces here because the extractor would refuse
		// the wrong type.
		payload, _, err := got.Extract(w.ZeroValue)
		if err != nil {
			t.Errorf("allBindings[%d].Extract refused zero-value %T: %v", i, w.ZeroValue, err)
			continue
		}
		if !reflect.DeepEqual(payload, w.ZeroValue) {
			t.Errorf("allBindings[%d].Extract returned %#v want %#v", i, payload, w.ZeroValue)
		}
		// And confirm the extractor REFUSES a different type
		// (closed-set pin from the other side).
		_, _, err = got.Extract("wrong-type")
		if err == nil {
			t.Errorf("allBindings[%d].Extract accepted wrong-type payload", i)
		}
		if err != nil && !errors.Is(err, errUnexpectedPayload) {
			t.Errorf("allBindings[%d].Extract wrong-type err: not errUnexpectedPayload (%v)", i, err)
		}
	}
}

// TestEventTypeVocabulary_RoadmapNames pins the bare verb-phrase
// audit-vocabulary names match the M9.7 roadmap entry. The pin is
// two-way (iter-1 critic m3):
//   - every roadmap name MUST be in allBindings (no silent drop);
//   - every allBindings.EventType MUST be in the roadmap list
//     (no silent addition of a non-roadmap event_type).
//
// A future deferred-topic landing adds a new const + a row in
// allBindings AND extends `roadmapNames` here; the symmetric pin
// fails until both sides match.
func TestEventTypeVocabulary_RoadmapNames(t *testing.T) {
	t.Parallel()
	roadmapNames := []string{
		"source_synced",
		"source_failed",
		"tool_proposed",
		"tool_approved",
		"tool_rejected",
		"tool_dry_run_executed",
		"tool_shadowed",
		"local_patch_applied",
		"hosted_tool_exported",
		"tool_share_proposed",
		"tool_share_pr_opened",
	}
	if len(allBindings) != len(roadmapNames) {
		t.Errorf("len(allBindings)=%d does not match len(roadmapNames)=%d (symmetric drift)",
			len(allBindings), len(roadmapNames))
	}
	have := map[string]struct{}{}
	for _, b := range allBindings {
		have[b.EventType] = struct{}{}
	}
	wantSet := map[string]struct{}{}
	for _, name := range roadmapNames {
		wantSet[name] = struct{}{}
		if _, ok := have[name]; !ok {
			t.Errorf("roadmap-named event %q is NOT in allBindings", name)
		}
	}
	for name := range have {
		if _, ok := wantSet[name]; !ok {
			t.Errorf("allBindings contains event_type %q that is NOT in the roadmap list", name)
		}
	}
}
