// botprofile_step.go is the M7.1.c.c saga.Step implementation that
// applies a watchkeeper's [messenger.BotProfile] to its already-
// installed Slack bot during the spawn flow. The step:
//
//  1. Reads the [saga.SpawnContext] off the call's `context.Context`
//     and extracts the watchkeeperID (= [saga.SpawnContext.AgentID]).
//  2. Dispatches via the configured [BotProfileSetter] seam, which the
//     production wiring backs with a clone of the messenger
//     [slack.Client] re-authenticated using the bot OAuth token
//     M7.1.c.b.b stored (encrypted) on the same watchkeeper row.
//
// Audit discipline (M7.1.c.a / M7.1.c.b.b pattern, AC7): the step
// does NOT emit any new keepers_log event. The underlying
// [slack.Client.Do] redaction discipline applies to the
// `users.profile.set` exchange itself; the saga core ([saga.Runner])
// emits `saga_step_started` / `saga_step_completed`.
//
// PII discipline: the [messenger.BotProfile] contents (DisplayName,
// StatusText, Metadata) NEVER appear in any returned error string.
// The wrap chain surfaces only the step-prefix + the underlying
// sentinel (e.g. [ErrMissingSpawnContext], [ErrMissingAgentID], or
// the Setter's typed error).
package spawn

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// BotProfileStepName is the stable closed-set identifier for the
// BotProfile step. Used by the [saga.Runner] as the `current_step`
// DAO column and as the `step_name` audit payload key. Hoisted to a
// constant so a typo at the call site is a compile error.
const BotProfileStepName = "bot_profile"

// BotProfileSetter is the seam the BotProfile step dispatches through.
// Implementations resolve the watchkeeper's bot OAuth token (stored
// encrypted by M7.1.c.b.b on the slack_app_creds row), build a
// per-call [messenger.Adapter] clone re-authenticated as that bot, and
// invoke [messenger.Adapter.SetBotProfile]. Test wiring satisfies the
// interface with a hand-rolled fake (no mocking lib — M3.6 / M6.3.e
// pattern).
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct sagas. The production wrapper holds an immutable
// reference to the secrets / DAO / messenger seams and builds per-call
// clones; the test fake uses sync primitives to record calls.
type BotProfileSetter interface {
	// SetBotProfile applies `profile` to the bot owned by the
	// watchkeeper identified by `watchkeeperID`. The implementation
	// resolves the bot's OAuth token from the M7.1.c.b.b creds row,
	// constructs a per-call messenger client authenticated as that
	// bot, and forwards to [messenger.Adapter.SetBotProfile].
	//
	// Returns the wrapped underlying error chain so callers can
	// `errors.Is` / `errors.As` against the underlying sentinels
	// (e.g. [ErrCredsNotFound] when the M7.1.c.b.b row is missing,
	// or any [messenger] sentinel surfaced by the API call).
	SetBotProfile(ctx context.Context, watchkeeperID uuid.UUID, profile messenger.BotProfile) error
}

// BotProfileStepDeps is the construction-time bag wired into
// [NewBotProfileStep]. Held in a struct so a future addition (e.g.
// a manifest-driven profile builder) lands as a new field without
// breaking the constructor signature.
type BotProfileStepDeps struct {
	// Setter is the per-watchkeeper [messenger.Adapter.SetBotProfile]
	// dispatcher. Required; a nil Setter is rejected at construction.
	Setter BotProfileSetter

	// Profile is the [messenger.BotProfile] applied on every saga run.
	// Phase-1 admin-grant flow: a static per-deployment profile (the
	// Watchmaster wiring layer derives it from the seeded
	// `bots/watchmaster.yaml` manifest). M7.x will replace this with
	// a per-saga profile derived from the manifest_version row's
	// personality / language fields. An entirely-empty profile is a
	// documented no-op at the messenger layer; the step still runs
	// (the saga.Runner emits started/completed regardless).
	Profile messenger.BotProfile
}

// BotProfileStep is the [saga.Step] implementation for the
// `bot_profile` step. Construct via [NewBotProfileStep]; the zero
// value is NOT usable.
//
// Concurrency: safe for concurrent use across distinct sagas. Holds
// only immutable configuration; per-call state lives on the goroutine
// stack and on the per-call `context.Context` (which carries the
// [saga.SpawnContext] keying the watchkeeper).
type BotProfileStep struct {
	setter  BotProfileSetter
	profile messenger.BotProfile
}

// Compile-time assertion: [*BotProfileStep] satisfies [saga.Step].
// Pins the integration shape so a future change to the interface
// surface fails the build here.
var _ saga.Step = (*BotProfileStep)(nil)

// NewBotProfileStep constructs a [BotProfileStep] with the supplied
// [BotProfileStepDeps]. Setter is required; a nil value panics with
// a clear message — matches the panic discipline of
// [NewCreateAppStep] and [NewOAuthInstallStep].
//
// An empty [BotProfileStepDeps.Profile] is permitted: the messenger
// adapter contract treats an entirely-empty profile as a no-op
// (returns nil without contacting the platform), so the step
// degrades gracefully when a deployment does not supply a profile.
//
// Profile defensive copy: [BotProfileStepDeps.Profile.Metadata]
// (map) and [BotProfileStepDeps.Profile.AvatarPNG] (byte slice) are
// reference types; if the caller retains and mutates them after
// construction the step's "immutable configuration" claim would
// break under concurrent saga runs and post-construction mutations
// could change what gets sent to Slack. We deep-copy both fields
// here — mirrors the [NewCreateAppStep] `Scopes` defensive-copy
// pattern.
func NewBotProfileStep(deps BotProfileStepDeps) *BotProfileStep {
	if deps.Setter == nil {
		panic("spawn: NewBotProfileStep: deps.Setter must not be nil")
	}
	return &BotProfileStep{
		setter:  deps.Setter,
		profile: cloneBotProfile(deps.Profile),
	}
}

// cloneBotProfile returns a deep copy of `p`: scalar fields copy by
// value, [messenger.BotProfile.Metadata] is duplicated into a fresh
// map, [messenger.BotProfile.AvatarPNG] into a fresh byte slice. A
// nil Metadata stays nil (avoids allocating an empty map when the
// caller did not supply one); a nil AvatarPNG stays nil for the
// same reason.
func cloneBotProfile(p messenger.BotProfile) messenger.BotProfile {
	clone := messenger.BotProfile{
		DisplayName: p.DisplayName,
		StatusText:  p.StatusText,
	}
	if p.AvatarPNG != nil {
		clone.AvatarPNG = append([]byte(nil), p.AvatarPNG...)
	}
	if p.Metadata != nil {
		clone.Metadata = make(map[string]string, len(p.Metadata))
		for k, v := range p.Metadata {
			clone.Metadata[k] = v
		}
	}
	return clone
}

// Name satisfies [saga.Step.Name]. Returns the stable closed-set
// identifier `bot_profile`. The runner uses it as the `current_step`
// DAO column and as the `step_name` audit payload key.
func (s *BotProfileStep) Name() string {
	return BotProfileStepName
}

// Execute satisfies [saga.Step.Execute].
//
// Resolution order:
//
//  1. Read the [saga.SpawnContext] off `ctx`. A miss returns a
//     wrapped [ErrMissingSpawnContext]; the Setter is NOT touched.
//  2. Validate the SpawnContext's AgentID is non-zero (uuid.Nil
//     cannot be a credential-store key). A miss returns a wrapped
//     [ErrMissingAgentID]; the Setter is NOT touched.
//  3. Dispatch through the [BotProfileSetter] seam, forwarding the
//     watchkeeperID + the construction-time profile.
//
// Errors are wrapped with `fmt.Errorf("spawn: bot_profile step:
// %w", err)` so a caller's `errors.Is` against the underlying
// sentinel still matches.
//
// Audit discipline: this method does NOT call
// [keeperslog.Writer.Append] (AC7). The audit chain belongs to the
// saga core.
func (s *BotProfileStep) Execute(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("spawn: bot_profile step: %w", err)
	}

	sc, ok := saga.SpawnContextFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: bot_profile step: %w", ErrMissingSpawnContext)
	}
	if sc.AgentID == uuid.Nil {
		return fmt.Errorf("spawn: bot_profile step: %w", ErrMissingAgentID)
	}

	if err := s.setter.SetBotProfile(ctx, sc.AgentID, cloneBotProfile(s.profile)); err != nil {
		return fmt.Errorf("spawn: bot_profile step: %w", err)
	}
	return nil
}
