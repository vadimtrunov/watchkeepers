package runtime_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// ExampleNotebookSupervisor demonstrates the canonical
// open → use → close lifecycle for a single agent's notebook handle.
// Real callers (the eventual concrete [runtime.AgentRuntime]) wire this
// into their `Start` / `Terminate` hookpoints; tests and one-off
// utilities can drive the supervisor directly.
func ExampleNotebookSupervisor() {
	// Pin the data dir so the example does not write under
	// $HOME/.local/share/watchkeepers when run via `go test`.
	dir, err := os.MkdirTemp("", "wk-supervisor-example-")
	if err != nil {
		fmt.Println("mktemp:", err)
		return
	}
	defer os.RemoveAll(dir)
	_ = os.Setenv("WATCHKEEPER_DATA", dir)
	defer os.Unsetenv("WATCHKEEPER_DATA")

	sup := runtime.NewNotebookSupervisor()

	const agentID = "11111111-2222-3333-4444-555555555555"

	// Open creates (or returns the existing handle for) the per-agent
	// SQLite file. A second Open(agentID) before Close returns the same
	// pointer.
	db, err := sup.Open(agentID)
	if err != nil {
		fmt.Println("open:", err)
		return
	}
	_ = db // *notebook.DB — call Recall / Remember / Forget here.

	// Lookup is a pure registry read; useful for layers that did not
	// open the agent themselves but want the live handle.
	if _, ok := sup.Lookup(agentID); ok {
		fmt.Println("registered")
	}

	// Close is idempotent — a second Close on the same agent (or a
	// Close on a never-opened agent) returns nil.
	if err := sup.Close(agentID); err != nil {
		fmt.Println("close:", err)
		return
	}

	// Output: registered
}

// testAgentID is a canonical RFC-4122 UUID used by the supervisor tests.
// Re-used across tests so the on-disk filename pattern is stable.
const testAgentID = "11111111-2222-3333-4444-555555555555"

// pinDataDir points $WATCHKEEPER_DATA at a fresh `t.TempDir()` so the
// supervisor's per-agent SQLite files land in a sandboxed tree the test
// framework cleans up. The notebook package's `agentDBPath` reads the
// env var on every Open call, so this is the right seam.
func pinDataDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("WATCHKEEPER_DATA", dir)
	return dir
}

// TestNotebookSupervisor_OpenLookupClose_Lifecycle exercises the
// happy-path lifecycle: a fresh supervisor returns nil/false from Lookup,
// Open registers the agent, Lookup returns the same handle, Close
// unregisters it, and a post-Close Lookup returns nil/false.
func TestNotebookSupervisor_OpenLookupClose_Lifecycle(t *testing.T) {
	pinDataDir(t)
	sup := runtime.NewNotebookSupervisor()

	if got, ok := sup.Lookup(testAgentID); ok || got != nil {
		t.Fatalf("Lookup before Open: got (%v, %v), want (nil, false)", got, ok)
	}

	db, err := sup.Open(testAgentID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if db == nil {
		t.Fatal("Open returned nil *notebook.DB")
	}

	got, ok := sup.Lookup(testAgentID)
	if !ok {
		t.Fatal("Lookup after Open: ok=false, want true")
	}
	if got != db {
		t.Fatalf("Lookup after Open: pointer mismatch (got %p, want %p)", got, db)
	}

	if err := sup.Close(testAgentID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got, ok := sup.Lookup(testAgentID); ok || got != nil {
		t.Fatalf("Lookup after Close: got (%v, %v), want (nil, false)", got, ok)
	}
}

// TestNotebookSupervisor_IdempotentOpen_SameHandle asserts that a second
// Open(a) before any Close returns the SAME *notebook.DB pointer — no
// re-open, no leak.
func TestNotebookSupervisor_IdempotentOpen_SameHandle(t *testing.T) {
	pinDataDir(t)
	sup := runtime.NewNotebookSupervisor()

	first, err := sup.Open(testAgentID)
	if err != nil {
		t.Fatalf("Open #1: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close(testAgentID) })

	second, err := sup.Open(testAgentID)
	if err != nil {
		t.Fatalf("Open #2: %v", err)
	}
	if second != first {
		t.Fatalf("idempotent Open: pointer mismatch (first=%p, second=%p)", first, second)
	}
}

// TestNotebookSupervisor_IdempotentClose_NoError asserts that a second
// Close(a) is a no-op (returns nil) and does not panic.
func TestNotebookSupervisor_IdempotentClose_NoError(t *testing.T) {
	pinDataDir(t)
	sup := runtime.NewNotebookSupervisor()

	if _, err := sup.Open(testAgentID); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := sup.Close(testAgentID); err != nil {
		t.Fatalf("Close #1: %v", err)
	}
	if err := sup.Close(testAgentID); err != nil {
		t.Fatalf("Close #2 (idempotent): %v", err)
	}
}

// TestNotebookSupervisor_CloseUnknown_NoError asserts that Close on an
// agent that was never Opened returns nil rather than an error.
func TestNotebookSupervisor_CloseUnknown_NoError(t *testing.T) {
	pinDataDir(t)
	sup := runtime.NewNotebookSupervisor()

	if err := sup.Close(testAgentID); err != nil {
		t.Fatalf("Close on unknown agent: %v", err)
	}
}

// TestNotebookSupervisor_OpenInvalidAgentID_NoFilesystem asserts that a
// malformed agent id is rejected before any filesystem touch — the
// supervisor's data dir stays empty (no `notebook/` subdir created).
func TestNotebookSupervisor_OpenInvalidAgentID_NoFilesystem(t *testing.T) {
	dir := pinDataDir(t)
	sup := runtime.NewNotebookSupervisor()

	_, err := sup.Open("not-a-uuid")
	if err == nil {
		t.Fatal("Open with malformed id: err=nil, want non-nil")
	}
	if !errors.Is(err, notebook.ErrInvalidAgentID) {
		t.Fatalf("Open with malformed id: err = %v, want errors.Is(_, notebook.ErrInvalidAgentID)", err)
	}

	// Assert the supervisor did NOT touch the filesystem under $WATCHKEEPER_DATA.
	// The notebook/ subdir is created by agentDBPath only AFTER UUID validation,
	// so a rejected Open must leave dir empty.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("invalid-id Open touched filesystem under %q: %v", dir, names)
	}

	// And the supervisor MUST NOT register the agent.
	if got, ok := sup.Lookup("not-a-uuid"); ok || got != nil {
		t.Fatalf("Lookup after failed Open: got (%v, %v), want (nil, false)", got, ok)
	}
}

// TestNotebookSupervisor_OnDiskFileExists asserts that after Open the
// per-agent SQLite file lives at <data>/notebook/<id>.sqlite and is
// readable only by the owner (mode 0o600).
func TestNotebookSupervisor_OnDiskFileExists(t *testing.T) {
	dir := pinDataDir(t)
	sup := runtime.NewNotebookSupervisor()

	db, err := sup.Open(testAgentID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if db == nil {
		t.Fatal("Open returned nil *notebook.DB")
	}
	t.Cleanup(func() { _ = sup.Close(testAgentID) })

	expected := filepath.Join(dir, "notebook", testAgentID+".sqlite")
	info, err := os.Stat(expected)
	if err != nil {
		t.Fatalf("Stat(%q): %v", expected, err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %#o, want 0o600", got)
	}
}

// TestNotebookSupervisor_ConcurrentOpenClose_NoRace launches 100 goroutines
// mixing Open+Close on a small set of agent ids; under `-race` any
// unsynchronised access to the registry would surface here.
func TestNotebookSupervisor_ConcurrentOpenClose_NoRace(t *testing.T) {
	pinDataDir(t)
	sup := runtime.NewNotebookSupervisor()

	// 8 distinct UUIDs — enough to exercise concurrent Open of different
	// keys (no contention) AND concurrent Open+Close of the same key
	// (mutex-protected) at 100 goroutines / 8 keys = ~12 hits per key.
	ids := make([]string, 8)
	for i := range ids {
		// Deterministic, valid RFC-4122 shape.
		ids[i] = fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
	}

	const goroutines = 100
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := ids[i%len(ids)]
			switch i % 3 {
			case 0:
				if _, err := sup.Open(id); err != nil {
					t.Errorf("goroutine %d Open(%s): %v", i, id, err)
				}
			case 1:
				_, _ = sup.Lookup(id)
			case 2:
				if err := sup.Close(id); err != nil {
					t.Errorf("goroutine %d Close(%s): %v", i, id, err)
				}
			}
		}()
	}
	wg.Wait()

	// Drain any handles still open so the test does not leak file descriptors.
	for _, id := range ids {
		if err := sup.Close(id); err != nil {
			t.Errorf("final Close(%s): %v", id, err)
		}
	}
}
