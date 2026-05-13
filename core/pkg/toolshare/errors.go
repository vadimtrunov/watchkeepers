package toolshare

import "errors"

// ErrInvalidShareRequest is returned by [ShareRequest.Validate] /
// [Sharer.Share] when the request fails the shape contract.
// Wraps with field context via fmt.Errorf for triage.
var ErrInvalidShareRequest = errors.New("toolshare: invalid share request")

// ErrUnknownSource is returned by [Sharer.Share] when the
// requested source name does not resolve via the configured
// [SourceLookup].
var ErrUnknownSource = errors.New("toolshare: unknown source")

// ErrSourceLookupMismatch is returned when the [SourceLookup]
// returns a [toolregistry.SourceConfig] whose `Name` field does
// not match the requested source name.
var ErrSourceLookupMismatch = errors.New("toolshare: source lookup contract violated")

// ErrToolMissing is returned by [Sharer.Share] when the on-disk
// tool tree under `<DataDir>/tools/<source>/<tool>/` is absent.
var ErrToolMissing = errors.New("toolshare: tool tree missing under source")

// ErrManifestRead is returned when the tool tree's `manifest.json`
// is missing or fails [toolregistry.DecodeManifest].
var ErrManifestRead = errors.New("toolshare: manifest read failed")

// ErrSourceRead is returned when the on-disk tool tree cannot be
// opened, walked, or read.
var ErrSourceRead = errors.New("toolshare: source tree read failed")

// ErrTargetResolution wraps a [TargetRepoResolver] failure.
var ErrTargetResolution = errors.New("toolshare: target repo resolution failed")

// ErrInvalidTarget is returned when the resolved
// [TargetRepoResolver] result has empty owner / repo / base.
var ErrInvalidTarget = errors.New("toolshare: invalid target")

// ErrIdentityResolution wraps a [ProposerIdentityResolver] failure.
var ErrIdentityResolution = errors.New("toolshare: proposer identity resolution failed")

// ErrEmptyResolvedIdentity is returned when the
// [ProposerIdentityResolver] returns `("", nil)`.
var ErrEmptyResolvedIdentity = errors.New("toolshare: resolver returned empty proposer identity")

// ErrInvalidProposerID is returned when the resolved proposer id
// exceeds [MaxProposerIDLength] or contains characters outside the
// stable-public-identifier allowlist.
var ErrInvalidProposerID = errors.New("toolshare: invalid proposer id")

// ErrUnsafePath is returned when a derived path resolves outside
// its expected parent.
var ErrUnsafePath = errors.New("toolshare: unsafe path")

// ErrPublishToolShareProposed is returned when the on-disk read +
// validation succeeded but the [Publisher.Publish] of
// [TopicToolShareProposed] failed. No github call is attempted
// after this error — the share aborts before any side effect on
// the target repo. Mirrors `localpatch.ErrPublishLocalPatchApplied`.
var ErrPublishToolShareProposed = errors.New("toolshare: publish tool_share_proposed failed")

// ErrPublishToolSharePROpened is returned when the github calls
// succeeded but the subsequent [Publisher.Publish] of
// [TopicToolSharePROpened] failed. The wrapped underlying error
// chains. Distinguish from `ErrPublishToolShareProposed`: this
// one indicates "PR exists on the target repo, audit notification
// missed"; the proposed-publish-failure aborts before any PR.
var ErrPublishToolSharePROpened = errors.New("toolshare: publish tool_share_pr_opened failed")

// ErrGitHubCreateBranch is returned when the [GitHubClient]
// CreateRef call failed. The wrapped underlying error chains.
var ErrGitHubCreateBranch = errors.New("toolshare: github create branch failed")

// ErrGitHubGetBaseRef is returned when the [GitHubClient] GetRef
// call (for the base branch tip SHA) failed.
var ErrGitHubGetBaseRef = errors.New("toolshare: github get base ref failed")

// ErrGitHubCreateFile is returned when a [GitHubClient]
// CreateOrUpdateFile call failed mid-tree-upload. Half the tree
// may be present on the share branch; the operator triages by
// inspecting the branch on the target repo and either re-running
// the share or manually deleting the half-uploaded branch.
var ErrGitHubCreateFile = errors.New("toolshare: github create file failed")

// ErrGitHubOpenPR is returned when the [GitHubClient]
// CreatePullRequest call failed. The share branch may exist on
// the target repo without an open PR; the operator triages by
// inspecting the branch.
var ErrGitHubOpenPR = errors.New("toolshare: github open pr failed")
