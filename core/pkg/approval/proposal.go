package approval

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Maximum byte lengths enforced by [ProposalInput.Validate]. Bounds
// are deliberate at the agent-facing authoring boundary so an
// adversarial agent cannot land an unbounded `code_draft` body or a
// `plain_language_description` paragraph that DOS-es the
// downstream approval card / AI reviewer. The numbers are
// conservative — typical tools are well under these limits — and
// exported so callers can surface "max length" hints in their UI.
const (
	// MaxToolNameLength bounds [ProposalInput.Name]; matches the
	// per-tool subdirectory naming convention under
	// `$DATA_DIR/tools/<source>/<tool>/`.
	MaxToolNameLength = 64

	// MaxPurposeLength bounds [ProposalInput.Purpose] (the agent's
	// rationale for needing the tool).
	MaxPurposeLength = 1024

	// MaxPlainLanguageDescriptionLength bounds
	// [ProposalInput.PlainLanguageDescription] (the lead-facing
	// summary rendered on the M9.4.b approval card).
	MaxPlainLanguageDescriptionLength = 4096

	// MaxCodeDraftLength bounds [ProposalInput.CodeDraft]; 64 KiB
	// fits any TS source the agent realistically produces while
	// refusing pathological payloads.
	MaxCodeDraftLength = 65536

	// MaxCapabilityCount bounds the number of capability ids per
	// proposal. A tool needing more than 32 capabilities is almost
	// certainly mis-scoped.
	MaxCapabilityCount = 32

	// MaxCapabilityIDLength bounds an individual capability id.
	MaxCapabilityIDLength = 128

	// MaxProposerIDLength bounds the [IdentityResolver]'s returned
	// identifier. The bound exists so a buggy resolver returning
	// e.g. a bearer-token-shaped string cannot leak unbounded
	// content onto the [TopicToolProposed] event payload or the
	// publish-failure logger entry. 128 bytes fits a canonical UUID
	// (36 chars) plus any reasonable opaque agent identifier; longer
	// values surface [ErrInvalidProposerID] at [Proposer.Submit]
	// time. Same defensive-bounds discipline as the per-field
	// `ProposalInput` bounds.
	MaxProposerIDLength = 128
)

// ErrProposalFieldTooLong is returned by [ProposalInput.Validate]
// when a string field exceeds its [MaxToolNameLength] /
// [MaxPurposeLength] / [MaxPlainLanguageDescriptionLength] /
// [MaxCodeDraftLength] / [MaxCapabilityIDLength] bound, or when
// `Capabilities` exceeds [MaxCapabilityCount]. The wrapped error
// names the field + the actual length so operators can debug.
var ErrProposalFieldTooLong = errors.New("approval: proposal field exceeds maximum length")

// toolNameRegex pins the lower_snake_case convention for tool names
// at the authoring boundary. The first character must be a
// lowercase letter; subsequent characters may include digits and
// underscores. Same convention documented on
// [toolregistry.Manifest.Name] (whose godoc deferred enforcement to
// a linter — this is the linter).
var toolNameRegex = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// ProposalInput is the caller-supplied input to [Proposer.Submit]:
// the exact field set the agent-facing `propose_tool` capability
// accepts. The wire shape matches the roadmap M9.4 text:
// `propose_tool(name, purpose, plain_language_description,
// code_draft, capabilities, target_source ∈ {platform, private})`.
//
// PII discipline: this struct flows in-process to [Proposer.Submit];
// it is NOT serialised onto the eventbus. The metadata-only
// [ToolProposed] payload carries proposal id + tool name + proposer
// id + target source + capability ids + timestamp + correlation id
// only. `Purpose`, `PlainLanguageDescription`, and `CodeDraft` are
// reserved for the downstream M9.4.b approval card AND the future
// audit-subscriber persistence (M9.7); the eventbus boundary
// excludes them by construction.
type ProposalInput struct {
	// Name is the proposed tool identifier (lower_snake_case,
	// `^[a-z][a-z0-9_]*$`, max [MaxToolNameLength] bytes). Becomes
	// [toolregistry.Manifest.Name] when the proposal is approved
	// and a manifest is generated.
	Name string

	// Purpose is the agent's free-form rationale for needing the
	// tool. Required; max [MaxPurposeLength] bytes.
	Purpose string

	// PlainLanguageDescription is the lead-facing natural-language
	// summary rendered on the M9.4.b approval card. Required; max
	// [MaxPlainLanguageDescriptionLength] bytes. Distinct from
	// `Purpose`: `Purpose` is the agent's reason, this is the
	// lead-readable description (non-engineer leads need this).
	PlainLanguageDescription string

	// CodeDraft is the TypeScript / JavaScript source draft the
	// agent proposes as the tool body. The CI gates (M9.4.d) and
	// the in-process AI reviewer (M9.4.b) consume it; the runtime
	// loads it post-approval via the standard
	// [toolregistry.Scheduler] path. Required; max
	// [MaxCodeDraftLength] bytes.
	CodeDraft string

	// Capabilities is the list of capability ids the tool will
	// request at runtime. Required (non-empty); max
	// [MaxCapabilityCount] entries; each non-blank and max
	// [MaxCapabilityIDLength] bytes. Membership against M9.3.a's
	// `dict/capabilities.yaml` is NOT enforced here — by design,
	// to keep the dependency arrow one-way (capdict consumes the
	// dictionary; approval emits the proposal; neither imports the
	// other). The card renderer surfaces unknown ids via the
	// `(no translation registered)` placeholder, and the
	// canonical-set bijection test in `core/pkg/capdict/canonical_test.go`
	// pins the dictionary against the in-code authority. The
	// approval-side validator stays SHAPE-ONLY (non-blank +
	// length + duplicate-free); capdict's grammar is strictly
	// tighter, so every capdict id is a legal proposer id but
	// NOT vice-versa.
	Capabilities []string

	// TargetSource selects platform vs private (no `local` per the
	// [TargetSource] godoc). Required; validated via
	// [TargetSource.Validate].
	TargetSource TargetSource
}

// Validate enforces the shape contract on a [ProposalInput].
// Returns the first applicable sentinel (one of:
// [ErrMissingProposalName], [ErrInvalidProposalName],
// [ErrProposalFieldTooLong], [ErrMissingProposalPurpose],
// [ErrMissingPlainLanguageDescription], [ErrMissingCodeDraft],
// [ErrMissingProposalCapabilities], [ErrInvalidProposalCapability],
// [ErrDuplicateProposalCapability], [ErrInvalidTargetSource])
// wrapped with field context.
//
// Validation order is deterministic so test assertions on the
// "first failure" stay stable. Name is checked before all other
// fields because an invalid name pins the resulting manifest path
// (`$DATA_DIR/tools/<source>/<name>/`) and is the most common
// authoring mistake. The function delegates per-field checks to
// helpers so the top-level function stays simple and the
// gocyclo budget stays small.
func (p ProposalInput) Validate() error {
	if err := validateProposalName(p.Name); err != nil {
		return err
	}
	if err := validateBoundedText(p.Purpose, "purpose", MaxPurposeLength, ErrMissingProposalPurpose); err != nil {
		return err
	}
	if err := validateBoundedText(p.PlainLanguageDescription, "plain_language_description", MaxPlainLanguageDescriptionLength, ErrMissingPlainLanguageDescription); err != nil {
		return err
	}
	if err := validateBoundedText(p.CodeDraft, "code_draft", MaxCodeDraftLength, ErrMissingCodeDraft); err != nil {
		return err
	}
	if err := validateCapabilities(p.Capabilities); err != nil {
		return err
	}
	return p.TargetSource.Validate()
}

// validateProposalName runs the name-specific shape checks:
// non-blank, bounded length, lower_snake_case regex.
func validateProposalName(name string) error {
	if strings.TrimSpace(name) == "" {
		return ErrMissingProposalName
	}
	if len(name) > MaxToolNameLength {
		return fmt.Errorf("%w: name has %d bytes (max %d)", ErrProposalFieldTooLong, len(name), MaxToolNameLength)
	}
	if !toolNameRegex.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrInvalidProposalName, name)
	}
	return nil
}

// validateBoundedText runs the "required + bounded" pattern shared
// by Purpose / PlainLanguageDescription / CodeDraft. `fieldName` is
// the user-facing label injected into the "too long" error message;
// `missingSentinel` is the package-level sentinel returned when
// the value is empty / whitespace.
func validateBoundedText(value, fieldName string, maxLen int, missingSentinel error) error {
	if strings.TrimSpace(value) == "" {
		return missingSentinel
	}
	if len(value) > maxLen {
		return fmt.Errorf("%w: %s has %d bytes (max %d)", ErrProposalFieldTooLong, fieldName, len(value), maxLen)
	}
	return nil
}

// validateCapabilities runs the capabilities-specific shape checks:
// non-empty list, bounded count, per-entry non-blank + bounded
// length + no duplicates.
func validateCapabilities(caps []string) error {
	if len(caps) == 0 {
		return ErrMissingProposalCapabilities
	}
	if len(caps) > MaxCapabilityCount {
		return fmt.Errorf("%w: capabilities has %d entries (max %d)", ErrProposalFieldTooLong, len(caps), MaxCapabilityCount)
	}
	seen := make(map[string]struct{}, len(caps))
	for i, c := range caps {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("%w: capabilities[%d]", ErrInvalidProposalCapability, i)
		}
		if len(c) > MaxCapabilityIDLength {
			return fmt.Errorf("%w: capabilities[%d] has %d bytes (max %d)", ErrProposalFieldTooLong, i, len(c), MaxCapabilityIDLength)
		}
		if _, dup := seen[c]; dup {
			return fmt.Errorf("%w: %q at capabilities[%d]", ErrDuplicateProposalCapability, c, i)
		}
		seen[c] = struct{}{}
	}
	return nil
}

// Proposal is the persisted (in-memory at M9.4.a; SQL DAO at
// M9.4.b) record of a successful [Proposer.Submit]. The struct is
// returned to the caller AND used to construct the [ToolProposed]
// event payload — defensive deep-copy of reference-typed fields
// happens in [Proposer.Submit] BEFORE both consumers see the
// Proposal, so caller mutation post-Submit does not corrupt either.
type Proposal struct {
	// ID is the durable proposal identifier; M9.4.b's webhook
	// receiver and the M9.4.b approval-card button callbacks
	// look up by this id.
	ID uuid.UUID

	// ProposerID is the agent identity resolved by the
	// [IdentityResolver] at [Proposer.Submit] time — never empty.
	ProposerID string

	// Input is a defensively-deep-copied snapshot of the
	// [ProposalInput] passed to [Proposer.Submit]. Caller-side
	// mutation of the original input slice MUST NOT affect this
	// field.
	Input ProposalInput

	// ProposedAt is the wall-clock timestamp captured from the
	// configured [Clock] inside [Proposer.Submit].
	ProposedAt time.Time

	// CorrelationID is a process-monotonic identifier (same opaque
	// shape as [toolregistry.SourceSynced.CorrelationID]). The
	// [ToolProposed] event for this proposal carries the same
	// value so downstream subscribers can join the proposal record
	// to the event stream.
	CorrelationID string
}

// cloneProposalInput defensively deep-copies the reference-typed
// fields on a [ProposalInput]. Only `Capabilities` requires the
// copy — Go strings are immutable header-plus-pointer values, so
// aliasing the backing bytes is safe (no caller can mutate them
// after construction). Mirrors the M7.1.c.c `cloneBotProfile`
// discipline and M9.1.a's `cloneStringSlice`.
func cloneProposalInput(in ProposalInput) ProposalInput {
	out := in
	if in.Capabilities != nil {
		out.Capabilities = make([]string, len(in.Capabilities))
		copy(out.Capabilities, in.Capabilities)
	}
	return out
}
