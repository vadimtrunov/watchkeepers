package main

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"

	"github.com/vadimtrunov/watchkeepers/core/pkg/messenger"
)

// Sentinel errors emitted by [parseManifest] and [run]. Stable phrases
// (LESSON M2.1.b) so CI assertions never depend on lc_messages.
var (
	// ErrManifestEmpty — the supplied --manifest path is empty / not
	// passed.
	ErrManifestEmpty = errors.New("spawn-dev-bot: --manifest is required")

	// ErrCredentialsOutEmpty — --credentials-out path is empty / not
	// passed.
	ErrCredentialsOutEmpty = errors.New("spawn-dev-bot: --credentials-out is required")

	// ErrConfigTokenKeyEmpty — --config-token-key flag is empty.
	ErrConfigTokenKeyEmpty = errors.New("spawn-dev-bot: --config-token-key is required")

	// ErrManifestNameMissing — the parsed manifest is missing the
	// required `name` field.
	ErrManifestNameMissing = errors.New("spawn-dev-bot: manifest.name is required")

	// ErrManifestParse — the manifest file is malformed YAML or has the
	// wrong shape. The wrap chain carries the underlying yaml.Unmarshal
	// error.
	ErrManifestParse = errors.New("spawn-dev-bot: manifest parse failed")
)

// Manifest is the YAML shape spawn-dev-bot consumes. The structure
// mirrors the portable [messenger.AppManifest] shape so the script's
// transformation is a straight field copy. Unknown YAML keys at the
// top level are rejected by yaml.Decoder strict-mode.
//
// Schema:
//
//	name: watchkeeper-dev          # required
//	description: Watchkeeper dev bot
//	scopes:                        # bot OAuth scopes
//	  - chat:write
//	  - users:read
//	metadata:                      # optional Slack manifest extensions
//	  socket_mode_enabled: "true"
//	  long_description: "..."
//
// The `metadata` map keys are forwarded to the Slack adapter only when
// they are in the documented allow-lists
// (`recognisedManifestDisplayKeys` / `recognisedManifestSettingsKeys`
// in core/pkg/messenger/slack/create_app.go); other keys are dropped
// at the adapter boundary.
type Manifest struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Scopes      []string          `yaml:"scopes"`
	Metadata    map[string]string `yaml:"metadata"`
}

// parseManifest decodes `data` (a YAML document) into a [Manifest] and
// validates the required fields. Returns [ErrManifestNameMissing] when
// the name is empty, [ErrManifestParse] (wrapping the underlying yaml
// error) on a decode failure.
//
// Strict mode (KnownFields(true)) is enabled so a typo'd top-level key
// fails fast with an actionable error rather than silently dropping
// the misspelled field — operator-friendly for hand-edited manifests.
func parseManifest(data []byte) (Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytesReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrManifestParse, err)
	}
	if m.Name == "" {
		return Manifest{}, ErrManifestNameMissing
	}
	return m, nil
}

// toMessengerManifest copies the parsed [Manifest] onto the portable
// [messenger.AppManifest] the slack adapter consumes. Defensive copy
// of slices/maps so a later mutation of the input does not race the
// adapter's request build.
func (m Manifest) toMessengerManifest() messenger.AppManifest {
	scopes := append([]string(nil), m.Scopes...)
	var meta map[string]string
	if len(m.Metadata) > 0 {
		meta = make(map[string]string, len(m.Metadata))
		for k, v := range m.Metadata {
			meta[k] = v
		}
	}
	return messenger.AppManifest{
		Name:        m.Name,
		Description: m.Description,
		Scopes:      scopes,
		Metadata:    meta,
	}
}
