package capdict

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// FS is the read-only filesystem seam [LoadFromFile] consumes.
// Mirrors `toolregistry.FS`. The production wiring satisfies it via
// `osfs.OS` (or any wrapper); tests satisfy it with an in-memory map.
//
// A nil [FS] passed to [LoadFromFile] is rejected — the loader does
// NOT fall back to `os.ReadFile` silently because the production
// wiring layer owns the FS choice (sandbox-friendly seam discipline).
type FS interface {
	ReadFile(path string) ([]byte, error)
}

// LoadFromFile reads `path` via the supplied [FS] seam and decodes
// the YAML payload via [LoadFromBytes]. Read failures surface under
// the distinct [ErrDictionaryRead] sentinel; structural failures
// surface under [ErrInvalidDictionary] (via [LoadFromBytes]).
func LoadFromFile(filesystem FS, path string) (*Dictionary, error) {
	if filesystem == nil {
		return nil, fmt.Errorf("%w: fs must not be nil", ErrInvalidDictionary)
	}
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: path must not be empty", ErrInvalidDictionary)
	}
	raw, err := filesystem.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %q: %w", ErrDictionaryRead, path, err)
	}
	return LoadFromBytes(raw)
}

// yamlDocument is the on-disk shape of `dict/capabilities.yaml`.
// Strict yaml.v3 decoding (`KnownFields(true)`) rejects any extra
// top-level key (typo `capabilites:`) or extra per-capability field
// (typo `descriiption:`) — operator typos surface loudly.
type yamlDocument struct {
	Version      int                   `yaml:"version"`
	Capabilities map[string]Capability `yaml:"capabilities"`
}

// LoadFromBytes decodes a YAML payload into a validated [*Dictionary].
// Steps:
//
//  1. Strict yaml.v3 decode (rejects unknown fields, malformed yaml,
//     multi-document streams, scalar payloads via the [validateVersion]
//     check downstream).
//  2. [validateVersion] (one supported schema version in Phase 1).
//  3. Per-row validation in DETERMINISTIC sort order — ids are sorted
//     before the walk so the surfaced first-failure message is
//     reproducible across runs (Go map iteration order is otherwise
//     randomised).
//  4. Defensive deep-copy of the decoded map into the returned
//     [Dictionary] (defense-in-depth, see [newDictionary]).
//
// Empty payloads ARE rejected — an empty dictionary defeats the
// loader's purpose AND would silently render every capability as the
// "no translation registered" fallback on the approval card. Scalar
// payloads (`null`, `42`, `"just a string"`) decode into the
// [yamlDocument] zero-value and surface under [ErrUnsupportedVersion]
// because Version == 0 is outside the supported set.
func LoadFromBytes(raw []byte) (*Dictionary, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("%w: payload is empty", ErrInvalidDictionary)
	}
	var doc yamlDocument
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: payload decoded to nothing", ErrInvalidDictionary)
		}
		return nil, fmt.Errorf("%w: yaml decode: %w", ErrInvalidDictionary, err)
	}
	// Multi-document streams are rejected; same discipline as
	// `toolregistry/yaml.go` DecodeSourcesYAML. A second document
	// would carry a silent override we never intend to support.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("%w: extra yaml document after the first", ErrInvalidDictionary)
		}
		return nil, fmt.Errorf("%w: yaml decode (extra document): %w", ErrInvalidDictionary, err)
	}
	if err := validateVersion(doc.Version); err != nil {
		return nil, err
	}
	if len(doc.Capabilities) == 0 {
		return nil, fmt.Errorf("%w: no capabilities declared", ErrInvalidDictionary)
	}
	if err := validateCapabilities(doc.Capabilities); err != nil {
		return nil, err
	}
	return newDictionary(doc.Version, doc.Capabilities), nil
}

// validateVersion enforces the closed schema-version set. Chained
// under [ErrInvalidDictionary] so callers can match the umbrella OR
// the leaf via [errors.Is].
func validateVersion(v int) error {
	if v != SupportedSchemaVersion {
		return fmt.Errorf("%w: %w: got version=%d, supported=%d", ErrInvalidDictionary, ErrUnsupportedVersion, v, SupportedSchemaVersion)
	}
	return nil
}

// validateCapabilities walks every decoded row in DETERMINISTIC
// sort order so a multi-bad-entry payload surfaces the SAME
// first-failure across runs (Go map iteration is otherwise
// randomised). Surfaces the first failure verbatim so operators see
// which row is bad.
func validateCapabilities(caps map[string]Capability) error {
	ids := make([]string, 0, len(caps))
	for id := range caps {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := validateCapabilityID(id); err != nil {
			return err
		}
		if err := validateDescription(id, caps[id].Description); err != nil {
			return err
		}
	}
	return nil
}
