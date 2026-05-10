// createapp_step.go is the M7.1.c.a saga.Step implementation that
// invokes the existing [SlackAppRPC.CreateApp] privileged RPC during
// the Watchkeeper spawn flow. The step composes a [CreateAppRequest]
// from the saga's [saga.SpawnContext] (manifest_version_id, agent_id,
// claim), dispatches to the RPC, and persists the returned AppID
// plus the OUT-OF-BAND credential bundle (`client_id`,
// `client_secret`, `signing_secret`, `verification_token`) via a
// [WatchkeeperSlackAppCredsDAO].
//
// Wire-up note: the credentials bundle does NOT ride back through
// the [SlackAppRPC.CreateApp] return value. The underlying
// `apps.manifest.create` Slack call routes the credentials
// out-of-band through a [slack.CreateAppCredsSink] callback (M4.2.d
// design). This step installs its own sink that bridges to the
// configured DAO; the sink closes over the watchkeeper id read from
// the SpawnContext (NOT over `creds.AppID` — the watchkeeper id is
// the stable saga-row id; the Slack-assigned app id can change
// across re-create scenarios).
//
// Audit discipline (AC7): the step does NOT emit any new
// keepers_log event. The underlying [SlackAppRPC.CreateApp] already
// emits `watchmaster_slack_app_create_*` per M6.1.b, and the saga
// core ([saga.Runner]) emits `saga_step_started` /
// `saga_step_completed`. Adding a third audit row here would
// double-emit the same external action.
//
// # M7.3.c rollback contract — [Compensate]
//
// On saga rollback (a later step's [saga.Step.Execute] returned non-
// nil) the [saga.Runner] dispatches [CreateAppStep.Compensate] in
// REVERSE forward order. Compensate sources `watchkeeperID` from the
// [saga.SpawnContext] on `ctx` (NEVER receiver-stash — the M7.3.b
// per-saga state contract forbids it; multiple sagas may share a
// step instance) and dispatches via the [SlackAppTeardown] seam.
//
// The production wiring for the [SlackAppTeardown] seam is NOT
// shipped in M7.3.c — the seam interface lands here so the rollback
// chain compiles end-to-end; the wrapper itself is deferred to a
// future M7.3.d-or-M7.4 reconciler (Phase 1: no Slack `apps.delete`
// API exists; the wrapper-side strategy is best-effort local-state
// wipe via [WatchkeeperSlackAppCredsDAO.WipeInstallTokens] + an
// orphan-app_id reconciler queue an operator can drain manually).
// Returns typed errors classed via [saga.LastErrorClassed] so the
// audit row's `last_error_class` pins the failure mode.
//
// PII discipline on the rollback path: no creds substring, no
// `app_id` raw value, and no Slack response body lands on the
// returned error string. The wrap chain surfaces only the step-
// prefix + the underlying typed sentinel (e.g.
// [ErrMissingSpawnContext], [ErrMissingAgentID], or the Teardown's
// own typed error).
//
// # Failed-step partial-success surface — deferred to M7.3.d-or-M7.4
//
// The M7.3.b runner does NOT dispatch [Compensate] on a step whose
// [Execute] returned non-nil. The `apps.manifest.create` Slack call
// is the side effect AND it precedes any client-side fail-fast we
// could do, so a sink-failure path returns non-nil from [Execute]
// AFTER the platform mutated. Today the orphaned Slack App
// survives that path; recovery is deferred to a future
// M7.3.d-or-M7.4 reconciler (widened seam signature taking the
// in-process app_id, OR a sweep of `slack_app_creds` rows for
// orphan ids without companion install-tokens). See
// docs/lessons/M7.md M7.3.c iter-1 patterns #1.
package spawn

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	slackmessenger "github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// CreateAppStepName is the stable closed-set identifier for the
// CreateApp step. Used by the [saga.Runner] as the `current_step`
// DAO column and as the `step_name` audit payload key. Hoisted to a
// constant so a typo at the call site is a compile error.
const CreateAppStepName = "create_app"

// ErrMissingSpawnContext is the typed error
// [CreateAppStep.Execute] returns when the supplied `context.Context`
// does not carry a [saga.SpawnContext] value. Matchable via
// [errors.Is]; the step wraps it with `spawn: create_app step:` so
// the wrap chain stays uniform with the other failure paths.
var ErrMissingSpawnContext = errors.New("spawn: create_app step: missing SpawnContext")

// ErrMissingManifestVersion is the typed error
// [CreateAppStep.Execute] returns when the [saga.SpawnContext] is
// present but its [saga.SpawnContext.ManifestVersionID] is the zero
// uuid. The RPC is NOT touched on this path. Matchable via
// [errors.Is].
var ErrMissingManifestVersion = errors.New("spawn: create_app step: missing manifest_version_id")

// ErrMissingAgentID is the typed error [CreateAppStep.Execute]
// returns when the [saga.SpawnContext] is present but its
// [saga.SpawnContext.AgentID] is the zero uuid. The RPC is NOT
// touched on this path. Matchable via [errors.Is].
var ErrMissingAgentID = errors.New("spawn: create_app step: missing agent_id")

// CreateAppStepDeps is the construction-time bag wired into
// [NewCreateAppStep]. Held in a struct so a future addition (e.g. a
// clock, a tracer, a manifest reader) lands as a new field without
// breaking the constructor signature.
type CreateAppStepDeps struct {
	// RPC is the privileged-RPC seam. Required; a nil RPC is
	// rejected at construction with a clear panic message — a step
	// with no RPC cannot do anything useful and silently no-oping
	// every call would mask the bug.
	RPC SlackAppRPC

	// CredsDAO is the credential-store seam. Required; a nil DAO is
	// rejected at construction. The step's installed sink bridges
	// every successful RPC's OUT-OF-BAND credentials bundle to
	// [WatchkeeperSlackAppCredsDAO.Put].
	CredsDAO WatchkeeperSlackAppCredsDAO

	// Teardown is the M7.3.c rollback seam dispatched by
	// [CreateAppStep.Compensate] when a later saga step fails and
	// the runner walks the rollback chain in reverse. Required; a
	// nil Teardown is rejected at construction — a step that
	// satisfies [saga.Compensator] but cannot actually compensate
	// is a wiring bug that surfaces best at boot, not on the first
	// rollback (which would otherwise fall onto the runner's
	// `safeCompensate` panic harness). The seam closes over the
	// watchkeeperID supplied to [SlackAppTeardown.TeardownApp]; per-
	// saga state lives on the [saga.SpawnContext] read from `ctx`,
	// NEVER on the step receiver (M7.3.b lesson #1).
	Teardown SlackAppTeardown

	// AppName is the Slack app's display name forwarded to
	// [CreateAppRequest.AppName]. Required; empty values fail at
	// construction so a misconfigured wiring layer is caught at
	// boot, not on the first saga run. The step does NOT read the
	// manifest_version row to derive the app name in M7.1.c.a — the
	// caller injects a stable display name; richer per-manifest
	// derivation lands with M7.1.c.b/c.
	AppName string

	// AppDescription is the optional Slack app long-form description
	// forwarded to [CreateAppRequest.AppDescription]. Empty is
	// allowed.
	AppDescription string

	// Scopes is the list of OAuth bot scopes forwarded to
	// [CreateAppRequest.Scopes]. Empty produces an app with no bot
	// scopes; richer per-manifest derivation lands with M7.1.c.b/c.
	Scopes []string

	// ApprovalToken is the opaque token the M6.3.b approval saga
	// minted for this spawn. Required; empty values would surface
	// from the RPC as [ErrApprovalRequired] anyway, but catching it
	// at construction avoids burning a tier-2 rate-limit token.
	ApprovalToken string
}

// CreateAppStep is the [saga.Step] implementation for the
// `create_app` step. Construct via [NewCreateAppStep]; the zero
// value is NOT usable.
//
// Concurrency: safe for concurrent use across distinct sagas. Holds
// only immutable configuration; per-call state lives on the
// goroutine stack and on the per-call `context.Context` (which
// carries the [saga.SpawnContext] keying the credential row).
type CreateAppStep struct {
	rpc            SlackAppRPC
	credsDAO       WatchkeeperSlackAppCredsDAO
	teardown       SlackAppTeardown
	appName        string
	appDescription string
	scopes         []string
	approvalToken  string
}

// Compile-time assertion: [*CreateAppStep] satisfies [saga.Step]
// (AC2). Pins the integration shape so a future change to the
// interface surface fails the build here.
var _ saga.Step = (*CreateAppStep)(nil)

// Compile-time assertion: [*CreateAppStep] satisfies [saga.Compensator]
// (M7.3.c). Pins the rollback contract — a future signature change to
// [saga.Compensator] surfaces here.
var _ saga.Compensator = (*CreateAppStep)(nil)

// NewCreateAppStep constructs a [CreateAppStep] with the supplied
// [CreateAppStepDeps]. RPC, CredsDAO, AppName, and ApprovalToken are
// required; a nil/empty value for any of them panics with a clear
// message — matches the panic discipline of [NewSlackAppRPC] and
// [NewSpawnKickoffer].
func NewCreateAppStep(deps CreateAppStepDeps) *CreateAppStep {
	if deps.RPC == nil {
		panic("spawn: NewCreateAppStep: deps.RPC must not be nil")
	}
	if deps.CredsDAO == nil {
		panic("spawn: NewCreateAppStep: deps.CredsDAO must not be nil")
	}
	if deps.Teardown == nil {
		panic("spawn: NewCreateAppStep: deps.Teardown must not be nil")
	}
	if deps.AppName == "" {
		panic("spawn: NewCreateAppStep: deps.AppName must not be empty")
	}
	if deps.ApprovalToken == "" {
		panic("spawn: NewCreateAppStep: deps.ApprovalToken must not be empty")
	}
	scopes := append([]string(nil), deps.Scopes...)
	return &CreateAppStep{
		rpc:            deps.RPC,
		credsDAO:       deps.CredsDAO,
		teardown:       deps.Teardown,
		appName:        deps.AppName,
		appDescription: deps.AppDescription,
		scopes:         scopes,
		approvalToken:  deps.ApprovalToken,
	}
}

// Name satisfies [saga.Step.Name]. Returns the stable closed-set
// identifier `create_app`. The runner uses it as the `current_step`
// DAO column and as the `step_name` audit payload key.
func (s *CreateAppStep) Name() string {
	return CreateAppStepName
}

// Execute satisfies [saga.Step.Execute].
//
// Resolution order:
//
//  1. Read the [saga.SpawnContext] off `ctx`. A miss returns a
//     wrapped [ErrMissingSpawnContext]; the RPC is NOT touched.
//  2. Validate the SpawnContext's ManifestVersionID + AgentID are
//     non-zero. A miss returns the matching wrapped sentinel; the
//     RPC is NOT touched.
//  3. Build a per-call sink that bridges [slack.CreateAppCredsSink]
//     to [WatchkeeperSlackAppCredsDAO.Put]. The sink closes over the
//     watchkeeper id derived from the SpawnContext (NOT over
//     `creds.AppID` — the watchkeeper id is the stable saga-row id).
//  4. Build a per-call RPC value via [WithCreateAppCredsSink] and
//     dispatch [SlackAppRPC.CreateApp]. The privileged RPC handles
//     the underlying audit chain (`watchmaster_slack_app_create_*`)
//     and the platform call.
//  5. The sink closure surfaces a non-nil error via the wrap chain;
//     the step returns it wrapped. On RPC success the sink already
//     ran (it executes inside the underlying adapter's success
//     branch BEFORE the RPC return), so an RPC-success +
//     sink-error path surfaces as a wrapped error here.
//
// Errors are wrapped with `fmt.Errorf("spawn: create_app step:
// %w", err)` so a caller's `errors.Is` against the underlying
// sentinel (e.g. [ErrUnauthorized], [ErrCredsAlreadyStored]) still
// matches.
//
// Audit discipline: this method does NOT call
// [keeperslog.Writer.Append] (AC7). The underlying RPC and the saga
// core own the audit chain.
func (s *CreateAppStep) Execute(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("spawn: create_app step: %w", err)
	}

	sc, ok := saga.SpawnContextFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: create_app step: %w", ErrMissingSpawnContext)
	}
	if sc.ManifestVersionID == uuid.Nil {
		return fmt.Errorf("spawn: create_app step: %w", ErrMissingManifestVersion)
	}
	if sc.AgentID == uuid.Nil {
		return fmt.Errorf("spawn: create_app step: %w", ErrMissingAgentID)
	}

	// Sink-to-DAO bridge: closes over the watchkeeper id from the
	// SpawnContext so the DAO row keys off the stable saga id, NOT
	// the Slack-assigned app id (which can change across re-create
	// scenarios). The sink runs inside the underlying messenger
	// adapter's success branch (M4.2.d.2 design); a non-nil return
	// surfaces back up the RPC's wrap chain.
	//
	// M7.3.d in-Execute partial-success cleanup: the sink also
	// captures `creds.AppID` into a local `capturedAppID` so the
	// post-RPC failure branch below can dispatch a best-effort
	// platform-side teardown when the sink errored AFTER the
	// platform call succeeded (the saga.Runner skips Compensate on
	// failed steps per M7.3.b — without in-Execute cleanup the
	// orphaned Slack App would survive).
	//
	// `sinkFired` is the load-bearing "did the platform-side state
	// get created" signal — distinct from `capturedAppID != ""`
	// which would be correct-by-coincidence on the today's Slack
	// contract ("AppID always non-empty on a successful exchange")
	// but would silently skip the cleanup if a future Slack response
	// shape change OR a misbehaving fake supplied an empty AppID
	// after a successful platform call. `sinkFired` is set
	// regardless of the captured value, so the post-RPC failure
	// branch always dispatches when the platform side mutated.
	// Iter-1 critic Minor.
	watchkeeperID := sc.AgentID
	sinkErr := error(nil)
	var capturedAppID messenger.AppID
	var sinkFired bool
	sink := slackmessenger.CreateAppCredsSink(
		func(sinkCtx context.Context, creds slackmessenger.CreateAppCredentials) error {
			sinkFired = true
			capturedAppID = creds.AppID
			if err := s.credsDAO.Put(sinkCtx, watchkeeperID, creds); err != nil {
				// Capture for diagnostic visibility on the
				// returned wrap chain. We NEVER reflect any creds
				// substring on the returned error — only the
				// underlying DAO sentinel rides through.
				sinkErr = err
				return err
			}
			return nil
		},
	)

	rpc := s.rpc
	if installer, ok := rpc.(CreateAppCredsSinkInstaller); ok {
		rpc = installer.WithCreateAppCredsSink(sink)
	}

	req := CreateAppRequest{
		AgentID:        sc.Claim.AgentID,
		AppName:        s.appName,
		AppDescription: s.appDescription,
		Scopes:         append([]string(nil), s.scopes...),
		ApprovalToken:  s.approvalToken,
	}
	claim := Claim{
		OrganizationID:  sc.Claim.OrganizationID,
		AgentID:         sc.Claim.AgentID,
		AuthorityMatrix: sc.Claim.AuthorityMatrix,
	}

	if _, err := rpc.CreateApp(ctx, req, claim); err != nil {
		// M7.3.d in-Execute partial-success cleanup: when the sink
		// captured an AppID, the platform-side app exists even
		// though the local DAO write failed (or the RPC's wrap
		// chain returned an error AFTER the sink ran). Best-effort
		// teardown here so the saga.Runner's failed-step-not-
		// compensated discipline (M7.3.b) does NOT leak the
		// orphaned platform artefact.
		//
		// The cleanup is dispatched under [context.WithoutCancel]
		// (mirrors the M7.3.b iter-1 #2 saga.compensate discipline
		// in saga.go): the most likely trigger for the sink-failure
		// path is a request-bound parent ctx that fired Cancel mid-
		// saga (HTTP timeout, operator-initiated abort, dispatcher
		// tear-down). Passing the cancelled parent here would make
		// the in-Execute cleanup uniformly fail on exactly the
		// scenario this branch is designed to defend against,
		// silently leaking the orphaned Slack App. The cleanup ctx
		// inherits the parent's deadline + values but does NOT
		// propagate the parent's cancellation; per-call timeouts
		// belong to the seam impl.
		//
		// The teardown error is SILENTLY discarded — the operator's
		// load-bearing signal is the original sink/RPC error; an
		// additional teardown failure is a pure observability loss
		// handled by the production wrapper's own diagnostic sink.
		if sinkFired {
			cleanupCtx := context.WithoutCancel(ctx)
			if cleanupErr := s.teardown.TeardownApp(cleanupCtx, sc.AgentID, capturedAppID); cleanupErr != nil {
				// Best-effort discard at the return-value layer
				// (the original sink/RPC error is the operator's
				// load-bearing signal — see iter-3 #3 lesson)
				// PLUS a structured-log emit at the call boundary
				// so an ops investigation has a discoverable
				// signal that the in-Execute teardown was
				// attempted and dropped. Mirrors the M7.3.b
				// kickoffer rejection-emit slog.WarnContext
				// pattern in spawnkickoff.go. Iter-1 critic
				// Minor: the silent discard otherwise leaves NO
				// observability surface for the cleanup attempt.
				slog.WarnContext(
					cleanupCtx, "spawn: create_app step: in-Execute teardown failed",
					"watchkeeper_id", sc.AgentID.String(),
					"err_class", "create_app_in_execute_teardown_dropped",
				)
			}
		}
		// Prefer the sink error when both fired — sink errors carry
		// the DAO sentinel callers branch on (e.g.
		// [ErrCredsAlreadyStored]); the RPC's wrap chain already
		// embeds the sink error verbatim, so `errors.Is` succeeds
		// either way. Returning the sink error verbatim keeps the
		// wrap chain shallow.
		if sinkErr != nil {
			return fmt.Errorf("spawn: create_app step: %w", sinkErr)
		}
		return fmt.Errorf("spawn: create_app step: %w", err)
	}

	return nil
}

// CreateAppCredsSinkInstaller is the optional capability a
// [SlackAppRPC] may expose so the [CreateAppStep] can inject a
// per-call [slack.CreateAppCredsSink] without rebuilding the
// underlying messenger.Client. Production wiring satisfies the
// capability via a thin wrapper that returns a per-call clone of
// the RPC carrying the sink-installed adapter; tests satisfy it by
// recording the installed sink for direct invocation.
//
// The capability is optional because the step's contract MUST
// continue to compile against the bare [SlackAppRPC] interface for
// callers who pre-wire a sink at adapter construction time. When the
// RPC does not implement this capability, the step falls back to
// invoking the bare RPC and trusts the caller to have wired a sink
// up the stack.
type CreateAppCredsSinkInstaller interface {
	// WithCreateAppCredsSink returns a new [SlackAppRPC] value whose
	// underlying messenger.Client carries `sink` installed via
	// [slack.WithCreateAppCredsSink]. The returned RPC is safe for
	// single-call use; the step does not retain it across calls.
	WithCreateAppCredsSink(sink slackmessenger.CreateAppCredsSink) SlackAppRPC
}

// SlackAppTeardown is the M7.3.c rollback seam the
// [CreateAppStep.Compensate] dispatches through. Implementations
// undo the externally-visible side effect produced by
// [CreateAppStep.Execute]: the freshly-minted Slack App + the
// `slack_app_creds` row + any partially-stored install tokens
// keyed by the supplied watchkeeper id.
//
// Phase-1 reality check: Slack does NOT expose a public
// `apps.delete` API for the `apps.manifest.create`-flow surface,
// so the future production wrapper (deferred to M7.3.d-or-M7.4
// per the file-level rollback-contract section above) performs
// the best-effort teardown the platform allows (typically: wipe
// the local `slack_app_creds` row + record the abandoned
// `app_id` to a reconciler queue an operator can drain manually).
// The seam exists so the future migration to a richer
// platform-side teardown lands without churning the step's
// signature.
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct sagas. The production wrapper holds an immutable
// reference to the DAO + secrets seams; the test fake uses sync
// primitives to record calls.
//
// Idempotency: implementations MUST be safe to call MORE than once
// with the same `watchkeeperID`. The [saga.Runner]'s rollback
// chain is best-effort (M7.3.b discipline) — a transient operator
// retry of the same failed saga would dispatch Compensate twice;
// the second call MUST NOT panic on missing rows or surface
// `ErrCredsNotFound` as a hard failure.
//
// Typed-error contract for `last_error_class`: errors returned
// SHOULD implement [saga.LastErrorClassed] to override the default
// `step_compensate_error` sentinel emitted on the
// `saga_compensation_failed` audit row (e.g.
// `slack_app_teardown_unauthorized`,
// `slack_app_teardown_not_found`). The wrap chain in
// [CreateAppStep.Compensate] preserves the underlying chain so
// `errors.As` walks through.
type SlackAppTeardown interface {
	// TeardownApp undoes the side effect of a prior
	// [CreateAppStep.Execute] for `watchkeeperID`. Returns nil on
	// successful teardown; returns a typed error chain on failure.
	// MUST be idempotent (see type-level discipline above).
	//
	// `knownAppID` is the platform-assigned app id captured
	// in-process by the M7.3.d in-Execute partial-success cleanup
	// path (CreateAppStep.Execute on the post-RPC failure branch
	// where the Slack `apps.manifest.create` call succeeded but the
	// sink/DAO write failed; the app exists platform-side but no
	// `slack_app_creds` row was persisted, so a DAO lookup would
	// return [ErrCredsNotFound]). When non-empty, the seam impl
	// uses it directly for the platform-side teardown call. When
	// empty (the M7.3.c rollback-path-via-Compensate dispatch),
	// the seam impl falls back to a DAO lookup keyed by
	// `watchkeeperID` to resolve the app id from the persisted
	// `slack_app_creds` row.
	TeardownApp(ctx context.Context, watchkeeperID uuid.UUID, knownAppID messenger.AppID) error
}

// Compensate satisfies [saga.Compensator].
//
// Resolution order:
//
//  1. Read the [saga.SpawnContext] off `ctx`. A miss returns a
//     wrapped [ErrMissingSpawnContext]; the [SlackAppTeardown] is
//     NOT touched. The [saga.Runner] dispatches Compensate under
//     [context.WithoutCancel] (M7.3.b iter-1 #2) so a parent
//     cancellation does NOT poison the rollback walk.
//  2. Validate the SpawnContext's AgentID is non-zero (uuid.Nil
//     cannot be a credential-store key). A miss returns a wrapped
//     [ErrMissingAgentID]; the [SlackAppTeardown] is NOT touched.
//  3. Dispatch through the [SlackAppTeardown] seam, forwarding the
//     watchkeeperID. The seam owns the actual rollback (Slack-side
//     uninstall best-effort + DAO wipe).
//
// Errors are wrapped with `fmt.Errorf("spawn: create_app step
// compensate: %w", err)` so a caller's `errors.Is` against the
// underlying sentinel still matches AND the saga audit row's
// `last_error_class` resolver picks the deepest
// [saga.LastErrorClassed] in the chain via `errors.As` (M7.3.b
// resolver discipline).
//
// Audit discipline: this method does NOT call
// [keeperslog.Writer.Append] (AC7). The audit chain belongs to the
// saga core; per-step `saga_step_compensated` /
// `saga_compensation_failed` rows are emitted by the [saga.Runner]
// based on the returned error.
//
// PII discipline: NEVER reflects creds, raw `app_id`, or Slack
// response substrings into the returned error string. Only the
// typed sentinel chain rides through.
func (s *CreateAppStep) Compensate(ctx context.Context) error {
	// No `ctx.Err()` short-circuit here (asymmetric vs Execute) —
	// the saga.Runner dispatches Compensate under
	// context.WithoutCancel (M7.3.b iter-1 #2; see saga.compensate),
	// so the parent ctx's cancellation never reaches this body. The
	// asymmetry is intentional; every Compensate in this package
	// follows the same shape.
	sc, ok := saga.SpawnContextFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: create_app step compensate: %w", ErrMissingSpawnContext)
	}
	if sc.AgentID == uuid.Nil {
		return fmt.Errorf("spawn: create_app step compensate: %w", ErrMissingAgentID)
	}
	// Compensate path: pass empty knownAppID — the seam impl falls
	// back to a DAO lookup keyed by watchkeeperID. The in-Execute
	// partial-success cleanup path (Execute body) supplies the
	// captured platform-side app id directly; see the M7.3.d
	// "in-Execute partial-success cleanup" wiring below.
	if err := s.teardown.TeardownApp(ctx, sc.AgentID, ""); err != nil {
		return fmt.Errorf("spawn: create_app step compensate: %w", err)
	}
	return nil
}
