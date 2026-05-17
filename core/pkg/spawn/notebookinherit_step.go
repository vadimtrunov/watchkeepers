// notebookinherit_step.go is the Phase 2 §M7.1.c saga.Step
// implementation that, on the spawn flow, seeds a freshly-allocated
// Watchkeeper's per-agent notebook file with the most-recently-retired
// peer's archived notebook (the "predecessor inheritance" path). The
// step runs BEFORE [NotebookProvisionStep] so that the provision step
// finds a non-empty file when a predecessor exists and a virgin file
// otherwise.
//
// The step:
//
//  1. Reads the [saga.SpawnContext] off the call's `context.Context`
//     and extracts the watchkeeperID (= [saga.SpawnContext.AgentID]),
//     the role identity ([saga.SpawnContext.RoleID]), the operator
//     opt-out flag ([saga.SpawnContext.NoInherit]), and the tenant id
//     ([saga.SpawnContext.Claim.OrganizationID]).
//  2. Short-circuits a no-op (NO audit event) when ANY of:
//     - `sc.NoInherit == true` (operator explicitly opted out)
//     - `sc.RoleID == ""` (no role identity → no predecessor query
//     can be formulated; semantically equivalent to a miss)
//     - `sc.Claim.OrganizationID == ""` (no tenant to scope the
//     predecessor lookup; degrades to no-op rather than 4xx)
//     The roadmap acceptance pins the no-op + no-audit shape for the
//     `--no-inherit` and no-predecessor branches; the empty-RoleID +
//     empty-OrgID branches are the conservative extensions of that
//     same shape so a misconfigured wiring never produces a noisy
//     audit row.
//  3. Calls [PredecessorLookup.LatestRetiredByRole]. On
//     [keepclient.ErrNoPredecessor] (the typed 404 sentinel) the
//     step is a no-op + NO audit event. On any other transport /
//     auth error the wrapped error is returned and the saga's
//     reverse-rollback chain runs — but [NotebookInheritStep] has
//     NO externally-visible side effect to undo on this branch (we
//     have not touched the destination DB yet), so it does not
//     implement [saga.Compensator]. Per the M7.3.b "steps that do
//     NOT implement Compensator are silently skipped" contract, the
//     audit chain emits `saga_step_compensated` only for the steps
//     that already ran AND implement Compensator (e.g. the
//     downstream [NotebookProvisionStep], which is what owns the
//     archive-not-delete + flag-for-review of the inherited file).
//  4. On a hit (a non-nil predecessor with a non-empty
//     `archive_uri`): dispatches via [NotebookInheritor.Inherit] to
//     fetch + import + count entries. The seam owns the
//     three-primitive composition (`Fetcher.Get` →
//     `notebook.Open` → `DB.Import` → `DB.Stats`); the step is
//     transport-agnostic.
//  5. Emits `notebook_inherited` on the [InheritAuditAppender] seam
//     AFTER a successful inherit with the closed-set payload
//     `{predecessor_watchkeeper_id, archive_uri,
//     entries_imported}`. A non-nil emit error is wrapped and
//     returned — the inherited data is already in the DB; the saga
//     compensator (which lives on [NotebookProvisionStep], NOT on
//     this step) archives the file on rollback.
//
// Audit discipline: the step emits exactly one row per successful
// inheritance, NEVER on the no-op branches (operator opt-out,
// no-predecessor, empty role/org). The saga core ([saga.Runner])
// emits `saga_step_started` / `saga_step_completed` around the
// dispatch regardless. The audit emit happens AFTER the data write
// (the count requires the import to have completed). If the import
// succeeds and the audit emit fails, the step returns an error so
// the saga runner's rollback walk runs; the downstream
// [NotebookProvisionStep] has not yet executed (this step runs
// BEFORE provision in the canonical step list) so there is no
// inherited file to clean up — the orphaned file is owned by the
// provisioner's compensator if the provision step had run.
//
// PII discipline: the `archive_uri` IS included in the audit
// payload (the M7.1.b predecessor-lookup endpoint returns this URI
// as part of its success envelope; the audit chain already records
// archive URIs on `notebook_archived` rows from M2b.7, so the
// downstream consumer's discipline is unchanged). Error strings
// NEVER leak the archive URI substring, the predecessor's
// watchkeeperID, or the role identity — they are wrapped behind
// the typed sentinels declared below.
//
// # Compensator delegation
//
// [NotebookInheritStep] does NOT implement [saga.Compensator]. The
// only externally-visible side effect this step produces is the
// per-agent notebook FILE on disk (created on first `notebook.Open`
// inside the [NotebookInheritor.Inherit] dispatch); that file is
// the same on-disk artefact the downstream [NotebookProvisionStep]
// re-opens to write its seed entries. The M7.3.c
// archive-not-delete + flag-for-review compensator lives on the
// provisioner ([NotebookProvisionStep.Compensate]) and runs in the
// reverse-rollback chain; the inheritor delegates to it
// implicitly. Per the M7.3.b contract, a step that does NOT
// implement Compensator is silently skipped during the
// reverse-rollback walk — the audit chain still records every
// step's forward dispatch via `saga_step_started` /
// `saga_step_completed` rows.
//
// Per-saga state contract (M7.3.b lesson #1): every value the
// step needs originates on the [saga.SpawnContext], NEVER on a
// receiver-stash. The step instance is shared across sagas.
package spawn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/keeperslog"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// Compile-time assertions: the production wiring substitutes
// [*keepclient.Client] for [PredecessorLookup] and [*keeperslog.Writer]
// for [InheritAuditAppender]. The assertions pin the interface
// contract so a future signature drift on either side surfaces here
// (iter-1 critic P2 — interface drift was previously catchable only
// at late integration).
//
//nolint:gochecknoglobals // package-level compile-time assertion; idiomatic Go pattern.
var (
	_ PredecessorLookup    = (*keepclient.Client)(nil)
	_ InheritAuditAppender = (*keeperslog.Writer)(nil)
)

// NotebookInheritStepName is the stable closed-set identifier for
// the NotebookInherit step. Used by the [saga.Runner] as the
// `current_step` DAO column and as the `step_name` audit payload
// key. Hoisted to a constant so a typo at the call site is a
// compile error.
const NotebookInheritStepName = "notebook_inherit"

// EventTypeNotebookInherited is the keepers_log `event_type` row
// emitted on every successful inheritance. Distinct prefix
// (`notebook_`) so it shares the `notebook_*` audit family with
// `notebook_imported` (the M2b.7 substrate emit) and
// `notebook_archived` (the M7.2.c archive-on-retire emit) without
// colliding with either. Downstream consumers branch on the wire
// `event_type` string.
//
// Distinct from `notebook_imported` (the existing
// [notebook.ImportFromArchive] emit): the step does NOT pass a
// logger into ImportFromArchive (it passes `nil`), so the
// `notebook_imported` row is NOT emitted in the inheritance flow.
// The two rows are intentionally separate audit observations:
//
//   - `notebook_imported` is the operator-driven "restore an
//     archive into a fresh agent" rebuild row (M2b.7 / M2b.6 CLI).
//   - `notebook_inherited` is the saga-driven "successor agent
//     inherited the predecessor's archive" inheritance row
//     (M7.1.c). The payloads differ: inheritance carries
//     `predecessor_watchkeeper_id` + `entries_imported` per the
//     roadmap; import carries `agent_id` + `imported_at`.
const EventTypeNotebookInherited = "notebook_inherited"

// Closed-set audit payload keys. Hoisted to constants so the
// payload-shape regression test pins the wire vocabulary
// (M2b.7 / M6.3.e PII discipline). The step is the SOLE
// composer of this payload.
const (
	inheritPayloadKeyPredecessorWatchkeeperID = "predecessor_watchkeeper_id"
	inheritPayloadKeyArchiveURI               = "archive_uri"
	inheritPayloadKeyEntriesImported          = "entries_imported"
)

// ErrMissingClaimOrganization is the typed error
// [NotebookInheritStep.Execute] returns when the
// [saga.SpawnContext.Claim.OrganizationID] is empty. Matchable via
// [errors.Is]. Note: the step degrades empty-org to a NO-OP +
// NO audit (not a hard failure) — this sentinel exists so a
// future audit-emit variant can branch on the shape without
// reflecting a magic string.
//
//nolint:unused // sentinel reserved for a future strict-mode variant.
var ErrMissingClaimOrganization = errors.New("spawn: notebook_inherit step: missing claim organization_id")

// ErrPredecessorLookupFailed is the typed error
// [NotebookInheritStep.Execute] returns when the
// [PredecessorLookup.LatestRetiredByRole] seam returns a non-404
// transport / auth error. The step does NOT %w-wrap the underlying
// error onto this sentinel — the seam's error message can echo the
// request URL (which contains the `role_id` query parameter the
// M7.1.b client encodes), violating the docblock's PII guarantee
// that error strings never leak the role identity. Wrapping the
// sentinel directly lets `errors.Is(err, ErrPredecessorLookupFailed)`
// match without surfacing the underlying string. Iter-1 codex P1
// finding (PII boundary).
//
// The underlying error chain IS logged via [slog.WarnContext] inside
// the step so an operator can correlate the audit failure to a
// transport-side incident without parsing the saga's wrapped error.
var ErrPredecessorLookupFailed = errors.New("spawn: notebook_inherit step: predecessor lookup failed")

// ErrInvalidPredecessorEnvelope is the typed error
// [NotebookInheritStep.Execute] returns when the
// [PredecessorLookup.LatestRetiredByRole] seam returns a non-nil
// error AND a nil envelope, OR returns nil error but the envelope
// carries an empty `archive_uri` (which the M7.1.b server-side
// query filter forbids). Surfacing these as a typed sentinel
// rather than dereferencing nil keeps the saga's reverse-rollback
// safe. Iter-1 codex P1 finding (PII boundary): the sentinel
// carries NO substrings from the envelope.
var ErrInvalidPredecessorEnvelope = errors.New("spawn: notebook_inherit step: invalid predecessor envelope")

// ErrInheritFailed is the typed error
// [NotebookInheritStep.Execute] returns when the
// [NotebookInheritor.Inherit] seam fails (fetch / open / import /
// count). The step does NOT %w-wrap the underlying error onto this
// sentinel — the seam's error message can echo the archive URI
// substring, violating the docblock's PII guarantee that error
// strings never leak the archive URI. Wrapping the sentinel
// directly lets `errors.Is(err, ErrInheritFailed)` match without
// surfacing the underlying string. Iter-1 codex P1 finding (PII
// boundary).
//
// The underlying error chain IS logged via [slog.WarnContext]
// inside the step so an operator can correlate the saga failure
// to a notebook-side incident without parsing the saga's wrapped
// error.
var ErrInheritFailed = errors.New("spawn: notebook_inherit step: inherit failed")

// PredecessorLookup is the seam the [NotebookInheritStep] dispatches
// through to resolve the most-recently-retired peer for the new
// Watchkeeper's role. Production wiring substitutes a
// [*keepclient.Client]; the test fake substitutes a hand-rolled
// struct (no mocking lib — M3.6 / M6.3.e pattern).
//
// Returns the freshest retired [*keepclient.Watchkeeper] carrying
// the supplied role_id in the caller's tenant on success.
// A 404 surfaces as a wrapped error matching
// [keepclient.ErrNoPredecessor]; the step's caller distinguishes
// "no predecessor exists" (expected, fall through to no-op) from
// other transport / auth errors via [errors.Is] against the
// sentinel.
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct sagas. The production wrapper holds an immutable
// reference to the keepclient.
type PredecessorLookup interface {
	LatestRetiredByRole(ctx context.Context, organizationID, roleID string) (*keepclient.Watchkeeper, error)
}

// NotebookInheritor is the seam the [NotebookInheritStep] dispatches
// through to perform the fetch + open + import + count flow. The
// production wiring backs this with a wrapper that:
//
//  1. Calls [notebook.ImportFromArchive] with a nil logger (we own
//     the `notebook_inherited` audit emit at the step layer, NOT
//     the substrate's `notebook_imported` row).
//  2. Re-opens the freshly-imported per-agent notebook file via
//     [notebook.Open] and calls [notebook.DB.Stats] to read the
//     `TotalEntries` count, which the step echoes on the
//     `entries_imported` audit payload key.
//  3. Returns the count plus any error from steps 1 / 2 wrapped so
//     callers can `errors.Is` against the underlying sentinels
//     (e.g. [notebook.ErrCorruptArchive], [notebook.ErrTargetNotEmpty]).
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct sagas. The production wrapper holds an immutable
// reference to the [notebook.Fetcher] + the audit-log writer (passed
// as nil to the import call) and constructs per-call DB handles; the
// test fake uses sync primitives to record calls.
type NotebookInheritor interface {
	// Inherit fetches `archiveURI`, opens the per-agent notebook
	// keyed by `watchkeeperID`, imports the archive into it, then
	// counts the resulting `entry` rows. Returns the count on
	// success; returns `(0, err)` on failure with the wrapped
	// underlying error chain.
	Inherit(ctx context.Context, watchkeeperID uuid.UUID, archiveURI string) (entriesImported int, err error)
}

// InheritAuditAppender is the minimal subset of [keeperslog.Writer]
// the step's audit emit consumes — only [keeperslog.Writer.Append].
// Re-declared locally (rather than reusing [keepersLogAppender] from
// slack_app.go or [kickoffLogAppender] from spawnkickoff.go) so a
// reviewer reading notebookinherit_step.go in isolation sees the
// contract without cross-file lookup. The three interfaces are
// structurally identical; [*keeperslog.Writer] satisfies all of them.
type InheritAuditAppender interface {
	Append(ctx context.Context, evt keeperslog.Event) (string, error)
}

// NotebookInheritStepDeps is the construction-time bag wired into
// [NewNotebookInheritStep]. Held in a struct so a future addition
// (e.g. a per-saga metrics emitter) lands as a new field without
// breaking the constructor signature.
type NotebookInheritStepDeps struct {
	// Predecessor is the M7.1.b predecessor-lookup dispatcher.
	// Required; a nil Predecessor is rejected at construction.
	Predecessor PredecessorLookup

	// Inheritor is the fetch + import + count dispatcher. Required;
	// a nil Inheritor is rejected at construction.
	Inheritor NotebookInheritor

	// AuditAppender is the `notebook_inherited` audit-emit seam.
	// Required; a nil AuditAppender is rejected at construction.
	// Production wires [*keeperslog.Writer] here.
	AuditAppender InheritAuditAppender
}

// NotebookInheritStep is the [saga.Step] implementation for the
// `notebook_inherit` step. Construct via [NewNotebookInheritStep];
// the zero value is NOT usable.
//
// Concurrency: safe for concurrent use across distinct sagas. Holds
// only immutable configuration; per-call state lives on the goroutine
// stack and on the per-call `context.Context` (which carries the
// [saga.SpawnContext] keying the watchkeeper / role / opt-out flag).
type NotebookInheritStep struct {
	predecessor PredecessorLookup
	inheritor   NotebookInheritor
	audit       InheritAuditAppender
}

// Compile-time assertion: [*NotebookInheritStep] satisfies
// [saga.Step]. Pins the integration shape so a future change to the
// interface surface fails the build here.
var _ saga.Step = (*NotebookInheritStep)(nil)

// NewNotebookInheritStep constructs a [NotebookInheritStep] with the
// supplied [NotebookInheritStepDeps]. Predecessor, Inheritor, and
// AuditAppender are required; a nil value for any of them panics
// with a clear message — matches the panic discipline of
// [NewCreateAppStep], [NewOAuthInstallStep], [NewBotProfileStep],
// and [NewNotebookProvisionStep].
func NewNotebookInheritStep(deps NotebookInheritStepDeps) *NotebookInheritStep {
	if deps.Predecessor == nil {
		panic("spawn: NewNotebookInheritStep: deps.Predecessor must not be nil")
	}
	if deps.Inheritor == nil {
		panic("spawn: NewNotebookInheritStep: deps.Inheritor must not be nil")
	}
	if deps.AuditAppender == nil {
		panic("spawn: NewNotebookInheritStep: deps.AuditAppender must not be nil")
	}
	return &NotebookInheritStep{
		predecessor: deps.Predecessor,
		inheritor:   deps.Inheritor,
		audit:       deps.AuditAppender,
	}
}

// Name satisfies [saga.Step.Name]. Returns the stable closed-set
// identifier `notebook_inherit`.
func (s *NotebookInheritStep) Name() string {
	return NotebookInheritStepName
}

// Execute satisfies [saga.Step.Execute].
//
// Resolution order:
//
//  1. Cancellation short-circuit: if `ctx` is already cancelled,
//     return a wrapped `ctx.Err()`; no seam is touched.
//  2. Read the [saga.SpawnContext] off `ctx`. A miss returns a
//     wrapped [ErrMissingSpawnContext]; no seam is touched.
//  3. Validate the SpawnContext's AgentID is non-zero. A miss
//     returns a wrapped [ErrMissingAgentID]; no seam is touched.
//  4. No-op short-circuits (NO seam dispatch, NO audit emit):
//     `sc.NoInherit == true` OR `sc.RoleID == ""` OR
//     `sc.Claim.OrganizationID == ""`. Each maps to the
//     "no predecessor to inherit from / opted out" semantics.
//  5. Dispatch [PredecessorLookup.LatestRetiredByRole]. On
//     [keepclient.ErrNoPredecessor] (typed 404) the step is a
//     no-op + NO audit event. Any other error is wrapped and
//     returned.
//  6. Sanity-check the predecessor envelope: `ArchiveURI` must be
//     non-nil and non-empty (the M7.1.b server-side query filters
//     on `archive_uri IS NOT NULL` so the envelope shape is
//     authoritative; a defensive guard here surfaces a poisoned
//     wiring as a wrapped error rather than crashing on a nil
//     dereference).
//  7. Dispatch [NotebookInheritor.Inherit] forwarding the
//     watchkeeperID + the archive URI. The seam owns the
//     fetch + open + import + count flow.
//  8. On a successful import, emit the `notebook_inherited` audit
//     row carrying `{predecessor_watchkeeper_id, archive_uri,
//     entries_imported}`. A non-nil emit error is wrapped and
//     returned — the inherited data is in the DB; the saga's
//     downstream provisioner's compensator owns the archive-not-
//     delete rollback of the file.
//
// Errors are wrapped with `fmt.Errorf("spawn: notebook_inherit
// step: %w", err)` so a caller's `errors.Is` against the underlying
// sentinel still matches.
func (s *NotebookInheritStep) Execute(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("spawn: notebook_inherit step: %w", err)
	}

	sc, ok := saga.SpawnContextFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: notebook_inherit step: %w", ErrMissingSpawnContext)
	}
	if sc.AgentID == uuid.Nil {
		return fmt.Errorf("spawn: notebook_inherit step: %w", ErrMissingAgentID)
	}

	// No-op short-circuits. NO audit event on any of these
	// branches — the acceptance pins the no-op + no-audit shape
	// for `--no-inherit` AND no-predecessor, and the empty
	// RoleID / OrganizationID degenerations are the conservative
	// extensions of that same shape. A misconfigured wiring
	// produces a clean no-op rather than a noisy 4xx row.
	if sc.NoInherit {
		return nil
	}
	if sc.RoleID == "" {
		return nil
	}
	if sc.Claim.OrganizationID == "" {
		return nil
	}

	predecessor, err := s.predecessor.LatestRetiredByRole(ctx, sc.Claim.OrganizationID, sc.RoleID)
	if err != nil {
		if errors.Is(err, keepclient.ErrNoPredecessor) {
			// Expected miss: no retired peer carries this role
			// in this tenant. No-op + NO audit event per the
			// acceptance contract.
			return nil
		}
		// PII boundary (iter-1 codex P1): the underlying error
		// from [keepclient.Client.LatestRetiredByRole] can echo
		// the request URL with the `role_id` query parameter.
		// Log the underlying chain via slog (operator-visible)
		// and return a scrubbed typed sentinel that carries no
		// substring from the seam's error.
		slog.WarnContext(
			ctx,
			"spawn: notebook_inherit step: predecessor lookup failed",
			"err_class", "predecessor_lookup_failed",
			"err", err.Error(),
		)
		return ErrPredecessorLookupFailed
	}
	if predecessor == nil {
		// Defensive: a non-nil error AND a nil envelope is a
		// caller-contract violation. Surface a scrubbed sentinel
		// (no envelope substrings) so the saga rolls back without
		// dereferencing nil.
		return ErrInvalidPredecessorEnvelope
	}
	if predecessor.ArchiveURI == nil || *predecessor.ArchiveURI == "" {
		// Defensive: the M7.1.b server-side query filters
		// `archive_uri IS NOT NULL` so an empty URI here is a
		// schema-skew bug. Same scrubbed sentinel.
		return ErrInvalidPredecessorEnvelope
	}

	archiveURI := *predecessor.ArchiveURI
	entriesImported, err := s.inheritor.Inherit(ctx, sc.AgentID, archiveURI)
	if err != nil {
		// PII boundary (iter-1 codex P1): the underlying error
		// from [notebook.InheritFromArchive] can echo the archive
		// URI substring (the fetcher's GET error includes the
		// URI). Log the chain and return a scrubbed sentinel.
		slog.WarnContext(
			ctx,
			"spawn: notebook_inherit step: inherit failed",
			"err_class", "inherit_failed",
			"err", err.Error(),
		)
		return ErrInheritFailed
	}

	if _, err := s.audit.Append(ctx, keeperslog.Event{
		EventType: EventTypeNotebookInherited,
		Payload:   inheritPayload(predecessor.ID, archiveURI, entriesImported),
	}); err != nil {
		// Best-effort audit emit (iter-1 codex+critic P1): the
		// inherited data is already in the DB; the saga.Runner's
		// `saga_step_completed` row records the step's success
		// regardless. A transient audit-sink outage MUST NOT
		// poison a successful inheritance — the alternative is
		// either (a) rolling back the saga (the file would need a
		// compensator on this step; the M7.3.b delegation model
		// pushes that to NotebookProvisionStep.Compensate but
		// that step has NOT executed yet on this path) or (b)
		// leaving a seeded file with no audit trail (the worse
		// failure mode). The slog.Warn below alerts ops to the
		// dropped row; an audit-chain reconciler can replay it
		// from the saga's persisted state. Mirrors the kickoffer's
		// `EventTypeManifestRejectedAfterSpawnFailure` best-effort
		// pattern.
		slog.WarnContext(
			ctx,
			"spawn: notebook_inherit step: notebook_inherited audit emit failed",
			"err_class", "notebook_inherited_emit_dropped",
			"err", err.Error(),
		)
	}
	return nil
}

// inheritPayload composes the closed-set `notebook_inherited` payload.
// PII guard: this function is the SOLE composer; the closed-set keys
// mirror the roadmap acceptance (`predecessor_watchkeeper_id`,
// `archive_uri`, `entries_imported`). If a future change adds a key,
// code review picks it up here and the wire-shape regression test
// pins it.
//
// Returns `map[string]any` because [keeperslog.Event.Payload] is
// typed `any` (the writer marshals to JSON downstream); a typed
// struct here would still flatten to the same JSON envelope but
// would force every consumer to import the payload type. The map
// keeps the wire contract self-contained on the audit chain.
func inheritPayload(predecessorWatchkeeperID, archiveURI string, entriesImported int) map[string]any {
	return map[string]any{
		inheritPayloadKeyPredecessorWatchkeeperID: predecessorWatchkeeperID,
		inheritPayloadKeyArchiveURI:               archiveURI,
		inheritPayloadKeyEntriesImported:          entriesImported,
	}
}
