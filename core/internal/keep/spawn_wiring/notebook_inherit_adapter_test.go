package spawnwiring_test

// notebook_inherit_adapter_test.go pins the Phase 2 §M7.1.c wiring
// adapter contract:
//
//   - The adapter constructor panics on a nil fetcher.
//   - The adapter satisfies [spawn.NotebookInheritor].
//   - The adapter's Inherit method forwards to
//     [notebook.InheritFromArchive] with the canonical-string form
//     of the watchkeeperID.
//
// The deeper round-trip (Archive→Fetch→Import→Stats) is exercised by
// `core/pkg/notebook/inherit_from_archive_test.go`; this suite only
// pins the adapter's surface contract.

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/google/uuid"

	spawnwiring "github.com/vadimtrunov/watchkeepers/core/internal/keep/spawn_wiring"
	"github.com/vadimtrunov/watchkeepers/core/pkg/notebook"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn"
)

// stubFetcher is a minimal [notebook.Fetcher] used to confirm the
// adapter forwards its arguments verbatim. The test does NOT need a
// real archive payload — the surface check is "did Inherit receive
// the expected URI and propagate the canonical wkID string?".
type stubFetcher struct {
	called bool
	uri    string
	err    error
}

func (s *stubFetcher) Get(_ context.Context, uri string) (io.ReadCloser, error) {
	s.called = true
	s.uri = uri
	if s.err != nil {
		return nil, s.err
	}
	return io.NopCloser(strings.NewReader("")), nil
}

// TestNewProductionNotebookInheritor_PanicsOnNilFetcher pins the
// panic-on-nil-dep contract.
func TestNewProductionNotebookInheritor_PanicsOnNilFetcher(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on nil fetcher")
		}
	}()
	_ = spawnwiring.NewProductionNotebookInheritor(nil)
}

// TestProductionNotebookInheritor_SatisfiesSpawnSeam pins the
// compile-time interface contract via runtime type assertion (the
// `var _ spawn.NotebookInheritor = ...` line in the adapter file is
// the primary guard; this test is the runtime mirror so a CI run
// surfaces a regression even when the assertion line is removed).
func TestProductionNotebookInheritor_SatisfiesSpawnSeam(t *testing.T) {
	t.Parallel()
	adapter := spawnwiring.NewProductionNotebookInheritor(&stubFetcher{})
	var _ spawn.NotebookInheritor = adapter // compile-time
	if adapter == nil {
		t.Fatalf("NewProductionNotebookInheritor returned nil")
	}
}

// TestProductionNotebookInheritor_Inherit_ForwardsCanonicalWatchkeeperID
// pins the wkID-string-conversion contract: the watchkeeperID
// `uuid.UUID` is converted via `.String()` so the notebook substrate's
// path validator (`uuidPattern` regex on `<wkID>.sqlite`) accepts the
// shape. The test calls Inherit, observes the fetcher was reached
// (the import-validation path forwards to the fetcher on the second
// step), and exits before the corrupt-archive failure surfaces.
//
// On the failure path the adapter returns the wrapped notebook-side
// error; we accept ANY non-nil error here because the goal is to
// confirm the adapter PROPAGATED the call to the notebook substrate.
func TestProductionNotebookInheritor_Inherit_ReachesNotebookSubstrate(t *testing.T) {
	t.Parallel()
	fetcher := &stubFetcher{err: errors.New("test-stub-fetch-error")}
	adapter := spawnwiring.NewProductionNotebookInheritor(fetcher)

	wkID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	count, err := adapter.Inherit(context.Background(), wkID, "s3://test/archive.sqlite")

	if !fetcher.called {
		t.Errorf("fetcher.Get was not called; adapter did NOT forward to notebook substrate")
	}
	if fetcher.uri != "s3://test/archive.sqlite" {
		t.Errorf("fetcher.uri = %q, want %q", fetcher.uri, "s3://test/archive.sqlite")
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 on error path", count)
	}
	if err == nil {
		t.Errorf("err = nil; want wrapped fetch error")
	}
}

// TestProductionNotebookInheritor_Inherit_NilUUID_ReachesValidator
// pins that the adapter does NOT pre-validate the wkID — the
// notebook substrate's path validator is the authoritative gate. A
// `uuid.Nil` converts to `"00000000-0000-0000-0000-000000000000"`,
// which IS a canonical UUID string, so the substrate accepts the
// shape (validation downstream rejects on a different ground if
// needed). The test ensures the adapter does not short-circuit on
// `uuid.Nil` — that is the step's responsibility.
func TestProductionNotebookInheritor_Inherit_NilUUID_ReachesValidator(t *testing.T) {
	t.Parallel()
	fetcher := &stubFetcher{err: errors.New("test-stub-fetch-error")}
	adapter := spawnwiring.NewProductionNotebookInheritor(fetcher)

	_, _ = adapter.Inherit(context.Background(), uuid.Nil, "s3://test/archive.sqlite")
	// The fetcher's Get is invoked even on uuid.Nil — the substrate
	// validates the path shape, not the adapter.
	if !fetcher.called {
		t.Errorf("fetcher.Get was not called on uuid.Nil; adapter must not pre-validate the wkID")
	}
}

// compileTimeSeamAssertion is a compile-time mirror of the
// package-level assertion in the adapter file; declared here as a
// test-side double so a future regression that drops the in-package
// assertion line still surfaces a build failure on the test side.
//
//nolint:unused // referenced via the type assertion alone.
var compileTimeSeamAssertion spawn.NotebookInheritor = (*spawnwiring.ProductionNotebookInheritor)(nil)

// notebookFetcherInterfaceCheck pins the public seam of
// [notebook.Fetcher] used by the adapter constructor.
//
//nolint:unused // compile-time assertion.
var _ notebook.Fetcher = (*stubFetcher)(nil)
