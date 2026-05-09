// notebookarchive_step.go is the M7.2.b saga.Step implementation
// that archives a per-agent [notebook.DB] file (the M2b.1 SQLite +
// sqlite-vec substrate) into an [archivestore.ArchiveStore] during
// the Watchkeeper retire flow. The step:
//
//  1. Reads the [saga.SpawnContext] off the call's `context.Context`
//     and extracts the watchkeeperID (= [saga.SpawnContext.AgentID]).
//  2. Reads the [saga.RetireResult] outbox pointer off the same ctx;
//     this is where the archive URI lands so the M7.2.c MarkRetired
//     step can read it back. Missing outbox is a wiring bug —
//     [ErrMissingRetireResult].
//  3. Dispatches via the configured [NotebookArchiver] seam, which
//     the production wiring backs with a wrapper that calls
//     `notebook.ArchiveOnRetire(ctx, db, watchkeeperID.String(),
//     store, keepClient)`. The substrate streams the live notebook
//     through [notebook.DB.Archive] into the [archivestore.ArchiveStore]
//     and emits the M2b.7 `notebook_archived` audit row via the
//     supplied keepclient.
//  4. On success (uri non-empty, err nil) writes `result.ArchiveURI`
//     so the M7.2.c MarkRetired step can persist the archive
//     reference onto the watchkeeper row.
//
// Audit discipline (M7.1.c.a / M7.1.d / M7.1.e pattern, AC7):
// the step does NOT emit any new keepers_log event itself. The
// substrate's M2b.7 mutation-audit emit (`notebook_archived`) fires
// from inside the production [NotebookArchiver] when it forwards to
// [notebook.ArchiveOnRetire]; the saga core ([saga.Runner]) emits
// `saga_step_started` / `saga_step_completed` around the dispatch.
//
// PII discipline: the URI returned by the archiver is the success
// payload, NOT something the step embeds in failure error strings.
// Failure paths surface only the wrap-prefix + the underlying typed
// error (e.g. [ErrMissingSpawnContext], [ErrMissingAgentID],
// [ErrMissingRetireResult], [ErrEmptyArchiveURI], or the Archiver's
// own typed error). The watchkeeperID is already on the saga audit
// chain via [saga.SpawnContext.AgentID]; the step does not re-leak
// it through error messages.
//
// # Partial-success collapse
//
// The substrate [notebook.ArchiveOnRetire] returns `(uri, err)` and
// MAY pair a non-empty `uri` with a non-nil `err` on multiple
// post-Put paths (the documented LogAppend-failure path AND the
// theoretical audit-marshal path; future substrate edits could add
// more). The [NotebookArchiver] seam contract therefore requires
// the production wrapper to COLLAPSE the tuple uniformly on ANY
// non-nil err: return `(uri, nil)` on FULL success only, return
// `("", err)` on any failure regardless of which substrate sub-path
// produced it. The wrapper does NOT branch on err shape to decide
// whether to forward the URI; it collapses unconditionally.
//
// The saga step is therefore a clean success/fail unit; partial-
// success retry semantics belong to M7.3 compensations + the
// production wrapper's retry-on-LogAppend-failure logic, NOT to the
// step. Iter-1 critic finding: the prior wording named only the
// LogAppend-failure path and risked a wrapper author missing the
// audit-marshal path; the unconditional-collapse contract avoids
// that.
package spawn

import (
	"context"
	"errors"
	"fmt"
	"net/url"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// NotebookArchiveStepName is the stable closed-set identifier for
// the NotebookArchive step. Used by the [saga.Runner] as the
// `current_step` DAO column and as the `step_name` audit payload key.
// Hoisted to a constant so a typo at the call site is a compile error.
//
// Distinct from M7.1.d's `notebook_provision`: this step handles the
// retire-side archive (Archive→Put→audit) whereas
// `notebook_provision` handles the spawn-side bootstrap (Open→Remember).
const NotebookArchiveStepName = "notebook_archive"

// ErrMissingRetireResult is returned by [NotebookArchiveStep.Execute]
// when the per-call `ctx` does NOT carry a [saga.RetireResult]
// pointer (the kickoffer forgot to seed it via [saga.WithRetireResult]).
// A missing outbox is a programmer / wiring bug, not a runtime fault —
// the step refuses to dispatch because there is nowhere to publish the
// resulting archive URI for the M7.2.c MarkRetired step to consume.
// Matchable via [errors.Is] so the M7.3 compensator (when it lands)
// can branch on it.
//
// Sentinel text is bare (no `spawn: notebook_archive step:` prefix)
// so the wrap chain composes cleanly:
// `spawn: notebook_archive step: missing RetireResult on context`.
// Iter-1 strengthening of the M7.1.e log-grep callout.
var ErrMissingRetireResult = errors.New("missing RetireResult on context")

// ErrEmptyArchiveURI is returned by [NotebookArchiveStep.Execute] when
// the configured [NotebookArchiver] reports success (`err == nil`)
// but returns an empty `uri`. The substrate
// [notebook.ArchiveOnRetire]'s contract is that a successful archive
// always yields a non-empty URI — an empty URI on the success path
// is a wiring bug in the production wrapper that must surface
// loudly (the M7.2.c MarkRetired step would otherwise persist an
// empty archive_uri onto the watchkeeper row, severing the retire
// trail). Matchable via [errors.Is].
//
// Sentinel text is bare (no step prefix) so the wrap chain reads
// `spawn: notebook_archive step: archiver returned empty uri on success`.
var ErrEmptyArchiveURI = errors.New("archiver returned empty uri on success")

// ErrInvalidArchiveURI is returned by [NotebookArchiveStep.Execute]
// when the [NotebookArchiver] reports success but the returned `uri`
// fails minimal RFC 3986 shape validation (parses cleanly via
// [net/url.Parse] AND has a non-empty scheme). A wrapper that
// returns `"garbage"`, whitespace, or a bare path on the success
// path is a wiring bug — the M7.2.c MarkRetired step persists this
// value onto the watchkeeper row and the future
// [archivestore.ArchiveStore.Get] call routes by scheme, so a
// shape-broken value would either break retrieval or write garbage
// to the audit trail. Matchable via [errors.Is]. Iter-1 codex
// finding strengthening of the M7.2.b empty-URI fail-closed
// pattern.
var ErrInvalidArchiveURI = errors.New("archiver returned malformed uri on success")

// NotebookArchiver is the seam the NotebookArchive step dispatches
// through. Implementations resolve the watchkeeper's per-agent
// [notebook.DB] file, stream its contents through
// [notebook.DB.Archive] into a configured
// [archivestore.ArchiveStore], emit the M2b.7 `notebook_archived`
// audit row, and return the resulting URI. The production wrapper
// composes [notebook.ArchiveOnRetire(ctx, db,
// watchkeeperID.String(), store, keepClient)]. Test wiring
// satisfies the interface with a hand-rolled fake (no mocking lib —
// M3.6 / M6.3.e / M7.1.d pattern).
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct sagas. The production wrapper holds an immutable
// reference to the notebook + archivestore + audit seams; the test
// fake uses sync primitives to record calls.
//
// # Partial-success collapse contract
//
// The implementation MUST collapse the substrate's `(uri, err)`
// LogAppend-failure tuple into a clean success/fail: return
// `(uri, nil)` on full success only; return `("", err)` on any
// failure (including the post-Archive-pre-LogAppend partial-success
// path). The step layer treats success as "URI is publishable"; any
// retry-on-audit-failure behaviour belongs in the wrapper, NOT in
// the step.
type NotebookArchiver interface {
	// ArchiveNotebook archives the per-agent notebook keyed by
	// `watchkeeperID` and returns the storage URI on success. On any
	// failure (substrate, store, audit), returns `("", err)` with the
	// underlying error chain wrapped so callers can `errors.Is` /
	// `errors.As` against the substrate sentinels (e.g.
	// [notebook.ErrInvalidEntry] for a malformed watchkeeperID, or
	// any [archivestore] sentinel surfaced by the Put).
	ArchiveNotebook(ctx context.Context, watchkeeperID uuid.UUID) (uri string, err error)
}

// NotebookArchiveStepDeps is the construction-time bag wired into
// [NewNotebookArchiveStep]. Held in a struct so a future addition
// (e.g. a per-call retry policy) lands as a new field without
// breaking the constructor signature.
type NotebookArchiveStepDeps struct {
	// Archiver is the per-watchkeeper [notebook.ArchiveOnRetire]
	// dispatcher. Required; a nil Archiver is rejected at
	// construction.
	Archiver NotebookArchiver
}

// NotebookArchiveStep is the [saga.Step] implementation for the
// `notebook_archive` step. Construct via [NewNotebookArchiveStep];
// the zero value is NOT usable.
//
// Concurrency: safe for concurrent use across distinct sagas. Holds
// only an immutable reference to the [NotebookArchiver]; per-call
// state lives on the goroutine stack and on the per-call
// `context.Context` (which carries the [saga.SpawnContext] keying
// the watchkeeper AND the [saga.RetireResult] outbox the step writes
// to).
type NotebookArchiveStep struct {
	archiver NotebookArchiver
}

// Compile-time assertion: [*NotebookArchiveStep] satisfies
// [saga.Step]. Pins the integration shape so a future change to the
// interface surface fails the build here.
var _ saga.Step = (*NotebookArchiveStep)(nil)

// NewNotebookArchiveStep constructs a [NotebookArchiveStep] with the
// supplied [NotebookArchiveStepDeps]. Archiver is required; a nil
// value panics with a clear message — matches the panic discipline
// of [NewCreateAppStep], [NewOAuthInstallStep], [NewBotProfileStep],
// [NewNotebookProvisionStep], and [NewRuntimeLaunchStep].
func NewNotebookArchiveStep(deps NotebookArchiveStepDeps) *NotebookArchiveStep {
	if deps.Archiver == nil {
		panic("spawn: NewNotebookArchiveStep: deps.Archiver must not be nil")
	}
	return &NotebookArchiveStep{
		archiver: deps.Archiver,
	}
}

// Name satisfies [saga.Step.Name]. Returns the stable closed-set
// identifier `notebook_archive`. The runner uses it as the
// `current_step` DAO column and as the `step_name` audit payload key.
func (s *NotebookArchiveStep) Name() string {
	return NotebookArchiveStepName
}

// Execute satisfies [saga.Step.Execute].
//
// Resolution order:
//
//  1. Cancellation short-circuit: if `ctx` is already cancelled, return
//     a wrapped `ctx.Err()`; the Archiver is NOT touched.
//  2. Read the [saga.SpawnContext] off `ctx`. A miss returns a wrapped
//     [ErrMissingSpawnContext]; the Archiver is NOT touched.
//  3. Validate the SpawnContext's AgentID is non-zero (uuid.Nil cannot
//     key a per-agent notebook file). A miss returns a wrapped
//     [ErrMissingAgentID]; the Archiver is NOT touched.
//  4. Read the [saga.RetireResult] outbox pointer off `ctx`. A miss
//     returns a wrapped [ErrMissingRetireResult]; the Archiver is
//     NOT touched. The pointer is GUARANTEED non-nil when present
//     ([saga.WithRetireResult] panics on nil at the seam — iter-1
//     strengthening), so a single `!ok` branch suffices.
//  5. Dispatch through the [NotebookArchiver] seam, forwarding the
//     watchkeeperID. On error, wrap and return; do NOT mutate the
//     outbox (failure ⇒ no observable result side effect).
//  6. On success, validate the returned URI is non-empty
//     ([ErrEmptyArchiveURI]) and shape-correct (parses via
//     [net/url.Parse] with a non-empty scheme — [ErrInvalidArchiveURI]);
//     empty-or-malformed on success is a wiring bug downstream
//     consumers must NOT silently swallow. Iter-1 codex finding:
//     M7.2.c persists this URI directly onto the watchkeeper row,
//     so a `garbage` string from a buggy wrapper would poison the
//     retire trail without the shape check.
//  7. Publish the URI: `result.ArchiveURI = uri`. The M7.2.c
//     MarkRetired step reads it back via
//     [saga.RetireResultFromContext].
//
// Errors are wrapped with `fmt.Errorf("spawn: notebook_archive
// step: %w", err)` so a caller's `errors.Is` against the underlying
// sentinel still matches.
//
// Audit discipline: this method does NOT call
// [keeperslog.Writer.Append] (AC7). The audit chain belongs to the
// saga core; the M2b.7 mutation-audit emit on archive belongs to
// the [notebook.ArchiveOnRetire] substrate the production
// [NotebookArchiver] wraps.
func (s *NotebookArchiveStep) Execute(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("spawn: notebook_archive step: %w", err)
	}

	sc, ok := saga.SpawnContextFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: notebook_archive step: %w", ErrMissingSpawnContext)
	}
	if sc.AgentID == uuid.Nil {
		return fmt.Errorf("spawn: notebook_archive step: %w", ErrMissingAgentID)
	}

	result, ok := saga.RetireResultFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: notebook_archive step: %w", ErrMissingRetireResult)
	}

	uri, err := s.archiver.ArchiveNotebook(ctx, sc.AgentID)
	if err != nil {
		return fmt.Errorf("spawn: notebook_archive step: %w", err)
	}
	if uri == "" {
		return fmt.Errorf("spawn: notebook_archive step: %w", ErrEmptyArchiveURI)
	}
	if parsed, parseErr := url.Parse(uri); parseErr != nil || parsed.Scheme == "" {
		return fmt.Errorf("spawn: notebook_archive step: %w", ErrInvalidArchiveURI)
	}
	result.ArchiveURI = uri
	return nil
}
