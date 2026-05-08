package runtime

import (
	"reflect"
	"testing"
)

// TestToolset_Names_Empty asserts the empty/nil receiver returns nil
// (not an empty-but-non-nil slice). Pinned so the deny-all default at
// the M5.5.b.a ACL gate site continues to compare via len(...) == 0.
func TestToolset_Names_Empty(t *testing.T) {
	t.Parallel()

	var nilTS Toolset
	if got := nilTS.Names(); got != nil {
		t.Errorf("Names() on nil Toolset = %v, want nil", got)
	}

	emptyTS := Toolset{}
	if got := emptyTS.Names(); got != nil {
		t.Errorf("Names() on empty Toolset = %v, want nil", got)
	}
}

// TestToolset_Names_Single asserts a single-entry Toolset projects to a
// single-element slice carrying the entry's Name (Version ignored).
func TestToolset_Names_Single(t *testing.T) {
	t.Parallel()

	ts := Toolset{{Name: "echo", Version: "v1.0.0"}}
	got := ts.Names()
	want := []string{"echo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v", got, want)
	}
}

// TestToolset_Names_PreservesOrder asserts multi-entry Toolset Names()
// preserves the original entry order. The loader does NOT sort; the
// ACL gate compares names by exact match so order does not affect
// authorization semantics, but downstream LLM tool schemas reflect the
// manifest's declared order.
func TestToolset_Names_PreservesOrder(t *testing.T) {
	t.Parallel()

	ts := Toolset{
		{Name: "c", Version: "v3"},
		{Name: "a", Version: "v1"},
		{Name: "b"},
	}
	got := ts.Names()
	want := []string{"c", "a", "b"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v (order MUST be preserved)", got, want)
	}
}

// TestToolset_Names_VersionsIgnored asserts entries with empty Version
// project to the same Names slice as entries with populated Version —
// .Names() projects the Name field only, never Version. Pins the
// migration safety net for the M5.5.b.a ACL gate (gate keys on Name,
// not on Name+Version).
func TestToolset_Names_VersionsIgnored(t *testing.T) {
	t.Parallel()

	versioned := Toolset{{Name: "echo", Version: "v2"}, {Name: "sum", Version: "v1"}}
	bare := Toolset{{Name: "echo"}, {Name: "sum"}}
	if !reflect.DeepEqual(versioned.Names(), bare.Names()) {
		t.Errorf("Names() differs between versioned %v and bare %v variants", versioned.Names(), bare.Names())
	}
}

// TestToolset_Names_DuplicatesPreserved asserts duplicate Names are NOT
// deduplicated — Names() is a pure projection, dedupe is the caller's
// responsibility (the ACL-gate map inserts already coalesce duplicates
// natively).
func TestToolset_Names_DuplicatesPreserved(t *testing.T) {
	t.Parallel()

	ts := Toolset{{Name: "echo"}, {Name: "echo", Version: "v2"}}
	got := ts.Names()
	want := []string{"echo", "echo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Names() = %v, want %v (duplicates MUST be preserved)", got, want)
	}
}
