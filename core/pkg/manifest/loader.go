// Package manifest implements the M5.5 loader that promotes a wire-format
// [keepclient.ManifestVersion] into a portable [runtime.Manifest]. This
// sub-package focuses on the personality/language slice of the loader's
// responsibility documented at runtime.go:50-117 — templating Personality
// and Language into SystemPrompt and forwarding AgentID verbatim. Toolset
// jsonb decoding, AuthorityMatrix projection, Notebook open, and the
// Remember built-in tool live in sibling milestones (M5.5.b, M5.5.c,
// M5.5.d) and do NOT belong here.
package manifest

import (
	"context"
	"fmt"
	"strings"

	"github.com/vadimtrunov/watchkeepers/core/pkg/keepclient"
	"github.com/vadimtrunov/watchkeepers/core/pkg/runtime"
)

// ManifestFetcher is the single-method interface [LoadManifest] consumes
// for retrieving a [keepclient.ManifestVersion] by manifest UUID. The real
// [keepclient.Client] satisfies it via Go's structural typing; tests
// inject a hand-rolled fake without touching the HTTP client. Mirrors the
// same signature as [keepclient.Client.GetManifest] so the call site
// passes the client through verbatim.
//
// The name intentionally retains the `Manifest` prefix per the TASK M5.5.a
// AC1 contract — the loader's caller-facing surface needs the unambiguous
// `manifest.ManifestFetcher` reading at the import site (where it sits
// next to other Fetcher-shaped interfaces in sibling packages).
//
//nolint:revive // name is fixed by TASK M5.5.a AC1.
type ManifestFetcher interface {
	GetManifest(ctx context.Context, manifestID string) (*keepclient.ManifestVersion, error)
}

// LoadManifest retrieves the manifest_version row identified by manifestID
// via kc and returns the [runtime.Manifest] the runtime needs to boot a
// session. The transformation:
//
//   - SystemPrompt is composed deterministically as
//     base + suffix where base = ManifestVersion.SystemPrompt and suffix
//     appends only non-empty fields, in order, each on its own line —
//     "\n\nPersonality: <p>" then "\nLanguage: <l>". Empty Personality
//     and empty Language produce no orphan headers; an empty Language
//     after a non-empty Personality terminates the suffix cleanly; an
//     empty Personality with non-empty Language still emits the leading
//     blank-line block as "\n\nLanguage: <l>" so the language hint is
//     visually separated from base prose.
//   - AgentID is copied from ManifestVersion.ManifestID (the stable
//     identifier on this surface; agent_id ↔ manifest_id resolution
//     lives in M5.5.b).
//   - Personality and Language are copied verbatim onto runtime.Manifest
//     fields so meta-tools can introspect them after templating.
//
// An empty manifestID returns [runtime.ErrInvalidManifest] synchronously,
// before any fetcher call (mirrors keepclient's ErrInvalidRequest shape).
// Fetcher errors are wrapped as fmt.Errorf("manifest: load: %w", err) so
// callers can errors.Is the underlying sentinel (typically
// [keepclient.ErrNotFound]).
//
// Toolset, AuthorityMatrix, Model, Autonomy, and Metadata are not set by
// this loader; their wiring lands in M5.5.b alongside ACL / autonomy
// enforcement.
func LoadManifest(ctx context.Context, kc ManifestFetcher, manifestID string) (runtime.Manifest, error) {
	if manifestID == "" {
		return runtime.Manifest{}, runtime.ErrInvalidManifest
	}

	mv, err := kc.GetManifest(ctx, manifestID)
	if err != nil {
		return runtime.Manifest{}, fmt.Errorf("manifest: load: %w", err)
	}

	return runtime.Manifest{
		AgentID:      mv.ManifestID,
		SystemPrompt: composeSystemPrompt(mv.SystemPrompt, mv.Personality, mv.Language),
		Personality:  mv.Personality,
		Language:     mv.Language,
	}, nil
}

// composeSystemPrompt is the deterministic templater documented on
// [LoadManifest]. One [strings.Builder], two conditional appends; no
// micro-optimizations. Empty personality and empty language each suppress
// their own line; both empty returns base verbatim.
func composeSystemPrompt(base, personality, language string) string {
	if personality == "" && language == "" {
		return base
	}

	var b strings.Builder
	b.Grow(len(base) + len(personality) + len(language) + 32)
	b.WriteString(base)
	if personality != "" {
		b.WriteString("\n\nPersonality: ")
		b.WriteString(personality)
		if language != "" {
			b.WriteString("\nLanguage: ")
			b.WriteString(language)
		}
		return b.String()
	}
	// personality == "" && language != "": leading blank line attaches
	// to Language alone (explicit precedence rule, AC2 / test plan).
	b.WriteString("\n\nLanguage: ")
	b.WriteString(language)
	return b.String()
}
