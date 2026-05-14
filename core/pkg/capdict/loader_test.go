package capdict

import (
	"errors"
	"strings"
	"testing"
)

func TestLoadFromFile_HappyPath(t *testing.T) {
	fs := newMapFS(map[string][]byte{
		"dict/capabilities.yaml": []byte(minimalYAML),
	})
	d, err := LoadFromFile(fs, "dict/capabilities.yaml")
	if err != nil {
		t.Fatalf("LoadFromFile happy path: %v", err)
	}
	if d == nil || d.Len() != 1 {
		t.Fatalf("expected one-row dictionary; got len=%d", d.Len())
	}
}

func TestLoadFromFile_NilFS_Rejected(t *testing.T) {
	_, err := LoadFromFile(nil, "dict/capabilities.yaml")
	if !errors.Is(err, ErrInvalidDictionary) {
		t.Fatalf("err: got %v, want wrap of ErrInvalidDictionary", err)
	}
	if !strings.Contains(err.Error(), "fs must not be nil") {
		t.Errorf("err should mention nil-fs; got %q", err.Error())
	}
}

func TestLoadFromFile_EmptyPath_Rejected(t *testing.T) {
	fs := newMapFS(nil)
	_, err := LoadFromFile(fs, "  ")
	if !errors.Is(err, ErrInvalidDictionary) {
		t.Fatalf("err: got %v, want wrap of ErrInvalidDictionary", err)
	}
	if !strings.Contains(err.Error(), "path must not be empty") {
		t.Errorf("err should mention empty-path; got %q", err.Error())
	}
}

func TestLoadFromFile_ReadError_WrappedAndIncludesPath(t *testing.T) {
	fs := newMapFS(map[string][]byte{})
	_, err := LoadFromFile(fs, "dict/missing.yaml")
	if !errors.Is(err, ErrDictionaryRead) {
		t.Fatalf("err: got %v, want wrap of ErrDictionaryRead (distinct from ErrInvalidDictionary; I/O failure)", err)
	}
	if errors.Is(err, ErrInvalidDictionary) {
		t.Errorf("read error must NOT chain to ErrInvalidDictionary; operators must distinguish I/O from structural failure (err=%v)", err)
	}
	if !strings.Contains(err.Error(), "dict/missing.yaml") {
		t.Errorf("err should carry the failing path; got %q", err.Error())
	}
}

// TestLoadFromFile_RealDictionary_LoadsAndContainsCanonicalIDs reads
// the production `dict/capabilities.yaml` end-to-end via the os FS
// adapter and asserts every id in [CanonicalCapabilities] is
// present. Acts as a smoke pin distinct from the bijection test —
// fails loudly if the loader regresses against the actual on-disk
// payload (a yaml shape the unit tests' synthetic fixtures don't
// cover).
func TestLoadFromFile_RealDictionary_LoadsAndContainsCanonicalIDs(t *testing.T) {
	d := loadRealDictionary(t)
	for _, id := range CanonicalCapabilities {
		desc, ok := d.Translate(id)
		if !ok {
			t.Errorf("Translate(%q): not present in real dict/capabilities.yaml", id)
			continue
		}
		if strings.TrimSpace(desc) == "" {
			t.Errorf("Translate(%q): empty description on real load", id)
		}
	}
}
