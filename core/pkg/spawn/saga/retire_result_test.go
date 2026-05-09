package saga_test

import (
	"context"
	"testing"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// TestRetireResult_RoundTrip pins the basic store-then-retrieve
// contract: a value placed on ctx via [WithRetireResult] is returned
// verbatim (same pointer identity) by [RetireResultFromContext].
// Pointer-identity is load-bearing — write-then-read across distinct
// steps relies on the SAME memory backing every retrieval (the M7.2.b
// NotebookArchive step writes ArchiveURI; M7.2.c MarkRetired reads it).
func TestRetireResult_RoundTrip(t *testing.T) {
	t.Parallel()

	r := &saga.RetireResult{}
	ctx := saga.WithRetireResult(context.Background(), r)

	got, ok := saga.RetireResultFromContext(ctx)
	if !ok {
		t.Fatal("RetireResultFromContext: ok = false; want true")
	}
	if got != r {
		t.Errorf("RetireResultFromContext returned a different pointer (%p) than was stored (%p) — retrieve-by-pointer-identity is load-bearing for cross-step writes", got, r)
	}
}

// TestRetireResult_MutationVisibleAcrossReads pins the inter-step
// write-then-read invariant: a mutation through the pointer returned
// by [RetireResultFromContext] is visible on a SUBSEQUENT
// [RetireResultFromContext] call against the SAME ctx. This is what
// makes the M7.2.b → M7.2.c URI handoff work.
func TestRetireResult_MutationVisibleAcrossReads(t *testing.T) {
	t.Parallel()

	r := &saga.RetireResult{}
	ctx := saga.WithRetireResult(context.Background(), r)

	first, _ := saga.RetireResultFromContext(ctx)
	first.ArchiveURI = "file:///snapshots/wk/2026-05-09T12-34-56Z.tar.gz"

	second, _ := saga.RetireResultFromContext(ctx)
	if second.ArchiveURI != first.ArchiveURI {
		t.Errorf("second read ArchiveURI = %q, want %q (mutation through the first pointer must be visible on subsequent reads)",
			second.ArchiveURI, first.ArchiveURI)
	}
}

// TestRetireResult_MissingKey pins the absent-key behaviour: a
// `context.Background()` carries no [RetireResult]; the boolean
// reports false and the pointer is nil. Step authors rely on this to
// reject malformed kickoffs (a kickoffer that forgot to seed) with a
// wrapped sentinel rather than dereferencing nil.
func TestRetireResult_MissingKey(t *testing.T) {
	t.Parallel()

	got, ok := saga.RetireResultFromContext(context.Background())
	if ok {
		t.Errorf("RetireResultFromContext on bare ctx: ok = true; want false")
	}
	if got != nil {
		t.Errorf("RetireResultFromContext on bare ctx: got = %v; want nil", got)
	}
}

// TestWithRetireResult_PanicsOnNilPointer pins the iter-1
// strengthened seam contract: passing nil to [WithRetireResult]
// panics at the seam rather than producing a `(nil, true)` ctx that
// would surface as a downstream nil-pointer dereference. The prior
// "consumer also rejects nil" contract was a weaker API that forced
// every retire step to remember the second guard branch; with the
// constructor-side panic, [RetireResultFromContext] is now
// guaranteed `(non-nil, true)` whenever `ok == true`.
func TestWithRetireResult_PanicsOnNilPointer(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("WithRetireResult(nil) did not panic")
		}
	}()
	_ = saga.WithRetireResult(context.Background(), nil)
}

// TestRetireResult_ReplacesPriorValue pins last-write-wins semantics:
// re-stamping ctx with a different [RetireResult] pointer replaces
// the prior one. Future kickoffer iterations that re-seed mid-saga
// would surface a behaviour change here.
func TestRetireResult_ReplacesPriorValue(t *testing.T) {
	t.Parallel()

	first := &saga.RetireResult{ArchiveURI: "file:///first"}
	second := &saga.RetireResult{ArchiveURI: "file:///second"}

	ctx := saga.WithRetireResult(context.Background(), first)
	ctx = saga.WithRetireResult(ctx, second)

	got, _ := saga.RetireResultFromContext(ctx)
	if got != second {
		// Iter-1 critic finding (Cr6): nil-check before deref so a
		// regression that returned nil here surfaces as an
		// assert-fail, not a test-process panic.
		var prev string
		if got != nil {
			prev = got.ArchiveURI
		}
		t.Errorf("RetireResultFromContext after re-stamp: got = %p (URI=%q); want %p (URI=%q)",
			got, prev, second, second.ArchiveURI)
	}
}

// TestRetireResultKey_ExportedForDiagnostics pins the doc-block claim
// that [RetireResultKey] is exported as `any` so a diagnostics
// extractor can reach the value without importing the unexported
// type. A regression that down-grades the export (e.g. typed
// `var RetireResultKey retireResultKeyType`) would surface as a
// compile error here.
func TestRetireResultKey_ExportedForDiagnostics(t *testing.T) {
	t.Parallel()

	k := saga.RetireResultKey
	if k == nil {
		t.Errorf("saga.RetireResultKey is nil; want non-nil context-key sentinel")
	}
}
