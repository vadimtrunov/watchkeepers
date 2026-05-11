package toolregistry

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// sourcesYAMLDocument wraps the operator-facing `tool_sources:` key
// at the document root. Operator config.yaml files put the list
// under a top-level `tool_sources:` key; the decoder reads that key
// strictly so a typo (e.g. `toolsources:`) is rejected up-front.
type sourcesYAMLDocument struct {
	ToolSources []SourceConfig `yaml:"tool_sources"`
}

// DecodeSourcesYAML decodes a YAML byte buffer with a top-level
// `tool_sources:` key into a validated []SourceConfig. Strict mode
// (yaml.v3 `KnownFields(true)`) rejects unknown keys both at the
// document root AND within each entry — operator typos surface
// loudly. Validation is performed via [ValidateSources] before
// returning.
//
// Returns:
//
//   - nil on success; the returned slice is empty if the YAML omits
//     `tool_sources:` entirely (no sources configured is a legitimate
//     state for a quickstart deployment).
//   - A wrapped yaml.v3 error on parse failure.
//   - The first [SourceConfig.Validate] / duplicate-name failure on
//     validation failure.
func DecodeSourcesYAML(raw []byte) ([]SourceConfig, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var doc sourcesYAMLDocument
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, fmt.Errorf("toolregistry: decode tool_sources yaml: %w", err)
	}
	// Multi-document streams are rejected to surface accidental
	// merge artefacts; same discipline as `core/pkg/config`.
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, fmt.Errorf("toolregistry: decode tool_sources yaml: extra document")
		}
		return nil, fmt.Errorf("toolregistry: decode tool_sources yaml: %w", err)
	}
	if err := ValidateSources(doc.ToolSources); err != nil {
		return nil, err
	}
	return doc.ToolSources, nil
}

// LoadSourcesYAMLFromFile reads `path` via the supplied [FS] and
// decodes it via [DecodeSourcesYAML]. Empty `path` is treated as
// "no file"; the function returns a nil slice. A non-empty `path`
// that cannot be read surfaces a wrapped read error so the operator
// sees which file failed.
func LoadSourcesYAMLFromFile(filesystem FS, path string) ([]SourceConfig, error) {
	if filesystem == nil {
		return nil, fmt.Errorf("toolregistry: load tool_sources yaml: fs must not be nil")
	}
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := filesystem.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("toolregistry: load tool_sources yaml %q: %w", path, err)
	}
	return DecodeSourcesYAML(raw)
}
