package toolregistry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// Manifest is the wire-format per-tool `manifest.json` decoded by
// [DecodeManifest] / [LoadManifestFromFile]. The schema mirrors the
// roadmap M9.1 description: `name`, `version`, `capabilities`,
// zod-compatible `schema`, auto-filled `source`, and optional
// `signature`. Strict decoding rejects unknown fields so an
// operator-authored typo never silently degrades to defaults.
type Manifest struct {
	// Name is the tool identifier used by the runtime ACL gate at
	// `InvokeTool` time. Required; an empty value yields
	// [ErrManifestMissingRequired]. Convention: lower_snake_case
	// (e.g. `find_overdue_tickets`); enforcement is deferred to a
	// linter rule rather than the decoder.
	Name string `json:"name"`

	// Version is the SemVer-shaped string the runtime ACL projects
	// alongside Name to gate per-tool pin checks (see runtime.ToolEntry).
	// Required; empty yields [ErrManifestMissingRequired]. Strict format
	// enforcement is deferred to M9.4's CI gates so this decoder stays
	// transport-only.
	Version string `json:"version"`

	// Capabilities is the list of capability ids this tool requires.
	// At least one entry is required (an empty list signals "no
	// capabilities declared" which is a non-sensical state for an
	// authored tool — the [capability] package's deny-by-default
	// stance would refuse every call). The plain-language description
	// per id lives in M9.3's `dict/capabilities.yaml`.
	Capabilities []string `json:"capabilities"`

	// Schema is the zod-compatible JSON-schema fragment describing the
	// tool's call arguments. Stored verbatim as [json.RawMessage] so
	// downstream tooling can transform it (e.g. emit a TypeScript
	// `z.object` wrapper or a Go runtime validator) without the
	// decoder picking a normal form. Required; null / empty yields
	// [ErrManifestMissingRequired].
	Schema json.RawMessage `json:"schema"`

	// Source is the name of the [SourceConfig] this manifest was
	// synced from. Auto-filled by [LoadManifestFromFile] from the
	// caller-supplied `sourceName` argument so the runtime always
	// observes a populated value. An operator-authored manifest that
	// hard-codes `source` is treated as a tamper attempt:
	// [LoadManifestFromFile] returns [ErrManifestSourceCollision] when
	// the on-disk value disagrees with the stamping argument.
	Source string `json:"source,omitempty"`

	// Signature is the optional detached-signature blob over the
	// manifest + source-tree (cosign / minisign output, M9.3). Empty
	// signature is acceptable in M9.1.a; the default
	// [NoopSignatureVerifier] accepts both states. Strict decoding
	// still allows the field name to be absent entirely (the JSON tag
	// is `omitempty`).
	Signature string `json:"signature,omitempty"`
}

// Validate runs the post-decode required-field check. Called by
// [DecodeManifest] before returning to the caller so a [Manifest] in
// hand is always well-formed.
func (m Manifest) Validate() error {
	if strings.TrimSpace(m.Name) == "" {
		return fmt.Errorf("%w: name", ErrManifestMissingRequired)
	}
	if strings.TrimSpace(m.Version) == "" {
		return fmt.Errorf("%w: version", ErrManifestMissingRequired)
	}
	if len(m.Capabilities) == 0 {
		return fmt.Errorf("%w: capabilities", ErrManifestMissingRequired)
	}
	for i, c := range m.Capabilities {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("%w: capabilities[%d]", ErrManifestMissingRequired, i)
		}
	}
	if len(bytes.TrimSpace(m.Schema)) == 0 || bytes.Equal(bytes.TrimSpace(m.Schema), []byte("null")) {
		return fmt.Errorf("%w: schema", ErrManifestMissingRequired)
	}
	return nil
}

// DecodeManifest strictly decodes `raw` into a [Manifest], validates
// required fields, and returns it. Strict mode (`DisallowUnknownFields`)
// rejects any JSON key not declared on [Manifest]; a typo therefore
// surfaces as [ErrManifestUnknownField] rather than silently degrading
// to a default value.
//
// Failure modes:
//
//   - Malformed JSON → wrapped [ErrManifestParse].
//   - Unknown field → wrapped [ErrManifestUnknownField] (chained through
//     [ErrManifestParse] so callers matching either sentinel succeed).
//   - Missing required field → wrapped [ErrManifestMissingRequired].
//
// The returned [Manifest] is a defensive deep-copy of all reference-
// typed fields (`Capabilities`, `Schema`) — the caller's `raw` buffer
// is no longer aliased after this function returns.
func DecodeManifest(raw []byte) (Manifest, error) {
	if len(raw) == 0 {
		return Manifest{}, fmt.Errorf("%w: empty input", ErrManifestParse)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		if isUnknownFieldErr(err) {
			return Manifest{}, fmt.Errorf("%w: %w: %w", ErrManifestUnknownField, ErrManifestParse, err)
		}
		return Manifest{}, fmt.Errorf("%w: %w", ErrManifestParse, err)
	}

	// Guard against trailing JSON garbage. `json.Decoder.Decode` only
	// reads the first value; a clean payload yields [io.EOF] on the
	// second call. Anything else — a second valid value, syntactically
	// invalid bytes, an unterminated literal — surfaces as
	// [ErrManifestParse] so a manifest with `{...}garbage` is refused
	// just like a manifest with `{...}{...}`.
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Manifest{}, fmt.Errorf("%w: trailing document", ErrManifestParse)
		}
		return Manifest{}, fmt.Errorf("%w: trailing input: %w", ErrManifestParse, err)
	}

	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}

	// Defensive deep-copy of reference-typed fields so a caller
	// mutating `raw` post-decode cannot bleed into the returned
	// value. Mirrors the M7.1.c.c `cloneBotProfile` pattern.
	m.Capabilities = cloneStringSlice(m.Capabilities)
	m.Schema = cloneBytes(m.Schema)
	return m, nil
}

// LoadManifestFromFile reads `<dir>/manifest.json` from `fs`, decodes
// it via [DecodeManifest], stamps `Source` from `sourceName`, and
// returns the result. The stamping rule:
//
//   - On-disk `source` empty: the field is filled from `sourceName`.
//   - On-disk `source` non-empty AND equal to `sourceName`: no-op.
//   - On-disk `source` non-empty AND different from `sourceName`:
//     [ErrManifestSourceCollision].
//
// `sourceName` is required; an empty value yields
// [ErrInvalidSourceName] without touching the filesystem.
func LoadManifestFromFile(filesystem FS, dir, sourceName string) (Manifest, error) {
	if filesystem == nil {
		panic("toolregistry: LoadManifestFromFile: fs must not be nil")
	}
	if strings.TrimSpace(sourceName) == "" {
		return Manifest{}, ErrInvalidSourceName
	}
	path := filepath.Join(dir, "manifest.json")
	raw, err := filesystem.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrManifestParse, err)
	}
	m, err := DecodeManifest(raw)
	if err != nil {
		return Manifest{}, err
	}
	switch {
	case m.Source == "":
		m.Source = sourceName
	case m.Source != sourceName:
		return Manifest{}, fmt.Errorf(
			"%w: on-disk %q != stamping %q",
			ErrManifestSourceCollision, m.Source, sourceName,
		)
	}
	return m, nil
}

// isUnknownFieldErr reports whether `err` is the encoding/json
// "unknown field" diagnostic. The stdlib does not export a typed
// sentinel for this case, so we sniff the message text — same
// pattern as `core/pkg/config/config.go`'s `isUnknownFieldErr`.
func isUnknownFieldErr(err error) bool {
	return strings.Contains(err.Error(), "unknown field")
}

func cloneStringSlice(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

func cloneBytes(in []byte) []byte {
	if in == nil {
		return nil
	}
	out := make([]byte, len(in))
	copy(out, in)
	return out
}
