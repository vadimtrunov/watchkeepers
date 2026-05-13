package hostedexport_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/hostedexport"
)

func TestM96A_TopicHostedToolExportedPinned(t *testing.T) {
	const want = "hostedexport.hosted_tool_exported"
	if hostedexport.TopicHostedToolExported != want {
		t.Fatalf("TopicHostedToolExported=%q want %q", hostedexport.TopicHostedToolExported, want)
	}
}

// TestM96A_HostedToolExported_FieldAllowlist pins the field set and
// TYPE in declaration order. A future field addition forces the
// author to bump the allowlist AND consciously document the new
// field's PII shape — mirror M9.5's
// `TestM95_LocalPatchApplied_FieldAllowlist`.
func TestM96A_HostedToolExported_FieldAllowlist(t *testing.T) {
	type want struct {
		name  string
		gType string
	}
	wants := []want{
		{"SourceName", "string"},
		{"ToolName", "string"},
		{"ToolVersion", "string"},
		{"OperatorID", "string"},
		{"Reason", "string"},
		{"BundleDigest", "string"},
		{"ExportedAt", "time.Time"},
		{"CorrelationID", "string"},
	}
	rt := reflect.TypeOf(hostedexport.HostedToolExported{})
	if rt.NumField() != len(wants) {
		t.Fatalf("HostedToolExported field count=%d want %d", rt.NumField(), len(wants))
	}
	for i, w := range wants {
		f := rt.Field(i)
		if f.Name != w.name {
			t.Errorf("field[%d] name=%q want %q", i, f.Name, w.name)
		}
		got := f.Type.String()
		if got != w.gType {
			t.Errorf("field[%d] type=%q want %q", i, got, w.gType)
		}
	}
}

// TestM96A_HostedToolExported_NoBannedField pins the negative side
// of the allowlist: explicitly banned field names that a future
// silent refactor might add. Mirror M9.5's
// `TestM95_LocalPatchApplied_NoBannedField`.
func TestM96A_HostedToolExported_NoBannedField(t *testing.T) {
	banned := []string{
		"DestinationPath", "LivePath", "FolderContent", "ManifestBody",
		"SourceContent", "Token", "AuthSecret", "OperatorIDHint",
		"Diff", "Patch",
	}
	rt := reflect.TypeOf(hostedexport.HostedToolExported{})
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		for _, b := range banned {
			if f.Name == b {
				t.Errorf("HostedToolExported contains banned field %q", b)
			}
		}
	}
}

func TestM96A_HostedToolExported_TimeType(t *testing.T) {
	// Sanity: ExportedAt is the only time.Time field; the
	// reflection-based test above checks the type string but this
	// confirms time.Time is actually consumable via a real value.
	ev := hostedexport.HostedToolExported{ExportedAt: time.Unix(1700000000, 0).UTC()}
	if ev.ExportedAt.IsZero() {
		t.Fatalf("ExportedAt round-trip broke: %v", ev.ExportedAt)
	}
}
