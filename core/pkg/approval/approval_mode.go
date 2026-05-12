package approval

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"

	"github.com/vadimtrunov/watchkeepers/core/pkg/toolregistry"
)

// Mode is the closed-set enum the per-deployment operator
// config declares to choose how proposed tools graduate from
// `tool_proposed` to `tool_approved`:
//
//   - [ModeGitPR] — `git-pr` runs the shared CI workflow on
//     the target git repo (M9.4.d), a human lead merges, the merge
//     fires a webhook that triggers a re-sync.
//   - [ModeSlackNative] — `slack-native` runs the same
//     gate set in-process via the Watchmaster-as-AI-reviewer and
//     posts an approval card with the M9.4.b `[Approve] [Reject]
//     [Test in my DM] [Ask questions]` buttons.
//   - [ModeBoth] — `both` runs both flows in parallel; the
//     proposal is approved when EITHER fires `tool_approved` first
//     (the other flow's pending state is cancelled).
//
// The cross-field invariant: when any configured
// [toolregistry.SourceConfig] has [toolregistry.SourceKindHosted],
// the mode MUST include the `slack-native` branch (either
// [ModeSlackNative] or [ModeBoth]). The hosted
// authoring flow renders exclusively through the Slack approval
// card; a `git-pr`-only deployment with a hosted source would
// orphan every proposal targeting it. [ValidateModeForSources]
// enforces this at the operator-config boundary.
type Mode string

const (
	// ModeGitPR routes proposals through a git PR + CI gate
	// + human-merge cycle.
	ModeGitPR Mode = "git-pr"

	// ModeSlackNative runs the same gate set in-process and
	// posts an approval card to the lead.
	ModeSlackNative Mode = "slack-native"

	// ModeBoth runs the git-pr + slack-native flows in
	// parallel; first-to-approve wins.
	ModeBoth Mode = "both"
)

// Validate reports whether `m` is in the closed [Mode] set.
// Returns [ErrInvalidMode] otherwise (including the empty
// string).
func (m Mode) Validate() error {
	switch m {
	case ModeGitPR, ModeSlackNative, ModeBoth:
		return nil
	default:
		return fmt.Errorf("%w: %q", ErrInvalidMode, string(m))
	}
}

// IncludesSlackNative reports whether `m` is one of the two modes
// that runs the slack-native flow. Used by
// [ValidateModeForSources] for the hosted-source cross-check
// and by future M9.4.b wiring to decide whether to construct the
// in-process AI reviewer.
func (m Mode) IncludesSlackNative() bool {
	return m == ModeSlackNative || m == ModeBoth
}

// IncludesGitPR reports whether `m` is one of the two modes that
// runs the git-pr flow. Used by future M9.4.b wiring to decide
// whether to register the webhook receiver.
func (m Mode) IncludesGitPR() bool {
	return m == ModeGitPR || m == ModeBoth
}

// ValidateModeForSources is the cross-field validator for
// the operator config: when any configured
// [toolregistry.SourceConfig] has [toolregistry.SourceKindHosted],
// the [Mode] MUST include the slack-native branch.
//
// Asymmetry note: the validator enforces hosted→slack-native ONLY.
// A deployment with [ModeSlackNative] (or
// [ModeBoth]) and zero hosted sources is silently accepted
// because slack-native is a strict superset of the git-pr surface —
// it works for `git` and `local` sources too. A future operator
// migrating off the hosted route therefore does not need to flip
// the approval_mode at the same time. The inverse check
// (git-pr-required-when-no-hosted) is intentionally NOT in scope.
//
// The single-field [Mode.Validate] / [TargetSource.Validate]
// checks are mutually independent — callers SHOULD invoke both
// single-field validators before passing the values here.
func ValidateModeForSources(mode Mode, sources []toolregistry.SourceConfig) error {
	if err := mode.Validate(); err != nil {
		return err
	}
	if mode.IncludesSlackNative() {
		return nil
	}
	for _, src := range sources {
		if src.Kind == toolregistry.SourceKindHosted {
			return fmt.Errorf("%w: source %q is hosted", ErrModeMismatchHosted, src.Name)
		}
	}
	return nil
}

// approvalModeYAMLDocument wraps the strict YAML decode of
// `approval_mode: <value>` blocks. The wrapper struct exists so
// `KnownFields(true)` rejects sibling typos (e.g.
// `approvalMode: slack-native`) rather than silently producing a
// zero value.
type approvalModeYAMLDocument struct {
	Mode Mode `yaml:"approval_mode"`
}

// DecodeModeYAML strictly decodes a single-document YAML
// payload of the form `approval_mode: <value>` and returns the
// validated [Mode].
//
// Failure modes:
//
//   - Empty input → [ErrModeYAMLParse] (empty config never
//     carries an `approval_mode:` block — fail-loud).
//   - Unknown field at the document root → [ErrModeYAMLParse]
//     wrapping the yaml.v3 strict-decode failure.
//   - Multiple `---`-separated documents → [ErrModeYAMLMultiDoc].
//   - Decoded value outside the closed set → [ErrInvalidMode]
//     via [Mode.Validate].
//
// Same strict-document discipline as [toolregistry.DecodeSourcesYAML]
// (M9.1.a): `KnownFields(true)` at the root + multi-document guard.
func DecodeModeYAML(raw []byte) (Mode, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", fmt.Errorf("%w: empty input", ErrModeYAMLParse)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	var doc approvalModeYAMLDocument
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%w: empty document", ErrModeYAMLParse)
		}
		return "", fmt.Errorf("%w: %w", ErrModeYAMLParse, err)
	}
	// Multi-document guard. yaml.v3 surfaces a trailing `---`
	// separator with no following content as a successful Decode
	// into a zero-valued document; only a non-zero second document
	// (i.e. an actually-populated `approval_mode:` key) is a true
	// multi-doc input. This matches operator-config files generated
	// by templating tools (Helm, Ansible) that often emit a
	// trailing `---` for consistency across rendered fragments.
	var trailing approvalModeYAMLDocument
	err := dec.Decode(&trailing)
	switch {
	case errors.Is(err, io.EOF):
		// Single-document input — happy path.
	case err == nil && trailing == (approvalModeYAMLDocument{}):
		// Trailing `---` with empty document — accepted as single-doc.
	case err == nil:
		return "", ErrModeYAMLMultiDoc
	default:
		return "", fmt.Errorf("%w: %w", ErrModeYAMLParse, err)
	}
	if err := doc.Mode.Validate(); err != nil {
		return "", err
	}
	return doc.Mode, nil
}
