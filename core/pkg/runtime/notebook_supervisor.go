package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
)

// notebookFileMode is the on-disk permission the supervisor enforces on
// every per-agent SQLite file after [notebook.Open] returns. SQLite
// itself creates the file with the process umask applied to 0o644;
// notebook contents are private memories, so the supervisor narrows the
// mode to owner-only via [os.Chmod] regardless of umask.
const notebookFileMode os.FileMode = 0o600

// notebookSubdir mirrors the layout enforced by `notebook.agentDBPath`:
// per-agent SQLite files live under `<WATCHKEEPER_DATA>/<notebookSubdir>/<id>.sqlite`.
// The supervisor recomputes the path here (rather than asking the
// notebook package) because [notebook.DB] does not expose its on-disk
// path and `agentDBPath` is package-private. Keeping this constant in
// lock-step with `core/pkg/notebook/path.go` is verified by
// [TestNotebookSupervisor_OnDiskFileExists].
const notebookSubdir = "notebook"

// envWatchkeeperData mirrors the env-var override the notebook package
// reads in `agentDBPath` / `resolveDataDir`. The supervisor consults the
// same key so [NotebookSupervisor.Open] can chmod the freshly-created
// SQLite file without round-tripping through a private notebook helper.
const envWatchkeeperData = "WATCHKEEPER_DATA"

// NotebookSupervisor owns the lifecycle of per-agent [notebook.DB]
// handles for a runtime process. The eventual concrete [AgentRuntime]
// (M5.5.c.d / M6) calls [NotebookSupervisor.Open] from its `Start`
// implementation, [NotebookSupervisor.Lookup] from its turn-loop /
// recall layer, and [NotebookSupervisor.Close] from its `Terminate`
// implementation; nothing else mediates per-agent SQLite handles. The
// supervisor is also callable directly so tests and harness-side
// integration code can drive it without a runtime in front.
//
// Concurrency: a single [sync.Mutex] guards the registry map. The
// critical sections are short (map lookup + opaque pointer copy) so a
// global mutex is simpler to reason about than [sync.Map] for the
// open-or-create idempotent pattern. [TestNotebookSupervisor_ConcurrentOpenClose_NoRace]
// exercises this under `-race`.
//
// Idempotency: [NotebookSupervisor.Open] returns the SAME `*notebook.DB`
// on a second call for the same agent (no re-open, no leak).
// [NotebookSupervisor.Close] returns nil for both already-closed and
// never-opened agents — the registry is the source of truth, and the
// underlying `notebook.DB.Close` is itself idempotent.
type NotebookSupervisor struct {
	mu  sync.Mutex
	dbs map[string]*notebook.DB
}

// NewNotebookSupervisor returns a fresh, empty supervisor. Zero-arg by
// design — the supervisor consults `WATCHKEEPER_DATA` (or the
// `$HOME/.local/share/watchkeepers` fallback) at Open time and does not
// need a configured base path here.
func NewNotebookSupervisor() *NotebookSupervisor {
	return &NotebookSupervisor{
		dbs: make(map[string]*notebook.DB),
	}
}

// Open returns the [notebook.DB] handle for `agentID`, opening a fresh
// one (and creating the on-disk SQLite file at
// `<WATCHKEEPER_DATA>/notebook/<agentID>.sqlite`) on the first call.
// Subsequent calls for the same `agentID` return the SAME handle
// pointer until [NotebookSupervisor.Close] removes the entry.
//
// Errors from [notebook.Open] propagate verbatim. Callers can match on
// [notebook.ErrInvalidAgentID] via [errors.Is] to distinguish a
// malformed UUID from infrastructure failures.
//
// On success the supervisor narrows the file mode to 0o600 (owner-only)
// via [os.Chmod] — SQLite itself applies the process umask to 0o644 by
// default, which is too permissive for an agent's private memory.
func (s *NotebookSupervisor) Open(agentID string) (*notebook.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.dbs[agentID]; ok {
		return existing, nil
	}

	// Delegate UUID validation + on-disk file creation to notebook.Open.
	// notebook.Open calls agentDBPath which validates the UUID FIRST,
	// returning notebook.ErrInvalidAgentID before any filesystem touch.
	db, err := notebook.Open(context.Background(), agentID)
	if err != nil {
		return nil, fmt.Errorf("runtime: open notebook for %q: %w", agentID, err)
	}

	// Narrow the freshly-created file's mode to 0o600. The path mirrors
	// the layout `notebook.agentDBPath` enforces; if a future refactor
	// in core/pkg/notebook/ moves the file, [TestNotebookSupervisor_OnDiskFileExists]
	// will catch the drift.
	if path, ok := agentDBFilePath(agentID); ok {
		if err := os.Chmod(path, notebookFileMode); err != nil {
			// Best-effort: the file exists (notebook.Open succeeded);
			// chmod failure is a security-posture regression worth
			// surfacing. Roll back the open so the caller can retry.
			_ = db.Close()
			return nil, fmt.Errorf("runtime: chmod notebook file %q: %w", path, err)
		}
	}

	s.dbs[agentID] = db
	return db, nil
}

// Lookup returns the live [notebook.DB] handle for `agentID` if it has
// been Opened (and not yet Closed); otherwise `(nil, false)`. No errors:
// Lookup is a pure registry read.
//
// The returned pointer is stable for the lifetime of the underlying
// handle — callers can cache it across turns of an agent session as
// long as they coordinate with [NotebookSupervisor.Close].
func (s *NotebookSupervisor) Lookup(agentID string) (*notebook.DB, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, ok := s.dbs[agentID]
	return db, ok
}

// Close releases the [notebook.DB] handle for `agentID` and removes it
// from the registry. Returns `nil` when the agent was never opened or
// has already been closed (idempotent). Returns the error from
// `notebook.DB.Close` when the underlying close fails — but the agent
// is still removed from the registry so a follow-up Open creates a
// fresh handle rather than re-using a half-closed one.
func (s *NotebookSupervisor) Close(agentID string) error {
	s.mu.Lock()
	db, ok := s.dbs[agentID]
	if !ok {
		s.mu.Unlock()
		return nil
	}
	delete(s.dbs, agentID)
	s.mu.Unlock()

	// notebook.DB.Close is itself idempotent (sync.Once-guarded); calling
	// it once here is sufficient even if a paranoid caller races with a
	// second supervisor.Close — the second call won't find the entry and
	// will return nil per the early-return above.
	if err := db.Close(); err != nil {
		return fmt.Errorf("runtime: close notebook for %q: %w", agentID, err)
	}
	return nil
}

// agentDBFilePath recomputes the on-disk path of the per-agent SQLite
// file the way `notebook.agentDBPath` does. Returns `(path, true)` on
// success and `("", false)` if the data dir cannot be resolved (e.g.
// neither `WATCHKEEPER_DATA` nor `HOME` is set). The supervisor uses
// this only for the post-Open chmod; a path-resolution miss just skips
// the chmod and surfaces nothing — `notebook.Open` already proved the
// directory exists, so missing-data-dir here is a programmer error in
// the test harness, not a production code path.
func agentDBFilePath(agentID string) (string, bool) {
	if v := os.Getenv(envWatchkeeperData); v != "" {
		return filepath.Join(v, notebookSubdir, agentID+".sqlite"), true
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(home, ".local", "share", "watchkeepers", notebookSubdir, agentID+".sqlite"), true
}
