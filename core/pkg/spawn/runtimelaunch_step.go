// runtimelaunch_step.go is the M7.1.e saga.Step implementation that
// drives the spawn flow to completion: it boots the watchkeeper's
// agent runtime (the M5.3+ harness via [runtime.AgentRuntime.Start])
// AND posts the bot's intro message into the deployment's announce
// channel via [messenger.Adapter.SendMessage]. The step is the LAST
// concrete step in the M7.1 spawn saga — its successful return
// triggers the saga's `saga_completed` audit emit (see [saga.Runner])
// and the watchkeeper is "live" for human interaction.
//
// The step:
//
//  1. Reads the [saga.SpawnContext] off the call's `context.Context`
//     and extracts the watchkeeperID (= [saga.SpawnContext.AgentID]).
//  2. Dispatches via the configured [RuntimeLauncher] seam, which
//     the production wiring backs with a wrapper that:
//     a. Resolves the watchkeeper's manifest projection (M5.5 loader
//     output: SystemPrompt, Toolset, AuthorityMatrix, Model, …).
//     b. Calls [runtime.AgentRuntime.Start] with the projected
//     [runtime.Manifest] (Personality / Language seeded from the
//     supplied [RuntimeLaunchProfile]).
//     c. Resolves the bot's installed [messenger.Adapter] clone
//     (re-authenticated using the M7.1.c.b.b encrypted bot token)
//     and posts the intro message via
//     [messenger.Adapter.SendMessage] to the deployment-configured
//     announce channel.
//
// Audit discipline (M7.1.c.a / .c.b.b / .c.c / .d pattern, AC7): the
// step does NOT emit any new keepers_log event. The saga core
// ([saga.Runner]) emits `saga_step_started` / `saga_step_completed`
// around the dispatch; the runtime layer's own audit emit (M5.6.c
// `lesson_learned` reflection) and the messenger's redaction-disciplined
// `chat.postMessage` exchange ride underneath the [RuntimeLauncher]
// implementation.
//
// PII discipline: the [RuntimeLaunchProfile] contents (Personality,
// Language, IntroText) NEVER appear in any returned error string. The
// wrap chain surfaces the step-prefix plus the underlying sentinel
// (e.g. [ErrMissingSpawnContext], [ErrMissingAgentID],
// [ErrMissingManifestVersion], or the Launcher's typed error). The
// reused [ErrMissingSpawnContext] / [ErrMissingAgentID] /
// [ErrMissingManifestVersion] sentinels were minted in M7.1.c.a's
// [CreateAppStep] so their literal text reads
// `"spawn: create_app step: missing ..."`; that historical prefix
// surfaces inside the M7.1.e wrap chain (`spawn: runtime_launch step:
// spawn: create_app step: missing ...`) — not a matching bug
// ([errors.Is] still resolves) but a callout for log-grep tooling.
//
// # M7.3.c rollback contract — runtime teardown + cost finalisation
//
// On saga rollback the [saga.Runner] dispatches
// [RuntimeLaunchStep.Compensate] in REVERSE forward order. The
// roadmap mandates "runtime teardown, cost-record finalisation":
//
//  1. Stop the running agent runtime ([runtime.Subscription.Stop]
//     equivalent) keyed by watchkeeperID.
//  2. Finalise the cost-record ledger for any in-flight
//     `llm_turn_cost_*` rows the runtime emitted before the abort
//     (the M6.3.e cost-logger decorator emits one row per
//     successful LLM turn; an aborted runtime may have observed N
//     turns whose rows are valid but no `lifecycle_completed`
//     terminal row will ever land — the finalisation step writes
//     a synthetic terminal row so a downstream cost aggregator
//     can close the ledger).
//
// Both actions live behind the [RuntimeTeardown] seam; production
// wiring backs it with a wrapper around the runtime's
// [runtime.AgentRuntime.Stop] (or its Subscription.Stop equivalent)
// and the cost package's ledger-finalisation surface.
//
// At-least-once semantics: the M7.1.e doc-block already pins that
// [RuntimeLauncher.LaunchRuntime] is at-least-once on the spawn
// path; the M7.3.c rollback inherits the same semantics on the
// teardown side. A repeat Compensate against an already-stopped
// runtime MUST return nil (idempotent).
//
// Per-saga state contract (M7.3.b lesson #1): watchkeeperID +
// manifestVersionID originate on the [saga.SpawnContext], NEVER
// on a receiver-stash. The runtime instance is keyed off
// watchkeeperID; the manifestVersionID is forwarded so a future
// teardown impl that needs to drain version-pinned cost rows can
// branch on it without re-resolving the saga.
package spawn

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// RuntimeLaunchStepName is the stable closed-set identifier for the
// RuntimeLaunch step. Used by the [saga.Runner] as the `current_step`
// DAO column and as the `step_name` audit payload key. Hoisted to a
// constant so a typo at the call site is a compile error.
const RuntimeLaunchStepName = "runtime_launch"

// RuntimeLaunchProfile is the construction-time identity-and-greeting
// bundle the step hands to the [RuntimeLauncher] on every dispatch.
// Phase-1 admin-grant flow: a static per-deployment profile (the
// wiring layer derives it from the seeded `bots/watchmaster.yaml`
// manifest). M7.x will replace this with a per-saga profile derived
// from the manifest_version row.
//
// All fields are scalar strings: no internal map / slice / pointer
// fields, so the value is safe to copy by value across goroutines and
// no defensive deep copy is required at construction or on dispatch.
// If a future field grows a reference type (e.g. an `Attachments`
// byte-slice for the intro message), follow the M7.1.c.c
// `cloneBotProfile` pattern: deep-copy at construction AND on every
// Execute.
type RuntimeLaunchProfile struct {
	// Personality is the agent's free-form personality blurb.
	// Forwarded verbatim to the [RuntimeLauncher]; the production
	// wiring lifts it onto [runtime.Manifest.Personality] when starting
	// the harness session. Empty is permitted — a deployment without a
	// configured personality still completes the saga (the runtime
	// composes a system prompt without the persona blob).
	Personality string

	// Language is the agent's preferred natural language for the
	// system prompt, conventionally an IETF BCP 47 tag (e.g. "en",
	// "ru"). Forwarded verbatim to the [RuntimeLauncher]; the
	// production wiring lifts it onto [runtime.Manifest.Language] and
	// onto the intro message's locale-formatted body. Empty is
	// permitted — the runtime falls back to its default language when
	// no value is supplied.
	Language string

	// IntroText is the message body the production wrapper posts to
	// the deployment-configured announce channel after
	// [runtime.AgentRuntime.Start] succeeds. Empty is permitted — a
	// deployment without a configured intro message still completes
	// the saga (the wrapper short-circuits the SendMessage call when
	// IntroText is empty).
	IntroText string
}

// RuntimeLauncher is the seam the RuntimeLaunch step dispatches
// through. Implementations resolve the watchkeeper's manifest
// projection keyed on `manifestVersionID` (M5.5 loader output —
// pinning the saga to the version snapshotted at approval time, NOT
// the bot's current-active manifest), call
// [runtime.AgentRuntime.Start] with a [runtime.Manifest] composed
// from the projection plus the supplied [RuntimeLaunchProfile]
// (Personality / Language), and post the bot's intro message via
// [messenger.Adapter.SendMessage] to the deployment-configured
// announce channel. Test wiring satisfies the interface with a
// hand-rolled fake (no mocking lib — M3.6 / M6.3.e pattern).
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct sagas. The production wrapper holds an immutable
// reference to the runtime / messenger / DAO seams; the test fake
// uses sync primitives to record calls.
//
// Idempotency contract (Phase-1 admin-grant): until M7.3 ships
// compensations, [LaunchRuntime] is the only retry boundary covering
// BOTH `runtime.AgentRuntime.Start` AND the intro `SendMessage`. A
// partial success (Start succeeds, intro post fails) returns an error
// from the seam → the saga.Runner records `saga_failed` while the
// runtime is already live ("live but silent"). Implementations MUST
// therefore be safe to call AGAIN with the same `watchkeeperID` and
// `manifestVersionID` — typically by detecting an already-running
// runtime via a DAO lookup and either no-oping or emitting a
// platform-specific re-announce. The step itself does not decide
// retry policy; the operator (or the M7.3 compensator) does. This is
// a deliberate, documented limitation; the M7.1.e lesson narrative
// names it explicitly so a future maintainer cannot mistake the
// at-least-once semantics for exactly-once.
type RuntimeLauncher interface {
	// LaunchRuntime boots the agent runtime for `watchkeeperID`
	// using the manifest pinned by `manifestVersionID` and posts the
	// bot's intro message. The implementation is responsible for
	// [runtime.AgentRuntime.Start] error wrapping, the M5.6.c
	// reflector emit on tool-error paths (rides underneath), and the
	// messenger's redaction-disciplined `chat.postMessage` exchange.
	//
	// Implementations MUST be idempotent across retries on the same
	// `(watchkeeperID, manifestVersionID)` pair (see the type-level
	// idempotency contract above).
	//
	// Returns the wrapped underlying error chain so callers can
	// `errors.Is` / `errors.As` against the underlying sentinels
	// (e.g. [ErrCredsNotFound] when the M7.1.c.b.b row is missing,
	// [runtime.ErrInvalidManifest] when the projection fails
	// validation, or any [messenger] sentinel surfaced by the intro
	// post).
	LaunchRuntime(ctx context.Context, watchkeeperID uuid.UUID, manifestVersionID uuid.UUID, profile RuntimeLaunchProfile) error
}

// RuntimeLaunchStepDeps is the construction-time bag wired into
// [NewRuntimeLaunchStep]. Held in a struct so a future addition (e.g.
// a manifest-driven profile builder) lands as a new field without
// breaking the constructor signature.
type RuntimeLaunchStepDeps struct {
	// Launcher is the per-watchkeeper runtime + intro-message
	// dispatcher. Required; a nil Launcher is rejected at construction.
	Launcher RuntimeLauncher

	// Teardown is the M7.3.c rollback seam dispatched by
	// [RuntimeLaunchStep.Compensate]. Required; a nil Teardown is
	// rejected at construction. The seam owns runtime stop +
	// cost-record finalisation; production wiring backs it with a
	// wrapper around the runtime's stop primitive and the cost
	// package's ledger surface.
	Teardown RuntimeTeardown

	// Profile is the [RuntimeLaunchProfile] applied on every saga run.
	// Phase-1 admin-grant flow: a static per-deployment profile (the
	// wiring layer derives it from the seeded `bots/watchmaster.yaml`
	// manifest). An entirely-empty profile is a documented partial
	// no-op at the production [RuntimeLauncher] (the runtime still
	// boots; the intro post is skipped when IntroText is empty); the
	// step still runs (the saga.Runner emits started/completed
	// regardless).
	Profile RuntimeLaunchProfile
}

// RuntimeLaunchStep is the [saga.Step] implementation for the
// `runtime_launch` step. Construct via [NewRuntimeLaunchStep]; the
// zero value is NOT usable.
//
// Concurrency: safe for concurrent use across distinct sagas. Holds
// only immutable configuration; per-call state lives on the goroutine
// stack and on the per-call `context.Context` (which carries the
// [saga.SpawnContext] keying the watchkeeper).
type RuntimeLaunchStep struct {
	launcher RuntimeLauncher
	teardown RuntimeTeardown
	profile  RuntimeLaunchProfile
}

// Compile-time assertion: [*RuntimeLaunchStep] satisfies [saga.Step].
// Pins the integration shape so a future change to the interface
// surface fails the build here.
var _ saga.Step = (*RuntimeLaunchStep)(nil)

// Compile-time assertion: [*RuntimeLaunchStep] satisfies
// [saga.Compensator] (M7.3.c). Pins the rollback contract.
var _ saga.Compensator = (*RuntimeLaunchStep)(nil)

// NewRuntimeLaunchStep constructs a [RuntimeLaunchStep] with the
// supplied [RuntimeLaunchStepDeps]. Launcher is required; a nil value
// panics with a clear message — matches the panic discipline of
// [NewCreateAppStep], [NewOAuthInstallStep], [NewBotProfileStep], and
// [NewNotebookProvisionStep].
//
// An empty [RuntimeLaunchStepDeps.Profile] is permitted: the
// production [RuntimeLauncher] treats empty Personality and empty
// Language as documented no-ops at the runtime-projection layer, and
// empty IntroText as a documented no-op at the messenger-post layer,
// so the step degrades gracefully when a deployment does not supply
// identity / greeting fields.
//
// Profile defensive copy: [RuntimeLaunchProfile] holds only scalar
// string fields, so a value-copy is sufficient. If a future field
// grows a reference type (map / slice / pointer), follow the
// M7.1.c.c `cloneBotProfile` pattern and add a deep-copy here AND on
// every Execute.
func NewRuntimeLaunchStep(deps RuntimeLaunchStepDeps) *RuntimeLaunchStep {
	if deps.Launcher == nil {
		panic("spawn: NewRuntimeLaunchStep: deps.Launcher must not be nil")
	}
	if deps.Teardown == nil {
		panic("spawn: NewRuntimeLaunchStep: deps.Teardown must not be nil")
	}
	return &RuntimeLaunchStep{
		launcher: deps.Launcher,
		teardown: deps.Teardown,
		profile:  deps.Profile,
	}
}

// Name satisfies [saga.Step.Name]. Returns the stable closed-set
// identifier `runtime_launch`. The runner uses it as the
// `current_step` DAO column and as the `step_name` audit payload key.
func (s *RuntimeLaunchStep) Name() string {
	return RuntimeLaunchStepName
}

// Execute satisfies [saga.Step.Execute].
//
// Resolution order:
//
//  1. Cancellation short-circuit: if `ctx` is already cancelled,
//     return a wrapped `ctx.Err()`; the Launcher is NOT touched.
//  2. Read the [saga.SpawnContext] off `ctx`. A miss returns a
//     wrapped [ErrMissingSpawnContext]; the Launcher is NOT touched.
//  3. Validate the SpawnContext's ManifestVersionID is non-zero
//     (uuid.Nil cannot pin a manifest-version snapshot). A miss
//     returns a wrapped [ErrMissingManifestVersion]; the Launcher is
//     NOT touched. Mirrors [CreateAppStep.Execute]: the saga's
//     manifest-version pin is load-bearing for runtime determinism
//     so a wrapper cannot fall back to "current active manifest"
//     and silently boot the wrong runtime config.
//  4. Validate the SpawnContext's AgentID is non-zero (uuid.Nil
//     cannot be a per-agent runtime key). A miss returns a wrapped
//     [ErrMissingAgentID]; the Launcher is NOT touched.
//  5. Dispatch through the [RuntimeLauncher] seam, forwarding the
//     watchkeeperID + manifestVersionID + the construction-time
//     profile.
//
// Errors are wrapped with `fmt.Errorf("spawn: runtime_launch step:
// %w", err)` so a caller's `errors.Is` against the underlying
// sentinel still matches.
//
// Audit discipline: this method does NOT call
// [keeperslog.Writer.Append] (AC7). The audit chain belongs to the
// saga core; the M5.6.c reflector emit on tool-error paths and the
// messenger's redaction-disciplined `chat.postMessage` exchange ride
// underneath the [RuntimeLauncher] implementation.
func (s *RuntimeLaunchStep) Execute(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("spawn: runtime_launch step: %w", err)
	}

	sc, ok := saga.SpawnContextFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: runtime_launch step: %w", ErrMissingSpawnContext)
	}
	if sc.ManifestVersionID == uuid.Nil {
		return fmt.Errorf("spawn: runtime_launch step: %w", ErrMissingManifestVersion)
	}
	if sc.AgentID == uuid.Nil {
		return fmt.Errorf("spawn: runtime_launch step: %w", ErrMissingAgentID)
	}

	if err := s.launcher.LaunchRuntime(ctx, sc.AgentID, sc.ManifestVersionID, s.profile); err != nil {
		return fmt.Errorf("spawn: runtime_launch step: %w", err)
	}
	return nil
}

// RuntimeTeardown is the M7.3.c rollback seam the
// [RuntimeLaunchStep.Compensate] dispatches through. Implementations
// undo the externally-visible side effect produced by
// [RuntimeLaunchStep.Execute]:
//
//  1. Stop the running agent runtime keyed by `watchkeeperID`
//     ([runtime.AgentRuntime.Stop] equivalent).
//  2. Finalise the cost-record ledger for any in-flight
//     `llm_turn_cost_*` rows the runtime emitted before the abort
//     so a downstream cost aggregator can close the ledger
//     deterministically.
//
// `manifestVersionID` is forwarded so a future teardown impl that
// needs to drain version-pinned cost rows can branch on it without
// re-resolving the saga; the M7.3.c production wrapper currently
// uses only `watchkeeperID` (the runtime is keyed off it; the cost
// aggregator's join is per-watchkeeper).
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct sagas. The production wrapper holds an immutable
// reference to the runtime + cost seams; the test fake uses sync
// primitives to record calls.
//
// Idempotency: implementations MUST be safe to call MORE than once
// with the same `(watchkeeperID, manifestVersionID)` pair. A repeat
// Teardown against an already-stopped runtime returns nil; the
// runtime's [runtime.Subscription.Stop] surface already documents
// idempotency for this case (see `core/pkg/runtime/README.md`).
//
// Typed-error contract: errors SHOULD implement
// [saga.LastErrorClassed] to override the default
// `step_compensate_error` sentinel (e.g.
// `runtime_teardown_runtime_stuck`,
// `runtime_teardown_cost_finalise_failed`).
//
// PII discipline: implementations MUST NOT reflect runtime
// session state, in-flight LLM turn payloads, or intro-message
// substrings into returned error strings.
type RuntimeTeardown interface {
	// Teardown stops the runtime + finalises the cost ledger for
	// `watchkeeperID` / `manifestVersionID`. Returns nil on success;
	// returns a typed error chain on failure. MUST be idempotent
	// (see type-level discipline above).
	Teardown(ctx context.Context, watchkeeperID uuid.UUID, manifestVersionID uuid.UUID) error
}

// Compensate satisfies [saga.Compensator].
//
// Resolution order:
//
//  1. Read the [saga.SpawnContext] off `ctx`. A miss returns a
//     wrapped [ErrMissingSpawnContext]; the [RuntimeTeardown] is
//     NOT touched. The [saga.Runner] dispatches Compensate under
//     [context.WithoutCancel] (M7.3.b iter-1 #2) so a parent
//     cancellation does NOT poison the rollback walk.
//  2. Validate the SpawnContext's ManifestVersionID is non-zero
//     (uuid.Nil cannot pin a version-scoped cost ledger drain).
//     A miss returns a wrapped [ErrMissingManifestVersion]; the
//     seam is NOT touched. Mirrors [RuntimeLaunchStep.Execute]'s
//     ordering.
//  3. Validate the SpawnContext's AgentID is non-zero. A miss
//     returns a wrapped [ErrMissingAgentID]; the seam is NOT
//     touched.
//  4. Dispatch through the [RuntimeTeardown] seam, forwarding the
//     watchkeeperID + manifestVersionID. The seam owns the runtime
//     stop + cost-record finalisation work.
//
// Errors are wrapped with `fmt.Errorf("spawn: runtime_launch step
// compensate: %w", err)` so a caller's `errors.Is` against the
// underlying sentinel still matches.
//
// Audit discipline: this method does NOT call
// [keeperslog.Writer.Append] (AC7). The audit chain belongs to the
// saga core; the M6.3.e cost-logger decorator's
// `llm_turn_cost_completed` rows ride underneath the runtime layer
// the production [RuntimeTeardown] wraps.
//
// PII discipline: NEVER reflects runtime session state, in-flight
// LLM turn payloads, or the intro-message body into the returned
// error string.
func (s *RuntimeLaunchStep) Compensate(ctx context.Context) error {
	sc, ok := saga.SpawnContextFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: runtime_launch step compensate: %w", ErrMissingSpawnContext)
	}
	if sc.ManifestVersionID == uuid.Nil {
		return fmt.Errorf("spawn: runtime_launch step compensate: %w", ErrMissingManifestVersion)
	}
	if sc.AgentID == uuid.Nil {
		return fmt.Errorf("spawn: runtime_launch step compensate: %w", ErrMissingAgentID)
	}

	if err := s.teardown.Teardown(ctx, sc.AgentID, sc.ManifestVersionID); err != nil {
		return fmt.Errorf("spawn: runtime_launch step compensate: %w", err)
	}
	return nil
}
