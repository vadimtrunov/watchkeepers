package localpatch

import (
	"io/fs"
	"os"
)

// OSFS is the production [FS] implementation: a thin shim around the
// `os` package. Mirror `toolregistry.OSFS`. Production wiring (the
// `wk-tool` CLI) constructs an [OSFS] zero value; tests substitute a
// hand-rolled in-memory fake.
type OSFS struct{}

// MkdirAll implements [FS.MkdirAll].
func (OSFS) MkdirAll(path string, perm fs.FileMode) error {
	return os.MkdirAll(path, perm)
}

// ReadDir implements [FS.ReadDir].
func (OSFS) ReadDir(path string) ([]fs.DirEntry, error) {
	return os.ReadDir(path)
}

// ReadFile implements [FS.ReadFile].
func (OSFS) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}

// Stat implements [FS.Stat]. `os.Stat` follows symlinks; used by
// the live-tree presence check in [Installer.Install].
func (OSFS) Stat(path string) (fs.FileInfo, error) {
	return os.Stat(path)
}

// Lstat implements [FS.Lstat]. `os.Lstat` does NOT follow symlinks;
// used by the digest walker so a symlink is observed as a symlink
// (and skipped) rather than recursed-into.
func (OSFS) Lstat(path string) (fs.FileInfo, error) {
	return os.Lstat(path)
}

// WriteFile implements [FS.WriteFile].
func (OSFS) WriteFile(path string, data []byte, perm fs.FileMode) error {
	return os.WriteFile(path, data, perm)
}

// RemoveAll implements [FS.RemoveAll].
func (OSFS) RemoveAll(path string) error {
	return os.RemoveAll(path)
}
