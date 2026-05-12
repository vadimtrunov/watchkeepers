package approval

import "fmt"

// TargetSource is the closed-set enum the agent-facing `propose_tool`
// capability selects to indicate which source the proposed tool
// should land in:
//
//   - [TargetSourcePlatform] — the platform `watchkeeper-tools`
//     repository, broadly applicable across customers; reviewed by
//     the platform team.
//   - [TargetSourcePrivate] — the customer's private repository (or
//     hosted bucket); reviewed by the customer's lead.
//
// `local` is DELIBERATELY absent from this enum — the M9.4 roadmap
// text states "local source never offered to the agent" because
// local sources are operator-installed out-of-band via the M9.5
// `make tools-local-install <folder>` workflow. The lead may
// override `target_source` at approval time (M9.4.b's approval
// card); that override is a distinct lead-only enum and does not
// flow through this validator.
//
// The closed-set discipline mirrors [toolregistry.SourceKind] (and
// the M9.1.a iter-1 lesson "loud-failure default"): a typo at the
// `propose_tool` boundary yields [ErrInvalidTargetSource] rather
// than a silent acceptance.
type TargetSource string

const (
	// TargetSourcePlatform routes the proposal to the platform
	// `watchkeeper-tools` repository.
	TargetSourcePlatform TargetSource = "platform"

	// TargetSourcePrivate routes the proposal to the customer's
	// private repository / hosted bucket.
	TargetSourcePrivate TargetSource = "private"
)

// Validate reports whether `t` is in the closed [TargetSource] set.
// Returns [ErrInvalidTargetSource] otherwise (including the empty
// string).
func (t TargetSource) Validate() error {
	switch t {
	case TargetSourcePlatform, TargetSourcePrivate:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidTargetSource, string(t))
	}
}
