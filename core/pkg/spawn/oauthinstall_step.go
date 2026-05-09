// oauthinstall_step.go is the M7.1.c.b.b saga.Step implementation that
// invokes the M4.2.d.2 [slack.Client.InstallApp] surface during the
// Watchkeeper spawn flow. The step:
//
//  1. Reads the [saga.SpawnContext] off the call's `context.Context` and
//     extracts the watchkeeper id + the operator-supplied [saga.SpawnContext.OAuthCode].
//  2. Resolves `client_id` / `client_secret` from the M7.1.c.a creds DAO
//     keyed by the watchkeeper id (the row must have been created by a
//     prior CreateAppStep run).
//  3. Builds a per-call [slack.InstallParamsResolver] that surfaces the
//     OAuth code + client credentials to the messenger adapter, and a
//     per-call [slack.InstallTokenSink] that encrypts each returned
//     bot/user/refresh token via the injected [secrets.Encrypter] and
//     persists the ciphertexts via the DAO's [WatchkeeperSlackAppCredsDAO.PutInstallTokens]
//     method.
//  4. Dispatches via the configured [SlackAppInstaller] seam, which the
//     production wiring backs with a `*slack.Client` clone carrying the
//     per-call sink + resolver options installed via
//     [slack.WithInstallTokenSink] / [slack.WithInstallParamsResolver].
//
// Audit discipline (AC7): the step does NOT emit any new keepers_log
// event. The underlying [slack.Client.Do] redaction discipline applies
// to the OAuth exchange itself; the saga core ([saga.Runner]) emits
// `saga_step_started` / `saga_step_completed`.
//
// PII discipline (AC7): plaintext tokens, the OAuth code, and the KEK
// material NEVER appear in any returned error string. The wrap chain
// surfaces only sentinel-level diagnostics (e.g. [ErrMissingOAuthCode],
// [ErrCredsNotFound]).
package spawn

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
	slackmessenger "github.com/vadimtrunov/watchkeepers/core/pkg/messenger/slack"
	"github.com/vadimtrunov/watchkeepers/core/pkg/secrets"
	"github.com/vadimtrunov/watchkeepers/core/pkg/spawn/saga"
)

// OAuthInstallStepName is the stable closed-set identifier for the
// OAuthInstall step. Used by the [saga.Runner] as the `current_step`
// DAO column and as the `step_name` audit payload key. Hoisted to a
// constant so a typo at the call site is a compile error.
const OAuthInstallStepName = "oauth_install"

// ErrMissingOAuthCode is the typed error
// [OAuthInstallStep.Execute] returns when the [saga.SpawnContext] is
// present but its [saga.SpawnContext.OAuthCode] field is empty. The
// underlying [SlackAppInstaller] is NOT touched on this path —
// resolution failure is a security boundary; if the code is missing the
// request is never sent. Matchable via [errors.Is].
var ErrMissingOAuthCode = errors.New("spawn: oauth_install step: missing OAuth code")

// SlackAppInstaller is the privileged-RPC seam the OAuthInstall step
// dispatches through. The production wiring wraps a `*slack.Client`
// clone carrying the per-call [slack.WithInstallTokenSink] +
// [slack.WithInstallParamsResolver] options so the underlying
// `oauth.v2.access` exchange surfaces tokens via the supplied sink and
// reads OAuth params from the supplied resolver. Test wiring satisfies
// the interface with a hand-rolled fake (no mocking lib — M3.6 / M6.3.e
// pattern).
//
// Concurrency: implementations MUST be safe for concurrent calls
// across distinct sagas. The production wrapper holds an immutable
// `*slack.Client` reference and builds per-call clones; the test fake
// uses sync primitives to record calls.
type SlackAppInstaller interface {
	// InstallApp invokes Slack's `oauth.v2.access` for `appID` /
	// `workspace`, resolving OAuth params via `resolver` and surfacing
	// the returned tokens via `sink`. Returns the returned
	// [messenger.Installation] (carries only non-secret platform-side
	// identifiers — every raw token rides via `sink`).
	//
	// `resolver` and `sink` MUST both be non-nil — the wrapper rejects
	// nils synchronously via the underlying [slack.ErrInstallParamsUnset]
	// / [slack.ErrInstallTokenSinkUnset] sentinels.
	InstallApp(
		ctx context.Context,
		appID messenger.AppID,
		workspace messenger.WorkspaceRef,
		resolver slackmessenger.InstallParamsResolver,
		sink slackmessenger.InstallTokenSink,
	) (messenger.Installation, error)
}

// OAuthInstallStepDeps is the construction-time bag wired into
// [NewOAuthInstallStep]. Held in a struct so a future addition (e.g.
// a clock for deterministic InstalledAt stamping) lands as a new
// field without breaking the constructor signature.
type OAuthInstallStepDeps struct {
	// Installer is the install-RPC seam. Required; a nil Installer is
	// rejected at construction.
	Installer SlackAppInstaller

	// CredsDAO is the credential-store seam. Required; a nil DAO is
	// rejected at construction. The step looks up `client_id` /
	// `client_secret` by watchkeeperID and persists the install
	// tokens via [WatchkeeperSlackAppCredsDAO.PutInstallTokens].
	CredsDAO WatchkeeperSlackAppCredsDAO

	// Encrypter is the secrets-at-rest seam. Required; a nil
	// Encrypter is rejected at construction. Each non-empty bot / user
	// / refresh token is sealed via [secrets.Encrypter.Encrypt] before
	// the DAO write; an empty refresh_token short-circuits to `nil`
	// (NOT encrypted-empty-string — see the M7.1.c.b.b plan).
	Encrypter secrets.Encrypter

	// Workspace identifies the Slack workspace this install targets.
	// Required; an empty Workspace.ID is rejected at construction. For
	// the Phase-1 admin-grant flow this is the operator's dev
	// workspace (fixed per-deployment); a future M7.x will widen the
	// field to per-saga workspace selection.
	Workspace messenger.WorkspaceRef

	// RedirectURI is the OAuth redirect URI Slack consented to. Forwarded
	// verbatim into [slack.InstallParams.RedirectURI]. Optional — when
	// empty the underlying messenger adapter omits the field from the
	// `oauth.v2.access` request body and Slack falls back to the manifest
	// value (per the M4.2.d.2 doc-block on InstallParams.RedirectURI).
	RedirectURI string
}

// OAuthInstallStep is the [saga.Step] implementation for the
// `oauth_install` step. Construct via [NewOAuthInstallStep]; the zero
// value is NOT usable.
//
// Concurrency: safe for concurrent use across distinct sagas. Holds
// only immutable configuration; per-call state lives on the goroutine
// stack and on the per-call `context.Context` (which carries the
// [saga.SpawnContext] keying the credential row).
type OAuthInstallStep struct {
	installer   SlackAppInstaller
	credsDAO    WatchkeeperSlackAppCredsDAO
	encrypter   secrets.Encrypter
	workspace   messenger.WorkspaceRef
	redirectURI string
}

// Compile-time assertion: [*OAuthInstallStep] satisfies [saga.Step]
// (AC2). Pins the integration shape so a future change to the
// interface surface fails the build here.
var _ saga.Step = (*OAuthInstallStep)(nil)

// NewOAuthInstallStep constructs an [OAuthInstallStep] with the supplied
// [OAuthInstallStepDeps]. Installer, CredsDAO, Encrypter, and a
// non-empty Workspace.ID are required; a nil/empty value for any of
// them panics with a clear message — matches the panic discipline of
// [NewCreateAppStep] and [NewSlackAppRPC].
func NewOAuthInstallStep(deps OAuthInstallStepDeps) *OAuthInstallStep {
	if deps.Installer == nil {
		panic("spawn: NewOAuthInstallStep: deps.Installer must not be nil")
	}
	if deps.CredsDAO == nil {
		panic("spawn: NewOAuthInstallStep: deps.CredsDAO must not be nil")
	}
	if deps.Encrypter == nil {
		panic("spawn: NewOAuthInstallStep: deps.Encrypter must not be nil")
	}
	if deps.Workspace.ID == "" {
		panic("spawn: NewOAuthInstallStep: deps.Workspace.ID must not be empty")
	}
	return &OAuthInstallStep{
		installer:   deps.Installer,
		credsDAO:    deps.CredsDAO,
		encrypter:   deps.Encrypter,
		workspace:   deps.Workspace,
		redirectURI: deps.RedirectURI,
	}
}

// Name satisfies [saga.Step.Name]. Returns the stable closed-set
// identifier `oauth_install`. The runner uses it as the `current_step`
// DAO column and as the `step_name` audit payload key.
func (s *OAuthInstallStep) Name() string {
	return OAuthInstallStepName
}

// Execute satisfies [saga.Step.Execute].
//
// Resolution order:
//
//  1. Read the [saga.SpawnContext] off `ctx`. A miss returns a
//     wrapped [ErrMissingSpawnContext]; the installer is NOT touched.
//  2. Validate the SpawnContext's AgentID is non-zero (uuid.Nil
//     cannot be a credential-store key). A miss returns a wrapped
//     [ErrMissingAgentID]; the installer is NOT touched.
//  3. Validate the SpawnContext's OAuthCode is non-empty. A miss
//     returns a wrapped [ErrMissingOAuthCode]; the installer is NOT
//     touched (resolution failure is a security boundary).
//  4. Resolve `client_id` / `client_secret` / `app_id` from the creds
//     DAO. A miss surfaces wrapped [ErrCredsNotFound]; the installer
//     is NOT touched.
//  5. Dispatch through the [SlackAppInstaller] seam with a per-call
//     resolver (returns the operator-supplied OAuth code + the DAO-
//     stored client credentials) and a per-call sink (encrypts each
//     non-empty token via the [secrets.Encrypter] and persists the
//     ciphertexts via [WatchkeeperSlackAppCredsDAO.PutInstallTokens]).
//
// Errors are wrapped with `fmt.Errorf("spawn: oauth_install step:
// %w", err)` so a caller's `errors.Is` against the underlying sentinel
// (e.g. [ErrCredsNotFound], [ErrMissingOAuthCode]) still matches.
//
// Audit discipline: this method does NOT call
// [keeperslog.Writer.Append] (AC7). The audit chain belongs to the
// saga core.
func (s *OAuthInstallStep) Execute(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("spawn: oauth_install step: %w", err)
	}

	sc, ok := saga.SpawnContextFromContext(ctx)
	if !ok {
		return fmt.Errorf("spawn: oauth_install step: %w", ErrMissingSpawnContext)
	}
	if sc.AgentID == uuid.Nil {
		return fmt.Errorf("spawn: oauth_install step: %w", ErrMissingAgentID)
	}
	if sc.OAuthCode == "" {
		return fmt.Errorf("spawn: oauth_install step: %w", ErrMissingOAuthCode)
	}

	watchkeeperID := sc.AgentID
	creds, err := s.credsDAO.Get(ctx, watchkeeperID)
	if err != nil {
		return fmt.Errorf("spawn: oauth_install step: %w", err)
	}

	// Per-call resolver: surfaces the operator-supplied OAuth code +
	// the DAO-stored client credentials to the underlying messenger
	// adapter. Closes over the SpawnContext-derived OAuthCode and the
	// DAO-resolved creds. The resolver runs INSIDE the install call
	// so it inherits ctx cancellation.
	resolver := slackmessenger.InstallParamsResolver(
		func(_ context.Context, _ messenger.AppID, _ messenger.WorkspaceRef) (slackmessenger.InstallParams, error) {
			return slackmessenger.InstallParams{
				Code:         sc.OAuthCode,
				ClientID:     creds.ClientID,
				ClientSecret: creds.ClientSecret,
				RedirectURI:  s.redirectURI,
			}, nil
		},
	)

	// Per-call sink: encrypts each non-empty token via the configured
	// [secrets.Encrypter] and persists the ciphertexts via the DAO's
	// PutInstallTokens method. Captures the watchkeeperID from the
	// SpawnContext (NOT from `tokens.AppID` — appID is Slack's,
	// watchkeeperID is the saga's primary key). A non-nil sink return
	// surfaces back up the installer's wrap chain; we capture it for
	// preferred-error semantics consistent with M7.1.c.a.
	sinkErr := error(nil)
	sink := slackmessenger.InstallTokenSink(
		func(sinkCtx context.Context, tokens slackmessenger.InstallTokens) error {
			botCT, err := encryptIfNonEmpty(sinkCtx, s.encrypter, tokens.AccessToken)
			if err != nil {
				sinkErr = err
				return err
			}
			userCT, err := encryptIfNonEmpty(sinkCtx, s.encrypter, tokens.UserAccessToken)
			if err != nil {
				sinkErr = err
				return err
			}
			refreshCT, err := encryptIfNonEmpty(sinkCtx, s.encrypter, tokens.RefreshToken)
			if err != nil {
				sinkErr = err
				return err
			}
			expiresAt := expiryFromExpiresIn(tokens.ExpiresIn)
			if err := s.credsDAO.PutInstallTokens(
				sinkCtx, watchkeeperID, botCT, userCT, refreshCT, expiresAt,
			); err != nil {
				sinkErr = err
				return err
			}
			return nil
		},
	)

	if _, err := s.installer.InstallApp(ctx, creds.AppID, s.workspace, resolver, sink); err != nil {
		// Prefer the sink error when both fired — sink errors carry
		// the DAO / Encrypter sentinel callers branch on (e.g.
		// [ErrCredsNotFound]); the installer's wrap chain already
		// embeds the sink error verbatim, so `errors.Is` succeeds
		// either way. Returning the sink error verbatim keeps the
		// wrap chain shallow.
		if sinkErr != nil {
			return fmt.Errorf("spawn: oauth_install step: %w", sinkErr)
		}
		return fmt.Errorf("spawn: oauth_install step: %w", err)
	}

	return nil
}

// encryptIfNonEmpty seals `plaintext` via `enc` when non-empty; returns
// `nil` for the zero-length case. The empty-input short-circuit is
// load-bearing: encrypting an empty plaintext produces a 28-byte
// ciphertext (12-byte nonce + 16-byte tag) which would silently
// disagree with downstream `len() == 0` callers (the M7.1.c.b.b plan
// pins this on the test plan as the "RefreshToken empty → stored CT
// is nil or len==0" edge case).
func encryptIfNonEmpty(ctx context.Context, enc secrets.Encrypter, plaintext string) ([]byte, error) {
	if plaintext == "" {
		return nil, nil
	}
	return enc.Encrypt(ctx, []byte(plaintext))
}

// expiryFromExpiresIn maps the Slack `oauth.v2.access` `expires_in`
// (seconds) to a UTC absolute expiry time. Returns the zero
// [time.Time] when `expiresIn` is non-positive — the documented
// sentinel for "no expiry" (rotation disabled on the app manifest).
// We stamp the expiry at the OAuthInstall step layer so the DAO does
// not need a clock dependency; the slight skew vs Slack's server-side
// clock is irrelevant given the multi-hour token lifetimes
// (`expires_in` is typically 43200 seconds = 12 hours).
func expiryFromExpiresIn(expiresIn int) time.Time {
	if expiresIn <= 0 {
		return time.Time{}
	}
	return time.Now().UTC().Add(time.Duration(expiresIn) * time.Second)
}
