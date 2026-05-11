package toolregistry

import (
	"encoding/json"
	"testing"
	"time"
)

func mkManifest(name, version string, caps ...string) Manifest {
	if len(caps) == 0 {
		caps = []string{"placeholder"}
	}
	cp := make([]string, len(caps))
	copy(cp, caps)
	return Manifest{
		Name:         name,
		Version:      version,
		Capabilities: cp,
		Schema:       json.RawMessage(`{"type":"object"}`),
	}
}

func TestEffectiveToolset_LookupHappyPath(t *testing.T) {
	t.Parallel()
	tools := []EffectiveTool{
		{Source: "platform", Manifest: mkManifest("count_open_prs", "1.0.0")},
		{Source: "private", Manifest: mkManifest("find_overdue_tickets", "0.4.0")},
	}
	snap := newEffectiveToolset(7, time.Now(), tools)
	got, ok := snap.Lookup("count_open_prs")
	if !ok {
		t.Fatal("Lookup count_open_prs: not found")
	}
	if got.Source != "platform" {
		t.Errorf("Source: got %q", got.Source)
	}
	if got.Manifest.Version != "1.0.0" {
		t.Errorf("Version: got %q", got.Manifest.Version)
	}
}

func TestEffectiveToolset_LookupMissingReturnsZeroFalse(t *testing.T) {
	t.Parallel()
	snap := newEffectiveToolset(1, time.Now(), nil)
	got, ok := snap.Lookup("ghost")
	if ok {
		t.Errorf("Lookup ghost: ok=true, want false")
	}
	if got.Source != "" || got.Manifest.Name != "" {
		t.Errorf("Lookup ghost: got %+v, want zero", got)
	}
}

func TestEffectiveToolset_NilSnapshotSafe(t *testing.T) {
	t.Parallel()
	var snap *EffectiveToolset
	got, ok := snap.Lookup("x")
	if ok || got.Source != "" || got.Manifest.Name != "" {
		t.Errorf("nil Lookup: got %+v, %v", got, ok)
	}
	if got := snap.Names(); got != nil {
		t.Errorf("nil Names: got %v, want nil", got)
	}
	if got := snap.Len(); got != 0 {
		t.Errorf("nil Len: got %d, want 0", got)
	}
}

func TestEffectiveToolset_NamesStableAlphabeticalOrder(t *testing.T) {
	t.Parallel()
	tools := []EffectiveTool{
		{Source: "a", Manifest: mkManifest("zebra", "1")},
		{Source: "a", Manifest: mkManifest("ant", "1")},
		{Source: "b", Manifest: mkManifest("mango", "1")},
	}
	snap := newEffectiveToolset(1, time.Now(), tools)
	names := snap.Names()
	want := []string{"ant", "mango", "zebra"}
	if len(names) != len(want) {
		t.Fatalf("len: got %d, want %d", len(names), len(want))
	}
	for i := range names {
		if names[i] != want[i] {
			t.Errorf("names[%d]: got %q, want %q", i, names[i], want[i])
		}
	}
}

func TestEffectiveToolset_DefensiveCopyAtBuild(t *testing.T) {
	t.Parallel()
	tools := []EffectiveTool{
		{Source: "a", Manifest: mkManifest("t1", "1")},
	}
	snap := newEffectiveToolset(1, time.Now(), tools)
	tools[0].Source = "MUTATED"
	got, _ := snap.Lookup("t1")
	if got.Source != "a" {
		t.Errorf("caller mutation bled into snapshot: Source=%q", got.Source)
	}
}

func TestEffectiveToolset_NamesReturnsFreshSlice(t *testing.T) {
	t.Parallel()
	tools := []EffectiveTool{
		{Source: "a", Manifest: mkManifest("t1", "1")},
		{Source: "a", Manifest: mkManifest("t2", "1")},
	}
	snap := newEffectiveToolset(1, time.Now(), tools)
	first := snap.Names()
	first[0] = "MUTATED"
	second := snap.Names()
	if second[0] == "MUTATED" {
		t.Errorf("Names() shares backing array — second call returned mutated name")
	}
}

func TestEffectiveToolset_LenMatches(t *testing.T) {
	t.Parallel()
	tools := []EffectiveTool{
		{Source: "a", Manifest: mkManifest("t1", "1")},
		{Source: "a", Manifest: mkManifest("t2", "1")},
		{Source: "b", Manifest: mkManifest("t3", "1")},
	}
	snap := newEffectiveToolset(1, time.Now(), tools)
	if snap.Len() != 3 {
		t.Errorf("Len: got %d, want 3", snap.Len())
	}
}

func TestEffectiveToolset_RevisionAndBuiltAtStamped(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 11, 12, 0, 0, 0, time.UTC)
	snap := newEffectiveToolset(42, now, nil)
	if snap.Revision != 42 {
		t.Errorf("Revision: got %d, want 42", snap.Revision)
	}
	if !snap.BuiltAt.Equal(now) {
		t.Errorf("BuiltAt: got %v, want %v", snap.BuiltAt, now)
	}
}
