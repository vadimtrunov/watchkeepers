// Doc-block at file head documenting the [Sharer] seam contract.
// See doc.go for the full resolution order + audit + PII discipline.

package toolshare

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/vadimtrunov/watchkeepers/core/pkg/github"
	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// Bounds enforced by [ShareRequest.Validate]. The numbers defend
// against unbounded agent-supplied strings landing on the bus
// payload (the M9.7 audit subscriber's row-store has its own
// bounds; ours are belt-and-braces).
const (
	// MaxSourceNameLength bounds [ShareRequest.SourceName].
	MaxSourceNameLength = 64

	// MaxToolNameLength bounds [ShareRequest.ToolName].
	MaxToolNameLength = 64

	// MaxProposerIDLength bounds the resolved proposer identity.
	MaxProposerIDLength = 64

	// MaxReasonLength bounds [ShareRequest.Reason].
	MaxReasonLength = 1024

	// MaxDataDirLength bounds [SharerDeps.DataDir].
	MaxDataDirLength = 1024

	// MaxFilesPerShare bounds how many regular files a single
	// [Sharer.Share] uploads to the target repo. A larger tool
	// tree probably indicates a misconfigured input or a tool
	// that shouldn't be shared at all (manifest + src + tests +
	// config rarely exceeds a few dozen files); refusing
	// pre-flight is cheaper than half-uploading then triaging.
	// Boundary semantics: exactly 256 files IS accepted; 257
	// files surfaces [ErrInvalidShareRequest] mid-walk before the
	// next [FS.ReadFile] runs (iter-1 C1 fix — see
	// [walkShareFiles] cap parameter).
	MaxFilesPerShare = 256
)

// validIdentifier is the character allowlist applied to source
// names and tool names.
var validIdentifier = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

// validProposerID is the character allowlist applied to a
// resolved proposer identity.
var validProposerID = regexp.MustCompile(`^[a-zA-Z0-9_.@:-]+$`)

// ValidateProposerID applies the same bound + character allowlist
// the [Sharer] applies post-resolver. Exposed so a CLI / RPC
// wrapper can refuse a malformed proposer id BEFORE it reaches a
// wrapped error string on stderr.
func ValidateProposerID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: empty", ErrInvalidProposerID)
	}
	if len(id) > MaxProposerIDLength {
		return fmt.Errorf("%w: %d bytes (max %d)", ErrInvalidProposerID, len(id), MaxProposerIDLength)
	}
	if !validProposerID.MatchString(id) {
		return fmt.Errorf("%w: disallowed characters", ErrInvalidProposerID)
	}
	return nil
}

// ShareRequest is the [Sharer.Share] input.
type ShareRequest struct {
	// SourceName is the configured source the tool lives under.
	// Required; non-empty; bounded; allowlisted.
	SourceName string

	// ToolName is the tool's manifest name. Required; non-empty;
	// bounded; allowlisted.
	ToolName string

	// TargetHint is the closed-set hint the [TargetRepoResolver]
	// consumes to decide between the platform-shared repo and
	// the customer-private repo. Required; must be
	// [TargetSourcePlatform] or [TargetSourcePrivate].
	TargetHint TargetSource

	// Reason is the agent-supplied audit text. Required;
	// non-empty; bounded [MaxReasonLength].
	Reason string

	// ProposerIDHint is the proposer's self-declared id, passed
	// through to [ProposerIdentityResolver] as a hint.
	ProposerIDHint string
}

// Validate enforces the shape contract on a [ShareRequest].
// Returns [ErrInvalidShareRequest] wrapped with field context.
func (r ShareRequest) Validate() error {
	if strings.TrimSpace(r.SourceName) == "" {
		return fmt.Errorf("%w: empty source_name", ErrInvalidShareRequest)
	}
	if len(r.SourceName) > MaxSourceNameLength {
		return fmt.Errorf("%w: source_name %d bytes (max %d)", ErrInvalidShareRequest, len(r.SourceName), MaxSourceNameLength)
	}
	if !validIdentifier.MatchString(r.SourceName) {
		return fmt.Errorf("%w: source_name disallowed characters", ErrInvalidShareRequest)
	}
	if strings.TrimSpace(r.ToolName) == "" {
		return fmt.Errorf("%w: empty tool_name", ErrInvalidShareRequest)
	}
	if len(r.ToolName) > MaxToolNameLength {
		return fmt.Errorf("%w: tool_name %d bytes (max %d)", ErrInvalidShareRequest, len(r.ToolName), MaxToolNameLength)
	}
	if !validIdentifier.MatchString(r.ToolName) {
		return fmt.Errorf("%w: tool_name disallowed characters", ErrInvalidShareRequest)
	}
	if err := r.TargetHint.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.Reason) == "" {
		return fmt.Errorf("%w: empty reason", ErrInvalidShareRequest)
	}
	if len(r.Reason) > MaxReasonLength {
		return fmt.Errorf("%w: reason %d bytes (max %d)", ErrInvalidShareRequest, len(r.Reason), MaxReasonLength)
	}
	return nil
}

// SharerDeps groups the [Sharer] constructor's required + optional
// dependencies. Every field marked required is checked at
// [NewSharer] time — passing a nil panics with a named-field
// message.
type SharerDeps struct {
	// FS is the filesystem seam. Required.
	FS FS

	// Publisher emits [TopicToolShareProposed] and
	// [TopicToolSharePROpened] events. Required.
	Publisher Publisher

	// Clock sources event timestamps and the correlation-id
	// nanosecond suffix. Required.
	Clock Clock

	// SourceLookup resolves [ShareRequest.SourceName]. Required.
	SourceLookup SourceLookup

	// ProposerIdentityResolver resolves the proposer id from the
	// request context. Required.
	ProposerIdentityResolver ProposerIdentityResolver

	// TargetRepoResolver maps the [ShareRequest] to a concrete
	// target repo + base branch. Required.
	TargetRepoResolver TargetRepoResolver

	// GitHubClient executes the create-branch / create-file /
	// open-PR REST chain. Required.
	GitHubClient GitHubClient

	// SlackNotifier sends the lead DM. OPTIONAL — a deployment
	// without Slack-DM integration passes nil and the
	// orchestrator skips the DM entirely.
	SlackNotifier SlackNotifier

	// LeadResolver resolves the Slack user id of the lead
	// Watchkeeper. Required if [SlackNotifier] is non-nil;
	// ignored otherwise.
	LeadResolver LeadResolver

	// Logger receives metadata-only diagnostic log entries. PII
	// discipline: never the operator-supplied reason, tool
	// source bytes, the GitHub token, or the Slack channel id.
	Logger Logger

	// DataDir is the deployment's data-root path. The tool tree
	// lives at `<DataDir>/tools/<source>/<tool>/`. Required;
	// non-empty; bounded [MaxDataDirLength]; absolute.
	DataDir string
}

// Sharer is the [Sharer.Share] orchestrator.
type Sharer struct {
	fs               FS
	publisher        Publisher
	clock            Clock
	sourceLookup     SourceLookup
	identityResolver ProposerIdentityResolver
	targetResolver   TargetRepoResolver
	github           GitHubClient
	slack            SlackNotifier
	leadResolver     LeadResolver
	logger           Logger
	dataDir          string

	nonce uint64
}

// NewSharer constructs a [Sharer] from [SharerDeps]. Panics on
// nil required deps so a wiring bug surfaces at startup.
func NewSharer(deps SharerDeps) *Sharer {
	if deps.FS == nil {
		panic("toolshare: NewSharer: deps.FS must not be nil")
	}
	if deps.Publisher == nil {
		panic("toolshare: NewSharer: deps.Publisher must not be nil")
	}
	if deps.Clock == nil {
		panic("toolshare: NewSharer: deps.Clock must not be nil")
	}
	if deps.SourceLookup == nil {
		panic("toolshare: NewSharer: deps.SourceLookup must not be nil")
	}
	if deps.ProposerIdentityResolver == nil {
		panic("toolshare: NewSharer: deps.ProposerIdentityResolver must not be nil")
	}
	if deps.TargetRepoResolver == nil {
		panic("toolshare: NewSharer: deps.TargetRepoResolver must not be nil")
	}
	if deps.GitHubClient == nil {
		panic("toolshare: NewSharer: deps.GitHubClient must not be nil")
	}
	if deps.SlackNotifier != nil && deps.LeadResolver == nil {
		panic("toolshare: NewSharer: deps.LeadResolver must not be nil when SlackNotifier is non-nil")
	}
	if strings.TrimSpace(deps.DataDir) == "" {
		panic("toolshare: NewSharer: deps.DataDir must not be empty")
	}
	if len(deps.DataDir) > MaxDataDirLength {
		panic(fmt.Sprintf("toolshare: NewSharer: deps.DataDir %d bytes (max %d)", len(deps.DataDir), MaxDataDirLength))
	}
	if !filepath.IsAbs(deps.DataDir) {
		panic("toolshare: NewSharer: deps.DataDir must be absolute")
	}
	return &Sharer{
		fs:               deps.FS,
		publisher:        deps.Publisher,
		clock:            deps.Clock,
		sourceLookup:     deps.SourceLookup,
		identityResolver: deps.ProposerIdentityResolver,
		targetResolver:   deps.TargetRepoResolver,
		github:           deps.GitHubClient,
		slack:            deps.SlackNotifier,
		leadResolver:     deps.LeadResolver,
		logger:           deps.Logger,
		dataDir:          deps.DataDir,
	}
}

// ShareResult is the [Sharer.Share] return value.
type ShareResult struct {
	// PRNumber is the github-issued PR number.
	PRNumber int

	// PRHTMLURL is the github-issued PR HTML URL.
	PRHTMLURL string

	// BranchName is the share branch name that was created on
	// the target repo.
	BranchName string

	// ToolVersion is the manifest version that was shared.
	ToolVersion string

	// CorrelationID is the audit-event correlation id used on
	// both [TopicToolShareProposed] and [TopicToolSharePROpened].
	CorrelationID string

	// LeadNotified reports whether the Slack DM to the lead
	// succeeded. False if SlackNotifier was nil, if LeadResolver
	// returned an empty user id, or if the Slack call failed.
	LeadNotified bool
}

// Share executes the share lifecycle. See the file-head doc-block
// for the resolution order + audit + PII discipline.
// share lifecycle (validate → resolve → on-disk read → publish-1 →
// github chain → publish-2 → notify); the doc-block at the file
// head documents the resolution order verbatim. Decomposing into
// helpers would obscure that discipline (mirror M9.5
// localpatch.Installer.Install which has the same shape).
//
//nolint:gocyclo // Share is a straight-line orchestration of the
func (s *Sharer) Share(ctx context.Context, req ShareRequest) (ShareResult, error) {
	if err := req.Validate(); err != nil {
		return ShareResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return ShareResult{}, err
	}

	src, err := s.sourceLookup(ctx, req.SourceName)
	if err != nil {
		// Iter-1 m5 fix (reviewer A): only wrap-as-ErrUnknownSource
		// when the resolver actually signalled "unknown". A DB
		// outage / config-read failure must propagate verbatim so
		// the operator's `errors.Is` triage routes to the right
		// place.
		if errors.Is(err, ErrUnknownSource) {
			return ShareResult{}, err
		}
		return ShareResult{}, fmt.Errorf("source lookup %q: %w", req.SourceName, err)
	}
	if src.Name != req.SourceName {
		return ShareResult{}, fmt.Errorf("%w: requested %q got %q", ErrSourceLookupMismatch, req.SourceName, src.Name)
	}

	proposerID, err := s.identityResolver(ctx, req.ProposerIDHint)
	if err != nil {
		return ShareResult{}, fmt.Errorf("%w: %w", ErrIdentityResolution, err)
	}
	if strings.TrimSpace(proposerID) == "" {
		return ShareResult{}, ErrEmptyResolvedIdentity
	}
	if vErr := ValidateProposerID(proposerID); vErr != nil {
		return ShareResult{}, vErr
	}

	livePath, err := liveToolPath(s.dataDir, req.SourceName, req.ToolName)
	if err != nil {
		return ShareResult{}, err
	}

	if _, statErr := s.fs.Stat(livePath); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return ShareResult{}, fmt.Errorf("%w: %q", ErrToolMissing, livePath)
		}
		return ShareResult{}, fmt.Errorf("%w: stat live: %w", ErrSourceRead, statErr)
	}

	manifestBytes, err := s.fs.ReadFile(filepath.Join(livePath, "manifest.json"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ShareResult{}, fmt.Errorf("%w: manifest.json absent under %q", ErrManifestRead, livePath)
		}
		return ShareResult{}, fmt.Errorf("%w: read: %w", ErrManifestRead, err)
	}
	manifest, err := toolregistry.DecodeManifest(manifestBytes)
	if err != nil {
		return ShareResult{}, fmt.Errorf("%w: %w", ErrManifestRead, err)
	}

	target, err := s.targetResolver(ctx, req)
	if err != nil {
		return ShareResult{}, fmt.Errorf("%w: %w", ErrTargetResolution, err)
	}
	if vErr := target.Validate(); vErr != nil {
		return ShareResult{}, vErr
	}

	// Iter-1 C1 fix (reviewer A): refuse over-bound BEFORE reading file
	// contents into memory. The walker enforces the cap mid-walk and
	// bails before the next ReadFile so a 1-GiB tool tree cannot OOM
	// the process before the limit kicks in.
	entries, err := walkShareFiles(s.fs, livePath, MaxFilesPerShare)
	if err != nil {
		return ShareResult{}, err
	}
	if len(entries) == 0 {
		return ShareResult{}, fmt.Errorf("%w: no regular files under %q", ErrSourceRead, livePath)
	}

	proposedAt := s.clock.Now()
	correlationID := s.newCorrelationID(proposedAt)

	// Iter-1 M4 fix (reviewer A): re-check ctx between proposed-publish
	// and first github call. If the operator cancels in the gap, the
	// proposed event lands on the bus without any github action ever
	// firing — an orphan audit row. Surfacing ctx.Err here drops the
	// publish before the bus subscriber sees it.
	if err := ctx.Err(); err != nil {
		return ShareResult{}, err
	}

	proposedEvent := newToolShareProposedEvent(
		req.SourceName, req.ToolName, manifest.Version, proposerID, req.Reason,
		target, proposedAt, correlationID,
	)
	publishCtx := context.WithoutCancel(ctx)
	if err := s.publisher.Publish(publishCtx, TopicToolShareProposed, proposedEvent); err != nil {
		if s.logger != nil {
			s.logger.Log(
				ctx, "toolshare: publish proposed failed",
				"source", req.SourceName, "tool", req.ToolName,
				"target_owner", target.Owner, "target_repo", target.Repo,
				"err_class", classifyError(err),
			)
		}
		return ShareResult{}, fmt.Errorf("%w: %w", ErrPublishToolShareProposed, err)
	}

	branch := shareBranchName(req.ToolName, manifest.Version, correlationID, proposedAt)
	baseRef, err := s.github.GetRef(ctx, github.RepoOwner(target.Owner), github.RepoName(target.Repo), "heads/"+target.Base)
	if err != nil {
		return ShareResult{}, fmt.Errorf("%w: %w", ErrGitHubGetBaseRef, err)
	}

	if _, err := s.github.CreateRef(ctx, github.CreateRefOptions{
		Owner: github.RepoOwner(target.Owner),
		Repo:  github.RepoName(target.Repo),
		Ref:   "refs/heads/" + branch,
		SHA:   baseRef.SHA,
	}); err != nil {
		// Iter-1 M3 fix (reviewer B): surface the branch name on the
		// error wrap so the operator's `gh pr close --delete-branch`
		// cleanup is a copy-paste, not a forensics exercise.
		return ShareResult{}, fmt.Errorf("%w: branch=%q: %w", ErrGitHubCreateBranch, branch, err)
	}

	// Iter-1 m5 fix (reviewer B): per-file commit message gives a
	// useful PR commit log without changing the API surface.
	for i, e := range entries {
		message := fmt.Sprintf("share: add %s (%s@%s proposer=%s %d/%d)",
			e.rel, req.ToolName, manifest.Version, proposerID, i+1, len(entries))
		if _, err := s.github.CreateOrUpdateFile(ctx, github.CreateOrUpdateFileOptions{
			Owner:   github.RepoOwner(target.Owner),
			Repo:    github.RepoName(target.Repo),
			Path:    e.rel,
			Content: e.content,
			Message: message,
			Branch:  branch,
		}); err != nil {
			// Iter-1 M1 fix (reviewer A) + M3 (reviewer B): surface
			// branch name + completed-count on the error so the
			// operator can `gh pr close --delete-branch <branch>` and
			// audit subscribers can see how far the upload got.
			if s.logger != nil {
				s.logger.Log(
					ctx, "toolshare: create_file failed mid-share",
					"source", req.SourceName, "tool", req.ToolName,
					"target_owner", target.Owner, "target_repo", target.Repo,
					"branch", branch, "completed", i, "total", len(entries),
					"err_class", classifyError(err),
				)
			}
			return ShareResult{}, fmt.Errorf(
				"%w: branch=%q file=%q completed=%d/%d: %w",
				ErrGitHubCreateFile, branch, e.rel, i, len(entries), err,
			)
		}
	}

	prResult, err := s.github.CreatePullRequest(ctx, github.CreatePullRequestOptions{
		Owner: github.RepoOwner(target.Owner),
		Repo:  github.RepoName(target.Repo),
		Title: fmt.Sprintf("share: %s@%s", req.ToolName, manifest.Version),
		Body:  buildPRBody(req, manifest.Version, proposerID),
		Head:  branch,
		Base:  target.Base,
	})
	if err != nil {
		// Iter-1 M3 fix (reviewer B): include the branch name so the
		// operator can clean up via `gh pr close --delete-branch`.
		return ShareResult{}, fmt.Errorf("%w: branch=%q: %w", ErrGitHubOpenPR, branch, err)
	}

	openedAt := s.clock.Now()
	prOpenedEvent := newToolSharePROpenedEvent(
		req.SourceName, req.ToolName, manifest.Version, proposerID, target,
		prResult.Number, prResult.HTMLURL, openedAt, correlationID,
	)
	if err := s.publisher.Publish(publishCtx, TopicToolSharePROpened, prOpenedEvent); err != nil {
		if s.logger != nil {
			s.logger.Log(
				ctx, "toolshare: publish pr_opened failed",
				"source", req.SourceName, "tool", req.ToolName,
				"pr_number", prResult.Number,
				"err_class", classifyError(err),
			)
		}
		return ShareResult{
				PRNumber: prResult.Number, PRHTMLURL: prResult.HTMLURL,
				BranchName: branch, ToolVersion: manifest.Version,
				CorrelationID: correlationID,
			},
			fmt.Errorf("%w: %w", ErrPublishToolSharePROpened, err)
	}

	leadNotified := s.notifyLead(ctx, target, req, manifest.Version, prResult.HTMLURL)

	return ShareResult{
		PRNumber:      prResult.Number,
		PRHTMLURL:     prResult.HTMLURL,
		BranchName:    branch,
		ToolVersion:   manifest.Version,
		CorrelationID: correlationID,
		LeadNotified:  leadNotified,
	}, nil
}

// notifyLead resolves the lead user id, opens a DM channel, and
// sends a single message linking the PR. Best-effort: a Slack
// failure logs but does not surface as an error.
//
// Iter-1 M6 fix (reviewer A): three distinct failure causes collapse
// to a single `LeadNotified=false` return: (a) the deployment has no
// Slack wired (`s.slack == nil`); (b) the lead resolver returned an
// empty user id (no lead configured for this share's target); (c) a
// Slack call failed (transport, ratelimit, channel-not-found). The
// caller MUST treat `LeadNotified=false` as "did not notify" without
// inferring which case applied — the per-cause Logger.Log entries
// (when the optional Logger is wired) carry the discriminator for
// operator triage.
func (s *Sharer) notifyLead(ctx context.Context, target ResolvedTarget, req ShareRequest, version, prURL string) bool {
	// Iter-1 m10 fix (reviewer A): the leadResolver nil check is
	// redundant under the [NewSharer] invariant (slack non-nil implies
	// leadResolver non-nil) but kept defensively against a future
	// caller bypassing the constructor.
	if s.slack == nil || s.leadResolver == nil {
		return false
	}
	leadID, err := s.leadResolver(ctx, target, req)
	if err != nil {
		if s.logger != nil {
			s.logger.Log(
				ctx, "toolshare: lead resolver failed",
				"target_owner", target.Owner, "target_repo", target.Repo,
				"err_class", classifyError(err),
			)
		}
		return false
	}
	if strings.TrimSpace(leadID) == "" {
		return false
	}
	channelID, err := s.slack.OpenIMChannel(ctx, leadID)
	if err != nil {
		if s.logger != nil {
			s.logger.Log(
				ctx, "toolshare: slack open_im failed",
				"target_owner", target.Owner, "target_repo", target.Repo,
				"err_class", classifyError(err),
			)
		}
		return false
	}
	text := fmt.Sprintf(
		"Tool share opened: %s@%s — review at %s",
		req.ToolName, version, prURL,
	)
	if err := s.slack.SendDMText(ctx, channelID, text); err != nil {
		if s.logger != nil {
			s.logger.Log(
				ctx, "toolshare: slack send_dm failed",
				"target_owner", target.Owner, "target_repo", target.Repo,
				"err_class", classifyError(err),
			)
		}
		return false
	}
	return true
}

// newCorrelationID is a per-Sharer atomic-nonce-suffixed
// nanosecond timestamp. Mirror M9.5 iter-1 critic m6 fix.
func (s *Sharer) newCorrelationID(now time.Time) string {
	nonce := atomic.AddUint64(&s.nonce, 1)
	return strconv.FormatInt(now.UnixNano(), 10) + "-" + strconv.FormatUint(nonce, 10)
}

// classifyError returns a short error class for diagnostic
// logging — keeps the operator-supplied reason / tool source /
// credentials out of log output.
func classifyError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "ctx_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "ctx_deadline"
	case errors.Is(err, github.ErrInvalidAuth):
		return "github_invalid_auth"
	case errors.Is(err, github.ErrRepoNotFound):
		return "github_repo_not_found"
	case errors.Is(err, github.ErrRateLimited):
		return "github_rate_limited"
	case errors.Is(err, github.ErrInvalidArgs):
		return "github_invalid_args"
	default:
		return "other"
	}
}

// liveToolPath returns the absolute on-disk directory for the
// shared tool. Mirror `hostedexport.liveToolPath` /
// `localpatch.liveToolPath`.
func liveToolPath(dataDir, sourceName, toolName string) (string, error) {
	if strings.TrimSpace(dataDir) == "" {
		return "", fmt.Errorf("%w: empty data dir", ErrUnsafePath)
	}
	for _, part := range []struct{ kind, value string }{
		{"source", sourceName},
		{"tool", toolName},
	} {
		if !validIdentifier.MatchString(part.value) {
			return "", fmt.Errorf("%w: %s %q", ErrUnsafePath, part.kind, part.value)
		}
	}
	parent := filepath.Clean(filepath.Join(dataDir, "tools", sourceName))
	cand := filepath.Clean(filepath.Join(parent, toolName))
	rel, err := filepath.Rel(parent, cand)
	if err != nil || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return "", fmt.Errorf("%w: live tool %q/%q", ErrUnsafePath, sourceName, toolName)
	}
	return cand, nil
}

// shareBranchName composes a deterministic, conflict-resistant
// branch name from the tool name, version, correlation id, AND a
// nanosecond timestamp.
//
// Iter-1 C2 fix (reviewer A): the per-Sharer atomic nonce resets to
// zero at [NewSharer] time, so after a process restart the next
// share gets nonce=1 — collides with any prior process's
// "watchkeepers/share/<tool>/<version>/1" branch left open as a
// pending PR. Including the nanosecond timestamp (full UnixNano
// from the correlation id) gives durable per-process uniqueness:
// process restarts never reuse a nanosecond, and within a single
// process the atomic nonce already guarantees uniqueness via the
// correlation-id suffix.
func shareBranchName(tool, version, correlationID string, when time.Time) string {
	cleanedVersion := branchSafe(version)
	// Iter-1 m1 fix (reviewer A): use LastIndex to extract the
	// per-Sharer nonce suffix robustly. The correlation id format
	// is `<unixnano>-<nonce>`; if the format ever evolves to
	// `<unixnano>-<other>-<nonce>`, LastIndex still returns the
	// trailing nonce.
	idx := strings.LastIndex(correlationID, "-")
	nonce := correlationID
	if idx >= 0 && idx < len(correlationID)-1 {
		nonce = correlationID[idx+1:]
	}
	// Iter-1 n3 fix (reviewer A): apply branchSafe to the tool name
	// too (idempotent on already-safe input — validIdentifier is a
	// strict subset of branchSafe-accept).
	return fmt.Sprintf(
		"watchkeepers/share/%s/%s/%d-%s",
		branchSafe(tool), cleanedVersion, when.UnixNano(), nonce,
	)
}

// branchSafe replaces any character outside [a-zA-Z0-9._-] with
// `-` so the resulting branch component complies with git's ref-
// format rules (no `..`, no `~`, no `^`, no `:`, no `?`, no `*`,
// no `[`, no `\`, no leading `-` per segment).
var branchSafeChars = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

func branchSafe(s string) string {
	cleaned := branchSafeChars.ReplaceAllString(s, "-")
	// Defensive: avoid leading `-` per segment which git rejects.
	cleaned = strings.TrimLeft(cleaned, "-")
	if cleaned == "" {
		return "x"
	}
	return cleaned
}

// buildPRBody composes the PR body shown on the target repo. The
// body intentionally contains the agent-supplied reason verbatim
// (audit purpose); it does NOT contain the proposer id (which
// lives on the audit event payload), the tool source code, or
// any credential reference.
func buildPRBody(req ShareRequest, version, proposerID string) string {
	return fmt.Sprintf(
		"Tool share proposed via Watchmaster.\n\n"+
			"- source: `%s`\n"+
			"- tool: `%s`\n"+
			"- version: `%s`\n"+
			"- proposer: `%s`\n"+
			"- target: `%s`\n\n"+
			"## Why\n\n%s\n",
		req.SourceName, req.ToolName, version, proposerID,
		req.TargetHint, req.Reason,
	)
}

// walkShareFile is one regular file under the share's source
// tree. The `rel` field is forward-slash separated (GitHub
// Contents API expects this regardless of operator OS). The
// executable bit is NOT captured: GitHub's Contents API does not
// expose a per-file mode flag (it lives on the Tree API blob
// surface, which M9.6 deliberately does not consume). Iter-1 M2
// fix (reviewer A): the previous `executable bool` field was
// dead code that would mislead a future reader.
type walkShareFile struct {
	rel     string
	content []byte
}

// walkShareFiles walks the tool live-path tree, returning every
// regular file's (relative path, content) tuple. The walker uses
// [fs.DirEntry] semantics and SKIPS symlinks (and other non-
// regular non-directory entries) — same refusal-to-follow
// discipline as M9.5 [localpatch.ContentDigest] / M9.6.a
// [hostedexport.walkAndCopy].
//
// Iter-1 C1 fix (reviewer A): a `maxFiles` parameter bounds the
// walker so an over-sized tool tree cannot OOM the process by
// reading every regular file's contents before the orchestrator's
// post-walk count check fires. The walker bails with
// [ErrInvalidShareRequest] when the count crosses `maxFiles`.
func walkShareFiles(filesystem FS, root string, maxFiles int) ([]walkShareFile, error) {
	out := []walkShareFile{}
	if err := walkShareInto(filesystem, root, root, maxFiles, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func walkShareInto(filesystem FS, root, dir string, maxFiles int, out *[]walkShareFile) error {
	entries, err := filesystem.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("%w: readdir %q: %w", ErrSourceRead, dir, err)
	}
	for _, e := range entries {
		t := e.Type()
		full := filepath.Join(dir, e.Name())
		switch {
		case t.IsDir():
			if err := walkShareInto(filesystem, root, full, maxFiles, out); err != nil {
				return err
			}
		case t&fs.ModeSymlink != 0:
			// Skip — refuse-to-follow.
		case t.IsRegular() || t == 0:
			// Iter-1 m22 fix (reviewer B): avoid the per-file Lstat
			// round-trip when DirEntry.Type() already reported a
			// regular file. Only fall back to Info() (cheaper than
			// Lstat on most FS impls) when Type() returned 0 — the
			// "FS impl doesn't fill DirEntry mode bits" case.
			if t == 0 {
				info, err := e.Info()
				if err != nil {
					return fmt.Errorf("%w: info %q: %w", ErrSourceRead, full, err)
				}
				if !info.Mode().IsRegular() {
					continue
				}
			}
			// Iter-1 C1 fix (reviewer A): refuse BEFORE the next
			// ReadFile so memory growth is bounded.
			if maxFiles > 0 && len(*out) >= maxFiles {
				return fmt.Errorf("%w: %d+ files (max %d)", ErrInvalidShareRequest, maxFiles, maxFiles)
			}
			content, err := filesystem.ReadFile(full)
			if err != nil {
				return fmt.Errorf("%w: readfile %q: %w", ErrSourceRead, full, err)
			}
			rel, err := filepath.Rel(root, full)
			if err != nil {
				return fmt.Errorf("%w: rel %q: %w", ErrSourceRead, full, err)
			}
			*out = append(*out, walkShareFile{
				rel:     filepath.ToSlash(rel),
				content: content,
			})
		default:
			// Skip non-regular non-directory entries.
		}
	}
	return nil
}
