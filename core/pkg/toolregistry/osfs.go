package toolregistry

import (
	"io/fs"
	"os"
)

// OSFS is the production [FS] adapter that delegates every method to
// the corresponding `os` / `os.ReadFile` / `os.ReadDir` primitive.
// The zero value is usable; pass `OSFS{}` to [Deps.FS].
//
// Kept as a thin shim so the scheduler stays decoupled from the real
// filesystem in tests — every method here is a one-line forward.
type OSFS struct{}

// MkdirAll implements [FS] via [os.MkdirAll].
func (OSFS) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm) //nolint:gosec // path is operator-derived (DataDir/tools/<sourceName>), not user input
}

// Stat implements [FS] via [os.Stat].
func (OSFS) Stat(path string) (os.FileInfo, error) { return os.Stat(path) }

// ReadFile implements [FS] via [os.ReadFile].
func (OSFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // path is operator-derived (DataDir/tools/<sourceName>/manifest.json), not user input
}

// ReadDir implements [FS] via [os.ReadDir]. The M9.1.b scanner uses
// it to discover per-tool subdirectories under
// `<DataDir>/tools/<sourceName>/`.
func (OSFS) ReadDir(path string) ([]fs.DirEntry, error) {
	return os.ReadDir(path) //nolint:gosec // path is operator-derived (DataDir/tools/<sourceName>), not user input
}
