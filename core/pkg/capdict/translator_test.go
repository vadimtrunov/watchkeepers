package capdict

import (
	"strings"
	"sync"
	"testing"
)

func TestTranslator_HitReturnsDescription(t *testing.T) {
	d, err := LoadFromBytes([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	tr := Translator(d)
	if tr == nil {
		t.Fatalf("Translator returned nil closure")
	}
	got := tr("github:read")
	if !strings.Contains(got, "GitHub") {
		t.Errorf("Translator hit: got %q, want a substring %q", got, "GitHub")
	}
}

func TestTranslator_MissReturnsEmptyString(t *testing.T) {
	d, err := LoadFromBytes([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	tr := Translator(d)
	if got := tr("does:not:exist"); got != "" {
		t.Errorf("miss: got %q, want empty string (CapabilityTranslator contract)", got)
	}
}

// TestTranslator_NilDictionary_AlwaysEmpty pins the
// degradation-shape contract for the M9.3-pending fallback: a nil
// dictionary returns a non-nil closure that yields the empty
// string for every id, mirroring the approval card's documented
// fallback.
func TestTranslator_NilDictionary_AlwaysEmpty(t *testing.T) {
	tr := Translator(nil)
	if tr == nil {
		t.Fatalf("nil dictionary: Translator must return a non-nil closure")
	}
	for _, id := range []string{"", "github:read", "tool:share"} {
		if got := tr(id); got != "" {
			t.Errorf("nil dictionary translator(%q): got %q, want empty", id, got)
		}
	}
}

// TestTranslator_AssignableToApprovalCapabilityTranslator pins the
// shape compatibility with `approval.CapabilityTranslator` at the Go
// type-system level. A future signature change on either side
// breaks the assertion at COMPILE time so the renderer wiring stays
// safe; declared `var _ approvalLikeTranslator = Translator(nil)`
// with a duplicate type definition that mirrors the approval seam
// (the duplication is intentional — capdict must not import
// approval, see doc.go).
func TestTranslator_AssignableToApprovalCapabilityTranslator(_ *testing.T) {
	type approvalLikeTranslator func(capabilityID string) string
	var _ approvalLikeTranslator = Translator(nil)
}

func TestTranslator_ConcurrentInvocation_NoRace(t *testing.T) {
	d := loadRealDictionary(t)
	tr := Translator(d)
	const goroutines = 16
	const iterations = 25
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				for _, id := range CanonicalCapabilities {
					_ = tr(id)
				}
			}
		}()
	}
	wg.Wait()
}
