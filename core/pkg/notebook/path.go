package notebook

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// dirMode is the file mode applied to the notebook directory tree. The
// notebook stores per-agent SQLite files that may contain private memories,
// so only the owner reads (0o700). Verified after [os.MkdirAll] in case the
// process umask interfered.
const dirMode os.FileMode = 0o700

// envDataDir is the override for the data directory containing the
// `notebook/` subtree. When unset, [agentDBPath] falls back to
// `$HOME/.local/share/watchkeepers` on Linux/macOS.
const envDataDir = "WATCHKEEPER_DATA"

// uuidPattern matches the canonical RFC 4122 text form (8-4-4-4-12 hex with
// dashes). Mirrors the validator used by the Keep server's
// `core/internal/keep/server/handlers_write.go` and the keepclient.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ErrInvalidAgentID is returned by [agentDBPath] when the supplied
// `agentID` does not match the canonical UUID shape.
var ErrInvalidAgentID = errors.New("notebook: agent id must be a canonical UUID")

// agentDBPath resolves the on-disk SQLite path for the given agent.
//
// Layout: `<data>/notebook/<agentID>.sqlite` where `<data>` is
// `$WATCHKEEPER_DATA` when set, else `$HOME/.local/share/watchkeepers` on
// Linux/macOS. The `notebook/` directory is created with mode 0o700 if
// missing; the per-agent file itself is created lazily by [Open].
//
// Returns [ErrInvalidAgentID] for malformed UUIDs (no filesystem touched),
// or a wrapped error when the home directory or notebook directory cannot be
// resolved/created.
func agentDBPath(agentID string) (string, error) {
	if !uuidPattern.MatchString(agentID) {
		return "", ErrInvalidAgentID
	}

	dataDir, err := resolveDataDir()
	if err != nil {
		return "", err
	}

	notebookDir := filepath.Join(dataDir, "notebook")
	if err := os.MkdirAll(notebookDir, dirMode); err != nil {
		return "", fmt.Errorf("notebook: create dir %q: %w", notebookDir, err)
	}
	// The umask may have stripped bits we care about. Force 0o700 so a
	// fresh-on-disk directory matches a re-used one.
	if err := os.Chmod(notebookDir, dirMode); err != nil {
		return "", fmt.Errorf("notebook: chmod dir %q: %w", notebookDir, err)
	}

	return filepath.Join(notebookDir, agentID+".sqlite"), nil
}

// resolveDataDir returns the base data directory. `WATCHKEEPER_DATA` wins
// when set; otherwise `$HOME/.local/share/watchkeepers`. Returns an error if
// neither resolves (e.g. unset HOME on a misconfigured CI runner).
func resolveDataDir() (string, error) {
	if v := os.Getenv(envDataDir); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("notebook: resolve home dir: %w", err)
	}
	return filepath.Join(home, ".local", "share", "watchkeepers"), nil
}
