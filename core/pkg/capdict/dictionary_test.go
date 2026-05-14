package capdict

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// minimal valid yaml fixture used by the structural tests below.
const minimalYAML = `
version: 1
capabilities:
  "github:read":
    description: Read GitHub issues and pull requests.
`

func TestLoadFromBytes_HappyPath(t *testing.T) {
	d, err := LoadFromBytes([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("LoadFromBytes happy path: unexpected err: %v", err)
	}
	if got := d.Version(); got != SupportedSchemaVersion {
		t.Errorf("Version: got %d, want %d", got, SupportedSchemaVersion)
	}
	if got := d.Len(); got != 1 {
		t.Errorf("Len: got %d, want 1", got)
	}
	desc, ok := d.Translate("github:read")
	if !ok {
		t.Fatalf("Translate(github:read): ok=false on a present id")
	}
	if !strings.Contains(desc, "GitHub") {
		t.Errorf("Translate description: got %q, want a substring %q", desc, "GitHub")
	}
}

func TestLoadFromBytes_EmptyPayloadRejected(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"nil", nil},
		{"empty", []byte("")},
		{"whitespace", []byte("   \n\t\n")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadFromBytes(c.in)
			if !errors.Is(err, ErrInvalidDictionary) {
				t.Fatalf("err: got %v, want wrap of ErrInvalidDictionary", err)
			}
		})
	}
}

func TestLoadFromBytes_UnknownTopLevelKeyRejected(t *testing.T) {
	in := `
version: 1
capabilities:
  "github:read":
    description: ok
typo: should-be-rejected
`
	_, err := LoadFromBytes([]byte(in))
	if !errors.Is(err, ErrInvalidDictionary) {
		t.Fatalf("err: got %v, want wrap of ErrInvalidDictionary", err)
	}
	if !strings.Contains(err.Error(), "typo") {
		t.Errorf("err.Error() should mention the offending key %q; got %q", "typo", err.Error())
	}
}

func TestLoadFromBytes_UnknownPerCapKeyRejected(t *testing.T) {
	in := `
version: 1
capabilities:
  "github:read":
    description: ok
    descriiption: typo
`
	_, err := LoadFromBytes([]byte(in))
	if !errors.Is(err, ErrInvalidDictionary) {
		t.Fatalf("err: got %v, want wrap of ErrInvalidDictionary", err)
	}
}

func TestLoadFromBytes_UnsupportedVersionRejected(t *testing.T) {
	in := `
version: 99
capabilities:
  "github:read":
    description: ok
`
	_, err := LoadFromBytes([]byte(in))
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("err: got %v, want wrap of ErrUnsupportedVersion", err)
	}
	if !errors.Is(err, ErrUnsupportedVersion) || !strings.Contains(err.Error(), "99") {
		t.Errorf("err should carry got=99 detail; got %q", err.Error())
	}
}

func TestLoadFromBytes_NoCapabilitiesRejected(t *testing.T) {
	in := `
version: 1
capabilities: {}
`
	_, err := LoadFromBytes([]byte(in))
	if !errors.Is(err, ErrInvalidDictionary) {
		t.Fatalf("err: got %v, want wrap of ErrInvalidDictionary", err)
	}
}

func TestLoadFromBytes_EmptyDescriptionRejected(t *testing.T) {
	in := `
version: 1
capabilities:
  "github:read":
    description: "   "
`
	_, err := LoadFromBytes([]byte(in))
	if !errors.Is(err, ErrEmptyDescription) {
		t.Fatalf("err: got %v, want wrap of ErrEmptyDescription", err)
	}
}

func TestLoadFromBytes_InvalidCapabilityIDRejected(t *testing.T) {
	cases := []struct {
		name string
		id   string
	}{
		{"empty segment", "github::read"},
		{"uppercase letter", "GitHub:read"},
		{"hyphen", "github-read"},
		{"dot", "github.read"},
		{"space", "github read"},
		{"leading colon", ":github:read"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := "version: 1\ncapabilities:\n  \"" + c.id + "\":\n    description: ok\n"
			_, err := LoadFromBytes([]byte(in))
			if !errors.Is(err, ErrInvalidCapabilityID) {
				t.Fatalf("id %q: got %v, want wrap of ErrInvalidCapabilityID", c.id, err)
			}
		})
	}
}

func TestLoadFromBytes_IDTooLongRejected(t *testing.T) {
	longID := strings.Repeat("a", MaxCapabilityIDLength+1)
	in := "version: 1\ncapabilities:\n  \"" + longID + "\":\n    description: ok\n"
	_, err := LoadFromBytes([]byte(in))
	if !errors.Is(err, ErrInvalidCapabilityID) {
		t.Fatalf("err: got %v, want wrap of ErrInvalidCapabilityID", err)
	}
}

func TestLoadFromBytes_DescriptionTooLongRejected(t *testing.T) {
	longDesc := strings.Repeat("x", MaxDescriptionLength+1)
	in := "version: 1\ncapabilities:\n  \"github:read\":\n    description: \"" + longDesc + "\"\n"
	_, err := LoadFromBytes([]byte(in))
	if !errors.Is(err, ErrDescriptionTooLong) {
		t.Fatalf("err: got %v, want wrap of ErrDescriptionTooLong", err)
	}
	if !errors.Is(err, ErrInvalidDictionary) {
		t.Errorf("err must also chain to ErrInvalidDictionary umbrella; got %v", err)
	}
}

func TestLoadFromBytes_ControlByteInDescriptionRejected(t *testing.T) {
	// raw NL inside a quoted scalar would be a yaml error before our
	// check; use a literal escape that yaml.v3 decodes to a NL inside
	// the value.
	in := "version: 1\ncapabilities:\n  \"github:read\":\n    description: \"ok\\nfollow-up\"\n"
	_, err := LoadFromBytes([]byte(in))
	if !errors.Is(err, ErrEmbeddedControlByte) {
		t.Fatalf("err: got %v, want wrap of ErrEmbeddedControlByte", err)
	}
	if !errors.Is(err, ErrInvalidDictionary) {
		t.Errorf("err must also chain to ErrInvalidDictionary umbrella; got %v", err)
	}
}

func TestDictionary_TranslateOrError_HitReturnsDescription(t *testing.T) {
	d, err := LoadFromBytes([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	got, err := d.TranslateOrError("github:read")
	if err != nil {
		t.Fatalf("TranslateOrError(hit): unexpected err: %v", err)
	}
	if !strings.Contains(got, "GitHub") {
		t.Errorf("description: got %q, want substring %q", got, "GitHub")
	}
}

func TestLoadFromBytes_DuplicateKeyRejected(t *testing.T) {
	// yaml.v3 strict mode rejects duplicate keys at decode time.
	// The loader chains the yaml error under ErrInvalidDictionary so
	// callers can match the umbrella sentinel; a yaml.v3 behavioural
	// change (silent overwrite, last-wins) would regress this pin.
	in := `
version: 1
capabilities:
  "github:read":
    description: first
  "github:read":
    description: second
`
	_, err := LoadFromBytes([]byte(in))
	if err == nil {
		t.Fatalf("duplicate key: expected an error, got nil")
	}
	if !errors.Is(err, ErrInvalidDictionary) {
		t.Fatalf("err must chain to ErrInvalidDictionary; got %v", err)
	}
	// yaml.v3 surfaces duplicate-key detection with the substring
	// "already defined" in its error message; pinning the substring
	// catches a yaml.v3 release that quietly stops emitting it.
	if !strings.Contains(err.Error(), "already defined") {
		t.Errorf("err.Error() should carry yaml.v3 duplicate-key detail; got %q", err.Error())
	}
}

func TestLoadFromBytes_MultiDocumentRejected(t *testing.T) {
	in := `
version: 1
capabilities:
  "github:read":
    description: ok
---
version: 1
capabilities:
  "github:write":
    description: ok
`
	_, err := LoadFromBytes([]byte(in))
	if !errors.Is(err, ErrInvalidDictionary) {
		t.Fatalf("err: got %v, want wrap of ErrInvalidDictionary", err)
	}
	if !strings.Contains(err.Error(), "extra") {
		t.Errorf("err should mention extra document; got %q", err.Error())
	}
}

func TestDictionary_Translate_NilSafe(t *testing.T) {
	var d *Dictionary
	desc, ok := d.Translate("github:read")
	if ok || desc != "" {
		t.Errorf("nil receiver: got (%q, %v), want (empty, false)", desc, ok)
	}
}

func TestDictionary_TranslateOrError_UnknownReturnsSentinel(t *testing.T) {
	d, err := LoadFromBytes([]byte(minimalYAML))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	_, err = d.TranslateOrError("does:not:exist")
	if !errors.Is(err, ErrUnknownCapability) {
		t.Fatalf("err: got %v, want wrap of ErrUnknownCapability", err)
	}
	if !strings.Contains(err.Error(), "does:not:exist") {
		t.Errorf("err should carry the offending id; got %q", err.Error())
	}
}

func TestDictionary_Capabilities_SortedDefensiveCopy(t *testing.T) {
	in := `
version: 1
capabilities:
  "zebra:write":
    description: zebra
  "alpha:read":
    description: alpha
  "mango:list":
    description: mango
`
	d, err := LoadFromBytes([]byte(in))
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	first := d.Capabilities()
	want := []string{"alpha:read", "mango:list", "zebra:write"}
	if !equalSlices(first, want) {
		t.Fatalf("Capabilities: got %v, want %v (sorted)", first, want)
	}
	// Mutate the returned slice; the next call must NOT reflect the
	// mutation (defensive copy contract).
	first[0] = "tampered"
	second := d.Capabilities()
	if !equalSlices(second, want) {
		t.Errorf("post-mutation: got %v, want %v (defensive copy must protect dictionary)", second, want)
	}
}

func TestDictionary_Capabilities_NilSafe(t *testing.T) {
	var d *Dictionary
	if got := d.Capabilities(); got != nil {
		t.Errorf("nil receiver: got %v, want nil", got)
	}
}

func TestDictionary_ConcurrentTranslate_NoRace(t *testing.T) {
	d := mustLoadRealForRaceTest(t)
	const goroutines = 16
	const iterations = 25
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				for _, id := range CanonicalCapabilities {
					_, _ = d.Translate(id)
				}
			}
		}()
	}
	wg.Wait()
}

func mustLoadRealForRaceTest(t *testing.T) *Dictionary {
	t.Helper()
	return loadRealDictionary(t)
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
