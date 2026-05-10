// notebookprovision_step.go is the M7.1.d saga.Step implementation
// that provisions a per-agent [notebook.DB] file (the M2b.1 SQLite +
// sqlite-vec substrate) during the Watchkeeper spawn flow. The step:
//
//  1. Reads the [saga.SpawnContext] off the call's `context.Context`
//     and extracts the watchkeeperID (= [saga.SpawnContext.AgentID]).
//  2. Dispatches via the configured [NotebookProvisioner] seam, which
//     the production wiring backs with a wrapper that calls
//     `notebook.Open(ctx, watchkeeperID.String(), ...)` (creating
//     `<WATCHKEEPER_DATA>/notebook/<watchkeeperID>.sqlite` on first
//     touch) and seeds the agent's personality / language as
//     foundational entries via [notebook.DB.Remember]. The wrapper
//     converts the `uuid.UUID` to its canonical string form because
//     the [notebook.Open] signature takes `agentID string` (the
//     M2b.1 path validator uses the canonical UUID text form).
//
// Audit discipline (M7.1.c.a / M7.1.c.b.b / M7.1.c.c pattern, AC7):
// the step does NOT emit any new keepers_log event. The notebook
// substrate's M2b.7 mutation-audit emit (`notebook_entry_remembered`)
// fires from inside the production [NotebookProvisioner] when it
// writes the seed entries; the saga core ([saga.Runner]) emits
// `saga_step_started` / `saga_step_completed` around the dispatch.
//
// PII discipline: the [NotebookProfile] contents (Personality,
// Language) NEVER appear in any returned error string. The wrap chain
// surfaces only the step-prefix + the underlying sentinel (e.g.
// [ErrMissingSpawnContext], [ErrMissingAgentID], or the Provisioner's
// typed error).
//
// # M7.3.c rollback contract — archive-not-delete + flag-for-review
//
// On saga rollback the [saga.Runner] dispatches
// [NotebookProvisionStep.Compensate]. The roadmap mandates
// "archive-not-delete + flag-for-review" — the per-watchkeeper
// notebook file must NOT be deleted (any seed entries the M2b.7
// `notebook_entry_remembered` audit chain wrote are durable
// records of the failed spawn, valuable for postmortem) and the
// archived blob must be flagged for operator review (an aborted
// spawn that left a notebook file is a wiring or platform issue
// the operator should examine before retrying).
//
// Compensate dispatches via the [NotebookProvisionArchiver] seam
// which:
//
//  1. Streams the per-watchkeeper notebook file through the
//     existing M2b.1 [notebook.DB.Archive] surface into the
//     configured [archivestore.ArchiveStore].
//  2. Sets a closed-set "flagged_for_review" attribute on the
//     archived blob (production wiring writes a sidecar metadata
//     file or sets an S3 object tag).
//  3. Returns the archive URI on success; non-empty + non-nil-err
//     is the success contract (mirrors [NotebookArchiver] from the
//     M7.2.b retire saga).
//
// Compensate does NOT publish the URI to a [saga.RetireResult]
// outbox — the spawn-saga's audit chain alone records the
// rollback (`saga_step_compensated` for `notebook_provision`),
// the URI is operator-visible via the production archiver's own
// audit emit (`notebook_archived` per M2b.7) plus the
// flag-for-review marker.
//
// Per-saga state contract (M7.3.b lesson #1): watchkeeperID
// originates on the [saga.SpawnContext], NEVER on a
// receiver-stash. The notebook file path is derivable from
// watchkeeperID alone (the M2b.1 path validator).
package spawn

import (
	"context"
	"fmt"
	"net/url"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// NotebookProvisionStepName is the stable closed-set identifier for
// the NotebookProvision step. Used by the [saga.Runner] as the
// `current_step` DAO column and as the `step_name` audit payload key.
// Hoisted to a constant so a typo at the call site is a compile error.
// The literal string `notebook_provision` is also referenced as the
// canonical example in [saga.Step]'s docstring (`core/pkg/spawn/saga/saga.go`).
const NotebookProvisionStepName = "notebook_provision"

// NotebookProfile is the construction-time identity bundle the step
// hands to the [NotebookProvisioner] on every dispatch. Phase-1
// admin-grant flow: a static per-deployment profile (the wiring layer
// derives it from the seeded `bots/watchmaster.yaml` manifest). M7.x
// will replace this with a per-saga profile derived from the
// manifest_version row's personality / language fields.
//
// All fields are scalar strings: no internal map / slice / pointer
// fields, so the value is safe to copy by value across goroutines and
// no defensive deep copy is required at construction or on dispatch.
// If a future field grows a reference type (e.g. extra metadata
// map), follow the M7.1.c.c `cloneBotProfile` pattern: deep-copy at
// construction AND on every Execute.
type NotebookProfile struct {
	// Personality is the agent's free-form personality blurb. Forwarded
	// verbatim to the [NotebookProvisioner]; the production wiring
	// stores it as a foundational notebook entry the harness recalls
	// back when composing the system prompt at boot (M5.5.a templater
	// path). Empty is permitted — a deployment without a configured
	// personality still completes the saga with an empty seed.
	Personality string

	// Language is the agent's preferred natural language for the
	// system prompt, conventionally an IETF BCP 47 tag (e.g. "en",
	// "ru"). Forwarded verbatim to the [NotebookProvisioner]; the
	// production wiring stores it as a foundational notebook entry the
	// harness recalls back at boot. Empty is permitted — the harness
	// falls back to its default language when no value is seeded.
	Language string
}

// NotebookProvisioner is the seam the NotebookProvision step
// dispatches through. Implementations resolve the watchkeeper's
// per-agent [notebook.DB] file (creating it on first touch via
// `notebook.Open(ctx, watchkeeperID.String(), ...)` — the
// substrate's path validator consumes the canonical UUID text form)
// and seed the agent's personality / language as foundational
// entries via [notebook.DB.Remember]. Test wiring satisfies the
// interface with a hand-rolled fake (no mocking lib — M3.6 / M6.3.e
// pattern).
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct sagas. The production wrapper holds an immutable
// reference to the notebook + audit seams; the test fake uses sync
// primitives to record calls.
type NotebookProvisioner interface {
	// ProvisionNotebook ensures the per-agent notebook file backing
	// `watchkeeperID` exists (creating it on first touch) and seeds
	// the supplied `profile` as foundational entries. The
	// implementation is responsible for [notebook.Open] error wrapping,
	// the M2b.7 `notebook_entry_remembered` audit emit on the seed
	// writes, and any close-on-error rollback the substrate requires.
	//
	// Returns the wrapped underlying error chain so callers can
	// `errors.Is` / `errors.As` against the underlying sentinels (e.g.
	// [notebook.ErrInvalidAgentID] when the watchkeeperID is malformed,
	// or any [notebook] mutation sentinel surfaced by the seed write).
	ProvisionNotebook(ctx context.Context, watchkeeperID uuid.UUID, profile NotebookProfile) error
}

// NotebookProvisionStepDeps is the construction-time bag wired into
// [NewNotebookProvisionStep]. Held in a struct so a future addition
// (e.g. a manifest-driven profile builder) lands as a new field
// without breaking the constructor signature.
type NotebookProvisionStepDeps struct {
	// Provisioner is the per-watchkeeper [notebook.DB] dispatcher.
	// Required; a nil Provisioner is rejected at construction.
	Provisioner NotebookProvisioner

	// Archiver is the M7.3.c rollback seam dispatched by
	// [NotebookProvisionStep.Compensate]. Required; a nil Archiver is
	// rejected at construction. The seam owns archive-not-delete +
	// flag-for-review; production wiring backs it with a wrapper
	// around [notebook.ArchiveOnRetire] + an
	// [archivestore.ArchiveStore] tag-set call.
	Archiver NotebookProvisionArchiver

	// Profile is the [NotebookProfile] applied on every saga run.
	// Phase-1 admin-grant flow: a static per-deployment profile (the
	// wiring layer derives it from the seeded `bots/watchmaster.yaml`
	// manifest). An entirely-empty profile is a documented no-op at
	// the production [NotebookProvisioner] (the file is still created;
	// no seed entries are written); the step still runs (the
	// saga.Runner emits started/completed regardless).
	Profile NotebookProfile
}

// NotebookProvisionStep is the [saga.Step] implementation for the
// `notebook_provision` step. Construct via [NewNotebookProvisionStep];
// the zero value is NOT usable.
//
// Concurrency: safe for concurrent use across distinct sagas. Holds
// only immutable configuration; per-call state lives on the goroutine
// stack and on the per-call `context.Context` (which carries the
// [saga.SpawnContext] keying the watchkeeper).
type NotebookProvisionStep struct {
	provisioner NotebookProvisioner
	archiver    NotebookProvisionArchiver
	profile     NotebookProfile
}

// Compile-time assertion: [*NotebookProvisionStep] satisfies
// [saga.Step]. Pins the integration shape so a future change to the
// interface surface fails the build here.
var _ saga.Step = (*NotebookProvisionStep)(nil)

// Compile-time assertion: [*NotebookProvisionStep] satisfies
// [saga.Compensator] (M7.3.c). Pins the rollback contract.
var _ saga.Compensator = (*NotebookProvisionStep)(nil)

// NewNotebookProvisionStep constructs a [NotebookProvisionStep] with
// the supplied [NotebookProvisionStepDeps]. Provisioner is required;
// a nil value panics with a clear message — matches the panic
// discipline of [NewCreateAppStep], [NewOAuthInstallStep], and
// [NewBotProfileStep].
//
// An empty [NotebookProvisionStepDeps.Profile] is permitted: the
// production [NotebookProvisioner] treats both empty Personality and
// empty Language as documented no-ops on the seed-write path, so the
// step degrades gracefully when a deployment does not supply identity
// fields.
//
// Profile defensive copy: [NotebookProfile] holds only scalar string
// fields, so a value-copy is sufficient. If a future field grows a
// reference type (map / slice / pointer), follow the M7.1.c.c
// `cloneBotProfile` pattern and add a deep-copy here AND on every
// Execute.
func NewNotebookProvisionStep(deps NotebookProvisionStepDeps) *NotebookProvisionStep {
	if deps.Provisioner == nil {
		panic("spawn: NewNotebookProvisionStep: deps.Provisioner must not be nil")
	}
	if deps.Archiver == nil {
		panic("spawn: NewNotebookProvisionStep: deps.Archiver must not be nil")
	}
	return &NotebookProvisionStep{
		provisioner: deps.Provisioner,
		archiver:    deps.Archiver,
		profile:     deps.Profile,
	}
}

// Name satisfies [saga.Step.Name]. Returns the stable closed-set
// identifier `notebook_provision`. The runner uses it as the
// `current_step` DAO column and as the `step_name` audit payload key.
func (s *NotebookProvisionStep) Name() string {
	return NotebookProvisionStepName
}

// Execute satisfies [saga.Step.Execute].
//
// Resolution order:
//
//  1. Cancellation short-circuit: if `ctx` is already cancelled, return
//     a wrapped `ctx.Err()`; the Provisioner is NOT touched.
//  2. Read the [saga.SpawnContext] off `ctx`. A miss returns a wrapped
//     [ErrMissingSpawnContext]; the Provisioner is NOT touched.
//  3. Validate the SpawnContext's AgentID is non-zero (uuid.Nil cannot
//     be a per-agent notebook file key). A miss returns a wrapped
//     [ErrMissingAgentID]; the Provisioner is NOT touched.
//  4. Dispatch through the [NotebookProvisioner] seam, forwarding the
//     watchkeeperID + the construction-time profile.
//
// Errors are wrapped with `fmt.Errorf("spawn: notebook_provision
// step: %w", err)` so a caller's `errors.Is` against the underlying
// sentinel still matches.
//
// Audit discipline: this method does NOT call
// [keeperslog.Writer.Append] (AC7). The audit chain belongs to the
// saga core; the M2b.7 mutation-audit emit on seed writes belongs to
// the [notebook.DB] substrate the production [NotebookProvisioner]
// wraps.
func (s *NotebookProvisionStep) Execute(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("spawn: notebook_provision step: %w", err)
	}

	sc, ok := saga.SpawnContextFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: notebook_provision step: %w", ErrMissingSpawnContext)
	}
	if sc.AgentID == uuid.Nil {
		return fmt.Errorf("spawn: notebook_provision step: %w", ErrMissingAgentID)
	}

	if err := s.provisioner.ProvisionNotebook(ctx, sc.AgentID, s.profile); err != nil {
		return fmt.Errorf("spawn: notebook_provision step: %w", err)
	}
	return nil
}

// NotebookProvisionArchiver is the M7.3.c rollback seam the
// [NotebookProvisionStep.Compensate] dispatches through.
// Implementations undo the externally-visible side effect produced
// by [NotebookProvisionStep.Execute] via archive-not-delete +
// flag-for-review:
//
//  1. Stream the per-watchkeeper notebook file through
//     [notebook.DB.Archive] into the configured
//     [archivestore.ArchiveStore].
//  2. Set a closed-set "flagged_for_review" attribute on the
//     archived blob so an operator's review queue surfaces the
//     aborted-spawn artifact.
//  3. NEVER delete the source notebook file — the seed entries
//     written by [NotebookProvisionStep.Execute] are durable
//     records of the failed spawn that an operator might need for
//     post-mortem.
//
// The seam returns the archive URI on success (non-empty +
// shape-valid per the M7.2.b discipline). The
// [NotebookProvisionStep.Compensate] body validates the URI
// shape via [net/url.Parse] and reuses the M7.2.b sentinels
// [ErrEmptyArchiveURI] / [ErrInvalidArchiveURI] for shape errors —
// a buggy archiver returning `"garbage"` MUST surface as a
// `saga_compensation_failed` audit row rather than silently
// poisoning a future review-queue lookup.
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct sagas. The production wrapper holds an immutable
// reference to the notebook + archivestore + audit seams; the test
// fake uses sync primitives to record calls.
//
// Idempotency: implementations MUST be safe to call MORE than once
// with the same `watchkeeperID`. A re-archive of an already-
// archived notebook MAY produce a different URI (e.g. a new
// timestamped object) — the rollback contract treats both as
// success; the operator's review queue de-dupes by watchkeeperID.
//
// Typed-error contract: errors SHOULD implement
// [saga.LastErrorClassed] to override the default
// `step_compensate_error` sentinel (e.g.
// `notebook_archive_store_full`,
// `notebook_archive_flag_failed`).
type NotebookProvisionArchiver interface {
	// ArchiveAndFlagForReview archives the per-agent notebook keyed
	// by `watchkeeperID` AND sets the closed-set "flagged_for_review"
	// attribute on the resulting archive blob. Returns the archive
	// URI on success; the URI MUST be non-empty AND shape-valid
	// (parses via [net/url.Parse] with a non-empty scheme — see the
	// M7.2.b [NotebookArchiver] precedent). Returns `("", err)` on
	// failure with the underlying error chain wrapped so callers
	// can `errors.Is` against substrate sentinels.
	ArchiveAndFlagForReview(ctx context.Context, watchkeeperID uuid.UUID) (uri string, err error)
}

// Compensate satisfies [saga.Compensator].
//
// Resolution order:
//
//  1. Read the [saga.SpawnContext] off `ctx`. A miss returns a
//     wrapped [ErrMissingSpawnContext]; the [NotebookProvisionArchiver]
//     is NOT touched. The [saga.Runner] dispatches Compensate under
//     [context.WithoutCancel] (M7.3.b iter-1 #2) so a parent
//     cancellation does NOT poison the rollback walk.
//  2. Validate the SpawnContext's AgentID is non-zero. A miss
//     returns a wrapped [ErrMissingAgentID]; the seam is NOT
//     touched.
//  3. Dispatch through the [NotebookProvisionArchiver] seam,
//     forwarding the watchkeeperID. The seam owns the archive +
//     flag-for-review work.
//  4. On the seam's success branch, validate the returned URI
//     non-empty ([ErrEmptyArchiveURI]) AND shape-correct
//     (parses via [net/url.Parse] with a non-empty scheme —
//     [ErrInvalidArchiveURI]). A buggy wrapper returning
//     `"garbage"` MUST NOT silently succeed — the operator's
//     review queue would then key off an unparseable string.
//
// Errors are wrapped with `fmt.Errorf("spawn: notebook_provision
// step compensate: %w", err)` so a caller's `errors.Is` against
// the underlying sentinel still matches.
//
// Audit discipline: this method does NOT call
// [keeperslog.Writer.Append] (AC7). The audit chain belongs to the
// saga core; the M2b.7 mutation-audit emit on archive belongs to
// the [notebook.ArchiveOnRetire] substrate the production
// [NotebookProvisionArchiver] wraps.
//
// PII discipline: NEVER reflects the [NotebookProfile] contents,
// the seed entries, or the archive URI substring (the URI is the
// SUCCESS payload, not a failure-message ingredient) into the
// returned error string.
func (s *NotebookProvisionStep) Compensate(ctx context.Context) error {
	sc, ok := saga.SpawnContextFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: notebook_provision step compensate: %w", ErrMissingSpawnContext)
	}
	if sc.AgentID == uuid.Nil {
		return fmt.Errorf("spawn: notebook_provision step compensate: %w", ErrMissingAgentID)
	}

	uri, err := s.archiver.ArchiveAndFlagForReview(ctx, sc.AgentID)
	if err != nil {
		return fmt.Errorf("spawn: notebook_provision step compensate: %w", err)
	}
	if uri == "" {
		return fmt.Errorf("spawn: notebook_provision step compensate: %w", ErrEmptyArchiveURI)
	}
	if parsed, parseErr := url.Parse(uri); parseErr != nil || parsed.Scheme == "" {
		return fmt.Errorf("spawn: notebook_provision step compensate: %w", ErrInvalidArchiveURI)
	}
	return nil
}
