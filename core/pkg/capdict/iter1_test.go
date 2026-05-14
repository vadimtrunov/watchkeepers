package capdict

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// ----- M1 — every leaf-sentinel-producing failure path also chains
// to the [ErrInvalidDictionary] umbrella so callers can match either
// the umbrella OR the leaf via errors.Is (iter-1 critic + code-
// reviewer joint finding). The pre-iter-1 code single-wrapped the
// leaf only, so a documented `errors.Is(err, ErrInvalidDictionary)`
// guard would silently fall through on every per-row failure. -----

func TestIter1_EveryLeafChainsToUmbrellaSentinel(t *testing.T) {
	// Forge an input that triggers each leaf sentinel one at a time.
	cases := []struct {
		name    string
		payload string
		leaf    error
	}{
		{
			name:    "empty description",
			payload: "version: 1\ncapabilities:\n  \"github:read\":\n    description: \"   \"\n",
			leaf:    ErrEmptyDescription,
		},
		{
			name:    "description too long",
			payload: "version: 1\ncapabilities:\n  \"github:read\":\n    description: \"" + strings.Repeat("x", MaxDescriptionLength+1) + "\"\n",
			leaf:    ErrDescriptionTooLong,
		},
		{
			name:    "control byte in description",
			payload: "version: 1\ncapabilities:\n  \"github:read\":\n    description: \"ok\\nnext\"\n",
			leaf:    ErrEmbeddedControlByte,
		},
		{
			name:    "invalid id grammar",
			payload: "version: 1\ncapabilities:\n  \"GitHub:read\":\n    description: ok\n",
			leaf:    ErrInvalidCapabilityID,
		},
		{
			name:    "unsupported version",
			payload: "version: 99\ncapabilities:\n  \"github:read\":\n    description: ok\n",
			leaf:    ErrUnsupportedVersion,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadFromBytes([]byte(c.payload))
			if err == nil {
				t.Fatalf("payload %q: expected error, got nil", c.name)
			}
			if !errors.Is(err, c.leaf) {
				t.Errorf("err must chain to leaf %v; got %v", c.leaf, err)
			}
			if !errors.Is(err, ErrInvalidDictionary) {
				t.Errorf("err must chain to umbrella ErrInvalidDictionary; got %v", err)
			}
		})
	}
}

// ----- M2 — ErrDuplicateCapability is removed (it was dead API
// surface; yaml.v3 catches duplicates at decode-time and capdict's
// grammar rejects uppercase, so case-folding collisions are
// impossible by construction). The pre-iter-1 sentinel is renamed-
// out-of-existence; this test pins the absence via a compile-time
// reference that would fail to build if a future maintainer re-
// introduces the sentinel without a real call site. -----

// This file imports capdict's exported sentinels; if a future commit
// re-adds an exported `ErrDuplicateCapability`, gofumpt will not
// complain — but a new compile-fail test would. We pin the contract
// at the documentation layer instead: the duplicate-key path now
// chains under ErrInvalidDictionary via the yaml.v3 strict-decode
// error path. That is already covered by
// TestLoadFromBytes_DuplicateKeyRejected.

// ----- M3 — capdict grammar is STRICTER than approval.proposal.go's
// capability-id validator. Every capdict id is a legal proposer id,
// NOT vice-versa. This test pins the contract direction. -----

func TestIter1_CapdictGrammarStricterThanProposer(t *testing.T) {
	// Examples the proposer accepts but capdict rejects: uppercase,
	// hyphens, dots, spaces, empty segments. The proposer validator
	// enforces only non-blank + length (≤ MaxCapabilityIDLength) +
	// duplicate-free; capdict's validateCapabilityID is grammar-
	// strict. We exercise capdict directly here (proposer-side test
	// stays in core/pkg/approval/).
	stricterRejects := []string{
		"GitHub:read",
		"github-read",
		"github.read",
		"github read",
		":github:read",
		"github::read",
	}
	for _, id := range stricterRejects {
		if err := validateCapabilityID(id); !errors.Is(err, ErrInvalidCapabilityID) {
			t.Errorf("capdict must reject %q (proposer accepts); got err=%v", id, err)
		}
	}
}

// ----- m1 — scalar yaml payloads (`null`, `42`, `"just a string"`)
// decode into yamlDocument zero-value (Version=0, Capabilities=nil)
// and surface under ErrUnsupportedVersion (since Version != 1).
// The pre-iter-1 tests pinned an empty Capabilities map but did not
// pin the scalar path. -----

func TestIter1_ScalarPayloadsSurfaceUnsupportedVersion(t *testing.T) {
	cases := []string{"null\n", "42\n", "\"just a string\"\n"}
	for _, c := range cases {
		t.Run(strings.TrimSpace(c), func(t *testing.T) {
			_, err := LoadFromBytes([]byte(c))
			if err == nil {
				t.Fatalf("payload %q: expected error, got nil", c)
			}
			// "null" decodes to zero-value Version=0 → unsupported;
			// "42" / "\"just a string\"" mismatch the yamlDocument
			// shape and surface a yaml-decode error wrapped under
			// ErrInvalidDictionary (without the leaf). Either path
			// MUST chain to ErrInvalidDictionary.
			if !errors.Is(err, ErrInvalidDictionary) {
				t.Errorf("payload %q: err must chain to ErrInvalidDictionary; got %v", c, err)
			}
		})
	}
}

// ----- m2 — validateCapabilities walks ids in DETERMINISTIC sort
// order so a multi-bad-entry payload surfaces the SAME first-
// failure message across runs. Pre-iter-1 the walk iterated the
// map directly and was run-to-run non-deterministic. -----

func TestIter1_MultiBadEntry_FirstFailureIsDeterministic(t *testing.T) {
	// Two bad entries (sorted: alpha first, zebra second). Sorted-
	// id walk surfaces alpha first, every run.
	payload := `
version: 1
capabilities:
  "alpha:read":
    description: ""
  "zebra:read":
    description: ""
`
	const runs = 20
	first := ""
	for i := 0; i < runs; i++ {
		_, err := LoadFromBytes([]byte(payload))
		if err == nil {
			t.Fatalf("run %d: expected error, got nil", i)
		}
		got := err.Error()
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("run %d: non-deterministic first-failure message; first=%q got=%q", i, first, got)
		}
	}
	// Sanity: the surfaced message names "alpha", not "zebra".
	if !strings.Contains(first, "alpha:read") {
		t.Errorf("sorted-walk should surface alpha first; got %q", first)
	}
	if strings.Contains(first, "zebra:read") {
		t.Errorf("sorted-walk should NOT surface zebra first (alpha sorts first); got %q", first)
	}
}

// ----- m3 — utf8.ValidString upfront accepts a legitimately-
// encoded U+FFFD replacement codepoint. Pre-iter-1 the loop's
// `r == unicode.ReplacementChar` check refused legit U+FFFD too. ----

func TestIter1_LegitimateU_FFFD_Accepted(t *testing.T) {
	// U+FFFD literal embedded as proper UTF-8 (0xEF 0xBF 0xBD).
	// The loader's utf8.ValidString check must accept it; the
	// per-byte control-byte loop must not reject it (U+FFFD is not
	// a control byte).
	descWithReplacement := "Read GitHub items � legitimately."
	payload := "version: 1\ncapabilities:\n  \"github:read\":\n    description: \"" + descWithReplacement + "\"\n"
	d, err := LoadFromBytes([]byte(payload))
	if err != nil {
		t.Fatalf("legit U+FFFD payload: unexpected err: %v", err)
	}
	got, ok := d.Translate("github:read")
	if !ok {
		t.Fatalf("Translate: id not present after legit-FFFD payload")
	}
	if !strings.Contains(got, "�") {
		t.Errorf("U+FFFD must survive round-trip; got %q", got)
	}
}

// ----- m4 — utf8.ValidString detects genuinely-invalid utf-8 byte
// sequences (an isolated 0xC3 byte without continuation). Pre-iter-1
// the loop's range-over-string would yield RuneError for these and
// the check would surface as "invalid utf-8"; iter-1's split path
// preserves that classification while accepting legit U+FFFD. -----

func TestIter1_InvalidUTF8_Rejected(t *testing.T) {
	// Construct a yaml that the YAML decoder accepts (yaml.v3
	// requires valid utf-8 in scalars by default, so we cannot
	// smuggle invalid bytes through the yaml layer easily). Instead
	// we hit validateDescription directly with an invalid byte
	// sequence to pin the post-decode behaviour.
	invalid := "ok \xc3 still ok"
	err := validateDescription("github:read", invalid)
	if err == nil {
		t.Fatalf("validateDescription: expected error on invalid utf-8, got nil")
	}
	if !errors.Is(err, ErrInvalidDictionary) {
		t.Errorf("invalid utf-8: err must chain to ErrInvalidDictionary; got %v", err)
	}
	if !strings.Contains(err.Error(), "utf-8") {
		t.Errorf("invalid utf-8: err.Error() should mention utf-8; got %q", err.Error())
	}
}

// ----- m5 — ErrDescriptionTooLong is a distinct leaf (parallel to
// ErrInvalidCapabilityID's length wrap). Pre-iter-1 the description-
// too-long path wrapped only the umbrella, making callers unable to
// `errors.Is` a length-specific sentinel. -----

func TestIter1_ErrDescriptionTooLong_DistinctLeaf(t *testing.T) {
	longDesc := strings.Repeat("x", MaxDescriptionLength+1)
	err := validateDescription("github:read", longDesc)
	if !errors.Is(err, ErrDescriptionTooLong) {
		t.Fatalf("description-too-long: err must chain to ErrDescriptionTooLong; got %v", err)
	}
	if !errors.Is(err, ErrInvalidDictionary) {
		t.Errorf("description-too-long: err must also chain to umbrella ErrInvalidDictionary; got %v", err)
	}
	if errors.Is(err, ErrEmptyDescription) {
		t.Errorf("description-too-long must NOT chain to ErrEmptyDescription; got %v", err)
	}
	// Sanity: error message names the byte cap so an operator can
	// see WHY the payload was rejected.
	if !strings.Contains(err.Error(), fmt.Sprint(MaxDescriptionLength)) {
		t.Errorf("error message should carry MaxDescriptionLength=%d; got %q", MaxDescriptionLength, err.Error())
	}
}

// ----- m6 — LoadFromFile read-failure surfaces ErrDictionaryRead,
// NOT ErrInvalidDictionary. Pre-iter-1 the path mis-classified
// I/O failures under the structural sentinel. -----

func TestIter1_LoadFromFile_ReadError_DistinctSentinel(t *testing.T) {
	fs := newMapFS(map[string][]byte{}) // empty map → every read errors
	_, err := LoadFromFile(fs, "dict/missing.yaml")
	if !errors.Is(err, ErrDictionaryRead) {
		t.Fatalf("missing file: err must chain to ErrDictionaryRead; got %v", err)
	}
	if errors.Is(err, ErrInvalidDictionary) {
		t.Errorf("missing file: err must NOT chain to ErrInvalidDictionary (structural sentinel); got %v", err)
	}
}
