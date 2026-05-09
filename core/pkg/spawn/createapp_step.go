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
package spawn

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

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
	appName        string
	appDescription string
	scopes         []string
	approvalToken  string
}

// Compile-time assertion: [*CreateAppStep] satisfies [saga.Step]
// (AC2). Pins the integration shape so a future change to the
// interface surface fails the build here.
var _ saga.Step = (*CreateAppStep)(nil)

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
	watchkeeperID := sc.AgentID
	sinkErr := error(nil)
	sink := slackmessenger.CreateAppCredsSink(
		func(sinkCtx context.Context, creds slackmessenger.CreateAppCredentials) error {
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
