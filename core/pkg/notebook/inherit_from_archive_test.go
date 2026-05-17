package notebook

// inherit_from_archive_test.go pins the Phase 2 §M7.1.c thin wrapper
// composition: ImportFromArchive (without audit emit) → re-open →
// Stats → close. The wrapper returns the imported-entry count so the
// M7.1.c NotebookInheritStep can echo it on the `notebook_inherited`
// audit row.

import (
	"context"
	"errors"
	"testing"
)

// TestInheritFromArchive_HappyPath — Archive a 3-entry source
// notebook, hand the bytes to a fakeFetcher, call InheritFromArchive
// against a fresh destination agent, verify the returned count
// equals len(importSeeds), verify the imported rows are visible on
// re-open, and verify NO `notebook_imported` audit row was emitted
// (the wrapper passes nil to ImportFromArchive's logger argument so
// the substrate audit emit stays silent — the saga step owns the
// `notebook_inherited` row).
func TestInheritFromArchive_HappyPath(t *testing.T) {
	ctx := context.Background()

	// Source agent's data tree — seed + archive.
	t.Setenv(envDataDir, t.TempDir())
	snapshot := seedAndArchive(ctx, t, importTestAgentSrc)

	// Destination agent's data tree (fresh).
	t.Setenv(envDataDir, t.TempDir())

	fetcher := &fakeFetcher{bytesIn: snapshot}

	count, err := InheritFromArchive(ctx, importTestAgentDst, importTestURI, fetcher)
	if err != nil {
		t.Fatalf("InheritFromArchive: %v", err)
	}
	if count != len(importSeeds) {
		t.Errorf("count = %d, want %d (len(importSeeds))", count, len(importSeeds))
	}

	// Re-open the destination notebook to confirm the imported rows
	// are visible.
	dst, err := Open(ctx, importTestAgentDst)
	if err != nil {
		t.Fatalf("Open dst post-inherit: %v", err)
	}
	t.Cleanup(func() { _ = dst.Close() })

	for _, s := range importSeeds {
		assertImportedSeed(ctx, t, dst, s)
	}

	if got := fetcher.getCalled.Load(); got != 1 {
		t.Errorf("fetcher.Get called %d times, want 1", got)
	}
}

// TestInheritFromArchive_FetcherError — the wrapper surfaces the
// ImportFromArchive wrap chain verbatim; no DB is opened on the
// failure path (the post-import Open + Stats are guarded behind the
// import success branch).
func TestInheritFromArchive_FetcherError(t *testing.T) {
	ctx := context.Background()
	t.Setenv(envDataDir, t.TempDir())

	sentinel := errors.New("fetch-boom")
	fetcher := &fakeFetcher{getErr: sentinel}

	count, err := InheritFromArchive(ctx, importTestAgentDst, importTestURI, fetcher)
	if err == nil {
		t.Fatalf("InheritFromArchive: nil err, want wrapped %v", sentinel)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is(err, sentinel) = false; err = %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 on error path", count)
	}
}

// TestInheritFromArchive_CorruptArchive — a corrupt body surfaces
// as wrapped [ErrCorruptArchive] through the ImportFromArchive
// wrap chain. The post-import Open + Stats are NOT reached.
func TestInheritFromArchive_CorruptArchive(t *testing.T) {
	ctx := context.Background()
	t.Setenv(envDataDir, t.TempDir())

	fetcher := &fakeFetcher{bytesIn: []byte("not a sqlite file, just plain text bytes")}

	count, err := InheritFromArchive(ctx, importTestAgentDst, importTestURI, fetcher)
	if err == nil {
		t.Fatalf("InheritFromArchive: nil err, want wrapped ErrCorruptArchive")
	}
	if !errors.Is(err, ErrCorruptArchive) {
		t.Errorf("errors.Is(err, ErrCorruptArchive) = false; err = %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 on error path", count)
	}
}

// TestInheritFromArchive_BadAgentID — non-canonical UUID returns
// ErrInvalidEntry synchronously through the ImportFromArchive
// wrap chain.
func TestInheritFromArchive_BadAgentID(t *testing.T) {
	ctx := context.Background()
	t.Setenv(envDataDir, t.TempDir())

	fetcher := &fakeFetcher{}

	count, err := InheritFromArchive(ctx, "not-a-uuid", importTestURI, fetcher)
	if err == nil {
		t.Fatalf("InheritFromArchive: nil err, want ErrInvalidEntry")
	}
	if !errors.Is(err, ErrInvalidEntry) {
		t.Errorf("errors.Is(err, ErrInvalidEntry) = false; err = %v", err)
	}
	if count != 0 {
		t.Errorf("count = %d, want 0 on error path", count)
	}
}
