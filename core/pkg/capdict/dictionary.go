package capdict

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

// SupportedSchemaVersion is the only `version:` value the dictionary
// loader accepts in Phase 1. Future versions land additively on the
// same major (new optional locale keys, etc.) without bumping this
// constant; a major schema rewrite would bump the constant AND retain
// a back-compat decode path for the prior major.
//
// Note on additive schema evolution: the loader uses
// `yaml.KnownFields(true)`, so a new optional yaml field (e.g.
// `descriptions_i18n:` under each capability) requires a matching Go
// field on [Capability] before the loader will accept it. Pure-yaml
// additive change is NOT automatic — both sides must move together.
// This is a deliberate trade: strict decode buys typo-catching at
// the cost of strict-deploy on schema additions.
const SupportedSchemaVersion = 1

// MaxDescriptionLength caps a single capability description. Tight
// enough that the approval card's `Capabilities` section (one mrkdwn
// bullet per id, max [card.cardCapabilityListMaxLines] = 32 bullets)
// stays well below Slack's ~3000-char section-block limit even when
// every row is at the cap. The dictionary owns its own cap rather
// than reading the renderer's — the loader stays decoupled from the
// renderer's exact-shape constraint.
const MaxDescriptionLength = 240

// MaxCapabilityIDLength bounds the byte length of a capability id.
// Mirrors `approval.MaxCapabilityIDLength` so the dictionary loader
// rejects every id the proposer validator would also reject. The
// duplicate-rather-than-imported constant avoids a capdict→approval
// import edge.
const MaxCapabilityIDLength = 128

// Capability is one row of the dictionary — currently a single
// `description` field with the per-id object shape kept on disk so
// future locale keys (e.g. `descriptions_i18n: {ru: ...}`) land
// additively without a v2 bump.
//
// The type is EXPORTED anticipating a future `Capability(id) (Capability, ok)`
// surface for callers that need the full row rather than just the
// description (e.g. a future operator-facing CLI surfacing the
// id-plus-locale view). No public constructor is provided today by
// design — every Capability flows through the strict-decode pipe in
// [LoadFromBytes].
type Capability struct {
	// Description is the lead-facing plain-language line rendered on
	// the approval card. Required at decode time.
	Description string `yaml:"description"`
}

// Dictionary is the typed, validated form of `dict/capabilities.yaml`.
// The zero value is NOT usable — callers construct via [LoadFromFile]
// or [LoadFromBytes].
type Dictionary struct {
	version int
	// caps is the internal lookup map. The map is built once at
	// construction and never mutated; concurrent readers are safe by
	// the never-mutated invariant (not by a runtime lock). The
	// per-construction deep-copy on the value rows is defense in
	// depth against future private construction paths that might
	// share a yaml-decoded source map; today the only public path
	// ([LoadFromBytes]) fresh-decodes so the copy is not strictly
	// necessary.
	caps map[string]Capability
}

// Version returns the schema version of the loaded dictionary. Always
// equals [SupportedSchemaVersion] in Phase 1.
func (d *Dictionary) Version() int {
	if d == nil {
		return 0
	}
	return d.version
}

// Len returns the number of capabilities in the dictionary.
func (d *Dictionary) Len() int {
	if d == nil {
		return 0
	}
	return len(d.caps)
}

// Translate looks up the supplied capability id and returns its
// description plus an `ok` flag. The empty string + `false` signals
// a miss; the [Translator] adapter folds the miss into the empty-
// string return shape the `approval.CapabilityTranslator` contract
// expects.
func (d *Dictionary) Translate(id string) (string, bool) {
	if d == nil {
		return "", false
	}
	c, ok := d.caps[id]
	if !ok {
		return "", false
	}
	return c.Description, true
}

// TranslateOrError is the error-returning sibling of [Translate]. A
// miss surfaces as [ErrUnknownCapability] wrapped with the offending
// id so operator callers can `errors.Is` the sentinel.
func (d *Dictionary) TranslateOrError(id string) (string, error) {
	desc, ok := d.Translate(id)
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrUnknownCapability, id)
	}
	return desc, nil
}

// Capabilities returns a freshly-allocated, sorted slice of every
// capability id in the dictionary. Mutating the returned slice does
// NOT affect subsequent lookups (defensive copy on every call).
func (d *Dictionary) Capabilities() []string {
	if d == nil || len(d.caps) == 0 {
		return nil
	}
	out := make([]string, 0, len(d.caps))
	for id := range d.caps {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// newDictionary constructs a Dictionary from validated raw inputs.
// Defensive deep-copy of `caps` so a future private construction
// path that supplies a caller-mutable map (testdata fixture, etc.)
// cannot bleed into the Dictionary state by mutating the source map
// post-construction. The deep-copy is defense in depth — today's
// only public path ([LoadFromBytes]) fresh-decodes so the copy is
// not strictly necessary; it preserves the invariant for future
// private factories.
func newDictionary(version int, caps map[string]Capability) *Dictionary {
	cp := make(map[string]Capability, len(caps))
	for id, c := range caps {
		cp[id] = Capability{Description: c.Description}
	}
	return &Dictionary{version: version, caps: cp}
}

// validateCapabilityID enforces capdict's lower-snake-case + colon-
// namespace grammar:
//
//   - Non-empty, ≤ [MaxCapabilityIDLength] bytes.
//   - One or more lowercase ASCII letters / digits / underscores.
//   - Optional `:`-separated segments (so both `tool:share` and
//     `localdev` validate; multi-segment ids must have non-empty
//     segments).
//
// Capdict is STRICTER than the approval-side `proposal.go` capability-
// id validator: the proposer enforces only non-blank + length +
// duplicate-free, while capdict additionally rejects uppercase,
// hyphens, dots, spaces, and empty segments. Every id capdict
// accepts is a legal proposer id; the reverse is NOT true.
//
// Returns nil on a legal id; a chained [ErrInvalidDictionary] +
// [ErrInvalidCapabilityID] otherwise. Error messages carry the
// offending id verbatim — a capability id is a PUBLIC dictionary
// key, not a credential.
func validateCapabilityID(id string) error {
	if id == "" {
		return fmt.Errorf("%w: %w: empty id", ErrInvalidDictionary, ErrInvalidCapabilityID)
	}
	if len(id) > MaxCapabilityIDLength {
		return fmt.Errorf("%w: %w: id %q exceeds %d bytes", ErrInvalidDictionary, ErrInvalidCapabilityID, id, MaxCapabilityIDLength)
	}
	for _, seg := range strings.Split(id, ":") {
		if seg == "" {
			return fmt.Errorf("%w: %w: id %q has an empty segment", ErrInvalidDictionary, ErrInvalidCapabilityID, id)
		}
		for _, r := range seg {
			switch {
			case r >= 'a' && r <= 'z':
			case r >= '0' && r <= '9':
			case r == '_':
			default:
				return fmt.Errorf("%w: %w: id %q has illegal byte %q", ErrInvalidDictionary, ErrInvalidCapabilityID, id, r)
			}
		}
	}
	return nil
}

// validateDescription enforces the per-row description invariants:
// non-empty after trim, valid utf-8, no ASCII control bytes (TAB /
// NL / CR / NUL / DEL), ≤ [MaxDescriptionLength].
//
// Returns nil on a legal description; a chained [ErrInvalidDictionary]
// + leaf sentinel ([ErrEmptyDescription] / [ErrDescriptionTooLong]
// / [ErrEmbeddedControlByte]) otherwise. Invalid utf-8 surfaces
// under the umbrella sentinel without a dedicated leaf.
func validateDescription(id, desc string) error {
	if strings.TrimSpace(desc) == "" {
		return fmt.Errorf("%w: %w: capability %q", ErrInvalidDictionary, ErrEmptyDescription, id)
	}
	if len(desc) > MaxDescriptionLength {
		return fmt.Errorf("%w: %w: capability %q description exceeds %d bytes", ErrInvalidDictionary, ErrDescriptionTooLong, id, MaxDescriptionLength)
	}
	// Reject genuinely-malformed utf-8 ONCE before the control-byte
	// loop. Using utf8.ValidString upfront means a legitimately-
	// encoded U+FFFD codepoint flows through (operator may
	// legitimately use the replacement char as a literal); only
	// truly-invalid byte sequences are rejected here.
	if !utf8.ValidString(desc) {
		return fmt.Errorf("%w: capability %q description is not valid utf-8", ErrInvalidDictionary, id)
	}
	for i, r := range desc {
		// Reject ASCII control bytes (0x00..0x1F + 0x7F). Spaces
		// (0x20) and printable runes are allowed; multi-byte UTF-8
		// codepoints flow through verbatim — the dictionary is i18n-
		// ready by design.
		if r < 0x20 || r == 0x7F {
			return fmt.Errorf("%w: %w: capability %q description carries control byte 0x%02X at offset %d", ErrInvalidDictionary, ErrEmbeddedControlByte, id, r, i)
		}
	}
	return nil
}
