package capdict

import (
	"sort"
	"testing"
)

// TestCanonicalCapabilities_Sorted pins the in-code slice as sorted.
// The "adding a capability" workflow inserts the new id in sorted
// order so a future reader can confirm coverage by eyeballing; a
// random-order append would still pass the bijection test but
// degrade the operator-readability contract.
func TestCanonicalCapabilities_Sorted(t *testing.T) {
	sorted := append([]string(nil), CanonicalCapabilities...)
	sort.Strings(sorted)
	if !equalSlices(CanonicalCapabilities, sorted) {
		t.Fatalf("CanonicalCapabilities must be sorted: got %v, want %v", CanonicalCapabilities, sorted)
	}
}

// TestCanonicalCapabilities_NoDuplicates pins the in-code slice as
// duplicate-free. A duplicate would silently inflate the bijection
// test's `len()` check on one side and mask a missing yaml row on
// the other.
func TestCanonicalCapabilities_NoDuplicates(t *testing.T) {
	seen := make(map[string]struct{}, len(CanonicalCapabilities))
	for _, id := range CanonicalCapabilities {
		if _, ok := seen[id]; ok {
			t.Errorf("CanonicalCapabilities: duplicate id %q", id)
		}
		seen[id] = struct{}{}
	}
}

// TestCapabilities_BijectionWithCanonicalSet is THE CI completeness
// gate. Two directions + len symmetry:
//
//  1. Forward: every [CanonicalCapabilities] id has a matching yaml
//     row (and a non-empty description).
//  2. Reverse: every yaml row's id is in [CanonicalCapabilities].
//  3. Length: `len(yaml.Capabilities()) == len(CanonicalCapabilities)`.
//
// Mirrors M9.7's `TestEventTypeVocabulary_RoadmapNames` (lessons
// pattern #6 — a two-way closed-set pin catches asymmetric drift).
func TestCapabilities_BijectionWithCanonicalSet(t *testing.T) {
	d := loadRealDictionary(t)

	yamlIDs := d.Capabilities()
	canonicalSet := make(map[string]struct{}, len(CanonicalCapabilities))
	for _, id := range CanonicalCapabilities {
		canonicalSet[id] = struct{}{}
	}
	yamlSet := make(map[string]struct{}, len(yamlIDs))
	for _, id := range yamlIDs {
		yamlSet[id] = struct{}{}
	}

	// Forward direction: every canonical id has a yaml entry.
	for _, id := range CanonicalCapabilities {
		desc, ok := d.Translate(id)
		if !ok {
			t.Errorf("forward bijection: canonical id %q has no yaml entry", id)
			continue
		}
		if desc == "" {
			t.Errorf("forward bijection: canonical id %q yaml entry has empty description", id)
		}
	}

	// Reverse direction: every yaml id is in canonical.
	for _, id := range yamlIDs {
		if _, ok := canonicalSet[id]; !ok {
			t.Errorf("reverse bijection: yaml id %q is not in CanonicalCapabilities", id)
		}
	}

	// Length symmetric pin: catches the case where forward + reverse
	// both pass but one side carries a duplicate that masks a miss.
	if len(yamlIDs) != len(CanonicalCapabilities) {
		t.Errorf("length symmetry: len(yaml)=%d len(canonical)=%d", len(yamlIDs), len(CanonicalCapabilities))
	}
}
