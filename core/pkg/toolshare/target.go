package toolshare

import "fmt"

// TargetSource is the closed-set discriminator for the share
// destination. Mirrors the M9.4.a `approval.TargetSource` enum but
// scoped to the share-specific use case so the audit-vocabulary
// stays one-package-one-source-of-truth.
//
// Values:
//
//   - [TargetSourcePlatform] — the platform-shared
//     `watchkeeper-tools` repo (broadly-useful tools graduate
//     here).
//   - [TargetSourcePrivate] — the customer-owned private git
//     repository (customer-IP tools graduate here).
type TargetSource string

const (
	// TargetSourcePlatform is the platform-shared repo target.
	TargetSourcePlatform TargetSource = "platform"

	// TargetSourcePrivate is the customer-owned private repo target.
	TargetSourcePrivate TargetSource = "private"
)

// Validate reports whether `t` is a known [TargetSource] value.
// Returns a wrapped [ErrInvalidShareRequest] otherwise.
func (t TargetSource) Validate() error {
	switch t {
	case TargetSourcePlatform, TargetSourcePrivate:
		return nil
	default:
		return fmt.Errorf("%w: invalid target_source %q", ErrInvalidShareRequest, string(t))
	}
}

// ResolvedTarget is the [TargetRepoResolver] return value
// identifying the target repo, base branch, and which closed-set
// [TargetSource] the resolver chose.
type ResolvedTarget struct {
	// Owner is the GitHub owner (org or user) of the target repo.
	Owner string

	// Repo is the GitHub repo name.
	Repo string

	// Base is the base branch the share PR targets for merge
	// (typically `main`).
	Base string

	// Source is the closed-set [TargetSource] the resolver chose.
	// Recorded on the audit payload so the M9.7 audit subscriber
	// can join the share row to the closed-set vocabulary the
	// M9.4 approval flow uses.
	Source TargetSource
}

// Validate reports whether the resolved target is well-formed.
// Returns [ErrInvalidTarget] wrapped with field context.
func (r ResolvedTarget) Validate() error {
	if r.Owner == "" {
		return fmt.Errorf("%w: empty owner", ErrInvalidTarget)
	}
	if r.Repo == "" {
		return fmt.Errorf("%w: empty repo", ErrInvalidTarget)
	}
	if r.Base == "" {
		return fmt.Errorf("%w: empty base", ErrInvalidTarget)
	}
	if err := r.Source.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidTarget, err)
	}
	return nil
}
