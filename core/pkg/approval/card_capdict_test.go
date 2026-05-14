package approval

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/capdict"
)

// TestRenderApprovalCard_WiredCapdictTranslator_RendersHumanReadableLine
// is the M9.3.a end-to-end pin between the production
// `dict/capabilities.yaml`, the capdict loader, and the approval
// card renderer. Walks from yaml-on-disk → Dictionary →
// CapabilityTranslator → rendered Block-Kit slice, and asserts the
// rendered card carries the lead-facing plain-language description
// for two representative capability ids (`github:read`,
// `tool:share`). A regression on either side (loader silently
// drops a row, renderer drops the translator) surfaces here as a
// missing substring.
//
// Distinct from the existing translator tests
// (`TestRenderApprovalCard_CapabilityTranslator_Used`) — that
// test uses a hand-rolled translator. This test wires the REAL
// dictionary so a future yaml-shape change (per-id object → list,
// etc.) is caught at the renderer boundary.
func TestRenderApprovalCard_WiredCapdictTranslator_RendersHumanReadableLine(t *testing.T) {
	raw, err := os.ReadFile(realDictPathForCardTest(t))
	if err != nil {
		t.Fatalf("read real dict/capabilities.yaml: %v", err)
	}
	d, err := capdict.LoadFromBytes(raw)
	if err != nil {
		t.Fatalf("capdict.LoadFromBytes(real dict/capabilities.yaml): %v", err)
	}
	translate := capdict.Translator(d)

	in := newTestCardInput()
	in.Capabilities = []string{"github:read", "tool:share"}

	blocks, _, err := RenderApprovalCard(in, translate)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	joined := jsonBlocksString(t, blocks)

	// Real-yaml description fragments. Asserting on a stable
	// substring rather than the full text so a future yaml copy
	// edit (style polish, typo fix) does not break the pin.
	wantSubstrings := []string{
		"Read GitHub issues", // dict/capabilities.yaml "github:read"
		"Propose sharing",    // dict/capabilities.yaml "tool:share"
	}
	for _, s := range wantSubstrings {
		if !strings.Contains(joined, s) {
			t.Errorf("rendered card missing wired-translator substring %q; rendered: %s", s, joined)
		}
	}

	// The dictionary-not-loaded fallback MUST NOT appear when the
	// translator is wired — that placeholder is reserved for the
	// nil-translator degradation path.
	if strings.Contains(joined, FallbackTranslationDictionaryNotLoaded) {
		t.Errorf("wired translator must not surface dictionary-not-loaded placeholder %q; rendered: %s", FallbackTranslationDictionaryNotLoaded, joined)
	}
}

// realDictPathForCardTest resolves the on-disk path to the real
// `dict/capabilities.yaml` regardless of the test's working
// directory. Mirrors the capdict-side helper but lives here so the
// approval test stays self-contained against capdict's _test.go
// boundary.
func realDictPathForCardTest(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) failed")
	}
	// approval test file lives at core/pkg/approval/; the real yaml
	// is at <repo-root>/dict/capabilities.yaml — three levels up.
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	path := filepath.Join(repoRoot, "dict", "capabilities.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("real dictionary file unreadable at %s: %v", path, err)
	}
	return path
}
