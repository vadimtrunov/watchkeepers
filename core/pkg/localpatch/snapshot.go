package localpatch

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
)

// historyDirName is the on-disk parent directory for snapshots,
// SIBLING to the M9.1.b `tools/` source root the
// [toolregistry.Registry] scanner walks. Keeping `_history/` outside
// `tools/` is load-bearing: a snapshot tree under
// `tools/<source>/_history/<tool>/<version>/` would be observed by
// the M9.1.b scanner as a "tool" directory and emit decode failures
// into the registry log on every recompute.
const historyDirName = "_history"

// snapshotPath returns the absolute on-disk directory for a (source,
// tool, version) snapshot. The path is `<dataDir>/_history/<source>/<tool>/<version>/`;
// each path component is validated against the [validIdentifier]
// allowlist BEFORE composition so a `..` / `/` slip cannot escape
// the history root.
//
// Returns [ErrUnsafePath] when any component is empty / over-bound /
// contains a path-traversal byte. The check is belt-and-braces over
// the per-field validators on [InstallRequest] / [RollbackRequest].
func snapshotPath(dataDir, sourceName, toolName, version string) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", fmt.Errorf("%w: empty data dir", ErrUnsafePath)
	}
	for _, part := range []struct {
		kind  string
		value string
		ok    bool
	}{
		{"source", sourceName, validIdentifier.MatchString(sourceName)},
		{"tool", toolName, validIdentifier.MatchString(toolName)},
		{"version", version, validVersion.MatchString(version)},
	} {
		if !part.ok {
			return "", fmt.Errorf("%w: %s %q", ErrUnsafePath, part.kind, part.value)
		}
	}
	parent := filepath.Clean(filepath.Join(dataDir, historyDirName))
	cand := filepath.Clean(filepath.Join(parent, sourceName, toolName, version))
	rel, err := filepath.Rel(parent, cand)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: snapshot %q/%q/%q", ErrUnsafePath, sourceName, toolName, version)
	}
	return cand, nil
}

// liveToolPath returns the absolute on-disk directory for the live
// tool tree the M9.1.b scanner walks: `<dataDir>/tools/<source>/<tool>/`.
// Same allowlist + traversal-defence discipline as [snapshotPath].
func liveToolPath(dataDir, sourceName, toolName string) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", fmt.Errorf("%w: empty data dir", ErrUnsafePath)
	}
	for _, part := range []struct{ kind, value string }{
		{"source", sourceName},
		{"tool", toolName},
	} {
		if !validIdentifier.MatchString(part.value) {
			return "", fmt.Errorf("%w: %s %q", ErrUnsafePath, part.kind, part.value)
		}
	}
	parent := filepath.Clean(filepath.Join(dataDir, "tools", sourceName))
	cand := filepath.Clean(filepath.Join(parent, toolName))
	rel, err := filepath.Rel(parent, cand)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: live tool %q/%q", ErrUnsafePath, sourceName, toolName)
	}
	return cand, nil
}

// copyTree copies every regular file under `src` to `dst`,
// preserving the relative directory layout and the executable bit.
// Symlink-cycle-safe via the same realpath visited set as
// [walkRegularFiles]. Returns wrapped [ErrLiveWrite] /
// [ErrSnapshotWrite] depending on the caller-supplied `wrap`
// sentinel so the operator's errors.Is triage stays unambiguous.
//
// `dst` is created via [FS.MkdirAll] (idempotent). The caller is
// responsible for any pre-copy `RemoveAll(dst)` when overwriting
// an existing tree.
func copyTree(filesystem FS, src, dst string, wrap error) error {
	entries, err := walkRegularFiles(filesystem, src)
	if err != nil {
		return err
	}
	if err := filesystem.MkdirAll(dst, 0o755); err != nil {
		return fmt.Errorf("%w: mkdir %q: %w", wrap, dst, err)
	}
	for _, e := range entries {
		dstPath := filepath.Join(dst, filepath.FromSlash(e.rel))
		if err := filesystem.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
			return fmt.Errorf("%w: mkdir %q: %w", wrap, filepath.Dir(dstPath), err)
		}
		mode := fs.FileMode(0o644)
		if e.executable {
			mode = 0o755
		}
		if err := filesystem.WriteFile(dstPath, e.content, mode); err != nil {
			return fmt.Errorf("%w: write %q: %w", wrap, dstPath, err)
		}
	}
	return nil
}

// snapshotIfPresent copies the live tool tree to its snapshot path
// IF the live tree exists. If the live tree is absent (a
// first-install of a brand-new tool), the function is a no-op AND
// returns `(false, nil)`. Returns `(true, nil)` after a successful
// snapshot.
//
// The version under which the live tree is snapshotted comes from
// the live tree's own `manifest.json`, decoded via
// [toolregistry.LoadManifestFromFile] (the caller passes that result
// in via `priorVersion`). When the live tree has no decodable
// manifest, the caller MUST refuse the install — overwriting an
// undecidable tree would lose the prior contents without a
// restorable snapshot.
//
// Snapshot collisions: if a snapshot already exists at the target
// version (operator double-installed the same version), the existing
// snapshot is preserved (NOT overwritten). The operator's audit
// trail therefore retains the FIRST install's contents and the
// repeat install just bumps the live tree.
func snapshotIfPresent(filesystem FS, dataDir, sourceName, toolName, priorVersion string) (bool, error) {
	livePath, err := liveToolPath(dataDir, sourceName, toolName)
	if err != nil {
		return false, err
	}
	info, err := filesystem.Stat(livePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("%w: stat live %q: %w", ErrFolderRead, livePath, err)
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%w: live tool path is not a directory: %q", ErrFolderRead, livePath)
	}
	snapPath, err := snapshotPath(dataDir, sourceName, toolName, priorVersion)
	if err != nil {
		return false, err
	}
	// Snapshot-collision policy: preserve the FIRST snapshot.
	if _, err := filesystem.Stat(snapPath); err == nil {
		return true, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("%w: stat snapshot %q: %w", ErrSnapshotWrite, snapPath, err)
	}
	if err := copyTree(filesystem, livePath, snapPath, ErrSnapshotWrite); err != nil {
		return false, err
	}
	return true, nil
}

// replaceLive replaces the live tool tree with the contents of `src`.
// The live tree is removed first (so a smaller new tree does not
// inherit stale files from the prior tree), then `src` is copied in
// fresh. Atomicity caveat: a process crash between RemoveAll and the
// final WriteFile leaves the live tree partially populated; the
// operator triages by inspecting the snapshot tree at
// `<DataDir>/_history/<source>/<tool>/<priorVersion>/` and
// re-running rollback to restore.
func replaceLive(filesystem FS, dataDir, sourceName, toolName, src string) error {
	livePath, err := liveToolPath(dataDir, sourceName, toolName)
	if err != nil {
		return err
	}
	if err := filesystem.RemoveAll(livePath); err != nil {
		return fmt.Errorf("%w: remove live %q: %w", ErrLiveWrite, livePath, err)
	}
	return copyTree(filesystem, src, livePath, ErrLiveWrite)
}

// listSnapshots returns the version directory names under
// `<dataDir>/_history/<source>/<tool>/` in lexicographic order. Used
// by the rollback CLI's diagnostic surface when an operator passes
// a `--to <version>` that does not exist — the caller renders the
// available versions so the operator can correct.
//
// Returns `(nil, nil)` when the snapshot parent does not exist
// (no snapshots have ever been taken for this tool).
func listSnapshots(filesystem FS, dataDir, sourceName, toolName string) ([]string, error) {
	if !validIdentifier.MatchString(sourceName) {
		return nil, fmt.Errorf("%w: source %q", ErrUnsafePath, sourceName)
	}
	if !validIdentifier.MatchString(toolName) {
		return nil, fmt.Errorf("%w: tool %q", ErrUnsafePath, toolName)
	}
	parent := filepath.Clean(filepath.Join(dataDir, historyDirName, sourceName, toolName))
	entries, err := filesystem.ReadDir(parent)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: readdir %q: %w", ErrFolderRead, parent, err)
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}
