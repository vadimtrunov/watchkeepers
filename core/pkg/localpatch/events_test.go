package localpatch

import (
	"reflect"
	"testing"
	"time"
)

// TestM95_TopicLocalPatchApplied_PinnedName pins the topic constant
// to the M9.7-promised name. A renamed topic is a downstream
// subscriber's behaviour change.
func TestM95_TopicLocalPatchApplied_PinnedName(t *testing.T) {
	const want = "localpatch.local_patch_applied"
	if TopicLocalPatchApplied != want {
		t.Errorf("TopicLocalPatchApplied: got %q want %q", TopicLocalPatchApplied, want)
	}
}

// TestM95_LocalPatchApplied_FieldAllowlist pins the field shape of
// [LocalPatchApplied] BY NAME AND TYPE in declaration order.
// Mirrors M9.4.c TestM94C_DryRunExecuted_FieldAllowlist.
func TestM95_LocalPatchApplied_FieldAllowlist(t *testing.T) {
	want := []struct {
		Name string
		Type string
	}{
		{"SourceName", "string"},
		{"ToolName", "string"},
		{"ToolVersion", "string"},
		{"OperatorID", "string"},
		{"Reason", "string"},
		{"DiffHash", "string"},
		{"Operation", "localpatch.Operation"},
		{"AppliedAt", "time.Time"},
		{"CorrelationID", "string"},
	}
	ty := reflect.TypeOf(LocalPatchApplied{})
	if ty.NumField() != len(want) {
		t.Errorf("LocalPatchApplied: want %d fields got %d", len(want), ty.NumField())
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

// TestM95_LocalPatchApplied_NoBannedField asserts no PII / source-
// content field name is present on the bus payload (mirror M9.4.c's
// `TestM94C_DryRunExecuted_NoBodyOrArgsField`). Iter-1 critic m5 fix
// expanded the banlist to cover Manifest, OperatorIDHint, Diff,
// Patch, SourceContent — fields a future refactor might accidentally
// add as a verbose denormalisation.
func TestM95_LocalPatchApplied_NoBannedField(t *testing.T) {
	banned := []string{
		"FolderPath", "FolderContent", "ManifestBody", "Manifest",
		"SnapshotPath", "DataDir", "LivePath", "SourceContent",
		"AuthSecret", "Token", "Credential", "Secret",
		"OperatorIDHint", "Diff", "Patch", "DiffBody",
	}
	ty := reflect.TypeOf(LocalPatchApplied{})
	for i := 0; i < ty.NumField(); i++ {
		name := ty.Field(i).Name
		for _, b := range banned {
			if name == b {
				t.Errorf("LocalPatchApplied carries banned field %s", name)
			}
		}
	}
}

// TestNewLocalPatchAppliedEvent_PopulatesAllFields asserts the
// constructor maps every input into the payload exactly once.
// Centralised mapping discipline mirrors `approval.newDryRunExecutedEvent`.
func TestNewLocalPatchAppliedEvent_PopulatesAllFields(t *testing.T) {
	now := time.Date(2026, 5, 13, 12, 0, 0, 0, time.UTC)
	ev := newLocalPatchAppliedEvent(
		"src1", "toolA", "1.0.0",
		"alice", "incident #4711", "0123456789abcdef",
		OperationInstall, now, "corr-1",
	)
	if ev.SourceName != "src1" || ev.ToolName != "toolA" || ev.ToolVersion != "1.0.0" {
		t.Errorf("header: %+v", ev)
	}
	if ev.OperatorID != "alice" || ev.Reason != "incident #4711" {
		t.Errorf("operator/reason: %+v", ev)
	}
	if ev.DiffHash != "0123456789abcdef" {
		t.Errorf("DiffHash: %q", ev.DiffHash)
	}
	if ev.Operation != OperationInstall {
		t.Errorf("Operation: %q", string(ev.Operation))
	}
	if !ev.AppliedAt.Equal(now) {
		t.Errorf("AppliedAt: %v want %v", ev.AppliedAt, now)
	}
	if ev.CorrelationID != "corr-1" {
		t.Errorf("CorrelationID: %q", ev.CorrelationID)
	}
}

// TestM95_OperationConstants pins the closed-set operation values.
func TestM95_OperationConstants(t *testing.T) {
	if string(OperationInstall) != "install" {
		t.Errorf("OperationInstall: got %q want %q", OperationInstall, "install")
	}
	if string(OperationRollback) != "rollback" {
		t.Errorf("OperationRollback: got %q want %q", OperationRollback, "rollback")
	}
}

// TestM95_Operation_Validate exercises the closed-set Validate
// discipline (iter-1 critic n3 fix).
func TestM95_Operation_Validate(t *testing.T) {
	for _, ok := range []Operation{OperationInstall, OperationRollback} {
		if err := ok.Validate(); err != nil {
			t.Errorf("Operation(%q).Validate() = %v; want nil", ok, err)
		}
	}
	for _, bad := range []Operation{"", "INSTALL", "delete", "Install"} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Operation(%q).Validate() = nil; want error", bad)
		}
	}
}
