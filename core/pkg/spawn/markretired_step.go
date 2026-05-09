// markretired_step.go is the M7.2.c saga.Step implementation that
// finalises the retire saga: it transitions the watchkeeper row from
// `active` to `retired` and persists the archive URI emitted by the
// upstream M7.2.b NotebookArchive step. The step is the LAST concrete
// step of the M7.2 retire family — its successful return drives the
// saga's `saga_completed` audit emit (see [saga.Runner]).
//
// The step:
//
//  1. Reads the [saga.SpawnContext] off the call's `context.Context`
//     and extracts the watchkeeperID (= [saga.SpawnContext.AgentID]).
//  2. Reads the [saga.RetireResult] outbox pointer off the same ctx;
//     this is where the M7.2.b NotebookArchive step published the
//     archive URI on success. A missing outbox is a wiring bug —
//     [ErrMissingRetireResult] (the same sentinel M7.2.b uses).
//  3. Validates the published [saga.RetireResult.ArchiveURI] is
//     non-empty. The M7.2.b step is the sole producer and already
//     fail-closes on empty / malformed URIs ([ErrEmptyArchiveURI],
//     [ErrInvalidArchiveURI]); a fresh empty value here means a step
//     ordering / wiring regression upstream and the M7.2.c sentinel
//     [ErrMissingArchiveURI] surfaces it explicitly so the audit
//     chain pins which step's contract was broken.
//  4. Dispatches via the configured [WatchkeeperRetirer] seam, which
//     the production wiring backs with a wrapper that calls
//     `keepclient.Client.UpdateWatchkeeperRetired(ctx,
//     watchkeeperID.String(), archiveURI)`. The Keep server's PATCH
//     handler enforces the `active→retired` transition rule and
//     stamps `retired_at = now()` + `archive_uri = $2` atomically
//     inside the same scoped tx (see migration
//     `022_watchkeepers_archive_uri.sql`).
//
// Audit discipline (M7.1.c.a / M7.1.d / M7.1.e / M7.2.b pattern, AC7):
// the step does NOT emit any new keepers_log event itself. The saga
// core ([saga.Runner]) emits `saga_step_started` /
// `saga_step_completed` around the dispatch; the keep server's
// `PATCH /v1/watchkeepers/{id}/status` handler is the
// state-of-record for the row transition (it never emits to
// keepers_log directly — the M6.2.c synchronous tool's
// `watchmaster_retire_watchkeeper_*` chain stays distinct from the
// saga family's audit chain by design; see M7.2.a lesson #4).
//
// PII discipline: the archive URI is the load-bearing payload here,
// NOT something the step embeds in failure error strings. Failure
// paths surface only the wrap-prefix + the underlying typed error
// (e.g. [ErrMissingSpawnContext], [ErrMissingAgentID],
// [ErrMissingRetireResult], [ErrMissingArchiveURI], or the Retirer's
// own typed error including the keepclient sentinels
// [keepclient.ErrInvalidStatusTransition] /
// [keepclient.ErrNotFound]). The watchkeeperID is already on the
// saga audit chain via [saga.SpawnContext.AgentID]; the step does
// not re-leak it through error messages. The archive URI is also
// already on the M2b.7 `notebook_archived` audit row emitted by the
// substrate the M7.2.b step wrapped — embedding it again in retire-
// step error strings would be a redundant leak vector.
//
// # M6.2.c compatibility note
//
// The pre-existing M6.2.c synchronous `RetireWatchkeeper` Watchmaster
// tool (see [RetireWatchkeeper] in retire_watchkeeper.go) keeps its
// own `watchmaster_retire_watchkeeper_*` audit chain and continues
// to call [keepclient.Client.UpdateWatchkeeperStatus] with
// `status="retired"` and NO archive_uri. That path is wire-compatible
// with the M7.2.c migration: the column is `text NULL` and the
// server's parser leaves it NULL when the body field is absent.
// Future wiring (a follow-up PR after M7.2.c) routes the M6.2.c tool
// through [RetireKickoffer] so the saga family becomes the sole
// retire surface; today the two surfaces coexist by design.
package spawn

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// MarkRetiredStepName is the stable closed-set identifier for the
// MarkRetired step. Used by the [saga.Runner] as the `current_step`
// DAO column and as the `step_name` audit payload key. Hoisted to a
// constant so a typo at the call site is a compile error.
//
// Distinct from M7.2.b's `notebook_archive` (the upstream archive
// substrate dispatch) and M6.2.c's tool name `retire_watchkeeper`
// (the synchronous Watchmaster tool's `tool_name` payload value);
// the saga step's identifier names the WHAT of its work
// (`mark_retired`) rather than the WHY of the surface that triggered
// it.
const MarkRetiredStepName = "mark_retired"

// ErrMissingArchiveURI is returned by [MarkRetiredStep.Execute] when
// the [saga.RetireResult] outbox carries an empty `ArchiveURI` field.
// The upstream M7.2.b NotebookArchive step is the sole producer; an
// empty value here means either the step list mis-ordered the family
// (NotebookArchive after MarkRetired) or NotebookArchive's outbox
// publish was skipped (its own fail-closed gates already reject the
// `("", nil)` and malformed-URI corner cases). Either way it is a
// wiring bug, not a runtime fault — surfacing it here pins the
// invariant at the consumer boundary so the audit chain shows
// MarkRetired refused to mark a row retired without a URI to record.
//
// Sentinel text is bare (no `spawn: mark_retired step:` prefix)
// matching the M7.2.b iter-1 strengthening of the wrap-chain
// composition discipline; the wrap chain reads
// `spawn: mark_retired step: missing ArchiveURI on RetireResult`.
//
// Matchable via [errors.Is] so the M7.3 compensator (when it lands)
// can branch on it. Distinct from [ErrMissingRetireResult] (which
// covers the missing-OUTBOX case); the M7.2.c step's missing-URI
// case is structurally different — the outbox is present, the field
// is empty.
var ErrMissingArchiveURI = errors.New("missing ArchiveURI on RetireResult")

// WatchkeeperRetirer is the seam the MarkRetired step dispatches
// through. Implementations call the keep server's
// `PATCH /v1/watchkeepers/{id}/status` endpoint with
// `status:"retired"` + the supplied archiveURI; the production
// wrapper composes
// [keepclient.Client.UpdateWatchkeeperRetired](ctx,
// watchkeeperID.String(), archiveURI). Test wiring satisfies the
// interface with a hand-rolled fake (no mocking lib — M3.6 / M6.3.e
// / M7.1.d / M7.2.b pattern).
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct sagas. The production wrapper holds an immutable
// reference to the [keepclient.Client]; the test fake uses sync
// primitives to record calls.
//
// Idempotency contract (Phase-1 admin-grant): until M7.3 ships
// compensations, [MarkRetired] is not retry-aware on the saga side;
// the keep server enforces the `active→retired` transition rule, so
// a re-run of the same `(watchkeeperID, archiveURI)` pair on an
// already-retired row surfaces as
// [keepclient.ErrInvalidStatusTransition] from the seam, the saga
// records `saga_failed`, and the operator (or future M7.3
// compensator) decides whether to recover. Implementations MUST NOT
// silently swallow that error — the row's archive_uri column is
// authoritative and re-mark attempts are a wiring signal worth
// surfacing.
type WatchkeeperRetirer interface {
	// MarkRetired transitions the watchkeeper row identified by
	// `watchkeeperID` to `retired` and stamps `archiveURI` onto the
	// `archive_uri` column atomically inside the keep server's scoped
	// tx. On any failure (transport, server-side validation, transition
	// rule), returns the wrapped error chain so callers can
	// `errors.Is` / `errors.As` against the substrate sentinels (e.g.
	// [keepclient.ErrInvalidStatusTransition] for a non-active row,
	// [keepclient.ErrNotFound] for an unknown id).
	MarkRetired(ctx context.Context, watchkeeperID uuid.UUID, archiveURI string) error
}

// MarkRetiredStepDeps is the construction-time bag wired into
// [NewMarkRetiredStep]. Held in a struct so a future addition (e.g.
// a per-call retry policy, a clock for timeout shaping) lands as a
// new field without breaking the constructor signature.
type MarkRetiredStepDeps struct {
	// Retirer is the per-watchkeeper status-transition dispatcher.
	// Required; a nil Retirer is rejected at construction.
	Retirer WatchkeeperRetirer
}

// MarkRetiredStep is the [saga.Step] implementation for the
// `mark_retired` step. Construct via [NewMarkRetiredStep]; the zero
// value is NOT usable.
//
// Concurrency: safe for concurrent use across distinct sagas. Holds
// only an immutable reference to the [WatchkeeperRetirer]; per-call
// state lives on the goroutine stack and on the per-call
// `context.Context` (which carries the [saga.SpawnContext] keying
// the watchkeeper AND the [saga.RetireResult] outbox the step reads
// from).
type MarkRetiredStep struct {
	retirer WatchkeeperRetirer
}

// Compile-time assertion: [*MarkRetiredStep] satisfies [saga.Step].
// Pins the integration shape so a future change to the interface
// surface fails the build here.
var _ saga.Step = (*MarkRetiredStep)(nil)

// NewMarkRetiredStep constructs a [MarkRetiredStep] with the supplied
// [MarkRetiredStepDeps]. Retirer is required; a nil value panics with
// a clear message — matches the panic discipline of
// [NewCreateAppStep], [NewOAuthInstallStep], [NewBotProfileStep],
// [NewNotebookProvisionStep], [NewRuntimeLaunchStep], and
// [NewNotebookArchiveStep].
func NewMarkRetiredStep(deps MarkRetiredStepDeps) *MarkRetiredStep {
	if deps.Retirer == nil {
		panic("spawn: NewMarkRetiredStep: deps.Retirer must not be nil")
	}
	return &MarkRetiredStep{
		retirer: deps.Retirer,
	}
}

// Name satisfies [saga.Step.Name]. Returns the stable closed-set
// identifier `mark_retired`. The runner uses it as the `current_step`
// DAO column and as the `step_name` audit payload key.
func (s *MarkRetiredStep) Name() string {
	return MarkRetiredStepName
}

// Execute satisfies [saga.Step.Execute].
//
// Resolution order:
//
//  1. Cancellation short-circuit: if `ctx` is already cancelled, return
//     a wrapped `ctx.Err()`; the Retirer is NOT touched.
//  2. Read the [saga.SpawnContext] off `ctx`. A miss returns a wrapped
//     [ErrMissingSpawnContext]; the Retirer is NOT touched.
//  3. Validate the SpawnContext's AgentID is non-zero (uuid.Nil cannot
//     key the watchkeeper-being-retired). A miss returns a wrapped
//     [ErrMissingAgentID]; the Retirer is NOT touched.
//  4. Read the [saga.RetireResult] outbox pointer off `ctx`. A miss
//     returns a wrapped [ErrMissingRetireResult] (the M7.2.b sentinel,
//     reused per the saga-family one-sentinel-per-error-class rule);
//     the Retirer is NOT touched. The pointer is GUARANTEED non-nil
//     when present ([saga.WithRetireResult] panics on nil at the seam),
//     so a single `!ok` branch suffices.
//  5. Validate the published archive URI is non-empty. M7.2.b's gates
//     already reject `("", nil)` and malformed URIs at the producer
//     boundary, so a fresh empty value here is a step-ordering /
//     wiring regression rather than a runtime fault — surface it via
//     [ErrMissingArchiveURI] without touching the Retirer.
//  6. Dispatch through the [WatchkeeperRetirer] seam, forwarding the
//     watchkeeperID and the archive URI. On error, wrap and return.
//
// Errors are wrapped with `fmt.Errorf("spawn: mark_retired step: %w",
// err)` so a caller's `errors.Is` against the underlying sentinel
// still matches.
//
// Audit discipline: this method does NOT call
// [keeperslog.Writer.Append] (AC7). The saga core
// ([saga.Runner]) emits `saga_step_started` /
// `saga_step_completed` around the dispatch; the keep server's
// `PATCH /v1/watchkeepers/{id}/status` handler is the
// state-of-record for the row transition (it never emits to
// keepers_log directly).
func (s *MarkRetiredStep) Execute(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("spawn: mark_retired step: %w", err)
	}

	sc, ok := saga.SpawnContextFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: mark_retired step: %w", ErrMissingSpawnContext)
	}
	if sc.AgentID == uuid.Nil {
		return fmt.Errorf("spawn: mark_retired step: %w", ErrMissingAgentID)
	}

	result, ok := saga.RetireResultFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: mark_retired step: %w", ErrMissingRetireResult)
	}
	if result.ArchiveURI == "" {
		return fmt.Errorf("spawn: mark_retired step: %w", ErrMissingArchiveURI)
	}

	if err := s.retirer.MarkRetired(ctx, sc.AgentID, result.ArchiveURI); err != nil {
		return fmt.Errorf("spawn: mark_retired step: %w", err)
	}
	return nil
}
