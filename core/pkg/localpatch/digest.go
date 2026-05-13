package localpatch

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// ContentDigest computes the lower-hex SHA256 of the canonicalised
// contents of a tool directory. The digest is stable across:
//
//   - filesystem ordering (entries are sorted by relative path before
//     hashing so two filesystems returning entries in different
//     [FS.ReadDir] orders produce identical digests);
//   - mtime / atime / ctime (only file contents and relative paths
//     are hashed — never timestamps);
//   - permission bits beyond the executable flag (mode is collapsed
//     to "regular file" or "executable file" because operator-host
//     umask differences would otherwise change the digest without a
//     content change).
//
// The digest format is a length-prefixed concatenation of `(rel_path,
// content)` records. Length-prefixing each field defends against the
// classic "pathname collision" attack (two different trees producing
// the same byte stream by shifting separator bytes) — without it,
// `(a, bc)` and `(ab, c)` would hash identically.
//
// Symlink handling: the walker uses [FS.Lstat] semantics and SKIPS
// symlinks (and other non-regular non-directory entries) silently.
// This is a deliberate refusal-to-follow stance — following symlinks
// would either require a realpath visited set (per-OS quirks around
// `filepath.EvalSymlinks` make it fragile to test in-memory) or
// leave a cycle vulnerability. Tool authors stage regular files
// only; the M9.4.d CI's `undeclared_fs_net` gate already enforces
// the same shape upstream of any tool reaching this digest path.
//
// The digest is suitable as the `diff_hash` audit field on
// [LocalPatchApplied] — bounded length (64 chars), no PII surface
// (it is a cryptographic summary of the tool source), and stable
// across re-installs of byte-identical content (so a downstream
// "did anything actually change?" check is cheap).
func ContentDigest(filesystem FS, root string) (string, error) {
	if filesystem == nil {
		panic("localpatch: ContentDigest: fs must not be nil")
	}
	if strings.TrimSpace(root) == "" {
		return "", fmt.Errorf("%w: empty root", ErrFolderRead)
	}
	cleanedRoot := filepath.Clean(root)
	entries, err := walkRegularFiles(filesystem, cleanedRoot)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].rel < entries[j].rel
	})
	h := sha256.New()
	for _, e := range entries {
		writeLengthPrefixed(h, []byte(e.rel))
		modeFlag := byte(0)
		if e.executable {
			modeFlag = 1
		}
		_, _ = h.Write([]byte{modeFlag})
		writeLengthPrefixed(h, e.content)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// walkEntry is an intermediate (relative-path, content, executable
// bit) record produced by [walkRegularFiles]. Only regular files
// land here — directories are recursed into; non-regular entries
// (symlinks, devices, fifos) are skipped with no error so a tool
// tree containing harmless local-only build artifacts (e.g. a
// dangling `.git/HEAD -> ../HEAD` symlink under a hand-staged
// folder) still digests cleanly.
type walkEntry struct {
	rel        string
	content    []byte
	executable bool
}

// walkRegularFiles walks `root` recursively and returns every
// regular file's (relative path, content, exec-bit) tuple. The
// walker uses [FS.Lstat] semantics and SKIPS symlinks (and other
// non-regular non-directory entries), so a symlink cycle is
// impossible by construction.
//
// `root` MUST be cleaned by the caller. The returned `rel` field is
// always slash-separated regardless of the underlying OS path
// separator — same canonicalisation discipline that lets the digest
// stay stable across operator hosts.
func walkRegularFiles(filesystem FS, root string) ([]walkEntry, error) {
	out := make([]walkEntry, 0, 16)
	if err := walkInto(filesystem, root, root, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// walkInto is the recursive worker for [walkRegularFiles]. Iter-1
// codex C1 fix: replaced the `filepath.Abs` realpath visited set
// (which was lexical, not path-resolving — empty defence on a
// symlink cycle) with a non-following [FS.Lstat] discipline. A
// symlink encountered during the walk is skipped silently, which
// makes a cycle impossible by construction. Tool authors stage
// regular files only; the M9.4.d CI's `undeclared_fs_net` gate
// already enforces the shape upstream.
func walkInto(filesystem FS, root, dir string, out *[]walkEntry) error {
	entries, err := filesystem.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("%w: readdir %q: %w", ErrFolderRead, dir, err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, e := range entries {
		// Iter-1 codex m1 fix: consult `e.Type()` first to avoid an
		// extra Lstat round-trip per entry. Lstat is needed only for
		// regular-file entries (to get the exec-bit) AND for entries
		// whose Type() is the zero value (some FS implementations
		// return zero from ReadDir and require a follow-up Stat).
		t := e.Type()
		full := filepath.Join(dir, e.Name())
		switch {
		case t.IsDir():
			if err := walkInto(filesystem, root, full, out); err != nil {
				return err
			}
		case t&fs.ModeSymlink != 0:
			// Skip — see walkInto godoc.
		case t.IsRegular() || t == 0:
			info, err := filesystem.Lstat(full)
			if err != nil {
				return fmt.Errorf("%w: lstat %q: %w", ErrFolderRead, full, err)
			}
			if !info.Mode().IsRegular() {
				// A symlink reported as zero-Type by ReadDir but
				// resolved as non-regular by Lstat — skip.
				continue
			}
			content, err := filesystem.ReadFile(full)
			if err != nil {
				return fmt.Errorf("%w: readfile %q: %w", ErrFolderRead, full, err)
			}
			rel, err := filepath.Rel(root, full)
			if err != nil {
				return fmt.Errorf("%w: rel %q: %w", ErrFolderRead, full, err)
			}
			rel = filepath.ToSlash(rel)
			*out = append(*out, walkEntry{
				rel:        rel,
				content:    content,
				executable: info.Mode()&0o111 != 0,
			})
		default:
			// Skip non-regular, non-directory entries (sockets,
			// devices, fifos). A tool tree that somehow contains
			// them is malformed but the digest should still be
			// computable for the regular-file portion.
		}
	}
	return nil
}

// writeLengthPrefixed writes a 64-bit big-endian length followed by
// `data` to `h`. The length prefix turns the (path, content) record
// into an unambiguous byte stream so two trees with permuted
// separators cannot collide. The narrow [hash.Hash] type is iter-1
// codex M6 fix — a wider `interface{ Write }` would let a future
// caller pass a real `io.Writer` whose IO errors this helper silently
// drops.
func writeLengthPrefixed(h hash.Hash, data []byte) {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(len(data)))
	_, _ = h.Write(buf[:])
	_, _ = h.Write(data)
}

// FS is the filesystem seam consumed by [ContentDigest], the
// snapshot/restore helpers, and both [Installer.Install] /
// [Rollbacker.Rollback]. Production wiring satisfies it via [OSFS];
// tests substitute a hand-rolled in-memory implementation.
//
// Method contract:
//
//   - MkdirAll behaves like [os.MkdirAll]: idempotent, ok-if-exists.
//   - ReadDir / ReadFile mirror stdlib semantics. ReadDir on a missing
//     path MUST return an error chain that satisfies
//     `errors.Is(err, fs.ErrNotExist)` so callers can distinguish
//     "directory absent" from "I/O failure".
//   - Stat returns the stdlib [os.ErrNotExist] sentinel chain when
//     the path is absent; follows symlinks (`stat`, not `lstat`) —
//     used by the live-tree-presence check in [Installer.Install]
//     where the operator IS expected to follow a symlinked tools dir.
//   - Lstat returns the stdlib [os.ErrNotExist] sentinel chain when
//     the path is absent; does NOT follow symlinks. The digest
//     walker uses Lstat so a symlink in the tool tree is observed
//     as a symlink (and skipped) rather than recursed-into.
//   - WriteFile writes `data` to `path` atomically-enough for the
//     local-patch use case (the parent dir is assumed pre-created
//     via MkdirAll). Existing files are overwritten.
//   - RemoveAll behaves like [os.RemoveAll]: removes a directory
//     tree, idempotent on a missing path. Used by the live-tools
//     swap so a smaller new tree replaces a larger old tree
//     cleanly.
type FS interface {
	MkdirAll(path string, perm fs.FileMode) error
	ReadDir(path string) ([]fs.DirEntry, error)
	ReadFile(path string) ([]byte, error)
	Stat(path string) (fs.FileInfo, error)
	Lstat(path string) (fs.FileInfo, error)
	WriteFile(path string, data []byte, perm fs.FileMode) error
	RemoveAll(path string) error
}
