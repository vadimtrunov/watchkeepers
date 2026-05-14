package capdict

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// mapFS is an in-memory [FS] satisfying the loader's read seam in
// tests. Mirrors `toolregistry/fakes_test.go` mapFS — distinct copy
// because the package boundary forbids leaking test helpers across
// packages.
type mapFS struct {
	files map[string][]byte
	err   error
}

func newMapFS(files map[string][]byte) *mapFS {
	return &mapFS{files: files}
}

func (m *mapFS) ReadFile(path string) ([]byte, error) {
	if m.err != nil {
		return nil, m.err
	}
	b, ok := m.files[path]
	if !ok {
		return nil, fmt.Errorf("mapFS: file not found: %q", path)
	}
	// Defensive copy on read — a caller mutating the returned slice
	// must not bleed into subsequent reads of the same path.
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

// realDictionaryPath resolves the on-disk path to the real
// `dict/capabilities.yaml` regardless of the test's working
// directory. Uses [runtime.Caller] to anchor the lookup at the test
// file's location, then walks three levels up to reach the repo
// root (core/pkg/capdict → core/pkg → core → <root>).
func realDictionaryPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("runtime.Caller(0) failed")
	}
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	path := filepath.Join(repoRoot, "dict", "capabilities.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("real dictionary file unreadable at %s: %v", path, err)
	}
	return path
}

// loadRealDictionary loads the production `dict/capabilities.yaml`
// via [LoadFromFile] using a thin os-backed FS adapter.
func loadRealDictionary(t *testing.T) *Dictionary {
	t.Helper()
	d, err := LoadFromFile(osFS{}, realDictionaryPath(t))
	if err != nil {
		t.Fatalf("LoadFromFile(real dict/capabilities.yaml): %v", err)
	}
	if d == nil {
		t.Fatalf("LoadFromFile returned nil dictionary")
	}
	return d
}

// osFS is a tiny [FS] backed by `os.ReadFile` for the real-yaml
// integration tests. Mirrors the production wiring shape (the
// operator-config FS adapter at boot).
type osFS struct{}

func (osFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
