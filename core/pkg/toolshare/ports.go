package toolshare

import (
	"context"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/github"
	"github.com/vadimtrunov/watchkeepers/core/pkg/localpatch"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// FS is the filesystem seam consumed by [Sharer.Share]. Aliased
// to [localpatch.FS] for one source of truth across M9 packages
// that walk on-disk tool trees.
type FS = localpatch.FS

// Publisher is the [eventbus.Bus] subset [Sharer.Share] consumes.
// Mirror `localpatch.Publisher` / `hostedexport.Publisher` /
// `toolregistry.Publisher`.
type Publisher interface {
	Publish(ctx context.Context, topic string, event any) error
}

// Clock is the time seam.
type Clock interface {
	Now() time.Time
}

// ClockFunc adapts a plain `func() time.Time` to [Clock].
type ClockFunc func() time.Time

// Now implements [Clock].
func (f ClockFunc) Now() time.Time { return f() }

// Logger is the optional structured-log seam. PII discipline:
// implementations MUST NOT include the operator-supplied reason,
// tool source bytes, the GitHub token, or the Slack channel id.
type Logger interface {
	Log(ctx context.Context, msg string, kv ...any)
}

// SourceLookup is the per-call seam for resolving a configured
// [toolregistry.SourceConfig] by name. Identical contract to
// `localpatch.SourceLookup` / `hostedexport.SourceLookup`.
//
// Contract:
//
//   - Return the resolved [toolregistry.SourceConfig] on success.
//   - Return [ErrUnknownSource] (or any wrapping of it) when the
//     source name does not resolve.
//   - This package does NOT gate on source kind (share applies to
//     any source the deployment has configured).
type SourceLookup func(ctx context.Context, sourceName string) (toolregistry.SourceConfig, error)

// ProposerIdentityResolver is the per-call seam for resolving the
// proposer identity. Mirror `localpatch.OperatorIdentityResolver`
// / `hostedexport.OperatorIdentityResolver`.
//
// Contract:
//
//   - Return a non-empty identifier on success.
//   - Return `("", <cause>)` on resolution failure;
//     [Sharer.Share] wraps with [ErrIdentityResolution].
//   - Returning `("", nil)` is a programmer error caught as
//     [ErrEmptyResolvedIdentity].
type ProposerIdentityResolver func(ctx context.Context, hint string) (string, error)

// TargetRepoResolver is the per-call seam for mapping a
// [ShareRequest] to a concrete target repo + base branch +
// closed-set [TargetSource] discriminator. Production wiring
// reads the deployment's configured platform-shared repo and
// the per-Watchkeeper private-repo binding; tests substitute a
// constant resolver.
//
// Contract:
//
//   - Return a [ResolvedTarget] with non-empty Owner / Repo /
//     Base and a valid [TargetSource] value.
//   - Return `(ResolvedTarget{}, <cause>)` on resolution failure;
//     [Sharer.Share] wraps with [ErrTargetResolution].
//
// The resolver receives the full [ShareRequest] so a target
// policy can branch on the request's [TargetSource] hint.
type TargetRepoResolver func(ctx context.Context, req ShareRequest) (ResolvedTarget, error)

// GitHubClient is the subset of `*github.Client` [Sharer.Share]
// consumes. Defined locally so tests substitute a hand-rolled
// fake and production wiring forwards to a real
// [github.NewClient]-built instance.
//
// Method-level NOTE on token-source ownership: the underlying
// [github.Client] is constructed with a per-call [github.TokenSource]
// closing over the deployment's PAT or GitHub-App credential. The
// orchestrator does NOT own credential lifecycle; rotation flows
// through the TokenSource's underlying secrets backend.
type GitHubClient interface {
	GetRef(ctx context.Context, owner github.RepoOwner, repo github.RepoName, ref string) (github.GetRefResult, error)
	CreateRef(ctx context.Context, opts github.CreateRefOptions) (github.CreateRefResult, error)
	CreateOrUpdateFile(ctx context.Context, opts github.CreateOrUpdateFileOptions) (github.CreateOrUpdateFileResult, error)
	CreatePullRequest(ctx context.Context, opts github.CreatePullRequestOptions) (github.CreatePullRequestResult, error)
}

// SlackNotifier is the subset of `*slack.Client` [Sharer.Share]
// consumes for the lead DM. Optional — a deployment that skips
// the Slack DM passes `nil` (see [SharerDeps.SlackNotifier]).
//
// The orchestrator opens an IM channel with the lead user id
// (returned by [LeadResolver]) and sends a single message
// pointing at the just-opened PR. A Slack outage is logged but
// not surfaced as a [Sharer.Share] error — the PR open IS the
// durable outcome.
type SlackNotifier interface {
	// OpenIMChannel resolves the channel id for a 1-1 DM with
	// the given user id.
	OpenIMChannel(ctx context.Context, userID string) (string, error)

	// SendDMText sends a plain-text message to the channel id.
	// The two-step (open-then-send) shape mirrors the M7
	// messenger/slack adapter; the orchestrator stays decoupled
	// from the messenger.Message envelope shape.
	SendDMText(ctx context.Context, channelID, text string) error
}

// LeadResolver is the per-call seam for resolving the Slack user
// id of the lead Watchkeeper for a given source / tool / target
// triple. Production wiring closes over the per-Watchkeeper
// admin config; tests substitute a constant resolver.
//
// Contract:
//
//   - Return a non-empty Slack user id on success (e.g. `U02ABC123`).
//   - Return `("", nil)` to signal "no lead configured; skip the
//     DM" (orchestrator suppresses the Slack call without error).
//   - Return `("", <cause>)` on resolution failure; the
//     orchestrator logs the failure and skips the DM (DM is best-
//     effort; never fatal).
type LeadResolver func(ctx context.Context, target ResolvedTarget, req ShareRequest) (string, error)
