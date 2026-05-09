// retire_result.go defines the [RetireResult] mutable side-channel
// the retire-saga's caller threads through `context.Context` so a
// retire-flow [Step] (M7.2.b NotebookArchive) can publish per-saga
// outputs (today: `archive_uri`) for later steps (M7.2.c MarkRetired)
// to read.
//
// The value is the retire-saga analogue of [SpawnContext]: where
// [SpawnContext] is a READ-ONLY input bag seeded once at kickoff time
// and consumed by every step, [RetireResult] is a WRITE-then-READ
// outbox the caller seeds as a fresh `&RetireResult{}` and successive
// steps mutate. The two value types coexist on the same `ctx` — a
// retire saga's Kickoffer seeds BOTH, an M7.1 spawn saga's Kickoffer
// seeds ONLY [SpawnContext] (no spawn step needs an outbox today).
//
// # Pointer storage discipline
//
// `context.WithValue` stores values by interface-copy semantics: a
// non-pointer struct stored on ctx is snapshot-copied on every
// `.Value(...)` retrieval, so any mutation by Step A would be invisible
// to Step B. To make inter-step writes observable, [WithRetireResult]
// stores a `*RetireResult`; [RetireResultFromContext] returns the same
// pointer, and step authors mutate fields through it. Within one saga
// run the [Runner] dispatches steps sequentially, so concurrent writes
// to a single [RetireResult] are not possible. Across distinct sagas,
// each goroutine's ctx carries its OWN pointer — no shared state.
//
// # Why a distinct value type vs extending SpawnContext
//
// Roadmap §M7.2.b names the seam "SpawnContext-equivalent". A separate
// type keeps the spawn flow's [SpawnContext] truly read-only (no
// future maintainer edits a SpawnContext field assuming the change
// will surface to a downstream step — it would not, value semantics)
// and signals at the type level that retire-flow steps participate in
// an outbox protocol the spawn family does not.
package saga

import "context"

// retireResultKeyType is a private type used as the key for storing
// the [RetireResult] pointer in a `context.Context`. The unexported
// type ensures no other package can collide with the key (string-keyed
// `context.WithValue` calls cannot reach this key by accident).
type retireResultKeyType struct{}

// RetireResultKey is the context key under which [WithRetireResult]
// stores the [RetireResult] pointer. Exported as an `any` so callers
// reading the key for diagnostics (e.g. a tracing extractor) can
// reference it without importing the unexported type. This is the
// idiomatic context.Key pattern.
//
//nolint:gochecknoglobals // package-level singleton; idiomatic context.Key pattern.
var RetireResultKey any = retireResultKeyType{}

// RetireResult is the retire-saga's per-call outbox. Steps that
// produce values consumed by later steps mutate fields on the
// pointer returned by [RetireResultFromContext].
//
// The zero value is a valid empty outbox: every field is the zero
// value of its type, and a Step that reads from it before any prior
// step has written sees those zeros. A consuming step that requires
// a non-zero field validates it in its own `Execute` body and returns
// a wrapped error before doing any externally-visible work.
//
// All fields are typed (no `interface{}`) per the package's
// fail-fast-on-shape discipline.
type RetireResult struct {
	// ArchiveURI is the storage URI returned by the M7.2.b
	// [notebook.ArchiveOnRetire]-bridge wrapper after a successful
	// archive. Empty until the M7.2.b NotebookArchive step writes it;
	// the M7.2.c MarkRetired step reads it back so the
	// `keepclient.Update`-equivalent call carries the archive
	// pointer onto the watchkeeper row.
	ArchiveURI string
}

// WithRetireResult returns a new context carrying `r` under
// [RetireResultKey]. The pointer MUST be non-nil; a nil `r` panics
// at the seam to fail-fast on the wiring bug rather than letting it
// surface as a nil-pointer dereference inside a downstream step's
// `result.ArchiveURI = uri` write. Iter-1 strengthening: the prior
// "consumer also checks for nil" contract was a weaker API that
// forced every retire step to remember the second guard branch; the
// retire kickoffer is the SOLE caller of this constructor in
// production today, and it always passes `&RetireResult{}`.
func WithRetireResult(parent context.Context, r *RetireResult) context.Context {
	if r == nil {
		panic("saga: WithRetireResult: r must not be nil")
	}
	return context.WithValue(parent, retireResultKeyType{}, r)
}

// RetireResultFromContext returns the [RetireResult] pointer stored
// on `ctx` by a prior [WithRetireResult] call, plus a boolean
// reporting whether such a value was present. The returned pointer
// is GUARANTEED non-nil when `ok` is true ([WithRetireResult]
// rejects nil at the seam), so step authors only need the `!ok`
// branch.
//
// Callers that require the pointer to be present (e.g. the M7.2.b
// NotebookArchive step) wrap a missing-result error rather than
// proceeding without a destination for their writes.
func RetireResultFromContext(ctx context.Context) (*RetireResult, bool) {
	r, ok := ctx.Value(retireResultKeyType{}).(*RetireResult)
	return r, ok
}
